package credits

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/modelcatalog"
)

// fakePricer prices by token: input and output rates are USD per token.
type fakePricer struct {
	inputRate, outputRate float64
	maxOutput             int
	unknown               bool
	// cost overrides what a finished response costs; nil prices its usage
	cost *float64
}

func (f *fakePricer) GetPricingEntryForModel(string, schemas.ModelProvider) *modelcatalog.PricingEntry {
	if f.unknown {
		return nil
	}
	return &modelcatalog.PricingEntry{}
}
func (f *fakePricer) GetMaxOutputTokens(string, schemas.ModelProvider) int { return f.maxOutput }
func (f *fakePricer) CalculateCostForUsage(u *schemas.BifrostLLMUsage, _ schemas.ModelProvider, _ string, _ schemas.RequestType, _ *modelcatalog.PricingLookupScopes) float64 {
	return float64(u.PromptTokens)*f.inputRate + float64(u.CompletionTokens)*f.outputRate
}
func (f *fakePricer) CalculateCost(r *schemas.BifrostResponse, s *modelcatalog.PricingLookupScopes) float64 {
	if f.cost != nil {
		return *f.cost
	}
	if r.ChatResponse != nil && r.ChatResponse.Usage != nil {
		return f.CalculateCostForUsage(r.ChatResponse.Usage, "", "", "", s)
	}
	return 0
}

// fakeLedger records calls and can be made to fail.
type fakeLedger struct {
	mu          sync.Mutex
	balance     int64
	reserved    []int64
	settled     []Settlement
	reserveErr  error
	settleErr   error
	reserveCall atomic.Int64

	settleFailures int           // fail this many Settle calls before succeeding
	settleGate     chan struct{} // when set, Settle waits until it is closed
	settleCall     atomic.Int64
}

func (f *fakeLedger) settlements() []Settlement {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Settlement(nil), f.settled...)
}

func (f *fakeLedger) Reserve(_ context.Context, _ string, amount int64, _ time.Duration) (string, bool, error) {
	f.reserveCall.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reserveErr != nil {
		return "", false, f.reserveErr
	}
	if amount > f.balance {
		return "", false, nil
	}
	f.reserved = append(f.reserved, amount)
	return "res_1", true, nil
}

func (f *fakeLedger) Settle(_ context.Context, st Settlement) (SettleResult, error) {
	f.settleCall.Add(1)
	if f.settleGate != nil {
		<-f.settleGate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.settleFailures > 0 {
		f.settleFailures--
		return SettleResult{}, errors.New("connection reset")
	}
	if f.settleErr != nil {
		return SettleResult{}, f.settleErr
	}
	f.settled = append(f.settled, st)
	return SettleResult{Charged: st.Cost > 0, Released: true}, nil
}

const testOrg = "org_test"

// 1 USD per million input tokens, 2 USD per million output tokens, 1000 tokens of output at most
func testPricer() *fakePricer {
	return &fakePricer{inputRate: 1e-6, outputRate: 2e-6, maxOutput: 1000}
}

func testPlugin(l ledger, p pricer) *Plugin {
	plugin := newPlugin(l, p, bifrost.NewNoOpLogger(), &Config{})
	plugin.settleBackoff = time.Millisecond
	return plugin
}

func testContext(organizationID string) *schemas.BifrostContext {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if organizationID != "" {
		ctx.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, organizationID)
	}
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req_1")
	return ctx
}

func chatRequest(requestType schemas.RequestType, maxTokens *int) *schemas.BifrostRequest {
	content := "hello"
	req := &schemas.BifrostChatRequest{
		Provider: schemas.OpenAI,
		Model:    "gpt-4o-mini",
		Input:    []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &content}}},
	}
	if maxTokens != nil {
		req.Params = &schemas.ChatParameters{MaxCompletionTokens: maxTokens}
	}
	return &schemas.BifrostRequest{RequestType: requestType, ChatRequest: req}
}

func chatResponse(requestType schemas.RequestType, prompt, completion int) *schemas.BifrostResponse {
	return &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
		Usage: &schemas.BifrostLLMUsage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion},
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: requestType, Provider: schemas.OpenAI, OriginalModelRequested: "gpt-4o-mini",
		},
	}}
}

func refusalOf(t *testing.T, sc *schemas.LLMPluginShortCircuit) (int, string) {
	t.Helper()
	if sc == nil || sc.Error == nil || sc.Error.StatusCode == nil || sc.Error.Type == nil {
		t.Fatalf("expected a refusal, got %+v", sc)
	}
	if sc.Error.AllowFallbacks == nil || *sc.Error.AllowFallbacks {
		t.Error("a refusal must not fall back to another provider")
	}
	return *sc.Error.StatusCode, *sc.Error.Type
}

func TestPreLLMHook_HoldsTheMaximumCostOfTheAttempt(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd}
	plugin := testPlugin(ledger, testPricer())
	ctx := testContext(testOrg)

	_, sc, err := plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil))
	if err != nil || sc != nil {
		t.Fatalf("PreLLMHook: sc=%+v err=%v", sc, err)
	}
	if len(ledger.reserved) != 1 {
		t.Fatalf("reservations: %v", ledger.reserved)
	}
	// 1000 output tokens at 2 micro-USD is 2000; the input is a handful of tokens on top
	if got := ledger.reserved[0]; got < 2000 || got > 2100 {
		t.Fatalf("held %d micro-USD, want a little over 2000", got)
	}
	if held, _ := ctx.Value(holdContextKey).(*hold); held == nil || held.organizationID != testOrg || held.amount != ledger.reserved[0] {
		t.Fatalf("hold on the context: %+v", held)
	}
}

func TestPreLLMHook_TheRequestsOwnLimitLowersTheHold(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd}
	plugin := testPlugin(ledger, testPricer())
	limit := 100
	above := 50_000

	if _, sc, _ := plugin.PreLLMHook(testContext(testOrg), chatRequest(schemas.ChatCompletionRequest, &limit)); sc != nil {
		t.Fatalf("refused: %+v", sc.Error)
	}
	// a limit above what the model can produce does not raise the hold past the model's maximum
	if _, sc, _ := plugin.PreLLMHook(testContext(testOrg), chatRequest(schemas.ChatCompletionRequest, &above)); sc != nil {
		t.Fatalf("refused: %+v", sc.Error)
	}
	if ledger.reserved[0] < 200 || ledger.reserved[0] > 300 {
		t.Errorf("with max_tokens 100: held %d, want a little over 200", ledger.reserved[0])
	}
	if ledger.reserved[1] < 2000 || ledger.reserved[1] > 2100 {
		t.Errorf("with max_tokens above the model's maximum: held %d, want a little over 2000", ledger.reserved[1])
	}
}

func TestPreLLMHook_MarkupIsHeldToo(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd}
	plain := testPlugin(ledger, testPricer())
	marked := newPlugin(ledger, testPricer(), bifrost.NewNoOpLogger(), &Config{MarkupBps: 1000})
	plain.PreLLMHook(testContext(testOrg), chatRequest(schemas.ChatCompletionRequest, nil))
	marked.PreLLMHook(testContext(testOrg), chatRequest(schemas.ChatCompletionRequest, nil))

	want := ledger.reserved[0] + ledger.reserved[0]/10
	if got := ledger.reserved[1]; got < want || got > want+1 {
		t.Fatalf("with 10 percent markup held %d, want %d", got, want)
	}
}

func TestPreLLMHook_Refuses(t *testing.T) {
	cases := []struct {
		name       string
		ledger     *fakeLedger
		pricer     *fakePricer
		org        string
		req        *schemas.BifrostRequest
		wantStatus int
		wantType   string
		wantCalls  int64
	}{
		{"a balance that does not cover the hold", &fakeLedger{balance: 100}, testPricer(), testOrg,
			chatRequest(schemas.ChatCompletionRequest, nil), 402, ErrorTypeInsufficientCredits, 1},
		{"a key that belongs to no organization", &fakeLedger{balance: 10 * usd}, testPricer(), "",
			chatRequest(schemas.ChatCompletionRequest, nil), 403, ErrorTypeNoOrganization, 0},
		{"a model with no price", &fakeLedger{balance: 10 * usd}, &fakePricer{unknown: true}, testOrg,
			chatRequest(schemas.ChatCompletionRequest, nil), 400, ErrorTypeUnpriced, 0},
		{"a model with no maximum output and no limit on the request", &fakeLedger{balance: 10 * usd}, &fakePricer{inputRate: 1e-6, outputRate: 2e-6}, testOrg,
			chatRequest(schemas.ChatCompletionRequest, nil), 400, ErrorTypeUnpriced, 0},
		{"a ledger that cannot be reached", &fakeLedger{balance: 10 * usd, reserveErr: errors.New("connection refused")}, testPricer(), testOrg,
			chatRequest(schemas.ChatCompletionRequest, nil), 503, ErrorTypeLedgerUnavailable, 1},
		{"a request type it cannot bound: image generation", &fakeLedger{balance: 10 * usd}, testPricer(), testOrg,
			&schemas.BifrostRequest{RequestType: schemas.ImageGenerationRequest}, 403, ErrorTypeUnsupportedRequest, 0},
		{"a request type it cannot bound: speech", &fakeLedger{balance: 10 * usd}, testPricer(), testOrg,
			&schemas.BifrostRequest{RequestType: schemas.SpeechRequest}, 403, ErrorTypeUnsupportedRequest, 0},
		{"a request type it has never heard of", &fakeLedger{balance: 10 * usd}, testPricer(), testOrg,
			&schemas.BifrostRequest{RequestType: schemas.RequestType("something_new")}, 403, ErrorTypeUnsupportedRequest, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := testContext(c.org)
			_, sc, err := testPlugin(c.ledger, c.pricer).PreLLMHook(ctx, c.req)
			if err != nil {
				t.Fatal(err)
			}
			status, errorType := refusalOf(t, sc)
			if status != c.wantStatus || errorType != c.wantType {
				t.Fatalf("refused with %d %s, want %d %s", status, errorType, c.wantStatus, c.wantType)
			}
			if got := c.ledger.reserveCall.Load(); got != c.wantCalls {
				t.Errorf("ledger was asked %d times, want %d", got, c.wantCalls)
			}
			if ctx.Value(holdContextKey) != nil {
				t.Error("a refused request left a hold on the context")
			}
		})
	}
}

func TestPreLLMHook_LetsModelListingsThroughWithoutAHold(t *testing.T) {
	ledger := &fakeLedger{}
	plugin := testPlugin(ledger, testPricer())
	for _, requestType := range []schemas.RequestType{schemas.ListModelsRequest, schemas.ModelRetrieveRequest} {
		// even with no organization and no balance
		if _, sc, err := plugin.PreLLMHook(testContext(""), &schemas.BifrostRequest{RequestType: requestType}); sc != nil || err != nil {
			t.Errorf("%s: sc=%+v err=%v", requestType, sc, err)
		}
	}
	if ledger.reserveCall.Load() != 0 {
		t.Error("a model listing reached the ledger")
	}
}

func TestPreLLMHook_AFreeModelNeedsNoHold(t *testing.T) {
	ledger := &fakeLedger{}
	plugin := testPlugin(ledger, &fakePricer{maxOutput: 1000})
	ctx := testContext(testOrg)
	if _, sc, err := plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil)); sc != nil || err != nil {
		t.Fatalf("sc=%+v err=%v", sc, err)
	}
	if ledger.reserveCall.Load() != 0 || ctx.Value(holdContextKey) != nil {
		t.Error("a request that costs nothing was held")
	}
}

func TestRefusingPlugin_RefusesEverythingThatCouldCost(t *testing.T) {
	plugin := Refusing("database_url is empty", bifrost.NewNoOpLogger())
	_, sc, err := plugin.PreLLMHook(testContext(testOrg), chatRequest(schemas.ChatCompletionRequest, nil))
	if err != nil {
		t.Fatal(err)
	}
	if status, errorType := refusalOf(t, sc); status != 503 || errorType != ErrorTypeLedgerUnavailable {
		t.Fatalf("refused with %d %s", status, errorType)
	}
	if strings.Contains(sc.Error.Error.Message, "database_url") {
		t.Error("the configuration error leaked to the caller")
	}
}

func TestInit_AMisconfiguredPluginLoadsAndRefuses(t *testing.T) {
	for name, config := range map[string]*Config{
		"no configuration": nil,
		"no database url":  {},
		"unset env var":    {DatabaseURL: "env.CREDITS_TEST_SURELY_UNSET"},
		"negative markup":  {DatabaseURL: "postgres://x/y", MarkupBps: -1},
		"unparsable url":   {DatabaseURL: "not a url ://"},
	} {
		plugin, err := Init(context.Background(), config, bifrost.NewNoOpLogger(), &modelcatalog.ModelCatalog{})
		if err != nil || plugin == nil {
			t.Errorf("%s: Init returned %v, %v; it must return a plugin so the gateway cannot run without one", name, plugin, err)
			continue
		}
		if _, sc, _ := plugin.PreLLMHook(testContext(testOrg), chatRequest(schemas.ChatCompletionRequest, nil)); sc == nil || sc.Error == nil {
			t.Errorf("%s: the plugin let a request through", name)
		}
	}
	// and without a catalog there is nothing to price with
	if plugin, _ := Init(context.Background(), &Config{DatabaseURL: "postgres://x/y"}, bifrost.NewNoOpLogger(), nil); plugin == nil {
		t.Error("no catalog: Init returned no plugin")
	} else if _, sc, _ := plugin.PreLLMHook(testContext(testOrg), chatRequest(schemas.ChatCompletionRequest, nil)); sc == nil {
		t.Error("no catalog: the plugin let a request through")
	}
}

func TestPostLLMHook_ChargesTheRealCostAndReleasesTheHold(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd}
	plugin := newPlugin(ledger, testPricer(), bifrost.NewNoOpLogger(), &Config{MarkupBps: 500})
	ctx := testContext(testOrg)
	plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil))

	// 1000 input tokens and 500 output tokens: 0.001 + 0.001 = 0.002 USD, plus 5 percent
	resp := chatResponse(schemas.ChatCompletionRequest, 1000, 500)
	gotResp, gotErr, err := plugin.PostLLMHook(ctx, resp, nil)
	if err != nil || gotResp != resp || gotErr != nil {
		t.Fatalf("PostLLMHook changed the response: %v %v %v", gotResp, gotErr, err)
	}
	if len(ledger.settled) != 1 {
		t.Fatalf("settlements: %+v", ledger.settled)
	}
	st := ledger.settled[0]
	if st.Cost != 2100 || st.OrganizationID != testOrg || st.ReservationID != "res_1" || st.Reference != "req_1" || st.IdempotencyKey != "usage:res_1" {
		t.Fatalf("settlement: %+v", st)
	}
	if st.Metadata["provider_cost_usd"] != 0.002 || st.Metadata["model"] != "gpt-4o-mini" || st.Metadata["failed"] != false {
		t.Fatalf("metadata: %+v", st.Metadata)
	}

	// the pipeline calling the hook again for the same attempt settles nothing more
	plugin.PostLLMHook(ctx, resp, nil)
	if len(ledger.settled) != 1 {
		t.Fatalf("settled %d times", len(ledger.settled))
	}
}

func TestPostLLMHook_AStreamIsChargedOnceOnItsFinalChunk(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd}
	plugin := testPlugin(ledger, testPricer())
	ctx := testContext(testOrg)
	plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionStreamRequest, nil))

	for range 5 {
		plugin.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionStreamRequest, 0, 0), nil)
	}
	if len(ledger.settled) != 0 {
		t.Fatalf("settled before the stream ended: %+v", ledger.settled)
	}

	ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
	plugin.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionStreamRequest, 1000, 500), nil)
	plugin.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionStreamRequest, 1000, 500), nil)
	if len(ledger.settled) != 1 || ledger.settled[0].Cost != 2000 {
		t.Fatalf("settlements after the final chunk: %+v", ledger.settled)
	}
}

func TestPostLLMHook_AFailedAttemptReleasesTheHoldAndChargesOnlyWhatWasBilled(t *testing.T) {
	failure := func(billed *schemas.BifrostLLMUsage) *schemas.BifrostError {
		status := 500
		return &schemas.BifrostError{StatusCode: &status, Error: &schemas.ErrorField{Message: "upstream failed"},
			ExtraFields: schemas.BifrostErrorExtraFields{RequestType: schemas.ChatCompletionStreamRequest, Provider: schemas.OpenAI, OriginalModelRequested: "gpt-4o-mini", BilledUsage: billed}}
	}

	t.Run("nothing billed", func(t *testing.T) {
		ledger := &fakeLedger{balance: 10 * usd}
		plugin := testPlugin(ledger, testPricer())
		ctx := testContext(testOrg)
		// a stream that fails before its final chunk still ends the attempt
		plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionStreamRequest, nil))
		plugin.PostLLMHook(ctx, nil, failure(nil))
		if len(ledger.settled) != 1 || ledger.settled[0].Cost != 0 || ledger.settled[0].Metadata["failed"] != true {
			t.Fatalf("settlements: %+v", ledger.settled)
		}
	})

	t.Run("tokens billed before the failure", func(t *testing.T) {
		ledger := &fakeLedger{balance: 10 * usd}
		plugin := testPlugin(ledger, testPricer())
		ctx := testContext(testOrg)
		plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionStreamRequest, nil))
		plugin.PostLLMHook(ctx, nil, failure(&schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}))
		if len(ledger.settled) != 1 || ledger.settled[0].Cost != 1200 {
			t.Fatalf("settlements: %+v", ledger.settled)
		}
	})
}

func TestPostLLMHook_DoesNothingForARequestItDidNotHold(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd}
	plugin := testPlugin(ledger, testPricer())
	resp := chatResponse(schemas.ChatCompletionRequest, 1000, 500)
	if got, _, err := plugin.PostLLMHook(testContext(testOrg), resp, nil); err != nil || got != resp {
		t.Fatalf("PostLLMHook: %v %v", got, err)
	}
	if len(ledger.settled) != 0 {
		t.Fatalf("settled a request that was never held: %+v", ledger.settled)
	}
}

func TestPostLLMHook_AnUnusableCostIsNotGuessed(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd}
	negative := -1.0
	pricer := testPricer()
	pricer.cost = &negative
	plugin := testPlugin(ledger, pricer)
	ctx := testContext(testOrg)
	plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil))
	plugin.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionRequest, 1, 1), nil)
	// the hold is released and nothing is charged: a negative cost must never become a credit
	if len(ledger.settled) != 1 || ledger.settled[0].Cost != 0 {
		t.Fatalf("settlements: %+v", ledger.settled)
	}
}

func TestPostLLMHook_ALedgerFailureDoesNotBreakTheResponse(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd}
	plugin := testPlugin(ledger, testPricer())
	ctx := testContext(testOrg)
	plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil))
	ledger.settleErr = errors.New("connection reset")

	resp := chatResponse(schemas.ChatCompletionRequest, 1000, 500)
	got, gotErr, err := plugin.PostLLMHook(ctx, resp, nil)
	if err != nil || got != resp || gotErr != nil {
		t.Fatalf("the customer's response was affected: %v %v %v", got, gotErr, err)
	}
}

// The hooks against real Postgres with the gateway role: the whole path the money takes.
func TestHooks_AgainstTheRealLedger(t *testing.T) {
	db := newTestDB(t)
	plugin := newPlugin(NewStore(db.ledger), testPricer(), bifrost.NewNoOpLogger(), &Config{MarkupBps: 500})
	org := db.organization(t, 5000) // half a cent

	// held: a little over 2000 x 1.05. Two fit in 5000, a third does not.
	first, second, third := testContext(org), testContext(org), testContext(org)
	if _, sc, _ := plugin.PreLLMHook(first, chatRequest(schemas.ChatCompletionRequest, nil)); sc != nil {
		t.Fatalf("first refused: %+v", sc.Error)
	}
	if _, sc, _ := plugin.PreLLMHook(second, chatRequest(schemas.ChatCompletionRequest, nil)); sc != nil {
		t.Fatalf("second refused: %+v", sc.Error)
	}
	_, sc, _ := plugin.PreLLMHook(third, chatRequest(schemas.ChatCompletionRequest, nil))
	if status, errorType := refusalOf(t, sc); status != 402 || errorType != ErrorTypeInsufficientCredits {
		t.Fatalf("third: %d %s", status, errorType)
	}
	if s := db.consistent(t, org); s.Balance != 5000 || s.Held < 4200 || s.Held > 4500 {
		t.Fatalf("state while two requests are in flight: %+v", s)
	}

	// the first costs 0.002 USD x 1.05 = 2100; the second fails with nothing billed
	plugin.PostLLMHook(first, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)
	status := 502
	plugin.PostLLMHook(second, nil, &schemas.BifrostError{StatusCode: &status, Error: &schemas.ErrorField{Message: "bad gateway"},
		ExtraFields: schemas.BifrostErrorExtraFields{RequestType: schemas.ChatCompletionRequest, Provider: schemas.OpenAI}})

	s := db.consistent(t, org)
	if s.Balance != 2900 || s.Held != 0 || s.UsageEntries != 1 {
		t.Fatalf("state after both settled: %+v", s)
	}

	// and now the third fits
	if _, sc, _ := plugin.PreLLMHook(testContext(org), chatRequest(schemas.ChatCompletionRequest, nil)); sc != nil {
		t.Fatalf("a request that fits the remaining balance was refused: %+v", sc.Error)
	}
}

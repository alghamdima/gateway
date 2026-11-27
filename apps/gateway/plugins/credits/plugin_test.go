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
	// override is the name of the pricing override that priced the attempt; "" for none
	override string
}

func (f *fakePricer) GetPricingEntryForModel(string, schemas.ModelProvider) *modelcatalog.PricingEntry {
	if f.unknown {
		return nil
	}
	return &modelcatalog.PricingEntry{}
}
func (f *fakePricer) GetMaxOutputTokens(string, schemas.ModelProvider) int { return f.maxOutput }
func (f *fakePricer) PricingOverrideName(string, schemas.RequestType, *modelcatalog.PricingLookupScopes) string {
	return f.override
}
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
	fxs         []fxRate // the rate each hold was taken at
	settled     []Settlement
	reserveErr  error
	settleErr   error
	reserveCall atomic.Int64
	virtualKeys []string // the key each Reserve call named
	keyLimited  bool     // refuse every hold as over the key's limit

	unmapped  map[string]bool // customers OrganizationFor does not know
	lookupErr error
	lookups   int

	settleFailures int           // fail this many Settle calls before succeeding
	settleGate     chan struct{} // when set, Settle waits until it is closed
	settleCall     atomic.Int64

	extended atomic.Int64 // Extend calls
}

func (f *fakeLedger) Extend(context.Context, string, time.Duration) (bool, error) {
	f.extended.Add(1)
	return true, nil
}

// Shortfall reports the fake's balance, and the key as at its limit when keyLimited is set.
func (f *fakeLedger) Shortfall(context.Context, string, string) (Shortfall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := Shortfall{HasAccount: true, Balance: f.balance}
	if f.keyLimited {
		limit := int64(10 * usd)
		out.KeyLimit, out.KeySpent, out.KeyReset = &limit, limit, "daily"
	}
	return out, nil
}

func (f *fakeLedger) settlements() []Settlement {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Settlement(nil), f.settled...)
}

// OrganizationFor maps each customer to the organization of the same id, unless unmapped
// lists it.
func (f *fakeLedger) OrganizationFor(_ context.Context, customerID string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lookupErr != nil {
		return "", false, f.lookupErr
	}
	f.lookups++
	if f.unmapped[customerID] {
		return "", false, nil
	}
	return customerID, true, nil
}

func (f *fakeLedger) Reserve(_ context.Context, _, virtualKeyID string, amount int64, fx fxRate, _ time.Duration) (string, ReserveOutcome, error) {
	f.reserveCall.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.virtualKeys = append(f.virtualKeys, virtualKeyID)
	if f.reserveErr != nil {
		return "", InsufficientCredits, f.reserveErr
	}
	if f.keyLimited {
		return "", KeyLimitReached, nil
	}
	if amount > f.balance {
		return "", InsufficientCredits, nil
	}
	f.reserved = append(f.reserved, amount)
	f.fxs = append(f.fxs, fx)
	return "res_1", Reserved, nil
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

// testPlugin has its rate loaded, at parity.
func testPlugin(l ledger, p pricer) *Plugin {
	plugin := newPlugin(l, p, bifrost.NewNoOpLogger(), &Config{})
	plugin.settleBackoff = time.Millisecond
	return atParityRate(plugin)
}

// atParity is a USD to SAR rate of exactly 1. The tests of holds and bounds use it,
// so their amounts read as micro-USD; the tests of the conversion set the peg (pegRate).
const (
	atParity = int64(1_000_000)
	pegRate  = int64(3_750_000)
)

// fxAt is the rate with a spread, as LoadRate would publish it.
func fxAt(rateE6, spreadBps int64) fxRate {
	fx, err := newFXRate(rateE6, spreadBps)
	if err != nil {
		panic(err)
	}
	return fx
}

// testFX is what the store tests record on holds and charges: the peg with a 2.5% spread.
var testFX = fxAt(pegRate, 250)

// atParityRate publishes a rate of exactly 1, as a successful load from the database would.
func atParityRate(p *Plugin) *Plugin {
	return withRate(p, atParity, 0)
}

// withRate publishes another USD to SAR rate and spread, as staff changing them in the admin
// would.
func withRate(p *Plugin, rateE6, spreadBps int64) *Plugin {
	fx := fxAt(rateE6, spreadBps)
	p.rate.Store(&fx)
	return p
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
		wantSource string
		wantReason string
	}{
		{"a balance that does not cover the hold", &fakeLedger{balance: 100}, testPricer(), testOrg,
			chatRequest(schemas.ChatCompletionRequest, nil), 402, ErrorTypeInsufficientCredits, 1, "credits", ReasonRequestExceedsBalance},
		{"a key over its spending limit", &fakeLedger{balance: 10 * usd, keyLimited: true}, testPricer(), testOrg,
			chatRequest(schemas.ChatCompletionRequest, nil), 402, ErrorTypeKeyLimit, 1, "key_limit", ReasonKeyLimitReached},
		{"a key that belongs to no organization", &fakeLedger{balance: 10 * usd}, testPricer(), "",
			chatRequest(schemas.ChatCompletionRequest, nil), 403, ErrorTypeNoOrganization, 0, "key", ReasonKeyHasNoOrganization},
		{"a model with no price", &fakeLedger{balance: 10 * usd}, &fakePricer{unknown: true}, testOrg,
			chatRequest(schemas.ChatCompletionRequest, nil), 400, ErrorTypeUnpriced, 0, "credits", ReasonModelNotPriced},
		{"a model with no maximum output and no limit on the request", &fakeLedger{balance: 10 * usd}, &fakePricer{inputRate: 1e-6, outputRate: 2e-6}, testOrg,
			chatRequest(schemas.ChatCompletionRequest, nil), 400, ErrorTypeUnpriced, 0, "credits", ReasonModelNotPriced},
		{"a ledger that cannot be reached", &fakeLedger{balance: 10 * usd, reserveErr: errors.New("connection refused")}, testPricer(), testOrg,
			chatRequest(schemas.ChatCompletionRequest, nil), 503, ErrorTypeLedgerUnavailable, 1, "service", ReasonLedgerUnavailable},
		{"a request type it cannot bound: image generation", &fakeLedger{balance: 10 * usd}, testPricer(), testOrg,
			&schemas.BifrostRequest{RequestType: schemas.ImageGenerationRequest}, 403, ErrorTypeUnsupportedRequest, 0, "request_type", ReasonRequestTypeNotEnabled},
		{"a request type it cannot bound: speech", &fakeLedger{balance: 10 * usd}, testPricer(), testOrg,
			&schemas.BifrostRequest{RequestType: schemas.SpeechRequest}, 403, ErrorTypeUnsupportedRequest, 0, "request_type", ReasonRequestTypeNotEnabled},
		{"a request type it has never heard of", &fakeLedger{balance: 10 * usd}, testPricer(), testOrg,
			&schemas.BifrostRequest{RequestType: schemas.RequestType("something_new")}, 403, ErrorTypeUnsupportedRequest, 0, "request_type", ReasonRequestTypeNotEnabled},
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
			refusal := sc.Error.Error.Refusal
			if refusal == nil || refusal.StatusCode != c.wantStatus || refusal.LimitSource != c.wantSource || refusal.Reason != c.wantReason || refusal.RemedyHint == "" {
				t.Fatalf("refusal metadata %+v, want %d %s %s with a remedy hint", refusal, c.wantStatus, c.wantSource, c.wantReason)
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

func TestPreLLMHook_NamesTheVirtualKeyToTheLedger(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd}
	ctx := testContext(testOrg)
	ctx.SetValue(schemas.BifrostContextKeyGovernanceVirtualKeyID, "vk_123")
	if _, sc, err := testPlugin(ledger, testPricer()).PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil)); err != nil || sc != nil {
		t.Fatalf("hook: %v %+v", err, sc)
	}
	if len(ledger.virtualKeys) != 1 || ledger.virtualKeys[0] != "vk_123" {
		t.Fatalf("ledger was given keys %q, want [vk_123]", ledger.virtualKeys)
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
	plugin := testPlugin(ledger, testPricer())
	ctx := testContext(testOrg)
	plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil))

	// 1000 input tokens and 500 output tokens: 0.001 + 0.001 = 0.002 USD, at parity
	resp := chatResponse(schemas.ChatCompletionRequest, 1000, 500)
	gotResp, gotErr, err := plugin.PostLLMHook(ctx, resp, nil)
	if err != nil || gotResp != resp || gotErr != nil {
		t.Fatalf("PostLLMHook changed the response: %v %v %v", gotResp, gotErr, err)
	}
	if len(ledger.settled) != 1 {
		t.Fatalf("settlements: %+v", ledger.settled)
	}
	st := ledger.settled[0]
	if st.Cost != 2000 || st.OrganizationID != testOrg || st.ReservationID != "res_1" || st.Reference != "req_1" || st.IdempotencyKey != "usage:res_1" {
		t.Fatalf("settlement: %+v", st)
	}
	if st.Metadata["provider_cost_usd"] != 0.002 || st.Metadata["model"] != "gpt-4o-mini" || st.Metadata["failed"] != false {
		t.Fatalf("metadata: %+v", st.Metadata)
	}
	if st.Metadata["prompt_tokens"] != 1000 || st.Metadata["completion_tokens"] != 500 || st.Metadata["total_tokens"] != 1500 {
		t.Fatalf("token counts: %+v", st.Metadata)
	}
	// there is no markup to record: the charge is the provider's cost at the rate
	if _, ok := st.Metadata["markup_bps"]; ok {
		t.Fatalf("the entry records a markup: %+v", st.Metadata)
	}

	// the pipeline calling the hook again for the same attempt settles nothing more
	plugin.PostLLMHook(ctx, resp, nil)
	if len(ledger.settled) != 1 {
		t.Fatalf("settled %d times", len(ledger.settled))
	}
}

func TestPostLLMHook_RecordsTheProviderKeyThatServedTheAttempt(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd}
	plugin := testPlugin(ledger, testPricer())

	served := testContext(testOrg)
	plugin.PreLLMHook(served, chatRequest(schemas.ChatCompletionRequest, nil))
	served.SetValue(schemas.BifrostContextKeySelectedKeyID, "key-in-kingdom")
	plugin.PostLLMHook(served, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)

	// an attempt that ended before a key was chosen names none
	unserved := testContext(testOrg)
	plugin.PreLLMHook(unserved, chatRequest(schemas.ChatCompletionRequest, nil))
	plugin.PostLLMHook(unserved, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)

	settled := ledger.settlements()
	if len(settled) != 2 || settled[0].Metadata["provider_key_id"] != "key-in-kingdom" {
		t.Fatalf("settlements: %+v", settled)
	}
	if _, named := settled[1].Metadata["provider_key_id"]; named {
		t.Fatalf("a key was named for an attempt that chose none: %+v", settled[1].Metadata)
	}
}

func TestPostLLMHook_RecordsThePriceOverrideThatPricedTheAttempt(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd}
	pricer := testPricer()
	pricer.override = "lemru-price:42"
	plugin := testPlugin(ledger, pricer)
	ctx := testContext(testOrg)
	plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil))
	plugin.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)

	// priced by the datasheet alone: nothing to name
	pricer.override = ""
	alone := testContext(testOrg)
	plugin.PreLLMHook(alone, chatRequest(schemas.ChatCompletionRequest, nil))
	plugin.PostLLMHook(alone, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)

	settled := ledger.settlements()
	if len(settled) != 2 || settled[0].Metadata["price_override"] != "lemru-price:42" {
		t.Fatalf("settlements: %+v", settled)
	}
	if _, named := settled[1].Metadata["price_override"]; named {
		t.Fatalf("an override was named for an attempt the datasheet priced: %+v", settled[1].Metadata)
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
	if ledger.settled[0].Metadata["total_tokens"] != 1500 {
		t.Fatalf("stream token counts: %+v", ledger.settled[0].Metadata)
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
		if ledger.settled[0].Metadata["prompt_tokens"] != 1000 || ledger.settled[0].Metadata["completion_tokens"] != 100 {
			t.Fatalf("billed token counts: %+v", ledger.settled[0].Metadata)
		}
	})
}

func TestPostLLMHook_ARunningStreamKeepsItsHoldAlive(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd}
	plugin := testPlugin(ledger, testPricer())
	plugin.reservationTTL = 300 * time.Millisecond
	ctx := testContext(testOrg)
	plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionStreamRequest, nil))
	extendsAfter := func(chunks int) int64 {
		t.Helper()
		for range chunks {
			plugin.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionStreamRequest, 0, 0), nil)
		}
		time.Sleep(20 * time.Millisecond) // the extension runs off the stream's path
		return ledger.extended.Load()
	}

	if n := extendsAfter(5); n != 0 {
		t.Fatalf("extended a fresh hold %d times", n)
	}
	time.Sleep(plugin.reservationTTL / 3)
	if n := extendsAfter(5); n != 1 {
		t.Fatalf("after a third of the lifetime, %d extensions for 5 chunks, want 1", n)
	}
	time.Sleep(plugin.reservationTTL / 3)
	if n := extendsAfter(1); n != 2 {
		t.Fatalf("extensions %d, want 2", n)
	}
}

func TestPostLLMHook_ANonStreamingAttemptIsNotExtended(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd}
	plugin := testPlugin(ledger, testPricer())
	plugin.reservationTTL = 30 * time.Millisecond
	ctx := testContext(testOrg)
	plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil))
	time.Sleep(plugin.reservationTTL)
	plugin.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)
	time.Sleep(20 * time.Millisecond)
	if n := ledger.extended.Load(); n != 0 {
		t.Fatalf("extended %d times", n)
	}
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
	store := NewStore(db.ledger)
	plugin := newPlugin(store, testPricer(), bifrost.NewNoOpLogger(), &Config{})
	// the rate comes from the database, as staff would set it: the peg the migration seeds
	plugin.rates = store
	if err := plugin.refreshRate(context.Background()); err != nil {
		t.Fatal(err)
	}
	org := db.organization(t, 17_000) // 0.017 SAR

	// held: a little over 2000 x 3.75 = 7500. Two fit in 17000, a third does not.
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
	if s := db.consistent(t, org); s.Balance != 17_000 || s.Held < 15_000 || s.Held > 16_900 {
		t.Fatalf("state while two requests are in flight: %+v", s)
	}

	// the first costs 0.002 USD x 3.75 = 7500 micro-SAR; the second fails with nothing billed
	plugin.PostLLMHook(first, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)
	status := 502
	plugin.PostLLMHook(second, nil, &schemas.BifrostError{StatusCode: &status, Error: &schemas.ErrorField{Message: "bad gateway"},
		ExtraFields: schemas.BifrostErrorExtraFields{RequestType: schemas.ChatCompletionRequest, Provider: schemas.OpenAI}})

	s := db.consistent(t, org)
	if s.Balance != 9_500 || s.Held != 0 || s.UsageEntries != 1 {
		t.Fatalf("state after both settled: %+v", s)
	}

	// and now the third fits
	if _, sc, _ := plugin.PreLLMHook(testContext(org), chatRequest(schemas.ChatCompletionRequest, nil)); sc != nil {
		t.Fatalf("a request that fits the remaining balance was refused: %+v", sc.Error)
	}
}

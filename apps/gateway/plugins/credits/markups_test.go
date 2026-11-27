package credits

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/modelcatalog"
)

func requestTo(provider schemas.ModelProvider) *schemas.BifrostRequest {
	req := chatRequest(schemas.ChatCompletionRequest, nil)
	req.ChatRequest.Provider = provider
	return req
}

func TestPreLLMHook_RefusesUntilTheMarkupsAreLoaded(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd}
	plugin := newPlugin(ledger, testPricer(), bifrost.NewNoOpLogger(), &Config{})

	_, sc, _ := plugin.PreLLMHook(testContext(testOrg), chatRequest(schemas.ChatCompletionRequest, nil))
	if status, errorType := refusalOf(t, sc); status != 503 || errorType != ErrorTypeLedgerUnavailable {
		t.Fatalf("got %d %s", status, errorType)
	}
	if ledger.reserveCall.Load() != 0 {
		t.Fatal("a hold was taken without knowing the markup")
	}
	// model listings cost nothing and need no markup
	if _, sc, _ := plugin.PreLLMHook(testContext(testOrg), &schemas.BifrostRequest{RequestType: schemas.ListModelsRequest}); sc != nil {
		t.Fatalf("a model listing was refused: %+v", sc.Error)
	}
}

func TestHooks_EachProviderIsHeldAndChargedAtItsOwnMarkup(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd}
	plugin := withMarkups(testPlugin(ledger, testPricer()), 500, map[string]int64{"anthropic": 2000})

	cases := []struct {
		provider   schemas.ModelProvider
		markupBps  int64
		wantCharge int64 // 0.002 USD of usage
	}{
		{schemas.Anthropic, 2000, 2400},
		{schemas.OpenAI, 500, 2100},
	}
	for i, c := range cases {
		ctx := testContext(testOrg)
		if _, sc, _ := plugin.PreLLMHook(ctx, requestTo(c.provider)); sc != nil {
			t.Fatalf("%s refused: %+v", c.provider, sc.Error)
		}
		plugin.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)

		st := ledger.settlements()[i]
		if st.Cost != c.wantCharge || st.Metadata["markup_bps"] != c.markupBps {
			t.Errorf("%s: charged %d with markup %v, want %d with %d", c.provider, st.Cost, st.Metadata["markup_bps"], c.wantCharge, c.markupBps)
		}
	}
	// the hold carries the same markup as the charge
	if ratio := float64(ledger.reserved[0]) / float64(ledger.reserved[1]); ratio < 1.14 || ratio > 1.15 {
		t.Errorf("holds %v do not reflect 20 vs 5 percent", ledger.reserved)
	}
}

func TestHooks_AChangeDuringAnAttemptDoesNotChangeItsPrice(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd}
	plugin := withMarkups(testPlugin(ledger, testPricer()), 500, nil)
	ctx := testContext(testOrg)
	plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil))

	withMarkups(plugin, 5000, nil) // staff raise it while the attempt runs
	plugin.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)
	if st := ledger.settlements()[0]; st.Cost != 2100 {
		t.Fatalf("charged %d, want 2100: the markup the hold was taken at", st.Cost)
	}

	next := testContext(testOrg)
	plugin.PreLLMHook(next, chatRequest(schemas.ChatCompletionRequest, nil))
	plugin.PostLLMHook(next, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)
	if st := ledger.settlements()[1]; st.Cost != 3000 {
		t.Fatalf("the next attempt was charged %d, want 3000 at the new markup", st.Cost)
	}
}

// fakeSource serves a scripted sequence of loads; the last one repeats.
type fakeSource struct {
	mu    sync.Mutex
	loads []func() (*markups, error)
	calls int
}

func (f *fakeSource) LoadMarkups(context.Context) (*markups, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	load := f.loads[min(f.calls, len(f.loads)-1)]
	f.calls++
	return load()
}

func (f *fakeSource) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestRefreshLoop_RetriesQuicklyUntilTheFirstLoad(t *testing.T) {
	down := func() (*markups, error) { return nil, errors.New("connection refused") }
	up := func() (*markups, error) { return &markups{defaultBps: 300, fx: fxAt(atParity, 0)}, nil }
	source := &fakeSource{loads: []func() (*markups, error){down, down, down, up}}
	plugin := testPlugin(&fakeLedger{}, testPricer())
	plugin.markups.Store(nil)
	plugin.prices = source
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// the regular interval is an hour: only the retry interval can get it loaded in time
	go plugin.refreshLoop(ctx, time.Hour, time.Millisecond)
	waitFor(t, "the markups to load after three failures", func() bool { return plugin.markups.Load() != nil })
}

func TestRefreshLoop_KeepsTheLastGoodValuesAndPicksUpChanges(t *testing.T) {
	down := func() (*markups, error) { return nil, errors.New("connection refused") }
	first := func() (*markups, error) { return &markups{defaultBps: 300, fx: fxAt(atParity, 0)}, nil }
	second := func() (*markups, error) { return &markups{defaultBps: 400, fx: fxAt(atParity, 0)}, nil }
	source := &fakeSource{loads: []func() (*markups, error){down, down, first, down, second}}

	plugin := testPlugin(&fakeLedger{}, testPricer())
	plugin.markups.Store(nil)
	plugin.prices = source
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		plugin.refreshLoop(ctx, 20*time.Millisecond, time.Millisecond)
		close(done)
	}()

	waitFor(t, "the first successful load", func() bool { return plugin.markups.Load() != nil })
	if got := plugin.markups.Load().defaultBps; got != 300 {
		t.Fatalf("first snapshot: %d", got)
	}
	// the next load fails: the snapshot must survive it
	waitFor(t, "the failed reload", func() bool { return source.count() >= 4 })
	if m := plugin.markups.Load(); m == nil || m.defaultBps != 300 {
		t.Fatalf("a failed reload dropped the markups: %+v", m)
	}
	waitFor(t, "the change to be picked up", func() bool { return plugin.markups.Load().defaultBps == 400 })

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh loop did not stop")
	}
}

func TestRefreshLoop_DoesNotHammerTheDatabaseOnceLoaded(t *testing.T) {
	source := &fakeSource{loads: []func() (*markups, error){func() (*markups, error) { return &markups{fx: fxAt(atParity, 0)}, nil }}}
	plugin := testPlugin(&fakeLedger{}, testPricer())
	plugin.prices = source
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go plugin.refreshLoop(ctx, time.Hour, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	if n := source.count(); n != 0 {
		t.Fatalf("loaded %d times with markups already in place and an hour's interval", n)
	}
}

// The statement against the real schema, as the gateway role.
func TestLoadMarkups_ReadsWhatStaffSet(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)

	m, err := store.LoadMarkups(context.Background())
	if err != nil || m.defaultBps != 0 || len(m.byProvider) != 0 || m.fx != fxAt(pegRate, 0) {
		t.Fatalf("fresh database: %+v, %v; want no markups at the peg the migration seeds", m, err)
	}

	// the rate and spread staff set are read by their named columns, and the effective rate
	// is worked out from them: 3.761 + 1.5% = 3.817415
	db.setRate(t, 3_761_000)
	db.setSpread(t, 150)
	if m, err = store.LoadMarkups(context.Background()); err != nil || m.fx != (fxRate{officialE6: 3_761_000, spreadBps: 150, effectiveE6: 3_817_415}) {
		t.Fatalf("after staff set 3.761 and 1.5%%: %+v, %v", m, err)
	}
	// a spread outside its typo ceiling is refused by the database
	for _, typo := range []int64{1001, -1} {
		if _, err := db.admin.Exec(context.Background(), `UPDATE pricing_settings SET fx_spread_bps = $1 WHERE id = 1`, typo); err == nil {
			t.Fatalf("the database accepted a spread of %d bps", typo)
		}
	}
	db.setSpread(t, 0)
	// outside the sanity range the database refuses it: a typo cannot reach the plugin
	for _, typo := range []int64{37_500_000, 375_000, 0} {
		if _, err := db.admin.Exec(context.Background(), `UPDATE pricing_settings SET usd_to_sar_rate_e6 = $1 WHERE id = 1`, typo); err == nil {
			t.Fatalf("the database accepted a rate of %d", typo)
		}
	}
	// and a rate that got past the database anyway is refused, never turned into free requests
	if _, err := db.admin.Exec(context.Background(), `ALTER TABLE pricing_settings DROP CONSTRAINT pricing_settings_usd_to_sar_rate_range`); err != nil {
		t.Fatal(err)
	}
	db.setRate(t, 0)
	if m, err := store.LoadMarkups(context.Background()); err == nil {
		t.Fatalf("a zero rate was loaded: %+v", m)
	}
	db.setRate(t, pegRate)

	db.setPricing(t, 250, map[string]int64{"openai": 0, "anthropic": 1200})
	m, err = store.LoadMarkups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.defaultBps != 250 || m.forProvider(schemas.OpenAI) != 0 || m.forProvider(schemas.Anthropic) != 1200 || m.forProvider(schemas.Gemini) != 250 {
		t.Fatalf("got %+v", m)
	}

	// the database's bounds are the first defence; a negative value that got past them anyway
	// is refused rather than turned into a discount below cost
	if _, err := db.admin.Exec(context.Background(), `ALTER TABLE provider_markup DROP CONSTRAINT provider_markup_range`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.admin.Exec(context.Background(), `UPDATE provider_markup SET markup_bps = -1 WHERE provider = 'openai'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadMarkups(context.Background()); err == nil {
		t.Fatal("a negative markup was loaded")
	}
	if _, err := db.admin.Exec(context.Background(), `UPDATE provider_markup SET markup_bps = 0 WHERE provider = 'openai'`); err != nil {
		t.Fatal(err)
	}

	// without the settings row there are no prices: the plugin keeps what it had
	plugin := testPlugin(&fakeLedger{}, testPricer())
	plugin.prices = store
	if err := plugin.refreshMarkups(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.admin.Exec(context.Background(), `DELETE FROM pricing_settings`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadMarkups(context.Background()); err == nil {
		t.Fatal("no settings row loaded without an error")
	}
	if err := plugin.refreshMarkups(context.Background()); err == nil {
		t.Fatal("refresh reported success without a settings row")
	}
	if m := plugin.markups.Load(); m == nil || m.defaultBps != 250 {
		t.Fatalf("the last good markups were dropped: %+v", m)
	}
}

// Init loads the markups before the gateway starts serving, so the first requests are not
// refused while the refresh loop catches up.
func TestInit_LoadsTheMarkupsBeforeServing(t *testing.T) {
	db := newTestDB(t)
	db.setPricing(t, 700, nil)
	plugin, err := Init(context.Background(), &Config{DatabaseURL: ledgerURL(t, db)}, bifrost.NewNoOpLogger(), &modelcatalog.ModelCatalog{})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Cleanup()
	m := plugin.(*Plugin).markups.Load()
	if m == nil || m.defaultBps != 700 {
		t.Fatalf("markups right after Init: %+v", m)
	}
}

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

func TestPreLLMHook_RefusesUntilTheRateIsLoaded(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd}
	plugin := newPlugin(ledger, testPricer(), bifrost.NewNoOpLogger(), &Config{})

	_, sc, _ := plugin.PreLLMHook(testContext(testOrg), chatRequest(schemas.ChatCompletionRequest, nil))
	if status, errorType := refusalOf(t, sc); status != 503 || errorType != ErrorTypeLedgerUnavailable {
		t.Fatalf("got %d %s", status, errorType)
	}
	if ledger.reserveCall.Load() != 0 {
		t.Fatal("a hold was taken without knowing the rate")
	}
	// model listings cost nothing and need no rate
	if _, sc, _ := plugin.PreLLMHook(testContext(testOrg), &schemas.BifrostRequest{RequestType: schemas.ListModelsRequest}); sc != nil {
		t.Fatalf("a model listing was refused: %+v", sc.Error)
	}
}

// Every provider is held and charged its own cost at the same effective rate: there is no
// markup, per provider or by default.
func TestHooks_EveryProviderIsChargedItsCostAtTheRate(t *testing.T) {
	ledger := &fakeLedger{balance: 100 * usd}
	plugin := withRate(testPlugin(ledger, testPricer()), pegRate, 250)

	providers := []schemas.ModelProvider{schemas.Anthropic, schemas.OpenAI, schemas.Gemini}
	for i, provider := range providers {
		ctx := testContext(testOrg)
		if _, sc, _ := plugin.PreLLMHook(ctx, requestTo(provider)); sc != nil {
			t.Fatalf("%s refused: %+v", provider, sc.Error)
		}
		plugin.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)

		// 0.002 USD x 3.84375 = 0.0076875 SAR, rounded half up
		st := ledger.settlements()[i]
		if st.Cost != 7688 || st.CostMicroUSD != 2000 {
			t.Errorf("%s: charged %d micro-SAR for %d micro-USD, want 7688 for 2000", provider, st.Cost, st.CostMicroUSD)
		}
		if st.Cost > ledger.reserved[i] {
			t.Errorf("%s: charged %d, more than the hold %d", provider, st.Cost, ledger.reserved[i])
		}
	}
}

// fakeSource serves a scripted sequence of loads; the last one repeats.
type fakeSource struct {
	mu    sync.Mutex
	loads []func() (fxRate, error)
	calls int
}

func (f *fakeSource) LoadRate(context.Context) (fxRate, error) {
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

func loadsRate(rateE6 int64) func() (fxRate, error) {
	return func() (fxRate, error) { return fxAt(rateE6, 0), nil }
}

func down() (fxRate, error) { return fxRate{}, errors.New("connection refused") }

func TestRefreshLoop_RetriesQuicklyUntilTheFirstLoad(t *testing.T) {
	source := &fakeSource{loads: []func() (fxRate, error){down, down, down, loadsRate(pegRate)}}
	plugin := testPlugin(&fakeLedger{}, testPricer())
	plugin.rate.Store(nil)
	plugin.rates = source
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// the regular interval is an hour: only the retry interval can get it loaded in time
	go plugin.refreshLoop(ctx, time.Hour, time.Millisecond)
	waitFor(t, "the rate to load after three failures", func() bool { return plugin.rate.Load() != nil })
}

func TestRefreshLoop_KeepsTheLastGoodValueAndPicksUpChanges(t *testing.T) {
	source := &fakeSource{loads: []func() (fxRate, error){down, down, loadsRate(pegRate), down, loadsRate(4_000_000)}}

	plugin := testPlugin(&fakeLedger{}, testPricer())
	plugin.rate.Store(nil)
	plugin.rates = source
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		plugin.refreshLoop(ctx, 20*time.Millisecond, time.Millisecond)
		close(done)
	}()

	waitFor(t, "the first successful load", func() bool { return plugin.rate.Load() != nil })
	if got := plugin.rate.Load().effectiveE6; got != pegRate {
		t.Fatalf("first rate: %d", got)
	}
	// the next load fails: the rate must survive it
	waitFor(t, "the failed reload", func() bool { return source.count() >= 4 })
	if fx := plugin.rate.Load(); fx == nil || fx.effectiveE6 != pegRate {
		t.Fatalf("a failed reload dropped the rate: %+v", fx)
	}
	waitFor(t, "the change to be picked up", func() bool { return plugin.rate.Load().effectiveE6 == 4_000_000 })

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh loop did not stop")
	}
}

func TestRefreshLoop_DoesNotHammerTheDatabaseOnceLoaded(t *testing.T) {
	source := &fakeSource{loads: []func() (fxRate, error){loadsRate(atParity)}}
	plugin := testPlugin(&fakeLedger{}, testPricer())
	plugin.rates = source
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go plugin.refreshLoop(ctx, time.Hour, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	if n := source.count(); n != 0 {
		t.Fatalf("loaded %d times with a rate already in place and an hour's interval", n)
	}
}

// The statement against the real schema, as the gateway role.
func TestLoadRate_ReadsWhatStaffSet(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)

	fx, err := store.LoadRate(context.Background())
	if err != nil || fx != fxAt(pegRate, 0) {
		t.Fatalf("fresh database: %+v, %v; want the peg the migration seeds", fx, err)
	}

	// the rate and spread staff set are read by their named columns, and the effective rate
	// is worked out from them: 3.761 + 1.5% = 3.817415
	db.setRate(t, 3_761_000)
	db.setSpread(t, 150)
	if fx, err = store.LoadRate(context.Background()); err != nil || fx != (fxRate{officialE6: 3_761_000, spreadBps: 150, effectiveE6: 3_817_415}) {
		t.Fatalf("after staff set 3.761 and 1.5%%: %+v, %v", fx, err)
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
	if fx, err := store.LoadRate(context.Background()); err == nil {
		t.Fatalf("a zero rate was loaded: %+v", fx)
	}
	db.setRate(t, pegRate)

	// the markups are gone from the schema: nothing staff set before can reach a charge
	var markupTable *string
	var markupColumns int
	if err := db.admin.QueryRow(context.Background(), `SELECT to_regclass('public.provider_markup')::text, (SELECT count(*) FROM information_schema.columns WHERE table_name = 'pricing_settings' AND column_name LIKE '%markup%')`).Scan(&markupTable, &markupColumns); err != nil || markupTable != nil || markupColumns != 0 {
		t.Fatalf("a markup is still in the schema: table %v, %d columns (%v)", markupTable, markupColumns, err)
	}

	// without the settings row there is no rate: the plugin keeps what it had
	plugin := testPlugin(&fakeLedger{}, testPricer())
	plugin.rates = store
	if err := plugin.refreshRate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.admin.Exec(context.Background(), `DELETE FROM pricing_settings`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadRate(context.Background()); err == nil {
		t.Fatal("no settings row loaded without an error")
	}
	if err := plugin.refreshRate(context.Background()); err == nil {
		t.Fatal("refresh reported success without a settings row")
	}
	if fx := plugin.rate.Load(); fx == nil || *fx != fxAt(pegRate, 0) {
		t.Fatalf("the last good rate was dropped: %+v", fx)
	}
}

// Init loads the rate before the gateway starts serving, so the first requests are not
// refused while the refresh loop catches up.
func TestInit_LoadsTheRateBeforeServing(t *testing.T) {
	db := newTestDB(t)
	db.setRate(t, 3_761_000)
	plugin, err := Init(context.Background(), &Config{DatabaseURL: ledgerURL(t, db)}, bifrost.NewNoOpLogger(), &modelcatalog.ModelCatalog{})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Cleanup()
	fx := plugin.(*Plugin).rate.Load()
	if fx == nil || *fx != fxAt(3_761_000, 0) {
		t.Fatalf("rate right after Init: %+v", fx)
	}
}

package credits

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

func settlement(i int) Settlement {
	return Settlement{ReservationID: fmt.Sprintf("res_%d", i), OrganizationID: testOrg, Cost: int64(i + 1), Reference: fmt.Sprintf("req_%d", i)}
}

// waitFor polls: the things awaited here happen on other goroutines.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSettle_TheResponseDoesNotWaitForTheLedger(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd, settleGate: make(chan struct{})}
	plugin := testPlugin(ledger, testPricer())
	plugin.start(8, 2, 0)

	ctx := testContext(testOrg)
	plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil))
	done := make(chan struct{})
	go func() {
		plugin.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PostLLMHook waited for a ledger that was not answering")
	}
	if got := ledger.settlements(); len(got) != 0 {
		t.Fatalf("settled before the ledger answered: %+v", got)
	}

	close(ledger.settleGate)
	plugin.background.flush()
	if got := ledger.settlements(); len(got) != 1 || got[0].Cost != 2000 {
		t.Fatalf("settlements: %+v", got)
	}
	plugin.Cleanup()
}

func TestSettle_RetriesUntilTheLedgerTakesIt(t *testing.T) {
	ledger := &fakeLedger{settleFailures: settleAttempts - 1}
	plugin := testPlugin(ledger, testPricer())
	plugin.start(8, 1, 0)
	plugin.settle(settlement(0))
	plugin.background.flush()
	if got := ledger.settlements(); len(got) != 1 || got[0].ReservationID != "res_0" {
		t.Fatalf("settlements: %+v", got)
	}
	if calls := ledger.settleCall.Load(); calls != settleAttempts {
		t.Fatalf("Settle was called %d times, want %d", calls, settleAttempts)
	}
	plugin.Cleanup()
}

func TestSettle_GivesUpAfterTheLastAttemptAndKeepsWorking(t *testing.T) {
	ledger := &fakeLedger{settleFailures: settleAttempts}
	plugin := testPlugin(ledger, testPricer())
	plugin.start(8, 1, 0)
	plugin.settle(settlement(0)) // every attempt fails
	plugin.background.flush()
	if calls := ledger.settleCall.Load(); calls != settleAttempts {
		t.Fatalf("Settle was called %d times, want %d", calls, settleAttempts)
	}
	plugin.settle(settlement(1)) // the worker is still there for the next one
	plugin.background.flush()
	if got := ledger.settlements(); len(got) != 1 || got[0].ReservationID != "res_1" {
		t.Fatalf("settlements: %+v", got)
	}
	plugin.Cleanup()
}

func TestSettle_AFullQueueIsWrittenByTheCallerNotDropped(t *testing.T) {
	ledger := &fakeLedger{settleGate: make(chan struct{})}
	plugin := testPlugin(ledger, testPricer())
	plugin.start(2, 1, 0)

	// one is taken by the worker and waits at the gate, two fill the queue
	plugin.settle(settlement(0))
	waitFor(t, "the worker to take the first settlement", func() bool { return ledger.settleCall.Load() == 1 })
	plugin.settle(settlement(1))
	plugin.settle(settlement(2))

	// the fourth has nowhere to go: its caller must be the one writing it
	returned := make(chan struct{})
	go func() {
		plugin.settle(settlement(3))
		close(returned)
	}()
	waitFor(t, "the caller to write the overflow itself", func() bool { return ledger.settleCall.Load() == 2 })
	select {
	case <-returned:
		t.Fatal("settle returned while its settlement was neither queued nor written")
	default:
	}

	close(ledger.settleGate)
	<-returned
	plugin.Cleanup()
	got := ledger.settlements()
	seen := map[string]bool{}
	for _, st := range got {
		seen[st.ReservationID] = true
	}
	if len(got) != 4 || len(seen) != 4 {
		t.Fatalf("want each of the 4 settlements written once, got %+v", got)
	}
}

func TestCleanup_WritesWhatIsQueuedAndLaterSettlementsStillLand(t *testing.T) {
	ledger := &fakeLedger{settleGate: make(chan struct{})}
	plugin := testPlugin(ledger, testPricer())
	plugin.start(64, 2, 0)
	for i := range 20 {
		plugin.settle(settlement(i))
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(ledger.settleGate)
	}()
	if err := plugin.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if got := ledger.settlements(); len(got) != 20 {
		t.Fatalf("Cleanup returned with %d of 20 settlements written", len(got))
	}

	// a response that finishes after shutdown began: no panic, and still written
	plugin.settle(settlement(20))
	if got := ledger.settlements(); len(got) != 21 {
		t.Fatalf("a settlement after Cleanup was lost: %d", len(got))
	}
	if err := plugin.Cleanup(); err != nil { // and a second Cleanup is harmless
		t.Fatal(err)
	}
}

func TestCleanup_DoesNotHangOnALedgerThatNeverAnswers(t *testing.T) {
	ledger := &fakeLedger{settleGate: make(chan struct{})}
	defer close(ledger.settleGate)
	plugin := testPlugin(ledger, testPricer())
	plugin.shutdownGrace = 30 * time.Millisecond
	plugin.start(8, 2, time.Hour)
	for i := range 5 {
		plugin.settle(settlement(i))
	}
	done := make(chan struct{})
	go func() {
		plugin.Cleanup()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Cleanup did not return after its grace period")
	}
}

func TestSettle_ConcurrentSettlementsAndShutdownLoseNothing(t *testing.T) {
	ledger := &fakeLedger{}
	plugin := testPlugin(ledger, testPricer())
	plugin.start(4, 2, 0)
	const n = 200
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			plugin.settle(settlement(i))
		}()
		if i == n/2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				plugin.Cleanup()
			}()
		}
	}
	wg.Wait()
	plugin.Cleanup()
	seen := map[string]int{}
	for _, st := range ledger.settlements() {
		seen[st.ReservationID]++
	}
	if len(seen) != n {
		t.Fatalf("%d of %d settlements were written", len(seen), n)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("%s was written %d times", id, count)
		}
	}
}

type fakeReaper struct {
	calls atomic.Int64
	err   error
}

func (f *fakeReaper) Reap(context.Context) (int64, error) {
	f.calls.Add(1)
	return 1, f.err
}

func TestReaper_RunsOnItsIntervalSurvivesErrorsAndStopsOnCleanup(t *testing.T) {
	reaper := &fakeReaper{err: context.DeadlineExceeded}
	plugin := testPlugin(&fakeLedger{}, testPricer())
	plugin.reaper = reaper
	plugin.start(1, 1, 2*time.Millisecond)
	waitFor(t, "the reaper to run repeatedly despite failing", func() bool { return reaper.calls.Load() >= 3 })
	plugin.Cleanup()
	after := reaper.calls.Load()
	time.Sleep(20 * time.Millisecond)
	if now := reaper.calls.Load(); now != after {
		t.Fatalf("the reaper ran %d more times after Cleanup", now-after)
	}
}

// 50 requests at once through the hooks, the queue and real Postgres as the gateway role.
func TestBackground_FiftyConcurrentRequestsAgainstTheRealLedger(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	plugin := withMarkups(newPlugin(store, testPricer(), bifrost.NewNoOpLogger(), &Config{}), 0, nil)
	plugin.reaper = store
	plugin.start(8, 2, 10*time.Millisecond) // a small queue, so some settlements overflow to the caller
	// each request holds a little over 2000 and costs 2000: 200000 covers all fifty at once
	org := db.organization(t, 200_000)

	var wg sync.WaitGroup
	var refused atomic.Int64
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := testContext(org)
			if _, sc, _ := plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil)); sc != nil {
				refused.Add(1)
				return
			}
			plugin.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)
		}()
	}
	wg.Wait()
	if err := plugin.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if refused.Load() != 0 {
		t.Fatalf("%d requests were refused with a balance that covers all of them", refused.Load())
	}
	s := db.consistent(t, org)
	if s.Balance != 100_000 || s.Held != 0 || s.UsageEntries != 50 {
		t.Fatalf("after 50 requests of 2000 each: %+v", s)
	}
}

// A hold whose settlement never arrives is given back by the reaper loop, not only by a test
// calling Reap.
func TestBackground_TheReaperLoopReleasesAnAbandonedHold(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	plugin := withMarkups(newPlugin(store, testPricer(), bifrost.NewNoOpLogger(), &Config{}), 0, nil)
	plugin.reaper = store
	org := db.organization(t, 5000)

	if _, got, err := store.Reserve(context.Background(), org, "", 4000, 50*time.Millisecond); err != nil || got != Reserved {
		t.Fatalf("reserve: %v %v", got, err)
	}
	if s := db.consistent(t, org); s.Held != 4000 {
		t.Fatalf("held: %+v", s)
	}
	plugin.start(1, 1, 20*time.Millisecond)
	defer plugin.Cleanup()
	waitFor(t, "the reaper to release the expired hold", func() bool { return db.consistent(t, org).Held == 0 })
	if s := db.consistent(t, org); s.Balance != 5000 || s.UsageEntries != 0 {
		t.Fatalf("a released hold must cost nothing: %+v", s)
	}
}

// Step 4.3's first criterion, through the hooks: 50 requests arrive together on a balance that
// covers 10 holds. Exactly 10 reach the provider, 40 get 402, and the balance stays positive.
func TestConcurrent_FiftyRequestsOnABalanceForTenAdmitExactlyTen(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	plugin := withMarkups(newPlugin(store, testPricer(), bifrost.NewNoOpLogger(), &Config{}), 0, nil)
	plugin.start(8, 2, 0)

	// what one request holds, measured rather than assumed
	probe := db.organization(t, usd)
	plugin.PreLLMHook(testContext(probe), chatRequest(schemas.ChatCompletionRequest, nil))
	hold := db.consistent(t, probe).Held
	if hold <= 2000 {
		t.Fatalf("a request that can cost 2000 held %d", hold)
	}

	balance := 10*hold + hold/2
	org := db.organization(t, balance)
	contexts := make([]*schemas.BifrostContext, 50)
	admitted := make([]bool, 50)
	var wg sync.WaitGroup
	for i := range contexts {
		contexts[i] = testContext(org)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, sc, _ := plugin.PreLLMHook(contexts[i], chatRequest(schemas.ChatCompletionRequest, nil))
			if sc == nil {
				admitted[i] = true
			} else if status, errorType := refusalOf(t, sc); status != 402 || errorType != ErrorTypeInsufficientCredits {
				t.Errorf("refused with %d %s", status, errorType)
			}
		}()
	}
	wg.Wait()
	count := 0
	for _, ok := range admitted {
		if ok {
			count++
		}
	}
	if count != 10 {
		t.Fatalf("%d requests were admitted on a balance that covers 10 holds", count)
	}
	if s := db.consistent(t, org); s.Held != 10*hold || s.Balance != balance {
		t.Fatalf("while the 10 are in flight: %+v", s)
	}

	for i, ok := range admitted {
		if ok {
			wg.Add(1)
			go func() {
				defer wg.Done()
				plugin.PostLLMHook(contexts[i], chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)
			}()
		}
	}
	wg.Wait()
	plugin.Cleanup()
	if s := db.consistent(t, org); s.Balance != balance-10*2000 || s.Held != 0 || s.UsageEntries != 10 {
		t.Fatalf("after the 10 settled: %+v", s)
	}
}

// flush waits until every accepted settlement has been written or given up on.
func (bg *background) flush() {
	bg.pending.Wait()
}

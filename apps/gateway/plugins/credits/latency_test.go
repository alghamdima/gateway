package credits

import (
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

// TestLatency_Report measures what the plugin adds to a request: PreLLMHook (one round trip to
// Postgres, on the request path) and PostLLMHook (a queue push). It asserts nothing; it prints
// the numbers recorded in docs/platform/credits-latency.md. Run with CREDITS_LATENCY=1.
func TestLatency_Report(t *testing.T) {
	if os.Getenv("CREDITS_LATENCY") == "" {
		t.Skip("CREDITS_LATENCY is not set")
	}
	db := newTestDB(t)
	store := NewStore(db.ledger)
	plugin := withMarkups(newPlugin(store, testPricer(), bifrost.NewNoOpLogger(), &Config{}), 0, nil)
	plugin.reaper = store
	plugin.start(defaultSettleQueueSize, defaultSettleWorkers, defaultReapInterval)
	defer plugin.Cleanup()
	orgs := make([]string, 50)
	for i := range orgs {
		orgs[i] = db.organization(t, 1_000_000*usd)
	}

	// shared: every request is on orgs[0], so they all contend for one account row
	run := func(concurrency, perWorker int, shared bool) (pre, post []time.Duration) {
		var mu sync.Mutex
		var wg sync.WaitGroup
		for worker := range concurrency {
			org := orgs[0]
			if !shared {
				org = orgs[worker]
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range perWorker {
					ctx := testContext(org)
					start := time.Now()
					_, sc, _ := plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil))
					mid := time.Now()
					if sc != nil {
						t.Errorf("refused: %+v", sc.Error)
						return
					}
					plugin.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)
					end := time.Now()
					mu.Lock()
					pre = append(pre, mid.Sub(start))
					post = append(post, end.Sub(mid))
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		plugin.background.flush()
		return pre, post
	}
	report := func(name string, d []time.Duration) {
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		at := func(q float64) time.Duration { return d[min(len(d)-1, int(q*float64(len(d))))] }
		t.Logf("%-28s n=%d p50=%v p95=%v p99=%v max=%v", name, len(d), at(0.50), at(0.95), at(0.99), d[len(d)-1])
	}

	run(1, 200, true) // warm the pool
	for _, c := range []int{1, 10, 50} {
		for _, shared := range []bool{true, false} {
			if c == 1 && !shared {
				continue
			}
			pre, post := run(c, 2000/c, shared)
			t.Logf("concurrency %d, one organization for all: %v", c, shared)
			report("  PreLLMHook (reserve)", pre)
			report("  PostLLMHook (enqueue)", post)
		}
	}
	for _, org := range orgs {
		if s := db.consistent(t, org); s.Held != 0 {
			t.Fatalf("holds left after the run: %+v", s)
		}
	}
}

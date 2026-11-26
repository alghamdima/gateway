package credits

import (
	"context"
	"sync"
	"time"
)

const (
	defaultSettleQueueSize = 1024
	defaultSettleWorkers   = 4
	defaultReapInterval    = time.Minute
	settleAttempts         = 5
	settleBackoff          = 200 * time.Millisecond
	settleBackoffMax       = 5 * time.Second
	defaultShutdownGrace   = 30 * time.Second
)

// background owns what runs off the request path: the settlement workers and the reaper.
//
// A settlement is written after the response has been produced, so it is queued and a worker
// writes it, retrying on failure. The hold taken in PreLLMHook keeps the balance safe while a
// settlement waits. When the queue is full the caller writes the settlement itself instead of
// dropping it. Nothing here is durable: settlements still queued when the process dies are
// lost, their holds are released by the reaper, and the daily reconciliation against the
// gateway's log finds the missing charge (ADR 0002).
type background struct {
	plugin  *Plugin
	queue   chan Settlement
	pending sync.WaitGroup // settlements accepted and not yet written or given up on
	workers sync.WaitGroup
	cancel  context.CancelFunc
	mu      sync.RWMutex // guards closed against a settle racing stop
	closed  bool
}

// start launches the workers and, when reapInterval is positive, the reaper.
func (p *Plugin) start(queueSize, workers int, reapInterval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	bg := &background{plugin: p, queue: make(chan Settlement, queueSize), cancel: cancel}
	p.background = bg
	for range workers {
		bg.workers.Add(1)
		go func() {
			defer bg.workers.Done()
			for st := range bg.queue {
				p.write(st)
				bg.pending.Done()
			}
		}()
	}
	if p.prices != nil {
		bg.workers.Add(1)
		go func() {
			defer bg.workers.Done()
			p.refreshLoop(ctx, markupRefreshInterval, markupRetryInterval)
		}()
	}
	if reapInterval > 0 && p.reaper != nil {
		bg.workers.Add(1)
		go func() {
			defer bg.workers.Done()
			ticker := time.NewTicker(reapInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					bg.reap(ctx)
				}
			}
		}()
	}
}

// settle hands a settlement to a worker. When the queue is full, the plugin is shutting down,
// or there is no background at all, the caller writes it on the spot: slowing a response down
// is better than losing its charge.
func (p *Plugin) settle(st Settlement) {
	if bg := p.background; bg != nil && bg.enqueue(st) {
		return
	}
	p.write(st)
}

func (bg *background) enqueue(st Settlement) bool {
	bg.mu.RLock()
	defer bg.mu.RUnlock()
	if bg.closed {
		return false
	}
	bg.pending.Add(1)
	select {
	case bg.queue <- st:
		return true
	default:
		bg.pending.Done()
		return false
	}
}

// write settles with retries. Settle is idempotent, so trying again after an error that may
// or may not have reached the database is safe.
func (p *Plugin) write(st Settlement) {
	var err error
	delay := p.settleBackoff
	for attempt := 1; attempt <= settleAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), ledgerTimeout)
		_, err = p.ledger.Settle(ctx, st)
		cancel()
		if err == nil {
			return
		}
		if attempt < settleAttempts {
			time.Sleep(delay)
			delay = min(delay*2, settleBackoffMax)
		}
	}
	// the hold stays until the reaper releases it; reconciliation has to find this charge
	p.logger.Error("credits: settlement of %s for organization %s (%d micro-USD, request %s) was not written after %d attempts: %v",
		st.ReservationID, st.OrganizationID, st.Cost, st.Reference, settleAttempts, err)
}

func (bg *background) reap(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, ledgerTimeout)
	defer cancel()
	released, err := bg.plugin.reaper.Reap(ctx)
	if err != nil {
		bg.plugin.logger.Warn("credits: reaper failed: %v", err)
		return
	}
	if released > 0 {
		// each one is a request whose settlement never arrived
		bg.plugin.logger.Warn("credits: reaper released %d expired reservations", released)
	}
}

// stop ends the reaper and lets the workers write what is queued, for at most the plugin's
// shutdown grace. Settlements that arrive afterwards are written by their callers. What is
// still unwritten when the grace runs out is lost with the process: the reaper of another
// instance releases those holds and reconciliation finds the charges.
func (bg *background) stop() {
	bg.mu.Lock()
	if bg.closed {
		bg.mu.Unlock()
		return
	}
	bg.closed = true
	bg.mu.Unlock()
	bg.cancel()
	close(bg.queue)
	done := make(chan struct{})
	go func() {
		bg.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(bg.plugin.shutdownGrace):
		bg.plugin.logger.Error("credits: shut down after %s with settlements still unwritten", bg.plugin.shutdownGrace)
	}
}

package credits

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const usd = int64(1_000_000)

func TestReserve_HoldsWhenTheBalanceCovers(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	org := db.organization(t, 10*usd)

	id, got, err := store.Reserve(context.Background(), org, "", 4*usd, testFX, time.Minute)
	if err != nil || got != Reserved || id == "" {
		t.Fatalf("Reserve = %q, %d, %v", id, got, err)
	}
	if s := db.consistent(t, org); s.Balance != 10*usd || s.Held != 4*usd {
		t.Fatalf("state after reserve: %+v", s)
	}
}

func TestReserve_RefusesWhatTheBalanceDoesNotCover(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	ctx := context.Background()
	org := db.organization(t, 10*usd)

	if _, got, err := store.Reserve(ctx, org, "", 10*usd+1, testFX, time.Minute); err != nil || got == Reserved {
		t.Fatalf("more than the balance: outcome=%d err=%v", got, err)
	}
	if _, got, err := store.Reserve(ctx, org, "", 7*usd, testFX, time.Minute); err != nil || got != Reserved {
		t.Fatalf("first hold: outcome=%d err=%v", got, err)
	}
	// 3 USD left to hold: the spendable amount is balance minus what is already held
	if _, got, err := store.Reserve(ctx, org, "", 3*usd+1, testFX, time.Minute); err != nil || got == Reserved {
		t.Fatalf("more than balance - held: outcome=%d err=%v", got, err)
	}
	if _, got, err := store.Reserve(ctx, org, "", 3*usd, testFX, time.Minute); err != nil || got != Reserved {
		t.Fatalf("exactly balance - held: outcome=%d err=%v", got, err)
	}
	if s := db.consistent(t, org); s.Held != 10*usd {
		t.Fatalf("state: %+v", s)
	}
}

func TestReserve_RefusesAnOrganizationWithoutAnAccount(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	ctx := context.Background()

	if _, got, err := store.Reserve(ctx, db.organization(t, 0), "", 1, testFX, time.Minute); err != nil || got == Reserved {
		t.Fatalf("no account: outcome=%d err=%v", got, err)
	}
	if _, got, err := store.Reserve(ctx, "org_that_does_not_exist", "", 1, testFX, time.Minute); err != nil || got == Reserved {
		t.Fatalf("no organization: outcome=%d err=%v", got, err)
	}
	if _, _, err := store.Reserve(ctx, db.organization(t, usd), "", 0, testFX, time.Minute); err == nil {
		t.Fatal("a zero reservation must be an error")
	}
}

func TestReserve_ConcurrentRequestsNeverHoldMoreThanTheBalance(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	org := db.organization(t, 10*usd)

	var granted atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, got, err := store.Reserve(context.Background(), org, "", usd, testFX, time.Minute)
			if err != nil {
				t.Error(err)
			}
			if got == Reserved {
				granted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if granted.Load() != 10 {
		t.Fatalf("50 requests of 1 USD against 10 USD: %d were granted, want 10", granted.Load())
	}
	if s := db.consistent(t, org); s.Held != 10*usd || s.Balance != 10*usd {
		t.Fatalf("state: %+v", s)
	}
}

func TestSettle_ChargesOnceAndReleasesTheHold(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	ctx := context.Background()
	org := db.organization(t, 10*usd)
	id := must[string](t)(reserve(store, org, 4*usd))

	st := Settlement{
		ReservationID: id, OrganizationID: org, Cost: 1_250_000, CostMicroUSD: 333_333, FX: testFX,
		IdempotencyKey: "nonce:req_1:0:0", Reference: "req_1",
		Metadata: map[string]any{"provider_cost_usd": 0.333333, "model": "gpt-4o-mini"},
	}
	first := must[SettleResult](t)(store.Settle(ctx, st))
	if !first.Charged || !first.Released {
		t.Fatalf("first settle: %+v", first)
	}
	if s := db.consistent(t, org); s.Balance != 8_750_000 || s.Held != 0 || s.UsageEntries != 1 {
		t.Fatalf("state after settle: %+v", s)
	}
	// the entry stores the USD cost and the rate as columns; the hold stored the rate too
	var costMicroUSD int64
	var entry, hold fxRate
	if err := db.admin.QueryRow(ctx, `
		SELECT e.cost_micro_usd, e.usd_to_sar_rate_e6, e.fx_spread_bps, e.effective_usd_to_sar_rate_e6,
		       r.usd_to_sar_rate_e6, r.fx_spread_bps, r.effective_usd_to_sar_rate_e6
		  FROM credit_ledger_entry e, credit_reservation r WHERE e.idempotency_key = $1 AND r.id = $2`, st.IdempotencyKey, id).Scan(
		&costMicroUSD, &entry.officialE6, &entry.spreadBps, &entry.effectiveE6, &hold.officialE6, &hold.spreadBps, &hold.effectiveE6); err != nil || costMicroUSD != 333_333 || entry != testFX || hold != testFX {
		t.Fatalf("entry: %d micro-USD at %+v, hold at %+v (%v)", costMicroUSD, entry, hold, err)
	}
	// the database holds the effective rate to official + spread, rounded up
	if _, err := db.admin.Exec(ctx, `INSERT INTO credit_reservation (id, organization_id, amount, expires_at, usd_to_sar_rate_e6, fx_spread_bps, effective_usd_to_sar_rate_e6) VALUES ('res_wrong_rate', $1, 1, now(), 3750000, 250, 3750000)`, org); err == nil {
		t.Fatal("a hold whose effective rate is not official + spread was accepted")
	}

	// the same attempt delivered again: after a crash, a retry, or by another instance
	for range 3 {
		again := must[SettleResult](t)(store.Settle(ctx, st))
		if again.Charged || again.Released {
			t.Fatalf("replay changed something: %+v", again)
		}
	}
	if s := db.consistent(t, org); s.Balance != 8_750_000 || s.Held != 0 || s.UsageEntries != 1 {
		t.Fatalf("state after replays: %+v", s)
	}

	var reference, model string
	var amount int64
	err := db.admin.QueryRow(ctx, `SELECT amount, reference, metadata->>'model' FROM credit_ledger_entry WHERE idempotency_key = $1`, st.IdempotencyKey).Scan(&amount, &reference, &model)
	if err != nil || amount != -1_250_000 || reference != "req_1" || model != "gpt-4o-mini" {
		t.Fatalf("entry: amount=%d reference=%q model=%q err=%v", amount, reference, model, err)
	}
}

func TestSettle_ConcurrentDeliveriesOfOneAttemptChargeOnce(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	org := db.organization(t, 10*usd)
	id := must[string](t)(reserve(store, org, 2*usd))

	var charged, released atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := store.Settle(context.Background(), Settlement{ReservationID: id, OrganizationID: org, Cost: usd, FX: testFX, IdempotencyKey: "nonce:req_c:0:0"})
			if err != nil {
				t.Error(err)
			}
			if res.Charged {
				charged.Add(1)
			}
			if res.Released {
				released.Add(1)
			}
		}()
	}
	wg.Wait()

	if charged.Load() != 1 || released.Load() != 1 {
		t.Fatalf("20 deliveries: charged %d times, released %d times", charged.Load(), released.Load())
	}
	if s := db.consistent(t, org); s.Balance != 9*usd || s.Held != 0 || s.UsageEntries != 1 {
		t.Fatalf("state: %+v", s)
	}
}

func TestSettle_ZeroCostReleasesTheHoldAndWritesNothing(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	org := db.organization(t, 10*usd)
	id := must[string](t)(reserve(store, org, 4*usd))

	res := must[SettleResult](t)(store.Settle(context.Background(), Settlement{ReservationID: id, OrganizationID: org, Cost: 0, IdempotencyKey: "nonce:req_failed:0:0"}))
	if res.Charged || !res.Released {
		t.Fatalf("settle: %+v", res)
	}
	if s := db.consistent(t, org); s.Balance != 10*usd || s.Held != 0 || s.UsageEntries != 0 {
		t.Fatalf("state: %+v", s)
	}
}

func TestSettle_CostAboveTheHoldCanTakeTheBalanceBelowZero(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	ctx := context.Background()
	org := db.organization(t, usd)
	id := must[string](t)(reserve(store, org, usd))

	// the estimate was 1 USD, the attempt really cost 1.40
	res := must[SettleResult](t)(store.Settle(ctx, Settlement{ReservationID: id, OrganizationID: org, Cost: 1_400_000, FX: testFX, IdempotencyKey: "nonce:req_over:0:0"}))
	if !res.Charged || !res.Released {
		t.Fatalf("settle: %+v", res)
	}
	if s := db.consistent(t, org); s.Balance != -400_000 || s.Held != 0 {
		t.Fatalf("state: %+v", s)
	}
	// and the next request is refused until a top-up
	if _, got, err := store.Reserve(ctx, org, "", 1, testFX, time.Minute); err != nil || got == Reserved {
		t.Fatalf("reserve on a negative balance: outcome=%d err=%v", got, err)
	}
}

func TestSettle_RejectsBadInput(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	ctx := context.Background()
	org := db.organization(t, usd)
	id := must[string](t)(reserve(store, org, usd))

	if _, err := store.Settle(ctx, Settlement{ReservationID: id, OrganizationID: org, Cost: -1, IdempotencyKey: "k"}); err == nil {
		t.Error("a negative cost must be an error")
	}
	if _, err := store.Settle(ctx, Settlement{ReservationID: id, OrganizationID: org, Cost: 1, IdempotencyKey: "k_no_rate"}); err == nil {
		t.Fatal("a charge without its rate was accepted")
	}
	if _, err := store.Settle(ctx, Settlement{ReservationID: id, OrganizationID: org, Cost: 1}); err == nil {
		t.Error("a missing idempotency key must be an error")
	}
	// someone else's reservation id does not release this organization's hold
	other := db.organization(t, usd)
	res := must[SettleResult](t)(store.Settle(ctx, Settlement{ReservationID: id, OrganizationID: other, Cost: 0, IdempotencyKey: "k2"}))
	if res.Released {
		t.Error("released a reservation that belongs to another organization")
	}
	if s := db.consistent(t, org); s.Held != usd {
		t.Fatalf("state: %+v", s)
	}
}

func TestReap_ReleasesExpiredHoldsOnly(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	ctx := context.Background()
	org := db.organization(t, 10*usd)
	other := db.organization(t, 10*usd)

	expired1 := must[string](t)(reserve(store, org, 2*usd))
	expired2 := must[string](t)(reserve(store, org, 3*usd))
	live := must[string](t)(reserve(store, org, usd))
	must[string](t)(reserve(store, other, 4*usd))
	if _, err := db.admin.Exec(ctx, `UPDATE credit_reservation SET expires_at = now() - interval '1 second' WHERE id = ANY($1)`, []string{expired1, expired2}); err != nil {
		t.Fatal(err)
	}

	if n := must[int64](t)(store.Reap(ctx)); n != 2 {
		t.Fatalf("reaped %d reservations, want 2", n)
	}
	if s := db.consistent(t, org); s.Held != usd || s.Balance != 10*usd {
		t.Fatalf("state: %+v", s)
	}
	if s := db.consistent(t, other); s.Held != 4*usd {
		t.Fatalf("another organization's live hold was touched: %+v", s)
	}
	if n := must[int64](t)(store.Reap(ctx)); n != 0 {
		t.Fatalf("second reap released %d", n)
	}

	// the request behind a reaped hold finishes after all: it is charged, and its hold is not released twice
	res := must[SettleResult](t)(store.Settle(ctx, Settlement{ReservationID: expired1, OrganizationID: org, Cost: 500_000, FX: testFX, IdempotencyKey: "nonce:late:0:0"}))
	if !res.Charged || res.Released {
		t.Fatalf("late settle: %+v", res)
	}
	if s := db.consistent(t, org); s.Held != usd || s.Balance != 9_500_000 {
		t.Fatalf("state after late settle: %+v", s)
	}
	_ = live
}

func TestExtend_KeepsAnOpenHoldFromTheReaper(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	ctx := context.Background()
	org := db.organization(t, 10*usd)

	open := must[string](t)(reserve(store, org, 2*usd))
	settled := must[string](t)(reserve(store, org, usd))
	must[SettleResult](t)(store.Settle(ctx, Settlement{ReservationID: settled, OrganizationID: org, Cost: 0, IdempotencyKey: "nonce:extend:0:0"}))
	// both are past their lifetime when the extension arrives
	if _, err := db.admin.Exec(ctx, `UPDATE credit_reservation SET expires_at = now() - interval '1 second' WHERE id = ANY($1)`, []string{open, settled}); err != nil {
		t.Fatal(err)
	}

	if ok := must[bool](t)(store.Extend(ctx, open, time.Hour)); !ok {
		t.Fatal("an open hold was not extended")
	}
	if ok := must[bool](t)(store.Extend(ctx, settled, time.Hour)); ok {
		t.Fatal("a settled hold was extended")
	}
	var minutes float64
	if err := db.admin.QueryRow(ctx, `SELECT extract(epoch FROM expires_at - now()) / 60 FROM credit_reservation WHERE id = $1`, open).Scan(&minutes); err != nil || minutes < 59 || minutes > 61 {
		t.Fatalf("new lifetime %.1f minutes, %v", minutes, err)
	}
	if n := must[int64](t)(store.Reap(ctx)); n != 0 {
		t.Fatalf("reaped %d holds, want none", n)
	}
	if s := db.consistent(t, org); s.Held != 2*usd {
		t.Fatalf("state: %+v", s)
	}
}

// TestConcurrentTraffic is the Phase 4 check: many requests at once against a small
// balance never hold more than the balance, and the books agree afterwards.
func TestConcurrentTraffic_BooksAgreeAndHoldsNeverExceedTheBalance(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	org := db.organization(t, 10*usd)

	// each request reserves 1 USD and really costs 0.80
	var served, refused atomic.Int64
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			id, got, err := store.Reserve(ctx, org, "", usd, testFX, time.Minute)
			if err != nil {
				t.Error(err)
				return
			}
			if got != Reserved {
				refused.Add(1)
				return
			}
			if _, err := store.Settle(ctx, Settlement{ReservationID: id, OrganizationID: org, Cost: 800_000, FX: testFX, IdempotencyKey: fmt.Sprintf("nonce:traffic_%d:0:0", i)}); err != nil {
				t.Error(err)
				return
			}
			served.Add(1)
		}()
	}
	wg.Wait()

	s := db.consistent(t, org)
	if s.Held != 0 {
		t.Fatalf("holds left open: %+v", s)
	}
	if s.Balance != 10*usd-served.Load()*800_000 || s.UsageEntries != served.Load() {
		t.Fatalf("served %d, state %+v", served.Load(), s)
	}
	// How many are served depends on the interleaving, within hard bounds. At least 10: ten 1 USD
	// holds always fit in 10 USD. At most 12: after twelve charges of 0.80 only 0.40 is left, which
	// cannot cover a 1 USD hold. Holds in flight can make it 10 or 11 instead of 12. What must
	// never happen is a 13th, or a negative balance.
	if n := served.Load(); n < 10 || n > 12 || s.Balance < 0 {
		t.Fatalf("served %d requests (refused %d), balance %d: want 10 to 12 and no overdraft", n, refused.Load(), s.Balance)
	}
}

func reserve(store *Store, organizationID string, amount int64) (string, error) {
	id, got, err := store.Reserve(context.Background(), organizationID, "", amount, testFX, time.Minute)
	if err == nil && got != Reserved {
		err = fmt.Errorf("reservation of %d refused", amount)
	}
	return id, err
}

func TestGatewayRole_CannotMintCredit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	org := db.organization(t, 10*usd)
	hold := must[string](t)(reserve(NewStore(db.ledger), org, usd))

	for name, statement := range map[string]string{
		"a top-up entry":           `INSERT INTO credit_ledger_entry (id, organization_id, amount, kind, idempotency_key) VALUES ('mint1', $1, 1000000, 'top_up', 'mint1')`,
		"a positive usage entry":   `INSERT INTO credit_ledger_entry (id, organization_id, amount, kind, idempotency_key, cost_micro_usd, usd_to_sar_rate_e6, fx_spread_bps, effective_usd_to_sar_rate_e6) VALUES ('mint2', $1, 1000000, 'usage', 'mint2', 0, 3750000, 0, 3750000)`,
		"a raised balance":         `UPDATE credit_account SET balance = balance + 1000000 WHERE organization_id = $1`,
		"a changed account owner":  `UPDATE credit_account SET organization_id = organization_id WHERE organization_id = $1`,
		"a hold's amount":          `UPDATE credit_reservation SET amount = 1 WHERE organization_id = $1`,
		"a hold moved to an owner": `UPDATE credit_reservation SET organization_id = organization_id WHERE organization_id = $1`,
	} {
		if _, err := db.ledger.Exec(ctx, statement, org); err == nil {
			t.Errorf("gateway_ledger wrote %s", name)
		}
	}
	if s := db.consistent(t, org); s.Balance != 10*usd || s.Held != usd {
		t.Fatalf("state: %+v", s)
	}

	// what the gateway does still works: charge, release, extend, reap
	store := NewStore(db.ledger)
	if ok := must[bool](t)(store.Extend(ctx, hold, time.Minute)); !ok {
		t.Fatal("extend failed")
	}
	res := must[SettleResult](t)(store.Settle(ctx, Settlement{ReservationID: hold, OrganizationID: org, Cost: 400_000, FX: testFX, IdempotencyKey: "nonce:role:0:0"}))
	if !res.Charged || !res.Released {
		t.Fatalf("settle: %+v", res)
	}
	must[int64](t)(store.Reap(ctx))
	if s := db.consistent(t, org); s.Balance != 10*usd-400_000 || s.Held != 0 {
		t.Fatalf("state after settle: %+v", s)
	}
}

func TestGatewayRole_TheControlPlaneStillCredits(t *testing.T) {
	db := newTestDB(t)
	org := db.organization(t, usd)
	// the owning role (the control plane) records top-ups as before
	if _, err := db.admin.Exec(context.Background(), `INSERT INTO credit_ledger_entry (id, organization_id, amount, kind, idempotency_key) VALUES ('topup2', $1, 500000, 'top_up', 'topup2')`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := db.admin.Exec(context.Background(), `UPDATE credit_account SET balance = balance + 500000 WHERE organization_id = $1`, org); err != nil {
		t.Fatal(err)
	}
	if s := db.consistent(t, org); s.Balance != usd+500_000 {
		t.Fatalf("state: %+v", s)
	}
}

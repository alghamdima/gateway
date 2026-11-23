package credits

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Per-key spending limits are enforced on what the ledger charges, markup included
// (docs/platform/adr/0013-key-limits-in-the-ledger.md).

// apiKey stores a key the way the control plane does, with an optional limit.
func (db *testDB) apiKey(t *testing.T, organizationID, virtualKeyID string, limit *int64, reset *string) {
	t.Helper()
	_, err := db.admin.Exec(context.Background(), `
INSERT INTO api_key (id, organization_id, created_by, name, virtual_key_id, prefix, value_hash, limit_micro_usd, limit_reset)
VALUES ($1, $2, 'user_1', $1, $1, 'sk-test', $1, $3, $4)`, virtualKeyID, organizationID, limit, reset)
	if err != nil {
		t.Fatalf("api key: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestReserve_KeyLimitCountsSpendAndOpenHolds(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	ctx := context.Background()
	org := db.organization(t, 100*usd)
	db.apiKey(t, org, "vk_limited", ptr(5*usd), ptr("monthly"))

	first, outcome, err := store.Reserve(ctx, org, "vk_limited", 3*usd, time.Minute)
	if err != nil || outcome != Reserved {
		t.Fatalf("first hold: %d %v", outcome, err)
	}
	if _, outcome, _ := store.Reserve(ctx, org, "vk_limited", 2*usd, time.Minute); outcome != Reserved {
		t.Fatalf("hold up to the limit: %d", outcome)
	}
	if _, outcome, _ := store.Reserve(ctx, org, "vk_limited", 1, time.Minute); outcome != KeyLimitReached {
		t.Fatalf("one micro-USD over the limit: %d", outcome)
	}

	// the first attempt cost $1 of its $3 hold: $2 of room comes back, and $1 stays spent
	must[SettleResult](t)(store.Settle(ctx, Settlement{ReservationID: first, OrganizationID: org, Cost: usd, IdempotencyKey: "usage:" + first}))
	if _, outcome, _ := store.Reserve(ctx, org, "vk_limited", 2*usd+1, time.Minute); outcome != KeyLimitReached {
		t.Fatalf("over the room left: %d", outcome)
	}
	if _, outcome, _ := store.Reserve(ctx, org, "vk_limited", 2*usd, time.Minute); outcome != Reserved {
		t.Fatalf("exactly the room left: %d", outcome)
	}

	var keyOnEntry string
	if err := db.admin.QueryRow(ctx, `SELECT virtual_key_id FROM credit_ledger_entry WHERE idempotency_key = $1`, "usage:"+first).Scan(&keyOnEntry); err != nil || keyOnEntry != "vk_limited" {
		t.Fatalf("usage entry key = %q, %v", keyOnEntry, err)
	}
	// the organization's own balance was never the constraint
	if s := db.consistent(t, org); s.Balance != 99*usd || s.Held != 4*usd {
		t.Fatalf("account: %+v", s)
	}
}

func TestReserve_KeyLimitWindowStartsAtTheUTCBoundary(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	ctx := context.Background()
	org := db.organization(t, 100*usd)

	for _, tc := range []struct {
		reset  *string
		before string // a moment just before the current window
		inside string // a moment inside it
	}{
		{ptr("daily"), "date_trunc('day', now() AT TIME ZONE 'UTC') - interval '1 second'", "date_trunc('day', now() AT TIME ZONE 'UTC')"},
		{ptr("weekly"), "date_trunc('week', now() AT TIME ZONE 'UTC') - interval '1 second'", "date_trunc('week', now() AT TIME ZONE 'UTC')"},
		{ptr("monthly"), "date_trunc('month', now() AT TIME ZONE 'UTC') - interval '1 second'", "date_trunc('month', now() AT TIME ZONE 'UTC')"},
		{nil, "'2000-01-01'::timestamp", "'2000-01-01'::timestamp"},
	} {
		name := "never"
		if tc.reset != nil {
			name = *tc.reset
		}
		t.Run(name, func(t *testing.T) {
			key := "vk_window_" + name
			db.apiKey(t, org, key, ptr(5*usd), tc.reset)
			spend := func(id, at string, amount int64) {
				if _, err := db.admin.Exec(ctx, `INSERT INTO credit_ledger_entry (id, organization_id, amount, kind, idempotency_key, virtual_key_id, created_at)
VALUES ($1, $2, $3, 'usage', $1, $4, `+at+`)`, id, org, -amount, key); err != nil {
					t.Fatal(err)
				}
			}
			spend(key+"_before", tc.before, 4*usd)
			spend(key+"_inside", tc.inside, 1*usd)

			// daily, weekly, monthly: only the $1 inside the window counts, so $4 is left;
			// never: both count, so nothing is left
			room := 4 * usd
			if tc.reset == nil {
				room = 0
			}
			if _, outcome, _ := store.Reserve(ctx, org, key, room+1, time.Minute); outcome != KeyLimitReached {
				t.Fatalf("over the room: %d", outcome)
			}
			if room > 0 {
				if _, outcome, _ := store.Reserve(ctx, org, key, room, time.Minute); outcome != Reserved {
					t.Fatalf("the room: %d", outcome)
				}
			}
		})
	}
}

func TestReserve_ConcurrentHoldsNeverExceedTheKeyLimit(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	org := db.organization(t, 1000*usd)
	db.apiKey(t, org, "vk_race", ptr(5*usd), ptr("daily"))

	var granted, limited atomic.Int64
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, outcome, err := store.Reserve(context.Background(), org, "vk_race", usd, time.Minute)
			if err != nil {
				t.Error(err)
				return
			}
			switch outcome {
			case Reserved:
				granted.Add(1)
			case KeyLimitReached:
				limited.Add(1)
			}
		}()
	}
	wg.Wait()
	if granted.Load() != 5 || limited.Load() != 35 {
		t.Fatalf("granted %d, limited %d: want 5 and 35", granted.Load(), limited.Load())
	}
}

func TestReserve_KeyWithoutLimitOrUnknownOnlyNeedsTheBalance(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	ctx := context.Background()
	org := db.organization(t, 10*usd)
	db.apiKey(t, org, "vk_unlimited", nil, nil)

	for _, key := range []string{"vk_unlimited", "vk_made_outside_the_control_plane"} {
		id, outcome, err := store.Reserve(ctx, org, key, 4*usd, time.Minute)
		if err != nil || outcome != Reserved {
			t.Fatalf("%s: %d %v", key, outcome, err)
		}
		var recorded string
		if err := db.admin.QueryRow(ctx, `SELECT virtual_key_id FROM credit_reservation WHERE id = $1`, id).Scan(&recorded); err != nil || recorded != key {
			t.Fatalf("%s: reservation key %q, %v", key, recorded, err)
		}
	}
	if _, outcome, _ := store.Reserve(ctx, org, "vk_unlimited", 2*usd+1, time.Minute); outcome != InsufficientCredits {
		t.Fatalf("over the balance: %d", outcome)
	}
}

func TestReserve_TheGatewayRoleCannotReadKeyHashes(t *testing.T) {
	db := newTestDB(t)
	org := db.organization(t, usd)
	db.apiKey(t, org, "vk_private", nil, nil)
	var hash string
	err := db.ledger.QueryRow(context.Background(), `SELECT value_hash FROM api_key LIMIT 1`).Scan(&hash)
	if err == nil {
		t.Fatalf("gateway_ledger read a key hash: %q", hash)
	}
}

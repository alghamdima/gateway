package credits

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store runs the three statements that move money in the control plane's ledger
// (docs/platform/adr/0001-credits-ledger.md, 0009-credits-plugin-hooks.md).
// Every amount is micro-USD (1 USD = 1,000,000). Each method is one SQL statement,
// so it is atomic without an explicit transaction and safe under concurrency.
// The connection only needs the gateway_ledger role.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps a pool. The caller owns the pool and closes it.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// reserveSQL raises held only if the spendable balance covers the amount, and records
// the reservation in the same statement. No returned row means the hold was refused:
// either the balance is too low or the organization has no account.
const reserveSQL = `
WITH hold AS (
  UPDATE credit_account
     SET held = held + $3, updated_at = now()
   WHERE organization_id = $2 AND balance - held >= $3
  RETURNING organization_id
)
INSERT INTO credit_reservation (id, organization_id, amount, expires_at)
SELECT $1, organization_id, $3, now() + make_interval(secs => $4)
  FROM hold
RETURNING id`

// Reserve holds amount against the organization's balance. ok is false when the balance
// does not cover it. amount must be positive.
func (s *Store) Reserve(ctx context.Context, organizationID string, amount int64, ttl time.Duration) (reservationID string, ok bool, err error) {
	if amount <= 0 {
		return "", false, fmt.Errorf("credits: reservation amount must be positive, got %d", amount)
	}
	id := uuid.NewString()
	var got string
	err = s.pool.QueryRow(ctx, reserveSQL, id, organizationID, amount, ttl.Seconds()).Scan(&got)
	if err == pgx.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("credits: reserve: %w", err)
	}
	return got, true, nil
}

// Settlement is the outcome of one attempt.
type Settlement struct {
	// ReservationID is the hold taken for this attempt.
	ReservationID  string
	OrganizationID string
	// Cost is what the attempt is charged, in micro-USD. Zero releases the hold and writes no entry.
	Cost int64
	// IdempotencyKey identifies the attempt. Settling the same key again changes nothing.
	IdempotencyKey string
	// Reference is the request id, for support and reconciliation.
	Reference string
	// Metadata is stored with the entry: provider cost before markup, model, provider.
	Metadata map[string]any
}

// SettleResult says what a settlement changed.
type SettleResult struct {
	// Charged is true when this call appended the usage entry.
	Charged bool
	// Released is true when this call released the hold.
	Released bool
}

// settleSQL closes the reservation, appends the usage entry and moves the account, in one
// statement. Each part is guarded on its own, so a replay does nothing: the reservation is
// already settled and the idempotency key already exists. A reservation the reaper has
// already released is not released twice, and its usage is still charged.
const settleSQL = `
WITH released AS (
  UPDATE credit_reservation
     SET settled_at = now()
   WHERE id = $1 AND organization_id = $2 AND settled_at IS NULL
  RETURNING amount
), charged AS (
  INSERT INTO credit_ledger_entry (id, organization_id, amount, kind, idempotency_key, reference, metadata)
  SELECT $3, $2, -$4::bigint, 'usage', $5, $6, $7::jsonb
   WHERE $4::bigint > 0
  ON CONFLICT (idempotency_key) DO NOTHING
  RETURNING amount
), account AS (
  UPDATE credit_account
     SET balance = balance + coalesce((SELECT amount FROM charged), 0),
         held = held - coalesce((SELECT amount FROM released), 0),
         updated_at = now()
   WHERE organization_id = $2
     AND (EXISTS (SELECT 1 FROM charged) OR EXISTS (SELECT 1 FROM released))
  RETURNING 1
)
SELECT (SELECT count(*) FROM charged) > 0, (SELECT count(*) FROM released) > 0, (SELECT count(*) FROM account)`

// Settle charges an attempt and releases its hold. It is idempotent on IdempotencyKey.
func (s *Store) Settle(ctx context.Context, st Settlement) (SettleResult, error) {
	if st.Cost < 0 {
		return SettleResult{}, fmt.Errorf("credits: settlement cost cannot be negative, got %d", st.Cost)
	}
	if st.IdempotencyKey == "" {
		return SettleResult{}, fmt.Errorf("credits: settlement needs an idempotency key")
	}
	metadata := []byte("null")
	if st.Metadata != nil {
		var err error
		if metadata, err = json.Marshal(st.Metadata); err != nil {
			return SettleResult{}, fmt.Errorf("credits: settlement metadata: %w", err)
		}
	}
	var res SettleResult
	var accounts int
	err := s.pool.QueryRow(ctx, settleSQL,
		st.ReservationID, st.OrganizationID, uuid.NewString(), st.Cost, st.IdempotencyKey, st.Reference, string(metadata),
	).Scan(&res.Charged, &res.Released, &accounts)
	if err != nil {
		return SettleResult{}, fmt.Errorf("credits: settle: %w", err)
	}
	if (res.Charged || res.Released) && accounts != 1 {
		// cannot happen while credit_account rows are never deleted under an open reservation;
		// if it does, the ledger and the cache disagree and reconciliation must look at it
		return res, fmt.Errorf("credits: settle moved the ledger for %s but found %d account rows", st.OrganizationID, accounts)
	}
	return res, nil
}

// reapSQL releases every reservation that is past its lifetime and still open.
const reapSQL = `
WITH expired AS (
  UPDATE credit_reservation
     SET settled_at = now()
   WHERE settled_at IS NULL AND expires_at < now()
  RETURNING organization_id, amount
), totals AS (
  SELECT organization_id, sum(amount) AS amount, count(*) AS n FROM expired GROUP BY organization_id
), account AS (
  UPDATE credit_account a
     SET held = a.held - t.amount, updated_at = now()
    FROM totals t
   WHERE a.organization_id = t.organization_id
  RETURNING 1
)
SELECT coalesce((SELECT sum(n) FROM totals), 0)`

// Reap releases holds whose request never settled (a crashed gateway, a lost settlement).
// It returns how many reservations it released.
func (s *Store) Reap(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, reapSQL).Scan(&n); err != nil {
		return 0, fmt.Errorf("credits: reap: %w", err)
	}
	return n, nil
}

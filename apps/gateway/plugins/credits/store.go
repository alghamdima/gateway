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
// Every amount is micro-SAR (1 SAR = 1,000,000; docs/platform/adr/0017-sar-ledger.md). Each
// hold and charge also records the official USD to SAR rate, the bank's spread and the
// effective rate it was converted at. Each method is one SQL statement,
// so it is atomic without an explicit transaction and safe under concurrency.
// The connection only needs the gateway_ledger role.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps a pool. The caller owns the pool and closes it.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ReserveOutcome says whether a hold was taken, and if not, why.
type ReserveOutcome int

const (
	// Reserved means the hold was taken.
	Reserved ReserveOutcome = iota
	// InsufficientCredits means the organization's spendable balance does not cover it,
	// or the organization has no account.
	InsufficientCredits
	// KeyLimitReached means the API key's spending limit for its current window does not
	// cover it (docs/platform/adr/0013-key-limits-in-the-ledger.md).
	KeyLimitReached
)

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
INSERT INTO credit_reservation (id, organization_id, amount, expires_at, virtual_key_id,
                                usd_to_sar_rate_e6, fx_spread_bps, effective_usd_to_sar_rate_e6)
SELECT $1, organization_id, $3, now() + make_interval(secs => $4), nullif($5, ''), $6, $7, $8
  FROM hold
RETURNING id`

// keyLimitSQL reads the limit the control plane stored for a virtual key. No row: the
// key was not made by the control plane, and only the balance applies.
const keyLimitSQL = `SELECT limit_micro_sar, limit_reset FROM api_key WHERE virtual_key_id = $1`

// keyLockSQL serializes the limit check of one key: two holds checked at once would both
// see the same spend. The lock ends with the transaction.
const keyLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended('credits:key:' || $1, 0))`

// keySpendSQL is what a key has spent in its current window (UTC calendar day, ISO week
// or month; all time without a reset), plus what its open holds may still spend.
const keySpendSQL = `
SELECT coalesce((
         SELECT -sum(amount) FROM credit_ledger_entry
          WHERE virtual_key_id = $1 AND kind = 'usage'
            AND created_at >= CASE $2::text
                  WHEN 'daily' THEN date_trunc('day', now() AT TIME ZONE 'UTC')
                  WHEN 'weekly' THEN date_trunc('week', now() AT TIME ZONE 'UTC')
                  WHEN 'monthly' THEN date_trunc('month', now() AT TIME ZONE 'UTC')
                  ELSE '-infinity'::timestamp
                END), 0)
     + coalesce((
         SELECT sum(amount) FROM credit_reservation
          WHERE virtual_key_id = $1 AND settled_at IS NULL), 0)`

// Reserve holds amount (micro-SAR) against the organization's balance and, when virtualKeyID
// names a key with a spending limit, against that limit too. amount must be positive.
// fx is the rate the amount was converted at; the hold keeps it, so its charge is converted
// at the same rate and reports can show it.
func (s *Store) Reserve(ctx context.Context, organizationID, virtualKeyID string, amount int64, fx fxRate, ttl time.Duration) (string, ReserveOutcome, error) {
	if amount <= 0 {
		return "", InsufficientCredits, fmt.Errorf("credits: reservation amount must be positive, got %d", amount)
	}
	if fx.officialE6 <= 0 || fx.effectiveE6 <= 0 {
		return "", InsufficientCredits, fmt.Errorf("credits: reservation rate must be positive, got %+v", fx)
	}
	id := uuid.NewString()
	if virtualKeyID == "" {
		return s.hold(ctx, s.pool, id, organizationID, "", amount, fx, ttl)
	}

	var limit *int64
	var reset *string
	err := s.pool.QueryRow(ctx, keyLimitSQL, virtualKeyID).Scan(&limit, &reset)
	if err != nil && err != pgx.ErrNoRows {
		return "", InsufficientCredits, fmt.Errorf("credits: read key limit: %w", err)
	}
	if limit == nil {
		// no limit: the common case pays nothing more than the balance check
		return s.hold(ctx, s.pool, id, organizationID, virtualKeyID, amount, fx, ttl)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", InsufficientCredits, fmt.Errorf("credits: reserve: %w", err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // a committed transaction ignores it
	if _, err := tx.Exec(ctx, keyLockSQL, virtualKeyID); err != nil {
		return "", InsufficientCredits, fmt.Errorf("credits: lock key: %w", err)
	}
	// read after the lock, so holds committed by the previous holder are counted
	var spent int64
	if err := tx.QueryRow(ctx, keySpendSQL, virtualKeyID, reset).Scan(&spent); err != nil {
		return "", InsufficientCredits, fmt.Errorf("credits: key spend: %w", err)
	}
	if spent+amount > *limit {
		return "", KeyLimitReached, nil
	}
	got, outcome, err := s.hold(ctx, tx, id, organizationID, virtualKeyID, amount, fx, ttl)
	if err != nil || outcome != Reserved {
		return got, outcome, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", InsufficientCredits, fmt.Errorf("credits: reserve: %w", err)
	}
	return got, Reserved, nil
}

// querier is a pool or a transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) hold(ctx context.Context, q querier, id, organizationID, virtualKeyID string, amount int64, fx fxRate, ttl time.Duration) (string, ReserveOutcome, error) {
	var got string
	err := q.QueryRow(ctx, reserveSQL, id, organizationID, amount, ttl.Seconds(), virtualKeyID,
		fx.officialE6, fx.spreadBps, fx.effectiveE6).Scan(&got)
	if err == pgx.ErrNoRows {
		return "", InsufficientCredits, nil
	}
	if err != nil {
		return "", InsufficientCredits, fmt.Errorf("credits: reserve: %w", err)
	}
	return got, Reserved, nil
}

// Settlement is the outcome of one attempt.
type Settlement struct {
	// ReservationID is the hold taken for this attempt.
	ReservationID  string
	OrganizationID string
	// Cost is what the attempt is charged, in micro-SAR. Zero releases the hold and writes no entry.
	Cost int64
	// CostMicroUSD is the provider's cost before markup, in micro-USD, stored on the entry.
	CostMicroUSD int64
	// FX is the rate Cost was converted at: the hold's.
	FX fxRate
	// IdempotencyKey identifies the attempt. Settling the same key again changes nothing.
	IdempotencyKey string
	// Reference is the request id, for support and reconciliation.
	Reference string
	// Metadata is stored with the entry: the exact provider cost, markup, model, provider.
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
  INSERT INTO credit_ledger_entry (id, organization_id, amount, kind, idempotency_key, reference, metadata, virtual_key_id,
                                   cost_micro_usd, usd_to_sar_rate_e6, fx_spread_bps, effective_usd_to_sar_rate_e6)
  SELECT $3, $2, -$4::bigint, 'usage', $5, $6, $7::jsonb,
         (SELECT virtual_key_id FROM credit_reservation WHERE id = $1), $8, $9, $10, $11
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
	if st.Cost > 0 && (st.CostMicroUSD < 0 || st.FX.officialE6 <= 0 || st.FX.effectiveE6 <= 0) {
		return SettleResult{}, fmt.Errorf("credits: a charge needs its USD cost and rate, got %d micro-USD at %+v", st.CostMicroUSD, st.FX)
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
		st.CostMicroUSD, st.FX.officialE6, st.FX.spreadBps, st.FX.effectiveE6,
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

// Extend gives an open hold a new lifetime from now. A hold already settled or reaped
// stays as it is; ok says whether one was extended.
func (s *Store) Extend(ctx context.Context, reservationID string, ttl time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE credit_reservation SET expires_at = now() + make_interval(secs => $2) WHERE id = $1 AND settled_at IS NULL`,
		reservationID, ttl.Seconds())
	if err != nil {
		return false, fmt.Errorf("credits: extend: %w", err)
	}
	return tag.RowsAffected() == 1, nil
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

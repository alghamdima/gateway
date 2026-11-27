package credits

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testDB is a throwaway database with the control plane's real migrations applied.
// admin is the owning role, for arranging and inspecting; ledger is what the plugin
// gets in production: every connection is switched to the gateway_ledger role.
type testDB struct {
	admin  *pgxpool.Pool
	ledger *pgxpool.Pool
}

// migrationsDir finds packages/db/migrations by walking up from the test's directory.
func migrationsDir() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		candidate := filepath.Join(dir, "packages", "db", "migrations")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// newTestDB needs CREDITS_TEST_DATABASE_URL (a role that may create databases) and the
// monorepo's migrations. Without either the test is skipped: these tests are about what
// Postgres itself enforces, and a fake would prove nothing.
func newTestDB(t *testing.T) *testDB {
	t.Helper()
	adminURL := os.Getenv("CREDITS_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("CREDITS_TEST_DATABASE_URL is not set")
	}
	migrations, ok := migrationsDir()
	if !ok {
		t.Skip("packages/db/migrations not found above the test directory")
	}
	ctx := context.Background()

	name := "credits_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	root, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to CREDITS_TEST_DATABASE_URL: %v", err)
	}
	if _, err := root.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("create database: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	cfg.MaxConns = 4
	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}

	files, err := filepath.Glob(filepath.Join(migrations, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations in %s: %v", migrations, err)
	}
	sort.Strings(files)
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range strings.Split(string(raw), "--> statement-breakpoint") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := admin.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", filepath.Base(file), err)
			}
		}
	}

	ledgerCfg := cfg.Copy()
	ledgerCfg.MaxConns = 20
	ledgerCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE gateway_ledger")
		return err
	}
	ledger, err := pgxpool.NewWithConfig(ctx, ledgerCfg)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		ledger.Close()
		admin.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := root.Exec(ctx, `DROP DATABASE "`+name+`" WITH (FORCE)`); err != nil {
			t.Logf("drop database %s: %v", name, err)
		}
		root.Close(ctx)
	})
	return &testDB{admin: admin, ledger: ledger}
}

// organization creates an organization with a credit account holding balance micro-SAR.
func (db *testDB) organization(t *testing.T, balance int64) string {
	t.Helper()
	ctx := context.Background()
	id := "org_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if _, err := db.admin.Exec(ctx, `INSERT INTO organization (id, name, slug, created_at) VALUES ($1, $1, $1, now())`, id); err != nil {
		t.Fatal(err)
	}
	// its gateway customer has the same id here; organizations_test.go covers different ids
	if _, err := db.admin.Exec(ctx, `INSERT INTO gateway_customer (organization_id, customer_id) VALUES ($1, $1)`, id); err != nil {
		t.Fatal(err)
	}
	if balance == 0 {
		return id
	}
	if _, err := db.admin.Exec(ctx, `INSERT INTO credit_ledger_entry (id, organization_id, amount, kind, idempotency_key) VALUES ($1, $2, $3, 'top_up', $1)`, "seed_"+id, id, balance); err != nil {
		t.Fatal(err)
	}
	if _, err := db.admin.Exec(ctx, `INSERT INTO credit_account (organization_id, balance) VALUES ($1, $2)`, id, balance); err != nil {
		t.Fatal(err)
	}
	return id
}

// setPricing sets the default markup and replaces the per-provider markups, as staff do
// in the admin console.
func (db *testDB) setPricing(t *testing.T, defaultBps int64, byProvider map[string]int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.admin.Exec(ctx, `UPDATE pricing_settings SET default_markup_bps = $1 WHERE id = 1`, defaultBps); err != nil {
		t.Fatal(err)
	}
	if _, err := db.admin.Exec(ctx, `DELETE FROM provider_markup`); err != nil {
		t.Fatal(err)
	}
	for provider, bps := range byProvider {
		if _, err := db.admin.Exec(ctx, `INSERT INTO provider_markup (provider, markup_bps) VALUES ($1, $2)`, provider, bps); err != nil {
			t.Fatal(err)
		}
	}
}

// setRate sets the USD to SAR rate x 1,000,000, as staff do in the admin console.
func (db *testDB) setRate(t *testing.T, rateE6 int64) {
	t.Helper()
	if _, err := db.admin.Exec(context.Background(), `UPDATE pricing_settings SET usd_to_sar_rate_e6 = $1 WHERE id = 1`, rateE6); err != nil {
		t.Fatal(err)
	}
}

// setSpread sets the bank's FX spread in basis points, as staff do in the admin console.
func (db *testDB) setSpread(t *testing.T, spreadBps int64) {
	t.Helper()
	if _, err := db.admin.Exec(context.Background(), `UPDATE pricing_settings SET fx_spread_bps = $1 WHERE id = 1`, spreadBps); err != nil {
		t.Fatal(err)
	}
}

type accountState struct {
	Balance, Held, LedgerSum, OpenReservations, UsageEntries int64
}

// state reads the cached account next to the two sources of truth it must agree with.
func (db *testDB) state(t *testing.T, organizationID string) accountState {
	t.Helper()
	var s accountState
	err := db.admin.QueryRow(context.Background(), `
		SELECT coalesce((SELECT balance FROM credit_account WHERE organization_id = $1), 0),
		       coalesce((SELECT held FROM credit_account WHERE organization_id = $1), 0),
		       coalesce((SELECT sum(amount) FROM credit_ledger_entry WHERE organization_id = $1), 0),
		       coalesce((SELECT sum(amount) FROM credit_reservation WHERE organization_id = $1 AND settled_at IS NULL), 0),
		       (SELECT count(*) FROM credit_ledger_entry WHERE organization_id = $1 AND kind = 'usage')`,
		organizationID).Scan(&s.Balance, &s.Held, &s.LedgerSum, &s.OpenReservations, &s.UsageEntries)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// consistent fails the test unless the cache equals the ledger and the open reservations.
func (db *testDB) consistent(t *testing.T, organizationID string) accountState {
	t.Helper()
	s := db.state(t, organizationID)
	if s.Balance != s.LedgerSum {
		t.Errorf("balance %d differs from the sum of ledger entries %d", s.Balance, s.LedgerSum)
	}
	if s.Held != s.OpenReservations {
		t.Errorf("held %d differs from the sum of open reservations %d", s.Held, s.OpenReservations)
	}
	return s
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(fmt.Errorf("unexpected error: %w", err))
		}
		return v
	}
}

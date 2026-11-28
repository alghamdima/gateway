package credits

import (
	"context"
	"errors"
	"testing"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

func TestPreLLMHook_ACustomerWithNoOrganizationIsRefused(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd, unmapped: map[string]bool{"cust_orphan": true}}
	plugin := testPlugin(ledger, testPricer())
	_, sc, _ := plugin.PreLLMHook(testContext("cust_orphan"), chatRequest(schemas.ChatCompletionRequest, nil))
	if status, errorType := refusalOf(t, sc); status != 403 || errorType != ErrorTypeNoOrganization {
		t.Fatalf("got %d %s", status, errorType)
	}
	if ledger.reserveCall.Load() != 0 {
		t.Fatal("a hold was taken for a customer that belongs to no organization")
	}
}

func TestPreLLMHook_ALookupFailureRefusesWith503(t *testing.T) {
	ledger := &fakeLedger{balance: 10 * usd, lookupErr: errors.New("connection refused")}
	plugin := testPlugin(ledger, testPricer())
	_, sc, _ := plugin.PreLLMHook(testContext(testOrg), chatRequest(schemas.ChatCompletionRequest, nil))
	if status, errorType := refusalOf(t, sc); status != 503 || errorType != ErrorTypeLedgerUnavailable {
		t.Fatalf("got %d %s", status, errorType)
	}
}

func TestOrganizationFor_CachesHitsOnlyAndAMissIsAskedAgain(t *testing.T) {
	ledger := &fakeLedger{unmapped: map[string]bool{"cust_new": true}}
	plugin := testPlugin(ledger, testPricer())
	ctx := context.Background()

	for range 3 {
		if org, ok, err := plugin.organizationFor(ctx, "cust_known"); err != nil || !ok || org != "cust_known" {
			t.Fatalf("known customer: %q %v %v", org, ok, err)
		}
	}
	if ledger.lookups != 1 {
		t.Fatalf("a known customer was looked up %d times, want 1", ledger.lookups)
	}

	if _, ok, _ := plugin.organizationFor(ctx, "cust_new"); ok {
		t.Fatal("an unmapped customer resolved")
	}
	ledger.mu.Lock()
	delete(ledger.unmapped, "cust_new") // the organization is created and mapped
	ledger.mu.Unlock()
	if org, ok, _ := plugin.organizationFor(ctx, "cust_new"); !ok || org != "cust_new" {
		t.Fatal("a customer mapped after a miss was not found: misses must not be cached")
	}
}

// Against the real schema, as the gateway role: the customer id the gateway generated is not
// the organization id, and the organization is the one charged.
func TestHooks_ChargeTheOrganizationBehindTheGatewayCustomer(t *testing.T) {
	db := newTestDB(t)
	store := NewStore(db.ledger)
	org := db.organization(t, usd)
	if _, err := db.admin.Exec(context.Background(),
		`UPDATE gateway_customer SET customer_id = '5b91db81-1730-436b-9a18-e1929af07faf' WHERE organization_id = $1`, org); err != nil {
		t.Fatal(err)
	}
	plugin := atParityRate(newPlugin(store, testPricer(), bifrost.NewNoOpLogger(), &Config{}))

	if _, _, err := store.OrganizationFor(context.Background(), org); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.OrganizationFor(context.Background(), org); ok {
		t.Fatal("the organization id resolved as a customer id")
	}

	ctx := testContext("5b91db81-1730-436b-9a18-e1929af07faf")
	if _, sc, _ := plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil)); sc != nil {
		t.Fatalf("refused: %+v", sc.Error)
	}
	plugin.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)
	if s := db.consistent(t, org); s.Balance != usd-2000 || s.UsageEntries != 1 || s.Held != 0 {
		t.Fatalf("organization state: %+v", s)
	}

	_, sc, _ := plugin.PreLLMHook(testContext("customer-nobody-knows"), chatRequest(schemas.ChatCompletionRequest, nil))
	if status, errorType := refusalOf(t, sc); status != 403 || errorType != ErrorTypeNoOrganization {
		t.Fatalf("unknown customer: %d %s", status, errorType)
	}
}

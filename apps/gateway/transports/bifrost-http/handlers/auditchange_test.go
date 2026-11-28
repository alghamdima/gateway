package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// auditChanges returns the audit changes a handler recorded on ctx.
func auditChanges(t *testing.T, ctx *fasthttp.RequestCtx) []configstore.AuditChange {
	t.Helper()
	changes, _ := ctx.UserValue(configstore.AuditChangesUserValueKey).([]configstore.AuditChange)
	return changes
}

// requireSingleAuditChange asserts exactly one change with the given event name and returns it.
func requireSingleAuditChange(t *testing.T, ctx *fasthttp.RequestCtx, eventName string) configstore.AuditChange {
	t.Helper()
	changes := auditChanges(t, ctx)
	if len(changes) != 1 {
		t.Fatalf("expected 1 audit change, got %d: %+v", len(changes), changes)
	}
	if changes[0].EventName != eventName {
		t.Fatalf("event name got %q, want %q", changes[0].EventName, eventName)
	}
	return changes[0]
}

// requireNoSecret fails when any snapshot of change contains secret.
func requireNoSecret(t *testing.T, change configstore.AuditChange, secret string) {
	t.Helper()
	if secret == "" {
		t.Fatal("secret under test must not be empty")
	}
	if strings.Contains(string(change.Before), secret) {
		t.Fatalf("before snapshot leaks the secret: %s", change.Before)
	}
	if strings.Contains(string(change.After), secret) {
		t.Fatalf("after snapshot leaks the secret: %s", change.After)
	}
}

// auditTestGovernanceManager is budgetOverrideTestGovernanceManager with a no-op RemoveVirtualKey.
type auditTestGovernanceManager struct {
	*budgetOverrideTestGovernanceManager
}

// RemoveVirtualKey does nothing; the tests only read the database.
func (auditTestGovernanceManager) RemoveVirtualKey(context.Context, string) error {
	return nil
}

// newAuditVKHandler builds a governance handler on a fresh SQLite store with one customer.
func newAuditVKHandler(t *testing.T) (*GovernanceHandler, configstore.ConfigStore) {
	t.Helper()
	SetLogger(&mockLogger{})
	store := setupPricingOverrideHandlerStore(t)
	if err := store.CreateCustomer(context.Background(), &configstoreTables.TableCustomer{ID: "cust-audit", Name: "Audit Customer"}); err != nil {
		t.Fatalf("create customer: %v", err)
	}
	return &GovernanceHandler{configStore: store, governanceManager: auditTestGovernanceManager{&budgetOverrideTestGovernanceManager{store: store}}}, store
}

// createAuditVK creates a customer-owned VK through the handler and returns its id and plaintext value.
func createAuditVK(t *testing.T, h *GovernanceHandler, store configstore.ConfigStore) (*fasthttp.RequestCtx, string, string) {
	t.Helper()
	ctx := newTestRequestCtx(`{"name":"audit-vk","description":"first","customer_id":"cust-audit"}`)
	h.createVirtualKey(ctx)
	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("create status=%d body=%s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
	var resp struct {
		VirtualKey struct {
			ID string `json:"id"`
		} `json:"virtual_key"`
	}
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	vk, err := store.GetVirtualKey(context.Background(), resp.VirtualKey.ID)
	if err != nil {
		t.Fatalf("load created VK: %v", err)
	}
	return ctx, vk.ID, vk.Value.GetValue()
}

func TestAuditChange_VirtualKeyCreate(t *testing.T) {
	h, store := newAuditVKHandler(t)
	ctx, vkID, value := createAuditVK(t, h, store)

	change := requireSingleAuditChange(t, ctx, "virtual_key.created")
	if change.TargetType != "virtual_key" || change.TargetID != vkID || change.TargetName != "audit-vk" {
		t.Fatalf("unexpected target: %+v", change)
	}
	if change.Before != nil {
		t.Fatalf("create must have no before snapshot, got %s", change.Before)
	}
	if len(change.After) == 0 {
		t.Fatal("create must have an after snapshot")
	}
	requireNoSecret(t, change, value)
}

func TestAuditChange_VirtualKeyUpdate(t *testing.T) {
	h, store := newAuditVKHandler(t)
	_, vkID, value := createAuditVK(t, h, store)

	ctx := newTestRequestCtx(`{"description":"second"}`)
	ctx.SetUserValue("vk_id", vkID)
	h.updateVirtualKey(ctx)
	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("update status=%d body=%s", ctx.Response.StatusCode(), ctx.Response.Body())
	}

	change := requireSingleAuditChange(t, ctx, "virtual_key.updated")
	if change.TargetID != vkID || change.TargetName != "audit-vk" {
		t.Fatalf("unexpected target: %+v", change)
	}
	if len(change.Before) == 0 || len(change.After) == 0 {
		t.Fatalf("update must carry both snapshots: before=%s after=%s", change.Before, change.After)
	}
	var before, after struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(change.Before, &before); err != nil {
		t.Fatalf("decode before: %v", err)
	}
	if err := json.Unmarshal(change.After, &after); err != nil {
		t.Fatalf("decode after: %v", err)
	}
	if before.Description != "first" || after.Description != "second" {
		t.Fatalf("before/after descriptions got %q/%q, want first/second", before.Description, after.Description)
	}
	requireNoSecret(t, change, value)
}

func TestAuditChange_VirtualKeyUpdateRejectedRecordsNothing(t *testing.T) {
	h, store := newAuditVKHandler(t)
	_, vkID, _ := createAuditVK(t, h, store)

	ctx := newTestRequestCtx(`{"expires_at":"not-a-time"}`)
	ctx.SetUserValue("vk_id", vkID)
	h.updateVirtualKey(ctx)
	if ctx.Response.StatusCode() != fasthttp.StatusBadRequest {
		t.Fatalf("update status=%d body=%s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
	if changes := auditChanges(t, ctx); len(changes) != 0 {
		t.Fatalf("a rejected update must record nothing, got %+v", changes)
	}
}

func TestAuditChange_VirtualKeyDelete(t *testing.T) {
	h, store := newAuditVKHandler(t)
	_, vkID, value := createAuditVK(t, h, store)

	ctx := newTestRequestCtx("")
	ctx.SetUserValue("vk_id", vkID)
	h.deleteVirtualKey(ctx)
	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("delete status=%d body=%s", ctx.Response.StatusCode(), ctx.Response.Body())
	}

	change := requireSingleAuditChange(t, ctx, "virtual_key.deleted")
	if change.TargetID != vkID || change.TargetName != "audit-vk" {
		t.Fatalf("unexpected target: %+v", change)
	}
	if len(change.Before) == 0 || change.After != nil {
		t.Fatalf("delete must carry only a before snapshot: before=%s after=%s", change.Before, change.After)
	}
	requireNoSecret(t, change, value)
}

func TestAuditChange_ProviderKeyUpdate(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})
	const storedValue = "sk-stored-audit-raw-0123456789abcdef"
	const newValue = "sk-updated-audit-raw-fedcba9876543210"

	h := &ProviderHandler{
		inMemoryStore: &lib.Config{
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				schemas.OpenAI: {Keys: []schemas.Key{{
					ID:     "key-1",
					Name:   "openai-key",
					Value:  *schemas.NewSecretVar(storedValue),
					Weight: 1,
				}}},
			},
		},
		modelsManager: &mockModelsManager{},
	}
	attachBifrostClient(t, h.inMemoryStore)

	ctx := newTestRequestCtx(`{"name":"openai-key","value":"` + newValue + `","weight":2}`)
	ctx.SetUserValue("provider", string(schemas.OpenAI))
	ctx.SetUserValue("key_id", "key-1")
	h.updateProviderKey(ctx)
	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("update status=%d body=%s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
	if got := h.inMemoryStore.Providers[schemas.OpenAI].Keys[0].Value.GetValue(); got != newValue {
		t.Fatalf("stored value not updated, got %q", got)
	}

	change := requireSingleAuditChange(t, ctx, "provider_key.updated")
	if change.TargetType != "provider_key" || change.TargetID != "key-1" || change.TargetName != "openai-key" {
		t.Fatalf("unexpected target: %+v", change)
	}
	if len(change.Before) == 0 || len(change.After) == 0 {
		t.Fatalf("update must carry both snapshots: before=%s after=%s", change.Before, change.After)
	}
	requireNoSecret(t, change, storedValue)
	requireNoSecret(t, change, newValue)
}

func TestAuditChange_VirtualKeyDeleteSnapshotIncludesBudgets(t *testing.T) {
	h, _ := newAuditVKHandler(t)
	createCtx := newTestRequestCtx(`{"name":"audit-vk","customer_id":"cust-audit","budgets":[{"max_limit":123.5,"reset_duration":"1d"}]}`)
	h.createVirtualKey(createCtx)
	if createCtx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("create status=%d body=%s", createCtx.Response.StatusCode(), createCtx.Response.Body())
	}
	var resp struct {
		VirtualKey struct {
			ID string `json:"id"`
		} `json:"virtual_key"`
	}
	if err := json.Unmarshal(createCtx.Response.Body(), &resp); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	ctx := newTestRequestCtx("")
	ctx.SetUserValue("vk_id", resp.VirtualKey.ID)
	h.deleteVirtualKey(ctx)
	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("delete status=%d body=%s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
	change := requireSingleAuditChange(t, ctx, "virtual_key.deleted")
	if !strings.Contains(string(change.Before), "123.5") {
		t.Fatalf("deletion snapshot lost the key's budget: %s", change.Before)
	}
}

// failingProxyReloadManager fails the proxy reload after the config is saved.
type failingProxyReloadManager struct {
	stubConfigManager
}

func (failingProxyReloadManager) ReloadProxyConfig(context.Context, *configstoreTables.GlobalProxyConfig) error {
	return errors.New("reload failed")
}

// TestAuditChange_ProxyConfigRecordedWhenReloadFails pins that a saved proxy
// config is audited even when the live reload after it fails.
func TestAuditChange_ProxyConfigRecordedWhenReloadFails(t *testing.T) {
	SetLogger(&mockLogger{})
	h := &ConfigHandler{store: &lib.Config{ConfigStore: setupPricingOverrideHandlerStore(t)}, configManager: failingProxyReloadManager{}}

	ctx := newTestRequestCtx(`{"enabled":false,"type":"http","url":"http://proxy.example:8080"}`)
	h.updateProxyConfig(ctx)

	if code := ctx.Response.StatusCode(); code != fasthttp.StatusInternalServerError {
		t.Fatalf("expected 500 from the reload, got %d: %s", code, ctx.Response.Body())
	}
	change := requireSingleAuditChange(t, ctx, "proxy_config.updated")
	if !strings.Contains(string(change.After), "proxy.example") {
		t.Fatalf("after snapshot missing the saved URL: %s", change.After)
	}
}

// snapshotReadFailingStore fails the reads the proxy and metadata handlers
// take their before snapshots from, and counts the writes that follow.
type snapshotReadFailingStore struct {
	configstore.ConfigStore
	writes int
}

func (s *snapshotReadFailingStore) GetProxyConfig(context.Context) (*configstoreTables.GlobalProxyConfig, error) {
	return nil, errors.New("read failed")
}

func (s *snapshotReadFailingStore) UpdateProxyConfig(context.Context, *configstoreTables.GlobalProxyConfig) error {
	s.writes++
	return nil
}

func (s *snapshotReadFailingStore) GetClientMetadata(context.Context) (map[string]any, error) {
	return nil, errors.New("read failed")
}

func (s *snapshotReadFailingStore) UpdateClientMetadata(context.Context, map[string]any) error {
	s.writes++
	return nil
}

// TestAuditChange_SnapshotReadFailureBlocksWrite pins that a failed before
// read fails the request instead of writing and auditing an empty before.
func TestAuditChange_SnapshotReadFailureBlocksWrite(t *testing.T) {
	SetLogger(&mockLogger{})
	for name, run := range map[string]func(*ConfigHandler, *fasthttp.RequestCtx){
		"proxy":    (*ConfigHandler).updateProxyConfig,
		"metadata": (*ConfigHandler).updateMetadata,
	} {
		t.Run(name, func(t *testing.T) {
			cfgStore := &snapshotReadFailingStore{}
			h := &ConfigHandler{store: &lib.Config{ConfigStore: cfgStore}, configManager: stubConfigManager{}}
			body := `{"theme":"dark"}`
			if name == "proxy" {
				body = `{"enabled":false,"type":"http","url":""}`
			}
			ctx := newTestRequestCtx(body)
			run(h, ctx)

			if code := ctx.Response.StatusCode(); code != fasthttp.StatusInternalServerError {
				t.Fatalf("expected 500, got %d: %s", code, ctx.Response.Body())
			}
			if cfgStore.writes != 0 {
				t.Fatalf("expected no write after a failed snapshot read, got %d", cfgStore.writes)
			}
			if changes := auditChanges(t, ctx); len(changes) != 0 {
				t.Fatalf("expected no audit change, got %+v", changes)
			}
		})
	}
}

func TestAuditDeletedIDsCapsTheList(t *testing.T) {
	ids := make([]string, auditDeletedIDsLimit+5)
	for i := range ids {
		ids[i] = "log-id"
	}
	snap := auditDeletedIDs(ids)
	if snap["count"] != len(ids) || snap["ids_truncated"] != true {
		t.Fatalf("expected full count and truncation flag, got %+v", snap)
	}
	if got := len(snap["ids"].([]string)); got != auditDeletedIDsLimit {
		t.Fatalf("expected %d ids, got %d", auditDeletedIDsLimit, got)
	}
	if small := auditDeletedIDs([]string{"a"}); small["ids_truncated"] != nil {
		t.Fatalf("a small delete must not be marked truncated: %+v", small)
	}
}

// TestAuditChange_ProxyURLPasswordMaskedWhenURLDoesNotParse pins that the audit snapshot
// masks a proxy URL's password even when the URL does not parse. A disabled update is not
// validated, so the restored password can be saved inside a malformed URL.
func TestAuditChange_ProxyURLPasswordMaskedWhenURLDoesNotParse(t *testing.T) {
	SetLogger(&mockLogger{})
	store := setupPricingOverrideHandlerStore(t)
	if err := store.UpdateProxyConfig(context.Background(), &configstoreTables.GlobalProxyConfig{
		Enabled: true, Type: "http", URL: "http://alice:secret@proxy.example:8080",
	}); err != nil {
		t.Fatalf("seed proxy config: %v", err)
	}
	h := &ConfigHandler{store: &lib.Config{ConfigStore: store}, configManager: &proxyReloadRecorder{}}

	ctx := newTestRequestCtx(`{"enabled":false,"type":"http","url":"http://alice:<redacted>@proxy.example:bad"}`)
	h.updateProxyConfig(ctx)

	if code := ctx.Response.StatusCode(); code != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, ctx.Response.Body())
	}
	change := requireSingleAuditChange(t, ctx, "proxy_config.updated")
	if strings.Contains(string(change.Before), "secret") || strings.Contains(string(change.After), "secret") {
		t.Fatalf("audit snapshot carries the proxy password: before %s, after %s", change.Before, change.After)
	}
	if !strings.Contains(string(change.After), "proxy.example:bad") {
		t.Fatalf("after snapshot missing the saved URL: %s", change.After)
	}
}

package credits

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The end-to-end test runs the real gateway binary with the plugin loaded from its config,
// a mock provider behind it, and the real ledger in Postgres. It needs CREDITS_E2E_BINARY
// (the built gateway) on top of what newTestDB needs, and is skipped without it.

const (
	e2ePromptTokens     = 1000
	e2eCompletionTokens = 500
)

// mockProvider answers like the OpenAI API and counts the calls that would cost money.
type mockProvider struct {
	server *httptest.Server
	chats  atomic.Int64
	others atomic.Int64
}

func newMockProvider(t *testing.T) *mockProvider {
	m := &mockProvider{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			if r.Method != http.MethodGet {
				m.others.Add(1)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-4o-mini","object":"model","owned_by":"openai"}]}`)
			return
		}
		m.chats.Add(1)
		var body struct {
			Stream bool `json:"stream"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		usage := fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}`, e2ePromptTokens, e2eCompletionTokens, e2ePromptTokens+e2eCompletionTokens)
		if !body.Stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":%s}`, usage)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		send := func(payload string) {
			fmt.Fprintf(w, "data: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
		}
		for _, word := range []string{"hel", "lo"} {
			send(fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, word))
		}
		send(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		send(fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o-mini","choices":[],"usage":%s}`, usage))
		send("[DONE]")
	}))
	t.Cleanup(m.server.Close)
	return m
}

type e2eGateway struct {
	url string
}

// startGateway boots the binary with the given credits plugin config and waits for /health.
func startGateway(t *testing.T, binary string, provider *mockProvider, creditsConfig map[string]any, organizations []string) *e2eGateway {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	migrations, _ := migrationsDir()
	pricing := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(migrations))), "apps", "gateway", "examples", "configs", "withlocalpricingfiles")

	// allowed_models and key_ids both default to deny-all: a key that lists neither can call nothing
	providerConfigs := []map[string]any{{"provider": "openai", "allowed_models": []string{"*"}, "key_ids": []string{"*"}}}
	customers := []map[string]any{}
	keys := []map[string]any{{
		"id": "vk-no-org", "name": "no organization", "value": "sk-bf-e2e-no-organization", "is_active": true,
		"provider_configs": providerConfigs,
	}}
	for _, org := range organizations {
		customers = append(customers, map[string]any{"id": org, "name": org})
		keys = append(keys, map[string]any{
			"id": "vk-" + org, "name": org, "value": "sk-bf-e2e-" + org, "is_active": true, "customer_id": org,
			"provider_configs": providerConfigs,
		})
	}
	config := map[string]any{
		"client": map[string]any{"enforce_auth_on_inference": true},
		"providers": map[string]any{"openai": map[string]any{
			"keys":           []map[string]any{{"name": "mock", "value": "sk-mock-upstream", "models": []string{"*"}, "weight": 1}},
			"network_config": map[string]any{"base_url": provider.server.URL, "allow_private_network": true, "max_retries": 0},
		}},
		"governance": map[string]any{"customers": customers, "virtual_keys": keys},
		"plugins":    []map[string]any{{"name": PluginName, "enabled": true, "config": creditsConfig}},
		"framework": map[string]any{"pricing": map[string]any{
			"pricing_url":          "file://" + filepath.Join(pricing, "pricing.json"),
			"model_parameters_url": "file://" + filepath.Join(pricing, "model-parameters.json"),
		}},
	}
	appDir := t.TempDir()
	raw, _ := json.MarshalIndent(config, "", "  ")
	if err := os.WriteFile(filepath.Join(appDir, "config.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(appDir, "gateway.log")
	logFile, _ := os.Create(logPath)
	cmd := exec.Command(binary, "-app-dir", appDir, "-port", fmt.Sprint(port), "-host", "127.0.0.1")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", binary, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		logFile.Close()
		if t.Failed() {
			if out, err := os.ReadFile(logPath); err == nil {
				lines := strings.Split(string(out), "\n")
				t.Logf("gateway log (last lines):\n%s", strings.Join(lines[max(0, len(lines)-40):], "\n"))
			}
		}
	})

	gw := &e2eGateway{url: fmt.Sprintf("http://127.0.0.1:%d", port)}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(gw.url + "/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return gw
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("the gateway did not become healthy")
	return nil
}

func (g *e2eGateway) post(t *testing.T, path, key string, body map[string]any) (int, string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, g.url+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func chatBody(stream bool) map[string]any {
	return map[string]any{
		"model": "openai/gpt-4o-mini", "max_tokens": 600, "stream": stream,
		"messages": []map[string]any{{"role": "user", "content": "say hello"}},
	}
}

// ledgerURL is the test database reached as the gateway_ledger role, the way production is.
func ledgerURL(t *testing.T, db *testDB) string {
	t.Helper()
	u, err := url.Parse(os.Getenv("CREDITS_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + db.admin.Config().ConnConfig.Database
	q := u.Query()
	q.Set("options", "-c role=gateway_ledger")
	u.RawQuery = q.Encode()
	return u.String()
}

// settled waits until the organization has no open hold: settlement happens after the response.
func settled(t *testing.T, db *testDB, org string) accountState {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		s := db.state(t, org)
		if s.Held == 0 || time.Now().After(deadline) {
			return db.consistent(t, org)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestEndToEnd_TheGatewayChargesTheLedger(t *testing.T) {
	binary := os.Getenv("CREDITS_E2E_BINARY")
	if binary == "" {
		t.Skip("CREDITS_E2E_BINARY is not set")
	}
	db := newTestDB(t)
	provider := newMockProvider(t)
	funded := db.organization(t, usd)
	broke := db.organization(t, 10)
	// as staff would set them: 1 percent by default, 5 percent for the provider under test
	db.setPricing(t, 100, map[string]int64{"openai": 500})
	gw := startGateway(t, binary, provider, map[string]any{"database_url": ledgerURL(t, db)}, []string{funded, broke})

	// gpt-4o-mini in the local pricing file: 0.15 USD per million input tokens, 0.60 per million output
	providerCost := e2ePromptTokens*0.15e-6 + e2eCompletionTokens*0.60e-6
	charge, err := toMicroUSD(providerCost, 500)
	if err != nil || charge < 470 || charge > 475 {
		t.Fatalf("expected charge: %d, %v", charge, err)
	}

	t.Run("a chat completion is charged once at cost plus the provider's markup", func(t *testing.T) {
		status, body := gw.post(t, "/v1/chat/completions", "sk-bf-e2e-"+funded, chatBody(false))
		if status != 200 || !strings.Contains(body, "hello") {
			t.Fatalf("status %d: %s", status, body)
		}
		s := settled(t, db, funded)
		if s.UsageEntries != 1 || s.Balance != usd-charge || s.Held != 0 {
			t.Fatalf("state %+v, want one usage entry of %d", s, charge)
		}
		var cost float64
		var model, reference string
		var markup int64
		err := db.admin.QueryRow(context.Background(), `SELECT (metadata->>'provider_cost_usd')::float8, metadata->>'model', reference, (metadata->>'markup_bps')::bigint FROM credit_ledger_entry WHERE organization_id = $1 AND kind = 'usage'`, funded).Scan(&cost, &model, &reference, &markup)
		if err != nil || cost < providerCost-1e-9 || cost > providerCost+1e-9 || !strings.Contains(model, "gpt-4o-mini") || reference == "" || markup != 500 {
			t.Fatalf("entry: cost=%v model=%q reference=%q markup=%d err=%v", cost, model, reference, markup, err)
		}
	})

	t.Run("a streamed completion is charged once, after its final chunk", func(t *testing.T) {
		status, body := gw.post(t, "/v1/chat/completions", "sk-bf-e2e-"+funded, chatBody(true))
		if status != 200 || !strings.Contains(body, "[DONE]") {
			t.Fatalf("status %d: %s", status, body)
		}
		s := settled(t, db, funded)
		if s.UsageEntries != 2 || s.Balance != usd-2*charge || s.Held != 0 {
			t.Fatalf("state %+v, want two usage entries of %d", s, charge)
		}
	})

	t.Run("a balance that cannot cover the request gets 402 and the provider is never called", func(t *testing.T) {
		before := provider.chats.Load()
		status, body := gw.post(t, "/v1/chat/completions", "sk-bf-e2e-"+broke, chatBody(false))
		if status != 402 || !strings.Contains(body, ErrorTypeInsufficientCredits) {
			t.Fatalf("status %d: %s", status, body)
		}
		if provider.chats.Load() != before {
			t.Fatal("the provider was called for a request that was refused")
		}
		if s := db.consistent(t, broke); s.Balance != 10 || s.Held != 0 || s.UsageEntries != 0 {
			t.Fatalf("state %+v", s)
		}
	})

	t.Run("a key with no organization gets 403 and the provider is never called", func(t *testing.T) {
		before := provider.chats.Load()
		status, body := gw.post(t, "/v1/chat/completions", "sk-bf-e2e-no-organization", chatBody(false))
		if status != 403 || !strings.Contains(body, ErrorTypeNoOrganization) {
			t.Fatalf("status %d: %s", status, body)
		}
		if provider.chats.Load() != before {
			t.Fatal("the provider was called for a request that was refused")
		}
	})

	t.Run("an image request is refused and the provider is never called", func(t *testing.T) {
		// governance may refuse it first (the model is not in the catalog) or the plugin does;
		// what matters is that it is refused, nothing is called and nothing is charged
		before := provider.others.Load()
		status, body := gw.post(t, "/v1/images/generations", "sk-bf-e2e-"+funded, map[string]any{"model": "openai/gpt-image-1", "prompt": "a cat"})
		if status != 403 {
			t.Fatalf("status %d: %s", status, body)
		}
		if provider.others.Load() != before {
			t.Fatal("the provider was called for a request that was refused")
		}
		if s := db.consistent(t, funded); s.UsageEntries != 2 || s.Held != 0 {
			t.Fatalf("state %+v", s)
		}
	})

	t.Run("a markup staff change applies without restarting the gateway", func(t *testing.T) {
		if testing.Short() {
			t.Skip("waits for the plugin's 30 second refresh")
		}
		db.setPricing(t, 100, map[string]int64{"openai": 2000})
		time.Sleep(markupRefreshInterval + 2*time.Second)
		before := db.consistent(t, funded)
		status, body := gw.post(t, "/v1/chat/completions", "sk-bf-e2e-"+funded, chatBody(false))
		if status != 200 {
			t.Fatalf("status %d: %s", status, body)
		}
		raised, _ := toMicroUSD(providerCost, 2000)
		if s := settled(t, db, funded); s.Balance != before.Balance-raised {
			t.Fatalf("charged %d, want %d at the new 20 percent markup", before.Balance-s.Balance, raised)
		}
	})

	t.Run("no key at all is refused by the gateway itself", func(t *testing.T) {
		status, _ := gw.post(t, "/v1/chat/completions", "", chatBody(false))
		if status != 401 {
			t.Fatalf("status %d, want 401", status)
		}
	})
}

func TestEndToEnd_AMisconfiguredPluginRefusesInsteadOfServingForFree(t *testing.T) {
	binary := os.Getenv("CREDITS_E2E_BINARY")
	if binary == "" {
		t.Skip("CREDITS_E2E_BINARY is not set")
	}
	db := newTestDB(t)
	provider := newMockProvider(t)
	org := db.organization(t, usd)

	for name, config := range map[string]map[string]any{
		"no database url":             {},
		"a config of the wrong shape": {"database_url": 5},
		"a database that is down":     {"database_url": "postgres://nobody:nothing@127.0.0.1:1/none"},
	} {
		t.Run(name, func(t *testing.T) {
			gw := startGateway(t, binary, provider, config, []string{org})
			before := provider.chats.Load()
			status, body := gw.post(t, "/v1/chat/completions", "sk-bf-e2e-"+org, chatBody(false))
			if status != 503 || !strings.Contains(body, ErrorTypeLedgerUnavailable) {
				t.Fatalf("status %d: %s", status, body)
			}
			if provider.chats.Load() != before {
				t.Fatal("the provider was called although the request could not be accounted for")
			}
		})
	}
	if s := db.consistent(t, org); s.Balance != usd || s.UsageEntries != 0 {
		t.Fatalf("state %+v", s)
	}
}

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
			Stream   bool `json:"stream"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
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
		if len(body.Messages) > 0 && body.Messages[0].Content == e2eSlowPrompt {
			// a long stream: a chunk a second, then the usage
			for range 6 {
				send(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"tick "},"finish_reason":null}]}`)
				time.Sleep(time.Second)
			}
			send(fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o-mini","choices":[],"usage":%s}`, usage))
			send("[DONE]")
			return
		}
		if len(body.Messages) > 0 && body.Messages[0].Content == e2eStallPrompt {
			// some text, then nothing until the caller gives up: usage never arrives
			for range 4 {
				send(fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, strings.Repeat("a", 100)))
			}
			select {
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
			return
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

// e2eStallPrompt makes the mock provider stream a little and then stall;
// e2eSlowPrompt makes it stream for six seconds.
const (
	e2eStallPrompt = "stall"
	e2eSlowPrompt  = "slow"
)

const (
	e2eAdminUser     = "e2e-admin"
	e2eAdminPassword = "e2e-admin-password"
)

type e2eGateway struct {
	url string
	// logPath is the gateway's own log, for what it says at boot.
	logPath string
}

// shippedConfig is the config we deploy (apps/gateway/compose/config.json):
// content logging, retention, inference auth, the admin, the encryption key and
// the credits plugin are tested as shipped, not as each test would set them.
// The Postgres stores are left out: the gateway's default local stores serve
// the tests.
func shippedConfig(t *testing.T, migrations string) map[string]any {
	t.Helper()
	path := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(migrations))), "apps", "gateway", "compose", "config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var shipped map[string]any
	if err := json.Unmarshal(raw, &shipped); err != nil || shipped["client"] == nil {
		t.Fatalf("%s has no client block: %v", path, err)
	}
	delete(shipped, "config_store")
	delete(shipped, "logs_store")
	return shipped
}

// shippedEnv is what the shipped config reads from the environment.
func shippedEnv(ledgerURL string) []string {
	return []string{
		"GATEWAY_ADMIN_USER=" + e2eAdminUser,
		"GATEWAY_ADMIN_PASSWORD=" + e2eAdminPassword,
		"GATEWAY_ENCRYPTION_KEY=e2e-encryption-key-0123456789abcdef",
		"GATEWAY_LEDGER_DATABASE_URL=" + ledgerURL,
	}
}

// startGateway boots the binary with the shipped config, charging the ledger at
// ledgerURL, and waits for /health.
func startGateway(t *testing.T, binary string, provider *mockProvider, ledgerURL string, organizations []string) *e2eGateway {
	t.Helper()
	return bootGateway(t, binary, provider, nil, shippedEnv(ledgerURL), organizations)
}

// startGatewayWithCredits replaces the shipped credits plugin config with creditsConfig.
func startGatewayWithCredits(t *testing.T, binary string, provider *mockProvider, creditsConfig map[string]any, organizations []string) *e2eGateway {
	t.Helper()
	return bootGateway(t, binary, provider, creditsConfig, shippedEnv(""), organizations)
}

func bootGateway(t *testing.T, binary string, provider *mockProvider, creditsConfig map[string]any, env []string, organizations []string) *e2eGateway {
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
	config := shippedConfig(t, migrations)
	// what a deployment adds at run time: providers, customers and keys, and pricing
	config["providers"] = map[string]any{"openai": map[string]any{
		"keys":           []map[string]any{{"name": "mock", "value": "sk-mock-upstream", "models": []string{"*"}, "weight": 1}},
		"network_config": map[string]any{"base_url": provider.server.URL, "allow_private_network": true, "max_retries": 0},
	}}
	config["governance"] = map[string]any{"customers": customers, "virtual_keys": keys}
	config["framework"] = map[string]any{"pricing": map[string]any{
		"pricing_url":          "file://" + filepath.Join(pricing, "pricing.json"),
		"model_parameters_url": "file://" + filepath.Join(pricing, "model-parameters.json"),
	}}
	if creditsConfig != nil {
		config["plugins"] = []map[string]any{{"name": PluginName, "enabled": true, "config": creditsConfig}}
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
	cmd.Env = append(os.Environ(), env...)
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

	gw := &e2eGateway{url: fmt.Sprintf("http://127.0.0.1:%d", port), logPath: logPath}
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
	status, out, _ := g.postWithHeaders(t, path, key, body)
	return status, out
}

func (g *e2eGateway) postWithHeaders(t *testing.T, path, key string, body map[string]any) (int, string, http.Header) {
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
	return resp.StatusCode, string(out), resp.Header
}

// e2eRefusal is a refusal as a client reads it (SPEC API-08, docs/platform/api-errors.md): the
// HTTP status as error.code, a message and type, and the metadata.
type e2eRefusal struct {
	Error struct {
		Code     int            `json:"code"`
		Message  string         `json:"message"`
		Type     string         `json:"type"`
		Metadata map[string]any `json:"metadata"`
	} `json:"error"`
}

// checkRefusal reads a refusal body and checks the contract every refusal keeps.
func checkRefusal(t *testing.T, status int, body string, wantSource, wantReason string) e2eRefusal {
	t.Helper()
	var refusal e2eRefusal
	if err := json.Unmarshal([]byte(body), &refusal); err != nil {
		t.Fatalf("not JSON: %s", body)
	}
	if refusal.Error.Code != status || refusal.Error.Message == "" || refusal.Error.Type == "" {
		t.Fatalf("error object %+v: want code %d, a message and a type", refusal.Error, status)
	}
	m := refusal.Error.Metadata
	if m["limit_source"] != wantSource || m["reason"] != wantReason || m["remedy_hint"] == "" || m["remedy_hint"] == nil {
		t.Fatalf("metadata %v: want %s %s with a remedy hint", m, wantSource, wantReason)
	}
	return refusal
}

// hasProviderKey asks the gateway whether it holds a provider key with this id.
func (g *e2eGateway) hasProviderKey(t *testing.T, keyID string) bool {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, g.url+"/api/keys", nil)
	req.SetBasicAuth(e2eAdminUser, e2eAdminPassword)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var keys []struct {
		KeyID string `json:"key_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		t.Fatalf("GET /api/keys: %v", err)
	}
	for _, key := range keys {
		if keyID != "" && key.KeyID == keyID {
			return true
		}
	}
	return false
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
	cutOff := db.organization(t, usd)
	gw := startGateway(t, binary, provider, ledgerURL(t, db), []string{funded, broke, cutOff})

	// gpt-4o-mini in the local pricing file: 0.15 USD per million input tokens, 0.60 per million output
	providerCost := e2ePromptTokens*0.15e-6 + e2eCompletionTokens*0.60e-6 // 0.00045 USD
	// in SAR at the rate the migration seeds (3.75, the peg), with no markup: 0.00045 x 3.75 =
	// 1,687.5 micro-SAR on paper, but the float sum is a hair under 0.00045, so the exact
	// conversion of the value actually held is 1,687.4999... and rounds to 1,687
	const charge = int64(1687)
	if got, err := toMicroSAR(providerCost, pegRate); err != nil || got != charge {
		t.Fatalf("expected charge: %d, %v", got, err)
	}

	t.Run("a chat completion is charged once at the provider's cost times the rate, in SAR", func(t *testing.T) {
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
		// there is no markup to record: the charge is the provider's cost at the rate
		var hasMarkup bool
		err := db.admin.QueryRow(context.Background(), `SELECT (metadata->>'provider_cost_usd')::float8, metadata->>'model', reference, metadata ? 'markup_bps' FROM credit_ledger_entry WHERE organization_id = $1 AND kind = 'usage'`, funded).Scan(&cost, &model, &reference, &hasMarkup)
		if err != nil || cost < providerCost-1e-9 || cost > providerCost+1e-9 || !strings.Contains(model, "gpt-4o-mini") || reference == "" || hasMarkup {
			t.Fatalf("entry: cost=%v model=%q reference=%q markup recorded=%v err=%v", cost, model, reference, hasMarkup, err)
		}
		// the provider key that served it is on the entry: the gateway's own id for it
		var providerKey string
		err = db.admin.QueryRow(context.Background(), `SELECT coalesce(metadata->>'provider_key_id', '') FROM credit_ledger_entry WHERE organization_id = $1 AND kind = 'usage'`, funded).Scan(&providerKey)
		if err != nil || !gw.hasProviderKey(t, providerKey) {
			t.Fatalf("provider key on the entry: %q, %v", providerKey, err)
		}
		var prompt, completion int
		err = db.admin.QueryRow(context.Background(), `SELECT (metadata->>'prompt_tokens')::int, (metadata->>'completion_tokens')::int FROM credit_ledger_entry WHERE organization_id = $1 AND kind = 'usage'`, funded).Scan(&prompt, &completion)
		if err != nil || prompt != e2ePromptTokens || completion != e2eCompletionTokens {
			t.Fatalf("token counts on the entry: %d %d %v", prompt, completion, err)
		}
		// the entry stores the provider's USD cost, the official rate, the spread (none is
		// seeded) and the effective rate it was converted at, as columns, and the hold it
		// settled was taken at the same rate
		var amount, costMicroUSD, rate, spread, effective, holdEffective int64
		err = db.admin.QueryRow(context.Background(), `
			SELECT -e.amount, e.cost_micro_usd, e.usd_to_sar_rate_e6, e.fx_spread_bps, e.effective_usd_to_sar_rate_e6, r.effective_usd_to_sar_rate_e6
			  FROM credit_ledger_entry e JOIN credit_reservation r ON e.idempotency_key = 'usage:' || r.id
			 WHERE e.organization_id = $1 AND e.kind = 'usage'`, funded).Scan(&amount, &costMicroUSD, &rate, &spread, &effective, &holdEffective)
		if err != nil || amount != charge || costMicroUSD != 450 || rate != pegRate || spread != 0 || effective != pegRate || holdEffective != pegRate {
			t.Fatalf("entry: %d micro-SAR for %d micro-USD at %d + %d bps = %d (hold at %d), err=%v; want %d for 450 at %d", amount, costMicroUSD, rate, spread, effective, holdEffective, err, charge, pegRate)
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

	t.Run("a stream the caller abandons is charged its input and what was streamed", func(t *testing.T) {
		body := chatBody(true)
		body["messages"] = []map[string]any{{"role": "user", "content": e2eStallPrompt}}
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, gw.url+"/v1/chat/completions", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer sk-bf-e2e-"+cutOff)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		// read until the streamed text has arrived, then hang up
		var seen strings.Builder
		buf := make([]byte, 4096)
		for strings.Count(seen.String(), "aaaa") < 4*25 {
			n, err := resp.Body.Read(buf)
			seen.Write(buf[:n])
			if err != nil {
				t.Fatalf("stream ended before the text arrived: %v: %s", err, seen.String())
			}
		}
		resp.Body.Close()

		s := settled(t, db, cutOff)
		if s.UsageEntries != 1 || s.Held != 0 {
			t.Fatalf("state %+v, want one usage entry and no open hold", s)
		}
		// the provider never reported usage; the gateway counts the prompt and the
		// text received (core/streamusage, fed by the logging plugin's record of the stream)
		var cost, held int64
		var prompt, completion int
		err = db.admin.QueryRow(context.Background(), `SELECT -amount, (metadata->>'held')::bigint, coalesce((metadata->>'prompt_tokens')::int, 0), coalesce((metadata->>'completion_tokens')::int, 0) FROM credit_ledger_entry WHERE organization_id = $1 AND kind = 'usage'`, cutOff).Scan(&cost, &held, &prompt, &completion)
		if err != nil || cost <= 0 || cost > held || prompt == 0 || completion == 0 {
			t.Fatalf("entry: cost=%d held=%d prompt=%d completion=%d err=%v", cost, held, prompt, completion, err)
		}
	})

	t.Run("a request whose cost the hold cannot bound is refused and the provider is never called", func(t *testing.T) {
		before := provider.chats.Load()
		for name, extra := range map[string]map[string]any{
			"priority tier":     {"service_tier": "priority"},
			"server-side tools": {"tools": []map[string]any{{"type": "web_search"}}},
		} {
			body := chatBody(false)
			for k, v := range extra {
				body[k] = v
			}
			status, out := gw.post(t, "/v1/chat/completions", "sk-bf-e2e-"+funded, body)
			if status != 400 || !strings.Contains(out, ErrorTypeUnsupportedRequest) {
				t.Errorf("%s: status %d: %s", name, status, out)
			}
			checkRefusal(t, status, out, "credits", ReasonCostCannotBeBounded)
		}
		if provider.chats.Load() != before {
			t.Fatal("the provider was called for a request that was refused")
		}
	})

	t.Run("a balance that cannot cover the request gets 402 and the provider is never called", func(t *testing.T) {
		before := provider.chats.Load()
		status, body, headers := gw.postWithHeaders(t, "/v1/chat/completions", "sk-bf-e2e-"+broke, chatBody(false))
		if status != 402 || !strings.Contains(body, ErrorTypeInsufficientCredits) {
			t.Fatalf("status %d: %s", status, body)
		}
		// 10 micro-SAR against a hold of thousands: retrying will not help, so no Retry-After
		refusal := checkRefusal(t, status, body, "credits", ReasonRequestExceedsBalance)
		if m := refusal.Error.Metadata; m["balance"] != 0.00001 || m["currency"] != "SAR" || m["required"] == nil {
			t.Fatalf("amounts %v", m)
		}
		if headers.Get("Retry-After") != "" {
			t.Fatal("Retry-After on a refusal retrying cannot fix")
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
		checkRefusal(t, status, body, "key", ReasonKeyHasNoOrganization)
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
		// a model the key may use reaches the plugin, which refuses the switched-off type in the
		// error contract (SPEC API-08), with no Retry-After: switching it on is not a matter of time
		status, body, headers := gw.postWithHeaders(t, "/v1/images/generations", "sk-bf-e2e-"+funded, map[string]any{"model": "openai/gpt-4o-mini", "prompt": "a cat"})
		if status != 403 || !strings.Contains(body, ErrorTypeUnsupportedRequest) {
			t.Fatalf("status %d: %s", status, body)
		}
		if refusal := checkRefusal(t, status, body, "request_type", ReasonRequestTypeNotEnabled); refusal.Error.Metadata["request_type"] != "image_generation" {
			t.Fatalf("metadata %v", refusal.Error.Metadata)
		}
		if headers.Get("Retry-After") != "" {
			t.Fatal("Retry-After on a switched-off request type")
		}
		if provider.others.Load() != before {
			t.Fatal("the provider was called for a request that was refused")
		}
		if s := db.consistent(t, funded); s.UsageEntries != 2 || s.Held != 0 {
			t.Fatalf("state %+v", s)
		}
	})

	t.Run("a rate and spread staff change apply to the next request only, and past entries keep theirs", func(t *testing.T) {
		if testing.Short() {
			t.Skip("waits for the plugin's 30 second refresh")
		}
		entries := func() map[string][2]int64 {
			rows, err := db.admin.Query(context.Background(), `SELECT id, -amount, effective_usd_to_sar_rate_e6 FROM credit_ledger_entry WHERE organization_id = $1 AND kind = 'usage'`, funded)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			out := map[string][2]int64{}
			for rows.Next() {
				var id string
				var amount, rate int64
				if err := rows.Scan(&id, &amount, &rate); err != nil {
					t.Fatal(err)
				}
				out[id] = [2]int64{amount, rate}
			}
			return out
		}

		// one request at the peg
		if status, body := gw.post(t, "/v1/chat/completions", "sk-bf-e2e-"+funded, chatBody(false)); status != 200 {
			t.Fatalf("status %d: %s", status, body)
		}
		settled(t, db, funded)
		past := entries()
		atPeg, _ := toMicroSAR(providerCost, pegRate)

		// staff set 4.00 and a 2 percent bank spread; the next request converts at 4.08
		db.setRate(t, 4_000_000)
		db.setSpread(t, 200)
		const newRate = int64(4_080_000)
		// a markup left over from the old schema, readable by the gateway, cannot reach the
		// charge: the plugin no longer reads one
		if _, err := db.admin.Exec(context.Background(), `
ALTER TABLE pricing_settings ADD COLUMN default_markup_bps integer NOT NULL DEFAULT 2000;
CREATE TABLE provider_markup (provider text PRIMARY KEY, markup_bps integer NOT NULL);
INSERT INTO provider_markup VALUES ('openai', 2000);
GRANT SELECT ON provider_markup TO gateway_ledger`); err != nil {
			t.Fatal(err)
		}
		time.Sleep(rateRefreshInterval + 2*time.Second)
		before := db.consistent(t, funded)
		if status, body := gw.post(t, "/v1/chat/completions", "sk-bf-e2e-"+funded, chatBody(false)); status != 200 {
			t.Fatalf("status %d: %s", status, body)
		}
		// 0.00045 x 4.08 = 0.001836 SAR exactly: the provider's cost at the new rate, no markup
		const atNewRate = int64(1836)
		if got, _ := toMicroSAR(providerCost, newRate); got != atNewRate {
			t.Fatalf("expected charge at the new rate: %d", got)
		}
		var official, spread int64
		if s := settled(t, db, funded); s.Balance != before.Balance-atNewRate || atNewRate <= atPeg {
			t.Fatalf("charged %d, want %d at the new rate (it was %d at the peg)", before.Balance-s.Balance, atNewRate, atPeg)
		}
		if err := db.admin.QueryRow(context.Background(), `SELECT usd_to_sar_rate_e6, fx_spread_bps FROM credit_ledger_entry WHERE organization_id = $1 AND kind = 'usage' ORDER BY created_at DESC LIMIT 1`, funded).Scan(&official, &spread); err != nil || official != 4_000_000 || spread != 200 {
			t.Fatalf("the new entry stores %d + %d bps, want 4,000,000 + 200 (%v)", official, spread, err)
		}

		now := entries()
		if len(now) != len(past)+1 {
			t.Fatalf("%d entries, want %d", len(now), len(past)+1)
		}
		for id, entry := range now {
			if old, ok := past[id]; ok {
				if old != entry {
					t.Errorf("entry %s changed from %v to %v when the rate changed", id, old, entry)
				}
				continue
			}
			if entry != [2]int64{atNewRate, newRate} {
				t.Errorf("new entry %v, want %d at %d", entry, atNewRate, newRate)
			}
		}
		last := 0
		for _, entry := range past {
			if entry[1] == pegRate && entry[0] == atPeg {
				last++
			}
		}
		if last == 0 {
			t.Errorf("no past entry of %d at the peg: %v", atPeg, past)
		}
	})

	t.Run("no key at all is refused by the gateway itself", func(t *testing.T) {
		status, _ := gw.post(t, "/v1/chat/completions", "", chatBody(false))
		if status != 401 {
			t.Fatalf("status %d, want 401", status)
		}
	})

	t.Run("the management API needs the admin", func(t *testing.T) {
		for _, admin := range []bool{false, true} {
			req, _ := http.NewRequest(http.MethodGet, gw.url+"/api/providers", nil)
			if admin {
				req.SetBasicAuth(e2eAdminUser, e2eAdminPassword)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if want := map[bool]int{false: 401, true: 200}[admin]; resp.StatusCode != want {
				t.Errorf("admin=%v: status %d, want %d", admin, resp.StatusCode, want)
			}
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

	for name, c := range map[string]struct {
		config     map[string]any
		wantReason string
		retryAfter string // a database that is down comes back; a broken config does not
	}{
		"no database url":             {map[string]any{}, ReasonCreditsNotConfigured, ""},
		"a config of the wrong shape": {map[string]any{"database_url": 5}, ReasonCreditsNotConfigured, ""},
		"a database that is down":     {map[string]any{"database_url": "postgres://nobody:nothing@127.0.0.1:1/none"}, ReasonLedgerUnavailable, "5"},
	} {
		t.Run(name, func(t *testing.T) {
			gw := startGatewayWithCredits(t, binary, provider, c.config, []string{org})
			before := provider.chats.Load()
			status, body, headers := gw.postWithHeaders(t, "/v1/chat/completions", "sk-bf-e2e-"+org, chatBody(false))
			if status != 503 || !strings.Contains(body, ErrorTypeLedgerUnavailable) {
				t.Fatalf("status %d: %s", status, body)
			}
			checkRefusal(t, status, body, "service", c.wantReason)
			if got := headers.Get("Retry-After"); got != c.retryAfter {
				t.Fatalf("Retry-After %q, want %q", got, c.retryAfter)
			}
			if strings.Contains(body, "nothing@") {
				t.Fatal("the refusal leaks the database url")
			}
			if provider.chats.Load() != before {
				t.Fatal("the provider was called although the request could not be accounted for")
			}
		})
	}
	t.Run("the shipped config with no ledger url set", func(t *testing.T) {
		gw := startGateway(t, binary, provider, "", []string{org})
		before := provider.chats.Load()
		status, body := gw.post(t, "/v1/chat/completions", "sk-bf-e2e-"+org, chatBody(false))
		if status != 503 || !strings.Contains(body, ErrorTypeLedgerUnavailable) {
			t.Fatalf("status %d: %s", status, body)
		}
		if provider.chats.Load() != before {
			t.Fatal("the provider was called although the request could not be accounted for")
		}
	})
	if s := db.consistent(t, org); s.Balance != usd || s.UsageEntries != 0 {
		t.Fatalf("state %+v", s)
	}
}

func TestEndToEnd_AStreamThatOutlivesItsHoldKeepsIt(t *testing.T) {
	binary := os.Getenv("CREDITS_E2E_BINARY")
	if binary == "" {
		t.Skip("CREDITS_E2E_BINARY is not set")
	}
	db := newTestDB(t)
	provider := newMockProvider(t)
	org := db.organization(t, usd)
	// a hold lives 3 seconds; the stream takes 6
	gw := startGatewayWithCredits(t, binary, provider, map[string]any{"database_url": ledgerURL(t, db), "reservation_ttl_seconds": 3}, []string{org})

	done := make(chan int, 1)
	go func() {
		body := chatBody(true)
		body["messages"] = []map[string]any{{"role": "user", "content": e2eSlowPrompt}}
		status, _ := gw.post(t, "/v1/chat/completions", "sk-bf-e2e-"+org, body)
		done <- status
	}()

	time.Sleep(4500 * time.Millisecond)
	var live, expired int
	err := db.admin.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE expires_at > now()), count(*) FILTER (WHERE expires_at <= now()) FROM credit_reservation WHERE organization_id = $1 AND settled_at IS NULL`, org).Scan(&live, &expired)
	if err != nil || live != 1 || expired != 0 {
		t.Fatalf("1.5 seconds past the hold's first lifetime: %d live, %d expired open holds (%v)", live, expired, err)
	}

	if status := <-done; status != 200 {
		t.Fatalf("status %d", status)
	}
	if s := settled(t, db, org); s.UsageEntries != 1 || s.Held != 0 {
		t.Fatalf("state %+v, want one usage entry and no open hold", s)
	}
}

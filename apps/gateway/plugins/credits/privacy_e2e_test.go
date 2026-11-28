package credits

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Prompts and completions are not stored, whatever the request asks, and logs are kept
// for the retention the deployment ships with (docs/platform/PLAN.md, step 9.0). The
// gateway runs with the "client" block of apps/gateway/compose/config.json.
func TestEndToEnd_PromptsAndCompletionsAreNotStored(t *testing.T) {
	binary := os.Getenv("CREDITS_E2E_BINARY")
	if binary == "" {
		t.Skip("CREDITS_E2E_BINARY is not set")
	}
	db := newTestDB(t)
	provider := newMockProvider(t)
	org := db.organization(t, usd)
	gw := startGateway(t, binary, provider, ledgerURL(t, db), []string{org})

	// a request that asks for its content to be kept: the shipped config must ignore it
	const secret = "the-customer-prompt-must-not-be-stored"
	body, _ := json.Marshal(map[string]any{
		"model":    "openai/gpt-4o-mini",
		"messages": []map[string]any{{"role": "user", "content": secret}},
	})
	req, _ := http.NewRequest(http.MethodPost, gw.url+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-bf-e2e-"+org)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-bf-disable-content-logging", "false")
	req.Header.Set("x-bf-store-raw-request-response", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat: %d %s", resp.StatusCode, answer)
	}
	settled(t, db, org)

	// the log is written after the response; wait for it
	var entry map[string]any
	var page string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline) && entry == nil; time.Sleep(200 * time.Millisecond) {
		logsReq, _ := http.NewRequest(http.MethodGet, gw.url+"/api/logs?limit=10", nil)
		logsReq.SetBasicAuth(e2eAdminUser, e2eAdminPassword)
		r, err := http.DefaultClient.Do(logsReq)
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(r.Body)
		r.Body.Close()
		page = string(raw)
		var logs struct {
			Logs []map[string]any `json:"logs"`
		}
		if json.Unmarshal(raw, &logs) == nil && len(logs.Logs) > 0 {
			entry = logs.Logs[0]
		}
	}
	if entry == nil {
		t.Fatalf("no log entry appeared: %s", page)
	}

	if strings.Contains(page, secret) || strings.Contains(page, "hello") {
		t.Fatalf("the log API returned the prompt or the completion: %s", page)
	}
	for _, field := range []string{"input_history", "output_message", "content_summary", "raw_request", "raw_response"} {
		if value, ok := entry[field]; ok && value != nil && value != "" {
			t.Errorf("log field %s was stored: %v", field, value)
		}
	}
	// what usage reports need is still there
	for _, field := range []string{"model", "provider", "status"} {
		if entry[field] == nil || entry[field] == "" {
			t.Errorf("log field %s is missing: %v", field, entry)
		}
	}
	if tokens, _ := entry["total_tokens"].(float64); tokens == 0 {
		if usage, _ := entry["token_usage"].(map[string]any); usage == nil || usage["total_tokens"] == nil {
			t.Errorf("token counts are missing from the log: %v", entry)
		}
	}

	// the retention it shipped with is the one in force
	boot, _ := os.ReadFile(gw.logPath)
	if !strings.Contains(string(boot), "log retention days: 90") {
		t.Errorf("the gateway did not start with 90 days of log retention")
	}
}

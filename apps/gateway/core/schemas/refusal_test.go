package schemas

import (
	"encoding/json"
	"testing"
	"time"
)

// A refusal the product made itself is written in its error contract (docs/platform/api-errors.md):
// error.code is the HTTP status as a number, error.metadata names the limit, and error.message and
// error.type stay where OpenAI SDKs read them.
func TestRefusalIsWrittenAsTheErrorContract(t *testing.T) {
	bifrostErr := NewRefusalError(402, "insufficient_credits", "Insufficient credits.", Refusal{
		LimitSource: LimitSourceCredits,
		Reason:      "insufficient_balance",
		RemedyHint:  "Add credits to continue.",
		Details:     map[string]any{"balance": 0.0, "reason": "a detail never overrides the fixed fields"},
	})
	body, err := json.Marshal(bifrostErr)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Type       string `json:"type"`
		StatusCode int    `json:"status_code"`
		Error      struct {
			Code     any            `json:"code"`
			Type     string         `json:"type"`
			Message  string         `json:"message"`
			Metadata map[string]any `json:"metadata"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Error.Code != float64(402) {
		t.Fatalf("error.code = %#v, want the number 402: %s", decoded.Error.Code, body)
	}
	if decoded.Error.Type != "insufficient_credits" || decoded.Type != "insufficient_credits" || decoded.StatusCode != 402 || decoded.Error.Message != "Insufficient credits." {
		t.Fatalf("body %s", body)
	}
	m := decoded.Error.Metadata
	if m["limit_source"] != "credits" || m["reason"] != "insufficient_balance" || m["remedy_hint"] != "Add credits to continue." || m["balance"] != 0.0 {
		t.Fatalf("metadata %v", m)
	}
	if _, has := m["retry_after_seconds"]; has {
		t.Fatal("no retry_after_seconds on a refusal retrying cannot fix")
	}
}

func TestAnErrorWithoutARefusalIsWrittenAsBefore(t *testing.T) {
	code := "context_length_exceeded"
	body, err := json.Marshal(&ErrorField{Message: "too long", Code: &code})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"code":"context_length_exceeded","message":"too long"}` {
		t.Fatalf("body %s", body)
	}
}

func TestRetryAfterIsWholeSecondsRoundedUp(t *testing.T) {
	r := &Refusal{RetryAfter: 1500 * time.Millisecond}
	if r.RetryAfterSeconds() != 2 || r.Metadata()["retry_after_seconds"] != int64(2) {
		t.Fatalf("seconds %d, metadata %v", r.RetryAfterSeconds(), r.Metadata())
	}
	var none *Refusal
	if none.RetryAfterSeconds() != 0 || none.Metadata() != nil {
		t.Fatal("a nil refusal has no Retry-After and no metadata")
	}
}

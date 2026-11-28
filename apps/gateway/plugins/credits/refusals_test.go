package credits

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// metadataOf is the refusal as a client reads it: the error object of the JSON body.
func metadataOf(t *testing.T, sc *schemas.LLMPluginShortCircuit) (map[string]any, *schemas.Refusal) {
	t.Helper()
	body, err := json.Marshal(sc.Error)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded.Error, sc.Error.Error.Refusal
}

func TestCreditsRefusal_SaysWhetherRetryingCanHelp(t *testing.T) {
	cases := []struct {
		name       string
		shortfall  Shortfall
		amount     int64
		wantReason string
		wantRetry  bool
	}{
		{"no account", Shortfall{}, 2_000, ReasonInsufficientBalance, false},
		{"a zero balance", Shortfall{HasAccount: true}, 2_000, ReasonInsufficientBalance, false},
		{"one request costs more than the whole balance", Shortfall{HasAccount: true, Balance: 1_000}, 2_000, ReasonRequestExceedsBalance, false},
		{"the balance is held by requests still running", Shortfall{HasAccount: true, Balance: 5_000, Held: 4_000}, 2_000, ReasonBalanceHeldInFlight, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc := creditsRefusal(c.shortfall, true, c.amount)
			if *sc.Error.StatusCode != 402 || *sc.Error.Type != ErrorTypeInsufficientCredits {
				t.Fatalf("refused with %d %s", *sc.Error.StatusCode, *sc.Error.Type)
			}
			errorObject, refusal := metadataOf(t, sc)
			metadata, _ := errorObject["metadata"].(map[string]any)
			if errorObject["code"] != float64(402) || errorObject["type"] != ErrorTypeInsufficientCredits || errorObject["message"] == "" {
				t.Fatalf("error object %v: want numeric code 402, the type and a message (what OpenAI SDKs read)", errorObject)
			}
			if metadata["limit_source"] != "credits" || metadata["reason"] != c.wantReason || metadata["remedy_hint"] == "" || metadata["currency"] != "SAR" {
				t.Fatalf("metadata %v", metadata)
			}
			if metadata["required"] != sar(c.amount) || metadata["available"] != sar(max(0, c.shortfall.Balance-c.shortfall.Held)) {
				t.Fatalf("amounts %v", metadata)
			}
			if got := refusal.RetryAfterSeconds() > 0; got != c.wantRetry {
				t.Fatalf("Retry-After given: %v, want %v", got, c.wantRetry)
			}
			if _, has := metadata["retry_after_seconds"]; has != c.wantRetry {
				t.Fatalf("retry_after_seconds in metadata: %v, want %v", has, c.wantRetry)
			}
		})
	}

	t.Run("a balance that could not be read still refuses as credits, without numbers", func(t *testing.T) {
		errorObject, refusal := metadataOf(t, creditsRefusal(Shortfall{}, false, 2_000))
		metadata, _ := errorObject["metadata"].(map[string]any)
		if refusal.Reason != ReasonInsufficientBalance || metadata["limit_source"] != "credits" {
			t.Fatalf("refusal %+v", refusal)
		}
		if _, has := metadata["balance"]; has {
			t.Fatal("numbers the plugin could not read must not be made up")
		}
	})
}

func TestKeyLimitRefusal_GivesTheNumbersAndTheReset(t *testing.T) {
	limit := int64(10_000_000) // SAR 10
	resetsAt := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC) // read from key_limit_window_end by Shortfall

	sc := keyLimitRefusal(Shortfall{KeyLimit: &limit, KeySpent: 9_500_000, KeyReset: "daily", KeyResetsAt: &resetsAt}, true, 1_000_000)
	if *sc.Error.StatusCode != 402 || *sc.Error.Type != ErrorTypeKeyLimit {
		t.Fatalf("refused with %d %s, want 402 %s", *sc.Error.StatusCode, *sc.Error.Type, ErrorTypeKeyLimit)
	}
	errorObject, refusal := metadataOf(t, sc)
	metadata, _ := errorObject["metadata"].(map[string]any)
	want := map[string]any{
		"limit_source": "key_limit", "reason": ReasonKeyLimitReached, "limit": 10.0, "spent": 9.5,
		"remaining": 0.5, "required": 1.0, "limit_reset": "daily", "resets_at": "2026-11-28T00:00:00Z", "currency": "SAR",
	}
	for k, v := range want {
		if metadata[k] != v {
			t.Errorf("metadata[%s] = %v, want %v", k, metadata[k], v)
		}
	}
	if refusal.RetryAfterSeconds() != 0 {
		t.Error("waiting for a key's window is hours away: no Retry-After")
	}

	t.Run("one request bigger than the whole limit says retrying will not help", func(t *testing.T) {
		sc := keyLimitRefusal(Shortfall{KeyLimit: &limit}, true, limit+1)
		if reason := sc.Error.Error.Refusal.Reason; reason != ReasonRequestExceedsKeyLimit {
			t.Fatalf("reason %s", reason)
		}
	})

	t.Run("a limit with no reset has no reset time", func(t *testing.T) {
		errorObject, _ := metadataOf(t, keyLimitRefusal(Shortfall{KeyLimit: &limit, KeySpent: limit}, true, 1))
		metadata, _ := errorObject["metadata"].(map[string]any)
		if metadata["resets_at"] != nil || metadata["limit_reset"] != nil {
			t.Fatalf("metadata %v", metadata)
		}
	})
}

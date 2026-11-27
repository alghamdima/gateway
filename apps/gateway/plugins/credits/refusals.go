package credits

import (
	"context"
	"fmt"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// Every refusal of this plugin is answered in the product's error contract (SPEC API-08,
// docs/platform/api-errors.md): the HTTP status, a type, a message, and metadata saying which
// limit refused the request (limit_source), why (reason), what to do (remedy_hint) and, for money,
// the numbers in SAR. Clients branch on limit_source and reason, never on the message.

// currency is the unit of every amount in a refusal's metadata (docs/platform/adr/0017-sar-ledger.md).
const currency = "SAR"

// Retry-After values, for the refusals where retrying the same request can succeed.
const (
	// ledgerRetryAfter: the ledger or the prices could not be read; a connection usually comes back
	ledgerRetryAfter = 5 * time.Second
	// inFlightRetryAfter: the balance is held by requests still running, which settle when they end
	inFlightRetryAfter = 5 * time.Second
)

// The reasons this plugin gives, by limit source.
const (
	ReasonInsufficientBalance    = "insufficient_balance"
	ReasonRequestExceedsBalance  = "request_exceeds_balance"
	ReasonBalanceHeldInFlight    = "balance_held_by_requests_in_flight"
	ReasonKeyLimitReached        = "key_limit_reached"
	ReasonRequestExceedsKeyLimit = "request_exceeds_key_limit"
	ReasonRequestTypeNotEnabled  = "request_type_not_enabled"
	ReasonRequestEmpty           = "request_empty"
	ReasonCostCannotBeBounded    = "cost_cannot_be_bounded"
	ReasonModelNotPriced         = "model_not_priced"
	ReasonKeyHasNoOrganization   = "key_has_no_organization"
	ReasonLedgerUnavailable      = "ledger_unavailable"
	ReasonCreditsNotConfigured   = "credits_not_configured"
)

func refuse(status int, errorType, message string, refusal schemas.Refusal) *schemas.LLMPluginShortCircuit {
	allowFallbacks := false
	bifrostErr := schemas.NewRefusalError(status, errorType, message, refusal)
	bifrostErr.AllowFallbacks = &allowFallbacks
	return &schemas.LLMPluginShortCircuit{Error: bifrostErr}
}

func refuseLedgerUnavailable(message string) *schemas.LLMPluginShortCircuit {
	return refuse(503, ErrorTypeLedgerUnavailable, message, schemas.Refusal{
		LimitSource: schemas.LimitSourceService,
		Reason:      ReasonLedgerUnavailable,
		RemedyHint:  "A temporary problem on our side. Retry after the number of seconds in Retry-After.",
		RetryAfter:  ledgerRetryAfter,
	})
}

func refuseNoOrganization() *schemas.LLMPluginShortCircuit {
	return refuse(403, ErrorTypeNoOrganization, "This API key is not linked to an organization.", schemas.Refusal{
		LimitSource: schemas.LimitSourceKey,
		Reason:      ReasonKeyHasNoOrganization,
		RemedyHint:  "Use an API key created in your organization's dashboard.",
	})
}

func refuseUnbounded(message string) *schemas.LLMPluginShortCircuit {
	return refuse(400, ErrorTypeUnsupportedRequest, message, schemas.Refusal{
		LimitSource: schemas.LimitSourceCredits,
		Reason:      ReasonCostCannotBeBounded,
		RemedyHint:  "The most this request could cost cannot be worked out before it is sent. Remove the feature the message names and retry.",
	})
}

// refuseHold explains a hold the ledger refused, with the numbers read after the refusal. A failed
// read still refuses with the right status and source, only without the numbers.
func (p *Plugin) refuseHold(organizationID, virtualKeyID string, outcome ReserveOutcome, amount int64) *schemas.LLMPluginShortCircuit {
	ctx, cancel := context.WithTimeout(context.Background(), ledgerTimeout)
	defer cancel()
	shortfall, err := p.ledger.Shortfall(ctx, organizationID, virtualKeyID)
	known := err == nil
	if err != nil {
		p.logger.Warn("credits: cannot read why a hold for organization %s was refused: %v", organizationID, err)
	}
	if outcome == KeyLimitReached {
		return keyLimitRefusal(shortfall, known, amount, time.Now())
	}
	return creditsRefusal(shortfall, known, amount)
}

func creditsRefusal(s Shortfall, known bool, amount int64) *schemas.LLMPluginShortCircuit {
	refusal := schemas.Refusal{
		LimitSource: schemas.LimitSourceCredits,
		Reason:      ReasonInsufficientBalance,
		RemedyHint:  "Add credits to continue.",
	}
	message := "Insufficient credits. Add credits to continue."
	if known {
		available := max(0, s.Balance-s.Held)
		refusal.Details = map[string]any{
			"balance":   sar(s.Balance),
			"held":      sar(s.Held),
			"available": sar(available),
			"required":  sar(amount),
			"currency":  currency,
		}
		switch {
		case !s.HasAccount || s.Balance <= 0:
			// nothing to spend: the default above
		case amount > s.Balance:
			refusal.Reason = ReasonRequestExceedsBalance
			refusal.RemedyHint = "The most this request can cost is more than your whole balance, so retrying will not help. Add credits, or lower max_tokens."
			message = "Insufficient credits: the most this request can cost is more than your balance. Add credits, or lower max_tokens."
		default:
			// the balance covers it once the requests still running settle
			refusal.Reason = ReasonBalanceHeldInFlight
			refusal.RemedyHint = "Your balance is held by requests still running. Retry after the number of seconds in Retry-After, or add credits."
			refusal.RetryAfter = inFlightRetryAfter
			message = "Insufficient credits right now: your balance is held by requests still running."
		}
	}
	return refuse(402, ErrorTypeInsufficientCredits, message, refusal)
}

func keyLimitRefusal(s Shortfall, known bool, amount int64, now time.Time) *schemas.LLMPluginShortCircuit {
	refusal := schemas.Refusal{
		LimitSource: schemas.LimitSourceKeyLimit,
		Reason:      ReasonKeyLimitReached,
		RemedyHint:  "Raise this key's spending limit in the dashboard, or wait for it to reset.",
	}
	message := "This API key has reached its spending limit. Raise the limit, wait for it to reset, or lower max_tokens."
	if known && s.KeyLimit != nil {
		limit := *s.KeyLimit
		details := map[string]any{
			"limit":       sar(limit),
			"spent":       sar(s.KeySpent),
			"remaining":   sar(max(0, limit-s.KeySpent)),
			"required":    sar(amount),
			"limit_reset": nil,
			"resets_at":   nil,
			"currency":    currency,
		}
		if resetsAt, ok := windowEnd(s.KeyReset, now); ok {
			details["limit_reset"] = s.KeyReset
			details["resets_at"] = resetsAt.Format(time.RFC3339)
		}
		refusal.Details = details
		if amount > limit {
			refusal.Reason = ReasonRequestExceedsKeyLimit
			refusal.RemedyHint = "The most this request can cost is more than this key's whole limit, so retrying will not help. Raise the key's limit, or lower max_tokens."
			message = "The most this request can cost is more than this API key's spending limit. Raise the limit, or lower max_tokens."
		}
	}
	return refuse(402, ErrorTypeKeyLimit, message, refusal)
}

// windowEnd is when a key's limit window resets: the next UTC midnight, the next ISO week (Monday)
// or the next calendar month, the same windows the ledger counts in (keySpendSQL). False for a
// limit that never resets.
func windowEnd(reset string, now time.Time) (time.Time, bool) {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch reset {
	case "daily":
		return day.AddDate(0, 0, 1), true
	case "weekly":
		// days since Monday: Go's Sunday is 0
		sinceMonday := (int(day.Weekday()) + 6) % 7
		return day.AddDate(0, 0, 7-sinceMonday), true
	case "monthly":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0), true
	}
	return time.Time{}, false
}

// sar turns micro-SAR into SAR, as the public API writes amounts (packages/api/src/modules/v1/queries.ts).
func sar(microSAR int64) float64 {
	return float64(microSAR) / microPerUnit
}

func requestTypeRefusal(requestType schemas.RequestType) *schemas.LLMPluginShortCircuit {
	return refuse(403, ErrorTypeUnsupportedRequest, fmt.Sprintf("Requests of type %q are not available.", requestType), schemas.Refusal{
		LimitSource: schemas.LimitSourceRequestType,
		Reason:      ReasonRequestTypeNotEnabled,
		RemedyHint:  "This kind of request is not switched on yet. Chat, responses, text completions and embeddings are available.",
		Details:     map[string]any{"request_type": string(requestType)},
	})
}

package lib

import (
	"slices"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

const ClientSafeInternalErrorMessage = "internal server error"

// SanitizeBifrostErrorForClient returns a copy safe to serialize to API clients.
// Internal errors can contain stack traces or database details; keep those in logs only.
// A provider's error about the platform's own account with it, or its outage, is replaced by the
// product's refusal (hideProviderAccountError).
func SanitizeBifrostErrorForClient(err *schemas.BifrostError) *schemas.BifrostError {
	if err == nil {
		return nil
	}

	if hidden := hideProviderAccountError(err); hidden != nil {
		return hidden
	}

	sanitized := *err
	if err.Error != nil {
		errorField := *err.Error
		if shouldHideErrorDetails(err, err.Error) {
			errorField.Message = ClientSafeInternalErrorMessage
			errorField.Error = nil
			errorField.Param = nil
		}
		sanitized.Error = &errorField
	}
	sanitized.ExtraFields.FallbackAttempts = sanitizeFallbackAttempts(err.ExtraFields.FallbackAttempts)

	return &sanitized
}

// sanitizeFallbackAttempts applies the same check to each fallback's message, which an exhausted
// chain copies onto the primary's error. The slice is copied only when a message has to be hidden,
// so the original error, which logging may still hold, is never changed.
func sanitizeFallbackAttempts(attempts []schemas.FallbackAttempt) []schemas.FallbackAttempt {
	var out []schemas.FallbackAttempt
	for i, attempt := range attempts {
		message := attempt.Message
		if containsStackTrace(message) || containsSQLDetails(message) {
			message = ClientSafeInternalErrorMessage
		} else if attempt.FromProvider && attempt.StatusCode != nil {
			// a fallback's provider failed for a reason the caller is not told (see below)
			if class := providerErrorClassOf(*attempt.StatusCode, attempt.Type, "", attempt.Message); class != nil {
				message = class.message
			}
		}
		if message == attempt.Message {
			continue
		}
		if out == nil {
			out = slices.Clone(attempts)
		}
		out[i].Message = message
		if message != ClientSafeInternalErrorMessage {
			out[i].Type = ""
		}
	}
	if out == nil {
		return attempts
	}
	return out
}

func shouldHideErrorDetails(_ *schemas.BifrostError, field *schemas.ErrorField) bool {
	message := field.Message
	if field.Error != nil {
		message += " " + field.Error.Error()
	}

	return containsStackTrace(message) || containsSQLDetails(message)
}

func containsStackTrace(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "stack trace") ||
		strings.Contains(lower, "traceback (most recent call last)") ||
		strings.Contains(lower, "runtime/debug.stack") ||
		strings.Contains(lower, "goroutine ") ||
		strings.Contains(lower, "panic:") ||
		strings.Contains(lower, ".go:")
}

func containsSQLDetails(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "sqlstate") ||
		strings.Contains(lower, "sql:") ||
		strings.Contains(lower, "pq:") ||
		strings.Contains(lower, "pgx:") ||
		strings.Contains(lower, "duplicate key value violates") ||
		strings.Contains(lower, "violates foreign key constraint") ||
		strings.Contains(lower, "violates unique constraint") ||
		strings.Contains(lower, "syntax error at or near") ||
		strings.Contains(lower, "relation does not exist") ||
		strings.Contains(lower, "database/sql")
}

// providerErrorClass is what a caller is told in place of a provider's own error.
type providerErrorClass struct {
	status    int
	errorType string
	reason    string
	message   string
	hint      string
	// retryable: the same request can succeed later without anyone changing anything.
	retryable bool
}

var (
	providerUnavailable = &providerErrorClass{
		status:    fasthttp.StatusServiceUnavailable,
		errorType: "provider_unavailable",
		reason:    "provider_unavailable",
		message:   "This model's provider is not available right now. Try another model, or try again later.",
		hint:      "Nothing is wrong with your request or your account. Use another model, or try again later.",
	}
	providerRateLimited = &providerErrorClass{
		status:    fasthttp.StatusTooManyRequests,
		errorType: "provider_rate_limited",
		reason:    "provider_rate_limited",
		message:   "This model's provider is receiving too many requests right now. Try again shortly, or use another model.",
		hint:      "Wait and retry with backoff, or use another model.",
		retryable: true,
	}
	providerFailed = &providerErrorClass{
		status:    fasthttp.StatusBadGateway,
		errorType: "provider_error",
		reason:    "provider_error",
		message:   "This model's provider returned an error. Try again, or use another model.",
		hint:      "Retry the request, or use another model.",
		retryable: true,
	}
)

// providerErrorClassOf says what a caller is told when a provider answered with this status, or
// nil when the provider's own message is about the caller's request and is passed on.
//
// A provider's 401, 402, 403 and 429 are about the platform's account with that provider: its
// key, its permissions, its quota, its billing, its rate limit. The caller can do nothing about
// them and must not read them: "add credits at <the provider's billing page>" tells a customer
// whose own balance is fine that the platform has not paid its supplier. A 5xx or a timeout is the
// provider's outage, and its text names internals (request ids, support addresses). A 400, 404,
// 413 or 422 is about the request itself (a bad parameter, a prompt that is too long) and is what
// the developer needs to fix it, so it is passed on as it is.
func providerErrorClassOf(status int, errorType, code, message string) *providerErrorClass {
	switch {
	case status == fasthttp.StatusUnauthorized, status == fasthttp.StatusPaymentRequired, status == fasthttp.StatusForbidden:
		return providerUnavailable
	case status == fasthttp.StatusTooManyRequests:
		if isQuotaOrBilling(errorType + " " + code + " " + message) {
			return providerUnavailable
		}
		return providerRateLimited
	case status == fasthttp.StatusRequestTimeout, status >= fasthttp.StatusInternalServerError:
		return providerFailed
	}
	return nil
}

// isQuotaOrBilling tells a spent quota or an unpaid account from a rate limit: both arrive as 429,
// and only the rate limit passes by waiting.
func isQuotaOrBilling(text string) bool {
	lower := strings.ToLower(text)
	for _, word := range []string{"insufficient_quota", "quota", "billing", "credit", "balance", "payment"} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// hideProviderAccountError replaces a provider's error the caller must not read with the
// product's own refusal (docs/platform/api-errors.md), or returns nil when err is not one. Only
// the copy sent to the client changes: logging, settlement and fallbacks have already seen the
// original. Only an error marked as the provider's own answer is touched (BifrostError.FromProvider):
// a refusal the product made, a plugin's error and a gateway fault are not.
func hideProviderAccountError(err *schemas.BifrostError) *schemas.BifrostError {
	if !err.FromProvider || err.Error == nil || err.Error.Refusal != nil || err.StatusCode == nil {
		return nil
	}
	code := ""
	if err.Error.Code != nil {
		code = *err.Error.Code
	}
	errorType := ""
	if err.Error.Type != nil {
		errorType = *err.Error.Type
	}
	class := providerErrorClassOf(*err.StatusCode, errorType, code, err.Error.Message)
	if class == nil {
		return nil
	}

	refusal := schemas.Refusal{
		LimitSource: schemas.LimitSourceProvider,
		Reason:      class.reason,
		RemedyHint:  class.hint,
	}
	// the provider's own hint for a rate limit is a number, not its words: it is passed on
	if class == providerRateLimited && err.ExtraFields.RetryAfter > 0 {
		refusal.RetryAfter = time.Duration(err.ExtraFields.RetryAfter) * time.Millisecond
	}
	hidden := schemas.NewRefusalError(class.status, class.errorType, class.message, refusal)
	hidden.EventID = err.EventID
	hidden.ExtraFields = err.ExtraFields
	// the provider's body, the request as sent to it and its keys' states are not for the caller
	hidden.ExtraFields.RawRequest = nil
	hidden.ExtraFields.RawResponse = nil
	hidden.ExtraFields.KeyStatuses = nil
	hidden.ExtraFields.FallbackAttempts = sanitizeFallbackAttempts(err.ExtraFields.FallbackAttempts)
	return hidden
}

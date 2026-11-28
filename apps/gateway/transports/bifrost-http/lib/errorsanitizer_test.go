package lib

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func TestSanitizeBifrostErrorForClientHidesInternalDetails(t *testing.T) {
	statusCode := fasthttp.StatusInternalServerError
	err := &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     &statusCode,
		Error: &schemas.ErrorField{
			Message: "failed to create customer: pq: duplicate key value violates unique constraint users_email_key",
			Error:   errors.New("goroutine 1 [running]:\nmain.handler\n\t/app/server.go:42"),
			Param:   "users_email_key",
		},
	}

	sanitized := SanitizeBifrostErrorForClient(err)

	if sanitized == err {
		t.Fatal("expected sanitizer to return a copy")
	}
	if sanitized.Error.Message != ClientSafeInternalErrorMessage {
		t.Fatalf("expected generic message, got %q", sanitized.Error.Message)
	}
	if sanitized.Error.Error != nil {
		t.Fatalf("expected sensitive nested error to be removed, got %v", sanitized.Error.Error)
	}
	if sanitized.Error.Param != nil {
		t.Fatalf("expected param to be removed, got %v", sanitized.Error.Param)
	}
	if err.Error.Message == ClientSafeInternalErrorMessage || err.Error.Error == nil || err.Error.Param == nil {
		t.Fatal("expected original error to remain unchanged")
	}
}

func TestSanitizeBifrostErrorForClientPreservesClientValidationMessage(t *testing.T) {
	statusCode := fasthttp.StatusBadRequest
	err := &schemas.BifrostError{
		StatusCode: &statusCode,
		Error: &schemas.ErrorField{
			Message: "model is required",
			Error:   errors.New("missing model"),
			Param:   "model",
		},
	}

	sanitized := SanitizeBifrostErrorForClient(err)

	if sanitized.Error.Message != "model is required" {
		t.Fatalf("expected validation message to be preserved, got %q", sanitized.Error.Message)
	}
	if sanitized.Error.Param != "model" {
		t.Fatalf("expected param to be preserved, got %v", sanitized.Error.Param)
	}
	if sanitized.Error.Error == nil {
		t.Fatal("expected non-sensitive nested error to be preserved")
	}
}

func TestSanitizeBifrostErrorForClientPreservesNonSensitiveServerMessage(t *testing.T) {
	statusCode := fasthttp.StatusInternalServerError
	err := &schemas.BifrostError{
		StatusCode: &statusCode,
		Error: &schemas.ErrorField{
			Message: "failed to reload config",
		},
	}

	sanitized := SanitizeBifrostErrorForClient(err)

	if sanitized.Error.Message != "failed to reload config" {
		t.Fatalf("expected non-sensitive server message to be preserved, got %q", sanitized.Error.Message)
	}
}

// A fallback's message is copied onto the primary's error when the chain runs out, so it reaches
// the client through the same response and must pass the same check (#3261).
func TestSanitizeBifrostErrorForClientHidesFallbackAttemptDetails(t *testing.T) {
	statusCode := fasthttp.StatusTooManyRequests
	fallbackStatus := fasthttp.StatusInternalServerError
	refusedStatus := fasthttp.StatusForbidden
	err := &schemas.BifrostError{
		StatusCode: &statusCode,
		Error:      &schemas.ErrorField{Message: "rate limited"},
		ExtraFields: schemas.BifrostErrorExtraFields{
			FallbackAttempts: []schemas.FallbackAttempt{
				{Provider: "prov-b", Model: "m-b", StatusCode: &fallbackStatus, Message: "Traceback (most recent call last):\n  File \"/srv/app.py\", line 12"},
				{Provider: "prov-c", Model: "m-c", StatusCode: &refusedStatus, Type: "provider_blocked", Message: "provider prov-c is not allowed"},
			},
		},
	}

	sanitized := SanitizeBifrostErrorForClient(err)

	attempts := sanitized.ExtraFields.FallbackAttempts
	if len(attempts) != 2 {
		t.Fatalf("expected 2 fallback attempts, got %d", len(attempts))
	}
	if attempts[0].Message != ClientSafeInternalErrorMessage {
		t.Fatalf("expected fallback traceback to be hidden, got %q", attempts[0].Message)
	}
	if attempts[0].StatusCode == nil || *attempts[0].StatusCode != fallbackStatus {
		t.Fatalf("expected fallback status to be preserved, got %v", attempts[0].StatusCode)
	}
	if attempts[1].Message != "provider prov-c is not allowed" {
		t.Fatalf("expected non-sensitive fallback message to be preserved, got %q", attempts[1].Message)
	}
	if err.ExtraFields.FallbackAttempts[0].Message == ClientSafeInternalErrorMessage {
		t.Fatal("expected original fallback attempts to remain unchanged")
	}
}

func providerError(status int, errorType, code, message string) *schemas.BifrostError {
	return &schemas.BifrostError{
		StatusCode:   &status,
		FromProvider: true,
		Error:        &schemas.ErrorField{Type: &errorType, Code: &code, Message: message},
		ExtraFields: schemas.BifrostErrorExtraFields{
			Provider:    schemas.OpenAI,
			RawResponse: map[string]any{"error": message},
			KeyStatuses: []schemas.KeyStatus{{}},
		},
	}
}

// A provider's error about the platform's own account with it never reaches the caller: the
// real case was OpenAI's "You have no credits remaining. Add credits ... at
// https://platform.openai.com/settings/organization/billing/." sent to a customer whose balance
// was fine.
func TestSanitizeHidesProviderAccountErrors(t *testing.T) {
	billing := "You have no credits remaining. Add credits to continue using the API at https://platform.openai.com/settings/organization/billing/."
	cases := []struct {
		name       string
		err        *schemas.BifrostError
		wantStatus int
		wantReason string
	}{
		{"spent quota", providerError(429, "insufficient_quota", "credit_balance_exhausted", billing), 503, "provider_unavailable"},
		{"bad provider key", providerError(401, "invalid_request_error", "invalid_api_key", "Incorrect API key provided: sk-proj-abc. You can find your API key at https://platform.openai.com/account/api-keys."), 503, "provider_unavailable"},
		{"no permission", providerError(403, "permission_error", "", "Your organization must be verified to use this model."), 503, "provider_unavailable"},
		{"payment required", providerError(402, "billing_error", "", "Your credit balance is too low."), 503, "provider_unavailable"},
		{"rate limit", providerError(429, "rate_limit_error", "rate_limit_exceeded", "Rate limit reached for org-abc123 on tokens per min. Visit https://platform.openai.com/account/rate-limits."), 429, "provider_rate_limited"},
		{"provider outage", providerError(500, "server_error", "", "The server had an error. Contact us through help.openai.com with request ID req_123."), 502, "provider_error"},
		{"provider overloaded", providerError(529, "overloaded_error", "", "Overloaded"), 502, "provider_error"},
		{"provider timeout", providerError(408, "", "", "upstream timed out after 60s at 10.0.0.7"), 502, "provider_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := tc.err.Error.Message
			sanitized := SanitizeBifrostErrorForClient(tc.err)

			if got := sanitized.EffectiveHTTPStatus(); got != tc.wantStatus {
				t.Fatalf("status = %d, want %d", got, tc.wantStatus)
			}
			refusal := sanitized.Error.Refusal
			if refusal == nil || refusal.LimitSource != schemas.LimitSourceProvider || refusal.Reason != tc.wantReason {
				t.Fatalf("refusal = %+v, want provider/%s", refusal, tc.wantReason)
			}
			body, err := json.Marshal(sanitized)
			if err != nil {
				t.Fatal(err)
			}
			for _, leak := range []string{"openai.com", "sk-proj", "org-abc123", "req_123", "10.0.0.7", "credits remaining", "insufficient_quota", "credit_balance_exhausted", "raw_response", "key_statuses"} {
				if strings.Contains(string(body), leak) {
					t.Fatalf("the client's body carries %q: %s", leak, body)
				}
			}
			if sanitized.Error.Code != nil || sanitized.ExtraFields.RawResponse != nil || sanitized.ExtraFields.KeyStatuses != nil {
				t.Fatalf("provider fields were kept: %+v", sanitized)
			}
			if tc.err.Error.Message != original || tc.err.ExtraFields.RawResponse == nil {
				t.Fatal("the original error, which the logs keep, was changed")
			}
		})
	}
}

// What a provider says about the request itself is what the developer needs to fix it.
func TestSanitizeKeepsProviderErrorsAboutTheRequest(t *testing.T) {
	for _, status := range []int{400, 404, 413, 422} {
		message := "This model's maximum context length is 128000 tokens."
		sanitized := SanitizeBifrostErrorForClient(providerError(status, "invalid_request_error", "context_length_exceeded", message))

		if sanitized.Error.Message != message || sanitized.Error.Refusal != nil || sanitized.EffectiveHTTPStatus() != status {
			t.Fatalf("status %d: got %d %q, refusal %+v", status, sanitized.EffectiveHTTPStatus(), sanitized.Error.Message, sanitized.Error.Refusal)
		}
	}
}

// A refusal the product made itself, and a plugin's error, are not the provider's answer.
func TestSanitizeLeavesOwnRefusalsAndGatewayErrors(t *testing.T) {
	own := schemas.NewRefusalError(429, "rate_limited", "You are over your rate limit.", schemas.Refusal{
		LimitSource: schemas.LimitSourceRateLimit, Reason: "rate_limited",
	})
	own.ExtraFields.Provider = schemas.OpenAI
	if got := SanitizeBifrostErrorForClient(own); got.Error.Refusal.LimitSource != schemas.LimitSourceRateLimit || got.Error.Message != "You are over your rate limit." {
		t.Fatalf("the product's own refusal was rewritten: %+v", got.Error)
	}

	// a plugin's refusal that is not built as a Refusal, with the provider already named on it
	status := 403
	plugin := &schemas.BifrostError{StatusCode: &status, Error: &schemas.ErrorField{Message: "model is not allowed"}}
	plugin.ExtraFields.Provider = schemas.OpenAI
	if got := SanitizeBifrostErrorForClient(plugin); got.Error.Message != "model is not allowed" || got.Error.Refusal != nil || got.EffectiveHTTPStatus() != 403 {
		t.Fatalf("a plugin's error was rewritten: %+v", got.Error)
	}
}

// A provider's rate limit keeps its number of seconds, never its words.
func TestSanitizePassesOnTheProvidersRetryHint(t *testing.T) {
	err := providerError(429, "rate_limit_error", "", "Rate limit reached. Try again in 20s.")
	err.ExtraFields.RetryAfter = 20_000

	sanitized := SanitizeBifrostErrorForClient(err)

	if got := sanitized.Error.Refusal.RetryAfterSeconds(); got != 20 {
		t.Fatalf("retry after = %d s, want 20", got)
	}
}

// The fallbacks that failed are listed on the final error: their provider messages are hidden too.
func TestSanitizeHidesProviderWordsInFallbackAttempts(t *testing.T) {
	quota, bad := 429, 400
	err := providerError(500, "server_error", "", "boom")
	err.ExtraFields.FallbackAttempts = []schemas.FallbackAttempt{
		{Provider: schemas.Anthropic, Model: "m", StatusCode: &quota, Type: "insufficient_quota", FromProvider: true, Message: "Your credit balance is too low to access the Anthropic API."},
		{Provider: schemas.OpenAI, Model: "n", StatusCode: &bad, Type: "invalid_request_error", FromProvider: true, Message: "max_tokens is too large"},
	}

	attempts := SanitizeBifrostErrorForClient(err).ExtraFields.FallbackAttempts

	if strings.Contains(attempts[0].Message, "Anthropic") || attempts[0].Type != "" {
		t.Fatalf("a fallback's account error was kept: %+v", attempts[0])
	}
	if attempts[1].Message != "max_tokens is too large" {
		t.Fatalf("a fallback's error about the request was changed: %+v", attempts[1])
	}
	if !strings.Contains(err.ExtraFields.FallbackAttempts[0].Message, "Anthropic") {
		t.Fatal("the original attempts, which the logs keep, were changed")
	}
}

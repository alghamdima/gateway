package schemas

import (
	"maps"
	"math"
	"time"
)

// Refusal is what a caller is told, beside the message, when the product itself refused a request
// before any provider was called: which limit refused it, why, and what to do next
// (docs/platform/api-errors.md, SPEC API-08). On the wire it adds two fields to the error object:
// "code", the HTTP status as a number, and "metadata". Clients branch on metadata.limit_source and
// metadata.reason, never on the message text.
type Refusal struct {
	// StatusCode is the HTTP status of the refusal, written as error.code.
	StatusCode int
	// LimitSource names what refused the request; one of the LimitSource* values.
	LimitSource string
	// Reason is the machine-readable cause within the limit source, e.g. "insufficient_balance".
	Reason string
	// RemedyHint is one sentence for a person: what to do next.
	RemedyHint string
	// RetryAfter is how long until the same request can succeed. Zero when retrying it will not
	// help; then no Retry-After header is written.
	RetryAfter time.Duration
	// Details are context fields copied into metadata: amounts, limits, reset times. They must
	// describe the caller's own account only.
	Details map[string]any
}

// The limit sources a refusal can name. Each is documented for customers in
// docs/platform/api-errors.md; a new one is added there in the same change.
const (
	// LimitSourceCredits: the organization's prepaid balance cannot cover the request.
	LimitSourceCredits = "credits"
	// LimitSourceKeyLimit: the API key's own spending limit cannot cover the request.
	LimitSourceKeyLimit = "key_limit"
	// LimitSourceKey: the API key itself is missing, unknown, disabled or expired.
	LimitSourceKey = "key"
	// LimitSourceRateLimit: the organization sent more requests or tokens than its rate allows.
	LimitSourceRateLimit = "rate_limit"
	// LimitSourceDataPolicy: the organization's data policy allows no endpoint for the request.
	LimitSourceDataPolicy = "data_policy"
	// LimitSourceRequestType: the kind of request is not switched on.
	LimitSourceRequestType = "request_type"
	// LimitSourceBudget: a gateway budget on the key or organization is spent.
	LimitSourceBudget = "budget"
	// LimitSourceService: the gateway could not check credits or prices; temporary.
	LimitSourceService = "service"
)

// RetryAfterSeconds is RetryAfter in whole seconds, rounded up; 0 when retrying will not help.
func (r *Refusal) RetryAfterSeconds() int64 {
	if r == nil || r.RetryAfter <= 0 {
		return 0
	}
	return int64(math.Ceil(r.RetryAfter.Seconds()))
}

// Metadata is the error.metadata object: the details, then the fixed fields, which win.
func (r *Refusal) Metadata() map[string]any {
	if r == nil {
		return nil
	}
	metadata := make(map[string]any, len(r.Details)+4)
	maps.Copy(metadata, r.Details)
	metadata["limit_source"] = r.LimitSource
	metadata["reason"] = r.Reason
	metadata["remedy_hint"] = r.RemedyHint
	if seconds := r.RetryAfterSeconds(); seconds > 0 {
		metadata["retry_after_seconds"] = seconds
	}
	return metadata
}

// NewRefusalError builds the error a refusal is returned as. errorType is kept both as the error's
// top-level "type" and as error.type, which OpenAI SDKs read.
func NewRefusalError(statusCode int, errorType, message string, refusal Refusal) *BifrostError {
	refusal.StatusCode = statusCode
	return &BifrostError{
		Type:       &errorType,
		StatusCode: &statusCode,
		Error: &ErrorField{
			Type:    &errorType,
			Message: message,
			Refusal: &refusal,
		},
	}
}

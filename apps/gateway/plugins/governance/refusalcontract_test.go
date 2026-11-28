package governance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The product's error contract (SPEC API-08, docs/platform/api-errors.md) on governance's
// refusals: each carries a limit source, a reason and a hint, and a rate limit says when it resets.

func TestRateLimitRefusalSaysWhenToRetry(t *testing.T) {
	logger := NewMockLogger()
	rateLimit := buildRateLimitWithUsage("rl1", 10000, 10000, 1000, 0) // tokens at the ceiling, a 1m window
	rateLimit.TokenLastReset = time.Now().Add(-20 * time.Second)
	provider := buildProviderWithGovernance("openai", nil, rateLimit)
	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		Providers:  []configstoreTables.TableProvider{*provider},
		RateLimits: []configstoreTables.TableRateLimit{*rateLimit},
	}, nil, nil)
	require.NoError(t, err)
	plugin := &GovernancePlugin{store: store, resolver: NewBudgetResolver(store, nil, logger, nil)}

	_, bifrostErr := plugin.Evaluate(emptyCtx(), &EvaluationRequest{Provider: schemas.OpenAI, Model: "gpt-4"})

	require.NotNil(t, bifrostErr)
	assert.Equal(t, 429, *bifrostErr.StatusCode)
	assert.Equal(t, string(DecisionTokenLimited), *bifrostErr.Type, "the type stays the raw decision")
	refusal := bifrostErr.Error.Refusal
	require.NotNil(t, refusal)
	assert.Equal(t, schemas.LimitSourceRateLimit, refusal.LimitSource)
	assert.Equal(t, "token_limit_reached", refusal.Reason)
	// the window reset 20 s ago of 60: about 40 s left
	assert.InDelta(t, 40, refusal.RetryAfterSeconds(), 2, "Retry-After is the time until the window resets")
	assert.Equal(t, "tokens", refusal.Details["limit_kind"])
	assert.Equal(t, int64(10000), refusal.Details["limit"])
	assert.NotNil(t, refusal.Details["resets_at"])

	body, err := json.Marshal(bifrostErr)
	require.NoError(t, err)
	var decoded struct {
		Error struct {
			Code     int            `json:"code"`
			Message  string         `json:"message"`
			Type     string         `json:"type"`
			Metadata map[string]any `json:"metadata"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &decoded))
	assert.Equal(t, 429, decoded.Error.Code)
	assert.NotEmpty(t, decoded.Error.Message)
	assert.Equal(t, "rate_limit", decoded.Error.Metadata["limit_source"])
	assert.Equal(t, float64(refusal.RetryAfterSeconds()), decoded.Error.Metadata["retry_after_seconds"])
}

func TestRateLimitRetryAfterIsNeverBelowASecond(t *testing.T) {
	expired := &RateLimitError{breach: rateLimitBreach{kind: "requests", resetsAt: time.Now().Add(-time.Minute)}}
	assert.Equal(t, time.Second, expired.RetryAfter(time.Now()))
	unknown := &RateLimitError{breach: rateLimitBreach{kind: "requests"}}
	assert.Equal(t, time.Second, unknown.RetryAfter(time.Now()))
	assert.Nil(t, unknown.Details(time.Now())["resets_at"])
}

func TestEachDecisionHasALimitSource(t *testing.T) {
	for decision, want := range map[Decision][2]string{
		DecisionAccessNotFound:  {schemas.LimitSourceKey, "key_invalid"},
		DecisionProviderBlocked: {schemas.LimitSourceDataPolicy, "provider_not_allowed"},
		DecisionModelBlocked:    {schemas.LimitSourceDataPolicy, "model_not_allowed"},
		DecisionRateLimited:     {schemas.LimitSourceRateLimit, "rate_limited"},
		DecisionRequestLimited:  {schemas.LimitSourceRateLimit, "request_limit_reached"},
		DecisionBudgetExceeded:  {schemas.LimitSourceBudget, "budget_exceeded"},
	} {
		got := defaultRefusal(decision)
		assert.Equal(t, want, [2]string{got.LimitSource, got.Reason}, string(decision))
		assert.NotEmpty(t, got.RemedyHint, string(decision))
	}

	t.Run("a wiring fault carries no refusal: there is nothing for the caller to act on", func(t *testing.T) {
		bifrostErr := refusal(&EvaluationResult{Decision: DecisionAccessUnresolved, Reason: "no grant"}, 500, schemas.ErrorTypeBifrostInternal)
		assert.Nil(t, bifrostErr.Error.Refusal)
	})
}

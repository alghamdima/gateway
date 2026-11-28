package datasheet

import (
	"testing"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The control plane prices through scoped overrides named after its price
// versions (docs/platform/adr/0018-model-price-table.md); the credits plugin
// records the name of the one that priced each attempt.
func TestPricingOverrideName_NamesTheOverrideThatPrices(t *testing.T) {
	s := newTestStore()
	s.pricingData[makeKeyNormalized("gpt-4o-mini", "openai", "chat")] = configstoreTables.TableModelPricing{
		Model:              "gpt-4o-mini",
		Provider:           "openai",
		Mode:               "chat",
		InputCostPerToken:  bifrost.Ptr(1.5e-7),
		OutputCostPerToken: bifrost.Ptr(6e-7),
	}
	provider := "openai"
	key := "key-negotiated"
	text := []schemas.RequestType{schemas.ChatCompletionRequest, schemas.TextCompletionRequest, schemas.ResponsesRequest, schemas.EmbeddingRequest}
	require.NoError(t, s.SetOverrides([]configstoreTables.TablePricingOverride{
		{
			ID: "o-provider", Name: "lemru-price:7", ScopeKind: string(ScopeKindProvider), ProviderID: &provider,
			MatchType: string(MatchTypeExact), Pattern: "gpt-4o-mini", RequestTypes: text,
			PricingPatchJSON: `{"input_cost_per_token":2e-7,"output_cost_per_token":8e-7}`,
		},
		{
			ID: "o-key", Name: "lemru-price:9", ScopeKind: string(ScopeKindProviderKey), ProviderKeyID: &key,
			MatchType: string(MatchTypeExact), Pattern: "gpt-4o-mini", RequestTypes: text,
			// only the input: the key's override does not merge with the provider's
			PricingPatchJSON: `{"input_cost_per_token":1e-7}`,
		},
	}))

	shared := LookupScopes{Provider: "openai"}
	viaKey := LookupScopes{Provider: "openai", SelectedKeyID: key}

	assert.Equal(t, "lemru-price:7", s.PricingOverrideName("gpt-4o-mini", schemas.ChatCompletionRequest, shared))
	assert.Equal(t, "lemru-price:7", s.PricingOverrideName("GPT-4o-Mini", schemas.ChatCompletionStreamRequest, shared))
	assert.Equal(t, "lemru-price:9", s.PricingOverrideName("gpt-4o-mini", schemas.ResponsesRequest, viaKey))
	// a request type the overrides do not list, a model they do not name, another provider
	assert.Equal(t, "", s.PricingOverrideName("gpt-4o-mini", schemas.ImageGenerationRequest, shared))
	assert.Equal(t, "", s.PricingOverrideName("gpt-4o", schemas.ChatCompletionRequest, shared))
	assert.Equal(t, "", s.PricingOverrideName("gpt-4o-mini", schemas.ChatCompletionRequest, LookupScopes{Provider: "azure"}))

	// the name and the price come from the same override
	sharedPrice := s.resolvePricing(schemas.RoutingInfo{Provider: "openai", Model: "gpt-4o-mini"}, schemas.ChatCompletionRequest, shared)
	require.NotNil(t, sharedPrice)
	assert.Equal(t, 2e-7, *sharedPrice.InputCostPerToken)
	assert.Equal(t, 8e-7, *sharedPrice.OutputCostPerToken)
	keyPrice := s.resolvePricing(schemas.RoutingInfo{Provider: "openai", Model: "gpt-4o-mini"}, schemas.ChatCompletionRequest, viaKey)
	require.NotNil(t, keyPrice)
	assert.Equal(t, 1e-7, *keyPrice.InputCostPerToken)
	// the output falls back to the datasheet, not to the provider's override: a key's price must be whole
	assert.Equal(t, 6e-7, *keyPrice.OutputCostPerToken)

	s.customPricing = nil
	assert.Equal(t, "", s.PricingOverrideName("gpt-4o-mini", schemas.ChatCompletionRequest, shared))
}

package governance

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/grant"
	"github.com/stretchr/testify/assert"
)

// mockInMemoryStore is a test double for InMemoryStore.
type mockInMemoryStore struct {
	configuredProviders map[schemas.ModelProvider]configstore.ProviderConfig
}

func (m *mockInMemoryStore) GetConfiguredProviders() map[schemas.ModelProvider]configstore.ProviderConfig {
	return m.configuredProviders
}

func (m *mockInMemoryStore) GetConfiguredProviderNames() []string {
	names := make([]string, 0, len(m.configuredProviders))
	for provider := range m.configuredProviders {
		names = append(names, string(provider))
	}
	return names
}

// A permit that grants every provider names none, so the providers it grants by the flag alone are
// materialised onto it at construction. Every consumer then reads one list and cannot disagree with
// the permit about what it grants — which is what let a listing refuse a provider the request path
// admitted.
func TestAppendAllProviderPermits(t *testing.T) {
	configured := []string{"openai", "anthropic", "bedrock", ""}

	t.Run("materialises the providers no config names", func(t *testing.T) {
		permits := AppendAllProviderPermits([]schemas.ProviderPermit{
			{Provider: "openai", AllowedModels: schemas.WhiteList{"gpt-4o"}, KeyIDs: schemas.WhiteList{"key-1"}},
		}, configured)

		byProvider := map[string]schemas.ProviderPermit{}
		for _, permit := range permits {
			byProvider[permit.Provider] = permit
		}
		if len(byProvider) != 3 {
			t.Fatalf("expected openai, anthropic and bedrock, got %v", permits)
		}
		// The empty name the deployment reported is not a provider and must not become a permit.
		if _, ok := byProvider[""]; ok {
			t.Error("an empty provider name must not be materialised")
		}
		// A provider the permit already named keeps its own rules: the flag widens the set, it does
		// not relax the overrides.
		if got := byProvider["openai"].AllowedModels; len(got) != 1 || got[0] != "gpt-4o" {
			t.Errorf("openai override was replaced: %v", got)
		}
		// One it did not name is granted with nothing narrowed, and no weight, since a weight is a
		// routing preference a provider config expresses and this one expresses none.
		anthropic := byProvider["anthropic"]
		if !anthropic.AllowedModels.IsUnrestricted() {
			t.Errorf("expected all models allowed, got %v", anthropic.AllowedModels)
		}
		if !schemas.WhiteList(anthropic.KeyIDs).IsUnrestricted() {
			t.Errorf("expected all keys allowed, got %v", anthropic.KeyIDs)
		}
		if len(anthropic.BlacklistedModels) != 0 {
			t.Errorf("expected nothing blocked, got %v", anthropic.BlacklistedModels)
		}
		if anthropic.Weight != nil {
			t.Errorf("expected no weight, got %v", *anthropic.Weight)
		}
	})

	t.Run("a deployment with no providers adds nothing", func(t *testing.T) {
		permits := AppendAllProviderPermits(nil, nil)
		if len(permits) != 0 {
			t.Fatalf("expected no permits, got %v", permits)
		}
	})
}

// The end the fix is for: a key that grants every provider must answer for one it holds no config
// for, in the same list every consumer reads. Asked through the access, because that is what the
// listing routes and the routing allowlist ask.
func TestPermitForVirtualKeyGrantsEveryConfiguredProvider(t *testing.T) {
	store := &LocalGovernanceStore{inMemoryStore: &mockInMemoryStore{
		configuredProviders: map[schemas.ModelProvider]configstore.ProviderConfig{
			schemas.OpenAI: {}, schemas.Anthropic: {}, schemas.Bedrock: {},
		},
	}}
	// The customer's shape: allow-all, plus one provider config carrying an override.
	vk := &configstoreTables.TableVirtualKey{
		ID: "vk-1", Name: "allow-all", AllowAllProviders: true,
		ProviderConfigs: []configstoreTables.TableVirtualKeyProviderConfig{{
			Provider: string(schemas.Bedrock), AllowedModels: schemas.WhiteList{"anthropic.claude-haiku-4-5"}, AllowAllKeys: true,
		}},
	}
	access := grant.NewAccess([]schemas.Permit{store.permitForVirtualKey(emptyCtx(), vk)}, "", nil)

	assert.ElementsMatch(t, []string{"openai", "anthropic", "bedrock"}, access.GrantedProvidersForModel(""),
		"a provider the key names no config for is still granted")
	// The override survives: the flag widens the provider set, it does not relax what a config says.
	assert.True(t, access.IsModelAllowed("bedrock", "anthropic.claude-haiku-4-5"))
	assert.False(t, access.IsModelAllowed("bedrock", "amazon.titan-embed-text-v2:0"))
	// And a provider granted by the flag alone narrows nothing.
	assert.True(t, access.IsModelAllowed("anthropic", "claude-haiku-4-5"))

	t.Run("without the flag only the named providers are granted", func(t *testing.T) {
		vk.AllowAllProviders = false
		access := grant.NewAccess([]schemas.Permit{store.permitForVirtualKey(emptyCtx(), vk)}, "", nil)

		assert.Equal(t, []string{"bedrock"}, access.GrantedProvidersForModel(""))
		assert.False(t, access.IsModelAllowed("anthropic", "claude-haiku-4-5"))
	})
}

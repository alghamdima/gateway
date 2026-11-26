package configstore

import (
	"slices"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

// TestGenerateKeyHash_AnthropicWorkloadIdentityHashesByReference pins that a rotated env-backed
// identity token, which short-lived OIDC tokens always are, leaves an unchanged config.json key's
// hash alone, while a real edit to the block still moves it.
func TestGenerateKeyHash_AnthropicWorkloadIdentityHashesByReference(t *testing.T) {
	build := func(token, workspace string) schemas.Key {
		wif := &schemas.AnthropicWorkloadIdentityConfig{
			FederationRuleID: *schemas.NewSecretVar("fdrl_01abc"),
			OrganizationID:   *schemas.NewSecretVar("org-uuid"),
			ServiceAccountID: *schemas.NewSecretVar("svac_01def"),
			IdentitySource:   &schemas.IdentitySource{Type: schemas.IdentitySourceInline, Token: schemas.NewSecretVar(token)},
		}
		if workspace != "" {
			wif.WorkspaceID = schemas.NewSecretVar(workspace)
		}
		return schemas.Key{ID: "k1", Name: "anthropic-wif", AnthropicKeyConfig: &schemas.AnthropicKeyConfig{WorkloadIdentity: wif}}
	}
	t.Setenv("HASH_TEST_ANTHROPIC_IDENTITY_TOKEN", "jwt-v1")
	before, err := GenerateKeyHash(build("env.HASH_TEST_ANTHROPIC_IDENTITY_TOKEN", ""))
	require.NoError(t, err)
	t.Setenv("HASH_TEST_ANTHROPIC_IDENTITY_TOKEN", "jwt-v2")
	after, err := GenerateKeyHash(build("env.HASH_TEST_ANTHROPIC_IDENTITY_TOKEN", ""))
	require.NoError(t, err)
	require.Equal(t, before, after, "rotating the env-backed identity token changed the hash of an unchanged file")

	withWorkspace, err := GenerateKeyHash(build("env.HASH_TEST_ANTHROPIC_IDENTITY_TOKEN", "wrkspc_01ghi"))
	require.NoError(t, err)
	require.NotEqual(t, before, withWorkspace, "adding a workspace is an edit")

	literal, err := GenerateKeyHash(build("jwt-v2", ""))
	require.NoError(t, err)
	require.NotEqual(t, after, literal, "switching between a reference and a literal is an edit")

	plain, err := GenerateKeyHash(schemas.Key{ID: "k1", Name: "anthropic-wif"})
	require.NoError(t, err)
	require.NotEqual(t, plain, before, "adding the block is an edit")
}

// TestGenerateKeyHash_AliasesIsOrderIndependent pins the same property for aliases, which is
// also a map: a key whose only change is the encoder's walk order must not be treated as edited.
func TestGenerateKeyHash_AliasesIsOrderIndependent(t *testing.T) {
	build := func(order []string) schemas.Key {
		aliases := schemas.KeyAliases{}
		for _, name := range order {
			aliases[name] = schemas.AliasConfig{ModelID: "upstream-" + name}
		}
		return schemas.Key{ID: "k1", Name: "aliased", Value: *schemas.NewSecretVar("sk"), Aliases: aliases}
	}
	want, err := GenerateKeyHash(build([]string{"a", "b", "c", "d", "e", "f"}))
	require.NoError(t, err)
	for i := 0; i < 40; i++ {
		got, err := GenerateKeyHash(build([]string{"f", "e", "d", "c", "b", "a"}))
		require.NoError(t, err)
		require.Equal(t, want, got, "iteration %d: the hash must not depend on map iteration order", i)
	}
}

// TestLegacyAliasOrderKeyHash_ReproducesTheUnsortedEncoding pins that the legacy hash is
// rebuilt from the exact bytes sonic.Marshal used to feed the hasher before aliases were
// sorted: for two aliases sonic emits one of two orders, and one of them must be ours.
func TestLegacyAliasOrderKeyHash_ReproducesTheUnsortedEncoding(t *testing.T) {
	canonical := "gpt-4o"
	key := schemas.Key{ID: "k1", Name: "aliased", Value: *schemas.NewSecretVar("sk"), Weight: 1, Aliases: schemas.KeyAliases{
		"plain": {ModelID: "upstream-plain"},
		"rich":  {ModelID: "upstream-rich", ModelName: &canonical},
	}}
	unsorted, err := sonic.Marshal(key.Aliases)
	require.NoError(t, err)
	matched := false
	for _, order := range [][]string{{"plain", "rich"}, {"rich", "plain"}} {
		legacy, err := legacyAliasBytes(key.Aliases, order)
		require.NoError(t, err)
		if string(legacy) == string(unsorted) {
			matched = true
		}
	}
	require.True(t, matched, "no legacy ordering reproduces sonic's unsorted bytes %s", unsorted)
}

// TestKeyHashMatchesLegacyAliasOrder pins the upgrade path for keys stored before alias
// hashing was sorted: a config_hash produced under any alias order is recognised as this
// unchanged key, another key's hash is not, a single alias needs no search (both encodings
// coincide), and the search is bounded.
func TestKeyHashMatchesLegacyAliasOrder(t *testing.T) {
	build := func(names ...string) schemas.Key {
		aliases := schemas.KeyAliases{}
		for _, name := range names {
			aliases[name] = schemas.AliasConfig{ModelID: "upstream-" + name}
		}
		return schemas.Key{ID: "k1", Name: "aliased", Value: *schemas.NewSecretVar("sk"), Weight: 1, Aliases: aliases}
	}
	key := build("a", "b", "c", "d")
	sorted, err := GenerateKeyHash(key)
	require.NoError(t, err)

	identity, err := LegacyAliasOrderKeyHash(key, []string{"a", "b", "c", "d"})
	require.NoError(t, err)
	require.Equal(t, sorted, identity, "the sorted order is one of the legacy orders")

	for _, order := range [][]string{{"d", "c", "b", "a"}, {"b", "d", "a", "c"}} {
		legacy, err := LegacyAliasOrderKeyHash(key, order)
		require.NoError(t, err)
		require.NotEqual(t, sorted, legacy, "order %v must hash differently from the sorted form", order)
		ok, err := KeyHashMatchesLegacyAliasOrder(key, legacy)
		require.NoError(t, err)
		require.True(t, ok, "legacy hash for order %v must be recognised", order)
	}

	other, err := GenerateKeyHash(build("a", "b", "c", "x"))
	require.NoError(t, err)
	ok, err := KeyHashMatchesLegacyAliasOrder(key, other)
	require.NoError(t, err)
	require.False(t, ok, "another key's hash must not match")

	edited := key
	edited.Weight = 0.5
	editedLegacy, err := LegacyAliasOrderKeyHash(edited, []string{"d", "c", "b", "a"})
	require.NoError(t, err)
	ok, err = KeyHashMatchesLegacyAliasOrder(key, editedLegacy)
	require.NoError(t, err)
	require.False(t, ok, "a real file edit must still be detected under every legacy order")

	ok, err = KeyHashMatchesLegacyAliasOrder(build("a"), "not-a-hash")
	require.NoError(t, err)
	require.False(t, ok, "a single alias has one encoding, so a mismatch is a real change")

	many := build("a", "b", "c", "d", "e", "f", "g", "h", "i", "j")
	manyLegacy, err := LegacyAliasOrderKeyHash(many, []string{"j", "i", "h", "g", "f", "e", "d", "c", "b", "a"})
	require.NoError(t, err)
	ok, err = KeyHashMatchesLegacyAliasOrder(many, manyLegacy)
	require.NoError(t, err)
	require.False(t, ok, "above maxLegacyMapPermutations the search is skipped and the file wins")
}

// TestGenerateConfigHash_SortedOverridesMarker pins which providers carry the sorted-overrides
// marker: only custom providers with two or more overrides, whose encoding changed. Every
// other provider keeps the hash older builds stored, so upgrading does not look like an edit.
func TestGenerateConfigHash_SortedOverridesMarker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    *ProviderConfig
		marked bool
	}{
		{"not custom", &ProviderConfig{}, false},
		{"no overrides", customProviderWithOverrides(), false},
		{"one override", customProviderWithOverrides("a"), false},
		{"two overrides", customProviderWithOverrides("a", "b"), true},
		{"above the search bound", customProviderWithOverrides("a", "b", "c", "d", "e", "f", "g", "h"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := tc.cfg.GenerateConfigHash("custom")
			require.NoError(t, err)
			require.Equal(t, tc.marked, strings.HasSuffix(h, SortedOverridesConfigHashMarker))
			if !tc.marked && tc.cfg.CustomProviderConfig != nil {
				legacy, err := tc.cfg.LegacyOverrideOrderConfigHash("custom", sortedOverrideNames(tc.cfg))
				require.NoError(t, err)
				require.Equal(t, legacy, h, "a provider whose encoding did not change keeps its stored hash")
			}
		})
	}
}

func sortedOverrideNames(p *ProviderConfig) []schemas.RequestType {
	names := make([]schemas.RequestType, 0, len(p.CustomProviderConfig.RequestPathOverrides))
	for name := range p.CustomProviderConfig.RequestPathOverrides {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// customProviderWithOverrides builds a custom provider config whose request_path_overrides
// holds one entry per named request type.
func customProviderWithOverrides(names ...schemas.RequestType) *ProviderConfig {
	overrides := map[schemas.RequestType]string{}
	for _, name := range names {
		overrides[name] = "/custom/" + string(name)
	}
	return &ProviderConfig{CustomProviderConfig: &schemas.CustomProviderConfig{
		BaseProviderType:     schemas.OpenAI,
		AllowedRequests:      &schemas.AllowedRequests{ChatCompletion: true},
		RequestPathOverrides: overrides,
	}}
}

// TestGenerateConfigHash_RequestPathOverridesOrderIndependent pins that the provider hash
// does not depend on map iteration order: an unchanged custom provider with several
// overrides must hash the same on every call, or reconciliation sees a file edit.
func TestGenerateConfigHash_RequestPathOverridesOrderIndependent(t *testing.T) {
	p := customProviderWithOverrides("a", "b", "c", "d", "e", "f")
	want, err := p.GenerateConfigHash("custom")
	require.NoError(t, err)
	for i := 0; i < 40; i++ {
		got, err := p.GenerateConfigHash("custom")
		require.NoError(t, err)
		require.Equal(t, want, got, "iteration %d: the hash must not depend on map iteration order", i)
	}
}

// TestLegacyOverrideOrderConfigHash_ReproducesTheUnsortedEncoding pins that the legacy hash
// is rebuilt from the exact bytes sonic.Marshal used to feed the hasher before overrides were
// sorted: for two overrides sonic emits one of two orders, and one of them must be ours.
func TestLegacyOverrideOrderConfigHash_ReproducesTheUnsortedEncoding(t *testing.T) {
	cfg := customProviderWithOverrides("chat_completion", "embedding").CustomProviderConfig
	unsorted, err := sonic.Marshal(cfg)
	require.NoError(t, err)
	matched := false
	for _, order := range [][]schemas.RequestType{{"chat_completion", "embedding"}, {"embedding", "chat_completion"}} {
		legacy, err := legacyCustomProviderConfigBytes(cfg, order)
		require.NoError(t, err)
		if string(legacy) == string(unsorted) {
			matched = true
		}
	}
	require.True(t, matched, "no legacy ordering reproduces sonic's unsorted bytes %s", unsorted)
}

// TestConfigHashMatchesLegacyOverrideOrder pins the upgrade path for custom providers stored
// before request_path_overrides hashing was sorted, mirroring the alias one: a config_hash
// produced under any override order is recognised as this unchanged provider, a real edit is
// not, a single override needs no search, and the search is bounded.
func TestConfigHashMatchesLegacyOverrideOrder(t *testing.T) {
	p := customProviderWithOverrides("a", "b", "c", "d")
	sorted, err := p.GenerateConfigHash("custom")
	require.NoError(t, err)

	identity, err := p.LegacyOverrideOrderConfigHash("custom", []schemas.RequestType{"a", "b", "c", "d"})
	require.NoError(t, err)
	require.Equal(t, identity+SortedOverridesConfigHashMarker, sorted, "the sorted order is one of the legacy orders; the sorted hash only adds the marker")

	for _, order := range [][]schemas.RequestType{{"d", "c", "b", "a"}, {"b", "d", "a", "c"}} {
		legacy, err := p.LegacyOverrideOrderConfigHash("custom", order)
		require.NoError(t, err)
		require.NotEqual(t, sorted, legacy, "order %v must hash differently from the sorted form", order)
		ok, err := p.ConfigHashMatchesLegacyOverrideOrder("custom", legacy)
		require.NoError(t, err)
		require.True(t, ok, "legacy hash for order %v must be recognised", order)
	}

	ok, err := p.ConfigHashMatchesLegacyOverrideOrder("custom", mustLegacyConfigHash(t, p, "other", "d", "c", "b", "a"))
	require.NoError(t, err)
	require.False(t, ok, "another provider's hash must not match")

	edited := customProviderWithOverrides("a", "b", "c", "d")
	edited.CustomProviderConfig.IsKeyLess = true
	ok, err = p.ConfigHashMatchesLegacyOverrideOrder("custom", mustLegacyConfigHash(t, edited, "custom", "d", "c", "b", "a"))
	require.NoError(t, err)
	require.False(t, ok, "a real file edit must still be detected under every legacy order")

	ok, err = customProviderWithOverrides("a").ConfigHashMatchesLegacyOverrideOrder("custom", "not-a-hash")
	require.NoError(t, err)
	require.False(t, ok, "a single override has one encoding, so a mismatch is a real change")

	ok, err = (&ProviderConfig{}).ConfigHashMatchesLegacyOverrideOrder("custom", "not-a-hash")
	require.NoError(t, err)
	require.False(t, ok, "a provider that is not custom has nothing to search")

	many := customProviderWithOverrides("a", "b", "c", "d", "e", "f", "g", "h")
	ok, err = many.ConfigHashMatchesLegacyOverrideOrder("custom", mustLegacyConfigHash(t, many, "custom", "h", "g", "f", "e", "d", "c", "b", "a"))
	require.NoError(t, err)
	require.False(t, ok, "above maxLegacyMapPermutations the search is skipped and the file wins")

	ok, err = p.ConfigHashMatchesLegacyOverrideOrder("custom", "deadbeef"+SortedOverridesConfigHashMarker)
	require.NoError(t, err)
	require.False(t, ok, "a stored hash carrying the sorted marker is never a legacy hash")
}

// TestConfigHashLegacyOverrideOrderUnverifiable pins when reconciliation must stop trusting a
// hash mismatch: only for a stored hash on a custom provider with more overrides than the
// legacy search covers. Within the bound the search decides, and a provider without overrides
// never had an unsorted encoding.
func TestConfigHashLegacyOverrideOrderUnverifiable(t *testing.T) {
	names := func(n int) []schemas.RequestType {
		out := make([]schemas.RequestType, n)
		for i := range out {
			out[i] = schemas.RequestType(string(rune('a' + i)))
		}
		return out
	}
	require.False(t, customProviderWithOverrides(names(maxLegacyMapPermutations)...).ConfigHashLegacyOverrideOrderUnverifiable("stored"), "the search covers up to the bound")
	require.True(t, customProviderWithOverrides(names(maxLegacyMapPermutations+1)...).ConfigHashLegacyOverrideOrderUnverifiable("stored"), "above the bound the stored hash cannot be verified")
	require.False(t, customProviderWithOverrides(names(maxLegacyMapPermutations+1)...).ConfigHashLegacyOverrideOrderUnverifiable(""), "a provider with no stored hash has nothing to verify")
	require.False(t, customProviderWithOverrides(names(maxLegacyMapPermutations+1)...).ConfigHashLegacyOverrideOrderUnverifiable("stored"+SortedOverridesConfigHashMarker), "a sorted-era hash is verified by direct compare, so a mismatch is a real edit")
	require.False(t, (&ProviderConfig{}).ConfigHashLegacyOverrideOrderUnverifiable("stored"), "a provider that is not custom has no overrides")
}

func mustLegacyConfigHash(t *testing.T, p *ProviderConfig, providerName string, order ...schemas.RequestType) string {
	t.Helper()
	h, err := p.LegacyOverrideOrderConfigHash(providerName, order)
	require.NoError(t, err)
	return h
}

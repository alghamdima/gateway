package grant

import (
	"fmt"
	"slices"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/modelcatalog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Access is a request's resolved access: the permits its caller holds and the levels they sit at.
// What is covered here is the fold: every question a consumer can ask of a request, and how the
// caller's permits and levels combine to answer it.

// Permit kinds for these tests. The access engine reads a permit's level, not its kind, so the kinds
// only label where a permit stands in a case: a caller's own, or an organization level above the
// caller.
const (
	testCallerPermit   PermitType = "test_caller"
	testTeamPermit     PermitType = "test_team"
	testUnitPermit     PermitType = "test_unit"
	testCustomerPermit PermitType = "test_customer"
)

func TestAccess_DegenerateCases(t *testing.T) {
	basePermit := permitWithProviders(PermitVirtualKey, "vk1", "Caller Key", "openai")

	tests := []struct {
		name          string
		base          *Permit
		levelMode     CompositionMode
		wantOpenAI    bool
		wantAnthropic bool
	}{
		{
			name:          "one permit, nothing above it: the level mode is irrelevant",
			base:          basePermit,
			levelMode:     Intersect,
			wantOpenAI:    true,
			wantAnthropic: false,
		},
		{
			name:          "one permit, union",
			base:          basePermit,
			levelMode:     Union,
			wantOpenAI:    true,
			wantAnthropic: false,
		},
		{
			name:          "no permit: nothing is permitted",
			wantOpenAI:    false,
			wantAnthropic: false,
		},
		{
			name:          "no permit, intersect: nothing is permitted",
			levelMode:     Intersect,
			wantOpenAI:    false,
			wantAnthropic: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			access := NewAccess(held(tt.base), tt.levelMode, nil)

			assert.Equal(t, tt.wantOpenAI, access.IsProviderAllowed("openai"), "openai provider")
			assert.Equal(t, tt.wantOpenAI, access.IsModelAllowed("openai", "gpt-4o"), "openai model")
			assert.Equal(t, tt.wantAnthropic, access.IsProviderAllowed("anthropic"), "anthropic provider")
			assert.Equal(t, tt.wantAnthropic, access.IsModelAllowed("anthropic", "claude-sonnet-4"), "anthropic model")
		})
	}
}

func TestAccess_NilReceiverPermitsNothing(t *testing.T) {
	var access *Access

	assert.False(t, access.IsProviderAllowed("openai"))
	assert.False(t, access.IsModelAllowed("openai", "gpt-4o"))
	assert.False(t, access.AllowsEveryModel("openai"))
	assert.False(t, access.RestrictsKeys("openai"))
	assert.Nil(t, access.Bases())
	assert.Equal(t, "", access.LevelMode())
	assert.Empty(t, access.GrantedProvidersForModel("gpt-4o"))
	assert.Empty(t, access.ProvidersForModel("gpt-4o"))
	assert.Nil(t, access.PermitsForModel("openai", "gpt-4o"))
	assert.Nil(t, access.DeniedPermitsForModel("openai", "gpt-4o"))

	keyIDs, restricted := access.KeysForModel("openai", "gpt-4o")
	assert.Nil(t, keyIDs)
	assert.False(t, restricted)
}

// Holding no permit is not holding a permit that permits nothing, and the two must stay
// distinguishable: consumers branch on whether a request has access resolved at all.
func TestAccess_NoPermitIsNotAnEmptyPermit(t *testing.T) {
	empty := newPermit(permitSpec{Type: PermitVirtualKey, ID: "vk1", Name: "Empty Key"})

	withEmptyPermit := NewAccess(held(empty), "", nil)
	assert.NotEmpty(t, withEmptyPermit.Bases(), "the caller holds a permit, which happens to permit nothing")
	assert.False(t, withEmptyPermit.IsProviderAllowed("openai"))
	denied := withEmptyPermit.DeniedPermitsForModel("openai", "")
	require.Len(t, denied, 1)
	assert.Equal(t, "Empty Key", denied[0].Name())

	withNoPermit := NewAccess(nil, "", nil)
	assert.Empty(t, withNoPermit.Bases(), "the caller holds nothing at all")
	assert.False(t, withNoPermit.IsProviderAllowed("openai"))
	assert.Nil(t, withNoPermit.DeniedPermitsForModel("openai", ""), "there is no permit to name")
}

// The two level modes on the same permits: under Union every permit adds, under Intersect the level
// above the caller's caps it and grants nothing of its own.
func TestAccess_ProviderFold(t *testing.T) {
	base := permitWithProviders(testCallerPermit, "ap1", "Caller Key", "openai", "bedrock")
	team := atLevel(permitWithProviders(testTeamPermit, "t1", "Team", "openai", "anthropic"), 1)

	unionAccess := NewAccess(held(base, team), Union, nil)
	intersectAccess := NewAccess(held(base, team), Intersect, nil)

	// Held by both.
	assert.True(t, unionAccess.IsProviderAllowed("openai"))
	assert.True(t, intersectAccess.IsProviderAllowed("openai"))

	// The caller's own only.
	assert.True(t, unionAccess.IsProviderAllowed("bedrock"))
	assert.False(t, intersectAccess.IsProviderAllowed("bedrock"))

	// The level above only.
	assert.True(t, unionAccess.IsProviderAllowed("anthropic"))
	assert.False(t, intersectAccess.IsProviderAllowed("anthropic"))

	// Neither.
	assert.False(t, unionAccess.IsProviderAllowed("cohere"))
	assert.False(t, intersectAccess.IsProviderAllowed("cohere"))
}

func TestAccess_ModelFold(t *testing.T) {
	base := permitWithModels(testCallerPermit, "ap1", "Caller Key", "openai", "gpt-4o", "gpt-4o-mini")
	team := atLevel(permitWithModels(testTeamPermit, "t1", "Team", "openai", "gpt-4o", "o3"), 1)

	unionAccess := NewAccess(held(base, team), Union, nil)
	intersectAccess := NewAccess(held(base, team), Intersect, nil)

	assert.True(t, unionAccess.IsModelAllowed("openai", "gpt-4o"))
	assert.True(t, intersectAccess.IsModelAllowed("openai", "gpt-4o"))

	assert.True(t, unionAccess.IsModelAllowed("openai", "gpt-4o-mini"))
	assert.False(t, intersectAccess.IsModelAllowed("openai", "gpt-4o-mini"))

	assert.True(t, unionAccess.IsModelAllowed("openai", "o3"))
	assert.False(t, intersectAccess.IsModelAllowed("openai", "o3"))

	assert.False(t, unionAccess.IsModelAllowed("openai", "gpt-3.5-turbo"))
	assert.False(t, intersectAccess.IsModelAllowed("openai", "gpt-3.5-turbo"))
}

// An empty model asks about the provider alone, which is what lets a consumer gate on the
// provider before it knows the model.
func TestAccess_EmptyModelAsksAboutTheProvider(t *testing.T) {
	base := permitWithModels(PermitVirtualKey, "vk1", "Caller Key", "openai", "gpt-4o")
	access := NewAccess(held(base), "", nil)

	assert.True(t, access.IsModelAllowed("openai", ""))
	assert.False(t, access.IsModelAllowed("anthropic", ""))
	assertPermitsAre(t, access.PermitsForModel("openai", ""), base)
	assert.Nil(t, access.PermitsForModel("anthropic", ""))
}

// Within one permit, several provider permits for a provider are still an any-of: the multiplicity
// that went away was across sources, not inside one.
func TestAccess_ProviderPermitsWithinAPermitAreAnyOf(t *testing.T) {
	base := newPermit(permitSpec{
		Type: PermitVirtualKey, ID: "vk1", Name: "Caller Key",
		ProviderPermits: []schemas.ProviderPermit{
			{Provider: "openai", AllowedModels: []string{"gpt-4o"}},
			{Provider: "openai", AllowedModels: []string{"o3"}},
		},
	})
	access := NewAccess(held(base), "", nil)

	assert.True(t, access.IsModelAllowed("openai", "gpt-4o"))
	assert.True(t, access.IsModelAllowed("openai", "o3"))
	assert.False(t, access.IsModelAllowed("openai", "gpt-4o-mini"))
}

func TestAccess_BlacklistWins(t *testing.T) {
	base := newPermit(permitSpec{
		Type: PermitVirtualKey, ID: "vk1", Name: "Caller Key",
		ProviderPermits: []schemas.ProviderPermit{
			{Provider: "openai", AllowedModels: []string{"*"}, BlacklistedModels: []string{"gpt-4o"}},
			// A second, permissive provider permit for the same provider must not reopen it.
			{Provider: "openai", AllowedModels: []string{"gpt-4o"}},
		},
	})
	access := NewAccess(held(base), "", nil)

	assert.True(t, access.IsProviderAllowed("openai"), "the provider itself stays permitted")
	assert.False(t, access.IsModelAllowed("openai", "gpt-4o"), "blacklisted in one provider permit blocks the provider for that model")
	assert.True(t, access.IsModelAllowed("openai", "o3"))
}

// A permit that allows all providers grants even a provider it holds no provider permit for, with
// every model and every key. A provider it does list keeps that permit's own rules, so allow-all
// coexists with per-provider restrictions rather than overriding them.
func TestAccess_AllowAllProviders(t *testing.T) {
	// Lists openai with a blacklist and a key restriction; allows every other provider by the flag.
	base := NewPermit(PermitVirtualKey, "vk1", "Caller Key", true, false,
		[]schemas.ProviderPermit{
			{Provider: "openai", AllowedModels: []string{"*"}, BlacklistedModels: []string{"gpt-4o"}, KeyIDs: []string{"key-a"}},
			// Restrictive, non-wildcard allowlist: allow-all must not open models it omits.
			{Provider: "anthropic", AllowedModels: []string{"claude-3-5-sonnet"}},
		}, WithAllowAllProviders(true))
	access := NewAccess(held(base), "", nil)

	// A provider with no permit is allowed for any model, with no key restriction.
	assert.True(t, access.IsProviderAllowed("cohere"), "allow-all grants an unlisted provider")
	assert.True(t, access.IsModelAllowed("cohere", "command-r-plus"), "allow-all grants any model of an unlisted provider")
	assert.True(t, access.IsModelAllowed("cohere", ""), "an empty model asks about the provider alone")
	keyIDs, restricted := access.KeysForModel("cohere", "command-r-plus")
	assert.False(t, restricted, "an unlisted provider under allow-all has no key restriction")
	assert.Nil(t, keyIDs)

	// A listed provider keeps its own rules: the blacklist still wins, and its key restriction stands.
	assert.True(t, access.IsProviderAllowed("openai"))
	assert.False(t, access.IsModelAllowed("openai", "gpt-4o"), "a listed provider's blacklist wins even under allow-all")
	assert.True(t, access.IsModelAllowed("openai", "o3"))
	keyIDs, restricted = access.KeysForModel("openai", "o3")
	assert.True(t, restricted, "a listed provider keeps its key restriction under allow-all")
	assert.Equal(t, []string{"key-a"}, keyIDs)

	// A listed provider's restrictive allowlist wins: allow-all does not grant a model it omits.
	assert.True(t, access.IsProviderAllowed("anthropic"))
	assert.True(t, access.IsModelAllowed("anthropic", "claude-3-5-sonnet"), "a model in the allowlist is allowed")
	assert.False(t, access.IsModelAllowed("anthropic", "claude-3-opus"), "allow-all must not grant a model outside a listed provider's allowlist")
}

// AllowsEveryModel is the question for a request that will run models it does not name: it is true
// only when every permit deciding for the provider allows all models and blacklists none, so a
// false answer means a model restriction exists that only a named model lets the access apply.
func TestAccess_AllowsEveryModel(t *testing.T) {
	wildcard := newPermit(permitSpec{
		Type: PermitVirtualKey, ID: "vk1", Name: "Caller Key",
		ProviderPermits: []schemas.ProviderPermit{{Provider: "openai", AllowedModels: []string{"*"}}},
	})
	restricted := newPermit(permitSpec{
		Type: PermitVirtualKey, ID: "vk2", Name: "Restricted Key",
		ProviderPermits: []schemas.ProviderPermit{{Provider: "openai", AllowedModels: []string{"gpt-4"}}},
	})
	blacklisting := newPermit(permitSpec{
		Type: PermitVirtualKey, ID: "vk3", Name: "Blacklisting Key",
		ProviderPermits: []schemas.ProviderPermit{{Provider: "openai", AllowedModels: []string{"*"}, BlacklistedModels: []string{"gpt-4o"}}},
	})
	allowAll := NewPermit(PermitVirtualKey, "vk4", "Allow All", true, false,
		[]schemas.ProviderPermit{{Provider: "anthropic", AllowedModels: []string{"claude-sonnet-4"}}}, WithAllowAllProviders(true))

	assert.True(t, NewAccess(held(wildcard), "", nil).AllowsEveryModel("openai"))
	assert.False(t, NewAccess(held(wildcard), "", nil).AllowsEveryModel("anthropic"), "an unlisted provider is not granted at all")
	assert.False(t, NewAccess(held(restricted), "", nil).AllowsEveryModel("openai"), "an allowlist is a restriction")
	assert.False(t, NewAccess(held(blacklisting), "", nil).AllowsEveryModel("openai"), "a blacklist is a restriction even under a wildcard allowlist")
	assert.True(t, NewAccess(held(allowAll), "", nil).AllowsEveryModel("cohere"), "allow-all grants every model of an unlisted provider")
	assert.False(t, NewAccess(held(allowAll), "", nil).AllowsEveryModel("anthropic"), "a listed provider keeps its own allowlist under allow-all")

	// Several caller permits are read as one: any of them allowing every model is enough.
	assert.True(t, NewAccess(held(restricted, wildcard), "", nil).AllowsEveryModel("openai"))

	// Within one permit, duplicate provider permits are a union too, as for IsModelAllowed: an
	// unrestricted entry beside a restricted one still permits every model, unless any entry for
	// the provider blacklists a model, which is decisive.
	mixed := newPermit(permitSpec{
		Type: PermitVirtualKey, ID: "vk5", Name: "Mixed Key",
		ProviderPermits: []schemas.ProviderPermit{
			{Provider: "openai", AllowedModels: []string{"gpt-4"}},
			{Provider: "openai", AllowedModels: []string{"*"}},
		},
	})
	mixedWithBlacklist := newPermit(permitSpec{
		Type: PermitVirtualKey, ID: "vk6", Name: "Mixed Blacklisting Key",
		ProviderPermits: []schemas.ProviderPermit{
			{Provider: "openai", AllowedModels: []string{"*"}},
			{Provider: "openai", AllowedModels: []string{"gpt-4"}, BlacklistedModels: []string{"gpt-4o"}},
		},
	})
	assert.True(t, NewAccess(held(mixed), "", nil).IsModelAllowed("openai", "o3"), "the union already permits every model")
	assert.True(t, NewAccess(held(mixed), "", nil).AllowsEveryModel("openai"), "an unrestricted entry beside a restricted one permits every model")
	assert.False(t, NewAccess(held(mixedWithBlacklist), "", nil).AllowsEveryModel("openai"), "a blacklist on any entry for the provider is decisive")
}

// Without the flag, a provider the permit does not list is denied: allow-all is opt-in.
func TestAccess_WithoutAllowAllProvidersUnlistedIsDenied(t *testing.T) {
	base := permitWithProviders(PermitVirtualKey, "vk1", "Caller Key", "openai")
	access := NewAccess(held(base), "", nil)

	assert.False(t, access.IsProviderAllowed("cohere"))
	assert.False(t, access.IsModelAllowed("cohere", "command-r-plus"))
}

func TestAccess_ModelNamesResolveThroughTheMatcher(t *testing.T) {
	// The matcher is what makes "*" mean "every model this provider actually serves" rather than
	// "every string", so the same permit answers differently with and without one. Without a
	// matcher, entries are matched by name.
	unrestricted := permitWithModels(PermitVirtualKey, "vk1", "Caller Key", "openai", "*")
	catalog := modelcatalog.NewTestCatalog(nil)
	matcher := catalogMatcher(catalog, &fakeProviderConfigSource{})

	byName := NewAccess(held(unrestricted), "", nil)
	assert.True(t, byName.IsModelAllowed("openai", "not-a-real-model"))

	withCatalog := NewAccess(held(unrestricted), "", matcher)
	assert.False(t, withCatalog.IsModelAllowed("openai", "not-a-real-model"),
		"the catalog does not place this model at this provider")

	// A named entry still matches exactly, catalog or not.
	named := permitWithModels(PermitVirtualKey, "vk2", "Caller Key", "openai", "gpt-4o")
	assert.True(t, NewAccess(held(named), "", matcher).IsModelAllowed("openai", "gpt-4o"))
	assert.False(t, NewAccess(held(named), "", matcher).IsModelAllowed("openai", "o3"))

	// Blacklists never go through the matcher: they are name matches, and they win.
	blacklisted := newPermit(permitSpec{
		Type: PermitVirtualKey, ID: "vk3", Name: "Caller Key",
		ProviderPermits: []schemas.ProviderPermit{{
			Provider:          "openai",
			AllowedModels:     []string{"gpt-4o"},
			BlacklistedModels: []string{"gpt-4o"},
		}},
	})
	assert.False(t, NewAccess(held(blacklisted), "", matcher).IsModelAllowed("openai", "gpt-4o"))

	t.Run("the matcher is handed the provider permit's own list, past the blacklist only", func(t *testing.T) {
		type call struct {
			provider, model string
			allowed         []string
		}
		var calls []call
		recording := func(provider, model string, allowed []string) bool {
			calls = append(calls, call{provider, model, allowed})
			return true
		}
		permit := newPermit(permitSpec{
			Type: PermitVirtualKey, ID: "vk4", Name: "Caller Key",
			ProviderPermits: []schemas.ProviderPermit{{
				Provider: "openai", AllowedModels: []string{"gpt-4o"}, BlacklistedModels: []string{"o3"},
			}},
		})
		access := NewAccess(held(permit), "", recording)

		assert.False(t, access.IsModelAllowed("openai", "o3"))
		assert.Empty(t, calls, "a blacklist wins before the matcher is consulted")

		assert.True(t, access.IsModelAllowed("openai", "gpt-4o-mini"), "the matcher's answer is the answer")
		require.Len(t, calls, 1)
		assert.Equal(t, call{"openai", "gpt-4o-mini", []string{"gpt-4o"}}, calls[0])

		assert.False(t, access.IsModelAllowed("anthropic", "claude-sonnet-4"))
		assert.Len(t, calls, 1, "a provider the permit does not hold never reaches the matcher")
	})
}

func TestAccess_KeysFor(t *testing.T) {
	baseRestricted := newPermit(permitSpec{
		Type: PermitVirtualKey, ID: "vk1", Name: "Caller Key",
		ProviderPermits: []schemas.ProviderPermit{
			{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"key-own"}},
			// A second provider permit for the same provider pools its keys with the first's.
			{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"key-own-too"}},
			{Provider: "bedrock", AllowedModels: []string{"*"}, KeyIDs: []string{"*"}},
			{Provider: "cohere", AllowedModels: []string{"*"}, KeyIDs: []string{}},
		},
	})
	access := NewAccess(held(baseRestricted), "", nil)

	// Both openai provider permits authorize the model, so the request may use either one's keys,
	// pooled in order. The assertion also pins the return type: consumers type-assert []string, so a
	// named slice type would fail their assertion silently and read as "no key restriction".
	keyIDs, restricted := access.KeysForModel("openai", "gpt-4o")
	assert.True(t, restricted)
	assert.Equal(t, []string{"key-own", "key-own-too"}, keyIDs)
	assert.IsType(t, []string{}, keyIDs)

	// Every key of the provider is allowed: no restriction to stamp anywhere.
	keyIDs, restricted = access.KeysForModel("bedrock", "claude-sonnet-4")
	assert.False(t, restricted)
	assert.Nil(t, keyIDs)

	// An empty restriction allows no key, and must not read as "unrestricted".
	keyIDs, restricted = access.KeysForModel("cohere", "command-r")
	assert.True(t, restricted)
	assert.NotNil(t, keyIDs)
	assert.Empty(t, keyIDs)

	// A provider nobody holds: the request cannot proceed on it, so there is no restriction to
	// report.
	keyIDs, restricted = access.KeysForModel("mistral", "mistral-large")
	assert.False(t, restricted)
	assert.Nil(t, keyIDs)
}

func TestAccess_KeysForComposes(t *testing.T) {
	withKeys := func(id string, keyIDs ...string) *Permit {
		return newPermit(permitSpec{
			Type: PermitType(id), ID: id, Name: id,
			ProviderPermits: []schemas.ProviderPermit{
				{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: keyIDs},
			},
		})
	}
	// capping holds keys at the level above the caller's, where it caps under Intersect.
	capping := func(id string, keyIDs ...string) *Permit {
		return atLevel(withKeys(id, keyIDs...), 1)
	}

	// The reason keys have to compose at all: a level above that narrows the request to two of the
	// provider's keys means it, and the caller's permit must not reopen the ones it excluded.
	t.Run("a capping level narrows the keys it excluded", func(t *testing.T) {
		access := NewAccess(held(withKeys("base", "key-a", "key-b"), capping("team", "key-b", "key-c")), Intersect, nil)
		keyIDs, restricted := access.KeysForModel("openai", "gpt-4o")
		assert.True(t, restricted)
		assert.Equal(t, []string{"key-b"}, keyIDs, "key-a is the caller's but not the team's")
	})

	t.Run("the caller's permits widen", func(t *testing.T) {
		access := NewAccess(held(withKeys("base", "key-a"), withKeys("other", "key-b")), "", nil)
		keyIDs, restricted := access.KeysForModel("openai", "gpt-4o")
		assert.True(t, restricted)
		assert.Equal(t, []string{"key-a", "key-b"}, keyIDs)
	})

	// Disjoint sets under a cap leave no key that may serve the provider, which is a restriction to
	// nothing rather than the absence of one.
	t.Run("disjoint under a cap allows no key", func(t *testing.T) {
		access := NewAccess(held(withKeys("base", "key-a"), capping("team", "key-b")), Intersect, nil)
		keyIDs, restricted := access.KeysForModel("openai", "gpt-4o")
		assert.True(t, restricted)
		assert.NotNil(t, keyIDs)
		assert.Empty(t, keyIDs)
	})

	// The wildcard is the universe, not an entry.
	t.Run("the wildcard composes as every key", func(t *testing.T) {
		capped := NewAccess(held(withKeys("base", "key-a"), capping("team", "*")), Intersect, nil)
		keyIDs, restricted := capped.KeysForModel("openai", "gpt-4o")
		assert.True(t, restricted)
		assert.Equal(t, []string{"key-a"}, keyIDs, "intersecting with every key is the other side")

		narrowed := NewAccess(held(withKeys("base", "*"), capping("team", "key-b")), Intersect, nil)
		keyIDs, restricted = narrowed.KeysForModel("openai", "gpt-4o")
		assert.True(t, restricted)
		assert.Equal(t, []string{"key-b"}, keyIDs, "a level above may narrow a caller who held every key")

		widened := NewAccess(held(withKeys("base", "key-a"), withKeys("other", "*")), "", nil)
		keyIDs, restricted = widened.KeysForModel("openai", "gpt-4o")
		assert.False(t, restricted, "unioning with every key is no restriction at all")
		assert.Nil(t, keyIDs)

		bothOpen := NewAccess(held(withKeys("base", "*"), capping("team", "*")), Intersect, nil)
		keyIDs, restricted = bothOpen.KeysForModel("openai", "gpt-4o")
		assert.False(t, restricted)
		assert.Nil(t, keyIDs)
	})

	t.Run("only the admitting permits hold the provider", func(t *testing.T) {
		// Under a union the caller's own permit admits the request, and another of its permits that
		// does not authorize the provider has no say in its keys.
		baseOnly := NewAccess(held(withKeys("base", "key-a"), permitWithProviders("other", "o", "O", "anthropic")), "", nil)
		keyIDs, restricted := baseOnly.KeysForModel("openai", "gpt-4o")
		assert.True(t, restricted)
		assert.Equal(t, []string{"key-a"}, keyIDs)

		alone := NewAccess(held(withKeys("base", "key-a")), "", nil)
		keyIDs, _ = alone.KeysForModel("openai", "gpt-4o")
		assert.Equal(t, []string{"key-a"}, keyIDs)

		// When that permit sits at a capping level the same shape is a refusal, and a refused
		// request has no keys.
		refused := NewAccess(held(withKeys("base", "key-a"), atLevel(permitWithProviders(testTeamPermit, "t", "T", "anthropic"), 1)), Intersect, nil)
		assert.False(t, refused.IsModelAllowed("openai", "gpt-4o"))
		keyIDs, restricted = refused.KeysForModel("openai", "gpt-4o")
		assert.False(t, restricted)
		assert.Nil(t, keyIDs)
	})

	// "This provider permit names no key" and "no provider permit was found" both arrive as an
	// empty list, and they are opposite answers: the first permits no key, the second imposes no
	// restriction. Deciding on emptiness rather than on whether a provider permit was found turns
	// the strictest possible restriction into the loosest.
	t.Run("a provider permit recording no key permits none", func(t *testing.T) {
		noKeys := newPermit(permitSpec{
			Type: PermitVirtualKey, ID: "vk1", Name: "Caller Key",
			ProviderPermits: []schemas.ProviderPermit{{Provider: "openai", AllowedModels: []string{"*"}}},
		})
		access := NewAccess(held(noKeys), "", nil)

		keyIDs, restricted := access.KeysForModel("openai", "gpt-4o")
		assert.True(t, restricted, "a provider permit with no key list restricts to no key")
		assert.NotNil(t, keyIDs)
		assert.Empty(t, keyIDs)

		keyIDs, restricted = access.KeysForModel("anthropic", "claude-sonnet-4")
		assert.False(t, restricted, "a provider no permit holds has no restriction to report")
		assert.Nil(t, keyIDs)
	})

	t.Run("key ids match exactly", func(t *testing.T) {
		// Key IDs are opaque, so a case-folding comparison would intersect two different keys
		// into one and hand the request a key neither level granted.
		access := NewAccess(held(withKeys("base", "Key-A"), capping("team", "key-a")), Intersect, nil)
		keyIDs, restricted := access.KeysForModel("openai", "gpt-4o")
		assert.True(t, restricted)
		assert.Empty(t, keyIDs)
	})

	// Within one permit, two provider permits for a provider are read as one too: the request may
	// use any key either of them allows, and that pooled list is what a capping level narrows.
	t.Run("two provider permits for one provider pool their keys", func(t *testing.T) {
		pooled := func() *Permit {
			return newPermit(permitSpec{
				Type: PermitVirtualKey, ID: "vk1", Name: "Caller Key",
				ProviderPermits: []schemas.ProviderPermit{
					{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"key-a"}},
					{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"key-b", "key-a"}},
				},
			})
		}
		keyIDs, restricted := NewAccess(held(pooled()), "", nil).KeysForModel("openai", "gpt-4o")
		assert.True(t, restricted)
		assert.Equal(t, []string{"key-a", "key-b"}, keyIDs, "the union, in order, without repeats")

		keyIDs, restricted = NewAccess(held(pooled(), capping("team", "key-b", "key-c")), Intersect, nil).KeysForModel("openai", "gpt-4o")
		assert.True(t, restricted)
		assert.Equal(t, []string{"key-b"}, keyIDs, "the cap narrows the pooled list, not either provider permit's own")

		open := newPermit(permitSpec{
			Type: PermitVirtualKey, ID: "vk2", Name: "Caller Key",
			ProviderPermits: []schemas.ProviderPermit{
				{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"key-a"}},
				{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"*"}},
			},
		})
		keyIDs, restricted = NewAccess(held(open), "", nil).KeysForModel("openai", "gpt-4o")
		assert.False(t, restricted, "one of them allows every key, so the pool is every key")
		assert.Nil(t, keyIDs)
	})
}

func TestAccess_KeysForReturnsACopy(t *testing.T) {
	base := newPermit(permitSpec{
		Type: PermitVirtualKey, ID: "vk1", Name: "Caller Key",
		ProviderPermits: []schemas.ProviderPermit{
			{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"key-own"}},
		},
	})
	access := NewAccess(held(base), "", nil)

	keyIDs, _ := access.KeysForModel("openai", "gpt-4o")
	require.NotEmpty(t, keyIDs)
	keyIDs[0] = "mutated"

	fresh, _ := access.KeysForModel("openai", "gpt-4o")
	assert.Equal(t, []string{"key-own"}, fresh, "a consumer must not be able to edit the permit")
}

// A candidate's keys are a copy for the same reason, on both paths that would otherwise hand out
// the permit's own slice: the candidate's own keys when nothing composed them, and the side
// composeKeyIDs passes through untouched when a capping level is unrestricted.
func TestAccess_ProvidersForCandidateKeysAreACopy(t *testing.T) {
	newBase := func() *Permit {
		return newPermit(permitSpec{
			Type: PermitVirtualKey, ID: "vk1", Name: "Caller Key",
			ProviderPermits: []schemas.ProviderPermit{
				{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"key-1", "key-2"}},
			},
		})
	}

	t.Run("the candidate's own keys", func(t *testing.T) {
		base := newBase()
		candidates := NewAccess(held(base), "", nil).ProvidersForModel("gpt-4o")
		require.Len(t, candidates, 1)
		require.NotEmpty(t, candidates[0].KeyIDs)
		candidates[0].KeyIDs[0] = "mutated"

		assert.Equal(t, schemas.WhiteList{"key-1", "key-2"}, base.ProviderPermits()[0].KeyIDs)
	})

	t.Run("keys composeKeyIDs passes through untouched", func(t *testing.T) {
		base := newBase()
		team := atLevel(newPermit(permitSpec{
			Type: testTeamPermit, ID: "t1", Name: "Team",
			ProviderPermits: []schemas.ProviderPermit{
				{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"*"}},
			},
		}), 1)
		candidates := NewAccess(held(base, team), Intersect, nil).ProvidersForModel("gpt-4o")
		require.Len(t, candidates, 1)
		require.NotEmpty(t, candidates[0].KeyIDs)
		candidates[0].KeyIDs[0] = "mutated"

		assert.Equal(t, schemas.WhiteList{"key-1", "key-2"}, base.ProviderPermits()[0].KeyIDs)
	})
}

func TestAccess_ProvidersFor(t *testing.T) {
	base := newPermit(permitSpec{
		Type: PermitVirtualKey, ID: "vk1", Name: "Caller Key",
		ProviderPermits: []schemas.ProviderPermit{
			{
				Provider:      "openai",
				AllowedModels: []string{"*"},
				KeyIDs:        []string{"key-own"},
				Weight:        ptr(0.7),
			},
			{
				// No weight: still a candidate, so the caller can see it and decide.
				Provider:      "bedrock",
				AllowedModels: []string{"*"},
				KeyIDs:        []string{"*"},
			},
			{
				// Does not permit the model.
				Provider:      "cohere",
				AllowedModels: []string{"command-r"},
			},
		},
	})

	t.Run("base permit only", func(t *testing.T) {
		access := NewAccess(held(base), "", nil)
		candidates := access.ProvidersForModel("gpt-4o")

		require.Len(t, candidates, 2)
		assert.Equal(t, "openai", candidates[0].Provider)
		assert.Equal(t, ptr(0.7), candidates[0].Weight)
		// The permits are what a caller asks for the limits covering this provider.
		assertPermitsAre(t, access.PermitsForModel(candidates[0].Provider, "gpt-4o"), base)
		assert.Equal(t, "bedrock", candidates[1].Provider)
		assert.Nil(t, candidates[1].Weight)
	})

	t.Run("a capping level never adds a provider", func(t *testing.T) {
		team := atLevel(newPermit(permitSpec{
			Type: testTeamPermit, ID: "t1", Name: "Team",
			ProviderPermits: []schemas.ProviderPermit{
				{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"key-team"}, Weight: ptr(0.1)},
				{Provider: "anthropic", AllowedModels: []string{"*"}, KeyIDs: []string{"key-team"}, Weight: ptr(0.3)},
			},
		}), 1)
		access := NewAccess(held(base, team), Intersect, nil)
		candidates := access.ProvidersForModel("gpt-4o")

		require.Len(t, candidates, 1)
		assert.Equal(t, "openai", candidates[0].Provider)
		assert.Equal(t, ptr(0.7), candidates[0].Weight, "the caller's own weight: a cap sets no preference")
		// The two key lists are disjoint, so the cap leaves no key that may serve this candidate.
		// It is still reported: whether an unusable candidate is worth attempting is the caller's
		// decision, the same as an unweighted one.
		assert.NotNil(t, candidates[0].KeyIDs)
		assert.Empty(t, candidates[0].KeyIDs)
	})

	t.Run("no model means no candidates", func(t *testing.T) {
		access := NewAccess(held(base), "", nil)
		assert.Empty(t, access.ProvidersForModel(""))
	})
}

func TestAccess_ProvidersForPerProviderPermitGranularity(t *testing.T) {
	// Two provider permits for one provider: only the one permitting the model is a candidate,
	// and a blacklist in either of them removes both.
	base := newPermit(permitSpec{
		Type: PermitVirtualKey, ID: "vk1", Name: "Caller Key",
		ProviderPermits: []schemas.ProviderPermit{
			{Provider: "openai", AllowedModels: []string{"o3"}, Weight: ptr(0.2)},
			{Provider: "openai", AllowedModels: []string{"gpt-4o"}, Weight: ptr(0.8)},
		},
	})
	access := NewAccess(held(base), "", nil)

	candidates := access.ProvidersForModel("gpt-4o")
	require.Len(t, candidates, 1)
	assert.Equal(t, ptr(0.8), candidates[0].Weight)

	blacklisted := newPermit(permitSpec{
		Type: PermitVirtualKey, ID: "vk2", Name: "Caller Key",
		ProviderPermits: []schemas.ProviderPermit{
			{Provider: "openai", AllowedModels: []string{"*"}, BlacklistedModels: []string{"gpt-4o"}, Weight: ptr(0.2)},
			{Provider: "openai", AllowedModels: []string{"gpt-4o"}, Weight: ptr(0.8)},
		},
	})
	assert.Empty(t, NewAccess(held(blacklisted), "", nil).ProvidersForModel("gpt-4o"))
}

// The caller's permits are read as one, so any of them can be the one authorizing a request, which
// makes "the access permits this pair" and "this permit permits this pair" different questions. A
// permit that blacklists the model must not serve it even while another of the caller's permits
// permits it, and the request must still be served, by the permit that does.
func TestAccess_ServesFromThePermitThatPermits(t *testing.T) {
	base := newPermit(permitSpec{
		Type: PermitVirtualKey, ID: "vk1", Name: "Caller Key",
		ProviderPermits: []schemas.ProviderPermit{{
			Provider:          "openai",
			AllowedModels:     []string{"*"},
			BlacklistedModels: []string{"gpt-4o"},
			KeyIDs:            []string{"key-own"},
		}},
	})
	other := newPermit(permitSpec{
		Type: PermitVirtualKey, ID: "vk2", Name: "Other Key",
		ProviderPermits: []schemas.ProviderPermit{{
			Provider:      "openai",
			AllowedModels: []string{"*"},
			KeyIDs:        []string{"key-other"},
		}},
	})
	access := NewAccess(held(base, other), "", nil)

	require.True(t, access.IsModelAllowed("openai", "gpt-4o"), "the other permit permits it")
	// And it alone is named: a permit that blacklists the model does not permit the pair.
	assertPermitsAre(t, access.PermitsForModel("openai", "gpt-4o"), other)

	candidates := access.ProvidersForModel("gpt-4o")
	require.Len(t, candidates, 1, "the request is permitted, so something must be able to serve it")
	// Served by the permit that permits the model, not the one that blacklists it.
	assertPermitsAre(t, access.PermitsForModel(candidates[0].Provider, "gpt-4o"), other)
	assert.Equal(t, schemas.WhiteList{"key-other"}, candidates[0].KeyIDs,
		"the blacklisting permit does not get to say which keys serve a request it refused")

	// KeysForModel agrees with the candidate: the admitting permit's keys alone, because the
	// blacklisting permit is not authorizing this request.
	keyIDs, restricted := access.KeysForModel("openai", "gpt-4o")
	assert.True(t, restricted)
	assert.Equal(t, []string{"key-other"}, keyIDs)

	// The coarse gate has to agree: a provider one permit holds but cannot serve is still granted,
	// through the other.
	assert.Equal(t, []string{"openai"}, access.GrantedProvidersForModel("gpt-4o"))

	// A model both permit is offered from each under its own keys, the request may use either's,
	// and both are named for it.
	both := access.ProvidersForModel("o3")
	require.Len(t, both, 2)
	assert.Equal(t, schemas.WhiteList{"key-own"}, both[0].KeyIDs)
	assert.Equal(t, schemas.WhiteList{"key-other"}, both[1].KeyIDs)
	keyIDs, _ = access.KeysForModel("openai", "o3")
	assert.Equal(t, []string{"key-own", "key-other"}, keyIDs)
	assertPermitsAre(t, access.PermitsForModel("openai", "o3"), base, other)
}

func TestAccess_GrantedProvidersFor(t *testing.T) {
	base := newPermit(permitSpec{
		Type: PermitVirtualKey, ID: "vk1", Name: "Caller Key",
		ProviderPermits: []schemas.ProviderPermit{
			{Provider: "openai", AllowedModels: []string{"gpt-4o"}},
			{Provider: "bedrock", AllowedModels: []string{"*"}, BlacklistedModels: []string{"gpt-4o"}},
			{Provider: "  ", AllowedModels: []string{"*"}},
		},
	})

	t.Run("base permit only", func(t *testing.T) {
		access := NewAccess(held(base), "", nil)
		assert.Equal(t, []string{"openai"}, access.GrantedProvidersForModel("gpt-4o"))
		// No model to filter on keeps every granted provider.
		assert.Equal(t, []string{"openai", "bedrock"}, access.GrantedProvidersForModel(""))
	})

	t.Run("a capping level drops what it does not hold", func(t *testing.T) {
		team := atLevel(permitWithProviders(testTeamPermit, "t1", "Team", "openai", "anthropic"), 1)
		access := NewAccess(held(base, team), Intersect, nil)
		assert.Equal(t, []string{"openai"}, access.GrantedProvidersForModel("gpt-4o"), "a cap never adds anthropic")

		// A provider the caller holds but the level above does not is dropped.
		narrow := atLevel(permitWithProviders(testTeamPermit, "t2", "Team", "anthropic"), 1)
		narrowAccess := NewAccess(held(base, narrow), Intersect, nil)
		assert.Empty(t, narrowAccess.GrantedProvidersForModel("gpt-4o"))
	})

	t.Run("empty is not nil", func(t *testing.T) {
		// An empty allowlist means "no provider is permitted", which a consumer must be able to
		// tell apart from "nothing was published".
		access := NewAccess(nil, "", nil)
		providers := access.GrantedProvidersForModel("gpt-4o")
		assert.NotNil(t, providers)
		assert.Empty(t, providers)
	})
}

// The coarse gate reports providers, not provider permits, so a permit holding several for one
// provider still names it once. A consumer treats the result as a set: a repeated provider would
// read as two ways in where there is one.
func TestAccess_GrantedProvidersForDeduplicates(t *testing.T) {
	base := newPermit(permitSpec{
		Type: PermitVirtualKey, ID: "vk1", Name: "Caller Key",
		ProviderPermits: []schemas.ProviderPermit{
			{Provider: "openai", AllowedModels: []string{"gpt-4o"}},
			{Provider: "openai", AllowedModels: []string{"gpt-4o", "o3"}},
			{Provider: "anthropic", AllowedModels: []string{"*"}},
		},
	})
	access := NewAccess(held(base), "", nil)

	assert.Equal(t, []string{"openai", "anthropic"}, access.GrantedProvidersForModel("gpt-4o"),
		"both openai provider permits permit it, and it is still one provider")
	assert.Equal(t, []string{"openai", "anthropic"}, access.GrantedProvidersForModel(""))

	// A model only the second provider permit permits still names the provider once.
	assert.Equal(t, []string{"openai", "anthropic"}, access.GrantedProvidersForModel("o3"))

	// And ProvidersForModel, which reports provider permits rather than providers, sees both: the two
	// answer different questions about the same permit.
	require.Len(t, access.ProvidersForModel("gpt-4o"), 3, "two openai provider permits plus anthropic")
}

func TestAccess_GrantedProvidersForIgnoresTheMatcher(t *testing.T) {
	// This coarse gate must not resolve model names even when a matcher is available: the layers
	// consuming it run their own resolution.
	base := permitWithModels(PermitVirtualKey, "vk1", "Caller Key", "openai", "*")
	catalog := modelcatalog.NewTestCatalog(nil)
	matcher := catalogMatcher(catalog, &fakeProviderConfigSource{})

	access := NewAccess(held(base), "", matcher)
	assert.Equal(t, []string{"openai"}, access.GrantedProvidersForModel("not-a-real-model"))
	assert.False(t, access.IsModelAllowed("openai", "not-a-real-model"), "the exact check does resolve it")
}

func TestAccess_DeniedBy(t *testing.T) {
	base := permitWithModels(PermitVirtualKey, "vk1", "Caller Key", "openai", "gpt-4o")
	newTeam := func() *Permit {
		return atLevel(permitWithModels(testTeamPermit, "t1", "Team Name", "openai", "o3"), 1)
	}

	t.Run("allowed requests have no denying permit", func(t *testing.T) {
		access := NewAccess(held(base), "", nil)
		assert.Nil(t, access.DeniedPermitsForModel("openai", "gpt-4o"))
		assert.Nil(t, access.DeniedPermitsForModel("openai", ""))
	})

	t.Run("the base permit is named when it denies", func(t *testing.T) {
		access := NewAccess(held(base), "", nil)
		denied := access.DeniedPermitsForModel("anthropic", "claude-sonnet-4")
		require.Len(t, denied, 1)
		assert.Equal(t, "Caller Key", denied[0].Name())
		assert.Equal(t, string(PermitVirtualKey), denied[0].Type())
	})

	t.Run("a capping level is named when it alone denies", func(t *testing.T) {
		access := NewAccess(held(base, newTeam()), Intersect, nil)
		denied := access.DeniedPermitsForModel("openai", "gpt-4o")
		require.Len(t, denied, 1)
		assert.Equal(t, "Team Name", denied[0].Name())
		assert.Equal(t, string(testTeamPermit), denied[0].Type())

		// Provider-level denials are attributed the same way.
		narrow := atLevel(permitWithProviders(testTeamPermit, "t2", "Narrow", "anthropic"), 1)
		providerAccess := NewAccess(held(base, narrow), Intersect, nil)
		denied = providerAccess.DeniedPermitsForModel("openai", "")
		require.Len(t, denied, 1)
		assert.Equal(t, "Narrow", denied[0].Name())
	})

	t.Run("a refusing base is named without the level above", func(t *testing.T) {
		// The cap did not stand in the way: with no base permitting, its own verdict changes nothing.
		access := NewAccess(held(base, newTeam()), Intersect, nil)
		denied := access.DeniedPermitsForModel("openai", "o3")
		require.Len(t, denied, 1)
		assert.Same(t, base, denied[0])
	})

	t.Run("a refused request always names a permit", func(t *testing.T) {
		for _, levelMode := range []CompositionMode{Union, Intersect} {
			access := NewAccess(held(base, newTeam()), levelMode, nil)
			for _, model := range []string{"gpt-4o", "o3", "gpt-4o-mini"} {
				if access.IsModelAllowed("openai", model) {
					continue
				}
				assert.NotEmpty(t, access.DeniedPermitsForModel("openai", model), "level mode %q model %q", levelMode, model)
			}
		}
	})

	t.Run("nothing is named when the caller holds no permit", func(t *testing.T) {
		for _, levelMode := range []CompositionMode{Union, Intersect} {
			access := NewAccess(nil, levelMode, nil)
			assert.False(t, access.IsModelAllowed("openai", "o3"))
			assert.Nil(t, access.DeniedPermitsForModel("openai", "o3"), "level mode %q: the caller holds no permit to name", levelMode)
		}
	})
}

func TestAccess_Accessors(t *testing.T) {
	base := permitWithProviders(PermitVirtualKey, "vk1", "Caller Key", "openai")

	access := NewAccess(held(base), Union, nil)
	assert.Equal(t, string(Union), access.LevelMode())
	bases := access.Bases()
	require.Len(t, bases, 1)
	assert.Same(t, base, bases[0])

	bare := NewAccess(held(base), "", nil)
	assert.Equal(t, "", bare.LevelMode())

	t.Run("bases are handed out as a copy", func(t *testing.T) {
		mine := access.Bases()
		mine[0] = nil
		fresh := access.Bases()
		require.Len(t, fresh, 1)
		assert.Same(t, base, fresh[0], "a consumer must not be able to edit the access through the answer")
	})
}

func TestAccess_NewAccessDropsWhatIsNotThere(t *testing.T) {
	first := permitWithProviders(testCallerPermit, "ap1", "First", "openai")
	second := permitWithProviders(testCallerPermit, "ap2", "Second", "anthropic")

	// A resolver that found nothing for one source does not have to say so twice: nil entries,
	// bare or typed, are dropped.
	access := NewAccess([]schemas.Permit{nil, first, (*Permit)(nil), second}, Intersect, nil)

	bases := access.Bases()
	require.Len(t, bases, 2)
	assert.Same(t, first, bases[0])
	assert.Same(t, second, bases[1])

	assert.True(t, access.IsModelAllowed("anthropic", "claude-sonnet-4"),
		"a dropped entry is no level: both permits sit at the caller's own level, read as one")
	assert.True(t, access.IsModelAllowed("openai", "gpt-4o"))
}

// twoProfiles is the shape a caller holding several permits has: two profiles, in attribution
// order. They overlap on openai and bedrock, disagree about what each permits there, and each holds
// a provider the other does not.
func twoProfiles() (first, second *Permit) {
	first = newPermit(permitSpec{
		Type: testCallerPermit, ID: "ap1", Name: "First Profile",
		ProviderPermits: []schemas.ProviderPermit{
			{
				Provider: "openai", AllowedModels: []string{"gpt-4o"}, KeyIDs: []string{"key-first"}, Weight: ptr(0.5),
			},
			{
				Provider: "bedrock", AllowedModels: []string{"*"}, BlacklistedModels: []string{"claude-opus-4"}, KeyIDs: []string{"key-first-bedrock"},
			},
		},
	})
	second = newPermit(permitSpec{
		Type: testCallerPermit, ID: "ap2", Name: "Second Profile",
		ProviderPermits: []schemas.ProviderPermit{
			{
				Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"key-second"}, Weight: ptr(0.9),
			},
			{
				Provider: "bedrock", AllowedModels: []string{"*"}, KeyIDs: []string{"key-second-bedrock"},
			},
			{
				Provider: "anthropic", AllowedModels: []string{"claude-sonnet-4"}, KeyIDs: []string{"key-second-anthropic"},
			},
		},
	})
	return first, second
}

// openaiTeam is a team level above the caller holding openai alone, under keys, for every model.
func openaiTeam(keyIDs ...string) *Permit {
	return atLevel(newPermit(permitSpec{
		Type: testTeamPermit, ID: "t1", Name: "Team",
		ProviderPermits: []schemas.ProviderPermit{
			{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: keyIDs},
		},
	}), 1)
}

// The caller's permits are read as one: what any of them permits, the caller may reach.
func TestAccess_SeveralBasesAreReadAsOne(t *testing.T) {
	first, second := twoProfiles()
	access := NewAccess(held(first, second), "", nil)

	assert.True(t, access.IsModelAllowed("openai", "gpt-4o"), "the first permits it")
	assert.True(t, access.IsModelAllowed("openai", "o3"), "only the second permits it")
	assert.True(t, access.IsModelAllowed("anthropic", "claude-sonnet-4"), "a provider only the second holds")
	assert.True(t, access.IsProviderAllowed("anthropic"))
	assert.False(t, access.IsModelAllowed("anthropic", "claude-opus-4"), "neither permits it")
	assert.False(t, access.IsProviderAllowed("cohere"), "neither holds it")

	// A blacklist blocks the permit that carries it, not the caller: the second permit does not
	// blacklist the model, so through it the caller may reach it.
	assert.True(t, access.IsModelAllowed("bedrock", "claude-opus-4"))
}

// The attribution rule: every one of the caller's permits that permits the pair is named, in their
// order. The caller's permits are read as one, so each that permits the pair covers the request and
// each is charged.
func TestAccess_PermitsForModelNamesEveryPermittingPermit(t *testing.T) {
	first, second := twoProfiles()
	access := NewAccess(held(first, second), "", nil)

	tests := []struct {
		name     string
		provider string
		model    string
		want     []schemas.Permit // nil: the request may not use the pair at all
	}{
		{name: "both bases permit it: both are named, in their order", provider: "openai", model: "gpt-4o", want: []schemas.Permit{first, second}},
		{name: "only the second permits the model", provider: "openai", model: "o3", want: []schemas.Permit{second}},
		{name: "a provider only the second holds", provider: "anthropic", model: "claude-sonnet-4", want: []schemas.Permit{second}},
		{name: "the first blacklists it, the second permits it", provider: "bedrock", model: "claude-opus-4", want: []schemas.Permit{second}},
		{name: "the provider alone names every base holding it", provider: "openai", model: "", want: []schemas.Permit{first, second}},
		{name: "no base permits the model", provider: "anthropic", model: "claude-opus-4"},
		{name: "no base holds the provider", provider: "cohere", model: "command-r"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := access.PermitsForModel(tt.provider, tt.model)
			if tt.want == nil {
				assert.False(t, access.IsModelAllowed(tt.provider, tt.model), "a refused pair answers to no permit")
				assert.Nil(t, got)
				return
			}
			assert.True(t, access.IsModelAllowed(tt.provider, tt.model))
			assertPermitsAre(t, got, tt.want...)
		})
	}

	t.Run("the answer is a copy", func(t *testing.T) {
		mine := access.PermitsForModel("openai", "gpt-4o")
		require.Len(t, mine, 2)
		mine[0] = nil
		assertPermitsAre(t, access.PermitsForModel("openai", "gpt-4o"), first, second)
	})
}

// The caller's permits are read as one for keys too: any key any of the permitting bases allows may
// serve the request, and that pooled list is what a capping level narrows.
func TestAccess_KeysForPoolTheCallersPermits(t *testing.T) {
	first, second := twoProfiles()

	t.Run("the union of every permitting base's keys", func(t *testing.T) {
		access := NewAccess(held(first, second), "", nil)

		keyIDs, restricted := access.KeysForModel("openai", "gpt-4o")
		assert.True(t, restricted)
		assert.Equal(t, []string{"key-first", "key-second"}, keyIDs, "both permit it, so either one's keys may serve it")

		keyIDs, restricted = access.KeysForModel("openai", "o3")
		assert.True(t, restricted)
		assert.Equal(t, []string{"key-second"}, keyIDs, "the first does not permit it, and has no say")

		keyIDs, restricted = access.KeysForModel("bedrock", "claude-opus-4")
		assert.True(t, restricted)
		assert.Equal(t, []string{"key-second-bedrock"}, keyIDs, "the first blacklists it, and has no say")

		keyIDs, restricted = access.KeysForModel("anthropic", "claude-opus-4")
		assert.False(t, restricted, "a refused pair has no key restriction to report")
		assert.Nil(t, keyIDs)
	})

	t.Run("a capping level narrows the pooled keys", func(t *testing.T) {
		access := NewAccess(held(first, second, openaiTeam("key-first", "key-team")), Intersect, nil)

		keyIDs, _ := access.KeysForModel("openai", "gpt-4o")
		assert.Equal(t, []string{"key-first"}, keyIDs, "of the pooled keys, only the one the team also allows")

		keyIDs, restricted := access.KeysForModel("openai", "o3")
		assert.True(t, restricted)
		assert.Empty(t, keyIDs, "the second's keys and the team's are disjoint")

		keyIDs, restricted = access.KeysForModel("bedrock", "claude-sonnet-4")
		assert.False(t, restricted, "the team does not hold bedrock, so the pair is refused")
		assert.Nil(t, keyIDs)
	})

	t.Run("a wildcard in any permitting base opens the caller's side", func(t *testing.T) {
		everyKey := func() *Permit {
			return newPermit(permitSpec{
				Type: testCallerPermit, ID: "ap3", Name: "Every Key",
				ProviderPermits: []schemas.ProviderPermit{
					{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"*"}},
				},
			})
		}

		keyIDs, restricted := NewAccess(held(first, everyKey()), "", nil).KeysForModel("openai", "gpt-4o")
		assert.False(t, restricted, "one base allows every key, so the pool is every key")
		assert.Nil(t, keyIDs)

		keyIDs, restricted = NewAccess(held(first, everyKey(), openaiTeam("key-first", "key-team")), Intersect, nil).KeysForModel("openai", "gpt-4o")
		assert.True(t, restricted)
		assert.Equal(t, []string{"key-first", "key-team"}, keyIDs, "a capping level still narrows an open pool")
	})
}

// The caller's permits are read as one, so nothing shadows across them: every base is offered in
// full, in base order, and a provider two bases serve is two candidates, each under its own weight
// and keys. A provider a base holds but cannot serve for this model is still offered from whatever
// can.
func TestAccess_ProvidersForOffersEveryBaseInFull(t *testing.T) {
	first, second := twoProfiles()

	t.Run("a provider two bases serve is offered once per base", func(t *testing.T) {
		access := NewAccess(held(first, second), "", nil)
		candidates := access.ProvidersForModel("gpt-4o")

		require.Len(t, candidates, 4, "the first's two providers, then the second's two")
		// The first base, in full.
		assert.Equal(t, "openai", candidates[0].Provider)
		assert.Equal(t, ptr(0.5), candidates[0].Weight)
		assert.Equal(t, schemas.WhiteList{"key-first"}, candidates[0].KeyIDs)
		assert.Equal(t, "bedrock", candidates[1].Provider)
		assert.Nil(t, candidates[1].Weight)
		assert.Equal(t, schemas.WhiteList{"key-first-bedrock"}, candidates[1].KeyIDs)
		// Then the second, in full: the same providers again, under its own weight and keys.
		assert.Equal(t, "openai", candidates[2].Provider)
		assert.Equal(t, ptr(0.9), candidates[2].Weight)
		assert.Equal(t, schemas.WhiteList{"key-second"}, candidates[2].KeyIDs)
		assert.Equal(t, "bedrock", candidates[3].Provider)
		assert.Nil(t, candidates[3].Weight)
		assert.Equal(t, schemas.WhiteList{"key-second-bedrock"}, candidates[3].KeyIDs)

		// Both openai candidates rest on the same answer: every permit that permits the pair.
		assertPermitsAre(t, access.PermitsForModel("openai", "gpt-4o"), first, second)
		assertPermitsAre(t, access.PermitsForModel("bedrock", "gpt-4o"), first, second)
	})

	t.Run("a provider the first base holds but cannot serve is offered from the second", func(t *testing.T) {
		access := NewAccess(held(first, second), "", nil)
		candidates := access.ProvidersForModel("claude-opus-4")

		require.Len(t, candidates, 2)
		assert.Equal(t, "openai", candidates[0].Provider)
		assert.Equal(t, ptr(0.9), candidates[0].Weight, "the first does not allow the model on openai, so only the second offers it")
		assert.Equal(t, schemas.WhiteList{"key-second"}, candidates[0].KeyIDs)
		assertPermitsAre(t, access.PermitsForModel("openai", "claude-opus-4"), second)
		assert.Equal(t, "bedrock", candidates[1].Provider)
		assert.Equal(t, schemas.WhiteList{"key-second-bedrock"}, candidates[1].KeyIDs, "the first blacklists the model on bedrock, so only the second offers it")
		assertPermitsAre(t, access.PermitsForModel("bedrock", "claude-opus-4"), second)
	})

	t.Run("evaluation order is base order, each base in full", func(t *testing.T) {
		access := NewAccess(held(first, second), "", nil)
		candidates := access.ProvidersForModel("o3")

		require.Len(t, candidates, 3, "bedrock from the first, then openai and bedrock from the second")
		assert.Equal(t, "bedrock", candidates[0].Provider)
		assert.Equal(t, schemas.WhiteList{"key-first-bedrock"}, candidates[0].KeyIDs)
		assert.Equal(t, "openai", candidates[1].Provider)
		assert.Equal(t, ptr(0.9), candidates[1].Weight)
		assert.Equal(t, "bedrock", candidates[2].Provider)
		assert.Equal(t, schemas.WhiteList{"key-second-bedrock"}, candidates[2].KeyIDs, "served by the first already, and offered again from the second")
		assertPermitsAre(t, access.PermitsForModel("openai", "o3"), second)
		assertPermitsAre(t, access.PermitsForModel("bedrock", "o3"), first, second)
	})

	t.Run("under a cap each base's candidate is narrowed on its own", func(t *testing.T) {
		access := NewAccess(held(first, second, openaiTeam("key-first", "key-team")), Intersect, nil)
		candidates := access.ProvidersForModel("gpt-4o")

		require.Len(t, candidates, 2, "openai from each base: the team does not hold bedrock")
		assert.Equal(t, "openai", candidates[0].Provider)
		assert.Equal(t, ptr(0.5), candidates[0].Weight)
		assert.Equal(t, schemas.WhiteList{"key-first"}, candidates[0].KeyIDs, "the first's keys intersected with the team's")
		assert.Equal(t, "openai", candidates[1].Provider)
		assert.Equal(t, ptr(0.9), candidates[1].Weight)
		assert.NotNil(t, candidates[1].KeyIDs)
		assert.Empty(t, candidates[1].KeyIDs, "the second's keys and the team's are disjoint, and the candidate is still reported")
	})

	t.Run("within one base nothing shadows either", func(t *testing.T) {
		twice := newPermit(permitSpec{
			Type: testCallerPermit, ID: "ap3", Name: "Twice",
			ProviderPermits: []schemas.ProviderPermit{
				{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"key-a"}, Weight: ptr(0.2)},
				{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"key-b"}, Weight: ptr(0.8)},
			},
		})
		access := NewAccess(held(first, twice), "", nil)
		candidates := access.ProvidersForModel("o3")

		require.Len(t, candidates, 3, "bedrock from the first, then both openai provider permits of the second")
		assert.Equal(t, "bedrock", candidates[0].Provider)
		assert.Equal(t, "openai", candidates[1].Provider)
		assert.Equal(t, ptr(0.2), candidates[1].Weight)
		assert.Equal(t, "openai", candidates[2].Provider)
		assert.Equal(t, ptr(0.8), candidates[2].Weight)
		assertPermitsAre(t, access.PermitsForModel("openai", "o3"), twice)
	})
}

func TestAccess_GrantedProvidersForMergesBases(t *testing.T) {
	first, second := twoProfiles()

	t.Run("deduplicated, in base order", func(t *testing.T) {
		access := NewAccess(held(first, second), "", nil)
		assert.Equal(t, []string{"openai", "bedrock", "anthropic"}, access.GrantedProvidersForModel(""))
		assert.Equal(t, []string{"openai", "bedrock"}, access.GrantedProvidersForModel("gpt-4o"))
		assert.Equal(t, []string{"bedrock", "openai"}, access.GrantedProvidersForModel("o3"),
			"openai is named by the first permit that grants it for the model")
	})

	t.Run("under a cap only what the level above also allows by name", func(t *testing.T) {
		access := NewAccess(held(first, second, openaiTeam("*")), Intersect, nil)
		assert.Equal(t, []string{"openai"}, access.GrantedProvidersForModel("gpt-4o"))
		assert.Equal(t, []string{"openai"}, access.GrantedProvidersForModel(""))
	})
}

func TestAccess_DeniedByNamesEveryRefusingSide(t *testing.T) {
	first, second := twoProfiles()

	t.Run("every base when none permits", func(t *testing.T) {
		access := NewAccess(held(first, second), "", nil)
		denied := access.DeniedPermitsForModel("anthropic", "claude-opus-4")
		require.Len(t, denied, 2)
		assert.Same(t, first, denied[0])
		assert.Same(t, second, denied[1])
	})

	t.Run("only the capping level when a base permits", func(t *testing.T) {
		team := openaiTeam("*")
		access := NewAccess(held(first, second, team), Intersect, nil)
		denied := access.DeniedPermitsForModel("bedrock", "claude-sonnet-4")
		require.Len(t, denied, 1)
		assert.Same(t, team, denied[0])

		denied = access.DeniedPermitsForModel("cohere", "command-r")
		require.Len(t, denied, 2, "no base permits, and the cap's refusal changes nothing")
		assert.Same(t, first, denied[0])
		assert.Same(t, second, denied[1])
	})
}

// A candidate is only ever offered by a permit that permits the model on its provider, and every
// such permit is among those the request answers to for the pair, which is what lets the permit be
// left off the candidate: whoever needs to know what a candidate costs asks PermitsForModel with
// the candidate's provider and the model. The walk below reproduces ProvidersForModel's own
// acceptance rule to recover the offering permit of every candidate, in order, and pins that
// PermitsForModel names it.
func TestAccess_CandidatesAreOfferedByPermitsForModel(t *testing.T) {
	first, second := twoProfiles()
	twice := newPermit(permitSpec{
		Type: testCallerPermit, ID: "ap3", Name: "Twice",
		ProviderPermits: []schemas.ProviderPermit{
			{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"key-a"}, Weight: ptr(0.2)},
			{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"key-b"}, Weight: ptr(0.8)},
		},
	})
	accesses := map[string]*Access{
		"two bases":                         NewAccess(held(first, second), "", nil),
		"two bases under a cap":             NewAccess(held(first, second, openaiTeam("key-first")), Intersect, nil),
		"second base first":                 NewAccess(held(second, first), "", nil),
		"one base holding a provider twice": NewAccess(held(first, twice), "", nil),
	}
	models := []string{"gpt-4o", "o3", "claude-opus-4", "claude-sonnet-4", "nothing-permits-this"}

	type offer struct {
		provider string
		owner    schemas.Permit
	}
	for name, access := range accesses {
		for _, model := range models {
			var offers []offer
			access.eachProviderPermit(func(owner schemas.Permit, pp *schemas.ProviderPermit) bool {
				if !access.IsModelAllowed(pp.Provider, model) || blacklistsModel(owner, pp.Provider, model) || !access.permitsModel(pp, model, pp.KeyIDs) {
					return false
				}
				offers = append(offers, offer{provider: pp.Provider, owner: owner})
				return true
			})
			candidates := access.ProvidersForModel(model)
			require.Len(t, candidates, len(offers), "%s / %s", name, model)
			for i, candidate := range candidates {
				assert.Equal(t, offers[i].provider, candidate.Provider, "%s / %s / candidate %d", name, model, i)
				permits := access.PermitsForModel(candidate.Provider, model)
				assert.True(t, containsPermit(permits, offers[i].owner),
					"%s / %s / %s: the offering permit is among those the request answers to", name, model, candidate.Provider)
			}
		}
	}
}

// containsPermit reports whether permits holds want, by identity.
func containsPermit(permits []schemas.Permit, want schemas.Permit) bool {
	for _, permit := range permits {
		if permit == want {
			return true
		}
	}
	return false
}

// assertPermitsAre pins the exact permits, in order, by identity.
func assertPermitsAre(t *testing.T, got []schemas.Permit, want ...schemas.Permit) {
	t.Helper()
	require.Len(t, got, len(want))
	for i := range want {
		assert.Same(t, want[i], got[i], "permit %d", i)
	}
}

// A blacklist wins over a key alias: naming a blocked model through an alias does not get it past
// the permit, whether the alias is admitted by a pattern or by its own name.
func TestAccess_AliasTargetHeldToBlacklist(t *testing.T) {
	allowAll := func(string, string, []string) bool { return true }
	bestToOpus := func(provider, model string, _ []string, visit func(string)) {
		if provider == "bedrock" && model == "best-model" {
			visit("claude-opus-4-6")
		}
	}
	otherTarget := func(provider, model string, _ []string, visit func(string)) {
		if model == "best-model" {
			visit("claude-sonnet-4-6")
		}
	}
	regexPermit := func() *Permit {
		return permitWithBlacklist(PermitVirtualKey, "vk1", "VK", "bedrock", []string{"regex:^claude-.*"}, "claude-opus-4-6")
	}

	t.Run("an alias of a blacklisted model is refused and the permit is named", func(t *testing.T) {
		access := NewAccess(held(regexPermit()), "", allowAll).WithAliasTargets(bestToOpus)
		assert.False(t, access.IsModelAllowed("bedrock", "best-model"))
		assert.Len(t, access.DeniedPermitsForModel("bedrock", "best-model"), 1)
		assert.False(t, access.IsModelAllowed("bedrock", "claude-opus-4-6"), "the literal blacklist still holds")
	})
	t.Run("without a source, or with another target, the alias is admitted", func(t *testing.T) {
		assert.True(t, NewAccess(held(regexPermit()), "", allowAll).IsModelAllowed("bedrock", "best-model"))
		assert.True(t, NewAccess(held(regexPermit()), "", allowAll).WithAliasTargets(otherTarget).IsModelAllowed("bedrock", "best-model"))
	})
	t.Run("an alias allowed by its own name is refused when its target is blacklisted", func(t *testing.T) {
		p := permitWithBlacklist(PermitVirtualKey, "vk1", "VK", "bedrock", []string{"best-model"}, "claude-opus-4-6")
		access := NewAccess(held(p), "", nil).WithAliasTargets(bestToOpus)
		assert.False(t, access.IsModelAllowed("bedrock", "best-model"))
	})
	t.Run("a permit that blacklists nothing never consults the source", func(t *testing.T) {
		calls := 0
		counting := func(string, string, []string, func(string)) { calls++ }
		access := NewAccess(held(permitWithModels(PermitVirtualKey, "vk1", "VK", "bedrock", "regex:^claude-.*")), "", allowAll).WithAliasTargets(counting)
		assert.True(t, access.IsModelAllowed("bedrock", "best-model"))
		assert.Zero(t, calls)
	})
	t.Run("a blacklist on an unrelated model admits the alias and consults the source once", func(t *testing.T) {
		calls := 0
		counting := func(provider, model string, _ []string, visit func(string)) {
			calls++
			bestToOpus(provider, model, nil, visit)
		}
		p := permitWithBlacklist(PermitVirtualKey, "vk1", "VK", "bedrock", []string{"regex:^claude-.*"}, "claude-haiku-4-5")
		access := NewAccess(held(p), "", allowAll).WithAliasTargets(counting)
		assert.True(t, access.IsModelAllowed("bedrock", "best-model"))
		assert.Equal(t, 1, calls)
	})
	t.Run("two base permits, only one blacklisting the target: the other still admits (any base permitting)", func(t *testing.T) {
		open := permitWithModels(PermitVirtualKey, "vk2", "Open", "bedrock", "regex:^claude-.*")
		access := NewAccess(held(regexPermit(), open), "", allowAll).WithAliasTargets(bestToOpus)
		assert.True(t, access.IsModelAllowed("bedrock", "best-model"))
	})
}

// The key-scoped matcher and the alias-target source are told which keys the permit lets serve the
// request, so a key the permit excludes neither vetoes an alias nor adds a target (#6620 review). A
// permit holding several provider permits for one provider is served by all of their keys, so it
// reports every key.
func TestAccess_KeyScopedMatcherAndAliasTargetsSeeThePermitsKeys(t *testing.T) {
	keyed := func(pps ...schemas.ProviderPermit) *Permit {
		return NewPermit(PermitVirtualKey, "vk1", "VK", true, false, pps)
	}
	var seen [][]string
	matcher := func(_, model string, allowed, keyIDs []string) bool {
		seen = append(seen, keyIDs)
		return model == "best-model" && slices.Equal(keyIDs, []string{"aliased"})
	}
	t.Run("one provider permit: its own keys", func(t *testing.T) {
		seen = nil
		access := NewAccess(held(keyed(schemas.ProviderPermit{Provider: "bedrock", AllowedModels: []string{"claude-sonnet-4-6"}, KeyIDs: []string{"aliased"}})), "", nil).WithKeyScopedMatcher(matcher)
		assert.True(t, access.IsModelAllowed("bedrock", "best-model"))
		require.NotEmpty(t, seen)
		assert.Equal(t, []string{"aliased"}, seen[0])
		for _, c := range access.ProvidersForModel("best-model") {
			assert.ElementsMatch(t, []string{"aliased"}, c.KeyIDs)
		}
	})
	t.Run("two provider permits for the provider: the union of their keys", func(t *testing.T) {
		seen = nil
		access := NewAccess(held(keyed(
			schemas.ProviderPermit{Provider: "bedrock", AllowedModels: []string{"claude-sonnet-4-6"}, KeyIDs: []string{"aliased"}},
			schemas.ProviderPermit{Provider: "bedrock", AllowedModels: []string{"titan-*"}, KeyIDs: []string{"plain"}},
		)), "", nil).WithKeyScopedMatcher(matcher)
		assert.False(t, access.IsModelAllowed("bedrock", "best-model"), "the plain permit's key could serve the name as written")
		require.NotEmpty(t, seen)
		assert.ElementsMatch(t, []string{"aliased", "plain"}, seen[0])
	})
	t.Run("two restricted provider permits: a key neither allows does not veto the alias", func(t *testing.T) {
		// Keys a and b map the alias to an allowed model; key c has no alias, but no permit lets it serve.
		aliasedOnAB := func(_, model string, _, keyIDs []string) bool {
			if model != "best-model" || len(keyIDs) == 0 {
				return false
			}
			for _, id := range keyIDs {
				if id != "a" && id != "b" {
					return false
				}
			}
			return true
		}
		access := NewAccess(held(keyed(
			schemas.ProviderPermit{Provider: "bedrock", AllowedModels: []string{"claude-sonnet-4-6"}, KeyIDs: []string{"a"}},
			schemas.ProviderPermit{Provider: "bedrock", AllowedModels: []string{"claude-sonnet-4-6"}, KeyIDs: []string{"b"}},
		)), "", nil).WithKeyScopedMatcher(aliasedOnAB)
		assert.True(t, access.IsModelAllowed("bedrock", "best-model"))
		withC := NewAccess(held(keyed(
			schemas.ProviderPermit{Provider: "bedrock", AllowedModels: []string{"claude-sonnet-4-6"}, KeyIDs: []string{"a"}},
			schemas.ProviderPermit{Provider: "bedrock", AllowedModels: []string{"claude-sonnet-4-6"}, KeyIDs: []string{"b", "c"}},
		)), "", nil).WithKeyScopedMatcher(aliasedOnAB)
		assert.False(t, withC.IsModelAllowed("bedrock", "best-model"), "key c can serve the request and has no alias")
	})
	t.Run("an unrestricted provider permit among several: every key", func(t *testing.T) {
		seen = nil
		access := NewAccess(held(keyed(
			schemas.ProviderPermit{Provider: "bedrock", AllowedModels: []string{"claude-sonnet-4-6"}, KeyIDs: []string{"aliased"}},
			schemas.ProviderPermit{Provider: "bedrock", AllowedModels: []string{"titan-*"}, KeyIDs: []string{"*"}},
		)), "", nil).WithKeyScopedMatcher(matcher)
		assert.False(t, access.IsModelAllowed("bedrock", "best-model"))
		require.NotEmpty(t, seen)
		assert.Equal(t, []string{"*"}, seen[0])
	})
	t.Run("the key-scoped matcher replaces the plain matcher", func(t *testing.T) {
		never := func(string, string, []string) bool { return false }
		access := NewAccess(held(keyed(schemas.ProviderPermit{Provider: "bedrock", AllowedModels: []string{"claude-sonnet-4-6"}, KeyIDs: []string{"aliased"}})), "", never).WithKeyScopedMatcher(matcher)
		assert.True(t, access.IsModelAllowed("bedrock", "best-model"))
	})
	t.Run("the alias-target source is told the permit's keys", func(t *testing.T) {
		var got []string
		source := func(_, model string, keyIDs []string, visit func(string)) {
			got = keyIDs
			if slices.Contains(keyIDs, "*") || slices.Contains(keyIDs, "opus-key") {
				visit("claude-opus-4-6")
			}
		}
		pp := schemas.ProviderPermit{Provider: "bedrock", AllowedModels: []string{"regex:^claude-.*", "best-model"}, BlacklistedModels: []string{"claude-opus-4-6"}, KeyIDs: []string{"sonnet-key"}}
		assert.True(t, NewAccess(held(keyed(pp)), "", nil).WithAliasTargets(source).IsModelAllowed("bedrock", "best-model"),
			"the key mapping the name to the blacklisted model is not one the permit may use")
		assert.Equal(t, []string{"sonnet-key"}, got)
		pp.KeyIDs = []string{"*"}
		assert.False(t, NewAccess(held(keyed(pp)), "", nil).WithAliasTargets(source).IsModelAllowed("bedrock", "best-model"))
	})
}

func TestAccess_RestrictsKeys(t *testing.T) {
	all := permitWithModels(PermitVirtualKey, "vk1", "VK", "bedrock", "*")
	some := NewPermit(PermitVirtualKey, "vk2", "VK2", true, false, []schemas.ProviderPermit{{Provider: "bedrock", AllowedModels: []string{"*"}, KeyIDs: []string{"k1"}}})
	none := NewPermit(PermitVirtualKey, "vk3", "VK3", true, false, []schemas.ProviderPermit{{Provider: "bedrock", AllowedModels: []string{"*"}, KeyIDs: []string{}}})
	var nilAccess *Access
	assert.False(t, nilAccess.RestrictsKeys("bedrock"))
	assert.False(t, NewAccess(held(all), "", nil).RestrictsKeys("bedrock"))
	assert.False(t, NewAccess(held(some), "", nil).RestrictsKeys("openai"), "another provider's restriction does not count")
	assert.True(t, NewAccess(held(some), "", nil).RestrictsKeys("bedrock"))
	assert.True(t, NewAccess(held(none), "", nil).RestrictsKeys("bedrock"), "allowing no key is a restriction")
	cappingSome := atLevel(NewPermit(testTeamPermit, "t1", "Team", true, false, []schemas.ProviderPermit{{Provider: "bedrock", AllowedModels: []string{"*"}, KeyIDs: []string{"k1"}}}), 1)
	assert.True(t, NewAccess(held(all, cappingSome), Intersect, nil).RestrictsKeys("bedrock"), "a capping level counts")
	// Whenever it answers false, KeysForModel reports no restriction.
	_, restricted := NewAccess(held(all), "", nil).KeysForModel("bedrock", "any-model")
	assert.False(t, restricted)
}

func BenchmarkAccess_IsModelAllowed_AliasTargets(b *testing.B) {
	allowAll := func(string, string, []string) bool { return true }
	source := func(provider, model string, _ []string, visit func(string)) {
		if model == "best-model" {
			visit("claude-opus-4-6")
		}
	}
	plain := func() *Permit {
		return permitWithModels(PermitVirtualKey, "vk1", "VK", "bedrock", "regex:^claude-.*")
	}
	blocking := func() *Permit {
		return permitWithBlacklist(PermitVirtualKey, "vk1", "VK", "bedrock", []string{"regex:^claude-.*"}, "claude-haiku-4-5")
	}
	for _, bc := range []struct {
		name   string
		access *Access
	}{
		{"no_source", NewAccess(held(plain()), "", allowAll)},
		{"source_no_blacklist", NewAccess(held(plain()), "", allowAll).WithAliasTargets(source)},
		{"source_blacklist_alias", NewAccess(held(blocking()), "", allowAll).WithAliasTargets(source)},
	} {
		b.Run(bc.name, func(b *testing.B) {
			if !bc.access.IsModelAllowed("bedrock", "best-model") {
				b.Fatal("best-model should be allowed")
			}
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				bc.access.IsModelAllowed("bedrock", "best-model")
			}
		})
	}
}

// atLevel places a test permit at level, as a resolver does with WithLevel.
func atLevel(p *Permit, level int) *Permit {
	WithLevel(level)(p)
	return p
}

// ---------------------------------------------------------------------------
// Levels
// ---------------------------------------------------------------------------

// Under Intersect: own permits, then team/BU profiles, then customer profiles. The lowest level
// present grants and each level above caps.

func TestAccessLevels_UnionReadsEveryPermitAsOne(t *testing.T) {
	caller := permitWithModels(testCallerPermit, "ap1", "Member", "openai", "model-a")
	team := atLevel(permitWithModels(testTeamPermit, "t1", "Team", "openai", "model-b"), 1)
	customer := atLevel(permitWithModels(testCustomerPermit, "c1", "Customer", "openai", "model-c"), 2)

	for _, access := range []*Access{
		NewAccess(held(caller, team, customer), "", nil),
		NewAccess(held(caller, team, customer), Union, nil),
		// Anything other than Intersect reads every permit as one, a mode it does not know included.
		NewAccess(held(caller, team, customer), "something-new", nil),
	} {
		for _, model := range []string{"model-a", "model-b", "model-c"} {
			assert.True(t, access.IsModelAllowed("openai", model), "%s: every permit adds", model)
		}
		assertPermitsAre(t, access.Bases(), caller, team, customer)
	}
	assert.Equal(t, "", NewAccess(held(caller), "", nil).LevelMode())
	assert.Equal(t, "union", NewAccess(held(caller), Union, nil).LevelMode())
}

func TestAccessLevels_AHigherLevelTakesAwayWhatTheCallerHolds(t *testing.T) {
	// The caller holds models a and b; the level above allows only c. Nothing is left.
	caller := permitWithModels(testCallerPermit, "ap1", "Member", "openai", "model-a", "model-b")
	team := atLevel(permitWithModels(testTeamPermit, "t1", "Team", "openai", "model-c"), 1)

	access := NewAccess(held(caller, team), Intersect, nil)

	assert.False(t, access.IsModelAllowed("openai", "model-a"))
	assert.False(t, access.IsModelAllowed("openai", "model-b"))
	assert.False(t, access.IsModelAllowed("openai", "model-c"), "a capping level grants nothing of its own")
	assert.Empty(t, access.ProvidersForModel("model-c"), "a capping level never offers a candidate")
	assertPermitsAre(t, access.DeniedPermitsForModel("openai", "model-a"), team)
	assertPermitsAre(t, access.DeniedPermitsForModel("openai", "model-c"), caller)
}

// WithModelMatcher swaps how names are matched and nothing else: the copy keeps every level, so a
// literal-name check is capped exactly as the access it was made from. Rebuilding from Bases, which
// is only the granting level, would drop the cap and allow more than the access does.
func TestAccessLevels_WithModelMatcherKeepsTheLevels(t *testing.T) {
	caller := permitWithModels(testCallerPermit, "ap1", "Member", "openai", "model-a", "model-b")
	team := atLevel(permitWithModels(testTeamPermit, "t1", "Team", "openai", "model-a"), 1)
	byName := func(_, model string, allowed []string) bool { return schemas.WhiteList(allowed).IsAllowed(model) }

	access := NewAccess(held(caller, team), Intersect, byName)
	literal := access.WithModelMatcher(byName)

	for _, a := range []*Access{access, literal} {
		assert.True(t, a.IsModelAllowed("openai", "model-a"))
		assert.False(t, a.IsModelAllowed("openai", "model-b"), "the level above caps model-b")
	}
	assert.Equal(t, access.LevelMode(), literal.LevelMode())
	assert.Len(t, literal.Bases(), len(access.Bases()))
	assert.Nil(t, (*Access)(nil).WithModelMatcher(byName))
}

func TestAccessLevels_ALevelIsReadAsOne(t *testing.T) {
	caller := permitWithModels(testCallerPermit, "ap1", "Member", "openai", "model-a", "model-b")
	team := atLevel(permitWithModels(testTeamPermit, "t1", "Team", "openai", "model-a"), 1)
	unit := atLevel(permitWithModels(testUnitPermit, "b1", "Unit", "openai", "model-b"), 1)

	access := NewAccess(held(caller, team, unit), Intersect, nil)

	assert.True(t, access.IsModelAllowed("openai", "model-a"), "the team allows it")
	assert.True(t, access.IsModelAllowed("openai", "model-b"), "the business unit allows it: teams and units share a level")
	assertPermitsAre(t, access.PermitsForModel("openai", "model-a"), caller, team)
}

func TestAccessLevels_EveryLevelMustAllow(t *testing.T) {
	caller := permitWithModels(testCallerPermit, "ap1", "Member", "openai", "model-a", "model-b")
	team := atLevel(permitWithModels(testTeamPermit, "t1", "Team", "openai", "model-a", "model-b"), 1)
	customer := atLevel(permitWithModels(testCustomerPermit, "c1", "Customer", "openai", "model-b"), 2)

	access := NewAccess(held(caller, team, customer), Intersect, nil)

	assert.False(t, access.IsModelAllowed("openai", "model-a"))
	assert.True(t, access.IsModelAllowed("openai", "model-b"))
	assertPermitsAre(t, access.DeniedPermitsForModel("openai", "model-a"), customer)
	assertPermitsAre(t, access.PermitsForModel("openai", "model-b"), caller, team, customer)
}

func TestAccessLevels_TheLowestLevelPresentGrants(t *testing.T) {
	team := atLevel(permitWithModels(testTeamPermit, "t1", "Team", "openai", "model-a", "model-b"), 1)
	customer := atLevel(permitWithModels(testCustomerPermit, "c1", "Customer", "openai", "model-b", "model-c"), 2)

	t.Run("a caller holding nothing of their own is granted by their teams and capped above", func(t *testing.T) {
		access := NewAccess(held(team, customer), Intersect, nil)
		assert.False(t, access.IsModelAllowed("openai", "model-a"), "the customer caps it")
		assert.True(t, access.IsModelAllowed("openai", "model-b"))
		assert.False(t, access.IsModelAllowed("openai", "model-c"), "the customer grants nothing of its own")
		assertPermitsAre(t, access.Bases(), team)
	})

	t.Run("a lone level grants", func(t *testing.T) {
		access := NewAccess(held(customer), Intersect, nil)
		assert.True(t, access.IsModelAllowed("openai", "model-c"))
		assertPermitsAre(t, access.Bases(), customer)
	})

	t.Run("a permit with no level is the caller's own", func(t *testing.T) {
		other := permitWithModels(PermitType("something_else"), "x1", "Other", "openai", "model-a")
		access := NewAccess(held(other, team), Intersect, nil)
		assertPermitsAre(t, access.Bases(), other)
		assert.True(t, access.IsModelAllowed("openai", "model-a"))
	})
}

func TestAccessLevels_TheOrderPermitsArriveInDoesNotMatter(t *testing.T) {
	caller := permitWithModels(testCallerPermit, "ap1", "Member", "openai", "model-a", "model-b")
	team := atLevel(permitWithModels(testTeamPermit, "t1", "Team", "openai", "model-a", "model-b"), 1)
	customer := atLevel(permitWithModels(testCustomerPermit, "c1", "Customer", "openai", "model-b"), 2)

	ordered := NewAccess(held(caller, team, customer), Intersect, nil)
	shuffled := NewAccess(held(customer, caller, team), Intersect, nil)

	for _, model := range []string{"model-a", "model-b"} {
		assert.Equal(t, ordered.IsModelAllowed("openai", model), shuffled.IsModelAllowed("openai", model), model)
		assert.Equal(t, permitIDs(ordered.PermitsForModel("openai", model)), permitIDs(shuffled.PermitsForModel("openai", model)), model)
	}
	assertPermitsAre(t, shuffled.Bases(), caller)
}

func TestAccessLevels_ProvidersAndBlacklists(t *testing.T) {
	caller := permitWithProviders(testCallerPermit, "ap1", "Member", "openai", "anthropic")

	t.Run("a provider the level does not permit is refused", func(t *testing.T) {
		team := atLevel(permitWithProviders(testTeamPermit, "t1", "Team", "anthropic"), 1)
		access := NewAccess(held(caller, team), Intersect, nil)
		assert.False(t, access.IsProviderAllowed("openai"))
		assert.True(t, access.IsProviderAllowed("anthropic"))
		assert.False(t, access.AllowsEveryModel("openai"))
		assert.True(t, access.AllowsEveryModel("anthropic"))
		assert.Equal(t, []string{"anthropic"}, access.GrantedProvidersForModel("claude-sonnet-4"))
	})

	t.Run("a blacklist in the level blocks", func(t *testing.T) {
		team := atLevel(newPermit(permitSpec{
			Type: testTeamPermit, ID: "t1", Name: "Team", IsActive: true,
			ProviderPermits: []schemas.ProviderPermit{{
				Provider: "openai", AllowedModels: []string{"*"}, BlacklistedModels: []string{"gpt-4o"},
			}},
		}), 1)
		access := NewAccess(held(caller, team), Intersect, nil)
		assert.False(t, access.IsModelAllowed("openai", "gpt-4o"))
		assert.True(t, access.IsModelAllowed("openai", "gpt-4o-mini"))
		assert.Empty(t, access.GrantedProvidersForModel("gpt-4o"))
	})

	t.Run("a level that allows all providers caps nothing", func(t *testing.T) {
		team := atLevel(NewPermit(testTeamPermit, "t1", "Team", true, false, nil, WithAllowAllProviders(true)), 1)
		access := NewAccess(held(caller, team), Intersect, nil)
		assert.True(t, access.IsModelAllowed("openai", "gpt-4o"))
		assert.ElementsMatch(t, []string{"openai", "anthropic"}, access.GrantedProvidersForModel(""))
	})
}

func TestAccessLevels_KeysAreNarrowed(t *testing.T) {
	caller := newPermit(permitSpec{
		Type: testCallerPermit, ID: "ap1", Name: "Member", IsActive: true,
		ProviderPermits: []schemas.ProviderPermit{
			{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"key-a", "key-b"}},
			{Provider: "anthropic", AllowedModels: []string{"*"}, KeyIDs: []string{"*"}},
		},
	})
	team := atLevel(newPermit(permitSpec{
		Type: testTeamPermit, ID: "t1", Name: "Team", IsActive: true,
		ProviderPermits: []schemas.ProviderPermit{
			{Provider: "openai", AllowedModels: []string{"*"}, KeyIDs: []string{"key-b", "key-c"}},
			{Provider: "anthropic", AllowedModels: []string{"*"}, KeyIDs: []string{"key-team"}},
		},
	}), 1)
	customer := atLevel(NewPermit(testCustomerPermit, "c1", "Customer", true, false, nil, WithAllowAllProviders(true)), 2)

	access := NewAccess(held(caller, team, customer), Intersect, nil)

	keyIDs, restricted := access.KeysForModel("openai", "gpt-4o")
	assert.True(t, restricted)
	assert.Equal(t, []string{"key-b"}, keyIDs)

	keyIDs, restricted = access.KeysForModel("anthropic", "claude-sonnet-4")
	assert.True(t, restricted, "an unrestricted caller takes the level's restriction")
	assert.Equal(t, []string{"key-team"}, keyIDs)

	// Both providers are offered, each with its keys narrowed by the team's.
	candidates := access.ProvidersForModel("gpt-4o")
	require.Len(t, candidates, 2)
	assert.Equal(t, schemas.ProviderCandidate{Provider: "openai", KeyIDs: []string{"key-b"}}, candidates[0])
	assert.Equal(t, schemas.ProviderCandidate{Provider: "anthropic", KeyIDs: []string{"key-team"}}, candidates[1])
}

func TestAccessLevels_BasesAreTheGrantingLevel(t *testing.T) {
	caller := permitWithModels(testCallerPermit, "ap1", "Member", "openai", "model-a")
	team := atLevel(permitWithModels(testTeamPermit, "t1", "Team", "openai", "model-a"), 1)
	access := NewAccess(held(caller, team), Intersect, nil)

	assert.Equal(t, "intersect", access.LevelMode())
	bases := access.Bases()
	assertPermitsAre(t, bases, caller)
	bases[0] = nil
	assertPermitsAre(t, access.Bases(), caller)
}

// The matrix tries every shape with up to two own, two team and two customer permits, and checks
// each answer against an oracle written from the rule alone.

// matrixPermit is one permit of the matrix: whether it allows what is asked, and the keys it holds.
type matrixPermit struct {
	allows bool
	keys   []string
}

// build turns a matrix permit into a real one. A refusing permit holds the provider but grants
// something else.
func (m matrixPermit) build(kind PermitType, level int, id string) *Permit {
	model := "other-model"
	if m.allows {
		model = "gpt-4o"
	}
	return NewPermit(kind, id, id, true, false,
		[]schemas.ProviderPermit{{Provider: "openai", AllowedModels: []string{model}, KeyIDs: m.keys}},
		WithLevel(level))
}

// matrixLevelKinds is the permit kind at each matrix level.
var matrixLevelKinds = []PermitType{testCallerPermit, testTeamPermit, testCustomerPermit}

// matrixCase is one access shape: the permits at each level, some possibly empty.
type matrixCase struct {
	levels [3][]matrixPermit
}

// permitID names the permit at position i of level l.
func permitID(l, i int) string {
	return fmt.Sprintf("l%d-%d", l, i)
}

// name labels a case so a failure says which shape broke.
func (c matrixCase) name() string {
	v := func(ps []matrixPermit) string {
		out := ""
		for _, p := range ps {
			if p.allows {
				out += "A"
			} else {
				out += "D"
			}
		}
		if out == "" {
			return "-"
		}
		return out
	}
	return fmt.Sprintf("own=%s,team=%s,customer=%s", v(c.levels[0]), v(c.levels[1]), v(c.levels[2]))
}

// anyAllows reports whether any permit in ps allows what is asked.
func anyAllows(ps []matrixPermit) bool {
	return slices.ContainsFunc(ps, func(p matrixPermit) bool { return p.allows })
}

// present returns the levels that hold a permit, lowest first, with their indexes.
func (c matrixCase) present() (indexes []int, levels [][]matrixPermit) {
	for l, level := range c.levels {
		if len(level) > 0 {
			indexes = append(indexes, l)
			levels = append(levels, level)
		}
	}
	return indexes, levels
}

// oracle is the rule, written independently of the fold: some level holds a permit, and every level
// that does has one allowing.
func (c matrixCase) oracle() (allowed bool) {
	_, levels := c.present()
	allowed = len(levels) > 0
	for _, level := range levels {
		allowed = allowed && anyAllows(level)
	}
	return allowed
}

// oracleKeys is the expected key restriction: granting keys narrowed by each capping level.
func (c matrixCase) oracleKeys() []string {
	if !c.oracle() {
		return nil
	}
	allowingKeys := func(ps []matrixPermit) []string {
		var keys []string
		for _, p := range ps {
			if !p.allows {
				continue
			}
			for _, k := range p.keys {
				if !slices.Contains(keys, k) {
					keys = append(keys, k)
				}
			}
		}
		return keys
	}
	_, levels := c.present()
	keys := allowingKeys(levels[0])
	for _, level := range levels[1:] {
		levelKeys := allowingKeys(level)
		keys = slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return !slices.Contains(levelKeys, k) })
	}
	return keys
}

// oracleCandidateKeys is the expected keys of each candidate: one per allowing granting permit.
func (c matrixCase) oracleCandidateKeys() [][]string {
	if !c.oracle() {
		return nil
	}
	var candidates [][]string
	_, levels := c.present()
	for _, base := range levels[0] {
		if !base.allows {
			continue
		}
		keys := slices.Clone(base.keys)
		for _, level := range levels[1:] {
			var levelKeys []string
			for _, p := range level {
				if p.allows {
					levelKeys = append(levelKeys, p.keys...)
				}
			}
			keys = slices.DeleteFunc(keys, func(k string) bool { return !slices.Contains(levelKeys, k) })
		}
		candidates = append(candidates, keys)
	}
	return candidates
}

// matrixCases enumerates every shape.
func matrixCases() []matrixCase {
	a, d := matrixPermit{allows: true}, matrixPermit{}
	ownSets := [][]matrixPermit{{}, {a}, {d}, {a, a}, {a, d}, {d, d}}
	levelSets := [][]matrixPermit{{a}, {d}, {a, d}, {d, d}}
	// The levels above the caller: none, teams only, or teams and customers.
	var aboveSets [][2][]matrixPermit
	aboveSets = append(aboveSets, [2][]matrixPermit{})
	for _, teams := range levelSets {
		aboveSets = append(aboveSets, [2][]matrixPermit{teams, nil})
		for _, customers := range levelSets {
			aboveSets = append(aboveSets, [2][]matrixPermit{teams, customers})
		}
	}

	var cases []matrixCase
	for _, own := range ownSets {
		for _, above := range aboveSets {
			var c matrixCase
			// Keys overlap across levels, so narrowing keeps some and drops others.
			for l, level := range [3][]matrixPermit{own, above[0], above[1]} {
				for i, p := range level {
					p.keys = []string{permitID(0, i), permitID(l, i)}
					c.levels[l] = append(c.levels[l], p)
				}
			}
			cases = append(cases, c)
		}
	}
	return cases
}

// TestAccessLevelsMatrix checks every verdict of every shape against the oracle.
func TestAccessLevelsMatrix(t *testing.T) {
	cases := matrixCases()
	require.Len(t, cases, 6*21)
	for _, c := range cases {
		t.Run(c.name(), func(t *testing.T) {
			var permits []schemas.Permit
			for l, level := range c.levels {
				for i, p := range level {
					permits = append(permits, p.build(matrixLevelKinds[l], l, permitID(l, i)))
				}
			}
			access := NewAccess(permits, Intersect, nil)
			want := c.oracle()
			indexes, levels := c.present()

			// Every capped dimension answers to the same rule.
			assert.Equal(t, want, access.IsModelAllowed("openai", "gpt-4o"), "model")

			// The granting level is the lowest one present.
			granting := []string{}
			if len(levels) > 0 {
				for i := range levels[0] {
					granting = append(granting, permitID(indexes[0], i))
				}
			}
			assert.Equal(t, granting, permitIDs(access.Bases()), "bases")

			// A refusal names someone exactly when it's refused and someone holds a permit.
			denied := access.DeniedPermitsForModel("openai", "gpt-4o")
			assert.Equal(t, !want && len(permits) > 0, len(denied) > 0, "denied permits named: %v", permitIDs(denied))
			// A capping level that refused is named instead of the granting level.
			if !want && len(levels) > 0 && anyAllows(levels[0]) {
				var refusing []string
				for li, level := range levels[1:] {
					if !anyAllows(level) {
						for i := range level {
							refusing = append(refusing, permitID(indexes[li+1], i))
						}
					}
				}
				assert.Equal(t, refusing, permitIDs(denied), "the refusing levels are named")
			}

			// Attribution and candidates follow the verdict.
			attributed := access.PermitsForModel("openai", "gpt-4o")
			assert.Equal(t, want, len(attributed) > 0, "permits for the model")
			if want {
				for i, p := range levels[0] {
					assert.Equal(t, p.allows, slices.Contains(permitIDs(attributed), permitID(indexes[0], i)), "granting permit %d attributed", i)
				}
			}
			candidates := access.ProvidersForModel("gpt-4o")
			assert.Equal(t, want, len(candidates) > 0, "provider candidates")
			wantCandidates := c.oracleCandidateKeys()
			if assert.Len(t, candidates, len(wantCandidates), "one candidate per offering permit") {
				for i := range wantCandidates {
					assert.ElementsMatch(t, wantCandidates[i], candidates[i].KeyIDs, "candidate %d keys", i)
				}
			}
			granted := access.GrantedProvidersForModel("gpt-4o")
			assert.Equal(t, want, slices.Contains(granted, "openai"), "granted providers")

			// The key restriction is the rule's.
			keyIDs, restricted := access.KeysForModel("openai", "gpt-4o")
			if want {
				assert.True(t, restricted, "every permit restricts keys")
				assert.ElementsMatch(t, c.oracleKeys(), keyIDs, "keys")
			} else {
				assert.False(t, restricted)
				assert.Nil(t, keyIDs)
			}
		})
	}
}

// The caller's permits are read as one, so another of them may admit a model one permit blacklists;
// that permit must then not serve it, whether named literally or through an alias whose target it
// blacklists.
func TestAccess_CandidateHeldToItsPermitsBlacklistOfAliasTarget(t *testing.T) {
	allowAll := func(string, string, []string) bool { return true }
	bestToOpus := func(_, model string, _ []string, visit func(string)) {
		if model == "best-model" {
			visit("claude-opus-4-6")
		}
	}
	base := NewPermit(PermitVirtualKey, "vk1", "VK", true, false, []schemas.ProviderPermit{{
		Provider: "bedrock", AllowedModels: []string{"regex:^claude-.*"}, BlacklistedModels: []string{"claude-opus-4-6"}, KeyIDs: []string{"base-key"},
	}})
	other := NewPermit(PermitVirtualKey, "vk2", "Other", true, false, []schemas.ProviderPermit{{
		Provider: "bedrock", AllowedModels: []string{"*"}, KeyIDs: []string{"other-key"},
	}})
	access := NewAccess(held(base, other), "", allowAll).WithAliasTargets(bestToOpus)
	for _, model := range []string{"best-model", "claude-opus-4-6"} {
		assert.True(t, access.IsModelAllowed("bedrock", model), model)
		candidates := access.ProvidersForModel(model)
		require.NotEmpty(t, candidates, model)
		for _, c := range candidates {
			assert.NotContains(t, c.KeyIDs, "base-key", "%s must not be served through the permit that blacklists it", model)
		}
	}
}

// A capping level that blacklists the alias's target refuses the alias.
func TestAccessLevels_ACappingBlacklistHoldsAliasTarget(t *testing.T) {
	allowAll := func(string, string, []string) bool { return true }
	bestToOpus := func(_, model string, _ []string, visit func(string)) {
		if model == "best-model" {
			visit("claude-opus-4-6")
		}
	}
	base := permitWithModels(PermitVirtualKey, "vk1", "VK", "bedrock", "*")
	team := atLevel(permitWithBlacklist(testTeamPermit, "t1", "Team", "bedrock", []string{"regex:^claude-.*"}, "claude-opus-4-6"), 1)
	access := NewAccess(held(base, team), Intersect, allowAll).WithAliasTargets(bestToOpus)
	assert.False(t, access.IsModelAllowed("bedrock", "best-model"))
	assert.False(t, access.IsModelAllowed("bedrock", "claude-opus-4-6"))
	assertPermitsAre(t, access.DeniedPermitsForModel("bedrock", "best-model"), team)
	assert.True(t, NewAccess(held(base, team), Intersect, allowAll).IsModelAllowed("bedrock", "best-model"), "no source, no alias target")
}

func TestAccess_RestrictsKeysAllocatesNothing(t *testing.T) {
	access := NewAccess(held(permitWithModels(PermitVirtualKey, "vk1", "VK", "bedrock", "*")), "", nil)
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = access.RestrictsKeys("bedrock") }))
}

// permitIDs lists permit ids, in order.
func permitIDs(permits []schemas.Permit) []string {
	ids := make([]string, 0, len(permits))
	for _, p := range permits {
		ids = append(ids, p.ID())
	}
	return ids
}

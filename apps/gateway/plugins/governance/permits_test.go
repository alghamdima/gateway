package governance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/grant"
	"github.com/maximhq/bifrost/framework/tracing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The permit model and the fold are the framework's, and tested there. What this file covers is
// this package's side: turning a virtual key into the permit it confers, what funds that permit,
// and resolving what permits a request carries at all.

// permitWithProviders builds a permit allowing each provider for all models.
func permitWithProviders(permitType grant.PermitType, id, name string, providers ...string) *grant.Permit {
	providerPermits := make([]schemas.ProviderPermit, 0, len(providers))
	for _, provider := range providers {
		providerPermits = append(providerPermits, schemas.ProviderPermit{
			Provider:      provider,
			AllowedModels: []string{"*"},
			KeyIDs:        []string{"*"},
		})
	}
	return grant.NewPermit(permitType, id, name, true, false, providerPermits)
}

// ---------------------------------------------------------------------------
// the built-in virtual key source
// ---------------------------------------------------------------------------

// vkPermit builds the permit a key confers. The builder belongs to the store that owns the key
// data, so tests reach it through one.
func vkPermit(vk *configstoreTables.TableVirtualKey) *grant.Permit {
	store := &LocalGovernanceStore{inMemoryStore: &mockInMemoryStore{}}
	return store.permitForVirtualKey(emptyCtx(), vk)
}

// vkAccess is the access a request carrying only vk's permit has.
func vkAccess(vk *configstoreTables.TableVirtualKey) schemas.Access {
	return grant.NewAccess([]schemas.Permit{vkPermit(vk)}, "", nil)
}

// A key also answers to where it sits in the organization, so its holder limits carry its team's
// and customer's alongside its own. The limits come from the store's team and customer records
// rather than the key's preloaded relations, which carry only enough to say which team it is.
func TestPermitForVirtualKey_OrganizationLimits(t *testing.T) {
	teamRateLimit, customerRateLimit := "rl-team", "rl-customer"
	team := &configstoreTables.TableTeam{
		ID: "team-1", Name: "Platform",
		RateLimitID: &teamRateLimit,
		Budgets:     []configstoreTables.TableBudget{{ID: "budget-team"}},
	}
	customer := &configstoreTables.TableCustomer{
		ID: "cust-1", Name: "Acme",
		RateLimitID: &customerRateLimit,
		Budgets:     []configstoreTables.TableBudget{{ID: "budget-customer"}},
	}

	// What funds a key across every provider is answered by HolderLimits, keyed by the permit's
	// identity: none of it can tell one provider from another, so load balancing has no use for it.
	storeWith := func(vk *configstoreTables.TableVirtualKey) ([]schemas.Limit, []schemas.Limit) {
		gs := &LocalGovernanceStore{inMemoryStore: &mockInMemoryStore{}}
		gs.teams.Store(team.ID, team)
		gs.customers.Store(customer.ID, customer)
		gs.virtualKeysByID.Store(vk.ID, vk)
		ctx := emptyCtx()
		return gs.HolderLimits(ctx, gs.permitForVirtualKey(ctx, vk))
	}

	t.Run("a customer reached through the key's team", func(t *testing.T) {
		vk := buildVirtualKey("vk-1", "sk-bf-test", "Key", true)
		vk.Budgets = []configstoreTables.TableBudget{{ID: "budget-key"}}
		vk.TeamID = &team.ID
		vk.Team = &configstoreTables.TableTeam{ID: team.ID, Name: team.Name, CustomerID: &customer.ID}

		budgets, rateLimits := storeWith(vk)

		require.Len(t, budgets, 3, "the key's own, its team's, and the customer containing that team")
		assert.Equal(t, []string{"budget-key", "budget-team", "budget-customer"}, limitIDsOf(budgets))
		assert.Equal(t, []string{
			string(grant.LimitHolderVirtualKey), string(grant.LimitHolderTeam), string(grant.LimitHolderCustomer),
		}, holderKindsOf(budgets))
		assert.Equal(t, "Platform", budgets[1].HolderName, "a refusal has to be able to name the team")
		assert.Equal(t, []string{"rl-team", "rl-customer"}, limitIDsOf(rateLimits))

		// None of them name a provider: they govern every one, and answer for whichever serves.
		for _, budget := range budgets {
			assert.Empty(t, budget.Provider, budget.ID)
		}
	})

	t.Run("a key attached straight to a customer", func(t *testing.T) {
		vk := buildVirtualKey("vk-2", "sk-bf-direct", "Direct Key", true)
		vk.CustomerID = &customer.ID

		budgets, rateLimits := storeWith(vk)

		assert.Equal(t, []string{"budget-customer"}, limitIDsOf(budgets), "no team in the chain")
		assert.Equal(t, []string{"rl-customer"}, limitIDsOf(rateLimits))
	})

	t.Run("a key's own customer wins over its team's", func(t *testing.T) {
		other := &configstoreTables.TableCustomer{ID: "cust-2", Name: "Other", Budgets: []configstoreTables.TableBudget{{ID: "budget-other"}}}
		vk := buildVirtualKey("vk-3", "sk-bf-both", "Both Key", true)
		vk.CustomerID = &other.ID
		vk.TeamID = &team.ID
		vk.Team = &configstoreTables.TableTeam{ID: team.ID, Name: team.Name, CustomerID: &customer.ID}

		gs := &LocalGovernanceStore{inMemoryStore: &mockInMemoryStore{}}
		gs.teams.Store(team.ID, team)
		gs.customers.Store(customer.ID, customer)
		gs.customers.Store(other.ID, other)

		gs.virtualKeysByID.Store(vk.ID, vk)
		ctx := emptyCtx()
		budgets, _ := gs.HolderLimits(ctx, gs.permitForVirtualKey(ctx, vk))

		assert.Equal(t, []string{"budget-team", "budget-other"}, limitIDsOf(budgets),
			"the customer the key names, not the one its team belongs to")
	})

	t.Run("no organization above the key", func(t *testing.T) {
		vk := buildVirtualKey("vk-4", "sk-bf-lonely", "Lonely Key", true)
		vk.Budgets = []configstoreTables.TableBudget{{ID: "budget-key"}}

		budgets, _ := storeWith(vk)

		assert.Equal(t, []string{"budget-key"}, limitIDsOf(budgets))
	})

	t.Run("a team the store has never heard of contributes nothing", func(t *testing.T) {
		missing := "team-gone"
		vk := buildVirtualKey("vk-5", "sk-bf-stale", "Stale Key", true)
		vk.TeamID = &missing

		budgets, rateLimits := storeWith(vk)

		assert.Empty(t, budgets)
		assert.Empty(t, rateLimits)
	})

	t.Run("a permit that is not a key's is funded by nothing here", func(t *testing.T) {
		gs := &LocalGovernanceStore{inMemoryStore: &mockInMemoryStore{}}
		budgets, rateLimits := gs.HolderLimits(emptyCtx(), permitWithProviders("other", "o1", "Other", "openai"))
		assert.Empty(t, budgets)
		assert.Empty(t, rateLimits)
	})
}

func holderKindsOf(limits []schemas.Limit) []string {
	kinds := make([]string, 0, len(limits))
	for _, limit := range limits {
		kinds = append(kinds, limit.HolderKind)
	}
	return kinds
}

func TestPermitForVirtualKey_Identity(t *testing.T) {
	vk := buildVirtualKey("vk-1", "sk-bf-test", "Data Team Key", true)

	permit := vkPermit(vk)

	require.NotNil(t, permit)
	assert.Equal(t, string(grant.PermitVirtualKey), permit.Type())
	assert.Equal(t, "vk-1", permit.ID())
	assert.Equal(t, "Data Team Key", permit.Name(), "the display name a denial quotes")
	assert.True(t, permit.IsActive())
	assert.False(t, permit.IsExpired())

	assert.Nil(t, vkPermit(nil), "no key, no permit")
}

func TestPermitForVirtualKey_ProviderPermits(t *testing.T) {
	vk := buildVirtualKey("vk-1", "sk-bf-test", "Key", true)
	vk.ProviderConfigs = []configstoreTables.TableVirtualKeyProviderConfig{
		{
			Provider:          "openai",
			AllowedModels:     schemas.WhiteList{"gpt-4o"},
			BlacklistedModels: schemas.BlackList{"gpt-4o-mini"},
			AllowAllKeys:      true,
			Weight:            schemas.Ptr(0.7),
		},
		{
			Provider:      "anthropic",
			AllowedModels: schemas.WhiteList{"*"},
			Keys: []configstoreTables.TableKey{
				{KeyID: "key-a"},
				{KeyID: "key-b"},
			},
		},
		{
			// No allow-all flag and no keys: the key may use none of the provider's keys.
			Provider:      "bedrock",
			AllowedModels: schemas.WhiteList{"*"},
		},
	}

	permit := vkPermit(vk)
	require.Len(t, permit.ProviderPermits(), 3)

	openai := permit.ProviderPermits()[0]
	assert.Equal(t, "openai", openai.Provider)
	assert.Equal(t, schemas.WhiteList{"gpt-4o"}, openai.AllowedModels)
	assert.Equal(t, schemas.BlackList{"gpt-4o-mini"}, openai.BlacklistedModels)
	assert.Equal(t, schemas.WhiteList{"*"}, openai.KeyIDs, "allow-all becomes the wildcard")
	assert.Equal(t, schemas.Ptr(0.7), openai.Weight)

	anthropic := permit.ProviderPermits()[1]
	assert.Equal(t, schemas.WhiteList{"key-a", "key-b"}, anthropic.KeyIDs)
	assert.Nil(t, anthropic.Weight, "no weight configured stays no weight")

	bedrock := permit.ProviderPermits()[2]
	assert.NotNil(t, bedrock.KeyIDs, "an empty restriction is not the absence of one")
	assert.Empty(t, bedrock.KeyIDs)
}

// A key is governed at two levels, and neither travels on the permit: each provider config's
// limits, spent by what that config serves, and the key's own, spent whichever provider serves.
// Both are read from the store by the permit's identity, the first per provider so load balancing
// can tell one provider from another, the second once the attempt's provider is settled.
func TestPermitForVirtualKey_Limits(t *testing.T) {
	keyRateLimitID := "rl-key"
	openaiRateLimitID := "rl-openai"
	vk := buildVirtualKey("vk-1", "sk-bf-test", "Key", true)
	vk.RateLimitID = &keyRateLimitID
	vk.Budgets = []configstoreTables.TableBudget{{ID: "budget-key-daily"}, {ID: "budget-key-monthly"}}
	vk.ProviderConfigs = []configstoreTables.TableVirtualKeyProviderConfig{
		{
			ID:            7,
			Provider:      "openai",
			AllowedModels: schemas.WhiteList{"*"},
			RateLimitID:   &openaiRateLimitID,
			Budgets:       []configstoreTables.TableBudget{{ID: "budget-openai"}},
		},
		{
			// Governed by nothing of its own: the key's budget is what pays for it.
			ID:            8,
			Provider:      "anthropic",
			AllowedModels: schemas.WhiteList{"*"},
		},
	}

	gs := &LocalGovernanceStore{inMemoryStore: &mockInMemoryStore{}}
	gs.virtualKeysByID.Store(vk.ID, vk)
	ctx := emptyCtx()
	permit := gs.permitForVirtualKey(ctx, vk)

	// Each config's limits are answered for that provider, so which provider they answer for is
	// what was asked rather than something to filter on.
	openaiBudgets, openaiRateLimits := gs.PermitProviderLimits(ctx, permit, schemas.OpenAI)
	require.Len(t, openaiBudgets, 1, "the openai config's own")
	assert.Equal(t, schemas.Limit{
		ID: "budget-openai", HolderKind: string(grant.LimitHolderVirtualKeyProviderConfig),
		HolderID: "7", HolderName: "Key", Provider: "openai",
	}, openaiBudgets[0])
	assert.Equal(t, []string{"rl-openai"}, limitIDsOf(openaiRateLimits))

	anthropicBudgets, anthropicRateLimits := gs.PermitProviderLimits(ctx, permit, schemas.Anthropic)
	assert.Empty(t, anthropicBudgets, "a config governed by nothing of its own")
	assert.Empty(t, anthropicRateLimits)

	noProviderBudgets, _ := gs.PermitProviderLimits(ctx, permit, "")
	assert.Empty(t, noProviderBudgets, "and there is no unscoped bucket to find the key's own in")

	// The key's own limits are HolderLimits' to answer.
	heldBudgets, heldRateLimits := gs.HolderLimits(ctx, permit)
	assert.Equal(t, []string{"budget-key-daily", "budget-key-monthly"}, limitIDsOf(heldBudgets))
	assert.Equal(t, []string{"rl-key"}, limitIDsOf(heldRateLimits))

	// A permit describes what may be reached and nothing else, so it carries no limit at all.
	for _, pp := range permit.ProviderPermits() {
		assert.Equal(t, schemas.ProviderPermit{
			Provider: pp.Provider, AllowedModels: pp.AllowedModels, BlacklistedModels: pp.BlacklistedModels,
			KeyIDs: pp.KeyIDs, Weight: pp.Weight,
		}, pp)
	}
}

// A key may hold two configs for one provider, governed separately: nothing makes a provider
// unique within a key. Asking about that provider answers for both, and what keeps them distinct
// is the holder each names.
func TestPermitForVirtualKey_LimitsOfTwoConfigsForOneProvider(t *testing.T) {
	firstRateLimit, secondRateLimit := "rl-first", "rl-second"
	vk := buildVirtualKey("vk-1", "sk-bf-test", "Key", true)
	vk.ProviderConfigs = []configstoreTables.TableVirtualKeyProviderConfig{
		{
			ID: 1, Provider: "openai", AllowedModels: schemas.WhiteList{"gpt-4o"},
			RateLimitID: &firstRateLimit,
			Budgets:     []configstoreTables.TableBudget{{ID: "budget-first"}},
		},
		{
			ID: 2, Provider: "openai", AllowedModels: schemas.WhiteList{"o3"},
			RateLimitID: &secondRateLimit,
			Budgets:     []configstoreTables.TableBudget{{ID: "budget-second"}},
		},
	}

	gs := &LocalGovernanceStore{inMemoryStore: &mockInMemoryStore{}}
	gs.virtualKeysByID.Store(vk.ID, vk)
	ctx := emptyCtx()
	permit := gs.permitForVirtualKey(ctx, vk)

	budgets, rateLimits := gs.PermitProviderLimits(ctx, permit, schemas.OpenAI)
	require.Len(t, budgets, 2, "both configs govern openai traffic")
	assert.Equal(t, []string{"budget-first", "budget-second"}, []string{budgets[0].ID, budgets[1].ID})
	assert.Equal(t, []string{"1", "2"}, []string{budgets[0].HolderID, budgets[1].HolderID},
		"the config each came from, which is what tells two limits on one provider apart")

	require.Len(t, rateLimits, 2)
	assert.Equal(t, []string{"rl-first", "rl-second"}, []string{rateLimits[0].ID, rateLimits[1].ID})
}

// ---------------------------------------------------------------------------
// equivalence with the existing virtual-key walkers
// ---------------------------------------------------------------------------

func TestPermitForVirtualKey_ProviderAndModelRules(t *testing.T) {
	// The rules the key's own permit must keep: a provider with no config is not permitted, a
	// configured provider is, a model must be in the allowlist, and a blacklisted model is refused
	// however permissive the allowlist.
	vk := buildVirtualKey("vk-1", "sk-bf-test", "Key", true)
	vk.ProviderConfigs = []configstoreTables.TableVirtualKeyProviderConfig{
		{Provider: "openai", AllowedModels: schemas.WhiteList{"gpt-4o", "o3"}, BlacklistedModels: schemas.BlackList{"o3"}},
		{Provider: "anthropic", AllowedModels: schemas.WhiteList{"*"}},
		{Provider: "bedrock", AllowedModels: schemas.WhiteList{}},
	}
	access := vkAccess(vk)

	assert.True(t, access.IsProviderAllowed("openai"))
	assert.True(t, access.IsProviderAllowed("anthropic"))
	assert.True(t, access.IsProviderAllowed("bedrock"), "configured, even with no model allowed")
	assert.False(t, access.IsProviderAllowed("cohere"), "not configured at all")

	assert.True(t, access.IsModelAllowed("openai", "gpt-4o"))
	assert.False(t, access.IsModelAllowed("openai", "o3"), "blacklisted wins over the allowlist")
	assert.False(t, access.IsModelAllowed("openai", "gpt-4o-mini"), "not in the allowlist")
	assert.True(t, access.IsModelAllowed("anthropic", "claude-sonnet-4"), "wildcard allows any model")
	assert.False(t, access.IsModelAllowed("bedrock", "anything"), "empty allowlist permits nothing")
	assert.False(t, access.IsModelAllowed("cohere", "command-r"))

	// A key with no provider config at all permits nothing: deny by default.
	bare := buildVirtualKey("vk-2", "sk-bf-bare", "Bare Key", true)
	bareAccess := vkAccess(bare)
	assert.False(t, bareAccess.IsProviderAllowed("openai"))
	assert.False(t, bareAccess.IsModelAllowed("openai", "gpt-4o"))
}

func TestPermitForVirtualKey_KeysForModel(t *testing.T) {
	// Two configs for one provider are read as one: a request served by that provider may use any
	// key either config allows.
	vk := buildVirtualKey("vk-1", "sk-bf-test", "Key", true)
	vk.ProviderConfigs = []configstoreTables.TableVirtualKeyProviderConfig{
		{Provider: "openai", AllowedModels: schemas.WhiteList{"*"}, Keys: []configstoreTables.TableKey{{KeyID: "key-us-1"}}},
		{Provider: "openai", AllowedModels: schemas.WhiteList{"*"}, Keys: []configstoreTables.TableKey{{KeyID: "key-us-2"}}},
		{Provider: "anthropic", AllowedModels: schemas.WhiteList{"*"}, AllowAllKeys: true},
	}
	access := vkAccess(vk)

	keyIDs, restricted := access.KeysForModel("openai", "gpt-4o")
	assert.True(t, restricted)
	assert.Equal(t, []string{"key-us-1", "key-us-2"}, keyIDs, "every config for the provider has a say")

	keyIDs, restricted = access.KeysForModel("anthropic", "claude-sonnet-4")
	assert.False(t, restricted, "allow-all stamps no restriction")
	assert.Nil(t, keyIDs)
}

// ---------------------------------------------------------------------------
// per-request resolution
// ---------------------------------------------------------------------------

// permitStore stands in for a store that resolves permit holders beyond virtual keys. It records
// how often it was asked, and can replace the caller's side to stand for a caller whose permits do
// not come from a key.
type permitStore struct {
	GovernanceStore
	baseOverride schemas.Permit
	// bases replaces the caller's side with several permits at once, for a caller that holds more
	// than one. It wins over baseOverride when both are set.
	bases []schemas.Permit
	// above is appended to the store's bases; levelMode is returned as is.
	above     []schemas.Permit
	levelMode grant.CompositionMode
	// resolvesNothing makes the store answer with no permit at all, as a store that resolves callers
	// beyond keys does when the caller it was asked about has no access configured.
	resolvesNothing bool
	calls           int
}

func (s *permitStore) ResolvePermits(ctx *schemas.BifrostContext) ([]schemas.Permit, grant.CompositionMode) {
	s.calls++
	if s.resolvesNothing {
		return nil, ""
	}
	var bases []schemas.Permit
	switch {
	case len(s.bases) > 0:
		bases = s.bases
	case s.baseOverride != nil:
		bases = []schemas.Permit{s.baseOverride}
	default:
		bases, _ = s.GovernanceStore.ResolvePermits(ctx)
	}
	return append(bases, s.above...), s.levelMode
}

// newAccessTestPlugin builds a plugin over a store that serves vk, optionally wrapped so the
// store composes further permits onto every request.
func newAccessTestPlugin(t testing.TB, vk *configstoreTables.TableVirtualKey, wrap *permitStore) *GovernancePlugin {
	t.Helper()
	logger := NewMockLogger()
	local, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
	}, nil, &mockInMemoryStore{})
	require.NoError(t, err)

	var store GovernanceStore = local
	if wrap != nil {
		wrap.GovernanceStore = local
		store = wrap
	}

	plugin, err := InitFromStore(context.Background(), &Config{IsVkMandatory: boolPtr(false)},
		logger, store, nil, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })
	return plugin
}

func TestResolveAccess(t *testing.T) {
	vk := buildAccessTestVK()

	t.Run("a store that knows only keys answers with the key's own permit", func(t *testing.T) {
		plugin := newAccessTestPlugin(t, vk, nil)

		access, err := plugin.ResolveAccess(newPreRequestCtx())

		require.NoError(t, err)
		require.NotNil(t, access)
		assert.Equal(t, "", access.LevelMode())
		require.Len(t, access.Bases(), 1)
		assert.Equal(t, vk.ID, access.Bases()[0].ID())
		assert.Equal(t, string(grant.PermitVirtualKey), access.Bases()[0].Type())
		assert.True(t, access.IsModelAllowed("openai", "gpt-4o"))
	})

	t.Run("a further permit the store answers with is folded in", func(t *testing.T) {
		store := &permitStore{above: []schemas.Permit{permitWithProviders("other", "o1", "Other", "anthropic")}}
		plugin := newAccessTestPlugin(t, vk, store)

		access, err := plugin.ResolveAccess(newPreRequestCtx())

		require.NoError(t, err)
		require.NotNil(t, access)
		assert.Equal(t, 1, store.calls, "asked once per resolution")
		assert.True(t, access.IsProviderAllowed("openai"), "the key's own provider")
		assert.True(t, access.IsProviderAllowed("anthropic"), "the further permit's provider")
	})

	t.Run("a store answering with intersecting levels caps the caller's permits", func(t *testing.T) {
		// The key allows every openai model and the team only one, so only that one is left.
		team := grant.NewPermit(testTeamLevelPermit, "t1", "Team", true, false, []schemas.ProviderPermit{{
			Provider: "openai", AllowedModels: []string{"gpt-4o-mini"}, KeyIDs: []string{"*"},
		}}, grant.WithLevel(1))
		store := &permitStore{above: []schemas.Permit{team}, levelMode: grant.Intersect}
		plugin := newAccessTestPlugin(t, vk, store)

		access, err := plugin.ResolveAccess(newPreRequestCtx())

		require.NoError(t, err)
		require.NotNil(t, access)
		assert.Equal(t, string(grant.Intersect), access.LevelMode())
		require.Len(t, access.Bases(), 1, "the key grants; the team level only caps it")
		assert.True(t, access.IsModelAllowed("openai", "gpt-4o-mini"))
		assert.False(t, access.IsModelAllowed("openai", "gpt-4o"))
		denied := access.DeniedPermitsForModel("openai", "gpt-4o")
		require.Len(t, denied, 1)
		assert.Equal(t, "t1", denied[0].ID())
	})

	t.Run("the store may answer for a caller whose permits are not a key's", func(t *testing.T) {
		// The caller's side is the store's to decide: a caller can hold a permit without holding a
		// key, and such a request must still resolve to real access.
		store := &permitStore{baseOverride: permitWithProviders("other", "u1", "Someone", "bedrock")}
		plugin := newAccessTestPlugin(t, vk, store)

		// No key on the request at all: the store still answers for the caller.
		access, err := plugin.ResolveAccess(emptyCtx())

		require.NoError(t, err)
		require.NotNil(t, access)
		require.Len(t, access.Bases(), 1)
		assert.Equal(t, "u1", access.Bases()[0].ID())
		assert.True(t, access.IsProviderAllowed("bedrock"))
	})

	t.Run("a request presenting nothing resolves to no access", func(t *testing.T) {
		plugin := newAccessTestPlugin(t, vk, nil)
		ctx := emptyCtx()

		// Nobody granted it anything, and it is restricted by nothing. That is no access at all,
		// which every consumer reads as unrestricted, rather than an access that permits everything.
		access, err := plugin.ResolveAccess(ctx)

		require.NoError(t, err)
		assert.Nil(t, access)
		assert.Nil(t, ctx.Grant().Access(), "and nothing is recorded")
	})

	t.Run("a context carrying no grant is a wiring fault", func(t *testing.T) {
		// The transport installs a grant on every request context. One without is not a request
		// nothing governs; it is a request nothing settled, and resolution says so instead of
		// installing a grant nobody settled an identity on.
		plugin := newAccessTestPlugin(t, vk, nil)

		access, err := plugin.ResolveAccess(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline))

		assert.Error(t, err)
		assert.Nil(t, access)
	})
}

// Resolving happens once per request. A second caller finds the answer already recorded on the
// grant and gets the same object back, without the store being asked again, which is what lets
// every path that might be the first to see a request call this unconditionally.
func TestResolveAccessResolvesOnce(t *testing.T) {
	vk := buildAccessTestVK()
	store := &permitStore{}
	plugin := newAccessTestPlugin(t, vk, store)
	ctx := newPreRequestCtx()

	first, err := plugin.ResolveAccess(ctx)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, 1, store.calls)

	second, err := plugin.ResolveAccess(ctx)

	require.NoError(t, err)
	assert.Same(t, first, second, "the recorded answer, not a freshly folded one")
	assert.Equal(t, 1, store.calls, "the store is asked once per request")
	assert.Same(t, first, ctx.Grant().Access(), "and it is what everything downstream reads")
}

// Once per request, not once per attempt. What a request may reach is a fact about the caller, and
// a request that fails over changes its provider, not its caller: the next attempt reads the same
// access and settles only its own limits again (see TestResolveLimits).
func TestResolveAccessHoldsAcrossAttempts(t *testing.T) {
	vk := buildAccessTestVK()
	store := &permitStore{}
	plugin := newAccessTestPlugin(t, vk, store)
	ctx := newPreRequestCtx()

	first, err := plugin.ResolveAccess(ctx)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.False(t, first.IsProviderAllowed("anthropic"))

	// A permit widened while the first attempt was in flight, and the next attempt starting.
	store.above = []schemas.Permit{permitWithProviders("other", "o1", "Other", "anthropic")}
	resolveLimits(ctx, store, schemas.Anthropic, "claude-sonnet-4")

	second, err := plugin.ResolveAccess(ctx)

	require.NoError(t, err)
	assert.Same(t, first, second, "the access the request was admitted under is what every attempt reads")
	assert.Equal(t, 1, store.calls)
	assert.False(t, second.IsProviderAllowed("anthropic"), "configuration that changed mid-request is picked up by the next request")
}

// A request whose presented credential resolves to nothing records nothing, so it stays
// indistinguishable from one nobody has resolved yet. Asking repeatedly re-asks the store, which is
// the price of not caching a negative.
func TestResolveAccessRecordsNothingForAnUnresolvableCredential(t *testing.T) {
	vk := buildAccessTestVK()
	store := &permitStore{}
	plugin := newAccessTestPlugin(t, vk, store)
	ctx := presentCtx("sk-bf-unknown")

	access, err := plugin.ResolveAccess(ctx)
	require.NoError(t, err)
	assert.Nil(t, access)
	assert.Nil(t, ctx.Grant().Access(), "nothing resolved is not access permitting nothing")

	access, err = plugin.ResolveAccess(ctx)
	require.NoError(t, err)
	assert.Nil(t, access)
	assert.Equal(t, 2, store.calls)
}

// Resolving the access is what completes the request's identity: the key it presented resolves to
// a row, and the team and customer above it are recorded where everything downstream reads them.
func TestResolveAccessCompletesTheIdentity(t *testing.T) {
	customer := buildCustomer("cust-1", "Acme", nil)
	team := buildTeam("team-1", "Platform", nil)
	team.CustomerID = &customer.ID
	team.Customer = customer
	vk := buildAccessTestVK()
	vk.TeamID = &team.ID
	vk.Team = team

	logger := NewMockLogger()
	local, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
		Teams:       []configstoreTables.TableTeam{*team},
		Customers:   []configstoreTables.TableCustomer{*customer},
	}, nil, &mockInMemoryStore{})
	require.NoError(t, err)
	plugin, err := InitFromStore(context.Background(), &Config{IsVkMandatory: boolPtr(false)}, logger, local, nil, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })

	// As the transport leaves it: the credential settled, nothing resolved.
	ctx := emptyCtx()
	ctx.Grant().SetIdentity(grant.NewIdentity(grant.NewCredential(grant.CredentialVirtualKey, accessTestVKValue), nil, nil, nil, nil))

	_, err = plugin.ResolveAccess(ctx)
	require.NoError(t, err)

	identity := ctx.Grant().Identity()
	require.NotNil(t, identity)
	assert.Equal(t, grant.NewCredential(grant.CredentialVirtualKey, accessTestVKValue), identity.Credential(), "what was presented is kept")
	require.NotNil(t, identity.VirtualKey())
	assert.Equal(t, vk.ID, identity.VirtualKey().ID)
	assert.Equal(t, vk.Name, identity.VirtualKey().Name)
	assert.Equal(t, []schemas.EntityRef{{ID: "team-1", Name: "Platform"}}, identity.Teams())
	assert.Equal(t, []schemas.EntityRef{{ID: "cust-1", Name: "Acme"}}, identity.Customers())

	// And the keys everything that predates the identity still reads.
	assert.Equal(t, vk.ID, ctx.Value(schemas.BifrostContextKeyGovernanceVirtualKeyID))
	assert.Equal(t, "team-1", ctx.Value(schemas.BifrostContextKeyGovernanceTeamID))
	assert.Equal(t, "cust-1", ctx.Value(schemas.BifrostContextKeyGovernanceCustomerID))
}

// Resolving access publishes the key's content-logging decision for the logging plugin to read:
// true and false are each stamped as said, and a key that never said anything stamps nothing, so
// the plugin falls through to the client setting rather than seeing a false it must treat as a
// decision.
func TestResolveAccessStampsContentLoggingDecision(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision *bool
	}{
		{name: "inherit stamps nothing", decision: nil},
		{name: "off is stamped as true", decision: new(true)},
		{name: "on is stamped as false", decision: new(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vk := buildAccessTestVK()
			vk.DisableContentLogging = tc.decision

			logger := NewMockLogger()
			local, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
				VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
			}, nil, &mockInMemoryStore{})
			require.NoError(t, err)
			plugin, err := InitFromStore(context.Background(), &Config{IsVkMandatory: boolPtr(false)}, logger, local, nil, nil, nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })

			ctx := emptyCtx()
			ctx.Grant().SetIdentity(grant.NewIdentity(grant.NewCredential(grant.CredentialVirtualKey, accessTestVKValue), nil, nil, nil, nil))

			_, err = plugin.ResolveAccess(ctx)
			require.NoError(t, err)
			require.Equal(t, vk.ID, ctx.Value(schemas.BifrostContextKeyGovernanceVirtualKeyID), "the key resolved")

			stamped := ctx.Value(schemas.BifrostContextKeyGovernanceDisableContentLogging)
			if tc.decision == nil {
				assert.Nil(t, stamped, "inherit must leave the key absent, not stamp false")
				return
			}
			assert.Equal(t, *tc.decision, stamped)
		})
	}
}

// A key that turns content off also marks the request's root span, which is how connectors that
// never see the request context (they are handed the finished trace) learn to strip content. The
// mark is one-directional: a key that keeps content on, or inherits, leaves the span alone so it
// can never loosen a connector's own disable_content_logging.
func TestResolveAccessMarksTraceWhenContentLoggingOff(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision *bool
		marked   bool
	}{
		{name: "off marks the root span", decision: new(true), marked: true},
		{name: "on leaves the root span alone", decision: new(false), marked: false},
		{name: "inherit leaves the root span alone", decision: nil, marked: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vk := buildAccessTestVK()
			vk.DisableContentLogging = tc.decision

			logger := NewMockLogger()
			local, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
				VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
			}, nil, &mockInMemoryStore{})
			require.NoError(t, err)
			plugin, err := InitFromStore(context.Background(), &Config{IsVkMandatory: boolPtr(false)}, logger, local, nil, nil, nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })

			// A real tracer with a root span already open, as the HTTP tracing middleware leaves it
			// before any handler runs.
			traceStore := tracing.NewTraceStore(time.Minute, nil)
			t.Cleanup(traceStore.Stop)
			tracer := tracing.NewTracer(traceStore, nil, nil)
			t.Cleanup(tracer.Stop)
			traceID := tracer.CreateTrace("")
			ctx := emptyCtx()
			ctx.SetValue(schemas.BifrostContextKeyTracer, tracer)
			ctx.SetValue(schemas.BifrostContextKeyTraceID, traceID)
			_, handle := tracer.StartSpanID(ctx, "request", schemas.SpanKindHTTPRequest)
			require.NotNil(t, handle, "root span must open for the trace")
			ctx.Grant().SetIdentity(grant.NewIdentity(grant.NewCredential(grant.CredentialVirtualKey, accessTestVKValue), nil, nil, nil, nil))

			_, err = plugin.ResolveAccess(ctx)
			require.NoError(t, err)
			require.Equal(t, vk.ID, ctx.Value(schemas.BifrostContextKeyGovernanceVirtualKeyID), "the key resolved")

			trace := traceStore.GetTrace(traceID)
			require.NotNil(t, trace)
			require.NotNil(t, trace.RootSpan)
			assert.Equal(t, tc.marked, schemas.ContentLoggingDisabledForTrace(trace))
			if !tc.marked {
				_, present := trace.RootSpan.Attributes[schemas.AttrBifrostContentLoggingDisabled]
				assert.False(t, present, "the mark is never written as false")
			}
		})
	}
}

// The key a request presented is read off the identity the transport settled, and only falls back
// to the context key for a context nothing settled an identity on.
func TestPresentedVirtualKey(t *testing.T) {
	t.Run("an identity that presented nothing presented no key", func(t *testing.T) {
		// Whatever the context carries under the key's own name, the transport settled that nothing
		// was presented, and that is what the request goes by.
		ctx := emptyCtx()
		ctx.SetValue(schemas.BifrostContextKeyVirtualKey, "sk-bf-stamped")
		assert.Equal(t, "", PresentedVirtualKey(ctx))
	})

	t.Run("an identity that presented a key answers with it", func(t *testing.T) {
		ctx := emptyCtx()
		ctx.SetValue(schemas.BifrostContextKeyVirtualKey, "sk-bf-stale")
		ctx.Grant().SetIdentity(grant.NewIdentity(grant.NewCredential(grant.CredentialVirtualKey, "sk-bf-settled"), nil, nil, nil, nil))
		assert.Equal(t, "sk-bf-settled", PresentedVirtualKey(ctx), "what the transport settled, not what else the context carries")
	})

	t.Run("an identity that presented something else presented no key", func(t *testing.T) {
		ctx := emptyCtx()
		ctx.SetValue(schemas.BifrostContextKeyVirtualKey, "sk-bf-stale")
		ctx.Grant().SetIdentity(grant.NewIdentity(grant.NewCredential(grant.CredentialSessionToken, "u1"), &schemas.UserRef{ID: "u1"}, nil, nil, nil))
		assert.Equal(t, "", PresentedVirtualKey(ctx))
	})
}

func TestPreRequestHookRecordsAccess(t *testing.T) {
	vk := buildAccessTestVK()
	store := &permitStore{above: []schemas.Permit{permitWithProviders("other", "o1", "Other", "anthropic")}}
	plugin := newAccessTestPlugin(t, vk, store)
	ctx := newPreRequestCtx()

	require.NoError(t, plugin.PreRequestHook(ctx, newChatRequest()))

	access := ctx.Grant().Access()
	require.NotNil(t, access, "the hook resolves access for every request carrying a key")
	require.Len(t, access.Bases(), 2)
	assert.Equal(t, vk.ID, access.Bases()[0].ID())
	assert.True(t, access.IsModelAllowed("openai", "gpt-4o"))
	assert.True(t, access.IsProviderAllowed("anthropic"), "the store's contribution reached the hook")
}

func TestPreRequestHookWithoutACredentialRecordsNothing(t *testing.T) {
	vk := buildAccessTestVK()
	plugin := newAccessTestPlugin(t, vk, nil)
	ctx := emptyCtx()

	require.NoError(t, plugin.PreRequestHook(ctx, newChatRequest()))

	// Nothing granted it anything, so nothing restricts it: no access is recorded, and every
	// consumer reads that absence as unrestricted.
	assert.Nil(t, ctx.Grant().Access())

	// And nothing narrowed its tools to the empty set on the way through.
}

func TestPreRequestHookWithoutAGrantReportsTheFault(t *testing.T) {
	vk := buildAccessTestVK()
	plugin := newAccessTestPlugin(t, vk, nil)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	err := plugin.PreRequestHook(ctx, newChatRequest())

	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// hot-path cost
// ---------------------------------------------------------------------------

// benchVK is a key of a realistic size: several providers.
func benchVK() *configstoreTables.TableVirtualKey {
	vk := buildVirtualKey("vk-bench", "sk-bf-bench", "bench-key", true)
	for _, provider := range []string{"openai", "anthropic", "bedrock", "vertex", "groq"} {
		vk.ProviderConfigs = append(vk.ProviderConfigs, configstoreTables.TableVirtualKeyProviderConfig{
			Provider:      provider,
			AllowedModels: schemas.WhiteList{"gpt-4o", "claude-sonnet-4", "o3"},
			AllowAllKeys:  true,
			Weight:        schemas.Ptr(1.0),
		})
	}
	return vk
}

func benchStore(b *testing.B, vk *configstoreTables.TableVirtualKey) *LocalGovernanceStore {
	b.Helper()
	store, err := NewLocalGovernanceStore(context.Background(), NewMockLogger(), nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
	}, nil, &mockInMemoryStore{})
	if err != nil {
		b.Fatal(err)
	}
	return store
}

func benchPlugin(b *testing.B, store *LocalGovernanceStore) *GovernancePlugin {
	b.Helper()
	plugin, err := InitFromStore(context.Background(), &Config{IsVkMandatory: boolPtr(false)},
		NewMockLogger(), store, nil, nil, store.inMemoryStore)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := plugin.Cleanup(); err != nil {
			b.Fatal(err)
		}
	})
	return plugin
}

// benchCtx carries only the key on a grant nothing has resolved into: what resolution costs is
// exactly what these benchmarks measure, so the access must not already be on the context.
func benchCtx(_ *configstoreTables.TableVirtualKey) *schemas.BifrostContext {
	ctx := presentCtx("sk-bf-bench")
	return ctx
}

// BenchmarkResolveAccess is what a request pays once, up front, to resolve access.
func BenchmarkResolveAccess(b *testing.B) {
	vk := benchVK()
	plugin := benchPlugin(b, benchStore(b, vk))

	b.ReportAllocs()
	for b.Loop() {
		if access, err := plugin.ResolveAccess(benchCtx(vk)); err != nil || access == nil {
			b.Fatal("no access resolved")
		}
	}
}

// BenchmarkAccessChecks is the same questions answered off already-resolved access, which is what
// every call site pays per attempt once they read it instead of the key.
func BenchmarkAccessChecks(b *testing.B) {
	vk := benchVK()
	plugin := benchPlugin(b, benchStore(b, vk))
	access, err := plugin.ResolveAccess(benchCtx(vk))
	if err != nil || access == nil {
		b.Fatal("no access resolved")
	}

	b.ReportAllocs()
	for b.Loop() {
		_ = access.IsProviderAllowed("groq")
		_ = access.IsModelAllowed("groq", "o3")
	}
}

// filterModelsForAccess is what a virtual-key caller sees in a models listing. It answers from
// the same access the request path enforces, so the listing cannot advertise a model the very
// next request would refuse.
func TestFilterModelsForAccess(t *testing.T) {
	vk := buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Test VK", []configstoreTables.TableVirtualKeyProviderConfig{
		{Provider: "openai", AllowedModels: schemas.WhiteList{"gpt-4o"}},
		{Provider: "anthropic", AllowedModels: schemas.WhiteList{"*"}, BlacklistedModels: schemas.BlackList{"claude-3-haiku"}},
	})
	plugin := newAccessTestPlugin(t, vk, nil)

	models := []schemas.Model{
		{ID: "openai/gpt-4o"},
		{ID: "openai/gpt-4o-mini"},
		{ID: "anthropic/claude-3-5-sonnet"},
		{ID: "anthropic/claude-3-haiku"},
		{ID: "bedrock/nova-pro"},
	}

	t.Run("keeps only what the request may use", func(t *testing.T) {
		ctx := presentCtx("sk-bf-test")

		access, err := plugin.ResolveAccess(ctx)
		require.NoError(t, err)
		filtered := plugin.filterModelsForAccess(access, models)

		ids := make([]string, 0, len(filtered))
		for _, model := range filtered {
			ids = append(ids, model.ID)
		}
		// gpt-4o-mini is outside the allowlist, claude-3-haiku is blacklisted, and bedrock is
		// not granted at all.
		assert.Equal(t, []string{"openai/gpt-4o", "anthropic/claude-3-5-sonnet"}, ids)
	})

	t.Run("a credential that resolves to nothing lists nothing", func(t *testing.T) {
		// Presenting a key that grants nothing is not the same as presenting none: the key was the
		// authority for the listing and it turned out to grant nothing, so nothing is listed.
		ctx := presentCtx("sk-bf-unknown")

		access, err := plugin.ResolveAccess(ctx)
		require.NoError(t, err)
		filtered := plugin.filterModelsForAccess(access, models)

		assert.NotNil(t, filtered)
		assert.Empty(t, filtered)
	})

	t.Run("presenting nothing lists nothing either", func(t *testing.T) {
		// A request nobody granted anything carries no access, and the listing is only narrowed for
		// a request that presented something; the caller gates on that before asking (see
		// PostLLMHook), so this is never reached for such a request. Reached anyway, it has no
		// access to list against.
		ctx := emptyCtx()

		access, err := plugin.ResolveAccess(ctx)
		require.NoError(t, err)
		filtered := plugin.filterModelsForAccess(access, models)

		assert.Empty(t, filtered)
	})
}

// Evaluation is the funnel every caller passes through, so the permits a request holds without a
// key are enforced there too, not only where a key is presented.
func TestEvaluateEnforcesPermitsHeldWithoutAKey(t *testing.T) {
	vk := buildAccessTestVK()
	held := permitWithProviders("other", "h1", "Holder", "openai")

	t.Run("a provider the request does not hold is refused", func(t *testing.T) {
		plugin := newAccessTestPlugin(t, vk, &permitStore{baseOverride: held})
		ctx := emptyCtx()

		result, bifrostErr := plugin.Evaluate(ctx, &EvaluationRequest{
			Provider: schemas.Anthropic,
			Model:    "claude-3-5-sonnet",
		})

		require.NotNil(t, bifrostErr)
		assert.Equal(t, DecisionProviderBlocked, result.Decision)
		assert.Contains(t, result.Reason, "Holder", "the denial names the permit in the way")
	})

	t.Run("a provider it does hold is allowed", func(t *testing.T) {
		plugin := newAccessTestPlugin(t, vk, &permitStore{baseOverride: held})
		ctx := emptyCtx()

		result, bifrostErr := plugin.Evaluate(ctx, &EvaluationRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-4o",
		})

		assert.Nil(t, bifrostErr)
		assert.Equal(t, DecisionAllow, result.Decision)
	})

	// Unchanged for a deployment whose store answers only for keys: a request with no key resolves
	// no permits, so nothing gates it.
	t.Run("a request holding nothing is unaffected", func(t *testing.T) {
		plugin := newAccessTestPlugin(t, vk, nil)
		ctx := emptyCtx()

		result, bifrostErr := plugin.Evaluate(ctx, &EvaluationRequest{
			Provider: schemas.Anthropic,
			Model:    "claude-3-5-sonnet",
		})

		assert.Nil(t, bifrostErr)
		assert.Equal(t, DecisionAllow, result.Decision)
	})

	// A caller holding two permits is read as holding both: what either permits is allowed, and a
	// refusal names both when neither permits.
	t.Run("several permits are read as one", func(t *testing.T) {
		second := permitWithProviders("other", "h2", "Second", "bedrock")
		plugin := newAccessTestPlugin(t, vk, &permitStore{bases: []schemas.Permit{held, second}})

		result, bifrostErr := plugin.Evaluate(emptyCtx(), &EvaluationRequest{Provider: schemas.Bedrock, Model: "nova"})
		assert.Nil(t, bifrostErr)
		assert.Equal(t, DecisionAllow, result.Decision, "the second permit allows it")

		result, bifrostErr = plugin.Evaluate(emptyCtx(), &EvaluationRequest{Provider: schemas.Anthropic, Model: "claude"})
		require.NotNil(t, bifrostErr)
		assert.Equal(t, DecisionProviderBlocked, result.Decision)
		assert.Contains(t, result.Reason, "'Holder', 'Second'", "both refused, both are named")
	})
}

// A credential the request presented that resolves to no permit is a failed authentication, not
// an anonymous request. The two are indistinguishable from the access alone (there is none either
// way), so the funnel separates them by asking whether anything was presented at all.
func TestEvaluateRefusesAPresentedCredentialThatGrantsNothing(t *testing.T) {
	vk := buildAccessTestVK()
	plugin := newAccessTestPlugin(t, vk, nil)

	t.Run("a credential that resolves to nothing is refused", func(t *testing.T) {
		ctx := presentCtx("sk-bf-revoked")

		result, bifrostErr := plugin.Evaluate(ctx, &EvaluationRequest{
			RequestType: schemas.ChatCompletionRequest,
			Provider:    schemas.OpenAI,
			Model:       "gpt-4o",
		})

		require.NotNil(t, bifrostErr, "a revoked credential must not read as an anonymous request")
		assert.Equal(t, DecisionAccessNotFound, result.Decision)
		require.NotNil(t, bifrostErr.StatusCode)
		assert.Equal(t, 401, *bifrostErr.StatusCode, "authentication failed rather than permission denied")
	})

	t.Run("a credential the transport settled is read the same way", func(t *testing.T) {
		// What was presented is the identity's to say once the transport has settled one.
		ctx := emptyCtx()
		ctx.Grant().SetIdentity(grant.NewIdentity(grant.NewCredential(grant.CredentialVirtualKey, "sk-bf-revoked"), nil, nil, nil, nil))

		result, bifrostErr := plugin.Evaluate(ctx, &EvaluationRequest{
			RequestType: schemas.ChatCompletionRequest,
			Provider:    schemas.OpenAI,
			Model:       "gpt-4o",
		})

		require.NotNil(t, bifrostErr)
		assert.Equal(t, DecisionAccessNotFound, result.Decision)
	})

	t.Run("an authenticated identity whose access is missing is refused too", func(t *testing.T) {
		// A caller can be granted access by something other than a key, and a store that resolves
		// such callers answers with nothing when the one it was asked about has none. That must
		// refuse: reading only the key would let the caller through as though they had presented
		// nothing, which is unrestricted, the moment whatever configures their access stopped
		// answering for them.
		identityPlugin := newAccessTestPlugin(t, vk, &permitStore{resolvesNothing: true})
		ctx := presentUserCtx("user-with-no-access")

		result, bifrostErr := identityPlugin.Evaluate(ctx, &EvaluationRequest{
			RequestType: schemas.ChatCompletionRequest,
			Provider:    schemas.OpenAI,
			Model:       "gpt-4o",
		})

		require.NotNil(t, bifrostErr, "an identity that resolves to nothing must not read as an anonymous request")
		assert.Equal(t, DecisionAccessNotFound, result.Decision)
	})

	t.Run("presenting nothing is not a failed authentication", func(t *testing.T) {
		// Whether an anonymous request is allowed at all is the deployment's mandatory-auth
		// decision, made before this; it is not an access refusal.
		ctx := emptyCtx()

		result, bifrostErr := plugin.Evaluate(ctx, &EvaluationRequest{
			RequestType: schemas.ChatCompletionRequest,
			Provider:    schemas.OpenAI,
			Model:       "gpt-4o",
		})

		assert.Nil(t, bifrostErr)
		assert.Equal(t, DecisionAllow, result.Decision)
	})

	t.Run("a request with no grant cannot be evaluated", func(t *testing.T) {
		// Not refused as a caller, reported as a wiring fault: nothing settled who the request is.
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

		result, bifrostErr := plugin.Evaluate(ctx, &EvaluationRequest{
			RequestType: schemas.ChatCompletionRequest,
			Provider:    schemas.OpenAI,
			Model:       "gpt-4o",
		})

		require.NotNil(t, bifrostErr)
		assert.Equal(t, DecisionAccessUnresolved, result.Decision)
		require.NotNil(t, bifrostErr.StatusCode)
		assert.Equal(t, 500, *bifrostErr.StatusCode)
	})
}

// Mandatory authentication asks what the request carried, and a deployment may authenticate a
// caller without issuing them a key. Refusing such a caller for holding no key would reject
// exactly the requests the setting exists to admit: its own message offers a user token as an
// alternative.
func TestMandatoryAuthAcceptsAnAuthenticatedIdentity(t *testing.T) {
	vk := buildAccessTestVK()
	logger := NewMockLogger()
	local, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
	}, nil, &mockInMemoryStore{})
	require.NoError(t, err)

	plugin, err := InitFromStore(context.Background(), &Config{IsVkMandatory: boolPtr(true)},
		logger, local, nil, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })

	evaluate := func(t *testing.T, stamp func(ctx *schemas.BifrostContext)) *schemas.BifrostError {
		t.Helper()
		ctx := emptyCtx()
		stamp(ctx)
		_, bifrostErr := plugin.Evaluate(ctx, &EvaluationRequest{
			RequestType: schemas.ChatCompletionRequest,
			Provider:    schemas.OpenAI,
			Model:       "gpt-4o",
		})
		return bifrostErr
	}

	t.Run("presenting nothing is refused", func(t *testing.T) {
		bifrostErr := evaluate(t, func(*schemas.BifrostContext) {})

		require.NotNil(t, bifrostErr)
		require.NotNil(t, bifrostErr.StatusCode)
		assert.Equal(t, 401, *bifrostErr.StatusCode)
	})

	t.Run("an identity that presented nothing is refused too", func(t *testing.T) {
		// The transport settles an identity on every request, including one that presented nothing.
		bifrostErr := evaluate(t, func(ctx *schemas.BifrostContext) {
			ctx.Grant().SetIdentity(grant.NewIdentity(schemas.Credential{}, nil, nil, nil, nil))
		})

		require.NotNil(t, bifrostErr)
		require.NotNil(t, bifrostErr.StatusCode)
		assert.Equal(t, 401, *bifrostErr.StatusCode)
	})

	t.Run("an authenticated identity satisfies it", func(t *testing.T) {
		// The contrast with the cases above is the whole assertion: the same request, refused for
		// presenting nothing and not refused for that once it presents an identity, is Step 1 reading
		// what was presented rather than what it resolved to. What such a caller may then do is the
		// store's answer, not this step's: this store resolves permits from keys alone, so an identity
		// it has no access for is refused as access not found, not as unauthenticated.
		bifrostErr := evaluate(t, func(ctx *schemas.BifrostContext) {
			ctx.Grant().SetIdentity(grant.NewIdentity(grant.NewCredential(grant.CredentialSessionToken, "user-1"), &schemas.UserRef{ID: "user-1"}, nil, nil, nil))
		})

		require.NotNil(t, bifrostErr)
		require.NotNil(t, bifrostErr.Type)
		assert.Equal(t, string(DecisionAccessNotFound), *bifrostErr.Type, "past mandatory auth, refused by the store instead")
	})
}

// A signed-in user nothing grants access to is refused as access not found, as is a presented key
// that resolves to nothing and a credential that names no user.
func TestEvaluateRefusesAnUngrantedUser(t *testing.T) {
	vk := buildAccessTestVK()
	logger := NewMockLogger()
	local, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
	}, nil, &mockInMemoryStore{})
	require.NoError(t, err)

	plugin, err := InitFromStore(context.Background(), &Config{IsVkMandatory: boolPtr(false)},
		logger, local, nil, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })

	user := &schemas.UserRef{ID: "user-1"}
	session := grant.NewCredential(grant.CredentialSessionToken, "user-1")
	evaluate := func(t *testing.T, identity schemas.Identity) (*EvaluationResult, *schemas.BifrostError) {
		t.Helper()
		ctx := emptyCtx()
		ctx.Grant().SetIdentity(identity)
		return plugin.Evaluate(ctx, &EvaluationRequest{
			RequestType: schemas.ChatCompletionRequest,
			Provider:    schemas.OpenAI,
			Model:       "gpt-4o",
		})
	}

	t.Run("a user with no access is refused", func(t *testing.T) {
		result, bifrostErr := evaluate(t, grant.NewIdentity(session, user, nil, nil, nil))

		require.NotNil(t, bifrostErr)
		assert.Equal(t, DecisionAccessNotFound, result.Decision)
	})

	t.Run("a presented key that resolves to nothing is refused", func(t *testing.T) {
		result, bifrostErr := evaluate(t, grant.NewIdentity(grant.NewCredential(grant.CredentialVirtualKey, "sk-bf-nobody"), user, nil, nil, nil))

		require.NotNil(t, bifrostErr)
		assert.Equal(t, DecisionAccessNotFound, result.Decision)
	})

	t.Run("a credential naming no user is refused", func(t *testing.T) {
		result, bifrostErr := evaluate(t, grant.NewIdentity(session, nil, nil, nil, nil))

		require.NotNil(t, bifrostErr)
		assert.Equal(t, DecisionAccessNotFound, result.Decision)
	})
}

// Whether a credential may be used at all is settled when its permit is built, so the funnel reads
// it off the permit rather than resolving the credential again. Inactive and expired are reported
// distinctly, and inactive wins when a key is both: a key switched off is not a key that ran out.
func TestEvaluateRefusesAPermitThatMayNotBeUsed(t *testing.T) {
	newPlugin := func(t *testing.T, mutate func(vk *configstoreTables.TableVirtualKey)) (*GovernancePlugin, *schemas.BifrostContext) {
		vk := buildAccessTestVK()
		mutate(vk)
		return newAccessTestPlugin(t, vk, nil), newPreRequestCtx()
	}
	request := &EvaluationRequest{
		RequestType: schemas.ChatCompletionRequest,
		Provider:    schemas.OpenAI,
		Model:       "gpt-4o",
	}

	t.Run("an expired permit", func(t *testing.T) {
		past := time.Now().UTC().Add(-time.Second)
		plugin, ctx := newPlugin(t, func(vk *configstoreTables.TableVirtualKey) { vk.ExpiresAt = &past })

		result, bifrostErr := plugin.Evaluate(ctx, request)

		require.NotNil(t, bifrostErr)
		assert.Equal(t, DecisionAccessBlocked, result.Decision)
		assert.Contains(t, result.Reason, "expired")
		require.NotNil(t, bifrostErr.StatusCode)
		assert.Equal(t, 401, *bifrostErr.StatusCode, "an expired credential fails authentication (SPEC API-08)")
		require.NotNil(t, bifrostErr.Error.Refusal)
		assert.Equal(t, "key_expired", bifrostErr.Error.Refusal.Reason)
	})

	t.Run("an inactive permit, even with a future expiry", func(t *testing.T) {
		future := time.Now().UTC().Add(time.Hour)
		plugin, ctx := newPlugin(t, func(vk *configstoreTables.TableVirtualKey) {
			inactive := false
			vk.IsActive = &inactive
			vk.ExpiresAt = &future
		})

		result, bifrostErr := plugin.Evaluate(ctx, request)

		require.NotNil(t, bifrostErr)
		assert.Equal(t, DecisionAccessBlocked, result.Decision)
		assert.Contains(t, result.Reason, "inactive", "switched off is not the same as run out")
	})

	// The refusal names what kind of thing was refused, so a deployment granting access through
	// something other than a key does not report it as one.
	t.Run("the refusal names the permit's kind", func(t *testing.T) {
		past := time.Now().UTC().Add(-time.Second)
		plugin, ctx := newPlugin(t, func(vk *configstoreTables.TableVirtualKey) { vk.ExpiresAt = &past })

		result, _ := plugin.Evaluate(ctx, request)

		assert.Contains(t, result.Reason, "virtual key")
	})
}

// A holder's own model configs are stored under a scope name, and a permit names its holder by
// type. For virtual keys the two spell it differently, so the lookup has to translate rather than
// cast: a near-miss finds nothing and reads as "this key configured no model limits", which is
// indistinguishable from the truth until a budget that should have refused a request quietly never
// does.
//
// The test builds the permit the way production does, because a test that constructs a
// scope-shaped type of its own would agree with a broken lookup.
func TestPermitFindsItsOwnScopedModelConfigs(t *testing.T) {
	logger := NewMockLogger()
	vk := buildVirtualKey("vk1", "sk-bf-scoped", "Scoped VK", true)
	vk.ProviderConfigs = []configstoreTables.TableVirtualKeyProviderConfig{
		buildProviderConfig("openai", []string{"*"}),
	}
	budget := buildBudget("b-vk-model", 100.0, "1h")
	modelConfig := buildVKScopedModelConfig("mc-vk", "gpt-4o", nil, vk.ID, budget, nil)

	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys:  []configstoreTables.TableVirtualKey{*vk},
		ModelConfigs: []configstoreTables.TableModelConfig{*modelConfig},
		Budgets:      []configstoreTables.TableBudget{*budget},
	}, nil, nil)
	require.NoError(t, err)

	ctx := resolverCtx(store, "sk-bf-scoped")
	bases, _ := store.ResolvePermits(ctx)
	require.Len(t, bases, 1)
	require.Equal(t, string(grant.PermitVirtualKey), bases[0].Type(), "the type production stamps, not a scope name")

	budgets, _ := store.PermitModelLimits(ctx, bases[0], schemas.OpenAI, "gpt-4o")

	assert.Contains(t, limitIDsOf(budgets), "b-vk-model",
		"a key's own model budget must be found through the permit production builds for it")
	for _, budget := range budgets {
		if budget.ID == "b-vk-model" {
			assert.Equal(t, string(grant.LimitHolderVirtualKeyModelConfig), budget.HolderKind, "attributed to the key's own model config")
		}
	}
}

// settlingErrorStore wraps a real store so GatherLimits fails the way a store composing several
// possible payers can: the payers could not be settled at all, not a limit being exhausted.
type settlingErrorStore struct {
	GovernanceStore
	err error
}

func (s *settlingErrorStore) GatherLimits(ctx *schemas.BifrostContext, access schemas.Access, provider schemas.ModelProvider, model string) ([]schemas.Limit, []schemas.Limit, error) {
	return nil, nil, s.err
}

// newSettlingErrorTestPlugin builds a plugin whose store fails to settle limits with err.
func newSettlingErrorTestPlugin(t *testing.T, err error) *GovernancePlugin {
	t.Helper()
	vk := buildAccessTestVK()
	logger := NewMockLogger()
	local, localErr := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
	}, nil, &mockInMemoryStore{})
	require.NoError(t, localErr)

	plugin, initErr := InitFromStore(context.Background(), &Config{IsVkMandatory: boolPtr(false)},
		logger, &settlingErrorStore{GovernanceStore: local, err: err}, nil, nil, nil)
	require.NoError(t, initErr)
	t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })
	return plugin
}

// TestEvaluateSkipsASettlingErrorWhenSpendingChecksAreSkipped covers the store.GatherLimits error
// path for a request that asked not to be checked against anything it answers to (e.g. a read-only
// list-models call). LocalGovernanceStore never returns one, but the interface allows a store
// composing several possible payers to fail this way, and such a request spends nothing, so an
// unsettled payer list is not something it depends on either.
func TestEvaluateSkipsASettlingErrorWhenSpendingChecksAreSkipped(t *testing.T) {
	request := &EvaluationRequest{Provider: schemas.OpenAI, Model: "gpt-4o"}

	t.Run("a settling error still blocks a request that did not skip spending checks", func(t *testing.T) {
		plugin := newSettlingErrorTestPlugin(t, errors.New("payers unavailable"))

		result, bifrostErr := plugin.Evaluate(emptyCtx(), request)

		require.NotNil(t, bifrostErr, "a request whose payers could not be settled was served anyway")
		assert.Equal(t, DecisionAccessBlocked, result.Decision)
	})

	t.Run("a request that skipped spending checks is not blocked by it", func(t *testing.T) {
		plugin := newSettlingErrorTestPlugin(t, errors.New("payers unavailable"))
		ctx := emptyCtx()
		ctx.SetValue(schemas.BifrostContextKeySkipBudgetAndRateLimits, true)

		result, bifrostErr := plugin.Evaluate(ctx, request)

		assert.Nil(t, bifrostErr, "a read-only request was blocked by a settling failure it does not depend on")
		assert.Equal(t, DecisionAllow, result.Decision)
	})
}

// testTeamLevelPermit labels a permit a store places one level above the caller, as an
// organization above a key would be. The access engine reads the level, not the kind, so the kind
// only names which side of a case the permit stands on.
const testTeamLevelPermit grant.PermitType = "test_team_level"

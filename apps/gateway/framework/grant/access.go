package grant

import (
	"slices"

	"github.com/maximhq/bifrost/core/schemas"
)

// CompositionMode is how the caller's permit levels combine, and how two key restrictions fold.
// These are the values a schemas.Access reports from LevelMode.
type CompositionMode string

const (
	// Intersect permits what every side permits.
	Intersect CompositionMode = "intersect"
	// Union permits what any side permits.
	Union CompositionMode = "union"
)

// ModelMatcher decides whether model, as the request names it, is one of allowed on provider.
// Whoever resolves a request's access supplies it, because deciding that needs the deployment's own
// view of the provider: how it names models, and which provider-prefixed or cross-provider entries
// in allowed stand for the same model. Without one, entries are matched by name.
type ModelMatcher func(provider, model string, allowed []string) bool

// Access is the one implementation of schemas.Access: the fold of the permits a request carries.
//
// Per attempt rather than per request, because a request that fails over resolves again for each
// provider it tries. Configuration can change between two slow calls, and the attempt running
// second has to answer to what is in force when it runs.
//
// The caller's permits are a list because a caller can hold several at once, and they are read as
// one, as if they were a single permit holding all of their provider permits: what any of
// them permits, the caller may reach; any key any of them allows may serve the request; and every
// one of them that permits the pair pays for it (see PermitsForModel). Their order is the order the
// resolver gave them, and is the order candidates and refusals are reported in.
//
// The caller's permits sit at the levels the resolver gave them (see schemas.Permit.Level). Within a
// level they always union; levelMode says whether levels union or intersect. Under Intersect the
// lowest level present grants and each level above it caps. Capping levels never add access.
type Access struct {
	// bases is every permit the caller holds, as given. Levels are read off each permit when needed.
	bases        []schemas.Permit
	levelMode    CompositionMode
	matcher      ModelMatcher
	keyMatcher   KeyScopedModelMatcher // optional; replaces matcher when set
	aliasTargets AliasTargets          // optional; what a requested name can become through key aliases
}

// KeyScopedModelMatcher is a ModelMatcher that is also told which of the provider's keys the
// provider permit lets serve the request (["*"] for every key, empty for none). A name that only a
// key alias makes allowed depends on the keys that can serve it, so a deployment resolving aliases
// supplies this instead of a plain ModelMatcher.
type KeyScopedModelMatcher func(provider, model string, allowed, keyIDs []string) bool

// WithKeyScopedMatcher sets the key-scoped matcher, which takes the place of the access's
// ModelMatcher, and returns the access. A nil access is left alone.
func (a *Access) WithKeyScopedMatcher(f KeyScopedModelMatcher) *Access {
	if a != nil {
		a.keyMatcher = f
	}
	return a
}

// WithModelMatcher returns a copy of the access that matches models through matcher alone, with no
// key-scoped matcher and no alias-target source: the same permits, levels and modes, read as names
// that reach the provider as written. A nil access gives nil.
func (a *Access) WithModelMatcher(matcher ModelMatcher) *Access {
	if a == nil {
		return nil
	}
	literal := *a
	literal.matcher = matcher
	literal.keyMatcher = nil
	literal.aliasTargets = nil
	return &literal
}

// AliasTargets calls visit with each model that key aliases map model to on provider, considering
// only the keys keyIDs names (["*"] for every key, empty for none). It is how an access learns what a
// requested name can become once key selection has resolved it, so a blacklist on that model is not
// side-stepped by naming it through an alias.
type AliasTargets func(provider, model string, keyIDs []string, visit func(target string))

// WithAliasTargets sets the alias-target source and returns the access. A nil access is left alone.
func (a *Access) WithAliasTargets(f AliasTargets) *Access {
	if a != nil {
		a.aliasTargets = f
	}
	return a
}

// NewAccess folds the permits the caller holds.
//
// levelMode is how the caller's levels combine: Intersect makes each level above the lowest cap it,
// anything else (Union, or "") reads every permit as one. Nil entries in bases are dropped, so a
// resolver that found nothing for one source does not have to say so twice.
func NewAccess(bases []schemas.Permit, levelMode CompositionMode, matcher ModelMatcher) *Access {
	held := make([]schemas.Permit, 0, len(bases))
	for _, base := range bases {
		if !isNilPermit(base) {
			held = append(held, base)
		}
	}
	return &Access{
		bases:     held,
		levelMode: levelMode,
		matcher:   matcher,
	}
}

// grantingLevel is the lowest level holding a permit, or -1 when there is none. Intersect only:
// union paths go through granting, levelsAbove or callerAllows.
func (a *Access) grantingLevel() int {
	lowest := -1
	for _, p := range a.bases {
		if level := p.Level(); lowest < 0 || level < lowest {
			lowest = level
		}
	}
	return lowest
}

// levelsAbove returns the levels that cap the granting one, lowest first. Under Union no level caps.
func (a *Access) levelsAbove() []int {
	if a.levelMode != Intersect {
		return nil
	}
	granting := a.grantingLevel()
	var levels []int
	for _, p := range a.bases {
		if level := p.Level(); level > granting && !slices.Contains(levels, level) {
			levels = append(levels, level)
		}
	}
	slices.Sort(levels)
	return levels
}

// permitsAt returns the caller's permits at level, in order. Intersect only: union paths go
// through granting, levelsAbove or callerAllows.
func (a *Access) permitsAt(level int) []schemas.Permit {
	var permits []schemas.Permit
	for _, p := range a.bases {
		if p.Level() == level {
			permits = append(permits, p)
		}
	}
	return permits
}

// granting returns the permits that grant. Under Union every level grants.
func (a *Access) granting() []schemas.Permit {
	if a.levelMode != Intersect {
		if len(a.bases) == 0 {
			return nil
		}
		return slices.Clone(a.bases)
	}
	return a.permitsAt(a.grantingLevel())
}

// Bases implements schemas.Access.
func (a *Access) Bases() []schemas.Permit {
	if a == nil {
		return nil
	}
	return a.granting()
}

// LevelMode implements schemas.Access.
func (a *Access) LevelMode() string {
	if a == nil {
		return ""
	}
	return string(a.levelMode)
}

// IsProviderAllowed implements schemas.Access.
func (a *Access) IsProviderAllowed(provider string) bool {
	if a == nil {
		return false
	}
	return a.callerAllows(func(p schemas.Permit) bool { return allowsProvider(p, provider) })
}

// IsModelAllowed implements schemas.Access.
func (a *Access) IsModelAllowed(provider string, model string) bool {
	if a == nil {
		return false
	}
	return a.callerAllows(func(p schemas.Permit) bool { return a.permitAllowsModel(p, provider, model) })
}

// AllowsEveryModel implements schemas.Access.
func (a *Access) AllowsEveryModel(provider string) bool {
	if a == nil {
		return false
	}
	return a.callerAllows(func(p schemas.Permit) bool { return permitAllowsEveryModel(p, provider) })
}

// permitAllowsEveryModel reports whether the permit permits every model on provider. Its provider
// permits for the provider are read as a union, as permitAllowsModel reads them: one unrestricted
// entry is enough, while a blacklist on any entry is decisive (blacklistsModel blocks the model
// whatever another entry allows). A provider the permit lists no permit for is every-model only
// under allow-all, as in permitAllowsModel.
func permitAllowsEveryModel(p schemas.Permit, provider string) bool {
	if isNilPermit(p) {
		return false
	}
	found, unrestricted := false, false
	for _, pp := range p.ProviderPermits() {
		if pp.Provider != provider {
			continue
		}
		found = true
		if len(pp.BlacklistedModels) > 0 {
			return false
		}
		if pp.AllowedModels.IsUnrestricted() {
			unrestricted = true
		}
	}
	if !found {
		return p.AllowsAllProviders()
	}
	return unrestricted
}

// PermitsForModel implements schemas.Access.
func (a *Access) PermitsForModel(provider string, model string) []schemas.Permit {
	if a == nil || !a.IsModelAllowed(provider, model) {
		return nil
	}
	permits := make([]schemas.Permit, 0, len(a.bases))
	allowsPair := func(p schemas.Permit) bool { return a.permitAllowsModel(p, provider, model) }
	for _, base := range a.granting() {
		if allowsPair(base) {
			permits = append(permits, base)
		}
	}
	// The capping levels it passed are named too, so their limits apply.
	for _, level := range a.levelsAbove() {
		for _, permit := range a.permitsAt(level) {
			if allowsPair(permit) {
				permits = append(permits, permit)
			}
		}
	}
	return permits
}

// KeysForModel implements schemas.Access.
//
// The caller's side is the union of every key restriction its permitting permits hold for the
// provider, across all of their provider permits for it: the caller's permits are read as one, and
// a request served by any of them may use any key any of them allows. Capping levels narrow it. A
// permit that does not authorize the pair has no say, which is why the model is asked for.
//
// The result is a plain []string rather than any list type of the permit's own: it is handed
// straight to consumers that type-assert []string, where a named slice type would fail the
// assertion silently and read as "no restriction at all".
func (a *Access) KeysForModel(provider string, model string) (keyIDs []string, restricted bool) {
	if a == nil || !a.IsModelAllowed(provider, model) {
		// The request cannot proceed on this provider, so there is no key restriction to report.
		return nil, false
	}
	granted, holds := a.unionKeysHeldBy(a.granting(), provider, model)
	granted, holds = a.capKeys(granted, holds, provider, model)
	if !holds {
		// No permit holds a provider permit, so there is no restriction to report. That is a
		// different answer from a permit whose restriction happens to name no key, which must not
		// read as "every key is allowed", so it is decided here, on whether a provider permit was
		// found at all, rather than on whether the list came back empty.
		return nil, false
	}
	if granted.IsUnrestricted() {
		return nil, false
	}
	// A copy, and always non-nil: a consumer must not be able to edit the permit through the answer,
	// and an empty restriction allows no key, which is not "no restriction at all".
	keyIDs = make([]string, 0, len(granted))
	keyIDs = append(keyIDs, granted...)
	return keyIDs, true
}

// RestrictsKeys reports whether any permit of the access holds a key restriction for provider.
// When none does, KeysForModel reports no restriction for every model of the provider, so a caller
// on a hot path can skip it. It reads the permits in place and allocates nothing.
func (a *Access) RestrictsKeys(provider string) bool {
	if a == nil {
		return false
	}
	restricts := func(p schemas.Permit) bool {
		if isNilPermit(p) {
			return false
		}
		// This package's own permit is read in place; ProviderPermits hands out a copy.
		pps := []schemas.ProviderPermit(nil)
		if own, ok := p.(*Permit); ok {
			pps = own.providerPermits
		} else {
			pps = p.ProviderPermits()
		}
		for i := range pps {
			if pps[i].Provider == provider && !pps[i].KeyIDs.IsUnrestricted() {
				return true
			}
		}
		return false
	}
	return slices.ContainsFunc(a.bases, restricts)
}

// capKeys narrows the caller's keys by each capping level that restricts this provider's keys. A
// level with no restriction for the provider leaves them alone.
func (a *Access) capKeys(keys schemas.WhiteList, holds bool, provider string, model string) (schemas.WhiteList, bool) {
	for _, level := range a.levelsAbove() {
		layerKeys, layerHolds := a.unionKeysHeldBy(a.permitsAt(level), provider, model)
		if !layerHolds {
			continue
		}
		if !holds {
			keys, holds = layerKeys, true
			continue
		}
		keys = composeKeyIDs(keys, layerKeys, Intersect)
	}
	return keys, holds
}

// unionKeysHeldBy is the union of the key restrictions that permits which authorize model on
// provider hold for that provider, and whether any of them holds one at all. A permit that does not
// authorize the pair has no say.
func (a *Access) unionKeysHeldBy(permits []schemas.Permit, provider string, model string) (keys schemas.WhiteList, holds bool) {
	for _, permit := range permits {
		if !a.permitAllowsModel(permit, provider, model) {
			continue
		}
		for _, pp := range permit.ProviderPermits() {
			if pp.Provider != provider {
				continue
			}
			holds = true
			keys = composeKeyIDs(keys, pp.KeyIDs, Union)
		}
	}
	return keys, holds
}

// ProvidersForModel implements schemas.Access. Model names resolve through the matcher here. Unlike
// GrantedProvidersForModel, this is the decision itself rather than a gate over one, so cross-provider
// naming and provider-prefixed entries must resolve exactly as the deployment resolves them.
func (a *Access) ProvidersForModel(model string) []schemas.ProviderCandidate {
	if a == nil || model == "" {
		return nil
	}

	candidates := make([]schemas.ProviderCandidate, 0, a.providerPermitCount())
	a.eachProviderPermit(func(owner schemas.Permit, pp *schemas.ProviderPermit) bool {
		provider := pp.Provider
		// Three separate questions, none of which implies another. Whether the composed access
		// permits the pair at all:
		if !a.IsModelAllowed(provider, model) {
			return false
		}
		// Whether *this* permit permits it: another of the caller's permits may be the one authorizing
		// the request, and a permit that blacklists the model must not serve it. One blacklisting
		// provider permit blocks the provider for that model across the whole permit, so a
		// permissive one cannot reopen it, and so does one that blacklists a model the name becomes through
		// a key alias:
		if !a.permitAllowsModel(owner, provider, model) {
			return false
		}
		// And whether this provider permit in particular permits it, so a permit holding several for
		// a provider only offers the ones that can serve the model:
		if !a.permitsModel(pp, model, pp.KeyIDs) {
			return false
		}
		// A candidate a capping level refuses is not offered.
		if !a.capsAllow(func(p schemas.Permit) bool { return a.permitAllowsModel(p, provider, model) }) {
			return false
		}
		// Per candidate the keys are this provider permit's own, narrowed by the capping levels;
		// KeysForModel is where the caller's permits are read as one.
		selectedKeyIDs := a.capCandidateKeys(pp.KeyIDs, provider, model)
		// A copy, as in KeysForModel: selectedKeyIDs is the permit's own slice whenever nothing composed
		// it, and a consumer editing the candidate must not edit the permit through it.
		candidates = append(candidates, schemas.ProviderCandidate{
			Provider: provider,
			Weight:   pp.Weight,
			KeyIDs:   slices.Clone(selectedKeyIDs),
		})
		return true
	})

	if len(candidates) == 0 {
		return nil
	}
	return candidates
}

// capCandidateKeys narrows one candidate's keys by each capping level, as capKeys does.
func (a *Access) capCandidateKeys(keyIDs schemas.WhiteList, provider string, model string) schemas.WhiteList {
	for _, level := range a.levelsAbove() {
		if layerKeys, layerHolds := a.unionKeysHeldBy(a.permitsAt(level), provider, model); layerHolds {
			keyIDs = composeKeyIDs(keyIDs, layerKeys, Intersect)
		}
	}
	return keyIDs
}

// GrantedProvidersForModel implements schemas.Access. Because this gate cannot ask ProvidersForModel
// anything, the composition it needs is re-derived by name below.
func (a *Access) GrantedProvidersForModel(model string) []string {
	if a == nil {
		return nil
	}

	allowed := make([]string, 0, a.providerPermitCount())
	seen := make(map[string]struct{})
	a.eachProviderPermit(func(_ schemas.Permit, pp *schemas.ProviderPermit) bool {
		if _, dup := seen[pp.Provider]; dup {
			return true
		}
		if !providerPermitAllowsModel(pp, model) {
			return false
		}
		if !a.capsAllow(func(p schemas.Permit) bool { return allowsModelByName(p, pp.Provider, model) }) {
			return false
		}
		seen[pp.Provider] = struct{}{}
		allowed = append(allowed, pp.Provider)
		return true
	})
	return allowed
}

// eachProviderPermit visits the provider permits the request could be served from: every provider
// permit of every one of the caller's permits, in order. The coarse provider gate and candidate
// selection walk it identically rather than each keeping its own copy of the rule.
//
// The caller's permits do not shadow one another: they are read as one permit, and within one
// permit two provider permits for the same provider are both offered, which is what lets a holder
// run a provider under two weights or key sets.
func (a *Access) eachProviderPermit(accept func(owner schemas.Permit, pp *schemas.ProviderPermit) bool) {
	for _, base := range a.granting() {
		eachProviderPermit(base, func(pp *schemas.ProviderPermit) bool {
			accept(base, pp)
			return true
		})
	}
}

// providerPermitCount is how many provider permits the request holds across every permit, for
// sizing results.
func (a *Access) providerPermitCount() int {
	count := 0
	for _, base := range a.bases {
		count += len(base.ProviderPermits())
	}
	return count
}

// DeniedPermitsForModel implements schemas.Access.
//
// What is named is whatever refused and whose permission would have changed the answer. The
// caller's permits are read as one, so when none of them permits the pair all of them refused and
// all are named: a caller holding two permits is told that neither permits the model, not that one
// of them does not. When they allow but a capping level refuses, that level is named. A caller
// holding no permit at all has nothing to be named.
func (a *Access) DeniedPermitsForModel(provider string, model string) []schemas.Permit {
	if a == nil {
		return nil
	}
	return a.deniedBy(func(p schemas.Permit) bool { return a.permitAllowsModel(p, provider, model) }, true)
}

// deniedBy identifies which permits refused, given a verdict per permit.
//
// When the caller's permits allow but a capping level refuses, the refusing levels are named instead.
// capped is false for questions the granting level alone decides.
func (a *Access) deniedBy(permits func(schemas.Permit) bool, capped bool) []schemas.Permit {
	baseAllows := a.anyBase(permits)
	var refusingCaps []schemas.Permit
	if baseAllows && capped {
		refusingCaps = a.refusingCaps(permits)
		baseAllows = len(refusingCaps) == 0
	}
	if baseAllows {
		return nil
	}
	if len(refusingCaps) > 0 {
		return refusingCaps
	}
	return a.refusingBases()
}

// refusingBases returns the granting level's permits, to name when none of them allowed.
func (a *Access) refusingBases() []schemas.Permit {
	return a.granting()
}

// refusingCaps returns the permits of every capping level that refused.
func (a *Access) refusingCaps(permits func(schemas.Permit) bool) []schemas.Permit {
	var refused []schemas.Permit
	for _, level := range a.levelsAbove() {
		if layer := a.permitsAt(level); !anyPermit(layer, permits) {
			refused = append(refused, layer...)
		}
	}
	return refused
}

// callerAllows reports whether the caller's side allows it: any permit under Union, or one permit
// in every level holding one under Intersect.
func (a *Access) callerAllows(permits func(schemas.Permit) bool) bool {
	if a.levelMode != Intersect {
		return slices.ContainsFunc(a.bases, permits)
	}
	present, allowed := map[int]bool{}, map[int]bool{}
	for _, p := range a.bases {
		level := p.Level()
		present[level] = true
		if permits(p) {
			allowed[level] = true
		}
	}
	for level := range present {
		if !allowed[level] {
			return false
		}
	}
	return len(present) > 0
}

// capsAllow reports whether every capping level allows it. True when nothing caps the caller.
func (a *Access) capsAllow(permits func(schemas.Permit) bool) bool {
	for _, level := range a.levelsAbove() {
		if layer := a.permitsAt(level); !anyPermit(layer, permits) {
			return false
		}
	}
	return true
}

// anyPermit reports whether any permit in layer satisfies permits.
func anyPermit(layer []schemas.Permit, permits func(schemas.Permit) bool) bool {
	for _, permit := range layer {
		if permits(permit) {
			return true
		}
	}
	return false
}

// anyBase reports whether any permit of the granting level allows it.
func (a *Access) anyBase(permits func(schemas.Permit) bool) bool {
	for _, base := range a.granting() {
		if permits(base) {
			return true
		}
	}
	return false
}

// composeKeyIDs folds two key restrictions under a composition mode. Order follows the first
// argument, so the result is stable. Key IDs are identifiers, so membership here is exact rather
// than through the list type's case-folding methods; only the wildcard is read off the type.
//
// The wildcard is the universe rather than an entry: it stands for every key the provider has, so
// intersecting with it yields the other side untouched and unioning with it is unrestricted. An
// unrecognized mode permits nothing, as everywhere else.
func composeKeyIDs(first, second schemas.WhiteList, mode CompositionMode) schemas.WhiteList {
	switch mode {
	case Intersect:
		if first.IsUnrestricted() {
			return second
		}
		if second.IsUnrestricted() {
			return first
		}
		shared := make(schemas.WhiteList, 0, len(first))
		for _, keyID := range first {
			if slices.Contains(second, keyID) {
				shared = append(shared, keyID)
			}
		}
		return shared
	case Union:
		if first.IsUnrestricted() || second.IsUnrestricted() {
			return schemas.WhiteList{Wildcard}
		}
		merged := make(schemas.WhiteList, 0, len(first)+len(second))
		merged = append(merged, first...)
		for _, keyID := range second {
			if !slices.Contains(merged, keyID) {
				merged = append(merged, keyID)
			}
		}
		return merged
	}
	return []string{}
}

// permitAllowsModel reports whether the permit permits model on provider: blacklisted by no
// provider permit of that provider, and allowed by at least one. A permit that is not there permits
// nothing. An empty model asks about the provider alone.
func (a *Access) permitAllowsModel(p schemas.Permit, provider string, model string) bool {
	if isNilPermit(p) {
		return false
	}
	if model == "" {
		return allowsProvider(p, provider)
	}
	if blacklistsModel(p, provider, model) {
		return false
	}
	pps := p.ProviderPermits()
	found := false
	var keyIDs schemas.WhiteList
	for i := range pps {
		pp := &pps[i]
		if pp.Provider != provider {
			continue
		}
		if !found && (a.keyMatcher != nil || a.aliasTargets != nil) {
			keyIDs = keyIDsServing(pps, provider)
		}
		found = true
		if a.permitsModel(pp, model, keyIDs) {
			return a.aliasTargetsNotBlacklisted(pps, provider, model, keyIDs)
		}
	}
	// A provider the permit lists keeps its own model rules even under allow-all; only a provider it
	// lists no permit for is opened up by allow-all, and then every model of it is allowed.
	if !found && p.AllowsAllProviders() {
		return true
	}
	return false
}

// aliasTargetsNotBlacklisted reports that no model model can become through a key alias is blacklisted
// by the provider permits pps. It reads the permits already fetched by the caller and does nothing,
// allocating nothing, unless an alias source is set and a permit blacklists something for provider.
func (a *Access) aliasTargetsNotBlacklisted(pps []schemas.ProviderPermit, provider, model string, keyIDs schemas.WhiteList) bool {
	if a.aliasTargets == nil {
		return true
	}
	blacklists := false
	for i := range pps {
		if pps[i].Provider == provider && len(pps[i].BlacklistedModels) > 0 {
			blacklists = true
			break
		}
	}
	if !blacklists {
		return true
	}
	ok := true
	a.aliasTargets(provider, model, keyIDs, func(target string) {
		for i := range pps {
			if pps[i].Provider == provider && pps[i].BlacklistedModels.IsBlocked(target) {
				ok = false
				return
			}
		}
	})
	return ok
}

// allKeyIDs is the key restriction that restricts nothing.
var allKeyIDs = schemas.WhiteList{"*"}

// keyIDsServing is the key restriction a request permitted by one of pps on provider is served
// under. Key selection may use the keys of every provider permit the permit holds for the provider
// (see unionKeysHeldBy), so with more than one it answers the union of their keys: every key when
// one of them is unrestricted, otherwise only the keys some permit names, so a key no permit lets
// serve the request cannot veto an alias. One provider permit, the usual case, allocates nothing.
func keyIDsServing(pps []schemas.ProviderPermit, provider string) schemas.WhiteList {
	var keyIDs schemas.WhiteList
	n := 0
	for i := range pps {
		if pps[i].Provider != provider {
			continue
		}
		if n == 0 {
			keyIDs = pps[i].KeyIDs
		} else {
			keyIDs = composeKeyIDs(keyIDs, pps[i].KeyIDs, Union)
		}
		n++
	}
	if n == 0 {
		return allKeyIDs
	}
	return keyIDs
}

// permitsModel applies one provider permit's allowed-models rule, through the key-scoped matcher or
// the matcher when there is one and by name otherwise. keyIDs is the key restriction the request is
// served under; only the key-scoped matcher reads it.
func (a *Access) permitsModel(pp *schemas.ProviderPermit, model string, keyIDs schemas.WhiteList) bool {
	if a.keyMatcher != nil {
		return a.keyMatcher(pp.Provider, model, pp.AllowedModels, keyIDs)
	}
	if a.matcher == nil {
		return pp.AllowedModels.IsAllowed(model)
	}
	return a.matcher(pp.Provider, model, pp.AllowedModels)
}

// Compile-time checks that this package implements the shapes core declares.
var (
	_ schemas.Grant    = (*Grant)(nil)
	_ schemas.Identity = (*Identity)(nil)
	_ schemas.Permit   = (*Permit)(nil)
	_ schemas.Access   = (*Access)(nil)
	_ schemas.Limits   = (*Limits)(nil)
)

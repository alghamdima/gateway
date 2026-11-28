// Package configstore provides a persistent configuration store for Bifrost.
package configstore

import (
	"context"
	"fmt"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/maximhq/bifrost/framework/vectorstore"
	"gorm.io/gorm"
)

// VirtualKeyQueryParams holds pagination, filtering, and search parameters for virtual key queries.
type VirtualKeyQueryParams struct {
	Limit                      int
	Offset                     int
	Search                     string
	CustomerID                 string
	TeamID                     string
	UserID                     string // Enterprise-only: filters to VKs assigned to this user; matches nothing in OSS
	SortBy                     string // name, budget_spent, created_at, status (default: created_at)
	Order                      string // asc, desc (default: asc)
	Export                     bool   // When true, skip default pagination limits (caller controls limit)
	ExcludeAssignedVirtualKeys bool   // When true, exclude VKs that already have any user assignment
	ForUserAssignment          bool   // When true, exclude VKs assigned to any entity (team, customer, or user) — intended for user-assignment pickers
	// MetadataFilters narrows to keys whose metadata has every listed key set to exactly the given
	// value (AND). Keys failing tables.IsValidVirtualKeyMetadataKey match nothing.
	MetadataFilters map[string]string
}

// ModelConfigsQueryParams holds pagination, filtering, and search parameters for model configs queries.
type ModelConfigsQueryParams struct {
	Limit  int
	Offset int
	Search string
	// Scope optionally filters to an exact scope value (e.g. "global",
	// "virtual_key").
	//
	// Deprecated: use Scopes, which can carry more than one. Kept so existing
	// callers of this published module keep compiling and behaving identically; a
	// value set here is OR-ed together with anything in Scopes.
	Scope string
	// Scopes optionally filters to one or more exact scope values. Several values
	// are OR-ed, which lets a UI present one filter option covering scopes a user
	// thinks of as the same thing — e.g. rows scoped to a user directly and rows
	// materialized onto them from an access profile. Empty (with Scope also empty)
	// means no scope filter.
	Scopes   []string
	ScopeID  string // optional; filters to an exact scope target (e.g. a virtual key or user ID)
	Provider string // optional; filters to an exact provider value (e.g. "openai")
}

// effectiveScopes returns the scope values to filter on, merging the deprecated
// single Scope with Scopes and dropping blanks and duplicates. Returns nil when
// neither is set, meaning "no scope filter".
func (p ModelConfigsQueryParams) effectiveScopes() []string {
	seen := make(map[string]struct{}, len(p.Scopes)+1)
	out := make([]string, 0, len(p.Scopes)+1)
	for _, s := range append([]string{p.Scope}, p.Scopes...) {
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// RoutingRulesQueryParams holds pagination, filtering, and search parameters for routing rules queries.
type RoutingRulesQueryParams struct {
	Limit  int
	Offset int
	Search string
}

// TeamsQueryParams holds pagination, filtering, and search parameters for team queries.
type TeamsQueryParams struct {
	Limit      int
	Offset     int
	Search     string
	CustomerID string
}

// CustomersQueryParams holds pagination, filtering, and search parameters for customer queries.
type CustomersQueryParams struct {
	Limit  int
	Offset int
	Search string
}

// PricingOverrideFilters holds the filters for pricing overrides.
type PricingOverrideFilters struct {
	ScopeKind     *string
	UserID        *string
	VirtualKeyID  *string
	ProviderID    *string
	ProviderKeyID *string
}

// PricingOverridesQueryParams holds pagination, filtering, and search parameters for pricing override queries.
type PricingOverridesQueryParams struct {
	Limit         int
	Offset        int
	Search        string
	ScopeKind     *string
	UserID        *string
	VirtualKeyID  *string
	ProviderID    *string
	ProviderKeyID *string
}

// ConfigStore is the interface for the config store.
type ConfigStore interface {
	// Health check
	Ping(ctx context.Context) error

	// Encryption
	EncryptPlaintextRows(ctx context.Context) error

	// Client config CRUD
	UpdateClientConfig(ctx context.Context, config *ClientConfig) error
	GetClientConfig(ctx context.Context) (*ClientConfig, error)
	// Client config metadata (UI/admin preferences blob — bypasses config.json sync)
	GetClientMetadata(ctx context.Context) (map[string]any, error)
	UpdateClientMetadata(ctx context.Context, patch map[string]any) error

	// Framework config CRUD
	UpdateFrameworkConfig(ctx context.Context, config *tables.TableFrameworkConfig) error
	GetFrameworkConfig(ctx context.Context) (*tables.TableFrameworkConfig, error)

	// Provider config CRUD
	UpdateProvidersConfig(ctx context.Context, providers map[schemas.ModelProvider]ProviderConfig, tx ...*gorm.DB) error
	AddProvider(ctx context.Context, provider schemas.ModelProvider, config ProviderConfig, tx ...*gorm.DB) error
	UpdateProvider(ctx context.Context, provider schemas.ModelProvider, config ProviderConfig, tx ...*gorm.DB) error
	DeleteProvider(ctx context.Context, provider schemas.ModelProvider, tx ...*gorm.DB) error
	GetProvidersConfig(ctx context.Context) (map[schemas.ModelProvider]ProviderConfig, error)
	GetProviderConfig(ctx context.Context, provider schemas.ModelProvider) (*ProviderConfig, error)
	GetProviderKeys(ctx context.Context, provider schemas.ModelProvider) ([]schemas.Key, error)
	GetProviderKey(ctx context.Context, provider schemas.ModelProvider, keyID string) (*schemas.Key, error)
	CreateProviderKey(ctx context.Context, provider schemas.ModelProvider, key schemas.Key, tx ...*gorm.DB) error
	UpdateProviderKey(ctx context.Context, provider schemas.ModelProvider, keyID string, key schemas.Key, tx ...*gorm.DB) error
	DeleteProviderKey(ctx context.Context, provider schemas.ModelProvider, keyID string, tx ...*gorm.DB) error
	GetProviders(ctx context.Context) ([]tables.TableProvider, error)
	GetProvider(ctx context.Context, provider schemas.ModelProvider) (*tables.TableProvider, error)
	UpdateStatus(ctx context.Context, provider schemas.ModelProvider, keyID string, status, errorMsg string) error

	// Vector store config CRUD
	UpdateVectorStoreConfig(ctx context.Context, config *vectorstore.Config) error
	GetVectorStoreConfig(ctx context.Context) (*vectorstore.Config, error)

	// Logs store config CRUD
	UpdateLogsStoreConfig(ctx context.Context, config *logstore.Config) error
	GetLogsStoreConfig(ctx context.Context) (*logstore.Config, error)

	// Config CRUD
	GetConfig(ctx context.Context, key string) (*tables.TableGovernanceConfig, error)
	UpdateConfig(ctx context.Context, config *tables.TableGovernanceConfig, tx ...*gorm.DB) error
	// GetComplexityAnalyzerConfig retrieves the persisted analyzer config, if configured.
	GetComplexityAnalyzerConfig(ctx context.Context) (*ComplexityAnalyzerConfig, error)
	// UpdateComplexityAnalyzerConfig persists the normalized analyzer config.
	UpdateComplexityAnalyzerConfig(ctx context.Context, config *ComplexityAnalyzerConfig, tx ...*gorm.DB) error
	// ResetComplexityAnalyzerConfig restores the tier boundaries and phrase lists from the
	// supplied defaults, preserves every other section of the stored record, and returns what
	// was persisted. Read and save share one transaction so a concurrent edit to a preserved
	// section cannot be overwritten.
	ResetComplexityAnalyzerConfig(ctx context.Context, defaults *ComplexityAnalyzerConfig) (*ComplexityAnalyzerConfig, error)

	// Model routers CRUD.
	GetModelRouters(ctx context.Context) ([]ModelRouter, error)
	GetModelRouter(ctx context.Context, id string) (*ModelRouter, error)
	CreateModelRouter(ctx context.Context, router *ModelRouter, tx ...*gorm.DB) error
	UpdateModelRouter(ctx context.Context, router *ModelRouter, tx ...*gorm.DB) error
	// EditModelRouter applies edit to the stored router under its row lock and
	// saves what edit changed; see RDBConfigStore.EditModelRouter.
	EditModelRouter(ctx context.Context, id string, edit func(*ModelRouter) (bool, error)) (*ModelRouter, error)
	DeleteModelRouter(ctx context.Context, id string, tx ...*gorm.DB) error
	ResetModelRouter(ctx context.Context, id string, defaults *ComplexityAnalyzerConfig) (*ModelRouter, error)
	// SetModelRouterTargets replaces the model each tier of a saved router gets.
	SetModelRouterTargets(ctx context.Context, routerID string, targets []tables.TableModelRouterTarget, tx ...*gorm.DB) error
	// SetModelRouterRouting replaces which requests a saved router takes.
	SetModelRouterRouting(ctx context.Context, routerID string, routing ModelRouterRouting, tx ...*gorm.DB) error
	// SetModelRouterRoutings replaces several routers' routing in one
	// transaction, checking priorities on the final state.
	SetModelRouterRoutings(ctx context.Context, changes []ModelRouterRoutingChange, tx ...*gorm.DB) error

	// Plugins CRUD
	GetPlugins(ctx context.Context) ([]*tables.TablePlugin, error)
	GetPlugin(ctx context.Context, name string) (*tables.TablePlugin, error)
	CreatePlugin(ctx context.Context, plugin *tables.TablePlugin, tx ...*gorm.DB) error
	UpsertPlugin(ctx context.Context, plugin *tables.TablePlugin, tx ...*gorm.DB) error
	UpdatePlugin(ctx context.Context, plugin *tables.TablePlugin, tx ...*gorm.DB) error
	DeletePlugin(ctx context.Context, name string, tx ...*gorm.DB) error

	// Governance config CRUD
	GetVirtualKeys(ctx context.Context) ([]tables.TableVirtualKey, error)
	GetVirtualKeysPaginated(ctx context.Context, params VirtualKeyQueryParams) ([]tables.TableVirtualKey, int64, error)
	// ListExpiredVirtualKeysForDeletion returns keys whose expiry has passed and whose
	// delete_after_expire is true, or unset when includeUnset is true.
	ListExpiredVirtualKeysForDeletion(ctx context.Context, now time.Time, includeUnset bool) ([]tables.TableVirtualKey, error)
	// DeleteExpiredVirtualKey deletes the key only if it is still expired and eligible
	// under the row lock, returning the deleted row, or nil when it no longer qualifies.
	DeleteExpiredVirtualKey(ctx context.Context, id string, now time.Time, includeUnset bool) (*tables.TableVirtualKey, error)
	GetRedactedVirtualKeys(ctx context.Context, ids []string) ([]tables.TableVirtualKey, error) // leave ids empty to get all
	GetVirtualKey(ctx context.Context, id string) (*tables.TableVirtualKey, error)
	GetVirtualKeyByValue(ctx context.Context, value string) (*tables.TableVirtualKey, error)
	GetVirtualKeyQuotaByValue(ctx context.Context, value string) (*tables.TableVirtualKey, error)
	CreateVirtualKey(ctx context.Context, virtualKey *tables.TableVirtualKey, tx ...*gorm.DB) error
	UpdateVirtualKey(ctx context.Context, virtualKey *tables.TableVirtualKey, tx ...*gorm.DB) error
	DeleteVirtualKey(ctx context.Context, id string, tx ...*gorm.DB) error

	// Virtual key provider config CRUD
	GetVirtualKeyProviderConfigs(ctx context.Context, virtualKeyID string) ([]tables.TableVirtualKeyProviderConfig, error)
	CreateVirtualKeyProviderConfig(ctx context.Context, virtualKeyProviderConfig *tables.TableVirtualKeyProviderConfig, tx ...*gorm.DB) error
	ReplaceVirtualKeyProviderConfigs(ctx context.Context, virtualKeyID string, virtualKeyProviderConfigs []tables.TableVirtualKeyProviderConfig, tx *gorm.DB) error
	UpdateVirtualKeyProviderConfig(ctx context.Context, virtualKeyProviderConfig *tables.TableVirtualKeyProviderConfig, tx ...*gorm.DB) error
	DeleteVirtualKeyProviderConfig(ctx context.Context, id uint, tx ...*gorm.DB) error

	// Team CRUD
	GetTeams(ctx context.Context, customerID string) ([]tables.TableTeam, error)
	GetTeamsPaginated(ctx context.Context, params TeamsQueryParams) ([]tables.TableTeam, int64, error)
	GetTeam(ctx context.Context, id string, tx ...*gorm.DB) (*tables.TableTeam, error)
	GetTeamByName(ctx context.Context, name string, customerID string) (*tables.TableTeam, error)
	GetTeamBySourceID(ctx context.Context, sourceID string) (*tables.TableTeam, error)
	CreateTeam(ctx context.Context, team *tables.TableTeam, tx ...*gorm.DB) error
	UpdateTeam(ctx context.Context, team *tables.TableTeam, tx ...*gorm.DB) error
	DeleteTeam(ctx context.Context, id string, tx ...*gorm.DB) error

	// Customer CRUD
	GetCustomers(ctx context.Context) ([]tables.TableCustomer, error)
	GetCustomersPaginated(ctx context.Context, params CustomersQueryParams) ([]tables.TableCustomer, int64, error)
	GetCustomer(ctx context.Context, id string, tx ...*gorm.DB) (*tables.TableCustomer, error)
	CreateCustomer(ctx context.Context, customer *tables.TableCustomer, tx ...*gorm.DB) error
	UpdateCustomer(ctx context.Context, customer *tables.TableCustomer, tx ...*gorm.DB) error
	DeleteCustomer(ctx context.Context, id string, tx ...*gorm.DB) error

	// Rate limit CRUD
	GetRateLimits(ctx context.Context) ([]tables.TableRateLimit, error)
	GetRateLimit(ctx context.Context, id string, tx ...*gorm.DB) (*tables.TableRateLimit, error)
	CreateRateLimit(ctx context.Context, rateLimit *tables.TableRateLimit, tx ...*gorm.DB) error
	UpdateRateLimit(ctx context.Context, rateLimit *tables.TableRateLimit, tx ...*gorm.DB) error
	UpdateRateLimits(ctx context.Context, rateLimits []*tables.TableRateLimit, tx ...*gorm.DB) error
	DeleteRateLimit(ctx context.Context, id string, tx ...*gorm.DB) error

	// Budget CRUD
	GetBudgets(ctx context.Context) ([]tables.TableBudget, error)
	GetBudget(ctx context.Context, id string, tx ...*gorm.DB) (*tables.TableBudget, error)
	// GetVirtualKeyBudgets returns the budgets a virtual key holds directly, through virtual_key_id:
	// the ones config.json declares inline on the key. Budgets held through a model config are not
	// included.
	GetVirtualKeyBudgets(ctx context.Context, virtualKeyID string, tx ...*gorm.DB) ([]tables.TableBudget, error)
	// GetVirtualKeyProviderConfigBudgets returns the budgets a virtual key provider config holds
	// directly, through provider_config_id. Budgets held through a model config are not included.
	GetVirtualKeyProviderConfigBudgets(ctx context.Context, providerConfigID uint, tx ...*gorm.DB) ([]tables.TableBudget, error)
	CreateBudget(ctx context.Context, budget *tables.TableBudget, tx ...*gorm.DB) error
	UpdateBudget(ctx context.Context, budget *tables.TableBudget, tx ...*gorm.DB) error
	// UpdateBudgetOverride updates only the override state and returns the refreshed budget.
	//
	// A finite grant is anchored at the budget's current window boundary so every
	// cluster node derives the same remaining-cycle count from it. calendarAligned
	// must come from the owning entity (virtual key, team, customer or access
	// profile) because the budget row does not persist it, and getting it wrong
	// anchors the grant on the wrong lattice.
	UpdateBudgetOverride(ctx context.Context, id string, amount float64, mode tables.BudgetOverrideMode, cyclesTotal int, calendarAligned bool, tx ...*gorm.DB) (*tables.TableBudget, error)
	UpdateBudgets(ctx context.Context, budgets []*tables.TableBudget, tx ...*gorm.DB) error
	DeleteBudget(ctx context.Context, id string, tx ...*gorm.DB) error
	UpdateBudgetUsage(ctx context.Context, id string, currentUsage float64, tx ...*gorm.DB) error
	UpdateRateLimitUsage(ctx context.Context, id string, tokenCurrentUsage int64, requestCurrentUsage int64, tx ...*gorm.DB) error

	// Routing Rules CRUD
	GetRoutingRules(ctx context.Context) ([]tables.TableRoutingRule, error)
	GetRoutingRulesByScope(ctx context.Context, scope string, scopeID string) ([]tables.TableRoutingRule, error)
	GetRoutingRule(ctx context.Context, id string) (*tables.TableRoutingRule, error)
	GetRedactedRoutingRules(ctx context.Context, ids []string) ([]tables.TableRoutingRule, error) // leave ids empty to get all
	GetRoutingRulesPaginated(ctx context.Context, params RoutingRulesQueryParams) ([]tables.TableRoutingRule, int64, error)
	CreateRoutingRule(ctx context.Context, rule *tables.TableRoutingRule, tx ...*gorm.DB) error
	UpdateRoutingRule(ctx context.Context, rule *tables.TableRoutingRule, tx ...*gorm.DB) error
	// SyncRoutingRules applies a batch of creates and updates atomically, deferring the
	// unique-priority-per-scope check until all rules are written so that a valid permutation
	// (e.g. swapping two rules' priorities) succeeds despite a transient intermediate collision.
	SyncRoutingRules(ctx context.Context, toAdd []tables.TableRoutingRule, toUpdate []tables.TableRoutingRule, tx ...*gorm.DB) error
	DeleteRoutingRule(ctx context.Context, id string, tx ...*gorm.DB) error

	// Model config CRUD
	// GetModelConfigs reads every model config with no row-visibility scope. It serves internal
	// callers that must see all rows (the governance load, the name collision check); a listing
	// for a caller goes through GetModelConfigsPaginated.
	GetModelConfigs(ctx context.Context) ([]tables.TableModelConfig, error)
	GetModelConfigsByScopeAndScopeIDs(ctx context.Context, scope string, scopeIDs []string, tx ...*gorm.DB) ([]tables.TableModelConfig, error)
	GetProviderGovernanceModelConfigs(ctx context.Context) ([]tables.TableModelConfig, error)
	GetModelConfigsPaginated(ctx context.Context, params ModelConfigsQueryParams) ([]tables.TableModelConfig, int64, error)
	GetModelConfig(ctx context.Context, scope string, scopeID *string, modelName string, provider *string) (*tables.TableModelConfig, error)
	GetModelConfigByID(ctx context.Context, id string) (*tables.TableModelConfig, error)
	CreateModelConfig(ctx context.Context, modelConfig *tables.TableModelConfig, tx ...*gorm.DB) error
	UpdateModelConfig(ctx context.Context, modelConfig *tables.TableModelConfig, tx ...*gorm.DB) error
	UpdateModelConfigs(ctx context.Context, modelConfigs []*tables.TableModelConfig, tx ...*gorm.DB) error
	DeleteModelConfig(ctx context.Context, id string, tx ...*gorm.DB) error
	// DeleteModelConfigsForScope deletes all model configs (and their owned budgets/rate-limits) for a scope owner. Must run inside the owner-delete transaction.
	DeleteModelConfigsForScope(ctx context.Context, tx *gorm.DB, scope, scopeID string) error
	// GetModelConfigsForScope loads all model configs (with Budgets/RateLimit) for a scope owner
	// within the given transaction, so a caller mid-transaction sees its own uncommitted writes
	// (e.g. deletions staged earlier in the same tx) rather than the last-committed state.
	GetModelConfigsForScope(ctx context.Context, tx *gorm.DB, scope, scopeID string) ([]tables.TableModelConfig, error)

	// Governance config CRUD
	GetGovernanceConfig(ctx context.Context) (*GovernanceConfig, error)

	// Auth config CRUD
	GetAuthConfig(ctx context.Context) (*AuthConfig, error)
	UpdateAuthConfig(ctx context.Context, config *AuthConfig) error

	// Proxy config CRUD
	GetProxyConfig(ctx context.Context) (*tables.GlobalProxyConfig, error)
	UpdateProxyConfig(ctx context.Context, config *tables.GlobalProxyConfig) error

	// Restart required config CRUD
	GetRestartRequiredConfig(ctx context.Context) (*tables.RestartRequiredConfig, error)
	SetRestartRequiredConfig(ctx context.Context, config *tables.RestartRequiredConfig) error
	ClearRestartRequiredConfig(ctx context.Context) error

	// Model pricing CRUD
	GetModelPrices(ctx context.Context) ([]tables.TableModelPricing, error)
	UpsertModelPrices(ctx context.Context, pricing *tables.TableModelPricing, tx ...*gorm.DB) error
	UpsertModelPricesBatch(ctx context.Context, pricing []tables.TableModelPricing, tx ...*gorm.DB) error
	DeleteModelPrices(ctx context.Context, tx ...*gorm.DB) error

	// UpsertModelPricingAttributes writes only the additional_attributes column
	// on the pricing rows keyed by (model, provider). Returns the number of
	// rows updated; 0 means no such pricing row exists.
	UpsertModelPricingAttributes(ctx context.Context, model, provider string, attrs map[string]string, tx ...*gorm.DB) (int64, error)

	// Governance pricing overrides CRUD
	GetPricingOverrides(ctx context.Context, filters PricingOverrideFilters) ([]tables.TablePricingOverride, error)
	GetPricingOverridesPaginated(ctx context.Context, params PricingOverridesQueryParams) ([]tables.TablePricingOverride, int64, error)
	GetPricingOverrideByID(ctx context.Context, id string) (*tables.TablePricingOverride, error)
	CreatePricingOverride(ctx context.Context, override *tables.TablePricingOverride, tx ...*gorm.DB) error
	UpdatePricingOverride(ctx context.Context, override *tables.TablePricingOverride, tx ...*gorm.DB) error
	DeletePricingOverride(ctx context.Context, id string, tx ...*gorm.DB) error

	// Datasheet overrides: user edits re-applied on top of every datasheet sync
	GetDatasheetOverrides(ctx context.Context) ([]tables.TableDatasheetOverride, error)
	GetDatasheetOverrideByID(ctx context.Context, id string) (*tables.TableDatasheetOverride, error)
	UpsertDatasheetOverride(ctx context.Context, override *tables.TableDatasheetOverride, tx ...*gorm.DB) error
	DeleteDatasheetOverride(ctx context.Context, id string, tx ...*gorm.DB) error

	// Model parameters
	GetModelParameters(ctx context.Context) ([]tables.TableModelParameters, error)
	GetModelParametersByModel(ctx context.Context, model string) (*tables.TableModelParameters, error)
	UpsertModelParameters(ctx context.Context, params *tables.TableModelParameters, tx ...*gorm.DB) error
	UpsertModelParametersBatch(ctx context.Context, params []tables.TableModelParameters, tx ...*gorm.DB) error

	// Key management
	GetKeysByIDs(ctx context.Context, ids []string) ([]tables.TableKey, error)
	GetKeysByProvider(ctx context.Context, provider string) ([]tables.TableKey, error)
	GetAllRedactedKeys(ctx context.Context, ids []string) ([]schemas.Key, error) // leave ids empty to get all

	// Generic transaction manager
	ExecuteTransaction(ctx context.Context, fn func(tx *gorm.DB) error) error

	// TryAcquireLock attempts to insert a lock row. Returns true if the lock was acquired.
	// If the lock already exists and is not expired, returns false.
	TryAcquireLock(ctx context.Context, lock *tables.TableDistributedLock) (bool, error)

	// GetLock retrieves a lock by its key. Returns nil if the lock doesn't exist.
	GetLock(ctx context.Context, lockKey string) (*tables.TableDistributedLock, error)

	// UpdateLockExpiry updates the expiration time for an existing lock.
	// Only succeeds if the holder ID matches the current lock holder.
	UpdateLockExpiry(ctx context.Context, lockKey, holderID string, expiresAt time.Time) error

	// ReleaseLock deletes a lock if the holder ID matches.
	// Returns true if the lock was released, false if it wasn't held by the given holder.
	ReleaseLock(ctx context.Context, lockKey, holderID string) (bool, error)

	// CleanupExpiredLockByKey atomically deletes a specific lock only if it has expired.
	// Returns true if an expired lock was deleted, false if the lock doesn't exist or hasn't expired.
	CleanupExpiredLockByKey(ctx context.Context, lockKey string) (bool, error)

	// CleanupExpiredLocks removes all locks that have expired.
	// Returns the number of locks cleaned up.
	CleanupExpiredLocks(ctx context.Context) (int64, error)

	// Not found retry wrapper
	RetryOnNotFound(ctx context.Context, fn func(ctx context.Context) (any, error), maxRetries int, retryDelay time.Duration) (any, error)

	// Sidekiq - generic durable background jobs
	CreateSidekiqJob(ctx context.Context, job *tables.TableSidekiqJob) error
	GetSidekiqJob(ctx context.Context, id string) (*tables.TableSidekiqJob, error)
	ClaimSidekiqJob(ctx context.Context, id, runnerID string, staleBefore time.Time) (bool, error)
	ClaimPartitionedSidekiqJob(ctx context.Context, id, runnerID string, staleBefore time.Time, partitioningKey string, createdAt time.Time) (bool, error)
	HeartbeatSidekiqJob(ctx context.Context, id, runnerID string) (bool, error)
	UpdateSidekiqJobProgress(ctx context.Context, id, runnerID, metadata string) error
	CompleteSidekiqJob(ctx context.Context, id, runnerID, metadata string) error
	FailSidekiqJob(ctx context.Context, id, runnerID, metadata, lastErr string) error
	CancelSidekiqJob(ctx context.Context, id string) (bool, error)
	FinalizeCancelledSidekiqJob(ctx context.Context, id, runnerID, metadata string) error
	ListClaimableSidekiqJobs(ctx context.Context, staleBefore time.Time) ([]tables.TableSidekiqJob, error)
	GetInFlightSidekiqJobByKind(ctx context.Context, kind string) (*tables.TableSidekiqJob, error)
	GetLatestSidekiqJobByKind(ctx context.Context, kind string) (*tables.TableSidekiqJob, error)
	ListSidekiqJobs(ctx context.Context, terminalSince time.Time, limit int, quietKinds []string) ([]tables.TableSidekiqJob, error)
	MarkStaleSidekiqJobsFailed(ctx context.Context, staleBefore time.Time) (int64, error)

	// Phrase-suggestion schedule state: the next run of a router's schedule,
	// claimed by one node when due. On the interface rather than read off the
	// concrete store, so a store that wraps it keeps the shared schedule.
	GetComplexitySuggestionSchedule(ctx context.Context, routerID string) (*tables.TableComplexitySuggestionSchedule, error)
	SetComplexitySuggestionSchedule(ctx context.Context, routerID, scheduleHash string, nextRunAt time.Time, readVersion *int64) (bool, error)
	ClaimComplexitySuggestionRun(ctx context.Context, routerID string, version int64, dueAt, nextRunAt time.Time, runID string) (bool, error)

	// Batch jobs - mutable coordination state for delayed batch accounting
	UpsertProviderJob(ctx context.Context, job *tables.TableProviderJob) error
	GetProviderJob(ctx context.Context, jobID string) (*tables.TableProviderJob, error)
	// GetProviderJobsByIDs returns the provider jobs among the given stable ids
	// that exist; ids with no row are simply absent from the result.
	GetProviderJobsByIDs(ctx context.Context, jobIDs []string) ([]*tables.TableProviderJob, error)
	ListDueProviderJobs(ctx context.Context, kind, provider string, now time.Time, limit int) ([]*tables.TableProviderJob, error)
	ClaimProviderJob(ctx context.Context, jobID, runnerID string, staleBefore time.Time, allowUnpriceable bool) (bool, error)
	MarkProviderJobAggregateLogWritten(ctx context.Context, jobID, runnerID string) error
	MarkProviderJobGovernanceReported(ctx context.Context, jobID, runnerID string) error
	CompleteProviderJob(ctx context.Context, jobID, runnerID string) error
	MarkProviderJobUnpriceable(ctx context.Context, jobID, runnerID, reason string, err error) error
	FailProviderJob(ctx context.Context, jobID, runnerID string, err error) error
	// GetProviderJobsByFileIDs returns the provider jobs on provider whose input,
	// output or error file is one of fileIDs.
	GetProviderJobsByFileIDs(ctx context.Context, provider string, fileIDs []string) ([]*tables.TableProviderJob, error)
	// GetProviderJobsByJobIDSuffixes returns the provider jobs of kind on provider
	// whose provider-side id ends in "/"+one of suffixes: full resource names
	// addressed by their last segment.
	GetProviderJobsByJobIDSuffixes(ctx context.Context, kind, provider string, suffixes []string) ([]*tables.TableProviderJob, error)

	// Provider objects - which virtual key created a provider-side object
	UpsertProviderObject(ctx context.Context, object *tables.TableProviderObject) error
	// GetProviderObjectsByIDs returns the provider objects among the given stable
	// ids that exist; ids with no row are simply absent from the result.
	GetProviderObjectsByIDs(ctx context.Context, ids []string) ([]*tables.TableProviderObject, error)
	// GetProviderObjectsByObjectIDSuffixes returns the provider objects of kind on
	// provider whose provider-side id ends in "/"+one of suffixes: full resource
	// names addressed by their last segment.
	GetProviderObjectsByObjectIDSuffixes(ctx context.Context, kind, provider string, suffixes []string) ([]*tables.TableProviderObject, error)
	// GetProviderObjectsByObjectID returns the provider objects of kind recorded for
	// objectID under any provider name, in no particular order.
	GetProviderObjectsByObjectID(ctx context.Context, kind, objectID string) ([]*tables.TableProviderObject, error)
	DeleteProviderObject(ctx context.Context, id string) error
	// DeleteProviderObjectsByObjectIDSuffix forgets the provider objects of kind on
	// provider that virtualKeyID created and whose provider-side id ends in
	// "/"+suffix: a full resource name addressed by its last segment.
	DeleteProviderObjectsByObjectIDSuffix(ctx context.Context, kind, provider, suffix, virtualKeyID string) error

	// DB returns the underlying database connection.
	DB() *gorm.DB

	// ScopedDB returns the underlying DB bound to ctx with any
	// QueryScope on ctx pre-applied, or the caller's transaction when one is
	// passed, so a read taken inside a transaction sees that transaction's own
	// writes. The scope is applied either way. Use this in read paths that
	// should respect caller-driven row visibility; use DB().WithContext(ctx)
	// for writes and internal lookups that must bypass scoping.
	ScopedDB(ctx context.Context, tx ...*gorm.DB) *gorm.DB

	// RunMigration opens a throwaway *gorm.DB against the same
	// backing database, invokes fn with it, and closes the connection. Use
	// this for DDL (typically downstream-consumer migrations) that must not
	// leave cached prepared-statement plans on the runtime pool.
	//
	// After fn returns successfully, callers should invoke
	// RefreshConnectionPool if the migration altered tables the runtime pool
	// has already queried — otherwise SQLSTATE 0A000 can surface on reads
	// whose cached plans predate the DDL.
	//
	// For SQLite backends, this is a pass-through that runs fn on the
	// existing connection (no server-side plan cache, single-writer lock).
	RunMigration(ctx context.Context, fn func(context.Context, *gorm.DB) error) error

	// RefreshConnectionPool tears down the runtime pool and opens a fresh
	// one against the same configuration. In-flight queries on the old
	// pool complete before it closes; subsequent DB() calls return the new
	// pool, whose connections carry no cached plans. SQLite is a no-op.
	RefreshConnectionPool(ctx context.Context) error

	// Cleanup
	Close(ctx context.Context) error
}

// NewConfigStore creates a new config store based on the configuration
func NewConfigStore(ctx context.Context, config *Config, logger schemas.Logger, opts ...Option) (ConfigStore, error) {
	var o storeOptions
	for _, opt := range opts {
		opt(&o)
	}
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if !config.Enabled {
		return nil, nil
	}
	logger.Info("connecting to %s database", config.Type)
	switch config.Type {
	case ConfigStoreTypeSQLite:
		if sqliteConfig, ok := config.Config.(*SQLiteConfig); ok {
			return newSqliteConfigStore(ctx, sqliteConfig, logger)
		}
		return nil, fmt.Errorf("invalid sqlite config: %T", config.Config)
	case ConfigStoreTypePostgres:
		if postgresConfig, ok := config.Config.(*PostgresConfig); ok {
			return newPostgresConfigStore(ctx, postgresConfig, logger, o.migrationMatViewPause)
		}
		return nil, fmt.Errorf("invalid postgres config: %T", config.Config)
	}
	return nil, fmt.Errorf("unsupported config store type: %s", config.Type)
}

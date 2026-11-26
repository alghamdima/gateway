package configstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/encrypt"
	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/maximhq/bifrost/framework/migrator"
	"github.com/maximhq/bifrost/framework/postgresconn"
	"github.com/maximhq/bifrost/framework/queryscope"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// addColumnIfNotExists is a package-local alias for migrator.AddColumnIfNotExists,
// the idempotent column-add helper shared with logstore. Declared at package
// scope (where `migrator` resolves to the package, not the `migrator :=
// tx.Migrator()` locals inside migration closures) so every call site can keep
// calling addColumnIfNotExists(tx, ...) directly.
var addColumnIfNotExists = migrator.AddColumnIfNotExists

// dropColumnIfExists is the drop counterpart to addColumnIfNotExists: a
// package-local alias for migrator.DropColumnIfExists, the idempotent
// column-drop helper shared with logstore. Declared at package scope for the
// same reason as above so call sites can use dropColumnIfExists(tx, ...).
var dropColumnIfExists = migrator.DropColumnIfExists

// migrationAdvisoryLockKey is used for PostgreSQL advisory locks
// to serialize migrations across cluster nodes
const migrationAdvisoryLockKey = 1000001

// Lock-wait tuning. Variables only so tests can shorten them.
var (
	// advisoryLockRetryInterval is how long to wait between lock acquisition attempts.
	advisoryLockRetryInterval = 5 * time.Second

	// advisoryLockTimeout is how long to wait for the advisory lock while its
	// holder shows no sign of work (a crashed instance's lingering session) before
	// giving up with actionable operator guidance.
	advisoryLockTimeout = 1 * time.Minute

	// advisoryLockBusyTimeout is how long to wait while the holder is visibly
	// running statements: a live instance whose migrations are slow, for example a
	// CREATE INDEX CONCURRENTLY waiting on another replica's long transaction.
	// Giving up on it only crash-loops this instance without speeding anything up.
	advisoryLockBusyTimeout = 20 * time.Minute
)

// pauseMatViewsForMigrations pauses materialized-view refreshes for a migration
// run, and migrationMatViewPause is the pause used when the caller configures
// none (see WithMigrationMatViewPause). Variables so tests can shorten the cancel
// delay or switch the pause off to reproduce the pre-pause behaviour.
var (
	pauseMatViewsForMigrations = logstore.PauseMatViewsForMigrations
	migrationMatViewPause      = logstore.MatViewPauseOptions{CancelAfter: logstore.DefaultMatViewPauseCancelAfter}
)

// migrationLock holds a dedicated connection for the advisory lock.
// This ensures the lock is held on the same connection throughout migrations,
// preventing race conditions caused by GORM's connection pooling.
type migrationLock struct {
	conn *sql.Conn
}

// acquireMigrationLock gets a dedicated connection and acquires an advisory lock
// using pg_try_advisory_lock with retry + timeout. This prevents pods from
// blocking indefinitely if a previous pod crashed without releasing the lock
// (e.g., behind a connection proxy or with slow TCP keepalive detection), while
// still waiting out a live holder that is doing slow but legitimate work.
// For non-PostgreSQL databases, returns a no-op lock.
func acquireMigrationLock(ctx context.Context, db *gorm.DB, logger schemas.Logger) (*migrationLock, error) {
	if db.Dialector.Name() != "postgres" {
		return &migrationLock{}, nil
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to get sql.DB: %w", err)
	}

	// Get a dedicated connection (not returned to pool until Close())
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get dedicated connection: %w", err)
	}

	logger.Info("[configstore] attempting to get migration lock %d", migrationAdvisoryLockKey)
	if err := postgresconn.WaitForAdvisoryLock(ctx, conn, logger, postgresconn.AdvisoryWaitOptions{
		Key:           migrationAdvisoryLockKey,
		Label:         "configstore migration",
		LogPrefix:     "[configstore]",
		RetryInterval: advisoryLockRetryInterval,
		Timeout:       advisoryLockTimeout,
		BusyTimeout:   advisoryLockBusyTimeout,
	}); err != nil {
		conn.Close()
		return nil, err
	}
	return &migrationLock{conn: conn}, nil
}

// release unlocks and closes the dedicated connection
func (l *migrationLock) release(ctx context.Context) {
	if l.conn == nil {
		return
	}
	// Release lock on the SAME connection that acquired it
	_, _ = l.conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", migrationAdvisoryLockKey)
	l.conn.Close()
}

// RunSingleMigration applies a single gormigrate migration on the given
// *gorm.DB. Mirrors (*RDBConfigStore).RunMigration but takes the *gorm.DB
// directly, so downstream consumers (bifrost-enterprise, plugins) can run
// their migrations inside a MigrateOnFreshConnection callback without having
// to reach the throwaway pool through the ConfigStore abstraction.
func RunSingleMigration(ctx context.Context, options *migrator.Options, db *gorm.DB, logger schemas.Logger, migration *migrator.Migration) error {
	if db == nil {
		return fmt.Errorf("db cannot be nil")
	}
	if migration == nil {
		return fmt.Errorf("migration cannot be nil")
	}
	migrationOpts := migrator.DefaultOptions
	if options != nil {
		migrationOpts = options
	}
	m := migrator.New(db.WithContext(ctx), migrationOpts, []*migrator.Migration{migration})
	return m.Migrate()
}

// legacyBudgetVirtualKey holds the legacy budget virtual key model.
type legacyBudgetVirtualKey struct {
	tables.TableVirtualKey
	BudgetID *string `gorm:"column:budget_id;type:varchar(255);index"`
}

// TableName returns the governance_virtual_keys table name for legacyBudgetVirtualKey.
func (legacyBudgetVirtualKey) TableName() string { return "governance_virtual_keys" }

// legacyBudgetVirtualKeyProviderConfig holds the legacy budget virtual key provider config model.
type legacyBudgetVirtualKeyProviderConfig struct {
	tables.TableVirtualKeyProviderConfig
	BudgetID *string `gorm:"column:budget_id;type:varchar(255);index"`
}

// TableName returns the governance_virtual_key_provider_configs table name for legacyBudgetVirtualKeyProviderConfig.
func (legacyBudgetVirtualKeyProviderConfig) TableName() string {
	return "governance_virtual_key_provider_configs"
}

// legacyBudgetTeam holds the legacy budget team model.
type legacyBudgetTeam struct {
	tables.TableTeam
	BudgetID *string `gorm:"column:budget_id;type:varchar(255);index"`
}

// TableName returns the governance_teams table name for legacyBudgetTeam.
func (legacyBudgetTeam) TableName() string { return "governance_teams" }

// sqliteColumnInfo holds the information about a SQLite column.
type sqliteColumnInfo struct {
	Name string `gorm:"column:name"`
}

// legacyBudgetColumnModel returns the legacy budget column model for a given table name.
func legacyBudgetColumnModel(tableName string) (any, error) {
	switch tableName {
	case "governance_virtual_keys":
		return &legacyBudgetVirtualKey{}, nil
	case "governance_virtual_key_provider_configs":
		return &legacyBudgetVirtualKeyProviderConfig{}, nil
	case "governance_teams":
		return &legacyBudgetTeam{}, nil
	default:
		return nil, fmt.Errorf("unsupported legacy budget column drop table: %s", tableName)
	}
}

// currentBudgetOwnerModel returns the current budget owner model for a given table name.
func currentBudgetOwnerModel(tableName string) (any, error) {
	switch tableName {
	case "governance_virtual_keys":
		return &tables.TableVirtualKey{}, nil
	case "governance_virtual_key_provider_configs":
		return &tables.TableVirtualKeyProviderConfig{}, nil
	case "governance_teams":
		return &tables.TableTeam{}, nil
	default:
		return nil, fmt.Errorf("unsupported legacy budget column drop table: %s", tableName)
	}
}

// migrationStep records the migration IDs written by one migration function.
// Most functions write one ID, but a few grouped migrations write multiple IDs.
type migrationStep struct {
	IDs []string
	run func(context.Context, *gorm.DB, schemas.Logger) error
}

// migrationStepIDs flattens migration step IDs in execution order.
func migrationStepIDs(steps []migrationStep) []string {
	ids := make([]string, 0, len(steps))
	for _, step := range steps {
		ids = append(ids, step.IDs...)
	}
	return ids
}

// pendingMigrationStepIDs returns the migration IDs from steps that are not
// recorded in the migration table yet.
func pendingMigrationStepIDs(ctx context.Context, db *gorm.DB, steps []migrationStep) ([]string, error) {
	return migrator.PendingIDs(ctx, db, migrator.DefaultOptions, migrationStepIDs(steps))
}

// migrationStepLockRetry tunes how runMigrationStepsWithRetry retries a step that fails
// on a lock timeout. The zero value takes postgresconn's defaults; a variable
// only so tests can shrink the budget and backoff.
var migrationStepLockRetry postgresconn.LockRetryOptions

// runMigrationStepsWithRetry runs migration steps in their declared order. It reads
// the pending IDs once and skips steps whose IDs are all recorded, so a deploy with
// one new migration does not pay a round trip per already-applied step. If the
// preflight read fails, every step runs and each one checks its own row. A step
// that fails on a lock timeout is retried within the retry budget; a zero budget
// takes postgresconn's default.
func runMigrationStepsWithRetry(ctx context.Context, db *gorm.DB, logger schemas.Logger, steps []migrationStep, retry postgresconn.LockRetryOptions) error {
	pending, err := pendingMigrationStepIDs(ctx, db, steps)
	var pendingSet map[string]struct{}
	if err != nil {
		logger.Warn("[configstore] migration preflight failed; running every step: %v", err)
	} else {
		pendingSet = make(map[string]struct{}, len(pending))
		for _, id := range pending {
			pendingSet[id] = struct{}{}
		}
	}
	for _, step := range steps {
		if pendingSet != nil && !stepHasPendingID(step, pendingSet) {
			continue
		}
		// A step that times out waiting for a table lock (another session holds a
		// conflicting lock, e.g. a long read on the table being altered) is retried
		// with backoff instead of failing the boot. Steps are idempotent: the
		// migrator records completed IDs and the DDL helpers use IF [NOT] EXISTS.
		label := "[configstore] migration " + strings.Join(step.IDs, ",")
		if err := postgresconn.RunWithLockRetry(ctx, logger, label, retry, func() error {
			return step.run(ctx, db, logger)
		}); err != nil {
			return err
		}
	}
	return nil
}

// stepHasPendingID reports whether any ID the step writes is still pending.
func stepHasPendingID(step migrationStep, pending map[string]struct{}) bool {
	for _, id := range step.IDs {
		if _, ok := pending[id]; ok {
			return true
		}
	}
	return false
}

// configstoreMigrationSteps is the ordered source of truth for configstore
// migration execution and preflight checks.
var configstoreMigrationSteps = []migrationStep{
	{IDs: []string{"init"}, run: migrationInit},
	{IDs: []string{"many2manyjoin"}, run: migrationMany2ManyJoinTable},
	{IDs: []string{"addcustomproviderconfigjsoncolumn"}, run: migrationAddCustomProviderConfigJSONColumn},
	{IDs: []string{"addvirtualkeyproviderconfig"}, run: migrationAddVirtualKeyProviderConfigTable},
	{IDs: []string{"add_allowed_origins_json_column"}, run: migrationAddAllowedOriginsJSONColumn},
	{IDs: []string{"add_allow_direct_keys_column"}, run: migrationAddAllowDirectKeysColumn},
	{IDs: []string{"add_enable_litellm_fallbacks_column"}, run: migrationAddEnableLiteLLMFallbacksColumn},
	{IDs: []string{"add_profile_config_claims_columns_to_team_table"}, run: migrationTeamsTableUpdates},
	{IDs: []string{"add_team_source_id_column"}, run: migrationAddTeamSourceIDColumn},
	{IDs: []string{"add_key_name_column"}, run: migrationAddKeyNameColumn},
	{IDs: []string{"add_framework_configs_table"}, run: migrationAddFrameworkConfigsTable},
	{IDs: []string{"update_plugins_table_for_custom_plugins"}, run: migrationAddPluginPathColumn},
	{IDs: []string{"add_provider_config_budget_rate_limit"}, run: migrationAddProviderConfigBudgetRateLimit},
	{IDs: []string{"add_sessions_table"}, run: migrationAddSessionsTable},
	{IDs: []string{"add_disable_content_logging_column"}, run: migrationAddDisableContentLoggingColumn},
	{IDs: []string{"add_vertex_project_number_column"}, run: migrationAddVertexProjectNumberColumn},
	{IDs: []string{"add_vertex_deployments_json_column"}, run: migrationAddVertexDeploymentsJSONColumn},
	{IDs: []string{"add_and_fill_provider_column_in_key_table"}, run: migrationMissingProviderColumnInKeyTable},
	{IDs: []string{"add_log_retention_days_column"}, run: migrationAddLogRetentionDaysColumn},
	{IDs: []string{"add_enabled_column_to_key_table"}, run: migrationAddEnabledColumnToKeyTable},
	{IDs: []string{"update_model_pricing_table_to_add_cache_and_batch_pricing"}, run: migrationAddBatchAndCachePricingColumns},
	{IDs: []string{"move_keys_to_provider_config"}, run: migrationMoveKeysToProviderConfig},
	{IDs: []string{"add_plugin_version_column"}, run: migrationAddPluginVersionColumn},
	{IDs: []string{"add_send_back_raw_request_columns"}, run: migrationAddSendBackRawRequestColumns},
	{IDs: []string{"add_config_hash_column"}, run: migrationAddConfigHashColumn},
	{IDs: []string{"add_virtual_key_config_hash_column"}, run: migrationAddVirtualKeyConfigHashColumn},
	{IDs: []string{"add_additional_config_hash_columns"}, run: migrationAddAdditionalConfigHashColumns},
	{IDs: []string{"add_200k_token_pricing_columns"}, run: migrationAdd200kTokenPricingColumns},
	{IDs: []string{"add_image_pricing_columns"}, run: migrationAddImagePricingColumns},
	{IDs: []string{"add_use_for_batch_api_column"}, run: migrationAddUseForBatchAPIColumnAndS3BucketsConfig},
	{IDs: []string{"add_header_filter_config_json_column"}, run: migrationAddHeaderFilterConfigJSONColumn},
	{IDs: []string{"add_azure_client_id_and_client_secret_and_tenant_id_columns"}, run: migrationAddAzureClientIDAndClientSecretAndTenantIDColumns},
	{IDs: []string{"add_distributed_locks_table"}, run: migrationAddDistributedLocksTable},
	{IDs: []string{"add_model_config_table"}, run: migrationAddModelConfigTable},
	{IDs: []string{"add_provider_governance_columns"}, run: migrationAddProviderGovernanceColumns},
	{IDs: []string{"add_allowed_headers_json_column"}, run: migrationAddAllowedHeadersJSONColumn},
	{IDs: []string{"add_disable_db_pings_in_health_column"}, run: migrationAddDisableDBPingsInHealthColumn},
	{IDs: []string{"add_routing_rules_table"}, run: migrationAddRoutingRulesTable},
	{IDs: []string{"add_base_model_pricing_column"}, run: migrationAddBaseModelPricingColumn},
	{IDs: []string{"add_azure_scopes_column"}, run: migrationAddAzureScopesColumn},
	{IDs: []string{"add_replicate_deployments_json_column"}, run: migrationAddReplicateDeploymentsJSONColumn},
	{IDs: []string{"add_key_status_columns"}, run: migrationAddKeyStatusColumns},
	{IDs: []string{"add_provider_status_columns"}, run: migrationAddProviderStatusColumns},
	{IDs: []string{"add_rate_limit_to_teams_and_customers"}, run: migrationAddRateLimitToTeamsAndCustomers},
	{IDs: []string{"add_async_job_result_ttl_column"}, run: migrationAddAsyncJobResultTTLColumn},
	{IDs: []string{"add_required_headers_json_column"}, run: migrationAddRequiredHeadersJSONColumn},
	{IDs: []string{"add_logging_headers_json_column"}, run: migrationAddLoggingHeadersJSONColumn},
	{IDs: []string{"add_hide_deleted_virtual_keys_in_filters_column"}, run: migrationAddHideDeletedVirtualKeysInFiltersColumn},
	{IDs: []string{"add_enforce_scim_auth_column"}, run: migrationAddEnforceSCIMAuthColumn},
	{IDs: []string{"add_enforce_auth_on_inference_column"}, run: migrationAddEnforceAuthOnInferenceColumn},
	{IDs: []string{"reconcile_pricing_overrides_table"}, run: migrationReconcilePricingOverridesTable},
	{IDs: []string{"add_encryption_columns"}, run: migrationAddEncryptionColumns},
	{IDs: []string{"add_output_cost_per_video_per_second_and_output_cost_per_second_columns"}, run: migrationAddOutputCostPerVideoPerSecond},
	{IDs: []string{"drop_enable_governance_column"}, run: migrationDropEnableGovernanceColumn},
	{IDs: []string{"add_vllm_key_config_columns"}, run: migrationAddVLLMKeyConfigColumns},
	{IDs: []string{"widen_encrypted_varchar_columns"}, run: migrationWidenEncryptedVarcharColumns},
	{IDs: []string{"add_bedrock_assume_role_columns"}, run: migrationAddBedrockAssumeRoleColumns},
	{IDs: []string{"add_store_raw_request_response_column"}, run: migrationAddStoreRawRequestResponseColumn},
	{IDs: []string{"add_pricing_refactor_columns"}, run: migrationAddPricingRefactorColumns},
	{IDs: []string{"rename_truncated_pricing_column"}, run: migrationRenameTruncatedPricingColumn},
	{IDs: []string{"add_image_quality_pricing_columns"}, run: migrationAddImageQualityPricingColumns},
	{IDs: []string{"add_routing_targets_table"}, run: migrationAddRoutingTargetsTable},
	{IDs: []string{"add_model_parameters_table"}, run: migrationAddModelParametersTable},
	{IDs: []string{"add_plugin_order_columns"}, run: migrationAddPluginOrderColumns},
	{IDs: []string{"add_allow_all_keys_to_provider_config"}, run: migrationAddAllowAllKeysToProviderConfig},
	{IDs: []string{"add_vk_provider_config_blacklisted_models_column"}, run: migrationAddVirtualKeyBlacklistedModelsColumn},
	{IDs: []string{"backfill_empty_virtual_key_configs"}, run: migrationBackfillEmptyVirtualKeyConfigs},
	{IDs: []string{"backfill_allowed_models_wildcard"}, run: migrationBackfillAllowedModelsWildcard},
	{IDs: []string{"make_base_pricing_columns_nullable"}, run: migrationMakeBasePricingColumnsNullable},
	{IDs: []string{"add_open_ai_config_json_column"}, run: migrationAddOpenAIConfigJSONColumn},
	{IDs: []string{"add_key_blacklisted_models_json_column"}, run: migrationAddKeyBlacklistedModelsJSONColumn},
	{IDs: []string{"add_chain_rule_column_to_routing_rules"}, run: migrationAddChainRuleColumnToRoutingRules},
	{IDs: []string{"drop_deployment_columns_and_add_aliases"}, run: migrationDropDeploymentColumnsAndAddAliases},
	{IDs: []string{"add_replicate_key_config_column"}, run: migrationAddReplicateKeyConfigColumn},
	{IDs: []string{"add_budget_calendar_aligned_column"}, run: migrationAddBudgetCalendarAlignedColumn},
	{IDs: []string{"add_routing_chain_max_depth_column"}, run: migrationAddRoutingChainMaxDepthColumn},
	{IDs: []string{"add_model_capability_columns"}, run: migrationAddModelCapabilityColumns},
	{IDs: []string{"add_ollama_sgl_config_columns"}, run: migrationAddOllamaSGLConfigColumns},
	{IDs: []string{"add_multi_budget_tables"}, run: migrationAddMultiBudgetTables},
	{IDs: []string{"add_whitelisted_routes_json_column"}, run: migrationAddWhitelistedRoutesJSONColumn},
	{IDs: []string{"replace_enable_litellm_with_compat_columns"}, run: migrationReplaceEnableLiteLLMWithCompatColumns},
	{IDs: []string{"add_model_pricing_unique_index"}, run: migrationAddModelPricingUniqueIndex},
	{IDs: []string{"default_compat_should_convert_params_false"}, run: migrationDefaultCompatShouldConvertParamsFalse},
	{IDs: []string{"add_priority_tier_pricing_columns"}, run: migrationAddPriorityTierPricingColumns},
	{IDs: []string{"add_flex_tier_pricing_columns"}, run: migrationAddFlexTierPricingColumns},
	{IDs: []string{"normalize_otel_trace_type"}, run: migrationNormalizeOtelTraceType},
	{IDs: []string{"migrate_calendar_aligned"}, run: migrateCalendarAlignedToBudgetsAndRateLimitsTable},
	{IDs: []string{"add_team_budgets_to_budgets_table"}, run: migrationAddTeamBudgetsToBudgetsTable},
	{IDs: []string{"add_ocr_pricing_columns"}, run: migrationAddOCRPricingColumns},
	{IDs: []string{"add_allow_per_request_content_storage_override_column"}, run: migrationAddAllowPerRequestContentStorageOverrideColumn},
	{IDs: []string{"add_retain_content_in_object_storage_column"}, run: migrationAddRetainContentInObjectStorageColumn},
	{IDs: []string{"add_allow_per_request_raw_override_column"}, run: migrationAddAllowPerRequestRawOverrideColumn},
	{IDs: []string{"gov_unique_team_names"}, run: migrationUniqueTeamNames},
	{IDs: []string{"drop_allow_direct_keys_column"}, run: migrationDropAllowDirectKeysColumn},
	{IDs: []string{"drop_allow_direct_keys_column_ddl"}, run: migrationDropAllowDirectKeysColumnDDL},
	{IDs: []string{"add_team_calendar_aligned_column"}, run: migrationAddTeamCalendarAlignedColumn},
	{IDs: []string{"drop_legacy_calendar_aligned_columns"}, run: migrationDropLegacyCalendarAlignedColumns},
	{IDs: []string{"add_vk_access_profile_id_column"}, run: migrationAddVKAccessProfileIDColumn},
	{IDs: []string{"drop_vk_access_profile_id_column"}, run: migrationDropVKAccessProfileIDColumn},
	{IDs: []string{"add_feature_flags_table"}, run: migrationAddFeatureFlagsTable},
	{IDs: []string{"add_framework_config_hash_column"}, run: migrationAddFrameworkConfigHashColumn},
	{IDs: []string{"add_model_parameters_url_column"}, run: migrationAddModelParametersURLColumn},
	{IDs: []string{"add_client_config_metadata_json_column"}, run: migrationAddClientConfigMetadataColumn},
	{IDs: []string{"backfill_vk_provider_config_blacklisted_models"}, run: migrationBackfillVirtualKeyBlacklistedModels},
	{IDs: []string{"add_created_by_user_id_column_for_virtual_keys"}, run: migrationAddCreatedByUserIDColumnForVirtualKeys},
	{IDs: []string{"re_add_allow_direct_keys_column"}, run: migrationReAddAllowDirectKeysColumn},
	{IDs: []string{"drop_azure_api_version_column"}, run: migrationDropAzureAPIVersionColumn},
	{IDs: []string{"add_additional_attributes_to_pricing"}, run: migrationAddAdditionalAttributesToPricing},
	{IDs: []string{"add_model_config_scope_columns"}, run: migrationAddModelConfigScopeColumns},
	{IDs: []string{"migrate_provider_governance_to_model_configs"}, run: migrationMigrateProviderGovernanceToModelConfigs},
	{IDs: []string{"add_budget_model_config_id_column"}, run: migrationAddBudgetModelConfigIDColumn},
	{IDs: []string{"add_vertex_aws_workload_identity_column"}, run: migrationAddVertexAWSWorkloadIdentityColumn},
	{IDs: []string{"add_model_config_calendar_aligned_column"}, run: migrationAddModelConfigCalendarAlignedColumn},
	{IDs: []string{"migrate_virtual_key_governance_to_model_configs"}, run: migrationMigrateVirtualKeyGovernanceToModelConfigs},
	{IDs: []string{"add_customer_calendar_aligned_column"}, run: migrationAddCustomerCalendarAlignedColumn},
	{IDs: []string{"add_customer_budgets_to_budgets_table"}, run: migrationAddCustomerBudgetsToBudgetsTable},
	{IDs: []string{"add_model_config_budgets_fk_constraint"}, run: migrationAddModelConfigBudgetsFKConstraint},
	{IDs: []string{"add_fast_mode_pricing_columns"}, run: migrationAddFastModePricingColumns},
	{IDs: []string{"add_customer_name_unique_constraint_dedup", "add_customer_name_unique_constraint_index"}, run: migrationAddCustomerNameUniqueConstraint},
	{IDs: []string{"null_legacy_customer_budget_id_refs"}, run: migrationNullLegacyCustomerBudgetID},
	{IDs: []string{"add_skills_repo_tables"}, run: migrationAddSkillsRepoTables},
	{IDs: []string{"add_dump_errors_in_console_logs_column"}, run: migrationAddDumpErrorsInConsoleLogsColumn},
	{IDs: []string{"add_bedrock_mantle_key_columns"}, run: migrationAddBedrockMantleKeyColumns},
	{IDs: []string{"add_model_pricing_is_deprecated_column"}, run: migrationAddModelPricingIsDeprecatedColumn},
	{IDs: []string{"add_virtual_key_expires_at_column"}, run: migrationAddVirtualKeyExpiresAtColumn},
	{IDs: []string{"add_virtual_key_delete_after_expire_column"}, run: migrationAddVirtualKeyDeleteAfterExpireColumn},
	{IDs: []string{"add_client_config_delete_expired_virtual_keys_column"}, run: migrationAddClientConfigDeleteExpiredVirtualKeysColumn},
	{IDs: []string{"add_fast_mode_cache_pricing_columns"}, run: migrationAddFastModeCachePricingColumns},
	{IDs: []string{"add_inference_geo_multiplier_column"}, run: migrationAddInferenceGeoMultiplierColumn},
	{IDs: []string{"add_flex_and_cache_creation_272k_pricing_columns"}, run: migrationAddFlexAndCacheCreation272kPricingColumns},
	{IDs: []string{"add_vertex_force_single_region_column"}, run: migrationAddVertexForceSingleRegionColumn},
	{IDs: []string{"add_sidekiq_table"}, run: migrationAddSidekiqTable},
	{IDs: []string{"add_sidekiq_kind_status_created_index"}, run: migrationAddSidekiqKindStatusCreatedIndex},
	{IDs: []string{"add_sidekiq_partitioning_key_column"}, run: migrationAddSidekiqPartitioningKeyColumn},
	{IDs: []string{"repair_bare_wildcard_allowed_models"}, run: migrationRepairBareWildcardAllowedModels},
	{IDs: []string{"add_bedrock_project_id_columns"}, run: migrationAddBedrockProjectIDColumns},
	{IDs: []string{"add_dual_credential_conflict_behavior_column"}, run: migrationAddDualCredentialConflictBehaviorColumn},
	{IDs: []string{"add_use_anthropic_endpoints_column"}, run: migrationAddUseAnthropicEndpointsColumn},
	{IDs: []string{"add_bedrock_batch_role_arn_column"}, run: migrationAddBedrockBatchRoleARNColumn},
	{IDs: []string{"add_budget_override_columns"}, run: migrationAddBudgetOverrideColumns},
	{IDs: []string{"add_budget_override_anchor_columns"}, run: migrationAddBudgetOverrideAnchorColumns},
	{IDs: []string{"add_live_models_sync_interval_column"}, run: migrationAddLiveModelsSyncIntervalColumn},
	{IDs: []string{"add_pricing_override_user_id_column"}, run: migrationAddPricingOverrideUserIDColumn},
	{IDs: []string{"add_budget_reset_config_column"}, run: migrationAddBudgetResetConfigColumn},
	{IDs: []string{"add_bedrock_endpoints_columns"}, run: migrationAddBedrockEndpointsColumns},
	{IDs: []string{"add_cost_per_request_pricing_column"}, run: migrationAddCostPerRequestPricingColumn},
	{IDs: []string{"backfill_default_complexity_exemplars_v2"}, run: migrationBackfillDefaultComplexityExemplars},
	{IDs: []string{"add_notifications_table"}, run: migrationAddNotificationsTable},
	{IDs: []string{"add_batch_jobs_table"}, run: migrationAddBatchJobsTable},
	{IDs: []string{"add_image_megapixel_tier_pricing_columns"}, run: migrationAddImageMegapixelTierPricingColumns},
	{IDs: []string{"add_input_cost_per_query_column"}, run: migrationAddInputCostPerQueryColumn},
	{IDs: []string{"add_ultrafast_pricing_columns"}, run: migrationAddUltrafastPricingColumns},
	{IDs: []string{"add_image_size_quality_pricing_columns"}, run: migrationAddImageSizeQualityPricingColumns},
	{IDs: []string{"add_batch_jobs_attribution_columns"}, run: migrationAddBatchJobsAttributionColumns},
	{IDs: []string{"add_vk_rotation_cooldown_columns"}, run: migrationAddVKRotationCooldownColumns},
	{IDs: []string{"add_vk_rotation_cooldown_client_column"}, run: migrationAddVKRotationCooldownClientColumn},
	{IDs: []string{"add_video_resolution_pricing_columns"}, run: migrationAddVideoResolutionPricingColumns},
	{IDs: []string{"add_provider_job_kind_columns", "swap_provider_job_indexes"}, run: migrationAddProviderJobKindColumns},
	{IDs: []string{"add_compat_azure_deepseek_column"}, run: migrationAddCompatAzureDeepseekColumn},
	{IDs: []string{"clear_plugin_config_hashes"}, run: migrationClearPluginConfigHashes},
	{IDs: []string{"add_databricks_key_config_columns"}, run: migrationAddDatabricksKeyConfigColumns},
	{IDs: []string{"add_oauth_key_config_columns"}, run: migrationAddOAuthKeyConfigColumns},
	{IDs: []string{"add_allow_all_providers_to_virtual_key"}, run: migrationAddAllowAllProvidersToVirtualKey},
	{IDs: []string{"backfill_vk_allow_all_providers_hash"}, run: migrationBackfillVirtualKeyAllowAllProvidersHash},
	{IDs: []string{"add_prompt_cache_json_column"}, run: migrationAddPromptCacheJSONColumn},
	{IDs: []string{"add_hidden_request_types_json_column"}, run: migrationAddHiddenRequestTypesJSONColumn},
	{IDs: []string{"add_use_openai_endpoints_column"}, run: migrationAddUseOpenAIEndpointsColumn},
	{IDs: []string{"add_time_of_day_pricing_columns"}, run: migrationAddTimeOfDayPricingColumns},
	{IDs: []string{"migrate_vk_standalone_limits_to_model_configs"}, run: migrationMigrateVKStandaloneLimitsToModelConfigs},
	{IDs: []string{"add_virtual_key_business_unit_column"}, run: migrationAddVirtualKeyBusinessUnitColumn},
	{IDs: []string{"add_virtual_key_disable_content_logging_column"}, run: migrationAddVirtualKeyDisableContentLoggingColumn},
	{IDs: []string{"add_ttft_timeout_ms_column_to_routing_targets"}, run: migrationAddTTFTTimeoutMsColumnToRoutingTargets},
	{IDs: []string{"add_web_search_cost_per_request_column"}, run: migrationAddWebSearchCostPerRequestColumn},
	{IDs: []string{"move_pricing_override_search_context_to_web_search"}, run: migrationMovePricingOverrideSearchContextToWebSearch},
	{IDs: []string{"add_vk_provider_config_virtual_key_id_index"}, run: migrationAddVKProviderConfigVirtualKeyIDIndex},
	{IDs: []string{"add_value_hash_and_token_hash_indexes"}, run: migrationAddValueHashAndTokenHashIndexes},
	{IDs: []string{"add_vk_provider_config_provider_and_key_indexes"}, run: migrationAddVKProviderConfigProviderAndKeyIndexes},
	{IDs: []string{"add_virtual_keys_created_at_id_index"}, run: migrationAddVirtualKeysCreatedAtIDIndex},
	{IDs: []string{"add_batch_jobs_due_index"}, run: migrationAddBatchJobsDueIndex},
	{IDs: []string{"add_ultrafast_above_272k_pricing_columns"}, run: migrationAddUltrafastAbove272kPricingColumns},
	{IDs: []string{"add_priority_above_272k_cache_creation_pricing_column"}, run: migrationAddPriorityAbove272kCacheCreationPricingColumn},
	{IDs: []string{"add_compat_force_reasoning_only_models_to_responses_column"}, run: migrationAddCompatForceReasoningOnlyModelsToResponsesColumn},
	{IDs: []string{"backfill_compat_force_reasoning_only_models_to_responses"}, run: migrationBackfillCompatForceReasoningOnlyModelsToResponses},
	{IDs: []string{"add_ignore_provider_cost_column"}, run: migrationAddIgnoreProviderCostColumn},
	{IDs: []string{"add_100k_token_pricing_columns"}, run: migrationAdd100kTokenPricingColumns},
	{IDs: []string{"add_decisions_pricing_columns"}, run: migrationAddDecisionsPricingColumns},
	{IDs: []string{"add_compat_convert_decision_to_responses_column"}, run: migrationAddCompatConvertDecisionToResponsesColumn},
	{IDs: []string{"add_cloudflare_key_config_columns"}, run: migrationAddCloudflareKeyConfigColumns},
	{IDs: []string{"move_config_json_vk_limits_to_model_configs"}, run: migrationMoveConfigJSONVKLimitsToModelConfigs},
	{IDs: []string{"add_provider_objects_table"}, run: migrationAddProviderObjectsTable},
	{IDs: []string{"add_batch_jobs_file_id_indexes"}, run: migrationAddBatchJobsFileIDIndexes},
	{IDs: []string{"add_provider_objects_object_id_index"}, run: migrationAddProviderObjectsObjectIDIndex},
	{IDs: []string{"add_client_config_enforce_vk_object_ownership_column"}, run: migrationAddClientConfigEnforceVKObjectOwnershipColumn},
	{IDs: []string{"add_client_config_extra_fields_placement_column"}, run: migrationAddClientConfigExtraFieldsPlacementColumn},
	{IDs: []string{"add_skills_external_client_url_column"}, run: migrationAddSkillsExternalClientURLColumn},
	{IDs: []string{"add_virtual_key_metadata_column"}, run: migrationAddVirtualKeyMetadataColumn},
	{IDs: []string{"add_vertex_use_session_routing_column"}, run: migrationAddVertexUseSessionRoutingColumn},
	{IDs: []string{"add_client_config_skip_access_profile_user_virtual_key_lifecycle_columns"}, run: migrationAddClientConfigSkipAccessProfileUserVirtualKeyLifecycleColumns},
	{IDs: []string{"add_passthrough_json_column"}, run: migrationAddPassthroughJSONColumn},
	{IDs: []string{"add_azure_use_speech_endpoints_column"}, run: migrationAddAzureUseSpeechEndpointsColumn},
	{IDs: []string{"add_azure_use_voice_live_endpoints_column"}, run: migrationAddAzureUseVoiceLiveEndpointsColumn},
	{IDs: []string{"add_sagemaker_key_columns"}, run: migrationAddSageMakerKeyColumns},
	{IDs: []string{"add_video_token_pricing_column"}, run: migrationAddVideoTokenPricingColumn},
	{IDs: []string{"add_voice_live_pricing_columns"}, run: migrationAddVoiceLivePricingColumns},
	{IDs: []string{"add_anthropic_workload_identity_column"}, run: migrationAddAnthropicWorkloadIdentityColumn},
	{IDs: []string{"add_cache_creation_above_12hr_pricing_columns"}, run: migrationAddCacheCreationAbove12hrPricingColumns},
	{IDs: []string{"add_datasheet_overrides_table"}, run: migrationAddDatasheetOverridesTable},
	{IDs: []string{"add_usage_estimation_tokenizers_json_column"}, run: migrationAddUsageEstimationTokenizersJSONColumn},
	{IDs: []string{"add_budget_business_unit_id_column"}, run: migrationAddBudgetBusinessUnitIDColumn},
	{IDs: []string{"add_budget_business_unit_id_index"}, run: migrationAddBudgetBusinessUnitIDIndex},
	{IDs: []string{"add_databricks_scopes_column"}, run: migrationAddDatabricksScopesColumn},
	{IDs: []string{"add_model_routing_suggestion_schedules_table"}, run: migrationAddComplexitySuggestionSchedulesTable},
	{IDs: []string{"add_model_routers_table"}, run: migrationAddModelRoutersTable},
	{IDs: []string{"add_model_router_targets_and_routing"}, run: migrationAddModelRouterTargetsAndRouting},
	{IDs: []string{"add_model_router_suggestions_tier_columns"}, run: migrationAddModelRouterSuggestionTierColumns},
	{IDs: []string{"add_client_config_entity_access_profile_mode_column"}, run: migrationAddClientConfigEntityAccessProfileModeColumn},
}

// videoResolutionPricingColumns are the resolution-banded video output rate columns.
// Providers publish a different per-second rate per output resolution (sora-2-pro is
// $0.30/s at 720p but $0.70/s at 1080p; Veo 3.1 is $0.40/s at 720p/1080p and $0.60/s
// at 4K), which the single output_cost_per_video_per_second column cannot express.
var videoResolutionPricingColumns = []string{
	"output_cost_per_video_per_second_480p",
	"output_cost_per_video_per_second_720p",
	"output_cost_per_video_per_second_1024p",
	"output_cost_per_video_per_second_1080p",
	"output_cost_per_video_per_second_4k",
}

func migrationAddVideoResolutionPricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_video_resolution_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range videoResolutionPricingColumns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(*gorm.DB) error {
			return fmt.Errorf("add_video_resolution_pricing_columns is non-rollbackable: dropping the resolution-banded video rate columns would permanently delete every custom per-resolution price an operator has set, and the affected models would silently bill at their unbanded rate instead; the columns are additive and older binaries safely ignore them")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// migrationAddVideoTokenPricingColumn adds output_cost_per_video_token, the
// per-token video output rate for providers that meter video in tokens
// (BytePlus Seedance bills on the task's completion_tokens).
func migrationAddVideoTokenPricingColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_video_token_pricing_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := setMigrationLockTimeout(tx); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, "output_cost_per_video_token"); err != nil {
				return fmt.Errorf("failed to add column output_cost_per_video_token: %w", err)
			}
			return nil
		},
		Rollback: func(*gorm.DB) error {
			return fmt.Errorf("add_video_token_pricing_column is non-rollbackable: dropping output_cost_per_video_token would permanently delete every custom per-token video price an operator has set, and token-metered video would silently bill at $0; the column is additive and older binaries safely ignore it")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// migrationAddCompatAzureDeepseekColumn adds the compat_azure_deepseek column to
// config_client.
func migrationAddCompatAzureDeepseekColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_compat_azure_deepseek_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "CompatAzureDeepseek"); err != nil {
				return fmt.Errorf("failed to add compat_azure_deepseek column: %w", err)
			}

			// The conversion was unconditional before this toggle existed, so existing
			// deployments are backfilled to TRUE to keep the behaviour they have today.
			// The column default stays FALSE, matching the other compat flags.
			if err := tx.Exec("UPDATE config_client SET compat_azure_deepseek = TRUE").Error; err != nil {
				return fmt.Errorf("failed to backfill compat_azure_deepseek: %w", err)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "compat_azure_deepseek"); err != nil {
				return fmt.Errorf("failed to drop compat_azure_deepseek column: %w", err)
			}

			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running compat_azure_deepseek migration: %s", err.Error())
	}
	return nil
}

// migrationAddAllowAllProvidersToVirtualKey adds the allow_all_providers column to the virtual
// key table. Default false preserves the existing deny-by-default behaviour, so the column needs
// no data backfill: existing VKs keep allowing only the providers in their ProviderConfigs. The
// config_hash is refreshed separately by migrationBackfillVirtualKeyAllowAllProvidersHash, since
// allow_all_providers now feeds GenerateVirtualKeyHash.
func migrationAddAllowAllProvidersToVirtualKey(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_allow_all_providers_to_virtual_key"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableVirtualKey{}, "allow_all_providers"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableVirtualKey{}, "allow_all_providers"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running add allow_all_providers to virtual key migration: %s", err.Error())
	}
	return nil
}

// migrationBackfillVirtualKeyAllowAllProvidersHash recomputes config_hash for every virtual key
// after allow_all_providers joined GenerateVirtualKeyHash. Existing rows carry a hash computed
// without that field, so without this backfill config synchronization would see false drift on the
// first boot after upgrade. It is a separate migration from the column-add so the DDL's lock is
// never held across this SELECT + UPDATE backfill. The hash is deterministic, so this is safe to
// re-run.
func migrationBackfillVirtualKeyAllowAllProvidersHash(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "backfill_vk_allow_all_providers_hash"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			var virtualKeys []tables.TableVirtualKey
			if err := tx.
				Preload("ProviderConfigs").
				Preload("ProviderConfigs.Keys").
				Find(&virtualKeys).Error; err != nil {
				return fmt.Errorf("failed to fetch virtual keys for hash recomputation: %w", err)
			}
			logger.Info("[configstore] %s: processing %d virtualKeys", migrationName, len(virtualKeys))
			for _, vk := range virtualKeys {
				newHash, err := GenerateVirtualKeyHash(vk)
				if err != nil {
					return fmt.Errorf("failed to generate hash for VK %s: %w", vk.ID, err)
				}
				if err := tx.Model(&tables.TableVirtualKey{}).
					Where("id = ?", vk.ID).
					Update("config_hash", newHash).Error; err != nil {
					return fmt.Errorf("failed to update config_hash for VK %s: %w", vk.ID, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running backfill_vk_allow_all_providers_hash migration: %s", err.Error())
	}
	return nil
}

// migrationAddBatchJobsAttributionColumns adds the requester-identity columns to
// batch_jobs. Before these, a settled batch could only be attributed to its virtual
// key — which an access profile shares across users — and the sweeper, having no
// request context at all, wrote its cost row with no user, team, or customer.
func migrationAddBatchJobsAttributionColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_batch_jobs_attribution_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	columns := []string{"user_id", "team_id", "customer_id", "source_log_id"}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mig := tx.Migrator()
			for _, column := range columns {
				if mig.HasColumn(&tables.TableProviderJob{}, column) {
					continue
				}
				if err := mig.AddColumn(&tables.TableProviderJob{}, column); err != nil {
					return fmt.Errorf("failed to add %s column to batch_jobs: %w", column, err)
				}
			}
			// Backs per-user cost lookups over in-flight and settled batches.
			return tx.Exec(`CREATE INDEX IF NOT EXISTS idx_batch_jobs_user_id ON batch_jobs (user_id)`).Error
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mig := tx.Migrator()
			if err := tx.Exec("DROP INDEX IF EXISTS idx_batch_jobs_user_id").Error; err != nil {
				return fmt.Errorf("failed to drop index idx_batch_jobs_user_id: %w", err)
			}
			for _, column := range columns {
				if !mig.HasColumn(&tables.TableProviderJob{}, column) {
					continue
				}
				if err := mig.DropColumn(&tables.TableProviderJob{}, column); err != nil {
					return fmt.Errorf("failed to drop %s column from batch_jobs: %w", column, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// migrationAddProviderObjectsTable creates the provider_objects table that records
// which virtual key created a provider-side object (file, video, container, cached
// content, batch), so later requests for that object can be bound to its creator.
func migrationAddProviderObjectsTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_provider_objects_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	return RunSingleMigration(ctx, nil, db, logger, providerObjectsTableMigration(ctx, migrationName))
}

// providerObjectsTableMigration is the migration migrationAddProviderObjectsTable runs.
func providerObjectsTableMigration(ctx context.Context, migrationName string) *migrator.Migration {
	return &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			return tx.WithContext(ctx).AutoMigrate(&tables.TableProviderObject{})
		},
		Rollback: func(*gorm.DB) error {
			return fmt.Errorf("%s is non-rollbackable: dropping provider_objects would forget the creating virtual key of every recorded file, container and cached content, and those objects would become reachable to every virtual key allowed on the provider; the table is additive and older binaries ignore it", migrationName)
		},
	}
}

// migrationAddBatchJobsFileIDIndexes indexes batch_jobs.input_file_id, output_file_id and
// error_file_id. The file-ownership fallback (GetProviderJobsByFileIDs) filters on them for every
// virtual-key file request whose file has no ownership row; without the indexes each one scans
// the provider's whole batch history. Built concurrently so the upgrade never blocks writes.
func migrationAddBatchJobsFileIDIndexes(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_batch_jobs_file_id_indexes"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	noTxOpts := *migrator.DefaultOptions
	noTxOpts.UseTransaction = false
	if err := RunSingleMigration(ctx, &noTxOpts, db, logger, batchJobsFileIDIndexesMigration(ctx, migrationName)); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// batchJobsFileIDIndexesMigration builds the migration applied by
// migrationAddBatchJobsFileIDIndexes, so tests exercise its callbacks.
func batchJobsFileIDIndexesMigration(ctx context.Context, migrationName string) *migrator.Migration {
	indexes := map[string]string{
		"idx_batch_jobs_input_file_id":  "input_file_id",
		"idx_batch_jobs_output_file_id": "output_file_id",
		"idx_batch_jobs_error_file_id":  "error_file_id",
	}
	return &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for name, column := range indexes {
				if err := ensureIndexConcurrently(tx, "batch_jobs", name, column, false); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for name := range indexes {
				if err := dropIndexConcurrently(tx, "batch_jobs", name); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// migrationAddProviderObjectsObjectIDIndex indexes provider_objects by (kind, object_id), the
// lookup a live-session ownership check makes on every control route: by the session id under
// any provider name. The table's other index leads with (kind, provider), which this lookup
// cannot use past kind, and session rows stay after sessions end.
func migrationAddProviderObjectsObjectIDIndex(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_provider_objects_object_id_index"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	noTxOpts := *migrator.DefaultOptions
	noTxOpts.UseTransaction = false
	if err := RunSingleMigration(ctx, &noTxOpts, db, logger, providerObjectsObjectIDIndexMigration(ctx, migrationName)); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// providerObjectsObjectIDIndexMigration builds the migration applied by
// migrationAddProviderObjectsObjectIDIndex, so tests exercise its callbacks.
func providerObjectsObjectIDIndexMigration(ctx context.Context, migrationName string) *migrator.Migration {
	const name = "idx_provider_objects_kind_object_id"
	return &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			return ensureIndexConcurrently(tx.WithContext(ctx), "provider_objects", name, "kind, object_id", false)
		},
		Rollback: func(tx *gorm.DB) error {
			return dropIndexConcurrently(tx.WithContext(ctx), "provider_objects", name)
		},
	}
}

func migrationAddNotificationsTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_notifications_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	return RunSingleMigration(ctx, nil, db, logger, &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			return tx.WithContext(ctx).AutoMigrate(&tables.TableNotification{})
		},
		Rollback: func(tx *gorm.DB) error {
			return tx.WithContext(ctx).Migrator().DropTable(&tables.TableNotification{})
		},
	})
}

// migrationAddDatasheetOverridesTable creates governance_datasheet_overrides,
// the user edit layer the datasheet sync re-applies after every refresh.
func migrationAddDatasheetOverridesTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_datasheet_overrides_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	return RunSingleMigration(ctx, nil, db, logger, &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			mgr := tx.WithContext(ctx).Migrator()
			if mgr.HasTable(&tables.TableDatasheetOverride{}) {
				return nil
			}
			return mgr.CreateTable(&tables.TableDatasheetOverride{})
		},
		Rollback: func(*gorm.DB) error {
			return fmt.Errorf("%s is non-rollbackable: dropping governance_datasheet_overrides would permanently delete user datasheet edits; the table is additive and older binaries ignore it", migrationName)
		},
	})
}

// migrationAddAnthropicWorkloadIdentityColumn adds the anthropic_workload_identity_json column to
// config_keys, holding an Anthropic key's encrypted anthropic_key_config.workload_identity block.
func migrationAddAnthropicWorkloadIdentityColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_anthropic_workload_identity_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := setMigrationLockTimeout(tx); err != nil {
				return err
			}
			return addColumnIfNotExists(tx, logger, &tables.TableKey{}, "anthropic_workload_identity_json")
		},
		Rollback: rollbackAnthropicWorkloadIdentityColumn,
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// rollbackAnthropicWorkloadIdentityColumn refuses, like the Vertex workload identity column: the
// column may hold a literal identity token and the federation settings of every federated key.
func rollbackAnthropicWorkloadIdentityColumn(*gorm.DB) error {
	return fmt.Errorf("add_anthropic_workload_identity_column is non-rollbackable: dropping anthropic_workload_identity_json would permanently delete the workload identity configuration of every Anthropic key that uses it; the column is additive and older binaries safely ignore it")
}

// quoteSQLiteIdentifier quotes a SQLite identifier, escaping any double quotes.
func quoteSQLiteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sqliteTableColumns returns the column names of a SQLite table.
func sqliteTableColumns(tx *gorm.DB, tableName string) ([]string, error) {
	var columns []sqliteColumnInfo
	query := fmt.Sprintf("PRAGMA table_info(%s)", quoteSQLiteIdentifier(tableName))
	if err := tx.Raw(query).Scan(&columns).Error; err != nil {
		return nil, err
	}

	result := make([]string, 0, len(columns))
	for _, column := range columns {
		result = append(result, column.Name)
	}
	return result, nil
}

// sqliteTableHasColumn checks if a SQLite table has a column with the given name.
func sqliteTableHasColumn(tx *gorm.DB, tableName, columnName string) (bool, error) {
	columns, err := sqliteTableColumns(tx, tableName)
	if err != nil {
		return false, err
	}
	if slices.Contains(columns, columnName) {
		return true, nil
	}
	return false, nil
}

// hasColumn checks if a table has a column with the given name.
// Returns the introspection error rather than collapsing it to false so callers
// can distinguish "column missing" from "could not determine".
func hasColumn(tx *gorm.DB, table, column string) (bool, error) {
	var count int64
	var q string
	switch tx.Dialector.Name() {
	case "sqlite":
		q = `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`
	default:
		q = `SELECT COUNT(*) FROM information_schema.columns WHERE table_name = ? AND column_name = ? AND table_schema = current_schema()`
	}
	if err := tx.Raw(q, table, column).Scan(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// sqliteDropLegacyBudgetColumn removes the legacy budget_id column from a
// SQLite table by dumping data, recreating the table from the current GORM
// model, and copying data back.
//
// Strategy: dump-data → drop-original → create-clean → restore-data.
// We never RENAME the original table because SQLite propagates ALTER TABLE
// RENAME into FK references in OTHER tables, corrupting them.
func sqliteDropLegacyBudgetColumn(tx *gorm.DB, tableName string) error {
	model, err := currentBudgetOwnerModel(tableName)
	if err != nil {
		return err
	}

	columns, err := sqliteTableColumns(tx, tableName)
	if err != nil {
		return fmt.Errorf("failed to inspect SQLite columns for %s: %w", tableName, err)
	}

	preservedColumns := make([]string, 0, len(columns))
	for _, column := range columns {
		if column != "budget_id" {
			preservedColumns = append(preservedColumns, column)
		}
	}
	if len(preservedColumns) == len(columns) {
		return nil // budget_id column not present, nothing to do
	}

	// Build the column list for data transfer.
	quotedColumns := make([]string, 0, len(preservedColumns))
	for _, column := range preservedColumns {
		quotedColumns = append(quotedColumns, quoteSQLiteIdentifier(column))
	}
	columnList := strings.Join(quotedColumns, ", ")

	// Dump existing data into a temporary table (data-only, no constraints).
	dumpTable := tableName + "__dump"
	if err := tx.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s", quoteSQLiteIdentifier(dumpTable))).Error; err != nil {
		return fmt.Errorf("failed to drop stale dump table %s: %w", dumpTable, err)
	}
	dumpSQL := fmt.Sprintf("CREATE TABLE %s AS SELECT %s FROM %s",
		quoteSQLiteIdentifier(dumpTable), columnList, quoteSQLiteIdentifier(tableName))
	if err := tx.Exec(dumpSQL).Error; err != nil {
		return fmt.Errorf("failed to dump %s data: %w", tableName, err)
	}

	// Drop the original table. Safe because PRAGMA foreign_keys is OFF.
	// This also removes all indexes and FK definitions cleanly.
	if err := tx.Exec(fmt.Sprintf("DROP TABLE %s", quoteSQLiteIdentifier(tableName))).Error; err != nil {
		return fmt.Errorf("failed to drop original SQLite table %s: %w", tableName, err)
	}

	// Recreate the table from the current GORM model (no budget_id column,
	// proper indexes and constraints). The original table name is now free.
	if err := tx.Migrator().CreateTable(model); err != nil {
		return fmt.Errorf("failed to recreate SQLite table %s: %w", tableName, err)
	}

	// Restore data from the dump.
	restoreSQL := fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s",
		quoteSQLiteIdentifier(tableName), columnList, columnList, quoteSQLiteIdentifier(dumpTable))
	if err := tx.Exec(restoreSQL).Error; err != nil {
		return fmt.Errorf("failed to restore data into %s: %w", tableName, err)
	}

	// Clean up the dump table.
	if err := tx.Exec(fmt.Sprintf("DROP TABLE %s", quoteSQLiteIdentifier(dumpTable))).Error; err != nil {
		return fmt.Errorf("failed to drop dump table %s: %w", dumpTable, err)
	}
	return nil
}

// dropColumnSQL returns a dialect-correct `ALTER TABLE ... DROP COLUMN` statement.
// Postgres supports (and we use) the `IF EXISTS` guard so the drop is a no-op when the
// column is already gone; SQLite's ALTER TABLE grammar has no `IF EXISTS` clause and
// errors with `near "EXISTS": syntax error`, so we emit a plain DROP COLUMN there.
// Callers must still guard the SQLite path with a HasColumn check, since plain DROP
// COLUMN errors on a missing column.
func dropColumnSQL(tx *gorm.DB, table, column string) string {
	if tx.Dialector.Name() == "postgres" {
		return fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s", table, column)
	}
	return fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", table, column)
}

func dropLegacyBudgetColumn(tx *gorm.DB, tableName string) error {
	mg := tx.Migrator()
	if !mg.HasColumn(tableName, "budget_id") {
		return nil
	}

	if tx.Dialector.Name() == "sqlite" {
		if err := sqliteDropLegacyBudgetColumn(tx, tableName); err != nil {
			return err
		}
	} else {
		model, err := legacyBudgetColumnModel(tableName)
		if err != nil {
			return err
		}
		if err := dropColumnIfExists(tx, nil, model, "budget_id"); err != nil {
			return fmt.Errorf("failed to drop legacy %s.budget_id column: %w", tableName, err)
		}
	}

	var stillExists bool
	var err error
	if tx.Dialector.Name() == "sqlite" {
		stillExists, err = sqliteTableHasColumn(tx, tableName, "budget_id")
		if err != nil {
			return fmt.Errorf("failed to verify legacy %s.budget_id column drop: %w", tableName, err)
		}
	} else {
		stillExists = mg.HasColumn(tableName, "budget_id")
	}
	if stillExists {
		return fmt.Errorf("legacy %s.budget_id column still exists after migration", tableName)
	}
	return nil
}

// areThereAnyPendingMigrations returns true if there are any pending migrations to be applied.
func areThereAnyPendingMigrations(ctx context.Context, db *gorm.DB, logger schemas.Logger) bool {
	pending, err := pendingMigrationStepIDs(ctx, db, configstoreMigrationSteps)
	if err != nil {
		logger.Warn("[configstore] migration preflight failed; acquiring migration lock and running migrations: %v", err)
	}
	// Fail open: on a preflight error we proceed to acquire the lock and run
	// migrations rather than silently skipping them (matches logstore and the
	// warn log above). Only a clean "nothing pending" result skips the run.
	return err != nil || len(pending) > 0
}

// triggerMigrations performs the necessary database migrations, pausing
// materialized-view refreshes with the package default options.
func triggerMigrations(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	return triggerMigrationsWithPause(ctx, db, logger, migrationMatViewPause)
}

// triggerMigrationsWithPause is triggerMigrations with explicit matview pause
// options, so a configured migration_matview_cancel_after (including "off")
// governs config store migrations as well as log store ones.
func triggerMigrationsWithPause(ctx context.Context, db *gorm.DB, logger schemas.Logger, pause logstore.MatViewPauseOptions) error {
	return triggerMigrationsWithOptions(ctx, db, logger, pause, migrationStepLockRetry)
}

// triggerMigrationsWithOptions is triggerMigrationsWithPause with the lock-timeout
// retry options too, so a configured migration_lock_retry_budget governs how
// long a blocked step keeps retrying. A test override of migrationStepLockRetry
// keeps precedence over the configured budget.
func triggerMigrationsWithOptions(ctx context.Context, db *gorm.DB, logger schemas.Logger, pause logstore.MatViewPauseOptions, retry postgresconn.LockRetryOptions) error {
	if !areThereAnyPendingMigrations(ctx, db, logger) {
		logger.Info("[configstore] no pending migrations; skipping migration run")
		return nil
	}
	// Acquire advisory lock to serialize migrations across cluster nodes.
	// This prevents race conditions when multiple nodes start simultaneously
	// and try to create the same tables in parallel.
	lock, err := acquireMigrationLock(ctx, db, logger)
	if err != nil {
		return err
	}
	defer lock.release(ctx)
	// Checking again if there are any pending migrations after acquiring the lock.
	if !areThereAnyPendingMigrations(ctx, db, logger) {
		logger.Info("[configstore] no pending migrations after lock acquisition; skipping migration run")
		return nil
	}
	if db.Dialector.Name() == "postgres" {
		pending, err := pendingMigrationStepIDs(ctx, db, configstoreMigrationSteps)
		if err == nil && len(pending) == 0 {
			logger.Info("[configstore] migrations completed by another node; skipping migration run")
			return nil
		}
		if err != nil {
			logger.Warn("[configstore] migration preflight after lock failed; running migrations: %v", err)
		}
	}
	// Pause materialized-view refreshes for the run. The config store often
	// shares its database with the log store, and CREATE INDEX CONCURRENTLY waits
	// on every older transaction in the database, so a long refresh of the logs
	// views from any replica would otherwise stall these index builds. Taken after
	// the migration lock and released before it (defers run LIFO).
	releasePause := pauseMatViewsForMigrations(ctx, db, logger, pause)
	defer releasePause()
	return runMigrationStepsWithRetry(ctx, db, logger, configstoreMigrationSteps, migrationStepLockRetry.WithBudgetFallback(retry))
}

// migrationAddClientConfigMetadataColumn adds the metadata_json column to
// config_client. The column stores a JSON blob of UI/admin preferences (e.g.
// onboarding_dismissed) and is deliberately not part of the ClientConfig API
// struct, so config.json sync cannot overwrite it.
func migrationAddClientConfigMetadataColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_client_config_metadata_json_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "metadata_json"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "metadata_json"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddFeatureFlagsTable creates the feature_flags table holding
// user-toggled overrides for the in-memory featureflags registry.
func migrationAddFeatureFlagsTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_feature_flags_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			if !migrator.HasTable(&tables.TableFeatureFlag{}) {
				logger.Info("[configstore] %s: creating table TableFeatureFlag", migrationName)
				if err := migrator.CreateTable(&tables.TableFeatureFlag{}); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

func migrationAddStoreRawRequestResponseColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_store_raw_request_response_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableProvider{}, "store_raw_request_response"); err != nil {
				return err
			}
			// Backfill config_hash for existing providers so they don't appear
			// dirty after upgrade. StoreRawRequestResponse is now part of the
			// hash input; rows written before this migration have stale hashes.
			var providers []tables.TableProvider
			if err := tx.
				Select(
					"id",
					"name",
					"network_config_json",
					"concurrency_buffer_json",
					"proxy_config_json",
					"custom_provider_config_json",
					"send_back_raw_request",
					"send_back_raw_response",
					"store_raw_request_response",
					"encryption_status",
				).
				Find(&providers).Error; err != nil {
				return fmt.Errorf("failed to fetch providers for hash backfill: %w", err)
			}
			logger.Info("[configstore] %s: processing %d providers", migrationName, len(providers))
			for _, provider := range providers {
				providerConfig := ProviderConfig{
					NetworkConfig:            provider.NetworkConfig,
					ConcurrencyAndBufferSize: provider.ConcurrencyAndBufferSize,
					ProxyConfig:              provider.ProxyConfig,
					SendBackRawRequest:       provider.SendBackRawRequest,
					SendBackRawResponse:      provider.SendBackRawResponse,
					StoreRawRequestResponse:  provider.StoreRawRequestResponse,
					CustomProviderConfig:     provider.CustomProviderConfig,
				}
				// Here the default value of store_raw_request_response should be based on the default value of SendBackRawRequest and SendBackRawResponse
				if provider.SendBackRawRequest || provider.SendBackRawResponse {
					providerConfig.StoreRawRequestResponse = true
				}
				hash, err := providerConfig.GenerateConfigHash(provider.Name)
				if err != nil {
					return fmt.Errorf("failed to generate hash for provider %s: %w", provider.Name, err)
				}
				if err := tx.Model(&provider).Updates(map[string]interface{}{
					"config_hash":                hash,
					"store_raw_request_response": providerConfig.StoreRawRequestResponse,
				}).Error; err != nil {
					return fmt.Errorf("failed to update hash for provider %s: %w", provider.Name, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableProvider{}, "store_raw_request_response"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running add store raw request response column migration: %s", err.Error())
	}
	return nil
}

// migrationInit is the first migration
func migrationInit(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "init"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			if !migrator.HasTable(&tables.TableConfigHash{}) {
				logger.Info("[configstore] %s: creating table TableConfigHash", migrationName)
				if err := migrator.CreateTable(&tables.TableConfigHash{}); err != nil {
					return err
				}
			}
			// TableBudget and TableRateLimit must be created before TableProvider
			// because TableProvider has FK references to them
			if !migrator.HasTable(&tables.TableBudget{}) {
				logger.Info("[configstore] %s: creating table TableBudget", migrationName)
				if err := migrator.CreateTable(&tables.TableBudget{}); err != nil {
					return err
				}
			}
			if !migrator.HasTable(&tables.TableRateLimit{}) {
				logger.Info("[configstore] %s: creating table TableRateLimit", migrationName)
				if err := migrator.CreateTable(&tables.TableRateLimit{}); err != nil {
					return err
				}
			}
			if !migrator.HasTable(&tables.TableProvider{}) {
				logger.Info("[configstore] %s: creating table TableProvider", migrationName)
				if err := migrator.CreateTable(&tables.TableProvider{}); err != nil {
					return err
				}
			}
			if !migrator.HasTable(&tables.TableKey{}) {
				logger.Info("[configstore] %s: creating table TableKey", migrationName)
				if err := migrator.CreateTable(&tables.TableKey{}); err != nil {
					return err
				}
			}
			if !migrator.HasTable(&tables.TableModel{}) {
				logger.Info("[configstore] %s: creating table TableModel", migrationName)
				if err := migrator.CreateTable(&tables.TableModel{}); err != nil {
					return err
				}
			}
			if !migrator.HasTable(&tables.TableClientConfig{}) {
				logger.Info("[configstore] %s: creating table TableClientConfig", migrationName)
				if err := migrator.CreateTable(&tables.TableClientConfig{}); err != nil {
					return err
				}
			} else if !migrator.HasColumn(&tables.TableClientConfig{}, "max_request_body_size_mb") {
				if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "max_request_body_size_mb"); err != nil {
					return err
				}
			}
			if !migrator.HasTable(&tables.TableEnvKey{}) {
				logger.Info("[configstore] %s: creating table TableEnvKey", migrationName)
				if err := migrator.CreateTable(&tables.TableEnvKey{}); err != nil {
					return err
				}
			}
			if !migrator.HasTable(&tables.TableVectorStoreConfig{}) {
				logger.Info("[configstore] %s: creating table TableVectorStoreConfig", migrationName)
				if err := migrator.CreateTable(&tables.TableVectorStoreConfig{}); err != nil {
					return err
				}
			}
			if !migrator.HasTable(&tables.TableLogStoreConfig{}) {
				logger.Info("[configstore] %s: creating table TableLogStoreConfig", migrationName)
				if err := migrator.CreateTable(&tables.TableLogStoreConfig{}); err != nil {
					return err
				}
			}
			if !migrator.HasTable(&tables.TableCustomer{}) {
				logger.Info("[configstore] %s: creating table TableCustomer", migrationName)
				if err := migrator.CreateTable(&tables.TableCustomer{}); err != nil {
					return err
				}
			}
			if !migrator.HasTable(&tables.TableTeam{}) {
				logger.Info("[configstore] %s: creating table TableTeam", migrationName)
				if err := migrator.CreateTable(&tables.TableTeam{}); err != nil {
					return err
				}
			}
			if !migrator.HasTable(&tables.TableVirtualKey{}) {
				logger.Info("[configstore] %s: creating table TableVirtualKey", migrationName)
				if err := migrator.CreateTable(&tables.TableVirtualKey{}); err != nil {
					return err
				}
			}
			if !migrator.HasTable(&tables.TableGovernanceConfig{}) {
				logger.Info("[configstore] %s: creating table TableGovernanceConfig", migrationName)
				if err := migrator.CreateTable(&tables.TableGovernanceConfig{}); err != nil {
					return err
				}
			}
			if !migrator.HasTable(&tables.TableModelPricing{}) {
				logger.Info("[configstore] %s: creating table TableModelPricing", migrationName)
				if err := migrator.CreateTable(&tables.TableModelPricing{}); err != nil {
					return err
				}
			}
			if !migrator.HasTable(&tables.TablePricingOverride{}) {
				logger.Info("[configstore] %s: creating table TablePricingOverride", migrationName)
				if err := migrator.CreateTable(&tables.TablePricingOverride{}); err != nil {
					return err
				}
			}
			if !migrator.HasTable(&tables.TablePlugin{}) {
				logger.Info("[configstore] %s: creating table TablePlugin", migrationName)
				if err := migrator.CreateTable(&tables.TablePlugin{}); err != nil {
					return err
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			// Drop children first, then parents (adjust if your actual FKs differ)
			logger.Info("[configstore] %s: dropping table TableVirtualKey", migrationName)
			if err := migrator.DropTable(&tables.TableVirtualKey{}); err != nil {
				return err
			}
			logger.Info("[configstore] %s: dropping table TableKey", migrationName)
			if err := migrator.DropTable(&tables.TableKey{}); err != nil {
				return err
			}
			logger.Info("[configstore] %s: dropping table TableTeam", migrationName)
			if err := migrator.DropTable(&tables.TableTeam{}); err != nil {
				return err
			}
			logger.Info("[configstore] %s: dropping table TableProvider", migrationName)
			if err := migrator.DropTable(&tables.TableProvider{}); err != nil {
				return err
			}
			logger.Info("[configstore] %s: dropping table TableCustomer", migrationName)
			if err := migrator.DropTable(&tables.TableCustomer{}); err != nil {
				return err
			}
			logger.Info("[configstore] %s: dropping table TableBudget", migrationName)
			if err := migrator.DropTable(&tables.TableBudget{}); err != nil {
				return err
			}
			logger.Info("[configstore] %s: dropping table TableRateLimit", migrationName)
			if err := migrator.DropTable(&tables.TableRateLimit{}); err != nil {
				return err
			}
			logger.Info("[configstore] %s: dropping table TableModel", migrationName)
			if err := migrator.DropTable(&tables.TableModel{}); err != nil {
				return err
			}
			logger.Info("[configstore] %s: dropping table TableClientConfig", migrationName)
			if err := migrator.DropTable(&tables.TableClientConfig{}); err != nil {
				return err
			}
			logger.Info("[configstore] %s: dropping table TableEnvKey", migrationName)
			if err := migrator.DropTable(&tables.TableEnvKey{}); err != nil {
				return err
			}
			logger.Info("[configstore] %s: dropping table TableVectorStoreConfig", migrationName)
			if err := migrator.DropTable(&tables.TableVectorStoreConfig{}); err != nil {
				return err
			}
			logger.Info("[configstore] %s: dropping table TableLogStoreConfig", migrationName)
			if err := migrator.DropTable(&tables.TableLogStoreConfig{}); err != nil {
				return err
			}
			logger.Info("[configstore] %s: dropping table TableGovernanceConfig", migrationName)
			if err := migrator.DropTable(&tables.TableGovernanceConfig{}); err != nil {
				return err
			}
			logger.Info("[configstore] %s: dropping table TableModelPricing", migrationName)
			if err := migrator.DropTable(&tables.TableModelPricing{}); err != nil {
				return err
			}
			logger.Info("[configstore] %s: dropping table TablePricingOverride", migrationName)
			if err := migrator.DropTable(&tables.TablePricingOverride{}); err != nil {
				return err
			}
			logger.Info("[configstore] %s: dropping table TablePlugin", migrationName)
			if err := migrator.DropTable(&tables.TablePlugin{}); err != nil {
				return err
			}
			logger.Info("[configstore] %s: dropping table TableConfigHash", migrationName)
			if err := migrator.DropTable(&tables.TableConfigHash{}); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// createMany2ManyJoinTable creates a many-to-many join table for the given tables.
func migrationMany2ManyJoinTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "many2manyjoin"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()

			// create the many-to-many join table for virtual keys and keys
			if !migrator.HasTable("governance_virtual_key_keys") {
				createJoinTableSQL := `
					CREATE TABLE IF NOT EXISTS governance_virtual_key_keys (
						table_virtual_key_id VARCHAR(255) NOT NULL,
						table_key_id INTEGER NOT NULL,
						PRIMARY KEY (table_virtual_key_id, table_key_id),
						FOREIGN KEY (table_virtual_key_id) REFERENCES governance_virtual_keys(id) ON DELETE CASCADE,
						FOREIGN KEY (table_key_id) REFERENCES config_keys(id) ON DELETE CASCADE
					)
				`
				logger.Info("[configstore] adding join table for governance_virtual_key_keys: %s", migrationName)
				if err := tx.Exec(createJoinTableSQL).Error; err != nil {
					return fmt.Errorf("failed to create governance_virtual_key_keys table: %w", err)
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			logger.Info("[configstore] dropping join table for governance_virtual_key_keys: %s", migrationName)
			if err := tx.Exec("DROP TABLE IF EXISTS governance_virtual_key_keys").Error; err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddCustomProviderConfigJSONColumn adds the custom_provider_config_json column to the provider table
func migrationAddCustomProviderConfigJSONColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "addcustomproviderconfigjsoncolumn"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := addColumnIfNotExists(tx, logger, &tables.TableProvider{}, "custom_provider_config_json"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddVirtualKeyProviderConfigTable adds the virtual_key_provider_config table
func migrationAddVirtualKeyProviderConfigTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "addvirtualkeyproviderconfig"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()

			if !migrator.HasTable(&tables.TableVirtualKeyProviderConfig{}) {
				logger.Info("[configstore] %s: creating table TableVirtualKeyProviderConfig", migrationName)
				if err := migrator.CreateTable(&tables.TableVirtualKeyProviderConfig{}); err != nil {
					return err
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()

			logger.Info("[configstore] %s: dropping table TableVirtualKeyProviderConfig", migrationName)
			if err := migrator.DropTable(&tables.TableVirtualKeyProviderConfig{}); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddAllowedOriginsJSONColumn adds the allowed_origins_json column to the client config table
// migrationAddBedrockMantleKeyColumns adds the bedrock_mantle_* SigV4 credential columns to the
// config_keys table for the standalone bedrock_mantle provider.
func migrationAddBedrockMantleKeyColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_bedrock_mantle_key_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	cols := []string{
		"bedrock_mantle_access_key",
		"bedrock_mantle_secret_key",
		"bedrock_mantle_session_token",
		"bedrock_mantle_region",
		"bedrock_mantle_role_arn",
		"bedrock_mantle_external_id",
		"bedrock_mantle_role_session_name",
	}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, col := range cols {
				if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, col); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, col := range cols {
				if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, col); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddBedrockProjectIDColumns adds the bedrock_project_id and bedrock_mantle_project_id
// columns to the config_keys table. These scope Bedrock Mantle inference / model listing to a
// specific Bedrock project via the OpenAI-Project / anthropic-workspace-id header.
func migrationAddBedrockProjectIDColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_bedrock_project_id_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	cols := []string{
		"bedrock_project_id",
		"bedrock_mantle_project_id",
	}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, col := range cols {
				if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, col); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, col := range cols {
				if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, col); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

func migrationAddAllowedOriginsJSONColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_allowed_origins_json_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "allowed_origins_json"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddAllowDirectKeysColumn adds the allow_direct_keys column to the client config table.
// Use raw SQL since the struct field was removed in v1.5 when the feature was retired.
// This column is subsequently dropped by migrationDropAllowDirectKeysColumn.
func migrationAddAllowDirectKeysColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_allow_direct_keys_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			if !migrator.HasColumn(&tables.TableClientConfig{}, "allow_direct_keys") {
				if err := tx.Exec("ALTER TABLE config_client ADD COLUMN allow_direct_keys BOOLEAN DEFAULT FALSE").Error; err != nil {
					return err
				}
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationDropAllowDirectKeysColumn recomputes config_hash after the
// AllowDirectKeys field was removed from GenerateClientConfigHash in v1.5.
// Without this, every existing config_hash would mismatch on first startup
// and trigger a spurious config-reload cycle.
//
// The actual DROP COLUMN is handled by migrationDropAllowDirectKeysColumnDDL
// so that the DDL (AccessExclusiveLock) never shares a transaction with the
// SELECT + UPDATE on the same table — that combination was observed to lock
// config_client indefinitely on contended Postgres instances.
func migrationDropAllowDirectKeysColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "drop_allow_direct_keys_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			var clientConfigs []tables.TableClientConfig
			if err := tx.Find(&clientConfigs).Error; err != nil {
				return fmt.Errorf("failed to fetch client configs for hash recompute: %w", err)
			}
			logger.Info("[configstore] %s: processing %d clientConfigs", migrationName, len(clientConfigs))
			for _, cc := range clientConfigs {
				if cc.ConfigHash == "" {
					continue
				}
				clientConfig := ClientConfig{
					DropExcessRequests:                    cc.DropExcessRequests,
					InitialPoolSize:                       cc.InitialPoolSize,
					PrometheusLabels:                      cc.PrometheusLabels,
					EnableLogging:                         cc.EnableLogging,
					DisableContentLogging:                 cc.DisableContentLogging,
					AllowPerRequestContentStorageOverride: cc.AllowPerRequestContentStorageOverride,
					AllowPerRequestRawOverride:            cc.AllowPerRequestRawOverride,
					DisableDBPingsInHealth:                cc.DisableDBPingsInHealth,
					LogRetentionDays:                      cc.LogRetentionDays,
					EnforceAuthOnInference:                cc.EnforceAuthOnInference,
					AllowedOrigins:                        cc.AllowedOrigins,
					AllowedHeaders:                        cc.AllowedHeaders,
					MaxRequestBodySizeMB:                  cc.MaxRequestBodySizeMB,
					HeaderFilterConfig:                    cc.HeaderFilterConfig,
					AsyncJobResultTTL:                     cc.AsyncJobResultTTL,
					RequiredHeaders:                       cc.RequiredHeaders,
					LoggingHeaders:                        cc.LoggingHeaders,
					WhitelistedRoutes:                     cc.WhitelistedRoutes,
					HideDeletedVirtualKeysInFilters:       cc.HideDeletedVirtualKeysInFilters,
					RoutingChainMaxDepth:                  cc.RoutingChainMaxDepth,
					Compat: CompatConfig{
						ConvertTextToChat:      cc.CompatConvertTextToChat,
						ConvertChatToResponses: cc.CompatConvertChatToResponses,
						ShouldDropParams:       cc.CompatShouldDropParams,
						ShouldConvertParams:    cc.CompatShouldConvertParams,
					},
				}
				newHash, err := clientConfig.GenerateClientConfigHash()
				if err != nil {
					return fmt.Errorf("failed to generate hash for client config %d: %w", cc.ID, err)
				}
				if err := tx.Model(&cc).Update("config_hash", newHash).Error; err != nil {
					return fmt.Errorf("failed to update hash for client config %d: %w", cc.ID, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running drop_allow_direct_keys_column migration: %s", err.Error())
	}
	return nil
}

// migrationDropAllowDirectKeysColumnDDL drops the now-unused allow_direct_keys
// column in its own migration. Splitting the DDL from the hash-recompute DML
// ensures the AccessExclusiveLock from DROP COLUMN is held only for the brief
// catalog update and never contends with reads/writes on the same table.
func migrationDropAllowDirectKeysColumnDDL(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "drop_allow_direct_keys_column_ddl"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "allow_direct_keys"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if !tx.Migrator().HasColumn(&tables.TableClientConfig{}, "allow_direct_keys") {
				if err := tx.Exec("ALTER TABLE config_client ADD COLUMN allow_direct_keys BOOLEAN DEFAULT FALSE").Error; err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running drop_allow_direct_keys_column_ddl migration: %s", err.Error())
	}
	return nil
}

// migrationAddEnableLiteLLMFallbacksColumn adds the enable_litellm_fallbacks column to the client config table
func migrationAddEnableLiteLLMFallbacksColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_enable_litellm_fallbacks_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			// Use raw SQL since the struct field was removed in a later migration.
			// This column is subsequently dropped by migrationReplaceEnableLiteLLMWithCompatColumns.
			if !tx.Migrator().HasColumn(&tables.TableClientConfig{}, "enable_litellm_fallbacks") {
				if err := tx.Exec("ALTER TABLE config_client ADD COLUMN enable_litellm_fallbacks BOOLEAN DEFAULT FALSE").Error; err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := tx.Exec("ALTER TABLE config_client DROP COLUMN IF EXISTS enable_litellm_fallbacks").Error; err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationTeamsTableUpdates adds profile, config, and claims columns to the team table
func migrationTeamsTableUpdates(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_profile_config_claims_columns_to_team_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableTeam{}, "profile"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableTeam{}, "config"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableTeam{}, "claims"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddFrameworkConfigsTable adds the framework_configs table
func migrationAddFrameworkConfigsTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_framework_configs_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			if !migrator.HasTable(&tables.TableFrameworkConfig{}) {
				logger.Info("[configstore] %s: creating table TableFrameworkConfig", migrationName)
				if err := migrator.CreateTable(&tables.TableFrameworkConfig{}); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddTeamSourceIDColumn adds optional source_id to governance_teams, with a unique index
func migrationAddTeamSourceIDColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_team_source_id_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	const idxName = "idx_governance_teams_source_id"
	return RunSingleMigration(ctx, nil, db, logger, &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()
			if err := addColumnIfNotExists(tx, logger, &tables.TableTeam{}, "source_id"); err != nil {
				return fmt.Errorf("add source_id column to governance_teams: %w", err)
			}
			if !mg.HasIndex(&tables.TableTeam{}, idxName) {
				logger.Info("[configstore] %s: creating index SourceID on TableTeam", migrationName)
				if err := mg.CreateIndex(&tables.TableTeam{}, "SourceID"); err != nil {
					return fmt.Errorf("create unique index on governance_teams.source_id: %w", err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()
			if mg.HasIndex(&tables.TableTeam{}, idxName) {
				logger.Info("[configstore] %s: dropping index %s on TableTeam", migrationName, idxName)
				if err := mg.DropIndex(&tables.TableTeam{}, idxName); err != nil {
					return fmt.Errorf("drop unique index on governance_teams.source_id: %w", err)
				}
			}
			if err := dropColumnIfExists(tx, logger, &tables.TableTeam{}, "source_id"); err != nil {
				return fmt.Errorf("drop source_id column from governance_teams: %w", err)
			}
			return nil
		},
	})
}

// migrationAddKeyNameColumn adds the name column to the key table and populates unique names
func migrationAddKeyNameColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_key_name_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			if !migrator.HasColumn(&tables.TableKey{}, "name") {
				// Step 1: Add the column as nullable first
				if err := tx.Exec("ALTER TABLE config_keys ADD COLUMN name VARCHAR(255)").Error; err != nil {
					return fmt.Errorf("failed to add name column: %w", err)
				}

				// Step 2: Populate unique names for all existing keys
				var keys []tables.TableKey
				if err := tx.Find(&keys).Error; err != nil {
					return fmt.Errorf("failed to fetch keys: %w", err)
				}

				logger.Info("[configstore] %s: processing %d keys", migrationName, len(keys))
				for _, key := range keys {
					// Create unique name: provider_name-key-{first8chars_of_key_id}-{key_index}
					keyIDShort := key.KeyID
					if len(keyIDShort) > 8 {
						keyIDShort = keyIDShort[:8]
					}
					keyName := keyIDShort + "-" + strconv.Itoa(int(key.ID))
					uniqueName := fmt.Sprintf("%s-key-%s", key.Provider, keyName)

					// Update the key with the unique name
					if err := tx.Model(&key).Update("name", uniqueName).Error; err != nil {
						return fmt.Errorf("failed to update key %s with name %s: %w", key.KeyID, uniqueName, err)
					}
				}

				// Step 3: Add unique index (SQLite compatible)
				if err := tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_key_name ON config_keys (name)").Error; err != nil {
					return fmt.Errorf("failed to create unique index on name: %w", err)
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			// Drop the unique index first to avoid orphaned index artifacts
			if err := tx.Exec("DROP INDEX IF EXISTS idx_key_name").Error; err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "name"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddProviderConfigBudgetRateLimit adds budget_id and rate_limit_id columns with proper foreign key constraints
func migrationAddProviderConfigBudgetRateLimit(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_provider_config_budget_rate_limit"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()

			// Add budget_id and rate_limit_id columns if they don't exist
			// Note: budget_id is added via raw SQL because the field was later removed from the struct
			// (migrated to governance_budgets.provider_config_id in add_multi_budget_tables)
			if migrator.HasTable(&tables.TableVirtualKeyProviderConfig{}) {
				if err := tx.Exec("ALTER TABLE governance_virtual_key_provider_configs ADD COLUMN IF NOT EXISTS budget_id VARCHAR(255)").Error; err != nil {
					// Ignore error for databases that don't support IF NOT EXISTS (e.g., SQLite)
					// The column may already exist from a previous run
				}

				// Add RateLimitID column if it doesn't exist
				if err := addColumnIfNotExists(tx, logger, &tables.TableVirtualKeyProviderConfig{}, "rate_limit_id"); err != nil {
					return fmt.Errorf("failed to add rate_limit_id column: %w", err)
				}

				// Create foreign key indexes for better performance
				if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_provider_config_budget ON governance_virtual_key_provider_configs (budget_id)").Error; err != nil {
					// Ignore - index may already exist or column may not exist yet
				}

				if !migrator.HasIndex(&tables.TableVirtualKeyProviderConfig{}, "idx_provider_config_rate_limit") {
					if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_provider_config_rate_limit ON governance_virtual_key_provider_configs (rate_limit_id)").Error; err != nil {
						return fmt.Errorf("failed to create rate_limit_id index: %w", err)
					}
				}

				// Create FK constraint for RateLimit (Budget FK is no longer needed - budgets use direct FK on budget table)
				if !migrator.HasConstraint(&tables.TableVirtualKeyProviderConfig{}, "RateLimit") {
					if err := migrator.CreateConstraint(&tables.TableVirtualKeyProviderConfig{}, "RateLimit"); err != nil {
						return fmt.Errorf("failed to create RateLimit FK constraint: %w", err)
					}
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()

			// Drop indexes
			_ = tx.Exec("DROP INDEX IF EXISTS idx_provider_config_budget")
			_ = tx.Exec("DROP INDEX IF EXISTS idx_provider_config_rate_limit")

			// Drop FK constraints
			if migrator.HasConstraint(&tables.TableVirtualKeyProviderConfig{}, "RateLimit") {
				if err := migrator.DropConstraint(&tables.TableVirtualKeyProviderConfig{}, "RateLimit"); err != nil {
					return fmt.Errorf("failed to drop RateLimit FK constraint: %w", err)
				}
			}

			// Drop columns via raw SQL (budget_id no longer on struct)
			_ = tx.Exec("ALTER TABLE governance_virtual_key_provider_configs DROP COLUMN IF EXISTS budget_id")
			if err := dropColumnIfExists(tx, logger, &tables.TableVirtualKeyProviderConfig{}, "rate_limit_id"); err != nil {
				return fmt.Errorf("failed to drop rate_limit_id column: %w", err)
			}

			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running provider config budget/rate limit migration: %s", err.Error())
	}
	return nil
}

// migrationAddPluginPathColumn adds the path column to the plugin table
func migrationAddPluginPathColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "update_plugins_table_for_custom_plugins"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TablePlugin{}, "path"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TablePlugin{}, "is_custom"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TablePlugin{}, "path"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &tables.TablePlugin{}, "is_custom"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running plugin path migration: %s", err.Error())
	}
	return nil
}

// migrationAddSessionsTable adds the sessions table
func migrationAddSessionsTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_sessions_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			if !migrator.HasTable(&tables.SessionsTable{}) {
				logger.Info("[configstore] %s: creating table SessionsTable", migrationName)
				if err := migrator.CreateTable(&tables.SessionsTable{}); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			logger.Info("[configstore] %s: dropping table SessionsTable", migrationName)
			if err := migrator.DropTable(&tables.SessionsTable{}); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddDisableContentLoggingColumn adds the disable_content_logging column to the client config table
func migrationAddDisableContentLoggingColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_disable_content_logging_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "disable_content_logging"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "disable_content_logging"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddVertexProjectNumberColumn adds the vertex_project_number column to the key table
func migrationAddVertexProjectNumberColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_vertex_project_number_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "vertex_project_number"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "vertex_project_number"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running vertex project number migration: %s", err.Error())
	}
	return nil
}

// migrationAddVertexDeploymentsJSONColumn adds the vertex_deployments_json column to the key table.
// This column is later dropped by migrationDropDeploymentColumnsAndAddAliases after data is migrated.
func migrationAddVertexDeploymentsJSONColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_vertex_deployments_json_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if !tx.Migrator().HasColumn(&tables.TableKey{}, "vertex_deployments_json") {
				if err := tx.Exec("ALTER TABLE config_keys ADD COLUMN vertex_deployments_json TEXT").Error; err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if tx.Migrator().HasColumn(&tables.TableKey{}, "vertex_deployments_json") {
				if err := tx.Exec("ALTER TABLE config_keys DROP COLUMN vertex_deployments_json").Error; err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running vertex deployments JSON migration: %s", err.Error())
	}
	return nil
}

func migrationMissingProviderColumnInKeyTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_and_fill_provider_column_in_key_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	options := &migrator.Options{
		TableName:                 migrator.DefaultOptions.TableName,
		IDColumnName:              migrator.DefaultOptions.IDColumnName,
		IDColumnSize:              migrator.DefaultOptions.IDColumnSize,
		UseTransaction:            true,
		ValidateUnknownMigrations: migrator.DefaultOptions.ValidateUnknownMigrations,
	}
	m := migrator.New(db, options, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()

			// Step 1: Add the provider column if it doesn't exist
			if migrator.HasColumn(&tables.TableKey{}, "provider") {
				return nil
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "provider"); err != nil {
				return fmt.Errorf("failed to add provider column: %w", err)
			}

			// Step 2: Find all keys where provider is empty/null but provider_id is set
			var keys []tables.TableKey
			if err := tx.Where("provider IS NULL OR provider = ''").Find(&keys).Error; err != nil {
				return fmt.Errorf("failed to fetch keys with missing provider: %w", err)
			}

			// Step 3: Update each key with the provider name from the provider table
			logger.Info("[configstore] %s: processing %d keys", migrationName, len(keys))
			for _, key := range keys {
				var provider tables.TableProvider
				if err := tx.First(&provider, key.ProviderID).Error; err != nil {
					// Skip keys with invalid provider_id
					if err == gorm.ErrRecordNotFound {
						continue
					}
					return fmt.Errorf("failed to fetch provider %d for key %s: %w", key.ProviderID, key.KeyID, err)
				}

				// Update the key with the provider name
				if err := tx.Model(&key).Update("provider", provider.Name).Error; err != nil {
					return fmt.Errorf("failed to update key %s with provider %s: %w", key.KeyID, provider.Name, err)
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "provider"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running add and fill provider column migration: %s", err.Error())
	}
	return nil
}

// migrationAddLogRetentionDaysColumn adds the log_retention_days column to the client config table
func migrationAddLogRetentionDaysColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_log_retention_days_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "log_retention_days"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "log_retention_days"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddEnabledColumnToKeyTable adds the enabled column to the config_keys table
func migrationAddEnabledColumnToKeyTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_enabled_column_to_key_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "enabled"); err != nil {
				return fmt.Errorf("failed to add enabled column: %w", err)
			}
			// Set default = true for existing rows
			if err := tx.Exec("UPDATE config_keys SET enabled = TRUE WHERE enabled IS NULL").Error; err != nil {
				return fmt.Errorf("failed to backfill enabled column: %w", err)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "enabled"); err != nil {
				return fmt.Errorf("failed to drop enabled column: %w", err)
			}

			return nil
		},
	}})

	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running enabled column migration: %s", err.Error())
	}
	return nil
}

// migrationAddBatchAndCachePricingColumns adds the cache_read_input_token_cost, cache_creation_input_token_cost, input_cost_per_token_batches, and output_cost_per_token_batches columns to the model_pricing table
func migrationAddBatchAndCachePricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "update_model_pricing_table_to_add_cache_and_batch_pricing"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, "cache_read_input_token_cost"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, "cache_creation_input_token_cost"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, "input_cost_per_token_batches"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, "output_cost_per_token_batches"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, "cache_read_input_token_cost"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, "cache_creation_input_token_cost"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, "input_cost_per_token_batches"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, "output_cost_per_token_batches"); err != nil {
				return err
			}
			return nil
		},
	}})
	return m.Migrate()
}

// migrationMoveKeysToProviderConfig migrates keys from virtual key level to provider config level
func migrationMoveKeysToProviderConfig(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "move_keys_to_provider_config"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			gormMigrator := tx.Migrator()

			// Step 1: Create the new join table for provider config -> keys relationship
			// Setup the join table so GORM knows about the custom structure
			if err := tx.SetupJoinTable(&tables.TableVirtualKeyProviderConfig{}, "Keys", &tables.TableVirtualKeyProviderConfigKey{}); err != nil {
				return fmt.Errorf("failed to setup join table for provider config keys: %w", err)
			}

			// Create the join table if it doesn't exist
			if !gormMigrator.HasTable(&tables.TableVirtualKeyProviderConfigKey{}) {
				logger.Info("[configstore] %s: creating table TableVirtualKeyProviderConfigKey", migrationName)
				if err := gormMigrator.CreateTable(&tables.TableVirtualKeyProviderConfigKey{}); err != nil {
					return fmt.Errorf("failed to create join table for provider config keys: %w", err)
				}
			}

			// Step 2: Migrate existing key associations from virtual key to provider config level
			// Check if old join table exists
			hasOldTable := gormMigrator.HasTable("governance_virtual_key_keys")

			if hasOldTable {
				// Get all existing associations from old table using GORM's Table method
				type OldAssociation struct {
					VirtualKeyID string `gorm:"column:table_virtual_key_id"`
					KeyID        uint   `gorm:"column:table_key_id"`
				}
				var oldAssociations []OldAssociation
				if err := tx.Table("governance_virtual_key_keys").Find(&oldAssociations).Error; err == nil {
					// Process each association
					logger.Info("[configstore] %s: processing %d oldAssociations", migrationName, len(oldAssociations))
					for _, assoc := range oldAssociations {
						// Get only the key ID and provider - using a minimal struct to avoid
						// querying columns that may not exist yet (added by later migrations)
						type KeyMinimal struct {
							ID       uint
							Provider string
						}
						var keyData KeyMinimal
						if err := tx.Table("config_keys").Select("id, provider").Where("id = ?", assoc.KeyID).First(&keyData).Error; err != nil {
							// Key might have been deleted, skip
							continue
						}

						// Find existing provider config for this virtual key and provider
						var providerConfig tables.TableVirtualKeyProviderConfig
						result := tx.Where("virtual_key_id = ? AND provider = ?", assoc.VirtualKeyID, keyData.Provider).First(&providerConfig)

						if result.Error != nil {
							if result.Error == gorm.ErrRecordNotFound {
								// Create a new provider config for this provider
								providerConfig = tables.TableVirtualKeyProviderConfig{
									VirtualKeyID:  assoc.VirtualKeyID,
									Provider:      keyData.Provider,
									Weight:        bifrost.Ptr(1.0),
									AllowedModels: []string{},
								}
								if err := tx.Create(&providerConfig).Error; err != nil {
									return fmt.Errorf("failed to create provider config for migration: %w", err)
								}
							} else {
								return fmt.Errorf("failed to query provider config: %w", result.Error)
							}
						}

						// Insert directly into the join table using clause.OnConflict for
						// database-agnostic duplicate handling (works for SQLite and PostgreSQL)
						joinEntry := tables.TableVirtualKeyProviderConfigKey{
							TableVirtualKeyProviderConfigID: providerConfig.ID,
							TableKeyID:                      keyData.ID,
						}
						if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&joinEntry).Error; err != nil {
							return fmt.Errorf("failed to associate key %d with provider config %d: %w", keyData.ID, providerConfig.ID, err)
						}
					}
				}

				// Step 3: Drop the old join table
				logger.Info("[configstore] %s: dropping table governance_virtual_key_keys", migrationName)
				if err := gormMigrator.DropTable("governance_virtual_key_keys"); err != nil {
					return fmt.Errorf("failed to drop old governance_virtual_key_keys table: %w", err)
				}
			}

			// Note: Empty keys in provider config means all keys are allowed at runtime
			// We don't pre-populate keys here - this is handled at runtime

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			gormMigrator := tx.Migrator()

			// Recreate the old join table structure
			type OldJoinTable struct {
				VirtualKeyID string `gorm:"column:table_virtual_key_id;primaryKey"`
				KeyID        uint   `gorm:"column:table_key_id;primaryKey"`
			}
			logger.Info("[configstore] %s: creating table OldJoinTable", migrationName)
			if err := gormMigrator.CreateTable(&OldJoinTable{}); err != nil {
				// Table might already exist, ignore error
				_ = err
			}
			// Rename to correct table name if needed
			if gormMigrator.HasTable(&OldJoinTable{}) && !gormMigrator.HasTable("governance_virtual_key_keys") {
				if err := gormMigrator.RenameTable(&OldJoinTable{}, "governance_virtual_key_keys"); err != nil {
					return fmt.Errorf("failed to rename old join table: %w", err)
				}
			}

			// Note: We cannot fully rollback the data migration as it would require
			// reconstructing which keys belonged to which virtual keys

			// Drop the new join table
			logger.Info("[configstore] %s: dropping table governance_virtual_key_provider_config_keys", migrationName)
			if err := gormMigrator.DropTable("governance_virtual_key_provider_config_keys"); err != nil {
				return fmt.Errorf("failed to drop governance_virtual_key_provider_config_keys table: %w", err)
			}

			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running move keys to provider config migration: %s", err.Error())
	}
	return nil
}

// migrationAddPluginVersionColumn adds the version column to the plugin table
func migrationAddPluginVersionColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_plugin_version_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TablePlugin{}, "version"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TablePlugin{}, "version"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running add plugin version column migration: %s", err.Error())
	}
	return nil
}

func migrationAddSendBackRawRequestColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_send_back_raw_request_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableProvider{}, "send_back_raw_request"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableProvider{}, "send_back_raw_request"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running add send back raw request columns migration: %s", err.Error())
	}
	return nil
}

// migrationAddConfigHashColumn adds the config_hash column to the provider and key tables
func migrationAddConfigHashColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_config_hash_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			// Add config_hash to providers table
			if !migrator.HasColumn(&tables.TableProvider{}, "config_hash") {
				if err := addColumnIfNotExists(tx, logger, &tables.TableProvider{}, "config_hash"); err != nil {
					return err
				}
				// Pre-populate hashes for existing providers
				var providers []tables.TableProvider
				if err := tx.Find(&providers).Error; err != nil {
					return fmt.Errorf("failed to fetch providers for hash migration: %w", err)
				}
				logger.Info("[configstore] %s: processing %d providers", migrationName, len(providers))
				for _, provider := range providers {
					if provider.ConfigHash == "" {
						// Convert to ProviderConfig and generate hash
						providerConfig := ProviderConfig{
							NetworkConfig:            provider.NetworkConfig,
							ConcurrencyAndBufferSize: provider.ConcurrencyAndBufferSize,
							ProxyConfig:              provider.ProxyConfig,
							SendBackRawRequest:       provider.SendBackRawRequest,
							SendBackRawResponse:      provider.SendBackRawResponse,
							CustomProviderConfig:     provider.CustomProviderConfig,
						}
						hash, err := providerConfig.GenerateConfigHash(provider.Name)
						if err != nil {
							return fmt.Errorf("failed to generate hash for provider %s: %w", provider.Name, err)
						}
						if err := tx.Model(&provider).Update("config_hash", hash).Error; err != nil {
							return fmt.Errorf("failed to update hash for provider %s: %w", provider.Name, err)
						}
					}
				}
			}
			// Add config_hash to keys table
			if !migrator.HasColumn(&tables.TableKey{}, "config_hash") {
				if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "config_hash"); err != nil {
					return err
				}
				// Pre-populate hashes for existing keys
				var keys []tables.TableKey
				if err := tx.Find(&keys).Error; err != nil {
					return fmt.Errorf("failed to fetch keys for hash migration: %w", err)
				}
				logger.Info("[configstore] %s: processing %d keys", migrationName, len(keys))
				for _, key := range keys {
					if key.ConfigHash == "" {
						// Convert to schemas.Key and generate hash
						schemaKey := schemas.Key{
							Name:             key.Name,
							Value:            key.Value,
							Models:           key.Models,
							Weight:           getWeight(key.Weight),
							AzureKeyConfig:   key.AzureKeyConfig,
							VertexKeyConfig:  key.VertexKeyConfig,
							BedrockKeyConfig: key.BedrockKeyConfig,
							Aliases:          key.Aliases,
						}
						hash, err := GenerateKeyHash(schemaKey)
						if err != nil {
							return fmt.Errorf("failed to generate hash for key %s: %w", key.Name, err)
						}
						if err := tx.Model(&key).Update("config_hash", hash).Error; err != nil {
							return fmt.Errorf("failed to update hash for key %s: %w", key.Name, err)
						}
					}
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableProvider{}, "config_hash"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "config_hash"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running add config hash column migration: %s", err.Error())
	}
	return nil
}

// migrationAddVirtualKeyConfigHashColumn adds the config_hash column to the virtual keys table
func migrationAddVirtualKeyConfigHashColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_virtual_key_config_hash_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			// Add config_hash to virtual keys table
			if !migrator.HasColumn(&tables.TableVirtualKey{}, "config_hash") {
				if err := addColumnIfNotExists(tx, logger, &tables.TableVirtualKey{}, "config_hash"); err != nil {
					return err
				}
				// Pre-populate hashes for existing virtual keys
				var virtualKeys []tables.TableVirtualKey
				if err := tx.Preload("ProviderConfigs").Preload("ProviderConfigs.Keys").Find(&virtualKeys).Error; err != nil {
					return fmt.Errorf("failed to fetch virtual keys for hash migration: %w", err)
				}
				logger.Info("[configstore] %s: processing %d virtualKeys", migrationName, len(virtualKeys))
				for _, vk := range virtualKeys {
					if vk.ConfigHash == "" {
						hash, err := GenerateVirtualKeyHash(vk)
						if err != nil {
							return fmt.Errorf("failed to generate hash for virtual key %s: %w", vk.ID, err)
						}
						if err := tx.Model(&vk).Update("config_hash", hash).Error; err != nil {
							return fmt.Errorf("failed to update hash for virtual key %s: %w", vk.ID, err)
						}
					}
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableVirtualKey{}, "config_hash"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running add virtual key config hash column migration: %s", err.Error())
	}
	return nil
}

// migrationAddAdditionalConfigHashColumns adds config_hash columns to client config, budget, rate limit,
// customer, team, and plugin tables for reconciliation support
func migrationAddAdditionalConfigHashColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_additional_config_hash_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()

			// Add config_hash to client config table
			if !migrator.HasColumn(&tables.TableClientConfig{}, "config_hash") {
				if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "config_hash"); err != nil {
					return err
				}
				// Pre-populate hashes for existing client configs
				var clientConfigs []tables.TableClientConfig
				if err := tx.Find(&clientConfigs).Error; err != nil {
					return fmt.Errorf("failed to fetch client configs for hash migration: %w", err)
				}
				logger.Info("[configstore] %s: processing %d clientConfigs", migrationName, len(clientConfigs))
				for _, cc := range clientConfigs {
					if cc.ConfigHash == "" {
						clientConfig := ClientConfig{
							DropExcessRequests:      cc.DropExcessRequests,
							InitialPoolSize:         cc.InitialPoolSize,
							PrometheusLabels:        cc.PrometheusLabels,
							EnableLogging:           cc.EnableLogging,
							DisableContentLogging:   cc.DisableContentLogging,
							LogRetentionDays:        cc.LogRetentionDays,
							EnforceGovernanceHeader: cc.EnforceGovernanceHeader,
							AllowedOrigins:          cc.AllowedOrigins,
							MaxRequestBodySizeMB:    cc.MaxRequestBodySizeMB,
						}
						hash, err := clientConfig.GenerateClientConfigHash()
						if err != nil {
							return fmt.Errorf("failed to generate hash for client config %d: %w", cc.ID, err)
						}
						if err := tx.Model(&cc).Update("config_hash", hash).Error; err != nil {
							return fmt.Errorf("failed to update hash for client config %d: %w", cc.ID, err)
						}
					}
				}
			}

			// Add config_hash to budgets table
			if !migrator.HasColumn(&tables.TableBudget{}, "config_hash") {
				if err := addColumnIfNotExists(tx, logger, &tables.TableBudget{}, "config_hash"); err != nil {
					return err
				}
				// Pre-populate hashes for existing budgets
				var budgets []tables.TableBudget
				if err := tx.Find(&budgets).Error; err != nil {
					return fmt.Errorf("failed to fetch budgets for hash migration: %w", err)
				}
				logger.Info("[configstore] %s: processing %d budgets", migrationName, len(budgets))
				for _, budget := range budgets {
					if budget.ConfigHash == "" {
						hash, err := GenerateBudgetHash(budget)
						if err != nil {
							return fmt.Errorf("failed to generate hash for budget %s: %w", budget.ID, err)
						}
						if err := tx.Model(&budget).Update("config_hash", hash).Error; err != nil {
							return fmt.Errorf("failed to update hash for budget %s: %w", budget.ID, err)
						}
					}
				}
			}

			// Add config_hash to rate limits table
			if !migrator.HasColumn(&tables.TableRateLimit{}, "config_hash") {
				if err := addColumnIfNotExists(tx, logger, &tables.TableRateLimit{}, "config_hash"); err != nil {
					return err
				}
				// Pre-populate hashes for existing rate limits
				var rateLimits []tables.TableRateLimit
				if err := tx.Find(&rateLimits).Error; err != nil {
					return fmt.Errorf("failed to fetch rate limits for hash migration: %w", err)
				}
				logger.Info("[configstore] %s: processing %d rateLimits", migrationName, len(rateLimits))
				for _, rl := range rateLimits {
					if rl.ConfigHash == "" {
						hash, err := GenerateRateLimitHash(rl)
						if err != nil {
							return fmt.Errorf("failed to generate hash for rate limit %s: %w", rl.ID, err)
						}
						if err := tx.Model(&rl).Update("config_hash", hash).Error; err != nil {
							return fmt.Errorf("failed to update hash for rate limit %s: %w", rl.ID, err)
						}
					}
				}
			}

			// Add config_hash to customers table
			if !migrator.HasColumn(&tables.TableCustomer{}, "config_hash") {
				if err := addColumnIfNotExists(tx, logger, &tables.TableCustomer{}, "config_hash"); err != nil {
					return err
				}
				// Pre-populate hashes for existing customers
				var customers []tables.TableCustomer
				if err := tx.Find(&customers).Error; err != nil {
					return fmt.Errorf("failed to fetch customers for hash migration: %w", err)
				}
				logger.Info("[configstore] %s: processing %d customers", migrationName, len(customers))
				for _, customer := range customers {
					if customer.ConfigHash == "" {
						hash, err := GenerateCustomerHash(customer)
						if err != nil {
							return fmt.Errorf("failed to generate hash for customer %s: %w", customer.ID, err)
						}
						if err := tx.Model(&customer).Update("config_hash", hash).Error; err != nil {
							return fmt.Errorf("failed to update hash for customer %s: %w", customer.ID, err)
						}
					}
				}
			}

			// Add config_hash to teams table
			if !migrator.HasColumn(&tables.TableTeam{}, "config_hash") {
				if err := addColumnIfNotExists(tx, logger, &tables.TableTeam{}, "config_hash"); err != nil {
					return err
				}
				// Pre-populate hashes for existing teams
				var teams []tables.TableTeam
				if err := tx.Find(&teams).Error; err != nil {
					return fmt.Errorf("failed to fetch teams for hash migration: %w", err)
				}
				logger.Info("[configstore] %s: processing %d teams", migrationName, len(teams))
				for _, team := range teams {
					if team.ConfigHash == "" {
						hash, err := GenerateTeamHash(team)
						if err != nil {
							return fmt.Errorf("failed to generate hash for team %s: %w", team.ID, err)
						}
						if err := tx.Model(&team).Update("config_hash", hash).Error; err != nil {
							return fmt.Errorf("failed to update hash for team %s: %w", team.ID, err)
						}
					}
				}
			}

			// Add config_hash to plugins table
			if !migrator.HasColumn(&tables.TablePlugin{}, "config_hash") {
				if err := addColumnIfNotExists(tx, logger, &tables.TablePlugin{}, "config_hash"); err != nil {
					return err
				}
				// Pre-populate hashes for existing plugins
				var plugins []tables.TablePlugin
				if err := tx.Find(&plugins).Error; err != nil {
					return fmt.Errorf("failed to fetch plugins for hash migration: %w", err)
				}
				logger.Info("[configstore] %s: processing %d plugins", migrationName, len(plugins))
				for _, plugin := range plugins {
					if plugin.ConfigHash == "" {
						hash, err := GeneratePluginHash(plugin)
						if err != nil {
							return fmt.Errorf("failed to generate hash for plugin %s: %w", plugin.Name, err)
						}
						if err := tx.Model(&plugin).Update("config_hash", hash).Error; err != nil {
							return fmt.Errorf("failed to update hash for plugin %s: %w", plugin.Name, err)
						}
					}
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "config_hash"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &tables.TableBudget{}, "config_hash"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &tables.TableRateLimit{}, "config_hash"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &tables.TableCustomer{}, "config_hash"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &tables.TableTeam{}, "config_hash"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &tables.TablePlugin{}, "config_hash"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running add additional config hash columns migration: %s", err.Error())
	}
	return nil
}

// migrationClearPluginConfigHashes clears config_plugins.config_hash on every row, putting
// them all in the "no config.json baseline recorded" state that startup treats as legacy.
//
// The column has never held a usable baseline. migrationAddAdditionalConfigHashColumns only
// populates it on databases predating the column, and it populates it from the stored row
// rather than from config.json - so a row edited through the UI since carries a hash that
// disagrees with the file and reads as "config.json changed". Every other row was written by
// a path that never set the column at all. Clearing is what makes the state honest: startup
// then seeds each row with the hash of its current config.json entry and keeps the stored
// config, so UI/API edits survive and only a later file edit reconciles.
func migrationClearPluginConfigHashes(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "clear_plugin_config_hashes"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	return RunSingleMigration(ctx, nil, db, logger, &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			if !tx.Migrator().HasTable(&tables.TablePlugin{}) {
				return nil
			}
			return tx.WithContext(ctx).
				Session(&gorm.Session{AllowGlobalUpdate: true}).
				Model(&tables.TablePlugin{}).
				Where("config_hash IS NOT NULL AND config_hash <> ?", "").
				UpdateColumn("config_hash", "").Error
		},
		// Nothing to restore: the cleared values were never a valid config.json baseline.
		Rollback: func(tx *gorm.DB) error { return nil },
	})
}

// migrationAdd200kTokenPricingColumns adds pricing columns for 200k token tier models
func migrationAdd200kTokenPricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_200k_token_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			columns := []string{
				"input_cost_per_token_above_200k_tokens",
				"output_cost_per_token_above_200k_tokens",
				"cache_creation_input_token_cost_above_200k_tokens",
				"cache_read_input_token_cost_above_200k_tokens",
			}

			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			columns := []string{
				"input_cost_per_token_above_200k_tokens",
				"output_cost_per_token_above_200k_tokens",
				"cache_creation_input_token_cost_above_200k_tokens",
				"cache_read_input_token_cost_above_200k_tokens",
			}

			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	return m.Migrate()
}

// migrationAddImagePricingColumns adds the image generation pricing columns to the model_pricing table
func migrationAddImagePricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_image_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			columns := []string{
				"input_cost_per_image_token",
				"output_cost_per_image_token",
				"input_cost_per_image",
				"output_cost_per_image",
				"cache_read_input_image_token_cost",
			}

			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			columns := []string{
				"input_cost_per_image_token",
				"output_cost_per_image_token",
				"input_cost_per_image",
				"output_cost_per_image",
				"cache_read_input_image_token_cost",
			}

			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	return m.Migrate()
}

// migrationAddUseForBatchAPIColumnAndS3BucketsConfig adds the use_for_batch_api and bedrock_batch_s3_config_json columns to the config_keys table
// Existing keys are backfilled with use_for_batch_api = TRUE to preserve current behavior
func migrationAddUseForBatchAPIColumnAndS3BucketsConfig(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_use_for_batch_api_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// Add use_for_batch_api column
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "use_for_batch_api"); err != nil {
				return fmt.Errorf("failed to add use_for_batch_api column: %w", err)
			}

			// Add bedrock_batch_s3_config_json column
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "bedrock_batch_s3_config_json"); err != nil {
				return fmt.Errorf("failed to add bedrock_batch_s3_config_json column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "use_for_batch_api"); err != nil {
				return fmt.Errorf("failed to drop use_for_batch_api column: %w", err)
			}

			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "bedrock_batch_s3_config_json"); err != nil {
				return fmt.Errorf("failed to drop bedrock_batch_s3_config_json column: %w", err)
			}

			return nil
		},
	}})

	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running use_for_batch_api migration: %s", err.Error())
	}
	return nil
}

// migrationAddUseAnthropicEndpointsColumn adds the use_anthropic_endpoints column to the config_keys table.
func migrationAddUseAnthropicEndpointsColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_use_anthropic_endpoints_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "use_anthropic_endpoints"); err != nil {
				return fmt.Errorf("failed to add use_anthropic_endpoints column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "use_anthropic_endpoints"); err != nil {
				return fmt.Errorf("failed to drop use_anthropic_endpoints column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_use_anthropic_endpoints_column migration: %s", err.Error())
	}
	return nil
}

// migrationAddBudgetOverrideColumns adds additive override state to governance budgets.
func migrationAddBudgetOverrideColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_budget_override_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableBudget{}, "override_amount"); err != nil {
				return fmt.Errorf("failed to add override_amount column: %w", err)
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableBudget{}, "override_mode"); err != nil {
				return fmt.Errorf("failed to add override_mode column: %w", err)
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableBudget{}, "override_cycles_remaining"); err != nil {
				return fmt.Errorf("failed to add override_cycles_remaining column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return fmt.Errorf("add_budget_override_columns is non-rollbackable: dropping the override columns would permanently destroy saved budget override state; the columns are additive and older binaries safely ignore them")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddBudgetOverrideAnchorColumns adds the immutable grant columns that
// make finite budget-override consumption a derived value rather than mutable
// state, and adopts every already-active finite override into that model.
//
// Without a grant, the remaining-cycle count is independently mutable, so every
// cluster node keeps its own tally while only the leader persists one, and any
// config reload replaying a pre-reset row hands back a cycle the reset path had
// already spent. Deriving the count from (anchor, total, last_reset) removes both
// failure modes because all three inputs are either immutable or monotonic.
func migrationAddBudgetOverrideAnchorColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_budget_override_anchor_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableBudget{}, "override_cycles_total"); err != nil {
				return fmt.Errorf("failed to add override_cycles_total column: %w", err)
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableBudget{}, "override_anchor_reset"); err != nil {
				return fmt.Errorf("failed to add override_anchor_reset column: %w", err)
			}
			// Adopt active finite overrides: re-anchor each at its current window
			// and re-grant its remaining count from there. Without knowing when the
			// grant was originally written this is the only available reading, and
			// it can only be generous by less than one window.
			//
			// Required, not optional: validateOverride rejects a cycles override
			// with no anchor, so an unadopted row would be treated as having no
			// lifecycle at all.
			if err := tx.Exec(`
				UPDATE governance_budgets
				SET override_cycles_total = override_cycles_remaining,
				    override_anchor_reset = last_reset
				WHERE override_mode = 'cycles'
				  AND override_cycles_remaining > 0
				  AND override_anchor_reset IS NULL
			`).Error; err != nil {
				return fmt.Errorf("failed to backfill budget override grants: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return fmt.Errorf("add_budget_override_anchor_columns is non-rollbackable: dropping the grant columns would strip every active finite override of the state its remaining-cycle count is derived from; the columns are additive and older binaries safely ignore them")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddHeaderFilterConfigJSONColumn adds the header_filter_config_json column to the config_client table
func migrationAddHeaderFilterConfigJSONColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_header_filter_config_json_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "header_filter_config_json"); err != nil {
				return fmt.Errorf("failed to add header_filter_config_json column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "header_filter_config_json"); err != nil {
				return fmt.Errorf("failed to drop header_filter_config_json column: %w", err)
			}
			return nil
		},
	}})

	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running header_filter_config_json migration: %s", err.Error())
	}
	return nil
}

// migrationAddAzureClientIDAndClientSecretAndTenantIDColumns adds the azure_client_id, azure_client_secret, and azure_tenant_id columns to the key table
func migrationAddAzureClientIDAndClientSecretAndTenantIDColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_azure_client_id_and_client_secret_and_tenant_id_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "azure_client_id"); err != nil {
				return fmt.Errorf("failed to add azure_client_id column: %w", err)
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "azure_client_secret"); err != nil {
				return fmt.Errorf("failed to add azure_client_secret column: %w", err)
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "azure_tenant_id"); err != nil {
				return fmt.Errorf("failed to add azure_tenant_id column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "azure_client_id"); err != nil {
				return fmt.Errorf("failed to drop azure_client_id column: %w", err)
			}
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "azure_client_secret"); err != nil {
				return fmt.Errorf("failed to drop azure_client_secret column: %w", err)
			}
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "azure_tenant_id"); err != nil {
				return fmt.Errorf("failed to drop azure_tenant_id column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running azure_client_id_and_client_secret_and_tenant_id migration: %s", err.Error())
	}
	return nil
}

// migrationAddDistributedLocksTable adds the distributed_locks table for distributed locking
func migrationAddDistributedLocksTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_distributed_locks_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			// Use raw SQL with IF NOT EXISTS for atomic, race-condition-safe table creation
			createTableSQL := `
				CREATE TABLE IF NOT EXISTS distributed_locks (
					lock_key VARCHAR(255) PRIMARY KEY,
					holder_id VARCHAR(255) NOT NULL,
					expires_at TIMESTAMP NOT NULL,
					created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
				)
			`
			if err := tx.Exec(createTableSQL).Error; err != nil {
				return fmt.Errorf("failed to create distributed_locks table: %w", err)
			}
			// Create index on expires_at for efficient cleanup queries
			createIndexSQL := `CREATE INDEX IF NOT EXISTS idx_distributed_locks_expires_at ON distributed_locks (expires_at)`
			if err := tx.Exec(createIndexSQL).Error; err != nil {
				return fmt.Errorf("failed to create expires_at index: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := tx.Exec("DROP TABLE IF EXISTS distributed_locks").Error; err != nil {
				return fmt.Errorf("failed to drop distributed_locks table: %w", err)
			}
			return nil
		},
	}})

	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running distributed_locks table migration: %s", err.Error())
	}
	return nil
}

// migrationAddModelConfigTable adds the governance_model_configs table
func migrationAddModelConfigTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_model_config_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			if !migrator.HasTable(&tables.TableModelConfig{}) {
				logger.Info("[configstore] %s: creating table TableModelConfig", migrationName)
				if err := migrator.CreateTable(&tables.TableModelConfig{}); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			logger.Info("[configstore] %s: dropping table TableModelConfig", migrationName)
			if err := migrator.DropTable(&tables.TableModelConfig{}); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running add model config table migration: %s", err.Error())
	}
	return nil
}

// migrationAddProviderGovernanceColumns adds budget_id and rate_limit_id columns to config_providers table
func migrationAddProviderGovernanceColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_provider_governance_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			provider := &tables.TableProvider{}

			// Add budget_id column if it doesn't exist
			if err := addColumnIfNotExists(tx, logger, provider, "budget_id"); err != nil {
				return fmt.Errorf("failed to add budget_id column: %w", err)
			}
			// Create index for budget_id (outside HasColumn to handle reruns where column exists but index doesn't)
			if !migrator.HasIndex(provider, "idx_provider_budget") {
				if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_provider_budget ON config_providers (budget_id)").Error; err != nil {
					return fmt.Errorf("failed to create budget_id index: %w", err)
				}
			}

			// Add rate_limit_id column if it doesn't exist
			if err := addColumnIfNotExists(tx, logger, provider, "rate_limit_id"); err != nil {
				return fmt.Errorf("failed to add rate_limit_id column: %w", err)
			}
			// Create index for rate_limit_id (outside HasColumn to handle reruns where column exists but index doesn't)
			if !migrator.HasIndex(provider, "idx_provider_rate_limit") {
				if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_provider_rate_limit ON config_providers (rate_limit_id)").Error; err != nil {
					return fmt.Errorf("failed to create rate_limit_id index: %w", err)
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			provider := &tables.TableProvider{}

			// Drop indexes first
			if migrator.HasIndex(provider, "idx_provider_rate_limit") {
				if err := tx.Exec("DROP INDEX IF EXISTS idx_provider_rate_limit").Error; err != nil {
					return fmt.Errorf("failed to drop rate_limit_id index: %w", err)
				}
			}

			if migrator.HasIndex(provider, "idx_provider_budget") {
				if err := tx.Exec("DROP INDEX IF EXISTS idx_provider_budget").Error; err != nil {
					return fmt.Errorf("failed to drop budget_id index: %w", err)
				}
			}

			// Drop rate_limit_id column if it exists
			if err := dropColumnIfExists(tx, logger, provider, "rate_limit_id"); err != nil {
				return fmt.Errorf("failed to drop rate_limit_id column: %w", err)
			}

			// Drop budget_id column if it exists
			if err := dropColumnIfExists(tx, logger, provider, "budget_id"); err != nil {
				return fmt.Errorf("failed to drop budget_id column: %w", err)
			}

			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running add provider governance columns migration: %s", err.Error())
	}
	return nil
}

// migrationAddModelConfigScopeColumns adds the scope and scope_id columns to
// governance_model_configs and swaps the unique index from (model_name, provider)
// to (scope, scope_id, model_name, provider). Existing rows are backfilled to the
// "global" scope, preserving pre-scope behavior. The new index is created before
// the old one is dropped so uniqueness is never unenforced during the migration.
func migrationAddModelConfigScopeColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_model_config_scope_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			modelConfig := &tables.TableModelConfig{}

			// Add scope column (NOT NULL DEFAULT 'global' backfills existing rows).
			if err := addColumnIfNotExists(tx, logger, modelConfig, "scope"); err != nil {
				return fmt.Errorf("failed to add scope column: %w", err)
			}
			// Add scope_id column (nullable).
			if err := addColumnIfNotExists(tx, logger, modelConfig, "scope_id"); err != nil {
				return fmt.Errorf("failed to add scope_id column: %w", err)
			}
			// Belt-and-suspenders backfill in case the column default did not populate
			// existing rows on this dialect.
			if err := tx.Exec("UPDATE governance_model_configs SET scope = ? WHERE scope IS NULL OR scope = ''", tables.ModelConfigScopeGlobal).Error; err != nil {
				return fmt.Errorf("failed to backfill scope: %w", err)
			}

			// Create the new composite unique index BEFORE dropping the old one. The
			// composite index is strictly more selective, so already-unique rows stay
			// unique under it; this ordering avoids any window where uniqueness is
			// unenforced. CreateIndex reads the struct tags so it is dialect-safe.
			if !migrator.HasIndex(modelConfig, "idx_model_scope_provider") {
				logger.Info("[configstore] %s: creating index idx_model_scope_provider on TableModelConfig", migrationName)
				if err := migrator.CreateIndex(modelConfig, "idx_model_scope_provider"); err != nil {
					return fmt.Errorf("failed to create idx_model_scope_provider: %w", err)
				}
			}
			// Drop the now-superseded (model_name, provider) unique index.
			if migrator.HasIndex(modelConfig, "idx_model_provider") {
				logger.Info("[configstore] %s: dropping index idx_model_provider on TableModelConfig", migrationName)
				if err := migrator.DropIndex(modelConfig, "idx_model_provider"); err != nil {
					return fmt.Errorf("failed to drop idx_model_provider: %w", err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return fmt.Errorf("add_model_config_scope_columns is non-rollbackable: scope-aware rows and the previous uniqueness invariant cannot be restored safely")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running add model config scope columns migration: %s", err.Error())
	}
	return nil
}

// migrationMigrateProviderGovernanceToModelConfigs folds provider-level governance
// (config_providers.budget_id / rate_limit_id) into governance_model_configs as
// (scope='global', provider=<name>, model_name='*') "all models on this provider" rows,
// reusing the same budget/rate-limit rows. It then NULLs the provider FKs so the old
// provider-governance enforcement path goes inert (single source of truth = model_configs).
func migrationMigrateProviderGovernanceToModelConfigs(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "migrate_provider_governance_to_model_configs"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// Guard: only run once the model-config table + scope columns exist.
			if !tx.Migrator().HasTable(&tables.TableModelConfig{}) || !tx.Migrator().HasColumn(&tables.TableModelConfig{}, "scope") {
				return nil
			}

			var providers []tables.TableProvider
			if err := tx.Where("budget_id IS NOT NULL OR rate_limit_id IS NOT NULL").Find(&providers).Error; err != nil {
				return fmt.Errorf("failed to load providers with governance: %w", err)
			}

			now := time.Now()
			logger.Info("[configstore] %s: processing %d providers", migrationName, len(providers))
			for i := range providers {
				p := &providers[i]

				// Load any existing global all-models row for this provider. This keeps the
				// migration idempotent (a re-run skips re-insert) AND lossless: if a wildcard
				// row already exists - hand-created via the UI, or left by a partial prior run -
				// we merge the provider's governance into it instead of skipping, so clearing
				// the provider FKs below can never silently drop budget/rate-limit ownership.
				// Select only the columns that exist at this point in the migration sequence
				// (later columns like calendar_aligned do not yet exist).
				var existing tables.TableModelConfig
				err := tx.
					Where("scope = ? AND scope_id IS NULL AND model_name = ? AND provider = ?",
						tables.ModelConfigScopeGlobal, tables.ModelConfigAllModels, p.Name).
					Select("id", "budget_id", "rate_limit_id").
					First(&existing).Error
				switch {
				case err == gorm.ErrRecordNotFound:
					providerName := p.Name
					// Insert via an explicit column map rather than the live TableModelConfig
					// struct: GORM derives the INSERT column list from today's struct, so a
					// column added by a *later* migration (e.g. calendar_aligned, added by
					// add_model_config_calendar_aligned_column further down the sequence) would
					// otherwise appear in this INSERT before its column exists and fail boot.
					// The map pins this historical migration to the columns present at this point.
					if err := tx.Table((tables.TableModelConfig{}).TableName()).Create(map[string]any{
						"id":            uuid.NewString(),
						"model_name":    tables.ModelConfigAllModels,
						"provider":      providerName,
						"scope":         tables.ModelConfigScopeGlobal,
						"budget_id":     p.BudgetID,
						"rate_limit_id": p.RateLimitID,
						"created_at":    now,
						"updated_at":    now,
					}).Error; err != nil {
						return fmt.Errorf("failed to create wildcard model config for provider %q: %w", p.Name, err)
					}
				case err != nil:
					return fmt.Errorf("failed to check existing wildcard config for provider %q: %w", p.Name, err)
				default:
					// A wildcard row already exists: backfill only the governance slots it lacks,
					// so we never overwrite values already present. This is commutative and safe
					// to repeat (a clean re-run finds nothing to fill and writes nothing).
					updates := map[string]any{}
					if existing.BudgetID == nil && p.BudgetID != nil {
						updates["budget_id"] = p.BudgetID
					}
					if existing.RateLimitID == nil && p.RateLimitID != nil {
						updates["rate_limit_id"] = p.RateLimitID
					}
					if len(updates) > 0 {
						updates["updated_at"] = now
						if err := tx.Table((tables.TableModelConfig{}).TableName()).
							Where("id = ?", existing.ID).Updates(updates).Error; err != nil {
							return fmt.Errorf("failed to merge provider governance into wildcard model config for provider %q: %w", p.Name, err)
						}
					}
				}

				// Detach governance from the provider (FK rows are reused by the model config above).
				if err := tx.Model(&tables.TableProvider{}).Where("name = ?", p.Name).
					Updates(map[string]any{"budget_id": nil, "rate_limit_id": nil}).Error; err != nil {
					return fmt.Errorf("failed to clear governance FKs for provider %q: %w", p.Name, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// Nothing to reverse if the model-config table/scope columns are gone.
			if !tx.Migrator().HasTable(&tables.TableModelConfig{}) || !tx.Migrator().HasColumn(&tables.TableModelConfig{}, "scope") {
				return nil
			}

			// Reverse provider-level wildcards:
			// (scope='global', scope_id IS NULL, model_name='*', provider IS NOT NULL).
			var wildcards []tables.TableModelConfig
			if err := tx.Where(
				"scope = ? AND scope_id IS NULL AND model_name = ? AND provider IS NOT NULL",
				tables.ModelConfigScopeGlobal, tables.ModelConfigAllModels,
			).Find(&wildcards).Error; err != nil {
				return fmt.Errorf("failed to load provider wildcard configs: %w", err)
			}

			logger.Info("[configstore] %s: processing %d wildcards", migrationName, len(wildcards))
			for i := range wildcards {
				mc := &wildcards[i]
				// Re-attach the budget/rate-limit FK rows to the provider row.
				if err := tx.Model(&tables.TableProvider{}).Where("name = ?", *mc.Provider).
					Updates(map[string]any{"budget_id": mc.BudgetID, "rate_limit_id": mc.RateLimitID}).Error; err != nil {
					return fmt.Errorf("failed to restore governance FKs for provider %q: %w", *mc.Provider, err)
				}
				// Drop the wildcard model config; its FK rows now live on the provider again.
				if err := tx.Delete(&tables.TableModelConfig{}, "id = ?", mc.ID).Error; err != nil {
					return fmt.Errorf("failed to delete wildcard config for provider %q: %w", *mc.Provider, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running migrate provider governance to model configs migration: %s", err.Error())
	}
	return nil
}

// migrationAddBudgetModelConfigIDColumn adds governance_budgets.model_config_id and
// backfills it from the legacy single governance_model_configs.budget_id, inverting
// budget ownership so a model config can own multiple budgets via the FK.
func migrationAddBudgetModelConfigIDColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_budget_model_config_id_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mig := tx.Migrator()

			if err := addColumnIfNotExists(tx, logger, &tables.TableBudget{}, "model_config_id"); err != nil {
				return fmt.Errorf("failed to add model_config_id column: %w", err)
			}

			// Backfill from the legacy single budget_id. Idempotent via the IS NULL guard.
			if !mig.HasColumn(&tables.TableModelConfig{}, "budget_id") {
				return nil
			}
			var mcs []tables.TableModelConfig
			if err := tx.Where("budget_id IS NOT NULL").Find(&mcs).Error; err != nil {
				return fmt.Errorf("failed to load model configs with budgets: %w", err)
			}
			logger.Info("[configstore] %s: processing %d mcs", migrationName, len(mcs))
			for i := range mcs {
				mc := &mcs[i]
				if mc.BudgetID == nil {
					continue
				}
				if err := tx.Exec(
					"UPDATE governance_budgets SET model_config_id = ? WHERE id = ? AND model_config_id IS NULL",
					mc.ID, *mc.BudgetID,
				).Error; err != nil {
					return fmt.Errorf("failed to backfill model_config_id for budget %q: %w", *mc.BudgetID, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return fmt.Errorf("add_budget_model_config_id_column is non-rollbackable: dropping model_config_id would permanently lose multi-budget ownership data that cannot be recovered from the legacy single budget_id column")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running add budget model_config_id column migration: %s", err.Error())
	}
	return nil
}

// ensureVKModelConfig returns the ID of the VK-scoped model config for the given
// (vkID, provider) pair, creating it if absent.
func ensureVKModelConfig(tx *gorm.DB, vkID string, provider *string, calendarAligned bool, now time.Time) (string, error) {
	q := tx.Model(&tables.TableModelConfig{}).
		Where("scope = ? AND scope_id = ? AND model_name = ?",
			tables.ModelConfigScopeVirtualKey, vkID, tables.ModelConfigAllModels)
	if provider == nil {
		q = q.Where("provider IS NULL")
	} else {
		q = q.Where("provider = ?", *provider)
	}
	var existing []tables.TableModelConfig
	if err := q.Limit(1).Find(&existing).Error; err != nil {
		return "", fmt.Errorf("failed to look up VK model config: %w", err)
	}
	if len(existing) > 0 {
		return existing[0].ID, nil
	}
	mc := tables.TableModelConfig{
		ID:              uuid.NewString(),
		ModelName:       tables.ModelConfigAllModels,
		Provider:        provider,
		Scope:           tables.ModelConfigScopeVirtualKey,
		ScopeID:         &vkID,
		CalendarAligned: calendarAligned,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := tx.Create(&mc).Error; err != nil {
		return "", fmt.Errorf("failed to create VK model config: %w", err)
	}
	return mc.ID, nil
}

// migrationMigrateVirtualKeyGovernanceToModelConfigs folds VK-level governance into
// model_configs as VK-scoped all-models wildcard rows:
//   - VK top-level budgets/rate-limit -> (scope=virtual_key, scope_id=vk, model_name='*', provider=NULL)
//   - per-provider-config budgets/rate-limit -> (..., provider=<that provider>)
func migrationMigrateVirtualKeyGovernanceToModelConfigs(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "migrate_virtual_key_governance_to_model_configs"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// Required tables/columns must exist (scope columns + the new owner FK).
			if !tx.Migrator().HasTable(&tables.TableModelConfig{}) ||
				!tx.Migrator().HasColumn(&tables.TableModelConfig{}, "scope") ||
				!tx.Migrator().HasColumn(&tables.TableBudget{}, "model_config_id") {
				return nil
			}

			var vks []tables.TableVirtualKey
			if err := tx.Preload("Budgets").Preload("ProviderConfigs").Preload("ProviderConfigs.Budgets").
				Find(&vks).Error; err != nil {
				return fmt.Errorf("failed to load virtual keys: %w", err)
			}

			now := time.Now()
			logger.Info("[configstore] %s: processing %d vks", migrationName, len(vks))
			for i := range vks {
				vk := &vks[i]

				// VK top-level governance -> all-providers wildcard.
				if len(vk.Budgets) > 0 || vk.RateLimitID != nil {
					mcID, err := ensureVKModelConfig(tx, vk.ID, nil, vk.CalendarAligned, now)
					if err != nil {
						return err
					}
					logger.Info("[configstore] %s: processing %d vk.Budgets", migrationName, len(vk.Budgets))
					for _, b := range vk.Budgets {
						if err := tx.Exec(
							"UPDATE governance_budgets SET model_config_id = ?, virtual_key_id = NULL WHERE id = ? AND model_config_id IS NULL",
							mcID, b.ID,
						).Error; err != nil {
							return fmt.Errorf("failed to reparent VK budget %q: %w", b.ID, err)
						}
					}
					if vk.RateLimitID != nil {
						if err := tx.Exec("UPDATE governance_model_configs SET rate_limit_id = ? WHERE id = ?", *vk.RateLimitID, mcID).Error; err != nil {
							return fmt.Errorf("failed to move VK rate limit to model config: %w", err)
						}
						if err := tx.Exec("UPDATE governance_virtual_keys SET rate_limit_id = NULL WHERE id = ?", vk.ID).Error; err != nil {
							return fmt.Errorf("failed to clear VK rate limit FK: %w", err)
						}
					}
				}

				// Per-provider-config governance -> provider-specific wildcard.
				logger.Info("[configstore] %s: processing %d vk.ProviderConfigs", migrationName, len(vk.ProviderConfigs))
				for j := range vk.ProviderConfigs {
					pc := &vk.ProviderConfigs[j]
					if len(pc.Budgets) == 0 && pc.RateLimitID == nil {
						continue
					}
					provider := pc.Provider
					mcID, err := ensureVKModelConfig(tx, vk.ID, &provider, vk.CalendarAligned, now)
					if err != nil {
						return err
					}
					logger.Info("[configstore] %s: processing %d pc.Budgets", migrationName, len(pc.Budgets))
					for _, b := range pc.Budgets {
						if err := tx.Exec(
							"UPDATE governance_budgets SET model_config_id = ?, provider_config_id = NULL WHERE id = ? AND model_config_id IS NULL",
							mcID, b.ID,
						).Error; err != nil {
							return fmt.Errorf("failed to reparent provider-config budget %q: %w", b.ID, err)
						}
					}
					if pc.RateLimitID != nil {
						if err := tx.Exec("UPDATE governance_model_configs SET rate_limit_id = ? WHERE id = ?", *pc.RateLimitID, mcID).Error; err != nil {
							return fmt.Errorf("failed to move provider-config rate limit to model config: %w", err)
						}
						if err := tx.Exec("UPDATE governance_virtual_key_provider_configs SET rate_limit_id = NULL WHERE id = ?", pc.ID).Error; err != nil {
							return fmt.Errorf("failed to clear provider-config rate limit FK: %w", err)
						}
					}
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if !tx.Migrator().HasTable(&tables.TableModelConfig{}) ||
				!tx.Migrator().HasColumn(&tables.TableModelConfig{}, "scope") {
				return nil
			}

			// Only the VK-scoped all-models wildcards this migration creates.
			var mcs []tables.TableModelConfig
			if err := tx.Where("scope = ? AND model_name = ?",
				tables.ModelConfigScopeVirtualKey, tables.ModelConfigAllModels).Find(&mcs).Error; err != nil {
				return fmt.Errorf("failed to load VK wildcard model configs: %w", err)
			}

			logger.Info("[configstore] %s: processing %d mcs", migrationName, len(mcs))
			for i := range mcs {
				mc := &mcs[i]
				if mc.ScopeID == nil {
					continue
				}
				var budgets []tables.TableBudget
				if err := tx.Where("model_config_id = ?", mc.ID).Find(&budgets).Error; err != nil {
					return fmt.Errorf("failed to load budgets for model config %q: %w", mc.ID, err)
				}

				if mc.Provider == nil {
					// VK top-level: restore VK ownership + rate limit.
					logger.Info("[configstore] %s: processing %d budgets", migrationName, len(budgets))
					for _, b := range budgets {
						if err := tx.Exec("UPDATE governance_budgets SET virtual_key_id = ?, model_config_id = NULL WHERE id = ?", *mc.ScopeID, b.ID).Error; err != nil {
							return fmt.Errorf("failed to restore VK budget %q: %w", b.ID, err)
						}
					}
					if mc.RateLimitID != nil {
						if err := tx.Exec("UPDATE governance_virtual_keys SET rate_limit_id = ? WHERE id = ?", *mc.RateLimitID, *mc.ScopeID).Error; err != nil {
							return fmt.Errorf("failed to restore VK rate limit: %w", err)
						}
					}
				} else {
					// Provider-specific: find the matching provider config to restore onto.
					var pcs []tables.TableVirtualKeyProviderConfig
					if err := tx.Where("virtual_key_id = ? AND provider = ?", *mc.ScopeID, *mc.Provider).
						Limit(1).Find(&pcs).Error; err != nil {
						return fmt.Errorf("failed to find provider config for VK %q provider %q: %w", *mc.ScopeID, *mc.Provider, err)
					}
					if len(pcs) > 0 {
						pcID := pcs[0].ID
						logger.Info("[configstore] %s: processing %d budgets", migrationName, len(budgets))
						for _, b := range budgets {
							if err := tx.Exec("UPDATE governance_budgets SET provider_config_id = ?, model_config_id = NULL WHERE id = ?", pcID, b.ID).Error; err != nil {
								return fmt.Errorf("failed to restore provider-config budget %q: %w", b.ID, err)
							}
						}
						if mc.RateLimitID != nil {
							if err := tx.Exec("UPDATE governance_virtual_key_provider_configs SET rate_limit_id = ? WHERE id = ?", *mc.RateLimitID, pcID).Error; err != nil {
								return fmt.Errorf("failed to restore provider-config rate limit: %w", err)
							}
						}
					}
				}

				if err := tx.Delete(&tables.TableModelConfig{}, "id = ?", mc.ID).Error; err != nil {
					return fmt.Errorf("failed to delete VK wildcard model config %q: %w", mc.ID, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running migrate virtual key governance to model configs migration: %s", err.Error())
	}
	return nil
}

// migrationAddModelConfigCalendarAlignedColumn adds governance_model_configs.calendar_aligned
// and backfills VK-scoped wildcards from their owning virtual key. Budgets folded out of a
// calendar-aligned VK then keep snapping resets to calendar boundaries.
func migrationAddModelConfigCalendarAlignedColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_model_config_calendar_aligned_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := addColumnIfNotExists(tx, logger, &tables.TableModelConfig{}, "calendar_aligned"); err != nil {
				return fmt.Errorf("failed to add calendar_aligned column: %w", err)
			}

			// Backfill VK-scoped configs from their owning VK.
			type vkRow struct {
				ID              string
				CalendarAligned bool
			}
			var rows []vkRow
			if err := tx.Table("governance_virtual_keys").Select("id, calendar_aligned").Scan(&rows).Error; err != nil {
				return fmt.Errorf("failed to load virtual keys for calendar_aligned backfill: %w", err)
			}
			logger.Info("[configstore] %s: processing %d rows", migrationName, len(rows))
			for _, r := range rows {
				if !r.CalendarAligned {
					continue // default is already false
				}
				if err := tx.Exec(
					"UPDATE governance_model_configs SET calendar_aligned = ? WHERE scope = ? AND scope_id = ?",
					true, tables.ModelConfigScopeVirtualKey, r.ID,
				).Error; err != nil {
					return fmt.Errorf("failed to backfill calendar_aligned for VK %q: %w", r.ID, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableModelConfig{}, "calendar_aligned"); err != nil {
				return fmt.Errorf("failed to drop calendar_aligned column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running add model config calendar_aligned column migration: %s", err.Error())
	}
	return nil
}

// migrationAddAllowedHeadersJSONColumn adds the allowed_headers_json column to the client config table
func migrationAddAllowedHeadersJSONColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_allowed_headers_json_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "allowed_headers_json"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "allowed_headers_json"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddDisableDBPingsInHealthColumn adds the disable_db_pings_in_health column to the client config table
func migrationAddDisableDBPingsInHealthColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_disable_db_pings_in_health_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "disable_db_pings_in_health"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "disable_db_pings_in_health"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddDumpErrorsInConsoleLogsColumn adds the dump_errors_in_console_logs column to the client config table
func migrationAddDumpErrorsInConsoleLogsColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_dump_errors_in_console_logs_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "dump_errors_in_console_logs"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "dump_errors_in_console_logs"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddRoutingRulesTable adds the routing rules table for intelligent request routing
func migrationAddRoutingRulesTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_routing_rules_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()

			if !migrator.HasTable(&tables.TableRoutingRule{}) {
				logger.Info("[configstore] %s: creating table TableRoutingRule", migrationName)
				if err := migrator.CreateTable(&tables.TableRoutingRule{}); err != nil {
					return err
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()

			logger.Info("[configstore] %s: dropping table TableRoutingRule", migrationName)
			if err := migrator.DropTable(&tables.TableRoutingRule{}); err != nil {
				return err
			}

			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running routing_rules_table migration: %s", err.Error())
	}
	return nil
}

// migrationAddBaseModelPricingColumn adds the base_model column to the model_pricing table
func migrationAddBaseModelPricingColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_base_model_pricing_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, "base_model"); err != nil {
				return fmt.Errorf("failed to add column base_model: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, "base_model"); err != nil {
				return fmt.Errorf("failed to drop column base_model: %w", err)
			}
			return nil
		},
	}})
	return m.Migrate()
}

// migrationAddAzureScopesColumn adds the azure_scopes column to the key table for Entra ID OAuth scopes
func migrationAddAzureScopesColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_azure_scopes_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "azure_scopes"); err != nil {
				return fmt.Errorf("failed to add azure_scopes column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "azure_scopes"); err != nil {
				return fmt.Errorf("failed to drop azure_scopes column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running azure_scopes migration: %s", err.Error())
	}
	return nil
}

// migrationAddReplicateDeploymentsJSONColumn adds the replicate_deployments_json column to the key table.
// This column is later dropped by migrationDropDeploymentColumnsAndAddAliases after data is migrated.
func migrationAddReplicateDeploymentsJSONColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_replicate_deployments_json_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if !tx.Migrator().HasColumn(&tables.TableKey{}, "replicate_deployments_json") {
				if err := tx.Exec("ALTER TABLE config_keys ADD COLUMN replicate_deployments_json TEXT").Error; err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if tx.Migrator().HasColumn(&tables.TableKey{}, "replicate_deployments_json") {
				if err := tx.Exec("ALTER TABLE config_keys DROP COLUMN replicate_deployments_json").Error; err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running replicate deployments JSON migration: %s", err.Error())
	}
	return nil
}

// migrationDropDeploymentColumnsAndAddAliases adds the unified aliases_json column, migrates
// existing per-provider deployment data into it, then drops the legacy columns.
// Only one deployment column will be populated per row (they were mutually exclusive).
func migrationDropDeploymentColumnsAndAddAliases(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "drop_deployment_columns_and_add_aliases"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			m := tx.Migrator()

			// Add aliases_json column first
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "aliases_json"); err != nil {
				return err
			}

			// Copy data from whichever legacy deployment column is populated into aliases_json.
			// Only rows where aliases_json is not already set are touched.
			// Exactly one deployment column will be non-null per row (they were mutually exclusive).
			for _, col := range []string{
				"azure_deployments_json",
				"vertex_deployments_json",
				"bedrock_deployments_json",
				"replicate_deployments_json",
			} {
				if !m.HasColumn(&tables.TableKey{}, col) {
					continue
				}
				if err := tx.Exec(
					"UPDATE config_keys SET aliases_json = " + col +
						" WHERE aliases_json IS NULL AND " + col + " IS NOT NULL AND " + col + " != ''",
				).Error; err != nil {
					return err
				}
			}

			// Drop legacy deployment columns
			for _, col := range []string{
				"azure_deployments_json",
				"vertex_deployments_json",
				"bedrock_deployments_json",
				"replicate_deployments_json",
			} {
				if m.HasColumn(&tables.TableKey{}, col) {
					if err := tx.Exec("ALTER TABLE config_keys DROP COLUMN " + col).Error; err != nil {
						return err
					}
				}
			}

			// Fail fast if there are encrypted rows that need fixup but encryption isn't initialized.
			// The migration drops legacy deployment columns below, so skipping this fixup
			// would silently lose the ability to recover those values.
			// This case will ideally never happen
			var encryptedAliasCount int64
			if err := tx.Table("config_keys").
				Where(
					"encryption_status = ? AND aliases_json IS NOT NULL AND aliases_json != '' AND aliases_json != '{}'",
					tables.EncryptionStatusEncrypted,
				).
				Count(&encryptedAliasCount).Error; err != nil {
				return fmt.Errorf("failed to count encrypted aliases for fixup: %w", err)
			}
			if encryptedAliasCount > 0 && !encrypt.IsEnabled() {
				return fmt.Errorf("encryption must be enabled before migrating encrypted aliases")
			}

			// Encrypt aliases_json for rows where encryption_status is already 'encrypted'.
			// The raw SQL copy above preserved the original column's encryption state:
			// - bedrock_deployments_json was encrypted -> aliases_json is already encrypted
			// - azure/vertex/replicate were never encrypted -> aliases_json is plaintext
			// AfterFind will try to decrypt aliases_json for encrypted rows, so we must
			// encrypt any plaintext values first.
			if encrypt.IsEnabled() {
				type aliasRow struct {
					ID          uint
					AliasesJSON *string
				}
				var plainRows []aliasRow
				if err := tx.Raw(
					"SELECT id, aliases_json FROM config_keys WHERE encryption_status = ? AND aliases_json IS NOT NULL AND aliases_json != '' AND aliases_json != '{}'",
					tables.EncryptionStatusEncrypted,
				).Scan(&plainRows).Error; err != nil {
					return fmt.Errorf("failed to fetch aliases for encryption fixup: %w", err)
				}
				logger.Info("[configstore] %s: processing %d plainRows", migrationName, len(plainRows))
				for _, row := range plainRows {
					if row.AliasesJSON == nil || *row.AliasesJSON == "" {
						continue
					}
					// If Decrypt succeeds, the value is already encrypted — skip it (bedrock case).
					// If Decrypt fails, the value is plaintext — encrypt it.
					if _, err := encrypt.Decrypt(*row.AliasesJSON); err != nil {
						if !json.Valid([]byte(*row.AliasesJSON)) {
							return fmt.Errorf("failed to decrypt aliases for key %d: %w", row.ID, err)
						}
						encrypted, encErr := encrypt.Encrypt(*row.AliasesJSON)
						if encErr != nil {
							return fmt.Errorf("failed to encrypt aliases for key %d: %w", row.ID, encErr)
						}
						if err := tx.Exec(
							"UPDATE config_keys SET aliases_json = ? WHERE id = ?",
							encrypted, row.ID,
						).Error; err != nil {
							return fmt.Errorf("failed to update encrypted aliases for key %d: %w", row.ID, err)
						}
					}
				}
			}

			// Recompute config_hash for keys that had aliases_json populated above,
			// since aliases_json is part of the hash input and these rows now have stale hashes.
			var affectedKeys []tables.TableKey
			if err := tx.Where(
				"aliases_json IS NOT NULL AND aliases_json != ? AND aliases_json != ?", "", "{}",
			).Find(&affectedKeys).Error; err != nil {
				return fmt.Errorf("failed to fetch keys for hash recomputation: %w", err)
			}
			logger.Info("[configstore] %s: processing %d affectedKeys", migrationName, len(affectedKeys))
			for _, key := range affectedKeys {
				schemaKey := schemas.Key{
					Name:               key.Name,
					Value:              key.Value,
					Models:             key.Models,
					BlacklistedModels:  key.BlacklistedModels,
					Weight:             getWeight(key.Weight),
					AzureKeyConfig:     key.AzureKeyConfig,
					VertexKeyConfig:    key.VertexKeyConfig,
					BedrockKeyConfig:   key.BedrockKeyConfig,
					Aliases:            key.Aliases,
					VLLMKeyConfig:      key.VLLMKeyConfig,
					ReplicateKeyConfig: key.ReplicateKeyConfig,
					Enabled:            key.Enabled,
					UseForBatchAPI:     key.UseForBatchAPI,
				}
				hash, err := GenerateKeyHash(schemaKey)
				if err != nil {
					return fmt.Errorf("failed to generate hash for key %s: %w", key.Name, err)
				}
				if err := tx.Model(&key).Update("config_hash", hash).Error; err != nil {
					return fmt.Errorf("failed to update config_hash for key %s: %w", key.Name, err)
				}
				logger.Info("[Migration] Recomputed config_hash for key '%s' after aliases migration", key.Name)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "aliases_json"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running drop deployment columns and add aliases migration: %s", err.Error())
	}
	return nil
}

// migrationAddKeyStatusColumns adds status and description columns to config_keys table
// These columns track the status and description of each individual key
func migrationAddKeyStatusColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_key_status_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// Add status column
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "status"); err != nil {
				return err
			}

			// Add description column
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "description"); err != nil {
				return err
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// Drop description column
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "description"); err != nil {
				return err
			}

			// Drop status column
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "status"); err != nil {
				return err
			}

			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running key model discovery status migration: %s", err.Error())
	}
	return nil
}

// migrationAddProviderStatusColumns adds status and description columns to config_providers table
// These columns track the status of model discovery attempts for keyless providers
func migrationAddProviderStatusColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_provider_status_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// Add status column
			if err := addColumnIfNotExists(tx, logger, &tables.TableProvider{}, "status"); err != nil {
				return err
			}

			// Add description column
			if err := addColumnIfNotExists(tx, logger, &tables.TableProvider{}, "description"); err != nil {
				return err
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// Drop description column
			if err := dropColumnIfExists(tx, logger, &tables.TableProvider{}, "description"); err != nil {
				return err
			}

			// Drop status column
			if err := dropColumnIfExists(tx, logger, &tables.TableProvider{}, "status"); err != nil {
				return err
			}

			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running provider model discovery status migration: %s", err.Error())
	}
	return nil
}

// migrationAddAsyncJobResultTTLColumn adds async_job_result_ttl column to config_client table
func migrationAddAsyncJobResultTTLColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_async_job_result_ttl_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "AsyncJobResultTTL"); err != nil {
				return fmt.Errorf("failed to add async_job_result_ttl column: %w", err)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "async_job_result_ttl"); err != nil {
				return fmt.Errorf("failed to drop async_job_result_ttl column: %w", err)
			}

			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running async_job_result_ttl migration: %s", err.Error())
	}
	return nil
}

// migrationAddRateLimitToTeamsAndCustomers adds rate_limit_id column to governance_teams and governance_customers tables
func migrationAddRateLimitToTeamsAndCustomers(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_rate_limit_to_teams_and_customers"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// Add rate_limit_id to governance_teams table
			if err := addColumnIfNotExists(tx, logger, &tables.TableTeam{}, "rate_limit_id"); err != nil {
				return fmt.Errorf("failed to add rate_limit_id column to teams: %w", err)
			}

			// Add rate_limit_id to governance_customers table
			if err := addColumnIfNotExists(tx, logger, &tables.TableCustomer{}, "rate_limit_id"); err != nil {
				return fmt.Errorf("failed to add rate_limit_id column to customers: %w", err)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := dropColumnIfExists(tx, logger, &tables.TableTeam{}, "rate_limit_id"); err != nil {
				return fmt.Errorf("failed to drop rate_limit_id column from teams: %w", err)
			}

			if err := dropColumnIfExists(tx, logger, &tables.TableCustomer{}, "rate_limit_id"); err != nil {
				return fmt.Errorf("failed to drop rate_limit_id column from customers: %w", err)
			}

			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running rate limit migration for teams and customers: %s", err.Error())
	}
	return nil
}

// migrationBackfillEmptyVirtualKeyConfigs backfills existing virtual keys that have
// empty ProviderConfigs with all available providers.
// This preserves the previous "empty means all" behavior for existing VKs after
// the semantic change to "empty means none" (deny-by-default).
func migrationBackfillEmptyVirtualKeyConfigs(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "backfill_empty_virtual_key_configs"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// Step 1: Backfill ProviderConfigs for VKs that have none
			// Find all virtual keys
			var allVKs []tables.TableVirtualKey
			if err := tx.Find(&allVKs).Error; err != nil {
				return fmt.Errorf("failed to query virtual keys: %w", err)
			}

			// Get all available providers
			var allProviders []tables.TableProvider
			if err := tx.Find(&allProviders).Error; err != nil {
				return fmt.Errorf("failed to query providers: %w", err)
			}

			// Track which VK IDs were modified so we can recompute their config_hash
			modifiedVKIDs := make(map[string]struct{})

			logger.Info("[configstore] %s: processing %d allVKs", migrationName, len(allVKs))
			for _, vk := range allVKs {
				// Check if this VK has any provider configs
				var providerConfigCount int64
				if err := tx.Model(&tables.TableVirtualKeyProviderConfig{}).Where("virtual_key_id = ?", vk.ID).Count(&providerConfigCount).Error; err != nil {
					return fmt.Errorf("failed to count provider configs for VK %s: %w", vk.ID, err)
				}

				if providerConfigCount == 0 && len(allProviders) > 0 {
					// VK has no provider configs - backfill with all available providers
					logger.Info("[configstore] %s: processing %d allProviders", migrationName, len(allProviders))
					for _, provider := range allProviders {
						providerConfig := tables.TableVirtualKeyProviderConfig{
							VirtualKeyID:  vk.ID,
							Provider:      provider.Name,
							Weight:        bifrost.Ptr(1.0),
							AllowedModels: []string{},
							AllowAllKeys:  true,
						}
						if err := tx.Create(&providerConfig).Error; err != nil {
							return fmt.Errorf("failed to create provider config for VK %s, provider %s: %w", vk.ID, provider.Name, err)
						}
					}
					modifiedVKIDs[vk.ID] = struct{}{}
					logger.Info("[Migration] Backfilled VK '%s' with %d provider configs", vk.Name, len(allProviders))
				}
			}

			// Step 2: Recompute and persist config_hash for every VK that was modified.
			// Without this, subsequent config-sync diff logic would see a stale hash and
			// attempt to re-reconcile the VK (potentially undoing the backfill).
			logger.Info("[configstore] %s: processing %d modifiedVKIDs", migrationName, len(modifiedVKIDs))
			for vkID := range modifiedVKIDs {
				var vk tables.TableVirtualKey
				if err := tx.
					Preload("ProviderConfigs").
					Preload("ProviderConfigs.Keys").
					First(&vk, "id = ?", vkID).Error; err != nil {
					return fmt.Errorf("failed to reload VK %s for hash recomputation: %w", vkID, err)
				}
				newHash, err := GenerateVirtualKeyHash(vk)
				if err != nil {
					return fmt.Errorf("failed to generate hash for VK %s: %w", vkID, err)
				}
				if err := tx.Model(&tables.TableVirtualKey{}).
					Where("id = ?", vkID).
					Update("config_hash", newHash).Error; err != nil {
					return fmt.Errorf("failed to update config_hash for VK %s: %w", vkID, err)
				}
				logger.Info("[Migration] Recomputed config_hash for VK '%s'", vk.Name)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			// No rollback needed - the backfilled configs are valid data
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running backfill empty virtual key configs migration: %s", err.Error())
	}
	return nil
}

// migrationAddRequiredHeadersJSONColumn adds the required_headers_json column to the config_client table
func migrationAddRequiredHeadersJSONColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_required_headers_json_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "RequiredHeadersJSON"); err != nil {
				return fmt.Errorf("failed to add required_headers_json column: %w", err)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "required_headers_json"); err != nil {
				return fmt.Errorf("failed to drop required_headers_json column: %w", err)
			}

			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running required_headers_json migration: %s", err.Error())
	}
	return nil
}

// migrationAddOutputCostPerVideoPerSecond adds output_cost_per_video_per_second column to governance_model_pricing table
func migrationAddOutputCostPerVideoPerSecond(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_output_cost_per_video_per_second_and_output_cost_per_second_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, "output_cost_per_video_per_second"); err != nil {
				return fmt.Errorf("failed to add output_cost_per_video_per_second column: %w", err)
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, "output_cost_per_second"); err != nil {
				return fmt.Errorf("failed to add output_cost_per_second column: %w", err)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, "output_cost_per_video_per_second"); err != nil {
				return fmt.Errorf("failed to drop output_cost_per_video_per_second column: %w", err)
			}

			if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, "output_cost_per_second"); err != nil {
				return fmt.Errorf("failed to drop output_cost_per_second column: %w", err)
			}

			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running output_cost_per_video_per_second migration: %s", err.Error())
	}
	return nil
}

// migrationAddLoggingHeadersJSONColumn adds the logging_headers_json column to the config_client table
func migrationAddLoggingHeadersJSONColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_logging_headers_json_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "LoggingHeadersJSON"); err != nil {
				return fmt.Errorf("failed to add logging_headers_json column: %w", err)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "logging_headers_json"); err != nil {
				return fmt.Errorf("failed to drop logging_headers_json column: %w", err)
			}

			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running logging_headers_json migration: %s", err.Error())
	}
	return nil
}

// migrationAddHideDeletedVirtualKeysInFiltersColumn adds the hide_deleted_virtual_keys_in_filters column to config_client.
func migrationAddHideDeletedVirtualKeysInFiltersColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_hide_deleted_virtual_keys_in_filters_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "HideDeletedVirtualKeysInFilters"); err != nil {
				return fmt.Errorf("failed to add hide_deleted_virtual_keys_in_filters column: %w", err)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "hide_deleted_virtual_keys_in_filters"); err != nil {
				return fmt.Errorf("failed to drop hide_deleted_virtual_keys_in_filters column: %w", err)
			}

			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running hide_deleted_virtual_keys_in_filters migration: %s", err.Error())
	}
	return nil
}

// migrationAddEnforceSCIMAuthColumn adds the enforce_scim_auth column to the client config table
func migrationAddEnforceSCIMAuthColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_enforce_scim_auth_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "enforce_scim_auth"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "enforce_scim_auth"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running enforce SCIM auth column migration: %s", err.Error())
	}
	return nil
}

// migrationAddEnforceAuthOnInferenceColumn adds the enforce_auth_on_inference column to the config_client table
func migrationAddEnforceAuthOnInferenceColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_enforce_auth_on_inference_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "enforce_auth_on_inference"); err != nil {
				return err
			}
			// Populate from old fields: set to true if either old flag was true
			if err := tx.Exec("UPDATE config_client SET enforce_auth_on_inference = true WHERE enforce_governance_header = true OR enforce_scim_auth = true").Error; err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "enforce_auth_on_inference"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running enforce auth on inference column migration: %s", err.Error())
	}
	return nil
}

// migrationAddDualCredentialConflictBehaviorColumn adds the dual_credential_conflict_behavior
// column to the config_client table. The column is added with its gorm-defined
// NOT NULL default ('prefer_idp'), so existing rows retain the pre-feature behavior.
func migrationAddDualCredentialConflictBehaviorColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_dual_credential_conflict_behavior_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "dual_credential_conflict_behavior"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "dual_credential_conflict_behavior"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running dual credential conflict behavior column migration: %s", err.Error())
	}
	return nil
}

func migrationReconcilePricingOverridesTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "reconcile_pricing_overrides_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mgr := tx.Migrator()

			if !mgr.HasTable(&tables.TablePricingOverride{}) {
				logger.Info("[configstore] %s: creating table TablePricingOverride", migrationName)
				if err := mgr.CreateTable(&tables.TablePricingOverride{}); err != nil {
					return fmt.Errorf("failed to create governance_pricing_overrides table: %w", err)
				}
				return nil
			}
			logger.Info("[configstore] %s: auto-migrating TablePricingOverride", migrationName)
			if err := tx.AutoMigrate(&tables.TablePricingOverride{}); err != nil {
				return fmt.Errorf("failed to automigrate governance_pricing_overrides table: %w", err)
			}
			for _, indexName := range []string{"idx_pricing_override_scope", "idx_pricing_override_match"} {
				if mgr.HasIndex(&tables.TablePricingOverride{}, indexName) {
					continue
				}
				logger.Info("[configstore] %s: creating index %s on TablePricingOverride", migrationName, indexName)
				if err := mgr.CreateIndex(&tables.TablePricingOverride{}, indexName); err != nil {
					return fmt.Errorf("failed to create pricing override index %s: %w", indexName, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mgr := tx.Migrator()
			if mgr.HasTable(&tables.TablePricingOverride{}) {
				logger.Info("[configstore] %s: dropping table TablePricingOverride", migrationName)
				if err := mgr.DropTable(&tables.TablePricingOverride{}); err != nil {
					return fmt.Errorf("failed to drop governance_pricing_overrides table: %w", err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running pricing overrides table reconcile migration: %s", err.Error())
	}
	return nil
}

// migrationAddEncryptionColumns adds the encryption_status column to the config_keys, governance_virtual_keys, sessions, config_providers, config_vector_store, and config_plugins tables
func migrationAddEncryptionColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_encryption_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			type encryptionTable struct {
				table   interface{}
				columns []string
			}

			targets := []encryptionTable{
				{&tables.TableKey{}, []string{"encryption_status"}},
				{&tables.TableVirtualKey{}, []string{"encryption_status", "value_hash"}},
				{&tables.SessionsTable{}, []string{"encryption_status", "token_hash"}},
				{&tables.TableProvider{}, []string{"encryption_status"}},
				{&tables.TableVectorStoreConfig{}, []string{"encryption_status"}},
				{&tables.TablePlugin{}, []string{"encryption_status"}},
			}

			for _, t := range targets {
				for _, col := range t.columns {
					if err := addColumnIfNotExists(tx, logger, t.table, col); err != nil {
						return fmt.Errorf("failed to add column %s: %w", col, err)
					}
				}
			}

			// Backfill encryption_status for all tables that have the column
			backfillTables := []string{
				"config_keys",
				"governance_virtual_keys",
				"sessions",
				"config_providers",
				"config_vector_store",
				"config_plugins",
			}
			logger.Info("[configstore] %s: processing %d backfillTables", migrationName, len(backfillTables))
			for _, table := range backfillTables {
				if err := tx.Exec(fmt.Sprintf(
					"UPDATE %s SET encryption_status = 'plain_text' WHERE encryption_status IS NULL OR encryption_status = ''",
					table,
				)).Error; err != nil {
					return fmt.Errorf("failed to backfill encryption_status in %s: %w", table, err)
				}
			}

			// Backfill value_hash for existing virtual keys
			// Use NULL instead of '' to avoid unique constraint violations
			// (multiple rows with '' would violate the unique index, but NULLs are excluded)
			if err := tx.Exec(`
				UPDATE governance_virtual_keys
				SET value_hash = NULL
				WHERE value_hash IS NULL OR value_hash = ''
			`).Error; err != nil {
				return fmt.Errorf("failed to initialize value_hash: %w", err)
			}

			// Backfill token_hash for existing sessions
			// Use NULL instead of '' to avoid unique constraint violations
			if err := tx.Exec(`
				UPDATE sessions
				SET token_hash = NULL
				WHERE token_hash IS NULL OR token_hash = ''
			`).Error; err != nil {
				return fmt.Errorf("failed to initialize token_hash: %w", err)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			type dropInfo struct {
				table   interface{}
				columns []string
			}

			drops := []dropInfo{
				{&tables.TableKey{}, []string{"encryption_status"}},
				{&tables.TableVirtualKey{}, []string{"encryption_status", "value_hash"}},
				{&tables.SessionsTable{}, []string{"encryption_status", "token_hash"}},
				{&tables.TableProvider{}, []string{"encryption_status"}},
				{&tables.TableVectorStoreConfig{}, []string{"encryption_status"}},
				{&tables.TablePlugin{}, []string{"encryption_status"}},
			}

			for _, d := range drops {
				for _, col := range d.columns {
					if err := dropColumnIfExists(tx, logger, d.table, col); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running encryption columns migration: %s", err.Error())
	}
	return nil
}

// migrationDropEnableGovernanceColumn drops the enable_governance column from the config_client table
func migrationDropEnableGovernanceColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "drop_enable_governance_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "enable_governance"); err != nil {
				return fmt.Errorf("failed to drop enable_governance column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running drop enable governance column rollback: %s", err.Error())
	}
	return nil
}

// migrationAddVLLMKeyConfigColumns adds vllm_url and vllm_model_name columns to the key table
func migrationAddVLLMKeyConfigColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_vllm_key_config_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "vllm_url"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "vllm_model_name"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "vllm_url"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "vllm_model_name"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running vllm key config columns migration: %s", err.Error())
	}
	return nil
}

// migrationWidenEncryptedVarcharColumns widens varchar columns that store AES-256-GCM
// encrypted values to TEXT. Encryption adds ~28 bytes of overhead plus base64 expansion (4/3x),
// so a varchar(255) can only hold ~153-char plaintext. Using TEXT removes any size constraints.
// SQLite does not enforce varchar(n) size constraints, so no migration is needed there.
func migrationWidenEncryptedVarcharColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "widen_encrypted_varchar_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if tx.Dialector.Name() != "postgres" {
				return nil
			}
			// azure_api_version was removed in v1 API migration; only widen it if it
			// still exists (existing DBs that haven't run the drop migration yet).
			if tx.Migrator().HasColumn(&tables.TableKey{}, "azure_api_version") {
				if err := tx.Exec("ALTER TABLE config_keys ALTER COLUMN azure_api_version TYPE TEXT").Error; err != nil {
					return fmt.Errorf("failed to widen column azure_api_version: %w", err)
				}
			}

			stmts := []string{
				// config_keys table - all encrypted SecretVar fields
				"ALTER TABLE config_keys ALTER COLUMN azure_client_id TYPE TEXT",
				"ALTER TABLE config_keys ALTER COLUMN azure_tenant_id TYPE TEXT",
				"ALTER TABLE config_keys ALTER COLUMN vertex_project_id TYPE TEXT",
				"ALTER TABLE config_keys ALTER COLUMN vertex_project_number TYPE TEXT",
				"ALTER TABLE config_keys ALTER COLUMN vertex_region TYPE TEXT",
				"ALTER TABLE config_keys ALTER COLUMN bedrock_access_key TYPE TEXT",
				"ALTER TABLE config_keys ALTER COLUMN bedrock_region TYPE TEXT",
				// sessions table
				"ALTER TABLE sessions ALTER COLUMN token TYPE TEXT",
				// governance_virtual_keys table
				"ALTER TABLE governance_virtual_keys ALTER COLUMN value TYPE TEXT",
			}
			logger.Info("[configstore] %s: processing %d stmts", migrationName, len(stmts))
			for _, stmt := range stmts {
				if err := tx.Exec(stmt).Error; err != nil {
					return fmt.Errorf("failed to widen column (%s): %w", stmt, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running widen encrypted varchar columns migration: %s", err.Error())
	}
	return nil
}

// migrationAddBedrockAssumeRoleColumns adds bedrock_role_arn, bedrock_external_id, and bedrock_role_session_name
// columns to the config_keys table for STS AssumeRole support in Bedrock keys.
func migrationAddBedrockAssumeRoleColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_bedrock_assume_role_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "bedrock_role_arn"); err != nil {
				return fmt.Errorf("failed to add bedrock_role_arn column: %w", err)
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "bedrock_external_id"); err != nil {
				return fmt.Errorf("failed to add bedrock_external_id column: %w", err)
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "bedrock_role_session_name"); err != nil {
				return fmt.Errorf("failed to add bedrock_role_session_name column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "bedrock_role_arn"); err != nil {
				return fmt.Errorf("failed to drop bedrock_role_arn column: %w", err)
			}
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "bedrock_external_id"); err != nil {
				return fmt.Errorf("failed to drop bedrock_external_id column: %w", err)
			}
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "bedrock_role_session_name"); err != nil {
				return fmt.Errorf("failed to drop bedrock_role_session_name column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running bedrock assume role columns migration: %s", err.Error())
	}
	return nil
}

// migrationAddAllowAllKeysToProviderConfig adds the allow_all_keys column to the provider config table
// and backfills existing rows: any provider config with no keys in the join table previously meant
// "allow all keys" (old semantic), so they get allow_all_keys = true to preserve behaviour.
func migrationAddAllowAllKeysToProviderConfig(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_allow_all_keys_to_provider_config"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	// opts is a value copy: migrator.DefaultOptions is a shared global pointer,
	// so mutating it in place would disable transactions for other migrations.
	opts := *migrator.DefaultOptions
	opts.UseTransaction = false
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// Add the column if it doesn't exist
			if err := addColumnIfNotExists(tx, logger, &tables.TableVirtualKeyProviderConfig{}, "allow_all_keys"); err != nil {
				return fmt.Errorf("failed to add allow_all_keys column: %w", err)
			}

			// Backfill: find all provider configs that have no keys in the join table.
			// These previously meant "allow all keys", so set allow_all_keys = true.
			var allConfigs []tables.TableVirtualKeyProviderConfig
			if err := tx.Find(&allConfigs).Error; err != nil {
				return fmt.Errorf("failed to query provider configs: %w", err)
			}

			// Track which VK IDs were modified so we can recompute their config_hash.
			// Without this, subsequent config-sync diff logic would see a stale hash
			// and attempt to re-reconcile the VK (potentially undoing the backfill).
			modifiedVKIDs := make(map[string]struct{})

			logger.Info("[configstore] %s: processing %d allConfigs", migrationName, len(allConfigs))
			for _, pc := range allConfigs {
				var keyCount int64
				if err := tx.Table("governance_virtual_key_provider_config_keys").
					Where("table_virtual_key_provider_config_id = ?", pc.ID).
					Count(&keyCount).Error; err != nil {
					return fmt.Errorf("failed to count keys for provider config %d: %w", pc.ID, err)
				}

				if keyCount == 0 {
					if err := tx.Model(&tables.TableVirtualKeyProviderConfig{}).
						Where("id = ?", pc.ID).
						Update("allow_all_keys", true).Error; err != nil {
						return fmt.Errorf("failed to backfill allow_all_keys for provider config %d: %w", pc.ID, err)
					}
					modifiedVKIDs[pc.VirtualKeyID] = struct{}{}
				}
			}

			// Recompute and persist config_hash for every VK that was modified.
			logger.Info("[configstore] %s: processing %d modifiedVKIDs", migrationName, len(modifiedVKIDs))
			for vkID := range modifiedVKIDs {
				var vk tables.TableVirtualKey
				if err := tx.
					Preload("ProviderConfigs").
					Preload("ProviderConfigs.Keys").
					First(&vk, "id = ?", vkID).Error; err != nil {
					return fmt.Errorf("failed to reload VK %s for hash recomputation: %w", vkID, err)
				}
				newHash, err := GenerateVirtualKeyHash(vk)
				if err != nil {
					return fmt.Errorf("failed to generate hash for VK %s: %w", vkID, err)
				}
				if err := tx.Model(&tables.TableVirtualKey{}).
					Where("id = ?", vkID).
					Update("config_hash", newHash).Error; err != nil {
					return fmt.Errorf("failed to update config_hash for VK %s: %w", vkID, err)
				}
				logger.Info("[Migration] Recomputed config_hash for VK '%s'", vk.Name)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableVirtualKeyProviderConfig{}, "allow_all_keys"); err != nil {
				return fmt.Errorf("failed to drop allow_all_keys column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running allow_all_keys migration: %s", err.Error())
	}
	return nil
}

// migrationAddPricingRefactorColumns adds all new pricing columns introduced in the pricing module refactor
func migrationAddPricingRefactorColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_pricing_refactor_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			columns := []string{
				"input_cost_per_token_priority",
				"output_cost_per_token_priority",
				"cache_creation_input_token_cost_above_1hr",
				"cache_creation_input_token_cost_above_1hr_above_200k_tokens",
				"cache_creation_input_audio_token_cost",
				"cache_read_input_token_cost_priority",
				"input_cost_per_pixel",
				"output_cost_per_pixel",
				"output_cost_per_image_premium_image",
				"output_cost_per_image_above_512_and_512_pixels",
				"output_cost_per_image_above_512x512_pixels_premium",
				"output_cost_per_image_above_1024_and_1024_pixels",
				"output_cost_per_image_above_1024x1024_pixels_premium",
				"input_cost_per_audio_token",
				"input_cost_per_second",
				"input_cost_per_video_per_second",
				"input_cost_per_audio_per_second",
				"output_cost_per_audio_token",
				"search_context_cost_per_query",
				"code_interpreter_cost_per_session",
				"input_cost_per_character",
				"input_cost_per_token_above_128k_tokens",
				"input_cost_per_image_above_128k_tokens",
				"input_cost_per_video_per_second_above_128k_tokens",
				"input_cost_per_audio_per_second_above_128k_tokens",
				"output_cost_per_token_above_128k_tokens",
			}

			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			columns := []string{
				"input_cost_per_token_priority",
				"output_cost_per_token_priority",
				"cache_creation_input_token_cost_above_1hr",
				"cache_creation_input_token_cost_above_1hr_above_200k_tokens",
				"cache_creation_input_audio_token_cost",
				"cache_read_input_token_cost_priority",
				"input_cost_per_pixel",
				"output_cost_per_pixel",
				"output_cost_per_image_premium_image",
				"output_cost_per_image_above_512_and_512_pixels",
				"output_cost_per_image_above_512x512_pixels_premium",
				"output_cost_per_image_above_1024_and_1024_pixels",
				"output_cost_per_image_above_1024x1024_pixels_premium",
				"input_cost_per_audio_token",
				"input_cost_per_second",
				"input_cost_per_video_per_second",
				"input_cost_per_audio_per_second",
				"output_cost_per_audio_token",
				"search_context_cost_per_query",
				"code_interpreter_cost_per_session",
				"input_cost_per_character",
				"input_cost_per_token_above_128k_tokens",
				"input_cost_per_image_above_128k_tokens",
				"input_cost_per_video_per_second_above_128k_tokens",
				"input_cost_per_audio_per_second_above_128k_tokens",
				"output_cost_per_token_above_128k_tokens",
			}

			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running pricing refactor columns migration: %s", err.Error())
	}
	return nil
}

// migrationRenameTruncatedPricingColumn renames the output_cost_per_image_above_512_and_512_pixels_and_premium_image
// column which at 64 chars exceeds PostgreSQL's 63-character identifier limit. PostgreSQL silently truncated
// it to output_cost_per_image_above_512_and_512_pixels_and_premium_imag (63 chars), while SQLite kept the
// full 64-char name. This migration renames whichever variant exists to the shorter canonical name.
func migrationRenameTruncatedPricingColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "rename_truncated_pricing_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()

			const newName = "output_cost_per_image_above_512x512_pixels_premium"
			if mg.HasColumn(&tables.TableModelPricing{}, newName) {
				return nil
			}

			// PostgreSQL truncated the 64-char name to 63 chars
			const oldNamePG = "output_cost_per_image_above_512_and_512_pixels_and_premium_imag"
			// SQLite kept the full 64-char name
			const oldNameSQLite = "output_cost_per_image_above_512_and_512_pixels_and_premium_image"

			if mg.HasColumn(&tables.TableModelPricing{}, oldNamePG) {
				if err := tx.Exec("ALTER TABLE governance_model_pricing RENAME COLUMN " + oldNamePG + " TO " + newName).Error; err != nil {
					return fmt.Errorf("failed to rename column %s to %s: %w", oldNamePG, newName, err)
				}
			} else if mg.HasColumn(&tables.TableModelPricing{}, oldNameSQLite) {
				if err := tx.Exec("ALTER TABLE governance_model_pricing RENAME COLUMN " + oldNameSQLite + " TO " + newName).Error; err != nil {
					return fmt.Errorf("failed to rename column %s to %s: %w", oldNameSQLite, newName, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running rename_truncated_pricing_column migration: %s", err.Error())
	}
	return nil
}

// migrationAddImageQualityPricingColumns adds quality-based per-image cost columns (low, medium, high, auto).
func migrationAddImageQualityPricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_image_quality_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			columns := []string{
				"output_cost_per_image_above_2048_and_2048_pixels",
				"output_cost_per_image_above_4096_and_4096_pixels",
				"output_cost_per_image_low_quality",
				"output_cost_per_image_medium_quality",
				"output_cost_per_image_high_quality",
				"output_cost_per_image_auto_quality",
			}
			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			columns := []string{
				"output_cost_per_image_above_2048_and_2048_pixels",
				"output_cost_per_image_above_4096_and_4096_pixels",
				"output_cost_per_image_low_quality",
				"output_cost_per_image_medium_quality",
				"output_cost_per_image_high_quality",
				"output_cost_per_image_auto_quality",
			}
			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running image quality pricing columns migration: %s", err.Error())
	}
	return nil
}

// legacyRoutingRuleColumns is a migration-only struct that represents the old routing_rules
// schema before provider/model/key_id were moved to the routing_targets table.
// GORM's SQLite DropColumn/AddColumn need a real struct (not a string table name) to
// reconstruct the table correctly, so we keep this stub around for migration use only.
type legacyRoutingRuleColumns struct {
	Provider string `gorm:"column:provider;type:varchar(255)"`
	Model    string `gorm:"column:model;type:varchar(255)"`
}

func (legacyRoutingRuleColumns) TableName() string { return "routing_rules" }

// migrationAddRoutingTargetsTable creates the routing_targets table and seeds one target row per
// existing routing rule, migrating the legacy provider/model columns.
// After seeding, the legacy columns are dropped from routing_rules.
func migrationAddRoutingTargetsTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_routing_targets_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()

			// 1. Create routing_targets table
			if !mg.HasTable(&tables.TableRoutingTarget{}) {
				logger.Info("[configstore] %s: creating table TableRoutingTarget", migrationName)
				if err := mg.CreateTable(&tables.TableRoutingTarget{}); err != nil {
					return fmt.Errorf("failed to create routing_targets table: %w", err)
				}
			}
			if !mg.HasConstraint(&tables.TableRoutingRule{}, "Targets") {
				if err := mg.CreateConstraint(&tables.TableRoutingRule{}, "Targets"); err != nil {
					return fmt.Errorf("failed to create routing_targets foreign key: %w", err)
				}
			}

			// 2. Read legacy data BEFORE dropping columns, then drop columns, then seed.
			// Order matters: DropColumn on SQLite recreates the routing_rules table, which
			// triggers the OnDelete:CASCADE on routing_targets and deletes any rows inserted
			// before the drop. So we read first, drop, then insert.
			type legacyRule struct {
				ID       string
				Provider string
				Model    string
			}
			var legacyRows []legacyRule
			if mg.HasColumn("routing_rules", "provider") {
				if err := tx.Table("routing_rules").Select("id, provider, model").Scan(&legacyRows).Error; err != nil {
					return fmt.Errorf("failed to scan routing_rules for seeding: %w", err)
				}
			}

			// 3. Drop legacy single-target columns from routing_rules.
			// Must use the struct form (not string) so SQLite can reconstruct the table correctly.
			// Do this BEFORE seeding so the CASCADE triggered by table recreation hits an empty
			// routing_targets table (nothing to delete yet).
			legacyModel := &legacyRoutingRuleColumns{}
			for _, col := range []string{"provider", "model"} {
				if err := dropColumnIfExists(tx, logger, legacyModel, col); err != nil {
					return fmt.Errorf("failed to drop column %s from routing_rules: %w", col, err)
				}
			}

			// 4. Seed routing_targets from the legacy data read above (idempotent).
			logger.Info("[configstore] %s: processing %d legacyRows", migrationName, len(legacyRows))
			for _, row := range legacyRows {
				var count int64
				if err := tx.Table("routing_targets").Where("rule_id = ?", row.ID).Count(&count).Error; err != nil {
					return fmt.Errorf("failed to count targets for rule %s: %w", row.ID, err)
				}
				if count > 0 {
					continue // already seeded
				}
				target := tables.TableRoutingTarget{
					RuleID: row.ID,
					Weight: 1.0,
				}
				if row.Provider != "" {
					p := row.Provider
					target.Provider = &p
				}
				if row.Model != "" {
					m := row.Model
					target.Model = &m
				}
				if err := tx.Create(&target).Error; err != nil {
					return fmt.Errorf("failed to seed target for rule %s: %w", row.ID, err)
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()

			if !mg.HasTable(&tables.TableRoutingTarget{}) {
				return nil
			}

			// 1. Add provider and model columns back to routing_rules (before dropping targets)
			legacyModel := &legacyRoutingRuleColumns{}
			for _, col := range []string{"provider", "model"} {
				if err := addColumnIfNotExists(tx, logger, legacyModel, col); err != nil {
					return fmt.Errorf("failed to add column %s to routing_rules: %w", col, err)
				}
			}

			// 2. Backfill provider/model from routing_targets into routing_rules (join by rule_id)
			type targetRow struct {
				RuleID   string
				Provider *string
				Model    *string
			}
			var targets []targetRow
			if err := tx.Table("routing_targets").Select("rule_id, provider, model").Order("rule_id").Scan(&targets).Error; err != nil {
				return fmt.Errorf("failed to scan routing_targets for backfill: %w", err)
			}
			ruleData := make(map[string]targetRow)
			for _, t := range targets {
				if _, ok := ruleData[t.RuleID]; !ok {
					ruleData[t.RuleID] = t
				}
			}
			logger.Info("[configstore] %s: processing %d ruleData", migrationName, len(ruleData))
			for ruleID, t := range ruleData {
				provider, model := "", ""
				if t.Provider != nil {
					provider = *t.Provider
				}
				if t.Model != nil {
					model = *t.Model
				}
				if err := tx.Table("routing_rules").Where("id = ?", ruleID).Updates(map[string]interface{}{
					"provider": provider,
					"model":    model,
				}).Error; err != nil {
					return fmt.Errorf("failed to backfill routing_rule %s: %w", ruleID, err)
				}
			}

			// 3. Drop routing_targets table
			if mg.HasConstraint(&tables.TableRoutingRule{}, "Targets") {
				if err := mg.DropConstraint(&tables.TableRoutingRule{}, "Targets"); err != nil {
					return fmt.Errorf("failed to drop routing_targets foreign key: %w", err)
				}
			}
			logger.Info("[configstore] %s: dropping table TableRoutingTarget", migrationName)
			if err := mg.DropTable(&tables.TableRoutingTarget{}); err != nil {
				return fmt.Errorf("failed to drop routing_targets table: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running routing_targets_table migration: %s", err.Error())
	}
	return nil
}

// migrationAddModelParametersTable adds the table of model parameters and capabilities.
func migrationAddModelParametersTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_model_parameters_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: "add_model_parameters_table",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			if !migrator.HasTable(&tables.TableModelParameters{}) {
				logger.Info("[configstore] %s: creating table TableModelParameters", migrationName)
				if err := migrator.CreateTable(&tables.TableModelParameters{}); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			if migrator.HasTable(&tables.TableModelParameters{}) {
				logger.Info("[configstore] %s: dropping table TableModelParameters", migrationName)
				if err := migrator.DropTable(&tables.TableModelParameters{}); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running add_model_parameters_table migration: %s", err.Error())
	}

	return nil
}

// migrationBackfillAllowedModelsWildcard converts empty allowed_models on
// governance_virtual_key_provider_configs and empty models_json on keys to ["*"],
// preserving the previous "empty = allow all" semantics for existing records.
// After this migration the new convention applies: ["*"] = allow all, [] = deny all.
func migrationBackfillAllowedModelsWildcard(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "backfill_allowed_models_wildcard"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// --- Field 1: vk.provider_config.allowed_models ---
			// Rows with '[]' previously meant "allow all models"; migrate to '["*"]'.
			if err := tx.Model(&tables.TableVirtualKeyProviderConfig{}).
				Where("allowed_models = ? OR allowed_models IS NULL", `[]`).
				Update("allowed_models", `["*"]`).Error; err != nil {
				return fmt.Errorf("failed to backfill provider_config allowed_models: %w", err)
			}

			// Recompute config_hash for all VKs that have provider configs
			// (any of them may have had their allowed_models updated above).
			var modifiedVKIDs []string
			if err := tx.Model(&tables.TableVirtualKeyProviderConfig{}).
				Distinct("virtual_key_id").
				Pluck("virtual_key_id", &modifiedVKIDs).Error; err != nil {
				return fmt.Errorf("failed to query VK IDs for hash recomputation: %w", err)
			}

			logger.Info("[configstore] %s: processing %d modifiedVKIDs", migrationName, len(modifiedVKIDs))
			for _, vkID := range modifiedVKIDs {
				var vk tables.TableVirtualKey
				if err := tx.
					Preload("ProviderConfigs").
					Preload("ProviderConfigs.Keys").
					First(&vk, "id = ?", vkID).Error; err != nil {
					if err == gorm.ErrRecordNotFound {
						// Orphaned provider config row — VK was deleted; skip.
						continue
					}
					return fmt.Errorf("failed to reload VK %s for hash recomputation: %w", vkID, err)
				}
				newHash, err := GenerateVirtualKeyHash(vk)
				if err != nil {
					return fmt.Errorf("failed to generate hash for VK %s: %w", vkID, err)
				}
				if err := tx.Model(&tables.TableVirtualKey{}).
					Where("id = ?", vkID).
					Update("config_hash", newHash).Error; err != nil {
					return fmt.Errorf("failed to update config_hash for VK %s: %w", vkID, err)
				}
				logger.Info("[Migration] Recomputed config_hash for VK '%s' after allowed_models backfill", vk.Name)
			}

			// --- Field 2: provider.key.models (models_json column) ---
			// Rows with '[]' or empty string previously meant "allow all models"; migrate to '["*"]'.
			if err := tx.Model(&tables.TableKey{}).
				Where("models_json = ? OR models_json = ? OR models_json IS NULL", `[]`, ``).
				Update("models_json", `["*"]`).Error; err != nil {
				return fmt.Errorf("failed to backfill key models_json: %w", err)
			}

			// Recompute config_hash for all keys since models_json is part of the hash input.
			var keys []tables.TableKey
			if err := tx.Find(&keys).Error; err != nil {
				return fmt.Errorf("failed to fetch keys for hash recomputation: %w", err)
			}
			logger.Info("[configstore] %s: processing %d keys", migrationName, len(keys))
			for _, key := range keys {
				schemaKey := schemas.Key{
					Name:                key.Name,
					Value:               key.Value,
					Models:              key.Models,
					Weight:              getWeight(key.Weight),
					AzureKeyConfig:      key.AzureKeyConfig,
					VertexKeyConfig:     key.VertexKeyConfig,
					BedrockKeyConfig:    key.BedrockKeyConfig,
					Aliases:             key.Aliases,
					VLLMKeyConfig:       key.VLLMKeyConfig,
					ReplicateKeyConfig:  key.ReplicateKeyConfig,
					OllamaKeyConfig:     key.OllamaKeyConfig,
					SGLKeyConfig:        key.SGLKeyConfig,
					DatabricksKeyConfig: key.DatabricksKeyConfig,
					Enabled:             key.Enabled,
					UseForBatchAPI:      key.UseForBatchAPI,
				}
				hash, err := GenerateKeyHash(schemaKey)
				if err != nil {
					return fmt.Errorf("failed to generate hash for key %s: %w", key.Name, err)
				}
				if err := tx.Model(&key).Update("config_hash", hash).Error; err != nil {
					return fmt.Errorf("failed to update config_hash for key %s: %w", key.Name, err)
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			// Rollback is intentionally a no-op: reverting ["*"] back to [] would
			// re-introduce the ambiguous "empty = allow all" semantics on downgrade.
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running backfill_allowed_models_wildcard migration: %s", err.Error())
	}
	return nil
}

// migrationRepairBareWildcardAllowedModels repairs governance_virtual_key_provider_configs
// rows whose allowed_models / blacklisted_models column holds the bare one-character
// string '*' instead of the JSON array '["*"]'. Such rows abort the GORM json
// deserializer ("invalid character '*' ...") when the VK is loaded, which poisons the
// whole provider admin surface (see issue #4318). The repair rewrites the column to the
// canonical '["*"]' form the serializer:json tag is supposed to produce; the intended
// value at write time was already the WhiteList ["*"], so the config_hash — computed
// from the in-memory slice — stays consistent and needs no recomputation.
func migrationRepairBareWildcardAllowedModels(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "repair_bare_wildcard_allowed_models"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// Match the documented manual workaround exactly: bare '*' → '["*"]'.
			// Covers both whitelist columns on the provider config, which share the
			// serializer:json tag and the same corruption class.
			for _, column := range []string{"allowed_models", "blacklisted_models"} {
				res := tx.Model(&tables.TableVirtualKeyProviderConfig{}).
					Where(column+" = ?", "*").
					Update(column, `["*"]`)
				if res.Error != nil {
					return fmt.Errorf("failed to repair bare wildcard %s: %w", column, res.Error)
				}
				if res.RowsAffected > 0 {
					logger.Info("[configstore] %s: repaired %d rows with bare wildcard %s", migrationName, res.RowsAffected, column)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			// Rollback is a no-op: reverting '["*"]' back to '*' would re-introduce
			// the value that breaks the deserializer.
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// migrationAddSkillsRepoTables adds the skills repository tables.
// Files belong to skill_versions (not directly to skills); blobs are reused
// across versions via shared blob_id/storage_key references.
//
// Idempotent: guards each table create so retrying after a partially applied
// migration does not fail when some tables were already created.
func migrationAddSkillsRepoTables(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	logger.Info("[configstore] running migrationAddSkillsRepoTables")
	defer logger.Info("[configstore] migrationAddSkillsRepoTables finished")
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: "add_skills_repo_tables",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()
			// --- skills table ---
			if !mg.HasTable(&tables.TableSkill{}) {
				if err := mg.CreateTable(&tables.TableSkill{}); err != nil {
					return fmt.Errorf("create skills table: %w", err)
				}
			}
			// --- skill_versions table ---
			if !mg.HasTable(&tables.TableSkillVersion{}) {
				if err := mg.CreateTable(&tables.TableSkillVersion{}); err != nil {
					return fmt.Errorf("create skill_versions table: %w", err)
				}
			}
			// --- skill_file_blobs table ---
			if !mg.HasTable(&tables.TableSkillFileBlob{}) {
				if err := mg.CreateTable(&tables.TableSkillFileBlob{}); err != nil {
					return fmt.Errorf("create skill_file_blobs table: %w", err)
				}
			}
			// --- skill_files table ---
			if !mg.HasTable(&tables.TableSkillFile{}) {
				if err := mg.CreateTable(&tables.TableSkillFile{}); err != nil {
					return fmt.Errorf("create skill_files table: %w", err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()
			if err := mg.DropTable(&tables.TableSkillFile{}); err != nil {
				return err
			}
			if err := mg.DropTable(&tables.TableSkillVersion{}); err != nil {
				return err
			}
			if err := mg.DropTable(&tables.TableSkill{}); err != nil {
				return err
			}
			if err := mg.DropTable(&tables.TableSkillFileBlob{}); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running skills repo tables migration: %s", err.Error())
	}
	return nil
}

// migrationAddPluginOrderColumns adds placement and exec_order columns to config_plugins table
func migrationAddPluginOrderColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_plugin_order_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := addColumnIfNotExists(tx, logger, &tables.TablePlugin{}, "Placement"); err != nil {
				return fmt.Errorf("failed to add placement column: %w", err)
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TablePlugin{}, "Order"); err != nil {
				return fmt.Errorf("failed to add exec_order column: %w", err)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := dropColumnIfExists(tx, logger, &tables.TablePlugin{}, "placement"); err != nil {
				return fmt.Errorf("failed to drop placement column: %w", err)
			}
			if err := dropColumnIfExists(tx, logger, &tables.TablePlugin{}, "exec_order"); err != nil {
				return fmt.Errorf("failed to drop exec_order column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running add_plugin_order_columns migration: %s", err.Error())
	}
	return nil
}

// migrationMakeBasePricingColumnsNullable drops the NOT NULL constraint on
// input_cost_per_token and output_cost_per_token in governance_model_pricing,
// allowing models that only have non-token pricing (image, audio, video) to be
// stored without a placeholder zero value.
func migrationMakeBasePricingColumnsNullable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "make_base_pricing_columns_nullable"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			m := tx.Migrator()
			if err := m.AlterColumn(&tables.TableModelPricing{}, "InputCostPerToken"); err != nil {
				return fmt.Errorf("failed to alter input_cost_per_token: %w", err)
			}
			if err := m.AlterColumn(&tables.TableModelPricing{}, "OutputCostPerToken"); err != nil {
				return fmt.Errorf("failed to alter output_cost_per_token: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running make_base_pricing_columns_nullable migration: %s", err.Error())
	}
	return nil
}

// migrationAddOpenAIConfigJSONColumn adds the open_ai_config_json column to the provider table
func migrationAddOpenAIConfigJSONColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_open_ai_config_json_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableProvider{}, "OpenAIConfigJSON"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableProvider{}, "open_ai_config_json"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running add_open_ai_config_json_column migration: %s", err.Error())
	}
	return nil
}

// migrationAddPromptCacheJSONColumn adds the prompt_cache_json column to the provider
// table, backing ProviderConfig.PromptCache.
//
// Provider config is stored as one text column per sub-struct rather than a single
// JSON blob, so a new config struct needs its own column. Without this the field
// round-trips through the API and the UI but is dropped at the database boundary.
func migrationAddPromptCacheJSONColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_prompt_cache_json_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableProvider{}, "PromptCacheJSON"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableProvider{}, "prompt_cache_json"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running add_prompt_cache_json_column migration: %s", err.Error())
	}
	return nil
}

// migrationAddKeyBlacklistedModelsJSONColumn adds blacklisted_models_json to config_keys
// for per-key model deny lists (JSON array of model ids, default []).
func migrationAddKeyBlacklistedModelsJSONColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_key_blacklisted_models_json_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "blacklisted_models_json"); err != nil {
				return fmt.Errorf("failed to add blacklisted_models_json column: %w", err)
			}
			if err := tx.Exec("UPDATE config_keys SET blacklisted_models_json = '[]' WHERE blacklisted_models_json IS NULL OR blacklisted_models_json = ''").Error; err != nil {
				return fmt.Errorf("failed to backfill blacklisted_models_json: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "blacklisted_models_json"); err != nil {
				return fmt.Errorf("failed to drop blacklisted_models_json column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_key_blacklisted_models_json_column migration: %s", err.Error())
	}
	return nil
}

// migrationAddChainRuleColumnToRoutingRules adds chain_rule to routing_rules.
// When true, the routing engine re-evaluates the full rule set after this rule matches,
// using the resolved provider/model as the new context input.
func migrationAddChainRuleColumnToRoutingRules(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_chain_rule_column_to_routing_rules"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableRoutingRule{}, "chain_rule"); err != nil {
				return fmt.Errorf("failed to add chain_rule column: %w", err)
			}

			// Backfill config_hash for all existing routing rules.
			// GenerateRoutingRuleHash now includes chain_rule, so existing hashes
			// (computed without it) are stale and must be recomputed to avoid
			// every rule appearing as changed after this upgrade.
			var rules []tables.TableRoutingRule
			if err := tx.Preload("Targets").Find(&rules).Error; err != nil {
				return fmt.Errorf("failed to load routing rules for config_hash backfill: %w", err)
			}
			logger.Info("[configstore] %s: processing %d rules", migrationName, len(rules))
			for _, rule := range rules {
				hash, err := GenerateRoutingRuleHash(rule)
				if err != nil {
					return fmt.Errorf("failed to generate config_hash for routing rule %s: %w", rule.ID, err)
				}
				if err := tx.Model(&tables.TableRoutingRule{}).Where("id = ?", rule.ID).Update("config_hash", hash).Error; err != nil {
					return fmt.Errorf("failed to update config_hash for routing rule %s: %w", rule.ID, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableRoutingRule{}, "chain_rule"); err != nil {
				return fmt.Errorf("failed to drop chain_rule column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_chain_rule_column_to_routing_rules migration: %s", err.Error())
	}
	return nil
}

// migrationAddReplicateKeyConfigColumn adds the replicate_use_deployments_endpoint column to the key table
func migrationAddReplicateKeyConfigColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_replicate_key_config_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()
			if !mg.HasColumn(&tables.TableKey{}, "replicate_use_deployments_endpoint") {
				if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "replicate_use_deployments_endpoint"); err != nil {
					return err
				}
				// Backfill: Replicate keys that had deployments configured (now in aliases_json after
				// migrationDropDeploymentColumnsAndAddAliases) were using the deployments endpoint.
				trueVal := true
				if err := tx.Model(&tables.TableKey{}).
					Where("provider = ? AND aliases_json IS NOT NULL AND aliases_json != ? AND aliases_json != ?",
						string(schemas.Replicate), "", "{}",
					).
					Update("ReplicateUseDeploymentsEndpoint", &trueVal).Error; err != nil {
					return err
				}

				// Recompute config_hash for Replicate keys that were updated above,
				// since replicate_use_deployments_endpoint is part of the hash input.
				var affectedKeys []tables.TableKey
				if err := tx.Where(
					"provider = ? AND replicate_use_deployments_endpoint IS NOT NULL",
					string(schemas.Replicate),
				).Find(&affectedKeys).Error; err != nil {
					return fmt.Errorf("failed to fetch replicate keys for hash recomputation: %w", err)
				}
				logger.Info("[configstore] %s: processing %d affectedKeys", migrationName, len(affectedKeys))
				for _, key := range affectedKeys {
					schemaKey := schemas.Key{
						Name:               key.Name,
						Value:              key.Value,
						Models:             key.Models,
						BlacklistedModels:  key.BlacklistedModels,
						Weight:             getWeight(key.Weight),
						AzureKeyConfig:     key.AzureKeyConfig,
						VertexKeyConfig:    key.VertexKeyConfig,
						BedrockKeyConfig:   key.BedrockKeyConfig,
						Aliases:            key.Aliases,
						VLLMKeyConfig:      key.VLLMKeyConfig,
						ReplicateKeyConfig: key.ReplicateKeyConfig,
						Enabled:            key.Enabled,
						UseForBatchAPI:     key.UseForBatchAPI,
					}
					hash, err := GenerateKeyHash(schemaKey)
					if err != nil {
						return fmt.Errorf("failed to generate hash for key %s: %w", key.Name, err)
					}
					if err := tx.Model(&key).Update("config_hash", hash).Error; err != nil {
						return fmt.Errorf("failed to update config_hash for key %s: %w", key.Name, err)
					}
					logger.Info("[Migration] Recomputed config_hash for replicate key '%s' after replicate config backfill", key.Name)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "replicate_use_deployments_endpoint"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_replicate_key_config_column migration: %s", err.Error())
	}
	return nil
}

// migrationAddBudgetCalendarAlignedColumn was originally for adding calendar_aligned to governance_budgets.
// Calendar alignment is now a VK-level field (governance_virtual_keys.calendar_aligned) added in migrationAddMultiBudgetTables.
// This migration is kept as a no-op so the migrator doesn't try to re-run it.
func migrationAddBudgetCalendarAlignedColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_budget_calendar_aligned_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID:       migrationName,
		Migrate:  func(tx *gorm.DB) error { return nil },
		Rollback: func(tx *gorm.DB) error { return nil },
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_budget_calendar_aligned_column migration: %s", err.Error())
	}
	return nil
}

// migrationAddRoutingChainMaxDepthColumn adds routing_chain_max_depth to the client config table.
// Defaults to 10, which is the built-in default for routing rule chain evaluation depth.
func migrationAddRoutingChainMaxDepthColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_routing_chain_max_depth_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()
			if !mg.HasColumn(&tables.TableClientConfig{}, "routing_chain_max_depth") {
				if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "routing_chain_max_depth"); err != nil {
					return fmt.Errorf("failed to add routing_chain_max_depth column: %w", err)
				}
				// Recompute config_hash for all existing client configs that have one.
				// RoutingChainMaxDepth is now included in the hash (when > 0), so without
				// this recompute the stored hash would mismatch on every startup after upgrade.
				var clientConfigs []tables.TableClientConfig
				if err := tx.Find(&clientConfigs).Error; err != nil {
					return fmt.Errorf("failed to fetch client configs for hash recompute: %w", err)
				}
				logger.Info("[configstore] %s: processing %d clientConfigs", migrationName, len(clientConfigs))
				for _, cc := range clientConfigs {
					if cc.ConfigHash == "" {
						continue // no stored hash to invalidate
					}
					depth := cc.RoutingChainMaxDepth
					if depth == 0 {
						// Should never happen, but just in case.
						depth = 10 // DefaultRoutingChainMaxDepth
					}
					clientConfig := ClientConfig{
						DropExcessRequests:              cc.DropExcessRequests,
						InitialPoolSize:                 cc.InitialPoolSize,
						PrometheusLabels:                cc.PrometheusLabels,
						EnableLogging:                   cc.EnableLogging,
						DisableContentLogging:           cc.DisableContentLogging,
						DisableDBPingsInHealth:          cc.DisableDBPingsInHealth,
						LogRetentionDays:                cc.LogRetentionDays,
						EnforceAuthOnInference:          cc.EnforceAuthOnInference,
						AllowedOrigins:                  cc.AllowedOrigins,
						AllowedHeaders:                  cc.AllowedHeaders,
						MaxRequestBodySizeMB:            cc.MaxRequestBodySizeMB,
						HideDeletedVirtualKeysInFilters: cc.HideDeletedVirtualKeysInFilters,
						AsyncJobResultTTL:               cc.AsyncJobResultTTL,
						LoggingHeaders:                  cc.LoggingHeaders,
						RequiredHeaders:                 cc.RequiredHeaders,
						HeaderFilterConfig:              cc.HeaderFilterConfig,
						RoutingChainMaxDepth:            depth,
					}
					newHash, err := clientConfig.GenerateClientConfigHash()
					if err != nil {
						return fmt.Errorf("failed to generate hash for client config %d: %w", cc.ID, err)
					}
					if err := tx.Model(&cc).Update("config_hash", newHash).Error; err != nil {
						return fmt.Errorf("failed to update hash for client config %d: %w", cc.ID, err)
					}
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "routing_chain_max_depth"); err != nil {
				return fmt.Errorf("failed to drop routing_chain_max_depth column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_routing_chain_max_depth_column migration: %s", err.Error())
	}
	return nil
}

// migrationAddModelCapabilityColumns adds model capability metadata columns to governance_model_pricing.
func migrationAddModelCapabilityColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_model_capability_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			columns := []string{
				"context_length",
				"max_input_tokens",
				"max_output_tokens",
				"architecture",
			}
			for _, column := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, column); err != nil {
					return fmt.Errorf("failed to add %s column: %w", column, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			columns := []string{
				"context_length",
				"max_input_tokens",
				"max_output_tokens",
				"architecture",
			}
			for _, column := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, column); err != nil {
					return fmt.Errorf("failed to drop %s column: %w", column, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_model_capability_columns migration: %s", err.Error())
	}
	return nil
}

// migrationAddOllamaSGLConfigColumns adds ollama_url and sgl_url columns to the key table
func migrationAddOllamaSGLConfigColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_ollama_sgl_config_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "ollama_url"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "sgl_url"); err != nil {
				return err
			}

			// Backfill: for each ollama/sgl provider with a base_url, create a key
			// with that URL and clear base_url from network_config.
			var providers []tables.TableProvider
			if err := tx.Where("name IN ?", []string{"ollama", "sgl"}).Find(&providers).Error; err != nil {
				return fmt.Errorf("failed to fetch ollama/sgl providers for URL backfill: %w", err)
			}
			logger.Info("[configstore] %s: processing %d providers", migrationName, len(providers))
			for _, p := range providers {
				if p.NetworkConfigJSON == "" {
					continue
				}
				var nc schemas.NetworkConfig
				if err := json.Unmarshal([]byte(p.NetworkConfigJSON), &nc); err != nil {
					logger.Info("[Migration] Failed to parse network_config for provider %s (id=%d), skipping: %v", p.Name, p.ID, err)
					continue
				}
				if nc.BaseURL == "" {
					continue
				}

				// Create a new key with the provider's base_url
				urlSecretVar := schemas.SecretVar{Val: nc.BaseURL}
				enabled := true
				weight := 1.0
				newKey := tables.TableKey{
					Provider:   p.Name,
					ProviderID: p.ID,
					KeyID:      uuid.NewString(),
					Weight:     &weight,
					Enabled:    &enabled,
					Models:     schemas.WhiteList{"*"},
				}
				if strings.ToLower(p.Name) == "ollama" {
					newKey.Name = "Default Ollama Key"
					newKey.OllamaKeyConfig = &schemas.OllamaKeyConfig{URL: urlSecretVar}
				}
				if strings.ToLower(p.Name) == "sgl" {
					newKey.Name = "Default SGL Key"
					newKey.SGLKeyConfig = &schemas.SGLKeyConfig{URL: urlSecretVar}
				}

				schemaKey := schemaKeyFromTableKey(newKey)
				hash, err := GenerateKeyHash(schemaKey)
				if err != nil {
					return fmt.Errorf("failed to generate hash for new key on provider %s: %w", p.Name, err)
				}
				newKey.ConfigHash = hash
				if err := tx.Create(&newKey).Error; err != nil {
					return fmt.Errorf("failed to create key for provider %s: %w", p.Name, err)
				}
				logger.Info("[Migration] Created key '%s' for provider '%s' from network_config.base_url", newKey.Name, p.Name)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "ollama_url"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "sgl_url"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running ollama sgl key config columns migration: %s", err.Error())
	}
	return nil
}

// migrationAddMultiBudgetTables creates junction tables for multi-budget support and backfills existing data.
func migrationAddMultiBudgetTables(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_multi_budget_tables"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()
			// Add calendar_aligned to governance_virtual_keys (VK-level setting)
			if err := addColumnIfNotExists(tx, logger, &tables.TableVirtualKey{}, "CalendarAligned"); err != nil {
				return fmt.Errorf("failed to add calendar_aligned column to governance_virtual_keys: %w", err)
			}

			// Add FK columns on governance_budgets for multi-budget ownership
			if err := addColumnIfNotExists(tx, logger, &tables.TableBudget{}, "VirtualKeyID"); err != nil {
				return fmt.Errorf("failed to add virtual_key_id column to governance_budgets: %w", err)
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableBudget{}, "ProviderConfigID"); err != nil {
				return fmt.Errorf("failed to add provider_config_id column to governance_budgets: %w", err)
			}

			// Create indexes on the new FK columns (AddColumn doesn't create indexes from struct tags)
			if !mg.HasIndex(&tables.TableBudget{}, "idx_governance_budgets_virtual_key_id") {
				logger.Info("[configstore] %s: creating index VirtualKeyID on TableBudget", migrationName)
				if err := mg.CreateIndex(&tables.TableBudget{}, "VirtualKeyID"); err != nil {
					return fmt.Errorf("failed to create index on governance_budgets.virtual_key_id: %w", err)
				}
			}
			if !mg.HasIndex(&tables.TableBudget{}, "idx_governance_budgets_provider_config_id") {
				logger.Info("[configstore] %s: creating index ProviderConfigID on TableBudget", migrationName)
				if err := mg.CreateIndex(&tables.TableBudget{}, "ProviderConfigID"); err != nil {
					return fmt.Errorf("failed to create index on governance_budgets.provider_config_id: %w", err)
				}
			}

			// Backfill: set virtual_key_id from legacy VK budget_id (if column still exists)
			if mg.HasColumn(&tables.TableVirtualKey{}, "budget_id") {
				if err := tx.Exec(`
					UPDATE governance_budgets SET virtual_key_id = (
						SELECT id FROM governance_virtual_keys
						WHERE governance_virtual_keys.budget_id = governance_budgets.id
					) WHERE virtual_key_id IS NULL AND EXISTS (
						SELECT 1 FROM governance_virtual_keys
						WHERE governance_virtual_keys.budget_id = governance_budgets.id
					)
				`).Error; err != nil {
					return fmt.Errorf("failed to backfill VK budget virtual_key_id: %w", err)
				}
			}

			// Backfill: set provider_config_id from legacy PC budget_id (if column still exists)
			if mg.HasColumn(&tables.TableVirtualKeyProviderConfig{}, "budget_id") {
				if err := tx.Exec(`
					UPDATE governance_budgets SET provider_config_id = (
						SELECT id FROM governance_virtual_key_provider_configs
						WHERE governance_virtual_key_provider_configs.budget_id = governance_budgets.id
					) WHERE provider_config_id IS NULL AND EXISTS (
						SELECT 1 FROM governance_virtual_key_provider_configs
						WHERE governance_virtual_key_provider_configs.budget_id = governance_budgets.id
					)
				`).Error; err != nil {
					return fmt.Errorf("failed to backfill PC budget provider_config_id: %w", err)
				}
			}

			// Backfill: copy calendar_aligned from legacy budget column to VK-level field
			// (governance_budgets.calendar_aligned was added by add_budget_calendar_aligned_column on main)
			if mg.HasColumn(&tables.TableBudget{}, "calendar_aligned") {
				if err := tx.Exec(`
					UPDATE governance_virtual_keys SET calendar_aligned = true
					WHERE id IN (
						SELECT DISTINCT virtual_key_id FROM governance_budgets
						WHERE calendar_aligned = true AND virtual_key_id IS NOT NULL
					) AND calendar_aligned = false
				`).Error; err != nil {
					return fmt.Errorf("failed to backfill calendar_aligned from budgets to virtual keys: %w", err)
				}
				// Drop the legacy calendar_aligned column from governance_budgets.
				// Plain column with no FK references — not a correctness risk if left behind,
				// but log a warning so it's not invisible.
				if err := tx.Exec(dropColumnSQL(tx, "governance_budgets", "calendar_aligned")).Error; err != nil {
					logger.Info("[Migration] warning: could not drop legacy calendar_aligned column from governance_budgets: %v", err)
				}
			}

			// Drop legacy budget_id columns BEFORE creating FK constraints.
			// On SQLite, ALTER TABLE RENAME propagates into FK references in other tables.
			// If we create FK constraints on governance_budgets first, then rename the
			// parent table during the legacy column drop (table rebuild), SQLite updates
			// those FK references to point at the temporary backup table name.
			if err := dropLegacyBudgetColumn(tx, "governance_virtual_keys"); err != nil {
				return err
			}
			if err := dropLegacyBudgetColumn(tx, "governance_virtual_key_provider_configs"); err != nil {
				return err
			}

			// Create FK constraints with CASCADE delete (defined on parent structs).
			// Must happen after legacy column drops so SQLite rename propagation
			// cannot corrupt these FK references.
			if !mg.HasConstraint(&tables.TableVirtualKey{}, "Budgets") {
				if err := mg.CreateConstraint(&tables.TableVirtualKey{}, "Budgets"); err != nil {
					return fmt.Errorf("failed to create FK constraint for VirtualKey -> Budgets: %w", err)
				}
			}
			if !mg.HasConstraint(&tables.TableVirtualKeyProviderConfig{}, "Budgets") {
				if err := mg.CreateConstraint(&tables.TableVirtualKeyProviderConfig{}, "Budgets"); err != nil {
					return fmt.Errorf("failed to create FK constraint for ProviderConfig -> Budgets: %w", err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableBudget{}, "VirtualKeyID"); err != nil {
				return fmt.Errorf("failed to drop virtual_key_id column from governance_budgets: %w", err)
			}
			if err := dropColumnIfExists(tx, logger, &tables.TableBudget{}, "ProviderConfigID"); err != nil {
				return fmt.Errorf("failed to drop provider_config_id column from governance_budgets: %w", err)
			}
			return nil
		},
	}})
	// SQLite workaround: GORM's CreateConstraint rebuilds the table via DROP+RENAME
	// inside a transaction. The DROP fails when other tables have FKs pointing at the
	// target table and foreign_keys is ON. PRAGMA foreign_keys cannot be changed inside
	// a transaction, so we disable it before the migrator opens its transaction.
	// This only affects SQLite — Postgres supports ALTER TABLE ADD CONSTRAINT natively.
	if db.Dialector.Name() == "sqlite" {
		// PRAGMA foreign_keys is per-connection in SQLite. Pin the pool to a single
		// connection so the PRAGMA and the migration transaction share the same one.
		sqlDB, err := db.DB()
		if err != nil {
			return fmt.Errorf("failed to get underlying sql.DB: %w", err)
		}
		prevMaxOpenConns := sqlDB.Stats().MaxOpenConnections
		sqlDB.SetMaxOpenConns(1)
		defer sqlDB.SetMaxOpenConns(prevMaxOpenConns)

		if err := db.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
			return fmt.Errorf("failed to disable SQLite foreign keys: %w", err)
		}
		defer func() {
			if err := db.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
				log.Fatalf("[Migration] FATAL: failed to re-enable SQLite foreign keys: %v", err)
			}
		}()
	}
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_multi_budget_tables migration: %s", err.Error())
	}
	return nil
}

// migrationAddTeamBudgetsToBudgetsTable pivots team budgets from a single-FK on
// governance_teams.budget_id to multi-budget ownership via governance_budgets.team_id,
// mirroring how VK/ProviderConfig budgets were restructured in migrationAddMultiBudgetTables.
func migrationAddTeamBudgetsToBudgetsTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_team_budgets_to_budgets_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()

			// Add team_id FK column on governance_budgets
			if err := addColumnIfNotExists(tx, logger, &tables.TableBudget{}, "TeamID"); err != nil {
				return fmt.Errorf("failed to add team_id column to governance_budgets: %w", err)
			}

			// Create index on the new FK column (AddColumn doesn't create indexes from struct tags)
			if !mg.HasIndex(&tables.TableBudget{}, "idx_governance_budgets_team_id") {
				logger.Info("[configstore] %s: creating index TeamID on TableBudget", migrationName)
				if err := mg.CreateIndex(&tables.TableBudget{}, "TeamID"); err != nil {
					return fmt.Errorf("failed to create index on governance_budgets.team_id: %w", err)
				}
			}

			// Backfill: set team_id from legacy governance_teams.budget_id (if column still exists)
			if mg.HasColumn(&tables.TableTeam{}, "budget_id") {
				// Preflight: raw SQL below bypasses TableBudget.BeforeSave (which now
				// enforces exactly-one-of {TeamID, VirtualKeyID, ProviderConfigID}).
				// Fail fast if any team-referenced budget is already owned by a VK or
				// ProviderConfig, rather than silently producing a multi-owner row
				// that would later be rejected by the hook on its next update.
				var conflictCount int64
				if err := tx.Raw(`
					SELECT COUNT(*) FROM governance_budgets b
					WHERE (b.virtual_key_id IS NOT NULL OR b.provider_config_id IS NOT NULL)
					  AND EXISTS (SELECT 1 FROM governance_teams t WHERE t.budget_id = b.id)
				`).Scan(&conflictCount).Error; err != nil {
					return fmt.Errorf("failed to check for multi-owner team budget conflicts: %w", err)
				}
				if conflictCount > 0 {
					return fmt.Errorf(
						"cannot migrate team budgets: %d budget row(s) referenced by a team are already owned by a virtual key or provider config; resolve manually before re-running",
						conflictCount,
					)
				}

				if err := tx.Exec(`
					UPDATE governance_budgets SET team_id = (
						SELECT id FROM governance_teams
						WHERE governance_teams.budget_id = governance_budgets.id
					) WHERE team_id IS NULL AND EXISTS (
						SELECT 1 FROM governance_teams
						WHERE governance_teams.budget_id = governance_budgets.id
					)
				`).Error; err != nil {
					return fmt.Errorf("failed to backfill team budget team_id: %w", err)
				}

				// Drop legacy budget_id column BEFORE creating FK constraint.
				// On SQLite, ALTER TABLE RENAME propagates into FK references in other
				// tables. Dropping first prevents the FK on governance_budgets.team_id
				// from being corrupted by the table rebuild's rename step.
				if err := dropLegacyBudgetColumn(tx, "governance_teams"); err != nil {
					return err
				}
			}

			// Create FK constraint with CASCADE delete (defined on TableTeam.Budgets).
			// Must happen after legacy column drop so SQLite rename propagation
			// cannot corrupt this FK reference.
			if !mg.HasConstraint(&tables.TableTeam{}, "Budgets") {
				if err := mg.CreateConstraint(&tables.TableTeam{}, "Budgets"); err != nil {
					return fmt.Errorf("failed to create FK constraint for Team -> Budgets: %w", err)
				}
			}

			// Refresh config_hash for teams whose budgets just got linked. GenerateTeamHash
			// now includes sorted budget IDs, so hashes written by the earlier
			// migrationAddConfigHashColumn (which ran before budgets were associated)
			// are stale and would cause phantom drift on the next config.json sync.
			var teamsToRehash []tables.TableTeam
			if err := tx.Preload("Budgets").Find(&teamsToRehash).Error; err != nil {
				return fmt.Errorf("failed to fetch teams for hash refresh: %w", err)
			}
			logger.Info("[configstore] %s: processing %d teamsToRehash", migrationName, len(teamsToRehash))
			for _, team := range teamsToRehash {
				if len(team.Budgets) == 0 {
					continue // hash did not change; skip
				}
				hash, err := GenerateTeamHash(team)
				if err != nil {
					return fmt.Errorf("failed to generate hash for team %s: %w", team.ID, err)
				}
				if err := tx.Model(&tables.TableTeam{}).Where("id = ?", team.ID).Update("config_hash", hash).Error; err != nil {
					return fmt.Errorf("failed to update hash for team %s: %w", team.ID, err)
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableBudget{}, "TeamID"); err != nil {
				return fmt.Errorf("failed to drop team_id column from governance_budgets: %w", err)
			}
			return nil
		},
	}})
	// SQLite workaround — same reasoning as migrationAddMultiBudgetTables.
	if db.Dialector.Name() == "sqlite" {
		sqlDB, err := db.DB()
		if err != nil {
			return fmt.Errorf("failed to get underlying sql.DB: %w", err)
		}
		prevMaxOpenConns := sqlDB.Stats().MaxOpenConnections
		sqlDB.SetMaxOpenConns(1)
		defer sqlDB.SetMaxOpenConns(prevMaxOpenConns)

		if err := db.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
			return fmt.Errorf("failed to disable SQLite foreign keys: %w", err)
		}
		defer func() {
			if err := db.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
				log.Fatalf("[Migration] FATAL: failed to re-enable SQLite foreign keys: %v", err)
			}
		}()
	}
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_team_budgets_to_budgets_table migration: %s", err.Error())
	}
	return nil
}

// migrationAddModelConfigBudgetsFKConstraint adds the missing
// governance_budgets.model_config_id -> governance_model_configs(id)
// ON DELETE CASCADE foreign key (defined on TableModelConfig.Budgets via
// foreignKey:ModelConfigID;constraint:OnDelete:CASCADE).
//
// migrationAddMultiBudgetTables created the equivalent cascade FKs for VK- and
// ProviderConfig-owned budgets but never the model-config edge, and
// migrationAddBudgetModelConfigIDColumn added the column without a constraint.
// As a result deleting a model config never cascaded to its multi-budget rows,
// so they leaked (orphaned governance_budgets whose model_config_id points at a
// since-deleted config). This makes that cleanup structurally sound at the DB
// level, underneath the existing application-level cleanup in
// DeleteModelConfigsForScope/DeleteModelConfig. (The single owned rate-limit is
// intentionally left to application cleanup — rate-limits use the opposite
// owner.rate_limit_id convention, so reversing it just for model configs would
// introduce a one-off ownership split for a one-row-per-config leak surface.)
func migrationAddModelConfigBudgetsFKConstraint(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_model_config_budgets_fk_constraint"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()

			// Pre-clean: budgets whose model_config_id already references a
			// missing config would violate the FK we're about to add and block
			// its creation. They are exactly the rows the cascade would have
			// removed, so delete them — but only when nothing live still
			// references them via the legacy governance_model_configs.budget_id
			// (a NO ACTION FK), so this DELETE can't trip that constraint.
			if err := tx.Exec(`
				DELETE FROM governance_budgets
				WHERE model_config_id IS NOT NULL
				  AND model_config_id NOT IN (SELECT id FROM governance_model_configs)
				  AND id NOT IN (
				      SELECT budget_id FROM governance_model_configs WHERE budget_id IS NOT NULL
				  )
			`).Error; err != nil {
				return fmt.Errorf("failed to pre-clean orphaned model-config budgets: %w", err)
			}

			// Create the cascade FK (no-op if a prior fresh-DB migrate already made it).
			if !mg.HasConstraint(&tables.TableModelConfig{}, "Budgets") {
				if err := mg.CreateConstraint(&tables.TableModelConfig{}, "Budgets"); err != nil {
					return fmt.Errorf("failed to create FK constraint for ModelConfig -> Budgets: %w", err)
				}
			}
			return nil
		},
		// Partially non-rollbackable: dropping the FK restores the previous
		// schema, but the orphaned governance_budgets rows removed by the
		// pre-clean are gone permanently. That loss is intentional — they were
		// exactly the dead rows the missing cascade had leaked — so the schema
		// rollback below is still provided rather than hard-failing.
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()
			if mg.HasConstraint(&tables.TableModelConfig{}, "Budgets") {
				if err := mg.DropConstraint(&tables.TableModelConfig{}, "Budgets"); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	// SQLite workaround — same reasoning as migrationAddMultiBudgetTables:
	// CreateConstraint rebuilds governance_budgets via DROP+RENAME inside a
	// transaction, which fails while other tables hold FKs into it and
	// foreign_keys is ON. PRAGMA foreign_keys can't change inside a transaction,
	// so disable it (pinned to one connection) before the migrator opens its tx.
	// Postgres supports ALTER TABLE ADD CONSTRAINT natively and needs none of this.
	if db.Dialector.Name() == "sqlite" {
		sqlDB, err := db.DB()
		if err != nil {
			return fmt.Errorf("failed to get underlying sql.DB: %w", err)
		}
		prevMaxOpenConns := sqlDB.Stats().MaxOpenConnections
		sqlDB.SetMaxOpenConns(1)
		defer sqlDB.SetMaxOpenConns(prevMaxOpenConns)

		if err := db.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
			return fmt.Errorf("failed to disable SQLite foreign keys: %w", err)
		}
		defer func() {
			if err := db.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
				log.Fatalf("[Migration] FATAL: failed to re-enable SQLite foreign keys: %v", err)
			}
		}()
	}
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_model_config_budgets_fk_constraint migration: %s", err.Error())
	}
	return nil
}

// migrationAddAllowPerRequestContentStorageOverrideColumn adds the allow_per_request_content_storage_override column to config_client.
func migrationAddAllowPerRequestContentStorageOverrideColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_allow_per_request_content_storage_override_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "AllowPerRequestContentStorageOverride"); err != nil {
				return fmt.Errorf("failed to add allow_per_request_content_storage_override column: %w", err)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "allow_per_request_content_storage_override"); err != nil {
				return fmt.Errorf("failed to drop allow_per_request_content_storage_override column: %w", err)
			}

			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running allow_per_request_content_storage_override migration: %s", err.Error())
	}
	return nil
}

// migrationAddRetainContentInObjectStorageColumn adds the retain_content_in_object_storage column to config_client.
func migrationAddRetainContentInObjectStorageColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_retain_content_in_object_storage_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "RetainContentInObjectStorage"); err != nil {
				return fmt.Errorf("failed to add retain_content_in_object_storage column: %w", err)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "retain_content_in_object_storage"); err != nil {
				return fmt.Errorf("failed to drop retain_content_in_object_storage column: %w", err)
			}

			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running retain_content_in_object_storage migration: %s", err.Error())
	}
	return nil
}

// migrationAddAllowPerRequestRawOverrideColumn adds the allow_per_request_raw_override column to config_client.
func migrationAddAllowPerRequestRawOverrideColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_allow_per_request_raw_override_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "AllowPerRequestRawOverride"); err != nil {
				return fmt.Errorf("failed to add allow_per_request_raw_override column: %w", err)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "allow_per_request_raw_override"); err != nil {
				return fmt.Errorf("failed to drop allow_per_request_raw_override column: %w", err)
			}

			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running allow_per_request_raw_override migration: %s", err.Error())
	}
	return nil
}

// migrationAddPriorityTierPricingColumns adds pricing columns for the 272k token tier
// and the 200k priority variants.
func migrationAddPriorityTierPricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_priority_tier_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			columns := []string{
				"input_cost_per_token_above_272k_tokens",
				"input_cost_per_token_above_272k_tokens_priority",
				"output_cost_per_token_above_272k_tokens",
				"output_cost_per_token_above_272k_tokens_priority",
				"cache_read_input_token_cost_above_272k_tokens",
				"cache_read_input_token_cost_above_272k_tokens_priority",
				"input_cost_per_token_above_200k_tokens_priority",
				"output_cost_per_token_above_200k_tokens_priority",
				"cache_read_input_token_cost_above_200k_tokens_priority",
			}

			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			columns := []string{
				"input_cost_per_token_above_272k_tokens",
				"input_cost_per_token_above_272k_tokens_priority",
				"output_cost_per_token_above_272k_tokens",
				"output_cost_per_token_above_272k_tokens_priority",
				"cache_read_input_token_cost_above_272k_tokens",
				"cache_read_input_token_cost_above_272k_tokens_priority",
				"input_cost_per_token_above_200k_tokens_priority",
				"output_cost_per_token_above_200k_tokens_priority",
				"cache_read_input_token_cost_above_200k_tokens_priority",
			}

			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running priority tier pricing columns migration: %s", err.Error())
	}
	return nil
}

// migrationAddFlexTierPricingColumns adds pricing columns for the flex service tier
func migrationAddFlexTierPricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_flex_tier_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			columns := []string{
				"input_cost_per_token_flex",
				"output_cost_per_token_flex",
				"cache_read_input_token_cost_flex",
			}

			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			columns := []string{
				"input_cost_per_token_flex",
				"output_cost_per_token_flex",
				"cache_read_input_token_cost_flex",
			}

			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running flex tier pricing columns migration: %s", err.Error())
	}
	return nil
}

// migrationAddFastModePricingColumns adds pricing columns for Anthropic fast mode
// (research preview, speed:"fast" on Opus 4.6/4.7/4.8).
func migrationAddFastModePricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_fast_mode_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			columns := []string{
				"input_cost_per_token_fast",
				"output_cost_per_token_fast",
			}

			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			columns := []string{
				"input_cost_per_token_fast",
				"output_cost_per_token_fast",
			}

			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running fast mode pricing columns migration: %s", err.Error())
	}
	return nil
}

// migrationAddFastModeCachePricingColumns adds fast-mode cache pricing columns
// for Anthropic (speed:"fast"). Caching multipliers stack on the fast base input
// rate, so cache tokens need dedicated fast rates instead of the standard ones.
func migrationAddFastModeCachePricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_fast_mode_cache_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	columns := []string{
		"cache_creation_input_token_cost_fast",
		"cache_creation_input_token_cost_above_1hr_fast",
		"cache_read_input_token_cost_fast",
	}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running fast mode cache pricing columns migration: %s", err.Error())
	}
	return nil
}

// migrationAddInferenceGeoMultiplierColumn adds the inference_geo_us_multiplier
// column for Anthropic data residency (inference_geo:"us" applies a 1.1x
// multiplier stacking on top of all token/cache costs).
func migrationAddInferenceGeoMultiplierColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_inference_geo_multiplier_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	columns := []string{
		"inference_geo_us_multiplier",
	}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running inference geo multiplier column migration: %s", err.Error())
	}
	return nil
}

// migrationAddFlexAndCacheCreation272kPricingColumns adds the flex 272k-tier
// rates and the OpenAI cache-write (cache-creation) tiered rates introduced with
// gpt-5.6 (flex, priority, and the 272k context tier).
func migrationAddFlexAndCacheCreation272kPricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_flex_and_cache_creation_272k_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	columns := []string{
		"input_cost_per_token_flex_above_272k_tokens",
		"output_cost_per_token_flex_above_272k_tokens",
		"cache_read_input_token_cost_flex_above_272k_tokens",
		"cache_creation_input_token_cost_above_272k_tokens",
		"cache_creation_input_token_cost_flex",
		"cache_creation_input_token_cost_flex_above_272k_tokens",
		"cache_creation_input_token_cost_priority",
	}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running flex and cache creation 272k pricing columns migration: %s", err.Error())
	}
	return nil
}

// migrationAddWhitelistedRoutesJSONColumn adds the whitelisted_routes_json column to the config_client table
func migrationAddWhitelistedRoutesJSONColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_whitelisted_routes_json_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "WhitelistedRoutesJSON"); err != nil {
				return fmt.Errorf("failed to add whitelisted_routes_json column: %w", err)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "whitelisted_routes_json"); err != nil {
				return fmt.Errorf("failed to drop whitelisted_routes_json column: %w", err)
			}

			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running whitelisted_routes_json migration: %s", err.Error())
	}
	return nil
}

// migrationReplaceEnableLiteLLMWithCompatColumns replaces the single enable_litellm_fallbacks
// boolean with compat feature columns. If enable_litellm_fallbacks was true,
// only convert_text_to_chat is set to true (preserving the original behavior).
func migrationReplaceEnableLiteLLMWithCompatColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "replace_enable_litellm_with_compat_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mig := tx.Migrator()

			// Add new columns
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "compat_convert_text_to_chat"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "compat_convert_chat_to_responses"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "compat_should_drop_params"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "compat_should_convert_params"); err != nil {
				return err
			}

			if err := tx.Exec("UPDATE config_client SET compat_should_convert_params = FALSE").Error; err != nil {
				return err
			}

			// Migrate data: if enable_litellm_fallbacks was true, set convert_text_to_chat = true
			if mig.HasColumn(&tables.TableClientConfig{}, "enable_litellm_fallbacks") {
				if err := tx.Exec("UPDATE config_client SET compat_convert_text_to_chat = enable_litellm_fallbacks").Error; err != nil {
					return err
				}
				if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "enable_litellm_fallbacks"); err != nil {
					return err
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mig := tx.Migrator()
			if tx.Migrator().HasColumn(&tables.TableClientConfig{}, "enable_litellm_fallbacks") {
				if err := tx.Exec("ALTER TABLE config_client ADD COLUMN enable_litellm_fallbacks BOOLEAN DEFAULT FALSE").Error; err != nil {
					return err
				}
			}
			if mig.HasColumn(&tables.TableClientConfig{}, "compat_convert_text_to_chat") {
				if err := tx.Exec("UPDATE config_client SET enable_litellm_fallbacks = COALESCE(compat_convert_text_to_chat, FALSE)").Error; err != nil {
					return err
				}
			}
			for _, col := range []string{
				"compat_convert_text_to_chat",
				"compat_convert_chat_to_responses",
				"compat_should_drop_params",
				"compat_should_convert_params",
			} {
				if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, col); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running replace_enable_litellm_with_compat_columns migration: %s", err.Error())
	}
	return nil
}

// migrationDefaultCompatShouldConvertParamsFalse ensures existing deployments
// converge to the new default for compat_should_convert_params. The earlier
// compat migration may already be marked as applied, so changing its body is not
// sufficient for installed databases.
func migrationDefaultCompatShouldConvertParamsFalse(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "default_compat_should_convert_params_false"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mig := tx.Migrator()

			if !mig.HasColumn(&tables.TableClientConfig{}, "compat_should_convert_params") {
				return nil
			}

			if err := tx.Exec("UPDATE config_client SET compat_should_convert_params = FALSE").Error; err != nil {
				return err
			}

			if err := mig.AlterColumn(&tables.TableClientConfig{}, "CompatShouldConvertParams"); err != nil {
				return fmt.Errorf("failed to alter compat_should_convert_params default: %w", err)
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mig := tx.Migrator()

			if !mig.HasColumn(&tables.TableClientConfig{}, "compat_should_convert_params") {
				return nil
			}

			switch tx.Dialector.Name() {
			case "postgres":
				if err := tx.Exec("ALTER TABLE config_client ALTER COLUMN compat_should_convert_params SET DEFAULT FALSE").Error; err != nil {
					return err
				}
			}

			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running default_compat_should_convert_params_false migration: %s", err.Error())
	}
	return nil
}

// migrationAddModelPricingUniqueIndex ensures the composite unique index (model, provider, mode)
// exists on governance_model_pricing so that atomic ON CONFLICT upserts work correctly.
func migrationAddModelPricingUniqueIndex(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_model_pricing_unique_index"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()

			// Remove duplicate rows before creating the unique index.
			// The old find-then-insert path could have produced duplicates on
			// multinode deployments, and CREATE UNIQUE INDEX will fail on a table
			// that still contains them. Keep the row with the lowest ID for each
			// (model, provider, mode) combination.
			result := tx.Exec(`
				DELETE FROM governance_model_pricing
				WHERE id NOT IN (
					SELECT MIN(id)
					FROM governance_model_pricing
					GROUP BY model, provider, mode
				)
			`)
			if result.Error != nil {
				return fmt.Errorf("failed to deduplicate model pricing rows: %w", result.Error)
			}
			if result.RowsAffected > 0 {
				logger.Info("[migration] removed %d duplicate row(s) from governance_model_pricing before creating unique index", result.RowsAffected)
			}

			if !mg.HasIndex(&tables.TableModelPricing{}, "idx_model_provider_mode") {
				logger.Info("[configstore] %s: creating index idx_model_provider_mode on TableModelPricing", migrationName)
				if err := mg.CreateIndex(&tables.TableModelPricing{}, "idx_model_provider_mode"); err != nil {
					return fmt.Errorf("failed to create unique index idx_model_provider_mode: %w", err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()
			if mg.HasIndex(&tables.TableModelPricing{}, "idx_model_provider_mode") {
				logger.Info("[configstore] %s: dropping index idx_model_provider_mode on TableModelPricing", migrationName)
				if err := mg.DropIndex(&tables.TableModelPricing{}, "idx_model_provider_mode"); err != nil {
					return fmt.Errorf("failed to drop unique index idx_model_provider_mode: %w", err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_model_pricing_unique_index migration: %s", err.Error())
	}
	return nil
}

// migrationNormalizeOtelTraceType rewrites the legacy OTEL plugin trace_type value "otel" to "genai_extension".
// No-op if the plugin row is missing or trace_type is already correct.
func migrationNormalizeOtelTraceType(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "normalize_otel_trace_type"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			var plugin tables.TablePlugin
			err := tx.Where("name = ?", "otel").First(&plugin).Error
			if err != nil {
				if err == gorm.ErrRecordNotFound {
					return nil
				}
				return fmt.Errorf("failed to load otel plugin row: %w", err)
			}

			cfgMap, ok := plugin.Config.(map[string]any)
			if !ok || len(cfgMap) == 0 {
				return nil
			}
			if tt, _ := cfgMap["trace_type"].(string); tt != "otel" {
				return nil
			}

			cfgMap["trace_type"] = "genai_extension"
			plugin.Config = cfgMap
			plugin.ConfigJSON = ""
			plugin.EncryptionStatus = tables.EncryptionStatusPlainText

			if err := tx.Save(&plugin).Error; err != nil {
				return fmt.Errorf("failed to save normalized otel config: %w", err)
			}
			logger.Info("[Migration] Normalized otel trace_type 'otel' to 'genai_extension'")
			return nil
		},
		Rollback: func(tx *gorm.DB) error { return nil },
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running normalize_otel_trace_type migration: %s", err.Error())
	}
	return nil
}

// migrateCalendarAlignedToBudgetsAndRateLimitsTable
func migrateCalendarAlignedToBudgetsAndRateLimitsTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "migrate_calendar_aligned"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			// Adding columns first
			budgetsHasCol, err := hasColumn(tx, "governance_budgets", "calendar_aligned")
			if err != nil {
				return fmt.Errorf("failed to introspect governance_budgets for calendar_aligned: %w", err)
			}
			if !budgetsHasCol {
				if err := tx.Exec(`ALTER TABLE governance_budgets ADD COLUMN calendar_aligned BOOLEAN DEFAULT FALSE`).Error; err != nil {
					return fmt.Errorf("failed to add calendar_aligned column to budgets: %w", err)
				}
			}
			// Adding columns first
			rateLimitsHasCol, err := hasColumn(tx, "governance_rate_limits", "calendar_aligned")
			if err != nil {
				return fmt.Errorf("failed to introspect governance_rate_limits for calendar_aligned: %w", err)
			}
			if !rateLimitsHasCol {
				if err := tx.Exec(`ALTER TABLE governance_rate_limits ADD COLUMN calendar_aligned BOOLEAN DEFAULT FALSE`).Error; err != nil {
					return fmt.Errorf("failed to add calendar_aligned column to rate_limits: %w", err)
				}
			}
			// Prefill calendar_aligned for existing budgets and rate_limits attached to virtual keys.
			// Use subquery-based raw SQL (compatible with both PostgreSQL and SQLite) to avoid
			// "cached plan must not change result type" (SQLSTATE 0A000): earlier migrations in
			// the same run added columns to these tables, invalidating pgx's prepared-statement cache.
			if err := tx.Exec(`
				UPDATE governance_rate_limits
				SET calendar_aligned = true
				WHERE id IN (
					SELECT rate_limit_id FROM governance_virtual_keys
					WHERE calendar_aligned = true AND rate_limit_id IS NOT NULL
				)
			`).Error; err != nil {
				return fmt.Errorf("failed to propagate calendar_aligned to rate limits: %w", err)
			}
			if err := tx.Exec(`
				UPDATE governance_budgets
				SET calendar_aligned = true
				WHERE virtual_key_id IN (
					SELECT id FROM governance_virtual_keys WHERE calendar_aligned = true
				)
			`).Error; err != nil {
				return fmt.Errorf("failed to propagate calendar_aligned to budgets: %w", err)
			}
			logger.Info("[Migration] Prefilled calendar_aligned field for existing budgets and rate limits")
			return nil
		},
		Rollback: func(tx *gorm.DB) error { return nil },
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running migrate_calendar_aligned migration: %s", err.Error())
	}
	return nil
}

func migrationAddOCRPricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_ocr_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			columns := []string{
				"ocr_cost_per_page",
				"annotation_cost_per_page",
			}
			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			columns := []string{
				"ocr_cost_per_page",
				"annotation_cost_per_page",
			}
			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_ocr_pricing_columns migration: %s", err.Error())
	}
	return nil
}

func migrationAddCostPerRequestPricingColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_cost_per_request_pricing_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, "cost_per_request"); err != nil {
				return fmt.Errorf("failed to add column cost_per_request: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, "cost_per_request"); err != nil {
				return fmt.Errorf("failed to drop column cost_per_request: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_cost_per_request_pricing_column migration: %s", err.Error())
	}
	return nil
}

// migrationUniqueTeamNames deduplicates governance_teams.name and adds a unique
// index. Duplicate rows (same name, different ID) have a short UUID suffix
// appended so no data is lost. The struct tag uniqueIndex makes GORM enforce
// this on new rows going forward.
func migrationUniqueTeamNames(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "gov_unique_team_names"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	return RunSingleMigration(ctx, nil, db, logger, &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// Find all names that appear more than once.
			type dupRow struct {
				Name string
			}
			var dups []dupRow
			if err := tx.Raw(`
				SELECT name FROM governance_teams
				GROUP BY name HAVING COUNT(*) > 1
			`).Scan(&dups).Error; err != nil {
				return fmt.Errorf("find duplicate team names: %w", err)
			}

			logger.Info("[configstore] %s: processing %d dups", migrationName, len(dups))
			for _, d := range dups {
				// oldest row keeps the original name
				type row struct {
					ID string
				}
				var rows []row
				if err := tx.Raw(`
					SELECT id FROM governance_teams
					WHERE name = ?
					ORDER BY created_at ASC, id ASC
				`, d.Name).Scan(&rows).Error; err != nil {
					return fmt.Errorf("fetch duplicates for %q: %w", d.Name, err)
				}
				for _, r := range rows[1:] {
					newName := d.Name + "-" + r.ID[:8]
					if err := tx.Exec(`UPDATE governance_teams SET name = ? WHERE id = ?`, newName, r.ID).Error; err != nil {
						return fmt.Errorf("rename duplicate team %s: %w", r.ID, err)
					}
				}
			}

			// Add the unique index. Skip if it already exists.
			if !tx.Migrator().HasIndex(&tables.TableTeam{}, "idx_governance_teams_name") {
				logger.Info("[configstore] %s: creating index Name on TableTeam", migrationName)
				if err := tx.Migrator().CreateIndex(&tables.TableTeam{}, "Name"); err != nil {
					return fmt.Errorf("create unique index on governance_teams.name: %w", err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			logger.Info("[configstore] %s: dropping index idx_governance_teams_name on TableTeam", migrationName)
			_ = tx.Migrator().DropIndex(&tables.TableTeam{}, "idx_governance_teams_name")
			return nil
		},
	})
}

// migrationAddSkillsExternalClientURLColumn adds the public marketplace URL setting.
// DDL runs separately from the backfill so its exclusive lock is released before updating rows.
func migrationAddSkillsExternalClientURLColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	return RunSingleMigration(ctx, nil, db, logger, &migrator.Migration{
		ID: "add_skills_external_client_url_column",
		Migrate: func(tx *gorm.DB) error {
			return addColumnIfNotExists(tx.WithContext(ctx), logger, &tables.TableClientConfig{}, "SkillsExternalClientURL")
		},
		Rollback: func(tx *gorm.DB) error {
			return fmt.Errorf("add_skills_external_client_url_column is non-rollbackable: dropping it would discard marketplace configuration")
		},
	})
}

// migrationDropLegacyCalendarAlignedColumns drops the legacy calendar_aligned
// columns from governance_budgets and governance_rate_limits. Calendar
// alignment is now a VK-only setting (governance_virtual_keys.calendar_aligned);
// budget and rate-limit reset logic derives the value from the owning VK at
// reset time. The columns were re-added by migrate_calendar_aligned after
// add_multi_budget_tables dropped governance_budgets.calendar_aligned, so any
// DB that ran both still has them — this migration cleans them up.
func migrationDropLegacyCalendarAlignedColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "drop_legacy_calendar_aligned_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			// Use a raw `ALTER TABLE ... DROP COLUMN` (dialect-aware via dropColumnSQL) instead of
			// GORM's Migrator.DropColumn, which on SQLite does a full table rebuild that aborts on
			// pre-existing FK violations; since these unconstrained boolean columns are safe to
			// leave behind, we log a warning rather than fail boot.
			if err := tx.Exec(dropColumnSQL(tx, "governance_budgets", "calendar_aligned")).Error; err != nil {
				logger.Info("[Migration] warning: could not drop legacy calendar_aligned column from governance_budgets: %v", err)
			}
			if err := tx.Exec(dropColumnSQL(tx, "governance_rate_limits", "calendar_aligned")).Error; err != nil {
				logger.Info("[Migration] warning: could not drop legacy calendar_aligned column from governance_rate_limits: %v", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error { return nil },
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running drop_legacy_calendar_aligned_columns migration: %s", err.Error())
	}
	return nil
}

// migrationAddTeamCalendarAlignedColumn adds calendar_aligned to governance_teams so
// team-level calendar alignment (governing all team budgets and the team rate limit)
// can be persisted.
func migrationAddTeamCalendarAlignedColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_team_calendar_aligned_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mig := tx.Migrator()
			if err := addColumnIfNotExists(tx, logger, &tables.TableTeam{}, "CalendarAligned"); err != nil {
				return fmt.Errorf("failed to add calendar_aligned column to governance_teams: %w", err)
			}
			// Backfill from legacy per-budget / per-rate-limit flags before the
			// drop migration removes them. Any team-owned budget with
			// calendar_aligned=true, or a team rate-limit with calendar_aligned=true,
			// promotes the team to calendar-aligned so behavior is preserved across upgrade.
			if mig.HasColumn(&tables.TableBudget{}, "calendar_aligned") {
				if err := tx.Exec(`
					UPDATE governance_teams
					SET calendar_aligned = TRUE
					WHERE EXISTS (
						SELECT 1 FROM governance_budgets b
						WHERE b.team_id = governance_teams.id AND b.calendar_aligned = TRUE
					)
				`).Error; err != nil {
					return fmt.Errorf("failed to backfill team calendar_aligned from budgets: %w", err)
				}
			}
			if mig.HasColumn(&tables.TableRateLimit{}, "calendar_aligned") {
				if err := tx.Exec(`
					UPDATE governance_teams
					SET calendar_aligned = TRUE
					WHERE rate_limit_id IN (
						SELECT id FROM governance_rate_limits WHERE calendar_aligned = TRUE
					)
				`).Error; err != nil {
					return fmt.Errorf("failed to backfill team calendar_aligned from rate limits: %w", err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableTeam{}, "calendar_aligned")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_team_calendar_aligned_column migration: %s", err.Error())
	}
	return nil
}

// migrationAddModelParametersURLColumn adds the model_parameters_url column to framework_configs.
func migrationAddModelParametersURLColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_model_parameters_url_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableFrameworkConfig{}, "ModelParametersURL"); err != nil {
				return fmt.Errorf("failed to add model_parameters_url column to framework_configs: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableFrameworkConfig{}, "model_parameters_url")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_model_parameters_url_column migration: %s", err.Error())
	}
	return nil
}

// migrationAddFrameworkConfigHashColumn adds the config_hash column to framework_configs
// so that file-vs-DB precedence can be determined via hash comparison on restart.
func migrationAddFrameworkConfigHashColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_framework_config_hash_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableFrameworkConfig{}, "ConfigHash"); err != nil {
				return fmt.Errorf("failed to add config_hash column to framework_configs: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableFrameworkConfig{}, "config_hash")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_framework_config_hash_column migration: %s", err.Error())
	}
	return nil
}

// migrationAddVirtualKeyBlacklistedModelsColumn adds the blacklisted_models JSON column
// to governance_virtual_key_provider_configs, matching the provider-key blacklist pattern.
// This migration performs the DDL only. The data backfill and config_hash recompute are
// done by migrationBackfillVirtualKeyBlacklistedModels so the ALTER's ACCESS EXCLUSIVE
// lock is not held across the backfill SELECT + UPDATE, which can deadlock Postgres.
func migrationAddVirtualKeyBlacklistedModelsColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_vk_provider_config_blacklisted_models_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableVirtualKeyProviderConfig{}, "blacklisted_models"); err != nil {
				return fmt.Errorf("failed to add blacklisted_models column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_vk_provider_config_blacklisted_models_column migration: %s", err.Error())
	}
	return nil
}

// migrationBackfillVirtualKeyBlacklistedModels backfills empty JSON arrays into the
// blacklisted_models column added by migrationAddVirtualKeyBlacklistedModelsColumn and
// recomputes config_hash for every virtual key so they do not appear stale after upgrade.
// It is a separate migration from the column-add so the DDL's ACCESS EXCLUSIVE lock is
// never held across this SELECT + UPDATE backfill.
func migrationBackfillVirtualKeyBlacklistedModels(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "backfill_vk_provider_config_blacklisted_models"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// Backfill empty arrays for existing rows. Idempotent via the WHERE clause.
			if err := tx.Exec("UPDATE governance_virtual_key_provider_configs SET blacklisted_models = '[]' WHERE blacklisted_models IS NULL OR blacklisted_models = ''").Error; err != nil {
				return fmt.Errorf("failed to backfill blacklisted_models: %w", err)
			}

			// Recompute config_hash for all VKs affected by the backfill so they do
			// not appear stale after upgrade. The hash is deterministic, so this is
			// safe to re-run.
			var virtualKeys []tables.TableVirtualKey
			if err := tx.
				Preload("ProviderConfigs").
				Preload("ProviderConfigs.Keys").
				Find(&virtualKeys).Error; err != nil {
				return fmt.Errorf("failed to fetch virtual keys for hash recomputation: %w", err)
			}
			logger.Info("[configstore] %s: processing %d virtualKeys", migrationName, len(virtualKeys))
			for _, vk := range virtualKeys {
				newHash, err := GenerateVirtualKeyHash(vk)
				if err != nil {
					return fmt.Errorf("failed to generate hash for VK %s: %w", vk.ID, err)
				}
				if err := tx.Model(&tables.TableVirtualKey{}).
					Where("id = ?", vk.ID).
					Update("config_hash", newHash).Error; err != nil {
					return fmt.Errorf("failed to update config_hash for VK %s: %w", vk.ID, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running backfill_vk_provider_config_blacklisted_models migration: %s", err.Error())
	}
	return nil
}

// migrationAddVKAccessProfileIDColumn adds the access_profile_id column to governance_virtual_keys.
func migrationAddVKAccessProfileIDColumn(_ context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_vk_access_profile_id_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_vk_access_profile_id_column migration: %s", err.Error())
	}
	return nil
}

// migrationDropVKAccessProfileIDColumn drops the access_profile_id column and its
// index from governance_virtual_keys, reverting migrationAddVKAccessProfileIDColumn.
func migrationDropVKAccessProfileIDColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "drop_vk_access_profile_id_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			// DROP INDEX IF EXISTS avoids aborting the Postgres transaction when
			// the index was never created (e.g. fresh installs where the add
			// migration ran as a no-op).
			if err := tx.Exec("DROP INDEX IF EXISTS idx_governance_virtual_keys_access_profile_id").Error; err != nil {
				return fmt.Errorf("failed to drop index idx_governance_virtual_keys_access_profile_id: %w", err)
			}
			// Use raw ALTER TABLE instead of GORM's DropColumn: GORM's SQLite
			// DropColumn does a full table rebuild inside a transaction where
			// PRAGMA foreign_keys cannot be disabled, causing FK checks on
			// unrelated columns to fail against test data.
			// ALTER TABLE DROP COLUMN (SQLite 3.35+) modifies the schema in place.
			if tx.Migrator().HasColumn("governance_virtual_keys", "access_profile_id") {
				if err := tx.Exec("ALTER TABLE governance_virtual_keys DROP COLUMN access_profile_id").Error; err != nil {
					return fmt.Errorf("failed to drop access_profile_id column from governance_virtual_keys: %w", err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableVirtualKeyProviderConfig{}, "blacklisted_models"); err != nil {
				return fmt.Errorf("failed to drop blacklisted_models column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running drop_vk_access_profile_id_column migration: %s", err.Error())
	}
	return nil
}

// migrationAddCreatedByUserIDColumnForVirtualKeys adds the created_by_user_id column to the governance_virtual_keys table.
func migrationAddCreatedByUserIDColumnForVirtualKeys(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_created_by_user_id_column_for_virtual_keys"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if !tx.Migrator().HasColumn(&tables.TableVirtualKey{}, "created_by_user_id") {
				if err := tx.Exec("ALTER TABLE governance_virtual_keys ADD COLUMN created_by_user_id VARCHAR(255)").Error; err != nil {
					return fmt.Errorf("failed to add created_by_user_id column to governance_virtual_keys: %w", err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if tx.Migrator().HasColumn(&tables.TableVirtualKey{}, "created_by_user_id") {
				if err := tx.Exec("ALTER TABLE governance_virtual_keys DROP COLUMN created_by_user_id").Error; err != nil {
					return fmt.Errorf("failed to drop created_by_user_id column from governance_virtual_keys: %w", err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_created_by_user_id_column_for_virtual_keys migration: %s", err.Error())
	}
	return nil
}

// migrationDropAzureAPIVersionColumn adds the created_by_user_id column to the governance_virtual_keys table
func migrationDropAzureAPIVersionColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "drop_azure_api_version_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if tx.Migrator().HasColumn(&tables.TableKey{}, "azure_api_version") {
				if err := tx.Exec("ALTER TABLE config_keys DROP COLUMN azure_api_version").Error; err != nil {
					return fmt.Errorf("failed to drop azure_api_version column: %w", err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if !tx.Migrator().HasColumn(&tables.TableKey{}, "azure_api_version") {
				if err := tx.Exec("ALTER TABLE config_keys ADD COLUMN azure_api_version TEXT").Error; err != nil {
					return fmt.Errorf("failed to re-add azure_api_version column: %w", err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running drop_azure_api_version_column migration: %s", err.Error())
	}
	return nil
}

// migrationReAddAllowDirectKeysColumn re-adds the allow_direct_keys column to config_client.
// The column was originally added then dropped in v1.5.0 when the direct key bypass feature
// was removed. It is re-added here as the feature is being restored with header-gated access.
func migrationReAddAllowDirectKeysColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "re_add_allow_direct_keys_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if !tx.Migrator().HasColumn(&tables.TableClientConfig{}, "allow_direct_keys") {
				if err := tx.Exec("ALTER TABLE config_client ADD COLUMN allow_direct_keys BOOLEAN DEFAULT FALSE").Error; err != nil {
					return fmt.Errorf("failed to re-add allow_direct_keys column: %w", err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "allow_direct_keys"); err != nil {
				return fmt.Errorf("failed to drop allow_direct_keys column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running re_add_allow_direct_keys_column migration: %s", err.Error())
	}
	return nil
}

// migrationAddAdditionalAttributesToPricing adds the additional_attributes
// column to governance_model_pricing. The column stores editorial per-model
// metadata (e.g. description) as a JSON blob. It is intentionally excluded
// from UpsertModelPrices' update list so the 24-hour pricing sync never
// overwrites it.
func migrationAddAdditionalAttributesToPricing(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_additional_attributes_to_pricing"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, "AdditionalAttributesJSON"); err != nil {
				return fmt.Errorf("failed to add additional_attributes column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, "AdditionalAttributesJSON"); err != nil {
				return fmt.Errorf("failed to drop additional_attributes column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_additional_attributes_to_pricing migration: %s", err.Error())
	}
	return nil
}

// migrationAddModelPricingIsDeprecatedColumn adds is_deprecated to
// governance_model_pricing so synced datasheets can mark models that should
// remain listable but are no longer recommended for new use.
func migrationAddModelPricingIsDeprecatedColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_model_pricing_is_deprecated_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, "IsDeprecated"); err != nil {
				return fmt.Errorf("failed to add is_deprecated column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, "IsDeprecated"); err != nil {
				return fmt.Errorf("failed to drop is_deprecated column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_model_pricing_is_deprecated_column migration: %s", err.Error())
	}
	return nil
}

// migrationAddCustomerCalendarAlignedColumn adds calendar_aligned to governance_customers
// so customer-level calendar alignment can be persisted. No backfill is needed: the
// legacy per-budget/per-rate-limit calendar_aligned columns were dropped by
// drop_legacy_calendar_aligned_columns before this migration runs, and calendar_aligned
// never worked for customers, so there is no prior behavior to preserve.
func migrationAddCustomerCalendarAlignedColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_customer_calendar_aligned_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableCustomer{}, "CalendarAligned"); err != nil {
				return fmt.Errorf("failed to add calendar_aligned column to governance_customers: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableCustomer{}, "calendar_aligned")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_customer_calendar_aligned_column migration: %s", err.Error())
	}
	return nil
}

// migrationAddCustomerBudgetsToBudgetsTable pivots customer budgets from a single-FK on
// governance_customers.budget_id to multi-budget ownership via governance_budgets.customer_id,
// mirroring how team budgets were restructured in migrationAddTeamBudgetsToBudgetsTable.
func migrationAddCustomerBudgetsToBudgetsTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_customer_budgets_to_budgets_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()

			// Add customer_id FK column on governance_budgets.
			if err := addColumnIfNotExists(tx, logger, &tables.TableBudget{}, "CustomerID"); err != nil {
				return fmt.Errorf("failed to add customer_id column to governance_budgets: %w", err)
			}

			// Create index on the new FK column (AddColumn doesn't create indexes from struct tags).
			if !mg.HasIndex(&tables.TableBudget{}, "idx_governance_budgets_customer_id") {
				logger.Info("[configstore] %s: creating index CustomerID on TableBudget", migrationName)
				if err := mg.CreateIndex(&tables.TableBudget{}, "CustomerID"); err != nil {
					return fmt.Errorf("failed to create index on governance_budgets.customer_id: %w", err)
				}
			}

			// Backfill: set customer_id from legacy governance_customers.budget_id (if column still exists).
			// The column is intentionally kept; a future migration can drop it once all instances
			// have migrated and the column is confirmed unused.
			legacyExists, err := hasColumn(tx, "governance_customers", "budget_id")
			if err != nil {
				return fmt.Errorf("failed to introspect governance_customers for budget_id: %w", err)
			}
			if legacyExists {
				// Preflight: fail if any customer-referenced budget is already owned by another entity.
				var conflictCount int64
				if err := tx.Raw(`
					SELECT COUNT(*) FROM governance_budgets b
					WHERE (b.virtual_key_id IS NOT NULL OR b.provider_config_id IS NOT NULL OR b.team_id IS NOT NULL OR b.model_config_id IS NOT NULL)
					  AND EXISTS (SELECT 1 FROM governance_customers c WHERE c.budget_id = b.id)
				`).Scan(&conflictCount).Error; err != nil {
					return fmt.Errorf("failed to check for multi-owner customer budget conflicts: %w", err)
				}
				if conflictCount > 0 {
					return fmt.Errorf(
						"cannot migrate customer budgets: %d budget row(s) referenced by a customer are already owned by another entity; resolve manually before re-running",
						conflictCount,
					)
				}

				if err := tx.Exec(`
					UPDATE governance_budgets SET customer_id = (
						SELECT id FROM governance_customers
						WHERE governance_customers.budget_id = governance_budgets.id
					) WHERE customer_id IS NULL AND EXISTS (
						SELECT 1 FROM governance_customers
						WHERE governance_customers.budget_id = governance_budgets.id
					)
				`).Error; err != nil {
					return fmt.Errorf("failed to backfill customer budget customer_id: %w", err)
				}
			}

			// Create FK constraint with CASCADE delete (defined on TableCustomer.Budgets).
			if !mg.HasConstraint(&tables.TableCustomer{}, "Budgets") {
				if err := mg.CreateConstraint(&tables.TableCustomer{}, "Budgets"); err != nil {
					return fmt.Errorf("failed to create FK constraint for Customer -> Budgets: %w", err)
				}
			}

			// Refresh config_hash for customers whose budgets just got linked. GenerateCustomerHash
			// now includes sorted budget IDs, so hashes written before multi-budget support are stale.
			var customersToRehash []tables.TableCustomer
			if err := tx.Preload("Budgets").Find(&customersToRehash).Error; err != nil {
				return fmt.Errorf("failed to fetch customers for hash refresh: %w", err)
			}
			logger.Info("[configstore] %s: processing %d customersToRehash", migrationName, len(customersToRehash))
			for _, c := range customersToRehash {
				if len(c.Budgets) == 0 {
					continue
				}
				hash, err := GenerateCustomerHash(c)
				if err != nil {
					return fmt.Errorf("failed to generate hash for customer %s: %w", c.ID, err)
				}
				if err := tx.Model(&tables.TableCustomer{}).Where("id = ?", c.ID).Update("config_hash", hash).Error; err != nil {
					return fmt.Errorf("failed to update hash for customer %s: %w", c.ID, err)
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableBudget{}, "CustomerID"); err != nil {
				return fmt.Errorf("failed to drop customer_id column from governance_budgets: %w", err)
			}
			return nil
		},
	}})
	// SQLite workaround — same reasoning as migrationAddMultiBudgetTables: GORM's
	// CreateConstraint (Customer -> Budgets) rebuilds governance_budgets via DROP+RENAME,
	// which fails when other tables hold FKs into it and foreign_keys is ON. PRAGMA
	// foreign_keys cannot change inside a transaction, so disable it on a pinned single
	// connection before the migrator opens its transaction, then restore it.
	if db.Dialector.Name() == "sqlite" {
		sqlDB, err := db.DB()
		if err != nil {
			return fmt.Errorf("failed to get underlying sql.DB: %w", err)
		}
		prevMaxOpenConns := sqlDB.Stats().MaxOpenConnections
		sqlDB.SetMaxOpenConns(1)
		defer sqlDB.SetMaxOpenConns(prevMaxOpenConns)

		if err := db.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
			return fmt.Errorf("failed to disable SQLite foreign keys: %w", err)
		}
		defer func() {
			if err := db.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
				log.Fatalf("[Migration] FATAL: failed to re-enable SQLite foreign keys: %v", err)
			}
		}()
	}
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_customer_budgets_to_budgets_table migration: %s", err.Error())
	}
	return nil
}

// migrationNullLegacyCustomerBudgetID clears the legacy governance_customers.budget_id
// values left behind by migrationAddCustomerBudgetsToBudgetsTable. The column and its
// FK (fk_governance_customers_budget) are intentionally kept — dropping either is
// deferred to a major release — but rows still holding a value make DeleteCustomer's
// `DELETE FROM governance_budgets WHERE customer_id = ?` fail that FK check. Ownership
// already lives on governance_budgets.customer_id, so after a defensive backfill the
// legacy values can be nulled; a null reference satisfies the FK unconditionally.
func migrationNullLegacyCustomerBudgetID(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "null_legacy_customer_budget_id_refs"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			legacyExists, err := hasColumn(tx, "governance_customers", "budget_id")
			if err != nil {
				return fmt.Errorf("failed to introspect governance_customers for budget_id: %w", err)
			}
			if !legacyExists {
				return nil
			}
			// Customers the defensive backfill below will attach a budget to.
			// GenerateCustomerHash includes sorted budget IDs, so their stored
			// config_hash goes stale once the budget is linked and must be refreshed.
			var affectedCustomerIDs []string
			if err := tx.Raw(`
				SELECT DISTINCT c.id
				FROM governance_customers c
				JOIN governance_budgets b ON b.id = c.budget_id
				WHERE b.customer_id IS NULL
				  AND b.virtual_key_id IS NULL AND b.team_id IS NULL
				  AND b.provider_config_id IS NULL AND b.model_config_id IS NULL
			`).Scan(&affectedCustomerIDs).Error; err != nil {
				return fmt.Errorf("failed to identify customers affected by budget backfill: %w", err)
			}
			// Defensive backfill (same shape as migrationAddCustomerBudgetsToBudgetsTable)
			// in case a budget_id was written after that migration ran, e.g. by an older
			// instance in a mixed-version cluster. Only claims budgets with no owner yet.
			if err := tx.Exec(`
				UPDATE governance_budgets SET customer_id = (
					SELECT id FROM governance_customers
					WHERE governance_customers.budget_id = governance_budgets.id
				) WHERE customer_id IS NULL
				  AND virtual_key_id IS NULL AND team_id IS NULL
				  AND provider_config_id IS NULL AND model_config_id IS NULL
				  AND EXISTS (
					SELECT 1 FROM governance_customers
					WHERE governance_customers.budget_id = governance_budgets.id
				)
			`).Error; err != nil {
				return fmt.Errorf("failed to backfill customer budget customer_id: %w", err)
			}
			// Refresh config_hash for customers whose budgets just got linked, keeping
			// migration and runtime hash generation in parity (same as
			// migrationAddCustomerBudgetsToBudgetsTable).
			logger.Info("[configstore] %s: processing %d affectedCustomerIDs", migrationName, len(affectedCustomerIDs))
			for _, customerID := range affectedCustomerIDs {
				var customer tables.TableCustomer
				if err := tx.Preload("Budgets").First(&customer, "id = ?", customerID).Error; err != nil {
					return fmt.Errorf("failed to reload customer %s for hash refresh: %w", customerID, err)
				}
				hash, err := GenerateCustomerHash(customer)
				if err != nil {
					return fmt.Errorf("failed to generate hash for customer %s: %w", customerID, err)
				}
				if err := tx.Model(&tables.TableCustomer{}).Where("id = ?", customerID).Update("config_hash", hash).Error; err != nil {
					return fmt.Errorf("failed to update hash for customer %s: %w", customerID, err)
				}
			}
			if err := tx.Exec(`UPDATE governance_customers SET budget_id = NULL WHERE budget_id IS NOT NULL`).Error; err != nil {
				return fmt.Errorf("failed to clear legacy governance_customers.budget_id values: %w", err)
			}
			return nil
		},
		// Best-effort inverse: repopulate budget_id from governance_budgets.customer_id.
		// The legacy column held a single value while the new model allows several
		// budgets per customer, so for multi-budget customers the oldest budget is
		// picked — for any customer that predates the pivot that is the original
		// legacy budget, since later additions sort newer.
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			legacyExists, err := hasColumn(tx, "governance_customers", "budget_id")
			if err != nil {
				return fmt.Errorf("failed to introspect governance_customers for budget_id: %w", err)
			}
			if !legacyExists {
				return nil
			}
			if err := tx.Exec(`
				UPDATE governance_customers SET budget_id = (
					SELECT id FROM governance_budgets
					WHERE governance_budgets.customer_id = governance_customers.id
					ORDER BY created_at ASC, id ASC
					LIMIT 1
				) WHERE budget_id IS NULL AND EXISTS (
					SELECT 1 FROM governance_budgets
					WHERE governance_budgets.customer_id = governance_customers.id
				)
			`).Error; err != nil {
				return fmt.Errorf("failed to restore legacy governance_customers.budget_id values: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running null_legacy_customer_budget_id_refs migration: %s", err.Error())
	}
	return nil
}

// migrationAddLiveModelsSyncIntervalColumn adds the live_models_sync_interval
// column to framework_configs. It stores how often each provider's list-models
// response is re-fetched in the background, in seconds, with 0 meaning the
// refresher is disabled. Idempotent via HasColumn guards.
func migrationAddLiveModelsSyncIntervalColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_live_models_sync_interval_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableFrameworkConfig{}, "LiveModelsSyncInterval"); err != nil {
				return fmt.Errorf("add live_models_sync_interval column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableFrameworkConfig{}, "LiveModelsSyncInterval"); err != nil {
				return fmt.Errorf("drop live_models_sync_interval column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_live_models_sync_interval_column migration: %s", err.Error())
	}
	return nil
}

// migrationAddCustomerNameUniqueConstraint deduplicates governance_customers by
// appending -1, -2, … to later occurrences of the same name (ordered by
// created_at then id), then adds a unique index on the name column.
func migrationAddCustomerNameUniqueConstraint(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_customer_name_unique_constraint_dedup"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	const idxName = "idx_governance_customers_name"

	// Step 1 (transactional): rename duplicate customer names so the later
	// CREATE UNIQUE INDEX cannot fail due to pre-existing duplicates.
	if err := RunSingleMigration(ctx, nil, db, logger, &migrator.Migration{
		ID: "add_customer_name_unique_constraint_dedup",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// Fetch all customers in a stable order so the earliest-created row
			// always keeps the original name and later duplicates receive suffixes.
			var customers []tables.TableCustomer
			if err := tx.Order("created_at ASC, id ASC").Find(&customers).Error; err != nil {
				return fmt.Errorf("failed to fetch customers: %w", err)
			}

			// taken tracks every name that is currently (or will be) in use so
			// suffix search never collides with an existing original name.
			taken := make(map[string]bool, len(customers))
			for _, c := range customers {
				taken[c.Name] = true
			}

			firstSeen := make(map[string]bool, len(customers))
			logger.Info("[configstore] %s: processing %d customers", migrationName, len(customers))
			for _, c := range customers {
				if !firstSeen[c.Name] {
					firstSeen[c.Name] = true
					continue // earliest occurrence keeps its name
				}
				// Find the lowest suffix whose candidate name is not already taken.
				suffix := 1
				candidate := fmt.Sprintf("%s-%d", c.Name, suffix)
				for taken[candidate] {
					suffix++
					candidate = fmt.Sprintf("%s-%d", c.Name, suffix)
				}
				taken[candidate] = true
				if err := tx.Model(&tables.TableCustomer{}).Where("id = ?", c.ID).Update("name", candidate).Error; err != nil {
					return fmt.Errorf("failed to rename customer %s to %q: %w", c.ID, candidate, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return nil // name renames are not reversed; dropping the index in step 2 restores the invariant
		},
	}); err != nil {
		return err
	}

	// Step 2 (non-transactional): create the unique index.
	// UseTransaction must be false because CREATE INDEX CONCURRENTLY cannot
	// execute inside a transaction block. IF NOT EXISTS makes this step safe
	// to re-run if the process crashes after the index is built but before
	// the migration record is written.
	noTxOpts := *migrator.DefaultOptions
	noTxOpts.UseTransaction = false
	return RunSingleMigration(ctx, &noTxOpts, db, logger, &migrator.Migration{
		ID: "add_customer_name_unique_constraint_index",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			// SQLite does not support CONCURRENTLY; use the plain form there.
			var stmt string
			if tx.Dialector.Name() == "sqlite" {
				stmt = "CREATE UNIQUE INDEX IF NOT EXISTS " + idxName + " ON governance_customers (name)"
			} else {
				stmt = "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS " + idxName + " ON governance_customers (name)"
			}
			if err := tx.Exec(stmt).Error; err != nil {
				return fmt.Errorf("failed to create unique index on governance_customers.name: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return tx.Exec("DROP INDEX IF EXISTS " + idxName).Error
		},
	})
}

// migrationAddSidekiqPartitioningKeyColumn adds the nullable partitioning_key column and
// its index, enabling cluster-wide FIFO-ordered job execution per key.
func migrationAddSidekiqPartitioningKeyColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_sidekiq_partitioning_key_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableSidekiqJob{}, "partitioning_key"); err != nil {
				return err
			}
			return tx.Exec(`CREATE INDEX IF NOT EXISTS idx_sidekiq_partitioning_key ON sidekiq (partitioning_key)`).Error
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableSidekiqJob{}, "partitioning_key")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddVirtualKeyExpiresAtColumn adds nullable expires_at to governance_virtual_keys.
// No index: expiry is checked in-memory from the already-loaded VK, never queried by column.
// migrationAddVKRotationCooldownClientColumn adds the vk_rotation_cooldown_ns
// column to config_client. Nanosecond storage matches schemas.Duration's JSON
// integer encoding; default 0 keeps the pre-cooldown immediate-flip behavior,
// and the hash only covers non-zero values so existing rows show no drift.
func migrationAddVKRotationCooldownClientColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_vk_rotation_cooldown_client_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "vk_rotation_cooldown_ns")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "vk_rotation_cooldown_ns")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddVKRotationCooldownColumns adds the rotation grace-period columns to
// governance_virtual_keys: previous_value, previous_value_hash,
// previous_value_expires_at, and rotated_at. All nullable and additive, no
// backfill (NULL = no previous value = pre-cooldown behavior), so the migration
// is safe during rolling upgrades; pgx cached-plan invalidation (0A000) after
// ADD COLUMN is handled by the postgresconn retry pool.
func migrationAddVKRotationCooldownColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_vk_rotation_cooldown_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	columns := []string{"previous_value", "previous_value_hash", "previous_value_expires_at", "rotated_at"}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, column := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableVirtualKey{}, column); err != nil {
					return err
				}
			}
			// AddColumn doesn't create indexes from struct tags; grace-period
			// auth looks keys up by previous_value_hash, so upgraded databases
			// need the index created explicitly. CreateIndex reads the struct
			// tags so it is dialect-safe.
			mg := tx.Migrator()
			if !mg.HasIndex(&tables.TableVirtualKey{}, "idx_virtual_key_previous_value_hash") {
				logger.Info("[configstore] %s: creating index PreviousValueHash on TableVirtualKey", migrationName)
				if err := mg.CreateIndex(&tables.TableVirtualKey{}, "PreviousValueHash"); err != nil {
					return fmt.Errorf("failed to create index on governance_virtual_keys.previous_value_hash: %w", err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, column := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableVirtualKey{}, column); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

func migrationAddVirtualKeyExpiresAtColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_virtual_key_expires_at_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return addColumnIfNotExists(tx, logger, &tables.TableVirtualKey{}, "expires_at")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableVirtualKey{}, "expires_at")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddClientConfigEnforceVKObjectOwnershipColumn adds enforce_vk_object_ownership
// to config_client. It defaults to true, so an upgrade keeps provider objects bound to the
// virtual key that created them, as they were before the setting existed.
func migrationAddClientConfigEnforceVKObjectOwnershipColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_client_config_enforce_vk_object_ownership_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "enforce_vk_object_ownership"); err != nil {
				return err
			}
			return tx.Exec("UPDATE config_client SET enforce_vk_object_ownership = ? WHERE enforce_vk_object_ownership IS NULL", true).Error
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "enforce_vk_object_ownership")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddClientConfigExtraFieldsPlacementColumn adds extra_fields_placement to
// config_client. It defaults to "body", so an upgrade keeps extra_fields in the JSON
// body of inference responses, as it was before the setting existed.
func migrationAddClientConfigExtraFieldsPlacementColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_client_config_extra_fields_placement_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "extra_fields_placement")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "extra_fields_placement")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddClientConfigDeleteExpiredVirtualKeysColumn adds delete_expired_virtual_keys to config_client.
func migrationAddClientConfigDeleteExpiredVirtualKeysColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_client_config_delete_expired_virtual_keys_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "delete_expired_virtual_keys")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "delete_expired_virtual_keys")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddVirtualKeyDeleteAfterExpireColumn adds the nullable delete_after_expire
// column to governance_virtual_keys. NULL inherits client.delete_expired_virtual_keys.
// No index: the daily cleanup scan already filters on expires_at and touches few rows.
func migrationAddVirtualKeyDeleteAfterExpireColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_virtual_key_delete_after_expire_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return addColumnIfNotExists(tx, logger, &tables.TableVirtualKey{}, "delete_after_expire")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableVirtualKey{}, "delete_after_expire")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddVertexForceSingleRegionColumn adds the vertex_force_single_region column to the key table.
// Existing keys default to false (NULL), preserving the current multi-region promotion behaviour.
func migrationAddVertexForceSingleRegionColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_vertex_force_single_region_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return addColumnIfNotExists(tx, logger, &tables.TableKey{}, "vertex_force_single_region")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableKey{}, "vertex_force_single_region")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddSidekiqTable creates the generic `sidekiq` background-job table. Uses raw SQL
// (not GORM auto-DDL) so the schema is explicit and stable across GORM versions.
// Idempotent via CREATE TABLE IF NOT EXISTS; covers postgres and sqlite dialects.
func migrationAddSidekiqTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_sidekiq_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			var createTable string
			switch tx.Dialector.Name() {
			case "postgres":
				createTable = `
					CREATE TABLE IF NOT EXISTS sidekiq (
						id                  TEXT PRIMARY KEY,
						kind                TEXT NOT NULL,
						status              TEXT NOT NULL DEFAULT 'pending',
						runner_id           TEXT,
						metadata            TEXT DEFAULT '{}',
						attempts            INTEGER NOT NULL DEFAULT 0,
						last_error          TEXT,
						created_at          TIMESTAMPTZ NOT NULL,
						updated_at          TIMESTAMPTZ NOT NULL,
						started_at          TIMESTAMPTZ,
						created_by_user_id  VARCHAR(255),
						completed_at        TIMESTAMPTZ
					)`
			case "sqlite":
				createTable = `
					CREATE TABLE IF NOT EXISTS sidekiq (
						id                  TEXT PRIMARY KEY,
						kind                TEXT NOT NULL,
						status              TEXT NOT NULL DEFAULT 'pending',
						runner_id           TEXT,
						metadata            TEXT DEFAULT '{}',
						attempts            INTEGER NOT NULL DEFAULT 0,
						last_error          TEXT,
						created_at          DATETIME NOT NULL,
						updated_at          DATETIME NOT NULL,
						started_at          DATETIME,
						created_by_user_id  VARCHAR(255),
						completed_at        DATETIME
					)`
			default:
				// Fall back to GORM for any other dialect so the migration does not
				// hard-fail on an unsupported backend.
				if err := tx.Migrator().AutoMigrate(&tables.TableSidekiqJob{}); err != nil {
					return err
				}
				return nil
			}
			if err := tx.Exec(createTable).Error; err != nil {
				return err
			}

			// For existing tables, add new columns that were not present in the
			// original schema (no-op when the columns already exist).
			if err := addColumnIfNotExists(tx, logger, &tables.TableSidekiqJob{}, "runner_id"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableSidekiqJob{}, "created_by_user_id"); err != nil {
				return err
			}

			// idx_sidekiq_status_updated supports the reaper/recovery scan.
			if err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_sidekiq_status_updated ON sidekiq (status, updated_at)`).Error; err != nil {
				return err
			}
			// idx_sidekiq_runner supports fencing lookups by runner_id.
			return tx.Exec(`CREATE INDEX IF NOT EXISTS idx_sidekiq_runner ON sidekiq (runner_id)`).Error
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			// It is okay to drop the table
			return tx.Exec(`DROP TABLE IF EXISTS sidekiq`).Error
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddSidekiqKindStatusCreatedIndex adds a composite index on
// (kind, status, created_at) so GetInFlightSidekiqJobByKind — which filters by
// kind + status and orders by created_at DESC — can seek instead of doing a
// filtered full scan plus sort as the table accumulates historical jobs.
// Idempotent via CREATE INDEX IF NOT EXISTS; works for postgres and sqlite.
//
// The index is built non-transactionally (UseTransaction=false) with CREATE
// INDEX CONCURRENTLY on postgres so it does not take a ShareLock that blocks
// concurrent writes to the sidekiq table for the duration of the build. SQLite
// does not support CONCURRENTLY, so it uses the plain form there.
func migrationAddSidekiqKindStatusCreatedIndex(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_sidekiq_kind_status_created_index"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	noTxOpts := *migrator.DefaultOptions
	noTxOpts.UseTransaction = false
	if err := RunSingleMigration(ctx, &noTxOpts, db, logger, &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			// SQLite does not support CONCURRENTLY; use the plain form there.
			var stmt string
			if tx.Dialector.Name() == "sqlite" {
				stmt = "CREATE INDEX IF NOT EXISTS idx_sidekiq_kind_status_created ON sidekiq (kind, status, created_at)"
			} else {
				stmt = "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_sidekiq_kind_status_created ON sidekiq (kind, status, created_at)"
			}
			return tx.Exec(stmt).Error
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return tx.Exec(`DROP INDEX IF EXISTS idx_sidekiq_kind_status_created`).Error
		},
	}); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddBudgetResetConfigColumn adds the reset_config_json column to
// governance_budgets.
//
// Additive and nullable with no backfill: an existing budget has no quarter
// definition, and a NULL column reads back as a nil ResetConfig, which every
// window call site treats as January. Pre-existing budgets therefore keep their
// current cadence exactly.
func migrationAddBudgetResetConfigColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_budget_reset_config_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()
			if !mg.HasColumn(&tables.TableBudget{}, "reset_config_json") {
				if err := mg.AddColumn(&tables.TableBudget{}, "ResetConfigJSON"); err != nil {
					return fmt.Errorf("add reset_config_json column: %w", err)
				}
			}
			return nil
		},
		Rollback: rollbackBudgetResetConfigColumn,
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running budget reset config column migration: %s", err.Error())
	}
	return nil
}

// rollbackBudgetResetConfigColumn refuses to undo
// migrationAddBudgetResetConfigColumn. reset_config_json is the only home for a
// budget's quarter definition, and QuarterStartMonth reads a nil ResetConfig as
// January, so dropping the column would not merely lose data: it would silently
// re-window every fiscal-year budget onto the calendar year.
func rollbackBudgetResetConfigColumn(*gorm.DB) error {
	return fmt.Errorf("add_budget_reset_config_column is non-rollbackable: dropping reset_config_json would permanently delete every budget's fiscal-quarter definition and silently revert those budgets to the January default; the column is additive and older binaries safely ignore it")
}

// migrationAddBedrockBatchRoleARNColumn adds the bedrock_batch_role_arn column to the config_keys
// table. It stores the service role passed to Bedrock batch jobs for S3 access, kept separate from
// the STS AssumeRole identity in bedrock_role_arn.
func migrationAddBedrockBatchRoleARNColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_bedrock_batch_role_arn_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return addColumnIfNotExists(tx, logger, &tables.TableKey{}, "bedrock_batch_role_arn")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableKey{}, "bedrock_batch_role_arn")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddPricingOverrideUserIDColumn adds the user_id scope column to
// governance_pricing_overrides and rebuilds the composite scope index so it
// covers the new column.
func migrationAddPricingOverrideUserIDColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_pricing_override_user_id_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TablePricingOverride{}, "user_id"); err != nil {
				return fmt.Errorf("failed to add user_id column to governance_pricing_overrides: %w", err)
			}
			mgr := tx.Migrator()
			if mgr.HasIndex(&tables.TablePricingOverride{}, "idx_pricing_override_scope") {
				if err := mgr.DropIndex(&tables.TablePricingOverride{}, "idx_pricing_override_scope"); err != nil {
					return fmt.Errorf("failed to drop pricing override scope index for rebuild: %w", err)
				}
			}
			if err := mgr.CreateIndex(&tables.TablePricingOverride{}, "idx_pricing_override_scope"); err != nil {
				return fmt.Errorf("failed to recreate pricing override scope index: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mgr := tx.Migrator()
			// Drop the rebuilt index first: SQLite refuses to drop an indexed
			// column, and Postgres would silently drop the index with it.
			if mgr.HasIndex(&tables.TablePricingOverride{}, "idx_pricing_override_scope") {
				if err := mgr.DropIndex(&tables.TablePricingOverride{}, "idx_pricing_override_scope"); err != nil {
					return fmt.Errorf("failed to drop pricing override scope index for rollback: %w", err)
				}
			}
			if mgr.HasColumn(&tables.TablePricingOverride{}, "user_id") {
				if err := mgr.DropColumn(&tables.TablePricingOverride{}, "user_id"); err != nil {
					return fmt.Errorf("failed to drop user_id column from governance_pricing_overrides: %w", err)
				}
			}
			// Restore the legacy index shape via raw SQL; CreateIndex would use
			// the current struct tags, which include user_id.
			if err := tx.Exec("CREATE INDEX idx_pricing_override_scope ON governance_pricing_overrides (scope_kind, virtual_key_id, provider_id, provider_key_id)").Error; err != nil {
				return fmt.Errorf("failed to restore legacy pricing override scope index: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running pricing override user_id column migration: %s", err.Error())
	}
	return nil
}

// migrationAddBedrockEndpointsColumns adds the bedrock_endpoints_json and
// bedrock_mantle_endpoints_json columns to the config_keys table. They hold the interface VPC
// endpoint hosts dialled in place of the public regional endpoints, serialized as
// schemas.BedrockEndpoints.
func migrationAddBedrockEndpointsColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_bedrock_endpoints_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	columns := []string{"bedrock_endpoints_json", "bedrock_mantle_endpoints_json"}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, column := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, column); err != nil {
					return fmt.Errorf("failed to add %s column: %w", column, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, column := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, column); err != nil {
					return fmt.Errorf("failed to drop %s column: %w", column, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddBatchJobsTable creates the batch_jobs coordination table that tracks
// the provider batch lifecycle and delayed accounting state. Uses raw SQL (not
// GORM auto-DDL) so the schema is explicit and stable; idempotent via CREATE TABLE
// IF NOT EXISTS; covers postgres and sqlite dialects with a GORM fallback.
func migrationAddBatchJobsTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_batch_jobs_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			var createTable string
			switch tx.Dialector.Name() {
			case "postgres":
				createTable = `
					CREATE TABLE IF NOT EXISTS batch_jobs (
						id                       VARCHAR(512) PRIMARY KEY,
						provider                 VARCHAR(255) NOT NULL,
						batch_id                 VARCHAR(255) NOT NULL,
						model                    VARCHAR(255),
						endpoint                 VARCHAR(255),
						provider_status          VARCHAR(50),
						input_file_id            VARCHAR(255),
						output_file_id           VARCHAR(255),
						error_file_id            VARCHAR(255),
						results_url              TEXT,
						next_check_at            TIMESTAMPTZ,
						poll_attempts            INTEGER NOT NULL DEFAULT 0,
						accounting_status        VARCHAR(50) NOT NULL,
						runner_id                VARCHAR(255),
						claimed_at               TIMESTAMPTZ,
						unpriceable_reason       VARCHAR(255),
						last_error               TEXT,
						aggregate_log_written_at TIMESTAMPTZ,
						governance_reported_at   TIMESTAMPTZ,
						selected_key_id          VARCHAR(255),
						virtual_key_id           VARCHAR(255),
						budget_ids               TEXT,
						rate_limit_ids           TEXT,
						created_at               TIMESTAMPTZ NOT NULL,
						updated_at               TIMESTAMPTZ NOT NULL
					)`
			case "sqlite":
				createTable = `
					CREATE TABLE IF NOT EXISTS batch_jobs (
						id                       TEXT PRIMARY KEY,
						provider                 TEXT NOT NULL,
						batch_id                 TEXT NOT NULL,
						model                    TEXT,
						endpoint                 TEXT,
						provider_status          TEXT,
						input_file_id            TEXT,
						output_file_id           TEXT,
						error_file_id            TEXT,
						results_url              TEXT,
						next_check_at            DATETIME,
						poll_attempts            INTEGER NOT NULL DEFAULT 0,
						accounting_status        TEXT NOT NULL,
						runner_id                TEXT,
						claimed_at               DATETIME,
						unpriceable_reason       TEXT,
						last_error               TEXT,
						aggregate_log_written_at DATETIME,
						governance_reported_at   DATETIME,
						selected_key_id          TEXT,
						virtual_key_id           TEXT,
						budget_ids               TEXT,
						rate_limit_ids           TEXT,
						created_at               DATETIME NOT NULL,
						updated_at               DATETIME NOT NULL
					)`
			default:
				// Fall back to GORM for any other dialect so the migration does not
				// hard-fail on an unsupported backend.
				return tx.Migrator().AutoMigrate(&tables.TableProviderJob{})
			}

			if err := tx.Exec(createTable).Error; err != nil {
				return err
			}

			// idx_batch_jobs_identity enforces one row per (provider, batch_id).
			if err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_batch_jobs_identity ON batch_jobs (provider, batch_id)`).Error; err != nil {
				return err
			}
			// idx_batch_jobs_sweeper backs the due-job poll scan.
			if err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_batch_jobs_sweeper ON batch_jobs (provider, accounting_status, next_check_at)`).Error; err != nil {
				return err
			}
			// idx_batch_jobs_runner_id supports fencing lookups by runner_id.
			return tx.Exec(`CREATE INDEX IF NOT EXISTS idx_batch_jobs_runner_id ON batch_jobs (runner_id)`).Error
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return tx.Migrator().DropTable(&tables.TableProviderJob{})
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while creating batch_jobs table: %s", err.Error())
	}
	return nil
}

// readComplexityConfigRow returns a governance_config value, or "" when the row
// is absent or blank.
func readComplexityConfigRow(tx *gorm.DB, key string) (string, error) {
	var entry tables.TableGovernanceConfig
	err := tx.First(&entry, "key = ?", key).Error
	if err == gorm.ErrRecordNotFound {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query persisted %s: %w", key, err)
	}
	return strings.TrimSpace(entry.Value), nil
}

// preSplitComplexityAnalyzerRow is an analyzer row written before the lexical
// and semantic configs moved into separate rows.
type preSplitComplexityAnalyzerRow struct {
	Keywords     ComplexityEditableKeywordConfig
	Semantic     *ComplexitySemanticConfig
	LLM          *ComplexityLLMConfig
	ConfigHashes ComplexityAnalyzerConfigHashes
	// semanticUnreadable and llmUnreadable hold the reason a block was skipped,
	// if it was present but could not be decoded.
	semanticUnreadable string
	llmUnreadable      string
}

// complexityConfigFromPreSplitAnalyzerRow reads everything worth carrying out of
// a pre-split analyzer row.
//
// Both keyword spellings are accepted: an installation upgrading from a release
// carries the four-list lexical shape, and one upgrading from a pre-split build
// of this version carries the canonical three.
//
// The semantic block is decoded on a best-effort basis and reported rather than
// returned as an error. A build further up the stack may have written fields
// this version has no name for, and refusing to migrate at all would be a worse
// outcome than continuing without a section that was never readable here.
func complexityConfigFromPreSplitAnalyzerRow(data []byte) (preSplitComplexityAnalyzerRow, error) {
	var row struct {
		Keywords     ComplexityEditableKeywordConfig `json:"keywords"`
		Semantic     json.RawMessage                 `json:"semantic"`
		LLM          json.RawMessage                 `json:"llm"`
		ConfigHashes ComplexityAnalyzerConfigHashes  `json:"_config_hashes"`
	}
	if err := json.Unmarshal(data, &row); err != nil {
		return preSplitComplexityAnalyzerRow{}, err
	}

	out := preSplitComplexityAnalyzerRow{
		Keywords:     row.Keywords,
		ConfigHashes: row.ConfigHashes,
	}
	if len(row.Semantic) > 0 && string(row.Semantic) != "null" {
		var semantic ComplexitySemanticConfig
		if err := json.Unmarshal(row.Semantic, &semantic); err != nil {
			out.semanticUnreadable = err.Error()
			out.ConfigHashes.SemanticSettings = ""
		} else {
			out.Semantic = &semantic
		}
	}
	// The llm block travels with the semantic settings it backs: without it, a
	// carried-over "fallback": "llm" would name a classifier that is not there,
	// which Validate rejects.
	if len(row.LLM) > 0 && string(row.LLM) != "null" {
		var llm ComplexityLLMConfig
		if err := json.Unmarshal(row.LLM, &llm); err != nil {
			out.llmUnreadable = err.Error()
			out.ConfigHashes.LLMSettings = ""
			if out.Semantic != nil && strings.EqualFold(strings.TrimSpace(out.Semantic.Fallback), ComplexitySemanticFallbackLLM) {
				// The selector goes with the block it selects. Left alone it
				// names a classifier that is not there, which is what Validate
				// rejects -- and the caller drops the whole carried config over
				// it, not just the block that could not be read. Its section
				// hash goes too, so a config.json that still asks for the llm
				// fallback re-applies both blocks on the next boot.
				out.Semantic.Fallback = ComplexitySemanticFallbackNone
				out.ConfigHashes.SemanticSettings = ""
			}
		} else {
			out.LLM = &llm
		}
	}
	return out, nil
}

// migrationBackfillDefaultComplexityExemplars appends the curated semantic
// exemplars to persisted complexity configurations created before those
// defaults existed. Existing phrases and tier assignments always win.
func migrationBackfillDefaultComplexityExemplars(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "backfill_default_complexity_exemplars_v2"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)

	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			analyzerRaw, err := readComplexityConfigRow(tx, tables.ConfigComplexityAnalyzerConfigKey)
			if err != nil {
				return err
			}
			semanticRaw, err := readComplexityConfigRow(tx, tables.ConfigComplexitySemanticConfigKey)
			if err != nil {
				return err
			}
			if analyzerRaw == "" && semanticRaw == "" {
				// Fresh install: nothing has been persisted for either
				// classifier, so the defaults apply without being written down.
				return nil
			}

			semanticRow, err := decodeComplexitySemanticConfigRow([]byte(semanticRaw))
			if err != nil {
				// The row is there but this version cannot read it, most often
				// because a build further up the stack wrote a field this one has
				// no name for: ComplexitySemanticConfig rejects unknown fields.
				// Failing would abort this migration and every migration queued
				// behind it, and treating the row as absent would rebuild it from
				// the pre-split analyzer row, overwriting whatever it actually
				// holds. The backfill is dropped instead and the row is left
				// exactly as it was, the same trade the validation failure below
				// makes.
				logger.Warn("[configstore] %s: skipped exemplar backfill, the persisted semantic config is unreadable: %v",
					migrationName, err)
				return nil
			}

			var config ComplexityAnalyzerConfig
			if semanticRow != nil {
				config.Keywords = semanticRow.Keywords
				config.Semantic = semanticRow.Semantic
			} else if analyzerRaw != "" {
				// Nothing has been written to the semantic row yet, so this
				// installation predates the split. Whatever its analyzer row
				// holds is what it has been routing with, so it seeds the
				// semantic row rather than being dropped for the defaults.
				preSplit, err := complexityConfigFromPreSplitAnalyzerRow([]byte(analyzerRaw))
				if err != nil {
					return fmt.Errorf("read exemplars from persisted complexity analyzer config: %w", err)
				}
				config.Keywords = preSplit.Keywords
				config.Semantic = preSplit.Semantic
				// The semantic row owns the keyword sections after the split, so it
				// owns their section hashes too. Dropping them here would leave the
				// row looking like the config file's keyword sections had never been
				// applied, and the next sync would reapply them over the phrases this
				// installation has been routing with.
				config.ConfigHashes.SimpleKeywords = preSplit.ConfigHashes.SimpleKeywords
				config.ConfigHashes.MediumKeywords = preSplit.ConfigHashes.MediumKeywords
				config.ConfigHashes.ComplexKeywords = preSplit.ConfigHashes.ComplexKeywords
				config.LLM = preSplit.LLM
				config.ConfigHashes.SemanticSettings = preSplit.ConfigHashes.SemanticSettings
				config.ConfigHashes.LLMSettings = preSplit.ConfigHashes.LLMSettings
				if preSplit.llmUnreadable != "" {
					logger.Warn("[configstore] %s: could not carry the pre-split llm block forward: %s",
						migrationName, preSplit.llmUnreadable)
				}
				if preSplit.semanticUnreadable != "" {
					// A semantic block this version cannot parse comes from a
					// build further up the stack. Dropping it is better than
					// failing the migration, but it is not silent.
					logger.Warn("[configstore] %s: could not carry the pre-split semantic block forward: %s",
						migrationName, preSplit.semanticUnreadable)
				}
			}

			added := appendMissingDefaultComplexityExemplars(&config, legacyComplexityExemplarsV2())
			if added == 0 && semanticRow != nil {
				return nil
			}

			// Normalize before persisting: appendMissingDefaultComplexityExemplars
			// adds the curated phrases in their authored form, and persisted
			// phrases are stored lowercased and deduplicated.
			normalized := config.Normalized()
			if err := normalized.Validate(); err != nil {
				// The stored config is what this installation has been routing
				// with, and the backfill only meant to add phrases to it.
				// Failing here would abort this migration and every migration
				// queued behind it, so the backfill is dropped and the stored
				// config is left exactly as it was.
				logger.Warn("[configstore] %s: skipped exemplar backfill, the result does not validate: %v",
					migrationName, err)
				return nil
			}

			record := complexitySemanticConfigRecord{
				Keywords: normalized.Keywords,
				Semantic: normalized.Semantic,
				LLM:      normalized.LLM,
				ConfigHashes: complexitySemanticRowHashes{
					SimpleKeywords:   config.ConfigHashes.SimpleKeywords,
					MediumKeywords:   config.ConfigHashes.MediumKeywords,
					ComplexKeywords:  config.ConfigHashes.ComplexKeywords,
					SemanticSettings: config.ConfigHashes.SemanticSettings,
					LLMSettings:      config.ConfigHashes.LLMSettings,
				},
			}
			if semanticRow != nil {
				record.ConfigHashes = semanticRow.ConfigHashes
				// The fingerprint records which exemplars were embedded, and
				// those just changed, so whatever it names is stale. It is left
				// blank rather than carried over.
			}
			raw, err := json.Marshal(record)
			if err != nil {
				return fmt.Errorf("encode complexity semantic config after exemplar backfill: %w", err)
			}

			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "key"}},
				DoUpdates: clause.AssignmentColumns([]string{"value"}),
			}).Create(&tables.TableGovernanceConfig{
				Key:   tables.ConfigComplexitySemanticConfigKey,
				Value: string(raw),
			}).Error; err != nil {
				return fmt.Errorf("persist complexity semantic exemplar backfill: %w", err)
			}

			logger.Info("[configstore] %s: added %d default complexity exemplars", migrationName, added)
			return nil
		},
		Rollback: func(*gorm.DB) error {
			// Removing phrases later would also remove administrator-owned data
			// that happens to match a default exemplar.
			return fmt.Errorf("%s is non-rollbackable: appended default phrases cannot be distinguished safely from administrator-owned phrases", migrationName)
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddModelRoutersTable creates model_routers; nothing is copied into it.
func migrationAddModelRoutersTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_model_routers_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	return RunSingleMigration(ctx, nil, db, logger, &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			return tx.WithContext(ctx).AutoMigrate(&tables.TableModelRouter{})
		},
		Rollback: func(*gorm.DB) error {
			return fmt.Errorf("%s is non-rollbackable: dropping model_routers would delete every model router", migrationName)
		},
	})
}

// migrationAddModelRouterTargetsAndRouting creates the model routers'
// per-tier targets and adds the routing columns, which decide the requests a
// router takes, to model_routers. Routers start without targets and off: a
// routing rule's complexity_tier condition is untouched, so nothing that
// routes today changes.
func migrationAddModelRouterTargetsAndRouting(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_model_router_targets_and_routing"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	return RunSingleMigration(ctx, nil, db, logger, &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			return tx.WithContext(ctx).AutoMigrate(&tables.TableModelRouter{}, &tables.TableModelRouterTarget{})
		},
		Rollback: func(*gorm.DB) error {
			return fmt.Errorf("%s is non-rollbackable: dropping model_router_targets would delete every router's models", migrationName)
		},
	})
}

// modelRouterSuggestionTierColumns hold a router's suggestions tier block: the
// classifier that decides suggested tiers, its model, and the text it reads.
var modelRouterSuggestionTierColumns = []string{
	"SuggestionsTierClassifier", "SuggestionsTierProvider", "SuggestionsTierModel",
	"SuggestionsTierPrompt", "SuggestionsTierInstructions", "SuggestionsTierCriteriaJSON",
}

// migrationAddModelRouterSuggestionTierColumns adds the suggestions tier
// block's columns. A router without them reads as having no tier block, which
// decides tiers with its phrase model, as before tiers were configurable.
func migrationAddModelRouterSuggestionTierColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_model_router_suggestions_tier_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := setMigrationLockTimeout(tx); err != nil {
				return err
			}
			for _, column := range modelRouterSuggestionTierColumns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelRouter{}, column); err != nil {
					return fmt.Errorf("add %s column to model_routers: %w", column, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, column := range modelRouterSuggestionTierColumns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelRouter{}, column); err != nil {
					return fmt.Errorf("drop %s column from model_routers: %w", column, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

func appendMissingDefaultComplexityExemplars(config *ComplexityAnalyzerConfig, defaults ComplexityEditableKeywordConfig) int {
	type tierPhrases struct {
		values   *[]string
		defaults []string
	}
	tiers := []tierPhrases{
		{values: &config.Keywords.SimpleKeywords, defaults: defaults.SimpleKeywords},
		{values: &config.Keywords.MediumKeywords, defaults: defaults.MediumKeywords},
		{values: &config.Keywords.ComplexKeywords, defaults: defaults.ComplexKeywords},
	}

	seen := make(map[string]struct{})
	for _, tier := range tiers {
		for _, phrase := range *tier.values {
			seen[normalizeComplexityExemplarKey(phrase)] = struct{}{}
		}
	}

	added := 0
	for _, tier := range tiers {
		for _, phrase := range tier.defaults {
			key := normalizeComplexityExemplarKey(phrase)
			if _, exists := seen[key]; exists {
				continue
			}
			*tier.values = append(*tier.values, phrase)
			seen[key] = struct{}{}
			added++
		}
	}
	return added
}

func normalizeComplexityExemplarKey(phrase string) string {
	return strings.ToLower(strings.Join(strings.Fields(phrase), " "))
}

// migrationAddImageMegapixelTierPricingColumns adds the megapixel-banded output
// image cost tier columns (output_cost_per_image_above_{4,8,16,32,64}_megapixels),
// used by providers (e.g. Replicate's upscaler models) that publish tiered
// per-image pricing by total output megapixels rather than by a squared
// width/height threshold.
func migrationAddImageMegapixelTierPricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_image_megapixel_tier_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	columns := []string{
		"output_cost_per_image_above_4_megapixels",
		"output_cost_per_image_above_8_megapixels",
		"output_cost_per_image_above_16_megapixels",
		"output_cost_per_image_above_32_megapixels",
		"output_cost_per_image_above_64_megapixels",
	}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running add_image_megapixel_tier_pricing_columns migration: %s", err.Error())
	}
	return nil
}

// migrationAddInputCostPerQueryColumn adds the per-query rerank rate. Rerank models bill per
// query (a "search unit" covering up to 100 document chunks) rather than per token, so without
// this column every rerank request costs zero.
func migrationAddInputCostPerQueryColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_input_cost_per_query_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, "input_cost_per_query"); err != nil {
				return fmt.Errorf("failed to add column input_cost_per_query: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, "input_cost_per_query"); err != nil {
				return fmt.Errorf("failed to drop column input_cost_per_query: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// migrationAddUltrafastPricingColumns adds the OpenAI Ultrafast service-tier
// rates. The fields are nullable so catalogs without Ultrafast pricing retain
// the existing standard-rate fallback.
func migrationAddUltrafastPricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_ultrafast_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	columns := []string{
		"input_cost_per_token_ultrafast",
		"output_cost_per_token_ultrafast",
		"cache_read_input_token_cost_ultrafast",
		"cache_creation_input_token_cost_ultrafast",
	}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// migrationAddUltrafastAbove272kPricingColumns adds the OpenAI Ultrafast rates
// for prompts above 272k tokens. The fields are nullable so catalogs without them
// keep the flat Ultrafast rate as the fallback.
func migrationAddUltrafastAbove272kPricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_ultrafast_above_272k_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	columns := []string{
		"input_cost_per_token_above_272k_tokens_ultrafast",
		"output_cost_per_token_above_272k_tokens_ultrafast",
		"cache_read_input_token_cost_above_272k_tokens_ultrafast",
		"cache_creation_input_token_cost_above_272k_tokens_ultrafast",
	}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// migrationAddImageSizeQualityPricingColumns adds the per-size and joint
// size+quality per-image output rate columns to the model pricing table.
func migrationAddImageSizeQualityPricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_image_size_quality_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	columns := []string{
		"output_cost_per_image_above_1024_and_1536_pixels",
		"output_cost_per_image_above_1536_and_1024_pixels",
		"output_cost_per_image_above_1024_and_1024_pixels_low_quality",
		"output_cost_per_image_above_1024_and_1536_pixels_low_quality",
		"output_cost_per_image_above_1536_and_1024_pixels_low_quality",
		"output_cost_per_image_above_1024_and_1024_pixels_medium_quality",
		"output_cost_per_image_above_1024_and_1536_pixels_medium_quality",
		"output_cost_per_image_above_1536_and_1024_pixels_medium_quality",
		"output_cost_per_image_above_1024_and_1024_pixels_high_quality",
		"output_cost_per_image_above_1024_and_1536_pixels_high_quality",
		"output_cost_per_image_above_1536_and_1024_pixels_high_quality",
		"output_cost_per_image_above_1024x1024_pixels_standard_quality",
		"output_cost_per_image_above_1024x1536_pixels_standard_quality",
		"output_cost_per_image_above_1536x1024_pixels_standard_quality",
	}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// migrationAddProviderJobKindColumns generalises batch_jobs from a batch-only
// table to one holding every provider-side job kind.
//
// Additive by construction: kind carries a constant default, so it is
// metadata-only on postgres 11+ and O(1) on sqlite — existing rows *become*
// kind='batch' without an UPDATE touching a single one of them, which is exactly
// what they always were. params is nullable with no default.
//
// The identity index widens from (provider, batch_id) to (provider, kind,
// batch_id). The replacement is strictly weaker than the index it replaces, so it
// can never fail to build on existing data, and it is created before the old one
// is dropped so uniqueness is never briefly unenforced.
func migrationAddProviderJobKindColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_provider_job_kind_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)

	// Step 1 (transactional): the columns. Both are metadata-only — kind's constant
	// default is applied on read rather than by rewriting rows, so this is O(1) on
	// postgres 11+ and sqlite regardless of how large batch_jobs has grown.
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mig := tx.Migrator()

			// Raw DDL rather than AddColumn so the NOT NULL DEFAULT is explicit: the
			// default is what backfills every pre-existing row, and it must not depend
			// on how GORM chooses to render a struct tag.
			if !mig.HasColumn(&tables.TableProviderJob{}, "kind") {
				if err := tx.Exec(`ALTER TABLE batch_jobs ADD COLUMN kind VARCHAR(50) NOT NULL DEFAULT 'batch'`).Error; err != nil {
					return fmt.Errorf("failed to add kind column to batch_jobs: %w", err)
				}
			}
			if !mig.HasColumn(&tables.TableProviderJob{}, "params") {
				if err := tx.Exec(`ALTER TABLE batch_jobs ADD COLUMN params TEXT`).Error; err != nil {
					return fmt.Errorf("failed to add params column to batch_jobs: %w", err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return rollbackProviderJobKindColumns(ctx, tx)
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}

	// Step 2 (non-transactional): swap the indexes onto the new column.
	return migrationSwapProviderJobIndexes(ctx, db, logger)
}

// providerJobIndexSwap is the index rework batch_jobs needs once rows carry a kind:
// identity widens to include it, and the sweeper scans by it first.
var providerJobIndexSwap = []struct {
	name     string
	columns  string
	unique   bool
	replaces string
}{
	{
		name:     "idx_batch_jobs_identity_v2",
		columns:  "(provider, kind, batch_id)",
		unique:   true,
		replaces: "idx_batch_jobs_identity",
	},
	{
		name:     "idx_batch_jobs_sweeper_v2",
		columns:  "(kind, provider, accounting_status, next_check_at)",
		replaces: "idx_batch_jobs_sweeper",
	},
}

func migrationSwapProviderJobIndexes(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "swap_provider_job_indexes"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)

	noTxOpts := *migrator.DefaultOptions
	noTxOpts.UseTransaction = false
	return RunSingleMigration(ctx, &noTxOpts, db, logger, &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			// SQLite has a single writer and no CONCURRENTLY; the plain form is both
			// required and harmless there.
			concurrent := tx.Dialector.Name() != "sqlite"

			for _, idx := range providerJobIndexSwap {
				unique := ""
				if idx.unique {
					unique = "UNIQUE "
				}
				if concurrent {
					valid, err := postgresIndexIsValid(tx, "batch_jobs", idx.name)
					if err != nil {
						return fmt.Errorf("failed to check whether %s is valid: %w", idx.name, err)
					}
					if !valid {
						waited, err := waitForPostgresIndexBuild(tx, idx.name)
						if err != nil {
							return err
						}
						if waited {
							if valid, err = postgresIndexIsValid(tx, "batch_jobs", idx.name); err != nil {
								return fmt.Errorf("failed to check whether %s is valid: %w", idx.name, err)
							}
						}
					}
					if !valid {
						if err := tx.Exec("DROP INDEX CONCURRENTLY IF EXISTS " + idx.name).Error; err != nil {
							return fmt.Errorf("failed to clear a partial %s: %w", idx.name, err)
						}
					}
					if err := tx.Exec(fmt.Sprintf("CREATE %sINDEX CONCURRENTLY IF NOT EXISTS %s ON batch_jobs %s", unique, idx.name, idx.columns)).Error; err != nil {
						return fmt.Errorf("failed to create %s: %w", idx.name, err)
					}
				} else if err := tx.Exec(fmt.Sprintf("CREATE %sINDEX IF NOT EXISTS %s ON batch_jobs %s", unique, idx.name, idx.columns)).Error; err != nil {
					return fmt.Errorf("failed to create %s: %w", idx.name, err)
				}

				// Only now drop what it replaces. The new index is strictly weaker than
				// the old one, so it can never fail to build on data the old one already
				// accepted — but dropping second still means uniqueness is never briefly
				// unenforced.
				drop := "DROP INDEX IF EXISTS " + idx.replaces
				if concurrent {
					drop = "DROP INDEX CONCURRENTLY IF EXISTS " + idx.replaces
				}
				if err := tx.Exec(drop).Error; err != nil {
					return fmt.Errorf("failed to drop %s: %w", idx.replaces, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			// Index-only; the column rollback restores these names itself.
			return nil
		},
	})
}

// rollbackProviderJobKindColumns reverses migrationAddProviderJobKindColumns.
//
// Order matters, and it is the reverse of the intuitive one: the columns go first,
// the narrow indexes are recreated last. Dropping a column takes its dependent
// indexes with it — postgres cascades, and sqlite rebuilds the whole table from the
// model — so anything restored beforehand is destroyed on the way past.
func rollbackProviderJobKindColumns(ctx context.Context, db *gorm.DB) error {
	tx := db.WithContext(ctx)
	mig := tx.Migrator()

	// Refuse before touching anything if a non-batch job exists. The narrow index
	// this rollback restores is UNIQUE on (provider, batch_id), and a video job is
	// free to carry the same provider-side id as a batch — so rebuilding it would
	// fail on real data. Failing here rather than there matters: the column drop
	// comes first, so a rollback that got as far as the index would already have
	// erased the only thing distinguishing those rows, leaving the sweeper to
	// settle a video as a batch.
	if mig.HasColumn(&tables.TableProviderJob{}, "kind") {
		var foreign int64
		if err := tx.Table("batch_jobs").Where("kind <> ?", tables.ProviderJobKindBatch).Count(&foreign).Error; err != nil {
			return fmt.Errorf("failed to check for non-batch provider jobs: %w", err)
		}
		if foreign > 0 {
			return fmt.Errorf("add_provider_job_kind_columns is non-rollbackable while %d non-batch job(s) exist in batch_jobs: dropping kind would merge them into the batch namespace, where they collide on (provider, batch_id) and would be settled as batches; delete those rows first if the rollback is genuinely intended", foreign)
		}
	}

	if mig.HasColumn(&tables.TableProviderJob{}, "params") {
		var captured int64
		if err := tx.Table("batch_jobs").Where("params IS NOT NULL AND params <> ''").Count(&captured).Error; err != nil {
			return fmt.Errorf("failed to check for captured provider job params: %w", err)
		}
		if captured > 0 {
			return fmt.Errorf("add_provider_job_kind_columns is non-rollbackable while %d job(s) in batch_jobs carry captured params: dropping params discards the pricing basis recorded at submission, which no provider response can reconstruct, and those jobs would settle unpriced; clear the column first if the rollback is genuinely intended", captured)
		}
	}

	if err := tx.Exec(`DROP INDEX IF EXISTS idx_batch_jobs_identity_v2`).Error; err != nil {
		return fmt.Errorf("failed to drop idx_batch_jobs_identity_v2: %w", err)
	}
	if err := tx.Exec(`DROP INDEX IF EXISTS idx_batch_jobs_sweeper_v2`).Error; err != nil {
		return fmt.Errorf("failed to drop idx_batch_jobs_sweeper_v2: %w", err)
	}

	// Raw DDL rather than Migrator.DropColumn. On sqlite the GORM helper rebuilds
	// the table from the *model*, and after an ALTER TABLE ADD COLUMN the physical
	// column order no longer matches the model's field order — the rebuild then
	// copies values into the wrong columns and dies on a NOT NULL violation. Both
	// dialects support DROP COLUMN natively (sqlite since 3.35), so drop in place.
	// DROP COLUMN IF EXISTS is postgres-only syntax, hence the HasColumn guard.
	for _, column := range []string{"kind", "params"} {
		if !mig.HasColumn(&tables.TableProviderJob{}, column) {
			continue
		}
		if err := tx.Exec(`ALTER TABLE batch_jobs DROP COLUMN ` + column).Error; err != nil {
			return fmt.Errorf("failed to drop %s column from batch_jobs: %w", column, err)
		}
	}

	if err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_batch_jobs_identity ON batch_jobs (provider, batch_id)`).Error; err != nil {
		return fmt.Errorf("failed to restore idx_batch_jobs_identity: %w", err)
	}
	if err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_batch_jobs_sweeper ON batch_jobs (provider, accounting_status, next_check_at)`).Error; err != nil {
		return fmt.Errorf("failed to restore idx_batch_jobs_sweeper: %w", err)
	}
	return nil
}

// postgresIndexIsValid reports whether an index exists on table and is usable.
// A missing index and an INVALID one both report false: the caller wants to know
// "can I rely on this", and an INVALID index — the residue of an interrupted
// CREATE INDEX CONCURRENTLY — answers no while still occupying the name.
// Only the table visible on the current search_path counts: a same-named table
// in another schema of the same database must not make a missing index look present.
func postgresIndexIsValid(tx *gorm.DB, table, index string) (bool, error) {
	var valid bool
	err := tx.Raw(`
		SELECT COALESCE(bool_and(pi.indisvalid), false)
		FROM pg_class pc
		JOIN pg_index pi ON pi.indrelid = pc.oid
		JOIN pg_class ic ON ic.oid = pi.indexrelid
		WHERE pc.relname = ? AND ic.relname = ? AND pg_catalog.pg_table_is_visible(pc.oid)
	`, table, index).Scan(&valid).Error
	return valid, err
}

// migrationAddDatabricksKeyConfigColumns adds the Databricks per-key columns: the workspace
// URL, the OAuth M2M service principal credentials, the inference surface selector, and the
// AI Gateway request-tag opt-in.
func migrationAddDatabricksKeyConfigColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_databricks_key_config_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	columns := []string{
		"databricks_workspace_url",
		"databricks_client_id",
		"databricks_client_secret",
		"databricks_api_format",
		"databricks_forward_gateway_tags",
	}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, col := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, col); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, col := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, col); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running databricks key config columns migration: %s", err.Error())
	}
	return nil
}

// oauthKeyConfigColumns are the oauth_key_config columns on the key table.
var oauthKeyConfigColumns = []string{
	"oauth_token_url",
	"oauth_client_id",
	"oauth_client_secret",
	"oauth_private_key",
	"oauth_settings_json",
}

// migrationAddOAuthKeyConfigColumns adds the oauth_key_config columns to the key table. There
// is nothing to backfill: no existing key carries an OAuth credential.
func migrationAddOAuthKeyConfigColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_oauth_key_config_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)

	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if tx.Dialector.Name() == "postgres" {
				// ADD COLUMN takes ACCESS EXCLUSIVE on config_keys. During a rolling upgrade a
				// long transaction can hold the table, and an unbounded wait parks this statement
				// at the head of the lock queue where every later key read and write waits behind
				// it. Bounding the wait fails this boot's migration instead, and the next boot
				// retries it. SET LOCAL reverts with the migration's transaction.
				if err := tx.Exec("SET LOCAL lock_timeout = '5s'").Error; err != nil {
					return fmt.Errorf("failed to bound the config_keys lock wait: %w", err)
				}
			}
			for _, column := range oauthKeyConfigColumns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, column); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: rollbackOAuthKeyConfigColumns,
	}})

	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// rollbackOAuthKeyConfigColumns refuses rather than dropping: oauth_private_key holds signing
// keys an identity provider may have issued once. The columns are additive, so an older binary ignores them.
func rollbackOAuthKeyConfigColumns(*gorm.DB) error {
	return fmt.Errorf("add_oauth_key_config_columns is non-rollbackable: dropping the oauth_* columns would permanently delete stored OAuth client secrets and private keys; the columns are additive and older binaries safely ignore them")
}

// migrationAddHiddenRequestTypesJSONColumn adds the hidden_request_types_json column to
// config_client so hidden request types can be edited from the UI as well as config.json.
// Existing rows get an empty list; a value in config.json's client section reconciles in on boot.
func migrationAddHiddenRequestTypesJSONColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_hidden_request_types_json_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "HiddenRequestTypesJSON"); err != nil {
				return fmt.Errorf("failed to add hidden_request_types_json column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "hidden_request_types_json"); err != nil {
				return fmt.Errorf("failed to drop hidden_request_types_json column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddUseOpenAIEndpointsColumn adds the use_openai_endpoints column to the config_keys table.
func migrationAddUseOpenAIEndpointsColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_use_openai_endpoints_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "use_openai_endpoints"); err != nil {
				return fmt.Errorf("failed to add use_openai_endpoints column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "use_openai_endpoints"); err != nil {
				return fmt.Errorf("failed to drop use_openai_endpoints column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("failed to run migration %s: %w", migrationName, err)
	}
	return nil
}

// migrationAddTimeOfDayPricingColumns adds the peak/off-peak pricing columns to
// governance_model_pricing. Providers such as DeepSeek bill the same model at
// two different rates depending on the time of day; off_peak_cost_multiplier
// scales usage-based charges outside the windows declared in peak_hours.
func migrationAddTimeOfDayPricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_time_of_day_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	columns := []string{
		"off_peak_cost_multiplier",
		"peak_hours",
	}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationMigrateVKStandaloneLimitsToModelConfigs cleans up the bad state
// produced by the config.json standalone-limits flow when combined with UI edits:
//
//  1. Standalone budgets (virtual_key_id set, model_config_id nil) owned by a VK
//     are re-pointed into the VK's top-level model config (scope=virtual_key,
//     model_name=*), creating that model config if it doesn't exist yet. Usage
//     (current_usage, last_reset) is preserved — only the ownership FK moves.
//
//  2. Orphaned duplicate rate limit rows created by the UI (UUID IDs, referenced
//     by a VK-scoped model config) are deleted and the model config is re-pointed
//     to the original config.json-created rate limit (the one still on vk.rate_limit_id),
//     preserving its usage counters. The VK's rate_limit_id column is then cleared
//     since ownership now lives on the model config.
func migrationMigrateVKStandaloneLimitsToModelConfigs(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "migrate_vk_standalone_limits_to_model_configs"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)

	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// --- Part 1: adopt standalone VK budgets into VK-scoped model configs ---

			// Find all budgets owned directly by a VK (old config.json flow).
			type standaloneVKBudget struct {
				ID            string
				VirtualKeyID  string
				MaxLimit      float64
				ResetDuration string
				CurrentUsage  float64
				LastReset     time.Time
				ConfigHash    string
			}
			var standaloneBudgets []standaloneVKBudget
			if err := tx.Raw(`
				SELECT id, virtual_key_id, max_limit, reset_duration, current_usage, last_reset, config_hash
				FROM governance_budgets
				WHERE virtual_key_id IS NOT NULL AND model_config_id IS NULL
			`).Scan(&standaloneBudgets).Error; err != nil {
				return fmt.Errorf("failed to query standalone VK budgets: %w", err)
			}

			// Group by VK ID.
			budgetsByVK := make(map[string][]standaloneVKBudget)
			for _, b := range standaloneBudgets {
				budgetsByVK[b.VirtualKeyID] = append(budgetsByVK[b.VirtualKeyID], b)
			}

			for vkID, budgets := range budgetsByVK {
				// Look up whether this VK has a direct rate_limit_id that also needs moving.
				var vkRateLimitID *string
				if err := tx.Raw(`SELECT rate_limit_id FROM governance_virtual_keys WHERE id = ?`, vkID).Scan(&vkRateLimitID).Error; err != nil {
					return fmt.Errorf("failed to query VK %s rate_limit_id: %w", vkID, err)
				}

				// Find or create the VK's top-level model config (scope=virtual_key, model_name=*).
				var mcID string
				err := tx.Raw(`
					SELECT id FROM governance_model_configs
					WHERE scope = 'virtual_key' AND scope_id = ? AND model_name = '*' AND provider IS NULL
					LIMIT 1
				`, vkID).Scan(&mcID).Error
				if err != nil {
					return fmt.Errorf("failed to query model config for VK %s: %w", vkID, err)
				}

				if mcID == "" {
					mcID = uuid.NewString()
					now := time.Now()
					if err := tx.Exec(`
						INSERT INTO governance_model_configs (id, model_name, scope, scope_id, created_at, updated_at)
						VALUES (?, '*', 'virtual_key', ?, ?, ?)
					`, mcID, vkID, now, now).Error; err != nil {
						return fmt.Errorf("failed to create model config for VK %s: %w", vkID, err)
					}
					logger.Info("[configstore] %s: created model config %s for VK %s", migrationName, mcID, vkID)
				}

				// If the VK has a rate limit not yet on the MC, move it now so Part 3's
				// NOT EXISTS doesn't skip this VK (Part 3 excludes VKs that already have an MC).
				if vkRateLimitID != nil {
					res := tx.Exec(`
						UPDATE governance_model_configs SET rate_limit_id = ? WHERE id = ? AND rate_limit_id IS NULL
					`, *vkRateLimitID, mcID)
					if res.Error != nil {
						return fmt.Errorf("failed to set rate_limit_id on model config %s: %w", mcID, res.Error)
					}
					// Only drop the VK reference when the model config took ownership.
					// Otherwise leave it so Part 2 can pick the canonical row and delete the duplicate.
					if res.RowsAffected > 0 {
						if err := tx.Exec(`UPDATE governance_virtual_keys SET rate_limit_id = NULL WHERE id = ?`, vkID).Error; err != nil {
							return fmt.Errorf("failed to clear VK %s rate_limit_id: %w", vkID, err)
						}
						logger.Info("[configstore] %s: moved rate limit %s from VK %s onto model config %s", migrationName, *vkRateLimitID, vkID, mcID)
					}
				}

				// Re-point each budget's ownership from virtual_key_id to model_config_id.
				ids := make([]string, len(budgets))
				for i, b := range budgets {
					ids[i] = b.ID
				}
				if err := tx.Exec(`
					UPDATE governance_budgets
					SET model_config_id = ?, virtual_key_id = NULL
					WHERE id IN ? AND virtual_key_id = ?
				`, mcID, ids, vkID).Error; err != nil {
					return fmt.Errorf("failed to re-point budgets for VK %s: %w", vkID, err)
				}
				logger.Info("[configstore] %s: re-pointed %d budget(s) for VK %s into model config %s", migrationName, len(ids), vkID, mcID)
			}

			// --- Part 2: deduplicate rate limit rows ---
			// Find VK-scoped model configs that have a rate_limit_id, where the owning
			// VK also has a different rate_limit_id set directly (the config.json row).
			type dupRateLimit struct {
				MCID          string
				MCRateLimitID string
				VKID          string
				VKRateLimitID string
			}
			var dups []dupRateLimit
			if err := tx.Raw(`
				SELECT mc.id AS mc_id, mc.rate_limit_id AS mc_rate_limit_id,
				       vk.id AS vk_id, vk.rate_limit_id AS vk_rate_limit_id
				FROM governance_model_configs mc
				JOIN governance_virtual_keys vk ON vk.id = mc.scope_id
				WHERE mc.scope = 'virtual_key'
				  AND mc.model_name = '*'
				  AND mc.provider IS NULL
				  AND mc.rate_limit_id IS NOT NULL
				  AND vk.rate_limit_id IS NOT NULL
				  AND mc.rate_limit_id != vk.rate_limit_id
			`).Scan(&dups).Error; err != nil {
				return fmt.Errorf("failed to query duplicate rate limits: %w", err)
			}

			for _, d := range dups {
				// Re-point the model config to the canonical config.json row (vk.rate_limit_id),
				// preserving its usage. The UUID row (mc.rate_limit_id) is the orphan to delete.
				orphanID := d.MCRateLimitID
				canonicalID := d.VKRateLimitID

				if err := tx.Exec(`
					UPDATE governance_model_configs SET rate_limit_id = ? WHERE id = ?
				`, canonicalID, d.MCID).Error; err != nil {
					return fmt.Errorf("failed to re-point model config %s to canonical rate limit: %w", d.MCID, err)
				}

				// Delete the orphaned UUID rate limit row.
				if err := tx.Exec(`DELETE FROM governance_rate_limits WHERE id = ?`, orphanID).Error; err != nil {
					return fmt.Errorf("failed to delete orphaned rate limit %s: %w", orphanID, err)
				}
				logger.Info("[configstore] %s: VK %s — replaced orphan rate limit %s with canonical %s", migrationName, d.VKID, orphanID, canonicalID)

				// Clear vk.rate_limit_id — ownership now lives on the model config.
				if err := tx.Exec(`UPDATE governance_virtual_keys SET rate_limit_id = NULL WHERE id = ?`, d.VKID).Error; err != nil {
					return fmt.Errorf("failed to clear VK %s rate_limit_id: %w", d.VKID, err)
				}
			}

			// For VKs that only have rate_limit_id set directly (no model config yet
			// and no duplicate), move the reference onto a model config so future UI
			// edits use the single-row path.
			type vkOnlyRL struct {
				VKID          string
				VKRateLimitID string
			}
			var vkOnlyRLs []vkOnlyRL
			if err := tx.Raw(`
				SELECT vk.id AS vk_id, vk.rate_limit_id AS vk_rate_limit_id
				FROM governance_virtual_keys vk
				WHERE vk.rate_limit_id IS NOT NULL
				  AND NOT EXISTS (
					SELECT 1 FROM governance_model_configs mc
					WHERE mc.scope = 'virtual_key' AND mc.scope_id = vk.id
					  AND mc.model_name = '*' AND mc.provider IS NULL
				  )
			`).Scan(&vkOnlyRLs).Error; err != nil {
				return fmt.Errorf("failed to query VKs with standalone rate limits: %w", err)
			}

			for _, v := range vkOnlyRLs {
				mcID := uuid.NewString()
				now := time.Now()
				if err := tx.Exec(`
					INSERT INTO governance_model_configs (id, model_name, scope, scope_id, rate_limit_id, created_at, updated_at)
					VALUES (?, '*', 'virtual_key', ?, ?, ?, ?)
				`, mcID, v.VKID, v.VKRateLimitID, now, now).Error; err != nil {
					return fmt.Errorf("failed to create model config for VK %s: %w", v.VKID, err)
				}
				if err := tx.Exec(`UPDATE governance_virtual_keys SET rate_limit_id = NULL WHERE id = ?`, v.VKID).Error; err != nil {
					return fmt.Errorf("failed to clear VK %s rate_limit_id: %w", v.VKID, err)
				}
				logger.Info("[configstore] %s: moved rate limit %s from VK %s into new model config %s", migrationName, v.VKRateLimitID, v.VKID, mcID)
			}

			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationMoveConfigJSONVKLimitsToModelConfigs moves directly owned virtual key budgets and rate limits onto the key's model config tiers.
func migrationMoveConfigJSONVKLimitsToModelConfigs(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "move_config_json_vk_limits_to_model_configs"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)

	// One transaction per tier, not one for all of them. Safe to re-run: only still-direct rows move.
	noTxOpts := *migrator.DefaultOptions
	noTxOpts.UseTransaction = false
	runErr := RunSingleMigration(ctx, &noTxOpts, db, logger, &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// Not a union: a typed NULL for the key tier's absent provider makes Postgres pad real values.
			type tier struct {
				VirtualKeyID string
				Provider     *string
			}
			var keyTiers []string
			if err := tx.Raw(`
				SELECT DISTINCT vk.id
				FROM governance_virtual_keys vk
				WHERE vk.rate_limit_id IS NOT NULL
				   OR EXISTS (
					SELECT 1 FROM governance_budgets b
					WHERE b.virtual_key_id = vk.id AND b.model_config_id IS NULL
				   )
			`).Scan(&keyTiers).Error; err != nil {
				return fmt.Errorf("failed to query virtual keys holding limits directly: %w", err)
			}
			var providerTiers []tier
			if err := tx.Raw(`
				SELECT DISTINCT pc.virtual_key_id AS virtual_key_id, pc.provider AS provider
				FROM governance_virtual_key_provider_configs pc
				JOIN governance_virtual_keys vk ON vk.id = pc.virtual_key_id
				WHERE pc.rate_limit_id IS NOT NULL
				   OR EXISTS (
					SELECT 1 FROM governance_budgets b
					WHERE b.provider_config_id = pc.id AND b.model_config_id IS NULL
				   )
			`).Scan(&providerTiers).Error; err != nil {
				return fmt.Errorf("failed to query provider configs holding limits directly: %w", err)
			}

			tiers := make([]tier, 0, len(keyTiers)+len(providerTiers))
			for _, vkID := range keyTiers {
				tiers = append(tiers, tier{VirtualKeyID: vkID})
			}
			tiers = append(tiers, providerTiers...)
			if len(tiers) == 0 {
				return nil
			}

			movedBudgets, movedRateLimits := 0, 0
			for _, t := range tiers {
				if err := tx.Transaction(func(txn *gorm.DB) error {
					return moveVKTierLimits(txn, t.VirtualKeyID, t.Provider, &movedBudgets, &movedRateLimits)
				}); err != nil {
					return err
				}
			}
			logger.Info("[configstore] %s: moved %d budget(s) and %d rate limit(s) onto %d virtual key model config tier(s)",
				migrationName, movedBudgets, movedRateLimits, len(tiers))
			return nil
		},
		Rollback: func(*gorm.DB) error {
			return fmt.Errorf("%s is non-rollbackable: it moves budget and rate-limit ownership onto model config tiers and does not record which owner each row came from, so the original ownership cannot be restored", migrationName)
		},
	})
	if runErr != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, runErr)
	}
	return nil
}

// moveVKTierLimits moves one tier's directly owned budgets and rate limit onto its model config.
func moveVKTierLimits(tx *gorm.DB, vkID string, provider *string, movedBudgets, movedRateLimits *int) error {
	mcID, err := ensureVKLimitModelConfig(tx, vkID, provider)
	if err != nil {
		return err
	}
	if mcID == "" {
		// The key disappeared between the scan and here.
		return nil
	}
	// Lock the provider configs before their budgets, the order DeleteVirtualKeyProviderConfig takes:
	// it locks the config then deletes the budgets, so moving the budgets first would deadlock it.
	// Ordered by id so two of these cannot take the same rows in opposite orders.
	if provider != nil {
		var providerConfigIDs []uint
		if err := dbForUpdate(tx).Table("governance_virtual_key_provider_configs").
			Where("virtual_key_id = ? AND provider = ?", vkID, *provider).
			Order("id").Pluck("id", &providerConfigIDs).Error; err != nil {
			return fmt.Errorf("failed to lock the provider configs of virtual key %s: %w", vkID, err)
		}
	}
	// Only the owning FK moves. Raw SQL skips BeforeSave, so clear the direct column here.
	var res *gorm.DB
	if provider == nil {
		res = tx.Exec(`
			UPDATE governance_budgets SET model_config_id = ?, virtual_key_id = NULL
			WHERE virtual_key_id = ? AND model_config_id IS NULL
		`, mcID, vkID)
	} else {
		res = tx.Exec(`
			UPDATE governance_budgets SET model_config_id = ?, provider_config_id = NULL
			WHERE model_config_id IS NULL AND provider_config_id IN (
				SELECT id FROM governance_virtual_key_provider_configs
				WHERE virtual_key_id = ? AND provider = ?
			)
		`, mcID, vkID, *provider)
	}
	if res.Error != nil {
		return fmt.Errorf("failed to move budgets of virtual key %s onto model config %s: %w", vkID, mcID, res.Error)
	}
	*movedBudgets += int(res.RowsAffected)

	moved, err := moveVKTierRateLimit(tx, vkID, provider, mcID)
	if err != nil {
		return err
	}
	*movedRateLimits += moved
	return nil
}

// ensureVKLimitModelConfig find-or-creates a key's all-models tier. "" when the key is gone.
func ensureVKLimitModelConfig(tx *gorm.DB, vkID string, provider *string) (string, error) {
	// Serialize against a concurrent DeleteVirtualKey, as CreateModelConfig does: scope_id carries no
	// foreign key, so without the lock a tier inserted here can outlive the key it is scoped to.
	var owner []string
	if err := dbForUpdate(tx).Table("governance_virtual_keys").
		Where("id = ?", vkID).Limit(1).Pluck("id", &owner).Error; err != nil {
		return "", fmt.Errorf("failed to lock virtual key %s: %w", vkID, err)
	}
	if len(owner) == 0 {
		return "", nil
	}
	q := tx.Table("governance_model_configs").
		Where("scope = ? AND scope_id = ? AND model_name = ?",
			tables.ModelConfigScopeVirtualKey, vkID, tables.ModelConfigAllModels)
	if provider == nil {
		q = q.Where("provider IS NULL")
	} else {
		q = q.Where("provider = ?", *provider)
	}
	var ids []string
	if err := q.Limit(1).Pluck("id", &ids).Error; err != nil {
		return "", fmt.Errorf("failed to query model config of virtual key %s: %w", vkID, err)
	}
	if len(ids) > 0 {
		return ids[0], nil
	}
	mcID := uuid.NewString()
	now := time.Now()
	res := tx.Exec(`
		INSERT INTO governance_model_configs (id, model_name, scope, scope_id, provider, calendar_aligned, created_at, updated_at)
		SELECT ?, ?, ?, vk.id, ?, vk.calendar_aligned, ?, ?
		FROM governance_virtual_keys vk WHERE vk.id = ?
	`, mcID, tables.ModelConfigAllModels, tables.ModelConfigScopeVirtualKey, provider, now, now, vkID)
	if res.Error != nil {
		return "", fmt.Errorf("failed to create model config for virtual key %s: %w", vkID, res.Error)
	}
	if res.RowsAffected == 0 {
		return "", nil
	}
	return mcID, nil
}

// moveVKTierRateLimit moves a directly referenced rate limit onto the tier. A tier that has one keeps it.
func moveVKTierRateLimit(tx *gorm.DB, vkID string, provider *string, mcID string) (int, error) {
	ownerTable, ownerWhere := "governance_virtual_keys", "id = ?"
	ownerArgs := []any{vkID}
	if provider != nil {
		ownerTable, ownerWhere = "governance_virtual_key_provider_configs", "virtual_key_id = ? AND provider = ?"
		ownerArgs = []any{vkID, *provider}
	}
	var rateLimitIDs []string
	if err := tx.Table(ownerTable).Where(ownerWhere, ownerArgs...).
		Where("rate_limit_id IS NOT NULL").Limit(1).Pluck("rate_limit_id", &rateLimitIDs).Error; err != nil {
		return 0, fmt.Errorf("failed to read rate limit of virtual key %s: %w", vkID, err)
	}
	if len(rateLimitIDs) == 0 {
		return 0, nil
	}
	res := tx.Exec(`
		UPDATE governance_model_configs SET rate_limit_id = ? WHERE id = ? AND rate_limit_id IS NULL
	`, rateLimitIDs[0], mcID)
	if res.Error != nil {
		return 0, fmt.Errorf("failed to attach rate limit %s to model config %s: %w", rateLimitIDs[0], mcID, res.Error)
	}
	// Only when the tier took it: dropping a reference this migration did not relocate would leave
	// the operator's row owned by nobody.
	if res.RowsAffected > 0 {
		if err := tx.Table(ownerTable).Where(ownerWhere, ownerArgs...).
			Update("rate_limit_id", nil).Error; err != nil {
			return 0, fmt.Errorf("failed to clear rate limit reference of virtual key %s: %w", vkID, err)
		}
	}
	return int(res.RowsAffected), nil
}

// migrationAddVirtualKeyBusinessUnitColumn adds business_unit_id to governance_virtual_keys, the
// third owner a key can have alongside a team and a customer. A business unit is an enterprise
// table this module does not model, so the column is a bare indexed varchar with no foreign key:
// what it points at is resolved by whoever owns business units, and a deployment without them
// simply never writes it.
func migrationAddVirtualKeyBusinessUnitColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_virtual_key_business_unit_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()
			if err := addColumnIfNotExists(tx, logger, &tables.TableVirtualKey{}, "business_unit_id"); err != nil {
				return fmt.Errorf("failed to add business_unit_id column: %w", err)
			}
			// Every request made with a key owned by a business unit walks this column, and AddColumn
			// does not create indexes from struct tags.
			if !mg.HasIndex(&tables.TableVirtualKey{}, "idx_governance_virtual_keys_business_unit_id") {
				if err := mg.CreateIndex(&tables.TableVirtualKey{}, "BusinessUnitID"); err != nil {
					return fmt.Errorf("failed to create index on governance_virtual_keys.business_unit_id: %w", err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			// The index belongs to the column and goes with it.
			return dropColumnIfExists(tx, logger, &tables.TableVirtualKey{}, "business_unit_id")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddVirtualKeyDisableContentLoggingColumn adds disable_content_logging to
// governance_virtual_keys: the key's own tri-state say on content logging. The column is nullable
// with no default because NULL is a meaning of its own (inherit the client setting), so every
// existing key comes out of the migration inheriting exactly as it did before.
func migrationAddVirtualKeyDisableContentLoggingColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_virtual_key_disable_content_logging_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableVirtualKey{}, "disable_content_logging"); err != nil {
				return fmt.Errorf("failed to add disable_content_logging column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return fmt.Errorf("add_virtual_key_disable_content_logging_column is non-rollbackable: dropping disable_content_logging would permanently delete every virtual key's content-logging decision and silently revert content-off keys to logging content; the column is additive and older binaries safely ignore it")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationAddTTFTTimeoutMsColumnToRoutingTargets adds the nullable ttft_timeout_ms
// column to routing_targets. Existing targets keep NULL (no TTFT deadline), and
// GenerateRoutingRuleHash only hashes the field when it is set, so no
// config_hash backfill is needed.
func migrationAddTTFTTimeoutMsColumnToRoutingTargets(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_ttft_timeout_ms_column_to_routing_targets"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableRoutingTarget{}, "ttft_timeout_ms"); err != nil {
				return fmt.Errorf("failed to add column ttft_timeout_ms: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableRoutingTarget{}, "ttft_timeout_ms"); err != nil {
				return fmt.Errorf("failed to drop column ttft_timeout_ms: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// migrationAddWebSearchCostPerRequestColumn adds web_search_cost_per_request, seeded from search_context_cost_per_query.
func migrationAddWebSearchCostPerRequestColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_web_search_cost_per_request_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, "web_search_cost_per_request"); err != nil {
				return fmt.Errorf("failed to add column web_search_cost_per_request: %w", err)
			}
			// Existing rows would bill web search at $0 until the next datasheet sync.
			if err := tx.Model(&tables.TableModelPricing{}).
				Where("web_search_cost_per_request IS NULL AND search_context_cost_per_query IS NOT NULL").
				Update("web_search_cost_per_request", gorm.Expr("search_context_cost_per_query")).Error; err != nil {
				return fmt.Errorf("failed to backfill web_search_cost_per_request: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, "web_search_cost_per_request"); err != nil {
				return fmt.Errorf("failed to drop column web_search_cost_per_request: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// migrationMovePricingOverrideSearchContextToWebSearch renames search_context_cost_per_query to web_search_cost_per_request in override patches.
func migrationMovePricingOverrideSearchContextToWebSearch(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "move_pricing_override_search_context_to_web_search"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			var rows []struct {
				ID               string
				PricingPatchJSON string
			}
			if err := tx.Table(tables.TablePricingOverride{}.TableName()).
				Select("id, pricing_patch_json").
				Where("pricing_patch_json LIKE ?", "%search_context_cost_per_query%").
				Scan(&rows).Error; err != nil {
				return fmt.Errorf("failed to load pricing overrides: %w", err)
			}
			for _, row := range rows {
				legacy := gjson.Get(row.PricingPatchJSON, "search_context_cost_per_query")
				if !legacy.Exists() {
					continue
				}
				patch := row.PricingPatchJSON
				var err error
				// An explicit web_search_cost_per_request wins over the legacy rate.
				if !gjson.Get(patch, "web_search_cost_per_request").Exists() {
					if patch, err = sjson.SetRaw(patch, "web_search_cost_per_request", legacy.Raw); err != nil {
						return fmt.Errorf("failed to set web_search_cost_per_request on override %s: %w", row.ID, err)
					}
				}
				if patch, err = sjson.Delete(patch, "search_context_cost_per_query"); err != nil {
					return fmt.Errorf("failed to drop search_context_cost_per_query on override %s: %w", row.ID, err)
				}
				if err := tx.Table(tables.TablePricingOverride{}.TableName()).
					Where("id = ?", row.ID).
					UpdateColumn("pricing_patch_json", patch).Error; err != nil {
					return fmt.Errorf("failed to update override %s: %w", row.ID, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// postgresIndexOnTable looks up index name among the indexes of table (the
// table visible on the current search_path). It returns the index's
// schema-qualified name and whether it is valid, or found=false when table has
// no index of that name. Index names are unique per schema, not per database, so
// a bare name can also resolve to another table's index or another schema's;
// callers drop by the qualified name returned here, never by the bare name.
func postgresIndexOnTable(tx *gorm.DB, table, name string) (qualified string, valid, found bool, err error) {
	var rows []struct {
		Qualified string
		Valid     bool
	}
	err = tx.Raw(`
		SELECT quote_ident(n.nspname) || '.' || quote_ident(ic.relname) AS qualified, pi.indisvalid AS valid
		FROM pg_class pc
		JOIN pg_index pi ON pi.indrelid = pc.oid
		JOIN pg_class ic ON ic.oid = pi.indexrelid
		JOIN pg_namespace n ON n.oid = ic.relnamespace
		WHERE pc.relname = ? AND ic.relname = ? AND pg_catalog.pg_table_is_visible(pc.oid)
	`, table, name).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return "", false, false, err
	}
	return rows[0].Qualified, rows[0].Valid, true, nil
}

// concurrentIndexBuildPollInterval is how often waitForPostgresIndexBuild
// re-checks a concurrent build that another backend is still running.
const concurrentIndexBuildPollInterval = 2 * time.Second

// waitForPostgresIndexBuild blocks while a backend is still running CREATE INDEX
// CONCURRENTLY (or REINDEX CONCURRENTLY) for index, and reports whether it waited.
//
// An index under a concurrent build reads indisvalid=false, the same as the
// leftover of an interrupted build, and that build can outlive the pod that
// started it: when a pod is killed, Postgres ends its idle advisory-lock session
// at once but keeps the busy build running until it next touches the socket. The
// next pod then holds the migration lock and sees what looks like a leftover.
// Dropping it blocks behind the live build and then fails one side with "tuple
// concurrently updated". Callers that drop an INVALID index call this first and
// re-check validity when it waited: a finished build is kept, and only an index
// still INVALID with no build running is dropped.
//
// Postgres hides index_relid in pg_stat_progress_create_index for backends of
// another role unless the caller has pg_read_all_stats. Bifrost nodes share one
// role; a hidden build reads as "not building" and keeps the drop-and-rebuild path.
func waitForPostgresIndexBuild(tx *gorm.DB, index string) (bool, error) {
	ctx := tx.Statement.Context
	if ctx == nil {
		ctx = context.Background()
	}
	for waited := false; ; waited = true {
		var building bool
		if err := tx.Raw(`SELECT EXISTS (SELECT 1 FROM pg_stat_progress_create_index
			WHERE index_relid = pg_catalog.to_regclass(?))`, index).Scan(&building).Error; err != nil {
			return waited, fmt.Errorf("check concurrent build of %s: %w", index, err)
		}
		if !building {
			return waited, nil
		}
		select {
		case <-ctx.Done():
			return waited, fmt.Errorf("wait for concurrent build of %s: %w", index, ctx.Err())
		case <-time.After(concurrentIndexBuildPollInterval):
		}
	}
}

// ensureIndexConcurrently creates index name on table with the given column
// definition without blocking writes for long. On postgres it runs CREATE
// [UNIQUE] INDEX CONCURRENTLY, which cannot run inside a transaction, so callers
// must use a migration with UseTransaction=false; a small table instead gets a
// plain build under a bounded lock wait (see PostgresTableIsSmall). An INVALID
// index left by an interrupted
// concurrent build still occupies the name and would make IF NOT EXISTS a silent
// no-op, so it is dropped and rebuilt; only an index on table itself is ever
// dropped, by its schema-qualified name. SQLite has no concurrent build and uses
// the plain form. Identifier arguments are migration-controlled constants, never input.
func ensureIndexConcurrently(tx *gorm.DB, table, name, definition string, unique bool) error {
	return ensurePartialIndexConcurrently(tx, table, name, definition, "", unique)
}

// ensurePartialIndexConcurrently is ensureIndexConcurrently with an optional
// predicate: a non-empty where builds a partial index (CREATE INDEX ... WHERE
// where), which both Postgres and SQLite support. An empty where builds a full
// index. The same no-transaction requirement applies on postgres.
func ensurePartialIndexConcurrently(tx *gorm.DB, table, name, definition, where string, unique bool) error {
	return ensureIndexConcurrentlyUsing(tx, table, name, "", definition, where, unique)
}

// ensureIndexConcurrentlyUsing is ensurePartialIndexConcurrently with an index access method
// (USING method, e.g. "hash"); "" is the default btree.
func ensureIndexConcurrentlyUsing(tx *gorm.DB, table, name, method, definition, where string, unique bool) error {
	kind := "INDEX"
	if unique {
		kind = "UNIQUE INDEX"
	}
	predicate := ""
	if where != "" {
		predicate = " WHERE " + where
	}
	on := table
	if method != "" {
		on += " USING " + method
	}
	if tx.Dialector.Name() != "postgres" {
		return tx.Exec(fmt.Sprintf("CREATE %s IF NOT EXISTS %s ON %s (%s)%s", kind, name, on, definition, predicate)).Error
	}
	qualified, valid, found, err := postgresIndexOnTable(tx, table, name)
	if err != nil {
		return fmt.Errorf("check index %s: %w", name, err)
	}
	if found && !valid {
		waited, err := waitForPostgresIndexBuild(tx, qualified)
		if err != nil {
			return err
		}
		if waited {
			if qualified, valid, found, err = postgresIndexOnTable(tx, table, name); err != nil {
				return fmt.Errorf("check index %s: %w", name, err)
			}
		}
	}
	if found && valid {
		return nil
	}
	// A small table gets a plain build under a bounded lock wait. CREATE INDEX
	// CONCURRENTLY waits for every transaction in the database older than it,
	// whatever table it touches, so on a database shared with a large log store a
	// tiny config table's index waits out the longest log-table transaction (a
	// materialized-view refresh can run for many minutes). A plain build only
	// takes a SHARE lock on this table, which on a small table is held for
	// milliseconds.
	small, err := PostgresTableIsSmall(tx, table)
	if err != nil {
		return fmt.Errorf("check size of %s: %w", table, err)
	}
	if small {
		stmts := make([]string, 0, 2)
		if found {
			stmts = append(stmts, fmt.Sprintf("DROP INDEX IF EXISTS %s", qualified))
		}
		stmts = append(stmts, fmt.Sprintf("CREATE %s IF NOT EXISTS %s ON %s (%s)%s", kind, name, on, definition, predicate))
		return ExecWithBoundedLock(tx, "index "+name, stmts...)
	}
	if found {
		if err := tx.Exec(fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS %s", qualified)).Error; err != nil {
			return fmt.Errorf("drop invalid index %s: %w", qualified, err)
		}
	}
	return tx.Exec(fmt.Sprintf("CREATE %s CONCURRENTLY IF NOT EXISTS %s ON %s (%s)%s", kind, name, on, definition, predicate)).Error
}

// smallTableIndexMaxBytes and smallTableIndexMaxRows decide when an index is
// built without CONCURRENTLY (see postgresconn.TableIsSmall). Variables only so
// tests can force the concurrent path.
var (
	smallTableIndexMaxBytes = postgresconn.DefaultSmallTableMaxBytes
	smallTableIndexMaxRows  = postgresconn.DefaultSmallTableMaxRows
)

// PostgresTableIsSmall reports whether table is small enough to index without
// CONCURRENTLY, under this package's thresholds. Exported so downstream migration
// helpers (bifrost-enterprise) apply the same rule; see postgresconn.TableIsSmall.
func PostgresTableIsSmall(tx *gorm.DB, table string) (bool, error) {
	return postgresconn.TableIsSmall(tx, table, smallTableIndexMaxBytes, smallTableIndexMaxRows)
}

// ExecWithBoundedLock runs stmts in one short transaction with a bounded lock
// wait and statement time, retrying lock timeouts. Kept exported for downstream
// callers; see postgresconn.ExecWithBoundedLock.
func ExecWithBoundedLock(tx *gorm.DB, label string, stmts ...string) error {
	return postgresconn.ExecWithBoundedLock(tx, label, stmts...)
}

// dropIndexConcurrently is the rollback counterpart of ensureIndexConcurrently: it
// drops index name on table without blocking writes on postgres, by its
// schema-qualified name and only when table has it, and with the plain form on SQLite.
func dropIndexConcurrently(tx *gorm.DB, table, name string) error {
	if tx.Dialector.Name() != "postgres" {
		return tx.Exec(fmt.Sprintf("DROP INDEX IF EXISTS %s", name)).Error
	}
	qualified, _, found, err := postgresIndexOnTable(tx, table, name)
	if err != nil {
		return fmt.Errorf("check index %s: %w", name, err)
	}
	if !found {
		return nil
	}
	return tx.Exec(fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS %s", qualified)).Error
}

// migrationAddVKProviderConfigVirtualKeyIDIndex indexes
// governance_virtual_key_provider_configs.virtual_key_id. Every provider-config
// preload, per-VK lookup, and the ON DELETE CASCADE check of every virtual key
// delete filters on this column; without an index each one scans the whole table
// (100k-500k rows at 100k keys). Built concurrently so the upgrade never blocks writes.
func migrationAddVKProviderConfigVirtualKeyIDIndex(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_vk_provider_config_virtual_key_id_index"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	noTxOpts := *migrator.DefaultOptions
	noTxOpts.UseTransaction = false
	if err := RunSingleMigration(ctx, &noTxOpts, db, logger, vkProviderConfigVirtualKeyIDIndexMigration(ctx, migrationName)); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// vkProviderConfigVirtualKeyIDIndexMigration builds the migration applied by
// migrationAddVKProviderConfigVirtualKeyIDIndex, so tests exercise the same
// Migrate and Rollback callbacks the upgrade runs.
func vkProviderConfigVirtualKeyIDIndexMigration(ctx context.Context, id string) *migrator.Migration {
	return &migrator.Migration{
		ID: id,
		Migrate: func(tx *gorm.DB) error {
			return ensureIndexConcurrently(tx.WithContext(ctx), "governance_virtual_key_provider_configs",
				"idx_vk_provider_configs_virtual_key_id", "virtual_key_id", false)
		},
		Rollback: func(tx *gorm.DB) error {
			return dropIndexConcurrently(tx.WithContext(ctx), "governance_virtual_key_provider_configs", "idx_vk_provider_configs_virtual_key_id")
		},
	}
}

// hashIndexSpec describes one credential-hash column whose tag-declared UNIQUE
// index upgraded databases never received.
type hashIndexSpec struct {
	table  string
	column string
	unique string
}

// hashIndexSpecs lists the hash columns migrationAddEncryptionColumns added with
// addColumnIfNotExists, which adds the column but never builds its tag index.
var hashIndexSpecs = []hashIndexSpec{
	{table: "governance_virtual_keys", column: "value_hash", unique: "idx_virtual_key_value_hash"},
	{table: "sessions", column: "token_hash", unique: "idx_session_token_hash"},
}

// hashIndexExists reports whether index name exists and is usable on table.
func hashIndexExists(tx *gorm.DB, table, name string) (bool, error) {
	if tx.Dialector.Name() == "postgres" {
		return postgresIndexIsValid(tx, table, name)
	}
	var n int64
	err := tx.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = ? AND name = ?", table, name).Scan(&n).Error
	return n > 0, err
}

// countDuplicateHashes returns how many distinct non-NULL values of column occur
// more than once in table, which is what would make a UNIQUE build fail.
func countDuplicateHashes(tx *gorm.DB, table, column string) (int64, error) {
	var n int64
	err := tx.Raw(fmt.Sprintf(
		"SELECT COUNT(*) FROM (SELECT %s FROM %s WHERE %s IS NOT NULL GROUP BY %s HAVING COUNT(*) > 1) dup",
		column, table, column, column)).Scan(&n).Error
	return n, err
}

// hashNormalizeBatchSize bounds how many rows one empty-hash normalization
// statement touches. A variable only so tests can exercise several batches.
var hashNormalizeBatchSize = 10000

// normalizeEmptyHashes sets column to NULL wherever it is the empty string,
// walking the primary key in batches of hashNormalizeBatchSize so no single
// statement locks every matching row. Each batch updates one id range and
// commits on its own (the migration runs without a transaction). The cursor
// keeps the id's own type, since the tables it runs on key by string (virtual
// keys) and by integer (sessions). Identifiers are migration constants.
func normalizeEmptyHashes(tx *gorm.DB, table, column string) error {
	var cursor any
	for {
		selectSQL := fmt.Sprintf("SELECT id FROM %s WHERE %s = ''", table, column)
		args := []any{}
		if cursor != nil {
			selectSQL += " AND id > ?"
			args = append(args, cursor)
		}
		selectSQL += " ORDER BY id LIMIT ?"
		args = append(args, hashNormalizeBatchSize)
		rows, err := tx.Raw(selectSQL, args...).Rows()
		if err != nil {
			return err
		}
		var ids []any
		for rows.Next() {
			var id any
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			if b, ok := id.([]byte); ok {
				id = string(b)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		last := ids[len(ids)-1]
		updateSQL := fmt.Sprintf("UPDATE %s SET %s = NULL WHERE %s = '' AND id <= ?", table, column, column)
		updateArgs := []any{last}
		if cursor != nil {
			updateSQL += " AND id > ?"
			updateArgs = append(updateArgs, cursor)
		}
		if err := tx.Exec(updateSQL, updateArgs...).Error; err != nil {
			return err
		}
		cursor = last
		if len(ids) < hashNormalizeBatchSize {
			return nil
		}
	}
}

// ensureHashIndex builds the UNIQUE lookup index for one hash column, or a
// non-unique fallback when existing duplicates make UNIQUE impossible. The
// fallback keeps auth lookups indexed without blocking startup; the duplicates
// themselves are left for an operator, and the warning names the table and count.
// An empty-string hash is normalized to NULL first (the value migrationAddEncryptionColumns chose):
// a row without a value has no hash, and NULLs never collide in a UNIQUE index.
func ensureHashIndex(tx *gorm.DB, logger schemas.Logger, spec hashIndexSpec) error {
	exists, err := hashIndexExists(tx, spec.table, spec.unique)
	if err != nil {
		return fmt.Errorf("check index %s: %w", spec.unique, err)
	}
	if exists {
		return nil
	}
	if err := normalizeEmptyHashes(tx, spec.table, spec.column); err != nil {
		return fmt.Errorf("normalize empty %s.%s: %w", spec.table, spec.column, err)
	}
	dups, err := countDuplicateHashes(tx, spec.table, spec.column)
	if err != nil {
		return fmt.Errorf("count duplicate %s.%s: %w", spec.table, spec.column, err)
	}
	if dups == 0 {
		buildErr := ensureIndexConcurrently(tx, spec.table, spec.unique, spec.column, true)
		if buildErr == nil {
			return nil
		}
		// A concurrent writer can add a duplicate between the count and the build.
		// Recount: only a real duplicate justifies the fallback, anything else is a
		// genuine failure the migration must report.
		dups, err = countDuplicateHashes(tx, spec.table, spec.column)
		if err != nil || dups == 0 {
			return fmt.Errorf("create unique index %s: %w", spec.unique, buildErr)
		}
		if err := dropIndexConcurrently(tx, spec.table, spec.unique); err != nil {
			return fmt.Errorf("drop failed unique index %s: %w", spec.unique, err)
		}
	}
	nonUnique := spec.unique + "_nonunique"
	logger.Warn("[configstore] %s has %d duplicated %s values; building non-unique index %s instead of unique %s. Remove the duplicates and rebuild %s as UNIQUE.",
		spec.table, dups, spec.column, nonUnique, spec.unique, spec.unique)
	return ensureIndexConcurrently(tx, spec.table, nonUnique, spec.column, false)
}

// migrationAddValueHashAndTokenHashIndexes builds the lookup indexes that the
// struct tags declare on governance_virtual_keys.value_hash and
// sessions.token_hash. Upgraded databases got both columns from
// migrationAddEncryptionColumns, whose addColumnIfNotExists never builds tag
// indexes, so every VK auth by hash and every session lookup scanned the whole
// table. Both indexes are UNIQUE, matching fresh installs; if existing duplicate
// hashes make that impossible, a non-unique *_nonunique index is built instead and
// a warning is logged, so a data problem never blocks startup. Built concurrently.
func migrationAddValueHashAndTokenHashIndexes(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_value_hash_and_token_hash_indexes"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	noTxOpts := *migrator.DefaultOptions
	noTxOpts.UseTransaction = false
	if err := RunSingleMigration(ctx, &noTxOpts, db, logger, valueHashAndTokenHashIndexesMigration(ctx, migrationName, logger)); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// valueHashAndTokenHashIndexesMigration builds the migration applied by
// migrationAddValueHashAndTokenHashIndexes, so tests exercise its callbacks.
func valueHashAndTokenHashIndexesMigration(ctx context.Context, migrationName string, logger schemas.Logger) *migrator.Migration {
	return &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, spec := range hashIndexSpecs {
				if err := ensureHashIndex(tx, logger, spec); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(*gorm.DB) error {
			return fmt.Errorf("%s is non-rollbackable: on a fresh install the struct tags build these unique lookup indexes before the migration runs, and the ledger cannot tell which indexes it created, so dropping them could remove the schema's own auth lookup indexes", migrationName)
		},
	}
}

// migrationAddVKProviderConfigProviderAndKeyIndexes indexes
// governance_virtual_key_provider_configs.provider and
// governance_virtual_key_provider_config_keys.table_key_id. Provider updates and
// deletes find every VK config of one provider, and key removal finds every join
// row of one key; table_key_id is only the second column of the join table's
// primary key, so neither lookup had a usable index.
func migrationAddVKProviderConfigProviderAndKeyIndexes(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_vk_provider_config_provider_and_key_indexes"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	noTxOpts := *migrator.DefaultOptions
	noTxOpts.UseTransaction = false
	if err := RunSingleMigration(ctx, &noTxOpts, db, logger, &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := ensureIndexConcurrently(tx, "governance_virtual_key_provider_configs",
				"idx_vk_provider_configs_provider", "provider", false); err != nil {
				return err
			}
			return ensureIndexConcurrently(tx, "governance_virtual_key_provider_config_keys",
				"idx_vkpc_keys_table_key_id", "table_key_id", false)
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropIndexConcurrently(tx, "governance_virtual_key_provider_configs", "idx_vk_provider_configs_provider"); err != nil {
				return err
			}
			return dropIndexConcurrently(tx, "governance_virtual_key_provider_config_keys", "idx_vkpc_keys_table_key_id")
		},
	}); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddVirtualKeysCreatedAtIDIndex indexes governance_virtual_keys on
// (created_at, id), the exact keyset the all-VK loaders page by. With only the
// single-column created_at index, each page re-read the index from its start and
// filtered, so loading N keys cost O(N^2) index reads.
func migrationAddVirtualKeysCreatedAtIDIndex(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_virtual_keys_created_at_id_index"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	noTxOpts := *migrator.DefaultOptions
	noTxOpts.UseTransaction = false
	if err := RunSingleMigration(ctx, &noTxOpts, db, logger, &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			return ensureIndexConcurrently(tx.WithContext(ctx), "governance_virtual_keys",
				"idx_virtual_keys_created_at_id", "created_at, id", false)
		},
		Rollback: func(tx *gorm.DB) error {
			return dropIndexConcurrently(tx.WithContext(ctx), "governance_virtual_keys", "idx_virtual_keys_created_at_id")
		},
	}); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// batchJobsDueBackfillBatchSize is how many batch_jobs rows one backfill UPDATE
// touches. A variable only so tests can exercise several batches on a few rows.
var batchJobsDueBackfillBatchSize = 10000

// backfillTerminalBatchJobsNextCheckAt clears next_check_at on jobs already in a
// terminal accounting status, walking the primary key in batches so no single
// statement locks the whole table. Each batch commits on its own (the migration
// runs without a transaction); re-running is safe because the WHERE clause only
// matches rows that still need clearing.
func backfillTerminalBatchJobsNextCheckAt(tx *gorm.DB) error {
	terminal := []string{tables.ProviderJobAccountingStatusAccounted, tables.ProviderJobAccountingStatusUnpriceable}
	cursor := ""
	for {
		var ids []string
		if err := tx.Table("batch_jobs").
			Where("id > ? AND next_check_at IS NOT NULL AND accounting_status IN ?", cursor, terminal).
			Order("id ASC").
			Limit(batchJobsDueBackfillBatchSize).
			Pluck("id", &ids).Error; err != nil {
			return fmt.Errorf("select terminal batch_jobs after %q: %w", cursor, err)
		}
		if len(ids) == 0 {
			return nil
		}
		inClause, arg := queryscope.InStrings(tx, "id", ids)
		if err := tx.Exec("UPDATE batch_jobs SET next_check_at = NULL WHERE "+inClause+" AND accounting_status IN ?", arg, terminal).Error; err != nil {
			return fmt.Errorf("clear next_check_at on terminal batch_jobs: %w", err)
		}
		cursor = ids[len(ids)-1]
		if len(ids) < batchJobsDueBackfillBatchSize {
			return nil
		}
	}
}

// migrationAddBatchJobsDueIndex keeps the sweeper's due-job scan proportional to
// the jobs that can still be due. finishProviderJob used to leave next_check_at
// set on accounted and unpriceable jobs, so ListDueProviderJobs re-read every
// finished job ever recorded on each sweep. This clears next_check_at on existing
// terminal rows (in primary-key batches) and builds the partial index
// idx_batch_jobs_due (kind, next_check_at) WHERE next_check_at IS NOT NULL, which
// then only holds live jobs. Built concurrently.
func migrationAddBatchJobsDueIndex(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_batch_jobs_due_index"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	noTxOpts := *migrator.DefaultOptions
	noTxOpts.UseTransaction = false
	if err := RunSingleMigration(ctx, &noTxOpts, db, logger, batchJobsDueIndexMigration(ctx, migrationName)); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// batchJobsDueIndexMigration builds the migration applied by
// migrationAddBatchJobsDueIndex, so tests exercise its callbacks.
func batchJobsDueIndexMigration(ctx context.Context, migrationName string) *migrator.Migration {
	return &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := backfillTerminalBatchJobsNextCheckAt(tx); err != nil {
				return err
			}
			return ensurePartialIndexConcurrently(tx, "batch_jobs", "idx_batch_jobs_due",
				"kind, next_check_at", "next_check_at IS NOT NULL", false)
		},
		Rollback: func(*gorm.DB) error {
			return fmt.Errorf("%s is non-rollbackable: it cleared next_check_at on accounted and unpriceable batch jobs, which dropping idx_batch_jobs_due cannot restore", migrationName)
		},
	}
}

// migrationAddVertexAWSWorkloadIdentityColumn adds the vertex_aws_workload_identity_json column to
// the config_keys table. It holds the JSON-serialized aws_workload_identity block of a Vertex key
// (GCP Workload Identity Federation from an AWS identity), encrypted like the other key secrets.
func migrationAddVertexAWSWorkloadIdentityColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_vertex_aws_workload_identity_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "vertex_aws_workload_identity_json"); err != nil {
				return fmt.Errorf("failed to add vertex_aws_workload_identity_json column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return rollbackVertexAWSWorkloadIdentityColumn(ctx, tx, logger)
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running vertex aws workload identity column migration: %s", err.Error())
	}
	return nil
}

// rollbackVertexAWSWorkloadIdentityColumn is the down path of add_vertex_aws_workload_identity_column.
// It refuses: the column holds operator-entered Vertex federation configuration, so dropping it would
// permanently delete those keys' authentication settings. The column is additive and older binaries
// safely ignore it, which is the same contract the other non-rollbackable operator-settings columns use.
func rollbackVertexAWSWorkloadIdentityColumn(context.Context, *gorm.DB, schemas.Logger) error {
	return fmt.Errorf("add_vertex_aws_workload_identity_column is non-rollbackable: dropping vertex_aws_workload_identity_json would permanently delete the AWS workload identity configuration of every Vertex key that uses it; the column is additive and older binaries safely ignore it")
}

// migrationAddPriorityAbove272kCacheCreationPricingColumn adds the OpenAI
// Priority/Fast cache-write rate for prompts above 272k tokens. The input,
// output, and cache-read >272k priority columns already exist; this is the
// one the datasheet publishes that the table had no home for. Nullable so
// catalogs without it keep the flat priority cache-write rate as the fallback.
func migrationAddPriorityAbove272kCacheCreationPricingColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_priority_above_272k_cache_creation_pricing_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	column := "cache_creation_input_token_cost_above_272k_tokens_priority"
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, column); err != nil {
				return fmt.Errorf("failed to add column %s: %w", column, err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, column); err != nil {
				return fmt.Errorf("failed to drop column %s: %w", column, err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// migrationAddCompatForceReasoningOnlyModelsToResponsesColumn adds compat_force_reasoning_only_models_to_responses
// to config_client.
func migrationAddCompatForceReasoningOnlyModelsToResponsesColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_compat_force_reasoning_only_models_to_responses_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "CompatForceReasoningOnlyModelsToResponses"); err != nil {
				return fmt.Errorf("failed to add compat_force_reasoning_only_models_to_responses column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "compat_force_reasoning_only_models_to_responses"); err != nil {
				return fmt.Errorf("failed to drop compat_force_reasoning_only_models_to_responses column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// migrationAddCompatConvertDecisionToResponsesColumn adds compat_convert_decision_to_responses
// to config_client. No backfill: decision emulation is opt-in, so existing rows stay false.
func migrationAddCompatConvertDecisionToResponsesColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_compat_convert_decision_to_responses_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "CompatConvertDecisionToResponses"); err != nil {
				return fmt.Errorf("failed to add compat_convert_decision_to_responses column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "compat_convert_decision_to_responses"); err != nil {
				return fmt.Errorf("failed to drop compat_convert_decision_to_responses column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// migrationBackfillCompatForceReasoningOnlyModelsToResponses sets compat_force_reasoning_only_models_to_responses
// TRUE on existing config_client rows so the toggle is on for existing deployments.
func migrationBackfillCompatForceReasoningOnlyModelsToResponses(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "backfill_compat_force_reasoning_only_models_to_responses"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := tx.Exec("UPDATE config_client SET compat_force_reasoning_only_models_to_responses = TRUE").Error; err != nil {
				return fmt.Errorf("failed to backfill compat_force_reasoning_only_models_to_responses: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			// Forward-only: the backfilled values are not reverted; rolling back the column migration drops them.
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// migrationAddIgnoreProviderCostColumn adds the ignore_provider_cost column to config_providers.
func migrationAddIgnoreProviderCostColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_ignore_provider_cost_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableProvider{}, "ignore_provider_cost"); err != nil {
				return fmt.Errorf("failed to add ignore_provider_cost column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return rollbackIgnoreProviderCostColumn(tx, logger)
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("failed to run migration %s: %w", migrationName, err)
	}
	return nil
}

// rollbackIgnoreProviderCostColumn refuses to undo add_ignore_provider_cost_column.
func rollbackIgnoreProviderCostColumn(*gorm.DB, schemas.Logger) error {
	return fmt.Errorf("add_ignore_provider_cost_column is non-rollbackable: dropping ignore_provider_cost would discard every operator's per-provider setting and silently send those providers back to trusting their reported usage.cost; the column is additive and older binaries safely ignore it")
}

// migrationAdd100kTokenPricingColumns adds the rates for prompts above 100k
// tokens (Claude Haiku 5.5). Nullable so models without them keep base rates.
func migrationAdd100kTokenPricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_100k_token_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	columns := []string{
		"input_cost_per_token_above_100k_tokens",
		"output_cost_per_token_above_100k_tokens",
		"cache_creation_input_token_cost_above_100k_tokens",
		"cache_read_input_token_cost_above_100k_tokens",
		"cache_creation_input_token_cost_above_1hr_above_100k_tokens",
	}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// migrationAddDecisionsPricingColumns adds the rates for decision requests on a
// row of another mode (OpenAI gpt-6-luna, a chat row served on /v1/decisions).
// Nullable so models without them keep pricing decisions at their normal rates.
func migrationAddDecisionsPricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_decisions_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	columns := []string{
		"input_cost_per_token_decisions",
		"output_cost_per_token_decisions",
	}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// migrationAddCloudflareKeyConfigColumns adds the Cloudflare per-key account ID column.
func migrationAddCloudflareKeyConfigColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_cloudflare_key_config_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return addColumnIfNotExists(tx, logger, &tables.TableKey{}, "cloudflare_account_id")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableKey{}, "cloudflare_account_id")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running cloudflare key config columns migration: %s", err.Error())
	}
	return nil
}

// migrationAddVirtualKeyMetadataColumn adds the nullable metadata column to
// governance_virtual_keys. Existing keys keep NULL (no metadata), and
// GenerateVirtualKeyHash only hashes metadata when the key has some, so no
// config_hash backfill is needed.
func migrationAddVirtualKeyMetadataColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_virtual_key_metadata_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := setMigrationLockTimeout(tx); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableVirtualKey{}, "metadata"); err != nil {
				return fmt.Errorf("failed to add metadata column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return fmt.Errorf("add_virtual_key_metadata_column is non-rollbackable: dropping metadata would permanently delete every virtual key's metadata; the column is additive and older binaries safely ignore it")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// migrationLockTimeout bounds how long a column migration waits for its table lock on postgres.
const migrationLockTimeout = "5s"

// setMigrationLockTimeout bounds the table-lock wait of the schema changes that follow in tx on
// postgres. ALTER TABLE needs an ACCESS EXCLUSIVE lock even to add a nullable column, and while it
// waits behind a long-running transaction every later query on the table queues behind it. With
// the bound the migration fails fast instead (lock_not_available), the transaction rolls back, and
// the next startup retries. SET LOCAL scopes it to the migration's transaction (migrator runs each
// migration in one by default). Other dialects have no table-lock wait to bound.
func setMigrationLockTimeout(tx *gorm.DB) error {
	if tx.Dialector.Name() != "postgres" {
		return nil
	}
	if err := tx.Exec("SET LOCAL lock_timeout = '" + migrationLockTimeout + "'").Error; err != nil {
		return fmt.Errorf("set migration lock_timeout: %w", err)
	}
	return nil
}

// migrationAddVertexUseSessionRoutingColumn adds the vertex_use_session_routing column to the key table.
// Existing keys default to false (NULL), so no request gains a session_id.
func migrationAddVertexUseSessionRoutingColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_vertex_use_session_routing_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return addColumnIfNotExists(tx, logger, &tables.TableKey{}, "vertex_use_session_routing")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableKey{}, "vertex_use_session_routing")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddPassthroughJSONColumn adds the passthrough_json column to the provider
// table, backing ProviderConfig.Passthrough.
func migrationAddPassthroughJSONColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_passthrough_json_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return addColumnIfNotExists(tx, logger, &tables.TableProvider{}, "PassthroughJSON")
		},
		Rollback: func(tx *gorm.DB) error {
			return rollbackPassthroughJSONColumn(tx)
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// rollbackPassthroughJSONColumn refuses to undo add_passthrough_json_column.
func rollbackPassthroughJSONColumn(*gorm.DB) error {
	return fmt.Errorf("add_passthrough_json_column is non-rollbackable: dropping passthrough_json would delete every provider's passthrough allowlist, and after a re-upgrade built-in providers would silently forward every passthrough route again; the column is additive and older binaries safely ignore it")
}

// migrationAddClientConfigSkipAccessProfileUserVirtualKeyLifecycleColumns adds
// skip_access_profile_user_virtual_key_creation and skip_access_profile_user_virtual_key_deletion to
// config_client. Existing rows default to false, keeping access profiles managing user keys.
func migrationAddClientConfigSkipAccessProfileUserVirtualKeyLifecycleColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_client_config_skip_access_profile_user_virtual_key_lifecycle_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, column := range []string{"skip_access_profile_user_virtual_key_creation", "skip_access_profile_user_virtual_key_deletion"} {
				if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, column); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, column := range []string{"skip_access_profile_user_virtual_key_creation", "skip_access_profile_user_virtual_key_deletion"} {
				if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, column); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddAzureUseSpeechEndpointsColumn adds the azure_use_speech_endpoints column to the key table.
// Existing keys default to false (NULL), so their audio keeps going to Azure OpenAI.
func migrationAddAzureUseSpeechEndpointsColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_azure_use_speech_endpoints_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return addColumnIfNotExists(tx, logger, &tables.TableKey{}, "azure_use_speech_endpoints")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableKey{}, "azure_use_speech_endpoints")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddAzureUseVoiceLiveEndpointsColumn adds the azure_use_voice_live_endpoints column to the key table.
// Existing keys default to false (NULL), so their realtime sessions keep going to Azure OpenAI.
func migrationAddAzureUseVoiceLiveEndpointsColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_azure_use_voice_live_endpoints_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return addColumnIfNotExists(tx, logger, &tables.TableKey{}, "azure_use_voice_live_endpoints")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableKey{}, "azure_use_voice_live_endpoints")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddSageMakerKeyColumns adds the sagemaker_* credential and routing columns to the
// config_keys table for the sagemaker provider.
func migrationAddSageMakerKeyColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_sagemaker_key_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	cols := []string{
		"sagemaker_access_key",
		"sagemaker_secret_key",
		"sagemaker_session_token",
		"sagemaker_region",
		"sagemaker_role_arn",
		"sagemaker_external_id",
		"sagemaker_role_session_name",
		"sagemaker_endpoints_json",
	}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := setMigrationLockTimeout(tx); err != nil {
				return err
			}
			for _, col := range cols {
				if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, col); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, col := range cols {
				if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, col); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running db migration: %s", err.Error())
	}
	return nil
}

// voiceLivePricingColumns are the Azure Voice Live rate columns on governance_model_pricing.
var voiceLivePricingColumns = []string{
	"input_cost_per_token_voice_live",
	"output_cost_per_token_voice_live",
	"cache_read_input_token_cost_voice_live",
	"input_cost_per_audio_token_voice_live",
	"output_cost_per_audio_token_voice_live",
	"input_cost_per_standard_speech_audio_token_voice_live",
	"output_cost_per_standard_speech_audio_token_voice_live",
	"input_cost_per_custom_speech_audio_token_voice_live",
	"output_cost_per_custom_speech_audio_token_voice_live",
}

// migrationAddVoiceLivePricingColumns adds the Azure Voice Live rate columns.
func migrationAddVoiceLivePricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_voice_live_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db.WithContext(ctx), migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := setMigrationLockTimeout(tx); err != nil {
				return err
			}
			for _, field := range voiceLivePricingColumns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return rollbackVoiceLivePricingColumns(ctx, tx, logger)
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// rollbackVoiceLivePricingColumns is the down path of add_voice_live_pricing_columns: it refuses, since
// the columns hold operator-set rates.
func rollbackVoiceLivePricingColumns(context.Context, *gorm.DB, schemas.Logger) error {
	return fmt.Errorf("add_voice_live_pricing_columns is non-rollbackable: dropping the _voice_live columns would permanently delete every Voice Live rate an operator has set, and Voice Live turns would silently bill at the normal rates; the columns are additive and older binaries safely ignore them")
}

// migrationAddCacheCreationAbove12hrPricingColumns adds OpenAI 12h cache-write rates by service tier and 272k tier.
func migrationAddCacheCreationAbove12hrPricingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_cache_creation_above_12hr_pricing_columns"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	columns := []string{
		"cache_creation_input_token_cost_above_12hr",
		"cache_creation_input_token_cost_above_12hr_above_272k_tokens",
		"cache_creation_input_token_cost_above_12hr_priority",
		"cache_creation_input_token_cost_above_12hr_above_272k_priority",
		"cache_creation_input_token_cost_above_12hr_flex",
		"cache_creation_input_token_cost_above_12hr_above_272k_flex",
		"cache_creation_input_token_cost_above_12hr_ultrafast",
		"cache_creation_input_token_cost_above_12hr_above_272k_ultrafast",
	}
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := setMigrationLockTimeout(tx); err != nil {
				return err
			}
			for _, field := range columns {
				if err := addColumnIfNotExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to add column %s: %w", field, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, field := range columns {
				if err := dropColumnIfExists(tx, logger, &tables.TableModelPricing{}, field); err != nil {
					return fmt.Errorf("failed to drop column %s: %w", field, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running cache creation above 12hr pricing columns migration: %s", err.Error())
	}
	return nil
}

// migrationAddUsageEstimationTokenizersJSONColumn adds the usage_estimation_tokenizers_json
// column to config_client. Existing rows get an empty list, which keeps cancelled-stream
// estimation on the base tiktoken encodings; a value in config.json's client section
// reconciles in on boot.
func migrationAddUsageEstimationTokenizersJSONColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_usage_estimation_tokenizers_json_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "UsageEstimationTokenizersJSON"); err != nil {
				return fmt.Errorf("failed to add usage_estimation_tokenizers_json column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "usage_estimation_tokenizers_json"); err != nil {
				return fmt.Errorf("failed to drop usage_estimation_tokenizers_json column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddBudgetBusinessUnitIDColumn adds the business_unit_id owner column to
// governance_budgets, so a business unit can hold one budget per reset window the
// way a team or customer does (TableBudget.TeamID / CustomerID). Adding a nullable
// column is a catalog change and does not rewrite the table. Its index is built by
// the migration after this one, migrationAddBudgetBusinessUnitIDIndex.
//
// Index only, no foreign key: the config loader may write a budget naming a business
// unit before any row for that unit exists, so no database constraint ties the two.
func migrationAddBudgetBusinessUnitIDColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_budget_business_unit_id_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := setMigrationLockTimeout(tx); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableBudget{}, "BusinessUnitID"); err != nil {
				return fmt.Errorf("add business_unit_id column to governance_budgets: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableBudget{}, "BusinessUnitID"); err != nil {
				return fmt.Errorf("drop business_unit_id column from governance_budgets: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddBudgetBusinessUnitIDIndex indexes governance_budgets.business_unit_id, which a
// business unit's budget preload, its deletion and the config.json prune guard all filter on.
// Upgraded databases get the column from addColumnIfNotExists, which does not build the tag
// index. Built concurrently so the upgrade never blocks writes: budget usage is written to this
// table on every dump.
func migrationAddBudgetBusinessUnitIDIndex(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_budget_business_unit_id_index"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	noTxOpts := *migrator.DefaultOptions
	noTxOpts.UseTransaction = false
	if err := RunSingleMigration(ctx, &noTxOpts, db, logger, budgetBusinessUnitIDIndexMigration(ctx, migrationName)); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// budgetBusinessUnitIDIndexMigration builds the migration applied by
// migrationAddBudgetBusinessUnitIDIndex, so tests exercise the same Migrate and Rollback
// callbacks the upgrade runs.
func budgetBusinessUnitIDIndexMigration(ctx context.Context, id string) *migrator.Migration {
	const name = "idx_governance_budgets_business_unit_id"
	return &migrator.Migration{
		ID: id,
		Migrate: func(tx *gorm.DB) error {
			return ensureIndexConcurrently(tx.WithContext(ctx), "governance_budgets", name, "business_unit_id", false)
		},
		Rollback: func(tx *gorm.DB) error {
			return dropIndexConcurrently(tx.WithContext(ctx), "governance_budgets", name)
		},
	}
}

// migrationAddDatabricksScopesColumn adds the databricks_scopes column to config_keys. There is
// nothing to backfill: a NULL column keeps the key on the all-apis default it already requested.
func migrationAddDatabricksScopesColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_databricks_scopes_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if tx.Dialector.Name() == "postgres" {
				// Bound the ACCESS EXCLUSIVE wait on config_keys, as add_oauth_key_config_columns
				// does, so a long transaction fails this boot's migration instead of queueing
				// every key read behind it. The next boot retries.
				if err := tx.Exec("SET LOCAL lock_timeout = '5s'").Error; err != nil {
					return fmt.Errorf("failed to bound the config_keys lock wait: %w", err)
				}
			}
			if err := addColumnIfNotExists(tx, logger, &tables.TableKey{}, "databricks_scopes"); err != nil {
				return fmt.Errorf("failed to add databricks_scopes column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &tables.TableKey{}, "databricks_scopes"); err != nil {
				return fmt.Errorf("failed to drop databricks_scopes column: %w", err)
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddComplexitySuggestionSchedulesTable adds the per-router state of
// phrase-suggestion schedules: when the next run is due, and the version
// nodes claim a due run with. A router without a row gets one at its first
// schedule check, so nothing is moved here.
func migrationAddComplexitySuggestionSchedulesTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_model_routing_suggestion_schedules_table"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if tx.Migrator().HasTable(&tables.TableComplexitySuggestionSchedule{}) {
				return nil
			}
			return tx.Migrator().CreateTable(&tables.TableComplexitySuggestionSchedule{})
		},
		Rollback: func(tx *gorm.DB) error {
			return tx.WithContext(ctx).Migrator().DropTable(&tables.TableComplexitySuggestionSchedule{})
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

// migrationAddClientConfigEntityAccessProfileModeColumn adds entity_access_profile_mode to
// config_client. It defaults to "union", so an upgrade keeps team, business unit and customer
// access profiles adding to a caller's access, as they did before the setting existed.
func migrationAddClientConfigEntityAccessProfileModeColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "add_client_config_entity_access_profile_mode_column"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := setMigrationLockTimeout(tx); err != nil {
				return err
			}
			return addColumnIfNotExists(tx, logger, &tables.TableClientConfig{}, "entity_access_profile_mode")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &tables.TableClientConfig{}, "entity_access_profile_mode")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %w", migrationName, err)
	}
	return nil
}

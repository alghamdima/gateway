package logstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/migrator"
	"github.com/maximhq/bifrost/framework/postgresconn"
	"gorm.io/gorm"
)

// isValidJSON checks if a string is valid JSON.
func isValidJSON(s string) bool {
	var js json.RawMessage
	return json.Unmarshal([]byte(s), &js) == nil
}

// addColumnIfNotExists is a package-local alias for migrator.AddColumnIfNotExists,
// the idempotent column-add helper shared with configstore. Declared at package
// scope (where `migrator` resolves to the package, not the `migrator :=
// tx.Migrator()` locals inside migration closures) so every call site can keep
// calling addColumnIfNotExists(tx, ...) directly.
var addColumnIfNotExists = migrator.AddColumnIfNotExists

// dropColumnIfExists is the drop counterpart to addColumnIfNotExists: a
// package-local alias for migrator.DropColumnIfExists, the idempotent
// column-drop helper shared with configstore. Declared at package scope for the
// same reason as above so call sites can use dropColumnIfExists(tx, ...).
var dropColumnIfExists = migrator.DropColumnIfExists

// boundDDLLockWait caps how long the DDL that follows waits for its table lock,
// on PostgreSQL only.
//
// ADD COLUMN and DROP COLUMN both take ACCESS EXCLUSIVE on logs. The change
// itself is metadata-only, but waiting for the lock parks the statement at the
// head of the lock queue, where every query arriving behind a long-running read
// waits on it too - a cluster-wide stall on the highest-volume table. Bounding
// the wait fails this boot's migration instead, and the next boot retries it.
//
// SET LOCAL needs the transaction opts.UseTransaction gives the migration, and
// reverts with it. It applies to the whole transaction, so it is set once per
// callback rather than before each statement.
func boundDDLLockWait(tx *gorm.DB) error {
	if tx.Dialector.Name() != "postgres" {
		return nil
	}
	return tx.Exec("SET LOCAL lock_timeout = '5s'").Error
}

const (
	// migrationAdvisoryLockKey is used for PostgreSQL advisory locks
	// to serialize migrations across cluster nodes.
	// This is intentionally separate from the configstore migration lock so
	// configstore and logstore migrations can proceed independently.
	migrationAdvisoryLockKey = 1000011

	// indexAdvisoryLockKey serializes the background index build across
	// cluster nodes. It is intentionally a DIFFERENT key from migrationAdvisoryLockKey
	// so that the long-running CREATE INDEX CONCURRENTLY held by one pod's goroutine
	// does not block other pods from running their (fast) migrations on startup.
	indexAdvisoryLockKey = 1000012

	// matviewRefreshAdvisoryLockKey serializes materialized view maintenance
	// across cluster nodes. Startup create/repair and periodic refresh both use
	// this key so they never overlap.
	matviewRefreshAdvisoryLockKey = 1000015

	// advisoryLockRetryInterval is how long to wait between lock acquisition attempts.
	advisoryLockRetryInterval = 5 * time.Second

	// advisoryLockTimeout is how long to wait for an advisory lock while its
	// holder shows no sign of work (a crashed instance's lingering session) before
	// giving up with actionable operator guidance.
	advisoryLockTimeout = 5 * time.Minute

	// advisoryLockBusyTimeout is how long to wait for the migration lock while
	// its holder is visibly running statements: a live instance whose migrations are
	// slow, not a stale session. The background index lock does not get this
	// extension because its holder builds the same indexes this node would.
	advisoryLockBusyTimeout = 20 * time.Minute

	// maintenanceUpdateBatchSize bounds background data cleanups so they don't
	// lock or rewrite very large log tables in one transaction.
	maintenanceUpdateBatchSize = 10_000
)

// advisoryLock holds a dedicated connection and the advisory lock key.
// This ensures the lock is held on the same connection throughout its lifetime,
// preventing race conditions caused by GORM's connection pooling.
type advisoryLock struct {
	conn    *sql.Conn
	lockKey int64
}

// acquireAdvisoryLock gets a dedicated connection and acquires a PostgreSQL advisory lock
// using pg_try_advisory_lock with retry + timeout. This prevents pods from
// blocking indefinitely if a previous pod crashed without releasing the lock
// (e.g., behind a connection proxy or with slow TCP keepalive detection).
// For non-PostgreSQL databases, returns a no-op lock.
func acquireAdvisoryLock(ctx context.Context, db *gorm.DB, logger schemas.Logger, lockKey int64, label string, busyTimeout time.Duration) (*advisoryLock, error) {
	if db.Dialector.Name() != "postgres" {
		return &advisoryLock{}, nil
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to get sql.DB: %w", err)
	}

	// Get a dedicated connection (not returned to pool until Close())
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get dedicated connection for %s lock: %w", label, err)
	}

	logger.Info("[logstore] attempting to acquire %s lock %d", label, lockKey)
	if err := postgresconn.WaitForAdvisoryLock(ctx, conn, logger, postgresconn.AdvisoryWaitOptions{
		Key:           lockKey,
		Label:         "logstore " + label,
		LogPrefix:     "[logstore]",
		RetryInterval: advisoryLockRetryInterval,
		Timeout:       advisoryLockTimeout,
		BusyTimeout:   busyTimeout,
	}); err != nil {
		conn.Close()
		return nil, err
	}
	return &advisoryLock{conn: conn, lockKey: lockKey}, nil
}

// release unlocks and closes the dedicated connection.
func (l *advisoryLock) release(ctx context.Context) {
	if l.conn == nil {
		return
	}
	// Release lock on the SAME connection that acquired it.
	_, _ = l.conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", l.lockKey)
	l.conn.Close()
}

// acquireMigrationLock acquires the serialization lock for schema migrations.
func acquireMigrationLock(ctx context.Context, db *gorm.DB, logger schemas.Logger) (*advisoryLock, error) {
	return acquireAdvisoryLock(ctx, db, logger, migrationAdvisoryLockKey, "migration", advisoryLockBusyTimeout)
}

// acquireIndexLock acquires the serialization lock for the background index build.
func acquireIndexLock(ctx context.Context, db *gorm.DB, logger schemas.Logger) (*advisoryLock, error) {
	return acquireAdvisoryLock(ctx, db, logger, indexAdvisoryLockKey, "index", 0)
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

// migrationStepLockRetry tunes how runMigrationSteps retries a step that fails
// on a lock timeout. The zero value takes postgresconn's defaults; a variable
// only so tests can shrink the budget and backoff.
var migrationStepLockRetry postgresconn.LockRetryOptions

// runMigrationSteps runs migration steps in their declared order. It reads the
// pending IDs once and skips steps whose IDs are all recorded, so a deploy with
// one new migration does not pay a round trip per already-applied step. If the
// preflight read fails, every step runs and each one checks its own row. A step
// that fails on a lock timeout is retried within postgresconn's retry budget.
func runMigrationSteps(ctx context.Context, db *gorm.DB, logger schemas.Logger, steps []migrationStep) error {
	return runMigrationStepsWithRetry(ctx, db, logger, steps, migrationStepLockRetry)
}

// runMigrationStepsWithRetry is runMigrationSteps with explicit lock-timeout
// retry options; a zero budget takes postgresconn's default.
func runMigrationStepsWithRetry(ctx context.Context, db *gorm.DB, logger schemas.Logger, steps []migrationStep, retry postgresconn.LockRetryOptions) error {
	pending, err := pendingMigrationStepIDs(ctx, db, steps)
	var pendingSet map[string]struct{}
	if err != nil {
		logger.Warn("[logstore] migration preflight failed; running every step: %v", err)
	} else {
		pendingSet = make(map[string]struct{}, len(pending))
		for _, id := range pending {
			pendingSet[id] = struct{}{}
		}
	}
	// On a lock timeout, log who holds the log tables: on a rolling upgrade the
	// blocker is almost always an old replica's long read, which an operator can
	// cancel to let the migration through.
	retry.Describe = func(ctx context.Context) string {
		return postgresconn.DescribeTableLockHolders(ctx, db, "logs")
	}
	for _, step := range steps {
		if pendingSet != nil && !stepHasPendingID(step, pendingSet) {
			continue
		}
		// A step that times out waiting for a table lock (another session holds a
		// conflicting lock, e.g. a long read on the table being altered) is retried
		// with backoff instead of failing the boot. Steps are idempotent: the
		// migrator records completed IDs and the DDL helpers use IF [NOT] EXISTS.
		label := "[logstore] migration " + strings.Join(step.IDs, ",")
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

// logstoreMigrationSteps is the ordered source of truth for logstore migration
// execution and preflight checks.
var logstoreMigrationSteps = []migrationStep{
	{IDs: []string{"logs_init"}, run: migrationInit},
	{IDs: []string{"logs_init_update_object_column_values"}, run: migrationUpdateObjectColumnValues},
	{IDs: []string{"logs_init_add_parent_request_id_column"}, run: migrationAddParentRequestIDColumn},
	{IDs: []string{"logs_init_add_responses_output_column"}, run: migrationAddResponsesOutputColumn},
	{IDs: []string{"logs_init_add_cost_and_cache_debug_column"}, run: migrationAddCostAndCacheDebugColumn},
	{IDs: []string{"logs_add_guardrail_debug_column"}, run: migrationAddGuardrailDebugColumn},
	{IDs: []string{"logs_init_add_responses_input_history_column"}, run: migrationAddResponsesInputHistoryColumn},
	{IDs: []string{"logs_init_add_number_of_retries_and_fallback_index_and_selected_key_and_virtual_key_columns"}, run: migrationAddNumberOfRetriesAndFallbackIndexAndSelectedKeyAndVirtualKeyColumns},
	{IDs: []string{"logs_add_performance_indexes"}, run: migrationAddPerformanceIndexes},
	{IDs: []string{"logs_add_performance_indexes_v2"}, run: migrationAddPerformanceIndexesV2},
	{IDs: []string{"logs_update_timestamp_format"}, run: migrationUpdateTimestampFormat},
	{IDs: []string{"logs_add_raw_request_column"}, run: migrationAddRawRequestColumn},
	{IDs: []string{"logs_add_image_generation_output_column"}, run: migrationAddImageGenerationOutputColumn},
	{IDs: []string{"logs_add_image_generation_input_column"}, run: migrationAddImageGenerationInputColumn},
	{IDs: []string{"logs_add_routing_rule_id_and_routing_rule_name_columns"}, run: migrationAddRoutingRuleIDAndRoutingRuleNameColumns},
	{IDs: []string{"logs_add_routing_engine_used_column"}, run: migrationAddRoutingEngineUsedColumn},
	{IDs: []string{"logs_add_routing_engines_used_column"}, run: migrationAddRoutingEnginesUsedColumn},
	{IDs: []string{"logs_add_list_models_output_column"}, run: migrationAddListModelsOutputColumn},
	{IDs: []string{"logs_add_rerank_output_column"}, run: migrationAddRerankOutputColumn},
	{IDs: []string{"logs_add_routing_engine_logs_column"}, run: migrationAddRoutingEngineLogsColumn},
	{IDs: []string{"async_jobs_init"}, run: migrationCreateAsyncJobsTable},
	{IDs: []string{"logs_add_metadata_column"}, run: migrationAddMetadataColumn},
	{IDs: []string{"logs_add_histogram_composite_indexes"}, run: migrationAddHistogramCompositeIndexes},
	{IDs: []string{"logs_add_video_columns"}, run: migrationAddVideoColumns},
	{IDs: []string{"logs_add_provider_histogram_index"}, run: migrationAddProviderHistogramIndex},
	{IDs: []string{"logs_add_large_payload_columns"}, run: migrationAddLargePayloadColumns},
	{IDs: []string{"logs_add_passthrough_request_body_column"}, run: migrationAddPassthroughRequestBodyColumn},
	{IDs: []string{"logs_add_passthrough_response_body_column"}, run: migrationAddPassthroughResponseBodyColumn},
	{IDs: []string{"logs_add_metadata_gin_index_v3"}, run: migrationAddMetadataGINIndex},
	{IDs: []string{"logs_dashboard_enhancements"}, run: migrationAddDashboardEnhancements},
	{IDs: []string{"logs_and_dashboard_performance_indexes"}, run: migrationAddLogsAndDashboardPerformanceIndexes},
	{IDs: []string{"logs_add_image_edit_input_column"}, run: migrationAddImageEditInputColumn},
	{IDs: []string{"logs_add_image_variation_input_column"}, run: migrationAddImageVariationInputColumn},
	{IDs: []string{"logs_add_plugin_logs_column"}, run: migrationAddPluginLogsColumn},
	{IDs: []string{"logs_add_alias_column"}, run: migrationAddAliasColumn},
	{IDs: []string{"logs_add_governance_context_columns"}, run: migrationAddGovernanceContextColumns},
	{IDs: []string{"logs_recreate_matviews_with_governance_columns"}, run: migrationRecreateMatViewsWithGovernanceColumns},
	{IDs: []string{"logs_add_ocr_output_column"}, run: migrationAddOCROutputColumn},
	{IDs: []string{"logs_add_has_object_column"}, run: migrationAddHasObjectColumn},
	{IDs: []string{"logs_add_attempt_trail_column"}, run: migrationAddAttemptTrailColumn},
	{IDs: []string{"logs_add_user_name_column"}, run: migrationAddUserNameColumn},
	{IDs: []string{"logs_add_ocr_input_column"}, run: migrationAddOCRInputColumn},
	{IDs: []string{"logs_add_stop_reason_column"}, run: migrationAddStopReasonColumn},
	{IDs: []string{"logs_add_safe_jsonb_function"}, run: migrationAddSafeJsonbFunction},
	{IDs: []string{"logs_add_cluster_governance_columns"}, run: migrationAddClusterGovernanceColumns},
	{IDs: []string{"logs_add_inc_number_column"}, run: migrationAddLogIncNumberColumn},
	{IDs: []string{"logs_recreate_filter_users_matview"}, run: migrationRecreateFilterUsersMatView},
	{IDs: []string{"logs_add_multi_team_business_unit_columns"}, run: migrationAddMultiTeamBusinessUnitColumns},
	{IDs: []string{"logs_add_multi_team_bu_gin_indexes_v1"}, run: migrationAddMultiTeamBusinessUnitGINIndexes},
	{IDs: []string{"logs_recreate_filter_team_bu_matviews_multivalue"}, run: migrationRecreateFilterTeamBUMatViews},
	{IDs: []string{"logs_add_customer_array_columns"}, run: migrationAddCustomerArrayColumns},
	{IDs: []string{"logs_add_customer_array_gin_indexes_v1"}, run: migrationAddCustomerArrayGINIndexes},
	{IDs: []string{"logs_recreate_filter_customers_matview_multivalue"}, run: migrationRecreateFilterCustomersMatView},
	{IDs: []string{"logs_add_canonical_model_columns_v2"}, run: migrationAddCanonicalModelColumns},
	{IDs: []string{"logs_add_redaction_mapping_column"}, run: migrationAddRedactionMappingColumn},
	{IDs: []string{"async_jobs_add_request_id_column"}, run: migrationAddAsyncJobRequestIDColumn},
	{IDs: []string{"logs_add_content_hidden_column"}, run: migrationAddContentHiddenColumn},
	{IDs: []string{"logs_add_server_side_fallback_model_column"}, run: migrationAddServerSideFallbackModelColumn},
	{IDs: []string{"logs_add_billing_fidelity_columns"}, run: migrationAddBillingFidelityColumns},
	{IDs: []string{"logs_add_user_agent_column"}, run: migrationAddUserAgentColumn},
	{IDs: []string{"logs_recreate_matviews_with_app_column"}, run: migrationRecreateMatViewsWithUserAgentColumn},
	{IDs: []string{"logs_add_video_edit_input_column"}, run: migrationAddVideoEditInputColumn},
	{IDs: []string{"logs_add_upstream_and_overhead_latency_columns"}, run: migrationAddUpstreamAndOverheadLatencyColumns},
	{IDs: []string{"logs_add_complexity_routing_columns"}, run: migrationAddComplexityRoutingColumns},
	{IDs: []string{"logs_add_session_id_column"}, run: migrationAddSessionIDColumn},
	{IDs: []string{"logs_add_routing_metadata_column"}, run: migrationAddRoutingMetadataColumn},
	{IDs: []string{"logs_add_batch_debug_column"}, run: migrationAddBatchDebugColumn},
	{IDs: []string{"logs_add_cost_breakdown_columns"}, run: migrationAddCostBreakdownColumns},
	{IDs: []string{"logs_recreate_matviews_with_cost_breakdown"}, run: migrationRecreateMatViewsWithCostBreakdown},
	{IDs: []string{"logs_add_overhead_breakdown_column"}, run: migrationAddOverheadBreakdownColumn},
	{IDs: []string{"logs_add_video_debug_column"}, run: migrationAddVideoDebugColumn},
	{IDs: []string{"logs_add_project_columns"}, run: migrationAddProjectColumns},
	{IDs: []string{"logs_add_served_model_column"}, run: migrationAddServedModelColumn},
	{IDs: []string{"logs_add_tool_call_names_column"}, run: migrationAddToolCallNamesColumn},
	{IDs: []string{"logs_add_embedding_input_column"}, run: migrationAddEmbeddingInputColumn},
	{IDs: []string{"logs_add_owner_timestamp_indexes"}, run: migrationAddOwnerTimestampIndexes},
	{IDs: []string{"logs_add_ranking_name_timestamp_indexes"}, run: migrationAddRankingNameTimestampIndexes},
	{IDs: []string{"logs_add_live_session_column"}, run: migrationAddLiveSessionColumn},
	{IDs: []string{"logs_add_correlation_columns"}, run: migrationAddCorrelationColumns},
	{IDs: []string{"logs_add_matview_refresh_state_table"}, run: migrationAddMatViewRefreshStateTable},
	{IDs: []string{"logs_add_is_passthrough_column"}, run: migrationAddIsPassthroughColumn},
	{IDs: []string{"logs_add_client_ip_column"}, run: migrationAddClientIPColumn},
	{IDs: []string{"logs_add_user_email_column"}, run: migrationAddUserEmailColumn},
	{IDs: []string{"logs_add_model_routing_suggestion_tables"}, run: migrationAddComplexitySuggestionTables},
	{IDs: []string{"logs_add_model_routing_columns"}, run: migrationAddModelRoutingColumns},
	{IDs: []string{"logs_tune_retention_autovacuum"}, run: migrationTuneRetentionAutovacuum},
}

// areThereAnyPendingMigrations returns true if there are any pending migrations to be applied.
func areThereAnyPendingMigrations(ctx context.Context, db *gorm.DB, logger schemas.Logger) bool {
	pending, err := pendingMigrationStepIDs(ctx, db, logstoreMigrationSteps)
	if err != nil {
		logger.Warn("[logstore] migration preflight failed; acquiring migration lock and running migrations: %v", err)
	}
	logger.Info("[logstore] pending migrations")
	for _, id := range pending {
		logger.Info("[logstore] migration : %s", id)
	}
	return err != nil || len(pending) > 0
}

// triggerMigrations runs all registered logstore schema migrations in order under
// a PostgreSQL advisory lock so only one node migrates the logstore at a time,
// pausing materialized-view refreshes with the default options.
func triggerMigrations(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	return triggerMigrationsWithPause(ctx, db, logger, MatViewPauseOptions{CancelAfter: DefaultMatViewPauseCancelAfter})
}

// pauseMatViewsForMigrations is the pause triggerMigrationsWithPause takes; a
// variable only so tests can switch it off to reproduce the pre-pause behaviour.
var pauseMatViewsForMigrations = PauseMatViewsForMigrations

// triggerMigrationsWithPause is triggerMigrations with explicit matview pause
// options. When there is work to do, refreshes are paused for the whole run so a
// long REFRESH from any replica (including an older release mid-rollout) cannot
// hold the snapshot and table lock the DDL below needs.
func triggerMigrationsWithPause(ctx context.Context, db *gorm.DB, logger schemas.Logger, pauseOpts MatViewPauseOptions) error {
	return triggerMigrationsWithOptions(ctx, db, logger, pauseOpts, migrationStepLockRetry)
}

// triggerMigrationsWithOptions is triggerMigrationsWithPause with the lock-timeout
// retry options too, so a configured migration_lock_retry_budget governs how
// long a blocked step keeps retrying. A test override of migrationStepLockRetry
// keeps precedence over the configured budget.
func triggerMigrationsWithOptions(ctx context.Context, db *gorm.DB, logger schemas.Logger, pauseOpts MatViewPauseOptions, retry postgresconn.LockRetryOptions) error {
	if !areThereAnyPendingMigrations(ctx, db, logger) {
		logger.Info("[logstore] migrations already current; skipping migration lock")
		return nil
	}

	// Acquire advisory lock to serialize migrations across cluster nodes.
	lock, err := acquireMigrationLock(ctx, db, logger)
	if err != nil {
		return err
	}
	defer lock.release(ctx)

	if !areThereAnyPendingMigrations(ctx, db, logger) {
		logger.Info("[logstore] migrations completed by another node; skipping migration run")
		return nil
	}

	// Taken after the migration lock and released before it (defers run LIFO),
	// matching the configstore order so the two locks never invert.
	releasePause := pauseMatViewsForMigrations(ctx, db, logger, pauseOpts)
	defer releasePause()

	return runMigrationStepsWithRetry(ctx, db, logger, logstoreMigrationSteps, migrationStepLockRetry.WithBudgetFallback(retry))
}

// migrationInit creates the logs table if it does not exist.
func migrationInit(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_init"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			if !migrator.HasTable(&Log{}) {
				logger.Info("[logstore] %s: creating table Log", migrationName)
				if err := migrator.CreateTable(&Log{}); err != nil {
					return err
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			// Drop children first, then parents (adjust if your actual FKs differ)
			logger.Info("[logstore] %s: dropping table Log", migrationName)
			if err := migrator.DropTable(&Log{}); err != nil {
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

// migrationUpdateObjectColumnValues normalizes legacy object_type string values on the logs table.
func migrationUpdateObjectColumnValues(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_init_update_object_column_values"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = false
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if tx.Dialector.Name() != "postgres" {
				result := tx.Exec(`
						UPDATE logs
						SET object_type = CASE object_type
							WHEN 'chat.completion' THEN 'chat_completion'
							WHEN 'text.completion' THEN 'text_completion'
							WHEN 'list' THEN 'embedding'
							WHEN 'audio.speech' THEN 'speech'
							WHEN 'audio.transcription' THEN 'transcription'
							WHEN 'chat.completion.chunk' THEN 'chat_completion_stream'
							WHEN 'audio.speech.chunk' THEN 'speech_stream'
							WHEN 'audio.transcription.chunk' THEN 'transcription_stream'
							WHEN 'response' THEN 'responses'
							WHEN 'response.completion.chunk' THEN 'responses_stream'
							ELSE object_type
						END
						WHERE object_type IN (
							'chat.completion', 'text.completion', 'list',
							'audio.speech', 'audio.transcription', 'chat.completion.chunk',
							'audio.speech.chunk', 'audio.transcription.chunk',
							'response', 'response.completion.chunk'
						)`)
				if result.Error != nil {
					return fmt.Errorf("failed to update object_type values: %w", result.Error)
				}
				return nil
			}

			updateSQL := `
				WITH batch AS (
					SELECT ctid,
						CASE object_type
							WHEN 'chat.completion' THEN 'chat_completion'
							WHEN 'text.completion' THEN 'text_completion'
							WHEN 'list' THEN 'embedding'
							WHEN 'audio.speech' THEN 'speech'
							WHEN 'audio.transcription' THEN 'transcription'
							WHEN 'chat.completion.chunk' THEN 'chat_completion_stream'
							WHEN 'audio.speech.chunk' THEN 'speech_stream'
							WHEN 'audio.transcription.chunk' THEN 'transcription_stream'
							WHEN 'response' THEN 'responses'
							WHEN 'response.completion.chunk' THEN 'responses_stream'
							ELSE object_type
						END AS normalized_object_type
					FROM logs
					WHERE object_type IN (
						'chat.completion', 'text.completion', 'list',
						'audio.speech', 'audio.transcription', 'chat.completion.chunk',
						'audio.speech.chunk', 'audio.transcription.chunk',
						'response', 'response.completion.chunk'
					)
					LIMIT ?
					FOR UPDATE SKIP LOCKED
				)
				UPDATE logs
				SET object_type = batch.normalized_object_type
				FROM batch
				WHERE logs.ctid = batch.ctid`

			if err := execBatchedGormMaintenanceUpdate(tx, "object_type normalization", updateSQL); err != nil {
				return err
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			// Use a single CASE statement for efficient bulk rollback
			rollbackSQL := `
				UPDATE logs
				SET object_type = CASE object_type
					WHEN 'chat_completion' THEN 'chat.completion'
					WHEN 'text_completion' THEN 'text.completion'
					WHEN 'embedding' THEN 'list'
					WHEN 'speech' THEN 'audio.speech'
					WHEN 'transcription' THEN 'audio.transcription'
					WHEN 'chat_completion_stream' THEN 'chat.completion.chunk'
					WHEN 'speech_stream' THEN 'audio.speech.chunk'
					WHEN 'transcription_stream' THEN 'audio.transcription.chunk'
					WHEN 'responses' THEN 'response'
					WHEN 'responses_stream' THEN 'response.completion.chunk'
					ELSE object_type
				END
				WHERE object_type IN (
					'chat_completion', 'text_completion', 'embedding', 'speech',
					'transcription', 'chat_completion_stream', 'speech_stream',
					'transcription_stream', 'responses', 'responses_stream'
				)`

			result := tx.Exec(rollbackSQL)
			if result.Error != nil {
				return fmt.Errorf("failed to rollback object_type values: %w", result.Error)
			}

			return nil
		},
	}})

	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running object column migration: %s", err.Error())
	}
	return nil
}

// migrationAddParentRequestIDColumn adds the parent_request_id column to the logs table.
func migrationAddParentRequestIDColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_init_add_parent_request_id_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "parent_request_id"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "parent_request_id"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding parent_request_id column: %s", err.Error())
	}
	return nil
}

// migrationAddResponsesOutputColumn adds columns for Responses API output, chat/embedding
// payloads, and raw_response on the logs table.
func migrationAddResponsesOutputColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_init_add_responses_output_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "responses_output"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "input_history"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "output_message"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "embedding_output"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "raw_response"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "responses_output"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "input_history"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "output_message"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "embedding_output"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "raw_response"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding responses_output column: %s", err.Error())
	}
	return nil
}

// migrationAddCostAndCacheDebugColumn adds cost and cache_debug columns to the logs table.
func migrationAddCostAndCacheDebugColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_init_add_cost_and_cache_debug_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "cost"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "cache_debug"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "cost"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "cache_debug"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding cost column: %s", err.Error())
	}
	return nil
}

// migrationAddGuardrailDebugColumn adds the guardrail_debug column to the logs table.
func migrationAddGuardrailDebugColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_guardrail_debug_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return addColumnIfNotExists(tx, logger, &Log{}, "guardrail_debug")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &Log{}, "guardrail_debug")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding guardrail_debug column: %s", err.Error())
	}
	return nil
}

// migrationAddUpstreamAndOverheadLatencyColumns adds the upstream_latency and
// overhead_latency columns to the logs table.
func migrationAddUpstreamAndOverheadLatencyColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_upstream_and_overhead_latency_columns"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "upstream_latency"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "overhead_latency"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "upstream_latency"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "overhead_latency"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding upstream/overhead latency columns: %s", err.Error())
	}
	return nil
}

// migrationAddOverheadBreakdownColumn adds the overhead_breakdown column to the
// logs table. It holds the per-span self-time decomposition of Bifrost overhead.
func migrationAddOverheadBreakdownColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_overhead_breakdown_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return addColumnIfNotExists(tx, logger, &Log{}, "overhead_breakdown")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &Log{}, "overhead_breakdown")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding overhead_breakdown column: %s", err.Error())
	}
	return nil
}

// migrationAddRoutingMetadataColumn adds the routing_metadata column to the logs
// table, so the semantic classification embed and llm classification calls
// the routing plugin made for a request are visible in the log detail view,
// mirroring how guardrail_debug already surfaces guardrail judge calls.
func migrationAddRoutingMetadataColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_routing_metadata_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := boundDDLLockWait(tx); err != nil {
				return err
			}
			return addColumnIfNotExists(tx, logger, &Log{}, "routing_metadata")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := boundDDLLockWait(tx); err != nil {
				return err
			}
			return dropColumnIfExists(tx, logger, &Log{}, "routing_metadata")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding routing_metadata column: %s", err.Error())
	}
	return nil
}

// migrationAddResponsesInputHistoryColumn adds the responses_input_history column to the logs table.
func migrationAddResponsesInputHistoryColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_init_add_responses_input_history_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "responses_input_history"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "responses_input_history"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding responses_input_history column: %s", err.Error())
	}
	return nil
}

// migrationAddNumberOfRetriesAndFallbackIndexAndSelectedKeyAndVirtualKeyColumns adds retry,
// fallback, selected API key, and virtual key columns to the logs table.
func migrationAddNumberOfRetriesAndFallbackIndexAndSelectedKeyAndVirtualKeyColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_init_add_number_of_retries_and_fallback_index_and_selected_key_and_virtual_key_columns"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "number_of_retries"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "fallback_index"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "selected_key_id"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "selected_key_name"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "virtual_key_id"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "virtual_key_name"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "number_of_retries"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "fallback_index"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "selected_key_id"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "selected_key_name"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "virtual_key_id"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "virtual_key_name"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding number_of_retries and fallback_index columns: %s", err.Error())
	}
	return nil
}

// migrationAddPerformanceIndexes adds btree indexes on latency, total_tokens, and key columns.
func migrationAddPerformanceIndexes(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_performance_indexes"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			if tx.Dialector.Name() == "postgres" {
				return nil
			}
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()

			// Add index on latency for AVG aggregation queries
			if !migrator.HasIndex(&Log{}, "idx_logs_latency") {
				logger.Info("[logstore] %s: creating index idx_logs_latency on Log", migrationName)
				if err := migrator.CreateIndex(&Log{}, "idx_logs_latency"); err != nil {
					return fmt.Errorf("failed to create index on latency: %w", err)
				}
			}

			// Add index on total_tokens for SUM aggregation queries
			if !migrator.HasIndex(&Log{}, "idx_logs_total_tokens") {
				logger.Info("[logstore] %s: creating index idx_logs_total_tokens on Log", migrationName)
				if err := migrator.CreateIndex(&Log{}, "idx_logs_total_tokens"); err != nil {
					return fmt.Errorf("failed to create index on total_tokens: %w", err)
				}
			}

			// Add index on selected_key_id for filtering. The struct tags now define
			// the (selected_key_id, timestamp) composite instead, so older tables get
			// the single-column index by raw SQL and
			// migrationAddRankingNameTimestampIndexes later swaps it for the composite.
			if !migrator.HasIndex(&Log{}, "idx_logs_selected_key_ts") && !migrator.HasIndex(&Log{}, "idx_logs_selected_key_id") {
				logger.Info("[logstore] %s: creating index idx_logs_selected_key_id on Log", migrationName)
				if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_logs_selected_key_id ON logs(selected_key_id)").Error; err != nil {
					return fmt.Errorf("failed to create index on selected_key_id: %w", err)
				}
			}

			// Add index on virtual_key_id for filtering. The struct tags now define
			// the (virtual_key_id, timestamp) composite instead, so a table created
			// from them already has an index leading on virtual_key_id; older tables
			// get the single-column index by raw SQL, and
			// migrationAddOwnerTimestampIndexes later swaps it for the composite.
			if !migrator.HasIndex(&Log{}, "idx_logs_vk_ts") && !migrator.HasIndex(&Log{}, "idx_logs_virtual_key_id") {
				logger.Info("[logstore] %s: creating index idx_logs_virtual_key_id on Log", migrationName)
				if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_logs_virtual_key_id ON logs(virtual_key_id)").Error; err != nil {
					return fmt.Errorf("failed to create index on virtual_key_id: %w", err)
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()

			if migrator.HasIndex(&Log{}, "idx_logs_latency") {
				logger.Info("[logstore] %s: dropping index idx_logs_latency on Log", migrationName)
				if err := migrator.DropIndex(&Log{}, "idx_logs_latency"); err != nil {
					return err
				}
			}
			if migrator.HasIndex(&Log{}, "idx_logs_total_tokens") {
				logger.Info("[logstore] %s: dropping index idx_logs_total_tokens on Log", migrationName)
				if err := migrator.DropIndex(&Log{}, "idx_logs_total_tokens"); err != nil {
					return err
				}
			}
			if migrator.HasIndex(&Log{}, "idx_logs_selected_key_id") {
				logger.Info("[logstore] %s: dropping index idx_logs_selected_key_id on Log", migrationName)
				if err := migrator.DropIndex(&Log{}, "idx_logs_selected_key_id"); err != nil {
					return err
				}
			}
			if migrator.HasIndex(&Log{}, "idx_logs_virtual_key_id") {
				logger.Info("[logstore] %s: dropping index idx_logs_virtual_key_id on Log", migrationName)
				if err := migrator.DropIndex(&Log{}, "idx_logs_virtual_key_id"); err != nil {
					return err
				}
			}

			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding performance indexes: %s", err.Error())
	}
	return nil
}

// migrationAddPerformanceIndexesV2 adds additional indexes for improved query performance
// This migration adds indices based on query patterns in rdb.go
func migrationAddPerformanceIndexesV2(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_performance_indexes_v2"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			if tx.Dialector.Name() == "postgres" {
				return nil
			}
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()

			// Single-column indices for filtering and sorting
			// These indices optimize queries in applyFilters, SearchLogs, GetStats, and Flush

			// Add index on timestamp for range queries and default ordering
			// Used in: WHERE timestamp >= ? AND timestamp <= ? and ORDER BY timestamp
			if !migrator.HasIndex(&Log{}, "idx_logs_timestamp") {
				if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_logs_timestamp ON logs(timestamp)").Error; err != nil {
					return fmt.Errorf("failed to create index on timestamp: %w", err)
				}
			}

			// Add index on status for filtering (success, error, processing)
			// Used in: WHERE status IN ('success', 'error'), WHERE status = 'processing'
			if !migrator.HasIndex(&Log{}, "idx_logs_status") {
				if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_logs_status ON logs(status)").Error; err != nil {
					return fmt.Errorf("failed to create index on status: %w", err)
				}
			}

			// Add index on created_at for Flush operations
			// Used in: WHERE created_at < ?
			if !migrator.HasIndex(&Log{}, "idx_logs_created_at") {
				if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_logs_created_at ON logs(created_at)").Error; err != nil {
					return fmt.Errorf("failed to create index on created_at: %w", err)
				}
			}

			// Add index on provider for filtering
			// Used in: WHERE provider IN (?)
			if !migrator.HasIndex(&Log{}, "idx_logs_provider") {
				if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_logs_provider ON logs(provider)").Error; err != nil {
					return fmt.Errorf("failed to create index on provider: %w", err)
				}
			}

			// Add index on model for filtering
			// Used in: WHERE model IN (?)
			if !migrator.HasIndex(&Log{}, "idx_logs_model") {
				if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_logs_model ON logs(model)").Error; err != nil {
					return fmt.Errorf("failed to create index on model: %w", err)
				}
			}

			// Add index on object_type for filtering
			// Used in: WHERE object_type IN (?)
			if !migrator.HasIndex(&Log{}, "idx_logs_object_type") {
				if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_logs_object_type ON logs(object_type)").Error; err != nil {
					return fmt.Errorf("failed to create index on object_type: %w", err)
				}
			}

			// Add index on cost for range queries and ordering
			// Used in: WHERE cost >= ? AND cost <= ?, ORDER BY cost
			if !migrator.HasIndex(&Log{}, "idx_logs_cost") {
				if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_logs_cost ON logs(cost)").Error; err != nil {
					return fmt.Errorf("failed to create index on cost: %w", err)
				}
			}

			// Composite indices for common query patterns

			// Add composite index on (status, timestamp) for GetStats queries
			// Used when filtering completed requests (status IN ('success', 'error')) with timestamp ranges
			// This composite index is more efficient than individual indices for these combined queries
			if !migrator.HasIndex(&Log{}, "idx_logs_status_timestamp") {
				if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_logs_status_timestamp ON logs(status, timestamp)").Error; err != nil {
					return fmt.Errorf("failed to create composite index on (status, timestamp): %w", err)
				}
			}

			// Add composite index on (status, created_at) for Flush operations
			// Used in Flush: WHERE status = 'processing' AND created_at < ?
			// This composite index significantly improves cleanup query performance
			if !migrator.HasIndex(&Log{}, "idx_logs_status_created_at") {
				if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_logs_status_created_at ON logs(status, created_at)").Error; err != nil {
					return fmt.Errorf("failed to create composite index on (status, created_at): %w", err)
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()

			// Drop all indices added in this migration
			indices := []string{
				"idx_logs_timestamp",
				"idx_logs_status",
				"idx_logs_created_at",
				"idx_logs_provider",
				"idx_logs_model",
				"idx_logs_object_type",
				"idx_logs_cost",
				"idx_logs_status_timestamp",
				"idx_logs_status_created_at",
			}

			logger.Info("[logstore] %s: processing %d indices", migrationName, len(indices))
			for _, indexName := range indices {
				if migrator.HasIndex(&Log{}, indexName) {
					if err := tx.Exec(fmt.Sprintf("DROP INDEX IF EXISTS %s", indexName)).Error; err != nil {
						return fmt.Errorf("failed to drop index %s: %w", indexName, err)
					}
				}
			}

			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding performance indexes v2: %s", err.Error())
	}
	return nil
}

// migrationUpdateTimestampFormat converts timestamp and created_at values to UTC ISO-8601 form
// on SQLite only; other dialects are unchanged.
func migrationUpdateTimestampFormat(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_update_timestamp_format"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	// only run the migration for sqlite databases
	dialect := db.Dialector.Name()
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			if dialect != "sqlite" {
				logger.Info("[logstore] skipping migration %s for dialect %s", migrationName, dialect)
				return nil
			}
			tx = tx.WithContext(ctx)

			updateSQL := `
				UPDATE logs
				SET "timestamp" = strftime('%Y-%m-%dT%H:%M:%S', "timestamp", 'utc') || '.' ||
                    CAST(CAST(strftime('%f', "timestamp") * 1000 AS INTEGER) % 1000 AS TEXT) || 'Z'
				WHERE
					"timestamp" NOT LIKE '%Z'
					AND "timestamp" NOT LIKE '%+00%';
				UPDATE logs
				SET created_at = strftime('%Y-%m-%dT%H:%M:%S', created_at, 'utc') || '.' ||
                    CAST(CAST(strftime('%f', created_at) * 1000 AS INTEGER) % 1000 AS TEXT) ||
                    'Z'
				WHERE
					created_at NOT LIKE '%Z'
					AND created_at NOT LIKE '%+00%';
				`

			result := tx.Exec(updateSQL)
			if result.Error != nil {
				return fmt.Errorf("failed to update timestamp values: %w", result.Error)
			}

			return nil
		},
	}})

	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while running update timestamp for logs migration: %s", err.Error())
	}
	return nil
}

// migrationAddRawRequestColumn adds the raw_request column to the logs table.
func migrationAddRawRequestColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_raw_request_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "raw_request"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "raw_request"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding raw request column: %s", err.Error())
	}
	return nil
}

// migrationAddImageGenerationOutputColumn adds the image_generation_output column to the logs table.
func migrationAddImageGenerationOutputColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_image_generation_output_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "image_generation_output"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "image_generation_output"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding image generation output column: %s", err.Error())
	}
	return nil
}

// migrationAddImageGenerationInputColumn adds the image_generation_input column to the logs table.
func migrationAddImageGenerationInputColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_image_generation_input_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "image_generation_input"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "image_generation_input"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding image generation input column: %s", err.Error())
	}
	return nil
}

// migrationAddRoutingRuleIDAndRoutingRuleNameColumns adds routing_rule_id and routing_rule_name to the logs table.
func migrationAddRoutingRuleIDAndRoutingRuleNameColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_routing_rule_id_and_routing_rule_name_columns"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "routing_rule_id"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "routing_rule_name"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "routing_rule_id"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "routing_rule_name"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding routing rule id and routing rule name columns: %s", err.Error())
	}
	return nil
}

// migrationAddRoutingEngineUsedColumn adds routing_engine_used when the plural column does not exist yet.
func migrationAddRoutingEngineUsedColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_routing_engine_used_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			// Only add the column if it doesn't exist
			if !migrator.HasColumn(&Log{}, "routing_engine_used") && !migrator.HasColumn(&Log{}, "routing_engines_used") {
				// Use raw SQL to avoid GORM struct field dependency
				if err := tx.Exec("ALTER TABLE logs ADD COLUMN routing_engine_used VARCHAR(255)").Error; err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "routing_engine_used"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding routing engine used column: %s", err.Error())
	}
	return nil
}

// migrationAddRoutingEnginesUsedColumn renames routing_engine_used to routing_engines_used or drops the legacy column.
func migrationAddRoutingEnginesUsedColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_routing_engines_used_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()

			hasOldColumn := migrator.HasColumn(&Log{}, "routing_engine_used")
			hasNewColumn := migrator.HasColumn(&Log{}, "routing_engines_used")

			if hasOldColumn && !hasNewColumn {
				// Rename old column to new if new doesn't exist yet
				if err := migrator.RenameColumn(&Log{}, "routing_engine_used", "routing_engines_used"); err != nil {
					return fmt.Errorf("failed to rename routing_engine_used to routing_engines_used: %w", err)
				}
			} else if hasOldColumn && hasNewColumn {
				// Both columns exist - drop the old one (new column is already in use)
				if err := dropColumnIfExists(tx, logger, &Log{}, "routing_engine_used"); err != nil {
					return fmt.Errorf("failed to drop old routing_engine_used column: %w", err)
				}
			}
			// If only new column exists, do nothing (already migrated)

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()

			hasNewColumn := migrator.HasColumn(&Log{}, "routing_engines_used")
			hasOldColumn := migrator.HasColumn(&Log{}, "routing_engine_used")

			if hasNewColumn && !hasOldColumn {
				// Rename new column back to old if old doesn't exist
				if err := migrator.RenameColumn(&Log{}, "routing_engines_used", "routing_engine_used"); err != nil {
					return fmt.Errorf("failed to rename routing_engines_used back to routing_engine_used: %w", err)
				}
			}
			// If old column was dropped, recreate it would be complex, so we skip

			return nil
		},
	}})

	return m.Migrate()
}

// migrationAddListModelsOutputColumn adds the list_models_output column to the logs table.
func migrationAddListModelsOutputColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_list_models_output_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "list_models_output"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "list_models_output"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding list models output column: %s", err.Error())
	}
	return nil
}

// migrationAddRerankOutputColumn adds the rerank_output column to the logs table.
func migrationAddRerankOutputColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_rerank_output_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "rerank_output"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "rerank_output"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding rerank output column: %s", err.Error())
	}
	return nil
}

// migrationAddRoutingEngineLogsColumn adds the routing_engine_logs column to the logs table.
func migrationAddRoutingEngineLogsColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_routing_engine_logs_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "routing_engine_logs"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "routing_engine_logs"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding routing engine logs column: %s", err.Error())
	}
	return nil
}

// migrationAddLargePayloadColumns adds is_large_payload_request and is_large_payload_response to the logs table.
func migrationAddLargePayloadColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_large_payload_columns"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "is_large_payload_request"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "is_large_payload_response"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "is_large_payload_request"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "is_large_payload_response"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding large payload columns: %s", err.Error())
	}
	return nil
}

// migrationCreateAsyncJobsTable creates the async_jobs table and its indexes if missing.
func migrationCreateAsyncJobsTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "async_jobs_init"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			dbMigrator := tx.Migrator()
			if !dbMigrator.HasTable(&AsyncJob{}) {
				logger.Info("[logstore] %s: creating table AsyncJob", migrationName)
				if err := dbMigrator.CreateTable(&AsyncJob{}); err != nil {
					return err
				}
			}

			// Explicitly create indexes as declared in struct tags
			if !dbMigrator.HasIndex(&AsyncJob{}, "idx_async_jobs_status") {
				logger.Info("[logstore] %s: creating index idx_async_jobs_status on AsyncJob", migrationName)
				if err := dbMigrator.CreateIndex(&AsyncJob{}, "idx_async_jobs_status"); err != nil {
					return fmt.Errorf("failed to create index on status: %w", err)
				}
			}

			if !dbMigrator.HasIndex(&AsyncJob{}, "idx_async_jobs_vk_id") {
				logger.Info("[logstore] %s: creating index idx_async_jobs_vk_id on AsyncJob", migrationName)
				if err := dbMigrator.CreateIndex(&AsyncJob{}, "idx_async_jobs_vk_id"); err != nil {
					return fmt.Errorf("failed to create index on virtual_key_id: %w", err)
				}
			}

			if !dbMigrator.HasIndex(&AsyncJob{}, "idx_async_jobs_expires_at") {
				logger.Info("[logstore] %s: creating index idx_async_jobs_expires_at on AsyncJob", migrationName)
				if err := dbMigrator.CreateIndex(&AsyncJob{}, "idx_async_jobs_expires_at"); err != nil {
					return fmt.Errorf("failed to create index on expires_at: %w", err)
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			logger.Info("[logstore] %s: dropping table AsyncJob", migrationName)
			return tx.Migrator().DropTable(&AsyncJob{})
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while creating async_jobs table: %s", err.Error())
	}
	return nil
}

// migrationAddAsyncJobRequestIDColumn adds the request_id column to the
// async_jobs table: the inference request id the background execution runs
// under, which is also the id its LLM log row is keyed by.
func migrationAddAsyncJobRequestIDColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "async_jobs_add_request_id_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return addColumnIfNotExists(tx, logger, &AsyncJob{}, "request_id")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return dropColumnIfExists(tx, logger, &AsyncJob{}, "request_id")
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding request_id column: %s", err.Error())
	}
	return nil
}

// migrationAddMetadataColumn adds the metadata JSON column to the logs table.
func migrationAddMetadataColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_metadata_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "metadata"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "metadata"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding metadata column: %s", err.Error())
	}
	return nil
}

// migrationAddHistogramCompositeIndexes adds a covering index that optimizes all 4 histogram queries.
// Without this, even though idx_logs_status_timestamp filters the WHERE clause correctly,
// SQLite must seek back to the main table to read aggregation columns (tokens, cost, model).
// With large rows (~800 KB of JSON per log entry), these main-table lookups dominate query time.
// A covering index includes all columns the histogram queries need, so SQLite resolves
// them entirely from the compact index B-tree (~100 bytes/entry) without touching the main table.
func migrationAddHistogramCompositeIndexes(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_histogram_composite_indexes"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			if tx.Dialector.Name() == "postgres" {
				return nil
			}
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()

			// Covering index for all 4 histogram queries with any combination of dashboard filters.
			//
			// Leading columns (status, timestamp) drive the range scan.
			// Filter columns (selected_key_id, virtual_key_id, etc.) let the DB evaluate
			// WHERE predicates directly from the index without main-table lookups.
			// Aggregation columns (model, cost, tokens) provide data for GROUP BY / SUM.
			//
			// Without these filter columns in the index, the DB must seek back to the
			// main table (~800 KB per row with JSON blobs) to check each filter,
			// turning a 17 ms query into a 35+ second one.
			if !migrator.HasIndex(&Log{}, "idx_logs_histogram_cover") {
				dialect := tx.Dialector.Name()

				var createSQL string
				switch dialect {
				case "mysql":
					// MySQL/MariaDB: InnoDB has a 3072-byte composite key limit.
					// With utf8mb4 each varchar(255) uses up to 1020 bytes, so use
					// prefix lengths (50 chars) to keep the total well under the limit.
					createSQL = `CREATE INDEX idx_logs_histogram_cover ON logs(
						status(50), timestamp,
						selected_key_id(50), virtual_key_id(50), routing_rule_id(50), provider(50), object_type(50),
						model(50), cost, prompt_tokens, completion_tokens, total_tokens
					)`
				default:
					// SQLite / PostgreSQL: no prefix-index limit concerns.
					createSQL = `CREATE INDEX IF NOT EXISTS idx_logs_histogram_cover ON logs(
						status, timestamp,
						selected_key_id, virtual_key_id, routing_rule_id, provider, object_type,
						model, cost, prompt_tokens, completion_tokens, total_tokens
					)`
				}

				if err := tx.Exec(createSQL).Error; err != nil {
					return fmt.Errorf("failed to create covering index for histograms: %w", err)
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()

			if migrator.HasIndex(&Log{}, "idx_logs_histogram_cover") {
				if err := tx.Exec("DROP INDEX IF EXISTS idx_logs_histogram_cover").Error; err != nil {
					return fmt.Errorf("failed to drop index idx_logs_histogram_cover: %w", err)
				}
			}

			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding histogram covering index: %s", err.Error())
	}
	return nil
}

// migrationAddVideoColumns adds video generation, retrieval, download, list, and delete payload columns to the logs table.
func migrationAddVideoColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_video_columns"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			videoColumns := []string{
				"video_generation_input",
				"video_generation_output",
				"video_retrieve_output",
				"video_download_output",
				"video_list_output",
				"video_delete_output",
			}

			for _, column := range videoColumns {
				if err := addColumnIfNotExists(tx, logger, &Log{}, column); err != nil {
					return err
				}
			}

			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			videoColumns := []string{
				"video_generation_input",
				"video_generation_output",
				"video_retrieve_output",
				"video_download_output",
				"video_list_output",
				"video_delete_output",
			}

			for _, column := range videoColumns {
				if err := dropColumnIfExists(tx, logger, &Log{}, column); err != nil {
					return err
				}
			}

			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding video columns: %s", err.Error())
	}
	return nil
}

// migrationAddProviderHistogramIndex records the migration version for the provider histogram
// index. Actual index creation is deferred to ensurePerformanceIndexes (called post-startup
// in a background goroutine) because CREATE INDEX CONCURRENTLY cannot run inside a
// transaction and a regular CREATE INDEX takes an AccessExclusiveLock that blocks all
// reads/writes on large tables.
func migrationAddProviderHistogramIndex(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_provider_histogram_index"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = false
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			// No-op: actual index creation is handled by ensurePerformanceIndexes
			// to avoid blocking pod startup on large tables.
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := tx.Exec("DROP INDEX IF EXISTS idx_logs_ts_provider_status").Error; err != nil {
				return fmt.Errorf("failed to drop index idx_logs_ts_provider_status: %w", err)
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding provider histogram index: %s", err.Error())
	}
	return nil
}

// migrationAddPassthroughRequestBodyColumn adds passthrough_request_body to the logs table.
func migrationAddPassthroughRequestBodyColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_passthrough_request_body_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "passthrough_request_body"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "passthrough_request_body"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding passthrough request body column: %s", err.Error())
	}
	return nil
}

// migrationAddPassthroughResponseBodyColumn adds passthrough_response_body to the logs table.
func migrationAddPassthroughResponseBodyColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_passthrough_response_body_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "passthrough_response_body"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "passthrough_response_body"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding passthrough response body column: %s", err.Error())
	}
	return nil
}

// migrationAddMetadataGINIndex adds a GIN index on the metadata column for Postgres
// to speed up jsonb ->> queries used for metadata filtering.
// For SQLite, this is a no-op since json_extract works without special indices.
func migrationAddMetadataGINIndex(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_metadata_gin_index_v3"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	// UseTransaction must be false because CREATE INDEX CONCURRENTLY cannot
	// run inside a transaction. This avoids deadlocks during rolling upgrades
	// where old pods are still writing to the logs table.
	opts := *migrator.DefaultOptions
	opts.UseTransaction = false
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if tx.Dialector.Name() == "postgres" {
				if err := tx.Exec("DROP INDEX IF EXISTS idx_logs_metadata_gin").Error; err != nil {
					return fmt.Errorf("failed to drop metadata GIN index: %w", err)
				}
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding metadata GIN index: %s", err.Error())
	}
	return nil
}

// migrationAddMultiTeamBusinessUnitGINIndexes registers the GIN indexes backing
// multi-team / multi-BU log filtering. Like migrationAddMetadataGINIndex, the
// build itself is deferred to ensureMultiTeamBusinessUnitGINIndexes (post-startup,
// background, CONCURRENTLY); this migration exists only to provide a rollback that
// drops the indexes. Postgres-only.
func migrationAddMultiTeamBusinessUnitGINIndexes(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_multi_team_bu_gin_indexes_v1"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = false
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if tx.Dialector.Name() == "postgres" {
				if err := tx.Exec("DROP INDEX IF EXISTS idx_logs_team_ids_gin").Error; err != nil {
					return fmt.Errorf("failed to drop team_ids GIN index: %w", err)
				}
				if err := tx.Exec("DROP INDEX IF EXISTS idx_logs_business_unit_ids_gin").Error; err != nil {
					return fmt.Errorf("failed to drop business_unit_ids GIN index: %w", err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while registering multi team/BU GIN indexes: %s", err.Error())
	}
	return nil
}

// ensureMetadataGINIndex checks whether idx_logs_metadata_gin exists and is valid.
// If the index is missing or was left in an INVALID state by a previously interrupted
// CREATE INDEX CONCURRENTLY, it drops the remnant and rebuilds the index synchronously.
//
// This is intentionally separate from the migrationAddMetadataGINIndex migration so that
// the long-running CREATE INDEX CONCURRENTLY does not block pod startup. Callers that
// want non-blocking behaviour should invoke this in a goroutine (see postgres.go).
func ensureMetadataGINIndex(ctx context.Context, conn *sql.Conn) error {
	// pg_index.indisvalid is false when a CONCURRENTLY build was interrupted;
	// a missing index reads as not valid.
	indexValid, err := pgIndexValid(ctx, conn, "logs", "idx_logs_metadata_gin")
	if err != nil {
		return err
	}

	if indexValid {
		if err := cleanupInvalidLogMetadata(ctx, conn); err != nil {
			return err
		}
		return nil
	}

	// Drop any INVALID remnant left by a prior interrupted CONCURRENTLY build.
	if err := pgDropIndexConcurrently(ctx, conn, "idx_logs_metadata_gin"); err != nil {
		return err
	}

	// Boost memory available for the sort phase so PostgreSQL needs fewer merge
	// passes. Non-fatal: a lower maintenance_work_mem just means a slower build.
	_, _ = conn.ExecContext(ctx, "SET maintenance_work_mem = '512MB'")

	// Allow parallel workers for the index build (supported since PG 11).
	// Non-fatal: falls back to a single worker on older versions.
	_, _ = conn.ExecContext(ctx, "SET max_parallel_maintenance_workers = 4")

	// Defensively clean up invalid metadata values before building the index.
	// This runs in small autocommitted batches to avoid one massive row-lock/WAL event.
	if err := cleanupInvalidLogMetadata(ctx, conn); err != nil {
		return err
	}

	// CONCURRENTLY takes only a ShareUpdateExclusiveLock, which is compatible with
	// RowExclusiveLock (INSERT/UPDATE/DELETE), so concurrent writes from other pods
	// are not blocked during the build.
	//
	// jsonb_path_ops stores one hash per JSON path rather than indexing every key
	// and value separately, making the index ~3x smaller and faster to build.
	// It supports the @> containment operator used by all metadata filter queries.
	//
	// The partial predicate (WHERE metadata IS NOT NULL AND metadata IS JSON OBJECT) skips NULL and non-object rows,
	// further reducing build time and index size. Queries that filter on metadata
	// always include an IS NOT NULL guard (rdb.go) so the planner will use this index.
	if _, err := conn.ExecContext(ctx, "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_metadata_gin ON logs USING gin ((metadata::jsonb) jsonb_path_ops) WHERE metadata IS NOT NULL AND metadata IS JSON OBJECT"); err != nil {
		return fmt.Errorf("failed to create metadata GIN index: %w", err)
	}
	return nil
}

// invalidMetadataCleanupJobID and cachedReadTokensBackfillJobID are the
// migrations-ledger ids that record the one-time background data jobs as done.
// They are not migration steps (the background goroutine runs the jobs after
// startup), so they never appear in logstoreMigrationSteps; the ledger is only
// where their completion is remembered across boots.
const (
	invalidMetadataCleanupJobID   = "logs_bg_invalid_metadata_cleanup_v1"
	cachedReadTokensBackfillJobID = "logs_bg_cached_read_tokens_backfill_v1"
)

// maintenanceWindow is the timestamp span one keyset maintenance statement
// covers. Each statement walks one hour of the timestamp index, so no single
// UPDATE touches more than an hour of traffic and no statement rescans rows an
// earlier one already passed.
const maintenanceWindow = time.Hour

// cleanupInvalidLogMetadata nulls metadata values that are not JSON objects so
// the metadata GIN index and the jsonb filters never see them. It runs once:
// completion is recorded in the migrations ledger and later boots skip the scan.
func cleanupInvalidLogMetadata(ctx context.Context, conn *sql.Conn) error {
	return runBackgroundJobOnce(ctx, conn, invalidMetadataCleanupJobID, func() error {
		return execWindowedMaintenanceUpdate(ctx, conn, "invalid metadata cleanup", `
			UPDATE logs
			SET metadata = NULL
			WHERE timestamp >= $1 AND timestamp < $2
			  AND metadata IS NOT NULL
			  AND metadata IS NOT JSON OBJECT
		`)
	})
}

// runBackgroundJobOnce runs job unless id is already recorded in the
// migrations ledger, and records id after job succeeds. A failed ledger read
// runs the job anyway (the jobs are idempotent), and a failed ledger write only
// means the next boot repeats the job, so neither blocks the index builds that
// follow.
func runBackgroundJobOnce(ctx context.Context, conn *sql.Conn, id string, job func() error) error {
	var recorded int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM migrations WHERE id = $1", id).Scan(&recorded); err == nil && recorded > 0 {
		return nil
	}
	if err := job(); err != nil {
		return err
	}
	recordBackgroundJob(ctx, conn, id)
	return nil
}

// recordBackgroundJob writes id to the migrations ledger with the same
// sequence/applied_at/status shape the migrator writes. A ledger created
// before those columns existed only has id, so that insert is the fallback.
func recordBackgroundJob(ctx context.Context, conn *sql.Conn, id string) {
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO migrations (id, sequence, applied_at, status)
		SELECT $1, COALESCE(MAX(sequence), 0) + 1, NOW(), 'success' FROM migrations
		ON CONFLICT (id) DO NOTHING
	`, id); err == nil {
		return
	}
	_, _ = conn.ExecContext(ctx, "INSERT INTO migrations (id) VALUES ($1) ON CONFLICT (id) DO NOTHING", id)
}

// execWindowedMaintenanceUpdate runs query once per maintenanceWindow from the
// oldest to the newest logs timestamp. query must take the window as $1
// (inclusive) and $2 (exclusive) on timestamp, so each statement is an index
// range scan. This replaces restart-from-the-start ctid batching, which
// rescanned every already-clean row on each batch.
func execWindowedMaintenanceUpdate(ctx context.Context, conn *sql.Conn, label string, query string) error {
	var minTS, maxTS sql.NullTime
	if err := conn.QueryRowContext(ctx, "SELECT MIN(timestamp), MAX(timestamp) FROM logs").Scan(&minTS, &maxTS); err != nil {
		return fmt.Errorf("failed to read %s range: %w", label, err)
	}
	if !minTS.Valid || !maxTS.Valid {
		return nil
	}
	for start := minTS.Time.Truncate(maintenanceWindow); !start.After(maxTS.Time); start = start.Add(maintenanceWindow) {
		if _, err := conn.ExecContext(ctx, query, start, start.Add(maintenanceWindow)); err != nil {
			return fmt.Errorf("failed to run %s window starting %s: %w", label, start.Format(time.RFC3339), err)
		}
	}
	return nil
}

// ensureArrayGINIndex builds a partial jsonb_path_ops GIN index on a JSON-array
// text column (e.g. team_ids) so `column::jsonb @> '[...]'` containment filters
// are indexed. Mirrors ensureMetadataGINIndex's lifecycle: tolerates an INVALID
// remnant from an interrupted build and builds CONCURRENTLY so writers are not
// blocked. No data cleanup is needed — the log writer only ever stores a valid
// JSON array or NULL in these columns. indexName/column are internal constants
// (not user input), so identifier interpolation is safe. Postgres-only.
func ensureArrayGINIndex(ctx context.Context, conn *sql.Conn, indexName, column string) error {
	indexValid, err := pgIndexValid(ctx, conn, "logs", indexName)
	if err != nil {
		return err
	}
	if indexValid {
		return nil
	}

	// Drop any INVALID remnant left by a prior interrupted CONCURRENTLY build.
	if err := pgDropIndexConcurrently(ctx, conn, indexName); err != nil {
		return err
	}

	// Non-fatal tuning to speed up the build.
	_, _ = conn.ExecContext(ctx, "SET maintenance_work_mem = '512MB'")
	_, _ = conn.ExecContext(ctx, "SET max_parallel_maintenance_workers = 4")

	// jsonb_path_ops supports @> (containment) and is ~3x smaller than the default
	// opclass. The partial predicate matches the IS JSON ARRAY guard the filter
	// query adds (rdb.go), so the planner uses this index.
	stmt := fmt.Sprintf(
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON logs USING gin ((%s::jsonb) jsonb_path_ops) WHERE %s IS NOT NULL AND %s IS JSON ARRAY",
		indexName, column, column, column,
	)
	if _, err := conn.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("failed to create GIN index %s: %w", indexName, err)
	}
	return nil
}

// ensureMultiTeamBusinessUnitGINIndexes builds the GIN indexes backing multi-team
// and multi-BU log filtering (team_ids / business_unit_ids).
func ensureMultiTeamBusinessUnitGINIndexes(ctx context.Context, conn *sql.Conn) error {
	if err := ensureArrayGINIndex(ctx, conn, "idx_logs_team_ids_gin", "team_ids"); err != nil {
		return err
	}
	if err := ensureArrayGINIndex(ctx, conn, "idx_logs_business_unit_ids_gin", "business_unit_ids"); err != nil {
		return err
	}
	return ensureArrayGINIndex(ctx, conn, "idx_logs_customer_ids_gin", "customer_ids")
}

// migrationAddDashboardEnhancements adds cached_read_tokens column to logs table.
// The expensive backfill and covering index rebuild are deferred
// to ensureDashboardEnhancements (called post-startup in a background goroutine) so
// they do not block pod startup on large tables.
func migrationAddDashboardEnhancements(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_dashboard_enhancements"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = false
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := addColumnIfNotExists(tx, logger, &Log{}, "CachedReadTokens"); err != nil {
				return fmt.Errorf("failed to add cached_read_tokens column: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)

			if err := dropColumnIfExists(tx, logger, &Log{}, "cached_read_tokens"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running dashboard enhancements migration: %s", err.Error())
	}
	return nil
}

// ensureDashboardEnhancements performs the expensive dashboard migration work that was
// deferred from migrationAddDashboardEnhancements: backfilling cached_read_tokens from
// the token_usage JSON and rebuilding the histogram covering index to include the new column.
//
// This is intentionally separate so that the long-running UPDATE and index rebuild do not
// block pod startup. Callers that want non-blocking behaviour should invoke this in a
// goroutine (see postgres.go). All operations are idempotent and safe to re-run.
func ensureDashboardEnhancements(ctx context.Context, conn *sql.Conn) error {
	// Backfill cached_read_tokens from token_usage JSON.
	// The extra `AND cached_read_tokens = 0` plus `AND COALESCE(...) > 0` makes
	// re-runs cheap: rows already backfilled have non-zero values (skipped),
	// and rows with genuinely zero cached tokens are also skipped (correct as-is).
	if err := backfillCachedReadTokens(ctx, conn); err != nil {
		return err
	}

	// Rebuild histogram covering index with cached_read_tokens included,
	// but only if missing or invalid (skip if already healthy).
	logsIndexValid, err := pgIndexValid(ctx, conn, "logs", "idx_logs_histogram_cover")
	if err != nil {
		return err
	}
	if !logsIndexValid {
		if err := pgDropIndexConcurrently(ctx, conn, "idx_logs_histogram_cover"); err != nil {
			return err
		}
		createLogsIndexSQL := `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_histogram_cover ON logs(
			status, timestamp,
			selected_key_id, virtual_key_id, routing_rule_id, provider, object_type,
			model, cost, prompt_tokens, completion_tokens, total_tokens, cached_read_tokens
		)`
		if _, err := conn.ExecContext(ctx, createLogsIndexSQL); err != nil {
			return fmt.Errorf("failed to create updated covering index: %w", err)
		}
	}

	return nil
}

// backfillCachedReadTokens copies cached_read_tokens out of token_usage for rows
// written before the column existed. It runs once: completion is recorded in
// the migrations ledger and later boots skip the scan. New rows are written with
// the column set, so nothing needs it afterwards.
func backfillCachedReadTokens(ctx context.Context, conn *sql.Conn) error {
	return runBackgroundJobOnce(ctx, conn, cachedReadTokensBackfillJobID, func() error {
		return execWindowedMaintenanceUpdate(ctx, conn, "cached_read_tokens backfill", `
			UPDATE logs
			SET cached_read_tokens = (token_usage::jsonb->'prompt_tokens_details'->>'cached_read_tokens')::int
			WHERE timestamp >= $1 AND timestamp < $2
			  AND cached_read_tokens = 0
			  AND token_usage IS NOT NULL
			  AND token_usage != ''
			  AND token_usage != 'null'
			  AND token_usage ~ '^\s*\{.*\}\s*$'
			  AND COALESCE((token_usage::jsonb->'prompt_tokens_details'->>'cached_read_tokens')::int, 0) > 0
		`)
	})
}

// execBatchedGormMaintenanceUpdate repeats a LIMIT-$1 maintenance update inside a
// migration until a batch affects no rows.
func execBatchedGormMaintenanceUpdate(tx *gorm.DB, label string, query string) error {
	for {
		result := tx.Exec(query, maintenanceUpdateBatchSize)
		if result.Error != nil {
			return fmt.Errorf("failed to run %s batch: %w", label, result.Error)
		}
		if result.RowsAffected == 0 {
			return nil
		}
	}
}

// migrationAddLogsAndDashboardPerformanceIndexes records the migration version for the performance
// indexes. Actual index creation is deferred to ensurePerformanceIndexes (called
// post-startup in a background goroutine) because CREATE INDEX CONCURRENTLY cannot
// run inside a transaction.
func migrationAddLogsAndDashboardPerformanceIndexes(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_and_dashboard_performance_indexes"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = false
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			// No-op: actual index creation is handled by ensurePerformanceIndexes
			// to avoid blocking pod startup.
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			if tx.Dialector.Name() != "postgres" {
				return nil
			}
			tx = tx.WithContext(ctx)
			for _, indexName := range []string{
				"idx_logs_content_summary_fts",
				"idx_logs_routing_engines_arr",
			} {
				if err := tx.Exec("DROP INDEX CONCURRENTLY IF EXISTS " + indexName).Error; err != nil {
					return fmt.Errorf("failed to drop performance index %s: %w", indexName, err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error recording performance gin indexes migration: %w", err)
	}
	return nil
}

// performanceIndexDef is the table name, index name, and CREATE INDEX SQL for one Postgres index.
type performanceIndexDef struct {
	table string
	name  string
	sql   string
}

// ftsInputCharLimit is the maximum number of characters fed to to_tsvector via left().
// It must be low enough that even worst-case multi-byte / high-lexeme content keeps
// the tsvector output under Postgres's 1,048,575-byte hard limit. The same constant
// must be used in both the index expression and the query predicate so the planner
// recognises the expression match and uses the GIN index.
const ftsInputCharLimit = 250000

// performanceIndexes is the set of full-text and GIN indexes built by ensurePerformanceIndexes.
// Each statement uses CREATE INDEX CONCURRENTLY to avoid blocking writes.
var performanceIndexes = []performanceIndexDef{
	{
		table: "logs",
		name:  "idx_logs_latency",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_latency ON logs(latency)",
	},
	{
		table: "logs",
		name:  "idx_logs_total_tokens",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_total_tokens ON logs(total_tokens)",
	},
	{
		table: "logs",
		name:  "idx_logs_timestamp",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_timestamp ON logs(timestamp)",
	},
	{
		table: "logs",
		name:  "idx_logs_status",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_status ON logs(status)",
	},
	{
		table: "logs",
		name:  "idx_logs_created_at",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_created_at ON logs(created_at)",
	},
	{
		table: "logs",
		name:  "idx_logs_provider",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_provider ON logs(provider)",
	},
	{
		table: "logs",
		name:  "idx_logs_model",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_model ON logs(model)",
	},
	{
		table: "logs",
		name:  "idx_logs_object_type",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_object_type ON logs(object_type)",
	},
	{
		table: "logs",
		name:  "idx_logs_cost",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_cost ON logs(cost)",
	},
	// The logs list sorts cost and latency DESC NULLS LAST so requests with no
	// value trail the ones that have it. A plain btree only yields ASC NULLS LAST
	// or DESC NULLS FIRST, so without these the DESC sort seq-scans every row
	// that survives the WHERE clause. ASC NULLS LAST is served by idx_logs_cost
	// and idx_logs_latency. id DESC covers the tiebreak in searchLogs' ORDER BY.
	// Not partial: the query keeps NULL rows, so a partial index is unusable.
	{
		table: "logs",
		name:  "idx_logs_cost_desc_nulls_last",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_cost_desc_nulls_last ON logs(cost DESC NULLS LAST, id DESC)",
	},
	{
		table: "logs",
		name:  "idx_logs_latency_desc_nulls_last",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_latency_desc_nulls_last ON logs(latency DESC NULLS LAST, id DESC)",
	},
	{
		table: "logs",
		name:  "idx_logs_status_timestamp",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_status_timestamp ON logs(status, timestamp)",
	},
	{
		table: "logs",
		name:  "idx_logs_status_created_at",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_status_created_at ON logs(status, created_at)",
	},
	{
		table: "logs",
		name:  "idx_logs_content_summary_fts",
		// left() caps input characters to stay within to_tsvector's 1MB output limit.
		// Use ftsInputCharLimit: even worst-case UTF-8 / high-lexeme content stays
		// well under the 1,048,575-byte tsvector limit.
		sql: fmt.Sprintf("CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_content_summary_fts ON logs USING GIN (to_tsvector('simple', left(content_summary, %d))) WHERE content_summary IS NOT NULL", ftsInputCharLimit),
	},
	{
		table: "logs",
		name:  "idx_logs_routing_engines_arr",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_routing_engines_arr ON logs USING GIN (string_to_array(routing_engines_used, ',')) WHERE routing_engines_used IS NOT NULL",
	},
	{
		table: "logs",
		name:  "idx_logs_tool_call_names_arr",
		// Backs the tool_call_names filter's array-overlap predicate; partial so
		// the (dominant) rows that called no tools stay out of the index.
		sql: "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_tool_call_names_arr ON logs USING GIN (string_to_array(tool_call_names, ',')) WHERE tool_call_names IS NOT NULL",
	},
	{
		table: "logs",
		name:  "idx_logs_ts_provider_status",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_ts_provider_status ON logs(timestamp, provider, status)",
	},
	{
		table: "logs",
		name:  "idx_logs_alias",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_alias ON logs(alias)",
	},
	{
		table: "logs",
		name:  "idx_logs_canonical_model_name",
		// Filtering by model matches either `model` or `canonical_model_name`
		// (see applyFilters). This single-column index lets Postgres BitmapOr it
		// with idx_logs_model instead of scanning the whole table for the OR branch.
		sql: "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_canonical_model_name ON logs(canonical_model_name) WHERE canonical_model_name IS NOT NULL",
	},
	{
		table: "logs",
		name:  "idx_logs_parent_request_id",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_parent_request_id ON logs(parent_request_id) WHERE parent_request_id IS NOT NULL",
	},
	{
		table: "logs",
		name:  "idx_logs_status_parent_request_id",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_status_parent_request_id ON logs(status, parent_request_id) WHERE parent_request_id IS NOT NULL",
	},
	{
		table: "logs",
		name:  "idx_logs_stop_reason",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_stop_reason ON logs(stop_reason)",
	},
	{
		table: "logs",
		name:  "idx_logs_complexity_tier",
		// Partial: only requests routed through a complexity_tier rule carry a tier,
		// so the index skips the (dominant) NULL rows.
		sql: "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_complexity_tier ON logs(complexity_tier) WHERE complexity_tier IS NOT NULL",
	},
	{
		table: "logs",
		name:  "idx_logs_complexity_mechanism",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_complexity_mechanism ON logs(complexity_mechanism) WHERE complexity_mechanism IS NOT NULL",
	},
	{
		table: "logs",
		name:  "idx_logs_model_router_id",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_model_router_id ON logs(model_router_id) WHERE model_router_id IS NOT NULL",
	},
	{
		table: "logs",
		name:  "idx_logs_session_id",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_session_id ON logs(session_id) WHERE session_id IS NOT NULL",
	},
	{
		table: "logs",
		// Session grouping keeps each session's earliest chain root, which probes
		// a session's peers in timestamp order.
		name: "idx_logs_session_id_timestamp",
		sql:  "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_session_id_timestamp ON logs(session_id, timestamp) WHERE session_id IS NOT NULL",
	},
	{
		table: "logs",
		name:  "idx_logs_agent_correlation_id",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_agent_correlation_id ON logs(agent_correlation_id) WHERE agent_correlation_id IS NOT NULL",
	},
	{
		table: "logs",
		name:  "idx_logs_user_agent",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_user_agent ON logs(user_agent)",
	},
	{
		table: "logs",
		name:  "idx_logs_app",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_app ON logs(app)",
	},
	{
		table: "logs",
		name:  "idx_logs_cluster_node_id",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_cluster_node_id ON logs(cluster_node_id, timestamp) WHERE cluster_node_id IS NOT NULL",
	},
	{
		table: "logs",
		name:  "idx_logs_cluster_node_usage",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_cluster_node_usage ON logs(cluster_node_id, status, timestamp, id) WHERE cluster_node_id IS NOT NULL",
	},
	{
		table: "logs",
		name:  "idx_logs_cluster_node_inc_usage",
		sql:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_cluster_node_inc_usage ON logs(cluster_node_id, status, inc_number) WHERE cluster_node_id IS NOT NULL AND inc_number IS NOT NULL",
	},
}

// ensurePerformanceIndexes checks whether each performance GIN index exists and is
// valid. If an index is missing or was left in an INVALID state by a previously
// interrupted CREATE INDEX CONCURRENTLY, it drops the remnant and rebuilds.
//
// This is intentionally separate from migrationAddPerformanceGINIndexes so that the
// long-running CREATE INDEX CONCURRENTLY does not block pod startup. Callers that
// want non-blocking behaviour should invoke this in a goroutine (see postgres.go).
func ensurePerformanceIndexes(ctx context.Context, conn *sql.Conn, logger schemas.Logger) error {
	// Boost memory for sort phase during index builds.
	_, _ = conn.ExecContext(ctx, "SET maintenance_work_mem = '512MB'")
	_, _ = conn.ExecContext(ctx, "SET max_parallel_maintenance_workers = 4")

	for _, idx := range performanceIndexes {
		// Check if the index exists and is valid.
		indexValid, err := pgIndexValid(ctx, conn, idx.table, idx.name)
		if err != nil {
			return err
		}
		if indexValid {
			continue
		}

		logger.Info("[logstore] building performance index %s on %s", idx.name, idx.table)

		// Drop any INVALID remnant left by a prior interrupted CONCURRENTLY build.
		if err := pgDropIndexConcurrently(ctx, conn, idx.name); err != nil {
			return err
		}

		// Create the index concurrently (does not block writes).
		if _, err := conn.ExecContext(ctx, idx.sql); err != nil {
			return fmt.Errorf("failed to create index %s: %w", idx.name, err)
		}
		logger.Info("[logstore] built performance index %s on %s", idx.name, idx.table)
	}

	return nil
}

// ownerTimestampIndexDef is one (owner, timestamp) composite index and the
// single-column owner index it replaces.
type ownerTimestampIndexDef struct {
	table    string
	name     string
	column   string
	replaces string
}

// createSQL returns the CONCURRENTLY build statement for the composite.
func (d ownerTimestampIndexDef) createSQL() string {
	return fmt.Sprintf("CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s(%s, timestamp)", d.name, d.table, d.column)
}

// ownerTimestampIndexes back the logs list filtered by virtual key,
// user or team and ordered by timestamp DESC with a LIMIT. A composite leading
// on the owner column serves every equality lookup the single-column index
// served, so the single-column index is dropped once the composite is valid.
// The struct tags on Log define the same names, so fresh
// installs build them with the table.
var ownerTimestampIndexes = []ownerTimestampIndexDef{
	{table: "logs", name: "idx_logs_vk_ts", column: "virtual_key_id", replaces: "idx_logs_virtual_key_id"},
	{table: "logs", name: "idx_logs_user_ts", column: "user_id", replaces: "idx_logs_user_id"},
	{table: "logs", name: "idx_logs_team_ts", column: "team_id", replaces: "idx_logs_team_id"},
}

// rankingNameTimestampIndexes back the ranking name fallback: for each ranked id
// the view could not name, the latest logged name is one backward walk of the
// (id, timestamp) composite that stops at the first row carrying a name. Like
// ownerTimestampIndexes, each replaces the single-column index on its id.
var rankingNameTimestampIndexes = []ownerTimestampIndexDef{
	{table: "logs", name: "idx_logs_selected_key_ts", column: "selected_key_id", replaces: "idx_logs_selected_key_id"},
	{table: "logs", name: "idx_logs_routing_rule_ts", column: "routing_rule_id", replaces: "idx_logs_routing_rule_id"},
	{table: "logs", name: "idx_logs_customer_ts", column: "customer_id", replaces: "idx_logs_customer_id"},
	{table: "logs", name: "idx_logs_business_unit_ts", column: "business_unit_id", replaces: "idx_logs_business_unit_id"},
	{table: "logs", name: "idx_logs_project_ts", column: "project_id", replaces: "idx_logs_project_id"},
}

// pgIndexValid reports whether the named index on table exists and is valid in
// the connection's current schema. An interrupted CREATE INDEX CONCURRENTLY
// leaves the index present but invalid. A same-named table and index in another
// schema must not count, or the builder would skip the build and then drop this
// schema's replaced index with no replacement.
func pgIndexValid(ctx context.Context, conn *sql.Conn, table, name string) (bool, error) {
	var valid bool
	err := conn.QueryRowContext(ctx, `
		SELECT COALESCE(bool_and(pi.indisvalid), false)
		FROM pg_class pc
		JOIN pg_index pi ON pi.indrelid = pc.oid
		JOIN pg_class ic ON ic.oid = pi.indexrelid
		WHERE pc.relnamespace = to_regnamespace(current_schema())
		  AND pc.relname = $1
		  AND ic.relname = $2
	`, table, name).Scan(&valid)
	if err != nil {
		return false, fmt.Errorf("failed to check index %s validity: %w", name, err)
	}
	return valid, nil
}

// pgDropIndexConcurrently drops an INVALID remnant left by an interrupted
// CREATE INDEX CONCURRENTLY, so the build that follows starts clean. The name is
// qualified with the current schema, the one pgIndexValid checks: unqualified,
// a missing index here would resolve to a same-named index in a later
// search_path schema and drop that one instead.
//
// A build another backend is still running also reads as INVALID (see
// pgWaitForIndexBuild), so it waits for that build first and skips the drop
// when the build finished valid; every caller rebuilds with IF NOT EXISTS,
// which then keeps it.
func pgDropIndexConcurrently(ctx context.Context, conn *sql.Conn, name string) error {
	qualified, err := pgQualifiedIndex(ctx, conn, name)
	if err != nil {
		return err
	}
	waited, err := pgWaitForIndexBuild(ctx, conn, qualified)
	if err != nil {
		return err
	}
	if waited {
		var valid bool
		if err := conn.QueryRowContext(ctx, `SELECT COALESCE((SELECT indisvalid FROM pg_index
			WHERE indexrelid = to_regclass($1)), false)`, qualified).Scan(&valid); err != nil {
			return fmt.Errorf("failed to check index %s validity: %w", qualified, err)
		}
		if valid {
			return nil
		}
	}
	if _, err := conn.ExecContext(ctx, "DROP INDEX CONCURRENTLY IF EXISTS "+qualified); err != nil {
		return fmt.Errorf("failed to drop invalid index %s: %w", qualified, err)
	}
	return nil
}

// concurrentIndexBuildPollInterval is how often pgWaitForIndexBuild re-checks a
// concurrent build that another backend is still running.
const concurrentIndexBuildPollInterval = 2 * time.Second

// pgRowQuerier is the QueryRowContext of a *sql.Conn or a gorm ConnPool.
type pgRowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// pgWaitForIndexBuild blocks while a backend is still running CREATE INDEX
// CONCURRENTLY (or REINDEX CONCURRENTLY) for index, and reports whether it waited.
//
// An index under a concurrent build reads indisvalid=false, the same as the
// leftover of an interrupted build, and that build can outlive the pod that
// started it: when a pod is killed, Postgres ends its idle advisory-lock session
// at once but keeps the busy build running until it next touches the socket. The
// next pod then holds the migration lock and sees what looks like a leftover.
// Dropping it blocks behind the live build and then fails one side with "tuple
// concurrently updated". Callers that drop an INVALID index call this first and
// re-check validity when it waited.
//
// Postgres hides index_relid in pg_stat_progress_create_index for backends of
// another role unless the caller has pg_read_all_stats. Bifrost nodes share one
// role; a hidden build reads as "not building" and keeps the drop-and-rebuild path.
func pgWaitForIndexBuild(ctx context.Context, q pgRowQuerier, index string) (bool, error) {
	for waited := false; ; waited = true {
		var building bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_progress_create_index
			WHERE index_relid = to_regclass($1))`, index).Scan(&building); err != nil {
			return waited, fmt.Errorf("failed to check concurrent build of %s: %w", index, err)
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

// pgQualifiedIndex returns name qualified with the connection's current schema,
// so DROP INDEX never resolves to a same-named index in another schema.
func pgQualifiedIndex(ctx context.Context, conn *sql.Conn, name string) (string, error) {
	var schema string
	if err := conn.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		return "", fmt.Errorf("failed to read current schema: %w", err)
	}
	return pgx.Identifier{schema, name}.Sanitize(), nil
}

// ensureOwnerTimestampIndexes builds each (owner, timestamp) composite with
// CREATE INDEX CONCURRENTLY, dropping an INVALID leftover first, and then drops
// the single-column index it replaces with DROP INDEX CONCURRENTLY. The drop
// happens only after the composite is confirmed valid, so an owner lookup
// always has an index. Idempotent; Postgres only (see postgres.go).
func ensureOwnerTimestampIndexes(ctx context.Context, conn *sql.Conn, logger schemas.Logger) error {
	_, _ = conn.ExecContext(ctx, "SET maintenance_work_mem = '512MB'")
	_, _ = conn.ExecContext(ctx, "SET max_parallel_maintenance_workers = 4")

	for _, idx := range append(append([]ownerTimestampIndexDef{}, ownerTimestampIndexes...), rankingNameTimestampIndexes...) {
		valid, err := pgIndexValid(ctx, conn, idx.table, idx.name)
		if err != nil {
			return err
		}
		if !valid {
			logger.Info("[logstore] building index %s on %s", idx.name, idx.table)
			if err := pgDropIndexConcurrently(ctx, conn, idx.name); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, idx.createSQL()); err != nil {
				return fmt.Errorf("failed to create index %s: %w", idx.name, err)
			}
			if valid, err = pgIndexValid(ctx, conn, idx.table, idx.name); err != nil {
				return err
			}
			if !valid {
				return fmt.Errorf("index %s is still invalid after build", idx.name)
			}
			logger.Info("[logstore] built index %s on %s", idx.name, idx.table)
		}
		qualifiedReplaced, err := pgQualifiedIndex(ctx, conn, idx.replaces)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "DROP INDEX CONCURRENTLY IF EXISTS "+qualifiedReplaced); err != nil {
			return fmt.Errorf("failed to drop index %s replaced by %s: %w", idx.replaces, idx.name, err)
		}
	}
	return nil
}

// migrationAddOwnerTimestampIndexes swaps the single-column virtual_key_id,
// user_id and team_id indexes on logs for (owner, timestamp)
// composites on SQLite and other non-Postgres databases. Postgres builds the
// same indexes CONCURRENTLY in the background (ensureOwnerTimestampIndexes),
// because a plain CREATE INDEX there would block writes on a large table.
func migrationAddOwnerTimestampIndexes(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	return runTimestampIndexMigration(ctx, db, logger, "logs_add_owner_timestamp_indexes", ownerTimestampIndexes)
}

// migrationAddRankingNameTimestampIndexes swaps the single-column
// selected_key_id, routing_rule_id, customer_id, business_unit_id and project_id
// indexes on logs for (id, timestamp) composites on SQLite and other
// non-Postgres databases; Postgres builds them CONCURRENTLY in the background
// (ensureOwnerTimestampIndexes).
func migrationAddRankingNameTimestampIndexes(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	return runTimestampIndexMigration(ctx, db, logger, "logs_add_ranking_name_timestamp_indexes", rankingNameTimestampIndexes)
}

// runTimestampIndexMigration records migrationName and, off Postgres, builds each
// composite in defs and drops the single-column index it replaces.
func runTimestampIndexMigration(ctx context.Context, db *gorm.DB, logger schemas.Logger, migrationName string, defs []ownerTimestampIndexDef) error {
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = false
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			if tx.Dialector.Name() == "postgres" {
				return nil
			}
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()
			for _, idx := range defs {
				model := ownerTimestampIndexModel(idx.table)
				if !mg.HasIndex(model, idx.name) {
					if err := mg.CreateIndex(model, idx.name); err != nil {
						return fmt.Errorf("failed to create index %s: %w", idx.name, err)
					}
				}
				if mg.HasIndex(model, idx.replaces) {
					if err := mg.DropIndex(model, idx.replaces); err != nil {
						return fmt.Errorf("failed to drop index %s: %w", idx.replaces, err)
					}
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			// Postgres indexes belong to the background builder; rolling back this
			// ledger entry must not take a write-blocking lock on logs.
			if tx.Dialector.Name() == "postgres" {
				return nil
			}
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()
			for _, idx := range defs {
				model := ownerTimestampIndexModel(idx.table)
				if err := tx.Exec(fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s(%s)", idx.replaces, idx.table, idx.column)).Error; err != nil {
					return fmt.Errorf("failed to restore index %s: %w", idx.replaces, err)
				}
				if mg.HasIndex(model, idx.name) {
					if err := mg.DropIndex(model, idx.name); err != nil {
						return fmt.Errorf("failed to drop index %s: %w", idx.name, err)
					}
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

// ownerTimestampIndexModel returns the GORM model for an ownerTimestampIndexes table.
func ownerTimestampIndexModel(string) any {
	return &Log{}
}

// migrationAddImageEditInputColumn adds the image_edit_input column to the logs table.
func migrationAddImageEditInputColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_image_edit_input_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "image_edit_input"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "image_edit_input"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding image edit input column: %s", err.Error())
	}
	return nil
}

// migrationAddPluginLogsColumn adds the plugin_logs column to the logs table.
func migrationAddPluginLogsColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_plugin_logs_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "plugin_logs"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "plugin_logs"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding plugin logs column: %s", err.Error())
	}
	return nil
}

// migrationAddBatchDebugColumn adds the batch_debug column to the logs table.
// It carries provider request counts on batch rows and the per-model pricing
// breakdown on the aggregate cost row. Nullable and unindexed, so this is a
// metadata-only DDL change with no table rewrite and no backfill: rows written
// before it simply have no batch detail.
func migrationAddBatchDebugColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_batch_debug_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			return addColumnIfNotExists(tx.WithContext(ctx), logger, &Log{}, "batch_debug")
		},
		Rollback: func(tx *gorm.DB) error {
			return dropColumnIfExists(tx.WithContext(ctx), logger, &Log{}, "batch_debug")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding batch_debug column: %s", err.Error())
	}
	return nil
}

// migrationAddAliasColumn adds the alias column to the logs table.
// The alias field stores the original model name the caller used when routing resolved it to a different model via alias mapping.
// Index creation is deferred to ensurePerformanceIndexes (called post-startup in a background goroutine)
// because CREATE INDEX CONCURRENTLY cannot run inside a transaction and a regular CREATE INDEX
// takes a SHARE lock that blocks writes on large tables during rolling deploys.
func migrationAddAliasColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_alias_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "alias"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "alias"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding alias column: %s", err.Error())
	}
	return nil
}

// migrationAddCanonicalModelColumns adds the canonical_model_name and
// alias_model_family columns to the logs table. Both are copied from the resolved
// alias config when the request's model was resolved via alias mapping and the
// alias defines them.
func migrationAddCanonicalModelColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_canonical_model_columns_v2"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, column := range []string{"canonical_model_name", "alias_model_family"} {
				if err := addColumnIfNotExists(tx, logger, &Log{}, column); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, column := range []string{"canonical_model_name", "alias_model_family"} {
				if err := dropColumnIfExists(tx, logger, &Log{}, column); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding canonical model columns: %s", err.Error())
	}
	return nil
}

// migrationAddCostBreakdownColumns adds the denormalized input/output/additional
// cost columns (input_cost, output_cost, additional_cost) to the logs table. The
// total cost already lives in the cost column; these split it so per-model quota
// usage can be summed by category in SQL (input + output + additional == total).
// New rows populate them on write from the TokenUsage cost breakdown; existing
// rows keep 0 (their total cost column is unaffected).
func migrationAddCostBreakdownColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_cost_breakdown_columns"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, column := range []string{"input_cost", "output_cost", "additional_cost"} {
				if err := addColumnIfNotExists(tx, logger, &Log{}, column); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, column := range []string{"input_cost", "output_cost", "additional_cost"} {
				if err := dropColumnIfExists(tx, logger, &Log{}, column); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding cost breakdown columns: %s", err.Error())
	}
	return nil
}

// migrationAddHasObjectColumn adds the has_object boolean column to the logs table.
// Used by the hybrid log store to track whether a log's payload is stored in object storage.
func migrationAddHasObjectColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_has_object_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "has_object"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "has_object"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding has_object column: %s", err.Error())
	}
	return nil
}

// migrationAddContentHiddenColumn adds the content_hidden boolean column to the logs table.
// Marks logs whose payload is retained in object storage but must never be served back
// through the API/UI.
func migrationAddContentHiddenColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_content_hidden_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "content_hidden"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "content_hidden"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding content_hidden column: %w", err)
	}
	return nil
}

// migrationAddImageVariationInputColumn adds the image_variation_input column to the logs table.
func migrationAddImageVariationInputColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_image_variation_input_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "image_variation_input"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "image_variation_input"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding image variation input column: %s", err.Error())
	}
	return nil
}

// migrationAddUserNameColumn adds the user_name column to the logs table.
// Adding a nullable column is instant in Postgres (metadata-only change, no table rewrite).
func migrationAddUserNameColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_user_name_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "user_name"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "user_name"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding user_name column: %s", err.Error())
	}
	return nil
}

// migrationAddGovernanceContextColumns adds user_id, team_id, team_name, customer_id, customer_name,
// business_unit_id, business_unit_name columns to the logs table.
func migrationAddGovernanceContextColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_governance_context_columns"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true

	columns := []string{"user_id", "team_id", "team_name", "customer_id", "customer_name", "business_unit_id", "business_unit_name"}

	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, col := range columns {
				if err := addColumnIfNotExists(tx, logger, &Log{}, col); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, col := range columns {
				if err := dropColumnIfExists(tx, logger, &Log{}, col); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding governance context columns: %s", err.Error())
	}
	return nil
}

// migrationAddMultiTeamBusinessUnitColumns adds the JSON-array columns capturing
// the full deduped set of teams / business units a request belongs to (enterprise
// user/AP path). The scalar team_id/business_unit_id remain the primary; these
// power display, multi-team filtering (jsonb @> + GIN), and fan-out aggregation.
// migrationAddProjectColumns adds the project dimension to the request log. A request records the
// project it was scoped to the way it records the team or business unit it was made under: as the
// resolved id and its display name, so a log row still reads correctly after the project is renamed
// or deleted.
//
// Indexed on the id alone, because that is what filtering and rollups ask about; the name is only
// ever read back off a row that has already been found. The index is built CONCURRENTLY by
// ensurePerformanceIndexes (an entry appended to performanceIndexes), since addColumnIfNotExists
// adds only the column and an in-migration build would block writes on a populated table.
func migrationAddProjectColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_project_columns"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true

	columns := []string{"project_id", "project_name"}

	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, col := range columns {
				if err := addColumnIfNotExists(tx, logger, &Log{}, col); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, col := range columns {
				if err := dropColumnIfExists(tx, logger, &Log{}, col); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding project columns: %s", err.Error())
	}
	return nil
}

// migrationAddMultiTeamBusinessUnitColumns adds the JSON-array team and business-unit
// id/name columns to logs.
func migrationAddMultiTeamBusinessUnitColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_multi_team_business_unit_columns"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true

	columns := []string{"team_ids", "team_names", "business_unit_ids", "business_unit_names"}

	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, col := range columns {
				if err := addColumnIfNotExists(tx, logger, &Log{}, col); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, col := range columns {
				if err := dropColumnIfExists(tx, logger, &Log{}, col); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding multi team/business-unit columns: %s", err.Error())
	}
	return nil
}

// migrationRecreateMatViewsWithGovernanceColumns drops and recreates materialized views
// so they include the new governance context columns (user_id, team_id, customer_id, business_unit_id).
// The actual rebuild is deferred to ensureMatViews, which runs after startup on
// a dedicated connection. Dropping materialized views inline in this migration
// can queue heavy locks during rolling deploys on large log tables.
func migrationRecreateMatViewsWithGovernanceColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_recreate_matviews_with_governance_columns"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			// No rollback needed — ensureMatViews will recreate on next startup
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while recreating matviews with governance columns: %s", err.Error())
	}
	return nil
}

// migrationAddUserAgentColumn adds the user_agent and app columns to the logs table.
// user_agent stores the raw HTTP User-Agent verbatim; app stores the backend-
// detected client app (Claude Code, Codex, Cursor, ...).
//
// Indexes on user_agent and app are built CONCURRENTLY by ensurePerformanceIndexes
// (entries appended to performanceIndexes) so adding them does not block writes on
// a populated table.
func migrationAddUserAgentColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: "logs_add_user_agent_column",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			if !migrator.HasColumn(&Log{}, "user_agent") {
				if err := migrator.AddColumn(&Log{}, "user_agent"); err != nil {
					return err
				}
			}
			if !migrator.HasColumn(&Log{}, "app") {
				if err := migrator.AddColumn(&Log{}, "app"); err != nil {
					return err
				}
			}
			if !migrator.HasTable(&UserAgentMapping{}) {
				if err := migrator.CreateTable(&UserAgentMapping{}); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			migrator := tx.Migrator()
			if migrator.HasIndex(&Log{}, "idx_logs_app") {
				if err := migrator.DropIndex(&Log{}, "idx_logs_app"); err != nil {
					return err
				}
			}
			if migrator.HasIndex(&Log{}, "idx_logs_user_agent") {
				if err := migrator.DropIndex(&Log{}, "idx_logs_user_agent"); err != nil {
					return err
				}
			}
			if migrator.HasTable(&UserAgentMapping{}) {
				if err := migrator.DropTable(&UserAgentMapping{}); err != nil {
					return err
				}
			}
			if migrator.HasColumn(&Log{}, "app") {
				if err := migrator.DropColumn(&Log{}, "app"); err != nil {
					return err
				}
			}
			if migrator.HasColumn(&Log{}, "user_agent") {
				if err := migrator.DropColumn(&Log{}, "user_agent"); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding user_agent column: %s", err.Error())
	}
	return nil
}

// migrationRecreateMatViewsWithUserAgentColumn is a marker migration: the actual
// rebuild of mv_logs_hourly (now grouped by app, not raw user_agent) and the
// creation of mv_filter_apps happen on the next PostgreSQL startup via
// ensureMatViews / repairMatViewShapes, which detect the required app column
// and drop+recreate the drifted view. The rebuild is intentionally deferred to
// startup (not done inline here) to avoid heavy AccessExclusiveLock churn during
// rolling deploys on large logs tables. The user_agent_mappings table guard
// also runs here for local DBs that already recorded the edited column migration
// before the table was added to it.
func migrationRecreateMatViewsWithUserAgentColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: "logs_recreate_matviews_with_app_column",
		Migrate: func(tx *gorm.DB) error {
			migrator := tx.Migrator()
			if !migrator.HasTable(&UserAgentMapping{}) {
				if err := migrator.CreateTable(&UserAgentMapping{}); err != nil {
					return fmt.Errorf("failed to create user_agent_mappings table: %w", err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			// No rollback needed — ensureMatViews recreates on next startup.
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while recreating matviews with app column: %s", err.Error())
	}
	return nil
}

// migrationRecreateMatViewsWithCostBreakdown is a marker migration: the actual
// rebuild of mv_logs_hourly (now carrying total_input_cost / total_output_cost /
// total_additional_cost alongside total_cost) happens on the next PostgreSQL
// startup via ensureMatViews / repairMatViewShapes, which detect the new required
// columns and drop+recreate the drifted view. The rebuild is deferred to startup
// (not done inline here) to avoid heavy AccessExclusiveLock churn during rolling
// deploys on large logs tables.
func migrationRecreateMatViewsWithCostBreakdown(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: "logs_recreate_matviews_with_cost_breakdown",
		Migrate: func(tx *gorm.DB) error {
			// No-op: the drifted mv_logs_hourly is dropped and recreated on the
			// next startup by repairMatViewShapes once it sees the new columns.
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			// No rollback needed — ensureMatViews recreates on next startup.
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while recreating matviews with cost breakdown columns: %s", err.Error())
	}
	return nil
}

// migrationSplitFilterDataMatView drops the legacy mv_logs_filterdata view so
// ensureMatViews recreates it as per-dimension matviews (mv_filter_models,
// mv_filter_selected_keys, ...). The old view DISTINCTed across 16 columns and
// could grow nearly as large as the source `logs` table on multi-tenant
// deployments, making REFRESH ... CONCURRENTLY memory-intensive. Splitting per
// dimension keeps each view bounded by a single column's cardinality.
//
// NOT CALLED YET. Multi-replica deployments do rolling restarts, so dropping
// mv_logs_filterdata in this release would make every filterdata request on
// not-yet-upgraded replicas return "relation does not exist" until they
// restart. The per-dimension views ship in this release and the legacy view
// is intentionally left in place. A follow-up release — after this one has
// fully rolled out everywhere — will wire this migration into RunMigrations
// (or add "mv_logs_filterdata" to legacyMatViewNames in matviews.go) to
// actually perform the drop.
func migrationSplitFilterDataMatView(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_split_filter_data_matview"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	// Materialized views are PostgreSQL-only; skip on other dialects.
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			if db.Dialector.Name() != "postgres" {
				return nil
			}
			tx = tx.WithContext(ctx)
			if err := tx.Exec("DROP MATERIALIZED VIEW IF EXISTS mv_logs_filterdata CASCADE").Error; err != nil {
				return fmt.Errorf("failed to drop legacy mv_logs_filterdata: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			// No rollback — ensureMatViews recreates the per-dim views on next startup.
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while splitting filter-data matview: %s", err.Error())
	}
	return nil
}

// migrationAddOCROutputColumn adds the ocr_output column to the Log table
func migrationAddOCROutputColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_ocr_output_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "ocr_output"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "ocr_output"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding ocr output column: %s", err.Error())
	}
	return nil
}

// migrationAddAttemptTrailColumn adds the attempt_trail column to the Log table.
// This column stores a JSON-serialized []schemas.KeyAttemptRecord capturing the per-attempt
// key selection history for requests that use key-based providers.
func migrationAddAttemptTrailColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_attempt_trail_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "attempt_trail"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "attempt_trail"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding attempt trail column: %s", err.Error())
	}
	return nil
}

// migrationAddOCRInputColumn adds the ocr_input column to the logs table.
func migrationAddOCRInputColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_ocr_input_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "ocr_input"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "ocr_input"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding ocr_input column: %s", err.Error())
	}
	return nil
}

// migrationAddStopReasonColumn adds the stop_reason column to the logs table.
// This column stores the reason why the model stopped generating (e.g., "stop", "length", "content_filter", "tool_calls").
func migrationAddStopReasonColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_stop_reason_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "stop_reason"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "stop_reason"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding stop_reason column: %s", err.Error())
	}
	return nil
}

// migrationAddToolCallNamesColumn adds the tool_call_names column to the logs table.
// It holds the comma-separated distinct function names a response called, kept
// on the row (not offloaded as payload) so the logs filter works in hybrid mode.
func migrationAddToolCallNamesColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_tool_call_names_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := boundDDLLockWait(tx); err != nil {
				return err
			}
			return addColumnIfNotExists(tx, logger, &Log{}, "tool_call_names")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := boundDDLLockWait(tx); err != nil {
				return err
			}
			return dropColumnIfExists(tx, logger, &Log{}, "tool_call_names")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding tool_call_names column: %s", err.Error())
	}
	return nil
}

// migrationAddComplexityRoutingColumns adds the complexity_tier, complexity_mechanism,
// and complexity_score columns to the logs table. They record how the governance
// complexity classifier routed each request; all three stay NULL for requests where
// no routing rule referenced complexity_tier.
func migrationAddComplexityRoutingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_complexity_routing_columns"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := boundDDLLockWait(tx); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "complexity_tier"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "complexity_mechanism"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "complexity_score"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := boundDDLLockWait(tx); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "complexity_score"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "complexity_mechanism"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "complexity_tier"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding complexity routing columns: %w", err)
	}
	return nil
}

// migrationAddModelRoutingColumns adds model_router_id and model_router_name
// to the logs table. They record the model router that took a request after
// the routing rules, and stay NULL for requests no router took.
func migrationAddModelRoutingColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_model_routing_columns"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{modelRoutingColumnsMigration(ctx, logger)})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding model routing columns: %w", err)
	}
	return nil
}

// modelRoutingColumns are the logs columns that record the model router a
// request was routed by.
var modelRoutingColumns = []string{"model_router_id", "model_router_name"}

func modelRoutingColumnsMigration(ctx context.Context, logger schemas.Logger) *migrator.Migration {
	const migrationName = "logs_add_model_routing_columns"
	return &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := boundDDLLockWait(tx); err != nil {
				return err
			}
			for _, column := range modelRoutingColumns {
				if err := addColumnIfNotExists(tx, logger, &Log{}, column); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			// The router a request was routed by cannot be recaptured, so a
			// rollback is refused while any log records one.
			if err := refuseRollbackWhileRecorded(tx, migrationName, []any{&Log{}}, modelRoutingColumns...); err != nil {
				return err
			}
			for _, column := range modelRoutingColumns {
				if err := dropColumnIfExists(tx, logger, &Log{}, column); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// migrationAddSessionIDColumn adds the generic session identity resolved at
// ingress so callers can correlate all requests in a Bifrost session.
func migrationAddSessionIDColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_session_id_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := boundDDLLockWait(tx); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "session_id"); err != nil {
				return err
			}
			if tx.Dialector.Name() != "postgres" && !tx.Migrator().HasIndex(&Log{}, "idx_logs_session_id") {
				if err := tx.Migrator().CreateIndex(&Log{}, "idx_logs_session_id"); err != nil {
					return fmt.Errorf("create session_id index: %w", err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := boundDDLLockWait(tx); err != nil {
				return err
			}
			if tx.Dialector.Name() != "postgres" && tx.Migrator().HasIndex(&Log{}, "idx_logs_session_id") {
				if err := tx.Migrator().DropIndex(&Log{}, "idx_logs_session_id"); err != nil {
					return err
				}
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "session_id"); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding session ID column: %w", err)
	}
	return nil
}

// migrationAddRedactionMappingColumn adds the redaction_mapping column to the
// logs table. It stores the reversible redaction mapping (encrypted when an
// encryption key is configured) so that reveal data shares the log row's
// lifecycle: deleting the log deletes the mapping.
func migrationAddRedactionMappingColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_redaction_mapping_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "redaction_mapping"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			// No-op rollback: dropping the column would permanently destroy
			// reveal data for already-redacted logs.
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding redaction_mapping column: %s", err.Error())
	}
	return nil
}

// migrationAddSafeJsonbFunction installs a PL/pgSQL helper that the
// /api/logs list query uses to extract the last element of input_history /
// responses_input_history without aborting the whole query on a single bad row.
//
// The previous inline guard (`left(btrim(x),1)='['`) only checked the first
// character before casting to jsonb. Any row that looked array-shaped but
// contained malformed JSON (unterminated structures, trailing commas, unpaired
// UTF-16 surrogates, etc.) would fail the cast with 22P02 / 22P05 and abort the
// entire list response. The helper wraps the cast in an EXCEPTION block and
// returns the raw TEXT on any parse failure.
//
// Postgres-only; SQLite is guarded inline in listSelectColumns via json_valid().
func migrationAddSafeJsonbFunction(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_safe_jsonb_function"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			if db.Dialector.Name() != "postgres" {
				return nil
			}
			tx = tx.WithContext(ctx)
			const stmt = `
CREATE OR REPLACE FUNCTION bifrost_safe_jsonb(t text) RETURNS text
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    j jsonb;
BEGIN
    IF t IS NULL OR t = '' OR t = '[]' THEN
        RETURN t;
    END IF;
    IF left(btrim(t), 1) <> '[' THEN
        RETURN t;
    END IF;
    BEGIN
        j := t::jsonb;
    EXCEPTION WHEN invalid_text_representation OR untranslatable_character THEN
        RETURN t;
    END;
    IF jsonb_typeof(j) <> 'array' OR jsonb_array_length(j) = 0 THEN
        RETURN t;
    END IF;
    RETURN jsonb_build_array(j->-1)::text;
END;
$$;`
			if err := tx.Exec(stmt).Error; err != nil {
				return fmt.Errorf("failed to create bifrost_safe_jsonb: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			return tx.Exec("DROP FUNCTION IF EXISTS bifrost_safe_jsonb(text)").Error
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding bifrost_safe_jsonb function: %s", err.Error())
	}
	return nil
}

// migrationAddClusterGovernanceColumns adds cluster_node_id, budget_ids, and rate_limit_ids
// columns to the logs table for node usage recovery in clustered deployments.
func migrationAddClusterGovernanceColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_cluster_governance_columns"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "cluster_node_id"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "budget_ids"); err != nil {
				return err
			}
			if err := addColumnIfNotExists(tx, logger, &Log{}, "rate_limit_ids"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "cluster_node_id"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "budget_ids"); err != nil {
				return err
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "rate_limit_ids"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding cluster governance columns: %s", err.Error())
	}
	return nil
}

// migrationAddLogIncNumberColumn adds a database-assigned monotonic cursor to
// logs. Existing rows remain NULL; new rows receive values from a PostgreSQL
// sequence at insert time. Ghost reconciliation uses this column after its
// initial timestamp query to avoid missing rows flushed late by the async log
// writer.
func migrationAddLogIncNumberColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_inc_number_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "IncNumber"); err != nil {
				return err
			}

			if tx.Dialector.Name() == "postgres" {
				if err := tx.Exec("CREATE SEQUENCE IF NOT EXISTS logs_inc_number_seq").Error; err != nil {
					return fmt.Errorf("failed to create logs_inc_number_seq: %w", err)
				}
				if err := tx.Exec("ALTER SEQUENCE logs_inc_number_seq OWNED BY logs.inc_number").Error; err != nil {
					return fmt.Errorf("failed to set logs_inc_number_seq ownership: %w", err)
				}
				if err := tx.Exec("ALTER TABLE logs ALTER COLUMN inc_number SET DEFAULT nextval('logs_inc_number_seq')").Error; err != nil {
					return fmt.Errorf("failed to set inc_number default: %w", err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if tx.Dialector.Name() == "postgres" {
				if err := tx.Exec("ALTER TABLE logs ALTER COLUMN inc_number DROP DEFAULT").Error; err != nil {
					return fmt.Errorf("failed to drop inc_number default: %w", err)
				}
				if err := tx.Exec("DROP SEQUENCE IF EXISTS logs_inc_number_seq").Error; err != nil {
					return fmt.Errorf("failed to drop logs_inc_number_seq: %w", err)
				}
			}
			if err := dropColumnIfExists(tx, logger, &Log{}, "IncNumber"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding log inc_number column: %s", err.Error())
	}
	return nil
}

// migrationRecreateFilterUsersMatView drops mv_filter_users so ensureMatViews
// recreates it with the corrected WHERE clause that excludes rows without a
// user_name, preventing duplicate entries where name falls back to user_id.
func migrationRecreateFilterUsersMatView(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_recreate_filter_users_matview"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			if db.Dialector.Name() != "postgres" {
				return nil
			}
			tx = tx.WithContext(ctx)
			if err := tx.Exec("DROP MATERIALIZED VIEW IF EXISTS mv_filter_users CASCADE").Error; err != nil {
				return fmt.Errorf("failed to drop mv_filter_users: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while recreating filter users matview: %s", err.Error())
	}
	return nil
}

// migrationRecreateFilterTeamBUMatViews drops mv_filter_teams and
// mv_filter_business_units so ensureMatViews recreates them with the multi-value
// body (scalar column UNION the JSON-array column). Required because
// repairMatViewShapes only detects drift by column presence, and the column
// shape (id, name, user_id, team_id, virtual_key_id) is unchanged — only the
// SELECT body changed — so the views would otherwise keep their old scalar-only
// definition. Recreated views keep identical columns, so old replicas reading
// them during a rolling deploy are unaffected (no legacyMatViewNames dance).
func migrationRecreateFilterTeamBUMatViews(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_recreate_filter_team_bu_matviews_multivalue"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			if db.Dialector.Name() != "postgres" {
				return nil
			}
			tx = tx.WithContext(ctx)
			for _, view := range []string{"mv_filter_teams", "mv_filter_business_units"} {
				if err := tx.Exec("DROP MATERIALIZED VIEW IF EXISTS " + view + " CASCADE").Error; err != nil {
					return fmt.Errorf("failed to drop %s: %w", view, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while recreating filter team/business-unit matviews: %s", err.Error())
	}
	return nil
}

// migrationAddCustomerArrayColumns adds the JSON-array columns capturing the full
// deduped set of customers a request belongs to (a team can belong to many
// customers via the enterprise team↔customer M2M). The scalar customer_id remains
// the primary; these power display, multi-customer filtering (jsonb @> + GIN), and
// fan-out aggregation, mirroring team_ids / business_unit_ids.
func migrationAddCustomerArrayColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_customer_array_columns"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true

	columns := []string{"customer_ids", "customer_names"}

	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, col := range columns {
				if err := addColumnIfNotExists(tx, logger, &Log{}, col); err != nil {
					return err
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, col := range columns {
				if err := dropColumnIfExists(tx, logger, &Log{}, col); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding customer array columns: %s", err.Error())
	}
	return nil
}

// migrationAddCustomerArrayGINIndexes registers the GIN index backing multi-customer
// log filtering. Like the team/BU GIN migration, the build itself is deferred to
// ensureMultiTeamBusinessUnitGINIndexes (post-startup, background, CONCURRENTLY);
// this migration exists only to provide a rollback that drops the index. Postgres-only.
func migrationAddCustomerArrayGINIndexes(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_customer_array_gin_indexes_v1"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = false
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if tx.Dialector.Name() == "postgres" {
				if err := tx.Exec("DROP INDEX IF EXISTS idx_logs_customer_ids_gin").Error; err != nil {
					return fmt.Errorf("failed to drop customer_ids GIN index: %w", err)
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while registering customer GIN indexes: %s", err.Error())
	}
	return nil
}

// migrationRecreateFilterCustomersMatView drops mv_filter_customers so ensureMatViews
// recreates it with the multi-value body (scalar customer_id UNION the JSON-array
// customer_ids), mirroring migrationRecreateFilterTeamBUMatViews. The column shape
// is unchanged so old replicas reading it during a rolling deploy are unaffected.
func migrationRecreateFilterCustomersMatView(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_recreate_filter_customers_matview_multivalue"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			if db.Dialector.Name() != "postgres" {
				return nil
			}
			tx = tx.WithContext(ctx)
			if err := tx.Exec("DROP MATERIALIZED VIEW IF EXISTS mv_filter_customers CASCADE").Error; err != nil {
				return fmt.Errorf("failed to drop mv_filter_customers: %w", err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while recreating filter customers matview: %s", err.Error())
	}
	return nil
}

// migrationAddServerSideFallbackModelColumn adds the server_side_fallback_model
// column to the logs table. Records the model that actually produced the response
// when the provider swapped models inside a single call (Anthropic server-side
// fallback) — which routing never sees, so the log's model column still names what
// the caller asked for.
func migrationAddServerSideFallbackModelColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_server_side_fallback_model_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			return addColumnIfNotExists(tx.WithContext(ctx), logger, &Log{}, "server_side_fallback_model")
		},
		Rollback: func(tx *gorm.DB) error {
			return dropColumnIfExists(tx.WithContext(ctx), logger, &Log{}, "server_side_fallback_model")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding server side fallback model column: %s", err.Error())
	}
	return nil
}

// migrationAddCorrelationColumns adds agent_correlation_id to logs.
// PostgreSQL gets the indexes from the background builder (performanceIndexes);
// other dialects create them here.
func migrationAddCorrelationColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	return migrator.New(db, &opts, []*migrator.Migration{correlationColumnsMigration(ctx, logger)}).Migrate()
}

// correlationColumnsMigration refuses its rollback while a correlation column holds a value: what a
// caller sent to tie its requests together cannot be recaptured once dropped.
func correlationColumnsMigration(ctx context.Context, logger schemas.Logger) *migrator.Migration {
	const migrationName = "logs_add_correlation_columns"
	return &migrator.Migration{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := boundDDLLockWait(tx); err != nil {
				return err
			}
			for _, column := range []struct {
				model any
				name  string
				index string
			}{
				{&Log{}, "agent_correlation_id", "idx_logs_agent_correlation_id"},
			} {
				if err := addColumnIfNotExists(tx, logger, column.model, column.name); err != nil {
					return err
				}
				if tx.Dialector.Name() != "postgres" && !tx.Migrator().HasIndex(column.model, column.index) {
					if err := tx.Migrator().CreateIndex(column.model, column.index); err != nil {
						return fmt.Errorf("create %s index: %w", column.index, err)
					}
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := refuseRollbackWhileRecorded(tx, migrationName, []any{&Log{}}, "agent_correlation_id"); err != nil {
				return err
			}
			for _, column := range []struct {
				model any
				name  string
			}{
				{&Log{}, "agent_correlation_id"},
			} {
				if err := dropColumnIfExists(tx, logger, column.model, column.name); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// migrationAddBillingFidelityColumns adds the served-tier columns cost recomputation
// needs in order to reprice a row at the rates it was actually served at:
// service_tier, speed, and inference_geo.
//
// These need real columns because no existing JSON column can carry them:
// BifrostLLMUsage tags Speed and InferenceGeo `json:"-"` so they never enter any
// serialized usage payload, and service_tier lives on the response envelope rather than
// on usage at all. Keeping them outside payloadFields also means they survive hybrid
// object-storage offload and content-hidden rows, both of which blank token_usage.
//
// There is deliberately no backfill and no denormalized cache-token column.
//
// The tier columns are not backfillable even in principle — the information was never
// captured, so pre-migration rows keep repricing at standard rates, which is the honest
// outcome. And the cache breakdown does not need a column at all: pricing reads it from
// token_usage (hydrated from object storage when offloaded), and a row whose payload
// cannot be recovered is flagged by IsUsageDegraded and skipped rather than priced from
// a lossy fallback. cached_read_tokens exists for an unrelated reason — the matviews and
// token histograms SUM it — and nothing aggregates cache writes, so a matching column
// would have had no consumer while requiring an effectively quadratic backfill to be
// useful on existing rows.
func migrationAddBillingFidelityColumns(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_billing_fidelity_columns"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	columns := []string{"service_tier", "speed", "inference_geo"}
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, column := range columns {
				if err := addColumnIfNotExists(tx, logger, &Log{}, column); err != nil {
					return fmt.Errorf("failed to add %s column: %w", column, err)
				}
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			for _, column := range columns {
				if err := dropColumnIfExists(tx, logger, &Log{}, column); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding billing fidelity columns: %s", err.Error())
	}
	return nil
}

// migrationAddVideoEditInputColumn adds the video_edit_input column to the logs table.
func migrationAddVideoEditInputColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_video_edit_input_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := addColumnIfNotExists(tx, logger, &Log{}, "video_edit_input"); err != nil {
				return err
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := dropColumnIfExists(tx, logger, &Log{}, "video_edit_input"); err != nil {
				return err
			}
			return nil
		},
	}})
	err := m.Migrate()
	if err != nil {
		return fmt.Errorf("error while adding video edit input column: %s", err.Error())
	}
	return nil
}

// migrationAddVideoDebugColumn adds the video_debug column to the logs table. It
// carries the video job id and lifecycle status on video rows, and on the aggregate
// cost row the pricing detail that tells it apart from the submission it settles —
// which is otherwise identical to it but for a cost. Nullable and unindexed, so this
// is a metadata-only DDL change with no table rewrite and no backfill: rows written
// before it simply have no video detail.
func migrationAddVideoDebugColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_video_debug_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			return addColumnIfNotExists(tx.WithContext(ctx), logger, &Log{}, "video_debug")
		},
		Rollback: func(tx *gorm.DB) error {
			return dropColumnIfExists(tx.WithContext(ctx), logger, &Log{}, "video_debug")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding video_debug column: %s", err.Error())
	}
	return nil
}

// migrationAddServedModelColumn adds the served_model column to the logs table.
// Records the model the provider named on the response body when it differs from
// the one the caller addressed.
func migrationAddServedModelColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_served_model_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			return addColumnIfNotExists(tx.WithContext(ctx), logger, &Log{}, "served_model")
		},
		Rollback: func(tx *gorm.DB) error {
			return dropColumnIfExists(tx.WithContext(ctx), logger, &Log{}, "served_model")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding served model column: %s", err.Error())
	}
	return nil
}

// smallTableForPlainIndex decides when a logstore migration builds an index
// without CONCURRENTLY; see postgresconn.TableIsSmall. A variable only so tests
// can force the concurrent path.
var smallTableForPlainIndex = func(tx *gorm.DB, table string) (bool, error) {
	return postgresconn.TableIsSmall(tx, table, postgresconn.DefaultSmallTableMaxBytes, postgresconn.DefaultSmallTableMaxRows)
}

// migrationAddEmbeddingInputColumn adds the embedding_input column to the logs table.
func migrationAddEmbeddingInputColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_embedding_input_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := boundDDLLockWait(tx); err != nil {
				return err
			}
			return addColumnIfNotExists(tx, logger, &Log{}, "embedding_input")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := boundDDLLockWait(tx); err != nil {
				return err
			}
			return dropColumnIfExists(tx, logger, &Log{}, "embedding_input")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding embedding_input column: %s", err.Error())
	}
	return nil
}

// migrationAddLiveSessionColumn adds the live_session column to the logs table: a GPT Live
// session's typed log payload (transcript, delegations, voice and backend cost).
func migrationAddLiveSessionColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_live_session_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := boundDDLLockWait(tx); err != nil {
				return err
			}
			return addColumnIfNotExists(tx, logger, &Log{}, "live_session")
		},
		Rollback: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := boundDDLLockWait(tx); err != nil {
				return err
			}
			return dropColumnIfExists(tx, logger, &Log{}, "live_session")
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding live session column: %s", err.Error())
	}
	return nil
}

// migrationAddMatViewRefreshStateTable creates matview_refresh_state, the
// cluster-wide record of when each materialized view last refreshed successfully
// (see matViewRefreshStateDDL). Materialized views exist only on Postgres, so on
// other dialects the migration records itself without creating anything. The
// table is new and touches no existing table, so it takes no lock on logs.
func migrationAddMatViewRefreshStateTable(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_matview_refresh_state_table"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			if tx.Dialector.Name() != "postgres" {
				return nil
			}
			return tx.WithContext(ctx).Exec(matViewRefreshStateDDL).Error
		},
		Rollback: func(tx *gorm.DB) error {
			if tx.Dialector.Name() != "postgres" {
				return nil
			}
			return tx.WithContext(ctx).Exec("DROP TABLE IF EXISTS matview_refresh_state").Error
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while creating matview_refresh_state table: %w", err)
	}
	return nil
}

// migrationAddIsPassthroughColumn adds the is_passthrough column to the logs table. A passthrough
// call is stored under the request type its route was inferred as (chat_completion, responses, ...)
// and tagged with this flag; rows written before it keep object_type=passthrough. A constant
// default, so the ALTER does not rewrite the table on Postgres.
func migrationAddIsPassthroughColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_is_passthrough_column"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{isPassthroughColumnMigration(ctx, logger)})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while adding is_passthrough column: %s", err.Error())
	}
	return nil
}

// isPassthroughColumnMigration bounds its lock wait like every logs DDL (see boundDDLLockWait). Its
// rollback deliberately keeps the column: is_passthrough is the only marker of a passthrough row
// stored under its inferred type, dropping it would make those rows indistinguishable from typed
// requests, and older code ignores an extra column with a default.
func isPassthroughColumnMigration(ctx context.Context, logger schemas.Logger) *migrator.Migration {
	return &migrator.Migration{
		ID: "logs_add_is_passthrough_column",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := boundDDLLockWait(tx); err != nil {
				return err
			}
			return addColumnIfNotExists(tx, logger, &Log{}, "is_passthrough")
		},
		Rollback: func(tx *gorm.DB) error {
			return nil
		},
	}
}

// clientIPColumnModels are the log tables that carry client_ip.
var clientIPColumnModels = []any{&Log{}}

// migrationAddClientIPColumn adds client_ip to logs.
func migrationAddClientIPColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	return migrator.New(db, &opts, []*migrator.Migration{clientIPColumnMigration(ctx, logger)}).Migrate()
}

func clientIPColumnMigration(ctx context.Context, logger schemas.Logger) *migrator.Migration {
	eachTable := func(tx *gorm.DB, apply func(*gorm.DB, schemas.Logger, interface{}, string) error) error {
		tx = tx.WithContext(ctx)
		if err := boundDDLLockWait(tx); err != nil {
			return err
		}
		for _, model := range clientIPColumnModels {
			if err := apply(tx, logger, model, "ClientIP"); err != nil {
				return err
			}
		}
		return nil
	}
	return &migrator.Migration{
		ID:      "logs_add_client_ip_column",
		Migrate: func(tx *gorm.DB) error { return eachTable(tx, addColumnIfNotExists) },
		Rollback: func(tx *gorm.DB) error {
			if err := refuseRollbackWhileRecorded(tx.WithContext(ctx), "logs_add_client_ip_column", clientIPColumnModels, "client_ip"); err != nil {
				return err
			}
			return eachTable(tx, dropColumnIfExists)
		},
	}
}

// refuseRollbackWhileRecorded returns an error when any of columns holds a value on any of models. Caller identity
// cannot be recaptured after the fact, so dropping a populated column deletes data rather than reversing a schema change.
//
// On PostgreSQL it first takes the ACCESS EXCLUSIVE lock DROP COLUMN needs anyway, on every table at once and bounded by
// boundDDLLockWait, so a writer cannot commit a value between the check and the drop. The lock lasts until the
// migration's transaction ends. Without an index on the column, proving it empty reads the whole table while every
// reader and writer waits, so a statement timeout bounds the check too: a scan that runs over it fails the rollback
// before any column is dropped.
func refuseRollbackWhileRecorded(tx *gorm.DB, migrationID string, models []any, columns ...string) error {
	migrator := tx.Migrator()
	if tx.Dialector.Name() == "postgres" {
		if err := boundDDLLockWait(tx); err != nil {
			return err
		}
		if err := tx.Exec("SET LOCAL statement_timeout = '30s'").Error; err != nil {
			return fmt.Errorf("could not bound the recorded-value check for %s: %w", migrationID, err)
		}
		tables := make([]string, 0, len(models))
		for _, model := range models {
			if !migrator.HasTable(model) {
				continue
			}
			stmt := &gorm.Statement{DB: tx}
			if err := stmt.Parse(model); err != nil {
				return fmt.Errorf("could not resolve the table for %T before rolling back %s: %w", model, migrationID, err)
			}
			tables = append(tables, stmt.Quote(stmt.Schema.Table))
		}
		if len(tables) > 0 {
			if err := tx.Exec("LOCK TABLE " + strings.Join(tables, ", ") + " IN ACCESS EXCLUSIVE MODE").Error; err != nil {
				return fmt.Errorf("could not lock the log tables before rolling back %s: %w", migrationID, err)
			}
		}
	}
	for _, model := range models {
		if !migrator.HasTable(model) {
			continue
		}
		for _, column := range columns {
			if !migrator.HasColumn(model, column) {
				continue
			}
			var present []struct{ One int }
			if err := tx.Model(model).Select("1 AS one").Where(column + " IS NOT NULL").Limit(1).Find(&present).Error; err != nil {
				return fmt.Errorf("could not check %T.%s before rolling back %s: %w", model, column, migrationID, err)
			}
			if len(present) > 0 {
				return fmt.Errorf("%s is non-rollbackable: %T.%s holds recorded values that cannot be recaptured, and dropping it would delete them; clear the column first if the rollback is genuinely intended", migrationID, model, column)
			}
		}
	}
	return nil
}

// userEmailColumnModels are the log tables that carry user_email.
var userEmailColumnModels = []any{&Log{}}

// migrationAddUserEmailColumn adds user_email to logs.
func migrationAddUserEmailColumn(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	return migrator.New(db, &opts, []*migrator.Migration{userEmailColumnMigration(ctx, logger)}).Migrate()
}

func userEmailColumnMigration(ctx context.Context, logger schemas.Logger) *migrator.Migration {
	eachTable := func(tx *gorm.DB, apply func(*gorm.DB, schemas.Logger, interface{}, string) error) error {
		tx = tx.WithContext(ctx)
		if err := boundDDLLockWait(tx); err != nil {
			return err
		}
		for _, model := range userEmailColumnModels {
			if err := apply(tx, logger, model, "UserEmail"); err != nil {
				return err
			}
		}
		return nil
	}
	return &migrator.Migration{
		ID:      "logs_add_user_email_column",
		Migrate: func(tx *gorm.DB) error { return eachTable(tx, addColumnIfNotExists) },
		Rollback: func(tx *gorm.DB) error {
			if err := refuseRollbackWhileRecorded(tx.WithContext(ctx), "logs_add_user_email_column", userEmailColumnModels, "user_email"); err != nil {
				return err
			}
			return eachTable(tx, dropColumnIfExists)
		},
	}
}

// migrationAddComplexitySuggestionTables creates the storage for the
// complexity router's phrase suggestions: the suggestions, the runs that wrote
// them, and the retry list of captured request vectors whose delete failed.
// See complexitysuggestions.go.
func migrationAddComplexitySuggestionTables(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "logs_add_model_routing_suggestion_tables"
	logger.Info("[logstore] starting migration %s", migrationName)
	defer logger.Info("[logstore] finished migration %s", migrationName)
	// Transactional so a boot that dies part-way leaves none of the three. No
	// boundDDLLockWait: this only creates tables that do not exist yet.
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	m := migrator.New(db, &opts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			return tx.WithContext(ctx).AutoMigrate(&ComplexitySuggestion{}, &ComplexitySuggestionRun{}, &ComplexityVectorDelete{})
		},
		Rollback: func(tx *gorm.DB) error {
			return rollbackComplexitySuggestionTables(tx.WithContext(ctx))
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error running %s migration: %s", migrationName, err.Error())
	}
	return nil
}

// rollbackComplexitySuggestionTables drops the suggestion tables, but only
// while model_routing_suggestions and model_routing_vector_deletes are empty. A
// suggestion row carries the user's accept and dismiss decisions, which no
// rerun can rebuild; a vector delete row is a deleted log whose vector is
// still in the vector store, and dropping it would leave the vector there for
// good. The runs table is history only and is not checked. On PostgreSQL both
// are locked first, so nothing can write between the check and the DROP.
func rollbackComplexitySuggestionTables(tx *gorm.DB) error {
	guarded := []struct {
		model  any
		table  string
		reason string
	}{
		{&ComplexitySuggestion{}, "model_routing_suggestions", "model_routing_suggestions holds accepted and dismissed suggestions that cannot be rebuilt from the logs"},
		{&ComplexityVectorDelete{}, "model_routing_vector_deletes", "model_routing_vector_deletes lists deleted logs whose vectors are still in the vector store"},
	}
	for _, g := range guarded {
		if !tx.Migrator().HasTable(g.model) {
			continue
		}
		if tx.Dialector.Name() == "postgres" {
			if err := boundDDLLockWait(tx); err != nil {
				return err
			}
			if err := tx.Exec("LOCK TABLE " + g.table + " IN ACCESS EXCLUSIVE MODE").Error; err != nil {
				return fmt.Errorf("could not lock %s before rollback: %w", g.table, err)
			}
		}
		var present []struct{ One int }
		if err := tx.Model(g.model).Select("1 AS one").Limit(1).Find(&present).Error; err != nil {
			return fmt.Errorf("could not check %s before rollback: %w", g.table, err)
		}
		if len(present) > 0 {
			return fmt.Errorf("logs_add_model_routing_suggestion_tables is non-rollbackable: %s; clear it first if the rollback is genuinely intended", g.reason)
		}
	}
	return tx.Migrator().DropTable(&ComplexityVectorDelete{}, &ComplexitySuggestionRun{}, &ComplexitySuggestion{})
}

// retentionAutovacuumTables are the tables the retention cleaner deletes from on
// a schedule (the keys of retentionAdvisoryLockKeys), in a fixed order.
var retentionAutovacuumTables = []string{"logs"}

// retentionAutovacuumOptions are the storage parameters set on each retention
// table, written as Postgres reports them in pg_class.reloptions.
var retentionAutovacuumOptions = []string{"autovacuum_vacuum_scale_factor=0.01", "autovacuum_vacuum_threshold=10000"}

// retentionAutovacuumOptionNames returns the parameter names of
// retentionAutovacuumOptions, so rollback resets exactly what migrate set.
func retentionAutovacuumOptionNames() []string {
	names := make([]string, 0, len(retentionAutovacuumOptions))
	for _, opt := range retentionAutovacuumOptions {
		name, _, _ := strings.Cut(opt, "=")
		names = append(names, name)
	}
	return names
}

// migrationTuneRetentionAutovacuum makes autovacuum keep up with retention on
// Postgres. Retention deletes expired rows every hour, and a DELETE only leaves
// dead tuples; autovacuum turns them into reusable space, but by default it
// waits for 20% of a table to be dead. On a 50M-row logs table that is 10M dead
// rows, so the table and its indexes keep growing between vacuums even though
// the live row count is steady. Vacuuming after 1% (plus 10000 rows, so small
// tables are not vacuumed constantly) bounds that bloat. This does not hand
// space back to the filesystem; only VACUUM FULL or pg_repack do, and VACUUM
// FULL locks the table for its whole rewrite, so it is never run automatically.
func migrationTuneRetentionAutovacuum(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	if db.Dialector.Name() != "postgres" {
		return nil
	}
	opts := *migrator.DefaultOptions
	opts.UseTransaction = true
	return migrator.New(db, &opts, []*migrator.Migration{retentionAutovacuumMigration(ctx, logger)}).Migrate()
}

// retentionAutovacuumMigration sets or resets the autovacuum parameters on every
// retention table that exists. ALTER TABLE ... SET/RESET takes SHARE UPDATE
// EXCLUSIVE, which does not block reads or writes; boundDDLLockWait caps the wait
// behind a running vacuum so the migration retries instead of queueing.
func retentionAutovacuumMigration(ctx context.Context, logger schemas.Logger) *migrator.Migration {
	eachTable := func(tx *gorm.DB, stmt func(table string) string) error {
		tx = tx.WithContext(ctx)
		if tx.Dialector.Name() != "postgres" {
			return nil
		}
		if err := boundDDLLockWait(tx); err != nil {
			return err
		}
		for _, table := range retentionAutovacuumTables {
			var exists bool
			if err := tx.Raw("SELECT to_regclass(?) IS NOT NULL", table).Scan(&exists).Error; err != nil {
				return err
			}
			if !exists {
				logger.Debug("[logstore] %s does not exist; skipping autovacuum settings", table)
				continue
			}
			if err := tx.Exec(stmt(table)).Error; err != nil {
				return fmt.Errorf("could not set autovacuum parameters on %s: %w", table, err)
			}
		}
		return nil
	}
	return &migrator.Migration{
		ID: "logs_tune_retention_autovacuum",
		Migrate: func(tx *gorm.DB) error {
			return eachTable(tx, func(table string) string {
				return "ALTER TABLE " + table + " SET (" + strings.Join(retentionAutovacuumOptions, ", ") + ")"
			})
		},
		Rollback: func(tx *gorm.DB) error {
			return eachTable(tx, func(table string) string {
				return "ALTER TABLE " + table + " RESET (" + strings.Join(retentionAutovacuumOptionNames(), ", ") + ")"
			})
		},
	}
}

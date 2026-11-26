package configstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	bifrost "github.com/maximhq/bifrost/core"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/encrypt"
	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/maximhq/bifrost/framework/queryscope"
	"github.com/maximhq/bifrost/framework/vectorstore"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RDBConfigStore represents a configuration store that uses a relational database.
//
// The runtime *gorm.DB is held behind an atomic.Pointer so RefreshConnectionPool
// can swap it out without tearing callers down. migrateOnFreshFn and refreshPoolFn
// are backend-specific hooks installed by the constructor (postgres vs sqlite).
type RDBConfigStore struct {
	db               atomic.Pointer[gorm.DB]
	logger           schemas.Logger
	migrateOnFreshFn func(ctx context.Context, fn func(context.Context, *gorm.DB) error) error
	refreshPoolFn    func(ctx context.Context) error
}

// getWeight safely dereferences a *float64 weight pointer, returning 1.0 as default if nil.
// This allows distinguishing between "not set" (nil -> 1.0) and "explicitly set to 0" (0.0).
func getWeight(w *float64) float64 {
	if w == nil {
		return 1.0
	}
	return *w
}

// sortedProviderNames returns provider names in deterministic order for write paths.
func sortedProviderNames(providers map[schemas.ModelProvider]ProviderConfig) []schemas.ModelProvider {
	names := make([]schemas.ModelProvider, 0, len(providers))
	for provider := range providers {
		names = append(names, provider)
	}
	sort.Slice(names, func(i, j int) bool {
		return string(names[i]) < string(names[j])
	})
	return names
}

// sortedUintCopy returns a sorted copy of ids without mutating the caller's slice.
func sortedUintCopy(ids []uint) []uint {
	sorted := append([]uint(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted
}

// sortTableKeysByID sorts table keys by stable database identity for deterministic writes.
func sortTableKeysByID(keys []tables.TableKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ID == keys[j].ID {
			return keys[i].KeyID < keys[j].KeyID
		}
		return keys[i].ID < keys[j].ID
	})
}

// dbForUpdate adds a PostgreSQL row-level update lock to the query.
func dbForUpdate(db *gorm.DB) *gorm.DB {
	if db.Dialector.Name() != "postgres" {
		return db
	}
	return db.Clauses(clause.Locking{Strength: "UPDATE"})
}

// clientConfigAdvisoryLockKey serializes config_client writers across replicas; 1000001 is the configstore migration lock.
const clientConfigAdvisoryLockKey = 1000002

// lockClientConfigRow serializes config_client writers for the rest of the transaction so a
// concurrent DELETE+CREATE cannot hide metadata_json from the carry-forward read. Postgres takes a
// transaction-scoped advisory lock. SQLite has no row locks: its transactions start as readers, and
// a reader that later writes after another connection committed is refused at once with "database
// is locked" (the busy timeout is not applied to that upgrade). A no-op write as the first statement
// takes the write lock up front, where the busy timeout does apply.
func lockClientConfigRow(tx *gorm.DB) error {
	switch tx.Dialector.Name() {
	case "postgres":
		return tx.Exec("SELECT pg_advisory_xact_lock(?)", clientConfigAdvisoryLockKey).Error
	case "sqlite":
		return tx.Exec("UPDATE config_client SET id = id WHERE 1 = 0").Error
	default:
		return nil
	}
}

// budgetNamesNoOwner reports whether a budget write names none of the owner columns.
func budgetNamesNoOwner(budget *tables.TableBudget) bool {
	return budget.TeamID == nil && budget.VirtualKeyID == nil && budget.ProviderConfigID == nil &&
		budget.ModelConfigID == nil && budget.CustomerID == nil && budget.BusinessUnitID == nil
}

// lockBudgetOwner locks the owning governance parent before mutating a budget row.
func lockBudgetOwner(ctx context.Context, txDB *gorm.DB, budget tables.TableBudget) error {
	switch {
	case budget.VirtualKeyID != nil && *budget.VirtualKeyID != "":
		var vk tables.TableVirtualKey
		if err := dbForUpdate(txDB.WithContext(ctx)).First(&vk, "id = ?", *budget.VirtualKeyID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
	case budget.ProviderConfigID != nil:
		var providerConfig tables.TableVirtualKeyProviderConfig
		if err := dbForUpdate(txDB.WithContext(ctx)).First(&providerConfig, "id = ?", *budget.ProviderConfigID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
	case budget.TeamID != nil && *budget.TeamID != "":
		var team tables.TableTeam
		if err := dbForUpdate(txDB.WithContext(ctx)).First(&team, "id = ?", *budget.TeamID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
	case budget.CustomerID != nil && *budget.CustomerID != "":
		var customer tables.TableCustomer
		if err := dbForUpdate(txDB.WithContext(ctx)).First(&customer, "id = ?", *budget.CustomerID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
	}
	return nil
}

// schemaKeyFromTableKey converts a database key to a schema key.
func schemaKeyFromTableKey(dbKey tables.TableKey) schemas.Key {
	return schemas.Key{
		ID:                     dbKey.KeyID,
		Name:                   dbKey.Name,
		Value:                  dbKey.Value,
		Models:                 dbKey.Models,
		BlacklistedModels:      dbKey.BlacklistedModels,
		Weight:                 getWeight(dbKey.Weight),
		Enabled:                dbKey.Enabled,
		UseForBatchAPI:         dbKey.UseForBatchAPI,
		UseAnthropicEndpoints:  dbKey.UseAnthropicEndpoints,
		UseOpenAIEndpoints:     dbKey.UseOpenAIEndpoints,
		AzureKeyConfig:         dbKey.AzureKeyConfig,
		VertexKeyConfig:        dbKey.VertexKeyConfig,
		BedrockKeyConfig:       dbKey.BedrockKeyConfig,
		BedrockMantleKeyConfig: dbKey.BedrockMantleKeyConfig,
		SageMakerKeyConfig:     dbKey.SageMakerKeyConfig,
		Aliases:                dbKey.Aliases,
		VLLMKeyConfig:          dbKey.VLLMKeyConfig,
		ReplicateKeyConfig:     dbKey.ReplicateKeyConfig,
		OllamaKeyConfig:        dbKey.OllamaKeyConfig,
		SGLKeyConfig:           dbKey.SGLKeyConfig,
		DatabricksKeyConfig:    dbKey.DatabricksKeyConfig,
		CloudflareKeyConfig:    dbKey.CloudflareKeyConfig,
		AnthropicKeyConfig:     dbKey.AnthropicKeyConfig,
		OAuthKeyConfig:         dbKey.OAuthKeyConfig,
		ConfigHash:             dbKey.ConfigHash,
		Status:                 schemas.KeyStatusType(dbKey.Status),
		Description:            dbKey.Description,
	}
}

// tableKeyFromSchemaKey converts a schema key to a database key.
func tableKeyFromSchemaKey(provider tables.TableProvider, key schemas.Key) (tables.TableKey, error) {
	dbKey := tables.TableKey{
		Provider:               provider.Name,
		ProviderID:             provider.ID,
		KeyID:                  key.ID,
		Name:                   key.Name,
		Value:                  key.Value,
		Models:                 key.Models,
		BlacklistedModels:      key.BlacklistedModels,
		Weight:                 &key.Weight,
		Enabled:                key.Enabled,
		UseForBatchAPI:         key.UseForBatchAPI,
		UseAnthropicEndpoints:  key.UseAnthropicEndpoints,
		UseOpenAIEndpoints:     key.UseOpenAIEndpoints,
		AzureKeyConfig:         key.AzureKeyConfig,
		VertexKeyConfig:        key.VertexKeyConfig,
		BedrockKeyConfig:       key.BedrockKeyConfig,
		BedrockMantleKeyConfig: key.BedrockMantleKeyConfig,
		SageMakerKeyConfig:     key.SageMakerKeyConfig,
		Aliases:                key.Aliases,
		VLLMKeyConfig:          key.VLLMKeyConfig,
		ReplicateKeyConfig:     key.ReplicateKeyConfig,
		OllamaKeyConfig:        key.OllamaKeyConfig,
		SGLKeyConfig:           key.SGLKeyConfig,
		DatabricksKeyConfig:    key.DatabricksKeyConfig,
		CloudflareKeyConfig:    key.CloudflareKeyConfig,
		AnthropicKeyConfig:     key.AnthropicKeyConfig,
		OAuthKeyConfig:         key.OAuthKeyConfig,
		ConfigHash:             key.ConfigHash,
		Status:                 string(key.Status),
		Description:            key.Description,
	}

	if key.AzureKeyConfig != nil {
		dbKey.AzureEndpoint = &key.AzureKeyConfig.Endpoint
		dbKey.AzureUseSpeechEndpoints = &key.AzureKeyConfig.UseSpeechEndpoints
		dbKey.AzureUseVoiceLiveEndpoints = &key.AzureKeyConfig.UseVoiceLiveEndpoints
	}

	if key.VertexKeyConfig != nil {
		dbKey.VertexProjectID = &key.VertexKeyConfig.ProjectID
		dbKey.VertexProjectNumber = &key.VertexKeyConfig.ProjectNumber
		dbKey.VertexRegion = &key.VertexKeyConfig.Region
		dbKey.VertexAuthCredentials = &key.VertexKeyConfig.AuthCredentials
		dbKey.VertexForceSingleRegion = &key.VertexKeyConfig.ForceSingleRegion
		dbKey.VertexUseSessionRouting = &key.VertexKeyConfig.UseSessionRouting
		wif, err := tables.MarshalVertexAWSWorkloadIdentityJSON(key.VertexKeyConfig.AWSWorkloadIdentity)
		if err != nil {
			return tables.TableKey{}, err
		}
		dbKey.VertexAWSWorkloadIdentityJSON = wif
	}

	if key.BedrockKeyConfig != nil {
		dbKey.BedrockAccessKey = &key.BedrockKeyConfig.AccessKey
		dbKey.BedrockSecretKey = &key.BedrockKeyConfig.SecretKey
		dbKey.BedrockSessionToken = key.BedrockKeyConfig.SessionToken
		dbKey.BedrockRegion = key.BedrockKeyConfig.Region
		dbKey.BedrockARN = key.BedrockKeyConfig.ARN
		dbKey.BedrockRoleARN = key.BedrockKeyConfig.RoleARN
		dbKey.BedrockExternalID = key.BedrockKeyConfig.ExternalID
		dbKey.BedrockRoleSessionName = key.BedrockKeyConfig.RoleSessionName
		dbKey.BedrockBatchRoleARN = key.BedrockKeyConfig.BatchRoleARN
		if key.BedrockKeyConfig.BatchS3Config != nil {
			data, err := sonic.Marshal(key.BedrockKeyConfig.BatchS3Config)
			if err != nil {
				return tables.TableKey{}, err
			}
			s := string(data)
			dbKey.BedrockBatchS3ConfigJSON = &s
		}
		if key.BedrockKeyConfig.Endpoints != nil {
			data, err := sonic.Marshal(key.BedrockKeyConfig.Endpoints)
			if err != nil {
				return tables.TableKey{}, err
			}
			s := string(data)
			dbKey.BedrockEndpointsJSON = &s
		}
	}

	return dbKey, nil
}

// externalURLToString converts an *schemas.SecretVar to its storage string form.
// Stores "env.MY_VAR" when sourced from an env var, or the raw URL otherwise.
func externalURLToString(e *schemas.SecretVar) string {
	if e == nil {
		return ""
	}
	if v, err := e.Value(); err == nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// UpdateClientConfig updates the client configuration in the database.
func (s *RDBConfigStore) UpdateClientConfig(ctx context.Context, config *ClientConfig) error {
	dbConfig := tables.TableClientConfig{
		DropExcessRequests:                        config.DropExcessRequests,
		InitialPoolSize:                           config.InitialPoolSize,
		EnableLogging:                             config.EnableLogging,
		DisableContentLogging:                     config.DisableContentLogging,
		RetainContentInObjectStorage:              config.RetainContentInObjectStorage,
		DisableDBPingsInHealth:                    config.DisableDBPingsInHealth,
		DumpErrorsInConsoleLogs:                   config.DumpErrorsInConsoleLogs,
		LogRetentionDays:                          config.LogRetentionDays,
		EnforceAuthOnInference:                    config.EnforceAuthOnInference,
		DualCredentialConflictBehavior:            config.DualCredentialConflictBehavior,
		ExtraFieldsPlacement:                      config.ExtraFieldsPlacement,
		EnforceGovernanceHeader:                   config.EnforceGovernanceHeader,
		EnforceSCIMAuth:                           config.EnforceSCIMAuth,
		PrometheusLabels:                          config.PrometheusLabels,
		AllowedOrigins:                            config.AllowedOrigins,
		AllowedHeaders:                            config.AllowedHeaders,
		MaxRequestBodySizeMB:                      config.MaxRequestBodySizeMB,
		CompatConvertTextToChat:                   config.Compat.ConvertTextToChat,
		CompatConvertChatToResponses:              config.Compat.ConvertChatToResponses,
		CompatShouldDropParams:                    config.Compat.ShouldDropParams,
		CompatShouldConvertParams:                 config.Compat.ShouldConvertParams,
		CompatAzureDeepseek:                       config.Compat.AzureDeepseek,
		CompatForceReasoningOnlyModelsToResponses: config.Compat.ForceReasoningOnlyModelsToResponses,
		CompatConvertDecisionToResponses:          config.Compat.ConvertDecisionToResponses,
		AsyncJobResultTTL:                         config.AsyncJobResultTTL,
		RequiredHeaders:                           config.RequiredHeaders,
		LoggingHeaders:                            config.LoggingHeaders,
		WhitelistedRoutes:                         config.WhitelistedRoutes,
		HideDeletedVirtualKeysInFilters:           config.HideDeletedVirtualKeysInFilters,
		DeleteExpiredVirtualKeys:                  config.DeleteExpiredVirtualKeys,
		EnforceVKObjectOwnership:                  config.EnforceVKObjectOwnership,
		SkipAccessProfileUserVirtualKeyCreation:   config.SkipAccessProfileUserVirtualKeyCreation,
		SkipAccessProfileUserVirtualKeyDeletion:   config.SkipAccessProfileUserVirtualKeyDeletion,
		EntityAccessProfileMode:                   config.EntityAccessProfileMode,
		HiddenRequestTypes:                        config.HiddenRequestTypes,
		UsageEstimationTokenizers:                 config.UsageEstimationTokenizers,
		RoutingChainMaxDepth:                      config.RoutingChainMaxDepth,
		SkillsExternalClientURL:                   externalURLToString(config.SkillsExternalClientURL),
		HeaderFilterConfig:                        config.HeaderFilterConfig,
		AllowPerRequestContentStorageOverride:     config.AllowPerRequestContentStorageOverride,
		AllowPerRequestRawOverride:                config.AllowPerRequestRawOverride,
		AllowDirectKeys:                           config.AllowDirectKeys,
		VKRotationCooldownNS:                      int64(config.VKRotationCooldown),
		ConfigHash:                                config.ConfigHash,
	}
	// Delete existing client config and create new one in a transaction.
	// MetadataJSON is preserved here because Metadata is a UI/admin-preferences
	// blob that is NOT part of the API-facing ClientConfig (so config.json sync
	// can never set it). Reading it inside the transaction before DELETE keeps
	// callers from clobbering UI prefs on every config write.
	return s.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockClientConfigRow(tx); err != nil {
			return err
		}
		var existing tables.TableClientConfig
		if err := dbForUpdate(tx.Select("metadata_json")).First(&existing).Error; err == nil {
			dbConfig.MetadataJSON = existing.MetadataJSON
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&tables.TableClientConfig{}).Error; err != nil {
			return err
		}
		if err := tx.Create(&dbConfig).Error; err != nil {
			return err
		}
		// enforce_vk_object_ownership defaults to true: Create would drop an explicit false
		// and the database would store true, so force it through explicitly.
		if !config.EnforceVKObjectOwnership {
			if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Model(&tables.TableClientConfig{}).Update("enforce_vk_object_ownership", false).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// Ping checks if the database is reachable.
func (s *RDBConfigStore) Ping(ctx context.Context) error {
	return s.DB().WithContext(ctx).Exec("SELECT 1").Error
}

// DB returns the current runtime database connection. The returned pointer is
// only valid for the duration of the caller's operation — after a
// RefreshConnectionPool call, future DB() calls return a fresh *gorm.DB backed
// by a different *sql.DB pool. Callers that issue multiple operations should
// call DB() per operation rather than caching the pointer.
func (s *RDBConfigStore) DB() *gorm.DB {
	return s.db.Load()
}

// ScopedDB returns the DB bound to ctx with any QueryScope on ctx
// pre-applied, or the caller's transaction when one is passed, so a read
// taken inside a transaction sees that transaction's own writes instead of
// the state it started from. The scope is applied either way: reading
// through a transaction does not widen what the caller may see.
//
// Use this in read paths that should respect caller-driven row visibility.
// Use DB().WithContext(ctx) for writes and for internal lookups (e.g.
// inference VK auth) that must bypass scoping.
func (s *RDBConfigStore) ScopedDB(ctx context.Context, tx ...*gorm.DB) *gorm.DB {
	db := s.DB()
	if len(tx) > 0 && tx[0] != nil {
		db = tx[0]
	}
	scoped := db.WithContext(ctx)
	if scope := queryscope.FromContext(ctx); scope != nil {
		scoped = scope(scoped)
	}
	return scoped
}

// RunMigration opens a throwaway connection against the same
// backing database, invokes fn with it, and closes the connection. Use this
// for DDL that must not leave cached prepared-statement plans on the runtime
// pool. After fn returns, callers should invoke RefreshConnectionPool if the
// migration altered tables the runtime pool has already queried.
//
// For SQLite, the throwaway concept doesn't apply (no server-side plan cache,
// single-writer file lock), so this runs fn against the existing *gorm.DB.
//
// Returns an error if the store was constructed without a migration hook
// wired — e.g. a direct `&RDBConfigStore{}` literal that skipped the
// newPostgresConfigStore / newSqliteConfigStore constructor. An explicit
// error is safer than a silent fallback to the runtime pool: running DDL
// on the runtime pool would reintroduce SQLSTATE 0A000.
func (s *RDBConfigStore) RunMigration(ctx context.Context, fn func(context.Context, *gorm.DB) error) error {
	if s.migrateOnFreshFn == nil {
		return fmt.Errorf("configstore: migration hook is not configured; construct the store via newPostgresConfigStore or newSqliteConfigStore")
	}
	return s.migrateOnFreshFn(ctx, fn)
}

// RefreshConnectionPool closes the runtime pool and opens a fresh one against
// the same configuration. In-flight queries on the old pool complete before
// it closes; subsequent DB() calls return the new pool, whose connections
// carry no cached plans. SQLite is a no-op.
//
// Returns an error if the store was constructed without a refresh hook wired
// (same rationale as RunMigration).
func (s *RDBConfigStore) RefreshConnectionPool(ctx context.Context) error {
	if s.refreshPoolFn == nil {
		return fmt.Errorf("configstore: refresh hook is not configured; construct the store via newPostgresConfigStore or newSqliteConfigStore")
	}
	return s.refreshPoolFn(ctx)
}

// parseGormError parses GORM errors to provide user-friendly error messages.
// Currently handles unique constraint violations and is designed to be extended
// for other error types in the future (e.g., foreign key violations, not null constraints).
func (s *RDBConfigStore) parseGormError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	errMsg := err.Error()
	// Check for unique constraint violations
	// SQLite format: "UNIQUE constraint failed: table_name.column_name"
	// PostgreSQL format: "ERROR: duplicate key value violates unique constraint"

	if strings.Contains(errMsg, "UNIQUE constraint failed") ||
		strings.Contains(errMsg, "duplicate key value violates unique constraint") {

		// Extract column name from error message
		var columnName string

		// SQLite: extract from "UNIQUE constraint failed: table.column"
		if strings.Contains(errMsg, "UNIQUE constraint failed") {
			parts := strings.Split(errMsg, "UNIQUE constraint failed:")
			if len(parts) > 1 {
				tableColumn := strings.TrimSpace(parts[1])
				// Extract column name after the last dot
				if dotIndex := strings.LastIndex(tableColumn, "."); dotIndex != -1 {
					columnName = tableColumn[dotIndex+1:]
				} else {
					columnName = tableColumn
				}
			}
		} else if strings.Contains(errMsg, "duplicate key value violates unique constraint") {
			// PostgreSQL: try to extract from constraint name or detail
			// Example: duplicate key value violates unique constraint "idx_key_name"
			// Detail: Key (name)=(value) already exists.

			// First try to extract from Detail
			if strings.Contains(errMsg, "Key (") {
				startIdx := strings.Index(errMsg, "Key (")
				if startIdx != -1 {
					rest := errMsg[startIdx+5:]
					endIdx := strings.Index(rest, ")")
					if endIdx != -1 {
						columnName = rest[:endIdx]
					}
				}
			}
			// If not found, try to parse from constraint name
			if columnName == "" {
				// Extract constraint name
				if strings.Contains(errMsg, `"`) {
					parts := strings.Split(errMsg, `"`)
					if len(parts) >= 2 {
						constraintName := parts[1]
						// Remove idx_ prefix and try to extract column name
						if strings.HasPrefix(constraintName, "idx_") {
							constraintName = constraintName[4:]
							// Find the last underscore to get column name
							if lastUnderscore := strings.LastIndex(constraintName, "_"); lastUnderscore != -1 {
								columnName = constraintName[lastUnderscore+1:]
							} else {
								columnName = constraintName
							}
						}
					}
				}
			}
		}
		// Clean up column name (remove underscores, convert to readable format)
		if columnName != "" {
			// Convert snake_case to space-separated words
			columnName = strings.ReplaceAll(columnName, "_", " ")
			// For config_keys.name uniqueness violations, give a more specific error message.
			// Scope to config_keys specifically (SQLite: "config_keys.name",
			// PostgreSQL: constraint "idx_key_name") to avoid matching other tables like
			// governance_teams.name or config_plugins.name.
			if strings.Contains(errMsg, "config_keys.name") || strings.Contains(errMsg, "idx_key_name") {
				return fmt.Errorf("API key names must be unique across providers. A key with this name %w. Rename it in the UI or config.json", ErrAlreadyExists)
			}
			return fmt.Errorf("a record with this %s %w. Please use a different value", columnName, ErrAlreadyExists)
		}
		// Fallback message if we couldn't parse the column name
		return fmt.Errorf("a record with this value %w. Please use a different value", ErrAlreadyExists)
	}

	// For other errors, return the original error
	// Future: add handling for foreign key violations, not null constraints, etc.
	return err
}

// UpdateFrameworkConfig updates the framework configuration in the database.
func (s *RDBConfigStore) UpdateFrameworkConfig(ctx context.Context, config *tables.TableFrameworkConfig) error {
	// Update the framework configuration
	return s.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&tables.TableFrameworkConfig{}).Error; err != nil {
			return err
		}
		return tx.Create(config).Error
	})
}

// GetFrameworkConfig retrieves the framework configuration from the database.
func (s *RDBConfigStore) GetFrameworkConfig(ctx context.Context) (*tables.TableFrameworkConfig, error) {
	var dbConfig tables.TableFrameworkConfig
	if err := s.DB().WithContext(ctx).First(&dbConfig).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &dbConfig, nil
}

// ListFeatureFlags returns every persisted feature-flag override. Flags at
// their code default are absent from this table by design.
func (s *RDBConfigStore) ListFeatureFlags(ctx context.Context) ([]tables.TableFeatureFlag, error) {
	var rows []tables.TableFeatureFlag
	if err := s.DB().WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// UpsertFeatureFlag writes or replaces a single override row. ID is the
// primary key so concurrent writers cannot create duplicates. updatedAt is
// the caller-supplied logical timestamp used by gossip for last-write-wins.
func (s *RDBConfigStore) UpsertFeatureFlag(ctx context.Context, id string, enabled bool, updatedAt int64) error {
	row := tables.TableFeatureFlag{ID: id, Enabled: enabled, UpdatedAt: updatedAt}
	return s.DB().WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"enabled", "updated_at"}),
	}).Create(&row).Error
}

// GetClientConfig retrieves the client configuration from the database.
func (s *RDBConfigStore) GetClientConfig(ctx context.Context) (*ClientConfig, error) {
	var dbConfig tables.TableClientConfig
	if err := s.DB().WithContext(ctx).First(&dbConfig).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &ClientConfig{
		DropExcessRequests:             dbConfig.DropExcessRequests,
		InitialPoolSize:                dbConfig.InitialPoolSize,
		PrometheusLabels:               dbConfig.PrometheusLabels,
		EnableLogging:                  dbConfig.EnableLogging,
		DisableContentLogging:          dbConfig.DisableContentLogging,
		RetainContentInObjectStorage:   dbConfig.RetainContentInObjectStorage,
		DisableDBPingsInHealth:         dbConfig.DisableDBPingsInHealth,
		DumpErrorsInConsoleLogs:        dbConfig.DumpErrorsInConsoleLogs,
		LogRetentionDays:               dbConfig.LogRetentionDays,
		EnforceAuthOnInference:         dbConfig.EnforceAuthOnInference,
		DualCredentialConflictBehavior: dbConfig.DualCredentialConflictBehavior,
		ExtraFieldsPlacement:           dbConfig.ExtraFieldsPlacement,
		EnforceGovernanceHeader:        dbConfig.EnforceGovernanceHeader,
		EnforceSCIMAuth:                dbConfig.EnforceSCIMAuth,
		AllowedOrigins:                 dbConfig.AllowedOrigins,
		AllowedHeaders:                 dbConfig.AllowedHeaders,
		MaxRequestBodySizeMB:           dbConfig.MaxRequestBodySizeMB,
		Compat: CompatConfig{
			ConvertTextToChat:                   dbConfig.CompatConvertTextToChat,
			ConvertChatToResponses:              dbConfig.CompatConvertChatToResponses,
			ShouldDropParams:                    dbConfig.CompatShouldDropParams,
			ShouldConvertParams:                 dbConfig.CompatShouldConvertParams,
			AzureDeepseek:                       dbConfig.CompatAzureDeepseek,
			ForceReasoningOnlyModelsToResponses: dbConfig.CompatForceReasoningOnlyModelsToResponses,
			ConvertDecisionToResponses:          dbConfig.CompatConvertDecisionToResponses,
		},
		AsyncJobResultTTL:                       dbConfig.AsyncJobResultTTL,
		RequiredHeaders:                         dbConfig.RequiredHeaders,
		LoggingHeaders:                          dbConfig.LoggingHeaders,
		WhitelistedRoutes:                       dbConfig.WhitelistedRoutes,
		HideDeletedVirtualKeysInFilters:         dbConfig.HideDeletedVirtualKeysInFilters,
		DeleteExpiredVirtualKeys:                dbConfig.DeleteExpiredVirtualKeys,
		EnforceVKObjectOwnership:                dbConfig.EnforceVKObjectOwnership,
		SkipAccessProfileUserVirtualKeyCreation: dbConfig.SkipAccessProfileUserVirtualKeyCreation,
		SkipAccessProfileUserVirtualKeyDeletion: dbConfig.SkipAccessProfileUserVirtualKeyDeletion,
		EntityAccessProfileMode:                 dbConfig.EntityAccessProfileMode,
		HiddenRequestTypes:                      dbConfig.HiddenRequestTypes,
		UsageEstimationTokenizers:               dbConfig.UsageEstimationTokenizers,
		RoutingChainMaxDepth:                    dbConfig.RoutingChainMaxDepth,
		SkillsExternalClientURL:                 schemas.NewSecretVar(dbConfig.SkillsExternalClientURL),
		HeaderFilterConfig:                      dbConfig.HeaderFilterConfig,
		AllowPerRequestContentStorageOverride:   dbConfig.AllowPerRequestContentStorageOverride,
		AllowPerRequestRawOverride:              dbConfig.AllowPerRequestRawOverride,
		AllowDirectKeys:                         dbConfig.AllowDirectKeys,
		VKRotationCooldown:                      schemas.Duration(dbConfig.VKRotationCooldownNS),
		ConfigHash:                              dbConfig.ConfigHash,
	}, nil
}

// GetClientMetadata returns the UI/admin-preferences blob stored on config_client.
// Returns an empty (non-nil) map if no row exists yet or the blob is unset, so
// callers can read keys without nil-checking.
func (s *RDBConfigStore) GetClientMetadata(ctx context.Context) (map[string]any, error) {
	var dbConfig tables.TableClientConfig
	if err := s.DB().WithContext(ctx).First(&dbConfig).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if dbConfig.Metadata == nil {
		return map[string]any{}, nil
	}
	return dbConfig.Metadata, nil
}

// mergeMetadataPatch applies patch into dst following JSON Merge Patch
// semantics (RFC 7386): a nil patch value deletes the key; when both the
// existing value and the patch value are objects they are merged recursively;
// any other value replaces the existing one. dst is mutated in place.
func mergeMetadataPatch(dst, patch map[string]any) {
	for k, v := range patch {
		if v == nil {
			delete(dst, k)
			continue
		}
		patchObj, patchIsObj := v.(map[string]any)
		dstObj, dstIsObj := dst[k].(map[string]any)
		if patchIsObj && dstIsObj {
			mergeMetadataPatch(dstObj, patchObj)
			continue
		}
		dst[k] = v
	}
}

// UpdateClientMetadata merges patch into the existing metadata blob and writes
// it back via a targeted UPDATE on metadata_json only — no DELETE+CREATE, no
// risk of clobbering other ClientConfig columns. The merge follows JSON Merge
// Patch semantics (RFC 7386): nested objects are merged recursively, and keys
// with a nil value in patch are removed from the blob (callers can pass
// {"key": nil} to clear, including nested keys).
func (s *RDBConfigStore) UpdateClientMetadata(ctx context.Context, patch map[string]any) error {
	return s.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockClientConfigRow(tx); err != nil {
			return err
		}
		var existing tables.TableClientConfig
		if err := dbForUpdate(tx).First(&existing).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: client config must be initialized before metadata can be updated", ErrNotFound)
			}
			return err
		}
		merged := existing.Metadata
		if merged == nil {
			merged = map[string]any{}
		}
		mergeMetadataPatch(merged, patch)
		data, mErr := providerUtils.MarshalSorted(merged)
		if mErr != nil {
			return mErr
		}
		result := tx.Model(&tables.TableClientConfig{}).Where("id = ?", existing.ID).Update("metadata_json", string(data))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("client config metadata update affected no rows")
		}
		return nil
	})
}

// UpdateProvidersConfig updates the client configuration in the database.
func (s *RDBConfigStore) UpdateProvidersConfig(ctx context.Context, providers map[schemas.ModelProvider]ProviderConfig, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	// Pre-fetch governance FK references for all existing providers in one query.
	// ProviderConfig carries no governance fields, so without this the upsert
	// below would write NULL into budget_id/rate_limit_id on every startup.
	// If the columns don't exist yet, the fetch simply returns nothing
	governanceFKs := make(map[string]tables.TableProvider)
	var existingProviders []tables.TableProvider
	providerTableName := tables.TableProvider{}.TableName()

	if s.doesColumnExist(ctx, providerTableName, "budget_id") &&
		s.doesColumnExist(ctx, providerTableName, "rate_limit_id") {
		if err := txDB.WithContext(ctx).
			Select("name", "budget_id", "rate_limit_id").
			Find(&existingProviders).Error; err != nil {
			return fmt.Errorf("failed to prefetch provider governance fks: %w", err)
		}
		for _, p := range existingProviders {
			governanceFKs[p.Name] = p
		}
	}

	for _, providerName := range sortedProviderNames(providers) {
		providerConfig := providers[providerName]
		dbProvider := tables.TableProvider{
			Name:                     string(providerName),
			NetworkConfig:            providerConfig.NetworkConfig,
			ConcurrencyAndBufferSize: providerConfig.ConcurrencyAndBufferSize,
			ProxyConfig:              providerConfig.ProxyConfig,
			SendBackRawRequest:       providerConfig.SendBackRawRequest,
			SendBackRawResponse:      providerConfig.SendBackRawResponse,
			StoreRawRequestResponse:  providerConfig.StoreRawRequestResponse,
			IgnoreProviderCost:       providerConfig.IgnoreProviderCost,
			CustomProviderConfig:     providerConfig.CustomProviderConfig,
			OpenAIConfig:             providerConfig.OpenAIConfig,
			PromptCache:              providerConfig.PromptCache,
			Passthrough:              providerConfig.Passthrough,
			ConfigHash:               providerConfig.ConfigHash,
			Status:                   providerConfig.Status,
			Description:              providerConfig.Description,
		}

		// Carry over governance FKs from the existing row so UpdateAll never
		// overwrites them with NULL. New providers (not in governanceFKs) correctly
		// start with nil governance — governance is never set via the file sync path.
		if existing, ok := governanceFKs[string(providerName)]; ok {
			dbProvider.BudgetID = existing.BudgetID
			dbProvider.RateLimitID = existing.RateLimitID
		}

		// Upsert provider (create or update if exists).
		if err := txDB.WithContext(ctx).Clauses(
			clause.OnConflict{
				Columns:   []clause.Column{{Name: "name"}},
				UpdateAll: true,
			},
			clause.Returning{Columns: []clause.Column{{Name: "id"}}},
		).Create(&dbProvider).Error; err != nil {
			return s.parseGormError(err)
		}

		// Create keys for this provider
		dbKeys := make([]tables.TableKey, 0, len(providerConfig.Keys))
		for _, key := range providerConfig.Keys {
			// Use existing ConfigHash if set (came from reconciliation with DB),
			// otherwise generate new hash (new key from config.json)
			keyHash := key.ConfigHash
			if keyHash == "" {
				var err error
				keyHash, err = GenerateKeyHash(key)
				if err != nil {
					return fmt.Errorf("failed to generate key hash: %w", err)
				}
			}
			dbKey := tables.TableKey{
				Provider:               dbProvider.Name,
				ProviderID:             dbProvider.ID,
				KeyID:                  key.ID,
				Name:                   key.Name,
				Value:                  key.Value,
				Models:                 key.Models,
				BlacklistedModels:      key.BlacklistedModels,
				Weight:                 &key.Weight,
				Enabled:                key.Enabled,
				UseForBatchAPI:         key.UseForBatchAPI,
				UseAnthropicEndpoints:  key.UseAnthropicEndpoints,
				UseOpenAIEndpoints:     key.UseOpenAIEndpoints,
				AzureKeyConfig:         key.AzureKeyConfig,
				VertexKeyConfig:        key.VertexKeyConfig,
				BedrockKeyConfig:       key.BedrockKeyConfig,
				BedrockMantleKeyConfig: key.BedrockMantleKeyConfig,
				SageMakerKeyConfig:     key.SageMakerKeyConfig,
				Aliases:                key.Aliases,
				VLLMKeyConfig:          key.VLLMKeyConfig,
				ReplicateKeyConfig:     key.ReplicateKeyConfig,
				OllamaKeyConfig:        key.OllamaKeyConfig,
				SGLKeyConfig:           key.SGLKeyConfig,
				DatabricksKeyConfig:    key.DatabricksKeyConfig,
				CloudflareKeyConfig:    key.CloudflareKeyConfig,
				AnthropicKeyConfig:     key.AnthropicKeyConfig,
				OAuthKeyConfig:         key.OAuthKeyConfig,
				ConfigHash:             keyHash,
				Status:                 string(key.Status),
				Description:            key.Description,
			}

			// Handle Azure config
			if key.AzureKeyConfig != nil {
				dbKey.AzureEndpoint = &key.AzureKeyConfig.Endpoint
				dbKey.AzureUseSpeechEndpoints = &key.AzureKeyConfig.UseSpeechEndpoints
				dbKey.AzureUseVoiceLiveEndpoints = &key.AzureKeyConfig.UseVoiceLiveEndpoints
			}

			// Handle Vertex config
			if key.VertexKeyConfig != nil {
				dbKey.VertexProjectID = &key.VertexKeyConfig.ProjectID
				dbKey.VertexProjectNumber = &key.VertexKeyConfig.ProjectNumber
				dbKey.VertexRegion = &key.VertexKeyConfig.Region
				dbKey.VertexAuthCredentials = &key.VertexKeyConfig.AuthCredentials
				dbKey.VertexForceSingleRegion = &key.VertexKeyConfig.ForceSingleRegion
				dbKey.VertexUseSessionRouting = &key.VertexKeyConfig.UseSessionRouting
				wif, err := tables.MarshalVertexAWSWorkloadIdentityJSON(key.VertexKeyConfig.AWSWorkloadIdentity)
				if err != nil {
					return err
				}
				dbKey.VertexAWSWorkloadIdentityJSON = wif
			}

			// Handle Bedrock config
			if key.BedrockKeyConfig != nil {
				dbKey.BedrockAccessKey = &key.BedrockKeyConfig.AccessKey
				dbKey.BedrockSecretKey = &key.BedrockKeyConfig.SecretKey
				dbKey.BedrockSessionToken = key.BedrockKeyConfig.SessionToken
				dbKey.BedrockRegion = key.BedrockKeyConfig.Region
				dbKey.BedrockARN = key.BedrockKeyConfig.ARN
				dbKey.BedrockRoleARN = key.BedrockKeyConfig.RoleARN
				dbKey.BedrockExternalID = key.BedrockKeyConfig.ExternalID
				dbKey.BedrockRoleSessionName = key.BedrockKeyConfig.RoleSessionName
				dbKey.BedrockBatchRoleARN = key.BedrockKeyConfig.BatchRoleARN
				if key.BedrockKeyConfig.BatchS3Config != nil {
					data, err := sonic.Marshal(key.BedrockKeyConfig.BatchS3Config)
					if err != nil {
						return err
					}
					s := string(data)
					dbKey.BedrockBatchS3ConfigJSON = &s
				}
				if key.BedrockKeyConfig.Endpoints != nil {
					data, err := sonic.Marshal(key.BedrockKeyConfig.Endpoints)
					if err != nil {
						return err
					}
					s := string(data)
					dbKey.BedrockEndpointsJSON = &s
				}
			} else {
				dbKey.BedrockBatchS3ConfigJSON = nil
				dbKey.BedrockEndpointsJSON = nil
			}

			dbKeys = append(dbKeys, dbKey)
		}

		// Upsert keys to handle duplicates properly
		for _, dbKey := range dbKeys {
			// First try to find existing key by KeyID
			var existingKey tables.TableKey
			result := txDB.WithContext(ctx).Where("key_id = ?", dbKey.KeyID).First(&existingKey)

			if result.Error == nil {
				// Update existing key with new data
				dbKey.ID = existingKey.ID                 // Keep the same database ID
				dbKey.ProviderID = existingKey.ProviderID // Preserve the existing ProviderID
				if dbKey.Enabled == nil {
					dbKey.Enabled = existingKey.Enabled // Keep the stored value unless the file sets enabled
				}
				dbKey.Status = existingKey.Status                     // Preserve status (UI-managed)
				dbKey.Description = existingKey.Description           // Preserve description (UI-managed)
				dbKey.EncryptionStatus = existingKey.EncryptionStatus // Preserve encryption status
				dbKey.CreatedAt = existingKey.CreatedAt               // Preserve original creation timestamp
				if err := txDB.WithContext(ctx).Save(&dbKey).Error; err != nil {
					return s.parseGormError(err)
				}
			} else if errors.Is(result.Error, gorm.ErrRecordNotFound) {
				// KeyID not found, try fallback lookup by Name (handles config reload with new UUID)
				result = txDB.WithContext(ctx).Where("name = ?", dbKey.Name).First(&existingKey)
				if result.Error == nil {
					// Found by name - update existing key, preserve original KeyID
					dbKey.ID = existingKey.ID                 // Keep the same database ID
					dbKey.KeyID = existingKey.KeyID           // Preserve original KeyID
					dbKey.ProviderID = existingKey.ProviderID // Preserve the existing ProviderID
					if dbKey.Enabled == nil {
						dbKey.Enabled = existingKey.Enabled // Keep the stored value unless the file sets enabled
					}
					dbKey.Status = existingKey.Status                     // Preserve status (UI-managed)
					dbKey.Description = existingKey.Description           // Preserve description (UI-managed)
					dbKey.EncryptionStatus = existingKey.EncryptionStatus // Preserve encryption status
					dbKey.CreatedAt = existingKey.CreatedAt               // Preserve original creation timestamp
					if err := txDB.WithContext(ctx).Save(&dbKey).Error; err != nil {
						return s.parseGormError(err)
					}
				} else if errors.Is(result.Error, gorm.ErrRecordNotFound) {
					// Neither KeyID nor Name found - create new key
					if err := txDB.WithContext(ctx).Create(&dbKey).Error; err != nil {
						return s.parseGormError(err)
					}
				} else {
					// Other error occurred during name lookup
					return result.Error
				}
			} else {
				// Other error occurred
				return result.Error
			}
		}
	}
	return nil
}

// deleteJoinRowsForRemovedProviderKeys removes join-table entries that reference keys
// that are being deleted by UpdateProvider, for every VK provider config of
// provider. The caller MUST have already locked those VKPC rows (FOR UPDATE)
// before calling, so this helper performs no locking of its own. This keeps the
// resource order config_providers -> VKPC -> config_keys consistent with
// DeleteProvider and UpdateVirtualKeyProviderConfig.
//
// One statement covers every config: the provider's configs are selected by a
// subquery (idx_vk_provider_configs_provider) and the removed keys by
// idx_vkpc_keys_table_key_id, instead of one DELETE per config.
func (s *RDBConfigStore) deleteJoinRowsForRemovedProviderKeys(ctx context.Context, txDB *gorm.DB, provider string, lockedVKPCCount int, removedKeyIDs []uint) error {
	if len(removedKeyIDs) == 0 || lockedVKPCCount == 0 {
		return nil
	}
	providerConfigIDs := txDB.WithContext(ctx).Model(&tables.TableVirtualKeyProviderConfig{}).Select("id").Where("provider = ?", provider)
	return txDB.WithContext(ctx).
		Where("table_key_id IN ? AND table_virtual_key_provider_config_id IN (?)", removedKeyIDs, providerConfigIDs).
		Delete(&tables.TableVirtualKeyProviderConfigKey{}).Error
}

// cleanupVirtualKeyProviderConfigsForDeletedProvider removes every VK provider
// config of provider together with what each one owns, with the same effect as
// calling DeleteVirtualKeyProviderConfig on each: the config-key join rows, the
// budgets owned by the config, the config row, then the config's rate limit.
// The rows are first locked FOR UPDATE in id order (same lock order as before),
// then each child table is cleared with one set-based statement, so the cost no
// longer grows by several round trips per VK.
func (s *RDBConfigStore) cleanupVirtualKeyProviderConfigsForDeletedProvider(ctx context.Context, txDB *gorm.DB, provider string) error {
	type lockedConfig struct {
		ID          uint
		RateLimitID *string
	}
	var locked []lockedConfig
	if err := dbForUpdate(txDB.WithContext(ctx)).
		Model(&tables.TableVirtualKeyProviderConfig{}).
		Select("id", "rate_limit_id").
		Where("provider = ?", provider).
		Order("id ASC").
		Find(&locked).Error; err != nil {
		return err
	}
	if len(locked) == 0 {
		return nil
	}
	rateLimitIDs := make([]string, 0, len(locked))
	for _, c := range locked {
		if c.RateLimitID != nil {
			rateLimitIDs = append(rateLimitIDs, *c.RateLimitID)
		}
	}

	db := txDB.WithContext(ctx)
	providerConfigIDs := db.Model(&tables.TableVirtualKeyProviderConfig{}).Select("id").Where("provider = ?", provider)
	// Children before parents: join rows and budgets reference the config row.
	if err := db.Where("table_virtual_key_provider_config_id IN (?)", providerConfigIDs).
		Delete(&tables.TableVirtualKeyProviderConfigKey{}).Error; err != nil {
		return err
	}
	if err := db.Where("provider_config_id IN (?)", providerConfigIDs).Delete(&tables.TableBudget{}).Error; err != nil {
		return err
	}
	if err := db.Where("provider = ?", provider).Delete(&tables.TableVirtualKeyProviderConfig{}).Error; err != nil {
		return err
	}
	// Rate limits last: the config rows held the FK to them. The ids travel as one
	// bound parameter, so 100k configs never hit the bind-parameter limit.
	if len(rateLimitIDs) > 0 {
		inClause, arg := queryscope.InStrings(db, "id", rateLimitIDs)
		if err := db.Where(inClause, arg).Delete(&tables.TableRateLimit{}).Error; err != nil {
			return err
		}
	}
	return nil
}

// UpdateProvider updates a single provider configuration in the database without deleting/recreating.
func (s *RDBConfigStore) UpdateProvider(ctx context.Context, provider schemas.ModelProvider, config ProviderConfig, tx ...*gorm.DB) error {
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.UpdateProvider(ctx, provider, config, transaction)
		})
	}

	var txDB *gorm.DB
	txDB = tx[0]
	// Find the existing provider
	var dbProvider tables.TableProvider
	if err := dbForUpdate(txDB.WithContext(ctx)).Where("name = ?", string(provider)).First(&dbProvider).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}

	// Create a deep copy of the config to avoid modifying the original
	configCopy, err := deepCopy(config)
	if err != nil {
		return err
	}
	// Preserve ConfigHash (it has json:"-" tag so deepCopy via JSON doesn't copy it)
	configCopy.ConfigHash = config.ConfigHash
	// Update provider fields
	dbProvider.NetworkConfig = configCopy.NetworkConfig
	dbProvider.ConcurrencyAndBufferSize = configCopy.ConcurrencyAndBufferSize
	dbProvider.ProxyConfig = configCopy.ProxyConfig
	dbProvider.SendBackRawRequest = configCopy.SendBackRawRequest
	dbProvider.SendBackRawResponse = configCopy.SendBackRawResponse
	dbProvider.StoreRawRequestResponse = configCopy.StoreRawRequestResponse
	dbProvider.IgnoreProviderCost = configCopy.IgnoreProviderCost
	dbProvider.CustomProviderConfig = configCopy.CustomProviderConfig
	dbProvider.OpenAIConfig = configCopy.OpenAIConfig
	dbProvider.PromptCache = configCopy.PromptCache
	dbProvider.Passthrough = configCopy.Passthrough
	dbProvider.ConfigHash = configCopy.ConfigHash

	// Save the updated provider
	if err := txDB.WithContext(ctx).Save(&dbProvider).Error; err != nil {
		return s.parseGormError(err)
	}

	// Lock VKPC rows for this provider BEFORE locking config_keys so that the
	// resource order matches DeleteProvider and concurrent UpdateVirtualKeyProviderConfig
	// (which holds a VKPC row and then needs FK locks on config_keys via the join table).
	// Without this pre-lock the two paths invert on config_keys vs. VKPC and deadlock (40P01).
	// Only the ids are read: the lock is the point, and loading full rows cost
	// one row per VK holding this provider.
	var providerVKPCIDs []uint
	if err := dbForUpdate(txDB.WithContext(ctx)).
		Model(&tables.TableVirtualKeyProviderConfig{}).
		Where("provider = ?", dbProvider.Name).
		Order("id ASC").
		Pluck("id", &providerVKPCIDs).Error; err != nil {
		return err
	}

	// Get existing keys for this provider
	var existingKeys []tables.TableKey
	if err := dbForUpdate(txDB.WithContext(ctx)).Where("provider_id = ?", dbProvider.ID).Order("id ASC").Find(&existingKeys).Error; err != nil {
		return err
	}

	// Create a map of existing keys by KeyID for quick lookup
	existingKeysMap := make(map[string]tables.TableKey)
	for _, key := range existingKeys {
		existingKeysMap[key.KeyID] = key
	}

	// Process each key in the new config
	for _, key := range configCopy.Keys {
		// Generate key hash
		keyHash, err := GenerateKeyHash(key)
		if err != nil {
			return fmt.Errorf("failed to generate key hash: %w", err)
		}
		dbKey := tables.TableKey{
			Provider:               dbProvider.Name,
			ProviderID:             dbProvider.ID,
			KeyID:                  key.ID,
			Name:                   key.Name,
			Value:                  key.Value,
			Models:                 key.Models,
			BlacklistedModels:      key.BlacklistedModels,
			Weight:                 &key.Weight,
			Enabled:                key.Enabled,
			UseForBatchAPI:         key.UseForBatchAPI,
			UseAnthropicEndpoints:  key.UseAnthropicEndpoints,
			UseOpenAIEndpoints:     key.UseOpenAIEndpoints,
			AzureKeyConfig:         key.AzureKeyConfig,
			VertexKeyConfig:        key.VertexKeyConfig,
			BedrockKeyConfig:       key.BedrockKeyConfig,
			BedrockMantleKeyConfig: key.BedrockMantleKeyConfig,
			SageMakerKeyConfig:     key.SageMakerKeyConfig,
			Aliases:                key.Aliases,
			VLLMKeyConfig:          key.VLLMKeyConfig,
			ReplicateKeyConfig:     key.ReplicateKeyConfig,
			OllamaKeyConfig:        key.OllamaKeyConfig,
			SGLKeyConfig:           key.SGLKeyConfig,
			DatabricksKeyConfig:    key.DatabricksKeyConfig,
			CloudflareKeyConfig:    key.CloudflareKeyConfig,
			AnthropicKeyConfig:     key.AnthropicKeyConfig,
			OAuthKeyConfig:         key.OAuthKeyConfig,
			ConfigHash:             keyHash,
			Status:                 string(key.Status),
			Description:            key.Description,
		}

		// Handle Azure config
		if key.AzureKeyConfig != nil {
			dbKey.AzureEndpoint = &key.AzureKeyConfig.Endpoint
			dbKey.AzureUseSpeechEndpoints = &key.AzureKeyConfig.UseSpeechEndpoints
			dbKey.AzureUseVoiceLiveEndpoints = &key.AzureKeyConfig.UseVoiceLiveEndpoints
		}

		// Handle Vertex config
		if key.VertexKeyConfig != nil {
			dbKey.VertexProjectID = &key.VertexKeyConfig.ProjectID
			dbKey.VertexProjectNumber = &key.VertexKeyConfig.ProjectNumber
			dbKey.VertexRegion = &key.VertexKeyConfig.Region
			dbKey.VertexAuthCredentials = &key.VertexKeyConfig.AuthCredentials
			dbKey.VertexForceSingleRegion = &key.VertexKeyConfig.ForceSingleRegion
			dbKey.VertexUseSessionRouting = &key.VertexKeyConfig.UseSessionRouting
			wif, err := tables.MarshalVertexAWSWorkloadIdentityJSON(key.VertexKeyConfig.AWSWorkloadIdentity)
			if err != nil {
				return err
			}
			dbKey.VertexAWSWorkloadIdentityJSON = wif
		}

		// Handle Bedrock config
		if key.BedrockKeyConfig != nil {
			dbKey.BedrockAccessKey = &key.BedrockKeyConfig.AccessKey
			dbKey.BedrockSecretKey = &key.BedrockKeyConfig.SecretKey
			dbKey.BedrockSessionToken = key.BedrockKeyConfig.SessionToken
			dbKey.BedrockRegion = key.BedrockKeyConfig.Region
			dbKey.BedrockARN = key.BedrockKeyConfig.ARN
			dbKey.BedrockRoleARN = key.BedrockKeyConfig.RoleARN
			dbKey.BedrockExternalID = key.BedrockKeyConfig.ExternalID
			dbKey.BedrockRoleSessionName = key.BedrockKeyConfig.RoleSessionName
			dbKey.BedrockBatchRoleARN = key.BedrockKeyConfig.BatchRoleARN
			if key.BedrockKeyConfig.BatchS3Config != nil {
				data, err := sonic.Marshal(key.BedrockKeyConfig.BatchS3Config)
				if err != nil {
					return err
				}
				s := string(data)
				dbKey.BedrockBatchS3ConfigJSON = &s
			} else {
				dbKey.BedrockBatchS3ConfigJSON = nil
			}
			if key.BedrockKeyConfig.Endpoints != nil {
				data, err := sonic.Marshal(key.BedrockKeyConfig.Endpoints)
				if err != nil {
					return err
				}
				s := string(data)
				dbKey.BedrockEndpointsJSON = &s
			} else {
				dbKey.BedrockEndpointsJSON = nil
			}
		}

		// Check if this key already exists
		if existingKey, exists := existingKeysMap[key.ID]; exists {
			dbKey.ID = existingKey.ID                             // Keep the same database ID
			dbKey.ConfigHash = existingKey.ConfigHash             // Preserve config hash
			dbKey.Status = existingKey.Status                     // Preserve status (UI-managed)
			dbKey.Description = existingKey.Description           // Preserve description (UI-managed)
			dbKey.EncryptionStatus = existingKey.EncryptionStatus // Preserve encryption status
			dbKey.CreatedAt = existingKey.CreatedAt               // Preserve original creation timestamp
			if err := txDB.WithContext(ctx).Save(&dbKey).Error; err != nil {
				return s.parseGormError(err)
			}
			delete(existingKeysMap, key.ID)
		} else {
			if err := txDB.WithContext(ctx).Create(&dbKey).Error; err != nil {
				return s.parseGormError(err)
			}
		}
	}

	removedProviderKeyIDs := make([]uint, 0, len(existingKeysMap))
	for _, keyToDelete := range existingKeysMap {
		removedProviderKeyIDs = append(removedProviderKeyIDs, keyToDelete.ID)
	}
	removedProviderKeyIDs = sortedUintCopy(removedProviderKeyIDs)
	if err := s.deleteJoinRowsForRemovedProviderKeys(ctx, txDB, dbProvider.Name, len(providerVKPCIDs), removedProviderKeyIDs); err != nil {
		return err
	}

	// Delete keys that are no longer in the new config
	removedKeys := make([]tables.TableKey, 0, len(existingKeysMap))
	for _, keyToDelete := range existingKeysMap {
		removedKeys = append(removedKeys, keyToDelete)
	}
	sortTableKeysByID(removedKeys)
	for _, keyToDelete := range removedKeys {
		if err := txDB.WithContext(ctx).Delete(&keyToDelete).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
	}

	return nil
}

// AddProvider creates a new provider configuration in the database.
func (s *RDBConfigStore) AddProvider(ctx context.Context, provider schemas.ModelProvider, config ProviderConfig, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	// Create a deep copy of the config to avoid modifying the original
	configCopy, err := deepCopy(config)
	if err != nil {
		return err
	}
	// Preserve ConfigHash (it has json:"-" tag so deepCopy via JSON doesn't copy it)
	configCopy.ConfigHash = config.ConfigHash
	// Create new provider
	dbProvider := tables.TableProvider{
		Name:                     string(provider),
		NetworkConfig:            configCopy.NetworkConfig,
		ConcurrencyAndBufferSize: configCopy.ConcurrencyAndBufferSize,
		ProxyConfig:              configCopy.ProxyConfig,
		SendBackRawRequest:       configCopy.SendBackRawRequest,
		SendBackRawResponse:      configCopy.SendBackRawResponse,
		StoreRawRequestResponse:  configCopy.StoreRawRequestResponse,
		IgnoreProviderCost:       configCopy.IgnoreProviderCost,
		CustomProviderConfig:     configCopy.CustomProviderConfig,
		OpenAIConfig:             configCopy.OpenAIConfig,
		PromptCache:              configCopy.PromptCache,
		Passthrough:              configCopy.Passthrough,
		ConfigHash:               configCopy.ConfigHash,
	}
	// Create the provider
	if err := txDB.WithContext(ctx).Create(&dbProvider).Error; err != nil {
		return s.parseGormError(err)
	}
	// Create keys for this provider
	for _, key := range configCopy.Keys {
		dbKey := tables.TableKey{
			Provider:               dbProvider.Name,
			ProviderID:             dbProvider.ID,
			KeyID:                  key.ID,
			Name:                   key.Name,
			Value:                  key.Value,
			Models:                 key.Models,
			BlacklistedModels:      key.BlacklistedModels,
			Weight:                 &key.Weight,
			Enabled:                key.Enabled,
			UseForBatchAPI:         key.UseForBatchAPI,
			UseAnthropicEndpoints:  key.UseAnthropicEndpoints,
			UseOpenAIEndpoints:     key.UseOpenAIEndpoints,
			AzureKeyConfig:         key.AzureKeyConfig,
			VertexKeyConfig:        key.VertexKeyConfig,
			BedrockKeyConfig:       key.BedrockKeyConfig,
			BedrockMantleKeyConfig: key.BedrockMantleKeyConfig,
			SageMakerKeyConfig:     key.SageMakerKeyConfig,
			Aliases:                key.Aliases,
			VLLMKeyConfig:          key.VLLMKeyConfig,
			ReplicateKeyConfig:     key.ReplicateKeyConfig,
			OllamaKeyConfig:        key.OllamaKeyConfig,
			SGLKeyConfig:           key.SGLKeyConfig,
			DatabricksKeyConfig:    key.DatabricksKeyConfig,
			CloudflareKeyConfig:    key.CloudflareKeyConfig,
			AnthropicKeyConfig:     key.AnthropicKeyConfig,
			OAuthKeyConfig:         key.OAuthKeyConfig,
			ConfigHash:             key.ConfigHash,
			Status:                 string(key.Status),
			Description:            key.Description,
		}
		// Handle Azure config
		if key.AzureKeyConfig != nil {
			dbKey.AzureEndpoint = &key.AzureKeyConfig.Endpoint
			dbKey.AzureUseSpeechEndpoints = &key.AzureKeyConfig.UseSpeechEndpoints
			dbKey.AzureUseVoiceLiveEndpoints = &key.AzureKeyConfig.UseVoiceLiveEndpoints
		}
		// Handle Vertex config
		if key.VertexKeyConfig != nil {
			dbKey.VertexProjectID = &key.VertexKeyConfig.ProjectID
			dbKey.VertexProjectNumber = &key.VertexKeyConfig.ProjectNumber
			dbKey.VertexRegion = &key.VertexKeyConfig.Region
			dbKey.VertexAuthCredentials = &key.VertexKeyConfig.AuthCredentials
			dbKey.VertexForceSingleRegion = &key.VertexKeyConfig.ForceSingleRegion
			dbKey.VertexUseSessionRouting = &key.VertexKeyConfig.UseSessionRouting
			wif, err := tables.MarshalVertexAWSWorkloadIdentityJSON(key.VertexKeyConfig.AWSWorkloadIdentity)
			if err != nil {
				return err
			}
			dbKey.VertexAWSWorkloadIdentityJSON = wif
		}
		// Handle Bedrock config
		if key.BedrockKeyConfig != nil {
			dbKey.BedrockAccessKey = &key.BedrockKeyConfig.AccessKey
			dbKey.BedrockSecretKey = &key.BedrockKeyConfig.SecretKey
			dbKey.BedrockSessionToken = key.BedrockKeyConfig.SessionToken
			dbKey.BedrockRegion = key.BedrockKeyConfig.Region
			dbKey.BedrockARN = key.BedrockKeyConfig.ARN
			dbKey.BedrockRoleARN = key.BedrockKeyConfig.RoleARN
			dbKey.BedrockExternalID = key.BedrockKeyConfig.ExternalID
			dbKey.BedrockRoleSessionName = key.BedrockKeyConfig.RoleSessionName
			dbKey.BedrockBatchRoleARN = key.BedrockKeyConfig.BatchRoleARN
			if key.BedrockKeyConfig.BatchS3Config != nil {
				data, err := sonic.Marshal(key.BedrockKeyConfig.BatchS3Config)
				if err != nil {
					return err
				}
				s := string(data)
				dbKey.BedrockBatchS3ConfigJSON = &s
			} else {
				dbKey.BedrockBatchS3ConfigJSON = nil
			}
			if key.BedrockKeyConfig.Endpoints != nil {
				data, err := sonic.Marshal(key.BedrockKeyConfig.Endpoints)
				if err != nil {
					return err
				}
				s := string(data)
				dbKey.BedrockEndpointsJSON = &s
			} else {
				dbKey.BedrockEndpointsJSON = nil
			}
		}

		// Create the key
		if err := txDB.WithContext(ctx).Create(&dbKey).Error; err != nil {
			return s.parseGormError(err)
		}
	}

	return nil
}

// DeleteProvider deletes a single provider and all its associated keys from the database.
func (s *RDBConfigStore) DeleteProvider(ctx context.Context, provider schemas.ModelProvider, tx ...*gorm.DB) error {
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.DeleteProvider(ctx, provider, transaction)
		})
	}

	var txDB *gorm.DB
	txDB = tx[0]
	// Find the existing provider
	var dbProvider tables.TableProvider
	if err := dbForUpdate(txDB.WithContext(ctx)).Where("name = ?", string(provider)).First(&dbProvider).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}

	if err := s.cleanupVirtualKeyProviderConfigsForDeletedProvider(ctx, txDB, dbProvider.Name); err != nil {
		return err
	}

	// Store the budget and rate limit IDs before deleting
	budgetID := dbProvider.BudgetID
	rateLimitID := dbProvider.RateLimitID

	// Delete the provider first (keys will be deleted due to CASCADE constraint)
	if err := txDB.WithContext(ctx).Delete(&dbProvider).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}

	// Delete the budget if it exists
	if budgetID != nil {
		if err := txDB.WithContext(ctx).Delete(&tables.TableBudget{}, "id = ?", *budgetID).Error; err != nil {
			return err
		}
	}
	// Delete the rate limit if it exists
	if rateLimitID != nil {
		if err := txDB.WithContext(ctx).Delete(&tables.TableRateLimit{}, "id = ?", *rateLimitID).Error; err != nil {
			return err
		}
	}

	// Clean up model configs scoped to this provider (and their owned budgets/rate-limits).
	if err := s.deleteModelConfigsWhere(ctx, txDB, "provider = ?", string(provider)); err != nil {
		return err
	}

	return nil
}

// GetProvidersConfig retrieves the provider configuration from the database.
func (s *RDBConfigStore) GetProvidersConfig(ctx context.Context) (map[schemas.ModelProvider]ProviderConfig, error) {
	var dbProviders []tables.TableProvider
	if err := s.DB().WithContext(ctx).Preload("Keys").Find(&dbProviders).Error; err != nil {
		return nil, err
	}
	if len(dbProviders) == 0 {
		// No providers in database, auto-detect from environment
		return nil, nil
	}
	processedProviders := make(map[schemas.ModelProvider]ProviderConfig)
	for _, dbProvider := range dbProviders {
		provider := schemas.ModelProvider(dbProvider.Name)
		// Convert database keys to schemas.Key
		keys := make([]schemas.Key, len(dbProvider.Keys))
		for i, dbKey := range dbProvider.Keys {
			keys[i] = schemaKeyFromTableKey(dbKey)
		}
		providerConfig := ProviderConfig{
			Keys:                     keys,
			NetworkConfig:            dbProvider.NetworkConfig,
			ConcurrencyAndBufferSize: dbProvider.ConcurrencyAndBufferSize,
			ProxyConfig:              dbProvider.ProxyConfig,
			SendBackRawRequest:       dbProvider.SendBackRawRequest,
			SendBackRawResponse:      dbProvider.SendBackRawResponse,
			StoreRawRequestResponse:  dbProvider.StoreRawRequestResponse,
			IgnoreProviderCost:       dbProvider.IgnoreProviderCost,
			CustomProviderConfig:     dbProvider.CustomProviderConfig,
			OpenAIConfig:             dbProvider.OpenAIConfig,
			PromptCache:              dbProvider.PromptCache,
			Passthrough:              dbProvider.Passthrough,
			ConfigHash:               dbProvider.ConfigHash,
			Status:                   dbProvider.Status,
			Description:              dbProvider.Description,
		}
		processedProviders[provider] = providerConfig
	}
	return processedProviders, nil
}

// GetProviderConfig retrieves the provider configuration from the database.
func (s *RDBConfigStore) GetProviderConfig(ctx context.Context, provider schemas.ModelProvider) (*ProviderConfig, error) {
	var dbProvider tables.TableProvider
	if err := s.DB().WithContext(ctx).Preload("Keys").Where("name = ?", string(provider)).First(&dbProvider).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	keys := make([]schemas.Key, len(dbProvider.Keys))
	for i, dbKey := range dbProvider.Keys {
		keys[i] = schemaKeyFromTableKey(dbKey)
	}
	return &ProviderConfig{
		Keys:                     keys,
		NetworkConfig:            dbProvider.NetworkConfig,
		ConcurrencyAndBufferSize: dbProvider.ConcurrencyAndBufferSize,
		ProxyConfig:              dbProvider.ProxyConfig,
		SendBackRawRequest:       dbProvider.SendBackRawRequest,
		SendBackRawResponse:      dbProvider.SendBackRawResponse,
		StoreRawRequestResponse:  dbProvider.StoreRawRequestResponse,
		IgnoreProviderCost:       dbProvider.IgnoreProviderCost,
		CustomProviderConfig:     dbProvider.CustomProviderConfig,
		OpenAIConfig:             dbProvider.OpenAIConfig,
		PromptCache:              dbProvider.PromptCache,
		Passthrough:              dbProvider.Passthrough,
		ConfigHash:               dbProvider.ConfigHash,
		Status:                   dbProvider.Status,
		Description:              dbProvider.Description,
	}, nil
}

// GetProviderKeys retrieves all keys for a provider ordered by creation time.
func (s *RDBConfigStore) GetProviderKeys(ctx context.Context, provider schemas.ModelProvider) ([]schemas.Key, error) {
	var dbKeys []tables.TableKey
	result := s.DB().WithContext(ctx).
		Table("config_providers").
		Select("config_keys.*").
		Joins("LEFT JOIN config_keys ON config_keys.provider_id = config_providers.id").
		Where("config_providers.name = ?", string(provider)).
		Order("config_keys.created_at ASC").
		Scan(&dbKeys)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrNotFound
	}
	if len(dbKeys) == 1 && dbKeys[0].ID == 0 && dbKeys[0].KeyID == "" {
		return []schemas.Key{}, nil
	}

	keys := make([]schemas.Key, 0, len(dbKeys))
	for _, dbKey := range dbKeys {
		if dbKey.ID == 0 && dbKey.KeyID == "" {
			continue
		}
		if err := dbKey.AfterFind(nil); err != nil {
			return nil, err
		}
		keys = append(keys, schemaKeyFromTableKey(dbKey))
	}

	return keys, nil
}

func (s *RDBConfigStore) getProviderKeyByName(ctx context.Context, txDB *gorm.DB, provider schemas.ModelProvider, keyID string) (*tables.TableKey, error) {
	var dbKey tables.TableKey
	if err := dbForUpdate(txDB.WithContext(ctx)).
		Table("config_keys").
		Select("config_keys.*").
		Joins("JOIN config_providers ON config_providers.id = config_keys.provider_id").
		Where("config_providers.name = ? AND config_keys.key_id = ?", string(provider), keyID).
		First(&dbKey).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &dbKey, nil
}

// GetProviderKey retrieves a single key for a provider.
func (s *RDBConfigStore) GetProviderKey(ctx context.Context, provider schemas.ModelProvider, keyID string) (*schemas.Key, error) {
	dbKey, err := s.getProviderKeyByName(ctx, s.DB(), provider, keyID)
	if err != nil {
		return nil, err
	}

	key := schemaKeyFromTableKey(*dbKey)
	return &key, nil
}

// CreateProviderKey creates a new key for an existing provider.
func (s *RDBConfigStore) CreateProviderKey(ctx context.Context, provider schemas.ModelProvider, key schemas.Key, tx ...*gorm.DB) error {
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.CreateProviderKey(ctx, provider, key, transaction)
		})
	}

	var txDB *gorm.DB
	txDB = tx[0]
	var dbProvider tables.TableProvider
	if err := dbForUpdate(txDB.WithContext(ctx)).Where("name = ?", string(provider)).First(&dbProvider).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	dbKey, err := tableKeyFromSchemaKey(dbProvider, key)
	if err != nil {
		return err
	}
	if err := txDB.WithContext(ctx).Create(&dbKey).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// UpdateProviderKey updates a single key for an existing provider.
func (s *RDBConfigStore) UpdateProviderKey(ctx context.Context, provider schemas.ModelProvider, keyID string, key schemas.Key, tx ...*gorm.DB) error {
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.UpdateProviderKey(ctx, provider, keyID, key, transaction)
		})
	}

	var txDB *gorm.DB
	txDB = tx[0]

	existingKey, err := s.getProviderKeyByName(ctx, txDB, provider, keyID)
	if err != nil {
		return err
	}

	dbKey, err := tableKeyFromSchemaKey(tables.TableProvider{
		ID:   existingKey.ProviderID,
		Name: existingKey.Provider,
	}, key)
	if err != nil {
		return err
	}
	dbKey.ID = existingKey.ID
	dbKey.KeyID = existingKey.KeyID
	dbKey.ProviderID = existingKey.ProviderID
	dbKey.Provider = existingKey.Provider
	dbKey.ConfigHash = existingKey.ConfigHash
	dbKey.EncryptionStatus = existingKey.EncryptionStatus
	dbKey.CreatedAt = existingKey.CreatedAt // Preserve original creation timestamp

	if err := txDB.WithContext(ctx).Save(&dbKey).Error; err != nil {
		return s.parseGormError(err)
	}

	return nil
}

// DeleteProviderKey deletes a single key for an existing provider.
func (s *RDBConfigStore) DeleteProviderKey(ctx context.Context, provider schemas.ModelProvider, keyID string, tx ...*gorm.DB) error {
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.DeleteProviderKey(ctx, provider, keyID, transaction)
		})
	}

	var txDB *gorm.DB
	txDB = tx[0]

	var dbProvider tables.TableProvider
	if err := dbForUpdate(txDB.WithContext(ctx)).Where("name = ?", string(provider)).First(&dbProvider).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	var dbKey tables.TableKey
	if err := dbForUpdate(txDB.WithContext(ctx)).
		Where("provider_id = ? AND key_id = ?", dbProvider.ID, keyID).
		First(&dbKey).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	if err := txDB.WithContext(ctx).
		Table("governance_virtual_key_provider_config_keys").
		Where("table_key_id = ?", dbKey.ID).
		Delete(nil).Error; err != nil {
		return err
	}

	result := txDB.WithContext(ctx).Delete(&dbKey)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}

	return nil
}

// GetProviders retrieves all providers from the database with their governance relationships.
func (s *RDBConfigStore) GetProviders(ctx context.Context) ([]tables.TableProvider, error) {
	var providers []tables.TableProvider
	if err := s.DB().WithContext(ctx).Preload("Budget").Preload("RateLimit").Find(&providers).Error; err != nil {
		return nil, err
	}
	return providers, nil
}

// GetProvider retrieves a provider by name from the database with governance relationships.
func (s *RDBConfigStore) GetProvider(ctx context.Context, provider schemas.ModelProvider) (*tables.TableProvider, error) {
	var providerInfo tables.TableProvider
	if err := s.DB().WithContext(ctx).Preload("Budget").Preload("RateLimit").Where("name = ?", string(provider)).First(&providerInfo).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &providerInfo, nil
}

// GetProviderByName retrieves a provider by name from the database with governance relationships.
func (s *RDBConfigStore) GetProviderByName(ctx context.Context, name string) (*tables.TableProvider, error) {
	var provider tables.TableProvider
	if err := s.DB().WithContext(ctx).Preload("Budget").Preload("RateLimit").Where("name = ?", name).First(&provider).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &provider, nil
}

// UpdateStatus updates the status for either a key or provider.
// - If keyID is non-empty: updates the key's status (for keyed providers)
// - If keyID is empty and provider is non-empty: updates the provider's status (for keyless providers)
func (s *RDBConfigStore) UpdateStatus(ctx context.Context, provider schemas.ModelProvider, keyID string, status, description string) error {
	// Update key-level status (for keyed providers)
	if keyID != "" {
		result := s.DB().WithContext(ctx).
			Model(&tables.TableKey{}).
			Where("key_id = ?", keyID).
			Updates(map[string]interface{}{
				"status":      status,
				"description": description,
			})
		if result.Error != nil {
			return s.parseGormError(result.Error)
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	}

	// Update provider-level status (for keyless providers)
	if provider != "" {
		result := s.DB().WithContext(ctx).
			Model(&tables.TableProvider{}).
			Where("name = ?", string(provider)).
			Updates(map[string]interface{}{
				"status":      status,
				"description": description,
			})
		if result.Error != nil {
			return s.parseGormError(result.Error)
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	}

	return fmt.Errorf("either keyID or provider must be non-empty")
}

// GetVectorStoreConfig retrieves the vector store configuration from the database.
func (s *RDBConfigStore) GetVectorStoreConfig(ctx context.Context) (*vectorstore.Config, error) {
	var vectorStoreTableConfig tables.TableVectorStoreConfig
	if err := s.DB().WithContext(ctx).First(&vectorStoreTableConfig).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Return default cache configuration
			return nil, nil
		}
		return nil, err
	}
	return &vectorstore.Config{
		Enabled: vectorStoreTableConfig.Enabled,
		Config:  vectorStoreTableConfig.Config,
		Type:    vectorstore.VectorStoreType(vectorStoreTableConfig.Type),
	}, nil
}

// UpdateVectorStoreConfig updates the vector store configuration in the database.
func (s *RDBConfigStore) UpdateVectorStoreConfig(ctx context.Context, config *vectorstore.Config) error {
	return s.DB().Transaction(func(tx *gorm.DB) error {
		// Delete existing cache config
		if err := tx.WithContext(ctx).Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&tables.TableVectorStoreConfig{}).Error; err != nil {
			return err
		}
		jsonConfig, err := marshalToStringPtr(config.Config)
		if err != nil {
			return err
		}
		record := &tables.TableVectorStoreConfig{
			Type:    string(config.Type),
			Enabled: config.Enabled,
			Config:  jsonConfig,
		}
		// Create new cache config
		return tx.WithContext(ctx).Create(record).Error
	})
}

// GetLogsStoreConfig retrieves the logs store configuration from the database.
func (s *RDBConfigStore) GetLogsStoreConfig(ctx context.Context) (*logstore.Config, error) {
	var dbConfig tables.TableLogStoreConfig
	if err := s.DB().WithContext(ctx).First(&dbConfig).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if dbConfig.Config == nil || *dbConfig.Config == "" {
		return &logstore.Config{Enabled: dbConfig.Enabled}, nil
	}
	var logStoreConfig logstore.Config
	if err := json.Unmarshal([]byte(*dbConfig.Config), &logStoreConfig); err != nil {
		return nil, err
	}
	return &logStoreConfig, nil
}

// UpdateLogsStoreConfig updates the logs store configuration in the database.
func (s *RDBConfigStore) UpdateLogsStoreConfig(ctx context.Context, config *logstore.Config) error {
	return s.DB().Transaction(func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&tables.TableLogStoreConfig{}).Error; err != nil {
			return err
		}
		jsonConfig, err := marshalToStringPtr(config)
		if err != nil {
			return err
		}
		record := &tables.TableLogStoreConfig{
			Enabled: config.Enabled,
			Type:    string(config.Type),
			Config:  jsonConfig,
		}
		return tx.WithContext(ctx).Create(record).Error
	})
}

// GetConfig retrieves a specific config from the database.
func (s *RDBConfigStore) GetConfig(ctx context.Context, key string) (*tables.TableGovernanceConfig, error) {
	var config tables.TableGovernanceConfig
	if err := s.DB().WithContext(ctx).First(&config, "key = ?", key).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &config, nil
}

// UpdateConfig updates a specific config in the database.
func (s *RDBConfigStore) UpdateConfig(ctx context.Context, config *tables.TableGovernanceConfig, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	return txDB.WithContext(ctx).Save(config).Error
}

// GetModelPrices retrieves all model pricing records from the database.
func (s *RDBConfigStore) GetModelPrices(ctx context.Context) ([]tables.TableModelPricing, error) {
	var modelPrices []tables.TableModelPricing
	if err := s.DB().WithContext(ctx).Find(&modelPrices).Error; err != nil {
		return nil, err
	}
	return modelPrices, nil
}

// pricingSyncUpdateColumns is the explicit set of governance_model_pricing
// columns the pricing sync is allowed to overwrite via ON CONFLICT. Mirrors
// every column on TableModelPricing except `id` (the primary key) and
// `additional_attributes` (editorial metadata that must survive sync).
// Keep this list in lockstep with the table definition in
// framework/configstore/tables/modelpricing.go.
var pricingSyncUpdateColumns = []string{
	"model",
	"base_model",
	"provider",
	"mode",
	"context_length",
	"max_input_tokens",
	"max_output_tokens",
	"architecture",
	"is_deprecated",
	// Costs - Text
	"input_cost_per_token",
	"output_cost_per_token",
	"input_cost_per_token_batches",
	"output_cost_per_token_batches",
	"input_cost_per_token_priority",
	"output_cost_per_token_priority",
	"input_cost_per_token_ultrafast",
	"output_cost_per_token_ultrafast",
	"input_cost_per_token_flex",
	"output_cost_per_token_flex",
	"input_cost_per_token_fast",
	"output_cost_per_token_fast",
	"input_cost_per_character",
	// Costs - Decisions
	"input_cost_per_token_decisions",
	"output_cost_per_token_decisions",
	// Costs - 128k Tier
	"input_cost_per_token_above_128k_tokens",
	"input_cost_per_image_above_128k_tokens",
	"input_cost_per_video_per_second_above_128k_tokens",
	"input_cost_per_audio_per_second_above_128k_tokens",
	"output_cost_per_token_above_128k_tokens",
	// Costs - 100k Tier
	"input_cost_per_token_above_100k_tokens",
	"output_cost_per_token_above_100k_tokens",
	// Costs - 200k Tier
	"input_cost_per_token_above_200k_tokens",
	"input_cost_per_token_above_200k_tokens_priority",
	"output_cost_per_token_above_200k_tokens",
	"output_cost_per_token_above_200k_tokens_priority",
	// Costs - 272k Tier
	"input_cost_per_token_above_272k_tokens",
	"input_cost_per_token_above_272k_tokens_priority",
	"input_cost_per_token_flex_above_272k_tokens",
	"output_cost_per_token_above_272k_tokens",
	"output_cost_per_token_above_272k_tokens_priority",
	"output_cost_per_token_flex_above_272k_tokens",
	"input_cost_per_token_above_272k_tokens_ultrafast",
	"output_cost_per_token_above_272k_tokens_ultrafast",
	// Costs - Cache
	"cache_creation_input_token_cost",
	"cache_read_input_token_cost",
	"cache_creation_input_token_cost_above_200k_tokens",
	"cache_read_input_token_cost_above_200k_tokens",
	"cache_read_input_token_cost_above_200k_tokens_priority",
	"cache_creation_input_token_cost_above_1hr",
	"cache_creation_input_token_cost_above_1hr_above_200k_tokens",
	"cache_creation_input_token_cost_above_100k_tokens",
	"cache_read_input_token_cost_above_100k_tokens",
	"cache_creation_input_token_cost_above_1hr_above_100k_tokens",
	"cache_creation_input_audio_token_cost",
	"cache_read_input_token_cost_priority",
	"cache_read_input_token_cost_ultrafast",
	"cache_read_input_token_cost_flex",
	"cache_read_input_image_token_cost",
	"cache_read_input_token_cost_above_272k_tokens",
	"cache_read_input_token_cost_above_272k_tokens_priority",
	"cache_read_input_token_cost_flex_above_272k_tokens",
	"cache_read_input_token_cost_above_272k_tokens_ultrafast",
	"cache_creation_input_token_cost_above_272k_tokens",
	"cache_creation_input_token_cost_flex",
	"cache_creation_input_token_cost_flex_above_272k_tokens",
	"cache_creation_input_token_cost_priority",
	"cache_creation_input_token_cost_above_272k_tokens_priority",
	"cache_creation_input_token_cost_ultrafast",
	"cache_creation_input_token_cost_above_272k_tokens_ultrafast",
	"cache_creation_input_token_cost_fast",
	"cache_creation_input_token_cost_above_1hr_fast",
	"cache_creation_input_token_cost_above_12hr",
	"cache_creation_input_token_cost_above_12hr_above_272k_tokens",
	"cache_creation_input_token_cost_above_12hr_priority",
	"cache_creation_input_token_cost_above_12hr_above_272k_priority",
	"cache_creation_input_token_cost_above_12hr_flex",
	"cache_creation_input_token_cost_above_12hr_above_272k_flex",
	"cache_creation_input_token_cost_above_12hr_ultrafast",
	"cache_creation_input_token_cost_above_12hr_above_272k_ultrafast",
	"cache_read_input_token_cost_fast",
	"inference_geo_us_multiplier",
	// Costs - Image
	"input_cost_per_image",
	"input_cost_per_pixel",
	"output_cost_per_image",
	"output_cost_per_pixel",
	"output_cost_per_image_premium_image",
	"output_cost_per_image_above_512_and_512_pixels",
	"output_cost_per_image_above_512x512_pixels_premium",
	"output_cost_per_image_above_1024_and_1024_pixels",
	"output_cost_per_image_above_1024x1024_pixels_premium",
	"output_cost_per_image_above_2048_and_2048_pixels",
	"output_cost_per_image_above_4096_and_4096_pixels",
	"output_cost_per_image_above_4_megapixels",
	"output_cost_per_image_above_8_megapixels",
	"output_cost_per_image_above_16_megapixels",
	"output_cost_per_image_above_32_megapixels",
	"output_cost_per_image_above_64_megapixels",
	"output_cost_per_image_low_quality",
	"output_cost_per_image_medium_quality",
	"output_cost_per_image_high_quality",
	"output_cost_per_image_auto_quality",
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
	"input_cost_per_image_token",
	"output_cost_per_image_token",
	// Costs - Audio/Video
	"input_cost_per_audio_token",
	"input_cost_per_audio_per_second",
	"input_cost_per_second",
	"input_cost_per_video_per_second",
	"output_cost_per_audio_token",
	"output_cost_per_video_per_second",
	"output_cost_per_second",
	"input_cost_per_token_voice_live",
	"output_cost_per_token_voice_live",
	"cache_read_input_token_cost_voice_live",
	"input_cost_per_audio_token_voice_live",
	"output_cost_per_audio_token_voice_live",
	"input_cost_per_standard_speech_audio_token_voice_live",
	"output_cost_per_standard_speech_audio_token_voice_live",
	"input_cost_per_custom_speech_audio_token_voice_live",
	"output_cost_per_custom_speech_audio_token_voice_live",
	"output_cost_per_video_per_second_480p",
	"output_cost_per_video_per_second_720p",
	"output_cost_per_video_per_second_1024p",
	"output_cost_per_video_per_second_1080p",
	"output_cost_per_video_per_second_4k",
	"output_cost_per_video_token",
	// Costs - Other
	"search_context_cost_per_query",
	"web_search_cost_per_request",
	"input_cost_per_query",
	"code_interpreter_cost_per_session",
	"cost_per_request",
	// Costs - OCR
	"ocr_cost_per_page",
	"annotation_cost_per_page",
	// Costs - Time of day
	"off_peak_cost_multiplier",
	"peak_hours",
}

// UpsertModelPrices creates or updates a model pricing record in the database.
// Uses a single atomic ON CONFLICT statement to avoid deadlocks in multinode deployments
// where multiple nodes may attempt concurrent upserts for the same model on startup.
//
// The update list is intentionally explicit (pricingSyncUpdateColumns) rather
// than UpdateAll: every datasheet-sourced column is enumerated, but
// `additional_attributes` is omitted so the 24-hour pricing sync never
// overwrites editorial metadata set via UpsertModelPricingAttributes.
func (s *RDBConfigStore) UpsertModelPrices(ctx context.Context, pricing *tables.TableModelPricing, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	db := txDB.WithContext(ctx)

	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "model"}, {Name: "provider"}, {Name: "mode"}},
		DoUpdates: clause.AssignmentColumns(pricingSyncUpdateColumns),
	}).Create(pricing).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// pricingUpsertBatchSize bounds rows per multi-row INSERT on the PostgreSQL
// pricing-sync path, sized well under PostgreSQL's 65535 bind-parameter ceiling
// (TableModelPricing has ~76 columns, so ~860 rows would hit the limit; 500
// leaves comfortable headroom). Only PostgreSQL batches — see
// UpsertModelPricesBatch.
const pricingUpsertBatchSize = 500

// UpsertModelPricesBatch creates or updates many model pricing records with the
// same semantics as UpsertModelPrices (pricingSyncUpdateColumns only, so
// additional_attributes editorial metadata survives the sync). Callers must
// dedup by (model, provider, mode) first: a conflict target may not appear
// twice in one statement.
//
// The whole write runs in one transaction so the sync stays atomic. PostgreSQL
// uses chunked multi-row ON CONFLICT statements to cut the per-model round-trips
// that motivated batching against a remote DB. SQLite cannot batch these rows: a
// multi-row VALUES list rejects the DEFAULT keyword GORM emits for the table's
// many default:null columns ("near \"DEFAULT\": syntax error"), and its
// bind-variable ceiling is lower. SQLite is a local file, so per-row writes are
// ~free — it loops within the same transaction instead.
func (s *RDBConfigStore) UpsertModelPricesBatch(ctx context.Context, pricing []tables.TableModelPricing, tx ...*gorm.DB) error {
	if len(pricing) == 0 {
		return nil
	}
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	db := txDB.WithContext(ctx)

	onConflict := clause.OnConflict{
		Columns:   []clause.Column{{Name: "model"}, {Name: "provider"}, {Name: "mode"}},
		DoUpdates: clause.AssignmentColumns(pricingSyncUpdateColumns),
	}

	return db.Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Clauses(onConflict).CreateInBatches(pricing, pricingUpsertBatchSize).Error; err != nil {
				return s.parseGormError(err)
			}
			return nil
		}
		for i := range pricing {
			if err := tx.Clauses(onConflict).Create(&pricing[i]).Error; err != nil {
				return s.parseGormError(err)
			}
		}
		return nil
	})
}

// UpsertModelPricingAttributes writes only the additional_attributes column
// for the pricing row keyed by (model, provider). The row must already exist
// — callers may not seed pricing rows through this path; the management API
// enforces that. A nil/empty attrs map clears the column to an empty JSON object.
func (s *RDBConfigStore) UpsertModelPricingAttributes(ctx context.Context, model, provider string, attrs map[string]string, tx ...*gorm.DB) (int64, error) {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	db := txDB.WithContext(ctx)

	var value string
	if len(attrs) == 0 {
		value = "{}"
	} else {
		encoded, err := json.Marshal(attrs)
		if err != nil {
			return 0, fmt.Errorf("marshal additional_attributes: %w", err)
		}
		value = string(encoded)
	}

	res := db.Model(&tables.TableModelPricing{}).
		Where("model = ? AND provider = ?", model, provider).
		Update("additional_attributes", value)
	if res.Error != nil {
		return 0, s.parseGormError(res.Error)
	}
	return res.RowsAffected, nil
}

// DeleteModelPrices deletes all model pricing records from the database.
func (s *RDBConfigStore) DeleteModelPrices(ctx context.Context, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	return txDB.WithContext(ctx).Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&tables.TableModelPricing{}).Error
}

func (s *RDBConfigStore) GetPricingOverrides(ctx context.Context, filters PricingOverrideFilters) ([]tables.TablePricingOverride, error) {
	var overrides []tables.TablePricingOverride
	q := s.DB().WithContext(ctx).Model(&tables.TablePricingOverride{})
	if filters.ScopeKind != nil {
		q = q.Where("scope_kind = ?", *filters.ScopeKind)
	}
	if filters.UserID != nil {
		q = q.Where("user_id = ?", *filters.UserID)
	}
	if filters.VirtualKeyID != nil {
		q = q.Where("virtual_key_id = ?", *filters.VirtualKeyID)
	}
	if filters.ProviderID != nil {
		q = q.Where("provider_id = ?", *filters.ProviderID)
	}
	if filters.ProviderKeyID != nil {
		q = q.Where("provider_key_id = ?", *filters.ProviderKeyID)
	}
	if err := q.Order("created_at ASC").Find(&overrides).Error; err != nil {
		return nil, s.parseGormError(err)
	}
	return overrides, nil
}

func (s *RDBConfigStore) GetPricingOverridesPaginated(ctx context.Context, params PricingOverridesQueryParams) ([]tables.TablePricingOverride, int64, error) {
	baseQuery := s.DB().WithContext(ctx).Model(&tables.TablePricingOverride{})

	if params.Search != "" {
		search := "%" + strings.ToLower(params.Search) + "%"
		baseQuery = baseQuery.Where("LOWER(name) LIKE ?", search)
	}
	if params.ScopeKind != nil {
		baseQuery = baseQuery.Where("scope_kind = ?", *params.ScopeKind)
	}
	if params.UserID != nil {
		baseQuery = baseQuery.Where("user_id = ?", *params.UserID)
	}
	if params.VirtualKeyID != nil {
		baseQuery = baseQuery.Where("virtual_key_id = ?", *params.VirtualKeyID)
	}
	if params.ProviderID != nil {
		baseQuery = baseQuery.Where("provider_id = ?", *params.ProviderID)
	}
	if params.ProviderKeyID != nil {
		baseQuery = baseQuery.Where("provider_key_id = ?", *params.ProviderKeyID)
	}

	var totalCount int64
	if err := baseQuery.Count(&totalCount).Error; err != nil {
		return nil, 0, err
	}

	limit := params.Limit
	offset := params.Offset

	if limit <= 0 {
		limit = 25
	} else if limit > 100 {
		limit = 100
	}

	if offset < 0 {
		offset = 0
	}

	var overrides []tables.TablePricingOverride
	if err := baseQuery.
		Order("created_at ASC").
		Offset(offset).
		Limit(limit).
		Find(&overrides).Error; err != nil {
		return nil, 0, s.parseGormError(err)
	}
	return overrides, totalCount, nil
}

func (s *RDBConfigStore) GetPricingOverrideByID(ctx context.Context, id string) (*tables.TablePricingOverride, error) {
	var override tables.TablePricingOverride
	if err := s.DB().WithContext(ctx).First(&override, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, s.parseGormError(err)
	}
	return &override, nil
}

func (s *RDBConfigStore) CreatePricingOverride(ctx context.Context, override *tables.TablePricingOverride, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	if err := txDB.WithContext(ctx).Create(override).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

func (s *RDBConfigStore) UpdatePricingOverride(ctx context.Context, override *tables.TablePricingOverride, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	// created_at is immutable: Save writes every column, so a caller passing a row it
	// didn't read from the DB (e.g. a config.json reload) would otherwise zero it out.
	// A missing row is not an error here — this also serves create-or-update callers,
	// so fall through to Save and let it insert.
	if override.ID != "" {
		var existing tables.TablePricingOverride
		if err := txDB.WithContext(ctx).Select("created_at").First(&existing, "id = ?", override.ID).Error; err == nil {
			override.CreatedAt = existing.CreatedAt
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
	}
	if err := txDB.WithContext(ctx).Save(override).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

func (s *RDBConfigStore) DeletePricingOverride(ctx context.Context, id string, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	res := txDB.WithContext(ctx).Delete(&tables.TablePricingOverride{}, "id = ?", id)
	if res.Error != nil {
		return s.parseGormError(res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// DATASHEET OVERRIDES METHODS

// GetDatasheetOverrides returns every datasheet override, oldest first so
// application order is stable across nodes.
func (s *RDBConfigStore) GetDatasheetOverrides(ctx context.Context) ([]tables.TableDatasheetOverride, error) {
	var rows []tables.TableDatasheetOverride
	if err := s.DB().WithContext(ctx).Order("created_at ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// GetDatasheetOverrideByID returns one datasheet override or ErrNotFound.
func (s *RDBConfigStore) GetDatasheetOverrideByID(ctx context.Context, id string) (*tables.TableDatasheetOverride, error) {
	var row tables.TableDatasheetOverride
	if err := s.DB().WithContext(ctx).First(&row, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &row, nil
}

// UpsertDatasheetOverride writes the override for its (provider, model, mode)
// target. An existing row for the same target keeps its ID and created_at, so
// a client saving by target never forks a second row. The write is a single
// ON CONFLICT statement on the target index, so nodes saving the same new
// target at once converge on one row instead of one hitting the index; the
// stored ID and created_at are then read back into override. Without a
// caller-supplied tx both statements run in one transaction, so an error from
// the readback (a cancelled ctx, say) never leaves the write committed.
func (s *RDBConfigStore) UpsertDatasheetOverride(ctx context.Context, override *tables.TableDatasheetOverride, tx ...*gorm.DB) error {
	if len(tx) > 0 {
		return s.upsertDatasheetOverride(tx[0].WithContext(ctx), override)
	}
	return s.DB().WithContext(ctx).Transaction(func(db *gorm.DB) error {
		return s.upsertDatasheetOverride(db, override)
	})
}

// upsertDatasheetOverride runs the target upsert and ID readback on db.
func (s *RDBConfigStore) upsertDatasheetOverride(db *gorm.DB, override *tables.TableDatasheetOverride) error {
	now := time.Now()
	if override.CreatedAt.IsZero() {
		override.CreatedAt = now
	}
	override.UpdatedAt = now
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "provider"}, {Name: "model"}, {Name: "mode"}},
		DoUpdates: clause.AssignmentColumns([]string{"is_custom", "pricing_patch_json", "params_patch_json", "baseline_json", "config_hash", "pricing_claim", "updated_at"}),
	}).Create(override).Error; err != nil {
		if isPricingClaimViolation(err) {
			return ErrDatasheetPricingClaimTaken
		}
		return s.parseGormError(err)
	}
	var stored tables.TableDatasheetOverride
	if err := db.Select("id", "created_at").
		Where("provider = ? AND model = ? AND mode = ?", override.Provider, override.Model, override.Mode).
		First(&stored).Error; err != nil {
		return s.parseGormError(err)
	}
	override.ID = stored.ID
	override.CreatedAt = stored.CreatedAt
	return nil
}

// isPricingClaimViolation reports whether err is the unique index on
// pricing_claim rejecting a second override for the same folded pricing row
// (SQLite names the column, Postgres the index).
func isPricingClaimViolation(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "governance_datasheet_overrides.pricing_claim") || strings.Contains(msg, "idx_datasheet_override_pricing_claim")
}

// DeleteDatasheetOverride removes a datasheet override by ID.
func (s *RDBConfigStore) DeleteDatasheetOverride(ctx context.Context, id string, tx ...*gorm.DB) error {
	txDB := s.DB()
	if len(tx) > 0 {
		txDB = tx[0]
	}
	res := txDB.WithContext(ctx).Delete(&tables.TableDatasheetOverride{}, "id = ?", id)
	if res.Error != nil {
		return s.parseGormError(res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// MODEL PARAMETERS METHODS

// GetModelParameters returns all stored model parameter rows.
func (s *RDBConfigStore) GetModelParameters(ctx context.Context) ([]tables.TableModelParameters, error) {
	var rows []tables.TableModelParameters
	if err := s.DB().WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// GetModelParametersByModel retrieves model parameters for a specific model.
func (s *RDBConfigStore) GetModelParametersByModel(ctx context.Context, model string) (*tables.TableModelParameters, error) {
	var params tables.TableModelParameters
	if err := s.DB().WithContext(ctx).Where("model = ?", model).First(&params).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &params, nil
}

// UpsertModelParameters inserts or updates model parameters for a specific model.
// Uses a single atomic ON CONFLICT statement to avoid deadlocks in multinode deployments
// where multiple nodes may attempt concurrent upserts for the same model on startup.
func (s *RDBConfigStore) UpsertModelParameters(ctx context.Context, params *tables.TableModelParameters, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	db := txDB.WithContext(ctx)

	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "model"}},
		UpdateAll: true,
	}).Create(params).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

const modelParametersUpsertBatchSize = 100

// UpsertModelParametersBatch inserts or updates model parameters in batches.
// The sync path uses this to avoid one DB round-trip per model parameter row.
func (s *RDBConfigStore) UpsertModelParametersBatch(ctx context.Context, params []tables.TableModelParameters, tx ...*gorm.DB) error {
	if len(params) == 0 {
		return nil
	}
	deduped := make([]tables.TableModelParameters, 0, len(params))
	seen := make(map[string]int, len(params))
	for _, param := range params {
		if idx, ok := seen[param.Model]; ok {
			deduped[idx] = param
			continue
		}
		seen[param.Model] = len(deduped)
		deduped = append(deduped, param)
	}
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	db := txDB.WithContext(ctx)

	onConflict := clause.OnConflict{
		Columns:   []clause.Column{{Name: "model"}},
		UpdateAll: true,
	}
	upsert := func(tx *gorm.DB) error {
		// Unlike TableModelPricing, TableModelParameters has no nullable default
		// columns, so GORM's multi-row INSERT does not emit DEFAULT values that
		// SQLite rejects.
		if err := tx.Clauses(onConflict).CreateInBatches(deduped, modelParametersUpsertBatchSize).Error; err != nil {
			return s.parseGormError(err)
		}
		return nil
	}
	if len(tx) > 0 {
		return upsert(db)
	}
	return db.Transaction(upsert)
}

// PLUGINS METHODS

func (s *RDBConfigStore) GetPlugins(ctx context.Context) ([]*tables.TablePlugin, error) {
	var plugins []*tables.TablePlugin
	if err := s.DB().WithContext(ctx).Find(&plugins).Error; err != nil {
		return nil, err
	}
	return plugins, nil
}

func (s *RDBConfigStore) GetPlugin(ctx context.Context, name string) (*tables.TablePlugin, error) {
	var plugin tables.TablePlugin
	if err := s.DB().WithContext(ctx).First(&plugin, "name = ?", name).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &plugin, nil
}

// CreatePlugin creates a new plugin in the database.
func (s *RDBConfigStore) CreatePlugin(ctx context.Context, plugin *tables.TablePlugin, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	// Mark plugin as custom if path is not empty
	if plugin.Path != nil && strings.TrimSpace(*plugin.Path) != "" {
		plugin.IsCustom = true
	} else {
		plugin.IsCustom = false
	}
	if err := txDB.WithContext(ctx).Create(plugin).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// UpsertPlugin creates a new plugin in the database if it doesn't exist, otherwise updates it.
func (s *RDBConfigStore) UpsertPlugin(ctx context.Context, plugin *tables.TablePlugin, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	// Mark plugin as custom if path is not empty
	if plugin.Path != nil && strings.TrimSpace(*plugin.Path) != "" {
		plugin.IsCustom = true
	} else {
		plugin.IsCustom = false
	}
	// Upsert plugin (create or update if exists based on unique name)
	if err := txDB.WithContext(ctx).Clauses(
		clause.OnConflict{
			Columns:   []clause.Column{{Name: "name"}},
			UpdateAll: true,
		},
	).Create(plugin).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// UpdatePlugin updates an existing plugin in the database.
func (s *RDBConfigStore) UpdatePlugin(ctx context.Context, plugin *tables.TablePlugin, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	var localTx bool

	if len(tx) > 0 {
		txDB = tx[0]
		localTx = false
	} else {
		txDB = s.DB().Begin()
		localTx = true
	}
	// Mark plugin as custom if path is not empty
	if plugin.Path != nil && strings.TrimSpace(*plugin.Path) != "" {
		plugin.IsCustom = true
	} else {
		plugin.IsCustom = false
	}
	// Lock the row before reading it: everything preserved below is a read-modify-write
	// feeding a delete-and-recreate, so two concurrent edits would otherwise both read the
	// pre-state and the second would drop the first's fields, or race on the unique name.
	var existing tables.TablePlugin
	if err := dbForUpdate(txDB.WithContext(ctx)).Where("name = ?", plugin.Name).First(&existing).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			if localTx {
				txDB.Rollback()
			}
			return err
		}
		// not found — nothing to delete
	} else {
		// Preserve the config.json hash and version across this delete-and-recreate: callers
		// that do not set them (UI/API edits) would otherwise clear them, making the next
		// startup read the file as changed and revert the edit.
		if plugin.ConfigHash == "" {
			plugin.ConfigHash = existing.ConfigHash
		}
		// Placement and order are DB-only - the UI has no control over them and config.json
		// is documented as ignoring them - so a caller that sends neither is not asking to
		// clear them.
		if plugin.Placement == nil {
			plugin.Placement = existing.Placement
		}
		if plugin.Order == nil {
			plugin.Order = existing.Order
		}
		if plugin.Version == 0 {
			plugin.Version = existing.Version
		}
		plugin.CreatedAt = existing.CreatedAt
		if err := txDB.WithContext(ctx).Delete(&existing).Error; err != nil {
			if localTx {
				txDB.Rollback()
			}
			return err
		}
	}
	if err := txDB.WithContext(ctx).Create(plugin).Error; err != nil {
		if localTx {
			txDB.Rollback()
		}
		return s.parseGormError(err)
	}
	if localTx {
		return txDB.Commit().Error
	}
	return nil
}

// DeletePlugin deletes a plugin from the database.
func (s *RDBConfigStore) DeletePlugin(ctx context.Context, name string, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	var plugin tables.TablePlugin
	if err := txDB.WithContext(ctx).Where("name = ?", name).First(&plugin).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	return txDB.WithContext(ctx).Delete(&plugin).Error
}

// GOVERNANCE METHODS

// GetRedactedVirtualKeys retrieves redacted virtual keys from the database. Reads
// go through ScopedDB, so a QueryScope on ctx limits the result exactly as it
// does for GetVirtualKey; callers that resolve VK names through this method must
// not see keys outside the caller's scope.
func (s *RDBConfigStore) GetRedactedVirtualKeys(ctx context.Context, ids []string) ([]tables.TableVirtualKey, error) {
	var virtualKeys []tables.TableVirtualKey

	if len(ids) > 0 {
		// One bound parameter for the whole list: ids can hold enough VKs to
		// exceed the bind-parameter limit at scale.
		db := s.ScopedDB(ctx)
		err := db.Select("id, name, description, is_active").Where(queryscope.InStrings(db, "governance_virtual_keys.id", ids)).Find(&virtualKeys).Error
		if err != nil {
			return nil, err
		}
	} else {
		err := s.ScopedDB(ctx).Select("id, name, description, is_active").Find(&virtualKeys).Error
		if err != nil {
			return nil, err
		}
	}
	return virtualKeys, nil
}

// GetRedactedTeams returns the id and name of each team in ids, honouring any
// QueryScope on ctx. An empty ids returns nothing (never every team).
func (s *RDBConfigStore) GetRedactedTeams(ctx context.Context, ids []string) ([]tables.TableTeam, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var teams []tables.TableTeam
	db := s.ScopedDB(ctx)
	if err := db.Select("id, name").Where(queryscope.InStrings(db, "governance_teams.id", ids)).Find(&teams).Error; err != nil {
		return nil, err
	}
	return teams, nil
}

// GetRedactedCustomers returns the id and name of each customer in ids,
// honouring any QueryScope on ctx. An empty ids returns nothing.
func (s *RDBConfigStore) GetRedactedCustomers(ctx context.Context, ids []string) ([]tables.TableCustomer, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var customers []tables.TableCustomer
	db := s.ScopedDB(ctx)
	if err := db.Select("id, name").Where(queryscope.InStrings(db, "governance_customers.id", ids)).Find(&customers).Error; err != nil {
		return nil, err
	}
	return customers, nil
}

// preloadCustomerRelations preloads the customer relations for a virtual key.
func preloadCustomerRelations(db *gorm.DB, prefix string) *gorm.DB {
	relation := func(name string) string {
		if prefix == "" {
			return name
		}
		return prefix + name
	}
	return preloadCustomerRelationsWithoutVirtualKeys(db, prefix).
		Preload(relation("VirtualKeys"))
}

// preloadCustomerRelationsWithoutVirtualKeys preloads every customer relation
// except VirtualKeys. The paginated list path uses this and reports
// VirtualKeyCount instead, so a customer with thousands of keys doesn't drag
// them all into a page of 25 rows.
func preloadCustomerRelationsWithoutVirtualKeys(db *gorm.DB, prefix string) *gorm.DB {
	relation := func(name string) string {
		if prefix == "" {
			return name
		}
		return prefix + name
	}
	return db.
		Preload(relation("Teams")).
		Preload(relation("Teams.Budgets")).
		Preload(relation("Budgets")).
		Preload(relation("RateLimit"))
}

// attachCustomerVirtualKeyCounts populates VirtualKeyCount on each customer
// using a single grouped count, avoiding an N+1 over the customers in a page.
// The customers passed in have already been narrowed by any QueryScope on ctx,
// so counting their keys unscoped exposes nothing the caller can't already see.
func (s *RDBConfigStore) attachCustomerVirtualKeyCounts(ctx context.Context, customers []tables.TableCustomer) error {
	if len(customers) == 0 {
		return nil
	}
	ids := make([]string, 0, len(customers))
	for i := range customers {
		ids = append(ids, customers[i].ID)
	}
	var rows []struct {
		CustomerID string
		Count      int
	}
	if err := s.DB().WithContext(ctx).
		Model(&tables.TableVirtualKey{}).
		Select("customer_id, COUNT(*) AS count").
		Where("customer_id IN ?", ids).
		Group("customer_id").
		Scan(&rows).Error; err != nil {
		return err
	}
	countByCustomer := make(map[string]int, len(rows))
	for _, row := range rows {
		countByCustomer[row.CustomerID] = row.Count
	}
	for i := range customers {
		customers[i].VirtualKeyCount = countByCustomer[customers[i].ID]
	}
	return nil
}

// attachCustomerTeamCounts sets TeamCount on each customer with one grouped COUNT,
// so list responses report the count without loading the teams themselves.
func (s *RDBConfigStore) attachCustomerTeamCounts(ctx context.Context, customers []tables.TableCustomer) error {
	if len(customers) == 0 {
		return nil
	}
	ids := make([]string, 0, len(customers))
	for i := range customers {
		ids = append(ids, customers[i].ID)
	}
	var rows []struct {
		CustomerID string
		Count      int
	}
	if err := s.DB().WithContext(ctx).
		Model(&tables.TableTeam{}).
		Select("customer_id, COUNT(*) AS count").
		Where("customer_id IN ?", ids).
		Group("customer_id").
		Scan(&rows).Error; err != nil {
		return err
	}
	countByCustomer := make(map[string]int, len(rows))
	for _, row := range rows {
		countByCustomer[row.CustomerID] = row.Count
	}
	for i := range customers {
		customers[i].TeamCount = countByCustomer[customers[i].ID]
	}
	return nil
}

// preloadVirtualKeyBaseRelations preloads the base relationships for a virtual key.
func preloadVirtualKeyBaseRelations(db *gorm.DB) *gorm.DB {
	return db.
		Preload("Team").
		Preload("Team.Customer").
		Preload("Customer").
		Preload("Budgets").
		Preload("RateLimit").
		Preload("ProviderConfigs").
		Preload("ProviderConfigs.Budgets").
		Preload("ProviderConfigs.RateLimit").
		Preload("ProviderConfigs.Keys", func(db *gorm.DB) *gorm.DB {
			return db.Select("id, name, key_id, models_json, provider")
		})
}

// preloadVirtualKeyDetailRelations preloads the detail relationships for a virtual key.
func preloadVirtualKeyDetailRelations(db *gorm.DB) *gorm.DB {
	return preloadCustomerRelations(preloadVirtualKeyBaseRelations(db), "Customer.")
}

// virtualKeyInternalPageSize is the bounded page size used when loading every
// virtual key with preloaded relationships. Keeping each page small avoids
// PostgreSQL's extended protocol parameter limit during GORM preloads.
const virtualKeyInternalPageSize = 1000

// modelConfigInternalPageSize bounds the page size when loading every model config
// with preloaded relationships, for the same reason as virtualKeyInternalPageSize:
// a single un-paginated Find with preloads generates an IN(...) clause with one
// bind parameter per row and exceeds PostgreSQL's 65535-parameter limit at scale.
const modelConfigInternalPageSize = 1000

// ListExpiredVirtualKeysForDeletion returns the keys the daily cleanup job may delete:
// expires_at has passed and delete_after_expire is true, or unset when the client-wide
// default (includeUnset) says expired keys are deleted. Only the columns the job needs
// are selected; it re-fetches each key before deleting it.
func (s *RDBConfigStore) ListExpiredVirtualKeysForDeletion(ctx context.Context, now time.Time, includeUnset bool) ([]tables.TableVirtualKey, error) {
	var keys []tables.TableVirtualKey
	query := s.DB().WithContext(ctx).
		Select("id", "name", "expires_at", "delete_after_expire").
		Where("expires_at IS NOT NULL AND expires_at <= ?", now.UTC())
	if includeUnset {
		query = query.Where("delete_after_expire = ? OR delete_after_expire IS NULL", true)
	} else {
		query = query.Where("delete_after_expire = ?", true)
	}
	err := query.
		Order("expires_at ASC, id ASC").
		Find(&keys).Error
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// DeleteExpiredVirtualKey deletes the key only if it is still expired and eligible
// under the row lock, returning the deleted row, or nil when it no longer qualifies.
// A key without its own flag is deleted only if includeUnset and the client default
// read inside the transaction both allow it.
func (s *RDBConfigStore) DeleteExpiredVirtualKey(ctx context.Context, id string, now time.Time, includeUnset bool) (*tables.TableVirtualKey, error) {
	var deleted *tables.TableVirtualKey
	err := s.DB().WithContext(ctx).Transaction(func(txDB *gorm.DB) error {
		var vk tables.TableVirtualKey
		if err := dbForUpdate(txDB.WithContext(ctx)).First(&vk, "id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if vk.DeleteAfterExpire == nil && includeUnset {
			// The job scanned with the default on; an admin may have turned it off since.
			var clientCfg tables.TableClientConfig
			err := txDB.WithContext(ctx).Select("delete_expired_virtual_keys").First(&clientCfg).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			includeUnset = err == nil && clientCfg.DeleteExpiredVirtualKeys
		}
		if !vk.IsExpiredAt(now) || !vk.DeletesAfterExpire(includeUnset) {
			return nil
		}
		if err := s.DeleteVirtualKey(ctx, id, txDB); err != nil {
			return err
		}
		deleted = &vk
		return nil
	})
	if err != nil {
		return nil, err
	}
	return deleted, nil
}

// GetVirtualKeys retrieves all virtual keys from the database.
func (s *RDBConfigStore) GetVirtualKeys(ctx context.Context) ([]tables.TableVirtualKey, error) {
	var allVirtualKeys []tables.TableVirtualKey
	var lastCreatedAt time.Time
	var lastID string
	hasCursor := false

	start := time.Now()
	pageCount := 0
	defer func() {
		if s.logger != nil {
			s.logger.Info("[startup-timing] GetVirtualKeys loaded %d keys across %d pages in %v", len(allVirtualKeys), pageCount, time.Since(start))
		}
	}()

	for {
		virtualKeys, err := s.getVirtualKeysPage(ctx, virtualKeyInternalPageSize, lastCreatedAt, lastID, hasCursor)
		if err != nil {
			return nil, err
		}
		pageCount++
		if len(virtualKeys) == 0 {
			return allVirtualKeys, nil
		}

		allVirtualKeys = append(allVirtualKeys, virtualKeys...)
		last := virtualKeys[len(virtualKeys)-1]
		lastCreatedAt = last.CreatedAt
		lastID = last.ID
		hasCursor = true
		if len(virtualKeys) < virtualKeyInternalPageSize {
			return allVirtualKeys, nil
		}
	}
}

// virtualKeyKeysetCondition is the keyset predicate the all-VK loaders page by.
// The row comparison is equivalent to "created_at > ? OR (created_at = ? AND
// id > ?)", but Postgres turns it into one index condition on
// idx_virtual_keys_created_at_id and starts the scan at the cursor; the OR form
// can only be a filter, so every page re-read the index from the beginning.
// SQLite supports row values since 3.15.
const virtualKeyKeysetCondition = "(governance_virtual_keys.created_at, governance_virtual_keys.id) > (?, ?)"

// getVirtualKeysPage retrieves one unfiltered page of virtual keys without a
// COUNT query for internal all-key loading paths.
func (s *RDBConfigStore) getVirtualKeysPage(ctx context.Context, limit int, lastCreatedAt time.Time, lastID string, hasCursor bool) ([]tables.TableVirtualKey, error) {
	var virtualKeys []tables.TableVirtualKey
	query := preloadVirtualKeyBaseRelations(s.ScopedDB(ctx))
	if hasCursor {
		query = query.Where(virtualKeyKeysetCondition, lastCreatedAt, lastID)
	}
	if err := query.
		Order("governance_virtual_keys.created_at ASC, governance_virtual_keys.id ASC").
		Limit(limit).
		Find(&virtualKeys).Error; err != nil {
		return nil, err
	}
	return virtualKeys, nil
}

// getGovernanceConfigVirtualKeys loads every virtual key with the preloads needed
// by GetGovernanceConfig (ProviderConfigs and their Keys). It pages with a
// cursor so each preload's generated IN(...) clause stays within PostgreSQL's
// 65535 bind-parameter limit. A single un-paginated Find with these preloads
// fails once the key count exceeds ~65535 ("extended protocol limited to 65535
// parameters"). Mirrors GetVirtualKeys, which is paginated for the same reason.
func (s *RDBConfigStore) getGovernanceConfigVirtualKeys(ctx context.Context) ([]tables.TableVirtualKey, error) {
	var allVirtualKeys []tables.TableVirtualKey
	var lastCreatedAt time.Time
	var lastID string
	hasCursor := false

	for {
		var page []tables.TableVirtualKey
		query := s.DB().WithContext(ctx).
			Preload("ProviderConfigs").
			Preload("ProviderConfigs.Keys", func(db *gorm.DB) *gorm.DB {
				return db.Select("id, name, key_id, models_json, provider")
			})
		if hasCursor {
			query = query.Where(virtualKeyKeysetCondition, lastCreatedAt, lastID)
		}
		if err := query.
			Order("governance_virtual_keys.created_at ASC, governance_virtual_keys.id ASC").
			Limit(virtualKeyInternalPageSize).
			Find(&page).Error; err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return allVirtualKeys, nil
		}
		allVirtualKeys = append(allVirtualKeys, page...)
		last := page[len(page)-1]
		lastCreatedAt = last.CreatedAt
		lastID = last.ID
		hasCursor = true
		if len(page) < virtualKeyInternalPageSize {
			return allVirtualKeys, nil
		}
	}
}

// VirtualKeySearchTerm normalizes a raw search string into the LIKE pattern the
// virtual key search clauses match against. Exported so downstream stores build
// their extra clauses against exactly the same pattern.
func VirtualKeySearchTerm(search string) string {
	return "%" + strings.ToLower(search) + "%"
}

// VirtualKeySearchConditions builds the OR group that a virtual key search
// narrows on: the key's own name, its team's name, or its customer's name - i.e.
// everything the "Assigned To" column can display in OSS, so anything visible on
// screen is also findable - plus the key's metadata (keys and values, matched as
// a substring of the stored JSON).
//
// It returns the condition rather than applying it so downstream stores can OR in
// clauses of their own (enterprise adds the assigned user, whose link table it
// alone knows about) without restating these three.
//
// Subqueries rather than joins: a join against teams/customers would multiply
// rows and collide with the budget_spent sort join. The subqueries go through
// Model() so GORM's soft-delete scope applies and a deleted team cannot
// resurrect its keys into the results.
func VirtualKeySearchConditions(db *gorm.DB, search string) *gorm.DB {
	term := VirtualKeySearchTerm(search)
	teamIDs := db.Model(&tables.TableTeam{}).
		Select("id").
		Where("LOWER(name) LIKE ?", term)
	customerIDs := db.Model(&tables.TableCustomer{}).
		Select("id").
		Where("LOWER(name) LIKE ?", term)
	return db.Where("LOWER(governance_virtual_keys.name) LIKE ?", term).
		Or("governance_virtual_keys.team_id IN (?)", teamIDs).
		Or("governance_virtual_keys.customer_id IN (?)", customerIDs).
		Or(`LOWER(governance_virtual_keys.metadata) LIKE ? ESCAPE '\'`, virtualKeyMetadataSearchTerm(search))
}

// virtualKeyMetadataLikeEscaper escapes LIKE metacharacters for an ESCAPE '\' clause.
var virtualKeyMetadataLikeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// virtualKeyMetadataSearchTerm is the LIKE pattern for searching the stored metadata JSON, used
// with ESCAPE '\'. The column holds encoding/json output, which escapes &, <, > (and " and \)
// inside values, so the raw term would miss a value such as "AT&T": the term is encoded the same
// way first. Those encodings contain backslashes, which PostgreSQL's LIKE treats as its default
// escape character, so the backslashes (and any % or _ the user typed) are escaped explicitly and
// match literally on every dialect.
func virtualKeyMetadataSearchTerm(search string) string {
	term := strings.ToLower(search)
	if encoded, err := json.Marshal(term); err == nil {
		term = strings.TrimSuffix(strings.TrimPrefix(string(encoded), `"`), `"`)
	}
	return "%" + virtualKeyMetadataLikeEscaper.Replace(term) + "%"
}

// applyVirtualKeyMetadataFilters narrows q to keys whose metadata carries every
// key/value pair in filters. A key that fails the metadata key rule matches
// nothing rather than being dropped, so a bad filter can never widen the result.
// The key is spliced into the SQLite JSON path, which is safe only because the
// rule restricts it to [a-zA-Z0-9._-].
func applyVirtualKeyMetadataFilters(q *gorm.DB, dialect string, filters map[string]string) (*gorm.DB, error) {
	if len(filters) == 0 {
		return q, nil
	}
	q = q.Where("governance_virtual_keys.metadata IS NOT NULL")
	for key, value := range filters {
		if !tables.IsValidVirtualKeyMetadataKey(key) {
			return q.Where("1 = 0"), nil
		}
		switch dialect {
		case "postgres":
			// Containment on the JSON document; the fragment is marshalled rather than
			// formatted so the value is escaped exactly as JSON requires.
			fragment, err := json.Marshal(map[string]string{key: value})
			if err != nil {
				return nil, err
			}
			q = q.Where("governance_virtual_keys.metadata::jsonb @> ?::jsonb", string(fragment))
		default:
			// Quote the member name so dots and hyphens stay part of the key.
			q = q.Where("json_extract(governance_virtual_keys.metadata, ?) = ?", `$."`+key+`"`, value)
		}
	}
	return q, nil
}

// GetVirtualKeysPaginated retrieves virtual keys with pagination, filtering, and search support.
func (s *RDBConfigStore) GetVirtualKeysPaginated(ctx context.Context, params VirtualKeyQueryParams) ([]tables.TableVirtualKey, int64, error) {
	// Apply pagination defaults
	limit := params.Limit
	if params.Export {
		// Export mode: allow large fetches, cap at 10000 as a safety net
		if limit <= 0 {
			limit = 10000
		}
		if limit > 10000 {
			limit = 10000
		}
	} else {
		if limit <= 0 {
			limit = 25
		}
		if limit > 100 {
			limit = 100
		}
	}

	offset := params.Offset
	if offset < 0 {
		offset = 0
	}

	// ScopedDB applies any caller-supplied row visibility before
	// per-call filters so the total count and the page result agree
	// on what the caller is allowed to see.
	if limit <= virtualKeyInternalPageSize {
		return s.getVirtualKeysWindow(ctx, s.ScopedDB(ctx), params, limit, offset)
	}

	// A window larger than one chunk is loaded by several statements. Run the count
	// and every chunk in one read-only transaction so they all see the same snapshot:
	// otherwise a key created or deleted between chunks shifts the offsets, repeating
	// or skipping a row, and the total disagrees with the rows returned. Postgres needs
	// REPEATABLE READ for that (READ COMMITTED snapshots per statement); a SQLite read
	// transaction already holds one snapshot, and its drivers reject isolation levels.
	var txOpts []*sql.TxOptions
	if s.DB().Dialector.Name() == "postgres" {
		txOpts = append(txOpts, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	}
	var virtualKeys []tables.TableVirtualKey
	var totalCount int64
	err := s.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		virtualKeys, totalCount, err = s.getVirtualKeysWindow(ctx, s.ScopedDB(ctx, tx), params, limit, offset)
		return err
	}, txOpts...)
	if err != nil {
		return nil, 0, err
	}
	return virtualKeys, totalCount, nil
}

// getVirtualKeysWindow counts the virtual keys matching params on db and loads the
// window [offset, offset+limit) in chunks of at most virtualKeyInternalPageSize.
func (s *RDBConfigStore) getVirtualKeysWindow(ctx context.Context, db *gorm.DB, params VirtualKeyQueryParams, limit, offset int) ([]tables.TableVirtualKey, int64, error) {
	// Build base query with filters
	baseQuery := db.Model(&tables.TableVirtualKey{})

	// A virtual key is assigned to at most one of customer / team / user, so
	// combining assignment filters ORs them rather than narrowing to nothing.
	// UserID has no meaning in the OSS build (the VK↔user link lives in an
	// enterprise table), so it fails closed instead of silently widening the
	// result set; the enterprise store overrides this method to honour it.
	var assignmentClauses []string
	var assignmentArgs []interface{}
	if params.CustomerID != "" {
		assignmentClauses = append(assignmentClauses, "customer_id = ?")
		assignmentArgs = append(assignmentArgs, params.CustomerID)
	}
	if params.TeamID != "" {
		assignmentClauses = append(assignmentClauses, "team_id = ?")
		assignmentArgs = append(assignmentArgs, params.TeamID)
	}
	if params.BusinessUnitID != "" {
		assignmentClauses = append(assignmentClauses, "business_unit_id = ?")
		assignmentArgs = append(assignmentArgs, params.BusinessUnitID)
	}
	if params.UserID != "" {
		assignmentClauses = append(assignmentClauses, "1 = 0")
	}
	if len(assignmentClauses) > 0 {
		baseQuery = baseQuery.Where("("+strings.Join(assignmentClauses, " OR ")+")", assignmentArgs...)
	}
	if params.Search != "" {
		baseQuery = baseQuery.Where(VirtualKeySearchConditions(s.DB().WithContext(ctx), params.Search))
	}
	baseQuery, err := applyVirtualKeyMetadataFilters(baseQuery, s.DB().Dialector.Name(), params.MetadataFilters)
	if err != nil {
		return nil, 0, err
	}

	// Get total count before pagination
	var totalCount int64
	if err := baseQuery.Count(&totalCount).Error; err != nil {
		return nil, 0, err
	}

	// Determine sort order
	orderClause := "governance_virtual_keys.created_at ASC, governance_virtual_keys.id ASC"
	if params.SortBy != "" {
		dir := "ASC"
		if strings.EqualFold(params.Order, "desc") {
			dir = "DESC"
		}
		switch params.SortBy {
		case "name":
			orderClause = fmt.Sprintf("governance_virtual_keys.name %s, governance_virtual_keys.id ASC", dir)
		case "budget_spent":
			orderClause = fmt.Sprintf("COALESCE(vk_budget_totals.total_usage, 0) %s, governance_virtual_keys.id ASC", dir)
		case "created_at":
			orderClause = fmt.Sprintf("governance_virtual_keys.created_at %s, governance_virtual_keys.id ASC", dir)
		case "status":
			orderClause = fmt.Sprintf("governance_virtual_keys.is_active %s, governance_virtual_keys.id ASC", dir)
		}
	}

	// Fetch with preloads and pagination
	query := preloadVirtualKeyBaseRelations(baseQuery)
	if params.SortBy == "budget_spent" {
		// A virtual key can have multiple budgets (different reset intervals); take MAX so the
		// highest-spending budget drives the sort without duplicating rows.
		query = query.Joins(`LEFT JOIN (
			SELECT virtual_key_id, MAX(current_usage) AS total_usage
			FROM governance_budgets
			WHERE virtual_key_id IS NOT NULL
			GROUP BY virtual_key_id
		) AS vk_budget_totals ON vk_budget_totals.virtual_key_id = governance_virtual_keys.id`)
	}
	// Load the requested window in chunks of at most virtualKeyInternalPageSize.
	// Each preload (ProviderConfigs.Budgets, ProviderConfigs.Keys, ...) binds one
	// parameter per parent row, so a 10,000-key export page with a handful of
	// provider configs per key would exceed the bind-parameter limit in a single
	// Find. Every order clause ends in the unique id, so consecutive offset chunks
	// concatenate to exactly the rows one Find would have returned.
	var virtualKeys []tables.TableVirtualKey
	for loaded := 0; loaded < limit; {
		chunkSize := min(limit-loaded, virtualKeyInternalPageSize)
		var chunk []tables.TableVirtualKey
		if err := query.Session(&gorm.Session{}).
			Order(orderClause).
			Offset(offset + loaded).
			Limit(chunkSize).
			Find(&chunk).Error; err != nil {
			return nil, 0, err
		}
		virtualKeys = append(virtualKeys, chunk...)
		loaded += len(chunk)
		if len(chunk) < chunkSize {
			break
		}
	}
	return virtualKeys, totalCount, nil
}

// GetVirtualKey retrieves a virtual key from the database.
//
// When ctx carries a QueryScope, the query is narrowed to rows the
// caller is allowed to see. A row that exists but falls outside the
// scope returns ErrNotFound, the same response a genuinely-missing
// row produces, so URL guessing cannot distinguish "hidden" from
// "absent".
func (s *RDBConfigStore) GetVirtualKey(ctx context.Context, id string) (*tables.TableVirtualKey, error) {
	var virtualKey tables.TableVirtualKey
	q := preloadVirtualKeyDetailRelations(s.ScopedDB(ctx))
	if err := q.First(&virtualKey, "governance_virtual_keys.id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &virtualKey, nil
}

// GetVirtualKeysByIDs loads the virtual keys in ids with the same relations
// GetVirtualKey loads, in bounded chunks. An id with no row is simply absent;
// an empty ids returns nothing.
func (s *RDBConfigStore) GetVirtualKeysByIDs(ctx context.Context, ids []string) ([]tables.TableVirtualKey, error) {
	var virtualKeys []tables.TableVirtualKey
	// Each preload binds one parameter per parent row, so the keys load in chunks
	// the same size as the export path's.
	for start := 0; start < len(ids); start += virtualKeyInternalPageSize {
		end := min(start+virtualKeyInternalPageSize, len(ids))
		db := s.ScopedDB(ctx)
		var chunk []tables.TableVirtualKey
		if err := preloadVirtualKeyDetailRelations(db).
			Where(queryscope.InStrings(db, "governance_virtual_keys.id", ids[start:end])).
			Find(&chunk).Error; err != nil {
			return nil, err
		}
		virtualKeys = append(virtualKeys, chunk...)
	}
	return virtualKeys, nil
}

// findVirtualKeyByValue resolves a virtual key from a presented value: the
// current value hash first, then a rotation-grace previous value hash that is
// still inside its window, then the plaintext fallback for rows not yet
// migrated to hashed lookups.
func (s *RDBConfigStore) findVirtualKeyByValue(baseQuery *gorm.DB, value string) (*tables.TableVirtualKey, error) {
	valueHash := encrypt.HashSHA256(value)
	var virtualKey tables.TableVirtualKey
	err := baseQuery.Session(&gorm.Session{}).Where("value_hash = ?", valueHash).First(&virtualKey).Error
	if err == nil {
		return &virtualKey, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	// Rotation grace period: a retired value keeps resolving until its expiry.
	// Querying current hashes first guarantees a live value always beats
	// another key's retired value. The window check stays in Go so the
	// exclusive boundary has a single definition (HasActivePreviousValue)
	// across SQL dialects and the in-memory governance store.
	var graceKey tables.TableVirtualKey
	graceErr := baseQuery.Session(&gorm.Session{}).
		Where("previous_value_hash = ?", valueHash).
		Order("previous_value_expires_at DESC").
		First(&graceKey).Error
	if graceErr == nil {
		if graceKey.HasActivePreviousValue(time.Now().UTC()) {
			return &graceKey, nil
		}
	} else if !errors.Is(graceErr, gorm.ErrRecordNotFound) {
		return nil, graceErr
	}
	if schemas.IsSecretRef(value) {
		return nil, ErrNotFound
	}
	// Fallback: plaintext lookup for rows not yet migrated. Only rows still stored in plaintext
	// qualify; an encrypted row's value column holds ciphertext, which is not the key.
	if err := baseQuery.Session(&gorm.Session{}).Where(plaintextRowPredicate).Where("value = ?", value).First(&virtualKey).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &virtualKey, nil
}

// GetVirtualKeyByValue retrieves a virtual key by its value using hash-based lookup.
func (s *RDBConfigStore) GetVirtualKeyByValue(ctx context.Context, value string) (*tables.TableVirtualKey, error) {
	return s.findVirtualKeyByValue(preloadVirtualKeyBaseRelations(s.DB().WithContext(ctx)), value)
}

// GetVirtualKeyQuotaByValue retrieves budget, rate limit, and provider-level limit data for a virtual key.
// This is a lean query that avoids loading Team, Customer, and provider Keys.
func (s *RDBConfigStore) GetVirtualKeyQuotaByValue(ctx context.Context, value string) (*tables.TableVirtualKey, error) {
	baseQuery := s.DB().WithContext(ctx).
		Preload("Budgets").
		Preload("RateLimit").
		Preload("ProviderConfigs").
		Preload("ProviderConfigs.Budgets").
		Preload("ProviderConfigs.RateLimit")
	return s.findVirtualKeyByValue(baseQuery, value)
}

// CreateVirtualKey creates a new virtual key in the database.
func (s *RDBConfigStore) CreateVirtualKey(ctx context.Context, virtualKey *tables.TableVirtualKey, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	if err := txDB.WithContext(ctx).Create(virtualKey).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// UpdateVirtualKey updates an existing virtual key in the database.
func (s *RDBConfigStore) UpdateVirtualKey(ctx context.Context, virtualKey *tables.TableVirtualKey, tx ...*gorm.DB) error {
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.UpdateVirtualKey(ctx, virtualKey, transaction)
		})
	}

	txDB := tx[0]

	// Check if record exists by ID or Name
	var existing tables.TableVirtualKey
	err := dbForUpdate(txDB.WithContext(ctx)).
		Where("id = ? OR name = ?", virtualKey.ID, virtualKey.Name).
		First(&existing).Error

	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return s.parseGormError(err)
	}

	if errors.Is(err, gorm.ErrRecordNotFound) {
		if err := txDB.WithContext(ctx).Create(virtualKey).Error; err != nil {
			return s.parseGormError(err)
		}
	} else {
		virtualKey.ID = existing.ID
		// Rotation grace-period state travels with the row: unless the caller is
		// itself performing a rotation (RotatedAt set), preserve the stored
		// previous-value fields so a plain update cannot wipe an in-flight grace
		// window (the Select below writes zero values).
		if virtualKey.RotatedAt == nil {
			virtualKey.PreviousValue = existing.PreviousValue
			virtualKey.PreviousValueHash = existing.PreviousValueHash
			virtualKey.PreviousValueExpiresAt = existing.PreviousValueExpiresAt
			virtualKey.RotatedAt = existing.RotatedAt
		}
		// Preserve NULL when a VK-scoped MC already owns the rate limit; prevents config sync from reverting migration.
		if existing.RateLimitID == nil && virtualKey.RateLimitID != nil {
			var mcCount int64
			if err := txDB.WithContext(ctx).Model(&tables.TableModelConfig{}).
				Where("scope = 'virtual_key' AND scope_id = ? AND model_name = '*' AND provider IS NULL AND rate_limit_id IS NOT NULL", virtualKey.ID).
				Count(&mcCount).Error; err != nil {
				return s.parseGormError(err)
			}
			if mcCount > 0 {
				virtualKey.RateLimitID = nil
			}
		}
		columns := []string{"name", "description", "value", "is_active", "expires_at", "delete_after_expire", "team_id", "customer_id", "business_unit_id", "rate_limit_id", "calendar_aligned", "allow_all_providers", "disable_content_logging", "config_hash", "updated_at", "encryption_status", "value_hash", "previous_value", "previous_value_hash", "previous_value_expires_at", "rotated_at"}
		// Nil metadata means the caller did not set it (a config.json entry without a metadata
		// field, or an update of other fields), so the stored metadata is kept. An empty map is an
		// explicit clear and is written (as NULL, see BeforeSave). Decided here, before BeforeSave
		// collapses the empty map to nil.
		if virtualKey.Metadata != nil {
			columns = append(columns, "metadata")
		}
		if err := txDB.WithContext(ctx).
			Select(columns).
			Updates(virtualKey).Error; err != nil {
			return s.parseGormError(err)
		}
	}
	return nil
}

// GetKeysByIDs retrieves multiple keys by their IDs
func (s *RDBConfigStore) GetKeysByIDs(ctx context.Context, ids []string) ([]tables.TableKey, error) {
	if len(ids) == 0 {
		return []tables.TableKey{}, nil
	}
	var keys []tables.TableKey
	if err := s.DB().WithContext(ctx).Where("key_id IN ?", ids).Find(&keys).Error; err != nil {
		return nil, err
	}
	return keys, nil
}

// GetKeysByProvider retrieves all keys for a specific provider
func (s *RDBConfigStore) GetKeysByProvider(ctx context.Context, provider string) ([]tables.TableKey, error) {
	var keys []tables.TableKey
	if err := s.DB().WithContext(ctx).Where("provider = ?", provider).Find(&keys).Error; err != nil {
		return nil, err
	}
	return keys, nil
}

// GetAllRedactedKeys retrieves all redacted keys from the database.
func (s *RDBConfigStore) GetAllRedactedKeys(ctx context.Context, ids []string) ([]schemas.Key, error) {
	var keys []tables.TableKey
	if len(ids) > 0 {
		err := s.DB().WithContext(ctx).Select("id, key_id, name, models_json, blacklisted_models_json, weight").Where("key_id IN ?", ids).Find(&keys).Error
		if err != nil {
			return nil, err
		}
	} else {
		err := s.DB().WithContext(ctx).Select("id, key_id, name, models_json, blacklisted_models_json, weight").Find(&keys).Error
		if err != nil {
			return nil, err
		}
	}
	redactedKeys := make([]schemas.Key, len(keys))
	for i, key := range keys {
		models := key.Models
		if models == nil {
			models = []string{} // Ensure models is never nil in JSON response
		}
		blacklisted := key.BlacklistedModels
		if blacklisted == nil {
			blacklisted = []string{}
		}
		redactedKeys[i] = schemas.Key{
			ID:                key.KeyID,
			Name:              key.Name,
			Models:            models,
			BlacklistedModels: blacklisted,
			Weight:            getWeight(key.Weight),
		}
	}
	return redactedKeys, nil
}

// DeleteVirtualKey deletes a virtual key from the database.
func (s *RDBConfigStore) DeleteVirtualKey(ctx context.Context, id string, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	if err := txDB.WithContext(ctx).Transaction(func(txDB *gorm.DB) error {
		var virtualKey tables.TableVirtualKey
		if err := dbForUpdate(txDB.WithContext(ctx)).Preload("ProviderConfigs").First(&virtualKey, "id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}

		// Delete provider config resources before deleting the configs themselves
		var providerConfigRateLimitIDs []string
		sort.Slice(virtualKey.ProviderConfigs, func(i, j int) bool {
			return virtualKey.ProviderConfigs[i].ID < virtualKey.ProviderConfigs[j].ID
		})
		for _, pc := range virtualKey.ProviderConfigs {
			// Delete the keys join table entries
			if err := txDB.WithContext(ctx).Exec("DELETE FROM governance_virtual_key_provider_config_keys WHERE table_virtual_key_provider_config_id = ?", pc.ID).Error; err != nil {
				return err
			}
			// Delete budgets owned by this provider config
			if err := txDB.WithContext(ctx).Where("provider_config_id = ?", pc.ID).Delete(&tables.TableBudget{}).Error; err != nil {
				return err
			}
			if pc.RateLimitID != nil {
				providerConfigRateLimitIDs = append(providerConfigRateLimitIDs, *pc.RateLimitID)
			}
		}

		// Delete all provider configs associated with the virtual key
		if err := txDB.WithContext(ctx).Delete(&tables.TableVirtualKeyProviderConfig{}, "virtual_key_id = ?", id).Error; err != nil {
			return err
		}
		sort.Strings(providerConfigRateLimitIDs)
		for _, rateLimitID := range providerConfigRateLimitIDs {
			if err := txDB.WithContext(ctx).Delete(&tables.TableRateLimit{}, "id = ?", rateLimitID).Error; err != nil {
				return err
			}
		}
		// Delete budgets owned by this virtual key
		if err := txDB.WithContext(ctx).Where("virtual_key_id = ?", id).Delete(&tables.TableBudget{}).Error; err != nil {
			return err
		}
		// Delete model configs scoped to this virtual key, along with their owned
		// budgets/rate-limits. scope_id has no FK constraint, so this cleanup must be
		// explicit; otherwise per-VK model limits would orphan and leak budget/rate-limit rows.
		if err := s.DeleteModelConfigsForScope(ctx, txDB, tables.ModelConfigScopeVirtualKey, id); err != nil {
			return err
		}
		rateLimitID := virtualKey.RateLimitID
		// Delete the virtual key (use hydrated struct so AfterDelete vault cleanup fires correctly)
		if err := txDB.WithContext(ctx).Delete(&virtualKey).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		// Delete the rate limit associated with the virtual key
		if rateLimitID != nil {
			if err := txDB.WithContext(ctx).Delete(&tables.TableRateLimit{}, "id = ?", *rateLimitID).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// GetVirtualKeyProviderConfigs retrieves all virtual key provider configs from the database.
func (s *RDBConfigStore) GetVirtualKeyProviderConfigs(ctx context.Context, virtualKeyID string) ([]tables.TableVirtualKeyProviderConfig, error) {
	var virtualKey tables.TableVirtualKey
	if err := s.DB().WithContext(ctx).First(&virtualKey, "id = ?", virtualKeyID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return []tables.TableVirtualKeyProviderConfig{}, nil
		}
		return nil, err
	}
	if virtualKey.ID == "" {
		return nil, nil
	}
	var providerConfigs []tables.TableVirtualKeyProviderConfig
	if err := s.DB().WithContext(ctx).Where("virtual_key_id = ?", virtualKey.ID).Find(&providerConfigs).Error; err != nil {
		return nil, err
	}
	return providerConfigs, nil
}

// CreateVirtualKeyProviderConfig creates a new virtual key provider config in the database.
func (s *RDBConfigStore) CreateVirtualKeyProviderConfig(ctx context.Context, virtualKeyProviderConfig *tables.TableVirtualKeyProviderConfig, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	// Store keys before create
	keysToAssociate := virtualKeyProviderConfig.Keys

	// Resolve keys by name/key_id if they don't have database IDs
	// This handles config file inputs that only specify name
	if len(keysToAssociate) > 0 {
		resolvedKeys := make([]tables.TableKey, 0, len(keysToAssociate))
		var unresolvedKeys []string
		for i, k := range keysToAssociate {
			// If key already has a database ID (from UI), use it directly
			if k.ID > 0 {
				resolvedKeys = append(resolvedKeys, k)
				continue
			}
			// Otherwise resolve by KeyID or Name (from config file)
			var dbKey tables.TableKey
			var resolved bool
			if k.KeyID != "" {
				if err := txDB.WithContext(ctx).Where("key_id = ?", k.KeyID).First(&dbKey).Error; err == nil {
					resolvedKeys = append(resolvedKeys, dbKey)
					resolved = true
				}
			}
			if !resolved && k.Name != "" {
				if err := txDB.WithContext(ctx).Where("name = ? AND provider = ?", k.Name, virtualKeyProviderConfig.Provider).First(&dbKey).Error; err == nil {
					resolvedKeys = append(resolvedKeys, dbKey)
					resolved = true
				}
			}
			if !resolved {
				// Collect identifier for unresolved key
				if k.KeyID != "" {
					unresolvedKeys = append(unresolvedKeys, fmt.Sprintf("key_id=%s", k.KeyID))
				} else if k.Name != "" {
					unresolvedKeys = append(unresolvedKeys, fmt.Sprintf("name=%s", k.Name))
				} else {
					unresolvedKeys = append(unresolvedKeys, fmt.Sprintf("key[%d]", i))
				}
			}
		}
		if len(unresolvedKeys) > 0 {
			return &ErrUnresolvedKeys{Identifiers: unresolvedKeys}
		}
		keysToAssociate = resolvedKeys
	}
	sortTableKeysByID(keysToAssociate)

	// Clear Keys before Create to prevent GORM from auto-associating unresolved keys (with ID=0)
	// We'll manually associate the resolved keys after Create
	virtualKeyProviderConfig.Keys = nil

	if err := txDB.WithContext(ctx).Create(virtualKeyProviderConfig).Error; err != nil {
		return s.parseGormError(err)
	}

	// Associate keys after the provider config has an ID
	if len(keysToAssociate) > 0 {
		if err := txDB.WithContext(ctx).Model(virtualKeyProviderConfig).Association("Keys").Append(keysToAssociate); err != nil {
			return err
		}
	}
	return nil
}

// Bounds for the set-based provider-config replacement below. They exist to keep
// a single statement's payload predictable no matter how many providers a caller
// supplies: without them, a virtual key with thousands of providers would build
// one enormous multi-row INSERT or one enormous IN (...) list.
const (
	// Max rows per INSERT statement.
	virtualKeyProviderConfigInsertBatchSize = 100
	// Max values per IN (...) predicate.
	virtualKeyProviderConfigIDChunkSize = 1000
)

// providerKeyName identifies a key by the pair that is actually unique for it.
// Key names are only unique within a provider, so a bare name is not a safe map
// key when several providers are being resolved in one pass.
type providerKeyName struct {
	provider string
	name     string
}

// unresolvedKeyIdentifier renders a key reference for an error message, using
// whichever identifier the caller actually supplied. index is the key's position
// within its provider config and is only used when the reference is entirely
// blank, so the error can still point at something.
func unresolvedKeyIdentifier(key tables.TableKey, index int) string {
	switch {
	case key.KeyID != "":
		return fmt.Sprintf("key_id=%s", key.KeyID)
	case key.Name != "":
		return fmt.Sprintf("name=%s", key.Name)
	default:
		return fmt.Sprintf("key[%d]", index)
	}
}

// ReplaceVirtualKeyProviderConfigs atomically swaps a virtual key's entire provider
// config set — deleting the old rows and inserting the new ones — inside the caller's
// transaction.
//
// Why this exists: the previous call site looped over old configs calling
// DeleteVirtualKeyProviderConfig, then looped over new ones calling
// CreateVirtualKeyProviderConfig. That is ~4 statements per provider per virtual key,
// which dominated wall-clock time for callers that rewrite provider configs across
// thousands of virtual keys in one operation.
// This does the same work as a handful of set-based statements instead.
//
// Delete parity with DeleteVirtualKeyProviderConfig is deliberate and must be preserved:
// it removes the config-key join rows, the budgets owned by the config, the config row
// itself, and the config's rate limit. Anything added there must be added here too.
//
// Callers should note it mutates providerConfigs in place: IDs are zeroed before insert,
// then populated by the database, and Keys is detached so GORM does not auto-upsert the
// association (the join rows are written explicitly below).
//
// Requires an existing transaction so a partial replacement can never be committed.
func (s *RDBConfigStore) ReplaceVirtualKeyProviderConfigs(ctx context.Context, virtualKeyID string, providerConfigs []tables.TableVirtualKeyProviderConfig, txDB *gorm.DB) error {
	if txDB == nil {
		return fmt.Errorf("ReplaceVirtualKeyProviderConfigs requires a transaction, got nil tx")
	}
	txDB = txDB.WithContext(ctx)

	// Phase 1: read the outgoing rows. Locked FOR UPDATE so a concurrent writer
	// cannot delete or repoint them between this read and the deletes below.
	var oldConfigs []tables.TableVirtualKeyProviderConfig
	if err := dbForUpdate(txDB).Where("virtual_key_id = ?", virtualKeyID).Order("id").Find(&oldConfigs).Error; err != nil {
		return err
	}
	oldIDs := make([]uint, 0, len(oldConfigs))
	oldRateLimitIDs := make([]string, 0, len(oldConfigs))
	for i := range oldConfigs {
		oldIDs = append(oldIDs, oldConfigs[i].ID)
		if oldConfigs[i].RateLimitID != nil {
			oldRateLimitIDs = append(oldRateLimitIDs, *oldConfigs[i].RateLimitID)
		}
	}
	// Phase 2: delete children before parents. Join rows and budgets reference the
	// config row, so they must go first or the FKs would block the delete.
	for start := 0; start < len(oldIDs); start += virtualKeyProviderConfigIDChunkSize {
		end := min(start+virtualKeyProviderConfigIDChunkSize, len(oldIDs))
		ids := oldIDs[start:end]
		if err := txDB.Where("table_virtual_key_provider_config_id IN ?", ids).Delete(&tables.TableVirtualKeyProviderConfigKey{}).Error; err != nil {
			return err
		}
		if err := txDB.Where("provider_config_id IN ?", ids).Delete(&tables.TableBudget{}).Error; err != nil {
			return err
		}
		if err := txDB.Where("id IN ?", ids).Delete(&tables.TableVirtualKeyProviderConfig{}).Error; err != nil {
			return err
		}
	}
	// Rate limits are deleted last: the config row holds the FK to them, so they
	// only become unreferenced once every config chunk above is gone.
	for start := 0; start < len(oldRateLimitIDs); start += virtualKeyProviderConfigIDChunkSize {
		end := min(start+virtualKeyProviderConfigIDChunkSize, len(oldRateLimitIDs))
		if err := txDB.Where("id IN ?", oldRateLimitIDs[start:end]).Delete(&tables.TableRateLimit{}).Error; err != nil {
			return err
		}
	}
	// An empty incoming set means "remove all providers", which the deletes above
	// have already done.
	if len(providerConfigs) == 0 {
		return nil
	}

	// Phase 3: collect the key references that still need resolving. A key carried
	// over with a populated ID is already resolved; the rest are identified either
	// by KeyID (UI input) or by name alone (config-file input). Both forms are
	// gathered here and resolved in batched passes so the whole set costs a
	// handful of queries rather than one per key.
	keyIDs := make([]string, 0)
	namesByProvider := make(map[string][]string)
	for i := range providerConfigs {
		providerConfigs[i].ID = 0
		providerConfigs[i].VirtualKeyID = virtualKeyID
		for j := range providerConfigs[i].Keys {
			key := providerConfigs[i].Keys[j]
			if key.ID > 0 {
				continue
			}
			if key.KeyID != "" {
				keyIDs = append(keyIDs, key.KeyID)
			}
			// A key can carry both, because KeyID lookup is allowed to miss and
			// fall back to the name. Queue the name in that case too.
			if key.Name != "" {
				provider := providerConfigs[i].Provider
				namesByProvider[provider] = append(namesByProvider[provider], key.Name)
			}
		}
	}
	resolvedByKeyID := make(map[string]tables.TableKey, len(keyIDs))
	for start := 0; start < len(keyIDs); start += virtualKeyProviderConfigIDChunkSize {
		end := min(start+virtualKeyProviderConfigIDChunkSize, len(keyIDs))
		var keys []tables.TableKey
		if err := txDB.Where("key_id IN ?", keyIDs[start:end]).Find(&keys).Error; err != nil {
			return err
		}
		for i := range keys {
			resolvedByKeyID[keys[i].KeyID] = keys[i]
		}
	}
	// Names are only unique within a provider, so they are batched per provider
	// and keyed by the pair. The provider count is bounded by the incoming config
	// set, and each provider's names are chunked like every other IN (...) here.
	resolvedByName := make(map[providerKeyName]tables.TableKey)
	for provider, names := range namesByProvider {
		for start := 0; start < len(names); start += virtualKeyProviderConfigIDChunkSize {
			end := min(start+virtualKeyProviderConfigIDChunkSize, len(names))
			var keys []tables.TableKey
			if err := txDB.Where("provider = ? AND name IN ?", provider, names[start:end]).Find(&keys).Error; err != nil {
				return err
			}
			for i := range keys {
				resolvedByName[providerKeyName{provider: provider, name: keys[i].Name}] = keys[i]
			}
		}
	}
	// Phase 4: pair each config with its resolved keys. Collect every unresolved
	// reference before failing so the error names all of them at once, and so an
	// unresolved key aborts the transaction rather than silently dropping a grant.
	var unresolved []string
	keysByConfig := make([][]tables.TableKey, len(providerConfigs))
	for i := range providerConfigs {
		for j := range providerConfigs[i].Keys {
			key := providerConfigs[i].Keys[j]
			if key.ID == 0 {
				// Same precedence as the single-config path: KeyID wins, and a
				// KeyID that fails to resolve falls back to the name rather than
				// failing outright.
				resolvedKey, ok := resolvedByKeyID[key.KeyID]
				if !ok && key.Name != "" {
					resolvedKey, ok = resolvedByName[providerKeyName{provider: providerConfigs[i].Provider, name: key.Name}]
				}
				if !ok {
					unresolved = append(unresolved, unresolvedKeyIdentifier(key, j))
					continue
				}
				key = resolvedKey
			}
			keysByConfig[i] = append(keysByConfig[i], key)
		}
		providerConfigs[i].Keys = nil
	}
	if len(unresolved) > 0 {
		return &ErrUnresolvedKeys{Identifiers: unresolved}
	}

	// Phase 5: insert. Config rows first so the database assigns their IDs, which
	// the join rows below need.
	// CreateInBatches already splits the slice into batchSize-sized INSERTs, so the
	// bound is enforced here without an extra chunking loop. Keys is omitted because
	// the join rows are written explicitly below.
	if err := txDB.Omit("Keys").CreateInBatches(providerConfigs, virtualKeyProviderConfigInsertBatchSize).Error; err != nil {
		return s.parseGormError(err)
	}
	joinRows := make([]tables.TableVirtualKeyProviderConfigKey, 0)
	for i := range providerConfigs {
		for j := range keysByConfig[i] {
			joinRows = append(joinRows, tables.TableVirtualKeyProviderConfigKey{
				TableVirtualKeyProviderConfigID: providerConfigs[i].ID,
				TableKeyID:                      keysByConfig[i][j].ID,
			})
		}
	}
	if len(joinRows) == 0 {
		return nil
	}
	if err := txDB.CreateInBatches(joinRows, virtualKeyProviderConfigInsertBatchSize).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// UpdateVirtualKeyProviderConfig updates a virtual key provider config in the database.
func (s *RDBConfigStore) UpdateVirtualKeyProviderConfig(ctx context.Context, virtualKeyProviderConfig *tables.TableVirtualKeyProviderConfig, tx ...*gorm.DB) error {
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.UpdateVirtualKeyProviderConfig(ctx, virtualKeyProviderConfig, transaction)
		})
	}

	var txDB *gorm.DB
	txDB = tx[0]
	if virtualKeyProviderConfig.ID != 0 {
		var existing tables.TableVirtualKeyProviderConfig
		if err := dbForUpdate(txDB.WithContext(ctx)).First(&existing, "id = ?", virtualKeyProviderConfig.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
	}

	// Store keys before save
	keysToAssociate := virtualKeyProviderConfig.Keys

	// Resolve keys by name/key_id if they don't have database IDs
	// This handles config file inputs that only specify name
	if len(keysToAssociate) > 0 {
		resolvedKeys := make([]tables.TableKey, 0, len(keysToAssociate))
		var unresolvedKeys []string
		for i, k := range keysToAssociate {
			// If key already has a database ID (from UI), use it directly
			if k.ID > 0 {
				resolvedKeys = append(resolvedKeys, k)
				continue
			}
			// Otherwise resolve by KeyID or Name (from config file)
			var dbKey tables.TableKey
			var resolved bool
			if k.KeyID != "" {
				if err := txDB.WithContext(ctx).Where("key_id = ?", k.KeyID).First(&dbKey).Error; err == nil {
					resolvedKeys = append(resolvedKeys, dbKey)
					resolved = true
				}
			}
			if !resolved && k.Name != "" {
				if err := txDB.WithContext(ctx).Where("name = ? AND provider = ?", k.Name, virtualKeyProviderConfig.Provider).First(&dbKey).Error; err == nil {
					resolvedKeys = append(resolvedKeys, dbKey)
					resolved = true
				}
			}
			if !resolved {
				// Collect identifier for unresolved key
				if k.KeyID != "" {
					unresolvedKeys = append(unresolvedKeys, fmt.Sprintf("key_id=%s", k.KeyID))
				} else if k.Name != "" {
					unresolvedKeys = append(unresolvedKeys, fmt.Sprintf("name=%s", k.Name))
				} else {
					unresolvedKeys = append(unresolvedKeys, fmt.Sprintf("key[%d]", i))
				}
			}
		}
		if len(unresolvedKeys) > 0 {
			return &ErrUnresolvedKeys{Identifiers: unresolvedKeys}
		}
		keysToAssociate = resolvedKeys
	}
	sortTableKeysByID(keysToAssociate)

	// Clear Keys before Save to prevent GORM from auto-associating unresolved keys (with ID=0)
	// We'll manually manage the association after Save
	virtualKeyProviderConfig.Keys = nil

	if err := txDB.WithContext(ctx).Save(virtualKeyProviderConfig).Error; err != nil {
		return s.parseGormError(err)
	}

	// Clear existing key associations and set new ones
	if err := txDB.WithContext(ctx).Model(virtualKeyProviderConfig).Association("Keys").Clear(); err != nil {
		return err
	}
	if len(keysToAssociate) > 0 {
		if err := txDB.WithContext(ctx).Model(virtualKeyProviderConfig).Association("Keys").Append(keysToAssociate); err != nil {
			return err
		}
	}
	return nil
}

// DeleteVirtualKeyProviderConfig deletes a virtual key provider config from the database.
func (s *RDBConfigStore) DeleteVirtualKeyProviderConfig(ctx context.Context, id uint, tx ...*gorm.DB) error {
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.DeleteVirtualKeyProviderConfig(ctx, id, transaction)
		})
	}

	var txDB *gorm.DB
	txDB = tx[0]
	// First fetch the provider config to get budget and rate limit IDs
	var providerConfig tables.TableVirtualKeyProviderConfig
	if err := dbForUpdate(txDB.WithContext(ctx)).First(&providerConfig, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	if err := txDB.WithContext(ctx).Exec("DELETE FROM governance_virtual_key_provider_config_keys WHERE table_virtual_key_provider_config_id = ?", id).Error; err != nil {
		return err
	}
	// Store the rate limit ID before deleting
	rateLimitID := providerConfig.RateLimitID
	// Delete budgets owned by this provider config
	if err := txDB.WithContext(ctx).Where("provider_config_id = ?", id).Delete(&tables.TableBudget{}).Error; err != nil {
		return err
	}
	// Delete the provider config
	if err := txDB.WithContext(ctx).Delete(&tables.TableVirtualKeyProviderConfig{}, "id = ?", id).Error; err != nil {
		return err
	}
	// Delete the rate limit if it exists
	if rateLimitID != nil {
		if err := txDB.WithContext(ctx).Delete(&tables.TableRateLimit{}, "id = ?", *rateLimitID).Error; err != nil {
			return err
		}
	}
	return nil
}

const teamSelectWithVKCount = "governance_teams.*, (SELECT COUNT(*) FROM governance_virtual_keys WHERE team_id = governance_teams.id) AS virtual_key_count"

// GetTeams retrieves all teams from the database.
//
// When ctx carries a QueryScope, the query is narrowed to teams the
// caller is allowed to see.
func (s *RDBConfigStore) GetTeams(ctx context.Context, customerID string) ([]tables.TableTeam, error) {
	// Preload relationships for complete information
	query := s.ScopedDB(ctx).
		Select(teamSelectWithVKCount).
		Preload("Customer").Preload("Budgets").Preload("RateLimit")
	// Optional filtering by customer
	if customerID != "" {
		query = query.Where("customer_id = ?", customerID)
	}
	var teams []tables.TableTeam
	if err := query.Order("created_at ASC").Find(&teams).Error; err != nil {
		return nil, err
	}
	return teams, nil
}

// GetTeamsPaginated retrieves teams with pagination, filtering, and search support.
//
// When ctx carries a QueryScope, the query is narrowed to teams the
// caller is allowed to see.
func (s *RDBConfigStore) GetTeamsPaginated(ctx context.Context, params TeamsQueryParams) ([]tables.TableTeam, int64, error) {
	baseQuery := s.ScopedDB(ctx).Model(&tables.TableTeam{})

	if params.CustomerID != "" {
		baseQuery = baseQuery.Where("customer_id = ?", params.CustomerID)
	}
	if params.Search != "" {
		search := "%" + strings.ToLower(params.Search) + "%"
		baseQuery = baseQuery.Where("LOWER(name) LIKE ?", search)
	}

	var totalCount int64
	if err := baseQuery.Count(&totalCount).Error; err != nil {
		return nil, 0, err
	}

	limit := params.Limit
	offset := params.Offset
	if limit <= 0 {
		limit = 25
	}
	if offset < 0 {
		offset = 0
	}

	var teams []tables.TableTeam
	if err := baseQuery.
		Select(teamSelectWithVKCount).
		Preload("Customer").Preload("Budgets").Preload("RateLimit").
		Order("created_at ASC, id ASC").
		Offset(offset).Limit(limit).
		Find(&teams).Error; err != nil {
		return nil, 0, err
	}

	return teams, totalCount, nil
}

// GetTeam retrieves a specific team from the database.
//
// When ctx carries a QueryScope, a team that doesn't satisfy the scope
// returns ErrNotFound; the caller cannot distinguish "doesn't exist"
// from "not visible," matching the leak-prevention contract used by
// the other governance entities.
func (s *RDBConfigStore) GetTeam(ctx context.Context, id string, tx ...*gorm.DB) (*tables.TableTeam, error) {
	var team tables.TableTeam
	if err := s.ScopedDB(ctx, tx...).
		Select(teamSelectWithVKCount).
		Preload("Customer").Preload("Budgets").Preload("RateLimit").
		First(&team, "governance_teams.id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &team, nil
}

// GetTeamByName retrieves a team by name. When customerID is non-empty the lookup is scoped to that customer
func (s *RDBConfigStore) GetTeamByName(ctx context.Context, name string, customerID string) (*tables.TableTeam, error) {
	var team tables.TableTeam
	q := s.DB().WithContext(ctx).
		Select(teamSelectWithVKCount).
		Preload("Customer").Preload("Budgets").Preload("RateLimit").
		Where("name = ?", name)
	if customerID != "" {
		q = q.Where("customer_id = ?", customerID)
	}

	if err := q.First(&team).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &team, nil
}

// GetTeamBySourceID retrieves a team by its source ID.
func (s *RDBConfigStore) GetTeamBySourceID(ctx context.Context, sourceID string) (*tables.TableTeam, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil, ErrNotFound
	}
	var team tables.TableTeam
	if err := s.DB().WithContext(ctx).
		Select(teamSelectWithVKCount).
		Preload("Customer").Preload("Budgets").Preload("RateLimit").
		Where("source_id = ?", sourceID).
		First(&team).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &team, nil
}

// CreateTeam creates a new team in the database.
func (s *RDBConfigStore) CreateTeam(ctx context.Context, team *tables.TableTeam, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	if err := txDB.WithContext(ctx).Create(team).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// UpdateTeam updates an existing team in the database.
func (s *RDBConfigStore) UpdateTeam(ctx context.Context, team *tables.TableTeam, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	// created_at is immutable: Save writes every column, so a caller passing a row it
	// didn't read from the DB (e.g. a config.json reload) would otherwise zero it out.
	// A missing row is not an error here — this also serves create-or-update callers,
	// so fall through to Save and let it insert.
	if team.ID != "" {
		var existing tables.TableTeam
		if err := txDB.WithContext(ctx).Select("created_at").First(&existing, "id = ?", team.ID).Error; err == nil {
			team.CreatedAt = existing.CreatedAt
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
	}
	// Save would write the preloaded Customer back and copy its id over CustomerID, so a team
	// moved to another customer (or detached) would snap back; loaded virtual keys would likewise be
	// repointed at this team. Budgets and the rate limit are still saved with the team, which config
	// sync relies on for inline declarations.
	if err := txDB.WithContext(ctx).Omit("Customer", "VirtualKeys").Save(team).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// DeleteTeam deletes a team from the database.
// Owned budgets cascade via the governance_budgets.team_id FK.
// Rate limit is a sibling row (team holds a FK to it) — deleted explicitly.
func (s *RDBConfigStore) DeleteTeam(ctx context.Context, id string, tx ...*gorm.DB) error {
	if len(tx) == 0 || tx[0] == nil {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.DeleteTeam(ctx, id, transaction)
		})
	}

	txDB := tx[0]
	var team tables.TableTeam
	if err := dbForUpdate(txDB.WithContext(ctx)).Preload("RateLimit").First(&team, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	// Set team_id to null for all virtual keys associated with the team
	if err := txDB.WithContext(ctx).Model(&tables.TableVirtualKey{}).Where("team_id = ?", id).Update("team_id", nil).Error; err != nil {
		return err
	}
	rateLimitID := team.RateLimitID
	// Delete the team - owned budgets cascade via FK on governance_budgets.team_id
	if err := txDB.WithContext(ctx).Delete(&tables.TableTeam{}, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	// Delete the team's rate limit if it exists
	if rateLimitID != nil {
		if err := txDB.WithContext(ctx).Delete(&tables.TableRateLimit{}, "id = ?", *rateLimitID).Error; err != nil {
			return err
		}
	}
	return nil
}

// GetCustomers retrieves all customers from the database.
//
// When ctx carries a QueryScope, the query is narrowed to customers
// the caller is allowed to see.
func (s *RDBConfigStore) GetCustomers(ctx context.Context) ([]tables.TableCustomer, error) {
	var customers []tables.TableCustomer
	if err := preloadCustomerRelations(s.ScopedDB(ctx), "").
		Order("created_at ASC").
		Find(&customers).Error; err != nil {
		return nil, err
	}
	for i := range customers {
		customers[i].VirtualKeyCount = len(customers[i].VirtualKeys)
	}
	return customers, nil
}

// GetCustomersPaginated retrieves customers with pagination and optional
// search filtering.
//
// When ctx carries a QueryScope, the query is narrowed to customers
// the caller is allowed to see.
func (s *RDBConfigStore) GetCustomersPaginated(ctx context.Context, params CustomersQueryParams) ([]tables.TableCustomer, int64, error) {
	baseQuery := s.ScopedDB(ctx).Model(&tables.TableCustomer{})
	if params.Search != "" {
		search := "%" + strings.ToLower(params.Search) + "%"
		baseQuery = baseQuery.Where("LOWER(name) LIKE ?", search)
	}
	var totalCount int64
	if err := baseQuery.Count(&totalCount).Error; err != nil {
		return nil, 0, err
	}
	limit := params.Limit
	offset := params.Offset
	if limit <= 0 {
		limit = 25
	} else if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	var customers []tables.TableCustomer
	if err := baseQuery.
		Preload("Budgets").
		Preload("RateLimit").
		Order("created_at ASC, id ASC").
		Offset(offset).Limit(limit).
		Find(&customers).Error; err != nil {
		return nil, 0, err
	}
	if err := s.attachCustomerVirtualKeyCounts(ctx, customers); err != nil {
		return nil, 0, err
	}
	if err := s.attachCustomerTeamCounts(ctx, customers); err != nil {
		return nil, 0, err
	}
	return customers, totalCount, nil
}

// GetCustomer retrieves a specific customer from the database.
//
// When ctx carries a QueryScope, a customer that doesn't satisfy the
// scope returns ErrNotFound; the caller cannot distinguish "doesn't
// exist" from "not visible," matching the leak-prevention contract
// used by the other governance entities.
func (s *RDBConfigStore) GetCustomer(ctx context.Context, id string, tx ...*gorm.DB) (*tables.TableCustomer, error) {
	var customer tables.TableCustomer
	if err := preloadCustomerRelations(s.ScopedDB(ctx, tx...), "").
		First(&customer, "governance_customers.id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	customer.VirtualKeyCount = len(customer.VirtualKeys)
	return &customer, nil
}

// CreateCustomer creates a new customer in the database.
func (s *RDBConfigStore) CreateCustomer(ctx context.Context, customer *tables.TableCustomer, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	if err := txDB.WithContext(ctx).Create(customer).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// UpdateCustomer updates an existing customer in the database.
func (s *RDBConfigStore) UpdateCustomer(ctx context.Context, customer *tables.TableCustomer, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	// created_at is immutable: Save writes every column, so a caller passing a row it
	// didn't read from the DB (e.g. a config.json reload) would otherwise zero it out.
	// A missing row is not an error here — this also serves create-or-update callers,
	// so fall through to Save and let it insert.
	if customer.ID != "" {
		var existing tables.TableCustomer
		if err := txDB.WithContext(ctx).Select("created_at").First(&existing, "id = ?", customer.ID).Error; err == nil {
			customer.CreatedAt = existing.CreatedAt
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
	}
	// Saving the loaded teams and virtual keys would repoint each one's customer_id at this
	// customer, including one that moved to another customer after this row was read. Budgets and
	// the rate limit are still saved with the customer, which config sync relies on for inline
	// declarations.
	if err := txDB.WithContext(ctx).Omit("Teams", "VirtualKeys").Save(customer).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// DeleteCustomer deletes a customer from the database.
func (s *RDBConfigStore) DeleteCustomer(ctx context.Context, id string, tx ...*gorm.DB) error {
	if len(tx) == 0 || tx[0] == nil {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.DeleteCustomer(ctx, id, transaction)
		})
	}

	txDB := tx[0]
	var customer tables.TableCustomer
	if err := dbForUpdate(txDB.WithContext(ctx)).Preload("RateLimit").First(&customer, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	// Null out customer_id on associated VKs and teams before deleting the customer row.
	if err := txDB.WithContext(ctx).Model(&tables.TableVirtualKey{}).Where("customer_id = ?", id).Update("customer_id", nil).Error; err != nil {
		return err
	}
	// Set customer_id to null for all teams associated with the customer
	if err := txDB.WithContext(ctx).Model(&tables.TableTeam{}).Where("customer_id = ?", id).Update("customer_id", nil).Error; err != nil {
		return err
	}
	rateLimitID := customer.RateLimitID
	// Explicitly delete owned budgets before the customer row. FK cascades cannot
	// be relied on across all dialects for constraints added to pre-existing tables.
	if err := txDB.WithContext(ctx).Where("customer_id = ?", id).Delete(&tables.TableBudget{}).Error; err != nil {
		return err
	}
	if err := txDB.WithContext(ctx).Delete(&tables.TableCustomer{}, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	if rateLimitID != nil {
		if err := txDB.WithContext(ctx).Delete(&tables.TableRateLimit{}, "id = ?", *rateLimitID).Error; err != nil {
			return err
		}
	}
	return nil
}

// GetRateLimits retrieves all rate limits from the database.
func (s *RDBConfigStore) GetRateLimits(ctx context.Context) ([]tables.TableRateLimit, error) {
	var rateLimits []tables.TableRateLimit
	if err := s.DB().WithContext(ctx).Order("created_at ASC").Find(&rateLimits).Error; err != nil {
		return nil, err
	}
	return rateLimits, nil
}

// GetRateLimit retrieves a specific rate limit from the database.
func (s *RDBConfigStore) GetRateLimit(ctx context.Context, id string, tx ...*gorm.DB) (*tables.TableRateLimit, error) {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	var rateLimit tables.TableRateLimit
	if err := txDB.WithContext(ctx).First(&rateLimit, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &rateLimit, nil
}

// CreateRateLimit creates a new rate limit in the database.
func (s *RDBConfigStore) CreateRateLimit(ctx context.Context, rateLimit *tables.TableRateLimit, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	if err := txDB.WithContext(ctx).Create(rateLimit).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// UpdateRateLimit updates a rate limit in the database.
func (s *RDBConfigStore) UpdateRateLimit(ctx context.Context, rateLimit *tables.TableRateLimit, tx ...*gorm.DB) error {
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.UpdateRateLimit(ctx, rateLimit, transaction)
		})
	}

	txDB := tx[0]
	if rateLimit.ID != "" {
		var existing tables.TableRateLimit
		if err := dbForUpdate(txDB.WithContext(ctx)).First(&existing, "id = ?", rateLimit.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		// Same ownership boundary as UpdateBudget: the four counters are advanced
		// by the governance dump path and UpdateRateLimitUsage, never by a
		// configuration write, which is why GenerateRateLimitHash excludes them.
		// The existing row is already read under the same lock that guards the
		// Save below, so this adds no new race window.
		rateLimit.TokenCurrentUsage = existing.TokenCurrentUsage
		rateLimit.TokenLastReset = existing.TokenLastReset
		rateLimit.RequestCurrentUsage = existing.RequestCurrentUsage
		rateLimit.RequestLastReset = existing.RequestLastReset
		// created_at is immutable: Save writes every column, so a caller passing a row
		// it didn't read from the DB (e.g. a config.json reload) would zero it out.
		rateLimit.CreatedAt = existing.CreatedAt
	}
	if err := txDB.WithContext(ctx).Save(rateLimit).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// UpdateRateLimits updates multiple rate limits in the database.
func (s *RDBConfigStore) UpdateRateLimits(ctx context.Context, rateLimits []*tables.TableRateLimit, tx ...*gorm.DB) error {
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.UpdateRateLimits(ctx, rateLimits, transaction)
		})
	}

	txDB := tx[0]
	sortedRateLimits := append([]*tables.TableRateLimit(nil), rateLimits...)
	sort.Slice(sortedRateLimits, func(i, j int) bool { return sortedRateLimits[i].ID < sortedRateLimits[j].ID })
	for _, rl := range sortedRateLimits {
		if err := s.UpdateRateLimit(ctx, rl, txDB); err != nil {
			return err
		}
	}
	return nil
}

// DeleteRateLimit deletes a rate limit from the database.
func (s *RDBConfigStore) DeleteRateLimit(ctx context.Context, id string, tx ...*gorm.DB) error {
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.DeleteRateLimit(ctx, id, transaction)
		})
	}

	txDB := tx[0]
	var existing tables.TableRateLimit
	if err := dbForUpdate(txDB.WithContext(ctx)).First(&existing, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	if err := txDB.WithContext(ctx).Delete(&tables.TableRateLimit{}, "id = ?", id).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// GetBudgets retrieves all budgets from the database.
func (s *RDBConfigStore) GetBudgets(ctx context.Context) ([]tables.TableBudget, error) {
	var budgets []tables.TableBudget
	if err := s.DB().WithContext(ctx).Order("created_at ASC").Find(&budgets).Error; err != nil {
		return nil, err
	}
	return budgets, nil
}

// GetBudget retrieves a specific budget from the database.
func (s *RDBConfigStore) GetBudget(ctx context.Context, id string, tx ...*gorm.DB) (*tables.TableBudget, error) {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	var budget tables.TableBudget
	if err := txDB.WithContext(ctx).First(&budget, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &budget, nil
}

// GetVirtualKeyBudgets returns the budgets a virtual key holds through virtual_key_id, leaving out
// any a model config has taken over.
func (s *RDBConfigStore) GetVirtualKeyBudgets(ctx context.Context, virtualKeyID string, tx ...*gorm.DB) ([]tables.TableBudget, error) {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	var budgets []tables.TableBudget
	if err := txDB.WithContext(ctx).Where("virtual_key_id = ? AND model_config_id IS NULL", virtualKeyID).Find(&budgets).Error; err != nil {
		return nil, err
	}
	return budgets, nil
}

// GetVirtualKeyProviderConfigBudgets returns the budgets a virtual key provider config holds through
// provider_config_id, leaving out any a model config has taken over.
func (s *RDBConfigStore) GetVirtualKeyProviderConfigBudgets(ctx context.Context, providerConfigID uint, tx ...*gorm.DB) ([]tables.TableBudget, error) {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	var budgets []tables.TableBudget
	if err := txDB.WithContext(ctx).Where("provider_config_id = ? AND model_config_id IS NULL", providerConfigID).Find(&budgets).Error; err != nil {
		return nil, err
	}
	return budgets, nil
}

// CreateBudget creates a new budget in the database.
func (s *RDBConfigStore) CreateBudget(ctx context.Context, budget *tables.TableBudget, tx ...*gorm.DB) error {
	var txDB *gorm.DB
	if len(tx) > 0 {
		txDB = tx[0]
	} else {
		txDB = s.DB()
	}
	if err := txDB.WithContext(ctx).Create(budget).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// UpdateBudgets updates multiple budgets in the database.
func (s *RDBConfigStore) UpdateBudgets(ctx context.Context, budgets []*tables.TableBudget, tx ...*gorm.DB) error {
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.UpdateBudgets(ctx, budgets, transaction)
		})
	}

	txDB := tx[0]
	sortedBudgets := append([]*tables.TableBudget(nil), budgets...)
	sort.Slice(sortedBudgets, func(i, j int) bool { return sortedBudgets[i].ID < sortedBudgets[j].ID })
	for _, b := range sortedBudgets {
		if err := s.UpdateBudget(ctx, b, txDB); err != nil {
			return err
		}
	}
	return nil
}

// UpdateBudget updates a budget in the database.
func (s *RDBConfigStore) UpdateBudget(ctx context.Context, budget *tables.TableBudget, tx ...*gorm.DB) error {
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.UpdateBudget(ctx, budget, transaction)
		})
	}

	txDB := tx[0]
	if budget.ID != "" {
		var existing tables.TableBudget
		if err := txDB.WithContext(ctx).First(&existing, "id = ?", budget.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		ownerBudget := *budget
		if ownerBudget.VirtualKeyID == nil {
			ownerBudget.VirtualKeyID = existing.VirtualKeyID
		}
		if ownerBudget.ProviderConfigID == nil {
			ownerBudget.ProviderConfigID = existing.ProviderConfigID
		}
		if ownerBudget.TeamID == nil {
			ownerBudget.TeamID = existing.TeamID
		}
		if ownerBudget.CustomerID == nil {
			ownerBudget.CustomerID = existing.CustomerID
		}
		if err := lockBudgetOwner(ctx, txDB, ownerBudget); err != nil {
			return err
		}
		if err := dbForUpdate(txDB.WithContext(ctx)).First(&existing, "id = ?", budget.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		// Usage accounting is runtime-owned, never authored by a configuration
		// write: it is advanced by the governance dump path and UpdateBudgetUsage,
		// which is why GenerateBudgetHash excludes both columns. Carrying them
		// forward is what stops a source_of_truth=config.json startup force-sync
		// from replaying the file row's zero values over live accounting — the
		// file declares max_limit and reset_duration, so a whole-row Save of it
		// would otherwise write current_usage=0 and a zero last_reset.
		//
		// Consequence: current_usage / last_reset in config.json are seed values
		// applied by CreateBudget on first import, and inert thereafter.
		budget.CurrentUsage = existing.CurrentUsage
		budget.LastReset = existing.LastReset
		// A write that names no owner at all keeps the business unit the row already has: a budget a
		// unit links by budget_id is written here naming no owner, and saving the row as given would
		// detach it from its unit with nothing on the unit's side to notice. A write that names any
		// owner is an ownership change and is applied as given.
		if budgetNamesNoOwner(budget) {
			budget.BusinessUnitID = existing.BusinessUnitID
		}
		// Preserve MC ownership if already migrated; config.json sync would otherwise revert it.
		if existing.ModelConfigID != nil {
			budget.ModelConfigID = existing.ModelConfigID
			budget.VirtualKeyID = nil
		}
		// Overrides are managed by the dedicated override path, not UpdateBudget;
		// carry them forward so partial updates can't wipe an active override.
		// The grant columns must travel with the derived remaining count: dropping
		// the anchor would leave a cycles override with no lifecycle, which
		// validateOverride rejects outright.
		budget.OverrideAmount = existing.OverrideAmount
		budget.OverrideMode = existing.OverrideMode
		budget.OverrideCyclesRemaining = existing.OverrideCyclesRemaining
		budget.OverrideCyclesTotal = existing.OverrideCyclesTotal
		budget.OverrideAnchorReset = existing.OverrideAnchorReset
	}
	if err := txDB.WithContext(ctx).Save(budget).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// UpdateBudgetOverride atomically updates only override columns so concurrent usage changes are preserved.
//
// The grant is anchored at the budget's current window boundary rather than at
// the persisted last_reset. Those differ by up to one reset-ticker interval,
// because a node advances last_reset in memory before the next dump flushes it,
// and anchoring one window behind would silently grant an extra window.
func (s *RDBConfigStore) UpdateBudgetOverride(ctx context.Context, id string, amount float64, mode tables.BudgetOverrideMode, cyclesTotal int, calendarAligned bool, tx ...*gorm.DB) (*tables.TableBudget, error) {
	if len(tx) == 0 {
		var updated *tables.TableBudget
		err := s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			var err error
			updated, err = s.UpdateBudgetOverride(ctx, id, amount, mode, cyclesTotal, calendarAligned, transaction)
			return err
		})
		return updated, err
	}

	txDB := tx[0].WithContext(ctx)
	var budget tables.TableBudget
	if err := dbForUpdate(txDB).First(&budget, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	// IsCalendarAligned is not persisted on the budget row and a bare First does
	// not fire the owner's AfterFind, so the caller has to supply it.
	budget.IsCalendarAligned = calendarAligned
	if err := budget.SetOverrideAt(amount, mode, cyclesTotal, budget.WindowStart(time.Now())); err != nil {
		return nil, err
	}
	if err := txDB.Session(&gorm.Session{SkipHooks: true}).Model(&tables.TableBudget{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"override_amount":           budget.OverrideAmount,
			"override_mode":             budget.OverrideMode,
			"override_cycles_remaining": budget.OverrideCyclesRemaining,
			"override_cycles_total":     budget.OverrideCyclesTotal,
			"override_anchor_reset":     budget.OverrideAnchorReset,
			"updated_at":                time.Now(),
		}).Error; err != nil {
		return nil, s.parseGormError(err)
	}
	if err := txDB.First(&budget, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &budget, nil
}

// DeleteBudget deletes a budget from the database.
func (s *RDBConfigStore) DeleteBudget(ctx context.Context, id string, tx ...*gorm.DB) error {
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.DeleteBudget(ctx, id, transaction)
		})
	}

	txDB := tx[0]
	var existing tables.TableBudget
	if err := txDB.WithContext(ctx).First(&existing, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	if err := lockBudgetOwner(ctx, txDB, existing); err != nil {
		return err
	}
	if err := dbForUpdate(txDB.WithContext(ctx)).First(&existing, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	if err := txDB.WithContext(ctx).Delete(&tables.TableBudget{}, "id = ?", id).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// UpdateBudgetUsage updates only the current_usage field of a budget.
// Uses SkipHooks to avoid triggering BeforeSave validation since we're only updating usage.
func (s *RDBConfigStore) UpdateBudgetUsage(ctx context.Context, id string, currentUsage float64, tx ...*gorm.DB) error {
	db := s.DB()
	if len(tx) > 0 && tx[0] != nil {
		db = tx[0]
	}
	result := db.WithContext(ctx).
		Session(&gorm.Session{SkipHooks: true}).
		Model(&tables.TableBudget{}).
		Where("id = ?", id).
		Update("current_usage", currentUsage)
	if result.Error != nil {
		return s.parseGormError(result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateRateLimitUsage updates only the usage fields of a rate limit.
// Uses SkipHooks to avoid triggering BeforeSave validation since we're only updating usage.
func (s *RDBConfigStore) UpdateRateLimitUsage(ctx context.Context, id string, tokenCurrentUsage int64, requestCurrentUsage int64, tx ...*gorm.DB) error {
	db := s.DB()
	if len(tx) > 0 && tx[0] != nil {
		db = tx[0]
	}
	result := db.WithContext(ctx).
		Session(&gorm.Session{SkipHooks: true}).
		Model(&tables.TableRateLimit{}).
		Where("id = ?", id).
		Updates(map[string]interface{}{
			"token_current_usage":   tokenCurrentUsage,
			"request_current_usage": requestCurrentUsage,
		})
	if result.Error != nil {
		return s.parseGormError(result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// loadRoutingRulesOrdered loads routing rules with Targets preloaded, using consistent ordering:
// rules by priority ASC, created_at DESC, id ASC; targets by weight DESC for deterministic ordering.
func (s *RDBConfigStore) loadRoutingRulesOrdered(ctx context.Context, dest *[]tables.TableRoutingRule, scopes ...func(*gorm.DB) *gorm.DB) error {
	// ScopedDB, not DB: routing-rule reads must honor any row-visibility
	// QueryScope stashed on ctx (the enterprise DAC wrapper attaches one for
	// request-driven reads). Background callers (engine bootstrap, cache
	// reloads on context.Background) carry no scope and stay unfiltered.
	q := s.ScopedDB(ctx).
		Preload("Targets", func(db *gorm.DB) *gorm.DB {
			return db.Order("weight DESC").
				Order("COALESCE(provider, '') ASC").
				Order("COALESCE(model, '') ASC").
				Order("COALESCE(key_id, '') ASC")
		}).
		Order("priority ASC, created_at DESC, id ASC")
	for _, scope := range scopes {
		q = scope(q)
	}
	return q.Find(dest).Error
}

// GetRoutingRules retrieves all routing rules from the database.
func (s *RDBConfigStore) GetRoutingRules(ctx context.Context) ([]tables.TableRoutingRule, error) {
	var rules []tables.TableRoutingRule
	if err := s.loadRoutingRulesOrdered(ctx, &rules); err != nil {
		return nil, err
	}
	return rules, nil
}

// GetRoutingRulesPaginated retrieves routing rules with pagination and optional search filtering.
//
// When ctx carries a QueryScope, the query is narrowed to rules the
// caller is allowed to see; rules with scope='global' are always
// included by the scope builder.
func (s *RDBConfigStore) GetRoutingRulesPaginated(ctx context.Context, params RoutingRulesQueryParams) ([]tables.TableRoutingRule, int64, error) {
	baseQuery := s.ScopedDB(ctx).Model(&tables.TableRoutingRule{})

	if params.Search != "" {
		search := "%" + strings.ToLower(params.Search) + "%"
		baseQuery = baseQuery.Where("LOWER(name) LIKE ? OR LOWER(cel_expression) LIKE ?", search, search)
	}

	var totalCount int64
	if err := baseQuery.Count(&totalCount).Error; err != nil {
		return nil, 0, err
	}

	limit := params.Limit
	offset := params.Offset

	if limit <= 0 {
		limit = 25
	} else if limit > 100 {
		limit = 100
	}

	if offset < 0 {
		offset = 0
	}

	var rules []tables.TableRoutingRule
	if err := baseQuery.
		Preload("Targets", func(db *gorm.DB) *gorm.DB {
			return db.Order("weight DESC").
				Order("COALESCE(provider, '') ASC").
				Order("COALESCE(model, '') ASC").
				Order("COALESCE(key_id, '') ASC")
		}).
		Order("priority ASC, created_at DESC, id ASC").
		Offset(offset).
		Limit(limit).
		Find(&rules).Error; err != nil {
		return nil, 0, err
	}
	return rules, totalCount, nil
}

// GetRoutingRulesByScope retrieves routing rules by scope and scope ID, ordered by priority ASC.
func (s *RDBConfigStore) GetRoutingRulesByScope(ctx context.Context, scope string, scopeID string) ([]tables.TableRoutingRule, error) {
	if scope != "global" && scopeID == "" {
		return nil, fmt.Errorf("scopeID is required for non-global scope %q", scope)
	}
	var rules []tables.TableRoutingRule
	scopeFilter := func(q *gorm.DB) *gorm.DB {
		if scope == "global" {
			return q.Where("scope = ?", "global")
		}
		return q.Where("scope = ? AND scope_id = ?", scope, scopeID)
	}
	if err := s.loadRoutingRulesOrdered(ctx, &rules, scopeFilter, func(q *gorm.DB) *gorm.DB {
		return q.Where("enabled = ?", true)
	}); err != nil {
		return nil, err
	}
	return rules, nil
}

// GetRoutingRule retrieves a specific routing rule by ID.
func (s *RDBConfigStore) GetRoutingRule(ctx context.Context, id string) (*tables.TableRoutingRule, error) {
	var rules []tables.TableRoutingRule
	if err := s.loadRoutingRulesOrdered(ctx, &rules, func(q *gorm.DB) *gorm.DB {
		return q.Where("id = ?", id)
	}); err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, ErrNotFound
	}
	return &rules[0], nil
}

// GetRedactedRoutingRules retrieves redacted routing rules from the database.
func (s *RDBConfigStore) GetRedactedRoutingRules(ctx context.Context, ids []string) ([]tables.TableRoutingRule, error) {
	var routingRules []tables.TableRoutingRule

	if len(ids) > 0 {
		err := s.DB().WithContext(ctx).Select("id, name, description, enabled").Where("id IN ?", ids).Find(&routingRules).Error
		if err != nil {
			return nil, err
		}
	} else {
		err := s.DB().WithContext(ctx).Select("id, name, description, enabled").Find(&routingRules).Error
		if err != nil {
			return nil, err
		}
	}
	return routingRules, nil
}

// CreateRoutingRule creates a new routing rule in the database.
func (s *RDBConfigStore) CreateRoutingRule(ctx context.Context, rule *tables.TableRoutingRule, tx ...*gorm.DB) error {
	database := s.DB()
	if len(tx) > 0 && tx[0] != nil {
		database = tx[0]
	}

	// Validate scopeID is required for non-global scope
	if rule.Scope != "" && rule.Scope != "global" && rule.ScopeID == nil {
		return fmt.Errorf("scopeID is required for non-global scope '%s'", rule.Scope)
	}

	// Check if there is already a routing rule with the same priority for the same scope+scopeID
	var count int64
	query := database.WithContext(ctx).Where("scope = ? AND priority = ? AND id != ?", rule.Scope, rule.Priority, rule.ID)
	if rule.ScopeID != nil {
		query = query.Where("scope_id = ?", *rule.ScopeID)
	} else {
		query = query.Where("scope_id IS NULL")
	}
	if err := query.Model(&tables.TableRoutingRule{}).Count(&count).Error; err != nil {
		return s.parseGormError(err)
	}
	if count > 0 {
		if rule.ScopeID != nil {
			return fmt.Errorf("routing rule with priority %d %w for scope '%s' with scope ID '%s'", rule.Priority, ErrRoutingRulePriorityTaken, rule.Scope, *rule.ScopeID)
		}
		return fmt.Errorf("routing rule with priority %d %w for scope '%s'", rule.Priority, ErrRoutingRulePriorityTaken, rule.Scope)
	}

	return s.parseGormError(database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		targets := rule.Targets
		rule.Targets = nil
		if err := tx.Omit("Targets").Create(rule).Error; err != nil {
			return err
		}
		rule.Targets = targets

		for i := range rule.Targets {
			rule.Targets[i].RuleID = rule.ID
			if err := tx.Create(&rule.Targets[i]).Error; err != nil {
				return err
			}
		}
		return nil
	}))
}

// UpdateRoutingRule updates an existing routing rule in the database.
// It enforces the same unique-priority-per-scope invariant as CreateRoutingRule.
func (s *RDBConfigStore) UpdateRoutingRule(ctx context.Context, rule *tables.TableRoutingRule, tx ...*gorm.DB) error {
	database := s.DB()
	if len(tx) > 0 && tx[0] != nil {
		database = tx[0]
	}

	// Validate scopeID is required for non-global scope
	if rule.Scope != "" && rule.Scope != "global" && rule.ScopeID == nil {
		return fmt.Errorf("scopeID is required for non-global scope '%s'", rule.Scope)
	}

	return s.parseGormError(database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing tables.TableRoutingRule
		if err := dbForUpdate(tx).First(&existing, "id = ?", rule.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}

		// Check for another tables.TableRoutingRule with same scope (Scope + ScopeID) and Priority but different ID
		var count int64
		query := tx.Where("scope = ? AND priority = ? AND id != ?", rule.Scope, rule.Priority, rule.ID)
		if rule.ScopeID != nil {
			query = query.Where("scope_id = ?", *rule.ScopeID)
		} else {
			query = query.Where("scope_id IS NULL")
		}
		if err := query.Model(&tables.TableRoutingRule{}).Count(&count).Error; err != nil {
			return s.parseGormError(err)
		}
		if count > 0 {
			if rule.ScopeID != nil {
				return fmt.Errorf("routing rule with priority %d %w for scope '%s' with scope ID '%s'", rule.Priority, ErrRoutingRulePriorityTaken, rule.Scope, *rule.ScopeID)
			}
			return fmt.Errorf("routing rule with priority %d %w for scope '%s'", rule.Priority, ErrRoutingRulePriorityTaken, rule.Scope)
		}

		targets := rule.Targets
		rule.Targets = nil
		// created_at is immutable: Save writes every column, so a caller passing a rule it
		// didn't read from the DB would otherwise zero it out. Always keep the persisted value.
		rule.CreatedAt = existing.CreatedAt
		// enabled is NOT NULL with a DB default, and Save writes a nil Enabled as NULL.
		// An omitted value keeps the persisted state.
		if rule.Enabled == nil {
			rule.Enabled = existing.Enabled
		}
		if err := tx.Omit("Targets").Save(rule).Error; err != nil {
			return err
		}
		rule.Targets = targets

		if err := tx.Where("rule_id = ?", rule.ID).Delete(&tables.TableRoutingTarget{}).Error; err != nil {
			return err
		}
		for i := range rule.Targets {
			rule.Targets[i].RuleID = rule.ID
			if err := tx.Create(&rule.Targets[i]).Error; err != nil {
				return err
			}
		}
		return nil
	}))
}

// SyncRoutingRules applies a batch of routing rule creates and updates atomically, deferring the
// unique-priority-per-scope check until every rule is written. This lets a valid permutation (e.g.
// swapping two rules' priorities) succeed despite a transient intermediate collision, while a
// genuine end-state duplicate still errors. Used by config-file reloads that apply many rules at once.
func (s *RDBConfigStore) SyncRoutingRules(ctx context.Context, toAdd []tables.TableRoutingRule, toUpdate []tables.TableRoutingRule, tx ...*gorm.DB) error {
	database := s.DB()
	if len(tx) > 0 && tx[0] != nil {
		database = tx[0]
	}

	// Validate scopeID is required for non-global scope (same guard as CreateRoutingRule/UpdateRoutingRule).
	for _, list := range [][]tables.TableRoutingRule{toAdd, toUpdate} {
		for i := range list {
			if list[i].Scope != "" && list[i].Scope != "global" && list[i].ScopeID == nil {
				return fmt.Errorf("scopeID is required for non-global scope '%s'", list[i].Scope)
			}
		}
	}

	type scopeRef struct {
		scope   string
		scopeID *string
	}

	return s.parseGormError(database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		seen := make(map[string]bool)
		scopes := make([]scopeRef, 0, len(toAdd)+len(toUpdate))
		record := func(rule *tables.TableRoutingRule) {
			key := rule.Scope
			if rule.ScopeID != nil {
				key += "\x00" + *rule.ScopeID
			}
			if !seen[key] {
				seen[key] = true
				scopes = append(scopes, scopeRef{scope: rule.Scope, scopeID: rule.ScopeID})
			}
		}

		putTargets := func(rule *tables.TableRoutingRule) error {
			for i := range rule.Targets {
				rule.Targets[i].RuleID = rule.ID
				if err := tx.Create(&rule.Targets[i]).Error; err != nil {
					return err
				}
			}
			return nil
		}

		// Insert new rules.
		for i := range toAdd {
			rule := &toAdd[i]
			targets := rule.Targets
			rule.Targets = nil
			if err := tx.Omit("Targets").Create(rule).Error; err != nil {
				return err
			}
			rule.Targets = targets
			if err := putTargets(rule); err != nil {
				return err
			}
			record(rule)
		}

		// Update changed rules and replace their targets.
		for i := range toUpdate {
			rule := &toUpdate[i]
			var existing tables.TableRoutingRule
			if err := dbForUpdate(tx).First(&existing, "id = ?", rule.ID).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrNotFound
				}
				return err
			}
			targets := rule.Targets
			rule.Targets = nil
			// Rules in toUpdate come from config.json, which carries no created_at; GORM's Save
			// selects every column, so an unset CreatedAt would overwrite the original insert
			// timestamp with the zero time. Carry the persisted value forward.
			rule.CreatedAt = existing.CreatedAt
			// config.json rules usually omit "enabled"; keep the persisted value instead of
			// letting Save write NULL into the NOT NULL column.
			if rule.Enabled == nil {
				rule.Enabled = existing.Enabled
			}
			if err := tx.Omit("Targets").Save(rule).Error; err != nil {
				return err
			}
			rule.Targets = targets
			if err := tx.Where("rule_id = ?", rule.ID).Delete(&tables.TableRoutingTarget{}).Error; err != nil {
				return err
			}
			if err := putTargets(rule); err != nil {
				return err
			}
			record(rule)
		}

		// Deferred invariant: no two rules may share a priority within a scope's final state.
		for _, sc := range scopes {
			q := tx.Model(&tables.TableRoutingRule{}).Where("scope = ?", sc.scope)
			if sc.scopeID != nil {
				q = q.Where("scope_id = ?", *sc.scopeID)
			} else {
				q = q.Where("scope_id IS NULL")
			}
			var dup []int
			if err := q.Group("priority").Having("COUNT(*) > 1").Pluck("priority", &dup).Error; err != nil {
				return err
			}
			if len(dup) > 0 {
				if sc.scopeID != nil {
					return fmt.Errorf("routing rule with priority %d already exists for scope '%s' with scopeID '%s'", dup[0], sc.scope, *sc.scopeID)
				}
				return fmt.Errorf("routing rule with priority %d already exists for scope '%s'", dup[0], sc.scope)
			}
		}
		return nil
	}))
}

// DeleteRoutingRule deletes a routing rule and its targets from the database.
func (s *RDBConfigStore) DeleteRoutingRule(ctx context.Context, id string, tx ...*gorm.DB) error {
	database := s.DB()
	if len(tx) > 0 && tx[0] != nil {
		database = tx[0]
	}

	return s.parseGormError(database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing tables.TableRoutingRule
		if err := dbForUpdate(tx).First(&existing, "id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := tx.Where("rule_id = ?", id).Delete(&tables.TableRoutingTarget{}).Error; err != nil {
			return err
		}
		result := tx.Delete(&tables.TableRoutingRule{}, "id = ?", id)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	}))
}

// GetModelConfigs retrieves all model configs from the database.
func (s *RDBConfigStore) GetModelConfigs(ctx context.Context) ([]tables.TableModelConfig, error) {
	var allModelConfigs []tables.TableModelConfig
	lastID := ""
	hasCursor := false

	for {
		var page []tables.TableModelConfig
		query := s.DB().WithContext(ctx).Preload("Budgets").Preload("Budget").Preload("RateLimit")
		if hasCursor {
			query = query.Where("governance_model_configs.id > ?", lastID)
		}
		if err := query.
			Order("governance_model_configs.id ASC").
			Limit(modelConfigInternalPageSize).
			Find(&page).Error; err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return allModelConfigs, nil
		}
		allModelConfigs = append(allModelConfigs, page...)
		lastID = page[len(page)-1].ID
		hasCursor = true
		if len(page) < modelConfigInternalPageSize {
			return allModelConfigs, nil
		}
	}
}

// modelConfigScopeIDChunkSize is how many scope ids GetModelConfigsByScopeAndScopeIDs
// reads per statement. The id list itself binds as one parameter; the chunk bounds
// how many configs one read can return before its relations are loaded.
var modelConfigScopeIDChunkSize = 1000

// modelConfigPreloadBatchSize is the most model configs whose relations are loaded
// in one statement, keeping each relation read well under the 30,000-parameter
// budget even on dialects where the id list is not bound as a single parameter.
var modelConfigPreloadBatchSize = 10000

// GetModelConfigsByScopeAndScopeIDs retrieves model configs for a specific scope limited to the given scope IDs.
// Pass tx to read through a caller's transaction and see its uncommitted writes.
func (s *RDBConfigStore) GetModelConfigsByScopeAndScopeIDs(ctx context.Context, scope string, scopeIDs []string, tx ...*gorm.DB) ([]tables.TableModelConfig, error) {
	if len(scopeIDs) == 0 {
		return nil, nil
	}
	// A repeated id would land in two chunks and return its configs twice; one
	// IN list over the whole slice returned each row once. Keep first-seen order.
	seen := make(map[string]struct{}, len(scopeIDs))
	unique := make([]string, 0, len(scopeIDs))
	for _, id := range scopeIDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	scopeIDs = unique
	txDB := s.DB()
	if len(tx) > 0 && tx[0] != nil {
		txDB = tx[0]
	}
	db := txDB.WithContext(ctx)
	var modelConfigs []tables.TableModelConfig
	// scope_id binds as one parameter per chunk, and the relations load below in
	// batches, so neither the id list nor a large result can push a statement past
	// the dialect's bind-parameter limit. Rows keep the per-chunk read order.
	for start := 0; start < len(scopeIDs); start += modelConfigScopeIDChunkSize {
		end := min(start+modelConfigScopeIDChunkSize, len(scopeIDs))
		inClause, arg := queryscope.InStrings(db, "scope_id", scopeIDs[start:end])
		var chunk []tables.TableModelConfig
		if err := db.Where("scope = ?", scope).Where(inClause, arg).Find(&chunk).Error; err != nil {
			return nil, err
		}
		for bStart := 0; bStart < len(chunk); bStart += modelConfigPreloadBatchSize {
			bEnd := min(bStart+modelConfigPreloadBatchSize, len(chunk))
			if err := loadModelConfigRelations(db, chunk[bStart:bEnd]); err != nil {
				return nil, err
			}
		}
		modelConfigs = append(modelConfigs, chunk...)
	}
	return modelConfigs, nil
}

// loadModelConfigRelations attaches Budgets, the legacy Budget and RateLimit to
// configs, exactly what Preload("Budgets").Preload("Budget").Preload("RateLimit")
// loads, but binds each id list as a single parameter instead of one per row. It
// then re-applies the calendar-alignment stamp AfterFind would have applied had
// the budgets been present when the configs were scanned.
func loadModelConfigRelations(db *gorm.DB, configs []tables.TableModelConfig) error {
	if len(configs) == 0 {
		return nil
	}
	configIDs := make([]string, 0, len(configs))
	var budgetIDs, rateLimitIDs []string
	seenBudget := make(map[string]struct{})
	seenRateLimit := make(map[string]struct{})
	for i := range configs {
		configIDs = append(configIDs, configs[i].ID)
		if id := configs[i].BudgetID; id != nil {
			if _, ok := seenBudget[*id]; !ok {
				seenBudget[*id] = struct{}{}
				budgetIDs = append(budgetIDs, *id)
			}
		}
		if id := configs[i].RateLimitID; id != nil {
			if _, ok := seenRateLimit[*id]; !ok {
				seenRateLimit[*id] = struct{}{}
				rateLimitIDs = append(rateLimitIDs, *id)
			}
		}
	}

	var owned []tables.TableBudget
	if err := db.Where(queryscope.InStrings(db, "model_config_id", configIDs)).Find(&owned).Error; err != nil {
		return err
	}
	ownedByConfig := make(map[string][]tables.TableBudget, len(configs))
	for _, b := range owned {
		if b.ModelConfigID != nil {
			ownedByConfig[*b.ModelConfigID] = append(ownedByConfig[*b.ModelConfigID], b)
		}
	}

	legacyBudgets := make(map[string]tables.TableBudget, len(budgetIDs))
	if len(budgetIDs) > 0 {
		var rows []tables.TableBudget
		if err := db.Where(queryscope.InStrings(db, "id", budgetIDs)).Find(&rows).Error; err != nil {
			return err
		}
		for _, b := range rows {
			legacyBudgets[b.ID] = b
		}
	}

	rateLimits := make(map[string]tables.TableRateLimit, len(rateLimitIDs))
	if len(rateLimitIDs) > 0 {
		var rows []tables.TableRateLimit
		if err := db.Where(queryscope.InStrings(db, "id", rateLimitIDs)).Find(&rows).Error; err != nil {
			return err
		}
		for _, rl := range rows {
			rateLimits[rl.ID] = rl
		}
	}

	for i := range configs {
		mc := &configs[i]
		mc.Budgets = ownedByConfig[mc.ID]
		if mc.BudgetID != nil {
			if b, ok := legacyBudgets[*mc.BudgetID]; ok {
				mc.Budget = &b
			}
		}
		if mc.RateLimitID != nil {
			if rl, ok := rateLimits[*mc.RateLimitID]; ok {
				mc.RateLimit = &rl
			}
		}
		tables.StampCalendarAlignment(mc.CalendarAligned, mc.Budgets, nil)
	}
	return nil
}

// GetProviderGovernanceModelConfigs retrieves the wildcard "all models on a provider" configs
func (s *RDBConfigStore) GetProviderGovernanceModelConfigs(ctx context.Context) ([]tables.TableModelConfig, error) {
	var modelConfigs []tables.TableModelConfig
	if err := s.DB().WithContext(ctx).
		Preload("Budgets").Preload("Budget").Preload("RateLimit").
		Where("scope = ? AND model_name = ? AND provider IS NOT NULL", tables.ModelConfigScopeGlobal, tables.ModelConfigAllModels).
		Find(&modelConfigs).Error; err != nil {
		return nil, err
	}
	return modelConfigs, nil
}

// GetModelConfigsPaginated retrieves model configs with pagination, filtering, and search support.
func (s *RDBConfigStore) GetModelConfigsPaginated(ctx context.Context, params ModelConfigsQueryParams) ([]tables.TableModelConfig, int64, error) {
	baseQuery := s.ScopedDB(ctx).Model(&tables.TableModelConfig{})

	if params.Search != "" {
		search := "%" + strings.ToLower(params.Search) + "%"
		baseQuery = baseQuery.Where("LOWER(model_name) LIKE ?", search)
	}
	// Scope (deprecated, single) and Scopes are OR-ed together, so a caller still
	// setting only Scope filters exactly as it always did.
	if scopes := params.effectiveScopes(); len(scopes) == 1 {
		baseQuery = baseQuery.Where("scope = ?", scopes[0])
	} else if len(scopes) > 1 {
		baseQuery = baseQuery.Where("scope IN ?", scopes)
	}
	if params.ScopeID != "" {
		baseQuery = baseQuery.Where("scope_id = ?", params.ScopeID)
	}
	if params.Provider != "" {
		baseQuery = baseQuery.Where("provider = ?", params.Provider)
	}

	var totalCount int64
	if err := baseQuery.Count(&totalCount).Error; err != nil {
		return nil, 0, err
	}

	limit := params.Limit
	offset := params.Offset

	if limit <= 0 {
		limit = 25
	} else if limit > 100 {
		limit = 100
	}

	if offset < 0 {
		offset = 0
	}

	var modelConfigs []tables.TableModelConfig
	if err := baseQuery.
		Preload("Budgets").
		Preload("Budget").
		Preload("RateLimit").
		Order("created_at DESC, id DESC").
		Offset(offset).
		Limit(limit).
		Find(&modelConfigs).Error; err != nil {
		return nil, 0, err
	}
	return modelConfigs, totalCount, nil
}

// GetModelConfig retrieves a specific model config from the database by its identity:
// scope, optional scope ID, model name, and optional provider.
func (s *RDBConfigStore) GetModelConfig(ctx context.Context, scope string, scopeID *string, modelName string, provider *string) (*tables.TableModelConfig, error) {
	var modelConfig tables.TableModelConfig
	query := s.DB().WithContext(ctx).Where("model_name = ?", modelName).Where("scope = ?", scope)
	if scopeID != nil {
		query = query.Where("scope_id = ?", *scopeID)
	} else {
		query = query.Where("scope_id IS NULL")
	}
	if provider != nil {
		query = query.Where("provider = ?", *provider)
	} else {
		query = query.Where("provider IS NULL")
	}
	if err := query.Preload("Budgets").Preload("Budget").Preload("RateLimit").First(&modelConfig).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &modelConfig, nil
}

// GetModelConfigByID retrieves a specific model config from the database by ID. It reads
// through ScopedDB, so a caller whose row visibility is restricted reads a config it may not
// see as not found.
func (s *RDBConfigStore) GetModelConfigByID(ctx context.Context, id string) (*tables.TableModelConfig, error) {
	var modelConfig tables.TableModelConfig
	if err := s.ScopedDB(ctx).Preload("Budgets").Preload("Budget").Preload("RateLimit").First(&modelConfig, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &modelConfig, nil
}

// deleteModelConfigsWhere deletes every model config matching the given condition,
// along with the budgets and rate-limits those configs own. It is the single source
// of truth for tearing down model configs when their owner (a virtual key, provider,
// user, …) is removed — every owner-delete path funnels through here so the cleanup,
// including the easy-to-forget preload of multi-budget rows, lives in exactly one place.
//
// Owned budgets are gathered from BOTH the active Budgets slice (owned via
// ModelConfigID) and the legacy single BudgetID column. Deletion happens by
// snapshotted ID rather than re-running the WHERE clause, so a concurrent
// CreateModelConfig that lands between the snapshot and the delete can't leave its
// owned budget/rate-limit rows dangling. The snapshot is taken FOR UPDATE
// (mirroring DeleteModelConfig) so a concurrent UpdateModelConfig can't swap
// BudgetID/RateLimitID after the IDs are collected; rows are locked in stable id
// order to keep concurrent deleters deadlock-free. Configs are removed before
// their owned rows, matching DeleteModelConfig's order.
//
// A provider can own more model configs than a statement can bind parameters for,
// so every id list below travels as one bound parameter (queryscope.InStrings).
// That is also why the owned budgets are loaded by an explicit query instead of
// Preload("Budgets"), which would bind one parameter per config. The query runs
// unlocked, exactly like the preload it replaces.
func (s *RDBConfigStore) deleteModelConfigsWhere(ctx context.Context, txDB *gorm.DB, query string, args ...any) error {
	var modelConfigs []tables.TableModelConfig
	if err := dbForUpdate(txDB.WithContext(ctx)).Order("id").Where(query, args...).Find(&modelConfigs).Error; err != nil {
		return err
	}
	if len(modelConfigs) == 0 {
		return nil
	}

	mcIDs := make([]string, 0, len(modelConfigs))
	budgetIDs := make([]string, 0, len(modelConfigs))
	rateLimitIDs := make([]string, 0, len(modelConfigs))
	for i := range modelConfigs {
		mcIDs = append(mcIDs, modelConfigs[i].ID)
	}
	var ownedBudgetIDs []string
	if err := txDB.WithContext(ctx).Model(&tables.TableBudget{}).
		Where(queryscope.InStrings(txDB, "model_config_id", mcIDs)).
		Pluck("id", &ownedBudgetIDs).Error; err != nil {
		return err
	}
	budgetIDs = append(budgetIDs, ownedBudgetIDs...)
	for i := range modelConfigs {
		if modelConfigs[i].BudgetID != nil {
			budgetIDs = append(budgetIDs, *modelConfigs[i].BudgetID)
		}
		if modelConfigs[i].RateLimitID != nil {
			rateLimitIDs = append(rateLimitIDs, *modelConfigs[i].RateLimitID)
		}
	}

	if err := txDB.WithContext(ctx).Where(queryscope.InStrings(txDB, "id", mcIDs)).Delete(&tables.TableModelConfig{}).Error; err != nil {
		return err
	}
	if len(budgetIDs) > 0 {
		if err := txDB.WithContext(ctx).Where(queryscope.InStrings(txDB, "id", budgetIDs)).Delete(&tables.TableBudget{}).Error; err != nil {
			return err
		}
	}
	if len(rateLimitIDs) > 0 {
		if err := txDB.WithContext(ctx).Where(queryscope.InStrings(txDB, "id", rateLimitIDs)).Delete(&tables.TableRateLimit{}).Error; err != nil {
			return err
		}
	}
	return nil
}

// DeleteModelConfigsForScope removes all model configs targeting a given scope owner
// (e.g. scope=virtual_key, scopeID=<vk id>) along with their owned budgets/rate-limits.
// Thin wrapper over deleteModelConfigsWhere for the scope/scope_id axis. Exported so
// out-of-package owner-delete paths (e.g. the enterprise user-deletion flow cleaning up
// scope=user configs) funnel through the same cleanup instead of reimplementing it.
func (s *RDBConfigStore) DeleteModelConfigsForScope(ctx context.Context, txDB *gorm.DB, scope, scopeID string) error {
	// The tx is required (not variadic) on purpose: this cleanup must be atomic
	// with the owner's delete. Guard against nil rather than falling back to
	// s.DB(), which would silently run the cleanup outside that transaction.
	if txDB == nil {
		return fmt.Errorf("DeleteModelConfigsForScope requires the owner-delete transaction, got nil tx")
	}
	return s.deleteModelConfigsWhere(ctx, txDB, "scope = ? AND scope_id = ?", scope, scopeID)
}

// GetModelConfigsForScope loads all model configs (and their Budgets/RateLimit) targeting a given
// scope owner within txDB. The tx is required, not variadic, for the same reason as
// DeleteModelConfigsForScope: a caller reading and writing the same scope owner in one transaction
// (e.g. tearing down and recreating its configs) needs to see its own uncommitted writes, which
// s.DB() outside the transaction cannot.
func (s *RDBConfigStore) GetModelConfigsForScope(ctx context.Context, txDB *gorm.DB, scope, scopeID string) ([]tables.TableModelConfig, error) {
	if txDB == nil {
		return nil, fmt.Errorf("GetModelConfigsForScope requires the caller's transaction, got nil tx")
	}
	var modelConfigs []tables.TableModelConfig
	if err := txDB.WithContext(ctx).Preload("Budgets").Preload("RateLimit").
		Where("scope = ? AND scope_id = ?", scope, scopeID).
		Find(&modelConfigs).Error; err != nil {
		return nil, err
	}
	return modelConfigs, nil
}

// CreateModelConfig creates a new model config in the database.
func (s *RDBConfigStore) CreateModelConfig(ctx context.Context, modelConfig *tables.TableModelConfig, tx ...*gorm.DB) error {
	// Locking the scope owner and inserting the config must be atomic, so wrap in a
	// transaction when the caller didn't supply one.
	if len(tx) == 0 || tx[0] == nil {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.CreateModelConfig(ctx, modelConfig, transaction)
		})
	}
	txDB := tx[0]

	// Serialize against deletion of the scope owner. A scoped config's scope_id carries
	// no FK, so without this a CreateModelConfig for scope=virtual_key could commit just
	// after a concurrent DeleteVirtualKey, leaving the config (and its owned budgets/
	// rate-limits) pointing at a virtual key that no longer exists. Locking the owner row
	// makes the two transactions mutually exclusive and surfaces an already-deleted owner
	// as ErrNotFound. Callers that create owner-scoped configs must create the owner first.
	if err := s.lockModelConfigScopeOwner(ctx, txDB, modelConfig); err != nil {
		return err
	}

	if err := txDB.WithContext(ctx).Create(modelConfig).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// lockModelConfigScopeOwner takes a FOR UPDATE lock on the row a scoped model config
// targets and confirms it still exists, returning ErrNotFound when the owner is gone.
// Global configs (no scope owner) are a no-op. Only scopes whose owner table lives in
// this store are locked; other scopes (e.g. the enterprise "user" scope, whose owner
// table is out of package) are the responsibility of their own create path. The lock is
// FOR UPDATE on Postgres and a plain existence check on SQLite (whose writer
// serialization already prevents the interleave), mirroring lockBudgetOwner.
//
// Provider-bound configs (any scope) are additionally serialized against
// DeleteProvider, which tears down every config matching the provider column. The
// provider row is optional — providers may be env-configured with no DB row — so
// absence is tolerated rather than treated as a missing owner.
func (s *RDBConfigStore) lockModelConfigScopeOwner(ctx context.Context, txDB *gorm.DB, mc *tables.TableModelConfig) error {
	if mc == nil {
		return nil
	}
	if mc.Provider != nil && *mc.Provider != "" {
		var provider tables.TableProvider
		if err := dbForUpdate(txDB.WithContext(ctx)).First(&provider, "name = ?", *mc.Provider).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
	}
	if mc.ScopeID == nil || *mc.ScopeID == "" {
		return nil
	}
	switch mc.Scope {
	case tables.ModelConfigScopeVirtualKey:
		var vk tables.TableVirtualKey
		if err := dbForUpdate(txDB.WithContext(ctx)).First(&vk, "id = ?", *mc.ScopeID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
	}
	return nil
}

// UpdateModelConfig updates a model config in the database.
func (s *RDBConfigStore) UpdateModelConfig(ctx context.Context, modelConfig *tables.TableModelConfig, tx ...*gorm.DB) error {
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.UpdateModelConfig(ctx, modelConfig, transaction)
		})
	}

	txDB := tx[0]
	if modelConfig.ID != "" {
		var existing tables.TableModelConfig
		if err := dbForUpdate(txDB.WithContext(ctx)).First(&existing, "id = ?", modelConfig.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		// created_at is immutable: Save writes every column, so a caller passing a row
		// it didn't read from the DB (e.g. a config.json reload) would zero it out.
		modelConfig.CreatedAt = existing.CreatedAt
	}
	// Omit associations: budgets (has-many via ModelConfigID) and rate-limit are managed
	// explicitly by callers. A cascading Save would otherwise clobber their usage counters.
	if err := txDB.WithContext(ctx).Omit(clause.Associations).Save(modelConfig).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// UpdateModelConfigs updates multiple model configs in the database.
func (s *RDBConfigStore) UpdateModelConfigs(ctx context.Context, modelConfigs []*tables.TableModelConfig, tx ...*gorm.DB) error {
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.UpdateModelConfigs(ctx, modelConfigs, transaction)
		})
	}

	txDB := tx[0]
	sortedModelConfigs := append([]*tables.TableModelConfig(nil), modelConfigs...)
	sort.Slice(sortedModelConfigs, func(i, j int) bool { return sortedModelConfigs[i].ID < sortedModelConfigs[j].ID })
	for _, mc := range sortedModelConfigs {
		if err := s.UpdateModelConfig(ctx, mc, txDB); err != nil {
			return err
		}
	}
	return nil
}

// DeleteModelConfig deletes a model config from the database.
func (s *RDBConfigStore) DeleteModelConfig(ctx context.Context, id string, tx ...*gorm.DB) error {
	if len(tx) == 0 || tx[0] == nil {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.DeleteModelConfig(ctx, id, transaction)
		})
	}

	txDB := tx[0]
	// Fetch the model config with its owned budgets to collect all IDs to clean up.
	var modelConfig tables.TableModelConfig
	if err := dbForUpdate(txDB.WithContext(ctx)).Preload("Budgets").First(&modelConfig, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	// Collect budget IDs from both the Budgets slice (active path, owned via ModelConfigID)
	// and the legacy single BudgetID column.
	budgetIDs := make([]string, 0, len(modelConfig.Budgets)+1)
	for i := range modelConfig.Budgets {
		budgetIDs = append(budgetIDs, modelConfig.Budgets[i].ID)
	}
	if modelConfig.BudgetID != nil {
		budgetIDs = append(budgetIDs, *modelConfig.BudgetID)
	}
	rateLimitID := modelConfig.RateLimitID
	// Delete the model config first
	if err := txDB.WithContext(ctx).Delete(&tables.TableModelConfig{}, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return s.parseGormError(err)
	}
	// Delete the owned budgets (don't rely on FK cascade — it isn't applied to
	// pre-existing tables on all dialects).
	if len(budgetIDs) > 0 {
		if err := txDB.WithContext(ctx).Delete(&tables.TableBudget{}, "id IN ?", budgetIDs).Error; err != nil {
			return err
		}
	}
	// Delete the rate limit if it exists
	if rateLimitID != nil {
		if err := txDB.WithContext(ctx).Delete(&tables.TableRateLimit{}, "id = ?", *rateLimitID).Error; err != nil {
			return err
		}
	}
	return nil
}

// GetGovernanceConfig retrieves the governance configuration from the database.
func (s *RDBConfigStore) GetGovernanceConfig(ctx context.Context) (*GovernanceConfig, error) {
	var virtualKeys []tables.TableVirtualKey
	var teams []tables.TableTeam
	var customers []tables.TableCustomer
	var budgets []tables.TableBudget
	var rateLimits []tables.TableRateLimit
	var modelConfigs []tables.TableModelConfig
	var providers []tables.TableProvider
	var routingRules []tables.TableRoutingRule
	var pricingOverrides []tables.TablePricingOverride
	var governanceConfigs []tables.TableGovernanceConfig

	loadedVKs, err := s.getGovernanceConfigVirtualKeys(ctx)
	if err != nil {
		return nil, err
	}
	virtualKeys = loadedVKs
	if err := s.DB().WithContext(ctx).
		Select(teamSelectWithVKCount).
		Find(&teams).Error; err != nil {
		return nil, err
	}
	if err := s.DB().WithContext(ctx).Find(&customers).Error; err != nil {
		return nil, err
	}
	if err := s.DB().WithContext(ctx).Find(&budgets).Error; err != nil {
		return nil, err
	}
	if err := s.DB().WithContext(ctx).Find(&rateLimits).Error; err != nil {
		return nil, err
	}
	if err := s.DB().WithContext(ctx).Find(&modelConfigs).Error; err != nil {
		return nil, err
	}
	if err := s.DB().WithContext(ctx).Find(&providers).Error; err != nil {
		return nil, err
	}
	if err := s.loadRoutingRulesOrdered(ctx, &routingRules); err != nil {
		return nil, err
	}
	if err := s.DB().WithContext(ctx).Find(&pricingOverrides).Error; err != nil {
		return nil, err
	}
	// Fetching governance config for username and password
	if err := s.DB().WithContext(ctx).Find(&governanceConfigs).Error; err != nil {
		return nil, err
	}
	// Check if any config is present
	if len(virtualKeys) == 0 && len(teams) == 0 && len(customers) == 0 && len(budgets) == 0 && len(rateLimits) == 0 && len(modelConfigs) == 0 && len(providers) == 0 && len(governanceConfigs) == 0 && len(routingRules) == 0 && len(pricingOverrides) == 0 {
		return nil, nil
	}
	var authConfig *AuthConfig
	var complexityAnalyzerConfig *ComplexityAnalyzerConfig
	var complexitySemanticConfig *complexitySemanticConfigRecord
	if len(governanceConfigs) > 0 {
		// Checking if username and password is present
		var username *string
		var password *string
		var isEnabled bool
		for _, entry := range governanceConfigs {
			switch entry.Key {
			case tables.ConfigAdminUsernameKey:
				username = bifrost.Ptr(entry.Value)
			case tables.ConfigAdminPasswordKey:
				password = bifrost.Ptr(entry.Value)
			case tables.ConfigIsAuthEnabledKey:
				isEnabled = entry.Value == "true"
			case tables.ConfigComplexityAnalyzerConfigKey:
				if strings.TrimSpace(entry.Value) == "" {
					continue
				}
				decoded, err := DecodeComplexityAnalyzerConfig([]byte(entry.Value))
				if err != nil {
					if s.logger != nil {
						s.logger.Warn("failed to load complexity analyzer config from governance_config: %v", err)
					}
					continue
				}
				complexityAnalyzerConfig = decoded
			case tables.ConfigComplexitySemanticConfigKey:
				if strings.TrimSpace(entry.Value) == "" {
					continue
				}
				decoded, err := decodeComplexitySemanticConfigRow([]byte(entry.Value))
				if err != nil {
					if s.logger != nil {
						s.logger.Warn("failed to load complexity semantic config from governance_config: %v", err)
					}
					continue
				}
				complexitySemanticConfig = decoded
			}
		}
		// Attached after the loop because governance_config rows arrive in no
		// particular order, so the semantic row may be read before the analyzer
		// row it belongs to. A semantic section that fails validation is dropped
		// rather than discarding the analyzer config with it, matching how a
		// bad analyzer row degrades to defaults instead of failing the load.
		// The analyzer row alone is not a usable config: it carries tier
		// boundaries, while the exemplars every classification needs live in the
		// semantic row. Without that row there is nothing to route with, so the
		// caller falls back to defaults exactly as it would on a fresh install.
		if complexityAnalyzerConfig != nil {
			combined := applyComplexitySemanticConfigRow(complexityAnalyzerConfig, complexitySemanticConfig)
			if combined == nil {
				complexityAnalyzerConfig = nil
			} else {
				normalized := combined.Normalized()
				if err := normalized.Validate(); err != nil {
					if s.logger != nil {
						s.logger.Warn("failed to apply complexity semantic config from governance_config: %v", err)
					}
					complexityAnalyzerConfig = nil
				} else {
					complexityAnalyzerConfig = &normalized
				}
			}
		}
		if username != nil && password != nil {
			authConfig = &AuthConfig{
				AdminUserName: schemas.NewSecretVar(*username),
				AdminPassword: schemas.NewSecretVar(*password),
				IsEnabled:     isEnabled,
			}
		}
	}
	return &GovernanceConfig{
		VirtualKeys:              virtualKeys,
		Teams:                    teams,
		Customers:                customers,
		Budgets:                  budgets,
		RateLimits:               rateLimits,
		ModelConfigs:             modelConfigs,
		Providers:                providers,
		RoutingRules:             routingRules,
		PricingOverrides:         pricingOverrides,
		AuthConfig:               authConfig,
		ComplexityAnalyzerConfig: complexityAnalyzerConfig,
	}, nil
}

// GetComplexityAnalyzerConfig retrieves the typed complexity analyzer config.
func (s *RDBConfigStore) GetComplexityAnalyzerConfig(ctx context.Context) (*ComplexityAnalyzerConfig, error) {
	return s.getComplexityAnalyzerConfigWithDB(ctx, s.DB())
}

// getComplexityAnalyzerConfigWithDB reads the analyzer row and the semantic row
// and combines them into one runtime config. Both are required: the analyzer row
// holds the tier boundaries and the semantic row holds the exemplars, and a
// config missing either cannot classify anything.
func (s *RDBConfigStore) getComplexityAnalyzerConfigWithDB(ctx context.Context, db *gorm.DB) (*ComplexityAnalyzerConfig, error) {
	if db == nil {
		db = s.DB()
	}

	var configEntry tables.TableGovernanceConfig
	err := db.WithContext(ctx).First(&configEntry, "key = ?", tables.ConfigComplexityAnalyzerConfigKey).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if strings.TrimSpace(configEntry.Value) == "" {
		return nil, nil
	}
	decoded, err := DecodeComplexityAnalyzerConfig([]byte(configEntry.Value))
	if err != nil {
		return nil, err
	}
	if decoded == nil {
		return nil, nil
	}

	semantic, err := s.getComplexitySemanticConfigWithDB(ctx, db)
	if err != nil {
		return nil, err
	}
	combined := applyComplexitySemanticConfigRow(decoded, semantic)
	if combined == nil {
		// Boundaries without exemplars cannot classify anything. Report it the
		// same way an absent analyzer row is reported, so callers fall back to
		// defaults rather than to a config that would fail validation.
		return nil, nil
	}

	normalized := combined.Normalized()
	if err := normalized.Validate(); err != nil {
		// The rows are present but do not combine into something this version
		// can run. That is an unreadable stored config, not an unreachable
		// store, and callers with defaults should degrade rather than fail.
		return nil, fmt.Errorf("%w: invalid complexity analyzer config: %w", ErrConfigUnreadable, err)
	}
	return &normalized, nil
}

// getComplexitySemanticConfigWithDB reads the semantic row. An absent or empty
// row means semantic classification is not configured, which is not an error.
func (s *RDBConfigStore) getComplexitySemanticConfigWithDB(ctx context.Context, db *gorm.DB) (*complexitySemanticConfigRecord, error) {
	if db == nil {
		db = s.DB()
	}

	var configEntry tables.TableGovernanceConfig
	err := db.WithContext(ctx).First(&configEntry, "key = ?", tables.ConfigComplexitySemanticConfigKey).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if strings.TrimSpace(configEntry.Value) == "" {
		return nil, nil
	}
	return decodeComplexitySemanticConfigRow([]byte(configEntry.Value))
}

// UpdateComplexityAnalyzerConfig normalizes, validates, and persists the typed analyzer config.
func (s *RDBConfigStore) UpdateComplexityAnalyzerConfig(ctx context.Context, config *ComplexityAnalyzerConfig, tx ...*gorm.DB) error {
	if config == nil {
		return fmt.Errorf("complexity analyzer config is nil")
	}

	normalized := config.Normalized()
	if err := normalized.Validate(); err != nil {
		return err
	}

	if len(tx) > 0 && tx[0] != nil {
		return s.updateComplexityAnalyzerConfigWithTx(ctx, &normalized, tx[0], true)
	}
	// Standalone calls own the transaction so the carry-over read below and the save
	// that follows it cannot interleave with a concurrent update.
	return s.DB().WithContext(ctx).Transaction(func(txDB *gorm.DB) error {
		return s.updateComplexityAnalyzerConfigWithTx(ctx, &normalized, txDB, true)
	})
}

// ResetComplexityAnalyzerConfig restores the tier boundaries and the phrase lists from
// defaults, invalidates the embedding fingerprint since it named the exemplars just
// replaced, and keeps every other section of the stored record — the semantic block,
// the section hashes — returning what was persisted.
//
// The read and the save share one transaction, and the read takes the same FOR UPDATE lock
// as updateComplexityAnalyzerConfigWithTx, for the same reason that function does: a reset
// that read the record outside the transaction would hold a stale copy of the very sections
// it means to preserve, and would write that stale copy back over an edit committed in
// between. Which sections count as "default" is the caller's to define, so they arrive as an
// argument rather than being rebuilt here.
func (s *RDBConfigStore) ResetComplexityAnalyzerConfig(ctx context.Context, defaults *ComplexityAnalyzerConfig) (*ComplexityAnalyzerConfig, error) {
	if defaults == nil {
		return nil, fmt.Errorf("complexity analyzer defaults are nil")
	}

	var persisted ComplexityAnalyzerConfig
	err := s.DB().WithContext(ctx).Transaction(func(txDB *gorm.DB) error {
		// Lock before reading, so the read is serialized against a concurrent save even
		// when no row exists yet — otherwise a first-time save committed in between is
		// invisible here and gets overwritten by the defaults below.
		if err := s.lockComplexityAnalyzerConfigRow(ctx, txDB); err != nil {
			return err
		}
		existing, err := s.getComplexityAnalyzerConfigWithDB(ctx, dbForUpdate(txDB))
		if err != nil && !errors.Is(err, ErrConfigUnreadable) {
			return err
		}
		// A stored config this version cannot read is the state reset exists to
		// recover from, so it resets to the defaults rather than reporting the
		// error back and leaving the operator with no way out.
		restored := *defaults
		if existing != nil {
			restored = *existing
			restored.TierBoundaries = defaults.TierBoundaries
			restored.Keywords = defaults.Keywords
			// The fingerprint records which exemplars were embedded, and the phrase
			// lists were just replaced, so it is stale.
			restored.EmbeddingFingerprint = ""
		}
		normalized := restored.Normalized()
		if err := normalized.Validate(); err != nil {
			return err
		}
		// carryOverFingerprint is false: an empty fingerprint here means the reset
		// just invalidated it, not that the caller forgot to set it, so the
		// still-stale row on disk must not be read back in to fill it.
		if err := s.updateComplexityAnalyzerConfigWithTx(ctx, &normalized, txDB, false); err != nil {
			return err
		}
		persisted = normalized
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &persisted, nil
}

// lockComplexityAnalyzerConfigRow takes the row lock that serializes the reset and update
// paths against each other, and is what makes that serialization hold on a fresh install.
//
// FOR UPDATE locks a row, so it locks nothing at all when the row does not exist yet: a
// reset and a first-time save would both read "absent" and proceed, letting the reset write
// its defaults over the semantic block the save had just committed — the one section reset
// exists to preserve. Inserting a placeholder first gives both transactions the same row to
// contend for. An empty value still reads back as "no stored config" through
// getComplexityAnalyzerConfigWithDB, so the placeholder changes no caller's view, and the
// caller's own save overwrites it before the transaction commits.
//
// Both rows are locked, always analyzer first, because a save and a reset each
// write both and would otherwise be free to take them in opposite orders.
//
// txDB must be a transaction, otherwise the lock is released before the caller can use it.
func (s *RDBConfigStore) lockComplexityAnalyzerConfigRow(ctx context.Context, txDB *gorm.DB) error {
	for _, key := range []string{
		tables.ConfigComplexityAnalyzerConfigKey,
		tables.ConfigComplexitySemanticConfigKey,
	} {
		if err := txDB.WithContext(ctx).
			Clauses(clause.OnConflict{DoNothing: true}).
			Create(&tables.TableGovernanceConfig{Key: key}).Error; err != nil {
			return err
		}
		var row tables.TableGovernanceConfig
		if err := dbForUpdate(txDB.WithContext(ctx)).
			First(&row, "key = ?", key).Error; err != nil {
			return err
		}
	}
	return nil
}

// readComplexityCarryOverWithDB reads the section hashes and the embedding fingerprint
// off the two stored rows, for a save that arrives without them.
//
// It reads the rows rather than going through getComplexityAnalyzerConfigWithDB, which
// reports "nothing stored" whenever the analyzer row is absent or blank: that is what a
// freshly inserted lock placeholder looks like, and what an installation whose only write
// was the exemplar backfill migration looks like, and in both the semantic row still holds
// the keyword hashes and the fingerprint. An unreadable row is treated as no previous
// state for the same reason: bookkeeping this version cannot decode must not stop a valid
// config from being saved over it.
func (s *RDBConfigStore) readComplexityCarryOverWithDB(ctx context.Context, db *gorm.DB) (ComplexityAnalyzerConfigHashes, string, error) {
	var hashes ComplexityAnalyzerConfigHashes

	// The tier-boundaries hash is the analyzer row's; every other hash, and the
	// fingerprint, belong to the semantic row.
	var analyzerEntry tables.TableGovernanceConfig
	switch err := db.WithContext(ctx).First(&analyzerEntry, "key = ?", tables.ConfigComplexityAnalyzerConfigKey).Error; {
	case err == nil:
		if strings.TrimSpace(analyzerEntry.Value) != "" {
			decoded, err := DecodeComplexityAnalyzerConfig([]byte(analyzerEntry.Value))
			if err != nil && !errors.Is(err, ErrConfigUnreadable) {
				return hashes, "", err
			}
			if decoded != nil {
				hashes.TierBoundaries = decoded.ConfigHashes.TierBoundaries
			}
		}
	case errors.Is(err, gorm.ErrRecordNotFound), errors.Is(err, ErrNotFound):
	default:
		return hashes, "", err
	}

	semanticRow, err := s.getComplexitySemanticConfigWithDB(ctx, db)
	if err != nil {
		if errors.Is(err, ErrConfigUnreadable) {
			return hashes, "", nil
		}
		return hashes, "", err
	}
	if semanticRow == nil {
		return hashes, "", nil
	}
	hashes.SimpleKeywords = semanticRow.ConfigHashes.SimpleKeywords
	hashes.MediumKeywords = semanticRow.ConfigHashes.MediumKeywords
	hashes.ComplexKeywords = semanticRow.ConfigHashes.ComplexKeywords
	hashes.SemanticSettings = semanticRow.ConfigHashes.SemanticSettings
	hashes.ClassifierSettings = semanticRow.ConfigHashes.ClassifierSettings
	hashes.DecisionSettings = semanticRow.ConfigHashes.DecisionSettings
	hashes.LLMSettings = semanticRow.ConfigHashes.LLMSettings
	hashes.SessionSettings = semanticRow.ConfigHashes.SessionSettings
	hashes.SuggestionsSettings = semanticRow.ConfigHashes.SuggestionsSettings
	return hashes, semanticRow.EmbeddingFingerprint, nil
}

// updateComplexityAnalyzerConfigWithTx carries over ConfigHashes and EmbeddingFingerprint
// from the stored config when the incoming one omits them, then persists the result. The
// row is locked first (a plain read on SQLite, whose writer serialization already prevents
// the interleave) so a concurrent updater cannot save a stale copy of either field between
// this read and the save. txDB must be a transaction.
//
// carryOverFingerprint gates only the fingerprint half of the carry-over: an empty
// ConfigHashes always reads the stored hashes back in, but an empty fingerprint is
// ambiguous between "the caller omitted it" (carry the stored one forward) and "a
// reset just invalidated it" (leave it empty). Callers that intend the latter pass
// false so this does not read the still-stale row it is racing to overwrite.
func (s *RDBConfigStore) updateComplexityAnalyzerConfigWithTx(ctx context.Context, normalized *ComplexityAnalyzerConfig, txDB *gorm.DB, carryOverFingerprint bool) error {
	// Taken unconditionally, not just when the carry-over read below runs: a save that
	// already carries both fields still has to serialize against a concurrent reset.
	if err := s.lockComplexityAnalyzerConfigRow(ctx, txDB); err != nil {
		return err
	}
	needsHashes := normalized.ConfigHashes.Empty()
	needsFingerprint := carryOverFingerprint && normalized.EmbeddingFingerprint == ""
	if needsHashes || needsFingerprint {
		hashes, fingerprint, err := s.readComplexityCarryOverWithDB(ctx, dbForUpdate(txDB))
		if err != nil {
			return err
		}
		if needsHashes {
			normalized.ConfigHashes = hashes
		}
		if needsFingerprint {
			normalized.EmbeddingFingerprint = fingerprint
		}
	}

	raw, err := encodeComplexityAnalyzerConfig(*normalized)
	if err != nil {
		return err
	}
	if err := s.UpdateConfig(ctx, &tables.TableGovernanceConfig{
		Key:   tables.ConfigComplexityAnalyzerConfigKey,
		Value: string(raw),
	}, txDB); err != nil {
		return err
	}

	semanticRaw, err := encodeComplexitySemanticConfigRow(*normalized)
	if err != nil {
		return err
	}
	return s.UpdateConfig(ctx, &tables.TableGovernanceConfig{
		Key:   tables.ConfigComplexitySemanticConfigKey,
		Value: string(semanticRaw),
	}, txDB)
}

// GetAuthConfig retrieves the auth configuration from the database.
func (s *RDBConfigStore) GetAuthConfig(ctx context.Context) (*AuthConfig, error) {
	var username *string
	var password *string
	var isEnabled bool
	if err := s.DB().WithContext(ctx).First(&tables.TableGovernanceConfig{}, "key = ?", tables.ConfigAdminUsernameKey).Select("value").Scan(&username).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}
	if err := s.DB().WithContext(ctx).First(&tables.TableGovernanceConfig{}, "key = ?", tables.ConfigAdminPasswordKey).Select("value").Scan(&password).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}
	if err := s.DB().WithContext(ctx).First(&tables.TableGovernanceConfig{}, "key = ?", tables.ConfigIsAuthEnabledKey).Select("value").Scan(&isEnabled).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}
	if username == nil || password == nil {
		return nil, nil
	}
	return &AuthConfig{
		AdminUserName: schemas.NewSecretVar(*username),
		AdminPassword: schemas.NewSecretVar(*password),
		IsEnabled:     isEnabled,
	}, nil
}

// UpdateAuthConfig updates the auth configuration in the database.
func (s *RDBConfigStore) UpdateAuthConfig(ctx context.Context, config *AuthConfig) error {
	return s.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&tables.TableGovernanceConfig{
			Key:   tables.ConfigAdminUsernameKey,
			Value: config.AdminUserName.GetValue(),
		}).Error; err != nil {
			return err
		}
		if err := tx.Save(&tables.TableGovernanceConfig{
			Key:   tables.ConfigAdminPasswordKey,
			Value: config.AdminPassword.GetValue(),
		}).Error; err != nil {
			return err
		}
		if err := tx.Save(&tables.TableGovernanceConfig{
			Key:   tables.ConfigIsAuthEnabledKey,
			Value: fmt.Sprintf("%t", config.IsEnabled),
		}).Error; err != nil {
			return err
		}
		return nil
	})
}

// GetProxyConfig retrieves the proxy configuration from the database.
func (s *RDBConfigStore) GetProxyConfig(ctx context.Context) (*tables.GlobalProxyConfig, error) {
	var configEntry tables.TableGovernanceConfig
	if err := s.DB().WithContext(ctx).First(&configEntry, "key = ?", tables.ConfigProxyKey).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if configEntry.Value == "" {
		return nil, nil
	}
	var proxyConfig tables.GlobalProxyConfig
	if err := json.Unmarshal([]byte(configEntry.Value), &proxyConfig); err != nil {
		return nil, fmt.Errorf("failed to unmarshal proxy config: %w", err)
	}
	// Decrypt the password if it's not empty
	if proxyConfig.Password != "" {
		decryptedPassword, err := encrypt.Decrypt(proxyConfig.Password)
		if err != nil {
			// If decryption fails due to uninitialized key, the password might be stored in plaintext
			// (from before encryption was enabled), so we return it as-is
			if !errors.Is(err, encrypt.ErrEncryptionKeyNotInitialized) {
				return nil, fmt.Errorf("failed to decrypt proxy password: %w", err)
			}
		} else {
			proxyConfig.Password = decryptedPassword
		}
	}
	return &proxyConfig, nil
}

// UpdateProxyConfig updates the proxy configuration in the database.
func (s *RDBConfigStore) UpdateProxyConfig(ctx context.Context, config *tables.GlobalProxyConfig) error {
	// Create a copy to avoid modifying the original config
	configCopy := *config

	// Encrypt the password if it's not empty
	if configCopy.Password != "" {
		encryptedPassword, err := encrypt.Encrypt(configCopy.Password)
		if err != nil {
			return fmt.Errorf("failed to encrypt proxy password: %w", err)
		}
		configCopy.Password = encryptedPassword
	}

	configJSON, err := json.Marshal(&configCopy)
	if err != nil {
		return fmt.Errorf("failed to marshal proxy config: %w", err)
	}
	return s.DB().WithContext(ctx).Save(&tables.TableGovernanceConfig{
		Key:   tables.ConfigProxyKey,
		Value: string(configJSON),
	}).Error
}

// GetRestartRequiredConfig retrieves the restart required configuration from the database.
func (s *RDBConfigStore) GetRestartRequiredConfig(ctx context.Context) (*tables.RestartRequiredConfig, error) {
	var configEntry tables.TableGovernanceConfig
	if err := s.DB().WithContext(ctx).First(&configEntry, "key = ?", tables.ConfigRestartRequiredKey).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if configEntry.Value == "" {
		return nil, nil
	}
	var restartConfig tables.RestartRequiredConfig
	if err := json.Unmarshal([]byte(configEntry.Value), &restartConfig); err != nil {
		return nil, fmt.Errorf("failed to unmarshal restart required config: %w", err)
	}
	return &restartConfig, nil
}

// SetRestartRequiredConfig sets the restart required configuration in the database.
func (s *RDBConfigStore) SetRestartRequiredConfig(ctx context.Context, config *tables.RestartRequiredConfig) error {
	configJSON, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal restart required config: %w", err)
	}
	return s.DB().WithContext(ctx).Save(&tables.TableGovernanceConfig{
		Key:   tables.ConfigRestartRequiredKey,
		Value: string(configJSON),
	}).Error
}

// ClearRestartRequiredConfig clears the restart required configuration in the database.
func (s *RDBConfigStore) ClearRestartRequiredConfig(ctx context.Context) error {
	return s.DB().WithContext(ctx).Save(&tables.TableGovernanceConfig{
		Key:   tables.ConfigRestartRequiredKey,
		Value: `{"required":false,"reason":""}`,
	}).Error
}

// GetSession retrieves a session from the database.
func (s *RDBConfigStore) GetSession(ctx context.Context, token string) (*tables.SessionsTable, error) {
	var session tables.SessionsTable
	tokenHash := encrypt.HashSHA256(token)
	err := s.DB().WithContext(ctx).First(&session, "token_hash = ?", tokenHash).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Fall back to plaintext lookup for rows written before the hash existed. Only rows
			// still stored in plaintext qualify: an encrypted row's token column holds
			// ciphertext, which is not the token and must not resolve the session.
			if err := s.DB().WithContext(ctx).Where(plaintextRowPredicate).First(&session, "token = ?", token).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil, nil
				}
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	return &session, nil
}

// CreateSession creates a new session in the database.
func (s *RDBConfigStore) CreateSession(ctx context.Context, session *tables.SessionsTable) error {
	return s.DB().WithContext(ctx).Create(session).Error
}

// DeleteSession deletes a session from the database.
func (s *RDBConfigStore) DeleteSession(ctx context.Context, token string) error {
	tokenHash := encrypt.HashSHA256(token)
	var session tables.SessionsTable
	if err := s.DB().WithContext(ctx).First(&session, "token_hash = ?", tokenHash).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Fall back to plaintext lookup for rows written before the hash existed, and only for
			// rows still stored in plaintext (see GetSession).
			return s.DB().WithContext(ctx).Where(plaintextRowPredicate).Delete(&tables.SessionsTable{}, "token = ?", token).Error
		}
		return err
	}
	result := s.DB().WithContext(ctx).Delete(&session)
	if result.Error != nil {
		return result.Error
	}
	return nil
}

// FlushSessions flushes all sessions from the database.
func (s *RDBConfigStore) FlushSessions(ctx context.Context) error {
	return s.DB().WithContext(ctx).Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&tables.SessionsTable{}).Error
}

// ExecuteTransaction executes a transaction.
func (s *RDBConfigStore) ExecuteTransaction(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return s.DB().WithContext(ctx).Transaction(fn)
}

// RetryOnNotFound retries a function up to 3 times with 1-second delays if it returns ErrNotFound
func (s *RDBConfigStore) RetryOnNotFound(ctx context.Context, fn func(ctx context.Context) (any, error), maxRetries int, retryDelay time.Duration) (any, error) {
	var lastErr error
	for attempt := range maxRetries {
		result, err := fn(ctx)
		if err == nil {
			return result, nil
		}
		if !errors.Is(err, ErrNotFound) && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}

		lastErr = err

		// Don't wait after the last attempt
		if attempt < maxRetries-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryDelay):
				// Continue to next retry
			}
		}
	}
	return nil, lastErr
}

// doesTableExist checks if a table exists in the database.
func (s *RDBConfigStore) doesTableExist(ctx context.Context, tableName string) bool {
	return s.DB().WithContext(ctx).Migrator().HasTable(tableName)
}

func (s *RDBConfigStore) doesColumnExist(ctx context.Context, tableName, columnName string) bool {
	return s.DB().WithContext(ctx).Migrator().HasColumn(tableName, columnName)
}

// removeNullKeys removes null keys from the database.
func (s *RDBConfigStore) removeNullKeys(ctx context.Context) error {
	return s.DB().WithContext(ctx).Exec("DELETE FROM config_keys WHERE key_id IS NULL OR value IS NULL").Error
}

// removeDuplicateKeysAndNullKeys removes duplicate keys based on key_id and value combination
// Keeps the record with the smallest ID (oldest record) and deletes duplicates
func (s *RDBConfigStore) removeDuplicateKeysAndNullKeys(ctx context.Context) error {
	s.logger.Debug("removing duplicate keys and null keys from the database")
	// Check if the config_keys table exists first
	if !s.doesTableExist(ctx, "config_keys") {
		return nil
	}
	s.logger.Debug("removing null keys from the database")
	// First, remove null keys
	if err := s.removeNullKeys(ctx); err != nil {
		return fmt.Errorf("failed to remove null keys: %w", err)
	}
	s.logger.Debug("deleting duplicate keys from the database")
	// Find and delete duplicate keys, keeping only the one with the smallest ID
	// This query deletes all records except the one with the minimum ID for each (key_id, value) pair
	result := s.DB().WithContext(ctx).Exec(`
		DELETE FROM config_keys
		WHERE id NOT IN (
			SELECT MIN(id)
			FROM config_keys
			GROUP BY key_id, value
		)
	`)

	if result.Error != nil {
		return fmt.Errorf("failed to remove duplicate keys: %w", result.Error)
	}
	s.logger.Debug("migration complete")
	return nil
}

// Close closes the SQLite config store.
func (s *RDBConfigStore) Close(ctx context.Context) error {
	sqlDB, err := s.DB().DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// TryAcquireLock attempts to insert a lock row. Returns true if the lock was acquired.
// Uses INSERT ... ON CONFLICT DO NOTHING for atomic lock acquisition.
func (s *RDBConfigStore) TryAcquireLock(ctx context.Context, lock *tables.TableDistributedLock) (bool, error) {
	// Set CreatedAt if not already set
	if lock.CreatedAt.IsZero() {
		lock.CreatedAt = time.Now().UTC()
	}

	// Use GORM clause-based insert for dialect-appropriate SQL
	result := s.DB().WithContext(ctx).Clauses(
		clause.OnConflict{
			Columns:   []clause.Column{{Name: "lock_key"}},
			DoNothing: true,
		},
	).Create(lock)

	if result.Error != nil {
		return false, fmt.Errorf("failed to acquire lock: %w", result.Error)
	}

	// If RowsAffected is 1, the lock was acquired
	return result.RowsAffected == 1, nil
}

// GetLock retrieves a lock by its key. Returns nil if the lock doesn't exist.
func (s *RDBConfigStore) GetLock(ctx context.Context, lockKey string) (*tables.TableDistributedLock, error) {
	var lock tables.TableDistributedLock
	result := s.DB().WithContext(ctx).Where("lock_key = ?", lockKey).First(&lock)

	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get lock: %w", result.Error)
	}

	return &lock, nil
}

// UpdateLockExpiry updates the expiration time for an existing lock.
// Only succeeds if the holder ID matches the current lock holder.
func (s *RDBConfigStore) UpdateLockExpiry(ctx context.Context, lockKey, holderID string, expiresAt time.Time) error {
	result := s.DB().WithContext(ctx).Model(&tables.TableDistributedLock{}).
		Where("lock_key = ? AND holder_id = ? AND expires_at > ?", lockKey, holderID, time.Now().UTC()).
		Update("expires_at", expiresAt)

	if result.Error != nil {
		return fmt.Errorf("failed to update lock expiry: %w", result.Error)
	}

	if result.RowsAffected == 0 {
		return ErrLockNotHeld
	}

	return nil
}

// ReleaseLock deletes a lock if the holder ID matches.
// Returns true if the lock was released, false if it wasn't held by the given holder.
func (s *RDBConfigStore) ReleaseLock(ctx context.Context, lockKey, holderID string) (bool, error) {
	result := s.DB().WithContext(ctx).
		Where("lock_key = ? AND holder_id = ?", lockKey, holderID).
		Delete(&tables.TableDistributedLock{})

	if result.Error != nil {
		return false, fmt.Errorf("failed to release lock: %w", result.Error)
	}

	return result.RowsAffected > 0, nil
}

// CleanupExpiredLocks removes all locks that have expired.
// Returns the number of locks cleaned up.
func (s *RDBConfigStore) CleanupExpiredLocks(ctx context.Context) (int64, error) {
	result := s.DB().WithContext(ctx).
		Where("expires_at < ?", time.Now().UTC()).
		Delete(&tables.TableDistributedLock{})

	if result.Error != nil {
		return 0, fmt.Errorf("failed to cleanup expired locks: %w", result.Error)
	}

	return result.RowsAffected, nil
}

// CleanupExpiredLockByKey atomically deletes a specific lock only if it has expired.
// Returns true if an expired lock was deleted, false if the lock doesn't exist or hasn't expired.
func (s *RDBConfigStore) CleanupExpiredLockByKey(ctx context.Context, lockKey string) (bool, error) {
	result := s.DB().WithContext(ctx).
		Where("lock_key = ? AND expires_at < ?", lockKey, time.Now().UTC()).
		Delete(&tables.TableDistributedLock{})

	if result.Error != nil {
		return false, fmt.Errorf("failed to cleanup expired lock: %w", result.Error)
	}

	return result.RowsAffected > 0, nil
}

// normalizeLeaseTime is applied to claimed_until on every write and fence
// comparison: UTC (mixed offsets break SQLite's string datetime compares) and
// microsecond precision (Postgres truncates to microseconds, so an exact
// equality fence must not carry nanoseconds).
func normalizeLeaseTime(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}

// A caller's discovery lives only as long as the credential it was made with is
// usable. The helpers below delete discoveries inside the transaction that
// deletes or invalidates their credentials, so the two never disagree.

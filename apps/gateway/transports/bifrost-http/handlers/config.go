package handlers

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fasthttp/router"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/network"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/encrypt"
	"github.com/maximhq/bifrost/framework/modelcatalog"
	"github.com/maximhq/bifrost/plugins/compat"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// securityHeaders is the list of headers that cannot be configured in allowlist/denylist
// These headers are always blocked for security reasons regardless of user configuration
var securityHeaders = []string{
	"authorization",
	"proxy-authorization",
	"cookie",
	"host",
	"content-length",
	"connection",
	"transfer-encoding",
	"x-api-key",
	"x-goog-api-key",
	"xi-api-key",
	"x-bf-api-key",
	"x-bf-vk",
}

func getPasswordPolicyFailures(password string) []string {
	failures := make([]string, 0, 5)
	hasUppercase := false
	hasLowercase := false
	hasDigit := false
	hasSpecial := false

	for i := 0; i < len(password); i++ {
		char := password[i]
		switch {
		case char >= 'A' && char <= 'Z':
			hasUppercase = true
		case char >= 'a' && char <= 'z':
			hasLowercase = true
		case char >= '0' && char <= '9':
			hasDigit = true
		default:
			hasSpecial = true
		}
	}

	if len(password) < 12 {
		failures = append(failures, "at least 12 characters")
	}
	if !hasUppercase {
		failures = append(failures, "one uppercase letter")
	}
	if !hasLowercase {
		failures = append(failures, "one lowercase letter")
	}
	if !hasDigit {
		failures = append(failures, "one number")
	}
	if !hasSpecial {
		failures = append(failures, "one special character")
	}

	return failures
}

// ConfigManager is the interface for the config manager
type ConfigManager interface {
	UpdateAuthConfig(ctx context.Context, authConfig *configstore.AuthConfig) error
	// ValidateSetupToken checks the one-time bootstrap token required to create the
	// first admin account. Returns true once an admin account already exists.
	ValidateSetupToken(token string) bool
	// ValidateConfiguredSetupToken checks a token against the operator-configured setup
	// token regardless of whether an admin account exists; false when none is configured.
	ValidateConfiguredSetupToken(token string) bool
	ReloadClientConfigFromConfigStore(ctx context.Context) error
	UpdateSyncConfig(ctx context.Context) error
	ForceReloadPricing(ctx context.Context) error
	UpdateDropExcessRequests(ctx context.Context, value bool)
	ReloadPlugin(ctx context.Context, name string, path *string, pluginConfig any, placement *schemas.PluginPlacement, order *int) error
	RemovePlugin(ctx context.Context, name string) error
	ReloadProxyConfig(ctx context.Context, config *configstoreTables.GlobalProxyConfig) error
	ReloadHeaderFilterConfig(ctx context.Context, config *configstoreTables.GlobalHeaderFilterConfig) error
}

// ConfigHandler manages runtime configuration updates for Bifrost.
// It provides endpoints to update and retrieve settings persisted via the ConfigStore backed by sql database.
type ConfigHandler struct {
	store         *lib.Config
	configManager ConfigManager
	saveMu        sync.Mutex // Serializes updateConfig from snapshot through publication
}

// NewConfigHandler creates a new handler for configuration management.
// It requires the Bifrost client, a logger, and the config store.
func NewConfigHandler(configManager ConfigManager, store *lib.Config) *ConfigHandler {
	return &ConfigHandler{
		configManager: configManager,
		store:         store,
	}
}

// RegisterRoutes registers the configuration-related routes.
// It adds the `PUT /api/config` endpoint.
func (h *ConfigHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	r.GET("/api/config", lib.ChainMiddlewares(h.getConfig, middlewares...))
	r.PUT("/api/config", lib.ChainMiddlewares(h.updateConfig, middlewares...))
	r.POST("/api/config/metadata", lib.ChainMiddlewares(h.updateMetadata, middlewares...))
	r.GET("/api/version", lib.ChainMiddlewares(h.getVersion, middlewares...))
	r.GET("/api/proxy-config", lib.ChainMiddlewares(h.getProxyConfig, middlewares...))
	r.PUT("/api/proxy-config", lib.ChainMiddlewares(h.updateProxyConfig, middlewares...))
	r.POST("/api/pricing/force-sync", lib.ChainMiddlewares(h.forceSyncPricing, middlewares...))
}

// getVersion handles GET /api/version - Get the current version
func (h *ConfigHandler) getVersion(ctx *fasthttp.RequestCtx) {
	SendJSON(ctx, version)
}

// getConfig handles GET /config - Get the current configuration
func (h *ConfigHandler) getConfig(ctx *fasthttp.RequestCtx) {
	mapConfig := make(map[string]any)

	if query := string(ctx.QueryArgs().Peek("from_db")); query == "true" {
		if h.store.ConfigStore == nil {
			SendError(ctx, fasthttp.StatusServiceUnavailable, "config store not available")
			return
		}
		cc, err := h.store.ConfigStore.GetClientConfig(ctx)
		if err != nil {
			SendError(ctx, fasthttp.StatusInternalServerError,
				fmt.Sprintf("failed to fetch config from db: %v", err))
			return
		}
		if cc != nil {
			mapConfig["client_config"] = cc.Redacted()
		}
		// Fetching framework config
		fc, err := h.store.ConfigStore.GetFrameworkConfig(ctx)
		if err != nil {
			SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to fetch framework config from db: %v", err))
			return
		}
		normalizedFrameworkConfig, _, _ := lib.ResolveFrameworkPricingConfig(fc, nil)
		mapConfig["framework_config"] = *normalizedFrameworkConfig
	} else {
		mapConfig["client_config"] = h.store.ClientConfig.Redacted()
		// Snapshot under the read lock; updateConfig swaps this pointer from
		// another request goroutine.
		h.store.Mu.RLock()
		storedFrameworkConfig := h.store.FrameworkConfig
		h.store.Mu.RUnlock()
		normalizedFrameworkConfig, _, _ := lib.ResolveFrameworkPricingConfig(nil, storedFrameworkConfig)
		mapConfig["framework_config"] = *normalizedFrameworkConfig
	}
	if h.store.ConfigStore != nil {
		// Fetching governance config
		authConfig, err := h.store.ConfigStore.GetAuthConfig(ctx)
		if err != nil {
			logger.Warn("failed to get auth config from store: %v", err)
			SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to get auth config from store: %v", err))
			return
		}
		// Getting username and password from auth config
		// This username password is for the dashboard authentication
		if authConfig != nil {
			// For password, return SecretVar structure with redacted value
			// If from env, preserve env_var reference but clear value
			// If not from env, show <redacted> as the value
			var passwordSecretVar *schemas.SecretVar
			if authConfig.AdminPassword != nil && authConfig.AdminPassword.IsFromSecret() {
				passwordSecretVar = authConfig.AdminPassword.FullyRedacted()
			} else {
				passwordSecretVar = &schemas.SecretVar{
					Val: "<redacted>",
				}
			}
			mapConfig["auth_config"] = map[string]any{
				"admin_username": authConfig.AdminUserName,
				"admin_password": passwordSecretVar,
				"is_enabled":     authConfig.IsEnabled,
			}
		}
		// When authConfig is nil, no admin account has been created yet: leave
		// auth_config unset (rather than a placeholder object) so the UI's
		// isFirstTimeSetup check (!bifrostConfig?.auth_config) can tell "no admin
		// configured yet" apart from "admin configured with empty fields" and show
		// the setup-token field / include setup_token in the create request.
	} else {
		mapConfig["auth_config"] = map[string]any{
			"admin_username": &schemas.SecretVar{},
			"admin_password": &schemas.SecretVar{},
			"is_enabled":     false,
		}
	}
	mapConfig["is_db_connected"] = h.store.ConfigStore != nil
	if h.store.EnvLabel != "" {
		mapConfig["env_label"] = h.store.EnvLabel
	}
	mapConfig["disable_onboarding"] = h.store.DisableOnboarding
	mapConfig["is_git_available"] = CheckGitAvailability()
	mapConfig["skills_marketplace_base_url"] = h.store.GetSkillsExternalClientURL()
	mapConfig["is_cache_connected"] = h.store.VectorStore != nil
	mapConfig["is_logs_connected"] = h.store.LogsStore != nil
	mapConfig["is_object_storage_connected"] = h.store.LogsStoreConfig != nil && h.store.LogsStoreConfig.ObjectStorage != nil
	// Fetching proxy config
	if h.store.ConfigStore != nil {
		proxyConfig, err := h.store.ConfigStore.GetProxyConfig(ctx)
		if err != nil {
			logger.Warn("failed to get proxy config from store: %v", err)
		} else if proxyConfig != nil {
			// Redact password if present
			if proxyConfig.Password != "" {
				proxyConfig.Password = "<redacted>"
			}
			proxyConfig.URL = lib.RedactProxyURLPassword(proxyConfig.URL)
			mapConfig["proxy_config"] = proxyConfig
		}
		// Fetching restart required config
		restartConfig, err := h.store.ConfigStore.GetRestartRequiredConfig(ctx)
		if err != nil {
			logger.Warn("failed to get restart required config from store: %v", err)
		} else if restartConfig != nil {
			mapConfig["restart_required"] = restartConfig
		}
		// Fetching UI/admin metadata blob (onboarding_dismissed, etc.).
		// This is a free-form key/value store that bypasses config.json sync.
		if metadata, err := h.store.ConfigStore.GetClientMetadata(ctx); err != nil {
			if !errors.Is(err, configstore.ErrNotFound) {
				logger.Warn("failed to get client metadata from store: %v", err)
			}
		} else if len(metadata) > 0 {
			mapConfig["metadata"] = metadata
		}
	}
	SendJSON(ctx, mapConfig)
}

// updateMetadata handles POST /api/config/metadata - merges a JSON object of
// key/value pairs into the ClientConfig metadata blob. Keys with a nil value
// are removed. Intended for UI/admin preferences (onboarding state, dismissed
// tooltips, etc.) and is auth-gated by the same middleware as the rest of /api/config.
func (h *ConfigHandler) updateMetadata(ctx *fasthttp.RequestCtx) {
	if h.store.ConfigStore == nil {
		SendError(ctx, fasthttp.StatusServiceUnavailable, "config store not available")
		return
	}
	var patch map[string]any
	if err := json.Unmarshal(ctx.PostBody(), &patch); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Invalid request payload")
		return
	}
	if len(patch) == 0 {
		SendError(ctx, fasthttp.StatusBadRequest, "patch body must contain at least one key")
		return
	}
	// A missing row reads as an empty map, so any error here is a real read failure.
	current, err := h.store.ConfigStore.GetClientMetadata(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to read metadata: %v", err))
		return
	}
	before := map[string]any{}
	for k := range patch {
		if v, ok := current[k]; ok {
			before[k] = v
		}
	}
	if err := h.store.ConfigStore.UpdateClientMetadata(ctx, patch); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusConflict, fmt.Sprintf("failed to update metadata: %v", err))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to update metadata: %v", err))
		return
	}
	after := map[string]any{}
	for k, v := range patch {
		if v != nil {
			after[k] = v
		}
	}
	lib.RecordAuditChange(ctx, "config_metadata.updated", "config_metadata", "client_metadata", "Client metadata", before, after)
	SendJSON(ctx, map[string]any{"success": true})
}

// authConfigWithSetupToken extends the persisted AuthConfig shape with the
// one-time bootstrap setup_token field, for the auth_config object in PUT
// /api/config requests only. setup_token lives here (nested under auth_config
// on the wire) rather than directly on configstore.AuthConfig because that
// type is also what gets persisted to and read back from the DB via
// UpdateAuthConfig/GetAuthConfig; keeping SetupToken off it means there's no
// risk of it ever being written to storage or echoed back by GET /api/config.
type authConfigWithSetupToken struct {
	configstore.AuthConfig
	// SetupToken is the operator-configured bootstrap token (see AuthMiddleware.bootstrapToken):
	// required to create the very first admin account, and accepted as proof of control for
	// changes made while dashboard auth is disabled. It is never persisted.
	SetupToken      string `json:"setup_token,omitempty"`
	CurrentPassword string `json:"current_password,omitempty"` // stored admin password, proves control while auth is disabled; never persisted
}

// updateConfig updates the core configuration settings.
// Currently, it supports hot-reloading of the `drop_excess_requests` setting.
// Note that settings like `prometheus_labels` cannot be changed at runtime.
func (h *ConfigHandler) updateConfig(ctx *fasthttp.RequestCtx) {
	if h.store.ConfigStore == nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "Config store not initialized")
		return
	}
	// Overlapping saves would each snapshot the same live config and race its
	// publication, so a later save could undo an earlier one.
	h.saveMu.Lock()
	defer h.saveMu.Unlock()

	payload := struct {
		ClientConfig    configstore.ClientConfig               `json:"client_config"`
		FrameworkConfig configstoreTables.TableFrameworkConfig `json:"framework_config"`
		AuthConfig      *authConfigWithSetupToken              `json:"auth_config"`
	}{}

	if err := json.Unmarshal(ctx.PostBody(), &payload); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Invalid request payload")
		return
	}
	var requestFields struct {
		ClientConfig map[string]json.RawMessage `json:"client_config"`
	}
	if err := json.Unmarshal(ctx.PostBody(), &requestFields); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Invalid request payload")
		return
	}
	clientConfigFields := requestFields.ClientConfig

	// Validate URL overrides up front — the rest of this handler applies live
	// mutations (drop-excess flag, compat plugin reload) before persisting, so a
	// late rejection would leave the process in a partially-updated state.
	if err := configstore.ValidatePublicBaseURL(payload.ClientConfig.SkillsExternalClientURL.GetValue()); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("skills_external_client_url %v", err))
		return
	}

	// enforce_vk_object_ownership is a plain bool that defaults to true, so a JSON null would
	// decode as true and silently re-enable ownership. Refuse it; omit the field to keep the stored value.
	if raw, present := clientConfigFields["enforce_vk_object_ownership"]; present && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		SendError(ctx, fasthttp.StatusBadRequest, "enforce_vk_object_ownership must be true or false")
		return
	}
	// The access-profile user key settings are plain bools that default to false, so a JSON null would
	// decode as false and silently hand user key creation or deletion back to access profiles. Refuse
	// it; omit the field to keep the stored value.
	for _, field := range []string{"skip_access_profile_user_virtual_key_creation", "skip_access_profile_user_virtual_key_deletion"} {
		if raw, present := clientConfigFields[field]; present && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			SendError(ctx, fasthttp.StatusBadRequest, field+" must be true or false")
			return
		}
	}

	// vk_rotation_cooldown bounds: negative is meaningless, and anything past 30
	// days keeps a retired credential alive long enough to defeat the rotation.
	if payload.ClientConfig.VKRotationCooldown < 0 {
		SendError(ctx, fasthttp.StatusBadRequest, "vk_rotation_cooldown must not be negative")
		return
	}
	if payload.ClientConfig.VKRotationCooldown.D() > configstore.MaxVKRotationCooldown {
		SendError(ctx, fasthttp.StatusBadRequest, "vk_rotation_cooldown must not exceed 30 days")
		return
	}

	// Fetching existing framework config. The URL checks below skip a URL equal
	// to the stored one, so a save that sends the stored config back is not
	// refused for a URL only config.json may set, such as a file:// catalog.
	frameworkConfig, err := h.store.ConfigStore.GetFrameworkConfig(ctx)
	if err != nil {
		logger.Warn("failed to get framework config from store: %v", err)
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to get framework config from store: %v", err))
		return
	}
	var storedPricingURL, storedModelParametersURL *string
	if frameworkConfig != nil {
		storedPricingURL, storedModelParametersURL = frameworkConfig.PricingURL, frameworkConfig.ModelParametersURL
	}

	// Validating framework config. An empty pricing_url or model_parameters_url
	// (what the UI sends when the field is cleared) resets it to the default.
	if payload.FrameworkConfig.PricingURL != nil && strings.TrimSpace(*payload.FrameworkConfig.PricingURL) == "" {
		payload.FrameworkConfig.PricingURL = bifrost.Ptr(modelcatalog.DefaultPricingURL)
	}
	if payload.FrameworkConfig.ModelParametersURL != nil && strings.TrimSpace(*payload.FrameworkConfig.ModelParametersURL) == "" {
		payload.FrameworkConfig.ModelParametersURL = bifrost.Ptr(modelcatalog.DefaultModelParametersURL)
	}
	if payload.FrameworkConfig.PricingURL != nil && *payload.FrameworkConfig.PricingURL != modelcatalog.DefaultPricingURL &&
		!sameAsStoredURL(*payload.FrameworkConfig.PricingURL, storedPricingURL) {
		if err := checkURLAccessibility(*payload.FrameworkConfig.PricingURL); err != nil {
			logger.Warn("failed to check the accessibility of the pricing URL: %v", err)
			SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("failed to check the accessibility of the pricing URL: %v", err))
			return
		}
	}
	if payload.FrameworkConfig.ModelParametersURL != nil && *payload.FrameworkConfig.ModelParametersURL != modelcatalog.DefaultModelParametersURL &&
		!sameAsStoredURL(*payload.FrameworkConfig.ModelParametersURL, storedModelParametersURL) {
		if err := checkURLAccessibility(*payload.FrameworkConfig.ModelParametersURL); err != nil {
			logger.Warn("failed to check the accessibility of the model parameters URL: %v", err)
			SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("failed to check the accessibility of the model parameters URL: %v", err))
			return
		}
	}

	// Checking the pricing sync interval
	if payload.FrameworkConfig.PricingSyncInterval != nil && *payload.FrameworkConfig.PricingSyncInterval <= 0 {
		logger.Warn("pricing sync interval must be greater than 0")
		SendError(ctx, fasthttp.StatusBadRequest, "pricing sync interval must be greater than 0")
		return
	}

	// Checking the live models sync interval. Unlike the intervals above, 0 is
	// accepted: it is the documented way to turn the background refresher off.
	if payload.FrameworkConfig.LiveModelsSyncInterval != nil {
		interval := *payload.FrameworkConfig.LiveModelsSyncInterval
		if interval < 0 {
			logger.Warn("live models sync interval cannot be negative")
			SendError(ctx, fasthttp.StatusBadRequest, "live models sync interval cannot be negative (use 0 to disable background refresh)")
			return
		}
		if interval > 0 && interval < modelcatalog.MinimumLiveModelsSyncIntervalSec {
			logger.Warn("live models sync interval is below the minimum of %d seconds", modelcatalog.MinimumLiveModelsSyncIntervalSec)
			SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("live models sync interval must be 0 (disabled) or at least %d seconds", modelcatalog.MinimumLiveModelsSyncIntervalSec))
			return
		}
	}

	// Snapshot the live config under the same lock used by reloads and runtime readers.
	h.store.Mu.RLock()
	currentSnapshot := *h.store.ClientConfig
	h.store.Mu.RUnlock()
	currentConfig := &currentSnapshot
	// Build the update on an independent copy: the live config must not
	// change until persistence succeeds, or a failed store write would leave
	// memory ahead of the database. A shallow copy is enough because every
	// edit below replaces a whole field; nothing mutates through the shared
	// pointer or slice fields.
	copied := *currentConfig
	updatedConfig := &copied
	before := lib.AuditSnapshot(currentConfig.Redacted())

	// Validate first-admin setup before any live mutation or persistence below.
	var existingAuthConfig *configstore.AuthConfig
	var initialPasswordHash string
	if payload.AuthConfig != nil {
		var err error
		existingAuthConfig, err = h.store.ConfigStore.GetAuthConfig(ctx)
		if err != nil && !errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to get auth config from store: %v", err))
			return
		}
		if existingAuthConfig == nil && payload.AuthConfig.IsEnabled {
			if !isSetupTokenAuthenticated(ctx) && !h.configManager.ValidateSetupToken(payload.AuthConfig.SetupToken) {
				SendError(ctx, fasthttp.StatusForbidden, "a valid setup token is required to create the initial admin account")
				return
			}
			if payload.AuthConfig.AdminUserName == nil || payload.AuthConfig.AdminUserName.GetValue() == "" ||
				payload.AuthConfig.AdminPassword == nil || payload.AuthConfig.AdminPassword.GetValue() == "" || payload.AuthConfig.AdminPassword.ShouldPreserveStored() {
				SendError(ctx, fasthttp.StatusBadRequest, "auth username and password must be provided")
				return
			}
			if failures := getPasswordPolicyFailures(payload.AuthConfig.AdminPassword.GetValue()); len(failures) > 0 {
				SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("auth password must include %s", strings.Join(failures, ", ")))
				return
			}
			initialPasswordHash, err = encrypt.Hash(payload.AuthConfig.AdminPassword.GetValue())
			if err != nil {
				SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("invalid auth password: %v", err))
				return
			}
		}
	}

	// Only an omitted field keeps the stored placement; an explicit "" or null is a supplied
	// value and must be one of the two placements like any other.
	if _, present := clientConfigFields["extra_fields_placement"]; present &&
		payload.ClientConfig.ExtraFieldsPlacement != configstoreTables.ExtraFieldsPlacementBody &&
		payload.ClientConfig.ExtraFieldsPlacement != configstoreTables.ExtraFieldsPlacementHeaders {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("extra_fields_placement must be 'body' or 'headers', got %q", payload.ClientConfig.ExtraFieldsPlacement))
		return
	}
	// The same for the entity access profile mode: only an omitted field keeps the stored mode.
	if _, present := clientConfigFields["entity_access_profile_mode"]; present &&
		payload.ClientConfig.EntityAccessProfileMode != configstoreTables.EntityAccessProfileModeUnion &&
		payload.ClientConfig.EntityAccessProfileMode != configstoreTables.EntityAccessProfileModeIntersect {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("entity_access_profile_mode must be 'union' or 'intersect', got %q", payload.ClientConfig.EntityAccessProfileMode))
		return
	}

	if payload.ClientConfig.LogRetentionDays < 1 {
		logger.Warn("log_retention_days must be at least 1")
		SendError(ctx, fasthttp.StatusBadRequest, "log_retention_days must be at least 1")
		return
	}
	// Omitted keeps the stored tokenizer families. Validated here, before any live
	// mutation below, so a rejected name never leaves the runtime changed.
	_, tokenizersSent := clientConfigFields["usage_estimation_tokenizers"]
	var tokenizers []string
	if tokenizersSent {
		var err error
		if tokenizers, err = lib.NormalizeUsageEstimationTokenizers(payload.ClientConfig.UsageEstimationTokenizers); err != nil {
			SendError(ctx, fasthttp.StatusBadRequest, err.Error())
			return
		}
	}

	var restartReasons []string

	if payload.ClientConfig.DropExcessRequests != currentConfig.DropExcessRequests {
		h.configManager.UpdateDropExcessRequests(ctx, payload.ClientConfig.DropExcessRequests)
		updatedConfig.DropExcessRequests = payload.ClientConfig.DropExcessRequests
	}

	// No restart needed, and nothing is applied here: the reload after a successful
	// save hands the stored list to the client, so a rejected or failed request
	// leaves the runtime on the families it had.
	if tokenizersSent && !slices.Equal(tokenizers, currentConfig.UsageEstimationTokenizers) {
		updatedConfig.UsageEstimationTokenizers = tokenizers
	}

	if !slices.Equal(payload.ClientConfig.PrometheusLabels, currentConfig.PrometheusLabels) {
		updatedConfig.PrometheusLabels = payload.ClientConfig.PrometheusLabels
		restartReasons = append(restartReasons, "Prometheus labels")
	}

	if !slices.Equal(payload.ClientConfig.AllowedOrigins, currentConfig.AllowedOrigins) {
		updatedConfig.AllowedOrigins = payload.ClientConfig.AllowedOrigins
		restartReasons = append(restartReasons, "Allowed origins")
	}

	if !slices.Equal(payload.ClientConfig.AllowedHeaders, currentConfig.AllowedHeaders) {
		updatedConfig.AllowedHeaders = payload.ClientConfig.AllowedHeaders
		restartReasons = append(restartReasons, "Allowed headers")
	}

	// Only update InitialPoolSize if explicitly provided (> 0) to avoid clearing stored value
	if payload.ClientConfig.InitialPoolSize > 0 {
		if payload.ClientConfig.InitialPoolSize != currentConfig.InitialPoolSize {
			restartReasons = append(restartReasons, "Initial pool size")
		}
		updatedConfig.InitialPoolSize = payload.ClientConfig.InitialPoolSize
	}

	if payload.ClientConfig.EnableLogging != nil {
		payloadLogging := *payload.ClientConfig.EnableLogging
		currentLogging := currentConfig.EnableLogging == nil || *currentConfig.EnableLogging
		if payloadLogging != currentLogging {
			restartReasons = append(restartReasons, "Logging changed")
		}
		updatedConfig.EnableLogging = payload.ClientConfig.EnableLogging
	}

	// No restart needed - logging plugin holds a live pointer to ClientConfig.DisableContentLogging,
	// and ReloadClientConfigFromConfigStore mutates the struct in place so the next request picks up the new value.
	updatedConfig.DisableContentLogging = payload.ClientConfig.DisableContentLogging
	// No restart needed - logging plugin holds a live pointer to ClientConfig.RetainContentInObjectStorage.
	updatedConfig.RetainContentInObjectStorage = payload.ClientConfig.RetainContentInObjectStorage
	updatedConfig.DisableDBPingsInHealth = payload.ClientConfig.DisableDBPingsInHealth
	// No restart needed - ReloadClientConfigFromConfigStore calls CorsMiddleware.UpdateConfig,
	// which atomically swaps in a fresh immutable snapshot carrying the new value.
	updatedConfig.DumpErrorsInConsoleLogs = payload.ClientConfig.DumpErrorsInConsoleLogs

	enforceAuthOnInference := currentConfig.EnforceAuthOnInference
	if payload.ClientConfig.HasInferenceAuthSetting() {
		enforceAuthOnInference = payload.ClientConfig.EnforceAuthOnInference
	} else if payload.AuthConfig != nil && payload.AuthConfig.IsEnabled && existingAuthConfig == nil {
		enforceAuthOnInference = true
	}
	updatedConfig.EnforceAuthOnInference = enforceAuthOnInference
	// Sync deprecated columns to match new field so they stay consistent in the DB
	updatedConfig.EnforceGovernanceHeader = enforceAuthOnInference
	updatedConfig.EnforceSCIMAuth = enforceAuthOnInference

	// Only update when explicitly provided to avoid clearing the stored default (prefer_idp).
	// The conflict-vs-token_exchange validation already ran up front, before
	// any live mutation.
	if payload.ClientConfig.DualCredentialConflictBehavior != "" {
		updatedConfig.DualCredentialConflictBehavior = payload.ClientConfig.DualCredentialConflictBehavior
	}

	// Copied only when sent (validated up front), so an omitted field keeps the stored placement.
	if _, present := clientConfigFields["extra_fields_placement"]; present {
		updatedConfig.ExtraFieldsPlacement = payload.ClientConfig.ExtraFieldsPlacement
	}

	// Only update MaxRequestBodySizeMB if explicitly provided (> 0) to avoid clearing stored value
	if payload.ClientConfig.MaxRequestBodySizeMB > 0 {
		if payload.ClientConfig.MaxRequestBodySizeMB != currentConfig.MaxRequestBodySizeMB {
			restartReasons = append(restartReasons, "Max request body size")
		}
		updatedConfig.MaxRequestBodySizeMB = payload.ClientConfig.MaxRequestBodySizeMB
	}

	// Compat plugin reload is applied after the save below
	newCompat := payload.ClientConfig.Compat
	shouldReloadCompat := newCompat != currentConfig.Compat
	updatedConfig.Compat = newCompat

	// Only update AsyncJobResultTTL if explicitly provided (> 0) to avoid clearing stored value
	if payload.ClientConfig.AsyncJobResultTTL > 0 {
		updatedConfig.AsyncJobResultTTL = payload.ClientConfig.AsyncJobResultTTL
	}

	// Handle RequiredHeaders changes (no restart needed - governance plugin reads via pointer)
	updatedConfig.RequiredHeaders = payload.ClientConfig.RequiredHeaders

	// Handle LoggingHeaders changes (no restart needed - logging plugin reads via pointer)
	updatedConfig.LoggingHeaders = payload.ClientConfig.LoggingHeaders

	// Handle WhitelistedRoutes changes (updated dynamically via AuthMiddleware)
	updatedConfig.WhitelistedRoutes = payload.ClientConfig.WhitelistedRoutes

	// Toggle whether deleted virtual keys should appear in logs filter data.
	updatedConfig.HideDeletedVirtualKeysInFilters = payload.ClientConfig.HideDeletedVirtualKeysInFilters

	// Request types hidden from log reads. No restart needed: the log routes read the
	// live client config on every request, and the filter-data cache keys on the list.
	updatedConfig.HiddenRequestTypes = lib.NormalizeHiddenRequestTypes(payload.ClientConfig.HiddenRequestTypes)

	// Toggle allowing per-request override for content storage and raw request/response storage
	updatedConfig.AllowPerRequestContentStorageOverride = payload.ClientConfig.AllowPerRequestContentStorageOverride

	// Toggle allowing per-request override for raw request/response exposure
	updatedConfig.AllowPerRequestRawOverride = payload.ClientConfig.AllowPerRequestRawOverride

	// Toggle allowing direct key bypass via x-bf-direct-key header
	updatedConfig.AllowDirectKeys = payload.ClientConfig.AllowDirectKeys
	// Read by the daily expired-key cleanup job from the store, so no restart is needed.
	updatedConfig.DeleteExpiredVirtualKeys = payload.ClientConfig.DeleteExpiredVirtualKeys
	// Copied only when sent: an omitted field unmarshals as the default (true), which would
	// re-enable ownership on a partial save. Readers pick the saved value up on the next request.
	if _, present := clientConfigFields["enforce_vk_object_ownership"]; present {
		updatedConfig.EnforceVKObjectOwnership = payload.ClientConfig.EnforceVKObjectOwnership
	}
	// Copied only when sent, so a partial save doesn't silently hand key lifecycle back to access profiles.
	if _, present := clientConfigFields["skip_access_profile_user_virtual_key_creation"]; present {
		updatedConfig.SkipAccessProfileUserVirtualKeyCreation = payload.ClientConfig.SkipAccessProfileUserVirtualKeyCreation
	}
	if _, present := clientConfigFields["skip_access_profile_user_virtual_key_deletion"]; present {
		updatedConfig.SkipAccessProfileUserVirtualKeyDeletion = payload.ClientConfig.SkipAccessProfileUserVirtualKeyDeletion
	}
	// Copied only when sent (validated up front), so a partial save keeps the stored entity access profile mode.
	if _, present := clientConfigFields["entity_access_profile_mode"]; present {
		updatedConfig.EntityAccessProfileMode = payload.ClientConfig.EntityAccessProfileMode
	}

	// Rotation grace period; bounds validated up front. Copied unconditionally
	// so 0 clears a previously stored cooldown.
	updatedConfig.VKRotationCooldown = payload.ClientConfig.VKRotationCooldown

	// No restart needed - routing engine reads via pointer, change is effective immediately.
	if payload.ClientConfig.RoutingChainMaxDepth > 0 {
		updatedConfig.RoutingChainMaxDepth = payload.ClientConfig.RoutingChainMaxDepth
	}

	// Preserve the stored URLs when an older or partial client omits
	// the field. An explicit null still clears the override.
	if _, present := clientConfigFields["skills_external_client_url"]; present {
		updatedConfig.SkillsExternalClientURL = payload.ClientConfig.SkillsExternalClientURL
	}

	// Handle HeaderFilterConfig changes
	if !headerFilterConfigEqual(payload.ClientConfig.HeaderFilterConfig, currentConfig.HeaderFilterConfig) {
		// Validate that no security headers are in the allowlist or denylist
		if err := validateHeaderFilterConfig(payload.ClientConfig.HeaderFilterConfig); err != nil {
			logger.Warn("invalid header filter config: %v", err)
			SendError(ctx, fasthttp.StatusBadRequest, err.Error())
			return
		}
		updatedConfig.HeaderFilterConfig = payload.ClientConfig.HeaderFilterConfig
		if err := h.configManager.ReloadHeaderFilterConfig(ctx, payload.ClientConfig.HeaderFilterConfig); err != nil {
			logger.Warn("failed to reload header filter config: %v", err)
			SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to reload header filter config: %v", err))
			return
		}
	}

	updatedConfig.LogRetentionDays = payload.ClientConfig.LogRetentionDays

	if err := h.store.ConfigStore.UpdateClientConfig(ctx, updatedConfig); err != nil {
		logger.Warn("failed to save configuration: %v", err)
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to save configuration: %v", err))
		return
	}

	if shouldReloadCompat {
		compatCfg := &compat.Config{
			ConvertTextToChat:                   newCompat.ConvertTextToChat,
			ConvertChatToResponses:              newCompat.ConvertChatToResponses,
			ShouldDropParams:                    newCompat.ShouldDropParams,
			ShouldConvertParams:                 newCompat.ShouldConvertParams,
			AzureDeepseek:                       newCompat.AzureDeepseek,
			ForceReasoningOnlyModelsToResponses: newCompat.ForceReasoningOnlyModelsToResponses,
			ConvertDecisionToResponses:          newCompat.ConvertDecisionToResponses,
		}
		if err := h.configManager.ReloadPlugin(ctx, compat.PluginName, nil, compatCfg, nil, nil); err != nil {
			logger.Warn("failed to load compat plugin: %v", err)
			if rollbackErr := h.store.ConfigStore.UpdateClientConfig(ctx, currentConfig); rollbackErr != nil {
				logger.Error("failed to restore configuration after compat plugin reload failure, stored config now differs from the running gateway: %v", rollbackErr)
				SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to load compat plugin (%v) and the previous configuration could not be restored (%v); stored settings now differ from the running gateway, retry the save or restart", err, rollbackErr))
				return
			}
			SendError(ctx, 400, "Failed to load compat plugin")
			return
		}
	}

	// Apply the in-memory change only after persistence succeeds, copying
	// into the live struct (the same way ReloadClientConfigFromConfigStore
	// publishes) so every holder of the pointer observes it. Mu orders the
	// copy with readers such as the OAuth redirect policy check.
	h.store.Mu.Lock()
	*h.store.ClientConfig = *updatedConfig
	h.store.Mu.Unlock()
	// Recorded at the commit point: the reload, framework and auth steps below
	// can fail without undoing this write.
	lib.RecordAuditChange(ctx, "config.updated", "config", "client_config", "client_config", before, updatedConfig.Redacted())
	// Reloading client config from config store
	if err := h.configManager.ReloadClientConfigFromConfigStore(ctx); err != nil {
		logger.Warn("failed to reload client config from config store: %v", err)
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to reload client config from config store: %v", err))
		return
	}
	// if framework config is nil, we will use the default pricing config
	if frameworkConfig == nil {
		frameworkConfig = &configstoreTables.TableFrameworkConfig{
			ID:                     0,
			PricingURL:             bifrost.Ptr(modelcatalog.DefaultPricingURL),
			PricingSyncInterval:    bifrost.Ptr(int64(modelcatalog.DefaultSyncInterval.Seconds())),
			ModelParametersURL:     bifrost.Ptr(modelcatalog.DefaultModelParametersURL),
			LiveModelsSyncInterval: bifrost.Ptr(int64(modelcatalog.DefaultLiveModelsSyncInterval.Seconds())),
		}
	}
	// Handling individual nil cases
	if frameworkConfig.PricingURL == nil {
		frameworkConfig.PricingURL = bifrost.Ptr(modelcatalog.DefaultPricingURL)
	}
	if frameworkConfig.PricingSyncInterval == nil {
		frameworkConfig.PricingSyncInterval = bifrost.Ptr(int64(modelcatalog.DefaultSyncInterval.Seconds()))
	}
	if frameworkConfig.ModelParametersURL == nil {
		frameworkConfig.ModelParametersURL = bifrost.Ptr(modelcatalog.DefaultModelParametersURL)
	}
	if frameworkConfig.LiveModelsSyncInterval == nil {
		frameworkConfig.LiveModelsSyncInterval = bifrost.Ptr(int64(modelcatalog.DefaultLiveModelsSyncInterval.Seconds()))
	}
	// Updating framework config
	shouldReloadFrameworkConfig := false
	// URLs were already normalized and checked for accessibility above.
	if payload.FrameworkConfig.PricingURL != nil && *payload.FrameworkConfig.PricingURL != *frameworkConfig.PricingURL {
		frameworkConfig.PricingURL = payload.FrameworkConfig.PricingURL
		shouldReloadFrameworkConfig = true
	}
	if payload.FrameworkConfig.PricingSyncInterval != nil {
		syncInterval := int64(*payload.FrameworkConfig.PricingSyncInterval)
		if syncInterval != *frameworkConfig.PricingSyncInterval {
			frameworkConfig.PricingSyncInterval = &syncInterval
			shouldReloadFrameworkConfig = true
		}
	}
	if payload.FrameworkConfig.ModelParametersURL != nil && *payload.FrameworkConfig.ModelParametersURL != *frameworkConfig.ModelParametersURL {
		frameworkConfig.ModelParametersURL = payload.FrameworkConfig.ModelParametersURL
		shouldReloadFrameworkConfig = true
	}
	if payload.FrameworkConfig.LiveModelsSyncInterval != nil {
		syncInterval := *payload.FrameworkConfig.LiveModelsSyncInterval
		if frameworkConfig.LiveModelsSyncInterval == nil || syncInterval != *frameworkConfig.LiveModelsSyncInterval {
			frameworkConfig.LiveModelsSyncInterval = &syncInterval
			shouldReloadFrameworkConfig = true
		}
	}
	// Reload config if required
	if shouldReloadFrameworkConfig {
		var syncSeconds int64
		if frameworkConfig.PricingSyncInterval != nil {
			syncSeconds = *frameworkConfig.PricingSyncInterval
		} else {
			syncSeconds = int64(modelcatalog.DefaultSyncInterval.Seconds())
		}
		updatedFrameworkConfig := &framework.FrameworkConfig{
			Pricing: &modelcatalog.Config{
				PricingURL:             frameworkConfig.PricingURL,
				PricingSyncInterval:    &syncSeconds,
				ModelParametersURL:     frameworkConfig.ModelParametersURL,
				LiveModelsSyncInterval: frameworkConfig.LiveModelsSyncInterval,
			},
		}
		// Persist first so a failed store write leaves the runtime config
		// untouched and in step with the database.
		if err := h.store.ConfigStore.UpdateFrameworkConfig(ctx, frameworkConfig); err != nil {
			logger.Warn("failed to save framework configuration: %v", err)
			SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to save framework configuration: %v", err))
			return
		}
		// Publish the new config under the write lock: other request goroutines
		// read this pointer through LiveModelsSyncInterval and UpdateSyncConfig.
		// A whole new struct is swapped in rather than mutated in place, which is
		// what lets readers use the pointer after releasing the lock. Scoped to
		// the assignment alone — the reload below takes the read lock itself,
		// and sync.RWMutex is not reentrant.
		h.store.Mu.Lock()
		h.store.FrameworkConfig = updatedFrameworkConfig
		h.store.Mu.Unlock()
		// Reloading pricing manager
		h.configManager.UpdateSyncConfig(ctx)
	}
	// Checking auth config and trying to update if required
	if payload.AuthConfig != nil {
		authConfig := existingAuthConfig

		// Check if auth config has changed
		authChanged := false
		if authConfig == nil {
			// No existing config, any enabled state is a change
			if payload.AuthConfig.IsEnabled {
				authChanged = true
			}
		} else {
			// Compare with existing config using value comparison (not pointer comparison)
			// Password is considered changed when it was intentionally submitted —
			// ShouldPreserveStored() returns false for both plain values and secret refs.
			passwordChanged := payload.AuthConfig.AdminPassword != nil &&
				!payload.AuthConfig.AdminPassword.ShouldPreserveStored()
			usernameChanged := payload.AuthConfig.AdminUserName != nil &&
				!payload.AuthConfig.AdminUserName.Equals(authConfig.AdminUserName)
			if payload.AuthConfig.IsEnabled != authConfig.IsEnabled ||
				usernameChanged ||
				passwordChanged {
				authChanged = true
			}
		}

		if payload.AuthConfig.IsEnabled {
			// Initialize nil pointers to empty SecretVar to prevent nil-pointer dereference
			if payload.AuthConfig.AdminUserName == nil {
				payload.AuthConfig.AdminUserName = &schemas.SecretVar{}
			}
			if payload.AuthConfig.AdminPassword == nil {
				payload.AuthConfig.AdminPassword = &schemas.SecretVar{}
			}

			// Validate env variables are set if referenced
			if payload.AuthConfig.AdminUserName.IsFromSecret() && payload.AuthConfig.AdminUserName.GetValue() == "" {
				SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("external reference %s for admin_username resolved to an empty value", payload.AuthConfig.AdminUserName.GetRawRef()))
				return
			}
			if payload.AuthConfig.AdminPassword.IsFromSecret() && payload.AuthConfig.AdminPassword.GetValue() == "" {
				SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("external reference %s for admin_password resolved to an empty value", payload.AuthConfig.AdminPassword.GetRawRef()))
				return
			}

			if authConfig == nil && (payload.AuthConfig.AdminUserName.GetValue() == "" || payload.AuthConfig.AdminPassword.GetValue() == "") {
				SendError(ctx, fasthttp.StatusBadRequest, "auth username and password must be provided")
				return
			}

			// Creating the very first admin account is the one config write this
			// endpoint allows while genuinely unauthenticated (no admin exists yet to
			// authenticate as). Require the operator-configured setup token (config.json
			// setup_token or BIFROST_SETUP_TOKEN env var) so that action can't be taken by
			// an unauthenticated network caller racing the real operator to a freshly
			// exposed instance.
			if authConfig == nil && !isSetupTokenAuthenticated(ctx) && !h.configManager.ValidateSetupToken(payload.AuthConfig.SetupToken) {
				SendError(ctx, fasthttp.StatusForbidden, "a valid setup token is required to create the initial admin account; configure setup_token in config.json (or the BIFROST_SETUP_TOKEN env var) and pass it in this request")
				return
			}
			// Stored credentials with auth switched off: the caller was admitted without any
			// credential check, so switching auth back on (or replacing the credentials in
			// the same request) must first prove control of the instance.
			if authConfig != nil && isAuthBypassed(ctx) && !h.verifyStoredAdminCredential(ctx, authConfig, payload.AuthConfig) {
				return
			}
			// Fetching current Auth config
			if payload.AuthConfig.AdminUserName.GetValue() != "" {
				if payload.AuthConfig.AdminPassword.ShouldPreserveStored() {
					if authConfig == nil || authConfig.AdminPassword.GetValue() == "" {
						SendError(ctx, fasthttp.StatusBadRequest, "auth password must be provided")
						return
					}
					// Assuming that password hasn't been changed
					payload.AuthConfig.AdminPassword = authConfig.AdminPassword
				} else {
					hashed, ok := h.hashAdminPassword(ctx, payload.AuthConfig.AdminPassword)
					if !ok {
						return
					}
					// First-time setup pre-hashed the password while validating the request
					// (initialPasswordHash); reuse that hash rather than computing a second one.
					if initialPasswordHash != "" {
						hashed.Val = initialPasswordHash
					}
					payload.AuthConfig.AdminPassword = hashed
				}
			}
			// Save auth config - this handles both first-time creation and updates
			err = h.configManager.UpdateAuthConfig(ctx, &payload.AuthConfig.AuthConfig)
			if err != nil {
				logger.Warn("failed to update auth config: %v", err)
				SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to update auth config: %v", err))
				return
			}
		} else if authConfig != nil {
			// Auth is being disabled (or kept disabled) over an existing config - preserve the
			// credentials that were not resubmitted and update the disabled state.
			passwordReplaced := !payload.AuthConfig.AdminPassword.ShouldPreserveStored()
			if !passwordReplaced {
				payload.AuthConfig.AdminPassword = authConfig.AdminPassword
			}
			if payload.AuthConfig.AdminUserName == nil || payload.AuthConfig.AdminUserName.GetValue() == "" {
				payload.AuthConfig.AdminUserName = authConfig.AdminUserName
			}
			// Replacing the stored credentials while auth stays off is the same exposure as
			// re-enabling with new ones, so it needs the same proof of control.
			credentialsReplaced := passwordReplaced || !payload.AuthConfig.AdminUserName.Equals(authConfig.AdminUserName)
			if credentialsReplaced && isAuthBypassed(ctx) && !h.verifyStoredAdminCredential(ctx, authConfig, payload.AuthConfig) {
				return
			}
			if passwordReplaced {
				hashed, ok := h.hashAdminPassword(ctx, payload.AuthConfig.AdminPassword)
				if !ok {
					return
				}
				payload.AuthConfig.AdminPassword = hashed
			}
			err = h.configManager.UpdateAuthConfig(ctx, &payload.AuthConfig.AuthConfig)
			if err != nil {
				logger.Warn("failed to update auth config: %v", err)
				SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to update auth config: %v", err))
				return
			}
		}

		// Flush all existing sessions if auth details have been changed
		if authChanged {
			if err := h.store.ConfigStore.FlushSessions(ctx); err != nil {
				logger.Warn("updated auth config but failed to flush existing sessions, please restart the server: %v", err)
				SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("updated auth config but failed to flush existing sessions, please restart the server: %v", err))
				return
			}
		}
		// Note: AuthMiddleware is updated via ServerCallbacks.UpdateAuthConfig (handled by BifrostHTTPServer)
	}

	// Set restart required flag if any restart-requiring configs changed
	if len(restartReasons) > 0 {
		reason := fmt.Sprintf("%s settings have been updated. A restart is required for changes to take full effect.", strings.Join(restartReasons, ", "))
		if err := h.store.ConfigStore.SetRestartRequiredConfig(ctx, &configstoreTables.RestartRequiredConfig{
			Required: true,
			Reason:   reason,
		}); err != nil {
			logger.Warn("failed to set restart required config: %v", err)
		}
	}

	ctx.SetStatusCode(fasthttp.StatusOK)
	SendJSON(ctx, map[string]any{
		"status":  "success",
		"message": "configuration updated successfully",
	})
}

// forceSyncPricing triggers an immediate pricing sync and resets the pricing sync timer
func (h *ConfigHandler) forceSyncPricing(ctx *fasthttp.RequestCtx) {
	if h.store.ConfigStore == nil {
		SendError(ctx, fasthttp.StatusServiceUnavailable, "config store not available")
		return
	}

	if err := h.configManager.ForceReloadPricing(ctx); err != nil {
		logger.Warn("failed to force pricing sync: %v", err)
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to force pricing sync: %v", err))
		return
	}
	lib.RecordAuditChange(ctx, "pricing.synced", "pricing", "pricing", "Pricing", nil, map[string]any{"forced": true})

	ctx.SetStatusCode(fasthttp.StatusOK)
	SendJSON(ctx, map[string]any{
		"status":  "success",
		"message": "pricing synced successfully",
	})
}

// getProxyConfig handles GET /api/proxy-config - Get the current proxy configuration
func (h *ConfigHandler) getProxyConfig(ctx *fasthttp.RequestCtx) {
	if h.store.ConfigStore == nil {
		SendError(ctx, fasthttp.StatusServiceUnavailable, "config store not available")
		return
	}
	proxyConfig, err := h.store.ConfigStore.GetProxyConfig(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to get proxy config: %v", err))
		return
	}
	if proxyConfig == nil {
		// Return default empty config
		SendJSON(ctx, configstoreTables.GlobalProxyConfig{
			Enabled: false,
			Type:    network.GlobalProxyTypeHTTP,
		})
		return
	}
	// Redact password if present
	if proxyConfig.Password != "" {
		proxyConfig.Password = "<redacted>"
	}
	proxyConfig.URL = lib.RedactProxyURLPassword(proxyConfig.URL)
	SendJSON(ctx, proxyConfig)
}

// updateProxyConfig handles PUT /api/proxy-config - Update the proxy configuration
func (h *ConfigHandler) updateProxyConfig(ctx *fasthttp.RequestCtx) {
	if h.store.ConfigStore == nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "config store not initialized")
		return
	}

	var payload configstoreTables.GlobalProxyConfig
	if err := json.Unmarshal(ctx.PostBody(), &payload); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Invalid request payload")
		return
	}
	// The settings form trims what it validates; store what the dialer will parse.
	payload.URL = strings.TrimSpace(payload.URL)
	// GET redacts a password in the URL, and the settings page sends back what it got:
	// restore the stored one before the URL is compared or validated.
	if strings.Contains(payload.URL, lib.RedactedProxyURLPassword) {
		existingConfig, err := h.store.ConfigStore.GetProxyConfig(ctx)
		if err != nil && !errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to get existing proxy config: %v", err))
			return
		}
		stored := ""
		if existingConfig != nil {
			stored = existingConfig.URL
		}
		payload.URL = lib.RestoreProxyURLPassword(payload.URL, stored)
	}

	// Under the fail-open bypass (dashboard auth disabled/unconfigured), refuse to point the
	// global proxy somewhere new or to stop verifying its TLS: either lets whoever runs the
	// proxy read the provider credentials of every proxied request.
	if isAuthBypassed(ctx) {
		existingConfig, err := h.store.ConfigStore.GetProxyConfig(ctx)
		if err != nil && !errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to get existing proxy config: %v", err))
			return
		}
		if changed := globalProxyInterceptionChanges(existingConfig, payload); len(changed) > 0 {
			SendError(ctx, fasthttp.StatusForbidden, fmt.Sprintf("Changing the global proxy (%s) requires an authenticated admin session; dashboard auth is currently disabled or unconfigured. Enable dashboard authentication first.", strings.Join(changed, ", ")))
			return
		}
	}

	// Validate proxy config
	if payload.Enabled {
		// Validate proxy type. http and socks5 have dialers in the HTTP client
		// factory and the provider stacks (network.DialViaProxy); tcp has none.
		switch payload.Type {
		case network.GlobalProxyTypeHTTP, network.GlobalProxyTypeSOCKS5:
		case network.GlobalProxyTypeTCP:
			SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("proxy type %s is not yet supported", payload.Type))
			return
		default:
			SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("invalid proxy type: %s", payload.Type))
			return
		}

		// Validate URL is provided when enabled
		if payload.URL == "" {
			SendError(ctx, fasthttp.StatusBadRequest, "proxy URL is required when proxy is enabled")
			return
		}

		proxyURL, err := lib.ValidateGlobalProxyURL(payload.Type, payload.URL)
		if err != nil {
			SendError(ctx, fasthttp.StatusBadRequest, err.Error())
			return
		}
		// Every proxied request carries its provider credentials to this host, so the
		// proxy URL follows the same destination rule as a provider base URL. Private
		// and loopback hosts stay allowed (a self-hosted egress proxy is the normal
		// setup); link-local and unspecified addresses never are. The rule is about
		// the host, so it is checked the same way whatever the proxy scheme.
		if err := bifrost.ValidateExternalURL("http://"+proxyURL.Host, true); err != nil {
			SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid proxy URL: %v", err))
			return
		}

		// Validate timeout if provided
		if payload.Timeout < 0 {
			SendError(ctx, fasthttp.StatusBadRequest, "proxy timeout must be non-negative")
			return
		}
	}

	// A proxy CA that is not a PEM certificate would be ignored by every client that
	// should trust it, and proxied TLS would then fail with no hint why.
	if strings.TrimSpace(payload.CACertPEM) != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(payload.CACertPEM)) {
		SendError(ctx, fasthttp.StatusBadRequest, "proxy ca_cert_pem holds no valid PEM certificate")
		return
	}

	// Handle password - if it's "<redacted>", keep the existing password
	if payload.Password == "<redacted>" {
		existingConfig, err := h.store.ConfigStore.GetProxyConfig(ctx)
		if err != nil && !errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to get existing proxy config: %v", err))
			return
		}
		if existingConfig != nil {
			payload.Password = existingConfig.Password
		} else {
			payload.Password = ""
		}
	}

	existingProxyConfig, err := h.store.ConfigStore.GetProxyConfig(ctx)
	if err != nil && !errors.Is(err, configstore.ErrNotFound) {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to get existing proxy config: %v", err))
		return
	}
	before := auditProxyConfig(existingProxyConfig)
	// Save proxy config
	if err := h.store.ConfigStore.UpdateProxyConfig(ctx, &payload); err != nil {
		logger.Warn("failed to save proxy configuration: %v", err)
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to save proxy configuration: %v", err))
		return
	}
	// Recorded once saved: the reload below can fail without undoing the write.
	lib.RecordAuditChange(ctx, "proxy_config.updated", "proxy_config", "proxy_config", "proxy_config", before, auditProxyConfig(&payload))

	// Pulling the proxy config from the config store
	newProxyConfig, err := h.store.ConfigStore.GetProxyConfig(ctx)
	if err != nil {
		logger.Warn("failed to get proxy config from store: %v", err)
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to get proxy config from store: %v", err))
		return
	}
	if newProxyConfig == nil {
		newProxyConfig = &configstoreTables.GlobalProxyConfig{
			Enabled:       false,
			Type:          network.GlobalProxyTypeHTTP,
			URL:           "",
			Username:      "",
			Password:      "",
			NoProxy:       "",
			Timeout:       0,
			SkipTLSVerify: false,
		}
	}

	// Reload proxy config in the server
	if err := h.configManager.ReloadProxyConfig(ctx, newProxyConfig); err != nil {
		logger.Warn("failed to reload proxy config: %v", err)
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to reload proxy config: %v", err))
		return
	}

	// Set restart required flag for proxy config changes
	if err := h.store.ConfigStore.SetRestartRequiredConfig(ctx, &configstoreTables.RestartRequiredConfig{
		Required: true,
		Reason:   "Proxy configuration has been updated. A restart is required for all changes to take full effect.",
	}); err != nil {
		logger.Warn("failed to set restart required config: %v", err)
	}

	ctx.SetStatusCode(fasthttp.StatusOK)
	SendJSON(ctx, map[string]any{
		"status":  "success",
		"message": "proxy configuration updated successfully",
	})
}

// headerFilterConfigEqual compares two GlobalHeaderFilterConfig for equality
func headerFilterConfigEqual(a, b *configstoreTables.GlobalHeaderFilterConfig) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return slices.Equal(a.Allowlist, b.Allowlist) && slices.Equal(a.Denylist, b.Denylist)
}

// validateHeaderFilterConfig validates that no exact security header names are in the allowlist or denylist
// and that wildcard patterns use valid syntax (only trailing * is supported).
// Wildcard patterns that would match security headers are allowed because security headers
// are unconditionally stripped at runtime regardless of configuration.
// Returns an error if any exact security headers are found or patterns are invalid.
func validateHeaderFilterConfig(config *configstoreTables.GlobalHeaderFilterConfig) error {
	if config == nil {
		return nil
	}

	// Validate pattern syntax and normalize entries (trim, lowercase, drop empties)
	filteredAllow := config.Allowlist[:0]
	for _, header := range config.Allowlist {
		h := strings.ToLower(strings.TrimSpace(header))
		if h == "" {
			continue
		}
		if idx := strings.Index(h, "*"); idx != -1 && idx != len(h)-1 {
			return fmt.Errorf("invalid pattern %q: wildcard (*) is only supported at the end of a pattern", h)
		}
		filteredAllow = append(filteredAllow, h)
	}
	config.Allowlist = filteredAllow
	filteredDeny := config.Denylist[:0]
	for _, header := range config.Denylist {
		h := strings.ToLower(strings.TrimSpace(header))
		if h == "" {
			continue
		}
		if idx := strings.Index(h, "*"); idx != -1 && idx != len(h)-1 {
			return fmt.Errorf("invalid pattern %q: wildcard (*) is only supported at the end of a pattern", h)
		}
		filteredDeny = append(filteredDeny, h)
	}
	config.Denylist = filteredDeny

	var foundSecurityHeaders []string

	// Check allowlist for exact security header names.
	// Wildcard patterns are allowed — security headers are always stripped at runtime
	// unconditionally in ctx.go, regardless of allowlist/denylist configuration.
	for _, header := range config.Allowlist {
		headerLower := strings.ToLower(strings.TrimSpace(header))
		if strings.Contains(headerLower, "*") {
			continue
		}
		if slices.Contains(securityHeaders, headerLower) {
			foundSecurityHeaders = append(foundSecurityHeaders, headerLower)
		}
	}

	// Check denylist for exact security header names.
	for _, header := range config.Denylist {
		headerLower := strings.ToLower(strings.TrimSpace(header))
		if strings.Contains(headerLower, "*") {
			continue
		}
		if slices.Contains(securityHeaders, headerLower) && !slices.Contains(foundSecurityHeaders, headerLower) {
			foundSecurityHeaders = append(foundSecurityHeaders, headerLower)
		}
	}

	if len(foundSecurityHeaders) > 0 {
		return fmt.Errorf("the following headers are not allowed to be configured: %s. These headers are security headers and are always blocked", strings.Join(foundSecurityHeaders, ", "))
	}

	return nil
}

// checkURLAccessibilityDialContext is the dial function checkURLAccessibility's
// HTTP client uses. Overridable in tests that need to reach a loopback-bound
// httptest.Server; production code must never reassign it.
var checkURLAccessibilityDialContext = network.SSRFSafeDialContext(10 * time.Second)

// errURLNotReachable is the only failure checkURLAccessibility reports for a
// URL that could not be fetched, whatever the cause.
var errURLNotReachable = errors.New("url is not reachable")

// sameAsStoredURL reports whether a framework URL in a config save equals the
// stored one. Such a URL needs no accessibility check: it was set in
// config.json, where file:// and private hosts are allowed, or it passed the
// check when it was first saved over the API.
func sameAsStoredURL(submitted string, stored *string) bool {
	return stored != nil && *stored == submitted
}

// errFileURLOverAPI is returned for a file:// URL supplied over the API.
var errFileURLOverAPI = errors.New("file:// URLs can only be configured in config.json, not over the API")

// checkURLAccessibility verifies that a datasheet or catalog URL supplied over
// the API is an http(s) URL that answers 200 OK.
//
// file:// URLs are refused here. They are an operator feature for values set
// in config.json (air-gapped deployments; see
// docs/deployment-guides/how-to/airgapped.mdx), where whoever writes the file
// already has the host's filesystem. Accepting one from an API caller would let
// that caller name any path on the host for the catalog loaders to read.
//
// This runs against an admin-supplied URL, and no documented deployment points
// these at a private-network HTTP(S) target, so private/link-local/CGNAT
// addresses are rejected outright: both the save-time hostname check
// (ValidateExternalURL) and the actual dial are guarded, since a save-time-only
// check leaves a DNS-rebinding window, and the dial guard applies to every
// redirect hop. A failed fetch reports only errURLNotReachable: the transport
// error names the address that was dialed and, for a non-HTTP service, quotes
// the bytes it could not parse, which would turn this endpoint into a
// port-scan and banner oracle for anyone who can call it. That detail is
// useful to the operator, so it goes to the server log only.
func checkURLAccessibility(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if strings.EqualFold(parsed.Scheme, "file") {
		return errFileURLOverAPI
	}
	if err := bifrost.ValidateExternalURL(rawURL, false); err != nil {
		return fmt.Errorf("URL validation failed: %w", err)
	}
	client := &http.Client{
		Timeout: 60 * time.Second,
		// The global proxy when it is enabled for API traffic, else the environment's:
		// the sync that follows a successful check uses the same. A direct dial keeps
		// the public-only check; a proxied request dials only the proxy, which may be
		// on a private address, and goes to the host ValidateExternalURL just checked.
		Transport: &network.ProxyAwareTransport{
			Proxy:    network.DefaultProxyFunc(network.ClientPurposeAPI),
			Direct:   &http.Transport{DialContext: checkURLAccessibilityDialContext},
			ViaProxy: &http.Transport{Proxy: network.DefaultProxyFunc(network.ClientPurposeAPI), DialContext: network.PrivateNetworkDialContext(10 * time.Second)},
		},
		// The operator validated this URL, not wherever it redirects: a redirect
		// is returned as-is and fails the 200 check below instead of being followed.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	reqCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		// Log the host and the underlying transport error only: the full URL
		// can carry userinfo or query secrets, and *url.Error echoes it back.
		logErr := err
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			logErr = urlErr.Err
		}
		logger.Warn(fmt.Sprintf("URL accessibility check failed for host %s: %v", parsed.Hostname(), logErr))
		return errURLNotReachable
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		logger.Debug("url accessibility check for host %s returned HTTP %d", parsed.Hostname(), resp.StatusCode)
		return errURLNotReachable
	}
	return nil
}

// hashAdminPassword applies the password policy to a newly submitted admin password and
// returns its hash, keeping env/vault reference metadata so the stored value still records
// where the password came from. On failure it sends the response and returns false.
func (h *ConfigHandler) hashAdminPassword(ctx *fasthttp.RequestCtx, password *schemas.SecretVar) (*schemas.SecretVar, bool) {
	if failures := getPasswordPolicyFailures(password.GetValue()); len(failures) > 0 {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("auth password must include %s", strings.Join(failures, ", ")))
		return nil, false
	}
	hashed, err := encrypt.Hash(password.GetValue())
	if err != nil {
		logger.Warn("failed to hash password: %v", err)
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to hash password: %v", err))
		return nil, false
	}
	if password.IsFromSecret() {
		sv := *password
		sv.Val = hashed
		return &sv, true
	}
	return &schemas.SecretVar{Val: hashed}, true
}

// verifyStoredAdminCredential authorizes an auth_config change made while dashboard auth is
// disabled but admin credentials are stored. Such a request reached the handler without any
// credential check (BifrostContextKeyAuthBypassed), so switching auth back on or replacing
// the stored credentials must prove control of the instance: either current_password matches
// the stored admin password, or setup_token matches the operator-configured setup token
// (which, unlike the first-admin gate, stays valid for the life of the process). Without that
// proof anyone who can reach the port could install their own admin account. On failure it
// sends the 403 and returns false.
func (h *ConfigHandler) verifyStoredAdminCredential(ctx *fasthttp.RequestCtx, stored *configstore.AuthConfig, payload *authConfigWithSetupToken) bool {
	if payload.CurrentPassword != "" && stored.AdminPassword != nil {
		if ok, err := encrypt.CompareHash(stored.AdminPassword.GetValue(), payload.CurrentPassword); err == nil && ok {
			return true
		}
	}
	if payload.SetupToken != "" && h.configManager.ValidateConfiguredSetupToken(payload.SetupToken) {
		return true
	}
	SendError(ctx, fasthttp.StatusForbidden, "dashboard auth is disabled but an admin account exists; re-enabling it or changing the admin credentials requires current_password (the stored admin password) or a valid setup_token")
	return false
}

// globalProxyInterceptionChanges returns the global proxy settings next adds or changes,
// relative to old (nil when none is stored), that widen who can read proxied traffic: a new
// proxy URL or TLS verification turned off. Keeping the stored URL while editing other fields,
// disabling the proxy, or turning verification back on is not reported.
func globalProxyInterceptionChanges(old *configstoreTables.GlobalProxyConfig, next configstoreTables.GlobalProxyConfig) []string {
	var prev configstoreTables.GlobalProxyConfig
	if old != nil {
		prev = *old
	}
	var changed []string
	if next.URL != "" && next.URL != prev.URL {
		changed = append(changed, "url")
	}
	if next.SkipTLSVerify && !prev.SkipTLSVerify {
		changed = append(changed, "skip_tls_verify")
	}
	// The type picks the dialer, so the same URL under another type is another proxy
	// protocol (a bare host:port takes its scheme from the type, and no type dials as http).
	if (prev.Type != "" || prev.URL != "") && effectiveGlobalProxyType(next.Type) != effectiveGlobalProxyType(prev.Type) {
		changed = append(changed, "type")
	}
	// The proxy CA is trusted for targets reached through the proxy, so a new one lets
	// the proxy sign provider certificates. Removing it only narrows trust.
	if ca := strings.TrimSpace(next.CACertPEM); ca != "" && ca != strings.TrimSpace(prev.CACertPEM) {
		changed = append(changed, "ca_cert_pem")
	}
	return changed
}

// effectiveGlobalProxyType is the type the dialer uses: an empty type dials as http.
func effectiveGlobalProxyType(t network.GlobalProxyType) network.GlobalProxyType {
	if t == "" {
		return network.GlobalProxyTypeHTTP
	}
	return t
}

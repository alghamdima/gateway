package configstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
)

type EnvKeyType string

const (
	EnvKeyTypeAPIKey        EnvKeyType = "api_key"
	EnvKeyTypeAzureConfig   EnvKeyType = "azure_config"
	EnvKeyTypeVertexConfig  EnvKeyType = "vertex_config"
	EnvKeyTypeBedrockConfig EnvKeyType = "bedrock_config"
	EnvKeyTypeConnection    EnvKeyType = "connection_string"
)

// MaxVKRotationCooldown is the hard ceiling on the virtual key rotation grace
// period. Anything longer keeps a retired credential authenticating for over a
// month, which defeats the point of rotating it. Enforced at every config
// entry point (PUT /api/config and the config.json load path), mirroring
// MaxAuthCodeTTL, and matching the maximum published in config.schema.json.
const MaxVKRotationCooldown = 30 * 24 * time.Hour

// EnvKeyInfo stores information about a key sourced from environment
type EnvKeyInfo struct {
	SecretVar  string                // The environment variable name (without env. prefix)
	Provider   schemas.ModelProvider // The provider this key belongs to (empty for core configs)
	KeyType    EnvKeyType            // Type of key (e.g., "api_key", "azure_config", "vertex_config", "bedrock_config", "connection_string")
	ConfigPath string                // Path in config where this env var is used
	KeyID      string                // The key ID this env var belongs to (empty for non-key configs like bedrock_config, connection_string)
}

// CompatConfig holds the compat plugin feature flags.
type CompatConfig struct {
	ConvertTextToChat                   bool `json:"convert_text_to_chat"`
	ConvertChatToResponses              bool `json:"convert_chat_to_responses"`
	ShouldDropParams                    bool `json:"should_drop_params"`
	ShouldConvertParams                 bool `json:"should_convert_params"`
	AzureDeepseek                       bool `json:"azure_deepseek"`
	ForceReasoningOnlyModelsToResponses bool `json:"force_reasoning_only_models_to_responses"`
	ConvertDecisionToResponses          bool `json:"convert_decision_to_responses"`
}

// UnmarshalJSON defaults all bool fields to true when absent from JSON, except
// ConvertDecisionToResponses: emulating a decision through a chat model is opt-in.
func (c *CompatConfig) UnmarshalJSON(data []byte) error {
	type compatConfig struct {
		ConvertTextToChat                   *bool `json:"convert_text_to_chat"`
		ConvertChatToResponses              *bool `json:"convert_chat_to_responses"`
		ShouldDropParams                    *bool `json:"should_drop_params"`
		ShouldConvertParams                 *bool `json:"should_convert_params"`
		AzureDeepseek                       *bool `json:"azure_deepseek"`
		ForceReasoningOnlyModelsToResponses *bool `json:"force_reasoning_only_models_to_responses"`
		ConvertDecisionToResponses          *bool `json:"convert_decision_to_responses"`
	}
	var s compatConfig
	if err := sonic.Unmarshal(data, &s); err != nil {
		return err
	}
	c.ConvertTextToChat = s.ConvertTextToChat == nil || *s.ConvertTextToChat
	c.ConvertChatToResponses = s.ConvertChatToResponses == nil || *s.ConvertChatToResponses
	c.ShouldDropParams = s.ShouldDropParams == nil || *s.ShouldDropParams
	c.ShouldConvertParams = s.ShouldConvertParams == nil || *s.ShouldConvertParams
	c.AzureDeepseek = s.AzureDeepseek == nil || *s.AzureDeepseek
	c.ForceReasoningOnlyModelsToResponses = s.ForceReasoningOnlyModelsToResponses == nil || *s.ForceReasoningOnlyModelsToResponses
	c.ConvertDecisionToResponses = s.ConvertDecisionToResponses != nil && *s.ConvertDecisionToResponses
	return nil
}

// ClientConfig represents the core configuration for Bifrost HTTP transport and the Bifrost Client.
// It includes settings for excess request handling, Prometheus metrics, and initial pool size.
type ClientConfig struct {
	// Input-only presence information; never persisted or included in responses/hashes.
	inferenceAuthProvided bool

	DropExcessRequests                    bool                                  `json:"drop_excess_requests"`                       // Drop excess requests if the provider queue is full
	InitialPoolSize                       int                                   `json:"initial_pool_size"`                          // The initial pool size for the bifrost client
	PrometheusLabels                      []string                              `json:"prometheus_labels"`                          // The labels to be used for prometheus metrics
	EnableLogging                         *bool                                 `json:"enable_logging"`                             // Enable logging of requests and responses
	DisableContentLogging                 bool                                  `json:"disable_content_logging"`                    // Disable logging of content
	RetainContentInObjectStorage          bool                                  `json:"retain_content_in_object_storage"`           // When content logging is disabled (config or header), still offload content to object storage as hidden instead of dropping it
	AllowPerRequestContentStorageOverride bool                                  `json:"allow_per_request_content_storage_override"` // Allow per-request override of content storage via x-bf-disable-content-logging header/context
	AllowPerRequestRawOverride            bool                                  `json:"allow_per_request_raw_override"`             // Allow per-request override of raw request/response visibility via x-bf-send-back-raw-request and x-bf-send-back-raw-response headers
	AllowDirectKeys                       bool                                  `json:"allow_direct_keys"`                          // Allow callers to bypass the registered key pool via x-bf-direct-key: true header
	VKRotationCooldown                    schemas.Duration                      `json:"vk_rotation_cooldown,omitempty"`             // Grace period during which a rotated virtual key's previous value still authenticates (e.g. "5m"); 0 = old value stops working immediately
	DisableDBPingsInHealth                bool                                  `json:"disable_db_pings_in_health"`
	LogRetentionDays                      int                                   `json:"log_retention_days" validate:"min=1"`         // Number of days to retain logs (minimum 1 day)
	EnforceAuthOnInference                bool                                  `json:"enforce_auth_on_inference"`                   // Require auth (VK, API key, or user token) on inference endpoints
	DualCredentialConflictBehavior        tables.DualCredentialConflictBehavior `json:"dual_credential_conflict_behavior,omitempty"` // Behavior when both an IDP token and a VK are present on an inference request
	ExtraFieldsPlacement                  tables.ExtraFieldsPlacement           `json:"extra_fields_placement,omitempty"`            // Where inference responses carry extra_fields: "body" (default) or "headers"
	EnforceGovernanceHeader               bool                                  `json:"enforce_governance_header,omitempty"`         // Deprecated: use EnforceAuthOnInference
	EnforceSCIMAuth                       bool                                  `json:"enforce_scim_auth,omitempty"`                 // Deprecated: use EnforceAuthOnInference
	AllowedOrigins                        []string                              `json:"allowed_origins,omitempty"`                   // Additional allowed origins for CORS and WebSocket (localhost is always allowed)
	AllowedHeaders                        []string                              `json:"allowed_headers,omitempty"`                   // Additional allowed headers for CORS and WebSocket
	MaxRequestBodySizeMB                  int                                   `json:"max_request_body_size_mb"`                    // The maximum request body size in MB
	Compat                                CompatConfig                          `json:"compat"`                                      // Compat plugin configuration
	HeaderFilterConfig                    *tables.GlobalHeaderFilterConfig      `json:"header_filter_config,omitempty"`              // Global header filtering configuration for x-bf-eh-* headers
	AsyncJobResultTTL                     int                                   `json:"async_job_result_ttl"`                        // Default TTL for async job results in seconds (default: 3600 = 1 hour)
	RequiredHeaders                       []string                              `json:"required_headers,omitempty"`                  // Headers that must be present on every request (case-insensitive)
	LoggingHeaders                        []string                              `json:"logging_headers,omitempty"`                   // Headers to capture in log metadata
	WhitelistedRoutes                     []string                              `json:"whitelisted_routes,omitempty"`                // Routes that bypass auth middleware
	HideDeletedVirtualKeysInFilters       bool                                  `json:"hide_deleted_virtual_keys_in_filters"`        // Hide deleted virtual keys from logs filter data
	DeleteExpiredVirtualKeys              bool                                  `json:"delete_expired_virtual_keys"`                 // Delete expired virtual keys by default; a key's delete_after_expire overrides this
	EnforceVKObjectOwnership              bool                                  `json:"enforce_vk_object_ownership"`                 // Restrict provider objects (files, batches, containers, ...) to the virtual key that created them
	HiddenRequestTypes                    []string                              `json:"hidden_request_types,omitempty"`              // Request types excluded from dashboard and log API reads; logs are still written
	UsageEstimationTokenizers             []string                              `json:"usage_estimation_tokenizers,omitempty"`       // Embedded tokenizer families ("anthropic", "deepseek", "llama2", "llama3", "all") used to estimate a cancelled stream's usage; empty uses only o200k_base/cl100k_base
	RoutingChainMaxDepth                  int                                   `json:"routing_chain_max_depth"`                     // Maximum depth for routing rule chain evaluation (default: 10)
	ConfigHash                            string                                `json:"-"`                                           // Config hash for reconciliation (not serialized)
	DumpErrorsInConsoleLogs               bool                                  `json:"dump_errors_in_console_logs"`                 // Dump error details in console logs
}

// UnmarshalJSON defaults all bool fields to true when absent from JSON.
func (c *ClientConfig) UnmarshalJSON(data []byte) error {
	type ClientConfigAlias ClientConfig
	alias := ClientConfigAlias{
		EnforceVKObjectOwnership: true, // an omitted field keeps provider objects bound to their creator
		Compat: CompatConfig{
			ConvertTextToChat:                   true,
			ConvertChatToResponses:              true,
			ShouldDropParams:                    true,
			ShouldConvertParams:                 true,
			AzureDeepseek:                       true,
			ForceReasoningOnlyModelsToResponses: true,
		},
	}
	input := struct {
		*ClientConfigAlias
		EnforceAuthOnInference *bool `json:"enforce_auth_on_inference"`
	}{ClientConfigAlias: &alias}
	if err := sonic.Unmarshal(data, &input); err != nil {
		return err
	}
	*c = ClientConfig(alias)
	c.inferenceAuthProvided = input.EnforceAuthOnInference != nil
	if input.EnforceAuthOnInference != nil {
		c.EnforceAuthOnInference = *input.EnforceAuthOnInference
	}
	return nil
}

// HasInferenceAuthSetting distinguishes an explicit JSON opt-out from omission.
func (c *ClientConfig) HasInferenceAuthSetting() bool { return c.inferenceAuthProvided }

// GenerateClientConfigHash generates a SHA256 hash of the client configuration.
// This is used to detect changes between config.json and database config.
func (c *ClientConfig) GenerateClientConfigHash() (string, error) {
	hash := sha256.New()

	// Hash boolean fields
	if c.DropExcessRequests {
		hash.Write([]byte("dropExcessRequests:true"))
	} else {
		hash.Write([]byte("dropExcessRequests:false"))
	}

	enableLogging := c.EnableLogging == nil || *c.EnableLogging
	if enableLogging {
		hash.Write([]byte("enableLogging:true"))
	} else {
		hash.Write([]byte("enableLogging:false"))
	}

	if c.DisableContentLogging {
		hash.Write([]byte("disableContentLogging:true"))
	} else {
		hash.Write([]byte("disableContentLogging:false"))
	}

	if c.DisableDBPingsInHealth {
		hash.Write([]byte("disableDBPingsInHealth:true"))
	} else {
		hash.Write([]byte("disableDBPingsInHealth:false"))
	}

	if c.EnforceAuthOnInference {
		hash.Write([]byte("enforceAuthOnInference:true"))
	} else {
		hash.Write([]byte("enforceAuthOnInference:false"))
	}

	if c.DualCredentialConflictBehavior != "" && c.DualCredentialConflictBehavior != tables.DualCredentialConflictBehaviorPreferIDP {
		hash.Write([]byte("dualCredentialConflictBehavior:" + string(c.DualCredentialConflictBehavior)))
	}

	// Only hash non-default value to avoid legacy config hash churn on upgrade.
	if c.ExtraFieldsPlacement != "" && c.ExtraFieldsPlacement != tables.ExtraFieldsPlacementBody {
		hash.Write([]byte("extraFieldsPlacement:" + string(c.ExtraFieldsPlacement)))
	}

	if c.Compat.ConvertTextToChat {
		hash.Write([]byte("compatConvertTextToChat:true"))
	}
	if c.Compat.ConvertChatToResponses {
		hash.Write([]byte("compatConvertChatToResponses:true"))
	}
	if c.Compat.ShouldDropParams {
		hash.Write([]byte("compatShouldDropParams:true"))
	}
	if c.Compat.ShouldConvertParams {
		hash.Write([]byte("compatShouldConvertParams:true"))
	}
	if c.Compat.AzureDeepseek {
		hash.Write([]byte("compatAzureDeepseek:true"))
	}
	if !c.Compat.ForceReasoningOnlyModelsToResponses {
		hash.Write([]byte("compatForceReasoningOnlyModelsToResponses:false"))
	}
	if c.Compat.ConvertDecisionToResponses {
		hash.Write([]byte("compatConvertDecisionToResponses:true"))
	}

	// Only hash non-default value to avoid legacy config hash churn.
	if c.HideDeletedVirtualKeysInFilters {
		hash.Write([]byte("hideDeletedVirtualKeysInFilters:true"))
	}
	if c.DeleteExpiredVirtualKeys {
		hash.Write([]byte("deleteExpiredVirtualKeys:true"))
	}
	// Default is true; only hash the opt-out to avoid config hash churn on upgrade.
	if !c.EnforceVKObjectOwnership {
		hash.Write([]byte("enforceVKObjectOwnership:false"))
	}

	// Always hash when non-zero — explicitly setting the default (10) is a meaningful
	// config change that should be reflected in the hash. The migration that introduces
	// this field backfills existing rows with RoutingChainMaxDepth=10 and regenerates
	// their config_hash so there is no hash churn on upgrade for unmodified configs.
	if c.RoutingChainMaxDepth > 0 {
		hash.Write([]byte("routingChainMaxDepth:" + strconv.Itoa(c.RoutingChainMaxDepth)))
	}

	// Only hash non-default value to avoid legacy config hash churn on upgrade.
	if c.AllowPerRequestContentStorageOverride {
		hash.Write([]byte("allowPerRequestContentStorageOverride:true"))
	}

	// Only hash non-default value to avoid legacy config hash churn on upgrade.
	if c.RetainContentInObjectStorage {
		hash.Write([]byte("retainContentInObjectStorage:true"))
	}

	if c.AllowPerRequestRawOverride {
		hash.Write([]byte("allowPerRequestRawOverride:true"))
	}

	// Only hash non-default value to avoid legacy config hash churn on upgrade.
	if c.AllowDirectKeys {
		hash.Write([]byte("allowDirectKeys:true"))
	}

	// Only hash non-default value to avoid legacy config hash churn on upgrade.
	if c.VKRotationCooldown > 0 {
		hash.Write([]byte("vkRotationCooldown:" + strconv.FormatInt(int64(c.VKRotationCooldown), 10)))
	}

	if c.AsyncJobResultTTL > 0 {
		hash.Write([]byte("asyncJobResultTTL:" + strconv.Itoa(c.AsyncJobResultTTL)))
	} else {
		hash.Write([]byte("asyncJobResultTTL:0"))
	}

	// Only hash non-default value to avoid legacy config hash churn on upgrade.
	if c.DumpErrorsInConsoleLogs {
		hash.Write([]byte("dumpErrorsInConsoleLogs:true"))
	}

	// Hash integer fields
	data, err := sonic.Marshal(c.InitialPoolSize)
	if err != nil {
		return "", err
	}
	hash.Write(data)

	data, err = sonic.Marshal(c.LogRetentionDays)
	if err != nil {
		return "", err
	}
	hash.Write(data)

	data, err = sonic.Marshal(c.MaxRequestBodySizeMB)
	if err != nil {
		return "", err
	}
	hash.Write(data)

	// Hash PrometheusLabels (sorted for deterministic hashing)
	if len(c.PrometheusLabels) > 0 {
		sortedLabels := make([]string, len(c.PrometheusLabels))
		copy(sortedLabels, c.PrometheusLabels)
		sort.Strings(sortedLabels)
		data, err := sonic.Marshal(sortedLabels)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}

	// Hash AllowedOrigins (sorted for deterministic hashing)
	if len(c.AllowedOrigins) > 0 {
		sortedOrigins := make([]string, len(c.AllowedOrigins))
		copy(sortedOrigins, c.AllowedOrigins)
		sort.Strings(sortedOrigins)
		data, err := sonic.Marshal(sortedOrigins)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}

	// Hash AllowedHeaders (sorted for deterministic hashing)
	if len(c.AllowedHeaders) > 0 {
		sortedHeaders := make([]string, len(c.AllowedHeaders))
		copy(sortedHeaders, c.AllowedHeaders)
		sort.Strings(sortedHeaders)
		data, err := sonic.Marshal(sortedHeaders)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}

	// Hash LoggingHeaders (sorted for deterministic hashing)
	if len(c.LoggingHeaders) > 0 {
		sortedLogging := make([]string, len(c.LoggingHeaders))
		copy(sortedLogging, c.LoggingHeaders)
		sort.Strings(sortedLogging)
		data, err := sonic.Marshal(sortedLogging)
		if err != nil {
			return "", err
		}
		hash.Write([]byte("loggingHeaders:"))
		hash.Write(data)
	}

	// Hash HiddenRequestTypes (sorted for deterministic hashing). Only hashed when
	// set so existing config hashes do not churn on upgrade.
	if len(c.HiddenRequestTypes) > 0 {
		sortedHidden := make([]string, len(c.HiddenRequestTypes))
		copy(sortedHidden, c.HiddenRequestTypes)
		sort.Strings(sortedHidden)
		data, err := sonic.Marshal(sortedHidden)
		if err != nil {
			return "", err
		}
		hash.Write([]byte("hiddenRequestTypes:"))
		hash.Write(data)
	}

	// Hash UsageEstimationTokenizers (sorted for deterministic hashing). Only
	// hashed when set so existing config hashes do not churn on upgrade.
	if len(c.UsageEstimationTokenizers) > 0 {
		sortedTokenizers := make([]string, len(c.UsageEstimationTokenizers))
		copy(sortedTokenizers, c.UsageEstimationTokenizers)
		sort.Strings(sortedTokenizers)
		data, err := sonic.Marshal(sortedTokenizers)
		if err != nil {
			return "", err
		}
		hash.Write([]byte("usageEstimationTokenizers:"))
		hash.Write(data)
	}

	// Hash RequiredHeaders (sorted for deterministic hashing)
	if len(c.RequiredHeaders) > 0 {
		sortedRequired := make([]string, len(c.RequiredHeaders))
		copy(sortedRequired, c.RequiredHeaders)
		sort.Strings(sortedRequired)
		data, err := sonic.Marshal(sortedRequired)
		if err != nil {
			return "", err
		}
		hash.Write([]byte("requiredHeaders:"))
		hash.Write(data)
	}

	// Hash WhitelistedRoutes (sorted for deterministic hashing)
	if len(c.WhitelistedRoutes) > 0 {
		sortedRoutes := make([]string, len(c.WhitelistedRoutes))
		copy(sortedRoutes, c.WhitelistedRoutes)
		sort.Strings(sortedRoutes)
		data, err := sonic.Marshal(sortedRoutes)
		if err != nil {
			return "", err
		}
		hash.Write([]byte("whitelistedRoutes:"))
		hash.Write(data)
	}

	// Hash HeaderFilterConfig
	if c.HeaderFilterConfig != nil {
		// Hash Allowlist (sorted for deterministic hashing)
		if len(c.HeaderFilterConfig.Allowlist) > 0 {
			sortedAllowlist := make([]string, len(c.HeaderFilterConfig.Allowlist))
			copy(sortedAllowlist, c.HeaderFilterConfig.Allowlist)
			sort.Strings(sortedAllowlist)
			data, err := sonic.Marshal(sortedAllowlist)
			if err != nil {
				return "", err
			}
			hash.Write([]byte("headerFilterConfig.allowlist:"))
			hash.Write(data)
		}
		// Hash Denylist (sorted for deterministic hashing)
		if len(c.HeaderFilterConfig.Denylist) > 0 {
			sortedDenylist := make([]string, len(c.HeaderFilterConfig.Denylist))
			copy(sortedDenylist, c.HeaderFilterConfig.Denylist)
			sort.Strings(sortedDenylist)
			data, err := sonic.Marshal(sortedDenylist)
			if err != nil {
				return "", err
			}
			hash.Write([]byte("headerFilterConfig.denylist:"))
			hash.Write(data)
		}
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Redacted returns a copy of ClientConfig with any env-backed SecretVar fields masked.
func (c *ClientConfig) Redacted() ClientConfig {
	out := *c
	return out
}

// ProviderConfig represents the configuration for a specific AI model provider.
// It includes API keys, network settings, and concurrency settings.
type ProviderConfig struct {
	Keys                     []schemas.Key                     `json:"keys"`                                  // API keys for the provider with UUIDs
	NetworkConfig            *schemas.NetworkConfig            `json:"network_config,omitempty"`              // Network-related settings
	ConcurrencyAndBufferSize *schemas.ConcurrencyAndBufferSize `json:"concurrency_and_buffer_size,omitempty"` // Concurrency settings
	ProxyConfig              *schemas.ProxyConfig              `json:"proxy_config,omitempty"`                // Proxy configuration
	SendBackRawRequest       bool                              `json:"send_back_raw_request"`                 // Include raw request in BifrostResponse
	SendBackRawResponse      bool                              `json:"send_back_raw_response"`                // Include raw response in BifrostResponse
	StoreRawRequestResponse  bool                              `json:"store_raw_request_response"`            // Capture raw request/response for internal logging only; strip from API responses returned to clients
	IgnoreProviderCost       bool                              `json:"ignore_provider_cost"`                  // Ignore provider-reported usage.cost and price from Bifrost's catalog
	CustomProviderConfig     *schemas.CustomProviderConfig     `json:"custom_provider_config,omitempty"`      // Custom provider configuration
	OpenAIConfig             *schemas.OpenAIConfig             `json:"openai_config,omitempty"`               // OpenAI-specific configuration
	PromptCache              *schemas.PromptCacheConfig        `json:"prompt_cache,omitempty"`                // Prompt-cache breakpoint injection
	Passthrough              *schemas.PassthroughPolicy        `json:"passthrough,omitempty"`                 // Routes the HTTP relays may forward
	ConfigHash               string                            `json:"config_hash,omitempty"`                 // Hash of config.json version, used for change detection
	Status                   string                            `json:"status,omitempty"`                      // Model discovery status for keyless providers
	Description              string                            `json:"description,omitempty"`                 // Model discovery error message for keyless providers
}

// Redacted returns a redacted copy of the provider configuration.
func (p *ProviderConfig) Redacted() *ProviderConfig {
	// Create redacted config with same structure but redacted values
	var redactedNetworkConfig *schemas.NetworkConfig
	if p.NetworkConfig != nil {
		redactedNetworkConfig = p.NetworkConfig.Redacted()
	}
	redactedConfig := ProviderConfig{
		NetworkConfig:            redactedNetworkConfig,
		ConcurrencyAndBufferSize: p.ConcurrencyAndBufferSize,
		SendBackRawRequest:       p.SendBackRawRequest,
		SendBackRawResponse:      p.SendBackRawResponse,
		StoreRawRequestResponse:  p.StoreRawRequestResponse,
		IgnoreProviderCost:       p.IgnoreProviderCost,
		CustomProviderConfig:     p.CustomProviderConfig,
		OpenAIConfig:             p.OpenAIConfig,
		PromptCache:              p.PromptCache,
		Passthrough:              p.Passthrough,
		ConfigHash:               p.ConfigHash,
		Status:                   p.Status,
		Description:              p.Description,
	}

	if p.ProxyConfig != nil {
		redactedConfig.ProxyConfig = p.ProxyConfig.Redacted()
	}

	// Create redacted keys
	redactedConfig.Keys = make([]schemas.Key, len(p.Keys))
	for i, key := range p.Keys {
		models := key.Models
		if models == nil {
			models = []string{} // Ensure models is never nil in JSON response
		}
		blacklistedModels := key.BlacklistedModels
		if blacklistedModels == nil {
			blacklistedModels = []string{} // Match models: empty JSON array, not null
		}
		redactedConfig.Keys[i] = schemas.Key{
			ID:                key.ID,
			Name:              key.Name,
			Models:            models,
			BlacklistedModels: blacklistedModels,
			Weight:            key.Weight,
			ConfigHash:        key.ConfigHash,
		}
		if key.Enabled != nil {
			enabled := *key.Enabled
			redactedConfig.Keys[i].Enabled = &enabled
		}
		if key.Aliases != nil {
			redactedConfig.Keys[i].Aliases = make(schemas.KeyAliases, len(key.Aliases))
			for name, alias := range key.Aliases {
				redactedConfig.Keys[i].Aliases[name] = alias.Redacted()
			}
		}
		redactedConfig.Keys[i].Value = *key.Value.Redacted()
		// Add back use for batch api
		if key.UseForBatchAPI != nil {
			redactedConfig.Keys[i].UseForBatchAPI = key.UseForBatchAPI
		} else {
			redactedConfig.Keys[i].UseForBatchAPI = new(false)
		}
		// Add back use anthropic endpoints
		if key.UseAnthropicEndpoints != nil {
			redactedConfig.Keys[i].UseAnthropicEndpoints = key.UseAnthropicEndpoints
		} else {
			redactedConfig.Keys[i].UseAnthropicEndpoints = new(false)
		}
		// Add back use openai endpoints
		if key.UseOpenAIEndpoints != nil {
			redactedConfig.Keys[i].UseOpenAIEndpoints = key.UseOpenAIEndpoints
		} else {
			redactedConfig.Keys[i].UseOpenAIEndpoints = new(false)
		}

		// Add model discovery status and error
		redactedConfig.Keys[i].Status = key.Status
		redactedConfig.Keys[i].Description = key.Description

		// Redact Azure key config if present
		if key.AzureKeyConfig != nil {
			azureConfig := &schemas.AzureKeyConfig{}
			// The endpoint is a hostname, not a credential — surface it in plaintext.
			azureConfig.Endpoint = *key.AzureKeyConfig.Endpoint.RedactedIfSecret()
			if key.AzureKeyConfig.ClientID != nil {
				azureConfig.ClientID = key.AzureKeyConfig.ClientID.Redacted()
			}
			if key.AzureKeyConfig.ClientSecret != nil {
				azureConfig.ClientSecret = key.AzureKeyConfig.ClientSecret.Redacted()
			}
			if key.AzureKeyConfig.TenantID != nil {
				azureConfig.TenantID = key.AzureKeyConfig.TenantID.Redacted()
			}
			if len(key.AzureKeyConfig.Scopes) > 0 {
				azureConfig.Scopes = key.AzureKeyConfig.Scopes
			}
			azureConfig.UseSpeechEndpoints = key.AzureKeyConfig.UseSpeechEndpoints
			azureConfig.UseVoiceLiveEndpoints = key.AzureKeyConfig.UseVoiceLiveEndpoints
			redactedConfig.Keys[i].AzureKeyConfig = azureConfig
		}

		// Redact Vertex key config if present
		if key.VertexKeyConfig != nil {
			vertexConfig := &schemas.VertexKeyConfig{}
			vertexConfig.ProjectID = *key.VertexKeyConfig.ProjectID.Redacted()
			vertexConfig.ProjectNumber = *key.VertexKeyConfig.ProjectNumber.Redacted()
			// The region is a public identifier, not a credential — surface it in plaintext.
			vertexConfig.Region = *key.VertexKeyConfig.Region.RedactedIfSecret()
			vertexConfig.AuthCredentials = *key.VertexKeyConfig.AuthCredentials.Redacted()
			vertexConfig.ForceSingleRegion = key.VertexKeyConfig.ForceSingleRegion
			vertexConfig.UseSessionRouting = key.VertexKeyConfig.UseSessionRouting
			vertexConfig.AWSWorkloadIdentity = key.VertexKeyConfig.AWSWorkloadIdentity.Redacted()
			redactedConfig.Keys[i].VertexKeyConfig = vertexConfig
		}

		// Redact Bedrock key config if present
		if key.BedrockKeyConfig != nil {
			bedrockConfig := &schemas.BedrockKeyConfig{}
			bedrockConfig.AccessKey = *key.BedrockKeyConfig.AccessKey.Redacted()
			bedrockConfig.SecretKey = *key.BedrockKeyConfig.SecretKey.Redacted()
			if key.BedrockKeyConfig.SessionToken != nil {
				bedrockConfig.SessionToken = key.BedrockKeyConfig.SessionToken.Redacted()
			}
			// The region is a public identifier, not a credential — surface it in plaintext.
			if key.BedrockKeyConfig.Region != nil {
				bedrockConfig.Region = key.BedrockKeyConfig.Region.RedactedIfSecret()
			}
			if key.BedrockKeyConfig.ARN != nil {
				bedrockConfig.ARN = key.BedrockKeyConfig.ARN.Redacted()
			}
			if key.BedrockKeyConfig.RoleARN != nil {
				bedrockConfig.RoleARN = key.BedrockKeyConfig.RoleARN.Redacted()
			}
			if key.BedrockKeyConfig.ExternalID != nil {
				bedrockConfig.ExternalID = key.BedrockKeyConfig.ExternalID.Redacted()
			}
			if key.BedrockKeyConfig.RoleSessionName != nil {
				bedrockConfig.RoleSessionName = key.BedrockKeyConfig.RoleSessionName.Redacted()
			}
			if key.BedrockKeyConfig.BatchRoleARN != nil {
				bedrockConfig.BatchRoleARN = key.BedrockKeyConfig.BatchRoleARN.Redacted()
			}
			// Preserve literal identifiers, but mask resolved secret references.
			if key.BedrockKeyConfig.ProjectID != nil {
				bedrockConfig.ProjectID = key.BedrockKeyConfig.ProjectID.RedactedIfSecret()
			}
			// Add back s3 config
			if key.BedrockKeyConfig.BatchS3Config != nil {
				bedrockConfig.BatchS3Config = key.BedrockKeyConfig.BatchS3Config
			}
			bedrockConfig.Endpoints = key.BedrockKeyConfig.Endpoints.Redacted()
			redactedConfig.Keys[i].BedrockKeyConfig = bedrockConfig
		}

		// Redact Bedrock Mantle key config if present
		if key.BedrockMantleKeyConfig != nil {
			mantleConfig := &schemas.BedrockMantleKeyConfig{}
			mantleConfig.AccessKey = *key.BedrockMantleKeyConfig.AccessKey.Redacted()
			mantleConfig.SecretKey = *key.BedrockMantleKeyConfig.SecretKey.Redacted()
			if key.BedrockMantleKeyConfig.SessionToken != nil {
				mantleConfig.SessionToken = key.BedrockMantleKeyConfig.SessionToken.Redacted()
			}
			// The region is a public identifier, not a credential — surface it in plaintext.
			if key.BedrockMantleKeyConfig.Region != nil {
				mantleConfig.Region = key.BedrockMantleKeyConfig.Region.RedactedIfSecret()
			}
			if key.BedrockMantleKeyConfig.RoleARN != nil {
				mantleConfig.RoleARN = key.BedrockMantleKeyConfig.RoleARN.Redacted()
			}
			if key.BedrockMantleKeyConfig.ExternalID != nil {
				mantleConfig.ExternalID = key.BedrockMantleKeyConfig.ExternalID.Redacted()
			}
			if key.BedrockMantleKeyConfig.RoleSessionName != nil {
				mantleConfig.RoleSessionName = key.BedrockMantleKeyConfig.RoleSessionName.Redacted()
			}
			// Preserve literal identifiers, but mask resolved secret references.
			if key.BedrockMantleKeyConfig.ProjectID != nil {
				mantleConfig.ProjectID = key.BedrockMantleKeyConfig.ProjectID.RedactedIfSecret()
			}
			mantleConfig.Endpoints = key.BedrockMantleKeyConfig.Endpoints.Redacted()
			redactedConfig.Keys[i].BedrockMantleKeyConfig = mantleConfig
		}

		// Redact SageMaker key config if present
		if key.SageMakerKeyConfig != nil {
			smConfig := &schemas.SageMakerKeyConfig{}
			smConfig.AccessKey = *key.SageMakerKeyConfig.AccessKey.Redacted()
			smConfig.SecretKey = *key.SageMakerKeyConfig.SecretKey.Redacted()
			if key.SageMakerKeyConfig.SessionToken != nil {
				smConfig.SessionToken = key.SageMakerKeyConfig.SessionToken.Redacted()
			}
			if key.SageMakerKeyConfig.Region != nil {
				smConfig.Region = key.SageMakerKeyConfig.Region.RedactedIfSecret()
			}
			if key.SageMakerKeyConfig.RoleARN != nil {
				smConfig.RoleARN = key.SageMakerKeyConfig.RoleARN.Redacted()
			}
			if key.SageMakerKeyConfig.ExternalID != nil {
				smConfig.ExternalID = key.SageMakerKeyConfig.ExternalID.Redacted()
			}
			if key.SageMakerKeyConfig.RoleSessionName != nil {
				smConfig.RoleSessionName = key.SageMakerKeyConfig.RoleSessionName.Redacted()
			}
			smConfig.Endpoints = key.SageMakerKeyConfig.Endpoints.Redacted()
			redactedConfig.Keys[i].SageMakerKeyConfig = smConfig
		}

		if key.VLLMKeyConfig != nil {
			vllmConfig := &schemas.VLLMKeyConfig{
				ModelName: key.VLLMKeyConfig.ModelName,
			}
			// The URL is a service address, not a credential — surface it in plaintext.
			vllmConfig.URL = *key.VLLMKeyConfig.URL.RedactedIfSecret()
			redactedConfig.Keys[i].VLLMKeyConfig = vllmConfig
		}

		if key.ReplicateKeyConfig != nil {
			replicateConfig := &schemas.ReplicateKeyConfig{
				UseDeploymentsEndpoint: key.ReplicateKeyConfig.UseDeploymentsEndpoint,
			}
			redactedConfig.Keys[i].ReplicateKeyConfig = replicateConfig
		}

		if key.OllamaKeyConfig != nil {
			ollamaConfig := &schemas.OllamaKeyConfig{}
			// The URL is a service address, not a credential — surface it in plaintext.
			ollamaConfig.URL = *key.OllamaKeyConfig.URL.RedactedIfSecret()
			redactedConfig.Keys[i].OllamaKeyConfig = ollamaConfig
		}

		if key.SGLKeyConfig != nil {
			sglConfig := &schemas.SGLKeyConfig{}
			// The URL is a service address, not a credential — surface it in plaintext.
			sglConfig.URL = *key.SGLKeyConfig.URL.RedactedIfSecret()
			redactedConfig.Keys[i].SGLKeyConfig = sglConfig
		}

		if key.CloudflareKeyConfig != nil {
			cloudflareConfig := &schemas.CloudflareKeyConfig{}
			// The account ID is an identifier, not a credential — surface it in plaintext.
			cloudflareConfig.AccountID = *key.CloudflareKeyConfig.AccountID.RedactedIfSecret()
			redactedConfig.Keys[i].CloudflareKeyConfig = cloudflareConfig
		}

		if key.AnthropicKeyConfig != nil {
			// Identifiers readable unless secret-backed; the identity token always masked.
			redactedConfig.Keys[i].AnthropicKeyConfig = key.AnthropicKeyConfig.Redacted()
		}

		// Redact Databricks key config if present
		if key.DatabricksKeyConfig != nil {
			databricksConfig := &schemas.DatabricksKeyConfig{
				APIFormat:          key.DatabricksKeyConfig.APIFormat,
				ForwardGatewayTags: key.DatabricksKeyConfig.ForwardGatewayTags,
				Scopes:             slices.Clone(key.DatabricksKeyConfig.Scopes),
			}
			// The workspace URL is a hostname, not a credential — surface it in
			// plaintext so the UI can round-trip it, mirroring the Azure endpoint.
			databricksConfig.WorkspaceURL = *key.DatabricksKeyConfig.WorkspaceURL.RedactedIfSecret()
			if key.DatabricksKeyConfig.ClientID != nil {
				databricksConfig.ClientID = key.DatabricksKeyConfig.ClientID.Redacted()
			}
			if key.DatabricksKeyConfig.ClientSecret != nil {
				databricksConfig.ClientSecret = key.DatabricksKeyConfig.ClientSecret.Redacted()
			}
			redactedConfig.Keys[i].DatabricksKeyConfig = databricksConfig
		}

		if key.OAuthKeyConfig != nil {
			redactedConfig.Keys[i].OAuthKeyConfig = key.OAuthKeyConfig.Redacted()
		}
	}
	return &redactedConfig
}

// GenerateConfigHash generates a SHA256 hash of the provider configuration.
// This is used to detect changes between config.json and database config.
// Keys are excluded as they are hashed separately.
func (p *ProviderConfig) GenerateConfigHash(providerName string) (string, error) {
	var customProviderConfig []byte
	if p.CustomProviderConfig != nil {
		// RequestPathOverrides is a map, so the encoding must sort keys or the same provider
		// hashes differently between restarts and is reconciled as edited.
		data, err := providerUtils.MarshalSorted(p.CustomProviderConfig)
		if err != nil {
			return "", err
		}
		customProviderConfig = data
	}
	hash, err := p.generateConfigHash(providerName, customProviderConfig)
	if err != nil {
		return "", err
	}
	if p.CustomProviderConfig != nil && len(p.CustomProviderConfig.RequestPathOverrides) >= 2 {
		// Only multi-override encodings changed when overrides were sorted; the marker lets
		// reconciliation tell a sorted-era hash from one written by an older build.
		hash += SortedOverridesConfigHashMarker
	}
	return hash, nil
}

// SortedOverridesConfigHashMarker suffixes the config_hash of a custom provider with two or
// more request_path_overrides once they are hashed in sorted order. A stored hash without it
// on such a provider was written by an older build, whose override order is unknown.
const SortedOverridesConfigHashMarker = ":sorted-overrides"

// LegacyOverrideOrderConfigHash returns the config_hash a provider had on builds that hashed
// request_path_overrides with the unsorted encoder, with its overrides encoded in the given
// order. Reconciliation uses it to recognise a hash stored by such a build; tests use it to
// craft one.
func (p *ProviderConfig) LegacyOverrideOrderConfigHash(providerName string, order []schemas.RequestType) (string, error) {
	var customProviderConfig []byte
	if p.CustomProviderConfig != nil {
		data, err := legacyCustomProviderConfigBytes(p.CustomProviderConfig, order)
		if err != nil {
			return "", err
		}
		customProviderConfig = data
	}
	return p.generateConfigHash(providerName, customProviderConfig)
}

// ConfigHashMatchesLegacyOverrideOrder reports whether storedHash is this provider's hash
// under the unsorted request_path_overrides encoding of older builds, in any override order.
// GenerateConfigHash sorts them, so the first restart on a sorted build sees a mismatch for
// every custom provider with two or more overrides even when config.json is untouched;
// reconciliation calls this before treating that mismatch as a file edit, so keys added
// through the API or dashboard survive the upgrade. It runs only after a direct compare
// failed, never on the request path.
func (p *ProviderConfig) ConfigHashMatchesLegacyOverrideOrder(providerName, storedHash string) (bool, error) {
	if p.CustomProviderConfig == nil || strings.HasSuffix(storedHash, SortedOverridesConfigHashMarker) {
		return false, nil
	}
	return hashMatchesAnyLegacyOrder(p.CustomProviderConfig.RequestPathOverrides, storedHash, func(order []schemas.RequestType) (string, error) {
		return p.LegacyOverrideOrderConfigHash(providerName, order)
	})
}

// ConfigHashLegacyOverrideOrderUnverifiable reports whether storedHash was written by an older
// build with unsorted request_path_overrides (it lacks SortedOverridesConfigHashMarker), yet
// ConfigHashMatchesLegacyOverrideOrder cannot say whether config.json changed because the
// provider has more overrides than maxLegacyMapPermutations. For such a provider a hash
// mismatch does not prove that config.json was edited, so reconciliation must not treat it as
// proof either. Once the sorted hash is stored it carries the marker, so this holds on the
// upgrade boot only and later edits are detected normally.
func (p *ProviderConfig) ConfigHashLegacyOverrideOrderUnverifiable(storedHash string) bool {
	return storedHash != "" && !strings.HasSuffix(storedHash, SortedOverridesConfigHashMarker) &&
		p.CustomProviderConfig != nil &&
		len(p.CustomProviderConfig.RequestPathOverrides) > maxLegacyMapPermutations
}

// legacyCustomProviderConfigBytes rebuilds the bytes the unsorted encoder fed the hasher for
// cfg with its request_path_overrides in the given order. The struct's other fields hold no
// map, so its sorted encoding differs from the legacy one only inside that field.
func legacyCustomProviderConfigBytes(cfg *schemas.CustomProviderConfig, order []schemas.RequestType) ([]byte, error) {
	data, err := providerUtils.MarshalSorted(cfg)
	if err != nil || len(cfg.RequestPathOverrides) == 0 {
		return data, err
	}
	sorted, err := providerUtils.MarshalSorted(cfg.RequestPathOverrides)
	if err != nil {
		return nil, err
	}
	legacy, err := legacyMapBytes(cfg.RequestPathOverrides, order)
	if err != nil {
		return nil, err
	}
	const field = `"request_path_overrides":`
	sortedField := append([]byte(field), sorted...)
	if !bytes.Contains(data, sortedField) {
		return nil, fmt.Errorf("request_path_overrides not found in the encoded custom provider config")
	}
	return bytes.Replace(data, sortedField, append([]byte(field), legacy...), 1), nil
}

// generateConfigHash composes the provider hash; customProviderConfig is the already-encoded
// custom provider config (sorted for current hashes, in a given override order for legacy
// ones) or nil when the provider has none.
func (p *ProviderConfig) generateConfigHash(providerName string, customProviderConfig []byte) (string, error) {
	hash := sha256.New()

	// Hash provider name
	hash.Write([]byte(providerName))

	// Hash NetworkConfig
	if p.NetworkConfig != nil {
		data, err := sonic.Marshal(p.NetworkConfig)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}

	// Hash ConcurrencyAndBufferSize
	if p.ConcurrencyAndBufferSize != nil {
		data, err := sonic.Marshal(p.ConcurrencyAndBufferSize)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}

	// Hash ProxyConfig
	if p.ProxyConfig != nil {
		data, err := sonic.Marshal(p.ProxyConfig)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}

	// Hash CustomProviderConfig (already encoded by the caller)
	if customProviderConfig != nil {
		hash.Write(customProviderConfig)
	}

	// Hash OpenAIConfig
	if p.OpenAIConfig != nil {
		data, err := sonic.Marshal(p.OpenAIConfig)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}

	// Hash PromptCache
	if p.PromptCache != nil {
		data, err := sonic.Marshal(p.PromptCache)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}

	// Hash Passthrough
	if p.Passthrough != nil {
		data, err := sonic.Marshal(p.Passthrough)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}

	// Hash SendBackRawRequest
	if p.SendBackRawRequest {
		hash.Write([]byte("sendBackRawRequest"))
	}

	// Hash SendBackRawResponse
	if p.SendBackRawResponse {
		hash.Write([]byte("sendBackRawResponse"))
	}

	// Hash StoreRawRequestResponse
	if p.StoreRawRequestResponse {
		hash.Write([]byte("storeRawRequestResponse"))
	}

	// Hash IgnoreProviderCost
	if p.IgnoreProviderCost {
		hash.Write([]byte("ignoreProviderCost"))
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// GenerateKeyHash generates a SHA256 hash for an individual key.
// This is used to detect changes to keys between config.json and database.
// Skips: ID (dynamic UUID), timestamps
func GenerateKeyHash(key schemas.Key) (string, error) {
	var aliases []byte
	if key.Aliases != nil {
		// A map, so the encoding must sort keys or the same key hashes differently between
		// restarts and is reconciled as edited.
		data, err := providerUtils.MarshalSorted(key.Aliases)
		if err != nil {
			return "", err
		}
		aliases = data
	}
	return generateKeyHash(key, aliases)
}

// LegacyAliasOrderKeyHash returns the config_hash a key had on builds that hashed aliases
// with the unsorted encoder, with its aliases encoded in the given order. Reconciliation uses
// it to recognise a hash stored by such a build; tests use it to craft one.
func LegacyAliasOrderKeyHash(key schemas.Key, order []string) (string, error) {
	var aliases []byte
	if key.Aliases != nil {
		data, err := legacyAliasBytes(key.Aliases, order)
		if err != nil {
			return "", err
		}
		aliases = data
	}
	return generateKeyHash(key, aliases)
}

// maxLegacyMapPermutations bounds the upgrade searches in KeyHashMatchesLegacyAliasOrder and
// ProviderConfig.ConfigHashMatchesLegacyOverrideOrder: seven entries is 5040 hashes, a few
// milliseconds once per mismatching row at startup. A row with more entries falls back to
// treating the mismatch as a file edit, which is what every build before the sorted hash did
// at random.
const maxLegacyMapPermutations = 7

// KeyHashMatchesLegacyAliasOrder reports whether storedHash is this key's hash under the
// unsorted alias encoding of older builds, in any alias order. GenerateKeyHash sorts alias
// names, so the first restart on a sorted build sees a mismatch for every key with two or
// more aliases even when config.json is untouched; reconciliation calls this before treating
// that mismatch as a file edit, so API and dashboard edits survive the upgrade. It runs only
// after a direct compare failed, never on the request path.
func KeyHashMatchesLegacyAliasOrder(key schemas.Key, storedHash string) (bool, error) {
	return hashMatchesAnyLegacyOrder(map[string]schemas.AliasConfig(key.Aliases), storedHash, func(order []string) (string, error) {
		return LegacyAliasOrderKeyHash(key, order)
	})
}

// hashMatchesAnyLegacyOrder reports whether legacyHash, called with every ordering of m's
// keys, produces storedHash. It is the search behind the upgrade paths from unsorted map
// encodings; a map with fewer than two entries has a single encoding, so a mismatch there is
// a real change.
func hashMatchesAnyLegacyOrder[K ~string, V any](m map[K]V, storedHash string, legacyHash func(order []K) (string, error)) (bool, error) {
	n := len(m)
	if storedHash == "" || n < 2 || n > maxLegacyMapPermutations {
		return false, nil
	}
	names := make([]K, 0, n)
	for name := range m {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	// Heap's algorithm visits every ordering once, swapping one pair per step.
	counters := make([]int, n)
	for i := 0; ; {
		candidate, err := legacyHash(names)
		if err != nil {
			return false, err
		}
		if candidate == storedHash {
			return true, nil
		}
		for i = 0; i < n && counters[i] >= i; i++ {
			counters[i] = 0
		}
		if i >= n {
			return false, nil
		}
		if i%2 == 0 {
			names[0], names[i] = names[i], names[0]
		} else {
			names[counters[i]], names[i] = names[i], names[counters[i]]
		}
		counters[i]++
	}
}

// legacyAliasBytes rebuilds the bytes the unsorted encoder fed the hasher for aliases in the
// given order.
func legacyAliasBytes(aliases schemas.KeyAliases, order []string) ([]byte, error) {
	return legacyMapBytes(map[string]schemas.AliasConfig(aliases), order)
}

// legacyMapBytes rebuilds sonic's default (unsorted) encoding of m with its entries in the
// given order: each quoted key, a colon and the value's own encoding, comma-joined inside
// braces.
func legacyMapBytes[K ~string, V any](m map[K]V, order []K) ([]byte, error) {
	if len(order) != len(m) {
		return nil, fmt.Errorf("order names %d entries, map has %d", len(order), len(m))
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, name := range order {
		value, ok := m[name]
		if !ok {
			return nil, fmt.Errorf("%q is not in the map", name)
		}
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := sonic.Marshal(name)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		v, err := sonic.Marshal(value)
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// generateKeyHash composes the key hash; aliases is the already-encoded alias map (sorted for
// current hashes, in a given order for legacy ones) or nil when the key has none.
// oauthKeyConfigHashView returns the copy of an OAuth block that the key hash is computed
// over. Its secret-bearing fields carry their source, as the top-level Value does: a reference
// as "ref:<reference>" and a literal as "val:<value>". Hashing the resolved value instead would
// make an unchanged config.json look edited whenever the environment or vault rotates the
// secret behind it, and reconciliation would then replace the stored key and its edits.
func oauthKeyConfigHashView(cfg *schemas.OAuthKeyConfig) *schemas.OAuthKeyConfig {
	view := *cfg
	source := func(v *schemas.SecretVar) *schemas.SecretVar {
		if v == nil {
			return nil
		}
		if v.IsFromSecret() {
			return &schemas.SecretVar{Val: "ref:" + v.GetRawRef()}
		}
		return &schemas.SecretVar{Val: "val:" + v.Val}
	}
	view.TokenURL = *source(&cfg.TokenURL)
	view.ClientID = source(cfg.ClientID)
	view.ClientSecret = source(cfg.ClientSecret)
	view.PrivateKey = source(cfg.PrivateKey)
	return &view
}

// anthropicKeyConfigHashView is oauthKeyConfigHashView for anthropic_key_config: every field
// hashes by its source, so a rotated env identity token does not read as a config.json edit.
func anthropicKeyConfigHashView(cfg *schemas.AnthropicKeyConfig) *schemas.AnthropicKeyConfig {
	view := &schemas.AnthropicKeyConfig{}
	wif := cfg.WorkloadIdentity
	if wif == nil {
		return view
	}
	source := func(v *schemas.SecretVar) *schemas.SecretVar {
		if v == nil {
			return nil
		}
		if v.IsFromSecret() {
			return &schemas.SecretVar{Val: "ref:" + v.GetRawRef()}
		}
		return &schemas.SecretVar{Val: "val:" + v.Val}
	}
	view.WorkloadIdentity = &schemas.AnthropicWorkloadIdentityConfig{
		FederationRuleID: *source(&wif.FederationRuleID),
		OrganizationID:   *source(&wif.OrganizationID),
		ServiceAccountID: *source(&wif.ServiceAccountID),
		WorkspaceID:      source(wif.WorkspaceID),
	}
	if src := wif.IdentitySource; src != nil {
		view.WorkloadIdentity.IdentitySource = &schemas.IdentitySource{Type: src.Type, Path: source(src.Path), Token: source(src.Token)}
	}
	return view
}

func generateKeyHash(key schemas.Key, aliases []byte) (string, error) {
	hash := sha256.New()
	// Hash Name
	hash.Write([]byte(key.Name))
	// Hash Value (prefix with source type to prevent collisions between ref and literal)
	if key.Value.IsFromSecret() {
		hash.Write([]byte("ref:" + key.Value.GetRawRef()))
	} else {
		hash.Write([]byte("val:" + key.Value.Val))
	}
	// Hash Models (key-level model restrictions)
	if len(key.Models) > 0 {
		sortedModels := make([]string, len(key.Models))
		copy(sortedModels, key.Models)
		sort.Strings(sortedModels)
		data, err := sonic.Marshal(sortedModels)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}
	// Hash BlacklistedModels (key-level deny list)
	if len(key.BlacklistedModels) > 0 {
		sortedBlacklistedModels := make([]string, len(key.BlacklistedModels))
		copy(sortedBlacklistedModels, key.BlacklistedModels)
		sort.Strings(sortedBlacklistedModels)
		data, err := sonic.Marshal(sortedBlacklistedModels)
		if err != nil {
			return "", err
		}
		hash.Write([]byte("blacklistedModels:"))
		hash.Write(data)
	}
	// Hash Weight
	data, err := sonic.Marshal(key.Weight)
	if err != nil {
		return "", err
	}
	hash.Write(data)
	// Hash AzureKeyConfig
	if key.AzureKeyConfig != nil {
		data, err := sonic.Marshal(key.AzureKeyConfig)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}
	// Hash VertexKeyConfig
	if key.VertexKeyConfig != nil {
		data, err := sonic.Marshal(key.VertexKeyConfig)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}
	// Hash BedrockKeyConfig
	if key.BedrockKeyConfig != nil {
		data, err := sonic.Marshal(key.BedrockKeyConfig)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}
	// Hash BedrockMantleKeyConfig
	if key.BedrockMantleKeyConfig != nil {
		data, err := sonic.Marshal(key.BedrockMantleKeyConfig)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}
	// Hash SageMakerKeyConfig
	if key.SageMakerKeyConfig != nil {
		data, err := sonic.Marshal(key.SageMakerKeyConfig)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}
	// Hash Aliases (encoded by the caller, see GenerateKeyHash).
	if aliases != nil {
		hash.Write(aliases)
	}
	// Hash VLLMKeyConfig
	if key.VLLMKeyConfig != nil {
		data, err := sonic.Marshal(key.VLLMKeyConfig)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}
	// Hash ReplicateKeyConfig
	if key.ReplicateKeyConfig != nil {
		data, err := sonic.Marshal(key.ReplicateKeyConfig)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}
	// Hash OllamaKeyConfig
	if key.OllamaKeyConfig != nil {
		data, err := sonic.Marshal(key.OllamaKeyConfig)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}
	// Hash SGLKeyConfig
	if key.SGLKeyConfig != nil {
		data, err := sonic.Marshal(key.SGLKeyConfig)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}
	// Hash CloudflareKeyConfig
	if key.CloudflareKeyConfig != nil {
		data, err := sonic.Marshal(key.CloudflareKeyConfig)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}
	// Hash AnthropicKeyConfig by source, as OAuthKeyConfig below.
	if key.AnthropicKeyConfig != nil {
		data, err := providerUtils.MarshalSorted(anthropicKeyConfigHashView(key.AnthropicKeyConfig))
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}
	// Hash DatabricksKeyConfig
	if key.DatabricksKeyConfig != nil {
		data, err := sonic.Marshal(key.DatabricksKeyConfig)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}
	// Hash OAuthKeyConfig. extra_params is a map, so the encoding must sort keys or the same
	// key hashes differently between restarts and reconciliation replaces dashboard edits.
	if key.OAuthKeyConfig != nil {
		data, err := providerUtils.MarshalSorted(oauthKeyConfigHashView(key.OAuthKeyConfig))
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}
	// Hash Enabled (nil = false, only true produces different hash)
	if key.Enabled != nil && *key.Enabled {
		hash.Write([]byte("enabled:true"))
	}
	// Hash UseForBatchAPI (nil = default false for new keys)
	useForBatchAPI := false
	if key.UseForBatchAPI != nil {
		useForBatchAPI = *key.UseForBatchAPI
	}
	if useForBatchAPI {
		hash.Write([]byte("useForBatchAPI:true"))
	}
	// Hash UseAnthropicEndpoints (nil = default false for new keys)
	useAnthropicEndpoints := false
	if key.UseAnthropicEndpoints != nil {
		useAnthropicEndpoints = *key.UseAnthropicEndpoints
	}
	if useAnthropicEndpoints {
		hash.Write([]byte("useAnthropicEndpoints:true"))
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// VirtualKeyHashInput represents the fields used for virtual key hash generation.
// This struct is used to create a consistent hash from TableVirtualKey,
// excluding dynamic fields like ID, timestamps, and relationship objects.
type VirtualKeyHashInput struct {
	Name        string
	Description string
	IsActive    bool
	TeamID      *string
	CustomerID  *string
	RateLimitID *string
	// ProviderConfigs are hashed separately as they contain nested data
	ProviderConfigs []VirtualKeyProviderConfigHashInput
}

// VirtualKeyProviderConfigHashInput represents provider config fields for hashing
type VirtualKeyProviderConfigHashInput struct {
	Provider          string
	Weight            *float64
	AllowedModels     []string
	BlacklistedModels []string
	AllowAllKeys      bool
	RateLimitID       *string
	KeyIDs            []string // Only key IDs, not full key objects
}

// GenerateVirtualKeyHash generates a SHA256 hash for a virtual key.
// This is used to detect changes to virtual keys between config.json and database.
// Skips: ID (primary key), CreatedAt, UpdatedAt, and relationship objects (Team, Customer, Budget, RateLimit)
func GenerateVirtualKeyHash(vk tables.TableVirtualKey) (string, error) {
	hash := sha256.New()
	// Hash Name
	hash.Write([]byte(vk.Name))
	// Hash Description
	hash.Write([]byte(vk.Description))
	// Hash the resolved value so that secret rotation (vault/env change) is
	// detected as a config change and triggers a re-sync.
	hash.Write([]byte(vk.Value.GetValue()))
	// Hash IsActive (nil treated as DB default true)
	if vk.IsActiveValue() {
		hash.Write([]byte("isActive:true"))
	} else {
		hash.Write([]byte("isActive:false"))
	}
	// Hash AllowAllProviders so a config.json flip triggers re-sync
	if vk.AllowAllProviders {
		hash.Write([]byte("allowAllProviders:true"))
	} else {
		hash.Write([]byte("allowAllProviders:false"))
	}
	// Hash DisableContentLogging only when the key says something. Nil is inherit, and writing
	// nothing for it keeps every key that predates the column on the hash it already has, so
	// config sync sees no drift on upgrade (the unconditional AllowAllProviders write above is
	// what forced a backfill migration).
	if vk.DisableContentLogging != nil {
		if *vk.DisableContentLogging {
			hash.Write([]byte("disableContentLogging:true"))
		} else {
			hash.Write([]byte("disableContentLogging:false"))
		}
	}
	// Hash Metadata only when the key has some, so every key that predates the column keeps its
	// hash. Written as JSON-encoded [key, value] pairs in key order, so map iteration order cannot
	// move the hash and no key/value boundary is ambiguous.
	if len(vk.Metadata) > 0 {
		pairs := make([][2]string, 0, len(vk.Metadata))
		for k, v := range vk.Metadata {
			pairs = append(pairs, [2]string{k, v})
		}
		sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
		data, err := sonic.Marshal(pairs)
		if err != nil {
			return "", err
		}
		hash.Write([]byte("metadata:"))
		hash.Write(data)
	}
	// Hash ExpiresAt only when set, so rows created before expiry existed keep their hash
	if vk.ExpiresAt != nil {
		hash.Write([]byte("expiresAt:" + vk.ExpiresAt.UTC().Format(time.RFC3339Nano)))
	}
	// Hash DeleteAfterExpire only when set, for the same reason
	if vk.DeleteAfterExpire != nil {
		hash.Write([]byte(fmt.Sprintf("deleteAfterExpire:%t", *vk.DeleteAfterExpire)))
	}
	// Hash TeamID
	if vk.TeamID != nil {
		hash.Write([]byte("teamID:" + *vk.TeamID))
	}
	// Hash CustomerID
	if vk.CustomerID != nil {
		hash.Write([]byte("customerID:" + *vk.CustomerID))
	}
	// Hash RateLimitID
	if vk.RateLimitID != nil {
		hash.Write([]byte("rateLimitID:" + *vk.RateLimitID))
	}
	// Hash ProviderConfigs
	if len(vk.ProviderConfigs) > 0 {
		// Copy and sort provider configs for deterministic hashing
		sortedProviderConfigs := make([]tables.TableVirtualKeyProviderConfig, len(vk.ProviderConfigs))
		copy(sortedProviderConfigs, vk.ProviderConfigs)
		sort.Slice(sortedProviderConfigs, func(i, j int) bool {
			if sortedProviderConfigs[i].Provider != sortedProviderConfigs[j].Provider {
				return sortedProviderConfigs[i].Provider < sortedProviderConfigs[j].Provider
			}
			ri, rj := "", ""
			if sortedProviderConfigs[i].RateLimitID != nil {
				ri = *sortedProviderConfigs[i].RateLimitID
			}
			if sortedProviderConfigs[j].RateLimitID != nil {
				rj = *sortedProviderConfigs[j].RateLimitID
			}
			if ri != rj {
				return ri < rj
			}
			wi, wj := sortedProviderConfigs[i].Weight, sortedProviderConfigs[j].Weight
			if (wi == nil) != (wj == nil) {
				return wi == nil
			}
			if wi != nil && wj != nil && *wi != *wj {
				return *wi < *wj
			}
			return false
		})
		// Filter out provider configs that are not available
		providerConfigsForHash := make([]VirtualKeyProviderConfigHashInput, len(sortedProviderConfigs))
		for i, pc := range sortedProviderConfigs {
			// Sort key IDs for deterministic hashing
			keyIDs := make([]string, len(pc.Keys))
			for j, k := range pc.Keys {
				keyIDs[j] = k.KeyID
			}
			sort.Strings(keyIDs)

			// Sort allowed models for deterministic hashing
			sortedAllowedModels := make([]string, len(pc.AllowedModels))
			copy(sortedAllowedModels, pc.AllowedModels)
			sort.Strings(sortedAllowedModels)

			// Sort blacklisted models for deterministic hashing
			sortedBlacklistedModels := make([]string, len(pc.BlacklistedModels))
			copy(sortedBlacklistedModels, pc.BlacklistedModels)
			sort.Strings(sortedBlacklistedModels)
			providerConfigsForHash[i] = VirtualKeyProviderConfigHashInput{
				Provider:          pc.Provider,
				Weight:            pc.Weight,
				AllowedModels:     sortedAllowedModels,
				BlacklistedModels: sortedBlacklistedModels,
				AllowAllKeys:      pc.AllowAllKeys,
				RateLimitID:       pc.RateLimitID,
				KeyIDs:            keyIDs,
			}
		}
		data, err := sonic.Marshal(providerConfigsForHash)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// GenerateBudgetHash generates a SHA256 hash for a budget.
// This is used to detect changes to budgets between config.json and database.
// Skips: LastReset, CurrentUsage, CreatedAt, UpdatedAt (dynamic fields)
func GenerateBudgetHash(b tables.TableBudget) (string, error) {
	hash := sha256.New()

	// Hash ID
	hash.Write([]byte(b.ID))

	// Hash MaxLimit
	data, err := sonic.Marshal(b.MaxLimit)
	if err != nil {
		return "", err
	}
	hash.Write(data)

	// Hash ResetDuration
	hash.Write([]byte(b.ResetDuration))

	// Hash the quarter definition, without which a quarter-start edit in
	// config.json is invisible to change detection and never reaches the database.
	//
	// Read the effective month rather than ResetConfigJSON: this function compares
	// a budget parsed from config.json against one read from the database, and only
	// the latter has the blob populated, since BeforeSave has not run on the former.
	// Hashing the blob would make the two disagree on every comparison and resync
	// forever. Gating on the duration keeps every non-quarterly budget's digest
	// byte-identical, so upgrading does not look like a config change.
	if tables.IsQuarterlyDuration(b.ResetDuration) {
		hash.Write([]byte(strconv.Itoa(int(b.QuarterStartMonth()))))
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// GenerateRateLimitHash generates a SHA256 hash for a rate limit.
// This is used to detect changes to rate limits between config.json and database.
// Skips: CurrentUsage, LastReset, CreatedAt, UpdatedAt (dynamic fields)
func GenerateRateLimitHash(rl tables.TableRateLimit) (string, error) {
	hash := sha256.New()

	// Hash ID
	hash.Write([]byte(rl.ID))

	// Hash TokenMaxLimit
	if rl.TokenMaxLimit != nil {
		data, err := sonic.Marshal(*rl.TokenMaxLimit)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}

	// Hash TokenResetDuration
	if rl.TokenResetDuration != nil {
		hash.Write([]byte(*rl.TokenResetDuration))
	}

	// Hash RequestMaxLimit
	if rl.RequestMaxLimit != nil {
		data, err := sonic.Marshal(*rl.RequestMaxLimit)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}

	// Hash RequestResetDuration
	if rl.RequestResetDuration != nil {
		hash.Write([]byte(*rl.RequestResetDuration))
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// GenerateCustomerHash generates a SHA256 hash for a customer.
// This is used to detect changes to customers between config.json and database.
// Skips: CreatedAt, UpdatedAt, and relationship objects (dynamic fields)
func GenerateCustomerHash(c tables.TableCustomer) (string, error) {
	hash := sha256.New()

	// Hash ID
	hash.Write([]byte(c.ID))

	// Hash Name
	hash.Write([]byte(c.Name))

	// Collect budget IDs from both sources so config-file context (BudgetID) and
	// DB context (Budgets) produce the same hash for the same logical state.
	seen := make(map[string]bool, len(c.Budgets)+1)
	if c.BudgetID != nil {
		seen[*c.BudgetID] = true
	}
	for _, b := range c.Budgets {
		seen[b.ID] = true
	}
	budgetIDs := make([]string, 0, len(seen))
	for id := range seen {
		budgetIDs = append(budgetIDs, id)
	}
	sort.Strings(budgetIDs)
	for _, id := range budgetIDs {
		hash.Write([]byte("budgetID:" + id))
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// GenerateTeamHash generates a SHA256 hash for a team.
// This is used to detect changes to teams between config.json and database.
// Skips: CreatedAt, UpdatedAt, and relationship objects (dynamic fields)
func GenerateTeamHash(t tables.TableTeam) (string, error) {
	hash := sha256.New()

	// Hash ID
	hash.Write([]byte(t.ID))

	// Hash Name
	hash.Write([]byte(t.Name))

	// Hash CustomerID
	if t.CustomerID != nil {
		hash.Write([]byte("customerID:" + *t.CustomerID))
	}

	// Hash sorted budget IDs — team now owns multiple budgets; slice order must not
	// affect the hash, otherwise config-sync would flip the hash on every reload.
	if len(t.Budgets) > 0 {
		ids := make([]string, len(t.Budgets))
		for i, b := range t.Budgets {
			ids[i] = b.ID
		}
		sort.Strings(ids)
		hash.Write([]byte("budgetIDs:"))
		for i, id := range ids {
			if i > 0 {
				hash.Write([]byte{','})
			}
			hash.Write([]byte(id))
		}
	}

	// Hash Profile - use Profile if set, else marshal ParsedProfile
	// (Profile has json:"-" so when loading from JSON, only ParsedProfile is populated)
	// Use encoding/json for consistency with BeforeSave hook serialization
	if t.Profile != nil {
		hash.Write([]byte("profile:" + *t.Profile))
	} else if t.ParsedProfile != nil {
		data, err := json.Marshal(t.ParsedProfile)
		if err != nil {
			return "", err
		}
		hash.Write([]byte("profile:" + string(data)))
	}

	// Hash Config - use Config if set, else marshal ParsedConfig
	// Use encoding/json for consistency with BeforeSave hook serialization
	if t.Config != nil {
		hash.Write([]byte("config:" + *t.Config))
	} else if t.ParsedConfig != nil {
		data, err := json.Marshal(t.ParsedConfig)
		if err != nil {
			return "", err
		}
		hash.Write([]byte("config:" + string(data)))
	}

	// Hash Claims - use Claims if set, else marshal ParsedClaims
	// Use encoding/json for consistency with BeforeSave hook serialization
	if t.Claims != nil {
		hash.Write([]byte("claims:" + *t.Claims))
	} else if t.ParsedClaims != nil {
		data, err := json.Marshal(t.ParsedClaims)
		if err != nil {
			return "", err
		}
		hash.Write([]byte("claims:" + string(data)))
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// GenerateModelConfigHash generates a SHA256 hash for a model config.
// This is used to detect changes to model configs between config.json and database.
// Skips: CreatedAt, UpdatedAt, and relationship objects (dynamic fields)
func GenerateModelConfigHash(m tables.TableModelConfig) (string, error) {
	// Normalize an empty scope to "global" so a config.json entry that omits scope
	// hashes identically to the defaulted DB row.
	scope := m.Scope
	if scope == "" {
		scope = tables.ModelConfigScopeGlobal
	}
	hash := sha256.New()
	writeHashField(hash, "id", m.ID)
	writeHashField(hash, "model_name", m.ModelName)
	writeHashField(hash, "provider", derefStr(m.Provider))
	writeHashField(hash, "scope", scope)
	writeHashField(hash, "scope_id", derefStr(m.ScopeID))
	writeHashField(hash, "budget_id", derefStr(m.BudgetID))
	writeHashField(hash, "rate_limit_id", derefStr(m.RateLimitID))
	sortedBudgetIDs := append([]string(nil), m.BudgetIDs...)
	sort.Strings(sortedBudgetIDs)
	for _, id := range sortedBudgetIDs {
		writeHashField(hash, "budget_ids", id)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// GenerateProviderGovernanceHash generates a SHA256 hash for provider-governance
// bindings only (provider name + budget/rate-limit references).
// It intentionally excludes provider runtime/config fields and keys.
func GenerateProviderGovernanceHash(p tables.TableProvider) (string, error) {
	hash := sha256.New()
	writeHashField(hash, "name", p.Name)
	writeHashField(hash, "budget_id", derefStr(p.BudgetID))
	writeHashField(hash, "rate_limit_id", derefStr(p.RateLimitID))
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// GenerateComplexityAnalyzerConfigHashes returns stable section hashes for
// config.json-sourced analyzer config.
func GenerateComplexityAnalyzerConfigHashes(config *ComplexityAnalyzerConfig) (ComplexityAnalyzerConfigHashes, error) {
	if config == nil {
		return ComplexityAnalyzerConfigHashes{}, fmt.Errorf("complexity analyzer config is nil")
	}

	normalized := config.Normalized()
	normalized.ConfigHashes = ComplexityAnalyzerConfigHashes{}
	if err := normalized.Validate(); err != nil {
		return ComplexityAnalyzerConfigHashes{}, err
	}

	tierHash, err := hashComplexityValue(normalized.TierBoundaries)
	if err != nil {
		return ComplexityAnalyzerConfigHashes{}, fmt.Errorf("failed to hash tier boundaries: %w", err)
	}
	simpleHash, err := hashComplexityValue(normalized.Keywords.SimpleKeywords)
	if err != nil {
		return ComplexityAnalyzerConfigHashes{}, fmt.Errorf("failed to hash simple keywords: %w", err)
	}
	mediumHash, err := hashComplexityValue(normalized.Keywords.MediumKeywords)
	if err != nil {
		return ComplexityAnalyzerConfigHashes{}, fmt.Errorf("failed to hash medium keywords: %w", err)
	}
	complexHash, err := hashComplexityValue(normalized.Keywords.ComplexKeywords)
	if err != nil {
		return ComplexityAnalyzerConfigHashes{}, fmt.Errorf("failed to hash complex keywords: %w", err)
	}

	hashes := ComplexityAnalyzerConfigHashes{
		TierBoundaries:  tierHash,
		SimpleKeywords:  simpleHash,
		MediumKeywords:  mediumHash,
		ComplexKeywords: complexHash,
	}

	if normalized.Semantic != nil {
		settingsHash, err := hashComplexityValue(normalized.Semantic)
		if err != nil {
			return ComplexityAnalyzerConfigHashes{}, fmt.Errorf("failed to hash semantic settings: %w", err)
		}
		hashes.SemanticSettings = settingsHash
	}

	if config.Classifier != "" {
		settingsHash, err := hashComplexityValue(normalized.Classifier)
		if err != nil {
			return ComplexityAnalyzerConfigHashes{}, fmt.Errorf("failed to hash classifier settings: %w", err)
		}
		hashes.ClassifierSettings = settingsHash
	}

	if normalized.Decision != nil {
		settingsHash, err := hashComplexityValue(normalized.Decision)
		if err != nil {
			return ComplexityAnalyzerConfigHashes{}, fmt.Errorf("failed to hash decision settings: %w", err)
		}
		hashes.DecisionSettings = settingsHash
	}

	if normalized.LLM != nil {
		settingsHash, err := hashComplexityValue(normalized.LLM)
		if err != nil {
			return ComplexityAnalyzerConfigHashes{}, fmt.Errorf("failed to hash llm settings: %w", err)
		}
		hashes.LLMSettings = settingsHash
	}

	if normalized.Session != nil {
		settingsHash, err := hashComplexityValue(normalized.Session)
		if err != nil {
			return ComplexityAnalyzerConfigHashes{}, fmt.Errorf("failed to hash session settings: %w", err)
		}
		hashes.SessionSettings = settingsHash
	}

	if normalized.Suggestions != nil {
		settingsHash, err := hashComplexityValue(normalized.Suggestions)
		if err != nil {
			return ComplexityAnalyzerConfigHashes{}, fmt.Errorf("failed to hash suggestions settings: %w", err)
		}
		hashes.SuggestionsSettings = settingsHash
	}

	return hashes, nil
}

// legacyMediumKeywordsHashFromSectionHashes combines legacy code and technical hashes into the canonical medium hash.
func legacyMediumKeywordsHashFromSectionHashes(codeHash, technicalHash string) (string, error) {
	if codeHash == "" && technicalHash == "" {
		return "", nil
	}
	return hashComplexityValue(struct {
		CodeKeywords      string `json:"code_keywords"`
		TechnicalKeywords string `json:"technical_keywords"`
	}{
		CodeKeywords:      codeHash,
		TechnicalKeywords: technicalHash,
	})
}

// hashComplexityValue returns a stable hash for one complexity configuration section.
func hashComplexityValue(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// writeHashField writes a field identifier and a length-prefixed value so the
// resulting byte stream is unambiguous and cannot collide via concatenation.
func writeHashField(hash hash.Hash, fieldID, value string) {
	hash.Write([]byte(fieldID))
	hash.Write([]byte(":"))
	hash.Write([]byte(strconv.Itoa(len(value))))
	hash.Write([]byte(":"))
	hash.Write([]byte(value))
	hash.Write([]byte(";"))
}

// GenerateRoutingRuleHash generates a SHA256 hash for a routing rule.
// This is used to detect changes to routing rules between config.json and database.
// routingTargetHashPayload is a canonical struct for hashing a routing target.
// Used to ensure deterministic hashes regardless of slice order.
// Fields use plain string (not *string) so nil and "" both marshal to "" and produce the same hash.
type routingTargetHashPayload struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	KeyID    string  `json:"key_id"`
	Weight   float64 `json:"weight"`
	// TTFTTimeoutMs is omitted when unset so targets without it keep their hash.
	TTFTTimeoutMs int `json:"ttft_timeout_ms,omitempty"`
}

// derefStr returns the dereferenced value of s, or "" if s is nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// derefInt returns the dereferenced value of n, or 0 if n is nil.
func derefInt(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}

// Skips: CreatedAt, UpdatedAt (dynamic fields)
func GenerateRoutingRuleHash(r tables.TableRoutingRule) (string, error) {
	hash := sha256.New()

	// Hash ID
	hash.Write([]byte(r.ID))

	// Hash Name
	hash.Write([]byte(r.Name))

	// Hash Description
	hash.Write([]byte(r.Description))

	// Hash Enabled (nil treated as DB default true)
	if r.EnabledValue() {
		hash.Write([]byte("enabled:true"))
	} else {
		hash.Write([]byte("enabled:false"))
	}

	// Hash CelExpression
	hash.Write([]byte(r.CelExpression))

	// Hash Targets: sort by canonical marshaled payload for determinism, then hash each target as a single blob
	targets := make([]tables.TableRoutingTarget, len(r.Targets))
	copy(targets, r.Targets)
	sort.Slice(targets, func(i, j int) bool {
		pi := routingTargetHashPayload{Provider: derefStr(targets[i].Provider), Model: derefStr(targets[i].Model), KeyID: derefStr(targets[i].KeyID), Weight: targets[i].Weight, TTFTTimeoutMs: derefInt(targets[i].TTFTTimeoutMs)}
		pj := routingTargetHashPayload{Provider: derefStr(targets[j].Provider), Model: derefStr(targets[j].Model), KeyID: derefStr(targets[j].KeyID), Weight: targets[j].Weight, TTFTTimeoutMs: derefInt(targets[j].TTFTTimeoutMs)}
		di, err := sonic.Marshal(pi)
		if err != nil {
			return false
		}
		dj, err := sonic.Marshal(pj)
		if err != nil {
			return false
		}
		return string(di) < string(dj)
	})
	for _, t := range targets {
		payload := routingTargetHashPayload{Provider: derefStr(t.Provider), Model: derefStr(t.Model), KeyID: derefStr(t.KeyID), Weight: t.Weight, TTFTTimeoutMs: derefInt(t.TTFTTimeoutMs)}
		data, err := sonic.Marshal(payload)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}

	// Hash Fallbacks: use DB string when set, else marshal ParsedFallbacks (config-origin)
	if r.Fallbacks != nil {
		hash.Write([]byte(*r.Fallbacks))
	} else if len(r.ParsedFallbacks) > 0 {
		data, err := sonic.Marshal(r.ParsedFallbacks)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}

	// Hash Query: use raw string when set, else marshal ParsedQuery (config-origin)
	// Use OrderedMap's deterministic marshalling to ensure consistent hashes across runs
	if r.Query != nil {
		hash.Write([]byte(*r.Query))
	} else if len(r.ParsedQuery) > 0 {
		// Convert map to OrderedMap and use sorted marshalling for deterministic hashes
		orderedMap := schemas.OrderedMapFromMap(r.ParsedQuery)
		data, err := orderedMap.MarshalSorted()
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}

	// Hash ChainRule
	if r.ChainRule {
		hash.Write([]byte("chain_rule:true"))
	} else {
		hash.Write([]byte("chain_rule:false"))
	}

	// Hash Scope
	hash.Write([]byte(r.Scope))

	// Hash ScopeID (nil = global)
	scopeID := ""
	if r.ScopeID != nil {
		scopeID = *r.ScopeID
	}
	hash.Write([]byte(scopeID))

	// Hash Priority
	hash.Write([]byte(strconv.Itoa(r.Priority)))

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// GeneratePricingOverrideHash generates a SHA256 hash for a pricing override.
// Skips: CreatedAt, UpdatedAt, ConfigHash (dynamic/meta fields).
func GeneratePricingOverrideHash(p tables.TablePricingOverride) (string, error) {
	hash := sha256.New()
	hash.Write([]byte(p.ID))
	hash.Write([]byte(p.Name))
	hash.Write([]byte(p.ScopeKind))
	hash.Write([]byte(derefStr(p.UserID)))
	hash.Write([]byte(derefStr(p.VirtualKeyID)))
	hash.Write([]byte(derefStr(p.ProviderID)))
	hash.Write([]byte(derefStr(p.ProviderKeyID)))
	hash.Write([]byte(p.MatchType))
	hash.Write([]byte(p.Pattern))
	hash.Write([]byte(p.RequestTypesJSON))
	hash.Write([]byte(p.PricingPatchJSON))
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// GeneratePluginHash generates a SHA256 hash for a plugin.
// This is used to detect changes to plugins between config.json and database.
// Skips: ID (autoIncrement), CreatedAt, UpdatedAt, IsCustom (dynamic fields)
func GeneratePluginHash(p tables.TablePlugin) (string, error) {
	hash := sha256.New()

	// Hash Name
	hash.Write([]byte(p.Name))

	// Hash Enabled
	if p.Enabled {
		hash.Write([]byte("enabled:true"))
	} else {
		hash.Write([]byte("enabled:false"))
	}

	// Hash Path
	if p.Path != nil {
		hash.Write([]byte("path:" + *p.Path))
	}

	// Hash Config (use ConfigJSON for consistent hashing)
	// Normalize: nil and empty map ({}) are treated as equivalent (no hash contribution)
	if p.ConfigJSON != "" && p.ConfigJSON != "{}" {
		hash.Write([]byte(p.ConfigJSON))
	} else if p.Config != nil {
		// Check if Config is a non-empty map before hashing
		// Use encoding/json for consistency with BeforeSave hook serialization
		data, err := json.Marshal(p.Config)
		if err != nil {
			return "", err
		}
		// Only hash if it's not an empty object
		if string(data) != "{}" && string(data) != "null" {
			hash.Write(data)
		}
	}

	// Hash Version when set, so hashes already persisted by the config_hash migration stay valid.
	if p.Version != 0 {
		data, err := sonic.Marshal(p.Version)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}

	// Hash Placement and Order when set. config.json owns whichever of the two it declares,
	// so changing one has to read as a file edit; declaring neither contributes nothing and
	// leaves the stored ordering untouched.
	if p.Placement != nil {
		hash.Write([]byte("placement:" + string(*p.Placement)))
	}
	if p.Order != nil {
		hash.Write([]byte("order:" + strconv.Itoa(*p.Order)))
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// frameworkConfigHashPayload holds the config.json-sourced fields used for hashing.
//
// LiveModelsSyncInterval carries omitempty deliberately: a nil pointer must marshal to
// exactly the bytes this struct produced before the field existed.
type frameworkConfigHashPayload struct {
	PricingURL             *string `json:"pricing_url"`
	ModelParametersURL     *string `json:"model_parameters_url"`
	PricingSyncInterval    *int64  `json:"pricing_sync_interval"`
	LiveModelsSyncInterval *int64  `json:"live_models_sync_interval,omitempty"`
}

// FrameworkConfigHashOptions adds optional framework config fields to the
// config.json change-detection hash while preserving the pricing-only hash
// when omitted.
type FrameworkConfigHashOptions struct {
	LiveModelsSyncInterval *int64
}

// GenerateFrameworkConfigHash generates a SHA256 hash for a framework config.
// This is used to detect changes to framework config between config.json and database.
func GenerateFrameworkConfigHash(pricingURL *string, modelParametersURL *string, pricingSyncInterval *int64, opts ...FrameworkConfigHashOptions) (string, error) {
	payload := frameworkConfigHashPayload{
		PricingURL:          pricingURL,
		ModelParametersURL:  modelParametersURL,
		PricingSyncInterval: pricingSyncInterval,
	}
	if len(opts) > 0 {
		payload.LiveModelsSyncInterval = opts[0].LiveModelsSyncInterval
	}
	data, err := sonic.Marshal(payload)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), nil
}

// AuthConfig represents configured auth config for Bifrost dashboard
type AuthConfig struct {
	AdminUserName *schemas.SecretVar `json:"admin_username"`
	AdminPassword *schemas.SecretVar `json:"admin_password"`
	IsEnabled     bool               `json:"is_enabled"`
}

// ConfigMap maps provider names to their configurations.
type ConfigMap map[schemas.ModelProvider]ProviderConfig

// GovernanceConfig contains governance entities loaded from the config store or
// reconciled from config.json.
type GovernanceConfig struct {
	VirtualKeys              []tables.TableVirtualKey      `json:"virtual_keys"`
	Teams                    []tables.TableTeam            `json:"teams"`
	Customers                []tables.TableCustomer        `json:"customers"`
	Budgets                  []tables.TableBudget          `json:"budgets"`
	RateLimits               []tables.TableRateLimit       `json:"rate_limits"`
	ModelConfigs             []tables.TableModelConfig     `json:"model_configs"`
	Providers                []tables.TableProvider        `json:"providers"`
	RoutingRules             []tables.TableRoutingRule     `json:"routing_rules"`
	PricingOverrides         []tables.TablePricingOverride `json:"pricing_overrides,omitempty"`
	AuthConfig               *AuthConfig                   `json:"auth_config,omitempty"`
	ComplexityAnalyzerConfig *ComplexityAnalyzerConfig     `json:"complexity_analyzer_config,omitempty"`
}

// MaxPublicURLLength is the longest public URL a setting accepts.
const MaxPublicURLLength = 512

// ValidatePublicBaseURL accepts an empty setting or a public HTTP base URL without private URL components.
// Errors omit the value because it may contain credentials resolved from a secret reference.
func ValidatePublicBaseURL(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > MaxPublicURLLength {
		return fmt.Errorf("must be at most %d characters", MaxPublicURLLength)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("must be an absolute HTTP or HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(value, "#") {
		return fmt.Errorf("must not contain userinfo, query, or fragment components")
	}
	return nil
}

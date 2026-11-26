package tables

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// DualCredentialConflictBehavior controls what happens on inference requests that
// carry both an IDP access token (Authorization: Bearer <jwt>) and a virtual key (x-bf-vk).
type DualCredentialConflictBehavior string

const (
	// DualCredentialConflictBehaviorError rejects the request with 400.
	DualCredentialConflictBehaviorError DualCredentialConflictBehavior = "error"
	// DualCredentialConflictBehaviorPreferVK drops the IDP token and authenticates via the virtual key.
	DualCredentialConflictBehaviorPreferVK DualCredentialConflictBehavior = "prefer_vk"
	// DualCredentialConflictBehaviorPreferIDP uses the IDP token for identity (default when unset).
	DualCredentialConflictBehaviorPreferIDP DualCredentialConflictBehavior = "prefer_idp"
)

// ExtraFieldsPlacement controls where inference responses carry Bifrost's extra_fields metadata.
type ExtraFieldsPlacement string

const (
	// ExtraFieldsPlacementBody keeps extra_fields in the JSON body (default when unset).
	ExtraFieldsPlacementBody ExtraFieldsPlacement = "body"
	// ExtraFieldsPlacementHeaders removes extra_fields from the body and sends its scalar fields as x-bifrost-* headers.
	ExtraFieldsPlacementHeaders ExtraFieldsPlacement = "headers"
)

// EntityAccessProfileMode controls how the access profiles of the entities a caller belongs to
// (teams, business units, customers) combine with the caller's own access. The values match a
// project's access rule, since they are the same choice.
type EntityAccessProfileMode string

const (
	// EntityAccessProfileModeUnion lets those profiles add to what the caller may reach
	// (default when unset). Shown as "Extend".
	EntityAccessProfileModeUnion EntityAccessProfileMode = "union"
	// EntityAccessProfileModeIntersect makes each level holding a profile cap what the caller
	// may reach: the caller keeps only what every such level also allows. Shown as "Restrict".
	EntityAccessProfileModeIntersect EntityAccessProfileMode = "intersect"
)

// TableClientConfig represents global client configuration in the database
type TableClientConfig struct {
	ID                                      uint                           `gorm:"primaryKey;autoIncrement" json:"id"`
	DropExcessRequests                      bool                           `gorm:"default:false" json:"drop_excess_requests"`
	PrometheusLabelsJSON                    string                         `gorm:"type:text" json:"-"` // JSON serialized []string
	AllowedOriginsJSON                      string                         `gorm:"type:text" json:"-"` // JSON serialized []string
	AllowedHeadersJSON                      string                         `gorm:"type:text" json:"-"` // JSON serialized []string
	HeaderFilterConfigJSON                  string                         `gorm:"type:text" json:"-"` // JSON serialized GlobalHeaderFilterConfig
	MetadataJSON                            string                         `gorm:"type:text" json:"-"` // JSON serialized map[string]any for UI/admin preferences (e.g. onboarding_dismissed). Bypasses config.json sync.
	InitialPoolSize                         int                            `gorm:"default:300" json:"initial_pool_size"`
	EnableLogging                           *bool                          `gorm:"default:true" json:"enable_logging"`
	DisableContentLogging                   bool                           `gorm:"default:false" json:"disable_content_logging"`          // DisableContentLogging controls whether sensitive content (inputs, outputs, embeddings, etc.) is logged
	RetainContentInObjectStorage            bool                           `gorm:"default:false" json:"retain_content_in_object_storage"` // When content logging is disabled, still offload content to object storage as hidden instead of dropping it
	DisableDBPingsInHealth                  bool                           `gorm:"default:false" json:"disable_db_pings_in_health"`
	DumpErrorsInConsoleLogs                 bool                           `gorm:"default:false" json:"dump_errors_in_console_logs"`       // Dump full error details to the server console logs
	LogRetentionDays                        int                            `gorm:"default:365" json:"log_retention_days" validate:"min=1"` // Number of days to retain logs (minimum 1 day)
	EnforceAuthOnInference                  bool                           `gorm:"default:false" json:"enforce_auth_on_inference"`
	EnforceGovernanceHeader                 bool                           `gorm:"" json:"enforce_governance_header"`
	EnforceSCIMAuth                         bool                           `gorm:"default:false" json:"enforce_scim_auth"`
	DualCredentialConflictBehavior          DualCredentialConflictBehavior `gorm:"column:dual_credential_conflict_behavior;type:varchar(20);not null;default:'prefer_idp'" json:"dual_credential_conflict_behavior"`
	ExtraFieldsPlacement                    ExtraFieldsPlacement           `gorm:"column:extra_fields_placement;type:varchar(20);not null;default:'body'" json:"extra_fields_placement"`
	EntityAccessProfileMode                 EntityAccessProfileMode        `gorm:"column:entity_access_profile_mode;type:varchar(20);not null;default:'union'" json:"entity_access_profile_mode"`
	MaxRequestBodySizeMB                    int                            `gorm:"default:100" json:"max_request_body_size_mb"`
	AsyncJobResultTTL                       int                            `gorm:"default:3600" json:"async_job_result_ttl"`                           // Default TTL for async job results in seconds (default: 3600 = 1 hour)
	RequiredHeadersJSON                     string                         `gorm:"type:text" json:"-"`                                                 // JSON serialized []string
	LoggingHeadersJSON                      string                         `gorm:"type:text" json:"-"`                                                 // JSON serialized []string
	HideDeletedVirtualKeysInFilters         bool                           `gorm:"default:false" json:"hide_deleted_virtual_keys_in_filters"`          // Hide deleted virtual keys in logs filter dropdowns
	DeleteExpiredVirtualKeys                bool                           `gorm:"default:false" json:"delete_expired_virtual_keys"`                   // Default for virtual keys without an explicit delete_after_expire: delete them once expired
	EnforceVKObjectOwnership                bool                           `gorm:"default:true" json:"enforce_vk_object_ownership"`                    // Restrict provider objects to the virtual key that created them
	SkipAccessProfileUserVirtualKeyCreation bool                           `gorm:"default:false" json:"skip_access_profile_user_virtual_key_creation"` // A user's first access profile does not create a virtual key for them
	SkipAccessProfileUserVirtualKeyDeletion bool                           `gorm:"default:false" json:"skip_access_profile_user_virtual_key_deletion"` // Losing the last access profile does not delete the user's virtual keys
	HiddenRequestTypesJSON                  string                         `gorm:"type:text" json:"-"`                                                 // JSON serialized []string of request types hidden from log reads
	UsageEstimationTokenizersJSON           string                         `gorm:"type:text" json:"-"`                                                 // JSON serialized []string of tokenizer families used to estimate a cancelled stream's usage
	RoutingChainMaxDepth                    int                            `gorm:"default:10" json:"routing_chain_max_depth"`                          // Maximum depth for routing rule chain evaluation (default: 10)
	SkillsExternalClientURL                 string                         `gorm:"type:varchar(512)" json:"skills_external_client_url,omitempty"`      // Public base URL for Skills Repository marketplaces; empty disables marketplace serving
	WhitelistedRoutesJSON                   string                         `gorm:"type:text" json:"-"`                                                 // JSON serialized []string
	AllowPerRequestContentStorageOverride   bool                           `gorm:"default:false" json:"allow_per_request_content_storage_override"`    // Allow per-request override for content storage (e.g. long-term vs ephemeral)
	AllowPerRequestRawOverride              bool                           `gorm:"default:false" json:"allow_per_request_raw_override"`                // Allow per-request override for raw request/response storage
	AllowDirectKeys                         bool                           `gorm:"default:false" json:"allow_direct_keys"`                             // Allow callers to bypass the registered key pool via x-bf-direct-key header
	VKRotationCooldownNS                    int64                          `gorm:"column:vk_rotation_cooldown_ns;default:0" json:"-"`                  // Rotation grace period in nanoseconds (Go duration encoding); 0 = previous value stops working immediately

	// Compat plugin feature flags
	CompatConvertTextToChat                   bool `gorm:"column:compat_convert_text_to_chat;default:false" json:"-"`
	CompatConvertChatToResponses              bool `gorm:"column:compat_convert_chat_to_responses;default:false" json:"-"`
	CompatShouldDropParams                    bool `gorm:"column:compat_should_drop_params;default:false" json:"-"`
	CompatShouldConvertParams                 bool `gorm:"column:compat_should_convert_params;default:false" json:"-"`
	CompatAzureDeepseek                       bool `gorm:"column:compat_azure_deepseek;default:false" json:"-"`
	CompatForceReasoningOnlyModelsToResponses bool `gorm:"column:compat_force_reasoning_only_models_to_responses;default:false" json:"-"`
	CompatConvertDecisionToResponses          bool `gorm:"column:compat_convert_decision_to_responses;default:false" json:"-"`


	// Config hash is used to detect the changes synced from config.json file
	// Every time we sync the config.json file, we will update the config hash
	ConfigHash string `gorm:"type:varchar(255);null" json:"config_hash"`

	CreatedAt time.Time `gorm:"index;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"index;not null" json:"updated_at"`

	// Virtual fields for runtime use (not stored in DB)
	PrometheusLabels   []string                  `gorm:"-" json:"prometheus_labels"`
	AllowedOrigins     []string                  `gorm:"-" json:"allowed_origins,omitempty"`
	AllowedHeaders     []string                  `gorm:"-" json:"allowed_headers,omitempty"`
	RequiredHeaders    []string                  `gorm:"-" json:"required_headers,omitempty"`
	LoggingHeaders     []string                  `gorm:"-" json:"logging_headers,omitempty"`
	HiddenRequestTypes []string                  `gorm:"-" json:"hidden_request_types,omitempty"`
	WhitelistedRoutes  []string                  `gorm:"-" json:"whitelisted_routes,omitempty"`
	HeaderFilterConfig *GlobalHeaderFilterConfig `gorm:"-" json:"header_filter_config,omitempty"`
	Metadata           map[string]any            `gorm:"-" json:"metadata,omitempty"`

	// UsageEstimationTokenizers is persisted as UsageEstimationTokenizersJSON.
	UsageEstimationTokenizers []string `gorm:"-" json:"usage_estimation_tokenizers,omitempty"`
}

// TableName sets the table name for each model
func (TableClientConfig) TableName() string { return "config_client" }

func (cc *TableClientConfig) BeforeSave(tx *gorm.DB) error {
	if cc.PrometheusLabels != nil {
		data, err := json.Marshal(cc.PrometheusLabels)
		if err != nil {
			return err
		}
		cc.PrometheusLabelsJSON = string(data)
	} else {
		cc.PrometheusLabelsJSON = "[]"
	}

	if cc.AllowedOrigins != nil {
		data, err := json.Marshal(cc.AllowedOrigins)
		if err != nil {
			return err
		}
		cc.AllowedOriginsJSON = string(data)
	} else {
		cc.AllowedOriginsJSON = "[]"
	}

	if cc.AllowedHeaders != nil {
		data, err := json.Marshal(cc.AllowedHeaders)
		if err != nil {
			return err
		}
		cc.AllowedHeadersJSON = string(data)
	} else {
		cc.AllowedHeadersJSON = "[]"
	}

	if cc.WhitelistedRoutes != nil {
		data, err := json.Marshal(cc.WhitelistedRoutes)
		if err != nil {
			return err
		}
		cc.WhitelistedRoutesJSON = string(data)
	} else {
		cc.WhitelistedRoutesJSON = "[]"
	}

	if cc.RequiredHeaders != nil {
		data, err := json.Marshal(cc.RequiredHeaders)
		if err != nil {
			return err
		}
		cc.RequiredHeadersJSON = string(data)
	} else {
		cc.RequiredHeadersJSON = "[]"
	}

	if cc.LoggingHeaders != nil {
		data, err := json.Marshal(cc.LoggingHeaders)
		if err != nil {
			return err
		}
		cc.LoggingHeadersJSON = string(data)
	} else {
		cc.LoggingHeadersJSON = "[]"
	}

	if cc.HiddenRequestTypes != nil {
		data, err := json.Marshal(cc.HiddenRequestTypes)
		if err != nil {
			return err
		}
		cc.HiddenRequestTypesJSON = string(data)
	} else {
		cc.HiddenRequestTypesJSON = "[]"
	}

	if cc.UsageEstimationTokenizers != nil {
		data, err := json.Marshal(cc.UsageEstimationTokenizers)
		if err != nil {
			return err
		}
		cc.UsageEstimationTokenizersJSON = string(data)
	} else {
		cc.UsageEstimationTokenizersJSON = "[]"
	}

	if cc.HeaderFilterConfig != nil {
		data, err := json.Marshal(cc.HeaderFilterConfig)
		if err != nil {
			return err
		}
		cc.HeaderFilterConfigJSON = string(data)
	} else {
		cc.HeaderFilterConfigJSON = ""
	}

	// Metadata is preserved when nil — callers that DELETE+CREATE through
	// UpdateClientConfig must carry MetadataJSON forward explicitly, since the
	// API ClientConfig does not expose Metadata. A nil Metadata here means
	// "leave whatever MetadataJSON the caller set untouched."
	if cc.Metadata != nil {
		data, err := json.Marshal(cc.Metadata)
		if err != nil {
			return err
		}
		cc.MetadataJSON = string(data)
	}

	return nil
}

// AfterFind hooks for deserialization
func (cc *TableClientConfig) AfterFind(tx *gorm.DB) error {
	if cc.PrometheusLabelsJSON != "" {
		if err := json.Unmarshal([]byte(cc.PrometheusLabelsJSON), &cc.PrometheusLabels); err != nil {
			return err
		}
	}

	if cc.AllowedOriginsJSON != "" {
		if err := json.Unmarshal([]byte(cc.AllowedOriginsJSON), &cc.AllowedOrigins); err != nil {
			return err
		}
	}

	if cc.AllowedHeadersJSON != "" {
		if err := json.Unmarshal([]byte(cc.AllowedHeadersJSON), &cc.AllowedHeaders); err != nil {
			return err
		}
	}

	if cc.WhitelistedRoutesJSON != "" {
		if err := json.Unmarshal([]byte(cc.WhitelistedRoutesJSON), &cc.WhitelistedRoutes); err != nil {
			return err
		}
	}

	if cc.RequiredHeadersJSON != "" {
		if err := json.Unmarshal([]byte(cc.RequiredHeadersJSON), &cc.RequiredHeaders); err != nil {
			return err
		}
	}

	if cc.LoggingHeadersJSON != "" {
		if err := json.Unmarshal([]byte(cc.LoggingHeadersJSON), &cc.LoggingHeaders); err != nil {
			return err
		}
	}

	if cc.HiddenRequestTypesJSON != "" {
		if err := json.Unmarshal([]byte(cc.HiddenRequestTypesJSON), &cc.HiddenRequestTypes); err != nil {
			return err
		}
	}

	if cc.UsageEstimationTokenizersJSON != "" {
		if err := json.Unmarshal([]byte(cc.UsageEstimationTokenizersJSON), &cc.UsageEstimationTokenizers); err != nil {
			return err
		}
	}

	if cc.HeaderFilterConfigJSON != "" {
		var headerFilterConfig GlobalHeaderFilterConfig
		if err := json.Unmarshal([]byte(cc.HeaderFilterConfigJSON), &headerFilterConfig); err != nil {
			return err
		}
		cc.HeaderFilterConfig = &headerFilterConfig
	}

	if cc.MetadataJSON != "" {
		var metadata map[string]any
		if err := json.Unmarshal([]byte(cc.MetadataJSON), &metadata); err != nil {
			return err
		}
		cc.Metadata = metadata
	} else {
		cc.Metadata = nil
	}

	return nil
}

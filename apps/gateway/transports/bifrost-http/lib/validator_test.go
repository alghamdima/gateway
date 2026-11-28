package lib

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/vectorstore"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// loadLocalSchema reads the local config.schema.json for use in tests,
// avoiding remote fetches during test execution.
func loadLocalSchema(t *testing.T) []byte {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to get current file path")
	}
	schemaPath := filepath.Join(filepath.Dir(filename), "..", "..", "config.schema.json")
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("failed to read config.schema.json: %v", err)
	}
	return data
}

func TestValidateConfigSchema_ValidConfig(t *testing.T) {
	// Minimal valid config matching the schema
	validConfig := `{
		"providers": {
			"openai": {
				"keys": [
					{
						"name": "default",
						"value": "sk-test-key",
						"weight": 1.0,
						"models": ["gpt-4"]
					}
				]
			}
		}
	}`

	err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected valid config to pass validation, got error: %v", err)
	}
}

// TestValidateConfigSchema_GlobalProxyTypes: the file accepts the global proxy types the
// settings endpoint accepts (http, socks5) and still refuses tcp, which has no dialer.
func TestValidateConfigSchema_GlobalProxyTypes(t *testing.T) {
	for _, tc := range []struct {
		proxyType string
		valid     bool
	}{
		{"http", true},
		{"socks5", true},
		{"tcp", false},
	} {
		t.Run(tc.proxyType, func(t *testing.T) {
			config := `{"proxy_config": {"enabled": true, "type": "` + tc.proxyType + `", "url": "socks5://10.0.0.5:1080", "enable_for_inference": true}}`
			err := ValidateConfigSchema([]byte(config), loadLocalSchema(t))
			if tc.valid && err != nil {
				t.Fatalf("expected proxy_config.type %q to pass validation: %v", tc.proxyType, err)
			}
			if !tc.valid && err == nil {
				t.Fatalf("expected proxy_config.type %q to fail validation", tc.proxyType)
			}
		})
	}
}

// TestValidateConfigSchema_NoWorkloadIdentityMappings pins that config.json holds no
// workload identity mappings: workloads are mapped to service accounts in the dashboard or
// through the API, and only the trusted issuers live in the file, on the SSO provider.
func TestValidateConfigSchema_NoWorkloadIdentityMappings(t *testing.T) {
	config := `{"workload_identity": {"mappings": [{"name": "CI", "platform": "entra", "object_id": "5b1e2c7a", "service_account": "svc-ci"}]}}`
	if err := ValidateConfigSchema([]byte(config), loadLocalSchema(t)); err == nil {
		t.Fatal("expected a top-level workload_identity section to fail validation")
	}
}

func TestValidateConfigSchema_EmptyObject(t *testing.T) {
	// Empty object should be valid (all properties are optional)
	emptyConfig := `{}`

	err := ValidateConfigSchema([]byte(emptyConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected empty config to pass validation, got error: %v", err)
	}
}

func TestValidateConfigSchema_CustomSchemaURL(t *testing.T) {
	schema := loadLocalSchema(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(schema)
	}))
	t.Cleanup(server.Close)
	t.Setenv(ConfigSchemaURLEnv, "")

	config := []byte(`{"$schema":"` + server.URL + `"}`)
	err := ValidateConfigSchema(config)
	if err != nil {
		t.Errorf("expected schema to load from the config's custom $schema URL, got error: %v", err)
	}
}

func TestValidateConfigSchema_CustomSchemaURL_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	t.Setenv(ConfigSchemaURLEnv, "")

	config := []byte(`{"$schema":"` + server.URL + `"}`)
	err := ValidateConfigSchema(config)
	if err == nil {
		t.Fatal("expected a non-2xx schema response to fail")
	}
	if !strings.Contains(err.Error(), "failed to get config schema from") {
		t.Errorf("expected load error to name the schema location, got: %v", err)
	}
}

func TestValidateConfigSchema_CustomSchemaFilePath(t *testing.T) {
	schemaPath := filepath.Join(t.TempDir(), "config.schema.json")
	if err := os.WriteFile(schemaPath, loadLocalSchema(t), 0644); err != nil {
		t.Fatalf("failed to write temp schema: %v", err)
	}
	t.Setenv(ConfigSchemaURLEnv, schemaPath)

	err := ValidateConfigSchema([]byte(`{"$schema":"/opt/bifrost/config.schema.json"}`))
	if err != nil {
		t.Errorf("expected custom schema filesystem path to pass validation, got error: %v", err)
	}
}

func TestValidateConfigSchema_ConfigSchemaFilePath(t *testing.T) {
	schemaPath := filepath.Join(t.TempDir(), "config.schema.json")
	if err := os.WriteFile(schemaPath, loadLocalSchema(t), 0644); err != nil {
		t.Fatalf("failed to write temp schema: %v", err)
	}
	oldCandidates := localSchemaCandidates
	localSchemaCandidates = nil
	t.Cleanup(func() { localSchemaCandidates = oldCandidates })

	config := []byte(`{"$schema":"` + schemaPath + `"}`)
	err := ValidateConfigSchema(config)
	if err != nil {
		t.Errorf("expected config $schema filesystem path to load schema, got error: %v", err)
	}
}

func TestValidateConfigSchema_FileURL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "schema dir") // space exercises percent-decoding
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	schemaPath := filepath.Join(dir, "config.schema.json")
	if err := os.WriteFile(schemaPath, loadLocalSchema(t), 0644); err != nil {
		t.Fatalf("failed to write temp schema: %v", err)
	}
	fileURL := "file://" + strings.ReplaceAll(schemaPath, " ", "%20")
	t.Setenv(ConfigSchemaURLEnv, fileURL)

	err := ValidateConfigSchema([]byte(`{"$schema":"/opt/bifrost/config.schema.json"}`))
	if err != nil {
		t.Errorf("expected file:// schema URL to load schema from filesystem, got error: %v", err)
	}
}

func TestFilePathFromSchemaLocation(t *testing.T) {
	cases := map[string]string{
		"/opt/bifrost/config.schema.json":               "/opt/bifrost/config.schema.json",
		"file:///opt/bifrost/config.schema.json":        "/opt/bifrost/config.schema.json",
		"file://localhost/opt/config.schema.json":       "/opt/config.schema.json",
		"file:///opt/my%20dir/config.schema.json":       "/opt/my dir/config.schema.json",
		"https://www.getbifrost.ai/schema-as-path-only": "https://www.getbifrost.ai/schema-as-path-only",
	}
	for input, want := range cases {
		if got := filePathFromSchemaLocation(input); got != want {
			t.Errorf("filePathFromSchemaLocation(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestValidateConfigSchema_InvalidJSON(t *testing.T) {
	invalidJSON := `{invalid json`

	err := ValidateConfigSchema([]byte(invalidJSON), loadLocalSchema(t))
	if err == nil {
		t.Error("expected invalid JSON to fail validation")
	}
	if !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("expected error to mention 'invalid JSON', got: %v", err)
	}
}

func TestValidateConfigSchema_InvalidType(t *testing.T) {
	// client.initial_pool_size should be an integer, not a string
	invalidConfig := `{
		"client": {
			"initial_pool_size": "not-a-number"
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config with wrong type to fail validation")
	}
}

func TestValidateConfigSchema_InvalidEnum(t *testing.T) {
	// vector_store.type must be one of: weaviate, redis, qdrant, pinecone
	invalidConfig := `{
		"vector_store": {
			"enabled": true,
			"type": "invalid-store-type"
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config with invalid enum value to fail validation")
	}
}

func TestValidateConfigSchema_MissingRequiredField(t *testing.T) {
	// governance.budgets items require id, max_limit, and reset_duration
	invalidConfig := `{
		"governance": {
			"budgets": [
				{
					"id": "budget-1"
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing required fields to fail validation")
	}
}

func TestValidateConfigSchema_ValidGovernanceConfig(t *testing.T) {
	validConfig := `{
		"governance": {
			"budgets": [
				{
					"id": "budget-1",
					"max_limit": 100.0,
					"reset_duration": "1d"
				}
			],
			"virtual_keys": [
				{
					"id": "vk-1",
					"name": "Test Key",
					"value": "vk_test123"
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected valid governance config to pass validation, got error: %v", err)
	}
}

func TestValidateConfigSchema_ValidClientConfig(t *testing.T) {
	// allowed_origins with "*" or URI strings (but not both in same array according to oneOf)
	validConfig := `{
		"client": {
			"initial_pool_size": 500,
			"drop_excess_requests": true,
			"enable_logging": true,
			"log_retention_days": 30,
			"allowed_origins": ["https://example.com"]
		}
	}`

	err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected valid client config to pass validation, got error: %v", err)
	}
}

func TestValidateConfigSchema_InvalidMinimum(t *testing.T) {
	// initial_pool_size has minimum: 1
	invalidConfig := `{
		"client": {
			"initial_pool_size": 0
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config with value below minimum to fail validation")
	}
}

// =============================================================================
// Provider Key Required Fields Tests
// =============================================================================

func TestValidateConfigSchema_ProviderKey_Valid(t *testing.T) {
	// Valid provider key with all required fields: name, value, weight
	validConfig := `{
		"providers": {
			"openai": {
				"keys": [
					{
						"name": "my-key",
						"value": "sk-test-key-12345",
						"weight": 1.0
					}
				]
			}
		}
	}`

	err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected valid provider key config to pass validation, got error: %v", err)
	}
}

func TestValidateConfigSchema_ProviderKey_MissingName(t *testing.T) {
	// Missing required field: name
	invalidConfig := `{
		"providers": {
			"openai": {
				"keys": [
					{
						"value": "sk-test-key-12345",
						"weight": 1.0
					}
				]
			}
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'name' in provider key to fail validation")
	}
}

func TestValidateConfigSchema_ProviderKey_MissingWeight(t *testing.T) {
	// Missing required field: weight
	invalidConfig := `{
		"providers": {
			"openai": {
				"keys": [
					{
						"name": "my-key",
						"value": "sk-test-key-12345"
					}
				]
			}
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'weight' in provider key to fail validation")
	}
}

// =============================================================================
// Governance Budget Required Fields Tests
// =============================================================================

func TestValidateConfigSchema_Budget_Valid(t *testing.T) {
	// Valid budget with all required fields: id, max_limit, reset_duration
	validConfig := `{
		"governance": {
			"budgets": [
				{
					"id": "budget-1",
					"max_limit": 100.0,
					"reset_duration": "30d"
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected valid budget config to pass validation, got error: %v", err)
	}
}

func TestValidateConfigSchema_Budget_MissingId(t *testing.T) {
	// Missing required field: id
	invalidConfig := `{
		"governance": {
			"budgets": [
				{
					"max_limit": 100.0,
					"reset_duration": "30d"
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'id' in budget to fail validation")
	}
}

func TestValidateConfigSchema_Budget_MissingMaxLimit(t *testing.T) {
	// Missing required field: max_limit
	invalidConfig := `{
		"governance": {
			"budgets": [
				{
					"id": "budget-1",
					"reset_duration": "30d"
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'max_limit' in budget to fail validation")
	}
}

func TestValidateConfigSchema_Budget_MissingResetDuration(t *testing.T) {
	// Missing required field: reset_duration
	invalidConfig := `{
		"governance": {
			"budgets": [
				{
					"id": "budget-1",
					"max_limit": 100.0
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'reset_duration' in budget to fail validation")
	}
}

// =============================================================================
// Governance Rate Limit Required Fields Tests
// =============================================================================

func TestValidateConfigSchema_RateLimit_Valid(t *testing.T) {
	// Valid rate limit with required field: id
	validConfig := `{
		"governance": {
			"rate_limits": [
				{
					"id": "rate-limit-1",
					"token_max_limit": 10000,
					"token_reset_duration": "1h"
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected valid rate limit config to pass validation, got error: %v", err)
	}
}

func TestValidateConfigSchema_RateLimit_MissingId(t *testing.T) {
	// Missing required field: id
	invalidConfig := `{
		"governance": {
			"rate_limits": [
				{
					"token_max_limit": 10000,
					"token_reset_duration": "1h"
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'id' in rate limit to fail validation")
	}
}

// =============================================================================
// Governance Customer Required Fields Tests
// =============================================================================

func TestValidateConfigSchema_Customer_Valid(t *testing.T) {
	// Valid customer with all required fields: id, name
	validConfig := `{
		"governance": {
			"customers": [
				{
					"id": "customer-1",
					"name": "Acme Corp"
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected valid customer config to pass validation, got error: %v", err)
	}
}

func TestValidateConfigSchema_Customer_MissingId(t *testing.T) {
	// Missing required field: id
	invalidConfig := `{
		"governance": {
			"customers": [
				{
					"name": "Acme Corp"
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'id' in customer to fail validation")
	}
}

func TestValidateConfigSchema_Customer_MissingName(t *testing.T) {
	// Missing required field: name
	invalidConfig := `{
		"governance": {
			"customers": [
				{
					"id": "customer-1"
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'name' in customer to fail validation")
	}
}

// =============================================================================
// Governance Team Required Fields Tests
// =============================================================================

func TestValidateConfigSchema_Team_Valid(t *testing.T) {
	// Valid team with all required fields: id, name
	validConfig := `{
		"governance": {
			"teams": [
				{
					"id": "team-1",
					"name": "Engineering"
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected valid team config to pass validation, got error: %v", err)
	}
}

func TestValidateConfigSchema_Team_MissingId(t *testing.T) {
	// Missing required field: id
	invalidConfig := `{
		"governance": {
			"teams": [
				{
					"name": "Engineering"
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'id' in team to fail validation")
	}
}

func TestValidateConfigSchema_Team_MissingName(t *testing.T) {
	// Missing required field: name
	invalidConfig := `{
		"governance": {
			"teams": [
				{
					"id": "team-1"
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'name' in team to fail validation")
	}
}

// =============================================================================
// Governance Virtual Key Required Fields Tests
// =============================================================================

func TestValidateConfigSchema_VirtualKey_Valid(t *testing.T) {
	// Valid virtual key with all required fields: id, name, value
	validConfig := `{
		"governance": {
			"virtual_keys": [
				{
					"id": "vk-1",
					"name": "Test Virtual Key",
					"value": "vk_test_123456"
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected valid virtual key config to pass validation, got error: %v", err)
	}
}

func TestValidateConfigSchema_VirtualKey_MissingId(t *testing.T) {
	// Missing required field: id
	invalidConfig := `{
		"governance": {
			"virtual_keys": [
				{
					"name": "Test Virtual Key",
					"value": "vk_test_123456"
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'id' in virtual key to fail validation")
	}
}

func TestValidateConfigSchema_VirtualKey_MissingName(t *testing.T) {
	// Missing required field: name
	invalidConfig := `{
		"governance": {
			"virtual_keys": [
				{
					"id": "vk-1",
					"value": "vk_test_123456"
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'name' in virtual key to fail validation")
	}
}

// =============================================================================
// Virtual Key Provider Config Required Fields Tests
// =============================================================================

func TestValidateConfigSchema_VirtualKeyProviderConfig_Valid(t *testing.T) {
	// Valid virtual key provider config with required field: provider
	validConfig := `{
		"governance": {
			"virtual_keys": [
				{
					"id": "vk-1",
					"name": "Test Virtual Key",
					"value": "vk_test_123456",
					"provider_configs": [
						{
							"provider": "openai"
						}
					]
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected valid virtual key provider config to pass validation, got error: %v", err)
	}
}

func TestValidateConfigSchema_VirtualKeyProviderConfig_MissingProvider(t *testing.T) {
	// Missing required field: provider
	invalidConfig := `{
		"governance": {
			"virtual_keys": [
				{
					"id": "vk-1",
					"name": "Test Virtual Key",
					"value": "vk_test_123456",
					"provider_configs": [
						{
							"weight": 1.0
						}
					]
				}
			]
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'provider' in virtual key provider config to fail validation")
	}
}

// =============================================================================
// Concurrency Config Required Fields Tests
// =============================================================================

func TestValidateConfigSchema_ConcurrencyConfig_Valid(t *testing.T) {
	// Valid concurrency config with all required fields: concurrency, buffer_size
	validConfig := `{
		"providers": {
			"openai": {
				"keys": [
					{
						"name": "my-key",
						"value": "sk-test-key",
						"weight": 1.0
					}
				],
				"concurrency_and_buffer_size": {
					"concurrency": 10,
					"buffer_size": 100
				}
			}
		}
	}`

	err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected valid concurrency config to pass validation, got error: %v", err)
	}
}

func TestValidateConfigSchema_ConcurrencyConfig_MissingConcurrency(t *testing.T) {
	// Missing required field: concurrency
	invalidConfig := `{
		"providers": {
			"openai": {
				"keys": [
					{
						"name": "my-key",
						"value": "sk-test-key",
						"weight": 1.0
					}
				],
				"concurrency_and_buffer_size": {
					"buffer_size": 100
				}
			}
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'concurrency' in concurrency config to fail validation")
	}
}

func TestValidateConfigSchema_ConcurrencyConfig_MissingBufferSize(t *testing.T) {
	// Missing required field: buffer_size
	invalidConfig := `{
		"providers": {
			"openai": {
				"keys": [
					{
						"name": "my-key",
						"value": "sk-test-key",
						"weight": 1.0
					}
				],
				"concurrency_and_buffer_size": {
					"concurrency": 10
				}
			}
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'buffer_size' in concurrency config to fail validation")
	}
}

// =============================================================================
// Plugin Required Fields Tests
// =============================================================================

func TestValidateConfigSchema_Plugin_Valid(t *testing.T) {
	// Valid plugin with all required fields: enabled, name, config
	// Note: telemetry plugin requires config object
	validConfig := `{
		"plugins": [
			{
				"enabled": true,
				"name": "telemetry",
				"config": {}
			}
		]
	}`

	err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected valid plugin config to pass validation, got error: %v", err)
	}
}

func TestValidateConfigSchema_Plugin_MissingEnabled(t *testing.T) {
	// Missing required field: enabled
	invalidConfig := `{
		"plugins": [
			{
				"name": "telemetry"
			}
		]
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'enabled' in plugin to fail validation")
	}
}

func TestValidateConfigSchema_Plugin_MissingName(t *testing.T) {
	// Missing required field: name
	invalidConfig := `{
		"plugins": [
			{
				"enabled": true
			}
		]
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'name' in plugin to fail validation")
	}
}

// =============================================================================
// Semantic Cache Plugin Config Required Fields Tests
// =============================================================================

func TestValidateConfigSchema_SemanticCachePlugin_Valid(t *testing.T) {
	// Valid semantic cache plugin with provider, embedding model, and dimension. Keys are injected at runtime.
	validConfig := `{
		"plugins": [
			{
				"enabled": true,
				"name": "semantic_cache",
				"config": {
					"provider": "openai",
					"embedding_model": "text-embedding-3-small",
					"dimension": 1536
				}
			}
		]
	}`

	err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected valid semantic cache plugin config to pass validation, got error: %v", err)
	}
}

func TestValidateConfigSchema_SemanticCachePlugin_MissingProvider(t *testing.T) {
	// Missing required field: provider for semantic mode (dimension > 1)
	invalidConfig := `{
		"plugins": [
			{
				"enabled": true,
				"name": "semantic_cache",
				"config": {
					"dimension": 1536
				}
			}
		]
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'provider' in semantic cache plugin to fail validation")
	}
}

func TestValidateConfigSchema_SemanticCachePlugin_ProviderWithoutKeys(t *testing.T) {
	// Keys are not required at schema level for provider-backed config.
	validConfig := `{
		"plugins": [
			{
				"enabled": true,
				"name": "semantic_cache",
				"config": {
					"provider": "openai",
					"embedding_model": "text-embedding-3-small",
					"dimension": 1536
				}
			}
		]
	}`

	err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected provider-backed semantic cache config without plugin keys to pass validation, got error: %v", err)
	}
}

func TestValidateConfigSchema_SemanticCachePlugin_ProviderWithoutEmbeddingModel(t *testing.T) {
	invalidConfig := `{
		"plugins": [
			{
				"enabled": true,
				"name": "semantic_cache",
				"config": {
					"provider": "openai",
					"dimension": 1536
				}
			}
		]
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected provider-backed semantic cache config without embedding_model to fail validation")
	}
}

func TestValidateConfigSchema_SemanticCachePlugin_DirectModeValid(t *testing.T) {
	validConfig := `{
		"plugins": [
			{
				"enabled": true,
				"name": "semantic_cache",
				"config": {
					"dimension": 1
				}
			}
		]
	}`

	err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected direct-only semantic cache config to pass validation, got error: %v", err)
	}
}

// The identity-scoping opt-out is a documented config.json field, so the schema must accept it in
// both modes; additionalProperties would otherwise reject a config that turns sharing back on.
func TestValidateConfigSchema_SemanticCachePlugin_DisableIdentityScopingValid(t *testing.T) {
	for name, config := range map[string]string{
		"direct":   `{"dimension": 1, "disable_identity_scoping": true}`,
		"semantic": `{"provider": "openai", "embedding_model": "text-embedding-3-small", "dimension": 1536, "disable_identity_scoping": false}`,
	} {
		t.Run(name, func(t *testing.T) {
			validConfig := `{"plugins": [{"enabled": true, "name": "semantic_cache", "config": ` + config + `}]}`
			if err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t)); err != nil {
				t.Errorf("expected semantic cache config with disable_identity_scoping to pass validation, got error: %v", err)
			}
		})
	}
}

func TestValidateConfigSchema_SemanticCachePlugin_DirectModeWithEmbeddingModelInvalid(t *testing.T) {
	invalidConfig := `{
		"plugins": [
			{
				"enabled": true,
				"name": "semantic_cache",
				"config": {
					"dimension": 1,
					"embedding_model": "text-embedding-3-small"
				}
			}
		]
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected direct-only semantic cache config with embedding_model to fail validation")
	}
}

func TestValidateConfigSchema_SemanticCachePlugin_DimensionOneWithProviderInvalid(t *testing.T) {
	invalidConfig := `{
		"plugins": [
			{
				"enabled": true,
				"name": "semantic_cache",
				"config": {
					"provider": "openai",
					"embedding_model": "text-embedding-3-small",
					"dimension": 1
				}
			}
		]
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected dimension: 1 with provider in semantic cache plugin to fail validation")
	}
}

func TestValidateConfigSchema_SemanticCachePlugin_MissingDimension(t *testing.T) {
	// Missing required field: dimension
	invalidConfig := `{
		"plugins": [
			{
				"enabled": true,
				"name": "semantic_cache",
				"config": {
					"provider": "openai",
					"embedding_model": "text-embedding-3-small"
				}
			}
		]
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'dimension' in semantic cache plugin to fail validation")
	}
}

// =============================================================================
// OTEL Plugin Config Required Fields Tests
// =============================================================================

func TestValidateConfigSchema_OtelPlugin_Valid(t *testing.T) {
	// Valid OTEL plugin with all required fields: collector_url, trace_type, protocol
	validConfig := `{
		"plugins": [
			{
				"enabled": true,
				"name": "otel",
				"config": {
					"collector_url": "http://localhost:4318",
					"trace_type": "genai_extension",
					"protocol": "http"
				}
			}
		]
	}`

	err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected valid OTEL plugin config to pass validation, got error: %v", err)
	}
}

func TestValidateConfigSchema_OtelPlugin_GrpcAddress(t *testing.T) {
	// gRPC configs use bare host:port — previously failed because host:port satisfies both
	// format:uri (RFC 3986 opaque URI) and the host:port pattern, causing oneOf to reject it.
	grpcConfig := `{
		"plugins": [
			{
				"enabled": true,
				"name": "otel",
				"config": {
					"collector_url": "something.somewhere.svc.cluster.local:1111",
					"trace_type": "open_inference",
					"protocol": "grpc",
					"metrics_enabled": true,
					"metrics_endpoint": "something.somewhere.svc.cluster.local:1111"
				}
			}
		]
	}`

	err := ValidateConfigSchema([]byte(grpcConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected gRPC OTel config to pass validation, got error: %v", err)
	}
}

func TestValidateConfigSchema_OtelPlugin_MissingCollectorUrl(t *testing.T) {
	// Missing required field: collector_url
	invalidConfig := `{
		"plugins": [
			{
				"enabled": true,
				"name": "otel",
				"config": {
					"trace_type": "genai_extension",
					"protocol": "http"
				}
			}
		]
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'collector_url' in OTEL plugin to fail validation")
	}
}

func TestValidateConfigSchema_OtelPlugin_MissingTraceType(t *testing.T) {
	// Missing required field: trace_type
	invalidConfig := `{
		"plugins": [
			{
				"enabled": true,
				"name": "otel",
				"config": {
					"collector_url": "http://localhost:4318",
					"protocol": "http"
				}
			}
		]
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'trace_type' in OTEL plugin to fail validation")
	}
}

func TestValidateConfigSchema_OtelPlugin_MissingProtocol(t *testing.T) {
	// Missing required field: protocol
	invalidConfig := `{
		"plugins": [
			{
				"enabled": true,
				"name": "otel",
				"config": {
					"collector_url": "http://localhost:4318",
					"trace_type": "genai_extension"
				}
			}
		]
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'protocol' in OTEL plugin to fail validation")
	}
}

// =============================================================================
// Azure Key Config Required Fields Tests
// Note: Azure provider uses a special key schema that extends base_key
// The azure_key_config is only valid within the azure provider's keys array
// =============================================================================

func TestValidateConfigSchema_AzureKeyConfig_MissingEndpoint(t *testing.T) {
	// Missing required field: endpoint in azure_key_config
	// This test validates that when azure_key_config is present, endpoint is required
	invalidConfig := `{
		"providers": {
			"azure": {
				"keys": [
					{
						"name": "azure-key",
						"value": "azure-api-key",
						"weight": 1.0,
						"azure_key_config": {
							"api_version": "2024-02-15-preview"
						}
					}
				]
			}
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'endpoint' in Azure key config to fail validation")
	}
}

func TestValidateConfigSchema_AzureKeyConfig_ApiVersionOptional(t *testing.T) {
	// api_version is optional; Azure uses the provider default when it is omitted.
	validConfig := `{
		"providers": {
			"azure": {
				"keys": [
					{
						"name": "azure-key",
						"value": "azure-api-key",
						"weight": 1.0,
						"azure_key_config": {
							"endpoint": "https://my-resource.openai.azure.com"
						}
					}
				]
			}
		}
	}`

	err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t))
	if err != nil {
		t.Errorf("expected config missing optional 'api_version' in Azure key config to pass validation, got: %v", err)
	}
}

// =============================================================================
// Vertex Key Config Required Fields Tests
// Note: Vertex provider uses a special key schema that extends base_key
// =============================================================================

func TestValidateConfigSchema_VertexKeyConfig_MissingProjectId(t *testing.T) {
	// Missing required field: project_id in vertex_key_config
	invalidConfig := `{
		"providers": {
			"vertex": {
				"keys": [
					{
						"name": "vertex-key",
						"value": "vertex-api-key",
						"weight": 1.0,
						"vertex_key_config": {
							"region": "us-central1"
						}
					}
				]
			}
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'project_id' in Vertex key config to fail validation")
	}
}

func TestValidateConfigSchema_VertexKeyConfig_MissingRegion(t *testing.T) {
	// Missing required field: region in vertex_key_config
	invalidConfig := `{
		"providers": {
			"vertex": {
				"keys": [
					{
						"name": "vertex-key",
						"value": "vertex-api-key",
						"weight": 1.0,
						"vertex_key_config": {
							"project_id": "my-gcp-project"
						}
					}
				]
			}
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'region' in Vertex key config to fail validation")
	}
}

func TestValidateConfigSchema_VertexKeyConfig_AWSWorkloadIdentity(t *testing.T) {
	// A federation block with only an audience is the minimal valid shape.
	validConfig := `{
		"providers": {
			"vertex": {
				"keys": [
					{
						"name": "vertex-eks-key",
						"value": "",
						"weight": 1.0,
						"vertex_key_config": {
							"project_id": "my-gcp-project",
							"region": "us-central1",
							"aws_workload_identity": {
								"audience": "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/eks/providers/aws",
								"service_account_email": "vertex@my-gcp-project.iam.gserviceaccount.com",
								"token_lifetime_seconds": 1800,
								"aws_region": "us-east-1",
								"aws_role_arn": "env.VERTEX_AWS_ROLE_ARN"
							}
						}
					}
				]
			}
		}
	}`

	if err := ValidateConfigSchema([]byte(validConfig), loadLocalSchema(t)); err != nil {
		t.Errorf("expected Vertex key with aws_workload_identity to pass validation, got: %v", err)
	}
}

func TestValidateConfigSchema_VertexKeyConfig_AWSWorkloadIdentity_MissingAudience(t *testing.T) {
	invalidConfig := `{
		"providers": {
			"vertex": {
				"keys": [
					{
						"name": "vertex-eks-key",
						"value": "",
						"weight": 1.0,
						"vertex_key_config": {
							"project_id": "my-gcp-project",
							"region": "us-central1",
							"aws_workload_identity": {
								"service_account_email": "vertex@my-gcp-project.iam.gserviceaccount.com"
							}
						}
					}
				]
			}
		}
	}`

	if err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t)); err == nil {
		t.Error("expected aws_workload_identity without 'audience' to fail validation")
	}
}

func TestValidateConfigSchema_VertexKeyConfig_AWSWorkloadIdentity_UnknownField(t *testing.T) {
	invalidConfig := `{
		"providers": {
			"vertex": {
				"keys": [
					{
						"name": "vertex-eks-key",
						"value": "",
						"weight": 1.0,
						"vertex_key_config": {
							"project_id": "my-gcp-project",
							"region": "us-central1",
							"aws_workload_identity": {
								"audience": "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/eks/providers/aws",
								"pool_id": "eks"
							}
						}
					}
				]
			}
		}
	}`

	if err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t)); err == nil {
		t.Error("expected unknown field inside aws_workload_identity to fail validation")
	}
}

func TestValidateConfigSchema_VertexKeyConfig_AWSWorkloadIdentity_ExcludesAuthCredentials(t *testing.T) {
	// Federation and a credentials JSON are two different identities; the schema must refuse both at once.
	invalidConfig := `{
		"providers": {
			"vertex": {
				"keys": [
					{
						"name": "vertex-eks-key",
						"value": "",
						"weight": 1.0,
						"vertex_key_config": {
							"project_id": "my-gcp-project",
							"region": "us-central1",
							"auth_credentials": "{\"type\":\"service_account\"}",
							"aws_workload_identity": {
								"audience": "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/eks/providers/aws"
							}
						}
					}
				]
			}
		}
	}`

	if err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t)); err == nil {
		t.Error("expected auth_credentials alongside aws_workload_identity to fail validation")
	}

	// An empty auth_credentials string, as written by the UI when it clears the JSON tab, is fine.
	emptyCredentials := strings.Replace(invalidConfig, `"auth_credentials": "{\"type\":\"service_account\"}"`, `"auth_credentials": ""`, 1)
	if err := ValidateConfigSchema([]byte(emptyCredentials), loadLocalSchema(t)); err != nil {
		t.Errorf("expected empty auth_credentials alongside aws_workload_identity to pass validation, got: %v", err)
	}
}

// =============================================================================
// Bedrock Key Config Required Fields Tests
// Note: Bedrock provider uses a special key schema that extends base_key
// =============================================================================

func TestValidateConfigSchema_BedrockKeyConfig_MissingRegion(t *testing.T) {
	// Missing required field: region in bedrock_key_config
	invalidConfig := `{
		"providers": {
			"bedrock": {
				"keys": [
					{
						"name": "bedrock-key",
						"value": "bedrock-api-key",
						"weight": 1.0,
						"bedrock_key_config": {
							"access_key": "AKIAIOSFODNN7EXAMPLE"
						}
					}
				]
			}
		}
	}`

	err := ValidateConfigSchema([]byte(invalidConfig), loadLocalSchema(t))
	if err == nil {
		t.Error("expected config missing 'region' in Bedrock key config to fail validation")
	}
}

func TestValidateConfigSchema_VirtualKeyDeleteAfterExpireRequiresExpiresAt(t *testing.T) {
	schema := loadLocalSchema(t)
	withoutExpiry := `{
		"governance": {
			"virtual_keys": [
				{"id": "vk-1", "name": "Test Key", "delete_after_expire": true}
			]
		}
	}`
	if err := ValidateConfigSchema([]byte(withoutExpiry), schema); err == nil {
		t.Error("expected delete_after_expire without expires_at to fail validation")
	}

	withExpiry := `{
		"governance": {
			"virtual_keys": [
				{"id": "vk-1", "name": "Test Key", "expires_at": "2027-01-01T00:00:00Z", "delete_after_expire": true}
			]
		}
	}`
	if err := ValidateConfigSchema([]byte(withExpiry), schema); err != nil {
		t.Errorf("expected delete_after_expire with expires_at to pass validation, got error: %v", err)
	}
}

// getSchemaPath returns the absolute path to config.schema.json.
func getSchemaPath(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to get caller info")
	}
	schemaPath := filepath.Join(filepath.Dir(filename), "..", "..", "config.schema.json")
	if _, err := os.Stat(schemaPath); err != nil {
		t.Fatalf("config.schema.json not found at %s", schemaPath)
	}
	return schemaPath
}

// navigateJSON traverses a nested JSON structure using a sequence of keys.
// Supports string keys for objects and int keys for arrays.
func navigateJSON(data interface{}, keys ...interface{}) (interface{}, bool) {
	current := data
	for _, key := range keys {
		switch k := key.(type) {
		case string:
			m, ok := current.(map[string]interface{})
			if !ok {
				return nil, false
			}
			current, ok = m[k]
			if !ok {
				return nil, false
			}
		case int:
			arr, ok := current.([]interface{})
			if !ok || k >= len(arr) {
				return nil, false
			}
			current = arr[k]
		default:
			return nil, false
		}
	}
	return current, true
}

// findPostgresPortType finds the port type in a store's postgres config branch.
// It handles both anyOf and oneOf schema patterns used by config_store and logs_store.
func findPostgresPortType(schema map[string]interface{}, storeName string) (string, bool) {
	configBlock, ok := navigateJSON(schema, "properties", storeName, "properties", "config")
	if !ok {
		return "", false
	}
	configMap, ok := configBlock.(map[string]interface{})
	if !ok {
		return "", false
	}

	var branches []interface{}
	if anyOf, exists := configMap["anyOf"]; exists {
		branches, _ = anyOf.([]interface{})
	} else if oneOf, exists := configMap["oneOf"]; exists {
		branches, _ = oneOf.([]interface{})
	}

	for _, branch := range branches {
		thenBlock, ok := navigateJSON(branch, "then")
		if !ok {
			continue
		}
		portType, ok := navigateJSON(thenBlock, "properties", "port", "type")
		if !ok {
			continue
		}
		if typeStr, ok := portType.(string); ok {
			return typeStr, true
		}
	}
	return "", false
}

func TestSchemaLogsStorePortType(t *testing.T) {
	schemaPath := getSchemaPath(t)
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("failed to read schema: %v", err)
	}

	var schema map[string]interface{}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("failed to parse schema: %v", err)
	}

	t.Run("logs_store port type is string", func(t *testing.T) {
		portType, found := findPostgresPortType(schema, "logs_store")
		if !found {
			t.Fatal("could not find logs_store postgres port type in schema")
		}
		if portType != "string" {
			t.Errorf("logs_store.config.port type = %q, want %q (Go code uses *schemas.SecretVar)", portType, "string")
		}
	})

	t.Run("config_store port type is string", func(t *testing.T) {
		portType, found := findPostgresPortType(schema, "config_store")
		if !found {
			t.Fatal("could not find config_store postgres port type in schema")
		}
		if portType != "string" {
			t.Errorf("config_store.config.port type = %q, want %q (Go code uses *schemas.SecretVar)", portType, "string")
		}
	})

	t.Run("both store port types are consistent", func(t *testing.T) {
		logsPortType, logsFound := findPostgresPortType(schema, "logs_store")
		configPortType, configFound := findPostgresPortType(schema, "config_store")
		if !logsFound || !configFound {
			t.Fatal("both store port types must be found in schema")
		}
		if logsPortType != configPortType {
			t.Errorf("port type mismatch: logs_store=%q, config_store=%q", logsPortType, configPortType)
		}
	})
}

func TestSchemaPostgresPasswordCommand(t *testing.T) {
	compiled := compileSchema(t)

	config := `{
		"config_store": {
			"enabled": true,
			"type": "postgres",
			"config": {
				"host": "db.example.com",
				"port": "5432",
				"user": "bifrost",
				"password_command": {
					"command": "aws",
					"args": ["rds", "generate-db-auth-token"],
					"timeout": "10s"
				},
				"db_name": "bifrost",
				"ssl_mode": "require",
				"conn_max_lifetime": "10m"
			}
		},
		"logs_store": {
			"enabled": true,
			"type": "postgres",
			"config": {
				"host": "db.example.com",
				"port": "5432",
				"user": "bifrost",
				"password_command": {
					"command": "aws",
					"args": ["rds", "generate-db-auth-token"],
					"timeout": "10s"
				},
				"db_name": "bifrost",
				"ssl_mode": "require",
				"conn_max_lifetime": "10m"
			}
		}
	}`

	if err := validateConfig(t, compiled, config); err != nil {
		t.Fatalf("postgres password_command config should be valid, got: %v", err)
	}
}

func TestSchemaPostgresPasswordCommandValidation(t *testing.T) {
	compiled := compileSchema(t)

	tests := []struct {
		name   string
		config string
	}{
		{
			name: "config_store rejects password and password_command together",
			config: postgresStoreConfig("config_store", `"password": "secret",
				"password_command": {"command": "aws"}`),
		},
		{
			name: "logs_store rejects password and password_command together",
			config: postgresStoreConfig("logs_store", `"password": "secret",
				"password_command": {"command": "aws"}`),
		},
		{
			name:   "config_store rejects empty password command",
			config: postgresStoreConfig("config_store", `"password_command": {"command": ""}`),
		},
		{
			name:   "config_store rejects inline args in password command",
			config: postgresStoreConfig("config_store", `"password_command": {"command": "aws rds"}`),
		},
		{
			name:   "config_store rejects zero password command timeout",
			config: postgresStoreConfig("config_store", `"password_command": {"command": "aws", "timeout": "0s"}`),
		},
		{
			name:   "logs_store rejects zero password command timeout",
			config: postgresStoreConfig("logs_store", `"password_command": {"command": "aws", "timeout": "0s"}`),
		},
		{
			name: "config_store rejects zero conn max lifetime",
			config: postgresStoreConfig("config_store", `"password_command": {"command": "aws"},
				"conn_max_lifetime": "0s"`),
		},
		{
			name: "logs_store rejects zero conn max lifetime",
			config: postgresStoreConfig("logs_store", `"password_command": {"command": "aws"},
				"conn_max_lifetime": "0s"`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateConfig(t, compiled, tt.config); err == nil {
				t.Fatal("config should be invalid")
			}
		})
	}
}

func TestSchemaLogsStoreWriterConfig(t *testing.T) {
	compiled := compileSchema(t)

	validConfig := `{
		"logs_store": {
			"enabled": true,
			"type": "sqlite",
			"config": {
				"path": "/tmp/logs.db"
			},
			"writer": {
				"max_batch_size": 500,
				"batch_interval": "2s",
				"max_batch_bytes": 1048576,
				"write_queue_capacity": 2000,
				"deferred_usage_concurrency": 3
			}
		}
	}`
	if err := validateConfig(t, compiled, validConfig); err != nil {
		t.Fatalf("logs_store.writer config should be valid, got: %v", err)
	}

	tests := []struct {
		name   string
		writer string
	}{
		{
			name:   "rejects zero max batch size",
			writer: `"max_batch_size": 0`,
		},
		{
			name:   "rejects zero batch interval",
			writer: `"batch_interval": "0s"`,
		},
		{
			name:   "rejects zero max batch bytes",
			writer: `"max_batch_bytes": 0`,
		},
		{
			name:   "rejects zero write queue capacity",
			writer: `"write_queue_capacity": 0`,
		},
		{
			name:   "rejects zero deferred usage concurrency",
			writer: `"deferred_usage_concurrency": 0`,
		},
		{
			name:   "rejects unknown writer field",
			writer: `"unknown": 1`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := fmt.Sprintf(`{
				"logs_store": {
					"enabled": true,
					"type": "sqlite",
					"config": {
						"path": "/tmp/logs.db"
					},
					"writer": {
						%s
					}
				}
			}`, tt.writer)
			if err := validateConfig(t, compiled, config); err == nil {
				t.Fatal("config should be invalid")
			}
		})
	}
}

// TestSchemaPostgresSessionTimeouts pins that both Postgres stores accept the
// runtime-pool session timeouts postgresconn.Config reads (statement_timeout,
// idle_in_transaction_session_timeout) with every value time.ParseDuration takes
// there, including the documented "0" and negative values that keep the server
// default, and still reject values it would refuse at startup.
func TestSchemaPostgresSessionTimeouts(t *testing.T) {
	compiled := compileSchema(t)
	for _, store := range []string{"config_store", "logs_store"} {
		for _, field := range []string{"statement_timeout", "idle_in_transaction_session_timeout"} {
			for _, value := range []string{"30s", "60s", "1m30s", "1.5s", "0", "-1s", "+1s"} {
				config := postgresStoreConfig(store, fmt.Sprintf(`"password": "secret", %q: %q`, field, value))
				if err := validateConfig(t, compiled, config); err != nil {
					t.Errorf("%s.config.%s = %q should be valid, got: %v", store, field, value, err)
				}
			}
			for _, value := range []string{"30", "abc", "", "5 s"} {
				config := postgresStoreConfig(store, fmt.Sprintf(`"password": "secret", %q: %q`, field, value))
				if err := validateConfig(t, compiled, config); err == nil {
					t.Errorf("%s.config.%s = %q should be rejected", store, field, value)
				}
			}
		}
	}
}

func postgresStoreConfig(storeName string, passwordFields string) string {
	return fmt.Sprintf(`{
		"%s": {
			"enabled": true,
			"type": "postgres",
			"config": {
				"host": "db.example.com",
				"port": "5432",
				"user": "bifrost",
				%s,
				"db_name": "bifrost",
				"ssl_mode": "require"
			}
		}
	}`, storeName, passwordFields)
}

func TestSchemaObjectStorageAzure(t *testing.T) {
	compiled := compileSchema(t)
	tests := []struct {
		name      string
		storage   string
		wantError bool
	}{
		{name: "identity", storage: `{"type":"azure","bucket":"logs","endpoint":"https://example.blob.core.windows.net"}`},
		{name: "connection string", storage: `{"type":"azure","bucket":"logs","connection_string":"UseDevelopmentStorage=true"}`},
		{name: "environment references", storage: `{"type":"azure","bucket":"env.AZURE_CONTAINER","connection_string":"env.AZURE_STORAGE_CONNECTION_STRING"}`},
		{name: "structured secrets", storage: `{"type":"azure","bucket":{"value":"","ref":"env.AZURE_CONTAINER","type":"env"},"endpoint":{"value":"","ref":"env.AZURE_BLOB_ENDPOINT","type":"env"}}`},
		{name: "structured connection string", storage: `{"type":"azure","bucket":"logs","connection_string":{"value":"","ref":"env.AZURE_STORAGE_CONNECTION_STRING","type":"env"}}`},
		{name: "inferred structured env reference", storage: `{"type":"azure","bucket":"logs","endpoint":{"value":"","ref":"env.AZURE_BLOB_ENDPOINT"}}`},
		{name: "structured vault references", storage: `{"type":"azure","bucket":{"value":"","ref":"vault.azure/container","type":"vault"},"connection_string":{"value":"","ref":"vault.azure/connection-string","type":"vault"}}`},
		{name: "structured vault endpoint", storage: `{"type":"azure","bucket":"logs","endpoint":{"value":"","ref":"vault.azure/endpoint","type":"vault"}}`},
		{name: "legacy env references", storage: `{"type":"azure","bucket":{"value":"","env_var":"AZURE_CONTAINER","from_env":true},"endpoint":{"value":"","env_var":"AZURE_BLOB_ENDPOINT","from_env":true}}`},
		{name: "legacy connection string reference", storage: `{"type":"azure","bucket":"logs","connection_string":{"value":"","env_var":"AZURE_STORAGE_CONNECTION_STRING","from_env":true}}`},
		{name: "reference-only env identity", storage: `{"type":"azure","bucket":{"ref":"env.AZURE_CONTAINER","type":"env"},"endpoint":{"ref":"env.AZURE_BLOB_ENDPOINT","type":"env"}}`},
		{name: "reference-only env connection string", storage: `{"type":"azure","bucket":"logs","connection_string":{"ref":"env.AZURE_STORAGE_CONNECTION_STRING","type":"env"}}`},
		{name: "reference-only vault identity", storage: `{"type":"azure","bucket":{"ref":"vault.azure/container","type":"vault"},"endpoint":{"ref":"vault.azure/endpoint","type":"vault"}}`},
		{name: "reference-only vault connection string", storage: `{"type":"azure","bucket":"logs","connection_string":{"ref":"vault.azure/connection-string","type":"vault"}}`},
		{name: "reference-only legacy identity", storage: `{"type":"azure","bucket":{"env_var":"AZURE_CONTAINER","from_env":true},"endpoint":{"env_var":"AZURE_BLOB_ENDPOINT","from_env":true}}`},
		{name: "reference-only legacy connection string", storage: `{"type":"azure","bucket":"logs","connection_string":{"env_var":"AZURE_STORAGE_CONNECTION_STRING","from_env":true}}`},
		{name: "gzip", storage: `{"type":"azure","bucket":"logs","connection_string":"UseDevelopmentStorage=true","compress":true}`},
		{name: "missing container", storage: `{"type":"azure","endpoint":"https://example.blob.core.windows.net"}`, wantError: true},
		{name: "empty container", storage: `{"type":"azure","bucket":"","endpoint":"https://example.blob.core.windows.net"}`, wantError: true},
		{name: "empty structured container", storage: `{"type":"azure","bucket":{"value":""},"endpoint":"https://example.blob.core.windows.net"}`, wantError: true},
		{name: "missing authentication", storage: `{"type":"azure","bucket":"logs"}`, wantError: true},
		{name: "conflicting authentication", storage: `{"type":"azure","bucket":"logs","endpoint":"https://example.blob.core.windows.net","connection_string":"UseDevelopmentStorage=true"}`, wantError: true},
		{name: "empty endpoint", storage: `{"type":"azure","bucket":"logs","endpoint":""}`, wantError: true},
		{name: "empty connection string", storage: `{"type":"azure","bucket":"logs","connection_string":""}`, wantError: true},
		{name: "empty structured endpoint", storage: `{"type":"azure","bucket":"logs","endpoint":{"value":""}}`, wantError: true},
		{name: "empty structured connection string", storage: `{"type":"azure","bucket":"logs","connection_string":{"value":""}}`, wantError: true},
		{name: "empty reference rejected", storage: `{"type":"azure","bucket":"logs","connection_string":{"value":"","ref":"","type":"env"}}`, wantError: true},
		{name: "empty legacy env name rejected", storage: `{"type":"azure","bucket":"logs","endpoint":{"value":"","env_var":"","from_env":true}}`, wantError: true},
		{name: "inactive legacy env reference rejected", storage: `{"type":"azure","bucket":"logs","endpoint":{"value":"","env_var":"AZURE_BLOB_ENDPOINT","from_env":false}}`, wantError: true},
		{name: "legacy name without flag rejected", storage: `{"type":"azure","bucket":"logs","connection_string":{"value":"","env_var":"AZURE_STORAGE_CONNECTION_STRING"}}`, wantError: true},
		{name: "empty container object", storage: `{"type":"azure","bucket":{},"endpoint":"https://example.blob.core.windows.net"}`, wantError: true},
		{name: "empty endpoint object", storage: `{"type":"azure","bucket":"logs","endpoint":{}}`, wantError: true},
		{name: "empty connection string object", storage: `{"type":"azure","bucket":"logs","connection_string":{}}`, wantError: true},
		{name: "reference without type or value", storage: `{"type":"azure","bucket":"logs","connection_string":{"ref":"env.AZURE_STORAGE_CONNECTION_STRING"}}`, wantError: true},
		{name: "reference-only empty reference", storage: `{"type":"azure","bucket":"logs","connection_string":{"ref":"","type":"env"}}`, wantError: true},
		{name: "reference-only inactive legacy reference", storage: `{"type":"azure","bucket":"logs","endpoint":{"env_var":"AZURE_BLOB_ENDPOINT","from_env":false}}`, wantError: true},
		{name: "null endpoint", storage: `{"type":"azure","bucket":"logs","endpoint":null}`, wantError: true},
		{name: "S3 field rejected", storage: `{"type":"azure","bucket":"logs","endpoint":"https://example.blob.core.windows.net","access_key_id":"key"}`, wantError: true},
		{name: "GCS field rejected", storage: `{"type":"azure","bucket":"logs","endpoint":"https://example.blob.core.windows.net","credentials_json":"{}"}`, wantError: true},
		{name: "unknown field rejected", storage: `{"type":"azure","bucket":"logs","endpoint":"https://example.blob.core.windows.net","unknown":true}`, wantError: true},
		{name: "S3 unchanged", storage: `{"type":"s3","bucket":"logs","region":"us-east-1","endpoint":"https://minio.example","access_key_id":"key","secret_access_key":"secret"}`},
		{name: "GCS unchanged", storage: `{"type":"gcs","bucket":"logs","project_id":"project","credentials_json":{"value":"GCS_CREDENTIALS","type":"env"}}`},
		{name: "S3 rejects Azure connection string", storage: `{"type":"s3","bucket":"logs","connection_string":"UseDevelopmentStorage=true"}`, wantError: true},
		{name: "GCS rejects Azure endpoint", storage: `{"type":"gcs","bucket":"logs","endpoint":"https://example.blob.core.windows.net"}`, wantError: true},
		{name: "S3 reference-only bucket unchanged", storage: `{"type":"s3","bucket":{"ref":"env.S3_BUCKET","type":"env"}}`, wantError: true},
		{name: "GCS reference-only bucket unchanged", storage: `{"type":"gcs","bucket":{"ref":"env.GCS_BUCKET","type":"env"}}`, wantError: true},
	}
	for _, target := range []struct {
		name   string
		config string
	}{
		{name: "logs", config: `{"logs_store":{"enabled":true,"type":"sqlite","config":{"path":"/tmp/logs.db"},"object_storage":%s}}`},
	} {
		t.Run(target.name, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					err := validateConfig(t, compiled, fmt.Sprintf(target.config, tt.storage))
					if tt.wantError && err == nil {
						t.Fatal("expected schema validation to fail")
					}
					if !tt.wantError && err != nil {
						t.Fatalf("expected schema validation to pass: %v", err)
					}
				})
			}
		})
	}
}

func TestSchemaObjectStorageAzureFormats(t *testing.T) {
	compiled := compileSchema(t)
	for _, tt := range []struct {
		name      string
		fields    string
		wantError bool
	}{
		{name: "Parquet snappy", fields: `"format":"parquet","compression":"snappy"`},
		{name: "Parquet zstd", fields: `"format":"parquet","compression":"zstd"`},
		{name: "JSON gzip", fields: `"format":"json","compression":"gzip"`},
		{name: "JSON rejects snappy", fields: `"format":"json","compression":"snappy"`, wantError: true},
		{name: "default JSON rejects zstd", fields: `"compression":"zstd"`, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			storage := `{"type":"azure","bucket":"logs","connection_string":"UseDevelopmentStorage=true",` + tt.fields + `}`
			config := `{"logs_store":{"enabled":true,"type":"sqlite","config":{"path":"/tmp/logs.db"},"object_storage":` + storage + `}}`
			err := validateConfig(t, compiled, config)
			if tt.wantError && err == nil {
				t.Fatal("expected schema validation to fail")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("expected schema validation to pass: %v", err)
			}
		})
	}
}

// compileSchema loads and compiles the config.schema.json for validation tests.
func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	schemaPath := getSchemaPath(t)
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("failed to read schema: %v", err)
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("failed to parse schema JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("config.schema.json", schemaDoc); err != nil {
		t.Fatalf("failed to add schema resource: %v", err)
	}
	compiled, err := c.Compile("config.schema.json")
	if err != nil {
		t.Fatalf("failed to compile schema: %v", err)
	}
	return compiled
}

// validateConfig unmarshals a JSON config string and validates it against the schema.
func validateConfig(t *testing.T, schema *jsonschema.Schema, configJSON string) error {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(configJSON), &v); err != nil {
		t.Fatalf("invalid test JSON: %v", err)
	}
	return schema.Validate(v)
}

func TestSchemaComplexityAnalyzerDependencies(t *testing.T) {
	compiled := compileSchema(t)

	tests := []struct {
		name      string
		analyzer  string
		wantError bool
	}{
		{
			name:     "base analyzer remains valid",
			analyzer: complexityAnalyzerSchemaConfig(),
		},
		{
			name:     "disabled session does not require semantic config",
			analyzer: complexityAnalyzerSchemaConfig(`,"session":{"enabled":false}`),
		},
		{
			name:      "enabled session requires semantic config",
			analyzer:  complexityAnalyzerSchemaConfig(`,"session":{"enabled":true}`),
			wantError: true,
		},
		{
			name:     "enabled session accepts semantic config",
			analyzer: complexityAnalyzerSchemaConfig(`,"session":{"enabled":true}`, `,"semantic":{"provider":"openai","embedding_model":"text-embedding-3-small"}`),
		},
		{
			name:     "enabled suggestions accept their own model without an llm block",
			analyzer: complexityAnalyzerSchemaConfig(semanticSchemaBlock, `,"suggestions":{"enabled":true,"provider":"openai","model":"gpt-5"}`),
		},
		{
			name:      "enabled suggestions require a model",
			analyzer:  complexityAnalyzerSchemaConfig(semanticSchemaBlock, `,"suggestions":{"enabled":true}`),
			wantError: true,
		},
		{
			name:      "enabled suggestions are not satisfied by a model without its provider",
			analyzer:  complexityAnalyzerSchemaConfig(semanticSchemaBlock, `,"suggestions":{"enabled":true,"model":"gpt-5"}`),
			wantError: true,
		},
		{
			name:      "enabled suggestions require a semantic block",
			analyzer:  complexityAnalyzerSchemaConfig(`,"suggestions":{"enabled":true,"provider":"openai","model":"gpt-5"}`),
			wantError: true,
		},
		{
			name:      "enabled suggestions reject a decision primary classifier",
			analyzer:  complexityAnalyzerSchemaConfig(semanticSchemaBlock, `,"classifier":"decision","suggestions":{"enabled":true,"provider":"openai","model":"gpt-5"}`),
			wantError: true,
		},
		{
			name:     "disabled suggestions need neither semantic nor a model",
			analyzer: complexityAnalyzerSchemaConfig(`,"suggestions":{"enabled":false}`),
		},
		{
			name:     "suggestions accept sample rate and lookback",
			analyzer: complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"sample_rate":0.25,"lookback_days":14}`),
		},
		{
			name:      "suggestions require enabled",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"sample_rate":0.25}`),
			wantError: true,
		},
		{
			name:      "suggestions reject a zero sample rate",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"sample_rate":0}`),
			wantError: true,
		},
		{
			name:      "suggestions reject a lookback past 90 days",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"lookback_days":91}`),
			wantError: true,
		},
		{
			name:      "suggestions reject unknown fields",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"samplerate":0.5}`),
			wantError: true,
		},
		{
			name: "suggestions accept run settings",
			analyzer: complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"schedule":{"every":"1w","start_at":"2026-11-06T04:30:00Z"},`+
				`"max_clustered_requests":5000,"min_cluster_requests":10,"provider":"openai","model":"gpt-5","instructions":"be brief"}`),
		},
		{
			name: "suggestions accept a decision tier step",
			analyzer: complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"provider":"openai","model":"gpt-5",`+
				`"tier":{"classifier":"decision","provider":"typesafe","model":"jev-latest","instructions":"judge the work"}}`),
		},
		{
			name:     "suggestions accept an llm tier step without a model",
			analyzer: complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"tier":{"classifier":"llm"}}`),
		},
		{
			name: "suggestions accept an edited tier prompt and per-tier criteria",
			analyzer: complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"tier":{"classifier":"decision","prompt":"Refunds are SIMPLE.",`+
				`"instructions":"Which tier?","criteria":{"SIMPLE":{"definition":"One short answer.","signals":[],"examples":["where is my refund"]}}}}`),
		},
		{
			name:     "suggestions accept a tier prompt without a classifier, which defaults to llm",
			analyzer: complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"tier":{"prompt":"Refunds are SIMPLE."}}`),
		},
		{
			name:      "suggestions reject tier criteria for an unknown tier",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"tier":{"classifier":"decision","criteria":{"HARD":{"definition":"x"}}}}`),
			wantError: true,
		},
		{
			name:      "suggestions reject tier criteria without a definition",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"tier":{"classifier":"decision","criteria":{"SIMPLE":{"examples":["hi"]}}}}`),
			wantError: true,
		},
		{
			name:      "suggestions reject an unknown tier classifier",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"tier":{"classifier":"lexical"}}`),
			wantError: true,
		},
		{
			name:      "suggestions reject a tier provider without a model",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"tier":{"classifier":"llm","provider":"openai"}}`),
			wantError: true,
		},
		{
			name:      "suggestions reject a blank tier provider",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"tier":{"classifier":"llm","provider":"  ","model":"gpt-4o-mini"}}`),
			wantError: true,
		},
		{
			name:      "suggestions reject a blank tier model",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"tier":{"classifier":"llm","provider":"openai","model":"\t"}}`),
			wantError: true,
		},
		{
			name:      "suggestions reject a blank tier provider and model",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"tier":{"classifier":"llm","provider":" ","model":" "}}`),
			wantError: true,
		},
		{
			// As for the phrase model: Go's regexp \S is ASCII-only, so a
			// non-breaking space passed it, then strings.TrimSpace emptied the
			// tier provider and the config failed to load.
			name:      "suggestions reject a Unicode-whitespace-only tier provider",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"tier":{"classifier":"llm","provider":" ","model":"gpt-4o-mini"}}`),
			wantError: true,
		},
		{
			name:      "suggestions reject a Unicode-whitespace-only tier model",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"tier":{"classifier":"llm","provider":"openai","model":"\u0085"}}`),
			wantError: true,
		},
		{
			name:     "suggestions accept a tier provider and model",
			analyzer: complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"tier":{"classifier":"llm","provider":"openai","model":"gpt-4o-mini"}}`),
		},
		{
			name:      "suggestions reject unknown tier fields",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"tier":{"mode":"llm"}}`),
			wantError: true,
		},
		{
			name:     "suggestions accept an hourly schedule",
			analyzer: complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"schedule":{"every":"6h"}}`),
		},
		{
			name:      "suggestions reject hours of a day or more",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"schedule":{"every":"24h"}}`),
			wantError: true,
		},
		{
			name:      "suggestions reject a malformed first run",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"schedule":{"every":"1d","start_at":"tomorrow at 3"}}`),
			wantError: true,
		},
		{
			name:      "suggestions reject the old wall-clock fields",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"schedule":{"every":"1d","at":"03:00","timezone":"UTC"}}`),
			wantError: true,
		},
		{
			name:      "suggestions reject unknown schedule fields",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"schedule":{"cron":"0 3 * * *"}}`),
			wantError: true,
		},
		{
			name:      "suggestions reject a provider without a model",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"provider":"openai"}`),
			wantError: true,
		},
		{
			name:      "suggestions reject a whitespace-only provider and model",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"provider":"  ","model":"\t"}`),
			wantError: true,
		},
		{
			name:      "suggestions reject a whitespace-only provider",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"provider":" ","model":"gpt-5"}`),
			wantError: true,
		},
		{
			name:      "suggestions reject a whitespace-only model",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"provider":"openai","model":"  "}`),
			wantError: true,
		},
		{
			// Go's regexp \S is ASCII-only, so a non-breaking space passed it,
			// then strings.TrimSpace emptied the model and the config failed
			// to load.
			name:      "suggestions reject a Unicode-whitespace-only model",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"provider":"openai","model":"\u00a0"}`),
			wantError: true,
		},
		{
			name:     "suggestions accept a first run with fractional seconds and an offset",
			analyzer: complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"schedule":{"start_at":"2028-02-29T23:59:59.5+05:30"}}`),
		},
		{
			name:      "suggestions reject a first run on an impossible date",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"schedule":{"start_at":"2026-99-99T03:00:00Z"}}`),
			wantError: true,
		},
		{
			name:      "suggestions reject a first run past the end of its month",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"schedule":{"start_at":"2026-04-31T03:00:00Z"}}`),
			wantError: true,
		},
		{
			name:      "suggestions reject a first run at an impossible time",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"schedule":{"start_at":"2026-11-06T24:00:00Z"}}`),
			wantError: true,
		},
		{
			name:      "suggestions reject a cluster budget below 1000",
			analyzer:  complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":{"enabled":true,"max_clustered_requests":10}`),
			wantError: true,
		},
		{
			name:     "semantic fallback none does not require llm config",
			analyzer: complexityAnalyzerSchemaConfig(`,"semantic":{"provider":"openai","embedding_model":"text-embedding-3-small","fallback":"none"}`),
		},
		{
			name:      "semantic fallback llm requires llm config",
			analyzer:  complexityAnalyzerSchemaConfig(`,"semantic":{"provider":"openai","embedding_model":"text-embedding-3-small","fallback":"llm"}`),
			wantError: true,
		},
		{
			name: "semantic fallback llm accepts llm config",
			analyzer: complexityAnalyzerSchemaConfig(
				`,"llm":{"provider":"openai","model":"gpt-4o-mini"}`,
				`,"semantic":{"provider":"openai","embedding_model":"text-embedding-3-small","fallback":"llm"}`,
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := fmt.Sprintf(`{"governance":{"complexity_analyzer_config":%s}}`, tt.analyzer)
			// Suggestions are a model router's; their rules are checked there.
			if strings.Contains(tt.analyzer, `"suggestions"`) {
				config = fmt.Sprintf(`{"model_routers":[%s]}`, modelRouterSchemaConfig(tt.analyzer))
			}
			err := validateConfig(t, compiled, config)
			if tt.wantError && err == nil {
				t.Fatal("expected schema validation to fail")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("expected schema validation to pass: %v", err)
			}
		})
	}
}

// TestSchemaSuggestionsModelMatchesGoTrimming pins the suggestions provider
// and model patterns to strings.TrimSpace: every rune Go trims away is
// rejected on its own, so a value the schema accepts is never emptied by
// normalization, while real content padded with those runes still passes.
func TestSchemaSuggestionsModelMatchesGoTrimming(t *testing.T) {
	compiled := compileSchema(t)
	validate := func(provider, model string) error {
		block, err := json.Marshal(map[string]any{"enabled": true, "provider": provider, "model": model})
		if err != nil {
			t.Fatal(err)
		}
		return validateConfig(t, compiled, fmt.Sprintf(`{"model_routers":[%s]}`,
			modelRouterSchemaConfig(complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":`+string(block)))))
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !unicode.IsSpace(r) {
			continue
		}
		if validate("openai", string(r)) == nil {
			t.Errorf("model %U alone is trimmed to nothing, yet the schema accepted it", r)
		}
		if validate(string(r), "gpt-5") == nil {
			t.Errorf("provider %U alone is trimmed to nothing, yet the schema accepted it", r)
		}
		if err := validate("openai", string(r)+"gpt-5"+string(r)); err != nil {
			t.Errorf("model with %U around it keeps its content, yet the schema refused it: %v", r, err)
		}
	}
}

// semanticSchemaBlock and suggestionsSchemaPrereqs satisfy what enabled
// suggestions require, so a suggestions case fails only on what it tests.
const (
	semanticSchemaBlock      = `,"semantic":{"provider":"openai","embedding_model":"text-embedding-3-small"}`
	suggestionsSchemaPrereqs = semanticSchemaBlock + `,"llm":{"provider":"openai","model":"gpt-4.1-mini"}`
)

func complexityAnalyzerSchemaConfig(fields ...string) string {
	return `{"keywords":{"simple_keywords":["simple"],"medium_keywords":["medium"],"complex_keywords":["complex"]}` + strings.Join(fields, "") + `}`
}

// modelRouterSchemaConfig turns a complexityAnalyzerSchemaConfig into a
// model_routers entry with the same settings.
func modelRouterSchemaConfig(analyzer string) string {
	return `{"id":"suggestions-router","name":"Suggestions router",` + strings.TrimPrefix(analyzer, "{")
}

// The complexity analyzer config makes no phrase suggestions: only model
// routers do, so config.json refuses a suggestions block on it, enabled or
// not, and takes the same block on a router.
func TestSchemaComplexityAnalyzerConfigRefusesSuggestions(t *testing.T) {
	compiled := compileSchema(t)
	for _, block := range []string{`{"enabled":true,"provider":"openai","model":"gpt-5"}`, `{"enabled":false}`} {
		analyzer := complexityAnalyzerSchemaConfig(suggestionsSchemaPrereqs, `,"suggestions":`+block)
		if err := validateConfig(t, compiled, fmt.Sprintf(`{"governance":{"complexity_analyzer_config":%s}}`, analyzer)); err == nil {
			t.Fatalf("the complexity config must refuse suggestions %s", block)
		}
		if err := validateConfig(t, compiled, fmt.Sprintf(`{"model_routers":[%s]}`, modelRouterSchemaConfig(analyzer))); err != nil {
			t.Fatalf("a model router must take suggestions %s: %v", block, err)
		}
	}
}

func TestSchemaKeyAliases(t *testing.T) {
	schema := loadSchema(t)

	t.Run("base_key $def includes aliases field", func(t *testing.T) {
		_, found := navigateJSON(schema, "$defs", "base_key", "properties", "aliases")
		if !found {
			t.Error("$defs/base_key is missing 'aliases' property — aliases replaced per-provider deployments maps")
		}
	})

	t.Run("vertex_key $def includes project_number field", func(t *testing.T) {
		_, found := navigateJSON(schema, "$defs", "vertex_key", "allOf", 1, "properties", "vertex_key_config", "properties", "project_number")
		if !found {
			t.Error("$defs/vertex_key is missing 'project_number' property — VertexKeyConfig Go struct defines this field")
		}
	})

	t.Run("vertex_key_config does not include deployments field", func(t *testing.T) {
		_, found := navigateJSON(schema, "$defs", "vertex_key", "allOf", 1, "properties", "vertex_key_config", "properties", "deployments")
		if found {
			t.Error("$defs/vertex_key still has 'deployments' in vertex_key_config — deployments were moved to top-level key aliases")
		}
	})

	t.Run("key with aliases validates successfully", func(t *testing.T) {
		compiled := compileSchema(t)
		config := `{
			"providers": {
				"vertex": {
					"keys": [{
						"name": "test",
						"value": "",
						"weight": 1,
						"models": ["gemini-2.0-flash"],
						"aliases": {"gemini-2.0-flash": "gemini-2.0-flash-001"},
						"vertex_key_config": {
							"project_id": "my-project",
							"region": "us-central1",
							"auth_credentials": "",
							"project_number": "123456"
						}
					}]
				}
			}
		}`
		if err := validateConfig(t, compiled, config); err != nil {
			t.Errorf("key with aliases should be valid, got: %v", err)
		}
	})

	t.Run("azure key with aliases validates successfully", func(t *testing.T) {
		compiled := compileSchema(t)
		config := `{
			"providers": {
				"azure": {
					"keys": [{
						"name": "test",
						"value": "my-api-key",
						"weight": 1,
						"models": ["gpt-4o"],
						"aliases": {"gpt-4o": "gpt-4o-deployment"},
						"azure_key_config": {
							"endpoint": "https://my-resource.openai.azure.com",
							"api_version": "2024-02-01"
						}
					}]
				}
			}
		}`
		if err := validateConfig(t, compiled, config); err != nil {
			t.Errorf("azure key with aliases should be valid, got: %v", err)
		}
	})
}

func TestSchemaGovernanceModelConfigs(t *testing.T) {
	schemaPath := getSchemaPath(t)
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("failed to read schema: %v", err)
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("failed to parse schema: %v", err)
	}

	t.Run("governance includes model_configs property", func(t *testing.T) {
		_, found := navigateJSON(schema, "properties", "governance", "properties", "model_configs")
		if !found {
			t.Error("governance is missing 'model_configs' property — GovernanceData struct and per-model rate limiting depend on it")
		}
	})

	t.Run("governance with model_configs validates successfully", func(t *testing.T) {
		compiled := compileSchema(t)
		config := `{
			"governance": {
				"rate_limits": [{"id": "rl-1", "token_max_limit": 1000000, "token_reset_duration": "1m"}],
				"model_configs": [{"id": "mc-1", "model_name": "gemini-2.0-flash", "provider": "vertex", "rate_limit_id": "rl-1"}]
			}
		}`
		if err := validateConfig(t, compiled, config); err != nil {
			t.Errorf("governance with model_configs should be valid, got: %v", err)
		}
	})
}

// loadSchema reads and parses config.schema.json into a generic map.
func loadSchema(t *testing.T) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(getSchemaPath(t))
	if err != nil {
		t.Fatalf("failed to read schema: %v", err)
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("failed to parse schema: %v", err)
	}
	return schema
}

func TestSchemaClientAllowedHeaders(t *testing.T) {
	schema := loadSchema(t)
	fields := []string{
		"allowed_headers",
	}
	for _, field := range fields {
		t.Run("client has "+field, func(t *testing.T) {
			_, found := navigateJSON(schema, "properties", "client", "properties", field)
			if !found {
				t.Errorf("client is missing '%s' property — ClientConfig Go struct defines this field", field)
			}
		})
	}

	t.Run("client allowed_headers validates successfully", func(t *testing.T) {
		compiled := compileSchema(t)
		config := `{
			"client": {
				"allowed_headers": ["X-Custom-Header"]
			}
		}`
		if err := validateConfig(t, compiled, config); err != nil {
			t.Errorf("client with allowed_headers should be valid, got: %v", err)
		}
	})
}

func TestSchemaProviderRawRequest(t *testing.T) {
	schema := loadSchema(t)

	for _, def := range []string{"provider", "provider_with_bedrock_config", "provider_with_vllm_config", "provider_with_azure_config", "provider_with_vertex_config"} {
		t.Run(def+" has send_back_raw_request", func(t *testing.T) {
			_, found := navigateJSON(schema, "$defs", def, "properties", "send_back_raw_request")
			if !found {
				t.Errorf("$defs/%s is missing 'send_back_raw_request' property — ProviderConfig Go struct defines this field", def)
			}
		})
		t.Run(def+" has custom_provider_config", func(t *testing.T) {
			_, found := navigateJSON(schema, "$defs", def, "properties", "custom_provider_config")
			if !found {
				t.Errorf("$defs/%s is missing 'custom_provider_config' property — ProviderConfig Go struct defines this field", def)
			}
		})
	}

	t.Run("provider with send_back_raw_request validates successfully", func(t *testing.T) {
		compiled := compileSchema(t)
		config := `{
			"providers": {
				"openai": {
					"keys": [{"name": "test", "value": "sk-test", "weight": 1, "models": ["gpt-4"]}],
					"send_back_raw_request": true,
					"send_back_raw_response": true
				}
			}
		}`
		if err := validateConfig(t, compiled, config); err != nil {
			t.Errorf("provider with send_back_raw_request should be valid, got: %v", err)
		}
	})

	t.Run("provider with custom_provider_config validates successfully", func(t *testing.T) {
		compiled := compileSchema(t)
		config := `{
			"providers": {
				"openai": {
					"keys": [{"name": "test", "value": "sk-test", "weight": 1, "models": ["gpt-4"]}],
					"custom_provider_config": {
						"base_provider_type": "openai",
						"is_key_less": false,
						"allowed_requests": {
							"chat_completion": true,
							"chat_completion_stream": true
						}
					}
				}
			}
		}`
		if err := validateConfig(t, compiled, config); err != nil {
			t.Errorf("provider with custom_provider_config should be valid, got: %v", err)
		}
	})
}

func TestSchemaGovernanceProviders(t *testing.T) {
	schema := loadSchema(t)

	t.Run("governance includes providers property", func(t *testing.T) {
		_, found := navigateJSON(schema, "properties", "governance", "properties", "providers")
		if !found {
			t.Error("governance is missing 'providers' property — GovernanceConfig Go struct defines this field")
		}
	})

	t.Run("governance with providers validates successfully", func(t *testing.T) {
		compiled := compileSchema(t)
		config := `{
			"governance": {
				"providers": [
					{"name": "openai", "budget_id": "b-1", "send_back_raw_request": true}
				]
			}
		}`
		if err := validateConfig(t, compiled, config); err != nil {
			t.Errorf("governance with providers should be valid, got: %v", err)
		}
	})
}

func TestSchemaVKRotationCooldownBounds(t *testing.T) {
	compiled := compileSchema(t)

	cooldownConfig := func(value string) string {
		return fmt.Sprintf(`{"client": {"vk_rotation_cooldown": %s}}`, value)
	}

	t.Run("valid duration string accepted", func(t *testing.T) {
		if err := validateConfig(t, compiled, cooldownConfig(`"5m"`)); err != nil {
			t.Errorf("duration string cooldown should be valid, got: %v", err)
		}
	})

	t.Run("zero accepted", func(t *testing.T) {
		if err := validateConfig(t, compiled, cooldownConfig(`0`)); err != nil {
			t.Errorf("zero cooldown should be valid, got: %v", err)
		}
	})

	t.Run("30 days in nanoseconds accepted", func(t *testing.T) {
		if err := validateConfig(t, compiled, cooldownConfig(`2592000000000000`)); err != nil {
			t.Errorf("30-day cooldown should be valid, got: %v", err)
		}
	})

	t.Run("negative integer rejected", func(t *testing.T) {
		if err := validateConfig(t, compiled, cooldownConfig(`-1`)); err == nil {
			t.Error("negative cooldown must be rejected by the schema")
		}
	})

	t.Run("integer above 30 days rejected", func(t *testing.T) {
		if err := validateConfig(t, compiled, cooldownConfig(`2592000000000001`)); err == nil {
			t.Error("cooldown above 30 days must be rejected by the schema")
		}
	})
}

func TestSchemaAllowedOriginsWildcard(t *testing.T) {
	schemaPath := getSchemaPath(t)
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("failed to read schema: %v", err)
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("failed to parse schema: %v", err)
	}

	t.Run("allowed_origins uses anyOf not oneOf", func(t *testing.T) {
		items, found := navigateJSON(schema, "properties", "client", "properties", "allowed_origins", "items")
		if !found {
			t.Fatal("could not find allowed_origins.items in schema")
		}
		itemsMap, ok := items.(map[string]interface{})
		if !ok {
			t.Fatal("allowed_origins.items is not an object")
		}
		if _, hasOneOf := itemsMap["oneOf"]; hasOneOf {
			t.Error("allowed_origins.items uses 'oneOf' — should use 'anyOf' because '*' matches both const and format:uri subschemas")
		}
		if _, hasAnyOf := itemsMap["anyOf"]; !hasAnyOf {
			t.Error("allowed_origins.items should use 'anyOf'")
		}
	})

	t.Run("allowed_origins wildcard validates successfully", func(t *testing.T) {
		compiled := compileSchema(t)
		config := `{
			"client": {
				"allowed_origins": ["*"]
			}
		}`
		if err := validateConfig(t, compiled, config); err != nil {
			t.Errorf("allowed_origins with '*' should be valid, got: %v", err)
		}
	})
}

func TestSchemaBedrockKeyConfigSTSFields(t *testing.T) {
	schema := loadSchema(t)

	stsFields := []string{"role_arn", "external_id", "session_name"}
	for _, field := range stsFields {
		t.Run("$defs/bedrock_key has "+field, func(t *testing.T) {
			_, found := navigateJSON(schema, "$defs", "bedrock_key", "allOf", 1, "properties", "bedrock_key_config", "properties", field)
			if !found {
				t.Errorf("$defs/bedrock_key bedrock_key_config is missing '%s' — BedrockKeyConfig Go struct defines this field for STS AssumeRole", field)
			}
		})
	}

	t.Run("$defs/bedrock_key has batch_s3_config", func(t *testing.T) {
		_, found := navigateJSON(schema, "$defs", "bedrock_key", "allOf", 1, "properties", "bedrock_key_config", "properties", "batch_s3_config")
		if !found {
			t.Error("$defs/bedrock_key bedrock_key_config is missing 'batch_s3_config' — BedrockKeyConfig Go struct defines this field for batch operations")
		}
	})

	t.Run("bedrock config with STS fields validates successfully", func(t *testing.T) {
		compiled := compileSchema(t)
		config := `{
			"providers": {
				"bedrock": {
					"keys": [
						{
							"name": "cross-account",
							"weight": 1,
							"models": ["us.anthropic.claude-sonnet-4-20250514-v1:0"],
							"bedrock_key_config": {
								"region": "us-west-2",
								"role_arn": "arn:aws:iam::123456789012:role/BedrockAccessRole",
								"session_name": "bifrost-cross-account",
								"external_id": "my-external-id"
							}
						}
					]
				}
			}
		}`
		if err := validateConfig(t, compiled, config); err != nil {
			t.Errorf("bedrock config with STS AssumeRole fields should be valid, got: %v", err)
		}
	})

	t.Run("bedrock config with batch_s3_config validates successfully", func(t *testing.T) {
		compiled := compileSchema(t)
		config := `{
			"providers": {
				"bedrock": {
					"keys": [
						{
							"name": "batch-key",
							"weight": 1,
							"models": ["us.anthropic.claude-sonnet-4-20250514-v1:0"],
							"bedrock_key_config": {
								"region": "us-east-1",
								"batch_s3_config": {
									"buckets": [
										{"bucket_name": "my-batch-bucket", "prefix": "bifrost/", "is_default": true},
										{"bucket_name": "my-secondary-bucket"}
									]
								}
							}
						}
					]
				}
			}
		}`
		if err := validateConfig(t, compiled, config); err != nil {
			t.Errorf("bedrock config with batch_s3_config should be valid, got: %v", err)
		}
	})

	t.Run("bedrock config with unknown fields is rejected", func(t *testing.T) {
		compiled := compileSchema(t)
		config := `{
			"providers": {
				"bedrock": {
					"keys": [
						{
							"name": "bad-key",
							"weight": 1,
							"models": ["us.anthropic.claude-sonnet-4-20250514-v1:0"],
							"bedrock_key_config": {
								"region": "us-east-1",
								"unknown_field": "should-fail"
							}
						}
					]
				}
			}
		}`
		if err := validateConfig(t, compiled, config); err == nil {
			t.Error("bedrock config with unknown fields should fail schema validation (additionalProperties: false)")
		}
	})
}

// TestSchemaLiveModelsSyncInterval pins the 0-or->=60 contract on
// framework.pricing.live_models_sync_interval. This schema is the source of
// truth users author against, so it has to reject the same values the config
// API rejects (ConfigHandler.updateConfig) and the file resolver silently
// clamps (ResolveFrameworkPricingConfig). A bare "minimum": 0 accepted 1-59
// and left the user with a config that validated and then behaved as 60.
func TestSchemaLiveModelsSyncInterval(t *testing.T) {
	compiled := compileSchema(t)

	tests := []struct {
		name      string
		value     string
		wantError bool
	}{
		{name: "zero disables the background refresh", value: "0"},
		{name: "the minimum enabled interval is accepted", value: "60"},
		{name: "the default is accepted", value: "3600"},
		{name: "below the minimum but above zero is rejected", value: "59", wantError: true},
		{name: "one second is rejected", value: "1", wantError: true},
		{name: "negative is rejected", value: "-1", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := `{"framework":{"pricing":{"live_models_sync_interval":` + tt.value + `}}}`
			err := validateConfig(t, compiled, config)
			if tt.wantError && err == nil {
				t.Errorf("live_models_sync_interval=%s should be rejected, got no error", tt.value)
			}
			if !tt.wantError && err != nil {
				t.Errorf("live_models_sync_interval=%s should be valid, got: %v", tt.value, err)
			}
		})
	}
}

// TestSchemaComplexitySemanticTimeout pins the timeout schema to what
// ComplexitySemanticConfig.UnmarshalJSON actually accepts. The two forms are one
// setting: the decoder takes a duration string or a number of milliseconds,
// rejects only negatives ("must be non-negative"), and normalized() then reads
// zero as "unset" and substitutes DefaultComplexitySemanticTimeout. A schema that
// accepted "0s" but rejected 0 flagged a config the gateway runs happily.
func TestSchemaComplexitySemanticTimeout(t *testing.T) {
	compiled := compileSchema(t)
	semantic := func(timeout string) string {
		return `{"governance": {"complexity_analyzer_config": {
			"keywords": {"simple_keywords": ["hi"], "medium_keywords": ["build"], "complex_keywords": ["design a system"]},
			"semantic": {"provider": "openai", "embedding_model": "text-embedding-3-small", "timeout": ` + timeout + `}
		}}}`
	}

	for name, timeout := range map[string]string{
		"zero as a number": "0",
		"zero as duration": `"0s"`,
		"number":           "1500",
		"fractional":       "1.5",
		"duration string":  `"1.5s"`,
	} {
		if err := validateConfig(t, compiled, semantic(timeout)); err != nil {
			t.Errorf("semantic timeout (%s) must be valid — the decoder accepts it, got: %v", name, err)
		}
	}

	for name, timeout := range map[string]string{
		"negative number":   "-1",
		"negative duration": `"-1s"`,
		"unitless string":   `"1500"`,
	} {
		if err := validateConfig(t, compiled, semantic(timeout)); err == nil {
			t.Errorf("semantic timeout (%s) must be rejected", name)
		}
	}
}

// TestSchemaComplexityLLMTimeout is the llm-block half of
// TestSchemaComplexitySemanticTimeout. Both blocks decode timeout the same way —
// a time.Duration with no empty-string sentinel, so absent, null, and zero all
// arrive as 0 and normalized() substitutes the default — which is why zero has to
// be accepted in both forms here too. Fields whose Go type is a string spell
// "unset" as "" and do reject an explicit zero; those carry a ^[1-9] pattern
// instead.
func TestSchemaComplexityLLMTimeout(t *testing.T) {
	compiled := compileSchema(t)
	llm := func(timeout string) string {
		return `{"governance": {"complexity_analyzer_config": {
			"keywords": {"simple_keywords": ["hi"], "medium_keywords": ["build"], "complex_keywords": ["design a system"]},
			"semantic": {"provider": "openai", "embedding_model": "text-embedding-3-small", "fallback": "llm"},
			"llm": {"provider": "openai", "model": "gpt-4.1-mini", "timeout": ` + timeout + `}
		}}}`
	}

	for name, timeout := range map[string]string{
		"zero as a number": "0",
		"zero as duration": `"0s"`,
		"number":           "4000",
		"fractional":       "2.5",
		"duration string":  `"2s"`,
	} {
		if err := validateConfig(t, compiled, llm(timeout)); err != nil {
			t.Errorf("llm timeout (%s) must be valid — the decoder accepts it and normalized() defaults zero, got: %v", name, err)
		}
	}

	for name, timeout := range map[string]string{
		"negative number":   "-1",
		"negative duration": `"-1s"`,
		"unitless string":   `"4000"`,
	} {
		if err := validateConfig(t, compiled, llm(timeout)); err == nil {
			t.Errorf("llm timeout (%s) must be rejected", name)
		}
	}
}

// TestSchemaComplexityLLMFallbackRequiresLLMBlock pins the schema to
// ComplexityAnalyzerConfig.Validate, which rejects a semantic block selecting the
// "llm" fallback with no llm block to run it. The conditional has to read the
// nested semantic.fallback and require both keys: an `if` that merely names a
// property passes vacuously when the property is absent, which would demand an
// llm block from every analyzer config that never mentioned a fallback.
func TestSchemaComplexityLLMFallbackRequiresLLMBlock(t *testing.T) {
	compiled := compileSchema(t)
	analyzer := func(body string) string {
		fields := `"keywords": {"simple_keywords": ["hi"], "medium_keywords": ["build"], "complex_keywords": ["design a system"]}`
		if body != "" {
			fields += ", " + body
		}
		return `{"governance": {"complexity_analyzer_config": {` + fields + `}}}`
	}
	const semanticBase = `"provider": "openai", "embedding_model": "text-embedding-3-small"`
	const llmBlock = `"llm": {"provider": "openai", "model": "gpt-4.1-mini"}`

	valid := map[string]string{
		"llm fallback with its classifier":   `"semantic": {` + semanticBase + `, "fallback": "llm"}, ` + llmBlock,
		"fallback none without an llm block": `"semantic": {` + semanticBase + `, "fallback": "none"}`,
		// Validate() lets an llm block sit dormant so toggling the fallback back on
		// does not lose it.
		"dormant llm block under fallback none": `"semantic": {` + semanticBase + `, "fallback": "none"}, ` + llmBlock,
		// The vacuous-truth guards: neither of these names a fallback, so the
		// conditional must not fire.
		"semantic block with no fallback key": `"semantic": {` + semanticBase + `}`,
		"no semantic block at all":            `"llm": {"provider": "openai", "model": "gpt-4.1-mini"}`,
		"keywords only":                       "",
	}
	for name, body := range valid {
		if err := validateConfig(t, compiled, analyzer(body)); err != nil {
			t.Errorf("%s must be valid — the loader accepts it, got: %v", name, err)
		}
	}

	missing := analyzer(`"semantic": {` + semanticBase + `, "fallback": "llm"}`)
	if err := validateConfig(t, compiled, missing); err == nil {
		t.Error("an llm fallback with no llm block must be rejected: Validate reports \"requires an llm config block\"")
	}
}

// TestSchemaBudgetQuarterStartMonth pins the schema to the value range the Go
// layer actually accepts. BudgetResetConfig.QuarterStartMonth is a plain int, so
// an omitted quarter_start_month and an explicit 0 are indistinguishable after
// unmarshal - validateBudget therefore has to treat 0 as "unset", and the schema
// has to let that value through or config.json rejects a value the runtime
// documents as valid and handles correctly.
func TestSchemaBudgetQuarterStartMonth(t *testing.T) {
	compiled := compileSchema(t)

	budgetConfig := func(resetConfig string) string {
		return `{
			"governance": {
				"budgets": [{
					"id": "b-1",
					"max_limit": 100,
					"reset_duration": "1Q",
					"reset_config": ` + resetConfig + `
				}]
			}
		}`
	}

	t.Run("explicit zero is accepted as the unset value", func(t *testing.T) {
		if err := validateConfig(t, compiled, budgetConfig(`{"quarter_start_month": 0}`)); err != nil {
			t.Errorf("quarter_start_month 0 is documented as January and accepted by validateBudget, so the schema must accept it, got: %v", err)
		}
	})

	t.Run("an omitted quarter start is accepted", func(t *testing.T) {
		if err := validateConfig(t, compiled, budgetConfig(`{}`)); err != nil {
			t.Errorf("an empty reset_config should be valid, got: %v", err)
		}
	})

	t.Run("the 1-12 range still validates", func(t *testing.T) {
		for _, month := range []int{1, 4, 7, 10, 12} {
			cfg := budgetConfig(fmt.Sprintf(`{"quarter_start_month": %d}`, month))
			if err := validateConfig(t, compiled, cfg); err != nil {
				t.Errorf("quarter_start_month %d should be valid, got: %v", month, err)
			}
		}
	})

	t.Run("out of range months are still rejected", func(t *testing.T) {
		for _, month := range []int{-1, 13} {
			cfg := budgetConfig(fmt.Sprintf(`{"quarter_start_month": %d}`, month))
			if err := validateConfig(t, compiled, cfg); err == nil {
				t.Errorf("quarter_start_month %d is outside 1-12 and must be rejected", month)
			}
		}
	})

	// $defs/budget_line is a second copy of the same shape, reached through
	// customers and provider configs. A budget written there
	// deserializes into the same Go struct, so the two copies have to agree on
	// which values they accept.
	t.Run("the budget_line copy accepts the same range", func(t *testing.T) {
		lineConfig := func(month string) string {
			return `{
				"governance": {
					"customers": [{
						"id": "c-1",
						"name": "Acme",
						"budgets": [{
							"id": "b-1",
							"max_limit": 100,
							"reset_duration": "1Q",
							"reset_config": {"quarter_start_month": ` + month + `}
						}]
					}]
				}
			}`
		}

		if err := validateConfig(t, compiled, lineConfig("0")); err != nil {
			t.Errorf("quarter_start_month 0 must be accepted here exactly as it is under governance.budgets, got: %v", err)
		}
		if err := validateConfig(t, compiled, lineConfig("4")); err != nil {
			t.Errorf("quarter_start_month 4 should be valid, got: %v", err)
		}
		if err := validateConfig(t, compiled, lineConfig("13")); err == nil {
			t.Error("quarter_start_month 13 is outside 1-12 and must be rejected")
		}
	})
}

// TestSchemaResetConfigRequiresQuarterlyDuration pins the quarterly-only rule the
// reset_config description already states. validateResetConfig rejects a quarter
// definition on any non-quarterly duration, so without this constraint a
// config.json passes schema validation and only fails later at reconciliation -
// the slowest possible place to learn the file is wrong.
func TestSchemaResetConfigRequiresQuarterlyDuration(t *testing.T) {
	compiled := compileSchema(t)

	// governance.budgets and $defs/budget_line are separate copies of the same
	// shape, so both need the constraint and both are checked here.
	scopes := map[string]func(string) string{
		"governance.budgets": func(budget string) string {
			return `{"governance": {"budgets": [` + budget + `]}}`
		},
		"$defs/budget_line via customers": func(budget string) string {
			return `{"governance": {"customers": [{"id": "c-1", "name": "Acme", "budgets": [` + budget + `]}]}}`
		},
	}

	for name, wrap := range scopes {
		t.Run(name, func(t *testing.T) {
			quarterly := `{"id": "b-1", "max_limit": 100, "reset_duration": "1Q", "reset_config": {"quarter_start_month": 4}}`
			if err := validateConfig(t, compiled, wrap(quarterly)); err != nil {
				t.Errorf("reset_config on a 1Q budget must stay valid, got: %v", err)
			}

			for _, duration := range []string{"1M", "1h", "1Y"} {
				cfg := wrap(`{"id": "b-1", "max_limit": 100, "reset_duration": "` + duration + `", "reset_config": {"quarter_start_month": 4}}`)
				if err := validateConfig(t, compiled, cfg); err == nil {
					t.Errorf("reset_config with reset_duration %q must be rejected: validateResetConfig refuses it at reconciliation", duration)
				}
			}

			// A quarter definition with no duration at all is the same mistake with
			// the evidence missing, so it has to be rejected too.
			orphan := wrap(`{"id": "b-1", "max_limit": 100, "reset_config": {"quarter_start_month": 4}}`)
			if err := validateConfig(t, compiled, orphan); err == nil {
				t.Error("reset_config with no reset_duration must be rejected")
			}

			// Budgets without a quarter definition are unaffected by the constraint.
			plain := wrap(`{"id": "b-1", "max_limit": 100, "reset_duration": "1M"}`)
			if err := validateConfig(t, compiled, plain); err != nil {
				t.Errorf("a budget with no reset_config must stay valid on any duration, got: %v", err)
			}
		})
	}
}

// TestSchemaVectorStoreTypes pins the schema's store list to what NewVectorStore
// can actually construct. The two drifted once already: chromem became a
// selectable store type in the framework while the schema still listed four,
// so a config the loader accepts read as invalid to every editor pointed at the
// published schema.
func TestSchemaVectorStoreTypes(t *testing.T) {
	// Each supported type and the $defs section that describes its config block.
	wantTypes := map[vectorstore.VectorStoreType]string{
		vectorstore.VectorStoreTypeWeaviate: "weaviate_config",
		vectorstore.VectorStoreTypeRedis:    "redis_config",
		vectorstore.VectorStoreTypeQdrant:   "qdrant_config",
		vectorstore.VectorStoreTypePinecone: "pinecone_config",
		vectorstore.VectorStoreTypeChromem:  "chromem_config",
	}

	schema := loadSchema(t)
	rawEnum, found := navigateJSON(schema, "properties", "vector_store", "properties", "type", "enum")
	if !found {
		t.Fatal("vector_store.type is missing its enum")
	}
	enum, ok := rawEnum.([]interface{})
	if !ok {
		t.Fatalf("vector_store.type enum is not a list, got %T", rawEnum)
	}
	listed := make(map[string]bool, len(enum))
	for _, entry := range enum {
		value, ok := entry.(string)
		if !ok {
			t.Fatalf("vector_store.type enum holds a non-string entry %v", entry)
		}
		listed[value] = true
	}

	for storeType, configDef := range wantTypes {
		t.Run(string(storeType), func(t *testing.T) {
			if !listed[string(storeType)] {
				t.Errorf("vector_store.type enum is missing %q — NewVectorStore constructs this type", storeType)
			}
			if _, found := navigateJSON(schema, "$defs", configDef); !found {
				t.Errorf("$defs is missing %q — %q has no config schema", configDef, storeType)
			}
		})
	}

	// The reverse direction: a type the schema offers but the loader rejects
	// sends operators down a path that fails at boot.
	for value := range listed {
		if _, ok := wantTypes[vectorstore.VectorStoreType(value)]; !ok {
			t.Errorf("vector_store.type enum offers %q, which NewVectorStore cannot construct", value)
		}
	}

	// The backend config block is only meaningful next to the type that owns it.
	// Accepting a mismatched pair means the schema validates a config the loader
	// will reject at boot, which is the failure mode a schema exists to prevent.
	t.Run("config block must match the declared type", func(t *testing.T) {
		compiled := compileSchema(t)

		valid := map[string]string{
			"weaviate": `{"vector_store": {"enabled": true, "type": "weaviate", "config": {"scheme": "http", "host": "localhost:8080"}}}`,
			"redis":    `{"vector_store": {"enabled": true, "type": "redis", "config": {"addr": "localhost:6379"}}}`,
			"qdrant":   `{"vector_store": {"enabled": true, "type": "qdrant", "config": {"host": "localhost"}}}`,
			"pinecone": `{"vector_store": {"enabled": true, "type": "pinecone", "config": {"api_key": "k", "index_host": "h"}}}`,
			"chromem":  `{"vector_store": {"enabled": true, "type": "chromem", "config": {"path": "/app/data/chromem"}}}`,
		}
		for name, config := range valid {
			if err := validateConfig(t, compiled, config); err != nil {
				t.Errorf("%s paired with its own config must be valid, got: %v", name, err)
			}
		}

		mismatched := map[string]string{
			// chromem_config requires nothing, so an empty object satisfies it. A
			// type-blind branch lets that stand in for every other backend and
			// waves through a redis store with no addr to dial.
			"redis with an empty config":    `{"vector_store": {"enabled": true, "type": "redis", "config": {}}}`,
			"redis with a chromem config":   `{"vector_store": {"enabled": true, "type": "redis", "config": {"path": "/app/data/chromem"}}}`,
			"weaviate with a redis config":  `{"vector_store": {"enabled": true, "type": "weaviate", "config": {"addr": "localhost:6379"}}}`,
			"qdrant with a pinecone config": `{"vector_store": {"enabled": true, "type": "qdrant", "config": {"api_key": "k", "index_host": "h"}}}`,
			"pinecone with a qdrant config": `{"vector_store": {"enabled": true, "type": "pinecone", "config": {"host": "localhost"}}}`,
			"chromem with a redis config":   `{"vector_store": {"enabled": true, "type": "chromem", "config": {"addr": "localhost:6379"}}}`,
			"weaviate with an empty config": `{"vector_store": {"enabled": true, "type": "weaviate", "config": {}}}`,
			"pinecone with an empty config": `{"vector_store": {"enabled": true, "type": "pinecone", "config": {}}}`,
		}
		for name, config := range mismatched {
			if err := validateConfig(t, compiled, config); err == nil {
				t.Errorf("%s must be rejected: the config block belongs to a different backend", name)
			}
		}
	})

	// NewVectorStore rejects a nil config for every service-backed type
	// ("<type> config is required"), because each needs an endpoint to dial.
	// Chromem is the sole exception: a nil config there means memory-only mode.
	t.Run("service-backed types require a config block", func(t *testing.T) {
		compiled := compileSchema(t)

		for _, storeType := range []string{"weaviate", "redis", "qdrant", "pinecone"} {
			omitted := `{"vector_store": {"enabled": true, "type": "` + storeType + `"}}`
			if err := validateConfig(t, compiled, omitted); err == nil {
				t.Errorf("%s with no config block must be rejected: it has no endpoint to dial", storeType)
			}
		}

		// The negative control lives in "chromem config validates" below, which
		// pins the memory-only form as valid.
	})

	t.Run("chromem config validates", func(t *testing.T) {
		compiled := compileSchema(t)
		persistent := `{"vector_store": {"enabled": true, "type": "chromem", "config": {"path": "/app/data/chromem", "compress": false}}}`
		if err := validateConfig(t, compiled, persistent); err != nil {
			t.Errorf("a persistent chromem store must be valid, got: %v", err)
		}
		// Path is optional: an empty config is chromem's memory-only mode.
		memoryOnly := `{"vector_store": {"enabled": true, "type": "chromem"}}`
		if err := validateConfig(t, compiled, memoryOnly); err != nil {
			t.Errorf("a memory-only chromem store must be valid, got: %v", err)
		}
		unknownField := `{"vector_store": {"enabled": true, "type": "chromem", "config": {"path": "/app/data/chromem", "collection": "x"}}}`
		if err := validateConfig(t, compiled, unknownField); err == nil {
			t.Error("an unknown chromem config field must be rejected: ChromemConfig defines only path and compress")
		}
		// compress only reaches chromem.NewPersistentDB, so enabling it without a
		// path silently does nothing — reject it rather than let an operator
		// believe their memory-only store is compressed.
		compressNoPath := `{"vector_store": {"enabled": true, "type": "chromem", "config": {"compress": true}}}`
		if err := validateConfig(t, compiled, compressNoPath); err == nil {
			t.Error("compress without path must be rejected: compression only applies to a persistent store")
		}
		compressEmptyPath := `{"vector_store": {"enabled": true, "type": "chromem", "config": {"path": "", "compress": true}}}`
		if err := validateConfig(t, compiled, compressEmptyPath); err == nil {
			t.Error("compress with an empty path must be rejected: an empty path is memory-only mode")
		}
		compressWithPath := `{"vector_store": {"enabled": true, "type": "chromem", "config": {"path": "/app/data/chromem", "compress": true}}}`
		if err := validateConfig(t, compiled, compressWithPath); err != nil {
			t.Errorf("compress alongside a path must be valid, got: %v", err)
		}
		// newChromemStore trims the path before deciding between the persistent
		// and memory-only db, so a blank path is memory-only however it is
		// spelled. Both compression settings must reject it: writing one reads
		// as a persistent store that silently never persists.
		blankPaths := map[string]string{
			"empty, no compress":          `{"vector_store": {"enabled": true, "type": "chromem", "config": {"path": ""}}}`,
			"whitespace, no compress":     `{"vector_store": {"enabled": true, "type": "chromem", "config": {"path": "   "}}}`,
			"empty, compress false":       `{"vector_store": {"enabled": true, "type": "chromem", "config": {"path": "", "compress": false}}}`,
			"whitespace, compress false":  `{"vector_store": {"enabled": true, "type": "chromem", "config": {"path": " \t ", "compress": false}}}`,
			"whitespace, compress true":   `{"vector_store": {"enabled": true, "type": "chromem", "config": {"path": "   ", "compress": true}}}`,
			"newline only, compress true": `{"vector_store": {"enabled": true, "type": "chromem", "config": {"path": "\n", "compress": true}}}`,
		}
		for name, config := range blankPaths {
			if err := validateConfig(t, compiled, config); err == nil {
				t.Errorf("a blank chromem path (%s) must be rejected: omit path for memory-only mode", name)
			}
		}
		// The rejection must not spill onto real paths under either setting.
		for name, config := range map[string]string{
			"compress false": `{"vector_store": {"enabled": true, "type": "chromem", "config": {"path": "/app/data/chromem", "compress": false}}}`,
			"compress true":  `{"vector_store": {"enabled": true, "type": "chromem", "config": {"path": "./relative path/chromem", "compress": true}}}`,
		} {
			if err := validateConfig(t, compiled, config); err != nil {
				t.Errorf("a non-blank chromem path (%s) must stay valid, got: %v", name, err)
			}
		}
	})
}

// TestSchemaOAuthKeyConfig pins that oauth_key_config is accepted on any key (the base key
// definition carries it) and that the schema enforces the per-grant required fields, so a
// half-configured block is refused by the schema rather than at boot.
func TestSchemaOAuthKeyConfig(t *testing.T) {
	compiled := compileSchema(t)

	customProvider := func(keyBody string) string {
		return fmt.Sprintf(`{
		"providers": {
			"openai": {
				"keys": [{%s}]
			},
			"acme": {
				"keys": [{%s}],
				"custom_provider_config": {"base_provider_type": "openai"},
				"network_config": {"base_url": "https://llm.acme.example/v1"}
			}
		}
	}`, keyBody, keyBody)
	}

	const cc = `"oauth_key_config": {
		"grant_type": "client_credentials",
		"token_url": "https://idp.acme.example/oauth2/token",
		"client_id": "env.ACME_CLIENT_ID",
		"client_secret": "env.ACME_CLIENT_SECRET",
		"scopes": ["inference"],
		"auth_style": "body"
	}`
	const jb = `"oauth_key_config": {
		"grant_type": "jwt_bearer",
		"token_url": "https://idp.acme.example/oauth2/token",
		"private_key": "env.ACME_PRIVATE_KEY",
		"issuer": "svc-bifrost",
		"audience": "https://idp.acme.example/oauth2/token",
		"signing_algorithm": "ES256",
		"assertion_lifetime_seconds": 120
	}`

	valid := []struct {
		name   string
		config string
	}{
		{"client_credentials", customProvider(`"name": "k", "models": ["*"], "weight": 1.0, ` + cc)},
		{"jwt_bearer", customProvider(`"name": "k", "models": ["*"], "weight": 1.0, ` + jb)},
		{"alongside a value", customProvider(`"name": "k", "value": "sk", "models": ["*"], "weight": 1.0, ` + cc)},
		// OAuthKeyConfig.Validate treats 0 as "use the default lifetime", so the schema must too.
		{"zero assertion lifetime means the default", customProvider(`"name": "k", "models": ["*"], "weight": 1.0, "oauth_key_config": {"grant_type": "jwt_bearer", "token_url": "https://idp/token", "private_key": "pem", "issuer": "i", "audience": "a", "assertion_lifetime_seconds": 0}`)},
	}
	for _, tt := range valid {
		t.Run("valid "+tt.name, func(t *testing.T) {
			if err := validateConfig(t, compiled, tt.config); err != nil {
				t.Fatalf("config should be valid: %v", err)
			}
		})
	}

	invalid := []struct {
		name   string
		config string
	}{
		{"client_credentials without secret", customProvider(`"name": "k", "models": ["*"], "weight": 1.0, "oauth_key_config": {"grant_type": "client_credentials", "token_url": "https://idp/token", "client_id": "id"}`)},
		{"jwt_bearer without issuer", customProvider(`"name": "k", "models": ["*"], "weight": 1.0, "oauth_key_config": {"grant_type": "jwt_bearer", "token_url": "https://idp/token", "private_key": "pem", "audience": "a"}`)},
		{"unknown grant", customProvider(`"name": "k", "models": ["*"], "weight": 1.0, "oauth_key_config": {"grant_type": "password", "token_url": "https://idp/token"}`)},
		{"symmetric algorithm", customProvider(`"name": "k", "models": ["*"], "weight": 1.0, "oauth_key_config": {"grant_type": "jwt_bearer", "token_url": "https://idp/token", "private_key": "pem", "issuer": "i", "audience": "a", "signing_algorithm": "HS256"}`)},
		{"unknown field", customProvider(`"name": "k", "models": ["*"], "weight": 1.0, "oauth_key_config": {"grant_type": "client_credentials", "token_url": "https://idp/token", "client_id": "id", "client_secret": "s", "refresh_token": "x"}`)},
		// Validate rejects a blank literal for every per-grant credential, so presence alone is
		// not enough: an empty string must fail the schema the same way a missing field does.
		{"blank client_id", customProvider(`"name": "k", "models": ["*"], "weight": 1.0, "oauth_key_config": {"grant_type": "client_credentials", "token_url": "https://idp/token", "client_id": "", "client_secret": "s"}`)},
		{"blank client_secret", customProvider(`"name": "k", "models": ["*"], "weight": 1.0, "oauth_key_config": {"grant_type": "client_credentials", "token_url": "https://idp/token", "client_id": "id", "client_secret": ""}`)},
		{"blank private_key", customProvider(`"name": "k", "models": ["*"], "weight": 1.0, "oauth_key_config": {"grant_type": "jwt_bearer", "token_url": "https://idp/token", "private_key": "", "issuer": "i", "audience": "a"}`)},
		{"blank issuer", customProvider(`"name": "k", "models": ["*"], "weight": 1.0, "oauth_key_config": {"grant_type": "jwt_bearer", "token_url": "https://idp/token", "private_key": "pem", "issuer": "", "audience": "a"}`)},
		{"blank audience", customProvider(`"name": "k", "models": ["*"], "weight": 1.0, "oauth_key_config": {"grant_type": "jwt_bearer", "token_url": "https://idp/token", "private_key": "pem", "issuer": "i", "audience": ""}`)},
		{"negative assertion lifetime", customProvider(`"name": "k", "models": ["*"], "weight": 1.0, "oauth_key_config": {"grant_type": "jwt_bearer", "token_url": "https://idp/token", "private_key": "pem", "issuer": "i", "audience": "a", "assertion_lifetime_seconds": -1}`)},
	}
	for _, tt := range invalid {
		t.Run("invalid "+tt.name, func(t *testing.T) {
			if err := validateConfig(t, compiled, tt.config); err == nil {
				t.Fatal("config should be invalid")
			}
		})
	}
}

// TestSchemaAnthropicKeyConfig pins that anthropic_key_config.workload_identity is accepted on
// anthropic and anthropic-based custom provider keys, and that the schema demands the same
// shape as AnthropicWorkloadIdentityConfig.Validate: the three IDs and exactly one token source.
func TestSchemaAnthropicKeyConfig(t *testing.T) {
	compiled := compileSchema(t)

	providers := func(keyBody string) string {
		return fmt.Sprintf(`{
		"providers": {
			"anthropic": {
				"keys": [{%s}]
			},
			"acme": {
				"keys": [{%s}],
				"custom_provider_config": {"base_provider_type": "anthropic"},
				"network_config": {"base_url": "https://llm.acme.example"}
			}
		}
	}`, keyBody, keyBody)
	}
	const key = `"name": "k", "models": ["*"], "weight": 1.0, `
	const ids = `"federation_rule_id": "fdrl_01abc", "organization_id": "env.ANTHROPIC_ORG_ID", "service_account_id": "svac_01def"`

	valid := []struct {
		name   string
		config string
	}{
		{"token file", providers(key + `"anthropic_key_config": {"workload_identity": {` + ids + `, "identity_source": {"type": "file", "path": "/var/run/secrets/anthropic/token"}}}`)},
		{"env token with workspace", providers(key + `"anthropic_key_config": {"workload_identity": {` + ids + `, "workspace_id": "wrkspc_01ghi", "identity_source": {"type": "inline", "token": "env.ANTHROPIC_IDENTITY_TOKEN"}}}`)},
		{"alongside a value", providers(key + `"value": "sk-ant", "anthropic_key_config": {"workload_identity": {` + ids + `, "identity_source": {"type": "file", "path": "/f"}}}`)},
		{"empty section", providers(key + `"value": "sk-ant", "anthropic_key_config": {}`)},
	}
	for _, tt := range valid {
		t.Run("valid "+tt.name, func(t *testing.T) {
			if err := validateConfig(t, compiled, tt.config); err != nil {
				t.Fatalf("config should be valid: %v", err)
			}
		})
	}

	invalid := []struct {
		name   string
		config string
	}{
		{"a file source carrying a token", providers(key + `"anthropic_key_config": {"workload_identity": {` + ids + `, "identity_source": {"type": "file", "path": "/f", "token": "t"}}}`)},
		{"an inline source carrying a path", providers(key + `"anthropic_key_config": {"workload_identity": {` + ids + `, "identity_source": {"type": "inline", "token": "t", "path": "/f"}}}`)},
		{"a file source without a path", providers(key + `"anthropic_key_config": {"workload_identity": {` + ids + `, "identity_source": {"type": "file"}}}`)},
		{"an unknown source type", providers(key + `"anthropic_key_config": {"workload_identity": {` + ids + `, "identity_source": {"type": "gcp_metadata"}}}`)},
		{"no identity source", providers(key + `"anthropic_key_config": {"workload_identity": {` + ids + `}}`)},
		{"the flat identity_token_file field", providers(key + `"anthropic_key_config": {"workload_identity": {` + ids + `, "identity_token_file": "/f"}}`)},
		{"missing service account", providers(key + `"anthropic_key_config": {"workload_identity": {"federation_rule_id": "fdrl_01abc", "organization_id": "org", "identity_source": {"type": "file", "path": "/f"}}}`)},
		{"blank federation rule", providers(key + `"anthropic_key_config": {"workload_identity": {"federation_rule_id": "", "organization_id": "org", "service_account_id": "svac_01def", "identity_source": {"type": "file", "path": "/f"}}}`)},
		{"unknown field", providers(key + `"anthropic_key_config": {"workload_identity": {` + ids + `, "identity_source": {"type": "file", "path": "/f"}, "token_path": "/g"}}`)},
		{"unknown section field", providers(key + `"anthropic_key_config": {"workload_identity_federation": {}}`)},
	}
	for _, tt := range invalid {
		t.Run("invalid "+tt.name, func(t *testing.T) {
			if err := validateConfig(t, compiled, tt.config); err == nil {
				t.Fatal("config should be invalid")
			}
		})
	}
}

// TestValidateConfigSchema_VirtualKeyMetadata checks that config.schema.json enforces the same
// metadata rules as the configstore validator, including the reserved keys.
func TestValidateConfigSchema_VirtualKeyMetadata(t *testing.T) {
	tests := []struct {
		name     string
		metadata string
		wantErr  bool
	}{
		{name: "string values", metadata: `{"cost_center": "cc-42", "owner.email": "a@example.com", "env-name": ""}`},
		{name: "empty object", metadata: `{}`},
		{name: "non-string value", metadata: `{"cost_center": 42}`, wantErr: true},
		{name: "key with space", metadata: `{"cost center": "cc-42"}`, wantErr: true},
		{name: "empty key", metadata: `{"": "cc-42"}`, wantErr: true},
		{name: "value too long", metadata: `{"k": "` + strings.Repeat("v", 513) + `"}`, wantErr: true},
		{name: "not an object", metadata: `["cc-42"]`, wantErr: true},
		{name: "reserved key", metadata: `{"isAsyncRequest": "true"}`, wantErr: true},
		{name: "load balancer prefix", metadata: `{"bifrost_alb_provider": "x"}`, wantErr: true},
		{name: "reserved name as a substring", metadata: `{"my_bifrost_alb_x": "v", "isAsyncRequestCount": "1"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := `{"governance": {"virtual_keys": [{"id": "vk-1", "name": "Test Virtual Key", "value": "vk_test_123456", "metadata": ` + tt.metadata + `}]}}`
			err := ValidateConfigSchema([]byte(config), loadLocalSchema(t))
			if tt.wantErr && err == nil {
				t.Errorf("expected metadata %s to fail validation", tt.metadata)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("expected metadata %s to pass validation, got error: %v", tt.metadata, err)
			}
		})
	}
}

// TestValidateConfigSchema_Passthrough pins passthrough on every provider shape, including a
// custom provider, and mirrors the rule checks PassthroughPolicy.Validate makes at write time.
func TestValidateConfigSchema_Passthrough(t *testing.T) {
	schema := loadLocalSchema(t)
	const passthrough = `"passthrough": {"allowed_routes": [{"methods": ["POST"], "path": "/v1/chat/completions"}, {"methods": ["GET", "DELETE"], "path": "/v1/files/*"}, {"path": "/v1beta/models/*:generateContent"}, {"path": "/v1/batches/**"}]}`
	keyConfigs := map[string]string{
		"azure":      `, "azure_key_config": {"endpoint": "https://example.openai.azure.com"}`,
		"vertex":     `, "vertex_key_config": {"project_id": "p", "region": "us-central1"}`,
		"ollama":     `, "ollama_key_config": {"url": "http://localhost:11434"}`,
		"sgl":        `, "sgl_key_config": {"url": "http://localhost:30000"}`,
		"vllm":       `, "vllm_key_config": {"url": "http://localhost:8000", "model_name": "m"}`,
		"databricks": `, "databricks_key_config": {"workspace_url": "https://dbc-1234abcd-5678.cloud.databricks.com"}`,
	}
	for _, provider := range []string{"openai", "anthropic", "bedrock", "bedrock_mantle", "azure", "vertex", "ollama", "sgl", "vllm", "replicate", "databricks", "deepseek", "fireworks"} {
		key := fmt.Sprintf(`{"name": "k", "value": "v", "weight": 1.0%s}`, keyConfigs[provider])
		base := fmt.Sprintf(`{"providers": {%q: {"keys": [%s]}}}`, provider, key)
		if err := ValidateConfigSchema([]byte(base), schema); err != nil {
			t.Errorf("%s: base config must pass before passthrough is checked: %v", provider, err)
			continue
		}
		with := fmt.Sprintf(`{"providers": {%q: {"keys": [%s], %s}}}`, provider, key, passthrough)
		if err := ValidateConfigSchema([]byte(with), schema); err != nil {
			t.Errorf("%s: passthrough rejected: %v", provider, err)
		}
	}
	custom := `{"providers": {"baseten-sidecar": {"keys": [{"name": "k", "value": "v", "weight": 1.0}], "custom_provider_config": {"base_provider_type": "openai"}, ` + passthrough + `}}}`
	if err := ValidateConfigSchema([]byte(custom), schema); err != nil {
		t.Errorf("custom provider: passthrough rejected: %v", err)
	}

	config := func(block string) string {
		return `{"providers": {"openai": {"keys": [{"name": "k", "value": "v", "weight": 1.0}], "passthrough": ` + block + `}}}`
	}
	for _, block := range []string{`{"allowed_routes": []}`, `{"allowed_routes": [{"path": "/**"}]}`, `{"allowed_routes": [{"path": "/"}]}`, `{"allowed_routes": [{"path": "/v1/models/*/predict"}]}`, `{"allowed_routes": [{"path": "/v1/.well-known/**"}]}`} {
		if err := ValidateConfigSchema([]byte(config(block)), schema); err != nil {
			t.Errorf("%s: passthrough rejected: %v", block, err)
		}
	}
	for name, block := range map[string]string{
		"missing allowed_routes":  `{}`,
		"unknown field":           `{"allowed_routes": [], "deny": []}`,
		"rule without a path set": `{"allowed_routes": [{"methods": ["GET"]}]}`,
		"lowercase method":        `{"allowed_routes": [{"methods": ["get"], "path": "/v1/files"}]}`,
		"relative path":           `{"allowed_routes": [{"path": "v1/files"}]}`,
		"query in path":           `{"allowed_routes": [{"path": "/v1/files?purpose=batch"}]}`,
		"encoded path":            `{"allowed_routes": [{"path": "/v1/%66iles"}]}`,
		"empty segment":           `{"allowed_routes": [{"path": "/v1//files"}]}`,
		"trailing slash":          `{"allowed_routes": [{"path": "/v1/files/"}]}`,
		"dot segment":             `{"allowed_routes": [{"path": "/v1/./files"}]}`,
		"dot-dot segment":         `{"allowed_routes": [{"path": "/v1/../admin"}]}`,
		"inner double star":       `{"allowed_routes": [{"path": "/v1/**/content"}]}`,
		"double star in segment":  `{"allowed_routes": [{"path": "/v1/files**"}]}`,
		"unknown rule field":      `{"allowed_routes": [{"path": "/v1/files", "method": "GET"}]}`,
		"null rule":               `{"allowed_routes": [null]}`,
	} {
		if err := ValidateConfigSchema([]byte(config(block)), schema); err == nil {
			t.Errorf("%s: expected passthrough to fail validation", name)
		}
	}
}

func TestValidateConfigSchema_DatasheetOverrideConstraints(t *testing.T) {
	schema := loadLocalSchema(t)
	for _, tc := range []struct {
		name    string
		fields  string
		wantErr bool
	}{
		{"provider patch", `"params_patch":{"provider":"anthropic"}`, true},
		{"null provider patch", `"params_patch":{"provider":null}`, true},
		{"provider patch with pricing", `"params_patch":{"provider":"anthropic"},"pricing_patch":{"input_cost_per_token":1}`, true},
		{"custom missing mode", `"is_custom":true`, true},
		{"custom empty mode", `"is_custom":true,"mode":""`, true},
		{"custom whitespace mode", `"is_custom":true,"mode":" \t\n"`, true},
		{"custom valid mode", `"is_custom":true,"mode":"chat"`, false},
		{"custom padded mode", `"is_custom":true,"mode":" chat "`, false},
		{"mode wide pricing", `"pricing_patch":{"context_length":64000}`, false},
		{"empty noncustom mode", `"mode":"","pricing_patch":{"input_cost_per_token":1}`, false},
		{"other parameters", `"params_patch":{"supports_mcp":true,"max_output_tokens":1024,"custom_parameter":null},"pricing_patch":{"input_cost_per_token":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := `{"datasheet_overrides":[{"provider":"openai","model":"m",` + tc.fields + `}]}`
			err := ValidateConfigSchema([]byte(config), schema)
			if (err != nil) != tc.wantErr {
				t.Errorf("validation error = %v, want error = %v", err, tc.wantErr)
			}
		})
	}

	// The loader trims provider and model and skips an entry whose identity
	// is then empty, so the schema must reject a whitespace-only value.
	for _, tc := range []struct {
		name     string
		identity string
		wantErr  bool
	}{
		{"empty provider", `"provider":"","model":"m"`, true},
		{"whitespace provider", `"provider":" \t\n","model":"m"`, true},
		{"empty model", `"provider":"openai","model":""`, true},
		{"whitespace model", `"provider":"openai","model":"  "`, true},
		{"padded identity", `"provider":" openai ","model":" m "`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := `{"datasheet_overrides":[{` + tc.identity + `,"pricing_patch":{"input_cost_per_token":1}}]}`
			err := ValidateConfigSchema([]byte(config), schema)
			if (err != nil) != tc.wantErr {
				t.Errorf("validation error = %v, want error = %v", err, tc.wantErr)
			}
		})
	}
}

// The schema's schedule patterns must accept exactly what the Go schedule
// parser accepts, or config.json passes schema validation and then fails to
// load (or the reverse).
func TestConfigSchemaSuggestionsScheduleMatchesParser(t *testing.T) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(loadLocalSchema(t)))
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("config.schema.json", doc); err != nil {
		t.Fatalf("add schema: %v", err)
	}
	schedule, err := c.Compile("config.schema.json#/$defs/complexity_suggestions_schedule")
	if err != nil {
		t.Fatalf("compile schedule schema: %v", err)
	}
	for _, unit := range []string{"h", "d", "w"} {
		for n := 1; n <= 40; n++ {
			every := fmt.Sprintf("%d%s", n, unit)
			goErr := (&configstore.ComplexitySuggestionsSchedule{Every: every}).Validate()
			schemaErr := schedule.Validate(map[string]any{"every": every})
			if (goErr == nil) != (schemaErr == nil) {
				t.Errorf("%s: parser error %v, schema error %v", every, goErr, schemaErr)
			}
		}
	}
	// start_at: the schema's pattern accepts exactly the RFC 3339 times the Go
	// decoder does.
	for _, start := range []string{"2026-11-12T03:00:00Z", "2026-11-12T08:30:00+05:30", "2026-11-12T03:00:00.5Z",
		"2026-11-12T03:00Z", "2026-11-12 03:00:00Z", "2026-11-12", "tomorrow"} {
		var decoded configstore.ComplexitySuggestionsSchedule
		goErr := json.Unmarshal([]byte(`{"start_at":"`+start+`"}`), &decoded)
		schemaErr := schedule.Validate(map[string]any{"start_at": start})
		if (goErr == nil) != (schemaErr == nil) {
			t.Errorf("start_at %s: decoder error %v, schema error %v", start, goErr, schemaErr)
		}
	}
}

// Config schema validation, used by the tests to check config files against config.schema.json.

const ConfigSchemaURLEnv = "BIFROST_SCHEMA_URL"

const schemaFetchTimeout = 10 * time.Second

func configuredConfigSchemaLocation() string {
	return strings.TrimSpace(os.Getenv(ConfigSchemaURLEnv))
}

// loadSchemaFromLocation reads schema bytes from an HTTP(S) URL, a file:// URL,
// or a plain filesystem path.
func loadSchemaFromLocation(location string) ([]byte, error) {
	if strings.HasPrefix(location, "http://") || strings.HasPrefix(location, "https://") {
		client := http.Client{Timeout: schemaFetchTimeout}
		resp, err := client.Get(location)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return nil, fmt.Errorf("unexpected HTTP status %d fetching schema", resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}
	return os.ReadFile(filePathFromSchemaLocation(location))
}

// filePathFromSchemaLocation converts a file:// URL to a filesystem path,
// honoring the optional localhost host (RFC 8089) and percent-encoding.
// Plain paths are returned unchanged.
func filePathFromSchemaLocation(location string) string {
	if !strings.HasPrefix(location, "file://") {
		return location
	}
	parsed, err := url.Parse(location)
	if err != nil {
		return strings.TrimPrefix(location, "file://")
	}
	if parsed.Opaque != "" {
		return parsed.Opaque
	}
	if parsed.Host != "" && parsed.Host != "localhost" {
		return parsed.Host + parsed.Path
	}
	return parsed.Path
}

func schemaLocationFromConfig(data []byte) string {
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return ""
	}
	schemaLocation, ok := config["$schema"].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(schemaLocation)
}

// localSchemaCandidates lists paths (relative to CWD) where config.schema.json may be found
// when running from a source checkout. Checked in order before falling back to the remote URL.
var localSchemaCandidates = []string{
	"config.schema.json",            // running from transports/
	"../config.schema.json",         // running from transports/bifrost-http/
	"../../config.schema.json",      // running from transports/bifrost-http/lib/
	"transports/config.schema.json", // running from repo root
}

// tryLoadLocalSchema attempts to read config.schema.json from known local paths.
// Returns nil if none are found.
func tryLoadLocalSchema() []byte {
	for _, p := range localSchemaCandidates {
		data, err := os.ReadFile(p)
		if err == nil {
			return data
		}
	}
	return nil
}

// ValidateConfigSchema validates config data against the JSON schema.
// Returns nil if valid, or a formatted error describing all validation failures.
// An optional schemaOverride can be provided to use a local schema instead of loading from the configured location.
// Schema resolution order: schemaOverride arg, BIFROST_SCHEMA_URL env, the config's
// own non-default $schema value, a local source-checkout copy, the default public URL.
func ValidateConfigSchema(data []byte, schemaOverride ...[]byte) error {
	var configSchemaJSONBytes []byte
	if len(schemaOverride) > 0 && len(schemaOverride[0]) > 0 {
		configSchemaJSONBytes = schemaOverride[0]
	} else {
		schemaLocation := configuredConfigSchemaLocation()
		if schemaLocation == "" {
			if fromConfig := schemaLocationFromConfig(data); fromConfig != "" && fromConfig != DefaultConfigSchemaURL {
				schemaLocation = fromConfig
			}
		}
		if schemaLocation == "" {
			if localSchema := tryLoadLocalSchema(); localSchema != nil {
				// Prefer the local schema file from the source checkout when available.
				// This avoids validating against a potentially stale remote schema.
				configSchemaJSONBytes = localSchema
			} else {
				schemaLocation = DefaultConfigSchemaURL
			}
		}
		if configSchemaJSONBytes == nil {
			var err error
			configSchemaJSONBytes, err = loadSchemaFromLocation(schemaLocation)
			if err != nil {
				return fmt.Errorf("failed to get config schema from %s: %w", schemaLocation, err)
			}
		}
	}
	// Parse the schema JSON
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(configSchemaJSONBytes))
	if err != nil {
		return fmt.Errorf("failed to parse config schema JSON: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("config.schema.json", schemaDoc); err != nil {
		return fmt.Errorf("failed to add config schema resource: %w", err)
	}
	// Compile the schema
	compiledSchema, err := c.Compile("config.schema.json")
	if err != nil {
		return fmt.Errorf("failed to compile config schema: %w", err)
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	err = compiledSchema.Validate(v)
	if err == nil {
		return nil
	}
	// Format validation errors for better readability
	return formatValidationError(err)
}

// formatValidationError converts jsonschema validation errors into user-friendly messages
func formatValidationError(err error) error {
	validationErr, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return err
	}

	// Use the GoString format which provides detailed hierarchical output
	return fmt.Errorf("schema validation failed:\n%s", validationErr.GoString())
}

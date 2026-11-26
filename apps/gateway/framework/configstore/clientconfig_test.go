package configstore

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/require"
)

// Telemetry values are hashed length-prefixed: concatenated, a drop attribute or header that
// contains the next field's text hashed like two shorter values, so reconciliation missed a
// real change.
func TestGenerateClientConfigHash_TelemetryValuesAreUnambiguous(t *testing.T) {
	hashOf := func(telemetry *tables.ClientGatewayTelemetryConfig) string {
		t.Helper()
		c := ClientConfig{EnableLogging: new(true), InitialPoolSize: 100, LogRetentionDays: 30, MaxRequestBodySizeMB: 50, EnforceVKObjectOwnership: true,
			OAuth2ServerConfig: &tables.OAuth2ServerConfig{Gateways: &tables.ClientGatewaysConfig{ClaudeCode: &tables.ClaudeCodeGatewayConfig{Enabled: true, Telemetry: telemetry}}}}
		h, err := c.GenerateClientConfigHash()
		require.NoError(t, err)
		return h
	}
	dest := func(headers map[string]string) []tables.ClientGatewayTelemetryDestination {
		d := tables.ClientGatewayTelemetryDestination{Endpoint: schemas.NewSecretVar("https://otel.example.com"), Headers: map[string]*schemas.SecretVar{}}
		for k, v := range headers {
			d.Headers[k] = schemas.NewSecretVar(v)
		}
		return []tables.ClientGatewayTelemetryDestination{d}
	}

	require.NotEqual(t,
		hashOf(&tables.ClientGatewayTelemetryConfig{Enabled: true, Destinations: dest(nil), DropAttributes: []string{"prompt", "completion"}}),
		hashOf(&tables.ClientGatewayTelemetryConfig{Enabled: true, Destinations: dest(nil), DropAttributes: []string{"promptgateways.claude_code.telemetry.drop:completion"}}),
		"two drop attributes must not hash like one that contains the separator")
	require.NotEqual(t,
		hashOf(&tables.ClientGatewayTelemetryConfig{Enabled: true, Destinations: dest(map[string]string{"a": "val:x", "b": "val:y"})}),
		hashOf(&tables.ClientGatewayTelemetryConfig{Enabled: true, Destinations: dest(map[string]string{"a": "val:xgateways.claude_code.telemetry.header:b=val:y"})}),
		"two headers must not hash like one whose value contains the next header")
}

// TestGenerateClientConfigHash_GatewayAfterRedirectURIsIsUnambiguous pins that a redirect URI
// ending in the gateway marker does not hash like that URI plus a real gateway block.
func TestGenerateClientConfigHash_GatewayAfterRedirectURIsIsUnambiguous(t *testing.T) {
	hashOf := func(uri string, gateways *tables.ClientGatewaysConfig) string {
		t.Helper()
		c := ClientConfig{EnableLogging: new(true), InitialPoolSize: 100, LogRetentionDays: 30, MaxRequestBodySizeMB: 50, EnforceVKObjectOwnership: true,
			OAuth2ServerConfig: &tables.OAuth2ServerConfig{AllowedRedirectURIs: []string{uri}, Gateways: gateways}}
		h, err := c.GenerateClientConfigHash()
		require.NoError(t, err)
		return h
	}
	require.NotEqual(t,
		hashOf("https://example.test/callback", &tables.ClientGatewaysConfig{ClaudeCode: &tables.ClaudeCodeGatewayConfig{Enabled: true}}),
		hashOf("https://example.test/callbackgateways.claude_code.enabled:true", nil))
}

// bifrost_url and bifrost_mcp_allow_writes are hashed so that where one ends is
// unambiguous: written as plain "prefix + value", a URL whose path ends in the
// next field's text hashed the same as a shorter URL with that field set. Both
// unset keeps the bytes configs had before the fields existed.
func TestGenerateClientConfigHash_BifrostURLAndAllowWritesAreUnambiguous(t *testing.T) {
	base := func() ClientConfig {
		return ClientConfig{EnableLogging: new(true), InitialPoolSize: 100, LogRetentionDays: 30, MaxRequestBodySizeMB: 50, EnforceVKObjectOwnership: true}
	}

	short := base()
	short.BifrostURL = schemas.NewSecretVar("https://gw.example.com/a")
	short.BifrostMCPAllowWrites = new(true)
	long := base()
	long.BifrostURL = schemas.NewSecretVar("https://gw.example.com/abifrostMCPAllowWrites:true")
	shortHash, err := short.GenerateClientConfigHash()
	require.NoError(t, err)
	longHash, err := long.GenerateClientConfigHash()
	require.NoError(t, err)
	require.NotEqual(t, shortHash, longHash, "a URL ending in the next field's text must not hash like a shorter URL with that field set")

	unset := base()
	unsetHash, err := unset.GenerateClientConfigHash()
	require.NoError(t, err)
	require.Equal(t, "66ee28323ac8b742b63ca063f251b2f585209a32980345cf26dd0b81298ddf4c", unsetHash, "both unset keeps the hash configs had before the fields existed")
}

// The entity access profile mode is part of what decides whether a changed config.json replaces the
// stored client config: intersect must change the hash, while unset and an explicit union must hash
// the same,
// so an upgrade that starts writing the default does not churn every deployment's hash.
func TestGenerateClientConfigHashCoversEntityAccessProfileMode(t *testing.T) {
	hash := func(mode tables.EntityAccessProfileMode) string {
		t.Helper()
		h, err := (&ClientConfig{EntityAccessProfileMode: mode}).GenerateClientConfigHash()
		require.NoError(t, err)
		return h
	}
	unset := hash("")
	require.Equal(t, unset, hash(tables.EntityAccessProfileModeUnion), "union is the default and hashes as unset")
	require.NotEqual(t, unset, hash(tables.EntityAccessProfileModeIntersect), "intersect is a change")
}

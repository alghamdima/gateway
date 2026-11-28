package gemini

import (
	"encoding/json"
	"testing"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	schemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConvertBifrostToolsToGemini_UnionTypeProperty verifies that tool parameters
// with JSON Schema union types are passed through unchanged in parametersJsonSchema.
func TestConvertBifrostToolsToGemini_UnionTypeProperty(t *testing.T) {
	toolJSON := `{
		"type": "function",
		"function": {
			"name": "run_with_timeout",
			"description": "Run something with a timeout",
			"parameters": {
				"type": "object",
				"properties": {
					"timeout_secs": {
						"type": ["integer", "null"],
						"description": "Timeout in seconds"
					},
					"command": {
						"type": "string",
						"description": "Command to run"
					}
				},
				"required": ["command"]
			}
		}
	}`

	var chatTool schemas.ChatTool
	require.NoError(t, json.Unmarshal([]byte(toolJSON), &chatTool))

	geminiTools, err := convertBifrostToolsToGemini([]schemas.ChatTool{chatTool})
	require.NoError(t, err)
	require.Len(t, geminiTools, 1)
	require.Len(t, geminiTools[0].FunctionDeclarations, 1)

	fd := geminiTools[0].FunctionDeclarations[0]
	require.NotNil(t, fd.ParametersJSONSchema)
	assert.Nil(t, fd.Parameters, "chat tools use parametersJsonSchema passthrough, not Gemini Schema")

	raw, err := json.Marshal(fd.ParametersJSONSchema)
	require.NoError(t, err)

	var paramsSchema map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &paramsSchema))

	properties, ok := paramsSchema["properties"].(map[string]interface{})
	require.True(t, ok, "parameters must have properties")

	timeoutProp, ok := properties["timeout_secs"].(map[string]interface{})
	require.True(t, ok, "timeout_secs property must be present")

	timeoutType, ok := timeoutProp["type"].([]interface{})
	require.True(t, ok, "timeout_secs type must be a JSON Schema union array")
	assert.Equal(t, "integer", timeoutType[0])
	assert.Equal(t, "null", timeoutType[1])
	assert.Equal(t, "Timeout in seconds", timeoutProp["description"])

	commandProp, ok := properties["command"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "string", commandProp["type"])
	assert.Equal(t, "Command to run", commandProp["description"])
}

// TestConvertBifrostToolsToGemini_WirePayload verifies that the final
// serialized JSON bytes sent to Gemini/Vertex are correct for union-typed
// tool parameters. The original bug manifested at the serialization level
// (empty "type" field rejected by Vertex), so struct-level checks alone
// are not sufficient.
func TestConvertBifrostToolsToGemini_WirePayload(t *testing.T) {
	tests := []struct {
		name         string
		propertyJSON string
		propertyName string
		wantContains []string // substrings that must appear in the wire JSON
		wantAbsent   []string // substrings that must NOT appear in the wire JSON
	}{
		{
			name:         "nullable union array is passed through unchanged",
			propertyJSON: `"timeout_secs":{"type":["integer","null"],"description":"Timeout"}`,
			propertyName: "timeout_secs",
			// parametersJsonSchema passthrough: array form is preserved as-is
			wantContains: []string{`"type":["integer","null"]`},
			wantAbsent:   []string{`"nullable"`, `"anyOf"`},
		},
		{
			name:         "plain string type passes through unchanged",
			propertyJSON: `"command":{"type":"string","description":"Command to run"}`,
			propertyName: "command",
			wantContains: []string{`"type":"string"`},
			wantAbsent:   []string{`"nullable"`, `"anyOf"`},
		},
		{
			name:         "multi-type union array is passed through unchanged",
			propertyJSON: `"value":{"type":["integer","string"]}`,
			propertyName: "value",
			wantContains: []string{`"type":["integer","string"]`},
			wantAbsent:   []string{`"anyOf"`, `"nullable"`},
		},
		{
			name:         "multi-type nullable union array is passed through unchanged",
			propertyJSON: `"value":{"type":["integer","string","null"]}`,
			propertyName: "value",
			wantContains: []string{`"type":["integer","string","null"]`},
			wantAbsent:   []string{`"anyOf"`, `"nullable"`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			toolJSON := `{"type":"function","function":{"name":"test_fn","parameters":{"type":"object","properties":{` +
				tc.propertyJSON + `}}}}`

			var chatTool schemas.ChatTool
			require.NoError(t, json.Unmarshal([]byte(toolJSON), &chatTool))

			geminiTools, err := convertBifrostToolsToGemini([]schemas.ChatTool{chatTool})
			require.NoError(t, err)
			require.Len(t, geminiTools, 1)

			// Serialize to the exact bytes that would be sent to Vertex
			wire, err := providerUtils.MarshalSorted(geminiTools[0])
			require.NoError(t, err)
			wireStr := string(wire)

			for _, want := range tc.wantContains {
				assert.Contains(t, wireStr, want, "wire JSON must contain %q", want)
			}
			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, wireStr, absent, "wire JSON must not contain %q", absent)
			}
		})
	}
}

func TestConvertBifrostToolsToGemini_WirePayloadPreservesDefs(t *testing.T) {
	toolJSON := `{"type":"function","function":{"name":"suggest_time","parameters":{"type":"object","properties":{"attendeeEmails":{"type":"array","items":{"type":"string"},"description":"The attendee emails to find free time for."},"preferences":{"$ref":"#/$defs/Preferences"}},"required":["attendeeEmails"],"$defs":{"Preferences":{"type":"object","properties":{"startHour":{"type":"string"}}}}}}}`

	var chatTool schemas.ChatTool
	require.NoError(t, json.Unmarshal([]byte(toolJSON), &chatTool))

	geminiTools, err := convertBifrostToolsToGemini([]schemas.ChatTool{chatTool})
	require.NoError(t, err)
	require.Len(t, geminiTools, 1)

	wire, err := providerUtils.MarshalSorted(geminiTools[0])
	require.NoError(t, err)
	wireStr := string(wire)

	assert.Contains(t, wireStr, `"$defs"`)
	assert.Contains(t, wireStr, `"Preferences"`)
	assert.Contains(t, wireStr, `"$ref":"#/$defs/Preferences"`)
}

package logstore

import (
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractPayload_RoundTrip(t *testing.T) {
	metadata := `{"cortex-user-id":"user-123"}`
	log := &Log{
		ID:                      "test-1",
		InputHistory:            `[{"role":"user","content":"hello"}]`,
		ResponsesInputHistory:   `[{"role":"user","content":"hi"}]`,
		OutputMessage:           `{"role":"assistant","content":"world"}`,
		ResponsesOutput:         `[{"role":"assistant","content":"there"}]`,
		EmbeddingInput:          `[{"content":[{"type":"text","text":"embed me"}]}]`,
		EmbeddingOutput:         `[{"embedding":[0.1]}]`,
		RerankOutput:            `[{"score":0.9}]`,
		Params:                  `{"temperature":0.7}`,
		Tools:                   `[{"name":"tool1"}]`,
		ToolCalls:               `[{"id":"tc1"}]`,
		SpeechInput:             `{"input":"text"}`,
		TranscriptionInput:      `{"file":"test.mp3"}`,
		ImageGenerationInput:    `{"prompt":"cat"}`,
		ImageEditInput:          `{"prompt":"edit cat"}`,
		ImageVariationInput:     `{"image":"base64img"}`,
		VideoGenerationInput:    `{"prompt":"dog"}`,
		SpeechOutput:            `{"audio":"base64"}`,
		TranscriptionOutput:     `{"text":"hello"}`,
		ImageGenerationOutput:   `{"url":"http://img"}`,
		ListModelsOutput:        `[{"id":"model1"}]`,
		VideoGenerationOutput:   `{"id":"vid1"}`,
		VideoRetrieveOutput:     `{"status":"ready"}`,
		VideoDownloadOutput:     `{"url":"http://vid"}`,
		VideoListOutput:         `{"videos":[]}`,
		VideoDeleteOutput:       `{"deleted":true}`,
		CacheDebug:              `{"hit":true}`,
		GuardrailDebug:          `{"judge_calls":[{"total_tokens":18}]}`,
		TokenUsage:              `{"total_tokens":100}`,
		ErrorDetails:            `{"error":"bad"}`,
		RawRequest:              `{"method":"POST"}`,
		RawResponse:             `{"status":200}`,
		PassthroughRequestBody:  `body-req`,
		PassthroughResponseBody: `body-resp`,
		RoutingEngineLogs:       `routing log`,
		Metadata:                &metadata,
		PluginLogs:              `{"guardrails":[{"message":"pii detected"}]}`,
	}

	payload := ExtractPayload(log)
	// +1 for metadata, +6 for the always-present index fields (provider, model,
	// status, timestamp, selected_key_id, selected_key_name) added in #6070.
	assert.Equal(t, len(payloadFields)+1+6, len(payload), "payload map should have all payload fields plus metadata and index fields")
	assert.Equal(t, `[{"role":"user","content":"hello"}]`, payload["input_history"])
	assert.Equal(t, `{"role":"assistant","content":"world"}`, payload["output_message"])
	assert.Equal(t, `[{"content":[{"type":"text","text":"embed me"}]}]`, payload["embedding_input"])
	assert.Equal(t, `{"judge_calls":[{"total_tokens":18}]}`, payload["guardrail_debug"])
	assert.Equal(t, `routing log`, payload["routing_engine_logs"])
	assert.Equal(t, metadata, payload["metadata"], "metadata must be written to the snapshot for object consumers")
	assert.Equal(t, `{"guardrails":[{"message":"pii detected"}]}`, payload["plugin_logs"])

	// Clear and verify.
	ClearPayload(log)
	assert.Empty(t, log.InputHistory)
	assert.Empty(t, log.OutputMessage)
	assert.Empty(t, log.EmbeddingInput)
	assert.Nil(t, log.EmbeddingInputParsed)
	assert.Empty(t, log.RawRequest)
	assert.Empty(t, log.GuardrailDebug)
	assert.Empty(t, log.RoutingEngineLogs)
	require.NotNil(t, log.Metadata)
	assert.Equal(t, metadata, *log.Metadata)
	assert.Empty(t, log.PluginLogs)

	// Marshal and merge back.
	data, err := sonic.Marshal(payload)
	require.NoError(t, err)

	// Metadata is DB-authoritative: the snapshot carries a backup copy, but
	// MergePayloadFromJSON must NOT override the live DB value with it. Simulate
	// a DB row whose metadata was updated after the snapshot was written, then
	// confirm the merge leaves it untouched.
	dbMetadata := `{"cortex-user-id":"user-456"}`
	log.Metadata = &dbMetadata
	log.MetadataParsed = nil
	err = MergePayloadFromJSON(log, data)
	require.NoError(t, err)
	assert.Equal(t, `[{"role":"user","content":"hello"}]`, log.InputHistory)
	assert.Equal(t, `{"role":"assistant","content":"world"}`, log.OutputMessage)
	assert.Equal(t, `[{"content":[{"type":"text","text":"embed me"}]}]`, log.EmbeddingInput)
	assert.Equal(t, `{"judge_calls":[{"total_tokens":18}]}`, log.GuardrailDebug)
	assert.Equal(t, `routing log`, log.RoutingEngineLogs)
	assert.Equal(t, `{"guardrails":[{"message":"pii detected"}]}`, log.PluginLogs)
	require.NotNil(t, log.Metadata)
	assert.Equal(t, dbMetadata, *log.Metadata, "merge must not override DB-authoritative metadata with the snapshot")
	assert.Equal(t, "user-456", log.MetadataParsed["cortex-user-id"])
}

func TestExtractPayload_NilOrEmptyMetadataOmittedFromSnapshot(t *testing.T) {
	payload := ExtractPayload(&Log{ID: "x"})
	assert.NotContains(t, payload, "metadata", "nil Metadata must not appear in snapshot")

	empty := ""
	payload = ExtractPayload(&Log{ID: "y", Metadata: &empty})
	assert.NotContains(t, payload, "metadata", "empty Metadata must not appear in snapshot")
}

// TestExtractPayload_CarriesCallerIdentity pins that the object-store snapshot carries user_email and client_ip.
func TestExtractPayload_CarriesCallerIdentity(t *testing.T) {
	email, ip := "ada@example.com", "10.0.0.5"
	payload := ExtractPayload(&Log{ID: "who", UserEmail: &email, ClientIP: &ip})
	assert.Equal(t, email, payload["user_email"])
	assert.Equal(t, ip, payload["client_ip"])
	assert.NotContains(t, payload, "forwarded_for")

	bare := ExtractPayload(&Log{ID: "anon"})
	for _, key := range []string{"user_email", "client_ip"} {
		assert.NotContains(t, bare, key, "absent values stay out of the snapshot")
	}
	assert.True(t, isPayloadEmpty(ExtractPayload(&Log{ID: "identity-only", UserEmail: &email, ClientIP: &ip})), "identity alone does not trigger an upload")
}

func TestClearPayload_DoesNotTouchIndexFields(t *testing.T) {
	log := &Log{
		ID:           "test-1",
		Provider:     "anthropic",
		Model:        "claude-3",
		Status:       "success",
		InputHistory: `[{"role":"user","content":"hello"}]`,
	}
	ClearPayload(log)
	assert.Equal(t, "test-1", log.ID)
	assert.Equal(t, "anthropic", log.Provider)
	assert.Equal(t, "claude-3", log.Model)
	assert.Equal(t, "success", log.Status)
	assert.Empty(t, log.InputHistory)
}

func TestBuildInputContentSummary(t *testing.T) {
	content := "What is the weather?"
	log := &Log{
		InputHistoryParsed: []schemas.ChatMessage{
			{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &content}},
		},
		OutputMessageParsed: &schemas.ChatMessage{
			Content: &schemas.ChatMessageContent{ContentStr: strPtr("It's sunny")},
		},
	}

	summary := log.BuildInputContentSummary()
	assert.Contains(t, summary, "What is the weather?")
	assert.NotContains(t, summary, "It's sunny", "BuildInputContentSummary should not include output")
}

func TestBuildInputContentSummary_ToolLoopFallback(t *testing.T) {
	system := "You are an agent."
	toolResult := "25\nexit=0"
	toolName := "AskUserQuestion"
	callID := "call_1"
	fcType := schemas.ResponsesMessageTypeFunctionCall
	fcoType := schemas.ResponsesMessageTypeFunctionCallOutput
	ctcType := schemas.ResponsesMessageTypeCustomToolCall
	ctcoType := schemas.ResponsesMessageTypeCustomToolCallOutput
	args := `{"q":"amend?"}`
	output := "done"

	cases := []struct {
		name string
		log  *Log
		want string
	}{
		{
			name: "chat tool result",
			log: &Log{InputHistoryParsed: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: &system}},
				{Role: schemas.ChatMessageRoleTool, Content: &schemas.ChatMessageContent{ContentStr: &toolResult}, ChatToolMessage: &schemas.ChatToolMessage{ToolCallID: &callID}},
			}},
			want: toolResult,
		},
		{
			name: "chat assistant tool call",
			log: &Log{InputHistoryParsed: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: &system}},
				{Role: schemas.ChatMessageRoleAssistant, ChatAssistantMessage: &schemas.ChatAssistantMessage{ToolCalls: []schemas.ChatAssistantMessageToolCall{
					{Function: schemas.ChatAssistantMessageToolCallFunction{Name: &toolName, Arguments: args}},
				}}},
			}},
			want: toolName + "(" + args + ")",
		},
		{
			name: "responses function call",
			log: &Log{ResponsesInputHistoryParsed: []schemas.ResponsesMessage{
				{Type: &fcType, ResponsesToolMessage: &schemas.ResponsesToolMessage{CallID: &callID, Name: &toolName, Arguments: &args}},
			}},
			want: toolName + "(" + args + ")",
		},
		{
			name: "responses function call output",
			log: &Log{ResponsesInputHistoryParsed: []schemas.ResponsesMessage{
				{Type: &fcType, ResponsesToolMessage: &schemas.ResponsesToolMessage{CallID: &callID, Name: &toolName, Arguments: &args}},
				{Type: &fcoType, ResponsesToolMessage: &schemas.ResponsesToolMessage{CallID: &callID, Output: &schemas.ResponsesToolMessageOutputStruct{ResponsesToolCallOutputStr: &output}}},
			}},
			want: output,
		},
		{
			name: "chat assistant custom call",
			log: &Log{InputHistoryParsed: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: &system}},
				{Role: schemas.ChatMessageRoleAssistant, ChatAssistantMessage: &schemas.ChatAssistantMessage{ToolCalls: []schemas.ChatAssistantMessageToolCall{
					{Type: schemas.Ptr("custom"), Custom: &schemas.ChatAssistantMessageToolCallCustom{Name: schemas.Ptr("code_exec"), Input: "print(1)"}},
				}}},
			}},
			want: "code_exec(print(1))",
		},
		{
			name: "responses custom tool call",
			log: &Log{ResponsesInputHistoryParsed: []schemas.ResponsesMessage{
				{Type: &ctcType, ResponsesToolMessage: &schemas.ResponsesToolMessage{CallID: &callID, Name: schemas.Ptr("code_exec"), ResponsesCustomToolCall: &schemas.ResponsesCustomToolCall{Input: "print(1)"}}},
			}},
			want: "code_exec(print(1))",
		},
		{
			name: "responses custom tool call output",
			log: &Log{ResponsesInputHistoryParsed: []schemas.ResponsesMessage{
				{Type: &ctcType, ResponsesToolMessage: &schemas.ResponsesToolMessage{CallID: &callID, Name: schemas.Ptr("code_exec"), ResponsesCustomToolCall: &schemas.ResponsesCustomToolCall{Input: "print(1)"}}},
				{Type: &ctcoType, ResponsesToolMessage: &schemas.ResponsesToolMessage{CallID: &callID, Output: &schemas.ResponsesToolMessageOutputStruct{ResponsesToolCallOutputStr: &output}}},
			}},
			want: output,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.log.BuildInputContentSummary())
		})
	}
}

func TestBuildTags(t *testing.T) {
	vkID := "vk_123"
	rrID := "rr_456"
	log := &Log{
		Provider:      "anthropic",
		Model:         "claude-3-sonnet",
		Status:        "success",
		Object:        "chat.completion",
		VirtualKeyID:  &vkID,
		SelectedKeyID: "sk_789",
		RoutingRuleID: &rrID,
		Stream:        true,
		Timestamp:     time.Date(2026, 4, 3, 14, 0, 0, 0, time.UTC),
	}

	tags := BuildTags(log)
	assert.Equal(t, "anthropic", tags["provider"])
	assert.Equal(t, "claude-3-sonnet", tags["model"])
	assert.Equal(t, "success", tags["status"])
	assert.Equal(t, "chat.completion", tags["object_type"])
	assert.Equal(t, "vk_123", tags["virtual_key_id"])
	assert.Equal(t, "sk_789", tags["selected_key_id"])
	assert.Equal(t, "rr_456", tags["routing_rule_id"])
	assert.Equal(t, "true", tags["stream"])
	assert.Equal(t, "false", tags["has_error"])
	assert.Equal(t, "2026-04-03", tags["date"])
	assert.LessOrEqual(t, len(tags), 10, "S3 allows max 10 tags")
}

func TestBuildTags_ErrorStatus(t *testing.T) {
	log := &Log{Status: "error", Timestamp: time.Now()}
	tags := BuildTags(log)
	assert.Equal(t, "true", tags["has_error"])
}

func TestObjectKey(t *testing.T) {
	ts := time.Date(2026, 4, 3, 14, 0, 0, 0, time.UTC)
	key := ObjectKey("bifrost", ts, "req_abc123")
	assert.Equal(t, "bifrost/logs/2026/04/03/14/req_abc123.json.gz", key)
}

func TestHybridObjectKeysFollowFormat(t *testing.T) {
	ts := time.Date(2026, 4, 3, 14, 0, 0, 0, time.UTC)
	h := &HybridLogStore{prefix: "bifrost"}

	h.setObjectFormat(objectstore.FormatJSON, objectstore.CompressionGzip)
	jsonKey := ObjectKey("bifrost", ts, "req_1")
	parquetKey := "bifrost/logs/2026/04/03/14/req_1.parquet"
	assert.Equal(t, jsonKey, h.logKey(ts, "req_1"), "json keeps the existing keys")
	assert.Equal(t, parquetKey, h.alternateKey(jsonKey), "json keys fall back to parquet after a switch back")
	assert.Equal(t, []string{jsonKey, parquetKey}, h.withAlternateKeys(jsonKey))

	h.setObjectFormat(objectstore.FormatParquet, objectstore.CompressionSnappy)
	assert.Equal(t, parquetKey, h.logKey(ts, "req_1"))
	assert.Equal(t, jsonKey, h.alternateKey(parquetKey))
	assert.Equal(t, []string{parquetKey, jsonKey}, h.withAlternateKeys(parquetKey))
	assert.Empty(t, h.alternateKey("bifrost/skills/abc"), "keys outside the payload formats have no alternate")

	gcs := &HybridLogStore{prefix: "bifrost", singleFormat: true}
	gcs.setObjectFormat(objectstore.FormatJSON, objectstore.CompressionGzip)
	assert.Empty(t, gcs.alternateKey(jsonKey), "a json-only backend never probes parquet keys")
	assert.Equal(t, []string{jsonKey}, gcs.withAlternateKeys(jsonKey))

	h.setObjectFormat(objectstore.FormatParquet, objectstore.CompressionSnappy)
	assert.Equal(t, objectstore.CompressionSnappy, h.parquetCompression)
}

func strPtr(s string) *string {
	return &s
}

// TestIsPayloadEmpty_PluginLogsOnly verifies a log whose only payload content is
// plugin logs still uploads, so object-store consumers receive its plugin logs.
func TestIsPayloadEmpty_PluginLogsOnly(t *testing.T) {
	assert.True(t, isPayloadEmpty(ExtractPayload(&Log{ID: "empty"})))
	assert.False(t, isPayloadEmpty(ExtractPayload(&Log{ID: "plugin-only", PluginLogs: `{"guardrails":[{"message":"pii detected"}]}`})))
}

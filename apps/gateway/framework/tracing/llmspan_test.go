package tracing

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func TestPopulateErrorAttributesEmitsBilledUsage(t *testing.T) {
	msg := "stream cancelled by client"
	bifrostErr := &schemas.BifrostError{
		Error: &schemas.ErrorField{Message: msg},
	}
	bifrostErr.ExtraFields.RequestType = schemas.ChatCompletionStreamRequest
	bifrostErr.ExtraFields.BilledUsage = &schemas.BifrostLLMUsage{
		PromptTokens:     1200,
		CompletionTokens: 34,
		TotalTokens:      1234,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  1000,
			CachedWriteTokens: 200,
			CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens5m: 120,
				CachedWriteTokens1h: 80,
			},
		},
	}

	attrs := PopulateErrorAttributes(bifrostErr)

	for key, want := range map[string]any{
		schemas.AttrInputTokens:                     1200,
		schemas.AttrOutputTokens:                    34,
		schemas.AttrTotalTokens:                     1234,
		schemas.AttrUsageCacheReadInputTokens:       1000,
		schemas.AttrUsageCacheCreationInputTokens:   200,
		schemas.AttrPromptTokenDetailsCachedWrite5m: 120,
		schemas.AttrPromptTokenDetailsCachedWrite1h: 80,
	} {
		if got := attrs[key]; got != want {
			t.Errorf("attribute %s = %v, want %v", key, got, want)
		}
	}
	// A failed chat span must not carry the Responses namespace: the otel
	// plugin treats the two 5m/1h families as mutually exclusive per request.
	for _, key := range []string{
		schemas.AttrInputTokenDetailsCachedWrite5m,
		schemas.AttrInputTokenDetailsCachedWrite1h,
	} {
		if _, ok := attrs[key]; ok {
			t.Errorf("Responses-namespace attribute %s present on a chat span", key)
		}
	}
}

func TestPopulateErrorAttributesUsesResponsesNamespace(t *testing.T) {
	bifrostErr := &schemas.BifrostError{Error: &schemas.ErrorField{Message: "responses stream cancelled"}}
	bifrostErr.ExtraFields.RequestType = schemas.ResponsesStreamRequest
	bifrostErr.ExtraFields.BilledUsage = &schemas.BifrostLLMUsage{
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens5m: 120,
				CachedWriteTokens1h: 80,
			},
		},
	}

	attrs := PopulateErrorAttributes(bifrostErr)

	for key, want := range map[string]any{
		schemas.AttrInputTokenDetailsCachedWrite5m: 120,
		schemas.AttrInputTokenDetailsCachedWrite1h: 80,
	} {
		if got := attrs[key]; got != want {
			t.Errorf("attribute %s = %v, want %v", key, got, want)
		}
	}
	for _, key := range []string{
		schemas.AttrPromptTokenDetailsCachedWrite5m,
		schemas.AttrPromptTokenDetailsCachedWrite1h,
	} {
		if _, ok := attrs[key]; ok {
			t.Errorf("chat-namespace attribute %s present on a Responses span", key)
		}
	}
}

func TestPopulateErrorAttributesWithoutBilledUsageEmitsNoTokens(t *testing.T) {
	msg := "401 before the model ran"
	bifrostErr := &schemas.BifrostError{Error: &schemas.ErrorField{Message: msg}}

	attrs := PopulateErrorAttributes(bifrostErr)

	for _, key := range []string{schemas.AttrInputTokens, schemas.AttrOutputTokens, schemas.AttrTotalTokens} {
		if _, ok := attrs[key]; ok {
			t.Errorf("attribute %s present for a request that consumed no tokens", key)
		}
	}
}

func TestPopulateErrorAttributesEmitsCacheWriteDetailsWithoutAggregate(t *testing.T) {
	bifrostErr := &schemas.BifrostError{Error: &schemas.ErrorField{Message: "stream failed during cache creation"}}
	bifrostErr.ExtraFields.RequestType = schemas.ChatCompletionStreamRequest
	bifrostErr.ExtraFields.BilledUsage = &schemas.BifrostLLMUsage{
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens5m: 120,
				CachedWriteTokens1h: 80,
			},
		},
	}

	attrs := PopulateErrorAttributes(bifrostErr)

	for key, want := range map[string]any{
		schemas.AttrPromptTokenDetailsCachedWrite5m: 120,
		schemas.AttrPromptTokenDetailsCachedWrite1h: 80,
	} {
		if got := attrs[key]; got != want {
			t.Errorf("attribute %s = %v, want %v", key, got, want)
		}
	}
	// Zero-valued aggregates and totals stay absent: this BilledUsage carries
	// only cache-write details, so emitting the totals would stamp explicit
	// zeros on the span.
	for _, key := range []string{
		schemas.AttrUsageCacheCreationInputTokens,
		schemas.AttrInputTokens,
		schemas.AttrOutputTokens,
		schemas.AttrTotalTokens,
	} {
		if _, ok := attrs[key]; ok {
			t.Errorf("zero-valued attribute %s is present", key)
		}
	}
}

// ---------------------------------------------------------------------------
// Typed span payload fixtures.
// ---------------------------------------------------------------------------

func strPtr(s string) *string   { return &s }
func intPtr(i int) *int         { return &i }
func f64Ptr(f float64) *float64 { return &f }
func boolPtr(b bool) *bool      { return &b }

// chatFixture builds a chat request/response pair exercising params, content,
// tool calls, usage details and the response envelope.
func chatFixture() (*schemas.BifrostRequest, *schemas.BifrostResponse) {
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-4o",
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: strPtr("hello")}},
			},
			Params: &schemas.ChatParameters{
				MaxCompletionTokens: intPtr(512),
				Temperature:         f64Ptr(0.7),
				TopP:                f64Ptr(0.95),
				Stop:                []string{"END", "STOP"},
				PresencePenalty:     f64Ptr(0.1),
				FrequencyPenalty:    f64Ptr(0.2),
				ParallelToolCalls:   boolPtr(true),
				User:                strPtr("user-1"),
			},
		},
	}

	tier := schemas.BifrostServiceTier("flex")
	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			ID:                "chatcmpl-1",
			Model:             "gpt-4o-2024-08-06",
			Object:            "chat.completion",
			SystemFingerprint: "fp_abc",
			Created:           1700000000,
			ServiceTier:       &tier,
			Choices: []schemas.BifrostResponseChoice{{
				FinishReason: strPtr("stop"),
				ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
					Message: &schemas.ChatMessage{
						Role:    schemas.ChatMessageRoleAssistant,
						Content: &schemas.ChatMessageContent{ContentStr: strPtr("hi there")},
					},
				},
			}},
			Usage: &schemas.BifrostLLMUsage{
				PromptTokens:     10,
				CompletionTokens: 5,
				TotalTokens:      15,
				PromptTokensDetails: &schemas.ChatPromptTokensDetails{
					TextTokens:              8,
					AudioTokens:             2,
					CachedReadTokens:        4,
					CachedWriteTokens:       3,
					CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{CachedWriteTokens5m: 1, CachedWriteTokens1h: 2},
				},
				CompletionTokensDetails: &schemas.ChatCompletionTokensDetails{
					TextTokens:               5,
					ReasoningTokens:          3,
					AcceptedPredictionTokens: 1,
					RejectedPredictionTokens: 2,
					CitationTokens:           intPtr(4),
					NumSearchQueries:         intPtr(6),
				},
			},
		},
	}
	return req, resp
}

func responsesFixture() (*schemas.BifrostRequest, *schemas.BifrostResponse) {
	tier := schemas.BifrostServiceTier("priority")
	role := schemas.ResponsesInputMessageRoleUser
	msgType := schemas.ResponsesMessageTypeMessage
	req := &schemas.BifrostRequest{
		RequestType: schemas.ResponsesRequest,
		ResponsesRequest: &schemas.BifrostResponsesRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-5",
			Input: []schemas.ResponsesMessage{{
				Type:    &msgType,
				Role:    &role,
				Content: &schemas.ResponsesMessageContent{ContentStr: strPtr("summarize this")},
			}},
			Params: &schemas.ResponsesParameters{
				ParallelToolCalls: boolPtr(true),
				PromptCacheKey:    strPtr("cache-1"),
				SafetyIdentifier:  strPtr("safety-1"),
				ServiceTier:       &tier,
				Store:             boolPtr(false),
				Temperature:       f64Ptr(0.3),
				TopLogProbs:       intPtr(3),
				TopP:              f64Ptr(0.8),
				Truncation:        strPtr("auto"),
			},
		},
	}

	respID := "resp-1"
	resp := &schemas.BifrostResponse{
		ResponsesResponse: &schemas.BifrostResponsesResponse{
			ID:              &respID,
			Model:           "gpt-5-2026-01-01",
			ServiceTier:     &tier,
			MaxOutputTokens: intPtr(2048),
			MaxToolCalls:    intPtr(4),
			Store:           boolPtr(false),
			Temperature:     f64Ptr(0.3),
			TopP:            f64Ptr(0.8),
			Truncation:      strPtr("auto"),
			Usage: &schemas.ResponsesResponseUsage{
				InputTokens:  20,
				OutputTokens: 9,
				TotalTokens:  29,
				InputTokensDetails: &schemas.ResponsesResponseInputTokens{
					TextTokens: 18, CachedReadTokens: 6,
				},
				OutputTokensDetails: &schemas.ResponsesResponseOutputTokens{
					TextTokens: 9, ReasoningTokens: 4,
				},
			},
		},
	}
	return req, resp
}

// chatMultimodalFixture exercises the attachment path: a data: URL reduces to
// media type and size, an https URL is carried as a reference.
func chatMultimodalFixture() (*schemas.BifrostRequest, *schemas.BifrostResponse) {
	req, resp := chatFixture()
	req.ChatRequest.Input = []schemas.ChatMessage{{
		Role: schemas.ChatMessageRoleUser,
		Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{
			{Type: schemas.ChatContentBlockTypeText, Text: strPtr("what is in these")},
			{Type: schemas.ChatContentBlockTypeImage, ImageURLStruct: &schemas.ChatInputImage{
				URL: "data:image/png;base64,aGVsbG93b3JsZA==", Detail: strPtr("high")}},
			{Type: schemas.ChatContentBlockTypeImage, ImageURLStruct: &schemas.ChatInputImage{
				URL: "https://example.com/cat.png"}},
		}},
	}}
	return req, resp
}

// responsesMultimodalFixture is the Responses-API counterpart. Before
// ExtractResponsesAttachments existed this content was dropped entirely.
func responsesMultimodalFixture() (*schemas.BifrostRequest, *schemas.BifrostResponse) {
	req, resp := responsesFixture()
	role := schemas.ResponsesInputMessageRoleUser
	msgType := schemas.ResponsesMessageTypeMessage
	req.ResponsesRequest.Input = []schemas.ResponsesMessage{{
		Type: &msgType,
		Role: &role,
		Content: &schemas.ResponsesMessageContent{ContentBlocks: []schemas.ResponsesMessageContentBlock{
			{Type: schemas.ResponsesInputMessageContentBlockTypeText, Text: strPtr("summarize the attachment")},
			{Type: schemas.ResponsesInputMessageContentBlockTypeFile,
				ResponsesInputMessageContentBlockFile: &schemas.ResponsesInputMessageContentBlockFile{
					FileURL:  strPtr("https://example.com/report.pdf"),
					Filename: strPtr("report.pdf"),
					FileType: strPtr("application/pdf"),
				}},
		}},
	}}
	return req, resp
}

// TestSpanDataCarriesAttachments pins the one behavioural change the typed path
// introduces: span content now describes non-text blocks, which the previous
// text-only extraction dropped. Bounded by design - an inline payload is
// reduced to media type and size, never copied.
func TestSpanDataCarriesAttachments(t *testing.T) {
	for _, tc := range []struct {
		name string
		fx   func() (*schemas.BifrostRequest, *schemas.BifrostResponse)
		want schemas.AttachmentSummary
	}{
		{"chat inline image", chatMultimodalFixture, schemas.AttachmentSummary{
			Kind: schemas.AttachmentImage, MediaType: "image/png", Detail: "high",
			Inline: true, ByteSize: 10}},
		{"responses file reference", responsesMultimodalFixture, schemas.AttachmentSummary{
			Kind: schemas.AttachmentFile, MediaType: "application/pdf",
			URL: "https://example.com/report.pdf", Filename: "report.pdf"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := tc.fx()
			data := BuildLLMSpanData(req, nil, nil, SpanBuildOptions{WantContent: true})
			if len(data.InputMessages) == 0 {
				t.Fatal("no input messages")
			}
			got := data.InputMessages[0].Attachments
			if len(got) == 0 {
				t.Fatal("no attachments extracted")
			}
			if got[0] != tc.want {
				t.Errorf("attachment = %#v, want %#v", got[0], tc.want)
			}
			for _, a := range got {
				if a.Data != "" {
					t.Errorf("payload bytes copied by default: %#v", a)
				}
			}
		})
	}
}

// Raw is built only when a connector declared RawPayloadConsumer.
func TestRawPayloadsOnlyBuiltUnderDemand(t *testing.T) {
	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{ID: "c1", Model: "gpt-4o-mini"},
	}
	resp.ChatResponse.ExtraFields.RawRequest = map[string]any{"prompt": "secret-request"}
	resp.ChatResponse.ExtraFields.RawResponse = map[string]any{"text": "secret-response"}

	off := BuildLLMSpanData(nil, resp, nil, SpanBuildOptions{})
	if off.RawRequest != "" || off.RawResponse != "" {
		t.Errorf("raw built without demand: req=%q resp=%q", off.RawRequest, off.RawResponse)
	}
	if attrs := off.Attributes(); attrs[schemas.AttrBifrostRawRequest] != nil || attrs[schemas.AttrBifrostRawResponse] != nil {
		t.Error("raw attributes emitted without demand")
	}

	on := BuildLLMSpanData(nil, resp, nil, SpanBuildOptions{WantRawPayloads: true})
	if !strings.Contains(on.RawRequest, "secret-request") {
		t.Errorf("raw request not captured under demand: %q", on.RawRequest)
	}
	if !strings.Contains(on.RawResponse, "secret-response") {
		t.Errorf("raw response not captured under demand: %q", on.RawResponse)
	}
	// Even under demand, raw stays off span.Attributes.
	attrs := on.Attributes()
	if attrs[schemas.AttrBifrostRawRequest] != nil || attrs[schemas.AttrBifrostRawResponse] != nil {
		t.Error("raw leaked into span attributes under demand")
	}
}

// An oversized body is dropped, not truncated.
func TestRawPayloadsOverCapAreDropped(t *testing.T) {
	huge := strings.Repeat("x", schemas.RawPayloadCap+1)
	resp := &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{ID: "c1"}}
	resp.ChatResponse.ExtraFields.RawResponse = huge
	resp.ChatResponse.ExtraFields.RawRequest = "small"

	d := BuildLLMSpanData(nil, resp, nil, SpanBuildOptions{WantRawPayloads: true})
	if d.RawResponse != "" {
		t.Errorf("over-cap payload kept (%d bytes)", len(d.RawResponse))
	}
	if d.RawRequest != "small" {
		t.Errorf("under-cap payload dropped: %q", d.RawRequest)
	}
}

// Raw is content, so disable_content_logging strips it.
func TestRawPayloadsAreContentAttributes(t *testing.T) {
	for _, k := range []string{schemas.AttrBifrostRawRequest, schemas.AttrBifrostRawResponse} {
		if !schemas.IsContentAttribute(k) {
			t.Errorf("%s is not classified as content; disable_content_logging would not strip it", k)
		}
	}
}

// ---------------------------------------------------------------------------
// Client tool call items whose input does not live on `arguments`.
// ---------------------------------------------------------------------------

// customToolCallItem builds the item Codex emits for a custom tool.
func customToolCallItem(name, input string) schemas.ResponsesMessage {
	return schemas.ResponsesMessage{
		Type: schemas.Ptr(schemas.ResponsesMessageTypeCustomToolCall),
		ID:   schemas.Ptr("ctc_1"),
		ResponsesToolMessage: &schemas.ResponsesToolMessage{
			CallID:                  schemas.Ptr("call_1"),
			Name:                    schemas.Ptr(name),
			ResponsesCustomToolCall: &schemas.ResponsesCustomToolCall{Input: input},
		},
	}
}

// A custom_tool_call keeps its input on `input`; the old type tag dropped it.
func TestResponsesSummariesCarryCustomToolCallInput(t *testing.T) {
	const input = `{"cmd":"whoami","max_output_tokens":1000}`
	item := customToolCallItem("exec_command", input)

	for _, tc := range []struct {
		side string
		got  []ResponsesMessageSummary
	}{
		{"output", extractResponsesOutputMessages(&schemas.BifrostResponsesResponse{Output: []schemas.ResponsesMessage{item}})},
		{"input", extractResponsesInputMessages([]schemas.ResponsesMessage{item})},
	} {
		if len(tc.got) != 1 {
			t.Fatalf("%s: len = %d, want 1: %+v", tc.side, len(tc.got), tc.got)
		}
		summary := tc.got[0]
		if summary.Content != "" {
			t.Fatalf("%s: content = %q, want the call on tool_calls", tc.side, summary.Content)
		}
		if len(summary.ToolCalls) != 1 {
			t.Fatalf("%s: tool_calls = %+v, want 1", tc.side, summary.ToolCalls)
		}
		call := summary.ToolCalls[0]
		if call.Type != "custom" || call.Name != "exec_command" {
			t.Fatalf("%s: type/name = %q/%q, want custom/exec_command", tc.side, call.Type, call.Name)
		}
		if call.Args != input {
			t.Fatalf("%s: args = %q, want %q", tc.side, call.Args, input)
		}
		if call.ID != "ctc_1" {
			t.Fatalf("%s: id = %q, want ctc_1", tc.side, call.ID)
		}
	}
}

// local_shell_call keeps its command on `action`, code_interpreter_call on `code`.
func TestResponsesSummariesCarryShellAndInterpreterInput(t *testing.T) {
	output := []schemas.ResponsesMessage{
		{
			Type: schemas.Ptr(schemas.ResponsesMessageTypeLocalShellCall),
			ResponsesToolMessage: &schemas.ResponsesToolMessage{
				Action: &schemas.ResponsesToolMessageActionStruct{
					ResponsesLocalShellToolCallAction: &schemas.ResponsesLocalShellToolCallAction{
						Type:    "exec",
						Command: []string{"bash", "-lc", "whoami"},
					},
				},
			},
		},
		{
			Type: schemas.Ptr(schemas.ResponsesMessageTypeCodeInterpreterCall),
			ResponsesToolMessage: &schemas.ResponsesToolMessage{
				ResponsesCodeInterpreterToolCall: &schemas.ResponsesCodeInterpreterToolCall{
					Code: schemas.Ptr("print(1)"),
				},
			},
		},
	}

	got := extractResponsesOutputMessages(&schemas.BifrostResponsesResponse{Output: output})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}

	shell := got[0].ToolCalls
	if len(shell) != 1 || shell[0].Type != "local_shell" || shell[0].Name != "local_shell" {
		t.Fatalf("shell tool call = %+v", shell)
	}
	if !strings.Contains(shell[0].Args, `"whoami"`) {
		t.Fatalf("shell args = %q, want the command", shell[0].Args)
	}

	interp := got[1].ToolCalls
	if len(interp) != 1 || interp[0].Type != "code_interpreter" || interp[0].Args != "print(1)" {
		t.Fatalf("interpreter tool call = %+v", interp)
	}
}

// Items with no model-generated input keep the placeholder tag.
func TestResponsesSummariesTagItemsWithoutInput(t *testing.T) {
	got := extractResponsesOutputMessages(&schemas.BifrostResponsesResponse{
		Output: []schemas.ResponsesMessage{{
			Type:                 schemas.Ptr(schemas.ResponsesMessageTypeFileSearchCall),
			ResponsesToolMessage: &schemas.ResponsesToolMessage{Name: schemas.Ptr("docs")},
		}},
	})
	if len(got) != 1 || got[0].Content != "[file_search_call] docs" {
		t.Fatalf("got = %+v, want the [file_search_call] docs tag", got)
	}
}

// End-to-end: a custom tool call must survive spanbuild and attribute marshaling,
// which is what a downstream sink (the otel plugin) actually reads.
func TestSpanAttributesCarryCustomToolCallInput(t *testing.T) {
	const input = `{"cmd":"whoami","max_output_tokens":1000}`
	resp := &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{
		ID:     schemas.Ptr("resp_1"),
		Model:  "gpt-5-codex",
		Output: []schemas.ResponsesMessage{customToolCallItem("exec_command", input)},
	}}
	req := &schemas.BifrostRequest{RequestType: schemas.ResponsesRequest}

	attrs := BuildLLMSpanData(req, resp, nil, SpanBuildOptions{WantContent: true}).Attributes()
	raw, ok := attrs[schemas.AttrOutputMessages].(string)
	if !ok {
		t.Fatalf("%s = %T, want JSON string", schemas.AttrOutputMessages, attrs[schemas.AttrOutputMessages])
	}
	if !strings.Contains(raw, `whoami`) || !strings.Contains(raw, `"name":"exec_command"`) {
		t.Fatalf("%s = %s, want the tool name and its input", schemas.AttrOutputMessages, raw)
	}

	// Tool args are content, so they must vanish when no connector reads content.
	noContent := BuildLLMSpanData(req, resp, nil, SpanBuildOptions{}).Attributes()
	if got, ok := noContent[schemas.AttrOutputMessages]; ok {
		t.Fatalf("%s = %v, want absent without WantContent", schemas.AttrOutputMessages, got)
	}
}

// A status-only error must keep its status; Error is optional.
func TestPopulateErrorAttributes_StatusOnlyError(t *testing.T) {
	attrs := PopulateErrorAttributes(&schemas.BifrostError{StatusCode: schemas.Ptr(429)})

	if got := schemas.GetIntAttr(attrs, schemas.AttrHTTPResponseStatusCode); got != 429 {
		t.Errorf("%s = %d, want 429", schemas.AttrHTTPResponseStatusCode, got)
	}
	for _, absent := range []string{schemas.AttrError, schemas.AttrErrorTypeSpec, schemas.AttrErrorCode} {
		if _, ok := attrs[absent]; ok {
			t.Errorf("%s should be absent when Error is nil, got %v", absent, attrs[absent])
		}
	}
}

func TestPopulateErrorAttributes_NilError(t *testing.T) {
	if attrs := PopulateErrorAttributes(nil); len(attrs) != 0 {
		t.Errorf("attrs = %v, want empty for a nil error", attrs)
	}
}

// A failed request carries its own raw bodies on BifrostError.ExtraFields. They used to
// be dropped entirely: ApplyResponse only reached buildResponseSide when resp != nil, and
// the raw assignment lived only there, so the provider's error body — the most useful
// thing to see for a failure — never reached a connector.
func TestRawPayloadsCapturedOnFailedRequest(t *testing.T) {
	newErr := func() *schemas.BifrostError {
		e := &schemas.BifrostError{Error: &schemas.ErrorField{Message: "model_not_found"}}
		e.ExtraFields.RawRequest = map[string]any{"prompt": "failed-request-body"}
		e.ExtraFields.RawResponse = map[string]any{"error": "failed-response-body"}
		return e
	}

	t.Run("captured under demand with no response", func(t *testing.T) {
		d := BuildLLMSpanData(nil, nil, newErr(), SpanBuildOptions{WantRawPayloads: true})
		if !strings.Contains(d.RawRequest, "failed-request-body") {
			t.Errorf("raw request not captured on failure: %q", d.RawRequest)
		}
		if !strings.Contains(d.RawResponse, "failed-response-body") {
			t.Errorf("raw response not captured on failure: %q", d.RawResponse)
		}
	})

	t.Run("still gated on demand", func(t *testing.T) {
		d := BuildLLMSpanData(nil, nil, newErr(), SpanBuildOptions{})
		if d.RawRequest != "" || d.RawResponse != "" {
			t.Errorf("raw built without demand: req=%q resp=%q", d.RawRequest, d.RawResponse)
		}
	})

	t.Run("via ApplyResponse, the live two-pass path", func(t *testing.T) {
		d := BuildLLMSpanData(nil, nil, nil, SpanBuildOptions{WantRawPayloads: true})
		ApplyResponse(d, nil, newErr(), SpanBuildOptions{WantRawPayloads: true})
		if !strings.Contains(d.RawRequest, "failed-request-body") {
			t.Errorf("raw request not captured via ApplyResponse: %q", d.RawRequest)
		}
		if !strings.Contains(d.RawResponse, "failed-response-body") {
			t.Errorf("raw response not captured via ApplyResponse: %q", d.RawResponse)
		}
	})

	// A cancelled stream arrives with BOTH an accumulated response and an error. The
	// response side runs first and has the richer bodies; the error side must not
	// overwrite them with its own.
	t.Run("does not clobber payloads the response side already set", func(t *testing.T) {
		resp := &schemas.BifrostResponse{
			ChatResponse: &schemas.BifrostChatResponse{ID: "c1", Model: "gpt-4o-mini"},
		}
		resp.ChatResponse.ExtraFields.RawRequest = map[string]any{"prompt": "accumulated-request"}
		resp.ChatResponse.ExtraFields.RawResponse = map[string]any{"text": "accumulated-response"}

		d := BuildLLMSpanData(nil, resp, newErr(), SpanBuildOptions{WantRawPayloads: true})
		if !strings.Contains(d.RawRequest, "accumulated-request") {
			t.Errorf("response-side raw request was overwritten: %q", d.RawRequest)
		}
		if !strings.Contains(d.RawResponse, "accumulated-response") {
			t.Errorf("response-side raw response was overwritten: %q", d.RawResponse)
		}
	})
}

// A chat request carried every other parameter onto the span but never its tools, so
// gen_ai.request.tools was absent on chat while Responses emitted it — the otel connector
// e2e failed on it.
func TestChatRequestExportsTools(t *testing.T) {
	desc := "Look up the weather"
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Params: &schemas.ChatParameters{
				Tools: []schemas.ChatTool{
					{Type: "function", Function: &schemas.ChatToolFunction{Name: "get_weather", Description: &desc}},
					{Type: "function", Function: &schemas.ChatToolFunction{Name: "get_time"}},
					{Type: "web_search"}, // built-in: no function, falls back to the type
				},
			},
		},
	}

	attrs := BuildLLMSpanData(req, nil, nil, SpanBuildOptions{WantContent: true}).Attributes()

	raw, ok := attrs[schemas.AttrTools].(string)
	if !ok {
		t.Fatalf("%s missing or not a string: %#v", schemas.AttrTools, attrs[schemas.AttrTools])
	}
	var got []schemas.ToolSummary
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("%s is not valid JSON: %v (%s)", schemas.AttrTools, err, raw)
	}
	want := []schemas.ToolSummary{
		{Name: "get_weather", Description: "Look up the weather"},
		{Name: "get_time"},
		{Name: "web_search"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d tools, want %d: %s", len(got), len(want), raw)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tool %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A chat request without tools must not emit an empty tools attribute.
func TestChatRequestWithoutToolsOmitsAttribute(t *testing.T) {
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{Params: &schemas.ChatParameters{}},
	}
	attrs := BuildLLMSpanData(req, nil, nil, SpanBuildOptions{WantContent: true}).Attributes()
	if _, present := attrs[schemas.AttrTools]; present {
		t.Errorf("%s present with no tools configured", schemas.AttrTools)
	}
}

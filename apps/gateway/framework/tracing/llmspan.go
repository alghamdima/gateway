// Package tracing provides distributed tracing utilities for Bifrost.
package tracing

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

func formatTraceValue(v any) string {
	if v == nil {
		return ""
	}

	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return ""
		}
		rv = rv.Elem()
	}

	switch rv.Kind() {
	case reflect.Map, reflect.Slice, reflect.Array, reflect.Struct:
		if data, err := schemas.MarshalString(v); err == nil {
			return data
		}
	}

	return fmt.Sprint(rv.Interface())
}

// PopulateErrorAttributes extracts error attributes from a BifrostError.
func PopulateErrorAttributes(err *schemas.BifrostError) map[string]any {
	attrs := make(map[string]any)
	if err == nil {
		return attrs
	}

	// Error is optional, so a status-only error must not return early here.
	if err.Error != nil {
		attrs[schemas.AttrError] = err.Error.Message
		if err.Error.Type != nil {
			attrs[schemas.AttrErrorTypeSpec] = *err.Error.Type
		}
		if err.Error.Code != nil {
			attrs[schemas.AttrErrorCode] = *err.Error.Code
		}
	}
	// Effective, not raw: an internal error has no StatusCode but still returns 500.
	attrs[schemas.AttrHTTPResponseStatusCode] = err.EffectiveHTTPStatus()

	// Usage the provider billed us for even though the request failed or was
	// cancelled (see BifrostError.ExtraFields.BilledUsage). Governance and the
	// logging plugin already charge for this; without emitting it here every
	// span-based consumer (the otel plugin) records zero tokens for the same request.
	if u := err.ExtraFields.BilledUsage; u != nil {
		// Everything below is gated on > 0: attachBilledUsageFromContext
		// (core/providers/utils) attaches a BilledUsage when *any* of tokens,
		// details or cost is set, so a details-only usage reaches here with
		// zero totals; emitting those would stamp explicit zeros on the span
		// where the success paths only ever see provider-reported values.
		if u.PromptTokens > 0 {
			attrs[schemas.AttrInputTokens] = u.PromptTokens
		}
		if u.CompletionTokens > 0 {
			attrs[schemas.AttrOutputTokens] = u.CompletionTokens
		}
		if u.TotalTokens > 0 {
			attrs[schemas.AttrTotalTokens] = u.TotalTokens
		}

		if d := u.PromptTokensDetails; d != nil {
			// The nested cached-write detail keys are namespaced per request
			// type on the success paths (input_token_details.* for Responses,
			// prompt_token_details.* for chat); mirror that here instead of
			// emitting both families, which would break the otel plugin's
			// assumption that the two namespaces are mutually exclusive.
			isResponses := err.ExtraFields.RequestType == schemas.ResponsesRequest ||
				err.ExtraFields.RequestType == schemas.ResponsesStreamRequest
			if d.CachedReadTokens > 0 {
				attrs[schemas.AttrUsageCacheReadInputTokens] = d.CachedReadTokens
			}
			if d.CachedWriteTokens > 0 {
				attrs[schemas.AttrUsageCacheCreationInputTokens] = d.CachedWriteTokens
			}
			if wd := d.CachedWriteTokenDetails; wd != nil {
				if wd.CachedWriteTokens5m > 0 {
					if isResponses {
						attrs[schemas.AttrInputTokenDetailsCachedWrite5m] = wd.CachedWriteTokens5m
					} else {
						attrs[schemas.AttrPromptTokenDetailsCachedWrite5m] = wd.CachedWriteTokens5m
					}
				}
				if wd.CachedWriteTokens1h > 0 {
					if isResponses {
						attrs[schemas.AttrInputTokenDetailsCachedWrite1h] = wd.CachedWriteTokens1h
					} else {
						attrs[schemas.AttrPromptTokenDetailsCachedWrite1h] = wd.CachedWriteTokens1h
					}
				}
			}
		}
	}

	return attrs
}

// ===============================================
// Chat Completion Request/Response
// ===============================================

// ===============================================
// Text Completion Request/Response
// ===============================================

// ===============================================
// Embedding Request/Response
// ===============================================

// ===============================================
// Transcription Request/Response
// ===============================================

// ===============================================
// Speech Request/Response
// ===============================================

// ===============================================
// Responses API Request/Response
// ===============================================

// ===============================================
// Batch Operations Request/Response
// ===============================================

// ===============================================
// File Operations Request/Response
// ===============================================

// ===============================================
// Helper functions for extracting messages
// ===============================================

// ResponsesMessageSummary extends schemas.MessageSummary with reasoning
type ResponsesMessageSummary struct {
	Role        string                      `json:"role"`
	Content     string                      `json:"content"`
	Attachments []schemas.AttachmentSummary `json:"attachments,omitempty"`
	Reasoning   string                      `json:"reasoning,omitempty"`
	ToolCalls   []schemas.ToolCallSummary   `json:"tool_calls,omitempty"`
	ToolCallID  string                      `json:"tool_call_id,omitempty"`
}

// extractResponsesOutputMessages extracts output messages from a Responses API response.
func extractResponsesOutputMessages(resp *schemas.BifrostResponsesResponse) []ResponsesMessageSummary {
	if resp == nil {
		return nil
	}

	result := make([]ResponsesMessageSummary, 0, len(resp.Output))
	for _, msg := range resp.Output {
		msgType := schemas.ResponsesMessageTypeMessage
		if msg.Type != nil {
			msgType = *msg.Type
		}

		switch msgType {
		case schemas.ResponsesMessageTypeMessage:
			if msg.Role == nil {
				continue
			}
			result = append(result, ResponsesMessageSummary{
				Role:      string(*msg.Role),
				Content:   extractResponsesMessageTextContent(&msg),
				Reasoning: extractResponsesReasoning(msg.ResponsesReasoning),
			})

		case schemas.ResponsesMessageTypeReasoning:
			reasoning := extractResponsesReasoning(msg.ResponsesReasoning)
			if reasoning == "" {
				continue
			}
			result = append(result, ResponsesMessageSummary{
				Role:      "assistant",
				Reasoning: reasoning,
			})

		case schemas.ResponsesMessageTypeFunctionCall:
			if msg.ResponsesToolMessage == nil || msg.ResponsesToolMessage.Name == nil {
				continue
			}
			tc := schemas.ToolCallSummary{
				Type: "function",
				Name: *msg.ResponsesToolMessage.Name,
			}
			if msg.ID != nil {
				tc.ID = *msg.ID
			}
			if msg.ResponsesToolMessage.Arguments != nil {
				tc.Args = *msg.ResponsesToolMessage.Arguments
			}
			result = append(result, ResponsesMessageSummary{
				Role:      "assistant",
				ToolCalls: []schemas.ToolCallSummary{tc},
			})

		case schemas.ResponsesMessageTypeMCPCall:
			if msg.ResponsesToolMessage == nil || msg.ResponsesToolMessage.Name == nil {
				continue
			}
			tc := schemas.ToolCallSummary{
				Type: "mcp",
				Name: *msg.ResponsesToolMessage.Name,
			}
			if msg.ID != nil {
				tc.ID = *msg.ID
			}
			if msg.ResponsesToolMessage.Arguments != nil {
				tc.Args = *msg.ResponsesToolMessage.Arguments
			}
			result = append(result, ResponsesMessageSummary{
				Role:      "assistant",
				ToolCalls: []schemas.ToolCallSummary{tc},
			})

		case schemas.ResponsesMessageTypeWebSearchCall:
			result = append(result, ResponsesMessageSummary{
				Role:    "assistant",
				Content: "[web_search_call]",
			})

		case schemas.ResponsesMessageTypeComputerCall:
			result = append(result, ResponsesMessageSummary{
				Role:    "assistant",
				Content: "[computer_call]",
			})

		case schemas.ResponsesMessageTypeCustomToolCall,
			schemas.ResponsesMessageTypeLocalShellCall,
			schemas.ResponsesMessageTypeShellCall,
			schemas.ResponsesMessageTypeApplyPatchCall,
			schemas.ResponsesMessageTypeCodeInterpreterCall:
			result = append(result, ResponsesMessageSummary{
				Role:      "assistant",
				ToolCalls: []schemas.ToolCallSummary{responsesItemToolCall(&msg, msgType)},
			})

		case schemas.ResponsesMessageTypeFileSearchCall,
			schemas.ResponsesMessageTypeImageGenerationCall:
			result = append(result, ResponsesMessageSummary{
				Role:    "assistant",
				Content: responsesItemTag(&msg, msgType),
			})

		default:
			continue
		}
	}
	return result
}

// extractResponsesInputMessages extracts input messages from a Responses API request.
func extractResponsesInputMessages(messages []schemas.ResponsesMessage) []ResponsesMessageSummary {
	if len(messages) == 0 {
		return nil
	}
	result := make([]ResponsesMessageSummary, 0, len(messages))
	for _, msg := range messages {
		// Nil Type is treated as "message", consistent with the provider converters.
		msgType := schemas.ResponsesMessageTypeMessage
		if msg.Type != nil {
			msgType = *msg.Type
		}

		switch msgType {
		case schemas.ResponsesMessageTypeMessage:
			role := ""
			if msg.Role != nil {
				role = string(*msg.Role)
			}
			summary := ResponsesMessageSummary{
				Role:        role,
				Content:     extractResponsesMessageTextContent(&msg),
				Attachments: schemas.ExtractResponsesAttachments(msg.Content, schemas.AttachmentOptions{}),
				Reasoning:   extractResponsesReasoning(msg.ResponsesReasoning),
			}
			result = append(result, summary)

		case schemas.ResponsesMessageTypeReasoning:
			reasoning := extractResponsesReasoning(msg.ResponsesReasoning)
			if reasoning == "" {
				continue
			}
			result = append(result, ResponsesMessageSummary{
				Role:      "assistant",
				Reasoning: reasoning,
			})

		case schemas.ResponsesMessageTypeFunctionCall:
			if msg.ResponsesToolMessage == nil || msg.ResponsesToolMessage.Name == nil {
				continue
			}
			tc := schemas.ToolCallSummary{
				Type: "function",
				Name: *msg.ResponsesToolMessage.Name,
			}
			if msg.ID != nil {
				tc.ID = *msg.ID
			}
			if msg.ResponsesToolMessage.Arguments != nil {
				tc.Args = *msg.ResponsesToolMessage.Arguments
			}
			result = append(result, ResponsesMessageSummary{
				Role:      "assistant",
				ToolCalls: []schemas.ToolCallSummary{tc},
			})

		case schemas.ResponsesMessageTypeFunctionCallOutput:
			if msg.ResponsesToolMessage == nil {
				continue
			}
			summary := ResponsesMessageSummary{
				Role:    "tool",
				Content: extractResponsesToolOutputContent(msg.ResponsesToolMessage.Output),
			}
			if msg.ResponsesToolMessage.CallID != nil {
				summary.ToolCallID = *msg.ResponsesToolMessage.CallID
			}
			result = append(result, summary)

		case schemas.ResponsesMessageTypeMCPCall:
			if msg.ResponsesToolMessage == nil {
				continue
			}
			if msg.ResponsesToolMessage.Name != nil {
				// Outbound MCP tool call (assistant side)
				tc := schemas.ToolCallSummary{
					Type: "mcp",
					Name: *msg.ResponsesToolMessage.Name,
				}
				if msg.ID != nil {
					tc.ID = *msg.ID
				}
				if msg.ResponsesToolMessage.Arguments != nil {
					tc.Args = *msg.ResponsesToolMessage.Arguments
				}
				result = append(result, ResponsesMessageSummary{
					Role:      "assistant",
					ToolCalls: []schemas.ToolCallSummary{tc},
				})
			} else if msg.ResponsesToolMessage.CallID != nil {
				// Inbound MCP tool result (user side)
				result = append(result, ResponsesMessageSummary{
					Role:       "tool",
					ToolCallID: *msg.ResponsesToolMessage.CallID,
					Content:    extractResponsesToolOutputContent(msg.ResponsesToolMessage.Output),
				})
			}

		case schemas.ResponsesMessageTypeMCPApprovalRequest:
			content := "[mcp_approval_request]"
			if msg.ResponsesToolMessage != nil &&
				msg.ResponsesToolMessage.Action != nil &&
				msg.ResponsesToolMessage.Action.ResponsesMCPApprovalRequestAction != nil {
				content = msg.ResponsesToolMessage.Action.ResponsesMCPApprovalRequestAction.Name
			}
			result = append(result, ResponsesMessageSummary{
				Role:    "assistant",
				Content: content,
			})

		case schemas.ResponsesMessageTypeWebSearchCall:
			result = append(result, ResponsesMessageSummary{
				Role:    "assistant",
				Content: "[web_search_call]",
			})

		case schemas.ResponsesMessageTypeComputerCall:
			result = append(result, ResponsesMessageSummary{
				Role:    "assistant",
				Content: "[computer_call]",
			})

		case schemas.ResponsesMessageTypeComputerCallOutput:
			result = append(result, ResponsesMessageSummary{
				Role:    "tool",
				Content: "[computer_call_output]",
			})

		case schemas.ResponsesMessageTypeCustomToolCall,
			schemas.ResponsesMessageTypeLocalShellCall,
			schemas.ResponsesMessageTypeShellCall,
			schemas.ResponsesMessageTypeApplyPatchCall,
			schemas.ResponsesMessageTypeCodeInterpreterCall:
			result = append(result, ResponsesMessageSummary{
				Role:      "assistant",
				ToolCalls: []schemas.ToolCallSummary{responsesItemToolCall(&msg, msgType)},
			})

		case schemas.ResponsesMessageTypeFileSearchCall,
			schemas.ResponsesMessageTypeImageGenerationCall:
			result = append(result, ResponsesMessageSummary{
				Role:    "assistant",
				Content: responsesItemTag(&msg, msgType),
			})

		case schemas.ResponsesMessageTypeLocalShellCallOutput,
			schemas.ResponsesMessageTypeShellCallOutput,
			schemas.ResponsesMessageTypeApplyPatchCallOutput,
			schemas.ResponsesMessageTypeCustomToolCallOutput:
			content := ""
			if msg.ResponsesToolMessage != nil {
				content = extractResponsesToolOutputContent(msg.ResponsesToolMessage.Output)
			}
			summary := ResponsesMessageSummary{
				Role:    "tool",
				Content: content,
			}
			if msg.ResponsesToolMessage != nil && msg.ResponsesToolMessage.CallID != nil {
				summary.ToolCallID = *msg.ResponsesToolMessage.CallID
			}
			result = append(result, summary)

		default:
			continue
		}
	}
	return result
}

// responsesItemToolCall summarizes a tool call whose model-generated payload does
// not live on `arguments`: `input`, `action` and `code` respectively.
func responsesItemToolCall(msg *schemas.ResponsesMessage, msgType schemas.ResponsesMessageType) schemas.ToolCallSummary {
	tc := schemas.ToolCallSummary{Type: responsesItemToolType(msgType)}
	if msg.ID != nil {
		tc.ID = *msg.ID
	}
	tm := msg.ResponsesToolMessage
	if tm == nil {
		tc.Name = tc.Type
		return tc
	}
	if tc.ID == "" && tm.CallID != nil {
		tc.ID = *tm.CallID
	}
	if tm.Name != nil {
		tc.Name = *tm.Name
	}
	switch msgType {
	case schemas.ResponsesMessageTypeCustomToolCall:
		if tm.ResponsesCustomToolCall != nil {
			tc.Args = tm.ResponsesCustomToolCall.Input
		}
	case schemas.ResponsesMessageTypeLocalShellCall:
		if tm.Action != nil && tm.Action.ResponsesLocalShellToolCallAction != nil {
			if args, err := schemas.MarshalString(tm.Action.ResponsesLocalShellToolCallAction); err == nil {
				tc.Args = args
			}
		}
	case schemas.ResponsesMessageTypeShellCall:
		if tm.Action != nil && tm.Action.ResponsesShellToolCallAction != nil {
			if args, err := schemas.MarshalString(tm.Action.ResponsesShellToolCallAction); err == nil {
				tc.Args = args
			}
		}
	case schemas.ResponsesMessageTypeApplyPatchCall:
		if tm.ResponsesApplyPatchCall != nil && tm.ResponsesApplyPatchCall.Operation != nil {
			if args, err := schemas.MarshalString(tm.ResponsesApplyPatchCall.Operation); err == nil {
				tc.Args = args
			}
		}
	case schemas.ResponsesMessageTypeCodeInterpreterCall:
		if tm.ResponsesCodeInterpreterToolCall != nil && tm.ResponsesCodeInterpreterToolCall.Code != nil {
			tc.Args = *tm.ResponsesCodeInterpreterToolCall.Code
		}
	}
	// local_shell_call, shell_call, apply_patch_call and code_interpreter_call have no name of their own.
	if tc.Name == "" {
		tc.Name = tc.Type
	}
	return tc
}

// responsesItemToolType maps an item type onto the tool type the summary reports.
func responsesItemToolType(msgType schemas.ResponsesMessageType) string {
	switch msgType {
	case schemas.ResponsesMessageTypeCustomToolCall:
		return "custom"
	case schemas.ResponsesMessageTypeLocalShellCall:
		return "local_shell"
	case schemas.ResponsesMessageTypeShellCall:
		return "shell"
	case schemas.ResponsesMessageTypeApplyPatchCall:
		return "apply_patch"
	case schemas.ResponsesMessageTypeCodeInterpreterCall:
		return "code_interpreter"
	default:
		return string(msgType)
	}
}

// responsesItemTag renders the placeholder for items with no input to record,
// e.g. "[file_search_call] my_tool".
func responsesItemTag(msg *schemas.ResponsesMessage, msgType schemas.ResponsesMessageType) string {
	content := "[" + string(msgType) + "]"
	if msg.ResponsesToolMessage != nil && msg.ResponsesToolMessage.Name != nil && *msg.ResponsesToolMessage.Name != "" {
		content += " " + *msg.ResponsesToolMessage.Name
	}
	return content
}

// extractResponsesMessageTextContent extracts plain text from a ResponsesMessage's Content field.
func extractResponsesMessageTextContent(msg *schemas.ResponsesMessage) string {
	if msg.Content == nil {
		return ""
	}
	if msg.Content.ContentStr != nil && *msg.Content.ContentStr != "" {
		return *msg.Content.ContentStr
	}
	var sb strings.Builder
	for _, block := range msg.Content.ContentBlocks {
		if block.Text != nil {
			sb.WriteString(*block.Text)
		} else if block.ResponsesOutputMessageContentRefusal != nil && block.ResponsesOutputMessageContentRefusal.Refusal != "" {
			sb.WriteString(block.ResponsesOutputMessageContentRefusal.Refusal)
		}
	}
	return sb.String()
}

// extractResponsesToolOutputContent extracts text from a ResponsesToolMessageOutputStruct,
func extractResponsesToolOutputContent(output *schemas.ResponsesToolMessageOutputStruct) string {
	if output == nil {
		return ""
	}
	if output.ResponsesToolCallOutputStr != nil {
		return *output.ResponsesToolCallOutputStr
	}
	if len(output.ResponsesShellCallOutput) > 0 {
		return schemas.ShellCallOutputText(output.ResponsesShellCallOutput)
	}
	var sb strings.Builder
	for _, block := range output.ResponsesFunctionToolCallOutputBlocks {
		if block.Text != nil {
			sb.WriteString(*block.Text)
		}
	}
	return sb.String()
}

// extractResponsesReasoning concatenates all summary text blocks from a ResponsesReasoning.
func extractResponsesReasoning(r *schemas.ResponsesReasoning) string {
	if r == nil {
		return ""
	}
	var sb strings.Builder
	for _, block := range r.Summary {
		if block.Text != "" {
			sb.WriteString(block.Text)
		}
	}
	return sb.String()
}

// extractMessageContent concatenates a chat message's text blocks. Kept as a
// thin wrapper because tracer.go uses it for root-span propagation.
func extractMessageContent(content *schemas.ChatMessageContent) string {
	return schemas.ExtractChatContentText(content)
}

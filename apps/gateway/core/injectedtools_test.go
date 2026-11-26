package bifrost

import (
	"context"
	"errors"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeInjectedToolSource struct {
	tools    map[string]schemas.ChatTool
	maxDepth int
}

func (f fakeInjectedToolSource) GetInjectedTool(_ context.Context, clientName, toolName string) (schemas.ChatTool, error) {
	tool, ok := f.tools[clientName+"-"+toolName]
	if !ok {
		return schemas.ChatTool{}, errors.New("not found")
	}
	return schemas.DeepCopyChatTool(tool), nil // the MCP manager's contract: the caller owns the copy
}

func (f fakeInjectedToolSource) GetMaxAgentDepth() int { return f.maxDepth }

func tavilySearchTool() schemas.ChatTool {
	return schemas.ChatTool{
		Type: schemas.ChatToolTypeFunction,
		Function: &schemas.ChatToolFunction{
			Name:        "tavily-search",
			Description: schemas.Ptr("Search the web"),
			Parameters:  &schemas.ToolFunctionParameters{Type: "object"},
		},
	}
}

func chatToolCall(id, name string) schemas.ChatAssistantMessageToolCall {
	return schemas.ChatAssistantMessageToolCall{
		ID:       schemas.Ptr(id),
		Type:     schemas.Ptr("function"),
		Function: schemas.ChatAssistantMessageToolCallFunction{Name: schemas.Ptr(name), Arguments: `{"query":"q"}`},
	}
}

func chatTurnResponse(text string, finish string, promptTokens int, calls ...schemas.ChatAssistantMessageToolCall) *schemas.BifrostChatResponse {
	msg := &schemas.ChatMessage{Role: schemas.ChatMessageRoleAssistant}
	if text != "" {
		msg.Content = &schemas.ChatMessageContent{ContentStr: schemas.Ptr(text)}
	}
	if len(calls) > 0 {
		msg.ChatAssistantMessage = &schemas.ChatAssistantMessage{ToolCalls: calls}
	}
	return &schemas.BifrostChatResponse{
		ID: "resp",
		Choices: []schemas.BifrostResponseChoice{{
			FinishReason:                schemas.Ptr(finish),
			ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{Message: msg},
		}},
		Usage: &schemas.BifrostLLMUsage{PromptTokens: promptTokens, CompletionTokens: 1, TotalTokens: promptTokens + 1},
	}
}

// scriptedChatTurns answers each dispatch with the next scripted response and records
// every request the loop sent.
type scriptedChatTurns struct {
	responses []*schemas.BifrostChatResponse
	sent      []*schemas.BifrostChatRequest
}

func (s *scriptedChatTurns) dispatch(req *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	s.sent = append(s.sent, req)
	resp := s.responses[0]
	s.responses = s.responses[1:]
	return resp, nil
}

func searchResult(call schemas.ChatAssistantMessageToolCall) *schemas.ChatMessage {
	return &schemas.ChatMessage{
		Role:            schemas.ChatMessageRoleTool,
		Content:         &schemas.ChatMessageContent{ContentStr: schemas.Ptr("results for " + *call.ID)},
		ChatToolMessage: &schemas.ChatToolMessage{ToolCallID: call.ID},
	}
}

func responsesFunctionCall(callID, name string) schemas.ResponsesMessage {
	return schemas.ResponsesMessage{
		ID:   schemas.Ptr("fc_" + callID),
		Type: schemas.Ptr(schemas.ResponsesMessageTypeFunctionCall),
		ResponsesToolMessage: &schemas.ResponsesToolMessage{
			CallID: schemas.Ptr(callID), Name: schemas.Ptr(name), Arguments: schemas.Ptr(`{"query":"q"}`),
		},
	}
}

func responsesText(text string) schemas.ResponsesMessage {
	return schemas.ResponsesMessage{
		Type:    schemas.Ptr(schemas.ResponsesMessageTypeMessage),
		Role:    schemas.Ptr(schemas.ResponsesInputMessageRoleAssistant),
		Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr(text)},
	}
}

// An allowed_tools choice in required mode is relaxed by mode only: its set is the
// client's constraint on which tools may run, and must survive into later turns.
func TestRelaxForcedChoice_AllowedToolsKeepsItsSet(t *testing.T) {
	chatSet := []schemas.ChatToolChoiceAllowedToolsTool{{Type: "function", Function: schemas.ChatToolChoiceFunction{Name: "get_weather"}}}
	chatIn := &schemas.ChatParameters{ToolChoice: &schemas.ChatToolChoice{ChatToolChoiceStruct: &schemas.ChatToolChoiceStruct{
		Type:         schemas.ChatToolChoiceTypeAllowedTools,
		AllowedTools: &schemas.ChatToolChoiceAllowedTools{Mode: "required", Tools: chatSet},
	}}}
	chat := relaxForcedChatChoice(chatIn)
	require.NotNil(t, chat.ToolChoice.ChatToolChoiceStruct, "the allowed set must not collapse to a bare auto")
	allowed := chat.ToolChoice.ChatToolChoiceStruct.AllowedTools
	require.NotNil(t, allowed)
	assert.Equal(t, "auto", allowed.Mode)
	assert.Equal(t, chatSet, allowed.Tools)
	assert.Equal(t, "required", chatIn.ToolChoice.ChatToolChoiceStruct.AllowedTools.Mode, "the turn's own params are not written")

	respSet := []schemas.ResponsesToolChoiceAllowedToolDef{{Type: "function", Name: schemas.Ptr("get_weather")}}
	respIn := &schemas.ResponsesParameters{ToolChoice: &schemas.ResponsesToolChoice{ResponsesToolChoiceStruct: &schemas.ResponsesToolChoiceStruct{
		Type:  schemas.ResponsesToolChoiceTypeAllowedTools,
		Mode:  schemas.Ptr("required"),
		Tools: respSet,
	}}}
	resp := relaxForcedResponsesChoice(respIn)
	require.NotNil(t, resp.ToolChoice.ResponsesToolChoiceStruct, "the allowed set must not collapse to a bare auto")
	assert.Equal(t, schemas.ResponsesToolChoiceTypeAllowedTools, resp.ToolChoice.ResponsesToolChoiceStruct.Type)
	assert.Equal(t, "auto", *resp.ToolChoice.ResponsesToolChoiceStruct.Mode)
	assert.Equal(t, respSet, resp.ToolChoice.ResponsesToolChoiceStruct.Tools)
	assert.Equal(t, "required", *respIn.ToolChoice.ResponsesToolChoiceStruct.Mode, "the turn's own params are not written")
}

func deferredToolsResponsesRequest(provider schemas.ModelProvider, model string) *schemas.BifrostRequest {
	tool := func(name string, deferred bool) schemas.ResponsesTool {
		t := schemas.ResponsesTool{
			Type: schemas.ResponsesToolTypeFunction, Name: schemas.Ptr(name), Description: schemas.Ptr(name + " tool"),
			ResponsesToolFunction: &schemas.ResponsesToolFunction{},
		}
		if deferred {
			t.DeferLoading = schemas.Ptr(true)
		}
		return t
	}
	return &schemas.BifrostRequest{
		RequestType: schemas.ResponsesRequest,
		ResponsesRequest: &schemas.BifrostResponsesRequest{
			Provider: provider,
			Model:    model,
			Input: []schemas.ResponsesMessage{{
				Role:    schemas.Ptr(schemas.ResponsesInputMessageRoleUser),
				Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("weather in Tokyo?")},
			}},
			Params: &schemas.ResponsesParameters{Tools: []schemas.ResponsesTool{tool("lookup_city", false), tool("get_weather", true)}},
		},
	}
}

// The gateway tool search joins an attempt only when it defers tools on a provider
// with no tool search of its own, and rides alongside any configured MCP tools.
func TestWithGatewayToolSearch(t *testing.T) {
	gemini := deferredToolsResponsesRequest(schemas.Gemini, "gemini-3.8-flash")
	set := withGatewayToolSearch(nil, gemini, schemas.Gemini, "gemini-3.8-flash")
	require.NotNil(t, set)
	require.NotNil(t, set.search)
	assert.True(t, set.isInjected("bifrost_tool_search"))
	assert.Equal(t, 3, set.maxDepth)

	assert.Nil(t, withGatewayToolSearch(nil, deferredToolsResponsesRequest(schemas.OpenAI, "gpt-6-sol"), schemas.OpenAI, "gpt-6-sol"),
		"gpt-6-sol searches natively")
	assert.Nil(t, withGatewayToolSearch(nil, deferredToolsResponsesRequest(schemas.Anthropic, "claude-sonnet-4-6"), schemas.Anthropic, "claude-sonnet-4-6"),
		"Claude searches natively")

	stream := deferredToolsResponsesRequest(schemas.Gemini, "gemini-3.8-flash")
	stream.RequestType = schemas.ResponsesStreamRequest
	assert.NotNil(t, withGatewayToolSearch(nil, stream, schemas.Gemini, "gemini-3.8-flash"), "streaming attempts search too")
	chat := deferredToolsResponsesRequest(schemas.Gemini, "gemini-3.8-flash")
	chat.RequestType = schemas.ChatCompletionRequest
	assert.Nil(t, withGatewayToolSearch(nil, chat, schemas.Gemini, "gemini-3.8-flash"), "chat requests carry no deferred tools")

	eager := deferredToolsResponsesRequest(schemas.Gemini, "gemini-3.8-flash")
	eager.ResponsesRequest.Params.Tools[1].DeferLoading = nil
	assert.Nil(t, withGatewayToolSearch(nil, eager, schemas.Gemini, "gemini-3.8-flash"), "nothing deferred")

	mcp := &injectedToolSet{tools: []schemas.ChatTool{{Type: schemas.ChatToolTypeFunction, Function: &schemas.ChatToolFunction{Name: "web_search"}}}, maxDepth: 7}
	combined := withGatewayToolSearch(mcp, gemini, schemas.Gemini, "gemini-3.8-flash")
	assert.Same(t, mcp, combined)
	assert.Len(t, combined.tools, 2)
	assert.Equal(t, 7, combined.maxDepth, "the configured agent depth is kept")
}

// The loop loads what the search found into the next turn, and the caller sees one
// response without the search call.
func TestRunInjectedResponsesLoop_GatewayToolSearchLoadsTools(t *testing.T) {
	req := deferredToolsResponsesRequest(schemas.Gemini, "gemini-3.8-flash")
	set := withGatewayToolSearch(nil, req, schemas.Gemini, "gemini-3.8-flash")
	require.NotNil(t, set)

	call := func(callID, name, args string) schemas.ResponsesMessage {
		return schemas.ResponsesMessage{
			Type:                 schemas.Ptr(schemas.ResponsesMessageTypeFunctionCall),
			ResponsesToolMessage: &schemas.ResponsesToolMessage{CallID: schemas.Ptr(callID), Name: schemas.Ptr(name), Arguments: schemas.Ptr(args)},
		}
	}
	var turns []*schemas.BifrostResponsesRequest
	resp, bifrostErr := runInjectedResponsesLoop(req.ResponsesRequest, set, func(turn *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
		turns = append(turns, turn)
		if len(turns) == 1 {
			return &schemas.BifrostResponsesResponse{Output: []schemas.ResponsesMessage{call("call_s1", "bifrost_tool_search", `{"query":"weather"}`)}}, nil
		}
		return &schemas.BifrostResponsesResponse{Output: []schemas.ResponsesMessage{call("call_w1", "get_weather", `{"city":"Tokyo"}`)}}, nil
	}, func(c schemas.ChatAssistantMessageToolCall) *schemas.ChatMessage {
		return set.search.Execute(c)
	})
	require.Nil(t, bifrostErr)
	require.Len(t, turns, 2)
	toolNames := func(r *schemas.BifrostResponsesRequest) []string {
		var names []string
		for _, tool := range r.Params.Tools {
			names = append(names, *tool.Name)
		}
		return names
	}
	assert.Equal(t, []string{"lookup_city", "bifrost_tool_search"}, toolNames(turns[0]))
	assert.Equal(t, []string{"lookup_city", "get_weather", "bifrost_tool_search"}, toolNames(turns[1]))
	require.Len(t, resp.Output, 1)
	assert.Equal(t, "get_weather", *resp.Output[0].Name)
	assert.True(t, *req.ResponsesRequest.Params.Tools[1].DeferLoading, "the caller's request is untouched")
}

// The per-attempt check every Responses request pays when no tool is deferred.
func BenchmarkWithGatewayToolSearch_NoDeferredTools(b *testing.B) {
	req := deferredToolsResponsesRequest(schemas.Gemini, "gemini-3.8-flash")
	req.ResponsesRequest.Params.Tools[1].DeferLoading = nil
	b.ReportAllocs()
	for b.Loop() {
		if withGatewayToolSearch(nil, req, schemas.Gemini, "gemini-3.8-flash") != nil {
			b.Fatal("unexpected tool search")
		}
	}
}
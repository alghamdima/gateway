package bifrost

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type chatDelta struct {
	role, text, finish string
	call               *schemas.ChatAssistantMessageToolCall
	usage              *schemas.BifrostLLMUsage
	noChoice           bool
}

func chatStreamChunk(id string, d chatDelta) *schemas.BifrostChatResponse {
	resp := &schemas.BifrostChatResponse{ID: id, Created: 100, Model: "m", Object: "chat.completion.chunk", Usage: d.usage}
	if d.noChoice {
		return resp
	}
	delta := &schemas.ChatStreamResponseChoiceDelta{}
	if d.role != "" {
		delta.Role = schemas.Ptr(d.role)
	}
	if d.text != "" {
		delta.Content = schemas.Ptr(d.text)
	}
	if d.call != nil {
		delta.ToolCalls = []schemas.ChatAssistantMessageToolCall{*d.call}
	}
	choice := schemas.BifrostResponseChoice{ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{Delta: delta}}
	if d.finish != "" {
		choice.FinishReason = schemas.Ptr(d.finish)
	}
	resp.Choices = []schemas.BifrostResponseChoice{choice}
	return resp
}

func callStart(index uint16, id, name string) *schemas.ChatAssistantMessageToolCall {
	return &schemas.ChatAssistantMessageToolCall{
		Index: index, ID: schemas.Ptr(id), Type: schemas.Ptr("function"),
		Function: schemas.ChatAssistantMessageToolCallFunction{Name: schemas.Ptr(name)},
	}
}

func callArgs(index uint16, fragment string) *schemas.ChatAssistantMessageToolCall {
	return &schemas.ChatAssistantMessageToolCall{Index: index, Function: schemas.ChatAssistantMessageToolCallFunction{Arguments: fragment}}
}

func usage(prompt int) *schemas.BifrostLLMUsage {
	return &schemas.BifrostLLMUsage{PromptTokens: prompt, CompletionTokens: 1, TotalTokens: prompt + 1}
}

// feed runs a turn's chunks through the session; the last chunk is the stream's final one.
func feed(s *injectedChatStream, id string, deltas ...chatDelta) []streamEmit {
	var out []streamEmit
	for i, d := range deltas {
		out = append(out, s.onChunk(chatStreamChunk(id, d), i == len(deltas)-1)...)
	}
	return out
}

func emittedText(emits []streamEmit) string {
	var text string
	for _, e := range emits {
		if d := e.resp.Choices; len(d) > 0 && d[0].Delta != nil && d[0].Delta.Content != nil {
			text += *d[0].Delta.Content
		}
	}
	return text
}

// gateCountingTracer counts every chunk that passes through the pause/resume gate.
type gateCountingTracer struct {
	schemas.NoOpTracer
	sends atomic.Int32
}

func (t *gateCountingTracer) GateSend(traceID string, chunk *schemas.BifrostStreamChunk, isFinal, isHardErr bool, ch chan *schemas.BifrostStreamChunk, ctx *schemas.BifrostContext) bool {
	t.sends.Add(1)
	return t.NoOpTracer.GateSend(traceID, chunk, isFinal, isHardErr, ch, ctx)
}

// A provider gates every chunk it sends on its own channel. Gating it again on the way
// to the client would buffer or replay it twice, so the pump forwards it as is.
func TestForwardUpstreamChunks_DoesNotGateTwice(t *testing.T) {
	tracer := &gateCountingTracer{}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyStreamGated, true)
	ctx.SetValue(schemas.BifrostContextKeyTracer, schemas.Tracer(tracer))
	ctx.SetValue(schemas.BifrostContextKeyTraceID, "trace")

	upstream := make(chan *schemas.BifrostStreamChunk, 2)
	upstream <- &schemas.BifrostStreamChunk{}
	upstream <- &schemas.BifrostStreamChunk{}
	close(upstream)
	out := make(chan *schemas.BifrostStreamChunk, 2)
	forwardUpstreamChunks(ctx, upstream, out)

	assert.Len(t, out, 2, "every provider chunk reaches the client")
	assert.Zero(t, tracer.sends.Load(), "chunks the provider already gated are not gated again")
}

type responsesEvents struct {
	seq    int
	respID string
}

func (r *responsesEvents) ev(typ schemas.ResponsesStreamResponseType) *schemas.BifrostResponsesStreamResponse {
	e := &schemas.BifrostResponsesStreamResponse{Type: typ, SequenceNumber: r.seq}
	r.seq++
	return e
}

func (r *responsesEvents) created() *schemas.BifrostResponsesStreamResponse {
	e := r.ev(schemas.ResponsesStreamResponseTypeCreated)
	e.Response = &schemas.BifrostResponsesResponse{ID: schemas.Ptr(r.respID)}
	return e
}

func (r *responsesEvents) item(typ schemas.ResponsesStreamResponseType, index int, item schemas.ResponsesMessage) *schemas.BifrostResponsesStreamResponse {
	e := r.ev(typ)
	e.OutputIndex = schemas.Ptr(index)
	e.Item = &item
	return e
}

func (r *responsesEvents) textDelta(index int, itemID, text string) *schemas.BifrostResponsesStreamResponse {
	e := r.ev(schemas.ResponsesStreamResponseTypeOutputTextDelta)
	e.OutputIndex = schemas.Ptr(index)
	e.ItemID = schemas.Ptr(itemID)
	e.Delta = schemas.Ptr(text)
	return e
}

func (r *responsesEvents) argsDelta(index int, itemID, args string) *schemas.BifrostResponsesStreamResponse {
	e := r.ev(schemas.ResponsesStreamResponseTypeFunctionCallArgumentsDelta)
	e.OutputIndex = schemas.Ptr(index)
	e.ItemID = schemas.Ptr(itemID)
	e.Delta = schemas.Ptr(args)
	return e
}

func (r *responsesEvents) completed(input int) *schemas.BifrostResponsesStreamResponse {
	e := r.ev(schemas.ResponsesStreamResponseTypeCompleted)
	e.Response = &schemas.BifrostResponsesResponse{
		ID:    schemas.Ptr(r.respID),
		Usage: &schemas.ResponsesResponseUsage{InputTokens: input, OutputTokens: 1, TotalTokens: input + 1},
	}
	return e
}

func textItem(id, text string) schemas.ResponsesMessage {
	m := responsesText(text)
	m.ID = schemas.Ptr(id)
	return m
}

func feedResponses(s *injectedResponsesStream, events ...*schemas.BifrostResponsesStreamResponse) []responsesEmit {
	var out []responsesEmit
	for i, e := range events {
		out = append(out, s.onEvent(e, i == len(events)-1)...)
	}
	return out
}

// A streaming Responses request that defers tools on gpt-4o (no native tool search)
// goes through the gateway search: the first upstream turn searches, the second
// carries the loaded tool, and the client stream shows neither the search call nor a
// second response.created.
func TestResponsesStream_GatewayToolSearch(t *testing.T) {
	searchTurn := sseHandler(
		`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"gpt-4o","output":[]}}`,
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"fc_1","type":"function_call","status":"in_progress","call_id":"call_s1","name":"bifrost_tool_search","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","sequence_number":2,"output_index":0,"item_id":"fc_1","delta":"{\"query\":\"weather\"}"}`,
		`{"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_s1","name":"bifrost_tool_search","arguments":"{\"query\":\"weather\"}"}}`,
		`{"type":"response.completed","sequence_number":4,"response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-4o","output":[{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_s1","name":"bifrost_tool_search","arguments":"{\"query\":\"weather\"}"}],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}`,
	)
	answerTurn := sseHandler(
		`{"type":"response.created","sequence_number":0,"response":{"id":"resp_2","object":"response","created_at":2,"status":"in_progress","model":"gpt-4o","output":[]}}`,
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"msg_2","type":"message","status":"in_progress","role":"assistant","content":[]}}`,
		`{"type":"response.output_text.delta","sequence_number":2,"output_index":0,"content_index":0,"item_id":"msg_2","delta":"It is sunny."}`,
		`{"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":{"id":"msg_2","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"It is sunny.","annotations":[]}]}}`,
		`{"type":"response.completed","sequence_number":4,"response":{"id":"resp_2","object":"response","created_at":2,"status":"completed","model":"gpt-4o","output":[{"id":"msg_2","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"It is sunny.","annotations":[]}]}],"usage":{"input_tokens":20,"output_tokens":3,"total_tokens":23}}}`,
	)
	recorder := &recordingServer{}
	recorder.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if recorder.record(string(body)) == 1 {
			searchTurn(w, r)
			return
		}
		answerTurn(w, r)
	}))
	defer recorder.Close()

	account := NewMockAccount()
	configureUpstream(account, schemas.OpenAI, "openai-key", recorder.URL, 0)
	client := newStreamTestClient(t, account)

	req := deferredToolsResponsesRequest(schemas.OpenAI, "gpt-4o").ResponsesRequest
	ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(10*time.Second))
	stream, bifrostErr := client.ResponsesStreamRequest(ctx, req)
	require.Nil(t, bifrostErr)

	var text strings.Builder
	var created, completed int
	var final *schemas.BifrostResponsesResponse
	for chunk := range stream {
		require.Nil(t, chunk.BifrostError, "unexpected stream error")
		ev := chunk.BifrostResponsesStreamResponse
		if ev == nil {
			continue
		}
		if ev.Item != nil && ev.Item.ResponsesToolMessage != nil && ev.Item.Name != nil {
			assert.NotEqual(t, "bifrost_tool_search", *ev.Item.Name, "the search call reached the client")
		}
		switch ev.Type {
		case schemas.ResponsesStreamResponseTypeCreated:
			created++
		case schemas.ResponsesStreamResponseTypeOutputTextDelta:
			if ev.Delta != nil {
				text.WriteString(*ev.Delta)
			}
		case schemas.ResponsesStreamResponseTypeCompleted:
			completed++
			final = ev.Response
		}
	}

	bodies := recorder.snapshot()
	require.Len(t, bodies, 2, "want a search turn and an answer turn")
	names := func(body string) []string {
		var out []string
		for _, tool := range gjson.Get(body, "tools").Array() {
			out = append(out, tool.Get("name").String())
		}
		return out
	}
	assert.Equal(t, []string{"lookup_city", "bifrost_tool_search"}, names(bodies[0]))
	assert.Equal(t, []string{"lookup_city", "get_weather", "bifrost_tool_search"}, names(bodies[1]))
	assert.Contains(t, gjson.Get(bodies[1], `input.#(type=="function_call_output").output`).String(), "get_weather")

	assert.Equal(t, "It is sunny.", text.String())
	assert.Equal(t, 1, created, "one client response")
	assert.Equal(t, 1, completed)
	require.NotNil(t, final)
	require.NotNil(t, final.Usage)
	assert.Equal(t, 30, final.Usage.InputTokens, "usage summed across turns")
}

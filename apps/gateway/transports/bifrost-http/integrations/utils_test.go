package integrations

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/providers/gemini"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// testLogger implements schemas.Logger for testing (all no-ops)
type testLogger struct{}

func (t *testLogger) Debug(msg string, args ...any)                     {}
func (t *testLogger) Info(msg string, args ...any)                      {}
func (t *testLogger) Warn(msg string, args ...any)                      {}
func (t *testLogger) Error(msg string, args ...any)                     {}
func (t *testLogger) Fatal(msg string, args ...any)                     {}
func (t *testLogger) SetLevel(level schemas.LogLevel)                   {}
func (t *testLogger) SetOutputType(outputType schemas.LoggerOutputType) {}
func (t *testLogger) LogHTTPRequest(level schemas.LogLevel, msg string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

var _ schemas.Logger = (*testLogger)(nil)

func ptr(i int) *int {
	return &i
}

func newTestGenericRouter() *GenericRouter {
	return NewGenericRouter(nil, &mockHandlerStore{}, nil, nil, nil, &testLogger{})
}

func newTestBifrostContext() *schemas.BifrostContext {
	return schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
}

func TestExtractAndParseFallbacks_GeminiGenerationRequest(t *testing.T) {
	router := newTestGenericRouter()
	geminiReq := &gemini.GeminiGenerationRequest{
		Model:     "gemini/gemini-3-flash-preview",
		Fallbacks: []string{"vertex/gemini-3-flash-preview"},
	}
	bifrostReq := &schemas.BifrostRequest{
		ResponsesRequest: &schemas.BifrostResponsesRequest{
			Provider: schemas.Gemini,
			Model:    "gemini-3-flash-preview",
		},
	}

	err := router.extractAndParseFallbacks(newTestBifrostContext(), geminiReq, bifrostReq)

	require.NoError(t, err)
	require.NotNil(t, bifrostReq.ResponsesRequest)
	require.Len(t, bifrostReq.ResponsesRequest.Fallbacks, 1)
	assert.Equal(t, schemas.Vertex, bifrostReq.ResponsesRequest.Fallbacks[0].Provider)
	assert.Equal(t, "gemini-3-flash-preview", bifrostReq.ResponsesRequest.Fallbacks[0].Model)
}

// TestJSONErrorsPreserveBodyForBodylessStatus verifies that an upstream status
// that cannot carry a body is answered as 502 with the serialized error.
func TestJSONErrorsPreserveBodyForBodylessStatus(t *testing.T) {
	router := newTestGenericRouter()
	converter := func(_ *schemas.BifrostContext, err *schemas.BifrostError) interface{} {
		return err
	}
	for _, code := range []int{
		fasthttp.StatusContinue,
		fasthttp.StatusSwitchingProtocols,
		fasthttp.StatusEarlyHints,
		fasthttp.StatusNoContent,
		fasthttp.StatusResetContent,
		fasthttp.StatusNotModified,
	} {
		ctx := &fasthttp.RequestCtx{}
		bifrostCtx := newTestBifrostContext()
		bifrostErr := &schemas.BifrostError{
			StatusCode: ptr(code),
			Error:      &schemas.ErrorField{Message: "provider API error"},
		}
		router.sendError(ctx, bifrostCtx, converter, bifrostErr)
		assert.Equal(t, fasthttp.StatusBadGateway, ctx.Response.StatusCode(), "upstream=%d", code)
		assert.Equal(t, "application/json", string(ctx.Response.Header.ContentType()))
		assert.True(t, sonic.Valid(ctx.Response.Body()))
		assert.Contains(t, ctx.Response.String(), "provider API error",
			"serialized error body missing: upstream=%d", code)
	}
}

// TestTryStreamLargeResponse_AppliesRoutedIdentityHeaders verifies that
// large-response early returns (speech audio, transcription, image bytes)
// carry the routed-identity headers even though they skip the common footer
// in handleNonStreamingRequest that normally emits them.
func TestTryStreamLargeResponse_AppliesRoutedIdentityHeaders(t *testing.T) {
	router := newTestGenericRouter()
	ctx := &fasthttp.RequestCtx{}
	bifrostCtx := newTestBifrostContext()
	bifrostCtx.SetValue(schemas.BifrostContextKeyLargeResponseMode, true)
	bifrostCtx.SetValue(schemas.BifrostContextKeyLargeResponseReader, io.NopCloser(strings.NewReader("audio-bytes")))
	// This branch forwards provider headers after writing the routed identity, so a chained
	// upstream Bifrost's x-bifrost-* headers must not replace it.
	bifrostCtx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, map[string]string{
		"X-Bifrost-Provider":         "upstream-provider",
		"X-Bifrost-Routing-Info-Key": "upstream-key",
		"X-Request-Id":               "req_upstream",
	})

	extra := schemas.BifrostResponseExtraFields{
		Provider:          schemas.OpenAI,
		ResolvedModelUsed: "tts-1",
		RequestType:       schemas.SpeechRequest,
		RoutingInfo: schemas.RoutingInfo{
			Provider: schemas.OpenAI,
			Model:    "tts-1",
			Key:      "openai-key",
		},
	}

	handled := router.tryStreamLargeResponse(ctx, bifrostCtx, extra)

	require.True(t, handled, "large response mode active — call must handle the response")
	assert.Equal(t, "openai", string(ctx.Response.Header.Peek(lib.HeaderBifrostProvider)))
	assert.Equal(t, "tts-1", string(ctx.Response.Header.Peek(lib.HeaderBifrostResolvedModel)))
	assert.Equal(t, string(schemas.SpeechRequest), string(ctx.Response.Header.Peek(lib.HeaderBifrostRequestType)))
	assert.Equal(t, "openai", string(ctx.Response.Header.Peek(lib.HeaderBifrostRoutingInfoProvider)))
	assert.Equal(t, "tts-1", string(ctx.Response.Header.Peek(lib.HeaderBifrostRoutingInfoModel)))
	assert.Equal(t, "openai-key", string(ctx.Response.Header.Peek(lib.HeaderBifrostRoutingInfoKey)))
	assert.Equal(t, "req_upstream", string(ctx.Response.Header.Peek("x-request-id")))
}

// TestSendHelpers_ExtraFieldsPlacement pins client.extra_fields_placement on the integration
// routes: body mode keeps extra_fields in success and error bodies, headers mode drops it.
func TestSendHelpers_ExtraFieldsPlacement(t *testing.T) {
	router := newTestGenericRouter()
	converter := func(_ *schemas.BifrostContext, err *schemas.BifrostError) interface{} {
		return err
	}
	resp := &schemas.BifrostChatResponse{
		ID:          "chatcmpl-1",
		ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.ChatCompletionRequest, Provider: schemas.OpenAI},
	}
	newErr := func() *schemas.BifrostError {
		return &schemas.BifrostError{
			StatusCode:  ptr(fasthttp.StatusBadRequest),
			Error:       &schemas.ErrorField{Message: "bad"},
			ExtraFields: schemas.BifrostErrorExtraFields{Provider: schemas.OpenAI},
		}
	}

	for _, inHeaders := range []bool{false, true} {
		newCtx := func() *fasthttp.RequestCtx {
			ctx := &fasthttp.RequestCtx{}
			if inHeaders {
				ctx.SetUserValue(lib.FastHTTPUserValueExtraFieldsInHeaders, true)
			}
			return ctx
		}

		success := newCtx()
		router.sendSuccess(success, newTestBifrostContext(), converter, resp, nil)
		assert.Contains(t, string(success.Response.Body()), `"id":"chatcmpl-1"`)
		assert.Equal(t, !inHeaders, strings.Contains(string(success.Response.Body()), `"extra_fields"`), "success headers=%v", inHeaders)

		errCtx := newCtx()
		router.sendError(errCtx, newTestBifrostContext(), converter, newErr())
		assert.Equal(t, !inHeaders, strings.Contains(string(errCtx.Response.Body()), `"extra_fields"`), "error headers=%v", inHeaders)
	}
}

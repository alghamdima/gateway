package credits

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// The hold must be the most an attempt can cost (ADR 0009): what multiplies
// the cost is counted, and what the estimate cannot bound is refused before
// the provider is called. testPricer: 1 micro-USD per input token, 2 per output
// token, at most 1000 output tokens.

func chatWith(edit func(*schemas.BifrostChatRequest)) *schemas.BifrostRequest {
	req := chatRequest(schemas.ChatCompletionRequest, nil)
	if req.ChatRequest.Params == nil {
		req.ChatRequest.Params = &schemas.ChatParameters{}
	}
	edit(req.ChatRequest)
	return req
}

func holdFor(t *testing.T, ctx *schemas.BifrostContext, req *schemas.BifrostRequest) int64 {
	t.Helper()
	ledger := &fakeLedger{balance: 1000 * usd}
	if _, sc, err := testPlugin(ledger, testPricer()).PreLLMHook(ctx, req); err != nil || sc != nil {
		t.Fatalf("refused: %v %+v", err, sc)
	}
	return ledger.reserved[0]
}

func TestBounds_NAndBestOfMultiplyTheOutputHeld(t *testing.T) {
	one := holdFor(t, testContext(testOrg), chatWith(func(*schemas.BifrostChatRequest) {}))
	eight := holdFor(t, testContext(testOrg), chatWith(func(r *schemas.BifrostChatRequest) { r.Params.N = ptr(8) }))
	// 1000 output tokens at 2 micro-USD each, eight times, plus the same few input tokens
	if eight-one < 7*2000 || eight-one > 7*2000+50 {
		t.Fatalf("n=8 held %d, n=1 held %d: want 14,000 more", eight, one)
	}
}

func TestBounds_EachImageIsHeldAtItsCeilingWhateverItsSize(t *testing.T) {
	text := "describe these"
	images := chatWith(func(r *schemas.BifrostChatRequest) {
		blocks := []schemas.ChatContentBlock{{Type: schemas.ChatContentBlockTypeText, Text: &text}}
		for range 3 {
			blocks = append(blocks, schemas.ChatContentBlock{
				Type:           schemas.ChatContentBlockTypeImage,
				ImageURLStruct: &schemas.ChatInputImage{URL: "https://example.com/a.png"},
			})
		}
		r.Input = []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentBlocks: blocks}}}
	})
	plain := holdFor(t, testContext(testOrg), chatWith(func(*schemas.BifrostChatRequest) {}))
	held := holdFor(t, testContext(testOrg), images)
	if held-plain < 3*imagePartTokens {
		t.Fatalf("three images held %d more than text alone, want at least %d", held-plain, 3*imagePartTokens)
	}
}

func TestBounds_RefusesWhatTheEstimateCannotBound(t *testing.T) {
	webSearch := schemas.ChatTool{Type: schemas.ChatToolType("web_search")}
	cases := []struct {
		name string
		ctx  func(*schemas.BifrostContext)
		req  *schemas.BifrostRequest
	}{
		{"a priority service tier", nil, chatWith(func(r *schemas.BifrostChatRequest) {
			r.Params.ServiceTier = ptr(schemas.BifrostServiceTierPriority)
		})},
		{"fast mode", nil, chatWith(func(r *schemas.BifrostChatRequest) { r.Params.Speed = ptr("fast") })},
		{"a chosen inference region", nil, chatWith(func(r *schemas.BifrostChatRequest) { r.Params.InferenceGeo = ptr("us") })},
		{"extra provider parameters", nil, chatWith(func(r *schemas.BifrostChatRequest) {
			r.Params.ExtraParams = map[string]any{"max_tokens": 64000}
		})},
		{"a tool the provider runs", nil, chatWith(func(r *schemas.BifrostChatRequest) { r.Params.Tools = []schemas.ChatTool{webSearch} })},
		{"provider web search", nil, chatWith(func(r *schemas.BifrostChatRequest) {
			r.Params.WebSearchOptions = &schemas.ChatWebSearchOptions{}
		})},
		{"a file sent by URL", nil, chatWith(func(r *schemas.BifrostChatRequest) {
			r.Input = []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{{
				Type: schemas.ChatContentBlockTypeFile, File: &schemas.ChatInputFile{FileURL: ptr("https://example.com/book.pdf")},
			}}}}}
		})},
		{"a body sent to the provider unchanged", func(c *schemas.BifrostContext) {
			c.SetValue(schemas.BifrostContextKeyUseRawRequestBody, true)
		}, chatWith(func(*schemas.BifrostChatRequest) {})},
		{"large payload mode", func(c *schemas.BifrostContext) {
			c.SetValue(schemas.BifrostContextKeyLargePayloadMode, true)
		}, chatWith(func(*schemas.BifrostChatRequest) {})},
		{"extra parameters passed through by header", func(c *schemas.BifrostContext) {
			c.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
		}, chatWith(func(*schemas.BifrostChatRequest) {})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ledger := &fakeLedger{balance: 1000 * usd}
			ctx := testContext(testOrg)
			if c.ctx != nil {
				c.ctx(ctx)
			}
			_, sc, err := testPlugin(ledger, testPricer()).PreLLMHook(ctx, c.req)
			if err != nil {
				t.Fatal(err)
			}
			status, errorType := refusalOf(t, sc)
			if status != 400 || errorType != ErrorTypeUnsupportedRequest {
				t.Fatalf("refused with %d %s, want 400 %s", status, errorType, ErrorTypeUnsupportedRequest)
			}
			if ledger.reserveCall.Load() != 0 {
				t.Error("a refused request reached the ledger")
			}
		})
	}
}

func TestBounds_AllowsWhatItCanBound(t *testing.T) {
	data := "JVBERi0xLjQK"
	for name, req := range map[string]*schemas.BifrostRequest{
		"the default tier": chatWith(func(r *schemas.BifrostChatRequest) {
			r.Params.ServiceTier = ptr(schemas.BifrostServiceTierDefault)
		}),
		"standard speed and the global region": chatWith(func(r *schemas.BifrostChatRequest) {
			r.Params.Speed = ptr("standard")
			r.Params.InferenceGeo = ptr("global")
		}),
		"a function tool": chatWith(func(r *schemas.BifrostChatRequest) {
			r.Params.Tools = []schemas.ChatTool{{Type: schemas.ChatToolTypeFunction, Function: &schemas.ChatToolFunction{Name: "lookup"}}}
		}),
		"a file sent inline": chatWith(func(r *schemas.BifrostChatRequest) {
			r.Input = []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{{
				Type: schemas.ChatContentBlockTypeFile, File: &schemas.ChatInputFile{FileData: &data},
			}}}}}
		}),
	} {
		t.Run(name, func(t *testing.T) {
			if held := holdFor(t, testContext(testOrg), req); held <= 0 {
				t.Fatalf("held %d", held)
			}
		})
	}
}

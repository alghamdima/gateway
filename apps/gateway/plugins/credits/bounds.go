package credits

import (
	"encoding/json"
	"fmt"

	"github.com/maximhq/bifrost/core/schemas"
)

// What the estimate cannot bound is refused, never guessed (ADR 0009). The
// hold must be the most an attempt can cost, so anything that lets the
// provider do more than the measured request is either counted or refused.

// imagePartTokens is the most one image part is counted as, whatever its
// size in the request: a URL is a few bytes but an image costs up to about
// 1,600 tokens on the providers we serve.
const imagePartTokens = 2000

// requestShape is what the estimate learns from the measured request body.
type requestShape struct {
	// completions is how many answers one attempt generates (n, best_of).
	completions int
	// imageParts are counted at imagePartTokens each.
	imageParts int
}

// refusal is why a request cannot be bounded; empty when it can.
type refusal string

// unboundableError is an estimate refused for a reason the caller can act on.
type unboundableError struct{ reason refusal }

func (e unboundableError) Error() string { return string(e.reason) }

// unboundable reports why a request cannot be held for, from the context the
// transport built: a body sent to the provider as it came, or extra fields
// merged into it, is not the body that was measured.
func unboundable(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) refusal {
	if flag, _ := ctx.Value(schemas.BifrostContextKeyUseRawRequestBody).(bool); flag {
		return "Requests passed through to the provider unchanged are not available."
	}
	if flag, _ := ctx.Value(schemas.BifrostContextKeyLargePayloadMode).(bool); flag {
		return "Requests this large are not available."
	}
	if flag, _ := ctx.Value(schemas.BifrostContextKeyPassthroughExtraParams).(bool); flag {
		return "Extra provider parameters are not available."
	}
	if extra := extraParamsOf(req); len(extra) > 0 {
		return "Extra provider parameters are not available."
	}
	return ""
}

func extraParamsOf(req *schemas.BifrostRequest) map[string]any {
	switch {
	case req.ChatRequest != nil && req.ChatRequest.Params != nil:
		return req.ChatRequest.Params.ExtraParams
	case req.ResponsesRequest != nil && req.ResponsesRequest.Params != nil:
		return req.ResponsesRequest.Params.ExtraParams
	case req.TextCompletionRequest != nil && req.TextCompletionRequest.Params != nil:
		return req.TextCompletionRequest.Params.ExtraParams
	case req.EmbeddingRequest != nil && req.EmbeddingRequest.Params != nil:
		return req.EmbeddingRequest.Params.ExtraParams
	}
	return nil
}

// shapeOf walks the measured body. It reads the parameters that multiply the
// cost, counts image parts, and refuses what has a cost of its own that the
// estimate does not know: priority tiers and fast mode, content referenced by
// URL or file id that is not an image, and tools the provider runs itself.
func shapeOf(encoded []byte) (requestShape, refusal, error) {
	var body any
	if err := json.Unmarshal(encoded, &body); err != nil {
		return requestShape{}, "", fmt.Errorf("read measured request: %w", err)
	}
	shape := requestShape{completions: 1}
	var reason refusal
	walk(body, "", func(key string, node map[string]any) {
		if reason != "" {
			return
		}
		reason = checkNode(key, node, &shape)
	})
	return shape, reason, nil
}

func walk(node any, key string, visit func(key string, node map[string]any)) {
	switch value := node.(type) {
	case map[string]any:
		visit(key, value)
		for child, nested := range value {
			walk(nested, child, visit)
		}
	case []any:
		for _, item := range value {
			walk(item, key, visit)
		}
	}
}

var (
	// tiers billed at the standard rate; any other is priced differently at settlement
	standardTiers = map[string]bool{"": true, "auto": true, "default": true}
	// tool types the model calls and the customer runs: no provider-side cost
	clientTools = map[string]bool{"function": true, "custom": true}
	// content parts referenced rather than inlined, whose size the body does not show
	referencedParts = map[string]bool{
		"file": true, "input_file": true, "video": true, "video_url": true, "input_video": true,
		"document": true, "audio_url": true,
	}
	imageParts = map[string]bool{"image_url": true, "input_image": true, "image": true}
)

func checkNode(key string, node map[string]any, shape *requestShape) refusal {
	for _, name := range []string{"n", "best_of"} {
		if count, ok := node[name].(float64); ok && int(count) > shape.completions {
			shape.completions = int(count)
		}
	}
	if tier, ok := node["service_tier"].(string); ok && !standardTiers[tier] {
		return refusal(fmt.Sprintf("The %q service tier is not available.", tier))
	}
	if speed, ok := node["speed"].(string); ok && speed != "" && speed != "standard" {
		return "Fast mode is not available."
	}
	if geo, ok := node["inference_geo"].(string); ok && geo != "" && geo != "global" {
		return "Choosing the inference region is not available."
	}
	if _, ok := node["mcp_servers"]; ok {
		return "Provider-run MCP servers are not available."
	}
	if options, ok := node["web_search_options"]; ok && options != nil {
		return "Provider-run web search is not available."
	}

	kind, _ := node["type"].(string)
	if key == "tools" && kind != "" && !clientTools[kind] {
		return refusal(fmt.Sprintf("Tools of type %q are not available.", kind))
	}
	if imageParts[kind] {
		shape.imageParts++
	}
	if referencedParts[kind] && !inlined(node) {
		return refusal(fmt.Sprintf("Content of type %q must be sent inline.", kind))
	}
	return ""
}

// inlined reports whether a content part carries its data in the request, so
// that its bytes were measured: base64 data, and no URL or file id.
func inlined(node map[string]any) bool {
	var hasData, hasReference bool
	walk(node, "", func(_ string, part map[string]any) {
		for _, field := range []string{"file_data", "data"} {
			if value, ok := part[field].(string); ok && value != "" {
				hasData = true
			}
		}
		for _, field := range []string{"url", "file_url", "file_id", "video_url"} {
			if value, ok := part[field].(string); ok && value != "" {
				hasReference = true
			}
		}
	})
	return hasData && !hasReference
}

package anthropic

import (
	"context"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// countPrefixed counts entries in a merged beta list belonging to one beta family.
func countPrefixed(betas []string, prefix string) int {
	n := 0
	for _, b := range betas {
		if strings.HasPrefix(b, prefix) {
			n++
		}
	}
	return n
}

// TestAddMissingBetaHeaders_HonoursCallerVersion covers the interaction between auto-injection
// and the caller's own anthropic-beta. The caller pins an older MCP beta; Bifrost's default is
// the current one. The "passthrough wins" guard has to see the caller's value, or it injects its
// own version and MergeBetaHeaders (exact-token dedup, not prefix) forwards BOTH versions of the
// same family, which Anthropic rejects.
func TestAddMissingBetaHeaders_HonoursCallerVersion(t *testing.T) {
	const callerMCPBeta = "mcp-client-2025-04-04" // deliberately older than AnthropicMCPClientBetaHeader

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyExtraHeaders, map[string][]string{
		"anthropic-beta": {callerMCPBeta},
	})

	req := &AnthropicMessageRequest{MCPServers: []AnthropicMCPServerV2{{}}}
	if err := AddMissingBetaHeadersToContext(ctx, req, ""); err != nil {
		t.Fatalf("AddMissingBetaHeadersToContext: %v", err)
	}

	merged := MergeBetaHeaders(ctx, nil)
	if n := countPrefixed(merged, "mcp-client-"); n != 1 {
		t.Fatalf("expected exactly 1 mcp-client beta, got %d: %v", n, merged)
	}
	if merged[0] != callerMCPBeta {
		t.Errorf("caller's pinned version should win: got %q, want %q", merged[0], callerMCPBeta)
	}
}

// TestAddMissingBetaHeaders_StillInjectsWhenCallerSentNone — the guard must not over-trigger:
// with no caller beta at all, auto-injection still has to happen.
func TestAddMissingBetaHeaders_StillInjectsWhenCallerSentNone(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	req := &AnthropicMessageRequest{MCPServers: []AnthropicMCPServerV2{{}}}
	if err := AddMissingBetaHeadersToContext(ctx, req, ""); err != nil {
		t.Fatalf("AddMissingBetaHeadersToContext: %v", err)
	}

	merged := MergeBetaHeaders(ctx, nil)
	if countPrefixed(merged, "mcp-client-") != 1 || merged[0] != AnthropicMCPClientBetaHeader {
		t.Fatalf("expected auto-injected %q, got %v", AnthropicMCPClientBetaHeader, merged)
	}
}

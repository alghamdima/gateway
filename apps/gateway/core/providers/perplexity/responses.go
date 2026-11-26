package perplexity

import (
	"strings"
)

// isPerplexityResponsesSupported reports whether the model should use /v1/responses vs /chat/completions.
// Denylist by design: /chat/completions serves only the Sonar family, so base "sonar" and the sonar-*
// variants go to chat (/v1/responses rejects them with `model "sonar" is not supported`) and every other
// model goes to responses. This keeps newly-shipped non-Sonar models working without a code change;
// they'd fail on chat, which doesn't serve them.
func isPerplexityResponsesSupported(model string) bool {
	bare := strings.ToLower(strings.TrimPrefix(model, "perplexity/"))
	return bare != "sonar" && !strings.HasPrefix(bare, "sonar-")
}

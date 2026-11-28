package governance

import (
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"

	"github.com/maximhq/bifrost/core/schemas"
)

// accessTestVKValue is the value of the virtual key the access fixtures below serve.
const accessTestVKValue = "sk-bf-access-test"

// buildAccessTestVK builds a virtual key allowed every OpenAI model.
func buildAccessTestVK() *configstoreTables.TableVirtualKey {
	return buildVirtualKeyWithProviders(
		"vk-access-test",
		accessTestVKValue,
		"access-test-vk",
		[]configstoreTables.TableVirtualKeyProviderConfig{
			buildProviderConfig("openai", []string{"*"}),
		},
	)
}

// newPreRequestCtx presents only the key, on a context carrying the grant every transport
// installs: PreRequestHook is what resolves access.
func newPreRequestCtx() *schemas.BifrostContext {
	return presentCtx(accessTestVKValue)
}

func newChatRequest() *schemas.BifrostRequest {
	return &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "gpt-4o"},
	}
}

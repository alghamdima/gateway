package bedrock

// NewBedrockResponsesStreamState returns a freshly initialised stream state for the
// tests in package bedrock_test.
func NewBedrockResponsesStreamState() *BedrockResponsesStreamState {
	return acquireBedrockResponsesStreamState()
}

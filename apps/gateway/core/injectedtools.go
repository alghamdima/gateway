package bifrost

import (
	"sync"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/core/toolsearch"
)

// Injected tools are tools Bifrost adds to a Responses request and executes itself when
// the model calls them: today only the gateway tool search (core/toolsearch). The client
// never declares them and never sees their calls.
//
// Everything here is per attempt and copy-on-write: req.BifrostRequest is shared across
// retries and fallbacks, so a fallback to a provider without injected tools must see
// the caller's request exactly as sent, native web search tool included.

// injectedToolSet is the resolved set of tools injected into one attempt.
type injectedToolSet struct {
	tools    []schemas.ChatTool  // function tools named as the model sees them
	maxDepth int                 // model turns the tool loop may run before it stops executing
	search   *toolsearch.Session // gateway-run search over the caller's deferred tools, nil when none
}

func (s *injectedToolSet) isInjected(name string) bool {
	if s == nil {
		return false
	}
	return s.search != nil && name == toolsearch.ToolName
}

// duplicates reports whether a client tool named name must be dropped because it carries
// an injected tool's name. A tool choice pinning a dropped declaration is pointed at the
// injected tool by pinInjectedResponsesChoice.
func (s *injectedToolSet) duplicates(name string) bool {
	return name != "" && s.isInjected(name)
}

// injectedChoiceTarget is the injected tool a function or custom tool choice for name
// must pin once the matching client declaration is dropped, or false when the choice is
// fine as it is.
func injectedChoiceTarget(set *injectedToolSet, name string, isFunction bool) (string, bool) {
	// A function choice already pins the injected function; a custom one must become one.
	return name, set.isInjected(name) && !isFunction
}

// pinInjectedResponsesChoice points a function or custom tool choice whose declaration an
// injected tool replaced at that injected function, and rewrites the entries of an
// allowed_tools choice the same way. Other choices pass through.
func pinInjectedResponsesChoice(choice *schemas.ResponsesToolChoice, set *injectedToolSet) *schemas.ResponsesToolChoice {
	if choice == nil || choice.ResponsesToolChoiceStruct == nil {
		return choice
	}
	s := choice.ResponsesToolChoiceStruct
	if s.Type == schemas.ResponsesToolChoiceTypeAllowedTools {
		return pinInjectedResponsesAllowedTools(choice, set)
	}
	if s.Name == nil || (s.Type != schemas.ResponsesToolChoiceTypeFunction && s.Type != schemas.ResponsesToolChoiceTypeCustom) {
		return choice
	}
	target, ok := injectedChoiceTarget(set, *s.Name, s.Type == schemas.ResponsesToolChoiceTypeFunction)
	if !ok {
		return choice
	}
	return &schemas.ResponsesToolChoice{ResponsesToolChoiceStruct: &schemas.ResponsesToolChoiceStruct{
		Type: schemas.ResponsesToolChoiceTypeFunction,
		Name: new(target),
	}}
}

// pinInjectedResponsesAllowedTools rewrites an allowed_tools choice whose entries name a
// declaration an injected tool dropped, so each names the injected tool, once. The
// caller's choice is returned as is when nothing changes, and is never written.
func pinInjectedResponsesAllowedTools(choice *schemas.ResponsesToolChoice, set *injectedToolSet) *schemas.ResponsesToolChoice {
	s := choice.ResponsesToolChoiceStruct
	changed := false
	seen := make(map[string]struct{}, len(s.Tools))
	tools := make([]schemas.ResponsesToolChoiceAllowedToolDef, 0, len(s.Tools))
	for _, tool := range s.Tools {
		target, ok := "", false
		if tool.Name != nil && (tool.Type == string(schemas.ResponsesToolChoiceTypeFunction) || tool.Type == string(schemas.ResponsesToolChoiceTypeCustom)) {
			target, ok = injectedChoiceTarget(set, *tool.Name, tool.Type == string(schemas.ResponsesToolChoiceTypeFunction))
		}
		if ok {
			tool = schemas.ResponsesToolChoiceAllowedToolDef{Type: string(schemas.ResponsesToolChoiceTypeFunction), Name: new(target)}
			changed = true
		}
		key := tool.Type + "\x00"
		if tool.Name != nil {
			key += *tool.Name
		}
		if tool.ServerLabel != nil {
			key += "\x00" + *tool.ServerLabel
		}
		if _, dup := seen[key]; dup {
			changed = true
			continue
		}
		seen[key] = struct{}{}
		tools = append(tools, tool)
	}
	if !changed {
		return choice
	}
	cp := *s
	cp.Tools = tools
	return &schemas.ResponsesToolChoice{ResponsesToolChoiceStruct: &cp}
}

// applyInjectedToolsResponses returns a copy of r with the injected tools declared and the
// deferred tools held back. r and its Params are never written.
func applyInjectedToolsResponses(r *schemas.BifrostResponsesRequest, set *injectedToolSet) *schemas.BifrostResponsesRequest {
	if r == nil || set == nil {
		return r
	}
	cp := *r
	var params schemas.ResponsesParameters
	if r.Params != nil {
		params = *r.Params
	}
	tools := make([]schemas.ResponsesTool, 0, len(params.Tools)+len(set.tools))
	for _, tool := range params.Tools {
		// A client tool with an injected tool's name is replaced by it.
		if tool.Name != nil && set.duplicates(*tool.Name) {
			continue
		}
		// A deferred tool stays out until the gateway search loads it.
		if set.search != nil {
			var keep bool
			if tool, keep = set.search.FilterTool(tool); !keep {
				continue
			}
		}
		tools = append(tools, tool)
	}
	for i := range set.tools {
		tools = append(tools, *set.tools[i].ToResponsesTool())
	}
	params.Tools = tools
	params.ToolChoice = pinInjectedResponsesChoice(params.ToolChoice, set)
	cp.Params = &params
	return &cp
}

// runInjectedResponsesLoop is the Responses API parallel of runInjectedChatLoop. Every
// output item of an intermediate turn except the client's function calls is replayed as
// input, so reasoning items and their signatures reach the next turn intact.
func runInjectedResponsesLoop(
	original *schemas.BifrostResponsesRequest,
	set *injectedToolSet,
	dispatch func(*schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError),
	exec func(schemas.ChatAssistantMessageToolCall) *schemas.ChatMessage,
) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	turn := applyInjectedToolsResponses(original, set)
	var usage *schemas.BifrostLLMUsage
	var priorItems []schemas.ResponsesMessage
	for depth := 1; ; depth++ {
		resp, bifrostErr := dispatch(turn)
		if bifrostErr != nil {
			billEarlierTurns(bifrostErr, usage)
			return nil, bifrostErr
		}
		var injected []schemas.ChatAssistantMessageToolCall
		for _, item := range resp.Output {
			if call, ok := injectedResponsesCall(item, set); ok {
				injected = append(injected, call)
			}
		}
		if len(injected) == 0 || depth >= set.maxDepth {
			output := make([]schemas.ResponsesMessage, 0, len(priorItems)+len(resp.Output))
			output = append(output, priorItems...)
			for _, item := range resp.Output {
				if _, ok := injectedResponsesCall(item, set); !ok {
					output = append(output, item)
				}
			}
			resp.Output = output
			resp.Usage = schemas.MergeBifrostLLMUsage(usage, resp.Usage.ToBifrostLLMUsage()).ToResponsesResponseUsage()
			return resp, nil
		}
		usage = schemas.MergeBifrostLLMUsage(usage, resp.Usage.ToBifrostLLMUsage())

		results := executeInjectedCalls(injected, exec)
		next := *turn
		next.Params = relaxForcedResponsesChoice(nextResponsesTools(original, turn, set).Params)
		next.Input = make([]schemas.ResponsesMessage, 0, len(turn.Input)+len(resp.Output)+len(injected))
		next.Input = append(next.Input, turn.Input...)
		for _, item := range resp.Output {
			_, isInjected := injectedResponsesCall(item, set)
			if isFunctionCall(item) && !isInjected {
				continue
			}
			next.Input = append(next.Input, item)
			if !isFunctionCall(item) {
				priorItems = append(priorItems, item)
			}
		}
		for _, result := range results {
			next.Input = append(next.Input, result.ToResponsesMessages()...)
		}
		turn = &next
	}
}

// nextResponsesTools returns the request whose tools the next turn sends. The tool set
// only changes when the gateway search loaded tools during the turn, so the request is
// rebuilt from the caller's original only then.
func nextResponsesTools(original, turn *schemas.BifrostResponsesRequest, set *injectedToolSet) *schemas.BifrostResponsesRequest {
	if set.search == nil {
		return turn
	}
	return applyInjectedToolsResponses(original, set)
}

// executeInjectedCalls runs the calls in parallel and returns their results in call
// order, which is the order providers expect tool results in.
func executeInjectedCalls(calls []schemas.ChatAssistantMessageToolCall, exec func(schemas.ChatAssistantMessageToolCall) *schemas.ChatMessage) []*schemas.ChatMessage {
	results := make([]*schemas.ChatMessage, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = exec(call)
		}()
	}
	wg.Wait()
	return results
}

func isFunctionCall(item schemas.ResponsesMessage) bool {
	return item.Type != nil && *item.Type == schemas.ResponsesMessageTypeFunctionCall && item.ResponsesToolMessage != nil
}

// injectedResponsesCall returns the item as a chat tool call when it calls an injected tool.
func injectedResponsesCall(item schemas.ResponsesMessage, set *injectedToolSet) (schemas.ChatAssistantMessageToolCall, bool) {
	if !isFunctionCall(item) || item.Name == nil || !set.isInjected(*item.Name) {
		return schemas.ChatAssistantMessageToolCall{}, false
	}
	arguments := ""
	if item.Arguments != nil {
		arguments = *item.Arguments
	}
	return schemas.ChatAssistantMessageToolCall{
		ID:       item.CallID,
		Type:     new("function"),
		Function: schemas.ChatAssistantMessageToolCallFunction{Name: item.Name, Arguments: arguments},
	}, true
}

// relaxForcedChatChoice returns params for the turn after an injected call ran. A forced
// choice (the remapped web search, "required") would force another tool call after
// every result until maxDepth, leaving the model no turn to answer in, so it becomes
// "auto". An allowed_tools choice keeps its set and only switches to mode auto. Params
// are copied; the caller's are never written.
func relaxForcedChatChoice(params *schemas.ChatParameters) *schemas.ChatParameters {
	if params == nil || !params.ToolChoice.IsForced() {
		return params
	}
	cp := *params
	// An allowed_tools set is the client's limit on which tools may run; only its mode relaxes.
	if s := params.ToolChoice.ChatToolChoiceStruct; s != nil && s.Type == schemas.ChatToolChoiceTypeAllowedTools && s.AllowedTools != nil {
		cp.ToolChoice = &schemas.ChatToolChoice{ChatToolChoiceStruct: &schemas.ChatToolChoiceStruct{
			Type:         schemas.ChatToolChoiceTypeAllowedTools,
			AllowedTools: &schemas.ChatToolChoiceAllowedTools{Mode: string(schemas.ChatToolChoiceTypeAuto), Tools: s.AllowedTools.Tools},
		}}
		return &cp
	}
	cp.ToolChoice = &schemas.ChatToolChoice{ChatToolChoiceStr: new(string(schemas.ChatToolChoiceTypeAuto))}
	return &cp
}

// relaxForcedResponsesChoice is the Responses API parallel of relaxForcedChatChoice.
func relaxForcedResponsesChoice(params *schemas.ResponsesParameters) *schemas.ResponsesParameters {
	if params == nil || !params.ToolChoice.IsForced() {
		return params
	}
	cp := *params
	// An allowed_tools set is the client's limit on which tools may run; only its mode relaxes.
	if s := params.ToolChoice.ResponsesToolChoiceStruct; s != nil && s.Type == schemas.ResponsesToolChoiceTypeAllowedTools {
		relaxed := *s
		relaxed.Mode = new(string(schemas.ResponsesToolChoiceTypeAuto))
		cp.ToolChoice = &schemas.ResponsesToolChoice{ResponsesToolChoiceStruct: &relaxed}
		return &cp
	}
	cp.ToolChoice = &schemas.ResponsesToolChoice{ResponsesToolChoiceStr: new(string(schemas.ResponsesToolChoiceTypeAuto))}
	return &cp
}

// billEarlierTurns adds the usage of turns that already finished to an error that ends
// the request. The provider bills only the turn it was serving, but every earlier turn
// was consumed too, and governance billing and logging read BilledUsage off the error.
func billEarlierTurns(bifrostErr *schemas.BifrostError, earlier *schemas.BifrostLLMUsage) {
	if bifrostErr == nil || earlier == nil {
		return
	}
	bifrostErr.ExtraFields.BilledUsage = schemas.MergeBifrostLLMUsage(earlier, bifrostErr.ExtraFields.BilledUsage)
}

// injectedToolsForAttempt resolves the tools Bifrost injects into one attempt: the
// gateway tool search when the request defers tools on a provider with no tool search
// of its own (baseProvider and model are this attempt's).
func injectedToolsForAttempt(ctx *schemas.BifrostContext, req *schemas.BifrostRequest, baseProvider schemas.ModelProvider, model string) *injectedToolSet {
	return withGatewayToolSearch(nil, req, baseProvider, schemas.ResolveCanonicalModel(ctx, model))
}

// withGatewayToolSearch adds the gateway tool search to set when the attempt needs it.
// The check costs one pass over the request's tools and allocates nothing when no
// tool is deferred.
func withGatewayToolSearch(set *injectedToolSet, req *schemas.BifrostRequest, baseProvider schemas.ModelProvider, model string) *injectedToolSet {
	if (req.RequestType != schemas.ResponsesRequest && req.RequestType != schemas.ResponsesStreamRequest) ||
		!toolsearch.Candidate(req.ResponsesRequest) ||
		toolsearch.NativeSupport(baseProvider, model) {
		return set
	}
	session := toolsearch.NewSession(req.ResponsesRequest)
	if set == nil {
		set = &injectedToolSet{maxDepth: toolsearch.MaxTurns}
	}
	set.search = session
	set.tools = append(set.tools, session.Tool())
	return set
}

// runInjectedTools serves one non-streaming attempt whose provider injects tools. Every
// turn goes to the same provider, model and key; plugins see only the final response.
func (bifrost *Bifrost) runInjectedTools(provider schemas.Provider, config *schemas.ProviderConfig, req *ChannelMessage, key schemas.Key, set *injectedToolSet) (*schemas.BifrostResponse, *schemas.BifrostError) {
	ctx := req.Context
	exec := func(call schemas.ChatAssistantMessageToolCall) *schemas.ChatMessage {
		return bifrost.executeInjectedCall(ctx, set, call)
	}
	switch req.RequestType {
	case schemas.ResponsesRequest:
		original := req.BifrostRequest.ResponsesRequest
		resp, bifrostErr := runInjectedResponsesLoop(original, set, func(turn *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
			return bifrost.dispatchResponses(ctx, provider, config, key, turn, original)
		}, exec)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		return &schemas.BifrostResponse{ResponsesResponse: resp}, nil
	}
	return bifrost.handleProviderRequest(provider, config, req, key, nil)
}

// executeInjectedCall runs one injected tool call. A call that names no injected tool
// goes back to the model as an error result rather than failing the request.
func (bifrost *Bifrost) executeInjectedCall(ctx *schemas.BifrostContext, set *injectedToolSet, call schemas.ChatAssistantMessageToolCall) *schemas.ChatMessage {
	// From here the attempt's response is assembled from several turns
	// (see BifrostContextKeyInjectedToolsExecuted).
	ctx.SetValue(schemas.BifrostContextKeyInjectedToolsExecuted, true)
	if set.search != nil && call.Function.Name != nil && *call.Function.Name == toolsearch.ToolName {
		return set.search.Execute(call)
	}
	message := "tool execution failed"
	return &schemas.ChatMessage{
		Role:            schemas.ChatMessageRoleTool,
		Content:         &schemas.ChatMessageContent{ContentStr: &message},
		ChatToolMessage: &schemas.ChatToolMessage{ToolCallID: call.ID, IsError: new(true)},
	}
}

// clearAnthropicPassthroughForInjectedTools moves an attempt with injected tools off
// Anthropic raw-body passthrough, the path Claude Code takes to Anthropic models, and
// returns the tool set the attempt should use.
//
// Passthrough forwards the caller's bytes, so the request rewrite (deferred tools out,
// tool search in) and the appended turns would never reach the wire, and the raw
// upstream events streamed back would show the client the injected tool_use blocks.
// The typed path is the one every non-Anthropic provider already serves Claude Code
// through. It runs before applyRawCaptureSignals for the same reason as
// clearAnthropicPassthroughForNonNativeProvider.
//
// A request whose conversation exists only in the raw body (a direct SDK call with an
// empty typed input) has nothing for the typed path to send. It keeps passthrough and
// goes out without the injected tool: injection fails open, as it does when the tool
// cannot be resolved.
//
// The passthrough settings are saved first, so a later attempt without injected tools
// (a fallback) gets them back from restoreAnthropicPassthroughAfterInjectedTools.
func clearAnthropicPassthroughForInjectedTools(ctx *schemas.BifrostContext, set *injectedToolSet, req *schemas.BifrostRequest) *injectedToolSet {
	if set == nil {
		return nil
	}
	if useRaw, _ := ctx.Value(schemas.BifrostContextKeyUseRawRequestBody).(bool); !useRaw {
		return set
	}
	if !hasTypedInput(req) {
		return nil
	}
	if _, saved := ctx.Value(injectedToolsPassthroughKey).(*passthroughSnapshot); !saved {
		snapshot := &passthroughSnapshot{values: make(map[schemas.BifrostContextKey]any, len(passthroughKeys))}
		for _, key := range passthroughKeys {
			snapshot.values[key] = ctx.Value(key)
		}
		ctx.SetValue(injectedToolsPassthroughKey, snapshot)
	}
	disableAnthropicPassthrough(ctx)
	return set
}

// restoreAnthropicPassthroughAfterInjectedTools puts back the passthrough settings an
// earlier attempt with injected tools switched off. Each attempt calls it before
// deciding passthrough afresh, so an attempt's own checks (a non-Anthropic provider,
// unsupported structured output, its own injected tools) still switch it off again.
func restoreAnthropicPassthroughAfterInjectedTools(ctx *schemas.BifrostContext) {
	snapshot, ok := ctx.Value(injectedToolsPassthroughKey).(*passthroughSnapshot)
	if !ok {
		return
	}
	for key, value := range snapshot.values {
		if value == nil {
			ctx.ClearValue(key)
		} else {
			ctx.SetValue(key, value)
		}
	}
	ctx.ClearValue(injectedToolsPassthroughKey)
}

// injectedToolsContextKey keys core-internal state for injected tools on the context.
type injectedToolsContextKey string

const injectedToolsPassthroughKey injectedToolsContextKey = "injected-tools-passthrough-snapshot"

// passthroughKeys are the settings disableAnthropicPassthrough changes.
var passthroughKeys = []schemas.BifrostContextKey{
	schemas.BifrostContextKeyUseRawRequestBody,
	schemas.BifrostContextKeyRawRequestBodyTextRewriter,
	schemas.BifrostContextKeySendBackRawResponse,
	schemas.BifrostContextKeyPassthroughOverridesPresent,
	schemas.BifrostContextKeyURLPath,
}

// passthroughSnapshot holds the passthrough settings as ingress set them. Small handles
// only: flags, a path and a rewriter func.
type passthroughSnapshot struct {
	values map[schemas.BifrostContextKey]any
}

func hasTypedInput(req *schemas.BifrostRequest) bool {
	switch {
	case req == nil:
		return false
	case req.ResponsesRequest != nil:
		return len(req.ResponsesRequest.Input) > 0
	case req.ChatRequest != nil:
		return len(req.ChatRequest.Input) > 0
	}
	return false
}

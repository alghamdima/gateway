// Package credits is the gateway plugin that enforces the prepaid balance: it holds the
// maximum cost of each attempt before the provider is called and charges the real cost
// afterwards (docs/platform/adr/0001-credits-ledger.md, 0009-credits-plugin-hooks.md). The
// charge is the provider's cost at the USD to SAR rate staff set in the admin console, with no
// markup (0010-admin-controlled-pricing.md); the ledger is in micro-SAR (0017-sar-ledger.md).
//
// It fails closed. A request it cannot attribute to an organization, cannot price, or
// cannot record is refused, not served for free.
package credits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/modelcatalog"
)

// PluginName is the name the plugin is configured under.
const PluginName = "credits"

const (
	defaultReservationTTL     = 15 * time.Minute
	defaultInputBytesPerToken = 3
	ledgerTimeout             = 10 * time.Second
)

// Error types returned to the caller in the error's "type" field.
const (
	ErrorTypeInsufficientCredits = "insufficient_credits"
	ErrorTypeNoOrganization      = "credits_no_organization"
	ErrorTypeUnsupportedRequest  = "credits_unsupported_request"
	ErrorTypeUnpriced            = "credits_unpriced_model"
	ErrorTypeLedgerUnavailable   = "credits_ledger_unavailable"
	ErrorTypeKeyLimit            = "credits_key_limit"
)

// Config is the plugin's entry in the gateway config.
type Config struct {
	// DatabaseURL is the control plane database, reached as the gateway_ledger role.
	// "env.NAME" reads it from the environment.
	DatabaseURL string `json:"database_url"`
	// ReservationTTLSeconds is how long a hold lives before the reaper may release it.
	ReservationTTLSeconds int `json:"reservation_ttl_seconds"`
	// InputBytesPerToken turns the size of a request into an input token estimate.
	// Lower is more cautious: it holds more for the same request.
	InputBytesPerToken int `json:"input_bytes_per_token"`
}

// ledger is the part of Store the hooks use.
type ledger interface {
	OrganizationFor(ctx context.Context, customerID string) (string, bool, error)
	Reserve(ctx context.Context, organizationID, virtualKeyID string, amount int64, fx fxRate, ttl time.Duration) (string, ReserveOutcome, error)
	Settle(ctx context.Context, st Settlement) (SettleResult, error)
	Extend(ctx context.Context, reservationID string, ttl time.Duration) (bool, error)
	Shortfall(ctx context.Context, organizationID, virtualKeyID string) (Shortfall, error)
}

// reaper releases holds whose settlement never arrived.
type reaper interface {
	Reap(ctx context.Context) (int64, error)
}

// pricer is the part of the model catalog the plugin uses.
type pricer interface {
	GetPricingEntryForModel(model string, provider schemas.ModelProvider) *modelcatalog.PricingEntry
	GetMaxOutputTokens(model string, provider schemas.ModelProvider) int
	CalculateCost(result *schemas.BifrostResponse, scopes *modelcatalog.PricingLookupScopes) float64
	CalculateCostForUsage(usage *schemas.BifrostLLMUsage, provider schemas.ModelProvider, model string, requestType schemas.RequestType, scopes *modelcatalog.PricingLookupScopes) float64
	// PricingOverrideName names the pricing override that priced the attempt: the control
	// plane names each one after the version of its price table (docs/platform/adr/0018-model-price-table.md)
	PricingOverrideName(model string, requestType schemas.RequestType, scopes *modelcatalog.PricingLookupScopes) string
}

// Plugin implements schemas.LLMPlugin.
type Plugin struct {
	ledger ledger
	pricer pricer
	logger schemas.Logger
	rates  rateSource
	// rate is the USD to SAR rate new holds convert at; nil until the first load (rates.go)
	rate atomic.Pointer[fxRate]
	// organizations caches gateway customer id -> organization id (organizations.go)
	organizations      sync.Map
	reservationTTL     time.Duration
	inputBytesPerToken int
	// refuseAll is set when the plugin could not be configured. It then refuses every
	// request with this reason, so a broken setup cannot serve traffic unaccounted.
	refuseAll  string
	pool       *pgxpool.Pool
	reaper     reaper
	background *background

	settleBackoff time.Duration
	shutdownGrace time.Duration
}

// hold is what PreLLMHook leaves on the context for the matching PostLLMHook.
type hold struct {
	reservationID  string
	organizationID string
	requestType    schemas.RequestType
	// amount is the hold in micro-SAR
	amount int64
	// fx is fixed when the hold is taken, so the attempt is charged at the rate its hold was
	// sized for even if staff change it meanwhile
	fx      fxRate
	settled atomic.Bool
	// when the hold's lifetime last started (unix nanoseconds): a stream that is still
	// running pushes it forward (keepAlive)
	renewedAt atomic.Int64
}

const holdContextKey schemas.BifrostContextKey = "credits-hold"

// Init builds the plugin. It does not return an error for a configuration it cannot use:
// the gateway only logs a plugin that fails to load and then serves without it, which here
// would mean serving for free. A misconfigured plugin loads and refuses every request.
func Init(ctx context.Context, config *Config, logger schemas.Logger, catalog *modelcatalog.ModelCatalog) (schemas.LLMPlugin, error) {
	plugin, err := build(ctx, config, logger, catalog)
	if err != nil {
		logger.Error("credits plugin is misconfigured and will refuse every request: %v", err)
		return Refusing(err.Error(), logger), nil
	}
	return plugin, nil
}

// Refusing returns a plugin that refuses every request with the given reason.
func Refusing(reason string, logger schemas.Logger) *Plugin {
	return &Plugin{refuseAll: reason, logger: logger}
}

func build(ctx context.Context, config *Config, logger schemas.Logger, catalog *modelcatalog.ModelCatalog) (*Plugin, error) {
	if config == nil {
		return nil, errors.New("no configuration")
	}
	if catalog == nil {
		return nil, errors.New("no model catalog: requests cannot be priced")
	}
	url := config.DatabaseURL
	if name, ok := strings.CutPrefix(url, "env."); ok {
		url = os.Getenv(name)
	}
	if url == "" {
		return nil, errors.New("database_url is empty")
	}
	// NewWithConfig does not connect: a database that is down at boot is a runtime
	// condition, handled per request by refusing.
	poolConfig, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("database_url: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("database pool: %w", err)
	}
	store := NewStore(pool)
	plugin := newPlugin(store, catalog, logger, config)
	plugin.pool = pool
	plugin.reaper = store
	plugin.rates = store
	// a failure is logged and retried by the refresh loop; until then requests are refused.
	// Bounded, so a database that is down delays the gateway's boot by seconds at most.
	bootCtx, cancelBoot := context.WithTimeout(ctx, rateBootTimeout)
	_ = plugin.refreshRate(bootCtx)
	cancelBoot()
	plugin.start(defaultSettleQueueSize, defaultSettleWorkers, defaultReapInterval)
	return plugin, nil
}

func newPlugin(l ledger, p pricer, logger schemas.Logger, config *Config) *Plugin {
	plugin := &Plugin{
		ledger:             l,
		pricer:             p,
		logger:             logger,
		reservationTTL:     defaultReservationTTL,
		inputBytesPerToken: defaultInputBytesPerToken,
		settleBackoff:      settleBackoff,
		shutdownGrace:      defaultShutdownGrace,
	}
	if config.ReservationTTLSeconds > 0 {
		plugin.reservationTTL = time.Duration(config.ReservationTTLSeconds) * time.Second
	}
	if config.InputBytesPerToken > 0 {
		plugin.inputBytesPerToken = config.InputBytesPerToken
	}
	return plugin
}

// GetName returns the plugin's name.
func (p *Plugin) GetName() string { return PluginName }

// Cleanup writes the settlements still queued, then closes the ledger connection.
func (p *Plugin) Cleanup() error {
	if p.background != nil {
		p.background.stop()
	}
	if p.pool != nil {
		p.pool.Close()
	}
	return nil
}

// PreRequestHook does nothing: the plugin takes no part in routing.
func (p *Plugin) PreRequestHook(_ *schemas.BifrostContext, _ *schemas.BifrostRequest) error {
	return nil
}

// requestClass says how the plugin treats a request type.
type requestClass int

const (
	classUnsupported requestClass = iota // refused: it may cost money and cannot be bounded yet
	classFree                            // passes: it calls no model
	classBillable                        // held and charged
)

func classify(requestType schemas.RequestType) requestClass {
	switch requestType {
	case schemas.ListModelsRequest, schemas.ModelRetrieveRequest:
		return classFree
	case schemas.ChatCompletionRequest, schemas.ChatCompletionStreamRequest,
		schemas.ResponsesRequest, schemas.ResponsesStreamRequest,
		schemas.TextCompletionRequest, schemas.TextCompletionStreamRequest,
		schemas.EmbeddingRequest:
		return classBillable
	default:
		return classUnsupported
	}
}

// PreLLMHook holds the maximum cost of this attempt, or refuses the request.
func (p *Plugin) PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	if p.refuseAll != "" {
		return req, refuse(503, ErrorTypeLedgerUnavailable, "Credits are not configured on this gateway.", schemas.Refusal{
			LimitSource: schemas.LimitSourceService,
			Reason:      ReasonCreditsNotConfigured,
			RemedyHint:  "A problem on our side that retrying will not fix. Contact support.",
		}), nil
	}
	if req == nil {
		return req, refuse(400, ErrorTypeUnsupportedRequest, "The request is empty.", schemas.Refusal{
			LimitSource: schemas.LimitSourceRequestType,
			Reason:      ReasonRequestEmpty,
			RemedyHint:  "Send a request body.",
		}), nil
	}
	switch classify(req.RequestType) {
	case classFree:
		return req, nil, nil
	case classUnsupported:
		return req, requestTypeRefusal(req.RequestType), nil
	}

	customerID := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyGovernanceCustomerID)
	if customerID == "" {
		return req, refuseNoOrganization(), nil
	}
	lookupCtx, cancelLookup := context.WithTimeout(context.Background(), ledgerTimeout)
	organizationID, mapped, err := p.organizationFor(lookupCtx, customerID)
	cancelLookup()
	if err != nil {
		p.logger.Error("credits: cannot resolve gateway customer %s: %v", customerID, err)
		return req, refuseLedgerUnavailable("Credits could not be checked. Try again shortly."), nil
	}
	if !mapped {
		return req, refuseNoOrganization(), nil
	}

	rate := p.rate.Load()
	if rate == nil {
		return req, refuseLedgerUnavailable("Prices could not be loaded. Try again shortly."), nil
	}
	fx := *rate

	if reason := unboundable(ctx, req); reason != "" {
		return req, refuseUnbounded(string(reason)), nil
	}
	amount, err := p.estimate(ctx, req, fx.effectiveE6)
	var cannotBound unboundableError
	if errors.As(err, &cannotBound) {
		return req, refuseUnbounded(string(cannotBound.reason)), nil
	}
	if err != nil {
		p.logger.Warn("credits: cannot estimate %s for organization %s: %v", req.RequestType, organizationID, err)
		return req, refuse(400, ErrorTypeUnpriced, "This model cannot be priced, so the request was not sent.", schemas.Refusal{
			LimitSource: schemas.LimitSourceCredits,
			Reason:      ReasonModelNotPriced,
			RemedyHint:  "Choose a model listed with a price on /api/v1/models.",
		}), nil
	}
	if amount == 0 {
		// a model that costs nothing needs no hold, and settles nothing
		return req, nil, nil
	}

	ledgerCtx, cancel := context.WithTimeout(context.Background(), ledgerTimeout)
	defer cancel()
	virtualKeyID := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyGovernanceVirtualKeyID)
	reservationID, outcome, err := p.ledger.Reserve(ledgerCtx, organizationID, virtualKeyID, amount, fx, p.reservationTTL)
	if err != nil {
		p.logger.Error("credits: reserve failed for organization %s: %v", organizationID, err)
		return req, refuseLedgerUnavailable("Credits could not be checked. Try again shortly."), nil
	}
	if outcome != Reserved {
		return req, p.refuseHold(organizationID, virtualKeyID, outcome, amount), nil
	}

	held := &hold{
		reservationID:  reservationID,
		organizationID: organizationID,
		requestType:    req.RequestType,
		amount:         amount,
		fx:             fx,
	}
	held.renewedAt.Store(time.Now().UnixNano())
	ctx.SetValue(holdContextKey, held)
	return req, nil, nil
}

// PostLLMHook charges the attempt and releases its hold. For a stream it acts once, on the
// final chunk or on an error.
func (p *Plugin) PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	held, _ := ctx.Value(holdContextKey).(*hold)
	if held == nil {
		return resp, bifrostErr, nil
	}
	if bifrostErr == nil && bifrost.IsStreamRequestType(held.requestType) && !bifrost.IsFinalChunk(ctx) {
		p.keepAlive(held)
		return resp, bifrostErr, nil
	}
	if !held.settled.CompareAndSwap(false, true) {
		return resp, bifrostErr, nil
	}

	requestType, provider, model, _ := bifrost.GetResponseFields(resp, bifrostErr)
	scopes := modelcatalog.PricingLookupScopesFromContext(ctx, string(provider))
	var costUSD float64
	switch {
	case resp != nil:
		costUSD = p.pricer.CalculateCost(resp, scopes)
	case bifrostErr != nil && bifrostErr.ExtraFields.BilledUsage != nil:
		// the provider billed for tokens before the attempt failed; for a stream
		// that was cancelled or timed out, the gateway counts the prompt and the
		// text already received (core/streamusage)
		costUSD = p.pricer.CalculateCostForUsage(bifrostErr.ExtraFields.BilledUsage, provider, model, requestType, scopes)
	}
	// the provider's cost converted at the hold's rate, with no markup, so one attempt is never
	// split between two rates; rounded half up to the nearest micro-SAR (the hold was rounded up)
	cost, err := toMicroSAR(costUSD, held.fx.effectiveE6)
	costMicroUSD, usdErr := toMicroUSD(costUSD)
	if err = errors.Join(err, usdErr); err != nil {
		// never guess a charge: release the hold and leave the difference to reconciliation
		p.logger.Error("credits: attempt %s for organization %s has an unusable cost and was not charged: %v", held.reservationID, held.organizationID, err)
		cost, costMicroUSD = 0, 0
	}

	requestID := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyRequestID)
	settlement := Settlement{
		ReservationID:  held.reservationID,
		OrganizationID: held.organizationID,
		Cost:           cost,
		CostMicroUSD:   costMicroUSD,
		FX:             held.fx,
		// one reservation is one attempt, so its id identifies the attempt
		IdempotencyKey: "usage:" + held.reservationID,
		Reference:      requestID,
		Metadata: map[string]any{
			"provider_cost_usd": costUSD,
			"held":              held.amount,
			"provider":          string(provider),
			"model":             model,
			"request_type":      string(requestType),
			"fallback_index":    bifrost.GetIntFromContext(ctx, schemas.BifrostContextKeyFallbackIndex),
			"attempt":           bifrost.GetIntFromContext(ctx, schemas.BifrostContextKeyNumberOfRetries),
			"failed":            bifrostErr != nil,
		},
	}
	// the price it was charged at: the pricing override that priced it, named after the
	// control plane's price version (ADR 0018); absent when the datasheet priced it alone
	if name := p.pricer.PricingOverrideName(model, requestType, scopes); name != "" {
		settlement.Metadata["price_override"] = name
	}
	// which provider key served the attempt: the ledger keeps it for good, as evidence
	// of where the request went (docs/platform/adr/0015-data-policy.md)
	if keyID := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeySelectedKeyID); keyID != "" {
		settlement.Metadata["provider_key_id"] = keyID
	}
	// token counts for the activity pages, which read the ledger (kept for good) rather
	// than the gateway's logs (kept for the retention window, ADR 0014)
	if usage := usageOf(resp, bifrostErr); usage != nil {
		settlement.Metadata["prompt_tokens"] = usage.PromptTokens
		settlement.Metadata["completion_tokens"] = usage.CompletionTokens
		settlement.Metadata["total_tokens"] = usage.TotalTokens
		settlement.Tokens = tokenDetailsOf(usage)
	}
	p.settle(settlement)
	return resp, bifrostErr, nil
}

// estimate returns the most this attempt can cost, in micro-SAR at the effective rate rateE6,
// rounded up.
func (p *Plugin) estimate(ctx *schemas.BifrostContext, req *schemas.BifrostRequest, rateE6 int64) (int64, error) {
	provider, model, _ := req.GetRequestFields()
	if model == "" {
		return 0, errors.New("request names no model")
	}
	if p.pricer.GetPricingEntryForModel(model, provider) == nil {
		return 0, fmt.Errorf("no price for %s/%s", provider, model)
	}

	var body any
	var outputLimit *int
	generates := true
	switch {
	case req.ChatRequest != nil:
		body = req.ChatRequest
		if req.ChatRequest.Params != nil {
			outputLimit = req.ChatRequest.Params.MaxCompletionTokens
		}
	case req.ResponsesRequest != nil:
		body = req.ResponsesRequest
		if req.ResponsesRequest.Params != nil {
			outputLimit = req.ResponsesRequest.Params.MaxOutputTokens
		}
	case req.TextCompletionRequest != nil:
		body = req.TextCompletionRequest
		if req.TextCompletionRequest.Params != nil {
			outputLimit = req.TextCompletionRequest.Params.MaxTokens
		}
	case req.EmbeddingRequest != nil:
		body = req.EmbeddingRequest
		generates = false
	default:
		return 0, fmt.Errorf("request of type %s carries no body", req.RequestType)
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("measure request: %w", err)
	}
	shape, reason, err := shapeOf(encoded)
	if err != nil {
		return 0, err
	}
	if reason != "" {
		return 0, unboundableError{reason: reason}
	}
	inputTokens := (len(encoded)+p.inputBytesPerToken-1)/p.inputBytesPerToken + shape.imageParts*imagePartTokens

	outputTokens := 0
	if generates {
		outputTokens = p.pricer.GetMaxOutputTokens(model, provider)
		if outputLimit != nil && *outputLimit > 0 && (outputTokens == 0 || *outputLimit < outputTokens) {
			outputTokens = *outputLimit
		}
		if outputTokens == 0 {
			return 0, fmt.Errorf("%s/%s has no maximum output and the request sets none", provider, model)
		}
		// n and best_of generate that many answers, each up to the limit
		outputTokens *= shape.completions
	}

	usage := &schemas.BifrostLLMUsage{PromptTokens: inputTokens, CompletionTokens: outputTokens, TotalTokens: inputTokens + outputTokens}
	scopes := modelcatalog.PricingLookupScopesFromContext(ctx, string(provider))
	return ceilMicroSAR(p.pricer.CalculateCostForUsage(usage, provider, model, req.RequestType, scopes), rateE6)
}

// usageOf is the attempt's token usage: the response's, or what the provider billed for
// before the attempt failed. Nil when neither says.
func usageOf(resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) *schemas.BifrostLLMUsage {
	if usage := resp.NormalizedUsage(); usage != nil {
		return usage
	}
	if bifrostErr != nil {
		return bifrostErr.ExtraFields.BilledUsage
	}
	return nil
}

// tokenDetailsOf reads the cached and reasoning counts out of a usage the gateway has normalized:
// the cache read is prompt_tokens_details.cached_read_tokens (what OpenAI calls cached_tokens),
// the cache write cached_write_tokens, reasoning completion_tokens_details.reasoning_tokens.
func tokenDetailsOf(usage *schemas.BifrostLLMUsage) *TokenDetails {
	details := &TokenDetails{}
	if p := usage.PromptTokensDetails; p != nil {
		details.CachedTokens = int64(p.CachedReadTokens)
		details.CacheWriteTokens = int64(p.CachedWriteTokens)
	}
	if c := usage.CompletionTokensDetails; c != nil {
		details.ReasoningTokens = int64(c.ReasoningTokens)
	}
	if details.ReasoningTokens == 0 && usage.ReasoningTokens > 0 {
		// a provider that reports reasoning at the top level, not yet folded in
		details.ReasoningTokens = int64(usage.ReasoningTokens)
	}
	return details
}

// keepAlive extends a streaming attempt's hold once a third of its lifetime has passed.
// The gateway puts no limit on how long a stream runs, only on the silence between its
// chunks, so a long stream can outlive a fixed hold. A released hold would free the
// money it covers for other requests, and stop counting against the key's limit, while
// the stream goes on spending it. The update runs off the stream's path.
func (p *Plugin) keepAlive(held *hold) {
	last := held.renewedAt.Load()
	now := time.Now().UnixNano()
	if time.Duration(now-last) < p.reservationTTL/3 || !held.renewedAt.CompareAndSwap(last, now) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), ledgerTimeout)
		defer cancel()
		if _, err := p.ledger.Extend(ctx, held.reservationID, p.reservationTTL); err != nil {
			p.logger.Warn("credits: cannot extend the hold of attempt %s for organization %s: %v", held.reservationID, held.organizationID, err)
		}
	}()
}

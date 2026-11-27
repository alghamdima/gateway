package logging

import (
	"context"
	"time"

	"github.com/maximhq/bifrost/framework/logstore"
)

// GenerationSummarizer is the optional part of a LogManager that counts one customer's
// generations and refusals and reads one request's attempts (Lemru, SPEC ACT-01, ROUTE-04).
// It is kept out of LogManager so the managers that do not need it are unchanged.
type GenerationSummarizer interface {
	GetGenerationSummary(ctx context.Context, customerID string, start, end time.Time) (*logstore.GenerationSummary, error)
	GetGenerationAttempts(ctx context.Context, requestID string) ([]logstore.GenerationAttempt, error)
}

// GetGenerationSummary reads the store's summary, when the store has one.
func (p *LoggerPlugin) GetGenerationSummary(ctx context.Context, customerID string, start, end time.Time) (*logstore.GenerationSummary, error) {
	reader, ok := p.store.(logstore.GenerationReader)
	if !ok {
		return nil, logstore.ErrGenerationSummaryUnsupported
	}
	return reader.GetGenerationSummary(ctx, customerID, start, end)
}

// GetGenerationAttempts reads a request's attempts, when the store can.
func (p *LoggerPlugin) GetGenerationAttempts(ctx context.Context, requestID string) ([]logstore.GenerationAttempt, error) {
	reader, ok := p.store.(logstore.GenerationReader)
	if !ok {
		return nil, logstore.ErrGenerationSummaryUnsupported
	}
	return reader.GetGenerationAttempts(ctx, requestID)
}

// GetGenerationSummary implements GenerationSummarizer.
func (p *PluginLogManager) GetGenerationSummary(ctx context.Context, customerID string, start, end time.Time) (*logstore.GenerationSummary, error) {
	return p.plugin.GetGenerationSummary(ctx, customerID, start, end)
}

// GetGenerationAttempts implements GenerationSummarizer.
func (p *PluginLogManager) GetGenerationAttempts(ctx context.Context, requestID string) ([]logstore.GenerationAttempt, error) {
	return p.plugin.GetGenerationAttempts(ctx, requestID)
}

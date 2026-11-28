package logging

import (
	"context"
	"time"

	"github.com/maximhq/bifrost/framework/logstore"
)

// EndpointStatsProvider is the optional part of a LogManager that counts every endpoint's
// attempts by hour (Lemru, SPEC CAT-11, CAT-12). It is kept out of LogManager so the managers that
// do not need it are unchanged.
type EndpointStatsProvider interface {
	GetEndpointStats(ctx context.Context, start, end time.Time) (*logstore.EndpointStats, error)
}

// GetEndpointStats reads the store's statistics, when the store has them.
func (p *LoggerPlugin) GetEndpointStats(ctx context.Context, start, end time.Time) (*logstore.EndpointStats, error) {
	reader, ok := p.store.(logstore.EndpointStatsReader)
	if !ok {
		return nil, logstore.ErrEndpointStatsUnsupported
	}
	return reader.GetEndpointStats(ctx, start, end)
}

// GetEndpointStats implements EndpointStatsProvider.
func (p *PluginLogManager) GetEndpointStats(ctx context.Context, start, end time.Time) (*logstore.EndpointStats, error) {
	return p.plugin.GetEndpointStats(ctx, start, end)
}

package handlers

import (
	"errors"
	"time"

	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/maximhq/bifrost/plugins/logging"
	"github.com/valyala/fasthttp"
)

// maxEndpointStatsWindow bounds one read: the control plane asks for a few hours at a time, and a
// backfill for a day.
const maxEndpointStatsWindow = 31 * 24 * time.Hour

// getEndpointStats handles GET /api/logs/endpoints/hourly?start_time=&end_time= (Lemru, SPEC
// CAT-11, CAT-12): every endpoint's attempts in the window by UTC hour, with its tokens, its cost
// and the histograms of its timings. Times are RFC 3339 and on the hour, so no hour is returned in
// part; the window is [start_time, end_time). Never request or response content, and nothing about
// a customer: the figures are per endpoint.
func (h *LoggingHandler) getEndpointStats(ctx *fasthttp.RequestCtx) {
	provider, ok := h.logManager.(logging.EndpointStatsProvider)
	if !ok {
		SendError(ctx, fasthttp.StatusNotImplemented, "endpoint statistics are not available")
		return
	}
	start, startErr := time.Parse(time.RFC3339, string(ctx.QueryArgs().Peek("start_time")))
	end, endErr := time.Parse(time.RFC3339, string(ctx.QueryArgs().Peek("end_time")))
	if startErr != nil || endErr != nil || !end.After(start) || end.Sub(start) > maxEndpointStatsWindow ||
		!start.Equal(start.Truncate(time.Hour)) || !end.Equal(end.Truncate(time.Hour)) {
		SendError(ctx, fasthttp.StatusBadRequest, "start_time and end_time must be RFC 3339, on the hour, end after start, at most 31 days apart")
		return
	}
	stats, err := provider.GetEndpointStats(ctx, start.UTC(), end.UTC())
	if errors.Is(err, logstore.ErrEndpointStatsUnsupported) {
		SendError(ctx, fasthttp.StatusNotImplemented, err.Error())
		return
	}
	if err != nil {
		logger.Error("failed to read endpoint statistics: %v", err)
		SendError(ctx, fasthttp.StatusInternalServerError, "endpoint statistics failed")
		return
	}
	SendJSON(ctx, stats)
}

package handlers

import (
	"errors"
	"time"

	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/maximhq/bifrost/plugins/logging"
	"github.com/valyala/fasthttp"
)

// maxGenerationSummaryWindow bounds one summary read: the activity page asks for at most a year.
const maxGenerationSummaryWindow = 367 * 24 * time.Hour

// getGenerationSummary handles GET /api/logs/generations/summary?customer_id=&start_time=&end_time=
// (Lemru, SPEC ACT-01, ACT-02): one customer's generations by day, model and virtual key, its
// refusals by reason, and the timings of its successful generations. Times are RFC 3339; the
// window is [start_time, end_time).
func (h *LoggingHandler) getGenerationSummary(ctx *fasthttp.RequestCtx) {
	summarizer, ok := h.logManager.(logging.GenerationSummarizer)
	if !ok {
		SendError(ctx, fasthttp.StatusNotImplemented, "generation summary is not available")
		return
	}
	customerID := string(ctx.QueryArgs().Peek("customer_id"))
	if customerID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "customer_id is required")
		return
	}
	start, startErr := time.Parse(time.RFC3339Nano, string(ctx.QueryArgs().Peek("start_time")))
	end, endErr := time.Parse(time.RFC3339Nano, string(ctx.QueryArgs().Peek("end_time")))
	if startErr != nil || endErr != nil || !end.After(start) || end.Sub(start) > maxGenerationSummaryWindow {
		SendError(ctx, fasthttp.StatusBadRequest, "start_time and end_time must be RFC 3339, end after start, at most 367 days apart")
		return
	}
	summary, err := summarizer.GetGenerationSummary(ctx, customerID, start.UTC(), end.UTC())
	if errors.Is(err, logstore.ErrGenerationSummaryUnsupported) {
		SendError(ctx, fasthttp.StatusNotImplemented, err.Error())
		return
	}
	if err != nil {
		logger.Error("failed to summarize generations: %v", err)
		SendError(ctx, fasthttp.StatusInternalServerError, "generation summary failed")
		return
	}
	SendJSON(ctx, summary)
}

// getGenerationAttempts handles GET /api/logs/generations/{id}/attempts (Lemru, SPEC ROUTE-04,
// API-04): the request's root row and its fallback rows, in attempt order, with each attempt's
// status, HTTP status, error type, refusal reason and timings. Never request or response content.
func (h *LoggingHandler) getGenerationAttempts(ctx *fasthttp.RequestCtx) {
	summarizer, ok := h.logManager.(logging.GenerationSummarizer)
	if !ok {
		SendError(ctx, fasthttp.StatusNotImplemented, "generation attempts are not available")
		return
	}
	id, _ := ctx.UserValue("id").(string)
	if id == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "request id is required")
		return
	}
	attempts, err := summarizer.GetGenerationAttempts(ctx, id)
	if errors.Is(err, logstore.ErrGenerationSummaryUnsupported) {
		SendError(ctx, fasthttp.StatusNotImplemented, err.Error())
		return
	}
	if err != nil {
		logger.Error("failed to read generation attempts: %v", err)
		SendError(ctx, fasthttp.StatusInternalServerError, "generation attempts failed")
		return
	}
	SendJSON(ctx, map[string]any{"attempts": attempts})
}

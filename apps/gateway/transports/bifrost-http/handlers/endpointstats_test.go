package handlers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/maximhq/bifrost/framework/logstore"
	loggingplugin "github.com/maximhq/bifrost/plugins/logging"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// endpointStatsLogManager answers the statistics read and records the window it was asked for.
type endpointStatsLogManager struct {
	loggingplugin.LogManager
	start, end time.Time
	err        error
}

func (m *endpointStatsLogManager) GetEndpointStats(_ context.Context, start, end time.Time) (*logstore.EndpointStats, error) {
	m.start, m.end = start, end
	if m.err != nil {
		return nil, m.err
	}
	return &logstore.EndpointStats{Start: start, End: end, Hours: []logstore.EndpointHour{{Provider: "openai", Model: "gpt-4o", Attempts: 3}}}, nil
}

// Lemru, SPEC CAT-11: the window is whole UTC hours, so the control plane never stores part of one.
func TestGetEndpointStats(t *testing.T) {
	SetLogger(&mockLogger{})
	tests := []struct {
		name       string
		query      string
		wantStatus int
	}{
		{name: "whole hours", query: "start_time=2026-11-27T10:00:00Z&end_time=2026-11-27T12:00:00Z", wantStatus: fasthttp.StatusOK},
		{name: "an offset is read as its UTC hour", query: "start_time=2026-11-27T13:00:00%2B03:00&end_time=2026-11-27T15:00:00%2B03:00", wantStatus: fasthttp.StatusOK},
		{name: "not on the hour", query: "start_time=2026-11-27T10:30:00Z&end_time=2026-11-27T12:00:00Z", wantStatus: fasthttp.StatusBadRequest},
		{name: "end before start", query: "start_time=2026-11-27T12:00:00Z&end_time=2026-11-27T10:00:00Z", wantStatus: fasthttp.StatusBadRequest},
		{name: "longer than 31 days", query: "start_time=2026-10-01T00:00:00Z&end_time=2026-11-27T00:00:00Z", wantStatus: fasthttp.StatusBadRequest},
		{name: "missing", query: "start_time=2026-11-27T10:00:00Z", wantStatus: fasthttp.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &endpointStatsLogManager{}
			h := &LoggingHandler{logManager: mgr}
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.SetRequestURI("/api/logs/endpoints/hourly?" + tt.query)

			h.getEndpointStats(ctx)

			require.Equal(t, tt.wantStatus, ctx.Response.StatusCode(), string(ctx.Response.Body()))
			if tt.wantStatus != fasthttp.StatusOK {
				require.True(t, mgr.start.IsZero(), "the store is not asked for a window that was refused")
				return
			}
			require.Equal(t, time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC), mgr.start)
			require.Equal(t, time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC), mgr.end)
			var body logstore.EndpointStats
			require.NoError(t, json.Unmarshal(ctx.Response.Body(), &body))
			require.Len(t, body.Hours, 1)
			require.Equal(t, int64(3), body.Hours[0].Attempts)
		})
	}
}

// A store with no SQL for the statistics, and a manager that has none, answer 501, not 500.
func TestGetEndpointStats_Unsupported(t *testing.T) {
	SetLogger(&mockLogger{})
	const query = "/api/logs/endpoints/hourly?start_time=2026-11-27T10:00:00Z&end_time=2026-11-27T11:00:00Z"
	for name, manager := range map[string]loggingplugin.LogManager{
		"the store cannot":  &endpointStatsLogManager{err: logstore.ErrEndpointStatsUnsupported},
		"the manager lacks": &timestampHintLogManager{},
	} {
		t.Run(name, func(t *testing.T) {
			h := &LoggingHandler{logManager: manager}
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.SetRequestURI(query)

			h.getEndpointStats(ctx)

			require.Equal(t, fasthttp.StatusNotImplemented, ctx.Response.StatusCode(), string(ctx.Response.Body()))
		})
	}
}

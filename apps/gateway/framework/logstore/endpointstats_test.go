package logstore

import (
	"context"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Lemru, SPEC CAT-11/CAT-12: every attempt that reached a provider is counted on its endpoint and
// hour, refusals and unfinished requests are not, and the timings come as histograms that add up.
func TestGetEndpointStats_CountsAttemptsByEndpointAndHour(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&Log{}))
	checkEndpointStats(t, db)
}

// The deployed log store is Postgres: the same answers, with its JSON operators.
func TestGetEndpointStats_Postgres(t *testing.T) {
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available, skipping test")
	}
	dropAllManagedMatViews(db)
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS logs CASCADE").Error)
	require.NoError(t, db.AutoMigrate(&Log{}))
	t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS logs CASCADE") })
	checkEndpointStats(t, db)
}

// bucketOf is the histogram bucket a value falls in, worked out from the bounds the answer carries.
func bucketOf(bounds []float64, value float64) int {
	for i, bound := range bounds {
		if value < bound {
			return i
		}
	}
	return len(bounds)
}

func sum(counts []int64) int64 {
	var total int64
	for _, n := range counts {
		total += n
	}
	return total
}

func checkEndpointStats(t *testing.T, db *gorm.DB) {
	t.Helper()
	store := &RDBLogStore{db: db}

	ten := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	str := func(v string) *string { return &v }
	ms := func(v float64) *float64 { return &v }
	usd := func(v float64) *float64 { return &v }
	refusal := `{"status_code":402,"error":{"code":402,"message":"no","metadata":{"limit_source":"credits","reason":"insufficient_balance"}}}`
	providerError := `{"status_code":500,"error":{"type":"server_error","message":"down"}}`
	usage := `{"prompt_tokens":1000,"completion_tokens":400,"completion_tokens_details":{"reasoning_tokens":150}}`
	row := func(l Log) Log {
		if l.Object == "" {
			l.Object = "chat_completion"
		}
		if l.Provider == "" {
			l.Provider = "openai"
		}
		if l.Model == "" {
			l.Model = "gpt-4o"
		}
		switch l.SelectedKeyID {
		case "":
			l.SelectedKeyID = "key_a"
		case "-": // a failed row: core records no selected key
			l.SelectedKeyID = ""
		}
		if l.Timestamp.IsZero() {
			l.Timestamp = ten.Add(5 * time.Minute)
		}
		return l
	}
	seed := []Log{
		// two answers on the same endpoint and hour, one of them a stream
		row(Log{ID: "a", Status: "success", Latency: ms(1000), UpstreamLatency: ms(800), PromptTokens: 1000, CompletionTokens: 400,
			CachedReadTokens: 600, TokenUsage: usage, InputCost: 0.002, OutputCost: 0.004, Cost: usd(0.006)}),
		row(Log{ID: "b", Status: "success", Stream: true, TimeToFirstToken: ms(300), Latency: ms(4000), UpstreamLatency: ms(2000),
			PromptTokens: 500, CompletionTokens: 100, InputCost: 0.001, OutputCost: 0.001, Cost: usd(0.002)}),
		// the provider failed: an attempt, not an answer, and none of its tokens or timings. Core
		// keeps selected_key_id for answers only: a failure's key is the last one of its trail, and
		// "auto" is the tier the request asked for, the provider's standard tier
		row(Log{ID: "c", Status: "error", ErrorDetails: providerError, Latency: ms(50), PromptTokens: 999, SelectedKeyID: "-",
			ServiceTier: str("auto"), AttemptTrailParsed: []schemas.KeyAttemptRecord{{Attempt: 0, KeyID: "key_a", KeyName: "a", FailReason: str("server_error")}}}),
		// answered on key_a after key_c failed and the gateway moved on: one failed attempt on key_c
		row(Log{ID: "k", Status: "success", ServiceTier: str("default"), Latency: ms(1000), UpstreamLatency: ms(1000),
			AttemptTrailParsed: []schemas.KeyAttemptRecord{
				{Attempt: 0, KeyID: "key_c", KeyName: "c", FailReason: str("rate_limited"), TriggeredRotation: true},
				{Attempt: 1, KeyID: "key_a", KeyName: "a"},
			}}),
		// a fallback's attempt counts on the endpoint that carried it
		row(Log{ID: "c1", ParentRequestID: str("c"), FallbackIndex: 1, Status: "success", SelectedKeyID: "key_b", Latency: ms(700),
			UpstreamLatency: ms(500), PromptTokens: 10, CompletionTokens: 50}),
		// another tier of the same model is another endpoint
		row(Log{ID: "d", Status: "success", ServiceTier: str("priority"), Latency: ms(200), UpstreamLatency: ms(100), CompletionTokens: 100}),
		// the next hour is another row
		row(Log{ID: "e", Status: "success", Timestamp: ten.Add(65 * time.Minute), Latency: ms(1000), UpstreamLatency: ms(1000), CompletionTokens: 10}),
		// not counted: refused before any provider, a model listing, still running, cancelled, outside the window
		row(Log{ID: "f", Status: "error", ErrorDetails: refusal}),
		row(Log{ID: "g", Status: "success", Object: "list_models"}),
		row(Log{ID: "h", Status: "processing"}),
		row(Log{ID: "i", Status: "cancelled"}),
		row(Log{ID: "j", Status: "success", Timestamp: ten.Add(-time.Minute)}),
	}
	for i := range seed {
		require.NoError(t, db.Create(&seed[i]).Error)
	}

	got, err := store.GetEndpointStats(context.Background(), ten, ten.Add(2*time.Hour))
	require.NoError(t, err)

	require.Len(t, got.Bounds.LatencyMs, EndpointStatsBuckets-1)
	require.Len(t, got.Bounds.Throughput, EndpointStatsBuckets-1)
	type id struct {
		hour               int
		keyID, serviceTier string
	}
	byID := map[id]EndpointHour{}
	for _, h := range got.Hours {
		require.Equal(t, "openai", h.Provider)
		require.Equal(t, "gpt-4o", h.Model)
		byID[id{h.Hour.Hour(), h.KeyID, h.ServiceTier}] = h
	}
	require.Len(t, byID, 5, "key_a, key_b, key_c and the priority tier at 10:00, key_a at 11:00")

	main := byID[id{10, "key_a", ""}]
	require.Equal(t, int64(4), main.Attempts, "a, b, k and the failed c, whose key is read from its trail")
	require.Equal(t, int64(3), main.Succeeded)

	rotatedAway := byID[id{10, "key_c", ""}]
	require.Equal(t, int64(1), rotatedAway.Attempts, "the key that failed before k was answered")
	require.Equal(t, int64(0), rotatedAway.Succeeded)
	require.Equal(t, int64(1), main.Streamed)
	require.Equal(t, int64(1500), main.PromptTokens, "the failed attempt's tokens are not counted")
	require.Equal(t, int64(500), main.CompletionTokens)
	require.Equal(t, int64(150), main.ReasoningTokens)
	require.Equal(t, int64(600), main.CachedReadTokens)
	require.InDelta(t, 0.003, main.InputCostUSD, 1e-9)
	require.InDelta(t, 0.005, main.OutputCostUSD, 1e-9)
	require.InDelta(t, 0.008, main.CostUSD, 1e-9)

	// latency of a and k (1000 ms) and b (4000 ms); time to first token of the stream only (300 ms);
	// throughput 400/0.8 = 500 and 100/2 = 50 tokens per second (k wrote no tokens)
	require.Equal(t, int64(3), sum(main.LatencyMs))
	require.Equal(t, int64(2), main.LatencyMs[bucketOf(got.Bounds.LatencyMs, 1000)], "a and k")
	require.Equal(t, int64(1), main.LatencyMs[bucketOf(got.Bounds.LatencyMs, 4000)])
	require.Equal(t, int64(1), sum(main.TimeToFirstTokenMs))
	require.Equal(t, int64(1), main.TimeToFirstTokenMs[bucketOf(got.Bounds.LatencyMs, 300)])
	require.Equal(t, int64(2), sum(main.Throughput))
	require.Equal(t, int64(1), main.Throughput[bucketOf(got.Bounds.Throughput, 500)])
	require.Equal(t, int64(1), main.Throughput[bucketOf(got.Bounds.Throughput, 50)])

	fallback := byID[id{10, "key_b", ""}]
	require.Equal(t, int64(1), fallback.Attempts)
	require.Equal(t, int64(1), fallback.Succeeded)
	require.Equal(t, int64(50), fallback.CompletionTokens)

	require.Equal(t, int64(1), byID[id{10, "key_a", "priority"}].Succeeded)
	require.Equal(t, int64(1), byID[id{11, "key_a", ""}].Succeeded)

	// the hours come in order, and an hour outside the window is not there
	require.True(t, got.Hours[0].Hour.Equal(ten))
	require.True(t, got.Hours[len(got.Hours)-1].Hour.Equal(ten.Add(time.Hour)))
}

// A value read back from its bucket is within the growth factor of the true value.
func TestEndpointBounds_AreGeometricAndCoverTheRange(t *testing.T) {
	bounds := endpointBounds(endpointLatencyFirstMs)

	require.Len(t, bounds, EndpointStatsBuckets-1)
	require.InDelta(t, endpointLatencyFirstMs, bounds[0], 1e-9)
	for i := 1; i < len(bounds); i++ {
		require.InDelta(t, endpointStatsGrowth, bounds[i]/bounds[i-1], 1e-9)
	}
	require.Greater(t, bounds[len(bounds)-1], float64(25*60*1000), "the last bound is past 25 minutes")
}

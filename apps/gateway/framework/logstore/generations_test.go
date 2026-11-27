package logstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Lemru, SPEC ACT-01/ACT-02: a request is counted once however many attempts it took, refusals are
// counted apart by reason, and timings are read from the attempt that answered.
func TestGetGenerationSummary_CountsEachRequestOnceAndRefusalsApart(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&Log{}))
	checkGenerationSummary(t, db)
}

// The deployed log store is Postgres: the same answers, with its percentile_cont and JSON operators.
func TestGetGenerationSummary_Postgres(t *testing.T) {
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available, skipping test")
	}
	dropAllManagedMatViews(db)
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS logs CASCADE").Error)
	require.NoError(t, db.AutoMigrate(&Log{}))
	t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS logs CASCADE") })
	checkGenerationSummary(t, db)
}

func checkGenerationSummary(t *testing.T, db *gorm.DB) {
	t.Helper()
	store := &RDBLogStore{db: db}

	day := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	str := func(v string) *string { return &v }
	ms := func(v float64) *float64 { return &v }
	refusal := `{"status_code":402,"type":"insufficient_credits","error":{"code":402,"type":"insufficient_credits","message":"no","metadata":{"limit_source":"credits","reason":"insufficient_balance","remedy_hint":"Add credits."}}}`
	providerError := `{"status_code":500,"error":{"type":"server_error","message":"down"}}`
	row := func(l Log) Log {
		if l.CustomerID == nil {
			l.CustomerID = str("cus_1")
		}
		if l.Object == "" {
			l.Object = "chat_completion"
		}
		if l.Provider == "" {
			l.Provider = "openai"
		}
		if l.Model == "" {
			l.Model = "gpt-4o-mini"
		}
		if l.Timestamp.IsZero() {
			l.Timestamp = day
		}
		return l
	}
	seed := []Log{
		// answered at once
		row(Log{ID: "a", Status: "success", VirtualKeyID: str("vk_1"), Latency: ms(100), UpstreamLatency: ms(80), CompletionTokens: 500}),
		// fell back: the first attempt failed, the fallback answered on another model
		row(Log{ID: "b", Status: "error", VirtualKeyID: str("vk_1"), ErrorDetails: providerError, Latency: ms(10)}),
		row(Log{ID: "b1", ParentRequestID: str("b"), FallbackIndex: 1, Status: "success", Model: "gpt-4o", VirtualKeyID: str("vk_1"), Latency: ms(50), UpstreamLatency: ms(40), CompletionTokens: 200}),
		// every attempt failed: one failed generation
		row(Log{ID: "c", Status: "error", VirtualKeyID: str("vk_1"), ErrorDetails: providerError}),
		row(Log{ID: "c1", ParentRequestID: str("c"), FallbackIndex: 1, Status: "error", Model: "gpt-4o", ErrorDetails: providerError}),
		// refused before any provider: not a generation
		row(Log{ID: "d", Status: "error", VirtualKeyID: str("vk_1"), ErrorDetails: refusal}),
		// a stream, timed to its first chunk
		row(Log{ID: "e", Status: "success", VirtualKeyID: str("vk_2"), Stream: true, TimeToFirstToken: ms(300), Latency: ms(1000), UpstreamLatency: ms(900), CompletionTokens: 450}),
		// a first attempt whose parent the client set from its session baggage is still a request
		row(Log{ID: "h", ParentRequestID: str("client-session-1"), Status: "success", VirtualKeyID: str("vk_2"), Latency: ms(200), UpstreamLatency: ms(100), CompletionTokens: 100}),
		// the day before, inside the window
		row(Log{ID: "i", Status: "success", VirtualKeyID: str("vk_2"), Timestamp: day.Add(-24 * time.Hour), Latency: ms(400), UpstreamLatency: ms(200), CompletionTokens: 100}),
		// not counted: a model listing, a request still running, another customer, outside the window
		row(Log{ID: "f", Status: "success", Object: "list_models"}),
		row(Log{ID: "g", Status: "processing"}),
		row(Log{ID: "x", Status: "success", CustomerID: str("cus_2")}),
		row(Log{ID: "y", Status: "success", Timestamp: day.Add(-30 * 24 * time.Hour)}),
	}
	for i := range seed {
		require.NoError(t, db.Create(&seed[i]).Error)
	}

	start, end := day.Add(-7*24*time.Hour), day.Add(time.Hour)
	got, err := store.GetGenerationSummary(context.Background(), "cus_1", start, end)
	require.NoError(t, err)

	require.Equal(t, []GenerationDay{
		{Day: "2026-11-26", Generations: 1},
		{Day: "2026-11-27", Generations: 5, Failed: 1, Refused: 1},
	}, got.ByDay)
	require.Equal(t, []GenerationModel{
		{Provider: "openai", Model: "gpt-4o-mini", Generations: 5},
		{Provider: "openai", Model: "gpt-4o", Generations: 1},
	}, got.ByModel)
	require.Equal(t, []GenerationKey{{VirtualKeyID: "vk_1", Generations: 3}, {VirtualKeyID: "vk_2", Generations: 3}}, got.ByKey)
	require.Equal(t, []GenerationRefusal{{LimitSource: "credits", Reason: "insufficient_balance", StatusCode: 402, Requests: 1}}, got.Refusals)

	// successful generations a, b1, e, h, i; time to first token is the latency unless streamed
	timings := got.Timings
	require.Equal(t, int64(5), timings.Measured)
	require.Equal(t, int64(1), timings.Streamed)
	require.InDelta(t, (100+50+300+200+400)/5.0, timings.TimeToFirstToken.Avg, 1e-9)
	require.InDelta(t, 200.0, timings.TimeToFirstToken.P50, 1e-9)
	require.InDelta(t, 360.0, timings.TimeToFirstToken.P90, 1e-9) // between 300 and 400, as percentile_cont
	require.InDelta(t, 200.0, timings.Latency.P50, 1e-9)
	require.InDelta(t, (100+50+1000+200+400)/5.0, timings.Latency.Avg, 1e-9)
	require.InDelta(t, (80+40+900+100+200)/5.0, timings.GenerationTimeAvg, 1e-9)
	require.InDelta(t, (500/0.08+200/0.04+450/0.9+100/0.1+100/0.2)/5, timings.ThroughputAvg, 1e-6)

	// an empty window answers with empty lists, not nulls
	empty, err := store.GetGenerationSummary(context.Background(), "cus_none", start, end)
	require.NoError(t, err)
	require.NotNil(t, empty.ByDay)
	require.Empty(t, empty.ByDay)
	require.Equal(t, int64(0), empty.Timings.Measured)

	attempts, err := store.GetGenerationAttempts(context.Background(), "b")
	require.NoError(t, err)
	require.Len(t, attempts, 2)
	require.Equal(t, "b", attempts[0].ID)
	require.Equal(t, 500, *attempts[0].StatusCode)
	require.Equal(t, "server_error", attempts[0].ErrorType)
	require.Equal(t, "b1", attempts[1].ID)
	require.Equal(t, "b", attempts[1].ParentRequestID)
	require.Equal(t, 200, *attempts[1].StatusCode)
	require.Equal(t, "gpt-4o", attempts[1].Model)

	refused, err := store.GetGenerationAttempts(context.Background(), "d")
	require.NoError(t, err)
	require.Len(t, refused, 1)
	require.Equal(t, "credits", refused[0].LimitSource)
	require.Equal(t, "insufficient_balance", refused[0].Reason)
	require.Equal(t, 402, *refused[0].StatusCode)
}

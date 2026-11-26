package logstore

import (
	"context"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/queryscope"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// GetStats reports input/output tokens alongside the total so the logs stats
// card can show the split. Only terminal requests contribute, matching the
// total/cost aggregates computed in the same query.
func TestGetStatsTokenSplit(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Log{}))

	s := &RDBLogStore{db: db, logger: bifrost.NewDefaultLogger(schemas.LogLevelInfo)}
	ctx := context.Background()
	now := time.Now()

	seed := []struct {
		id                        string
		prompt, completion, total int
		status                    string
	}{
		{"a", 100, 10, 110, "success"},
		{"b", 200, 20, 220, "success"},
		{"c", 400, 40, 440, "error"},       // terminal, must count
		{"d", 999, 99, 1098, "processing"}, // non-terminal, must NOT count
	}
	for _, sd := range seed {
		require.NoError(t, db.Create(&Log{
			ID:               sd.id,
			Timestamp:        now,
			Status:           sd.status,
			PromptTokens:     sd.prompt,
			CompletionTokens: sd.completion,
			TotalTokens:      sd.total,
		}).Error)
	}

	stats, err := s.GetStats(ctx, SearchFilters{})
	require.NoError(t, err)

	require.Equal(t, int64(770), stats.TotalTokens, "total excludes non-terminal")
	require.Equal(t, int64(700), stats.PromptTokens, "prompt = 100+200+400")
	require.Equal(t, int64(70), stats.CompletionTokens, "completion = 10+20+40")
	require.Equal(t, stats.TotalTokens, stats.PromptTokens+stats.CompletionTokens, "split sums to total")
}

func ptrTime(value time.Time) *time.Time { return &value }

// TestGetDistinctMCPKeyPairsFollowsRelayOptions checks the MCP filter dropdowns list options the way
// the Relay Logs ones do.
func TestGetDistinctMCPKeyPairsFollowsRelayOptions(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&MCPToolLog{}))
	store := &RDBLogStore{db: db, logger: bifrost.NewDefaultLogger(schemas.LogLevelInfo)}
	assertMCPKeyPairs(t, store, store.BatchCreateMCPToolLogsIfNotExists)
}

// assertMCPKeyPairs pins the MCP filter options: teams, customers and business units fan out over
// their JSON-array columns, each id is listed once, a user with no name is still listed, search and
// limit narrow the list, and rows older than the filter-data window or outside the row scope add
// nothing. create is the store's own writer, since ClickHouse writes MCP tool logs its own way.
func assertMCPKeyPairs(t *testing.T, store *RDBLogStore, create func(context.Context, []*MCPToolLog) error) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	userID, userName, namelessUserID, oldUserID := "user-a", "MCP User", "user-nameless", "user-old"
	projectID, projectName := "project-a", "Apollo"
	soloTeamID, soloTeamName := "team-solo", "Solo"
	require.NoError(t, create(ctx, []*MCPToolLog{
		{
			ID: "options-a", ToolName: "Read", Timestamp: now, Status: "success",
			UserID: &userID, UserName: &userName, ProjectID: &projectID, ProjectName: &projectName,
			TeamIDsParsed: []string{"team-b", "team-a"}, TeamNamesParsed: []string{"Beta", "Alpha"},
			CustomerIDsParsed: []string{"customer-b", "customer-a"}, CustomerNamesParsed: []string{"Bravo", "Acme"},
			BusinessUnitIDsParsed: []string{"bu-b", "bu-a"}, BusinessUnitNamesParsed: []string{"Build", "Advisory"},
		},
		// The same team under a second name is still one option.
		{ID: "options-renamed", ToolName: "Read", Timestamp: now, Status: "success", UserID: &userID, UserName: &userName,
			TeamIDsParsed: []string{"team-a"}, TeamNamesParsed: []string{"Alpha Renamed"}},
		// A scalar team with no array, as the virtual key path writes it.
		{ID: "options-scalar", ToolName: "Read", Timestamp: now, Status: "success", UserID: &userID, UserName: &userName, TeamID: &soloTeamID, TeamName: &soloTeamName},
		{ID: "options-nameless", ToolName: "Read", Timestamp: now, Status: "success", UserID: &namelessUserID},
		{ID: "options-old", ToolName: "Read", Timestamp: now.AddDate(0, 0, -(defaultFilterDataCutoffDays + 1)), Status: "success", UserID: &oldUserID},
	}))

	options := func(ctx context.Context, idCol, nameCol string, limit int, query string) []KeyPairResult {
		t.Helper()
		pairs, err := store.GetDistinctMCPKeyPairs(ctx, idCol, nameCol, limit, query)
		require.NoError(t, err)
		return pairs
	}
	// Where a nameless id sorts follows the database's NULL ordering, as on the Relay Logs options.
	require.ElementsMatch(t, []KeyPairResult{{ID: namelessUserID, Name: ""}, {ID: userID, Name: userName}}, options(ctx, "user_id", "user_name", 100, ""))
	require.Equal(t, []KeyPairResult{{ID: "team-a", Name: "Alpha Renamed"}, {ID: "team-b", Name: "Beta"}, {ID: soloTeamID, Name: soloTeamName}}, options(ctx, "team_id", "team_name", 100, ""))
	require.Equal(t, []KeyPairResult{{ID: "customer-a", Name: "Acme"}, {ID: "customer-b", Name: "Bravo"}}, options(ctx, "customer_id", "customer_name", 100, ""))
	require.Equal(t, []KeyPairResult{{ID: "bu-a", Name: "Advisory"}, {ID: "bu-b", Name: "Build"}}, options(ctx, "business_unit_id", "business_unit_name", 100, ""))
	require.Equal(t, []KeyPairResult{{ID: projectID, Name: projectName}}, options(ctx, "project_id", "project_name", 100, ""))
	require.Equal(t, []KeyPairResult{{ID: "team-b", Name: "Beta"}}, options(ctx, "team_id", "team_name", 1, "et"))

	scoped := queryscope.WithQueryScope(ctx, func(db *gorm.DB) *gorm.DB {
		return db.Where("user_id = ?", namelessUserID)
	})
	require.Empty(t, options(scoped, "team_id", "team_name", 100, ""))
}

// TestPostgresMCPKeyPairsAndAttributionFilters runs the MCP option and attribution checks on Postgres,
// whose fan-out and membership SQL (IS JSON ARRAY, a lateral join) differ from SQLite's and ClickHouse's.
func TestPostgresMCPKeyPairsAndAttributionFilters(t *testing.T) {
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available")
	}
	require.NoError(t, db.AutoMigrate(&MCPToolLog{}))
	store := &RDBLogStore{db: db, logger: bifrost.NewDefaultLogger(schemas.LogLevelInfo)}
	// The logstore_test schema is the suite's own, so each check starts from an empty table.
	clear := func() { require.NoError(t, db.Exec("DELETE FROM mcp_tool_logs").Error) }
	t.Cleanup(clear)
	clear()
	assertMCPKeyPairs(t, store, store.BatchCreateMCPToolLogsIfNotExists)
	clear()
	assertMCPAttributionFilters(t, store, store.BatchCreateMCPToolLogsIfNotExists)
}

// TestGetDistinctMCPKeyPairsAppliesDimensionCeiling verifies the MCP filter dropdowns list only the
// organisations the caller may be shown, like the LLM and Relay dropdowns.
func TestGetDistinctMCPKeyPairsAppliesDimensionCeiling(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&MCPToolLog{}))
	store := &RDBLogStore{db: db, logger: bifrost.NewDefaultLogger(schemas.LogLevelInfo)}
	userID, userName := "user-a", "MCP User"
	otherID, otherName := "user-b", "Other User"
	require.NoError(t, store.BatchCreateMCPToolLogsIfNotExists(context.Background(), []*MCPToolLog{
		{
			ID: "ceiling-a", ToolName: "Read", Timestamp: time.Now().UTC(), Status: "success", UserID: &userID, UserName: &userName,
			TeamIDsParsed: []string{"team-a", "team-hidden"}, TeamNamesParsed: []string{"Alpha", "Hidden"},
			CustomerIDsParsed: []string{"customer-a", "customer-hidden"}, CustomerNamesParsed: []string{"Acme", "Hidden Co"},
			BusinessUnitIDsParsed: []string{"bu-hidden"}, BusinessUnitNamesParsed: []string{"Hidden BU"},
		},
		{ID: "ceiling-b", ToolName: "Read", Timestamp: time.Now().UTC(), Status: "success", UserID: &otherID, UserName: &otherName},
	}))

	allowed := map[string][]string{"team_id": {"team-a"}, "customer_id": {"customer-a"}, "business_unit_id": {}, "user_id": {userID}}
	ctx := queryscope.WithDimensionScope(context.Background(), func(col string) ([]string, bool) {
		ids, ok := allowed[col]
		return ids, ok
	})
	for _, tc := range []struct {
		idCol, nameCol string
		want           []KeyPairResult
	}{
		{"team_id", "team_name", []KeyPairResult{{ID: "team-a", Name: "Alpha"}}},
		{"customer_id", "customer_name", []KeyPairResult{{ID: "customer-a", Name: "Acme"}}},
		{"business_unit_id", "business_unit_name", nil},
		{"user_id", "user_name", []KeyPairResult{{ID: userID, Name: userName}}},
	} {
		pairs, err := store.GetDistinctMCPKeyPairs(ctx, tc.idCol, tc.nameCol, 100, "")
		require.NoError(t, err)
		if tc.want == nil {
			require.Empty(t, pairs, tc.idCol)
			continue
		}
		require.Equal(t, tc.want, pairs, tc.idCol)
	}

	_, err = store.GetDistinctMCPKeyPairs(context.Background(), "tool_name", "tool_name", 100, "")
	require.Error(t, err)
}

// TestMCPStatsAndHistogramsCountFinishedCalls checks the MCP stats and histograms count calls as the
// LLM and Relay logs do.
func TestMCPStatsAndHistogramsCountFinishedCalls(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&MCPToolLog{}))
	store := &RDBLogStore{db: db, logger: bifrost.NewDefaultLogger(schemas.LogLevelInfo)}
	assertMCPFinishedCallAggregates(t, store, store.BatchCreateMCPToolLogsIfNotExists)
}

// TestPostgresMCPStatsAndHistograms runs the aggregate checks on Postgres, whose latency percentiles
// come from percentile_cont rather than the in-process ranking SQLite uses.
func TestPostgresMCPStatsAndHistograms(t *testing.T) {
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available")
	}
	require.NoError(t, db.AutoMigrate(&MCPToolLog{}))
	store := &RDBLogStore{db: db, logger: bifrost.NewDefaultLogger(schemas.LogLevelInfo)}
	// The logstore_test schema is the suite's own, so the check starts from an empty table.
	clear := func() { require.NoError(t, db.Exec("DELETE FROM mcp_tool_logs").Error) }
	t.Cleanup(clear)
	clear()
	assertMCPFinishedCallAggregates(t, store, store.BatchCreateMCPToolLogsIfNotExists)
}

// assertMCPFinishedCallAggregates pins how MCP stats and histograms count calls, as the LLM and Relay
// logs do: a cancelled call is a finished call, counted in the success rate, the average latency, the
// cost and the volume histogram, while a call still running counts only toward the total. The latency
// histogram reads the same finished calls, and has no overhead to report for MCP tool calls. create is
// the store's own writer, since ClickHouse writes MCP tool logs its own way.
func assertMCPFinishedCallAggregates(t *testing.T, store *RDBLogStore, create func(context.Context, []*MCPToolLog) error) {
	t.Helper()
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)
	ptr := func(v float64) *float64 { return &v }
	require.NoError(t, create(ctx, []*MCPToolLog{
		{ID: "agg-success", ToolName: "Read", Timestamp: at, Status: "success", Latency: ptr(10), Cost: ptr(0.1)},
		{ID: "agg-error", ToolName: "Read", Timestamp: at, Status: "error", Latency: ptr(20), Cost: ptr(0.2)},
		{ID: "agg-cancelled", ToolName: "Read", Timestamp: at, Status: "cancelled", Latency: ptr(30), Cost: ptr(0.3)},
		{ID: "agg-processing", ToolName: "Read", Timestamp: at, Status: "processing", Latency: ptr(1000), Cost: ptr(5)},
	}))
	start, end := at.Add(-time.Hour), at.Add(time.Hour)
	filters := MCPToolLogSearchFilters{StartTime: &start, EndTime: &end}

	stats, err := store.GetMCPToolLogStats(ctx, filters)
	require.NoError(t, err)
	require.EqualValues(t, 4, stats.TotalExecutions)
	require.EqualValues(t, 1, stats.SuccessCount)
	require.EqualValues(t, 1, stats.ErrorCount)
	require.EqualValues(t, 1, stats.CancelledCount)
	require.InDelta(t, 100.0/3, stats.SuccessRate, 1e-9)
	require.InDelta(t, 20, stats.AverageLatency, 1e-9)
	require.InDelta(t, 0.6, stats.TotalCost, 1e-9)

	histogram, err := store.GetMCPHistogram(ctx, filters, 3600)
	require.NoError(t, err)
	var total MCPHistogramBucket
	for _, bucket := range histogram.Buckets {
		total.Count += bucket.Count
		total.Success += bucket.Success
		total.Error += bucket.Error
		total.Cancelled += bucket.Cancelled
	}
	require.Equal(t, MCPHistogramBucket{Count: 3, Success: 1, Error: 1, Cancelled: 1}, total)

	latency, err := store.GetMCPLatencyHistogram(ctx, filters, 3600)
	require.NoError(t, err)
	require.EqualValues(t, 3600, latency.BucketSizeSeconds)
	var timed []LatencyHistogramBucket
	for _, bucket := range latency.Buckets {
		if bucket.TotalRequests > 0 {
			timed = append(timed, bucket)
		}
	}
	require.Len(t, timed, 1)
	require.EqualValues(t, 3, timed[0].TotalRequests)
	require.InDelta(t, 20, timed[0].AvgLatency, 1e-9)
	require.Greater(t, timed[0].P95Latency, 20.0)
	require.LessOrEqual(t, timed[0].P95Latency, 30.0)
	require.Zero(t, timed[0].AvgOverhead)
	require.Zero(t, timed[0].P95Overhead)
}

// TestMCPAttributionFiltersApplyToRowsAndStats checks each stored scope filters records, stats and the histogram.
func TestMCPAttributionFiltersApplyToRowsAndStats(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&MCPToolLog{}))
	store := &RDBLogStore{db: db, logger: bifrost.NewDefaultLogger(schemas.LogLevelInfo)}
	assertMCPAttributionFilters(t, store, store.BatchCreateMCPToolLogsIfNotExists)
}

// assertMCPAttributionFilters pins every attribution filter on MCP tool logs, including ids held only
// in the team, customer and business-unit arrays.
// create is the store's own writer, since ClickHouse writes MCP tool logs its own way.
func assertMCPAttributionFilters(t *testing.T, store *RDBLogStore, create func(context.Context, []*MCPToolLog) error) {
	t.Helper()
	ctx := context.Background()
	a, b := "a", "b"
	now := time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, create(ctx, []*MCPToolLog{
		{
			ID: a, ToolName: "Read", Timestamp: now, Status: "success", UserID: &a, TeamID: &a, CustomerID: &a, BusinessUnitID: &a, ProjectID: &a, DeviceID: &a,
			TeamIDsParsed: []string{"team-shared", a}, CustomerIDsParsed: []string{"customer-shared", a}, BusinessUnitIDsParsed: []string{"bu-shared", a},
		},
		{ID: b, ToolName: "Read", Timestamp: now.Add(time.Second), Status: "error", UserID: &b, TeamID: &b, CustomerID: &b, BusinessUnitID: &b, ProjectID: &b, DeviceID: &b},
	}))
	start, end := now.Add(-time.Minute), now.Add(time.Minute)
	for _, filters := range []MCPToolLogSearchFilters{
		{UserIDs: []string{a}},
		{TeamIDs: []string{a}},
		{TeamIDs: []string{"team-shared"}},
		// Two ids, one matching: ClickHouse bound a list inside hasAny's brackets as one tuple.
		{TeamIDs: []string{"team-shared", "team-none"}},
		{CustomerIDs: []string{a}},
		{CustomerIDs: []string{"customer-shared"}},
		{CustomerIDs: []string{"customer-shared", "customer-none"}},
		{BusinessUnitIDs: []string{a}},
		{BusinessUnitIDs: []string{"bu-shared"}},
		{BusinessUnitIDs: []string{"bu-shared", "bu-none"}},
		{ProjectIDs: []string{a}},
		{DeviceIDs: []string{a}},
	} {
		filters.StartTime, filters.EndTime = &start, &end
		result, err := store.SearchMCPToolLogs(ctx, filters, PaginationOptions{Limit: 10, SortBy: "timestamp", Order: "desc"})
		require.NoError(t, err)
		require.Len(t, result.Logs, 1, "filters %+v", filters)
		require.Equal(t, a, result.Logs[0].ID)
		stats, err := store.GetMCPToolLogStats(ctx, filters)
		require.NoError(t, err)
		require.EqualValues(t, 1, stats.TotalExecutions)
		require.EqualValues(t, 100, stats.SuccessRate)
		histogram, err := store.GetMCPHistogram(ctx, filters, 60)
		require.NoError(t, err)
		var histogramCount int64
		for _, bucket := range histogram.Buckets {
			histogramCount += bucket.Count
		}
		require.EqualValues(t, 1, histogramCount, "filters %+v", filters)
	}
}

package logstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// A generation is one client request that reached the model step, counted once however many
// attempts (fallbacks) it took, as OpenRouter counts its generations. A refusal is a client
// request the product refused before any provider was called (a limit, the data policy, a
// switched-off request type: the API-08 contract, error.metadata.limit_source); it is not a
// generation and is counted apart, by its reason. Both are read for one gateway customer (one
// organization of the control plane), so the summary is the activity page's source of request
// counts and timings (docs/platform/SPEC.md ACT-01, ACT-02).
//
// Which rows a generation is, in a chain where the root row is the first attempt and each
// fallback attempt is a child row (parent_request_id = the root's id, fallback_index > 0):
//   - the attempt that answered (status success): the root, or the fallback that succeeded;
//   - else the root, when no attempt of the chain succeeded: a failed generation.
//
// Each chain has at most one successful attempt, so each generation is exactly one row, and that
// row names the model and provider that served it.

// ErrGenerationSummaryUnsupported is returned on a store whose dialect the summary has no SQL for.
var ErrGenerationSummaryUnsupported = errors.New("generation summary is not supported on this log store")

// GenerationDay is one UTC day of a customer's requests.
type GenerationDay struct {
	Day         string `json:"day"` // YYYY-MM-DD, UTC
	Generations int64  `json:"generations"`
	Failed      int64  `json:"failed"` // generations no attempt of which succeeded
	Refused     int64  `json:"refused"`
}

// GenerationModel is the generations one model of one provider served (or, failing, was asked for).
type GenerationModel struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	Generations int64  `json:"generations"`
}

// GenerationKey is the generations made with one virtual key.
type GenerationKey struct {
	VirtualKeyID string `json:"virtual_key_id"`
	Generations  int64  `json:"generations"`
}

// GenerationRefusal is the requests refused for one reason.
type GenerationRefusal struct {
	LimitSource string `json:"limit_source"`
	Reason      string `json:"reason"`
	StatusCode  int    `json:"status_code"`
	Requests    int64  `json:"requests"`
}

// LatencySummary is an average and two percentiles in milliseconds; all zero when nothing was measured.
type LatencySummary struct {
	Avg float64 `json:"avg"`
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
}

// GenerationTimings are measured on the attempt that answered each successful generation.
type GenerationTimings struct {
	// Measured is how many successful generations the figures cover.
	Measured int64 `json:"measured"`
	Streamed int64 `json:"streamed"`
	// TimeToFirstToken: request received to first chunk for a stream; the whole latency otherwise,
	// as OpenRouter's generation latency.
	TimeToFirstToken LatencySummary `json:"time_to_first_token_ms"`
	// Latency is request received to response finished.
	Latency LatencySummary `json:"latency_ms"`
	// GenerationTime is the provider's time on the answering attempt (upstream latency).
	GenerationTimeAvg float64 `json:"generation_time_ms_avg"`
	// ThroughputAvg is completion tokens per second of generation time, averaged over generations.
	ThroughputAvg float64 `json:"throughput_tokens_per_second_avg"`
}

// GenerationSummary is one customer's requests over [Start, End).
type GenerationSummary struct {
	Start    time.Time           `json:"start_time"`
	End      time.Time           `json:"end_time"`
	ByDay    []GenerationDay     `json:"by_day"`
	ByModel  []GenerationModel   `json:"by_model"`
	ByKey    []GenerationKey     `json:"by_virtual_key"`
	Refusals []GenerationRefusal `json:"refusals"`
	Timings  GenerationTimings   `json:"timings"`
}

// GenerationAttempt is one attempt of a request: the root row or one of its fallback rows.
type GenerationAttempt struct {
	ID               string    `json:"id"`
	ParentRequestID  string    `json:"parent_request_id,omitempty"`
	FallbackIndex    int       `json:"fallback_index"`
	Timestamp        time.Time `json:"timestamp"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	Status           string    `json:"status"`
	StatusCode       *int      `json:"status_code,omitempty"`
	ErrorType        string    `json:"error_type,omitempty"`
	LimitSource      string    `json:"limit_source,omitempty"`
	Reason           string    `json:"reason,omitempty"`
	Stream           bool      `json:"stream"`
	Latency          *float64  `json:"latency,omitempty"`
	UpstreamLatency  *float64  `json:"upstream_latency,omitempty"`
	TimeToFirstToken *float64  `json:"time_to_first_token,omitempty"`
	CustomerID       string    `json:"customer_id,omitempty"`
}

// generationExcludedObjects call no model: listing models is not a request anyone pays for.
var generationExcludedObjects = []string{"list_models", "model_retrieve"}

// errorPathExpr extracts a text value at error_details -> error -> metadata -> field (or error -> field
// when metadata is false), per dialect; NULL when absent.
func errorPathExpr(dialect, alias, field string, metadata bool) (string, bool) {
	col := alias + ".error_details"
	switch dialect {
	case "postgres":
		path := "{error," + field + "}"
		if metadata {
			path = "{error,metadata," + field + "}"
		}
		return fmt.Sprintf(`(CASE WHEN %[1]s IS NOT NULL AND %[1]s IS JSON OBJECT THEN %[1]s::jsonb #>> '%[2]s' ELSE NULL END)`, col, path), true
	case "sqlite":
		path := "$.error." + field
		if metadata {
			path = "$.error.metadata." + field
		}
		return fmt.Sprintf(`(CASE WHEN %[1]s IS NOT NULL AND json_valid(%[1]s) THEN CAST(json_extract(%[1]s, '%[2]s') AS TEXT) ELSE NULL END)`, col, path), true
	}
	return "", false
}

// statusCodeExpr is error_details.status_code as text; NULL on a row with no error.
func statusCodeExpr(dialect, alias string) (string, bool) {
	return jsonTopLevelNumberExpr(dialect, alias+".error_details", "status_code")
}

// isRootExpr is the first attempt of a request. fallback_index is set by the gateway, never by the
// client; parent_request_id is not a test of it, because a client's session baggage can set it on a
// first attempt too (see rdbrootsonly_test.go).
func isRootExpr(alias string) string {
	return alias + ".fallback_index = 0"
}

// GetGenerationSummary counts a customer's generations and refusals in [start, end) and times the
// successful generations. See the top of this file for what each is.
func (s *RDBLogStore) GetGenerationSummary(ctx context.Context, customerID string, start, end time.Time) (*GenerationSummary, error) {
	dialect := s.db.Dialector.Name()
	limitSource, ok := errorPathExpr(dialect, "l", "limit_source", true)
	if !ok {
		return nil, ErrGenerationSummaryUnsupported
	}
	reason, _ := errorPathExpr(dialect, "l", "reason", true)
	statusCode, _ := statusCodeExpr(dialect, "l")
	day := dayBucketExpr(dialect)

	// rows of this customer and window that ended, outside the excluded objects
	scope := "l.customer_id = ? AND l.timestamp >= ? AND l.timestamp < ? AND l.status IN ('success', 'error', 'cancelled') AND l.object_type NOT IN ?"
	scopeArgs := []any{customerID, start, end, generationExcludedObjects}
	root := isRootExpr("l")
	notRefused := limitSource + " IS NULL"
	// a generation's row: the answering attempt, or a root none of whose attempts succeeded
	generation := fmt.Sprintf(`(%[1]s AND (
		l.status = 'success'
		OR (%[2]s AND l.status <> 'success' AND NOT EXISTS (
			SELECT 1 FROM logs c WHERE c.parent_request_id = l.id AND c.id <> l.id AND c.fallback_index > 0 AND c.status = 'success'))))`,
		notRefused, root)
	const failed = "CASE WHEN l.status = 'success' THEN 0 ELSE 1 END"

	db := s.ScopedDB(ctx)
	out := &GenerationSummary{Start: start, End: end}

	// by day: generations and failed ones, then refusals
	type dayRow struct {
		Bucket int64
		Total  int64
		Failed int64
	}
	var days []dayRow
	if err := db.Raw(fmt.Sprintf(`SELECT bucket, COUNT(*) AS total, SUM(failed) AS failed FROM (
			SELECT %s AS bucket, %s AS failed FROM logs l WHERE %s AND %s) g GROUP BY bucket`,
		day, failed, scope, generation), scopeArgs...).Scan(&days).Error; err != nil {
		return nil, fmt.Errorf("generation summary by day: %w", err)
	}
	byDay := map[string]*GenerationDay{}
	dayOf := func(bucket int64) *GenerationDay {
		key := time.Unix(bucket, 0).UTC().Format("2006-01-02")
		if d, ok := byDay[key]; ok {
			return d
		}
		d := &GenerationDay{Day: key}
		byDay[key] = d
		return d
	}
	for _, row := range days {
		d := dayOf(row.Bucket)
		d.Generations += row.Total
		d.Failed += row.Failed
	}

	type refusalRow struct {
		Bucket      int64
		LimitSource string
		Reason      *string
		StatusCode  *string
		Requests    int64
	}
	var refusals []refusalRow
	if err := db.Raw(fmt.Sprintf(`SELECT bucket, limit_source, reason, status_code, COUNT(*) AS requests FROM (
			SELECT %s AS bucket, %s AS limit_source, %s AS reason, %s AS status_code FROM logs l
			 WHERE %s AND %s AND %s IS NOT NULL) r GROUP BY bucket, limit_source, reason, status_code`,
		day, limitSource, reason, statusCode, scope, root, limitSource), scopeArgs...).Scan(&refusals).Error; err != nil {
		return nil, fmt.Errorf("generation summary refusals: %w", err)
	}
	type refusalKey struct {
		source, reason string
		status         int
	}
	byReason := map[refusalKey]int64{}
	for _, row := range refusals {
		dayOf(row.Bucket).Refused += row.Requests
		key := refusalKey{source: row.LimitSource}
		if row.Reason != nil {
			key.reason = *row.Reason
		}
		if row.StatusCode != nil {
			_, _ = fmt.Sscanf(*row.StatusCode, "%d", &key.status)
		}
		byReason[key] += row.Requests
	}
	for key, n := range byReason {
		out.Refusals = append(out.Refusals, GenerationRefusal{LimitSource: key.source, Reason: key.reason, StatusCode: key.status, Requests: n})
	}
	sort.Slice(out.Refusals, func(i, j int) bool {
		a, b := out.Refusals[i], out.Refusals[j]
		if a.Requests != b.Requests {
			return a.Requests > b.Requests
		}
		return a.LimitSource+a.Reason < b.LimitSource+b.Reason
	})
	for _, d := range byDay {
		out.ByDay = append(out.ByDay, *d)
	}
	sort.Slice(out.ByDay, func(i, j int) bool { return out.ByDay[i].Day < out.ByDay[j].Day })

	if err := db.Raw(fmt.Sprintf(`SELECT l.provider AS provider, l.model AS model, COUNT(*) AS generations FROM logs l
		 WHERE %s AND %s GROUP BY l.provider, l.model ORDER BY generations DESC, l.provider, l.model`, scope, generation),
		scopeArgs...).Scan(&out.ByModel).Error; err != nil {
		return nil, fmt.Errorf("generation summary by model: %w", err)
	}
	if err := db.Raw(fmt.Sprintf(`SELECT COALESCE(l.virtual_key_id, '') AS virtual_key_id, COUNT(*) AS generations FROM logs l
		 WHERE %s AND %s GROUP BY COALESCE(l.virtual_key_id, '') ORDER BY generations DESC, virtual_key_id`, scope, generation),
		scopeArgs...).Scan(&out.ByKey).Error; err != nil {
		return nil, fmt.Errorf("generation summary by key: %w", err)
	}

	if err := s.generationTimings(ctx, dialect, &out.Timings, scope, generation, scopeArgs); err != nil {
		return nil, err
	}
	if out.ByDay == nil {
		out.ByDay = []GenerationDay{}
	}
	if out.ByModel == nil {
		out.ByModel = []GenerationModel{}
	}
	if out.ByKey == nil {
		out.ByKey = []GenerationKey{}
	}
	if out.Refusals == nil {
		out.Refusals = []GenerationRefusal{}
	}
	return out, nil
}

// dayBucketExpr is the UTC day of l.timestamp as unix seconds (unixBucketExpr, on the l alias).
func dayBucketExpr(dialect string) string {
	if dialect == "sqlite" {
		return "(CAST(strftime('%s', l.timestamp) AS INTEGER) / 86400) * 86400"
	}
	return "CAST(FLOOR(EXTRACT(EPOCH FROM l.timestamp) / 86400) * 86400 AS BIGINT)"
}

// timing expressions over a successful generation's row: time to first token falls back to the
// whole latency for a request that was not streamed, or a stream whose first chunk was not timed
const (
	ttftExpr       = "COALESCE(l.time_to_first_token, l.latency)"
	throughputExpr = "CASE WHEN l.upstream_latency > 0 THEN l.completion_tokens * 1000.0 / l.upstream_latency ELSE NULL END"
)

func (s *RDBLogStore) generationTimings(ctx context.Context, dialect string, out *GenerationTimings, scope, generation string, args []any) error {
	db := s.ScopedDB(ctx)
	where := fmt.Sprintf("%s AND %s AND l.status = 'success'", scope, generation)
	var row struct {
		Measured      int64    `gorm:"column:measured"`
		Streamed      int64    `gorm:"column:streamed"`
		TTFTAvg       *float64 `gorm:"column:ttft_avg"`
		LatencyAvg    *float64 `gorm:"column:latency_avg"`
		GenerationAvg *float64 `gorm:"column:generation_avg"`
		ThroughputAvg *float64 `gorm:"column:throughput_avg"`
		TTFTP50       *float64 `gorm:"column:ttft_p50"`
		TTFTP90       *float64 `gorm:"column:ttft_p90"`
		LatencyP50    *float64 `gorm:"column:latency_p50"`
		LatencyP90    *float64 `gorm:"column:latency_p90"`
	}
	percentiles := ""
	if dialect == "postgres" {
		percentiles = fmt.Sprintf(`,
			percentile_cont(0.5) WITHIN GROUP (ORDER BY %[1]s) AS ttft_p50,
			percentile_cont(0.9) WITHIN GROUP (ORDER BY %[1]s) AS ttft_p90,
			percentile_cont(0.5) WITHIN GROUP (ORDER BY l.latency) AS latency_p50,
			percentile_cont(0.9) WITHIN GROUP (ORDER BY l.latency) AS latency_p90`, ttftExpr)
	}
	if err := db.Raw(fmt.Sprintf(`SELECT COUNT(*) AS measured,
			SUM(CASE WHEN l.stream THEN 1 ELSE 0 END) AS streamed,
			AVG(%s) AS ttft_avg, AVG(l.latency) AS latency_avg, AVG(l.upstream_latency) AS generation_avg,
			AVG(%s) AS throughput_avg%s
		  FROM logs l WHERE %s`, ttftExpr, throughputExpr, percentiles, where), args...).Scan(&row).Error; err != nil {
		return fmt.Errorf("generation timings: %w", err)
	}
	out.Measured, out.Streamed = row.Measured, row.Streamed
	out.TimeToFirstToken.Avg = orZero(row.TTFTAvg)
	out.Latency.Avg = orZero(row.LatencyAvg)
	out.GenerationTimeAvg = orZero(row.GenerationAvg)
	out.ThroughputAvg = orZero(row.ThroughputAvg)
	if dialect == "postgres" {
		out.TimeToFirstToken.P50, out.TimeToFirstToken.P90 = orZero(row.TTFTP50), orZero(row.TTFTP90)
		out.Latency.P50, out.Latency.P90 = orZero(row.LatencyP50), orZero(row.LatencyP90)
		return nil
	}
	// other dialects: percentiles in Go over the measured values (development and tests)
	var values []struct {
		TTFT    *float64
		Latency *float64
	}
	if err := db.Raw(fmt.Sprintf(`SELECT %s AS ttft, l.latency AS latency FROM logs l WHERE %s`, ttftExpr, where), args...).Scan(&values).Error; err != nil {
		return fmt.Errorf("generation timing values: %w", err)
	}
	var ttft, latency []float64
	for _, v := range values {
		if v.TTFT != nil {
			ttft = append(ttft, *v.TTFT)
		}
		if v.Latency != nil {
			latency = append(latency, *v.Latency)
		}
	}
	sort.Float64s(ttft)
	sort.Float64s(latency)
	out.TimeToFirstToken.P50, out.TimeToFirstToken.P90 = computePercentile(ttft, 0.5), computePercentile(ttft, 0.9)
	out.Latency.P50, out.Latency.P90 = computePercentile(latency, 0.5), computePercentile(latency, 0.9)
	return nil
}

func orEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func orZero(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// GetGenerationAttempts returns a request's root row and its fallback rows, in attempt order. Empty
// when the gateway has no row with that id (never logged, or past retention).
func (s *RDBLogStore) GetGenerationAttempts(ctx context.Context, requestID string) ([]GenerationAttempt, error) {
	dialect := s.db.Dialector.Name()
	limitSource, ok := errorPathExpr(dialect, "l", "limit_source", true)
	if !ok {
		return nil, ErrGenerationSummaryUnsupported
	}
	reason, _ := errorPathExpr(dialect, "l", "reason", true)
	errorType, _ := errorPathExpr(dialect, "l", "type", false)
	statusCode, _ := statusCodeExpr(dialect, "l")
	var rows []struct {
		ID               string
		ParentRequestID  *string
		FallbackIndex    int
		Timestamp        time.Time
		Provider         string
		Model            string
		Status           string
		StatusCode       *string
		ErrorType        *string
		LimitSource      *string
		Reason           *string
		Stream           bool
		Latency          *float64
		UpstreamLatency  *float64
		TimeToFirstToken *float64
		CustomerID       *string
	}
	if err := s.ScopedDB(ctx).Raw(fmt.Sprintf(`SELECT l.id, l.parent_request_id, l.fallback_index, l.timestamp, l.provider, l.model, l.status,
			%s AS status_code, %s AS error_type, %s AS limit_source, %s AS reason,
			l.stream, l.latency, l.upstream_latency, l.time_to_first_token, l.customer_id
		  FROM logs l
		 WHERE l.id = ? OR (l.parent_request_id = ? AND l.id <> ? AND l.fallback_index > 0)
		 ORDER BY l.fallback_index, l.timestamp`, statusCode, errorType, limitSource, reason),
		requestID, requestID, requestID).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("generation attempts: %w", err)
	}
	attempts := make([]GenerationAttempt, 0, len(rows))
	for _, r := range rows {
		a := GenerationAttempt{
			ID: r.ID, FallbackIndex: r.FallbackIndex, Timestamp: r.Timestamp.UTC(), Provider: r.Provider, Model: r.Model,
			Status: r.Status, Stream: r.Stream, Latency: r.Latency, UpstreamLatency: r.UpstreamLatency, TimeToFirstToken: r.TimeToFirstToken,
		}
		if r.ParentRequestID != nil && *r.ParentRequestID != r.ID {
			a.ParentRequestID = *r.ParentRequestID
		}
		if r.StatusCode != nil {
			var code int
			if _, err := fmt.Sscanf(*r.StatusCode, "%d", &code); err == nil {
				a.StatusCode = &code
			}
		} else if r.Status == "success" {
			code := 200
			a.StatusCode = &code
		}
		a.ErrorType, a.LimitSource, a.Reason, a.CustomerID = orEmpty(r.ErrorType), orEmpty(r.LimitSource), orEmpty(r.Reason), orEmpty(r.CustomerID)
		attempts = append(attempts, a)
	}
	return attempts, nil
}

// GetGenerationSummary reads the database rows: the summary needs no payload.
func (h *HybridLogStore) GetGenerationSummary(ctx context.Context, customerID string, start, end time.Time) (*GenerationSummary, error) {
	inner, ok := h.inner.(generationReader)
	if !ok {
		return nil, ErrGenerationSummaryUnsupported
	}
	return inner.GetGenerationSummary(ctx, customerID, start, end)
}

// GetGenerationAttempts reads the database rows: attempts carry no payload.
func (h *HybridLogStore) GetGenerationAttempts(ctx context.Context, requestID string) ([]GenerationAttempt, error) {
	inner, ok := h.inner.(generationReader)
	if !ok {
		return nil, ErrGenerationSummaryUnsupported
	}
	return inner.GetGenerationAttempts(ctx, requestID)
}

// generationReader is the store that can summarize generations; the logging plugin asks its store
// for it rather than widening LogStore.
type generationReader interface {
	GetGenerationSummary(ctx context.Context, customerID string, start, end time.Time) (*GenerationSummary, error)
	GetGenerationAttempts(ctx context.Context, requestID string) ([]GenerationAttempt, error)
}

// GenerationReader is exported for the logging plugin.
type GenerationReader = generationReader

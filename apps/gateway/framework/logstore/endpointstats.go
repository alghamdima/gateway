package logstore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/bytedance/sonic"
)

// Endpoint statistics (Lemru, SPEC CAT-11 and CAT-12, docs/platform/plans/model-page.md).
//
// An endpoint is one way a model is served: the provider, the model as that provider names it,
// the provider key that carried the call and the service tier the provider served it at. The
// control plane keeps one row per endpoint and UTC hour, filled from here, so a model page never
// scans the log and a chart can reach further back than the log is kept.
//
// What is counted is the attempt, not the request: an endpoint's speed and reliability are what
// each call to it did, whichever attempt of a fallback chain it was. Only attempts that reached a
// provider count: a refusal the product made (it carries error.metadata.limit_source) never left
// the gateway, and listing models calls no model.
//
// Which key carried a row: the selected key when it answered. Core keeps selected_key_id for
// answers only and says to use attempt_trail for failures (core/bifrost.go), so a failed row is
// counted on the key of its last trail record. A row that tried several keys also counts one
// failed attempt on each earlier key that failed, read from the same trail.
//
// The service tier is the one the provider reported. Its standard tier is one endpoint whatever
// it is called: no tier, "default", and "auto" (what a request asked for and all a failed row
// knows) are the same endpoint, written "".
//
// Latency, time to first token and throughput are returned as histograms over fixed buckets, not
// as percentiles: percentiles of two hours cannot be combined, bucket counts can, so any window
// and any group of endpoints gets its percentiles from the sum of the hours.

// ErrEndpointStatsUnsupported is returned on a store whose dialect the statistics have no SQL for.
var ErrEndpointStatsUnsupported = errors.New("endpoint statistics are not supported on this log store")

// EndpointStatsBuckets is how many buckets each histogram has.
const EndpointStatsBuckets = 64

// endpointStatsGrowth is the ratio between two bucket bounds: a percentile read from a bucket is
// within about 10% of the true value.
const endpointStatsGrowth = 1.2

// The lowest bound of each histogram; bucket i holds values in [first*growth^(i-1), first*growth^i),
// bucket 0 everything below first, the last bucket everything above.
const (
	endpointLatencyFirstMs      = 20.0 // 20 ms .. about 27 min
	endpointThroughputFirstTokS = 0.5  // 0.5 tokens/s .. about 40,000
)

// EndpointStatsBounds are the upper bounds of the histogram buckets (the last bucket has none), in
// milliseconds for the two latencies and in tokens per second for throughput. Returned with every
// answer so the reader never has to know the constants above.
type EndpointStatsBounds struct {
	LatencyMs  []float64 `json:"latency_ms"`
	Throughput []float64 `json:"throughput_tokens_per_second"`
}

func endpointBounds(first float64) []float64 {
	bounds := make([]float64, EndpointStatsBuckets-1)
	for i := range bounds {
		bounds[i] = first * math.Pow(endpointStatsGrowth, float64(i))
	}
	return bounds
}

// EndpointHour is one endpoint's attempts in one UTC hour.
type EndpointHour struct {
	Hour        time.Time `json:"hour"` // the start of the hour, UTC
	Provider    string    `json:"provider"`
	Model       string    `json:"model"`
	KeyID       string    `json:"key_id"`       // the provider key that carried the calls; "" for a keyless provider
	ServiceTier string    `json:"service_tier"` // as the provider reported it; "" when it reported none

	Attempts  int64 `json:"attempts"`  // calls that reached the provider and ended
	Succeeded int64 `json:"succeeded"` // of which answered
	Streamed  int64 `json:"streamed"`  // of the answered, how many were streams

	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`   // part of the completion tokens
	CachedReadTokens int64 `json:"cached_read_tokens"` // part of the prompt tokens

	// What the answered calls cost at the provider, in USD, split as the catalog priced them.
	InputCostUSD  float64 `json:"input_cost_usd"`
	OutputCostUSD float64 `json:"output_cost_usd"`
	CostUSD       float64 `json:"cost_usd"`

	// Histograms over the answered calls, EndpointStatsBuckets counts each. Latency is request
	// received to response finished; time to first token is measured on streams only; throughput
	// is completion tokens per second of the provider's own time.
	LatencyMs          []int64 `json:"latency_ms"`
	TimeToFirstTokenMs []int64 `json:"time_to_first_token_ms"`
	Throughput         []int64 `json:"throughput_tokens_per_second"`
}

// EndpointStats is every endpoint's hours in [Start, End).
type EndpointStats struct {
	Start  time.Time           `json:"start_time"`
	End    time.Time           `json:"end_time"`
	Bounds EndpointStatsBounds `json:"bounds"`
	Hours  []EndpointHour      `json:"hours"`
}

// hourBucketExpr is the UTC hour of l.timestamp as unix seconds.
func hourBucketExpr(dialect string) string {
	if dialect == "sqlite" {
		return "(CAST(strftime('%s', l.timestamp) AS INTEGER) / 3600) * 3600"
	}
	return "CAST(FLOOR(EXTRACT(EPOCH FROM l.timestamp) / 3600) * 3600 AS BIGINT)"
}

// reasoningTokensExpr reads completion_tokens_details.reasoning_tokens from the stored usage; 0
// when the row has no usage or the provider reported none.
func reasoningTokensExpr(dialect string) (string, bool) {
	switch dialect {
	case "postgres":
		return `COALESCE(CAST(CASE WHEN l.token_usage IS NOT NULL AND l.token_usage IS JSON OBJECT
			THEN l.token_usage::jsonb #>> '{completion_tokens_details,reasoning_tokens}' ELSE NULL END AS BIGINT), 0)`, true
	case "sqlite":
		return `COALESCE(CAST(CASE WHEN l.token_usage IS NOT NULL AND json_valid(l.token_usage)
			THEN json_extract(l.token_usage, '$.completion_tokens_details.reasoning_tokens') ELSE NULL END AS INTEGER), 0)`, true
	}
	return "", false
}

// endpointKeyExpr is the provider key a row is counted on (see the top of this file): the selected
// key, else the key of the last record of its attempt trail, else "".
func endpointKeyExpr(dialect string) (string, bool) {
	switch dialect {
	case "postgres":
		return `COALESCE(NULLIF(l.selected_key_id, ''),
			CASE WHEN l.attempt_trail IS NOT NULL AND l.attempt_trail IS JSON ARRAY THEN l.attempt_trail::jsonb -> -1 ->> 'key_id' ELSE NULL END, '')`, true
	case "sqlite":
		return `COALESCE(NULLIF(l.selected_key_id, ''),
			CASE WHEN l.attempt_trail IS NOT NULL AND json_valid(l.attempt_trail) THEN CAST(json_extract(l.attempt_trail, '$[#-1].key_id') AS TEXT) ELSE NULL END, '')`, true
	}
	return "", false
}

// trailLengthExpr is how many key attempts a row's trail records; 0 when it has none.
func trailLengthExpr(dialect string) string {
	if dialect == "sqlite" {
		return "(CASE WHEN l.attempt_trail IS NOT NULL AND json_valid(l.attempt_trail) THEN json_array_length(l.attempt_trail) ELSE 0 END)"
	}
	return "(CASE WHEN l.attempt_trail IS NOT NULL AND l.attempt_trail IS JSON ARRAY THEN jsonb_array_length(l.attempt_trail::jsonb) ELSE 0 END)"
}

// endpointTierExpr is the service tier a row is counted on: the provider's standard tier is "".
const endpointTierExpr = "(CASE WHEN l.service_tier IS NULL OR l.service_tier IN ('', 'auto', 'default') THEN '' ELSE l.service_tier END)"

// bucketExpr is the histogram bucket of value, as a CASE over the bounds: plain comparisons, so it
// reads the same on every dialect and needs no logarithm from the database.
func bucketExpr(value string, bounds []float64) string {
	var b strings.Builder
	b.WriteString("CASE")
	for i, bound := range bounds {
		fmt.Fprintf(&b, " WHEN %s < %g THEN %d", value, bound, i)
	}
	fmt.Fprintf(&b, " ELSE %d END", len(bounds))
	return b.String()
}

// The throughput of an answered call: completion tokens per second of the provider's own time.
const endpointThroughputExpr = "(l.completion_tokens * 1000.0 / l.upstream_latency)"

type endpointKey struct {
	bucket                              int64
	provider, model, keyID, serviceTier string
}

// GetEndpointStats returns every endpoint's attempts in [start, end), by UTC hour. Both times must
// be on the hour, so an hour is never returned in part.
func (s *RDBLogStore) GetEndpointStats(ctx context.Context, start, end time.Time) (*EndpointStats, error) {
	dialect := s.db.Dialector.Name()
	limitSource, ok := errorPathExpr(dialect, "l", "limit_source", true)
	if !ok {
		return nil, ErrEndpointStatsUnsupported
	}
	reasoning, _ := reasoningTokensExpr(dialect)
	keyID, _ := endpointKeyExpr(dialect)
	hour := hourBucketExpr(dialect)

	// attempts that reached a provider and ended
	scope := fmt.Sprintf(`l.timestamp >= ? AND l.timestamp < ? AND l.status IN ('success', 'error')
		AND l.object_type NOT IN ? AND %s IS NULL AND l.provider <> ''`, limitSource)
	args := []any{start, end, generationExcludedObjects}
	endpoint := fmt.Sprintf("l.provider, l.model, %s, %s", keyID, endpointTierExpr)
	const answered = "l.status = 'success'"

	db := s.ScopedDB(ctx)
	out := &EndpointStats{
		Start: start, End: end,
		Bounds: EndpointStatsBounds{
			LatencyMs:  endpointBounds(endpointLatencyFirstMs),
			Throughput: endpointBounds(endpointThroughputFirstTokS),
		},
	}

	type totalsRow struct {
		Bucket           int64
		Provider         string
		Model            string
		KeyID            string  `gorm:"column:key_id"`
		ServiceTier      string  `gorm:"column:service_tier"`
		Attempts         int64   `gorm:"column:attempts"`
		Succeeded        int64   `gorm:"column:succeeded"`
		Streamed         int64   `gorm:"column:streamed"`
		PromptTokens     int64   `gorm:"column:prompt_tokens"`
		CompletionTokens int64   `gorm:"column:completion_tokens"`
		ReasoningTokens  int64   `gorm:"column:reasoning_tokens"`
		CachedReadTokens int64   `gorm:"column:cached_read_tokens"`
		InputCost        float64 `gorm:"column:input_cost"`
		OutputCost       float64 `gorm:"column:output_cost"`
		Cost             float64 `gorm:"column:cost"`
	}
	var totals []totalsRow
	sumAnswered := func(expr string) string {
		return fmt.Sprintf("COALESCE(SUM(CASE WHEN %s THEN %s ELSE 0 END), 0)", answered, expr)
	}
	if err := db.Raw(fmt.Sprintf(`SELECT %[1]s AS bucket, l.provider AS provider, l.model AS model,
			%[13]s AS key_id, %[14]s AS service_tier,
			COUNT(*) AS attempts, %[3]s AS succeeded, %[4]s AS streamed,
			%[5]s AS prompt_tokens, %[6]s AS completion_tokens, %[7]s AS reasoning_tokens, %[8]s AS cached_read_tokens,
			%[9]s AS input_cost, %[10]s AS output_cost, %[11]s AS cost
		  FROM logs l WHERE %[2]s GROUP BY %[1]s, %[12]s`,
		hour, scope, sumAnswered("1"), sumAnswered("CASE WHEN l.stream THEN 1 ELSE 0 END"),
		sumAnswered("l.prompt_tokens"), sumAnswered("l.completion_tokens"), sumAnswered(reasoning), sumAnswered("l.cached_read_tokens"),
		sumAnswered("l.input_cost"), sumAnswered("l.output_cost"), sumAnswered("COALESCE(l.cost, 0)"), endpoint, keyID, endpointTierExpr),
		args...).Scan(&totals).Error; err != nil {
		return nil, fmt.Errorf("endpoint statistics totals: %w", err)
	}

	hours := make(map[endpointKey]*EndpointHour, len(totals))
	for _, row := range totals {
		hours[endpointKey{row.Bucket, row.Provider, row.Model, row.KeyID, row.ServiceTier}] = &EndpointHour{
			Hour:     time.Unix(row.Bucket, 0).UTC(),
			Provider: row.Provider, Model: row.Model, KeyID: row.KeyID, ServiceTier: row.ServiceTier,
			Attempts: row.Attempts, Succeeded: row.Succeeded, Streamed: row.Streamed,
			PromptTokens: row.PromptTokens, CompletionTokens: row.CompletionTokens,
			ReasoningTokens: row.ReasoningTokens, CachedReadTokens: row.CachedReadTokens,
			InputCostUSD: row.InputCost, OutputCostUSD: row.OutputCost, CostUSD: row.Cost,
			LatencyMs:          make([]int64, EndpointStatsBuckets),
			TimeToFirstTokenMs: make([]int64, EndpointStatsBuckets),
			Throughput:         make([]int64, EndpointStatsBuckets),
		}
	}

	histograms := []struct {
		name   string
		value  string
		where  string
		bounds []float64
		into   func(*EndpointHour) []int64
	}{
		{"latency", "l.latency", "l.latency IS NOT NULL", out.Bounds.LatencyMs, func(h *EndpointHour) []int64 { return h.LatencyMs }},
		{"time to first token", "l.time_to_first_token", "l.stream AND l.time_to_first_token IS NOT NULL", out.Bounds.LatencyMs,
			func(h *EndpointHour) []int64 { return h.TimeToFirstTokenMs }},
		{"throughput", endpointThroughputExpr, "l.upstream_latency > 0 AND l.completion_tokens > 0", out.Bounds.Throughput,
			func(h *EndpointHour) []int64 { return h.Throughput }},
	}
	type histogramRow struct {
		Bucket      int64
		Provider    string
		Model       string
		KeyID       string `gorm:"column:key_id"`
		ServiceTier string `gorm:"column:service_tier"`
		Slot        int    `gorm:"column:slot"`
		Calls       int64  `gorm:"column:calls"`
	}
	for _, histogram := range histograms {
		var rows []histogramRow
		slot := bucketExpr(histogram.value, histogram.bounds)
		if err := db.Raw(fmt.Sprintf(`SELECT %[1]s AS bucket, l.provider AS provider, l.model AS model,
				%[7]s AS key_id, %[8]s AS service_tier,
				%[2]s AS slot, COUNT(*) AS calls
			  FROM logs l WHERE %[3]s AND %[4]s AND %[5]s GROUP BY %[1]s, %[6]s, %[2]s`,
			hour, slot, scope, answered, histogram.where, endpoint, keyID, endpointTierExpr), args...).Scan(&rows).Error; err != nil {
			return nil, fmt.Errorf("endpoint statistics %s histogram: %w", histogram.name, err)
		}
		for _, row := range rows {
			h, ok := hours[endpointKey{row.Bucket, row.Provider, row.Model, row.KeyID, row.ServiceTier}]
			if !ok || row.Slot < 0 || row.Slot >= EndpointStatsBuckets {
				continue
			}
			histogram.into(h)[row.Slot] = row.Calls
		}
	}

	// a row that tried several keys: each earlier key that failed made one more failed attempt
	type trailRow struct {
		Bucket      int64
		Provider    string
		Model       string
		ServiceTier string `gorm:"column:service_tier"`
		Trail       string `gorm:"column:trail"`
	}
	var trails []trailRow
	if err := db.Raw(fmt.Sprintf(`SELECT %[1]s AS bucket, l.provider AS provider, l.model AS model,
			%[2]s AS service_tier, l.attempt_trail AS trail
		  FROM logs l WHERE %[3]s AND %[4]s > 1`,
		hour, endpointTierExpr, scope, trailLengthExpr(dialect)), args...).Scan(&trails).Error; err != nil {
		return nil, fmt.Errorf("endpoint statistics key attempts: %w", err)
	}
	for _, row := range trails {
		var records []struct {
			KeyID      string  `json:"key_id"`
			FailReason *string `json:"fail_reason"`
		}
		if err := sonic.UnmarshalString(row.Trail, &records); err != nil || len(records) < 2 {
			continue
		}
		// the last record is the row's own outcome, already counted on its key above
		for _, record := range records[:len(records)-1] {
			if record.FailReason == nil {
				continue
			}
			key := endpointKey{row.Bucket, row.Provider, row.Model, record.KeyID, row.ServiceTier}
			h, ok := hours[key]
			if !ok {
				h = &EndpointHour{
					Hour:     time.Unix(row.Bucket, 0).UTC(),
					Provider: row.Provider, Model: row.Model, KeyID: record.KeyID, ServiceTier: row.ServiceTier,
					LatencyMs:          make([]int64, EndpointStatsBuckets),
					TimeToFirstTokenMs: make([]int64, EndpointStatsBuckets),
					Throughput:         make([]int64, EndpointStatsBuckets),
				}
				hours[key] = h
			}
			h.Attempts++
		}
	}

	out.Hours = make([]EndpointHour, 0, len(hours))
	for _, h := range hours {
		out.Hours = append(out.Hours, *h)
	}
	sort.Slice(out.Hours, func(i, j int) bool {
		a, b := out.Hours[i], out.Hours[j]
		if !a.Hour.Equal(b.Hour) {
			return a.Hour.Before(b.Hour)
		}
		return a.Provider+"\x00"+a.Model+"\x00"+a.KeyID+"\x00"+a.ServiceTier < b.Provider+"\x00"+b.Model+"\x00"+b.KeyID+"\x00"+b.ServiceTier
	})
	return out, nil
}

// GetEndpointStats reads the database rows: the statistics need no payload.
func (h *HybridLogStore) GetEndpointStats(ctx context.Context, start, end time.Time) (*EndpointStats, error) {
	inner, ok := h.inner.(EndpointStatsReader)
	if !ok {
		return nil, ErrEndpointStatsUnsupported
	}
	return inner.GetEndpointStats(ctx, start, end)
}

// EndpointStatsReader is the store that can count endpoints' attempts by hour; the logging plugin
// asks its store for it rather than widening LogStore.
type EndpointStatsReader interface {
	GetEndpointStats(ctx context.Context, start, end time.Time) (*EndpointStats, error)
}

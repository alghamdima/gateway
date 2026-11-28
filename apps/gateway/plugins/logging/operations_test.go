package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	cstables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/jobaccounting"
	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/maximhq/bifrost/framework/modelcatalog"
	"github.com/maximhq/bifrost/framework/modelcatalog/datasheet"
	"github.com/maximhq/bifrost/framework/streaming"
	"github.com/maximhq/bifrost/framework/tracing"
	"github.com/stretchr/testify/require"
)

type testLogger struct{}

func (testLogger) Debug(string, ...any)                   {}
func (testLogger) Info(string, ...any)                    {}
func (testLogger) Warn(string, ...any)                    {}
func (testLogger) Error(string, ...any)                   {}
func (testLogger) Fatal(string, ...any)                   {}
func (testLogger) SetLevel(schemas.LogLevel)              {}
func (testLogger) SetOutputType(schemas.LoggerOutputType) {}
func (testLogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

// fakeBatchStore is an in-memory jobaccounting.SweepStore for logging tests.
type fakeBatchStore struct {
	jobs map[string]*cstables.TableProviderJob
}

func newFakeBatchStore() *fakeBatchStore {
	return &fakeBatchStore{jobs: make(map[string]*cstables.TableProviderJob)}
}

func (s *fakeBatchStore) UpsertProviderJob(ctx context.Context, job *cstables.TableProviderJob) error {
	if job.ID == "" {
		job.ID = cstables.ProviderJobID(cstables.ProviderJobKindBatch, job.Provider, job.JobID)
	}
	existing, ok := s.jobs[job.ID]
	if !ok {
		copied := *job
		if copied.AccountingStatus == "" {
			copied.AccountingStatus = cstables.ProviderJobAccountingStatusPending
		}
		s.jobs[job.ID] = &copied
		return nil
	}
	if job.Model != "" {
		existing.Model = job.Model
	}
	if job.ProviderStatus != "" {
		existing.ProviderStatus = job.ProviderStatus
	}
	if job.InputFileID != "" {
		existing.InputFileID = job.InputFileID
	}
	if job.OutputFileID != nil {
		existing.OutputFileID = job.OutputFileID
	}
	if job.NextCheckAt != nil {
		existing.NextCheckAt = job.NextCheckAt
	}
	if cstables.IsTerminalBatchProviderStatus(job.ProviderStatus) &&
		job.ProviderStatus != string(schemas.BatchStatusCompleted) &&
		job.ProviderStatus != string(schemas.BatchStatusEnded) {
		existing.NextCheckAt = nil
	}
	return nil
}

func (s *fakeBatchStore) GetProviderJob(ctx context.Context, jobID string) (*cstables.TableProviderJob, error) {
	job, ok := s.jobs[jobID]
	if !ok {
		return nil, errors.New("missing batch job")
	}
	copied := *job
	return &copied, nil
}

func (s *fakeBatchStore) ListDueProviderJobs(ctx context.Context, kind, provider string, now time.Time, limit int) ([]*cstables.TableProviderJob, error) {
	var jobs []*cstables.TableProviderJob
	if kind == "" {
		kind = cstables.ProviderJobKindBatch
	}
	for _, job := range s.jobs {
		// Mirror the real store: a sweeper only ever sees its own kind's rows.
		jobKind := job.Kind
		if jobKind == "" {
			jobKind = cstables.ProviderJobKindBatch
		}
		if jobKind != kind {
			continue
		}
		if provider != "" && job.Provider != provider {
			continue
		}
		if job.NextCheckAt == nil || job.NextCheckAt.After(now) {
			continue
		}
		if job.AccountingStatus == cstables.ProviderJobAccountingStatusAccounted || job.AccountingStatus == cstables.ProviderJobAccountingStatusUnpriceable {
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (s *fakeBatchStore) ClaimProviderJob(ctx context.Context, jobID, runnerID string, staleBefore time.Time, allowUnpriceable bool) (bool, error) {
	entry, ok := s.jobs[jobID]
	if !ok {
		return false, errors.New("missing batch job")
	}
	if entry.AccountingStatus == cstables.ProviderJobAccountingStatusAccounted {
		return false, nil
	}
	// "unpriceable" is a stop-polling marker, not a refusal of money: a caller
	// holding real results may re-drive it. Mirrors the real store's allowUnpriceable,
	// which this fake previously ignored — hiding the result-driven re-accounting path
	// from every test using it.
	if entry.AccountingStatus == cstables.ProviderJobAccountingStatusUnpriceable && !allowUnpriceable {
		return false, nil
	}
	// Claim staleness reads claimed_at, not updated_at — updated_at is refreshed by
	// the unfenced UpsertProviderJob, so it cannot represent claim age.
	if entry.AccountingStatus == cstables.ProviderJobAccountingStatusProcessing &&
		entry.ClaimedAt != nil && entry.ClaimedAt.After(staleBefore) {
		return false, nil
	}
	now := time.Now().UTC()
	rid := runnerID
	entry.AccountingStatus = cstables.ProviderJobAccountingStatusProcessing
	entry.RunnerID = &rid
	entry.ClaimedAt = &now
	entry.UpdatedAt = now
	return true, nil
}

func (s *fakeBatchStore) markTerminal(id, status, reason string) error {
	entry, ok := s.jobs[id]
	if !ok {
		return errors.New("missing batch job")
	}
	entry.AccountingStatus = status
	entry.RunnerID = nil
	entry.ClaimedAt = nil
	if reason != "" {
		entry.UnpriceableReason = &reason
	}
	return nil
}

func (s *fakeBatchStore) MarkProviderJobAggregateLogWritten(ctx context.Context, id, runnerID string) error {
	entry, ok := s.jobs[id]
	if !ok {
		return errors.New("missing batch job")
	}
	now := time.Now().UTC()
	entry.AggregateLogWrittenAt = &now
	return nil
}

func (s *fakeBatchStore) MarkProviderJobGovernanceReported(ctx context.Context, id, runnerID string) error {
	entry, ok := s.jobs[id]
	if !ok {
		return errors.New("missing batch job")
	}
	now := time.Now().UTC()
	entry.GovernanceReportedAt = &now
	return nil
}

func (s *fakeBatchStore) CompleteProviderJob(ctx context.Context, id, runnerID string) error {
	return s.markTerminal(id, cstables.ProviderJobAccountingStatusAccounted, "")
}

func (s *fakeBatchStore) MarkProviderJobUnpriceable(ctx context.Context, id, runnerID, reason string, err error) error {
	return s.markTerminal(id, cstables.ProviderJobAccountingStatusUnpriceable, reason)
}

func (s *fakeBatchStore) FailProviderJob(ctx context.Context, id, runnerID string, err error) error {
	return s.markTerminal(id, cstables.ProviderJobAccountingStatusError, "")
}

func newTestStore(t testing.TB) logstore.LogStore {
	t.Helper()

	store, err := logstore.NewLogStore(context.Background(), &logstore.Config{
		Enabled: true,
		Type:    logstore.LogStoreTypeSQLite,
		Config: &logstore.SQLiteConfig{
			Path: filepath.Join(t.TempDir(), "logging.db"),
		},
	}, testLogger{})
	if err != nil {
		t.Fatalf("NewLogStore() error = %v", err)
	}
	return store
}

func TestUserAgentFromContextUsesRequestHeaders(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{
		"user-agent": "codex-tui/1.0",
	})
	ctx.SetValue(schemas.BifrostContextKeyUserAgent, "fallback/1.0")

	if got := userAgentFromContext(ctx); got != "codex-tui/1.0" {
		t.Fatalf("expected request-header user agent, got %q", got)
	}
}

func TestUserAgentFromContextUsesCanonicalRequestHeader(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{
		"User-Agent": "Claude-Code/1.0",
	})

	if got := userAgentFromContext(ctx); got != "Claude-Code/1.0" {
		t.Fatalf("expected canonical request-header user agent, got %q", got)
	}
}

func TestUserAgentFromContextUsesUppercaseRequestHeader(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{
		"USER-AGENT": "Cursor/0.47",
	})

	if got := userAgentFromContext(ctx); got != "Cursor/0.47" {
		t.Fatalf("expected uppercase request-header user agent, got %q", got)
	}
}

func TestUserAgentFromContextFallsBackWhenHeaderValueIsEmpty(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{
		"user-agent": "",
	})
	ctx.SetValue(schemas.BifrostContextKeyUserAgent, "fallback/1.0")

	if got := userAgentFromContext(ctx); got != "fallback/1.0" {
		t.Fatalf("expected fallback user agent for empty header, got %q", got)
	}
}

func TestUserAgentFromContextFallsBackToUserAgentKey(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyUserAgent, "cursor/0.47")

	if got := userAgentFromContext(ctx); got != "cursor/0.47" {
		t.Fatalf("expected fallback user agent, got %q", got)
	}
}

// TestAntigravityLogClassification preserves synthetic raw agents while giving logs distinct built-in labels and honoring custom mappings.
func TestAntigravityLogClassification(t *testing.T) {
	plugin := &LoggerPlugin{}
	for userAgent, expected := range map[string]string{"antigravity/2.23.0": "Antigravity Desktop", "antigravity-cli/1.0": "Antigravity CLI"} {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{"user-agent": userAgent})
		ctx.SetValue(schemas.BifrostContextKeyUserAgent, "bifrost-edge/0.1.0")
		actual := userAgentFromContext(ctx)
		if actual != userAgent || plugin.detectAppFromUserAgent(actual) != expected {
			t.Fatalf("user agent %q was not classified as %s", actual, expected)
		}
		entry := &logstore.Log{}
		applyUserAgent(entry, actual)
		if entry.App == nil || *entry.App != expected || entry.UserAgent == nil || *entry.UserAgent != userAgent {
			t.Fatalf("log lost raw agent or classification: %+v", entry)
		}
	}
	plugin.userAgentMappings.Store([]compiledUserAgentMapping{{Pattern: "antigravity/", MatchType: schemas.UserAgentMappingMatchTypeContains, App: "Custom Antigravity"}})
	if actual := plugin.detectAppFromUserAgent("antigravity/2.23.0"); actual != "Custom Antigravity" {
		t.Fatalf("custom mapping overridden: %s", actual)
	}
}

func TestPreLLMHookSetsAppContextFromDetectedApp(t *testing.T) {
	store := newTestStore(t)
	defer store.Close(context.Background())
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := plugin.Cleanup(); cleanupErr != nil {
			t.Errorf("Cleanup() error = %v", cleanupErr)
		}
	})
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-app-context")
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{
		"user-agent":   "claude-cli/2.1.168 (external, cli)",
		"x-bf-vk":      "vk-test",
		"x-bf-user-id": "user-test",
	})

	_, _, err = plugin.PreLLMHook(ctx, &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-4o-mini",
			Params:   &schemas.ChatParameters{},
		},
	})
	if err != nil {
		t.Fatalf("PreLLMHook() error = %v", err)
	}
	if got, _ := ctx.Value(schemas.BifrostContextKeyApp).(string); got != "claude-code" {
		t.Fatalf("app context = %q, want claude-code", got)
	}
}

// TestPreLLMHookContextKeyComesFromUserAgentNotAgentHeader replays the header
// shape the desktop agent stamps for a Claude Cowork request: Cowork's runtime
// shares Claude Code's CLI User-Agent, so the logging plugin derives
// claude-code for the context key. Header-based app enforcement (X-Bf-App)
// lives in the enterprise agent policy plugin, which prefers the header over
// this context key.
func TestPreLLMHookContextKeyComesFromUserAgentNotAgentHeader(t *testing.T) {
	store := newTestStore(t)
	defer store.Close(context.Background())
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := plugin.Cleanup(); cleanupErr != nil {
			t.Errorf("Cleanup() error = %v", cleanupErr)
		}
	})
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-cowork-context")
	// Captured from the agent's MITM → GATEWAY forwarding of a Cowork request.
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{
		"user-agent":                "claude-cli/2.1.170 (external, local-agent, agent-sdk/0.3.170)",
		"anthropic-client-platform": "desktop_app",
		"x-app":                     "cli",
		"x-bf-app":                  "claude-cowork",
		"x-bifrost-agent":           "bifrost-agent/0.1.0",
	})

	_, _, err = plugin.PreLLMHook(ctx, &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.Anthropic,
			Model:    "claude-opus-4-8",
			Params:   &schemas.ChatParameters{},
		},
	})
	if err != nil {
		t.Fatalf("PreLLMHook() error = %v", err)
	}
	if got, _ := ctx.Value(schemas.BifrostContextKeyApp).(string); got != "claude-code" {
		t.Fatalf("app context = %q, want claude-code (derived from User-Agent)", got)
	}
}

func TestCustomUserAgentMappingOverridesBuiltInDetection(t *testing.T) {
	store := newTestStore(t)
	defer store.Close(context.Background())
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := plugin.Cleanup(); cleanupErr != nil {
			t.Errorf("Cleanup() error = %v", cleanupErr)
		}
	})

	_, err = plugin.CreateUserAgentMapping(context.Background(), &logstore.UserAgentMapping{
		Pattern:   `claude-cli/\d+\.\d+`,
		MatchType: string(schemas.UserAgentMappingMatchTypeRegex),
		App:       "Internal Claude Wrapper",
		IsActive:  true,
	})
	if err != nil {
		t.Fatalf("CreateUserAgentMapping() error = %v", err)
	}

	if got := plugin.detectAppFromUserAgent("claude-cli/2.1.168 (external, cli)"); got != "Internal Claude Wrapper" {
		t.Fatalf("expected custom app mapping, got %q", got)
	}
}

// Values longer than their columns (and the schema's maxLength) are a client
// fault, refused before they reach the store; limits count characters, as
// varchar does.
func TestValidateUserAgentMappingRejectsOversizedValues(t *testing.T) {
	mime := func(s string) *string { return &s }
	cases := []struct {
		name    string
		mapping logstore.UserAgentMapping
		wantErr bool
	}{
		{"pattern at limit", logstore.UserAgentMapping{Pattern: strings.Repeat("p", 512), MatchType: "contains", App: "a"}, false},
		{"pattern over limit", logstore.UserAgentMapping{Pattern: strings.Repeat("p", 513), MatchType: "contains", App: "a"}, true},
		{"app at limit in characters", logstore.UserAgentMapping{Pattern: "p", MatchType: "contains", App: strings.Repeat("é", 128)}, false},
		{"app over limit", logstore.UserAgentMapping{Pattern: "p", MatchType: "contains", App: strings.Repeat("a", 129)}, true},
		{"logo_mime at limit", logstore.UserAgentMapping{Pattern: "p", MatchType: "contains", App: "a", LogoMime: mime(strings.Repeat("m", 128))}, false},
		{"logo_mime over limit", logstore.UserAgentMapping{Pattern: "p", MatchType: "contains", App: "a", LogoMime: mime(strings.Repeat("m", 129))}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateUserAgentMapping(&tc.mapping)
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrInvalidUserAgentMapping)
		})
	}
}

func TestPreLLMHookSetsAppContextFromCustomMapping(t *testing.T) {
	store := newTestStore(t)
	defer store.Close(context.Background())
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := plugin.Cleanup(); cleanupErr != nil {
			t.Errorf("Cleanup() error = %v", cleanupErr)
		}
	})
	_, err = plugin.CreateUserAgentMapping(context.Background(), &logstore.UserAgentMapping{
		Pattern:   `custom-wrapper/\d+`,
		MatchType: string(schemas.UserAgentMappingMatchTypeRegex),
		App:       "Internal Claude Wrapper",
		IsActive:  true,
	})
	if err != nil {
		t.Fatalf("CreateUserAgentMapping() error = %v", err)
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-custom-app-context")
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{
		"user-agent": "custom-wrapper/42",
	})

	_, _, err = plugin.PreLLMHook(ctx, &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-4o-mini",
			Params:   &schemas.ChatParameters{},
		},
	})
	if err != nil {
		t.Fatalf("PreLLMHook() error = %v", err)
	}
	if got, _ := ctx.Value(schemas.BifrostContextKeyApp).(string); got != "internal-claude-wrapper" {
		t.Fatalf("app context = %q, want internal-claude-wrapper", got)
	}
}

func TestPostLLMHookNoPendingErrorPreservesMetadata(t *testing.T) {
	store := newTestStore(t)
	loggingHeaders := []string{"x-custom-log"}
	plugin, err := Init(context.Background(), &Config{LoggingHeaders: &loggingHeaders}, testLogger{}, store, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-error-no-pending")
	agentCorrelationID := strings.Repeat("a", maxPersistedAgentCorrelationIDLen+1)
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{
		"x-bf-lh-tenant":            "acme",
		"x-custom-log":              "custom-value",
		"X-Bf-Agent-Correlation-Id": agentCorrelationID,
	})
	ctx.SetValue(schemas.BifrostContextKeyDimensions, map[string]string{
		"region": "us-east",
	})
	ctx.SetValue(schemas.BifrostIsAsyncRequest, true)
	ctx.SetValue(schemas.BifrostContextKeySessionID, "session-no-pending-error")

	statusCode := 500
	bifrostErr := &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     &statusCode,
		Error:          &schemas.ErrorField{Message: "provider failed"},
		ExtraFields: schemas.BifrostErrorExtraFields{
			RequestType:            schemas.ChatCompletionRequest,
			Provider:               schemas.OpenAI,
			OriginalModelRequested: "gpt-4o",
			ResolvedModelUsed:      "gpt-4o",
		},
	}

	_, _, err = plugin.PostLLMHook(ctx, nil, bifrostErr)
	if err != nil {
		t.Fatalf("PostLLMHook() error = %v", err)
	}
	if err := plugin.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	logEntry, err := store.FindByID(context.Background(), "req-error-no-pending")
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if logEntry.Status != "error" {
		t.Fatalf("expected error status, got %q", logEntry.Status)
	}
	if logEntry.MetadataParsed == nil {
		t.Fatalf("expected metadata to be persisted")
	}
	if got := logEntry.MetadataParsed["tenant"]; got != "acme" {
		t.Fatalf("expected tenant metadata acme, got %#v", got)
	}
	if got := logEntry.MetadataParsed["x-custom-log"]; got != "custom-value" {
		t.Fatalf("expected configured header metadata custom-value, got %#v", got)
	}
	if got := logEntry.MetadataParsed["region"]; got != "us-east" {
		t.Fatalf("expected dimension metadata us-east, got %#v", got)
	}
	if got := logEntry.MetadataParsed["isAsyncRequest"]; got != true {
		t.Fatalf("expected async metadata true, got %#v", got)
	}
	if logEntry.SessionID == nil || *logEntry.SessionID != "session-no-pending-error" {
		t.Fatalf("expected session ID on the minimal-error entry, got %v", logEntry.SessionID)
	}
	if logEntry.AgentCorrelationID == nil || *logEntry.AgentCorrelationID != agentCorrelationID[:maxPersistedAgentCorrelationIDLen] {
		t.Fatalf("minimal-error agent correlation ID = %v, want %q", logEntry.AgentCorrelationID, agentCorrelationID[:maxPersistedAgentCorrelationIDLen])
	}
}

func TestEmitAggregateLogRunsCallback(t *testing.T) {
	store := newTestStore(t)
	plugin := &LoggerPlugin{
		ctx:   context.Background(),
		store: store,
	}
	callbacks := 0
	plugin.SetLogCallback(func(ctx context.Context, logEntry *logstore.Log) {
		callbacks++
	})

	entry := &logstore.Log{
		ID:        "batch-cost:openai:callback",
		Timestamp: time.Now().UTC(),
		Object:    string(schemas.BatchResultsRequest),
		Provider:  string(schemas.OpenAI),
		Model:     "gpt-4o-mini",
		Status:    "success",
	}
	plugin.EmitAggregateLog(context.Background(), entry)
	if callbacks != 1 {
		t.Fatalf("callback count after emit = %d, want 1", callbacks)
	}
}

// captureObsPlugin is a minimal ObservabilityPlugin that extracts the attributes of
// the first cost-bearing span in each injected trace. It copies the values out
// synchronously (the tracer recycles the trace after Inject returns) and hands them
// to the test over a channel.
type captureObsPlugin struct {
	spans chan map[string]any
}

func (c *captureObsPlugin) GetName() string { return "capture-obs" }
func (c *captureObsPlugin) Cleanup() error  { return nil }
func (c *captureObsPlugin) Inject(_ context.Context, trace *schemas.Trace) error {
	if trace == nil {
		return nil
	}
	spans := trace.Spans
	if trace.RootSpan != nil {
		spans = append(spans, trace.RootSpan)
	}
	for _, span := range spans {
		if span == nil {
			continue
		}
		if _, ok := span.Attributes[schemas.AttrUsageCost]; !ok {
			continue
		}
		attrs := make(map[string]any, len(span.Attributes))
		for k, v := range span.Attributes {
			attrs[k] = v
		}
		select {
		case c.spans <- attrs:
		default:
		}
		return nil
	}
	return nil
}

// TestEmitAggregateLogEmitsSettlementSpan verifies the settlement→observability
// bridge: a settled aggregate cost row is mirrored onto a span carrying the cost,
// tokens, request type, and enrichment dimensions, so the trace-fed connectors
// (OTEL) can record batch/video cost that the
// async settlement path would otherwise keep out of the tracing pipeline entirely.
func TestEmitAggregateLogEmitsSettlementSpan(t *testing.T) {
	logger := testLogger{}
	tracer := tracing.NewTracer(tracing.NewTraceStore(time.Minute, logger), nil, logger)
	capture := &captureObsPlugin{spans: make(chan map[string]any, 1)}
	tracer.SetObservabilityPlugins([]schemas.ObservabilityPlugin{capture}, nil)

	plugin := &LoggerPlugin{ctx: context.Background()}
	plugin.SetSettlementTracer(tracer)

	cost := 0.0123
	teamID := "team-42"
	vkID := "vk-7"
	entry := &logstore.Log{
		ID:               "batch-cost:openai:settlement-span",
		Timestamp:        time.Now().UTC(),
		Object:           string(schemas.BatchResultsRequest),
		Provider:         string(schemas.OpenAI),
		Model:            "gpt-4o-mini",
		Status:           "success",
		Cost:             &cost,
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
		TeamID:           &teamID,
		VirtualKeyID:     &vkID,
		BatchDebugParsed: &schemas.BifrostBatchDebug{BatchID: "batch_abc123"},
	}
	plugin.EmitAggregateLog(context.Background(), entry)

	select {
	case attrs := <-capture.spans:
		if got, _ := attrs[schemas.AttrBatchID].(string); got != "batch_abc123" {
			t.Fatalf("span batch id = %v, want batch_abc123", attrs[schemas.AttrBatchID])
		}
		if got, _ := attrs[schemas.AttrUsageCost].(float64); got != cost {
			t.Fatalf("span cost = %v, want %v", attrs[schemas.AttrUsageCost], cost)
		}
		if got, _ := attrs[schemas.AttrLegacyRequestType].(string); got != string(schemas.BatchResultsRequest) {
			t.Fatalf("span request.type = %q, want %q", got, schemas.BatchResultsRequest)
		}
		if got, _ := attrs[schemas.AttrProviderName].(string); got != string(schemas.OpenAI) {
			t.Fatalf("span provider = %q, want %q", got, schemas.OpenAI)
		}
		if got, _ := attrs[schemas.AttrRequestModel].(string); got != "gpt-4o-mini" {
			t.Fatalf("span model = %q, want gpt-4o-mini", got)
		}
		if got, _ := attrs[schemas.AttrTotalTokens].(int); got != 150 {
			t.Fatalf("span total tokens = %v, want 150", attrs[schemas.AttrTotalTokens])
		}
		if got, _ := attrs[schemas.AttrBifrostTeamID].(string); got != teamID {
			t.Fatalf("span team_id = %q, want %q", got, teamID)
		}
		if got, _ := attrs[schemas.AttrBifrostVirtualKeyID].(string); got != vkID {
			t.Fatalf("span virtual_key_id = %q, want %q", got, vkID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no settlement span injected to the observability connector")
	}
}

// TestEmitAggregateLogSkipsSettlementSpanWithoutCost verifies an unpriced aggregate
// row emits no span (nothing for a connector to record), and that a nil tracer is a
// safe no-op rather than a panic.
func TestEmitAggregateLogSkipsSettlementSpanWithoutCost(t *testing.T) {
	logger := testLogger{}
	tracer := tracing.NewTracer(tracing.NewTraceStore(time.Minute, logger), nil, logger)
	capture := &captureObsPlugin{spans: make(chan map[string]any, 1)}
	tracer.SetObservabilityPlugins([]schemas.ObservabilityPlugin{capture}, nil)

	plugin := &LoggerPlugin{ctx: context.Background()}
	plugin.SetSettlementTracer(tracer)

	entry := &logstore.Log{
		ID:        "batch-cost:openai:no-cost",
		Timestamp: time.Now().UTC(),
		Object:    string(schemas.BatchResultsRequest),
		Provider:  string(schemas.OpenAI),
		Model:     "gpt-4o-mini",
		Status:    "success",
		// Cost intentionally nil.
	}
	plugin.EmitAggregateLog(context.Background(), entry)

	select {
	case <-capture.spans:
		t.Fatal("unpriced aggregate row must not emit a settlement span")
	case <-time.After(200 * time.Millisecond):
	}

	// Nil tracer must not panic.
	plugin.SetSettlementTracer(nil)
	cost := 1.0
	entry.Cost = &cost
	plugin.EmitAggregateLog(context.Background(), entry)
}

func TestLogSubscribersComposeAndUnsubscribe(t *testing.T) {
	plugin := &LoggerPlugin{ctx: context.Background(), logger: testLogger{}}
	primary, first, second := 0, 0, 0
	plugin.SetLogCallback(func(context.Context, *logstore.Log) { primary++ })
	unsubscribeFirst := plugin.SubscribeLogCallback(func(context.Context, *logstore.Log) { first++ })
	plugin.SubscribeLogCallback(func(context.Context, *logstore.Log) { second++ })

	plugin.notifyLogCallbacks(context.Background(), &logstore.Log{ID: "one"})
	unsubscribeFirst()
	unsubscribeFirst()
	plugin.notifyLogCallbacks(context.Background(), &logstore.Log{ID: "two"})

	if primary != 2 || first != 1 || second != 2 {
		t.Fatalf("callback counts = primary:%d first:%d second:%d", primary, first, second)
	}
}

// A delete a caller makes through the manager is announced to every
// subscriber once the store has done it, with the ids; a delete the store
// refused announces nothing, since nothing was deleted.
func TestLogDeleteSubscribersAreToldWhatWasDeleted(t *testing.T) {
	store := newTestStore(t)
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })
	plugin := &LoggerPlugin{ctx: context.Background(), store: store, logger: testLogger{}}
	for _, id := range []string{"one", "two", "three"} {
		require.NoError(t, store.Create(context.Background(), &logstore.Log{ID: id, Timestamp: time.Now(), Object: "chat.completion", Status: "success"}))
	}
	var first, second [][]string
	unsubscribeFirst := plugin.SubscribeLogDeleteCallback(func(_ context.Context, ids []string) { first = append(first, ids) })
	plugin.SubscribeLogDeleteCallback(func(_ context.Context, ids []string) { second = append(second, ids) })
	manager := plugin.GetPluginLogManager()

	require.NoError(t, manager.DeleteLogs(context.Background(), []string{"one", "two"}))
	unsubscribeFirst()
	unsubscribeFirst()
	require.NoError(t, manager.DeleteLog(context.Background(), "three"))

	require.Equal(t, [][]string{{"one", "two"}}, first)
	require.Equal(t, [][]string{{"one", "two"}, {"three"}}, second)
	_, err := store.FindByID(context.Background(), "three")
	require.ErrorIs(t, err, logstore.ErrNotFound, "the store deleted it before anyone was told")

	// Nothing to delete, nothing to announce.
	require.NoError(t, manager.DeleteLogs(context.Background(), nil))
	require.Len(t, second, 2)
}

// failingDeleteStore fails every log delete with err.
type failingDeleteStore struct {
	logstore.LogStore
	err error
}

func (s failingDeleteStore) DeleteLog(context.Context, string) error    { return s.err }
func (s failingDeleteStore) DeleteLogs(context.Context, []string) error { return s.err }

// A delete that failed before the logs went announces nothing.
func TestLogDeleteSubscribersAreNotToldOfFailedDeletes(t *testing.T) {
	plugin := &LoggerPlugin{ctx: context.Background(), store: failingDeleteStore{err: errors.New("log delete failed")}, logger: testLogger{}}
	var told [][]string
	plugin.SubscribeLogDeleteCallback(func(_ context.Context, ids []string) { told = append(told, ids) })
	manager := plugin.GetPluginLogManager()

	require.Error(t, manager.DeleteLogs(context.Background(), []string{"four"}))
	require.Error(t, manager.DeleteLog(context.Background(), "five"))
	require.Empty(t, told)
}

func TestPostLLMHookStreamingErrorPreservesHeaderMetadata(t *testing.T) {
	store := newTestStore(t)
	loggingHeaders := []string{"x-custom-log"}
	plugin, err := Init(context.Background(), &Config{LoggingHeaders: &loggingHeaders}, testLogger{}, store, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-stream-error-metadata")
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{
		"x-custom-log":   "custom-value",
		"x-bf-lh-user":   `{"device_id":"device-1","session_id":"session-1"}`,
		"x-bf-lh-tag":    "from-header",
		"x-bf-lh-shared": "from-header",
	})
	ctx.SetValue(schemas.BifrostContextKeyDimensions, map[string]string{
		"environment": "staging",
	})

	req := &schemas.BifrostRequest{
		RequestType: schemas.ResponsesStreamRequest,
		ResponsesRequest: &schemas.BifrostResponsesRequest{
			Provider: schemas.Bedrock,
			Model:    "us.anthropic.claude-opus-4-7",
			Params:   &schemas.ResponsesParameters{},
		},
	}
	if _, _, err = plugin.PreLLMHook(ctx, req); err != nil {
		t.Fatalf("PreLLMHook() error = %v", err)
	}
	statusCode := 500
	bifrostErr := &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     &statusCode,
		Error:          &schemas.ErrorField{Message: "stream failed"},
		ExtraFields: schemas.BifrostErrorExtraFields{
			RequestType:            schemas.ResponsesStreamRequest,
			Provider:               schemas.Bedrock,
			OriginalModelRequested: "us.anthropic.claude-opus-4-7",
			ResolvedModelUsed:      "us.anthropic.claude-opus-4-7",
		},
	}
	if _, _, err = plugin.PostLLMHook(ctx, nil, bifrostErr); err != nil {
		t.Fatalf("PostLLMHook() error = %v", err)
	}
	if err := plugin.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	logEntry, err := store.FindByID(context.Background(), "req-stream-error-metadata")
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if logEntry.Status != "error" {
		t.Fatalf("expected error status, got %q", logEntry.Status)
	}
	if logEntry.MetadataParsed == nil {
		t.Fatalf("expected metadata to be persisted")
	}
	if got := logEntry.MetadataParsed["user"]; got != `{"device_id":"device-1","session_id":"session-1"}` {
		t.Fatalf("expected user metadata from header, got %#v", got)
	}
	if got := logEntry.MetadataParsed["tag"]; got != "from-header" {
		t.Fatalf("expected tag metadata from header, got %#v", got)
	}
	if got := logEntry.MetadataParsed["x-custom-log"]; got != "custom-value" {
		t.Fatalf("expected configured header metadata custom-value, got %#v", got)
	}
	if got := logEntry.MetadataParsed["shared"]; got != "from-header" {
		t.Fatalf("expected shared metadata from header, got %#v", got)
	}
	if got := logEntry.MetadataParsed["environment"]; got != "staging" {
		t.Fatalf("expected dimension metadata staging, got %#v", got)
	}
}

// TestPostLLMHookCancelledStreamLogsCost verifies #3357 at the logging layer: a
// streaming request cancelled mid-flight (including when a chunk is returned)
// whose error carries the
// partial usage the provider already processed (BifrostError.ExtraFields.BilledUsage)
// must produce a log row with status="cancelled", the consumed tokens, AND an
// accurate cost computed from the datasheet rates.
func TestPostLLMHookCancelledStreamLogsCost(t *testing.T) {
	store := newTestStore(t)

	// Pricing manager loaded from the committed datasheet testdata via an
	// offline file:// URL (no network).
	abs, err := filepath.Abs("../../framework/modelcatalog/datasheet/testdata/pricing.json")
	if err != nil {
		t.Fatalf("resolve testdata path: %v", err)
	}
	ds := datasheet.New(nil, testLogger{}, datasheet.Config{URL: "file://" + abs})
	if err := ds.LoadFromURLIntoMemory(context.Background()); err != nil {
		t.Fatalf("load pricing datasheet: %v", err)
	}
	pricingManager := modelcatalog.NewTestCatalogWithDatasheet(ds)

	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, pricingManager)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-cancel-cost")

	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionStreamRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-4o",
			Params:   &schemas.ChatParameters{},
		},
	}
	if _, _, err = plugin.PreLLMHook(ctx, req); err != nil {
		t.Fatalf("PreLLMHook() error = %v", err)
	}
	embeddingProvider := "openai"
	embeddingModel := "text-embedding-3-small"
	embeddingTokens := 12
	if !schemas.SetCacheMetadataOnContext(ctx, &schemas.BifrostCacheMetadata{
		ProviderUsed: &embeddingProvider,
		ModelUsed:    &embeddingModel,
		InputTokens:  &embeddingTokens,
	}) {
		t.Fatal("expected semantic cache metadata to be stored on context")
	}
	routingEmbeddingTokens := 13
	if !schemas.AppendRoutingCallOnContext(ctx, schemas.BifrostRoutingCall{
		ProviderUsed:       &embeddingProvider,
		ModelUsed:          &embeddingModel,
		InputTokens:        &routingEmbeddingTokens,
		CountTowardBudgets: true,
	}) {
		t.Fatal("expected routing metadata to be stored on context")
	}
	if !schemas.AppendGuardrailJudgeCallOnContext(ctx, schemas.BifrostGuardrailJudgeCall{
		JudgeProvider:    schemas.OpenAI,
		JudgeModel:       "gpt-4o",
		JudgeRequestType: schemas.ChatCompletionRequest,
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      15,
	}) {
		t.Fatal("expected guardrail judge call to be stored on context")
	}

	const promptTokens, completionTokens = 100, 50
	statusCode := 499 // client closed request (mid-stream cancel)
	bifrostErr := &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     &statusCode,
		Error:          &schemas.ErrorField{Message: "client disconnected"},
		ExtraFields: schemas.BifrostErrorExtraFields{
			RequestType:            schemas.ChatCompletionStreamRequest,
			Provider:               schemas.OpenAI,
			OriginalModelRequested: "gpt-4o",
			ResolvedModelUsed:      "gpt-4o",
			// Provider processed these tokens before the client disconnected.
			BilledUsage: &schemas.BifrostLLMUsage{
				PromptTokens:     promptTokens,
				CompletionTokens: completionTokens,
				TotalTokens:      promptTokens + completionTokens,
			},
		},
	}
	streamChunk := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType:            schemas.ChatCompletionStreamRequest,
				Provider:               schemas.OpenAI,
				OriginalModelRequested: "gpt-4o",
				ResolvedModelUsed:      "gpt-4o",
			},
		},
	}
	if _, _, err = plugin.PostLLMHook(ctx, streamChunk, bifrostErr); err != nil {
		t.Fatalf("PostLLMHook() error = %v", err)
	}
	if err := plugin.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	entry, err := store.FindByID(context.Background(), "req-cancel-cost")
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if entry.Status != "cancelled" {
		t.Fatalf("expected cancelled status, got %q", entry.Status)
	}
	if entry.TokenUsageParsed == nil {
		t.Fatalf("expected token usage recorded from BilledUsage on the cancel path")
	}
	if entry.TotalTokens != promptTokens+completionTokens {
		t.Fatalf("expected total_tokens %d, got %d", promptTokens+completionTokens, entry.TotalTokens)
	}
	if entry.Cost == nil {
		t.Fatalf("expected a cost to be logged for a cancelled request that consumed tokens (#3357)")
	}
	// gpt-4o testdata rates: input 2.5e-6/token, output 1e-5/token.
	// text-embedding-3-small is 2e-8/token. The cache lookup, routing
	// classification, and judge call must all be added even though the stream
	// ended with an error chunk.
	want := float64(promptTokens)*2.5e-6 + float64(completionTokens)*1e-5 +
		float64(embeddingTokens+routingEmbeddingTokens)*2e-8 + float64(10)*2.5e-6 + float64(5)*1e-5
	if diff := *entry.Cost - want; diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("logged cost %v does not match datasheet-computed cost %v", *entry.Cost, want)
	}
	// The routing classification call must survive the full write/read round
	// trip through the DB — this is what makes it visible in the log detail
	// view, not just correctly billed.
	if entry.RoutingMetadataParsed == nil || len(entry.RoutingMetadataParsed.Calls) != 1 {
		t.Fatalf("expected one routing call to round-trip through the DB, got %+v", entry.RoutingMetadataParsed)
	}
	if call := entry.RoutingMetadataParsed.Calls[0]; call.ProviderUsed == nil || *call.ProviderUsed != embeddingProvider ||
		call.InputTokens == nil || *call.InputTokens != routingEmbeddingTokens {
		t.Fatalf("routing call round-tripped incorrectly: %+v", call)
	}
}

func TestPostLLMHookContextTimeoutLogsCancelledStatus(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-context-timeout")

	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-4o",
			Params:   &schemas.ChatParameters{},
		},
	}
	if _, _, err = plugin.PreLLMHook(ctx, req); err != nil {
		t.Fatalf("PreLLMHook() error = %v", err)
	}

	statusCode := 504
	bifrostErr := &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     &statusCode,
		Error: &schemas.ErrorField{
			Message: "Request timed out by context: context deadline exceeded",
			Type:    schemas.Ptr(schemas.RequestTimedOut),
		},
		ExtraFields: schemas.BifrostErrorExtraFields{
			RequestType:            schemas.ChatCompletionRequest,
			Provider:               schemas.OpenAI,
			OriginalModelRequested: "gpt-4o",
			ResolvedModelUsed:      "gpt-4o",
		},
	}
	if _, _, err = plugin.PostLLMHook(ctx, nil, bifrostErr); err != nil {
		t.Fatalf("PostLLMHook() error = %v", err)
	}
	if err := plugin.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	entry, err := store.FindByID(context.Background(), "req-context-timeout")
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if entry.Status != "cancelled" {
		t.Fatalf("expected cancelled status, got %q", entry.Status)
	}
}

func TestPostLLMHookProviderTimeoutRemainsErrorStatus(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-provider-timeout")

	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-4o",
			Params:   &schemas.ChatParameters{},
		},
	}
	if _, _, err = plugin.PreLLMHook(ctx, req); err != nil {
		t.Fatalf("PreLLMHook() error = %v", err)
	}

	statusCode := 504
	bifrostErr := &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     &statusCode,
		Error: &schemas.ErrorField{
			Message: schemas.ErrProviderRequestTimedOut,
			Type:    schemas.Ptr(schemas.RequestTimedOut),
		},
		ExtraFields: schemas.BifrostErrorExtraFields{
			RequestType:            schemas.ChatCompletionRequest,
			Provider:               schemas.OpenAI,
			OriginalModelRequested: "gpt-4o",
			ResolvedModelUsed:      "gpt-4o",
		},
	}
	if _, _, err = plugin.PostLLMHook(ctx, nil, bifrostErr); err != nil {
		t.Fatalf("PostLLMHook() error = %v", err)
	}
	if err := plugin.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	entry, err := store.FindByID(context.Background(), "req-provider-timeout")
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if entry.Status != "error" {
		t.Fatalf("expected error status, got %q", entry.Status)
	}
}

func TestPostLLMHookCapturesComplexityRoutingContext(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-complexity-capture")

	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-4o",
			Params:   &schemas.ChatParameters{},
		},
	}
	if _, _, err = plugin.PreLLMHook(ctx, req); err != nil {
		t.Fatalf("PreLLMHook() error = %v", err)
	}

	// Set by the governance plugin when a routing rule references complexity_tier.
	ctx.SetValue(schemas.BifrostContextKeyGovernanceComplexityTier, "COMPLEX")
	ctx.SetValue(schemas.BifrostContextKeyGovernanceComplexityMechanism, "semantic")
	ctx.SetValue(schemas.BifrostContextKeyGovernanceComplexityScore, 0.42)
	ctx.SetValue(schemas.BifrostContextKeySessionID, "session-log-123")

	statusCode := 500
	bifrostErr := &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     &statusCode,
		Error:          &schemas.ErrorField{Message: "provider failed"},
		ExtraFields: schemas.BifrostErrorExtraFields{
			RequestType:            schemas.ChatCompletionRequest,
			Provider:               schemas.OpenAI,
			OriginalModelRequested: "gpt-4o",
			ResolvedModelUsed:      "gpt-4o",
		},
	}
	if _, _, err = plugin.PostLLMHook(ctx, nil, bifrostErr); err != nil {
		t.Fatalf("PostLLMHook() error = %v", err)
	}
	if err := plugin.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	entry, err := store.FindByID(context.Background(), "req-complexity-capture")
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if entry.ComplexityTier == nil || *entry.ComplexityTier != "COMPLEX" {
		t.Fatalf("expected complexity_tier COMPLEX, got %v", entry.ComplexityTier)
	}
	if entry.ComplexityMechanism == nil || *entry.ComplexityMechanism != "semantic" {
		t.Fatalf("expected complexity_mechanism semantic, got %v", entry.ComplexityMechanism)
	}
	if entry.ComplexityScore == nil || *entry.ComplexityScore != 0.42 {
		t.Fatalf("expected complexity_score 0.42, got %v", entry.ComplexityScore)
	}
	if entry.SessionID == nil || *entry.SessionID != "session-log-123" {
		t.Fatalf("expected session_id session-log-123, got %v", entry.SessionID)
	}
}

func TestLogStatusForErrorDoesNotTreatGenericDeadlineMessageAsCancelled(t *testing.T) {
	statusCode := 504
	bifrostErr := &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     &statusCode,
		Error: &schemas.ErrorField{
			Message: "provider request hit project deadline exceeded limit",
			Type:    schemas.Ptr(schemas.RequestTimedOut),
		},
	}

	if got := logStatusForError(bifrostErr); got != "error" {
		t.Fatalf("expected generic provider deadline message to remain error, got %q", got)
	}
}

func TestApplyStreamingOutputToEntryPreservesAccumulatorCancelledStatus(t *testing.T) {
	plugin := &LoggerPlugin{}
	entry := &logstore.Log{}
	statusCode := 499
	streamResponse := &streaming.ProcessedStreamResponse{
		Data: &streaming.AccumulatedData{
			ErrorDetails: &schemas.BifrostError{
				IsBifrostError: true,
				StatusCode:     &statusCode,
				Error: &schemas.ErrorField{
					Message: "Request cancelled: client disconnected",
					Type:    schemas.Ptr(schemas.RequestCancelled),
				},
			},
			Latency: 42,
		},
	}

	plugin.applyStreamingOutputToEntry(entry, streamResponse, false, true)
	if entry.Status != "cancelled" {
		t.Fatalf("expected initial cancelled status, got %q", entry.Status)
	}
	if entry.ErrorDetailsParsed == nil {
		t.Fatalf("expected parsed error details to be set")
	}
	if err := entry.SerializeFields(); err != nil {
		t.Fatalf("SerializeFields() error: %v", err)
	}
	if entry.ErrorDetails == "" {
		t.Fatalf("expected serialized error details after SerializeFields")
	}

	// Match the downstream Path B re-derivation in PostLLMHook. If
	// ErrorDetailsParsed is missing, this collapses accumulator-originated
	// cancellations back to "error".
	if entry.ErrorDetailsParsed != nil {
		entry.Status = logStatusForError(entry.ErrorDetailsParsed)
	}
	if entry.Status != "cancelled" {
		t.Fatalf("expected downstream reclassification to preserve cancelled status, got %q", entry.Status)
	}
}

func TestApplyStreamingOutputToEntryPreservesServiceTier(t *testing.T) {
	plugin := &LoggerPlugin{}
	entry := &logstore.Log{}
	priority := schemas.BifrostServiceTierPriority

	plugin.applyStreamingOutputToEntry(entry, &streaming.ProcessedStreamResponse{
		Data: &streaming.AccumulatedData{
			ServiceTier: &priority,
			TokenUsage:  &schemas.BifrostLLMUsage{TotalTokens: 1},
		},
	}, false, true)

	if entry.ServiceTier == nil || *entry.ServiceTier != string(schemas.BifrostServiceTierPriority) {
		t.Fatalf("expected priority service tier, got %v", entry.ServiceTier)
	}
}

// newTestPricingManager builds a ModelCatalog backed by the committed pricing
// testdata via an offline file:// URL (no network).
func newTestPricingManager(t *testing.T) *modelcatalog.ModelCatalog {
	t.Helper()
	abs, err := filepath.Abs("../../framework/modelcatalog/datasheet/testdata/pricing.json")
	if err != nil {
		t.Fatalf("resolve testdata path: %v", err)
	}
	ds := datasheet.New(nil, testLogger{}, datasheet.Config{URL: "file://" + abs})
	if err := ds.LoadFromURLIntoMemory(context.Background()); err != nil {
		t.Fatalf("load pricing datasheet: %v", err)
	}
	return modelcatalog.NewTestCatalogWithDatasheet(ds)
}

func TestApplyInternalCallCostsRoutingOwnershipAndBudgetFlag(t *testing.T) {
	plugin := &LoggerPlugin{pricingManager: newTestPricingManager(t)}
	provider, model, inputTokens := "openai", "text-embedding-3-small", 13

	for _, test := range []struct {
		name               string
		countTowardBudgets bool
		retryNumber        int
		fallbackIndex      int
		wantCost           bool
	}{
		{name: "initial attempt billed", countTowardBudgets: true, wantCost: true},
		{name: "budget attribution disabled", countTowardBudgets: false},
		{name: "retry does not rebill", countTowardBudgets: true, retryNumber: 1},
		{name: "fallback does not rebill", countTowardBudgets: true, fallbackIndex: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			ctx.SetValue(schemas.BifrostContextKeyNumberOfRetries, test.retryNumber)
			ctx.SetValue(schemas.BifrostContextKeyFallbackIndex, test.fallbackIndex)
			if !schemas.AppendRoutingCallOnContext(ctx, schemas.BifrostRoutingCall{
				ProviderUsed:       &provider,
				ModelUsed:          &model,
				InputTokens:        &inputTokens,
				CountTowardBudgets: test.countTowardBudgets,
			}) {
				t.Fatal("AppendRoutingCallOnContext() = false")
			}
			entry := &logstore.Log{Provider: string(schemas.OpenAI), Model: "gpt-4o"}

			plugin.applyInternalCallCosts(ctx, entry, nil)

			if !test.wantCost {
				if entry.Cost != nil {
					t.Fatalf("cost = %v, want nil", *entry.Cost)
				}
				return
			}
			if entry.Cost == nil {
				t.Fatal("cost = nil")
			}
			want := float64(inputTokens) * 2e-8
			if diff := *entry.Cost - want; diff < -1e-12 || diff > 1e-12 {
				t.Fatalf("cost = %v, want %v", *entry.Cost, want)
			}
		})
	}
}

// TestApplyErrorBillingFromBilledUsage_ComputesCostWhenTokensAlreadyParsed guards
// the case where stream accumulation already captured token usage on a failed
// request but no cost was computed: cost must still be backfilled, and the
// already-parsed token counters must be left untouched (not double-applied).
func TestApplyErrorBillingFromBilledUsage_ComputesCostWhenTokensAlreadyParsed(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, newTestPricingManager(t))
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	const promptTokens, completionTokens = 100, 50
	entry := &logstore.Log{
		Provider:         string(schemas.OpenAI),
		Model:            "gpt-4o",
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
		TokenUsageParsed: &schemas.BifrostLLMUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}
	billed := entry.TokenUsageParsed

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	plugin.applyErrorBillingFromBilledUsage(ctx, entry, billed, schemas.ChatCompletionStreamRequest)

	if entry.Cost == nil {
		t.Fatal("expected cost to be computed even though token usage was already parsed")
	}
	// gpt-4o testdata rates: input 2.5e-6/token, output 1e-5/token.
	want := float64(promptTokens)*2.5e-6 + float64(completionTokens)*1e-5
	if diff := *entry.Cost - want; diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("cost %v does not match datasheet-computed %v", *entry.Cost, want)
	}
	if entry.PromptTokens != promptTokens || entry.TotalTokens != promptTokens+completionTokens {
		t.Fatalf("token counters mutated: prompt=%d total=%d", entry.PromptTokens, entry.TotalTokens)
	}
	// The breakdown must ride on the usage so the denormalized split populates,
	// not just the scalar total (Discrepancy B: failed/cancelled billing).
	if entry.TokenUsageParsed.Cost == nil {
		t.Fatal("expected cost breakdown attached to usage for the denormalized split")
	}
	if err := entry.SerializeFields(); err != nil {
		t.Fatalf("SerializeFields: %v", err)
	}
	wantIn := float64(promptTokens) * 2.5e-6
	wantOut := float64(completionTokens) * 1e-5
	if d := entry.InputCost - wantIn; d < -1e-9 || d > 1e-9 {
		t.Fatalf("input_cost %v != %v", entry.InputCost, wantIn)
	}
	if d := entry.OutputCost - wantOut; d < -1e-9 || d > 1e-9 {
		t.Fatalf("output_cost %v != %v", entry.OutputCost, wantOut)
	}
}

// TestApplyErrorBillingFromBilledUsage_PricesEstimatedUsage covers a cancelled
// stream whose usage was estimated: the estimate replaces the accumulator's
// partial usage and its cost, and is priced at the same catalog rates with the
// input/output split carried on the stored usage.
func TestApplyErrorBillingFromBilledUsage_PricesEstimatedUsage(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, newTestPricingManager(t))
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// The accumulator saw no usage chunk, so it priced zero completion tokens.
	staleCost := 1e-6
	entry := &logstore.Log{
		Provider:         string(schemas.OpenAI),
		Model:            "gpt-4o",
		PromptTokens:     0,
		Cost:             &staleCost,
		TokenUsageParsed: &schemas.BifrostLLMUsage{Cost: &schemas.BifrostCost{TotalCost: staleCost}},
	}
	const promptTokens, completionTokens = 120, 40
	billed := &schemas.BifrostLLMUsage{
		PromptTokens: promptTokens, CompletionTokens: completionTokens, TotalTokens: promptTokens + completionTokens,
		Estimation: &schemas.TokenUsageEstimation{Prompt: true, Completion: true, Tokenizer: "o200k_base"},
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	plugin.applyErrorBillingFromBilledUsage(ctx, entry, billed, schemas.ChatCompletionStreamRequest)

	// gpt-4o testdata rates: input 2.5e-6/token, output 1e-5/token.
	wantIn, wantOut := float64(promptTokens)*2.5e-6, float64(completionTokens)*1e-5
	if entry.Cost == nil {
		t.Fatal("no cost computed for the estimated usage")
	}
	if math.Abs(*entry.Cost-(wantIn+wantOut)) > 1e-9 {
		t.Fatalf("cost = %v, want %v from the estimated tokens", *entry.Cost, wantIn+wantOut)
	}
	if entry.PromptTokens != promptTokens || entry.CompletionTokens != completionTokens || entry.TotalTokens != promptTokens+completionTokens {
		t.Fatalf("tokens = %d/%d/%d, want the estimate", entry.PromptTokens, entry.CompletionTokens, entry.TotalTokens)
	}
	if entry.TokenUsageParsed.Estimation == nil || entry.TokenUsageParsed.Cost == nil {
		t.Fatalf("stored usage = %+v, want the estimation provenance and a cost breakdown", entry.TokenUsageParsed)
	}
	if err := entry.SerializeFields(); err != nil {
		t.Fatalf("SerializeFields: %v", err)
	}
	if math.Abs(entry.InputCost-wantIn) > 1e-9 || math.Abs(entry.OutputCost-wantOut) > 1e-9 {
		t.Fatalf("split = %v/%v, want %v/%v", entry.InputCost, entry.OutputCost, wantIn, wantOut)
	}
}

// TestApplyInternalCallCosts_DenormalizesGuardrailToAdditional guards Discrepancy
// B for internal-only sidecar costs: a guardrail judge call with no provider
// response must land on the additional side of the split, not just the total,
// and must not leak into input/output.
func TestApplyInternalCallCosts_DenormalizesGuardrailToAdditional(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, newTestPricingManager(t))
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	entry := &logstore.Log{Provider: string(schemas.OpenAI), Model: "gpt-4o"}
	guardrail := &schemas.BifrostGuardrailMetadata{
		JudgeCalls: []schemas.BifrostGuardrailJudgeCall{{
			JudgeProvider:    schemas.OpenAI,
			JudgeModel:       "gpt-4o",
			PromptTokens:     100,
			CompletionTokens: 50,
			TotalTokens:      150,
		}},
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	plugin.applyInternalCallCosts(ctx, entry, guardrail)

	want := float64(100)*2.5e-6 + float64(50)*1e-5
	if entry.Cost == nil {
		t.Fatal("expected guardrail cost on entry.Cost")
	}
	if d := *entry.Cost - want; d < -1e-9 || d > 1e-9 {
		t.Fatalf("entry.Cost %v != guardrail %v", *entry.Cost, want)
	}
	// No usage carrier exists, so the split is written directly on the columns.
	if d := entry.AdditionalCost - want; d < -1e-9 || d > 1e-9 {
		t.Fatalf("additional_cost %v != guardrail %v", entry.AdditionalCost, want)
	}
	if entry.InputCost != 0 || entry.OutputCost != 0 {
		t.Fatalf("guardrail-only cost must not populate input/output: in=%v out=%v", entry.InputCost, entry.OutputCost)
	}
}

// TestApplyErrorBillingFromBilledUsage_FillsTokensAndCostWhenUnparsed pins the
// original behaviour: when no usage was captured yet, both tokens and cost are
// backfilled from BilledUsage.
func TestApplyErrorBillingFromBilledUsage_FillsTokensAndCostWhenUnparsed(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, newTestPricingManager(t))
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	const promptTokens, completionTokens = 100, 50
	entry := &logstore.Log{Provider: string(schemas.OpenAI), Model: "gpt-4o"}
	billed := &schemas.BifrostLLMUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	plugin.applyErrorBillingFromBilledUsage(ctx, entry, billed, schemas.ChatCompletionStreamRequest)

	if entry.TokenUsageParsed == nil || entry.TotalTokens != promptTokens+completionTokens {
		t.Fatalf("expected tokens backfilled, got %+v", entry.TokenUsageParsed)
	}
	if entry.Cost == nil {
		t.Fatal("expected cost computed on the unparsed path")
	}
}

func TestRecalculateCostsProcessesAllBatches(t *testing.T) {
	store := newTestStore(t)
	plugin := &LoggerPlugin{
		store:          store,
		pricingManager: newTestPricingManager(t),
		logger:         testLogger{},
	}

	now := time.Now().UTC()
	for i := range 5 {
		if err := store.Create(context.Background(), testRecalculateCostLog(
			"req-recalc-batch-"+string(rune('a'+i)),
			now.Add(time.Duration(i)*time.Second),
			100+i,
			50,
		)); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
	}

	result, err := plugin.RecalculateCosts(context.Background(), logstore.SearchFilters{}, 2)
	if err != nil {
		t.Fatalf("RecalculateCosts() error = %v", err)
	}
	if result.TotalMatched != 5 || result.Updated != 5 || result.Skipped != 0 || result.Remaining != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}

	for _, id := range []string{"req-recalc-batch-a", "req-recalc-batch-b", "req-recalc-batch-c", "req-recalc-batch-d", "req-recalc-batch-e"} {
		entry, err := store.FindByID(context.Background(), id)
		if err != nil {
			t.Fatalf("FindByID(%s) error = %v", id, err)
		}
		if entry.Cost == nil || *entry.Cost <= 0 {
			t.Fatalf("expected positive cost for %s, got %v", id, entry.Cost)
		}
	}
}

func TestRecalculateCostsSkipsUnresolvableRowsAndContinues(t *testing.T) {
	store := newTestStore(t)
	plugin := &LoggerPlugin{
		store:          store,
		pricingManager: newTestPricingManager(t),
		logger:         testLogger{},
	}

	now := time.Now().UTC()
	if err := store.Create(context.Background(), &logstore.Log{
		ID:        "req-recalc-skip",
		Timestamp: now,
		Object:    string(schemas.ChatCompletionRequest),
		Provider:  string(schemas.OpenAI),
		Model:     "gpt-4o",
		Status:    "success",
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	for i := range 3 {
		if err := store.Create(context.Background(), testRecalculateCostLog(
			"req-recalc-continue-"+string(rune('a'+i)),
			now.Add(time.Duration(i+1)*time.Second),
			100+i,
			50,
		)); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
	}

	result, err := plugin.RecalculateCosts(context.Background(), logstore.SearchFilters{}, 2)
	if err != nil {
		t.Fatalf("RecalculateCosts() error = %v", err)
	}
	if result.TotalMatched != 4 || result.Updated != 3 || result.Skipped != 1 || result.Remaining != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}

	skipped, err := store.FindByID(context.Background(), "req-recalc-skip")
	if err != nil {
		t.Fatalf("FindByID(req-recalc-skip) error = %v", err)
	}
	if skipped.Cost != nil {
		t.Fatalf("expected skipped row cost to remain nil, got %v", skipped.Cost)
	}
	for _, id := range []string{"req-recalc-continue-a", "req-recalc-continue-b", "req-recalc-continue-c"} {
		entry, err := store.FindByID(context.Background(), id)
		if err != nil {
			t.Fatalf("FindByID(%s) error = %v", id, err)
		}
		if entry.Cost == nil || *entry.Cost <= 0 {
			t.Fatalf("expected positive cost for %s, got %v", id, entry.Cost)
		}
	}
}

func TestRecalculateCostsDoesNotWriteZeroForUnresolvedPricing(t *testing.T) {
	store := newTestStore(t)
	plugin := &LoggerPlugin{
		store:          store,
		pricingManager: newTestPricingManager(t),
		logger:         testLogger{},
	}

	entry := testRecalculateCostLog("req-recalc-unpriced", time.Now().UTC(), 100, 50)
	entry.Model = "unknown-model"
	if err := store.Create(context.Background(), entry); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	result, err := plugin.RecalculateCosts(context.Background(), logstore.SearchFilters{}, 2)
	if err != nil {
		t.Fatalf("RecalculateCosts() error = %v", err)
	}
	if result.TotalMatched != 1 || result.Updated != 0 || result.Skipped != 1 || result.Remaining != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}

	logEntry, err := store.FindByID(context.Background(), "req-recalc-unpriced")
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if logEntry.Cost != nil {
		t.Fatalf("expected unresolved pricing to leave cost nil, got %v", *logEntry.Cost)
	}
}

func TestRecalculateCostsNormalizesProviderObjectForPricing(t *testing.T) {
	store := newTestStore(t)
	plugin := &LoggerPlugin{
		store:          store,
		pricingManager: newTestPricingManager(t),
		logger:         testLogger{},
	}

	entry := testRecalculateCostLog("req-recalc-provider-object", time.Now().UTC(), 100, 50)
	entry.Object = "chat.completion"
	zeroCost := 0.0
	entry.Cost = &zeroCost
	if err := store.Create(context.Background(), entry); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	result, err := plugin.RecalculateCosts(context.Background(), logstore.SearchFilters{}, 2)
	if err != nil {
		t.Fatalf("RecalculateCosts() error = %v", err)
	}
	if result.TotalMatched != 1 || result.Updated != 1 || result.Skipped != 0 || result.Remaining != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}

	logEntry, err := store.FindByID(context.Background(), "req-recalc-provider-object")
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if logEntry.Cost == nil || *logEntry.Cost <= 0 {
		t.Fatalf("expected legacy provider object to recalculate positive cost, got %v", logEntry.Cost)
	}
}

func testRecalculateCostLog(id string, timestamp time.Time, promptTokens int, completionTokens int) *logstore.Log {
	totalTokens := promptTokens + completionTokens
	return &logstore.Log{
		ID:               id,
		Timestamp:        timestamp,
		Object:           string(schemas.ChatCompletionRequest),
		Provider:         string(schemas.OpenAI),
		Model:            "gpt-4o",
		Status:           "success",
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
		TokenUsageParsed: &schemas.BifrostLLMUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      totalTokens,
		},
	}
}

func TestBuildInitialLogEntryPreservesMetadata(t *testing.T) {
	metadata := map[string]any{"tenant": "acme"}
	entry := buildInitialLogEntry(&PendingLogData{
		RequestID:     "req-initial-metadata",
		Timestamp:     time.Now().UTC(),
		FallbackIndex: 1,
		InitialData: &InitialLogData{
			Provider: string(schemas.OpenAI),
			Model:    "gpt-4o",
			Object:   string(schemas.ChatCompletionRequest),
			Metadata: metadata,
		},
	})

	if entry.MetadataParsed == nil {
		t.Fatalf("expected metadata on initial log entry")
	}
	if got := entry.MetadataParsed["tenant"]; got != "acme" {
		t.Fatalf("expected tenant metadata acme, got %#v", got)
	}
}

func TestBuildInitialLogEntryPreservesUserAgent(t *testing.T) {
	userAgent := "codex-cli/1.0"
	entry := buildInitialLogEntry(&PendingLogData{
		RequestID:     "req-initial-user-agent",
		Timestamp:     time.Now().UTC(),
		FallbackIndex: 1,
		InitialData: &InitialLogData{
			Provider:  string(schemas.OpenAI),
			Model:     "gpt-4o",
			Object:    string(schemas.ChatCompletionRequest),
			UserAgent: userAgent,
		},
	})

	if entry.UserAgent == nil {
		t.Fatalf("expected user agent on initial log entry")
	}
	if got := *entry.UserAgent; got != userAgent {
		t.Fatalf("expected user agent %q, got %q", userAgent, got)
	}
	if entry.App == nil {
		t.Fatalf("expected app on initial log entry")
	}
	if got := *entry.App; got != "Codex CLI" {
		t.Fatalf("expected app Codex CLI, got %q", got)
	}
}

func TestBuildCompleteLogEntryPreservesUserAgent(t *testing.T) {
	userAgent := "cursor/0.47"
	entry := buildCompleteLogEntryFromPending(&PendingLogData{
		RequestID:     "req-complete-user-agent",
		Timestamp:     time.Now().UTC(),
		FallbackIndex: 1,
		InitialData: &InitialLogData{
			Provider:  string(schemas.OpenAI),
			Model:     "gpt-4o",
			Object:    string(schemas.ChatCompletionRequest),
			UserAgent: userAgent,
		},
	})

	if entry.UserAgent == nil {
		t.Fatalf("expected user agent on complete log entry")
	}
	if got := *entry.UserAgent; got != userAgent {
		t.Fatalf("expected user agent %q, got %q", userAgent, got)
	}
	if entry.App == nil {
		t.Fatalf("expected app on complete log entry")
	}
	if got := *entry.App; got != "Cursor" {
		t.Fatalf("expected app Cursor, got %q", got)
	}
}

func TestBuildCompleteLogEntryBoundsAgentCorrelationID(t *testing.T) {
	agentCorrelationID := strings.Repeat("a", maxPersistedAgentCorrelationIDLen+1)
	entry := buildCompleteLogEntryFromPending(&PendingLogData{
		RequestID: "req-complete-agent-correlation",
		Timestamp: time.Now().UTC(),
		InitialData: &InitialLogData{
			AgentCorrelationID: agentCorrelationID,
		},
	})

	if entry.AgentCorrelationID == nil || *entry.AgentCorrelationID != agentCorrelationID[:maxPersistedAgentCorrelationIDLen] {
		t.Fatalf("complete log agent correlation ID = %v, want %q", entry.AgentCorrelationID, agentCorrelationID[:maxPersistedAgentCorrelationIDLen])
	}
}

func TestBuildLogEntriesOmitEmptyUserAgent(t *testing.T) {
	pending := &PendingLogData{
		RequestID:     "req-empty-user-agent",
		Timestamp:     time.Now().UTC(),
		FallbackIndex: 1,
		InitialData: &InitialLogData{
			Provider: string(schemas.OpenAI),
			Model:    "gpt-4o",
			Object:   string(schemas.ChatCompletionRequest),
		},
	}

	if entry := buildInitialLogEntry(pending); entry.UserAgent != nil {
		t.Fatalf("expected nil initial user agent, got %#v", entry.UserAgent)
	} else if entry.App != nil {
		t.Fatalf("expected nil initial app, got %#v", entry.App)
	}
	if entry := buildCompleteLogEntryFromPending(pending); entry.UserAgent != nil {
		t.Fatalf("expected nil complete user agent, got %#v", entry.UserAgent)
	} else if entry.App != nil {
		t.Fatalf("expected nil complete app, got %#v", entry.App)
	}
}

// TestPostLLMHookDropsCapturedInputWhenContentIsDisabledLate pins the passthrough ordering hole:
// governance skips PreRequestHook for passthrough and stamps the key's decision in its own
// PreLLMHook, which runs after ours, so the passthrough body is captured while nothing has turned
// content off yet. PostLLMHook must resolve the final policy and drop every captured input, not
// only mark the row content_hidden.
func TestPostLLMHookDropsCapturedInputWhenContentIsDisabledLate(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-passthrough-late-off")
	_, _, err = plugin.PreLLMHook(ctx, &schemas.BifrostRequest{
		RequestType: schemas.PassthroughRequest,
		PassthroughRequest: &schemas.BifrostPassthroughRequest{
			Provider: schemas.OpenAI,
			Method:   "POST",
			Path:     "/v1/fine-tuning/jobs",
			Body:     []byte(`{"training_file":"file-secret"}`),
			// The body is captured only for JSON requests.
			SafeHeaders: map[string]string{"content-type": "application/json"},
		},
	})
	if err != nil {
		t.Fatalf("PreLLMHook() error = %v", err)
	}
	pendingValue, ok := plugin.pendingLogsEntries.Load("req-passthrough-late-off")
	if !ok {
		t.Fatal("expected pending log entry")
	}
	if pendingValue.(*PendingLogData).InitialData.PassthroughRequestBody == "" {
		t.Fatal("the body is captured in PreLLMHook while nothing has turned content off yet")
	}

	// Governance's PreLLMHook runs after ours and stamps the key's decision.
	ctx.SetValue(schemas.BifrostContextKeyGovernanceDisableContentLogging, true)

	_, _, err = plugin.PostLLMHook(ctx, &schemas.BifrostResponse{
		PassthroughResponse: &schemas.BifrostPassthroughResponse{
			StatusCode: 200,
			Body:       []byte(`{"id":"ftjob-1"}`),
			Path:       "/v1/fine-tuning/jobs",
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.PassthroughRequest,
				Provider:    schemas.OpenAI,
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("PostLLMHook() error = %v", err)
	}
	if err := plugin.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	logEntry, err := store.FindByID(context.Background(), "req-passthrough-late-off")
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if !logEntry.ContentHidden {
		t.Fatal("expected the row to be content_hidden")
	}
	if logEntry.PassthroughRequestBody != "" {
		t.Fatalf("expected the passthrough body captured before the key was stamped to be dropped, got %q", logEntry.PassthroughRequestBody)
	}
	if logEntry.PassthroughResponseBody != "" {
		t.Fatalf("expected no passthrough response body, got %q", logEntry.PassthroughResponseBody)
	}
}

// TestPreLLMHookNotificationCarriesNoInputWhileKeyIsUnresolved pins that the live "processing"
// notification PreLLMHook sends to log subscribers never carries captured input for a request
// whose virtual key governance has not stamped yet: the key may turn content off once it does.
func TestPreLLMHookNotificationCarriesNoInputWhileKeyIsUnresolved(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() { _ = plugin.Cleanup() })
	var notified []*logstore.Log
	plugin.SetLogCallback(func(_ context.Context, logEntry *logstore.Log) { notified = append(notified, logEntry) })

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-notify-unresolved")
	ctx.SetValue(schemas.BifrostContextKeyVirtualKey, "sk-bf-presented")
	_, _, err = plugin.PreLLMHook(ctx, &schemas.BifrostRequest{
		RequestType: schemas.PassthroughRequest,
		PassthroughRequest: &schemas.BifrostPassthroughRequest{
			Provider:    schemas.OpenAI,
			Method:      "POST",
			Path:        "/v1/fine-tuning/jobs",
			Body:        []byte(`{"training_file":"file-secret"}`),
			SafeHeaders: map[string]string{"content-type": "application/json"},
		},
	})
	if err != nil {
		t.Fatalf("PreLLMHook() error = %v", err)
	}
	if len(notified) == 0 {
		t.Fatal("expected the processing notification")
	}
	if body := notified[0].PassthroughRequestBody; body != "" {
		t.Fatalf("the notification went out before the key was stamped and must carry no input, got %q", body)
	}
}

// TestPreLLMHookNotificationCarriesNoEmbeddingInputWhileKeyIsUnresolved pins that embedding input,
// captured provisionally because the presented key may turn content on, stays out of the notification.
func TestPreLLMHookNotificationCarriesNoEmbeddingInputWhileKeyIsUnresolved(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{DisableContentLogging: new(true)}, testLogger{}, store, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() { _ = plugin.Cleanup() })
	var notified []*logstore.Log
	plugin.SetLogCallback(func(_ context.Context, logEntry *logstore.Log) { notified = append(notified, logEntry) })

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-notify-unresolved-embedding")
	ctx.SetValue(schemas.BifrostContextKeyVirtualKey, "sk-bf-presented")
	text := "secret embedding text"
	_, _, err = plugin.PreLLMHook(ctx, &schemas.BifrostRequest{
		RequestType: schemas.EmbeddingRequest,
		EmbeddingRequest: &schemas.BifrostEmbeddingRequest{
			Provider: schemas.OpenAI,
			Model:    "text-embedding-3-small",
			Input: []schemas.EmbeddingInputItem{
				{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}},
			},
		},
	})
	if err != nil {
		t.Fatalf("PreLLMHook() error = %v", err)
	}
	if len(notified) == 0 {
		t.Fatal("expected the processing notification")
	}
	if input := notified[0].EmbeddingInputParsed; input != nil {
		t.Fatalf("the notification went out before the key was stamped and must carry no embedding input, got %#v", input)
	}
}

// TestGovernanceDecisionPending pins when the logging hooks treat the content decision on the
// context as not final yet: while a presented virtual key is still unresolved.
func TestGovernanceDecisionPending(t *testing.T) {
	t.Run("nothing presented", func(t *testing.T) {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		require.False(t, governanceDecisionPending(ctx))
	})
	t.Run("a presented key not yet resolved", func(t *testing.T) {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		ctx.SetValue(schemas.BifrostContextKeyVirtualKey, "sk-bf-presented")
		require.True(t, governanceDecisionPending(ctx))
	})
	t.Run("a stamped key is settled", func(t *testing.T) {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		ctx.SetValue(schemas.BifrostContextKeyVirtualKey, "sk-bf-presented")
		ctx.SetValue(schemas.BifrostContextKeyGovernanceVirtualKeyID, "vk-1")
		require.False(t, governanceDecisionPending(ctx))
	})
}

// TestActiveStreamSurvivesCleanup is the regression test for the prod issue where
// streaming requests running longer than the pending TTL had their in-memory
// pending entry evicted mid-flight (causing the final log row to be lost and a
// per-chunk "no pending log data found" warning). An entry whose CreatedAt is
// older than the TTL but whose LastActivity is recent must NOT be reaped.
func TestActiveStreamSurvivesCleanup(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := plugin.Cleanup(); cleanupErr != nil {
			t.Errorf("Cleanup() error = %v", cleanupErr)
		}
	})

	oldCreatedAt := time.Now().Add(-pendingLogTTL - time.Minute)
	pending := &PendingLogData{
		RequestID:   "req-active-stream",
		Timestamp:   oldCreatedAt,
		Status:      "processing",
		InitialData: &InitialLogData{Object: "chat.completion.chunk"},
		CreatedAt:   oldCreatedAt,
	}
	// Simulate a chunk that arrived just now: request started long ago but is
	// still actively streaming.
	pending.LastActivity.Store(time.Now().UnixNano())
	plugin.pendingLogsEntries.Store("req-active-stream", pending)

	plugin.cleanupStalePendingLogs()

	if _, ok := plugin.pendingLogsEntries.Load("req-active-stream"); !ok {
		t.Fatal("expected actively-streaming pending entry to survive cleanup")
	}
}

// TestIdlePendingEntryEvicted verifies the reaper still removes genuinely dead
// requests: an entry whose CreatedAt AND LastActivity are both older than the
// TTL (no chunk activity for the whole idle window) must be deleted.
func TestIdlePendingEntryEvicted(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := plugin.Cleanup(); cleanupErr != nil {
			t.Errorf("Cleanup() error = %v", cleanupErr)
		}
	})

	stale := time.Now().Add(-pendingLogTTL - time.Minute)
	pending := &PendingLogData{
		RequestID:   "req-idle",
		Timestamp:   stale,
		Status:      "processing",
		InitialData: &InitialLogData{Object: "chat.completion.chunk"},
		CreatedAt:   stale,
	}
	pending.LastActivity.Store(stale.UnixNano())
	plugin.pendingLogsEntries.Store("req-idle", pending)

	plugin.cleanupStalePendingLogs()

	if _, ok := plugin.pendingLogsEntries.Load("req-idle"); ok {
		t.Fatal("expected idle pending entry to be evicted by cleanup")
	}
}

func TestStoreOrEnqueueRetryPreservesAllEntries(t *testing.T) {
	// Simulate fallback/retry scenario where multiple PostLLMHook calls
	// store entries under the same traceID. All entries must be preserved.
	plugin := &LoggerPlugin{
		logger:     testLogger{},
		writeQueue: make(chan *writeQueueEntry, 10),
	}

	traceID := "trace-retry-test"
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyTraceID, traceID)

	// Simulate 3 retry attempts storing entries under the same traceID
	entry1 := &logstore.Log{ID: "req-attempt-1", Model: "gpt-4o"}
	entry2 := &logstore.Log{ID: "req-attempt-2", Model: "gpt-4o"}
	entry3 := &logstore.Log{ID: "req-attempt-3", Model: "claude-3-5-sonnet"}

	plugin.storeOrEnqueueEntry(ctx, entry1, nil)
	plugin.storeOrEnqueueEntry(ctx, entry2, nil)
	plugin.storeOrEnqueueEntry(ctx, entry3, nil)

	// Verify all 3 entries are stored
	val, ok := plugin.pendingLogsToInject.Load(traceID)
	if !ok {
		t.Fatal("expected pending entries for traceID, got none")
	}
	pending, ok := val.(*pendingInjectEntries)
	if !ok {
		t.Fatal("expected *pendingInjectEntries type")
	}
	if len(pending.entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(pending.entries))
	}
	if pending.entries[0].ID != "req-attempt-1" || pending.entries[1].ID != "req-attempt-2" || pending.entries[2].ID != "req-attempt-3" {
		t.Fatalf("entries not in expected order: %v, %v, %v", pending.entries[0].ID, pending.entries[1].ID, pending.entries[2].ID)
	}

	// Now test Inject flushes all entries with plugin logs attached
	trace := &schemas.Trace{
		TraceID: traceID,
		PluginLogs: []schemas.PluginLogEntry{
			{PluginName: "hello-world", Level: schemas.LogLevelInfo, Message: "test log"},
		},
	}

	if err := plugin.Inject(context.Background(), trace); err != nil {
		t.Fatalf("Inject() error = %v", err)
	}

	// Verify all 3 entries were enqueued to writeQueue
	if len(plugin.writeQueue) != 3 {
		t.Fatalf("expected 3 entries in writeQueue, got %d", len(plugin.writeQueue))
	}

	// Verify plugin logs were attached to each entry
	for i := 0; i < 3; i++ {
		qe := <-plugin.writeQueue
		if qe.log.PluginLogs == "" {
			t.Fatalf("entry %d: expected PluginLogs to be set", i)
		}
	}

	// Verify pendingLogsToInject was cleaned up
	if _, ok := plugin.pendingLogsToInject.Load(traceID); ok {
		t.Fatal("expected pendingLogsToInject to be cleaned up after Inject")
	}
}

// injectAndCollect runs Inject and returns the enqueued entries keyed by ID.
func injectAndCollect(t *testing.T, entries []*logstore.Log, upstreamMs, overheadMs float64) map[string]*logstore.Log {
	t.Helper()
	plugin := &LoggerPlugin{
		logger:     testLogger{},
		writeQueue: make(chan *writeQueueEntry, len(entries)),
	}
	traceID := "trace-overhead-order"
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyTraceID, traceID)
	for _, e := range entries {
		plugin.storeOrEnqueueEntry(ctx, e, nil)
	}
	trace := &schemas.Trace{
		TraceID: traceID,
		RootSpan: &schemas.Span{
			Attributes: map[string]any{
				schemas.AttrBifrostUpstreamDurationMs: upstreamMs,
				schemas.AttrBifrostOverheadDurationMs: overheadMs,
			},
		},
	}
	if err := plugin.Inject(context.Background(), trace); err != nil {
		t.Fatalf("Inject() error = %v", err)
	}
	out := make(map[string]*logstore.Log, len(entries))
	for len(plugin.writeQueue) > 0 {
		qe := <-plugin.writeQueue
		out[qe.log.ID] = qe.log
	}
	if len(out) != len(entries) {
		t.Fatalf("expected %d enqueued entries, got %d", len(entries), len(out))
	}
	return out
}

// TestInject_StampsOverheadOnLatestTimestampRow guards against positional selection:
// a concurrent list_models fan-out appends entries under one trace in nondeterministic
// completion order, so request-level upstream/overhead must land on the latest-Timestamp
// row, not "whichever slice position happened to be last". Here the latest row is the
// middle one, so a positional "last" would stamp the wrong entry.
func TestInject_StampsOverheadOnLatestTimestampRow(t *testing.T) {
	base := time.Now().UTC()
	got := injectAndCollect(t, []*logstore.Log{
		{ID: "a", Provider: "openai", Timestamp: base},
		{ID: "b", Provider: "anthropic", Timestamp: base.Add(2 * time.Second)}, // latest, but middle
		{ID: "c", Provider: "gemini", Timestamp: base.Add(1 * time.Second)},    // positional last
	}, 10, 5)

	b := got["b"]
	if b.UpstreamLatency == nil || *b.UpstreamLatency != 10 {
		t.Fatalf("entry b UpstreamLatency = %v, want 10", b.UpstreamLatency)
	}
	if b.OverheadLatency == nil || *b.OverheadLatency != 5 {
		t.Fatalf("entry b OverheadLatency = %v, want 5", b.OverheadLatency)
	}
	if b.Latency == nil || *b.Latency != 15 {
		t.Fatalf("entry b Latency = %v, want 15 (upstream+overhead)", b.Latency)
	}
	for _, id := range []string{"a", "c"} {
		if e := got[id]; e.UpstreamLatency != nil || e.OverheadLatency != nil {
			t.Fatalf("entry %s must not carry request-level latency: up=%v ov=%v", id, e.UpstreamLatency, e.OverheadLatency)
		}
	}
}

// TestInject_OverheadTieBrokenByID pins determinism when timestamps collide (the
// realistic fan-out case): the highest ID wins, so the target row is stable across runs.
func TestInject_OverheadTieBrokenByID(t *testing.T) {
	ts := time.Now().UTC()
	got := injectAndCollect(t, []*logstore.Log{
		{ID: "id-c", Provider: "openai", Timestamp: ts},
		{ID: "id-a", Provider: "anthropic", Timestamp: ts},
		{ID: "id-b", Provider: "gemini", Timestamp: ts},
	}, 8, 2)

	if e := got["id-c"]; e.OverheadLatency == nil || *e.OverheadLatency != 2 {
		t.Fatalf("highest ID id-c should carry overhead, got %v", e.OverheadLatency)
	}
	for _, id := range []string{"id-a", "id-b"} {
		if e := got[id]; e.UpstreamLatency != nil || e.OverheadLatency != nil {
			t.Fatalf("entry %s must not carry request-level latency", id)
		}
	}
}

// TestInject_SequentialAttemptsEachCarryOwnLatency pins per-row accounting for a trace
// that logged two attempts (an MCP agent loop's turns): each row carries only its own
// upstream, the first row reaches back to the request start, the last row reaches the
// request end, and upstream spent between the rows (a tool call) lands on neither.
func TestInject_SequentialAttemptsEachCarryOwnLatency(t *testing.T) {
	plugin := &LoggerPlugin{logger: testLogger{}, writeQueue: make(chan *writeQueueEntry, 2)}
	traceID := "trace-agent-turns"
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyTraceID, traceID)
	ctx.ResetUpstreamLatency()

	rootStart := time.Now()
	time.Sleep(10 * time.Millisecond) // transport parse and routing before turn 1
	ctx.MarkAttemptStart()
	time.Sleep(40 * time.Millisecond)
	schemas.AddUpstreamLatency(ctx, 30*time.Millisecond)
	turn1 := &logstore.Log{ID: "turn-1", Timestamp: time.Now().UTC()}
	// Bracket each call that sets a row's window edge, so the latency bounds come from
	// measured instants rather than from how long the sleeps happened to run.
	turn1EndMin := time.Now()
	plugin.storeOrEnqueueEntry(ctx, turn1, nil)
	turn1EndMax := time.Now()

	time.Sleep(20 * time.Millisecond) // tool execution between turns
	schemas.AddUpstreamLatency(ctx, 15*time.Millisecond)

	turn2StartMin := time.Now()
	ctx.MarkAttemptStart()
	turn2StartMax := time.Now()
	time.Sleep(60 * time.Millisecond)
	schemas.AddUpstreamLatency(ctx, 50*time.Millisecond)
	turn2 := &logstore.Log{ID: "turn-2", Timestamp: time.Now().UTC()}
	plugin.storeOrEnqueueEntry(ctx, turn2, nil)
	time.Sleep(10 * time.Millisecond) // response write
	rootEnd := time.Now()

	trace := &schemas.Trace{
		TraceID: traceID,
		RootSpan: &schemas.Span{
			StartTime: rootStart,
			EndTime:   rootEnd,
			Attributes: map[string]any{
				schemas.AttrBifrostUpstreamDurationMs: 95.0,
				schemas.AttrBifrostOverheadDurationMs: durationMs(rootEnd.Sub(rootStart)) - 95.0,
			},
		},
	}
	if err := plugin.Inject(context.Background(), trace); err != nil {
		t.Fatalf("Inject() error = %v", err)
	}
	got := map[string]*logstore.Log{}
	for len(plugin.writeQueue) > 0 {
		qe := <-plugin.writeQueue
		got[qe.log.ID] = qe.log
	}

	checkRow := func(id string, wantUpstream, minLatency, maxLatency float64) {
		t.Helper()
		e := got[id]
		if e == nil {
			t.Fatalf("row %s was not written", id)
		}
		if e.UpstreamLatency == nil || e.OverheadLatency == nil || e.Latency == nil {
			t.Fatalf("row %s: upstream=%v overhead=%v latency=%v, want all three set", id, e.UpstreamLatency, e.OverheadLatency, e.Latency)
		}
		if *e.UpstreamLatency != wantUpstream {
			t.Errorf("row %s upstream = %vms, want %vms (its own turn only)", id, *e.UpstreamLatency, wantUpstream)
		}
		if *e.Latency < minLatency-0.001 || *e.Latency > maxLatency+0.001 {
			t.Errorf("row %s latency = %vms, want within [%v, %v]ms", id, *e.Latency, minLatency, maxLatency)
		}
		if sum := *e.UpstreamLatency + *e.OverheadLatency; sum < *e.Latency-0.001 || sum > *e.Latency+0.001 {
			t.Errorf("row %s upstream + overhead = %vms, want latency %vms", id, sum, *e.Latency)
		}
	}
	// Turn 1 runs from the request start to its store; turn 2 from its mark to the request
	// end. The 15ms tool call between them is in neither.
	checkRow("turn-1", 30, durationMs(turn1EndMin.Sub(rootStart)), durationMs(turn1EndMax.Sub(rootStart)))
	checkRow("turn-2", 50, durationMs(rootEnd.Sub(turn2StartMax)), durationMs(rootEnd.Sub(turn2StartMin)))
}

func TestConvertToProcessedStreamResponseUsesResponsesStreamTypeForWebSocketResponses(t *testing.T) {
	result := &schemas.StreamAccumulatorResult{
		RequestID:      "req-ws-3000",
		RequestedModel: "gpt-4o-mini",
		ResolvedModel:  "gpt-4o-mini",
		Provider:       schemas.OpenAI,
		Status:         "success",
	}

	processed := convertToProcessedStreamResponse(result, schemas.WebSocketResponsesRequest)
	if processed == nil {
		t.Fatal("expected processed stream response, got nil")
	}
	if processed.StreamType != "responses" {
		t.Fatalf("expected stream type responses, got %s", processed.StreamType)
	}
}

func TestApplyRealtimeOutputToEntryBackfillsUserTranscriptFromRawRequest(t *testing.T) {
	plugin := &LoggerPlugin{}
	entry := &logstore.Log{}

	assistantText := "Hello!"
	messageType := schemas.ResponsesMessageTypeMessage
	assistantRole := schemas.ResponsesInputMessageRoleAssistant
	result := &schemas.BifrostResponse{
		ResponsesResponse: &schemas.BifrostResponsesResponse{
			Output: []schemas.ResponsesMessage{{
				Type: &messageType,
				Role: &assistantRole,
				Content: &schemas.ResponsesMessageContent{
					ContentStr: &assistantText,
				},
			}},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.RealtimeRequest,
				RawRequest:  `{"type":"conversation.item.input_audio_transcription.completed","transcript":"Hello."}`,
				RawResponse: `{"type":"response.done"}`,
			},
		},
	}

	plugin.applyRealtimeOutputToEntry(entry, result, true, true)
	if err := entry.SerializeFields(); err != nil {
		t.Fatalf("SerializeFields() error = %v", err)
	}

	if len(entry.InputHistoryParsed) != 1 {
		t.Fatalf("len(InputHistoryParsed) = %d, want 1", len(entry.InputHistoryParsed))
	}
	if entry.InputHistoryParsed[0].Role != schemas.ChatMessageRoleUser {
		t.Fatalf("InputHistoryParsed[0].Role = %q, want user", entry.InputHistoryParsed[0].Role)
	}
	if entry.InputHistoryParsed[0].Content == nil || entry.InputHistoryParsed[0].Content.ContentStr == nil || *entry.InputHistoryParsed[0].Content.ContentStr != "Hello." {
		t.Fatalf("InputHistoryParsed[0] = %+v, want transcript", entry.InputHistoryParsed[0])
	}
	if entry.OutputMessageParsed == nil || entry.OutputMessageParsed.Content == nil || entry.OutputMessageParsed.Content.ContentStr == nil || *entry.OutputMessageParsed.Content.ContentStr != assistantText {
		t.Fatalf("OutputMessageParsed = %+v, want assistant text", entry.OutputMessageParsed)
	}
	if !strings.Contains(entry.ContentSummary, "Hello.") {
		t.Fatalf("ContentSummary = %q, want user transcript", entry.ContentSummary)
	}
	if !strings.Contains(entry.ContentSummary, "Hello!") {
		t.Fatalf("ContentSummary = %q, want assistant text", entry.ContentSummary)
	}
}

func TestApplyRealtimeOutputToEntryBackfillsMissingTranscriptPlaceholder(t *testing.T) {
	plugin := &LoggerPlugin{}
	entry := &logstore.Log{}

	assistantText := "Hi there!"
	messageType := schemas.ResponsesMessageTypeMessage
	assistantRole := schemas.ResponsesInputMessageRoleAssistant
	result := &schemas.BifrostResponse{
		ResponsesResponse: &schemas.BifrostResponsesResponse{
			Output: []schemas.ResponsesMessage{{
				Type: &messageType,
				Role: &assistantRole,
				Content: &schemas.ResponsesMessageContent{
					ContentStr: &assistantText,
				},
			}},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.RealtimeRequest,
				RawRequest:  `{"type":"conversation.item.input_audio_transcription.completed","transcript":""}`,
				RawResponse: `{"type":"response.done"}`,
			},
		},
	}

	plugin.applyRealtimeOutputToEntry(entry, result, true, true)
	if err := entry.SerializeFields(); err != nil {
		t.Fatalf("SerializeFields() error = %v", err)
	}

	if len(entry.InputHistoryParsed) != 1 {
		t.Fatalf("len(InputHistoryParsed) = %d, want 1", len(entry.InputHistoryParsed))
	}
	if entry.InputHistoryParsed[0].Content == nil || entry.InputHistoryParsed[0].Content.ContentStr == nil || *entry.InputHistoryParsed[0].Content.ContentStr != realtimeMissingTranscriptText {
		t.Fatalf("InputHistoryParsed[0] = %+v, want missing transcript placeholder", entry.InputHistoryParsed[0])
	}
	if !strings.Contains(entry.ContentSummary, realtimeMissingTranscriptText) {
		t.Fatalf("ContentSummary = %q, want missing transcript placeholder", entry.ContentSummary)
	}
}

func TestApplyRealtimeOutputToEntryBackfillsDoneMissingTranscriptPlaceholder(t *testing.T) {
	plugin := &LoggerPlugin{}
	entry := &logstore.Log{}

	assistantText := "Hi there!"
	messageType := schemas.ResponsesMessageTypeMessage
	assistantRole := schemas.ResponsesInputMessageRoleAssistant
	result := &schemas.BifrostResponse{
		ResponsesResponse: &schemas.BifrostResponsesResponse{
			Output: []schemas.ResponsesMessage{{
				Type: &messageType,
				Role: &assistantRole,
				Content: &schemas.ResponsesMessageContent{
					ContentStr: &assistantText,
				},
			}},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.RealtimeRequest,
				RawRequest:  `{"type":"conversation.item.done","item":{"id":"item_user","type":"message","role":"user","status":"completed","content":[{"type":"input_audio","transcript":null}]}}`,
				RawResponse: `{"type":"response.done"}`,
			},
		},
	}

	plugin.applyRealtimeOutputToEntry(entry, result, true, true)
	if err := entry.SerializeFields(); err != nil {
		t.Fatalf("SerializeFields() error = %v", err)
	}

	if len(entry.InputHistoryParsed) != 1 {
		t.Fatalf("len(InputHistoryParsed) = %d, want 1", len(entry.InputHistoryParsed))
	}
	if entry.InputHistoryParsed[0].Content == nil || entry.InputHistoryParsed[0].Content.ContentStr == nil || *entry.InputHistoryParsed[0].Content.ContentStr != realtimeMissingTranscriptText {
		t.Fatalf("InputHistoryParsed[0] = %+v, want missing transcript placeholder", entry.InputHistoryParsed[0])
	}
}

func TestApplyRealtimeOutputToEntryBackfillsRetrievedUserAndToolHistory(t *testing.T) {
	plugin := &LoggerPlugin{}
	entry := &logstore.Log{}

	assistantText := "I checked that for you."
	messageType := schemas.ResponsesMessageTypeMessage
	assistantRole := schemas.ResponsesInputMessageRoleAssistant
	result := &schemas.BifrostResponse{
		ResponsesResponse: &schemas.BifrostResponsesResponse{
			Output: []schemas.ResponsesMessage{{
				Type: &messageType,
				Role: &assistantRole,
				Content: &schemas.ResponsesMessageContent{
					ContentStr: &assistantText,
				},
			}},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.RealtimeRequest,
				RawRequest: strings.Join([]string{
					`{"type":"conversation.item.retrieved","item":{"id":"item_user","type":"message","role":"user","status":"completed","content":[{"type":"input_text","text":"Where is my order?"}]}}`,
					`{"type":"conversation.item.retrieved","item":{"id":"item_tool","type":"function_call_output","call_id":"call_123","status":"completed","output":"{\"status\":\"delivered\"}"}}`,
				}, "\n\n"),
				RawResponse: `{"type":"response.done"}`,
			},
		},
	}

	plugin.applyRealtimeOutputToEntry(entry, result, true, true)
	if err := entry.SerializeFields(); err != nil {
		t.Fatalf("SerializeFields() error = %v", err)
	}

	if len(entry.InputHistoryParsed) != 2 {
		t.Fatalf("len(InputHistoryParsed) = %d, want 2", len(entry.InputHistoryParsed))
	}
	if entry.InputHistoryParsed[0].Role != schemas.ChatMessageRoleUser {
		t.Fatalf("InputHistoryParsed[0].Role = %q, want user", entry.InputHistoryParsed[0].Role)
	}
	if entry.InputHistoryParsed[0].Content == nil || entry.InputHistoryParsed[0].Content.ContentStr == nil || *entry.InputHistoryParsed[0].Content.ContentStr != "Where is my order?" {
		t.Fatalf("InputHistoryParsed[0] = %+v, want user content", entry.InputHistoryParsed[0])
	}
	if entry.InputHistoryParsed[1].Role != schemas.ChatMessageRoleTool {
		t.Fatalf("InputHistoryParsed[1].Role = %q, want tool", entry.InputHistoryParsed[1].Role)
	}
	if entry.InputHistoryParsed[1].Content == nil || entry.InputHistoryParsed[1].Content.ContentStr == nil || *entry.InputHistoryParsed[1].Content.ContentStr != `{"status":"delivered"}` {
		t.Fatalf("InputHistoryParsed[1] = %+v, want tool content", entry.InputHistoryParsed[1])
	}
	if entry.InputHistoryParsed[1].ChatToolMessage == nil || entry.InputHistoryParsed[1].ChatToolMessage.ToolCallID == nil || *entry.InputHistoryParsed[1].ChatToolMessage.ToolCallID != "call_123" {
		t.Fatalf("InputHistoryParsed[1].ChatToolMessage = %+v, want tool call id", entry.InputHistoryParsed[1].ChatToolMessage)
	}
}

func TestApplyRealtimeOutputToEntryBackfillsCreatedUserAndToolHistory(t *testing.T) {
	t.Parallel()

	plugin := &LoggerPlugin{}
	entry := &logstore.Log{}
	result := &schemas.BifrostResponse{
		ResponsesResponse: &schemas.BifrostResponsesResponse{
			ExtraFields: schemas.BifrostResponseExtraFields{
				RawRequest: strings.Join([]string{
					`{"type":"conversation.item.created","item":{"id":"item_user","type":"message","role":"user","status":"completed","content":[{"type":"input_text","text":"I need help"}]}}`,
					`{"type":"conversation.item.created","item":{"id":"item_tool","type":"function_call_output","call_id":"call_456","status":"completed","output":"{\"status\":\"ok\"}"}}`,
				}, "\n\n"),
			},
		},
	}

	plugin.applyRealtimeOutputToEntry(entry, result, true, true)

	if len(entry.InputHistoryParsed) != 2 {
		t.Fatalf("len(InputHistoryParsed) = %d, want 2", len(entry.InputHistoryParsed))
	}
	if entry.InputHistoryParsed[0].Role != schemas.ChatMessageRoleUser {
		t.Fatalf("InputHistoryParsed[0].Role = %q, want user", entry.InputHistoryParsed[0].Role)
	}
	if entry.InputHistoryParsed[0].Content == nil || entry.InputHistoryParsed[0].Content.ContentStr == nil || *entry.InputHistoryParsed[0].Content.ContentStr != "I need help" {
		t.Fatalf("InputHistoryParsed[0] = %+v, want user content", entry.InputHistoryParsed[0])
	}
	if entry.InputHistoryParsed[1].Role != schemas.ChatMessageRoleTool {
		t.Fatalf("InputHistoryParsed[1].Role = %q, want tool", entry.InputHistoryParsed[1].Role)
	}
	if entry.InputHistoryParsed[1].Content == nil || entry.InputHistoryParsed[1].Content.ContentStr == nil || *entry.InputHistoryParsed[1].Content.ContentStr != `{"status":"ok"}` {
		t.Fatalf("InputHistoryParsed[1] = %+v, want tool content", entry.InputHistoryParsed[1])
	}
	if entry.InputHistoryParsed[1].ChatToolMessage == nil || entry.InputHistoryParsed[1].ChatToolMessage.ToolCallID == nil || *entry.InputHistoryParsed[1].ChatToolMessage.ToolCallID != "call_456" {
		t.Fatalf("InputHistoryParsed[1].ChatToolMessage = %+v, want tool call id", entry.InputHistoryParsed[1].ChatToolMessage)
	}
}

func TestApplyRealtimeOutputToEntryBackfillsAddedUserAndToolHistory(t *testing.T) {
	t.Parallel()

	plugin := &LoggerPlugin{}
	entry := &logstore.Log{}

	assistantText := "Done."
	messageType := schemas.ResponsesMessageTypeMessage
	assistantRole := schemas.ResponsesInputMessageRoleAssistant
	result := &schemas.BifrostResponse{
		ResponsesResponse: &schemas.BifrostResponsesResponse{
			Output: []schemas.ResponsesMessage{{
				Type: &messageType,
				Role: &assistantRole,
				Content: &schemas.ResponsesMessageContent{
					ContentStr: &assistantText,
				},
			}},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.RealtimeRequest,
				RawRequest: strings.Join([]string{
					`{"type":"conversation.item.added","item":{"id":"item_user","type":"message","role":"user","status":"completed","content":[{"type":"input_text","text":"hello from added item"}]}}`,
					`{"type":"conversation.item.added","item":{"id":"item_tool","type":"function_call_output","call_id":"call_added","status":"completed","output":"{\"status\":\"ok\"}"}}`,
				}, "\n\n"),
				RawResponse: `{"type":"response.done"}`,
			},
		},
	}

	plugin.applyRealtimeOutputToEntry(entry, result, true, true)
	if err := entry.SerializeFields(); err != nil {
		t.Fatalf("SerializeFields() error = %v", err)
	}

	if len(entry.InputHistoryParsed) != 2 {
		t.Fatalf("len(InputHistoryParsed) = %d, want 2", len(entry.InputHistoryParsed))
	}
	if entry.InputHistoryParsed[0].Content == nil || entry.InputHistoryParsed[0].Content.ContentStr == nil || *entry.InputHistoryParsed[0].Content.ContentStr != "hello from added item" {
		t.Fatalf("InputHistoryParsed[0] = %+v, want added user content", entry.InputHistoryParsed[0])
	}
	if entry.InputHistoryParsed[1].ChatToolMessage == nil || entry.InputHistoryParsed[1].ChatToolMessage.ToolCallID == nil || *entry.InputHistoryParsed[1].ChatToolMessage.ToolCallID != "call_added" {
		t.Fatalf("InputHistoryParsed[1].ChatToolMessage = %+v, want added tool call id", entry.InputHistoryParsed[1].ChatToolMessage)
	}
}

func TestApplyRealtimeOutputToEntryMergesRawTranscriptIntoStructuredRealtimeHistory(t *testing.T) {
	t.Parallel()

	plugin := &LoggerPlugin{}
	entry := &logstore.Log{
		InputHistoryParsed: []schemas.ChatMessage{
			{
				Role: schemas.ChatMessageRoleUser,
				Content: &schemas.ChatMessageContent{
					ContentStr: schemas.Ptr("Can you help with my ticket?"),
				},
			},
			{
				Role: schemas.ChatMessageRoleTool,
				Content: &schemas.ChatMessageContent{
					ContentStr: schemas.Ptr(`{"status":"open"}`),
				},
				ChatToolMessage: &schemas.ChatToolMessage{
					ToolCallID: schemas.Ptr("call_789"),
				},
			},
		},
	}

	assistantText := "Let me check."
	messageType := schemas.ResponsesMessageTypeMessage
	assistantRole := schemas.ResponsesInputMessageRoleAssistant
	result := &schemas.BifrostResponse{
		ResponsesResponse: &schemas.BifrostResponsesResponse{
			Output: []schemas.ResponsesMessage{{
				Type: &messageType,
				Role: &assistantRole,
				Content: &schemas.ResponsesMessageContent{
					ContentStr: &assistantText,
				},
			}},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.RealtimeRequest,
				RawRequest: strings.Join([]string{
					`{"type":"conversation.item.input_audio_transcription.completed","transcript":"Hello."}`,
					`{"type":"conversation.item.retrieved","item":{"id":"item_tool","type":"function_call_output","call_id":"call_789","status":"completed","output":"{\"status\":\"open\"}"}}`,
				}, "\n\n"),
				RawResponse: `{"type":"response.done"}`,
			},
		},
	}

	plugin.applyRealtimeOutputToEntry(entry, result, true, true)
	if err := entry.SerializeFields(); err != nil {
		t.Fatalf("SerializeFields() error = %v", err)
	}

	if len(entry.InputHistoryParsed) != 3 {
		t.Fatalf("len(InputHistoryParsed) = %d, want 3", len(entry.InputHistoryParsed))
	}
	if entry.InputHistoryParsed[0].Content == nil || entry.InputHistoryParsed[0].Content.ContentStr == nil || *entry.InputHistoryParsed[0].Content.ContentStr != "Can you help with my ticket?" {
		t.Fatalf("InputHistoryParsed[0] = %+v, want structured user content", entry.InputHistoryParsed[0])
	}
	if entry.InputHistoryParsed[1].Role != schemas.ChatMessageRoleUser {
		t.Fatalf("InputHistoryParsed[1].Role = %q, want user", entry.InputHistoryParsed[1].Role)
	}
	if entry.InputHistoryParsed[1].Content == nil || entry.InputHistoryParsed[1].Content.ContentStr == nil || *entry.InputHistoryParsed[1].Content.ContentStr != "Hello." {
		t.Fatalf("InputHistoryParsed[1] = %+v, want raw transcript merge", entry.InputHistoryParsed[1])
	}
	if entry.InputHistoryParsed[2].Role != schemas.ChatMessageRoleTool {
		t.Fatalf("InputHistoryParsed[2].Role = %q, want tool", entry.InputHistoryParsed[2].Role)
	}
	if entry.InputHistoryParsed[2].ChatToolMessage == nil || entry.InputHistoryParsed[2].ChatToolMessage.ToolCallID == nil || *entry.InputHistoryParsed[2].ChatToolMessage.ToolCallID != "call_789" {
		t.Fatalf("InputHistoryParsed[2].ChatToolMessage = %+v, want original tool call id", entry.InputHistoryParsed[2].ChatToolMessage)
	}
	if strings.Count(entry.ContentSummary, "Hello.") != 1 {
		t.Fatalf("ContentSummary = %q, want one merged transcript", entry.ContentSummary)
	}
}

func TestApplyRealtimeOutputToEntryDoesNotPersistRawWhenShouldStoreRawFalse(t *testing.T) {
	plugin := &LoggerPlugin{}
	entry := &logstore.Log{}

	assistantText := "Hello!"
	messageType := schemas.ResponsesMessageTypeMessage
	assistantRole := schemas.ResponsesInputMessageRoleAssistant
	result := &schemas.BifrostResponse{
		ResponsesResponse: &schemas.BifrostResponsesResponse{
			Output: []schemas.ResponsesMessage{{
				Type: &messageType,
				Role: &assistantRole,
				Content: &schemas.ResponsesMessageContent{
					ContentStr: &assistantText,
				},
			}},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.RealtimeRequest,
				RawRequest:  `{"type":"conversation.item.input_audio_transcription.completed","transcript":"Hello."}`,
				RawResponse: `{"type":"response.done"}`,
			},
		},
	}

	plugin.applyRealtimeOutputToEntry(entry, result, false, true)

	if entry.RawRequest != "" {
		t.Fatalf("expected RawRequest to remain empty when shouldStoreRaw=false, got %q", entry.RawRequest)
	}
	if entry.RawResponse != "" {
		t.Fatalf("expected RawResponse to remain empty when shouldStoreRaw=false, got %q", entry.RawResponse)
	}
	if len(entry.InputHistoryParsed) == 0 {
		t.Fatal("expected InputHistoryParsed to still be backfilled when shouldStoreRaw=false")
	}
	if entry.InputHistoryParsed[0].Role != schemas.ChatMessageRoleUser {
		t.Fatalf("InputHistoryParsed[0].Role = %q, want user", entry.InputHistoryParsed[0].Role)
	}
}

// TestContentLoggingEnabledHelper verifies precedence: ctx override > global config > default-enabled.
func TestContentLoggingEnabledHelper(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }

	tests := []struct {
		name          string
		globalDisable *bool
		ctxOverride   *bool // nil = don't set the key
		want          bool
	}{
		{"no config no override → enabled", nil, nil, true},
		{"global disable=false no override → enabled", boolPtr(false), nil, true},
		{"global disable=true no override → disabled", boolPtr(true), nil, false},
		{"ctx override=false global disable=true → enabled", boolPtr(true), boolPtr(false), true},
		{"ctx override=true global disable=false → disabled", boolPtr(false), boolPtr(true), false},
		{"ctx override=true nil global → disabled", nil, boolPtr(true), false},
		{"ctx override=false nil global → enabled", nil, boolPtr(false), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &LoggerPlugin{disableContentLogging: tc.globalDisable}

			var ctx *schemas.BifrostContext
			if tc.ctxOverride != nil {
				ctx = schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
				ctx.SetValue(schemas.BifrostContextKeyAllowPerRequestStorageOverride, true)
				ctx.SetValue(schemas.BifrostContextKeyDisableContentLogging, *tc.ctxOverride)
			}

			got := p.contentLoggingEnabled(ctx)
			if got != tc.want {
				t.Errorf("contentLoggingEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestContentLoggingEnabledHelperNilCtx verifies nil context falls back to global config.
func TestContentLoggingEnabledHelperNilCtx(t *testing.T) {
	disabled := true
	p := &LoggerPlugin{disableContentLogging: &disabled}
	if p.contentLoggingEnabled(nil) {
		t.Error("expected false with nil ctx and global disable=true")
	}
}

// TestApplyNonStreamingOutputToEntryContentLoggingDisabled verifies that output fields are
// suppressed when contentLoggingEnabled=false.
func TestApplyNonStreamingOutputToEntryContentLoggingDisabled(t *testing.T) {
	plugin := &LoggerPlugin{}
	entry := &logstore.Log{}

	chatText := "should not appear"
	result := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Choices: []schemas.BifrostResponseChoice{
				{
					ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
						Message: &schemas.ChatMessage{
							Role:    schemas.ChatMessageRoleAssistant,
							Content: &schemas.ChatMessageContent{ContentStr: &chatText},
						},
					},
				},
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
			},
		},
	}

	plugin.applyNonStreamingOutputToEntry(entry, result, false, false)

	if entry.OutputMessageParsed != nil {
		t.Error("expected OutputMessageParsed to be nil when contentLoggingEnabled=false")
	}
}

// TestApplyNonStreamingOutputToEntryContentLoggingEnabled verifies that output fields are
// stored when contentLoggingEnabled=true regardless of the global plugin config.
func TestApplyNonStreamingOutputToEntryContentLoggingEnabled(t *testing.T) {
	disabled := true
	plugin := &LoggerPlugin{disableContentLogging: &disabled} // global off, but explicit true passed
	entry := &logstore.Log{}

	chatText := "should appear"
	result := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Choices: []schemas.BifrostResponseChoice{
				{
					ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
						Message: &schemas.ChatMessage{
							Role:    schemas.ChatMessageRoleAssistant,
							Content: &schemas.ChatMessageContent{ContentStr: &chatText},
						},
					},
				},
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
			},
		},
	}

	plugin.applyNonStreamingOutputToEntry(entry, result, false, true)

	if entry.OutputMessageParsed == nil {
		t.Error("expected OutputMessageParsed to be set when contentLoggingEnabled=true")
	}
}

// TestApplyNonStreamingOutputToEntryVideoOutputs verifies that each video response lands in
// its own log column. Generation, remix and retrieve all return BifrostVideoGenerationResponse,
// so the request type is the only thing separating generation from retrieve.
func TestApplyNonStreamingOutputToEntryVideoOutputs(t *testing.T) {
	videoResponse := func(requestType schemas.RequestType) *schemas.BifrostResponse {
		return &schemas.BifrostResponse{
			VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{
				ID:          "vid_123:openai",
				Status:      schemas.VideoStatusQueued,
				ExtraFields: schemas.BifrostResponseExtraFields{RequestType: requestType},
			},
		}
	}

	tests := []struct {
		name   string
		result *schemas.BifrostResponse
		verify func(t *testing.T, entry *logstore.Log)
	}{
		{
			name:   "generation",
			result: videoResponse(schemas.VideoGenerationRequest),
			verify: func(t *testing.T, entry *logstore.Log) {
				if entry.VideoGenerationOutputParsed == nil {
					t.Fatal("expected VideoGenerationOutputParsed to be set")
				}
				if entry.VideoRetrieveOutputParsed != nil {
					t.Error("expected VideoRetrieveOutputParsed to stay nil")
				}
				if entry.VideoGenerationOutputParsed.ID != "vid_123:openai" {
					t.Errorf("expected provider-scoped ID, got %q", entry.VideoGenerationOutputParsed.ID)
				}
			},
		},
		{
			name:   "remix routes to generation",
			result: videoResponse(schemas.VideoRemixRequest),
			verify: func(t *testing.T, entry *logstore.Log) {
				if entry.VideoGenerationOutputParsed == nil {
					t.Error("expected VideoGenerationOutputParsed to be set")
				}
			},
		},
		{
			name:   "retrieve",
			result: videoResponse(schemas.VideoRetrieveRequest),
			verify: func(t *testing.T, entry *logstore.Log) {
				if entry.VideoRetrieveOutputParsed == nil {
					t.Fatal("expected VideoRetrieveOutputParsed to be set")
				}
				if entry.VideoGenerationOutputParsed != nil {
					t.Error("expected VideoGenerationOutputParsed to stay nil")
				}
			},
		},
		{
			name: "download",
			result: &schemas.BifrostResponse{
				VideoDownloadResponse: &schemas.BifrostVideoDownloadResponse{
					VideoID:     "vid_123:openai",
					ContentType: "video/mp4",
					ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.VideoDownloadRequest},
				},
			},
			verify: func(t *testing.T, entry *logstore.Log) {
				if entry.VideoDownloadOutputParsed == nil {
					t.Error("expected VideoDownloadOutputParsed to be set")
				}
			},
		},
		{
			name: "list",
			result: &schemas.BifrostResponse{
				VideoListResponse: &schemas.BifrostVideoListResponse{
					Object:      "list",
					Data:        []schemas.VideoObject{{ID: "vid_123:openai"}},
					ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.VideoListRequest},
				},
			},
			verify: func(t *testing.T, entry *logstore.Log) {
				if entry.VideoListOutputParsed == nil {
					t.Error("expected VideoListOutputParsed to be set")
				}
			},
		},
		{
			name: "delete",
			result: &schemas.BifrostResponse{
				VideoDeleteResponse: &schemas.BifrostVideoDeleteResponse{
					ID:          "vid_123:openai",
					Deleted:     true,
					ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.VideoDeleteRequest},
				},
			},
			verify: func(t *testing.T, entry *logstore.Log) {
				if entry.VideoDeleteOutputParsed == nil {
					t.Error("expected VideoDeleteOutputParsed to be set")
				}
			},
		},
	}

	plugin := &LoggerPlugin{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := &logstore.Log{}
			plugin.applyNonStreamingOutputToEntry(entry, tt.result, false, true)
			tt.verify(t, entry)
		})
	}

	t.Run("content logging disabled", func(t *testing.T) {
		entry := &logstore.Log{}
		plugin.applyNonStreamingOutputToEntry(entry, videoResponse(schemas.VideoGenerationRequest), false, false)
		if entry.VideoGenerationOutputParsed != nil {
			t.Error("expected VideoGenerationOutputParsed to be nil when contentLoggingEnabled=false")
		}
	})
}

// TestGuardrailMetadataForLogReadsContextWithoutResponse verifies input blocks remain observable.
func TestGuardrailMetadataForLogReadsContextWithoutResponse(t *testing.T) {
	ctx := schemas.NewBifrostContext(nil, schemas.NoDeadline)
	requireCall := schemas.BifrostGuardrailJudgeCall{
		JudgeProvider: schemas.OpenAI,
		JudgeModel:    "gpt-test",
		TotalTokens:   18,
	}
	if !schemas.AppendGuardrailJudgeCallOnContext(ctx, requireCall) {
		t.Fatal("failed to append guardrail judge call")
	}

	metadata := guardrailMetadataForLog(ctx, nil)
	if metadata == nil || len(metadata.JudgeCalls) != 1 {
		t.Fatalf("guardrail metadata = %#v; want one context judge call", metadata)
	}
	if !reflect.DeepEqual(metadata.JudgeCalls[0], requireCall) {
		t.Fatalf("guardrail call = %#v; want %#v", metadata.JudgeCalls[0], requireCall)
	}
}

// Batch claims are fenced on the runner id, so it must differ between workers
// that could contend for the same job. In OSS SetClusterNodeID is never called,
// so the per-process fallback is what keeps replicas sharing a database apart.
func TestBatchRunnerID(t *testing.T) {
	plugin := &LoggerPlugin{}

	t.Run("falls back to a per-process id when no cluster node is set", func(t *testing.T) {
		id := plugin.batchRunnerID("logging")
		if id == "logging" || id == "logging:" {
			t.Fatalf("runner id must not collapse to a shared constant, got %q", id)
		}
		if !strings.HasPrefix(id, "logging:") {
			t.Fatalf("expected a logging: prefix, got %q", id)
		}
		// Distinct roles in the same process must not collide either.
		if other := plugin.batchRunnerID("batch-sweeper"); other == id {
			t.Fatalf("distinct prefixes produced the same runner id: %q", id)
		}
	})

	t.Run("prefers the cluster node id when present", func(t *testing.T) {
		clustered := &LoggerPlugin{}
		clustered.clusterNodeID.Store("node-7")
		if got := clustered.batchRunnerID("logging"); got != "logging:node-7" {
			t.Fatalf("expected logging:node-7, got %q", got)
		}
	})
}

func TestRecordBatchJobLifecycle_CompletedSetsNextCheckAt(t *testing.T) {
	store := newTestStore(t)
	bs := newFakeBatchStore()
	plugin := &LoggerPlugin{
		ctx:        context.Background(),
		store:      store,
		batchStore: bs,
		logger:     testLogger{},
	}

	tests := []struct {
		name       string
		status     string
		wantNow    bool
		wantMinute bool
		wantNil    bool
	}{
		{name: "in_progress", status: string(schemas.BatchStatusInProgress), wantMinute: true},
		{name: "validating", status: string(schemas.BatchStatusValidating), wantMinute: true},
		{name: "finalizing", status: string(schemas.BatchStatusFinalizing), wantMinute: true},
		{name: "completed", status: string(schemas.BatchStatusCompleted), wantNow: true},
		// Anthropic reports "ended"; it takes the same immediate-accounting branch.
		{name: "ended", status: string(schemas.BatchStatusEnded), wantNow: true},
		{name: "failed", status: string(schemas.BatchStatusFailed), wantNil: true},
		{name: "cancelled", status: string(schemas.BatchStatusCancelled), wantNil: true},
		{name: "expired", status: string(schemas.BatchStatusExpired), wantNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			batchID := "batch-" + tt.name
			entry := &logstore.Log{
				Provider: string(schemas.OpenAI),
				Model:    "gpt-4o-mini",
			}
			retrieveResp := &schemas.BifrostBatchRetrieveResponse{
				ID:       batchID,
				Endpoint: string(schemas.BatchEndpointChatCompletions),
				Status:   schemas.BatchStatus(tt.status),
			}
			result := &schemas.BifrostResponse{
				BatchRetrieveResponse: retrieveResp,
			}
			plugin.recordBatchJobLifecycle(entry, result)

			job, err := bs.GetProviderJob(context.Background(), cstables.ProviderJobID(cstables.ProviderJobKindBatch, "openai", batchID))
			if err != nil {
				t.Fatalf("GetProviderJob() error = %v", err)
			}
			if job.JobID != batchID {
				t.Fatalf("expected batch_id %s, got %s", batchID, job.JobID)
			}
			if job.ProviderStatus != tt.status {
				t.Fatalf("expected status %s, got %s", tt.status, job.ProviderStatus)
			}

			switch {
			case tt.wantNow:
				if job.NextCheckAt == nil {
					t.Fatal("expected NextCheckAt to be set")
				}
				if job.NextCheckAt.After(time.Now().UTC().Add(2 * time.Second)) {
					t.Fatalf("expected NextCheckAt to be now, got %v", job.NextCheckAt)
				}
			case tt.wantMinute:
				if job.NextCheckAt == nil {
					t.Fatal("expected NextCheckAt to be set")
				}
				if job.NextCheckAt.Before(time.Now().UTC().Add(50*time.Second)) ||
					job.NextCheckAt.After(time.Now().UTC().Add(70*time.Second)) {
					t.Fatalf("expected NextCheckAt ~1min from now, got %v (now=%v)", job.NextCheckAt, time.Now().UTC())
				}
			case tt.wantNil:
				if job.NextCheckAt != nil {
					t.Fatalf("expected NextCheckAt to be nil, got %v", *job.NextCheckAt)
				}
			}
		})
	}
}

func TestRecordBatchJobLifecycle_CreatePersistsModel(t *testing.T) {
	store := newTestStore(t)
	bs := newFakeBatchStore()
	plugin := &LoggerPlugin{
		ctx:        context.Background(),
		store:      store,
		batchStore: bs,
		logger:     testLogger{},
	}

	entry := &logstore.Log{
		ID:       "req-create-batch",
		Provider: string(schemas.OpenAI),
		Model:    "gpt-4o",
	}
	createResp := &schemas.BifrostBatchCreateResponse{
		ID:          "batch-create-123",
		Status:      schemas.BatchStatusValidating,
		InputFileID: "file-abc",
	}
	result := &schemas.BifrostResponse{
		BatchCreateResponse: createResp,
	}
	plugin.recordBatchJobLifecycle(entry, result)

	job, err := bs.GetProviderJob(context.Background(), cstables.ProviderJobID(cstables.ProviderJobKindBatch, "openai", "batch-create-123"))
	if err != nil {
		t.Fatalf("GetProviderJob() error = %v", err)
	}
	if job.Model != "gpt-4o" {
		t.Fatalf("expected model %s, got %s", "gpt-4o", job.Model)
	}
	if job.InputFileID != "file-abc" {
		t.Fatalf("expected input_file_id %s, got %s", "file-abc", job.InputFileID)
	}
	if job.AccountingStatus != cstables.ProviderJobAccountingStatusPending {
		t.Fatalf("expected accounting_status %s, got %s", cstables.ProviderJobAccountingStatusPending, job.AccountingStatus)
	}
}

func TestAccountBatchResults_NonResultsResponseIsNoop(t *testing.T) {
	store := newTestStore(t)
	bs := newFakeBatchStore()
	plugin := &LoggerPlugin{
		ctx:        context.Background(),
		store:      store,
		batchStore: bs,
		logger:     testLogger{},
		// Pricing must be set or accountBatchResults returns on the nil-pricing
		// guard and this test passes without ever reaching the check it covers.
		pricingManager: newTestPricingManager(t),
	}

	entry := &logstore.Log{
		ID:       "req-no-results",
		Provider: string(schemas.OpenAI),
		Model:    "gpt-4o-mini",
	}
	nonResultsResult := &schemas.BifrostResponse{
		ListModelsResponse: &schemas.BifrostListModelsResponse{},
	}
	// accountBatchResults with no BatchResultsResponse should be a no-op
	plugin.accountBatchResults(entry, nonResultsResult, nil)

	// Verify no batch_jobs rows were created
	jobs, err := bs.ListDueProviderJobs(context.Background(), cstables.ProviderJobKindBatch, "openai", time.Now().UTC().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("ListDueProviderJobs() error = %v", err)
	}
	if len(jobs) > 0 {
		t.Fatalf("expected no batch jobs from non-batch-response, got %d", len(jobs))
	}
}

func TestAccountBatchResults_EmptyResultsMarksUnpriceable(t *testing.T) {
	store := newTestStore(t)
	bs := newFakeBatchStore()
	plugin := &LoggerPlugin{
		ctx:            context.Background(),
		store:          store,
		batchStore:     bs,
		logger:         testLogger{},
		pricingManager: newTestPricingManager(t),
	}

	entry := &logstore.Log{
		ID:       "req-empty-batch-results",
		Provider: string(schemas.OpenAI),
		Model:    "gpt-4o-mini",
	}
	result := &schemas.BifrostResponse{
		BatchResultsResponse: &schemas.BifrostBatchResultsResponse{
			BatchID: "batch-empty-results",
			Results: nil,
		},
	}

	plugin.accountBatchResults(entry, result, nil)

	job, err := bs.GetProviderJob(context.Background(), cstables.ProviderJobID(cstables.ProviderJobKindBatch, "openai", "batch-empty-results"))
	if err != nil {
		t.Fatalf("GetProviderJob() error = %v", err)
	}
	if job.AccountingStatus != cstables.ProviderJobAccountingStatusUnpriceable {
		t.Fatalf("expected accounting_status %s, got %s", cstables.ProviderJobAccountingStatusUnpriceable, job.AccountingStatus)
	}
	if job.UnpriceableReason == nil || *job.UnpriceableReason != jobaccounting.UnpriceableReasonNoResults {
		t.Fatalf("expected unpriceable_reason %s, got %#v", jobaccounting.UnpriceableReasonNoResults, job.UnpriceableReason)
	}
}

func TestAccountBatchResults_RepeatedFetchDisplaysPriceWithoutBilling(t *testing.T) {
	store := newTestStore(t)
	bs := newFakeBatchStore()
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, bs, newTestPricingManager(t))
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	newResult := func() *schemas.BifrostResponse {
		return &schemas.BifrostResponse{
			BatchResultsResponse: &schemas.BifrostBatchResultsResponse{
				BatchID: "batch-repeat-fetch",
				Results: []schemas.BatchResultItem{{
					CustomID: "custom-1",
					Response: &schemas.BatchResultResponse{
						StatusCode: 200,
						Body: map[string]interface{}{
							"model": "gpt-4o",
							"usage": map[string]interface{}{
								"prompt_tokens":     100,
								"completion_tokens": 50,
								"total_tokens":      150,
							},
						},
					},
				}},
			},
		}
	}

	first := &logstore.Log{ID: "req-fetch-1", Provider: string(schemas.OpenAI), Model: "gpt-4o"}
	plugin.accountBatchResults(first, newResult(), nil)

	if first.BatchDebugParsed == nil || first.BatchDebugParsed.Accounting == nil || first.BatchDebugParsed.Accounting.Cost == nil {
		t.Fatalf("expected first call's own row to show the settled price, got %#v", first.BatchDebugParsed)
	}
	wantCost := *first.BatchDebugParsed.Accounting.Cost
	if wantCost <= 0 {
		t.Fatalf("expected a positive settled cost, got %v", wantCost)
	}
	if first.Cost != nil {
		t.Fatalf("entry.Cost must stay nil on a batch_results row, got %v", *first.Cost)
	}
	if first.BatchDebugParsed.Status != string(schemas.BatchStatusCompleted) {
		t.Fatalf("expected status %q, got %q", schemas.BatchStatusCompleted, first.BatchDebugParsed.Status)
	}

	second := &logstore.Log{ID: "req-fetch-2", Provider: string(schemas.OpenAI), Model: "gpt-4o"}
	plugin.accountBatchResults(second, newResult(), nil)

	if second.BatchDebugParsed == nil || second.BatchDebugParsed.Accounting == nil || second.BatchDebugParsed.Accounting.Cost == nil {
		t.Fatalf("expected the repeat fetch's own row to also show the settled price, got %#v", second.BatchDebugParsed)
	}
	if diff := *second.BatchDebugParsed.Accounting.Cost - wantCost; diff < -1e-12 || diff > 1e-12 {
		t.Fatalf("repeat fetch price %v does not match originally settled price %v", *second.BatchDebugParsed.Accounting.Cost, wantCost)
	}
	if second.BatchDebugParsed.Accounting.Incomplete {
		t.Fatal("repeat fetch must not show incomplete once the batch fully priced")
	}
	if second.Cost != nil {
		t.Fatalf("entry.Cost must stay nil on the repeat fetch's row too, got %v", *second.Cost)
	}
	if second.BatchDebugParsed.Status != string(schemas.BatchStatusCompleted) {
		t.Fatalf("expected mirrored status %q, got %q", schemas.BatchStatusCompleted, second.BatchDebugParsed.Status)
	}

	logs, err := store.SearchLogs(context.Background(), logstore.SearchFilters{}, logstore.PaginationOptions{Limit: 10})
	if err != nil {
		t.Fatalf("SearchLogs() error = %v", err)
	}
	aggregateRows := 0
	for _, l := range logs.Logs {
		if l.ID == jobaccounting.AccountingLogID(jobaccounting.ProviderJobKindBatch, schemas.OpenAI, "batch-repeat-fetch") {
			aggregateRows++
		}
	}
	if aggregateRows != 1 {
		t.Fatalf("expected exactly 1 aggregate cost row, got %d", aggregateRows)
	}
}

// noopBatchFetcher satisfies jobaccounting.BatchResultFetcher without touching a
// provider: the point of the test below is that it is never reached.
type noopBatchFetcher struct{}

func (noopBatchFetcher) RetrieveBatch(context.Context, *cstables.TableProviderJob) (*schemas.BifrostBatchRetrieveResponse, error) {
	return nil, nil
}

func (noopBatchFetcher) FetchBatchResults(context.Context, *cstables.TableProviderJob) (*schemas.BifrostBatchResultsResponse, error) {
	return nil, nil
}

// A plugin reload can call the wiring against an instance that is already shutting
// down. Cleanup sets the shutdown flag and cancels the sweeper under p.mu before it
// reaches p.wg.Wait(), so a start arriving afterwards must decline rather than run
// wg.Add concurrently with that Wait — which is a WaitGroup contract violation and
// would leave a sweeper writing through a closed plugin.
func TestStartBatchAccountingSweeperAfterCleanupIsRefused(t *testing.T) {
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, newTestStore(t), newFakeBatchStore(), newTestPricingManager(t))
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := plugin.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	cancel := plugin.StartBatchAccountingSweeper(noopBatchFetcher{}, time.Minute, nil)
	if cancel == nil {
		t.Fatal("StartBatchAccountingSweeper must always return a cancel func")
	}
	cancel() // must be safe to call on the refused path

	plugin.mu.Lock()
	registered := plugin.batchSweeperCancel
	plugin.mu.Unlock()
	if registered != nil {
		t.Fatal("no sweeper should be registered after Cleanup")
	}
}

// The batch job row is the only durable record of who to bill: settlement can run
// hours later from a sweeper with no request context. A virtual key alone is not
// enough — an access profile shares one across users — so the user, team, customer
// and the creating log row must all be captured at create time.
func TestRecordBatchJobLifecycle_CreatePersistsRequesterIdentity(t *testing.T) {
	store := newTestStore(t)
	bs := newFakeBatchStore()
	plugin := &LoggerPlugin{
		ctx:        context.Background(),
		store:      store,
		batchStore: bs,
		logger:     testLogger{},
	}

	vkID := "vk-access-profile"
	userID := "user-alice"
	teamID := "team-1"
	customerID := "customer-1"
	entry := &logstore.Log{
		ID:                 "req-create-batch-identity",
		Provider:           string(schemas.OpenAI),
		Model:              "gpt-4o",
		SelectedKeyID:      "key-1",
		VirtualKeyID:       &vkID,
		UserID:             &userID,
		TeamID:             &teamID,
		CustomerID:         &customerID,
		BudgetIDsParsed:    []string{"budget-1"},
		RateLimitIDsParsed: []string{"rl-1"},
	}
	plugin.recordBatchJobLifecycle(entry, &schemas.BifrostResponse{
		BatchCreateResponse: &schemas.BifrostBatchCreateResponse{
			ID:     "batch-identity-123",
			Status: schemas.BatchStatusValidating,
		},
	})

	job, err := bs.GetProviderJob(context.Background(), cstables.ProviderJobID(cstables.ProviderJobKindBatch, "openai", "batch-identity-123"))
	if err != nil {
		t.Fatalf("GetProviderJob() error = %v", err)
	}
	if job.UserID == nil || *job.UserID != userID {
		t.Fatalf("expected user_id %s, got %v", userID, job.UserID)
	}
	if job.TeamID == nil || *job.TeamID != teamID {
		t.Fatalf("expected team_id %s, got %v", teamID, job.TeamID)
	}
	if job.CustomerID == nil || *job.CustomerID != customerID {
		t.Fatalf("expected customer_id %s, got %v", customerID, job.CustomerID)
	}
	if job.SourceLogID == nil || *job.SourceLogID != entry.ID {
		t.Fatalf("expected source_log_id %s, got %v", entry.ID, job.SourceLogID)
	}
}

// newVideoLifecyclePlugin is the minimal plugin a video lifecycle record needs.
func newVideoLifecyclePlugin(t *testing.T) (*LoggerPlugin, *fakeBatchStore) {
	t.Helper()
	bs := newFakeBatchStore()
	return &LoggerPlugin{
		ctx:        context.Background(),
		store:      newTestStore(t),
		batchStore: bs,
		logger:     testLogger{},
	}, bs
}

// The request is the only witness to duration and resolution for most providers,
// and it is in hand exactly once — here. If it is not written down now, settlement
// minutes later has nothing to price from.
func TestRecordVideoJobLifecycle_CapturesRequestPricingDimensions(t *testing.T) {
	plugin, bs := newVideoLifecyclePlugin(t)

	audio := true
	eightSeconds := "8"
	entry := &logstore.Log{
		ID:            "req-video-1",
		Provider:      string(schemas.OpenAI),
		Model:         "sora-2-pro",
		SelectedKeyID: "key-abc",
		ParamsParsed: &schemas.VideoGenerationParameters{
			Seconds: &eightSeconds,
			Size:    "1920x1080",
			Audio:   &audio,
		},
	}
	result := &schemas.BifrostResponse{
		VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{
			ID:     "vid_abc",
			Status: schemas.VideoStatusQueued,
		},
	}
	plugin.recordVideoJobLifecycle(entry, result, schemas.VideoGenerationRequest)

	job, err := bs.GetProviderJob(context.Background(), cstables.ProviderJobID(cstables.ProviderJobKindVideo, "openai", "vid_abc"))
	if err != nil {
		t.Fatalf("expected a video job row: %v", err)
	}
	if job.Kind != cstables.ProviderJobKindVideo {
		t.Fatalf("kind = %q, want %q", job.Kind, cstables.ProviderJobKindVideo)
	}
	if job.Model != "sora-2-pro" {
		t.Fatalf("model = %q, want sora-2-pro", job.Model)
	}
	if job.NextCheckAt == nil {
		t.Fatal("a queued video must be scheduled for a poll")
	}
	// The sweeper pins its poll to this key. An OpenAI video id is visible only to
	// the key that created it, so a row without one can only be polled by whatever
	// key core happens to pick — which will be told the video does not exist.
	if job.SelectedKeyID != "key-abc" {
		t.Fatalf("selected key id = %q, want key-abc", job.SelectedKeyID)
	}

	dims := jobaccounting.VideoDimensionsFromJob(job)
	if dims.Seconds == nil || *dims.Seconds != 8 {
		t.Fatalf("seconds = %v, want 8", dims.Seconds)
	}
	if dims.Size != "1920x1080" {
		t.Fatalf("size = %q, want 1920x1080", dims.Size)
	}
	if dims.Audio == nil || !*dims.Audio {
		t.Fatalf("audio = %v, want true", dims.Audio)
	}
	if dims.RequestType != schemas.VideoGenerationRequest {
		t.Fatalf("request type = %q, want %q", dims.RequestType, schemas.VideoGenerationRequest)
	}
}

// A video submitted before this build was already charged at submission. Creating
// its row on a later retrieve would hand it to the sweeper and charge it twice.
func TestRecordVideoJobLifecycle_RetrieveDoesNotCreateRowForPreUpgradeJob(t *testing.T) {
	plugin, bs := newVideoLifecyclePlugin(t)

	entry := &logstore.Log{ID: "req-video-retrieve", Provider: string(schemas.OpenAI), Model: "sora-2-pro"}
	result := &schemas.BifrostResponse{
		VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{
			ID:     "vid_pre_upgrade",
			Status: schemas.VideoStatusCompleted,
		},
	}
	plugin.recordVideoJobLifecycle(entry, result, schemas.VideoRetrieveRequest)

	if _, err := bs.GetProviderJob(context.Background(), cstables.ProviderJobID(cstables.ProviderJobKindVideo, "openai", "vid_pre_upgrade")); err == nil {
		t.Fatal("a retrieve must never create a job row we did not see submitted")
	}
}

// A retrieve on a job we did submit must still advance it, without losing the
// dimensions the submission captured.
func TestRecordVideoJobLifecycle_RetrieveAdvancesKnownJob(t *testing.T) {
	plugin, bs := newVideoLifecyclePlugin(t)

	eightSeconds := "8"
	entry := &logstore.Log{
		ID:           "req-video-2",
		Provider:     string(schemas.OpenAI),
		Model:        "sora-2-pro",
		ParamsParsed: &schemas.VideoGenerationParameters{Seconds: &eightSeconds, Size: "1280x720"},
	}
	submitted := &schemas.BifrostResponse{
		VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{ID: "vid_known", Status: schemas.VideoStatusQueued},
	}
	plugin.recordVideoJobLifecycle(entry, submitted, schemas.VideoGenerationRequest)

	retrieved := &schemas.BifrostResponse{
		VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{ID: "vid_known", Status: schemas.VideoStatusCompleted},
	}
	plugin.recordVideoJobLifecycle(entry, retrieved, schemas.VideoRetrieveRequest)

	job, err := bs.GetProviderJob(context.Background(), cstables.ProviderJobID(cstables.ProviderJobKindVideo, "openai", "vid_known"))
	if err != nil {
		t.Fatalf("expected the submitted job row: %v", err)
	}
	if job.ProviderStatus != string(schemas.VideoStatusCompleted) {
		t.Fatalf("provider status = %q, want completed", job.ProviderStatus)
	}

	dims := jobaccounting.VideoDimensionsFromJob(job)
	if dims.Seconds == nil || *dims.Seconds != 8 {
		t.Fatalf("the submission's dimensions must survive a retrieve that reports none: %v", dims.Seconds)
	}
	if dims.Size != "1280x720" {
		t.Fatalf("size = %q, want 1280x720", dims.Size)
	}
}

// The retrieve path settles inline off the response the caller already fetched,
// instead of leaving the sweeper to fetch it a second time up to a sweep interval
// later. The submission is recorded first, then the terminal poll settles it.
func TestAccountVideoResults_SettlesTheJobTheSubmissionRecorded(t *testing.T) {
	store := newTestStore(t)
	bs := newFakeBatchStore()
	plugin := &LoggerPlugin{
		ctx:            context.Background(),
		store:          store,
		batchStore:     bs,
		logger:         testLogger{},
		pricingManager: newTestPricingManager(t),
	}

	eightSeconds := "8"
	entry := &logstore.Log{
		ID:           "req-video-inline",
		Provider:     string(schemas.OpenAI),
		Model:        "sora-2-pro",
		ParamsParsed: &schemas.VideoGenerationParameters{Seconds: &eightSeconds, Size: "1280x720"},
	}
	submitted := &schemas.BifrostResponse{
		VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{ID: "vid_inline_wired", Status: schemas.VideoStatusQueued},
	}
	plugin.recordVideoJobLifecycle(entry, submitted, schemas.VideoGenerationRequest)

	terminal := &schemas.BifrostResponse{
		VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{
			ID:     "vid_inline_wired",
			Status: schemas.VideoStatusCompleted,
			Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
			Usage:  &schemas.VideoUsage{Cost: &schemas.BifrostCost{TotalCost: 0.75}},
		},
	}
	plugin.recordVideoJobLifecycle(entry, terminal, schemas.VideoRetrieveRequest)
	plugin.accountVideoResults(entry, terminal, nil)

	job, err := bs.GetProviderJob(context.Background(), cstables.ProviderJobID(cstables.ProviderJobKindVideo, "openai", "vid_inline_wired"))
	if err != nil {
		t.Fatalf("GetProviderJob() error = %v", err)
	}
	if job.AccountingStatus != cstables.ProviderJobAccountingStatusAccounted {
		t.Fatalf("accounting_status = %s, want %s", job.AccountingStatus, cstables.ProviderJobAccountingStatusAccounted)
	}
}

// A video submitted before this build was charged at submission; settling it now
// would bill it twice. With no coordination row, the inline path must do nothing.
func TestAccountVideoResults_SkipsAJobWithNoCoordinationRow(t *testing.T) {
	store := newTestStore(t)
	bs := newFakeBatchStore()
	plugin := &LoggerPlugin{
		ctx:            context.Background(),
		store:          store,
		batchStore:     bs,
		logger:         testLogger{},
		pricingManager: newTestPricingManager(t),
	}

	entry := &logstore.Log{ID: "req-video-orphan", Provider: string(schemas.OpenAI), Model: "sora-2-pro"}
	terminal := &schemas.BifrostResponse{
		VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{
			ID:     "vid_orphan",
			Status: schemas.VideoStatusCompleted,
			Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
			Usage:  &schemas.VideoUsage{Cost: &schemas.BifrostCost{TotalCost: 0.75}},
		},
	}
	plugin.accountVideoResults(entry, terminal, nil)

	if _, err := bs.GetProviderJob(context.Background(), cstables.ProviderJobID(cstables.ProviderJobKindVideo, "openai", "vid_orphan")); err == nil {
		t.Fatal("inline settlement must not create a job row for a video we never saw submitted")
	}
}

// The submission row must name the video it started, so the settlement that lands
// minutes later is traceable back to it. No accounting block here — that is what
// distinguishes the settlement's own cost row from this one.
func TestRecordVideoJobLifecycle_MarksTheRequestRowWithTheVideo(t *testing.T) {
	plugin, _ := newVideoLifecyclePlugin(t)

	eightSeconds := "8"
	entry := &logstore.Log{
		ID:           "req-video-debug",
		Provider:     string(schemas.OpenAI),
		Model:        "sora-2-pro",
		ParamsParsed: &schemas.VideoGenerationParameters{Seconds: &eightSeconds, Size: "1280x720"},
	}
	result := &schemas.BifrostResponse{
		VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{
			ID:     "vid_marked",
			Status: schemas.VideoStatusQueued,
		},
	}
	plugin.recordVideoJobLifecycle(entry, result, schemas.VideoGenerationRequest)

	if entry.VideoDebugParsed == nil {
		t.Fatal("the submission row must name the video it started")
	}
	if entry.VideoDebugParsed.VideoID != "vid_marked" {
		t.Fatalf("video id = %q, want vid_marked", entry.VideoDebugParsed.VideoID)
	}
	if entry.VideoDebugParsed.Status != schemas.VideoStatusQueued {
		t.Fatalf("status = %q, want queued", entry.VideoDebugParsed.Status)
	}
	if entry.VideoDebugParsed.Accounting != nil {
		t.Fatal("only the settlement's aggregate cost row carries an accounting block")
	}
}

// newEndedTraceContext builds a context whose tracer already completed the trace,
// the state a late terminal hook observes after the transport flushed the request.
// The root span is started first because a live trace gets its root span from the
// transport before dispatch; ending the trace removes it from the tracer's store.
func newTracedContext(t *testing.T) (*schemas.BifrostContext, *tracing.Tracer, string) {
	t.Helper()
	logger := testLogger{}
	tracer := tracing.NewTracer(tracing.NewTraceStore(time.Minute, logger), nil, logger)
	traceID := tracer.CreateTrace("")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyTracer, tracer)
	ctx.SetValue(schemas.BifrostContextKeyTraceID, traceID)
	_, root := tracer.StartSpan(ctx, "http.request", schemas.SpanKindInternal)
	if root == nil {
		t.Fatal("root span was not started")
	}
	tracer.EndSpan(root, schemas.SpanStatusOk, "")
	return ctx, tracer, traceID
}

// TestStoreOrEnqueueAfterInjectStillWrites pins the second drop behind maximhq/bifrost#6972.
// When a client disconnects on a non-streaming request, the transport completes and
// flushes the trace (EndTrace, then Inject) as soon as it has written the 499, while
// the worker is still tearing down the upstream call. The worker's abandoned-billing
// terminal hook then reaches storeOrEnqueueEntry AFTER the trace ended. Parking the
// entry under the trace id would leave it for nothing but the TTL sweeper, so the
// request is billed by governance but never gets a log row. The check is against the
// tracer, not a timer, so it holds however late the hook lands.
func TestStoreOrEnqueueAfterInjectStillWrites(t *testing.T) {
	plugin := &LoggerPlugin{
		logger:     testLogger{},
		writeQueue: make(chan *writeQueueEntry, 10),
	}
	ctx, tracer, traceID := newTracedContext(t)

	// The transport flushed the trace with nothing parked yet.
	completed := tracer.EndTrace(traceID)
	if completed == nil {
		t.Fatal("EndTrace returned nil")
	}
	if err := plugin.Inject(context.Background(), completed); err != nil {
		t.Fatalf("Inject() error = %v", err)
	}

	// The worker's late terminal hook lands afterwards.
	plugin.storeOrEnqueueEntry(ctx, &logstore.Log{ID: "req-abandoned", Model: "gpt-4o-mini"}, nil)

	if got := len(plugin.writeQueue); got != 1 {
		t.Fatalf("expected the late entry to be enqueued for writing, writeQueue has %d entries", got)
	}
	if _, parked := plugin.pendingLogsToInject.Load(traceID); parked {
		t.Fatal("late entry was parked under an already-ended trace; nothing will ever drain it")
	}
}

// TestStoreOrEnqueueBeforeInjectStillParks guards the fix above against over-correcting:
// an entry stored while the trace is live must still be parked so Inject can attach
// plugin logs and the trace's authoritative latency before it is written.
func TestStoreOrEnqueueBeforeInjectStillParks(t *testing.T) {
	plugin := &LoggerPlugin{
		logger:     testLogger{},
		writeQueue: make(chan *writeQueueEntry, 10),
	}
	ctx, tracer, traceID := newTracedContext(t)

	plugin.storeOrEnqueueEntry(ctx, &logstore.Log{ID: "req-normal", Model: "gpt-4o-mini"}, nil)
	if got := len(plugin.writeQueue); got != 0 {
		t.Fatalf("entry stored while the trace is live must be parked, but writeQueue has %d entries", got)
	}
	completed := tracer.EndTrace(traceID)
	if completed == nil {
		t.Fatal("EndTrace returned nil")
	}
	if err := plugin.Inject(context.Background(), completed); err != nil {
		t.Fatalf("Inject() error = %v", err)
	}
	if got := len(plugin.writeQueue); got != 1 {
		t.Fatalf("Inject must drain the parked entry, writeQueue has %d entries", got)
	}
}

// The settlement span carries the parsed breakdown when the row has one, not just
// the flat total. Only the entry.Cost fallback was covered, so this branch could
// lose the per-category split while the shared CostAttributes tests stayed green.
func TestEmitAggregateLogEmitsSettlementCostBreakdown(t *testing.T) {
	logger := testLogger{}
	tracer := tracing.NewTracer(tracing.NewTraceStore(time.Minute, logger), nil, logger)
	capture := &captureObsPlugin{spans: make(chan map[string]any, 1)}
	tracer.SetObservabilityPlugins([]schemas.ObservabilityPlugin{capture}, nil)

	plugin := &LoggerPlugin{ctx: context.Background()}
	plugin.SetSettlementTracer(tracer)

	// Deliberately different from TotalCost so a fallback to entry.Cost is visible.
	flat := 9.99
	entry := &logstore.Log{
		ID:        "batch-cost:openai:settlement-breakdown",
		Timestamp: time.Now().UTC(),
		Object:    string(schemas.BatchResultsRequest),
		Provider:  string(schemas.OpenAI),
		Model:     "gpt-4o-mini",
		Status:    "success",
		Cost:      &flat,
		TokenUsageParsed: &schemas.BifrostLLMUsage{
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
			Cost: &schemas.BifrostCost{
				InputCost: 0.30, OutputCost: 0.50, AdditionalCost: 0.20, TotalCost: 1.00,
			},
		},
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
	}
	plugin.EmitAggregateLog(context.Background(), entry)

	select {
	case attrs := <-capture.spans:
		for key, want := range map[string]float64{
			schemas.AttrUsageCost:             1.00,
			schemas.AttrBifrostCostInput:      0.30,
			schemas.AttrBifrostCostOutput:     0.50,
			schemas.AttrBifrostCostAdditional: 0.20,
		} {
			got, ok := attrs[key].(float64)
			if !ok {
				t.Errorf("%s missing from the settlement span", key)
				continue
			}
			if got != want {
				t.Errorf("%s = %v, want %v", key, got, want)
			}
		}
		// The breakdown must win; falling back to entry.Cost would show 9.99.
		if got, _ := attrs[schemas.AttrUsageCost].(float64); got == flat {
			t.Errorf("%s = %v: fell back to entry.Cost instead of the parsed breakdown", schemas.AttrUsageCost, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no settlement span injected to the observability connector")
	}
}

// TestRefusalLogOutcome reproduces a synthetic zero-output refusal without changing inference results.
func TestRefusalLogOutcome(t *testing.T) {
	for _, reason := range []string{"refusal", "content_filter", "safety", "stop", "length", "tool_calls", "future_stop"} {
		for _, streamed := range []bool{false, true} {
			for _, content := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/content=%t", reason, streamed, content), func(t *testing.T) {
					p := &LoggerPlugin{}
					entry := &logstore.Log{Status: logStatusSuccess}
					result := &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{StopReason: schemas.Ptr(reason)}}
					if streamed {
						p.applyStreamingOutputToEntry(entry, &streaming.ProcessedStreamResponse{Data: &streaming.AccumulatedData{FinishReason: schemas.Ptr(reason), Cost: schemas.Ptr(0.27)}}, false, content)
						require.Equal(t, 0.27, *entry.Cost)
					} else {
						p.applyNonStreamingOutputToEntry(entry, result, false, content)
					}
					require.Equal(t, reason, *entry.StopReason)
					require.Equal(t, reason, *result.ResponsesResponse.StopReason)
					switch reason {
					case "refusal", "content_filter", "safety":
						require.Equal(t, logStatusError, entry.Status)
						require.NotNil(t, entry.ErrorDetailsParsed)
						require.Equal(t, "model_refusal", *entry.ErrorDetailsParsed.Error.Type)
						require.Contains(t, entry.ErrorDetailsParsed.Error.Message, reason)
						require.Nil(t, entry.ErrorDetailsParsed.StatusCode)
						require.NoError(t, entry.SerializeFields())
						require.Contains(t, entry.ErrorDetails, "model_refusal")
					default:
						require.Equal(t, logStatusSuccess, entry.Status)
						require.Nil(t, entry.ErrorDetailsParsed)
					}
				})
			}
		}
	}
}

func clientIPTestContext(requestID string) *schemas.BifrostContext {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestID, requestID)
	ctx.SetValue(schemas.BifrostContextKeyClientIP, "2001:db8::1")
	ctx.SetValue(schemas.BifrostContextKeyUserID, "user-1")
	ctx.SetValue(schemas.BifrostContextKeyUserEmail, "ada@example.com")
	return ctx
}

func clientIPTestError() *schemas.BifrostError {
	statusCode := 500
	return &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     &statusCode,
		Error:          &schemas.ErrorField{Message: "provider failed"},
		ExtraFields:    schemas.BifrostErrorExtraFields{RequestType: schemas.ChatCompletionRequest, Provider: schemas.OpenAI, OriginalModelRequested: "gpt-4o"},
	}
}

func requireClientIP(t *testing.T, clientIP *string) {
	t.Helper()
	require.NotNil(t, clientIP)
	require.Equal(t, "2001:db8::1", *clientIP)
}

// TestLLMLogsPersistClientIP drives PreLLMHook/PostLLMHook and the no-pending error path; both rows carry the caller IP.
func TestLLMLogsPersistClientIP(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{CaptureClientIP: true}, testLogger{}, store, nil, nil)
	require.NoError(t, err)

	ctx := clientIPTestContext("req-client-ip")
	_, _, err = plugin.PreLLMHook(ctx, &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "gpt-4o", Params: &schemas.ChatParameters{}},
	})
	require.NoError(t, err)
	_, _, err = plugin.PostLLMHook(ctx, nil, clientIPTestError())
	require.NoError(t, err)

	_, _, err = plugin.PostLLMHook(clientIPTestContext("req-client-ip-no-pending"), nil, clientIPTestError())
	require.NoError(t, err)
	require.NoError(t, plugin.Cleanup())

	for _, id := range []string{"req-client-ip", "req-client-ip-no-pending"} {
		entry, err := store.FindByID(context.Background(), id)
		require.NoError(t, err, id)
		requireClientIP(t, entry.ClientIP)
	}
	entry, err := store.FindByID(context.Background(), "req-client-ip")
	require.NoError(t, err)
	require.NotNil(t, entry.UserEmail, "the completed LLM row records the user email")
	require.Equal(t, "ada@example.com", *entry.UserEmail)
}

// TestClientIPFromContextClampsToColumns keeps an oversized forwarded header, which is caller-supplied, from failing the insert.
func TestClientIPFromContextClampsToColumns(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	plugin := &LoggerPlugin{captureClientIP: true}
	require.Empty(t, plugin.clientIPFromContext(ctx))
	require.Nil(t, optionalString(plugin.clientIPFromContext(ctx)), "an absent IP stays NULL")
	ctx.SetValue(schemas.BifrostContextKeyClientIP, strings.Repeat("9", maxPersistedClientIPLen+10))
	require.Len(t, plugin.clientIPFromContext(ctx), maxPersistedClientIPLen)
}

// streamAccount serves one provider whose upstream is a test server.
type streamAccount struct {
	provider schemas.ModelProvider
	config   schemas.ProviderConfig
}

func (a *streamAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{a.provider}, nil
}

func (a *streamAccount) GetKeysForProvider(context.Context, schemas.ModelProvider) ([]schemas.Key, error) {
	return []schemas.Key{{ID: "k1", Value: *schemas.NewSecretVar("mock"), Models: schemas.WhiteList{"*"}, Weight: 100}}, nil
}

func (a *streamAccount) GetConfigForProvider(provider schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	if provider != a.provider {
		return nil, fmt.Errorf("provider %s not configured", provider)
	}
	config := a.config
	return &config, nil
}

// failedUsageRecorder keeps the billed usage of every stream failure its post-hook sees.
type failedUsageRecorder struct {
	usage chan *schemas.BifrostLLMUsage
}

func (r *failedUsageRecorder) GetName() string { return "failed-usage-recorder" }

func (r *failedUsageRecorder) Cleanup() error { return nil }

func (r *failedUsageRecorder) PreRequestHook(*schemas.BifrostContext, *schemas.BifrostRequest) error {
	return nil
}

func (r *failedUsageRecorder) PreLLMHook(_ *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	return req, nil, nil
}

func (r *failedUsageRecorder) PostLLMHook(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	if err != nil && err.Error != nil {
		select {
		case r.usage <- err.ExtraFields.BilledUsage.DeepCopy():
		default:
		}
	}
	return resp, err, nil
}

// stallingUpstream writes events, flushes, and holds the stream open until the client leaves.
func stallingUpstream(t *testing.T, events ...string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// net/http notices a client hang-up only once the request body is consumed.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, e := range events {
			fmt.Fprint(w, e+"\n\n")
		}
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

const openAIStallingChunk = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"o"}}]}`

// newEstimatingClient runs Bifrost with the real tracer and its stream accumulator,
// fed by the logging plugin as in production, plus a recorder of failed-stream usage.
func newEstimatingClient(t *testing.T, provider schemas.ModelProvider, baseURL string, custom bool, idleTimeout int) (*bifrost.Bifrost, *failedUsageRecorder) {
	t.Helper()
	logger := testLogger{}
	tracer := tracing.NewTracer(tracing.NewTraceStore(time.Minute, logger), nil, logger)
	t.Cleanup(tracer.Stop)
	plugin, err := Init(context.Background(), &Config{}, logger, newTestStore(t), nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = plugin.Cleanup() })
	account := &streamAccount{provider: provider, config: schemas.ProviderConfig{
		NetworkConfig:            schemas.NetworkConfig{BaseURL: baseURL, DefaultRequestTimeoutInSeconds: 30, StreamIdleTimeoutInSeconds: idleTimeout},
		ConcurrencyAndBufferSize: schemas.ConcurrencyAndBufferSize{Concurrency: 1, BufferSize: 16},
	}}
	if custom {
		account.config.CustomProviderConfig = &schemas.CustomProviderConfig{BaseProviderType: schemas.OpenAI}
	}
	recorder := &failedUsageRecorder{usage: make(chan *schemas.BifrostLLMUsage, 4)}
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: account, Logger: logger, Tracer: tracer, LLMPlugins: []schemas.LLMPlugin{plugin, recorder},
	})
	require.NoError(t, err)
	t.Cleanup(client.Shutdown)
	return client, recorder
}

func hiRequest(provider schemas.ModelProvider, model string) *schemas.BifrostChatRequest {
	return &schemas.BifrostChatRequest{Provider: provider, Model: model, Params: &schemas.ChatParameters{},
		Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hi")}}}}
}

func awaitFailedUsage(t *testing.T, recorder *failedUsageRecorder, wait time.Duration) *schemas.BifrostLLMUsage {
	t.Helper()
	select {
	case usage := <-recorder.usage:
		return usage
	case <-time.After(wait):
		t.Fatal("the stream failure never reached the post-hooks")
		return nil
	}
}

// A cancelled stream is billed from what the real accumulator received before the
// cancel: the worker estimates it ahead of every post-hook.
func TestCancelledStreamEstimatedFromRealAccumulator(t *testing.T) {
	const provider schemas.ModelProvider = "cancel-estimate"
	client, recorder := newEstimatingClient(t, provider, stallingUpstream(t, openAIStallingChunk), true, 0)
	ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
	defer cancel()
	stream, streamErr := client.ChatCompletionStreamRequest(ctx, hiRequest(provider, "m"))
	require.Nil(t, streamErr)
	for chunk := range stream {
		if chunk.BifrostError == nil {
			cancel()
		}
	}
	usage := awaitFailedUsage(t, recorder, 3*time.Second)
	require.NotNil(t, usage, "cancelled stream carries no billed usage")
	require.Equal(t, 8, usage.PromptTokens, "cl100k_base fallback prompt count")
	require.Equal(t, 1, usage.CompletionTokens)
	require.Equal(t, 9, usage.TotalTokens)
	require.NotNil(t, usage.Estimation)
}

// Claude's message_start output count is provisional, so a stream cancelled after
// more output is re-counted from the accumulated text, on chat and responses.
func TestAnthropicCancelledStreamEstimatedFromRealAccumulator(t *testing.T) {
	output := strings.Repeat("hello world ", 1000)
	for _, responses := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "responses"}[responses], func(t *testing.T) {
			url := stallingUpstream(t,
				"event: message_start\n"+`data: {"type":"message_start","message":{"id":"msg_cancel","type":"message","role":"assistant","model":"claude-3-haiku","content":[],"usage":{"input_tokens":26,"output_tokens":4}}}`,
				"event: content_block_start\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				"event: content_block_delta\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+output+`"}}`)
			client, recorder := newEstimatingClient(t, schemas.Anthropic, url, false, 0)
			ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
			defer cancel()
			request := hiRequest(schemas.Anthropic, "claude-3-haiku")
			var stream chan *schemas.BifrostStreamChunk
			var streamErr *schemas.BifrostError
			if responses {
				stream, streamErr = client.ResponsesStreamRequest(ctx, request.ToResponsesRequest())
			} else {
				stream, streamErr = client.ChatCompletionStreamRequest(ctx, request)
			}
			require.Nil(t, streamErr)
			seen := 0
			for chunk := range stream {
				if chunk.BifrostChatResponse != nil {
					for _, c := range chunk.BifrostChatResponse.Choices {
						if c.ChatStreamResponseChoice != nil && c.Delta != nil && c.Delta.Content != nil {
							seen += len(*c.Delta.Content)
						}
					}
				}
				if c := chunk.BifrostResponsesStreamResponse; c != nil && c.Type == schemas.ResponsesStreamResponseTypeOutputTextDelta && c.Delta != nil {
					seen += len(*c.Delta)
				}
				if seen >= len(output) {
					cancel()
				}
			}
			usage := awaitFailedUsage(t, recorder, 3*time.Second)
			require.NotNil(t, usage)
			require.Equal(t, 26, usage.PromptTokens, "the provider's prompt count is kept")
			require.Greater(t, usage.CompletionTokens, 4, "the provisional output count was not replaced: %+v", usage)
			require.NotNil(t, usage.Estimation)
			require.True(t, usage.Estimation.Completion)
			require.Equal(t, usage.PromptTokens+usage.CompletionTokens, usage.TotalTokens)
		})
	}
}

// The Anthropic handlers' provisional-usage mark belongs to the attempt that set it;
// one left on the context must not mark real provider usage as provisional.
func TestCancelledStreamIgnoresStaleProvisionalUsage(t *testing.T) {
	const provider schemas.ModelProvider = "stale-counter"
	url := stallingUpstream(t, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"o"}}],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`)
	client, recorder := newEstimatingClient(t, provider, url, true, 0)
	ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
	defer cancel()
	ctx.SetValue(schemas.BifrostContextKeyStreamUsageProvisional, true) // left by an earlier Anthropic-handler attempt
	stream, streamErr := client.ChatCompletionStreamRequest(ctx, hiRequest(provider, "m"))
	require.Nil(t, streamErr)
	for chunk := range stream {
		if chunk.BifrostError == nil {
			cancel()
		}
	}
	usage := awaitFailedUsage(t, recorder, 3*time.Second)
	require.NotNil(t, usage)
	require.Nil(t, usage.Estimation, "provider usage replaced by a stale provisional mark: %+v", usage)
	require.Equal(t, 5, usage.PromptTokens)
	require.Equal(t, 7, usage.CompletionTokens)
}

// A stream that produces output and then goes silent fails on the per-chunk idle
// timeout with an untyped error; its received output is still estimated.
func TestIdleTimedOutStreamEstimatedFromRealAccumulator(t *testing.T) {
	const provider schemas.ModelProvider = "idle-estimate"
	client, recorder := newEstimatingClient(t, provider, stallingUpstream(t, openAIStallingChunk), true, 1)
	stream, streamErr := client.ChatCompletionStreamRequest(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), hiRequest(provider, "m"))
	require.Nil(t, streamErr)
	var failure *schemas.BifrostError
	for chunk := range stream {
		if chunk.BifrostError != nil {
			failure = chunk.BifrostError
		}
	}
	require.NotNil(t, failure)
	require.NotNil(t, failure.Error)
	require.ErrorIs(t, failure.Error.Error, providerUtils.ErrStreamIdleTimeout)
	usage := awaitFailedUsage(t, recorder, 5*time.Second)
	require.NotNil(t, usage)
	require.Equal(t, 8, usage.PromptTokens)
	require.Equal(t, 1, usage.CompletionTokens)
	require.NotNil(t, usage.Estimation)
}

// A speech session knows the text it spoke only when it ends, so it reports it with its final response, and that
// text is the row's input, as the text of a speech request is.
func TestApplySpeechSessionInputRecordsTheTextTheSessionSpoke(t *testing.T) {
	final := &schemas.BifrostResponse{SpeechStreamResponse: &schemas.BifrostSpeechStreamResponse{
		Type:         schemas.SpeechStreamResponseTypeDone,
		SessionInput: "Hello world.",
	}}

	entry := &logstore.Log{}
	applySpeechSessionInput(entry, final, true)
	require.NotNil(t, entry.SpeechInputParsed)
	require.Equal(t, "Hello world.", entry.SpeechInputParsed.Input)

	hidden := &logstore.Log{}
	applySpeechSessionInput(hidden, final, false)
	require.Nil(t, hidden.SpeechInputParsed, "content logging is off")

	silent := &logstore.Log{}
	applySpeechSessionInput(silent, &schemas.BifrostResponse{SpeechStreamResponse: &schemas.BifrostSpeechStreamResponse{}}, true)
	applySpeechSessionInput(silent, &schemas.BifrostResponse{}, true)
	applySpeechSessionInput(silent, nil, true)
	require.Nil(t, silent.SpeechInputParsed, "a response that reports no text leaves the input alone")
}

// A speech stream with no audio has no speech output to keep: a speech session streams its audio to the client and
// keeps none, so its row would otherwise carry an empty output that the logs page shows as a player with nothing to play.
func TestApplyStreamingOutputToEntryKeepsSpeechOutputOnlyWhenThereIsAudio(t *testing.T) {
	plugin := &LoggerPlugin{}

	spoken := &logstore.Log{}
	plugin.applyStreamingOutputToEntry(spoken, &streaming.ProcessedStreamResponse{
		Data: &streaming.AccumulatedData{AudioOutput: &schemas.BifrostSpeechResponse{Audio: []byte("ABC")}},
	}, false, true)
	require.NotNil(t, spoken.SpeechOutputParsed)
	require.Equal(t, []byte("ABC"), spoken.SpeechOutputParsed.Audio)

	silent := &logstore.Log{}
	plugin.applyStreamingOutputToEntry(silent, &streaming.ProcessedStreamResponse{
		Data: &streaming.AccumulatedData{AudioOutput: &schemas.BifrostSpeechResponse{}},
	}, false, true)
	require.Nil(t, silent.SpeechOutputParsed, "no audio, nothing to keep")
}

// A speech session is a socket that stays open as long as its client likes, which an HTTP speech stream is not, so its
// row has an object of its own and the logs page can tell the two apart.
func TestPreLLMHookLogsASpeechSessionUnderItsOwnObject(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil)
	require.NoError(t, err)
	req := &schemas.BifrostRequest{RequestType: schemas.SpeechStreamRequest, SpeechRequest: &schemas.BifrostSpeechRequest{
		Provider: schemas.Elevenlabs, Model: "eleven_v4_turbo", Input: &schemas.SpeechInput{},
	}}

	for _, tc := range []struct {
		requestID string
		session   bool
		want      string
	}{
		{"speech-session-1", true, "speech.session"},
		{"speech-http-stream-1", false, "speech_stream"},
	} {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		ctx.SetValue(schemas.BifrostContextKeyRequestID, tc.requestID)
		if tc.session {
			ctx.SetValue(schemas.BifrostContextKeySpeechSession, true)
		}
		_, _, err := plugin.PreLLMHook(ctx, req)
		require.NoError(t, err)
		pending, ok := plugin.pendingLogsEntries.Load(tc.requestID)
		require.True(t, ok, tc.requestID)
		require.Equal(t, tc.want, pending.(*PendingLogData).InitialData.Object, tc.requestID)
	}
	require.NoError(t, plugin.Cleanup())
}

// BenchmarkSpeechSessionContextLookup is the only work the session's own object adds to the request path: one
// context read for a speech stream, and none for any other request type.
func BenchmarkSpeechSessionContextLookup(b *testing.B) {
	for _, tc := range []struct {
		name    string
		session bool
	}{{"session", true}, {"http stream", false}} {
		b.Run(tc.name, func(b *testing.B) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-1")
			ctx.SetValue(schemas.BifrostContextKeySelectedKeyID, "key-1")
			if tc.session {
				ctx.SetValue(schemas.BifrostContextKeySpeechSession, true)
			}
			b.ReportAllocs()
			for b.Loop() {
				if speechSession, _ := ctx.Value(schemas.BifrostContextKeySpeechSession).(bool); speechSession != tc.session {
					b.Fatal("wrong object")
				}
			}
		})
	}
}

// TestKeyTurningContentOnOverClientFlagKeepsEarlyCapturedContent pins the other direction of the
// hook ordering hole: with the client flag off, a key that turns content on is still unstamped when
// PreLLMHook and PreMCPHook capture, so they must capture provisionally and let the final policy in
// the post-hooks decide, instead of dropping content the key is entitled to keep.
func TestKeyTurningContentOnOverClientFlagKeepsEarlyCapturedContent(t *testing.T) {
	newCtx := func(requestID string) *schemas.BifrostContext {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		ctx.SetValue(schemas.BifrostContextKeyRequestID, requestID)
		ctx.SetValue(schemas.BifrostContextKeyVirtualKey, "sk-bf-presented")
		return ctx
	}
	stampKeyOn := func(ctx *schemas.BifrostContext) {
		ctx.SetValue(schemas.BifrostContextKeyGovernanceVirtualKeyID, "vk-1")
		ctx.SetValue(schemas.BifrostContextKeyGovernanceDisableContentLogging, false)
	}

	t.Run("passthrough body", func(t *testing.T) {
		store := newTestStore(t)
		plugin, err := Init(context.Background(), &Config{DisableContentLogging: new(true)}, testLogger{}, store, nil, nil)
		if err != nil {
			t.Fatalf("Init() error = %v", err)
		}
		ctx := newCtx("req-key-on-passthrough")
		_, _, err = plugin.PreLLMHook(ctx, &schemas.BifrostRequest{
			RequestType: schemas.PassthroughRequest,
			PassthroughRequest: &schemas.BifrostPassthroughRequest{
				Provider:    schemas.OpenAI,
				Method:      "POST",
				Path:        "/v1/fine-tuning/jobs",
				Body:        []byte(`{"training_file":"file-kept"}`),
				SafeHeaders: map[string]string{"content-type": "application/json"},
			},
		})
		if err != nil {
			t.Fatalf("PreLLMHook() error = %v", err)
		}
		stampKeyOn(ctx)
		_, _, err = plugin.PostLLMHook(ctx, &schemas.BifrostResponse{
			PassthroughResponse: &schemas.BifrostPassthroughResponse{
				StatusCode:  200,
				Body:        []byte(`{"id":"ftjob-1"}`),
				Path:        "/v1/fine-tuning/jobs",
				ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.PassthroughRequest, Provider: schemas.OpenAI},
			},
		}, nil)
		if err != nil {
			t.Fatalf("PostLLMHook() error = %v", err)
		}
		if err := plugin.Cleanup(); err != nil {
			t.Fatalf("Cleanup() error = %v", err)
		}
		logEntry, err := store.FindByID(context.Background(), "req-key-on-passthrough")
		if err != nil {
			t.Fatalf("FindByID() error = %v", err)
		}
		if logEntry.ContentHidden {
			t.Fatal("a key that turns content on must leave the row visible")
		}
		if logEntry.PassthroughRequestBody != `{"training_file":"file-kept"}` {
			t.Fatalf("expected the passthrough body kept for a content-on key, got %q", logEntry.PassthroughRequestBody)
		}
	})

}

// TestClientIPCaptureOffByDefault leaves client_ip NULL on LLM rows unless logs_store.capture_client_ip is set; user email is still recorded.
func TestClientIPCaptureOffByDefault(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil)
	require.NoError(t, err)

	ctx := clientIPTestContext("req-client-ip-off")
	_, _, err = plugin.PreLLMHook(ctx, &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "gpt-4o", Params: &schemas.ChatParameters{}},
	})
	require.NoError(t, err)
	_, _, err = plugin.PostLLMHook(ctx, nil, clientIPTestError())
	require.NoError(t, err)
	_, _, err = plugin.PostLLMHook(clientIPTestContext("req-client-ip-off-no-pending"), nil, clientIPTestError())
	require.NoError(t, err)

	require.NoError(t, plugin.Cleanup())

	for _, id := range []string{"req-client-ip-off", "req-client-ip-off-no-pending"} {
		entry, err := store.FindByID(context.Background(), id)
		require.NoError(t, err, id)
		require.Nil(t, entry.ClientIP, id)
	}
	entry, err := store.FindByID(context.Background(), "req-client-ip-off")
	require.NoError(t, err)
	require.NotNil(t, entry.UserEmail, "user email does not depend on capture_client_ip")
}

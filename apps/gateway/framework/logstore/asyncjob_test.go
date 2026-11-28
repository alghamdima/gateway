package logstore

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/grant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

type asyncTestLogger struct{}

func (asyncTestLogger) Debug(string, ...any)                   {}
func (asyncTestLogger) Info(string, ...any)                    {}
func (asyncTestLogger) Warn(string, ...any)                    {}
func (asyncTestLogger) Error(string, ...any)                   {}
func (asyncTestLogger) Fatal(string, ...any)                   {}
func (asyncTestLogger) SetLevel(schemas.LogLevel)              {}
func (asyncTestLogger) SetOutputType(schemas.LoggerOutputType) {}
func (asyncTestLogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

type testGovernanceStore struct {
	virtualKeys map[string]*configstoreTables.TableVirtualKey
}

func (t *testGovernanceStore) GetVirtualKey(_ context.Context, vkValue string) (*configstoreTables.TableVirtualKey, bool) {
	vk, ok := t.virtualKeys[vkValue]
	return vk, ok
}

func newTestAsyncExecutor(t *testing.T) *AsyncJobExecutor {
	t.Helper()
	ctx := context.Background()

	// File-backed store: some tests below poll the job row from the test
	// goroutine while executeJob writes from another, and a :memory: DSN
	// gives each pooled connection its own database.
	store, err := newSqliteLogStore(ctx, &SQLiteConfig{
		Path: filepath.Join(t.TempDir(), "asyncjob.db"),
	}, asyncTestLogger{})
	require.NoError(t, err)
	t.Cleanup(func() { store.Close(ctx) })

	govStore := &testGovernanceStore{
		virtualKeys: map[string]*configstoreTables.TableVirtualKey{
			"sk-bf-test": {ID: "vk-123", Value: *schemas.NewSecretVar("sk-bf-test")},
		},
	}

	return NewAsyncJobExecutor(store, govStore, asyncTestLogger{})
}

// waitForJobCompletion polls until the operation callback has been invoked.
func waitForJobCompletion(t *testing.T, done *atomic.Bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if done.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for async job execution")
}

// waitForJobStatus polls FindAsyncJobByID until the job reaches a terminal
// status (completed or failed), or times out. This avoids a fragile time.Sleep
// between the operation callback completing and the DB update finishing.
// Processing is intermediate and must not be treated as terminal.
func waitForJobStatus(t *testing.T, store LogStore, jobID string) *AsyncJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job, err := store.FindAsyncJobByID(context.Background(), jobID)
		if err == nil && (job.Status == schemas.AsyncJobStatusCompleted || job.Status == schemas.AsyncJobStatusFailed) {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for async job to reach terminal status")
	return nil
}

func TestSubmitJob_PropagatesContextValues(t *testing.T) {
	executor := newTestAsyncExecutor(t)

	capturedCtx := schemas.NewBifrostContext(context.Background(), time.Now().Add(1*time.Minute))
	capturedCtx.SetValue(schemas.BifrostContextKeyVirtualKey, "sk-bf-test")
	capturedCtx.SetValue(schemas.BifrostContextKey("x-bf-eh-custom"), "custom-value")
	capturedCtx.SetValue(schemas.BifrostContextKey("x-bf-prom-env"), "production")
	var done atomic.Bool

	operation := func(bgCtx *schemas.BifrostContext) (interface{}, *schemas.BifrostError) {
		capturedCtx = bgCtx
		done.Store(true)
		return map[string]string{"status": "ok"}, nil
	}

	job, err := executor.SubmitJob(capturedCtx, 3600, operation, schemas.ChatCompletionRequest)
	require.NoError(t, err)
	require.NotNil(t, job)

	waitForJobCompletion(t, &done)

	assert.Equal(t, "sk-bf-test", capturedCtx.Value(schemas.BifrostContextKeyVirtualKey))
	assert.Equal(t, "production", capturedCtx.Value(schemas.BifrostContextKey("x-bf-prom-env")))
	assert.Equal(t, "custom-value", capturedCtx.Value(schemas.BifrostContextKey("x-bf-eh-custom")))
	assert.Equal(t, true, capturedCtx.Value(schemas.BifrostIsAsyncRequest))
}

// TestSubmitJob_PropagatesGrant covers the same gap realtime turn contexts already guard against
// (see newRealtimeTurnContext): the background context executeJob builds is new, so a grant
// settled on the submit call's context must be carried over explicitly, not assumed to survive
// GetUserValues — Grant lives on its own field, not the user-values map.
func TestSubmitJob_PropagatesGrant(t *testing.T) {
	executor := newTestAsyncExecutor(t)

	submitCtx := schemas.NewBifrostContext(context.Background(), time.Now().Add(1*time.Minute))
	submitCtx.SetValue(schemas.BifrostContextKeyVirtualKey, "sk-bf-test")
	g := grant.New()
	g.SetIdentity(grant.NewIdentity(grant.NewCredential(grant.CredentialVirtualKey, "sk-bf-test"), nil, nil, nil, nil))
	require.True(t, submitCtx.SetGrant(g))

	var gotGrant schemas.Grant
	var done atomic.Bool

	operation := func(bgCtx *schemas.BifrostContext) (interface{}, *schemas.BifrostError) {
		gotGrant = bgCtx.Grant()
		done.Store(true)
		return map[string]string{"status": "ok"}, nil
	}

	job, err := executor.SubmitJob(submitCtx, 3600, operation, schemas.ChatCompletionRequest)
	require.NoError(t, err)
	require.NotNil(t, job)

	waitForJobCompletion(t, &done)

	require.NotNil(t, gotGrant, "background context lost the grant settled on the submit call's context")
	assert.Same(t, g, gotGrant, "background context must carry the submit call's grant, not a copy")
	assert.Equal(t, "sk-bf-test", gotGrant.Identity().Credential().Value)
}

func TestSubmitJob_StoresRequestID(t *testing.T) {
	// File-backed store: the test polls the job row across goroutines, and a
	// :memory: DSN gives each pooled connection its own database.
	store, err := newSqliteLogStore(context.Background(), &SQLiteConfig{
		Path: filepath.Join(t.TempDir(), "asyncjobs.db"),
	}, asyncTestLogger{})
	require.NoError(t, err)
	t.Cleanup(func() { store.Close(context.Background()) })
	executor := NewAsyncJobExecutor(store, &testGovernanceStore{}, asyncTestLogger{})

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-123")

	job, err := executor.SubmitJob(ctx, 3600, func(*schemas.BifrostContext) (interface{}, *schemas.BifrostError) {
		return map[string]string{"status": "ok"}, nil
	}, schemas.ChatCompletionRequest)
	require.NoError(t, err)
	assert.Equal(t, "req-123", job.RequestID, "the submit call's request id keys the job's LLM log entry")

	stored := waitForJobStatus(t, executor.logstore, job.ID)
	assert.Equal(t, "req-123", stored.RequestID)
}

func TestSubmitJob_NilContextValues(t *testing.T) {
	executor := newTestAsyncExecutor(t)

	var capturedCtx *schemas.BifrostContext
	var done atomic.Bool

	operation := func(bgCtx *schemas.BifrostContext) (interface{}, *schemas.BifrostError) {
		capturedCtx = bgCtx
		done.Store(true)
		return map[string]string{"status": "ok"}, nil
	}

	job, err := executor.SubmitJob(capturedCtx, 3600, operation, schemas.ChatCompletionRequest)
	require.NoError(t, err)
	require.NotNil(t, job)

	waitForJobCompletion(t, &done)

	assert.NotNil(t, capturedCtx)
	assert.Equal(t, true, capturedCtx.Value(schemas.BifrostIsAsyncRequest))
}

func TestSubmitJob_EmptyContextValues(t *testing.T) {
	executor := newTestAsyncExecutor(t)

	var capturedCtx *schemas.BifrostContext
	var done atomic.Bool

	operation := func(bgCtx *schemas.BifrostContext) (interface{}, *schemas.BifrostError) {
		capturedCtx = bgCtx
		done.Store(true)
		return map[string]string{"status": "ok"}, nil
	}

	job, err := executor.SubmitJob(capturedCtx, 3600, operation, schemas.ChatCompletionRequest)
	require.NoError(t, err)
	require.NotNil(t, job)

	waitForJobCompletion(t, &done)

	assert.NotNil(t, capturedCtx)
	assert.Equal(t, true, capturedCtx.Value(schemas.BifrostIsAsyncRequest))
}

func TestSubmitJob_AsyncFlagOverridesContextValues(t *testing.T) {
	executor := newTestAsyncExecutor(t)

	inputCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	inputCtx.SetValue(schemas.BifrostIsAsyncRequest, false)

	var capturedCtx *schemas.BifrostContext
	var done atomic.Bool
	operation := func(bgCtx *schemas.BifrostContext) (interface{}, *schemas.BifrostError) {
		capturedCtx = bgCtx
		done.Store(true)
		return map[string]string{"status": "ok"}, nil
	}

	job, err := executor.SubmitJob(inputCtx, 3600, operation, schemas.ChatCompletionRequest)
	require.NoError(t, err)
	require.NotNil(t, job)

	waitForJobCompletion(t, &done)

	// BifrostIsAsyncRequest must be true — set AFTER restoring context values
	assert.Equal(t, true, capturedCtx.Value(schemas.BifrostIsAsyncRequest))
}

func TestSubmitJob_OperationFailure_PreservesContext(t *testing.T) {
	executor := newTestAsyncExecutor(t)

	inputCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	inputCtx.SetValue(schemas.BifrostContextKeyVirtualKey, "sk-bf-test")

	var capturedCtx *schemas.BifrostContext
	var done atomic.Bool

	statusCode := fasthttp.StatusBadRequest
	operation := func(bgCtx *schemas.BifrostContext) (interface{}, *schemas.BifrostError) {
		capturedCtx = bgCtx
		done.Store(true)
		return nil, &schemas.BifrostError{
			StatusCode: &statusCode,
			Error:      &schemas.ErrorField{Message: "test error"},
		}
	}

	job, err := executor.SubmitJob(inputCtx, 3600, operation, schemas.ChatCompletionRequest)
	require.NoError(t, err)
	require.NotNil(t, job)

	waitForJobCompletion(t, &done)

	// Context values should still be available even when operation fails
	assert.Equal(t, "sk-bf-test", capturedCtx.Value(schemas.BifrostContextKeyVirtualKey))
	assert.Equal(t, true, capturedCtx.Value(schemas.BifrostIsAsyncRequest))

	// Verify job was marked as failed — poll until DB update completes
	retrievedJob := waitForJobStatus(t, executor.logstore, job.ID)
	assert.Equal(t, schemas.AsyncJobStatusFailed, retrievedJob.Status)
}

// TestAsyncPollResultKeepsGoType pins that ToResponse hands Go callers the plain
// response pointer even when usage reported a zero; zeros are restored on marshal.
func TestAsyncPollResultKeepsGoType(t *testing.T) {
	raw := `{"id":"usage-job","usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`
	chat := (&AsyncJob{RequestType: schemas.ChatCompletionRequest, Response: raw}).ToResponse()
	chatResult, ok := chat.Result.(*schemas.BifrostChatResponse)
	require.True(t, ok, "chat Result is %T", chat.Result)
	assert.True(t, chatResult.Usage.PromptTokensExplicitZero)
	text := (&AsyncJob{RequestType: schemas.TextCompletionRequest, Response: raw}).ToResponse()
	_, ok = text.Result.(*schemas.BifrostTextCompletionResponse)
	require.True(t, ok, "text Result is %T", text.Result)
	for _, resp := range []*schemas.AsyncJobResponse{chat, text} {
		wire, err := sonic.ConfigStd.Marshal(resp)
		require.NoError(t, err)
		var poll struct {
			Result struct{ Usage json.RawMessage }
		}
		require.NoError(t, json.Unmarshal(wire, &poll))
		assert.JSONEq(t, `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`, string(poll.Result.Usage))
	}
}

// TestAsyncResultKeepsDefaultEncoding pins that persisted async results keep the
// pre-fix sonic.Marshal bytes; only Chat/Text with an explicit zero are wrapped.
func TestAsyncResultKeepsDefaultEncoding(t *testing.T) {
	const model = "fixture<b>&</b>"
	usage := &schemas.BifrostLLMUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}
	for _, tc := range []struct {
		name string
		resp any
	}{
		{"nonzero chat", &schemas.BifrostChatResponse{ID: "c", Model: model, Usage: usage}},
		{"nonzero text", &schemas.BifrostTextCompletionResponse{ID: "t", Model: model, Usage: usage}},
		{"responses", &schemas.BifrostResponsesResponse{Model: model}},
		{"embedding", &schemas.BifrostEmbeddingResponse{Model: model}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, err := sonic.Marshal(tc.resp)
			require.NoError(t, err)
			got, err := marshalAsyncResponse(tc.resp)
			require.NoError(t, err)
			require.Equal(t, string(want), string(got))
		})
	}
	t.Run("explicit zero chat", func(t *testing.T) {
		zero := &schemas.BifrostLLMUsage{PromptTokensExplicitZero: true, CompletionTokensExplicitZero: true}
		got, err := marshalAsyncResponse(&schemas.BifrostChatResponse{ID: "z", Model: model, Usage: zero})
		require.NoError(t, err)
		require.Contains(t, string(got), `"model":"`+model+`"`, "wrapper keeps default encoding")
		require.Contains(t, string(got), `"prompt_tokens":0`)
	})
}

// Exercise the persisted job and polling response, not just the provider's wire wrapper.
func TestAsyncCompletionPreservesZeroUsage(t *testing.T) {
	executor := newTestAsyncExecutor(t)
	for _, text := range []bool{false, true} {
		for _, usage := range []string{
			`{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`,
			`{"prompt_tokens":12,"completion_tokens":0,"total_tokens":12}`,
			`{"prompt_tokens":0,"total_tokens":12}`, `{"total_tokens":12}`,
			`{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}`,
		} {
			kind := schemas.ChatCompletionRequest
			if text {
				kind = schemas.TextCompletionRequest
			}
			t.Run(string(kind)+"/"+usage, func(t *testing.T) {
				raw := []byte(`{"id":"usage-job","usage":` + usage + `}`)
				chat := &schemas.BifrostChatResponse{}
				require.NoError(t, schemas.Unmarshal(raw, chat))
				chat.Usage.PreserveTokenCountZeros(raw, "usage.prompt_tokens", "usage.completion_tokens")
				var response any = chat
				if text {
					response = chat.ToTextCompletionResponse()
				}
				ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
				job, err := executor.SubmitJob(ctx, 3600, func(*schemas.BifrostContext) (any, *schemas.BifrostError) { return response, nil }, kind)
				require.NoError(t, err)
				job = waitForJobStatus(t, executor.logstore, job.ID)
				require.Equal(t, schemas.AsyncJobStatusCompleted, job.Status)
				var stored struct{ Usage json.RawMessage }
				require.NoError(t, json.Unmarshal([]byte(job.Response), &stored))
				assert.JSONEq(t, usage, string(stored.Usage), "persisted usage")
				// Also seed an already-correct row: fixing only the executor must not hide the polling bug.
				for _, row := range []*AsyncJob{job, {RequestType: kind, Response: string(raw)}} {
					wire, err := schemas.MarshalSorted(row.ToResponse())
					require.NoError(t, err)
					var poll struct {
						Result struct {
							ID    string
							Usage json.RawMessage
						}
					}
					require.NoError(t, json.Unmarshal(wire, &poll))
					assert.Equal(t, "usage-job", poll.Result.ID)
					assert.JSONEq(t, usage, string(poll.Result.Usage), "polled usage")
				}
			})
		}
	}
}

// Run against the current implementation and the pre-fix serialization overlay.
func BenchmarkAsyncCompletionUsage(b *testing.B) {
	for _, tc := range []struct {
		name  string
		usage *schemas.BifrostLLMUsage
	}{
		{"nonzero", &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}},
		{"zero", &schemas.BifrostLLMUsage{PromptTokensExplicitZero: true, CompletionTokensExplicitZero: true}},
		{"missing", &schemas.BifrostLLMUsage{TotalTokens: 12}},
	} {
		response := &schemas.BifrostChatResponse{ID: "bench", Model: "fixture", Usage: tc.usage}
		body, err := schemas.MarshalSorted(response.WithPreservedUsage())
		if err != nil {
			b.Fatal(err)
		}
		job := &AsyncJob{ID: "job-bench", RequestType: schemas.ChatCompletionRequest, Status: schemas.AsyncJobStatusCompleted, Response: string(body)}
		b.Run(tc.name+"/write", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := marshalAsyncResponse(response); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(tc.name+"/poll", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := schemas.MarshalSorted(job.ToResponse()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

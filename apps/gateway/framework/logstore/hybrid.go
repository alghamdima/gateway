package logstore

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/objectstore"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gorm.io/gorm"
)

const (
	defaultUploadWorkers   = 10
	defaultUploadQueueSize = 5000
	// MaxContentSummaryBytes caps the content_summary column, on write and in
	// the list query; a summary that long may have been cut mid-message.
	MaxContentSummaryBytes     = 2048
	defaultMaxUploadQueueBytes = 1 << 30 // 1 GiB
	// has_object flips are batched when the inner store implements
	// hasObjectMarker: a batch is written at markBatchSize rows or every
	// markFlushInterval, whichever comes first.
	markBatchSize     = 512
	markFlushInterval = 100 * time.Millisecond
	markQueueSize     = 4096
	markMaxAttempts   = 3
)

// hasObjectMarker is implemented by inner stores that set has_object on many
// rows at once (ClickHouse, where a per-row update is a full-row re-insert).
type hasObjectMarker interface {
	MarkHasObject(ctx context.Context, table string, marks []HasObjectMark) (HasObjectMarkResult, error)
}

// The hybrid store finds the batched path with a type assertion, so a
// signature drift would silently fall back to per-row flips.
var _ hasObjectMarker = (*ClickHouseLogStore)(nil)

// hasObjectMark is one pending has_object flip: the object at key is written.
type hasObjectMark struct {
	kind      uploadKind
	logID     string
	timestamp time.Time
	key       string
	attempts  int
	retryAt   time.Time
}

type uploadKind uint8

const (
	uploadKindLog uploadKind = iota
	// uploadKindLogMerge overlays the payload onto the object the row already
	// has (BatchUpdate's reconcile of a late completion) instead of replacing it.
	uploadKindLogMerge
)

// uploadWork represents an async S3 upload job.
type uploadWork struct {
	logID     string
	timestamp time.Time
	key       string
	kind      uploadKind
	status    string
	payload   []byte // JSON-encoded payload
	tags      map[string]string
	staleKey  string // object in the other format this upload replaces; deleted once the Put succeeds
}

// HybridLogStore wraps an existing LogStore and offloads large payload
// fields to object storage while keeping a lightweight index in the DB.
//
// Method routing:
//   - Delegated directly (40+ methods): all analytics, search, histogram, ranking,
//     distinct, async job methods
//   - Intercepted: Create, CreateIfNotExists, BatchCreateIfNotExists, FindByID,
//     Update, DeleteLog, DeleteLogs, DeleteLogsBatch, Close
type HybridLogStore struct {
	inner          LogStore
	objects        objectstore.ObjectStore
	prefix         string
	logger         schemas.Logger
	uploadQueues   []chan *uploadWork // one per worker, sharded by log id so a row's uploads run in enqueue order
	wg             sync.WaitGroup
	closed         atomic.Bool
	droppedUploads atomic.Int64
	pendingBytes   atomic.Int64
	// excludedPayloadFields is the set of payload field names (DB column names) that must NOT be offloaded to object storage and must remain in the DB.
	excludedPayloadFields map[string]struct{}
	// excludedRequestTypes is the set of log object types that are never offloaded; their rows stay complete in the DB.
	excludedRequestTypes map[string]struct{}
	// offloadErrorRawBodies: error_details is DB-resident only through databaseResidentPayloadFields, so its raw bodies are offloaded.
	offloadErrorRawBodies bool
	objectExt             string             // suffix of new object keys; selects json or parquet encoding
	parquetCompression    string             // parquet page codec, used when objectExt is parquetObjectExt
	singleFormat          bool               // the backend can only hold json objects, so the other format is never probed or deleted
	marker                hasObjectMarker    // nil: each upload worker flips has_object itself, one row at a time
	markQueue             chan hasObjectMark // pending flips, read by markWorker; nil when marker is nil
	markWG                sync.WaitGroup
}

type scopedDBLogStore interface {
	ScopedDB(context.Context) *gorm.DB
}

// databaseResidentPayloadFields stay in the database whatever the configured
// exclusions say. error_details backs the error_type, error_code and
// status_code rankings and filters, which query the column directly, so
// offloading it left every error breakdown empty. It is not request content,
// except for the provider raw bodies it can carry, which
// moveErrorRawBodiesToObject sends to object storage. Content-hidden rows
// still offload it with everything else.
//
// guardrail_debug backs the guardrail outcome filter (the Guardrails incidents
// list) the same way. It holds sanitized rule metadata - rule, guardrail,
// phase, reason, policy categories - and never matched request text.
//
// This applies to logs written from this change on. Rows written before it
// hold error_details only in their object, so they stay out of the error
// breakdowns: restoring them means one object read per failed log, with no
// bound on the count, and is not done automatically.
var databaseResidentPayloadFields = []string{"error_details", "guardrail_debug"}

// newHybridLogStore creates a HybridLogStore wrapping the given inner store.
// excludeFields lists payload field DB column names that should be kept in the
// database rather than offloaded to object storage, on top of
// databaseResidentPayloadFields. Pass nil to offload every other payload field. excludeRequestTypes lists log
// object types whose rows are written to the DB unchanged and never uploaded.
func newHybridLogStore(inner LogStore, objects objectstore.ObjectStore, prefix string, logger schemas.Logger, excludeFields []string, excludeRequestTypes []string) *HybridLogStore {
	excluded := make(map[string]struct{}, len(excludeFields)+len(databaseResidentPayloadFields))
	for _, f := range databaseResidentPayloadFields {
		excluded[f] = struct{}{}
	}
	offloadErrorRawBodies := true
	for _, f := range excludeFields {
		excluded[f] = struct{}{}
		if f == "error_details" {
			offloadErrorRawBodies = false
		}
	}
	excludedTypes := make(map[string]struct{}, len(excludeRequestTypes))
	for _, t := range excludeRequestTypes {
		if t = strings.TrimSpace(t); t != "" {
			excludedTypes[t] = struct{}{}
		}
	}
	h := &HybridLogStore{
		inner:                 inner,
		objects:               objects,
		prefix:                prefix,
		logger:                logger,
		uploadQueues:          make([]chan *uploadWork, defaultUploadWorkers),
		excludedPayloadFields: excluded,
		excludedRequestTypes:  excludedTypes,
		offloadErrorRawBodies: offloadErrorRawBodies,
		objectExt:             jsonObjectExt,
	}
	// Start upload workers, one per queue shard. Uploads for one log id always
	// land on the same shard (see uploadShard), so an insert's upload and a
	// later reconcile's merge upload for that row cannot race on the object key.
	for i := range h.uploadQueues {
		h.uploadQueues[i] = make(chan *uploadWork, defaultUploadQueueSize/defaultUploadWorkers)
		h.wg.Add(1)
		go h.uploadWorker(h.uploadQueues[i])
	}
	if marker, ok := inner.(hasObjectMarker); ok {
		h.marker = marker
		h.markQueue = make(chan hasObjectMark, markQueueSize)
		h.markWG.Add(1)
		go h.markWorker()
	}
	return h
}

// uploadWorker processes async S3 upload jobs from one queue shard, in order.
func (h *HybridLogStore) uploadWorker(queue chan *uploadWork) {
	defer h.wg.Done()
	for work := range queue {
		h.processUpload(work)
	}
}

// uploadShard picks the queue for a log id. The same id always maps to the
// same shard, which is what orders a row's uploads.
func (h *HybridLogStore) uploadShard(logID string) chan *uploadWork {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(logID))
	return h.uploadQueues[int(hash.Sum32()%uint32(len(h.uploadQueues)))]
}

// queuedUploads reports how many uploads are waiting across all shards.
func (h *HybridLogStore) queuedUploads() int {
	n := 0
	for _, q := range h.uploadQueues {
		n += len(q)
	}
	return n
}

// processUpload uploads a single payload to object storage.
// This is fire-and-forget by design: on Put failure the upload is dropped and
// counted in droppedUploads. The DB row retains has_object=false, so FindByID
// falls back to whatever data the DB holds. Retries are intentionally omitted
// to keep S3 latency from cascading into the write path.
func (h *HybridLogStore) processUpload(work *uploadWork) {
	payloadSize := int64(len(work.payload))
	defer h.pendingBytes.Add(-payloadSize)

	defer func() {
		if r := recover(); r != nil {
			h.logger.Error("objectstore: panic in upload worker (recovered): %v", r)
			h.droppedUploads.Add(1)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	realtimeCorrection := work.kind == uploadKindLogMerge && gjson.GetBytes(work.payload, realtimeInputCorrectionField).Exists()
	if work.kind == uploadKindLogMerge {
		merged, ok := h.mergeIntoStoredObject(ctx, work)
		if !ok {
			return
		}
		work.payload = merged
	}

	if err := h.putPayload(ctx, work.key, work.payload, work.tags); err != nil {
		h.logger.Warn("objectstore: failed to upload log %s: %v", work.logID, err)
		h.droppedUploads.Add(1)
		return
	}
	if realtimeCorrection {
		var corrected Log
		if err := MergePayloadFromJSON(&corrected, work.payload); err == nil && corrected.InputHistory != "" {
			_ = corrected.DeserializeFields()
			if err := h.inner.Update(withLogTimestamp(ctx, work.timestamp), work.logID, map[string]any{"content_summary": corrected.BuildContentSummary()}); err != nil {
				h.logger.Warn("objectstore: failed to refresh realtime input summary for log %s: %v", work.logID, err)
			}
		}
	}
	// The new object carries everything the other-format copy held, so drop
	// that copy: a later switch back must not find it first and serve or merge
	// stale content.
	if work.staleKey != "" {
		if err := h.objects.Delete(ctx, work.staleKey); err != nil {
			h.logger.Warn("objectstore: failed to delete replaced object %s for log %s: %v", work.staleKey, work.logID, err)
		}
	}

	// Mark the DB row as having an object. A batching store takes the flip off
	// this worker, which goes straight on to its next Put; Close drains the
	// queue after the upload workers exit, so the send cannot hit a closed channel.
	if h.markQueue != nil {
		h.markQueue <- hasObjectMark{kind: work.kind, logID: work.logID, timestamp: work.timestamp, key: work.key}
		return
	}
	// Otherwise flip it here, with a fresh context so that a slow Put doesn't
	// starve the DB update of its deadline. Retry up to 3 times with
	// exponential backoff to avoid orphaning the uploaded object.
	for attempt := 0; attempt < 3; attempt++ {
		dbCtx, dbCancel := context.WithTimeout(withLogTimestamp(context.Background(), work.timestamp), 10*time.Second)
		err := h.inner.Update(dbCtx, work.logID, map[string]interface{}{"has_object": true})
		dbCancel()
		if err == nil {
			return
		}
		h.logger.Warn("objectstore: failed to set has_object for log %s (attempt %d/3): %v", work.logID, attempt+1, err)
		if attempt < 2 {
			time.Sleep(time.Duration(1<<attempt) * time.Second) // 1s, 2s backoff
		}
	}
	h.logger.Error("objectstore: failed to set has_object for log %s after 3 attempts; payload orphaned in object store", work.logID)
	h.droppedUploads.Add(1)
}

// markTable is the table whose has_object column an upload of kind sets.
func markTable(kind uploadKind) string {
	return "logs"
}

// markWorker batches has_object flips for a hasObjectMarker store. A failed
// batch, and an id with no row yet, is retried with the same 1s, 2s
// backoff the per-row path uses, without holding up newer flips. On Close it
// flushes what is left once and gives up on anything that still fails.
func (h *HybridLogStore) markWorker() {
	defer h.markWG.Done()
	ticker := time.NewTicker(markFlushInterval)
	defer ticker.Stop()
	batch := make([]hasObjectMark, 0, markBatchSize)
	var retries []hasObjectMark
	for {
		select {
		case m, ok := <-h.markQueue:
			if !ok {
				h.flushMarks(append(batch, retries...), true)
				return
			}
			batch = append(batch, m)
			if len(batch) < markBatchSize {
				continue
			}
		case <-ticker.C:
		}
		now := time.Now()
		waiting := retries[:0]
		for _, m := range retries {
			if m.retryAt.After(now) {
				waiting = append(waiting, m)
			} else {
				batch = append(batch, m)
			}
		}
		retries = waiting
		if len(batch) > 0 {
			retries = append(retries, h.flushMarks(batch, false)...)
			batch = batch[:0]
		}
	}
}

// flushMarks writes one batch of flips, one MarkHasObject call per table, and
// returns the flips to retry later. When final is set nothing is retried.
func (h *HybridLogStore) flushMarks(batch []hasObjectMark, final bool) []hasObjectMark {
	if len(batch) == 0 {
		return nil
	}
	byTable := make(map[string][]hasObjectMark, 1)
	for _, m := range batch {
		t := markTable(m.kind)
		byTable[t] = append(byTable[t], m)
	}
	var retry []hasObjectMark
	// missing means the row was not found, as opposed to the write failing.
	// Every hybrid create inserts the row before it queues the upload, so a row
	// still missing when the retries run out was deleted while its flip waited.
	// The delete skipped the object because has_object was still false, so it
	// is removed here rather than leaving a deleted log's content in storage.
	giveUpOrRetry := func(m hasObjectMark, missing bool) {
		m.attempts++
		if final || m.attempts >= markMaxAttempts {
			if missing {
				h.deleteObject(m.key, m.logID)
				return
			}
			h.logger.Error("objectstore: failed to set has_object for log %s after %d attempts; payload orphaned in object store", m.logID, m.attempts)
			h.droppedUploads.Add(1)
			return
		}
		m.retryAt = time.Now().Add(time.Duration(1<<(m.attempts-1)) * time.Second)
		retry = append(retry, m)
	}
	for table, marks := range byTable {
		rows := make([]HasObjectMark, len(marks))
		for i, m := range marks {
			rows[i] = HasObjectMark{ID: m.logID, Timestamp: m.timestamp}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		result, err := h.marker.MarkHasObject(ctx, table, rows)
		cancel()
		if err != nil {
			h.logger.Warn("objectstore: failed to set has_object for %d %s rows: %v", len(marks), table, err)
			for _, m := range marks {
				giveUpOrRetry(m, false)
			}
			continue
		}
		if len(result.Missing) == 0 {
			continue
		}
		absent := make(map[string]struct{}, len(result.Missing))
		for _, id := range result.Missing {
			absent[id] = struct{}{}
		}
		for _, m := range marks {
			if _, ok := absent[m.logID]; !ok {
				continue
			}
			giveUpOrRetry(m, true)
		}
	}
	return retry
}

// deleteObject removes an object no row references any more, in both
// formats. logID only labels the warning.
func (h *HybridLogStore) deleteObject(key, logID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.objects.DeleteBatch(ctx, h.withAlternateKeys(key)); err != nil {
		h.logger.Warn("objectstore: failed to delete unreferenced object %s (log %q): %v", key, logID, err)
	}
}

// isPayloadEmpty returns true when the payload carries no offloadable content.
// Only the large TEXT payload fields count: metadata like provider, model,
// status and timestamp is populated on every entry (see ExtractPayload), so
// checking the whole map would never report empty and would defeat the skip
// for initial "processing" rows that have no input/output data yet. Skipping
// uploads for empty payloads avoids wasted S3 PUTs.
func isPayloadEmpty(payload map[string]string) bool {
	for _, f := range payloadFields {
		if payload[f] != "" {
			return false
		}
	}
	return true
}

// mergeIntoStoredObject prepares a merge upload: the row's object is read and
// the work's non-empty payload fields are laid over it, so the result carries
// everything the object held plus the late fields. It runs on the row's upload
// shard, after any earlier upload for the same id, which is what lets a
// reconcile queued before the insert's upload finished still see the full
// input. The object store is always consulted, never the row's has_object flag:
// a Put can succeed while every has_object update fails, leaving an object the
// flag does not admit to. A definite not-found means there is nothing to
// preserve and the payload is uploaded as is; any other read or decode failure
// drops the upload and counts it, because a payload rebuilt without the stored
// content must never replace an object that may exist (for a hidden row that
// object is the only copy of the input).
func (h *HybridLogStore) mergeIntoStoredObject(ctx context.Context, work *uploadWork) ([]byte, bool) {
	existing, foundKey, err := h.getPayload(ctx, work.key)
	if err != nil {
		if errors.Is(err, objectstore.ErrNotFound) {
			if gjson.GetBytes(work.payload, realtimeInputCorrectionField).Exists() {
				h.logger.Warn("objectstore: cannot correct realtime input for missing object %s", work.logID)
				h.droppedUploads.Add(1)
				return nil, false
			}
			return work.payload, true
		}
		h.logger.Warn("objectstore: failed to fetch payload for log %s before merge upload, leaving the stored object untouched: %v", work.logID, err)
		h.droppedUploads.Add(1)
		return nil, false
	}
	var base, overlay map[string]string
	if err := sonic.Unmarshal(existing, &base); err != nil {
		h.logger.Warn("objectstore: failed to decode payload for log %s before merge upload, leaving the stored object untouched: %v", work.logID, err)
		h.droppedUploads.Add(1)
		return nil, false
	}
	if err := sonic.Unmarshal(work.payload, &overlay); err != nil {
		h.logger.Warn("objectstore: failed to decode merge payload for log %s: %v", work.logID, err)
		h.droppedUploads.Add(1)
		return nil, false
	}
	if base == nil {
		base = make(map[string]string, len(overlay))
	}
	if foundKey != work.key {
		// The stored copy is in the other format; the merged object replaces it.
		work.staleKey = foundKey
	}
	if err := mergeRealtimeInputCorrection(base, overlay); err != nil {
		h.logger.Warn("objectstore: cannot merge realtime input correction for log %s: %v", work.logID, err)
		h.droppedUploads.Add(1)
		return nil, false
	}
	for k, v := range overlay {
		if v != "" {
			base[k] = v
		}
	}
	merged, err := sonic.Marshal(base)
	if err != nil {
		h.logger.Warn("objectstore: failed to encode merged payload for log %s: %v", work.logID, err)
		h.droppedUploads.Add(1)
		return nil, false
	}
	return merged, true
}

// setObjectFormat selects how new payload objects are encoded. "parquet"
// writes single-row parquet files under .parquet keys with the given page
// codec; anything else keeps JSON under .json.gz keys. Objects already in the
// bucket stay readable either way (see getPayload).
func (h *HybridLogStore) setObjectFormat(format, compression string) {
	if format == objectstore.FormatParquet {
		h.objectExt = parquetObjectExt
		h.parquetCompression = compression
		return
	}
	h.objectExt = jsonObjectExt
	h.parquetCompression = ""
}

func (h *HybridLogStore) logKey(timestamp time.Time, logID string) string {
	return objectKey(h.prefix, "logs", timestamp, logID, h.objectExt)
}

// alternateKey returns the same object path in the other format (.parquet
// for a .json.gz key and the reverse), or "" when the store only ever holds
// JSON. A row's object stays under the key it was written with when
// object_storage.format changes, so reads, merges and deletes also look there.
func (h *HybridLogStore) alternateKey(key string) string {
	if h.singleFormat {
		return ""
	}
	if base, ok := strings.CutSuffix(key, parquetObjectExt); ok {
		return base + jsonObjectExt
	}
	if base, ok := strings.CutSuffix(key, jsonObjectExt); ok {
		return base + parquetObjectExt
	}
	return ""
}

// withAlternateKeys returns key plus its other-format key, if any, so
// deletes also remove objects written before a format switch.
func (h *HybridLogStore) withAlternateKeys(key string) []string {
	if alt := h.alternateKey(key); alt != "" {
		return []string{key, alt}
	}
	return []string{key}
}

// getPayload reads the object at key and returns it as JSON, along with the
// key it was found under. A key with no object falls back to its other-format
// key, and parquet bodies are decoded, so callers always receive the JSON the
// payload was built from. The second lookup only happens on a not-found.
func (h *HybridLogStore) getPayload(ctx context.Context, key string) ([]byte, string, error) {
	found := key
	data, err := h.objects.Get(ctx, key)
	if err != nil && errors.Is(err, objectstore.ErrNotFound) {
		if alt := h.alternateKey(key); alt != "" {
			found = alt
			data, err = h.objects.Get(ctx, alt)
		}
	}
	if err != nil {
		return nil, "", err
	}
	if objectstore.IsParquet(data) {
		decoded, err := objectstore.DecodeParquetToJSONObject(data)
		return decoded, found, err
	}
	return data, found, nil
}

// putPayload writes a JSON payload in the encoding its key names: parquet
// keys get a parquet body (with no outer gzip, since parquet compresses its
// pages), every other key gets the JSON as is.
func (h *HybridLogStore) putPayload(ctx context.Context, key string, payload []byte, tags map[string]string) error {
	if !strings.HasSuffix(key, parquetObjectExt) {
		return h.objects.Put(ctx, key, payload, tags)
	}
	encoded, err := objectstore.EncodeJSONObjectToParquet(payload, h.parquetCompression)
	if err != nil {
		return err
	}
	return objectstore.PutWith(ctx, h.objects, key, encoded, tags, objectstore.PutOptions{
		ContentType:     objectstore.ContentTypeParquet,
		SkipCompression: true,
	})
}

// enqueueUpload pushes an upload job onto the queue. If the queue is full,
// the job is dropped to prevent S3 slowness from cascading.
func (h *HybridLogStore) enqueueUpload(logID string, timestamp time.Time, payload map[string]string, tags map[string]string) {
	if h.closed.Load() || isPayloadEmpty(payload) {
		return
	}
	// Recover from send-on-closed-channel panic: Close() may interleave
	// between the closed check above and the channel send below.
	// Same pattern as plugins/logging/writer.go enqueueLogEntry.
	defer func() {
		if r := recover(); r != nil {
			h.droppedUploads.Add(1)
		}
	}()
	data, err := sonic.Marshal(payload)
	if err != nil {
		h.logger.Warn("objectstore: failed to marshal payload for log %s: %v", logID, err)
		h.droppedUploads.Add(1)
		return
	}
	h.enqueueRawUpload(logID, timestamp, h.logKey(timestamp, logID), uploadKindLog, "", data, tags)
}

// enqueueRawUpload submits a pre-serialized payload to the upload queue.
// Drops the upload (and increments droppedUploads) when the store is closed,
// the data is empty, the in-flight byte budget would be exceeded, or the queue
// is full. Used by regular log uploads (via enqueueUpload) and log merges.
func (h *HybridLogStore) enqueueRawUpload(logID string, timestamp time.Time, key string, kind uploadKind, status string, data []byte, tags map[string]string) {
	h.enqueueReplacingUpload(logID, timestamp, key, "", kind, status, data, tags)
}

// enqueueReplacingUpload is enqueueRawUpload for a full rewrite whose stored
// copy was read from staleKey (the other-format key); the worker deletes that
// copy once the new object is written.
func (h *HybridLogStore) enqueueReplacingUpload(logID string, timestamp time.Time, key, staleKey string, kind uploadKind, status string, data []byte, tags map[string]string) {
	if h.closed.Load() || len(data) == 0 {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			h.droppedUploads.Add(1)
		}
	}()
	if h.pendingBytes.Load()+int64(len(data)) > defaultMaxUploadQueueBytes {
		h.droppedUploads.Add(1)
		h.logger.Warn("objectstore: upload queue memory limit reached, dropping upload for log %s", logID)
		return
	}
	select {
	case h.uploadShard(logID) <- &uploadWork{
		logID:     logID,
		timestamp: timestamp,
		key:       key,
		kind:      kind,
		status:    status,
		payload:   data,
		tags:      tags,
		staleKey:  staleKey,
	}:
		h.pendingBytes.Add(int64(len(data)))
	default:
		h.droppedUploads.Add(1)
		h.logger.Warn("objectstore: upload queue full, dropping upload for log %s", logID)
	}
}

// --- Intercepted methods ---

// prepareDBEntry builds the lightweight DB entry by extracting the content
// summary, trimming input history to the last user message, and clearing
// payload fields that will be offloaded to object storage. Fields in the
// excluded set are kept intact in the DB row. Pricing metadata is always kept
// DB-resident regardless of hybrid configuration or content visibility.
// Must be called after SerializeFields() populates the Parsed fields.
func prepareDBEntry(dbEntry *Log, excluded map[string]struct{}) {
	tokenUsage := dbEntry.TokenUsage
	cacheMetadata := dbEntry.CacheDebug
	tokenUsageParsed := dbEntry.TokenUsageParsed
	cacheDebugParsed := dbEntry.CacheDebugParsed
	restorePricingMetadata := func() {
		dbEntry.TokenUsage = tokenUsage
		dbEntry.CacheDebug = cacheMetadata
		dbEntry.TokenUsageParsed = tokenUsageParsed
		dbEntry.CacheDebugParsed = cacheDebugParsed
	}

	// Hidden entries keep no request/response content in the DB row: no summary,
	// last-user-message preview, or exclusion-list carve-outs. Pricing metadata is
	// not content and remains in the log store.
	if dbEntry.ContentHidden {
		ClearPayload(dbEntry)
		restorePricingMetadata()
		dbEntry.ContentSummary = ""
		return
	}

	idx := findLastUserMessageIndex(dbEntry.InputHistoryParsed)

	// Content summary: text of the last user message. When there is none, or it
	// has no text, BuildInputContentSummary falls back to the latest tool result
	// or tool call, then to non-chat inputs (speech, image, etc.). The reset
	// matters: SerializeFields already filled ContentSummary with the full-text
	// search summary, which must not leak into the row.
	dbEntry.ContentSummary = ""
	if idx >= 0 {
		dbEntry.ContentSummary = extractChatMessageText(&dbEntry.InputHistoryParsed[idx])
	}
	if dbEntry.ContentSummary == "" {
		dbEntry.ContentSummary = dbEntry.BuildInputContentSummary()
	}
	// Bound content summary to prevent large prompts from bloating the DB row.
	dbEntry.ContentSummary = truncateTag(dbEntry.ContentSummary, MaxContentSummaryBytes)

	// Serialize last user message before ClearPayload zeros everything.
	// Attachment payloads (base64 images/files/audio, media URLs) are replaced
	// with a placeholder so they never bloat the DB row; the full message stays
	// in object storage and is merged back on read.
	var lastUserMessage string
	if idx >= 0 {
		stripped := stripChatMessageAttachments(&dbEntry.InputHistoryParsed[idx])
		lastUserMessage, _ = sonic.MarshalString([]schemas.ChatMessage{stripped})
	}

	// Responses API requests carry their history in responses_input_history
	// rather than input_history. Preserve the last user message there too, so
	// the log list can render a preview instead of "-" once the full history is
	// offloaded to object storage. Only needed when there is no chat input
	// history (a request is either chat- or responses-shaped, never both).
	var lastUserResponsesMessage string
	if idx < 0 {
		if responsesIdx := findLastUserResponsesMessageIndex(dbEntry.ResponsesInputHistoryParsed); responsesIdx >= 0 {
			stripped := stripResponsesMessageAttachments(&dbEntry.ResponsesInputHistoryParsed[responsesIdx])
			lastUserResponsesMessage, _ = sonic.MarshalString([]schemas.ResponsesMessage{stripped})
		}
	}

	ClearPayloadFiltered(dbEntry, excluded)
	restorePricingMetadata()

	if _, hasInputHistoryExclusion := excluded["input_history"]; !hasInputHistoryExclusion {
		dbEntry.InputHistory = sanitizeJSONForJSONB(lastUserMessage)
	}
	if _, hasResponsesInputHistoryExclusion := excluded["responses_input_history"]; !hasResponsesInputHistoryExclusion {
		dbEntry.ResponsesInputHistory = sanitizeJSONForJSONB(lastUserResponsesMessage)
	}
}

// Create writes a lightweight DB row (with payload fields stripped per
// excludedPayloadFields) and asynchronously offloads the full payload to
// object storage. The caller's entry is preserved on DB failure by writing to
// a shallow copy; on success, only ContentSummary is propagated back so the
// caller can observe what was persisted.
// extractUploadPayload returns the payload map to offload for entry. Hidden
// entries always upload the complete payload: the exclusion list exists to
// keep fields DB-resident, and hidden rows must not retain content in the DB.
func (h *HybridLogStore) extractUploadPayload(entry *Log) map[string]string {
	if entry.ContentHidden {
		return ExtractPayload(entry)
	}
	return ExtractPayloadFiltered(entry, h.excludedPayloadFields)
}

// errorRawBodyPaths are the error_details paths holding provider raw bodies,
// present only when content logging and raw storage are both on.
var errorRawBodyPaths = []string{"extra_fields.raw_request", "extra_fields.raw_response"}

// moveErrorRawBodiesToObject keeps error_details in the DB row without the
// provider raw bodies it carries, and puts the whole value in the upload
// payload instead, which FindByID merges back over the row's copy. Without it,
// keeping error_details DB-resident would keep raw bodies in the database even
// though raw_request and raw_response are offloaded. An operator who lists
// error_details in object_storage_exclude_fields keeps it whole in the DB.
// Content-hidden rows already offload error_details in full.
func (h *HybridLogStore) moveErrorRawBodiesToObject(dbEntry *Log, payload map[string]string) {
	if !h.offloadErrorRawBodies || dbEntry.ContentHidden || dbEntry.ErrorDetails == "" {
		return
	}
	stripped := dbEntry.ErrorDetails
	for _, path := range errorRawBodyPaths {
		if !gjson.Get(stripped, path).Exists() {
			continue
		}
		next, err := sjson.Delete(stripped, path)
		if err != nil {
			return
		}
		stripped = next
	}
	if stripped == dbEntry.ErrorDetails {
		return
	}
	payload["error_details"] = dbEntry.ErrorDetails
	dbEntry.ErrorDetails = stripped
	// SerializeFields on the write path would re-serialize a parsed copy over it.
	dbEntry.ErrorDetailsParsed = nil
}

// skipsOffload reports whether entry's request type is excluded from object
// storage. Excluded entries are written to the inner store unchanged, which
// builds their content summary on write. Hidden entries are always offloaded:
// their content may only live in object storage, never in the DB row.
func (h *HybridLogStore) skipsOffload(entry *Log) bool {
	if entry.ContentHidden {
		return false
	}
	if _, ok := h.excludedRequestTypes[entry.Object]; ok {
		return true
	}
	// A passthrough row stored under its inferred type still honours an exclusion of passthrough.
	_, ok := h.excludedRequestTypes[entry.PassthroughObjectType()]
	return ok
}

func (h *HybridLogStore) Create(ctx context.Context, entry *Log) error {
	if h.skipsOffload(entry) {
		return h.inner.Create(ctx, entry)
	}
	if err := entry.SerializeFields(); err != nil {
		return fmt.Errorf("logstore: serialize before extract: %w", err)
	}
	payload := h.extractUploadPayload(entry)
	tags := BuildTags(entry)
	// Work on a shallow copy so the caller's entry is preserved on DB failure.
	dbEntry := *entry
	prepareDBEntry(&dbEntry, h.excludedPayloadFields)
	h.moveErrorRawBodiesToObject(&dbEntry, payload)
	if err := h.inner.Create(ctx, &dbEntry); err != nil {
		return err
	}
	entry.ContentSummary = dbEntry.ContentSummary
	h.enqueueUpload(entry.ID, entry.Timestamp, payload, tags)
	return nil
}

// CreateIfNotExists writes the lightweight DB row only when no row with the
// same ID exists, then offloads the full payload to object storage on insert.
// Same payload-stripping and shallow-copy semantics as Create.
func (h *HybridLogStore) CreateIfNotExists(ctx context.Context, entry *Log) error {
	if h.skipsOffload(entry) {
		return h.inner.CreateIfNotExists(ctx, entry)
	}
	if err := entry.SerializeFields(); err != nil {
		return fmt.Errorf("logstore: serialize before extract: %w", err)
	}
	payload := h.extractUploadPayload(entry)
	tags := BuildTags(entry)
	// Work on a shallow copy so the caller's entry is preserved on DB failure.
	dbEntry := *entry
	prepareDBEntry(&dbEntry, h.excludedPayloadFields)
	h.moveErrorRawBodiesToObject(&dbEntry, payload)
	if err := h.inner.CreateIfNotExists(ctx, &dbEntry); err != nil {
		return err
	}
	entry.ContentSummary = dbEntry.ContentSummary
	h.enqueueUpload(entry.ID, entry.Timestamp, payload, tags)
	return nil
}

// BatchCreateIfNotExists inserts many lightweight DB rows in a single inner
// call, then enqueues one object-storage upload per inserted entry. Nil
// entries are skipped. On DB failure no uploads are enqueued.
func (h *HybridLogStore) BatchCreateIfNotExists(ctx context.Context, entries []*Log) error {
	if len(entries) == 0 {
		return nil
	}
	type pendingUpload struct {
		logID     string
		timestamp time.Time
		payload   map[string]string
		tags      map[string]string
	}
	var uploads []pendingUpload

	dbEntries := make([]*Log, 0, len(entries))
	origEntries := make([]*Log, 0, len(entries))
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		if h.skipsOffload(entry) {
			dbEntries = append(dbEntries, entry)
			origEntries = append(origEntries, entry)
			continue
		}
		if err := entry.SerializeFields(); err != nil {
			return fmt.Errorf("logstore: serialize before extract: %w", err)
		}
		payload := h.extractUploadPayload(entry)
		tags := BuildTags(entry)
		// Work on a shallow copy so the caller's entries are preserved on DB failure.
		dbEntry := *entry
		prepareDBEntry(&dbEntry, h.excludedPayloadFields)
		h.moveErrorRawBodiesToObject(&dbEntry, payload)
		dbEntries = append(dbEntries, &dbEntry)
		origEntries = append(origEntries, entry)
		uploads = append(uploads, pendingUpload{
			logID:     entry.ID,
			timestamp: entry.Timestamp,
			payload:   payload,
			tags:      tags,
		})
	}

	if len(dbEntries) == 0 {
		return nil
	}
	if err := h.inner.BatchCreateIfNotExists(ctx, dbEntries); err != nil {
		return err
	}

	for i, entry := range origEntries {
		entry.ContentSummary = dbEntries[i].ContentSummary
	}

	for _, u := range uploads {
		h.enqueueUpload(u.logID, u.timestamp, u.payload, u.tags)
	}
	return nil
}

// FindByID loads a log row from the DB and hydrates its payload fields from
// object storage when HasObject is true. Object-store fetch failures are
// logged but not returned, so the caller still receives the DB row.
func (h *HybridLogStore) FindByID(ctx context.Context, id string) (*Log, error) {
	log, err := h.inner.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	h.hydrateLog(ctx, log)
	return log, nil
}

// hydrateLog fetches the offloaded payload from object storage and merges it
// back into the Log struct. It is a no-op when HasObject is false.
//
// When requestedFields is non-empty, only the payload fields present in that
// projection are kept after merge — unrequested payload fields are cleared to
// honour projection semantics and avoid pulling large blobs unnecessarily.
func (h *HybridLogStore) hydrateLog(ctx context.Context, log *Log, requestedFields ...string) {
	// ContentHidden is the read-side enforcement for hidden logs: the payload
	// stays in object storage and is never fetched back into the serving path.
	if log == nil || !log.HasObject || log.ContentHidden {
		return
	}
	key := h.logKey(log.Timestamp, log.ID)
	data, _, err := h.getPayload(ctx, key)
	if err != nil {
		h.logger.Warn("objectstore: failed to fetch payload for log %s: %v", log.ID, err)
		return // Graceful degradation
	}
	if mergeErr := MergePayloadFromJSON(log, data); mergeErr != nil {
		h.logger.Warn("objectstore: failed to merge payload for log %s: %v", log.ID, mergeErr)
		return
	}
	pruneUnrequestedPayloadFields(log, requestedFields)
}

// Update passes the update through to the inner store for index/scalar field
// updates. Payload-field changes are not re-offloaded here; the logging
// plugin owns that path and calls processUpload directly when it needs to.
func (h *HybridLogStore) Update(ctx context.Context, id string, entry any) error {
	// Pass through to inner store for index field updates.
	// Payload fields in the update map are handled separately by the logging plugin.
	return h.inner.Update(ctx, id, entry)
}

// DeleteLog removes a single log row from the DB and, when HasObject is true,
// also deletes the corresponding payload from object storage. Object-store
// delete failures are logged but do not fail the operation, since the DB row
// (the source of truth for visibility) is already gone.
func (h *HybridLogStore) DeleteLog(ctx context.Context, id string) error {
	log, findErr := h.inner.FindByID(ctx, id)
	if findErr != nil && !errors.Is(findErr, ErrNotFound) {
		return findErr
	}
	if err := h.inner.DeleteLog(ctx, id); err != nil {
		return err
	}
	if log != nil && log.HasObject {
		var delErr error
		if keys := h.withAlternateKeys(h.logKey(log.Timestamp, log.ID)); len(keys) == 1 {
			delErr = h.objects.Delete(ctx, keys[0])
		} else {
			delErr = h.objects.DeleteBatch(ctx, keys)
		}
		if delErr != nil {
			h.logger.Warn("objectstore: failed to delete object for log %s: %v", id, delErr)
		}
	}
	return nil
}

// DeleteLogs removes many log rows from the DB and batches a single
// DeleteBatch call against object storage for any rows that had HasObject
// set. Keys are collected before the DB delete so we can still identify
// object-store entries to clean up.
func (h *HybridLogStore) DeleteLogs(ctx context.Context, ids []string) error {
	// Collect keys for S3 deletion before removing from DB.
	var keys []string
	for _, id := range ids {
		log, findErr := h.inner.FindByID(ctx, id)
		if findErr != nil && !errors.Is(findErr, ErrNotFound) {
			return findErr
		}
		if log != nil && log.HasObject {
			keys = append(keys, h.withAlternateKeys(h.logKey(log.Timestamp, log.ID))...)
		}
	}
	if err := h.inner.DeleteLogs(ctx, ids); err != nil {
		return err
	}
	if len(keys) > 0 {
		if delErr := h.objects.DeleteBatch(ctx, keys); delErr != nil {
			h.logger.Warn("objectstore: failed to batch delete %d objects: %v", len(keys), delErr)
		}
	}
	return nil
}

// DeleteLogsBatch deletes old DB rows older than cutoff in batches of
// batchSize. Object-store entries are intentionally NOT deleted here: the
// expectation is that the bucket has a lifecycle policy configured to expire
// objects on the same schedule, which is far cheaper than per-row DELETEs.
func (h *HybridLogStore) DeleteLogsBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	// Delegate to inner — S3 objects will be cleaned up by lifecycle policies.
	return h.inner.DeleteLogsBatch(ctx, cutoff, batchSize)
}

// RetentionInterval forwards to the inner store.
func (h *HybridLogStore) RetentionInterval() time.Duration {
	return h.inner.RetentionInterval()
}

// Close shuts the store down cleanly: marks the store closed (so further
// enqueues are dropped), closes the upload queue, waits for workers to drain
// any in-flight uploads, then closes the object store and the inner store.
// If ctx is cancelled before workers drain, the cancellation is logged but
// Close still waits for workers to exit to avoid closing dependencies
// mid-flight.
func (h *HybridLogStore) Close(ctx context.Context) error {
	h.closed.Store(true)
	for _, q := range h.uploadQueues {
		close(q)
	}
	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		h.logger.Warn("objectstore: shutdown cancelled before upload queue drained: %v", ctx.Err())
		// Still wait for workers to finish so we don't close dependencies mid-flight.
		<-done
	}
	// The upload workers were the only senders, so the flip queue can close now.
	if h.markQueue != nil {
		close(h.markQueue)
		h.markWG.Wait()
	}
	if err := h.objects.Close(); err != nil {
		h.logger.Warn("objectstore: error closing object store: %v", err)
	}
	return h.inner.Close(ctx)
}

// DroppedUploads returns the number of S3 uploads that were dropped.
func (h *HybridLogStore) DroppedUploads() int64 {
	return h.droppedUploads.Load()
}

// --- Delegated methods (pass through to inner store unchanged) ---

// ScopedDB exposes the underlying RDB query surface when hybrid mode wraps a
// Postgres/SQLite logstore. Enterprise logstore extensions use this for their
// own logstore-owned tables, such as alert history.
func (h *HybridLogStore) ScopedDB(ctx context.Context) *gorm.DB {
	if scoped, ok := h.inner.(scopedDBLogStore); ok {
		return scoped.ScopedDB(ctx)
	}
	return nil
}

// Ping delegates to the inner store's health check.
func (h *HybridLogStore) Ping(ctx context.Context) error {
	return h.inner.Ping(ctx)
}

// FindFirst returns the first log matching query. When the projection asks
// for payload-bearing fields (or the projection is empty, implying full
// hydration), the result is hydrated from object storage; otherwise the DB
// row is returned as-is.
func (h *HybridLogStore) FindFirst(ctx context.Context, query any, fields ...string) (*Log, error) {
	needsHydration := len(fields) == 0 || fieldsNeedHydration(fields)
	if needsHydration && len(fields) > 0 {
		fields = ensureHydrationFields(fields)
	}
	log, err := h.inner.FindFirst(ctx, query, fields...)
	if err != nil {
		return nil, err
	}
	if needsHydration {
		h.hydrateLog(ctx, log, fields...)
	}
	return log, nil
}

// FindAll returns all logs matching query. Each result is hydrated from
// object storage when the projection requires payload fields. Per-row
// hydration failures are logged inside hydrateLog and do not fail the call.
func (h *HybridLogStore) FindAll(ctx context.Context, query any, fields ...string) ([]*Log, error) {
	needsHydration := len(fields) == 0 || fieldsNeedHydration(fields)
	if needsHydration && len(fields) > 0 {
		fields = ensureHydrationFields(fields)
	}
	logs, err := h.inner.FindAll(ctx, query, fields...)
	if err != nil {
		return nil, err
	}
	if needsHydration {
		for _, log := range logs {
			h.hydrateLog(ctx, log, fields...)
		}
	}
	return logs, nil
}

// FindAllDistinct delegates to the inner store. No hydration is performed:
// distinct queries operate purely on DB-resident columns and never need the
// offloaded payload.
func (h *HybridLogStore) FindAllDistinct(ctx context.Context, query any, fields ...string) ([]*Log, error) {
	return h.inner.FindAllDistinct(ctx, query, fields...)
}

// HasLogs delegates to the inner store and reports whether any log rows
// exist. The object store is not consulted.
func (h *HybridLogStore) HasLogs(ctx context.Context) (bool, error) {
	return h.inner.HasLogs(ctx)
}

// SearchLogs delegates to the inner store. Search operates on indexed DB
// columns (including ContentSummary); payloads are not hydrated here.
func (h *HybridLogStore) SearchLogs(ctx context.Context, filters SearchFilters, pagination PaginationOptions) (*SearchResult, error) {
	return h.inner.SearchLogs(ctx, filters, pagination)
}

// SearchLogsForBilling delegates to the inner store. Hydration is deliberately NOT
// done here — see HydrateBillingChunk.
func (h *HybridLogStore) SearchLogsForBilling(ctx context.Context, filters SearchFilters, pagination PaginationOptions) (*SearchResult, error) {
	return h.inner.SearchLogsForBilling(ctx, filters, pagination)
}

// HydrateBillingChunk restores offloaded pricing inputs for a small slice of rows.
//
// This exists because of a real mis-billing bug: a row whose token_usage was offloaded
// comes back with a stub rebuilt from denormalized columns, and because PromptTokens is
// inclusive of the cache buckets, pricing that stub charges every cache-read token at
// the full input rate — inflating a cache-heavy request several fold.
//
// Two deliberate properties:
//
// SEQUENTIAL — one object in flight at a time. A parallel fan-out multiplies peak
// memory by its width, since each in-flight fetch holds the whole object as a []byte
// plus the map[string]string that MergePayloadFromJSON unmarshals it into, and objects
// can carry full message histories and raw request/response bodies. This runs inside a
// checkpointed background job, so predictability beats throughput. Combined with the
// caller paging and chunking at BillingHydrationChunkSize, a recompute worker's
// payload memory is bounded by a handful of rows no matter how large the window.
//
// SKIPS ROWS THAT DO NOT NEED IT — a row already carrying its pricing inputs costs no
// I/O at all. See billingRowNeedsHydration.
func (h *HybridLogStore) HydrateBillingChunk(ctx context.Context, logs []*Log) (BillingHydrationResult, error) {
	var result BillingHydrationResult
	for _, log := range logs {
		if log == nil || !log.HasObject {
			// Never offloaded: the row already carries its own payload.
			continue
		}
		if !billingRowNeedsHydration(log, h.excludedPayloadFields) {
			continue
		}
		// Checked per row rather than relying on Get to notice: on shutdown or job
		// cancellation this should stop fetching immediately, not walk the rest of
		// the chunk collecting context errors.
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if hydrateErr := h.hydrateLogForBilling(ctx, log); hydrateErr != nil {
			h.logger.Warn("objectstore: cannot hydrate pricing inputs for log %s: %v", log.ID, hydrateErr)
			result.Unpriceable = append(result.Unpriceable, log.ID)
			continue
		}
		result.Hydrated = append(result.Hydrated, log.ID)
	}
	return result, nil
}

// billingRowNeedsHydration reports whether an offloaded row is actually missing
// something pricing reads. Fetching a whole object to recover fields the row already
// has is pure waste — and because the store exposes only whole-key Get, one wasted
// fetch downloads the entire payload, message histories included.
//
// excluded is the store's object_storage_exclude_fields set, and it is what makes the
// check precise. An empty column on its own is ambiguous: it can mean "offloaded" or
// "never written". A row with no semantic cache has an empty cache_debug forever, so
// treating empty as "go fetch" would fetch nearly every row and would keep fetching
// the same row after a successful hydration recovered nothing. Consulting the
// exclusion set resolves that: if a column is configured DB-resident, empty means
// genuinely empty and there is nothing in the object to recover.
//
// The fields considered:
//
//   - token_usage: the field whose absence caused the overbill.
//   - cache_debug: absent means a semantic-cache hit could be priced at full LLM cost
//     instead of $0, so it is fetched unless configured resident.
//   - the modality payload: only the column this row's own request type bills on. A
//     chat row is not missing anything by having no speech_output.
func billingRowNeedsHydration(l *Log, excluded map[string]struct{}) bool {
	// Already fetched in this pass: whatever the object held is now on the row, and the
	// fields it did not hold will never appear no matter how often we ask.
	if l.billingPayloadsHydrated {
		return false
	}
	// Content-hidden rows are eligible for billing-only hydration even though ordinary
	// serving reads never hydrate them — but eligible is not the same as force-fetched.
	// They get their own branch rather than falling through because they are written with
	// ClearPayload, which ignores the DB-resident exclusion list entirely, so `excluded`
	// vouches for nothing on them and the checks below would misread it in both
	// directions: it would wave through an emptied-but-"resident" modality column, and it
	// would defeat the backfilled inference for a deployment that keeps token_usage
	// resident, refetching such a row on every pass to recover nothing.
	//
	// A stronger signal replaces it. prepareDBEntry restores token_usage and cache_debug
	// together on every hidden row, so a present token_usage means this row was written
	// after that change and its empty cache_debug is genuinely empty. A blank one is a
	// legacy hidden row whose pricing inputs live only in the retained object.
	if l.ContentHidden {
		if l.TokenUsage == "" {
			return true
		}
		// Modality payloads are cleared unconditionally here and are never backfilled,
		// so an empty column means the object holds the only copy.
		if col := billingPayloadColumnFor(l.Object); col != "" {
			return billingPayloadColumnValue(l, col) == ""
		}
		return false
	}

	resident := func(column string) bool {
		_, ok := excluded[column]
		return ok
	}

	// A previous recompute already fetched this row and wrote the small pricing inputs
	// back (see BulkBackfillBillingPayloads). That state is detectable without a marker
	// column: offloading blanks token_usage, so a has_object row that nonetheless has
	// one can only have been backfilled. The exception is a deployment that configured
	// token_usage to stay resident — there its presence is just config and says nothing
	// about whether anything else was recovered.
	//
	// This matters because writing an empty cache_debug back cannot speak for itself:
	// most rows have no semantic cache, so without this inference the gate would keep
	// refetching them forever and the backfill would buy nothing.
	backfilled := l.TokenUsage != "" && !resident("token_usage")

	vouched := func(column, value string) bool {
		return value != "" || resident(column) || backfilled
	}

	if !vouched("token_usage", l.TokenUsage) || !vouched("cache_debug", l.CacheDebug) {
		return true
	}
	if col := billingPayloadColumnFor(l.Object); col != "" {
		// Modality payloads are never backfilled — they are precisely what offloading
		// exists to keep out of the database — so only the exclusion set can vouch for
		// an empty column here, never a prior backfill.
		return billingPayloadColumnValue(l, col) == "" && !resident(col)
	}
	return false
}

// BulkBackfillBillingPayloads delegates to the inner store.
//
// Safe to write these columns even though the offload path blanked them: Update is a
// plain pass-through here (it does not re-run prepareDBEntry), so nothing re-strips
// them. The object keeps its own copy, so this is a cache, not a move.
func (h *HybridLogStore) BulkBackfillBillingPayloads(ctx context.Context, updates map[string]BillingPayloadBackfill) error {
	return h.inner.BulkBackfillBillingPayloads(ctx, updates)
}

// billingPayloadColumnValue reads the serialized value of one modality payload column.
func billingPayloadColumnValue(l *Log, column string) string {
	switch column {
	case "speech_output":
		return l.SpeechOutput
	case "transcription_output":
		return l.TranscriptionOutput
	case "image_generation_output":
		return l.ImageGenerationOutput
	case "video_generation_output":
		return l.VideoGenerationOutput
	case "ocr_output":
		return l.OCROutput
	default:
		return ""
	}
}

// hydrateLogForBilling fetches the offloaded payload and merges back only the
// fields pricing reads, then re-deserializes so the virtual structs match.
//
// Unlike hydrateLog it returns the error instead of degrading gracefully. Silent
// degradation is right for a rendering path — a missing message preview is
// cosmetic — but here it would leave the lossy usage stub in place and bill it,
// which is exactly the failure this method exists to prevent.
func (h *HybridLogStore) hydrateLogForBilling(ctx context.Context, log *Log) error {
	// Only the payload this row's own request type bills on is kept; the rest are
	// pruned after merge so an unrelated blob never lingers in memory.
	fields := []string{"token_usage", "cache_debug"}
	if col := billingPayloadColumnFor(log.Object); col != "" {
		fields = append(fields, col)
	}

	data, _, err := h.getPayload(ctx, h.logKey(log.Timestamp, log.ID))
	if err != nil {
		return fmt.Errorf("fetch payload: %w", err)
	}
	if err := MergePayloadFromJSON(log, data); err != nil {
		return fmt.Errorf("merge payload: %w", err)
	}
	pruneUnrequestedPayloadFields(log, fields)
	// MergePayloadFromJSON writes the serialized columns only. Re-deserialize so
	// TokenUsageParsed reflects the hydrated payload rather than the stub AfterFind
	// built while token_usage was still blank.
	if err := log.DeserializeFields(); err != nil {
		return fmt.Errorf("deserialize hydrated payload: %w", err)
	}
	// Recorded before the usage check below: the object was read, so re-asking would
	// return the same thing. Only a fetch that never happened should be retried.
	log.billingPayloadsHydrated = true
	// Drop the generated-image bytes the hydrated payload carries but pricing never
	// reads; it only needs the image count.
	stripNonBillingPayloadBytes(log)
	// Only token-billed rows need token_usage. A speech/ocr/video/image row prices off
	// the modality payload recovered above — input chars, pages processed, seconds,
	// image count — and computeOCRCost takes no usage argument at all. Rejecting those
	// here would report Unpriceable, which claims the inputs are unrecoverable, when
	// hydration in fact recovered everything pricing reads.
	if log.TokenUsage == "" && billingPayloadColumnFor(log.Object) == "" {
		// The object exists but carries no usage. Nothing to price from, and the
		// stub is still in place — report rather than let the caller bill it.
		return fmt.Errorf("hydrated payload has no token_usage")
	}
	return nil
}

// GetSessionLogs delegates to the inner store and returns the paginated logs
// belonging to the given session.
func (h *HybridLogStore) GetSessionLogs(ctx context.Context, sessionID string, pagination PaginationOptions) (*SessionDetailResult, error) {
	return h.inner.GetSessionLogs(ctx, sessionID, pagination)
}

// GetSessionSummary delegates to the inner store and returns an aggregated
// summary for the given session.
func (h *HybridLogStore) GetSessionSummary(ctx context.Context, sessionID string) (*SessionSummaryResult, error) {
	return h.inner.GetSessionSummary(ctx, sessionID)
}

// GetStats delegates to the inner store and returns aggregate statistics for
// the matching log rows.
func (h *HybridLogStore) GetStats(ctx context.Context, filters SearchFilters) (*SearchStats, error) {
	return h.inner.GetStats(ctx, filters)
}

// GetHistogram delegates to the inner store and returns a request-count
// histogram bucketed by bucketSizeSeconds.
func (h *HybridLogStore) GetHistogram(ctx context.Context, filters SearchFilters, bucketSizeSeconds int64) (*HistogramResult, error) {
	return h.inner.GetHistogram(ctx, filters, bucketSizeSeconds)
}

// GetTokenHistogram delegates to the inner store and returns a token-usage
// histogram bucketed by bucketSizeSeconds.
func (h *HybridLogStore) GetTokenHistogram(ctx context.Context, filters SearchFilters, bucketSizeSeconds int64) (*TokenHistogramResult, error) {
	return h.inner.GetTokenHistogram(ctx, filters, bucketSizeSeconds)
}

// GetCostHistogram delegates to the inner store and returns a cost histogram
// bucketed by bucketSizeSeconds.
func (h *HybridLogStore) GetCostHistogram(ctx context.Context, filters SearchFilters, bucketSizeSeconds int64) (*CostHistogramResult, error) {
	return h.inner.GetCostHistogram(ctx, filters, bucketSizeSeconds)
}

// GetModelHistogram delegates to the inner store and returns a per-model
// request histogram bucketed by bucketSizeSeconds.
func (h *HybridLogStore) GetModelHistogram(ctx context.Context, filters SearchFilters, bucketSizeSeconds int64) (*ModelHistogramResult, error) {
	return h.inner.GetModelHistogram(ctx, filters, bucketSizeSeconds)
}

// GetLatencyHistogram delegates to the inner store and returns a latency
// histogram bucketed by bucketSizeSeconds.
func (h *HybridLogStore) GetLatencyHistogram(ctx context.Context, filters SearchFilters, bucketSizeSeconds int64) (*LatencyHistogramResult, error) {
	return h.inner.GetLatencyHistogram(ctx, filters, bucketSizeSeconds)
}

// GetProviderCostHistogram delegates to the inner store and returns a
// per-provider cost histogram bucketed by bucketSizeSeconds.
func (h *HybridLogStore) GetProviderCostHistogram(ctx context.Context, filters SearchFilters, bucketSizeSeconds int64) (*ProviderCostHistogramResult, error) {
	return h.inner.GetProviderCostHistogram(ctx, filters, bucketSizeSeconds)
}

// GetProviderTokenHistogram delegates to the inner store and returns a
// per-provider token histogram bucketed by bucketSizeSeconds.
func (h *HybridLogStore) GetProviderTokenHistogram(ctx context.Context, filters SearchFilters, bucketSizeSeconds int64) (*ProviderTokenHistogramResult, error) {
	return h.inner.GetProviderTokenHistogram(ctx, filters, bucketSizeSeconds)
}

// GetProviderLatencyHistogram delegates to the inner store and returns a
// per-provider latency histogram bucketed by bucketSizeSeconds.
func (h *HybridLogStore) GetProviderLatencyHistogram(ctx context.Context, filters SearchFilters, bucketSizeSeconds int64) (*ProviderLatencyHistogramResult, error) {
	return h.inner.GetProviderLatencyHistogram(ctx, filters, bucketSizeSeconds)
}

// GetThroughputHistogram delegates to the inner store and returns a
// token-generation throughput (tokens/sec) histogram bucketed by bucketSizeSeconds.
func (h *HybridLogStore) GetThroughputHistogram(ctx context.Context, filters SearchFilters, bucketSizeSeconds int64) (*ThroughputHistogramResult, error) {
	return h.inner.GetThroughputHistogram(ctx, filters, bucketSizeSeconds)
}

// GetProviderThroughputHistogram delegates to the inner store and returns a
// per-provider throughput (tokens/sec) histogram bucketed by bucketSizeSeconds.
func (h *HybridLogStore) GetProviderThroughputHistogram(ctx context.Context, filters SearchFilters, bucketSizeSeconds int64) (*ProviderThroughputHistogramResult, error) {
	return h.inner.GetProviderThroughputHistogram(ctx, filters, bucketSizeSeconds)
}

// GetModelRankings delegates to the inner store and returns ranked usage
// aggregates per model for the matching log rows.
func (h *HybridLogStore) GetModelRankings(ctx context.Context, filters SearchFilters) (*ModelRankingResult, error) {
	return h.inner.GetModelRankings(ctx, filters)
}

// GetUserRankings delegates to the inner store and returns ranked usage
// aggregates per user for the matching log rows.
func (h *HybridLogStore) GetUserRankings(ctx context.Context, filters SearchFilters) (*UserRankingResult, error) {
	return h.inner.GetUserRankings(ctx, filters)
}

// GetUserSpend delegates to the inner store and returns each user's total cost in
// the filter window.
func (h *HybridLogStore) GetUserSpend(ctx context.Context, filters SearchFilters) ([]UserSpendEntry, error) {
	return h.inner.GetUserSpend(ctx, filters)
}

func (h *HybridLogStore) GetDimensionRankings(ctx context.Context, filters SearchFilters, dimension RankingDimension) (*DimensionRankingResult, error) {
	return h.inner.GetDimensionRankings(ctx, filters, dimension)
}

// GetDimensionUsage delegates to the inner store and returns each entity's usage
// on a rollup dimension in one read.
func (h *HybridLogStore) GetDimensionUsage(ctx context.Context, filters SearchFilters, dimension RankingDimension, compare bool) ([]DimensionUsageEntry, error) {
	return h.inner.GetDimensionUsage(ctx, filters, dimension, compare)
}

// GetDimensionModelUsage delegates to the inner store and returns usage per
// (entity, model, provider) on a rollup dimension in one read.
func (h *HybridLogStore) GetDimensionModelUsage(ctx context.Context, filters SearchFilters, dimension RankingDimension) ([]DimensionModelUsageEntry, error) {
	return h.inner.GetDimensionModelUsage(ctx, filters, dimension)
}

// GetDimensionCostHistogram delegates to the inner store and returns a cost
// histogram bucketed by bucketSizeSeconds and grouped by the given dimension.
func (h *HybridLogStore) GetDimensionCostHistogram(ctx context.Context, filters SearchFilters, bucketSizeSeconds int64, dimension HistogramDimension) (*DimensionCostHistogramResult, error) {
	return h.inner.GetDimensionCostHistogram(ctx, filters, bucketSizeSeconds, dimension)
}

// GetDimensionTokenHistogram delegates to the inner store and returns a token
// histogram bucketed by bucketSizeSeconds and grouped by the given dimension.
func (h *HybridLogStore) GetDimensionTokenHistogram(ctx context.Context, filters SearchFilters, bucketSizeSeconds int64, dimension HistogramDimension) (*DimensionTokenHistogramResult, error) {
	return h.inner.GetDimensionTokenHistogram(ctx, filters, bucketSizeSeconds, dimension)
}

// GetDimensionLatencyHistogram delegates to the inner store and returns a
// latency histogram bucketed by bucketSizeSeconds and grouped by the given
// dimension.
func (h *HybridLogStore) GetDimensionLatencyHistogram(ctx context.Context, filters SearchFilters, bucketSizeSeconds int64, dimension HistogramDimension) (*DimensionLatencyHistogramResult, error) {
	return h.inner.GetDimensionLatencyHistogram(ctx, filters, bucketSizeSeconds, dimension)
}

// BatchUpdate reconciles output fields onto existing rows the same way an
// insert would have placed them: the DB row gets the scalar columns, and the
// payload goes to object storage. The current row is read once per id (the
// update carries no timestamp, which the object key needs), every entry for
// that id is applied to the DB in batch order, and their non-empty payload
// fields are folded, in that same order, into ONE merge upload that the upload
// worker lays over the row's stored object (see mergeIntoStoredObject). The DB
// row stays content-free. Hidden rows are handled identically, which is what
// keeps a retained response from being lost: the hidden DB row must never hold
// content, so the object is the only place the late output can go. Rows whose
// request type is excluded from offloading pass through unchanged, and rows
// that do not exist are skipped, as in the inner store.
func (h *HybridLogStore) BatchUpdate(ctx context.Context, entries []*Log) error {
	type pendingUpload struct {
		logID     string
		timestamp time.Time
		payload   map[string]string
		tagRow    *Log // the saved row with every entry's non-zero fields applied, in batch order
	}
	uploadsByID := make(map[string]*pendingUpload)
	var uploadOrder []string
	rowsByID := make(map[string]*Log)
	dbEntries := make([]*Log, 0, len(entries))
	for _, entry := range entries {
		if entry == nil || entry.ID == "" {
			continue
		}
		row, seen := rowsByID[entry.ID]
		if !seen {
			var err error
			row, err = h.inner.FindByID(ctx, entry.ID)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					rowsByID[entry.ID] = nil
					continue
				}
				return err
			}
			rowsByID[entry.ID] = row
		}
		if row == nil {
			continue
		}
		// The entry carries the request's final visibility decision, which may
		// clear a hidden flag the row was saved with before that decision existed.
		hidden := entry.ContentHidden
		if _, excluded := h.excludedRequestTypes[row.Object]; excluded && !hidden {
			dbEntries = append(dbEntries, entry)
			continue
		}
		if err := entry.SerializeFields(); err != nil {
			return fmt.Errorf("logstore: serialize before update: %w", err)
		}
		// Shallow copies so the caller's entry is preserved on DB failure.
		payloadEntry := *entry
		// The update carries no timestamp; without the row's, the payload would
		// carry the zero time and the merge would overwrite the stored one.
		payloadEntry.Timestamp = row.Timestamp
		payloadEntry.ContentHidden = hidden
		dbEntry := *entry
		dbEntry.ContentHidden = hidden
		prepareDBEntry(&dbEntry, h.excludedPayloadFields)

		upload := uploadsByID[entry.ID]
		if upload == nil {
			tagRow := *row
			upload = &pendingUpload{logID: row.ID, timestamp: row.Timestamp, payload: make(map[string]string), tagRow: &tagRow}
			uploadsByID[entry.ID] = upload
			uploadOrder = append(uploadOrder, entry.ID)
		}
		// Later entries for the same id override earlier ones field by field,
		// matching the order the DB applies them.
		for k, v := range h.extractUploadPayload(&payloadEntry) {
			if v != "" {
				upload.payload[k] = v
			}
		}
		h.moveErrorRawBodiesToObject(&dbEntry, upload.payload)
		if entry.RealtimeInputCorrection != nil {
			var corrections []*RealtimeInputCorrection
			if prior := upload.payload[realtimeInputCorrectionField]; prior != "" {
				if err := sonic.UnmarshalString(prior, &corrections); err != nil {
					return err
				}
			}
			corrections = append(corrections, entry.RealtimeInputCorrection)
			encoded, err := sonic.MarshalString(corrections)
			if err != nil {
				return err
			}
			upload.payload[realtimeInputCorrectionField] = encoded
		}
		// Tags describe the row as the batch leaves it: the selected key, virtual
		// key, routing rule and stream flag only arrive with the completion.
		overlayNonZeroFields(upload.tagRow, entry)
		dbEntries = append(dbEntries, &dbEntry)
	}
	if len(dbEntries) == 0 {
		return nil
	}
	if err := h.inner.BatchUpdate(ctx, dbEntries); err != nil {
		return err
	}
	for _, id := range uploadOrder {
		u := uploadsByID[id]
		if isPayloadEmpty(u.payload) {
			continue
		}
		data, err := sonic.Marshal(u.payload)
		if err != nil {
			h.logger.Warn("objectstore: failed to marshal merge payload for log %s: %v", u.logID, err)
			h.droppedUploads.Add(1)
			continue
		}
		h.enqueueRawUpload(u.logID, u.timestamp, h.logKey(u.timestamp, u.logID), uploadKindLogMerge, "", data, BuildTags(u.tagRow))
	}
	return nil
}

// overlayNonZeroFields copies every non-zero field of src onto dst, the same
// semantics the DB update applies (GORM Updates with a struct), so a row
// assembled this way matches what the update leaves in the database.
func overlayNonZeroFields(dst, src *Log) {
	d := reflect.ValueOf(dst).Elem()
	s := reflect.ValueOf(src).Elem()
	for i := 0; i < s.NumField(); i++ {
		field := s.Field(i)
		if field.IsZero() || !d.Field(i).CanSet() {
			continue
		}
		d.Field(i).Set(field)
	}
}

// BulkUpdateCost delegates to the inner store and updates the cost columns
// (total + input/output/additional split) for each (logID -> CostUpdate) pair
// in a single round trip.
func (h *HybridLogStore) BulkUpdateCost(ctx context.Context, updates map[string]CostUpdate) error {
	return h.inner.BulkUpdateCost(ctx, updates)
}

// GetNodeUsageAfter delegates to the inner store and returns usage
// aggregates for the given node since cursor, used by the cluster gossip
// path to ship incremental usage to peers.
func (h *HybridLogStore) GetNodeUsageAfter(ctx context.Context, nodeID string, cursor NodeUsageCursor) (*NodeUsageAggregate, error) {
	return h.inner.GetNodeUsageAfter(ctx, nodeID, cursor)
}

// Flush delegates to the inner store and forces any buffered DB writes since
// the given timestamp to be persisted. Object-store uploads are independently
// queued and not affected.
func (h *HybridLogStore) Flush(ctx context.Context, since time.Time) error {
	return h.inner.Flush(ctx, since)
}

// IsLogEntryPresent delegates to the inner store and reports whether a log
// row with the given ID exists.
func (h *HybridLogStore) IsLogEntryPresent(ctx context.Context, id string) (bool, error) {
	return h.inner.IsLogEntryPresent(ctx, id)
}

// GetDistinctAliases delegates to the inner store and returns distinct alias
// values matching query, capped at limit.
func (h *HybridLogStore) GetDistinctAliases(ctx context.Context, limit int, query string) ([]string, error) {
	return h.inner.GetDistinctAliases(ctx, limit, query)
}

// GetDistinctModels delegates to the inner store and returns distinct model
// values matching query, capped at limit.
func (h *HybridLogStore) GetDistinctModels(ctx context.Context, limit int, query string) ([]string, error) {
	return h.inner.GetDistinctModels(ctx, limit, query)
}

// GetDistinctKeyPairs delegates to the inner store and returns distinct
// (idCol, nameCol) pairs matching query, capped at limit. Used to populate
// id/name filters in the UI for virtual keys, providers, etc.
func (h *HybridLogStore) GetDistinctKeyPairs(ctx context.Context, idCol, nameCol string, limit int, query string) ([]KeyPairResult, error) {
	return h.inner.GetDistinctKeyPairs(ctx, idCol, nameCol, limit, query)
}

// GetDistinctRoutingEngines delegates to the inner store and returns
// distinct routing-engine values matching query, capped at limit.
func (h *HybridLogStore) GetDistinctRoutingEngines(ctx context.Context, limit int, query string) ([]string, error) {
	return h.inner.GetDistinctRoutingEngines(ctx, limit, query)
}

// GetDistinctToolCallNames delegates to the inner store. tool_call_names is
// not a payload field, so the inner row carries it even when content is offloaded.
func (h *HybridLogStore) GetDistinctToolCallNames(ctx context.Context, limit int, query string) ([]string, error) {
	return h.inner.GetDistinctToolCallNames(ctx, limit, query)
}

// GetDistinctStopReasons delegates to the inner store and returns distinct
// stop-reason values matching query, capped at limit.
func (h *HybridLogStore) GetDistinctStopReasons(ctx context.Context, limit int, query string) ([]string, error) {
	return h.inner.GetDistinctStopReasons(ctx, limit, query)
}

// GetDistinctUserAgents delegates to the inner store and returns distinct
// raw User-Agent strings for the logs "App" filter, capped at limit.
func (h *HybridLogStore) GetDistinctUserAgents(ctx context.Context, limit int, query string) ([]string, error) {
	return h.inner.GetDistinctUserAgents(ctx, limit, query)
}

// GetDistinctApps delegates to the inner store and returns distinct backend-
// detected app labels from recent logs.
func (h *HybridLogStore) GetDistinctApps(ctx context.Context, limit int, query string) ([]string, error) {
	return h.inner.GetDistinctApps(ctx, limit, query)
}

func (h *HybridLogStore) CreateUserAgentMapping(ctx context.Context, mapping *UserAgentMapping) error {
	return h.inner.CreateUserAgentMapping(ctx, mapping)
}

func (h *HybridLogStore) UpdateUserAgentMapping(ctx context.Context, id string, mapping *UserAgentMapping) error {
	return h.inner.UpdateUserAgentMapping(ctx, id, mapping)
}

func (h *HybridLogStore) DeleteUserAgentMapping(ctx context.Context, id string) error {
	return h.inner.DeleteUserAgentMapping(ctx, id)
}

func (h *HybridLogStore) ListUserAgentMappings(ctx context.Context, activeOnly bool) ([]UserAgentMapping, error) {
	return h.inner.ListUserAgentMappings(ctx, activeOnly)
}

// GetDistinctMetadataKeys delegates to the inner store and returns distinct
// metadata keys (and their distinct values) matching query, capped at limit.
func (h *HybridLogStore) GetDistinctMetadataKeys(ctx context.Context, limit int, query string) (map[string][]string, error) {
	return h.inner.GetDistinctMetadataKeys(ctx, limit, query)
}

// Async Job methods — delegated directly.

// CreateAsyncJob creates a new async job.
func (h *HybridLogStore) CreateAsyncJob(ctx context.Context, job *AsyncJob) error {
	return h.inner.CreateAsyncJob(ctx, job)
}

// FindAsyncJobByID finds an async job by its ID.
func (h *HybridLogStore) FindAsyncJobByID(ctx context.Context, id string) (*AsyncJob, error) {
	return h.inner.FindAsyncJobByID(ctx, id)
}

// UpdateAsyncJob updates an async job with the given ID using the provided updates.
func (h *HybridLogStore) UpdateAsyncJob(ctx context.Context, id string, updates map[string]interface{}) error {
	return h.inner.UpdateAsyncJob(ctx, id, updates)
}

// DeleteExpiredAsyncJobs deletes async jobs that have not been updated since the given time.
func (h *HybridLogStore) DeleteExpiredAsyncJobs(ctx context.Context) (int64, error) {
	return h.inner.DeleteExpiredAsyncJobs(ctx)
}

// DeleteStaleAsyncJobs deletes async jobs that have not been updated since the given time.
func (h *HybridLogStore) DeleteStaleAsyncJobs(ctx context.Context, staleSince time.Time) (int64, error) {
	return h.inner.DeleteStaleAsyncJobs(ctx, staleSince)
}

// SetDistributedLocker hands the locker to the inner store when it has use for
// one (ClickHouse); the SQL stores serialize with row locks instead.
func (h *HybridLogStore) SetDistributedLocker(locker DistributedLocker) {
	if lockable, ok := h.inner.(interface{ SetDistributedLocker(DistributedLocker) }); ok {
		lockable.SetDistributedLocker(locker)
	}
}

// Complexity suggestion methods - delegated directly. Suggestions, runs and
// pending vector deletes hold no payloads, so they stay in the database.

// UpsertComplexitySuggestions writes suggestions whole.
func (h *HybridLogStore) UpsertComplexitySuggestions(ctx context.Context, suggestions []ComplexitySuggestion) error {
	return h.inner.UpsertComplexitySuggestions(ctx, suggestions)
}

// ReconcileComplexitySuggestions writes a run's outcome without overwriting a
// decision made since the run read the rows.
func (h *HybridLogStore) ReconcileComplexitySuggestions(ctx context.Context, writes []ComplexitySuggestionWrite) ([]string, error) {
	return h.inner.ReconcileComplexitySuggestions(ctx, writes)
}

// ListComplexitySuggestions returns the selected suggestions.
func (h *HybridLogStore) ListComplexitySuggestions(ctx context.Context, filter ComplexitySuggestionFilter) ([]ComplexitySuggestion, error) {
	return h.inner.ListComplexitySuggestions(ctx, filter)
}

// CountComplexitySuggestionsByStatus counts suggestions per status.
func (h *HybridLogStore) CountComplexitySuggestionsByStatus(ctx context.Context, filter ComplexitySuggestionFilter) (map[string]int, error) {
	return h.inner.CountComplexitySuggestionsByStatus(ctx, filter)
}

// UpdateComplexitySuggestionStatus moves a suggestion between statuses.
func (h *HybridLogStore) UpdateComplexitySuggestionStatus(ctx context.Context, id, from, to string, resolvedAt *time.Time) (bool, error) {
	return h.inner.UpdateComplexitySuggestionStatus(ctx, id, from, to, resolvedAt)
}

// DeleteComplexitySuggestions removes suggestions outright.
func (h *HybridLogStore) DeleteComplexitySuggestions(ctx context.Context, ids []string) error {
	return h.inner.DeleteComplexitySuggestions(ctx, ids)
}

// UpsertComplexitySuggestionRun writes a run whole.
func (h *HybridLogStore) UpsertComplexitySuggestionRun(ctx context.Context, run *ComplexitySuggestionRun) error {
	return h.inner.UpsertComplexitySuggestionRun(ctx, run)
}

// InsertComplexitySuggestionRun writes a run unless one with its ID exists.
func (h *HybridLogStore) InsertComplexitySuggestionRun(ctx context.Context, run *ComplexitySuggestionRun) (bool, error) {
	return h.inner.InsertComplexitySuggestionRun(ctx, run)
}

// ListComplexitySuggestionRuns returns up to limit of the selected runs,
// newest first.
func (h *HybridLogStore) ListComplexitySuggestionRuns(ctx context.Context, filter ComplexitySuggestionRunFilter, limit int) ([]ComplexitySuggestionRun, error) {
	return h.inner.ListComplexitySuggestionRuns(ctx, filter, limit)
}

// UpdateComplexitySuggestionRunStatus ends a run, only from the given statuses.
func (h *HybridLogStore) UpdateComplexitySuggestionRunStatus(ctx context.Context, id string, from []string, to string, finishedAt *time.Time) (bool, error) {
	return h.inner.UpdateComplexitySuggestionRunStatus(ctx, id, from, to, finishedAt)
}

// PruneComplexitySuggestionRuns keeps one router's newest keep runs.
func (h *HybridLogStore) PruneComplexitySuggestionRuns(ctx context.Context, routerID string, keep int) (int64, error) {
	return h.inner.PruneComplexitySuggestionRuns(ctx, routerID, keep)
}

// DeleteComplexityRouterSuggestions removes a router's suggestions and runs.
func (h *HybridLogStore) DeleteComplexityRouterSuggestions(ctx context.Context, routerID string) error {
	return h.inner.DeleteComplexityRouterSuggestions(ctx, routerID)
}

// AddComplexityVectorDeletes records deleted logs whose captured vector delete
// failed.
func (h *HybridLogStore) AddComplexityVectorDeletes(ctx context.Context, rows []ComplexityVectorDelete) error {
	return h.inner.AddComplexityVectorDeletes(ctx, rows)
}

// ListComplexityVectorDeletes returns up to limit pending vector deletes.
func (h *HybridLogStore) ListComplexityVectorDeletes(ctx context.Context, limit int) ([]ComplexityVectorDelete, error) {
	return h.inner.ListComplexityVectorDeletes(ctx, limit)
}

// DeleteComplexityVectorDeletes removes exactly the given (log, namespace)
// rows.
func (h *HybridLogStore) DeleteComplexityVectorDeletes(ctx context.Context, rows []ComplexityVectorDelete) error {
	return h.inner.DeleteComplexityVectorDeletes(ctx, rows)
}

// ListComplexityReplayUsage reads the usage of the given logs.
func (h *HybridLogStore) ListComplexityReplayUsage(ctx context.Context, logIDs []string) ([]ComplexityReplayUsage, error) {
	return h.inner.ListComplexityReplayUsage(ctx, logIDs)
}

// EachComplexityRouterLog pages the requests a model router took; IDs and
// timestamps live in the index store.
func (h *HybridLogStore) EachComplexityRouterLog(ctx context.Context, routerID string, from, to time.Time, fn func([]ComplexityRouterLog) error) error {
	return h.inner.EachComplexityRouterLog(ctx, routerID, from, to, fn)
}

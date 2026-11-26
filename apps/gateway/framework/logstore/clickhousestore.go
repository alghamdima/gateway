package logstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// ClickHouseLogStore is a LogStore backed by ClickHouse. It embeds *RDBLogStore
// to reuse the (dialect-aware) analytics/read path and overrides only the
// methods ClickHouse cannot satisfy through plain GORM:
//
//   - inserts that relied on ON CONFLICT DO NOTHING (ClickHouse has no upsert;
//     idempotency comes from ReplacingMergeTree dedup + the connection-level
//     `final = 1` setting, so a plain INSERT is correct),
//   - row updates (ClickHouse has no cheap UPDATE; we read-modify-write and
//     re-insert, letting the `ver` DEFAULT now64() column make the newest
//     insert win on merge - see clickhousemigrate.go),
//   - every delete. The GORM ClickHouse driver rewrites Delete() into
//     `ALTER TABLE ... DELETE`, a heavyweight mutation that rewrites every
//     column of every part holding a matching row and leaves the old copy on
//     disk for old_parts_lifetime (#7098). All deletes here go through
//     chLightweightDelete, a raw `DELETE FROM ... WHERE` that only writes the
//     _row_exists mask. Never call GORM Delete() on this store.
//
// Table TTL (logs_store.retention_days) is the primary retention mechanism and
// is reconciled on every startup (clickhouseReconcileTTL); the LogsCleaner
// sweep is a single lightweight delete per run.
type ClickHouseLogStore struct {
	*RDBLogStore
	// cluster is the optional ON CLUSTER name (empty = single-node). Retained
	// for future cluster-aware DDL.
	cluster string
	// rmwLocks serializes read-modify-write cycles per row key within this
	// process. Because updates re-insert the whole row, two concurrent updaters
	// of the same id (e.g. object offload setting has_object while the
	// completion writer sets status/cost) would otherwise both read the same
	// base row and the higher `ver` would silently drop the other's patch.
	// Cross-pod races are not covered, but a given request id is only mutated
	// by the pod that processed it.
	rmwLocks [chRMWShards]sync.Mutex
	// locker serializes user agent mapping writes across
	// instances; rmwLocks only covers this process. Nil means single-instance
	// behaviour. Set once at startup, before any such write, so it is read
	// without synchronization.
	locker                 DistributedLocker
	idLookupIgnoredIndexes string   // non-id skip indexes on the id-read tables; see idLookupContext
	idLookupSkipIndexes    bool     // false when that list could not be read, so id reads never use skip indexes under FINAL
	retentionCutoffs       sync.Map // table -> cutoff of its last retention delete; see chRetentionDelete
}

// userAgentMappingLockKey is the shared lock for one user agent mapping, hashed
// because the id comes from the request path and the lock table caps keys at
// 255 characters.
func userAgentMappingLockKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "user_agent_mapping:" + hex.EncodeToString(sum[:])
}

// DistributedLocker is mutual exclusion shared by every Bifrost instance that
// uses the same stores. configstore.DistributedLockManager satisfies it; it is
// declared here, not imported, because configstore already imports logstore.
type DistributedLocker interface {
	// Acquire blocks until key is held or ctx ends. It returns a context to run
	// the critical section on, cancelled if the lock is lost while held, and the
	// release.
	Acquire(ctx context.Context, key string) (held context.Context, release func(), err error)
}

// SetDistributedLocker gives the user agent mapping writes a lock every
// instance sharing this ClickHouse honours.
func (s *ClickHouseLogStore) SetDistributedLocker(locker DistributedLocker) {
	s.locker = locker
}

// lockShared takes the shared lock for key, or does nothing without a locker.
// The returned context is cancelled if the lock is lost mid-write.
func (s *ClickHouseLogStore) lockShared(ctx context.Context, key string) (context.Context, func(), error) {
	if s.locker == nil {
		return ctx, func() {}, nil
	}
	return s.locker.Acquire(ctx, key)
}

// chRMWShards is the number of RMW lock shards; keys are hashed onto them.
const chRMWShards = 128

// chRMWBatchChunk bounds how many rows a single batch insert locks at once.
// A batch is written under lockRMWBatch across two network round trips (the
// existence filter, then the insert). With the default MaxBatchSize of 1000
// against 128 shards, one unchunked batch locks effectively every shard, so it
// behaves as a global lock and stalls every concurrent Update — the object-storage
// upload workers and deferred-usage updaters in particular. Chunking keeps the
// held-shard set small enough that those callers interleave.
const chRMWBatchChunk = 128

// forEachRMWChunk applies fn to successive slices of at most chRMWBatchChunk
// entries, stopping at the first error.
func forEachRMWChunk[T any](entries []T, fn func([]T) error) error {
	for start := 0; start < len(entries); start += chRMWBatchChunk {
		end := min(start+chRMWBatchChunk, len(entries))
		if err := fn(entries[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func chRMWShard(table, id string) int {
	h := fnv.New32a()
	h.Write([]byte(table))
	h.Write([]byte{0})
	h.Write([]byte(id))
	return int(h.Sum32() % chRMWShards)
}

// lockRMW locks the shard for a single row key and returns the unlock func.
func (s *ClickHouseLogStore) lockRMW(table, id string) func() {
	mu := &s.rmwLocks[chRMWShard(table, id)]
	mu.Lock()
	return mu.Unlock
}

// lockRMWBatch locks the distinct shards covering a set of row keys in
// ascending shard order (so concurrent batch lockers cannot deadlock) and
// returns the unlock func.
func (s *ClickHouseLogStore) lockRMWBatch(table string, ids []string) func() {
	seen := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		seen[chRMWShard(table, id)] = struct{}{}
	}
	shards := make([]int, 0, len(seen))
	for sh := range seen {
		shards = append(shards, sh)
	}
	sort.Ints(shards)
	for _, sh := range shards {
		s.rmwLocks[sh].Lock()
	}
	return func() {
		for i := len(shards) - 1; i >= 0; i-- {
			s.rmwLocks[shards[i]].Unlock()
		}
	}
}

// chSchemaCache is a shared GORM schema parse cache reused across RMW calls.
var chSchemaCache sync.Map

func chParseSchema(db *gorm.DB, model interface{}) (*schema.Schema, error) {
	return schema.Parse(model, &chSchemaCache, db.NamingStrategy)
}

// chImmutableColumns are the ReplacingMergeTree dedup key columns (the tables'
// ORDER BY is `(timestamp, id)` / `id` - see clickhousemigrate.go). Updates must
// never rewrite them: a reinserted row with a different key value would be a
// new logical row instead of replacing the old one, so the helpers below skip
// them the same way a SQL UPDATE never rewrites its WHERE key.
var chImmutableColumns = map[string]struct{}{
	"id":         {},
	"timestamp":  {},
	"inc_number": {}, // DB-assigned monotonic insert-order number; must survive re-inserts
}

// chApplyUpdateMap applies a column->value map onto a struct pointer using the
// GORM schema field setters (which handle pointer / typed conversions).
// Dedup key columns are skipped.
func chApplyUpdateMap(ctx context.Context, st *schema.Schema, dest reflect.Value, updates map[string]interface{}) error {
	for col, val := range updates {
		if _, immutable := chImmutableColumns[col]; immutable {
			continue
		}
		f, ok := st.FieldsByDBName[col]
		if !ok {
			continue
		}
		if err := f.Set(ctx, dest, val); err != nil {
			return fmt.Errorf("clickhouse: set column %s: %w", col, err)
		}
	}
	return nil
}

// chApplyStructUpdate overlays the non-zero fields of src onto dest, mirroring
// GORM's Updates(struct) semantics (zero-valued fields are not written).
// Dedup key columns are skipped.
func chApplyStructUpdate(ctx context.Context, st *schema.Schema, dest, src reflect.Value) error {
	for _, f := range st.Fields {
		if f.DBName == "" {
			continue
		}
		if _, immutable := chImmutableColumns[f.DBName]; immutable {
			continue
		}
		val, isZero := f.ValueOf(ctx, src)
		if isZero {
			continue
		}
		if err := f.Set(ctx, dest, val); err != nil {
			return fmt.Errorf("clickhouse: set column %s: %w", f.DBName, err)
		}
	}
	return nil
}

// chReinsert re-inserts a (possibly patched) row with hooks skipped so the
// BeforeCreate serialization does not clobber already-serialized base columns.
// The omitted `ver` column defaults to now64(), so this insert supersedes the
// prior version on the next ReplacingMergeTree merge (and immediately under
// `final = 1` reads).
func (s *ClickHouseLogStore) chReinsert(ctx context.Context, v interface{}) error {
	return s.db.WithContext(ctx).Session(&gorm.Session{SkipHooks: true}).Create(v).Error
}

// --- Inserts (existence check first; RMT dedup is last-write-wins) ---

// ReplacingMergeTree keeps the row with the HIGHEST `ver`, so a duplicate
// INSERT would *replace* the existing row instead of being a no-op like the
// SQL stores' ON CONFLICT DO NOTHING. That inverts CreateIfNotExists
// semantics: a retried "processing" insert arriving after the completion
// update (and after the hybrid store's has_object flip) would resurrect the
// stale row and silently drop status/cost/has_object. The methods below
// therefore check existence under the RMW shard locks and insert only rows
// whose id is not already present.

// chFilterMissing returns the entries whose id is not present in table,
// skipping nil entries and duplicate ids within the batch (first occurrence
// wins, matching ON CONFLICT DO NOTHING). Must be called under the RMW locks
// covering ids so a concurrent Update re-insert cannot interleave.
//
// When every entry carries a non-zero timestamp, the lookup is bounded to the
// batch's [min, max] timestamp range so it prunes granules via the
// (timestamp, id) primary key instead of scanning the id column. This is safe
// because retried creates reuse the original entry (same timestamp), and the
// tables' dedup key is (timestamp, id) anyway - a same-id row at a different
// timestamp would be a distinct logical row regardless of this check.
//
// The window is per-call, so it cannot see a row an earlier chunk of the same
// logical batch inserted at a different timestamp. Chunked callers therefore
// carry their own batch-wide seen-id set and never hand the same id to two
// calls; this function's duplicate skipping only covers one call's entries.
func chFilterMissing[T any](ctx context.Context, db *gorm.DB, table string, entries []*T, idOf func(*T) string, tsOf func(*T) time.Time) ([]*T, error) {
	ids := make([]string, 0, len(entries))
	var minTS, maxTS time.Time
	boundable := true
	for _, e := range entries {
		if e == nil {
			continue
		}
		ids = append(ids, idOf(e))
		ts := tsOf(e)
		if ts.IsZero() {
			boundable = false
			continue
		}
		if minTS.IsZero() || ts.Before(minTS) {
			minTS = ts
		}
		if maxTS.IsZero() || ts.After(maxTS) {
			maxTS = ts
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	q := db.WithContext(ctx).Table(table).Where("id IN ?", ids)
	if boundable {
		// Bind epoch millis, not time.Time: the GORM ClickHouse driver formats
		// time args as toDateTime('...') at SECONDS precision, silently dropping
		// the sub-second part - a BETWEEN on the raw values would miss every row
		// whose DateTime64(3) timestamp has a non-zero millisecond component.
		q = q.Where("timestamp BETWEEN fromUnixTimestamp64Milli(?) AND fromUnixTimestamp64Milli(?)", minTS.UnixMilli(), maxTS.UnixMilli())
	}
	var existing []string
	if err := q.Pluck("id", &existing).Error; err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(existing))
	for _, id := range existing {
		seen[id] = struct{}{}
	}
	missing := make([]*T, 0, len(entries))
	for _, e := range entries {
		if e == nil {
			continue
		}
		id := idOf(e)
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		missing = append(missing, e)
	}
	return missing, nil
}

// CreateIfNotExists inserts a log entry only when no row with the same id
// exists (see the semantics note above).
func (s *ClickHouseLogStore) CreateIfNotExists(ctx context.Context, entry *Log) error {
	if entry == nil {
		return fmt.Errorf("log entry is nil")
	}
	defer s.lockRMW("logs", entry.ID)()
	missing, err := chFilterMissing(ctx, s.db, "logs", []*Log{entry}, func(l *Log) string { return l.ID }, func(l *Log) time.Time { return l.Timestamp })
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return nil
	}
	// Omit inc_number so ClickHouse's DEFAULT generateSnowflakeID() fires.
	return s.db.WithContext(ctx).Omit("inc_number").Create(entry).Error
}

// BatchCreateIfNotExists inserts the log entries whose ids are not already
// present. See CreateIfNotExists.
func (s *ClickHouseLogStore) BatchCreateIfNotExists(ctx context.Context, entries []*Log) error {
	if len(entries) == 0 {
		return nil
	}
	// Batch-wide, not chunk-local: chFilterMissing bounds its existence probe to
	// the chunk's timestamp window, so a repeat id carrying a different timestamp
	// in a later chunk would miss the row an earlier chunk inserted. Dedup on id
	// alone across the whole batch to match the SQL stores' ON CONFLICT (id) DO
	// NOTHING and keep the result independent of where chunk boundaries fall.
	seen := make(map[string]struct{}, len(entries))
	return forEachRMWChunk(entries, func(chunk []*Log) error {
		fresh := make([]*Log, 0, len(chunk))
		ids := make([]string, 0, len(chunk))
		for _, e := range chunk {
			if e == nil {
				continue
			}
			if _, ok := seen[e.ID]; ok {
				continue
			}
			seen[e.ID] = struct{}{}
			fresh = append(fresh, e)
			ids = append(ids, e.ID)
		}
		if len(fresh) == 0 {
			return nil
		}
		defer s.lockRMWBatch("logs", ids)()
		missing, err := chFilterMissing(ctx, s.db, "logs", fresh, func(l *Log) string { return l.ID }, func(l *Log) time.Time { return l.Timestamp })
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			return nil
		}
		// Omit inc_number so ClickHouse's DEFAULT generateSnowflakeID() fires.
		return s.db.WithContext(ctx).Omit("inc_number").Create(&missing).Error
	})
}

// BatchCreateMCPToolLogsIfNotExists inserts the MCP tool log entries whose
// ids are not already present. See CreateIfNotExists.
func (s *ClickHouseLogStore) BatchCreateMCPToolLogsIfNotExists(ctx context.Context, entries []*MCPToolLog) error {
	if len(entries) == 0 {
		return nil
	}
	// Batch-wide dedup for the same reason as BatchCreateIfNotExists above.
	seen := make(map[string]struct{}, len(entries))
	return forEachRMWChunk(entries, func(chunk []*MCPToolLog) error {
		fresh := make([]*MCPToolLog, 0, len(chunk))
		ids := make([]string, 0, len(chunk))
		for _, e := range chunk {
			if e == nil {
				continue
			}
			if _, ok := seen[e.ID]; ok {
				continue
			}
			seen[e.ID] = struct{}{}
			fresh = append(fresh, e)
			ids = append(ids, e.ID)
		}
		if len(fresh) == 0 {
			return nil
		}
		defer s.lockRMWBatch("mcp_tool_logs", ids)()
		missing, err := chFilterMissing(ctx, s.db, "mcp_tool_logs", fresh, func(l *MCPToolLog) string { return l.ID }, func(l *MCPToolLog) time.Time { return l.Timestamp })
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			return nil
		}
		// Omit inc_number so ClickHouse's DEFAULT generateSnowflakeID() fires.
		return s.db.WithContext(ctx).Omit("inc_number").Create(&missing).Error
	})
}

// --- Point lookups by id ---

// chLogTimestampWindow is how far either side of a FindByID timestamp hint the
// lookup searches. The hint is a JSON round trip of the row's DateTime64(3), so
// it is normally exact; the slack absorbs a client that rounded it to seconds.
const chLogTimestampWindow = time.Second

// chIDLookupTables are the tables whose rows are read back by id under FINAL.
var chIDLookupTables = []string{"logs", "mcp_tool_logs"}

// chMutableSkipIndexes lists, as an ignore_data_skipping_indices value, every
// skip index on chIDLookupTables except those on id. Names that do not exist on
// the queried table are ignored by ClickHouse, so one list serves all of them.
func chMutableSkipIndexes(ctx context.Context, db *gorm.DB) (string, error) {
	var names []string
	if err := db.WithContext(ctx).
		Raw("SELECT DISTINCT name FROM system.data_skipping_indices WHERE database = currentDatabase() AND table IN ? AND expr != 'id' ORDER BY name", chIDLookupTables).
		Scan(&names).Error; err != nil {
		return "", fmt.Errorf("clickhouse: read skip indexes: %w", err)
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "`" + chEscapeIdentifier(n) + "`"
	}
	return strings.Join(quoted, ","), nil
}

// idLookupContext lets a read whose selective predicate is the row id use the
// id bloom filters under the connection-level FINAL. Before 25.x ClickHouse
// ignores skip indexes under FINAL, because an index on a mutable column can
// prune the granule holding a row's newest version while keeping an older one.
// id is in every versioned table's dedup key and never rewritten
// (chImmutableColumns), so all versions of a row match its id together. Every
// other index stays off: the access scope and hidden-type predicates on these
// reads filter on columns an update can change (user_id, team_id, ...).
//
// WithSettings replaces any settings already on ctx rather than merging them,
// and the driver writes max_execution_time into the map, so it is built per call.
func (s *ClickHouseLogStore) idLookupContext(ctx context.Context) context.Context {
	if !s.idLookupSkipIndexes {
		return ctx
	}
	settings := clickhouse.Settings{"use_skip_indexes_if_final": 1}
	if s.idLookupIgnoredIndexes != "" {
		settings["ignore_data_skipping_indices"] = s.idLookupIgnoredIndexes
	}
	return clickhouse.Context(ctx, clickhouse.WithSettings(settings))
}

// FindByID overrides the RDB lookup. A bare `WHERE id = ?` cannot use the
// monthly partitions or the (timestamp, id) sort key, so without help
// ClickHouse reads every granule of logs under FINAL. A timestamp hint on ctx
// (LogTimestampContextKey) bounds the lookup to the granules around it; with no
// hint, or one that matches nothing, the id bloom filter does the pruning.
func (s *ClickHouseLogStore) FindByID(ctx context.Context, id string) (*Log, error) {
	idCtx := s.idLookupContext(ctx)
	var log Log
	if ts, ok := logTimestampFromContext(ctx); ok {
		err := s.scopedLogsDB(idCtx).
			Where("id = ?", id).
			// Epoch millis, not time.Time: the driver binds time args at second precision (see chFilterMissing).
			Where("timestamp BETWEEN fromUnixTimestamp64Milli(?) AND fromUnixTimestamp64Milli(?)",
				ts.Add(-chLogTimestampWindow).UnixMilli(), ts.Add(chLogTimestampWindow).UnixMilli()).
			First(&log).Error
		if err == nil {
			return s.withChildAggregates(ctx, log), nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}
	if err := s.scopedLogsDB(idCtx).Where("id = ?", id).First(&log).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.withChildAggregates(ctx, log), nil
}

// chChildCandidateLimit caps how many child rows withChildAggregates binds
// back into its FINAL rollup; a parent with more (a long realtime session)
// gets the unbounded rollup instead.
const chChildCandidateLimit = 10000

// withChildAggregates bounds the RDB child rollup. `parent_request_id IN ?`
// cannot use the sort key, and its bloom filter is unsafe under FINAL because
// the column can change between versions, so on 24.x the rollup read all of
// logs. Without FINAL that index can only over-match: every version that ever
// named this parent, including each child's newest, is found. Those rows' ids
// and timestamp range then bound the FINAL rollup, which still filters on
// parent_request_id after dedup; id and timestamp are immutable, so pruning on
// them cannot hide a newest version.
func (s *ClickHouseLogStore) withChildAggregates(ctx context.Context, log Log) *Log {
	var candidates []struct {
		ID        string
		Timestamp time.Time
	}
	noFinal := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"final": 0}))
	if err := s.db.WithContext(noFinal).Model(&Log{}).
		Select("id, timestamp").
		Where("parent_request_id = ? AND id <> parent_request_id", log.ID).
		Limit(chChildCandidateLimit + 1).
		Scan(&candidates).Error; err != nil {
		s.logger.Warn(fmt.Sprintf("logstore: child aggregates unavailable for log %s, returning it without them: %s", log.ID, err))
		return &log
	}
	if len(candidates) == 0 {
		return &log
	}
	if len(candidates) > chChildCandidateLimit {
		return s.RDBLogStore.withChildAggregates(ctx, log)
	}
	ids := make([]string, 0, len(candidates))
	lo, hi := candidates[0].Timestamp, candidates[0].Timestamp
	for _, c := range candidates {
		ids = append(ids, c.ID)
		if c.Timestamp.Before(lo) {
			lo = c.Timestamp
		}
		if c.Timestamp.After(hi) {
			hi = c.Timestamp
		}
	}
	narrow := func(db *gorm.DB) *gorm.DB {
		return db.Where("id IN ?", ids).
			Where("timestamp BETWEEN fromUnixTimestamp64Milli(?) AND fromUnixTimestamp64Milli(?)", lo.UnixMilli(), hi.UnixMilli())
	}
	rows := []Log{log}
	if err := s.attachChildAggregatesWhere(s.idLookupContext(ctx), rows, SearchFilters{}, narrow); err != nil {
		s.logger.Warn(fmt.Sprintf("logstore: child aggregates unavailable for log %s, returning it without them: %s", log.ID, err))
		return &log
	}
	return &rows[0]
}

// --- Updates (read-modify-write + re-insert) ---

// chFindForUpdate loads the row an RMW update patches. logs and mcp_tool_logs
// both sort by (timestamp, id), so a bare id read
// prunes only through the id bloom filter, and only where FINAL honours skip
// indexes; otherwise it reads every granule of the table. A timestamp hint on
// ctx (LogTimestampContextKey) bounds the read to the partition and granules
// around the row, the same way FindByID does. With no hint, or one that
// matches nothing, it falls back to the id alone.
func (s *ClickHouseLogStore) chFindForUpdate(ctx context.Context, dst any, id string) error {
	idCtx := s.idLookupContext(ctx)
	if ts, ok := logTimestampFromContext(ctx); ok {
		err := s.db.WithContext(idCtx).
			Where("id = ?", id).
			// Epoch millis, not time.Time: the driver binds time args at second precision (see chFilterMissing).
			Where("timestamp BETWEEN fromUnixTimestamp64Milli(?) AND fromUnixTimestamp64Milli(?)",
				ts.Add(-chLogTimestampWindow).UnixMilli(), ts.Add(chLogTimestampWindow).UnixMilli()).
			First(dst).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
	}
	if err := s.db.WithContext(idCtx).Where("id = ?", id).First(dst).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// Update applies an update (a column->value map, or a *Log/Log whose non-zero
// fields are written) to the log row by re-inserting a patched copy.
func (s *ClickHouseLogStore) Update(ctx context.Context, id string, entry any) error {
	st, err := chParseSchema(s.db, &Log{})
	if err != nil {
		return err
	}
	defer s.lockRMW("logs", id)()
	var existing Log
	if err := s.chFindForUpdate(ctx, &existing, id); err != nil {
		return err
	}
	dest := reflect.ValueOf(&existing).Elem()
	switch v := entry.(type) {
	case map[string]interface{}:
		if err := chApplyUpdateMap(ctx, st, dest, v); err != nil {
			return err
		}
	case *Log:
		if v == nil {
			return fmt.Errorf("clickhouse: nil *Log update")
		}
		if err := v.SerializeFields(); err != nil {
			return err
		}
		if err := chApplyStructUpdate(ctx, st, dest, reflect.ValueOf(v).Elem()); err != nil {
			return err
		}
	case Log:
		if err := v.SerializeFields(); err != nil {
			return err
		}
		if err := chApplyStructUpdate(ctx, st, dest, reflect.ValueOf(&v).Elem()); err != nil {
			return err
		}
	default:
		return fmt.Errorf("clickhouse: unsupported Update entry type %T", entry)
	}
	return s.chReinsert(ctx, &existing)
}

// HasObjectMark names a row whose payload object has been written.
type HasObjectMark struct {
	ID        string
	Timestamp time.Time // the row's timestamp; zero when unknown
}

// HasObjectMarkResult reports the marks MarkHasObject did not apply.
type HasObjectMarkResult struct {
	Missing []string // ids with no row
}

// MarkHasObject sets has_object on many rows of one offloaded table. Each chunk
// costs one read and at most one re-insert, where flipping rows one Update at a
// time costs a read and a single-row insert (a new part) per row. The read is
// bounded by the chunk's timestamp range, so it prunes on the (timestamp, id)
// sort key; ids it misses there (a wrong or zero timestamp) are read by id
// alone. Rows already flagged are not re-inserted.
func (s *ClickHouseLogStore) MarkHasObject(ctx context.Context, table string, marks []HasObjectMark) (HasObjectMarkResult, error) {
	switch table {
	case "logs":
		return chMarkHasObject(ctx, s, table, marks, func(r *Log) (string, *bool) { return r.ID, &r.HasObject })
	case "mcp_tool_logs":
		return chMarkHasObject(ctx, s, table, marks, func(r *MCPToolLog) (string, *bool) { return r.ID, &r.HasObject })
	default:
		return HasObjectMarkResult{}, fmt.Errorf("clickhouse: MarkHasObject: unsupported table %q", table)
	}
}

// chMarkHasObject is MarkHasObject for one table. fields returns a row's id
// and its has_object flag.
func chMarkHasObject[T any](ctx context.Context, s *ClickHouseLogStore, table string, marks []HasObjectMark, fields func(*T) (string, *bool)) (HasObjectMarkResult, error) {
	var result HasObjectMarkResult
	err := forEachRMWChunk(marks, func(chunk []HasObjectMark) error {
		ids := make([]string, 0, len(chunk))
		var lo, hi time.Time
		bounded := true
		for _, m := range chunk {
			ids = append(ids, m.ID)
			if m.Timestamp.IsZero() {
				bounded = false
				continue
			}
			if lo.IsZero() || m.Timestamp.Before(lo) {
				lo = m.Timestamp
			}
			if hi.IsZero() || m.Timestamp.After(hi) {
				hi = m.Timestamp
			}
		}
		defer s.lockRMWBatch(table, ids)()
		idCtx := s.idLookupContext(ctx)
		var rows []*T
		if bounded {
			// Epoch millis, not time.Time: the driver binds time args at second precision (see chFilterMissing).
			if err := s.db.WithContext(idCtx).Where("id IN ?", ids).
				Where("timestamp BETWEEN fromUnixTimestamp64Milli(?) AND fromUnixTimestamp64Milli(?)",
					lo.Add(-chLogTimestampWindow).UnixMilli(), hi.Add(chLogTimestampWindow).UnixMilli()).
				Find(&rows).Error; err != nil {
				return err
			}
		}
		found := make(map[string]struct{}, len(rows))
		for _, r := range rows {
			id, _ := fields(r)
			found[id] = struct{}{}
		}
		var rest []string
		for _, id := range ids {
			if _, ok := found[id]; !ok {
				rest = append(rest, id)
			}
		}
		if len(rest) > 0 {
			var more []*T
			if err := s.db.WithContext(idCtx).Where("id IN ?", rest).Find(&more).Error; err != nil {
				return err
			}
			for _, r := range more {
				id, _ := fields(r)
				found[id] = struct{}{}
			}
			rows = append(rows, more...)
		}
		byID := make(map[string]*T, len(rows))
		for _, r := range rows {
			id, _ := fields(r)
			byID[id] = r
		}
		applies := make(map[string]bool, len(rows))
		reported := make(map[string]struct{})
		for _, m := range chunk {
			if _, ok := byID[m.ID]; !ok {
				if _, seen := reported[m.ID]; !seen {
					result.Missing = append(result.Missing, m.ID)
					reported[m.ID] = struct{}{}
				}
				continue
			}
			applies[m.ID] = true
		}
		write := make([]*T, 0, len(applies))
		for id := range applies {
			r := byID[id]
			if _, flag := fields(r); !*flag {
				*flag = true
				write = append(write, r)
			}
		}
		if len(write) == 0 {
			return nil
		}
		return s.chReinsert(ctx, &write)
	})
	return result, err
}

// BulkUpdateCost backfills costs by reading each chunk of rows, patching cost,
// and re-inserting. Reading the full row is required because the re-insert must
// reproduce every column (the ReplacingMergeTree dedup key includes timestamp).
func (s *ClickHouseLogStore) BulkUpdateCost(ctx context.Context, updates map[string]CostUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	ids := make([]string, 0, len(updates))
	for id := range updates {
		ids = append(ids, id)
	}
	for start := 0; start < len(ids); start += bulkUpdateCostChunkSize {
		end := start + bulkUpdateCostChunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		if err := func() error {
			defer s.lockRMWBatch("logs", chunk)()
			var rows []*Log
			if err := s.db.WithContext(s.idLookupContext(ctx)).Where("id IN ?", chunk).Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) == 0 {
				return nil
			}
			for _, r := range rows {
				u := updates[r.ID]
				cost := u.Total
				r.Cost = &cost
				r.InputCost = u.Input
				r.OutputCost = u.Output
				r.AdditionalCost = u.Additional
			}
			return s.chReinsert(ctx, &rows)
		}(); err != nil {
			return err
		}
	}
	return nil
}

// BatchUpdate overlays each entry's non-zero fields onto its existing row via
// read-modify-write and re-insert, one chunk at a time under the RMW locks.
// It overrides the embedded RDB implementation, whose GORM UPDATE would be a
// heavyweight mutation here and would bypass rmwLocks. Several entries for one
// id are applied in batch order, as the RDB store applies them. Missing ids are
// skipped, matching the RDB semantics. content_summary is left as the row has
// it: the entry's own serialization would regenerate it from output only.
func (s *ClickHouseLogStore) BatchUpdate(ctx context.Context, entries []*Log) error {
	byID := make(map[string][]*Log, len(entries))
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry == nil || entry.ID == "" {
			continue
		}
		if _, seen := byID[entry.ID]; !seen {
			ids = append(ids, entry.ID)
		}
		byID[entry.ID] = append(byID[entry.ID], entry)
	}
	if len(ids) == 0 {
		return nil
	}
	st, err := chParseSchema(s.db, &Log{})
	if err != nil {
		return err
	}
	return forEachRMWChunk(ids, func(chunk []string) error {
		defer s.lockRMWBatch("logs", chunk)()
		var rows []*Log
		if err := s.db.WithContext(s.idLookupContext(ctx)).Where("id IN ?", chunk).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, r := range rows {
			for _, src := range byID[r.ID] {
				if err := src.SerializeFields(); err != nil {
					return err
				}
				src.ContentSummary = ""
				if err := chApplyStructUpdate(ctx, st, reflect.ValueOf(r).Elem(), reflect.ValueOf(src).Elem()); err != nil {
					return err
				}
				// The overlay skips zero values; the final visibility is always written.
				r.ContentHidden = src.ContentHidden
				if src.RealtimeInputCorrection != nil {
					r.ContentSummary = src.RealtimeInputCorrection.Summary
				}
			}
		}
		return s.chReinsert(ctx, &rows)
	})
}

// UpdateMCPToolLog applies an update to an MCP tool log row via read-modify-write.
func (s *ClickHouseLogStore) UpdateMCPToolLog(ctx context.Context, id string, entry any) error {
	st, err := chParseSchema(s.db, &MCPToolLog{})
	if err != nil {
		return err
	}
	defer s.lockRMW("mcp_tool_logs", id)()
	var existing MCPToolLog
	if err := s.chFindForUpdate(ctx, &existing, id); err != nil {
		return err
	}
	dest := reflect.ValueOf(&existing).Elem()
	switch v := entry.(type) {
	case map[string]interface{}:
		if err := chApplyUpdateMap(ctx, st, dest, v); err != nil {
			return err
		}
	case *MCPToolLog:
		if v == nil {
			return fmt.Errorf("clickhouse: nil *MCPToolLog update")
		}
		if err := v.SerializeFields(); err != nil {
			return err
		}
		if err := chApplyStructUpdate(ctx, st, dest, reflect.ValueOf(v).Elem()); err != nil {
			return err
		}
	case MCPToolLog:
		if err := v.SerializeFields(); err != nil {
			return err
		}
		if err := chApplyStructUpdate(ctx, st, dest, reflect.ValueOf(&v).Elem()); err != nil {
			return err
		}
	default:
		return fmt.Errorf("clickhouse: unsupported UpdateMCPToolLog entry type %T", entry)
	}
	return s.chReinsert(ctx, &existing)
}

// chLightweightDelete issues a ClickHouse lightweight DELETE through raw Exec,
// bypassing the GORM driver's rewrite of Delete() into a heavyweight
// `ALTER TABLE ... DELETE`. ClickHouse records it as
// `UPDATE _row_exists = 0 WHERE ...`: on wide parts only the _row_exists mask
// is written and every other column file is hardlinked, so one call costs a
// mask per affected part instead of a full part rewrite per call (#7098).
// lightweight_deletes_sync = 1 waits for the current replica only, matching
// the mutations_sync=1 the DSN sets for the remaining heavyweight mutations
// (the default 2 would block on every replica of a cluster). Requires
// ClickHouse 24.4+, where the setting was introduced.
//
// The driver never reports rows affected for mutations, so callers that need
// a count select it first (chDeleteWhere).
func (s *ClickHouseLogStore) chLightweightDelete(ctx context.Context, table, where string, args ...any) error {
	stmt := fmt.Sprintf("DELETE FROM `%s` WHERE %s SETTINGS lightweight_deletes_sync = 1", chEscapeIdentifier(table), where)
	return s.db.WithContext(ctx).Exec(stmt, args...).Error
}

// chCountWhere counts the logical rows matching where. The DSN-level final=1
// collapses ReplacingMergeTree versions, so the count matches what the SQL
// stores report for the same predicate (see the delete_logs_batch parity test).
func (s *ClickHouseLogStore) chCountWhere(ctx context.Context, table, where string, args ...any) (int64, error) {
	var count int64
	err := s.db.WithContext(ctx).
		Raw(fmt.Sprintf("SELECT count() FROM `%s` WHERE %s", chEscapeIdentifier(table), where), args...).
		Scan(&count).Error
	return count, err
}

// chExistsWhere reports whether any current row matches where. It runs under
// the connection-level final=1 so a superseded ReplacingMergeTree version (a
// log created as processing and later re-inserted as success) does not match;
// otherwise the minute sweep would issue a mutation on every run until the old
// version merged away, which on a large part can take hours. LIMIT 1 stops at
// the first hit.
func (s *ClickHouseLogStore) chExistsWhere(ctx context.Context, table, where string, args ...any) (bool, error) {
	var hits []uint8
	err := s.db.WithContext(ctx).
		Raw(fmt.Sprintf("SELECT 1 FROM `%s` WHERE %s LIMIT 1", chEscapeIdentifier(table), where), args...).
		Scan(&hits).Error
	return len(hits) > 0, err
}

// chDeleteWhere counts the rows matching where and, when there are any,
// removes them with a single lightweight delete. It returns the count so the
// cleaners can log and pace on an accurate number; when nothing matches no
// mutation is issued at all.
func (s *ClickHouseLogStore) chDeleteWhere(ctx context.Context, table, where string, args ...any) (int64, error) {
	count, err := s.chCountWhere(ctx, table, where, args...)
	if err != nil || count == 0 {
		return 0, err
	}
	if err := s.chLightweightDelete(ctx, table, where, args...); err != nil {
		return 0, err
	}
	return count, nil
}

// chFlushProcessing removes rows still marked processing that were created
// before since. It probes first so the once-a-minute sweep from the logging
// plugin issues no mutation on an idle table (the issue counted ~1,440
// heavyweight mutations per table per day from this path alone).
func (s *ClickHouseLogStore) chFlushProcessing(ctx context.Context, table string, since time.Time) error {
	const where = "status = 'processing' AND created_at < ?"
	exists, err := s.chExistsWhere(ctx, table, where, since)
	if err != nil || !exists {
		return err
	}
	return s.chLightweightDelete(ctx, table, where, since)
}

// UpdateUserAgentMapping re-inserts a patched copy of the mapping, since
// ReplacingMergeTree has no in-place UPDATE. The shared lock keeps a delete on
// another instance from landing between the read and the re-insert, which
// would bring the deleted mapping back.
func (s *ClickHouseLogStore) UpdateUserAgentMapping(ctx context.Context, id string, mapping *UserAgentMapping) error {
	ctx, release, err := s.lockShared(ctx, userAgentMappingLockKey(id))
	if err != nil {
		return fmt.Errorf("failed to acquire user agent mapping lock: %w", err)
	}
	defer release()
	defer s.lockRMW("user_agent_mappings", id)()
	var existing UserAgentMapping
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&existing).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return gorm.ErrRecordNotFound
		}
		return fmt.Errorf("failed to update user agent mapping: %w", err)
	}
	existing.Pattern = mapping.Pattern
	existing.MatchType = mapping.MatchType
	existing.App = mapping.App
	existing.Logo = mapping.Logo
	existing.LogoMime = mapping.LogoMime
	existing.IsActive = mapping.IsActive
	existing.UpdatedAt = time.Now().UTC()
	if err := s.chReinsert(ctx, &existing); err != nil {
		return fmt.Errorf("failed to update user agent mapping: %w", err)
	}
	return nil
}

// DeleteUserAgentMapping deletes a mapping with a lightweight delete. Mutations
// never report rows affected, so existence is checked first.
func (s *ClickHouseLogStore) DeleteUserAgentMapping(ctx context.Context, id string) error {
	ctx, release, err := s.lockShared(ctx, userAgentMappingLockKey(id))
	if err != nil {
		return fmt.Errorf("failed to acquire user agent mapping lock: %w", err)
	}
	defer release()
	defer s.lockRMW("user_agent_mappings", id)()
	exists, err := s.chExistsWhere(ctx, "user_agent_mappings", "id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to delete user agent mapping: %w", err)
	}
	if !exists {
		return gorm.ErrRecordNotFound
	}
	if err := s.chLightweightDelete(ctx, "user_agent_mappings", "id = ?", id); err != nil {
		return fmt.Errorf("failed to delete user agent mapping: %w", err)
	}
	return nil
}

// DeleteLogsBatch deletes every log older than cutoff with one lightweight
// delete per call. batchSize is ignored: a ClickHouse mutation costs the same
// per affected part whether it matches 100 rows or all of them, so batching
// by id would rewrite the same part once per batch (#7098). The returned
// count is selected before the delete because mutations never report rows
// affected. The LogsCleaner loop stops on a count above batchSize; otherwise
// it calls again, and chRetentionDelete answers that call with 0.
func (s *ClickHouseLogStore) DeleteLogsBatch(ctx context.Context, cutoff time.Time, _ int) (int64, error) {
	return s.chRetentionDelete(ctx, "logs", "created_at", cutoff)
}

// chRetentionDelete runs one retention delete per table and cutoff. The
// cleaner keeps calling until a batch deletes nothing, but one lightweight
// delete already removes every row before cutoff. lightweight_deletes_sync = 1
// only waits for the replica that ran it, so on a cluster a repeat count could
// reach a replica that has not applied it yet and issue the same mutation
// again. A pass uses one cutoff per table, so a repeat call for the cutoff
// already deleted returns 0 without a query. retentionCutoffs holds one
// time.Time per retention table.
func (s *ClickHouseLogStore) chRetentionDelete(ctx context.Context, table, column string, cutoff time.Time) (int64, error) {
	if last, ok := s.retentionCutoffs.Load(table); ok && last.(time.Time).Equal(cutoff) {
		return 0, nil
	}
	deleted, err := s.chDeleteWhere(ctx, table, column+" < ?", cutoff)
	if err != nil {
		return 0, err
	}
	s.retentionCutoffs.Store(table, cutoff)
	return deleted, nil
}

// DeleteLog deletes a log entry by id with a lightweight delete.
func (s *ClickHouseLogStore) DeleteLog(ctx context.Context, id string) error {
	return s.chLightweightDelete(ctx, "logs", "id = ?", id)
}

// DeleteLogs deletes multiple log entries by id with one lightweight delete.
func (s *ClickHouseLogStore) DeleteLogs(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return s.chLightweightDelete(ctx, "logs", "id IN ?", ids)
}

// DeleteMCPToolLogs deletes multiple MCP tool log entries by id with one
// lightweight delete.
func (s *ClickHouseLogStore) DeleteMCPToolLogs(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return s.chLightweightDelete(ctx, "mcp_tool_logs", "id IN ?", ids)
}

// Flush removes stale processing log rows. Overridden so the minute sweep is
// a probe plus at most one lightweight delete. The error text matches the
// SQL stores so the logging plugin's warnings are unchanged.
func (s *ClickHouseLogStore) Flush(ctx context.Context, since time.Time) error {
	if err := s.chFlushProcessing(ctx, "logs", since); err != nil {
		return fmt.Errorf("failed to cleanup old processing logs: %w", err)
	}
	return nil
}

// FlushMCPToolLogs removes stale processing MCP tool log rows. See Flush.
func (s *ClickHouseLogStore) FlushMCPToolLogs(ctx context.Context, since time.Time) error {
	if err := s.chFlushProcessing(ctx, "mcp_tool_logs", since); err != nil {
		return fmt.Errorf("failed to cleanup old processing MCP tool logs: %w", err)
	}
	return nil
}

// DeleteExpiredAsyncJobs deletes async jobs whose expiry has passed with one
// lightweight delete; the count is selected first because mutations never
// report rows affected.
func (s *ClickHouseLogStore) DeleteExpiredAsyncJobs(ctx context.Context) (int64, error) {
	return s.chDeleteWhere(ctx, "async_jobs", "expires_at IS NOT NULL AND expires_at < ?", time.Now().UTC())
}

// DeleteStaleAsyncJobs deletes processing jobs created before staleSince.
// See DeleteExpiredAsyncJobs.
func (s *ClickHouseLogStore) DeleteStaleAsyncJobs(ctx context.Context, staleSince time.Time) (int64, error) {
	return s.chDeleteWhere(ctx, "async_jobs", "status = 'processing' AND created_at < ?", staleSince)
}

// UpdateAsyncJob applies a column->value map to an async job row via
// read-modify-write.
func (s *ClickHouseLogStore) UpdateAsyncJob(ctx context.Context, id string, updates map[string]interface{}) error {
	st, err := chParseSchema(s.db, &AsyncJob{})
	if err != nil {
		return err
	}
	defer s.lockRMW("async_jobs", id)()
	var existing AsyncJob
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&existing).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	dest := reflect.ValueOf(&existing).Elem()
	if err := chApplyUpdateMap(ctx, st, dest, updates); err != nil {
		return err
	}
	return s.chReinsert(ctx, &existing)
}

// --- Complexity phrase suggestions ---
//
// The reads are inherited: plain SELECTs that the connection-level final = 1
// collapses to each row's newest version. The writes and deletes are
// overridden: a plain insert is the upsert, because each table is ordered by
// its identity and ReplacingMergeTree keeps the newest row written for it.

// UpsertComplexitySuggestions writes suggestions whole, replacing any row with
// the same ID.
func (s *ClickHouseLogStore) UpsertComplexitySuggestions(ctx context.Context, suggestions []ComplexitySuggestion) error {
	if len(suggestions) == 0 {
		return nil
	}
	if err := prepareComplexitySuggestions(suggestions, time.Now().UTC()); err != nil {
		return err
	}
	return chCreateInChunks(ctx, s, suggestions)
}

// chCreateInChunks inserts rows complexitySuggestionWriteBatch at a time, as
// the RDB store does, so one call never builds a single statement for all of
// them. It stops at the first failed chunk; the chunks before it stay
// written, which a retry of the whole call overwrites as the newest version.
func chCreateInChunks[T any](ctx context.Context, s *ClickHouseLogStore, rows []T) error {
	for chunk := range slices.Chunk(rows, complexitySuggestionWriteBatch) {
		if err := s.db.WithContext(ctx).Create(&chunk).Error; err != nil {
			return err
		}
	}
	return nil
}

// complexitySuggestionLockKey is the shared lock for one suggestion's status.
func complexitySuggestionLockKey(id string) string {
	return "model_routing_suggestion:" + id
}

// ReconcileComplexitySuggestions writes a run's outcome. New rows are
// inserted; an existing row is read, checked and written back whole under the
// suggestion's lock, as a status change is, only while it is still at the
// status the run read it at.
func (s *ClickHouseLogStore) ReconcileComplexitySuggestions(ctx context.Context, writes []ComplexitySuggestionWrite) ([]string, error) {
	inserts, updates, err := splitComplexitySuggestionWrites(writes)
	if err != nil {
		return nil, err
	}
	var skipped []string
	for _, w := range updates {
		written, err := s.writeComplexitySuggestionFrom(ctx, w)
		if err != nil {
			return skipped, err
		}
		if !written {
			skipped = append(skipped, w.Suggestion.ID)
		}
	}
	if len(inserts) > 0 {
		if err := chCreateInChunks(ctx, s, inserts); err != nil {
			return skipped, err
		}
	}
	return skipped, nil
}

func (s *ClickHouseLogStore) writeComplexitySuggestionFrom(ctx context.Context, w ComplexitySuggestionWrite) (bool, error) {
	held, release, err := s.lockShared(ctx, complexitySuggestionLockKey(w.Suggestion.ID))
	if err != nil {
		return false, fmt.Errorf("acquire suggestion lock: %w", err)
	}
	defer release()
	unlock := s.lockRMW("model_routing_suggestions", w.Suggestion.ID)
	defer unlock()
	rows, err := s.ListComplexitySuggestions(held, ComplexitySuggestionFilter{IDs: []string{w.Suggestion.ID}, OmitCentroids: true})
	if err != nil {
		return false, err
	}
	if len(rows) == 0 || rows[0].Status != w.From {
		return false, nil
	}
	written := []ComplexitySuggestion{w.Suggestion}
	if err := s.db.WithContext(held).Create(&written).Error; err != nil {
		return false, err
	}
	return true, nil
}

// UpdateComplexitySuggestionStatus moves a suggestion between statuses.
// ClickHouse has no conditional update, so the row is read, checked and
// written back whole under the suggestion's lock: the shared one when a
// locker is set, so decisions on any instance are serialized, and this
// process's row lock. Of two decisions read from the same status only the
// first lands. A run's writes to existing rows take the same lock (see
// ReconcileComplexitySuggestions), so a decision and a run never both land on
// the status they read.
func (s *ClickHouseLogStore) UpdateComplexitySuggestionStatus(ctx context.Context, id, from, to string, resolvedAt *time.Time) (bool, error) {
	held, release, err := s.lockShared(ctx, complexitySuggestionLockKey(id))
	if err != nil {
		return false, fmt.Errorf("acquire suggestion lock: %w", err)
	}
	defer release()
	unlock := s.lockRMW("model_routing_suggestions", id)
	defer unlock()
	rows, err := s.ListComplexitySuggestions(held, ComplexitySuggestionFilter{IDs: []string{id}})
	if err != nil {
		return false, err
	}
	if len(rows) == 0 || rows[0].Status != from {
		return false, nil
	}
	row := rows[0]
	row.Status, row.ResolvedAt = to, resolvedAt
	written := []ComplexitySuggestion{row}
	if err := prepareComplexitySuggestions(written, time.Now().UTC()); err != nil {
		return false, err
	}
	if err := s.db.WithContext(held).Create(&written).Error; err != nil {
		return false, err
	}
	return true, nil
}

// chDeleteChunk bounds the ids one delete names, which bounds the
// statement's text below max_query_size (256 KiB by default).
const chDeleteChunk = 1000

// DeleteComplexitySuggestions removes suggestions outright.
func (s *ClickHouseLogStore) DeleteComplexitySuggestions(ctx context.Context, ids []string) error {
	for chunk := range slices.Chunk(ids, chDeleteChunk) {
		if err := s.chLightweightDelete(ctx, "model_routing_suggestions", "id IN ?", chunk); err != nil {
			return err
		}
	}
	return nil
}

// complexitySuggestionRunLockKey is the shared lock for one run's row.
func complexitySuggestionRunLockKey(id string) string {
	return "model_routing_suggestion_run:" + id
}

// UpsertComplexitySuggestionRun writes a run whole, replacing any row with the
// same ID. It takes the run's locks, as UpdateComplexitySuggestionRunStatus
// does, so a conditional status change is never interleaved with a write of
// the same run.
func (s *ClickHouseLogStore) UpsertComplexitySuggestionRun(ctx context.Context, run *ComplexitySuggestionRun) error {
	held, release, err := s.lockShared(ctx, complexitySuggestionRunLockKey(run.ID))
	if err != nil {
		return fmt.Errorf("acquire suggestion run lock: %w", err)
	}
	defer release()
	unlock := s.lockRMW("model_routing_suggestion_runs", run.ID)
	defer unlock()
	if err := prepareComplexitySuggestionRun(run, time.Now().UTC()); err != nil {
		return err
	}
	return s.db.WithContext(held).Create(run).Error
}

// UpdateComplexitySuggestionRunStatus ends a run. ClickHouse has no
// conditional update, so the row is read, checked and written back whole
// under the run's locks: the shared one when a locker is set, so it is
// serialized with every instance's writes of the run, and this process's row
// lock. Every write of a run takes the same locks, so an outcome the run
// wrote first is never overwritten.
func (s *ClickHouseLogStore) UpdateComplexitySuggestionRunStatus(ctx context.Context, id string, from []string, to string, finishedAt *time.Time) (bool, error) {
	if len(from) == 0 {
		return false, nil
	}
	held, release, err := s.lockShared(ctx, complexitySuggestionRunLockKey(id))
	if err != nil {
		return false, fmt.Errorf("acquire suggestion run lock: %w", err)
	}
	defer release()
	unlock := s.lockRMW("model_routing_suggestion_runs", id)
	defer unlock()
	runs, err := s.ListComplexitySuggestionRuns(held, ComplexitySuggestionRunFilter{IDs: []string{id}}, 1)
	if err != nil {
		return false, err
	}
	if len(runs) == 0 || !slices.Contains(from, runs[0].Status) {
		return false, nil
	}
	run := runs[0]
	run.Status, run.Stage, run.FinishedAt = to, "", finishedAt
	if err := prepareComplexitySuggestionRun(&run, time.Now().UTC()); err != nil {
		return false, err
	}
	if err := s.db.WithContext(held).Create(&run).Error; err != nil {
		return false, err
	}
	return true, nil
}

// InsertComplexitySuggestionRun writes a run unless one with its ID exists.
// ClickHouse has no insert-if-absent, so the run is looked up and written
// under its lock: the shared one when a locker is set, so registrations on
// any instance are serialized, and this process's row lock.
func (s *ClickHouseLogStore) InsertComplexitySuggestionRun(ctx context.Context, run *ComplexitySuggestionRun) (bool, error) {
	if err := prepareComplexitySuggestionRun(run, time.Now().UTC()); err != nil {
		return false, err
	}
	held, release, err := s.lockShared(ctx, complexitySuggestionRunLockKey(run.ID))
	if err != nil {
		return false, fmt.Errorf("acquire suggestion run lock: %w", err)
	}
	defer release()
	unlock := s.lockRMW("model_routing_suggestion_runs", run.ID)
	defer unlock()
	existing, err := s.ListComplexitySuggestionRuns(held, ComplexitySuggestionRunFilter{IDs: []string{run.ID}}, 1)
	if err != nil {
		return false, err
	}
	if len(existing) > 0 {
		return false, nil
	}
	if err := s.db.WithContext(held).Create(run).Error; err != nil {
		return false, err
	}
	return true, nil
}

// PruneComplexitySuggestionRuns keeps one router's newest keep runs, with
// one lightweight delete.
func (s *ClickHouseLogStore) PruneComplexitySuggestionRuns(ctx context.Context, routerID string, keep int) (int64, error) {
	if keep <= 0 {
		return 0, fmt.Errorf("keep must be positive")
	}
	where, args, err := complexitySuggestionRunPruneWhere(ctx, s.db, routerID, keep)
	if err != nil || where == "" {
		return 0, err
	}
	return s.chDeleteWhere(ctx, "model_routing_suggestion_runs", where, args...)
}

// DeleteComplexityRouterSuggestions removes a router's suggestions and runs,
// with one lightweight delete each. There is no transaction to undo the first
// with, so a failure on the second leaves the runs, which a retry removes.
func (s *ClickHouseLogStore) DeleteComplexityRouterSuggestions(ctx context.Context, routerID string) error {
	if routerID == "" {
		return fmt.Errorf("router id is required")
	}
	if err := s.chLightweightDelete(ctx, "model_routing_suggestions", "router_id = ?", routerID); err != nil {
		return err
	}
	return s.chLightweightDelete(ctx, "model_routing_suggestion_runs", "router_id = ?", routerID)
}

// AddComplexityVectorDeletes records deleted logs whose captured vector delete
// failed. A (log, namespace) recorded twice is one row once the table
// merges.
func (s *ClickHouseLogStore) AddComplexityVectorDeletes(ctx context.Context, rows []ComplexityVectorDelete) error {
	if len(rows) == 0 {
		return nil
	}
	if err := prepareComplexityVectorDeletes(rows, time.Now()); err != nil {
		return err
	}
	return chCreateInChunks(ctx, s, rows)
}

// DeleteComplexityVectorDeletes removes exactly the given (log, namespace)
// rows.
func (s *ClickHouseLogStore) DeleteComplexityVectorDeletes(ctx context.Context, rows []ComplexityVectorDelete) error {
	for chunk := range slices.Chunk(rows, chDeleteChunk) {
		where, args := complexityVectorDeleteWhere(chunk)
		if err := s.chLightweightDelete(ctx, "model_routing_vector_deletes", where, args...); err != nil {
			return err
		}
	}
	return nil
}

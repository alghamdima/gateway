package logstore

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

const (
	cleanupInterval = 24 * time.Hour
	// postgresCleanupInterval is shorter because only the replica holding a table's retention lock does any work.
	postgresCleanupInterval = time.Hour
	defaultRetentionDays    = 365

	// batchSize is how many expired rows one retention statement deletes. At
	// ~5.6M expired rows per day, 100-row batches under a 30-minute cap could
	// never catch up; 5000 keeps each statement short (one index range walk,
	// a bounded WAL burst) while finishing a day of expiry in about a thousand
	// statements.
	batchSize = 5000

	// cleanupRunCeiling bounds one retention pass. The pass normally runs until
	// no expired rows remain; the ceiling only stops a pathological backlog
	// (for example after retention was shortened) from holding a connection
	// indefinitely. The next run continues where this one stopped.
	cleanupRunCeiling = 6 * time.Hour

	// cleanupProgressEvery is how many batches pass between Debug progress lines.
	cleanupProgressEvery = 100

	// cleanupGrowAfter is how many batches in a row must succeed before a shrunk batch size doubles again.
	cleanupGrowAfter = 20
)

// LogRetentionManager defines the interface for managing log retention and deletion
type LogRetentionManager interface {
	DeleteLogsBatch(ctx context.Context, cutoff time.Time, batchSize int) (deletedCount int64, err error)
}

// RetentionScheduler is how the cleaner reads a store's retention interval; every LogStore implements it.
type RetentionScheduler interface {
	RetentionInterval() time.Duration
}

// CleanerConfig holds configuration for the log cleaner
type CleanerConfig struct {
	RetentionDays int
}

// LogsCleaner manages the cleanup of old logs
type LogsCleaner struct {
	manager     LogRetentionManager
	config      CleanerConfig
	logger      schemas.Logger
	stopCleanup chan struct{}
	mu          sync.Mutex
	// batchSizes keeps each table's batch size after a statement timeout shrank it, so later passes start there.
	batchSizes map[string]int
}

// NewLogsCleaner creates a new LogsCleaner instance
func NewLogsCleaner(manager LogRetentionManager, config CleanerConfig, logger schemas.Logger) *LogsCleaner {
	return &LogsCleaner{
		manager: manager,
		config:  config,
		logger:  logger,
	}
}

// RetentionDays is the retention period the next cleanup pass will use.
func (c *LogsCleaner) RetentionDays() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.config.RetentionDays
}

// UpdateRetentionDays updates the retention period used by subsequent cleanup
// passes. A pass already in progress keeps its cutoff for both log tables.
func (c *LogsCleaner) UpdateRetentionDays(retentionDays int) {
	c.mu.Lock()
	c.config.RetentionDays = retentionDays
	c.mu.Unlock()
}

// StartCleanupRoutine starts a goroutine that periodically cleans up old logs
func (c *LogsCleaner) StartCleanupRoutine() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Return early if already running
	if c.stopCleanup != nil {
		c.logger.Debug("log cleanup routine already running")
		return
	}

	c.stopCleanup = make(chan struct{})
	stopCh := c.stopCleanup

	go func() {
		// At the beginning, we will cleanup the logs
		c.runCleanupPass(stopCh)
		// Calculate initial delay with jitter
		timer := time.NewTimer(c.nextRunDuration())
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				// Run cleanup
				c.runCleanupPass(stopCh)

				// Reset timer with new jitter for next run
				timer.Reset(c.nextRunDuration())

			case <-stopCh:
				c.logger.Info("log cleanup routine stopped")
				return
			}
		}
	}()
	c.logger.Info("log cleanup routine started")
}

// StopCleanupRoutine gracefully stops the cleanup goroutine
func (c *LogsCleaner) StopCleanupRoutine() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Return early if already stopped
	if c.stopCleanup == nil {
		c.logger.Debug("log cleanup routine already stopped")
		return
	}

	close(c.stopCleanup)
	c.stopCleanup = nil
}

// runCleanupPass runs one retention pass bounded by cleanupRunCeiling. Closing
// stopCh cancels the pass between batches so StopCleanupRoutine does not wait
// for a long backlog to drain.
func (c *LogsCleaner) runCleanupPass(stopCh <-chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupRunCeiling)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-done:
		}
	}()
	c.cleanupOldLogs(ctx)
}

// cleanupOldLogs deletes logs older than the retention period, in batches, until no
// expired rows remain or ctx ends.
func (c *LogsCleaner) cleanupOldLogs(ctx context.Context) {
	c.mu.Lock()
	retentionDays := c.config.RetentionDays
	c.mu.Unlock()
	if retentionDays < 1 {
		retentionDays = defaultRetentionDays
	}

	// Calculate cutoff time
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)
	c.logger.Info("starting log cleanup: deleting logs older than %s (retention: %d days)", cutoff.Format(time.RFC3339), retentionDays)

	c.drainExpired(ctx, "logs", cutoff, c.manager.DeleteLogsBatch)
}

// drainExpired calls deleteBatch until it deletes nothing, another replica holds the table, or ctx ends.
// It returns false when the table stopped early (cancellation or an error).
func (c *LogsCleaner) drainExpired(ctx context.Context, label string, cutoff time.Time, deleteBatch func(context.Context, time.Time, int) (int64, error)) bool {
	totalDeleted := int64(0)
	batchCount := 0
	size := c.batchSizeFor(label)
	streak := 0

	for {
		// Check if context is cancelled
		select {
		case <-ctx.Done():
			c.logger.Warn("%s cleanup stopped after %d rows in %d batches: %v", label, totalDeleted, batchCount, ctx.Err())
			return false
		default:
		}

		// A DELETE queued ahead of another node's pending ALTER TABLE stalls that
		// node's migration; hold off between batches until the lock is released.
		if !c.waitForMigrationWindow(ctx) {
			c.logger.Warn("%s cleanup stopped after %d rows in %d batches: %v", label, totalDeleted, batchCount, ctx.Err())
			return false
		}

		deleted, err := deleteBatch(ctx, cutoff, size)
		if errors.Is(err, ErrRetentionBusy) {
			c.logger.Debug("%s cleanup yielded to another replica after %d rows in %d batches", label, totalDeleted, batchCount)
			return true
		}
		// A timed-out batch rolls back whole and the next pass picks the same rows, so retry those rows in a smaller batch.
		if err != nil && ctx.Err() == nil && size > 1 && IsPostgresStatementTimeoutError(err) {
			size = max(size/2, 1)
			streak = 0
			c.setBatchSize(label, size)
			c.logger.Warn("%s cleanup batch hit the statement timeout; retrying with %d rows per batch", label, size)
			continue
		}
		if err != nil {
			c.logger.Error("failed to delete old %s: %v", label, err)
			return false
		}

		if deleted == 0 {
			// No more logs to delete
			break
		}

		totalDeleted += deleted
		batchCount++
		if batchCount%cleanupProgressEvery == 0 {
			c.logger.Debug("%s cleanup progress: %d rows deleted in %d batches", label, totalDeleted, batchCount)
		}

		// A short batch can mean another session took some of the picked rows, so only an empty batch ends the drain.
		// ClickHouse deletes the whole expired range in one statement and returns a count above batchSize (#7098).
		if deleted > int64(size) {
			break
		}

		if size < batchSize {
			if streak++; streak >= cleanupGrowAfter {
				size = min(size*2, batchSize)
				streak = 0
				c.setBatchSize(label, size)
			}
		}
	}

	if totalDeleted > 0 {
		c.logger.Info("%s cleanup completed: deleted %d rows in %d batches", label, totalDeleted, batchCount)
	} else {
		c.logger.Debug("%s cleanup completed: nothing older than the cutoff", label)
	}
	return true
}

// batchSizeFor is label's batch size: batchSize unless a statement timeout shrank it.
func (c *LogsCleaner) batchSizeFor(label string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if size, ok := c.batchSizes[label]; ok {
		return size
	}
	return batchSize
}

func (c *LogsCleaner) setBatchSize(label string, size int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.batchSizes == nil {
		c.batchSizes = make(map[string]int)
	}
	c.batchSizes[label] = size
}

// cleanupMigrationPollInterval is how often a paused retention pass re-checks
// the migration lock.
var cleanupMigrationPollInterval = time.Second

// waitForMigrationWindow blocks while the manager reports a migration in
// progress, polling every cleanupMigrationPollInterval. It returns false only
// when ctx ends first, so the pass deadline still bounds the wait. A manager
// without a MaintenanceGate never pauses.
func (c *LogsCleaner) waitForMigrationWindow(ctx context.Context) bool {
	if !MigrationInProgressFor(ctx, c.manager) {
		return true
	}
	c.logger.Info("log cleanup paused: migration lock is held by another node")
	timer := time.NewTimer(cleanupMigrationPollInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}
		if !MigrationInProgressFor(ctx, c.manager) {
			c.logger.Info("log cleanup resumed: migration lock released")
			return true
		}
		timer.Reset(cleanupMigrationPollInterval)
	}
}

// nextRunDuration is the store's retention interval plus a jitter of 1/96 to 1/48 of it (15-30 minutes on a day).
func (c *LogsCleaner) nextRunDuration() time.Duration {
	interval := cleanupInterval
	if scheduler, ok := c.manager.(RetentionScheduler); ok && scheduler.RetentionInterval() > 0 {
		interval = scheduler.RetentionInterval()
	}
	return calculateNextRunDuration(interval)
}

// calculateNextRunDuration returns interval plus a random jitter between interval/96 and interval/48.
func calculateNextRunDuration(interval time.Duration) time.Duration {
	minJitter := interval / 96
	return interval + minJitter + time.Duration(rand.Int63n(int64(minJitter)+1))
}

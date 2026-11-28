package logstore

import (
	"testing"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// newCorrelationFilterStore builds an in-memory store over the real log table, so the filters run
// against the columns the migrations create rather than a struct literal.
func newCorrelationFilterStore(t *testing.T) *RDBLogStore {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Log{}))
	return &RDBLogStore{db: db, logger: bifrost.NewDefaultLogger(schemas.LogLevelInfo)}
}

// A correlation id is matched exactly: "agent-1" must not also find "agent-10".
func TestCorrelationFiltersAreExactScalars(t *testing.T) {
	s := newCorrelationFilterStore(t)

	for _, row := range []struct{ id, session, agent string }{
		{"one", "session-1", "agent-1"},
		{"two", "session-10", "agent-10"},
	} {
		session, agent := row.session, row.agent
		require.NoError(t, s.db.Create(&Log{ID: row.id, Status: "success", SessionID: &session, AgentCorrelationID: &agent}).Error)
	}

	var llmCount int64
	require.NoError(t, s.applyFilters(s.db.Model(&Log{}), SearchFilters{AgentCorrelationID: "agent-1"}).Count(&llmCount).Error)
	assert.EqualValues(t, 1, llmCount)
}

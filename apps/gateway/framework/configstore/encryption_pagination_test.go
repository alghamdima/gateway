package configstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The startup backfill walks each table by primary key. These tests pin the two
// properties that walk depends on: it must visit every matching row across batch
// boundaries, and it must terminate even when a row's status never advances.

// TestEncryptPlaintextKeys_WalksEveryRowPastBatchSize covers the same walk on a
// table with an integer primary key, where the cursor is numeric rather than
// lexical.
func TestEncryptPlaintextKeys_WalksEveryRowPastBatchSize(t *testing.T) {
	store, db := setupEncryptionTestStore(t)
	const total = encryptionBatchSize + 23
	now := time.Now()
	for i := 0; i < total; i++ {
		insertPlaintextRow(t, db,
			`INSERT INTO config_keys (name, provider_id, provider, key_id, value, encryption_status, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, 'plain_text', ?, ?)`,
			fmt.Sprintf("key-%05d", i), 1, "openai", fmt.Sprintf("key-id-%05d", i),
			fmt.Sprintf("sk-plaintext-%d", i), now, now)
	}

	count, err := store.encryptPlaintextKeys(context.Background())
	require.NoError(t, err)
	assert.Equal(t, total, count)

	var remaining int64
	require.NoError(t, db.Table("config_keys").
		Where("encryption_status = ? OR encryption_status IS NULL OR encryption_status = ''", encryptionStatusPlainText).
		Count(&remaining).Error)
	assert.Zero(t, remaining)

	var found tables.TableKey
	require.NoError(t, db.Where("key_id = ?", fmt.Sprintf("key-id-%05d", total-1)).First(&found).Error)
	assert.Equal(t, fmt.Sprintf("sk-plaintext-%d", total-1), found.Value.GetValue())
}

package configstore

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// The startup backfill walks each table by primary key. These tests pin the two
// properties that walk depends on: it must visit every matching row across batch
// boundaries, and it must terminate even when a row's status never advances.

// oauthConfigID returns a zero-padded id so lexical and insertion order agree,
// making batch boundaries predictable.
func oauthConfigID(i int) string { return fmt.Sprintf("oauth-cfg-%05d", i) }

// seedPlaintextOAuthConfigs inserts n oauth_configs rows via raw SQL so the
// BeforeSave hook never runs and the rows stay unencrypted.
func seedPlaintextOAuthConfigs(t *testing.T, db *gorm.DB, n int, secretPrefix string, statuses ...string) {
	t.Helper()
	if len(statuses) == 0 {
		statuses = []string{encryptionStatusPlainText}
	}
	now := time.Now()
	for i := 0; i < n; i++ {
		status := statuses[i%len(statuses)]
		var statusArg any
		if status == "NULL" {
			statusArg = nil
		} else {
			statusArg = status
		}
		insertPlaintextRow(t, db,
			`INSERT INTO oauth_configs (id, client_secret, redirect_uri, status, encryption_status, created_at, updated_at)
			 VALUES (?, ?, ?, 'pending', ?, ?, ?)`,
			oauthConfigID(i), secretPrefix+fmt.Sprint(i), "https://example.com/cb", statusArg, now, now)
	}
}

// countOAuthConfigUpdates counts UPDATE statements issued against oauth_configs,
// so a test can tell "wrote each row once" from "kept rewriting the same rows".
func countOAuthConfigUpdates(t *testing.T, db *gorm.DB, n *atomic.Int64) {
	t.Helper()
	require.NoError(t, db.Callback().Update().After("gorm:update").
		Register("test:count_oauth_updates", func(tx *gorm.DB) {
			if tx.Statement != nil && tx.Statement.Table == "oauth_configs" {
				n.Add(1)
			}
		}))
}

func plaintextOAuthConfigCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Table("oauth_configs").
		Where("encryption_status = ? OR encryption_status IS NULL OR encryption_status = ''", encryptionStatusPlainText).
		Count(&n).Error)
	return n
}

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

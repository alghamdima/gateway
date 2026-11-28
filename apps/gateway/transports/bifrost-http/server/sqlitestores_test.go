package server

import (
	"context"
	"path/filepath"
	"testing"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/stretchr/testify/require"
)

// TestSQLiteStoresNamesEachStoreOnSQLite pins what the startup refusal reads: a config store
// opened on SQLite is found by its dialect even though a defaulted one records no config, and
// a log store by the config it was opened with. A config with neither names nothing.
func TestSQLiteStoresNamesEachStoreOnSQLite(t *testing.T) {
	SetLogger(bifrost.NewNoOpLogger())
	require.Empty(t, sqliteStores(nil))
	require.Empty(t, sqliteStores(&lib.Config{}))

	ctx := context.Background()
	store, err := configstore.NewConfigStore(ctx, &configstore.Config{
		Enabled: true,
		Type:    configstore.ConfigStoreTypeSQLite,
		Config:  &configstore.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")},
	}, logger)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close(ctx) })

	// the defaulted config store: opened, with no config recorded
	require.Equal(t, []string{"config store is"}, sqliteStores(&lib.Config{ConfigStore: store}))

	logsConfig := &logstore.Config{
		Enabled: true,
		Type:    logstore.LogStoreTypeSQLite,
		Config:  &logstore.SQLiteConfig{Path: filepath.Join(t.TempDir(), "logs.db")},
	}
	logs, err := logstore.NewLogStore(ctx, logsConfig, logger)
	require.NoError(t, err)
	t.Cleanup(func() { logs.Close(ctx) })

	require.Equal(t, []string{"config store is", "log store is"},
		sqliteStores(&lib.Config{ConfigStore: store, LogsStore: logs, LogsStoreConfig: logsConfig}))

	// a log store on another database is not named
	postgres := &logstore.Config{Enabled: true, Type: logstore.LogStoreTypePostgres}
	require.Empty(t, sqliteStores(&lib.Config{LogsStore: logs, LogsStoreConfig: postgres}))
}

// TestRefuseSQLiteUnlessAllowed pins the rule itself: a gateway with a store on SQLite does
// not start, the error says which store and how to fix it, and the test-only switch lets it by.
func TestRefuseSQLiteUnlessAllowed(t *testing.T) {
	SetLogger(bifrost.NewNoOpLogger())
	ctx := context.Background()
	store, err := configstore.NewConfigStore(ctx, &configstore.Config{
		Enabled: true,
		Type:    configstore.ConfigStoreTypeSQLite,
		Config:  &configstore.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")},
	}, logger)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close(ctx) })
	config := &lib.Config{ConfigStore: store}

	err = refuseSQLite(config, false)
	require.ErrorContains(t, err, "config store is on SQLite")
	require.ErrorContains(t, err, "postgres in config.json")
	require.ErrorContains(t, err, "-allow-sqlite")

	require.NoError(t, refuseSQLite(config, true))
	require.NoError(t, refuseSQLite(&lib.Config{}, false), "nothing on SQLite, nothing refused")
}

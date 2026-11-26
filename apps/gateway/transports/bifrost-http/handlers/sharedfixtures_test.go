package handlers

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// newRealConfigStore opens a SQLite config store in a temporary directory.
func newRealConfigStore(t testing.TB) configstore.ConfigStore {
	t.Helper()
	cs, err := configstore.NewConfigStore(context.Background(), &configstore.Config{
		Enabled: true,
		Type:    configstore.ConfigStoreTypeSQLite,
		Config:  &configstore.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")},
	}, &mockLogger{})
	require.NoError(t, err)
	require.NotNil(t, cs)
	return cs
}

// newTestConfig builds a config over store with the given inference-auth setting.
func newTestConfig(store configstore.ConfigStore, enforceAuth bool) *lib.Config {
	return &lib.Config{
		ConfigStore:  store,
		ClientConfig: &configstore.ClientConfig{EnforceAuthOnInference: enforceAuth},
	}
}

func initCtx(req *fasthttp.Request) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(req, nil, nil)
	return ctx
}

func bgCtx() *fasthttp.RequestCtx {
	var req fasthttp.Request
	return initCtx(&req)
}

func getCtx(uri string) *fasthttp.RequestCtx {
	var req fasthttp.Request
	req.Header.SetMethod("GET")
	req.SetRequestURI(uri)
	return initCtx(&req)
}

func putConfigCtx(body string) *fasthttp.RequestCtx {
	var req fasthttp.Request
	req.Header.SetMethod("PUT")
	req.Header.SetContentType("application/json")
	req.SetBodyString(body)
	return initCtx(&req)
}

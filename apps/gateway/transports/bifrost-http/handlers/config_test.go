package handlers

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/network"
	"github.com/maximhq/bifrost/core/network/proxytest"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configtables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/encrypt"
	"github.com/maximhq/bifrost/framework/modelcatalog"
	"github.com/maximhq/bifrost/plugins/compat"
	"github.com/maximhq/bifrost/plugins/governance"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
)

type inferenceSetupAccount struct {
	wsSpanTestAccount
	url string
}

func (a inferenceSetupAccount) GetConfigForProvider(p schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	c, err := a.wsSpanTestAccount.GetConfigForProvider(p)
	if err == nil {
		c.NetworkConfig.BaseURL = a.url
		c.NetworkConfig.AllowPrivateNetwork = true
	}
	return c, err
}

func TestUpdateConfig_InferenceAuthBlocksUpstream(t *testing.T) {
	SetLogger(&mockLogger{})
	store := newRealConfigStore(t)
	cfg := newTestConfig(store, false)
	h := &ConfigHandler{store: cfg, configManager: setupConfigManager{store: store, validToken: true}}
	setup := putConfigCtx(`{"client_config":{"log_retention_days":7},"auth_config":{"is_enabled":true,"admin_username":"admin","admin_password":"StrongPassword1!"}}`)
	h.updateConfig(setup)
	require.Equal(t, 200, setup.Response.StatusCode(), string(setup.Response.Body()))

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat/completions":
			fmt.Fprint(w, `{"id":"chat-test","object":"chat.completion","model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		case "/files":
			fmt.Fprint(w, `{"object":"list","data":[]}`)
		default:
			fmt.Fprint(w, `{"id":"resp_test","object":"response","status":"completed","model":"gpt-4o-mini","output":[]}`)
		}
	}))
	defer upstream.Close()
	log := bifrost.NewDefaultLogger(schemas.LogLevelError)
	plugin, err := governance.Init(t.Context(), &governance.Config{IsVkMandatory: &cfg.ClientConfig.EnforceAuthOnInference}, log, nil,
		&configstore.GovernanceConfig{VirtualKeys: []configtables.TableVirtualKey{{
			ID: "setup-vk", Name: "setup-vk", Value: *schemas.NewSecretVar("sk-bf-setup"), IsActive: new(true),
			ProviderConfigs: []configtables.TableVirtualKeyProviderConfig{{Provider: "openai", AllowedModels: schemas.WhiteList{"*"}, AllowAllKeys: true}},
		}}}, nil, nil)
	require.NoError(t, err)
	client, err := bifrost.Init(t.Context(), schemas.BifrostConfig{
		Account: inferenceSetupAccount{url: upstream.URL}, Logger: log, LLMPlugins: []schemas.LLMPlugin{plugin},
	})
	require.NoError(t, err)
	defer client.Shutdown()
	for _, operation := range []string{"chat", "files", "responses"} {
		for _, credential := range []string{"", "sk-bf-setup"} {
			t.Run(operation+"/"+credential, func(t *testing.T) {
				ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
				if credential != "" {
					ctx.SetValue(schemas.BifrostContextKeyVirtualKey, credential)
				}
				lib.SettleIdentity(ctx)
				before := calls.Load()
				var failure *schemas.BifrostError
				switch operation {
				case "chat":
					_, failure = client.ChatCompletionRequest(ctx, &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "gpt-4o-mini", Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("hi")}}}})
				case "files":
					_, failure = client.FileListRequest(ctx, &schemas.BifrostFileListRequest{Provider: schemas.OpenAI})
				case "responses":
					_, failure = client.ResponsesRetrieveRequest(ctx, &schemas.BifrostResponsesRetrieveRequest{Provider: schemas.OpenAI, ResponseID: "resp_test"})
				}
				if credential == "" {
					require.NotNil(t, failure)
					require.NotNil(t, failure.StatusCode)
					assert.Equal(t, 401, *failure.StatusCode)
					assert.Equal(t, "virtual_key_required", *failure.Type)
					assert.Equal(t, before, calls.Load(), "anonymous request must not reach upstream")
				} else {
					require.Nil(t, failure, "%+v", failure)
					assert.Equal(t, before+1, calls.Load())
				}
			})
		}
	}
}

type setupConfigManager struct {
	stubConfigManager
	store      configstore.ConfigStore
	validToken bool
}

type inferenceSetupFailureStore struct {
	configstore.ConfigStore
	fail string
}

func (s inferenceSetupFailureStore) GetAuthConfig(ctx context.Context) (*configstore.AuthConfig, error) {
	if s.fail == "read" {
		return nil, errors.New("auth lookup failed")
	}
	return s.ConfigStore.GetAuthConfig(ctx)
}
func (s inferenceSetupFailureStore) UpdateClientConfig(ctx context.Context, c *configstore.ClientConfig) error {
	if s.fail == "write" {
		return errors.New("client persistence failed")
	}
	return s.ConfigStore.UpdateClientConfig(ctx, c)
}

func TestUpdateConfig_InferenceAuthStoreFailures(t *testing.T) {
	SetLogger(&mockLogger{})
	for _, failure := range []string{"read", "write"} {
		t.Run(failure, func(t *testing.T) {
			store := newRealConfigStore(t)
			cfg := newTestConfig(inferenceSetupFailureStore{store, failure}, false)
			require.NoError(t, store.UpdateClientConfig(bgCtx(), cfg.ClientConfig))
			h := &ConfigHandler{store: cfg, configManager: setupConfigManager{store: store, validToken: true}}
			ctx := putConfigCtx(`{"client_config":{"log_retention_days":7},"auth_config":{"is_enabled":true,"admin_username":"admin","admin_password":"StrongPassword1!"}}`)
			h.updateConfig(ctx)
			require.Equal(t, 500, ctx.Response.StatusCode())
			persisted, err := store.GetClientConfig(bgCtx())
			require.NoError(t, err)
			assert.False(t, persisted.EnforceAuthOnInference)
			assert.False(t, cfg.ClientConfig.EnforceAuthOnInference)
			auth, err := store.GetAuthConfig(bgCtx())
			require.NoError(t, err)
			assert.Nil(t, auth)
		})
	}
}

func (m setupConfigManager) ValidateSetupToken(string) bool { return m.validToken }
func (m setupConfigManager) UpdateAuthConfig(ctx context.Context, auth *configstore.AuthConfig) error {
	return m.store.UpdateAuthConfig(ctx, auth)
}

func TestUpdateConfig_InferenceAuthSetup(t *testing.T) {
	SetLogger(&mockLogger{})
	for _, tt := range []struct {
		name, field, password               string
		existing, initial, validToken, want bool
		status                              int
	}{
		{"first omitted", "", "StrongPassword1!", false, false, true, true, 200},
		{"first opt out", `,"enforce_auth_on_inference":false`, "StrongPassword1!", false, false, true, false, 200},
		{"first explicit on", `,"enforce_auth_on_inference":true`, "StrongPassword1!", false, false, true, true, 200},
		{"existing on omitted", "", "StrongPassword1!", true, true, true, true, 200},
		{"existing off omitted", "", "StrongPassword1!", true, false, true, false, 200},
		{"invalid setup token", "", "StrongPassword1!", false, false, false, false, 403},
		{"invalid setup password", "", "weak", false, false, true, false, 400},
		{"unhashable setup password", "", strings.Repeat("StrongPassword1!", 10), false, false, true, false, 400},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := newRealConfigStore(t)
			cfg := newTestConfig(store, false)
			cfg.ClientConfig.EnforceAuthOnInference = tt.initial
			require.NoError(t, store.UpdateClientConfig(bgCtx(), cfg.ClientConfig))
			if tt.existing {
				require.NoError(t, store.UpdateAuthConfig(bgCtx(), &configstore.AuthConfig{
					AdminUserName: schemas.NewSecretVar("admin"), AdminPassword: schemas.NewSecretVar("stored"), IsEnabled: true,
				}))
			}
			h := &ConfigHandler{store: cfg, configManager: setupConfigManager{store: store, validToken: tt.validToken}}
			ctx := putConfigCtx(fmt.Sprintf(`{"client_config":{"log_retention_days":7%s},"auth_config":{"is_enabled":true,"admin_username":"admin","admin_password":%q}}`, tt.field, tt.password))
			h.updateConfig(ctx)
			require.Equal(t, tt.status, ctx.Response.StatusCode(), string(ctx.Response.Body()))
			persisted, err := store.GetClientConfig(bgCtx())
			require.NoError(t, err)
			assert.Equal(t, tt.want, persisted.EnforceAuthOnInference)
			assert.Equal(t, tt.want, cfg.ClientConfig.EnforceAuthOnInference)
			if tt.status == 200 {
				// An unrelated partial save must preserve the just-selected setting.
				save := putConfigCtx(`{"client_config":{"log_retention_days":8}}`)
				h.updateConfig(save)
				require.Equal(t, 200, save.Response.StatusCode(), string(save.Response.Body()))
				persisted, err = store.GetClientConfig(bgCtx())
				require.NoError(t, err)
				assert.Equal(t, tt.want, persisted.EnforceAuthOnInference)
				status := putConfigCtx("")
				(&SessionHandler{configStore: store}).isAuthEnabled(status)
				var reported struct {
					Inference *bool `json:"inference_auth_enforced"`
				}
				require.NoError(t, json.Unmarshal(status.Response.Body(), &reported))
				require.NotNil(t, reported.Inference)
				assert.Equal(t, tt.want, *reported.Inference)
			}
			if tt.status != 200 {
				auth, err := store.GetAuthConfig(bgCtx())
				require.NoError(t, err)
				assert.Nil(t, auth)
			}
		})
	}
}

// TestUpdateConfig_FirstAdminAcceptsSetupTokenHeader pins that a first-admin PUT the OSS
// setup-lock gate already authenticated (X-Bifrost-Setup-Token) does not have to repeat the
// token in auth_config.setup_token, while an unauthenticated one without it is still refused.
func TestUpdateConfig_FirstAdminAcceptsSetupTokenHeader(t *testing.T) {
	SetLogger(&mockLogger{})
	for _, tt := range []struct {
		name        string
		setupAuthed bool
		status      int
	}{
		{name: "header authenticated", setupAuthed: true, status: 200},
		{name: "no token anywhere", setupAuthed: false, status: 403},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := newRealConfigStore(t)
			cfg := newTestConfig(store, false)
			require.NoError(t, store.UpdateClientConfig(bgCtx(), cfg.ClientConfig))
			h := &ConfigHandler{store: cfg, configManager: setupConfigManager{store: store, validToken: false}}
			ctx := putConfigCtx(`{"client_config":{"log_retention_days":7},"auth_config":{"is_enabled":true,"admin_username":"admin","admin_password":"StrongPassword1!"}}`)
			if tt.setupAuthed {
				ctx.SetUserValue(schemas.BifrostContextKeySetupTokenAuthenticated, true)
			}
			h.updateConfig(ctx)
			require.Equal(t, tt.status, ctx.Response.StatusCode(), string(ctx.Response.Body()))
			auth, err := store.GetAuthConfig(bgCtx())
			require.NoError(t, err)
			if tt.status == 200 {
				require.NotNil(t, auth)
				assert.True(t, auth.IsEnabled)
			} else {
				assert.Nil(t, auth)
			}
		})
	}
}

// TestIsAuthEnabled_ReportsSetupLockState pins the fields the dashboard uses to choose the
// setup screen: setup_required follows the gate's lock state, setup_token_configured whether
// a token exists, and both stay false when the gate is not installed (enterprise).
func TestIsAuthEnabled_ReportsSetupLockState(t *testing.T) {
	SetLogger(&mockLogger{})
	token := "s3cret"
	enabled := &configstore.AuthConfig{AdminUserName: schemas.NewSecretVar("admin"), AdminPassword: schemas.NewSecretVar("stored"), IsEnabled: true}
	for _, tt := range []struct {
		name                     string
		gate                     bool
		token                    string
		admin                    *configstore.AuthConfig
		wantRequired, wantConfig bool
	}{
		{name: "no gate (enterprise)", gate: false, token: token},
		{name: "locked with token", gate: true, token: token, wantRequired: true, wantConfig: true},
		{name: "locked without token", gate: true, wantRequired: true},
		{name: "auth enabled", gate: true, token: token, admin: enabled, wantConfig: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := newRealConfigStore(t)
			if tt.admin != nil {
				require.NoError(t, store.UpdateAuthConfig(bgCtx(), tt.admin))
			}
			h := &SessionHandler{configStore: store}
			if tt.gate {
				am := &AuthMiddleware{}
				if tt.token != "" {
					am.setupToken.Store(&tt.token)
				}
				am.UpdateAuthConfig(tt.admin)
				h.SetSetupLock(am)
			}
			ctx := putConfigCtx("")
			h.isAuthEnabled(ctx)
			var reported struct {
				SetupRequired        *bool `json:"setup_required"`
				SetupTokenConfigured *bool `json:"setup_token_configured"`
			}
			require.NoError(t, json.Unmarshal(ctx.Response.Body(), &reported), string(ctx.Response.Body()))
			require.NotNil(t, reported.SetupRequired)
			require.NotNil(t, reported.SetupTokenConfigured)
			assert.Equal(t, tt.wantRequired, *reported.SetupRequired)
			assert.Equal(t, tt.wantConfig, *reported.SetupTokenConfigured)
		})
	}
}

// TestStartSetupSession_IssuesHttpOnlyCookie pins POST /api/session/setup: it trades a valid
// setup token for an HttpOnly, SameSite=Strict cookie the gate accepts, refuses a wrong token,
// and refuses outright when the setup lock is not active. Logout expires the cookie.
func TestStartSetupSession_IssuesHttpOnlyCookie(t *testing.T) {
	SetLogger(&mockLogger{})
	token := "s3cret"
	newLocked := func() (*SessionHandler, *AuthMiddleware) {
		am := &AuthMiddleware{}
		am.setupToken.Store(&token)
		h := &SessionHandler{configStore: newRealConfigStore(t)}
		h.SetSetupLock(am)
		return h, am
	}
	post := func(h *SessionHandler, header string) *fasthttp.RequestCtx {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod("POST")
		ctx.Request.SetRequestURI("/api/session/setup")
		if header != "" {
			ctx.Request.Header.Set(SetupTokenHeader, header)
		}
		h.startSetupSession(ctx)
		return ctx
	}

	t.Run("valid token", func(t *testing.T) {
		h, am := newLocked()
		ctx := post(h, token)
		require.Equal(t, 200, ctx.Response.StatusCode(), string(ctx.Response.Body()))
		cookie := fasthttp.AcquireCookie()
		defer fasthttp.ReleaseCookie(cookie)
		cookie.SetKey(SetupSessionCookie)
		require.True(t, ctx.Response.Header.Cookie(cookie), "Set-Cookie %s missing", SetupSessionCookie)
		assert.True(t, cookie.HTTPOnly(), "setup session cookie must be HttpOnly")
		assert.Equal(t, fasthttp.CookieSameSiteStrictMode, cookie.SameSite())
		assert.Equal(t, "/", string(cookie.Path()))
		assert.NotContains(t, string(cookie.Value()), token, "cookie must not carry the token")
		assert.True(t, am.validSetupSession(string(cookie.Value()), time.Now()), "issued cookie must pass the gate")
	})
	t.Run("forwarded https sets Secure", func(t *testing.T) {
		h, _ := newLocked()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod("POST")
		ctx.Request.Header.Set(SetupTokenHeader, token)
		ctx.Request.Header.Set("X-Forwarded-Proto", "https")
		h.startSetupSession(ctx)
		require.Equal(t, 200, ctx.Response.StatusCode())
		cookie := fasthttp.AcquireCookie()
		defer fasthttp.ReleaseCookie(cookie)
		cookie.SetKey(SetupSessionCookie)
		require.True(t, ctx.Response.Header.Cookie(cookie))
		assert.True(t, cookie.Secure())
	})
	t.Run("wrong or missing token", func(t *testing.T) {
		h, _ := newLocked()
		for _, header := range []string{"nope", ""} {
			ctx := post(h, header)
			assert.Equal(t, 403, ctx.Response.StatusCode(), "header %q", header)
			assert.Empty(t, ctx.Response.Header.PeekCookie(SetupSessionCookie))
		}
	})
	t.Run("lock not active", func(t *testing.T) {
		enterprise := &SessionHandler{configStore: newRealConfigStore(t)}
		assert.Equal(t, 409, post(enterprise, token).Response.StatusCode(), "no gate installed")
		h, am := newLocked()
		am.UpdateAuthConfig(&configstore.AuthConfig{AdminUserName: schemas.NewSecretVar("admin"), AdminPassword: schemas.NewSecretVar("x"), IsEnabled: true})
		assert.Equal(t, 409, post(h, token).Response.StatusCode(), "dashboard auth enabled")
	})
	t.Run("logout expires the cookie", func(t *testing.T) {
		h, _ := newLocked()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod("POST")
		h.logout(ctx)
		cookie := fasthttp.AcquireCookie()
		defer fasthttp.ReleaseCookie(cookie)
		cookie.SetKey(SetupSessionCookie)
		require.True(t, ctx.Response.Header.Cookie(cookie), "logout must clear %s", SetupSessionCookie)
		assert.Empty(t, string(cookie.Value()))
		assert.True(t, cookie.Expire().Before(time.Now()))
	})
}

// TestUpdateConfig_SetupTokenFirstAdminCanLogIn pins the full first-admin path through the
// OSS setup lock: an admin created by a setup-token-authenticated PUT (the security view's
// payload shape) can then log in with exactly the credentials it was created with.
func TestUpdateConfig_SetupTokenFirstAdminCanLogIn(t *testing.T) {
	SetLogger(&mockLogger{})
	store := newRealConfigStore(t)
	cfg := newTestConfig(store, false)
	require.NoError(t, store.UpdateClientConfig(bgCtx(), cfg.ClientConfig))
	h := &ConfigHandler{store: cfg, configManager: setupConfigManager{store: store, validToken: false}}
	ctx := putConfigCtx(`{"client_config":{"log_retention_days":7},"auth_config":{"is_enabled":true,"admin_username":{"value":"admin","ref":""},"admin_password":{"value":"StrongPassword1!","ref":""}}}`)
	ctx.SetUserValue(schemas.BifrostContextKeySetupTokenAuthenticated, true)
	h.updateConfig(ctx)
	require.Equal(t, 200, ctx.Response.StatusCode(), string(ctx.Response.Body()))

	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{name: "same credentials", body: `{"username":"admin","password":"StrongPassword1!"}`, status: 200},
		{name: "wrong password", body: `{"username":"admin","password":"StrongPassword1"}`, status: 401},
	} {
		login := putConfigCtx(tc.body)
		(&SessionHandler{configStore: store}).login(login)
		assert.Equal(t, tc.status, login.Response.StatusCode(), "%s: %s", tc.name, login.Response.Body())
	}
}

func TestGetPasswordPolicyFailures(t *testing.T) {
	tests := []struct {
		name     string
		password string
		want     []string
	}{
		{
			name:     "valid password",
			password: "StrongPass1!",
			want:     []string{},
		},
		{
			name:     "missing all requirements",
			password: "",
			want: []string{
				"at least 12 characters",
				"one uppercase letter",
				"one lowercase letter",
				"one number",
				"one special character",
			},
		},
		{
			name:     "missing character classes",
			password: "weakpassword",
			want: []string{
				"one uppercase letter",
				"one number",
				"one special character",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getPasswordPolicyFailures(tt.password)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("getPasswordPolicyFailures() = %v, want %v", got, tt.want)
			}
		})
	}
}

// enforce_vk_object_ownership defaults to true when a body omits it, so the handler copies it only
// when sent: a partial save from an API client must not turn a stored false back on.
func TestUpdateConfig_EnforceVKObjectOwnershipPresence(t *testing.T) {
	SetLogger(&mockLogger{})
	store := newRealConfigStore(t)
	cfg := newTestConfig(store, false)
	cfg.ClientConfig.EnforceVKObjectOwnership = false
	require.NoError(t, store.UpdateClientConfig(bgCtx(), cfg.ClientConfig))
	h := &ConfigHandler{store: cfg, configManager: stubConfigManager{}}
	stored := func() bool {
		t.Helper()
		persisted, err := store.GetClientConfig(bgCtx())
		require.NoError(t, err)
		return persisted.EnforceVKObjectOwnership
	}

	omitted := putConfigCtx(`{"client_config":{"log_retention_days":8}}`)
	h.updateConfig(omitted)
	require.Equal(t, fasthttp.StatusOK, omitted.Response.StatusCode(), string(omitted.Response.Body()))
	assert.False(t, cfg.ClientConfig.EnforceVKObjectOwnership, "an omitted field keeps the live value")
	assert.False(t, stored(), "an omitted field keeps the stored value")

	enabled := putConfigCtx(`{"client_config":{"log_retention_days":8,"enforce_vk_object_ownership":true}}`)
	h.updateConfig(enabled)
	require.Equal(t, fasthttp.StatusOK, enabled.Response.StatusCode(), string(enabled.Response.Body()))
	assert.True(t, cfg.ClientConfig.EnforceVKObjectOwnership)
	assert.True(t, stored())

	disabled := putConfigCtx(`{"client_config":{"log_retention_days":8,"enforce_vk_object_ownership":false}}`)
	h.updateConfig(disabled)
	require.Equal(t, fasthttp.StatusOK, disabled.Response.StatusCode(), string(disabled.Response.Body()))
	assert.False(t, cfg.ClientConfig.EnforceVKObjectOwnership)
	assert.False(t, stored())
	// null would decode as the default (true) and pass the presence check, so it is refused
	// before anything changes; other non-boolean values already fail decoding.
	for _, body := range []string{
		`{"client_config":{"log_retention_days":8,"enforce_vk_object_ownership":null}}`,
		`{"client_config":{"log_retention_days":8,"enforce_vk_object_ownership":"yes"}}`,
	} {
		refused := putConfigCtx(body)
		h.updateConfig(refused)
		assert.Equal(t, fasthttp.StatusBadRequest, refused.Response.StatusCode(), "%s: %s", body, refused.Response.Body())
		assert.False(t, cfg.ClientConfig.EnforceVKObjectOwnership, "%s left the live value unchanged", body)
		assert.False(t, stored(), "%s left the stored value unchanged", body)
	}
}

// TestUpdateConfig_SkipAccessProfileUserVirtualKeyLifecyclePresence pins that each access-profile
// user-key setting is copied only when sent, so a partial save from an API client can't silently hand
// user key lifecycle back to access profiles, and that the two move independently.
func TestUpdateConfig_SkipAccessProfileUserVirtualKeyLifecyclePresence(t *testing.T) {
	SetLogger(&mockLogger{})
	store := newRealConfigStore(t)
	cfg := newTestConfig(store, false)
	cfg.ClientConfig.SkipAccessProfileUserVirtualKeyCreation = true
	cfg.ClientConfig.SkipAccessProfileUserVirtualKeyDeletion = true
	require.NoError(t, store.UpdateClientConfig(bgCtx(), cfg.ClientConfig))
	h := &ConfigHandler{store: cfg, configManager: stubConfigManager{}}
	stored := func() (creation, deletion bool) {
		t.Helper()
		persisted, err := store.GetClientConfig(bgCtx())
		require.NoError(t, err)
		return persisted.SkipAccessProfileUserVirtualKeyCreation, persisted.SkipAccessProfileUserVirtualKeyDeletion
	}
	put := func(body string) {
		t.Helper()
		req := putConfigCtx(body)
		h.updateConfig(req)
		require.Equal(t, fasthttp.StatusOK, req.Response.StatusCode(), string(req.Response.Body()))
	}

	put(`{"client_config":{"log_retention_days":8}}`)
	creation, deletion := stored()
	assert.True(t, creation && deletion, "omitted fields keep the stored values")
	assert.True(t, cfg.ClientConfig.SkipAccessProfileUserVirtualKeyCreation && cfg.ClientConfig.SkipAccessProfileUserVirtualKeyDeletion, "omitted fields keep the live values")

	put(`{"client_config":{"log_retention_days":8,"skip_access_profile_user_virtual_key_creation":false}}`)
	creation, deletion = stored()
	assert.False(t, creation)
	assert.True(t, deletion, "turning creation back on leaves deletion alone")
	assert.False(t, cfg.ClientConfig.SkipAccessProfileUserVirtualKeyCreation)

	put(`{"client_config":{"log_retention_days":8,"skip_access_profile_user_virtual_key_deletion":false}}`)
	creation, deletion = stored()
	assert.False(t, creation)
	assert.False(t, deletion)
	assert.False(t, cfg.ClientConfig.SkipAccessProfileUserVirtualKeyDeletion)

	put(`{"client_config":{"log_retention_days":8,"skip_access_profile_user_virtual_key_creation":true,"skip_access_profile_user_virtual_key_deletion":true}}`)
	creation, deletion = stored()
	assert.True(t, creation && deletion)

	// null would decode as false and pass the presence check, silently handing user key creation or
	// deletion back to access profiles, so it is refused before anything changes.
	for _, body := range []string{
		`{"client_config":{"log_retention_days":8,"skip_access_profile_user_virtual_key_creation":null}}`,
		`{"client_config":{"log_retention_days":8,"skip_access_profile_user_virtual_key_deletion":null}}`,
	} {
		refused := putConfigCtx(body)
		h.updateConfig(refused)
		assert.Equal(t, fasthttp.StatusBadRequest, refused.Response.StatusCode(), "%s: %s", body, refused.Response.Body())
		creation, deletion = stored()
		assert.True(t, creation && deletion, "%s left the stored values unchanged", body)
		assert.True(t, cfg.ClientConfig.SkipAccessProfileUserVirtualKeyCreation && cfg.ClientConfig.SkipAccessProfileUserVirtualKeyDeletion, "%s left the live values unchanged", body)
	}
}

// extra_fields_placement is copied only when sent, so a partial save keeps the stored value,
// and anything other than body/headers is refused before any live mutation.
func TestUpdateConfig_ExtraFieldsPlacement(t *testing.T) {
	SetLogger(&mockLogger{})
	store := newRealConfigStore(t)
	cfg := newTestConfig(store, false)
	require.NoError(t, store.UpdateClientConfig(bgCtx(), cfg.ClientConfig))
	h := &ConfigHandler{store: cfg, configManager: stubConfigManager{}}
	stored := func() configtables.ExtraFieldsPlacement {
		t.Helper()
		persisted, err := store.GetClientConfig(bgCtx())
		require.NoError(t, err)
		return persisted.ExtraFieldsPlacement
	}

	headers := putConfigCtx(`{"client_config":{"log_retention_days":8,"extra_fields_placement":"headers"}}`)
	h.updateConfig(headers)
	require.Equal(t, fasthttp.StatusOK, headers.Response.StatusCode(), string(headers.Response.Body()))
	assert.True(t, cfg.ShouldSendExtraFieldsInHeaders())
	assert.Equal(t, configtables.ExtraFieldsPlacementHeaders, stored())

	omitted := putConfigCtx(`{"client_config":{"log_retention_days":8}}`)
	h.updateConfig(omitted)
	require.Equal(t, fasthttp.StatusOK, omitted.Response.StatusCode(), string(omitted.Response.Body()))
	assert.True(t, cfg.ShouldSendExtraFieldsInHeaders(), "an omitted field keeps the live value")
	assert.Equal(t, configtables.ExtraFieldsPlacementHeaders, stored(), "an omitted field keeps the stored value")

	// Only an omitted field keeps the stored value: an explicit empty string or null is a
	// supplied value, and anything other than body/headers is refused before any change.
	for _, body := range []string{
		`{"client_config":{"log_retention_days":8,"extra_fields_placement":"trailers"}}`,
		`{"client_config":{"log_retention_days":8,"extra_fields_placement":""}}`,
		`{"client_config":{"log_retention_days":8,"extra_fields_placement":null}}`,
	} {
		refused := putConfigCtx(body)
		h.updateConfig(refused)
		assert.Equal(t, fasthttp.StatusBadRequest, refused.Response.StatusCode(), "%s: %s", body, refused.Response.Body())
		assert.True(t, cfg.ShouldSendExtraFieldsInHeaders(), "%s left the live value unchanged", body)
		assert.Equal(t, configtables.ExtraFieldsPlacementHeaders, stored(), "%s left the stored value unchanged", body)
	}

	body := putConfigCtx(`{"client_config":{"log_retention_days":8,"extra_fields_placement":"body"}}`)
	h.updateConfig(body)
	require.Equal(t, fasthttp.StatusOK, body.Response.StatusCode(), string(body.Response.Body()))
	assert.False(t, cfg.ShouldSendExtraFieldsInHeaders())
	assert.Equal(t, configtables.ExtraFieldsPlacementBody, stored())
}

// entity_access_profile_mode is copied only when sent, so a partial save keeps the stored mode,
// and anything other than union/intersect is refused before any live mutation.
func TestUpdateConfig_EntityAccessProfileMode(t *testing.T) {
	SetLogger(&mockLogger{})
	store := newRealConfigStore(t)
	cfg := newTestConfig(store, false)
	require.NoError(t, store.UpdateClientConfig(bgCtx(), cfg.ClientConfig))
	h := &ConfigHandler{store: cfg, configManager: stubConfigManager{}}
	stored := func() configtables.EntityAccessProfileMode {
		t.Helper()
		persisted, err := store.GetClientConfig(bgCtx())
		require.NoError(t, err)
		return persisted.EntityAccessProfileMode
	}
	assert.False(t, cfg.ShouldIntersectEntityAccessProfiles(), "union until set")

	intersect := putConfigCtx(`{"client_config":{"log_retention_days":8,"entity_access_profile_mode":"intersect"}}`)
	h.updateConfig(intersect)
	require.Equal(t, fasthttp.StatusOK, intersect.Response.StatusCode(), string(intersect.Response.Body()))
	assert.True(t, cfg.ShouldIntersectEntityAccessProfiles())
	assert.Equal(t, configtables.EntityAccessProfileModeIntersect, stored())

	omitted := putConfigCtx(`{"client_config":{"log_retention_days":8}}`)
	h.updateConfig(omitted)
	require.Equal(t, fasthttp.StatusOK, omitted.Response.StatusCode(), string(omitted.Response.Body()))
	assert.True(t, cfg.ShouldIntersectEntityAccessProfiles(), "an omitted field keeps the live value")
	assert.Equal(t, configtables.EntityAccessProfileModeIntersect, stored(), "an omitted field keeps the stored value")

	for _, body := range []string{
		`{"client_config":{"log_retention_days":8,"entity_access_profile_mode":"restrictive"}}`,
		`{"client_config":{"log_retention_days":8,"entity_access_profile_mode":""}}`,
		`{"client_config":{"log_retention_days":8,"entity_access_profile_mode":null}}`,
	} {
		refused := putConfigCtx(body)
		h.updateConfig(refused)
		assert.Equal(t, fasthttp.StatusBadRequest, refused.Response.StatusCode(), "%s: %s", body, refused.Response.Body())
		assert.True(t, cfg.ShouldIntersectEntityAccessProfiles(), "%s left the live value unchanged", body)
		assert.Equal(t, configtables.EntityAccessProfileModeIntersect, stored(), "%s left the stored value unchanged", body)
	}

	union := putConfigCtx(`{"client_config":{"log_retention_days":8,"entity_access_profile_mode":"union"}}`)
	h.updateConfig(union)
	require.Equal(t, fasthttp.StatusOK, union.Response.StatusCode(), string(union.Response.Body()))
	assert.False(t, cfg.ShouldIntersectEntityAccessProfiles())
	assert.Equal(t, configtables.EntityAccessProfileModeUnion, stored())
}

// TestUpdateConfig_EmptyDatasheetURLsResetToDefaults pins the regression where
// PUT /api/config rejected an empty pricing_url with "URL cannot be empty".
// The custom pricing page sends "" when the user clears the field; an empty
// pricing_url or model_parameters_url means "use the built-in URL".
func TestUpdateConfig_EmptyDatasheetURLsResetToDefaults(t *testing.T) {
	// The datasheet server below is loopback-bound; the accessibility check refuses loopback
	// by default, so route its dial through a plain dialer for this test only.
	prevDial := checkURLAccessibilityDialContext
	checkURLAccessibilityDialContext = (&net.Dialer{}).DialContext
	t.Cleanup(func() { checkURLAccessibilityDialContext = prevDial })
	SetLogger(&mockLogger{})
	store := newRealConfigStore(t)
	cfg := newTestConfig(store, false)
	h := &ConfigHandler{store: cfg, configManager: stubConfigManager{}}

	// A loopback datasheet server passes the accessibility check; file:// URLs
	// are refused over the API (TestUpdateConfig_RejectsFileURLsOverAPI).
	customURL := newDatasheetServer(t).URL

	save := func(t *testing.T, pricingURL, modelParamsURL string) {
		t.Helper()
		ctx := putConfigCtx(`{"client_config":{"log_retention_days":7},"framework_config":{"pricing_url":"` + pricingURL + `","model_parameters_url":"` + modelParamsURL + `"}}`)
		h.updateConfig(ctx)
		require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	save(t, customURL, customURL)
	persisted, err := store.GetFrameworkConfig(bgCtx())
	require.NoError(t, err)
	require.NotNil(t, persisted)
	assert.Equal(t, customURL, *persisted.PricingURL)
	assert.Equal(t, customURL, *persisted.ModelParametersURL)

	save(t, "", "")
	persisted, err = store.GetFrameworkConfig(bgCtx())
	require.NoError(t, err)
	require.NotNil(t, persisted)
	assert.Equal(t, modelcatalog.DefaultPricingURL, *persisted.PricingURL, "empty pricing_url must reset to the default")
	assert.Equal(t, modelcatalog.DefaultModelParametersURL, *persisted.ModelParametersURL, "empty model_parameters_url must reset to the default")
	assert.Equal(t, modelcatalog.DefaultPricingURL, *cfg.FrameworkConfig.Pricing.PricingURL)
	assert.Equal(t, modelcatalog.DefaultModelParametersURL, *cfg.FrameworkConfig.Pricing.ModelParametersURL)
}

// failingFrameworkConfigStore makes the framework config write fail while every
// other store call goes through to the real store.
type failingFrameworkConfigStore struct {
	configstore.ConfigStore
}

func (failingFrameworkConfigStore) UpdateFrameworkConfig(context.Context, *configtables.TableFrameworkConfig) error {
	return errors.New("simulated store failure")
}

// TestUpdateConfig_FrameworkConfigStoreFailureLeavesRuntimeUnchanged pins the
// ordering in updateConfig: the framework config is persisted before it is
// published to runtime, so a failed store write does not leave the in-memory
// config pointing at URLs the database never saved.
func TestUpdateConfig_FrameworkConfigStoreFailureLeavesRuntimeUnchanged(t *testing.T) {
	// The datasheet server below is loopback-bound; the accessibility check refuses loopback
	// by default, so route its dial through a plain dialer for this test only.
	prevDial := checkURLAccessibilityDialContext
	checkURLAccessibilityDialContext = (&net.Dialer{}).DialContext
	t.Cleanup(func() { checkURLAccessibilityDialContext = prevDial })
	SetLogger(&mockLogger{})
	store := newRealConfigStore(t)
	cfg := newTestConfig(failingFrameworkConfigStore{store}, false)
	before := cfg.FrameworkConfig
	h := &ConfigHandler{store: cfg, configManager: stubConfigManager{}}

	ctx := putConfigCtx(`{"client_config":{"log_retention_days":7},"framework_config":{"pricing_url":"` + newDatasheetServer(t).URL + `"}}`)
	h.updateConfig(ctx)
	require.Equal(t, fasthttp.StatusInternalServerError, ctx.Response.StatusCode(), string(ctx.Response.Body()))
	assert.Same(t, before, cfg.FrameworkConfig, "runtime framework config must not change when the store write fails")
}

// TestUpdateProxyConfig_InterceptionGuardWhenAuthBypassed pins that the fail-open bypass
// cannot point the global proxy at a caller-chosen host or turn off its TLS verification:
// either lets a third party read provider credentials for every proxied request. Saving
// other fields against the stored proxy URL must still go through.
func TestUpdateProxyConfig_InterceptionGuardWhenAuthBypassed(t *testing.T) {
	SetLogger(&mockLogger{})
	const storedURL = "http://10.0.0.5:3128"
	caServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	caServer.Close()
	caJSON, err := sonic.Marshal(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caServer.Certificate().Raw})))
	require.NoError(t, err)
	cases := []struct {
		name    string
		stored  *configtables.GlobalProxyConfig // nil: an http proxy at storedURL
		body    string
		want403 bool
	}{
		{name: "same url, timeout edit", body: `{"enabled":true,"type":"http","url":"` + storedURL + `","timeout":30,"enable_for_inference":true}`, want403: false},
		{name: "url changed", body: `{"enabled":true,"type":"http","url":"http://evil.example.com:8080","timeout":10,"enable_for_inference":true}`, want403: true},
		{name: "skip_tls_verify turned on", body: `{"enabled":true,"type":"http","url":"` + storedURL + `","timeout":10,"skip_tls_verify":true,"enable_for_inference":true}`, want403: true},
		// The type picks the dialer: the same URL under another type is another proxy protocol.
		{name: "type changed, same url", body: `{"enabled":true,"type":"socks5","url":"` + storedURL + `","timeout":10,"enable_for_inference":true}`, want403: true},
		// A stored proxy with no type dials as http, so naming socks5 for its bare host:port
		// changes the protocol, and naming http does not.
		{
			name:    "empty stored type changed to socks5, same bare url",
			stored:  &configtables.GlobalProxyConfig{Enabled: true, URL: "10.0.0.5:1080", Timeout: 10, EnableForInference: true},
			body:    `{"enabled":true,"type":"socks5","url":"10.0.0.5:1080","timeout":30,"enable_for_inference":true}`,
			want403: true,
		},
		{
			name:    "empty stored type named http, same bare url",
			stored:  &configtables.GlobalProxyConfig{Enabled: true, URL: "10.0.0.5:1080", Timeout: 10, EnableForInference: true},
			body:    `{"enabled":true,"type":"http","url":"10.0.0.5:1080","timeout":30,"enable_for_inference":true}`,
			want403: false,
		},
		// The proxy CA is trusted for targets reached through the proxy, so adding one lets
		// the proxy sign provider certificates: as sensitive as changing the URL.
		{name: "ca_cert_pem added, same url", body: `{"enabled":true,"type":"http","url":"` + storedURL + `","timeout":10,"enable_for_inference":true,"ca_cert_pem":` + string(caJSON) + `}`, want403: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newRealConfigStore(t)
			seed := tc.stored
			if seed == nil {
				seed = &configtables.GlobalProxyConfig{Enabled: true, Type: "http", URL: storedURL, Timeout: 10, EnableForInference: true}
			}
			require.NoError(t, store.UpdateProxyConfig(context.Background(), seed))
			cfg := newTestConfig(store, false)
			h := &ConfigHandler{store: cfg, configManager: stubConfigManager{}}

			ctx := newTestRequestCtx(tc.body)
			ctx.Request.Header.SetMethod(fasthttp.MethodPut)
			ctx.SetUserValue(schemas.BifrostContextKeyAuthBypassed, true)

			h.updateProxyConfig(ctx)

			got403 := ctx.Response.StatusCode() == fasthttp.StatusForbidden
			require.Equal(t, tc.want403, got403, "status %d; body=%s", ctx.Response.StatusCode(), ctx.Response.Body())
			stored, err := store.GetProxyConfig(context.Background())
			require.NoError(t, err)
			if tc.want403 {
				assert.Equal(t, seed.URL, stored.URL)
				assert.Equal(t, seed.Type, stored.Type)
				assert.False(t, stored.SkipTLSVerify)
				assert.Empty(t, stored.CACertPEM)
				assert.Equal(t, 10, stored.Timeout)
			} else {
				require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode(), "body=%s", ctx.Response.Body())
				assert.Equal(t, 30, stored.Timeout)
			}
		})
	}
}

// newDatasheetServer serves an empty JSON datasheet on loopback.
func newDatasheetServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCheckURLAccessibility_RejectsFileURLs: file:// datasheet and catalog
// URLs are an operator feature of config.json (air-gapped deployments) and are
// refused when they arrive over the HTTP API, whatever the path points at.
func TestCheckURLAccessibility_RejectsFileURLs(t *testing.T) {
	for _, raw := range []string{"file:///etc/hostname", "file://./pricing.json", "file:pricing.json", "FILE:///etc/hostname"} {
		err := checkURLAccessibility(raw)
		require.Error(t, err, raw)
		assert.Contains(t, err.Error(), "config.json", raw)
	}
}

// TestCheckURLAccessibility_ReachesConfiguredPrivateProxy pins that the URL check works
// behind a global API proxy on a private or loopback address. The proxy is dialed with
// the private-network policy; a direct dial keeps the public-only check, so saving a
// URL override does not fail just because the proxy is self-hosted.
func TestCheckURLAccessibility_ReachesConfiguredPrivateProxy(t *testing.T) {
	SetLogger(&mockLogger{})
	var seen atomic.Value
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.URL.Host)
		_, _ = w.Write([]byte("{}"))
	}))
	defer proxy.Close()
	network.SetDefaultHTTPClientFactory(network.NewHTTPClientFactory(&network.GlobalProxyConfig{
		Enabled: true, Type: network.GlobalProxyTypeHTTP, URL: proxy.URL, EnableForAPI: true,
	}, nil))
	t.Cleanup(func() { network.SetDefaultHTTPClientFactory(nil) })

	if err := checkURLAccessibility("http://203.0.113.10/pricing.json"); err != nil {
		t.Fatalf("a loopback API proxy must be usable for the URL check, got %v", err)
	}
	if got, _ := seen.Load().(string); got != "203.0.113.10" {
		t.Fatalf("proxy saw target %q, want 203.0.113.10", got)
	}
}

// TestCheckURLAccessibility_TrustsGlobalProxyCA pins that the URL check reaches an
// https:// API proxy whose certificate only the global proxy's ca_cert_pem trusts, the
// same proxy the sync after a successful check goes through. Without the CA the proxy
// hop is refused.
func TestCheckURLAccessibility_TrustsGlobalProxyCA(t *testing.T) {
	SetLogger(&mockLogger{})
	proxy := proxytest.NewHTTPSRecorder(t, "url-check-https-proxy")
	config := &network.GlobalProxyConfig{
		Enabled: true, Type: network.GlobalProxyTypeHTTP, URL: "https://127.0.0.1:" + proxy.Port(), EnableForAPI: true,
	}
	factory := network.NewHTTPClientFactory(config, nil)
	network.SetDefaultHTTPClientFactory(factory)
	t.Cleanup(func() { network.SetDefaultHTTPClientFactory(nil) })

	if err := checkURLAccessibility("http://203.0.113.10/pricing.json"); err == nil {
		t.Fatal("an https proxy no configured CA trusts must be refused")
	}

	withCA := *config
	withCA.CACertPEM = proxy.CAPEM
	factory.UpdateProxyConfig(&withCA)
	if err := checkURLAccessibility("http://203.0.113.10/pricing.json"); err != nil {
		t.Fatalf("the global proxy CA must be trusted for the https proxy, got %v", err)
	}
	if hits := proxy.Seen(); len(hits) == 0 || hits[len(hits)-1].Target != "203.0.113.10:80" {
		t.Fatalf("proxy hits %+v, want a tunnel to 203.0.113.10:80", hits)
	}
}

// TestCheckURLAccessibility_RefusesRedirectToLinkLocal: the entry host is a
// reachable loopback server whose 302 points at the cloud metadata address.
// The hop must be refused, and the caller-facing error must not name the
// target or carry the transport detail.
func TestCheckURLAccessibility_RefusesRedirectToLinkLocal(t *testing.T) {
	SetLogger(&mockLogger{})
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer redirect.Close()

	err := checkURLAccessibility(redirect.URL)
	require.Error(t, err)
	assert.Equal(t, "url is not reachable", err.Error())
}

// TestCheckURLAccessibility_DoesNotReflectTransportErrors: a non-HTTP service
// greets with a banner that net/http folds into its parse error. That detail
// is for the debug log, never for the response body.
func TestCheckURLAccessibility_DoesNotReflectTransportErrors(t *testing.T) {
	SetLogger(&mockLogger{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("SSH-2.0-OpenSSH_9.6 Ubuntu-3ubuntu0.1\r\n"))
			conn.Close()
		}
	}()

	err = checkURLAccessibility("http://" + ln.Addr().String() + "/")
	require.Error(t, err)
	assert.Equal(t, "url is not reachable", err.Error())
	assert.NotContains(t, strings.ToLower(err.Error()), "openssh")

	// A refused port reads the same as a banner: no open/closed oracle.
	ln.Close()
	err = checkURLAccessibility("http://" + ln.Addr().String() + "/")
	require.Error(t, err)
	assert.Equal(t, "url is not reachable", err.Error())
}

// TestUpdateConfig_RejectsFileURLsOverAPI pins the API-side rule for both
// catalog URLs: PUT /api/config answers 400 and persists nothing.
func TestUpdateConfig_RejectsFileURLsOverAPI(t *testing.T) {
	SetLogger(&mockLogger{})
	store := newRealConfigStore(t)
	cfg := newTestConfig(store, false)
	h := &ConfigHandler{store: cfg, configManager: stubConfigManager{}}

	for _, field := range []string{"pricing_url", "model_parameters_url"} {
		t.Run(field, func(t *testing.T) {
			ctx := putConfigCtx(`{"client_config":{"log_retention_days":7},"framework_config":{"` + field + `":"file:///etc/hostname"}}`)
			h.updateConfig(ctx)
			require.Equal(t, fasthttp.StatusBadRequest, ctx.Response.StatusCode(), string(ctx.Response.Body()))
			assert.Contains(t, string(ctx.Response.Body()), "config.json")
		})
	}
	persisted, err := store.GetFrameworkConfig(bgCtx())
	require.NoError(t, err)
	if persisted != nil {
		for _, got := range []*string{persisted.PricingURL, persisted.ModelParametersURL} {
			if got != nil {
				assert.NotContains(t, *got, "file://")
			}
		}
	}
}

// TestUpdateConfig_UnchangedFileURLsPassOverAPI pins that a file:// URL set in
// config.json does not break later saves: GET /api/config returns the stored
// URL, so a save that sends it back unchanged is accepted, while a different
// file:// URL is still refused.
func TestUpdateConfig_UnchangedFileURLsPassOverAPI(t *testing.T) {
	SetLogger(&mockLogger{})
	store := newRealConfigStore(t)
	const stored = "file:///var/lib/bifrost/catalog.json"
	require.NoError(t, store.UpdateFrameworkConfig(bgCtx(), &configtables.TableFrameworkConfig{
		PricingURL:         bifrost.Ptr(stored),
		ModelParametersURL: bifrost.Ptr(stored),
	}))
	cfg := newTestConfig(store, false)
	h := &ConfigHandler{store: cfg, configManager: stubConfigManager{}}

	for _, field := range []string{"pricing_url", "model_parameters_url"} {
		t.Run(field, func(t *testing.T) {
			ctx := putConfigCtx(`{"client_config":{"log_retention_days":7},"framework_config":{"` + field + `":"` + stored + `"}}`)
			h.updateConfig(ctx)
			require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode(), string(ctx.Response.Body()))

			ctx = putConfigCtx(`{"client_config":{"log_retention_days":7},"framework_config":{"` + field + `":"file:///etc/hostname"}}`)
			h.updateConfig(ctx)
			require.Equal(t, fasthttp.StatusBadRequest, ctx.Response.StatusCode(), string(ctx.Response.Body()))
			assert.Contains(t, string(ctx.Response.Body()), "config.json")
		})
	}
}

// TestUpdateProxyConfig_RejectsLinkLocalURL pins that the global proxy URL is held to the
// same destination rule as a provider base URL. Every proxied request carries its provider
// credentials to this host, so an address that can never be a legitimate egress proxy
// (link-local such as the cloud metadata endpoint, or unspecified) is refused even for a
// genuinely authenticated admin. Private and loopback hosts stay allowed: a self-hosted
// proxy on the local network is the normal setup.
func TestUpdateProxyConfig_RejectsLinkLocalURL(t *testing.T) {
	SetLogger(&mockLogger{})
	cases := []struct {
		name       string
		url        string
		wantStatus int
	}{
		{name: "link-local metadata endpoint", url: "http://169.254.169.254:80", wantStatus: fasthttp.StatusBadRequest},
		{name: "unspecified address", url: "http://0.0.0.0:3128", wantStatus: fasthttp.StatusBadRequest},
		{name: "private network proxy", url: "http://10.0.0.5:3128", wantStatus: fasthttp.StatusOK},
		{name: "loopback proxy", url: "http://127.0.0.1:3128", wantStatus: fasthttp.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newRealConfigStore(t)
			cfg := newTestConfig(store, false)
			h := &ConfigHandler{store: cfg, configManager: stubConfigManager{}}

			ctx := newTestRequestCtx(`{"enabled":true,"type":"http","url":"` + tc.url + `","timeout":0}`)
			ctx.Request.Header.SetMethod(fasthttp.MethodPut)

			h.updateProxyConfig(ctx)

			require.Equal(t, tc.wantStatus, ctx.Response.StatusCode(), "body=%s", ctx.Response.Body())
			stored, err := store.GetProxyConfig(context.Background())
			if tc.wantStatus == fasthttp.StatusOK {
				require.NoError(t, err)
				assert.Equal(t, tc.url, stored.URL)
				return
			}
			if err != nil {
				require.ErrorIs(t, err, configstore.ErrNotFound)
				return
			}
			if stored != nil {
				assert.Empty(t, stored.URL, "a rejected proxy URL must not be persisted")
			}
		})
	}
}

// authConfigTestManager persists auth config to the real store and matches setup tokens
// against one fixed value, so updateConfig's credential gates can be exercised end to end.
type authConfigTestManager struct {
	stubConfigManager
	store      configstore.ConfigStore
	setupToken string
}

func (m authConfigTestManager) UpdateAuthConfig(ctx context.Context, c *configstore.AuthConfig) error {
	return m.store.UpdateAuthConfig(ctx, c)
}

func (m authConfigTestManager) ValidateSetupToken(token string) bool {
	return m.setupToken != "" && token == m.setupToken
}

func (m authConfigTestManager) ValidateConfiguredSetupToken(token string) bool {
	return m.setupToken != "" && token == m.setupToken
}

// TestUpdateConfig_StoredCredentialsRequireProofWhenAuthBypassed pins that an admin account
// which exists but has dashboard auth switched off cannot be taken over through the open
// management API: a caller admitted without a credential check may switch auth back on, or
// replace the stored credentials, only by presenting the current admin password
// (current_password) or the operator-configured setup token. A genuinely authenticated
// session needs no extra proof, and a disabled-state save that resubmits nothing keeps the
// stored credentials as before.
func TestUpdateConfig_StoredCredentialsRequireProofWhenAuthBypassed(t *testing.T) {
	SetLogger(&mockLogger{})
	const (
		storedUser = "admin"
		storedPass = "Current-Pass-1234!"
		newPass    = "Replacement-Pass-5678!"
		setupToken = "operator-setup-token"
	)
	body := func(enabled, extra string) string {
		return `{"client_config":{"log_retention_days":7},"auth_config":{"is_enabled":` + enabled +
			`,"admin_username":"` + storedUser + `","admin_password":"` + newPass + `"` + extra + `}}`
	}
	cases := []struct {
		name        string
		body        string
		bypassed    bool
		wantStatus  int
		wantEnabled bool
		wantPass    string
	}{
		{name: "anonymous re-enable with new password", body: body("true", ""), bypassed: true, wantStatus: fasthttp.StatusForbidden, wantPass: storedPass},
		{name: "anonymous re-enable with wrong current_password", body: body("true", `,"current_password":"not-it"`), bypassed: true, wantStatus: fasthttp.StatusForbidden, wantPass: storedPass},
		{name: "anonymous re-enable with wrong setup token", body: body("true", `,"setup_token":"wrong"`), bypassed: true, wantStatus: fasthttp.StatusForbidden, wantPass: storedPass},
		{name: "anonymous re-enable with correct current_password", body: body("true", `,"current_password":"`+storedPass+`"`), bypassed: true, wantStatus: fasthttp.StatusOK, wantEnabled: true, wantPass: newPass},
		{name: "anonymous re-enable with valid setup token", body: body("true", `,"setup_token":"`+setupToken+`"`), bypassed: true, wantStatus: fasthttp.StatusOK, wantEnabled: true, wantPass: newPass},
		{name: "authenticated re-enable needs no extra proof", body: body("true", ""), bypassed: false, wantStatus: fasthttp.StatusOK, wantEnabled: true, wantPass: newPass},
		{name: "anonymous credential change while auth stays disabled", body: body("false", ""), bypassed: true, wantStatus: fasthttp.StatusForbidden, wantPass: storedPass},
		{name: "anonymous credential change while disabled with current_password", body: body("false", `,"current_password":"`+storedPass+`"`), bypassed: true, wantStatus: fasthttp.StatusOK, wantPass: newPass},
		{name: "anonymous disabled save without credentials keeps the stored ones", body: `{"client_config":{"log_retention_days":7},"auth_config":{"is_enabled":false}}`, bypassed: true, wantStatus: fasthttp.StatusOK, wantPass: storedPass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newRealConfigStore(t)
			hash, err := encrypt.Hash(storedPass)
			require.NoError(t, err)
			require.NoError(t, store.UpdateAuthConfig(context.Background(), &configstore.AuthConfig{
				AdminUserName: schemas.NewSecretVar(storedUser),
				AdminPassword: schemas.NewSecretVar(hash),
				IsEnabled:     false,
			}))
			cfg := newTestConfig(store, false)
			h := &ConfigHandler{store: cfg, configManager: authConfigTestManager{store: store, setupToken: setupToken}}

			ctx := putConfigCtx(tc.body)
			if tc.bypassed {
				ctx.SetUserValue(schemas.BifrostContextKeyAuthBypassed, true)
			}
			h.updateConfig(ctx)

			require.Equal(t, tc.wantStatus, ctx.Response.StatusCode(), "body=%s", ctx.Response.Body())
			stored, err := store.GetAuthConfig(context.Background())
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.Equal(t, tc.wantEnabled, stored.IsEnabled)
			assert.Equal(t, storedUser, stored.AdminUserName.GetValue())
			ok, _ := encrypt.CompareHash(stored.AdminPassword.GetValue(), tc.wantPass)
			assert.True(t, ok, "stored password must verify against %q", tc.wantPass)
		})
	}
}

// TestUpdateConfig_FirstAdminStillRequiresSetupToken pins that the first-admin gate is
// unchanged by the stored-credential proof: with no admin account, only the setup token
// creates one. A current_password cannot stand in for it - there is nothing to compare it to.
func TestUpdateConfig_FirstAdminStillRequiresSetupToken(t *testing.T) {
	SetLogger(&mockLogger{})
	const setupToken = "operator-setup-token"
	body := func(extra string) string {
		return `{"client_config":{"log_retention_days":7},"auth_config":{"is_enabled":true,"admin_username":"admin","admin_password":"First-Admin-Pass-1!"` + extra + `}}`
	}
	cases := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "no setup token", body: body(""), wantStatus: fasthttp.StatusForbidden},
		{name: "wrong setup token", body: body(`,"setup_token":"wrong"`), wantStatus: fasthttp.StatusForbidden},
		{name: "current_password instead of setup token", body: body(`,"current_password":"First-Admin-Pass-1!"`), wantStatus: fasthttp.StatusForbidden},
		{name: "valid setup token", body: body(`,"setup_token":"` + setupToken + `"`), wantStatus: fasthttp.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newRealConfigStore(t)
			cfg := newTestConfig(store, false)
			h := &ConfigHandler{store: cfg, configManager: authConfigTestManager{store: store, setupToken: setupToken}}

			ctx := putConfigCtx(tc.body)
			ctx.SetUserValue(schemas.BifrostContextKeyAuthBypassed, true)
			h.updateConfig(ctx)

			require.Equal(t, tc.wantStatus, ctx.Response.StatusCode(), "body=%s", ctx.Response.Body())
			stored, err := store.GetAuthConfig(context.Background())
			if tc.wantStatus == fasthttp.StatusOK {
				require.NoError(t, err)
				require.NotNil(t, stored)
				assert.True(t, stored.IsEnabled)
				return
			}
			if err != nil {
				require.ErrorIs(t, err, configstore.ErrNotFound)
				return
			}
			assert.Nil(t, stored, "no admin account may be created without the setup token")
		})
	}
}

// recordingCompatConfigManager records compat plugin reloads and removals.
type recordingCompatConfigManager struct {
	stubConfigManager
	reloaded   []*compat.Config
	removed    []string
	failReload bool
}

func (m *recordingCompatConfigManager) ReloadPlugin(_ context.Context, name string, _ *string, cfg any, _ *schemas.PluginPlacement, _ *int) error {
	if name == compat.PluginName {
		m.reloaded = append(m.reloaded, cfg.(*compat.Config))
		if m.failReload {
			return errors.New("compat reload failed")
		}
	}
	return nil
}

func (m *recordingCompatConfigManager) RemovePlugin(_ context.Context, name string) error {
	m.removed = append(m.removed, name)
	return nil
}

// TestUpdateConfig_CompatAllFlagsOffKeepsPluginLoaded pins that disabling every
// compat flag reloads the plugin instead of removing it, so per-request compat
// header overrides keep working.
func TestUpdateConfig_CompatAllFlagsOffKeepsPluginLoaded(t *testing.T) {
	SetLogger(&mockLogger{})
	store := newRealConfigStore(t)
	cfg := newTestConfig(store, false)
	cfg.ClientConfig.Compat = configstore.CompatConfig{ConvertTextToChat: true}
	cm := &recordingCompatConfigManager{}
	h := &ConfigHandler{store: cfg, configManager: cm}

	ctx := putConfigCtx(`{"client_config":{"log_retention_days":7,"compat":{"convert_text_to_chat":false,"convert_chat_to_responses":false,"should_drop_params":false,"should_convert_params":false,"azure_deepseek":false,"force_reasoning_only_models_to_responses":false}}}`)
	h.updateConfig(ctx)
	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode(), string(ctx.Response.Body()))

	assert.NotContains(t, cm.removed, compat.PluginName, "compat plugin must stay loaded for per-request overrides")
	require.Len(t, cm.reloaded, 1, "compat plugin must be reloaded with the new config")
	assert.Equal(t, compat.Config{}, *cm.reloaded[0])
}

const compatAllOffPayload = `{"client_config":{"log_retention_days":7,"compat":{"convert_text_to_chat":false,"convert_chat_to_responses":false,"should_drop_params":false,"should_convert_params":false,"azure_deepseek":false,"force_reasoning_only_models_to_responses":false}}}`

// TestUpdateConfig_CompatReloadWaitsForPersistence pins that a failed save never
// reaches the compat plugin, so runtime compat cannot run ahead of the database.
func TestUpdateConfig_CompatReloadWaitsForPersistence(t *testing.T) {
	SetLogger(&mockLogger{})
	store := newRealConfigStore(t)
	cfg := newTestConfig(inferenceSetupFailureStore{store, "write"}, false)
	cfg.ClientConfig.Compat = configstore.CompatConfig{ConvertTextToChat: true}
	cm := &recordingCompatConfigManager{}
	h := &ConfigHandler{store: cfg, configManager: cm}

	ctx := putConfigCtx(compatAllOffPayload)
	h.updateConfig(ctx)
	require.Equal(t, fasthttp.StatusInternalServerError, ctx.Response.StatusCode(), string(ctx.Response.Body()))

	assert.Empty(t, cm.reloaded, "compat plugin must not change when the save fails")
	assert.True(t, cfg.ClientConfig.Compat.ConvertTextToChat, "in-memory compat must stay unchanged")
}

// TestUpdateConfig_CompatReloadFailureRestoresPersistedConfig pins that a failed
// compat reload after a successful save rolls the persisted compat setting back.
func TestUpdateConfig_CompatReloadFailureRestoresPersistedConfig(t *testing.T) {
	SetLogger(&mockLogger{})
	store := newRealConfigStore(t)
	cfg := newTestConfig(store, false)
	cfg.ClientConfig.Compat = configstore.CompatConfig{ConvertTextToChat: true}
	require.NoError(t, store.UpdateClientConfig(bgCtx(), cfg.ClientConfig))
	cm := &recordingCompatConfigManager{failReload: true}
	h := &ConfigHandler{store: cfg, configManager: cm}

	ctx := putConfigCtx(compatAllOffPayload)
	h.updateConfig(ctx)
	require.Equal(t, fasthttp.StatusBadRequest, ctx.Response.StatusCode(), string(ctx.Response.Body()))

	persisted, err := store.GetClientConfig(bgCtx())
	require.NoError(t, err)
	assert.True(t, persisted.Compat.ConvertTextToChat, "persisted compat must be restored after a failed reload")
	assert.True(t, cfg.ClientConfig.Compat.ConvertTextToChat, "in-memory compat must stay unchanged")
}

// failingRestoreStore fails every UpdateClientConfig after the first, so the
// compensating restore write fails.
type failingRestoreStore struct {
	configstore.ConfigStore
	writes *int
}

func (s failingRestoreStore) UpdateClientConfig(ctx context.Context, c *configstore.ClientConfig) error {
	*s.writes++
	if *s.writes > 1 {
		return errors.New("restore write failed")
	}
	return s.ConfigStore.UpdateClientConfig(ctx, c)
}

// TestUpdateConfig_CompatReloadFailureReportsFailedRestore pins that a failed restore
// is reported as a server error instead of the clean-rollback 400.
func TestUpdateConfig_CompatReloadFailureReportsFailedRestore(t *testing.T) {
	SetLogger(&mockLogger{})
	store := newRealConfigStore(t)
	writes := 0
	cfg := newTestConfig(failingRestoreStore{store, &writes}, false)
	cfg.ClientConfig.Compat = configstore.CompatConfig{ConvertTextToChat: true}
	require.NoError(t, store.UpdateClientConfig(bgCtx(), cfg.ClientConfig))
	cm := &recordingCompatConfigManager{failReload: true}
	h := &ConfigHandler{store: cfg, configManager: cm}

	ctx := putConfigCtx(compatAllOffPayload)
	h.updateConfig(ctx)
	require.Equal(t, fasthttp.StatusInternalServerError, ctx.Response.StatusCode(), string(ctx.Response.Body()))
	assert.Contains(t, string(ctx.Response.Body()), "could not be restored")
	assert.Equal(t, 2, writes, "the handler must attempt the restore write")
	assert.True(t, cfg.ClientConfig.Compat.ConvertTextToChat, "in-memory compat must stay unchanged")
}

// proxyReloadRecorder records the global proxy config updateProxyConfig hands to the
// runtime.
type proxyReloadRecorder struct {
	stubConfigManager
	reloaded *configtables.GlobalProxyConfig
}

func (r *proxyReloadRecorder) ReloadProxyConfig(_ context.Context, cfg *configtables.GlobalProxyConfig) error {
	r.reloaded = cfg
	return nil
}

// TestUpdateProxyConfig_ProxyTypes pins which global proxy types PUT /api/proxy-config
// accepts. socks5 is accepted, stored and applied: the HTTP client factory and the
// provider stacks dial SOCKS5 proxies (with RFC 1929 credentials), so rejecting it as
// "not yet supported" only hid a working feature. tcp has no dialer and stays refused.
func TestUpdateProxyConfig_ProxyTypes(t *testing.T) {
	SetLogger(&mockLogger{})
	caServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	caServer.Close()
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caServer.Certificate().Raw}))
	caJSON, err := sonic.Marshal(caPEM)
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		body     string
		wantCode int
		wantType string
		wantURL  string
	}{
		{"http", `{"enabled":true,"type":"http","url":"http://10.0.0.5:3128","enable_for_inference":true}`, fasthttp.StatusOK, "http", "http://10.0.0.5:3128"},
		{"socks5", `{"enabled":true,"type":"socks5","url":"socks5://10.0.0.5:1080","username":"svc","password":"s3cret","enable_for_inference":true,"enable_for_api":true}`, fasthttp.StatusOK, "socks5", "socks5://10.0.0.5:1080"},
		{"socks5 bare host:port", `{"enabled":true,"type":"socks5","url":"10.0.0.5:1080"}`, fasthttp.StatusOK, "socks5", "10.0.0.5:1080"},
		{"socks5 upper-case scheme and spaces, as the form accepts", `{"enabled":true,"type":"socks5","url":"  SOCKS5://10.0.0.5:1080  "}`, fasthttp.StatusOK, "socks5", "SOCKS5://10.0.0.5:1080"},
		{"socks5 without url", `{"enabled":true,"type":"socks5","url":""}`, fasthttp.StatusBadRequest, "", ""},
		{"socks5 with an http url", `{"enabled":true,"type":"socks5","url":"http://10.0.0.5:1080"}`, fasthttp.StatusBadRequest, "", ""},
		// A URL the dialer cannot parse is refused: the runtime treats an unparsable
		// proxy URL as no proxy, so saving one would send traffic direct.
		{"socks5 with a non-numeric port", `{"enabled":true,"type":"socks5","url":"socks5://10.0.0.5:bad"}`, fasthttp.StatusBadRequest, "", ""},
		{"http with a non-numeric port", `{"enabled":true,"type":"http","url":"http://10.0.0.5:bad"}`, fasthttp.StatusBadRequest, "", ""},
		{"http without a host", `{"enabled":true,"type":"http","url":"http://:3128"}`, fasthttp.StatusBadRequest, "", ""},
		// The destination rule applies to the proxy host whatever the scheme.
		{"socks5 on a link-local host", `{"enabled":true,"type":"socks5","url":"socks5://169.254.169.254:1080"}`, fasthttp.StatusBadRequest, "", ""},
		// An http proxy is reached over http:// or https:// only; the dialer refuses any
		// other scheme at runtime, so it must not be saved.
		{"http with an ftp url", `{"enabled":true,"type":"http","url":"ftp://10.0.0.5:3128"}`, fasthttp.StatusBadRequest, "", ""},
		{"http with a socks5 url", `{"enabled":true,"type":"http","url":"socks5://10.0.0.5:1080"}`, fasthttp.StatusBadRequest, "", ""},
		{"tcp", `{"enabled":true,"type":"tcp","url":"tcp://10.0.0.5:9000"}`, fasthttp.StatusBadRequest, "", ""},
		// The proxy CA is stored as sent; one that is not a PEM certificate is refused
		// rather than silently ignored by every client that would have trusted it.
		{"https with a proxy CA", `{"enabled":true,"type":"http","url":"https://10.0.0.5:3129","ca_cert_pem":` + string(caJSON) + `}`, fasthttp.StatusOK, "http", "https://10.0.0.5:3129"},
		{"proxy CA that is not PEM", `{"enabled":true,"type":"http","url":"https://10.0.0.5:3129","ca_cert_pem":"not a certificate"}`, fasthttp.StatusBadRequest, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newRealConfigStore(t)
			cfg := newTestConfig(store, false)
			manager := &proxyReloadRecorder{}
			h := &ConfigHandler{store: cfg, configManager: manager}

			ctx := putConfigCtx(tc.body)
			h.updateProxyConfig(ctx)
			require.Equal(t, tc.wantCode, ctx.Response.StatusCode(), string(ctx.Response.Body()))
			if tc.wantCode != fasthttp.StatusOK {
				assert.Nil(t, manager.reloaded, "a refused proxy config must not reach the runtime")
				return
			}
			stored, err := store.GetProxyConfig(bgCtx())
			require.NoError(t, err)
			require.NotNil(t, manager.reloaded)
			assert.Equal(t, tc.wantType, string(stored.Type), "the proxy type must be persisted")
			assert.Equal(t, tc.wantType, string(manager.reloaded.Type), "the proxy type must reach the runtime")
			assert.Equal(t, tc.wantURL, stored.URL, "the proxy URL must be persisted trimmed")
			if strings.Contains(tc.body, "ca_cert_pem") {
				assert.Equal(t, caPEM, stored.CACertPEM, "the proxy CA must be persisted")
				assert.Equal(t, caPEM, manager.reloaded.CACertPEM, "the proxy CA must reach the runtime")
			}
		})
	}
}

// TestProxyConfig_URLPasswordRedacted pins that a password carried in the proxy URL's
// userinfo never comes back from GET, as the separate password field does not: the
// dialers use URL userinfo as proxy credentials for http and socks5 proxies alike. The
// settings page saves what GET returned, so a PUT that sends the redacted URL back keeps
// the stored password instead of saving the placeholder.
func TestProxyConfig_URLPasswordRedacted(t *testing.T) {
	SetLogger(&mockLogger{})
	for _, tc := range []struct {
		name     string
		proxy    string
		stored   string
		redacted string
	}{
		{"socks5", "socks5", "socks5://svc:s3cret@10.0.0.5:1080", "socks5://svc:<redacted>@10.0.0.5:1080"},
		{"socks5 bare host:port", "socks5", "svc:s3cret@10.0.0.5:1080", "svc:<redacted>@10.0.0.5:1080"},
		{"http", "http", "http://svc:s3cret@10.0.0.5:3128", "http://svc:<redacted>@10.0.0.5:3128"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newRealConfigStore(t)
			cfg := newTestConfig(store, false)
			h := &ConfigHandler{store: cfg, configManager: &proxyReloadRecorder{}}
			require.NoError(t, store.UpdateProxyConfig(bgCtx(), &configtables.GlobalProxyConfig{
				Enabled: true, Type: network.GlobalProxyType(tc.proxy), URL: tc.stored, EnableForInference: true,
			}))

			get := bgCtx()
			h.getProxyConfig(get)
			require.Equal(t, fasthttp.StatusOK, get.Response.StatusCode(), string(get.Response.Body()))
			var proxy configtables.GlobalProxyConfig
			require.NoError(t, json.Unmarshal(get.Response.Body(), &proxy))
			assert.Equal(t, tc.redacted, proxy.URL, "GET /api/proxy-config must not return the URL's password")

			all := bgCtx()
			h.getConfig(all)
			require.Equal(t, fasthttp.StatusOK, all.Response.StatusCode(), string(all.Response.Body()))
			var config struct {
				ProxyConfig configtables.GlobalProxyConfig `json:"proxy_config"`
			}
			require.NoError(t, json.Unmarshal(all.Response.Body(), &config))
			assert.Equal(t, tc.redacted, config.ProxyConfig.URL, "GET /api/config must not return the URL's password")

			body, err := json.Marshal(map[string]any{"enabled": true, "type": tc.proxy, "url": tc.redacted, "enable_for_inference": true})
			require.NoError(t, err)
			put := putConfigCtx(string(body))
			h.updateProxyConfig(put)
			require.Equal(t, fasthttp.StatusOK, put.Response.StatusCode(), string(put.Response.Body()))
			saved, err := store.GetProxyConfig(bgCtx())
			require.NoError(t, err)
			assert.Equal(t, tc.stored, saved.URL, "saving the redacted URL must keep the stored password")
		})
	}

	// With no stored password, the placeholder is dropped rather than saved as one.
	t.Run("no stored password", func(t *testing.T) {
		store := newRealConfigStore(t)
		cfg := newTestConfig(store, false)
		h := &ConfigHandler{store: cfg, configManager: &proxyReloadRecorder{}}
		put := putConfigCtx(`{"enabled":true,"type":"socks5","url":"socks5://svc:<redacted>@10.0.0.5:1080"}`)
		h.updateProxyConfig(put)
		require.Equal(t, fasthttp.StatusOK, put.Response.StatusCode(), string(put.Response.Body()))
		saved, err := store.GetProxyConfig(bgCtx())
		require.NoError(t, err)
		assert.Equal(t, "socks5://svc@10.0.0.5:1080", saved.URL)
	})
}

// TestUpdateProxyConfig_ErrorsDoNotEchoURLPassword pins that a refused proxy URL never
// quotes its password back. The PUT restores the stored password into a URL that keeps
// the redacted placeholder before validating it, so an error that echoed the URL would
// hand back the password GET hides.
func TestUpdateProxyConfig_ErrorsDoNotEchoURLPassword(t *testing.T) {
	SetLogger(&mockLogger{})
	for _, tc := range []struct{ name, url string }{
		{"non-numeric port", "socks5://svc:<redacted>@10.0.0.5:bad"},
		{"port 0", "socks5://svc:<redacted>@10.0.0.5:0"},
		{"no host", "socks5://svc:<redacted>@:1080"},
		{"unparsable host", "socks5://svc:<redacted>@[::1:1080"},
		{"wrong scheme", "http://svc:<redacted>@10.0.0.5:1080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newRealConfigStore(t)
			cfg := newTestConfig(store, false)
			h := &ConfigHandler{store: cfg, configManager: &proxyReloadRecorder{}}
			require.NoError(t, store.UpdateProxyConfig(bgCtx(), &configtables.GlobalProxyConfig{
				Enabled: true, Type: network.GlobalProxyTypeSOCKS5, URL: "socks5://svc:s3cret@10.0.0.5:1080",
			}))
			body, err := json.Marshal(map[string]any{"enabled": true, "type": "socks5", "url": tc.url})
			require.NoError(t, err)
			put := putConfigCtx(string(body))
			h.updateProxyConfig(put)
			require.Equal(t, fasthttp.StatusBadRequest, put.Response.StatusCode(), string(put.Response.Body()))
			assert.NotContains(t, string(put.Response.Body()), "s3cret", "the 400 must not quote the stored password")
		})
	}
}

// TestUpdateConfig_ConcurrentSavesAreSerialized pins that overlapping config saves each
// succeed: updateConfig serializes them from snapshot through publication.
func TestUpdateConfig_ConcurrentSavesAreSerialized(t *testing.T) {
	SetLogger(&mockLogger{})
	store := newRealConfigStore(t)
	cfg := newTestConfig(store, false)
	h := &ConfigHandler{store: cfg, configManager: stubConfigManager{}}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	for _, days := range []string{"7", "9"} {
		wg.Add(1)
		go func(days string) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				ctx := putConfigCtx(`{"client_config":{"log_retention_days":` + days + `}}`)
				h.updateConfig(ctx)
				if ctx.Response.StatusCode() != fasthttp.StatusOK {
					mu.Lock()
					failures = append(failures, string(ctx.Response.Body()))
					mu.Unlock()
				}
			}
		}(days)
	}
	wg.Wait()
	require.Empty(t, failures, "overlapping saves must each succeed")
}

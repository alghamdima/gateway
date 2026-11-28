package lib

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/grant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// Who a request is, as the transport settles it: read once off what the middlewares and headers
// left on the context, onto a grant every request context carries.

func TestRecordingInstallsAGrantWhenTheContextCarriesNone(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.Nil(t, ctx.Grant())

	RecordCredential(ctx, grant.NewCredential(grant.CredentialVirtualKey, "sk-bf-1"))
	require.NotNil(t, ctx.Grant(), "recording on a bare context installs its grant")
	installed := ctx.Grant()
	assert.Equal(t, "sk-bf-1", installed.Identity().Credential().Value)

	RecordCredential(ctx, grant.NewCredential(grant.CredentialAPIKey, "key-7"))
	assert.Same(t, installed, ctx.Grant(), "and keeps using it")
	assert.Equal(t, "key-7", ctx.Grant().Identity().Credential().Value)

	// A nil context has nothing to record on, and must not panic.
	RecordCredential(nil, grant.NewCredential(grant.CredentialVirtualKey, "sk-bf-1"))
}

func TestCredentialFromContext(t *testing.T) {
	stamped := grant.NewCredential(grant.CredentialAPIKey, "key-7")
	tests := []struct {
		name   string
		values map[any]any
		want   schemas.Credential
	}{
		{name: "nothing presented", values: nil, want: schemas.Credential{}},
		{
			name:   "a virtual key in a header",
			values: map[any]any{schemas.BifrostContextKeyVirtualKey: "sk-bf-1"},
			want:   grant.NewCredential(grant.CredentialVirtualKey, "sk-bf-1"),
		},
		{
			name:   "what the middleware recorded",
			values: map[any]any{schemas.BifrostContextKeyAuthCredential: stamped, schemas.BifrostContextKeyUserID: "u1"},
			want:   stamped,
		},
		{
			name: "a virtual key goes first",
			values: map[any]any{
				schemas.BifrostContextKeyVirtualKey:     "sk-bf-1",
				schemas.BifrostContextKeyAuthCredential: stamped,
			},
			want: grant.NewCredential(grant.CredentialVirtualKey, "sk-bf-1"),
		},
		{
			name:   "a user with nothing recorded is a user, not a credential",
			values: map[any]any{schemas.BifrostContextKeyUserID: "u1", schemas.IsAPIKeyAuthContextKey: true},
			want:   schemas.Credential{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			for key, value := range tt.values {
				ctx.SetValue(key, value)
			}
			assert.Equal(t, tt.want, credentialFromContext(ctx))
		})
	}
}

func TestUserFromContext(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	assert.Nil(t, userFromContext(ctx), "no user id means no user")

	ctx.SetValue(schemas.BifrostContextKeyUserID, "u1")
	assert.Equal(t, &schemas.UserRef{ID: "u1"}, userFromContext(ctx))

	ctx.SetValue(schemas.BifrostContextKeyUserName, "Ada")
	ctx.SetValue(schemas.BifrostContextKeyUserEmail, "ada@example.com")
	assert.Equal(t, &schemas.UserRef{ID: "u1", Name: "Ada", Email: "ada@example.com"}, userFromContext(ctx))
}

func TestRecordCredentialKeepsTheUser(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	RecordCredential(ctx, schemas.Credential{})
	assert.Nil(t, ctx.Grant(), "recording nothing records nothing, and installs nothing")

	g := grant.New()
	ctx.SetGrant(g)
	g.SetIdentity(grant.NewIdentity(schemas.Credential{}, &schemas.UserRef{ID: "u1", Name: "Ada"}, nil, nil, nil))

	RecordCredential(ctx, grant.NewCredential(grant.CredentialVirtualKey, "sk-bf-1"))
	assert.Equal(t, grant.NewCredential(grant.CredentialVirtualKey, "sk-bf-1"), g.Identity().Credential())
	assert.Equal(t, "u1", g.Identity().User().ID)
}

func TestRecordingKeepsWhatResolutionSettled(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	g := grant.New()
	ctx.SetGrant(g)
	g.SetIdentity(grant.NewIdentity(
		grant.NewCredential(grant.CredentialVirtualKey, "sk-bf-1"),
		&schemas.UserRef{ID: "u1"},
		&schemas.EntityRef{ID: "vk1", Name: "Key"},
		[]schemas.EntityRef{{ID: "t1"}},
		[]schemas.EntityRef{{ID: "c1"}},
	))

	RecordCredential(ctx, grant.NewCredential(grant.CredentialAPIKey, "key-7"))
	identity := g.Identity()
	assert.Equal(t, "key-7", identity.Credential().Value)
	assert.Equal(t, "vk1", identity.VirtualKey().ID)
	assert.Equal(t, []schemas.EntityRef{{ID: "t1"}}, identity.Teams())
	assert.Equal(t, []schemas.EntityRef{{ID: "c1"}}, identity.Customers())
	assert.Equal(t, "u1", identity.User().ID)
}

func TestConvertToBifrostContextSettlesTheIdentity(t *testing.T) {
	t.Run("a virtual key header", func(t *testing.T) {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.Set("x-bf-vk", "sk-bf-abc")
		converted, cancel := ConvertToBifrostContext(ctx, testHandlerStore{})
		defer cancel()

		require.NotNil(t, converted.Grant())
		identity := converted.Grant().Identity()
		require.NotNil(t, identity)
		assert.Equal(t, grant.NewCredential(grant.CredentialVirtualKey, "sk-bf-abc"), identity.Credential())
		assert.Nil(t, identity.User())
	})

	t.Run("what an upstream middleware verified and resolved", func(t *testing.T) {
		ctx := &fasthttp.RequestCtx{}
		ctx.SetUserValue(schemas.BifrostContextKeyAuthCredential, grant.NewCredential(grant.CredentialAPIKey, "key-7"))
		ctx.SetUserValue(schemas.BifrostContextKeyUserID, "u1")
		ctx.SetUserValue(schemas.BifrostContextKeyUserName, "Ada")
		converted, cancel := ConvertToBifrostContext(ctx, testHandlerStore{})
		defer cancel()

		identity := converted.Grant().Identity()
		require.NotNil(t, identity)
		assert.Equal(t, grant.NewCredential(grant.CredentialAPIKey, "key-7"), identity.Credential())
		assert.Equal(t, &schemas.UserRef{ID: "u1", Name: "Ada"}, identity.User())
	})

	t.Run("a virtual key header goes first, the session's user is kept", func(t *testing.T) {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.Set("x-bf-vk", "sk-bf-abc")
		ctx.SetUserValue(schemas.BifrostContextKeyAuthCredential, grant.NewCredential(grant.CredentialSessionToken, "u1"))
		ctx.SetUserValue(schemas.BifrostContextKeyUserID, "u1")
		converted, cancel := ConvertToBifrostContext(ctx, testHandlerStore{})
		defer cancel()

		identity := converted.Grant().Identity()
		require.NotNil(t, identity)
		assert.Equal(t, grant.NewCredential(grant.CredentialVirtualKey, "sk-bf-abc"), identity.Credential())
		assert.Equal(t, "u1", identity.User().ID)
	})

	t.Run("nothing presented is settled as nobody, not left unsettled", func(t *testing.T) {
		ctx := &fasthttp.RequestCtx{}
		converted, cancel := ConvertToBifrostContext(ctx, testHandlerStore{})
		defer cancel()

		require.NotNil(t, converted.Grant())
		identity := converted.Grant().Identity()
		require.NotNil(t, identity)
		assert.False(t, identity.Presented())
		assert.Nil(t, identity.User())
		assert.Nil(t, converted.Grant().Access(), "nothing resolved yet")
	})

	t.Run("a shared request context keeps the grant it already has", func(t *testing.T) {
		ctx := &fasthttp.RequestCtx{}
		first, cancelFirst := ConvertToBifrostContext(ctx, testHandlerStore{})
		defer cancelFirst()
		second, cancelSecond := ConvertToBifrostContext(ctx, testHandlerStore{})
		defer cancelSecond()
		assert.Same(t, first.Grant(), second.Grant())
	})
}

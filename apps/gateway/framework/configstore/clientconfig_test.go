package configstore

import (
	"testing"

	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/require"
)

// The entity access profile mode is part of what decides whether a changed config.json replaces the
// stored client config: intersect must change the hash, while unset and an explicit union must hash
// the same,
// so an upgrade that starts writing the default does not churn every deployment's hash.
func TestGenerateClientConfigHashCoversEntityAccessProfileMode(t *testing.T) {
	hash := func(mode tables.EntityAccessProfileMode) string {
		t.Helper()
		h, err := (&ClientConfig{EntityAccessProfileMode: mode}).GenerateClientConfigHash()
		require.NoError(t, err)
		return h
	}
	unset := hash("")
	require.Equal(t, unset, hash(tables.EntityAccessProfileModeUnion), "union is the default and hashes as unset")
	require.NotEqual(t, unset, hash(tables.EntityAccessProfileModeIntersect), "intersect is a change")
}

package logstore

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// governanceCtx builds a hook context carrying the values a fully settled request
// leaves behind.
func governanceCtx(t *testing.T, values map[schemas.BifrostContextKey]any) *schemas.BifrostContext {
	t.Helper()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	for key, value := range values {
		ctx.SetValue(key, value)
	}
	return ctx
}

func assertStrings(t *testing.T, field string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", field, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", field, got, want)
		}
	}
}


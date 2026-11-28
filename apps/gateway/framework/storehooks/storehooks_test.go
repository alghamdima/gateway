package storehooks

import (
	"testing"

	"gorm.io/gorm"
)

// TestPoolOpenedRunsRegisteredHooks checks hooks see the role and pool, and a nil pool is ignored.
func TestPoolOpenedRunsRegisteredHooks(t *testing.T) {
	var got []Role
	RegisterPoolOpened(func(role Role, db *gorm.DB) { got = append(got, role) })
	RegisterPoolOpened(nil)

	PoolOpened(RoleConfig, &gorm.DB{})
	PoolOpened(RoleLogs, nil)
	PoolOpened(RoleLogs, &gorm.DB{})

	if len(got) != 2 || got[0] != RoleConfig || got[1] != RoleLogs {
		t.Fatalf("hooks saw %v, want [config logs]", got)
	}
}

// RegisterPoolOpened adds fn to the hooks run for every new runtime pool. Call
// it before the stores are created; pools opened earlier are not replayed.
func RegisterPoolOpened(fn PoolOpenedFn) {
	if fn == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	hooks = append(hooks, fn)
}

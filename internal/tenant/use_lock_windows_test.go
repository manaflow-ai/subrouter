//go:build windows

package tenant

import "testing"

func TestWindowsTenantUseLockDefersDeletionWhileRequestIsActive(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	shared, err := registry.AcquireUse("team-1")
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()

	exclusive, acquired, err := registry.TryAcquireExclusiveUse("team-1")
	if err != nil {
		t.Fatal(err)
	}
	if acquired {
		exclusive.Close()
		t.Fatal("exclusive tenant lock acquired while a request held the shared lock")
	}
}

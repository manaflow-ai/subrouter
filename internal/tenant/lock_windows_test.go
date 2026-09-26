//go:build windows

package tenant

import (
	"testing"
	"time"
)

func TestWindowsRegistryLockSerializesProcesses(t *testing.T) {
	root := t.TempDir()
	first := NewRegistry(root)
	second := NewRegistry(root)
	firstLock, err := first.lockRegistry()
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		lock *registryLock
		err  error
	}
	acquired := make(chan result, 1)
	go func() {
		lock, err := second.lockRegistry()
		acquired <- result{lock: lock, err: err}
	}()
	select {
	case result := <-acquired:
		if result.lock != nil {
			result.lock.Close()
		}
		t.Fatalf("second registry lock acquired early: %v", result.err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := firstLock.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-acquired:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if err := result.lock.Close(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("second registry lock did not acquire after release")
	}
}

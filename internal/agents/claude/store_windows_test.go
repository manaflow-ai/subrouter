//go:build windows

package claude

import (
	"fmt"
	"sync"
	"testing"
)

func TestProfileRegistryReadsDoNotBlockAtomicReplacement(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	const writes = 100
	const readers = 8

	start := make(chan struct{})
	errors := make(chan error, writes+readers)
	var wait sync.WaitGroup
	for reader := 0; reader < readers; reader++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for iteration := 0; iteration < writes*20; iteration++ {
				_ = store.ListProfiles()
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		<-start
		for index := 0; index < writes; index++ {
			if _, err := store.CreateProfile(fmt.Sprintf("profile-%03d", index)); err != nil {
				errors <- err
				return
			}
		}
	}()
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Fatalf("atomic profile replacement failed during a concurrent read: %v", err)
	}
	if got := len(store.ListProfiles()); got != writes {
		t.Fatalf("profiles = %d, want %d", got, writes)
	}
}

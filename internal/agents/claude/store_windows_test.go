//go:build windows

package claude

import (
	"fmt"
	"path/filepath"
	"strings"
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

func TestProfileRegistryReadsSupportLongWindowsPaths(t *testing.T) {
	dir := t.TempDir()
	for len(dir) < 300 {
		dir = filepath.Join(dir, strings.Repeat("segment", 8))
	}
	store := Store{Dir: dir}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	profiles := store.ListProfiles()
	if len(profiles) != 1 || profiles[0].Name != "work" {
		t.Fatalf("profiles through long state path = %+v, want work", profiles)
	}
}

func TestWindowsExtendedLengthPathPreservesUNCMeaning(t *testing.T) {
	unc := `\\server\share\` + strings.Repeat(`deep\`, 60) + `claude.json`
	wantUNC := `\\?\UNC\server\share\` + strings.Repeat(`deep\`, 60) + `claude.json`
	if got := windowsExtendedLengthPath(unc); got != wantUNC {
		t.Fatalf("extended UNC path = %q, want %q", got, wantUNC)
	}
	drive := `C:\` + strings.Repeat(`deep\`, 60) + `claude.json`
	wantDrive := `\\?\` + drive
	if got := windowsExtendedLengthPath(drive); got != wantDrive {
		t.Fatalf("extended drive path = %q, want %q", got, wantDrive)
	}
	alreadyExtended := `\\?\C:\` + strings.Repeat(`deep\`, 60) + `claude.json`
	if got := windowsExtendedLengthPath(alreadyExtended); got != alreadyExtended {
		t.Fatalf("already extended path changed to %q", got)
	}
}

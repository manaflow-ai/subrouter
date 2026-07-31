package claude

import (
	"path/filepath"
	"sync"
)

type profileCredentialProcessLockEntry struct {
	mu   sync.Mutex
	refs int
}

var profileCredentialProcessLocks = struct {
	sync.Mutex
	entries map[string]*profileCredentialProcessLockEntry
}{entries: make(map[string]*profileCredentialProcessLockEntry)}

func lockProfileCredentialProcess(path string) func() {
	key := filepath.Clean(path)
	profileCredentialProcessLocks.Lock()
	entry := profileCredentialProcessLocks.entries[key]
	if entry == nil {
		entry = &profileCredentialProcessLockEntry{}
		profileCredentialProcessLocks.entries[key] = entry
	}
	entry.refs++
	profileCredentialProcessLocks.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		profileCredentialProcessLocks.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(profileCredentialProcessLocks.entries, key)
		}
		profileCredentialProcessLocks.Unlock()
	}
}

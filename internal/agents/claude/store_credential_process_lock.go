package claude

import (
	"context"
	"path/filepath"
	"sync"
)

type profileCredentialProcessLockEntry struct {
	token chan struct{}
	refs  int
}

var profileCredentialProcessLocks = struct {
	sync.Mutex
	entries map[string]*profileCredentialProcessLockEntry
}{entries: make(map[string]*profileCredentialProcessLockEntry)}

func lockProfileCredentialProcess(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := filepath.Clean(path)
	profileCredentialProcessLocks.Lock()
	entry := profileCredentialProcessLocks.entries[key]
	if entry == nil {
		entry = &profileCredentialProcessLockEntry{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		profileCredentialProcessLocks.entries[key] = entry
	}
	entry.refs++
	profileCredentialProcessLocks.Unlock()

	select {
	case <-entry.token:
		if err := ctx.Err(); err != nil {
			entry.token <- struct{}{}
			releaseProfileCredentialProcessLockRef(key, entry)
			return nil, err
		}
		return func() {
			entry.token <- struct{}{}
			releaseProfileCredentialProcessLockRef(key, entry)
		}, nil
	case <-ctx.Done():
		releaseProfileCredentialProcessLockRef(key, entry)
		return nil, ctx.Err()
	}
}

func releaseProfileCredentialProcessLockRef(
	key string,
	entry *profileCredentialProcessLockEntry,
) {
	profileCredentialProcessLocks.Lock()
	entry.refs--
	if entry.refs == 0 {
		delete(profileCredentialProcessLocks.entries, key)
	}
	profileCredentialProcessLocks.Unlock()
}

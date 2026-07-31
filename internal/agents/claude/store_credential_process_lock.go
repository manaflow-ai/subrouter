package claude

import (
	"hash/fnv"
	"path/filepath"
	"sync"
)

const profileCredentialProcessLockShards = 64

var profileCredentialProcessLocks [profileCredentialProcessLockShards]sync.Mutex

func lockProfileCredentialProcess(path string) func() {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(filepath.Clean(path)))
	lock := &profileCredentialProcessLocks[hasher.Sum32()%profileCredentialProcessLockShards]
	lock.Lock()
	return lock.Unlock
}

package cutovercanary

import (
	"errors"
	"os"
	"runtime"
)

var errChallengeActive = errors.New("existing-session challenge already active")
var errJournalActive = errors.New("cleanup journal already active")
var errStateActive = errors.New("authenticated handoff state already active")

type challengeLease struct {
	file *os.File
}

func acquireChallengeLease(challengePath string) (*challengeLease, error) {
	return acquireArtifactLease(challengePath, errChallengeActive)
}

func acquireJournalLease(journalPath string) (*challengeLease, error) {
	return acquireArtifactLease(journalPath, errJournalActive)
}

func acquireStateLease(statePath string) (*challengeLease, error) {
	return acquireArtifactLease(statePath, errStateActive)
}

func acquireArtifactLease(artifactPath string, activeError error) (*challengeLease, error) {
	if err := validateOutputPath(artifactPath); err != nil {
		return nil, err
	}
	lockPath := artifactPath + ".lock"
	if err := validateOutputPath(lockPath); err != nil {
		return nil, err
	}
	file, err := openArtifactLock(lockPath)
	if err != nil {
		return nil, errors.New("cannot open canary artifact lock")
	}
	if err := validateOpenedArtifactLock(file, lockPath); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, errors.New("cannot secure canary artifact lock")
	}
	if err := validateOpenedArtifactLock(file, lockPath); err != nil {
		file.Close()
		return nil, err
	}
	if err := tryLockChallengeFile(file); err != nil {
		file.Close()
		if errors.Is(err, errChallengeActive) {
			return nil, activeError
		}
		return nil, errors.New("cannot lock canary artifact")
	}
	return &challengeLease{file: file}, nil
}

func validateOpenedArtifactLock(file *os.File, lockPath string) error {
	opened, err := file.Stat()
	if err != nil {
		return errors.New("cannot inspect canary artifact lock")
	}
	named, err := os.Lstat(lockPath)
	if err != nil || named.Mode()&os.ModeSymlink != 0 || !named.Mode().IsRegular() ||
		!opened.Mode().IsRegular() || !os.SameFile(opened, named) {
		return errors.New("canary artifact lock changed during open")
	}
	if opened.Mode().Perm()&0o077 != 0 || named.Mode().Perm()&0o077 != 0 {
		return errors.New("canary artifact lock must be private")
	}
	if runtime.GOOS != "windows" && (fileUID(opened) != os.Getuid() || fileUID(named) != os.Getuid()) {
		return errors.New("canary artifact lock must be owned by the current user")
	}
	return nil
}

func (lease *challengeLease) Close() error {
	if lease == nil || lease.file == nil {
		return nil
	}
	unlockErr := unlockChallengeFile(lease.file)
	closeErr := lease.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func publishChallenge(challengePath, witnessPath string, challenge Challenge) (*challengeLease, error) {
	lease, err := acquireChallengeLease(challengePath)
	if err != nil {
		return nil, err
	}
	// Holding the kernel lease proves that no live cooperating coordinator owns
	// these artifacts. A prior SIGKILL can leave them behind, so remove them only
	// after acquiring the lease and publish the replacement without overwrite.
	if err := removeIfExists(challengePath); err != nil {
		lease.Close()
		return nil, err
	}
	if err := removeIfExists(witnessPath); err != nil {
		lease.Close()
		return nil, err
	}
	if err := createPrivateJSON(challengePath, challenge); err != nil {
		lease.Close()
		return nil, err
	}
	return lease, nil
}

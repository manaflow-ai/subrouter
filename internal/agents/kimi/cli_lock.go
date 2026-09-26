package kimi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const (
	cliRefreshLockStale     = 5 * time.Second
	cliRefreshLockHeartbeat = 2 * time.Second
	cliRefreshLockPoll      = 100 * time.Millisecond
	cliRefreshLockTimeout   = 60 * time.Second
)

// lockLocalCLIRefresh interoperates with Kimi Code's proper-lockfile lock at
// <KIMI_CODE_HOME>/oauth/kimi-code.lock. Refresh tokens rotate on use, so the
// CLI and Subrouter must serialize and re-read the credential after acquiring
// this lock or one process can invalidate the other's saved token.
func (s Store) lockLocalCLIRefresh(ctx context.Context) (*cliRefreshLock, error) {
	// The official client disables this filesystem lock on Windows. A Go-side
	// lock there would provide a false guarantee because the peer CLI would not
	// participate.
	if runtime.GOOS == "windows" {
		lockCtx, cancel := context.WithCancel(ctx)
		return &cliRefreshLock{ctx: lockCtx, cancel: cancel}, nil
	}
	home, err := s.cliHome()
	if err != nil {
		return nil, fmt.Errorf("resolve Kimi Code OAuth lock: %w", err)
	}
	oauthDir := filepath.Join(home, "oauth")
	if err := os.MkdirAll(oauthDir, 0o700); err != nil {
		return nil, fmt.Errorf("prepare Kimi Code OAuth lock: %w", err)
	}
	target := filepath.Join(oauthDir, accountID)
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("prepare Kimi Code OAuth lock: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("prepare Kimi Code OAuth lock: %w", err)
	}
	lockDir := target + ".lock"
	waitCtx, cancel := context.WithTimeout(ctx, cliRefreshLockTimeout)
	defer cancel()
	for {
		if err := os.Mkdir(lockDir, 0o700); err == nil {
			identity, statErr := os.Stat(lockDir)
			if statErr != nil {
				_ = os.Remove(lockDir)
				return nil, fmt.Errorf("confirm Kimi Code OAuth refresh lock: %w", statErr)
			}
			return maintainCLILock(ctx, lockDir, identity), nil
		} else if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("acquire Kimi Code OAuth refresh lock: %w", err)
		}
		if staleCLILock(lockDir, time.Now()) {
			// An empty-directory lock has no portable compare-and-delete
			// operation. Removing it after a stale Stat can displace an owner that
			// heartbeats in the gap and let two refresh-token exchanges overlap.
			// Fail closed; a human can verify no Kimi process is running before
			// removing the named stale directory.
			return nil, fmt.Errorf("Kimi Code OAuth refresh lock appears stale; verify no Kimi process is running, then remove %s", lockDir)
		}
		timer := time.NewTimer(cliRefreshLockPoll)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return nil, fmt.Errorf("acquire Kimi Code OAuth refresh lock: %w", waitCtx.Err())
		case <-timer.C:
		}
	}
}

func staleCLILock(path string, now time.Time) bool {
	first, err := os.Stat(path)
	if err != nil || !first.IsDir() || now.Sub(first.ModTime()) <= cliRefreshLockStale {
		return false
	}
	second, err := os.Stat(path)
	return err == nil && second.IsDir() && second.ModTime().Equal(first.ModTime()) && now.Sub(second.ModTime()) > cliRefreshLockStale
}

type cliRefreshLock struct {
	ctx      context.Context
	cancel   context.CancelFunc
	path     string
	identity os.FileInfo
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once

	mu         sync.Mutex
	lostErr    error
	releaseErr error
}

func maintainCLILock(parent context.Context, path string, identity os.FileInfo) *cliRefreshLock {
	ctx, cancel := context.WithCancel(parent)
	lock := &cliRefreshLock{
		ctx: ctx, cancel: cancel, path: path, identity: identity,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go func() {
		defer close(lock.done)
		ticker := time.NewTicker(cliRefreshLockHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := lock.Check(); err != nil {
					return
				}
				now := time.Now()
				if err := os.Chtimes(path, now, now); err != nil {
					lock.markLost(fmt.Errorf("heartbeat Kimi Code OAuth refresh lock: %w", err))
					return
				}
			case <-lock.stop:
				return
			case <-lock.ctx.Done():
				return
			}
		}
	}()
	return lock
}

func (l *cliRefreshLock) Context() context.Context {
	return l.ctx
}

func (l *cliRefreshLock) markLost(err error) {
	l.mu.Lock()
	if l.lostErr == nil {
		l.lostErr = err
		l.cancel()
	}
	l.mu.Unlock()
}

func (l *cliRefreshLock) Check() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	lostErr := l.lostErr
	l.mu.Unlock()
	if lostErr != nil {
		return lostErr
	}
	if l.path == "" {
		return nil
	}
	current, err := os.Stat(l.path)
	if err != nil {
		l.markLost(fmt.Errorf("Kimi Code OAuth refresh lock was lost: %w", err))
	} else if !current.IsDir() || !os.SameFile(l.identity, current) {
		l.markLost(fmt.Errorf("Kimi Code OAuth refresh lock ownership changed"))
	}
	l.mu.Lock()
	lostErr = l.lostErr
	l.mu.Unlock()
	return lostErr
}

func (l *cliRefreshLock) Release() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if l.stop != nil {
			close(l.stop)
			<-l.done
		}
		if l.path != "" {
			if err := l.Check(); err != nil {
				l.releaseErr = err
			} else if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				l.releaseErr = fmt.Errorf("release Kimi Code OAuth refresh lock: %w", err)
			}
		}
		l.cancel()
	})
	return l.releaseErr
}

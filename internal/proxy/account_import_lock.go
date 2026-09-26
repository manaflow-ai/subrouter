package proxy

import (
	"context"
	"errors"
	"sync"
	"time"
)

const accountImportLockRetryInterval = 10 * time.Millisecond

var errAccountImportTransactionBusy = errors.New("account import transaction is busy")

func lockMutexContext(ctx context.Context, mutex *sync.Mutex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if mutex.TryLock() {
		return nil
	}
	ticker := time.NewTicker(accountImportLockRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := ctx.Err(); err != nil {
				return err
			}
			if mutex.TryLock() {
				return nil
			}
		}
	}
}

func waitAccountImportLockRetry(ctx context.Context) error {
	timer := time.NewTimer(accountImportLockRetryInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

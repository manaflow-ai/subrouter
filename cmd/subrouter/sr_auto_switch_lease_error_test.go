package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// brokenLockLease returns a lease whose ".lock" sibling cannot be created for
// a reason other than "already exists": its name is one byte short of the
// filesystem limit once ".lock" is appended.
func brokenLockLease(t *testing.T) srAutoSwitchLease {
	t.Helper()
	return srAutoSwitchLease{path: filepath.Join(t.TempDir(), strings.Repeat("l", 252))}
}

// A lock failure other than "held by another worker" used to read as "not
// claimed" with no error, so every tick skipped the sweep forever and nothing
// was logged.
func TestAutoSwitchLeaseSurfacesLockErrorsAndStillAdmits(t *testing.T) {
	claimed, err := brokenLockLease(t).acquire(time.Minute)
	if err == nil {
		t.Fatal("acquire hid a lock failure")
	}
	if !claimed {
		t.Fatal("a lease failure disabled the sweep; it must degrade to sweeping")
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunSRAutoSwitchLogsRepeatedLeaseErrorOnce(t *testing.T) {
	var logs lockedBuffer
	var mu sync.Mutex
	sweeps := 0
	cfg := srAutoSwitchConfig{
		Interval: 10 * time.Millisecond,
		Accounts: []accounts.Account{{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth}},
		Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
		Lease:    brokenLockLease(t),
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{{AccountID: "a@example.com", Headroom: 1, ShortHeadroom: 1, Fresh: true}}, 1
		},
		SwitchActive: func(context.Context, string) error {
			mu.Lock()
			sweeps++
			mu.Unlock()
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	runSRAutoSwitch(ctx, cfg)

	mu.Lock()
	got := sweeps
	mu.Unlock()
	if got < 2 {
		t.Fatalf("sweeps = %d; a broken lease must not stop the sweep", got)
	}
	if n := strings.Count(logs.String(), "lease unavailable"); n != 1 {
		t.Fatalf("lease warning logged %d times, want once per distinct error:\n%s", n, logs.String())
	}
}

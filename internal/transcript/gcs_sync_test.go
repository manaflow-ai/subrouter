package transcript

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGCSSyncOnceUsesGsutilRsync(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "transcripts")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "gsutil.log")
	fakeGsutil := filepath.Join(dir, "gsutil")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"" + logPath + "\"\n"
	if err := os.WriteFile(fakeGsutil, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	syncer := NewGCSSyncer(GCSSyncerConfig{
		SourceDir:   source,
		Destination: "gs://example-bucket/subrouter",
		Command:     fakeGsutil,
		Timeout:     5 * time.Second,
	})
	if syncer == nil {
		t.Fatal("syncer was nil")
	}
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "-m rsync -r " + source + " gs://example-bucket/subrouter/\n"
	if string(got) != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestGCSSyncerRejectsNonGCSURI(t *testing.T) {
	syncer := NewGCSSyncer(GCSSyncerConfig{
		SourceDir:   t.TempDir(),
		Destination: "https://example.com/not-gcs",
	})
	if syncer != nil {
		t.Fatal("syncer accepted non-GCS destination")
	}
}

func TestGCSSyncerPrunesOldLocalFilesAfterArchiving(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "transcripts")
	sessionDir := filepath.Join(source, "by-agent", "codex", "by-session")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(sessionDir, "old.jsonl")
	recentPath := filepath.Join(sessionDir, "recent.jsonl")
	if err := os.WriteFile(oldPath, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recentPath, []byte("recent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(dir, "gsutil.log")
	fakeGsutil := filepath.Join(dir, "gsutil")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + logPath + "\"\n"
	if err := os.WriteFile(fakeGsutil, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	syncer := NewGCSSyncer(GCSSyncerConfig{
		SourceDir:      source,
		Destination:    "gs://example-bucket/subrouter",
		Command:        fakeGsutil,
		Timeout:        5 * time.Second,
		LocalRetention: 24 * time.Hour,
	})
	if syncer == nil {
		t.Fatal("syncer was nil")
	}
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old transcript should be pruned, stat error: %v", err)
	}
	if _, err := os.Stat(recentPath); err != nil {
		t.Fatalf("recent transcript should remain: %v", err)
	}
	logBody, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logBody)
	if !strings.Contains(logText, "-m rsync -r "+source+" gs://example-bucket/subrouter/") {
		t.Fatalf("missing rsync command:\n%s", logText)
	}
	if !strings.Contains(logText, "cp -n gs://example-bucket/subrouter/by-agent/codex/by-session/old.jsonl gs://example-bucket/subrouter/_archive/by-agent/codex/by-session/old.jsonl/") {
		t.Fatalf("missing archive command:\n%s", logText)
	}
}

func TestGCSSyncerKeepsLocalFileWhenArchiveFails(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "transcripts")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(source, "old.jsonl")
	if err := os.WriteFile(oldPath, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	fakeGsutil := filepath.Join(dir, "gsutil")
	script := `#!/bin/sh
case "$1" in
  cp) exit 7 ;;
  *) exit 0 ;;
esac
`
	if err := os.WriteFile(fakeGsutil, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	syncer := NewGCSSyncer(GCSSyncerConfig{
		SourceDir:      source,
		Destination:    "gs://example-bucket/subrouter",
		Command:        fakeGsutil,
		Timeout:        5 * time.Second,
		LocalRetention: 24 * time.Hour,
	})
	if syncer == nil {
		t.Fatal("syncer was nil")
	}
	if err := syncer.SyncOnce(context.Background()); err == nil {
		t.Fatal("expected archive failure")
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("old transcript should remain after archive failure: %v", err)
	}
}

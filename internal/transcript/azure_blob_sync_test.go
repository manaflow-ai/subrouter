package transcript

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// azureBlobFake stands in for the blob service: it stores what was PUT and
// answers HEAD from that store, which is all the syncer needs.
type azureBlobFake struct {
	mu      sync.Mutex
	blobs   map[string][]byte
	puts    []string
	heads   []string
	authOK  bool
	server  *httptest.Server
	failPut bool
}

func newAzureBlobFake(t *testing.T) *azureBlobFake {
	t.Helper()
	fake := &azureBlobFake{blobs: map[string][]byte{}, authOK: true}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		name := strings.TrimPrefix(r.URL.Path, "/transcripts/")
		if !strings.HasPrefix(r.Header.Get("Authorization"), "SharedKey ") ||
			r.Header.Get("x-ms-date") == "" || r.Header.Get("x-ms-version") == "" {
			fake.authOK = false
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodHead:
			fake.heads = append(fake.heads, name)
			body, ok := fake.blobs[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
		case http.MethodPut:
			if fake.failPut {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if r.Header.Get("x-ms-blob-type") != "BlockBlob" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			body, _ := io.ReadAll(r.Body)
			fake.blobs[name] = body
			fake.puts = append(fake.puts, name)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func azureTestSyncer(t *testing.T, fake *azureBlobFake, spool string, config AzureBlobSyncerConfig) *AzureBlobSyncer {
	t.Helper()
	config.SourceDir = spool
	if config.Destination == "" {
		config.Destination = fake.server.URL + "/transcripts/host-a"
	}
	if config.AccountKey == "" {
		config.AccountKey = "c2VjcmV0LWtleQ==" // base64("secret-key")
	}
	if config.Now == nil {
		// Every fixture file is written now, and the syncer deliberately skips
		// files still being appended, so tests look at the spool from later.
		config.Now = func() time.Time { return time.Now().Add(time.Hour) }
	}
	syncer := NewAzureBlobSyncer(config)
	if syncer == nil {
		t.Fatal("syncer was not configured")
	}
	return syncer
}

func writeSpoolFile(t *testing.T, spool, relPath, contents string) string {
	t.Helper()
	path := filepath.Join(spool, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAzureBlobSyncerUploadsAndSkipsUnchanged(t *testing.T) {
	fake := newAzureBlobFake(t)
	spool := t.TempDir()
	writeSpoolFile(t, spool, "codex/2026-08-18/session-1.jsonl", "{\"a\":1}\n")
	syncer := azureTestSyncer(t, fake, spool, AzureBlobSyncerConfig{})

	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	const blob = "host-a/codex/2026-08-18/session-1.jsonl"
	if string(fake.blobs[blob]) != "{\"a\":1}\n" {
		t.Fatalf("blob = %q, want the transcript body", fake.blobs[blob])
	}
	// A second pass must not re-upload an unchanged file: the spool is walked
	// every interval and re-uploading would grow with the archive.
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.puts) != 1 {
		t.Fatalf("uploads = %d, want 1", len(fake.puts))
	}
	if !fake.authOK {
		t.Fatal("a request was sent without a SharedKey signature")
	}
}

func TestAzureBlobSyncerReuploadsAfterAppend(t *testing.T) {
	fake := newAzureBlobFake(t)
	spool := t.TempDir()
	path := writeSpoolFile(t, spool, "codex/session-2.jsonl", "{\"a\":1}\n")
	syncer := azureTestSyncer(t, fake, spool, AzureBlobSyncerConfig{})
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\"a\":1}\n{\"a\":2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.puts) != 2 {
		t.Fatalf("uploads = %d, want the appended file re-uploaded", len(fake.puts))
	}
	if string(fake.blobs["host-a/codex/session-2.jsonl"]) != "{\"a\":1}\n{\"a\":2}\n" {
		t.Fatalf("blob = %q", fake.blobs["host-a/codex/session-2.jsonl"])
	}
}

// Retention deletes local files, so the bytes being deleted must exist remotely
// under a name that a later append cannot overwrite.
func TestAzureBlobSyncerArchivesBeforeDeletingLocally(t *testing.T) {
	fake := newAzureBlobFake(t)
	spool := t.TempDir()
	path := writeSpoolFile(t, spool, "codex/session-3.jsonl", "{\"a\":1}\n")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	syncer := azureTestSyncer(t, fake, spool, AzureBlobSyncerConfig{LocalRetention: time.Hour})

	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("local file survived retention: %v", err)
	}
	archived := false
	for name := range fake.blobs {
		if strings.HasPrefix(name, "host-a/_archive/codex/session-3.jsonl/") {
			archived = true
		}
	}
	if !archived {
		t.Fatalf("no archive copy was written before deletion: %v", fake.blobs)
	}
}

// A failed upload must not delete anything: the whole point of the archive step
// is that local deletion follows a confirmed remote copy.
func TestAzureBlobSyncerKeepsLocalFilesWhenUploadFails(t *testing.T) {
	fake := newAzureBlobFake(t)
	fake.failPut = true
	spool := t.TempDir()
	path := writeSpoolFile(t, spool, "codex/session-4.jsonl", "{\"a\":1}\n")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	syncer := azureTestSyncer(t, fake, spool, AzureBlobSyncerConfig{LocalRetention: time.Hour})

	if err := syncer.SyncOnce(context.Background()); err == nil {
		t.Fatal("a failing upload was reported as success")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("local file was deleted despite the upload failing: %v", err)
	}
}

func TestAzureBlobSyncerRequiresDestinationAndCredential(t *testing.T) {
	spool := t.TempDir()
	if NewAzureBlobSyncer(AzureBlobSyncerConfig{SourceDir: spool}) != nil {
		t.Fatal("a syncer was built with no destination")
	}
	if NewAzureBlobSyncer(AzureBlobSyncerConfig{
		SourceDir:   spool,
		Destination: "https://account.blob.core.windows.net/transcripts",
	}) != nil {
		t.Fatal("a syncer was built with no credential")
	}
	if NewAzureBlobSyncer(AzureBlobSyncerConfig{
		SourceDir:   spool,
		Destination: "https://account.blob.core.windows.net/transcripts",
		AccountKey:  "not base64!!",
	}) != nil {
		t.Fatal("a syncer was built with an unusable account key")
	}
	sas := NewAzureBlobSyncer(AzureBlobSyncerConfig{
		SourceDir:   spool,
		Destination: "https://account.blob.core.windows.net/transcripts/prefix",
		SASToken:    "?sv=2021-08-06&sig=abc",
	})
	if sas == nil || !sas.Enabled() {
		t.Fatal("a SAS credential was rejected")
	}
	if sas.Destination() != "https://account.blob.core.windows.net/transcripts/prefix" {
		t.Fatalf("destination = %q", sas.Destination())
	}
}

func TestParseAzureBlobDestination(t *testing.T) {
	account, container, prefix, endpoint, err := parseAzureBlobDestination("https://acct.blob.core.windows.net/transcripts/host-a/sub")
	if err != nil {
		t.Fatal(err)
	}
	if account != "acct" || container != "transcripts" || prefix != "host-a/sub" || endpoint != "https://acct.blob.core.windows.net" {
		t.Fatalf("parsed = %q %q %q %q", account, container, prefix, endpoint)
	}
	if _, _, _, _, err := parseAzureBlobDestination("https://acct.blob.core.windows.net"); err == nil {
		t.Fatal("a destination with no container was accepted")
	}
	if _, _, _, _, err := parseAzureBlobDestination("gs://bucket/prefix"); err == nil {
		t.Fatal("a non-https destination was accepted")
	}
}

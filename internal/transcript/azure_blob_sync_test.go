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
	mu          sync.Mutex
	blobs       map[string][]byte
	kinds       map[string]string
	snapshots   map[string]int
	puts        []string
	appends     []string
	heads       []string
	appendBytes int
	throttled   int
	failNext    int
	authOK      bool
	server      *httptest.Server
	failPut     bool
}

func newAzureBlobFake(t *testing.T) *azureBlobFake {
	t.Helper()
	fake := &azureBlobFake{blobs: map[string][]byte{}, kinds: map[string]string{}, snapshots: map[string]int{}, authOK: true}
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
		if fake.failNext > 0 || (fake.failPut && r.Method == http.MethodPut) {
			if fake.failNext > 0 {
				fake.failNext--
			}
			fake.throttled++
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		comp := r.URL.Query().Get("comp")
		switch {
		case r.Method == http.MethodHead:
			fake.heads = append(fake.heads, name)
			body, ok := fake.blobs[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.Header().Set("x-ms-blob-type", fake.kinds[name])
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut && comp == "snapshot":
			if _, ok := fake.blobs[name]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fake.snapshots[name]++
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut && comp == "appendblock":
			body, _ := io.ReadAll(r.Body)
			if fake.kinds[name] != "AppendBlob" {
				w.WriteHeader(http.StatusConflict)
				return
			}
			position, err := strconv.Atoi(r.Header.Get("x-ms-blob-condition-appendpos"))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if position != len(fake.blobs[name]) {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			fake.blobs[name] = append(fake.blobs[name], body...)
			fake.appends = append(fake.appends, name)
			fake.appendBytes += len(body)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			kind := r.Header.Get("x-ms-blob-type")
			if kind == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fake.blobs[name] = body
			fake.kinds[name] = kind
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
	if config.Backoff == nil {
		config.Backoff = func(int) time.Duration { return time.Millisecond }
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
	if fake.kinds[blob] != "AppendBlob" {
		t.Fatalf("blob type = %q, want AppendBlob", fake.kinds[blob])
	}
	// A second pass must send nothing for an unchanged file.
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.appends) != 1 {
		t.Fatalf("appends = %d, want 1", len(fake.appends))
	}
	if !fake.authOK {
		t.Fatal("a request was sent without a SharedKey signature")
	}
}

// The whole point: a transcript that reaches gigabytes must upload each byte
// once, not the whole file every interval.
func TestAzureBlobSyncerAppendsOnlyTheNewBytes(t *testing.T) {
	fake := newAzureBlobFake(t)
	spool := t.TempDir()
	path := writeSpoolFile(t, spool, "codex/session-2.jsonl", "{\"a\":1}\n")
	syncer := azureTestSyncer(t, fake, spool, AzureBlobSyncerConfig{})
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := fake.appendBytes

	if err := os.WriteFile(path, []byte("{\"a\":1}\n{\"a\":2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	const blob = "host-a/codex/session-2.jsonl"
	if string(fake.blobs[blob]) != "{\"a\":1}\n{\"a\":2}\n" {
		t.Fatalf("blob = %q, want both lines", fake.blobs[blob])
	}
	if delta := fake.appendBytes - first; delta != len("{\"a\":2}\n") {
		t.Fatalf("second pass uploaded %d bytes, want only the appended line", delta)
	}
}

// Blobs written by the earlier whole-file uploader must convert, or they keep
// being re-sent in full forever.
func TestAzureBlobSyncerMigratesABlockBlob(t *testing.T) {
	fake := newAzureBlobFake(t)
	spool := t.TempDir()
	writeSpoolFile(t, spool, "codex/session-3.jsonl", "{\"a\":1}\n{\"a\":2}\n")
	const blob = "host-a/codex/session-3.jsonl"
	fake.blobs[blob] = []byte("{\"a\":1}\n")
	fake.kinds[blob] = "BlockBlob"

	syncer := azureTestSyncer(t, fake, spool, AzureBlobSyncerConfig{})
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.kinds[blob] != "AppendBlob" {
		t.Fatalf("blob type = %q, want the block blob converted", fake.kinds[blob])
	}
	if string(fake.blobs[blob]) != "{\"a\":1}\n{\"a\":2}\n" {
		t.Fatalf("blob = %q, want the whole file re-sent once", fake.blobs[blob])
	}
}

// A truncated or rotated file is a different stream. Appending to the old blob
// would splice two unrelated transcripts together.
func TestAzureBlobSyncerRestartsAShrunkFile(t *testing.T) {
	fake := newAzureBlobFake(t)
	spool := t.TempDir()
	path := writeSpoolFile(t, spool, "codex/session-4.jsonl", "{\"a\":1}\n{\"a\":2}\n")
	syncer := azureTestSyncer(t, fake, spool, AzureBlobSyncerConfig{})
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\"b\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if string(fake.blobs["host-a/codex/session-4.jsonl"]) != "{\"b\":1}\n" {
		t.Fatalf("blob = %q, want the blob restarted", fake.blobs["host-a/codex/session-4.jsonl"])
	}
}

// Azure answers a busy account with 503. Giving up on the first one leaves the
// spool permanently behind.
func TestAzureBlobSyncerRetriesThrottling(t *testing.T) {
	fake := newAzureBlobFake(t)
	fake.failNext = 2
	spool := t.TempDir()
	writeSpoolFile(t, spool, "codex/session-5.jsonl", "{\"a\":1}\n")
	syncer := azureTestSyncer(t, fake, spool, AzureBlobSyncerConfig{})
	syncer.client = &http.Client{}

	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatalf("a throttled upload was not retried: %v", err)
	}
	if fake.throttled != 2 {
		t.Fatalf("throttles = %d, want both attempts refused before success", fake.throttled)
	}
	if string(fake.blobs["host-a/codex/session-5.jsonl"]) != "{\"a\":1}\n" {
		t.Fatalf("blob = %q", fake.blobs["host-a/codex/session-5.jsonl"])
	}
}

// Retention deletes local files. The live blob keeps growing with the session,
// so the preserved copy is a snapshot of what was deleted, taken server side.
func TestAzureBlobSyncerSnapshotsBeforeDeletingLocally(t *testing.T) {
	fake := newAzureBlobFake(t)
	spool := t.TempDir()
	path := writeSpoolFile(t, spool, "codex/session-6.jsonl", "{\"a\":1}\n")
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
	if fake.snapshots["host-a/codex/session-6.jsonl"] != 1 {
		t.Fatalf("snapshots = %v, want the blob frozen before deletion", fake.snapshots)
	}
	// No archive copy is uploaded: the bytes are already in the blob.
	for name := range fake.blobs {
		if strings.Contains(name, "_archive/") {
			t.Fatalf("an archive copy was uploaded anyway: %s", name)
		}
	}
}

// A failed upload must not delete anything.
func TestAzureBlobSyncerKeepsLocalFilesWhenUploadFails(t *testing.T) {
	fake := newAzureBlobFake(t)
	fake.failPut = true
	spool := t.TempDir()
	path := writeSpoolFile(t, spool, "codex/session-7.jsonl", "{\"a\":1}\n")
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

// One unhappy file must not stop the files behind it in the walk.
func TestAzureBlobSyncerContinuesPastAFailedFile(t *testing.T) {
	fake := newAzureBlobFake(t)
	spool := t.TempDir()
	writeSpoolFile(t, spool, "codex/aaa.jsonl", "{\"a\":1}\n")
	writeSpoolFile(t, spool, "codex/bbb.jsonl", "{\"b\":1}\n")
	syncer := azureTestSyncer(t, fake, spool, AzureBlobSyncerConfig{})
	fake.failNext = azureBlobRetryAttempts + 1

	if err := syncer.SyncOnce(context.Background()); err == nil {
		t.Fatal("the pass reported success despite a failed file")
	}
	if len(fake.blobs) == 0 {
		t.Fatal("no file uploaded; the first failure stopped the whole pass")
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

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
	deletes     []string
	copies      []string
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
		case r.Method == http.MethodDelete:
			if _, ok := fake.blobs[name]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			// Azure refuses to delete a blob that carries snapshots unless the
			// request says what to do with them.
			if fake.snapshots[name] > 0 && r.Header.Get("x-ms-delete-snapshots") == "" {
				w.WriteHeader(http.StatusConflict)
				return
			}
			delete(fake.blobs, name)
			delete(fake.kinds, name)
			delete(fake.snapshots, name)
			fake.deletes = append(fake.deletes, name)
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && r.Header.Get("x-ms-copy-source") != "":
			source := r.Header.Get("x-ms-copy-source")
			sourceName := source[strings.Index(source, "/transcripts/")+len("/transcripts/"):]
			body, ok := fake.blobs[sourceName]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fake.blobs[name] = append([]byte(nil), body...)
			fake.kinds[name] = fake.kinds[sourceName]
			fake.copies = append(fake.copies, name)
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			kind := r.Header.Get("x-ms-blob-type")
			if kind == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// Azure refuses to change a blob's type in place.
			if existing, ok := fake.kinds[name]; ok && existing != kind {
				w.WriteHeader(http.StatusConflict)
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
	// Azure refuses to change a blob's type in place, so the conversion must
	// delete first. Without it every pass fails with InvalidBlobType.
	if len(fake.deletes) != 1 || fake.deletes[0] != blob {
		t.Fatalf("deletes = %v, want the block blob removed before the append blob was created", fake.deletes)
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

// Retention snapshots a blob before deleting the local file, and the earlier
// uploader left block blobs behind. A blob that is both cannot simply be
// deleted: Azure answers "SnapshotsPresent", and every later pass failed.
func TestAzureBlobSyncerMigratesASnapshottedBlockBlob(t *testing.T) {
	fake := newAzureBlobFake(t)
	spool := t.TempDir()
	writeSpoolFile(t, spool, "codex/session-8.jsonl", "{\"a\":1}\n{\"a\":2}\n")
	const blob = "host-a/codex/session-8.jsonl"
	fake.blobs[blob] = []byte("{\"a\":1}\n")
	fake.kinds[blob] = "BlockBlob"
	fake.snapshots[blob] = 1

	syncer := azureTestSyncer(t, fake, spool, AzureBlobSyncerConfig{})
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatalf("a snapshotted block blob was not migrated: %v", err)
	}
	if fake.kinds[blob] != "AppendBlob" {
		t.Fatalf("blob type = %q, want the blob converted", fake.kinds[blob])
	}
	if string(fake.blobs[blob]) != "{\"a\":1}\n{\"a\":2}\n" {
		t.Fatalf("blob = %q", fake.blobs[blob])
	}
	// The bytes that were there before the delete must survive under an
	// archive name, because the blob could hold what the spool no longer does.
	preserved := false
	for name, body := range fake.blobs {
		if strings.Contains(name, "_archive/") && string(body) == "{\"a\":1}\n" {
			preserved = true
		}
	}
	if !preserved {
		t.Fatalf("the old contents were deleted without being preserved: %v", fake.copies)
	}
}

// Retention must never retire a file the blob has not caught up with. The
// append pass will finish it; deleting now would lose the tail.
func TestAzureBlobSyncerKeepsAFileWhoseBlobIsBehind(t *testing.T) {
	fake := newAzureBlobFake(t)
	spool := t.TempDir()
	path := writeSpoolFile(t, spool, "codex/session-9.jsonl", "{\"a\":1}\n{\"a\":2}\n")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	const blob = "host-a/codex/session-9.jsonl"
	fake.blobs[blob] = []byte("{\"a\":1}\n")
	fake.kinds[blob] = "AppendBlob"
	// Fail the appends so the blob stays behind for this pass.
	fake.failPut = true

	syncer := azureTestSyncer(t, fake, spool, AzureBlobSyncerConfig{LocalRetention: time.Hour})
	if err := syncer.SyncOnce(context.Background()); err == nil {
		t.Fatal("a failing pass reported success")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a file whose blob is behind was deleted: %v", err)
	}
	if fake.snapshots[blob] != 0 {
		t.Fatal("an incomplete blob was snapshotted as if it were retired")
	}
}

// The first backlog upload saturated the host's uplink, and the proxy shares
// that link. Transcript bytes must yield.
func TestUploadPacerSpreadsBytesOverTime(t *testing.T) {
	now := time.Unix(0, 0)
	var waits []time.Duration
	pacer := newUploadPacer(1024,
		func(_ context.Context, wait time.Duration) error {
			waits = append(waits, wait)
			now = now.Add(wait)
			return nil
		},
		func() time.Time { return now })

	// The first reservation is free; the next one waits for the first to drain.
	if err := pacer.reserve(context.Background(), 1024); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 0 {
		t.Fatalf("waits = %v, want the first block to go straight out", waits)
	}
	if err := pacer.reserve(context.Background(), 512); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0] != time.Second {
		t.Fatalf("waits = %v, want one second for 1024 bytes at 1024 B/s", waits)
	}

	// A negative cap disables pacing entirely, and a nil pacer never waits.
	if newUploadPacer(-1, nil, nil) != nil {
		t.Fatal("a negative cap did not disable pacing")
	}
	var absent *uploadPacer
	if err := absent.reserve(context.Background(), 1<<20); err != nil {
		t.Fatal(err)
	}
}

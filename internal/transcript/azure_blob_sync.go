package transcript

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAzureBlobSyncTimeout = 30 * time.Minute
	// azureBlobQuietPeriod keeps a transcript that is still being appended out
	// of the upload pass, so the archived copy is a settled file rather than a
	// half-written turn.
	azureBlobQuietPeriod = 2 * time.Minute
	azureBlobAPIVersion  = "2021-08-06"
)

// AzureBlobSyncer copies the local transcript spool into an Azure blob
// container and then prunes what it has archived. It is the Azure counterpart
// of GCSSyncer: same spool, same retention policy, different destination,
// because the hosts that record transcripts are not all on GCP and a syncer
// that can only authenticate through the GCE metadata server silently records
// nothing anywhere else.
type AzureBlobSyncer struct {
	sourceDir  string
	account    string
	container  string
	prefix     string
	endpoint   string
	accountKey []byte
	sasQuery   string
	interval   time.Duration
	timeout    time.Duration
	retention  time.Duration
	maxBytes   int64
	client     *http.Client
	logger     *slog.Logger
	now        func() time.Time
	// backoff is the wait before resending a throttled request. Injectable so
	// tests exercise the retry path without sleeping through it.
	backoff func(attempt int) time.Duration
}

type AzureBlobSyncerConfig struct {
	SourceDir string
	// Destination is the container URL, optionally with a prefix path, e.g.
	// https://account.blob.core.windows.net/transcripts/cmux-lawrence.
	Destination string
	// AccountKey is the storage account shared key. Preferred over a SAS
	// because it does not expire, and an expired credential would stop the
	// pipeline exactly as silently as the failure this replaces.
	AccountKey string
	// SASToken is used when no account key is configured.
	SASToken       string
	Interval       time.Duration
	Timeout        time.Duration
	LocalRetention time.Duration
	MaxLocalBytes  int64
	Logger         *slog.Logger
	Client         *http.Client
	Now            func() time.Time
	// Backoff overrides the wait before resending a throttled request.
	Backoff func(attempt int) time.Duration
}

// NewAzureBlobSyncer returns nil when the destination or source is not
// configured, which is how callers keep transcript sync opt-in.
func NewAzureBlobSyncer(config AzureBlobSyncerConfig) *AzureBlobSyncer {
	sourceDir := strings.TrimSpace(config.SourceDir)
	account, container, prefix, endpoint, err := parseAzureBlobDestination(config.Destination)
	if sourceDir == "" || err != nil {
		return nil
	}
	key := strings.TrimSpace(config.AccountKey)
	sas := strings.TrimSpace(config.SASToken)
	if key == "" && sas == "" {
		return nil
	}
	var decodedKey []byte
	if key != "" {
		decodedKey, err = base64.StdEncoding.DecodeString(key)
		if err != nil {
			return nil
		}
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = defaultAzureBlobSyncTimeout
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	client := config.Client
	if client == nil {
		client = &http.Client{}
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &AzureBlobSyncer{
		sourceDir:  sourceDir,
		account:    account,
		container:  container,
		prefix:     prefix,
		endpoint:   endpoint,
		accountKey: decodedKey,
		sasQuery:   strings.TrimPrefix(sas, "?"),
		interval:   config.Interval,
		timeout:    timeout,
		retention:  config.LocalRetention,
		maxBytes:   config.MaxLocalBytes,
		client:     client,
		logger:     logger,
		now:        now,
		backoff:    config.Backoff,
	}
}

// parseAzureBlobDestination accepts the container URL form Azure shows in the
// portal, with an optional prefix path.
func parseAzureBlobDestination(destination string) (account, container, prefix, endpoint string, err error) {
	trimmed := strings.TrimSpace(destination)
	if trimmed == "" {
		return "", "", "", "", fmt.Errorf("azure blob destination is empty")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", "", "", "", err
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", "", "", "", fmt.Errorf("azure blob destination %q must be an https url", destination)
	}
	host := parsed.Host
	if host == "" {
		return "", "", "", "", fmt.Errorf("azure blob destination %q has no host", destination)
	}
	account = host
	if index := strings.Index(host, "."); index > 0 {
		account = host[:index]
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) == 0 || segments[0] == "" {
		return "", "", "", "", fmt.Errorf("azure blob destination %q has no container", destination)
	}
	container = segments[0]
	prefix = strings.Join(segments[1:], "/")
	endpoint = parsed.Scheme + "://" + host
	return account, container, prefix, endpoint, nil
}

func (s *AzureBlobSyncer) Enabled() bool {
	return s != nil && s.sourceDir != "" && s.container != ""
}

func (s *AzureBlobSyncer) Destination() string {
	if !s.Enabled() {
		return ""
	}
	destination := s.endpoint + "/" + s.container
	if s.prefix != "" {
		destination += "/" + s.prefix
	}
	return destination
}

func (s *AzureBlobSyncer) Run(ctx context.Context) {
	if !s.Enabled() || s.interval <= 0 {
		return
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := s.SyncOnce(ctx); err != nil {
				s.logger.Warn("transcript azure sync failed", "destination", s.Destination(), "error", err)
			}
			timer.Reset(s.interval)
		}
	}
}

func (s *AzureBlobSyncer) SyncOnce(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}
	if _, err := os.Stat(s.sourceDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	syncCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	files, _, err := collectTranscriptFiles(s.sourceDir)
	if err != nil {
		return err
	}
	now := s.now()
	sort.Slice(files, func(i, j int) bool {
		if files[i].modTime.Equal(files[j].modTime) {
			return files[i].path < files[j].path
		}
		return files[i].modTime.Before(files[j].modTime)
	})
	// One unhappy file must not hold the rest of the spool hostage. A transcript
	// that fails now is retried next interval; the files behind it in the walk
	// would otherwise never upload at all.
	var firstErr error
	for _, file := range files {
		if now.Sub(file.modTime) < azureBlobQuietPeriod {
			continue
		}
		if err := s.syncFile(syncCtx, file); err != nil {
			if syncCtx.Err() != nil {
				return err
			}
			if firstErr == nil {
				firstErr = err
			}
			s.logger.Warn("transcript azure upload failed", "file", file.relPath, "error", err)
		}
	}
	if err := s.pruneLocal(syncCtx, s.now()); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// syncFile brings one transcript blob up to date by appending only the bytes
// written since the last pass.
//
// Transcripts are append-only and enormous: a busy session reaches gigabytes
// within hours. Re-uploading the whole file every interval, which is what a
// block blob forces, means uploading the same gigabytes hundreds of times a
// day. Azure answered that with "503 The server is busy", and it was right to.
// An append blob uploads each byte once.
func (s *AzureBlobSyncer) syncFile(ctx context.Context, file localFile) error {
	name := s.blobName(file.relPath)
	remote, err := s.blobState(ctx, name)
	if err != nil {
		return err
	}
	switch {
	case !remote.exists:
		if err := s.createAppendBlob(ctx, name); err != nil {
			return err
		}
		return s.appendRange(ctx, file.path, name, 0, file.size)
	case !remote.appendBlob:
		// A blob left by the earlier whole-file uploader. Replace it with an
		// append blob so this file stops being re-sent in full.
		if err := s.createAppendBlob(ctx, name); err != nil {
			return err
		}
		return s.appendRange(ctx, file.path, name, 0, file.size)
	case remote.size == file.size:
		return nil
	case remote.size > file.size:
		// The local file shrank, so it is not the same stream any more: a
		// rotated or replaced transcript. Start the blob over rather than
		// appending bytes that would not follow what is already there.
		if err := s.createAppendBlob(ctx, name); err != nil {
			return err
		}
		return s.appendRange(ctx, file.path, name, 0, file.size)
	default:
		return s.appendRange(ctx, file.path, name, remote.size, file.size-remote.size)
	}
}

// azureBlobState is what one HEAD tells us about a blob.
type azureBlobState struct {
	exists     bool
	appendBlob bool
	size       int64
}

func (s *AzureBlobSyncer) blobState(ctx context.Context, name string) (azureBlobState, error) {
	req, err := s.newRequest(ctx, http.MethodHead, name, "", nil, nil, 0)
	if err != nil {
		return azureBlobState{}, err
	}
	resp, err := s.do(ctx, req, func() (io.Reader, error) { return nil, nil }, 0)
	if err != nil {
		return azureBlobState{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return azureBlobState{}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return azureBlobState{}, azureBlobResponseError(resp)
	}
	state := azureBlobState{
		exists:     true,
		appendBlob: strings.EqualFold(resp.Header.Get("x-ms-blob-type"), "AppendBlob"),
	}
	if size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err == nil {
		state.size = size
	}
	return state, nil
}

// createAppendBlob creates (or resets) the blob. An existing blob of any type
// is replaced, which is what makes both the block-blob migration and the
// rotated-file case work.
func (s *AzureBlobSyncer) createAppendBlob(ctx context.Context, name string) error {
	req, err := s.newRequest(ctx, http.MethodPut, name, "", map[string]string{
		"x-ms-blob-type": "AppendBlob",
		"Content-Type":   "application/x-ndjson",
	}, nil, 0)
	if err != nil {
		return err
	}
	resp, err := s.do(ctx, req, func() (io.Reader, error) { return nil, nil }, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return azureBlobResponseError(resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

// azureAppendBlockMaxBytes is the service limit for one append.
const azureAppendBlockMaxBytes = 4 << 20

// appendRange appends [offset, offset+length) of a local file. Each block
// carries the position it expects to land at, so a retry that the service
// already applied is refused (412) instead of duplicating the bytes.
func (s *AzureBlobSyncer) appendRange(ctx context.Context, path, name string, offset, length int64) error {
	if length <= 0 {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	for remaining := length; remaining > 0; {
		block := remaining
		if block > azureAppendBlockMaxBytes {
			block = azureAppendBlockMaxBytes
		}
		position := offset
		body := func() (io.Reader, error) {
			if _, err := file.Seek(position, io.SeekStart); err != nil {
				return nil, err
			}
			return io.LimitReader(file, block), nil
		}
		reader, err := body()
		if err != nil {
			return err
		}
		req, err := s.newRequest(ctx, http.MethodPut, name, "comp=appendblock", map[string]string{
			"x-ms-blob-condition-appendpos": strconv.FormatInt(position, 10),
		}, reader, block)
		if err != nil {
			return err
		}
		resp, err := s.do(ctx, req, body, block)
		if err != nil {
			return err
		}
		status := resp.StatusCode
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		switch {
		case status >= 200 && status < 300:
		case status == http.StatusPreconditionFailed:
			// The blob is already past this position, so a previous attempt
			// landed. Anything else at this offset would duplicate lines.
			return nil
		default:
			return azureBlobResponseError(resp)
		}
		offset += block
		remaining -= block
	}
	return nil
}

// snapshotBlob freezes the current contents server side. It is how a local file
// is preserved before deletion: the live blob keeps growing with the session,
// and the snapshot holds exactly the bytes that were deleted, with no upload.
func (s *AzureBlobSyncer) snapshotBlob(ctx context.Context, name string) (bool, error) {
	req, err := s.newRequest(ctx, http.MethodPut, name, "comp=snapshot", nil, nil, 0)
	if err != nil {
		return false, err
	}
	resp, err := s.do(ctx, req, func() (io.Reader, error) { return nil, nil }, 0)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, azureBlobResponseError(resp)
	}
	return true, nil
}

// uploadWholeFile writes a file as a block blob in one request. Only the
// fallback archive path uses it, for the case where nothing was ever appended.
func (s *AzureBlobSyncer) uploadWholeFile(ctx context.Context, path, name string, size int64) error {
	open := func() (io.Reader, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return io.LimitReader(file, size), nil
	}
	reader, err := open()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	req, err := s.newRequest(ctx, http.MethodPut, name, "", map[string]string{
		"x-ms-blob-type": "BlockBlob",
	}, reader, size)
	if err != nil {
		return err
	}
	resp, err := s.do(ctx, req, open, size)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return azureBlobResponseError(resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

// pruneLocal deletes what retention and the size cap say should go, but only
// after that exact byte range is archived under a content-addressed name. A
// transcript is append-only, so the live blob can already have moved on; the
// archive copy is what makes deleting the local file safe.
func (s *AzureBlobSyncer) pruneLocal(ctx context.Context, now time.Time) error {
	if s.retention <= 0 && s.maxBytes <= 0 {
		return nil
	}
	files, totalBytes, err := collectTranscriptFiles(s.sourceDir)
	if err != nil {
		return err
	}
	selected := map[string]localFile{}
	for _, file := range files {
		if s.retention > 0 && !file.modTime.After(now.Add(-s.retention)) {
			selected[file.path] = file
		}
	}
	if s.maxBytes > 0 && totalBytes > s.maxBytes {
		sort.Slice(files, func(i, j int) bool {
			if files[i].modTime.Equal(files[j].modTime) {
				return files[i].path < files[j].path
			}
			return files[i].modTime.Before(files[j].modTime)
		})
		remaining := totalBytes
		for _, file := range files {
			if remaining <= s.maxBytes {
				break
			}
			selected[file.path] = file
			remaining -= file.size
		}
	}
	if len(selected) == 0 {
		return nil
	}
	ordered := make([]localFile, 0, len(selected))
	for _, file := range selected {
		ordered = append(ordered, file)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].modTime.Equal(ordered[j].modTime) {
			return ordered[i].path < ordered[j].path
		}
		return ordered[i].modTime.Before(ordered[j].modTime)
	})
	for _, file := range ordered {
		if err := s.archiveAndRemove(ctx, file); err != nil {
			return err
		}
	}
	_ = pruneEmptyDirs(s.sourceDir)
	return nil
}

// archiveAndRemove preserves a transcript before deleting it locally.
//
// The live blob keeps growing with the session, so it cannot be the archive.
// A snapshot freezes the current contents server side, with no upload at all,
// which matters when the file being retired is gigabytes. Only a file that was
// never appended (nothing uploaded yet) is copied the slow way.
func (s *AzureBlobSyncer) archiveAndRemove(ctx context.Context, file localFile) error {
	before, err := os.Stat(file.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// A file that changed since the walk is still live; leave it for the next
	// pass rather than archiving a size that no longer describes it.
	if before.Size() != file.size || !before.ModTime().Equal(file.modTime) {
		return nil
	}
	name := s.blobName(file.relPath)
	remote, err := s.blobState(ctx, name)
	if err != nil {
		return err
	}
	switch {
	case remote.exists && remote.size >= file.size:
		if snapshotted, err := s.snapshotBlob(ctx, name); err != nil {
			return err
		} else if !snapshotted {
			return nil
		}
	default:
		// Nothing uploaded, or the blob is behind the local file: the bytes
		// about to be deleted are not all in Azure yet, so send them first.
		archive := s.blobName("_archive/" + file.relPath + "/" + archiveFileName(file))
		if err := s.uploadWholeFile(ctx, file.path, archive, file.size); err != nil {
			return err
		}
	}
	after, err := os.Stat(file.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if after.Size() != file.size || !after.ModTime().Equal(file.modTime) {
		return nil
	}
	if err := os.Remove(file.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *AzureBlobSyncer) blobName(relPath string) string {
	relPath = strings.TrimLeft(strings.ReplaceAll(relPath, "\\", "/"), "/")
	if s.prefix == "" {
		return relPath
	}
	return s.prefix + "/" + relPath
}

// newRequest builds a signed blob request. Shared Key signing is done here
// rather than through the Azure SDK to keep the daemon's dependency set to the
// standard library, which is the same reason the GCS syncer speaks the REST API
// directly.
// newRequest builds a signed blob request.
//
// Every header the request will carry must be passed in here. Shared Key signs
// the x-ms-* headers and Content-Type, so a header added to the returned
// request afterwards is not in the signature, and Azure answers 403 with no
// hint about which header disagreed.
func (s *AzureBlobSyncer) newRequest(ctx context.Context, method, blobName, query string, headers map[string]string, body io.Reader, size int64) (*http.Request, error) {
	target := s.endpoint + "/" + s.container + "/" + azureBlobEscapePath(blobName)
	params := []string{}
	if query != "" {
		params = append(params, query)
	}
	if s.sasQuery != "" && len(s.accountKey) == 0 {
		params = append(params, s.sasQuery)
	}
	if len(params) > 0 {
		target += "?" + strings.Join(params, "&")
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	if method == http.MethodPut {
		req.ContentLength = size
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/x-ndjson")
		}
	}
	if len(s.accountKey) == 0 {
		return req, nil
	}
	req.Header.Set("x-ms-date", s.now().UTC().Format(http.TimeFormat))
	req.Header.Set("x-ms-version", azureBlobAPIVersion)
	signature, err := s.signature(req, size)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "SharedKey "+s.account+":"+signature)
	return req, nil
}

// azureBlobRetryAttempts bounds how many times one request is resent after a
// throttle or a server fault. Azure answers a busy account with 503, and a
// transcript spool that gives up on the first one falls permanently behind.
const azureBlobRetryAttempts = 4

// do sends a request, resending it on a throttle or server fault. body must
// return a fresh reader for each attempt, because the previous attempt consumed
// the last one. The request is rebuilt per attempt: Shared Key signs the date
// header, so a resend with the old signature is refused.
func (s *AzureBlobSyncer) do(ctx context.Context, req *http.Request, body func() (io.Reader, error), size int64) (*http.Response, error) {
	method := req.Method
	blobPath := req.URL.EscapedPath()
	query := req.URL.RawQuery
	headers := req.Header.Clone()
	var lastErr error
	for attempt := 0; attempt < azureBlobRetryAttempts; attempt++ {
		if attempt > 0 {
			reader, err := body()
			if err != nil {
				return nil, err
			}
			rebuilt, err := s.resign(ctx, method, blobPath, query, headers, reader, size)
			if err != nil {
				return nil, err
			}
			req = rebuilt
		}
		resp, err := s.client.Do(req)
		if err != nil {
			lastErr = err
		} else if !azureBlobRetryableStatus(resp.StatusCode) {
			return resp, nil
		} else {
			lastErr = azureBlobResponseError(resp)
			_ = resp.Body.Close()
		}
		wait := s.retryBackoff(attempt)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, lastErr
}

// resign rebuilds a request for a retry, including a fresh date and signature.
func (s *AzureBlobSyncer) resign(ctx context.Context, method, blobPath, query string, headers http.Header, body io.Reader, size int64) (*http.Request, error) {
	target := s.endpoint + blobPath
	params := []string{}
	if query != "" {
		params = append(params, query)
	}
	if len(params) > 0 {
		target += "?" + strings.Join(params, "&")
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	for key, values := range headers {
		lower := strings.ToLower(key)
		if lower == "authorization" || lower == "x-ms-date" {
			continue
		}
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if method == http.MethodPut {
		req.ContentLength = size
	}
	if len(s.accountKey) == 0 {
		return req, nil
	}
	req.Header.Set("x-ms-date", s.now().UTC().Format(http.TimeFormat))
	req.Header.Set("x-ms-version", azureBlobAPIVersion)
	signature, err := s.signature(req, size)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "SharedKey "+s.account+":"+signature)
	return req, nil
}

// azureBlobRetryableStatus reports whether the service asked us to come back
// later rather than refusing the request itself.
func azureBlobRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500
}

func (s *AzureBlobSyncer) retryBackoff(attempt int) time.Duration {
	if s.backoff != nil {
		return s.backoff(attempt)
	}
	return azureBlobRetryBackoff(attempt)
}

func azureBlobRetryBackoff(attempt int) time.Duration {
	wait := time.Second << attempt
	if wait > 8*time.Second {
		wait = 8 * time.Second
	}
	return wait
}

// signature builds the Shared Key string-to-sign exactly as documented: the
// fixed header block, then the canonicalized x-ms-* headers, then the
// canonicalized resource.
func (s *AzureBlobSyncer) signature(req *http.Request, size int64) (string, error) {
	// Azure signs an empty string for a zero-length body, even though Go still
	// sends the "Content-Length: 0" header.
	contentLength := ""
	if size > 0 {
		contentLength = strconv.FormatInt(size, 10)
	}
	canonicalHeaders := azureCanonicalizedHeaders(req.Header)
	canonicalResource := "/" + s.account + req.URL.EscapedPath() + azureCanonicalizedQuery(req.URL)
	stringToSign := strings.Join([]string{
		req.Method,
		"",            // Content-Encoding
		"",            // Content-Language
		contentLength, // Content-Length ("" when zero)
		"",            // Content-MD5
		req.Header.Get("Content-Type"),
		"", // Date (x-ms-date is used instead)
		"", // If-Modified-Since
		"", // If-Match
		"", // If-None-Match
		"", // If-Unmodified-Since
		"", // Range
		canonicalHeaders + canonicalResource,
	}, "\n")
	mac := hmac.New(sha256.New, s.accountKey)
	if _, err := mac.Write([]byte(stringToSign)); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

func azureCanonicalizedHeaders(header http.Header) string {
	names := make([]string, 0, len(header))
	for name := range header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-ms-") {
			names = append(names, lower)
		}
	}
	sort.Strings(names)
	var builder strings.Builder
	for _, name := range names {
		builder.WriteString(name)
		builder.WriteString(":")
		builder.WriteString(strings.TrimSpace(header.Get(name)))
		builder.WriteString("\n")
	}
	return builder.String()
}

// azureBlobEscapePath escapes each path segment while keeping the separators,
// because a transcript name carries the session's own directory layout.
func azureBlobEscapePath(name string) string {
	segments := strings.Split(name, "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

func azureBlobResponseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("azure blob %s: %s", resp.Status, strings.TrimSpace(string(body)))
}

// azureCanonicalizedQuery renders the query string the way Shared Key signing
// expects: one sorted, lowercased parameter per line. Without it every request
// that carries comp=appendblock or comp=snapshot is refused as unauthorized.
func azureCanonicalizedQuery(target *url.URL) string {
	values := target.Query()
	if len(values) == 0 {
		return ""
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, strings.ToLower(name))
	}
	sort.Strings(names)
	var builder strings.Builder
	for _, name := range names {
		entries := append([]string(nil), values[name]...)
		sort.Strings(entries)
		builder.WriteString("\n")
		builder.WriteString(name)
		builder.WriteString(":")
		builder.WriteString(strings.Join(entries, ","))
	}
	return builder.String()
}

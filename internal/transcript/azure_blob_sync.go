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
	for _, file := range files {
		if now.Sub(file.modTime) < azureBlobQuietPeriod {
			continue
		}
		if err := s.uploadIfNeeded(syncCtx, file); err != nil {
			return err
		}
	}
	return s.pruneLocal(syncCtx, s.now())
}

// uploadIfNeeded skips a blob that already holds the same number of bytes.
// Transcripts only ever grow, so equal length means equal content.
func (s *AzureBlobSyncer) uploadIfNeeded(ctx context.Context, file localFile) error {
	name := s.blobName(file.relPath)
	size, exists, err := s.blobSize(ctx, name)
	if err != nil {
		return err
	}
	if exists && size == file.size {
		return nil
	}
	return s.uploadFile(ctx, file.path, name, file.size)
}

func (s *AzureBlobSyncer) blobSize(ctx context.Context, name string) (int64, bool, error) {
	req, err := s.newRequest(ctx, http.MethodHead, name, nil, 0)
	if err != nil {
		return 0, false, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return 0, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, false, azureBlobResponseError(resp)
	}
	size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		return 0, true, nil
	}
	return size, true, nil
}

func (s *AzureBlobSyncer) uploadFile(ctx context.Context, path, name string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	req, err := s.newRequest(ctx, http.MethodPut, name, io.LimitReader(file, size), size)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return azureBlobResponseError(resp)
	}
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
	archive := s.blobName("_archive/" + file.relPath + "/" + archiveFileName(file))
	size, exists, err := s.blobSize(ctx, archive)
	if err != nil {
		return err
	}
	if !exists || size != file.size {
		if err := s.uploadFile(ctx, file.path, archive, file.size); err != nil {
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
func (s *AzureBlobSyncer) newRequest(ctx context.Context, method, blobName string, body io.Reader, size int64) (*http.Request, error) {
	target := s.endpoint + "/" + s.container + "/" + azureBlobEscapePath(blobName)
	if s.sasQuery != "" && len(s.accountKey) == 0 {
		target += "?" + s.sasQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if method == http.MethodPut {
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/x-ndjson")
		req.Header.Set("x-ms-blob-type", "BlockBlob")
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

// signature builds the Shared Key string-to-sign exactly as documented: the
// fixed header block, then the canonicalized x-ms-* headers, then the
// canonicalized resource.
func (s *AzureBlobSyncer) signature(req *http.Request, size int64) (string, error) {
	contentLength := ""
	if size > 0 {
		contentLength = strconv.FormatInt(size, 10)
	}
	canonicalHeaders := azureCanonicalizedHeaders(req.Header)
	canonicalResource := "/" + s.account + req.URL.EscapedPath()
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

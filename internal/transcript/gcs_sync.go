package transcript

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultGCSSyncTimeout  = 30 * time.Minute
	defaultGCSQuietPeriod  = 2 * time.Minute
	gceMetadataTokenURL    = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
	gcsUploadBaseURL       = "https://storage.googleapis.com/upload/storage/v1"
	gcsStorageBaseURL      = "https://storage.googleapis.com/storage/v1"
	gcsTokenRefreshPadding = time.Minute
)

var errGCSObjectNotFound = errors.New("gcs object not found")

type GCSSyncer struct {
	sourceDir   string
	destination string
	bucket      string
	prefix      string
	interval    time.Duration
	command     string
	timeout     time.Duration
	retention   time.Duration
	maxBytes    int64
	client      *http.Client
	token       string
	tokenExpiry time.Time
	logger      *slog.Logger
}

type GCSSyncerConfig struct {
	SourceDir      string
	Destination    string
	Interval       time.Duration
	Command        string
	Timeout        time.Duration
	LocalRetention time.Duration
	MaxLocalBytes  int64
	Logger         *slog.Logger
}

func NewGCSSyncer(config GCSSyncerConfig) *GCSSyncer {
	destination, bucket, prefix := normalizeGCSDestination(config.Destination)
	sourceDir := strings.TrimSpace(config.SourceDir)
	if sourceDir == "" || destination == "" {
		return nil
	}
	command := strings.TrimSpace(config.Command)
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = defaultGCSSyncTimeout
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &GCSSyncer{
		sourceDir:   sourceDir,
		destination: destination,
		bucket:      bucket,
		prefix:      prefix,
		interval:    config.Interval,
		command:     command,
		timeout:     timeout,
		retention:   config.LocalRetention,
		maxBytes:    config.MaxLocalBytes,
		client:      &http.Client{},
		logger:      logger,
	}
}

func (s *GCSSyncer) Enabled() bool {
	return s != nil && s.sourceDir != "" && s.destination != ""
}

func (s *GCSSyncer) Run(ctx context.Context) {
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
				s.logger.Warn("transcript gcs sync failed", "destination", s.destination, "error", err)
			}
			timer.Reset(s.interval)
		}
	}
}

func (s *GCSSyncer) SyncOnce(ctx context.Context) error {
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

	if err := s.pruneLocal(ctx, time.Now()); err != nil {
		s.logger.Warn("transcript local prune skipped", "destination", s.destination, "error", err)
	}
	if s.command != "" {
		if err := s.runCommand(syncCtx, "-m", "rsync", "-r", s.sourceDir, s.destination); err != nil {
			return err
		}
	} else {
		if err := s.syncNative(syncCtx); err != nil {
			return err
		}
	}
	if err := s.pruneLocal(ctx, time.Now()); err != nil {
		return err
	}
	return nil
}

func (s *GCSSyncer) runCommand(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, s.command, args...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (s *GCSSyncer) syncNative(ctx context.Context) error {
	files, _, err := s.localTranscriptFiles()
	if err != nil {
		return err
	}
	now := time.Now()
	sort.Slice(files, func(i, j int) bool {
		if files[i].modTime.Equal(files[j].modTime) {
			if files[i].size == files[j].size {
				return files[i].path < files[j].path
			}
			return files[i].size < files[j].size
		}
		return files[i].modTime.Before(files[j].modTime)
	})
	for _, file := range files {
		if now.Sub(file.modTime) < defaultGCSQuietPeriod {
			continue
		}
		if err := s.uploadIfNeeded(ctx, file); err != nil {
			return err
		}
	}
	return nil
}

func (s *GCSSyncer) uploadIfNeeded(ctx context.Context, file localFile) error {
	objectName := s.objectName(file.relPath)
	remoteSize, exists, err := s.objectSize(ctx, objectName)
	if err != nil {
		return err
	}
	if exists && remoteSize == file.size {
		return nil
	}
	return s.uploadFile(ctx, file.path, objectName, file.size)
}

func (s *GCSSyncer) objectSize(ctx context.Context, objectName string) (int64, bool, error) {
	requestURL := gcsStorageBaseURL + "/b/" + url.PathEscape(s.bucket) + "/o/" + url.PathEscape(objectName) + "?fields=size"
	req, err := s.newGCSRequest(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return 0, false, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, false, gcsResponseError(resp)
	}
	var body struct {
		Size string `json:"size"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, false, err
	}
	size, err := strconv.ParseInt(body.Size, 10, 64)
	if err != nil {
		return 0, false, err
	}
	return size, true, nil
}

func (s *GCSSyncer) uploadFile(ctx context.Context, path, objectName string, size int64) error {
	initURL := gcsUploadBaseURL + "/b/" + url.PathEscape(s.bucket) + "/o?uploadType=resumable&name=" + url.QueryEscape(objectName)
	initReq, err := s.newGCSRequest(ctx, http.MethodPost, initURL, nil)
	if err != nil {
		return err
	}
	initReq.Header.Set("X-Upload-Content-Type", "application/octet-stream")
	initReq.Header.Set("X-Upload-Content-Length", strconv.FormatInt(size, 10))
	initResp, err := s.client.Do(initReq)
	if err != nil {
		return err
	}
	if initResp.StatusCode < 200 || initResp.StatusCode >= 300 {
		defer initResp.Body.Close()
		return gcsResponseError(initResp)
	}
	_, _ = io.Copy(io.Discard, initResp.Body)
	_ = initResp.Body.Close()
	location := initResp.Header.Get("Location")
	if location == "" {
		return fmt.Errorf("gcs resumable upload did not return a Location header")
	}

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	uploadReq, err := s.newGCSRequest(ctx, http.MethodPut, location, io.LimitReader(file, size))
	if err != nil {
		return err
	}
	uploadReq.ContentLength = size
	uploadReq.Header.Set("Content-Type", "application/octet-stream")
	uploadResp, err := s.client.Do(uploadReq)
	if err != nil {
		return err
	}
	defer uploadResp.Body.Close()
	if uploadResp.StatusCode < 200 || uploadResp.StatusCode >= 300 {
		return gcsResponseError(uploadResp)
	}
	_, _ = io.Copy(io.Discard, uploadResp.Body)
	return nil
}

func (s *GCSSyncer) copyObject(ctx context.Context, sourceObject, destinationObject string) error {
	if _, exists, err := s.objectSize(ctx, destinationObject); err != nil || exists {
		return err
	}
	copyURL := gcsStorageBaseURL + "/b/" + url.PathEscape(s.bucket) + "/o/" + url.PathEscape(sourceObject) + "/copyTo/b/" + url.PathEscape(s.bucket) + "/o/" + url.PathEscape(destinationObject)
	req, err := s.newGCSRequest(ctx, http.MethodPost, copyURL, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return errGCSObjectNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return gcsResponseError(resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (s *GCSSyncer) newGCSRequest(ctx context.Context, method, requestURL string, body io.Reader) (*http.Request, error) {
	token, err := s.bearerToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

func (s *GCSSyncer) bearerToken(ctx context.Context) (string, error) {
	if s.token != "" && time.Until(s.tokenExpiry) > gcsTokenRefreshPadding {
		return s.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gceMetadataTokenURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", gcsResponseError(resp)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("metadata token response did not include an access token")
	}
	s.token = body.AccessToken
	s.tokenExpiry = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	return s.token, nil
}

func gcsResponseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("gcs %s: %s", resp.Status, strings.TrimSpace(string(body)))
}

type localFile struct {
	path    string
	relPath string
	size    int64
	modTime time.Time
}

func (s *GCSSyncer) pruneLocal(ctx context.Context, now time.Time) error {
	if s.retention <= 0 && s.maxBytes <= 0 {
		return nil
	}
	files, totalBytes, err := s.localTranscriptFiles()
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

	files = files[:0]
	for _, file := range selected {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].modTime.Equal(files[j].modTime) {
			return files[i].path < files[j].path
		}
		return files[i].modTime.Before(files[j].modTime)
	})
	for _, file := range files {
		if err := s.archiveAndRemove(ctx, file); err != nil {
			return err
		}
	}
	_ = pruneEmptyDirs(s.sourceDir)
	return nil
}

func (s *GCSSyncer) localTranscriptFiles() ([]localFile, int64, error) {
	return collectTranscriptFiles(s.sourceDir)
}

// collectTranscriptFiles walks a transcript spool. Shared with the Azure
// syncer so both destinations agree on what a transcript file is and on the
// relative names they archive under.
func collectTranscriptFiles(sourceDir string) ([]localFile, int64, error) {
	var files []localFile
	var totalBytes int64
	err := filepath.WalkDir(sourceDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		files = append(files, localFile{
			path:    path,
			relPath: filepath.ToSlash(relPath),
			size:    info.Size(),
			modTime: info.ModTime(),
		})
		totalBytes += info.Size()
		return nil
	})
	return files, totalBytes, err
}

func (s *GCSSyncer) archiveAndRemove(ctx context.Context, file localFile) error {
	info, err := os.Stat(file.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() != file.size || !info.ModTime().Equal(file.modTime) {
		return nil
	}
	archiveObject := s.archiveObjectName(file)
	archiveURI, err := s.archiveURI(file, archiveObject)
	if err != nil {
		return err
	}

	copyCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if s.command != "" {
		if err := s.runCommand(copyCtx, "cp", "-n", s.destination+file.relPath, archiveURI); err != nil {
			return err
		}
	} else {
		if err := s.copyObject(copyCtx, s.objectName(file.relPath), archiveObject); err != nil {
			if errors.Is(err, errGCSObjectNotFound) {
				return nil
			}
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

func (s *GCSSyncer) archiveObjectName(file localFile) string {
	return s.objectName("_archive/" + file.relPath + "/" + s.archiveFileName(file))
}

func (s *GCSSyncer) archiveURI(_ localFile, archiveObject string) (string, error) {
	return "gs://" + s.bucket + "/" + archiveObject, nil
}

func (s *GCSSyncer) archiveFileName(file localFile) string {
	return archiveFileName(file)
}

// archiveFileName names the immutable copy taken before a local transcript is
// deleted: modification time, size, and content hash, so re-archiving the same
// bytes is a no-op and a later append lands under a different name.
func archiveFileName(file localFile) string {
	sum, err := fileSHA256(file.path)
	if err != nil {
		return file.modTime.UTC().Format("20060102T150405.000000000Z") + fmt.Sprintf("-%d.jsonl", file.size)
	}
	return fmt.Sprintf("%s-%d-%s.jsonl", file.modTime.UTC().Format("20060102T150405.000000000Z"), file.size, sum[:16])
}

func (s *GCSSyncer) objectName(relPath string) string {
	relPath = strings.TrimLeft(filepath.ToSlash(relPath), "/")
	if s.prefix == "" {
		return relPath
	}
	return s.prefix + "/" + relPath
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func pruneEmptyDirs(root string) error {
	var dirs []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(dirs, func(i, j int) bool {
		return len(dirs[i]) > len(dirs[j])
	})
	for _, dir := range dirs {
		_ = os.Remove(dir)
	}
	return nil
}

func normalizeGCSDestination(destination string) (string, string, string) {
	destination = strings.TrimSpace(destination)
	if destination == "" {
		return "", "", ""
	}
	if !strings.HasPrefix(destination, "gs://") {
		return "", "", ""
	}
	withoutScheme := strings.TrimPrefix(destination, "gs://")
	bucket, prefix, _ := strings.Cut(withoutScheme, "/")
	bucket = strings.TrimSpace(bucket)
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if bucket == "" {
		return "", "", ""
	}
	normalized := "gs://" + bucket + "/"
	if prefix != "" {
		normalized += prefix + "/"
	}
	return normalized, bucket, prefix
}

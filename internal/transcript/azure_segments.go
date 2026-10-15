package transcript

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// AzureBlobClientConfig names a container (with optional prefix) and its
// credential, for reading and writing segments without a spool to sync.
type AzureBlobClientConfig struct {
	Destination string
	AccountKey  string
	SASToken    string
	// AccessTier is the tier new segments are written in ("Cool" by default).
	// Segments are written once and read rarely, so they skip Hot; the
	// account's lifecycle rule moves block blobs on to Archive.
	AccessTier        string
	MaxBytesPerSecond int64
	Client            *http.Client
}

// NewAzureBlobClient returns a client for segment upload, listing and
// download, or an error when the destination or credential is unusable.
func NewAzureBlobClient(config AzureBlobClientConfig) (*AzureBlobSyncer, error) {
	account, container, prefix, endpoint, err := parseAzureBlobDestination(config.Destination)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(config.AccountKey)
	sas := strings.TrimSpace(config.SASToken)
	if key == "" && sas == "" {
		return nil, fmt.Errorf("azure blob destination %q has no account key or SAS token", config.Destination)
	}
	var decodedKey []byte
	if key != "" {
		decodedKey, err = base64.StdEncoding.DecodeString(key)
		if err != nil {
			return nil, fmt.Errorf("azure account key is not base64")
		}
	}
	client := config.Client
	if client == nil {
		client = &http.Client{}
	}
	tier := strings.TrimSpace(config.AccessTier)
	if tier == "" {
		tier = "Cool"
	}
	syncer := &AzureBlobSyncer{
		account:    account,
		container:  container,
		prefix:     prefix,
		endpoint:   endpoint,
		accountKey: decodedKey,
		sasQuery:   strings.TrimPrefix(sas, "?"),
		timeout:    defaultAzureBlobSyncTimeout,
		client:     client,
		now:        nowFunc,
		accessTier: tier,
		pace:       newUploadPacer(config.MaxBytesPerSecond, nil, nil),
	}
	syncer.logger = discardLogger()
	return syncer, nil
}

// Destination for a client built without a spool.
func (s *AzureBlobSyncer) segmentDestination() string {
	destination := s.endpoint + "/" + s.container
	if s.prefix != "" {
		destination += "/" + s.prefix
	}
	return destination
}

// UploadSegment writes a compressed segment as a block blob and verifies the
// stored length and MD5 before returning. Block blobs, unlike the append
// blobs the raw spool used, can be tiered to Cool and Archive.
func (s *AzureBlobSyncer) UploadSegment(ctx context.Context, localPath string, info SegmentInfo) error {
	sum, size, err := fileMD5(localPath)
	if err != nil {
		return err
	}
	if size > azureSingleBlobMaxBytes {
		return fmt.Errorf("segment %s is %d bytes, over the single-request blob limit", info.SegmentID, size)
	}
	md5Header := base64.StdEncoding.EncodeToString(sum)
	name := s.blobName(SegmentBlobName(info))
	headers := map[string]string{
		"x-ms-blob-type":             "BlockBlob",
		"Content-Type":               "application/zstd",
		"Content-MD5":                md5Header,
		"x-ms-meta-subrouter_format": SegmentFormat,
		"x-ms-meta-segment_id":       safeMetaValue(info.SegmentID),
		"x-ms-meta-host":             safeMetaValue(info.Host),
		"x-ms-meta-raw_bytes":        strconv.FormatInt(info.RawBytes, 10),
		"x-ms-meta-raw_sha256":       safeMetaValue(info.RawSHA256),
		"x-ms-meta-compressed_bytes": strconv.FormatInt(size, 10),
	}
	if s.accessTier != "" {
		headers["x-ms-access-tier"] = s.accessTier
	}
	file, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer file.Close()
	body := func() (io.Reader, error) {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return file, nil
	}
	if err := s.pace.reserve(ctx, size); err != nil {
		return err
	}
	reader, err := body()
	if err != nil {
		return err
	}
	req, err := s.newRequest(ctx, http.MethodPut, name, "", headers, reader, size)
	if err != nil {
		return err
	}
	resp, err := s.do(ctx, req, body, size)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return azureBlobResponseError(resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()

	stored, err := s.headBlob(ctx, name)
	if err != nil {
		return err
	}
	if !stored.exists || stored.appendBlob || stored.size != size || stored.md5 != md5Header {
		return fmt.Errorf("%w: %s stored size=%d md5=%q, local size=%d md5=%q", errSegmentVerify, name, stored.size, stored.md5, size, md5Header)
	}
	return nil
}

// azureSingleBlobMaxBytes is the Put Blob limit for one request.
const azureSingleBlobMaxBytes = 5000 << 20

type azureStoredBlob struct {
	exists     bool
	appendBlob bool
	size       int64
	md5        string
}

func (s *AzureBlobSyncer) headBlob(ctx context.Context, name string) (azureStoredBlob, error) {
	req, err := s.newRequest(ctx, http.MethodHead, name, "", nil, nil, 0)
	if err != nil {
		return azureStoredBlob{}, err
	}
	resp, err := s.do(ctx, req, func() (io.Reader, error) { return nil, nil }, 0)
	if err != nil {
		return azureStoredBlob{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return azureStoredBlob{}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return azureStoredBlob{}, azureBlobResponseError(resp)
	}
	stored := azureStoredBlob{
		exists:     true,
		appendBlob: strings.EqualFold(resp.Header.Get("x-ms-blob-type"), "AppendBlob"),
		md5:        resp.Header.Get("Content-MD5"),
	}
	stored.size, _ = strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	return stored, nil
}

// AzureBlobEntry is one listed blob.
type AzureBlobEntry struct {
	Name     string
	Size     int64
	BlobType string
	Tier     string
}

// ListBlobs lists blob names under prefix, relative to the container (not to
// the client's own prefix), following continuation markers.
func (s *AzureBlobSyncer) ListBlobs(ctx context.Context, prefix string, visit func(AzureBlobEntry) error) error {
	marker := ""
	for {
		query := "restype=container&comp=list&maxresults=5000&prefix=" + url.QueryEscape(prefix)
		if marker != "" {
			query += "&marker=" + url.QueryEscape(marker)
		}
		req, err := s.newContainerRequest(ctx, query)
		if err != nil {
			return err
		}
		resp, err := s.do(ctx, req, func() (io.Reader, error) { return nil, nil }, 0)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("azure list %s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		if readErr != nil {
			return readErr
		}
		var result struct {
			Blobs []struct {
				Name       string `xml:"Name"`
				Properties struct {
					ContentLength int64  `xml:"Content-Length"`
					BlobType      string `xml:"BlobType"`
					AccessTier    string `xml:"AccessTier"`
				} `xml:"Properties"`
			} `xml:"Blobs>Blob"`
			NextMarker string `xml:"NextMarker"`
		}
		if err := xml.Unmarshal(bytes.TrimPrefix(body, []byte("\xef\xbb\xbf")), &result); err != nil {
			return err
		}
		for _, blob := range result.Blobs {
			if err := visit(AzureBlobEntry{Name: blob.Name, Size: blob.Properties.ContentLength, BlobType: blob.Properties.BlobType, Tier: blob.Properties.AccessTier}); err != nil {
				return err
			}
		}
		if result.NextMarker == "" {
			return nil
		}
		marker = result.NextMarker
	}
}

// newContainerRequest signs a container-level GET (List Blobs).
func (s *AzureBlobSyncer) newContainerRequest(ctx context.Context, query string) (*http.Request, error) {
	target := s.endpoint + "/" + s.container + "?" + query
	if s.sasQuery != "" && len(s.accountKey) == 0 {
		target += "&" + s.sasQuery
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if len(s.accountKey) == 0 {
		return req, nil
	}
	req.Header.Set("x-ms-date", s.now().UTC().Format(http.TimeFormat))
	req.Header.Set("x-ms-version", azureBlobAPIVersion)
	signature, err := s.signature(req, 0)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "SharedKey "+s.account+":"+signature)
	return req, nil
}

// OpenBlob streams a blob by its container-relative name.
func (s *AzureBlobSyncer) OpenBlob(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	req, err := s.newRequest(ctx, http.MethodGet, name, "", nil, nil, 0)
	if err != nil {
		return nil, 0, err
	}
	resp, err := s.do(ctx, req, func() (io.Reader, error) { return nil, nil }, 0)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, 0, azureBlobResponseError(resp)
	}
	return resp.Body, resp.ContentLength, nil
}

// Prefix is the client's own path prefix inside the container.
func (s *AzureBlobSyncer) Prefix() string { return s.prefix }

// safeMetaValue keeps metadata values to the ASCII Azure accepts.
func safeMetaValue(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if r >= 0x20 && r < 0x7f {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func nowFunc() time.Time { return time.Now() }

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// PutSmallBlob writes a small block blob by its container-relative name.
func (s *AzureBlobSyncer) PutSmallBlob(ctx context.Context, name string, data []byte, contentType string) error {
	body := func() (io.Reader, error) { return bytes.NewReader(data), nil }
	reader, _ := body()
	req, err := s.newRequest(ctx, http.MethodPut, name, "", map[string]string{
		"x-ms-blob-type": "BlockBlob",
		"Content-Type":   contentType,
	}, reader, int64(len(data)))
	if err != nil {
		return err
	}
	resp, err := s.do(ctx, req, body, int64(len(data)))
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

// BlobExists reports whether a container-relative blob exists.
func (s *AzureBlobSyncer) BlobExists(ctx context.Context, name string) (bool, error) {
	stored, err := s.headBlob(ctx, name)
	return stored.exists, err
}

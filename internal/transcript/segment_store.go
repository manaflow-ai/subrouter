package transcript

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
)

// segmentUploadTimeout bounds one segment upload, so a stalled connection
// cannot stop the store (and with it compression and the spool's drain).
const segmentUploadTimeout = 20 * time.Minute

// SegmentUploader stores a compressed segment durably. UploadSegment returns
// only after the stored object is verified to hold exactly the local bytes;
// the local file is deleted after that.
type SegmentUploader interface {
	UploadSegment(ctx context.Context, localPath string, info SegmentInfo) error
	Destination() string
}

// SegmentInfo describes one compressed segment. It is also the sidecar the
// store keeps next to a compressed segment until it is uploaded.
type SegmentInfo struct {
	SegmentID       string `json:"segment_id"`
	Host            string `json:"host"`
	RawBytes        int64  `json:"raw_bytes"`
	RawSHA256       string `json:"raw_sha256"`
	CompressedBytes int64  `json:"compressed_bytes"`
	// BlobName overrides the destination name derived from SegmentID.
	BlobName string `json:"blob_name,omitempty"`
}

// SegmentBlobName is the store-relative path of a segment: partitioned by the
// UTC date the segment started and the recording host.
func SegmentBlobName(info SegmentInfo) string {
	if info.BlobName != "" {
		return info.BlobName
	}
	day := "unknown-date"
	if started, err := time.Parse("20060102T150405Z", strings.SplitN(info.SegmentID, "-", 2)[0]); err == nil {
		day = started.UTC().Format("2006/01/02")
	}
	host := safeFilename(info.Host)
	return "segments/v1/" + day + "/" + host + "/" + info.SegmentID + ".jsonl.zst"
}

// segmentStore compresses sealed segments and uploads them, and keeps the
// spool accounting the writer consults before accepting an event.
type segmentStore struct {
	w    *segmentWriter
	kick chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	full          atomic.Bool
	spoolBytes    atomic.Int64
	uploaded      atomic.Int64
	uploadedBytes atomic.Int64
	uploadErrors  atomic.Int64
	compressed    atomic.Int64
	backoffUntil  time.Time
}

func newSegmentStore(w *segmentWriter) *segmentStore {
	ctx, cancel := context.WithCancel(context.Background())
	return &segmentStore{w: w, kick: make(chan struct{}, 1), ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

func (s *segmentStore) notify() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// stop abandons in-flight work; a claimed file is released so the next
// process picks it up.
func (s *segmentStore) stop() {
	s.once.Do(func() {
		s.cancel()
		<-s.done
		s.pass(context.Background(), false)
	})
}

func (s *segmentStore) run() {
	defer close(s.done)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		s.pass(s.ctx, true)
		select {
		case <-s.ctx.Done():
			return
		case <-s.kick:
		case <-ticker.C:
		}
	}
}

// pass runs one round. With work set it also compresses and uploads;
// without, it only recovers orphans and measures the spool.
func (s *segmentStore) pass(ctx context.Context, work bool) {
	s.recoverOrphans()
	s.measure()
	if !work {
		return
	}
	for _, path := range s.list("sealed", rawSegmentExt) {
		if ctx.Err() != nil {
			return
		}
		if err := s.compress(ctx, path); err != nil && ctx.Err() == nil {
			s.w.writeErrors.Add(1)
			s.w.cfg.Logger.Warn("transcript segment compression failed", "segment", filepath.Base(path), "error", err)
		}
		s.measure()
	}
	if s.w.cfg.Uploader == nil || s.w.cfg.Now().Before(s.backoffUntil) {
		return
	}
	for _, path := range s.list("upload", ".jsonl.zst") {
		if ctx.Err() != nil {
			return
		}
		if err := s.upload(ctx, path); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.uploadErrors.Add(1)
			s.backoffUntil = s.w.cfg.Now().Add(time.Minute)
			s.w.cfg.Logger.Warn("transcript segment upload failed", "segment", filepath.Base(path), "destination", s.w.cfg.Uploader.Destination(), "error", err)
			return
		}
		s.measure()
	}
}

// list returns the unclaimed files in a spool directory, oldest name first.
func (s *segmentStore) list(dir, suffix string) []string {
	entries, err := os.ReadDir(filepath.Join(s.w.root, dir))
	if err != nil {
		return nil
	}
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		paths = append(paths, filepath.Join(s.w.root, dir, entry.Name()))
	}
	sort.Strings(paths)
	return paths
}

func claimSuffix() string { return ".claim-" + strconv.Itoa(os.Getpid()) }

// claim renames path so only one process works on it; workers of two
// generations share the spool during a rollout.
func claim(path string) (string, bool) {
	claimed := path + claimSuffix()
	if err := os.Rename(path, claimed); err != nil {
		return "", false
	}
	return claimed, true
}

func (s *segmentStore) compress(ctx context.Context, path string) error {
	claimed, ok := claim(path)
	if !ok {
		return nil
	}
	release := func() { _ = os.Rename(claimed, path) }
	segID := strings.TrimSuffix(filepath.Base(path), rawSegmentExt)
	if err := os.MkdirAll(filepath.Join(s.w.root, "upload"), 0o700); err != nil {
		release()
		return err
	}
	target := filepath.Join(s.w.root, "upload", segID+".jsonl.zst")
	tmp := target + ".tmp-" + strconv.Itoa(os.Getpid())
	info, err := compressSegmentFile(ctx, claimed, tmp, s.w.cfg.CompressLevel, s.w.cfg.WindowSize)
	if err != nil {
		_ = os.Remove(tmp)
		release()
		return err
	}
	info.SegmentID = segID
	info.Host = s.w.cfg.Host
	if err := writeSidecar(target, info); err != nil {
		_ = os.Remove(tmp)
		release()
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		release()
		return err
	}
	s.compressed.Add(1)
	return os.Remove(claimed)
}

// compressSegmentFile compresses src to dst and proves the result decodes to
// the exact source bytes before reporting success.
func compressSegmentFile(ctx context.Context, src, dst string, level zstd.EncoderLevel, window int) (SegmentInfo, error) {
	in, err := os.Open(src)
	if err != nil {
		return SegmentInfo{}, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return SegmentInfo{}, err
	}
	encoder, err := zstd.NewWriter(out, zstd.WithEncoderLevel(level), zstd.WithWindowSize(window), zstd.WithEncoderConcurrency(1))
	if err != nil {
		_ = out.Close()
		return SegmentInfo{}, err
	}
	rawHash := sha256.New()
	rawBytes, err := io.Copy(io.MultiWriter(encoder, rawHash), contextReader{ctx: ctx, r: in})
	if err != nil {
		_ = encoder.Close()
		_ = out.Close()
		return SegmentInfo{}, err
	}
	if err := encoder.Close(); err != nil {
		_ = out.Close()
		return SegmentInfo{}, err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return SegmentInfo{}, err
	}
	if err := out.Close(); err != nil {
		return SegmentInfo{}, err
	}
	rawSum := hex.EncodeToString(rawHash.Sum(nil))
	verifyBytes, verifySum, err := decompressedDigest(ctx, dst)
	if err != nil {
		return SegmentInfo{}, fmt.Errorf("verify compressed segment: %w", err)
	}
	if verifyBytes != rawBytes || verifySum != rawSum {
		return SegmentInfo{}, fmt.Errorf("compressed segment decodes to %d bytes %s, want %d bytes %s", verifyBytes, verifySum, rawBytes, rawSum)
	}
	stat, err := os.Stat(dst)
	if err != nil {
		return SegmentInfo{}, err
	}
	return SegmentInfo{RawBytes: rawBytes, RawSHA256: rawSum, CompressedBytes: stat.Size()}, nil
}

func decompressedDigest(ctx context.Context, path string) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	decoder, err := NewSegmentDecoder(file)
	if err != nil {
		return 0, "", err
	}
	defer decoder.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, contextReader{ctx: ctx, r: decoder})
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(hash.Sum(nil)), nil
}

// NewSegmentDecoder reads a compressed segment, allowing windows up to the
// format maximum.
func NewSegmentDecoder(r io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(r, zstd.WithDecoderMaxWindow(1<<31), zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(false))
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func writeSidecar(target string, info SegmentInfo) error {
	body, err := json.Marshal(info)
	if err != nil {
		return err
	}
	tmp := target + ".json.tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, target+".json")
}

func readSidecar(target string) (SegmentInfo, error) {
	body, err := os.ReadFile(target + ".json")
	if err != nil {
		return SegmentInfo{}, err
	}
	var info SegmentInfo
	err = json.Unmarshal(body, &info)
	return info, err
}

func (s *segmentStore) upload(ctx context.Context, path string) error {
	info, err := readSidecar(path)
	if err != nil {
		// A segment compressed by a process that died before writing its
		// sidecar: describe it from the file itself.
		stat, statErr := os.Stat(path)
		if statErr != nil {
			return statErr
		}
		info = SegmentInfo{SegmentID: strings.TrimSuffix(filepath.Base(path), ".jsonl.zst"), Host: s.w.cfg.Host, CompressedBytes: stat.Size()}
	}
	claimed, ok := claim(path)
	if !ok {
		return nil
	}
	uploadCtx, cancel := context.WithTimeout(ctx, segmentUploadTimeout)
	err = s.w.cfg.Uploader.UploadSegment(uploadCtx, claimed, info)
	cancel()
	if err != nil {
		_ = os.Rename(claimed, path)
		return err
	}
	s.uploaded.Add(1)
	s.uploadedBytes.Add(info.CompressedBytes)
	s.w.cfg.Logger.Info("transcript segment uploaded", "segment", info.SegmentID, "raw_bytes", info.RawBytes, "compressed_bytes", info.CompressedBytes, "destination", s.w.cfg.Uploader.Destination())
	_ = os.Remove(path + ".json")
	return os.Remove(claimed)
}

// recoverOrphans returns work abandoned by a process that exited: its live
// segment is sealed as it is (a reader tolerates a missing footer), and its
// claims are released.
func (s *segmentStore) recoverOrphans() {
	liveRoot := filepath.Join(s.w.root, "live")
	entries, _ := os.ReadDir(liveRoot)
	for _, entry := range entries {
		dir := filepath.Join(liveRoot, entry.Name())
		if !entry.IsDir() || dir == s.w.live {
			continue
		}
		if pid, err := strconv.Atoi(strings.SplitN(entry.Name(), "-", 2)[0]); err == nil && (pid == os.Getpid() || processAlive(pid)) {
			continue
		}
		release, ok := tryLockLiveDir(dir)
		if !ok {
			continue
		}
		files, _ := os.ReadDir(dir)
		for _, file := range files {
			if strings.HasSuffix(file.Name(), rawSegmentExt) {
				_ = os.Rename(filepath.Join(dir, file.Name()), filepath.Join(s.w.root, "sealed", file.Name()))
			}
		}
		release()
		_ = os.RemoveAll(dir)
	}
	for _, dir := range []string{"sealed", "upload"} {
		files, _ := os.ReadDir(filepath.Join(s.w.root, dir))
		for _, file := range files {
			name := file.Name()
			path := filepath.Join(s.w.root, dir, name)
			if index := strings.LastIndex(name, ".claim-"); index > 0 {
				pid, err := strconv.Atoi(name[index+len(".claim-"):])
				if err == nil && pid != os.Getpid() && !processAlive(pid) {
					_ = os.Rename(path, filepath.Join(s.w.root, dir, name[:index]))
				}
				continue
			}
			if index := strings.LastIndex(name, ".tmp-"); index > 0 {
				pid, err := strconv.Atoi(name[index+len(".tmp-"):])
				if err == nil && pid != os.Getpid() && !processAlive(pid) {
					_ = os.Remove(path)
				}
			}
		}
	}
}

// measure totals the spool and flips the writer's admission when it crosses
// the cap.
func (s *segmentStore) measure() {
	var total int64
	_ = filepath.WalkDir(s.w.root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	s.spoolBytes.Store(total)
	limit := s.w.cfg.MaxLocalBytes
	full := limit > 0 && total >= limit
	if full != s.full.Load() {
		s.full.Store(full)
		if full {
			s.w.cfg.Logger.Warn("transcript spool is full; new events are dropped until uploads catch up", "spool_bytes", total, "max_local_bytes", limit)
		} else {
			s.w.cfg.Logger.Info("transcript spool has room again", "spool_bytes", total, "max_local_bytes", limit)
		}
	}
}

// errSegmentVerify reports that the stored object did not match the local
// segment.
var errSegmentVerify = errors.New("stored segment does not match the local file")

func fileMD5(path string) ([]byte, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	hash := md5.New()
	n, err := io.Copy(hash, file)
	if err != nil {
		return nil, 0, err
	}
	return hash.Sum(nil), n, nil
}

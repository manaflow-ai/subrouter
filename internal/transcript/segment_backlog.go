package transcript

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LegacySpoolOptions configures a conversion pass over a raw (jsonl format)
// transcript spool: Dir/by-agent/<agent>/by-session/<session>.jsonl.
type LegacySpoolOptions struct {
	// Dir is the transcript directory holding by-agent/.
	Dir string
	// Quiet skips files written to more recently than this; a process of the
	// old format may still be appending to them.
	Quiet time.Duration
	// MaxBytes bounds the source bytes one pass reads. Zero means no bound.
	MaxBytes int64
	// Delete removes each source file once the segment holding its last
	// event is uploaded and verified. It requires Segment.Uploader.
	Delete bool
	// Segment configures the output segments. Segment.Dir is a work
	// directory owned by this pass; it is emptied at the start.
	Segment SegmentConfig
	Logger  *slog.Logger
	// KeepWork leaves the produced segments in Segment.Dir (for a dry run).
	KeepWork bool
}

// LegacySpoolResult reports one pass.
type LegacySpoolResult struct {
	Files           int   `json:"files"`
	SourceBytes     int64 `json:"source_bytes"`
	SegmentRawBytes int64 `json:"segment_raw_bytes"`
	CompressedBytes int64 `json:"compressed_bytes"`
	Segments        int   `json:"segments"`
	Deleted         int   `json:"deleted"`
	Mismatched      int64 `json:"mismatched_bodies"`
	Incomplete      int64 `json:"incomplete_streams"`
}

type legacySource struct {
	path    string
	rel     string
	size    int64
	modTime time.Time
	claimed string
}

// ConvertLegacySpool converts quiet raw transcript files into segments, and
// with Delete set removes each source only after its data is stored and
// verified. A file that changes after it was read is left in place.
func ConvertLegacySpool(ctx context.Context, options LegacySpoolOptions) (LegacySpoolResult, error) {
	var result LegacySpoolResult
	if options.Delete && options.Segment.Uploader == nil {
		return result, errors.New("deleting converted transcripts needs an uploader")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	root := filepath.Join(options.Dir, "by-agent")
	releaseDeadClaims(root)
	sources, err := quietLegacySources(root, options.Quiet, options.MaxBytes)
	if err != nil || len(sources) == 0 {
		return result, err
	}
	// Work left by an interrupted pass is discarded: its sources were not
	// deleted, so they are converted again (an uploaded duplicate is
	// harmless; a lost file is not).
	_ = os.RemoveAll(options.Segment.Dir)
	if err := os.MkdirAll(options.Segment.Dir, 0o700); err != nil {
		return result, err
	}
	converter, err := NewLegacyConverter(ctx, options.Segment)
	if err != nil {
		return result, err
	}
	bySource := map[string]*legacySource{}
	converter.OnSealed = func(info SegmentInfo, completed []string) error {
		result.Segments++
		result.SegmentRawBytes += info.RawBytes
		result.CompressedBytes += info.CompressedBytes
		if !options.Delete {
			return nil
		}
		for _, name := range completed {
			source := bySource[name]
			if source == nil || source.claimed == "" {
				continue
			}
			info, err := os.Stat(source.claimed)
			if err != nil {
				continue
			}
			if info.Size() != source.size {
				// Appended to after it was read: put it back for a later pass.
				_ = os.Rename(source.claimed, source.path)
				source.claimed = ""
				continue
			}
			if err := os.Remove(source.claimed); err == nil {
				result.Deleted++
				source.claimed = ""
			}
		}
		return nil
	}
	release := func() {
		for _, source := range bySource {
			if source.claimed != "" {
				_ = os.Rename(source.claimed, source.path)
				source.claimed = ""
			}
		}
	}
	for _, source := range sources {
		if ctx.Err() != nil {
			release()
			return result, ctx.Err()
		}
		readPath := source.path
		if options.Delete {
			claimed, ok := claim(source.path)
			if !ok {
				continue
			}
			source.claimed = claimed
			readPath = claimed
		}
		file, err := os.Open(readPath)
		if err != nil {
			continue
		}
		bySource[source.rel] = source
		err = converter.Convert(source.rel, file)
		_ = file.Close()
		result.Files++
		result.SourceBytes += source.size
		if err != nil {
			release()
			return result, err
		}
	}
	if err := converter.Close(); err != nil {
		release()
		return result, err
	}
	release()
	result.Mismatched = converter.Mismatched
	result.Incomplete = converter.Incomplete
	if options.Delete {
		_ = pruneEmptyDirs(root)
	}
	if !options.KeepWork {
		_ = os.RemoveAll(options.Segment.Dir)
	}
	logger.Info("legacy transcripts converted", "files", result.Files, "source_bytes", result.SourceBytes,
		"compressed_bytes", result.CompressedBytes, "segments", result.Segments, "deleted", result.Deleted,
		"mismatched_bodies", result.Mismatched)
	return result, nil
}

func quietLegacySources(root string, quiet time.Duration, maxBytes int64) ([]*legacySource, error) {
	var sources []*legacySource
	now := time.Now()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		// "._" files are macOS AppleDouble metadata on non-Apple volumes.
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") || strings.HasPrefix(entry.Name(), "._") {
			return nil
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < quiet {
			return nil
		}
		rel, _ := filepath.Rel(filepath.Dir(root), path)
		sources = append(sources, &legacySource{path: path, rel: filepath.ToSlash(rel), size: info.Size(), modTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].modTime.Before(sources[j].modTime) })
	if maxBytes > 0 {
		var total int64
		for index, source := range sources {
			total += source.size
			if total > maxBytes && index > 0 {
				sources = sources[:index]
				break
			}
		}
	}
	return sources, nil
}

// releaseDeadClaims puts back files claimed by a pass whose process exited.
func releaseDeadClaims(root string) {
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		name := entry.Name()
		index := strings.LastIndex(name, ".claim-")
		if index <= 0 {
			return nil
		}
		pid, err := strconv.Atoi(name[index+len(".claim-"):])
		if err != nil || pid == os.Getpid() || processAlive(pid) {
			return nil
		}
		original := filepath.Join(filepath.Dir(path), name[:index])
		if _, err := os.Stat(original); err == nil {
			// The old process wrote a new file under the name meanwhile;
			// keep both by giving the claimed one a distinct name.
			original = strings.TrimSuffix(original, ".jsonl") + ".recovered-" + strconv.Itoa(pid) + ".jsonl"
		}
		_ = os.Rename(path, original)
		return nil
	})
}

// RunLegacySpoolConversion converts the raw spool in the background until ctx
// ends: one bounded pass, then a pause.
func RunLegacySpoolConversion(ctx context.Context, options LegacySpoolOptions, interval time.Duration) {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	for {
		result, err := ConvertLegacySpool(ctx, options)
		if err != nil && ctx.Err() == nil {
			logger.Warn("legacy transcript conversion failed; sources kept for the next pass", "error", err)
		}
		wait := interval
		if err == nil && result.Files > 0 && options.MaxBytes > 0 && result.SourceBytes >= options.MaxBytes {
			// A full pass means more backlog: continue after a short breath.
			wait = 5 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

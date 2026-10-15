package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/manaflow-ai/subrouter/internal/buildversion"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/internal/transcript"
)

// serveTranscriptOptions are the serve flags that decide how transcripts are
// recorded.
type serveTranscriptOptions struct {
	dir           string
	storeURL      string
	host          string
	maxSegmentAge time.Duration
	idleSeal      time.Duration
	indexFlush    time.Duration
	whois         proxy.TokenUsageWhoIs
}

// newServeTranscriptRecorder returns the recorder serve installs and whether
// it streams to blob storage. A container URL selects streaming (nothing
// touches local disk); otherwise --transcripts keeps the raw per-session
// files it always wrote.
func newServeTranscriptRecorder(options serveTranscriptOptions) (*transcript.Recorder, bool, error) {
	storeURL := strings.TrimSpace(options.storeURL)
	if storeURL == "" {
		storeURL = strings.TrimSpace(os.Getenv("SUBROUTER_TRANSCRIPT_AZURE_CONTAINER_URL"))
	}
	if storeURL == "" {
		if strings.TrimSpace(options.dir) == "" {
			return nil, false, nil
		}
		return transcript.NewRecorder(options.dir), false, nil
	}
	if strings.TrimSpace(options.dir) != "" {
		slog.Warn("--transcripts is ignored: transcripts stream to the blob container", "container", storeURL)
	}
	client, err := transcriptStoreClient(storeURL)
	if err != nil {
		return nil, false, fmt.Errorf("transcript container: %w", err)
	}
	host := strings.TrimSpace(options.host)
	if host == "" {
		// An environment setting, like the container URL, so a worker config
		// that names it still starts an older binary on rollback.
		host = strings.TrimSpace(os.Getenv("SUBROUTER_TRANSCRIPT_HOST"))
	}
	recorder, err := transcript.NewSegmentRecorder(transcript.SegmentConfig{
		Uploader:      client,
		Host:          host,
		MaxSegmentAge: options.maxSegmentAge,
		IdleSeal:      options.idleSeal,
		IndexFlush:    options.indexFlush,
		Logger:        slog.Default(),
		Version:       buildversion.Get().String(),
		WhoIs:         whoisFunc(options.whois),
	})
	if err != nil {
		return nil, false, err
	}
	slog.Info("transcripts stream to blob storage", "container", client.Destination())
	return recorder, true, nil
}

func whoisFunc(resolver proxy.TokenUsageWhoIs) transcript.WhoIsFunc {
	if resolver == nil {
		return nil
	}
	return func(ctx context.Context, remoteAddr string) (string, string, bool) {
		identity, ok := resolver.Lookup(ctx, remoteAddr)
		if !ok {
			return "", "", false
		}
		node := strings.TrimSuffix(identity.NodeName, ".")
		if index := strings.IndexByte(node, '.'); index > 0 {
			node = node[:index]
		}
		login := identity.LoginName
		if login == "" && len(identity.Tags) > 0 {
			login = strings.Join(identity.Tags, ",")
		}
		return node, login, node != "" || login != ""
	}
}

// transcriptStoreClient opens the container with the SAS or account key from
// the environment. Secrets come from files, never argv.
func transcriptStoreClient(containerURL string) (*transcript.AzureBlobSyncer, error) {
	sas, err := secretFromEnvironment("SUBROUTER_TRANSCRIPT_AZURE_SAS", "SUBROUTER_TRANSCRIPT_AZURE_SAS_FILE")
	if err != nil {
		return nil, err
	}
	key := ""
	if sas == "" {
		// The account key variables are shared with the raw-file syncer and
		// may name another account; a SAS, scoped to this container, wins.
		key, err = secretFromEnvironment("SUBROUTER_TRANSCRIPT_AZURE_KEY", "SUBROUTER_TRANSCRIPT_AZURE_KEY_FILE")
		if err != nil {
			return nil, err
		}
	}
	return transcript.NewAzureBlobClient(transcript.AzureBlobClientConfig{
		Destination:       containerURL,
		AccountKey:        key,
		SASToken:          strings.TrimPrefix(sas, "?"),
		MaxBytesPerSecond: -1,
	})
}

func runTranscriptsCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: subrouter transcripts export|verify|recompress [flags]")
	}
	switch args[0] {
	case "export":
		return transcriptsExport(args[1:], false)
	case "verify":
		return transcriptsExport(args[1:], true)
	case "recompress":
		return transcriptsRecompress(args[1:])
	default:
		return fmt.Errorf("unknown transcripts command %q (want export, verify or recompress)", args[0])
	}
}

// transcriptsExport rebuilds calls. With segment files as arguments it
// replays them whole. With --container it reads the index for a date range
// and rebuilds each call from just the byte ranges its row names, writing one
// JSON line per call (index row plus request and response bodies). verify
// writes nothing but stats and fails when any body does not rebuild exactly.
func transcriptsExport(args []string, verifyOnly bool) error {
	flags := flag.NewFlagSet("transcripts export", flag.ContinueOnError)
	container := flags.String("container", "", "container URL; defaults to SUBROUTER_TRANSCRIPT_AZURE_CONTAINER_URL (read credential: SUBROUTER_TRANSCRIPT_AZURE_SAS_FILE with read+list, or SUBROUTER_TRANSCRIPT_AZURE_KEY_FILE)")
	from := flags.String("from", "", "first UTC date (YYYY-MM-DD); default today")
	to := flags.String("to", "", "last UTC date (YYYY-MM-DD); default --from")
	session := flags.String("session", "", "only calls of this session id")
	call := flags.String("call", "", "only this request id")
	output := flags.String("out", "-", "output file, - for stdout")
	workers := flags.Int("workers", 8, "concurrent range reads")
	if err := flags.Parse(args); err != nil {
		return err
	}
	var out io.Writer = io.Discard
	if !verifyOnly {
		if *output == "-" {
			out = os.Stdout
		} else {
			file, err := os.Create(*output)
			if err != nil {
				return err
			}
			defer file.Close()
			out = file
		}
	}
	buffered := bufio.NewWriterSize(out, 1<<20)
	defer buffered.Flush()
	encoder := json.NewEncoder(buffered)
	encoder.SetEscapeHTML(false)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if files := flags.Args(); len(files) > 0 {
		exporter := transcript.NewSegmentExporter(func(record transcript.ExportRecord) error { return encoder.Encode(record) })
		sort.Strings(files)
		for _, path := range files {
			if err := exportLocalSegment(exporter, path); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		}
		exporter.Finish()
		stats, _ := json.Marshal(exporter.Stats)
		fmt.Fprintln(os.Stderr, string(stats))
		if exporter.Stats.Failed() {
			return fmt.Errorf("%d bodies did not match their sha256 and %d could not be decoded", exporter.Stats.Mismatched, exporter.Stats.DecodeErrors)
		}
		return nil
	}

	containerURL := strings.TrimSpace(*container)
	if containerURL == "" {
		containerURL = strings.TrimSpace(os.Getenv("SUBROUTER_TRANSCRIPT_AZURE_CONTAINER_URL"))
	}
	if containerURL == "" {
		return errors.New("give segment files or --container")
	}
	client, err := transcriptStoreClient(containerURL)
	if err != nil {
		return err
	}
	// Index blobs are named by when they were written, up to an hour or so
	// after the call; read one more day and keep rows by call time.
	first, last, err := dateRange(*from, *to)
	if err != nil {
		return err
	}
	prefixes, err := datePrefixes("index/v1/", first.Format("2006-01-02"), last.AddDate(0, 0, 1).Format("2006-01-02"))
	if err != nil {
		return err
	}
	inRange := func(row transcript.IndexRow) bool {
		day := row.Timestamp
		if len(day) >= 10 {
			day = day[:10]
		}
		return day >= first.Format("2006-01-02") && day <= last.Format("2006-01-02")
	}
	var rows []transcript.IndexRow
	for _, prefix := range prefixes {
		var parts []string
		listPrefix := prefix
		if client.Prefix() != "" {
			listPrefix = client.Prefix() + "/" + prefix
		}
		if err := client.ListBlobs(ctx, listPrefix, func(entry transcript.AzureBlobEntry) error {
			parts = append(parts, entry.Name)
			return nil
		}); err != nil {
			return err
		}
		for _, part := range parts {
			body, _, err := client.OpenBlob(ctx, part)
			if err != nil {
				return err
			}
			err = transcript.ReadIndexRows(body, func(row transcript.IndexRow) error {
				if inRange(row) && (*session == "" || row.SessionID == *session || transcript.BaseSessionID(row.SessionID) == *session) && (*call == "" || row.RequestID == *call) {
					rows = append(rows, row)
				}
				return nil
			})
			_ = body.Close()
			if err != nil {
				return fmt.Errorf("%s: %w", part, err)
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Timestamp < rows[j].Timestamp })
	read := func(blob string, offset, length int64) ([]byte, error) {
		return client.ReadRange(ctx, blob, offset, length)
	}
	// Calls are rebuilt in windows, so memory holds one window of bodies.
	const window = 256
	var verified, failed, lost int
	for start := 0; start < len(rows) && ctx.Err() == nil; start += window {
		batch := rows[start:min(start+window, len(rows))]
		results := make([]transcript.CallBodies, len(batch))
		errs := make([]error, len(batch))
		var next atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < max(1, *workers); i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					index := int(next.Add(1) - 1)
					if index >= len(batch) || ctx.Err() != nil {
						return
					}
					results[index], errs[index] = transcript.ExportCall(batch[index], read)
				}
			}()
		}
		wg.Wait()
		for index, result := range results {
			switch {
			case errs[index] != nil:
				failed++
				fmt.Fprintf(os.Stderr, "%s: %v\n", batch[index].RequestID, errs[index])
				continue
			case batch[index].BodyLost:
				lost++
			case result.Verified:
				verified++
			default:
				failed++
			}
			if err := encoder.Encode(result); err != nil {
				return err
			}
		}
	}
	summary, _ := json.Marshal(map[string]int{"calls": len(rows), "verified": verified, "failed": failed, "body_lost": lost})
	fmt.Fprintln(os.Stderr, string(summary))
	if failed > 0 {
		return fmt.Errorf("%d calls did not rebuild exactly", failed)
	}
	return nil
}

func dateRange(from, to string) (time.Time, time.Time, error) {
	start := time.Now().UTC().Truncate(24 * time.Hour)
	if from != "" {
		parsed, err := time.Parse("2006-01-02", from)
		if err != nil {
			return start, start, fmt.Errorf("--from: %w", err)
		}
		start = parsed
	}
	end := start
	if to != "" {
		parsed, err := time.Parse("2006-01-02", to)
		if err != nil {
			return start, start, fmt.Errorf("--to: %w", err)
		}
		end = parsed
	}
	return start, end, nil
}

func datePrefixes(base, from, to string) ([]string, error) {
	start := time.Now().UTC()
	if from != "" {
		parsed, err := time.Parse("2006-01-02", from)
		if err != nil {
			return nil, fmt.Errorf("--from: %w", err)
		}
		start = parsed
	}
	end := start
	if to != "" {
		parsed, err := time.Parse("2006-01-02", to)
		if err != nil {
			return nil, fmt.Errorf("--to: %w", err)
		}
		end = parsed
	}
	var prefixes []string
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		prefixes = append(prefixes, base+day.Format("2006/01/02")+"/")
	}
	return prefixes, nil
}

func exportLocalSegment(exporter *transcript.SegmentExporter, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if strings.HasSuffix(path, ".zst") {
		decoder, err := transcript.NewSegmentDecoder(file)
		if err != nil {
			return err
		}
		defer decoder.Close()
		return exporter.ReadSegment(filepath.Base(path), decoder)
	}
	return exporter.ReadSegment(filepath.Base(path), file)
}

// countingUploader discards what it is given and counts it (dry runs and
// level measurements).
type countingUploader struct {
	blocks, bytes, blobs, blobBytes atomic.Int64
}

func (u *countingUploader) PutBlock(_ context.Context, _, _ string, data []byte) error {
	u.blocks.Add(1)
	u.bytes.Add(int64(len(data)))
	return nil
}

func (u *countingUploader) PutBlockList(context.Context, string, []string, string, map[string]string) error {
	return nil
}

func (u *countingUploader) PutBlob(_ context.Context, _ string, data []byte, _ string) error {
	u.blobs.Add(1)
	u.blobBytes.Add(int64(len(data)))
	return nil
}

func (u *countingUploader) Destination() string { return "dry-run" }

// transcriptsRecompress converts raw per-session transcripts into the
// segment format: a local spool (--local-dir, deleting each file once its
// segments are committed) or the old account's append blobs (--source,
// never deleting anything).
func transcriptsRecompress(args []string) error {
	flags := flag.NewFlagSet("transcripts recompress", flag.ContinueOnError)
	localDir := flags.String("local-dir", "", "raw transcript spool (the directory holding by-agent/)")
	sourceURL := flags.String("source", "", "container URL with a raw transcript prefix in the old account, e.g. https://<old-account>.blob.core.windows.net/transcripts/cmux-lawrence/by-agent (read credential: SUBROUTER_TRANSCRIPT_SOURCE_KEY_FILE)")
	container := flags.String("container", "", "destination container URL; defaults to SUBROUTER_TRANSCRIPT_AZURE_CONTAINER_URL")
	deleteSources := flags.Bool("delete", false, "with --local-dir: delete each source file after its segments are committed")
	dryRun := flags.Bool("dry-run", false, "compress and count, upload nothing, delete nothing")
	quiet := flags.Duration("quiet", 15*time.Minute, "with --local-dir: skip files written to more recently than this")
	maxBlobs := flags.Int("max-blobs", 0, "with --source: convert at most this many blobs (0: no limit)")
	match := flags.String("match", "", "only sources whose name contains this text")
	level := flags.Int("level", 4, "zstd level 1-4 (fastest, default, better, best)")
	frameBytes := flags.String("frame-bytes", "16MiB", "raw bytes per independent zstd frame")
	host := flags.String("host", "", "recording host label; default the machine host name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	frame, err := parseByteSize(*frameBytes)
	if err != nil {
		return err
	}
	if *deleteSources && *dryRun {
		return errors.New("--delete cannot be combined with --dry-run")
	}
	var uploader transcript.BlockUploader
	counting := &countingUploader{}
	if *dryRun {
		uploader = counting
	} else {
		containerURL := strings.TrimSpace(*container)
		if containerURL == "" {
			containerURL = strings.TrimSpace(os.Getenv("SUBROUTER_TRANSCRIPT_AZURE_CONTAINER_URL"))
		}
		if containerURL == "" {
			return errors.New("--container is required unless --dry-run")
		}
		client, err := transcriptStoreClient(containerURL)
		if err != nil {
			return err
		}
		uploader = client
	}
	levels := map[int]zstd.EncoderLevel{1: zstd.SpeedFastest, 2: zstd.SpeedDefault, 3: zstd.SpeedBetterCompression, 4: zstd.SpeedBestCompression}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	converter, err := transcript.NewLegacyConverter(ctx, transcript.SegmentConfig{
		Uploader:      uploader,
		Host:          *host,
		FrameBytes:    int(frame),
		CompressLevel: levels[*level],
		Logger:        slog.Default(),
		Version:       buildversion.Get().String(),
		CloseTimeout:  30 * time.Minute,
	})
	if err != nil {
		return err
	}
	var deleted, kept atomic.Int64
	var storedSources []string
	deleteStored := func() {}
	started := time.Now()
	switch {
	case *localDir != "":
		sources := map[string]legacyFile{}
		var mu sync.Mutex
		converter.OnSourceStored = func(source string, ok bool) {
			mu.Lock()
			defer mu.Unlock()
			if ok {
				storedSources = append(storedSources, source)
			}
		}
		// Files go only after Close, which also writes the index rows that
		// point at their segments.
		deleteStored = func() {
			mu.Lock()
			defer mu.Unlock()
			for _, source := range storedSources {
				file := sources[source]
				info, err := os.Stat(file.path)
				if !*deleteSources || err != nil || info.Size() != file.size || !info.ModTime().Equal(file.modTime) {
					// Appended to after it was read: keep it for the next run.
					kept.Add(1)
					continue
				}
				if os.Remove(file.path) == nil {
					deleted.Add(1)
				}
			}
		}
		files, err := quietLegacyFiles(filepath.Join(*localDir, "by-agent"), *quiet, *match)
		if err != nil {
			return err
		}
		for _, file := range files {
			rel, _ := filepath.Rel(*localDir, file.path)
			mu.Lock()
			sources[rel] = file
			mu.Unlock()
			handle, err := os.Open(file.path)
			if err != nil {
				continue
			}
			err = converter.Convert(rel, handle)
			_ = handle.Close()
			if err != nil {
				return err
			}
		}
	case *sourceURL != "":
		key, err := secretFromEnvironment("SUBROUTER_TRANSCRIPT_SOURCE_KEY", "SUBROUTER_TRANSCRIPT_SOURCE_KEY_FILE")
		if err != nil {
			return err
		}
		source, err := transcript.NewAzureBlobClient(transcript.AzureBlobClientConfig{Destination: *sourceURL, AccountKey: key, MaxBytesPerSecond: -1})
		if err != nil {
			return err
		}
		var names []string
		if err := source.ListBlobs(ctx, source.Prefix()+"/", func(entry transcript.AzureBlobEntry) error {
			if strings.HasSuffix(entry.Name, ".jsonl") && (*match == "" || strings.Contains(entry.Name, *match)) {
				names = append(names, entry.Name)
			}
			return nil
		}); err != nil {
			return err
		}
		for index, name := range names {
			if *maxBlobs > 0 && index >= *maxBlobs {
				break
			}
			body, _, err := source.OpenBlob(ctx, name)
			if err != nil {
				return err
			}
			err = converter.Convert(name, body)
			_ = body.Close()
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	default:
		return errors.New("give --local-dir or --source")
	}
	closeErr := converter.Close()
	if closeErr == nil {
		deleteStored()
	}
	stats := converter.Stats()
	summary, _ := json.Marshal(map[string]any{
		"sources": converter.Sources, "source_bytes": converter.SourceBytes,
		"segment_raw_bytes": stats.CommittedRawBytes, "body_bytes": stats.CommittedBodyBytes,
		"stored_bytes": stats.CommittedBytes, "index_bytes_dry_run": counting.blobBytes.Load(),
		"segments": stats.CommittedSegments, "lost_segments": stats.LostSegments, "index_rows": stats.IndexRows,
		"mismatched_bodies": converter.Mismatched, "incomplete_streams": converter.Incomplete,
		"deleted": deleted.Load(), "kept": kept.Load(), "elapsed_seconds": time.Since(started).Seconds(), "dry_run": *dryRun,
	})
	fmt.Println(string(summary))
	return closeErr
}

type legacyFile struct {
	path    string
	size    int64
	modTime time.Time
}

// quietLegacyFiles lists raw transcript files not written to for quiet,
// oldest first.
func quietLegacyFiles(root string, quiet time.Duration, match string) ([]legacyFile, error) {
	var files []legacyFile
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
		if match != "" && !strings.Contains(path, match) {
			return nil
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < quiet {
			return nil
		}
		files = append(files, legacyFile{path: path, size: info.Size(), modTime: info.ModTime()})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	return files, err
}

type legacyTranscriptSyncOptions struct {
	dir, gcsURI, azureDestination, azureKey string
	interval, timeout, retention            time.Duration
	maxLocalBytes, azureRate                int64
}

// startLegacyTranscriptSync runs the raw-file syncers of the local format.
func startLegacyTranscriptSync(options legacyTranscriptSyncOptions) error {
	gcs := transcript.NewGCSSyncer(transcript.GCSSyncerConfig{
		SourceDir:      options.dir,
		Destination:    options.gcsURI,
		Interval:       options.interval,
		Timeout:        options.timeout,
		LocalRetention: options.retention,
		MaxLocalBytes:  options.maxLocalBytes,
		Logger:         slog.Default(),
	})
	if gcs.Enabled() {
		go gcs.Run(context.Background())
	}
	azure := transcript.NewAzureBlobSyncer(transcript.AzureBlobSyncerConfig{
		MaxBytesPerSecond: options.azureRate,
		SourceDir:         options.dir,
		Destination:       options.azureDestination,
		AccountKey:        options.azureKey,
		SASToken:          strings.TrimSpace(os.Getenv("SUBROUTER_TRANSCRIPT_AZURE_SAS")),
		Interval:          options.interval,
		Timeout:           options.timeout,
		LocalRetention:    options.retention,
		MaxLocalBytes:     options.maxLocalBytes,
		Logger:            slog.Default(),
	})
	if azure.Enabled() {
		slog.Info("transcript azure sync enabled", "destination", azure.Destination(), "interval", options.interval.String())
		go azure.Run(context.Background())
	} else if options.azureDestination != "" {
		// A destination that produces no syncer means the credential is missing
		// or the URL is malformed. Saying so at startup is the difference
		// between a broken pipeline and a silent one.
		slog.Warn("transcript azure sync is configured but not usable",
			"destination", options.azureDestination,
			"fix", "set SUBROUTER_TRANSCRIPT_AZURE_KEY_FILE (or SUBROUTER_TRANSCRIPT_AZURE_SAS) and check the container URL")
	}
	return nil
}

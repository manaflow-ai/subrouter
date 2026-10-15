package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/manaflow-ai/subrouter/internal/buildversion"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/internal/transcript"
)

// serveTranscriptOptions are the serve flags that decide how transcripts are
// recorded.
type serveTranscriptOptions struct {
	dir              string
	format           string
	multiTenant      bool
	host             string
	maxSegmentBytes  int64
	maxSegmentAge    time.Duration
	idleSeal         time.Duration
	maxLocalBytes    int64
	azureDestination string
	azureKey         string
	azureSAS         string
	azureTier        string
	azureRate        int64
	whois            proxy.TokenUsageWhoIs
	convertLegacy    bool
}

// newServeTranscriptRecorder returns the recorder serve installs, and whether
// it writes segments. Segments are the default for a single-tenant server:
// they hold every body losslessly at a fraction of the raw size, name the
// client machine, and upload as tierable block blobs.
func newServeTranscriptRecorder(options serveTranscriptOptions) (*transcript.Recorder, bool, error) {
	if strings.TrimSpace(options.dir) == "" {
		return nil, false, nil
	}
	format := strings.ToLower(strings.TrimSpace(options.format))
	if format == "" {
		format = "segments"
		if options.multiTenant {
			format = "jsonl"
		}
	}
	switch format {
	case "jsonl":
		return transcript.NewRecorder(options.dir), false, nil
	case "segments":
	default:
		return nil, false, fmt.Errorf("--transcript-format must be segments or jsonl, got %q", options.format)
	}
	config := transcript.SegmentConfig{
		Dir:             options.dir,
		Host:            options.host,
		MaxSegmentBytes: options.maxSegmentBytes,
		MaxSegmentAge:   options.maxSegmentAge,
		IdleSeal:        options.idleSeal,
		MaxLocalBytes:   options.maxLocalBytes,
		Logger:          slog.Default(),
		Version:         buildversion.Get().String(),
	}
	if options.whois != nil {
		resolver := options.whois
		config.WhoIs = func(ctx context.Context, remoteAddr string) (string, string, bool) {
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
	if options.azureDestination != "" {
		client, err := transcript.NewAzureBlobClient(transcript.AzureBlobClientConfig{
			Destination:       options.azureDestination,
			AccountKey:        options.azureKey,
			SASToken:          options.azureSAS,
			AccessTier:        options.azureTier,
			MaxBytesPerSecond: options.azureRate,
		})
		if err != nil {
			// Recording continues into the spool; the operator sees why
			// nothing is uploaded.
			slog.Warn("transcript segment upload is configured but not usable; segments stay in the spool", "destination", options.azureDestination, "error", err)
		} else {
			config.Uploader = client
			slog.Info("transcript segment upload enabled", "destination", client.Destination(), "tier", options.azureTier)
		}
	}
	recorder, err := transcript.NewSegmentRecorder(config)
	if err != nil {
		return nil, false, fmt.Errorf("transcript segments: %w", err)
	}
	if config.Uploader != nil && options.convertLegacy {
		// Raw per-session files left by a jsonl-format worker (an older
		// generation, or this host before segments) are converted, uploaded
		// and only then deleted, oldest first, in bounded passes.
		backlog := config
		backlog.Dir = filepath.Join(options.dir, "segments-backlog")
		backlog.Uploader = config.Uploader
		backlog.DeltaBaseBytes = 128 << 20
		backlog.WhoIs = nil
		go transcript.RunLegacySpoolConversion(context.Background(), transcript.LegacySpoolOptions{
			Dir:      options.dir,
			Quiet:    15 * time.Minute,
			MaxBytes: 16 << 30,
			Delete:   true,
			Segment:  backlog,
			Logger:   slog.Default(),
		}, 2*time.Minute)
	}
	return recorder, true, nil
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

func transcriptAzureClient(destination string) (*transcript.AzureBlobSyncer, error) {
	key, err := secretFromEnvironment("SUBROUTER_TRANSCRIPT_AZURE_KEY", "SUBROUTER_TRANSCRIPT_AZURE_KEY_FILE")
	if err != nil {
		return nil, err
	}
	return transcript.NewAzureBlobClient(transcript.AzureBlobClientConfig{
		Destination:       destination,
		AccountKey:        key,
		SASToken:          strings.TrimSpace(os.Getenv("SUBROUTER_TRANSCRIPT_AZURE_SAS")),
		MaxBytesPerSecond: -1,
	})
}

// transcriptsExport reads segments (local files, or a date range in Azure)
// and writes one JSON line per request with its response, per websocket
// message, and per body that has no request (legacy data). verify writes
// nothing but the stats and fails when any body does not rebuild exactly.
func transcriptsExport(args []string, verifyOnly bool) error {
	flags := flag.NewFlagSet("transcripts export", flag.ContinueOnError)
	azureURL := flags.String("azure", "", "container URL with prefix (https://<account>.blob.core.windows.net/<container>/<prefix>); defaults to SUBROUTER_TRANSCRIPT_AZURE_URL when no files are given")
	from := flags.String("from", "", "first UTC date to read from Azure (YYYY-MM-DD); default today")
	to := flags.String("to", "", "last UTC date to read from Azure (YYYY-MM-DD); default --from")
	host := flags.String("host", "", "only segments recorded by this host")
	blobPrefix := flags.String("blob-prefix", "", "read every segment under this container-relative prefix instead of a date range")
	session := flags.String("session", "", "only records of this session id")
	output := flags.String("out", "-", "output file, - for stdout")
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
	exporter := transcript.NewSegmentExporter(func(record transcript.ExportRecord) error {
		return encoder.Encode(record)
	})
	if *session != "" {
		wanted := *session
		exporter.Filter = func(record transcript.ExportRecord) bool {
			return record.SessionID == wanted || transcript.BaseSessionID(record.SessionID) == wanted
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	files := flags.Args()
	if len(files) > 0 {
		sort.Strings(files)
		for _, path := range files {
			if err := exportLocalSegment(exporter, path); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		}
	} else {
		destination := strings.TrimSpace(*azureURL)
		if destination == "" {
			destination = strings.TrimSpace(os.Getenv("SUBROUTER_TRANSCRIPT_AZURE_URL"))
		}
		if destination == "" {
			return errors.New("give segment files or --azure")
		}
		client, err := transcriptAzureClient(destination)
		if err != nil {
			return err
		}
		prefixes, err := exportPrefixes(client.Prefix(), *blobPrefix, *from, *to)
		if err != nil {
			return err
		}
		var names []string
		for _, prefix := range prefixes {
			err := client.ListBlobs(ctx, prefix, func(entry transcript.AzureBlobEntry) error {
				if !strings.HasSuffix(entry.Name, ".jsonl.zst") {
					return nil
				}
				if *host != "" && !strings.Contains(entry.Name, "/"+*host+"/") {
					return nil
				}
				names = append(names, entry.Name)
				return nil
			})
			if err != nil {
				return err
			}
		}
		sort.Slice(names, func(i, j int) bool { return filepath.Base(names[i]) < filepath.Base(names[j]) })
		for _, name := range names {
			if err := exportAzureSegment(ctx, exporter, client, name); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	exporter.Finish()
	if err := buffered.Flush(); err != nil {
		return err
	}
	stats, _ := json.Marshal(exporter.Stats)
	fmt.Fprintln(os.Stderr, string(stats))
	if exporter.Stats.Failed() {
		return fmt.Errorf("%d bodies did not match their sha256 and %d could not be decoded", exporter.Stats.Mismatched, exporter.Stats.DecodeErrors)
	}
	return nil
}

func exportPrefixes(clientPrefix, blobPrefix, from, to string) ([]string, error) {
	if blobPrefix != "" {
		return []string{blobPrefix}, nil
	}
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
	base := "segments/v1/"
	if clientPrefix != "" {
		base = clientPrefix + "/" + base
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

func exportAzureSegment(ctx context.Context, exporter *transcript.SegmentExporter, client *transcript.AzureBlobSyncer, name string) error {
	body, _, err := client.OpenBlob(ctx, name)
	if err != nil {
		return err
	}
	defer body.Close()
	decoder, err := transcript.NewSegmentDecoder(body)
	if err != nil {
		return err
	}
	defer decoder.Close()
	return exporter.ReadSegment(filepath.Base(name), decoder)
}

// transcriptsRecompress converts raw per-session transcript blobs (the
// append blobs the old syncer wrote) into compressed segment block blobs.
// It never deletes or modifies a source blob. Each converted source gets a
// marker blob, so a rerun skips it.
func transcriptsRecompress(args []string) error {
	flags := flag.NewFlagSet("transcripts recompress", flag.ContinueOnError)
	sourceURL := flags.String("source", "", "container URL with the raw transcript prefix, e.g. https://<account>.blob.core.windows.net/transcripts/cmux-lawrence/by-agent")
	destURL := flags.String("dest", "", "container URL with prefix for the segments, e.g. https://<account>.blob.core.windows.net/transcripts/cmux-lawrence; default: the source container and the first path element of the source prefix")
	spool := flags.String("spool", "", "local working directory for segments before upload (required)")
	maxBlobs := flags.Int("max-blobs", 1, "convert at most this many source blobs; 0 means no limit")
	maxBytes := flags.String("max-bytes", "1GiB", "stop after reading this many source bytes; 0 means no limit")
	match := flags.String("match", "", "only source blobs whose name contains this text")
	dryRun := flags.Bool("dry-run", false, "write segments locally and do not upload or mark anything")
	level := flags.Int("level", 4, "zstd level 1-4 (4 is the best ratio)")
	segmentBytes := flags.String("segment-bytes", "256MiB", "roll output segments at this raw size")
	localDir := flags.String("local-dir", "", "convert a local raw transcript spool (the directory holding by-agent/) instead of Azure blobs")
	deleteLocal := flags.Bool("delete", false, "with --local-dir: delete each source file after the segment holding it is uploaded and verified")
	quiet := flags.Duration("quiet", 15*time.Minute, "with --local-dir: skip files written to more recently than this")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *localDir != "" {
		return recompressLocalSpool(*localDir, *spool, *dryRun, *deleteLocal, *quiet, *maxBytes, *destURL, *level)
	}
	if *sourceURL == "" || *spool == "" {
		return errors.New("--source and --spool are required")
	}
	byteLimit, err := parseByteSize(*maxBytes)
	if err != nil {
		return err
	}
	rollBytes, err := parseByteSize(*segmentBytes)
	if err != nil {
		return err
	}
	source, err := transcriptAzureClient(*sourceURL)
	if err != nil {
		return err
	}
	destination := strings.TrimSpace(*destURL)
	if destination == "" {
		first := strings.SplitN(source.Prefix(), "/", 2)[0]
		destination = strings.TrimSuffix(strings.TrimSuffix(*sourceURL, "/"), strings.TrimPrefix(source.Prefix(), first))
	}
	dest, err := transcriptAzureClient(destination)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	markerPrefix := "segments/v1/legacy/converted/"
	config := transcript.SegmentConfig{
		Dir:             *spool,
		Host:            "legacy",
		MaxSegmentBytes: rollBytes,
		CompressLevel:   zstd.EncoderLevelFromZstd(map[int]int{1: 1, 2: 3, 3: 7, 4: 19}[*level]),
		BlobPrefix:      "segments/v1/legacy/" + time.Now().UTC().Format("2006/01/02"),
		Logger:          slog.Default(),
	}
	if !*dryRun {
		config.Uploader = dest
	}
	converter, err := transcript.NewLegacyConverter(ctx, config)
	if err != nil {
		return err
	}
	var sourceSizes = map[string]int64{}
	converter.OnSealed = func(info transcript.SegmentInfo, completed []string) error {
		fmt.Fprintf(os.Stderr, "segment %s raw=%d compressed=%d sources_completed=%d\n", info.SegmentID, info.RawBytes, info.CompressedBytes, len(completed))
		if *dryRun {
			return nil
		}
		for _, name := range completed {
			marker, _ := json.Marshal(map[string]any{"source": name, "source_bytes": sourceSizes[name], "completed_in": transcript.SegmentBlobName(info), "at": time.Now().UTC().Format(time.RFC3339)})
			if err := dest.PutSmallBlob(ctx, markerName(dest.Prefix(), markerPrefix, name), marker, "application/json"); err != nil {
				return err
			}
		}
		return nil
	}

	var names []transcript.AzureBlobEntry
	err = source.ListBlobs(ctx, source.Prefix()+"/", func(entry transcript.AzureBlobEntry) error {
		if !strings.HasSuffix(entry.Name, ".jsonl") || (*match != "" && !strings.Contains(entry.Name, *match)) {
			return nil
		}
		names = append(names, entry)
		return nil
	})
	if err != nil {
		return err
	}
	var readBytes int64
	converted := 0
	started := time.Now()
	for _, entry := range names {
		if *maxBlobs > 0 && converted >= *maxBlobs {
			break
		}
		if byteLimit > 0 && readBytes >= byteLimit {
			break
		}
		if !*dryRun {
			if exists, err := dest.BlobExists(ctx, markerName(dest.Prefix(), markerPrefix, entry.Name)); err != nil {
				return err
			} else if exists {
				continue
			}
		}
		body, _, err := source.OpenBlob(ctx, entry.Name)
		if err != nil {
			return err
		}
		sourceSizes[entry.Name] = entry.Size
		err = converter.Convert(entry.Name, body)
		_ = body.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", entry.Name, err)
		}
		readBytes += entry.Size
		converted++
		fmt.Fprintf(os.Stderr, "converted %s (%d bytes)\n", entry.Name, entry.Size)
	}
	if err := converter.Close(); err != nil {
		return err
	}
	var compressed, raw int64
	for _, info := range converter.Segments() {
		compressed += info.CompressedBytes
		raw += info.RawBytes
	}
	summary, _ := json.Marshal(map[string]any{
		"sources": converter.Sources, "source_bytes": converter.SourceBytes, "segment_raw_bytes": raw,
		"compressed_bytes": compressed, "segments": len(converter.Segments()),
		"ratio_source_to_compressed": ratio(converter.SourceBytes, compressed),
		"mismatched_bodies":          converter.Mismatched, "incomplete_streams": converter.Incomplete,
		"elapsed_seconds": time.Since(started).Seconds(), "uploaded": !*dryRun,
	})
	fmt.Println(string(summary))
	return nil
}

func markerName(clientPrefix, markerPrefix, source string) string {
	name := markerPrefix + strings.ReplaceAll(source, "/", "__") + ".json"
	if clientPrefix != "" {
		return clientPrefix + "/" + name
	}
	return name
}

func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

type legacyTranscriptSyncOptions struct {
	dir, gcsURI, azureDestination, azureKey string
	interval, timeout, retention            time.Duration
	maxLocalBytes, azureRate                int64
}

// startLegacyTranscriptSync runs the raw-file syncers of the jsonl format.
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

func recompressLocalSpool(dir, spool string, dryRun, deleteSources bool, quiet time.Duration, maxBytesRaw, destination string, level int) error {
	if spool == "" {
		return errors.New("--spool is required")
	}
	maxBytes, err := parseByteSize(maxBytesRaw)
	if err != nil {
		return err
	}
	if deleteSources && dryRun {
		return errors.New("--delete cannot be combined with --dry-run")
	}
	config := transcript.SegmentConfig{
		Dir:           spool,
		CompressLevel: zstd.EncoderLevelFromZstd(map[int]int{1: 1, 2: 3, 3: 7, 4: 19}[level]),
		Logger:        slog.Default(),
	}
	if !dryRun {
		if destination == "" {
			destination = strings.TrimSpace(os.Getenv("SUBROUTER_TRANSCRIPT_AZURE_URL"))
		}
		if destination == "" {
			return errors.New("--dest (or SUBROUTER_TRANSCRIPT_AZURE_URL) is required unless --dry-run")
		}
		client, err := transcriptAzureClient(destination)
		if err != nil {
			return err
		}
		config.Uploader = client
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	started := time.Now()
	result, err := transcript.ConvertLegacySpool(ctx, transcript.LegacySpoolOptions{
		Dir: dir, Quiet: quiet, MaxBytes: maxBytes, Delete: deleteSources, Segment: config, KeepWork: dryRun,
	})
	summary, _ := json.Marshal(map[string]any{"result": result, "ratio_source_to_compressed": ratio(result.SourceBytes, result.CompressedBytes), "elapsed_seconds": time.Since(started).Seconds()})
	fmt.Println(string(summary))
	return err
}

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
	"github.com/manaflow-ai/subrouter/internal/session"
	"github.com/manaflow-ai/subrouter/internal/storepath"
	"github.com/manaflow-ai/subrouter/internal/transcript"
)

func main() {
	program := filepath.Base(os.Args[0])
	configureDefaultLogger(program, os.Args[1:])
	if program == "cx" {
		if err := cxAlias(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "cx:", err)
			os.Exit(1)
		}
		return
	}
	if err := runForProgram(program, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "subrouter:", err)
		os.Exit(1)
	}
}

func configureDefaultLogger(program string, args []string) {
	if shouldUseProcessLogger(program, args) {
		return
	}
	path := filepath.Join(storepath.StateDir(), "logs", "subrouter-cli.log")
	handler := newCLIFileLogHandler(path)
	slog.SetDefault(slog.New(handler))
}

func shouldUseProcessLogger(_ string, args []string) bool {
	return len(args) > 0 && args[0] == "serve"
}

func newCLIFileLogHandler(path string) slog.Handler {
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return slog.NewTextHandler(io.Discard, opts)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return slog.NewTextHandler(io.Discard, opts)
	}
	return slog.NewTextHandler(file, opts)
}

func run(args []string) error {
	return runForProgram("subrouter", args)
}

func runForProgram(program string, args []string) error {
	if len(args) == 0 {
		if program == "sr" {
			return srForProgram(program, nil)
		}
		usage(program)
		return nil
	}

	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "accounts":
		return listAccounts()
	case "codex":
		return codex(args[1:])
	case "cx":
		return cxAlias(args[1:])
	case "install-daemon":
		return installDaemon(args[1:])
	case "install-systemd":
		return installSystemd(args[1:])
	case "help", "-h", "--help":
		usage(program)
		return nil
	default:
		if isDirectSRCommand(args[0]) || strings.Contains(args[0], "@") {
			return srForProgram(program, args)
		}
		return fmt.Errorf("unknown command %q", args[0])
	}
}

var directSRCommands = map[string]struct{}{
	"add":              {},
	"add-admin-key":    {},
	"add-api-key":      {},
	"add-key":          {},
	"admin-keys":       {},
	"attach-project":   {},
	"breadcrumbs":      {},
	"claude":           {},
	"claude-aws":       {},
	"claude-direct":    {},
	"cost":             {},
	"g":                {},
	"gemini":           {},
	"gui":              {},
	"gui-switch":       {},
	"gui-use":          {},
	"import":           {},
	"list":             {},
	"list-admin-keys":  {},
	"login":            {},
	"ls":               {},
	"pick":             {},
	"remove":           {},
	"remove-admin-key": {},
	"reset":            {},
	"rm":               {},
	"server":           {},
	"servers":          {},
	"spend":            {},
	"status":           {},
	"switch":           {},
	"trace":            {},
	"usage":            {},
	"use":              {},
	"why":              {},
}

func isDirectSRCommand(command string) bool {
	_, ok := directSRCommands[command]
	return ok
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", "127.0.0.1:31415", "listen address")
	upstreamRaw := flags.String("upstream", "", "force one upstream base URL for all accounts")
	codexUpstreamRaw := flags.String("codex-upstream", "https://chatgpt.com/backend-api/codex", "Codex subscription upstream base URL")
	apiUpstreamRaw := flags.String("api-upstream", "https://api.openai.com", "OpenAI API-key upstream base URL")
	claudeUpstreamRaw := flags.String("claude-upstream", "https://api.anthropic.com", "Claude subscription upstream base URL")
	kimiUpstreamRaw := flags.String("kimi-upstream", "https://api.kimi.com/coding/v1", "Kimi For Coding upstream base URL")
	zaiUpstreamRaw := flags.String("zai-upstream", "https://api.z.ai/api/coding/paas/v4", "Z.AI coding upstream base URL")
	sessionPath := flags.String("sessions", session.DefaultStorePath(), "session assignment store")
	transcriptDir := flags.String("transcripts", "", "directory for raw Subrouter transcript JSONL files")
	transcriptGCSURI := flags.String("transcript-gcs-uri", "", "optional gs:// bucket/prefix for background transcript sync")
	transcriptGCSSyncInterval := flags.Duration("transcript-gcs-sync-interval", 5*time.Minute, "interval for background transcript GCS sync; 0 disables")
	transcriptGCSSyncTimeout := flags.Duration("transcript-gcs-sync-timeout", 30*time.Minute, "timeout for each transcript GCS sync command")
	transcriptLocalRetention := flags.Duration("transcript-local-retention", 0, "delete local transcript files older than this after successful GCS sync; 0 disables")
	transcriptMaxLocalBytesRaw := flags.String("transcript-max-local-bytes", "0", "max bytes to keep in the local transcript spool after successful GCS sync; supports KiB/MiB/GiB suffixes; 0 disables")
	srSwitchInterval := defaultSRSwitchInterval
	flags.DurationVar(&srSwitchInterval, "sr-switch-interval", defaultSRSwitchInterval, "interval for switching the active sr account to the best OAuth account; 0 disables")
	flags.DurationVar(&srSwitchInterval, "cx-switch-interval", defaultSRSwitchInterval, "compatibility alias for --sr-switch-interval")
	usageScoreTTL := flags.Duration("usage-score-ttl", 30*time.Second, "maximum age for usage scores before account selection refreshes them; 0 disables")
	shutdownTimeout := flags.Duration("shutdown-timeout", 10*time.Minute, "maximum time to drain in-flight proxy requests after SIGTERM/SIGINT")
	adminToken := flags.String("admin-token", "", "admin token required for non-loopback _subrouter endpoints; defaults to SUBROUTER_ADMIN_TOKEN")
	maxBodyBytes := flags.Int64("max-body-bytes", 1<<20, "max JSON request body bytes to inspect for session IDs")
	fetchUsage := flags.Bool("fetch-usage", true, "fetch Codex usage on startup for account selection")
	bedrockEnable := flags.Bool("bedrock", false, "enable the /bedrock/* AWS SigV4 signing gateway for Claude Code Bedrock mode")
	bedrockRegion := flags.String("bedrock-region", "us-east-1", "comma-separated AWS regions for the Bedrock signing gateway")
	bedrockGatewayToken := flags.String("bedrock-gateway-token", "", "optional bearer token clients must present to the Bedrock gateway; defaults to SUBROUTER_BEDROCK_GATEWAY_TOKEN")
	bedrockProfiles := flags.String("bedrock-profiles", "", "comma-separated AWS profiles for the Bedrock gateway; defaults to SUBROUTER_BEDROCK_PROFILES or discovered awN profiles")
	bedrockAutoBump := flags.Bool("bedrock-autobump", false, "request a Service Quotas increase (2x, deduped) when Bedrock throttles Fable/Opus")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*transcriptGCSURI) != "" && strings.TrimSpace(*transcriptDir) == "" {
		return errors.New("--transcripts is required when --transcript-gcs-uri is set")
	}
	transcriptMaxLocalBytes, err := parseByteSize(*transcriptMaxLocalBytesRaw)
	if err != nil {
		return fmt.Errorf("transcript-max-local-bytes: %w", err)
	}
	if *adminToken == "" {
		*adminToken = strings.TrimSpace(os.Getenv("SUBROUTER_ADMIN_TOKEN"))
	}

	var upstream *url.URL
	if *upstreamRaw != "" {
		var err error
		upstream, err = url.Parse(*upstreamRaw)
		if err != nil {
			return err
		}
	}
	codexUpstream, err := url.Parse(*codexUpstreamRaw)
	if err != nil {
		return err
	}
	apiUpstream, err := url.Parse(*apiUpstreamRaw)
	if err != nil {
		return err
	}
	claudeUpstream, err := url.Parse(*claudeUpstreamRaw)
	if err != nil {
		return err
	}
	kimiUpstream, err := url.Parse(*kimiUpstreamRaw)
	if err != nil {
		return err
	}
	zaiUpstream, err := url.Parse(*zaiUpstreamRaw)
	if err != nil {
		return err
	}

	store, err := session.NewStore(*sessionPath)
	if err != nil {
		return err
	}

	codexStore := accounts.DefaultCodexStore()
	codexAccounts, err := codexStore.List()
	if err != nil {
		return err
	}
	claudeAccounts, err := agentclaude.DefaultStore().ListAccounts(context.Background())
	if err != nil {
		slog.Warn("Claude accounts skipped", "error", err)
		claudeAccounts = nil
	}
	// Start with optimistic fallback scores so the proxy begins accepting
	// connections immediately. Blocking startup on a synchronous usage fetch
	// (an OAuth refresh per account) stalls socket-activated connections during
	// a deploy restart, so the real scores are fetched in the background and
	// swapped in once ready. Per-request 401/429 failover covers the brief
	// window before fresh scores land.
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(fallbackScores(codexAccounts)))
	if *fetchUsage {
		go func() {
			fetchedScores, successful := fetchCodexScoresWithStore(context.Background(), codexStore, codexAccounts)
			if successful > 0 {
				schedulerRef.Set(selectacct.NewScheduler(fetchedScores))
			} else {
				slog.Warn("initial usage score fetch skipped", "reason", "no fresh OAuth usage scores")
			}
		}()
	}
	outboundTransport := proxy.NewOutboundTransport()

	var bedrockConfig *proxy.BedrockConfig
	if *bedrockEnable {
		token := strings.TrimSpace(*bedrockGatewayToken)
		if token == "" {
			token = strings.TrimSpace(os.Getenv("SUBROUTER_BEDROCK_GATEWAY_TOKEN"))
		}
		regions := parseBedrockRegions(*bedrockRegion)
		if len(regions) == 0 {
			return errors.New("bedrock: no AWS regions configured")
		}
		profileNames := bedrockAWSProfileNames(*bedrockProfiles)
		awsCfg, sources, err := loadBedrockAWSSources(context.Background(), regions[0], profileNames, *bedrockAutoBump)
		if err != nil {
			return fmt.Errorf("bedrock: load AWS config: %w", err)
		}
		if len(sources) == 0 {
			return errors.New("bedrock: no AWS credentials available")
		}
		bedrockConfig = &proxy.BedrockConfig{
			Regions:      regions,
			Sources:      sources,
			GatewayToken: token,
			Transport:    outboundTransport,
			CostLogPath:  filepath.Join(filepath.Dir(*sessionPath), "bedrock-cost.jsonl"),
		}
		if *bedrockAutoBump {
			bedrockConfig.Bumper = proxy.NewBedrockQuotaBumper(awsCfg, slog.Default())
		}
		slog.Info("bedrock gateway enabled", "regions", strings.Join(regions, ","), "auth", token != "", "autobump", *bedrockAutoBump, "profiles", strings.Join(bedrockSourceNames(sources), ","))
	}

	initialAccounts := append([]accounts.Account(nil), codexAccounts...)
	initialAccounts = append(initialAccounts, claudeAccounts...)
	accountRef := proxy.NewAccountRef(codexStore, initialAccounts, &http.Client{
		Timeout:   15 * time.Second,
		Transport: outboundTransport,
	})

	server := proxy.Server{
		Upstream:          upstream,
		CodexUpstream:     codexUpstream,
		APIUpstream:       apiUpstream,
		ClaudeUpstream:    claudeUpstream,
		KimiUpstream:      kimiUpstream,
		ZAIUpstream:       zaiUpstream,
		Accounts:          nil,
		AccountRef:        accountRef,
		Sessions:          store,
		SchedulerRef:      schedulerRef,
		UsageScoreTTL:     usageScoreTTLForServe(*fetchUsage, *usageScoreTTL),
		Transport:         outboundTransport,
		Logger:            slog.Default(),
		Lifecycle:         proxy.NewLifecycle(),
		AdminToken:        *adminToken,
		MaxBodyBytes:      *maxBodyBytes,
		Bedrock:           bedrockConfig,
		ClaudeFableAPIKey: strings.TrimSpace(os.Getenv("SUBROUTER_CLAUDE_FABLE_API_KEY")),
		Transcripts:       transcript.NewRecorder(*transcriptDir),
	}
	transcriptGCSSyncer := transcript.NewGCSSyncer(transcript.GCSSyncerConfig{
		SourceDir:      *transcriptDir,
		Destination:    *transcriptGCSURI,
		Interval:       *transcriptGCSSyncInterval,
		Timeout:        *transcriptGCSSyncTimeout,
		LocalRetention: *transcriptLocalRetention,
		MaxLocalBytes:  transcriptMaxLocalBytes,
		Logger:         slog.Default(),
	})
	if transcriptGCSSyncer.Enabled() {
		go transcriptGCSSyncer.Run(context.Background())
	}
	if srSwitchInterval > 0 && *fetchUsage {
		go runSRAutoSwitch(context.Background(), srAutoSwitchConfig{
			Interval:     srSwitchInterval,
			AccountsFunc: accountRef.All,
			Sessions:     store,
			SchedulerRef: schedulerRef,
			Logger:       slog.Default(),
		})
	} else if srSwitchInterval > 0 {
		slog.Info("sr auto-switch disabled because usage fetching is disabled", "interval", srSwitchInterval.String())
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if upstream != nil {
		slog.Info("subrouter listening", "addr", *addr, "upstream", upstream.String(), "codex_accounts", len(codexAccounts), "claude_accounts", len(claudeAccounts), "transcripts", *transcriptDir, "transcript_gcs_uri", *transcriptGCSURI)
	} else {
		slog.Info("subrouter listening", "addr", *addr, "codex_upstream", codexUpstream.String(), "api_upstream", apiUpstream.String(), "claude_upstream", claudeUpstream.String(), "codex_accounts", len(codexAccounts), "claude_accounts", len(claudeAccounts), "transcripts", *transcriptDir, "transcript_gcs_uri", *transcriptGCSURI)
	}
	return listenAndServeWithSignals(httpServer, server.Lifecycle, *shutdownTimeout, slog.Default())
}

func loadBedrockAWSSources(ctx context.Context, region string, profiles []string, autobump bool) (aws.Config, []proxy.BedrockCredentialSource, error) {
	if len(profiles) == 0 {
		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
		if err != nil {
			return aws.Config{}, nil, err
		}
		if cfg.Credentials == nil {
			return cfg, nil, nil
		}
		source := proxy.BedrockCredentialSource{
			Name:        "default",
			Credentials: aws.NewCredentialsCache(cfg.Credentials),
		}
		if autobump {
			source.Bumper = proxy.NewBedrockQuotaBumper(cfg, slog.Default())
		}
		return cfg, []proxy.BedrockCredentialSource{source}, nil
	}

	var first aws.Config
	sources := make([]proxy.BedrockCredentialSource, 0, len(profiles))
	for _, profile := range profiles {
		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region), awsconfig.WithSharedConfigProfile(profile))
		if err != nil {
			return aws.Config{}, nil, fmt.Errorf("profile %s: %w", profile, err)
		}
		if first.Region == "" {
			first = cfg
		}
		if cfg.Credentials == nil {
			return aws.Config{}, nil, fmt.Errorf("profile %s: no AWS credentials available", profile)
		}
		source := proxy.BedrockCredentialSource{
			Name:        profile,
			Credentials: aws.NewCredentialsCache(cfg.Credentials),
		}
		if autobump {
			source.Bumper = proxy.NewBedrockQuotaBumper(cfg, slog.Default())
		}
		sources = append(sources, source)
	}
	return first, sources, nil
}

func bedrockSourceNames(sources []proxy.BedrockCredentialSource) []string {
	names := make([]string, 0, len(sources))
	for _, source := range sources {
		names = append(names, source.Name)
	}
	return names
}

func bedrockAWSProfileNames(flagValue string) []string {
	if profiles := splitProfileList(flagValue); len(profiles) > 0 {
		return profiles
	}
	if profiles := splitProfileList(os.Getenv("SUBROUTER_BEDROCK_PROFILES")); len(profiles) > 0 {
		return profiles
	}
	return discoverBedrockAWSProfiles(awsSharedConfigPaths())
}

func parseBedrockRegions(raw string) []string {
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		region := strings.TrimSpace(part)
		if region == "" || seen[region] {
			continue
		}
		seen[region] = true
		out = append(out, region)
	}
	return out
}

func splitProfileList(raw string) []string {
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	}) {
		name := strings.TrimSpace(part)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func awsSharedConfigPaths() []string {
	var paths []string
	if path := strings.TrimSpace(os.Getenv("AWS_CONFIG_FILE")); path != "" {
		paths = append(paths, path)
	}
	if path := strings.TrimSpace(os.Getenv("AWS_SHARED_CREDENTIALS_FILE")); path != "" {
		paths = append(paths, path)
	}
	if len(paths) > 0 {
		return paths
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".aws", "config"),
		filepath.Join(home, ".aws", "credentials"),
	}
}

func discoverBedrockAWSProfiles(paths []string) []string {
	seen := map[string]bool{}
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			name, ok := awsProfileSectionName(scanner.Text())
			if ok && isBedrockAWSProfileName(name) {
				seen[name] = true
			}
		}
		_ = file.Close()
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool {
		ai, aok := numericAWSProfileSuffix(out[i])
		aj, jok := numericAWSProfileSuffix(out[j])
		if aok && jok {
			return ai < aj
		}
		if aok != jok {
			return aok
		}
		return out[i] < out[j]
	})
	return out
}

func awsProfileSectionName(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]"))
	name = strings.TrimPrefix(name, "profile ")
	name = strings.TrimSpace(name)
	return name, name != ""
}

func isBedrockAWSProfileName(name string) bool {
	_, ok := numericAWSProfileSuffix(name)
	return ok
}

func numericAWSProfileSuffix(name string) (int, bool) {
	if !strings.HasPrefix(name, "aw") || len(name) == 2 {
		return 0, false
	}
	n, err := strconv.Atoi(name[2:])
	return n, err == nil
}

func listenAndServeWithSignals(server *http.Server, lifecycle *proxy.Lifecycle, shutdownTimeout time.Duration, logger *slog.Logger) error {
	errCh := make(chan error, 1)
	go func() {
		err := listenAndServeHTTP(server, logger)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		if lifecycle != nil {
			lifecycle.Drain()
		}
		if logger != nil {
			logger.Info("subrouter shutdown signal received", "signal", sig.String(), "timeout", shutdownTimeout.String())
		}
		if shutdownTimeout < 0 {
			shutdownTimeout = 0
		}
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			if logger != nil {
				logger.Error("subrouter graceful shutdown failed", "error", err)
			}
			_ = server.Close()
			return err
		}
		return <-errCh
	}
}

func listenAndServeHTTP(server *http.Server, logger *slog.Logger) error {
	listeners, err := inheritedSystemdListeners()
	if err != nil {
		return err
	}
	if len(listeners) == 0 {
		return server.ListenAndServe()
	}
	if logger != nil {
		logger.Info("using inherited systemd socket", "listeners", len(listeners), "addr", listeners[0].Addr().String())
	}
	return server.Serve(listeners[0])
}

func inheritedSystemdListeners() ([]net.Listener, error) {
	pid, fdCount, ok, err := systemdListenFDs(os.Getpid(), os.Getenv)
	if err != nil || !ok {
		return nil, err
	}
	unsetSystemdListenEnv()
	if pid != os.Getpid() {
		return nil, nil
	}
	listeners := make([]net.Listener, 0, fdCount)
	var errs []error
	for i := 0; i < fdCount; i++ {
		fd := uintptr(3 + i)
		file := os.NewFile(fd, fmt.Sprintf("systemd-listen-fd-%d", i))
		if file == nil {
			errs = append(errs, fmt.Errorf("fd %d is unavailable", fd))
			continue
		}
		listener, listenErr := net.FileListener(file)
		closeErr := file.Close()
		if listenErr != nil {
			errs = append(errs, fmt.Errorf("fd %d: %w", fd, listenErr))
			continue
		}
		if closeErr != nil {
			_ = listener.Close()
			errs = append(errs, fmt.Errorf("fd %d: %w", fd, closeErr))
			continue
		}
		listeners = append(listeners, listener)
	}
	if len(listeners) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return listeners, nil
}

func systemdListenFDs(currentPID int, getenv func(string) string) (int, int, bool, error) {
	rawFDs := strings.TrimSpace(getenv("LISTEN_FDS"))
	if rawFDs == "" {
		return 0, 0, false, nil
	}
	fdCount, err := strconv.Atoi(rawFDs)
	if err != nil || fdCount < 0 {
		return 0, 0, true, fmt.Errorf("invalid LISTEN_FDS %q", rawFDs)
	}
	rawPID := strings.TrimSpace(getenv("LISTEN_PID"))
	pid, err := strconv.Atoi(rawPID)
	if err != nil {
		return 0, 0, true, fmt.Errorf("invalid LISTEN_PID %q", rawPID)
	}
	if pid != currentPID {
		return pid, fdCount, true, nil
	}
	return pid, fdCount, true, nil
}

func unsetSystemdListenEnv() {
	_ = os.Unsetenv("LISTEN_PID")
	_ = os.Unsetenv("LISTEN_FDS")
	_ = os.Unsetenv("LISTEN_FDNAMES")
}

func usageScoreTTLForServe(fetchUsage bool, ttl time.Duration) time.Duration {
	if !fetchUsage {
		return 0
	}
	return ttl
}

func parseByteSize(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "0" {
		return 0, nil
	}
	lower := strings.ToLower(trimmed)
	units := []struct {
		suffix string
		scale  int64
	}{
		{"gib", 1024 * 1024 * 1024},
		{"gb", 1000 * 1000 * 1000},
		{"g", 1024 * 1024 * 1024},
		{"mib", 1024 * 1024},
		{"mb", 1000 * 1000},
		{"m", 1024 * 1024},
		{"kib", 1024},
		{"kb", 1000},
		{"k", 1024},
		{"b", 1},
	}
	scale := int64(1)
	number := trimmed
	for _, unit := range units {
		if strings.HasSuffix(lower, unit.suffix) {
			scale = unit.scale
			number = strings.TrimSpace(trimmed[:len(trimmed)-len(unit.suffix)])
			break
		}
	}
	parsed, err := strconv.ParseFloat(number, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte size %q", value)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("byte size must be non-negative")
	}
	return int64(parsed * float64(scale)), nil
}

func fetchCodexScores(ctx context.Context, codexAccounts []accounts.Account) []selectacct.Score {
	scores, _ := fetchCodexScoresWithSuccess(ctx, codexAccounts)
	return scores
}

func fetchCodexScoresWithSuccess(ctx context.Context, codexAccounts []accounts.Account) ([]selectacct.Score, int) {
	return fetchCodexScoresWithStore(ctx, accounts.DefaultCodexStore(), codexAccounts)
}

func fetchCodexScoresWithStore(ctx context.Context, store accounts.CodexStore, codexAccounts []accounts.Account) ([]selectacct.Score, int) {
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: proxy.NewOutboundTransport(),
	}
	scores := fallbackScores(codexAccounts)
	scoreByID := make(map[string]int, len(scores))
	for i, score := range scores {
		scoreByID[selectacct.ScoreKey(score.Provider, score.AccountID)] = i
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	successful := 0
	for _, account := range codexAccounts {
		if account.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		wg.Add(1)
		go func(account accounts.Account) {
			defer wg.Done()
			stored, ok, err := store.FindStored(account.ID)
			if err != nil || !ok {
				slog.Warn("account refresh lookup failed", "account", account.ID, "error", err)
			} else if refreshed, _, err := store.RefreshStoredIfExpired(accounts.WithCodexRefreshReason(ctx, "serve.fetch-usage"), client, stored); err != nil {
				slog.Warn("account refresh failed", "account", account.ID, "error", err)
				mu.Lock()
				if idx, ok := scoreByID[selectacct.ScoreKey(account.Provider, account.ID)]; ok {
					scores[idx] = selectacct.Score{AccountID: account.ID, Provider: account.Provider, Headroom: 0, ShortHeadroom: 0}
				}
				mu.Unlock()
				return
			} else if refreshedAccount, ok := refreshed.Account(refreshed.SourcePath(store)); ok {
				account = refreshedAccount
			}
			windows, err := accounts.FetchCodexUsage(ctx, client, account)
			if err != nil {
				slog.Warn("usage fetch failed", "account", account.ID, "error", err)
				mu.Lock()
				if idx, ok := scoreByID[selectacct.ScoreKey(account.Provider, account.ID)]; ok {
					scores[idx] = selectacct.Score{AccountID: account.ID, Provider: account.Provider, Headroom: 0}
				}
				mu.Unlock()
				return
			}
			limitWindows := make([]selectacct.LimitWindow, 0, len(windows))
			for _, window := range windows {
				limitWindows = append(limitWindows, selectacct.LimitWindow{
					Name:               window.Name,
					UsedPercent:        window.UsedPercent,
					LimitWindowSeconds: window.LimitWindowSeconds,
					ResetAfterSeconds:  window.ResetAfterSeconds,
					Feature:            window.Feature,
				})
			}
			score := selectacct.ScoreFromLimitWindows(account.ID, 0, limitWindows)
			score.Provider = account.Provider
			mu.Lock()
			if idx, ok := scoreByID[selectacct.ScoreKey(account.Provider, account.ID)]; ok {
				scores[idx] = score
				successful++
			}
			mu.Unlock()
		}(account)
	}
	wg.Wait()

	for _, score := range scores {
		slog.Info("account score", "account", score.AccountID, "headroom", score.Headroom)
	}
	return scores, successful
}

func fallbackScores(codexAccounts []accounts.Account) []selectacct.Score {
	scores := make([]selectacct.Score, 0, len(codexAccounts))
	for _, account := range codexAccounts {
		headroom := 1.0
		if account.AuthMode == accounts.AuthModeAPIKey {
			headroom = 0.01
		}
		scores = append(scores, selectacct.Score{AccountID: account.ID, Provider: account.Provider, Headroom: headroom, ShortHeadroom: headroom})
	}
	return scores
}

func listAccounts() error {
	codexAccounts, err := accounts.DefaultCodexStore().List()
	if err != nil {
		return err
	}
	if len(codexAccounts) == 0 {
		fmt.Println("No Codex accounts found. Run: subrouter add")
		return nil
	}
	for _, account := range codexAccounts {
		fmt.Printf("%s\t%s\t%s\n", account.ID, account.Provider, account.AuthMode)
	}
	return nil
}

func usage(program string) {
	fmt.Print(usageText(program))
}

func usageText(program string) string {
	if program == "" {
		program = "subrouter"
	}
	return fmt.Sprintf(`%[1]s routes AI coding-agent traffic across subscription accounts.

Usage:
  %[1]s                    Show Codex and Claude usage, grouped by provider
  %[1]s add                Add a new Codex account (opens OAuth login)
  %[1]s add-key            Add an API key account
  %[1]s import             Import current ~/.codex/auth.json account
  %[1]s list               List all Codex accounts
  %[1]s switch [email]     Switch active Codex account and sync OpenCode/pi
  %[1]s g [email]          Switch active account, sync OpenCode/pi, and restart Codex.app
  %[1]s gui [email]        Switch active account, sync OpenCode/pi, and restart Codex.app
  %[1]s gui-switch [email] Switch active account, sync OpenCode/pi, and restart Codex.app
  %[1]s remove <email>     Remove a Codex account
  %[1]s status             Show Codex and Claude usage (non-interactive)
  %[1]s pick               Switch to the recommended account, failing if none has quota
  %[1]s reset [email]      Redeem a rate-limit reset credit (best candidate, or --all, or --dry-run)
  %[1]s usage [days]       Refresh and show API-key spend
  %[1]s trace <email>      Show OAuth refresh breadcrumbs for an account

  %[1]s server             Manage Subrouter servers
  %[1]s server add <name> --url <url> [--default]
  %[1]s server use <name|local> [--no-codex-config]
  %[1]s server rename <old> <new>
  %[1]s server install <name>
  %[1]s server login <name> [--device-auth]
  %[1]s server sync <name> [--device-auth] [--yes]

  %[1]s admin-keys         List stored OpenAI admin keys
  %[1]s add-admin-key      Add an sk-admin-* key
  %[1]s remove-admin-key <label>
  %[1]s attach-project <api-key-label> [--project-id <id-or-name>]

  %[1]s claude             Manage Claude Code profiles
  %[1]s claude-aws [--model fable] [claude args...]
                           Launch Claude Code on AWS Bedrock via the server (Fable 5)
  %[1]s claude-direct [claude args...]
                           Launch Claude Code directly on Anthropic (bypass subrouter)
  %[1]s spend              Show AWS Bedrock spend tracked by the server
  %[1]s gemini             Manage Gemini profiles

  %[1]s serve [--addr 127.0.0.1:31415] [--fetch-usage=true] [--codex-upstream URL] [--claude-upstream URL] [--kimi-upstream URL] [--zai-upstream URL] [--transcripts DIR] [--transcript-gcs-uri gs://bucket/prefix] [--transcript-gcs-sync-timeout 30m] [--transcript-local-retention 24h] [--transcript-max-local-bytes 2GiB]
  %[1]s accounts
  %[1]s codex [codex args...]
  %[1]s install-daemon [--start=true]       macOS LaunchAgent
  %[1]s install-systemd [--start=true]      Linux systemd service

Session stickiness:
  Prefer sending X-Subrouter-Session per conversation.
  Send X-Subrouter-Agent when the client is not Codex.
  Send X-Subrouter-User-Email for teammate-level observability.
  Send X-Subrouter-Account-ID to force a specific account, including an API-key account.
  Subrouter switches the active sr account every 10m by default; set --sr-switch-interval=0 to disable.
  For %[1]s codex, set SUBROUTER_CODEX_USER_EMAIL and/or SUBROUTER_CODEX_ACCOUNT_ID instead.
  The proxy also checks common session headers, query params, and small JSON bodies.
`, program)
}

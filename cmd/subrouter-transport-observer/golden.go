package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	goldenProbeInterval = 100 * time.Millisecond
	goldenHTTPTimeout   = 900 * time.Millisecond
)

// goldenTestHooks are set only by same-package deterministic tests. Production
// binaries have no environment or command-line switch that enables them.
var goldenTestHooks struct {
	enabled             bool
	releaseAPI          string
	releaseDownloadRoot string
}

type goldenOptions struct {
	cloudConfig       string
	codexHome         string
	codexBinary       string
	releasedVersion   string
	releasedClient    string
	artifactDir       string
	model             string
	streamLines       int
	timeout           time.Duration
	activation        []string
	rollback          []string
	oldGenerationTest []string
}

func parseGoldenArgs(args []string) (goldenOptions, error) {
	var options goldenOptions
	activationAt, rollbackAt, cleanupAt := -1, -1, -1
	for index, arg := range args {
		switch arg {
		case "--activate":
			if activationAt >= 0 {
				return options, errors.New("--activate may appear only once")
			}
			activationAt = index
		case "--rollback":
			if rollbackAt >= 0 {
				return options, errors.New("--rollback may appear only once")
			}
			rollbackAt = index
		case "--old-generation-check":
			if cleanupAt >= 0 {
				return options, errors.New("--old-generation-check may appear only once")
			}
			cleanupAt = index
		}
	}
	if activationAt < 0 || rollbackAt < 0 || cleanupAt < 0 ||
		!(activationAt < rollbackAt && rollbackAt < cleanupAt) {
		return options, errors.New("actions must be supplied as --activate COMMAND --rollback COMMAND --old-generation-check COMMAND")
	}
	if activationAt+1 == rollbackAt || rollbackAt+1 == cleanupAt || cleanupAt+1 == len(args) {
		return options, errors.New("each golden action requires a command")
	}
	flags := flag.NewFlagSet("golden", flag.ContinueOnError)
	flags.StringVar(&options.cloudConfig, "cloud-config", "", "source cmux.com cloud config")
	flags.StringVar(&options.codexHome, "codex-home", "", "source Codex home containing auth.json")
	flags.StringVar(&options.codexBinary, "codex-bin", "codex", "Codex CLI binary")
	flags.StringVar(&options.releasedVersion, "released-version", "latest", "released Subrouter version")
	flags.StringVar(&options.releasedClient, "released-client", "", "test-only released client override")
	flags.StringVar(&options.artifactDir, "artifact-dir", "", "content-blind evidence directory")
	flags.StringVar(&options.model, "model", "gpt-5.6-sol", "Codex model")
	flags.IntVar(&options.streamLines, "stream-lines", 12000, "numbered lines requested from each continuity turn")
	flags.DurationVar(&options.timeout, "timeout", 20*time.Minute, "overall golden gate timeout")
	if err := flags.Parse(args[:activationAt]); err != nil {
		return options, err
	}
	if flags.NArg() != 0 {
		return options, fmt.Errorf("unexpected golden arguments")
	}
	options.activation = append([]string(nil), args[activationAt+1:rollbackAt]...)
	options.rollback = append([]string(nil), args[rollbackAt+1:cleanupAt]...)
	options.oldGenerationTest = append([]string(nil), args[cleanupAt+1:]...)
	if options.streamLines < 100 && !goldenTestHooks.enabled {
		return options, errors.New("--stream-lines must be at least 100")
	}
	if options.timeout <= 0 {
		return options, errors.New("--timeout must be positive")
	}
	return options, nil
}

type jsonlRecorder struct {
	mu     sync.Mutex
	writer io.Writer
	err    error
}

func (r *jsonlRecorder) write(value any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.err = json.NewEncoder(r.writer).Encode(value)
	return r.err

}

func (r *jsonlRecorder) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

type goldenSummary struct {
	Passed                    bool                    `json:"passed"`
	Failure                   string                  `json:"failure,omitempty"`
	StartedAt                 string                  `json:"started_at"`
	CompletedAt               string                  `json:"completed_at"`
	ReleasedVersion           string                  `json:"released_version"`
	ReleasedSHA256            string                  `json:"released_sha256"`
	ReleaseChecksumVerified   bool                    `json:"release_checksum_verified"`
	ReleasePlatform           string                  `json:"release_platform"`
	ProbeFrequencyHz          int                     `json:"probe_frequency_hz"`
	Activation                goldenActionSummary     `json:"activation"`
	Rollback                  goldenActionSummary     `json:"rollback"`
	OldGenerationCleanup      goldenActionSummary     `json:"old_generation_cleanup"`
	Sessions                  []goldenSessionSummary  `json:"sessions"`
	Health                    []goldenProbeSummary    `json:"health"`
	ProcessSnapshots          []goldenProcessEvidence `json:"process_snapshots"`
	FreshLocalLeaseObserved   bool                    `json:"fresh_local_lease_observed"`
	PrivateWorkspaceRemoved   bool                    `json:"private_workspace_removed"`
	DeploymentEnvironmentRead bool                    `json:"deployment_environment_recorded"`
}

type goldenActionSummary struct {
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	ExitCode   int    `json:"exit_code"`
}

type goldenSessionSummary struct {
	Label                  string   `json:"label"`
	Route                  string   `json:"route"`
	Transport              string   `json:"transport"`
	ProcessID              int      `json:"process_id"`
	ThreadIDHash           string   `json:"thread_id_sha256"`
	NonceHash              string   `json:"nonce_sha256"`
	ResponseRequests       int      `json:"response_requests"`
	ResponseConnections    int      `json:"response_connections"`
	ResponseBytes          int64    `json:"response_bytes"`
	MaxChunkGapMillis      int64    `json:"max_chunk_gap_ms"`
	MarkerCount            int      `json:"marker_count"`
	ResumeMarkerCount      int      `json:"resume_marker_count"`
	ResumeNonceCount       int      `json:"resume_nonce_count"`
	RetryCount             int      `json:"retry_count"`
	ReconnectCount         int      `json:"reconnect_count"`
	FallbackCount          int      `json:"fallback_count"`
	ErrorCount             int      `json:"error_count"`
	NonzeroExitCount       int      `json:"nonzero_exit_count"`
	DuplicateMarkerCount   int      `json:"duplicate_marker_count"`
	SocketIDsBefore        []string `json:"socket_ids_before"`
	SocketIDsAfterRollback []string `json:"socket_ids_after_rollback"`
}

type goldenProbeSummary struct {
	Label             string `json:"label"`
	Samples           int    `json:"samples"`
	Failures          int    `json:"failures"`
	MaxStartGapMillis int64  `json:"max_start_gap_ms"`
}

type goldenProcessEvidence struct {
	Timestamp       string   `json:"timestamp"`
	Phase           string   `json:"phase"`
	Label           string   `json:"label"`
	ProcessID       int      `json:"process_id"`
	DescendantPIDs  []int    `json:"descendant_pids"`
	ProcessStates   []string `json:"process_states"`
	SocketIDs       []string `json:"socket_ids"`
	RemoteSocketIDs []string `json:"remote_socket_ids"`
}

type releasedClient struct {
	path             string
	version          string
	sha256           string
	checksumVerified bool
}

func runGolden(args []string) (runErr error) {
	options, err := parseGoldenArgs(args)
	if err != nil {
		return err
	}
	testMode := goldenTestHooks.enabled
	if !testMode && (runtime.GOOS != "darwin" || runtime.GOARCH != "arm64") {
		return errors.New("golden continuity gate must run locally on macOS arm64")
	}
	if options.releasedClient != "" && !testMode {
		return errors.New("--released-client is available only to deterministic tests")
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return errors.New("resolve operator home")
	}
	if options.cloudConfig == "" {
		options.cloudConfig = filepath.Join(userHome, ".config", "subrouter", "cloud.json")
	}
	if options.codexHome == "" {
		options.codexHome = filepath.Join(userHome, ".codex")
	}
	if options.artifactDir == "" {
		options.artifactDir = filepath.Join("artifacts", "golden-local-mac-continuity-"+time.Now().UTC().Format("20060102T150405Z"))
	}
	artifactDir, err := filepath.Abs(options.artifactDir)
	if err != nil {
		return errors.New("resolve artifact directory")
	}
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return errors.New("create artifact directory")
	}
	if err := os.Chmod(artifactDir, 0o700); err != nil {
		return errors.New("protect artifact directory")
	}
	privateRoot, err := os.MkdirTemp("", "subrouter-golden-private-*")
	if err != nil {
		return errors.New("create private workspace")
	}
	if err := os.Chmod(privateRoot, 0o700); err != nil {
		_ = os.RemoveAll(privateRoot)
		return errors.New("protect private workspace")
	}
	summary := goldenSummary{
		StartedAt:        time.Now().UTC().Format(time.RFC3339Nano),
		ProbeFrequencyHz: 10,
		ReleasePlatform:  "darwin/arm64",
	}
	defer func() {
		removeErr := os.RemoveAll(privateRoot)
		_, statErr := os.Stat(privateRoot)
		summary.PrivateWorkspaceRemoved = removeErr == nil && os.IsNotExist(statErr)
		if !summary.PrivateWorkspaceRemoved && runErr == nil {
			runErr = failGolden("private_workspace_cleanup_failed")
		}
		summary.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
		summary.Passed = runErr == nil
		if runErr != nil {
			summary.Failure = fixedGoldenFailure(runErr)
		}
		data, marshalErr := json.MarshalIndent(summary, "", "  ")
		if marshalErr == nil {
			data = append(data, '\n')
			if writeErr := writePrivateFile(filepath.Join(artifactDir, "result.json"), data); writeErr != nil && runErr == nil {
				runErr = failGolden("result_evidence_write_failed")
			}
		} else if runErr == nil {
			runErr = failGolden("result_evidence_encode_failed")
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()
	runner := goldenRunner{
		options:     options,
		artifactDir: artifactDir,
		privateRoot: privateRoot,
		summary:     &summary,
		testMode:    testMode,
	}
	if err := runner.run(ctx); err != nil {
		return err
	}
	if err := validateGoldenSummary(summary, testMode); err != nil {
		return err
	}
	return nil
}

func fixedGoldenFailure(err error) string {
	var failure *goldenFailure
	if errors.As(err, &failure) {
		return failure.code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "gate_failed"
}

type goldenFailure struct{ code string }

func (e *goldenFailure) Error() string { return e.code }

func failGolden(code string) error { return &goldenFailure{code: code} }

type goldenRunner struct {
	options     goldenOptions
	artifactDir string
	privateRoot string
	summary     *goldenSummary
	testMode    bool

	mu          sync.Mutex
	observers   []*runningGoldenObserver
	sessions    []*goldenSession
	processes   []*exec.Cmd
	probeCancel context.CancelFunc
	probeStats  *goldenProbeStats
	evidence    *jsonlRecorder
}

type goldenCloudConfig struct {
	Raw             map[string]any
	HostedURL       string
	TenantKey       string
	LocalProxyToken string
}

func loadGoldenCloudConfig(path string) (goldenCloudConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return goldenCloudConfig{}, failGolden("cloud_config_unreadable")
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return goldenCloudConfig{}, failGolden("cloud_config_invalid")
	}
	stringValue := func(key string) string {
		value, _ := raw[key].(string)
		return strings.TrimSpace(value)
	}
	config := goldenCloudConfig{
		Raw:             raw,
		HostedURL:       strings.TrimRight(stringValue("hostedUrl"), "/"),
		TenantKey:       stringValue("tenantKey"),
		LocalProxyToken: stringValue("localProxyToken"),
	}
	if config.HostedURL == "" || !validGoldenTenantKey(config.TenantKey) || config.LocalProxyToken == "" {
		return goldenCloudConfig{}, failGolden("cloud_config_not_ready")
	}
	if stringValue("accessToken") == "" || stringValue("refreshToken") == "" || stringValue("teamId") == "" {
		return goldenCloudConfig{}, failGolden("cloud_config_not_logged_in")
	}
	parsed, err := url.Parse(config.HostedURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		if !goldenTestHooks.enabled {
			return goldenCloudConfig{}, failGolden("hosted_url_invalid")
		}
	}
	return config, nil
}

func validGoldenTenantKey(value string) bool {
	if len(value) < len("srt_")+32 || !strings.HasPrefix(value, "srt_") {
		return false
	}
	for _, character := range value[len("srt_"):] {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func writeGoldenConfig(path string, source map[string]any, credentialSource, hostedURL string) error {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	clone["credentialSource"] = credentialSource
	if hostedURL != "" {
		clone["hostedUrl"] = hostedURL
	}
	data, err := json.Marshal(clone)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writePrivateFile(path, data)
}

func (r *goldenRunner) run(ctx context.Context) error {
	for _, command := range []string{"lsof", "pgrep", "ps", r.options.codexBinary} {
		if _, err := exec.LookPath(command); err != nil {
			return failGolden("required_command_missing")
		}
	}
	evidenceFile, err := os.OpenFile(filepath.Join(r.artifactDir, "gate-events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return failGolden("open_gate_evidence")
	}
	defer evidenceFile.Close()
	if err := evidenceFile.Chmod(0o600); err != nil {
		return failGolden("protect_gate_evidence")
	}
	r.evidence = &jsonlRecorder{writer: evidenceFile}

	client, err := acquireReleasedClient(ctx, r.options, r.privateRoot, r.testMode)
	if err != nil {
		return err
	}
	r.summary.ReleasedVersion = client.version
	r.summary.ReleasedSHA256 = client.sha256
	r.summary.ReleaseChecksumVerified = client.checksumVerified

	cloudConfig, err := loadGoldenCloudConfig(r.options.cloudConfig)
	if err != nil {
		return err
	}
	authSource := filepath.Join(r.options.codexHome, "auth.json")
	authData, err := os.ReadFile(authSource)
	if err != nil || len(bytes.TrimSpace(authData)) == 0 {
		return failGolden("codex_auth_unavailable")
	}

	hostedOrigin, err := url.Parse(cloudConfig.HostedURL)
	if err != nil {
		return failGolden("hosted_url_invalid")
	}
	leaseObserver, err := r.startObserver("local-lease", hostedOrigin)
	if err != nil {
		return err
	}
	defer r.stopAll()
	directConfigPath := filepath.Join(r.privateRoot, "hosted-cloud.json")
	teamConfigPath := filepath.Join(r.privateRoot, "team-cloud.json")
	if err := writeGoldenConfig(directConfigPath, cloudConfig.Raw, "hosted", cloudConfig.HostedURL); err != nil {
		return failGolden("private_config_write_failed")
	}
	if err := writeGoldenConfig(teamConfigPath, cloudConfig.Raw, "team", leaseObserver.baseURL); err != nil {
		return failGolden("private_config_write_failed")
	}

	localDaemon, localOrigin, err := r.startLocalDaemon(ctx, client.path, teamConfigPath)
	if err != nil {
		return err
	}
	defer stopAndWaitCommand(localDaemon)

	probeCtx, probeCancel := context.WithCancel(ctx)
	r.probeCancel = probeCancel
	probeStats, err := r.startProbes(probeCtx, hostedOrigin, localOrigin)
	if err != nil {
		return err
	}
	r.probeStats = probeStats
	defer probeCancel()

	type initialSpec struct {
		label     string
		route     string
		transport string
	}
	initialSpecs := []initialSpec{
		{label: "direct-websocket", route: "direct-hosted", transport: "websocket"},
		{label: "direct-http", route: "direct-hosted", transport: "http"},
		{label: "local-websocket", route: "local-egress", transport: "websocket"},
		{label: "local-http", route: "local-egress", transport: "http"},
	}
	initial := make([]*goldenSession, 0, len(initialSpecs))
	for _, spec := range initialSpecs {
		upstream := hostedOrigin
		if spec.route == "local-egress" {
			upstream = localOrigin
		}
		observation, startErr := r.startObserver(spec.label, upstream)
		if startErr != nil {
			return startErr
		}
		session, startErr := r.startInitialSession(ctx, client.path, authData, cloudConfig, directConfigPath, teamConfigPath, observation, spec.label, spec.route, spec.transport)
		if startErr != nil {
			return startErr
		}
		initial = append(initial, session)
	}
	for _, session := range initial {
		if err := waitGoldenInitialReady(ctx, session); err != nil {
			return err
		}
	}
	if err := requireUniqueThreadIDs(initial); err != nil {
		return err
	}
	if err := validateObserverTurns(initial, 1); err != nil {
		return err
	}
	if err := r.requireObserversRunning(); err != nil {
		return err
	}
	if !waitForObserverRequest(ctx, leaseObserver.stats, "/_subrouter/leases", 2, time.Time{}, nil) {
		return failGolden("initial_local_lease_missing")
	}

	before, err := r.capturePhase("before-activation", initial, localDaemon.Process.Pid)
	if err != nil {
		return err
	}
	if err := requireLocalEgress(before); err != nil {
		return err
	}
	beforeByLabel := evidenceByLabel(before)

	activation, err := r.startAction(ctx, "activation", r.options.activation)
	if err != nil {
		return err
	}
	r.summary.Activation.StartedAt = activation.started.UTC().Format(time.RFC3339Nano)
	leaseBefore := observerRequestCount(leaseObserver.stats, "/_subrouter/leases")
	fresh, err := r.startActivationSessions(ctx, client.path, authData, cloudConfig, directConfigPath, teamConfigPath, hostedOrigin, localOrigin)
	if err != nil {
		_ = waitAction(activation)
		return err
	}
	activationEvidenceErr := r.waitActivationEvidence(ctx, activation, fresh, leaseObserver, leaseBefore)
	activationResult := waitAction(activation)
	r.summary.Activation = activationResult
	if activationEvidenceErr != nil {
		return activationEvidenceErr
	}
	if activationResult.ExitCode != 0 {
		return failGolden("activation_nonzero_exit")
	}
	if err := r.requireObserversRunning(); err != nil {
		return err
	}
	if err := requireSessionsRunning(initial, "activation"); err != nil {
		return err
	}
	if err := waitGoldenSessions(ctx, fresh); err != nil {
		return err
	}
	if err := validateGoldenSessions(fresh, false); err != nil {
		return err
	}
	if err := validateObserverTurns(fresh, 1); err != nil {
		return err
	}

	during, err := r.capturePhase("after-activation", initial, localDaemon.Process.Pid)
	if err != nil {
		return err
	}
	if err := requireLocalEgress(during); err != nil {
		return err
	}
	if err := requireSessionsRunning(initial, "activation_followup"); err != nil {
		return err
	}

	rollback, err := r.startAction(ctx, "rollback", r.options.rollback)
	if err != nil {
		return err
	}
	r.summary.Rollback.StartedAt = rollback.started.UTC().Format(time.RFC3339Nano)
	rollbackResult := waitAction(rollback)
	r.summary.Rollback = rollbackResult
	if rollbackResult.ExitCode != 0 {
		return failGolden("rollback_nonzero_exit")
	}
	if err := r.requireObserversRunning(); err != nil {
		return err
	}
	if err := requireSessionsRunning(initial, "rollback"); err != nil {
		return err
	}

	after, err := r.capturePhase("after-rollback", initial, localDaemon.Process.Pid)
	if err != nil {
		return err
	}
	if err := requireLocalEgress(after); err != nil {
		return err
	}
	afterByLabel := evidenceByLabel(after)
	if err := requireStableSessionSockets(initial, beforeByLabel, afterByLabel); err != nil {
		return err
	}
	if err := requireStableLocalEgress(beforeByLabel, afterByLabel); err != nil {
		return err
	}

	if err := waitGoldenSessions(ctx, initial); err != nil {
		return err
	}
	if err := validateGoldenSessions(initial, false); err != nil {
		return err
	}

	resumeSessions, err := r.startResumeSessions(ctx, client.path, initial)
	if err != nil {
		return err
	}
	if err := waitGoldenSessions(ctx, resumeSessions); err != nil {
		return err
	}
	if err := validateGoldenSessions(resumeSessions, true); err != nil {
		return err
	}
	if err := validateResumeThreads(initial, resumeSessions); err != nil {
		return err
	}
	if err := validateObserverTurns(initial, 2); err != nil {
		return err
	}

	cleanupResult := runOpaqueAction(ctx, r.options.oldGenerationTest)
	r.summary.OldGenerationCleanup = cleanupResult
	if cleanupResult.ExitCode != 0 {
		return failGolden("old_generation_cleanup_failed")
	}
	probeCancel()
	probeStats.wait()
	r.summary.Health = probeStats.summaries()
	if r.evidence.failure() != nil || probeStats.record.failure() != nil {
		return failGolden("evidence_write_failed")
	}
	if err := probeStats.validateInterval(parseSummaryTime(r.summary.Activation.StartedAt), parseSummaryTime(r.summary.Rollback.FinishedAt)); err != nil {
		return err
	}
	r.summary.Sessions = buildGoldenSessionSummaries(initial, resumeSessions, fresh, beforeByLabel, afterByLabel)
	r.summary.FreshLocalLeaseObserved = true
	return nil
}

func parseSummaryTime(raw string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, raw)
	return parsed
}

func acquireReleasedClient(ctx context.Context, options goldenOptions, privateRoot string, testMode bool) (releasedClient, error) {
	if options.releasedClient != "" {
		info, err := os.Stat(options.releasedClient)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return releasedClient{}, failGolden("released_client_override_invalid")
		}
		digest, err := fileSHA256(options.releasedClient)
		if err != nil {
			return releasedClient{}, failGolden("released_client_hash_failed")
		}
		return releasedClient{path: options.releasedClient, version: "test-override", sha256: digest, checksumVerified: testMode}, nil
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	latestURL := "https://api.github.com/repos/manaflow-ai/subrouter/releases/latest"
	downloadRoot := "https://github.com/manaflow-ai/subrouter/releases/download"
	if testMode {
		if override := strings.TrimSpace(goldenTestHooks.releaseAPI); override != "" {
			latestURL = override
		}
		if override := strings.TrimRight(strings.TrimSpace(goldenTestHooks.releaseDownloadRoot), "/"); override != "" {
			downloadRoot = override
		}
	}
	version := strings.TrimPrefix(strings.TrimSpace(options.releasedVersion), "v")
	if version == "" || version == "latest" {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
		if err != nil {
			return releasedClient{}, failGolden("release_resolution_failed")
		}
		response, err := httpClient.Do(request)
		if err != nil {
			return releasedClient{}, failGolden("release_resolution_failed")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return releasedClient{}, failGolden("release_resolution_failed")
		}
		var payload struct {
			TagName string `json:"tag_name"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
			return releasedClient{}, failGolden("release_resolution_failed")
		}
		version = strings.TrimPrefix(strings.TrimSpace(payload.TagName), "v")
	}
	if version == "" || strings.ContainsAny(version, "/?#") {
		return releasedClient{}, failGolden("release_version_invalid")
	}
	asset := "subrouter_" + version + "_darwin_arm64"
	base := downloadRoot + "/v" + url.PathEscape(version) + "/"
	binaryData, err := downloadGoldenAsset(ctx, httpClient, base+asset, 256<<20)
	if err != nil {
		return releasedClient{}, failGolden("release_binary_download_failed")
	}
	sums, err := downloadGoldenAsset(ctx, httpClient, base+"SHA256SUMS", 4<<20)
	if err != nil {
		return releasedClient{}, failGolden("release_checksum_download_failed")
	}
	expected := ""
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			expected = strings.ToLower(fields[0])
			break
		}
	}
	actualBytes := sha256.Sum256(binaryData)
	actual := hex.EncodeToString(actualBytes[:])
	if len(expected) != 64 || expected != actual {
		return releasedClient{}, failGolden("release_checksum_mismatch")
	}
	path := filepath.Join(privateRoot, asset)
	if err := os.WriteFile(path, binaryData, 0o700); err != nil {
		return releasedClient{}, failGolden("released_client_write_failed")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return releasedClient{}, failGolden("released_client_write_failed")
	}
	return releasedClient{path: path, version: version, sha256: actual, checksumVerified: true}, nil
}

func downloadGoldenAsset(ctx context.Context, client *http.Client, rawURL string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download status")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, fmt.Errorf("download size")
	}
	return data, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type runningGoldenObserver struct {
	label    string
	baseURL  string
	server   *http.Server
	listener net.Listener
	events   *os.File
	stats    *observerStats
	pid      int
	done     chan error
}

func (r *goldenRunner) startObserver(label string, upstream *url.URL) (*runningGoldenObserver, error) {
	if err := validateObserverUpstream(upstream); err != nil {
		return nil, failGolden("observer_upstream_invalid")
	}
	eventPath := filepath.Join(r.artifactDir, "transport-"+label+".jsonl")
	events, err := os.OpenFile(eventPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, failGolden("observer_evidence_open_failed")
	}
	if err := events.Chmod(0o600); err != nil {
		events.Close()
		return nil, failGolden("observer_evidence_protect_failed")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		events.Close()
		return nil, failGolden("observer_listen_failed")
	}
	stats := newObserverStats()
	server := &http.Server{
		Handler:           newObserverHandlerWithStats(upstream, events, stats),
		ReadHeaderTimeout: 10 * time.Second,
	}
	running := &runningGoldenObserver{
		label: label, baseURL: "http://" + listener.Addr().String(), server: server,
		listener: listener, events: events, stats: stats, pid: os.Getpid(), done: make(chan error, 1),
	}
	r.mu.Lock()
	r.observers = append(r.observers, running)
	r.mu.Unlock()
	go func() { running.done <- server.Serve(listener) }()
	return running, nil
}

func (r *goldenRunner) requireObserversRunning() error {
	r.mu.Lock()
	observers := append([]*runningGoldenObserver(nil), r.observers...)
	r.mu.Unlock()
	for _, observer := range observers {
		select {
		case <-observer.done:
			return failGolden("observer_exited")
		default:
		}
	}
	return nil
}

func (o *runningGoldenObserver) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = o.server.Shutdown(ctx)
	_ = o.listener.Close()
	_ = o.events.Close()
}

func (r *goldenRunner) startLocalDaemon(ctx context.Context, clientPath, teamConfigPath string) (*exec.Cmd, *url.URL, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, failGolden("local_daemon_port_failed")
	}
	address := listener.Addr().String()
	_ = listener.Close()
	home := filepath.Join(r.privateRoot, "local-daemon-home")
	state := filepath.Join(r.privateRoot, "local-daemon-state")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, nil, failGolden("local_daemon_home_failed")
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, nil, failGolden("local_daemon_home_failed")
	}
	command := exec.CommandContext(ctx, clientPath,
		"serve", "--addr", address,
		"--cloud-config", teamConfigPath,
		"--sessions", filepath.Join(state, "sessions.json"),
		"--fetch-usage=false",
		"--sr-switch-interval=0",
	)
	configureProcessGroup(command)
	command.Env = goldenChildEnv(home, map[string]string{
		"SUBROUTER_STATE_DIR":    state,
		"SUBROUTER_CLOUD_CONFIG": teamConfigPath,
	})
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return nil, nil, failGolden("local_daemon_start_failed")
	}
	r.mu.Lock()
	r.processes = append(r.processes, command)
	r.mu.Unlock()
	origin, _ := url.Parse("http://" + address)
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-deadline.C:
			return nil, nil, failGolden("local_daemon_not_ready")
		case <-ticker.C:
			if !processAlive(command.Process) {
				return nil, nil, failGolden("local_daemon_exited")
			}
			probeCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
			request, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, origin.String()+"/_subrouter/ready", nil)
			response, requestErr := http.DefaultClient.Do(request)
			if requestErr == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
				response.Body.Close()
			}
			cancel()
			if requestErr == nil && response.StatusCode >= 200 && response.StatusCode < 300 {
				return command, origin, nil
			}
		}
	}
}

func goldenChildEnv(home string, overrides map[string]string) []string {
	allowed := map[string]bool{
		"PATH": true, "TMPDIR": true, "TMP": true, "TEMP": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "LANG": true, "LC_ALL": true,
	}
	values := make(map[string]string)
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok && allowed[key] {
			values[key] = value
		}
	}
	values["HOME"] = home
	for key, value := range overrides {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

type goldenProbeEvent struct {
	Kind          string `json:"kind"`
	Timestamp     string `json:"timestamp"`
	CompletedAt   string `json:"completed_at"`
	Label         string `json:"label"`
	OK            bool   `json:"ok"`
	StatusCode    int    `json:"status_code"`
	ResponseBytes int64  `json:"response_bytes"`
	ElapsedMillis int64  `json:"elapsed_ms"`
}

type goldenProbeStats struct {
	mu       sync.Mutex
	events   []goldenProbeEvent
	record   *jsonlRecorder
	loops    sync.WaitGroup
	samples  sync.WaitGroup
	finished chan struct{}
}

func (r *goldenRunner) startProbes(ctx context.Context, publicOrigin, localOrigin *url.URL) (*goldenProbeStats, error) {
	file, err := os.OpenFile(filepath.Join(r.artifactDir, "health-ready-10hz.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, failGolden("health_evidence_open_failed")
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, failGolden("health_evidence_protect_failed")
	}
	stats := &goldenProbeStats{record: &jsonlRecorder{writer: file}, finished: make(chan struct{})}
	targets := []struct {
		label string
		url   string
	}{
		{label: "public-health", url: strings.TrimRight(publicOrigin.String(), "/") + "/_subrouter/health"},
		{label: "public-ready", url: strings.TrimRight(publicOrigin.String(), "/") + "/_subrouter/ready"},
		{label: "local-health", url: strings.TrimRight(localOrigin.String(), "/") + "/_subrouter/health"},
		{label: "local-ready", url: strings.TrimRight(localOrigin.String(), "/") + "/_subrouter/ready"},
	}
	for _, target := range targets {
		target := target
		stats.loops.Add(1)
		go func() {
			defer stats.loops.Done()
			ticker := time.NewTicker(goldenProbeInterval)
			defer ticker.Stop()
			stats.launchProbe(ctx, target.label, target.url)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					stats.launchProbe(ctx, target.label, target.url)
				}
			}
		}()
	}
	go func() {
		<-ctx.Done()
		stats.loops.Wait()
		stats.samples.Wait()
		_ = file.Close()
		close(stats.finished)
	}()
	return stats, nil
}

func (s *goldenProbeStats) launchProbe(parent context.Context, label, rawURL string) {
	s.samples.Add(1)
	go func() {
		defer s.samples.Done()
		started := time.Now().UTC()
		ctx, cancel := context.WithTimeout(parent, goldenHTTPTimeout)
		defer cancel()
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		status := 0
		var responseBytes int64
		if requestErr == nil {
			client := &http.Client{
				Timeout: goldenHTTPTimeout,
				CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
					return http.ErrUseLastResponse
				},
			}
			response, err := client.Do(request)
			requestErr = err
			if err == nil {
				status = response.StatusCode
				responseBytes, requestErr = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
				response.Body.Close()
			}
		}
		completed := time.Now().UTC()
		event := goldenProbeEvent{
			Kind: "probe", Timestamp: started.Format(time.RFC3339Nano),
			CompletedAt: completed.Format(time.RFC3339Nano), Label: label,
			OK:         requestErr == nil && status >= 200 && status < 300,
			StatusCode: status, ResponseBytes: responseBytes,
			ElapsedMillis: completed.Sub(started).Milliseconds(),
		}
		s.mu.Lock()
		s.events = append(s.events, event)
		s.mu.Unlock()
		_ = s.record.write(event)
	}()
}

func (s *goldenProbeStats) wait() {
	<-s.finished
}

func (s *goldenProbeStats) summaries() []goldenProbeSummary {
	s.mu.Lock()
	events := append([]goldenProbeEvent(nil), s.events...)
	s.mu.Unlock()
	byLabel := make(map[string][]time.Time)
	failures := make(map[string]int)
	for _, event := range events {
		stamp, _ := time.Parse(time.RFC3339Nano, event.Timestamp)
		byLabel[event.Label] = append(byLabel[event.Label], stamp)
		if !event.OK {
			failures[event.Label]++
		}
	}
	labels := []string{"public-health", "public-ready", "local-health", "local-ready"}
	result := make([]goldenProbeSummary, 0, len(labels))
	for _, label := range labels {
		stamps := byLabel[label]
		sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })
		var maxGap time.Duration
		for index := 1; index < len(stamps); index++ {
			if gap := stamps[index].Sub(stamps[index-1]); gap > maxGap {
				maxGap = gap
			}
		}
		result = append(result, goldenProbeSummary{
			Label: label, Samples: len(stamps), Failures: failures[label], MaxStartGapMillis: maxGap.Milliseconds(),
		})
	}
	return result
}

func (s *goldenProbeStats) validateInterval(start, end time.Time) error {
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return failGolden("health_interval_missing")
	}
	s.mu.Lock()
	events := append([]goldenProbeEvent(nil), s.events...)
	s.mu.Unlock()
	labels := []string{"public-health", "public-ready", "local-health", "local-ready"}
	for _, label := range labels {
		var stamps []time.Time
		for _, event := range events {
			stamp, _ := time.Parse(time.RFC3339Nano, event.Timestamp)
			if event.Label == label && !stamp.Before(start) && !stamp.After(end) {
				if !event.OK {
					return failGolden("health_probe_failed")
				}
				stamps = append(stamps, stamp)
			}
		}
		sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })
		minimum := int(end.Sub(start).Seconds() * 8)
		if minimum < 2 {
			minimum = 2
		}
		if len(stamps) < minimum {
			return failGolden("health_probe_frequency_low")
		}
		if stamps[0].Sub(start) > 200*time.Millisecond || end.Sub(stamps[len(stamps)-1]) > 200*time.Millisecond {
			return failGolden("health_probe_coverage_gap")
		}
		for index := 1; index < len(stamps); index++ {
			if stamps[index].Sub(stamps[index-1]) > 250*time.Millisecond {
				return failGolden("health_probe_gap")
			}
		}
	}
	return nil
}

type goldenSession struct {
	label      string
	route      string
	transport  string
	nonce      string
	marker     string
	resume     bool
	home       string
	codexHome  string
	configPath string
	baseURL    string
	observer   *runningGoldenObserver
	command    *exec.Cmd
	startedAt  time.Time

	mu              sync.Mutex
	threadID        string
	threadIDCount   int
	markerCount     int
	nonceCount      int
	issues          map[string]int
	stdoutBytes     int64
	stderrBytes     int64
	exitCode        int
	waitErr         error
	done            chan struct{}
	threadAvailable chan struct{}
}

func randomGoldenToken(prefix string) (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(data), nil
}

func (r *goldenRunner) startInitialSession(
	ctx context.Context,
	clientPath string,
	authData []byte,
	cloud goldenCloudConfig,
	directConfigPath, teamConfigPath string,
	observation *runningGoldenObserver,
	label, route, transport string,
) (*goldenSession, error) {
	nonce, err := randomGoldenToken("nonce_")
	if err != nil {
		return nil, failGolden("nonce_generation_failed")
	}
	marker, err := randomGoldenToken("SR_GOLDEN_COMPLETE_")
	if err != nil {
		return nil, failGolden("marker_generation_failed")
	}
	prompt := fmt.Sprintf(
		"Do not use tools. First output exactly %s once on its own line. Then output %d numbered lines, each containing only its number and the letter x. Do not stop, summarize, or skip a number. After all numbered lines, output exactly %s once on its own line.",
		nonce, r.options.streamLines, marker,
	)
	return r.startSession(ctx, clientPath, authData, cloud, directConfigPath, teamConfigPath, observation, label, route, transport, nonce, marker, "", prompt, false)
}

func (r *goldenRunner) startFreshSession(
	ctx context.Context,
	clientPath string,
	authData []byte,
	cloud goldenCloudConfig,
	directConfigPath, teamConfigPath string,
	observation *runningGoldenObserver,
	label, route, transport string,
) (*goldenSession, error) {
	nonce, err := randomGoldenToken("fresh_nonce_")
	if err != nil {
		return nil, failGolden("nonce_generation_failed")
	}
	marker, err := randomGoldenToken("SR_GOLDEN_FRESH_")
	if err != nil {
		return nil, failGolden("marker_generation_failed")
	}
	prompt := fmt.Sprintf("Do not use tools. Reply with exactly %s then one space then exactly %s.", nonce, marker)
	return r.startSession(ctx, clientPath, authData, cloud, directConfigPath, teamConfigPath, observation, label, route, transport, nonce, marker, "", prompt, false)
}

func (r *goldenRunner) startSession(
	ctx context.Context,
	clientPath string,
	authData []byte,
	cloud goldenCloudConfig,
	directConfigPath, teamConfigPath string,
	observation *runningGoldenObserver,
	label, route, transport, nonce, marker, threadID, prompt string,
	resume bool,
) (*goldenSession, error) {
	home := filepath.Join(r.privateRoot, "session-"+label+"-home")
	codexHome := filepath.Join(r.privateRoot, "session-"+label+"-codex")
	if resume {
		// Resumes must reuse the exact isolated home that owns the original
		// thread. The caller replaces these paths immediately below.
		home = ""
		codexHome = ""
	} else {
		if err := os.MkdirAll(home, 0o700); err != nil {
			return nil, failGolden("session_home_failed")
		}
		if err := os.MkdirAll(codexHome, 0o700); err != nil {
			return nil, failGolden("session_home_failed")
		}
		if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), authData, 0o600); err != nil {
			return nil, failGolden("session_auth_copy_failed")
		}
	}
	configPath := directConfigPath
	baseURL := observation.baseURL + "/t/" + cloud.TenantKey + "/v1"
	if route == "local-egress" {
		configPath = teamConfigPath
		baseURL = observation.baseURL + "/v1"
	}
	session := &goldenSession{
		label: label, route: route, transport: transport, nonce: nonce, marker: marker,
		resume: resume, home: home, codexHome: codexHome, configPath: configPath,
		baseURL: baseURL, observer: observation, issues: make(map[string]int),
		done: make(chan struct{}), threadAvailable: make(chan struct{}),
	}
	if err := r.launchSession(ctx, clientPath, session, threadID, prompt); err != nil {
		return nil, err
	}
	return session, nil
}

func (r *goldenRunner) launchSession(ctx context.Context, clientPath string, session *goldenSession, resumeThreadID, prompt string) error {
	args := []string{
		"codex", "exec", "--json", "--ignore-user-config", "--ignore-rules",
		"--skip-git-repo-check", "-C", r.privateRoot, "-s", "read-only", "-m", r.options.model,
	}
	if session.transport == "http" {
		args = append(args, "-c", `model_providers.subrouter.supports_websockets=false`)
	}
	if session.resume {
		args = append(args, "resume", resumeThreadID, prompt)
	} else {
		args = append(args, prompt)
	}
	command := exec.CommandContext(ctx, clientPath, args...)
	configureProcessGroup(command)
	overrides := map[string]string{
		"CODEX_HOME":                 session.codexHome,
		"SUBROUTER_CLOUD_CONFIG":     session.configPath,
		"SUBROUTER_CODEX_BASE_URL":   session.baseURL,
		"SUBROUTER_CODEX_BIN":        r.options.codexBinary,
		"SUBROUTER_DISABLE_FALLBACK": "1",
		"SUBROUTER_STATE_DIR":        filepath.Join(session.home, ".subrouter"),
	}
	if session.route == "local-egress" {
		overrides["SUBROUTER_LOCAL_BASE_URL"] = session.baseURL
	}
	command.Env = goldenChildEnv(session.home, overrides)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return failGolden("session_pipe_failed")
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return failGolden("session_pipe_failed")
	}
	if err := command.Start(); err != nil {
		return failGolden("session_start_failed")
	}
	session.command = command
	session.startedAt = time.Now().UTC()
	r.mu.Lock()
	r.sessions = append(r.sessions, session)
	r.processes = append(r.processes, command)
	r.mu.Unlock()
	_ = r.evidence.write(map[string]any{
		"kind": "session_started", "timestamp": session.startedAt.Format(time.RFC3339Nano),
		"label": session.label, "route": session.route, "transport": session.transport,
		"process_id": command.Process.Pid, "resume": session.resume,
	})
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		consumeGoldenStdout(session, stdout)
	}()
	go func() {
		defer readers.Done()
		consumeGoldenStderr(session, stderr)
	}()
	go func() {
		readers.Wait()
		waitErr := command.Wait()
		session.mu.Lock()
		session.waitErr = waitErr
		session.exitCode = commandExitCode(waitErr)
		exitCode := session.exitCode
		markerCount := session.markerCount
		nonceCount := session.nonceCount
		issues := issueCount(session.issues)
		session.mu.Unlock()
		_ = r.evidence.write(map[string]any{
			"kind": "session_finished", "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"label": session.label, "exit_code": exitCode, "marker_count": markerCount,
			"nonce_count": nonceCount, "issue_count": issues,
		})
		close(session.done)
	}()
	return nil
}

func consumeGoldenStdout(session *goldenSession, reader io.Reader) {
	buffered := bufio.NewReaderSize(reader, 64<<10)
	for {
		line, err := buffered.ReadBytes('\n')
		if len(line) > 0 {
			session.mu.Lock()
			session.stdoutBytes += int64(len(line))
			session.mu.Unlock()
			var event struct {
				Type     string `json:"type"`
				ThreadID string `json:"thread_id"`
				Item     struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
			}
			if json.Unmarshal(line, &event) != nil {
				session.addIssue("invalid_json")
			} else {
				if event.Type == "thread.started" && strings.TrimSpace(event.ThreadID) != "" {
					session.setThreadID(strings.TrimSpace(event.ThreadID))
				}
				if strings.Contains(strings.ToLower(event.Type), "error") || strings.Contains(strings.ToLower(event.Type), "failed") {
					session.addIssue("codex_error")
				}
				if event.Item.Type == "agent_message" {
					session.mu.Lock()
					session.markerCount += strings.Count(event.Item.Text, session.marker)
					session.nonceCount += strings.Count(event.Item.Text, session.nonce)
					session.mu.Unlock()
				}
			}
		}
		if err != nil {
			if err != io.EOF && !errors.Is(err, os.ErrClosed) {
				session.addIssue("stdout_read")
			}
			return
		}
	}
}

func consumeGoldenStderr(session *goldenSession, reader io.Reader) {
	buffer := make([]byte, 32<<10)
	tail := ""
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			session.mu.Lock()
			session.stderrBytes += int64(n)
			session.mu.Unlock()
			text := strings.ToLower(tail + string(buffer[:n]))
			categories := map[string][]string{
				"reconnect": {"reconnect", "disconnected", "connection reset"},
				"retry":     {"retry", "retrying"},
				"fallback":  {"falling back", "fallback"},
				"timeout":   {"timed out", "timeout"},
				"error":     {"error", "failed", "failure"},
			}
			for category, needles := range categories {
				for _, needle := range needles {
					if strings.Contains(text, needle) {
						session.addIssue(category)
						break
					}
				}
			}
			if len(text) > 128 {
				tail = text[len(text)-128:]
			} else {
				tail = text
			}
		}
		if err != nil {
			if err != io.EOF && !errors.Is(err, os.ErrClosed) {
				session.addIssue("stderr_read")
			}
			return
		}
	}
}

func (s *goldenSession) setThreadID(threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threadIDCount++
	if s.threadID == "" {
		s.threadID = threadID
		close(s.threadAvailable)
	} else if s.threadID != threadID {
		s.issues["thread_changed"]++
	}
}

func (s *goldenSession) addIssue(category string) {
	s.mu.Lock()
	s.issues[category]++
	s.mu.Unlock()
}

func issueCount(issues map[string]int) int {
	total := 0
	for _, count := range issues {
		total += count
	}
	return total
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func waitGoldenInitialReady(ctx context.Context, session *goldenSession) error {
	for {
		if sessionHasResponseBytes(session) {
			select {
			case <-session.threadAvailable:
				if sessionDone(session) {
					return failGolden("initial_session_finished_before_activation")
				}
				return nil
			default:
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-session.done:
			return failGolden("initial_session_finished_before_response")
		case <-session.observer.stats.notify:
		case <-session.threadAvailable:
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func sessionHasResponseBytes(session *goldenSession) bool {
	_, chunks, _ := session.observer.stats.snapshot()
	for _, chunk := range chunks {
		if chunk.Kind == "response_chunk" && chunk.Bytes > 0 &&
			(chunk.Path == "/v1/responses" || chunk.Path == "/responses") {
			return true
		}
	}
	return false
}

func sessionDone(session *goldenSession) bool {
	select {
	case <-session.done:
		return true
	default:
		return false
	}
}

func requireUniqueThreadIDs(sessions []*goldenSession) error {
	seen := make(map[string]bool)
	for _, session := range sessions {
		session.mu.Lock()
		threadID := session.threadID
		session.mu.Unlock()
		if threadID == "" || seen[threadID] {
			return failGolden("thread_ids_not_unique")
		}
		seen[threadID] = true
	}
	return nil
}

func responseRequests(stats *observerStats) []transportEvent {
	requests, _, _ := stats.snapshot()
	var result []transportEvent
	for _, request := range requests {
		if request.Path == "/v1/responses" || request.Path == "/responses" {
			result = append(result, request)
		}
	}
	return result
}

func validateObserverTurns(sessions []*goldenSession, expectedRequests int) error {
	for _, session := range sessions {
		requests := responseRequests(session.observer.stats)
		if len(requests) != expectedRequests {
			return failGolden("response_request_count_invalid")
		}
		for _, request := range requests {
			if request.Transport != session.transport {
				return failGolden("transport_fallback_detected")
			}
			if request.ConnectionID == "" || request.RequestID == "" || request.Method == "" || request.Path == "" {
				return failGolden("transport_evidence_incomplete")
			}
		}
		_, chunks, proxyErrors := session.observer.stats.snapshot()
		if proxyErrors != 0 {
			return failGolden("observer_proxy_error")
		}
		responseBytes := int64(0)
		for _, chunk := range chunks {
			if chunk.Kind == "response_chunk" && (chunk.Path == "/v1/responses" || chunk.Path == "/responses") {
				if chunk.ConnectionID == "" || chunk.RequestID == "" || chunk.Timestamp == "" || chunk.Bytes <= 0 {
					return failGolden("chunk_evidence_incomplete")
				}
				responseBytes += chunk.Bytes
			}
		}
		if responseBytes == 0 {
			return failGolden("response_bytes_missing")
		}
	}
	return nil
}

func observerRequestCount(stats *observerStats, path string) int {
	requests, _, _ := stats.snapshot()
	count := 0
	for _, request := range requests {
		if request.Path == path {
			count++
		}
	}
	return count
}

func waitForObserverRequest(ctx context.Context, stats *observerStats, path string, minimum int, after time.Time, actionDone <-chan struct{}) bool {
	for {
		requests, _, _ := stats.snapshot()
		count := 0
		for _, request := range requests {
			stamp, _ := time.Parse(time.RFC3339Nano, request.Timestamp)
			if request.Path == path && (after.IsZero() || stamp.After(after)) {
				count++
			}
		}
		if count >= minimum {
			if actionDone != nil {
				select {
				case <-actionDone:
					return false
				default:
				}
			}
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-stats.notify:
		case <-actionDone:
			return false
		case <-time.After(20 * time.Millisecond):
		}
	}
}

type runningGoldenAction struct {
	label   string
	started time.Time
	done    chan struct{}
	result  goldenActionSummary
	mu      sync.Mutex
}

func (r *goldenRunner) startAction(ctx context.Context, label string, argv []string) (*runningGoldenAction, error) {
	if len(argv) == 0 {
		return nil, failGolden(label + "_command_missing")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	configureProcessGroup(command)
	// The operator action inherits the operator's environment and terminal.
	// Neither the command vector nor its environment is serialized by the gate.
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	started := time.Now().UTC()
	if err := command.Start(); err != nil {
		return nil, failGolden(label + "_start_failed")
	}
	action := &runningGoldenAction{label: label, started: started, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			killProcessGroup(command)
		case <-action.done:
		}
	}()
	go func() {
		err := command.Wait()
		result := goldenActionSummary{
			StartedAt:  started.Format(time.RFC3339Nano),
			FinishedAt: time.Now().UTC().Format(time.RFC3339Nano),
			ExitCode:   commandExitCode(err),
		}
		action.mu.Lock()
		action.result = result
		action.mu.Unlock()
		close(action.done)
	}()
	_ = r.evidence.write(map[string]any{
		"kind": "operator_action_started", "timestamp": started.Format(time.RFC3339Nano), "action": label,
	})
	return action, nil
}

func waitAction(action *runningGoldenAction) goldenActionSummary {
	<-action.done
	action.mu.Lock()
	defer action.mu.Unlock()
	return action.result
}

func runOpaqueAction(ctx context.Context, argv []string) goldenActionSummary {
	started := time.Now().UTC()
	result := goldenActionSummary{StartedAt: started.Format(time.RFC3339Nano), ExitCode: -1}
	if len(argv) == 0 {
		result.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return result
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	configureProcessGroup(command)
	command.Stdin = os.Stdin
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	runErr := command.Run()
	if ctx.Err() != nil {
		killProcessGroup(command)
	}
	result.ExitCode = commandExitCode(runErr)
	result.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return result
}

func (r *goldenRunner) startActivationSessions(
	ctx context.Context,
	clientPath string,
	authData []byte,
	cloud goldenCloudConfig,
	directConfigPath, teamConfigPath string,
	hostedOrigin, localOrigin *url.URL,
) ([]*goldenSession, error) {
	specs := []struct {
		label, route string
		upstream     *url.URL
	}{
		{label: "activation-direct", route: "direct-hosted", upstream: hostedOrigin},
		{label: "activation-local", route: "local-egress", upstream: localOrigin},
	}
	result := make([]*goldenSession, 0, len(specs))
	for _, spec := range specs {
		observation, err := r.startObserver(spec.label, spec.upstream)
		if err != nil {
			return nil, err
		}
		session, err := r.startFreshSession(ctx, clientPath, authData, cloud, directConfigPath, teamConfigPath, observation, spec.label, spec.route, "websocket")
		if err != nil {
			return nil, err
		}
		result = append(result, session)
	}
	return result, nil
}

func (r *goldenRunner) waitActivationEvidence(
	ctx context.Context,
	action *runningGoldenAction,
	fresh []*goldenSession,
	leaseObserver *runningGoldenObserver,
	leaseBefore int,
) error {
	for _, session := range fresh {
		for !sessionHasResponseBytes(session) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-session.done:
				if !sessionHasResponseBytes(session) {
					return failGolden("activation_fresh_response_missing")
				}
			case <-session.observer.stats.notify:
			case <-action.done:
				return failGolden("activation_ended_before_fresh_response")
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	if !waitForObserverRequest(ctx, leaseObserver.stats, "/_subrouter/leases", 1, action.started, action.done) {
		return failGolden("activation_fresh_local_lease_missing")
	}
	if observerRequestCount(leaseObserver.stats, "/_subrouter/leases") < leaseBefore+1 {
		return failGolden("activation_fresh_local_lease_missing")
	}
	select {
	case <-action.done:
		return failGolden("activation_ended_before_fresh_evidence")
	default:
	}
	r.summary.FreshLocalLeaseObserved = true
	return nil
}

func requireSessionsRunning(sessions []*goldenSession, phase string) error {
	for _, session := range sessions {
		if sessionDone(session) {
			return failGolden("original_session_ended_during_" + phase)
		}
		if !processAlive(session.command.Process) {
			return failGolden("original_session_process_missing")
		}
	}
	return nil
}

func waitGoldenSessions(ctx context.Context, sessions []*goldenSession) error {
	for _, session := range sessions {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-session.done:
		}
	}
	return nil
}

func validateGoldenSessions(sessions []*goldenSession, resume bool) error {
	for _, session := range sessions {
		session.mu.Lock()
		exitCode := session.exitCode
		markerCount := session.markerCount
		nonceCount := session.nonceCount
		threadID := session.threadID
		threadIDCount := session.threadIDCount
		issues := issueCount(session.issues)
		session.mu.Unlock()
		if exitCode != 0 {
			return failGolden("codex_nonzero_exit")
		}
		if markerCount != 1 {
			if markerCount > 1 {
				return failGolden("duplicate_completion_marker")
			}
			return failGolden("completion_marker_missing")
		}
		if nonceCount < 1 {
			return failGolden("nonce_context_missing")
		}
		if resume && nonceCount != 1 {
			return failGolden("resume_nonce_not_exact")
		}
		if threadID == "" || threadIDCount < 1 {
			return failGolden("thread_id_missing")
		}
		if issues != 0 {
			keys := make([]string, 0, len(session.issues))
			for key, count := range session.issues {
				if count > 0 {
					keys = append(keys, key)
				}
			}
			sort.Strings(keys)
			if len(keys) > 0 {
				return failGolden("codex_transport_issue_" + keys[0])
			}
			return failGolden("codex_transport_issue")
		}
	}
	return nil
}

func (r *goldenRunner) startResumeSessions(ctx context.Context, clientPath string, initial []*goldenSession) ([]*goldenSession, error) {
	result := make([]*goldenSession, 0, len(initial))
	for _, original := range initial {
		original.mu.Lock()
		threadID := original.threadID
		original.mu.Unlock()
		marker, err := randomGoldenToken("SR_GOLDEN_RESUME_")
		if err != nil {
			return nil, failGolden("marker_generation_failed")
		}
		prompt := "Do not use tools. Reply with the exact nonce from the first turn, then one newline, then exactly " + marker + ". Do not repeat either value."
		session := &goldenSession{
			label: original.label + "-resume", route: original.route, transport: original.transport,
			nonce: original.nonce, marker: marker, resume: true, home: original.home,
			codexHome: original.codexHome, configPath: original.configPath, baseURL: original.baseURL,
			observer: original.observer, issues: make(map[string]int), done: make(chan struct{}),
			threadAvailable: make(chan struct{}),
		}
		if err := r.launchSession(ctx, clientPath, session, threadID, prompt); err != nil {
			return nil, err
		}
		result = append(result, session)
	}
	return result, nil
}

func validateResumeThreads(initial, resumes []*goldenSession) error {
	if len(initial) != len(resumes) {
		return failGolden("resume_session_count_invalid")
	}
	for index := range initial {
		initial[index].mu.Lock()
		originalThread := initial[index].threadID
		initial[index].mu.Unlock()
		resumes[index].mu.Lock()
		resumedThread := resumes[index].threadID
		resumes[index].mu.Unlock()
		if originalThread == "" || resumedThread != originalThread {
			return failGolden("resume_thread_changed")
		}
	}
	return nil
}

func (r *goldenRunner) capturePhase(phase string, sessions []*goldenSession, localDaemonPID int) ([]goldenProcessEvidence, error) {
	var result []goldenProcessEvidence
	for _, session := range sessions {
		evidence, err := captureProcessEvidence(phase, session.label, session.command.Process.Pid)
		if err != nil {
			return nil, err
		}
		result = append(result, evidence)
	}
	local, err := captureProcessEvidence(phase, "local-daemon", localDaemonPID)
	if err != nil {
		return nil, err
	}
	result = append(result, local)
	for _, observer := range r.observers {
		state, _ := processState(observer.pid)
		result = append(result, goldenProcessEvidence{
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Phase: phase,
			Label: "observer-" + observer.label, ProcessID: observer.pid, ProcessStates: []string{state},
		})
	}
	r.summary.ProcessSnapshots = append(r.summary.ProcessSnapshots, result...)
	return result, nil
}

func captureProcessEvidence(phase, label string, pid int) (goldenProcessEvidence, error) {
	if pid <= 0 {
		return goldenProcessEvidence{}, failGolden("process_id_missing")
	}
	pids := descendantPIDs(pid)
	if len(pids) == 0 {
		return goldenProcessEvidence{}, failGolden("process_tree_missing")
	}
	var socketIDs, remoteIDs, states []string
	for _, processID := range pids {
		state, err := processState(processID)
		if err != nil {
			return goldenProcessEvidence{}, err
		}
		if strings.HasPrefix(state, "T") {
			return goldenProcessEvidence{}, failGolden("paused_process_detected")
		}
		states = append(states, state)
		command := exec.Command("lsof", "-nP", "-a", "-p", strconv.Itoa(processID), "-iTCP", "-sTCP:ESTABLISHED", "-FfnT")
		output, err := command.Output()
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return goldenProcessEvidence{}, failGolden("socket_snapshot_failed")
			}
		}
		for _, line := range strings.Split(string(output), "\n") {
			if len(line) < 2 || line[0] != 'n' {
				continue
			}
			name := strings.TrimSpace(line[1:])
			if name == "" {
				continue
			}
			hash := sha256.Sum256([]byte(strconv.Itoa(processID) + "\x00" + name))
			id := hex.EncodeToString(hash[:])
			socketIDs = append(socketIDs, id)
			if socketDestinationIsRemote(name) {
				remoteIDs = append(remoteIDs, id)
			}
		}
	}
	sort.Strings(socketIDs)
	sort.Strings(remoteIDs)
	return goldenProcessEvidence{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Phase: phase, Label: label,
		ProcessID: pid, DescendantPIDs: pids, ProcessStates: states, SocketIDs: deduplicateStrings(socketIDs),
		RemoteSocketIDs: deduplicateStrings(remoteIDs),
	}, nil
}

func processState(pid int) (string, error) {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "state=").Output()
	if err != nil {
		return "", failGolden("process_state_missing")
	}
	state := strings.TrimSpace(string(output))
	if fields := strings.Fields(state); len(fields) > 0 {
		state = fields[0]
	}
	if state == "" {
		return "", failGolden("process_state_missing")
	}
	return state, nil
}

func descendantPIDs(root int) []int {
	seen := map[int]bool{}
	queue := []int{root}
	var result []int
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if pid <= 0 || seen[pid] {
			continue
		}
		seen[pid] = true
		result = append(result, pid)
		output, err := exec.Command("pgrep", "-P", strconv.Itoa(pid)).Output()
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(string(output)) {
			child, parseErr := strconv.Atoi(field)
			if parseErr == nil && child > 0 {
				queue = append(queue, child)
			}
		}
	}
	sort.Ints(result)
	return result
}

func socketDestinationIsRemote(name string) bool {
	_, destination, found := strings.Cut(name, "->")
	if !found {
		return false
	}
	destination = strings.ToLower(strings.TrimSpace(destination))
	return !(strings.HasPrefix(destination, "127.") ||
		strings.HasPrefix(destination, "[::1]") ||
		strings.HasPrefix(destination, "localhost"))
}

func deduplicateStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

func requireLocalEgress(evidence []goldenProcessEvidence) error {
	for _, item := range evidence {
		if item.Label == "local-daemon" {
			if len(item.RemoteSocketIDs) == 0 {
				return failGolden("local_egress_socket_missing")
			}
			return nil
		}
	}
	return failGolden("local_daemon_evidence_missing")
}

func evidenceByLabel(items []goldenProcessEvidence) map[string]goldenProcessEvidence {
	result := make(map[string]goldenProcessEvidence)
	for _, item := range items {
		result[item.Label] = item
	}
	return result
}

func requireStableSessionSockets(sessions []*goldenSession, before, after map[string]goldenProcessEvidence) error {
	for _, session := range sessions {
		left, leftOK := before[session.label]
		right, rightOK := after[session.label]
		if !leftOK || !rightOK || len(left.SocketIDs) == 0 || len(right.SocketIDs) == 0 {
			return failGolden("session_socket_evidence_missing")
		}
		set := make(map[string]bool)
		for _, id := range left.SocketIDs {
			set[id] = true
		}
		stable := false
		for _, id := range right.SocketIDs {
			if set[id] {
				stable = true
				break
			}
		}
		if !stable {
			return failGolden("session_socket_identity_changed")
		}
	}
	return nil
}

func requireStableLocalEgress(before, after map[string]goldenProcessEvidence) error {
	left, leftOK := before["local-daemon"]
	right, rightOK := after["local-daemon"]
	if !leftOK || !rightOK || len(left.RemoteSocketIDs) == 0 || len(right.RemoteSocketIDs) == 0 {
		return failGolden("local_egress_continuity_missing")
	}
	seen := make(map[string]bool)
	for _, id := range left.RemoteSocketIDs {
		seen[id] = true
	}
	for _, id := range right.RemoteSocketIDs {
		if seen[id] {
			return nil
		}
	}
	return failGolden("local_egress_socket_changed")
}

func processAlive(process *os.Process) bool {
	return processAlivePlatform(process)
}

func interruptCommand(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	interruptProcessGroup(command)
}

func stopAndWaitCommand(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	interruptProcessGroup(command)
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		killProcessGroup(command)
		<-done
	}
}

func (r *goldenRunner) stopAll() {
	r.mu.Lock()
	sessions := append([]*goldenSession(nil), r.sessions...)
	observers := append([]*runningGoldenObserver(nil), r.observers...)
	r.mu.Unlock()
	for _, session := range sessions {
		if !sessionDone(session) && session.command != nil {
			interruptCommand(session.command)
		}
	}
	for _, session := range sessions {
		if sessionDone(session) || session.command == nil {
			continue
		}
		select {
		case <-session.done:
		case <-time.After(time.Second):
			killProcessGroup(session.command)
			select {
			case <-session.done:
			case <-time.After(2 * time.Second):
			}
		}
	}
	for _, observer := range observers {
		observer.stop()
	}
	if r.probeCancel != nil {
		r.probeCancel()
	}
}

func buildGoldenSessionSummaries(
	initial, resumes, fresh []*goldenSession,
	before, after map[string]goldenProcessEvidence,
) []goldenSessionSummary {
	result := make([]goldenSessionSummary, 0, len(initial)+len(fresh))
	resumeByBase := make(map[string]*goldenSession)
	for _, session := range resumes {
		resumeByBase[strings.TrimSuffix(session.label, "-resume")] = session
	}
	for _, session := range initial {
		resume := resumeByBase[session.label]
		result = append(result, summarizeGoldenSession(session, resume, 2, before[session.label], after[session.label]))
	}
	for _, session := range fresh {
		result = append(result, summarizeGoldenSession(session, nil, 1, goldenProcessEvidence{}, goldenProcessEvidence{}))
	}
	return result
}

func summarizeGoldenSession(session, resume *goldenSession, expectedRequests int, before, after goldenProcessEvidence) goldenSessionSummary {
	requests, chunks, proxyErrors := session.observer.stats.snapshot()
	var responseRequestEvents []transportEvent
	for _, request := range requests {
		if request.Path == "/v1/responses" || request.Path == "/responses" {
			responseRequestEvents = append(responseRequestEvents, request)
		}
	}
	connections := make(map[string]bool)
	fallbacks := 0
	for _, request := range responseRequestEvents {
		connections[request.ConnectionID] = true
		if request.Transport != session.transport {
			fallbacks++
		}
	}
	var responseBytes int64
	stampsByRequest := make(map[string][]time.Time)
	for _, chunk := range chunks {
		if chunk.Kind != "response_chunk" || (chunk.Path != "/v1/responses" && chunk.Path != "/responses") {
			continue
		}
		responseBytes += chunk.Bytes
		stamp, _ := time.Parse(time.RFC3339Nano, chunk.Timestamp)
		if !stamp.IsZero() {
			stampsByRequest[chunk.RequestID] = append(stampsByRequest[chunk.RequestID], stamp)
		}
	}
	var maxGap time.Duration
	for _, stamps := range stampsByRequest {
		sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })
		for index := 1; index < len(stamps); index++ {
			if gap := stamps[index].Sub(stamps[index-1]); gap > maxGap {
				maxGap = gap
			}
		}
	}
	session.mu.Lock()
	threadID := session.threadID
	markerCount := session.markerCount
	issues := copyIssues(session.issues)
	exitCode := session.exitCode
	session.mu.Unlock()
	resumeMarkerCount, resumeNonceCount, resumeExit, resumeIssues := 0, 0, 0, 0
	if resume != nil {
		resume.mu.Lock()
		resumeMarkerCount = resume.markerCount
		resumeNonceCount = resume.nonceCount
		resumeExit = resume.exitCode
		resumeIssues = issueCount(resume.issues)
		resume.mu.Unlock()
	}
	nonzero := 0
	if exitCode != 0 {
		nonzero++
	}
	if resume != nil && resumeExit != 0 {
		nonzero++
	}
	duplicate := 0
	if markerCount > 1 {
		duplicate += markerCount - 1
	}
	if resumeMarkerCount > 1 {
		duplicate += resumeMarkerCount - 1
	}
	reconnects := len(connections) - expectedRequests
	if reconnects < 0 {
		reconnects = 0
	}
	retries := len(responseRequestEvents) - expectedRequests
	if retries < 0 {
		retries = 0
	}
	return goldenSessionSummary{
		Label: session.label, Route: session.route, Transport: session.transport,
		ProcessID: session.command.Process.Pid, ThreadIDHash: hashGoldenValue(threadID),
		NonceHash: hashGoldenValue(session.nonce), ResponseRequests: len(responseRequestEvents),
		ResponseConnections: len(connections), ResponseBytes: responseBytes,
		MaxChunkGapMillis: maxGap.Milliseconds(), MarkerCount: markerCount,
		ResumeMarkerCount: resumeMarkerCount, ResumeNonceCount: resumeNonceCount,
		RetryCount: retries, ReconnectCount: reconnects, FallbackCount: fallbacks,
		ErrorCount:       issueCount(issues) + resumeIssues + proxyErrors,
		NonzeroExitCount: nonzero, DuplicateMarkerCount: duplicate,
		SocketIDsBefore: before.SocketIDs, SocketIDsAfterRollback: after.SocketIDs,
	}
}

func copyIssues(source map[string]int) map[string]int {
	result := make(map[string]int, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func hashGoldenValue(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func validateGoldenSummary(summary goldenSummary, testMode bool) error {
	if summary.ReleasedVersion == "" || len(summary.ReleasedSHA256) != 64 || !summary.ReleaseChecksumVerified || summary.ReleasePlatform != "darwin/arm64" {
		return failGolden("release_evidence_incomplete")
	}
	if !testMode && summary.ReleasedVersion == "test-override" {
		return failGolden("candidate_client_forbidden")
	}
	for _, action := range []goldenActionSummary{summary.Activation, summary.Rollback, summary.OldGenerationCleanup} {
		if action.StartedAt == "" || action.FinishedAt == "" || action.ExitCode != 0 {
			return failGolden("action_evidence_incomplete")
		}
	}
	if summary.ProbeFrequencyHz != 10 || len(summary.Health) != 4 {
		return failGolden("health_evidence_incomplete")
	}
	for _, health := range summary.Health {
		if health.Label == "" || health.Samples == 0 || health.Failures != 0 || health.MaxStartGapMillis > 250 {
			return failGolden("health_evidence_incomplete")
		}
	}
	expected := map[string]struct {
		route, transport string
	}{
		"direct-websocket":  {route: "direct-hosted", transport: "websocket"},
		"direct-http":       {route: "direct-hosted", transport: "http"},
		"local-websocket":   {route: "local-egress", transport: "websocket"},
		"local-http":        {route: "local-egress", transport: "http"},
		"activation-direct": {route: "direct-hosted", transport: "websocket"},
		"activation-local":  {route: "local-egress", transport: "websocket"},
	}
	if len(summary.Sessions) != len(expected) {
		return failGolden("session_evidence_incomplete")
	}
	for _, session := range summary.Sessions {
		want, ok := expected[session.Label]
		if !ok || session.Route != want.route || session.Transport != want.transport ||
			session.ProcessID <= 0 || len(session.ThreadIDHash) != 64 || len(session.NonceHash) != 64 ||
			session.ResponseRequests == 0 || session.ResponseConnections == 0 || session.ResponseBytes <= 0 ||
			session.MarkerCount != 1 || session.RetryCount != 0 || session.ReconnectCount != 0 ||
			session.FallbackCount != 0 || session.ErrorCount != 0 || session.NonzeroExitCount != 0 ||
			session.DuplicateMarkerCount != 0 {
			return failGolden("session_evidence_incomplete")
		}
		if strings.HasPrefix(session.Label, "activation-") {
			if session.ResumeMarkerCount != 0 || session.ResumeNonceCount != 0 {
				return failGolden("activation_session_evidence_invalid")
			}
		} else if session.ResumeMarkerCount != 1 || session.ResumeNonceCount != 1 ||
			len(session.SocketIDsBefore) == 0 || len(session.SocketIDsAfterRollback) == 0 {
			return failGolden("resume_evidence_incomplete")
		}
		delete(expected, session.Label)
	}
	if len(expected) != 0 || !summary.FreshLocalLeaseObserved || summary.DeploymentEnvironmentRead {
		return failGolden("golden_evidence_incomplete")
	}
	if len(summary.ProcessSnapshots) == 0 {
		return failGolden("process_evidence_incomplete")
	}
	requiredProcessEvidence := make(map[string]bool)
	initialLabels := []string{"direct-websocket", "direct-http", "local-websocket", "local-http", "local-daemon"}
	initialObservers := []string{"observer-local-lease", "observer-direct-websocket", "observer-direct-http", "observer-local-websocket", "observer-local-http"}
	for _, phase := range []string{"before-activation", "after-activation", "after-rollback"} {
		for _, label := range append(append([]string(nil), initialLabels...), initialObservers...) {
			requiredProcessEvidence[phase+"\x00"+label] = false
		}
		if phase != "before-activation" {
			requiredProcessEvidence[phase+"\x00observer-activation-direct"] = false
			requiredProcessEvidence[phase+"\x00observer-activation-local"] = false
		}
	}
	for _, item := range summary.ProcessSnapshots {
		key := item.Phase + "\x00" + item.Label
		if _, required := requiredProcessEvidence[key]; !required || item.ProcessID <= 0 || item.Timestamp == "" {
			return failGolden("process_evidence_incomplete")
		}
		if len(item.ProcessStates) == 0 {
			return failGolden("process_state_missing")
		}
		for _, state := range item.ProcessStates {
			if state == "" || strings.HasPrefix(state, "T") {
				return failGolden("paused_process_detected")
			}
		}
		if !strings.HasPrefix(item.Label, "observer-") && (len(item.DescendantPIDs) == 0 || len(item.SocketIDs) == 0) {
			return failGolden("socket_evidence_incomplete")
		}
		if item.Label == "local-daemon" && len(item.RemoteSocketIDs) == 0 {
			return failGolden("egress_evidence_incomplete")
		}
		requiredProcessEvidence[key] = true
	}
	for _, present := range requiredProcessEvidence {
		if !present {
			return failGolden("process_evidence_incomplete")
		}
	}
	return nil
}

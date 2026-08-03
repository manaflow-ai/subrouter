package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"debug/buildinfo"
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
	goldenProbeInterval                   = 100 * time.Millisecond
	goldenProbeScheduleTolerance          = 50 * time.Millisecond
	goldenHTTPTimeout                     = 900 * time.Millisecond
	goldenLocalEgressBindTimeout          = 2 * time.Second
	goldenActionEvidenceLimit             = 256 << 10
	goldenActivationLimit                 = 30 * time.Second
	goldenRetirementLimit                 = 30 * time.Second
	goldenChunkGapFloor                   = 5 * time.Second
	goldenRSSLimitBytes             int64 = 192 << 20
	goldenBaselineChunkSamples            = 20
	goldenProcessSampleInterval           = 20 * time.Millisecond
	goldenProcessSampleMaxGap             = 100 * time.Millisecond
	goldenPinnedPredecessorVersion        = "0.1.51"
	goldenPinnedPredecessorSHA256         = "74f4bfbbf6b8dcbe0509eaaa9f63b1eb688358a749ed3b451066e146591d2582"
	goldenPinnedPredecessorRevision       = "5eacb5411c0bd4a24f4e422d6366fa7bfd1843c8"
)

// goldenTestHooks are set only by same-package deterministic tests. Production
// binaries have no environment or command-line switch that enables them.
var goldenTestHooks struct {
	enabled             bool
	releaseAPI          string
	releaseDownloadRoot string
	socketEndpoint      string
	evidenceValidator   string
}

type goldenOptions struct {
	cloudConfig       string
	codexHome         string
	codexBinary       string
	releasedVersion   string
	predecessorSHA256 string
	candidateTag      string
	candidateSHA256   string
	candidateRevision string
	evidenceValidator string
	predecessorClient string
	releasedClient    string
	artifactDir       string
	model             string
	streamLines       int
	timeout           time.Duration
	migrationPrepare  []string
	migrationSwitch   []string
	legacyRetirement  []string
	activation        []string
	rollback          []string
	oldGenerationTest []string
}

func parseGoldenArgs(args []string) (goldenOptions, error) {
	var options goldenOptions
	actionNames := []string{
		"--migration-prepare", "--migration-switch", "--legacy-retirement",
		"--activate", "--rollback", "--old-generation-check",
	}
	positions := make(map[string]int, len(actionNames))
	for index, arg := range args {
		for _, name := range actionNames {
			if arg == name {
				if _, exists := positions[name]; exists {
					return options, fmt.Errorf("%s may appear only once", name)
				}
				positions[name] = index
			}
		}
	}
	previous := -1
	for index, name := range actionNames {
		position, ok := positions[name]
		if !ok || position <= previous || (index+1 < len(actionNames) && position+1 == positions[actionNames[index+1]]) ||
			(index+1 == len(actionNames) && position+1 == len(args)) {
			return options, errors.New("all migration and slot actions must be supplied in canonical order with a command")
		}
		previous = position
	}
	flags := flag.NewFlagSet("golden", flag.ContinueOnError)
	flags.StringVar(&options.cloudConfig, "cloud-config", "", "source cmux.com cloud config")
	flags.StringVar(&options.codexHome, "codex-home", "", "source Codex home containing auth.json")
	flags.StringVar(&options.codexBinary, "codex-bin", "codex", "Codex CLI binary")
	flags.StringVar(&options.releasedVersion, "predecessor-version", "", "explicit predecessor Subrouter release version")
	flags.StringVar(&options.releasedVersion, "released-version", "", "deprecated alias for --predecessor-version")
	flags.StringVar(&options.predecessorSHA256, "predecessor-sha256", "", "expected predecessor release asset SHA-256")
	flags.StringVar(&options.candidateTag, "candidate-tag", "", "immutable candidate release tag")
	flags.StringVar(&options.candidateSHA256, "candidate-sha256", "", "verified Linux candidate asset SHA-256")
	flags.StringVar(&options.candidateRevision, "candidate-revision", "", "verified candidate source revision")
	flags.StringVar(&options.evidenceValidator, "deploy-evidence-validator", "", "canonical deployment evidence validator")
	flags.StringVar(&options.predecessorClient, "predecessor-client", "", "locally verified pinned predecessor release asset")
	flags.StringVar(&options.releasedClient, "released-client", "", "test-only released client override")
	flags.StringVar(&options.artifactDir, "artifact-dir", "", "content-blind evidence directory")
	flags.StringVar(&options.model, "model", "gpt-5.6-sol", "Codex model")
	flags.IntVar(&options.streamLines, "stream-lines", 4000, "numbered lines requested from each continuity turn")
	flags.DurationVar(&options.timeout, "timeout", 20*time.Minute, "overall golden gate timeout")
	if err := flags.Parse(args[:positions[actionNames[0]]]); err != nil {
		return options, err
	}
	if flags.NArg() != 0 {
		return options, fmt.Errorf("unexpected golden arguments")
	}
	actions := make([][]string, len(actionNames))
	for index, name := range actionNames {
		end := len(args)
		if index+1 < len(actionNames) {
			end = positions[actionNames[index+1]]
		}
		actions[index] = append([]string(nil), args[positions[name]+1:end]...)
	}
	options.migrationPrepare, options.migrationSwitch, options.legacyRetirement = actions[0], actions[1], actions[2]
	options.activation, options.rollback, options.oldGenerationTest = actions[3], actions[4], actions[5]
	if options.streamLines < 100 && !goldenTestHooks.enabled {
		return options, errors.New("--stream-lines must be at least 100")
	}
	if options.timeout <= 0 {
		return options, errors.New("--timeout must be positive")
	}
	if !goldenTestHooks.enabled {
		version := strings.TrimPrefix(strings.TrimSpace(options.releasedVersion), "v")
		if version != goldenPinnedPredecessorVersion ||
			strings.ToLower(strings.TrimSpace(options.predecessorSHA256)) != goldenPinnedPredecessorSHA256 {
			return options, errors.New("the golden predecessor must be pinned v0.1.51 with its Darwin SHA-256")
		}
		if strings.TrimSpace(options.evidenceValidator) == "" {
			return options, errors.New("--deploy-evidence-validator is required")
		}
		if strings.TrimSpace(options.candidateTag) != "v0.1.52" || !validGoldenSHA256(options.candidateSHA256) ||
			len(strings.TrimSpace(options.candidateRevision)) != 40 {
			return options, errors.New("the golden candidate must be the verified immutable v0.1.52 release")
		}
	} else if options.evidenceValidator == "" {
		options.evidenceValidator = goldenTestHooks.evidenceValidator
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
	Passed                      bool                    `json:"passed"`
	Failure                     string                  `json:"failure,omitempty"`
	StartedAt                   string                  `json:"started_at"`
	CompletedAt                 string                  `json:"completed_at"`
	ReleasedVersion             string                  `json:"released_version"`
	ReleasedSHA256              string                  `json:"released_sha256"`
	ExpectedPredecessorSHA256   string                  `json:"expected_predecessor_sha256"`
	PredecessorRevision         string                  `json:"predecessor_revision"`
	PredecessorRevisionVerified bool                    `json:"predecessor_revision_verified"`
	ReleaseChecksumVerified     bool                    `json:"release_checksum_verified"`
	ReleasePlatform             string                  `json:"release_platform"`
	ProbeFrequencyHz            int                     `json:"probe_frequency_hz"`
	MigrationPreparation        goldenActionSummary     `json:"migration_preparation"`
	MigrationRehearsalCutover   goldenActionSummary     `json:"migration_rehearsal_cutover"`
	MigrationRollback           goldenActionSummary     `json:"migration_rollback"`
	MigrationFinalCutover       goldenActionSummary     `json:"migration_final_cutover"`
	LegacyCleanup               goldenActionSummary     `json:"legacy_cleanup"`
	Activation                  goldenActionSummary     `json:"activation"`
	Rollback                    goldenActionSummary     `json:"rollback"`
	OldGenerationCleanup        goldenActionSummary     `json:"old_generation_cleanup"`
	FinalActivation             goldenActionSummary     `json:"final_activation"`
	FinalOldGenerationCleanup   goldenActionSummary     `json:"final_old_generation_cleanup"`
	Sessions                    []goldenSessionSummary  `json:"sessions"`
	Health                      []goldenProbeSummary    `json:"health"`
	ProcessSnapshots            []goldenProcessEvidence `json:"process_snapshots"`
	FreshLocalLeaseObserved     bool                    `json:"fresh_local_lease_observed"`
	LegacyBrokerLeaseObserved   bool                    `json:"legacy_broker_lease_observed"`
	PrivateWorkspaceRemoved     bool                    `json:"private_workspace_removed"`
	DeploymentEnvironmentRead   bool                    `json:"deployment_environment_recorded"`
	LocalDaemonPeakRSSBytes     int64                   `json:"local_daemon_peak_rss_bytes"`
	LocalDaemonRSSSamples       int                     `json:"local_daemon_rss_samples"`
	LocalDaemonProcessSamples   int                     `json:"local_daemon_process_samples"`
	LocalDaemonMaxSampleGapMS   int64                   `json:"local_daemon_max_process_sample_gap_ms"`
	LocalDaemonPausedSamples    int                     `json:"local_daemon_paused_samples"`
}

type goldenActionSummary struct {
	EvidenceType             string `json:"evidence_type"`
	EvidenceFile             string `json:"evidence_file"`
	EvidenceSHA256           string `json:"evidence_sha256"`
	LinkedEvidenceSHA256     string `json:"linked_evidence_sha256,omitempty"`
	Mode                     string `json:"mode"`
	StartedAt                string `json:"started_at"`
	FinishedAt               string `json:"finished_at"`
	DurationMillis           int64  `json:"duration_ms"`
	RequestedAt              string `json:"requested_at,omitempty"`
	ActivatedAt              string `json:"activated_at,omitempty"`
	PhaseDurationMillis      int64  `json:"phase_duration_ms,omitempty"`
	ExitCode                 int    `json:"exit_code"`
	EvidenceValid            bool   `json:"evidence_valid"`
	ReleaseTag               string `json:"release_tag,omitempty"`
	ReleaseSourceRevision    string `json:"release_source_revision,omitempty"`
	FromSlot                 string `json:"from_slot,omitempty"`
	ToSlot                   string `json:"to_slot,omitempty"`
	ActiveSlot               string `json:"active_slot,omitempty"`
	FromGenerationIDHash     string `json:"from_generation_id_sha256"`
	ToGenerationIDHash       string `json:"to_generation_id_sha256"`
	ActiveGenerationIDHash   string `json:"active_generation_id_sha256"`
	FromReleaseSHA256        string `json:"from_release_sha256"`
	ToReleaseSHA256          string `json:"to_release_sha256"`
	RestartDelta             int64  `json:"restart_delta"`
	OOMDelta                 int64  `json:"oom_delta"`
	OldGenerationIDHash      string `json:"old_generation_id_sha256"`
	OldGenerationActive      bool   `json:"old_generation_active"`
	OldGenerationAccepting   bool   `json:"old_generation_accepting"`
	OldGenerationConnections int64  `json:"old_generation_connections"`
	ReportedRetiredWithinMS  int64  `json:"reported_retired_within_ms"`
	ObservedRetiredWithinMS  int64  `json:"observed_retired_within_ms"`
	ServerRSSBytes           int64  `json:"server_rss_bytes"`
	ServerOldPeakRSSBytes    int64  `json:"server_old_peak_rss_bytes,omitempty"`
	ServerNewPeakRSSBytes    int64  `json:"server_new_peak_rss_bytes,omitempty"`
	ServerFrontPeakRSSBytes  int64  `json:"server_front_peak_rss_bytes,omitempty"`
	LastConnectionClosedAt   string `json:"last_connection_closed_at,omitempty"`
	AbsentAt                 string `json:"absent_at,omitempty"`

	canonical          *goldenDeployEvidence    `json:"-"`
	migrationCanonical *goldenMigrationEvidence `json:"-"`
}

type goldenSessionSummary struct {
	Label                   string   `json:"label"`
	Route                   string   `json:"route"`
	Transport               string   `json:"transport"`
	ProcessID               int      `json:"process_id"`
	ThreadIDHash            string   `json:"thread_id_sha256"`
	NonceHash               string   `json:"nonce_sha256"`
	ResponseRequests        int      `json:"response_requests"`
	ResponseConnections     int      `json:"response_connections"`
	ResponseTransportSocket string   `json:"response_transport_socket_id"`
	TransportSocketStable   bool     `json:"transport_socket_stable"`
	ResponseBytes           int64    `json:"response_bytes"`
	MaxChunkGapMillis       int64    `json:"max_chunk_gap_ms"`
	PreDeployP99GapMillis   int64    `json:"pre_deploy_p99_chunk_gap_ms"`
	AllowedChunkGapMillis   int64    `json:"allowed_chunk_gap_ms"`
	DeployMaxChunkGapMillis int64    `json:"deploy_max_chunk_gap_ms"`
	PeakRSSBytes            int64    `json:"peak_rss_bytes"`
	RSSSamples              int      `json:"rss_samples"`
	ProcessSamples          int      `json:"process_samples"`
	MaxProcessSampleGapMS   int64    `json:"max_process_sample_gap_ms"`
	PausedProcessSamples    int      `json:"paused_process_samples"`
	MarkerCount             int      `json:"marker_count"`
	ResumeMarkerCount       int      `json:"resume_marker_count"`
	ResumeNonceCount        int      `json:"resume_nonce_count"`
	RetryCount              int      `json:"retry_count"`
	ReconnectCount          int      `json:"reconnect_count"`
	FallbackCount           int      `json:"fallback_count"`
	ErrorCount              int      `json:"error_count"`
	NonzeroExitCount        int      `json:"nonzero_exit_count"`
	DuplicateMarkerCount    int      `json:"duplicate_marker_count"`
	SocketIDsBefore         []string `json:"socket_ids_before"`
	SocketIDsAfterRollback  []string `json:"socket_ids_after_rollback"`
	LocalUpstreamSocket     string   `json:"local_upstream_socket_id,omitempty"`
	LocalEgressSocket       string   `json:"local_egress_socket_id,omitempty"`
	LocalEgressCorrelated   bool     `json:"local_egress_correlated,omitempty"`
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
	RSSBytes        int64    `json:"rss_bytes"`

	remoteSockets []goldenRemoteSocket
}

type releasedClient struct {
	path             string
	version          string
	sha256           string
	checksumVerified bool
	revision         string
	revisionVerified bool
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

	mu                  sync.Mutex
	observers           []*runningGoldenObserver
	sessions            []*goldenSession
	processes           []*exec.Cmd
	probeCancel         context.CancelFunc
	probeStats          *goldenProbeStats
	evidence            *jsonlRecorder
	localRSSMu          sync.Mutex
	localPeakRSS        int64
	localRSSSamples     int
	localRSSExceeded    bool
	localLastSample     time.Time
	localMaxSampleGap   time.Duration
	localPausedSamples  int
	localSampleFailures int
	localIssueMu        sync.Mutex
	localIssues         map[string]int
	localStderrDone     chan struct{}
	localEgressMu       sync.Mutex
	localEgressBindings []*goldenLocalEgressBinding
}

type goldenCloudConfig struct {
	Raw             map[string]any
	BrokerURL       string
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
		BrokerURL:       strings.TrimRight(stringValue("baseUrl"), "/"),
		HostedURL:       strings.TrimRight(stringValue("hostedUrl"), "/"),
		TenantKey:       stringValue("tenantKey"),
		LocalProxyToken: stringValue("localProxyToken"),
	}
	if config.BrokerURL == "" {
		config.BrokerURL = "https://cmux.com"
	}
	if config.HostedURL == "" || !validGoldenTenantKey(config.TenantKey) || config.LocalProxyToken == "" {
		return goldenCloudConfig{}, failGolden("cloud_config_not_ready")
	}
	if stringValue("accessToken") == "" || stringValue("refreshToken") == "" || stringValue("teamId") == "" {
		return goldenCloudConfig{}, failGolden("cloud_config_not_logged_in")
	}
	parsed, err := url.Parse(config.HostedURL)
	broker, brokerErr := url.Parse(config.BrokerURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" ||
		brokerErr != nil || broker.Scheme != "https" || broker.Host == "" || broker.User != nil ||
		(broker.Path != "" && broker.Path != "/") || broker.RawQuery != "" || broker.Fragment != "" ||
		strings.TrimRight(config.BrokerURL, "/") != "https://cmux.com" {
		if !goldenTestHooks.enabled {
			return goldenCloudConfig{}, failGolden("cloud_endpoint_invalid")
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

func isGoldenLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeGoldenConfig(path string, source map[string]any, credentialSource, hostedURL, brokerURL string) error {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	clone["credentialSource"] = credentialSource
	if hostedURL != "" {
		clone["hostedUrl"] = hostedURL
	}
	if brokerURL != "" {
		clone["baseUrl"] = brokerURL
	}
	data, err := json.Marshal(clone)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writePrivateFile(path, data)
}

func (r *goldenRunner) run(ctx context.Context) (runErr error) {
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
	r.summary.ExpectedPredecessorSHA256 = strings.ToLower(strings.TrimSpace(r.options.predecessorSHA256))
	r.summary.ReleaseChecksumVerified = client.checksumVerified
	r.summary.PredecessorRevision = client.revision
	r.summary.PredecessorRevisionVerified = client.revisionVerified
	if r.options.predecessorSHA256 != "" && client.sha256 != strings.ToLower(strings.TrimSpace(r.options.predecessorSHA256)) {
		return failGolden("predecessor_checksum_mismatch")
	}

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
	brokerOrigin, err := url.Parse(cloudConfig.BrokerURL)
	if err != nil {
		return failGolden("broker_url_invalid")
	}
	leaseObserver, err := r.startObserver("local-lease", brokerOrigin)
	if err != nil {
		return err
	}
	defer r.stopAll()
	directConfigPath := filepath.Join(r.privateRoot, "hosted-cloud.json")
	teamConfigPath := filepath.Join(r.privateRoot, "team-cloud.json")
	if err := writeGoldenConfig(directConfigPath, cloudConfig.Raw, "hosted", cloudConfig.HostedURL, cloudConfig.BrokerURL); err != nil {
		return failGolden("private_config_write_failed")
	}
	if err := writeGoldenConfig(teamConfigPath, cloudConfig.Raw, "team", cloudConfig.HostedURL, leaseObserver.baseURL); err != nil {
		return failGolden("private_config_write_failed")
	}

	localDaemon, localOrigin, err := r.startLocalDaemon(ctx, client.path, teamConfigPath)
	if err != nil {
		return err
	}
	localDaemonStopped := false
	defer func() {
		if localDaemonStopped {
			return
		}
		stopAndWaitCommand(localDaemon)
		if err := r.waitGoldenLocalDaemonStderr(); err != nil && runErr == nil {
			runErr = err
		}
		if err := r.requireGoldenLocalDaemonTransportClean(); err != nil && runErr == nil {
			runErr = err
		}
	}()
	localRSSCtx, cancelLocalRSS := context.WithCancel(ctx)
	localRSSDone := make(chan struct{})
	localRSSStopped := false
	defer func() {
		if !localRSSStopped {
			cancelLocalRSS()
			<-localRSSDone
		}
	}()
	go func() {
		defer close(localRSSDone)
		r.sampleLocalDaemonRSS(localRSSCtx, localDaemon.Process.Pid)
	}()

	probeCtx, probeCancel := context.WithCancel(ctx)
	r.probeCancel = probeCancel
	probeStats, err := r.startProbes(probeCtx, hostedOrigin, localOrigin)
	if err != nil {
		return err
	}
	r.probeStats = probeStats
	defer probeCancel()
	migration, err := r.runMigrationCycle(ctx, goldenCycleInputs{
		name: "migration", clientPath: client.path, authData: authData, cloud: cloudConfig,
		directConfigPath: directConfigPath, teamConfigPath: teamConfigPath,
		hostedOrigin: hostedOrigin, localOrigin: localOrigin, leaseObserver: leaseObserver,
		localDaemonPID: localDaemon.Process.Pid,
	})
	if err != nil {
		return err
	}
	if err := requireStableLocalEgress(migration.before, migration.after); err != nil {
		return err
	}
	if err := requireBoundLocalEgress(migration.initial, migration.before); err != nil {
		return err
	}
	if err := requireBoundLocalEgress(migration.initial, migration.after); err != nil {
		return err
	}
	if err := r.requireGoldenLocalDaemonTransportClean(); err != nil {
		return err
	}
	r.summary.MigrationPreparation = migration.preparation
	r.summary.MigrationRehearsalCutover = migration.rehearsalCutover
	r.summary.MigrationRollback = migration.rollback
	r.summary.MigrationFinalCutover = migration.finalCutover
	r.summary.LegacyCleanup = migration.cleanup
	if err := validateGoldenCounterContinuity(*r.summary); err != nil {
		return err
	}

	rehearsal, err := r.runRehearsalCycle(ctx, goldenCycleInputs{
		name: "rehearsal", clientPath: client.path, authData: authData, cloud: cloudConfig,
		directConfigPath: directConfigPath, teamConfigPath: teamConfigPath,
		hostedOrigin: hostedOrigin, localOrigin: localOrigin, leaseObserver: leaseObserver,
		localDaemonPID: localDaemon.Process.Pid,
	})
	if err != nil {
		return err
	}
	if migration.preparation.migrationCanonical == nil ||
		migration.preparation.migrationCanonical.Release.Tag != rehearsal.activation.ReleaseTag ||
		migration.preparation.migrationCanonical.Release.SHA256 != rehearsal.activation.ToReleaseSHA256 ||
		migration.preparation.migrationCanonical.Release.SourceRevision != rehearsal.activation.ReleaseSourceRevision {
		return failGolden("migration_slot_candidate_mismatch")
	}
	r.summary.Activation = rehearsal.activation
	r.summary.Rollback = rehearsal.rollback
	r.summary.OldGenerationCleanup = rehearsal.cleanup
	if err := validateGoldenCounterContinuity(*r.summary); err != nil {
		return err
	}

	final, err := r.runFinalCycle(ctx, goldenCycleInputs{
		name: "final", clientPath: client.path, authData: authData, cloud: cloudConfig,
		directConfigPath: directConfigPath, teamConfigPath: teamConfigPath,
		hostedOrigin: hostedOrigin, localOrigin: localOrigin, leaseObserver: leaseObserver,
		localDaemonPID: localDaemon.Process.Pid,
	}, rehearsal.activation)
	if err != nil {
		return err
	}
	r.summary.FinalActivation = final.activation
	r.summary.FinalOldGenerationCleanup = final.cleanup
	if err := validateGoldenCounterContinuity(*r.summary); err != nil {
		return err
	}
	if observerRequestCount(leaseObserver.stats, "/v1/responses") != 0 ||
		observerRequestCount(leaseObserver.stats, "/responses") != 0 ||
		observerRequestCount(leaseObserver.stats, "/_subrouter/leases") != 0 {
		return failGolden("local_route_bypassed_daemon")
	}
	probeCancel()
	probeStats.wait()
	r.summary.Health = probeStats.summaries()
	if r.evidence.failure() != nil || probeStats.record.failure() != nil {
		return failGolden("evidence_write_failed")
	}
	if err := probeStats.validateInterval(parseSummaryTime(r.summary.MigrationPreparation.StartedAt), parseSummaryTime(r.summary.FinalOldGenerationCleanup.FinishedAt)); err != nil {
		return err
	}
	r.summary.Sessions = append(
		buildGoldenSessionSummaries(migration.initial, migration.resumes, migration.fresh, migration.before, migration.after),
		buildGoldenSessionSummaries(rehearsal.initial, rehearsal.resumes, rehearsal.fresh, rehearsal.before, rehearsal.after)...,
	)
	r.summary.Sessions = append(
		r.summary.Sessions,
		buildGoldenSessionSummaries(final.initial, final.resumes, final.fresh, final.before, final.after)...,
	)
	cancelLocalRSS()
	<-localRSSDone
	localRSSStopped = true
	if err := r.finalizeLocalDaemonRSS(); err != nil {
		return err
	}
	stopAndWaitCommand(localDaemon)
	localDaemonStopped = true
	if err := r.waitGoldenLocalDaemonStderr(); err != nil {
		return err
	}
	if err := r.requireGoldenLocalDaemonTransportClean(); err != nil {
		return err
	}
	return nil
}

func parseSummaryTime(raw string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, raw)
	return parsed
}

type goldenCycleInputs struct {
	name                             string
	clientPath                       string
	authData                         []byte
	cloud                            goldenCloudConfig
	directConfigPath, teamConfigPath string
	hostedOrigin, localOrigin        *url.URL
	leaseObserver                    *runningGoldenObserver
	localDaemonPID                   int
}

type goldenCycleResult struct {
	initial, resumes, fresh []*goldenSession
	before, after           map[string]goldenProcessEvidence
	activation              goldenActionSummary
	rollback                goldenActionSummary
	cleanup                 goldenActionSummary
}

func (r *goldenRunner) startCycleInitialSessions(ctx context.Context, inputs goldenCycleInputs) ([]*goldenSession, error) {
	specs := []struct {
		suffix, route, transport string
	}{
		{suffix: "direct-websocket", route: "direct-hosted", transport: "websocket"},
		{suffix: "direct-http", route: "direct-hosted", transport: "http"},
		{suffix: "local-websocket", route: "local-egress", transport: "websocket"},
		{suffix: "local-http", route: "local-egress", transport: "http"},
	}
	result := make([]*goldenSession, 0, len(specs))
	for _, spec := range specs {
		upstream := inputs.hostedOrigin
		if spec.route == "local-egress" {
			upstream = inputs.localOrigin
		}
		label := inputs.name + "-" + spec.suffix
		var daemonBefore goldenProcessEvidence
		leaseBefore := 0
		if spec.route == "local-egress" {
			var err error
			daemonBefore, err = captureProcessEvidence(label+"-egress-before", "local-daemon", inputs.localDaemonPID)
			if err != nil {
				return nil, err
			}
			leaseBefore = observerRequestCount(inputs.leaseObserver.stats, "/api/subrouter/leases")
		}
		observation, err := r.startObserver(label, upstream)
		if err != nil {
			return nil, err
		}
		session, err := r.startInitialSession(
			ctx, inputs.clientPath, inputs.authData, inputs.cloud, inputs.directConfigPath,
			inputs.teamConfigPath, observation, label, spec.route, spec.transport,
		)
		if err != nil {
			return nil, err
		}
		result = append(result, session)
		if spec.route == "local-egress" {
			if err := waitGoldenInitialReady(ctx, session); err != nil {
				return nil, err
			}
			if err := requireGoldenLocalObserverPath(session); err != nil {
				return nil, err
			}
			if _, err := r.waitAndBindGoldenLocalEgress(
				ctx, session, inputs.leaseObserver, leaseBefore, daemonBefore,
				inputs.localDaemonPID, label+"-egress-bound",
			); err != nil {
				return nil, err
			}
		}
	}
	for _, session := range result {
		if err := waitGoldenInitialReady(ctx, session); err != nil {
			return nil, err
		}
	}
	if err := requireUniqueThreadIDs(result); err != nil {
		return nil, err
	}
	if err := validateObserverTurns(result, 1); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *goldenRunner) runRehearsalCycle(ctx context.Context, inputs goldenCycleInputs) (goldenCycleResult, error) {
	var result goldenCycleResult
	initial, err := r.startCycleInitialSessions(ctx, inputs)
	if err != nil {
		return result, err
	}
	result.initial = initial
	beforeEvidence, err := r.capturePhase(inputs.name+"-before-activation", initial, inputs.localDaemonPID)
	if err != nil {
		return result, err
	}
	if err := requireLocalEgress(beforeEvidence); err != nil {
		return result, err
	}
	result.before = evidenceByLabel(beforeEvidence)
	if err := requireBoundLocalEgress(initial, result.before); err != nil {
		return result, err
	}

	spanningLocal, spanningBefore, leaseBefore, err := r.startSpanningLocalSession(ctx, inputs)
	if err != nil {
		return result, err
	}
	result.fresh = []*goldenSession{spanningLocal}
	localSessions := append(append([]*goldenSession{}, initial...), spanningLocal)
	if err := requireBoundLocalEgress(localSessions, spanningBefore); err != nil {
		return result, err
	}
	localEgressMonitor, err := startGoldenLocalEgressMonitor(
		ctx, r, inputs.localDaemonPID, inputs.name+"-local-egress-window", spanningBefore["local-daemon"],
	)
	if err != nil {
		return result, err
	}
	localEgressMonitorStopped := false
	defer func() {
		if !localEgressMonitorStopped {
			_ = localEgressMonitor.stopAndValidate()
		}
	}()
	baselineEnd := time.Now().UTC()
	monitors, err := startGoldenContinuityMonitors(r, localSessions, baselineEnd)
	if err != nil {
		return result, err
	}
	monitorsStopped := false
	defer func() {
		if !monitorsStopped {
			cancelGoldenContinuityMonitors(monitors)
		}
	}()
	var postDirect *goldenSession
	var during map[string]goldenProcessEvidence
	result.activation, postDirect, during, err = r.runSlotActivationWithAck(
		ctx, "rehearsal", inputs, initial, spanningLocal, result.before, spanningBefore, monitors, localEgressMonitor,
	)
	if err != nil {
		return result, err
	}
	result.fresh = append(result.fresh, postDirect)
	if err := validateGoldenTransitionAction(result.activation, true); err != nil {
		return result, err
	}
	if err := validateGoldenProvenance(r.summary.ReleasedSHA256, result.activation); err != nil {
		return result, err
	}
	if err := r.validateGoldenSlotCandidate(result.activation); err != nil {
		return result, err
	}
	activated := parseSummaryTime(result.activation.ActivatedAt)
	if err := requireGoldenSessionSpans(spanningLocal, activated); err != nil {
		return result, err
	}
	_, localStarted, _ := goldenSessionRequestWindow(spanningLocal)
	if err := requireGoldenLeaseWindow(inputs.leaseObserver, localStarted, activated, leaseBefore); err != nil {
		return result, err
	}
	r.summary.FreshLocalLeaseObserved = true
	r.summary.LegacyBrokerLeaseObserved = true
	all := append(append([]*goldenSession{}, initial...), result.fresh...)
	if err := requireSessionsRunning(all, "rehearsal_activation"); err != nil {
		return result, err
	}
	result.rollback = r.runEvidenceAction(ctx, goldenEvidenceActionOptions{
		label: inputs.name + "-rollback", argv: r.options.rollback, expect: "slot-rollback",
		evidenceName: inputs.name + "-slot-rollback.json", linkFlag: "--activation-evidence",
		linkPath:    filepath.Join(r.artifactDir, result.activation.EvidenceFile),
		environment: map[string]string{"SUBROUTER_EXPECTED_RETIRING_CONNECTIONS": "1"},
	})
	if err := validateGoldenTransitionAction(result.rollback, false); err != nil {
		return result, err
	}
	if err := validateGoldenRollback(result.activation, result.rollback); err != nil {
		return result, err
	}
	if err := waitGoldenContinuityBoundary(ctx, monitors, parseSummaryTime(result.rollback.ActivatedAt)); err != nil {
		return result, err
	}
	if err := requireSessionsRunning(all, "rehearsal_rollback"); err != nil {
		return result, err
	}
	monitorErr := localEgressMonitor.stopAndValidate()
	localEgressMonitorStopped = true
	if monitorErr != nil {
		return result, monitorErr
	}
	afterEvidence, err := r.capturePhase(inputs.name+"-after-rollback", all, inputs.localDaemonPID)
	if err != nil {
		return result, err
	}
	after := evidenceByLabel(afterEvidence)
	result.after = after
	if err := requireStableSessionSockets(initial, result.before, after); err != nil {
		return result, err
	}
	if err := requireStableSessionSockets([]*goldenSession{spanningLocal}, spanningBefore, after); err != nil {
		return result, err
	}
	if err := requireStableSessionSockets([]*goldenSession{postDirect}, during, after); err != nil {
		return result, err
	}
	if err := requireStableLocalEgress(during, after); err != nil {
		return result, err
	}
	if err := requireBoundLocalEgress(all, after); err != nil {
		return result, err
	}
	if err := r.requireGoldenLocalDaemonTransportClean(); err != nil {
		return result, err
	}
	if err := stopGoldenContinuityMonitors(monitors, activated, parseSummaryTime(result.rollback.ActivatedAt)); err != nil {
		monitorsStopped = true
		return result, err
	}
	monitorsStopped = true

	if err := r.finishCycle(ctx, &result); err != nil {
		return result, err
	}
	closeGoldenSessionObservers(all)
	lastCandidateClose, err := waitGoldenResponseConnectionsClosed(ctx, []*goldenSession{postDirect})
	if err != nil {
		return result, err
	}
	result.cleanup, err = r.runRetirementCheck(
		ctx, lastCandidateClose, result.rollback.FromGenerationIDHash,
		result.rollback.FromSlot, result.rollback.ToSlot, result.rollback,
		inputs.name+"-slot-retirement.json",
	)
	if err != nil {
		return result, err
	}
	if err := r.resumeCycle(ctx, inputs.clientPath, &result); err != nil {
		return result, err
	}
	return result, nil
}

func (r *goldenRunner) runFinalCycle(ctx context.Context, inputs goldenCycleInputs, rehearsal goldenActionSummary) (goldenCycleResult, error) {
	var result goldenCycleResult
	initial, err := r.startCycleInitialSessions(ctx, inputs)
	if err != nil {
		return result, err
	}
	result.initial = initial
	beforeEvidence, err := r.capturePhase(inputs.name+"-before-activation", initial, inputs.localDaemonPID)
	if err != nil {
		return result, err
	}
	if err := requireLocalEgress(beforeEvidence); err != nil {
		return result, err
	}
	result.before = evidenceByLabel(beforeEvidence)
	if err := requireBoundLocalEgress(initial, result.before); err != nil {
		return result, err
	}

	spanningLocal, spanningBefore, leaseBefore, err := r.startSpanningLocalSession(ctx, inputs)
	if err != nil {
		return result, err
	}
	result.fresh = []*goldenSession{spanningLocal}
	localSessions := append(append([]*goldenSession{}, initial...), spanningLocal)
	if err := requireBoundLocalEgress(localSessions, spanningBefore); err != nil {
		return result, err
	}
	localEgressMonitor, err := startGoldenLocalEgressMonitor(
		ctx, r, inputs.localDaemonPID, inputs.name+"-local-egress-window", spanningBefore["local-daemon"],
	)
	if err != nil {
		return result, err
	}
	localEgressMonitorStopped := false
	defer func() {
		if !localEgressMonitorStopped {
			_ = localEgressMonitor.stopAndValidate()
		}
	}()
	baselineEnd := time.Now().UTC()
	monitors, err := startGoldenContinuityMonitors(r, localSessions, baselineEnd)
	if err != nil {
		return result, err
	}
	monitorsStopped := false
	defer func() {
		if !monitorsStopped {
			cancelGoldenContinuityMonitors(monitors)
		}
	}()
	var postDirect *goldenSession
	result.activation, postDirect, result.after, err = r.runSlotActivationWithAck(
		ctx, "final", inputs, initial, spanningLocal, result.before, spanningBefore, monitors, localEgressMonitor,
	)
	if err != nil {
		return result, err
	}
	result.fresh = append(result.fresh, postDirect)
	if err := validateGoldenTransitionAction(result.activation, true); err != nil {
		return result, err
	}
	if err := validateGoldenProvenance(r.summary.ReleasedSHA256, result.activation); err != nil {
		return result, err
	}
	if err := r.validateGoldenSlotCandidate(result.activation); err != nil {
		return result, err
	}
	if err := validateGoldenSameActivation(rehearsal, result.activation); err != nil {
		return result, err
	}
	activated := parseSummaryTime(result.activation.ActivatedAt)
	if err := requireGoldenSessionSpans(spanningLocal, activated); err != nil {
		return result, err
	}
	_, localStarted, _ := goldenSessionRequestWindow(spanningLocal)
	if err := requireGoldenLeaseWindow(inputs.leaseObserver, localStarted, activated, leaseBefore); err != nil {
		return result, err
	}
	all := append(append([]*goldenSession{}, initial...), result.fresh...)
	if err := requireSessionsRunning(all, "final_activation"); err != nil {
		return result, err
	}
	if err := requireStableSessionSockets(initial, result.before, result.after); err != nil {
		return result, err
	}
	if err := requireStableSessionSockets([]*goldenSession{spanningLocal}, spanningBefore, result.after); err != nil {
		return result, err
	}
	if err := requireStableSessionSockets([]*goldenSession{postDirect}, result.after, result.after); err != nil {
		return result, err
	}
	if err := requireBoundLocalEgress(all, result.after); err != nil {
		return result, err
	}
	if err := r.requireGoldenLocalDaemonTransportClean(); err != nil {
		return result, err
	}
	monitorErr := localEgressMonitor.stopAndValidate()
	localEgressMonitorStopped = true
	if monitorErr != nil {
		return result, monitorErr
	}
	retiringSessions := append(append([]*goldenSession{}, initial...), spanningLocal)
	if err := waitGoldenSessions(ctx, retiringSessions); err != nil {
		return result, err
	}
	if err := stopGoldenContinuityMonitors(monitors, activated, time.Now().UTC()); err != nil {
		monitorsStopped = true
		return result, err
	}
	monitorsStopped = true
	if err := validateGoldenSessions(initial, false); err != nil {
		return result, err
	}
	if err := validateGoldenSessions([]*goldenSession{spanningLocal}, false); err != nil {
		return result, err
	}
	if err := validateObserverTurns(retiringSessions, 1); err != nil {
		return result, err
	}
	closeGoldenSessionObservers(retiringSessions)
	directOldSessions := goldenSessionsForRoute(initial, "direct-hosted")
	if len(directOldSessions) != 2 {
		return result, failGolden("retirement_direct_connection_count_invalid")
	}
	lastOldClose, err := waitGoldenResponseConnectionsClosed(ctx, directOldSessions)
	if err != nil {
		return result, err
	}
	result.cleanup, err = r.runRetirementCheck(
		ctx, lastOldClose, result.activation.FromGenerationIDHash,
		result.activation.FromSlot, result.activation.ToSlot, result.activation,
		inputs.name+"-slot-retirement.json",
	)
	if err != nil {
		return result, err
	}
	if err := r.resumeCycle(ctx, inputs.clientPath, &result); err != nil {
		return result, err
	}
	if err := waitGoldenSessions(ctx, []*goldenSession{postDirect}); err != nil {
		return result, err
	}
	if err := r.validateCompletedCycleSessions(&result); err != nil {
		return result, err
	}
	return result, nil
}

func goldenSessionsForRoute(sessions []*goldenSession, route string) []*goldenSession {
	result := make([]*goldenSession, 0, len(sessions))
	for _, session := range sessions {
		if session != nil && session.route == route {
			result = append(result, session)
		}
	}
	return result
}

func (r *goldenRunner) finishCycle(ctx context.Context, result *goldenCycleResult) error {
	all := append(append([]*goldenSession{}, result.initial...), result.fresh...)
	if err := waitGoldenSessions(ctx, all); err != nil {
		return err
	}
	return r.validateCompletedCycleSessions(result)
}

func (r *goldenRunner) resumeCycle(ctx context.Context, clientPath string, result *goldenCycleResult) error {
	resumes, err := r.startResumeSessions(ctx, clientPath, result.initial)
	if err != nil {
		return err
	}
	result.resumes = resumes
	if err := waitGoldenSessions(ctx, resumes); err != nil {
		return err
	}
	if err := validateGoldenSessions(resumes, true); err != nil {
		return err
	}
	if err := validateResumeThreads(result.initial, resumes); err != nil {
		return err
	}
	if err := validateObserverTurns(resumes, 1); err != nil {
		return err
	}
	for index := range resumes {
		if err := requireGoldenFreshResumeConnection(result.initial[index], resumes[index], parseSummaryTime(result.cleanup.AbsentAt), r.testMode); err != nil {
			return err
		}
		if resumes[index].route == "local-egress" {
			if err := requireGoldenLocalObserverPath(resumes[index]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *goldenRunner) validateCompletedCycleSessions(result *goldenCycleResult) error {
	if err := validateGoldenSessions(result.initial, false); err != nil {
		return err
	}
	if err := validateGoldenSessions(result.fresh, false); err != nil {
		return err
	}
	return validateObserverTurns(result.fresh, 1)
}

func validateGoldenTransitionAction(action goldenActionSummary, activation bool) error {
	wantType := "slot-rollback"
	if activation {
		wantType = "slot-activation"
	}
	if action.ExitCode != 0 || !action.EvidenceValid || action.EvidenceType != wantType ||
		action.canonical == nil || !validGoldenSHA256(action.EvidenceSHA256) ||
		action.StartedAt == "" || action.FinishedAt == "" || action.RequestedAt == "" || action.ActivatedAt == "" ||
		len(action.FromGenerationIDHash) != 64 || len(action.ToGenerationIDHash) != 64 ||
		action.FromGenerationIDHash == action.ToGenerationIDHash || action.ActiveGenerationIDHash != action.ToGenerationIDHash ||
		!validGoldenSHA256(action.FromReleaseSHA256) || !validGoldenSHA256(action.ToReleaseSHA256) ||
		action.FromReleaseSHA256 == action.ToReleaseSHA256 || action.RestartDelta != 0 || action.OOMDelta != 0 ||
		action.ServerOldPeakRSSBytes <= 0 || action.ServerOldPeakRSSBytes > goldenRSSLimitBytes ||
		action.ServerNewPeakRSSBytes <= 0 || action.ServerNewPeakRSSBytes > goldenRSSLimitBytes ||
		action.ServerFrontPeakRSSBytes <= 0 || action.ServerFrontPeakRSSBytes > goldenFrontRSSLimitBytes {
		return failGolden("transition_evidence_invalid")
	}
	started, finished := parseSummaryTime(action.StartedAt), parseSummaryTime(action.FinishedAt)
	requested, activated := parseSummaryTime(action.RequestedAt), parseSummaryTime(action.ActivatedAt)
	if started.IsZero() || finished.Before(started) || action.DurationMillis != finished.Sub(started).Milliseconds() ||
		requested.IsZero() || activated.Before(requested) || action.PhaseDurationMillis != activated.Sub(requested).Milliseconds() {
		return failGolden("action_timing_invalid")
	}
	if activated.Sub(requested) >= goldenActivationLimit {
		return failGolden("activation_duration_exceeded")
	}
	return nil
}

func validateGoldenRollback(activation, rollback goldenActionSummary) error {
	if rollback.LinkedEvidenceSHA256 != activation.EvidenceSHA256 ||
		activation.FromSlot != rollback.ToSlot || activation.ToSlot != rollback.FromSlot ||
		activation.FromGenerationIDHash != rollback.ToGenerationIDHash ||
		activation.ToGenerationIDHash != rollback.FromGenerationIDHash ||
		activation.FromReleaseSHA256 != rollback.ToReleaseSHA256 ||
		activation.ToReleaseSHA256 != rollback.FromReleaseSHA256 ||
		activation.ReleaseTag != rollback.ReleaseTag ||
		activation.ReleaseSourceRevision != rollback.ReleaseSourceRevision {
		return failGolden("rollback_not_exact_reversal")
	}
	if err := validateGoldenSlotCounterHandoff(activation, rollback); err != nil {
		return err
	}
	if rollback.canonical == nil || rollback.canonical.Connections.ExpectedExternal == nil ||
		rollback.canonical.Connections.Before == nil || rollback.canonical.Connections.After == nil ||
		*rollback.canonical.Connections.ExpectedExternal != 1 ||
		*rollback.canonical.Connections.Before < 1 || *rollback.canonical.Connections.After < 1 {
		return failGolden("rollback_connection_count_invalid")
	}
	return nil
}

func validateGoldenSameActivation(first, second goldenActionSummary) error {
	if first.FromSlot != second.FromSlot || first.ToSlot != second.ToSlot || first.ActiveSlot != second.ActiveSlot ||
		first.FromReleaseSHA256 != second.FromReleaseSHA256 || first.ToReleaseSHA256 != second.ToReleaseSHA256 ||
		first.ReleaseTag != second.ReleaseTag || first.ReleaseSourceRevision != second.ReleaseSourceRevision {
		return failGolden("final_activation_candidate_changed")
	}
	return validateGoldenSlotCounterHandoff(first, second)
}

func validateGoldenProvenance(predecessorSHA string, activation goldenActionSummary) error {
	if !validGoldenSHA256(predecessorSHA) || activation.FromReleaseSHA256 != predecessorSHA ||
		activation.ToReleaseSHA256 == predecessorSHA || activation.ReleaseTag == "" ||
		len(activation.ReleaseSourceRevision) != 40 {
		return failGolden("deployment_provenance_mismatch")
	}
	return nil
}

func (r *goldenRunner) runRetirementCheck(
	ctx context.Context,
	lastClose time.Time,
	expectedGenerationHash string,
	expectedRetiredSlot string,
	expectedActiveSlot string,
	transition goldenActionSummary,
	evidenceName string,
) (goldenActionSummary, error) {
	result := r.runEvidenceAction(ctx, goldenEvidenceActionOptions{
		label: "slot-retirement", argv: r.options.oldGenerationTest, expect: "slot-retirement",
		evidenceName: evidenceName, linkFlag: "--transition-evidence",
		linkPath: filepath.Join(r.artifactDir, transition.EvidenceFile),
	})
	if result.ExitCode != 0 || !result.EvidenceValid {
		return result, failGolden("old_generation_cleanup_failed")
	}
	closed := parseSummaryTime(result.LastConnectionClosedAt)
	absent := parseSummaryTime(result.AbsentAt)
	if lastClose.IsZero() || closed.IsZero() || absent.IsZero() {
		return result, failGolden("old_generation_retirement_late")
	}
	if closed.Before(lastClose) || absent.Before(closed) || absent.Sub(closed) >= goldenRetirementLimit {
		return result, failGolden("old_generation_retirement_late")
	}
	result.ObservedRetiredWithinMS = absent.Sub(lastClose).Milliseconds()
	if result.OldGenerationIDHash != expectedGenerationHash || result.OldGenerationActive ||
		result.OldGenerationAccepting || result.OldGenerationConnections != 0 ||
		result.FromSlot != expectedRetiredSlot || result.ActiveSlot != expectedActiveSlot ||
		result.LinkedEvidenceSHA256 != transition.EvidenceSHA256 {
		return result, failGolden("old_generation_evidence_invalid")
	}
	if err := validateGoldenSlotCounterHandoff(transition, result); err != nil {
		return result, err
	}
	_ = r.evidence.write(map[string]any{
		"kind": "old_generation_retired", "timestamp": absent.Format(time.RFC3339Nano),
		"old_generation_id_sha256":   result.OldGenerationIDHash,
		"reported_retired_within_ms": result.ReportedRetiredWithinMS,
		"observed_retired_within_ms": result.ObservedRetiredWithinMS,
	})
	return result, nil
}

func cancelGoldenContinuityMonitors(monitors []*goldenContinuityMonitor) {
	for _, monitor := range monitors {
		close(monitor.stop)
	}
	for _, monitor := range monitors {
		<-monitor.done
	}
}

func waitGoldenContinuityBoundary(ctx context.Context, monitors []*goldenContinuityMonitor, boundary time.Time) error {
	if boundary.IsZero() {
		return failGolden("deployment_window_invalid")
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		complete := true
		for _, monitor := range monitors {
			requests := responseRequests(monitor.session.observer.stats)
			if len(requests) != 1 || requests[0].RequestID != monitor.requestID || requests[0].ConnectionID == "" {
				return failGolden("continuity_transport_identity_changed")
			}
			before, after := false, false
			_, chunks, _ := monitor.session.observer.stats.snapshot()
			for _, chunk := range chunks {
				if chunk.Kind != "response_chunk" || chunk.RequestID != monitor.requestID ||
					chunk.ConnectionID != requests[0].ConnectionID || chunk.Bytes <= 0 {
					continue
				}
				stamp, _ := time.Parse(time.RFC3339Nano, chunk.Timestamp)
				before = before || stamp.Before(boundary)
				after = after || stamp.After(boundary)
			}
			if !before || !after {
				complete = false
				if sessionDone(monitor.session) {
					return failGolden("continuity_boundary_bytes_missing")
				}
			}
		}
		if complete {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
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
		return releasedClient{
			path: options.releasedClient, version: "test-override", sha256: digest,
			checksumVerified: testMode, revision: "test-revision", revisionVerified: testMode,
		}, nil
	}
	if options.predecessorClient != "" {
		info, err := os.Stat(options.predecessorClient)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return releasedClient{}, failGolden("predecessor_client_invalid")
		}
		digest, err := fileSHA256(options.predecessorClient)
		if err != nil || digest != goldenPinnedPredecessorSHA256 || digest != strings.ToLower(strings.TrimSpace(options.predecessorSHA256)) {
			return releasedClient{}, failGolden("predecessor_checksum_mismatch")
		}
		revision, revisionVerified, err := verifyGoldenPredecessorProvenance(
			ctx, &http.Client{Timeout: 30 * time.Second}, options.predecessorClient, goldenPinnedPredecessorVersion, testMode,
		)
		if err != nil {
			return releasedClient{}, err
		}
		return releasedClient{
			path: options.predecessorClient, version: goldenPinnedPredecessorVersion, sha256: digest,
			checksumVerified: true, revision: revision, revisionVerified: revisionVerified,
		}, nil
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
	revision, revisionVerified, err := verifyGoldenPredecessorProvenance(ctx, httpClient, path, version, testMode)
	if err != nil {
		return releasedClient{}, err
	}
	return releasedClient{
		path: path, version: version, sha256: actual, checksumVerified: true,
		revision: revision, revisionVerified: revisionVerified,
	}, nil
}

func verifyGoldenPredecessorProvenance(
	ctx context.Context,
	httpClient *http.Client,
	binaryPath string,
	version string,
	testMode bool,
) (string, bool, error) {
	if testMode {
		return "test-revision", true, nil
	}
	if version != goldenPinnedPredecessorVersion {
		return "", false, failGolden("predecessor_version_unpinned")
	}
	info, err := buildinfo.ReadFile(binaryPath)
	if err != nil {
		return "", false, failGolden("predecessor_build_info_missing")
	}
	revision, modified := "", ""
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}
	if revision != goldenPinnedPredecessorRevision || modified == "true" {
		return "", false, failGolden("predecessor_revision_mismatch")
	}
	var tagged struct {
		SHA string `json:"sha"`
	}
	if err := getGoldenGitHubJSON(ctx, httpClient,
		"https://api.github.com/repos/manaflow-ai/subrouter/commits/v"+version, &tagged); err != nil || tagged.SHA != revision {
		return "", false, failGolden("predecessor_tag_mismatch")
	}
	var comparison struct {
		Status          string `json:"status"`
		MergeBaseCommit struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err := getGoldenGitHubJSON(ctx, httpClient,
		"https://api.github.com/repos/manaflow-ai/subrouter/compare/"+revision+"...main", &comparison); err != nil ||
		(comparison.Status != "ahead" && comparison.Status != "identical") || comparison.MergeBaseCommit.SHA != revision {
		return "", false, failGolden("predecessor_not_on_main")
	}
	return revision, true, nil
}

func getGoldenGitHubJSON(ctx context.Context, client *http.Client, rawURL string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("github provenance status")
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(target)
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
	label            string
	baseURL          string
	upstream         *url.URL
	server           *http.Server
	listener         net.Listener
	events           *os.File
	stats            *observerStats
	pid              int
	done             chan error
	upstreamLoopback bool
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
	observation := newObserver(events, stats)
	server := &http.Server{
		Handler:           newObserverHandlerWithObserver(upstream, observation),
		ReadHeaderTimeout: 10 * time.Second,
		ConnState: func(connection net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				observation.closeConnection(connection.RemoteAddr().String())
			}
		},
	}
	running := &runningGoldenObserver{
		label: label, baseURL: "http://" + listener.Addr().String(), server: server,
		listener: listener, events: events, stats: stats, pid: os.Getpid(), done: make(chan error, 1),
		upstream: upstream, upstreamLoopback: isGoldenLoopbackHost(upstream.Hostname()),
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

func closeGoldenSessionObservers(sessions []*goldenSession) {
	seen := make(map[*runningGoldenObserver]bool)
	for _, session := range sessions {
		if session == nil || session.observer == nil || seen[session.observer] {
			continue
		}
		seen[session.observer] = true
		session.observer.stop()
	}
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
	overrides := map[string]string{
		"SUBROUTER_STATE_DIR":    state,
		"SUBROUTER_CLOUD_CONFIG": teamConfigPath,
	}
	if goldenTestHooks.enabled {
		for _, key := range []string{
			"SUBROUTER_GOLDEN_FAKE_SOCKET_STATE",
			"SUBROUTER_GOLDEN_FAKE_DAEMON_PID",
			"SUBROUTER_GOLDEN_FAKE_STREAM_GENERATION",
		} {
			if value := strings.TrimSpace(os.Getenv(key)); value != "" {
				overrides[key] = value
			}
		}
	}
	command.Env = goldenChildEnv(home, overrides)
	command.Stdout = io.Discard
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, nil, failGolden("local_daemon_pipe_failed")
	}
	if err := command.Start(); err != nil {
		return nil, nil, failGolden("local_daemon_start_failed")
	}
	stderrDone := make(chan struct{})
	r.mu.Lock()
	r.localStderrDone = stderrDone
	r.processes = append(r.processes, command)
	r.mu.Unlock()
	go func() {
		defer close(stderrDone)
		r.consumeGoldenLocalDaemonStderr(stderr)
	}()
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

func (r *goldenRunner) sampleLocalDaemonRSS(ctx context.Context, pid int) {
	ticker := time.NewTicker(goldenProcessSampleInterval)
	defer ticker.Stop()
	workers := make(chan struct{}, 4)
	var group sync.WaitGroup
	launch := func() {
		select {
		case workers <- struct{}{}:
			group.Add(1)
			go func() {
				defer group.Done()
				defer func() { <-workers }()
				r.recordGoldenProcessSample(pid)
			}()
		default:
		}
	}
	launch()
	for {
		select {
		case <-ctx.Done():
			group.Wait()
			return
		case <-ticker.C:
			launch()
		}
	}
}

func (r *goldenRunner) recordGoldenProcessSample(pid int) {
	started := time.Now().UTC()
	r.mu.Lock()
	sessions := append([]*goldenSession(nil), r.sessions...)
	r.mu.Unlock()
	table, tableErr := loadGoldenProcessTable(nil)
	bytes, processes, paused, err := measureGoldenProcessTree(table, pid)
	if tableErr != nil {
		err = tableErr
	}
	r.localRSSMu.Lock()
	if started.After(r.localLastSample) {
		if !r.localLastSample.IsZero() {
			if gap := started.Sub(r.localLastSample); gap > r.localMaxSampleGap {
				r.localMaxSampleGap = gap
			}
		}
		r.localLastSample = started
	}
	if err == nil {
		if bytes > r.localPeakRSS {
			r.localPeakRSS = bytes
		}
		r.localRSSSamples++
		if paused {
			r.localPausedSamples++
		}
		if bytes > goldenRSSLimitBytes {
			r.localRSSExceeded = true
		}
	} else {
		r.localSampleFailures++
	}
	r.localRSSMu.Unlock()
	if err == nil {
		_ = r.evidence.write(map[string]any{
			"kind": "process_sample", "timestamp": started.Format(time.RFC3339Nano),
			"label": "local-daemon", "rss_bytes": bytes, "process_count": processes, "paused": paused,
		})
	}
	for _, session := range sessions {
		if sessionDone(session) {
			continue
		}
		sessionBytes, sessionProcesses, sessionPaused, sessionErr := measureGoldenProcessTree(table, session.command.Process.Pid)
		if tableErr != nil {
			sessionErr = tableErr
		}
		if sessionErr != nil && sessionDone(session) {
			continue
		}
		session.mu.Lock()
		if started.After(session.lastProcessSample) {
			if !session.lastProcessSample.IsZero() {
				if gap := started.Sub(session.lastProcessSample); gap > session.maxProcessSampleGap {
					session.maxProcessSampleGap = gap
				}
			}
			session.lastProcessSample = started
		}
		if sessionErr == nil {
			if sessionBytes > session.peakRSSBytes {
				session.peakRSSBytes = sessionBytes
			}
			session.rssSamples++
			if sessionPaused {
				session.pausedProcessSamples++
			}
			if sessionBytes > goldenRSSLimitBytes {
				session.rssExceeded = true
			}
		} else {
			session.processSampleFailures++
		}
		session.mu.Unlock()
		if sessionErr == nil {
			_ = r.evidence.write(map[string]any{
				"kind": "process_sample", "timestamp": started.Format(time.RFC3339Nano),
				"label": session.label, "rss_bytes": sessionBytes,
				"process_count": sessionProcesses, "paused": sessionPaused,
			})
		}
	}
}

func (r *goldenRunner) finalizeLocalDaemonRSS() error {
	r.localRSSMu.Lock()
	defer r.localRSSMu.Unlock()
	r.summary.LocalDaemonPeakRSSBytes = r.localPeakRSS
	r.summary.LocalDaemonRSSSamples = r.localRSSSamples
	r.summary.LocalDaemonProcessSamples = r.localRSSSamples
	r.summary.LocalDaemonMaxSampleGapMS = r.localMaxSampleGap.Milliseconds()
	r.summary.LocalDaemonPausedSamples = r.localPausedSamples
	if r.localRSSExceeded || r.localPeakRSS > goldenRSSLimitBytes {
		return failGolden("rss_limit_exceeded")
	}
	if r.localRSSSamples == 0 || r.localPeakRSS <= 0 {
		return failGolden("local_daemon_rss_missing")
	}
	if r.localPausedSamples != 0 {
		return failGolden("paused_process_detected")
	}
	if r.localSampleFailures != 0 {
		return failGolden("process_sampling_failed")
	}
	if r.localMaxSampleGap > goldenProcessSampleMaxGap {
		return failGolden("process_sampling_gap")
	}
	return nil
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
	finishedAt time.Time

	mu                    sync.Mutex
	threadID              string
	threadIDCount         int
	markerCount           int
	nonceCount            int
	payloadContract       bool
	payloadExpectedLines  int
	payloadMessageCount   int
	payloadNextLine       int
	payloadNumberedLines  int
	payloadSHA256         string
	payloadInvalid        bool
	issues                map[string]int
	stdoutBytes           int64
	stderrBytes           int64
	exitCode              int
	waitErr               error
	peakRSSBytes          int64
	rssSamples            int
	rssExceeded           bool
	lastProcessSample     time.Time
	maxProcessSampleGap   time.Duration
	pausedProcessSamples  int
	processSampleFailures int
	monitoredPIDs         []int
	preP99Gap             time.Duration
	allowedGap            time.Duration
	deployMaxGap          time.Duration
	transportSocketStable bool
	localUpstreamSocket   string
	localEgressSocket     string
	localEgressCorrelated bool
	localEgressBinding    *goldenLocalEgressBinding
	done                  chan struct{}
	threadAvailable       chan struct{}
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
	prompt := fmt.Sprintf(
		"Do not use tools. First output exactly %s once on its own line. Then output %d numbered lines, each containing only its number and the letter x. Do not stop, summarize, or skip a number. After all numbered lines, output exactly %s once on its own line.",
		nonce, r.options.streamLines, marker,
	)
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
		payloadContract: true, payloadExpectedLines: r.options.streamLines,
		done: make(chan struct{}), threadAvailable: make(chan struct{}),
	}
	if resume {
		session.payloadExpectedLines = 0
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
	session.monitoredPIDs = []int{command.Process.Pid}
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
		finishedAt := time.Now().UTC()
		session.mu.Lock()
		session.waitErr = waitErr
		session.exitCode = commandExitCode(waitErr)
		session.finishedAt = finishedAt
		exitCode := session.exitCode
		markerCount := session.markerCount
		nonceCount := session.nonceCount
		payloadLineCount := session.payloadNumberedLines
		payloadSHA256 := session.payloadSHA256
		issues := issueCount(session.issues)
		session.mu.Unlock()
		_ = r.evidence.write(map[string]any{
			"kind": "session_finished", "timestamp": finishedAt.Format(time.RFC3339Nano),
			"label": session.label, "exit_code": exitCode, "marker_count": markerCount,
			"nonce_count": nonceCount, "numbered_line_count": payloadLineCount,
			"numbered_lines_sha256": payloadSHA256, "issue_count": issues,
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
					observeGoldenAgentMessage(session, event.Item.Text)
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
		if sessionResponseChunkCount(session) >= goldenBaselineChunkSamples {
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

func sessionResponseChunkCount(session *goldenSession) int {
	_, chunks, _ := session.observer.stats.snapshot()
	count := 0
	for _, chunk := range chunks {
		if chunk.Kind == "response_chunk" && chunk.Bytes > 0 &&
			(chunk.Path == "/v1/responses" || chunk.Path == "/responses") {
			count++
		}
	}
	return count
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

type goldenContinuityMonitor struct {
	runner    *goldenRunner
	session   *goldenSession
	requestID string
	preP99    time.Duration
	allowed   time.Duration
	stop      chan struct{}
	done      chan struct{}

	mu      sync.Mutex
	liveErr error
}

func startGoldenContinuityMonitors(runner *goldenRunner, sessions []*goldenSession, baselineEnd time.Time) ([]*goldenContinuityMonitor, error) {
	result := make([]*goldenContinuityMonitor, 0, len(sessions))
	for _, session := range sessions {
		requests := responseRequests(session.observer.stats)
		if len(requests) != 1 {
			return nil, failGolden("baseline_response_request_invalid")
		}
		stamps := goldenResponseChunkTimes(session, requests[0].RequestID, time.Time{}, baselineEnd)
		if len(stamps) < goldenBaselineChunkSamples {
			return nil, failGolden("baseline_chunk_samples_missing")
		}
		preP99 := goldenP99Gap(stamps)
		allowed := 2 * preP99
		if allowed < goldenChunkGapFloor {
			allowed = goldenChunkGapFloor
		}
		monitor := &goldenContinuityMonitor{
			runner: runner, session: session, requestID: requests[0].RequestID,
			preP99: preP99, allowed: allowed, stop: make(chan struct{}), done: make(chan struct{}),
		}
		result = append(result, monitor)
		go monitor.run()
	}
	return result, nil
}

func (m *goldenContinuityMonitor) run() {
	ticker := time.NewTicker(goldenProbeInterval)
	defer ticker.Stop()
	defer close(m.done)
	for {
		_, chunks, _ := m.session.observer.stats.snapshot()
		latest := time.Time{}
		count := 0
		for _, chunk := range chunks {
			if chunk.Kind != "response_chunk" || chunk.RequestID != m.requestID {
				continue
			}
			stamp, _ := time.Parse(time.RFC3339Nano, chunk.Timestamp)
			if !stamp.IsZero() && (latest.IsZero() || stamp.After(latest)) {
				latest = stamp
			}
			count++
		}
		age := time.Duration(0)
		if !latest.IsZero() {
			reference := time.Now().UTC()
			m.session.mu.Lock()
			finishedAt := m.session.finishedAt
			m.session.mu.Unlock()
			if !finishedAt.IsZero() && finishedAt.Before(reference) {
				reference = finishedAt
			}
			age = reference.Sub(latest)
		}
		if latest.IsZero() || age > m.allowed {
			m.mu.Lock()
			if m.liveErr == nil {
				m.liveErr = failGolden("chunk_gap_limit_exceeded")
			}
			m.mu.Unlock()
		}
		_ = m.runner.evidence.write(map[string]any{
			"kind": "stream_continuity_sample", "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"label": m.session.label, "response_chunks": count, "last_chunk_age_ms": age.Milliseconds(),
			"allowed_gap_ms": m.allowed.Milliseconds(),
		})
		select {
		case <-m.stop:
			return
		case <-ticker.C:
		}
	}
}

func stopGoldenContinuityMonitors(monitors []*goldenContinuityMonitor, start, end time.Time) error {
	if start.IsZero() || end.Before(start) {
		return failGolden("deployment_window_invalid")
	}
	for _, monitor := range monitors {
		close(monitor.stop)
	}
	for _, monitor := range monitors {
		<-monitor.done
		monitor.mu.Lock()
		liveErr := monitor.liveErr
		monitor.mu.Unlock()
		if liveErr != nil {
			return liveErr
		}
		monitor.session.mu.Lock()
		finishedAt := monitor.session.finishedAt
		monitor.session.mu.Unlock()
		effectiveEnd := end
		if !finishedAt.IsZero() && finishedAt.Before(effectiveEnd) {
			effectiveEnd = finishedAt
		}
		stamps := goldenResponseChunkTimes(monitor.session, monitor.requestID, time.Time{}, effectiveEnd)
		var window []time.Time
		for _, stamp := range stamps {
			if stamp.Before(start) {
				if len(window) == 0 || stamp.After(window[0]) {
					window = []time.Time{stamp}
				}
				continue
			}
			window = append(window, stamp)
		}
		if len(window) < 2 {
			return failGolden("deployment_chunk_samples_missing")
		}
		maxGap := time.Duration(0)
		for index := 1; index < len(window); index++ {
			if gap := window[index].Sub(window[index-1]); gap > maxGap {
				maxGap = gap
			}
		}
		if endGap := effectiveEnd.Sub(window[len(window)-1]); endGap > maxGap {
			maxGap = endGap
		}
		if maxGap > monitor.allowed {
			return failGolden("chunk_gap_limit_exceeded")
		}
		monitor.session.mu.Lock()
		monitor.session.preP99Gap = monitor.preP99
		monitor.session.allowedGap = monitor.allowed
		monitor.session.deployMaxGap = maxGap
		monitor.session.mu.Unlock()
	}
	return nil
}

func goldenResponseChunkTimes(session *goldenSession, requestID string, after, before time.Time) []time.Time {
	_, chunks, _ := session.observer.stats.snapshot()
	result := make([]time.Time, 0, len(chunks))
	for _, chunk := range chunks {
		if chunk.Kind != "response_chunk" || chunk.RequestID != requestID {
			continue
		}
		stamp, _ := time.Parse(time.RFC3339Nano, chunk.Timestamp)
		if stamp.IsZero() || (!after.IsZero() && stamp.Before(after)) || (!before.IsZero() && stamp.After(before)) {
			continue
		}
		result = append(result, stamp)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Before(result[j]) })
	return result
}

func goldenP99Gap(stamps []time.Time) time.Duration {
	if len(stamps) < 2 {
		return 0
	}
	gaps := make([]time.Duration, 0, len(stamps)-1)
	for index := 1; index < len(stamps); index++ {
		gaps = append(gaps, stamps[index].Sub(stamps[index-1]))
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	rank := (99*len(gaps)+99)/100 - 1
	return gaps[rank]
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

func waitGoldenSessionChunks(ctx context.Context, session *goldenSession, minimum int) error {
	for {
		requests := responseRequests(session.observer.stats)
		if len(requests) == 1 {
			count := len(goldenResponseChunkTimes(session, requests[0].RequestID, time.Time{}, time.Time{}))
			if count >= minimum {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-session.done:
			return failGolden("stream_baseline_ended_early")
		case <-session.observer.stats.notify:
		}
	}
}

func goldenSessionRequestWindow(session *goldenSession) (transportEvent, time.Time, error) {
	requests := responseRequests(session.observer.stats)
	if len(requests) != 1 {
		return transportEvent{}, time.Time{}, failGolden("response_request_count_invalid")
	}
	started, err := time.Parse(time.RFC3339Nano, requests[0].Timestamp)
	if err != nil || started.IsZero() {
		return transportEvent{}, time.Time{}, failGolden("transport_evidence_incomplete")
	}
	return requests[0], started, nil
}

func requireGoldenSessionSpans(session *goldenSession, boundary time.Time) error {
	request, started, err := goldenSessionRequestWindow(session)
	if err != nil || !started.Before(boundary) {
		return failGolden("activation_spanning_request_missing")
	}
	chunks := goldenResponseChunkTimes(session, request.RequestID, time.Time{}, time.Time{})
	before, after := false, false
	for _, stamp := range chunks {
		before = before || stamp.Before(boundary)
		after = after || stamp.After(boundary)
	}
	if !before || !after || sessionDone(session) {
		return failGolden("activation_spanning_request_missing")
	}
	return nil
}

func requireGoldenSessionStartsAfter(session *goldenSession, boundary time.Time) error {
	_, started, err := goldenSessionRequestWindow(session)
	if err != nil || started.Before(boundary) || sessionDone(session) {
		return failGolden("post_activation_request_missing")
	}
	return nil
}

func requireGoldenLeaseWindow(leaseObserver *runningGoldenObserver, requestStart, activated time.Time, beforeCount int) error {
	requests, _, _ := leaseObserver.stats.snapshot()
	leaseCount := 0
	for _, request := range requests {
		if request.Path == "/v1/responses" || request.Path == "/responses" {
			return failGolden("local_route_bypassed_daemon")
		}
		if request.Path == "/_subrouter/leases" {
			return failGolden("candidate_lease_endpoint_substituted")
		}
		if request.Path != "/api/subrouter/leases" {
			continue
		}
		if request.Method != http.MethodPost {
			return failGolden("legacy_lease_method_invalid")
		}
		leaseCount++
		stamp, _ := time.Parse(time.RFC3339Nano, request.Timestamp)
		if !stamp.Before(requestStart) && !stamp.After(activated) && leaseCount > beforeCount {
			return nil
		}
	}
	return failGolden("activation_fresh_local_lease_missing")
}

func requireGoldenLocalObserverPath(session *goldenSession) error {
	if session.route != "local-egress" || !session.observer.upstreamLoopback {
		return failGolden("local_route_not_loopback")
	}
	request, _, err := goldenSessionRequestWindow(session)
	if err != nil {
		return err
	}
	opened := ""
	requestBytes, responseBytes := int64(0), int64(0)
	for _, event := range session.observer.stats.upstreamSnapshot() {
		if event.RequestID != request.RequestID {
			continue
		}
		switch event.Kind {
		case "upstream_connection_opened":
			if opened != "" && opened != event.ConnectionID {
				return failGolden("local_upstream_socket_changed")
			}
			opened = event.ConnectionID
		case "upstream_request_chunk":
			requestBytes += event.Bytes
		case "upstream_response_chunk":
			responseBytes += event.Bytes
		}
	}
	if len(opened) != 64 || requestBytes <= 0 || responseBytes <= 0 {
		return failGolden("local_upstream_evidence_missing")
	}
	session.mu.Lock()
	session.localUpstreamSocket = opened
	session.mu.Unlock()
	return nil
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

func validGoldenSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (r *goldenRunner) startActivationSession(
	ctx context.Context,
	label string,
	route string,
	clientPath string,
	authData []byte,
	cloud goldenCloudConfig,
	directConfigPath, teamConfigPath string,
	hostedOrigin, localOrigin *url.URL,
) (*goldenSession, error) {
	upstream := hostedOrigin
	if route == "local-egress" {
		upstream = localOrigin
	}
	observation, err := r.startObserver(label, upstream)
	if err != nil {
		return nil, err
	}
	return r.startFreshSession(
		ctx, clientPath, authData, cloud, directConfigPath, teamConfigPath,
		observation, label, route, "websocket",
	)
}

func (r *goldenRunner) startSpanningLocalSession(
	ctx context.Context,
	inputs goldenCycleInputs,
) (*goldenSession, map[string]goldenProcessEvidence, int, error) {
	phase := inputs.name + "-candidate-local"
	baseline, err := captureProcessEvidence(phase+"-before", "local-daemon", inputs.localDaemonPID)
	if err != nil {
		return nil, nil, 0, err
	}
	_ = r.evidence.write(map[string]any{
		"kind": "local_egress_baseline", "timestamp": baseline.Timestamp,
		"phase": phase, "remote_socket_ids": baseline.RemoteSocketIDs,
	})
	leaseBefore := observerRequestCount(inputs.leaseObserver.stats, "/api/subrouter/leases")
	session, err := r.startActivationSession(
		ctx, inputs.name+"-candidate-local", "local-egress", inputs.clientPath, inputs.authData,
		inputs.cloud, inputs.directConfigPath, inputs.teamConfigPath, inputs.hostedOrigin, inputs.localOrigin,
	)
	if err != nil {
		return nil, nil, 0, err
	}
	if err := waitGoldenSessionChunks(ctx, session, goldenBaselineChunkSamples); err != nil {
		return nil, nil, 0, err
	}
	if err := requireGoldenLocalObserverPath(session); err != nil {
		return nil, nil, 0, err
	}
	if _, err := r.waitAndBindGoldenLocalEgress(
		ctx, session, inputs.leaseObserver, leaseBefore, baseline,
		inputs.localDaemonPID, phase+"-during",
	); err != nil {
		return nil, nil, 0, err
	}
	beforeEvidence, err := r.capturePhase(phase+"-ready", []*goldenSession{session}, inputs.localDaemonPID)
	if err != nil {
		return nil, nil, 0, err
	}
	return session, evidenceByLabel(beforeEvidence), leaseBefore, nil
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

func waitGoldenResponseConnectionsClosed(ctx context.Context, sessions []*goldenSession) (time.Time, error) {
	type target struct {
		stats        *observerStats
		connectionID string
	}
	targets := make([]target, 0, len(sessions))
	for _, session := range sessions {
		requests := responseRequests(session.observer.stats)
		if len(requests) == 0 || requests[0].ConnectionID == "" {
			return time.Time{}, failGolden("response_connection_missing")
		}
		targets = append(targets, target{stats: session.observer.stats, connectionID: requests[0].ConnectionID})
	}
	for {
		allClosed := true
		latest := time.Time{}
		for _, item := range targets {
			closed := false
			for _, event := range item.stats.closedSnapshot() {
				if event.ConnectionID != item.connectionID {
					continue
				}
				stamp, _ := time.Parse(time.RFC3339Nano, event.Timestamp)
				if !stamp.IsZero() && (latest.IsZero() || stamp.After(latest)) {
					latest = stamp
				}
				closed = true
			}
			allClosed = allClosed && closed
		}
		if allClosed && !latest.IsZero() {
			return latest, nil
		}
		select {
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func validateGoldenSessions(sessions []*goldenSession, resume bool) error {
	for _, session := range sessions {
		session.mu.Lock()
		exitCode := session.exitCode
		markerCount := session.markerCount
		nonceCount := session.nonceCount
		payloadContract := session.payloadContract
		payloadExpectedLines := session.payloadExpectedLines
		payloadMessageCount := session.payloadMessageCount
		payloadNumberedLines := session.payloadNumberedLines
		payloadSHA256 := session.payloadSHA256
		payloadInvalid := session.payloadInvalid
		threadID := session.threadID
		threadIDCount := session.threadIDCount
		issues := issueCount(session.issues)
		peakRSS := session.peakRSSBytes
		rssSamples := session.rssSamples
		rssExceeded := session.rssExceeded
		maxSampleGap := session.maxProcessSampleGap
		pausedSamples := session.pausedProcessSamples
		sampleFailures := session.processSampleFailures
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
		if nonceCount != 1 {
			if resume {
				return failGolden("resume_nonce_not_exact")
			}
			return failGolden("nonce_context_not_exact")
		}
		if payloadContract && (payloadInvalid || payloadMessageCount < 1 ||
			payloadNumberedLines != payloadExpectedLines || len(payloadSHA256) != 64) {
			return failGolden("agent_payload_invalid")
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
		if rssExceeded || peakRSS > goldenRSSLimitBytes {
			return failGolden("rss_limit_exceeded")
		}
		if rssSamples == 0 || peakRSS <= 0 {
			return failGolden("process_rss_missing")
		}
		if pausedSamples != 0 {
			return failGolden("paused_process_detected")
		}
		if sampleFailures != 0 {
			return failGolden("process_sampling_failed")
		}
		if maxSampleGap > goldenProcessSampleMaxGap {
			return failGolden("process_sampling_gap")
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
		if original.observer == nil || original.observer.upstream == nil {
			return nil, failGolden("resume_observer_missing")
		}
		observation, err := r.startObserver(original.label+"-resume", original.observer.upstream)
		if err != nil {
			return nil, err
		}
		prompt := "Do not use tools. Reply with the exact nonce from the first turn, then one newline, then exactly " + marker + ". Do not repeat either value."
		baseURL := observation.baseURL + "/v1"
		if original.route == "direct-hosted" {
			baseURL = observation.baseURL + strings.TrimPrefix(original.baseURL, original.observer.baseURL)
		}
		session := &goldenSession{
			label: original.label + "-resume", route: original.route, transport: original.transport,
			nonce: original.nonce, marker: marker, resume: true, home: original.home,
			codexHome: original.codexHome, configPath: original.configPath, baseURL: baseURL,
			observer: observation, issues: make(map[string]int), payloadContract: true,
			done:            make(chan struct{}),
			threadAvailable: make(chan struct{}),
		}
		if err := r.launchSession(ctx, clientPath, session, threadID, prompt); err != nil {
			return nil, err
		}
		result = append(result, session)
	}
	return result, nil
}

func requireGoldenFreshResumeConnection(original, resume *goldenSession, after time.Time, testMode bool) error {
	if original == nil || resume == nil || original.observer == nil || resume.observer == nil ||
		original.observer == resume.observer || original.baseURL == resume.baseURL {
		return failGolden("resume_connection_not_fresh")
	}
	originalRequests := responseRequests(original.observer.stats)
	resumeRequests := responseRequests(resume.observer.stats)
	if len(originalRequests) != 1 || len(resumeRequests) != 1 ||
		originalRequests[0].ConnectionID == "" || resumeRequests[0].ConnectionID == "" {
		return failGolden("resume_connection_not_fresh")
	}
	resumeStarted, err := parseGoldenEvidenceTime(resumeRequests[0].Timestamp)
	if err != nil || after.IsZero() || !resumeStarted.After(after) ||
		(!testMode && originalRequests[0].ConnectionID == resumeRequests[0].ConnectionID) {
		return failGolden("resume_connection_not_fresh")
	}
	return nil
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
	table, err := loadGoldenProcessTable(nil)
	if err != nil {
		return nil, err
	}
	var result []goldenProcessEvidence
	for _, session := range sessions {
		evidence, err := captureProcessEvidenceFromTable(phase, session.label, session.command.Process.Pid, table)
		if err != nil {
			return nil, err
		}
		session.mu.Lock()
		session.monitoredPIDs = append([]int(nil), evidence.DescendantPIDs...)
		session.mu.Unlock()
		result = append(result, evidence)
	}
	local, err := captureProcessEvidenceFromTable(phase, "local-daemon", localDaemonPID, table)
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
	if err := r.requireGoldenLocalDaemonTransportClean(); err != nil {
		return nil, err
	}
	return result, nil
}

func captureProcessEvidence(phase, label string, pid int) (goldenProcessEvidence, error) {
	if pid <= 0 {
		return goldenProcessEvidence{}, failGolden("process_id_missing")
	}
	table, err := loadGoldenProcessTable(nil)
	if err != nil {
		return goldenProcessEvidence{}, err
	}
	return captureProcessEvidenceFromTable(phase, label, pid, table)
}

func captureProcessEvidenceFromTable(phase, label string, pid int, table goldenProcessTable) (goldenProcessEvidence, error) {
	if pid <= 0 {
		return goldenProcessEvidence{}, failGolden("process_id_missing")
	}
	pids := goldenProcessTreePIDs(table, pid)
	if len(pids) == 0 {
		return goldenProcessEvidence{}, failGolden("process_tree_missing")
	}
	var socketIDs, remoteIDs, states []string
	var remoteSockets []goldenRemoteSocket
	var rssBytes int64
	for _, processID := range pids {
		sample, ok := table.processes[processID]
		if !ok {
			return goldenProcessEvidence{}, failGolden("process_tree_missing")
		}
		state := sample.state
		if strings.HasPrefix(state, "T") {
			return goldenProcessEvidence{}, failGolden("paused_process_detected")
		}
		states = append(states, state)
		if sample.rss > goldenRSSLimitBytes-rssBytes {
			return goldenProcessEvidence{}, failGolden("rss_limit_exceeded")
		}
		rssBytes += sample.rss
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
			id := goldenSocketEndpointID(localSocketEndpoint(name))
			if id == "" {
				continue
			}
			socketIDs = append(socketIDs, id)
			if socketDestinationIsRemote(name) {
				remoteSocket, ok := newGoldenRemoteSocket(name)
				if !ok {
					continue
				}
				remoteIDs = append(remoteIDs, remoteSocket.SocketID)
				remoteSockets = append(remoteSockets, remoteSocket)
			}
		}
	}
	sort.Strings(socketIDs)
	sort.Strings(remoteIDs)
	sort.Slice(remoteSockets, func(i, j int) bool { return remoteSockets[i].SocketID < remoteSockets[j].SocketID })
	remoteSockets = deduplicateGoldenRemoteSockets(remoteSockets)
	return goldenProcessEvidence{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Phase: phase, Label: label,
		ProcessID: pid, DescendantPIDs: pids, ProcessStates: states, SocketIDs: deduplicateStrings(socketIDs),
		RemoteSocketIDs: deduplicateStrings(remoteIDs), RSSBytes: rssBytes, remoteSockets: remoteSockets,
	}, nil
}

func goldenProcessTreePIDs(table goldenProcessTable, root int) []int {
	if _, ok := table.processes[root]; !ok {
		return nil
	}
	seen := make(map[int]bool)
	queue := []int{root}
	result := make([]int, 0, 1)
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if _, ok := table.processes[pid]; !ok {
			continue
		}
		result = append(result, pid)
		queue = append(queue, table.children[pid]...)
	}
	sort.Ints(result)
	return result
}

func processTreeRSSBytes(root int) (int64, int, error) {
	pids := descendantPIDs(root)
	if len(pids) == 0 {
		return 0, 0, failGolden("process_tree_missing")
	}
	var total int64
	for _, pid := range pids {
		rss, err := processRSSBytes(pid)
		if err != nil {
			return 0, 0, err
		}
		if rss > goldenRSSLimitBytes-total {
			return total + rss, len(pids), failGolden("rss_limit_exceeded")
		}
		total += rss
	}
	return total, len(pids), nil
}

type goldenProcessSample struct {
	parent int
	state  string
	rss    int64
}

type goldenProcessTable struct {
	processes map[int]goldenProcessSample
	children  map[int][]int
}

func loadGoldenProcessTable(pids []int) (goldenProcessTable, error) {
	seen := make(map[int]bool)
	values := make([]string, 0, len(pids))
	for _, pid := range pids {
		if pid > 0 && !seen[pid] {
			seen[pid] = true
			values = append(values, strconv.Itoa(pid))
		}
	}
	arguments := []string{"-axo", "pid=,ppid=,state=,rss="}
	if len(values) != 0 {
		arguments = []string{"-p", strings.Join(values, ","), "-o", "pid=,ppid=,state=,rss="}
	}
	output, err := exec.Command("ps", arguments...).Output()
	if err != nil {
		return goldenProcessTable{}, failGolden("process_sample_failed")
	}
	processes := make(map[int]goldenProcessSample)
	children := make(map[int][]int)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		rssKiB, rssErr := strconv.ParseInt(fields[3], 10, 64)
		if pidErr != nil || parentErr != nil || rssErr != nil || pid <= 0 || rssKiB < 0 || rssKiB > (1<<62)/1024 {
			continue
		}
		processes[pid] = goldenProcessSample{parent: parent, state: fields[2], rss: rssKiB * 1024}
		children[parent] = append(children[parent], pid)
	}
	return goldenProcessTable{processes: processes, children: children}, nil
}

func measureGoldenProcessTree(table goldenProcessTable, root int) (int64, int, bool, error) {
	if _, ok := table.processes[root]; !ok {
		return 0, 0, false, failGolden("process_sample_root_missing")
	}
	seen := make(map[int]bool)
	queue := []int{root}
	var total int64
	paused := false
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		sample, ok := table.processes[pid]
		if !ok {
			continue
		}
		if sample.rss > (1<<62)-total {
			return 0, 0, false, failGolden("process_rss_invalid")
		}
		total += sample.rss
		paused = paused || strings.HasPrefix(sample.state, "T")
		queue = append(queue, table.children[pid]...)
	}
	if total <= 0 {
		return 0, 0, false, failGolden("process_rss_missing")
	}
	return total, len(seen), paused, nil
}

func processRSSBytes(pid int) (int64, error) {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "rss=").Output()
	if err != nil {
		return 0, failGolden("process_rss_missing")
	}
	fields := strings.Fields(string(output))
	if len(fields) != 1 {
		return 0, failGolden("process_rss_missing")
	}
	kibibytes, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || kibibytes <= 0 || kibibytes > (1<<62)/1024 {
		return 0, failGolden("process_rss_invalid")
	}
	return kibibytes * 1024, nil
}

func localSocketEndpoint(name string) string {
	local, _, _ := strings.Cut(strings.TrimSpace(name), "->")
	return strings.TrimSpace(local)
}

func goldenSocketEndpointID(endpoint string) string {
	endpoint = strings.ToLower(strings.TrimSpace(endpoint))
	if endpoint == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(hash[:])
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
		requests := responseRequests(session.observer.stats)
		if len(requests) == 0 || requests[0].ConnectionID == "" {
			return failGolden("response_transport_socket_missing")
		}
		transportID := requests[0].ConnectionID
		leftHasTransport := false
		for _, id := range left.SocketIDs {
			leftHasTransport = leftHasTransport || id == transportID
		}
		rightHasTransport := false
		for _, id := range right.SocketIDs {
			rightHasTransport = rightHasTransport || id == transportID
		}
		if !leftHasTransport || !rightHasTransport {
			return failGolden("session_socket_identity_changed")
		}
		session.mu.Lock()
		session.transportSocketStable = true
		session.mu.Unlock()
	}
	return nil
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
	type observerSnapshot struct {
		scope    string
		requests []transportEvent
		chunks   []transportEvent
	}
	requests, chunks, proxyErrors := session.observer.stats.snapshot()
	snapshots := []observerSnapshot{{scope: "initial", requests: requests, chunks: chunks}}
	if resume != nil && resume.observer != nil && resume.observer != session.observer {
		resumeRequests, resumeChunks, resumeProxyErrors := resume.observer.stats.snapshot()
		snapshots = append(snapshots, observerSnapshot{scope: "resume", requests: resumeRequests, chunks: resumeChunks})
		proxyErrors += resumeProxyErrors
	}
	responseRequests := 0
	connections := make(map[string]bool)
	responseTransportSocket := ""
	fallbacks := 0
	var responseBytes int64
	stampsByRequest := make(map[string][]time.Time)
	for _, snapshot := range snapshots {
		for _, request := range snapshot.requests {
			if request.Path != "/v1/responses" && request.Path != "/responses" {
				continue
			}
			responseRequests++
			// Connection IDs are scoped to one observer. A resumed turn deliberately
			// uses a new observer, so even an opaque-ID collision cannot collapse the
			// two independently observed transport connections in the summary.
			connections[snapshot.scope+"\x00"+request.ConnectionID] = true
			if responseTransportSocket == "" {
				responseTransportSocket = request.ConnectionID
			}
			if request.Transport != session.transport {
				fallbacks++
			}
		}
		for _, chunk := range snapshot.chunks {
			if chunk.Kind != "response_chunk" || (chunk.Path != "/v1/responses" && chunk.Path != "/responses") {
				continue
			}
			responseBytes += chunk.Bytes
			stamp, _ := time.Parse(time.RFC3339Nano, chunk.Timestamp)
			if !stamp.IsZero() {
				stampsByRequest[snapshot.scope+"\x00"+chunk.RequestID] = append(stampsByRequest[snapshot.scope+"\x00"+chunk.RequestID], stamp)
			}
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
	peakRSS := session.peakRSSBytes
	rssSamples := session.rssSamples
	maxProcessSampleGap := session.maxProcessSampleGap
	pausedProcessSamples := session.pausedProcessSamples
	preP99Gap := session.preP99Gap
	allowedGap := session.allowedGap
	deployMaxGap := session.deployMaxGap
	transportSocketStable := session.transportSocketStable
	localUpstreamSocket := session.localUpstreamSocket
	localEgressSocket := session.localEgressSocket
	localEgressCorrelated := session.localEgressCorrelated
	session.mu.Unlock()
	resumeMarkerCount, resumeNonceCount, resumeExit, resumeIssues := 0, 0, 0, 0
	if resume != nil {
		resume.mu.Lock()
		resumeMarkerCount = resume.markerCount
		resumeNonceCount = resume.nonceCount
		resumeExit = resume.exitCode
		resumeIssues = issueCount(resume.issues)
		if resume.peakRSSBytes > peakRSS {
			peakRSS = resume.peakRSSBytes
		}
		rssSamples += resume.rssSamples
		if resume.maxProcessSampleGap > maxProcessSampleGap {
			maxProcessSampleGap = resume.maxProcessSampleGap
		}
		pausedProcessSamples += resume.pausedProcessSamples
		resume.mu.Unlock()
	}
	if allowedGap == 0 {
		allowedGap = goldenChunkGapFloor
		deployMaxGap = maxGap
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
	retries := responseRequests - expectedRequests
	if retries < 0 {
		retries = 0
	}
	return goldenSessionSummary{
		Label: session.label, Route: session.route, Transport: session.transport,
		ProcessID: session.command.Process.Pid, ThreadIDHash: hashGoldenValue(threadID),
		NonceHash: hashGoldenValue(session.nonce), ResponseRequests: responseRequests,
		ResponseConnections: len(connections), ResponseTransportSocket: responseTransportSocket,
		TransportSocketStable: transportSocketStable, ResponseBytes: responseBytes,
		MaxChunkGapMillis: maxGap.Milliseconds(), PreDeployP99GapMillis: preP99Gap.Milliseconds(),
		AllowedChunkGapMillis: allowedGap.Milliseconds(), DeployMaxChunkGapMillis: deployMaxGap.Milliseconds(),
		PeakRSSBytes: peakRSS, RSSSamples: rssSamples, ProcessSamples: rssSamples,
		MaxProcessSampleGapMS: maxProcessSampleGap.Milliseconds(), PausedProcessSamples: pausedProcessSamples,
		MarkerCount:       markerCount,
		ResumeMarkerCount: resumeMarkerCount, ResumeNonceCount: resumeNonceCount,
		RetryCount: retries, ReconnectCount: reconnects, FallbackCount: fallbacks,
		ErrorCount:       issueCount(issues) + resumeIssues + proxyErrors,
		NonzeroExitCount: nonzero, DuplicateMarkerCount: duplicate,
		SocketIDsBefore: before.SocketIDs, SocketIDsAfterRollback: after.SocketIDs,
		LocalUpstreamSocket: localUpstreamSocket, LocalEgressSocket: localEgressSocket,
		LocalEgressCorrelated: localEgressCorrelated,
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
	if !testMode && (summary.ExpectedPredecessorSHA256 != summary.ReleasedSHA256 || !validGoldenSHA256(summary.ExpectedPredecessorSHA256)) {
		return failGolden("predecessor_evidence_incomplete")
	}
	if !testMode && (summary.ReleasedVersion != goldenPinnedPredecessorVersion ||
		summary.ReleasedSHA256 != goldenPinnedPredecessorSHA256 ||
		summary.PredecessorRevision != goldenPinnedPredecessorRevision || !summary.PredecessorRevisionVerified) {
		return failGolden("predecessor_evidence_incomplete")
	}
	if !testMode && summary.ReleasedVersion == "test-override" {
		return failGolden("candidate_client_forbidden")
	}
	if err := validateGoldenMigrationSummary(summary, testMode); err != nil {
		return err
	}
	if err := validateGoldenTransitionAction(summary.Activation, true); err != nil {
		return err
	}
	if err := validateGoldenProvenance(summary.ReleasedSHA256, summary.Activation); err != nil {
		return err
	}
	if err := validateGoldenTransitionAction(summary.Rollback, false); err != nil {
		return err
	}
	if err := validateGoldenTransitionAction(summary.FinalActivation, true); err != nil {
		return err
	}
	if err := validateGoldenProvenance(summary.ReleasedSHA256, summary.FinalActivation); err != nil {
		return err
	}
	if err := validateGoldenRollback(summary.Activation, summary.Rollback); err != nil {
		return err
	}
	if err := validateGoldenSameActivation(summary.Activation, summary.FinalActivation); err != nil {
		return err
	}
	if err := validateGoldenCleanupSummary(summary.OldGenerationCleanup, summary.Activation.ToGenerationIDHash); err != nil {
		return err
	}
	if summary.OldGenerationCleanup.LinkedEvidenceSHA256 != summary.Rollback.EvidenceSHA256 {
		return failGolden("old_generation_evidence_invalid")
	}
	if err := validateGoldenCleanupSummary(summary.FinalOldGenerationCleanup, summary.Activation.FromGenerationIDHash); err != nil {
		return err
	}
	if summary.FinalOldGenerationCleanup.LinkedEvidenceSHA256 != summary.FinalActivation.EvidenceSHA256 {
		return failGolden("old_generation_evidence_invalid")
	}
	if err := validateGoldenCounterContinuity(summary); err != nil {
		return err
	}
	if summary.ProbeFrequencyHz != 10 || len(summary.Health) != 4 {
		return failGolden("health_evidence_incomplete")
	}
	for _, health := range summary.Health {
		if health.Label == "" || health.Samples == 0 || health.Failures != 0 || health.MaxStartGapMillis > 250 {
			return failGolden("health_evidence_incomplete")
		}
	}
	expected := make(map[string]struct {
		route, transport string
	})
	for _, suffix := range []string{"direct-websocket", "direct-http", "local-websocket", "local-http"} {
		route := "direct-hosted"
		if strings.HasPrefix(suffix, "local-") {
			route = "local-egress"
		}
		transport := "websocket"
		if strings.HasSuffix(suffix, "-http") {
			transport = "http"
		}
		expected["migration-"+suffix] = struct{ route, transport string }{route: route, transport: transport}
	}
	for _, label := range []string{
		"migration-candidate-front-rehearsal-destination-direct",
		"migration-candidate-legacy-rollback-destination-direct",
		"migration-candidate-front-final-destination-direct",
	} {
		expected[label] = struct{ route, transport string }{route: "direct-hosted", transport: "websocket"}
	}
	for _, cycle := range []string{"rehearsal", "final"} {
		expected[cycle+"-direct-websocket"] = struct{ route, transport string }{route: "direct-hosted", transport: "websocket"}
		expected[cycle+"-direct-http"] = struct{ route, transport string }{route: "direct-hosted", transport: "http"}
		expected[cycle+"-local-websocket"] = struct{ route, transport string }{route: "local-egress", transport: "websocket"}
		expected[cycle+"-local-http"] = struct{ route, transport string }{route: "local-egress", transport: "http"}
		expected[cycle+"-candidate-direct"] = struct{ route, transport string }{route: "direct-hosted", transport: "websocket"}
		expected[cycle+"-candidate-local"] = struct{ route, transport string }{route: "local-egress", transport: "websocket"}
	}
	if len(summary.Sessions) != len(expected) {
		return fmt.Errorf("%w: got %d sessions, want %d", failGolden("session_evidence_incomplete"), len(summary.Sessions), len(expected))
	}
	for _, session := range summary.Sessions {
		want, ok := expected[session.Label]
		if !ok || session.Route != want.route || session.Transport != want.transport ||
			session.ProcessID <= 0 || len(session.ThreadIDHash) != 64 || len(session.NonceHash) != 64 ||
			session.ResponseRequests == 0 || session.ResponseConnections == 0 || len(session.ResponseTransportSocket) != 64 ||
			(!strings.Contains(session.Label, "-candidate-") && !session.TransportSocketStable) || session.ResponseBytes <= 0 ||
			session.MarkerCount != 1 || session.RetryCount != 0 || session.ReconnectCount != 0 ||
			session.FallbackCount != 0 || session.ErrorCount != 0 || session.NonzeroExitCount != 0 ||
			session.DuplicateMarkerCount != 0 || session.PeakRSSBytes <= 0 || session.PeakRSSBytes > goldenRSSLimitBytes ||
			session.RSSSamples == 0 || session.ProcessSamples == 0 || session.PausedProcessSamples != 0 ||
			session.MaxProcessSampleGapMS > goldenProcessSampleMaxGap.Milliseconds() ||
			session.AllowedChunkGapMillis < goldenChunkGapFloor.Milliseconds() ||
			session.DeployMaxChunkGapMillis > session.AllowedChunkGapMillis {
			return fmt.Errorf("%w: invalid session %q", failGolden("session_evidence_incomplete"), session.Label)
		}
		if strings.Contains(session.Label, "-candidate-") {
			if session.ResumeMarkerCount != 0 || session.ResumeNonceCount != 0 {
				return failGolden("activation_session_evidence_invalid")
			}
		} else if session.ResumeMarkerCount != 1 || session.ResumeNonceCount != 1 ||
			len(session.SocketIDsBefore) == 0 || len(session.SocketIDsAfterRollback) == 0 {
			return failGolden("resume_evidence_incomplete")
		}
		if session.Route == "local-egress" && len(session.LocalUpstreamSocket) != 64 {
			return failGolden("local_upstream_evidence_missing")
		}
		if session.Route == "local-egress" &&
			(!session.LocalEgressCorrelated || len(session.LocalEgressSocket) != 64) {
			return failGolden("local_egress_correlation_missing")
		}
		allowed := 2 * session.PreDeployP99GapMillis
		if allowed < goldenChunkGapFloor.Milliseconds() {
			allowed = goldenChunkGapFloor.Milliseconds()
		}
		if !strings.Contains(session.Label, "-candidate-") && session.AllowedChunkGapMillis != allowed {
			return failGolden("chunk_gap_threshold_invalid")
		}
		delete(expected, session.Label)
	}
	if len(expected) != 0 || !summary.FreshLocalLeaseObserved || !summary.LegacyBrokerLeaseObserved || summary.DeploymentEnvironmentRead {
		return failGolden("golden_evidence_incomplete")
	}
	if len(summary.ProcessSnapshots) == 0 {
		return failGolden("process_evidence_incomplete")
	}
	requiredProcessEvidence := make(map[string]bool)
	for _, suffix := range []string{"direct-websocket", "direct-http", "local-websocket", "local-http"} {
		requiredProcessEvidence["migration-before-rehearsal-cutover\x00migration-"+suffix] = false
		requiredProcessEvidence["migration-after-final-cutover\x00migration-"+suffix] = false
	}
	requiredProcessEvidence["migration-before-rehearsal-cutover\x00local-daemon"] = false
	requiredProcessEvidence["migration-after-final-cutover\x00local-daemon"] = false
	for _, label := range []string{
		"migration-candidate-front-rehearsal-destination-direct",
		"migration-candidate-legacy-rollback-destination-direct",
		"migration-candidate-front-final-destination-direct",
	} {
		requiredProcessEvidence["migration-after-final-cutover\x00"+label] = false
	}
	for _, cycle := range []string{"rehearsal", "final"} {
		phases := []string{cycle + "-before-activation", cycle + "-after-activation"}
		if cycle == "rehearsal" {
			phases = append(phases, cycle+"-after-rollback")
		}
		for _, phase := range phases {
			for _, suffix := range []string{"direct-websocket", "direct-http", "local-websocket", "local-http"} {
				requiredProcessEvidence[phase+"\x00"+cycle+"-"+suffix] = false
			}
			requiredProcessEvidence[phase+"\x00local-daemon"] = false
			if phase != cycle+"-before-activation" {
				requiredProcessEvidence[phase+"\x00"+cycle+"-candidate-direct"] = false
				requiredProcessEvidence[phase+"\x00"+cycle+"-candidate-local"] = false
			}
		}
	}
	for _, item := range summary.ProcessSnapshots {
		key := item.Phase + "\x00" + item.Label
		_, required := requiredProcessEvidence[key]
		if item.ProcessID <= 0 || item.Timestamp == "" {
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
		if !strings.HasPrefix(item.Label, "observer-") && (len(item.DescendantPIDs) == 0 || len(item.SocketIDs) == 0 ||
			item.RSSBytes <= 0 || item.RSSBytes > goldenRSSLimitBytes) {
			return failGolden("socket_evidence_incomplete")
		}
		if item.Label == "local-daemon" && len(item.RemoteSocketIDs) == 0 {
			return failGolden("egress_evidence_incomplete")
		}
		if required {
			requiredProcessEvidence[key] = true
		}
	}
	for _, present := range requiredProcessEvidence {
		if !present {
			return failGolden("process_evidence_incomplete")
		}
	}
	if summary.LocalDaemonRSSSamples == 0 || summary.LocalDaemonProcessSamples == 0 ||
		summary.LocalDaemonPausedSamples != 0 || summary.LocalDaemonMaxSampleGapMS > goldenProcessSampleMaxGap.Milliseconds() ||
		summary.LocalDaemonPeakRSSBytes <= 0 || summary.LocalDaemonPeakRSSBytes > goldenRSSLimitBytes {
		return failGolden("local_daemon_rss_missing")
	}
	return nil
}

func validateGoldenCleanupSummary(action goldenActionSummary, expectedGenerationHash string) error {
	if action.ExitCode != 0 || !action.EvidenceValid || action.EvidenceType != "slot-retirement" ||
		action.canonical == nil || !validGoldenSHA256(action.EvidenceSHA256) ||
		action.StartedAt == "" || action.FinishedAt == "" || action.LastConnectionClosedAt == "" || action.AbsentAt == "" ||
		action.OldGenerationIDHash != expectedGenerationHash || action.OldGenerationActive ||
		action.OldGenerationAccepting || action.OldGenerationConnections != 0 ||
		action.ReportedRetiredWithinMS < 0 || action.ReportedRetiredWithinMS > goldenRetirementLimit.Milliseconds() ||
		action.ObservedRetiredWithinMS < 0 {
		return failGolden("old_generation_evidence_invalid")
	}
	started, finished := parseSummaryTime(action.StartedAt), parseSummaryTime(action.FinishedAt)
	if started.IsZero() || !finished.After(started) || action.DurationMillis != finished.Sub(started).Milliseconds() {
		return failGolden("action_timing_invalid")
	}
	return nil
}

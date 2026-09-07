package main

import (
	"bufio"
	"context"
	"encoding/hex"
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
	agentantigravity "github.com/manaflow-ai/subrouter/internal/agents/antigravity"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	agentgrok "github.com/manaflow-ai/subrouter/internal/agents/grok"
	agentkimi "github.com/manaflow-ai/subrouter/internal/agents/kimi"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/internal/stackauth"
	"github.com/manaflow-ai/subrouter/internal/storepath"
	"github.com/manaflow-ai/subrouter/internal/tailnet"
	"github.com/manaflow-ai/subrouter/internal/tenant"
	"github.com/manaflow-ai/subrouter/internal/transcript"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
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
	// Process-owned commands, including read-only isolation checks, must keep
	// the logger installed by their caller. Creating the CLI file handler calls
	// StateDir and creates durable state as a side effect.
	if shouldKeepProcessLogger(program, args) {
		return
	}
	path := filepath.Join(storepath.StateDir(), "logs", "subrouter-cli.log")
	handler := newCLIFileLogHandler(path)
	slog.SetDefault(slog.New(handler))
}

func shouldKeepProcessLogger(_ string, args []string) bool {
	return isCodexIsolationCheckCommand(args) ||
		(len(args) > 0 && (args[0] == "serve" || args[0] == "supervise" || args[0] == "front" || args[0] == "listener-transfer"))
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

// envTrue reports whether an environment variable is set to a truthy value
// ("1", "true", "yes", "on", case-insensitive).
func envTrue(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

const maxSecretFileBytes = 64 << 10

func secretFromEnvironment(valueName, fileName string) (string, error) {
	direct := strings.TrimSpace(os.Getenv(valueName))
	path := strings.TrimSpace(os.Getenv(fileName))
	if direct != "" && path != "" {
		return "", fmt.Errorf("%s and %s cannot both be set", valueName, fileName)
	}
	if path == "" {
		return direct, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", fileName, err)
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxSecretFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", fileName, err)
	}
	if len(body) > maxSecretFileBytes {
		return "", fmt.Errorf("read %s: secret exceeds %d bytes", fileName, maxSecretFileBytes)
	}
	secret := strings.TrimSpace(string(body))
	if secret == "" {
		return "", fmt.Errorf("read %s: secret is empty", fileName)
	}
	return secret, nil
}

func secretValue(explicit, valueName, fileName string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	return secretFromEnvironment(valueName, fileName)
}

func shadowHealthKeyFromEnvironment() ([]byte, error) {
	const fileName = "SUBROUTER_SHADOW_HEALTH_KEY_FILE"
	path := strings.TrimSpace(os.Getenv(fileName))
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", fileName, err)
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxSecretFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", fileName, err)
	}
	if len(body) > maxSecretFileBytes {
		return nil, fmt.Errorf("read %s: key exceeds %d bytes", fileName, maxSecretFileBytes)
	}
	value := strings.TrimSpace(string(body))
	key, err := hex.DecodeString(value)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("read %s: key must be exactly 32 bytes encoded as hexadecimal", fileName)
	}
	return key, nil
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
	if isCodexAccountCommand(args) {
		return srForProgram(program, args)
	}
	if program == "sr" &&
		(isDirectSRCommand(args[0]) || strings.Contains(args[0], "@")) {
		return srForProgram(program, args)
	}

	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "supervise":
		return supervise(args[1:])
	case "front":
		return runFront(args[1:])
	case "listener-transfer":
		return runListenerTransfer(args[1:])
	case "probe":
		return probe(args[1:])
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
	case "install-launchd":
		return installLaunchd(args[1:])
	case "version", "-v", "--version":
		printVersion(os.Stdout, program)
		return nil
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

func probe(args []string) error {
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	baseURL := flags.String("url", "http://127.0.0.1:31415", "Subrouter base URL")
	timeout := flags.Duration("timeout", 2*time.Second, "health request timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *timeout <= 0 {
		return errors.New("probe timeout must be positive")
	}
	target, err := url.Parse(strings.TrimSpace(*baseURL))
	if err != nil {
		return fmt.Errorf("probe URL: %w", err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return fmt.Errorf("probe URL uses unsupported scheme %q", target.Scheme)
	}
	if target.Host == "" {
		return errors.New("probe URL has no host")
	}
	target.Path = "/_subrouter/health"
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""
	client := &http.Client{Timeout: *timeout}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return fmt.Errorf("probe %s: %w", target.Redacted(), err)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("probe %s: %w", target.Redacted(), err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("probe %s returned status %d", target.Redacted(), response.StatusCode)
	}
	return nil
}

var directSRCommands = map[string]struct{}{
	"add":              {},
	"add-admin-key":    {},
	"add-api-key":      {},
	"add-key":          {},
	"admin-keys":       {},
	"agy":              {},
	"antigravity":      {},
	"account":          {},
	"accounts":         {},
	"attach-project":   {},
	"breadcrumbs":      {},
	"claude":           {},
	"claude-aws":       {},
	"claude-direct":    {},
	"cleanup":          {},
	"cost":             {},
	"daemon":           {},
	"doctor":           {},
	"g":                {},
	"az":               {},
	"azure":            {},
	"gemini":           {},
	"gui":              {},
	"gui-switch":       {},
	"gui-use":          {},
	"import":           {},
	"kimi":             {},
	"list":             {},
	"list-admin-keys":  {},
	"login":            {},
	"ls":               {},
	"logout":           {},
	"pick":             {},
	"qwen":             {},
	"remove":           {},
	"remove-admin-key": {},
	"remote":           {},
	"remotes":          {},
	"reset":            {},
	"rm":               {},
	"server":           {},
	"servers":          {},
	"setup":            {},
	"spend":            {},
	"status":           {},
	"storage":          {},
	"switch":           {},
	"tenant":           {},
	"tenants":          {},
	"team":             {},
	"trace":            {},
	"usage":            {},
	"use":              {},
	"version":          {},
	"why":              {},
	"-v":               {},
	"--version":        {},
}

func isDirectSRCommand(command string) bool {
	_, ok := directSRCommands[command]
	return ok
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", "127.0.0.1:31415", "listen address")
	localDataSocket := flags.String("local-data-socket", "", "private mode-0600 Unix socket for local credential-bearing requests")
	upstreamRaw := flags.String("upstream", "", "force one upstream base URL for all accounts")
	codexUpstreamRaw := flags.String("codex-upstream", "https://chatgpt.com/backend-api/codex", "Codex subscription upstream base URL")
	apiUpstreamRaw := flags.String("api-upstream", "https://api.openai.com", "OpenAI API-key upstream base URL")
	claudeUpstreamRaw := flags.String("claude-upstream", "https://api.anthropic.com", "Claude subscription upstream base URL")
	kimiUpstreamRaw := flags.String("kimi-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderKimi), "Kimi For Coding upstream base URL")
	zaiUpstreamRaw := flags.String("zai-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderZAI), "Z.AI coding upstream base URL")
	openRouterUpstreamRaw := flags.String("openrouter-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderOpenRouter), "OpenRouter upstream base URL")
	deepSeekUpstreamRaw := flags.String("deepseek-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderDeepSeek), "DeepSeek upstream base URL")
	togetherUpstreamRaw := flags.String("together-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderTogether), "Together AI upstream base URL")
	fireworksUpstreamRaw := flags.String("fireworks-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderFireworks), "Fireworks AI upstream base URL")
	openCodeZenUpstreamRaw := flags.String("opencode-zen-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderOpenCodeZen), "OpenCode Zen upstream base URL")
	grokUpstreamRaw := flags.String("grok-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderGrok), "xAI Grok upstream base URL")
	grokSubscriptionUpstreamRaw := flags.String("grok-subscription-upstream", "https://cli-chat-proxy.grok.com/v1", "Grok subscription (OAuth) upstream base URL")
	var openAICompatibleRaw stringList
	flags.Var(&openAICompatibleRaw, "openai-compatible", "declare an OpenAI-compatible provider as name=BASE_URL (repeatable); aliases may follow the name as name|alias=BASE_URL")
	qwenAnthropicUpstreamRaw := flags.String("qwen-anthropic-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderQwenAnthropic), "Alibaba Model Studio Token Plan Anthropic-protocol upstream base URL (Beijing: https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic)")
	qwenTokenUpstreamRaw := flags.String("qwen-token-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderQwenToken), "Alibaba Model Studio Token Plan upstream base URL (Beijing: https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1)")
	qwenUpstreamRaw := flags.String("qwen-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderQwen), "Alibaba Model Studio Coding Plan upstream base URL (Beijing: https://coding.dashscope.aliyuncs.com/v1)")
	antigravityUpstreamRaw := flags.String("antigravity-upstream", "https://daily-cloudcode-pa.googleapis.com", "Antigravity subscription upstream base URL")
	antigravityLocalCredential := flags.Bool("antigravity-local-credential", true, "serve managed Antigravity profiles, falling back to the invoking user's CLI credential until the first import")
	sessionPath := flags.String("sessions", session.DefaultStorePath(), "session assignment store")
	transcriptDir := flags.String("transcripts", "", "directory for raw Subrouter transcript JSONL files")
	transcriptGCSURI := flags.String("transcript-gcs-uri", "", "optional gs:// bucket/prefix for background transcript sync")
	transcriptGCSSyncInterval := flags.Duration("transcript-gcs-sync-interval", 5*time.Minute, "interval for background transcript GCS sync; 0 disables")
	transcriptGCSSyncTimeout := flags.Duration("transcript-gcs-sync-timeout", 30*time.Minute, "timeout for each transcript GCS sync command")
	transcriptLocalRetention := flags.Duration("transcript-local-retention", 0, "delete local transcript files older than this after successful GCS sync; 0 disables")
	transcriptMaxLocalBytesRaw := flags.String("transcript-max-local-bytes", "0", "max bytes to keep in the local transcript spool after successful GCS sync; supports KiB/MiB/GiB suffixes; 0 disables")
	transcriptAzureRate := flags.String("transcript-azure-max-bytes-per-second", "2MiB", "cap on Azure transcript upload throughput; supports KiB/MiB/GiB suffixes; 0 disables the cap")
	transcriptAzureURL := flags.String("transcript-azure-url", "", "optional Azure blob container URL (https://<account>.blob.core.windows.net/<container>[/<prefix>]) for background transcript sync; defaults to SUBROUTER_TRANSCRIPT_AZURE_URL")
	srSwitchInterval := defaultSRSwitchInterval
	flags.DurationVar(&srSwitchInterval, "sr-switch-interval", defaultSRSwitchInterval, "interval for refreshing OAuth usage scores used by routing; non-positive disables scheduled refresh")
	flags.DurationVar(&srSwitchInterval, "cx-switch-interval", defaultSRSwitchInterval, "compatibility alias for --sr-switch-interval")
	usageScoreTTL := flags.Duration("usage-score-ttl", 30*time.Second, "maximum age for usage scores before account selection refreshes them; 0 disables")
	shutdownTimeout := flags.Duration("shutdown-timeout", 10*time.Minute, "maximum time to drain in-flight proxy requests after SIGTERM/SIGINT")
	adminToken := flags.String("admin-token", "", "admin token required for non-loopback _subrouter endpoints; defaults to SUBROUTER_ADMIN_TOKEN")
	accountImportToken := flags.String("account-import-token", "", "token limited to protected account import; defaults to SUBROUTER_ACCOUNT_IMPORT_TOKEN")
	tailscaleAuth := flags.Bool("tailscale-auth", false, "authenticate non-loopback callers with their tailnet identity instead of a token; defaults to SUBROUTER_TAILSCALE_AUTH")
	tailscaleAuthUsers := flags.String("tailscale-auth-users", "", "comma-separated tailnet logins allowed by --tailscale-auth; empty allows every tailnet peer")
	tailscaleAuthTags := flags.String("tailscale-auth-tags", "", "comma-separated tailnet tags allowed by --tailscale-auth; empty allows every tailnet peer")
	tailscaleCLI := flags.String("tailscale-cli", "", "path to the tailscale CLI used for peer identity")
	requireSessionLeases := flags.Bool("require-session-leases", false, "reject proxy requests without a valid session lease; defaults to SUBROUTER_REQUIRE_SESSION_LEASES")
	maxBodyBytes := flags.Int64("max-body-bytes", 1<<20, "max JSON request body bytes to inspect for session IDs")
	fetchUsage := flags.Bool("fetch-usage", true, "fetch Codex usage on startup for account selection")
	multiTenant := flags.Bool("multi-tenant", false, "reject unknown srt_ tenant keys even before the first tenant exists; tenant routing itself activates automatically once tenants exist")
	publicURL := flags.String("public-url", "", "public Subrouter origin used in hosted tenant responses; defaults to SUBROUTER_PUBLIC_URL")
	stackAPIURL := flags.String("stack-api-url", "", "Stack Auth API base URL; defaults to SUBROUTER_STACK_API_URL")
	stackProjectID := flags.String("stack-project-id", "", "Stack Auth project ID enabling hosted login; defaults to SUBROUTER_STACK_PROJECT_ID")
	stackPublishableClientKey := flags.String("stack-publishable-client-key", "", "Stack Auth publishable client key; defaults to SUBROUTER_STACK_PUBLISHABLE_CLIENT_KEY")
	stackTenantKeySecret := flags.String("stack-tenant-key-secret", "", "local-development override for stable Stack-team tenant keys; deployments use SUBROUTER_STACK_TENANT_KEY_SECRET")
	stackTenantDeleteToken := flags.String("stack-tenant-delete-token", "", "trusted cmux.com token required for hosted tenant exchange and deletion; deployments use SUBROUTER_STACK_TENANT_DELETE_TOKEN")
	stackLegacyKeyGrace := flags.Duration("stack-legacy-key-grace", 30*24*time.Hour, "one-time grace for pre-broker Stack tenant keys; capped at 90 days")
	bedrockEnable := flags.Bool("bedrock", false, "enable the /bedrock/* AWS SigV4 signing gateway for Claude Code Bedrock mode")
	bedrockRegion := flags.String("bedrock-region", "us-east-1", "comma-separated AWS regions for the Bedrock signing gateway")
	bedrockGatewayToken := flags.String("bedrock-gateway-token", "", "optional bearer token clients must present to the Bedrock gateway; defaults to SUBROUTER_BEDROCK_GATEWAY_TOKEN")
	bedrockProfiles := flags.String("bedrock-profiles", "", "comma-separated AWS profiles for the Bedrock gateway; defaults to SUBROUTER_BEDROCK_PROFILES or discovered awN profiles")
	bedrockAutoBump := flags.Bool("bedrock-autobump", false, "request a Service Quotas increase (2x, deduped) when Bedrock throttles Fable/Opus")
	fableBedrockPrimary := flags.Bool("fable-bedrock-primary", false, "route Claude Fable to Bedrock first (before the subscription pool); defaults to SUBROUTER_FABLE_BEDROCK_PRIMARY")
	cloudConfigPath := flags.String("cloud-config", "", "cmux.com team credential config; defaults to ~/.config/subrouter/cloud.json")
	cloudBaseURL := flags.String("cloud-base-url", "", "override the cmux.com API origin loaded from the cloud config")
	cloudCredentialSource := flags.String("cloud-credential-source", "", "override the credential source loaded from the cloud config: team, local, or legacy")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*transcriptGCSURI) != "" && strings.TrimSpace(*transcriptDir) == "" {
		return errors.New("--transcripts is required when --transcript-gcs-uri is set")
	}
	transcriptAzureDestination := strings.TrimSpace(*transcriptAzureURL)
	if transcriptAzureDestination == "" {
		transcriptAzureDestination = strings.TrimSpace(os.Getenv("SUBROUTER_TRANSCRIPT_AZURE_URL"))
	}
	if transcriptAzureDestination != "" && strings.TrimSpace(*transcriptDir) == "" {
		return errors.New("--transcripts is required when the Azure transcript destination is set")
	}
	transcriptMaxLocalBytes, err := parseByteSize(*transcriptMaxLocalBytesRaw)
	if err != nil {
		return fmt.Errorf("transcript-max-local-bytes: %w", err)
	}
	*adminToken, err = secretValue(*adminToken, "SUBROUTER_ADMIN_TOKEN", "SUBROUTER_ADMIN_TOKEN_FILE")
	if err != nil {
		return err
	}
	*accountImportToken, err = secretValue(*accountImportToken, "SUBROUTER_ACCOUNT_IMPORT_TOKEN", "SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE")
	if err != nil {
		return err
	}
	shadowHealthKey, err := shadowHealthKeyFromEnvironment()
	if err != nil {
		return err
	}
	if *publicURL == "" {
		*publicURL = strings.TrimSpace(os.Getenv("SUBROUTER_PUBLIC_URL"))
	}
	if *stackAPIURL == "" {
		*stackAPIURL = strings.TrimSpace(os.Getenv("SUBROUTER_STACK_API_URL"))
	}
	if *stackAPIURL == "" {
		*stackAPIURL = stackauth.DefaultAPIURL
	}
	if *stackProjectID == "" {
		*stackProjectID = strings.TrimSpace(os.Getenv("SUBROUTER_STACK_PROJECT_ID"))
	}
	if *stackPublishableClientKey == "" {
		*stackPublishableClientKey = strings.TrimSpace(os.Getenv("SUBROUTER_STACK_PUBLISHABLE_CLIENT_KEY"))
	}
	*stackTenantKeySecret, err = secretValue(
		*stackTenantKeySecret,
		"SUBROUTER_STACK_TENANT_KEY_SECRET",
		"SUBROUTER_STACK_TENANT_KEY_SECRET_FILE",
	)
	if err != nil {
		return err
	}
	*stackTenantDeleteToken, err = secretValue(
		*stackTenantDeleteToken,
		"SUBROUTER_STACK_TENANT_DELETE_TOKEN",
		"SUBROUTER_STACK_TENANT_DELETE_TOKEN_FILE",
	)
	if err != nil {
		return err
	}
	stackLoginValues := []string{
		*stackProjectID,
		*stackPublishableClientKey,
		*stackTenantKeySecret,
	}
	stackLoginConfigured := 0
	for _, value := range stackLoginValues {
		if value != "" {
			stackLoginConfigured++
		}
	}
	if stackLoginConfigured != 0 && stackLoginConfigured != len(stackLoginValues) {
		return errors.New("hosted Stack login requires all of --stack-project-id, --stack-publishable-client-key, and --stack-tenant-key-secret (or their SUBROUTER_STACK_* environment or secret-file equivalents)")
	}
	if *stackTenantKeySecret != "" && len(*stackTenantKeySecret) < 32 {
		return errors.New("--stack-tenant-key-secret or SUBROUTER_STACK_TENANT_KEY_SECRET must be at least 32 bytes")
	}
	if *stackTenantDeleteToken != "" && len(*stackTenantDeleteToken) < 32 {
		return errors.New("--stack-tenant-delete-token or SUBROUTER_STACK_TENANT_DELETE_TOKEN must be at least 32 bytes")
	}
	*publicURL, err = normalizePublicSubrouterURL(*publicURL)
	if err != nil {
		return err
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
	openRouterUpstream, err := url.Parse(*openRouterUpstreamRaw)
	if err != nil {
		return err
	}
	deepSeekUpstream, err := url.Parse(*deepSeekUpstreamRaw)
	if err != nil {
		return err
	}
	togetherUpstream, err := url.Parse(*togetherUpstreamRaw)
	if err != nil {
		return err
	}
	fireworksUpstream, err := url.Parse(*fireworksUpstreamRaw)
	if err != nil {
		return err
	}
	openCodeZenUpstream, err := url.Parse(*openCodeZenUpstreamRaw)
	if err != nil {
		return err
	}
	grokUpstream, err := url.Parse(*grokUpstreamRaw)
	if err != nil {
		return err
	}
	grokSubscriptionUpstream, err := url.Parse(*grokSubscriptionUpstreamRaw)
	if err != nil {
		return err
	}
	qwenUpstream, err := url.Parse(*qwenUpstreamRaw)
	if err != nil {
		return err
	}
	qwenTokenUpstream, err := url.Parse(*qwenTokenUpstreamRaw)
	if err != nil {
		return err
	}
	qwenAnthropicUpstream, err := url.Parse(*qwenAnthropicUpstreamRaw)
	if err != nil {
		return err
	}
	declaredProviders := make([]proxy.OpenAICompatibleProvider, 0, len(openAICompatibleRaw))
	for _, raw := range openAICompatibleRaw {
		declared, parseErr := proxy.ParseOpenAICompatibleFlag(raw)
		if parseErr != nil {
			return parseErr
		}
		declaredProviders = append(declaredProviders, declared)
	}
	if err := proxy.ConfigureOpenAICompatibleProviders(declaredProviders); err != nil {
		return err
	}
	antigravityUpstream, err := url.Parse(*antigravityUpstreamRaw)
	if err != nil {
		return err
	}

	cloudConfig, err := broker.LoadConfig(*cloudConfigPath)
	if err != nil {
		return fmt.Errorf("load cmux.com config: %w", err)
	}
	if strings.TrimSpace(*cloudBaseURL) != "" {
		cloudConfig.BaseURL = *cloudBaseURL
	}
	if strings.TrimSpace(*cloudCredentialSource) != "" {
		cloudConfig.CredentialSource = broker.CredentialSource(*cloudCredentialSource)
	}
	cloudConfig = cloudConfig.Normalized()
	if err := cloudConfig.Validate(); err != nil {
		return fmt.Errorf("configure cmux.com client: %w", err)
	}
	if cloudConfig.EffectiveCredentialSource() == broker.CredentialSourceTeam &&
		cloudConfig.LoggedIn() &&
		cloudConfig.TeamID == "" {
		return errors.New("cmux.com login has no selected team; run 'sr team use <team>'")
	}
	if cloudConfig.EffectiveCredentialSource() == broker.CredentialSourceTeam &&
		!cloudConfig.TeamModeReady() {
		return errors.New("team credential storage requires login and a hosted tenant; run 'sr login'")
	}
	if cloudConfig.TeamModeReady() &&
		strings.TrimSpace(cloudConfig.LocalProxyToken) == "" {
		return errors.New("team credential storage has no local proxy secret; run 'sr setup' to repair it")
	}
	localProxyToken := cloudServerProxyToken(cloudConfig)
	configuredProxyToken, err := secretFromEnvironment("SUBROUTER_PROXY_TOKEN", "SUBROUTER_PROXY_TOKEN_FILE")
	if err != nil {
		return err
	}
	if configuredProxyToken != "" {
		if localProxyToken != "" && localProxyToken != configuredProxyToken {
			return errors.New("configured proxy secret does not match the cloud config proxy secret")
		}
		localProxyToken = configuredProxyToken
	}
	fableAPIKey := strings.TrimSpace(
		os.Getenv("SUBROUTER_CLAUDE_FABLE_API_KEY"),
	)
	fableBedrockEnabled := *fableBedrockPrimary ||
		envTrue("SUBROUTER_FABLE_BEDROCK_PRIMARY")
	azureCodexConfig, err := azureCodexConfigFromEnvironment()
	if err != nil {
		return err
	}
	if cloudConfig.TeamModeReady() &&
		(*bedrockEnable || fableAPIKey != "" || fableBedrockEnabled || azureCodexConfig != nil) {
		return errors.New(
			"team credential storage cannot use local Bedrock, personal Fable, or Azure Codex credential fallback; remove those options or run 'sr storage local'",
		)
	}
	if azureCodexDisabled() {
		// Say it once at startup. A route that is off by configuration looks
		// identical to one that was never set up, and the difference matters
		// when the pool runs out of quota and nothing catches the traffic.
		slog.Info("azure codex fallback disabled by SUBROUTER_AZURE_CODEX_DISABLED",
			"reenable", "unset SUBROUTER_AZURE_CODEX_DISABLED and restart")
	}
	if azureCodexConfig != nil {
		azureCodexConfig.CostLogPath = filepath.Join(filepath.Dir(*sessionPath), "azure-codex-cost.jsonl")
		azureCodexConfig.PinStorePath = filepath.Join(filepath.Dir(*sessionPath), "azure-codex-pins.json")
		slog.Info("azure codex fallback enabled", "endpoints", azureCodexEndpointNames(azureCodexConfig))
	}
	var credentialBroker proxy.CredentialBroker
	if cloudConfig.TeamModeReady() {
		credentialBroker = broker.NewClient(cloudConfig)
	}
	outboundTransport := proxy.NewOutboundTransport()

	store, err := session.NewStore(*sessionPath)
	if err != nil {
		return err
	}

	// Serving must not perform the account manager's one-time legacy-store
	// migration. Account-manager commands own that import, while a daemon
	// startup may only inspect the effective source and enforce isolation.
	codexStore := accounts.DefaultCodexStoreForReadOnlyInspection()
	// Serving traffic is credential-read-only with respect to the daemon user's
	// interactive Codex login. Stored credentials may refresh for proxy routing,
	// but only explicit account-manager commands may replace auth.json.
	codexStore.DisableActiveAuthSync = true
	// A serving process cannot safely rotate a legacy refresh-token chain whose
	// independence from interactive auth is unknown. Account-manager commands
	// provide the explicit isolated re-enrollment path for both local and shared
	// stores, so serve fails closed until each legacy account is re-added.
	codexStore.RequireIsolatedOAuth = true
	claudeStore := agentclaude.DefaultStoreForReadOnlyInspection()
	oauthSources := []proxy.OAuthAccountSource{agentkimi.ServingStore(), agentgrok.DefaultStore()}
	if *antigravityLocalCredential {
		oauthSources = append(oauthSources, agentantigravity.ServingStore())
	}
	var accountRef *proxy.AccountRef
	var accountGeneration uint64
	var credentialRevision uint64
	var initialAccounts []accounts.Account
	var codexAccounts, claudeAccounts []accounts.Account
	if credentialBroker == nil {
		accountRef, err = proxy.OpenAccountRefWithSources(context.Background(), codexStore, claudeStore, &http.Client{
			Timeout:   15 * time.Second,
			Transport: outboundTransport,
		}, oauthSources)
		if err != nil {
			return err
		}
		var generation, revision uint64
		initialAccounts, generation, revision = accountRef.CredentialSnapshot()
		accountGeneration = generation
		credentialRevision = revision
		codexAccounts, claudeAccounts = schedulerAccountsByProvider(initialAccounts)
	}
	// Start with optimistic fallback scores so the proxy begins accepting
	// connections immediately. Blocking startup on a synchronous usage fetch
	// (an OAuth refresh per account) stalls socket-activated connections during
	// a deploy restart, so the real scores are fetched in the background and
	// swapped in once ready. Per-request 401/429 failover covers the brief
	// window before fresh scores land.
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(fallbackScores(codexAccounts)))
	schedulerRef.AdvanceAccountGenerationWithAccounts(accountGeneration, credentialRevision, initialSchedulerCredentialAccounts(initialAccounts))
	activeGenerationCtx, stopActiveGenerationTasks := context.WithCancel(context.Background())
	defer stopActiveGenerationTasks()
	autoSwitchScoresEnabled := srSwitchInterval > 0 && *fetchUsage && credentialBroker == nil
	startupScores := &startupScoreReadiness{
		required: autoSwitchScoresEnabled && requiresStartupScoreReadiness(initialAccounts),
	}
	var sharedScoreStore *srUsageScoreStore
	if autoSwitchScoresEnabled {
		sharedScoreStore = newSRUsageScoreStore(storepath.StateDir())
	}
	// With auto-switch enabled its immediate, leased sweep is the startup fetch.
	// Running this standalone fetch too would duplicate every usage request and,
	// during a supervisor overlap, bypass the cross-worker lease entirely. Keep
	// the standalone path for interval=0, where auto-switch is intentionally off.
	if shouldStartStandaloneUsageFetch(*fetchUsage, credentialBroker != nil, srSwitchInterval) {
		scoreRevision := schedulerRef.ScoreRevision()
		go func() {
			fetchedScores, successful := fetchCodexScoresWithAccountRef(context.Background(), accountRef, codexAccounts)
			loaded, generation, revision := accountRef.CredentialSnapshot()
			schedulerRef.SyncAccountCredentials(generation, revision, proxy.SchedulerAccounts(loaded))
			if successful > 0 {
				if !schedulerRef.SetForAccountGenerationAtScoreRevision(
					selectacct.NewScheduler(fetchedScores), accountGeneration, scoreRevision,
				) {
					slog.Debug("initial usage score fetch discarded after account reload")
				}
			} else {
				slog.Warn("initial usage score fetch skipped", "reason", "no fresh OAuth usage scores")
			}
		}()
	}
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

	// Tailnet authentication is for self-hosted servers whose port is already
	// restricted to a tailnet by ACL. It is deliberately incompatible with
	// multi-tenant mode: a shared cloud deployment authenticates tenants, not
	// network peers, and must never fall back to trusting a network boundary.
	var tailnetAuthorizer proxy.TailnetAuthorizer
	if *tailscaleAuth || envTrue("SUBROUTER_TAILSCALE_AUTH") {
		if *multiTenant {
			return errors.New("--tailscale-auth cannot be combined with --multi-tenant")
		}
		resolver, err := tailnet.NewResolver(*tailscaleCLI)
		if err != nil {
			return fmt.Errorf("tailscale auth: %w", err)
		}
		authorizer := &tailnet.Authorizer{
			Resolver: resolver,
			Users:    splitAndTrim(*tailscaleAuthUsers),
			Tags:     splitAndTrim(*tailscaleAuthTags),
		}
		tailnetAuthorizer = authorizer
		slog.Info(
			"tailnet authentication enabled",
			"cli", resolver.CLIPath,
			"users", strings.Join(authorizer.Users, ","),
			"tags", strings.Join(authorizer.Tags, ","),
		)
	}

	server := proxy.Server{
		StreamDrops:              &proxy.StreamDropStats{},
		Upstream:                 upstream,
		CodexUpstream:            codexUpstream,
		APIUpstream:              apiUpstream,
		ClaudeUpstream:           claudeUpstream,
		KimiUpstream:             kimiUpstream,
		ZAIUpstream:              zaiUpstream,
		OpenRouterUpstream:       openRouterUpstream,
		DeepSeekUpstream:         deepSeekUpstream,
		TogetherUpstream:         togetherUpstream,
		FireworksUpstream:        fireworksUpstream,
		OpenCodeZenUpstream:      openCodeZenUpstream,
		GrokUpstream:             grokUpstream,
		GrokSubscriptionUpstream: grokSubscriptionUpstream,
		QwenUpstream:             qwenUpstream,
		QwenTokenUpstream:        qwenTokenUpstream,
		QwenAnthropicUpstream:    qwenAnthropicUpstream,
		AntigravityUpstream:      antigravityUpstream,
		LegacyStoreAttestation:   strings.TrimSpace(*localDataSocket) == "" && !envTrue("SUBROUTER_PRIVATE_DATA_ROUTER"),
		Accounts:                 nil,
		AccountRef:               accountRef,
		CredentialBroker:         credentialBroker,
		Sessions:                 store,
		SchedulerRef:             schedulerRef,
		UsageScoreTTL:            usageScoreTTLForServe(*fetchUsage, *usageScoreTTL),
		ReadyCheck:               startupScores.check,
		Transport:                outboundTransport,
		Logger:                   slog.Default(),
		Lifecycle:                proxy.NewLifecycle(),
		AdminToken:               *adminToken,
		ShadowHealthKey:          shadowHealthKey,
		AccountImportToken:       *accountImportToken,
		TailnetAuth:              tailnetAuthorizer,
		RequireSessionLease:      *requireSessionLeases || envTrue("SUBROUTER_REQUIRE_SESSION_LEASES"),
		ForwardSessionHeaders:    envTrue("SUBROUTER_FORWARD_SESSION_HEADERS"),
		LocalProxyToken:          localProxyToken,
		MaxBodyBytes:             *maxBodyBytes,
		Bedrock:                  bedrockConfig,
		ClaudeFableAPIKey:        fableAPIKey,
		// SUBROUTER_FABLE_CACHE_1H_OFF=1 disables the ephemeral->1h
		// cache_control TTL upgrade on the Bedrock path.
		ClaudeFableCacheTTLUpgradeOff: envTrue("SUBROUTER_FABLE_CACHE_1H_OFF"),
		AzureCodex:                    azureCodexConfig,
		FableBedrockPrimary:           fableBedrockEnabled,
		Transcripts:                   transcript.NewRecorder(*transcriptDir),
	}
	if err := server.ValidateCredentialUpstreams(); err != nil {
		return err
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
	transcriptAzureKey, err := secretFromEnvironment("SUBROUTER_TRANSCRIPT_AZURE_KEY", "SUBROUTER_TRANSCRIPT_AZURE_KEY_FILE")
	if err != nil {
		return fmt.Errorf("transcript azure key: %w", err)
	}
	transcriptAzureMaxBytesPerSecond, err := parseByteSize(*transcriptAzureRate)
	if err != nil {
		return fmt.Errorf("transcript-azure-max-bytes-per-second: %w", err)
	}
	if transcriptAzureMaxBytesPerSecond == 0 {
		// The syncer reads zero as "use the default", so an explicit 0 from the
		// operator has to say "no cap" in the syncer's own terms.
		transcriptAzureMaxBytesPerSecond = -1
	}
	transcriptAzureSyncer := transcript.NewAzureBlobSyncer(transcript.AzureBlobSyncerConfig{
		MaxBytesPerSecond: transcriptAzureMaxBytesPerSecond,
		SourceDir:         *transcriptDir,
		Destination:       transcriptAzureDestination,
		AccountKey:        transcriptAzureKey,
		SASToken:          strings.TrimSpace(os.Getenv("SUBROUTER_TRANSCRIPT_AZURE_SAS")),
		Interval:          *transcriptGCSSyncInterval,
		Timeout:           *transcriptGCSSyncTimeout,
		LocalRetention:    *transcriptLocalRetention,
		MaxLocalBytes:     transcriptMaxLocalBytes,
		Logger:            slog.Default(),
	})
	if transcriptAzureSyncer.Enabled() {
		slog.Info("transcript azure sync enabled", "destination", transcriptAzureSyncer.Destination(), "interval", (*transcriptGCSSyncInterval).String())
		go transcriptAzureSyncer.Run(context.Background())
	} else if transcriptAzureDestination != "" {
		// A destination that produces no syncer means the credential is missing
		// or the URL is malformed. Saying so at startup is the difference
		// between a broken pipeline and a silent one, which is how the last
		// transcript sync stopped without anyone noticing.
		slog.Warn("transcript azure sync is configured but not usable",
			"destination", transcriptAzureDestination,
			"fix", "set SUBROUTER_TRANSCRIPT_AZURE_KEY_FILE (or SUBROUTER_TRANSCRIPT_AZURE_SAS) and check the container URL")
	}
	if autoSwitchScoresEnabled {
		fetchScores := func(ctx context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			scores, successful := fetchCodexScoresWithAccountRef(ctx, accountRef, candidates)
			loaded, generation, revision := accountRef.CredentialSnapshot()
			schedulerRef.SyncAccountCredentials(generation, revision, proxy.SchedulerAccounts(loaded))
			return scores, successful
		}
		go func() {
			if err := ensureStartupScores(activeGenerationCtx, startupScoreConfig{
				Interval:         srSwitchInterval,
				AccountsSnapshot: accountRef.Snapshot,
				RefreshAccounts: func() error {
					loaded, generation, err := accountRef.ReloadSnapshot()
					if err != nil {
						return err
					}
					_, _, revision := accountRef.CredentialSnapshot()
					schedulerRef.AdvanceAccountGenerationWithAccounts(
						generation, revision, proxy.SchedulerAccounts(loaded),
					)
					return nil
				},
				SchedulerRef:     schedulerRef,
				FetchScores:      fetchScores,
				Store:            sharedScoreStore,
				RetryFailedSweep: true,
			}); err != nil && activeGenerationCtx.Err() == nil {
				slog.Error("startup Codex usage scores unavailable", "error", err)
				return
			}
			if activeGenerationCtx.Err() == nil {
				startupScores.ready.Store(true)
			}
		}()
		go runSRAutoSwitch(activeGenerationCtx, srAutoSwitchConfig{
			Interval:             srSwitchInterval,
			AccountsSnapshotFunc: accountRef.Snapshot,
			Sessions:             store,
			SchedulerRef:         schedulerRef,
			Logger:               slog.Default(),
			FetchScores:          fetchScores,
			Lease:                newSRAutoSwitchLease(storepath.StateDir()),
			DelayFirstSweep:      true,
			ScoreSnapshots:       sharedScoreStore,
		})
	} else if srSwitchInterval > 0 && credentialBroker == nil {
		slog.Info("sr auto-switch disabled because usage fetching is disabled", "interval", srSwitchInterval.String())
	}

	// A server with no import credential rejects every `sr add`, and no other
	// signal reports it: ordinary admin reads stay open, health stays green, and
	// the accounts already on disk keep serving traffic. Say it once at startup
	// so the gap is visible when it appears rather than when someone next needs
	// to onboard an account.
	if !*multiTenant && server.AccountImportState() == proxy.AccountImportDisabled {
		slog.Warn(
			"account import is disabled: no admin or account-import credential is configured, so this server rejects every sr add",
			"fix", "set SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE and SUBROUTER_ADMIN_TOKEN_FILE, or run sr server install <name>",
		)
	}

	tenantRegistry := tenant.NewRegistry(storepath.StateDir())
	multiTenantHandler := &proxy.MultiTenant{
		Base:          server,
		Registry:      tenantRegistry,
		TranscriptDir: *transcriptDir,
		Enabled:       *multiTenant,
		PublicURL:     *publicURL,
	}
	if *stackProjectID != "" {
		stackHTTPClient := &http.Client{Timeout: 15 * time.Second}
		multiTenantHandler.StackVerifier = &stackauth.Verifier{
			APIURL: *stackAPIURL, ProjectID: *stackProjectID,
			HTTPClient: stackHTTPClient,
		}
		multiTenantHandler.StackTeams = &stackauth.Client{
			APIURL: *stackAPIURL, ProjectID: *stackProjectID,
			PublishableClientKey: *stackPublishableClientKey,
			HTTPClient:           stackHTTPClient,
		}
		multiTenantHandler.StackProjectID = *stackProjectID
		multiTenantHandler.StackTenantKeySecret = []byte(*stackTenantKeySecret)
		multiTenantHandler.StackTenantDeleteToken = []byte(*stackTenantDeleteToken)
		cutoff, err := tenantRegistry.EnsureLegacyCredentialCutoff(
			time.Now(),
			*stackLegacyKeyGrace,
		)
		if err != nil {
			return fmt.Errorf("initialize legacy Stack credential cutoff: %w", err)
		}
		multiTenantHandler.StackLegacyKeyCutoff = cutoff
		slog.Info(
			"legacy Stack tenant credentials have a fixed migration cutoff",
			"cutoff", cutoff.Format(time.RFC3339),
		)
	}
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           multiTenantHandler.Handler(server.Handler()),
		ConnContext:       proxy.LocalDataConnContext,
		ReadHeaderTimeout: 10 * time.Second,
		// Bound how long an idle client connection can pin this worker. The
		// supervisor's drain waits for a retired generation's connections to
		// close and never times out, so without this a client that simply holds
		// a keep-alive connection keeps an obsolete worker alive forever: one
		// from 4.5 hours and two upgrades earlier was still serving 64
		// connections on a pool sizing that had since been fixed, so deployed
		// fixes never reached it. Closing a connection that has been idle this
		// long costs a client nothing (it reconnects on its next request, onto
		// the current generation) and never interrupts work in flight, since
		// IdleTimeout only applies between requests.
		IdleTimeout: workerIdleTimeout,
	}

	if upstream != nil {
		slog.Info("subrouter listening", "addr", *addr, "upstream", upstream.String(), "codex_accounts", len(codexAccounts), "claude_accounts", len(claudeAccounts), "cloud_team", cloudConfig.TeamID, "transcripts", *transcriptDir, "transcript_gcs_uri", *transcriptGCSURI)
	} else {
		slog.Info("subrouter listening", "addr", *addr, "codex_upstream", codexUpstream.String(), "api_upstream", apiUpstream.String(), "claude_upstream", claudeUpstream.String(), "codex_accounts", len(codexAccounts), "claude_accounts", len(claudeAccounts), "cloud_team", cloudConfig.TeamID, "transcripts", *transcriptDir, "transcript_gcs_uri", *transcriptGCSURI)
	}
	return listenAndServeWithSignalsAndLocalSocket(httpServer, *localDataSocket, server.Lifecycle, *shutdownTimeout, slog.Default(), stopActiveGenerationTasks)
}

func schedulerAccountsByProvider(all []accounts.Account) (codex, claude []accounts.Account) {
	for _, account := range all {
		switch account.Provider {
		case accounts.ProviderCodex:
			codex = append(codex, account)
		case accounts.ProviderClaude:
			claude = append(claude, account)
		}
	}
	return codex, claude
}

func initialSchedulerCredentialAccounts(all []accounts.Account) []accounts.Account {
	return proxy.SchedulerAccounts(all)
}

// requiresStartupScoreReadiness keeps non-Codex-only servers available. The
// startup scorer intentionally ignores API-key and non-Codex accounts, so an
// empty Codex OAuth set has no score sweep that can make the server ready.
func requiresStartupScoreReadiness(all []accounts.Account) bool {
	return len(codexOAuthAccounts(all)) > 0
}

func validatePublicSubrouterURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("--public-url or SUBROUTER_PUBLIC_URL must be an origin such as https://sr.example.com")
	}
	host := strings.ToLower(parsed.Hostname())
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if parsed.Scheme != "https" &&
		!(parsed.Scheme == "http" && loopback) {
		return errors.New("--public-url or SUBROUTER_PUBLIC_URL must use HTTPS, except on loopback")
	}
	return nil
}

func normalizePublicSubrouterURL(raw string) (string, error) {
	normalized := strings.TrimSpace(raw)
	if err := validatePublicSubrouterURL(normalized); err != nil {
		return "", err
	}
	return strings.TrimSuffix(normalized, "/"), nil
}

func loadProxyAccounts(
	ctx context.Context,
	teamMode bool,
	codexStore accounts.CodexStore,
	claudeStore agentclaude.Store,
) ([]accounts.Account, []accounts.Account, error) {
	if teamMode {
		// Team mode never reads local provider credentials. Besides enforcing
		// central refresh custody, this keeps a corrupt legacy file from taking
		// down an otherwise healthy team daemon.
		return nil, nil, nil
	}
	codexAccounts, err := codexStore.List()
	if err != nil {
		return nil, nil, err
	}
	claudeAccounts, err := claudeStore.ListAccounts(ctx)
	if err != nil {
		slog.Warn("Claude accounts skipped", "error", err)
		claudeAccounts = nil
	}
	return codexAccounts, claudeAccounts, nil
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

// workerIdleTimeout caps how long a client connection may sit idle on this
// worker. Google Cloud's global Application Load Balancer keeps backend HTTP
// connections idle for up to 600 seconds, so the worker must wait longer or a
// GFE can reuse a connection just as the worker closes it and surface a 502.
// Retired generations still drain once their load-balancer connections expire.
const workerIdleTimeout = 620 * time.Second

func listenAndServeWithSignalsAndLocalSocket(server *http.Server, socket string, lifecycle *proxy.Lifecycle, shutdownTimeout time.Duration, logger *slog.Logger, stopActiveGenerationTasks ...func()) error {
	socket = filepath.Clean(strings.TrimSpace(socket))
	if socket == "." || socket == "" {
		return listenAndServeWithSignals(server, lifecycle, shutdownTimeout, logger, stopActiveGenerationTasks...)
	}
	listener, err := openPrivateLocalDataListener(socket)
	if err != nil {
		return fmt.Errorf("local-data-socket: %w", err)
	}
	defer func() {
		_ = listener.Close()
	}()
	localErrCh := make(chan error, 1)
	go func() {
		serveErr := server.Serve(listener)
		if errors.Is(serveErr, http.ErrServerClosed) || errors.Is(serveErr, net.ErrClosed) {
			serveErr = nil
		}
		localErrCh <- serveErr
	}()
	return listenAndServeWithSignalsExtra(server, lifecycle, shutdownTimeout, logger, localErrCh, stopActiveGenerationTasks...)
}

func openPrivateLocalDataListener(socket string) (net.Listener, error) {
	if !filepath.IsAbs(socket) || socket == string(filepath.Separator) {
		return nil, errors.New("path must be absolute")
	}
	parent, err := validatePrivateLocalServingStorePath(filepath.Dir(socket), true)
	if err != nil || parent != filepath.Dir(socket) {
		return nil, errors.New("parent must be canonical, current-user-owned, and not group/world writable")
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return nil, err
	}
	lease, err := acquireLocalDataSocketLease(socket)
	if err != nil {
		return nil, err
	}
	releaseLease := true
	defer func() {
		if releaseLease {
			_ = lease.Close()
		}
	}()
	if !lease.parentMatches(parentInfo) {
		return nil, errors.New("local data socket parent changed during lease acquisition")
	}
	if err := lease.removeStaleSocket(); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	if currentParent, err := os.Stat(parent); err != nil || !lease.parentMatches(currentParent) {
		_ = listener.Close()
		return nil, errors.New("local data socket parent changed during listener creation")
	}
	if err := unixFchmodatLocalDataSocket(lease, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	owned, err := wrapOwnedLocalDataListener(listener, socket, lease)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	releaseLease = false
	return owned, nil
}

func listenAndServeWithSignals(server *http.Server, lifecycle *proxy.Lifecycle, shutdownTimeout time.Duration, logger *slog.Logger, stopActiveGenerationTasks ...func()) error {
	return listenAndServeWithSignalsExtra(server, lifecycle, shutdownTimeout, logger, nil, stopActiveGenerationTasks...)
}

func listenAndServeWithSignalsExtra(server *http.Server, lifecycle *proxy.Lifecycle, shutdownTimeout time.Duration, logger *slog.Logger, extraErrCh <-chan error, stopActiveGenerationTasks ...func()) error {
	errCh := make(chan error, 1)
	go func() {
		err := listenAndServeHTTP(server, logger)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	sigCh := make(chan os.Signal, 2)
	watched := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if retireSignal != nil {
		watched = append(watched, retireSignal)
	}
	signal.Notify(sigCh, watched...)
	defer signal.Stop(sigCh)

	for {
		select {
		case err := <-errCh:
			return err
		case err := <-extraErrCh:
			if err != nil {
				_ = server.Close()
				return fmt.Errorf("private local data listener: %w", err)
			}
			return nil
		case sig := <-sigCh:
			if retireSignal != nil && sig == retireSignal {
				// Retired by the supervisor: a newer generation now owns new
				// connections, but clients holding keep-alive connections would
				// otherwise stay pinned to this worker indefinitely, since the
				// drain has no timeout. Disabling keep-alives makes every
				// response carry "Connection: close", so each client finishes
				// its current request and reconnects onto the new generation.
				// In-flight requests and streams are unaffected.
				retireServer(server, logger, stopActiveGenerationTasks...)
				continue
			}
			return shutdownOnSignal(server, lifecycle, sig, shutdownTimeout, logger, errCh)
		}
	}
}

// retireServer stops connection reuse without interrupting anything in flight.
func retireServer(server *http.Server, logger *slog.Logger, stopActiveGenerationTasks ...func()) {
	for _, stop := range stopActiveGenerationTasks {
		if stop != nil {
			stop()
		}
	}
	server.SetKeepAlivesEnabled(false)
	if logger != nil {
		logger.Info("subrouter worker retired; closing idle connections and asking clients to reconnect")
	}
}

func shutdownOnSignal(server *http.Server, lifecycle *proxy.Lifecycle, sig os.Signal, shutdownTimeout time.Duration, logger *slog.Logger, errCh chan error) error {
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

func listenAndServeHTTP(server *http.Server, logger *slog.Logger) error {
	listener, err := inheritedListenerFromEnv()
	if err != nil {
		return err
	}
	if listener != nil {
		if logger != nil {
			logger.Info("using inherited supervisor socket", "addr", listener.Addr().String())
		}
		return server.Serve(listener)
	}
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
	return fetchCodexScoresWithRefresh(ctx, codexAccounts, func(
		ctx context.Context, client *http.Client, account accounts.Account,
	) (accounts.Account, error) {
		stored, ok, err := store.FindStored(account.ID)
		if err != nil || !ok {
			return account, err
		}
		refreshed, _, err := store.RefreshStoredIfExpired(
			accounts.WithCodexRefreshReason(ctx, "serve.fetch-usage"), client, stored,
		)
		if err != nil {
			return account, err
		}
		if refreshedAccount, ok := refreshed.Account(refreshed.SourcePath(store)); ok {
			return refreshedAccount, nil
		}
		return account, nil
	})
}

func fetchCodexScoresWithAccountRef(ctx context.Context, ref *proxy.AccountRef, codexAccounts []accounts.Account) ([]selectacct.Score, int) {
	return fetchCodexScoresWithRefresh(ctx, codexAccounts, func(
		ctx context.Context, _ *http.Client, account accounts.Account,
	) (accounts.Account, error) {
		return ref.Refresh(accounts.WithCodexRefreshReason(ctx, "serve.fetch-usage"), account)
	})
}

func shouldStartStandaloneUsageFetch(fetchUsage, credentialBrokerConfigured bool, switchInterval time.Duration) bool {
	return fetchUsage && !credentialBrokerConfigured && switchInterval <= 0
}

const codexUsageFetchConcurrency = 4

func fetchCodexScoresWithRefresh(
	ctx context.Context,
	codexAccounts []accounts.Account,
	refresh func(context.Context, *http.Client, accounts.Account) (accounts.Account, error),
) ([]selectacct.Score, int) {
	codexAccounts, _ = schedulerAccountsByProvider(codexAccounts)
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
	oauthAccounts := make([]accounts.Account, 0, len(codexAccounts))
	for _, account := range codexAccounts {
		if account.AuthMode == accounts.AuthModeOAuth {
			oauthAccounts = append(oauthAccounts, account)
		}
	}
	jobs := make(chan accounts.Account, len(oauthAccounts))
	for _, account := range oauthAccounts {
		jobs <- account
	}
	close(jobs)
	workers := min(codexUsageFetchConcurrency, len(oauthAccounts))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for account := range jobs {
				refreshed, err := refresh(ctx, client, account)
				if err != nil {
					slog.Warn("account refresh failed", "account", account.ID, "error", err)
					mu.Lock()
					if idx, ok := scoreByID[selectacct.ScoreKey(account.Provider, account.ID)]; ok {
						scores[idx] = selectacct.Score{AccountID: account.ID, Provider: account.Provider, Headroom: 0, ShortHeadroom: 0}
					}
					mu.Unlock()
					continue
				}
				account = refreshed
				windows, err := accounts.FetchCodexUsage(ctx, client, account)
				if err != nil {
					slog.Warn("usage fetch failed", "account", account.ID, "error", err)
					mu.Lock()
					if idx, ok := scoreByID[selectacct.ScoreKey(account.Provider, account.ID)]; ok {
						scores[idx] = selectacct.Score{AccountID: account.ID, Provider: account.Provider, Headroom: 0}
					}
					mu.Unlock()
					continue
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
			}
		}()
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
		if account.Provider != accounts.ProviderCodex {
			continue
		}
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

Getting started:
  %[1]s login              Sign in to cmux.com with Stack Auth and choose a team
  %[1]s setup              Install and start the local proxy daemon, then verify it
  %[1]s setup --storage local
                           Set up this machine without shared credentials
  %[1]s doctor             Diagnose login, team vault, daemon, and local egress
  %[1]s cleanup            Remove the local daemon (--yes to apply, --purge for local credentials)
  %[1]s version            Print build version, commit, and build date

Credential storage:
  %[1]s storage            Show the active credential source
  %[1]s storage hosted     Use credentials hosted for the selected Stack team
  %[1]s storage local      Keep and use credentials only on this machine
  %[1]s storage legacy     Use the selected legacy remote Subrouter server

Team vault management:
  %[1]s team list          List Stack Auth teams
  %[1]s team current       Show the selected team
  %[1]s team use <team>    Select the team whose credentials this machine leases
  %[1]s account list       List credentials shared with the selected team
  %[1]s account add <codex|claude|openai-key|anthropic-key>
                           Add one credential to the selected team vault
  %[1]s account import --only <label> [--dry-run]
                           Copy one local credential for a canary
  %[1]s account import --all --dry-run
                           Copy local credentials into the selected team vault
  %[1]s account import --all --yes
                           Confirm the reviewed bulk upload
  %[1]s account repair <id>
                           Replace a broken shared credential in place
  %[1]s account remove <id>
  %[1]s logout             Revoke this machine's cmux.com session

Usage:
  %[1]s                    Show usage across all configured providers
  %[1]s add                Add an account; asks whether it is Codex or Claude
  %[1]s add codex          Add a Codex account (opens OAuth login)
  %[1]s add claude         Add a Claude account (opens OAuth login)
  %[1]s add-key            Add an API key account
  %[1]s import             Import current ~/.codex/auth.json account
  %[1]s list               List all Codex accounts
  %[1]s switch [email]     Switch active Codex account and sync OpenCode/pi
  %[1]s g [email]          Switch active account, sync OpenCode/pi, and restart Codex.app
  %[1]s gui [email]        Switch active account, sync OpenCode/pi, and restart Codex.app
  %[1]s gui-switch [email] Switch active account, sync OpenCode/pi, and restart Codex.app
  %[1]s remove <account>   Remove from an explicitly bound local state; selected-server removal is not yet supported
  %[1]s status             Show usage across all configured providers (non-interactive)
  %[1]s qwen login [--console-account <email-or-label>] <account>
                           Authorize live Qwen Token Plan quota status
  %[1]s qwen [args]        Launch Qwen Code through the selected Token Plan pool
  %[1]s qwen --account [account] [-- args]
                           Pin one Qwen account with no account failover
  %[1]s qwen proxy [args]  Explicit launcher alias for %[1]s qwen
  %[1]s kimi login <label> Add an isolated Kimi subscription account
  %[1]s kimi [args]        Launch Kimi Code through the selected Kimi pool
  %[1]s kimi --account [account] [-- args]
                           Pin one Kimi account with no account failover
  %[1]s kimi proxy [args]  Explicit launcher alias for %[1]s kimi
  %[1]s kimi list          List Kimi CLI and managed subscription accounts
  %[1]s kimi remove <label>
                           Remove one managed Kimi subscription account
  %[1]s pick               Switch to the recommended account, failing if none has quota
  %[1]s reset [email]      Redeem a rate-limit reset credit (best candidate, or --all, or --dry-run)
  %[1]s usage [days]       Refresh and show API-key spend
  %[1]s trace <email>      Show OAuth refresh breadcrumbs for an account
  %[1]s codex isolation-check [--json] [--retiring-state-dir PATH]
                           Check serving credential isolation without changing credentials
  %[1]s codex migrate-isolation [--device-auth]
                           Re-enroll legacy Codex OAuth accounts without changing local Codex auth
  %[1]s codex enroll-isolated --retiring-state-dir PATH [--device-auth] [--only ACCOUNT]...
                           Enroll the full isolated candidate by default; repeat --only for
                           validation-only accounts (partial candidates cannot activate)

  %[1]s remote -v          List local, cmux hosted, and self-hosted remotes
  %[1]s remote use local   Route agents through this computer
  %[1]s remote use cmux    Route agents through hosted cmux
  %[1]s remote add <name> <url>
                           Add a self-hosted Subrouter
  %[1]s remote use <name>  Route agents through a self-hosted Subrouter

  %[1]s daemon start       Start this machine's local proxy
  %[1]s daemon stop        Stop this machine's local proxy
  %[1]s daemon restart     Restart this machine's local proxy
  %[1]s daemon status      Show local proxy health
  %[1]s daemon logs        Follow local proxy logs

  %[1]s server             Manage legacy remote Subrouter servers
  %[1]s server up          Compatibility alias for daemon start
  %[1]s server down        Compatibility alias for daemon stop
  %[1]s server restart     Compatibility alias for daemon restart
  %[1]s server status      Compatibility health view
  %[1]s server add <name> --url <url> [--default]
  %[1]s server use <name|local> [--no-codex-config]
  %[1]s server rename <old> <new>
  %[1]s server install <name>
  %[1]s server login <name> [--device-auth]
  %[1]s server sync <name> [--device-auth] [--yes]

  %[1]s tenant create <name> [--server <name>]     Create an isolated per-tenant account pool
  %[1]s tenant list [--server <name>]
  %[1]s tenant key create <tenant> [--server <name>]
  %[1]s tenant key revoke <tenant> <key-prefix> [--server <name>]

  %[1]s admin-keys         List stored OpenAI admin keys
  %[1]s add-admin-key      Add an sk-admin-* key
  %[1]s remove-admin-key <label>
  %[1]s attach-project <api-key-label> [--project-id <id-or-name>]

  %[1]s claude             Interactively launch pooled Claude through Subrouter
  %[1]s claude-aws [--model fable] [claude args...]
                           Launch Claude Code on AWS Bedrock via the server (Fable 5)
  %[1]s claude-direct [claude args...]
                           Launch Claude Code directly on Anthropic (bypass subrouter)
  %[1]s agy                Launch AGY through the pooled Cloud Code route (use --account to pin; plain agy stays direct)
  %[1]s agy add <label>    Import the current plain agy OAuth login as an isolated account
  %[1]s agy list           List isolated Antigravity accounts
  %[1]s agy recover        Restore a native profile swap left by a crash
  %[1]s agy remove <label> Remove one isolated Antigravity account
  %[1]s spend              Show AWS Bedrock spend tracked by the server
  %[1]s gemini             Manage Gemini profiles (routing scaffold only)

  %[1]s serve [--addr 127.0.0.1:31415] [--fetch-usage=true] [--multi-tenant] [--codex-upstream URL] [--claude-upstream URL] [--kimi-upstream URL] [--zai-upstream URL] [--openrouter-upstream URL] [--deepseek-upstream URL] [--together-upstream URL] [--fireworks-upstream URL] [--opencode-zen-upstream URL] [--grok-upstream URL] [--grok-subscription-upstream URL] [--qwen-upstream URL] [--qwen-token-upstream URL] [--qwen-anthropic-upstream URL] [--antigravity-upstream URL] [--openai-compatible name=URL] [--transcripts DIR] [--transcript-gcs-uri gs://bucket/prefix] [--transcript-gcs-sync-timeout 30m] [--transcript-local-retention 24h] [--transcript-max-local-bytes 2GiB]
  %[1]s supervise --worker-bin PATH [--addr 127.0.0.1:31415] [--control-socket /var/run/subrouter-supervisor.sock] [--local-data-socket PATH] [--upgrade-inhibit-file PATH] [--expect-proxy-protocol] [--drain-timeout 10m] [--worker-stop-grace 30s] -- [serve flags]
  %[1]s front --backend-id ID --backend-address ADDRESS [--backend-network tcp|unix] [--addr 127.0.0.1:31415] [--control-socket /var/run/subrouter-front.sock] [--listener-transfer-socket /var/run/subrouter-front-listener.sock]
  %[1]s probe [--url http://127.0.0.1:31415]
  %[1]s accounts
  %[1]s codex [codex args...]
  %[1]s install-daemon [--start=true]       macOS LaunchAgent
  %[1]s install-systemd [--start=true]      Linux systemd service
  %[1]s install-launchd [--start=true]      macOS shared-server credentials

Session stickiness:
  Prefer sending X-Subrouter-Session per conversation.
  Send X-Subrouter-Agent when the client is not Codex.
  Send X-Subrouter-User-Email for teammate-level observability.
  Send X-Subrouter-Account-ID to force a specific account, including an API-key account.
  Subrouter refreshes routing scores every 10m by default; set --sr-switch-interval=0 to disable.
  For %[1]s codex, set SUBROUTER_CODEX_USER_EMAIL and/or SUBROUTER_CODEX_ACCOUNT_ID instead.
  The proxy also checks common session headers, query params, and small JSON bodies.
`, program)
}

// splitAndTrim parses a comma-separated flag value into non-empty entries.
func splitAndTrim(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// stringList collects a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
)

const (
	antigravityProxyHelp = `Usage: sr antigravity
       sr agy

Launch Antigravity through Subrouter's pooled Cloud Code route. Plain 'agy' remains
the direct CLI. Subrouter selects and pins isolated OAuth accounts server-side,
refreshes credentials, and fails over bounded transient/authentication errors.
`
	kimiProxyHelp = `Usage: sr kimi [--account [account]] -p <prompt> [kimi args...]
       sr kimi proxy [--account [account]] -p <prompt> [kimi args...]

Run a non-interactive Kimi prompt through the selected Subrouter pool.
Plain 'kimi' remains the direct interactive CLI.
Omit --account for pooled failover. A named account is pinned with no account failover;
bare --account opens a pinned-account picker.
The child gets a private routed-only home while its session store remains linked.
Kimi credential, migration/update, ACP, web, and server modes require the plain direct CLI.
Interactive routed launches are disabled because Kimi has no enforced slash-command denylist.
New sessions and --continue keep working-directory affinity; an explicit session ID keeps
the same affinity across working directories.
Use -p with --session <id>, --resume <id>, or --continue; session pickers are not supported.
`
	qwenProxyHelp = `Usage: sr qwen [--account [account]] [-- qwen args...]
       sr qwen proxy [--account [account]] [-- qwen args...]

Launch Qwen Code through the selected Qwen Token Plan pool. Plain 'qwen' remains direct.
Omit --account for pooled failover. A named account is pinned with no account failover;
bare --account opens a pinned-account picker.
The process-only routing launcher preserves Qwen's normal session store.
It forces Qwen's bare mode, so saved settings, extensions, skills, and MCP servers are not loaded.
Account affinity is stable per working directory for new routed sessions.
Qwen resume/continue can restore a saved direct provider route, so routed launches reject them.
Qwen serve/ACP, review, and model-bearing channel-service modes can reload saved environment routing,
so use plain 'qwen' for those modes.
`
)

type nativeProxySpec struct {
	command  string
	display  string
	agent    string
	route    string
	provider accounts.Provider
	authMode accounts.AuthMode
}

type nativeProxyLaunchOptions struct {
	accountSelector   string
	pickPinnedAccount bool
}

var (
	antigravityNativeProxy = nativeProxySpec{
		command: "agy", display: "Antigravity", agent: "antigravity",
		route: "antigravity", provider: accounts.ProviderAntigravity, authMode: accounts.AuthModeOAuth,
	}
	kimiNativeProxy = nativeProxySpec{
		command: "kimi", display: "Kimi", agent: "kimi",
		route: "kimi", provider: accounts.ProviderKimi,
	}
	qwenNativeProxy = nativeProxySpec{
		command: "qwen", display: "Qwen", agent: "qwen-token",
		route: "qwen-token", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey,
	}
)

func (r srRunner) antigravityCommand(ctx context.Context, args []string) error {
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(r.out, antigravityManagementHelp)
		return nil
	}
	options, vendorArgs, err := parseNativeProxyLaunchArgs(args)
	if err != nil {
		return err
	}
	// Keep the provider launchers consistent: `proxy` is an optional alias for
	// routed Kimi/Qwen launches, and must not be passed through as an AGY
	// vendor argument (where it would be interpreted as an unknown command).
	if len(vendorArgs) > 0 && vendorArgs[0] == "proxy" {
		vendorArgs = vendorArgs[1:]
	}
	// AGY exposes a supported CLOUD_CODE_URL override. Use the same routed
	// launcher as the other native providers so the server owns account
	// selection, quota affinity, refresh, and bounded failover. Plain `agy`
	// remains completely direct and untouched.
	return r.launchNativeProxy(ctx, antigravityNativeProxy, vendorArgs, options)
}

func (r srRunner) launchKimiProxy(ctx context.Context, args []string) error {
	options, vendorArgs, err := parseNativeProxyLaunchArgs(args)
	if err != nil {
		return err
	}
	if mode := kimiProxyReloadCapableMode(vendorArgs); mode != "" {
		if mode == "login" || mode == "provider" {
			return fmt.Errorf("%q changes Kimi's local credentials; use plain 'kimi %s' for the direct CLI or 'sr kimi login <label>' to manage the routed pool", mode, mode)
		}
		return fmt.Errorf("Kimi %s mode can start credential or provider control surfaces; use plain 'kimi %s' for the direct CLI", mode, mode)
	}
	if nativeProxyResumePickerRequested(kimiNativeProxy, vendorArgs) {
		return errors.New("'sr kimi --session' requires an explicit session ID so Subrouter can preserve sticky account routing")
	}
	if !kimiProxyPromptModeRequested(vendorArgs) {
		return errors.New("interactive 'sr kimi' is disabled because Kimi has no supported way to disable routing and server-launching slash commands; use 'sr kimi -p <prompt>' for routed prompt mode or plain 'kimi' for the direct interactive CLI")
	}
	return r.launchNativeProxy(ctx, kimiNativeProxy, vendorArgs, options)
}

func kimiProxyPromptModeRequested(args []string) bool {
	promptMode := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			// The main Kimi command has no positional prompt. Anything after its
			// own delimiter is therefore a subcommand or an invalid positional.
			return false
		case arg == "-p" || arg == "--prompt":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return false
			}
			promptMode = true
			i++
		case strings.HasPrefix(arg, "--prompt="):
			if strings.TrimSpace(strings.TrimPrefix(arg, "--prompt=")) == "" {
				return false
			}
			promptMode = true
		case strings.HasPrefix(arg, "-p="):
			if strings.TrimSpace(strings.TrimPrefix(arg, "-p=")) == "" {
				return false
			}
			promptMode = true
		case strings.HasPrefix(arg, "-p") && len(arg) > len("-p"):
			if strings.TrimSpace(strings.TrimPrefix(arg, "-p")) == "" {
				return false
			}
			promptMode = true
		case arg == "-S" || arg == "--session" || arg == "-r" || arg == "--resume":
			// Commander consumes an optional session ID only when it is not
			// another option, so `--session ID -p ...` and `--session -p ...`
			// retain the same distinction as Kimi itself.
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
		case strings.HasPrefix(arg, "--session=") || strings.HasPrefix(arg, "--resume=") ||
			(strings.HasPrefix(arg, "-S") && len(arg) > len("-S")) ||
			(strings.HasPrefix(arg, "-r") && len(arg) > len("-r")):
			continue
		case arg == "-m" || arg == "--model" || arg == "--output-format" ||
			arg == "--skills-dir" || arg == "--agent" || arg == "--agent-file" || arg == "--add-dir":
			// Required option values may themselves begin with '-'. Consume them
			// before looking for -p so a value cannot masquerade as prompt mode.
			if i+1 >= len(args) {
				return false
			}
			i++
		case strings.HasPrefix(arg, "--model=") || strings.HasPrefix(arg, "--output-format=") ||
			strings.HasPrefix(arg, "--skills-dir=") || strings.HasPrefix(arg, "--agent=") ||
			strings.HasPrefix(arg, "--agent-file=") || strings.HasPrefix(arg, "--add-dir=") ||
			(strings.HasPrefix(arg, "-m") && len(arg) > len("-m")):
			continue
		case arg == "-c" || arg == "--continue" || arg == "-C" ||
			arg == "-y" || arg == "--yolo" || arg == "--yes" || arg == "--auto-approve" ||
			arg == "--auto" || arg == "--plan":
			continue
		default:
			// Kimi's main command accepts no positional arguments. This rejects
			// subcommands (including server-launching `vis`) and unknown flags
			// instead of guessing how a future CLI release might parse them.
			return false
		}
	}
	return promptMode
}

func kimiProxyReloadCapableMode(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			continue
		}
		switch arg {
		case "-S", "--session", "-m", "--model", "-p", "--prompt", "--output-format",
			"--skills-dir", "--agent", "--agent-file", "--add-dir":
			if i+1 < len(args) {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		switch arg {
		case "login", "provider", "acp", "web", "server", "migrate", "upgrade", "update":
			return arg
		default:
			return ""
		}
	}
	return ""
}

func (r srRunner) launchQwenProxy(ctx context.Context, args []string) error {
	options, vendorArgs, err := parseNativeProxyLaunchArgs(args)
	if err != nil {
		return err
	}
	if qwenProxyReloadCapableMode(vendorArgs) {
		return errors.New("Qwen serve/ACP/review/channel-service modes can reload saved credentials and proxies; use plain 'qwen' for those modes")
	}
	if qwenProxyPersistentSessionRequested(vendorArgs) {
		return errors.New("Qwen resume/continue can restore a saved direct provider route and cannot be used with 'sr qwen'; start a new routed session or use plain 'qwen' for the existing direct session")
	}
	model, err := qwenProxyModel(vendorArgs)
	if err != nil {
		return err
	}
	if strings.Contains(model, ":") {
		return errors.New("provider-qualified Qwen models can bypass the routed Token Plan provider; use an unqualified Token Plan model ID")
	}
	if qwenProxyBundledShortOptionRequested(vendorArgs, "s") {
		return errors.New("-s controls Qwen routing and cannot be used with 'sr qwen'")
	}
	for i := 0; i < len(vendorArgs); i++ {
		if vendorArgs[i] == "--" {
			break
		}
		for _, option := range []string{"-s", "--sandbox", "--auth-type", "--openai-api-key", "--openai-base-url", "--proxy", "--fallback-model"} {
			if vendorArgs[i] == option || strings.HasPrefix(vendorArgs[i], option+"=") {
				return fmt.Errorf("%s controls Qwen routing and cannot be used with 'sr qwen'", option)
			}
		}
	}
	if nativeProxyResumePickerRequested(qwenNativeProxy, vendorArgs) {
		return errors.New("'sr qwen --resume' requires an explicit session ID so Subrouter can preserve sticky account routing")
	}
	return r.launchNativeProxy(ctx, qwenNativeProxy, vendorArgs, options)
}

func qwenProxyPersistentSessionRequested(args []string) bool {
	if qwenProxyBundledShortOptionRequested(args, "cr") {
		return true
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		if arg == "-c" || arg == "--continue" || strings.HasPrefix(arg, "--continue=") ||
			arg == "-r" || arg == "--resume" || strings.HasPrefix(arg, "-r=") || strings.HasPrefix(arg, "--resume=") {
			return true
		}
		switch arg {
		case "-m", "--model", "--fallback-model", "-p", "--prompt", "-i", "--prompt-interactive",
			"-o", "--output-format", "--auth-type", "--openai-api-key", "--openai-base-url", "--proxy":
			if qwenOptionConsumesNext(args, i) {
				i++
			}
		}
	}
	return false
}

// Qwen 0.22.3 uses yargs short-option groups, so tokens such as -cy and
// -sc activate --continue/--sandbox even though neither token equals the
// individual alias. Scan only pre-delimiter option tokens and preserve the
// value-skipping behavior used by the launcher argument scanners.
func qwenProxyBundledShortOptionRequested(args []string, restricted string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		if qwenBundledShortOptionContains(arg, restricted) {
			return true
		}
		switch arg {
		case "-m", "--model", "--fallback-model", "-p", "--prompt", "-i", "--prompt-interactive",
			"-o", "--output-format", "--auth-type", "--openai-api-key", "--openai-base-url", "--proxy":
			if qwenOptionConsumesNext(args, i) {
				i++
			}
		}
	}
	return false
}

func qwenBundledShortOptionContains(arg, restricted string) bool {
	if len(arg) <= 2 || arg[0] != '-' || arg[1] == '-' {
		return false
	}
	bundle := arg[1:]
	if separator := strings.IndexByte(bundle, '='); separator >= 0 {
		bundle = bundle[:separator]
	}
	if len(bundle) <= 1 {
		return false
	}
	// Qwen 0.22.3's yargs parser expands every byte in an attached short token:
	// -mc activates -c and -ms activates -s. Equals and separate-value forms
	// are handled before this point and must not be scanned as bundles.
	return strings.ContainsAny(bundle, restricted)
}

func qwenOptionConsumesNext(args []string, index int) bool {
	return index+1 < len(args) && args[index+1] != "--" && !strings.HasPrefix(args[index+1], "-")
}

func qwenProxyReloadCapableMode(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if arg == "--acp" || strings.HasPrefix(arg, "--acp=") ||
			arg == "--experimental-acp" || strings.HasPrefix(arg, "--experimental-acp=") {
			return true
		}
		// Scan every pre-delimiter token: a valued global option unknown to
		// this launcher can otherwise hide a later serve/ACP mode. Consume
		// only known free-form prompt, model, and session values so a literal
		// value named "serve" remains ordinary; other ambiguities fail closed.
		switch arg {
		case "-m", "--model", "--fallback-model",
			"-p", "--prompt", "-i", "--prompt-interactive", "--system-prompt", "--append-system-prompt",
			"-r", "--resume", "--channel":
			if qwenOptionConsumesNext(args, i) {
				i++
			}
			continue
		}
		if arg == "serve" {
			return true
		}
		if arg == "review" {
			return true
		}
		if arg == "channel" && qwenProxyChannelServiceMode(args[i+1:]) {
			return true
		}
	}
	return false
}

func qwenProxyChannelServiceMode(args []string) bool {
	// Qwen 0.22.3's start/daemon-worker commands host model-backed channel
	// sessions; set and reload can create or replace the same worker in a daemon.
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		switch arg {
		case "--telemetry-target", "--telemetry-otlp-endpoint", "--telemetry-otlp-protocol",
			"--telemetry-outfile", "--proxy", "--daemon-url", "--token", "--timeout", "--channel":
			if i+1 < len(args) {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		switch arg {
		case "start", "daemon-worker", "reload", "set":
			return true
		case "stop", "status", "pairing", "configure-weixin":
			return false
		}
	}
	return false
}

// parseNativeProxyLaunchArgs reserves --account only at the beginning of the
// sr wrapper's arguments. The first vendor argument ends wrapper parsing, and
// an explicit -- delimiter makes every following value vendor-owned.
func parseNativeProxyLaunchArgs(args []string) (nativeProxyLaunchOptions, []string, error) {
	var options nativeProxyLaunchOptions
	if len(args) == 0 {
		return options, nil, nil
	}
	if args[0] == "--" {
		return options, args[1:], nil
	}
	if strings.HasPrefix(args[0], "--account=") {
		options.accountSelector = strings.TrimSpace(strings.TrimPrefix(args[0], "--account="))
		if options.accountSelector == "" {
			return options, nil, errors.New("--account= requires a non-empty account selector")
		}
		args = args[1:]
	} else if args[0] == "--account" {
		if len(args) == 1 {
			options.pickPinnedAccount = true
			return options, nil, nil
		}
		if args[1] == "--" {
			options.pickPinnedAccount = true
			return options, args[2:], nil
		}
		if strings.HasPrefix(args[1], "-") {
			return options, nil, errors.New("--account requires an account selector; use '--account --' to open the picker and pass vendor arguments")
		}
		options.accountSelector = strings.TrimSpace(args[1])
		if options.accountSelector == "" {
			return options, nil, errors.New("--account requires a non-empty account selector")
		}
		args = args[2:]
	}
	if len(args) > 0 && (args[0] == "--account" || strings.HasPrefix(args[0], "--account=")) {
		return options, nil, errors.New("--account may be specified only once; use '--' before a vendor-owned --account option")
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	return options, args, nil
}

func nativeProxyResumePickerRequested(spec nativeProxySpec, args []string) bool {
	var pickerFlags []string
	switch spec.provider {
	case accounts.ProviderKimi:
		pickerFlags = []string{"-S", "--session", "-r", "--resume"}
	case accounts.ProviderQwenToken:
		pickerFlags = []string{"-r", "--resume"}
	default:
		return false
	}
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			return false
		}
		if spec.provider == accounts.ProviderKimi {
			switch args[i] {
			case "-m", "--model", "-p", "--prompt", "--output-format",
				"--skills-dir", "--agent", "--agent-file", "--add-dir":
				// Commander consumes required values even when they begin with '-'.
				// Do not reinterpret a prompt, agent name, or other required value
				// such as "--resume" as Kimi's optional resume picker.
				if i+1 < len(args) {
					i++
				}
				continue
			}
		}
		matched := false
		for _, flag := range pickerFlags {
			if args[i] == flag {
				matched = true
			}
			if strings.HasPrefix(args[i], flag+"=") && strings.TrimSpace(strings.TrimPrefix(args[i], flag+"=")) == "" {
				return true
			}
		}
		if !matched {
			continue
		}
		if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "-") {
			return true
		}
		// Keep scanning after an explicit optional value so a later bare picker
		// cannot hide behind an earlier valid session ID.
		i++
	}
	return false
}

func kimiExplicitSessionID(args []string) (string, bool) {
	var sessionID string
	found := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		switch arg {
		case "-m", "--model", "-p", "--prompt", "--output-format",
			"--skills-dir", "--agent", "--agent-file", "--add-dir":
			// These Kimi options require a value even when it begins with '-'.
			if i+1 < len(args) {
				i++
			}
			continue
		case "-S", "--session", "-r", "--resume":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && strings.TrimSpace(args[i+1]) != "" {
				sessionID = args[i+1]
				found = true
				i++
			}
			continue
		}
		for _, option := range []string{"--session=", "--resume="} {
			if strings.HasPrefix(arg, option) {
				value := strings.TrimPrefix(arg, option)
				if strings.TrimSpace(value) != "" {
					sessionID = value
					found = true
				}
				break
			}
		}
		for _, option := range []string{"-S", "-r"} {
			if strings.HasPrefix(arg, option) && len(arg) > len(option) {
				value := strings.TrimPrefix(strings.TrimPrefix(arg, option), "=")
				if strings.TrimSpace(value) != "" {
					sessionID = value
					found = true
				}
				break
			}
		}
	}
	return sessionID, found
}

func (r srRunner) launchNativeProxy(ctx context.Context, spec nativeProxySpec, args []string, options nativeProxyLaunchOptions) error {
	server, remote, err := r.nativeProxyServer(ctx)
	if err != nil {
		return err
	}
	servingStore := r.store
	if !remote {
		servingStore, err = localServingStore(r.store)
		if err != nil {
			return fmt.Errorf("resolve local %s serving store: %w", spec.display, err)
		}
		authority, authorityErr := r.localServingStoreAuthorityForStore(ctx, server, servingStore)
		if authorityErr != nil {
			return fmt.Errorf("verify local %s proxy authority: %w", spec.display, authorityErr)
		}
		if !authority.storeMatches {
			return fmt.Errorf("local proxy account store does not match this CLI; no %s account inventory or proxy credential was sent", spec.display)
		}
	}
	root, err := secureNativeProxyRoot(ctx, server)
	if err != nil {
		return fmt.Errorf("secure %s proxy transport: %w", spec.display, err)
	}
	cloudConfig, err := cloudModeConfig()
	if err != nil {
		return fmt.Errorf("load credential storage: %w", err)
	}
	credentialSource := cloudConfig.EffectiveCredentialSource()
	if credentialSource == broker.CredentialSourceTeam && !nativeProxyBrokerLeaseSupported(spec) {
		return fmt.Errorf("team credential storage cannot lease %s accounts; use local or legacy storage for 'sr %s'", spec.display, spec.command)
	}
	var inventory []remoteServerAccount
	if nativeProxyNeedsAccountInventory(options) {
		if credentialSource == broker.CredentialSourceTeam {
			inventory, err = nativeProxyTeamAccounts(ctx, cloudConfig, spec)
		} else {
			inventory, err = r.nativeProxyAccounts(ctx, server, spec)
		}
		if err != nil {
			return err
		}
	}
	forcedAccountID := ""
	if options.pickPinnedAccount {
		var chosen bool
		forcedAccountID, chosen, err = r.pickNativeProxyAccount(spec, inventory)
		if err != nil {
			return err
		}
		if !chosen {
			return nil
		}
	} else if strings.TrimSpace(options.accountSelector) != "" {
		forcedAccountID, err = resolveNativeProxyAccountSelector(spec, inventory, options.accountSelector)
		if err != nil {
			return fmt.Errorf("server %s: %w", server.Name, err)
		}
	}
	sessionID, err := nativeProxySessionID(spec, args)
	if err != nil {
		return err
	}
	if forcedAccountID != "" {
		sessionID = nativeProxyPinnedSessionID(sessionID, forcedAccountID)
	}
	var relayTransport *http.Transport
	if remote {
		relayTransport, err = nativeProxyRelayTransport(root)
	} else {
		// Build the connection-attesting transport before the durable local
		// data-plane token is loaded. Its DialContext proves every connection
		// before a credential-bearing request can leave this process.
		relayTransport, err = localServingRelayTransport(root, r.store)
	}
	if err != nil {
		return fmt.Errorf("secure %s proxy relay transport: %w", spec.display, err)
	}
	proxyToken, err := nativeProxyServerToken(server, remote)
	if err != nil {
		return err
	}
	if err := r.requireNativeProxyDataPlaneWithClient(ctx, root, proxyToken, &http.Client{Timeout: 15 * time.Second, Transport: relayTransport}); err != nil {
		return err
	}
	relay, err := startProxyRelay(root, spec.route, spec.agent, sessionID, proxyToken, forcedAccountID, "", "", "", relayTransport)
	if err != nil {
		return fmt.Errorf("start local %s proxy relay: %w", spec.display, err)
	}
	defer relay.Close()

	commandPath, err := exec.LookPath(spec.command)
	if err != nil {
		return fmt.Errorf("%s CLI %q was not found in PATH", spec.display, spec.command)
	}
	launchArgs := args
	if spec.provider == accounts.ProviderKimi {
		launchArgs = kimiNativeProxyArgs(args)
	} else if spec.provider == accounts.ProviderQwenToken {
		model, modelErr := qwenProxyModel(args)
		if modelErr != nil {
			return modelErr
		}
		launchArgs = qwenNativeProxyArgs(args, model)
	}
	env, cleanup, err := nativeProxyEnvironment(spec, relay.URL(), os.Environ(), args)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, commandPath, launchArgs...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	cmd.Env = env
	runErr := cmd.Run()
	return joinNativeProxyRunAndCleanupErrors(spec.display, runErr, cleanup)
}

func joinNativeProxyRunAndCleanupErrors(display string, runErr error, cleanup func() error) error {
	cleanupErr := cleanup()
	if cleanupErr != nil {
		cleanupErr = fmt.Errorf("clean up temporary %s proxy home: %w", display, cleanupErr)
	}
	return errors.Join(runErr, cleanupErr)
}

func nativeProxyNeedsAccountInventory(options nativeProxyLaunchOptions) bool {
	// A hard process-local pin must resolve one authoritative account ID before
	// launching. Pooled mode leaves account selection to the server on the first
	// routed request and therefore needs no admin inventory credential.
	return options.pickPinnedAccount || strings.TrimSpace(options.accountSelector) != ""
}

func nativeProxyBrokerLeaseSupported(spec nativeProxySpec) bool {
	return spec.provider == accounts.ProviderQwenToken && spec.authMode == accounts.AuthModeAPIKey
}

func nativeProxyServerToken(server srServerConfig, remote bool) (string, error) {
	if remote {
		if token := strings.TrimSpace(server.TenantKey); token != "" {
			return token, nil
		}
		return "subrouter", nil
	}
	root := server.URL
	if !sameLocalProxyEndpoint(root, localBaseURL()) {
		return "subrouter", nil
	}
	config, err := cloudModeConfig()
	if err != nil {
		return "", fmt.Errorf("load local Subrouter client credential: %w", err)
	}
	if token := cloudClientProxyToken(config, localBaseURL()); token != "" {
		return token, nil
	}
	return "subrouter", nil
}

func sameLocalProxyEndpoint(left, right string) bool {
	leftURL, leftErr := url.Parse(strings.TrimSpace(left))
	rightURL, rightErr := url.Parse(strings.TrimSpace(right))
	if leftErr != nil || rightErr != nil || !loopbackEndpoint(left) || !loopbackEndpoint(right) ||
		!strings.EqualFold(leftURL.Scheme, rightURL.Scheme) ||
		(!strings.EqualFold(leftURL.Scheme, "http") && !strings.EqualFold(leftURL.Scheme, "https")) {
		return false
	}
	if sameEndpoint(left, right) {
		return true
	}
	// Different loopback names are equivalent only for the canonical local
	// listener aliases. The rest of 127/8 can be bound by another process, so
	// matching only the port must never disclose the daemon credential.
	canonicalLocalHost := func(parsed *url.URL) bool {
		host := strings.ToLower(parsed.Hostname())
		if host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
	}
	if !canonicalLocalHost(leftURL) || !canonicalLocalHost(rightURL) {
		return false
	}
	port := func(parsed *url.URL) string {
		if value := parsed.Port(); value != "" {
			return value
		}
		if strings.EqualFold(parsed.Scheme, "https") {
			return "443"
		}
		return "80"
	}
	return port(leftURL) == port(rightURL)
}

func (r srRunner) requireNativeProxyDataPlane(ctx context.Context, root, proxyToken string) error {
	client := r.client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return r.requireNativeProxyDataPlaneWithClient(ctx, root, proxyToken, client)
}

func (r srRunner) requireNativeProxyDataPlaneWithClient(ctx context.Context, root, proxyToken string, client *http.Client) error {
	probeURL := strings.TrimRight(root, "/") + "/"
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, probeURL, nil)
	if err != nil {
		return errors.New("build native proxy data-plane preflight")
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(proxyToken))
	secured, err := securedServerRequestClient(client, root)
	if err != nil {
		return errors.New("selected router data-plane transport is not safe for a native proxy launcher; no vendor CLI was started")
	}
	response, err := secured.Do(request)
	if err != nil {
		return errors.New("selected router data-plane preflight failed; no vendor CLI was started")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode == http.StatusNoContent {
		return nil
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return errors.New("selected router requires session-lease or data-plane authentication that native proxy launchers do not support; no vendor CLI was started")
	}
	// A provider-specific server may intentionally return 503 from its generic
	// root when another provider has no accounts (for example an AGY-only
	// shadow). Use the authenticated health endpoint as the readiness probe in
	// that case; never treat the 503 itself as ready.
	healthURL := strings.TrimRight(root, "/") + "/_subrouter/health"
	healthRequest, healthErr := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if healthErr == nil {
		healthRequest.Header.Set("Authorization", "Bearer "+strings.TrimSpace(proxyToken))
		if healthResponse, doErr := secured.Do(healthRequest); doErr == nil {
			defer healthResponse.Body.Close()
			_, _ = io.Copy(io.Discard, io.LimitReader(healthResponse.Body, 4<<10))
			if healthResponse.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
	return fmt.Errorf("selected router data-plane preflight returned HTTP %d; no vendor CLI was started", response.StatusCode)
}

func (r srRunner) nativeProxyServer(ctx context.Context) (srServerConfig, bool, error) {
	config, err := cloudModeConfig()
	if err != nil {
		return srServerConfig{}, false, fmt.Errorf("load credential storage: %w", err)
	}
	source := config.EffectiveCredentialSource()
	explicitTarget := strings.TrimSpace(os.Getenv("SUBROUTER_SERVER"))
	if explicitTarget == "" {
		explicitTarget = strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_SERVER"))
	}
	explicitServer := explicitTarget != ""
	explicitLocal := explicitServer && isLocalServerName(explicitTarget)
	if explicitServer {
		server, remote, resolveErr := r.selectedRemoteServer()
		if resolveErr != nil {
			return srServerConfig{}, false, resolveErr
		}
		if remote {
			if source == broker.CredentialSourceTeam {
				return srServerConfig{}, false, errors.New("team credentials may only use the local Subrouter daemon; select local or change credential storage")
			}
			return server, true, nil
		}
	}
	if !explicitLocal {
		switch source {
		case broker.CredentialSourceHosted:
			if !config.HostedReady() {
				return srServerConfig{}, false, errors.New("hosted credential storage is incomplete; run 'sr login'")
			}
			return srServerConfig{
				Name: "cmux", URL: strings.TrimRight(config.HostedURL, "/"), TenantKey: config.TenantKey,
			}, true, nil
		case broker.CredentialSourceLegacy:
			server, remote, resolveErr := r.selectedRemoteServer()
			if resolveErr != nil {
				return srServerConfig{}, false, resolveErr
			}
			if remote {
				return server, true, nil
			}
			// Match `sr status` and serving account commands: legacy mode with
			// no selected remote uses the healthy local daemon as its serving
			// authority. This remains an HTTP control/data-plane boundary; the
			// launcher never treats its own default disk store as authoritative.
			server, resolveErr = r.readyLocalServingServer(ctx, defaultDaemonStarter())
			if resolveErr != nil {
				return srServerConfig{}, false, resolveErr
			}
			return server, false, nil
		}
	}
	if !ensureLocalHealthy(ctx, fallbackHTTPClient(), localBaseURL(), defaultDaemonStarter(), r.errOut) {
		return srServerConfig{}, false, fmt.Errorf("local proxy is unavailable; run '%s doctor'", r.programOrSubrouter())
	}
	return srServerConfig{Name: "local", URL: localBaseURL()}, false, nil
}

func secureNativeProxyRoot(ctx context.Context, server srServerConfig) (string, error) {
	root := canonicalServerProxyRootURL(server)
	protected := server
	parsed, _ := url.Parse(root)
	tenantInURL := parsed != nil && tenantKeyFromURL(parsed) != ""
	if strings.TrimSpace(protected.TenantKey) == "" && !tenantInURL {
		// Prompts and responses are private even on a single-tenant server. Force
		// the same HTTPS/loopback/Tailscale validation used by the Claude launcher.
		protected.TenantKey = "protected-native-proxy"
	}
	return secureTenantServerURL(ctx, root, protected)
}

func (r srRunner) nativeProxyAccounts(ctx context.Context, server srServerConfig, spec nativeProxySpec) ([]remoteServerAccount, error) {
	inventory, err := r.fetchServerAccounts(ctx, server)
	if err != nil {
		return nil, fmt.Errorf("load %s accounts from server %s: %w", spec.display, server.Name, err)
	}
	eligible := make([]remoteServerAccount, 0, len(inventory))
	for _, account := range inventory {
		if nativeProxyAccountEligible(spec, account) && validNativeProxyAccountID(strings.TrimSpace(account.ID)) {
			eligible = append(eligible, account)
		}
	}
	if len(eligible) > 0 {
		return eligible, nil
	}
	mode := ""
	if spec.authMode != "" {
		mode = " " + string(spec.authMode)
	}
	return nil, fmt.Errorf("no routed %s%s account is available on server %s", spec.display, mode, server.Name)
}

func nativeProxyTeamAccounts(ctx context.Context, config broker.Config, spec nativeProxySpec) ([]remoteServerAccount, error) {
	shared, err := broker.NewClient(config).ListAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("load %s accounts from the selected team: %w", spec.display, err)
	}
	eligible := make([]remoteServerAccount, 0, len(shared))
	for _, item := range shared {
		if item.Kind != string(spec.provider) {
			continue
		}
		account := remoteServerAccount{
			ID:       strings.TrimSpace(item.ID),
			Provider: spec.provider,
			AuthMode: spec.authMode,
			Label:    strings.TrimSpace(item.Label),
			Email:    strings.TrimSpace(item.Email),
			Source:   "team vault",
		}
		if nativeProxyAccountEligible(spec, account) && validNativeProxyAccountID(account.ID) {
			eligible = append(eligible, account)
		}
	}
	if len(eligible) > 0 {
		return eligible, nil
	}
	mode := ""
	if spec.authMode != "" {
		mode = " " + string(spec.authMode)
	}
	return nil, fmt.Errorf("no routed %s%s account is available in the selected team", spec.display, mode)
}

func (r srRunner) requireNativeProxyAccount(ctx context.Context, server srServerConfig, spec nativeProxySpec) error {
	_, err := r.nativeProxyAccounts(ctx, server, spec)
	return err
}

func nativeProxyAccountEligible(spec nativeProxySpec, account remoteServerAccount) bool {
	if account.Provider != spec.provider {
		return false
	}
	if spec.authMode != "" && account.AuthMode != spec.authMode {
		return false
	}
	if spec.provider != accounts.ProviderKimi || account.AuthMode != accounts.AuthModeOAuth {
		return true
	}
	// The singleton credential owned by the plain Kimi CLI is deliberately a
	// direct bypass. Only Subrouter-managed subscription profiles (or Kimi API
	// keys, handled above) may enter the routed pool.
	id := strings.ToLower(strings.TrimSpace(account.ID))
	source := strings.ToLower(strings.TrimSpace(account.Source))
	return id != "kimi-code" && !strings.HasPrefix(id, "kimi-code:") && !strings.Contains(source, "kimi-code credentials file")
}

func validNativeProxyAccountID(accountID string) bool {
	return accountID != "" && len(accountID) <= 256 && !nativeProxyTerminalControl(accountID)
}

func nativeProxyTerminalControl(value string) bool {
	for _, char := range value {
		if unicode.IsControl(char) || unicode.In(char, unicode.Cf, unicode.Zl, unicode.Zp) {
			return true
		}
	}
	return false
}

func nativeProxyAccountSelectorValues(spec nativeProxySpec, account remoteServerAccount) []string {
	id := strings.TrimSpace(account.ID)
	values := []string{id, strings.TrimSpace(account.Label), strings.TrimSpace(account.Email)}
	if prefix := string(spec.provider) + ":"; strings.HasPrefix(strings.ToLower(id), strings.ToLower(prefix)) {
		values = append(values, strings.TrimSpace(id[len(prefix):]))
	}
	if spec.provider == accounts.ProviderKimi {
		const managedPrefix = "kimi-subscription:"
		if strings.HasPrefix(strings.ToLower(id), managedPrefix) {
			values = append(values, strings.TrimSpace(id[len(managedPrefix):]))
		}
	}
	return values
}

func resolveNativeProxyAccountSelector(spec nativeProxySpec, inventory []remoteServerAccount, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", fmt.Errorf("%s account selector cannot be empty", spec.display)
	}
	if nativeProxyTerminalControl(selector) {
		return "", fmt.Errorf("%s account selector contains a control character", spec.display)
	}
	type match struct {
		account remoteServerAccount
		rank    int
	}
	matches := make([]match, 0)
	lowerSelector := strings.ToLower(selector)
	for _, account := range inventory {
		values := nativeProxyAccountSelectorValues(spec, account)
		rank := 4
		if len(values) > 0 && strings.EqualFold(values[0], selector) {
			rank = 0 // A canonical server routing ID always wins.
		}
		for index, value := range values {
			if value == "" {
				continue
			}
			switch {
			case index >= 3 && strings.EqualFold(value, selector) && rank > 1:
				rank = 1 // Provider-prefix-stripped routing ID.
			case index > 0 && index < 3 && strings.EqualFold(value, selector) && rank > 2:
				rank = 2 // User-facing label or email.
			case strings.Contains(strings.ToLower(value), lowerSelector) && rank > 3:
				rank = 3
			}
		}
		if rank < 4 {
			matches = append(matches, match{account: account, rank: rank})
		}
	}
	bestRank := 4
	for _, candidate := range matches {
		if candidate.rank < bestRank {
			bestRank = candidate.rank
		}
	}
	if bestRank < 4 {
		filtered := matches[:0]
		for _, candidate := range matches {
			if candidate.rank == bestRank {
				filtered = append(filtered, candidate)
			}
		}
		matches = filtered
	}
	unique := make(map[string]remoteServerAccount, len(matches))
	for _, candidate := range matches {
		key := strings.ToLower(string(candidate.account.Provider)) + "\x00" +
			strings.ToLower(string(candidate.account.AuthMode)) + "\x00" +
			strings.ToLower(strings.TrimSpace(candidate.account.ID))
		unique[key] = candidate.account
	}
	if len(unique) == 0 {
		return "", fmt.Errorf("%s account %q was not found in the routed pool", spec.display, selector)
	}
	if len(unique) != 1 {
		return "", fmt.Errorf("%s account selector %q is ambiguous; use the exact account ID or label", spec.display, selector)
	}
	var account remoteServerAccount
	for _, candidate := range unique {
		account = candidate
	}
	if !nativeProxyAccountEligible(spec, account) {
		return "", fmt.Errorf("account %q is not an eligible routed %s account", selector, spec.display)
	}
	accountID := strings.TrimSpace(account.ID)
	if !validNativeProxyAccountID(accountID) {
		return "", fmt.Errorf("%s account %q has an invalid server routing ID", spec.display, selector)
	}
	return accountID, nil
}

func nativeProxyAccountDisplay(account remoteServerAccount) string {
	for _, value := range []string{account.Label, account.Email, account.ID} {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 256 || nativeProxyTerminalControl(value) {
			continue
		}
		return value
	}
	return "account"
}

func nativeProxyAccountPickerRow(account remoteServerAccount) string {
	id := strings.TrimSpace(account.ID)
	display := nativeProxyAccountDisplay(account)
	mode := strings.TrimSpace(string(account.AuthMode))
	if !strings.EqualFold(display, id) {
		return fmt.Sprintf("%s (%s; %s)", display, id, mode)
	}
	return fmt.Sprintf("%s (%s)", id, mode)
}

func (r srRunner) pickNativeProxyAccount(spec nativeProxySpec, inventory []remoteServerAccount) (string, bool, error) {
	sort.Slice(inventory, func(i, j int) bool {
		left := strings.ToLower(nativeProxyAccountPickerRow(inventory[i]))
		right := strings.ToLower(nativeProxyAccountPickerRow(inventory[j]))
		if left != right {
			return left < right
		}
		return strings.ToLower(inventory[i].ID) < strings.ToLower(inventory[j].ID)
	})
	fmt.Fprintf(r.out, "Choose one %s account for this PINNED process. No account failover will occur.\n", spec.display)
	for i, account := range inventory {
		fmt.Fprintf(r.out, "  %d) %s\n", i+1, nativeProxyAccountPickerRow(account))
	}
	answer, err := promptLine(r.out, bufio.NewReader(r.in), "Launch account (# or exact account): ")
	if err != nil {
		return "", false, err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return "", false, nil
	}
	if index, parseErr := strconv.Atoi(answer); parseErr == nil && index >= 1 && index <= len(inventory) {
		return inventory[index-1].ID, true, nil
	}
	accountID, err := resolveNativeProxyAccountSelector(spec, inventory, answer)
	if err != nil {
		return "", false, err
	}
	return accountID, true, nil
}

type nativeProxyRelay struct {
	listener   net.Listener
	server     *http.Server
	transport  *http.Transport
	baseURL    string
	credential string
}

func nativeProxyRelayTransport(targetRoot string) (*http.Transport, error) {
	client, err := securedServerRequestClient(&http.Client{Timeout: 15 * time.Second}, targetRoot)
	if err != nil {
		return nil, err
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == nil {
		return nil, errors.New("native proxy relay requires a direct HTTP transport")
	}
	return transport, nil
}

func localStoreAttestedRelayTransport(targetRoot string, store accounts.CodexStore) (*http.Transport, error) {
	return localStoreAttestedRelayTransportWithResolver(targetRoot, func() (accounts.CodexStore, error) {
		return store, nil
	})
}

func localStoreAttestedRelayTransportWithResolvers(
	targetRoot string,
	resolveBindingStore func() (accounts.CodexStore, error),
	resolveServingStore func() (accounts.CodexStore, error),
) (*http.Transport, error) {
	client, err := newLocalDataClientWithStoreResolvers(
		&http.Client{Timeout: 15 * time.Second}, targetRoot, resolveBindingStore, resolveServingStore,
	)
	if err != nil {
		return nil, err
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == nil {
		return nil, errors.New("local proxy relay requires a direct attested HTTP transport")
	}
	return transport, nil
}

func localStoreAttestedRelayTransportWithResolver(
	targetRoot string,
	resolveStore func() (accounts.CodexStore, error),
) (*http.Transport, error) {
	return localStoreAttestedRelayTransportWithResolvers(targetRoot, resolveStore, resolveStore)
}

func localServingStoreResolver(store accounts.CodexStore) func() (accounts.CodexStore, error) {
	return func() (accounts.CodexStore, error) { return localServingStore(store) }
}

func localServingRelayTransport(targetRoot string, bindingStore accounts.CodexStore) (*http.Transport, error) {
	binding, found, err := readLocalServingStoreBinding(bindingStore)
	if err != nil {
		return nil, err
	}
	private := strings.TrimSpace(os.Getenv("SUBROUTER_LOCAL_DATA_SOCKET")) != "" ||
		strings.TrimSpace(os.Getenv("SUBROUTER_STATE_DIR")) != "" ||
		(found && binding.Schema == localServingStoreSchema)
	if private {
		return localStoreAttestedRelayTransportWithResolvers(
			targetRoot,
			func() (accounts.CodexStore, error) { return bindingStore, nil },
			localServingStoreResolver(bindingStore),
		)
	}
	client, err := newLegacyLocalStoreAttestedClientWithResolver(
		&http.Client{Timeout: 15 * time.Second}, targetRoot, localServingStoreResolver(bindingStore),
	)
	if err != nil {
		return nil, err
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == nil {
		return nil, errors.New("legacy local proxy relay requires a direct attested HTTP transport")
	}
	return transport, nil
}

func startNativeProxyRelay(targetRoot string, spec nativeProxySpec, sessionID, proxyToken, forcedAccountID string) (*nativeProxyRelay, error) {
	transport, err := nativeProxyRelayTransport(targetRoot)
	if err != nil {
		return nil, fmt.Errorf("secure proxy target transport: %w", err)
	}
	return startProxyRelay(targetRoot, spec.route, spec.agent, sessionID, proxyToken, forcedAccountID, "", "", "", transport)
}

func startLocalStoreAttestedProxyRelay(
	targetRoot string,
	route string,
	agent string,
	sessionID string,
	proxyToken string,
	forcedAccountID string,
	preferredAccountID string,
	store accounts.CodexStore,
) (*nativeProxyRelay, error) {
	return startLocalStoreAttestedProxyRelayWithResolver(
		targetRoot, route, agent, sessionID, proxyToken, forcedAccountID,
		preferredAccountID, func() (accounts.CodexStore, error) { return store, nil },
	)
}

func startLocalStoreAttestedProxyRelayWithResolver(
	targetRoot string,
	route string,
	agent string,
	sessionID string,
	proxyToken string,
	forcedAccountID string,
	preferredAccountID string,
	resolveStore func() (accounts.CodexStore, error),
) (*nativeProxyRelay, error) {
	return startLocalStoreAttestedProxyRelayWithResolvers(
		targetRoot, route, agent, sessionID, proxyToken, forcedAccountID,
		preferredAccountID, resolveStore, resolveStore,
	)
}

func startLocalStoreAttestedProxyRelayWithResolvers(
	targetRoot string,
	route string,
	agent string,
	sessionID string,
	proxyToken string,
	forcedAccountID string,
	preferredAccountID string,
	resolveBindingStore func() (accounts.CodexStore, error),
	resolveServingStore func() (accounts.CodexStore, error),
) (*nativeProxyRelay, error) {
	transport, err := localStoreAttestedRelayTransportWithResolvers(targetRoot, resolveBindingStore, resolveServingStore)
	if err != nil {
		return nil, fmt.Errorf("secure local proxy target transport: %w", err)
	}
	return startProxyRelay(targetRoot, route, agent, sessionID, proxyToken, forcedAccountID, preferredAccountID, "", "", transport)
}

func startLocalServingProxyRelay(
	targetRoot string,
	route string,
	agent string,
	sessionID string,
	proxyToken string,
	forcedAccountID string,
	preferredAccountID string,
	bindingStore accounts.CodexStore,
) (*nativeProxyRelay, error) {
	transport, err := localServingRelayTransport(targetRoot, bindingStore)
	if err != nil {
		return nil, fmt.Errorf("secure local proxy target transport: %w", err)
	}
	return startProxyRelay(targetRoot, route, agent, sessionID, proxyToken, forcedAccountID, preferredAccountID, "", "", transport)
}

func startProxyRelay(
	targetRoot string,
	route string,
	agent string,
	sessionID string,
	proxyToken string,
	forcedAccountID string,
	preferredAccountID string,
	userEmail string,
	model string,
	transport *http.Transport,
) (*nativeProxyRelay, error) {
	target, err := url.Parse(strings.TrimRight(targetRoot, "/"))
	if err != nil || target.Scheme == "" || target.Host == "" || target.User != nil || target.Fragment != "" {
		return nil, errors.New("proxy target must be an absolute URL")
	}
	if transport == nil {
		return nil, errors.New("proxy relay requires a direct HTTP transport")
	}
	forcedAccountID = strings.TrimSpace(forcedAccountID)
	if forcedAccountID != "" && !validNativeProxyAccountID(forcedAccountID) {
		return nil, errors.New("pinned account has an invalid server routing ID")
	}
	preferredAccountID = strings.TrimSpace(preferredAccountID)
	if preferredAccountID != "" && !validNativeProxyAccountID(preferredAccountID) {
		return nil, errors.New("preferred account has an invalid server routing ID")
	}
	if forcedAccountID != "" && preferredAccountID != "" {
		return nil, errors.New("pinned and preferred accounts are mutually exclusive")
	}
	userEmail = strings.TrimSpace(userEmail)
	model = strings.TrimSpace(model)
	if nativeProxyTerminalControl(userEmail) || nativeProxyTerminalControl(model) {
		return nil, errors.New("proxy relay routing metadata is invalid")
	}
	route = strings.Trim(strings.TrimSpace(route), "/")
	if route == "" || strings.Contains(route, "..") || strings.ContainsAny(route, "?#\\") {
		return nil, errors.New("proxy relay route is invalid")
	}
	agent = strings.TrimSpace(agent)
	if agent == "" || nativeProxyTerminalControl(agent) {
		return nil, errors.New("proxy relay agent is invalid")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	sessionID = strings.TrimSpace(sessionID)
	relayToken, err := newNativeProxyToken()
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	proxyToken = strings.TrimSpace(proxyToken)
	if proxyToken == "" {
		proxyToken = "subrouter"
	}
	relayPrefix := "/" + relayToken
	providerPrefix := relayPrefix + "/" + route
	relayHost := listener.Addr().String()
	reverse := &httputil.ReverseProxy{Transport: transport}
	reverse.Rewrite = func(proxyRequest *httputil.ProxyRequest) {
		proxyRequest.SetURL(target)
		request := proxyRequest.Out
		for _, header := range []string{
			"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key", "X-Goog-Api-Key", "X-Auth-Token",
			"OpenAI-Organization", "OpenAI-Project",
			"X-Subrouter-Lease", "X-Subrouter-Session", "X-Subrouter-Agent",
			"X-Subrouter-User-Email", "X-Subrouter-User", "X-User-Email",
			"X-Subrouter-Account-ID", "X-Subrouter-Account", "X-Subrouter-Preferred-Account-ID",
			"X-Subrouter-Model", "X-Model", "X-Subrouter-Azure", "X-Subrouter-No-Retry",
		} {
			request.Header.Del(header)
		}
		request.Host = target.Host
		request.Header.Set("Authorization", "Bearer "+proxyToken)
		request.Header.Set("X-Subrouter-Agent", agent)
		if sessionID != "" {
			request.Header.Set("X-Subrouter-Session", sessionID)
		}
		if forcedAccountID != "" {
			request.Header.Set("X-Subrouter-Account-ID", forcedAccountID)
		} else if preferredAccountID != "" {
			request.Header.Set("X-Subrouter-Preferred-Account-ID", preferredAccountID)
		}
		if userEmail != "" {
			request.Header.Set("X-Subrouter-User-Email", userEmail)
		}
		if model != "" {
			request.Header.Set("X-Subrouter-Model", model)
		}
	}
	reverse.ErrorLog = log.New(io.Discard, "", 0)
	reverse.ErrorHandler = func(response http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(response, "Subrouter relay could not reach the selected server", http.StatusBadGateway)
	}
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		// The random path is a process-local capability: other local processes
		// cannot turn this short-lived relay into a tenant-wide proxy merely by
		// discovering its port. Restrict it to the one advertised provider too.
		// Qwen also receives this relay origin as a fail-closed HTTP proxy guard;
		// absolute-form requests for any other host must not inherit the
		// capability merely because their path happens to match.
		requestPath := request.URL.Path
		if request.Method == http.MethodConnect || request.URL.IsAbs() || request.Host != relayHost ||
			request.URL.RawPath != "" || pathpkg.Clean(requestPath) != requestPath ||
			(requestPath != providerPrefix && !strings.HasPrefix(requestPath, providerPrefix+"/")) {
			http.NotFound(response, request)
			return
		}
		request.URL.Path = strings.TrimPrefix(requestPath, relayPrefix)
		request.URL.RawPath = ""
		reverse.ServeHTTP(response, request)
	})
	relay := &nativeProxyRelay{
		listener:   listener,
		server:     &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second},
		transport:  transport,
		baseURL:    "http://" + listener.Addr().String() + relayPrefix,
		credential: relayToken,
	}
	go func() { _ = relay.server.Serve(listener) }()
	return relay, nil
}

func (r *nativeProxyRelay) Credential() string {
	if r == nil {
		return ""
	}
	return r.credential
}

func (r *nativeProxyRelay) URL() string {
	if r == nil {
		return ""
	}
	return r.baseURL
}

func (r *nativeProxyRelay) Close() {
	if r == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.server.Shutdown(ctx)
	_ = r.listener.Close()
	if r.transport != nil {
		r.transport.CloseIdleConnections()
	}
}

func newNativeProxyToken() (string, error) {
	var body [32]byte
	if _, err := rand.Read(body[:]); err != nil {
		return "", fmt.Errorf("create local proxy capability: %w", err)
	}
	return hex.EncodeToString(body[:]), nil
}

func nativeProxySessionID(spec nativeProxySpec, args []string) (string, error) {
	if spec.provider == accounts.ProviderKimi {
		if sessionID, ok := kimiExplicitSessionID(args); ok {
			digest := sha256.Sum256([]byte(string(spec.provider) + "\x00session\x00" + sessionID))
			return "sr-native-" + hex.EncodeToString(digest[:16]), nil
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve native proxy workspace: %w", err)
	}
	cwd = filepath.Clean(cwd)
	if resolved, resolveErr := filepath.EvalSymlinks(cwd); resolveErr == nil {
		cwd = resolved
	}
	digest := sha256.Sum256([]byte(string(spec.provider) + "\x00workspace\x00" + cwd))
	return "sr-native-" + hex.EncodeToString(digest[:16]), nil
}

func nativeProxyPinnedSessionID(pooledSessionID, accountID string) string {
	digest := sha256.Sum256([]byte(pooledSessionID + "\x00pinned-account\x00" + accountID))
	return "sr-native-" + hex.EncodeToString(digest[:16])
}

var nativeProxyRoutingEnvKeys = []string{
	"CLOUD_CODE_URL",
	"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GEMINI_BASE_URL", "AGY_ADC_AUTH",
	"KIMI_CODE_HOME", "KIMI_CODE_NO_AUTO_UPDATE", "KIMI_CODE_LEGACY_FLAG",
	"KIMI_CODE_EXPERIMENTAL_SECONDARY_MODEL",
	"KIMI_CODE_OAUTH_HOST", "KIMI_OAUTH_HOST", "KIMI_CODE_PASSWORD", "KIMI_REGISTRY_API_KEY",
	"KIMI_CODE_BASE_URL", "KIMI_CODE_CUSTOM_HEADERS", "KIMI_API_KEY", "KIMI_BASE_URL",
	"KIMI_MODEL_NAME", "KIMI_MODEL_API_KEY", "KIMI_MODEL_BASE_URL", "KIMI_MODEL_PROVIDER_TYPE",
	"KIMI_MODEL_ADAPTIVE_THINKING", "KIMI_MODEL_CAPABILITIES", "KIMI_MODEL_DISPLAY_NAME",
	"KIMI_MODEL_MAX_COMPLETION_TOKENS", "KIMI_MODEL_MAX_CONTEXT_SIZE", "KIMI_MODEL_MAX_OUTPUT_SIZE",
	"KIMI_MODEL_MAX_TOKENS", "KIMI_MODEL_OUTPUT_FORMAT", "KIMI_MODEL_REASONING_KEY",
	"KIMI_MODEL_TEMPERATURE", "KIMI_MODEL_THINKING_EFFORT", "KIMI_MODEL_THINKING_KEEP", "KIMI_MODEL_TOP_P",
	"KIMI_SECONDARY_MODEL", "KIMI_SECONDARY_EFFORT",
	"KIMI_WEB_SEARCH_BASE_URL", "KIMI_WEB_SEARCH_API_KEY",
	"KIMI_WEB_FETCH_BASE_URL", "KIMI_WEB_FETCH_API_KEY",
	"QWEN_OAUTH", "QWEN_MODEL", "QWEN_SANDBOX", "QWEN_CODE_SIMPLE", "QWEN_CODE_RELAUNCH_ARGS", "QWEN_DISABLED_SLASH_COMMANDS",
	"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_MODEL",
	"OPENAI_ORG_ID", "OPENAI_PROJECT_ID",
	"BAILIAN_CODING_PLAN_API_KEY", "BAILIAN_TOKEN_PLAN_API_KEY", "DASHSCOPE_API_KEY",
}

func nativeProxyEnvironment(spec nativeProxySpec, relayRoot string, environ, args []string) ([]string, func() error, error) {
	env := envWithout(envWithoutSubrouterControl(environ), nativeProxyRoutingEnvKeys)
	env = directPlainHTTPEnvironment(env, relayRoot)
	providerURL := strings.TrimRight(relayRoot, "/") + "/" + spec.route
	switch spec.provider {
	case accounts.ProviderAntigravity:
		// Current AGY releases honor CLOUD_CODE_URL and append their
		// /v1internal:* Cloud Code paths to it. The relay injects the server
		// credential and routing metadata, so the local AGY OAuth credential is
		// used only to satisfy the CLI startup check and never leaves this
		// process. Do not set GOOGLE_GEMINI_BASE_URL here: that selects the
		// separate Gemini API-key mode and bypasses AGY subscription quotas.
		env = upsertEnv(env, "CLOUD_CODE_URL", providerURL)
		return env, func() error { return nil }, nil
	case accounts.ProviderKimi:
		overlay, cleanup, err := prepareKimiProxyHome(environ)
		if err != nil {
			return nil, func() error { return nil }, err
		}
		for key, value := range map[string]string{
			"KIMI_CODE_HOME":                         overlay.home,
			"KIMI_CODE_NO_AUTO_UPDATE":               "1",
			"KIMI_CODE_EXPERIMENTAL_SECONDARY_MODEL": "1",
			"KIMI_MODEL_NAME":                        "kimi-for-coding",
			"KIMI_MODEL_API_KEY":                     "subrouter",
			"KIMI_MODEL_BASE_URL":                    providerURL + "/v1",
			"KIMI_MODEL_PROVIDER_TYPE":               "kimi",
			"KIMI_MODEL_MAX_CONTEXT_SIZE":            "262144",
			"KIMI_SECONDARY_MODEL":                   "__kimi_env_model__",
			"KIMI_WEB_SEARCH_BASE_URL":               providerURL + "/v1/search",
			"KIMI_WEB_SEARCH_API_KEY":                "subrouter",
			"KIMI_WEB_FETCH_BASE_URL":                providerURL + "/v1/fetch",
			"KIMI_WEB_FETCH_API_KEY":                 "subrouter",
		} {
			env = upsertEnv(env, key, value)
		}
		return env, cleanup, nil
	case accounts.ProviderQwenToken:
		model, err := qwenProxyModel(args)
		if err != nil {
			return nil, func() error { return nil }, err
		}
		overlay, cleanup, err := prepareQwenProxyOverlay(providerURL+"/v1", model, environ)
		if err != nil {
			return nil, func() error { return nil }, err
		}
		proxyGuard, err := nativeProxyLoopbackGuardURL(relayRoot)
		if err != nil {
			return nil, func() error { return nil }, errors.Join(err, cleanup())
		}
		for key, value := range map[string]string{
			"QWEN_CODE_SYSTEM_SETTINGS_PATH": overlay.settings,
			"QWEN_CODE_SYSTEM_DEFAULTS_PATH": overlay.defaults,
			"QWEN_CODE_SIMPLE":               "1",
			"QWEN_DISABLED_SLASH_COMMANDS":   "auth,model",
			"OPENAI_API_KEY":                 "subrouter",
			"OPENAI_BASE_URL":                providerURL + "/v1",
			"OPENAI_MODEL":                   model,
			// Qwen loads .qwen/.env and settings.env only when a process key is
			// absent. Non-empty sentinels prevent either source from restoring a
			// direct Alibaba credential. The forced --auth-type=openai argument
			// and single-provider system overlay remain the routing authority.
			"BAILIAN_CODING_PLAN_API_KEY": "subrouter",
			"BAILIAN_TOKEN_PLAN_API_KEY":  "subrouter",
			"DASHSCOPE_API_KEY":           "subrouter",
			// Qwen treats empty values as unset when it loads .qwen/.env. A
			// non-secret loopback guard prevents a saved outbound proxy from being
			// restored; the relay rejects proxy targets other than itself.
			"HTTP_PROXY":  proxyGuard,
			"HTTPS_PROXY": proxyGuard,
			"ALL_PROXY":   proxyGuard,
			"http_proxy":  proxyGuard,
			"https_proxy": proxyGuard,
			"all_proxy":   proxyGuard,
			// Common HTTP stacks, including Qwen's EnvHttpProxyAgent, bypass the
			// guard for every destination. This preserves ordinary tool and child
			// process networking while the non-empty guard still blocks Qwen's
			// saved .env proxy values from being restored.
			"NO_PROXY": "*",
			"no_proxy": "*",
		} {
			env = upsertEnv(env, key, value)
		}
		return env, cleanup, nil
	default:
		return nil, func() error { return nil }, fmt.Errorf("unsupported native proxy provider %s", spec.provider)
	}
}

const kimiProxyConfig = `default_model = "__kimi_env_model__"

[secondary_model]
default_model = "__kimi_env_model__"
force = true
`

type kimiProxyOverlay struct {
	home string
}

func prepareKimiProxyHome(environ []string) (kimiProxyOverlay, func() error, error) {
	if err := kimiProxySessionLinksSupported(runtime.GOOS); err != nil {
		return kimiProxyOverlay{}, func() error { return nil }, err
	}
	sourceHome, err := kimiSourceHome(environ)
	if err != nil {
		return kimiProxyOverlay{}, func() error { return nil }, err
	}
	sessions, sessionIndex, err := ensureKimiSessionSource(sourceHome)
	if err != nil {
		return kimiProxyOverlay{}, func() error { return nil }, err
	}
	home, err := os.MkdirTemp("", "subrouter-kimi-proxy-")
	if err != nil {
		return kimiProxyOverlay{}, func() error { return nil }, fmt.Errorf("create temporary Kimi proxy home: %w", err)
	}
	cleanup, err := preparePrivateProxyHomeCleanup(home)
	if err != nil {
		setupErr := fmt.Errorf("prepare temporary Kimi proxy home cleanup: %w", err)
		return kimiProxyOverlay{}, func() error { return nil }, kimiProxySetupFailure(
			func() error { return removePrivateProxyHome(home) }, setupErr,
		)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return kimiProxyOverlay{}, func() error { return nil }, kimiProxySetupFailure(cleanup, fmt.Errorf("lock temporary Kimi proxy home: %w", err))
	}
	configPath := filepath.Join(home, "config.toml")
	if err := writeFileAtomic(configPath, []byte(kimiProxyConfig), 0o600); err != nil {
		return kimiProxyOverlay{}, func() error { return nil }, kimiProxySetupFailure(cleanup, fmt.Errorf("write temporary Kimi proxy config: %w", err))
	}
	if err := os.Chmod(configPath, 0o600); err != nil {
		return kimiProxyOverlay{}, func() error { return nil }, kimiProxySetupFailure(cleanup, fmt.Errorf("lock temporary Kimi proxy config: %w", err))
	}
	// Kimi initializes its logger and query cache before the first request.
	// Keep both child-local and writable. Kimi also needs its private root to
	// remain writable for the workspace catalog it atomically updates below.
	for _, name := range []string{"logs", "cache"} {
		if err := os.Mkdir(filepath.Join(home, name), 0o700); err != nil {
			return kimiProxyOverlay{}, func() error { return nil }, kimiProxySetupFailure(cleanup, fmt.Errorf("create temporary Kimi %s directory: %w", name, err))
		}
	}
	for name, target := range map[string]string{
		"sessions":            sessions,
		"session_index.jsonl": sessionIndex,
	} {
		if err := os.Symlink(target, filepath.Join(home, name)); err != nil {
			return kimiProxyOverlay{}, func() error { return nil }, kimiProxySetupFailure(cleanup, fmt.Errorf("link Kimi %s into routed home: %w", name, err))
		}
	}
	if err := os.Chmod(configPath, 0o400); err != nil {
		return kimiProxyOverlay{}, func() error { return nil }, kimiProxySetupFailure(cleanup, fmt.Errorf("restrict temporary Kimi proxy config: %w", err))
	}
	// Keep the private root writable only to this child/user. Kimi 0.39
	// atomically rewrites workspaces.json on every startup and cannot run in a
	// read-only home. Because the home starts without provider/OAuth state and is
	// removed with the child, any Kimi-managed in-session reconfiguration remains
	// ephemeral instead of being loaded from or written to the real Kimi config.
	return kimiProxyOverlay{home: home}, cleanup, nil
}

func kimiProxySetupFailure(cleanup func() error, setupErr error) error {
	if cleanupErr := cleanup(); cleanupErr != nil {
		return errors.Join(setupErr, fmt.Errorf("clean up incomplete temporary Kimi proxy home: %w", cleanupErr))
	}
	return setupErr
}

func kimiProxySessionLinksSupported(goos string) error {
	if goos == "windows" {
		return errors.New("sr kimi session isolation is not supported on Windows; use plain 'kimi' for the direct CLI")
	}
	return nil
}

func kimiSourceHome(environ []string) (string, error) {
	home := strings.TrimSpace(envValue(environ, "KIMI_CODE_HOME"))
	if home == "" {
		userHome := strings.TrimSpace(envValue(environ, "HOME"))
		if userHome == "" {
			var err error
			userHome, err = os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("locate Kimi session home: %w", err)
			}
		}
		home = filepath.Join(userHome, ".kimi-code")
	}
	absolute, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolve Kimi session home: %w", err)
	}
	absolute = filepath.Clean(absolute)
	if absolute == filepath.Dir(absolute) {
		return "", errors.New("Kimi session home cannot be a filesystem root")
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return "", fmt.Errorf("create Kimi session home: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("validate Kimi session home: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errors.New("Kimi session home is not a directory")
	}
	return resolved, nil
}

func ensureKimiSessionSource(home string) (string, string, error) {
	sessions := filepath.Join(home, "sessions")
	if err := ensureKimiSessionDirectory(sessions); err != nil {
		return "", "", err
	}
	index := filepath.Join(home, "session_index.jsonl")
	if err := ensureKimiSessionIndex(index); err != nil {
		return "", "", err
	}
	return sessions, index, nil
}

func ensureKimiSessionDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create Kimi sessions directory: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("validate Kimi sessions directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("Kimi sessions path must be a direct directory, not a link or other file")
	}
	return nil
}

func ensureKimiSessionIndex(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if createErr != nil && !errors.Is(createErr, os.ErrExist) {
			return fmt.Errorf("create Kimi session index: %w", createErr)
		}
		if createErr == nil {
			if closeErr := file.Close(); closeErr != nil {
				return fmt.Errorf("close Kimi session index: %w", closeErr)
			}
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("validate Kimi session index: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("Kimi session index must be a direct regular file, not a link or other file")
	}
	return nil
}

func kimiNativeProxyArgs(args []string) []string {
	out := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			out = append(out, "--model", "__kimi_env_model__")
			return append(out, args[i:]...)
		}
		switch {
		case args[i] == "-m" || args[i] == "--model":
			if i+1 < len(args) {
				i++
			}
			continue
		case strings.HasPrefix(args[i], "--model=") || strings.HasPrefix(args[i], "-m="):
			continue
		case args[i] == "-p" || args[i] == "--prompt" || args[i] == "--output-format" ||
			args[i] == "--skills-dir" || args[i] == "--agent" || args[i] == "--agent-file" || args[i] == "--add-dir":
			out = append(out, args[i])
			if i+1 < len(args) {
				i++
				out = append(out, args[i])
			}
		default:
			out = append(out, args[i])
		}
	}
	return append(out, "--model", "__kimi_env_model__")
}

const defaultQwenProxyModel = "qwen3.7-plus"

func qwenProxyModel(args []string) (string, error) {
	model := defaultQwenProxyModel
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break
		}
		switch {
		case args[i] == "-m" || args[i] == "--model":
			if !qwenOptionConsumesNext(args, i) || strings.TrimSpace(args[i+1]) == "" {
				return "", errors.New("Qwen -m/--model requires a non-option model value")
			}
			model = strings.TrimSpace(args[i+1])
			i++
		case strings.HasPrefix(args[i], "--model=") || strings.HasPrefix(args[i], "-m="):
			separator := strings.IndexByte(args[i], '=')
			candidate := strings.TrimSpace(args[i][separator+1:])
			if candidate == "" {
				return "", errors.New("Qwen -m/--model requires a non-empty model value")
			}
			model = candidate
		}
	}
	return model, nil
}

func qwenNativeProxyArgs(args []string, model string) []string {
	out := make([]string, 0, len(args)+7)
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			out = append(out,
				"--bare",
				"--auth-type", "openai",
				"--model", model,
				"--openai-api-key", "subrouter",
			)
			return append(out, args[i:]...)
		}
		switch {
		case args[i] == "--bare" || args[i] == "--no-bare" || strings.HasPrefix(args[i], "--bare="):
		case args[i] == "-m" || args[i] == "--model":
			if i+1 < len(args) {
				i++
			}
		case strings.HasPrefix(args[i], "--model=") || strings.HasPrefix(args[i], "-m="):
		case args[i] == "--fallback-model":
			if i+1 < len(args) {
				i++
			}
		case strings.HasPrefix(args[i], "--fallback-model="):
		default:
			out = append(out, args[i])
		}
	}
	return append(out,
		"--bare",
		"--auth-type", "openai",
		"--model", model,
		"--openai-api-key", "subrouter",
	)
}

type qwenProxyOverlay struct {
	settings string
	defaults string
}

func nativeProxyLoopbackGuardURL(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil ||
		!isLoopbackServerHost(parsed.Hostname()) {
		return "", errors.New("Qwen proxy relay must use an HTTP loopback URL")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func prepareQwenProxyOverlay(baseURL, model string, environ []string) (qwenProxyOverlay, func() error, error) {
	if conflict := qwenSystemPolicyConflict(environ, runtime.GOOS); conflict != "" {
		return qwenProxyOverlay{}, func() error { return nil }, fmt.Errorf("Qwen system policy %s is configured; refusing a proxy overlay that could bypass it", conflict)
	}
	proxyGuard, err := nativeProxyLoopbackGuardURL(baseURL)
	if err != nil {
		return qwenProxyOverlay{}, func() error { return nil }, err
	}
	dir, err := os.MkdirTemp("", "subrouter-qwen-proxy-")
	if err != nil {
		return qwenProxyOverlay{}, func() error { return nil }, fmt.Errorf("create temporary Qwen proxy overlay: %w", err)
	}
	cleanup, err := preparePrivateProxyHomeCleanup(dir)
	if err != nil {
		return qwenProxyOverlay{}, func() error { return nil }, errors.Join(
			fmt.Errorf("prepare temporary Qwen proxy cleanup: %w", err),
			removePrivateProxyHome(dir),
		)
	}
	routedModel := map[string]any{
		"id":      model,
		"name":    "Qwen Token Plan via Subrouter",
		"baseUrl": baseURL,
		"generationConfig": map[string]any{
			"customHeaders": map[string]string{"X-Subrouter-Agent": "qwen-token"},
		},
	}
	modelProviders := map[string]any{
		"openai":     []any{routedModel},
		"anthropic":  []any{},
		"gemini":     []any{},
		"vertex-ai":  []any{},
		"qwen-oauth": []any{},
	}
	payload := map[string]any{
		// A saved Qwen proxy would otherwise receive the capability-bearing
		// loopback URL. This truthy value wins the settings merge but Qwen's
		// normalizeProxyUrl trims it to disabled. The non-empty environment guards
		// below separately prevent .env from restoring an outbound proxy.
		"proxy": " ",
		"env": map[string]string{
			"BAILIAN_CODING_PLAN_API_KEY": "subrouter",
			"BAILIAN_TOKEN_PLAN_API_KEY":  "subrouter",
			"DASHSCOPE_API_KEY":           "subrouter",
			"HTTP_PROXY":                  proxyGuard,
			"HTTPS_PROXY":                 proxyGuard,
			"ALL_PROXY":                   proxyGuard,
			"http_proxy":                  proxyGuard,
			"https_proxy":                 proxyGuard,
			"all_proxy":                   proxyGuard,
			"NO_PROXY":                    "*",
			"no_proxy":                    "*",
		},
		"slashCommands": map[string]any{
			"disabled": []string{"auth", "model"},
		},
		// Qwen 0.22 deep-merges provider keys. Clear known built-ins as defense
		// in depth beneath the complete forced-bare boundary.
		"modelProviders": modelProviders,
		// Clear saved alternate roles as defense in depth; bare mode prevents the
		// persisted settings from loading at all.
		"fastModel":       "",
		"advisorModel":    "",
		"visionModel":     "",
		"compactionModel": "",
		"imageModel":      "",
		"voiceModel":      "",
		"modelFallbacks":  "",
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return qwenProxyOverlay{}, func() error { return nil }, errors.Join(fmt.Errorf("encode Qwen proxy overlay: %w", err), cleanup())
	}
	body = append(body, '\n')
	settings := filepath.Join(dir, "settings.json")
	defaults := filepath.Join(dir, "system-defaults.json")
	if err := writeFileAtomic(settings, body, 0o600); err != nil {
		return qwenProxyOverlay{}, func() error { return nil }, errors.Join(fmt.Errorf("write Qwen proxy overlay: %w", err), cleanup())
	}
	if err := writeFileAtomic(defaults, []byte("{}\n"), 0o600); err != nil {
		return qwenProxyOverlay{}, func() error { return nil }, errors.Join(fmt.Errorf("write Qwen proxy defaults: %w", err), cleanup())
	}
	return qwenProxyOverlay{settings: settings, defaults: defaults}, cleanup, nil
}

func qwenSystemPolicyConflict(environ []string, goos string) string {
	return qwenSystemPolicyConflictAtPathsForOS(environ, qwenDefaultSystemPolicyPaths(environ, goos), goos)
}

func qwenSystemPolicyConflictAtPaths(environ, paths []string) string {
	return qwenSystemPolicyConflictAtPathsForOS(environ, paths, runtime.GOOS)
}

func qwenSystemPolicyConflictAtPathsForOS(environ, paths []string, goos string) string {
	for _, key := range []string{"QWEN_CODE_SYSTEM_SETTINGS_PATH", "QWEN_CODE_SYSTEM_DEFAULTS_PATH"} {
		if strings.TrimSpace(envValueForOS(environ, key, goos)) != "" {
			return key
		}
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
			return path
		}
	}
	return ""
}

func qwenDefaultSystemPolicyPaths(environ []string, goos string) []string {
	switch goos {
	case "darwin":
		return []string{
			"/Library/Application Support/QwenCode/settings.json",
			"/Library/Application Support/QwenCode/system-defaults.json",
		}
	case "windows":
		root := envValueForOS(environ, "ProgramData", goos)
		if root == "" {
			root = `C:\ProgramData`
		}
		return []string{filepath.Join(root, "qwen-code", "settings.json"), filepath.Join(root, "qwen-code", "system-defaults.json")}
	default:
		return []string{"/etc/qwen-code/settings.json", "/etc/qwen-code/system-defaults.json"}
	}
}

func envValue(environ []string, key string) string {
	return envValueForOS(environ, key, runtime.GOOS)
}

func envValueForOS(environ []string, key, goos string) string {
	for i := len(environ) - 1; i >= 0; i-- {
		item := environ[i]
		separator := strings.IndexByte(item, '=')
		if separator < 0 {
			continue
		}
		name := item[:separator]
		if (goos == "windows" && strings.EqualFold(name, key)) ||
			(goos != "windows" && name == key) {
			return item[separator+1:]
		}
	}
	return ""
}

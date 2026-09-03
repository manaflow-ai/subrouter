package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/internal/tenant"
)

const srClaudeHelp = `sr claude - Manage local profiles and launch server-pooled Claude

Usage:
  sr claude                     Interactively launch pooled Claude (chosen account is a preference)
  sr claude add [name]          Add local profile from a 1-year Claude setup token
                                (runs 'claude setup-token', then asks you to paste the token)
    --token TOKEN|-             Use an already minted setup token (or read it from stdin)
    --oauth                     Use the classic browser OAuth login instead (same as 'sr claude login')
  sr claude login [name]        Add local profile with the classic browser OAuth login (refresh token, infers email)
  sr claude list                List local managed profiles with auth status and setup-token expiry
  sr claude switch [name]       Switch active local profile
  sr claude remove <name>       Remove a profile
  sr claude env                 Print CLAUDE_CONFIG_DIR for local/HTTPS profiles
  sr claude push [name]         Upload a profile to the default Subrouter server pool
  sr claude pick                Switch to the profile with the most quota left
  sr claude proxy [options] [args...]
                                Launch Claude profilelessly through the selected server pool
    sr claude proxy --resume ID Resume a direct or pooled session through the server pool
    --account [ACCOUNT]         Pin to one profile with no account failover; omit ACCOUNT for a picker
    --sr-expect-scope SCOPE --  Atomically bind launch to an opaque proxy scope (must be last option)
                                Wrapper options must precede Claude args; args at/after -- are literal
  sr claude proxy-scope         Print the opaque selected proxy session scope
  sr claude run [name] [...]    Launch one authenticated local managed profile safely
  sr claude --flag [...]        Launch Claude with the active profile
  sr claude <name> [...]        Shorthand for 'sr claude run <name>'
  sr claude help                Show this help
`

type claudeRunner struct {
	store  claude.Store
	in     io.Reader
	out    io.Writer
	errOut io.Writer
	client *http.Client
	// authStatus is a test seam for the read-only local-login preflight.
	// Production uses claude.AuthStatusForPath.
	authStatus func(context.Context, string, string) (*claude.AuthStatus, error)
	// pushToServer uploads a profile to the default Subrouter server, when
	// one is configured. nil when the claude runner is built without server
	// support (tests). pushAfterAdd is the same upload but no-ops silently
	// when no default server is configured.
	pushToServer func(ctx context.Context, name string) error
	pushAfterAdd func(ctx context.Context, name string) error
	pick         func(ctx context.Context) error
	// ephemeral is used by hosted account onboarding. OAuth runs in a
	// temporary store, the credential is uploaded, and no local profile or
	// trajectory directory survives the command.
	ephemeral bool
	// afterAuthVerified is a test seam for cancellation and publication-failure
	// races after Claude has durably written a credential.
	afterAuthVerified func()
	// mutateProfileInventoryForTest injects failures at the publication wrapper
	// boundary, including errors returned after mutate has committed.
	mutateProfileInventoryForTest func(context.Context, func() (bool, error)) error
	// verifyToken proves a pasted setup token against Anthropic before it is
	// stored. nil selects claude.VerifyAccessToken with the runner's client.
	verifyToken func(ctx context.Context, token string) error
	// now is the clock used to stamp a setup token's expiry. nil selects
	// time.Now.
	now func() time.Time
}

const claudeProfileReconcileTimeout = 10 * time.Second

func (r srRunner) claude(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "proxy-scope" {
		return r.printClaudeProxyScope()
	}
	if claudeLaunchesAgent(args) {
		if len(args) == 0 {
			selector, scope, _, err := r.pickClaudeProxyAccount(ctx, false)
			if err != nil {
				return err
			}
			return r.proxyClaudeSelectedRemote(ctx, nil, claudeProxyLaunchOptions{
				expectedScope:      scope,
				preferredAccountID: selector,
			})
		}
		options, launchArgs, err := parseClaudeProxyLaunchArgs(args[1:])
		if err != nil {
			return err
		}
		if options.pickPinnedAccount {
			selector, scope, chosen, pickErr := r.pickClaudeProxyAccount(ctx, true)
			if pickErr != nil {
				return pickErr
			}
			if !chosen {
				return nil
			}
			options.accountSelector = selector
			options.expectedScope = scope
		}
		return r.proxyClaudeSelectedRemote(ctx, launchArgs, options)
	}
	cr := claudeRunner{
		store:        claude.DefaultStore(),
		in:           r.in,
		out:          r.out,
		errOut:       r.errOut,
		client:       r.client,
		pushToServer: r.pushClaudeProfileToServer,
		pushAfterAdd: r.pushClaudeProfileAfterAdd,
		pick:         r.pickClaudeProfile,
	}
	return cr.run(ctx, args)
}

type claudeProxyLaunchOptions struct {
	expectedScope      string
	accountSelector    string
	preferredAccountID string
	pickPinnedAccount  bool
}

func claudeProxyScope(server srServerConfig, remote bool) string {
	if !remote {
		return "local"
	}
	endpoint := canonicalServerProxyRootURL(server)
	if parsed, err := url.Parse(endpoint); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		parsed.Host = strings.ToLower(parsed.Host)
		parsed.Fragment = ""
		endpoint = parsed.String()
	}
	identity := make([]string, 0, 3)
	if key := strings.TrimSpace(server.TenantKey); key != "" {
		identity = append(identity, "tenant:"+key)
	}
	if nodeID := strings.TrimSpace(server.TailscaleNodeID); nodeID != "" {
		identity = append(identity, "tailscale-node:"+nodeID)
	}
	if len(identity) == 0 {
		identity = append(identity, "server:"+strings.TrimSpace(server.Name))
	}
	identity = append(identity, "endpoint:"+endpoint)
	return strings.Join(identity, "|")
}

func opaqueClaudeProxyScope(scope string) string {
	hash := sha256.Sum256([]byte(scope))
	return fmt.Sprintf("%x", hash[:12])
}

func validOpaqueClaudeProxyScope(scope string) bool {
	if len(scope) != 24 {
		return false
	}
	for _, char := range scope {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func parseClaudeProxyLaunchArgs(args []string) (options claudeProxyLaunchOptions, launchArgs []string, err error) {
	for i := 0; i < len(args); {
		switch {
		case args[i] == "--account":
			if options.accountSelector != "" {
				return options, nil, fmt.Errorf("--account may be specified only once")
			}
			if i+1 >= len(args) {
				options.pickPinnedAccount = true
				return options, nil, nil
			}
			if args[i+1] == "--" {
				options.pickPinnedAccount = true
				return options, args[i+2:], nil
			}
			if strings.HasPrefix(args[i+1], "-") || strings.TrimSpace(args[i+1]) == "" {
				return options, nil, fmt.Errorf("--account requires a Claude account/profile selector")
			}
			options.accountSelector = strings.TrimSpace(args[i+1])
			i += 2
		case strings.HasPrefix(args[i], "--account="):
			if options.accountSelector != "" {
				return options, nil, fmt.Errorf("--account may be specified only once")
			}
			options.accountSelector = strings.TrimSpace(strings.TrimPrefix(args[i], "--account="))
			if options.accountSelector == "" {
				return options, nil, fmt.Errorf("--account requires a Claude account/profile selector")
			}
			i++
		case args[i] == "--sr-expect-scope":
			if len(args)-i < 3 || args[i+2] != "--" {
				return options, nil, fmt.Errorf("usage: sr claude proxy [--account <account>] --sr-expect-scope <scope> -- [claude args...]")
			}
			if !validOpaqueClaudeProxyScope(args[i+1]) {
				return options, nil, fmt.Errorf("invalid opaque Claude proxy scope")
			}
			options.expectedScope = args[i+1]
			return options, args[i+3:], nil
		default:
			// The first non-wrapper argument starts Claude's argv. In particular,
			// preserve a delimiter and everything after it byte-for-byte so an
			// apparent wrapper option there remains literal Claude input.
			return options, args[i:], nil
		}
	}
	return options, nil, nil
}

func (r srRunner) pickClaudeProxyAccount(ctx context.Context, pinned bool) (selector, expectedScope string, chosen bool, err error) {
	server, remote, err := r.selectedRemoteServer()
	if err != nil {
		return "", "", false, err
	}
	if !remote {
		if !ensureLocalHealthy(ctx, fallbackHTTPClient(), localBaseURL(), defaultDaemonStarter(), r.errOut) {
			return "", "", false, fmt.Errorf("local proxy is unavailable; run '%s doctor'", r.programOrSubrouter())
		}
		server = srServerConfig{Name: "local", URL: localBaseURL()}
	}
	inventory, err := r.fetchServerAccounts(ctx, server)
	if err != nil {
		return "", "", false, fmt.Errorf("load Claude accounts from server %s: %w", server.Name, err)
	}
	eligible := make([]remoteServerAccount, 0, len(inventory))
	for _, account := range inventory {
		if account.Provider == accounts.ProviderClaude && account.AuthMode == accounts.AuthModeOAuth && validClaudeProxyAccountID(strings.TrimSpace(account.ID)) {
			eligible = append(eligible, account)
		}
	}
	if len(eligible) == 0 {
		return "", "", false, fmt.Errorf("no Claude subscription profiles are available on server %s", server.Name)
	}
	sort.Slice(eligible, func(i, j int) bool { return strings.ToLower(eligible[i].ID) < strings.ToLower(eligible[j].ID) })
	if pinned {
		fmt.Fprintln(r.out, "Choose one Claude account for this PINNED process. No account failover will occur.")
	} else {
		fmt.Fprintln(r.out, "Choose the initial account for this POOLED Claude process. The choice is only an initial preference; failover remains enabled.")
		fmt.Fprintln(r.out, "  0) Automatic current recommendation")
	}
	for i, account := range eligible {
		name := strings.TrimSpace(account.Label)
		if name == "" {
			name = strings.TrimSpace(account.ID)
		}
		fmt.Fprintf(r.out, "  %d) %s\n", i+1, name)
	}
	answer, err := promptLine(r.out, bufio.NewReader(r.in), "Launch account (# or exact profile): ")
	if err != nil {
		return "", "", false, err
	}
	answer = strings.TrimSpace(answer)
	scope := opaqueClaudeProxyScope(claudeProxyScope(server, remote))
	if !pinned && (answer == "" || answer == "0") {
		return "", scope, true, nil
	}
	if pinned && answer == "" {
		return "", scope, false, nil
	}
	if index, parseErr := strconv.Atoi(answer); parseErr == nil && index >= 1 && index <= len(eligible) {
		return eligible[index-1].ID, scope, true, nil
	}
	accountID, err := resolveClaudeProxyAccountSelector(inventory, answer)
	if err != nil {
		return "", "", false, fmt.Errorf("server %s: %w", server.Name, err)
	}
	return accountID, scope, true, nil
}

func (r srRunner) printClaudeProxyScope() error {
	server, remote, err := r.selectedRemoteServer()
	if err != nil {
		return err
	}
	fmt.Fprintf(r.out, "scope=%s\n", opaqueClaudeProxyScope(claudeProxyScope(server, remote)))
	return nil
}

// proxyClaudeSelectedRemote launches Claude without consulting or mutating any
// local Claude profile. Pooled launches leave account selection to the server;
// pinned launches resolve one current server-side Claude profile and carry its
// routing ID only in the authoritative private settings for that child.
func (r srRunner) proxyClaudeSelectedRemote(ctx context.Context, args []string, options claudeProxyLaunchOptions) error {
	server, ok, err := r.selectedRemoteServer()
	if err != nil {
		return err
	}
	scope := claudeProxyScope(server, ok)
	actualScope := opaqueClaudeProxyScope(scope)
	if options.expectedScope != "" && options.expectedScope != actualScope {
		return fmt.Errorf("Claude proxy scope changed (expected %s, current %s); no request was sent", options.expectedScope, actualScope)
	}
	if !ok {
		// Prove the exact local serving store before reading any durable proxy
		// credential or requesting a server-side account inventory.
		localServer, _, servingErr := r.readyLocalServingServerWithAuthority(ctx, defaultDaemonStarter())
		if servingErr != nil {
			return servingErr
		}
		accountID, resolveErr := r.resolveClaudeProxyAccount(ctx, localServer, options.accountSelector)
		if resolveErr != nil {
			return resolveErr
		}
		preferredAccountID := strings.TrimSpace(options.preferredAccountID)
		if preferredAccountID != "" && !validClaudeProxyAccountID(preferredAccountID) {
			return fmt.Errorf("selected Claude preference has an invalid server routing ID")
		}
		config, configErr := cloudModeConfig()
		if configErr != nil {
			return fmt.Errorf("load cmux.com login: %w", configErr)
		}
		if config.EffectiveCredentialSource() == broker.CredentialSourceTeam && !config.Ready() {
			return fmt.Errorf("team credential storage requires login and a selected team; run '%s login'", programBase())
		}
		proxyToken := cloudClientProxyToken(config, localBaseURL())
		if proxyToken == "" {
			proxyToken = "subrouter"
		}
		return r.proxyClaudeArgsTo(ctx, args, localBaseURL(), proxyToken, "local", accountID, preferredAccountID)
	}
	accountID, err := r.resolveClaudeProxyAccount(ctx, server, options.accountSelector)
	if err != nil {
		return err
	}
	preferredAccountID := strings.TrimSpace(options.preferredAccountID)
	if preferredAccountID != "" && !validClaudeProxyAccountID(preferredAccountID) {
		return fmt.Errorf("selected Claude preference has an invalid server routing ID")
	}
	proxyToken := strings.TrimSpace(server.TenantKey)
	if proxyToken == "" {
		proxyToken = "subrouter"
	}
	return r.proxyClaudeArgsToServer(ctx, args, server, proxyToken, scope, accountID, preferredAccountID)
}

func (r srRunner) resolveClaudeProxyAccount(ctx context.Context, server srServerConfig, selector string) (string, error) {
	if strings.TrimSpace(selector) == "" {
		return "", nil
	}
	inventory, err := r.fetchServerAccounts(ctx, server)
	if err != nil {
		return "", fmt.Errorf("validate pinned Claude account on server %s: %w", server.Name, err)
	}
	accountID, err := resolveClaudeProxyAccountSelector(inventory, selector)
	if err != nil {
		return "", fmt.Errorf("server %s: %w", server.Name, err)
	}
	return accountID, nil
}

func resolveClaudeProxyAccountSelector(inventory []remoteServerAccount, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", fmt.Errorf("Claude account/profile selector cannot be empty")
	}
	type match struct {
		account remoteServerAccount
		exact   bool
	}
	findMatches := func(candidates []remoteServerAccount) []match {
		matches := make([]match, 0)
		for _, account := range candidates {
			id := strings.TrimSpace(account.ID)
			label := strings.TrimSpace(account.Label)
			exact := strings.EqualFold(id, selector) || (label != "" && strings.EqualFold(label, selector))
			partial := strings.Contains(strings.ToLower(id), strings.ToLower(selector)) ||
				(label != "" && strings.Contains(strings.ToLower(label), strings.ToLower(selector)))
			if exact || partial {
				matches = append(matches, match{account: account, exact: exact})
			}
		}
		return matches
	}
	eligible := make([]remoteServerAccount, 0, len(inventory))
	for _, account := range inventory {
		if account.Provider == accounts.ProviderClaude && account.AuthMode == accounts.AuthModeOAuth {
			eligible = append(eligible, account)
		}
	}
	// Provider inventories routinely contain Codex and Claude profiles with
	// the same email-derived ID. Resolve within the launch-eligible Claude
	// OAuth set first so an unrelated provider cannot make a valid pin
	// ambiguous. Fall back to the full inventory only to retain actionable
	// wrong-provider and wrong-auth-mode diagnostics.
	matches := findMatches(eligible)
	if len(matches) == 0 {
		matches = findMatches(inventory)
	}
	hasExact := false
	for _, candidate := range matches {
		hasExact = hasExact || candidate.exact
	}
	if hasExact {
		filtered := matches[:0]
		for _, candidate := range matches {
			if candidate.exact {
				filtered = append(filtered, candidate)
			}
		}
		matches = filtered
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("Claude account/profile %q was not found in the current account inventory", selector)
	}
	unique := make(map[string]remoteServerAccount, len(matches))
	for _, candidate := range matches {
		key := strings.ToLower(string(candidate.account.Provider)) + "\x00" +
			strings.ToLower(string(candidate.account.AuthMode)) + "\x00" +
			strings.ToLower(strings.TrimSpace(candidate.account.ID))
		unique[key] = candidate.account
	}
	if len(unique) != 1 {
		return "", fmt.Errorf("account/profile selector %q is ambiguous; use the exact Claude account ID or profile label", selector)
	}
	var account remoteServerAccount
	for _, candidate := range unique {
		account = candidate
	}
	if account.Provider != accounts.ProviderClaude {
		return "", fmt.Errorf("account/profile selector %q resolves to provider %s, not Claude", selector, account.Provider)
	}
	if account.AuthMode != accounts.AuthModeOAuth {
		return "", fmt.Errorf("Claude account/profile %q uses auth mode %s; --account requires a Claude subscription profile", selector, account.AuthMode)
	}
	accountID := strings.TrimSpace(account.ID)
	if !validClaudeProxyAccountID(accountID) {
		return "", fmt.Errorf("Claude account/profile %q has an invalid server routing ID", selector)
	}
	return accountID, nil
}

func validClaudeProxyAccountID(accountID string) bool {
	if accountID == "" || len(accountID) > 256 {
		return false
	}
	for _, char := range accountID {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

// cloudClaude launches Claude against the local proxy. The proxy leases an
// access-only team credential from cmux.com and sends the provider request from
// this machine, so Claude never sees a shared refresh token.
func (r srRunner) cloudClaude(ctx context.Context, args []string) error {
	config, err := cloudModeConfig()
	if err != nil {
		return fmt.Errorf("load cmux.com login: %w", err)
	}
	if !config.TeamModeReady() {
		return fmt.Errorf("cmux.com team vault is not configured; run '%s login'", programBase())
	}
	return r.proxyClaude(
		ctx,
		args,
		cloudClientProxyToken(config, localBaseURL()),
	)
}

func (r srRunner) proxyClaude(
	ctx context.Context,
	args []string,
	localProxyToken string,
) error {
	return r.proxyClaudeTo(ctx, args, localBaseURL(), localProxyToken)
}

func (r srRunner) proxyClaudeTo(
	ctx context.Context,
	args []string,
	baseURL string,
	proxyToken string,
) error {
	configDir, launchArgs, err := proxyClaudeInvocation(
		claude.DefaultStore(),
		args,
	)
	if err != nil {
		return err
	}
	return r.runProxyClaude(ctx, launchArgs, baseURL, proxyToken, configDir, "", "")
}

func (r srRunner) proxyClaudeArgsTo(
	ctx context.Context,
	args []string,
	baseURL string,
	proxyToken string,
	scope string,
	accountID string,
	preferredAccountID string,
) error {
	configDir := claudeProxyConfigDir(r.store.StoreDir(), scope, accountID)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("create isolated Claude proxy config: %w", err)
	}
	if err := os.Chmod(configDir, 0o700); err != nil {
		return fmt.Errorf("secure isolated Claude proxy config: %w", err)
	}
	if err := prepareClaudeProxySharedState(configDir, r.store.StoreDir()); err != nil {
		return fmt.Errorf("prepare shared Claude proxy history: %w", err)
	}
	if sameLocalProxyEndpoint(baseURL, localBaseURL()) {
		_, servingStoreErr := localServingStore(r.store)
		if servingStoreErr != nil {
			return fmt.Errorf("resolve local Claude serving store: %w", servingStoreErr)
		}
		upstreamToken := strings.TrimSpace(proxyToken)
		if upstreamToken == "" {
			upstreamToken = "subrouter"
		}
		relay, relayErr := startLocalServingProxyRelay(
			codexProxyRootURL(baseURL), "v1", "claude", "", upstreamToken,
			accountID, preferredAccountID, r.store,
		)
		if relayErr != nil {
			return fmt.Errorf("start local Claude proxy relay: %w", relayErr)
		}
		defer relay.Close()
		// The durable local token and authoritative account choice stay in the
		// relay. Claude receives only its short-lived process capability.
		return r.runProxyClaude(ctx, args, relay.URL(), relay.Credential(), configDir, "", "")
	}
	return r.runProxyClaude(ctx, args, baseURL, proxyToken, configDir, accountID, preferredAccountID)
}

func (r srRunner) proxyClaudeArgsToServer(
	ctx context.Context,
	args []string,
	server srServerConfig,
	proxyToken string,
	scope string,
	accountID string,
	preferredAccountID string,
) error {
	configDir := claudeProxyConfigDir(r.store.StoreDir(), scope, accountID)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("create isolated Claude proxy config: %w", err)
	}
	if err := os.Chmod(configDir, 0o700); err != nil {
		return fmt.Errorf("secure isolated Claude proxy config: %w", err)
	}
	if err := prepareClaudeProxySharedState(configDir, r.store.StoreDir()); err != nil {
		return fmt.Errorf("prepare shared Claude proxy history: %w", err)
	}
	return r.runProxyClaudeForServerAccount(ctx, args, server, proxyToken, configDir, accountID, preferredAccountID)
}

func claudeProxyConfigDir(storeDir, scope, accountID string) string {
	identity := scope
	if accountID != "" {
		identity += "\x00account:" + accountID
	}
	scopeHash := sha256.Sum256([]byte(identity))
	return filepath.Join(storeDir, "claude-proxy", fmt.Sprintf("%x", scopeHash[:12]))
}

func prepareClaudeProxySharedState(configDir, storeDir string) error {
	defaultStore := claude.DefaultStore()
	if filepath.Clean(storeDir) != filepath.Clean(defaultStore.Dir) {
		// Hermetic/test stores must never attach to the user's real Claude home.
		return nil
	}
	return defaultStore.PrepareSharedStateDir(configDir)
}

func (r srRunner) runProxyClaudeForServer(ctx context.Context, args []string, server srServerConfig, proxyToken, configDir string) error {
	return r.runProxyClaudeForServerAccount(ctx, args, server, proxyToken, configDir, "", "")
}

func (r srRunner) runProxyClaudeForServerAccount(ctx context.Context, args []string, server srServerConfig, proxyToken, configDir, accountID, preferredAccountID string) error {
	return r.runProxyClaudeForServerWithResolvers(
		ctx, args, server, proxyToken, configDir, accountID, preferredAccountID,
		net.DefaultResolver.LookupIPAddr, defaultTailscaleStatusLoader,
	)
}

func (r srRunner) runProxyClaudeForServerWithResolvers(
	ctx context.Context,
	args []string,
	server srServerConfig,
	proxyToken string,
	configDir string,
	accountID string,
	preferredAccountID string,
	lookup serverIPLookup,
	load tailscaleStatusLoader,
) error {
	proxyRoot := canonicalServerProxyRootURL(server)
	protectedServer := server
	parsedProxyRoot, _ := url.Parse(proxyRoot)
	if strings.TrimSpace(protectedServer.TenantKey) == "" && tenantKeyFromURL(parsedProxyRoot) == "" {
		// Profileless traffic still contains prompts and responses even when
		// the legacy compatibility token is non-secret. Force exact transport
		// verification without manufacturing a tenant route segment.
		protectedServer.TenantKey = "protected-profileless-claude"
	}
	secureBaseURL, err := secureTenantServerURLWithResolvers(ctx, proxyRoot, protectedServer, lookup, load)
	if err != nil {
		return err
	}
	return r.launchProxyClaude(ctx, args, secureBaseURL, proxyToken, configDir, accountID, preferredAccountID)
}

func (r srRunner) runProxyClaude(
	ctx context.Context,
	args []string,
	baseURL string,
	proxyToken string,
	configDir string,
	accountID string,
	preferredAccountID string,
) error {
	credential := strings.TrimSpace(proxyToken)
	if credential == "subrouter" {
		credential = ""
	}
	secureBaseURL, err := secureTenantProxyURL(ctx, baseURL, credential)
	if err != nil {
		return err
	}
	return r.launchProxyClaude(ctx, args, secureBaseURL, proxyToken, configDir, accountID, preferredAccountID)
}

func (r srRunner) launchProxyClaude(ctx context.Context, args []string, baseURL, proxyToken, configDir, accountID, preferredAccountID string) error {
	settingsBody, err := proxyClaudeLaunchSettings(baseURL, proxyToken, configDir, accountID, preferredAccountID)
	if err != nil {
		return err
	}
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}
	cmd := exec.CommandContext(ctx, claudePath)
	settingsArg, cleanupSettings, err := attachClaudeLaunchSettings(cmd, settingsBody)
	if err != nil {
		return err
	}
	defer cleanupSettings()
	launchArgs, err := managedClaudeLaunchArgs(args, settingsArg)
	if err != nil {
		return err
	}
	cmd.Args = append([]string{claudePath}, launchArgs...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut

	// The authoritative private settings file carries every routing value. Keep the
	// child environment credential-free so tenant URLs and keys cannot be read
	// through process inspection or inherited by subprocesses.
	cmd.Env = claudeSettingsChildEnvironment(os.Environ(), baseURL, configDir)
	return cmd.Run()
}

func proxyClaudeInvocation(
	store claude.Store,
	args []string,
) (string, []string, error) {
	name := ""
	launchArgs := args
	explicitProfile := false
	switch {
	case len(args) == 0:
		name = store.ActiveProfile()
	case args[0] == "run":
		launchArgs = args[1:]
		if len(launchArgs) > 0 && !strings.HasPrefix(launchArgs[0], "-") {
			name = launchArgs[0]
			explicitProfile = true
			launchArgs = launchArgs[1:]
		} else {
			name = store.ActiveProfile()
		}
	case strings.HasPrefix(args[0], "-"):
		name = store.ActiveProfile()
	default:
		explicitProfile = true
		name = args[0]
		launchArgs = args[1:]
	}
	if name == "" {
		if explicitProfile {
			return "", nil, fmt.Errorf("profile %q not found", name)
		}
		return "", launchArgs, nil
	}
	profile, ok, err := store.MatchProfile(name)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "", nil, fmt.Errorf("profile %q not found", name)
	}
	if err := store.SetActiveProfile(profile.Name); err != nil {
		return "", nil, err
	}
	return store.ClaudeConfigDir(profile.Name), launchArgs, nil
}

func claudeSettingsChildEnvironment(environ []string, baseURL, configDir string) []string {
	environ = envWithoutSubrouterControl(environ)
	env := envWithout(environ, claudeRoutingEnvKeys)
	env = directPlainHTTPEnvironment(env, baseURL)
	if configDir != "" {
		env = upsertEnv(env, "CLAUDE_CONFIG_DIR", configDir)
		env = upsertEnv(env, "CLAUDE_CODE_CONFIG_DIR", configDir)
	}
	return env
}

func (r claudeRunner) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return r.defaultInteractive(ctx)
	}
	switch args[0] {
	case "add":
		options, err := parseClaudeAddArgs(args[1:])
		if err != nil {
			return err
		}
		if options.oauth {
			return r.addOAuth(ctx, options.name)
		}
		return r.addSetupToken(ctx, options)
	case "login":
		// The pre-setup-token flow, kept verbatim: browser OAuth writes a
		// refreshable credential and the profile name defaults to the email.
		options, err := parseClaudeAddArgs(args[1:])
		if err != nil {
			return err
		}
		if options.token != "" || options.tokenFromStdin {
			return fmt.Errorf("sr claude login does not take --token; use 'sr claude add --token'")
		}
		return r.addOAuth(ctx, options.name)
	case "list", "ls", "status":
		return r.list(ctx, false)
	case "switch", "use":
		if len(args) < 2 {
			return r.defaultInteractive(ctx)
		}
		return r.switchProfile(args[1])
	case "remove", "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: sr claude remove <name>")
		}
		return r.remove(ctx, args[1])
	case "env":
		return r.env()
	case "pick":
		if r.pick == nil {
			return fmt.Errorf("pick is not available")
		}
		return r.pick(ctx)
	case "push", "upload":
		if r.pushToServer == nil {
			return fmt.Errorf("server push is not available")
		}
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		if name == "" {
			name = r.store.ActiveProfile()
		}
		if name == "" {
			return fmt.Errorf("usage: sr claude push <name>")
		}
		return r.pushToServer(ctx, name)
	case "run":
		name := ""
		extra := []string{}
		if len(args) > 1 {
			if strings.HasPrefix(args[1], "-") {
				extra = args[1:]
			} else {
				name = args[1]
				extra = args[2:]
			}
		}
		return r.runClaude(ctx, name, extra)
	case "help", "-h", "--help":
		fmt.Fprint(r.out, srClaudeHelp)
		return nil
	default:
		if strings.HasPrefix(args[0], "-") {
			return r.runClaude(ctx, "", args)
		}
		if _, ok := r.store.FindProfile(args[0]); ok {
			return r.runClaude(ctx, args[0], args[1:])
		}
		return fmt.Errorf("unknown command: sr claude %s\n%s", args[0], srClaudeHelp)
	}
}

type claudeAddOptions struct {
	name           string
	token          string
	tokenFromStdin bool
	oauth          bool
}

// parseClaudeAddArgs accepts `[name] [--token TOKEN|-] [--oauth|--setup-token]`
// in any order. A bare `-` after --token reads the token from stdin so scripts
// never place it on a command line.
func parseClaudeAddArgs(args []string) (claudeAddOptions, error) {
	var options claudeAddOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--oauth", arg == "--login":
			options.oauth = true
		case arg == "--setup-token":
			options.oauth = false
		case arg == "--token":
			if i+1 >= len(args) {
				return options, fmt.Errorf("usage: sr claude add [name] --token <token|->")
			}
			i++
			if err := options.setToken(args[i]); err != nil {
				return options, err
			}
		case strings.HasPrefix(arg, "--token="):
			if err := options.setToken(strings.TrimPrefix(arg, "--token=")); err != nil {
				return options, err
			}
		case strings.HasPrefix(arg, "-"):
			return options, fmt.Errorf("unknown option %q\n%s", arg, srClaudeHelp)
		default:
			if options.name != "" {
				return options, fmt.Errorf("usage: sr claude add [name] [--token <token|->] [--oauth]")
			}
			options.name = arg
		}
	}
	if options.oauth && (options.token != "" || options.tokenFromStdin) {
		return options, fmt.Errorf("--oauth and --token are mutually exclusive")
	}
	return options, nil
}

func (o *claudeAddOptions) setToken(value string) error {
	if o.token != "" || o.tokenFromStdin {
		return fmt.Errorf("--token was given twice")
	}
	if value == "-" {
		o.tokenFromStdin = true
		return nil
	}
	o.token = strings.TrimSpace(value)
	if o.token == "" {
		return fmt.Errorf("--token requires a value or - for stdin")
	}
	return nil
}

// addSetupToken is the default `sr claude add`: obtain a one-year Claude setup
// token (by running `claude setup-token`, or from --token), prove it against
// Anthropic, and store it as a refresh-less credential with its expiry
// recorded. Nothing here depends on Claude Code writing a credential file, so
// the profile directory is written by Subrouter alone.
func (r claudeRunner) addSetupToken(ctx context.Context, options claudeAddOptions) error {
	if options.name != "" {
		if err := claude.ValidateProfileNameAllowEmail(options.name); err != nil {
			return err
		}
	}
	token := options.token
	switch {
	case options.tokenFromStdin:
		line, err := bufio.NewReader(r.in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		token = strings.TrimSpace(line)
	case token == "":
		minted, err := r.mintSetupToken(ctx)
		if err != nil {
			return err
		}
		token = minted
	}
	if err := claude.ValidateSetupToken(token); err != nil {
		return err
	}
	issuedAt := time.Now()
	if r.now != nil {
		issuedAt = r.now()
	}
	verify := r.verifyToken
	if verify == nil {
		verify = func(ctx context.Context, token string) error {
			return claude.VerifyAccessToken(ctx, r.client, token)
		}
	}
	fmt.Fprintln(r.out, "Verifying the token with Anthropic...")
	if err := verify(ctx, token); err != nil {
		if errors.Is(err, claude.ErrSetupTokenRejected) {
			return fmt.Errorf("%w; mint a fresh one with 'claude setup-token' and retry", err)
		}
		return fmt.Errorf("could not verify the Claude setup token: %w", err)
	}

	name := options.name
	if name == "" {
		// A setup token carries the inference scope only, so unlike the OAuth
		// flow there is no profile endpoint to infer the email from.
		reader := bufio.NewReader(r.in)
		answer, err := promptLine(r.out, reader, "Profile name (e.g. work or you@example.com): ")
		if err != nil {
			return err
		}
		name = strings.TrimSpace(answer)
		if name == "" {
			return fmt.Errorf("a profile name is required: sr claude add <name>")
		}
		if err := claude.ValidateProfileNameAllowEmail(name); err != nil {
			return err
		}
	}
	credential := claude.SetupTokenCredential(token, issuedAt)
	expiresAt, _ := credential.ExpiresAtTime()

	if r.ephemeral {
		if r.pushAfterAdd == nil {
			return fmt.Errorf("hosted Claude upload is unavailable")
		}
		if err := r.store.ImportProfileCredential(name, credential); err != nil {
			return err
		}
		if err := r.pushAfterAdd(ctx, name); err != nil {
			return fmt.Errorf("upload Claude credential: %w", err)
		}
		fmt.Fprintf(r.out, "\nAdded Claude account %q to hosted cmux (setup token, expires %s).\n", name, formatSetupTokenExpiry(expiresAt, issuedAt))
		fmt.Fprintln(r.out, "Local Claude auth was left unchanged.")
		return nil
	}

	imported := false
	if _, err := r.mutateProfileInventory(ctx, func() (bool, error) {
		err := r.store.ImportProfileCredential(name, credential)
		imported = err == nil
		return imported, err
	}); err != nil {
		if imported {
			// The credential is durably registered; a publication teardown
			// failure must not delete a profile a worker can already observe.
			return fmt.Errorf("register Claude profile committed before publication teardown failed: %w", err)
		}
		return err
	}

	fmt.Fprintf(r.out, "\nAdded Claude profile %q from a setup token.\n", name)
	fmt.Fprintf(r.out, "Expires %s. Re-run 'sr claude add %s' before then; setup tokens do not renew.\n", formatSetupTokenExpiry(expiresAt, issuedAt), name)
	if r.pushAfterAdd != nil {
		if err := r.pushAfterAdd(ctx, name); err != nil {
			fmt.Fprintf(r.errOut, "warning: server upload failed (profile stays local-only): %v\n", err)
			fmt.Fprintf(r.errOut, "Retry with: sr claude push %s\n", name)
		}
	}
	fmt.Fprintf(r.out, "\n  sr claude switch %s\n", name)
	fmt.Fprintf(r.out, "  sr claude run %s\n", name)
	return nil
}

// mintSetupToken runs `claude setup-token` attached to the user's terminal and
// then asks for the token it printed. Claude Code shows the token exactly once
// and stores it nowhere, so the paste is the only handoff; the prompt is
// masked when stdin is a terminal.
func (r claudeRunner) mintSetupToken(ctx context.Context) (string, error) {
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return "", fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download, or pass --token <token> from a machine that has it")
	}
	// A scratch config dir keeps the mint away from the user's own Claude
	// login and skips the first-run wizard; setup-token itself writes nothing.
	scratchDir, err := os.MkdirTemp("", "sr-claude-setup-token-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(scratchDir)
	if err := prepareClaudeLoginFastPath(scratchDir); err != nil {
		fmt.Fprintf(r.errOut, "Warning: could not pre-seed the Claude scratch config: %s\n", err)
	}
	fmt.Fprintln(r.out, "Starting 'claude setup-token'...")
	fmt.Fprintln(r.out, "Complete the browser login. Claude prints a token valid for one year; paste it at the prompt that follows.")
	fmt.Fprintln(r.out)
	cmd := exec.CommandContext(ctx, claudePath, "setup-token")
	cmd.Dir = scratchDir
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	cmd.Env = claude.EnvForConfigDir(scratchDir)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude setup-token did not complete: %w", err)
	}
	r.restoreTerminal()
	fmt.Fprintln(r.out)
	reader := bufio.NewReader(r.in)
	token, err := promptSecret(r.out, reader, r.in, "Paste the token printed above (sk-ant-oat01-...): ")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(token), nil
}

// formatSetupTokenExpiry renders "2027-09-02 (in 365 days)". Days are rounded
// so a token minted seconds ago reads as one year, not 364 days.
func formatSetupTokenExpiry(expiresAt, now time.Time) string {
	days := int(math.Round(expiresAt.Sub(now).Hours() / 24))
	switch {
	case days < 0:
		return expiresAt.UTC().Format("2006-01-02") + " (expired)"
	case days == 0:
		return expiresAt.UTC().Format("2006-01-02") + " (today)"
	case days == 1:
		return expiresAt.UTC().Format("2006-01-02") + " (in 1 day)"
	default:
		return fmt.Sprintf("%s (in %d days)", expiresAt.UTC().Format("2006-01-02"), days)
	}
}

// addOAuth is the classic browser OAuth login. Claude Code writes a refreshable
// credential into the profile directory and Subrouter adopts it.
func (r claudeRunner) addOAuth(ctx context.Context, name string) error {
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}

	var instancePath string
	var tempDir string
	var err error
	if name != "" {
		created, createErr := r.mutateProfileInventory(ctx, func() (bool, error) {
			var createErr error
			instancePath, createErr = r.store.CreateProfile(name)
			return createErr == nil, createErr
		})
		if createErr != nil {
			if created {
				rollbackErr := r.rollbackProfileInventory(ctx, name)
				return errors.Join(createErr, wrapClaudeReconcileError("remove Claude profile committed before publication teardown failed", rollbackErr))
			}
			return createErr
		}
	} else {
		instancePath, tempDir, err = r.store.CreateTempInstance()
		if err != nil {
			return err
		}
	}
	claudeConfigDir := r.store.PreferredInstancePath(instancePath)
	if err := prepareClaudeLoginFastPath(claudeConfigDir); err != nil {
		fmt.Fprintf(r.errOut, "Warning: could not pre-seed the login fast path: %s\n", err)
	}

	fmt.Fprintln(r.out, "Starting Claude Code...")
	fmt.Fprintln(r.out, "Complete the OAuth login in your browser; Claude closes automatically once the login lands.")
	fmt.Fprintln(r.out)

	// Passing "/login" as the initial prompt triggers the login flow; with
	// forceLoginMethod seeded, Claude opens the browser directly.
	cmd := exec.CommandContext(ctx, claudePath, "/login")
	cmd.Dir = claudeConfigDir
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	cmd.Env = claude.EnvForConfigDir(claudeConfigDir)
	exitErr, autoClosed := r.runClaudeUntilCredential(ctx, cmd, claudeConfigDir)
	if exitErr != nil && !autoClosed {
		if name != "" {
			if rollbackErr := r.rollbackProfileInventory(ctx, name); rollbackErr != nil {
				return errors.Join(fmt.Errorf("Claude login did not complete: %w", exitErr), fmt.Errorf("remove incomplete Claude profile: %w", rollbackErr))
			}
		} else {
			_ = r.store.CleanupInstance(tempDir)
		}
		return fmt.Errorf("Claude login did not complete: %w", exitErr)
	}

	status, err := claude.AuthStatusForPath(ctx, claudePath, claudeConfigDir)
	if err != nil || status == nil || !status.LoggedIn {
		if name != "" {
			if rollbackErr := r.rollbackProfileInventory(ctx, name); rollbackErr != nil {
				return errors.Join(errors.New("login was not completed"), fmt.Errorf("remove incomplete Claude profile: %w", rollbackErr))
			}
		} else {
			_ = r.store.CleanupInstance(tempDir)
		}
		return fmt.Errorf("login was not completed")
	}
	if r.afterAuthVerified != nil {
		r.afterAuthVerified()
	}

	profileName := name
	if profileName == "" {
		profileName = status.Email
		if profileName == "" {
			profileName = "default"
		}
		registered := false
		_, registerErr := r.mutateProfileInventory(ctx, func() (bool, error) {
			if _, ok := r.store.FindProfile(profileName); ok {
				removed, removeErr := r.store.RemoveProfile(profileName)
				if removeErr != nil {
					return removed, removeErr
				}
			}
			err := r.store.RegisterProfile(profileName, tempDir)
			registered = err == nil
			return registered, err
		})
		if registerErr != nil {
			// The outer publication lock can report a teardown error after the
			// registry write committed. Never delete a credential directory that
			// the durable registry can still name. The explicit committed bit also
			// fails closed if the registry cannot subsequently be read or changes
			// concurrently after this transaction releases its lock.
			if registered || r.profileInventoryReferencesDir(tempDir) {
				return fmt.Errorf("register Claude profile committed before publication teardown failed: %w", registerErr)
			}
			if cleanupErr := r.cleanupTemporaryInstance(ctx, tempDir); cleanupErr != nil {
				return errors.Join(registerErr, fmt.Errorf("clean up authenticated temporary Claude profile: %w", cleanupErr))
			}
			return registerErr
		}
	} else if published, err := r.publishProfileCompletion(ctx); err != nil {
		// OAuth has already committed outside Subrouter's process. A cancellation
		// at this boundary must not leave the profile usable on disk but invisible
		// to a worker that consumed the earlier, credential-less generation.
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claudeProfileReconcileTimeout)
		retryPublished, reconcileErr := r.publishProfileCompletion(reconcileCtx)
		cancel()
		if reconcileErr != nil {
			if published || retryPublished {
				// At least one generation was durably written and its completion
				// mutation ran. A teardown failure must not reclassify that credential
				// as unpublished and delete a profile a worker can already observe.
				return errors.Join(
					fmt.Errorf("publish completed Claude profile: %w", err),
					fmt.Errorf("retry completed Claude profile publication: %w", reconcileErr),
				)
			}
			// A persistent generation-path failure would make the normal published
			// rollback fail for the same reason as both completion attempts. Journal
			// the exact profile identity before removing it so a restarted worker can
			// complete the deletion while every worker remains fail-closed.
			rollbackCtx, rollbackCancel := context.WithTimeout(context.WithoutCancel(ctx), claudeProfileReconcileTimeout)
			removed, rollbackErr := proxy.RollbackUnpublishedClaudeProfileDiskMutation(rollbackCtx, r.store.Dir, name)
			rollbackCancel()
			if !removed && rollbackErr == nil {
				rollbackErr = fmt.Errorf("Claude profile %q was not present for unpublished rollback", name)
			}
			return errors.Join(
				fmt.Errorf("publish completed Claude profile: %w", err),
				fmt.Errorf("retry completed Claude profile publication: %w", reconcileErr),
				wrapClaudeReconcileError("remove unpublished Claude profile", rollbackErr),
			)
		}
	}

	plan := ""
	if status.SubscriptionType != "" {
		plan = " [" + status.SubscriptionType + "]"
	}
	email := ""
	if status.Email != "" {
		email = " (" + status.Email + ")"
	}
	if r.ephemeral {
		if r.pushAfterAdd == nil {
			return fmt.Errorf("hosted Claude upload is unavailable")
		}
		if err := r.pushAfterAdd(ctx, profileName); err != nil {
			return fmt.Errorf("upload Claude credential: %w", err)
		}
		fmt.Fprintf(r.out, "\nAdded Claude account %q to hosted cmux.%s%s\n", profileName, email, plan)
		fmt.Fprintln(r.out, "Local Claude auth was left unchanged.")
		return nil
	}

	fmt.Fprintf(r.out, "\nAdded Claude profile %q.%s%s\n", profileName, email, plan)
	if r.pushAfterAdd != nil {
		if err := r.pushAfterAdd(ctx, profileName); err != nil {
			fmt.Fprintf(r.errOut, "warning: server upload failed (profile stays local-only): %v\n", err)
			fmt.Fprintf(r.errOut, "Retry with: sr claude push %s\n", profileName)
		}
	}
	fmt.Fprintf(r.out, "\n  sr claude switch %s\n", profileName)
	fmt.Fprintf(r.out, "  sr claude run %s\n", profileName)
	return nil
}

// prepareClaudeLoginFastPath seeds a fresh profile's config dir so Claude
// Code boots straight into the browser OAuth flow instead of walking the
// first-run wizard: onboarding is marked complete (skips the theme picker
// and tour) and the login method is pinned to the Claude-account flow
// (skips the claude.ai-vs-Console picker). Existing values are preserved,
// so re-running add against a lived-in profile changes nothing.
func prepareClaudeLoginFastPath(configDir string) error {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	statePath := filepath.Join(configDir, ".claude.json")
	state := map[string]any{}
	if body, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(body, &state); err != nil {
			return fmt.Errorf("parse %s: %w", statePath, err)
		}
	}
	if onboarded, _ := state["hasCompletedOnboarding"].(bool); !onboarded {
		state["hasCompletedOnboarding"] = true
		out, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			return err
		}
		out = append(out, '\n')
		if err := os.WriteFile(statePath, out, 0o600); err != nil {
			return err
		}
	}
	settingsPath := filepath.Join(configDir, "settings.json")
	settings := map[string]any{}
	if body, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(body, &settings); err != nil {
			return fmt.Errorf("parse %s: %w", settingsPath, err)
		}
	}
	if _, ok := settings["forceLoginMethod"]; ok {
		return nil
	}
	settings["forceLoginMethod"] = "claudeai"
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(settingsPath, out, 0o600)
}

func (r claudeRunner) list(ctx context.Context, numbered bool) error {
	infos := r.fetchInfos(ctx)
	displayClaudeProfiles(r.out, infos, numbered)
	return nil
}

func (r claudeRunner) defaultInteractive(ctx context.Context) error {
	profiles := r.store.ListProfiles()
	if len(profiles) == 0 {
		fmt.Fprintln(r.out, "No Claude profiles. Run 'sr claude add' to create one.")
		return nil
	}
	infos := r.fetchInfos(ctx)
	displayClaudeProfiles(r.out, infos, true)
	reader := bufio.NewReader(r.in)
	answer, err := promptLine(r.out, reader, "Switch to (#): ")
	if err != nil {
		return err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return nil
	}
	if idx, err := strconv.Atoi(answer); err == nil && idx >= 1 && idx <= len(infos) {
		return r.switchProfile(infos[idx-1].Name)
	}
	return r.switchProfile(answer)
}

func (r claudeRunner) fetchInfos(ctx context.Context) []claude.ProfileInfo {
	profiles := r.store.ListProfiles()
	active := r.store.ActiveProfile()
	claudePath, _ := claude.DetectCLI()
	infos := make([]claude.ProfileInfo, len(profiles))
	var wg sync.WaitGroup
	for i, profile := range profiles {
		i, profile := i, profile
		infos[i] = claude.ProfileInfo{Name: profile.Name, Active: profile.Name == active, CreatedAt: profile.CreatedAt}
		wg.Add(1)
		go func() {
			defer wg.Done()
			claudeConfigDir := r.store.ClaudeConfigDir(profile.Name)
			if _, err := secureManagedClaudeProfileTransport(claudeConfigDir); err != nil {
				infos[i].Error = err
				return
			}
			var auth *claude.AuthStatus
			var credential *claude.CredentialInfo
			var authErr, credentialErr error
			var inner sync.WaitGroup
			inner.Add(2)
			go func() {
				defer inner.Done()
				auth, authErr = claude.AuthStatusForPath(ctx, claudePath, claudeConfigDir)
			}()
			go func() {
				defer inner.Done()
				credential, credentialErr = r.store.ReadCredential(ctx, claudeConfigDir)
			}()
			inner.Wait()
			if authErr != nil {
				infos[i].Error = authErr
				return
			}
			if credentialErr != nil {
				infos[i].Error = credentialErr
				return
			}
			infos[i].Auth = auth
			infos[i].Credential = credential
			if credential != nil && credential.AccessToken != "" {
				usage, err := claude.FetchUsage(ctx, r.client, credential.AccessToken)
				if err == nil {
					infos[i].Usage = usage
				}
			}
		}()
	}
	wg.Wait()
	return infos
}

func (r claudeRunner) switchProfile(selector string) error {
	profile, ok, err := r.store.MatchProfile(selector)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no profile matching %q", selector)
	}
	if err := r.store.SetActiveProfile(profile.Name); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Active Claude profile: %s\n", profile.Name)
	configDir := r.store.ClaudeConfigDir(profile.Name)
	launchMode, err := managedClaudeProfileLaunchMode(configDir)
	if err != nil {
		return err
	}
	if launchMode == managedClaudeLaunchNeedsMigration {
		fmt.Fprintf(r.out, "\nThis legacy plaintext profile needs a durable server identity. Repair it first with:\n\n  sr claude push %s\n\nThen launch it with:\n\n  sr claude run %s [claude args...]\n", profile.Name, profile.Name)
		return nil
	}
	if launchMode == managedClaudeLaunchWrapped {
		fmt.Fprintf(r.out, "\nThis profile uses a protected plaintext server. Launch it with:\n\n  sr claude run %s [claude args...]\n", profile.Name)
		return nil
	}
	fmt.Fprintf(r.out, "\n  export CLAUDE_CONFIG_DIR=%s\n", configDir)
	fmt.Fprintln(r.out, "\nOr add to shell rc: eval \"$(sr claude env)\"")
	return nil
}

func (r claudeRunner) remove(ctx context.Context, selector string) error {
	profile, ok, err := r.store.MatchProfile(selector)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("profile %q not found", selector)
	}
	if err := r.removeProfileInventory(ctx, profile.Name); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Removed Claude profile: %s\n", profile.Name)
	return nil
}

func (r claudeRunner) mutateProfileInventory(ctx context.Context, mutate func() (bool, error)) (committed bool, err error) {
	trackedMutate := func() (bool, error) {
		committed, err = mutate()
		return committed, err
	}
	if r.mutateProfileInventoryForTest != nil {
		err = r.mutateProfileInventoryForTest(ctx, trackedMutate)
		return committed, err
	}
	if r.ephemeral {
		return trackedMutate()
	}
	err = proxy.PublishAccountDiskMutation(ctx, r.store.Dir, trackedMutate)
	return committed, err
}

func (r claudeRunner) removeProfileInventory(ctx context.Context, name string) error {
	_, err := r.mutateProfileInventory(ctx, func() (bool, error) {
		removed, err := r.store.RemoveProfile(name)
		if err != nil {
			return removed, err
		}
		if !removed {
			return false, fmt.Errorf("profile %q not found", name)
		}
		return true, nil
	})
	return err
}

func (r claudeRunner) rollbackProfileInventory(ctx context.Context, name string) error {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claudeProfileReconcileTimeout)
	defer cancel()
	return r.removeProfileInventory(rollbackCtx, name)
}

func (r claudeRunner) removeUnpublishedProfile(ctx context.Context, name string) (bool, error) {
	removed, err := r.store.RemoveUnpublishedProfileContext(ctx, name)
	if err != nil {
		return removed, err
	}
	if !removed {
		return false, fmt.Errorf("profile %q not found", name)
	}
	return true, nil
}

func wrapClaudeReconcileError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func (r claudeRunner) publishProfileCompletion(ctx context.Context) (bool, error) {
	return r.mutateProfileInventory(ctx, func() (bool, error) {
		// Claude writes the credential outside Subrouter's process during the
		// interactive login. Publish a completion generation after verifying it
		// so a worker that observed profile creation before credential landing
		// receives a second, usable snapshot.
		return true, nil
	})
}

func (r claudeRunner) cleanupTemporaryInstance(ctx context.Context, dir string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claudeProfileReconcileTimeout)
	defer cancel()
	return r.store.CleanupInstanceContext(cleanupCtx, dir)
}

func (r claudeRunner) profileInventoryReferencesDir(dir string) bool {
	for _, profile := range r.store.ListProfiles() {
		if profile.Dir == dir {
			return true
		}
	}
	return false
}

func (r claudeRunner) env() error {
	active := r.store.ActiveProfile()
	if active == "" {
		return nil
	}
	configDir := r.store.ClaudeConfigDir(active)
	launchMode, err := managedClaudeProfileLaunchMode(configDir)
	if err != nil {
		return err
	}
	if launchMode == managedClaudeLaunchNeedsMigration {
		return fmt.Errorf("profile %q is a legacy plaintext remote profile; repair it with 'sr claude push %s', then launch it with 'sr claude run %s [claude args...]'", active, active, active)
	}
	if launchMode == managedClaudeLaunchWrapped {
		return fmt.Errorf("profile %q uses a protected plaintext server; launch it with 'sr claude run %s [claude args...]' so Subrouter can verify the server for each run", active, active)
	}
	fmt.Fprintf(r.out, "export CLAUDE_CONFIG_DIR=%s\n", configDir)
	return nil
}

// runClaudeUntilCredential runs the interactive Claude login and polls for the
// OAuth credential landing in the profile's config dir. As soon as it appears,
// the Claude process is closed automatically so the user does not have to exit
// by hand. Returns the process exit error and whether we initiated the close
// (in which case a non-nil exit error is expected and not a failure).
func (r claudeRunner) runClaudeUntilCredential(ctx context.Context, cmd *exec.Cmd, claudeConfigDir string) (error, bool) {
	if err := cmd.Start(); err != nil {
		return err, false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err, false
		case <-ctx.Done():
			err := closeInteractiveProcess(cmd, done)
			return err, true
		case <-ticker.C:
			credential, _ := r.store.ReadCredential(ctx, claudeConfigDir)
			if credential == nil || credential.AccessToken == "" {
				continue
			}
			fmt.Fprintln(r.errOut, "\nLogin detected; closing Claude...")
			err := closeInteractiveProcess(cmd, done)
			r.restoreTerminal()
			return err, true
		}
	}
}

// closeInteractiveProcess ends an interactive TUI process: SIGINT twice (Claude
// requires a double Ctrl-C), then SIGTERM, then SIGKILL, waiting briefly after
// each signal.
func closeInteractiveProcess(cmd *exec.Cmd, done <-chan error) error {
	if cmd.Process == nil {
		return <-done
	}
	for i := 0; i < 2; i++ {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case err := <-done:
			return err
		case <-time.After(750 * time.Millisecond):
		}
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
	}
	_ = cmd.Process.Kill()
	return <-done
}

// restoreTerminal best-effort resets the controlling terminal in case the
// closed TUI left it in raw mode.
func (r claudeRunner) restoreTerminal() {
	stdin, ok := r.in.(*os.File)
	if !ok {
		return
	}
	cmd := exec.Command("stty", "sane")
	cmd.Stdin = stdin
	_ = cmd.Run()
}

func (r claudeRunner) runClaude(ctx context.Context, name string, extra []string) error {
	if name == "" {
		name = r.store.ActiveProfile()
	}
	if name == "" {
		return fmt.Errorf("no profile specified and no active profile set")
	}
	profile, ok, err := r.store.MatchProfile(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("profile %q not found", name)
	}
	// Resolve the existing instance without ClaudeConfigDir: ClaudeConfigDir
	// prepares shared-history links and therefore writes to disk. A rejected
	// local login must leave the profile tree byte-for-byte untouched.
	configDir := r.store.PreferredInstancePath(r.store.InstancePath(profile.Name))
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}
	authStatus := r.authStatus
	if authStatus == nil {
		authStatus = claude.AuthStatusForPath
	}
	auth, err := authStatus(ctx, claudePath, configDir)
	if err != nil {
		return fmt.Errorf("check local Claude profile %q login: %w", profile.Name, err)
	}
	if auth == nil || !auth.LoggedIn {
		return fmt.Errorf("local managed Claude profile %q is not logged in; server-pool availability is separate. Resume through the pool with 'sr claude proxy --resume <session-id>', pin the server-pool account with 'sr claude proxy --account %s', or create a logged-in local profile with 'sr claude add <new-name>'", profile.Name, shellQuote(profile.Name))
	}
	// Login is accepted; profile preparation and the remaining launch mutations
	// are now allowed.
	configDir = r.store.ClaudeConfigDir(profile.Name)
	secureBaseURL, err := secureManagedClaudeProfileTransport(configDir)
	if err != nil {
		return err
	}
	if err := r.store.SetActiveProfile(profile.Name); err != nil {
		return err
	}
	if sessionID := claude.ResumeSessionID(extra); sessionID != "" {
		from, migrateErr := r.store.MigrateSession(profile.Name, sessionID)
		if migrateErr != nil {
			fmt.Fprintf(r.errOut, "warning: could not migrate session %s: %v\n", sessionID, migrateErr)
		} else if from != "" {
			fmt.Fprintf(r.errOut, "Copied session %s from profile %q.\n", sessionID, from)
		}
	}
	launchArgs := extra
	var launchSettingsBody []byte
	if secureBaseURL != "" {
		settingsOverride, settingsErr := managedClaudeLaunchSettings(secureBaseURL, configDir)
		if settingsErr != nil {
			return settingsErr
		}
		launchSettingsBody = settingsOverride
	}
	cmd := exec.CommandContext(ctx, claudePath)
	if len(launchSettingsBody) > 0 {
		settingsArg, cleanupSettings, settingsErr := attachClaudeLaunchSettings(cmd, launchSettingsBody)
		if settingsErr != nil {
			return settingsErr
		}
		defer cleanupSettings()
		launchArgs, err = managedClaudeLaunchArgs(extra, settingsArg)
		if err != nil {
			return err
		}
	}
	cmd.Args = append([]string{claudePath}, launchArgs...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	cmd.Env = claudeSettingsChildEnvironment(claude.EnvForConfigDir(configDir), secureBaseURL, configDir)
	return cmd.Run()
}

func managedClaudeLaunchArgs(args []string, settingsPath string) ([]string, error) {
	clean := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			clean = append(clean, args[i:]...)
			break
		}
		switch {
		case arg == "--settings" || arg == "--managed-settings":
			if i+1 >= len(args) || args[i+1] == "--" || strings.HasPrefix(args[i+1], "-") {
				return nil, fmt.Errorf("%s requires a value", arg)
			}
			i++
		case strings.HasPrefix(arg, "--settings=") || strings.HasPrefix(arg, "--managed-settings="):
			_, value, _ := strings.Cut(arg, "=")
			if strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("%s requires a value", strings.SplitN(arg, "=", 2)[0])
			}
			// Drop user-provided settings and higher-precedence managed settings
			// before the option terminator. The verified transport overlay is the
			// only global settings source that can affect the launch.
		default:
			clean = append(clean, arg)
		}
	}
	return append([]string{"--settings", settingsPath}, clean...), nil
}

func isolatedClaudeSettingSourcesArgs(args []string) ([]string, error) {
	clean := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			clean = append(clean, args[i:]...)
			break
		}
		switch {
		case arg == "--setting-sources":
			if i+1 >= len(args) || args[i+1] == "--" {
				return nil, fmt.Errorf("%s requires a value", arg)
			}
			i++
		case strings.HasPrefix(arg, "--setting-sources="):
			// Drop caller-selected persisted sources. Direct mode must not reload a
			// config selector or provider route after its environment is scrubbed.
		default:
			clean = append(clean, arg)
		}
	}
	return append([]string{"--setting-sources", ""}, clean...), nil
}

func secureManagedClaudeProfileTransport(configDir string) (string, error) {
	return secureManagedClaudeProfileTransportWithResolvers(configDir, net.DefaultResolver.LookupIPAddr, defaultTailscaleStatusLoader)
}

func secureManagedClaudeProfileTransportWithResolvers(configDir string, lookup serverIPLookup, load tailscaleStatusLoader) (string, error) {
	settingsPath := filepath.Join(configDir, "settings.json")
	body, err := os.ReadFile(settingsPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read managed Claude settings: %w", err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		return "", fmt.Errorf("parse managed Claude settings: %w", err)
	}
	baseURL := strings.TrimSpace(settings.Env["ANTHROPIC_BASE_URL"])
	if baseURL == "" {
		return "", nil
	}
	authToken := strings.TrimSpace(settings.Env["ANTHROPIC_AUTH_TOKEN"])
	transportCredential := authToken
	if transportCredential == "subrouter" {
		transportCredential = ""
	}
	if transportCredential == "" {
		for _, key := range []string{
			"ANTHROPIC_API_KEY",
			"CLAUDE_CODE_OAUTH_TOKEN",
			"CLAUDE_CODE_API_KEY",
			"CLAUDE_CODE_AUTH_TOKEN",
		} {
			if strings.TrimSpace(settings.Env[key]) != "" {
				transportCredential = "protected-managed-credential"
				break
			}
		}
	}
	if transportCredential == "" && strings.TrimSpace(settings.Env["ANTHROPIC_CUSTOM_HEADERS"]) != "" {
		transportCredential = "protected-managed-custom-header"
	}
	serverURL := strings.TrimSpace(settings.Env[managedClaudeServerURLEnv])
	nodeID := strings.TrimSpace(settings.Env[managedClaudeTailscaleNodeEnv])
	if serverURL != "" || nodeID != "" {
		if serverURL == "" || nodeID == "" || baseURL != managedClaudeBlockedBaseURL {
			return "", fmt.Errorf("managed Claude server identity is incomplete; run 'sr claude push' to repair it")
		}
		server := srServerConfig{
			URL:             serverURL,
			TailscaleNodeID: nodeID,
		}
		if tenant.ValidKeyFormat(authToken) {
			server.TenantKey = authToken
		}
		proxyRoot := canonicalServerProxyRootURL(server)
		protectedServer := server
		parsedProxyRoot, _ := url.Parse(proxyRoot)
		if strings.TrimSpace(protectedServer.TenantKey) == "" && tenantKeyFromURL(parsedProxyRoot) == "" {
			// Force transport protection without allowing an arbitrary Claude
			// credential to become a tenant route segment.
			protectedServer.TenantKey = "protected-managed-credential"
		}
		secureBaseURL, err := secureTenantServerURLWithResolvers(context.Background(), proxyRoot, protectedServer, lookup, load)
		if err != nil {
			return "", fmt.Errorf("managed Claude profile has unsafe proxy transport: %w", err)
		}
		return secureBaseURL, nil
	}
	parsed, _ := url.Parse(baseURL)
	if parsed != nil && strings.EqualFold(parsed.Scheme, "http") && !isLoopbackServerHost(parsed.Hostname()) {
		return "", fmt.Errorf("managed Claude profile has unsafe proxy transport: plaintext server is missing an exact durable identity; run 'sr claude push' to repair it")
	}
	secureBaseURL, err := secureTenantServerURLWithResolvers(
		context.Background(), baseURL,
		srServerConfig{URL: baseURL, TenantKey: transportCredential},
		lookup, load,
	)
	if err != nil {
		return "", fmt.Errorf("managed Claude profile has unsafe proxy transport: %w", err)
	}
	return secureBaseURL, nil
}

type managedClaudeLaunchMode uint8

const (
	managedClaudeLaunchDirect managedClaudeLaunchMode = iota
	managedClaudeLaunchWrapped
	managedClaudeLaunchNeedsMigration
)

func managedClaudeProfileLaunchMode(configDir string) (managedClaudeLaunchMode, error) {
	body, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return managedClaudeLaunchDirect, nil
	}
	if err != nil {
		return managedClaudeLaunchDirect, fmt.Errorf("read managed Claude settings: %w", err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		return managedClaudeLaunchDirect, fmt.Errorf("parse managed Claude settings: %w", err)
	}
	if strings.TrimSpace(settings.Env[managedClaudeServerURLEnv]) != "" || strings.TrimSpace(settings.Env[managedClaudeTailscaleNodeEnv]) != "" {
		return managedClaudeLaunchWrapped, nil
	}
	parsed, _ := url.Parse(strings.TrimSpace(settings.Env["ANTHROPIC_BASE_URL"]))
	if parsed != nil && strings.EqualFold(parsed.Scheme, "http") && !isLoopbackServerHost(parsed.Hostname()) {
		return managedClaudeLaunchNeedsMigration, nil
	}
	return managedClaudeLaunchDirect, nil
}

func managedClaudeLaunchSettings(secureBaseURL, configDir string) ([]byte, error) {
	return claudeLaunchSettingsJSON(configDir, map[string]string{"ANTHROPIC_BASE_URL": secureBaseURL})
}

func proxyClaudeLaunchSettings(baseURL, proxyToken, configDir string, accountIDs ...string) ([]byte, error) {
	accountID := ""
	preferredAccountID := ""
	if len(accountIDs) > 2 {
		return nil, fmt.Errorf("encode Claude proxy launch settings: too many account routes")
	}
	if len(accountIDs) >= 1 {
		accountID = strings.TrimSpace(accountIDs[0])
		if accountID != "" && !validClaudeProxyAccountID(accountID) {
			return nil, fmt.Errorf("encode Claude proxy launch settings: invalid forced account ID")
		}
	}
	if len(accountIDs) == 2 {
		preferredAccountID = strings.TrimSpace(accountIDs[1])
		if preferredAccountID != "" && !validClaudeProxyAccountID(preferredAccountID) {
			return nil, fmt.Errorf("encode Claude proxy launch settings: invalid preferred account ID")
		}
	}
	if accountID != "" && preferredAccountID != "" {
		return nil, fmt.Errorf("encode Claude proxy launch settings: forced and preferred accounts are mutually exclusive")
	}
	baseURL = strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	customHeaders := "X-Subrouter-Agent: claude"
	if accountID != "" {
		customHeaders += "\nX-Subrouter-Account-ID: " + accountID
	} else if preferredAccountID != "" {
		customHeaders += "\nX-Subrouter-Preferred-Account-ID: " + preferredAccountID
	}
	return claudeLaunchSettingsJSON(configDir, map[string]string{
		"ANTHROPIC_BASE_URL":       baseURL,
		"ANTHROPIC_AUTH_TOKEN":     proxyToken,
		"ANTHROPIC_CUSTOM_HEADERS": customHeaders,
	})
}

func claudeLaunchSettingsJSON(configDir string, env map[string]string) ([]byte, error) {
	// Claude merges --settings with the selected CLAUDE_CONFIG_DIR settings.
	// Empty strings are Claude's own neutral value for provider-selection flags:
	// its provider overlay clears every flag before enabling one. Clear every
	// known routing value here as well so a reused profile cannot retain a
	// Bedrock, Vertex, gateway, or other alternate-provider route underneath the
	// private launch settings. Intended Subrouter values are applied last.
	authoritative := make(map[string]string, len(claudeRoutingEnvKeys)+len(env))
	for _, key := range claudeRoutingEnvKeys {
		// An explicitly empty CLAUDE_CONFIG_DIR is not the default directory:
		// Claude treats it as the current working directory. An explicitly set
		// ~/.claude also selects a separate Keychain namespace on macOS. For a
		// normal direct login, leave both selectors absent after scrubbing the
		// child environment; managed profiles pass a non-empty directory below.
		if configDir == "" && (key == "CLAUDE_CONFIG_DIR" || key == "CLAUDE_CODE_CONFIG_DIR") {
			continue
		}
		authoritative[key] = ""
	}
	if configDir != "" {
		// Keep Claude's dynamically consulted config selectors pinned to the same
		// durable profile selected for the child process. Clearing them would route
		// later session reads and writes back to the user's default profile.
		authoritative["CLAUDE_CONFIG_DIR"] = configDir
		authoritative["CLAUDE_CODE_CONFIG_DIR"] = configDir
	}
	for key, value := range env {
		authoritative[key] = value
	}
	override, err := json.Marshal(map[string]any{"env": authoritative})
	if err != nil {
		return nil, fmt.Errorf("encode managed Claude launch settings: %w", err)
	}
	return override, nil
}

// claudeAWS launches Claude Code in Amazon Bedrock gateway mode, routed through
// the default Subrouter server's /bedrock endpoint. The server holds the AWS
// credentials and SigV4-signs each request, so teammates need no AWS access.
// All flags after an optional --model are passed through to Claude Code
// unchanged. --model accepts a friendly alias (fable, opus, sonnet, haiku) or a
// full Bedrock model id / inference profile; it defaults to Fable 5.
func (r srRunner) claudeAWS(ctx context.Context, args []string) error {
	server, ok, err := r.defaultRemoteServer()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("sr claude-aws needs a default Subrouter server; run '%s server use <name>'", r.programOrSubrouter())
	}
	protectedServer := server
	if strings.TrimSpace(protectedServer.TenantKey) == "" {
		protectedServer.TenantKey = "protected-bedrock-request"
	}
	secureRoot, err := secureTenantServerURL(ctx, server.URL, protectedServer)
	if err != nil {
		return err
	}
	baseURL := strings.TrimRight(secureRoot, "/") + "/bedrock"

	model := "fable"
	region := "us-east-1"
	passthrough := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--model", "-m":
			if i+1 >= len(args) {
				return fmt.Errorf("--model requires a value")
			}
			model = args[i+1]
			i++
		case "--aws-region":
			if i+1 >= len(args) {
				return fmt.Errorf("--aws-region requires a value")
			}
			region = args[i+1]
			i++
		default:
			passthrough = append(passthrough, args[i])
		}
	}

	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}
	cmd := exec.CommandContext(ctx, claudePath, passthrough...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	gatewayToken := strings.TrimSpace(os.Getenv("SUBROUTER_BEDROCK_GATEWAY_TOKEN"))
	env := claudeAWSChildEnvironment(os.Environ(), baseURL, region, model, gatewayToken)
	cmd.Env = directPlainHTTPEnvironment(env, baseURL)
	return cmd.Run()
}

func claudeAWSChildEnvironment(environ []string, baseURL, region, model, gatewayToken string) []string {
	env := envWithoutSubrouterControl(environ)
	env = envWithout(env, claudeRoutingEnvKeys)
	env = envWithoutPrefix(env, "AWS_")
	env = append(env,
		"CLAUDE_CODE_USE_BEDROCK=1",
		"CLAUDE_CODE_SKIP_BEDROCK_AUTH=1",
		"ANTHROPIC_BEDROCK_BASE_URL="+baseURL,
		"AWS_REGION="+region,
		"AWS_DEFAULT_REGION="+region,
		"ANTHROPIC_MODEL="+bedrockModelID(model),
		"ANTHROPIC_SMALL_FAST_MODEL="+bedrockSmallFastModelID,
	)
	if gatewayToken != "" {
		env = append(env, "ANTHROPIC_AUTH_TOKEN="+gatewayToken)
	}
	return env
}

// claudeDirect launches Claude Code straight against Anthropic on the user's own
// claude.ai login, guaranteeing subrouter (and any Bedrock/Vertex/Mantle
// gateway) is not used. It strips every routing/gateway env var plus
// ANTHROPIC_API_KEY (which would otherwise override the claude.ai login and bill
// pay-per-token), so the run cannot be silently pointed at a proxy or API key,
// then passes all flags through unchanged. Direct mode leaves both config
// selectors absent, which is Claude's only login-preserving spelling of the
// normal config/Keychain identity on macOS.
func (r srRunner) claudeDirect(ctx context.Context, args []string) error {
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}
	settingsBody, err := claudeLaunchSettingsJSON("", nil)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, claudePath)
	settingsArg, cleanupSettings, err := attachClaudeLaunchSettings(cmd, settingsBody)
	if err != nil {
		return err
	}
	defer cleanupSettings()
	launchArgs, err := managedClaudeLaunchArgs(args, settingsArg)
	if err != nil {
		return err
	}
	launchArgs, err = isolatedClaudeSettingSourcesArgs(launchArgs)
	if err != nil {
		return err
	}
	cmd.Args = append([]string{claudePath}, launchArgs...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	cmd.Env = envWithout(envWithoutSubrouterControl(os.Environ()), claudeRoutingEnvKeys)
	return cmd.Run()
}

// claudeRoutingEnvKeys is shared with managed login/auth-status launches so
// every Claude child applies the same routing boundary.
var claudeRoutingEnvKeys = claude.RoutingEnvKeys()

// envWithout returns environ with the named keys removed (case-insensitive).
func envWithout(environ []string, keys []string) []string {
	drop := make(map[string]bool, len(keys))
	for _, k := range keys {
		drop[strings.ToLower(k)] = true
	}
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if drop[strings.ToLower(name)] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// envWithoutSubrouterControl prevents a vendor child from inheriting either
// control-plane credentials or paths that locate credential-bearing files.
// Launch-specific short-lived capabilities are added only after this scrub.
func envWithoutSubrouterControl(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, item := range environ {
		name := item
		if before, _, ok := strings.Cut(item, "="); ok {
			name = before
		}
		upper := strings.ToUpper(strings.TrimSpace(name))
		if strings.HasPrefix(upper, "SUBROUTER_") {
			continue
		}
		out = append(out, item)
	}
	return out
}

func envWithoutPrefix(environ []string, prefix string) []string {
	prefix = strings.ToUpper(strings.TrimSpace(prefix))
	out := make([]string, 0, len(environ))
	for _, item := range environ {
		name := item
		if before, _, ok := strings.Cut(item, "="); ok {
			name = before
		}
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(name)), prefix) {
			continue
		}
		out = append(out, item)
	}
	return out
}

const bedrockSmallFastModelID = "us.anthropic.claude-haiku-4-5-20251001-v1:0"

// bedrockModelID maps a friendly model alias to a Bedrock inference profile id.
// A value that already looks like a Bedrock id (contains "anthropic.") is passed
// through unchanged, as is any unrecognized value.
func bedrockModelID(name string) string {
	trimmed := strings.TrimSpace(name)
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "anthropic.") {
		return trimmed
	}
	switch lower {
	case "", "fable", "fable-5", "fable5", "claude-fable-5":
		return "us.anthropic.claude-fable-5"
	case "opus", "claude-opus-4-8", "opus-4-8":
		return "us.anthropic.claude-opus-4-8"
	case "sonnet", "claude-sonnet-5", "sonnet-5":
		return "us.anthropic.claude-sonnet-5"
	case "haiku", "claude-haiku-4-5":
		return bedrockSmallFastModelID
	default:
		return trimmed
	}
}

type claudeRow struct {
	label    string
	used     float64
	resetsIn string
}

func displayClaudeProfiles(out io.Writer, infos []claude.ProfileInfo, numbered bool) {
	if len(infos) == 0 {
		fmt.Fprintln(out, "No Claude profiles. Run 'sr claude add' to create one.")
		return
	}
	colored := colorEnabled(out)
	width := 0
	for _, info := range infos {
		for _, row := range collectClaudeRows(info) {
			width = max(width, len(row.label))
		}
	}
	fmt.Fprintln(out)
	for i, info := range infos {
		prefix := ""
		if numbered {
			prefix = fmt.Sprintf("%d) ", i+1)
		}
		active := ""
		if info.Active {
			active = " " + style(colored, ansiCyan, "(active)")
		}
		if info.Error != nil {
			fmt.Fprintf(out, "%s%s%s\n", style(colored, ansiDim, prefix), style(colored, ansiBold+ansiWhite, info.Name), active)
			fmt.Fprintf(out, "  %s\n\n", style(colored, ansiRed, "Error: "+info.Error.Error()))
			continue
		}
		tokenLine := setupTokenStatusLine(info, colored, time.Now())
		if info.Auth == nil || !info.Auth.LoggedIn {
			fmt.Fprintf(out, "%s%s%s\n", style(colored, ansiDim, prefix), style(colored, ansiBold+ansiWhite, info.Name), active)
			if tokenLine != "" {
				// A setup-token profile needs no Claude Code login state; the
				// stored credential and its expiry are the whole story.
				fmt.Fprintln(out, "  "+tokenLine)
			} else {
				fmt.Fprintln(out, "  "+style(colored, ansiDim, "not logged in"))
			}
			fmt.Fprintln(out)
			continue
		}
		plan := ""
		if info.Auth.SubscriptionType != "" {
			plan = " " + style(colored, ansiDim, "["+info.Auth.SubscriptionType+"]")
		}
		fmt.Fprintf(out, "%s%s%s%s\n", style(colored, ansiDim, prefix), style(colored, ansiBold+ansiWhite, info.Name), plan, active)
		if tokenLine != "" {
			fmt.Fprintln(out, "  "+tokenLine)
		}
		rows := collectClaudeRows(info)
		if len(rows) == 0 && tokenLine != "" {
			// A setup token has no profile scope, so the usage endpoint and the
			// email lookup have nothing to add beyond the token line.
			fmt.Fprintln(out)
			continue
		}
		if len(rows) == 0 {
			detail := info.Auth.Email
			if detail == "" {
				detail = "unknown"
			}
			if info.Auth.OrgName != "" {
				detail += " (" + info.Auth.OrgName + ")"
			}
			fmt.Fprintf(out, "  %s\n\n", style(colored, ansiDim, detail))
			continue
		}
		for _, row := range rows {
			fmt.Fprintf(out, "  %s: %s %s", style(colored, ansiDim, pad(row.label, width)), renderBar(row.used, colored), style(colored, usageColor(row.used), formatPercentLeft(row.used)+" left"))
			if row.resetsIn != "" {
				fmt.Fprintf(out, " %s", style(colored, ansiDim, "resets in "+row.resetsIn))
			}
			fmt.Fprintln(out)
		}
		fmt.Fprintln(out)
	}
}

// setupTokenStatusLine describes a long-lived (setup token) credential and
// when it stops working. It returns "" for refreshable OAuth profiles.
func setupTokenStatusLine(info claude.ProfileInfo, colored bool, now time.Time) string {
	if !info.Credential.LongLived() {
		return ""
	}
	expiresAt, ok := info.Credential.ExpiresAtTime()
	if !ok {
		return style(colored, ansiDim, "setup token, expiry unknown")
	}
	remaining := expiresAt.Sub(now)
	switch {
	case remaining <= 0:
		return style(colored, ansiRed, "setup token expired "+expiresAt.UTC().Format("2006-01-02")+" (re-add with: sr claude add "+info.Name+")")
	case remaining <= claude.SetupTokenExpiryWarning:
		return style(colored, ansiYellow, "setup token expires "+formatSetupTokenExpiry(expiresAt, now)+" (re-add with: sr claude add "+info.Name+")")
	default:
		return style(colored, ansiDim, "setup token, expires "+formatSetupTokenExpiry(expiresAt, now))
	}
}

func collectClaudeRows(info claude.ProfileInfo) []claudeRow {
	if info.Usage == nil {
		return nil
	}
	var rows []claudeRow
	add := func(label string, limit *claude.RateLimit) {
		if limit == nil || limit.Utilization == nil {
			return
		}
		rows = append(rows, claudeRow{label: label, used: *limit.Utilization, resetsIn: formatResetTime(limit.ResetsAt)})
	}
	add("5h limit", info.Usage.FiveHour)
	add("7d limit", info.Usage.SevenDay)
	add("Opus (weekly)", info.Usage.SevenDayOpus)
	add("Sonnet (weekly)", info.Usage.SevenDaySonnet)
	if info.Usage.ExtraUsage != nil && info.Usage.ExtraUsage.IsEnabled && info.Usage.ExtraUsage.Utilization != nil {
		rows = append(rows, claudeRow{label: "Extra usage", used: *info.Usage.ExtraUsage.Utilization})
	}
	return rows
}

func formatResetTime(value string) string {
	if value == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return ""
	}
	seconds := int64(time.Until(t).Seconds())
	if seconds < 0 {
		seconds = 0
	}
	return formatDuration(seconds)
}

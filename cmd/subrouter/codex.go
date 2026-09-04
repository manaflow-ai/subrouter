package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/session"
)

const defaultCodexBaseURL = "http://127.0.0.1:31415/v1"

const (
	subrouterCodexLauncherEnv      = "SUBROUTER_CODEX_LAUNCHER"
	subrouterCodexResumeCommandEnv = "SUBROUTER_CODEX_RESUME_COMMAND"
)

// ambientProxyEnvKeys covers the conventional upper- and lower-case spellings
// used by HTTP clients. envWithout compares names case-insensitively, so one
// entry per variable is sufficient.
var ambientProxyEnvKeys = []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"}

func codex(args []string) error {
	bin := envOrDefault("SUBROUTER_CODEX_BIN", "codex")
	if !codexInvocationUsesSubrouter(args) {
		return runCodexCommand(
			bin,
			args,
			envWithout(envWithoutSubrouterControl(os.Environ()), []string{
				subrouterCodexLauncherEnv,
				subrouterCodexResumeCommandEnv,
				"SUBROUTER_CODEX_DUMMY_API_KEY",
			}),
		)
	}
	baseURL, err := codexBaseURLWithFallback(defaultSRServerStore(accounts.DefaultCodexStore()), os.Stderr)
	if err != nil {
		return err
	}
	localTarget := codexResolvedTargetIsBuiltInLocal(
		defaultSRServerStore(accounts.DefaultCodexStore()), baseURL,
	)
	var localRelayTransport *http.Transport
	if localTarget {
		_, servingStoreErr := localServingStore(accounts.DefaultCodexStore())
		if servingStoreErr != nil {
			return fmt.Errorf("resolve local Codex serving store: %w", servingStoreErr)
		}
		// Construct the store-attesting transport before reading the durable
		// daemon credential. Its DialContext proves every connection before the
		// relay can send a credential-bearing request on that connection.
		localRelayTransport, err = localServingRelayTransport(
			codexProxyRootURL(baseURL), accounts.DefaultCodexStore(),
		)
		if err != nil {
			return fmt.Errorf("secure local Codex relay transport: %w", err)
		}
	}
	cloudConfig, err := cloudModeConfig()
	if err != nil {
		return err
	}
	localProxyToken := ""
	if localTarget {
		localProxyToken = cloudClientProxyToken(cloudConfig, baseURL)
	}
	userEmailRaw := os.Getenv("SUBROUTER_CODEX_USER_EMAIL")
	accountID := session.NormalizeAccountID(os.Getenv("SUBROUTER_CODEX_ACCOUNT_ID"))
	userEmail := ""
	if strings.TrimSpace(userEmailRaw) != "" {
		userEmail = session.NormalizeUserEmail(userEmailRaw)
		if userEmail == "" {
			return fmt.Errorf("SUBROUTER_CODEX_USER_EMAIL must be a valid email address; use SUBROUTER_CODEX_ACCOUNT_ID to force an account such as team-codex-1")
		}
	}
	childBaseURL := baseURL
	childProxyToken := localProxyToken
	childUserEmail := userEmail
	childAccountID := accountID
	var relay *nativeProxyRelay
	if localTarget {
		upstreamToken := strings.TrimSpace(localProxyToken)
		if upstreamToken == "" {
			upstreamToken = "subrouter"
		}
		relay, err = startProxyRelay(
			codexProxyRootURL(baseURL), "v1", "codex", "", upstreamToken,
			accountID, "", userEmail, codexModelArg(args), localRelayTransport,
		)
		if err != nil {
			return fmt.Errorf("start local Codex proxy relay: %w", err)
		}
		defer relay.Close()
		childBaseURL = relay.URL() + "/v1"
		childProxyToken = relay.Credential()
		childUserEmail = ""
		childAccountID = ""
	}

	return runCodexCommand(
		bin,
		codexArgsWithLocalProxyToken(
			args,
			childBaseURL,
			childUserEmail,
			childAccountID,
			childProxyToken,
		),
		directPlainHTTPEnvironment(codexChildEnv(os.Environ(), childProxyToken, programBase()), childBaseURL),
	)
}

// codexResolvedTargetIsBuiltInLocal distinguishes the built-in local daemon
// from a deliberately named remote that happens to use a loopback URL. Only
// the built-in target may consult the private local serving-store binding.
func codexResolvedTargetIsBuiltInLocal(store srServerStore, resolvedURL string) bool {
	local := localBaseURL()
	if !sameLocalProxyEndpoint(resolvedURL, local) {
		return false
	}
	if name := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_SERVER")); name != "" {
		return isLocalServerName(name)
	}
	if strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_BASE_URL")) != "" {
		return false
	}
	if config, err := cloudModeConfig(); err == nil {
		source := config.EffectiveCredentialSource()
		if source == broker.CredentialSourceTeam || source == broker.CredentialSourceLocal {
			return true
		}
	}
	file, err := store.load()
	if err != nil || strings.TrimSpace(file.Default) == "" {
		return true
	}
	configured, ok := file.find(file.Default)
	if !ok {
		return true
	}
	configuredURL, err := codexBaseURLForServer(configured)
	if err != nil {
		return true
	}
	// A distinct configured remote that failed health may have fallen back to
	// local; that is the built-in daemon. A configured loopback remote did not.
	return !sameLocalProxyEndpoint(configuredURL, local)
}

func directPlainHTTPEnvironment(environ []string, baseURL string) []string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || !strings.EqualFold(parsed.Scheme, "http") {
		return environ
	}
	return envWithout(environ, ambientProxyEnvKeys)
}

func runCodexCommand(bin string, args, env []string) error {
	cmd := exec.CommandContext(context.Background(), bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env
	return cmd.Run()
}

func codexChildEnv(environ []string, localProxyToken, launcher string) []string {
	environ = envWithoutSubrouterControl(environ)
	launcher = trustedCodexLauncher(launcher)
	environ = upsertEnv(environ, subrouterCodexLauncherEnv, launcher+" codex")
	environ = upsertEnv(environ, subrouterCodexResumeCommandEnv, launcher+" codex resume")
	if localProxyToken != "" {
		environ = upsertEnv(
			environ,
			"SUBROUTER_CODEX_DUMMY_API_KEY",
			localProxyToken,
		)
	}
	return environ
}

func trustedCodexLauncher(launcher string) string {
	switch strings.TrimSpace(launcher) {
	case "sr":
		return "sr"
	case "subrouter":
		return "subrouter"
	case "cx":
		return "cx"
	default:
		return "sr"
	}
}

func codexBaseURL(store srServerStore) (string, error) {
	if baseURL := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_BASE_URL")); baseURL != "" {
		return secureTenantProxyURL(context.Background(), baseURL, "protected-codex-credential")
	}
	if serverName := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_SERVER")); serverName != "" {
		return codexBaseURLForNamedServer(store, serverName)
	}
	return defaultCodexBaseURLFor(store)
}

func defaultCodexBaseURLFor(store srServerStore) (string, error) {
	file, err := store.load()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(file.Default) == "" {
		return defaultCodexBaseURL, nil
	}
	server, ok := file.find(file.Default)
	if !ok {
		return "", fmt.Errorf("default Subrouter server %q not found; run sr server use <name> or sr server clear-default", file.Default)
	}
	return codexBaseURLForServer(server)
}

func codexBaseURLWithTailscaleHealing(store srServerStore, warn io.Writer) (string, error) {
	if baseURL := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_BASE_URL")); baseURL != "" {
		return secureTenantProxyURL(context.Background(), baseURL, "protected-codex-credential")
	}
	serverName := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_SERVER"))
	if serverName == "local" || serverName == "localhost" {
		return defaultCodexBaseURL, nil
	}
	if serverName != "" {
		server, ok, err := store.find(serverName)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("Subrouter server %q not found", serverName)
		}
		server, err = healTailscaleServer(
			context.Background(), store, server, fallbackHTTPClient(), warn, nil,
		)
		if err != nil {
			return "", err
		}
		return codexBaseURLForServer(server)
	}
	file, err := store.load()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(file.Default) == "" {
		return defaultCodexBaseURL, nil
	}
	server, ok := file.find(file.Default)
	if !ok {
		return "", fmt.Errorf("default Subrouter server %q not found; run sr server use <name> or sr server clear-default", file.Default)
	}
	server, err = healTailscaleServer(
		context.Background(), store, server, fallbackHTTPClient(), warn, nil,
	)
	if err != nil {
		return "", err
	}
	return codexBaseURLForServer(server)
}

// codexBaseURLWithFallback resolves the base URL for launching codex, then
// substitutes the local daemon when the configured server is unreachable. An
// explicit SUBROUTER_CODEX_BASE_URL or SUBROUTER_CODEX_SERVER is treated as a
// deliberate pin and is never overridden.
func codexBaseURLWithFallback(store srServerStore, warn io.Writer) (string, error) {
	config, err := cloudModeConfig()
	if err != nil {
		return "", fmt.Errorf("load cmux.com login: %w", err)
	}
	source := config.EffectiveCredentialSource()
	if source == broker.CredentialSourceTeam && !config.Ready() {
		return "", fmt.Errorf("team credential storage requires login and a selected team; run '%s login'", programBase())
	}
	if source == broker.CredentialSourceTeam ||
		source == broker.CredentialSourceLocal {
		local := localBaseURL()
		if strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_BASE_URL")) != "" ||
			strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_SERVER")) != "" {
			pinned, pinErr := codexBaseURLWithTailscaleHealing(store, warn)
			if pinErr != nil {
				return "", pinErr
			}
			pinnedToBuiltInLocal := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_BASE_URL")) == "" &&
				isLocalServerName(os.Getenv("SUBROUTER_CODEX_SERVER"))
			if source == broker.CredentialSourceTeam && !pinnedToBuiltInLocal {
				return "", fmt.Errorf(
					"team credentials may only be sent through the local daemon at %s; unset SUBROUTER_CODEX_BASE_URL and SUBROUTER_CODEX_SERVER",
					local,
				)
			}
			if source == broker.CredentialSourceLocal && !pinnedToBuiltInLocal {
				return pinned, nil
			}
		}
		if !ensureLocalHealthy(
			context.Background(),
			fallbackHTTPClient(),
			local,
			defaultDaemonStarter(),
			warn,
		) {
			return "", fmt.Errorf("local proxy is unavailable; run '%s doctor'", programBase())
		}
		return local, nil
	}

	baseURL, err := codexBaseURLWithTailscaleHealing(store, warn)
	if err != nil {
		// An explicit environment selection is a deliberate fail-closed pin.
		// For the registry default, preserve the existing local fallback contract
		// without ever contacting the stale, identity-unverified remote URL.
		var repairFailure tailscaleRepairFailure
		if !errors.As(err, &repairFailure) ||
			strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_BASE_URL")) != "" ||
			strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_SERVER")) != "" ||
			fallbackDisabled() {
			return "", err
		}
		local := localBaseURL()
		if !ensureLocalHealthy(
			context.Background(),
			fallbackHTTPClient(),
			local,
			defaultDaemonStarter(),
			warn,
		) {
			return "", fmt.Errorf("repair default Subrouter server: %w; local proxy is unavailable", err)
		}
		if warn != nil {
			fmt.Fprintf(warn, "subrouter: cannot safely resolve the configured server (%v); falling back to the local daemon at %s\n", err, local)
		}
		return local, nil
	}
	if strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_BASE_URL")) != "" ||
		strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_SERVER")) != "" {
		// A legacy pin is never substituted, but a local pin is still repaired.
		local := localBaseURL()
		if sameEndpoint(baseURL, local) &&
			!ensureLocalHealthy(
				context.Background(),
				fallbackHTTPClient(),
				local,
				defaultDaemonStarter(),
				warn,
			) {
			return "", fmt.Errorf(
				"local proxy is unavailable; run '%s doctor'",
				programBase(),
			)
		}
		return baseURL, nil
	}
	return withLocalFallback(context.Background(), fallbackHTTPClient(), baseURL, warn), nil
}

func codexBaseURLForNamedServer(store srServerStore, name string) (string, error) {
	if name == "local" || name == "localhost" {
		return defaultCodexBaseURL, nil
	}
	server, ok, err := store.find(name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("Subrouter server %q not found", name)
	}
	return codexBaseURLForServer(server)
}

func codexBaseURLForServer(server srServerConfig) (string, error) {
	root, err := serverProxyRootURL(server)
	if err != nil {
		return "", err
	}
	return root + "/v1", nil
}

// serverProxyRootURL is the data-plane root for a server entry: the bare URL,
// or the tenant-scoped /t/<key> root when the entry carries a tenant key.
func serverProxyRootURL(server srServerConfig) (string, error) {
	root := canonicalServerProxyRootURL(server)
	return secureTenantServerURL(context.Background(), root, server)
}

func canonicalServerProxyRootURL(server srServerConfig) string {
	root := codexProxyRootURL(server.URL)
	if key := strings.TrimSpace(server.TenantKey); key != "" {
		root += "/t/" + key
	}
	return root
}

func codexArgs(args []string, baseURL, userEmail, accountID string) []string {
	return codexArgsWithLocalProxyToken(
		args,
		baseURL,
		userEmail,
		accountID,
		"",
	)
}

func codexArgsWithLocalProxyToken(
	args []string,
	baseURL string,
	userEmail string,
	accountID string,
	localProxyToken string,
) []string {
	if !codexInvocationUsesSubrouter(args) {
		return append([]string(nil), args...)
	}
	args = sanitizeCodexRoutingArgs(args)
	model := codexModelArg(args)
	configArgs := codexConfigArgs(
		baseURL,
		userEmail,
		accountID,
		model,
		localProxyToken != "",
	)
	return appendCodexConfigBeforeTerminator(args, configArgs)
}

// appendCodexConfigBeforeTerminator keeps the launcher's authoritative block
// after every real CLI override so Codex's last-value-wins merge cannot be
// superseded by an equivalent parent-table spelling. Arguments after -- are
// positional data and remain last and untouched.
func appendCodexConfigBeforeTerminator(args, configArgs []string) []string {
	insertAt := len(args)
	for i, arg := range args {
		if arg == "--" {
			insertAt = i
			break
		}
	}
	out := make([]string, 0, len(args)+len(configArgs))
	out = append(out, args[:insertAt]...)
	out = append(out, configArgs...)
	out = append(out, args[insertAt:]...)
	return out
}

// sanitizeCodexRoutingArgs removes routing settings owned by the sr codex
// launcher before appending its authoritative provider block. This prevents a
// shell shim, saved resume command, or copied older invocation from placing a
// stale Subrouter URL after the current one and silently winning by argument
// order. Direct Codex remains available by invoking codex without sr.
func sanitizeCodexRoutingArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(append(out, arg), args[i+1:]...)
		}
		switch {
		case arg == "--oss":
			continue
		case arg == "--local-provider":
			if i+1 < len(args) {
				i++
			}
			continue
		case strings.HasPrefix(arg, "--local-provider="):
			continue
		case arg == "-c" || arg == "--config":
			if i+1 < len(args) && isSubrouterOwnedCodexConfig(args[i+1]) {
				i++
				continue
			}
		case strings.HasPrefix(arg, "-c") && len(arg) > len("-c"):
			assignment := strings.TrimPrefix(strings.TrimPrefix(arg, "-c"), "=")
			if isSubrouterOwnedCodexConfig(assignment) {
				continue
			}
		case strings.HasPrefix(arg, "--config="):
			if isSubrouterOwnedCodexConfig(strings.TrimPrefix(arg, "--config=")) {
				continue
			}
		}
		out = append(out, arg)
	}
	return out
}

func isSubrouterOwnedCodexConfig(assignment string) bool {
	key, value, ok := strings.Cut(strings.TrimSpace(assignment), "=")
	if !ok {
		return false
	}
	// Codex override paths are split on literal dots; quoted segments are not
	// unquoted as TOML keys. For example, model_providers."subrouter" creates
	// a different path and cannot replace model_providers.subrouter.
	key = strings.TrimSpace(key)
	if key == "model_providers" && inlineTableOwnsSubrouter(value) {
		return true
	}
	switch key {
	case "model_provider",
		"openai_base_url",
		"chatgpt_base_url",
		"experimental_realtime_ws_base_url",
		"model_providers.subrouter":
		return true
	default:
		return strings.HasPrefix(key, "model_providers.subrouter.")
	}
}

func inlineTableOwnsSubrouter(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value[0] != '{' {
		return false
	}
	depth := 0
	segmentStart := -1
	var quote byte
	escaped := false
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if quote == '"' && ch == '\\' {
				escaped = true
				continue
			}
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case '{', '[':
			depth++
			if depth == 1 {
				segmentStart = i + 1
			}
		case '}', ']':
			depth--
		case ',':
			if depth == 1 {
				segmentStart = i + 1
			}
		case '=':
			if depth == 1 && segmentStart >= 0 {
				candidate := strings.TrimSpace(value[segmentStart:i])
				candidate = strings.Trim(candidate, `"'`)
				if candidate == "subrouter" || strings.HasPrefix(candidate, "subrouter.") {
					return true
				}
			}
		}
	}
	return false
}

func codexConfigArgs(
	baseURL string,
	userEmail string,
	accountID string,
	model string,
	forceAuthenticatedProvider bool,
) []string {
	authConfig := `model_providers.subrouter.experimental_bearer_token="subrouter"`
	if forceAuthenticatedProvider {
		authConfig = `model_providers.subrouter.env_key="SUBROUTER_CODEX_DUMMY_API_KEY"`
	}
	return []string{
		"-c", `model_provider="subrouter"`,
		"-c", `model_providers.subrouter.name="Subrouter"`,
		"-c", "model_providers.subrouter.base_url=" + strconv.Quote(baseURL),
		"-c", authConfig,
		"-c", `model_providers.subrouter.wire_api="responses"`,
		"-c", `model_providers.subrouter.supports_websockets=true`,
		"-c", `model_providers.subrouter.http_headers=` + codexSubrouterHeaders(userEmail, accountID, model),
		// A final whole-table override removes unknown leaves inherited through a
		// parent model_providers table; leaf overrides alone do not replace them.
		"-c", `model_providers.subrouter=` + codexSubrouterProviderTable(baseURL, userEmail, accountID, model, forceAuthenticatedProvider),
	}
}

func codexSubrouterProviderTable(baseURL, userEmail, accountID, model string, forceAuthenticatedProvider bool) string {
	auth := `experimental_bearer_token="subrouter"`
	if forceAuthenticatedProvider {
		auth = `env_key="SUBROUTER_CODEX_DUMMY_API_KEY"`
	}
	return `{name="Subrouter",base_url=` + strconv.Quote(baseURL) + `,` + auth + `,wire_api="responses",supports_websockets=true,http_headers=` + codexSubrouterHeaders(userEmail, accountID, model) + `}`
}

func codexSubrouterHeaders(userEmail, accountID, model string) string {
	headers := []string{`"X-Subrouter-Agent"="codex"`}
	if userEmail != "" {
		headers = append(headers, `"X-Subrouter-User-Email"=`+strconv.Quote(userEmail))
	}
	if accountID != "" {
		headers = append(headers, `"X-Subrouter-Account-ID"=`+strconv.Quote(accountID))
	}
	if model != "" {
		headers = append(headers, `"X-Subrouter-Model"=`+strconv.Quote(model))
	}
	return "{" + strings.Join(headers, ",") + "}"
}

func codexModelArg(args []string) string {
	model := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		switch {
		case arg == "-m" || arg == "--model":
			if i+1 < len(args) {
				model = session.NormalizeModel(args[i+1])
				i++
			}
		case strings.HasPrefix(arg, "--model="):
			model = session.NormalizeModel(strings.TrimPrefix(arg, "--model="))
		case strings.HasPrefix(arg, "-m="):
			model = session.NormalizeModel(strings.TrimPrefix(arg, "-m="))
		}
	}
	return model
}

func isSubrouterRoutedCodexCommand(command string) bool {
	switch command {
	case "exec", "e", "review", "resume", "fork", "app-server":
		return true
	default:
		return false
	}
}

func codexInvocationUsesSubrouter(args []string) bool {
	if codexUtilityFlagBeforeTerminator(args) {
		return false
	}
	command, commandIndex := codexSubcommandAt(args)
	if command == "" {
		return true
	}
	if command != "remote-control" {
		return isSubrouterRoutedCodexCommand(command)
	}
	// Only the app-server-creating operation sends model traffic. The other
	// remote-control operations manage that local process and must remain usable
	// even when the selected Subrouter server is unavailable.
	for i := commandIndex + 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if arg == "-c" || arg == "--config" || arg == "--enable" || arg == "--disable" {
			i++
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			return arg == "start"
		}
	}
	return false
}

func codexUtilityFlagBeforeTerminator(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		switch arg {
		case "--help", "-h", "--version", "-V":
			return true
		}
	}
	return false
}

// codexSubcommand finds a documented Codex subcommand after global options.
// The first non-option positional argument is otherwise the interactive
// prompt, so scanning must stop there instead of searching arbitrary prompt
// words for a command name.
func codexSubcommand(args []string) string {
	command, _ := codexSubcommandAt(args)
	return command
}

func codexSubcommandAt(args []string) (string, int) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return "", -1
		}
		if strings.HasPrefix(arg, "-") {
			if isCodexImageOption(arg) {
				if arg == "-i" || arg == "--image" {
					i++
					for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
						i++
					}
				}
				continue
			}
			if codexGlobalOptionTakesValue(arg) && !strings.Contains(arg, "=") {
				i++
			}
			continue
		}
		if isKnownCodexCommand(arg) {
			return arg, i
		}
		return "", -1
	}
	return "", -1
}

func isCodexImageOption(arg string) bool {
	return arg == "-i" || arg == "--image" ||
		strings.HasPrefix(arg, "-i=") || strings.HasPrefix(arg, "--image=") ||
		(strings.HasPrefix(arg, "-i") && len(arg) > len("-i"))
}

func codexGlobalOptionTakesValue(arg string) bool {
	switch arg {
	case "-c", "--config",
		"--enable", "--disable",
		"--remote", "--remote-auth-token-env",
		"-m", "--model",
		"--local-provider",
		"-p", "--profile",
		"-s", "--sandbox",
		"-C", "--cd",
		"--add-dir",
		"-a", "--ask-for-approval":
		return true
	default:
		return false
	}
}

func isKnownCodexCommand(command string) bool {
	switch command {
	case "agents", "exec", "e", "review", "login", "logout", "mcp", "plugin",
		"mcp-server", "app-server", "remote-control", "app", "completion", "update",
		"doctor", "sandbox", "debug", "apply", "a", "resume", "queue", "archive",
		"delete", "migrate-rollouts", "unarchive", "fork", "cloud", "exec-server",
		"features", "help":
		return true
	default:
		return false
	}
}

func envOrDefault(key string, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			out := append([]string(nil), env...)
			out[i] = prefix + value
			return out
		}
	}
	return append(env, prefix+value)
}

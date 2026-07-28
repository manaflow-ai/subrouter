package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

const defaultCodexBaseURL = "http://127.0.0.1:31415/v1"

func codex(args []string) error {
	baseURL, err := codexBaseURLWithFallback(defaultSRServerStore(accounts.DefaultCodexStore()), os.Stderr)
	if err != nil {
		return err
	}
	bin := envOrDefault("SUBROUTER_CODEX_BIN", "codex")
	userEmailRaw := os.Getenv("SUBROUTER_CODEX_USER_EMAIL")
	accountID := session.NormalizeAccountID(os.Getenv("SUBROUTER_CODEX_ACCOUNT_ID"))
	userEmail := ""
	if strings.TrimSpace(userEmailRaw) != "" {
		userEmail = session.NormalizeUserEmail(userEmailRaw)
		if userEmail == "" {
			return fmt.Errorf("SUBROUTER_CODEX_USER_EMAIL must be a valid email address; use SUBROUTER_CODEX_ACCOUNT_ID to force an account such as team-codex-1")
		}
	}

	cmd := exec.CommandContext(context.Background(), bin, codexArgs(args, baseURL, userEmail, accountID)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	env := os.Environ()
	if userEmail != "" || accountID != "" || codexModelArg(args) != "" {
		env = upsertEnv(env, "SUBROUTER_CODEX_DUMMY_API_KEY", "subrouter")
	}
	cmd.Env = env
	return cmd.Run()
}

func codexBaseURL(store srServerStore) (string, error) {
	if baseURL := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_BASE_URL")); baseURL != "" {
		return baseURL, nil
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
	return codexBaseURLForServer(server), nil
}

// codexBaseURLWithFallback resolves the base URL for launching codex, then
// substitutes the local daemon when the configured server is unreachable. An
// explicit SUBROUTER_CODEX_BASE_URL or SUBROUTER_CODEX_SERVER is treated as a
// deliberate pin and is never overridden.
func codexBaseURLWithFallback(store srServerStore, warn io.Writer) (string, error) {
	baseURL, err := codexBaseURL(store)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_BASE_URL")) != "" ||
		strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_SERVER")) != "" {
		// A pin is never substituted, but if it points at this machine's daemon
		// we still start it rather than failing against a dead socket.
		local := localBaseURL()
		if sameEndpoint(baseURL, local) {
			ensureLocalHealthy(context.Background(), fallbackHTTPClient(), local, defaultDaemonStarter(), warn)
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
	return codexBaseURLForServer(server), nil
}

func codexBaseURLForServer(server srServerConfig) string {
	baseURL := strings.TrimRight(server.URL, "/")
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL
	}
	return baseURL + "/v1"
}

func codexArgs(args []string, baseURL, userEmail, accountID string) []string {
	model := codexModelArg(args)
	configArgs := codexConfigArgs(baseURL, userEmail, accountID, model)
	if len(args) == 0 || strings.HasPrefix(args[0], "-") || !isKnownCodexCommand(args[0]) {
		return append(configArgs, args...)
	}
	if !isSubrouterRoutedCodexCommand(args[0]) {
		return append([]string(nil), args...)
	}
	out := []string{args[0]}
	out = append(out, configArgs...)
	out = append(out, args[1:]...)
	return out
}

func codexConfigArgs(baseURL, userEmail, accountID, model string) []string {
	if userEmail == "" && accountID == "" && model == "" {
		return []string{"-c", "openai_base_url=" + strconv.Quote(baseURL)}
	}
	return []string{
		"-c", `model_provider="subrouter"`,
		"-c", `model_providers.subrouter.name="Subrouter"`,
		"-c", "model_providers.subrouter.base_url=" + strconv.Quote(baseURL),
		"-c", `model_providers.subrouter.env_key="SUBROUTER_CODEX_DUMMY_API_KEY"`,
		"-c", `model_providers.subrouter.wire_api="responses"`,
		"-c", `model_providers.subrouter.supports_websockets=true`,
		"-c", `model_providers.subrouter.http_headers=` + codexSubrouterHeaders(userEmail, accountID, model),
	}
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
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-m" || arg == "--model":
			if i+1 < len(args) {
				return session.NormalizeModel(args[i+1])
			}
		case strings.HasPrefix(arg, "--model="):
			return session.NormalizeModel(strings.TrimPrefix(arg, "--model="))
		case strings.HasPrefix(arg, "-m="):
			return session.NormalizeModel(strings.TrimPrefix(arg, "-m="))
		}
	}
	return ""
}

func isSubrouterRoutedCodexCommand(command string) bool {
	switch command {
	case "exec", "e", "review", "resume", "fork", "app-server":
		return true
	default:
		return false
	}
}

func isKnownCodexCommand(command string) bool {
	switch command {
	case "exec", "e", "review", "login", "logout", "mcp", "plugin", "mcp-server", "app-server", "app", "completion", "sandbox", "debug", "apply", "a", "resume", "fork", "cloud", "exec-server", "features", "help":
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

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentqwen "github.com/manaflow-ai/subrouter/internal/agents/qwen"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

func (r srRunner) qwen(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(r.out, `Qwen Token Plan accounts use the standard provider account lifecycle:
  sr add-key --provider qwen-token
                           Add a Token Plan API-key account
  sr status                List accounts and show available plan/quota status
  sr remove <account>      Remove a Token Plan account

Qwen console commands attach plan and quota metadata to an existing account:
  sr qwen login [--console-account <email-or-label>] <account>
                           Authorize Alibaba console plan/quota metadata
  sr qwen label <account> <email-or-label>
                           Set the saved sign-in label shown by sr status
`)
		return nil
	}
	if args[0] != "login" {
		if args[0] == "label" && len(args) == 3 {
			return r.qwenLabel(ctx, args[1], args[2])
		}
		return fmt.Errorf("usage: sr qwen login [--console-account <email-or-label>] <qwen-token-account>")
	}
	selector, consoleAccount, err := parseQwenLoginArgs(args[1:], r.errOut)
	if err != nil {
		return err
	}
	return r.qwenLogin(ctx, selector, consoleAccount)
}

func parseQwenLoginArgs(args []string, errOut io.Writer) (string, string, error) {
	flags := flag.NewFlagSet("qwen login", flag.ContinueOnError)
	flags.SetOutput(errOut)
	consoleAccount := flags.String("console-account", "", "Alibaba sign-in email or safe label shown by sr status")
	if err := flags.Parse(args); err != nil {
		return "", "", err
	}
	if flags.NArg() != 1 {
		return "", "", fmt.Errorf("usage: sr qwen login [--console-account <email-or-label>] <qwen-token-account>")
	}
	return flags.Arg(0), *consoleAccount, nil
}

func (r srRunner) qwenLabel(ctx context.Context, selector, label string) error {
	stored, ok, err := r.store.FindStored(selector)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no account found matching %q", selector)
	}
	if !stored.IsAPIKey() || stored.ProviderOrDefault() != accounts.ProviderQwenToken {
		return fmt.Errorf("%s is not a Qwen Token Plan API-key account", stored.Email)
	}
	root := agentqwen.ConsoleRootForStore(r.store)
	if err := agentqwen.SetConsoleAccountIn(root, stored.Email, label); err != nil {
		return err
	}
	if err := r.syncQwenConsoleToSelectedRemoteIn(ctx, root, stored.Email); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Qwen console label for %s: %s\n", displayUsageAccountName(srUsageRow{email: stored.Email, provider: stored.ProviderOrDefault(), authMode: accounts.AuthModeAPIKey}), strings.TrimSpace(label))
	return nil
}

func (r srRunner) qwenLogin(ctx context.Context, selector, consoleAccount string) error {
	stored, ok, err := r.store.FindStored(selector)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no account found matching %q", selector)
	}
	if !stored.IsAPIKey() || stored.ProviderOrDefault() != accounts.ProviderQwenToken {
		return fmt.Errorf("%s is not a Qwen Token Plan API-key account", stored.Email)
	}
	root := agentqwen.ConsoleRootForStore(r.store)
	return r.qwenLoginStored(ctx, root, stored, consoleAccount, func(ctx context.Context, accountID string) error {
		return r.syncQwenConsoleToSelectedRemoteIn(ctx, root, accountID)
	})
}

func (r srRunner) qwenRemote(ctx context.Context, server srServerConfig, args []string) error {
	if len(args) == 3 && args[0] == "label" {
		remoteAccounts, err := r.fetchServerAccounts(ctx, server)
		if err != nil {
			return err
		}
		accountID, err := matchRemoteQwenAccount(remoteAccounts, args[1])
		if err != nil {
			return err
		}
		root := qwenRemoteConsoleRoot(server)
		if err := agentqwen.SetConsoleAccountIn(root, accountID, args[2]); err != nil {
			return err
		}
		return r.syncQwenConsoleToServer(ctx, root, server, accountID)
	}
	if len(args) == 0 || args[0] != "login" {
		return fmt.Errorf("remote Qwen setup supports: sr qwen login [--console-account <email-or-label>] <qwen-token-account>")
	}
	selector, consoleAccount, err := parseQwenLoginArgs(args[1:], r.errOut)
	if err != nil {
		return err
	}
	remoteAccounts, err := r.fetchServerAccounts(ctx, server)
	if err != nil {
		return err
	}
	accountID, err := matchRemoteQwenAccount(remoteAccounts, selector)
	if err != nil {
		return err
	}
	usageStatuses, available, err := r.fetchServerUsageStatuses(ctx, server)
	if err != nil {
		return err
	}
	if !available {
		return fmt.Errorf("remote server cannot expose the selected Qwen key fingerprint; update Subrouter on %s before authorizing quota", server.Name)
	}
	expectedFingerprint, err := remoteQwenKeyFingerprint(usageStatuses, accountID)
	if err != nil {
		return err
	}
	reader := bufio.NewReader(r.in)
	key, err := promptSecret(r.out, reader, r.in, "Qwen Token Plan API key for this remote account (used only for authorization): ")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(strings.TrimSpace(key), "sk-sp-") {
		return fmt.Errorf("Qwen Token Plan API key must start with sk-sp-")
	}
	if actual := accounts.APIKeyFingerprint(key); actual != expectedFingerprint {
		return fmt.Errorf("Qwen Token Plan API key does not match remote account %s", accountID)
	}
	stored := accounts.StoredCodexAccount{
		Email:    accountID,
		Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: strings.TrimSpace(key),
		},
	}
	root := qwenRemoteConsoleRoot(server)
	return r.qwenLoginStored(ctx, root, stored, consoleAccount, func(ctx context.Context, accountID string) error {
		return r.syncQwenConsoleToServer(ctx, root, server, accountID)
	})
}

func remoteQwenKeyFingerprint(statuses []remoteServerUsageStatus, accountID string) (string, error) {
	for _, status := range statuses {
		if status.ID != accountID {
			continue
		}
		fingerprint := strings.TrimSpace(status.KeyFingerprint)
		if fingerprint == "" {
			return "", fmt.Errorf("remote Qwen account %s does not expose a key fingerprint; update the remote Subrouter before authorizing quota", accountID)
		}
		return fingerprint, nil
	}
	return "", fmt.Errorf("remote Qwen account %s is missing from usage status", accountID)
}

func matchRemoteQwenAccount(all []remoteServerAccount, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	matches := make([]string, 0, 1)
	for _, item := range all {
		if item.Provider != accounts.ProviderQwenToken || item.AuthMode != accounts.AuthModeAPIKey {
			continue
		}
		if strings.EqualFold(item.ID, selector) || strings.EqualFold(item.Email, selector) || strings.EqualFold(strings.TrimPrefix(item.ID, "qwen-token:"), selector) {
			matches = append(matches, item.ID)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no remote Qwen Token Plan account found matching %q", selector)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("multiple remote Qwen Token Plan accounts match %q: %s", selector, strings.Join(matches, ", "))
	}
	return matches[0], nil
}

func (r srRunner) cloudQwen(ctx context.Context, args []string) error {
	if len(args) == 3 && args[0] == "label" {
		config, _, client, err := loadCloudClient(true)
		if err != nil {
			return err
		}
		if !config.HostedTenantReady() {
			return fmt.Errorf("Qwen quota authorization requires a hosted tenant endpoint; run 'sr login' to refresh this machine's team configuration")
		}
		shared, err := client.ListAccounts(ctx)
		if err != nil {
			return err
		}
		accountID, err := matchSharedQwenAccount(shared, args[1])
		if err != nil {
			return err
		}
		root := qwenHostedConsoleRoot(config)
		if err := agentqwen.SetConsoleAccountIn(root, accountID, args[2]); err != nil {
			return err
		}
		credential, err := agentqwen.ExportConsoleCredentialIn(root, accountID)
		if err != nil {
			return err
		}
		return client.UploadQwenConsoleCredential(ctx, accountID, credential)
	}
	if len(args) == 0 || args[0] != "login" {
		return fmt.Errorf("hosted Qwen setup supports: sr qwen login [--console-account <email-or-label>] <qwen-token-account>")
	}
	selector, consoleAccount, err := parseQwenLoginArgs(args[1:], r.errOut)
	if err != nil {
		return err
	}
	config, _, client, err := loadCloudClient(true)
	if err != nil {
		return err
	}
	if !config.HostedTenantReady() {
		return fmt.Errorf("Qwen quota authorization requires a hosted tenant endpoint; run 'sr login' to refresh this machine's team configuration")
	}
	shared, err := client.ListAccounts(ctx)
	if err != nil {
		return err
	}
	accountID, err := matchSharedQwenAccount(shared, selector)
	if err != nil {
		return err
	}
	lease, err := client.Lease(ctx, broker.LeaseRequest{
		Provider:         accounts.ProviderQwenToken,
		RequiredAuthMode: accounts.AuthModeAPIKey,
		AgentType:        "qwen-console-setup",
		SessionID:        "qwen-console:" + accountID,
		PreferAccountID:  accountID,
	})
	if err != nil {
		return fmt.Errorf("lease Qwen Token Plan key for console authorization: %w", err)
	}
	if lease.Account.ID != accountID {
		return fmt.Errorf("requested Qwen Token Plan account %q but the hosted pool leased %q", accountID, lease.Account.ID)
	}
	stored := accounts.StoredCodexAccount{
		Email:    accountID,
		Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: lease.Account.Token,
		},
	}
	root := qwenHostedConsoleRoot(config)
	return r.qwenLoginStored(ctx, root, stored, consoleAccount, func(ctx context.Context, accountID string) error {
		credential, err := agentqwen.ExportConsoleCredentialIn(root, accountID)
		if err != nil {
			return err
		}
		if err := client.UploadQwenConsoleCredential(ctx, accountID, credential); err != nil {
			return err
		}
		fmt.Fprintln(r.out, "Synced Qwen quota authorization to the selected hosted account pool.")
		return nil
	})
}

func qwenRemoteConsoleRoot(server srServerConfig) string {
	return agentqwen.ConsoleRootForScope("remote\x00" + server.Name + "\x00" + server.URL + "\x00" + server.TenantKey)
}

func qwenHostedConsoleRoot(config broker.Config) string {
	return agentqwen.ConsoleRootForScope("hosted\x00" + config.HostedURL + "\x00" + config.TeamID + "\x00" + config.TenantKey)
}

func matchSharedQwenAccount(all []broker.SharedAccount, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	matches := make([]string, 0, 1)
	for _, item := range all {
		if item.Kind != string(accounts.ProviderQwenToken) {
			continue
		}
		if strings.EqualFold(item.ID, selector) || strings.EqualFold(item.Label, selector) || strings.EqualFold(strings.TrimPrefix(item.ID, "qwen-token:"), selector) {
			matches = append(matches, item.ID)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no hosted Qwen Token Plan account found matching %q", selector)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("multiple hosted Qwen Token Plan accounts match %q: %s", selector, strings.Join(matches, ", "))
	}
	return matches[0], nil
}

func (r srRunner) qwenLoginStored(ctx context.Context, root string, stored accounts.StoredCodexAccount, consoleAccount string, syncCredential func(context.Context, string) error) error {
	consoleAccount = strings.TrimSpace(consoleAccount)
	if consoleAccount == "" {
		// The recovery command printed for an expired console session intentionally
		// needs only the account selector. Keep the operator-supplied identity from
		// the previous authorization instead of replacing it with an empty label.
		consoleAccount = agentqwen.ConsoleAccountIn(root, stored.Email)
	}
	stageRoot, err := os.MkdirTemp("", "subrouter-qwen-login-")
	if err != nil {
		return fmt.Errorf("prepare temporary Qwen login profile: %w", err)
	}
	stageRemoved := false
	preserveStage := false
	defer func() {
		if !stageRemoved && !preserveStage {
			_ = os.RemoveAll(stageRoot)
		}
	}()
	if err := agentqwen.PrepareConsoleLoginIn(stageRoot, stored.Email, stored.Auth.OpenAIAPIKey, proxy.ProviderDefaultUpstream(accounts.ProviderQwenToken)); err != nil {
		return err
	}

	fmt.Fprintf(r.out, "Opening Alibaba authorization for %s. Approve it in the browser; Subrouter will keep the resulting console credential isolated to this account.\n", displayUsageAccountName(srUsageRow{email: stored.Email, provider: stored.ProviderOrDefault(), authMode: accounts.AuthModeAPIKey}))
	env := []string{"BAILIAN_CONFIG_DIR=" + agentqwen.ConsoleConfigDirIn(stageRoot, stored.Email)}
	browserCleanup := func() {}
	if browserEnv, cleanup, browserErr := qwenBrowserEnv(); browserErr != nil {
		return browserErr
	} else if browserEnv != "" {
		env = append(env, browserEnv)
		browserCleanup = cleanup
	}
	defer browserCleanup()
	err = r.commandRunner().RunWithEnv(ctx, "bl", []string{"auth", "login", "--console", "--console-site", "international"}, env, r.in, r.out, r.errOut)
	if err != nil {
		baseErr := fmt.Errorf("Alibaba console authorization failed: %w", err)
		if errors.Is(err, exec.ErrNotFound) || strings.Contains(err.Error(), "executable file not found") {
			baseErr = fmt.Errorf("Bailian CLI is required; install it with: npm install -g bailian-cli")
		}
		return removeQwenLoginStage(stageRoot, &stageRemoved, baseErr)
	}
	if err := agentqwen.FinishConsoleLoginIn(stageRoot, stored.Email); err != nil {
		return removeQwenLoginStage(stageRoot, &stageRemoved, err)
	}
	credential, err := agentqwen.ExportConsoleCredentialIn(stageRoot, stored.Email)
	if err != nil {
		return removeQwenLoginStage(stageRoot, &stageRemoved, err)
	}
	credential.Account = consoleAccount
	if err := agentqwen.SaveConsoleCredentialIn(stageRoot, stored.Email, credential); err != nil {
		return removeQwenLoginStage(stageRoot, &stageRemoved, fmt.Errorf("preserve completed Alibaba authorization: %w", err))
	}
	if err := agentqwen.SaveConsoleCredentialIn(root, stored.Email, credential); err != nil {
		// Keep the completed browser authorization recoverable when the durable
		// destination is temporarily unwritable. It contains a credential, so the
		// error names the private staging directory explicitly for recovery.
		preserveStage = true
		return fmt.Errorf("save Alibaba console credential (authorization preserved at %s): %w", stageRoot, err)
	}
	if err := removeQwenLoginStage(stageRoot, &stageRemoved, nil); err != nil {
		return err
	}
	if syncCredential != nil {
		if err := syncCredential(ctx, stored.Email); err != nil {
			return err
		}
	}
	fmt.Fprintf(r.out, "Authorized Qwen quota status for %s. Run: sr status\n", displayUsageAccountName(srUsageRow{email: stored.Email, provider: stored.ProviderOrDefault(), authMode: accounts.AuthModeAPIKey}))
	return nil
}

func removeQwenLoginStage(path string, removed *bool, primary error) error {
	err := os.RemoveAll(path)
	if err == nil {
		*removed = true
		return primary
	}
	cleanupErr := fmt.Errorf("remove temporary Qwen login profile: %w", err)
	if primary != nil {
		return errors.Join(primary, cleanupErr)
	}
	return cleanupErr
}

func (r srRunner) syncQwenConsoleToSelectedRemote(ctx context.Context, accountID string) error {
	return r.syncQwenConsoleToSelectedRemoteIn(ctx, agentqwen.DefaultConsoleRoot(), accountID)
}

func (r srRunner) syncQwenConsoleToSelectedRemoteIn(ctx context.Context, root, accountID string) error {
	explicitRemote := strings.TrimSpace(os.Getenv("SUBROUTER_SERVER")) != "" || strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_SERVER")) != ""
	if !explicitRemote {
		config, err := cloudModeConfig()
		if err != nil {
			return err
		}
		if config.EffectiveCredentialSource() != broker.CredentialSourceLegacy {
			return nil
		}
	}
	server, ok, err := r.selectedRemoteServer()
	if err != nil || !ok {
		return err
	}
	return r.syncQwenConsoleToServer(ctx, root, server, accountID)
}

func (r srRunner) syncQwenConsoleToServer(ctx context.Context, root string, server srServerConfig, accountID string) error {
	credential, err := agentqwen.ExportConsoleCredentialIn(root, accountID)
	if err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		AccountID  string                      `json:"account_id"`
		Credential agentqwen.ConsoleCredential `json:"credential"`
	}{AccountID: accountID, Credential: credential})
	if err != nil {
		return err
	}
	baseURL, err := protectedServerControlBaseURL(server)
	if err != nil {
		return err
	}
	endpoint := baseURL + "/_subrouter/qwen-console"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return redactServerRequestError(err, server)
	}
	req.Header.Set("Content-Type", "application/json")
	addServerAccountImportAuth(req, server)
	client := r.client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	securedClient, err := securedServerRequestClient(client, endpoint)
	if err != nil {
		return fmt.Errorf("sync Qwen console credential to server %s: %w", server.Name, err)
	}
	res, err := securedClient.Do(req)
	if err != nil {
		return fmt.Errorf("sync Qwen console credential to server %s: %w", server.Name, redactServerRequestError(err, server))
	}
	defer res.Body.Close()
	_, _ = io.CopyN(io.Discard, res.Body, 4096)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
			return fmt.Errorf("sync Qwen console credential to server %s: %s; server account-import authentication is missing or invalid (run 'sr server install %s' or configure its account-import token)", server.Name, res.Status, server.Name)
		}
		return fmt.Errorf("sync Qwen console credential to server %s: %s", server.Name, res.Status)
	}
	fmt.Fprintf(r.out, "Synced Qwen quota authorization to server: %s\n", server.Name)
	return nil
}

// Bailian CLI hardcodes macOS's `open` command. Prefer Chrome when installed:
// Safari did not reliably return its console callback to the CLI's localhost
// listener during live Token Plan setup, while Chrome did.
func qwenBrowserEnv() (string, func(), error) {
	if runtime.GOOS != "darwin" {
		return "", func() {}, nil
	}
	openPath, err := exec.LookPath("open")
	if err != nil {
		return "", func() {}, nil
	}
	bundleID := ""
	for _, candidate := range []string{"com.google.Chrome", "com.google.Chrome.beta", "com.google.Chrome.canary"} {
		if exec.Command(openPath, "-Ra", "-b", candidate).Run() == nil {
			bundleID = candidate
			break
		}
	}
	if bundleID == "" {
		return "", func() {}, nil
	}
	dir, err := os.MkdirTemp("", "subrouter-qwen-browser-")
	if err != nil {
		return "", func() {}, fmt.Errorf("prepare Qwen browser launcher: %w", err)
	}
	launcher := filepath.Join(dir, "open")
	contents := []byte("#!/bin/sh\nexec " + strconv.Quote(openPath) + " -b " + strconv.Quote(bundleID) + " -- \"$@\"\n")
	if err := os.WriteFile(launcher, contents, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", func() {}, fmt.Errorf("prepare Qwen browser launcher: %w", err)
	}
	return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), func() { _ = os.RemoveAll(dir) }, nil
}

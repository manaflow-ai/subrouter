package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentqwen "github.com/manaflow-ai/subrouter/internal/agents/qwen"
	"github.com/manaflow-ai/subrouter/internal/broker"
)

func (r srRunner) qwen(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "subscription" {
		return r.qwenSubscription(ctx, args[1:], nil)
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(r.out, `Qwen.ai Plus/Pro: sr qwen subscription login <label>

Qwen accounts use one routed pool with separate backend credentials:
  sr add-key --provider qwen --backend token-plan
                           Add a Token Plan API-key account
                           Alias: sr add-key --provider qwen-token
  sr add-key --provider qwen --backend coding-plan
                           Add a Coding Plan API-key account
  sr status                List accounts and show available plan/quota status
  sr remove <account>      Remove a Token Plan account

Qwen console commands attach plan and quota metadata to an existing account:
  sr qwen login [--console-account <email-or-label>] <account>
                           Authorize Alibaba console plan/quota metadata
  sr qwen label <account> <email-or-label>
                           Set the saved sign-in label shown by sr status
  sr qwen [--account [account]] [-- qwen args...]
                           Launch Qwen Code through the selected Qwen pool
                           Omit --account for failover; a pin has no account failover
  sr qwen proxy [qwen args...]
                           Backward-compatible explicit launcher alias
                           (plain qwen remains direct)
  Qwen serve/ACP, review, model-bearing channel-service, and container sandbox modes intentionally remain direct.
`)
		return nil
	}
	if args[0] == "proxy" {
		return r.launchQwenProxy(ctx, args[1:])
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
	positional, err := parseFlagsAnywhere(flags, args)
	if err != nil {
		return "", "", err
	}
	if len(positional) != 1 {
		return "", "", fmt.Errorf("usage: sr qwen login [--console-account <email-or-label>] <qwen-token-account>")
	}
	return positional[0], *consoleAccount, nil
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
	if len(args) > 0 && args[0] == "subscription" {
		return r.qwenSubscription(ctx, args[1:], &server)
	}
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
	stored := accounts.StoredCodexAccount{Email: accountID, Provider: accounts.ProviderQwenToken}
	root := qwenRemoteConsoleRoot(server)
	return r.qwenLoginStored(ctx, root, stored, consoleAccount, func(ctx context.Context, accountID string) error {
		return r.syncQwenConsoleToServer(ctx, root, server, accountID)
	})
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
	if len(args) > 0 && args[0] == "subscription" {
		config, _, _, err := loadCloudClient(true)
		if err != nil {
			return err
		}
		if !config.HostedTenantReady() {
			return fmt.Errorf("Qwen subscriptions require a hosted tenant endpoint; run 'sr login' to refresh this machine's team configuration")
		}
		server := srServerConfig{Name: "cmux", URL: strings.TrimRight(config.HostedURL, "/"), TenantKey: config.TenantKey}
		return r.qwenSubscription(ctx, args[1:], &server)
	}
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
	stored := accounts.StoredCodexAccount{Email: accountID, Provider: accounts.ProviderQwenToken}
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
	authorize := r.qwenConsoleAuthorize
	if authorize == nil {
		authorize = func(ctx context.Context, out io.Writer) (agentqwen.ConsoleCredential, error) {
			return agentqwen.AuthorizeConsole(ctx, out, openBrowser)
		}
	}
	credential, err := authorize(ctx, r.out)
	if err != nil {
		return removeQwenLoginStage(stageRoot, &stageRemoved, fmt.Errorf("Alibaba console authorization failed: %w", err))
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
	explicitRemote := explicitServerTarget() != ""
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
	securedClient, err := r.securedRequestClientForServer(server, endpoint, 15*time.Second)
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
		return fmt.Errorf("sync Qwen console credential to server %s: %s", server.Name, res.Status)
	}
	fmt.Fprintf(r.out, "Synced Qwen quota authorization to server: %s\n", server.Name)
	return nil
}

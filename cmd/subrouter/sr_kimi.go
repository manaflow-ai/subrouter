package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	baseaccount "github.com/manaflow-ai/subrouter/account"
	agentkimi "github.com/manaflow-ai/subrouter/internal/agents/kimi"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

func (r srRunner) kimiCommand(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(r.out, "Usage: sr kimi login <label>")
		fmt.Fprintln(r.out, "       sr kimi list")
		fmt.Fprintln(r.out, "       sr kimi remove <label>")
		fmt.Fprintln(r.out, "       sr kimi [--account [account]] [-- kimi args...]")
		fmt.Fprintln(r.out, "       sr kimi proxy [--account [account]] [-- kimi args...]  (explicit alias)")
		fmt.Fprintln(r.out, "Omit --account for pooled failover; a pinned account has no account failover.")
		fmt.Fprintln(r.out, "Plain 'kimi' remains direct.")
		return nil
	}
	switch args[0] {
	case "login", "add":
		label := ""
		if len(args) > 1 {
			label = args[1]
		}
		if len(args) > 2 {
			return fmt.Errorf("usage: sr kimi login <label>")
		}
		return r.kimiLogin(ctx, label)
	case "list", "ls":
		if len(args) != 1 {
			return fmt.Errorf("usage: sr kimi list")
		}
		listed, err := agentkimi.DefaultStore().ListAccounts(ctx)
		if err != nil {
			warningOut := r.errOut
			if warningOut == nil {
				warningOut = r.out
			}
			if warningOut == nil {
				warningOut = io.Discard
			}
			fmt.Fprintf(warningOut, "Warning: some Kimi credentials are unavailable: %v\n", err)
		}
		if len(listed) == 0 {
			fmt.Fprintln(r.out, "No Kimi subscription accounts configured.")
			return nil
		}
		for _, acct := range listed {
			kind := "managed"
			if acct.ID == "kimi-code" {
				kind = "Kimi CLI; not routed"
			}
			fmt.Fprintf(r.out, "%s (%s; %s)\n", acct.ID, kind, acct.Label)
		}
		return nil
	case "remove", "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: sr kimi remove <label>")
		}
		var acct baseaccount.Account
		var ok bool
		err := proxy.PublishAccountDiskMutation(ctx, r.store.StoreDir(), func() (bool, error) {
			var removeErr error
			acct, ok, removeErr = agentkimi.DefaultStore().RemoveManagedAccount(args[1])
			return ok, removeErr
		})
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no managed Kimi account found matching %q", args[1])
		}
		fmt.Fprintf(r.out, "Removed Kimi account: %s\n", acct.ID)
		return nil
	default:
		return fmt.Errorf("unknown Kimi command %q; use proxy, login, list, or remove", args[0])
	}
}

func (r srRunner) kimiLogin(ctx context.Context, label string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		reader := bufio.NewReader(r.in)
		var err error
		label, err = promptLine(r.out, reader, "Kimi account label (e.g. work, personal): ")
		if err != nil {
			return err
		}
	}
	kimiStore := agentkimi.DefaultStore()
	credential, err := kimiStore.AuthorizeManaged(ctx, r.client, label, r.out)
	if err != nil {
		return fmt.Errorf("Kimi login failed: %w", err)
	}
	var acct baseaccount.Account
	err = proxy.PublishAccountDiskMutation(ctx, r.store.StoreDir(), func() (bool, error) {
		var saveErr error
		acct, saveErr = kimiStore.SaveManagedCredential(label, credential)
		return saveErr == nil, saveErr
	})
	if err != nil {
		return fmt.Errorf("signed in but could not publish the managed Kimi credential: %w", err)
	}
	// The server discovers the new profile from the same portable state root on
	// its next account refresh; no Kimi CLI profile switch is involved.
	fmt.Fprintf(r.out, "Added isolated Kimi account: %s. Run: sr status\n", acct.ID)
	return nil
}

func (r srRunner) kimiRemote(ctx context.Context, server srServerConfig, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(r.out, "Usage: sr kimi login <label>")
		fmt.Fprintln(r.out, "       sr kimi list")
		fmt.Fprintln(r.out, "       sr kimi remove <label>")
		fmt.Fprintf(r.out, "Managed profiles are stored on server %s.\n", server.Name)
		return nil
	}
	switch args[0] {
	case "login", "add":
		if len(args) != 2 {
			return fmt.Errorf("usage: sr kimi login <label>")
		}
		return r.kimiRemoteLogin(ctx, server, args[1])
	case "list", "ls":
		if len(args) != 1 {
			return fmt.Errorf("usage: sr kimi list")
		}
		all, err := r.fetchServerAccounts(ctx, server)
		if err != nil {
			return err
		}
		found := false
		for _, acct := range all {
			if acct.Provider != baseaccount.ProviderKimi || acct.AuthMode != baseaccount.AuthModeOAuth {
				continue
			}
			found = true
			label := strings.TrimSpace(acct.Label)
			if label == "" {
				label = acct.ID
			}
			fmt.Fprintf(r.out, "%s (%s)\n", label, acct.ID)
		}
		if !found {
			fmt.Fprintln(r.out, "No Kimi subscription accounts configured on the selected server.")
		}
		return nil
	case "remove", "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: sr kimi remove <label>")
		}
		if err := r.removeServerKimiAccount(ctx, server, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "Removed Kimi account %q from server %s.\n", strings.TrimSpace(args[1]), server.Name)
		return nil
	default:
		return fmt.Errorf("unknown Kimi command %q; use login, list, or remove", args[0])
	}
}

func (r srRunner) kimiRemoteLogin(ctx context.Context, server srServerConfig, label string) error {
	if err := r.ensureServerAccountImportProviderAvailable(ctx, server, baseaccount.ProviderKimi); err != nil {
		return err
	}
	stageRoot, err := os.MkdirTemp("", "subrouter-kimi-login-")
	if err != nil {
		return fmt.Errorf("prepare temporary Kimi login profile: %w", err)
	}
	defer func() { _ = os.RemoveAll(stageRoot) }()
	store := agentkimi.Store{
		Path:       filepath.Join(stageRoot, "unused-cli-credential.json"),
		ManagedDir: filepath.Join(stageRoot, "managed"),
	}
	if _, err := store.SignInManaged(ctx, r.client, label, r.out); err != nil {
		return fmt.Errorf("Kimi login failed: %w", err)
	}
	credential, ok, err := store.ReadManagedCredential(label, time.Now())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("Kimi login completed without a stored credential")
	}
	if err := r.uploadServerKimiAccount(ctx, server, label, credential); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Added isolated Kimi account %q to server %s. Run: sr status\n", strings.TrimSpace(label), server.Name)
	return nil
}

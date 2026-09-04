package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	baseaccount "github.com/manaflow-ai/subrouter/account"
	agentantigravity "github.com/manaflow-ai/subrouter/internal/agents/antigravity"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

const antigravityManagementHelp = `Usage: sr agy [--account [label-or-email]] [agy args...]
       sr agy add <label>
       sr agy list
       sr agy recover       Restore a native profile swap left by a crash
       sr agy remove <label>

Bare 'sr agy' launches AGY through Subrouter's pooled Cloud Code route. Use
--account to pin one server account; bare --account opens a picker. Account
selection, quota affinity, refresh, and bounded failover happen server-side.
Use 'sr agy add <label>' to import the current plain 'agy' OAuth login into an
isolated local profile. Repeat after signing plain 'agy' into each account.
The label is an alias for selecting/removing the profile; it does not change or
assert the Google identity in the credential. Status reports each verified
identity, plan, and model-family quota. The local AGY login is used only to
start the CLI; routed requests use the isolated server pool. Use plain 'agy'
for direct un-managed OAuth access.
`

func (r srRunner) antigravityManage(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(r.out, antigravityManagementHelp)
		return nil
	}
	store := &agentantigravity.Store{}
	switch args[0] {
	case "add", "import":
		if len(args) != 2 {
			return fmt.Errorf("usage: sr agy add <label>")
		}
		credential, ok, err := agentantigravity.ReadLocalCredential(ctx, time.Now())
		if err != nil {
			return fmt.Errorf("read current agy login: %w", err)
		}
		if !ok {
			return fmt.Errorf("plain agy is not logged in; sign in with 'agy', then rerun 'sr agy add %s'", strings.TrimSpace(args[1]))
		}
		grant := credential
		credential, err = agentantigravity.PrepareManagedCredential(ctx, r.client, credential, time.Now())
		if err != nil {
			return fmt.Errorf("validate current agy login before import: %w", err)
		}
		var acct baseaccount.Account
		err = proxy.PublishAccountDiskMutation(ctx, r.store.StoreDir(), func() (bool, error) {
			var saveErr error
			acct, saveErr = store.SaveManagedCredentialFromGrant(args[1], credential, grant)
			return saveErr == nil, saveErr
		})
		if err != nil {
			return fmt.Errorf("publish managed Antigravity credential: %w", err)
		}
		fmt.Fprintf(r.out, "Added isolated Antigravity account: %s. Run: sr status\n", acct.ID)
		return nil
	case "list", "ls":
		accounts, err := store.ForServing().ListAccounts(ctx)
		if err != nil {
			return err
		}
		if len(accounts) == 0 {
			fmt.Fprintln(r.out, "No isolated Antigravity accounts configured.")
			return nil
		}
		for _, acct := range accounts {
			fmt.Fprintf(r.out, "%s (%s)\n", acct.Label, acct.ID)
		}
		return nil
	case "recover":
		lockPath := filepath.Join(storepath.StateDir(), "antigravity", "native-keychain.lock")
		if err := agentantigravity.RecoverNativeProfile(ctx, lockPath); err != nil {
			return err
		}
		fmt.Fprintln(r.out, "Native AGY Keychain recovery complete (or no recovery was needed).")
		return nil
	case "remove", "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: sr agy remove <label>")
		}
		var removed bool
		err := proxy.PublishAccountDiskMutation(ctx, r.store.StoreDir(), func() (bool, error) {
			_, ok, removeErr := store.RemoveManagedAccount(args[1])
			removed = ok
			return ok, removeErr
		})
		if err != nil {
			return err
		}
		if !removed {
			return fmt.Errorf("no managed Antigravity account found matching %q", args[1])
		}
		fmt.Fprintf(r.out, "Removed Antigravity account %q.\n", strings.TrimSpace(args[1]))
		return nil
	default:
		return fmt.Errorf("unknown Antigravity command %q; use add, list, or remove", args[0])
	}
}

func (r srRunner) launchAntigravityNative(ctx context.Context, args []string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("native AGY profile pooling requires macOS Keychain; use plain agy on %s", runtime.GOOS)
	}
	selector, picker, vendorArgs, err := parseAntigravityNativeArgs(args)
	if err != nil {
		return err
	}
	store := (&agentantigravity.Store{}).ForServing()
	profiles, err := store.ListAccounts(ctx)
	if err != nil {
		return fmt.Errorf("list local AGY profiles: %w", err)
	}
	managed := make([]baseaccount.Account, 0, len(profiles))
	for _, profile := range profiles {
		if agentantigravity.IsManagedAccountID(profile.ID) {
			managed = append(managed, profile)
		}
	}
	if len(managed) == 0 {
		return fmt.Errorf("no local AGY profiles; sign plain 'agy' into an account, then run 'sr agy add <label>'")
	}
	chosen, err := chooseAntigravityProfile(r.in, r.out, managed, selector, picker)
	if err != nil {
		return err
	}
	credential, ok, err := store.ReadManagedCredential(chosen.Label)
	if err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("profile is missing its credential")
		}
		return fmt.Errorf("read local AGY profile %q: %w", chosen.Label, err)
	}
	if _, err = store.RefreshAccount(ctx, r.client, chosen); err != nil {
		return fmt.Errorf("refresh local AGY profile %q: %w", chosen.Label, err)
	}
	// RefreshAccount returns an account view; read the durable credential again
	// so a rotated refresh token is what gets installed in the native slot.
	credential, ok, err = store.ReadManagedCredential(chosen.Label)
	if err != nil || !ok {
		return fmt.Errorf("read refreshed local AGY profile %q: %w", chosen.Label, err)
	}
	lockPath := filepath.Join(storepath.StateDir(), "antigravity", "native-keychain.lock")
	lease, err := agentantigravity.AcquireNativeProfile(ctx, lockPath, credential)
	if err != nil {
		return err
	}
	defer func() {
		if restoreErr := lease.Restore(context.Background()); restoreErr != nil {
			fmt.Fprintf(r.errOut, "warning: restore native AGY Keychain profile: %v\n", restoreErr)
		}
	}()
	active, activeOK, err := agentantigravity.ReadLocalCredential(ctx, time.Now())
	if err != nil || !activeOK || active.RefreshToken != credential.RefreshToken {
		return fmt.Errorf("native AGY identity verification failed for %q; no vendor process started", chosen.Label)
	}
	command, err := exec.LookPath("agy")
	if err != nil {
		return fmt.Errorf("find agy: %w", err)
	}
	cmd := exec.CommandContext(ctx, command, vendorArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	fmt.Fprintf(r.out, "Launching AGY as %s (native profile is pinned for this process).\n", chosen.Label)
	return cmd.Run()
}

func parseAntigravityNativeArgs(args []string) (selector string, picker bool, vendorArgs []string, err error) {
	vendorArgs = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--account":
			picker = true
			if i+1 < len(args) && args[i+1] != "--" && !strings.HasPrefix(args[i+1], "-") {
				selector, picker, i = args[i+1], false, i+1
			}
		case strings.HasPrefix(arg, "--account="):
			selector = strings.TrimSpace(strings.TrimPrefix(arg, "--account="))
			if selector == "" {
				return "", false, nil, errors.New("--account requires a profile selector or no value for the picker")
			}
		default:
			vendorArgs = append(vendorArgs, arg)
		}
	}
	return selector, picker, vendorArgs, nil
}

func chooseAntigravityProfile(in io.Reader, out io.Writer, profiles []baseaccount.Account, selector string, picker bool) (baseaccount.Account, error) {
	if selector != "" {
		for _, profile := range profiles {
			if strings.EqualFold(profile.Label, selector) || strings.EqualFold(profile.Email, selector) || strings.EqualFold(profile.ID, selector) {
				return profile, nil
			}
		}
		return baseaccount.Account{}, fmt.Errorf("no local AGY profile matches %q", selector)
	}
	if !picker {
		return profiles[0], nil
	}
	for i, profile := range profiles {
		label := profile.Email
		if strings.TrimSpace(label) == "" {
			label = profile.Label
		}
		fmt.Fprintf(out, "%d) %s\n", i+1, label)
	}
	answer, err := promptLine(out, bufio.NewReader(in), "Use AGY account (#): ")
	if err != nil {
		return baseaccount.Account{}, err
	}
	index, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil || index < 1 || index > len(profiles) {
		return baseaccount.Account{}, errors.New("invalid AGY account selection")
	}
	return profiles[index-1], nil
}

func (r srRunner) antigravityRemote(ctx context.Context, server srServerConfig, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(r.out, antigravityManagementHelp)
		fmt.Fprintf(r.out, "Managed profiles are stored on server %s.\n", server.Name)
		return nil
	}
	switch args[0] {
	case "add", "import":
		if len(args) != 2 {
			return fmt.Errorf("usage: sr agy add <label>")
		}
		credential, ok, err := agentantigravity.ReadLocalCredential(ctx, time.Now())
		if err != nil {
			return fmt.Errorf("read current agy login: %w", err)
		}
		if !ok {
			return fmt.Errorf("plain agy is not logged in; sign in with 'agy', then rerun 'sr agy add %s'", strings.TrimSpace(args[1]))
		}
		grant := credential
		credential, err = agentantigravity.PrepareManagedCredential(ctx, r.client, credential, time.Now())
		if err != nil {
			return fmt.Errorf("validate current agy login before import: %w", err)
		}
		credential = antigravityRemoteImportCredential(credential, grant)
		if err := r.uploadServerAntigravityAccount(ctx, server, args[1], credential); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "Added isolated Antigravity account %q to server %s. Run: sr status\n", strings.TrimSpace(args[1]), server.Name)
		return nil
	case "list", "ls":
		all, err := r.fetchServerAccounts(ctx, server)
		if err != nil {
			return err
		}
		found := false
		for _, acct := range all {
			if acct.Provider == baseaccount.ProviderAntigravity && acct.AuthMode == baseaccount.AuthModeOAuth &&
				agentantigravity.IsManagedAccountID(acct.ID) {
				found = true
				fmt.Fprintf(r.out, "%s (%s)\n", acct.Label, acct.ID)
			}
		}
		if !found {
			fmt.Fprintln(r.out, "No isolated Antigravity accounts configured on the selected server.")
		}
		return nil
	case "recover":
		return errors.New("native AGY recovery is local-only; run 'sr server use local' and retry")
	case "remove", "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: sr agy remove <label>")
		}
		if err := r.removeServerAntigravityAccount(ctx, server, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "Removed Antigravity account %q from server %s.\n", strings.TrimSpace(args[1]), server.Name)
		return nil
	default:
		return fmt.Errorf("unknown Antigravity command %q; use add, list, or remove", args[0])
	}
}

func antigravityRemoteImportCredential(prepared, original agentantigravity.CredentialInfo) agentantigravity.CredentialInfo {
	// Preserve the submitted grant for server-side duplicate detection while
	// carrying the OAuth client binding discovered during local validation. The
	// server independently refresh-attests this original grant before storage.
	prepared.RefreshToken = original.RefreshToken
	return prepared
}

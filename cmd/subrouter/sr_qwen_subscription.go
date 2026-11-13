package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/agents/qwensubscription"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

const qwenSubscriptionHelp = `Qwen.ai Plus/Pro subscriptions:
  sr qwen subscription login <label>
  sr qwen subscription list
  sr qwen subscription remove <label>

Accounts are stored on the selected router. Login authorizes an independent
OAuth grant without changing the plain Qwen CLI login. OpenAI-compatible
clients use <router-url>/qwen-oauth/v1 with the router's proxy token.
Alibaba plan keys and sr qwen login remain separate.
`

func (r srRunner) qwenSubscription(ctx context.Context, args []string, server *srServerConfig) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(r.out, qwenSubscriptionHelp)
		return nil
	}
	store := qwensubscription.Store{}
	switch args[0] {
	case "list", "ls":
		if len(args) != 1 {
			return fmt.Errorf("usage: sr qwen subscription list")
		}
		if server != nil {
			all, err := r.fetchServerAccounts(ctx, *server)
			if err != nil {
				return err
			}
			found := false
			for _, acct := range all {
				if acct.Provider == accounts.ProviderQwenOAuth && acct.AuthMode == accounts.AuthModeOAuth {
					found = true
					fmt.Fprintf(r.out, "%s (%s)\n", acct.Label, acct.ID)
				}
			}
			if !found {
				fmt.Fprintln(r.out, "No Qwen.ai subscription accounts configured on the selected server.")
			}
			return nil
		}
		all, err := store.ListAccounts(ctx)
		if err != nil {
			return err
		}
		for _, acct := range all {
			fmt.Fprintf(r.out, "%s (%s)\n", acct.Label, acct.ID)
		}
		if len(all) == 0 {
			fmt.Fprintln(r.out, "No Qwen.ai subscription accounts configured.")
		}
		return nil
	case "login", "add", "remove", "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage:\n%s", qwenSubscriptionHelp)
		}
		label := strings.TrimSpace(args[1])
		if _, err := qwensubscription.ManagedAccountID(label); err != nil {
			return err
		}
		remove := args[0] == "remove" || args[0] == "rm"
		if server != nil {
			if err := r.ensureServerAccountImportProviderAvailable(ctx, *server, accounts.ProviderQwenOAuth); err != nil {
				return err
			}
		}
		payload := &serverQwenSubscriptionImport{Label: label, Remove: remove}
		if !remove {
			credential, err := store.AuthorizeManaged(ctx, r.client, label, r.out)
			if err != nil {
				return fmt.Errorf("Qwen subscription login failed: %w", err)
			}
			payload.Credential = credential
		}
		if server != nil {
			if err := r.postServerAccountImport(ctx, *server, serverAccountImportRequest{Provider: accounts.ProviderQwenOAuth, QwenSubscription: payload}); err != nil {
				return err
			}
		} else {
			err := proxy.PublishAccountDiskMutation(ctx, r.store.StoreDir(), func() (bool, error) {
				if remove {
					_, changed, err := store.RemoveManagedAccount(label)
					if err == nil && !changed {
						return false, fmt.Errorf("Qwen subscription %q was not found", label)
					}
					return changed, err
				}
				_, err := store.SaveManagedCredential(label, payload.Credential)
				return err == nil, err
			})
			if err != nil {
				return err
			}
		}
		if remove {
			fmt.Fprintf(r.out, "Removed Qwen subscription %q.\n", label)
		} else {
			fmt.Fprintf(r.out, "Added isolated Qwen subscription %q. Run: sr status\n", label)
		}
		return nil
	default:
		return fmt.Errorf("unknown Qwen subscription command %q\n%s", args[0], qwenSubscriptionHelp)
	}
}

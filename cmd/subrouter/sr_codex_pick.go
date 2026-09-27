package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"golang.org/x/term"
)

// codexAccountOptions is the opt-in `sr codex --account [SEL]` pin. Codex
// launches normally leave account choice to the server, which routes and
// fails over on its own; the picker is for when you want one account.
type codexAccountOptions struct {
	selector string
	pick     bool
}

func (o codexAccountOptions) requested() bool { return o.pick || o.selector != "" }

// takeCodexAccountFlag removes a leading --account [SEL] (or --account=SEL)
// from the launcher arguments. Like the Claude launcher, a `--` right after
// it ends sr's options. Anything later belongs to Codex.
func takeCodexAccountFlag(args []string) (codexAccountOptions, []string, error) {
	var options codexAccountOptions
	if len(args) == 0 {
		return options, args, nil
	}
	rest := args
	switch {
	case strings.HasPrefix(args[0], "--account="):
		options.selector = strings.TrimSpace(strings.TrimPrefix(args[0], "--account="))
		if options.selector == "" {
			return options, nil, errors.New("--account= requires a Codex account selector")
		}
		rest = args[1:]
	case args[0] == "--account":
		if len(args) == 1 || args[1] == "--" || strings.HasPrefix(args[1], "-") {
			options.pick = true
			rest = args[1:]
		} else {
			options.selector = strings.TrimSpace(args[1])
			rest = args[2:]
		}
	default:
		return options, args, nil
	}
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	return options, rest, nil
}

// resolveCodexLaunchAccount turns --account into a server routing ID, showing
// the same health and usage table as `sr` when picking. chosen=false means
// the user cancelled.
// codexPickerServer is the server the launch actually resolved to, so the
// picker lists the accounts Codex will be routed across: the local daemon
// after a fallback, or the registered remote whose proxy root is baseURL.
func codexPickerServer(localTarget bool, baseURL string) (srServerConfig, error) {
	if localTarget {
		return srServerConfig{Name: "local", URL: localBaseURL()}, nil
	}
	file, err := defaultSRServerStore(accounts.DefaultCodexStore()).load()
	if err == nil {
		// An exact proxy root identifies the server, including its tenant.
		// Otherwise accept a unique same-origin entry (a healed address).
		var sameOrigin []srServerConfig
		for _, server := range file.Servers {
			root, rootErr := codexBaseURLForServer(server)
			if rootErr != nil {
				continue
			}
			if strings.TrimRight(root, "/") == strings.TrimRight(baseURL, "/") {
				return server, nil
			}
			if sameEndpoint(root, baseURL) {
				sameOrigin = append(sameOrigin, server)
			}
		}
		if len(sameOrigin) == 1 {
			return sameOrigin[0], nil
		}
	}
	return srServerConfig{}, fmt.Errorf("--account cannot tell which server %s belongs to; set SUBROUTER_CODEX_ACCOUNT_ID to pin an account instead", redactedServerURL(baseURL))
}

// resolveCodexLaunchAccount prints to out (stderr in production) so the
// picker never mixes into Codex's stdout.
func resolveCodexLaunchAccount(ctx context.Context, server srServerConfig, options codexAccountOptions, in io.Reader, out io.Writer) (string, bool, error) {
	r := srRunner{
		program:       programBase(),
		store:         accounts.DefaultCodexStore(),
		useServingAPI: true,
		in:            in,
		out:           out,
		errOut:        os.Stderr,
		client:        &http.Client{Timeout: 30 * time.Second},
	}
	if options.pick && !stdinIsTerminal(in) {
		return "", false, fmt.Errorf("--account without an account needs an interactive terminal; pass the account, as in '%s codex --account ACCOUNT -- ...'", r.programOrSubrouter())
	}
	inventory, err := r.fetchServerAccounts(ctx, server)
	if err != nil {
		return "", false, fmt.Errorf("load Codex accounts from server %s: %w", server.Name, err)
	}
	var eligible []remoteServerAccount
	for _, account := range inventory {
		if account.Provider == accounts.ProviderCodex && strings.TrimSpace(account.ID) != "" {
			eligible = append(eligible, account)
		}
	}
	if len(eligible) == 0 {
		return "", false, fmt.Errorf("no Codex accounts are available on server %s", server.Name)
	}
	var statuses []remoteServerUsageStatus
	if options.selector != "" {
		// A named account is refused when broken, so judge it on live usage,
		// never on a cached copy from before it recovered.
		if usage, available, usageErr := r.fetchServerUsageStatuses(ctx, server); usageErr == nil && available {
			statuses = usage
		}
	} else {
		var notice string
		statuses, notice = r.accountPickerUsage(ctx, server)
		if notice != "" {
			fmt.Fprintln(r.errOut, notice)
		}
	}
	picker := newAccountPicker(accounts.ProviderCodex, eligible, statuses)
	if options.selector != "" {
		accountID, err := picker.resolveSelector(options.selector)
		if err != nil {
			if isKnownCodexCommand(options.selector) {
				err = fmt.Errorf("%w; to open the picker before a Codex command, use '%s codex --account -- %s ...'", err, r.programOrSubrouter(), options.selector)
			}
			return "", false, err
		}
		if err := picker.refuseBroken(accountID); err != nil {
			return "", false, err
		}
		return accountID, true, nil
	}
	fmt.Fprintln(out, "Choose one Codex account for this PINNED process. No account failover will occur.")
	picker.display(out, true)
	reader := bufio.NewReader(in)
	for attempt := 0; ; attempt++ {
		answer, err := promptLine(out, reader, picker.prompt(true))
		if err != nil {
			return "", false, err
		}
		accountID, chosen, pickErr := picker.choose(strings.TrimSpace(answer), true, inventory)
		if pickErr == nil {
			return accountID, chosen, nil
		}
		var unusable *claudePickerUnusableError
		if !errors.As(pickErr, &unusable) || attempt >= 2 {
			return "", false, pickErr
		}
		fmt.Fprintln(out, pickErr.Error())
	}
}

func stdinIsTerminal(in io.Reader) bool {
	file, ok := in.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

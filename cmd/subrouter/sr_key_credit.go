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
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

const srKeyCreditUsage = `usage:
  sr key-credit                     list Claude API-key credit grants on the server
  sr key-credit set <label> [--amount USD] [--expires YYYY-MM-DD] [--spent USD]
                                    record a key's free credit grant and its expiry`

// keyCredit reads or sets the server's Claude API-key credit grants. The
// router paces each key so its grant is spent before it expires.
func (r srRunner) keyCredit(ctx context.Context, args []string) error {
	server, ok, err := r.selectedRemoteServer()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("sr key-credit needs a server; select one with 'sr remote use <name>'")
	}
	if len(args) == 0 || args[0] == "list" || args[0] == "ls" {
		var out map[string]proxy.ClaudeCreditStatus
		if err := r.keyCreditRequest(ctx, server, http.MethodGet, nil, &out); err != nil {
			return err
		}
		ids := make([]string, 0, len(out))
		for id := range out {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if len(ids) == 0 {
			fmt.Fprintln(r.out, "No Claude API keys on the server. Run: sr add-key --provider claude")
			return nil
		}
		for _, id := range ids {
			printKeyCredit(r.out, id, out[id])
		}
		return nil
	}
	if args[0] != "set" || len(args) < 2 || strings.HasPrefix(args[1], "-") {
		return errors.New(srKeyCreditUsage)
	}
	id := strings.TrimSpace(args[1])
	if !strings.HasPrefix(id, "claude:") {
		id = "claude:" + id
	}
	flags := flag.NewFlagSet("sr key-credit set", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	amount := flags.Float64("amount", 0, "grant amount in USD")
	expires := flags.String("expires", "", "grant expiry date, YYYY-MM-DD (UTC) or RFC3339")
	spent := flags.Float64("spent", -1, "credit already spent this period, in USD")
	if err := flags.Parse(args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New(srKeyCreditUsage)
	}
	payload := map[string]any{"account_id": id}
	if *amount > 0 {
		payload["amount_usd"] = *amount
	}
	if *expires != "" {
		at, err := parseKeyCreditExpiry(*expires)
		if err != nil {
			return err
		}
		payload["expires_at"] = at
	}
	if *spent >= 0 {
		payload["spent_usd"] = *spent
	}
	if len(payload) == 1 {
		return errors.New("set at least one of --amount, --expires, --spent")
	}
	var status proxy.ClaudeCreditStatus
	if err := r.keyCreditRequest(ctx, server, http.MethodPost, payload, &status); err != nil {
		return err
	}
	printKeyCredit(r.out, id, status)
	return nil
}

// parseKeyCreditExpiry reads a date as the start of that day in UTC, the
// conservative reading of "expires on".
func parseKeyCreditExpiry(value string) (time.Time, error) {
	if at, err := time.Parse(time.RFC3339, value); err == nil {
		return at.UTC(), nil
	}
	at, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --expires %q, expected YYYY-MM-DD", value)
	}
	return at.UTC(), nil
}

func printKeyCredit(out io.Writer, id string, status proxy.ClaudeCreditStatus) {
	defaulted := ""
	if status.Defaulted {
		defaulted = " (defaulted; set with sr key-credit set)"
	}
	fmt.Fprintf(out, "%s  $%.2f of $%.2f left, expires %s, burn $%.2f/h, %s%s\n",
		strings.TrimPrefix(id, "claude:"), status.RemainingUSD, status.GrantUSD,
		status.ExpiresAt.UTC().Format("2006-01-02 15:04Z"), status.BurnPerHour, status.Pace, defaulted)
}

func (r srRunner) keyCreditRequest(ctx context.Context, server srServerConfig, method string, payload any, out any) error {
	baseURL, err := serverControlBaseURL(server)
	if err != nil {
		return err
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+"/_subrouter/claude-key-credit", body)
	if err != nil {
		return redactServerRequestError(err, server)
	}
	addServerAdminAuth(req, server)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client, err := r.securedRequestClientForServer(server, baseURL, 15*time.Second)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return redactServerRequestError(err, server)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound && method == http.MethodGet {
		return fmt.Errorf("server %s does not support key credits yet; upgrade it", server.Name)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("key credit request failed: %s: %s", res.Status, strings.TrimSpace(string(message)))
	}
	return json.NewDecoder(res.Body).Decode(out)
}

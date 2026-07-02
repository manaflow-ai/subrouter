package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/storepath"
	"github.com/manaflow-ai/subrouter/internal/tenant"
)

func srTenantHelp(program string) string {
	command := program
	if strings.TrimSpace(command) == "" {
		command = "sr"
	}
	command += " tenant"
	return fmt.Sprintf(`%[1]s - Manage Subrouter tenants (per-tenant account pools)

Usage:
  %[1]s create <name> [--server <name>]
  %[1]s list [--server <name>]
  %[1]s key create <tenant> [--server <name>]
  %[1]s key revoke <tenant> <key-prefix> [--server <name>]

Commands talk to a named server's admin endpoints (or the default server).
Without a configured server they operate on the local state dir, for use on
the server host itself.
`, command)
}

// tenant dispatches sr tenant subcommands. Tenant CRUD goes over the server's
// admin API when a server is named or a default server is configured, and
// falls back to direct filesystem access against the local state dir so the
// same commands work when run on the server host.
func (r srRunner) tenant(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(r.out, srTenantHelp(r.programOrSubrouter()))
		return nil
	}
	switch args[0] {
	case "create":
		return r.tenantCreate(ctx, args[1:])
	case "list", "ls":
		return r.tenantList(ctx, args[1:])
	case "key", "keys":
		return r.tenantKey(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(r.out, srTenantHelp(r.programOrSubrouter()))
		return nil
	default:
		return fmt.Errorf("unknown tenant command %q\n%s", args[0], srTenantHelp(r.programOrSubrouter()))
	}
}

func (r srRunner) tenantKey(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(r.out, srTenantHelp(r.programOrSubrouter()))
		return nil
	}
	switch args[0] {
	case "create", "add":
		return r.tenantKeyCreate(ctx, args[1:])
	case "revoke", "remove", "rm":
		return r.tenantKeyRevoke(ctx, args[1:])
	default:
		return fmt.Errorf("unknown tenant key command %q\n%s", args[0], srTenantHelp(r.programOrSubrouter()))
	}
}

// parseTenantArgs extracts positional args and the optional --server flag.
func (r srRunner) parseTenantArgs(name string, args []string, positional int) ([]string, srServerConfig, bool, error) {
	flags := flag.NewFlagSet(r.programOrSubrouter()+" tenant "+name, flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	serverName := flags.String("server", "", "named Subrouter server to manage tenants on; defaults to the default server, then the local state dir")
	if err := flags.Parse(args); err != nil {
		return nil, srServerConfig{}, false, err
	}
	if flags.NArg() != positional {
		return nil, srServerConfig{}, false, fmt.Errorf("usage:\n%s", srTenantHelp(r.programOrSubrouter()))
	}
	store := defaultSRServerStore(r.store)
	if strings.TrimSpace(*serverName) != "" {
		if isLocalServerName(*serverName) {
			return flags.Args(), srServerConfig{}, false, nil
		}
		server, ok, err := store.find(*serverName)
		if err != nil {
			return nil, srServerConfig{}, false, err
		}
		if !ok {
			return nil, srServerConfig{}, false, fmt.Errorf("server %q not found", *serverName)
		}
		return flags.Args(), server, true, nil
	}
	file, err := store.load()
	if err != nil {
		return nil, srServerConfig{}, false, err
	}
	if strings.TrimSpace(file.Default) != "" {
		if server, ok := file.find(file.Default); ok {
			return flags.Args(), server, true, nil
		}
	}
	return flags.Args(), srServerConfig{}, false, nil
}

func localTenantRegistry() *tenant.Registry {
	return tenant.NewRegistry(storepath.StateDir())
}

type tenantAdminKeyView struct {
	Prefix    string    `json:"prefix"`
	CreatedAt time.Time `json:"createdAt"`
}

type tenantAdminView struct {
	ID        string               `json:"id"`
	Name      string               `json:"name"`
	CreatedAt time.Time            `json:"createdAt"`
	Keys      []tenantAdminKeyView `json:"keys"`
}

func (r srRunner) tenantCreate(ctx context.Context, args []string) error {
	positional, server, remote, err := r.parseTenantArgs("create", args, 1)
	if err != nil {
		return err
	}
	name := positional[0]
	if remote {
		var created struct {
			Tenant tenantAdminView `json:"tenant"`
			Key    string          `json:"key"`
		}
		body, _ := json.Marshal(map[string]string{"name": name})
		if err := r.tenantAdminRequest(ctx, server, http.MethodPost, "/_subrouter/tenants", body, &created); err != nil {
			return err
		}
		r.printTenantKeyOnce(server.Name, created.Tenant.ID, created.Tenant.Name, created.Key)
		return nil
	}
	created, key, err := localTenantRegistry().Create(name)
	if err != nil {
		return err
	}
	r.printTenantKeyOnce("local", created.ID, created.Name, key)
	return nil
}

func (r srRunner) printTenantKeyOnce(where, id, name, key string) {
	fmt.Fprintf(r.out, "Created tenant %s (id %s) on %s\n", name, id, where)
	fmt.Fprintf(r.out, "Tenant key (shown once, store it now): %s\n", key)
	fmt.Fprintf(r.out, "Point clients at <server-url>/t/%s\n", key)
}

func (r srRunner) tenantList(ctx context.Context, args []string) error {
	_, server, remote, err := r.parseTenantArgs("list", args, 0)
	if err != nil {
		return err
	}
	var views []tenantAdminView
	where := "local"
	if remote {
		where = server.Name
		if err := r.tenantAdminRequest(ctx, server, http.MethodGet, "/_subrouter/tenants", nil, &views); err != nil {
			return err
		}
	} else {
		tenants, err := localTenantRegistry().List()
		if err != nil {
			return err
		}
		for _, t := range tenants {
			view := tenantAdminView{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt}
			for _, k := range t.Keys {
				view.Keys = append(view.Keys, tenantAdminKeyView{Prefix: k.Prefix, CreatedAt: k.CreatedAt})
			}
			views = append(views, view)
		}
	}
	if len(views) == 0 {
		fmt.Fprintf(r.out, "No tenants on %s. Run: %s tenant create <name>\n", where, r.programOrSubrouter())
		return nil
	}
	for _, view := range views {
		prefixes := make([]string, 0, len(view.Keys))
		for _, key := range view.Keys {
			prefixes = append(prefixes, key.Prefix+"…")
		}
		fmt.Fprintf(r.out, "%s\t%s\tkeys: %s\n", view.ID, view.Name, strings.Join(prefixes, ", "))
	}
	return nil
}

func (r srRunner) tenantKeyCreate(ctx context.Context, args []string) error {
	positional, server, remote, err := r.parseTenantArgs("key create", args, 1)
	if err != nil {
		return err
	}
	ref := positional[0]
	if remote {
		id, name, err := r.resolveRemoteTenant(ctx, server, ref)
		if err != nil {
			return err
		}
		var created struct {
			Tenant tenantAdminView `json:"tenant"`
			Key    string          `json:"key"`
		}
		if err := r.tenantAdminRequest(ctx, server, http.MethodPost, "/_subrouter/tenants/"+url.PathEscape(id)+"/keys", nil, &created); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "New key for tenant %s (id %s) on %s\n", name, id, server.Name)
		fmt.Fprintf(r.out, "Tenant key (shown once, store it now): %s\n", created.Key)
		return nil
	}
	registry := localTenantRegistry()
	found, ok, err := registry.Find(ref)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("tenant %q not found", ref)
	}
	_, key, err := registry.CreateKey(found.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.out, "New key for tenant %s (id %s) on local\n", found.Name, found.ID)
	fmt.Fprintf(r.out, "Tenant key (shown once, store it now): %s\n", key)
	return nil
}

func (r srRunner) tenantKeyRevoke(ctx context.Context, args []string) error {
	positional, server, remote, err := r.parseTenantArgs("key revoke", args, 2)
	if err != nil {
		return err
	}
	ref, keyRef := positional[0], positional[1]
	if remote {
		id, name, err := r.resolveRemoteTenant(ctx, server, ref)
		if err != nil {
			return err
		}
		var result struct {
			Revoked int `json:"revoked"`
		}
		path := "/_subrouter/tenants/" + url.PathEscape(id) + "/keys/" + url.PathEscape(keyRef)
		if err := r.tenantAdminRequest(ctx, server, http.MethodDelete, path, nil, &result); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "Revoked %d key(s) matching %s on tenant %s (%s)\n", result.Revoked, keyRef, name, server.Name)
		return nil
	}
	registry := localTenantRegistry()
	found, ok, err := registry.Find(ref)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("tenant %q not found", ref)
	}
	revoked, err := registry.RevokeKey(found.ID, keyRef)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Revoked %d key(s) matching %s on tenant %s (local)\n", revoked, keyRef, found.Name)
	return nil
}

func (r srRunner) resolveRemoteTenant(ctx context.Context, server srServerConfig, ref string) (id, name string, err error) {
	var views []tenantAdminView
	if err := r.tenantAdminRequest(ctx, server, http.MethodGet, "/_subrouter/tenants", nil, &views); err != nil {
		return "", "", err
	}
	for _, view := range views {
		if view.ID == ref || strings.EqualFold(view.Name, ref) {
			return view.ID, view.Name, nil
		}
	}
	return "", "", fmt.Errorf("tenant %q not found on server %s", ref, server.Name)
}

func (r srRunner) tenantAdminRequest(ctx context.Context, server srServerConfig, method, path string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(server.URL, "/")+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	addServerAdminAuth(req, server)
	client := r.client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("tenant admin request failed: %s: %s", res.Status, strings.TrimSpace(string(message)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

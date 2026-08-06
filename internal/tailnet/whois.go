// Package tailnet identifies the Tailscale peer behind a request, so a
// self-hosted server can use the tailnet itself as its authentication
// mechanism instead of carrying a second credential system on top of it.
//
// Identity comes from the local tailscaled through the `tailscale whois`
// command. That is an assertion by this machine's own daemon about a
// WireGuard-authenticated peer, not a claim carried in the request, so it
// cannot be forged by anything that merely reaches the port.
package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Identity is the tailnet principal behind a connection. LoginName is the
// tailnet user ("someone@example.com"), or "tagged-devices" for a tagged node,
// in which case Tags carries the tags that authorize it.
type Identity struct {
	LoginName string
	NodeName  string
	Tags      []string
}

// String renders an identity for logs: a tagged node is identified by its tags,
// which is what an ACL is written against.
func (i Identity) String() string {
	name := i.NodeName
	if name == "" {
		name = "unknown-node"
	}
	if len(i.Tags) > 0 {
		return fmt.Sprintf("%s (%s)", name, strings.Join(i.Tags, ","))
	}
	return fmt.Sprintf("%s (%s)", name, i.LoginName)
}

type commandRunner interface {
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Resolver answers "who is the peer at this address" with a short-lived cache.
// Admin and account-import requests are rare, but a status page can fan out
// several at once and there is no reason to spawn a process for each.
type Resolver struct {
	CLIPath string
	Timeout time.Duration
	TTL     time.Duration

	runner commandRunner
	now    func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	identity Identity
	ok       bool
	expires  time.Time
}

// DefaultCLICandidates covers Homebrew, the standard package location, and the
// macOS app bundle, in that order.
var DefaultCLICandidates = []string{
	"tailscale",
	"/opt/homebrew/bin/tailscale",
	"/usr/local/bin/tailscale",
	"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
}

// NewResolver locates the tailscale CLI. It fails when no CLI is present rather
// than degrading to "allow everyone": a server told to authenticate with the
// tailnet must not silently stop authenticating.
func NewResolver(cliPath string) (*Resolver, error) {
	resolved, err := resolveCLI(cliPath)
	if err != nil {
		return nil, err
	}
	return &Resolver{
		CLIPath: resolved,
		Timeout: 2 * time.Second,
		TTL:     30 * time.Second,
		runner:  execRunner{},
		now:     time.Now,
		entries: map[string]cacheEntry{},
	}, nil
}

func resolveCLI(cliPath string) (string, error) {
	if strings.TrimSpace(cliPath) != "" {
		path, err := exec.LookPath(cliPath)
		if err != nil {
			return "", fmt.Errorf("tailscale CLI %q is not executable: %w", cliPath, err)
		}
		return path, nil
	}
	for _, candidate := range DefaultCLICandidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", errors.New("tailscale CLI not found; install Tailscale or pass --tailscale-cli")
}

type whoisResponse struct {
	Node struct {
		Name string   `json:"Name"`
		Tags []string `json:"Tags"`
	} `json:"Node"`
	UserProfile struct {
		LoginName string `json:"LoginName"`
	} `json:"UserProfile"`
}

// Lookup resolves a remote address to its tailnet identity. A peer that is not
// on the tailnet returns ok=false rather than an error, because "not a tailnet
// peer" is a normal answer, not a malfunction.
func (r *Resolver) Lookup(ctx context.Context, remoteAddr string) (Identity, bool) {
	host := hostOnly(remoteAddr)
	if host == "" {
		return Identity{}, false
	}
	if identity, ok, found := r.cached(host); found {
		return identity, ok
	}
	identity, ok := r.lookupUncached(ctx, host)
	r.store(host, identity, ok)
	return identity, ok
}

func (r *Resolver) lookupUncached(ctx context.Context, host string) (Identity, bool) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// whois takes ip[:port]; the port is only used to disambiguate proto.
	body, err := r.runner.Output(lookupCtx, r.CLIPath, "whois", "--json", host)
	if err != nil {
		return Identity{}, false
	}
	var response whoisResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return Identity{}, false
	}
	login := strings.TrimSpace(response.UserProfile.LoginName)
	if login == "" && len(response.Node.Tags) == 0 {
		return Identity{}, false
	}
	return Identity{
		LoginName: login,
		NodeName:  strings.TrimSuffix(strings.TrimSpace(response.Node.Name), "."),
		Tags:      response.Node.Tags,
	}, true
}

func (r *Resolver) cached(host string) (Identity, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, found := r.entries[host]
	if !found || r.clock().After(entry.expires) {
		return Identity{}, false, false
	}
	return entry.identity, entry.ok, true
}

func (r *Resolver) store(host string, identity Identity, ok bool) {
	ttl := r.TTL
	if ttl <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = map[string]cacheEntry{}
	}
	r.entries[host] = cacheEntry{identity: identity, ok: ok, expires: r.clock().Add(ttl)}
}

func (r *Resolver) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func hostOnly(remoteAddr string) string {
	address := strings.TrimSpace(remoteAddr)
	if address == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return address
}

// Authorizer decides whether a resolved tailnet identity may act on this
// server. With no allowlist every tailnet peer is accepted, which is the point
// of the mode: the tailnet ACL already decides who can reach the port.
type Authorizer struct {
	Resolver *Resolver
	Users    []string
	Tags     []string
}

// Authorize reports the identity behind a request and whether it is allowed.
func (a *Authorizer) Authorize(ctx context.Context, remoteAddr string) (Identity, bool) {
	if a == nil || a.Resolver == nil {
		return Identity{}, false
	}
	identity, ok := a.Resolver.Lookup(ctx, remoteAddr)
	if !ok {
		return Identity{}, false
	}
	if len(a.Users) == 0 && len(a.Tags) == 0 {
		return identity, true
	}
	for _, user := range a.Users {
		if strings.EqualFold(strings.TrimSpace(user), identity.LoginName) {
			return identity, true
		}
	}
	for _, allowed := range a.Tags {
		for _, tag := range identity.Tags {
			if strings.EqualFold(normalizeTag(allowed), normalizeTag(tag)) {
				return identity, true
			}
		}
	}
	return identity, false
}

// normalizeTag accepts both "tag:ci" and "ci" so a flag value that omits the
// prefix does not silently match nothing.
func normalizeTag(tag string) string {
	trimmed := strings.TrimSpace(tag)
	if trimmed == "" {
		return ""
	}
	return "tag:" + strings.TrimPrefix(trimmed, "tag:")
}

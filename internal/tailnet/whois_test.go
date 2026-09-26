package tailnet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRunner struct {
	mu     sync.Mutex
	calls  int
	output string
	err    error
	args   []string
}

func (f *fakeRunner) Output(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.args = args
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.output), nil
}

const userWhois = `{
  "Node": {"Name": "lawrences-macbook-pro-2.tail137216.ts.net.", "Tags": null},
  "UserProfile": {"LoginName": "lawrence@manaflow.ai", "DisplayName": "Lawrence Chen"}
}`

const taggedWhois = `{
  "Node": {"Name": "cmux-lawrences-mac-mini.tail137216.ts.net.", "Tags": ["tag:dev-workstation"]},
  "UserProfile": {"LoginName": "tagged-devices", "DisplayName": "Tagged Devices"}
}`

func newTestResolver(runner commandRunner) *Resolver {
	return &Resolver{
		CLIPath: "tailscale",
		Timeout: time.Second,
		TTL:     30 * time.Second,
		runner:  runner,
		now:     time.Now,
		entries: map[string]cacheEntry{},
	}
}

func TestLookupReturnsUserIdentity(t *testing.T) {
	runner := &fakeRunner{output: userWhois}
	identity, ok := newTestResolver(runner).Lookup(context.Background(), "100.82.214.112:52344")
	if !ok {
		t.Fatal("expected the peer to resolve")
	}
	if identity.LoginName != "lawrence@manaflow.ai" {
		t.Fatalf("login = %q", identity.LoginName)
	}
	// The trailing dot in a MagicDNS name is noise in logs.
	if identity.NodeName != "lawrences-macbook-pro-2.tail137216.ts.net" {
		t.Fatalf("node = %q", identity.NodeName)
	}
	// whois takes the bare address; the port must be stripped before the call.
	if got := strings.Join(runner.args, " "); got != "whois --json 100.82.214.112" {
		t.Fatalf("args = %q", got)
	}
}

func TestLookupReturnsTagsForTaggedNode(t *testing.T) {
	identity, ok := newTestResolver(&fakeRunner{output: taggedWhois}).Lookup(context.Background(), "100.89.225.106:4001")
	if !ok {
		t.Fatal("expected the tagged peer to resolve")
	}
	if len(identity.Tags) != 1 || identity.Tags[0] != "tag:dev-workstation" {
		t.Fatalf("tags = %v", identity.Tags)
	}
	if !strings.Contains(identity.String(), "tag:dev-workstation") {
		t.Fatalf("string form should identify a tagged node by tag: %q", identity.String())
	}
}

// A peer that is not on the tailnet is a normal answer, not an error, and must
// never resolve to an identity.
func TestLookupRejectsNonTailnetPeer(t *testing.T) {
	for name, runner := range map[string]*fakeRunner{
		"command fails": {err: errors.New("no peer")},
		"empty profile": {output: `{"Node":{"Name":"x"},"UserProfile":{"LoginName":""}}`},
		"garbage":       {output: "not json"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := newTestResolver(runner).Lookup(context.Background(), "203.0.113.7:9000"); ok {
				t.Fatal("expected the peer to be rejected")
			}
		})
	}
}

func TestLookupCachesWithinTTL(t *testing.T) {
	runner := &fakeRunner{output: userWhois}
	resolver := newTestResolver(runner)
	now := time.Now()
	resolver.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if _, ok := resolver.Lookup(context.Background(), "100.82.214.112:1"); !ok {
			t.Fatal("expected the peer to resolve")
		}
	}
	if runner.calls != 1 {
		t.Fatalf("calls = %d, want 1 within the TTL", runner.calls)
	}
	now = now.Add(31 * time.Second)
	if _, ok := resolver.Lookup(context.Background(), "100.82.214.112:1"); !ok {
		t.Fatal("expected the peer to resolve after expiry")
	}
	if runner.calls != 2 {
		t.Fatalf("calls = %d, want a refresh after the TTL", runner.calls)
	}
}

func TestAuthorizerAllowsEveryTailnetPeerWithoutAnAllowlist(t *testing.T) {
	authorizer := &Authorizer{Resolver: newTestResolver(&fakeRunner{output: userWhois})}
	if _, ok := authorizer.Authorize(context.Background(), "100.82.214.112:1"); !ok {
		t.Fatal("an empty allowlist must accept any tailnet peer")
	}
}

func TestAuthorizerEnforcesUserAndTagAllowlists(t *testing.T) {
	for name, tc := range map[string]struct {
		whois string
		users []string
		tags  []string
		want  bool
	}{
		"matching user":                 {whois: userWhois, users: []string{"lawrence@manaflow.ai"}, want: true},
		"other user":                    {whois: userWhois, users: []string{"austin@manaflow.ai"}, want: false},
		"matching tag":                  {whois: taggedWhois, tags: []string{"tag:dev-workstation"}, want: true},
		"tag without prefix":            {whois: taggedWhois, tags: []string{"dev-workstation"}, want: true},
		"other tag":                     {whois: taggedWhois, tags: []string{"tag:ci"}, want: false},
		"user list rejects tagged node": {whois: taggedWhois, users: []string{"lawrence@manaflow.ai"}, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			authorizer := &Authorizer{
				Resolver: newTestResolver(&fakeRunner{output: tc.whois}),
				Users:    tc.users,
				Tags:     tc.tags,
			}
			_, ok := authorizer.Authorize(context.Background(), "100.64.0.5:1")
			if ok != tc.want {
				t.Fatalf("authorized = %v, want %v", ok, tc.want)
			}
		})
	}
}

// A nil authorizer must never authorize, so a disabled mode cannot become an
// accidental bypass.
func TestNilAuthorizerDeniesEverything(t *testing.T) {
	var authorizer *Authorizer
	if _, ok := authorizer.Authorize(context.Background(), "100.82.214.112:1"); ok {
		t.Fatal("a nil authorizer must deny")
	}
	if _, ok := (&Authorizer{}).Authorize(context.Background(), "100.82.214.112:1"); ok {
		t.Fatal("an authorizer without a resolver must deny")
	}
}

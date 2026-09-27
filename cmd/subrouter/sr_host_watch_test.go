package main

import (
	"context"
	"strings"
	"testing"
)

func cmuxCalls(fake *hostFakeHost, verb string) [][]string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var out [][]string
	for _, c := range fake.calls {
		if c[0] == "cmux" && len(c) > 1 && c[1] == verb {
			out = append(out, c)
		}
	}
	return out
}

func TestHostWatchLabelsMatchingCmuxWorkspaces(t *testing.T) {
	runner, fake, out, _ := setupHostTest(t, "http://127.0.0.1:31415")
	if err := runner.run(context.Background(), []string{"host", "attach", "big-red"}); err != nil {
		t.Fatalf("attach: %v\n%s", err, out)
	}
	old := hostInCmux
	hostInCmux = func() bool { return true }
	t.Cleanup(func() { hostInCmux = old })
	fake.cmuxList = `{"workspaces":[
		{"id":"ws-red","remote":{"destination":"leo@big-red","connected":true}},
		{"id":"ws-local"},
		{"id":"ws-other","remote":{"destination":"beryl7"}}]}`

	out.Reset()
	if err := runner.run(context.Background(), []string{"host", "watch", "--once"}); err != nil {
		t.Fatalf("watch: %v\n%s", err, out)
	}
	set := cmuxCalls(fake, "set-status")
	want := "cmux set-status sr-pool lawrence ✓ tunnel --workspace ws-red --color #34C759"
	if len(set) != 1 || strings.Join(set[0], " ") != want {
		t.Fatalf("set-status calls = %v", set)
	}
	if !strings.Contains(out.String(), "big-red") || !strings.Contains(out.String(), "tunnel (running)") {
		t.Fatalf("watch output:\n%s", out)
	}
	if n := len(cmuxCalls(fake, "clear-status")); n != 0 {
		t.Fatalf("--once cleared its snapshot (%d clears)", n)
	}
}

func TestHostCmuxPillsOnlyChangeOnTransitions(t *testing.T) {
	runner, fake, _, _ := setupHostTest(t, "http://127.0.0.1:31415")
	fake.cmuxList = `[{"id":"ws-red","remote":{"destination":"big-red"}}]`
	pills := &hostCmuxPills{r: runner, last: map[string]string{}}
	host := attachedHost{SSHHost: "big-red", Route: hostRouteTunnel, Server: "lawrence"}
	ok := []hostCheck{{host: host, reachable: true, healthy: true, tunnelUp: true}}
	down := []hostCheck{{host: host, reachable: true, healthy: false, tunnelUp: false}}
	for _, checks := range [][]hostCheck{ok, ok, down, down} {
		if err := pills.update(context.Background(), checks); err != nil {
			t.Fatal(err)
		}
	}
	set := cmuxCalls(fake, "set-status")
	if len(set) != 2 || set[1][3] != "lawrence ✗ tunnel stopped" || set[1][7] != "#FF3B30" {
		t.Fatalf("set-status calls = %v", set)
	}
	// The workspace goes away (closed): its pill state is dropped.
	fake.cmuxList = `[]`
	if err := pills.update(context.Background(), down); err != nil {
		t.Fatal(err)
	}
	if len(pills.last) != 0 || len(cmuxCalls(fake, "clear-status")) != 1 {
		t.Fatalf("stale pill kept: %v", pills.last)
	}
}

func TestHostCheckPill(t *testing.T) {
	tunnel := attachedHost{Route: hostRouteTunnel, Server: "lawrence"}
	direct := attachedHost{Route: hostRouteDirect, Server: "lawrence"}
	team := attachedHost{Route: hostRouteTeam}
	cases := []struct {
		check hostCheck
		want  string
	}{
		{hostCheck{host: tunnel, reachable: true, healthy: true, tunnelUp: true}, "lawrence ✓ tunnel"},
		{hostCheck{host: tunnel, reachable: true, tunnelUp: false}, "lawrence ✗ tunnel stopped"},
		{hostCheck{host: tunnel, reachable: true, tunnelUp: true}, "lawrence ✗ pool down"},
		{hostCheck{host: direct, reachable: true, healthy: true}, "lawrence ✓"},
		{hostCheck{host: team, reachable: true, healthy: true}, "team ✓"},
		{hostCheck{host: direct}, "lawrence ? host unreachable"},
	}
	for _, tc := range cases {
		if got, _ := tc.check.pill(); got != tc.want {
			t.Fatalf("pill(%+v) = %q, want %q", tc.check, got, tc.want)
		}
	}
}

func TestSameSSHHost(t *testing.T) {
	if !sameSSHHost("leo@big-red", "big-red") || !sameSSHHost("Big-Red", "big-red") {
		t.Fatal("user@host and case should match")
	}
	if sameSSHHost("big-red2", "big-red") || sameSSHHost("", "") {
		t.Fatal("different hosts matched")
	}
}

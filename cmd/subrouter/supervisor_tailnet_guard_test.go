package main

import (
	"strings"
	"testing"
)

// Behind PROXY protocol the client address is whatever the header claims, so
// tailnet auth may only sit behind a listener only this host can reach.
func TestSupervisorRefusesPublicProxyProtocolWithTailnetAuth(t *testing.T) {
	base := supervisorConfig{
		ControlSocket: "/tmp/sup.sock", WorkerBin: "/bin/true",
		ReadyTimeout: 1, DrainTimeout: 1, WorkerStopGrace: 1,
		TakeoverListenerFD: -1, ExpectProxyProtocol: true,
		WorkerArgs: []string{"--tailscale-auth"},
	}
	for _, tc := range []struct {
		addr   string
		reject bool
	}{
		{"0.0.0.0:31415", true},
		{"100.92.167.122:31415", true},
		{"127.0.0.1:31415", false},
		{"[::1]:31415", false},
		{"/tmp/front.sock", false},
	} {
		config := base
		config.Addr = tc.addr
		err := validateSupervisorConfig(config)
		if got := err != nil && containsTailnetGuard(err.Error()); got != tc.reject {
			t.Fatalf("addr %s: err = %v, want reject=%v", tc.addr, err, tc.reject)
		}
	}
	config := base
	config.Addr = "0.0.0.0:31415"
	config.WorkerArgs = nil
	if err := validateSupervisorConfig(config); err != nil && containsTailnetGuard(err.Error()) {
		t.Fatalf("guard fired without tailnet auth: %v", err)
	}
}

func containsTailnetGuard(message string) bool {
	return len(message) > 0 && strings.Contains(message, "--expect-proxy-protocol with --tailscale-auth")
}

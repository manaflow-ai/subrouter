package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBindAddrRequiresAdminToken(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:31415", false},
		{"127.0.0.99:31415", false},
		{"localhost:31415", false},
		{"[::1]:31415", false},
		{"0.0.0.0:31415", true},
		{"[::]:31415", true},
		{":31415", true},
		{"100.64.1.2:31415", true},
		{"192.168.1.10:31415", true},
		{"my-machine.tail1234.ts.net:31415", true},
		// Unparseable addresses never bind; net.Listen reports them with a
		// better error than the admin gate could.
		{"invalid:::", false},
	}
	for _, tc := range cases {
		if got := bindAddrRequiresAdminToken(tc.addr); got != tc.want {
			t.Errorf("bindAddrRequiresAdminToken(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// clearControlSecretEnv makes the test immune to ambient token configuration —
// an exported SUBROUTER_ADMIN_TOKEN on the host would silently satisfy the
// gate and turn these tests vacuous.
func clearControlSecretEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"SUBROUTER_ADMIN_TOKEN", "SUBROUTER_ADMIN_TOKEN_FILE",
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN", "SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE",
		"SUBROUTER_PROXY_TOKEN", "SUBROUTER_PROXY_TOKEN_FILE",
	} {
		t.Setenv(name, "")
	}
}

// The tailnet trap: authorizeAdmin grants loopback callers admin
// unconditionally, so the moment serve binds a wider address, the admin
// surface (accounts, transcripts, account-import, drain) is one HTTP request
// away for every host that can reach the port. A tokenless non-loopback bind
// must therefore die at startup, naming the flags that fix it.
func TestServeRefusesNonLoopbackBindWithoutAdminToken(t *testing.T) {
	for _, addr := range []string{"100.64.0.5:31415", "0.0.0.0:31415", ":31415"} {
		t.Run(addr, func(t *testing.T) {
			clearControlSecretEnv(t)
			tempDir := t.TempDir()
			t.Setenv("SUBROUTER_STATE_DIR", tempDir)
			t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(tempDir, "cloud.json"))

			err := serve([]string{
				"--addr", addr,
				"--fetch-usage=false",
				"--sr-switch-interval=0",
			})
			if err == nil {
				t.Fatal("serve accepted a tokenless non-loopback bind")
			}
			if !strings.Contains(err.Error(), "refusing to bind") ||
				!strings.Contains(err.Error(), "--admin-token") ||
				!strings.Contains(err.Error(), "--allow-unauthenticated-admin") {
				t.Fatalf("gate error %q does not name the fix", err)
			}
		})
	}
}

// Both sanctioned ways past the gate must actually get past it. The fixture
// points SUBROUTER_CLOUD_CONFIG at a directory so serve fails deterministically
// at the cloud-config load — a step that comes after the gate — proving the
// gate itself let the configuration through without ever binding a socket.
func TestServeAdminGateHonorsTokenAndExplicitOptIn(t *testing.T) {
	for name, extraArgs := range map[string][]string{
		"admin-token":                 {"--admin-token", "gate-test-token"},
		"allow-unauthenticated-admin": {"--allow-unauthenticated-admin"},
	} {
		t.Run(name, func(t *testing.T) {
			clearControlSecretEnv(t)
			tempDir := t.TempDir()
			t.Setenv("SUBROUTER_STATE_DIR", tempDir)
			t.Setenv("SUBROUTER_CLOUD_CONFIG", tempDir) // a directory: read fails past the gate

			args := append([]string{
				"--addr", "100.64.0.5:31415",
				"--fetch-usage=false",
				"--sr-switch-interval=0",
			}, extraArgs...)
			err := serve(args)
			if err == nil {
				t.Fatal("expected the deliberate cloud-config failure")
			}
			if strings.Contains(err.Error(), "refusing to bind") {
				t.Fatalf("gate fired despite %s: %v", name, err)
			}
		})
	}
}

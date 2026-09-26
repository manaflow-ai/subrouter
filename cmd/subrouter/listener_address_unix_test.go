//go:build darwin || linux

package main

import (
	"context"
	"net"
	"strconv"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// skipWithoutIPv6Listen skips tests that need an IPv6 wildcard listener on
// hosts (containers, CI sandboxes) whose kernel has IPv6 disabled. A plain
// listen is the probe, so a host that can listen still runs the full test.
func skipWithoutIPv6Listen(t *testing.T) {
	t.Helper()
	probe, err := net.Listen("tcp6", "[::]:0")
	if err != nil {
		t.Skipf("IPv6 listeners unavailable on this host (%v); dual-stack listener coverage needs one", err)
	}
	_ = probe.Close()
}

func TestListenerCoverageUsesLiveDualStackCapability(t *testing.T) {
	skipWithoutIPv6Listen(t)
	for _, test := range []struct {
		name   string
		v6Only int
		want   bool
	}{
		{name: "dual-stack", v6Only: 0, want: true},
		{name: "IPv6-only", v6Only: 1, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
				var socketErr error
				if err := raw.Control(func(descriptor uintptr) {
					socketErr = unix.SetsockoptInt(
						int(descriptor), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, test.v6Only,
					)
				}); err != nil {
					return err
				}
				return socketErr
			}}
			listener, err := config.Listen(context.Background(), "tcp6", "[::]:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			port := listener.Addr().(*net.TCPAddr).Port
			expected := net.JoinHostPort("0.0.0.0", strconv.Itoa(port))
			if got := listenerCoversConfiguredAddress(listener, expected); got != test.want {
				t.Fatalf("listenerCoversConfiguredAddress(%q, %q) = %t, want %t",
					listener.Addr(), expected, got, test.want)
			}
		})
	}
}

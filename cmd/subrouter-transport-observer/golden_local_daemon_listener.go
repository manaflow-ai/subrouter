package main

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

func goldenLocalDaemonListenAddress(ctx context.Context, pid int) (string, error) {
	if goldenTestHooks.enabled && goldenTestHooks.localDaemonListenAddr != nil {
		address, err := goldenTestHooks.localDaemonListenAddr(ctx, pid)
		if err != nil || strings.TrimSpace(address) == "" {
			return "", err
		}
		return normalizeGoldenLocalDaemonListenAddress(address)
	}
	output, err := exec.CommandContext(
		ctx, "lsof", "-nP", "-a", "-p", strconv.Itoa(pid), "-iTCP", "-sTCP:LISTEN", "-FfnT",
	).Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return "", err
		}
	}
	return parseGoldenLocalDaemonListenAddress(output)
}

func parseGoldenLocalDaemonListenAddress(output []byte) (string, error) {
	addresses := make(map[string]struct{})
	for _, line := range strings.Split(string(output), "\n") {
		if len(line) < 2 || line[0] != 'n' {
			continue
		}
		address, normalizeErr := normalizeGoldenLocalDaemonListenAddress(line[1:])
		if normalizeErr != nil {
			return "", normalizeErr
		}
		addresses[address] = struct{}{}
	}
	if len(addresses) == 0 {
		return "", nil
	}
	if len(addresses) != 1 {
		return "", failGolden("local_daemon_listener_invalid")
	}
	for address := range addresses {
		return address, nil
	}
	return "", nil
}

func normalizeGoldenLocalDaemonListenAddress(raw string) (string, error) {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(raw))
	if err != nil {
		return "", failGolden("local_daemon_listener_invalid")
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	port, err := strconv.Atoi(portText)
	if err != nil || ip == nil || !ip.IsLoopback() || port <= 0 || port > 65535 {
		return "", failGolden("local_daemon_listener_invalid")
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}

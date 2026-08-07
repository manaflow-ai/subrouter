package main

import (
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
)

func validateNumericTCPAddress(address string) error {
	_, err := numericTCPAddressKey(address)
	return err
}

func numericTCPAddressKey(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil {
		return "", fmt.Errorf("host %q is not an IP literal", host)
	}
	value, err := strconv.ParseUint(port, 10, 16)
	if err != nil || value == 0 {
		return "", fmt.Errorf("port %q is not an integer from 1 through 65535", port)
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return fmt.Sprintf("tcp4-%s-%d", hex.EncodeToString(ipv4), value), nil
	}
	return fmt.Sprintf("tcp6-%s-%d", hex.EncodeToString(ip.To16()), value), nil
}

func listenerAddressMatches(actual net.Addr, expected string) bool {
	actualHost, actualPort, err := net.SplitHostPort(actual.String())
	if err != nil {
		return false
	}
	expectedHost, expectedPort, err := net.SplitHostPort(expected)
	if err != nil || actualPort != expectedPort {
		return false
	}
	actualHost, expectedHost = strings.TrimSpace(actualHost), strings.TrimSpace(expectedHost)
	actualIP, expectedIP := net.ParseIP(actualHost), net.ParseIP(expectedHost)
	if expectedHost == "" {
		return actualIP != nil && actualIP.IsUnspecified()
	}
	if actualIP != nil && expectedIP != nil && expectedIP.IsUnspecified() {
		return actualIP.IsUnspecified() && (actualIP.To4() == nil) == (expectedIP.To4() == nil)
	}
	if actualIP != nil && expectedIP != nil {
		return actualIP.Equal(expectedIP)
	}
	return strings.EqualFold(actualHost, expectedHost)
}

// listenerCoversConfiguredAddress accepts an exact address match or a
// dual-stack IPv6 wildcard socket in place of an IPv4 wildcard configuration.
// The latter is safe only when the live socket has IPV6_V6ONLY disabled.
func listenerCoversConfiguredAddress(listener net.Listener, expected string) bool {
	if listener == nil {
		return false
	}
	if listenerAddressMatches(listener.Addr(), expected) {
		return true
	}
	actualHost, actualPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return false
	}
	expectedHost, expectedPort, err := net.SplitHostPort(expected)
	if err != nil || actualPort != expectedPort {
		return false
	}
	actualIP := net.ParseIP(strings.TrimSpace(actualHost))
	expectedIP := net.ParseIP(strings.TrimSpace(expectedHost))
	if actualIP == nil || expectedIP == nil ||
		!actualIP.IsUnspecified() || actualIP.To4() != nil ||
		!expectedIP.IsUnspecified() || expectedIP.To4() == nil {
		return false
	}
	return listenerIPv6WildcardAcceptsIPv4(listener)
}

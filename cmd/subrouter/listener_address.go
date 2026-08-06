package main

import (
	"net"
	"strings"
)

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

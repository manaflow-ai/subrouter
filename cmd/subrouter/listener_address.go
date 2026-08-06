package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

func validateNumericTCPAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if net.ParseIP(strings.TrimSpace(host)) == nil {
		return fmt.Errorf("host %q is not an IP literal", host)
	}
	value, err := strconv.ParseUint(port, 10, 16)
	if err != nil || value == 0 {
		return fmt.Errorf("port %q is not an integer from 1 through 65535", port)
	}
	return nil
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

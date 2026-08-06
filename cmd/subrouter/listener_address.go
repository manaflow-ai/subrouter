package main

import (
	"net"
	"strconv"
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
	expectedHost = strings.TrimSpace(expectedHost)
	if expectedHost == "" || expectedHost == "0.0.0.0" || expectedHost == "::" {
		return true
	}
	actualIP, expectedIP := net.ParseIP(actualHost), net.ParseIP(expectedHost)
	if actualIP != nil && expectedIP != nil {
		return actualIP.Equal(expectedIP)
	}
	actualPortNumber, actualErr := strconv.Atoi(actualPort)
	expectedPortNumber, expectedErr := strconv.Atoi(expectedPort)
	return actualErr == nil && expectedErr == nil && actualPortNumber == expectedPortNumber &&
		strings.EqualFold(actualHost, expectedHost)
}

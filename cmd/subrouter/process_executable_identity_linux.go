//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

func executableIdentityForProcess(pid int) (processExecutableIdentity, error) {
	file, err := os.Open(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return processExecutableIdentity{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return processExecutableIdentity{}, err
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return processExecutableIdentity{}, err
	}
	// The comm field is enclosed in parentheses, but Linux permits the
	// executable name to contain both spaces and closing parentheses. Use the
	// final closing delimiter so those names do not shift the field indexes.
	statText := string(stat)
	closing := strings.LastIndex(statText, ") ")
	if closing < 0 {
		return processExecutableIdentity{}, fmt.Errorf("invalid process stat for pid %d", pid)
	}
	fields := strings.Fields(statText[closing+2:])
	if len(fields) < 20 {
		return processExecutableIdentity{}, fmt.Errorf("invalid process stat for pid %d", pid)
	}
	return processExecutableIdentity{Kind: "linux-proc-exe-sha256", Value: hex.EncodeToString(hash.Sum(nil)), StartIdentity: "linux:" + fields[19]}, nil
}

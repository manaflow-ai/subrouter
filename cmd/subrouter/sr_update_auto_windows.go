//go:build windows

package main

import "syscall"

// detachedProcessAttributes is unused on Windows, where automatic updates
// are not started (maybeStartAutoUpdate).
func detachedProcessAttributes() *syscall.SysProcAttr { return nil }

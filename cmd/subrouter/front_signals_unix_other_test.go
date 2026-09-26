//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || netbsd || openbsd || solaris

package main

import (
	"syscall"
	"testing"
)

func TestFrontDoesNotInterceptReloadSignalWithoutOwnershipHandoff(t *testing.T) {
	for _, signal := range frontProcessSignals() {
		if signal == syscall.SIGHUP {
			t.Fatal("front intercepted SIGHUP on a platform without process-manager ownership handoff")
		}
	}
}

//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || netbsd || openbsd || solaris

package main

import (
	"os"
	"syscall"
)

func frontProcessSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

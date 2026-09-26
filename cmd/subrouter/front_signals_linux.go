//go:build linux

package main

import (
	"os"
	"syscall"
)

func frontProcessSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
}

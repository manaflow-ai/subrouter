//go:build !windows

package main

import (
	"os"
	"syscall"
)

// retireSignal tells a worker that a newer generation has taken over new
// connections, so it should stop reusing the ones it still holds. SIGUSR1 is
// chosen because the supervisor already uses SIGTERM for shutdown and SIGHUP
// for upgrade, and retirement must not mean either.
var retireSignal os.Signal = syscall.SIGUSR1

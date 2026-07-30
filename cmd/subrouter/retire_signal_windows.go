//go:build windows

package main

import "os"

// Windows has no SIGUSR1, so there is no signal that means "retired" without
// also meaning "shut down". The supervisor runs under launchd/systemd on unix
// in practice; on Windows a retired worker simply keeps serving its existing
// connections as it did before, and callers skip the retire step when this is
// nil. Release builds cross-compile for Windows, so this has to compile.
var retireSignal os.Signal = nil

//go:build darwin

package cutovercanary

import (
	"encoding/hex"
	"os"
	"syscall"
	"unsafe"
)

const csOpsCDHash = 5

func captureRunningExecutableIdentity() executableIdentity {
	// csops asks the kernel for the code-directory hash attached to this exact
	// running process image. Unlike reopening os.Executable(), an atomic path
	// replacement after exec cannot change this identity.
	digest := make([]byte, 20)
	_, _, errno := syscall.Syscall6(
		syscall.SYS_CSOPS,
		uintptr(os.Getpid()),
		csOpsCDHash,
		uintptr(unsafe.Pointer(&digest[0])),
		uintptr(len(digest)),
		0,
		0,
	)
	if errno != 0 {
		return executableIdentity{}
	}
	return executableIdentity{Kind: darwinCDHashIdentityKind, Value: hex.EncodeToString(digest)}
}

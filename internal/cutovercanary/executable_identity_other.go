//go:build !darwin

package cutovercanary

import (
	"crypto/sha256"
	"encoding/hex"
	"runtime/debug"
)

func captureRunningExecutableIdentity() executableIdentity {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return executableIdentity{}
	}
	digest := sha256.Sum256([]byte(info.String()))
	return executableIdentity{Kind: goBuildInfoIdentityKind, Value: hex.EncodeToString(digest[:])}
}

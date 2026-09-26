//go:build !darwin && !linux

package main

func executableIdentityForProcess(_ int) (processExecutableIdentity, error) {
	// Ordinary supervision remains supported on release platforms without a
	// kernel-bound implementation. This value is explicitly non-attesting: the
	// macOS activation transaction requires Kind == darwin-cdhash-sha256 and
	// therefore cannot mistake this placeholder for cutover evidence.
	return processExecutableIdentity{
		Kind:          "unsupported",
		Value:         "unsupported",
		StartIdentity: "unsupported",
	}, nil
}

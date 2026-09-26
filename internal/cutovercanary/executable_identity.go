package cutovercanary

import "errors"

const (
	darwinCDHashIdentityKind = "darwin-cdhash-sha256"
	goBuildInfoIdentityKind  = "go-build-info-sha256"
)

type executableIdentity struct {
	Kind  string
	Value string
}

var runningExecutableIdentity = captureRunningExecutableIdentity()

func currentExecutableIdentity() (executableIdentity, error) {
	if runningExecutableIdentity.Kind == "" || runningExecutableIdentity.Value == "" {
		return executableIdentity{}, errors.New("running executable identity unavailable")
	}
	return runningExecutableIdentity, nil
}

func validExecutableIdentity(kind, value string) bool {
	switch kind {
	case darwinCDHashIdentityKind:
		return len(value) == 40 && isLowerHex(value)
	case goBuildInfoIdentityKind:
		return validSHA256(value)
	default:
		return false
	}
}

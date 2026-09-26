package accounts

import (
	"os"
	"testing"
)

// A developer shell with a host identity would stamp claims into every test's
// account files; tests that need one set it themselves.
func TestMain(m *testing.M) {
	os.Unsetenv(HostIDEnv)
	os.Exit(m.Run())
}

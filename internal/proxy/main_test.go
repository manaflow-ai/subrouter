package proxy

import (
	"os"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// A developer shell with a host identity would stamp claims into every test's
// account files; tests that need one set it themselves.
func TestMain(m *testing.M) {
	os.Unsetenv(accounts.HostIDEnv)
	os.Exit(m.Run())
}

package proxy

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// A developer shell with a host identity would stamp claims into every test's
// account files; tests that need one set it themselves.
func TestMain(m *testing.M) {
	os.Unsetenv(accounts.HostIDEnv)
	code := m.Run()
	if seedTemplate.dir != "" {
		os.RemoveAll(seedTemplate.dir)
	}
	os.Exit(code)
}

// seedTemplate holds maxAccountImportAccounts API-key accounts saved once
// through CodexStore.SaveStored. SaveStored re-reads every stored account on
// each call, so filling a store to capacity one save at a time is quadratic,
// and the capacity tests doing that made this the slowest package in CI.
var seedTemplate struct {
	once  sync.Once
	dir   string
	files []string
	err   error
}

// seedCodexAPIKeyAccounts gives store the accounts apikey:seed-000 through
// apikey:seed-<count-1>, each with key sk-seed-NNN, as saving them with
// SaveStored in order would. It copies the account files from a template
// built once per test binary.
func seedCodexAPIKeyAccounts(t testing.TB, store accounts.CodexStore, count int) {
	t.Helper()
	if count < 0 || count > maxAccountImportAccounts {
		t.Fatalf("seed count %d outside 0..%d", count, maxAccountImportAccounts)
	}
	seedTemplate.once.Do(buildSeedTemplate)
	if seedTemplate.err != nil {
		t.Fatal(seedTemplate.err)
	}
	if err := os.MkdirAll(store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range seedTemplate.files[:count] {
		if err := copySeedFile(filepath.Join(seedTemplate.dir, name), filepath.Join(store.Dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func buildSeedTemplate() {
	root, err := os.MkdirTemp("", "subrouter-proxy-seed-")
	if err != nil {
		seedTemplate.err = err
		return
	}
	seedTemplate.dir = root
	store := accounts.CodexStore{Dir: root}
	for index := 0; index < maxAccountImportAccounts; index++ {
		account := accounts.StoredCodexAccount{
			Email:    fmt.Sprintf("apikey:seed-%03d", index),
			Provider: accounts.ProviderCodex,
			Auth: accounts.CodexAuthFile{
				AuthMode: "apikey", OpenAIAPIKey: fmt.Sprintf("sk-seed-%03d", index),
			},
		}
		if err := store.SaveStored(account); err != nil {
			seedTemplate.err = err
			return
		}
		seedTemplate.files = append(seedTemplate.files, filepath.Base(account.SourcePath(store)))
	}
}

func copySeedFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

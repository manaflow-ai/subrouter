package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/fsutil"
)

// A provider-rejected single-use refresh chain cannot recover by repeating
// the same request. Keep this tiny negative marker across Mac-mini worker
// restarts. Each credential gets a separate atomic file so concurrent workers
// never overwrite other accounts' failures. No token or upstream body is saved.
func (r *AccountRef) claudeTerminalRefreshMarkerPath(account accounts.Account) string {
	if r == nil || account.Provider != accounts.ProviderClaude || r.claudeStore.Dir == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(credFailureKey(account)))
	return filepath.Join(r.claudeStore.Dir, "terminal-refresh", hex.EncodeToString(digest[:])+".status")
}

func (r *AccountRef) persistedClaudeTerminalFailure(account accounts.Account) (string, bool) {
	path := r.claudeTerminalRefreshMarkerPath(account)
	if path == "" {
		return "", false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	switch reason := strings.TrimSpace(string(body)); reason {
	case "invalid_grant", "refresh_token_reused":
		return reason, true
	default:
		return "", false
	}
}

func (r *AccountRef) persistClaudeTerminalFailure(account accounts.Account, err error) {
	if err == nil || !irreparableClaudeRefreshFailure(err) {
		return
	}
	path := r.claudeTerminalRefreshMarkerPath(account)
	if path == "" {
		return
	}
	reason := "invalid_grant"
	if strings.Contains(strings.ToLower(err.Error()), "refresh_token_reused") {
		reason = "refresh_token_reused"
	}
	if mkdirErr := os.MkdirAll(filepath.Dir(path), 0o700); mkdirErr != nil {
		slog.Warn("cannot persist terminal Claude credential state", "error", mkdirErr)
		return
	}
	if writeErr := fsutil.WriteFileAtomic(path, []byte(reason+"\n"), 0o600); writeErr != nil {
		slog.Warn("cannot persist terminal Claude credential state", "error", writeErr)
	}
}

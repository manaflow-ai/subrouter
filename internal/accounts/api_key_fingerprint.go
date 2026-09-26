package accounts

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// APIKeyFingerprint returns a stable, non-secret identifier suitable for
// distinguishing multiple labeled keys in status output. It deliberately
// hashes the whole key instead of exposing a prefix or suffix issued by the
// provider.
func APIKeyFingerprint(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return "key:" + hex.EncodeToString(sum[:5])
}

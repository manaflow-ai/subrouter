package accounts

import (
	"strings"
	"testing"
)

func TestAPIKeyFingerprintIsStableDistinctAndRedacted(t *testing.T) {
	first := APIKeyFingerprint("sk-sp-first-secret")
	if first == "" || first != APIKeyFingerprint("  sk-sp-first-secret\n") {
		t.Fatalf("fingerprint must be stable after trimming: %q", first)
	}
	if first == APIKeyFingerprint("sk-sp-second-secret") {
		t.Fatal("different keys must have different fingerprints")
	}
	for _, fragment := range []string{"sk-sp", "first", "secret"} {
		if strings.Contains(first, fragment) {
			t.Fatalf("fingerprint %q leaked key fragment %q", first, fragment)
		}
	}
	if got := APIKeyFingerprint("  "); got != "" {
		t.Fatalf("blank key fingerprint = %q", got)
	}
}

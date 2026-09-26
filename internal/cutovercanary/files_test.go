package cutovercanary

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExecutableAttestationIsBoundToRunningImage(t *testing.T) {
	first, err := currentExecutableIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if !validExecutableIdentity(first.Kind, first.Value) {
		t.Fatalf("invalid running executable identity: %+v", first)
	}
	second, err := currentExecutableIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("running executable identity changed")
	}
}

func TestCanaryWritersPreserveUnrecognizedExistingFiles(t *testing.T) {
	dir := privateDir(t)
	path := filepath.Join(dir, "foreign.json")
	original := []byte("preserve me\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	value := proof{Schema: ProofSchema, Leg: "peer-health-readiness", OK: true}
	if err := writePrivateJSON(path, value); err == nil {
		t.Fatal("unrecognized existing file was overwritten")
	}
	if err := removeIfExists(path); err == nil {
		t.Fatal("unrecognized existing file was deleted")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(original) {
		t.Fatalf("foreign file changed: %q / %v", got, err)
	}
}

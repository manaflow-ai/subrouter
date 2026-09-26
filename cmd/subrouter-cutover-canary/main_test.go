package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/cutovercanary"
)

func TestPeerProbeSubcommandPrintsBoundedContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_subrouter/health" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"draining":false}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "probe.json")
	b, _ := json.Marshal(cutovercanary.PeerProbeConfig{Schema: cutovercanary.ConfigSchema, HTTP: cutovercanary.HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1024}})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"peer-probe", "--config", path}, bytes.NewReader(nil), &out); err != nil {
		t.Fatal(err)
	}
	var result cutovercanary.PeerProbeResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Schema != cutovercanary.PeerProbeSchema || !result.OK || !result.HealthOK || !result.ReadyOK || result.Draining || result.IdentityKind == "" || result.ExecutableIdentity == "" {
		t.Fatalf("unexpected output: %#v", result)
	}
}

func TestLegResultHasExactRunnerKeys(t *testing.T) {
	var out bytes.Buffer
	if err := cutovercanary.ServeLegResult(&out, "sticky-reuse"); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["schema"] != cutovercanary.LegSchema || got["leg"] != "sticky-reuse" || got["ok"] != true {
		t.Fatalf("unexpected runner result: %#v", got)
	}
}

func TestHelpDescribesDirectAndEnvironmentDrivenModes(t *testing.T) {
	for _, args := range [][]string{
		{"--help"},
		{"-help"},
		{"peer-probe", "--help"},
		{"witness", "-h"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var out bytes.Buffer
			if err := run(args, bytes.NewReader(nil), &out); err != nil {
				t.Fatal(err)
			}
			for _, expected := range []string{
				"SUBROUTER_CANARY_LEG_NAME",
				"peer-probe --config FILE",
				"witness --challenge FILE --witness FILE",
			} {
				if !bytes.Contains(out.Bytes(), []byte(expected)) {
					t.Fatalf("help missing %q:\n%s", expected, out.String())
				}
			}
		})
	}
}

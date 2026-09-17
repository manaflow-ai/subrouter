package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// A rejected upload used to surface as a bare "400 Bad Request". The server's
// reason must reach the user, and the outcome line must be visibly a failure.
func TestServerAccountImportFailureSurfacesServerReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"providers":["codex"]}`))
			return
		}
		http.Error(w, "invalid account import body: unknown field \"oauthCredentialOrigin\"\n\tsecond line\x07", http.StatusBadRequest)
	}))
	defer server.Close()

	runner := srRunner{client: server.Client(), out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	account := accounts.StoredCodexAccount{Email: "owner@example.com"}
	err := runner.postServerAccountImport(context.Background(), srServerConfig{
		Name:               "team",
		URL:                server.URL,
		AccountImportToken: "scoped-import-secret",
	}, serverAccountImportRequest{Provider: accounts.ProviderCodex, Codex: &account})
	if err == nil {
		t.Fatal("expected the 400 to fail the upload")
	}
	got := err.Error()
	want := `server team account import failed: 400 Bad Request: invalid account import body: unknown field "oauthCredentialOrigin" second line`
	if got != want {
		t.Fatalf("error = %q\nwant    %q", got, want)
	}
}

func TestServerAccountImportFailureReasonIsOneCleanLine(t *testing.T) {
	long := strings.Repeat("x", 600)
	cases := map[string]string{
		"":               "",
		"  \n ":          "",
		"plain reason\n": "plain reason",
		"a\r\nb\tc":      "a b c",
		"esc\x1b[31mape": "esc[31mape",
		long:             long[:512] + "...",
	}
	for input, want := range cases {
		if got := serverAccountImportFailureReason([]byte(input), nil); got != want {
			t.Errorf("reason(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestUploadOutcomeLineColorsSuccessGreenAndFailureRed(t *testing.T) {
	if got := uploadOutcomeLine(true, true, "Uploaded a to server b."); got != ansiGreen+"✓ Uploaded a to server b."+ansiReset {
		t.Fatalf("success line = %q", got)
	}
	if got := uploadOutcomeLine(true, false, "Upload of a to server b failed."); got != ansiRed+"✗ Upload of a to server b failed."+ansiReset {
		t.Fatalf("failure line = %q", got)
	}
	if got := uploadOutcomeLine(false, false, "Upload of a to server b failed."); got != "✗ Upload of a to server b failed." {
		t.Fatalf("uncolored failure line = %q", got)
	}
}

func TestPrintUploadOutcomeRoutesFailureToErrOut(t *testing.T) {
	var out, errOut bytes.Buffer
	runner := srRunner{out: &out, errOut: &errOut}
	runner.printUploadOutcome(true, "Uploaded a to server b.")
	runner.printUploadOutcome(false, "Upload of a to server b failed.")
	if !strings.Contains(out.String(), "✓ Uploaded a to server b.") || strings.Contains(out.String(), "failed") {
		t.Fatalf("stdout = %q", out.String())
	}
	if !strings.Contains(errOut.String(), "✗ Upload of a to server b failed.") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestServerAccountImportFailureReasonRedactsSubmittedSecrets(t *testing.T) {
	submitted := []byte(`{"provider":"codex","codex":{"email":"a@b.co","label":"pw1","oauthCredentialOrigin":"isolated-server-login","auth":{"tokens":{"access_token":"eyJhbGciOiJSUzI1NiJ9.payload.sig","refresh_token":"rt-very-secret-value","id_token":"eyJhbGciOiJSUzI1NiJ9.other.sig"},"auth_mode":"chatgpt"}}}`)
	body := "provider rejected rt-very-secret-value for a@b.co label pw1 via sk-live-abcdef and eyJhbGciOiJSUzI1NiJ9.payload.sig; mode chatgpt origin isolated-server-login provider codex"
	got := serverAccountImportFailureReason([]byte(body), submitted)
	want := "provider rejected [redacted] for [redacted] label [redacted] via [redacted] and [redacted]; mode chatgpt origin isolated-server-login provider codex"
	if got != want {
		t.Fatalf("reason = %q\nwant   %q", got, want)
	}
}

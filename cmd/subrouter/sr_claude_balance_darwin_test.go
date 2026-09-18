//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafariClaudeSessionKeys(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "Library/Containers/com.apple.Safari/Data/Library/Cookies")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	fixture := buildBinaryCookiesFixture(
		binaryCookieRecord(".claude.ai", "sessionKey", "/", "sk-ant-safari-key"),
	)
	if err := os.WriteFile(filepath.Join(dir, "Cookies.binarycookies"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
	got := safariClaudeSessionKeys(home)
	if len(got) != 1 || got[0].SessionKey != "sk-ant-safari-key" || got[0].Source != "Safari" {
		t.Fatalf("safariClaudeSessionKeys = %+v", got)
	}
}

func TestSafariClaudeSessionKeysSoftFail(t *testing.T) {
	home := t.TempDir()
	// Missing files and unreadable/corrupt files must not fail the chain.
	if got := safariClaudeSessionKeys(home); len(got) != 0 {
		t.Fatalf("missing file: %+v", got)
	}
	dir := filepath.Join(home, "Library/Cookies")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Cookies.binarycookies"), []byte("junk"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := safariClaudeSessionKeys(home); len(got) != 0 {
		t.Fatalf("corrupt file: %+v", got)
	}
}

// TestProbeClaudeWebTransportNotReady: with no compiled binary the transport
// reports not-ready without doing any work. swiftc is pointed at a missing
// path so the kicked-off background compile exits immediately.
func TestProbeClaudeWebTransportNotReady(t *testing.T) {
	claudeWebTestEnv(t)
	oldSwiftc := claudeWebSwiftcPath
	claudeWebSwiftcPath = filepath.Join(t.TempDir(), "no-swiftc")
	t.Cleanup(func() { claudeWebSwiftcPath = oldSwiftc })

	_, err := probeClaudeWebTransport(context.Background(), "http://127.0.0.1/", "sk-ant-test")
	if !errors.Is(err, errClaudeWebProbeNotReady) {
		t.Fatalf("err = %v", err)
	}
	if claudeWebProbeReady() {
		t.Fatal("probe must not be ready without a compiled binary")
	}
	// Wait for the kicked-off background compile so it cannot outlive the
	// temp dirs this test set up.
	<-claudeWebProbeCompileDone
}

// TestProbeClaudeWebTransportRoundTrip compiles the real Swift probe with
// swiftc -O and runs it against a local httptest server. Skipped when swiftc
// is unavailable; takes ~15-30s for the compile.
func TestProbeClaudeWebTransportRoundTrip(t *testing.T) {
	if _, err := os.Stat(claudeWebSwiftcPath); err != nil {
		t.Skip("swiftc not available")
	}
	claudeWebTestEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("sessionKey")
		if err != nil || !strings.HasPrefix(cookie.Value, "sk-ant-") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "sessionKey", Value: "sk-ant-probe-rotated"})
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"email_address":"probe@example.com"}`)
	}))
	defer server.Close()

	path := claudeWebProbeBinaryPath()
	compileClaudeWebProbe(path)
	if !claudeWebProbeBinaryExists(path) {
		t.Fatal("probe compile did not produce a binary")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("probe binary mode = %o", info.Mode().Perm())
	}

	resp, err := probeClaudeWebTransport(context.Background(), server.URL+"/account", "sk-ant-probe")
	if err != nil {
		t.Fatal(err)
	}
	if resp.status != http.StatusOK {
		t.Fatalf("status = %d", resp.status)
	}
	if !strings.Contains(string(resp.body), "probe@example.com") {
		t.Fatalf("body = %q", resp.body)
	}
	if got := claudeWebRotatedSessionKey(resp.setCookie); got != "sk-ant-probe-rotated" {
		t.Fatalf("rotated key = %q", got)
	}
}

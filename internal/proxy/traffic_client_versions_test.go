package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestNormalizeClientVersion(t *testing.T) {
	for value, want := range map[string]string{
		"v0.1.174":                 "v0.1.174",
		"  v0.1.174 ":              "v0.1.174",
		"devel+abc123def456-dirty": "devel+abc123def456-dirty",
		"0.2.0_rc1":                "0.2.0_rc1",
		"":                         "",
		"-v1":                      "",
		"v1 beta":                  "",
		"v1/../x":                  "",
		"v1\r\nX-Evil: 1":          "",
		"unknown":                  "",
		"other":                    "",
		strings.Repeat("9", 49):    "",
	} {
		if got := NormalizeClientVersion(value); got != want {
			t.Errorf("NormalizeClientVersion(%q) = %q, want %q", value, got, want)
		}
	}
}

// The histogram keeps at most trafficClientVersionCap real versions. Later
// versions count as "other"; a version already kept keeps counting; a missing
// or invalid one is "unknown" and never takes a slot.
func TestTrafficClientVersionHistogramIsBounded(t *testing.T) {
	stats := NewTrafficStats(time.Now())
	handler := stats.trafficCounted(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	send := func(version string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		if version != "" {
			req.Header.Set(ClientVersionHeader, version)
		}
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}
	send("")
	send("not a version!")
	for i := 0; i < trafficClientVersionCap; i++ {
		send(fmt.Sprintf("v0.1.%d", i))
	}
	send("v0.1.0")
	send("v9.9.9")
	send("v9.9.10")

	// Control requests are not traffic and carry no version count.
	control := httptest.NewRequest(http.MethodGet, "/_subrouter/traffic", nil)
	control.Header.Set(ClientVersionHeader, "v0.1.0")
	handler.ServeHTTP(httptest.NewRecorder(), control)

	got := stats.Snapshot(nil, time.Now()).ClientVersions
	if len(got) != trafficClientVersionCap+2 {
		t.Fatalf("len(client_versions) = %d, want %d: %v", len(got), trafficClientVersionCap+2, got)
	}
	if got["unknown"] != 2 || got["other"] != 2 || got["v0.1.0"] != 2 || got["v0.1.63"] != 1 {
		t.Fatalf("client_versions = %v", got)
	}
	if _, ok := got["v9.9.9"]; ok {
		t.Fatalf("a version past the cap took its own bucket: %v", got)
	}
}

// A request through the proxy is counted under its client version in
// /_subrouter/traffic, and the header never reaches the upstream provider.
func TestProxyCountsClientVersionAndStripsItUpstream(t *testing.T) {
	leaked := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked <- r.Header.Get(ClientVersionHeader)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID: "codex-a", AuthMode: accounts.AuthModeOAuth, Token: "token-a", AccountID: "acct-a",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1 << 20,
		Traffic:      NewTrafficStats(time.Now()),
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	for _, version := range []string{"v0.1.174", "v0.1.174", ""} {
		request, _ := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5-codex","input":"hi"}`))
		request.Header.Set("Content-Type", "application/json")
		if version != "" {
			request.Header.Set(ClientVersionHeader, version)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if got := <-leaked; got != "" {
			t.Fatalf("%s leaked upstream: %q", ClientVersionHeader, got)
		}
	}

	response, err := http.Get(subrouter.URL + "/_subrouter/traffic")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body struct {
		ClientVersions map[string]uint64 `json:"client_versions"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.ClientVersions) != 2 || body.ClientVersions["v0.1.174"] != 2 || body.ClientVersions["unknown"] != 1 {
		t.Fatalf("client_versions = %v, want v0.1.174:2 unknown:1", body.ClientVersions)
	}
}

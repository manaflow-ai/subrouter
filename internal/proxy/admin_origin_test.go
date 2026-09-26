package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/tenant"
	"github.com/manaflow-ai/subrouter/session"
)

func newAdminOriginTestServer(t *testing.T) Server {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return Server{
		Sessions:     store,
		Lifecycle:    NewLifecycle(),
		MaxBodyBytes: 1024,
		AdminToken:   "admin-secret",
	}
}

func loopbackAdminRequest(method, path, host string, headers map[string]string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = host
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return req
}

func TestLoopbackAdminRejectsBrowserCrossSiteRequests(t *testing.T) {
	handler := newAdminOriginTestServer(t).Handler()
	for _, test := range []struct {
		name    string
		path    string
		host    string
		headers map[string]string
	}{
		{"cross-site origin", "/_subrouter/quiesce", "127.0.0.1:31415", map[string]string{"Origin": "https://evil.example"}},
		{"null origin", "/_subrouter/quiesce", "127.0.0.1:31415", map[string]string{"Origin": "null"}},
		{"sec-fetch-site cross-site", "/_subrouter/rate-limit-reset", "localhost:31415", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"sec-fetch-site same-site", "/_subrouter/drain", "localhost:31415", map[string]string{"Sec-Fetch-Site": "same-site"}},
		{"non-loopback host", "/_subrouter/quiesce", "rebind.evil.example:31415", nil},
		{"non-loopback host with matching origin", "/_subrouter/drain", "attacker.example", map[string]string{"Origin": "http://attacker.example"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, loopbackAdminRequest(http.MethodPost, test.path, test.host, test.headers))
			if resp.Code != http.StatusForbidden {
				t.Fatalf("%s status = %d, want 403; body = %s", test.path, resp.Code, resp.Body.String())
			}
		})
	}
}

func TestLoopbackAdminAllowsLocalClients(t *testing.T) {
	handler := newAdminOriginTestServer(t).Handler()
	for _, test := range []struct {
		name    string
		host    string
		headers map[string]string
	}{
		{"cli ipv4", "127.0.0.1:31415", nil},
		{"cli localhost", "localhost:31415", nil},
		{"cli ipv6", "[::1]:31415", nil},
		{"cli bare localhost", "localhost", nil},
		{"dashboard same-origin fetch", "127.0.0.1:31415", map[string]string{"Origin": "http://127.0.0.1:31415", "Sec-Fetch-Site": "same-origin"}},
		{"typed-in navigation", "localhost:31415", map[string]string{"Sec-Fetch-Site": "none"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, loopbackAdminRequest(http.MethodGet, "/_subrouter/drain-status", test.host, test.headers))
			if resp.Code != http.StatusOK {
				t.Fatalf("drain-status status = %d, want 200; body = %s", resp.Code, resp.Body.String())
			}
		})
	}
}

func TestLoopbackAdminAcceptsConfiguredPublicHost(t *testing.T) {
	server := newAdminOriginTestServer(t)
	server.PublicURL = "https://Subrouter.Example.test/"
	handler := server.Handler()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, loopbackAdminRequest(http.MethodGet, "/_subrouter/drain-status", "subrouter.example.test", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("public host status = %d, want 200", resp.Code)
	}

	resp = httptest.NewRecorder()
	req := loopbackAdminRequest(http.MethodGet, "/_subrouter/drain-status", "other.example.test", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("token-bearing loopback request with other host status = %d, want 200", resp.Code)
	}
}

func TestAdminTokenRequestRejectsCrossSiteOrigin(t *testing.T) {
	handler := newAdminOriginTestServer(t).Handler()
	req := httptest.NewRequest(http.MethodGet, "/_subrouter/drain-status", nil)
	req.RemoteAddr = "100.64.0.9:1234"
	req.Host = "box.tailnet.ts.net:31415"
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("token request status = %d, want 200", resp.Code)
	}

	req.Header.Set("Origin", "https://evil.example")
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("cross-site token request status = %d, want 403", resp.Code)
	}
}

func TestLoopbackSessionLeaseAdminRejectsCrossSiteOrigin(t *testing.T) {
	handler := newAdminOriginTestServer(t).Handler()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, loopbackAdminRequest(http.MethodPost, "/internal/v1/session-leases", "127.0.0.1:31415", map[string]string{"Origin": "https://evil.example"}))
	if resp.Code != http.StatusForbidden {
		t.Fatalf("session lease status = %d, want 403; body = %s", resp.Code, resp.Body.String())
	}
}

func TestLoopbackModelProxyUnaffectedByAdminBrowserChecks(t *testing.T) {
	handler := newAdminOriginTestServer(t).Handler()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, loopbackAdminRequest(http.MethodHead, "/", "127.0.0.1:31415", nil))
	if resp.Code != http.StatusNoContent {
		t.Fatalf("base url probe status = %d, want 204", resp.Code)
	}
}

func TestTenantAccountStatusRefreshRequiresManageAccounts(t *testing.T) {
	registry, handler, _ := newMultiTenantFixture(t)
	const tenantID = "status-scope"
	const useKey = "srt_33333333333333333333333333333333"
	const manageKey = "srt_44444444444444444444444444444444"
	if _, err := registry.EnsureExternalRestricted(tenantID, "status", useKey, []tenant.Capability{tenant.CapabilityUse}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.EnsureExternalRestricted(tenantID, "status", manageKey, []tenant.Capability{tenant.CapabilityManageAccounts}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		key    string
		method string
		want   int
	}{
		{"use get", useKey, http.MethodGet, http.StatusOK},
		{"use post", useKey, http.MethodPost, http.StatusForbidden},
		{"manage get", manageKey, http.MethodGet, http.StatusOK},
		{"manage post", manageKey, http.MethodPost, http.StatusOK},
	} {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, httptest.NewRequest(test.method, "/t/"+test.key+"/_subrouter/account-status", nil))
		if resp.Code != test.want {
			t.Fatalf("%s account-status status = %d, want %d; body = %s", test.name, resp.Code, test.want, resp.Body.String())
		}
	}
}

// TestDashboardLinkNavigationAllowedButNotCrossSiteFetch asserts a link to
// the dashboard from another site opens it, while a cross-site fetch or a
// cross-site navigation to any other admin endpoint is still refused.
func TestDashboardLinkNavigationAllowedButNotCrossSiteFetch(t *testing.T) {
	nav := func(method, path, mode, dest string) *http.Request {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Sec-Fetch-Mode", mode)
		r.Header.Set("Sec-Fetch-Dest", dest)
		return r
	}
	if !adminBrowserSignalsAllowed(nav(http.MethodGet, "/_subrouter/dashboard", "navigate", "document")) {
		t.Fatal("link navigation to the dashboard was refused")
	}
	for _, r := range []*http.Request{
		nav(http.MethodGet, "/_subrouter/dashboard", "no-cors", "empty"),
		nav(http.MethodPost, "/_subrouter/dashboard", "navigate", "document"),
		nav(http.MethodGet, "/_subrouter/usage-status", "navigate", "document"),
		nav(http.MethodGet, "/_subrouter/dashboard", "navigate", "iframe"),
	} {
		if adminBrowserSignalsAllowed(r) {
			t.Fatalf("%s %s mode=%s dest=%s was allowed", r.Method, r.URL.Path, r.Header.Get("Sec-Fetch-Mode"), r.Header.Get("Sec-Fetch-Dest"))
		}
	}
}

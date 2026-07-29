package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func respWithCookies(values ...string) *http.Response {
	h := http.Header{}
	for _, v := range values {
		h.Add("Set-Cookie", v)
	}
	return &http.Response{Header: h}
}

func cookieHeader(t *testing.T, jar *upstreamCookieJar, key string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/ps/plugins/list", nil)
	jar.apply(key, r)
	return r.Header.Get("Cookie")
}

// The whole point: a cookie captured from one upstream response must ride along
// on the next request, or Cloudflare routes it to a different backend.
func TestUpstreamCookiesAreReplayedOnNextRequest(t *testing.T) {
	jar := newUpstreamCookieJar()
	key := upstreamCookieKey("acct-alice", "chatgpt.com")
	jar.capture(key, respWithCookies("__cflb=west-7; Path=/; Secure; HttpOnly"))

	if got := cookieHeader(t, jar, key); got != "__cflb=west-7" {
		t.Fatalf("Cookie header = %q, want __cflb=west-7", got)
	}
}

// Only Cloudflare infrastructure cookies. Session and auth cookies identify a
// user and must never be replayed by an intermediary.
func TestUpstreamCookiesRejectNonCloudflareCookies(t *testing.T) {
	jar := newUpstreamCookieJar()
	key := upstreamCookieKey("acct-alice", "chatgpt.com")
	jar.capture(key, respWithCookies(
		"__cflb=west-7; Path=/",
		"__Secure-next-auth.session-token=secret; Path=/",
		"chatgpt_session=secret; Path=/",
		"oai-auth-token=secret; Path=/",
	))

	got := cookieHeader(t, jar, key)
	if got != "__cflb=west-7" {
		t.Fatalf("Cookie header = %q, want only the Cloudflare cookie", got)
	}
}

func TestUpstreamCookiesAllowlistMatchesCodex(t *testing.T) {
	allowed := []string{
		"__cf_bm", "__cflb", "__cfruid", "__cfseq", "__cfwaitingroom",
		"_cfuvid", "cf_clearance", "cf_ob_info", "cf_use_ob", "cf_chl_rc_i",
	}
	for _, name := range allowed {
		if !isCloudflareInfraCookie(name) {
			t.Errorf("isCloudflareInfraCookie(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"__Secure-next-auth.session-token", "chatgpt_session", "oai-auth-token", "not_cf_clearance",
	} {
		if isCloudflareInfraCookie(name) {
			t.Errorf("isCloudflareInfraCookie(%q) = true, want false", name)
		}
	}
}

// Two accounts must never share backend affinity, or their traffic is
// correlatable and one account's clearance is replayed for another.
func TestUpstreamCookiesAreScopedPerAccountAndHost(t *testing.T) {
	jar := newUpstreamCookieJar()
	alice := upstreamCookieKey("acct-alice", "chatgpt.com")
	bob := upstreamCookieKey("acct-bob", "chatgpt.com")
	otherHost := upstreamCookieKey("acct-alice", "api.openai.com")
	jar.capture(alice, respWithCookies("__cflb=west-7; Path=/"))

	if got := cookieHeader(t, jar, bob); got != "" {
		t.Fatalf("bob got alice's cookie: %q", got)
	}
	if got := cookieHeader(t, jar, otherHost); got != "" {
		t.Fatalf("api.openai.com got the chatgpt.com cookie: %q", got)
	}
}

func TestUpstreamCookiesHonorExpiryAndDeletion(t *testing.T) {
	jar := newUpstreamCookieJar()
	key := upstreamCookieKey("acct-alice", "chatgpt.com")

	jar.capture(key, respWithCookies("__cflb=stale; Max-Age=-1"))
	if got := cookieHeader(t, jar, key); got != "" {
		t.Fatalf("cleared cookie was stored: %q", got)
	}

	jar.capture(key, respWithCookies("__cf_bm=short; Max-Age=1"))
	if got := cookieHeader(t, jar, key); got != "__cf_bm=short" {
		t.Fatalf("Cookie header = %q, want __cf_bm=short", got)
	}
	jar.mu.Lock()
	jar.entries[key]["__cf_bm"] = upstreamCookie{value: "short", expiresAt: time.Now().Add(-time.Second)}
	jar.mu.Unlock()
	if got := cookieHeader(t, jar, key); got != "" {
		t.Fatalf("expired cookie was replayed: %q", got)
	}
}

// The client knows its own session; a proxy-held cookie must not overwrite one
// the client sent itself.
func TestUpstreamCookiesDoNotClobberClientCookies(t *testing.T) {
	jar := newUpstreamCookieJar()
	key := upstreamCookieKey("acct-alice", "chatgpt.com")
	jar.capture(key, respWithCookies("__cflb=proxy-side; Path=/"))

	r := httptest.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/ps/plugins/list", nil)
	r.AddCookie(&http.Cookie{Name: "__cflb", Value: "client-side"})
	jar.apply(key, r)

	cookies := r.Cookies()
	if len(cookies) != 1 || cookies[0].Value != "client-side" {
		t.Fatalf("cookies = %v, want only the client's own __cflb", cookies)
	}
}

func TestUpstreamCookieJarIsBounded(t *testing.T) {
	jar := newUpstreamCookieJar()
	for i := 0; i < upstreamCookieMaxKeys*3; i++ {
		jar.capture(upstreamCookieKey(fmt.Sprintf("acct-%d", i), "chatgpt.com"),
			respWithCookies("__cflb=west; Path=/"))
	}
	if n := jar.len(); n > upstreamCookieMaxKeys {
		t.Fatalf("jar holds %d keys, want <= %d", n, upstreamCookieMaxKeys)
	}
}

// End to end over a fake Cloudflare-style upstream: a backend that only honors
// a pagination cursor when the sticky cookie comes back, and otherwise restarts
// the listing with a fresh cursor. A client following cursors terminates when
// affinity is preserved and loops forever when it is not. This is the shape
// that streamed 5.3 GB into one codex process.
func TestUpstreamCookiesTerminatePaginationAgainstStickyBackend(t *testing.T) {
	const totalPages = 3
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sticky, err := r.Cookie("__cflb")
		if err != nil || sticky.Value != "backend-1" {
			// Wrong backend: it has never seen this cursor, so it starts over.
			http.SetCookie(w, &http.Cookie{Name: "__cflb", Value: "backend-1", Path: "/"})
			fmt.Fprint(w, `{"page":1,"nextPageToken":"p2"}`)
			return
		}
		switch r.URL.Query().Get("pageToken") {
		case "":
			fmt.Fprint(w, `{"page":1,"nextPageToken":"p2"}`)
		case fmt.Sprintf("p%d", totalPages):
			fmt.Fprint(w, `{"page":3}`)
		default:
			fmt.Fprintf(w, `{"page":2,"nextPageToken":"p%d"}`, totalPages)
		}
	}))
	defer upstream.Close()

	jar := newUpstreamCookieJar()
	key := upstreamCookieKey("acct-alice", "upstream")
	client := upstream.Client()

	// Walk the cursor the way codex does: follow nextPageToken until it stops.
	token := ""
	pages := 0
	for pages < 25 {
		pages++
		url := upstream.URL + "/ps/plugins/list?limit=200"
		if token != "" {
			url += "&pageToken=" + token
		}
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		jar.apply(key, req)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		jar.capture(key, resp)
		var body struct {
			NextPageToken string `json:"nextPageToken"`
		}
		decodeJSON(t, resp, &body)
		resp.Body.Close()
		if body.NextPageToken == "" {
			break
		}
		token = body.NextPageToken
	}

	if pages != totalPages {
		t.Fatalf("walked %d pages, want %d: cursor affinity was not preserved", pages, totalPages)
	}

	// Same backend, no jar: this is the production behaviour before the fix.
	// The walk never terminates, which is what made codex allocate without end.
	noJarPages := 0
	token = ""
	for noJarPages < 25 {
		noJarPages++
		url := upstream.URL + "/ps/plugins/list?limit=200"
		if token != "" {
			url += "&pageToken=" + token
		}
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			NextPageToken string `json:"nextPageToken"`
		}
		decodeJSON(t, resp, &body)
		resp.Body.Close()
		if body.NextPageToken == "" {
			break
		}
		token = body.NextPageToken
	}
	if noJarPages != 25 {
		t.Fatalf("without the jar the walk terminated after %d pages; the test no longer reproduces the runaway", noJarPages)
	}
}

func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

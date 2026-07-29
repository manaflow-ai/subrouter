package proxy

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// Cloudflare pins a client to a specific backend with infrastructure cookies
// (__cflb for load-balancer affinity, __cf_bm for bot management, cf_clearance
// for challenge clearance). Codex keeps its own jar for these, but only for
// https://chatgpt.com URLs: see is_chatgpt_cookie_url in
// codex-rs/http-client/src/chatgpt_cloudflare_cookies.rs, which requires an
// https scheme and an allowlisted ChatGPT host. Pointed at http://127.0.0.1,
// the client stores nothing and sends nothing, and it is right not to hand
// session cookies to an arbitrary local proxy.
//
// That leaves subrouter as the only component that can hold them, and it must,
// because upstream state can be backend-bound. The plugin catalog's pagination
// cursor is: a continuation token minted by one backend is meaningless to
// another, which answers by restarting the listing and issuing a fresh cursor.
// A client that follows cursors until they run out then never terminates. Codex
// does exactly that (core-plugins/src/remote.rs), so one plugin-list call
// streamed 5.3 GB and drove the client to 18 GB RSS in two seconds.
//
// Stickiness aside, presenting the cookies a normal client would present is
// what keeps proxied traffic indistinguishable from direct traffic.
const (
	// upstreamCookieMaxKeys bounds the jar. Keys are (account, host) pairs, so
	// this is small in practice; the cap exists so it cannot grow without end.
	upstreamCookieMaxKeys = 512
)

type upstreamCookie struct {
	value     string
	expiresAt time.Time // zero means session cookie: keep for the process lifetime
}

type upstreamCookieJar struct {
	mu      sync.Mutex
	entries map[string]map[string]upstreamCookie
}

func newUpstreamCookieJar() *upstreamCookieJar {
	return &upstreamCookieJar{entries: make(map[string]map[string]upstreamCookie)}
}

// upstreamCookieKey scopes the jar. Cookies are per upstream host, and per
// account so two accounts are never correlated by a shared backend affinity.
func upstreamCookieKey(accountID, upstreamHost string) string {
	return accountID + "\n" + upstreamHost
}

// isCloudflareInfraCookie mirrors the allowlist Codex uses. Only Cloudflare
// infrastructure cookies belong here: never account, session, or auth cookies,
// which are user-identifying and must not be replayed across requests by an
// intermediary.
func isCloudflareInfraCookie(name string) bool {
	switch name {
	case "__cf_bm", "__cflb", "__cfruid", "__cfseq", "__cfwaitingroom",
		"_cfuvid", "cf_clearance", "cf_ob_info", "cf_use_ob":
		return true
	}
	return strings.HasPrefix(name, "cf_chl_")
}

// capture records allowlisted cookies from an upstream response.
func (j *upstreamCookieJar) capture(key string, resp *http.Response) {
	if j == nil || resp == nil {
		return
	}
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		return
	}
	now := time.Now()

	j.mu.Lock()
	defer j.mu.Unlock()
	for _, c := range cookies {
		if !isCloudflareInfraCookie(c.Name) {
			continue
		}
		if c.MaxAge < 0 || (!c.Expires.IsZero() && c.Expires.Before(now)) {
			// Upstream is clearing this cookie.
			if bucket, ok := j.entries[key]; ok {
				delete(bucket, c.Name)
			}
			continue
		}
		var expiresAt time.Time
		switch {
		case c.MaxAge > 0:
			expiresAt = now.Add(time.Duration(c.MaxAge) * time.Second)
		case !c.Expires.IsZero():
			expiresAt = c.Expires
		}
		bucket, ok := j.entries[key]
		if !ok {
			if len(j.entries) >= upstreamCookieMaxKeys {
				j.evictLocked(now)
			}
			bucket = make(map[string]upstreamCookie)
			j.entries[key] = bucket
		}
		bucket[c.Name] = upstreamCookie{value: c.Value, expiresAt: expiresAt}
	}
}

// apply attaches stored cookies to an outbound upstream request without
// disturbing cookies the client sent itself.
func (j *upstreamCookieJar) apply(key string, r *http.Request) {
	if j == nil || r == nil {
		return
	}
	now := time.Now()

	j.mu.Lock()
	bucket := j.entries[key]
	stored := make([]*http.Cookie, 0, len(bucket))
	for name, c := range bucket {
		if !c.expiresAt.IsZero() && now.After(c.expiresAt) {
			delete(bucket, name)
			continue
		}
		stored = append(stored, &http.Cookie{Name: name, Value: c.value})
	}
	j.mu.Unlock()
	if len(stored) == 0 {
		return
	}

	existing := make(map[string]struct{})
	for _, c := range r.Cookies() {
		existing[c.Name] = struct{}{}
	}
	for _, c := range stored {
		if _, ok := existing[c.Name]; ok {
			// The client's own cookie wins; it knows its session, we do not.
			continue
		}
		r.AddCookie(c)
	}
}

// evictLocked drops expired buckets, then arbitrary ones if still at the cap.
// Callers hold j.mu.
func (j *upstreamCookieJar) evictLocked(now time.Time) {
	for key, bucket := range j.entries {
		live := false
		for name, c := range bucket {
			if !c.expiresAt.IsZero() && now.After(c.expiresAt) {
				delete(bucket, name)
				continue
			}
			live = true
		}
		if !live {
			delete(j.entries, key)
		}
	}
	for key := range j.entries {
		if len(j.entries) < upstreamCookieMaxKeys {
			break
		}
		delete(j.entries, key)
	}
}

func (j *upstreamCookieJar) len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.entries)
}

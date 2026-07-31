package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Key completeness. Every dimension a shared response can depend on must
// change the key. The predecessor of this key (a stored-response cache keyed
// on path alone) replayed page 1 of the plugin catalog to a page-2 request:
// codex re-sent the same continuation cursor forever, 3,598 requests in one
// turn, 15 GB client RSS, kernel panics. Add a dimension to the coalescing
// contract, add it here, or the next omission repeats that.
func TestFlightKeyCoversEveryRequestDimension(t *testing.T) {
	base := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list?scope=GLOBAL&limit=200", nil)
		r.Header.Set("chatgpt-account-id", "acct-alice")
		r.Header.Set("Authorization", "Bearer alice-token")
		r.Header.Set("chatgpt-user-id", "user-alice")
		return r
	}
	variants := map[string]func(*http.Request){
		"path": func(r *http.Request) { r.URL.Path = "/backend-api/ps/plugins/installed" },
		"query value": func(r *http.Request) {
			r.URL.RawQuery = "scope=GLOBAL&limit=200&pageToken=TOKEN_B"
		},
		"query scope": func(r *http.Request) { r.URL.RawQuery = "scope=WORKSPACE&limit=200" },
		"method":      func(r *http.Request) { r.Method = http.MethodPost },
		"account":     func(r *http.Request) { r.Header.Set("chatgpt-account-id", "acct-bob") },
		"user":        func(r *http.Request) { r.Header.Set("chatgpt-user-id", "user-bob") },
		// The bearer is deliberately not a dimension while account headers are
		// present: tokens rotate per session and the account is what decides
		// which data comes back. It is a dimension only when nothing else
		// identifies the caller, which TestFlightKeyIdentityIgnoresRotatingBearer
		// covers from both sides.
		"bearer without account headers": func(r *http.Request) {
			r.Header.Del("chatgpt-account-id")
			r.Header.Del("chatgpt-user-id")
			r.Header.Set("Authorization", "Bearer bob-token")
		},
	}
	baseKey := flightKey(base())
	for name, mutate := range variants {
		r := base()
		mutate(r)
		if flightKey(r) == baseKey {
			t.Errorf("flightKey ignores %s: two different requests share one flight", name)
		}
	}
}

// Query parameter order must not fragment the key space, or concurrent
// sessions issuing equivalent requests stop sharing a flight.
func TestFlightKeyIgnoresQueryParameterOrder(t *testing.T) {
	first := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list?scope=GLOBAL&limit=200", nil)
	first.Header.Set("chatgpt-account-id", "acct-alice")
	reordered := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list?limit=200&scope=GLOBAL", nil)
	reordered.Header.Set("chatgpt-account-id", "acct-alice")
	if flightKey(first) != flightKey(reordered) {
		t.Fatal("parameter order split equivalent requests into separate flights")
	}
}

// Access tokens rotate per session. If identity is the credential rather than
// the account, every new session gets its own flight and concurrent sessions
// each pay for the same upstream work.
func TestFlightKeyIdentityIgnoresRotatingBearer(t *testing.T) {
	first := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list?scope=GLOBAL", nil)
	first.Header.Set("chatgpt-account-id", "acct-alice")
	first.Header.Set("chatgpt-user-id", "user-alice")
	first.Header.Set("Authorization", "Bearer token-from-session-1")

	second := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list?scope=GLOBAL", nil)
	second.Header.Set("chatgpt-account-id", "acct-alice")
	second.Header.Set("chatgpt-user-id", "user-alice")
	second.Header.Set("Authorization", "Bearer token-from-session-2")

	if flightKey(first) != flightKey(second) {
		t.Fatal("a rotated access token split the flight for one account")
	}

	other := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list?scope=GLOBAL", nil)
	other.Header.Set("chatgpt-account-id", "acct-bob")
	other.Header.Set("Authorization", "Bearer token-from-session-1")
	if flightKey(first) == flightKey(other) {
		t.Fatal("two accounts share a flight")
	}

	// With no account headers at all, the credential is the only separator.
	anonA := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list", nil)
	anonA.Header.Set("Authorization", "Bearer a")
	anonB := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list", nil)
	anonB.Header.Set("Authorization", "Bearer b")
	if flightKey(anonA) == flightKey(anonB) {
		t.Fatal("anonymous callers with different credentials share a flight")
	}
}

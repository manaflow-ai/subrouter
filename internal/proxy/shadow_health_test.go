package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestShadowHealthProofIsExplicitAndChallengeBound(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	server := Server{ShadowHealthKey: key}.Handler()

	request := httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	var ordinary map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &ordinary); err != nil {
		t.Fatal(err)
	}
	if _, exists := ordinary[ShadowHealthProofField]; exists {
		t.Fatalf("ordinary health exposed %s: %s", ShadowHealthProofField, response.Body.String())
	}

	challenge := []byte("fedcba9876543210fedcba9876543210")
	request = httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil)
	request.Header.Set(ShadowHealthChallengeHeader, hex.EncodeToString(challenge))
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	var challenged map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &challenged); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(shadowHealthDomain))
	_, _ = mac.Write(challenge)
	want := hex.EncodeToString(mac.Sum(nil))
	if got, _ := challenged[ShadowHealthProofField].(string); got != want {
		t.Fatalf("proof = %q, want %q", got, want)
	}

	request = httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil)
	request.Header.Set(ShadowHealthChallengeHeader, "not-a-valid-challenge")
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	var malformed map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &malformed); err != nil {
		t.Fatal(err)
	}
	if _, exists := malformed[ShadowHealthProofField]; exists {
		t.Fatalf("malformed challenge produced a proof: %s", response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil)
	request.Header.Set(ShadowHealthChallengeHeader, hex.EncodeToString(challenge))
	response = httptest.NewRecorder()
	Server{}.Handler().ServeHTTP(response, request)
	var unkeyed map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &unkeyed); err != nil {
		t.Fatal(err)
	}
	if _, exists := unkeyed[ShadowHealthProofField]; exists {
		t.Fatalf("unkeyed server produced a proof: %s", response.Body.String())
	}
}

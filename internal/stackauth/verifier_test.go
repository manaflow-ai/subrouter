package stackauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestVerifierAcceptsAuthenticatedSelectedTeam(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	projectID := "project-1"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "key-1", "kty": "EC", "crv": "P-256", "alg": "ES256",
			"x": encodeBigInt(privateKey.X), "y": encodeBigInt(privateKey.Y),
		}}})
	}))
	defer server.Close()
	now := time.Unix(2_000_000_000, 0)
	token := signedToken(t, privateKey, map[string]any{
		"sub":              "user-1",
		"project_id":       projectID,
		"selected_team_id": "team-1",
		"email":            "user@example.com",
		"role":             "authenticated",
		"iss":              server.URL + "/projects/" + projectID,
		"aud":              []string{projectID},
		"exp":              now.Add(time.Hour).Unix(),
		"is_anonymous":     false,
		"is_restricted":    false,
	})
	verifier := &Verifier{
		APIURL: server.URL, ProjectID: projectID, HTTPClient: server.Client(),
		Now: func() time.Time { return now },
	}
	claims, err := verifier.Verify(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.SelectedTeamID != "team-1" || claims.Email != "user@example.com" {
		t.Fatalf("claims = %#v", claims)
	}
}

func TestVerifierRejectsAnonymousAndWrongProject(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "key-1", "kty": "EC", "crv": "P-256", "alg": "ES256",
			"x": encodeBigInt(privateKey.X), "y": encodeBigInt(privateKey.Y),
		}}})
	}))
	defer server.Close()
	now := time.Unix(2_000_000_000, 0)
	verifier := &Verifier{
		APIURL: server.URL, ProjectID: "project-1", HTTPClient: server.Client(),
		Now: func() time.Time { return now },
	}
	for name, overrides := range map[string]map[string]any{
		"anonymous":      {"is_anonymous": true},
		"wrong project":  {"project_id": "project-2"},
		"missing team":   {"selected_team_id": ""},
		"expired":        {"exp": now.Add(-time.Hour).Unix()},
		"wrong issuer":   {"iss": "https://issuer.invalid"},
		"wrong audience": {"aud": []string{"project-2"}},
		"restricted":     {"is_restricted": true},
	} {
		t.Run(name, func(t *testing.T) {
			claims := map[string]any{
				"sub": "user-1", "project_id": "project-1", "selected_team_id": "team-1",
				"role": "authenticated", "iss": server.URL + "/projects/project-1",
				"aud": []string{"project-1"}, "exp": now.Add(time.Hour).Unix(),
				"is_anonymous": false, "is_restricted": false,
			}
			for key, value := range overrides {
				claims[key] = value
			}
			token := signedToken(t, privateKey, claims)
			if _, err := verifier.Verify(context.Background(), token); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestVerifierThrottlesForcedRefreshForRepeatedInvalidSignatures(t *testing.T) {
	trusted, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	attacker, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "key-1", "kty": "EC", "crv": "P-256", "alg": "ES256",
			"x": encodeBigInt(trusted.X), "y": encodeBigInt(trusted.Y),
		}}})
	}))
	defer server.Close()
	now := time.Unix(2_000_000_000, 0)
	claims := map[string]any{
		"sub": "user-1", "project_id": "project-1", "selected_team_id": "team-1",
		"role": "authenticated", "iss": server.URL + "/projects/project-1",
		"aud": []string{"project-1"}, "exp": now.Add(time.Hour).Unix(),
	}
	verifier := &Verifier{
		APIURL: server.URL, ProjectID: "project-1", HTTPClient: server.Client(),
		Now: func() time.Time { return now },
	}
	if _, err := verifier.Verify(context.Background(), signedToken(t, trusted, claims)); err != nil {
		t.Fatal(err)
	}
	badToken := signedToken(t, attacker, claims)
	for range 2 {
		if _, err := verifier.Verify(context.Background(), badToken); err == nil {
			t.Fatal("invalid signature accepted")
		}
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("JWKS fetches = %d, want initial fetch plus one forced refresh", got)
	}
}

func TestVerifierRejectsNonES256Algorithm(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","kid":"key-1"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{}`))
	verifier := &Verifier{ProjectID: "project-1"}
	if _, err := verifier.Verify(context.Background(), header+"."+payload+".signature"); err == nil {
		t.Fatal("non-ES256 token accepted")
	}
}

func TestVerifierRejectsUnexpectedTypeAndCriticalHeaders(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "key-1", "kty": "EC", "crv": "P-256", "alg": "ES256",
			"x": encodeBigInt(privateKey.X), "y": encodeBigInt(privateKey.Y),
		}}})
	}))
	defer server.Close()
	now := time.Unix(2_000_000_000, 0)
	claims := map[string]any{
		"sub": "user-1", "project_id": "project-1", "selected_team_id": "team-1",
		"role": "authenticated", "iss": server.URL + "/projects/project-1",
		"aud": []string{"project-1"}, "exp": now.Add(time.Hour).Unix(),
	}
	verifier := &Verifier{
		APIURL: server.URL, ProjectID: "project-1", HTTPClient: server.Client(),
		Now: func() time.Time { return now },
	}
	for name, header := range map[string]map[string]any{
		"unexpected type": {"alg": "ES256", "kid": "key-1", "typ": "JWE"},
		"critical header": {"alg": "ES256", "kid": "key-1", "crit": []string{"exp"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier.Verify(context.Background(), signedTokenWithHeader(t, privateKey, header, claims)); err == nil {
				t.Fatal("unsupported JOSE header accepted")
			}
		})
	}
	if _, err := verifier.Verify(context.Background(), signedTokenWithHeader(t, privateKey,
		map[string]any{"alg": "ES256", "kid": "key-1"}, claims)); err != nil {
		t.Fatalf("Stack-compatible token without typ rejected: %v", err)
	}
}

func signedToken(t *testing.T, privateKey *ecdsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	return signedTokenWithHeader(t, privateKey, map[string]any{
		"alg": "ES256", "kid": "key-1", "typ": "JWT",
	}, claims)
}

func signedTokenWithHeader(t *testing.T, privateKey *ecdsa.PrivateKey, headerFields, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(headerFields)
	payload, _ := json.Marshal(claims)
	first := base64.RawURLEncoding.EncodeToString(header)
	second := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := first + "." + second
	sum := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, privateKey, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 64)
	r.FillBytes(raw[:32])
	s.FillBytes(raw[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(raw)
}

func encodeBigInt(value *big.Int) string {
	raw := make([]byte, 32)
	value.FillBytes(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestAudienceContains(t *testing.T) {
	if !audienceContains(json.RawMessage(`"p"`), "p") ||
		!audienceContains(json.RawMessage(`["x","p"]`), "p") ||
		audienceContains(json.RawMessage(`["x"]`), "p") ||
		audienceContains(json.RawMessage(`null`), "p") {
		t.Fatal("audience matching failed")
	}
}

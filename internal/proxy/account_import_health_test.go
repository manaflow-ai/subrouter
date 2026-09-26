package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

func TestHealthReportsAccountImportState(t *testing.T) {
	for _, tc := range []struct {
		name        string
		adminToken  string
		importToken string
		want        string
	}{
		{name: "no credential configured", want: AccountImportDisabled},
		{name: "admin token", adminToken: "secret", want: AccountImportEnabled},
		{name: "import token", importToken: "secret", want: AccountImportEnabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := accounts.CodexStore{Dir: t.TempDir()}
			ref := NewAccountRef(store, nil, nil)
			ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
			handler := Server{
				AccountRef:         ref,
				AdminToken:         tc.adminToken,
				AccountImportToken: tc.importToken,
			}.Handler()

			req := httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil)
			challenge := "0000000000000000000000000000000000000000000000000000000000000000"
			req.Header.Set(accounts.StoreAuthorityChallengeHeader, challenge)
			req.RemoteAddr = "100.64.0.20:4321"
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)

			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.Code)
			}
			var body struct {
				OK                bool   `json:"ok"`
				AccountImport     string `json:"account_import"`
				AccountStoreID    string `json:"account_store_id"`
				AccountStoreProof string `json:"account_store_proof"`
			}
			if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode health: %v (%s)", err, resp.Body.String())
			}
			if !body.OK {
				t.Fatalf("health ok = false, body = %s", resp.Body.String())
			}
			if body.AccountImport != tc.want {
				t.Fatalf("account_import = %q, want %q", body.AccountImport, tc.want)
			}
			if body.AccountStoreID != "" || body.AccountStoreProof != "" {
				t.Fatalf("public health exposed store proof: %s", resp.Body.String())
			}
		})
	}
}

func TestHealthDoesNotPublishStoreIdentityWithoutAValidChallenge(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	ref := NewAccountRef(store, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref}.Handler()

	for _, challenge := range []string{"", "not-a-valid-challenge"} {
		req := httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil)
		if challenge != "" {
			req.Header.Set(accounts.StoreAuthorityChallengeHeader, challenge)
		}
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("challenge %q status = %d, want 200", challenge, resp.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"account_store_id", "account_store_proof"} {
			if _, ok := body[field]; ok {
				t.Fatalf("challenge %q exposed %s: %s", challenge, field, resp.Body.String())
			}
		}
	}
}

func TestPublicStoreHandshakeIsNotAnUnauthenticatedProofOracle(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	if _, err := accounts.StoreAuthorityProof(store.Dir, strings.Repeat("00", 32)); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(store, nil, nil)
	request := httptest.NewRequest(http.MethodPost, StoreHandshakePath, nil)
	request.RemoteAddr = "100.64.0.20:4321"
	response := httptest.NewRecorder()
	Server{AccountRef: ref}.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unauthenticated handshake status = %d, want 404", response.Code)
	}
	for _, secretField := range []string{"account_store_id", "account_store_proof"} {
		if strings.Contains(response.Body.String(), secretField) {
			t.Fatalf("unauthenticated handshake exposed %s: %s", secretField, response.Body.String())
		}
	}
}

func TestSuccessfulStoreHandshakeAuthorizesOnlyItsConnection(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	nonce := strings.Repeat("11", 32)
	if _, err := accounts.StoreAuthorityProof(store.Dir, strings.Repeat("00", 32)); err != nil {
		t.Fatal(err)
	}
	requestProof, err := accounts.ExistingStoreHandshakeRequestProof(store.Dir, nonce)
	if err != nil {
		t.Fatal(err)
	}
	server := Server{AccountRef: NewAccountRef(store, nil, nil), AdminToken: "required"}
	handler := server.Handler()
	connectionContext := LocalDataConnContext(context.Background(), nil)

	before := httptest.NewRequest(http.MethodGet, "/_subrouter/accounts", nil).WithContext(connectionContext)
	before.RemoteAddr = "private-data-router"
	beforeResponse := httptest.NewRecorder()
	handler.ServeHTTP(beforeResponse, before)
	if beforeResponse.Code != http.StatusUnauthorized {
		t.Fatalf("pre-handshake admin status = %d, want 401", beforeResponse.Code)
	}

	handshake := httptest.NewRequest(http.MethodPost, StoreHandshakePath, nil).WithContext(connectionContext)
	handshake.RemoteAddr = "private-data-router"
	handshake.Header.Set(accounts.StoreHandshakeNonceHeader, nonce)
	handshake.Header.Set(accounts.StoreHandshakeRequestHeader, requestProof)
	handshakeResponse := httptest.NewRecorder()
	handler.ServeHTTP(handshakeResponse, handshake)
	if handshakeResponse.Code != http.StatusOK {
		t.Fatalf("handshake status = %d body=%s", handshakeResponse.Code, handshakeResponse.Body.String())
	}

	after := httptest.NewRequest(http.MethodGet, "/_subrouter/accounts", nil).WithContext(connectionContext)
	after.RemoteAddr = "private-data-router"
	afterResponse := httptest.NewRecorder()
	handler.ServeHTTP(afterResponse, after)
	if afterResponse.Code != http.StatusOK {
		t.Fatalf("post-handshake admin status = %d body=%s", afterResponse.Code, afterResponse.Body.String())
	}

	otherContext := LocalDataConnContext(context.Background(), nil)
	other := httptest.NewRequest(http.MethodGet, "/_subrouter/accounts", nil).WithContext(otherContext)
	other.RemoteAddr = "private-data-router"
	otherResponse := httptest.NewRecorder()
	handler.ServeHTTP(otherResponse, other)
	if otherResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unattested sibling connection status = %d, want 401", otherResponse.Code)
	}
}

// A disabled report must mean the endpoint actually rejects imports, so the
// health field and the authorization rule cannot drift apart.
func TestHealthAccountImportStateMatchesAuthorization(t *testing.T) {
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	server := Server{AccountRef: ref}
	if server.AccountImportState() != AccountImportDisabled {
		t.Fatalf("state = %q, want %q", server.AccountImportState(), AccountImportDisabled)
	}

	req := httptest.NewRequest(http.MethodGet, "/_subrouter/account-import", nil)
	req.RemoteAddr = "100.64.0.20:4321"
	resp := httptest.NewRecorder()
	server.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("import status = %d, want 401 while state reports disabled", resp.Code)
	}
}

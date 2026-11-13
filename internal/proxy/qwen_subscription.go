package proxy

import (
	"net/http"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// Catalog GETs bypass the inference retry stack. Repair a rejected credential
// before response capture can turn a refreshable grant into an exhaustion mark.
type qwenSubscriptionReadRepairTransport struct {
	base    http.RoundTripper
	server  *Server
	account accounts.Account
}

func (t qwenSubscriptionReadRepairTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if err != nil || response.StatusCode != http.StatusUnauthorized || req.Context().Err() != nil || t.server.AccountRef == nil {
		return response, err
	}
	store, ok := t.server.AccountRef.qwenSubscriptionStore()
	if !ok {
		return response, nil
	}
	repaired, err := store.RefreshRejectedAccount(req.Context(), t.server.AccountRef.client, t.account)
	if err != nil {
		return response, nil
	}
	repaired, err = t.server.AccountRef.publishQwenSubscriptionAccount(req.Context(), repaired)
	if err != nil || repaired.CredentialIdentity() == t.account.CredentialIdentity() {
		return response, nil
	}
	replay := req.Clone(req.Context())
	setAccountAuthHeaders(replay.Header, repaired, "")
	_ = response.Body.Close()
	response, err = t.base.RoundTrip(replay)
	return tagRoutedResponseAccount(response, repaired), err
}

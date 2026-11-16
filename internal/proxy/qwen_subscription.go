package proxy

import (
	"net/http"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// Catalog reads and pinned inference bypass account failover. Repair only their
// selected credential before response capture can mark it exhausted.
type qwenSubscriptionRepairTransport struct {
	base    http.RoundTripper
	server  *Server
	account accounts.Account
	attempt *upstreamAttempt
}

func (t qwenSubscriptionRepairTransport) RoundTrip(req *http.Request) (*http.Response, error) {
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
	if t.attempt != nil {
		replay, err = t.attempt.replay(req, &repaired)
		if err != nil {
			return response, nil
		}
		if !t.attempt.consume() {
			_ = replay.Body.Close()
			return response, nil
		}
	} else {
		setAccountAuthHeaders(replay.Header, repaired, "")
	}
	_ = response.Body.Close()
	if t.attempt != nil {
		response, err = t.attempt.send(t.base, replay, repaired)
	} else {
		response, err = t.base.RoundTrip(replay)
	}
	return tagRoutedResponseAccount(response, repaired), err
}

package proxy

import (
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// upstreamAttempt is the state of one client request as it moves through the
// upstream transport stack that upstreamLayers.build composes. Every layer of
// that stack reads and updates this one value. Before it existed, each layer
// kept its own idea of the current account, and a layer that switched
// accounts had to hand the switch down through a request-context value, or
// the layer below charged the new account's failures to the old one.
//
// The tried set is deliberately not shared here. Each failover loop excludes
// a different scope today: the usage-limit loop starts a fresh set on every
// call, and the Codex capacity loop keeps its own across calls and clears it
// in persist mode. One shared set would change which account a retry picks,
// so unifying them is a behavior change for its own PR.
type upstreamAttempt struct {
	server    *Server
	provider  accounts.Provider
	agent     string
	session   string
	userEmail string
	// path is the client path before any account's auth-mode rewrite, so a
	// retarget derives the replacement account's own upstream path.
	path string
	// poolModel is the quota pool retries are scored against.
	poolModel string
	// account is the account the stack was built for: the request's initial
	// pick, with its credential version.
	account accounts.Account
	// budget is the request's shared retry allowance. Every layer spends from
	// it, so nested retry loops cannot multiply into one full budget each.
	budget *attemptBudget
	// getBody returns a fresh copy of the buffered client body.
	getBody func() (io.ReadCloser, error)

	mu sync.Mutex
	// addressed is the account the request most recently sent downward is
	// addressed to, or zero when no layer has named one (the initial account).
	addressed accounts.Account
}

// standaloneUpstreamAttempt is the private state a layer uses when it was
// constructed on its own rather than by upstreamLayers.build, as unit tests
// do. It reproduces what that layer saw before the shared state existed.
func standaloneUpstreamAttempt(req *http.Request, server *Server, account accounts.Account, budget *attemptBudget) *upstreamAttempt {
	return &upstreamAttempt{server: server, account: account, budget: budget, getBody: req.GetBody}
}

// replayable reports whether the client body can be sent again.
func (a *upstreamAttempt) replayable() bool {
	return a.getBody != nil
}

// consume claims one retry from the request's shared budget.
func (a *upstreamAttempt) consume() bool {
	return a.budget.consume()
}

// current is the account the last downward send was addressed to. A layer
// reads it on entry, before sending anything itself, where it is the account
// the layer above addressed this layer's input to.
func (a *upstreamAttempt) current() accounts.Account {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.addressed.ID != "" {
		return a.addressed
	}
	return a.account
}

// send records which account req is addressed to and sends it one layer
// down. Every layer sends through here, so the layers below always start from
// the account the request actually carries credentials for, including when a
// layer replays its own input after a lower layer had moved elsewhere.
func (a *upstreamAttempt) send(base http.RoundTripper, req *http.Request, account accounts.Account) (*http.Response, error) {
	a.mu.Lock()
	a.addressed = account
	a.mu.Unlock()
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// body reads the buffered client body, for layers that hand it to a
// different client (Azure) or need its bytes (the Fable fallback).
func (a *upstreamAttempt) body() ([]byte, bool) {
	if a.getBody == nil {
		return nil, false
	}
	rc, err := a.getBody()
	if err != nil {
		return nil, false
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, false
	}
	return body, true
}

// replay builds the next attempt of req with a fresh copy of the client
// body. With a nil account it is the same request again, addressed where req
// was. With an account it is retargeted: that account's upstream URL (a
// provider may route API-key and subscription credentials to different hosts)
// and its auth headers, so a failover never sends a credential to the previous
// account's upstream.
func (a *upstreamAttempt) replay(req *http.Request, account *accounts.Account) (*http.Request, error) {
	if a.getBody == nil {
		return nil, errors.New("request body is not replayable")
	}
	body, err := a.getBody()
	if err != nil {
		return nil, err
	}
	next := req.Clone(req.Context())
	next.Body = body
	next.GetBody = a.getBody
	next.ContentLength = req.ContentLength
	if account == nil {
		return next, nil
	}
	if a.server != nil {
		if upstream := a.server.upstreamForRequest(a.path, *account); upstream != nil {
			next.URL.Scheme = upstream.Scheme
			next.URL.Host = upstream.Host
			next.URL.User = upstream.User
			next.URL.Path = joinURLPath(upstream.Path, a.server.pathForUpstream(a.path, *account))
			next.URL.RawPath = ""
		}
	}
	setAccountAuthHeaders(next.Header, *account, a.poolModel)
	return next, nil
}

// upstreamLayers are the retry and fallback layers one request needs. A nil
// layer is left out. The handler fills in each layer's own policy; build wires
// every layer to the request's shared attempt state.
type upstreamLayers struct {
	usageLimit     *usageLimitRetryTransport
	replayablePost *replayablePostRetryTransport
	codexOverload  *codexOverloadFailoverTransport
	codexEgress    *codexEgressFallbackTransport
	azureCodex     *azureCodexFallbackTransport
}

// build composes the layers around base, innermost first. The order is the
// policy: each layer only sees what the layers below it could not handle.
func (l upstreamLayers) build(base http.RoundTripper, a *upstreamAttempt) http.RoundTripper {
	transport := base
	// Usage-limit and auth failover sits closest to the pool: quota, auth and
	// model-capability answers belong to the account that produced them, so
	// they are handled (marked, and failed over) before any layer above can
	// mistake them for a pool-wide failure. Same-account Anthropic/Kimi
	// overload retries, the one-shot Claude overload reroute and the Fable
	// fallback live here for the same reason.
	if l.usageLimit != nil {
		layer := *l.usageLimit
		layer.base, layer.attempt = transport, a
		layer.server, layer.provider = a.server, a.provider
		layer.agent, layer.session, layer.userEmail = a.agent, a.session, a.userEmail
		layer.account, layer.accountCredential = a.account.ID, a.account.CredentialVersion
		layer.path, layer.poolModel = a.path, a.poolModel
		transport = layer
	}
	// Transport-level replay (408, connection resets) wraps the account
	// layer: it re-sends its own input unchanged, so an account failover below
	// restarts from the account this request is addressed to.
	if l.replayablePost != nil {
		layer := *l.replayablePost
		layer.base, layer.attempt = transport, a
		layer.agent, layer.session, layer.account = a.agent, a.session, a.account.ID
		transport = layer
	}
	// Codex capacity retries sit above both retry layers: a capacity failure
	// is whatever still fails after quota and transport recovery, and the lever
	// the data supports is the same request on another account.
	if l.codexOverload != nil {
		layer := *l.codexOverload
		layer.base, layer.attempt = transport, a
		layer.server = a.server
		layer.agent, layer.session, layer.userEmail = a.agent, a.session, a.userEmail
		layer.account, layer.poolModel = a.account.ID, a.poolModel
		transport = layer
	}
	// Regional egress only after every tried account failed the same way: a
	// regional retry is the same model on the same account, so it must come
	// before paying a second provider.
	if l.codexEgress != nil {
		layer := *l.codexEgress
		layer.base, layer.attempt = transport, a
		layer.server, layer.agent = a.server, a.agent
		transport = layer
	}
	// Azure is outermost: it is a different provider, the answer of last
	// resort once the pool has spent its retry budget or cannot start.
	if l.azureCodex != nil {
		layer := *l.azureCodex
		layer.base, layer.attempt = transport, a
		layer.server, layer.accountID = a.server, a.account.ID
		transport = layer
	}
	return transport
}

package proxy

import (
	"context"
	"net/http"

	"github.com/manaflow-ai/subrouter/internal/tailnet"
)

// TailnetAuthorizer resolves the tailnet principal behind a request. It is an
// interface so the proxy never depends on how identity is obtained, and so
// tests can assert the authorization rule without a running tailscaled.
type TailnetAuthorizer interface {
	Authorize(ctx context.Context, remoteAddr string) (tailnet.Identity, bool)
}

// authorizeTailnet reports the tailnet identity behind a request when the mode
// is enabled and the peer is allowed. A peer that is not on the tailnet, or is
// outside the configured allowlist, falls through to the token rules rather
// than being rejected here: enabling tailnet auth adds a way in, it does not
// remove the existing ones.
func (s Server) authorizeTailnet(r *http.Request) (tailnet.Identity, bool) {
	if s.TailnetAuth == nil || r == nil {
		return tailnet.Identity{}, false
	}
	return s.TailnetAuth.Authorize(r.Context(), r.RemoteAddr)
}

// AuthMode describes how this server authenticates non-loopback callers, for
// health output and startup logging.
func (s Server) AuthMode() string {
	switch {
	case s.tenantAccountImportAuthorized:
		return "tenant"
	case s.TailnetAuth != nil:
		return "tailnet"
	case s.AdminToken != "" || s.AccountImportToken != "":
		return "token"
	default:
		return "open"
	}
}

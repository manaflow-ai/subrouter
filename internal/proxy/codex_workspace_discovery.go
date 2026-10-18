package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Codex 0.162 app-server resolves "workspace routing" before it answers
// account/read: it GETs <chatgpt_base_url>/wham/accounts/check and requires
// the workspace selected in auth.json (sent as ChatGPT-Account-Id) to appear
// exactly once, with an https workspace_backend_origin and a known
// account_routing_override. Forwarded upstream, that request carries a pool
// account's credentials, so the reply lists the pool account's workspaces and
// Codex fails account/read with "selected workspace missing from routing
// discovery", which breaks codex-acp.
//
// subrouter answers it instead. The pool account, not the caller's workspace,
// decides where a request goes, so the honest answer is the selected
// workspace, unconstrained, on the standard backend. Codex never applies that
// origin to model traffic through subrouter: it rewrites request origins only
// for a provider whose base URL ends in /backend-api/codex, and subrouter
// configures openai_base_url as <proxy>/v1.
const codexWorkspaceDiscoveryOrigin = "https://chatgpt.com"

func codexWorkspaceDiscoveryPath(path string) bool {
	return path == "/backend-api/wham/accounts/check" || path == "/backend-api/api/codex/accounts/check"
}

// codexSelectedWorkspace returns the caller's ChatGPT-Account-Id when it is
// a plausible account id; anything else is forwarded unchanged.
func codexSelectedWorkspace(r *http.Request) (string, bool) {
	values := r.Header.Values("ChatGPT-Account-Id")
	if len(values) != 1 {
		return "", false
	}
	id := strings.TrimSpace(values[0])
	if id == "" || len(id) > 128 {
		return "", false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return "", false
		}
	}
	return id, true
}

// serveCodexWorkspaceDiscovery answers a Codex workspace routing discovery
// for the caller's selected workspace. It reports false, writing nothing, for
// a request it does not answer.
func serveCodexWorkspaceDiscovery(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || !codexWorkspaceDiscoveryPath(r.URL.Path) {
		return false
	}
	id, ok := codexSelectedWorkspace(r)
	if !ok {
		return false
	}
	body, err := json.Marshal(map[string]any{
		"accounts": []map[string]any{{
			"id":                       id,
			"workspace_backend_origin": codexWorkspaceDiscoveryOrigin,
			"account_routing_override": "NO_CONSTRAINT",
		}},
		"account_ordering":   []string{id},
		"default_account_id": id,
	})
	if err != nil {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Subrouter-Reason", "codex_workspace_discovery")
	_, _ = w.Write(body)
	return true
}

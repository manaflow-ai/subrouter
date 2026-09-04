package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentantigravity "github.com/manaflow-ai/subrouter/internal/agents/antigravity"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	agentkimi "github.com/manaflow-ai/subrouter/internal/agents/kimi"
)

const serverAccountImportPath = "/_subrouter/account-import"

type serverAccountImportRequest struct {
	Provider    accounts.Provider               `json:"provider"`
	Codex       *accounts.StoredCodexAccount    `json:"codex,omitempty"`
	Claude      *serverClaudeAccountImport      `json:"claude,omitempty"`
	Kimi        *serverKimiAccountImport        `json:"kimi,omitempty"`
	Antigravity *serverAntigravityAccountImport `json:"antigravity,omitempty"`
}

type serverAntigravityAccountImport struct {
	Label      string                          `json:"label"`
	Credential agentantigravity.CredentialInfo `json:"credential,omitempty"`
	Remove     bool                            `json:"remove,omitempty"`
}

type serverClaudeAccountImport struct {
	Name       string                     `json:"name"`
	Credential agentclaude.CredentialInfo `json:"credential"`
}

type serverKimiAccountImport struct {
	Label      string                   `json:"label"`
	Credential agentkimi.CredentialInfo `json:"credential,omitempty"`
	Remove     bool                     `json:"remove,omitempty"`
}

func (r srRunner) ensureServerAccountImportAvailable(ctx context.Context, server srServerConfig) error {
	return r.ensureServerAccountImportProviderAvailable(ctx, server, "")
}

func (r srRunner) ensureServerAccountImportProviderAvailable(ctx context.Context, server srServerConfig, provider accounts.Provider) error {
	// Ask the server rather than assuming a stored credential is required. A
	// self-hosted server can authenticate callers by tailnet identity, in which
	// case this entry has nothing to carry and the preflight simply succeeds.
	res, err := r.doServerAccountImportRequest(ctx, server, http.MethodGet, nil)
	if err != nil {
		return fmt.Errorf("check HTTP account import on server %s: %w", server.Name, err)
	}
	defer res.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(res.Body, 16<<10))
	if readErr != nil {
		return fmt.Errorf("read account-import preflight from server %s: %w", server.Name, readErr)
	}
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		var response struct {
			OK        bool     `json:"ok"`
			Providers []string `json:"providers"`
		}
		if err := json.Unmarshal(body, &response); err != nil || !response.OK {
			return fmt.Errorf("server %s returned an invalid account-import preflight", server.Name)
		}
		if provider != "" {
			available := false
			for _, advertised := range response.Providers {
				if strings.EqualFold(strings.TrimSpace(advertised), string(provider)) {
					available = true
					break
				}
			}
			if !available {
				return fmt.Errorf("server %s does not advertise %s account import; run '%s install %s' first", server.Name, provider, r.serverCommand(), server.Name)
			}
		}
		return nil
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		if !serverHasAccountImportCredential(server) {
			return fmt.Errorf("server %s has no protected HTTP account-import credential; run '%s install %s' first", server.Name, r.serverCommand(), server.Name)
		}
		return fmt.Errorf("server %s rejected its protected HTTP account-import credential; run '%s install %s' to rotate it", server.Name, r.serverCommand(), server.Name)
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusMethodNotAllowed:
		return fmt.Errorf("server %s is too old for HTTP account import; run '%s install %s' first", server.Name, r.serverCommand(), server.Name)
	default:
		return fmt.Errorf("server %s account-import preflight failed: %s", server.Name, res.Status)
	}
}

func (r srRunner) uploadServerAccount(ctx context.Context, server srServerConfig, account accounts.StoredCodexAccount) error {
	provider := account.ProviderOrDefault()
	if err := r.ensureServerAccountImportAvailable(ctx, server); err != nil {
		return err
	}
	return r.postServerAccountImport(ctx, server, serverAccountImportRequest{
		Provider: provider,
		Codex:    &account,
	})
}

func (r srRunner) uploadServerClaudeAccount(ctx context.Context, server srServerConfig, name string, credential agentclaude.CredentialInfo) error {
	if err := r.ensureServerAccountImportAvailable(ctx, server); err != nil {
		return err
	}
	return r.postServerAccountImport(ctx, server, serverAccountImportRequest{
		Provider: accounts.ProviderClaude,
		Claude: &serverClaudeAccountImport{
			Name:       name,
			Credential: credential,
		},
	})
}

func (r srRunner) uploadServerKimiAccount(ctx context.Context, server srServerConfig, label string, credential agentkimi.CredentialInfo) error {
	if err := r.ensureServerAccountImportProviderAvailable(ctx, server, accounts.ProviderKimi); err != nil {
		return err
	}
	return r.postServerAccountImport(ctx, server, serverAccountImportRequest{
		Provider: accounts.ProviderKimi,
		Kimi:     &serverKimiAccountImport{Label: label, Credential: credential},
	})
}

func (r srRunner) removeServerKimiAccount(ctx context.Context, server srServerConfig, label string) error {
	if err := r.ensureServerAccountImportProviderAvailable(ctx, server, accounts.ProviderKimi); err != nil {
		return err
	}
	return r.postServerAccountImport(ctx, server, serverAccountImportRequest{
		Provider: accounts.ProviderKimi,
		Kimi:     &serverKimiAccountImport{Label: label, Remove: true},
	})
}

func (r srRunner) uploadServerAntigravityAccount(ctx context.Context, server srServerConfig, label string, credential agentantigravity.CredentialInfo) error {
	if err := r.ensureServerAccountImportProviderAvailable(ctx, server, accounts.ProviderAntigravity); err != nil {
		return err
	}
	return r.postServerAccountImport(ctx, server, serverAccountImportRequest{
		Provider:    accounts.ProviderAntigravity,
		Antigravity: &serverAntigravityAccountImport{Label: label, Credential: credential},
	})
}

func (r srRunner) removeServerAntigravityAccount(ctx context.Context, server srServerConfig, label string) error {
	if err := r.ensureServerAccountImportProviderAvailable(ctx, server, accounts.ProviderAntigravity); err != nil {
		return err
	}
	return r.postServerAccountImport(ctx, server, serverAccountImportRequest{
		Provider:    accounts.ProviderAntigravity,
		Antigravity: &serverAntigravityAccountImport{Label: label, Remove: true},
	})
}

func (r srRunner) postServerAccountImport(ctx context.Context, server srServerConfig, input serverAccountImportRequest) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	res, err := r.doServerAccountImportRequest(ctx, server, http.MethodPost, body)
	if err != nil {
		return fmt.Errorf("POST account to server %s: %w", server.Name, err)
	}
	defer res.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(res.Body, 16<<10))
	if readErr != nil {
		return fmt.Errorf("read account-import response from server %s: %w", server.Name, readErr)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("server %s account import failed: %s", server.Name, res.Status)
	}
	var response struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil || !response.OK {
		return fmt.Errorf("server %s returned an invalid account-import response", server.Name)
	}
	return nil
}

// serverHasAccountImportCredential reports whether this entry carries anything
// the server could have rejected, which decides whether a 401 means "add a
// credential" or "the one you have is wrong".
func serverHasAccountImportCredential(server srServerConfig) bool {
	return strings.TrimSpace(server.AccountImportToken) != "" ||
		strings.TrimSpace(server.AdminToken) != "" ||
		strings.TrimSpace(server.TenantKey) != ""
}

func (r srRunner) doServerAccountImportRequest(ctx context.Context, server srServerConfig, method string, body []byte) (*http.Response, error) {
	baseURL, err := protectedServerControlBaseURL(server)
	if err != nil {
		return nil, err
	}
	endpoint := baseURL + serverAccountImportPath
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, redactServerRequestError(err, server)
	}
	addServerAccountImportAuth(req, server)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	secured, err := r.securedRequestClientForServer(server, endpoint, 30*time.Second)
	if err != nil {
		return nil, err
	}
	res, err := secured.Do(req)
	if err != nil {
		return nil, redactServerRequestError(err, server)
	}
	return res, nil
}

var tenantPathErrorPattern = regexp.MustCompile(`/t/[^/?#\s"']+`)

func redactServerRequestError(err error, server srServerConfig) error {
	message := err.Error()
	secrets := []string{
		strings.TrimSpace(server.TenantKey),
		strings.TrimSpace(server.AccountImportToken),
		strings.TrimSpace(server.AdminToken),
	}
	if parsed, parseErr := url.Parse(strings.TrimSpace(server.URL)); parseErr == nil {
		secrets = append(secrets, tenantKeyFromURL(parsed))
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		message = strings.ReplaceAll(message, secret, "[redacted]")
		message = strings.ReplaceAll(message, url.PathEscape(secret), "[redacted]")
	}
	message = tenantPathErrorPattern.ReplaceAllString(message, "/t/[redacted]")
	return errors.New(message)
}

func addServerAccountImportAuth(req *http.Request, server srServerConfig) {
	token := strings.TrimSpace(server.AccountImportToken)
	if token == "" {
		token = strings.TrimSpace(server.AdminToken)
	}
	if token == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
}

func securedServerRequestClient(base *http.Client, rawURL string) (*http.Client, error) {
	clientCopy := *base
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	var transport *http.Transport
	switch configured := clientCopy.Transport.(type) {
	case nil:
		transport = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		transport = configured.Clone()
	default:
		return nil, errors.New("protected requests require a pinnable HTTP transport")
	}
	// Protected control requests intentionally bypass ambient proxies even for
	// HTTPS. This keeps tenant path keys and authorization metadata out of proxy
	// logs and matches the stricter direct-transport contract of account import.
	transport.Proxy = nil
	clientCopy.Transport = transport
	if parsed.Scheme != "http" {
		return &clientCopy, nil
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	loopbackName := strings.EqualFold(strings.TrimSuffix(host, "."), "localhost")
	if !loopbackName && (ip == nil || !safeAccountImportHTTPIP(ip)) {
		return nil, errors.New("plain HTTP protected requests require a pinned Tailscale or loopback address")
	}
	if loopbackName {
		transport.DialContext = dialLoopbackAddress
	}
	clientCopy.Transport = transport
	return &clientCopy, nil
}

func dialLoopbackAddress(ctx context.Context, network, address string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("loopback destination is invalid")
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, host := range []string{"127.0.0.1", "::1"} {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func safeAccountImportHTTPIP(ip net.IP) bool {
	if ip.IsLoopback() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		return v4[0] == 100 && v4[1]&0xc0 == 0x40 // 100.64.0.0/10
	}
	v6 := ip.To16()
	return v6 != nil && v6[0] == 0xfd && v6[1] == 0x7a && v6[2] == 0x11 && v6[3] == 0x5c && v6[4] == 0xa1 && v6[5] == 0xe0
}

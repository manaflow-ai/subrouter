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
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

const serverAccountImportPath = "/_subrouter/account-import"

type serverAccountImportRequest struct {
	Provider accounts.Provider            `json:"provider"`
	Codex    *accounts.StoredCodexAccount `json:"codex,omitempty"`
	Claude   *serverClaudeAccountImport   `json:"claude,omitempty"`
}

type serverClaudeAccountImport struct {
	Name       string                     `json:"name"`
	Credential agentclaude.CredentialInfo `json:"credential"`
}

func (r srRunner) ensureServerAccountImportAvailable(ctx context.Context, server srServerConfig) error {
	if strings.TrimSpace(server.AccountImportToken) == "" && strings.TrimSpace(server.AdminToken) == "" && strings.TrimSpace(server.TenantKey) == "" {
		return fmt.Errorf("server %s has no protected HTTP account-import credential; run '%s install %s' first", server.Name, r.serverCommand(), server.Name)
	}
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
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(body, &response); err != nil || !response.OK {
			return fmt.Errorf("server %s returned an invalid account-import preflight", server.Name)
		}
		return nil
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return fmt.Errorf("server %s rejected its protected HTTP account-import credential; run '%s install %s' to rotate it", server.Name, r.serverCommand(), server.Name)
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusMethodNotAllowed:
		return fmt.Errorf("server %s is too old for HTTP account import; run '%s install %s' first", server.Name, r.serverCommand(), server.Name)
	default:
		return fmt.Errorf("server %s account-import preflight failed: %s", server.Name, res.Status)
	}
}

func (r srRunner) uploadServerAccount(ctx context.Context, server srServerConfig, account accounts.StoredCodexAccount) error {
	if err := r.ensureServerAccountImportAvailable(ctx, server); err != nil {
		return err
	}
	provider := account.ProviderOrDefault()
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

func (r srRunner) doServerAccountImportRequest(ctx context.Context, server srServerConfig, method string, body []byte) (*http.Response, error) {
	endpoint := serverControlBaseURL(server) + serverAccountImportPath
	if err := validateServerAccountImportURL(ctx, endpoint); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	addServerAccountImportAuth(req, server)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	client := r.client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	secured, err := securedServerAccountImportClient(client, endpoint)
	if err != nil {
		return nil, err
	}
	res, err := secured.Do(req)
	if err != nil {
		return nil, redactServerAccountImportError(err, server)
	}
	return res, nil
}

func redactServerAccountImportError(err error, server srServerConfig) error {
	message := err.Error()
	for _, secret := range []string{
		strings.TrimSpace(server.TenantKey),
		strings.TrimSpace(server.AccountImportToken),
		strings.TrimSpace(server.AdminToken),
	} {
		if secret == "" {
			continue
		}
		message = strings.ReplaceAll(message, secret, "[redacted]")
		message = strings.ReplaceAll(message, url.PathEscape(secret), "[redacted]")
	}
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

func securedServerAccountImportClient(base *http.Client, rawURL string) (*http.Client, error) {
	clientCopy := *base
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "http" {
		return &clientCopy, nil
	}
	var transport *http.Transport
	switch configured := clientCopy.Transport.(type) {
	case nil:
		transport = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		transport = configured.Clone()
	default:
		// Test seams and explicit custom transports still pass the URL policy
		// above. Production uses *http.Transport, where plaintext credentials are
		// additionally pinned to a verified tailnet/loopback socket below.
		return &clientCopy, nil
	}
	transport.Proxy = nil
	transport.DialContext = dialSafeAccountImportAddress
	clientCopy.Transport = transport
	return &clientCopy, nil
}

func dialSafeAccountImportAddress(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("account-import destination is invalid")
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, errors.New("account-import host did not resolve")
		}
		for _, address := range addresses {
			ips = append(ips, address.IP)
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("account-import host did not resolve")
	}
	for _, ip := range ips {
		if !safeAccountImportHTTPIP(ip) {
			return nil, errors.New("plain HTTP account import is restricted to Tailscale or loopback")
		}
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func validateServerAccountImportURL(ctx context.Context, rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("account-import URL is invalid")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return nil
	case "http":
	default:
		return errors.New("account-import URL must use HTTPS or a Tailscale/loopback HTTP address")
	}
	host := parsed.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if safeAccountImportHTTPIP(ip) {
			return nil
		}
		return errors.New("plain HTTP account import is restricted to Tailscale or loopback")
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil || len(addresses) == 0 {
		return errors.New("account-import host did not resolve")
	}
	for _, address := range addresses {
		if !safeAccountImportHTTPIP(address.IP) {
			return errors.New("plain HTTP account import is restricted to Tailscale or loopback")
		}
	}
	return nil
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

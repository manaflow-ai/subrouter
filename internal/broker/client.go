package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

type AuthStart struct {
	DeviceCode       string `json:"deviceCode"`
	UserCode         string `json:"userCode"`
	VerificationURL  string `json:"verificationUrl"`
	ExpiresInSeconds int    `json:"expiresInSeconds"`
	IntervalSeconds  int    `json:"intervalSeconds"`
}

type AuthPoll struct {
	Status       string `json:"status"`
	Client       string `json:"client,omitempty"`
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
}

type Team struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Personal       bool   `json:"personal"`
	Use            bool   `json:"-"`
	ManageAccounts bool   `json:"-"`
}

type teamWire struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Personal    bool   `json:"personal"`
	Permissions struct {
		Use            bool `json:"use"`
		ManageAccounts bool `json:"manageAccounts"`
	} `json:"permissions"`
}

type teamsEnvelope struct {
	SelectedTeamID string     `json:"selectedTeamId"`
	Teams          []teamWire `json:"teams"`
}

type SharedAccount struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Label     string `json:"label,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
	Health    *struct {
		OK      bool   `json:"ok"`
		Message string `json:"message,omitempty"`
	} `json:"health,omitempty"`
}

type accountsEnvelope struct {
	TeamID   string          `json:"teamId"`
	Accounts []SharedAccount `json:"accounts"`
}

type AccountUpload map[string]any

type LeaseRequest struct {
	Provider         account.Provider
	RequiredAuthMode account.AuthMode
	AgentType        string
	SessionID        string
	UserEmail        string
	PreferAccountID  string
	Model            string
}

type Lease struct {
	ID                   string
	Account              account.Account
	CredentialGeneration int
	IssuedAt             time.Time
	ExpiresAt            time.Time
	CredentialExpiresAt  time.Time
}

type LeaseOutcome string

const (
	LeaseSuccess       LeaseOutcome = "success"
	LeaseUnauthorized  LeaseOutcome = "unauthorized"
	LeaseForbidden     LeaseOutcome = "forbidden"
	LeaseRateLimited   LeaseOutcome = "rate_limited"
	LeaseProviderError LeaseOutcome = "provider_error"
)

type LeaseCooldownScope string

const (
	LeaseCooldownAccount LeaseCooldownScope = "account"
	LeaseCooldownQuota   LeaseCooldownScope = "quota"
)

type LeaseReport struct {
	Outcome       LeaseOutcome
	StatusCode    int
	CooldownScope LeaseCooldownScope
	RetryAt       time.Time
}

type leaseWire struct {
	LeaseID              string `json:"leaseId"`
	AccountID            string `json:"accountId"`
	Provider             string `json:"provider"`
	AuthMode             string `json:"authMode"`
	Token                string `json:"token"`
	ProviderAccountID    string `json:"providerAccountId,omitempty"`
	Label                string `json:"label"`
	Email                string `json:"email,omitempty"`
	CredentialGeneration int    `json:"credentialGeneration"`
	IssuedAt             string `json:"issuedAt"`
	ExpiresAt            string `json:"expiresAt"`
	CredentialExpiresAt  string `json:"credentialExpiresAt,omitempty"`
}

func (wire *leaseWire) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, forbidden := range []string{
		"refreshToken",
		"refresh_token",
		"idToken",
		"id_token",
		"credentials",
		"tokens",
		"apiKey",
	} {
		if _, ok := fields[forbidden]; ok {
			return fmt.Errorf("cmux.com credential lease contained forbidden field %q", forbidden)
		}
	}
	type plainLeaseWire leaseWire
	var decoded plainLeaseWire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*wire = leaseWire(decoded)
	return nil
}

type leaseEnvelope struct {
	TeamID string    `json:"teamId"`
	Lease  leaseWire `json:"lease"`
}

type Client struct {
	Config     Config
	HTTPClient *http.Client

	mu         sync.Mutex
	cache      map[string]Lease
	leaseToKey map[string]string
	leaseRefs  map[string]leaseRef
}

type leaseRef struct {
	AccountID            string
	CredentialGeneration int
	ExpiresAt            time.Time
}

func NewClient(config Config) *Client {
	return &Client{
		Config: config.Normalized(),
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		cache:      map[string]Lease{},
		leaseToKey: map[string]string{},
		leaseRefs:  map[string]leaseRef{},
	}
}

func (c *Client) StartAuth(ctx context.Context) (AuthStart, error) {
	var response AuthStart
	err := c.doJSON(ctx, http.MethodPost, "/api/vault/cli/auth/start", map[string]any{
		"client": "subrouter",
	}, false, &response)
	return response, err
}

func (c *Client) PollAuth(ctx context.Context, deviceCode string) (AuthPoll, error) {
	var response AuthPoll
	err := c.doJSON(ctx, http.MethodPost, "/api/vault/cli/auth/poll", map[string]string{
		"deviceCode": deviceCode,
	}, false, &response)
	return response, err
}

// Logout revokes the exact Stack session stored in this client's config.
func (c *Client) Logout(ctx context.Context) error {
	return c.doJSON(
		ctx,
		http.MethodPost,
		"/api/subrouter/logout",
		nil,
		true,
		nil,
	)
}

func (c *Client) ListTeams(ctx context.Context) ([]Team, string, error) {
	var response teamsEnvelope
	if err := c.doJSON(ctx, http.MethodGet, "/api/subrouter/teams", nil, true, &response); err != nil {
		return nil, "", err
	}
	teams := make([]Team, 0, len(response.Teams))
	for _, item := range response.Teams {
		teams = append(teams, Team{
			ID:             item.ID,
			Name:           item.Name,
			Personal:       item.Personal,
			Use:            item.Permissions.Use,
			ManageAccounts: item.Permissions.ManageAccounts,
		})
	}
	return teams, response.SelectedTeamID, nil
}

func (c *Client) ListAccounts(ctx context.Context) ([]SharedAccount, error) {
	var response accountsEnvelope
	if err := c.doJSON(ctx, http.MethodGet, "/api/subrouter/accounts", nil, true, &response); err != nil {
		return nil, err
	}
	return response.Accounts, nil
}

func (c *Client) UploadAccount(ctx context.Context, input AccountUpload) (SharedAccount, error) {
	var response struct {
		Account SharedAccount `json:"account"`
	}
	err := c.doJSON(
		ctx,
		http.MethodPost,
		"/api/subrouter/accounts?adopt=1",
		input,
		true,
		&response,
	)
	return response.Account, err
}

func (c *Client) DeleteAccount(ctx context.Context, accountID string) error {
	path := "/api/subrouter/accounts/" + url.PathEscape(accountID)
	return c.doJSON(ctx, http.MethodDelete, path, nil, true, nil)
}

func (c *Client) RepairAccount(
	ctx context.Context,
	accountID string,
	input AccountUpload,
) (SharedAccount, error) {
	path := "/api/subrouter/accounts/" +
		url.PathEscape(accountID) +
		"/repair?adopt=1"
	var response struct {
		Account SharedAccount `json:"account"`
	}
	err := c.doJSON(ctx, http.MethodPost, path, input, true, &response)
	return response.Account, err
}

func (c *Client) Lease(ctx context.Context, input LeaseRequest) (Lease, error) {
	key := leaseCacheKey(input)
	now := time.Now()
	c.mu.Lock()
	for leaseID, ref := range c.leaseRefs {
		if now.Before(ref.ExpiresAt) {
			continue
		}
		delete(c.leaseRefs, leaseID)
		delete(c.leaseToKey, leaseID)
	}
	for cacheKey, cached := range c.cache {
		if now.Add(15 * time.Second).Before(cached.ExpiresAt) {
			continue
		}
		delete(c.cache, cacheKey)
	}
	if cached, ok := c.cache[key]; ok && now.Add(15*time.Second).Before(cached.ExpiresAt) {
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	body := map[string]any{
		"provider":  input.Provider,
		"agentType": input.AgentType,
		"sessionId": input.SessionID,
	}
	if input.UserEmail != "" {
		body["userEmail"] = input.UserEmail
	}
	if input.PreferAccountID != "" {
		body["preferAccountId"] = input.PreferAccountID
	}
	if input.Model != "" {
		body["model"] = input.Model
	}
	if input.RequiredAuthMode != "" {
		body["requiredAuthMode"] = input.RequiredAuthMode
	}
	var response leaseEnvelope
	if err := c.doJSON(ctx, http.MethodPost, "/api/subrouter/leases", body, true, &response); err != nil {
		return Lease{}, err
	}
	lease, err := parseLease(response.Lease)
	if err != nil {
		return Lease{}, err
	}
	if input.RequiredAuthMode != "" &&
		lease.Account.AuthMode != input.RequiredAuthMode {
		return Lease{}, errors.New(
			"cmux.com returned a credential with the wrong auth mode",
		)
	}
	c.mu.Lock()
	c.cache[key] = lease
	c.leaseToKey[lease.ID] = key
	c.leaseRefs[lease.ID] = leaseRef{
		AccountID:            lease.Account.ID,
		CredentialGeneration: lease.CredentialGeneration,
		ExpiresAt:            lease.ExpiresAt,
	}
	c.mu.Unlock()
	return lease, nil
}

func (c *Client) Report(
	ctx context.Context,
	leaseID string,
	report LeaseReport,
) error {
	body := map[string]any{"outcome": report.Outcome}
	if report.StatusCode > 0 {
		body["statusCode"] = report.StatusCode
	}
	if report.CooldownScope != "" {
		body["scope"] = report.CooldownScope
	}
	if !report.RetryAt.IsZero() {
		body["retryAt"] = report.RetryAt.UnixMilli()
	}
	path := "/api/subrouter/leases/" + url.PathEscape(leaseID) + "/events"
	err := c.doJSON(ctx, http.MethodPost, path, body, true, nil)
	if report.Outcome == LeaseUnauthorized ||
		report.Outcome == LeaseForbidden ||
		report.Outcome == LeaseRateLimited {
		c.invalidateLease(leaseID)
	}
	return err
}

func (c *Client) invalidateLease(leaseID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ref, ok := c.leaseRefs[leaseID]
	if !ok {
		if key := c.leaseToKey[leaseID]; key != "" {
			delete(c.cache, key)
		}
		delete(c.leaseToKey, leaseID)
		return
	}
	for key, lease := range c.cache {
		if lease.Account.ID == ref.AccountID &&
			lease.CredentialGeneration == ref.CredentialGeneration {
			delete(c.cache, key)
		}
	}
	for id, candidate := range c.leaseRefs {
		if candidate.AccountID == ref.AccountID &&
			candidate.CredentialGeneration == ref.CredentialGeneration {
			delete(c.leaseRefs, id)
			delete(c.leaseToKey, id)
		}
	}
}

// InvalidateLease removes an access lease from memory before an asynchronous
// outcome report reaches cmux.com. This closes the window where a second local
// request could reuse a credential that just returned 401 or 429.
func (c *Client) InvalidateLease(leaseID string) {
	c.invalidateLease(leaseID)
}

func (c *Client) doJSON(
	ctx context.Context,
	method string,
	path string,
	body any,
	auth bool,
	out any,
) error {
	if err := c.Config.Validate(); err != nil {
		return err
	}
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	request, err := http.NewRequestWithContext(
		ctx,
		method,
		c.Config.BaseURL+path,
		bytes.NewReader(payload),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if auth {
		if !c.Config.LoggedIn() {
			return errors.New("not logged in; run 'sr login'")
		}
		request.Header.Set("Authorization", "Bearer "+c.Config.AccessToken)
		request.Header.Set("X-Stack-Refresh-Token", c.Config.RefreshToken)
		if c.Config.TeamID != "" {
			request.Header.Set("X-Cmux-Team-ID", c.Config.TeamID)
		}
	}
	response, err := c.httpClient().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return apiErrorFromResponse(response.StatusCode, data)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return errors.New("cmux.com returned an invalid response")
	}
	return nil
}

func (c *Client) httpClient() *http.Client {
	base := c.HTTPClient
	if base == nil {
		base = http.DefaultClient
	}
	client := *base
	// API redirects are never expected. Refusing them prevents the long-lived
	// Stack refresh token in a custom header from crossing to another origin.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client
}

func parseLease(raw leaseWire) (Lease, error) {
	if raw.LeaseID == "" || raw.AccountID == "" || raw.Token == "" {
		return Lease{}, errors.New("cmux.com returned an incomplete credential lease")
	}
	provider := account.Provider(raw.Provider)
	if provider != account.ProviderCodex && provider != account.ProviderClaude {
		return Lease{}, errors.New("cmux.com returned an invalid lease provider")
	}
	authMode := account.AuthMode(raw.AuthMode)
	if authMode != account.AuthModeOAuth && authMode != account.AuthModeAPIKey {
		return Lease{}, errors.New("cmux.com returned an invalid lease auth mode")
	}
	issuedAt, err := time.Parse(time.RFC3339, raw.IssuedAt)
	if err != nil {
		return Lease{}, errors.New("cmux.com returned an invalid lease issue time")
	}
	expiresAt, err := time.Parse(time.RFC3339, raw.ExpiresAt)
	if err != nil || !expiresAt.After(time.Now()) {
		return Lease{}, errors.New("cmux.com returned an expired credential lease")
	}
	var credentialExpiresAt time.Time
	if raw.CredentialExpiresAt != "" {
		credentialExpiresAt, err = time.Parse(time.RFC3339, raw.CredentialExpiresAt)
		if err != nil {
			return Lease{}, errors.New("cmux.com returned an invalid credential expiry")
		}
	}
	return Lease{
		ID: raw.LeaseID,
		Account: account.Account{
			ID:        raw.AccountID,
			Provider:  provider,
			AuthMode:  authMode,
			Label:     raw.Label,
			Email:     raw.Email,
			Token:     raw.Token,
			AccountID: raw.ProviderAccountID,
			Source:    "cmux-team-vault",
		},
		CredentialGeneration: raw.CredentialGeneration,
		IssuedAt:             issuedAt,
		ExpiresAt:            expiresAt,
		CredentialExpiresAt:  credentialExpiresAt,
	}, nil
}

func leaseCacheKey(input LeaseRequest) string {
	return strings.Join([]string{
		string(input.Provider),
		string(input.RequiredAuthMode),
		input.AgentType,
		input.SessionID,
		input.UserEmail,
		input.PreferAccountID,
		input.Model,
	}, "\x00")
}

func apiErrorFromResponse(status int, body []byte) error {
	// The central service may be relaying a provider failure. Never copy an
	// untrusted response body into a local error because it can contain an
	// echoed credential and errors are routinely logged by supervisors.
	_ = body
	message := http.StatusText(status)
	if message == "" {
		message = "request failed"
	}
	return fmt.Errorf("cmux.com request failed (%d): %s", status, message)
}

package stackauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const DefaultAPIURL = "https://api.stack-auth.com/api/v1"

type PublicConfig struct {
	Version int `json:"version"`
	Auth    struct {
		APIURL               string `json:"apiUrl"`
		ProjectID            string `json:"projectId"`
		PublishableClientKey string `json:"publishableClientKey"`
		ConfirmURL           string `json:"confirmUrl"`
	} `json:"auth"`
	Subrouter struct {
		URL         string `json:"url"`
		ExchangeURL string `json:"exchangeUrl"`
	} `json:"subrouter"`
}

func (c PublicConfig) Validate() error {
	for name, value := range map[string]string{
		"Stack API URL":          c.Auth.APIURL,
		"Stack project ID":       c.Auth.ProjectID,
		"Stack publishable key":  c.Auth.PublishableClientKey,
		"CLI confirmation URL":   c.Auth.ConfirmURL,
		"Subrouter URL":          c.Subrouter.URL,
		"Subrouter exchange URL": c.Subrouter.ExchangeURL,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is missing", name)
		}
	}
	for name, raw := range map[string]string{
		"Stack API URL":          c.Auth.APIURL,
		"CLI confirmation URL":   c.Auth.ConfirmURL,
		"Subrouter URL":          c.Subrouter.URL,
		"Subrouter exchange URL": c.Subrouter.ExchangeURL,
	} {
		if err := validateHTTPSOriginOrURL(raw); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func FetchPublicConfig(ctx context.Context, httpClient *http.Client, cmuxBaseURL string) (PublicConfig, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(cmuxBaseURL), "/"))
	if err != nil || base.Host == "" {
		return PublicConfig{}, errors.New("invalid cmux.com base URL")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/cli/config"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return PublicConfig{}, err
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return PublicConfig{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return PublicConfig{}, publicConfigResponseError(res)
	}
	var config PublicConfig
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&config); err != nil {
		return PublicConfig{}, fmt.Errorf("decode CLI configuration: %w", err)
	}
	if err := config.Validate(); err != nil {
		return PublicConfig{}, fmt.Errorf("invalid CLI configuration: %w", err)
	}
	return config, nil
}

type Client struct {
	APIURL               string
	ProjectID            string
	PublishableClientKey string
	HTTPClient           *http.Client
}

type HTTPError struct {
	Action     string
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf(
		"%s: HTTP %d: %s",
		e.Action,
		e.StatusCode,
		e.Message,
	)
}

// Retryable reports whether a failed Stack request is safe to retry while an
// existing CLI login deadline and context still bound the operation.
func Retryable(err error) bool {
	var responseErr *HTTPError
	if errors.As(err, &responseErr) {
		return responseErr.StatusCode == http.StatusRequestTimeout ||
			responseErr.StatusCode == http.StatusTooManyRequests ||
			responseErr.StatusCode >= http.StatusInternalServerError
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return true
	}
	for _, retryable := range []error{
		syscall.ECONNRESET,
		syscall.ECONNREFUSED,
		syscall.ECONNABORTED,
		syscall.EPIPE,
		syscall.ENETUNREACH,
		syscall.EHOSTUNREACH,
	} {
		if errors.Is(err, retryable) {
			return true
		}
	}
	return false
}

type CLIStart struct {
	PollingCode string    `json:"polling_code"`
	LoginCode   string    `json:"login_code"`
	ExpiresAt   time.Time `json:"-"`
}

func (s *CLIStart) UnmarshalJSON(body []byte) error {
	var wire struct {
		PollingCode string `json:"polling_code"`
		LoginCode   string `json:"login_code"`
		ExpiresAt   string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return err
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, wire.ExpiresAt)
	if err != nil {
		return fmt.Errorf("invalid CLI login expiration: %w", err)
	}
	s.PollingCode = wire.PollingCode
	s.LoginCode = wire.LoginCode
	s.ExpiresAt = expiresAt
	return nil
}

type CLIPoll struct {
	Status       string `json:"status"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

type SessionTokens struct {
	AccessToken  string
	RefreshToken string
}

type TenantExchange struct {
	TenantID     string   `json:"tenantId"`
	TenantName   string   `json:"tenantName"`
	TenantKey    string   `json:"tenantKey"`
	ProxyURL     string   `json:"proxyUrl"`
	Capabilities []string `json:"capabilities"`
}

type Team struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

func (c Client) StartCLI(ctx context.Context, expires time.Duration) (CLIStart, error) {
	if expires <= 0 {
		expires = 15 * time.Minute
	}
	var out CLIStart
	err := c.doJSON(ctx, http.MethodPost, "/auth/cli", map[string]any{
		"expires_in_millis": expires.Milliseconds(),
	}, "", http.StatusOK, &out)
	if err != nil {
		return CLIStart{}, err
	}
	if out.PollingCode == "" || out.LoginCode == "" {
		return CLIStart{}, errors.New("Stack Auth returned an incomplete CLI login")
	}
	return out, nil
}

func (c Client) PollCLI(ctx context.Context, pollingCode string) (CLIPoll, error) {
	var out CLIPoll
	err := c.doJSON(ctx, http.MethodPost, "/auth/cli/poll", map[string]string{
		"polling_code": pollingCode,
	}, "", 0, &out)
	if err != nil {
		return CLIPoll{}, err
	}
	return out, nil
}

func (c Client) Refresh(ctx context.Context, refreshToken string) (SessionTokens, error) {
	values := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {c.ProjectID},
		"client_secret": {c.PublishableClientKey},
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(c.apiURL(), "/")+"/auth/oauth/token",
		strings.NewReader(values.Encode()),
	)
	if err != nil {
		return SessionTokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := c.httpClient().Do(req)
	if err != nil {
		return SessionTokens{}, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return SessionTokens{}, responseError("refresh Stack session", res)
	}
	var wire struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&wire); err != nil {
		return SessionTokens{}, err
	}
	if wire.AccessToken == "" {
		return SessionTokens{}, errors.New("Stack Auth refresh returned no access token")
	}
	if wire.RefreshToken == "" {
		wire.RefreshToken = refreshToken
	}
	return SessionTokens{AccessToken: wire.AccessToken, RefreshToken: wire.RefreshToken}, nil
}

func (c Client) ListTeams(ctx context.Context, accessToken string) ([]Team, error) {
	var envelope struct {
		Items []Team `json:"items"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/teams?user_id=me", nil, accessToken, http.StatusOK, &envelope); err != nil {
		return nil, err
	}
	return envelope.Items, nil
}

func (c Client) SelectTeam(ctx context.Context, accessToken, teamID string) error {
	return c.doJSON(ctx, http.MethodPatch, "/users/me", map[string]string{
		"selected_team_id": teamID,
	}, accessToken, 0, nil)
}

func (c Client) SignOut(ctx context.Context, accessToken, refreshToken string) error {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodDelete,
		strings.TrimRight(c.apiURL(), "/")+"/auth/sessions/current",
		strings.NewReader("{}"),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setHeaders(req, accessToken)
	for _, prefix := range []string{"X-Stack", "X-Hexclave"} {
		req.Header.Set(prefix+"-Refresh-Token", refreshToken)
	}
	res, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return responseError("revoke Stack session", res)
	}
	return nil
}

func ExchangeTenant(
	ctx context.Context,
	httpClient *http.Client,
	exchangeURL string,
	accessToken string,
	refreshToken string,
	teamID string,
	teamName string,
) (TenantExchange, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	exchangeClient := &http.Client{
		Transport: httpClient.Transport,
		Jar:       httpClient.Jar,
		Timeout:   httpClient.Timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	payload, err := json.Marshal(map[string]string{"teamId": teamID, "teamName": teamName})
	if err != nil {
		return TenantExchange{}, err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimSpace(exchangeURL),
		bytes.NewReader(payload),
	)
	if err != nil {
		return TenantExchange{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-Stack-Refresh-Token", refreshToken)
	req.Header.Set("X-Cmux-Team-Id", teamID)
	res, err := exchangeClient.Do(req)
	if err != nil {
		return TenantExchange{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return TenantExchange{}, responseError("exchange Stack login for hosted tenant", res)
	}
	var out TenantExchange
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out); err != nil {
		return TenantExchange{}, err
	}
	if out.TenantID == "" || out.TenantKey == "" || out.ProxyURL == "" ||
		!validTenantCapabilities(out.Capabilities) {
		return TenantExchange{}, errors.New("hosted Subrouter returned an incomplete tenant")
	}
	return out, nil
}

func validTenantCapabilities(values []string) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if value != "use" && value != "manage_accounts" {
			return false
		}
	}
	return true
}

// ParseClaimsUnverified reads identity hints used to select the user's team in
// the CLI. The hosted service independently verifies the signature and every
// security-sensitive claim before issuing a tenant key.
func ParseClaimsUnverified(token string) (Claims, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("malformed Stack access token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, errors.New("malformed Stack token payload")
	}
	var wire struct {
		Subject        string `json:"sub"`
		ProjectID      string `json:"project_id"`
		SelectedTeamID string `json:"selected_team_id"`
		Email          string `json:"email"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return Claims{}, errors.New("malformed Stack token payload")
	}
	return Claims{
		Subject: wire.Subject, ProjectID: wire.ProjectID,
		SelectedTeamID: wire.SelectedTeamID, Email: wire.Email,
	}, nil
}

func (c Client) doJSON(
	ctx context.Context,
	method string,
	path string,
	input any,
	accessToken string,
	expectedStatus int,
	output any,
) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(
		ctx,
		method,
		strings.TrimRight(c.apiURL(), "/")+path,
		body,
	)
	if err != nil {
		return err
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.setHeaders(req, accessToken)
	res, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if expectedStatus != 0 {
		if res.StatusCode != expectedStatus {
			return responseError(method+" "+path, res)
		}
	} else if res.StatusCode < 200 || res.StatusCode >= 300 {
		return responseError(method+" "+path, res)
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(output)
}

func (c Client) setHeaders(req *http.Request, accessToken string) {
	for _, prefix := range []string{"X-Stack", "X-Hexclave"} {
		req.Header.Set(prefix+"-Project-Id", c.ProjectID)
		req.Header.Set(prefix+"-Access-Type", "client")
		req.Header.Set(prefix+"-Publishable-Client-Key", c.PublishableClientKey)
		if accessToken != "" {
			req.Header.Set(prefix+"-Access-Token", accessToken)
		}
	}
	req.Header.Set("User-Agent", "subrouter-cli")
}

func (c Client) apiURL() string {
	if strings.TrimSpace(c.APIURL) == "" {
		return DefaultAPIURL
	}
	return strings.TrimRight(c.APIURL, "/")
}

func (c Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func responseError(action string, res *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	return responseErrorFromBody(action, res.StatusCode, body)
}

func publicConfigResponseError(res *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	var envelope struct {
		Error string `json:"error"`
	}
	if res.StatusCode == http.StatusServiceUnavailable &&
		json.Unmarshal(body, &envelope) == nil &&
		envelope.Error == "cli_auth_unavailable" {
		return errors.New("cmux.com login is temporarily unavailable; try again later")
	}
	return responseErrorFromBody("load CLI configuration", res.StatusCode, body)
}

func responseErrorFromBody(action string, statusCode int, body []byte) error {
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(statusCode)
	}
	return &HTTPError{
		Action: action, StatusCode: statusCode, Message: message,
	}
}

func validateHTTPSOriginOrURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return errors.New("invalid URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("URL cannot contain credentials, query, or fragment")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	ip := net.ParseIP(host)
	if parsed.Scheme == "http" &&
		(host == "localhost" || (ip != nil && ip.IsLoopback())) {
		return nil
	}
	return errors.New("URL must use HTTPS, except on loopback")
}

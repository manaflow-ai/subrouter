// Package oauthdevice implements the OAuth 2.0 device authorization grant
// (RFC 8628), which is how coding-CLI subscriptions are signed into on a
// machine with no browser callback. Providers differ only in their endpoints
// and client, so the flow lives here once.
package oauthdevice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Config identifies one provider's device-flow endpoints and client.
type Config struct {
	ClientID string
	// ClientSecret is optional. Installed-app clients often carry one, and it
	// is not confidential; a provider that omits it simply leaves this empty.
	ClientSecret string
	// DeviceAuthURL issues device and user codes.
	DeviceAuthURL string
	// TokenURL exchanges an authorized device code, and later refresh tokens.
	TokenURL string
	Scope    string
	// ExtraAuthParams are provider-specific fields on the device-code request.
	ExtraAuthParams map[string]string
	// Headers are provider-required client identity fields sent on device-code,
	// polling, and refresh requests.
	Headers map[string]string
}

// Code is the device-authorization response. The caller shows UserCode and
// VerificationURI to the person signing in.
type Code struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	// VerificationURIComplete embeds the user code so the person can follow one
	// link instead of typing the code. Empty when the provider does not send it.
	VerificationURIComplete string
	Interval                time.Duration
	ExpiresAt               time.Time
}

// Token is a successful token response.
type Token struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	TokenType    string
	Scope        string
	ExpiresAt    time.Time
}

const (
	// deviceCodeGrantType is the RFC 8628 grant type.
	deviceCodeGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	// defaultInterval applies when the provider omits one, per RFC 8628 §3.2.
	defaultInterval = 5 * time.Second
	// slowDownIncrement is the interval bump RFC 8628 §3.5 requires on
	// slow_down. Ignoring it gets the client rate-limited out of the flow.
	slowDownIncrement = 5 * time.Second
	// defaultExpiry applies when the provider omits expires_in on the device code.
	defaultExpiry = 15 * time.Minute
	// malformedTokenResponseRetries lets a transient intermediary response heal,
	// but prevents a persistently broken 2xx endpoint from hiding the decode
	// failure until the device code expires.
	malformedTokenResponseRetries = 2
)

// ErrAuthorizationDeclined reports that the person refused the sign-in.
var ErrAuthorizationDeclined = errors.New("device authorization was declined")

// ErrAuthorizationExpired reports that the device code expired before the
// person completed the sign-in.
var ErrAuthorizationExpired = errors.New("device authorization expired before it was approved")

type deviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIAlt      string `json:"verification_url"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int64  `json:"interval"`
	ExpiresIn               int64  `json:"expires_in"`
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	IDToken          string `json:"id_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// RequestCode starts a sign-in and returns the codes to show the person.
func RequestCode(ctx context.Context, client *http.Client, cfg Config, now time.Time) (Code, error) {
	form := url.Values{"client_id": {cfg.ClientID}}
	if cfg.Scope != "" {
		form.Set("scope", cfg.Scope)
	}
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}
	for key, value := range cfg.ExtraAuthParams {
		form.Set(key, value)
	}
	body, err := postForm(ctx, client, cfg.DeviceAuthURL, form, cfg.Headers)
	if err != nil {
		return Code{}, err
	}
	var parsed deviceCodeResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Code{}, fmt.Errorf("device authorization returned an undecodable body: %w", err)
	}
	if strings.TrimSpace(parsed.DeviceCode) == "" {
		return Code{}, errors.New("device authorization returned no device code")
	}
	if strings.TrimSpace(parsed.UserCode) == "" {
		return Code{}, errors.New("device authorization returned no user code")
	}
	verificationURI := firstNonEmpty(parsed.VerificationURI, parsed.VerificationURIAlt)
	if strings.TrimSpace(verificationURI) == "" {
		return Code{}, errors.New("device authorization returned no verification URI")
	}
	code := Code{
		DeviceCode:              parsed.DeviceCode,
		UserCode:                parsed.UserCode,
		VerificationURI:         verificationURI,
		VerificationURIComplete: parsed.VerificationURIComplete,
		Interval:                defaultInterval,
		ExpiresAt:               now.Add(defaultExpiry),
	}
	if parsed.Interval > 0 {
		code.Interval = time.Duration(parsed.Interval) * time.Second
	}
	if parsed.ExpiresIn > 0 {
		code.ExpiresAt = now.Add(time.Duration(parsed.ExpiresIn) * time.Second)
	}
	return code, nil
}

// Poll waits for the person to approve the sign-in and returns the token.
// It honours the provider's polling interval and the slow_down backoff RFC 8628
// requires, and gives up when the device code expires.
func Poll(ctx context.Context, client *http.Client, cfg Config, code Code, sleep func(context.Context, time.Duration) error, now func() time.Time) (Token, error) {
	if sleep == nil {
		sleep = sleepContext
	}
	if now == nil {
		now = time.Now
	}
	interval := code.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	nextDelay := interval
	malformedResponses := 0
	for {
		pollNow := now()
		if !code.ExpiresAt.IsZero() && !pollNow.Before(code.ExpiresAt) {
			return Token{}, ErrAuthorizationExpired
		}
		if !code.ExpiresAt.IsZero() {
			remaining := code.ExpiresAt.Sub(pollNow)
			if nextDelay > remaining {
				nextDelay = remaining
			}
		}
		if err := sleep(ctx, nextDelay); err != nil {
			return Token{}, err
		}
		nextDelay = interval
		if !code.ExpiresAt.IsZero() && !now().Before(code.ExpiresAt) {
			return Token{}, ErrAuthorizationExpired
		}
		form := url.Values{
			"client_id":   {cfg.ClientID},
			"device_code": {code.DeviceCode},
			"grant_type":  {deviceCodeGrantType},
		}
		if cfg.ClientSecret != "" {
			form.Set("client_secret", cfg.ClientSecret)
		}
		token, retry, err := exchange(ctx, client, cfg, form, now())
		if err == nil {
			return token, nil
		}
		if !retry {
			return Token{}, err
		}
		var malformedErr *malformedTokenResponseError
		if errors.As(err, &malformedErr) {
			malformedResponses++
			if malformedResponses > malformedTokenResponseRetries {
				return Token{}, err
			}
		} else {
			malformedResponses = 0
		}
		if errors.Is(err, errSlowDown) {
			interval += slowDownIncrement
			nextDelay = interval
		}
		if retryAfter, ok := pollingRetryAfter(err, now()); ok && retryAfter > nextDelay {
			nextDelay = retryAfter
		}
	}
}

// Refresh exchanges a refresh token for a new access token.
func Refresh(ctx context.Context, client *http.Client, cfg Config, refreshToken string, now time.Time) (Token, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return Token{}, errors.New("no refresh token")
	}
	form := url.Values{
		"client_id":     {cfg.ClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}
	token, _, err := exchange(ctx, client, cfg, form, now)
	if err != nil {
		return Token{}, err
	}
	// A provider that does not rotate the refresh token omits it; keeping the
	// caller's avoids blanking a credential that is still valid.
	if token.RefreshToken == "" {
		token.RefreshToken = refreshToken
	}
	return token, nil
}

// errSlowDown marks the retryable slow_down response so Poll can back off.
var errSlowDown = errors.New("slow_down")

// errAuthorizationPending marks the ordinary "not approved yet" response.
var errAuthorizationPending = errors.New("authorization_pending")

type malformedTokenResponseError struct {
	err error
}

func (err *malformedTokenResponseError) Error() string { return err.err.Error() }
func (err *malformedTokenResponseError) Unwrap() error { return err.err }

// exchange posts a token request and classifies the response. retry reports
// whether Poll should keep waiting.
func exchange(ctx context.Context, client *http.Client, cfg Config, form url.Values, now time.Time) (token Token, retry bool, err error) {
	body, httpErr := postForm(ctx, client, cfg.TokenURL, form, cfg.Headers)
	var parsed tokenResponse
	parseErr := json.Unmarshal(body, &parsed)
	if parseErr != nil {
		parseErr = &malformedTokenResponseError{err: fmt.Errorf("device flow token endpoint returned an undecodable body: %w", parseErr)}
	}
	switch parsed.Error {
	case "authorization_pending":
		if httpErr != nil {
			return Token{}, true, errors.Join(errAuthorizationPending, httpErr)
		}
		return Token{}, true, errAuthorizationPending
	case "slow_down":
		// Providers sometimes combine RFC 8628's slow_down body with HTTP 429
		// and Retry-After. Preserve both classifications: Poll must apply the
		// mandatory five-second interval increase and also honour a longer
		// server-requested delay.
		if httpErr != nil {
			return Token{}, true, errors.Join(errSlowDown, httpErr)
		}
		return Token{}, true, errSlowDown
	case "access_denied":
		return Token{}, false, ErrAuthorizationDeclined
	case "expired_token":
		return Token{}, false, ErrAuthorizationExpired
	}
	if httpErr != nil && retryablePollingEndpointError(ctx, httpErr) {
		return Token{}, true, httpErr
	}
	if parsed.Error != "" {
		if httpErr != nil {
			return Token{}, false, fmt.Errorf("device flow token error %s: %w", parsed.Error, httpErr)
		}
		return Token{}, false, fmt.Errorf("device flow token error %s", parsed.Error)
	}
	if httpErr != nil {
		return Token{}, false, httpErr
	}
	if parseErr != nil {
		return Token{}, true, parseErr
	}
	if strings.TrimSpace(parsed.AccessToken) == "" {
		return Token{}, false, errors.New("token response carried no access token")
	}
	token = Token{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		IDToken:      parsed.IDToken,
		TokenType:    parsed.TokenType,
		Scope:        parsed.Scope,
	}
	if parsed.ExpiresIn > 0 {
		token.ExpiresAt = now.Add(time.Duration(parsed.ExpiresIn) * time.Second).UTC()
	}
	return token, false, nil
}

type endpointStatusError struct {
	endpoint   string
	status     string
	statusCode int
	retryAfter string
}

func (err *endpointStatusError) Error() string {
	return fmt.Sprintf("device flow request to %s failed: %s", err.endpoint, err.status)
}

type endpointTransportError struct {
	err error
}

func (err *endpointTransportError) Error() string { return err.err.Error() }
func (err *endpointTransportError) Unwrap() error { return err.err }

func retryablePollingEndpointError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var statusErr *endpointStatusError
	if errors.As(err, &statusErr) {
		return retryablePollingStatus(statusErr.statusCode)
	}
	var transportErr *endpointTransportError
	if !errors.As(err, &transportErr) {
		return false
	}
	return retryablePollingTransportError(transportErr.err)
}

func retryablePollingStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusRequestTimeout,
		http.StatusTooEarly,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func pollingRetryAfter(err error, now time.Time) (time.Duration, bool) {
	var statusErr *endpointStatusError
	if !errors.As(err, &statusErr) || statusErr.statusCode != http.StatusTooManyRequests {
		return 0, false
	}
	value := strings.TrimSpace(statusErr.retryAfter)
	if value == "" {
		return 0, false
	}
	if seconds, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil {
		if seconds < 0 || seconds > int64((time.Duration(1<<63-1))/time.Second) {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	retryAt, parseErr := http.ParseTime(value)
	if parseErr != nil {
		return 0, false
	}
	delay := retryAt.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

// retryablePollingTransportError deliberately recognizes transient failures
// rather than assuming every RoundTripper error will heal. TLS trust failures,
// invalid proxy configuration, and permanent DNS errors otherwise turn an
// actionable sign-in failure into a silent polling loop that lasts until the
// device code expires.
func retryablePollingTransportError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	// An http.Client timeout has its own deadline even while the device-flow
	// parent context and device code remain valid.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.Timeout() || dnsErr.Temporary()
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	for _, transient := range []error{
		syscall.ECONNRESET,
		syscall.ECONNABORTED,
		syscall.ECONNREFUSED,
		syscall.ENETDOWN,
		syscall.ENETUNREACH,
		syscall.EHOSTDOWN,
		syscall.EHOSTUNREACH,
		syscall.ETIMEDOUT,
		syscall.EPIPE,
		io.EOF,
		io.ErrUnexpectedEOF,
	} {
		if errors.Is(err, transient) {
			return true
		}
	}
	return false
}

func postForm(ctx context.Context, client *http.Client, endpoint string, form url.Values, headers map[string]string) ([]byte, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	for name, value := range headers {
		if strings.TrimSpace(name) != "" && strings.TrimSpace(value) != "" {
			req.Header.Set(name, value)
		}
	}
	requestClient := *client
	requestClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	res, err := requestClient.Do(req)
	if err != nil {
		return nil, &endpointTransportError{err: err}
	}
	defer func() { _ = res.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if readErr != nil {
		return nil, &endpointTransportError{err: readErr}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// The body is returned alongside the error because RFC 8628 signals
		// authorization_pending and slow_down with a 400.
		return body, &endpointStatusError{
			endpoint:   endpoint,
			status:     res.Status,
			statusCode: res.StatusCode,
			retryAfter: res.Header.Get("Retry-After"),
		}
	}
	return body, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

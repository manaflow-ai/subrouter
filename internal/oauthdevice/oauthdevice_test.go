package oauthdevice

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"
)

type oauthRoundTripFunc func(*http.Request) (*http.Response, error)

func (f oauthRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

var reference = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

func testConfig(server *httptest.Server) Config {
	return Config{
		ClientID:      "client",
		ClientSecret:  "secret",
		DeviceAuthURL: server.URL + "/device",
		TokenURL:      server.URL + "/token",
		Scope:         "openid offline_access",
	}
}

func TestRequestCodeParsesTheDeviceAuthorization(t *testing.T) {
	var gotForm string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.Form.Encode()
		_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"WXYZ-1234","verification_uri":"https://example.test/device","verification_uri_complete":"https://example.test/device?user_code=WXYZ-1234","interval":7,"expires_in":600}`))
	}))
	defer server.Close()

	code, err := RequestCode(context.Background(), server.Client(), testConfig(server), reference)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if code.DeviceCode != "dc" || code.UserCode != "WXYZ-1234" {
		t.Fatalf("codes did not round-trip: %+v", code)
	}
	if code.VerificationURI != "https://example.test/device" {
		t.Fatalf("VerificationURI = %q", code.VerificationURI)
	}
	if code.Interval != 7*time.Second {
		t.Fatalf("Interval = %s, want the provider's 7s", code.Interval)
	}
	if want := reference.Add(10 * time.Minute); !code.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want %s", code.ExpiresAt, want)
	}
	for _, want := range []string{"client_id=client", "scope=openid", "client_secret=secret"} {
		if !strings.Contains(gotForm, want) {
			t.Fatal("device request form is missing an expected field")
		}
	}
}

func TestPostFormDoesNotReplayCredentialsAcrossRedirects(t *testing.T) {
	var sinkCalls int
	var parseErr error
	var gotRefreshToken string
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		sinkCalls++
	}))
	defer sink.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			parseErr = err
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		gotRefreshToken = request.Form.Get("refresh_token")
		http.Redirect(w, request, sink.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := postForm(t.Context(), redirector.Client(), redirector.URL,
		map[string][]string{"refresh_token": {"refresh-secret"}}, nil)
	if parseErr != nil {
		t.Fatalf("source request form could not be parsed: %v", parseErr)
	}
	if gotRefreshToken != "refresh-secret" {
		t.Fatal("source request omitted the refresh token")
	}
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("postForm error = %v, want rejected redirect", err)
	}
	if sinkCalls != 0 {
		t.Fatalf("redirect sink received %d credential-bearing requests", sinkCalls)
	}
}

// Some providers spell it verification_url. Falling back matters because an
// empty URI means the person has nowhere to go.
func TestRequestCodeAcceptsTheAlternateVerificationField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"U","verification_url":"https://alt.test/device"}`))
	}))
	defer server.Close()

	code, err := RequestCode(context.Background(), server.Client(), testConfig(server), reference)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if code.VerificationURI != "https://alt.test/device" {
		t.Fatalf("VerificationURI = %q, want the verification_url fallback", code.VerificationURI)
	}
	// Defaults apply when the provider omits them.
	if code.Interval != defaultInterval {
		t.Fatalf("Interval = %s, want the RFC default", code.Interval)
	}
	if want := reference.Add(defaultExpiry); !code.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want the default expiry", code.ExpiresAt)
	}
}

func TestRequestCodeRejectsIncompleteResponses(t *testing.T) {
	for _, body := range []string{
		`{"user_code":"U","verification_uri":"https://example.test"}`,
		`{"device_code":"D","verification_uri":"https://example.test"}`,
		`{"device_code":"D","user_code":"U"}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		_, err := RequestCode(context.Background(), server.Client(), testConfig(server), reference)
		server.Close()
		if err == nil {
			t.Fatalf("incomplete response %s should fail", body)
		}
	}
}

// authorization_pending is the ordinary case: keep polling until approval.
func TestPollWaitsThroughAuthorizationPending(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer server.Close()

	token, err := Poll(context.Background(), server.Client(), testConfig(server),
		Code{DeviceCode: "dc", Interval: time.Millisecond, ExpiresAt: reference.Add(time.Hour)},
		noSleep, func() time.Time { return reference })
	if err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	if token.AccessToken != "at" || token.RefreshToken != "rt" {
		t.Fatalf("token did not round-trip: %+v", token)
	}
	if want := reference.Add(time.Hour); !token.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want %s", token.ExpiresAt, want)
	}
	if calls != 3 {
		t.Fatalf("polled %d times, want 3", calls)
	}
}

// RFC 8628 §3.5 requires the interval to grow on slow_down. Ignoring it gets
// the client rate-limited out of its own sign-in.
func TestPollBacksOffOnSlowDown(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"slow_down"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"at","expires_in":60}`))
	}))
	defer server.Close()

	var waited []time.Duration
	record := func(_ context.Context, d time.Duration) error {
		waited = append(waited, d)
		return nil
	}
	if _, err := Poll(context.Background(), server.Client(), testConfig(server),
		Code{DeviceCode: "dc", Interval: 5 * time.Second, ExpiresAt: reference.Add(time.Hour)},
		record, func() time.Time { return reference }); err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	if len(waited) != 2 {
		t.Fatalf("waited %v, want two intervals", waited)
	}
	if waited[0] != 5*time.Second {
		t.Fatalf("first interval = %s, want the provider's 5s", waited[0])
	}
	if waited[1] != 10*time.Second {
		t.Fatalf("second interval = %s, want 5s + the RFC's 5s increment", waited[1])
	}
}

func TestPollHonoursRetryAfterAndSlowDownIncrementTogether(t *testing.T) {
	reference := time.Unix(100, 0)
	code := Code{DeviceCode: "device", Interval: 2 * time.Second, ExpiresAt: reference.Add(time.Minute)}
	for _, test := range []struct {
		name       string
		retryAfter string
		wantDelay  time.Duration
	}{
		{name: "server delay is longer", retryAfter: "30", wantDelay: 30 * time.Second},
		{name: "RFC increment is longer", retryAfter: "3", wantDelay: 7 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls == 1 {
					w.Header().Set("Retry-After", test.retryAfter)
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"error":"slow_down"}`))
					return
				}
				_, _ = w.Write([]byte(`{"access_token":"approved"}`))
			}))
			defer server.Close()
			var delays []time.Duration
			_, err := Poll(t.Context(), server.Client(), Config{ClientID: "client", TokenURL: server.URL}, code,
				func(_ context.Context, delay time.Duration) error {
					delays = append(delays, delay)
					return nil
				}, func() time.Time { return reference })
			if err != nil {
				t.Fatal(err)
			}
			if len(delays) != 2 || delays[0] != 2*time.Second || delays[1] != test.wantDelay {
				t.Fatalf("poll delays = %v, want [2s %s]", delays, test.wantDelay)
			}
		})
	}
}

func TestPollRetriesTransientEndpointFailures(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		switch calls {
		case 1:
			http.Error(w, "upstream temporarily unavailable", http.StatusServiceUnavailable)
		case 2:
			_, _ = w.Write([]byte("temporary gateway response"))
		default:
			_, _ = w.Write([]byte(`{"access_token":"at"}`))
		}
	}))
	defer server.Close()

	token, err := Poll(context.Background(), server.Client(), testConfig(server),
		Code{DeviceCode: "dc", Interval: time.Millisecond, ExpiresAt: reference.Add(time.Hour)},
		noSleep, func() time.Time { return reference })
	if err != nil {
		t.Fatalf("poll failed after transient endpoint responses: %v", err)
	}
	if token.AccessToken != "at" || calls != 3 {
		t.Fatalf("token = %+v, calls = %d; want success on the third poll", token, calls)
	}
}

func TestPollHonoursRetryAfterOnTooManyRequests(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"at"}`))
	}))
	defer server.Close()

	var waited []time.Duration
	token, err := Poll(t.Context(), server.Client(), testConfig(server),
		Code{DeviceCode: "dc", Interval: time.Second, ExpiresAt: reference.Add(time.Hour)},
		func(_ context.Context, delay time.Duration) error {
			waited = append(waited, delay)
			return nil
		}, func() time.Time { return reference })
	if err != nil {
		t.Fatalf("poll failed after Retry-After: %v", err)
	}
	if token.AccessToken != "at" || calls != 2 {
		t.Fatalf("token = %+v, calls = %d; want success on the second poll", token, calls)
	}
	if len(waited) != 2 || waited[0] != time.Second || waited[1] != 30*time.Second {
		t.Fatalf("waits = %v, want [1s 30s]", waited)
	}
}

func TestPollingRetryAfterParsesSecondsAndHTTPDates(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{name: "seconds", value: "45", want: 45 * time.Second, ok: true},
		{name: "http date", value: reference.Add(90 * time.Second).Format(http.TimeFormat), want: 90 * time.Second, ok: true},
		{name: "past date", value: reference.Add(-time.Minute).Format(http.TimeFormat), want: 0, ok: true},
		{name: "negative seconds", value: "-1"},
		{name: "invalid", value: "later"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pollingRetryAfter(&endpointStatusError{
				statusCode: http.StatusTooManyRequests,
				retryAfter: tc.value,
			}, reference)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("pollingRetryAfter(%q) = (%s, %t), want (%s, %t)", tc.value, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestPollBoundsRetryAfterByDeviceCodeExpiry(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Retry-After", "300")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	now := reference
	var waited []time.Duration
	_, err := Poll(t.Context(), server.Client(), testConfig(server),
		Code{DeviceCode: "dc", Interval: time.Second, ExpiresAt: reference.Add(10 * time.Second)},
		func(_ context.Context, delay time.Duration) error {
			waited = append(waited, delay)
			now = now.Add(delay)
			return nil
		}, func() time.Time { return now })
	if !errors.Is(err, ErrAuthorizationExpired) {
		t.Fatalf("err = %v, want ErrAuthorizationExpired", err)
	}
	if calls != 1 || len(waited) != 2 || waited[0] != time.Second || waited[1] != 9*time.Second {
		t.Fatalf("calls = %d, waits = %v; want one request and waits [1s 9s]", calls, waited)
	}
}

func TestPollStopsAfterBoundedMalformedSuccessfulResponses(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":`))
	}))
	defer server.Close()

	_, err := Poll(t.Context(), server.Client(), testConfig(server),
		Code{DeviceCode: "dc", Interval: time.Second, ExpiresAt: reference.Add(time.Hour)},
		noSleep, func() time.Time { return reference })
	if err == nil || !strings.Contains(err.Error(), "undecodable body") || !strings.Contains(err.Error(), "unexpected end of JSON input") {
		t.Fatalf("err = %v, want actionable token-response decode error", err)
	}
	if calls != 1+malformedTokenResponseRetries {
		t.Fatalf("poll attempts = %d, want %d", calls, 1+malformedTokenResponseRetries)
	}
}

func TestPollRetriesTransientTransportFailure(t *testing.T) {
	var calls int
	client := &http.Client{Transport: oauthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		switch calls {
		case 1:
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
		case 2:
			// A per-request client timeout is transient while the device-flow
			// parent context and device code remain live.
			return nil, context.DeadlineExceeded
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"at"}`)),
		}, nil
	})}

	token, err := Poll(context.Background(), client, Config{TokenURL: "https://oauth.example/token"},
		Code{DeviceCode: "dc", Interval: time.Millisecond, ExpiresAt: reference.Add(time.Hour)},
		noSleep, func() time.Time { return reference })
	if err != nil {
		t.Fatalf("poll failed after transient transport error: %v", err)
	}
	if token.AccessToken != "at" || calls != 3 {
		t.Fatalf("token = %+v, calls = %d; want success on the third poll", token, calls)
	}
}

func TestPollReturnsPermanentTransportFailuresPromptly(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want func(error) bool
	}{
		{
			name: "unknown certificate authority",
			err:  x509.UnknownAuthorityError{Cert: &x509.Certificate{}},
			want: func(err error) bool {
				var target x509.UnknownAuthorityError
				return errors.As(err, &target)
			},
		},
		{
			name: "invalid proxy configuration",
			err: &url.Error{
				Op:  "proxyconnect",
				URL: "https://oauth.example/token",
				Err: url.InvalidHostError("invalid proxy host"),
			},
			want: func(err error) bool {
				var target url.InvalidHostError
				return errors.As(err, &target)
			},
		},
		{
			name: "non-temporary DNS failure",
			err:  &net.DNSError{Err: "no such host", Name: "oauth.invalid", IsNotFound: true},
			want: func(err error) bool {
				var target *net.DNSError
				return errors.As(err, &target) && target.IsNotFound
			},
		},
		{
			name: "unclassified transport failure",
			err:  errors.New("transport configuration rejected"),
			want: func(err error) bool {
				return strings.Contains(err.Error(), "transport configuration rejected")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			var waits int
			stopRetry := errors.New("test stopped an unexpected retry")
			client := &http.Client{Transport: oauthRoundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, tc.err
			})}
			sleep := func(context.Context, time.Duration) error {
				waits++
				if waits > 1 {
					return stopRetry
				}
				return nil
			}

			_, err := Poll(t.Context(), client, Config{TokenURL: "https://oauth.example/token"},
				Code{DeviceCode: "dc", Interval: time.Millisecond, ExpiresAt: reference.Add(time.Hour)},
				sleep, func() time.Time { return reference })
			if err == nil || !tc.want(err) {
				t.Fatalf("err = %v, want original actionable transport failure", err)
			}
			if calls != 1 {
				t.Fatalf("poll attempts = %d, want one for a permanent transport failure", calls)
			}
		})
	}
}

func TestPollDoesNotRetryRejectedRedirect(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Redirect(w, r, "/different-token-endpoint", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	_, err := Poll(t.Context(), server.Client(), testConfig(server),
		Code{DeviceCode: "dc", Interval: time.Millisecond, ExpiresAt: reference.Add(time.Hour)},
		noSleep, func() time.Time { return reference })
	if err == nil || !strings.Contains(err.Error(), "307 Temporary Redirect") {
		t.Fatalf("err = %v, want the rejected redirect status", err)
	}
	if calls != 1 {
		t.Fatalf("poll attempts = %d, want one for a rejected redirect", calls)
	}
}

func TestPollStopsWhenTransportFailureCancelsTheFlow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	client := &http.Client{Transport: oauthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		cancel()
		return nil, context.Canceled
	})}

	_, err := Poll(ctx, client, Config{TokenURL: "https://oauth.example/token"},
		Code{DeviceCode: "dc", Interval: time.Millisecond, ExpiresAt: reference.Add(time.Hour)},
		noSleep, func() time.Time { return reference })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("poll attempts = %d, want one after parent cancellation", calls)
	}
}

func TestPollTransientFailuresRemainBoundedByDeviceCodeExpiry(t *testing.T) {
	var calls int
	client := &http.Client{Transport: oauthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	})}
	now := reference
	advance := func(context.Context, time.Duration) error {
		now = now.Add(time.Second)
		return nil
	}

	_, err := Poll(context.Background(), client, Config{TokenURL: "https://oauth.example/token"},
		Code{DeviceCode: "dc", Interval: time.Second, ExpiresAt: reference.Add(2 * time.Second)},
		advance, func() time.Time { return now })
	if !errors.Is(err, ErrAuthorizationExpired) {
		t.Fatalf("err = %v, want ErrAuthorizationExpired", err)
	}
	if calls != 1 {
		t.Fatalf("poll attempts = %d, want one attempt before local expiry", calls)
	}
}

func TestPollDoesNotRetryNonTransientEndpointFailure(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotImplemented} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls int
			var waits int
			stopRetry := errors.New("test stopped an unexpected retry")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				http.Error(w, "non-retryable device request", status)
			}))
			defer server.Close()
			sleep := func(context.Context, time.Duration) error {
				waits++
				if waits > 1 {
					return stopRetry
				}
				return nil
			}

			_, err := Poll(t.Context(), server.Client(), testConfig(server),
				Code{DeviceCode: "dc", Interval: time.Millisecond, ExpiresAt: reference.Add(time.Hour)},
				sleep, func() time.Time { return reference })
			if err == nil || !strings.Contains(err.Error(), http.StatusText(status)) {
				t.Fatalf("err = %v, want terminal %d endpoint failure", err, status)
			}
			if calls != 1 {
				t.Fatalf("poll attempts = %d, want one for a non-transient failure", calls)
			}
		})
	}
}

func TestPollStopsOnDeclinedAndExpired(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{name: "declined", body: `{"error":"access_denied"}`, want: ErrAuthorizationDeclined},
		{name: "expired", body: `{"error":"expired_token"}`, want: ErrAuthorizationExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			_, err := Poll(context.Background(), server.Client(), testConfig(server),
				Code{DeviceCode: "dc", Interval: time.Millisecond, ExpiresAt: reference.Add(time.Hour)},
				noSleep, func() time.Time { return reference })
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if calls != 1 {
				t.Fatalf("polled %d times, want to stop after the first refusal", calls)
			}
		})
	}
}

// A device code that has run out must stop the loop locally rather than poll a
// dead code forever.
func TestPollGivesUpOnceTheDeviceCodeExpires(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))
	defer server.Close()

	_, err := Poll(context.Background(), server.Client(), testConfig(server),
		Code{DeviceCode: "dc", Interval: time.Millisecond, ExpiresAt: reference.Add(-time.Second)},
		noSleep, func() time.Time { return reference })
	if !errors.Is(err, ErrAuthorizationExpired) {
		t.Fatalf("err = %v, want ErrAuthorizationExpired", err)
	}
	if calls != 0 {
		t.Fatalf("polled %d times, want none once the code has expired", calls)
	}
}

func TestPollRechecksExpiryAfterWaiting(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	now := reference
	advance := func(context.Context, time.Duration) error {
		now = reference.Add(time.Second)
		return nil
	}
	_, err := Poll(context.Background(), server.Client(), testConfig(server),
		Code{DeviceCode: "dc", Interval: time.Second, ExpiresAt: reference.Add(time.Second)},
		advance, func() time.Time { return now })
	if !errors.Is(err, ErrAuthorizationExpired) || calls != 0 {
		t.Fatalf("err = %v, calls = %d; want local expiry before polling", err, calls)
	}
}

func TestPollDoesNotCapSlowDownBackoff(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls <= 2 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"slow_down"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"at"}`))
	}))
	defer server.Close()
	var waited []time.Duration
	_, err := Poll(context.Background(), server.Client(), testConfig(server),
		Code{DeviceCode: "dc", Interval: 58 * time.Second, ExpiresAt: reference.Add(time.Hour)},
		func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil },
		func() time.Time { return reference })
	if err != nil || len(waited) != 3 || waited[2] != 68*time.Second {
		t.Fatalf("err = %v, waits = %v; want uncapped 58s, 63s, 68s", err, waited)
	}
}

func TestPollHonoursContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Poll(ctx, server.Client(), testConfig(server),
		Code{DeviceCode: "dc", Interval: time.Millisecond, ExpiresAt: reference.Add(time.Hour)},
		sleepContext, func() time.Time { return reference })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRefreshPreservesANonRotatedRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"fresh","expires_in":3600}`))
	}))
	defer server.Close()

	token, err := Refresh(context.Background(), server.Client(), testConfig(server), "original", reference)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if token.RefreshToken != "original" {
		t.Fatalf("RefreshToken = %q, want the caller's token preserved", token.RefreshToken)
	}
	if token.AccessToken != "fresh" {
		t.Fatalf("AccessToken = %q", token.AccessToken)
	}
}

func TestRefreshAdoptsARotatedRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"fresh","refresh_token":"rotated","expires_in":3600}`))
	}))
	defer server.Close()

	token, err := Refresh(context.Background(), server.Client(), testConfig(server), "original", reference)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if token.RefreshToken != "rotated" {
		t.Fatalf("RefreshToken = %q, want the rotated token", token.RefreshToken)
	}
}

func TestProviderHeadersAreSentOnAuthorizationPollingAndRefresh(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("X-Provider-Platform") != "supported-client" || r.Header.Get("X-Device-ID") != "device-123" {
			http.Error(w, "missing provider identity", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/device":
			_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"CODE","verification_uri":"https://example.test","interval":1,"expires_in":60}`))
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"fresh","refresh_token":"refresh","expires_in":3600}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := testConfig(server)
	cfg.Headers = map[string]string{"X-Provider-Platform": "supported-client", "X-Device-ID": "device-123"}
	code, err := RequestCode(context.Background(), server.Client(), cfg, reference)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Poll(context.Background(), server.Client(), cfg, code, noSleep, func() time.Time { return reference }); err != nil {
		t.Fatal(err)
	}
	if _, err := Refresh(context.Background(), server.Client(), cfg, "refresh", reference); err != nil {
		t.Fatal(err)
	}
	if requests != 3 {
		t.Fatalf("provider identity was exercised on %d requests, want all three flow stages", requests)
	}
}

func TestRefreshSurfacesInvalidGrant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"expired or revoked"}`))
	}))
	defer server.Close()

	_, err := Refresh(context.Background(), server.Client(), testConfig(server), "dead", reference)
	if err == nil {
		t.Fatal("a revoked refresh token must be an error")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("error %q must carry invalid_grant so the proxy marks the account for re-auth", err)
	}
}

func TestRefreshRejectsAnEmptyToken(t *testing.T) {
	if _, err := Refresh(context.Background(), nil, Config{}, "  ", reference); err == nil {
		t.Fatal("an empty refresh token must be rejected before any request")
	}
}

func noSleep(context.Context, time.Duration) error { return nil }

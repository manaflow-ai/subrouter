package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Persist mode with the failover off only makes the same-account gaps
// faster: it never shortens the wait the operator configured, and keeps an
// operator's unbounded wait.
func TestCodexPersistPresetNeverShortensOperatorWait(t *testing.T) {
	persist := codexCapacityRetryPolicy{persist: true, persistBudget: 2 * time.Minute}
	cases := []struct {
		name          string
		config        *CodexOverloadFailoverConfig
		wantMaxWait   time.Duration
		wantUnbounded bool
	}{
		{"unconfigured: the 4m default beats the 2m budget", nil, codexCapacityDefaultStayMaxWait, false},
		{"operator default", &CodexOverloadFailoverConfig{}, codexCapacityDefaultStayMaxWait, false},
		{"shorter operator wait: the budget wins", &CodexOverloadFailoverConfig{StayMaxWait: time.Minute}, 2 * time.Minute, false},
		{"longer operator wait wins", &CodexOverloadFailoverConfig{StayMaxWait: 10 * time.Minute}, 10 * time.Minute, false},
		{"operator unbounded stays unbounded", &CodexOverloadFailoverConfig{StayUnbounded: true}, 0, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			stay, explicit := test.config.stayPolicy(persist)
			if stay.interval != codexCapacityPersistStayInterval || !explicit || stay.unbounded != test.wantUnbounded {
				t.Fatalf("stay = %+v explicit=%t, want 1s gaps, explicit, unbounded=%t", stay, explicit, test.wantUnbounded)
			}
			if !test.wantUnbounded && stay.maxWait != test.wantMaxWait {
				t.Fatalf("stay max wait = %v, want %v", stay.maxWait, test.wantMaxWait)
			}
		})
	}
	// A longer persist budget still wins over the operator default.
	long := codexCapacityRetryPolicy{persist: true, persistBudget: 8 * time.Minute}
	if stay, _ := (&CodexOverloadFailoverConfig{}).stayPolicy(long); stay.maxWait != 8*time.Minute {
		t.Fatalf("stay max wait = %v, want the 8m persist budget", stay.maxWait)
	}
}

// The Fable API-key stage is called with the client's unstripped request on
// the handler-level fallback; no Subrouter control or forwarding header may
// reach api.anthropic.com.
func TestClaudeFableAPIKeyStageStripsSubrouterHeaders(t *testing.T) {
	var outbound http.Header
	server := Server{
		ClaudeFableAPIKey: "fable-key",
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			outbound = req.Header.Clone()
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}),
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	for _, key := range []string{
		"X-Subrouter-Retry", "X-Subrouter-Session", "X-Subrouter-User-Email", "X-Subrouter-Lease",
		"X-Subrouter-Agent", "X-Subrouter-Account-ID", "X-Subrouter-Preferred-Account-ID",
		"X-Subrouter-Capacity-Retry", "X-Subrouter-Something-New",
		"X-Forwarded-For", "Forwarded", "X-Real-IP",
	} {
		request.Header.Set(key, "client-value")
	}
	request.Header.Set("Anthropic-Version", "2023-06-01")
	request.Header.Set("Authorization", "Bearer oauth")
	response, err := server.claudeFableAPIKeyResponse(request, bytes.NewBufferString(`{"model":"claude-fable"}`).Bytes())
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	for key := range outbound {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "x-subrouter-") || strings.HasPrefix(lower, "x-forwarded-") || lower == "forwarded" || lower == "x-real-ip" {
			t.Fatalf("API-key stage sent %s upstream (all: %v)", key, outbound)
		}
	}
	if outbound.Get("X-Api-Key") != "fable-key" || outbound.Get("Authorization") != "" || outbound.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatalf("outbound headers = %v, want the API key, no OAuth, and the client's Anthropic-Version", outbound)
	}
}

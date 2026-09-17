package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestForcedCodexProviderIsolation(t *testing.T) {
	for _, provider := range []string{"Azure", "OpenAI"} {
		for _, fail := range []bool{false, true} {
			t.Run(provider+map[bool]string{false: "Success", true: "Failure"}[fail], func(t *testing.T) {
				var poolCalls, azureCalls, openaiCalls atomic.Int32
				pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { poolCalls.Add(1); io.WriteString(w, `{"id":"pool"}`) }))
				defer pool.Close()
				poolURL, _ := url.Parse(pool.URL)
				endpoint := func(name string, calls *atomic.Int32) *url.URL {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						if r.Header.Get("Authorization") != "Bearer test-key" {
							t.Error("missing API key")
						}
						if fail {
							w.WriteHeader(400)
							return
						}
						io.WriteString(w, `{"id":"`+name+`"}`)
					}))
					t.Cleanup(server.Close)
					path := "/openai/v1"
					if name == "OpenAI" {
						path = "/v1"
					}
					u, _ := url.Parse(server.URL + path)
					return u
				}
				azureURL := endpoint("Azure", &azureCalls)
				openaiURL := endpoint("OpenAI", &openaiCalls)
				s := azureCodexFallbackServer(t, azureURL, poolURL, 1)
				s.AzureCodex.Endpoints = []AzureCodexEndpoint{{Name: "azure", BaseURL: azureURL, APIKey: "test-key"}, {Name: "openai", BaseURL: openaiURL, APIKey: "test-key"}}
				server := httptest.NewServer(s.Handler())
				defer server.Close()
				req, _ := http.NewRequest("POST", server.URL+"/responses", strings.NewReader(`{"model":"gpt-5.6-codex"}`))
				req.Header.Set("X-Subrouter-"+provider, "force")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if fail && resp.StatusCode != 502 {
					t.Fatalf("status=%d body=%s", resp.StatusCode, body)
				}
				if !fail && !strings.Contains(string(body), provider) {
					t.Fatalf("body=%s", body)
				}
				if poolCalls.Load() != 0 {
					t.Fatalf("called subscription pool %d times", poolCalls.Load())
				}
				selected, other := azureCalls.Load(), openaiCalls.Load()
				if provider == "OpenAI" {
					selected, other = other, selected
				}
				if selected != 1 || other != 0 {
					t.Fatalf("selected calls=%d other calls=%d", selected, other)
				}
			})
		}
	}
}

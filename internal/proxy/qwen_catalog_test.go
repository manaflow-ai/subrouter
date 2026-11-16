package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

func TestQwenTokenCatalogAdvertisesAcceptedSubscriptionAlias(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[{"id":"qwen4-max-preview","context_length":1000000,"capabilities":{"tools":true}},{"id":"future-model"}],"extra":"preserve"}`)
	}))
	defer upstream.Close()
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := Server{
		Accounts:          []accounts.Account{{ID: "qwen-token:plan", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey, Token: "key"}},
		QwenTokenUpstream: mustParseURL(t, upstream.URL+"/v1"), Sessions: sessions,
	}
	for _, path := range []string{"/qwen-token/v1/models", "/qwen/v1/models"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d: %s", path, rec.Code, rec.Body.String())
		}
		var catalog struct {
			Data []struct {
				ID           string          `json:"id"`
				Context      int             `json:"context_length"`
				Capabilities map[string]bool `json:"capabilities"`
			} `json:"data"`
			Extra string `json:"extra"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
			t.Fatal(err)
		}
		if len(catalog.Data) != 3 || catalog.Data[0].ID != "qwen4-max-preview" || catalog.Data[1].ID != "future-model" || catalog.Data[2].ID != "qwen4.0-max-preview" || catalog.Data[2].Context != 1000000 || !catalog.Data[2].Capabilities["tools"] || catalog.Extra != "preserve" {
			t.Fatalf("%s: alias or upstream metadata missing: %s", path, rec.Body.String())
		}
	}
}

func TestQwenTokenCatalogAliasPreservesOtherResponses(t *testing.T) {
	for _, tt := range []struct {
		name           string
		status         int
		encoding, body string
	}{
		{"already advertised", 200, "", `{"data":[{"id":"qwen4-max-preview"},{"id":"qwen4.0-max-preview","metadata":"official"}]}`},
		{"different model", 200, "", `{"data":[{"id":"future-model"}]}`},
		{"provider error", 503, "", `{"error":{"code":"unavailable"}}`},
		{"unexpected schema", 200, "", `{"data":["qwen4-max-preview"]}`},
		{"not JSON", 200, "", "upstream maintenance"},
		{"compressed", 200, "gzip", "encoded bytes"},
		{"oversized", 200, "", `{"data":[{"id":"qwen4-max-preview","metadata":"` + strings.Repeat("x", 1<<20) + `"}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tt.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tt.body)), ContentLength: int64(len(tt.body))}
			response.Header.Set("Content-Length", strconv.Itoa(len(tt.body)))
			response.Header.Set("ETag", "original")
			response.Header.Set("Content-Encoding", tt.encoding)
			if err := addQwenTokenCatalogAlias(response); err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || string(body) != tt.body || response.ContentLength != int64(len(tt.body)) || response.Header.Get("ETag") != "original" {
				t.Fatalf("upstream response changed: bytes=%d, error=%v, length=%d, etag=%q", len(body), err, response.ContentLength, response.Header.Get("ETag"))
			}
		})
	}
}

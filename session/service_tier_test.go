package session

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestExtractServiceTierRestoresBody(t *testing.T) {
	cases := map[string]string{
		`{"model":"gpt-6-astra","service_tier":"priority","input":[]}`: "priority",
		`{"model":"gpt-6-astra","service_tier" : "Flex"}`:              "flex",
		`{"model":"gpt-6-astra","input":[]}`:                           "",
	}
	for body, want := range cases {
		request, _ := http.NewRequest(http.MethodPost, "http://x/responses", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if got := ExtractServiceTier(request, 1<<20); got != want {
			t.Errorf("ExtractServiceTier(%s) = %q, want %q", body, got, want)
		}
		rest, _ := io.ReadAll(request.Body)
		if string(rest) != body {
			t.Errorf("body after extraction = %q, want it restored", rest)
		}
	}
}

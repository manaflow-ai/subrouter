package proxy

import (
	"net/url"
	"strings"
	"testing"
)

func TestTranscriptURLDropsQueryCredentialsWithoutMutatingUpstream(t *testing.T) {
	upstream, err := url.Parse("https://gateway.example/v1?api_key=SECRET&session=opaque#fragment-secret")
	if err != nil {
		t.Fatal(err)
	}
	got := redactedTranscriptURL(upstream)
	if got != "https://gateway.example/v1" {
		t.Fatalf("transcript URL = %q", got)
	}
	if strings.Contains(got, "SECRET") || strings.Contains(got, "opaque") || strings.Contains(got, "fragment-secret") {
		t.Fatalf("transcript URL retained sensitive URL data: %q", got)
	}
	if upstream.RawQuery != "api_key=SECRET&session=opaque" || upstream.Fragment != "fragment-secret" {
		t.Fatalf("forwarded upstream was mutated: %s", upstream)
	}
}

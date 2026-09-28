package session

import (
	"bytes"
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

// ExtractBodySize measures the decoded body, so a zstd-compressed long
// conversation reads as long rather than as its small wire size.
func TestExtractBodySizeReportsDecodedLength(t *testing.T) {
	body := `{"model":"gpt-6-astra","input":[{"type":"message","content":"` + strings.Repeat("hello world ", 20000) + `"}]}`
	plain, _ := http.NewRequest(http.MethodPost, "http://x/responses", strings.NewReader(body))
	plain.Header.Set("Content-Type", "application/json")
	if got := ExtractBodySize(plain, 1<<20); got != int64(len(body)) {
		t.Fatalf("plain ExtractBodySize = %d, want %d", got, len(body))
	}

	wire := zstdBytes(t, body)
	if len(wire) >= len(body)/10 {
		t.Fatalf("test body compressed to %d of %d bytes; want a much smaller wire", len(wire), len(body))
	}
	compressed, _ := http.NewRequest(http.MethodPost, "http://x/responses", bytes.NewReader(wire))
	compressed.Header.Set("Content-Type", "application/json")
	compressed.Header.Set("Content-Encoding", "zstd")
	if got := ExtractBodySize(compressed, 1<<20); got != int64(len(body)) {
		t.Fatalf("zstd ExtractBodySize = %d, want the decoded %d", got, len(body))
	}
	rest, _ := io.ReadAll(compressed.Body)
	if !bytes.Equal(rest, wire) {
		t.Fatal("compressed body was not restored for upstream")
	}

	// A body that does not decode still reports its wire length as a floor.
	for _, encoding := range []string{"zstd", "br"} {
		garbage := bytes.Repeat([]byte{0xde, 0xad}, 4096)
		undecodable, _ := http.NewRequest(http.MethodPost, "http://x/responses", bytes.NewReader(garbage))
		undecodable.Header.Set("Content-Type", "application/json")
		undecodable.Header.Set("Content-Encoding", encoding)
		if got := ExtractBodySize(undecodable, 1<<20); got != int64(len(garbage)) {
			t.Fatalf("undecodable %s ExtractBodySize = %d, want the wire length %d", encoding, got, len(garbage))
		}
	}

	notJSON, _ := http.NewRequest(http.MethodPost, "http://x/responses", strings.NewReader("hi"))
	notJSON.Header.Set("Content-Type", "text/plain")
	if got := ExtractBodySize(notJSON, 1<<20); got != 0 {
		t.Fatalf("non-JSON ExtractBodySize = %d, want 0 (unknown)", got)
	}
}

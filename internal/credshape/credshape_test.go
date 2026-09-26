package credshape

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClassifyTrailingBytes(t *testing.T) {
	cases := []struct {
		name     string
		trailing []byte
		want     string
	}{
		{name: "empty", trailing: nil, want: "empty"},
		{name: "binary plist", trailing: []byte("bplist00\x00\x08"), want: "binary-plist"},
		{name: "nul padding", trailing: []byte{0, 0, 0, 0}, want: "nul-padding"},
		{name: "whitespace", trailing: []byte("\n\t  "), want: "whitespace"},
		{name: "json fragment", trailing: []byte(`Token":"abc"}}`), want: "json-fragment"},
		{name: "text", trailing: []byte("truncated write"), want: "text"},
		{name: "binary", trailing: []byte{0xff, 0xfe, 0x01}, want: "binary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyTrailingBytes(tc.trailing); got != tc.want {
				t.Fatalf("ClassifyTrailingBytes(%q) = %q, want %q", tc.trailing, got, tc.want)
			}
		})
	}
}

// The describer must never echo the payload: it holds an access token, and
// trailing bytes can be the tail of a previous one.
func TestDescribeNeverEchoesThePayload(t *testing.T) {
	valid := `{"claudeAiOauth":{"accessToken":"sk-ant-oat-secret"}}`
	for _, body := range [][]byte{
		[]byte(valid + `xToken":"sk-ant-ort-leftover"}}`),
		[]byte(valid + "bplist00"),
		[]byte(valid + "\x00\x00"),
		append([]byte(valid), 0xff, 0xfe),
	} {
		var raw map[string]any
		err := json.Unmarshal(body, &raw)
		if err == nil {
			t.Fatalf("payload should not decode: %q", body[:20])
		}
		summary := Describe(body, err)
		for _, secret := range []string{"sk-ant-oat-secret", "leftover"} {
			if strings.Contains(summary, secret) {
				t.Fatalf("summary leaked %q: %s", secret, summary)
			}
		}
		if !strings.Contains(summary, "bytes=") {
			t.Fatalf("summary should report a size, got %q", summary)
		}
	}
}

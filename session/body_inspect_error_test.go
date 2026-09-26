package session

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// failingAfterReader yields its data, then one non-EOF error, then EOF on
// every later read, like a connection that resets and is then closed.
type failingAfterReader struct {
	data   string
	failed bool
}

var errBodyReset = errors.New("connection reset")

func (r *failingAfterReader) Read(p []byte) (int, error) {
	if r.data != "" {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	if !r.failed {
		r.failed = true
		return 0, errBodyReset
	}
	return 0, io.EOF
}

func (r *failingAfterReader) Close() error { return nil }

// TestInspectedBodyReplaysPrefetchError: a body whose read fails while the
// extractors prefetch it must not be forwarded as a complete, shorter body.
// The consumer gets the buffered bytes and then the same error.
func TestInspectedBodyReplaysPrefetchError(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://example.test/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Body = &failingAfterReader{data: `{"model":"gpt-x","input":"trunc`}
	_ = ExtractModel(req, 1<<20)
	got, readErr := io.ReadAll(req.Body)
	if !errors.Is(readErr, errBodyReset) {
		t.Fatalf("read after failed prefetch: err = %v, want %v (body %q)", readErr, errBodyReset, got)
	}
	if !strings.HasPrefix(`{"model":"gpt-x","input":"trunc`, string(got)) {
		t.Fatalf("replayed bytes = %q", got)
	}
}

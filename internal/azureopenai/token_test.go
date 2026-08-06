package azureopenai

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

type recordingCommandRunner struct {
	mu      sync.Mutex
	outputs [][]byte
	calls   [][]string
}

func (r *recordingCommandRunner) Output(_ context.Context, name string, args []string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{name}, args...))
	if len(r.outputs) == 0 {
		return nil, fmt.Errorf("unexpected command")
	}
	output := r.outputs[0]
	r.outputs = r.outputs[1:]
	return output, nil
}

func TestCachedCLITokenSourceUsesAzureCLIAndRefreshesNearExpiry(t *testing.T) {
	now := time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)
	runner := &recordingCommandRunner{outputs: [][]byte{
		[]byte(`{"accessToken":"first-secret","expires_on":1786010400}`),
		[]byte(`{"accessToken":"second-secret","expires_on":1786014000}`),
	}}
	source := &CachedCLITokenSource{
		Profile: Profile{
			AzureCLI:      "/opt/homebrew/bin/az",
			TokenResource: FoundryTokenResource,
		},
		Runner: runner,
		Now:    func() time.Time { return now },
	}

	first, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != "first-secret" || second != "first-secret" {
		t.Fatalf("cached tokens = %q, %q", first, second)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("Azure CLI calls = %d, want 1", len(runner.calls))
	}
	wantCommand := []string{
		"/opt/homebrew/bin/az",
		"account", "get-access-token",
		"--resource", FoundryTokenResource,
		"--output", "json",
	}
	if !reflect.DeepEqual(runner.calls[0], wantCommand) {
		t.Fatalf("command = %#v, want %#v", runner.calls[0], wantCommand)
	}

	now = time.Unix(1786010400, 0).Add(-4 * time.Minute)
	refreshed, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if refreshed != "second-secret" {
		t.Fatalf("refreshed token = %q", refreshed)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("Azure CLI calls after expiry window = %d, want 2", len(runner.calls))
	}
}

func TestParseAccessTokenAcceptsCurrentAzureCLIExpiryFormats(t *testing.T) {
	for _, body := range []string{
		`{"accessToken":"secret","expires_on":"1786010400"}`,
		`{"accessToken":"secret","expiresOn":"2026-08-06 10:00:00.000000"}`,
	} {
		token, err := parseAccessToken([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if token.Value != "secret" || token.ExpiresAt.IsZero() {
			t.Fatalf("parsed token = %#v", token)
		}
	}
}

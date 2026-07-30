package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestStreamCancelAttribution(t *testing.T) {
	canceledClient, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name       string
		clientCtx  context.Context
		err        error
		want       string
		wantClient bool
	}{
		{
			// The upstream died on its own; the client is still waiting.
			name:      "upstream failure",
			clientCtx: context.Background(),
			err:       io.ErrUnexpectedEOF,
			want:      "upstream",
		},
		{
			// The downstream client hung up, which cancels the outbound
			// request. Expected, not a proxy defect.
			name:       "client disconnected",
			clientCtx:  canceledClient,
			err:        context.Canceled,
			want:       "client",
			wantClient: true,
		},
		{
			// The upstream read was canceled while the client was still
			// connected. This is the case worth alerting on.
			name:      "proxy dropped a live stream",
			clientCtx: context.Background(),
			err:       context.Canceled,
			want:      "proxy",
		},
		{
			name:      "wrapped cancellation is still attributed",
			clientCtx: context.Background(),
			err:       fmt.Errorf("reading body: %w", context.Canceled),
			want:      "proxy",
		},
		{
			name:      "missing client context",
			clientCtx: nil,
			err:       context.Canceled,
			want:      "unknown",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, clientErr := streamCancelAttribution(test.clientCtx, test.err)
			if got != test.want {
				t.Fatalf("canceled_by = %q, want %q", got, test.want)
			}
			if test.wantClient && !errors.Is(clientErr, context.Canceled) {
				t.Fatalf("client_ctx_err = %v, want context.Canceled", clientErr)
			}
			if !test.wantClient && clientErr != nil {
				t.Fatalf("client_ctx_err = %v, want nil", clientErr)
			}
		})
	}
}

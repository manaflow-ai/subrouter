package proxy

import (
	"testing"
	"time"
)

func TestCodexUsageLimitExpiryUsesProviderResetOrWorkspaceReason(t *testing.T) {
	now := time.Date(2026, 9, 19, 2, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		body string
		want time.Time
		ok   bool
	}{
		{
			name: "weekly window with resets_in_seconds",
			body: `{"error":{"type":"usage_limit_reached","message":"You've hit your usage limit.","plan_type":"team","resets_in_seconds":172800}}`,
			want: now.Add(48 * time.Hour), ok: true,
		},
		{
			name: "member credits depleted, no reset",
			body: `{"type":"error","error":{"code":"usage_limit_reached","message":"Your workspace is out of credits. Ask your workspace owner to refill in order to continue.","rate_limit_reached_type":"workspace_member_credits_depleted"}}`,
			want: now.Add(codexWorkspaceExhaustionTTL), ok: true,
		},
		{
			name: "owner spend cap under response.error",
			body: `{"type":"response.failed","response":{"error":{"code":"usage_limit_reached","message":"You hit your spend cap set in your workspace.","rate_limit_reached_type":"workspace_owner_usage_limit_reached"}}}`,
			want: now.Add(codexWorkspaceExhaustionTTL), ok: true,
		},
		{
			name: "resets_at epoch beyond clamp",
			body: `{"error":{"type":"usage_limit_reached","resets_at":` + itoa(now.Add(30*24*time.Hour).Unix()) + `}}`,
			want: now.Add(8 * 24 * time.Hour), ok: true,
		},
		{
			name: "personal rate limit with nothing else keeps default",
			body: `{"error":{"type":"usage_limit_reached","message":"You've hit your usage limit.","rate_limit_reached_type":"rate_limit_reached"}}`,
			ok:   false,
		},
		{
			name: "not a usage limit",
			body: `{"error":{"code":"server_is_overloaded","message":"busy"}}`,
			ok:   false,
		},
		{
			name: "past reset clamps to one minute",
			body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":5}}`,
			want: now.Add(time.Minute), ok: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := codexUsageLimitExpiry([]byte(test.body), now)
			if ok != test.ok {
				t.Fatalf("ok = %v, want %v (got %v)", ok, test.ok, got)
			}
			if ok && !got.Equal(test.want) {
				t.Fatalf("until = %v, want %v", got, test.want)
			}
		})
	}
}

func itoa(v int64) string {
	return formatInt(v)
}

func formatInt(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

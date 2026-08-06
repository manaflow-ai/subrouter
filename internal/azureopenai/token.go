package azureopenai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type CommandRunner interface {
	Output(context.Context, string, []string) ([]byte, error)
}

type ExecCommandRunner struct{}

func (ExecCommandRunner) Output(ctx context.Context, name string, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

type AccessToken struct {
	Value     string
	ExpiresAt time.Time
}

func FetchCLIAccessToken(ctx context.Context, runner CommandRunner, profile Profile) (AccessToken, error) {
	if runner == nil {
		runner = ExecCommandRunner{}
	}
	output, err := runner.Output(ctx, profile.AzureCLI, []string{
		"account", "get-access-token",
		"--resource", profile.TokenResource,
		"--output", "json",
	})
	if err != nil {
		return AccessToken{}, fmt.Errorf("Azure CLI authentication failed; run 'az login' and confirm Azure OpenAI access: %w", err)
	}
	token, err := parseAccessToken(output)
	if err != nil {
		return AccessToken{}, fmt.Errorf("parse Azure CLI access token response: %w", err)
	}
	return token, nil
}

func parseAccessToken(body []byte) (AccessToken, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return AccessToken{}, err
	}
	var value string
	if err := json.Unmarshal(fields["accessToken"], &value); err != nil || strings.TrimSpace(value) == "" {
		return AccessToken{}, errors.New("Azure CLI returned no access token")
	}
	expiresAt := parseExpiry(fields["expires_on"])
	if expiresAt.IsZero() {
		expiresAt = parseExpiry(fields["expiresOn"])
	}
	if expiresAt.IsZero() {
		expiresAt = jwtExpiry(value)
	}
	if expiresAt.IsZero() {
		// Azure CLI normally returns an expiry. A short cache still avoids one
		// process spawn per request on older or customized CLI output.
		expiresAt = time.Now().Add(10 * time.Minute)
	}
	return AccessToken{Value: strings.TrimSpace(value), ExpiresAt: expiresAt}, nil
}

func parseExpiry(raw json.RawMessage) time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}
	}
	var number json.Number
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err == nil {
		if seconds, err := strconv.ParseInt(number.String(), 10, 64); err == nil && seconds > 0 {
			return time.Unix(seconds, 0).UTC()
		}
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return time.Time{}
	}
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Unix(seconds, 0).UTC()
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Expires json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if err := decoder.Decode(&claims); err != nil {
		return time.Time{}
	}
	seconds, err := strconv.ParseInt(claims.Expires.String(), 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

type TokenSource interface {
	Token(context.Context) (string, error)
}

type CachedCLITokenSource struct {
	Profile       Profile
	Runner        CommandRunner
	Now           func() time.Time
	RefreshBefore time.Duration

	mu    sync.Mutex
	token AccessToken
}

func (s *CachedCLITokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	refreshBefore := s.RefreshBefore
	if refreshBefore <= 0 {
		refreshBefore = 5 * time.Minute
	}
	if s.token.Value != "" && now.Add(refreshBefore).Before(s.token.ExpiresAt) {
		return s.token.Value, nil
	}
	token, err := FetchCLIAccessToken(ctx, s.Runner, s.Profile)
	if err != nil {
		return "", err
	}
	s.token = token
	return token.Value, nil
}

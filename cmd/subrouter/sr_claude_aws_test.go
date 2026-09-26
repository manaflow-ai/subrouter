package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvWithoutStripsRoutingVars(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_BASE_URL=http://subrouter-team:31415",
		"ANTHROPIC_AUTH_TOKEN=secret",
		"ANTHROPIC_API_KEY=sk-ant-xyz",
		"CLAUDE_CODE_USE_BEDROCK=1",
		"ANTHROPIC_MODEL=claude-fable-5",
		"HOME=/home/x",
	}
	got := envWithout(environ, claudeRoutingEnvKeys)
	joined := strings.Join(got, "\n")
	for _, banned := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("%s should have been stripped: %v", banned, got)
		}
	}
	for _, kept := range []string{"PATH=/usr/bin", "HOME=/home/x", "ANTHROPIC_MODEL=claude-fable-5"} {
		if !strings.Contains(joined, kept) {
			t.Fatalf("%s should have been kept: %v", kept, got)
		}
	}
}

func TestClaudeAWSChildEnvironmentScrubsSubrouterControlSecrets(t *testing.T) {
	got := claudeAWSChildEnvironment([]string{
		"PATH=/usr/bin",
		"SUBROUTER_ADMIN_TOKEN=durable-admin-secret",
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE=/private/import-token",
		"SUBROUTER_FUTURE_KEY=future-secret",
		"SUBROUTER_CLOUD_CONFIG=/private/cloud-config",
		"SUBROUTER_STATE_DIR=/private/state",
		"ANTHROPIC_AUTH_TOKEN=stale-anthropic-token",
		"ANTHROPIC_API_KEY=stale-anthropic-key",
		"AWS_ACCESS_KEY_ID=stale-access-key",
		"AWS_SECRET_ACCESS_KEY=stale-secret-key",
		"AWS_SESSION_TOKEN=stale-session-token",
		"AWS_WEB_IDENTITY_TOKEN_FILE=/private/aws-web-identity",
		"AWS_PROFILE=stale-profile",
	}, "http://127.0.0.1:31415/bedrock", "us-east-1", "fable", "gateway-capability")
	joined := strings.Join(got, "\n")
	for _, forbidden := range []string{"durable-admin-secret", "/private/import-token", "future-secret", "/private/cloud-config", "/private/state", "stale-anthropic-token", "stale-anthropic-key", "stale-access-key", "stale-secret-key", "stale-session-token", "/private/aws-web-identity", "stale-profile"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("Claude AWS child retained Subrouter control secret %q:\n%s", forbidden, joined)
		}
	}
	for _, want := range []string{
		"PATH=/usr/bin",
		"ANTHROPIC_BEDROCK_BASE_URL=http://127.0.0.1:31415/bedrock",
		"ANTHROPIC_AUTH_TOKEN=gateway-capability",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Claude AWS child environment missing %q:\n%s", want, joined)
		}
	}
	withoutGateway := claudeAWSChildEnvironment([]string{
		"ANTHROPIC_AUTH_TOKEN=must-not-survive",
		"AWS_ACCESS_KEY_ID=must-not-survive",
	}, "http://127.0.0.1:31415/bedrock", "us-east-1", "fable", "")
	if joined := strings.Join(withoutGateway, "\n"); strings.Contains(joined, "ANTHROPIC_AUTH_TOKEN=") || strings.Contains(joined, "AWS_ACCESS_KEY_ID=") {
		t.Fatalf("Claude AWS child retained ambient credentials without a gateway capability:\n%s", joined)
	}
}

func TestBedrockModelID(t *testing.T) {
	cases := map[string]string{
		"":                                 "us.anthropic.claude-fable-5",
		"fable":                            "us.anthropic.claude-fable-5",
		"claude-fable-5":                   "us.anthropic.claude-fable-5",
		"opus":                             "us.anthropic.claude-opus-4-8",
		"sonnet":                           "us.anthropic.claude-sonnet-5",
		"haiku":                            bedrockSmallFastModelID,
		"us.anthropic.claude-fable-5":      "us.anthropic.claude-fable-5",
		"global.anthropic.claude-opus-4-8": "global.anthropic.claude-opus-4-8",
		"some-unknown-id":                  "some-unknown-id",
	}
	for in, want := range cases {
		if got := bedrockModelID(in); got != want {
			t.Errorf("bedrockModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBedrockAWSProfileNames(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	credentials := filepath.Join(dir, "credentials")
	if err := os.WriteFile(config, []byte("[profile aw10]\nregion=us-east-1\n[profile aw1]\nregion=us-east-1\n[profile root-login]\nregion=us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentials, []byte("[aw0]\naws_access_key_id=x\naws_secret_access_key=y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", config)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentials)
	t.Setenv("SUBROUTER_BEDROCK_PROFILES", "")

	if got := strings.Join(bedrockAWSProfileNames(""), ","); got != "aw0,aw1,aw10" {
		t.Fatalf("profiles = %q, want aw0,aw1,aw10", got)
	}
	if got := strings.Join(bedrockAWSProfileNames("aw2, aw1 aw2"), ","); got != "aw2,aw1" {
		t.Fatalf("explicit profiles = %q, want aw2,aw1", got)
	}
}

func TestParseBedrockRegions(t *testing.T) {
	if got := strings.Join(parseBedrockRegions(" us-east-1,us-west-2,, us-east-1,us-east-2 "), ","); got != "us-east-1,us-west-2,us-east-2" {
		t.Fatalf("regions = %q, want us-east-1,us-west-2,us-east-2", got)
	}
	if got := parseBedrockRegions(" , "); len(got) != 0 {
		t.Fatalf("empty regions = %v, want none", got)
	}
}

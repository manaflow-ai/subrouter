package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdUnitUsesServerDefaults(t *testing.T) {
	config := systemdConfig{
		ServiceName:      defaultSystemdServiceName,
		User:             "subrouter",
		Group:            "subrouter",
		Home:             "/var/lib/subrouter",
		Addr:             "0.0.0.0:31415",
		InstallPath:      "/usr/local/bin/subrouter",
		SessionsPath:     "/var/lib/subrouter/sessions.json",
		SRSwitchInterval: "10m",
	}
	unit, err := systemdUnit(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Requires=subrouter.socket",
		"After=network-online.target subrouter.socket",
		"Sockets=subrouter.socket",
		"User=subrouter",
		"Group=subrouter",
		"WorkingDirectory=/var/lib/subrouter",
		"Environment=HOME=/var/lib/subrouter",
		"EnvironmentFile=-/etc/default/subrouter",
		"ExecStart=/usr/local/bin/subrouter serve --addr ${SUBROUTER_ADDR}",
		"$SUBROUTER_TRANSCRIPT_ARGS",
		"--sr-switch-interval ${SUBROUTER_SR_SWITCH_INTERVAL}",
		"TimeoutStopSec=10min",
		"StartLimitIntervalSec=60",
		"StartLimitBurst=5",
		"ReadWritePaths=/var/lib/subrouter /var/log/subrouter",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
}

func TestSystemdSocketUsesConfiguredAddress(t *testing.T) {
	config := systemdConfig{
		ServiceName: defaultSystemdServiceName,
		Addr:        "0.0.0.0:31415",
	}
	socketUnit, err := systemdSocket(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ListenStream=0.0.0.0:31415",
		"NoDelay=true",
		"Service=subrouter.service",
		"WantedBy=sockets.target",
	} {
		if !strings.Contains(socketUnit, want) {
			t.Fatalf("socket unit missing %q:\n%s", want, socketUnit)
		}
	}
}

func TestSystemdDefaultsEscapesExtraArgs(t *testing.T) {
	config := systemdConfig{
		Addr:                "0.0.0.0:31415",
		Home:                "/var/lib/subrouter",
		SessionsPath:        "/var/lib/subrouter/sessions.json",
		TranscriptsDir:      "/var/lib/subrouter/transcripts",
		SRSwitchInterval:    "10m",
		AdminToken:          "secret-token",
		GeminiAPIKey:        "provider-key",
		GeminiGatewayKey:    "team-key",
		AnthropicAPIKey:     "anthropic-provider-key",
		AnthropicGatewayKey: "anthropic-team-key",
		OpenAIAPIKey:        "openai-provider-key",
		OpenAIGatewayKey:    "openai-team-key",
		ExtraArgs:           "--transcript-gcs-uri=gs://bucket/prefix --fetch-usage=false",
	}
	defaults := systemdDefaults(config)
	if !strings.Contains(defaults, "SUBROUTER_STATE_DIR=/var/lib/subrouter") {
		t.Fatalf("defaults missing state dir:\n%s", defaults)
	}
	if !strings.Contains(defaults, "SUBROUTER_SR_SWITCH_INTERVAL=10m") {
		t.Fatalf("defaults missing sr switch interval:\n%s", defaults)
	}
	if !strings.Contains(defaults, `SUBROUTER_TRANSCRIPT_ARGS="--transcripts=/var/lib/subrouter/transcripts"`) {
		t.Fatalf("defaults missing transcript args:\n%s", defaults)
	}
	if !strings.Contains(defaults, `SUBROUTER_EXTRA_ARGS="--transcript-gcs-uri=gs://bucket/prefix --fetch-usage=false"`) {
		t.Fatalf("defaults did not quote extra args:\n%s", defaults)
	}
	if !strings.Contains(defaults, `SUBROUTER_ADMIN_TOKEN="secret-token"`) {
		t.Fatalf("defaults did not quote admin token:\n%s", defaults)
	}
	if !strings.Contains(defaults, `SUBROUTER_GEMINI_API_KEY="provider-key"`) {
		t.Fatalf("defaults did not quote Gemini API key:\n%s", defaults)
	}
	if !strings.Contains(defaults, `SUBROUTER_GEMINI_GATEWAY_TOKEN="team-key"`) {
		t.Fatalf("defaults did not quote Gemini gateway token:\n%s", defaults)
	}
	for _, want := range []string{
		`SUBROUTER_ANTHROPIC_API_KEY="anthropic-provider-key"`,
		`SUBROUTER_ANTHROPIC_GATEWAY_TOKEN="anthropic-team-key"`,
		`SUBROUTER_OPENAI_API_KEY="openai-provider-key"`,
		`SUBROUTER_OPENAI_GATEWAY_TOKEN="openai-team-key"`,
	} {
		if !strings.Contains(defaults, want) {
			t.Fatalf("defaults missing %q:\n%s", want, defaults)
		}
	}
}

func TestSystemdDefaultsDisableTranscriptsByDefault(t *testing.T) {
	config := systemdConfig{
		Addr:             "0.0.0.0:31415",
		Home:             "/var/lib/subrouter",
		SessionsPath:     "/var/lib/subrouter/sessions.json",
		SRSwitchInterval: "10m",
	}
	defaults := systemdDefaults(config)
	if !strings.Contains(defaults, "SUBROUTER_TRANSCRIPTS=\n") {
		t.Fatalf("defaults should leave transcript dir empty:\n%s", defaults)
	}
	if !strings.Contains(defaults, `SUBROUTER_TRANSCRIPT_ARGS=""`) {
		t.Fatalf("defaults should leave transcript args empty:\n%s", defaults)
	}
}

func TestWriteSystemdDefaultsTightensExistingFileModeForSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subrouter")
	oldContents := []byte("SUBROUTER_ADDR=0.0.0.0:31415\n")
	newContents := []byte("SUBROUTER_GEMINI_API_KEY=secret\n")
	if err := os.WriteFile(path, oldContents, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	oldFile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer oldFile.Close()
	if err := writeSystemdDefaults(path, newContents, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("defaults mode = %04o, want 0600", got)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newContents) {
		t.Fatalf("defaults contents = %q, want %q", got, newContents)
	}
	oldView, err := io.ReadAll(oldFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(oldView) != string(oldContents) {
		t.Fatalf("pre-opened file observed replacement contents: %q", oldView)
	}
}

func TestApplyExistingSystemdDefaultsPreservesTranscriptsDir(t *testing.T) {
	// Reproduces the crash-loop: a re-install with no --transcripts flag must
	// not drop SUBROUTER_TRANSCRIPTS while keeping --transcript-gcs-uri in
	// SUBROUTER_EXTRA_ARGS, since serve rejects that combination at startup.
	path := filepath.Join(t.TempDir(), "subrouter")
	existing := strings.Join([]string{
		"SUBROUTER_ADDR=0.0.0.0:31415",
		"SUBROUTER_TRANSCRIPTS=/var/lib/subrouter/transcripts",
		`SUBROUTER_TRANSCRIPT_ARGS="--transcripts=/var/lib/subrouter/transcripts"`,
		"SUBROUTER_ADMIN_TOKEN=\"secret-token\"",
		"SUBROUTER_GEMINI_API_KEY=\"provider-key\"",
		"SUBROUTER_GEMINI_GATEWAY_TOKEN=\"team-key\"",
		"SUBROUTER_ANTHROPIC_API_KEY=\"anthropic-provider-key\"",
		"SUBROUTER_ANTHROPIC_GATEWAY_TOKEN=\"anthropic-team-key\"",
		"SUBROUTER_OPENAI_API_KEY=\"openai-provider-key\"",
		"SUBROUTER_OPENAI_GATEWAY_TOKEN=\"openai-team-key\"",
		`SUBROUTER_EXTRA_ARGS="--transcript-gcs-uri=gs://bucket/prefix --transcript-gcs-sync-interval=5m"`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	config := systemdConfig{
		Addr:             "0.0.0.0:31415",
		Home:             "/var/lib/subrouter",
		SessionsPath:     "/var/lib/subrouter/sessions.json",
		SRSwitchInterval: "10m",
	}
	applyExistingSystemdDefaults(&config, path)

	if config.TranscriptsDir != "/var/lib/subrouter/transcripts" {
		t.Fatalf("transcripts dir = %q, want preserved /var/lib/subrouter/transcripts", config.TranscriptsDir)
	}
	if config.AdminToken != "secret-token" {
		t.Fatalf("admin token = %q, want preserved secret-token", config.AdminToken)
	}
	if config.GeminiAPIKey != "provider-key" {
		t.Fatalf("Gemini API key = %q, want preserved provider-key", config.GeminiAPIKey)
	}
	if config.GeminiGatewayKey != "team-key" {
		t.Fatalf("Gemini gateway token = %q, want preserved team-key", config.GeminiGatewayKey)
	}
	if config.AnthropicAPIKey != "anthropic-provider-key" || config.AnthropicGatewayKey != "anthropic-team-key" {
		t.Fatalf("Anthropic credentials = %q / %q", config.AnthropicAPIKey, config.AnthropicGatewayKey)
	}
	if config.OpenAIAPIKey != "openai-provider-key" || config.OpenAIGatewayKey != "openai-team-key" {
		t.Fatalf("OpenAI credentials = %q / %q", config.OpenAIAPIKey, config.OpenAIGatewayKey)
	}
	if !strings.Contains(config.ExtraArgs, "--transcript-gcs-uri=gs://bucket/prefix") {
		t.Fatalf("extra args = %q, want preserved gcs uri", config.ExtraArgs)
	}

	defaults := systemdDefaults(config)
	if !strings.Contains(defaults, `SUBROUTER_TRANSCRIPT_ARGS="--transcripts=/var/lib/subrouter/transcripts"`) {
		t.Fatalf("regenerated defaults dropped transcripts, would crash-loop:\n%s", defaults)
	}
}

func TestReadDefaultValueUnquotesEnvFileValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subrouter")
	if err := os.WriteFile(path, []byte("SUBROUTER_EXTRA_ARGS=\"--transcript-gcs-uri=gs://bucket/prefix --fetch-usage=false\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readDefaultValue(path, "SUBROUTER_EXTRA_ARGS")
	want := "--transcript-gcs-uri=gs://bucket/prefix --fetch-usage=false"
	if got != want {
		t.Fatalf("extra args = %q, want %q", got, want)
	}
}

func TestReadDefaultValueDecodesQuotedSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subrouter")
	doubleQuoted := `team\token"quoted`
	singleQuoted := `team\single-token`
	literalEscape := `team\x41-token`
	contents := "DOUBLE=" + quoteSystemdDefaultValue(doubleQuoted) + "\n" +
		"SINGLE='" + singleQuoted + "'\n" +
		`LITERAL="team\x41-token"` + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readDefaultValue(path, "DOUBLE"); got != doubleQuoted {
		t.Fatalf("double-quoted value = %q, want %q", got, doubleQuoted)
	}
	if got := readDefaultValue(path, "SINGLE"); got != singleQuoted {
		t.Fatalf("single-quoted value = %q, want %q", got, singleQuoted)
	}
	if got := readDefaultValue(path, "LITERAL"); got != literalEscape {
		t.Fatalf("literal escape value = %q, want %q", got, literalEscape)
	}
}

func TestReadDefaultValueUsesLastAssignment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subrouter")
	contents := "SUBROUTER_OPENAI_GATEWAY_TOKEN=compromised\n" +
		"SUBROUTER_OPENAI_GATEWAY_TOKEN='rotated-token'\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readDefaultValue(path, "SUBROUTER_OPENAI_GATEWAY_TOKEN"); got != "rotated-token" {
		t.Fatalf("gateway token = %q, want rotated-token", got)
	}
}

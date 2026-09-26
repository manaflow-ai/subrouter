package main

import (
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

func TestSupervisorMigrationPreservesSystemdSandbox(t *testing.T) {
	scriptPath := filepath.Join("..", "..", "deploy", "gcp", "migrate-systemd-to-supervisor.sh")
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"NoNewPrivileges=true",
		"PrivateTmp=true",
		"ProtectSystem=full",
		"install -d -m 0750",
		"ProtectHome=read-only",
		"WorkingDirectory=${service_home}",
		"ReadWritePaths=${service_home} ${STATE_DIR} /var/log/subrouter",
	} {
		if !strings.Contains(string(script), want) {
			t.Fatalf("supervisor migration drops systemd sandbox directive %q", want)
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
		Addr:               "0.0.0.0:31415",
		Home:               "/var/lib/subrouter",
		SessionsPath:       "/var/lib/subrouter/sessions.json",
		TranscriptsDir:     "/var/lib/subrouter/transcripts",
		SRSwitchInterval:   "10m",
		AdminToken:         "secret-token",
		AccountImportToken: "import-secret",
		ExtraArgs:          "--transcript-gcs-uri=gs://bucket/prefix --fetch-usage=false",
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
	if !strings.Contains(defaults, `SUBROUTER_ACCOUNT_IMPORT_TOKEN="import-secret"`) {
		t.Fatalf("defaults did not quote account import token:\n%s", defaults)
	}
}

func TestSystemdDefaultsRoundTripQuotedEnvironmentValues(t *testing.T) {
	config := systemdConfig{
		Addr:               "0.0.0.0:31415",
		Home:               "/var/lib/subrouter",
		SessionsPath:       "/var/lib/subrouter/sessions.json",
		SRSwitchInterval:   "10m",
		AdminToken:         `admin\\token"quoted`,
		AccountImportToken: `import\\token"quoted`,
		ExtraArgs:          `--label="quoted value" --path=C:\\subrouter`,
	}
	path := filepath.Join(t.TempDir(), "subrouter")
	if err := os.WriteFile(path, []byte(systemdDefaults(config)), 0o600); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"SUBROUTER_ADMIN_TOKEN":          config.AdminToken,
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN": config.AccountImportToken,
		"SUBROUTER_EXTRA_ARGS":           config.ExtraArgs,
	} {
		if got := readDefaultValue(path, key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestSystemdDefaultsModeIsPrivateForEitherControlToken(t *testing.T) {
	for name, config := range map[string]systemdConfig{
		"admin":  {AdminToken: "admin-secret"},
		"import": {AccountImportToken: "import-secret"},
		"both":   {AdminToken: "admin-secret", AccountImportToken: "import-secret"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := systemdDefaultsMode(config); got != 0o600 {
				t.Fatalf("defaults mode = %o, want 600", got)
			}
		})
	}
	if got := systemdDefaultsMode(systemdConfig{}); got != 0o644 {
		t.Fatalf("token-free defaults mode = %o, want 644", got)
	}
}

func TestWriteSystemdDefaultsFileTightensExistingPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subrouter")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("SUBROUTER_ADMIN_TOKEN=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("defaults mode = %o, want 600", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "SUBROUTER_ADMIN_TOKEN=secret\n" {
		t.Fatalf("defaults body = %q", body)
	}
}

func TestReadSystemdAdminTokenFromStdin(t *testing.T) {
	token, err := readSystemdAdminToken(strings.NewReader("secret-token\n"))
	if err != nil {
		t.Fatal(err)
	}
	if token != "secret-token" {
		t.Fatalf("token = %q", token)
	}
	for _, value := range []string{"", "one\ntwo\n", strings.Repeat("x", 4097)} {
		if _, err := readSystemdAdminToken(strings.NewReader(value)); err == nil {
			t.Fatalf("accepted invalid admin token input of length %d", len(value))
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
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN=\"import-secret\"",
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
	if config.AccountImportToken != "import-secret" {
		t.Fatalf("account import token = %q, want preserved import-secret", config.AccountImportToken)
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

func TestUserSystemdRefusesExistingSystemService(t *testing.T) {
	dir := t.TempDir()
	unitPath := filepath.Join(dir, "subrouter.service")
	socketPath := filepath.Join(dir, "subrouter.socket")
	if err := refuseSystemSystemdConflict(unitPath, socketPath); err != nil {
		t.Fatalf("missing system service reported a conflict: %v", err)
	}
	if err := os.WriteFile(socketPath, []byte("[Socket]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := refuseSystemSystemdConflict(unitPath, socketPath)
	if err == nil ||
		!strings.Contains(err.Error(), "system-wide") ||
		!strings.Contains(err.Error(), "systemctl disable --now") {
		t.Fatalf("conflict error = %v", err)
	}
}

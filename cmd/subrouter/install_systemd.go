package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
	"time"

	"github.com/manaflow-ai/subrouter/internal/storepath"
)

const defaultSystemdServiceName = "subrouter"

type systemdConfig struct {
	ServiceName      string
	User             string
	Group            string
	Home             string
	Addr             string
	InstallPath      string
	SessionsPath     string
	TranscriptsDir   string
	SRSwitchInterval string
	AdminToken       string
	SentryDSN        string
	ExtraArgs        string
	Start            bool
	DryRun           bool
	InstallAliases   bool
	ReplaceLegacy    bool
}

func installSystemd(args []string) error {
	config := systemdConfig{}
	flags := flag.NewFlagSet("install-systemd", flag.ContinueOnError)
	flags.StringVar(&config.ServiceName, "service", defaultSystemdServiceName, "systemd service name")
	flags.StringVar(&config.User, "user", "subrouter", "system user")
	flags.StringVar(&config.Group, "group", "subrouter", "system group")
	flags.StringVar(&config.Home, "home", "/var/lib/subrouter", "service home directory")
	flags.StringVar(&config.Addr, "addr", "0.0.0.0:31415", "service listen address")
	flags.StringVar(&config.InstallPath, "install-path", "/usr/local/bin/subrouter", "subrouter binary install path")
	flags.StringVar(&config.SessionsPath, "sessions", "/var/lib/subrouter/sessions.json", "session assignment store")
	flags.StringVar(&config.TranscriptsDir, "transcripts", "", "transcript directory; empty preserves the existing SUBROUTER_TRANSCRIPTS, then disables transcript recording if unset")
	flags.StringVar(&config.SRSwitchInterval, "sr-switch-interval", "10m", "sr auto-switch interval; 0 disables")
	flags.StringVar(&config.SRSwitchInterval, "cx-switch-interval", "10m", "compatibility alias for --sr-switch-interval")
	flags.StringVar(&config.AdminToken, "admin-token", "", "admin token required for non-loopback _subrouter endpoints; preserves existing SUBROUTER_ADMIN_TOKEN by default")
	flags.StringVar(&config.SentryDSN, "sentry-dsn", "", "Sentry DSN for server error reporting; preserves existing SENTRY_DSN by default")
	flags.StringVar(&config.ExtraArgs, "extra-args", "", "extra arguments appended to subrouter serve")
	flags.BoolVar(&config.Start, "start", true, "enable and restart the systemd service")
	flags.BoolVar(&config.DryRun, "dry-run", false, "print the systemd unit without writing files")
	flags.BoolVar(&config.InstallAliases, "install-aliases", true, "install sr and cx symlinks to the subrouter binary")
	flags.BoolVar(&config.ReplaceLegacy, "replace-legacy", true, "stop legacy switchboard/gateway services and migrate their state")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if runtime.GOOS != "linux" && !config.DryRun {
		return errors.New("install-systemd is Linux-only")
	}
	applyExistingSystemdDefaults(&config, systemdDefaultPath(config))
	return installSystemdWithConfig(config, commandRunner{})
}

func installSystemdWithConfig(config systemdConfig, runner commandRunner) error {
	if err := validateSystemdConfig(config); err != nil {
		return err
	}
	unit, err := systemdUnit(config)
	if err != nil {
		return err
	}
	socketUnit, err := systemdSocket(config)
	if err != nil {
		return err
	}
	defaults := systemdDefaults(config)
	if config.DryRun {
		fmt.Print(unit)
		fmt.Print("\n")
		fmt.Print(socketUnit)
		return nil
	}
	if os.Geteuid() != 0 {
		return errors.New("install-systemd must run as root; use sudo")
	}

	if config.ReplaceLegacy {
		stopLegacySystemdServices(runner)
	}
	if err := ensureSystemUser(config, runner); err != nil {
		return err
	}
	for _, dir := range []string{
		config.Home,
		filepath.Join(config.Home, ".codex"),
		filepath.Dir(config.SessionsPath),
		"/var/log/subrouter",
	} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	if strings.TrimSpace(config.TranscriptsDir) != "" {
		if err := os.MkdirAll(config.TranscriptsDir, 0o750); err != nil {
			return err
		}
	}
	if config.ReplaceLegacy {
		migrateLegacySystemdState(config, runner)
	}
	if err := os.MkdirAll(filepath.Join(config.Home, "codex", "accounts"), 0o750); err != nil {
		return err
	}
	if err := installCurrentExecutable(config.InstallPath); err != nil {
		return err
	}
	if config.InstallAliases {
		for _, alias := range []string{"sr", "cx"} {
			if err := installBinaryAlias(config.InstallPath, filepath.Join(filepath.Dir(config.InstallPath), alias)); err != nil {
				return err
			}
		}
	}
	defaultMode := os.FileMode(0o644)
	if config.AdminToken != "" || config.SentryDSN != "" {
		defaultMode = 0o600
	}
	if err := os.WriteFile(systemdDefaultPath(config), []byte(defaults), defaultMode); err != nil {
		return err
	}
	if err := os.WriteFile(systemdUnitPath(config), []byte(unit), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(systemdSocketPath(config), []byte(socketUnit), 0o644); err != nil {
		return err
	}
	chownPaths := []string{
		config.Home,
		filepath.Join(config.Home, ".codex"),
		filepath.Join(config.Home, "codex"),
		filepath.Join(config.Home, "codex", "accounts"),
		filepath.Dir(config.SessionsPath),
		"/var/log/subrouter",
	}
	if strings.TrimSpace(config.TranscriptsDir) != "" {
		chownPaths = append(chownPaths, config.TranscriptsDir)
	}
	if err := runner.Run("chown", append([]string{config.User + ":" + config.Group}, chownPaths...)...); err != nil {
		return err
	}
	if err := runner.Run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if config.Start {
		if err := runner.Run("systemctl", "enable", config.ServiceName+".socket", config.ServiceName); err != nil {
			return err
		}
		if err := runner.Run("systemctl", "restart", config.ServiceName); err != nil {
			return err
		}
	}

	fmt.Printf("Installed %s\n", config.InstallPath)
	if config.InstallAliases {
		fmt.Printf("Installed aliases in %s\n", filepath.Dir(config.InstallPath))
	}
	fmt.Printf("Installed %s\n", systemdUnitPath(config))
	fmt.Printf("Installed %s\n", systemdSocketPath(config))
	if config.Start {
		fmt.Printf("Started %s\n", config.ServiceName)
	}
	return nil
}

func validateSystemdConfig(config systemdConfig) error {
	if strings.TrimSpace(config.ServiceName) == "" {
		return errors.New("service is required")
	}
	if strings.ContainsAny(config.ServiceName, `/\`) {
		return errors.New("service must be a systemd unit basename")
	}
	if strings.TrimSpace(config.User) == "" || strings.TrimSpace(config.Group) == "" {
		return errors.New("user and group are required")
	}
	if strings.TrimSpace(config.Home) == "" {
		return errors.New("home is required")
	}
	if strings.TrimSpace(config.Addr) == "" {
		return errors.New("addr is required")
	}
	if _, _, err := net.SplitHostPort(config.Addr); err != nil {
		return fmt.Errorf("addr must include host and port: %w", err)
	}
	if strings.TrimSpace(config.InstallPath) == "" {
		return errors.New("install-path is required")
	}
	if strings.TrimSpace(config.SessionsPath) == "" {
		return errors.New("sessions is required")
	}
	if strings.TrimSpace(config.SRSwitchInterval) == "" {
		return errors.New("sr-switch-interval is required")
	}
	if _, err := time.ParseDuration(config.SRSwitchInterval); err != nil {
		return fmt.Errorf("sr-switch-interval must be a Go duration such as 10m: %w", err)
	}
	return nil
}

func ensureSystemUser(config systemdConfig, runner commandRunner) error {
	if err := runner.Run("getent", "group", config.Group); err != nil {
		if err := runner.Run("groupadd", "--system", config.Group); err != nil {
			return err
		}
	}
	if err := runner.Run("id", "-u", config.User); err == nil {
		return nil
	}
	return runner.Run("useradd", "--system", "--gid", config.Group, "--home-dir", config.Home, "--create-home", "--shell", "/usr/sbin/nologin", config.User)
}

// applyExistingSystemdDefaults backfills config values from an existing
// EnvironmentFile so re-running install-systemd without re-passing every flag
// preserves the prior configuration. SUBROUTER_TRANSCRIPTS must be preserved
// alongside SUBROUTER_EXTRA_ARGS: dropping the transcript dir while keeping a
// --transcript-gcs-uri in extra args yields a config that serve rejects at
// startup, which crash-loops the service behind socket activation.
func applyExistingSystemdDefaults(config *systemdConfig, defaultPath string) {
	if config.ExtraArgs == "" {
		config.ExtraArgs = readDefaultValue(defaultPath, "SUBROUTER_EXTRA_ARGS")
		if config.ExtraArgs == "" && config.ReplaceLegacy {
			config.ExtraArgs = readLegacySystemdExtraArgs()
		}
	}
	if config.TranscriptsDir == "" {
		config.TranscriptsDir = readDefaultValue(defaultPath, "SUBROUTER_TRANSCRIPTS")
	}
	if config.AdminToken == "" {
		config.AdminToken = readDefaultValue(defaultPath, "SUBROUTER_ADMIN_TOKEN")
	}
	if config.SentryDSN == "" {
		config.SentryDSN = readDefaultValue(defaultPath, "SENTRY_DSN")
	}
}

func readLegacySystemdExtraArgs() string {
	for _, legacy := range []struct {
		path string
		key  string
	}{
		{"/etc/default/switchboard", "SWITCHBOARD_EXTRA_ARGS"},
		{"/etc/default/gateway", "GATEWAY_EXTRA_ARGS"},
	} {
		if value := readDefaultValue(legacy.path, legacy.key); value != "" {
			return value
		}
	}
	return ""
}

func readDefaultValue(path, keyName string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(body), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != keyName {
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"`)
		return value
	}
	return ""
}

func stopLegacySystemdServices(runner commandRunner) {
	for _, service := range []string{"switchboard", "gateway"} {
		_ = runner.Run("systemctl", "disable", "--now", service)
	}
}

func migrateLegacySystemdState(config systemdConfig, runner commandRunner) {
	for _, legacyHome := range []string{"/var/lib/switchboard", "/var/lib/gateway"} {
		if legacyHome == config.Home {
			continue
		}
		if info, err := os.Stat(legacyHome); err != nil || !info.IsDir() {
			continue
		}
		_ = runner.Run("cp", "-a", "-n", legacyHome+"/.", config.Home+"/")
	}
	_ = storepath.MigrateCodexDir(filepath.Join(config.Home, "codex"), filepath.Join(config.Home, ".codex-accounts"))
}

func systemdDefaultPath(config systemdConfig) string {
	return filepath.Join("/etc/default", config.ServiceName)
}

func systemdUnitPath(config systemdConfig) string {
	return filepath.Join("/etc/systemd/system", config.ServiceName+".service")
}

func systemdSocketPath(config systemdConfig) string {
	return filepath.Join("/etc/systemd/system", config.ServiceName+".socket")
}

func systemdDefaults(config systemdConfig) string {
	transcriptArgs := ""
	if strings.TrimSpace(config.TranscriptsDir) != "" {
		transcriptArgs = "--transcripts=" + strings.TrimSpace(config.TranscriptsDir)
	}
	return fmt.Sprintf(`SUBROUTER_ADDR=%s
SUBROUTER_STATE_DIR=%s
SUBROUTER_SESSIONS=%s
SUBROUTER_TRANSCRIPTS=%s
SUBROUTER_TRANSCRIPT_ARGS=%q
SUBROUTER_SR_SWITCH_INTERVAL=%s
SUBROUTER_ADMIN_TOKEN=%q
SENTRY_DSN=%q
SUBROUTER_EXTRA_ARGS=%q
`, config.Addr, config.Home, config.SessionsPath, config.TranscriptsDir, transcriptArgs, config.SRSwitchInterval, config.AdminToken, config.SentryDSN, config.ExtraArgs)
}

func systemdUnit(config systemdConfig) (string, error) {
	data := struct {
		ServiceName string
		SocketName  string
		User        string
		Group       string
		Home        string
		InstallPath string
		DefaultPath string
	}{
		ServiceName: config.ServiceName,
		SocketName:  config.ServiceName + ".socket",
		User:        config.User,
		Group:       config.Group,
		Home:        config.Home,
		InstallPath: config.InstallPath,
		DefaultPath: systemdDefaultPath(config),
	}
	var out bytes.Buffer
	if err := systemdTemplate.Execute(&out, data); err != nil {
		return "", err
	}
	return out.String(), nil
}

func systemdSocket(config systemdConfig) (string, error) {
	data := struct {
		ServiceName string
		Addr        string
	}{
		ServiceName: config.ServiceName + ".service",
		Addr:        config.Addr,
	}
	var out bytes.Buffer
	if err := systemdSocketTemplate.Execute(&out, data); err != nil {
		return "", err
	}
	return out.String(), nil
}

var systemdTemplate = template.Must(template.New("systemd").Parse(`[Unit]
Description=Subrouter AI agent router
Wants=network-online.target
Requires={{.SocketName}}
After=network-online.target {{.SocketName}}
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=simple
Sockets={{.SocketName}}
User={{.User}}
Group={{.Group}}
WorkingDirectory={{.Home}}
Environment=HOME={{.Home}}
EnvironmentFile=-{{.DefaultPath}}
ExecStart={{.InstallPath}} serve --addr ${SUBROUTER_ADDR} --sessions ${SUBROUTER_SESSIONS} $SUBROUTER_TRANSCRIPT_ARGS --sr-switch-interval ${SUBROUTER_SR_SWITCH_INTERVAL} $SUBROUTER_EXTRA_ARGS
Restart=on-failure
RestartSec=3
TimeoutStopSec=10min
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true
ReadWritePaths={{.Home}} /var/log/subrouter

[Install]
WantedBy=multi-user.target
`))

var systemdSocketTemplate = template.Must(template.New("systemd-socket").Parse(`[Unit]
Description=Subrouter AI agent router socket

[Socket]
ListenStream={{.Addr}}
NoDelay=true
Service={{.ServiceName}}

[Install]
WantedBy=sockets.target
`))

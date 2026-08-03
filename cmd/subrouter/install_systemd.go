package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
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
	ServiceName        string
	User               string
	Group              string
	Home               string
	Addr               string
	InstallPath        string
	SessionsPath       string
	TranscriptsDir     string
	SRSwitchInterval   string
	AdminToken         string
	AccountImportToken string
	ExtraArgs          string
	Start              bool
	DryRun             bool
	InstallAliases     bool
	ReplaceLegacy      bool
}

func installSystemd(args []string) error {
	return installSystemdWithInput(args, os.Stdin)
}

func installSystemdWithInput(args []string, input io.Reader) error {
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
	adminTokenStdin := flags.Bool("admin-token-stdin", false, "read the admin token from standard input without exposing it in process arguments")
	flags.StringVar(&config.AccountImportToken, "account-import-token", "", "token limited to protected account import; preserves existing SUBROUTER_ACCOUNT_IMPORT_TOKEN by default")
	accountImportTokenStdin := flags.Bool("account-import-token-stdin", false, "read the account import token from standard input without exposing it in process arguments")
	flags.StringVar(&config.ExtraArgs, "extra-args", "", "extra arguments appended to subrouter serve")
	flags.BoolVar(&config.Start, "start", true, "enable and restart the systemd service")
	flags.BoolVar(&config.DryRun, "dry-run", false, "print the systemd unit without writing files")
	flags.BoolVar(&config.InstallAliases, "install-aliases", true, "install sr and cx symlinks to the subrouter binary")
	flags.BoolVar(&config.ReplaceLegacy, "replace-legacy", true, "stop legacy switchboard/gateway services and migrate their state")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *adminTokenStdin && strings.TrimSpace(config.AdminToken) != "" {
		return errors.New("--admin-token and --admin-token-stdin cannot be used together")
	}
	if *accountImportTokenStdin && strings.TrimSpace(config.AccountImportToken) != "" {
		return errors.New("--account-import-token and --account-import-token-stdin cannot be used together")
	}
	requestedTokens := 0
	if *adminTokenStdin {
		requestedTokens++
	}
	if *accountImportTokenStdin {
		requestedTokens++
	}
	if requestedTokens > 0 {
		tokens, err := readSystemdTokens(input, requestedTokens)
		if err != nil {
			return err
		}
		index := 0
		if *adminTokenStdin {
			config.AdminToken = tokens[index]
			index++
		}
		if *accountImportTokenStdin {
			config.AccountImportToken = tokens[index]
		}
	}
	if runtime.GOOS != "linux" && !config.DryRun {
		return errors.New("install-systemd is Linux-only")
	}
	applyExistingSystemdDefaults(&config, systemdDefaultPath(config))
	return installSystemdWithConfig(config, commandRunner{})
}

func readSystemdAdminToken(input io.Reader) (string, error) {
	tokens, err := readSystemdTokens(input, 1)
	if err != nil {
		return "", err
	}
	return tokens[0], nil
}

func readSystemdTokens(input io.Reader, count int) ([]string, error) {
	if input == nil {
		return nil, errors.New("token input is unavailable")
	}
	limit := int64(count*4097 + 1)
	body, err := io.ReadAll(io.LimitReader(input, limit))
	if err != nil {
		return nil, fmt.Errorf("read service token: %w", err)
	}
	if int64(len(body)) >= limit {
		return nil, errors.New("service token input is too large")
	}
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) != count {
		return nil, fmt.Errorf("expected %d service token line(s)", count)
	}
	tokens := make([]string, count)
	for index, line := range lines {
		token := strings.TrimSpace(line)
		if token == "" {
			return nil, errors.New("service token is required on standard input")
		}
		if len(token) > 4096 {
			return nil, errors.New("service token is too large")
		}
		if strings.ContainsAny(token, "\r\n\x00") {
			return nil, errors.New("service token contains invalid characters")
		}
		tokens[index] = token
	}
	return tokens, nil
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
	defaultMode := systemdDefaultsMode(config)
	if err := writeFileAtomic(systemdDefaultPath(config), []byte(defaults), defaultMode); err != nil {
		return err
	}
	if err := writeFileAtomic(systemdUnitPath(config), []byte(unit), 0o644); err != nil {
		return err
	}
	if err := writeFileAtomic(systemdSocketPath(config), []byte(socketUnit), 0o644); err != nil {
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

func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

func systemdDefaultsMode(config systemdConfig) os.FileMode {
	if config.AdminToken != "" || config.AccountImportToken != "" {
		return 0o600
	}
	return 0o644
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
	for name, value := range map[string]string{
		"addr":                 config.Addr,
		"home":                 config.Home,
		"sessions":             config.SessionsPath,
		"transcripts":          config.TranscriptsDir,
		"sr-switch-interval":   config.SRSwitchInterval,
		"admin-token":          config.AdminToken,
		"account-import-token": config.AccountImportToken,
		"extra-args":           config.ExtraArgs,
	} {
		if strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("%s contains a newline or NUL byte", name)
		}
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
	if config.AccountImportToken == "" {
		config.AccountImportToken = readDefaultValue(defaultPath, "SUBROUTER_ACCOUNT_IMPORT_TOKEN")
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
		return parseSystemdEnvironmentValue(strings.TrimSpace(value))
	}
	return ""
}

// EnvironmentFile double-quoted values use shell-like escaping only for a
// small set of characters. Keep the writer and reader paired so a reinstall
// preserves credentials containing quotes or backslashes byte for byte.
func quoteSystemdEnvironmentValue(value string) string {
	var quoted strings.Builder
	quoted.Grow(len(value) + 2)
	quoted.WriteByte('"')
	for _, char := range value {
		if char == '\\' || char == '"' {
			quoted.WriteByte('\\')
		}
		quoted.WriteRune(char)
	}
	quoted.WriteByte('"')
	return quoted.String()
}

func parseSystemdEnvironmentValue(value string) string {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return value
	}
	value = value[1 : len(value)-1]
	var unquoted strings.Builder
	unquoted.Grow(len(value))
	for index := 0; index < len(value); index++ {
		if value[index] == '\\' && index+1 < len(value) {
			next := value[index+1]
			if next == '\\' || next == '"' {
				unquoted.WriteByte(next)
				index++
				continue
			}
		}
		unquoted.WriteByte(value[index])
	}
	return unquoted.String()
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
SUBROUTER_TRANSCRIPT_ARGS=%s
SUBROUTER_SR_SWITCH_INTERVAL=%s
SUBROUTER_ADMIN_TOKEN=%s
SUBROUTER_ACCOUNT_IMPORT_TOKEN=%s
SUBROUTER_EXTRA_ARGS=%s
`, config.Addr, config.Home, config.SessionsPath, config.TranscriptsDir,
		quoteSystemdEnvironmentValue(transcriptArgs), config.SRSwitchInterval,
		quoteSystemdEnvironmentValue(config.AdminToken),
		quoteSystemdEnvironmentValue(config.AccountImportToken),
		quoteSystemdEnvironmentValue(config.ExtraArgs))
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

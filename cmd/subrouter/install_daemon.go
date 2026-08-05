package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"
)

const defaultDaemonLabel = "ai.manaflow.subrouter"

type daemonConfig struct {
	Label                string
	Addr                 string
	AdminToken           string
	AdminTokenFile       string
	InstallPath          string
	TranscriptsDir       string
	LogDir               string
	WorkingDirectory     string
	SRSwitchInterval     string
	Path                 string
	InstallSRAlias       bool
	SRAliasPath          string
	InstallLegacyCXAlias bool
	LegacyCXAliasPath    string
	Start                bool
	DryRun               bool
}

func installDaemon(args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	defaultInstallPath := filepath.Join(home, "bin", "subrouter")
	flags := flag.NewFlagSet("install-daemon", flag.ContinueOnError)
	config := daemonConfig{}
	flags.StringVar(&config.Label, "label", defaultDaemonLabel, "launchd label")
	flags.StringVar(&config.Addr, "addr", "127.0.0.1:31415", "daemon listen address; a non-loopback address (e.g. a tailnet IP or MagicDNS name) requires --admin-token or --admin-token-file")
	flags.StringVar(&config.AdminToken, "admin-token", "", "admin token for a non-loopback bind, written into the plist as SUBROUTER_ADMIN_TOKEN; the plist is then chmod 0600. Prefer --admin-token-file")
	flags.StringVar(&config.AdminTokenFile, "admin-token-file", "", "path to a file holding the admin token, written into the plist as SUBROUTER_ADMIN_TOKEN_FILE; keeps the secret out of the plist entirely")
	flags.StringVar(&config.InstallPath, "install-path", defaultInstallPath, "subrouter binary install path")
	flags.StringVar(&config.TranscriptsDir, "transcripts", "", "local transcript directory; empty disables transcript recording")
	flags.StringVar(&config.LogDir, "log-dir", filepath.Join(home, "Library", "Logs"), "daemon log directory")
	flags.StringVar(&config.WorkingDirectory, "working-directory", cwd, "daemon working directory")
	flags.StringVar(&config.SRSwitchInterval, "sr-switch-interval", "10m", "sr auto-switch interval; 0 disables")
	flags.StringVar(&config.SRSwitchInterval, "cx-switch-interval", "10m", "compatibility alias for --sr-switch-interval")
	flags.StringVar(&config.Path, "path", defaultDaemonPath(defaultInstallPath), "PATH for the daemon")
	flags.BoolVar(&config.InstallSRAlias, "install-sr-shim", true, "install sr as a symlink to the subrouter binary")
	flags.StringVar(&config.SRAliasPath, "sr-shim-path", filepath.Join(home, "bin", "sr"), "sr symlink path")
	flags.BoolVar(&config.InstallLegacyCXAlias, "install-cx-shim", true, "install cx as a compatibility symlink to the subrouter binary")
	flags.StringVar(&config.LegacyCXAliasPath, "cx-shim-path", filepath.Join(home, "bin", "cx"), "cx compatibility symlink path")
	flags.BoolVar(&config.Start, "start", true, "load and restart the LaunchAgent after installation")
	flags.BoolVar(&config.DryRun, "dry-run", false, "print the LaunchAgent plist without writing files")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" && !config.DryRun {
		return errors.New("install-daemon is currently macOS-only; use systemd or another supervisor on this OS")
	}
	return installDaemonWithConfig(config, home, commandRunner{})
}

func installDaemonWithConfig(config daemonConfig, home string, runner commandRunner) error {
	if err := validateDaemonConfig(config); err != nil {
		return err
	}
	// The plist must carry the absolute path: launchd resolves relative paths
	// against WorkingDirectory, not the installing shell's cwd.
	resolvedTokenFile, err := resolveAdminTokenFile(config.AdminTokenFile)
	if err != nil {
		return err
	}
	if resolvedTokenFile != "" {
		config.AdminTokenFile = resolvedTokenFile
		if info, statErr := os.Stat(resolvedTokenFile); statErr == nil {
			if warning := adminTokenFileWarning(resolvedTokenFile, info.Mode()); warning != "" {
				fmt.Fprintln(os.Stderr, warning)
			}
		}
	}
	plist, err := launchAgentPlist(config, home)
	if err != nil {
		return err
	}
	if config.DryRun {
		fmt.Print(plist)
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(config.InstallPath), 0o755); err != nil {
		return err
	}
	if strings.TrimSpace(config.TranscriptsDir) != "" {
		if err := os.MkdirAll(config.TranscriptsDir, 0o700); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(config.LogDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(home, "Library", "LaunchAgents"), 0o755); err != nil {
		return err
	}
	if err := installCurrentExecutable(config.InstallPath); err != nil {
		return err
	}
	if config.InstallSRAlias {
		if err := installBinaryAlias(config.InstallPath, config.SRAliasPath); err != nil {
			return err
		}
	}
	if config.InstallLegacyCXAlias {
		if err := installBinaryAlias(config.InstallPath, config.LegacyCXAliasPath); err != nil {
			return err
		}
	}

	plistPath := launchAgentPath(home, config.Label)
	if err := os.WriteFile(plistPath, []byte(plist), launchAgentMode(config)); err != nil {
		return err
	}
	if config.Start {
		if err := restartLaunchAgent(plistPath, config.Label, runner); err != nil {
			return err
		}
	}

	fmt.Printf("Installed %s\n", config.InstallPath)
	if config.InstallSRAlias {
		fmt.Printf("Installed %s -> %s\n", config.SRAliasPath, config.InstallPath)
	}
	if config.InstallLegacyCXAlias {
		fmt.Printf("Installed %s -> %s\n", config.LegacyCXAliasPath, config.InstallPath)
	}
	fmt.Printf("Installed %s\n", plistPath)
	if config.Start {
		fmt.Printf("Started %s\n", config.Label)
	}
	return nil
}

// launchAgentMode mirrors systemdDefaultsMode: a plist carrying the admin token
// inline is readable by every local user at the default 0644, which would hand
// the credential to exactly the audience the token exists to gate. --admin-token-file
// keeps the secret in a separate file, so the plist itself stays 0644.
// resolveAdminTokenFile turns --admin-token-file into the absolute path the
// plist should carry, and proves the daemon will be able to read it.
//
// Both halves have to happen at install time rather than at boot. The plist
// sets KeepAlive, so a token file the daemon cannot read is not one clear
// failure: serve exits, launchd restarts it, and the only signal an operator
// gets is churn in subrouter.err.log. And a relative path would be resolved
// against the LaunchAgent's WorkingDirectory rather than the shell the
// installer ran in, so it would quietly find a different file, or none.
//
// The readability check runs through the same readSecretFile the daemon uses,
// so "install succeeded" implies "serve can start".
func resolveAdminTokenFile(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("admin-token-file %q: %w", trimmed, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("admin-token-file %s: %w", absolute, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("admin-token-file %s is a directory", absolute)
	}
	if _, err := readSecretFile(absolute, "admin-token-file"); err != nil {
		return "", err
	}
	return absolute, nil
}

// adminTokenFileWarning reports a token file other local users can read. That
// is not fatal -- the operator may have deliberate reasons -- but it defeats
// the only thing --admin-token-file buys over --admin-token, so it should not
// pass silently.
func adminTokenFileWarning(path string, mode os.FileMode) string {
	if mode.Perm()&0o077 == 0 {
		return ""
	}
	return fmt.Sprintf(
		"warning: %s is mode %04o, so other local users can read the admin token; chmod 600 it",
		path, mode.Perm(),
	)
}

func launchAgentMode(config daemonConfig) os.FileMode {
	if strings.TrimSpace(config.AdminToken) != "" {
		return 0o600
	}
	return 0o644
}

func validateDaemonConfig(config daemonConfig) error {
	if strings.TrimSpace(config.Label) == "" {
		return errors.New("label is required")
	}
	if strings.TrimSpace(config.Addr) == "" {
		return errors.New("addr is required")
	}
	if _, _, err := net.SplitHostPort(config.Addr); err != nil {
		return fmt.Errorf("addr must include host and port: %w", err)
	}
	// A non-loopback bind is allowed — it is what tailnet sharing needs — but it
	// carries the same exposure `serve` refuses to arm silently: every host the
	// ACLs admit reaches the admin surface (accounts, transcripts,
	// account-import, drain). Reuse serve's predicate rather than a second
	// spelling of "is this loopback", so the installed daemon can never be armed
	// in a state serve itself would reject at startup.
	if bindAddrRequiresAdminToken(config.Addr) &&
		strings.TrimSpace(config.AdminToken) == "" &&
		strings.TrimSpace(config.AdminTokenFile) == "" {
		return fmt.Errorf(
			"refusing to install a daemon bound to %s without an admin token: every host that can reach this address would get the admin endpoints (accounts, transcripts, account-import, drain). Pass --admin-token-file (preferred; keeps the secret out of the plist), --admin-token, or bind a loopback address such as 127.0.0.1:31415",
			config.Addr,
		)
	}
	if strings.TrimSpace(config.AdminToken) != "" && strings.TrimSpace(config.AdminTokenFile) != "" {
		return errors.New("pass only one of --admin-token or --admin-token-file")
	}
	if _, err := resolveAdminTokenFile(config.AdminTokenFile); err != nil {
		return err
	}
	if strings.TrimSpace(config.InstallPath) == "" {
		return errors.New("install-path is required")
	}
	if config.InstallSRAlias && strings.TrimSpace(config.SRAliasPath) == "" {
		return errors.New("sr-shim-path is required")
	}
	if config.InstallLegacyCXAlias && strings.TrimSpace(config.LegacyCXAliasPath) == "" {
		return errors.New("cx-shim-path is required")
	}
	if strings.TrimSpace(config.LogDir) == "" {
		return errors.New("log-dir is required")
	}
	if strings.TrimSpace(config.WorkingDirectory) == "" {
		return errors.New("working-directory is required")
	}
	if strings.TrimSpace(config.SRSwitchInterval) == "" {
		return errors.New("sr-switch-interval is required")
	}
	if _, err := time.ParseDuration(config.SRSwitchInterval); err != nil {
		return fmt.Errorf("sr-switch-interval must be a Go duration such as 10m: %w", err)
	}
	return nil
}

func installBinaryAlias(subrouterPath, shimPath string) error {
	if err := os.MkdirAll(filepath.Dir(shimPath), 0o755); err != nil {
		return err
	}
	subrouterPath, err := filepath.Abs(subrouterPath)
	if err != nil {
		return err
	}
	shimPath, err = filepath.Abs(shimPath)
	if err != nil {
		return err
	}
	if sameFile(subrouterPath, shimPath) {
		return nil
	}
	_ = os.Remove(shimPath)
	return os.Symlink(subrouterPath, shimPath)
}

func installCurrentExecutable(destination string) error {
	source, err := os.Executable()
	if err != nil {
		return err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return err
	}
	if sameFile(source, destination) {
		return nil
	}

	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()

	tmp := destination + ".tmp"
	output, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, destination)
}

func sameFile(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return os.SameFile(leftInfo, rightInfo)
}

func restartLaunchAgent(plistPath, label string, runner commandRunner) error {
	uid := strconv.Itoa(os.Getuid())
	domain := "gui/" + uid
	service := domain + "/" + label
	_ = runner.Run("launchctl", "bootout", domain, plistPath)
	if err := runner.Run("launchctl", "bootstrap", domain, plistPath); err != nil {
		return err
	}
	return runner.Run("launchctl", "kickstart", "-k", service)
}

type commandRunner struct{}

func (commandRunner) Run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func launchAgentPath(home, label string) string {
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

func defaultDaemonPath(installPath string) string {
	return strings.Join([]string{
		filepath.Dir(installPath),
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
		"/usr/sbin",
		"/sbin",
	}, ":")
}

func daemonCloudConfigPath(home string) (string, error) {
	path := strings.TrimSpace(os.Getenv("SUBROUTER_CLOUD_CONFIG"))
	if path == "" {
		path = filepath.Join(home, ".config", "subrouter", "cloud.json")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve cloud config path: %w", err)
	}
	return absolute, nil
}

func launchAgentPlist(config daemonConfig, home string) (string, error) {
	cloudConfigPath, err := daemonCloudConfigPath(home)
	if err != nil {
		return "", err
	}
	data := struct {
		Label             string
		Home              string
		Path              string
		InstallPath       string
		Addr              string
		AdminToken        string
		HasAdminToken     bool
		AdminTokenFile    string
		HasAdminTokenFile bool
		TranscriptsDir    string
		HasTranscripts    bool
		SRSwitchInterval  string
		LogPath           string
		ErrorLogPath      string
		WorkingDirectory  string
		CloudConfigPath   string
	}{
		Label:             escapeXMLString(config.Label),
		Home:              escapeXMLString(home),
		Path:              escapeXMLString(config.Path),
		InstallPath:       escapeXMLString(config.InstallPath),
		Addr:              escapeXMLString(config.Addr),
		AdminToken:        escapeXMLString(config.AdminToken),
		HasAdminToken:     strings.TrimSpace(config.AdminToken) != "",
		AdminTokenFile:    escapeXMLString(config.AdminTokenFile),
		HasAdminTokenFile: strings.TrimSpace(config.AdminTokenFile) != "",
		TranscriptsDir:    escapeXMLString(config.TranscriptsDir),
		HasTranscripts:    strings.TrimSpace(config.TranscriptsDir) != "",
		SRSwitchInterval:  escapeXMLString(config.SRSwitchInterval),
		LogPath:           escapeXMLString(filepath.Join(config.LogDir, "subrouter.log")),
		ErrorLogPath:      escapeXMLString(filepath.Join(config.LogDir, "subrouter.err.log")),
		WorkingDirectory:  escapeXMLString(config.WorkingDirectory),
		CloudConfigPath:   escapeXMLString(cloudConfigPath),
	}

	var out bytes.Buffer
	if err := launchAgentTemplate.Execute(&out, data); err != nil {
		return "", err
	}
	return out.String(), nil
}

func escapeXMLString(value string) string {
	var out bytes.Buffer
	_ = xml.EscapeText(&out, []byte(value))
	return out.String()
}

var launchAgentTemplate = template.Must(template.New("launch-agent").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>{{.Home}}</string>
		<key>PATH</key>
		<string>{{.Path}}</string>
		{{if .HasAdminTokenFile}}<key>SUBROUTER_ADMIN_TOKEN_FILE</key>
		<string>{{.AdminTokenFile}}</string>
		{{end}}{{if .HasAdminToken}}<key>SUBROUTER_ADMIN_TOKEN</key>
		<string>{{.AdminToken}}</string>
		{{end}}</dict>
	<key>KeepAlive</key>
	<true/>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.InstallPath}}</string>
		<string>serve</string>
		<string>--addr</string>
		<string>{{.Addr}}</string>
		<string>--cloud-config</string>
		<string>{{.CloudConfigPath}}</string>
		{{if .HasTranscripts}}<string>--transcripts</string>
		<string>{{.TranscriptsDir}}</string>
		{{end}}<string>--sr-switch-interval</string>
		<string>{{.SRSwitchInterval}}</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardErrorPath</key>
	<string>{{.ErrorLogPath}}</string>
	<key>StandardOutPath</key>
	<string>{{.LogPath}}</string>
	<key>WorkingDirectory</key>
	<string>{{.WorkingDirectory}}</string>
</dict>
</plist>
`))

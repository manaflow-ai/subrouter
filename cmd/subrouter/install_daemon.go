package main

import (
	"bytes"
	"debug/buildinfo"
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
	ForceShims           bool
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
	flags.StringVar(&config.Addr, "addr", "127.0.0.1:31415", "daemon listen address")
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
	flags.BoolVar(&config.ForceShims, "force-shims", false, "replace an existing sr or cx even when it is not a subrouter binary")
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
		if err := installBinaryAliasWith(config.InstallPath, config.SRAliasPath, config.ForceShims); err != nil {
			return err
		}
	}
	if config.InstallLegacyCXAlias {
		if err := installBinaryAliasWith(config.InstallPath, config.LegacyCXAliasPath, config.ForceShims); err != nil {
			return err
		}
	}

	plistPath := launchAgentPath(home, config.Label)
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
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

func validateDaemonConfig(config daemonConfig) error {
	if strings.TrimSpace(config.Label) == "" {
		return errors.New("label is required")
	}
	if strings.TrimSpace(config.Addr) == "" {
		return errors.New("addr is required")
	}
	host, _, err := net.SplitHostPort(config.Addr)
	if err != nil {
		return fmt.Errorf("addr must include host and port: %w", err)
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return fmt.Errorf("install-daemon only supports localhost addresses, got %q", config.Addr)
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

// errForeignAlias reports an existing sr/cx at the alias path that is not
// Subrouter's, which an install refuses to delete without --force-shims.
var errForeignAlias = errors.New("not a subrouter binary or a symlink to one")

const subrouterMainPackage = "github.com/manaflow-ai/subrouter/cmd/subrouter"

// installBinaryAlias points shimPath at subrouterPath, replacing only an
// existing alias that is already Subrouter's.
func installBinaryAlias(subrouterPath, shimPath string) error {
	return installBinaryAliasWith(subrouterPath, shimPath, false)
}

// installBinaryAliasWith is installBinaryAlias; force also replaces a file or
// symlink that belongs to another tool. The new link is created beside the
// alias and renamed over it, so a failure never leaves the path empty.
func installBinaryAliasWith(subrouterPath, shimPath string, force bool) error {
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
	if !force {
		ours, err := isSubrouterAlias(shimPath)
		if err != nil {
			return err
		}
		if !ours {
			return fmt.Errorf("refusing to replace %s: %w; move it aside or rerun with --force-shims", shimPath, errForeignAlias)
		}
	}
	if info, err := os.Lstat(shimPath); err == nil && info.IsDir() {
		return fmt.Errorf("refusing to replace %s: it is a directory", shimPath)
	}
	tmp := fmt.Sprintf("%s.tmp-%d", shimPath, os.Getpid())
	_ = os.Remove(tmp)
	if err := os.Symlink(subrouterPath, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, shimPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// isSubrouterAlias reports whether path is absent or already Subrouter's: a
// symlink whose target is named subrouter (including a dangling link left by
// an older install path) or resolves to a Subrouter binary, or a regular file
// that is a Subrouter binary.
func isSubrouterAlias(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return false, err
		}
		if filepath.Base(target) == "subrouter" {
			return true, nil
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return false, nil
		}
		return isSubrouterBinary(resolved), nil
	case info.Mode().IsRegular():
		return isSubrouterBinary(path), nil
	default:
		return false, nil
	}
}

func isSubrouterBinary(path string) bool {
	info, err := buildinfo.ReadFile(path)
	return err == nil && info.Path == subrouterMainPackage
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

func restartLaunchAgent(plistPath, label string, runner taskRunner) error {
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
		Label            string
		Home             string
		Path             string
		InstallPath      string
		Addr             string
		TranscriptsDir   string
		HasTranscripts   bool
		SRSwitchInterval string
		LogPath          string
		ErrorLogPath     string
		WorkingDirectory string
		CloudConfigPath  string
	}{
		Label:            escapeXMLString(config.Label),
		Home:             escapeXMLString(home),
		Path:             escapeXMLString(config.Path),
		InstallPath:      escapeXMLString(config.InstallPath),
		Addr:             escapeXMLString(config.Addr),
		TranscriptsDir:   escapeXMLString(config.TranscriptsDir),
		HasTranscripts:   strings.TrimSpace(config.TranscriptsDir) != "",
		SRSwitchInterval: escapeXMLString(config.SRSwitchInterval),
		LogPath:          escapeXMLString(filepath.Join(config.LogDir, "subrouter.log")),
		ErrorLogPath:     escapeXMLString(filepath.Join(config.LogDir, "subrouter.err.log")),
		WorkingDirectory: escapeXMLString(config.WorkingDirectory),
		CloudConfigPath:  escapeXMLString(cloudConfigPath),
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
	</dict>
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

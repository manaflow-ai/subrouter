package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultLaunchdServerLabel  = "ai.manaflow.subrouter-team"
	defaultLaunchdStateDir     = "/var/lib/subrouter"
	adminTokenFileName         = "admin-token"
	accountImportTokenFileName = "account-import-token"
	plistBuddyPath             = "/usr/libexec/PlistBuddy"
)

// launchdConfig provisions server control credentials into an existing macOS
// LaunchDaemon. Unlike install-systemd this does not create the service: a
// shared macOS host is built once (service user, supervisor layout, pf rules,
// log rotation) and from then on only its credentials change.
type launchdConfig struct {
	Label              string
	PlistPath          string
	StateDir           string
	AdminToken         string
	AccountImportToken string
	HealthURL          string
	Start              bool
	DryRun             bool
}

func installLaunchd(args []string) error {
	return installLaunchdWithInput(args, os.Stdin, os.Stdout)
}

func installLaunchdWithInput(args []string, input io.Reader, out io.Writer) error {
	config := launchdConfig{}
	flags := flag.NewFlagSet("install-launchd", flag.ContinueOnError)
	flags.StringVar(&config.Label, "label", defaultLaunchdServerLabel, "LaunchDaemon label")
	flags.StringVar(&config.PlistPath, "plist", "", "LaunchDaemon plist path; defaults to /Library/LaunchDaemons/<label>.plist")
	flags.StringVar(&config.StateDir, "state-dir", defaultLaunchdStateDir, "service state directory that holds the credential files")
	flags.StringVar(&config.AdminToken, "admin-token", "", "admin token required for non-loopback _subrouter endpoints")
	adminTokenStdin := flags.Bool("admin-token-stdin", false, "read the admin token from standard input without exposing it in process arguments")
	flags.StringVar(&config.AccountImportToken, "account-import-token", "", "token limited to protected account import")
	accountImportTokenStdin := flags.Bool("account-import-token-stdin", false, "read the account import token from standard input without exposing it in process arguments")
	flags.StringVar(&config.HealthURL, "health-url", "http://127.0.0.1:31415/_subrouter/health", "health endpoint polled after the service reloads")
	flags.BoolVar(&config.Start, "start", true, "reload the LaunchDaemon so the new credentials take effect")
	flags.BoolVar(&config.DryRun, "dry-run", false, "print the planned changes without writing files")
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
	if runtime.GOOS != "darwin" && !config.DryRun {
		return errors.New("install-launchd is macOS-only")
	}
	return installLaunchdWithConfig(config, commandRunner{}, out)
}

func (c launchdConfig) plistPath() string {
	if strings.TrimSpace(c.PlistPath) != "" {
		return c.PlistPath
	}
	return filepath.Join("/Library/LaunchDaemons", c.Label+".plist")
}

func (c launchdConfig) adminTokenPath() string {
	return filepath.Join(c.StateDir, adminTokenFileName)
}

func (c launchdConfig) accountImportTokenPath() string {
	return filepath.Join(c.StateDir, accountImportTokenFileName)
}

func validateLaunchdConfig(config launchdConfig) error {
	if strings.TrimSpace(config.Label) == "" {
		return errors.New("--label is required")
	}
	if strings.TrimSpace(config.StateDir) == "" || !filepath.IsAbs(config.StateDir) {
		return errors.New("--state-dir must be an absolute path")
	}
	if strings.TrimSpace(config.AdminToken) == "" {
		return errors.New("an admin token is required")
	}
	if strings.TrimSpace(config.AccountImportToken) == "" {
		return errors.New("an account import token is required")
	}
	return nil
}

// launchdPlistCommands is the surgical patch applied to an existing plist:
// point the service at credential files and drop any inline token values, which
// the server rejects when both a value and a file are set. PlistBuddy edits in
// place, so every unrelated key (ProgramArguments, UserName, KeepAlive, the
// Bedrock flags a host may carry) survives untouched.
func launchdPlistCommands(config launchdConfig) []string {
	commands := []string{}
	for _, key := range []string{
		"SUBROUTER_ADMIN_TOKEN",
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN",
		"SUBROUTER_ADMIN_TOKEN_FILE",
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE",
	} {
		commands = append(commands, "Delete :EnvironmentVariables:"+key)
	}
	commands = append(commands,
		"Add :EnvironmentVariables:SUBROUTER_ADMIN_TOKEN_FILE string "+config.adminTokenPath(),
		"Add :EnvironmentVariables:SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE string "+config.accountImportTokenPath(),
	)
	return commands
}

func installLaunchdWithConfig(config launchdConfig, runner commandRunner, out io.Writer) error {
	if err := validateLaunchdConfig(config); err != nil {
		return err
	}
	plist := config.plistPath()
	if config.DryRun {
		fmt.Fprintf(out, "plist %s\n", plist)
		fmt.Fprintf(out, "%s (0600)\n", config.adminTokenPath())
		fmt.Fprintf(out, "%s (0600)\n", config.accountImportTokenPath())
		for _, command := range launchdPlistCommands(config) {
			fmt.Fprintf(out, "PlistBuddy -c %q\n", command)
		}
		return nil
	}
	if os.Geteuid() != 0 {
		return errors.New("install-launchd must run as root; use sudo")
	}
	if _, err := os.Stat(plist); err != nil {
		return fmt.Errorf(
			"no LaunchDaemon at %s; build the host first with deploy/macos/migrate-launchdaemon-to-supervisor.sh, then rerun: %w",
			plist, err,
		)
	}
	owner, err := plistServiceUser(plist, runner)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(config.StateDir, 0o750); err != nil {
		return err
	}
	for path, token := range map[string]string{
		config.adminTokenPath():         config.AdminToken,
		config.accountImportTokenPath(): config.AccountImportToken,
	} {
		if err := writeCredentialFile(path, token, owner, runner); err != nil {
			return err
		}
	}
	for _, command := range launchdPlistCommands(config) {
		// Delete fails when the key is absent, which is the normal first run.
		if strings.HasPrefix(command, "Delete ") {
			_ = runner.Run(plistBuddyPath, "-c", command, plist)
			continue
		}
		if err := runner.Run(plistBuddyPath, "-c", command, plist); err != nil {
			return err
		}
	}
	if err := runner.Run("plutil", "-lint", plist); err != nil {
		return fmt.Errorf("patched plist is invalid: %w", err)
	}
	fmt.Fprintf(out, "Provisioned server credentials in %s\n", plist)
	if !config.Start {
		fmt.Fprintf(out, "Reload the service to apply them: launchctl bootout system/%s && launchctl bootstrap system %s\n", config.Label, plist)
		return nil
	}
	return reloadLaunchdService(config, runner, out)
}

// plistServiceUser reads UserName from a LaunchDaemon so credential files land
// with the same owner the service runs as. An empty result means the daemon
// runs as root and no chown is needed.
func plistServiceUser(path string, _ commandRunner) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	user, found, err := plistTopLevelString(body, "UserName")
	if err != nil {
		return "", fmt.Errorf("read %s: %w; convert it with 'plutil -convert xml1 %s'", path, err, path)
	}
	if !found {
		return "", nil
	}
	return user, nil
}

// plistTopLevelString returns a string value from the plist's root dictionary.
// It reads only the root level, so a matching key nested inside
// EnvironmentVariables or another dictionary is never mistaken for it.
func plistTopLevelString(body []byte, wanted string) (string, bool, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	depth := 0
	pendingKey := ""
	currentElement := ""
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		switch element := token.(type) {
		case xml.StartElement:
			currentElement = element.Name.Local
			switch element.Name.Local {
			case "dict", "array":
				depth++
			}
		case xml.EndElement:
			currentElement = ""
			switch element.Name.Local {
			case "dict", "array":
				depth--
			}
		case xml.CharData:
			if depth != 1 {
				continue
			}
			switch currentElement {
			case "key":
				pendingKey = strings.TrimSpace(string(element))
			case "string":
				if pendingKey == wanted {
					return strings.TrimSpace(string(element)), true, nil
				}
				pendingKey = ""
			}
		}
	}
}

// reloadLaunchdService applies the patched plist. launchctl kill reuses the
// cached job definition, so new EnvironmentVariables need a full
// bootout/bootstrap. bootout honours ExitTimeOut, and a supervisor drains
// in-flight streams with its public listener already closed, so waiting for a
// clean exit can mean minutes of downtime on a busy router. Escalate to SIGKILL
// after a short grace instead: the streams die either way, and this bounds the
// outage to seconds.
func reloadLaunchdService(config launchdConfig, runner commandRunner, out io.Writer) error {
	service := "system/" + config.Label
	bootout := exec.Command("launchctl", "bootout", service)
	if err := bootout.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- bootout.Wait() }()
	select {
	case <-done:
	case <-time.After(launchdBootoutGrace):
		fmt.Fprintf(out, "still draining after %s; stopping %s\n", launchdBootoutGrace, config.Label)
		_ = runner.Run("launchctl", "kill", "KILL", service)
		<-done
	}
	if err := runner.Run("launchctl", "bootstrap", "system", config.plistPath()); err != nil {
		return fmt.Errorf("bootstrap %s: %w", config.Label, err)
	}
	if err := waitForLaunchdHealth(config.HealthURL); err != nil {
		return err
	}
	fmt.Fprintf(out, "Reloaded %s\n", config.Label)
	return nil
}

const (
	launchdBootoutGrace = 10 * time.Second
	launchdHealthWait   = 60 * time.Second
)

func waitForLaunchdHealth(healthURL string) error {
	if strings.TrimSpace(healthURL) == "" {
		return nil
	}
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(launchdHealthWait)
	var lastErr error
	for time.Now().Before(deadline) {
		res, err := client.Get(healthURL)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode >= 200 && res.StatusCode < 300 {
				return nil
			}
			lastErr = fmt.Errorf("health returned %s", res.Status)
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("service did not answer %s within %s: %w", healthURL, launchdHealthWait, lastErr)
}

// writeCredentialFile writes a token 0600 and hands it to the service user, so
// the daemon can read it and other local accounts cannot. The file is replaced
// atomically: a half-written credential would fail closed on the next start.
func writeCredentialFile(path, token, owner string, runner commandRunner) error {
	temporary := path + ".new"
	if err := os.WriteFile(temporary, []byte(strings.TrimSpace(token)+"\n"), 0o600); err != nil {
		return err
	}
	if strings.TrimSpace(owner) != "" {
		if err := runner.Run("chown", owner, temporary); err != nil {
			_ = os.Remove(temporary)
			return err
		}
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

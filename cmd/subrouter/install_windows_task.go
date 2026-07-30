package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// Windows persistence uses a per-user Scheduled Task rather than a Windows
// Service. A service runs before and independently of login and needs
// administrator rights to install, which is wrong for a daemon whose whole
// purpose is holding one user's credentials on 127.0.0.1. A Startup-folder or
// Run-key entry needs no elevation either but cannot restart after a crash and
// has no queryable state. A logon-triggered task needs no administrator, starts
// at sign-in, restarts on failure, and answers /Query, which is what `sr server
// status` reports.
const (
	// defaultWindowsTaskName is the task path shown in Task Scheduler.
	defaultWindowsTaskName = `\Subrouter\Daemon`
	// windowsRestartCount bounds automatic restarts so a permanently broken
	// binary stops flapping instead of looping forever.
	windowsRestartCount = 3
	// windowsRestartInterval is the Task Scheduler duration between restarts.
	windowsRestartInterval = "PT1M"
)

// taskRunner runs an external command. commandRunner satisfies it; tests
// substitute a recorder so Windows behaviour is verifiable on any OS.
type taskRunner interface {
	Run(name string, args ...string) error
}

// windowsPaths holds the per-user locations the daemon installs into. Everything
// lives under LOCALAPPDATA: it is per-user, not roaming, and never synced to a
// domain profile, which matters for credential material.
type windowsPaths struct {
	Root       string
	BinDir     string
	Exe        string
	LogDir     string
	StateDir   string
	ConfigDir  string
	TaskXMLDir string
}

func newWindowsPaths(localAppData string) windowsPaths {
	root := filepath.Join(localAppData, "Subrouter")
	return windowsPaths{
		Root:       root,
		BinDir:     filepath.Join(root, "bin"),
		Exe:        filepath.Join(root, "bin", "subrouter.exe"),
		LogDir:     filepath.Join(root, "logs"),
		StateDir:   filepath.Join(root, "state"),
		ConfigDir:  filepath.Join(root, "config"),
		TaskXMLDir: filepath.Join(root, "tasks"),
	}
}

// windowsTaskConfig describes the task to register.
type windowsTaskConfig struct {
	TaskName         string
	Author           string
	Exe              string
	Addr             string
	SRSwitchInterval string
	WorkingDirectory string
}

// windowsTaskArguments builds the daemon's argument string. The listen address
// stays loopback-only: this daemon holds credentials and must never be
// reachable from the network.
func windowsTaskArguments(config windowsTaskConfig) (string, error) {
	addr := strings.TrimSpace(config.Addr)
	if addr == "" {
		addr = "127.0.0.1:31415"
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") && !strings.HasPrefix(addr, "localhost:") {
		return "", fmt.Errorf("daemon address %q must be loopback-only", addr)
	}
	args := []string{"serve", "--addr", addr}
	if interval := strings.TrimSpace(config.SRSwitchInterval); interval != "" && interval != "0" {
		args = append(args, "--sr-switch-interval", interval)
	}
	return strings.Join(args, " "), nil
}

const windowsTaskTemplate = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Author>{{.Author}}</Author>
    <Description>Subrouter local daemon (loopback only)</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <DisallowStartOnRemoteAppSession>false</DisallowStartOnRemoteAppSession>
    <UseUnifiedSchedulingEngine>true</UseUnifiedSchedulingEngine>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure>
      <Interval>{{.RestartInterval}}</Interval>
      <Count>{{.RestartCount}}</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>{{.Exe}}</Command>
      <Arguments>{{.Arguments}}</Arguments>
      <WorkingDirectory>{{.WorkingDirectory}}</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`

// windowsTaskXML renders the task definition. Task Scheduler XML must be UTF-16
// when imported from a file, which installWindowsTask handles when writing.
func windowsTaskXML(config windowsTaskConfig) (string, error) {
	arguments, err := windowsTaskArguments(config)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(config.Exe) == "" {
		return "", fmt.Errorf("task needs an executable path")
	}
	tmpl, err := template.New("task").Parse(windowsTaskTemplate)
	if err != nil {
		return "", err
	}
	author := config.Author
	if strings.TrimSpace(author) == "" {
		author = "Subrouter"
	}
	var buf bytes.Buffer
	err = tmpl.Execute(&buf, struct {
		Author           string
		Exe              string
		Arguments        string
		WorkingDirectory string
		RestartCount     int
		RestartInterval  string
	}{
		Author:           xmlEscape(author),
		Exe:              xmlEscape(config.Exe),
		Arguments:        xmlEscape(arguments),
		WorkingDirectory: xmlEscape(config.WorkingDirectory),
		RestartCount:     windowsRestartCount,
		RestartInterval:  windowsRestartInterval,
	})
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

func xmlEscape(value string) string {
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(value)); err != nil {
		return value
	}
	return buf.String()
}

// windowsController manages the registered task. It shells out to schtasks so
// the task Task Scheduler shows and the task `sr` manages are the same object.
type windowsController struct {
	name     string
	xmlPath  string
	runner   taskRunner
	queryErr func(name string) error
}

func (c windowsController) describe() string { return "scheduled task " + c.name }

func (c windowsController) query() error {
	if c.queryErr != nil {
		return c.queryErr(c.name)
	}
	return c.runner.Run("schtasks", "/Query", "/TN", c.name)
}

func (c windowsController) installed() bool { return c.query() == nil }

func (c windowsController) start() error {
	return c.runner.Run("schtasks", "/Run", "/TN", c.name)
}

// stop ends the running instance. The task itself stays registered so the next
// logon starts it again; `remove` is what uninstalls.
func (c windowsController) stop() error {
	return c.runner.Run("schtasks", "/End", "/TN", c.name)
}

func (c windowsController) restart() error {
	// A failed /End means it was not running, which is not a restart failure.
	_ = c.stop()
	return c.start()
}

func (c windowsController) remove() error {
	_ = c.stop()
	return c.runner.Run("schtasks", "/Delete", "/TN", c.name, "/F")
}

// installWindowsTask writes the task definition and registers it, replacing any
// existing registration so installation is repeatable.
func installWindowsTask(paths windowsPaths, config windowsTaskConfig, runner taskRunner) error {
	if strings.TrimSpace(config.TaskName) == "" {
		config.TaskName = defaultWindowsTaskName
	}
	if strings.TrimSpace(config.Exe) == "" {
		config.Exe = paths.Exe
	}
	if strings.TrimSpace(config.WorkingDirectory) == "" {
		config.WorkingDirectory = paths.Root
	}
	definition, err := windowsTaskXML(config)
	if err != nil {
		return err
	}
	for _, dir := range []string{paths.BinDir, paths.LogDir, paths.StateDir, paths.ConfigDir, paths.TaskXMLDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	xmlPath := filepath.Join(paths.TaskXMLDir, "daemon.xml")
	if err := os.WriteFile(xmlPath, encodeUTF16LEWithBOM(definition), 0o600); err != nil {
		return err
	}
	return runner.Run("schtasks", "/Create", "/TN", config.TaskName, "/XML", xmlPath, "/F")
}

// encodeUTF16LEWithBOM converts the definition for schtasks /XML, which rejects
// UTF-8 input despite the declared encoding.
func encodeUTF16LEWithBOM(value string) []byte {
	out := []byte{0xFF, 0xFE}
	for _, r := range value {
		if r > 0xFFFF {
			r = 0xFFFD
		}
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}

// secureWindowsCredentialDir removes inherited access and grants the current
// user alone full control, the Windows equivalent of the 0700 the POSIX
// installers rely on. Without it a credential directory inherits whatever the
// parent allows.
func secureWindowsCredentialDir(dir, user string, runner taskRunner) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("credential directory is required")
	}
	if strings.TrimSpace(user) == "" {
		return fmt.Errorf("cannot lock %s without a user name", dir)
	}
	if err := runner.Run("icacls", dir, "/inheritance:r"); err != nil {
		return fmt.Errorf("remove inherited access on %s: %w", dir, err)
	}
	if err := runner.Run("icacls", dir, "/grant:r", user+":(OI)(CI)F"); err != nil {
		return fmt.Errorf("grant %s sole access to %s: %w", user, dir, err)
	}
	return nil
}

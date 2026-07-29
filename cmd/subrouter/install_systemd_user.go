package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

func userSystemdUnitPath(home, service string) string {
	return filepath.Join(home, ".config", "systemd", "user", service+".service")
}

func installUserSystemd(home string, runner commandRunner) error {
	systemConfig := systemdConfig{ServiceName: defaultSystemdServiceName}
	if err := refuseSystemSystemdConflict(
		systemdUnitPath(systemConfig),
		systemdSocketPath(systemConfig),
	); err != nil {
		return err
	}
	binDir := filepath.Join(home, ".local", "bin")
	installPath := filepath.Join(binDir, "subrouter")
	configDir := filepath.Join(home, ".config", "subrouter")
	stateDir := filepath.Join(home, ".subrouter")
	unitPath := userSystemdUnitPath(home, defaultSystemdServiceName)
	for _, dir := range []string{
		binDir,
		configDir,
		stateDir,
		filepath.Dir(unitPath),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if err := installCurrentExecutable(installPath); err != nil {
		return err
	}
	for _, alias := range []string{"sr", "cx"} {
		if err := installBinaryAlias(
			installPath,
			filepath.Join(binDir, alias),
		); err != nil {
			return err
		}
	}

	unit, err := renderUserSystemdUnit(home, installPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(unitPath, []byte(unit), 0o600); err != nil {
		return err
	}
	if err := runner.Run("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := runner.Run(
		"systemctl",
		"--user",
		"enable",
		"--now",
		defaultSystemdServiceName,
	); err != nil {
		return err
	}
	fmt.Printf("Installed %s\n", installPath)
	fmt.Printf("Installed %s\n", unitPath)
	fmt.Printf("Started %s.service (user)\n", defaultSystemdServiceName)
	return nil
}

func renderUserSystemdUnit(home, installPath string) (string, error) {
	configPath, err := daemonCloudConfigPath(home)
	if err != nil {
		return "", err
	}
	data := struct {
		Home        string
		InstallPath string
		ConfigPath  string
		StateDir    string
	}{
		Home:        home,
		InstallPath: installPath,
		ConfigPath:  configPath,
		StateDir:    filepath.Join(home, ".subrouter"),
	}
	var out bytes.Buffer
	if err := userSystemdTemplate.Execute(&out, data); err != nil {
		return "", err
	}
	return out.String(), nil
}

func refuseSystemSystemdConflict(unitPath, socketPath string) error {
	var found []string
	for _, path := range []string{unitPath, socketPath} {
		if _, err := os.Stat(path); err == nil {
			found = append(found, path)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect existing system service %s: %w", path, err)
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf(
		"existing system-wide Subrouter service conflicts with the per-user daemon (%s); run 'sudo systemctl disable --now subrouter.service subrouter.socket', remove or migrate the old units, then rerun 'sr setup'",
		strings.Join(found, ", "),
	)
}

var userSystemdTemplate = template.Must(template.New("systemd-user").Parse(`[Unit]
Description=Subrouter local credential proxy
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=simple
Environment=HOME={{.Home}}
ExecStart={{.InstallPath}} serve --addr 127.0.0.1:31415 --cloud-config {{.ConfigPath}} --sr-switch-interval 0
Restart=on-failure
RestartSec=3
TimeoutStopSec=10min
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths={{.StateDir}} {{.Home}}/.config/subrouter
BindReadOnlyPaths={{.ConfigPath}}
RestrictSUIDSGID=true

[Install]
WantedBy=default.target
`))

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

func userSystemdUnitPath(home, service string) string {
	return filepath.Join(home, ".config", "systemd", "user", service+".service")
}

func installUserSystemd(home string, runner commandRunner) error {
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
	data := struct {
		Home        string
		InstallPath string
		ConfigPath  string
		StateDir    string
	}{
		Home:        home,
		InstallPath: installPath,
		ConfigPath:  filepath.Join(home, ".config", "subrouter", "cloud.json"),
		StateDir:    filepath.Join(home, ".subrouter"),
	}
	var out bytes.Buffer
	if err := userSystemdTemplate.Execute(&out, data); err != nil {
		return "", err
	}
	return out.String(), nil
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
RestrictSUIDSGID=true

[Install]
WantedBy=default.target
`))

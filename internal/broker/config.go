package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const DefaultBaseURL = "https://cmux.com"

type CredentialSource string

const (
	CredentialSourceTeam   CredentialSource = "team"
	CredentialSourceLocal  CredentialSource = "local"
	CredentialSourceLegacy CredentialSource = "legacy"
)

type Config struct {
	Version          int              `json:"version"`
	BaseURL          string           `json:"baseUrl"`
	AccessToken      string           `json:"accessToken"`
	RefreshToken     string           `json:"refreshToken"`
	TeamID           string           `json:"teamId,omitempty"`
	TeamName         string           `json:"teamName,omitempty"`
	CredentialSource CredentialSource `json:"credentialSource,omitempty"`
}

func (c Config) LoggedIn() bool {
	return strings.TrimSpace(c.AccessToken) != "" &&
		strings.TrimSpace(c.RefreshToken) != ""
}

func (c Config) Ready() bool {
	return c.LoggedIn() && strings.TrimSpace(c.TeamID) != ""
}

// EffectiveCredentialSource keeps existing installations compatible. A
// pre-source config with a complete cmux.com login was the old signal for team
// mode; a machine without that config keeps its legacy server behavior.
func (c Config) EffectiveCredentialSource() CredentialSource {
	switch c.CredentialSource {
	case CredentialSourceTeam, CredentialSourceLocal, CredentialSourceLegacy:
		return c.CredentialSource
	case "":
		if c.Ready() {
			return CredentialSourceTeam
		}
		return CredentialSourceLegacy
	default:
		return c.CredentialSource
	}
}

func (c Config) TeamModeReady() bool {
	return c.EffectiveCredentialSource() == CredentialSourceTeam && c.Ready()
}

func (c Config) UsesLocalCredentials() bool {
	return c.EffectiveCredentialSource() == CredentialSourceLocal
}

func (c Config) UsesLegacyServer() bool {
	return c.EffectiveCredentialSource() == CredentialSourceLegacy
}

func (c Config) Normalized() Config {
	out := c
	if out.Version == 0 {
		out.Version = 1
	}
	out.BaseURL = strings.TrimRight(strings.TrimSpace(out.BaseURL), "/")
	if out.BaseURL == "" {
		out.BaseURL = DefaultBaseURL
	}
	out.AccessToken = strings.TrimSpace(out.AccessToken)
	out.RefreshToken = strings.TrimSpace(out.RefreshToken)
	out.TeamID = strings.TrimSpace(out.TeamID)
	out.TeamName = strings.TrimSpace(out.TeamName)
	out.CredentialSource = CredentialSource(
		strings.ToLower(strings.TrimSpace(string(out.CredentialSource))),
	)
	return out
}

func (c Config) Validate() error {
	baseURL, err := url.Parse(c.Normalized().BaseURL)
	if err != nil {
		return fmt.Errorf("invalid cmux.com base URL: %w", err)
	}
	if baseURL.Host == "" || baseURL.User != nil ||
		(baseURL.Path != "" && baseURL.Path != "/") ||
		baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return errors.New("cmux.com base URL must be an origin without credentials, path, query, or fragment")
	}
	host := strings.ToLower(baseURL.Hostname())
	loopback := host == "localhost"
	if ip := net.ParseIP(host); ip != nil {
		loopback = ip.IsLoopback()
	}
	if baseURL.Scheme != "https" &&
		!(baseURL.Scheme == "http" && loopback) {
		return errors.New("cmux.com base URL must use HTTPS, except for a loopback development server")
	}
	switch c.Normalized().CredentialSource {
	case "", CredentialSourceTeam, CredentialSourceLocal, CredentialSourceLegacy:
	default:
		return fmt.Errorf(
			"credential source must be %q, %q, or %q",
			CredentialSourceTeam,
			CredentialSourceLocal,
			CredentialSourceLegacy,
		)
	}
	return nil
}

func DefaultConfigPath() (string, error) {
	if path := strings.TrimSpace(os.Getenv("SUBROUTER_CLOUD_CONFIG")); path != "" {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(home) == "" {
		return "", errors.New("home directory is empty")
	}
	return filepath.Join(home, ".config", "subrouter", "cloud.json"), nil
}

func LoadConfig(path string) (Config, error) {
	if strings.TrimSpace(path) == "" {
		var err error
		path, err = DefaultConfigPath()
		if err != nil {
			return Config{}, err
		}
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{Version: 1, BaseURL: DefaultBaseURL}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var config Config
	if err := json.Unmarshal(body, &config); err != nil {
		return Config{}, err
	}
	config = config.Normalized()
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func SaveConfig(path string, config Config) error {
	if strings.TrimSpace(path) == "" {
		var err error
		path, err = DefaultConfigPath()
		if err != nil {
			return err
		}
	}
	config = config.Normalized()
	if err := config.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	file, err := os.CreateTemp(filepath.Dir(path), ".cloud-*.tmp")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func DeleteConfig(path string) error {
	if strings.TrimSpace(path) == "" {
		var err error
		path, err = DefaultConfigPath()
		if err != nil {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

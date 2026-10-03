package wake

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct{ path string }

type Policy struct {
	AllowContinue   bool `json:"allow_continue"`
	MaxGoalAttempts int  `json:"max_goal_attempts"`
	ContinueAfter   int  `json:"continue_after"`
	CooldownSeconds int  `json:"cooldown_seconds"`
}

type configFile struct {
	Version         int               `json:"version"`
	Enabled         map[string]bool   `json:"enabled"`
	EarlyOnRecovery map[string]bool   `json:"early_on_recovery"`
	Policies        map[string]Policy `json:"policies"`
}

func NewConfig(path string) *Config { return &Config{path: path} }

func defaultConfig() configFile {
	return configFile{Version: 1, Enabled: map[string]bool{"codex": false, "claude": false}, EarlyOnRecovery: map[string]bool{"codex": true, "claude": true}, Policies: map[string]Policy{"codex": {MaxGoalAttempts: 2, ContinueAfter: 2, CooldownSeconds: 60}}}
}

func (c *Config) load() (configFile, error) {
	state := defaultConfig()
	body, err := os.ReadFile(c.path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return configFile{}, err
	}
	if len(body) != 0 {
		if err := json.Unmarshal(body, &state); err != nil {
			return configFile{}, fmt.Errorf("decode wake config: %w", err)
		}
	}
	if state.Enabled == nil {
		state.Enabled = map[string]bool{}
	}
	if state.Policies == nil {
		state.Policies = map[string]Policy{}
	}
	if state.EarlyOnRecovery == nil {
		state.EarlyOnRecovery = map[string]bool{"codex": true, "claude": true}
	}
	return state, nil
}

func (c *Config) EarlyOnRecovery(agent string) (bool, error) {
	if agent != "codex" && agent != "claude" {
		return false, fmt.Errorf("wake agent must be codex or claude")
	}
	state, err := c.load()
	if err != nil {
		return false, err
	}
	return state.EarlyOnRecovery[agent], nil
}

func (c *Config) SetEarlyOnRecovery(agent string, enabled bool) error {
	if agent != "codex" && agent != "claude" {
		return fmt.Errorf("wake agent must be codex or claude")
	}
	state, err := c.load()
	if err != nil {
		return err
	}
	state.EarlyOnRecovery[agent] = enabled
	return c.save(state)
}

func (c *Config) SetEnabled(agent string, enabled bool) error {
	if agent != "codex" && agent != "claude" {
		return fmt.Errorf("wake agent must be codex or claude")
	}
	state, err := c.load()
	if err != nil {
		return err
	}
	state.Enabled[agent] = enabled
	return c.save(state)
}

func (c *Config) Enabled(agent string) (bool, error) {
	state, err := c.load()
	if err != nil {
		return false, err
	}
	return state.Enabled[agent], nil
}

func (c *Config) Policy(agent string) (Policy, error) {
	if agent != "codex" && agent != "claude" {
		return Policy{}, fmt.Errorf("wake agent must be codex or claude")
	}
	state, err := c.load()
	if err != nil {
		return Policy{}, err
	}
	p := state.Policies[agent]
	if p.MaxGoalAttempts <= 0 {
		p.MaxGoalAttempts = 2
	}
	if p.ContinueAfter <= 0 {
		p.ContinueAfter = p.MaxGoalAttempts
	}
	if p.CooldownSeconds <= 0 {
		p.CooldownSeconds = 60
	}
	return p, nil
}

func (c *Config) SetPolicy(agent string, policy Policy) error {
	if agent != "codex" && agent != "claude" {
		return fmt.Errorf("wake agent must be codex or claude")
	}
	if policy.MaxGoalAttempts <= 0 || policy.ContinueAfter <= 0 || policy.CooldownSeconds <= 0 {
		return fmt.Errorf("policy attempt counts and cooldown must be positive")
	}
	state, err := c.load()
	if err != nil {
		return err
	}
	state.Policies[agent] = policy
	return c.save(state)
}

func (c *Config) save(state configFile) error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".wake-config-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, c.path)
}

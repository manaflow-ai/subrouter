package claude

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/storepath"
)

const (
	usageURL        = "https://api.anthropic.com/api/oauth/usage"
	oauthBetaHeader = "oauth-2025-04-20"
)

var reservedNames = map[string]bool{
	"add": true, "login": true, "list": true, "ls": true, "switch": true,
	"use": true, "remove": true, "rm": true, "env": true, "status": true,
	"run": true, "help": true,
}

type Store struct {
	Dir string
}

type Profile struct {
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt"`
	LastUsed  string `json:"lastUsed,omitempty"`
	Dir       string `json:"dir"`
}

type profilesFile struct {
	Active   string             `json:"active,omitempty"`
	Profiles map[string]Profile `json:"profiles"`
}

type AuthStatus struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod,omitempty"`
	APIProvider      string `json:"apiProvider,omitempty"`
	Email            string `json:"email,omitempty"`
	OrgID            string `json:"orgId,omitempty"`
	OrgName          string `json:"orgName,omitempty"`
	SubscriptionType string `json:"subscriptionType,omitempty"`
}

type CredentialInfo struct {
	AccessToken      string `json:"accessToken,omitempty"`
	SubscriptionType string `json:"subscriptionType,omitempty"`
	RateLimitTier    string `json:"rateLimitTier,omitempty"`
	ExpiresAt        int64  `json:"expiresAt,omitempty"`
}

type RateLimit struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type ExtraUsage struct {
	IsEnabled    bool     `json:"is_enabled"`
	MonthlyLimit *float64 `json:"monthly_limit"`
	UsedCredits  *float64 `json:"used_credits"`
	Utilization  *float64 `json:"utilization"`
}

type UsageResponse struct {
	FiveHour          *RateLimit  `json:"five_hour"`
	SevenDay          *RateLimit  `json:"seven_day"`
	SevenDayOpus      *RateLimit  `json:"seven_day_opus"`
	SevenDaySonnet    *RateLimit  `json:"seven_day_sonnet"`
	SevenDayOAuthApps *RateLimit  `json:"seven_day_oauth_apps"`
	ExtraUsage        *ExtraUsage `json:"extra_usage"`
}

type ProfileInfo struct {
	Name       string
	Active     bool
	CreatedAt  string
	Auth       *AuthStatus
	Credential *CredentialInfo
	Usage      *UsageResponse
	Error      error
}

func DefaultStore() Store {
	return Store{Dir: storepath.CodexDir()}
}

func (s Store) ProfilesPath() string {
	return filepath.Join(s.Dir, "claude.json")
}

func (s Store) InstancesDir() string {
	return filepath.Join(s.Dir, "claude")
}

func (s Store) InstancePath(name string) string {
	data := s.readProfiles()
	profile, ok := data.Profiles[name]
	dir := sanitizeName(name)
	if ok && profile.Dir != "" {
		dir = profile.Dir
	}
	return filepath.Join(s.InstancesDir(), dir)
}

func (s Store) ClaudeConfigDir(name string) string {
	return s.PreferredInstancePath(s.InstancePath(name))
}

func (s Store) PreferredInstancePath(instancePath string) string {
	cleanDir := filepath.Clean(s.Dir)
	cleanInstance := filepath.Clean(instancePath)
	if filepath.Base(cleanDir) != "codex" || filepath.Base(filepath.Dir(cleanDir)) != ".subrouter" {
		return cleanInstance
	}
	rel, err := filepath.Rel(cleanDir, cleanInstance)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
		return cleanInstance
	}
	home := filepath.Dir(filepath.Dir(cleanDir))
	candidate := filepath.Join(home, ".codex-accounts", rel)
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	return cleanInstance
}

func (s Store) ListProfiles() []Profile {
	data := s.readProfiles()
	out := make([]Profile, 0, len(data.Profiles))
	for _, profile := range data.Profiles {
		out = append(out, profile)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func (s Store) FindProfile(name string) (Profile, bool) {
	data := s.readProfiles()
	profile, ok := data.Profiles[name]
	return profile, ok
}

func (s Store) MatchProfile(selector string) (Profile, bool, error) {
	if profile, ok := s.FindProfile(selector); ok {
		return profile, true, nil
	}
	lower := strings.ToLower(selector)
	var matches []Profile
	for _, profile := range s.ListProfiles() {
		if strings.Contains(strings.ToLower(profile.Name), lower) {
			matches = append(matches, profile)
		}
	}
	if len(matches) == 0 {
		return Profile{}, false, nil
	}
	if len(matches) > 1 {
		names := make([]string, 0, len(matches))
		for _, match := range matches {
			names = append(names, match.Name)
		}
		return Profile{}, false, fmt.Errorf("multiple profiles match %q: %s", selector, strings.Join(names, ", "))
	}
	return matches[0], true, nil
}

func (s Store) ActiveProfile() string {
	return s.readProfiles().Active
}

func (s Store) SetActiveProfile(name string) error {
	data := s.readProfiles()
	profile, ok := data.Profiles[name]
	if !ok {
		return fmt.Errorf("profile %q not found", name)
	}
	data.Active = name
	profile.LastUsed = time.Now().UTC().Format(time.RFC3339)
	data.Profiles[name] = profile
	return s.writeProfiles(data)
}

func (s Store) CreateProfile(name string) (string, error) {
	if err := ValidateProfileName(name); err != nil {
		return "", err
	}
	if _, ok := s.FindProfile(name); ok {
		return "", fmt.Errorf("profile %q already exists", name)
	}
	dir := sanitizeName(name)
	instancePath := filepath.Join(s.InstancesDir(), dir)
	if err := s.initInstanceDir(instancePath); err != nil {
		return "", err
	}
	data := s.readProfiles()
	data.Profiles[name] = Profile{
		Name:      name,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Dir:       dir,
	}
	if data.Active == "" {
		data.Active = name
	}
	return instancePath, s.writeProfiles(data)
}

func (s Store) CreateTempInstance() (string, string, error) {
	dir := fmt.Sprintf("_p%d", time.Now().UnixMilli())
	instancePath := filepath.Join(s.InstancesDir(), dir)
	return instancePath, dir, s.initInstanceDir(instancePath)
}

func (s Store) RegisterProfile(name, dir string) error {
	if err := ValidateProfileNameAllowEmail(name); err != nil {
		return err
	}
	data := s.readProfiles()
	profile, ok := data.Profiles[name]
	if ok {
		profile.LastUsed = time.Now().UTC().Format(time.RFC3339)
		profile.Dir = dir
	} else {
		profile = Profile{
			Name:      name,
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
			Dir:       dir,
		}
	}
	data.Profiles[name] = profile
	if data.Active == "" {
		data.Active = name
	}
	return s.writeProfiles(data)
}

func (s Store) RemoveProfile(name string) (bool, error) {
	data := s.readProfiles()
	profile, ok := data.Profiles[name]
	if !ok {
		return false, nil
	}
	delete(data.Profiles, name)
	if data.Active == name {
		data.Active = ""
		for remaining := range data.Profiles {
			data.Active = remaining
			break
		}
	}
	if err := s.writeProfiles(data); err != nil {
		return false, err
	}
	dir := profile.Dir
	if dir == "" {
		dir = sanitizeName(name)
	}
	if err := os.RemoveAll(filepath.Join(s.InstancesDir(), dir)); err != nil {
		return false, err
	}
	return true, nil
}

func (s Store) CleanupInstance(dir string) error {
	if dir == "" {
		return nil
	}
	return os.RemoveAll(filepath.Join(s.InstancesDir(), dir))
}

func (s Store) readProfiles() profilesFile {
	body, err := os.ReadFile(s.ProfilesPath())
	if err != nil {
		return profilesFile{Profiles: map[string]Profile{}}
	}
	var data profilesFile
	if err := json.Unmarshal(body, &data); err != nil {
		return profilesFile{Profiles: map[string]Profile{}}
	}
	if data.Profiles == nil {
		data.Profiles = map[string]Profile{}
	}
	return data
}

func (s Store) writeProfiles(data profilesFile) error {
	if data.Profiles == nil {
		data.Profiles = map[string]Profile{}
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(s.ProfilesPath(), body, 0o600)
}

func (s Store) initInstanceDir(instancePath string) error {
	if err := os.MkdirAll(instancePath, 0o700); err != nil {
		return err
	}
	for _, name := range []string{"session-env", "todos", "logs", "file-history", "shell-snapshots", "debug", ".anthropic"} {
		if err := os.MkdirAll(filepath.Join(instancePath, name), 0o700); err != nil {
			return err
		}
	}
	return s.syncMCPServers(instancePath)
}

func (s Store) syncMCPServers(instancePath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	body, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return nil
	}
	var global map[string]any
	if err := json.Unmarshal(body, &global); err != nil {
		return nil
	}
	mcp, ok := global["mcpServers"].(map[string]any)
	if !ok || len(mcp) == 0 {
		return nil
	}

	instanceJSON := filepath.Join(instancePath, ".claude.json")
	content := map[string]any{}
	if body, err := os.ReadFile(instanceJSON); err == nil {
		_ = json.Unmarshal(body, &content)
	}
	existing, _ := content["mcpServers"].(map[string]any)
	merged := map[string]any{}
	for key, value := range mcp {
		merged[key] = value
	}
	for key, value := range existing {
		merged[key] = value
	}
	content["mcpServers"] = merged
	out, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		return nil
	}
	out = append(out, '\n')
	return os.WriteFile(instanceJSON, out, 0o600)
}

func ValidateProfileName(name string) error {
	if name == "" {
		return errors.New("profile name is required")
	}
	if reservedNames[strings.ToLower(name)] {
		return fmt.Errorf("%q is a reserved command name", name)
	}
	if !validSimpleName(name) {
		return errors.New("invalid name. Use letters, numbers, dash, underscore. Must start with a letter")
	}
	return nil
}

func ValidateProfileNameAllowEmail(name string) error {
	if strings.Contains(name, "@") {
		if name[0] == '@' || strings.HasSuffix(name, "@") {
			return errors.New("invalid email profile name")
		}
		return nil
	}
	return ValidateProfileName(name)
}

func validSimpleName(name string) bool {
	if name == "" || !isASCIIAlpha(name[0]) {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !isASCIIAlpha(c) && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func isASCIIAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.ToLower(b.String())
}

func DetectCLI() (string, bool) {
	path, err := exec.LookPath("claude")
	return path, err == nil && path != ""
}

func AuthStatusForPath(ctx context.Context, claudePath, instancePath string) (*AuthStatus, error) {
	if claudePath == "" {
		return nil, nil
	}
	if _, err := os.Stat(instancePath); err != nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, claudePath, "auth", "status")
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+instancePath)
	body, err := cmd.Output()
	if err != nil {
		return nil, nil
	}
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return nil, nil
	}
	var status AuthStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, nil
	}
	return &status, nil
}

func (s Store) ReadCredential(ctx context.Context, instancePath string) (*CredentialInfo, error) {
	if credential, ok := readCredentialFile(instancePath); ok {
		return credential, nil
	}
	if runtime.GOOS != "darwin" {
		return nil, nil
	}
	u, err := user.Current()
	if err != nil {
		return nil, nil
	}
	service := "Claude Code-credentials-" + keychainHash(instancePath)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "security", "find-generic-password", "-s", service, "-a", u.Username, "-w")
	body, err := cmd.Output()
	if err != nil || len(strings.TrimSpace(string(body))) == 0 {
		return nil, nil
	}
	return parseCredentialPayload(body)
}

func readCredentialFile(instancePath string) (*CredentialInfo, bool) {
	body, err := os.ReadFile(filepath.Join(instancePath, ".credentials.json"))
	if err != nil {
		return nil, false
	}
	credential, err := parseCredentialPayload(body)
	return credential, err == nil && credential != nil
}

func parseCredentialPayload(body []byte) (*CredentialInfo, error) {
	var raw struct {
		ClaudeAIOAuth *CredentialInfo `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	return raw.ClaudeAIOAuth, nil
}

func keychainHash(instancePath string) string {
	sum := sha256.Sum256([]byte(instancePath))
	return hex.EncodeToString(sum[:])[:8]
}

func FetchUsage(ctx context.Context, client *http.Client, accessToken string) (*UsageResponse, error) {
	if accessToken == "" {
		return nil, nil
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", oauthBetaHeader)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, nil
	}
	var usage UsageResponse
	if err := json.NewDecoder(res.Body).Decode(&usage); err != nil {
		return nil, err
	}
	return &usage, nil
}

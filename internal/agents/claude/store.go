package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

const (
	usageURL        = "https://api.anthropic.com/api/oauth/usage"
	oauthBetaHeader = "oauth-2025-04-20"
)

// messagesURL is a var so probe tests can point it at a local server.
var messagesURL = "https://api.anthropic.com/v1/messages"

const (
	FableModel      = "claude-fable-5"
	FableWindowName = "oauth-apps-weekly"
	// Quota-pool family keys stamped on Claude usage windows and matched
	// against canonicalized request models ("claude-opus-4-8[1m]" and
	// "claude-opus-4-8" both belong to the opus pool).
	FableFeature  = "claude-fable"
	OpusFeature   = "claude-opus"
	SonnetFeature = "claude-sonnet"
)

var oauthTokenURL = "https://platform.claude.com/v1/oauth/token"

const oauthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

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
	RefreshToken     string `json:"refreshToken,omitempty"`
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

func (s Store) ListAccounts(ctx context.Context) ([]accounts.Account, error) {
	profiles := s.ListProfiles()
	out := make([]accounts.Account, 0, len(profiles))
	for _, profile := range profiles {
		configDir := s.ClaudeConfigDir(profile.Name)
		credential, err := s.ReadCredential(ctx, configDir)
		if err != nil {
			// One profile with a corrupt credential (e.g. a malformed
			// keychain blob) must not drop every other Claude account.
			// Skip it and keep loading the rest.
			slog.Warn("Claude profile credential unreadable, skipping", "profile", profile.Name, "error", err)
			continue
		}
		account, ok := profileAccount(profile, configDir, credential)
		if !ok {
			continue
		}
		out = append(out, account)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out, nil
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
	if s.SharedStoreEnabled() {
		if _, err := s.EnsureSharedLayout(s.PreferredInstancePath(instancePath)); err != nil {
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

func (s Store) RefreshCredentialIfExpired(ctx context.Context, client *http.Client, profile Profile) (accounts.Account, bool, error) {
	configDir := s.ClaudeConfigDir(profile.Name)
	credential, err := s.ReadCredential(ctx, configDir)
	if err != nil {
		return accounts.Account{}, false, err
	}
	if credential == nil || credential.AccessToken == "" {
		return accounts.Account{}, false, fmt.Errorf("Claude profile %q has no access token", profile.Name)
	}
	didRefresh := false
	if credential.RefreshToken != "" && credentialExpired(credential, 60*time.Second) {
		refreshed, err := RefreshCredential(ctx, client, *credential)
		if err != nil {
			return accounts.Account{}, false, err
		}
		credential = &refreshed
		if err := s.WriteCredential(ctx, configDir, *credential); err != nil {
			return accounts.Account{}, false, err
		}
		didRefresh = true
	}
	account, ok := profileAccount(profile, configDir, credential)
	if !ok {
		return accounts.Account{}, false, fmt.Errorf("Claude profile %q has no usable credential", profile.Name)
	}
	return account, didRefresh, nil
}

func (s Store) RefreshAccountIfExpired(ctx context.Context, client *http.Client, account accounts.Account) (accounts.Account, bool, error) {
	profile, ok := s.FindProfile(account.ID)
	if !ok {
		return account, false, fmt.Errorf("Claude profile %q not found", account.ID)
	}
	return s.RefreshCredentialIfExpired(ctx, client, profile)
}

func (s Store) WriteCredential(ctx context.Context, instancePath string, credential CredentialInfo) error {
	body, err := credentialPayload(credential)
	if err != nil {
		return err
	}
	filePath := filepath.Join(instancePath, ".credentials.json")
	if _, err := os.Stat(filePath); err == nil {
		return os.WriteFile(filePath, body, 0o600)
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("Claude credential persistence is only supported for .credentials.json files on %s", runtime.GOOS)
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	service := "Claude Code-credentials-" + keychainHash(instancePath)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "security", "add-generic-password", "-U", "-s", service, "-a", u.Username, "-w", string(body))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("write Claude credential to keychain: %w", err)
	}
	return nil
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

func credentialPayload(credential CredentialInfo) ([]byte, error) {
	body, err := json.MarshalIndent(map[string]CredentialInfo{
		"claudeAiOauth": credential,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	return body, nil
}

func credentialExpired(credential *CredentialInfo, skew time.Duration) bool {
	if credential == nil || credential.ExpiresAt <= 0 {
		return false
	}
	expiresAt := time.UnixMilli(credential.ExpiresAt)
	return !time.Now().Add(skew).Before(expiresAt)
}

func RefreshCredential(ctx context.Context, client *http.Client, credential CredentialInfo) (CredentialInfo, error) {
	if credential.RefreshToken == "" {
		return credential, nil
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	payload := map[string]string{
		"client_id":     oauthClientID,
		"grant_type":    "refresh_token",
		"refresh_token": credential.RefreshToken,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return credential, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, bytes.NewReader(body))
	if err != nil {
		return credential, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", oauthBetaHeader)
	res, err := client.Do(req)
	if err != nil {
		return credential, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(res.Body)
		return credential, fmt.Errorf("Claude OAuth refresh failed: %s: %s", res.Status, strings.TrimSpace(buf.String()))
	}
	var refreshed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(res.Body).Decode(&refreshed); err != nil {
		return credential, err
	}
	if refreshed.AccessToken == "" {
		return credential, fmt.Errorf("Claude OAuth refresh response missing access_token")
	}
	credential.AccessToken = refreshed.AccessToken
	if refreshed.RefreshToken != "" {
		credential.RefreshToken = refreshed.RefreshToken
	}
	if refreshed.ExpiresIn > 0 {
		credential.ExpiresAt = time.Now().Add(time.Duration(refreshed.ExpiresIn) * time.Second).UnixMilli()
	}
	return credential, nil
}

func profileAccount(profile Profile, configDir string, credential *CredentialInfo) (accounts.Account, bool) {
	if credential == nil || credential.AccessToken == "" {
		return accounts.Account{}, false
	}
	addedAt, _ := time.Parse(time.RFC3339, profile.CreatedAt)
	email := ""
	if strings.Contains(profile.Name, "@") {
		email = profile.Name
	}
	return accounts.Account{
		ID:       profile.Name,
		Provider: accounts.ProviderClaude,
		AuthMode: accounts.AuthModeOAuth,
		Label:    profile.Name,
		Email:    email,
		AddedAt:  addedAt,
		Token:    credential.AccessToken,
		Source:   configDir,
	}, true
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
		return nil, fmt.Errorf("usage fetch failed: %s", res.Status)
	}
	var usage UsageResponse
	if err := json.NewDecoder(res.Body).Decode(&usage); err != nil {
		return nil, err
	}
	return &usage, nil
}

// FetchFableUsageWindows probes the Messages API because Anthropic's OAuth
// usage endpoint often omits the hidden Fable/OAuth-app weekly bucket. The
// response headers are the authoritative source for that bucket.
func FetchFableUsageWindows(ctx context.Context, client *http.Client, accessToken string) ([]accounts.UsageWindow, error) {
	if accessToken == "" {
		return nil, nil
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	// The probe must look like Claude Code: Anthropic rejects subscription
	// OAuth Messages calls that lack the Claude Code system prompt, beta tag,
	// and client identity with a headerless 429 rate_limit_error regardless of
	// quota (observed live 2026-07-04: a fresh Max 20x account with 0.0
	// utilization 429'd the bare probe but answered 200 with unified headers,
	// including 7d_oi, once the request carried the Claude Code shape).
	body := bytes.NewBufferString(`{"model":"` + FableModel + `","max_tokens":1,"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"messages":[{"role":"user","content":"."}]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, messagesURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", "claude-code-20250219,"+oauthBetaHeader)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.1.199 (external, cli)")
	req.Header.Set("x-app", "cli")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	windows := usageWindowsFromFableHeaders(res.Header, time.Now())
	if len(windows) > 0 {
		return windows, nil
	}
	// A headerless 429 is a transient burst or bot-shape rejection, never an
	// authoritative quota signal (the taxonomy that governs the proxy's own
	// exhaustion marking). Synthesizing 100% here falsely cooked every
	// account's fable pool and pushed all Fable traffic to Bedrock/API key.
	// Return no windows so the caller keeps the last known state.
	if res.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("fable probe failed: %s", res.Status)
	}
	return nil, nil
}

func usageWindowsFromFableHeaders(header http.Header, now time.Time) []accounts.UsageWindow {
	const (
		fiveHourSeconds = int64(5 * 60 * 60)
		sevenDaySeconds = int64(7 * 24 * 60 * 60)
	)
	var windows []accounts.UsageWindow
	add := func(prefix, name string, windowSeconds int64, feature string) {
		status := strings.ToLower(strings.TrimSpace(header.Get("anthropic-ratelimit-unified-" + prefix + "-status")))
		rawUtil := strings.TrimSpace(header.Get("anthropic-ratelimit-unified-" + prefix + "-utilization"))
		rawReset := strings.TrimSpace(header.Get("anthropic-ratelimit-unified-" + prefix + "-reset"))
		if status == "" && rawUtil == "" && rawReset == "" {
			return
		}
		used := 0.0
		if rawUtil != "" {
			if parsed, err := strconv.ParseFloat(rawUtil, 64); err == nil {
				used = parsed * 100
			}
		}
		if status == "rejected" && used < 100 {
			used = 100
		}
		window := accounts.UsageWindow{
			Name:               name,
			UsedPercent:        used,
			LimitWindowSeconds: windowSeconds,
			Feature:            feature,
		}
		if rawReset != "" {
			if epoch, err := strconv.ParseInt(rawReset, 10, 64); err == nil && epoch > 0 {
				seconds := int64(time.Unix(epoch, 0).Sub(now).Seconds())
				if seconds < 0 {
					seconds = 0
				}
				window.ResetAfterSeconds = seconds
			}
		}
		windows = append(windows, window)
	}
	add("5h", "5h", fiveHourSeconds, "")
	add("7d", "7d", sevenDaySeconds, "")
	add("7d_oi", FableWindowName, sevenDaySeconds, FableFeature)
	return windows
}

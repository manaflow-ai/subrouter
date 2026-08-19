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
	"unicode"

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
	// SharedStateDir is the user's ordinary Claude Code state directory.
	// Local credential profiles link high-growth history directories here so
	// Subrouter does not duplicate trajectories once per profile. Server-side
	// stores leave it empty because they never launch Claude Code.
	SharedStateDir string
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
	home, _ := os.UserHomeDir()
	shared := ""
	if home != "" {
		shared = filepath.Join(home, ".claude")
	}
	return Store{Dir: storepath.CodexDir(), SharedStateDir: shared}
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
	if ok && safeProfileDir(profile.Dir) {
		dir = profile.Dir
	}
	return filepath.Join(s.InstancesDir(), dir)
}

func (s Store) ClaudeConfigDir(name string) string {
	path := s.PreferredInstancePath(s.InstancePath(name))
	if err := s.prepareSharedState(path); err != nil {
		slog.Warn("Claude shared history setup failed", "profile", name, "error", err)
	}
	return path
}

func (s Store) PreferredInstancePath(instancePath string) string {
	cleanInstance := filepath.Clean(instancePath)
	candidate, ok := s.legacyInstancePath(cleanInstance)
	if !ok {
		return cleanInstance
	}
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	return cleanInstance
}

func (s Store) legacyInstancePath(instancePath string) (string, bool) {
	cleanDir := filepath.Clean(s.Dir)
	cleanInstance := filepath.Clean(instancePath)
	if filepath.Base(cleanDir) != "codex" || filepath.Base(filepath.Dir(cleanDir)) != ".subrouter" {
		return "", false
	}
	rel, err := filepath.Rel(cleanDir, cleanInstance)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
		return "", false
	}
	home := filepath.Dir(filepath.Dir(cleanDir))
	return filepath.Join(home, ".codex-accounts", rel), true
}

func (s Store) profileInstancePaths(dir string) ([]string, error) {
	if !safeProfileDir(dir) {
		return nil, errors.New("Claude profile directory is invalid")
	}
	canonicalRoot := filepath.Clean(s.InstancesDir())
	roots := []string{canonicalRoot}
	if legacyRoot, ok := s.legacyInstancePath(canonicalRoot); ok {
		roots = append(roots, legacyRoot)
	}
	unique := make(map[string]string, len(roots))
	for _, root := range roots {
		candidate := filepath.Join(root, dir)
		if !profileInstancePathWithinRoot(root, candidate) {
			return nil, errors.New("Claude profile directory escapes its instance root")
		}
		candidate = filepath.Clean(candidate)
		key := candidate
		if _, exists := unique[key]; !exists {
			unique[key] = candidate
		}
	}
	paths := make([]string, 0, len(unique))
	for _, candidate := range unique {
		paths = append(paths, candidate)
	}
	sort.Strings(paths)
	return paths, nil
}

func profileInstancePathWithinRoot(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && safeProfileDir(relative)
}

func profileInstancePathKey(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	if resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		return filepath.Join(resolvedParent, filepath.Base(path))
	}
	return path
}

func lockProfileCredentialPaths(
	ctx context.Context,
	paths []string,
) (locks []*profileCredentialLock, err error) {
	keyedPaths := make(map[string]string, len(paths))
	for _, path := range paths {
		key := profileInstancePathKey(path)
		if _, exists := keyedPaths[key]; !exists {
			keyedPaths[key] = path
		}
	}
	keys := make([]string, 0, len(keyedPaths))
	for key := range keyedPaths {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		path := keyedPaths[key]
		lock, lockErr := lockProfileCredential(ctx, path)
		if lockErr != nil {
			_ = closeProfileCredentialLocks(locks)
			return nil, lockErr
		}
		locks = append(locks, lock)
	}
	return locks, nil
}

func closeProfileCredentialLocks(locks []*profileCredentialLock) error {
	var closeErr error
	for index := len(locks) - 1; index >= 0; index-- {
		closeErr = errors.Join(closeErr, locks[index].Close())
	}
	return closeErr
}

type stagedProfileInstance struct {
	originalPath string
	stagedPath   string
	stagingRoot  string
}

func stageProfileInstancePaths(paths []string) ([]stagedProfileInstance, error) {
	staged := make([]stagedProfileInstance, 0, len(paths))
	for _, path := range paths {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, errors.Join(err, rollbackStagedProfileInstances(staged))
		}
		stagingRoot, err := os.MkdirTemp(
			filepath.Dir(path),
			"."+filepath.Base(path)+".remove-*",
		)
		if err != nil {
			return nil, errors.Join(err, rollbackStagedProfileInstances(staged))
		}
		entry := stagedProfileInstance{
			originalPath: path,
			stagedPath:   filepath.Join(stagingRoot, "instance"),
			stagingRoot:  stagingRoot,
		}
		if err := os.Rename(entry.originalPath, entry.stagedPath); err != nil {
			_ = os.Remove(stagingRoot)
			return nil, errors.Join(err, rollbackStagedProfileInstances(staged))
		}
		staged = append(staged, entry)
	}
	return staged, nil
}

func rollbackStagedProfileInstances(staged []stagedProfileInstance) error {
	var rollbackErr error
	for index := len(staged) - 1; index >= 0; index-- {
		entry := staged[index]
		if err := os.Rename(entry.stagedPath, entry.originalPath); err != nil {
			rollbackErr = errors.Join(rollbackErr, err)
			continue
		}
		rollbackErr = errors.Join(rollbackErr, os.Remove(entry.stagingRoot))
	}
	return rollbackErr
}

func deleteStagedProfileInstances(
	staged []stagedProfileInstance,
) error {
	var cleanupErr error
	for _, entry := range staged {
		cleanupErr = errors.Join(cleanupErr, os.RemoveAll(entry.stagingRoot))
	}
	return cleanupErr
}

type profileCredentialBackup struct {
	path       string
	credential CredentialInfo
}

func (s Store) profileCredentialBackups(
	ctx context.Context,
	instancePaths []string,
) ([]profileCredentialBackup, error) {
	if runtime.GOOS != "darwin" {
		return nil, nil
	}
	backups := make([]profileCredentialBackup, 0, len(instancePaths))
	for _, path := range instancePaths {
		credential, err := s.readCredential(ctx, path)
		if err != nil {
			return nil, err
		}
		if credential != nil {
			backups = append(backups, profileCredentialBackup{
				path:       path,
				credential: *credential,
			})
		}
	}
	return backups, nil
}

func deleteProfileKeychainCredentials(instancePaths []string) error {
	for _, instancePath := range instancePaths {
		if err := deleteKeychainCredential(instancePath); err != nil {
			return err
		}
	}
	return nil
}

func cloneProfilesFile(data profilesFile) profilesFile {
	cloned := profilesFile{
		Active:   data.Active,
		Profiles: make(map[string]Profile, len(data.Profiles)),
	}
	for name, profile := range data.Profiles {
		cloned.Profiles[name] = profile
	}
	return cloned
}

func (s Store) rollbackProfileRemoval(
	ctx context.Context,
	original profilesFile,
	staged []stagedProfileInstance,
	backups []profileCredentialBackup,
) error {
	if err := rollbackStagedProfileInstances(staged); err != nil {
		return err
	}
	for _, backup := range backups {
		if err := s.writeCredential(ctx, backup.path, backup.credential); err != nil {
			return err
		}
	}
	return s.writeProfiles(original)
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
	lock, err := lockProfileRegistry(s.ProfilesPath())
	if err != nil {
		return err
	}
	defer lock.Close()
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
	lock, err := lockProfileRegistry(s.ProfilesPath())
	if err != nil {
		return "", err
	}
	defer lock.Close()
	data := s.readProfiles()
	if _, ok := data.Profiles[name]; ok {
		return "", fmt.Errorf("profile %q already exists", name)
	}
	dir := sanitizeName(name)
	instancePath := s.PreferredInstancePath(filepath.Join(s.InstancesDir(), dir))
	if err := s.initInstanceDir(instancePath); err != nil {
		return "", err
	}
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
	instancePath := s.PreferredInstancePath(filepath.Join(s.InstancesDir(), dir))
	return instancePath, dir, s.initInstanceDir(instancePath)
}

func (s Store) RegisterProfile(name, dir string) error {
	if err := ValidateProfileNameAllowEmail(name); err != nil {
		return err
	}
	if !safeProfileDir(dir) {
		return errors.New("Claude profile directory is invalid")
	}
	lock, err := lockProfileRegistry(s.ProfilesPath())
	if err != nil {
		return err
	}
	defer lock.Close()
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

// ImportProfileCredential installs one server-owned OAuth credential without
// copying a client-side profile directory. The server derives the directory,
// writes the secret atomically, then publishes the profile in the registry.
func (s Store) ImportProfileCredential(name string, credential CredentialInfo) (err error) {
	if err := ValidateProfileNameAllowEmail(name); err != nil {
		return err
	}
	if strings.TrimSpace(credential.AccessToken) == "" || strings.TrimSpace(credential.RefreshToken) == "" {
		return errors.New("Claude OAuth access and refresh tokens are required")
	}
	lock, err := lockProfileRegistry(s.ProfilesPath())
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
	}()
	data := s.readProfiles()
	dir := importedProfileDir(name)
	profile, exists := data.Profiles[name]
	if exists {
		if profile.Dir != "" {
			dir = profile.Dir
		} else {
			// Preserve the legacy implicit directory for registries written before
			// every profile carried an explicit Dir field.
			dir = sanitizeName(name)
		}
	}
	if !safeProfileDir(dir) {
		return errors.New("Claude profile directory is invalid")
	}
	instancePath := s.PreferredInstancePath(filepath.Join(s.InstancesDir(), dir))
	if err := os.MkdirAll(instancePath, 0o700); err != nil {
		return err
	}
	instancePaths, err := s.profileInstancePaths(dir)
	if err != nil {
		return err
	}
	credentialLocks, err := lockProfileCredentialPaths(context.Background(), instancePaths)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := closeProfileCredentialLocks(credentialLocks); err == nil {
			err = closeErr
		}
	}()
	body, err := credentialPayload(credential)
	if err != nil {
		return err
	}
	if err := writePrivateFileAtomic(filepath.Join(instancePath, ".credentials.json"), body); err != nil {
		return err
	}
	for _, path := range instancePaths {
		if err := deleteKeychainCredential(path); err != nil {
			return err
		}
		if profileInstancePathKey(path) == profileInstancePathKey(instancePath) {
			continue
		}
		if err := os.Remove(filepath.Join(path, ".credentials.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if !exists {
		profile = Profile{Name: name, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	}
	profile.Dir = dir
	profile.LastUsed = time.Now().UTC().Format(time.RFC3339)
	data.Profiles[name] = profile
	if data.Active == "" {
		data.Active = name
	}
	return s.writeProfiles(data)
}

func (s Store) RemoveProfile(name string) (removed bool, err error) {
	lock, err := lockProfileRegistry(s.ProfilesPath())
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
	}()
	data := s.readProfiles()
	profile, ok := data.Profiles[name]
	if !ok {
		return false, nil
	}
	dir := profile.Dir
	if dir == "" {
		dir = sanitizeName(name)
	}
	instancePaths, err := s.profileInstancePaths(dir)
	if err != nil {
		return false, err
	}
	original := cloneProfilesFile(data)
	ctx := context.Background()
	credentialLocks, err := lockProfileCredentialPaths(ctx, instancePaths)
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := closeProfileCredentialLocks(credentialLocks); err == nil {
			err = closeErr
		}
	}()
	credentialBackups, err := s.profileCredentialBackups(ctx, instancePaths)
	if err != nil {
		return false, err
	}
	staged, err := stageProfileInstancePaths(instancePaths)
	if err != nil {
		return false, err
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
		return false, errors.Join(err, rollbackStagedProfileInstances(staged))
	}
	if err := deleteProfileKeychainCredentials(instancePaths); err != nil {
		rollbackErr := s.rollbackProfileRemoval(ctx, original, staged, credentialBackups)
		if rollbackErr == nil {
			return false, err
		}
		slog.Error(
			"Claude profile removal cleanup failed and rollback was incomplete; profile remains removed",
			"profile", name,
			"cleanup_error", err,
			"rollback_error", rollbackErr,
		)
		return true, nil
	}
	if err := deleteStagedProfileInstances(staged); err != nil {
		slog.Warn(
			"Claude profile removed with staged credential cleanup pending",
			"profile", name,
			"error", err,
		)
	}
	return true, nil
}

func (s Store) CleanupInstance(dir string) error {
	if dir == "" {
		return nil
	}
	instancePaths, err := s.profileInstancePaths(dir)
	if err != nil {
		return err
	}
	credentialLocks, err := lockProfileCredentialPaths(context.Background(), instancePaths)
	if err != nil {
		return err
	}
	defer closeProfileCredentialLocks(credentialLocks)
	for _, instancePath := range instancePaths {
		if err := deleteKeychainCredential(instancePath); err != nil {
			return err
		}
		if err := os.RemoveAll(instancePath); err != nil {
			return err
		}
	}
	return nil
}

func (s Store) readProfiles() profilesFile {
	body, err := readFileForAtomicReplace(s.ProfilesPath())
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
	return writePrivateFileAtomic(s.ProfilesPath(), body)
}

func safeProfileDir(dir string) bool {
	return dir != "" &&
		dir != "." &&
		!filepath.IsAbs(dir) &&
		filepath.VolumeName(dir) == "" &&
		filepath.Clean(dir) == dir &&
		filepath.Base(dir) == dir
}

func writePrivateFileAtomic(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (s Store) initInstanceDir(instancePath string) error {
	if err := os.MkdirAll(instancePath, 0o700); err != nil {
		return err
	}
	for _, name := range []string{".anthropic"} {
		if err := os.MkdirAll(filepath.Join(instancePath, name), 0o700); err != nil {
			return err
		}
	}
	if err := s.prepareSharedState(instancePath); err != nil {
		return err
	}
	if strings.TrimSpace(s.SharedStateDir) == "" {
		for _, name := range claudeHighGrowthDirs {
			if err := os.MkdirAll(filepath.Join(instancePath, name), 0o700); err != nil {
				return err
			}
		}
	}
	return s.syncMCPServers(instancePath)
}

var claudeHighGrowthDirs = []string{
	"projects",
	"file-history",
	"session-env",
	"todos",
	"logs",
	"shell-snapshots",
	"debug",
}

func (s Store) prepareSharedState(instancePath string) error {
	if strings.TrimSpace(s.SharedStateDir) == "" {
		return nil
	}
	if err := os.MkdirAll(s.SharedStateDir, 0o700); err != nil {
		return err
	}
	for _, name := range claudeHighGrowthDirs {
		source := filepath.Join(instancePath, name)
		target := filepath.Join(s.SharedStateDir, name)
		if err := migrateDirectoryToShared(source, target); err != nil {
			return fmt.Errorf("share %s: %w", name, err)
		}
	}
	return nil
}

func migrateDirectoryToShared(source, target string) error {
	if info, err := os.Lstat(source); err == nil && info.Mode()&os.ModeSymlink != 0 {
		current, readErr := os.Readlink(source)
		if readErr != nil {
			return readErr
		}
		currentPath := current
		if !filepath.IsAbs(currentPath) {
			currentPath = filepath.Join(filepath.Dir(source), currentPath)
		}
		currentAbs, _ := filepath.Abs(currentPath)
		targetAbs, _ := filepath.Abs(target)
		if currentAbs == targetAbs {
			return os.MkdirAll(target, 0o700)
		}
		return fmt.Errorf("existing symlink points to %s", current)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	if info, err := os.Stat(source); err == nil {
		if !info.IsDir() {
			return errors.New("existing profile state is not a directory")
		}
		if err := mergeDirectoryPreservingConflicts(source, target); err != nil {
			return err
		}
		if err := os.RemoveAll(source); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Symlink(target, source)
}

func mergeDirectoryPreservingConflicts(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, rel)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o700)
		}
		if _, err := os.Lstat(destination); errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return err
			}
			return os.Rename(path, destination)
		} else if err != nil {
			return err
		}
		destination = availableLegacyPath(destination)
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		return os.Rename(path, destination)
	})
}

func availableLegacyPath(path string) string {
	for index := 1; ; index++ {
		candidate := fmt.Sprintf("%s.subrouter-legacy-%d", path, index)
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
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
	for _, char := range name {
		if unicode.IsControl(char) || unicode.In(char, unicode.Cf, unicode.Zl, unicode.Zp) {
			return errors.New("profile name contains terminal control characters")
		}
	}
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

func importedProfileDir(name string) string {
	base := strings.Trim(sanitizeName(name), "-")
	if base == "" {
		base = "profile"
	}
	if len(base) > 80 {
		base = base[:80]
	}
	digest := sha256.Sum256([]byte(name))
	return base + "-" + hex.EncodeToString(digest[:8])
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

func (s Store) ReadCredential(ctx context.Context, instancePath string) (credential *CredentialInfo, err error) {
	lock, err := lockProfileCredential(ctx, instancePath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
	}()
	return s.readCredential(ctx, instancePath)
}

func (s Store) readCredential(ctx context.Context, instancePath string) (*CredentialInfo, error) {
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
	return parseCredentialPayload(body, "keychain")
}

func (s Store) RefreshCredentialIfExpired(ctx context.Context, client *http.Client, profile Profile) (accounts.Account, bool, error) {
	return s.refreshProfileCredential(ctx, client, profile, false)
}

func (s Store) ForceRefreshCredential(ctx context.Context, client *http.Client, profile Profile) (accounts.Account, bool, error) {
	return s.refreshProfileCredential(ctx, client, profile, true)
}

// refreshProfileCredential serializes concurrent refreshes of the same
// profile using lockProfileRefresh, a lock distinct from
// lockProfileCredential. Claude refresh tokens are single-use, so two
// concurrent callers racing to refresh the same profile must still be
// serialized across the network call itself — a caller that blocks here will
// observe the winner's already-refreshed, still-fresh credential on its own
// subsequent read and skip the network call entirely (see the shouldRefresh
// check below). Unlike lockProfileCredential, lockProfileRefresh is only ever
// held here, so holding it across the OAuth round-trip never blocks an
// unrelated ImportProfileCredential call for the same profile; each
// individual disk read/write still takes the short-lived
// lockProfileCredential via ReadCredential / writeRefreshedCredentialIfUnchanged.
func (s Store) refreshProfileCredential(ctx context.Context, client *http.Client, profile Profile, force bool) (account accounts.Account, didRefresh bool, err error) {
	configDir := s.ClaudeConfigDir(profile.Name)
	refreshLock, err := lockProfileRefresh(ctx, configDir)
	if err != nil {
		return accounts.Account{}, false, err
	}
	defer func() {
		if closeErr := refreshLock.Close(); err == nil {
			err = closeErr
		}
	}()

	current, ok := s.FindProfile(profile.Name)
	if !ok || profileInstancePathKey(s.ClaudeConfigDir(current.Name)) != profileInstancePathKey(configDir) {
		return accounts.Account{}, false, fmt.Errorf("Claude profile %q is no longer current", profile.Name)
	}
	profile = current
	credential, err := s.ReadCredential(ctx, configDir)
	if err != nil {
		return accounts.Account{}, false, err
	}
	if credential == nil || credential.AccessToken == "" {
		return accounts.Account{}, false, fmt.Errorf("Claude profile %q has no access token", profile.Name)
	}
	if force && credential.RefreshToken == "" {
		return accounts.Account{}, false, fmt.Errorf("Claude profile %q has no refresh token", profile.Name)
	}
	shouldRefresh := credential.RefreshToken != "" &&
		(force || credentialExpired(credential, 60*time.Second))
	if !shouldRefresh {
		account, ok = profileAccount(profile, configDir, credential)
		if !ok {
			return accounts.Account{}, false, fmt.Errorf("Claude profile %q has no usable credential", profile.Name)
		}
		return account, false, nil
	}

	credentialBeforeRefresh := *credential
	profileBeforeRefresh := profile
	refreshed, err := RefreshCredential(ctx, client, credentialBeforeRefresh)
	if err != nil {
		return accounts.Account{}, false, err
	}
	didRefresh = true

	current, ok = s.FindProfile(profile.Name)
	if !ok ||
		current.CreatedAt != profileBeforeRefresh.CreatedAt ||
		profileInstancePathKey(s.ClaudeConfigDir(current.Name)) != profileInstancePathKey(configDir) {
		return accounts.Account{}, false, fmt.Errorf("Claude profile %q is no longer current", profile.Name)
	}
	profile = current
	credential, err = s.writeRefreshedCredentialIfUnchanged(ctx, configDir, credentialBeforeRefresh, refreshed)
	if err != nil {
		return accounts.Account{}, false, err
	}
	if credential == nil || credential.AccessToken == "" {
		return accounts.Account{}, false, fmt.Errorf("Claude profile %q has no access token", profile.Name)
	}
	account, ok = profileAccount(profile, configDir, credential)
	if !ok {
		return accounts.Account{}, false, fmt.Errorf("Claude profile %q has no usable credential", profile.Name)
	}
	return account, didRefresh, nil
}

// writeRefreshedCredentialIfUnchanged briefly holds lockProfileCredential to
// re-read the on-disk credential and compare it against the value read before
// the network refresh. If nothing else wrote to the profile in the meantime,
// the refreshed credential is persisted and returned. Otherwise the newer
// on-disk credential wins and the refreshed value is discarded, so a
// concurrent ImportProfileCredential is never clobbered by a stale refresh.
func (s Store) writeRefreshedCredentialIfUnchanged(ctx context.Context, instancePath string, before, refreshed CredentialInfo) (credential *CredentialInfo, err error) {
	lock, err := lockProfileCredential(ctx, instancePath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
	}()

	current, err := s.readCredential(ctx, instancePath)
	if err != nil {
		return nil, err
	}
	if current == nil || current.AccessToken == "" {
		return current, nil
	}
	if *current != before {
		return current, nil
	}
	if err := s.writeCredential(ctx, instancePath, refreshed); err != nil {
		return nil, err
	}
	return &refreshed, nil
}

func (s Store) RefreshAccountIfExpired(ctx context.Context, client *http.Client, account accounts.Account) (accounts.Account, bool, error) {
	profile, ok := s.FindProfile(account.ID)
	if !ok {
		return account, false, fmt.Errorf("Claude profile %q not found", account.ID)
	}
	return s.RefreshCredentialIfExpired(ctx, client, profile)
}

func (s Store) WriteCredential(ctx context.Context, instancePath string, credential CredentialInfo) (err error) {
	lock, err := lockProfileCredential(ctx, instancePath)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
	}()
	return s.writeCredential(ctx, instancePath, credential)
}

func (s Store) writeCredential(ctx context.Context, instancePath string, credential CredentialInfo) error {
	body, err := credentialPayload(credential)
	if err != nil {
		return err
	}
	filePath := filepath.Join(instancePath, ".credentials.json")
	if _, err := os.Stat(filePath); err == nil {
		return writePrivateFileAtomic(filePath, body)
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

// UpsertCredentialProfile stores one remotely managed Claude credential
// without invoking Claude Code. Server-side tenant pools use file-backed
// credentials so systemd services never depend on a login keychain.
func (s Store) UpsertCredentialProfile(name string, credential CredentialInfo) (Profile, error) {
	name = strings.TrimSpace(name)
	if err := ValidateProfileNameAllowEmail(name); err != nil {
		return Profile{}, err
	}
	dir := sanitizeName(name)
	if dir == "" {
		return Profile{}, errors.New("invalid Claude profile name")
	}
	instancePath := filepath.Join(s.InstancesDir(), dir)
	if err := s.initInstanceDir(instancePath); err != nil {
		return Profile{}, err
	}
	body, err := credentialPayload(credential)
	if err != nil {
		return Profile{}, err
	}
	if err := writePrivateFileAtomic(filepath.Join(instancePath, ".credentials.json"), body); err != nil {
		return Profile{}, err
	}
	if err := s.RegisterProfile(name, dir); err != nil {
		return Profile{}, err
	}
	profile, ok := s.FindProfile(name)
	if !ok {
		return Profile{}, fmt.Errorf("Claude profile %q was not readable after registration", name)
	}
	return profile, nil
}

func deleteKeychainCredential(instancePath string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	service := "Claude Code-credentials-" + keychainHash(instancePath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(
		ctx,
		"security",
		"delete-generic-password",
		"-s", service,
		"-a", u.Username,
	).CombinedOutput()
	if err == nil || strings.Contains(strings.ToLower(string(output)), "could not be found") {
		return nil
	}
	return fmt.Errorf("delete Claude credential from keychain: %w", err)
}

func readCredentialFile(instancePath string) (*CredentialInfo, bool) {
	body, err := os.ReadFile(filepath.Join(instancePath, ".credentials.json"))
	if err != nil {
		return nil, false
	}
	credential, err := parseCredentialPayload(body, "credentials file")
	return credential, err == nil && credential != nil
}

// parseCredentialPayload decodes a stored credential blob. source names where
// the blob came from, and appears in the decode error along with a redacted
// summary of the payload's shape: a decode failure is otherwise indistinguishable
// between a keychain wrapper, a partial write, and a corrupt file.
func parseCredentialPayload(body []byte, source string) (*CredentialInfo, error) {
	var raw struct {
		ClaudeAIOAuth *CredentialInfo `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%s from %s (%s): %w", unreadableCredentialPhrase, source, describeCredentialPayload(body, err), err)
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

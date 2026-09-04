package claude

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
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

	accountpkg "github.com/manaflow-ai/subrouter/account"
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
	// syncDirectoryForTest injects registry directory-sync failures. Production
	// stores leave it nil and use the platform durability boundary below.
	syncDirectoryForTest func(string) error
	// syncRemovalParentsForTest injects instance staging/rollback/cleanup
	// directory-sync failures. Production stores leave it nil.
	syncRemovalParentsForTest func(map[string]struct{}) error
	// afterProfileInstancesStagedForTest injects a non-cooperating credential
	// mutation at the exact post-stage/pre-registry-commit boundary.
	afterProfileInstancesStagedForTest func([]stagedProfileInstance)
	// afterPostStageCredentialHashForTest injects a path recreation after the
	// post-stage traversal but before its final absence boundary.
	afterPostStageCredentialHashForTest func()
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

// ProfileRemovalSnapshot is a non-secret, exact identity for one registered
// profile and the credential currently stored at its instance paths. It is
// safe to persist in a crash-recovery journal.
type ProfileRemovalSnapshot struct {
	InstanceDir       string
	CredentialVersion string
}

var ErrProfileRemovalCredentialChanged = errors.New("Claude profile credential changed during removal")

// ErrProfileRegistryWriteCommitted means the registry rename is already
// visible but syncing its containing directory failed, so callers must not
// roll the visible mutation back as though the write never happened.
var ErrProfileRegistryWriteCommitted = errors.New("Claude profile registry write committed with uncertain durability")

type profileRegistryWriteCommittedError struct{ cause error }

func (e *profileRegistryWriteCommittedError) Error() string {
	return fmt.Sprintf("%v: %v", ErrProfileRegistryWriteCommitted, e.cause)
}

func (e *profileRegistryWriteCommittedError) Unwrap() []error {
	return []error{ErrProfileRegistryWriteCommitted, e.cause}
}

func profileRegistryWriteCommitted(err error) bool {
	return errors.Is(err, ErrProfileRegistryWriteCommitted)
}

func reportCommittedProfileRegistryWrite(operation string, err error) error {
	if !profileRegistryWriteCommitted(err) {
		return err
	}
	slog.Warn("Claude profile registry mutation is visible but directory durability is uncertain", "operation", operation, "error", err)
	return err
}

// PlanType returns the subscription label explicitly carried by a Claude
// credential. It never guesses a plan from token contents or observed usage.
func (credential *CredentialInfo) PlanType() string {
	if credential == nil {
		return "unknown"
	}
	plan := strings.TrimSpace(credential.SubscriptionType)
	if plan == "" {
		plan = strings.TrimSpace(credential.RateLimitTier)
	}
	if plan == "" {
		return "unknown"
	}
	lower := strings.ToLower(plan)
	for _, part := range strings.FieldsFunc(lower, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		switch part {
		case "max", "pro", "free":
			return part
		}
	}
	if len(plan) > 32 || strings.IndexFunc(plan, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '-' || r == '_' || r == '.' || r == '+')
	}) >= 0 {
		return "unknown"
	}
	return plan
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

// DefaultStoreForReadOnlyInspection uses the same effective account root as
// DefaultStore without importing the legacy Codex store. Serving processes use
// this constructor so daemon startup cannot copy interactive credentials.
func DefaultStoreForReadOnlyInspection() Store {
	home, _ := os.UserHomeDir()
	shared := ""
	if home != "" {
		shared = filepath.Join(home, ".claude")
	}
	return Store{Dir: storepath.CodexDirForReadOnlyInspection(), SharedStateDir: shared}
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

// PrepareSharedStateDir gives a credential-isolated Claude config home access
// to the same conversation history as direct and managed-profile launches.
// Only high-growth, non-credential state is shared; authentication and routing
// files remain private to configDir.
func (s Store) PrepareSharedStateDir(configDir string) error {
	return s.prepareSharedState(configDir)
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
	originalPath         string
	stagedPath           string
	stagingRoot          string
	operationID          string
	credentialSetVersion string
	prepared             bool
}

const (
	profileRemovalStageManifestVersion   = 3
	profileRemovalStageManifestName      = ".subrouter-removal-stage.json"
	profileRemovalStageEntryName         = "instance"
	profileRemovalStageManifestMaxSize   = 4096
	profileRemovalOperationMarkerVersion = 2
	profileRemovalOperationMarkerName    = ".subrouter-removal-operation.json"
	profileRemovalOperationMarkerMaxSize = 4096
)

type profileRemovalStageManifest struct {
	Version              int    `json:"version"`
	OriginalPath         string `json:"original_path"`
	StagingRoot          string `json:"staging_root"`
	EntryName            string `json:"entry_name"`
	OperationID          string `json:"operation_id"`
	CredentialSetVersion string `json:"credential_set_version"`
}

type profileRemovalOperationMarker struct {
	Version              int    `json:"version"`
	OriginalPath         string `json:"original_path"`
	OperationID          string `json:"operation_id"`
	CredentialSetVersion string `json:"credential_set_version"`
}

func stageProfileInstancePaths(paths []string) ([]stagedProfileInstance, error) {
	credentialSetVersion, err := filesystemProfileCredentialVersion(paths)
	if err != nil {
		return nil, err
	}
	return stageProfileInstancePathsWithVersion(paths, credentialSetVersion, syncProfileRemovalParents)
}

func stageProfileInstancePathsWithSync(
	paths []string,
	syncParents func(map[string]struct{}) error,
) ([]stagedProfileInstance, error) {
	credentialSetVersion, err := filesystemProfileCredentialVersion(paths)
	if err != nil {
		return nil, err
	}
	return stageProfileInstancePathsWithVersion(paths, credentialSetVersion, syncParents)
}

func stageProfileInstancePathsWithVersion(
	paths []string,
	credentialSetVersion string,
	syncParents func(map[string]struct{}) error,
) ([]stagedProfileInstance, error) {
	if !validProfileCredentialSetVersion(credentialSetVersion) {
		return nil, errors.New("invalid Claude profile credential-set version")
	}
	operationID, err := newProfileRemovalOperationID()
	if err != nil {
		return nil, err
	}
	staged := make([]stagedProfileInstance, 0, len(paths))
	for _, path := range paths {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, errors.Join(err, rollbackStagedProfileInstancesWithSync(staged, syncParents))
		}
		stagingRoot, err := os.MkdirTemp(
			filepath.Dir(path),
			"."+filepath.Base(path)+".remove-*",
		)
		if err != nil {
			return nil, errors.Join(err, rollbackStagedProfileInstancesWithSync(staged, syncParents))
		}
		entry := stagedProfileInstance{
			originalPath:         path,
			stagedPath:           filepath.Join(stagingRoot, profileRemovalStageEntryName),
			stagingRoot:          stagingRoot,
			operationID:          operationID,
			credentialSetVersion: credentialSetVersion,
		}
		if err := writeProfileRemovalStageManifest(entry); err != nil {
			_ = os.RemoveAll(stagingRoot)
			parents := map[string]struct{}{filepath.Dir(stagingRoot): {}}
			return nil, errors.Join(err, syncParents(parents), rollbackStagedProfileInstancesWithSync(staged, syncParents))
		}
		// Persist the stage-root entry itself before moving the only credential.
		if err := syncParents(map[string]struct{}{filepath.Dir(stagingRoot): {}}); err != nil {
			_ = os.RemoveAll(stagingRoot)
			return nil, errors.Join(err, syncParents(map[string]struct{}{filepath.Dir(stagingRoot): {}}), rollbackStagedProfileInstancesWithSync(staged, syncParents))
		}
		if err := writeProfileRemovalOperationMarker(entry); err != nil {
			cleanupErr := removeProfileRemovalOperationMarker(entry, syncParents)
			var stageCleanupErr error
			if cleanupErr == nil {
				stageCleanupErr = errors.Join(
					os.RemoveAll(stagingRoot),
					syncParents(map[string]struct{}{filepath.Dir(stagingRoot): {}}),
				)
			}
			// Never discard the ownership manifest while an exact live marker may
			// remain. Startup can safely finish marker cleanup only while that
			// provenance root is still available.
			return nil, errors.Join(err, cleanupErr, stageCleanupErr, rollbackStagedProfileInstancesWithSync(staged, syncParents))
		}
		if err := os.Rename(entry.originalPath, entry.stagedPath); err != nil {
			markerErr := removeProfileRemovalOperationMarker(entry, syncParents)
			_ = os.RemoveAll(stagingRoot)
			parents := map[string]struct{}{filepath.Dir(stagingRoot): {}}
			return nil, errors.Join(err, markerErr, syncParents(parents), rollbackStagedProfileInstancesWithSync(staged, syncParents))
		}
		staged = append(staged, entry)
		// The rename removes an entry from the instance parent and adds one to
		// the stage root. Both directory records must reach disk before the
		// next path is touched, otherwise a crash mid-loop can lose the only
		// copy before startup has a durable stage to recover.
		parents := map[string]struct{}{
			filepath.Dir(entry.originalPath): {},
			entry.stagingRoot:                {},
		}
		if err := syncParents(parents); err != nil {
			return nil, errors.Join(err, rollbackStagedProfileInstancesWithSync(staged, syncParents))
		}
	}
	return staged, nil
}

func (s Store) stageProfileInstancePathsValidated(
	ctx context.Context,
	paths []string,
) ([]stagedProfileInstance, error) {
	// Include already-owned stages so exact-removal replay fingerprints the
	// journaled credential rather than a stale path-keyed Keychain item.
	credentialSetVersion, err := s.profileCredentialVersionLocked(ctx, paths, true)
	if err != nil {
		return nil, err
	}
	staged, err := stageProfileInstancePathsWithVersion(paths, credentialSetVersion, s.syncProfileRemovalParents)
	if err != nil {
		return nil, err
	}
	if s.afterProfileInstancesStagedForTest != nil {
		s.afterProfileInstancesStagedForTest(staged)
	}
	postStageVersion, validationErr := s.profileCredentialVersionLockedRequiringAbsent(ctx, paths, true, staged)
	if s.afterPostStageCredentialHashForTest != nil {
		s.afterPostStageCredentialHashForTest()
	}
	if validationErr == nil {
		validationErr = validateStagedProfilePathsRemainAbsent(staged)
	}
	if validationErr == nil && postStageVersion == credentialSetVersion {
		return staged, nil
	}
	if validationErr == nil {
		validationErr = ErrProfileRemovalCredentialChanged
	}
	rollbackErr := rollbackStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents)
	return nil, errors.Join(
		fmt.Errorf("validate staged Claude profile credential identity: %w", validationErr),
		rollbackErr,
	)
}

func validateStagedProfilePathsRemainAbsent(staged []stagedProfileInstance) error {
	for _, entry := range staged {
		if _, err := os.Lstat(entry.originalPath); err == nil {
			return fmt.Errorf("%w: staged Claude profile path %q reappeared", ErrProfileRemovalCredentialChanged, entry.originalPath)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("validate staged Claude profile path %q: %w", entry.originalPath, err)
		}
	}
	return nil
}

func (s Store) validateStagedProfileCommitBoundary(staged []stagedProfileInstance) error {
	if err := validateStagedProfilePathsRemainAbsent(staged); err != nil {
		return errors.Join(err, rollbackStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents))
	}
	return nil
}

func rollbackStagedProfileInstances(staged []stagedProfileInstance) error {
	return rollbackStagedProfileInstancesWithSync(staged, syncProfileRemovalParents)
}

func rollbackStagedProfileInstancesWithSync(
	staged []stagedProfileInstance,
	syncParents func(map[string]struct{}) error,
) error {
	return rollbackStagedProfileInstancesWithOps(staged, syncParents, renameProfileInstanceNoReplace)
}

func rollbackStagedProfileInstancesWithOps(
	staged []stagedProfileInstance,
	syncParents func(map[string]struct{}) error,
	rename func(string, string) error,
) error {
	return restoreStagedProfileInstancesWithOps(staged, staged, syncParents, rename)
}

func restoreStagedProfileInstancesWithOps(
	restore []stagedProfileInstance,
	provenance []stagedProfileInstance,
	syncParents func(map[string]struct{}) error,
	rename func(string, string) error,
) error {
	var restoreErr error
	for index := len(restore) - 1; index >= 0; index-- {
		entry := restore[index]
		if err := rename(entry.stagedPath, entry.originalPath); err != nil {
			restoreErr = errors.Join(restoreErr, err)
			continue
		}
		// Prove the reverse rename before removing its ownership provenance. If
		// this boundary fails, startup sees a live path plus a manifest-only
		// prepared stage and can safely finish the rollback.
		if err := syncParents(map[string]struct{}{
			filepath.Dir(entry.originalPath): {},
			entry.stagingRoot:                {},
		}); err != nil {
			restoreErr = errors.Join(restoreErr, err)
		}
	}
	// Never remove any provenance unless every reverse rename and its immediate
	// durability boundary succeeded. This lets startup finish a multi-root
	// rollback without seeing one live path beside another actual stage merely
	// because the first root's sync failed.
	if restoreErr != nil {
		return restoreErr
	}
	return finalizeRestoredProfileInstances(provenance, syncParents)
}

func finalizeRestoredProfileInstances(
	provenance []stagedProfileInstance,
	syncParents func(map[string]struct{}) error,
) error {
	// The marker is the strong proof that each live directory is the directory
	// restored by this operation, not a same-path replacement. Keep every stage
	// manifest until all marker removals are durable across the whole set.
	var markerErr error
	for _, entry := range provenance {
		markerErr = errors.Join(markerErr, removeProfileRemovalOperationMarker(entry, syncParents))
	}
	if markerErr != nil {
		return markerErr
	}
	var cleanupErr error
	for index := len(provenance) - 1; index >= 0; index-- {
		entry := provenance[index]
		// Keep provenance present until cleanup removes the owned root. Removing
		// the manifest first would turn an interrupted rollback into an unowned
		// lookalike that safe restart reconciliation must refuse forever.
		removeErr := os.RemoveAll(entry.stagingRoot)
		syncErr := syncParents(map[string]struct{}{filepath.Dir(entry.stagingRoot): {}})
		cleanupErr = errors.Join(cleanupErr, removeErr, syncErr)
	}
	return cleanupErr
}

func deletePreparedProfileStagesWithSync(
	prepared []stagedProfileInstance,
	syncParents func(map[string]struct{}) error,
) error {
	if err := syncPreparedProfileStages(prepared, syncParents); err != nil {
		return err
	}
	return finalizeRestoredProfileInstances(prepared, syncParents)
}

func syncPreparedProfileStages(
	prepared []stagedProfileInstance,
	syncParents func(map[string]struct{}) error,
) error {
	var syncErr error
	for _, entry := range prepared {
		if !entry.prepared {
			return fmt.Errorf("Claude profile removal stage %q is not prepared", entry.stagingRoot)
		}
		// A manifest-only stage means the credential was never moved or was
		// already restored. Prove both live-parent and provenance-root state
		// across the whole set before discarding any recovery marker.
		syncErr = errors.Join(syncErr, syncParents(map[string]struct{}{
			filepath.Dir(entry.originalPath): {},
			entry.stagingRoot:                {},
		}))
	}
	return syncErr
}

func deleteStagedProfileInstances(
	staged []stagedProfileInstance,
) error {
	return deleteStagedProfileInstancesWithSync(staged, syncProfileRemovalParents)
}

func deleteStagedProfileInstancesWithSync(
	staged []stagedProfileInstance,
	syncParents func(map[string]struct{}) error,
) error {
	var cleanupErr error
	parents := make(map[string]struct{})
	for _, entry := range staged {
		cleanupErr = errors.Join(cleanupErr, os.RemoveAll(entry.stagingRoot))
		parents[filepath.Dir(entry.stagingRoot)] = struct{}{}
	}
	return errors.Join(cleanupErr, syncParents(parents))
}

func deleteOrphanedStagedProfileInstances(instancePaths []string) error {
	return deleteOrphanedStagedProfileInstancesWithSync(instancePaths, syncProfileRemovalParents)
}

func deleteOrphanedStagedProfileInstancesWithSync(
	instancePaths []string,
	syncParents func(map[string]struct{}) error,
) error {
	var cleanupErr error
	parents := make(map[string]struct{})
	for _, instancePath := range instancePaths {
		// Sync even when no orphan is currently visible. A prior cleanup may
		// have removed it but failed its parent-directory sync; replay must not
		// clear the durable journal until that directory boundary succeeds.
		parents[filepath.Dir(instancePath)] = struct{}{}
		matches, err := stagedProfileInstanceRoots(instancePath)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		for _, match := range matches {
			cleanupErr = errors.Join(cleanupErr, os.RemoveAll(match))
			parents[filepath.Dir(match)] = struct{}{}
		}
	}
	return errors.Join(cleanupErr, syncParents(parents))
}

func normalizedProfileRemovalPath(path string) (string, error) {
	return filepath.Abs(filepath.Clean(path))
}

func newProfileRemovalOperationID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate Claude profile removal operation identity: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func validProfileRemovalOperationID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func validProfileCredentialSetVersion(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func writeProfileRemovalStageManifest(entry stagedProfileInstance) error {
	if !validProfileRemovalOperationID(entry.operationID) || !validProfileCredentialSetVersion(entry.credentialSetVersion) {
		return errors.New("invalid Claude profile removal ownership identity")
	}
	originalPath, err := normalizedProfileRemovalPath(entry.originalPath)
	if err != nil {
		return err
	}
	stagingRoot, err := normalizedProfileRemovalPath(entry.stagingRoot)
	if err != nil {
		return err
	}
	body, err := json.Marshal(profileRemovalStageManifest{
		Version:              profileRemovalStageManifestVersion,
		OriginalPath:         originalPath,
		StagingRoot:          stagingRoot,
		EntryName:            profileRemovalStageEntryName,
		OperationID:          entry.operationID,
		CredentialSetVersion: entry.credentialSetVersion,
	})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return writePrivateFileAtomic(filepath.Join(entry.stagingRoot, profileRemovalStageManifestName), body)
}

func writeProfileRemovalOperationMarker(entry stagedProfileInstance) error {
	originalPath, err := normalizedProfileRemovalPath(entry.originalPath)
	if err != nil {
		return err
	}
	if !validProfileRemovalOperationID(entry.operationID) || !validProfileCredentialSetVersion(entry.credentialSetVersion) {
		return errors.New("invalid Claude profile removal operation identity")
	}
	markerPath := filepath.Join(entry.originalPath, profileRemovalOperationMarkerName)
	if _, err := os.Lstat(markerPath); err == nil {
		return fmt.Errorf("Claude profile removal operation marker %q already exists", markerPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	body, err := json.Marshal(profileRemovalOperationMarker{
		Version:              profileRemovalOperationMarkerVersion,
		OriginalPath:         originalPath,
		OperationID:          entry.operationID,
		CredentialSetVersion: entry.credentialSetVersion,
	})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return writePrivateFileAtomic(markerPath, body)
}

func readProfileRemovalOperationMarker(entry stagedProfileInstance) (bool, error) {
	return readProfileRemovalOperationMarkerAt(entry, entry.originalPath)
}

func readProfileRemovalOperationMarkerAt(entry stagedProfileInstance, directory string) (bool, error) {
	marker, present, err := readProfileRemovalOperationMarkerIdentity(directory, entry.originalPath)
	if err != nil || !present {
		return present, err
	}
	if marker.operationID != entry.operationID || marker.credentialSetVersion != entry.credentialSetVersion {
		return false, fmt.Errorf("Claude profile removal operation marker %q does not match its exact identity", filepath.Join(directory, profileRemovalOperationMarkerName))
	}
	return true, nil
}

func readProfileRemovalOperationMarkerIdentity(directory, expectedOriginalPath string) (stagedProfileInstance, bool, error) {
	markerPath := filepath.Join(directory, profileRemovalOperationMarkerName)
	entry := stagedProfileInstance{originalPath: filepath.Clean(expectedOriginalPath)}
	info, err := os.Lstat(markerPath)
	if errors.Is(err, os.ErrNotExist) {
		return entry, false, nil
	}
	if err != nil {
		return entry, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > profileRemovalOperationMarkerMaxSize {
		return entry, false, fmt.Errorf("Claude profile removal operation marker %q is not a safe regular file", markerPath)
	}
	body, err := os.ReadFile(markerPath)
	if err != nil {
		return entry, false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var marker profileRemovalOperationMarker
	if err := decoder.Decode(&marker); err != nil {
		return entry, false, fmt.Errorf("decode Claude profile removal operation marker %q: %w", markerPath, err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return entry, false, fmt.Errorf("Claude profile removal operation marker %q has trailing data", markerPath)
	}
	originalPath, err := normalizedProfileRemovalPath(expectedOriginalPath)
	if err != nil {
		return entry, false, err
	}
	if marker.Version != profileRemovalOperationMarkerVersion ||
		marker.OriginalPath != originalPath ||
		!validProfileRemovalOperationID(marker.OperationID) ||
		!validProfileCredentialSetVersion(marker.CredentialSetVersion) {
		return entry, false, fmt.Errorf("Claude profile removal operation marker %q does not match its exact identity", markerPath)
	}
	entry.operationID = marker.OperationID
	entry.credentialSetVersion = marker.CredentialSetVersion
	return entry, true, nil
}

func removeProfileRemovalOperationMarker(
	entry stagedProfileInstance,
	syncParents func(map[string]struct{}) error,
) error {
	present, err := readProfileRemovalOperationMarker(entry)
	if err != nil {
		return err
	}
	if present {
		if err := os.Remove(filepath.Join(entry.originalPath, profileRemovalOperationMarkerName)); err != nil {
			return err
		}
	}
	// Sync the live directory even when replay finds the marker absent: an
	// earlier attempt may have removed it but failed this durability boundary.
	return syncParents(map[string]struct{}{entry.originalPath: {}})
}

func readOwnedProfileRemovalStage(stagingRoot, expectedOriginalPath string) (stagedProfileInstance, error) {
	entry := stagedProfileInstance{
		originalPath: filepath.Clean(expectedOriginalPath),
		stagedPath:   filepath.Join(stagingRoot, profileRemovalStageEntryName),
		stagingRoot:  filepath.Clean(stagingRoot),
	}
	rootInfo, err := os.Lstat(entry.stagingRoot)
	if err != nil {
		return entry, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return entry, fmt.Errorf("Claude profile removal stage %q is not a regular directory", entry.stagingRoot)
	}
	manifestPath := filepath.Join(entry.stagingRoot, profileRemovalStageManifestName)
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil {
		return entry, fmt.Errorf("Claude profile removal stage %q has no valid ownership manifest: %w", entry.stagingRoot, err)
	}
	if manifestInfo.Mode()&os.ModeSymlink != 0 || !manifestInfo.Mode().IsRegular() || manifestInfo.Size() > profileRemovalStageManifestMaxSize {
		return entry, fmt.Errorf("Claude profile removal stage %q ownership manifest is not a safe regular file", entry.stagingRoot)
	}
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		return entry, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var manifest profileRemovalStageManifest
	if err := decoder.Decode(&manifest); err != nil {
		return entry, fmt.Errorf("decode Claude profile removal stage manifest %q: %w", manifestPath, err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return entry, fmt.Errorf("Claude profile removal stage manifest %q has trailing data", manifestPath)
	}
	expectedOriginal, err := normalizedProfileRemovalPath(expectedOriginalPath)
	if err != nil {
		return entry, err
	}
	expectedRoot, err := normalizedProfileRemovalPath(stagingRoot)
	if err != nil {
		return entry, err
	}
	if manifest.Version != profileRemovalStageManifestVersion ||
		manifest.OriginalPath != expectedOriginal ||
		manifest.StagingRoot != expectedRoot ||
		manifest.EntryName != profileRemovalStageEntryName ||
		!validProfileRemovalOperationID(manifest.OperationID) ||
		!validProfileCredentialSetVersion(manifest.CredentialSetVersion) {
		return entry, fmt.Errorf("Claude profile removal stage %q ownership manifest does not match its exact identity", entry.stagingRoot)
	}
	entry.operationID = manifest.OperationID
	entry.credentialSetVersion = manifest.CredentialSetVersion
	entries, err := os.ReadDir(entry.stagingRoot)
	if err != nil {
		return entry, err
	}
	if len(entries) == 1 && entries[0].Name() == profileRemovalStageManifestName {
		entry.prepared = true
		return entry, nil
	}
	if len(entries) != 2 {
		return entry, fmt.Errorf("Claude profile removal stage %q has unexpected structure", entry.stagingRoot)
	}
	seenManifest, seenInstance := false, false
	for _, child := range entries {
		switch child.Name() {
		case profileRemovalStageManifestName:
			seenManifest = true
		case profileRemovalStageEntryName:
			if child.Type()&os.ModeSymlink != 0 || !child.IsDir() {
				return entry, fmt.Errorf("Claude profile removal staged instance %q is not a regular directory", entry.stagedPath)
			}
			seenInstance = true
		default:
			return entry, fmt.Errorf("Claude profile removal stage %q has unexpected entry %q", entry.stagingRoot, child.Name())
		}
	}
	if !seenManifest || !seenInstance {
		return entry, fmt.Errorf("Claude profile removal stage %q has incomplete structure", entry.stagingRoot)
	}
	markerPresent, err := readProfileRemovalOperationMarkerAt(entry, entry.stagedPath)
	if err != nil {
		return entry, err
	}
	if !markerPresent {
		return entry, fmt.Errorf("Claude profile removal staged instance %q has no operation marker", entry.stagedPath)
	}
	return entry, nil
}

func stagedProfileInstanceRoots(instancePath string) ([]string, error) {
	parent := filepath.Dir(instancePath)
	entries, err := os.ReadDir(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	prefix := "." + filepath.Base(instancePath) + ".remove-"
	matches := make([]string, 0)
	for _, entry := range entries {
		suffix := strings.TrimPrefix(entry.Name(), prefix)
		generatedSuffix := suffix != "" && suffix != entry.Name()
		for index := 0; generatedSuffix && index < len(suffix); index++ {
			generatedSuffix = suffix[index] >= '0' && suffix[index] <= '9'
		}
		// os.MkdirTemp appends a decimal random value. Requiring that exact
		// suffix keeps a profile named "work" from claiming a stage owned by a
		// sibling named "work.remove-extra".
		if generatedSuffix {
			root := filepath.Join(parent, entry.Name())
			if _, err := readOwnedProfileRemovalStage(root, instancePath); err != nil {
				return nil, err
			}
			matches = append(matches, root)
		}
	}
	return matches, nil
}

// ReconcileProfileInstanceStagesContext repairs an interrupted profile removal
// using the registry as the commit authority. A registered profile whose live
// instance is absent gets its one exact stage restored. Stages for profiles no
// longer in the registry are deleted. A live replacement plus a staged secret,
// or multiple possible stages, is left untouched and reported as ambiguous.
func (s Store) ReconcileProfileInstanceStagesContext(ctx context.Context) (err error) {
	registryLock, err := lockProfileRegistryContext(ctx, s.ProfilesPath())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, registryLock.Close()) }()

	data := profilesFile{Profiles: map[string]Profile{}}
	body, readErr := readFileForAtomicReplace(s.ProfilesPath())
	if readErr == nil {
		if err := json.Unmarshal(body, &data); err != nil {
			return fmt.Errorf("decode Claude profile registry: %w", err)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}

	registeredByPath := make(map[string]string)
	pathsByProfile := make(map[string][]string)
	instanceRoots := []string{filepath.Clean(s.InstancesDir())}
	if legacyRoot, ok := s.legacyInstancePath(s.InstancesDir()); ok {
		instanceRoots = append(instanceRoots, filepath.Clean(legacyRoot))
	}
	for name, profile := range data.Profiles {
		if profile.Name != name {
			return fmt.Errorf("Claude profile %q has mismatched registry identity %q", name, profile.Name)
		}
		dir := resolvedClaudeProfileInstanceDir(name, profile)
		paths, err := s.profileInstancePaths(dir)
		if err != nil {
			return fmt.Errorf("resolve Claude profile %q instance paths: %w", name, err)
		}
		for _, path := range paths {
			path = filepath.Clean(path)
			if owner, exists := registeredByPath[path]; exists && owner != name {
				return fmt.Errorf("Claude profiles %q and %q share instance path %q", owner, name, path)
			}
			registeredByPath[path] = name
			pathsByProfile[name] = append(pathsByProfile[name], path)
		}
	}

	stagesByOriginal := make(map[string][]stagedProfileInstance)
	for _, root := range instanceRoots {
		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			originalPath, ok := stagedProfileOriginalPath(root, entry.Name())
			if !ok {
				continue
			}
			stagingRoot := filepath.Join(root, entry.Name())
			owned, err := readOwnedProfileRemovalStage(stagingRoot, originalPath)
			if err != nil {
				return err
			}
			stagesByOriginal[originalPath] = append(stagesByOriginal[originalPath], owned)
		}
	}
	orphanMarkersByOriginal := make(map[string]stagedProfileInstance)
	for originalPath := range registeredByPath {
		marker, present, err := readProfileRemovalOperationMarkerIdentity(originalPath, originalPath)
		if err != nil {
			return err
		}
		if present && len(stagesByOriginal[originalPath]) == 0 {
			orphanMarkersByOriginal[originalPath] = marker
		}
	}
	if len(stagesByOriginal) == 0 && len(orphanMarkersByOriginal) == 0 {
		return nil
	}

	lockPathSet := make(map[string]struct{})
	for path := range stagesByOriginal {
		lockPathSet[path] = struct{}{}
	}
	for path := range registeredByPath {
		lockPathSet[path] = struct{}{}
	}
	originalPaths := make([]string, 0, len(lockPathSet))
	for path := range lockPathSet {
		originalPaths = append(originalPaths, path)
	}
	sort.Strings(originalPaths)
	credentialLocks, err := lockProfileCredentialPaths(ctx, originalPaths)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeProfileCredentialLocks(credentialLocks)) }()

	// A path outside the current registry is a committed removal. Every stage
	// was provenance-validated before this point, so those roots can roll
	// forward independently.
	for _, originalPath := range originalPaths {
		if _, isRegistered := registeredByPath[originalPath]; isRegistered {
			continue
		}
		if staged := stagesByOriginal[originalPath]; len(staged) != 0 {
			if err := deleteStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents); err != nil {
				return err
			}
		}
	}

	profileNames := make([]string, 0, len(pathsByProfile))
	for name := range pathsByProfile {
		profileNames = append(profileNames, name)
	}
	sort.Strings(profileNames)
	for _, profileName := range profileNames {
		paths := pathsByProfile[profileName]
		sort.Strings(paths)
		var orphanMarkers []stagedProfileInstance
		profileHasStages := false
		for _, path := range paths {
			profileHasStages = profileHasStages || len(stagesByOriginal[path]) != 0
			if marker, found := orphanMarkersByOriginal[path]; found {
				orphanMarkers = append(orphanMarkers, marker)
			}
		}
		if len(orphanMarkers) != 0 {
			if profileHasStages {
				return fmt.Errorf("Claude profile %q has an orphan operation marker beside staged recovery state", profileName)
			}
			operationID := orphanMarkers[0].operationID
			credentialSetVersion := orphanMarkers[0].credentialSetVersion
			for _, marker := range orphanMarkers[1:] {
				if marker.operationID != operationID || marker.credentialSetVersion != credentialSetVersion {
					return fmt.Errorf("Claude profile %q has orphan markers from multiple removal operations", profileName)
				}
			}
			currentVersion, err := s.profileCredentialVersionLocked(ctx, paths, true)
			if err != nil {
				return err
			}
			if currentVersion != credentialSetVersion {
				return fmt.Errorf("Claude profile %q credential changed while an orphan removal marker remained", profileName)
			}
			var cleanupErr error
			for _, marker := range orphanMarkers {
				cleanupErr = errors.Join(cleanupErr, removeProfileRemovalOperationMarker(marker, s.syncProfileRemovalParents))
			}
			if cleanupErr != nil {
				return cleanupErr
			}
			continue
		}
		var prepared []stagedProfileInstance
		preparedByPath := make(map[string][]stagedProfileInstance)
		actualByPath := make(map[string][]stagedProfileInstance)
		for _, path := range paths {
			for _, candidate := range stagesByOriginal[path] {
				if candidate.prepared {
					prepared = append(prepared, candidate)
					preparedByPath[path] = append(preparedByPath[path], candidate)
				} else {
					actualByPath[path] = append(actualByPath[path], candidate)
				}
			}
		}
		actualCount := 0
		operationID := ""
		credentialSetVersion := ""
		for _, path := range paths {
			if len(preparedByPath[path]) > 1 {
				return fmt.Errorf("Claude profile %q has multiple prepared stages for %q", profileName, path)
			}
			if len(actualByPath[path]) > 1 {
				return fmt.Errorf("Claude profile %q has multiple staged credentials for %q", profileName, path)
			}
			actualCount += len(actualByPath[path])
			for _, candidate := range append(append([]stagedProfileInstance(nil), preparedByPath[path]...), actualByPath[path]...) {
				if operationID == "" {
					operationID = candidate.operationID
				} else if candidate.operationID != operationID {
					return fmt.Errorf("Claude profile %q has stages from multiple removal operations", profileName)
				}
				if credentialSetVersion == "" {
					credentialSetVersion = candidate.credentialSetVersion
				} else if candidate.credentialSetVersion != credentialSetVersion {
					return fmt.Errorf("Claude profile %q has stages from multiple credential snapshots", profileName)
				}
			}
		}
		if credentialSetVersion == "" {
			continue
		}
		currentCredentialSetVersion, err := s.profileCredentialVersionLocked(ctx, paths, true)
		if err != nil {
			return err
		}
		if currentCredentialSetVersion != credentialSetVersion {
			return fmt.Errorf("Claude profile %q credential changed during interrupted removal recovery", profileName)
		}
		liveByPath := make(map[string]bool)
		anyLive := false
		for _, path := range paths {
			if _, liveErr := os.Lstat(path); liveErr == nil {
				anyLive = true
				liveByPath[path] = true
			} else if !errors.Is(liveErr, os.ErrNotExist) {
				return liveErr
			}
		}
		if actualCount == 0 {
			for path := range preparedByPath {
				if !liveByPath[path] {
					return fmt.Errorf("Claude profile %q has a prepared stage without its live instance at %q", profileName, path)
				}
			}
			if err := deletePreparedProfileStagesWithSync(prepared, s.syncProfileRemovalParents); err != nil {
				return err
			}
			continue
		}
		var restore []stagedProfileInstance
		for _, path := range paths {
			restore = append(restore, actualByPath[path]...)
		}
		if anyLive {
			// A partial rollback is distinguishable from a replacement only when
			// every live physical path retains its exact prepared manifest and no
			// actual stage claims that same path. Any unproven live path is a
			// replacement boundary and must remain untouched.
			for _, path := range paths {
				if liveByPath[path] {
					if len(preparedByPath[path]) != 1 || len(actualByPath[path]) != 0 {
						return fmt.Errorf("Claude profile %q has both a live instance and a staged credential across its canonical or legacy paths", profileName)
					}
					markerPresent, markerErr := readProfileRemovalOperationMarker(preparedByPath[path][0])
					if markerErr != nil {
						return markerErr
					}
					if !markerPresent {
						return fmt.Errorf("Claude profile %q live instance %q lacks matching rollback operation provenance", profileName, path)
					}
				} else if len(preparedByPath[path]) != 0 {
					return fmt.Errorf("Claude profile %q has ambiguous prepared rollback provenance for %q", profileName, path)
				}
			}
			if err := syncPreparedProfileStages(prepared, s.syncProfileRemovalParents); err != nil {
				return err
			}
		} else if len(prepared) != 0 {
			return fmt.Errorf("Claude profile %q has prepared rollback provenance without a live instance", profileName)
		}
		provenance := append(append([]stagedProfileInstance(nil), restore...), prepared...)
		if err := restoreStagedProfileInstancesWithOps(restore, provenance, s.syncProfileRemovalParents, renameProfileInstanceNoReplace); err != nil {
			return fmt.Errorf("restore Claude profile %q instances: %w", profileName, err)
		}
	}
	return nil
}

func stagedProfileOriginalPath(root, entryName string) (string, bool) {
	if !strings.HasPrefix(entryName, ".") {
		return "", false
	}
	marker := strings.LastIndex(entryName, ".remove-")
	if marker <= 1 {
		return "", false
	}
	suffix := entryName[marker+len(".remove-"):]
	if suffix == "" {
		return "", false
	}
	for index := range suffix {
		if suffix[index] < '0' || suffix[index] > '9' {
			return "", false
		}
	}
	base := entryName[1:marker]
	if !safeProfileDir(base) {
		return "", false
	}
	return filepath.Join(filepath.Clean(root), base), true
}

func syncProfileRemovalParents(parents map[string]struct{}) error {
	return syncProfileRemovalParentsForOS(runtime.GOOS, parents, os.Open)
}

func (s Store) syncProfileRemovalParents(parents map[string]struct{}) error {
	if s.syncRemovalParentsForTest != nil {
		return s.syncRemovalParentsForTest(parents)
	}
	return syncProfileRemovalParents(parents)
}

func syncProfileRemovalParentsForOS(
	goos string,
	parents map[string]struct{},
	openDir func(string) (*os.File, error),
) error {
	if goos == "windows" {
		return nil
	}
	var syncErr error
	for parent := range parents {
		dir, err := openDir(parent)
		if err != nil {
			// A legacy instance root may never have existed on this host. There is
			// no removed directory entry to make durable in an absent parent, so
			// replay can treat that boundary as already satisfied.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			syncErr = errors.Join(syncErr, err)
			continue
		}
		syncErr = errors.Join(syncErr, dir.Sync(), dir.Close())
	}
	return syncErr
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

func deleteProfileKeychainCredentialsContext(ctx context.Context, instancePaths []string) error {
	for _, instancePath := range instancePaths {
		if err := deleteKeychainCredentialContext(ctx, instancePath); err != nil {
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
	if err := rollbackStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents); err != nil {
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

// ProfileInventoryCount returns the durable number of registered profiles and
// reports malformed or unreadable registry state. Routing can skip one broken
// credential, but capacity enforcement must not silently erase every profile
// when the registry itself cannot be inspected.
func (s Store) ProfileInventoryCount() (int, error) {
	body, err := readFileForAtomicReplace(s.ProfilesPath())
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var data profilesFile
	if err := json.Unmarshal(body, &data); err != nil {
		return 0, err
	}
	return len(data.Profiles), nil
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
	return reportCommittedProfileRegistryWrite("set active profile", s.writeProfiles(data))
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
	if err := s.ensureClaudeProfileInstanceDirAvailable(data, name, dir); err != nil {
		return "", err
	}
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
	return instancePath, reportCommittedProfileRegistryWrite("create profile", s.writeProfiles(data))
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
	if err := s.ensureClaudeProfileInstanceDirAvailable(data, name, dir); err != nil {
		return err
	}
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
	return reportCommittedProfileRegistryWrite("register profile", s.writeProfiles(data))
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
	if err := s.ensureClaudeProfileInstanceDirAvailable(data, name, dir); err != nil {
		return err
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
	return s.RemoveProfileContext(context.Background(), name)
}

func (s Store) RemoveProfileContext(ctx context.Context, name string) (removed bool, err error) {
	lock, err := lockProfileRegistryContext(ctx, s.ProfilesPath())
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
	staged, err := s.stageProfileInstancePathsValidated(ctx, instancePaths)
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
	// This is the final observable filesystem boundary before the atomic
	// registry replacement. A non-cooperating writer can always race the tiny
	// syscall gap, but every reappearance visible before it fails closed.
	if err := s.validateStagedProfileCommitBoundary(staged); err != nil {
		return false, err
	}
	if writeErr := s.writeProfiles(data); writeErr != nil {
		if profileRegistryWriteCommitted(writeErr) {
			// The registry deletion is already visible. Roll forward so a
			// returned removed=true never leaves an invisible staged secret.
			cleanupErr := errors.Join(
				deleteProfileKeychainCredentialsContext(ctx, instancePaths),
				deleteStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents),
			)
			if cleanupErr != nil {
				return true, errors.Join(writeErr, cleanupErr)
			}
			slog.Warn("Claude profile removal is visible but registry directory durability is uncertain", "profile", name, "error", writeErr)
			return true, nil
		}
		return false, errors.Join(writeErr, rollbackStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents))
	}
	if err := deleteProfileKeychainCredentialsContext(ctx, instancePaths); err != nil {
		// The caller's deadline may be the reason cleanup failed. Rollback must
		// have its own bounded lifetime so it can restore the credential and
		// registry atomically instead of reusing an already-canceled context.
		rollbackCtx, rollbackCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		rollbackErr := s.rollbackProfileRemoval(rollbackCtx, original, staged, credentialBackups)
		rollbackCancel()
		if rollbackErr == nil || profileRegistryWriteCommitted(rollbackErr) {
			return false, errors.Join(err, rollbackErr)
		}
		slog.Error(
			"Claude profile removal cleanup failed and rollback was incomplete; profile remains removed",
			"profile", name,
			"cleanup_error", err,
			"rollback_error", rollbackErr,
		)
		return true, errors.Join(err, fmt.Errorf("rollback Claude profile removal: %w", rollbackErr))
	}
	if err := deleteStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents); err != nil {
		slog.Warn(
			"Claude profile removed with staged credential cleanup pending",
			"profile", name,
			"error", err,
		)
		return true, err
	}
	return true, nil
}

// RemoveUnpublishedProfileContext removes a profile whose credential was
// authenticated after the last successfully published account generation.
// Unlike ordinary profile removal, cleanup failure must never restore this
// profile: a later unrelated generation could then publish a credential that
// no worker was told existed. The registry and canonical instance paths are
// removed first while their cross-process locks are held. Keychain and staged
// directory cleanup are best effort after that commit and any failure is
// returned with removed=true.
//
// Callers must hold the account-disk transaction across this removal and the
// subsequent publication of the credential-less generation.
func (s Store) RemoveUnpublishedProfileContext(ctx context.Context, name string) (removed bool, err error) {
	lock, err := lockProfileRegistryContext(ctx, s.ProfilesPath())
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
	credentialLocks, err := lockProfileCredentialPaths(ctx, instancePaths)
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := closeProfileCredentialLocks(credentialLocks); err == nil {
			err = closeErr
		}
	}()

	staged, err := s.stageProfileInstancePathsValidated(ctx, instancePaths)
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
	if err := s.validateStagedProfileCommitBoundary(staged); err != nil {
		return false, err
	}
	if writeErr := s.writeProfiles(data); writeErr != nil {
		if profileRegistryWriteCommitted(writeErr) {
			cleanupErr := errors.Join(
				deleteProfileKeychainCredentialsContext(ctx, instancePaths),
				deleteStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents),
			)
			return true, errors.Join(writeErr, cleanupErr)
		}
		return false, errors.Join(writeErr, rollbackStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents))
	}

	// The removal is committed at this point. In particular, do not restore the
	// registry or staged credential when Keychain cleanup fails.
	cleanupErr := errors.Join(
		deleteProfileKeychainCredentialsContext(ctx, instancePaths),
		deleteStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents),
	)
	if cleanupErr != nil {
		return true, cleanupErr
	}
	return true, nil
}

// PrepareUnpublishedProfileRemovalContext validates one profile under the
// registry lock and invokes prepare with its immutable, normalized instance
// directory. The callback is intended to durably journal that identity before
// the registry lock is released.
func (s Store) PrepareUnpublishedProfileRemovalContext(
	ctx context.Context,
	name string,
	prepare func(instanceDir string) error,
) (found bool, err error) {
	if err := ValidateProfileNameAllowEmail(name); err != nil {
		return false, err
	}
	lock, err := lockProfileRegistryContext(ctx, s.ProfilesPath())
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			found = false
			err = errors.Join(err, closeErr)
		}
	}()
	body, err := readFileForAtomicReplace(s.ProfilesPath())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var data profilesFile
	if err := json.Unmarshal(body, &data); err != nil {
		return false, fmt.Errorf("decode Claude profile registry: %w", err)
	}
	profile, found := data.Profiles[name]
	if !found {
		return false, nil
	}
	if profile.Name != name {
		return false, fmt.Errorf("Claude profile %q has mismatched registry identity %q", name, profile.Name)
	}
	dir := profile.Dir
	if dir == "" {
		dir = sanitizeName(name)
	}
	if !safeProfileDir(dir) {
		return false, errors.New("Claude profile directory is invalid")
	}
	return true, prepare(dir)
}

// SnapshotProfileRemovalContext returns one exact profile/credential identity
// while holding the registry and all path-keyed credential locks. A caller can
// compare snapshots before and after joining a broader account transaction
// without ever retaining secret material.
func (s Store) SnapshotProfileRemovalContext(ctx context.Context, name string) (snapshot ProfileRemovalSnapshot, found bool, err error) {
	return s.prepareExactProfileRemovalContext(ctx, name, nil)
}

// PrepareExactProfileRemovalContext snapshots one exact profile and invokes
// prepare under the same registry and credential locks. This makes the durable
// journal the final operation before those locks are released.
func (s Store) PrepareExactProfileRemovalContext(
	ctx context.Context,
	name string,
	prepare func(snapshot ProfileRemovalSnapshot) error,
) (found bool, err error) {
	_, found, err = s.prepareExactProfileRemovalContext(ctx, name, prepare)
	return found, err
}

func (s Store) prepareExactProfileRemovalContext(
	ctx context.Context,
	name string,
	prepare func(snapshot ProfileRemovalSnapshot) error,
) (snapshot ProfileRemovalSnapshot, found bool, err error) {
	if err := ValidateProfileNameAllowEmail(name); err != nil {
		return snapshot, false, err
	}
	lock, err := lockProfileRegistryContext(ctx, s.ProfilesPath())
	if err != nil {
		return snapshot, false, err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			found = false
			err = errors.Join(err, closeErr)
		}
	}()
	body, err := readFileForAtomicReplace(s.ProfilesPath())
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, false, nil
	}
	if err != nil {
		return snapshot, false, err
	}
	var data profilesFile
	if err := json.Unmarshal(body, &data); err != nil {
		return snapshot, false, fmt.Errorf("decode Claude profile registry: %w", err)
	}
	profile, found := data.Profiles[name]
	if !found {
		return snapshot, false, nil
	}
	if profile.Name != name {
		return snapshot, false, fmt.Errorf("Claude profile %q has mismatched registry identity %q", name, profile.Name)
	}
	dir := profile.Dir
	if dir == "" {
		dir = sanitizeName(name)
	}
	if !safeProfileDir(dir) {
		return snapshot, false, errors.New("Claude profile directory is invalid")
	}
	instancePaths, err := s.profileInstancePaths(dir)
	if err != nil {
		return snapshot, false, err
	}
	credentialLocks, err := lockProfileCredentialPaths(ctx, instancePaths)
	if err != nil {
		return snapshot, false, err
	}
	defer func() {
		if closeErr := closeProfileCredentialLocks(credentialLocks); closeErr != nil {
			found = false
			err = errors.Join(err, closeErr)
		}
	}()
	credentialVersion, err := s.profileCredentialVersionLocked(ctx, instancePaths, false)
	if err != nil {
		return snapshot, false, err
	}
	snapshot = ProfileRemovalSnapshot{InstanceDir: dir, CredentialVersion: credentialVersion}
	if prepare == nil {
		return snapshot, true, nil
	}
	return snapshot, true, prepare(snapshot)
}

// CompleteUnpublishedProfileRemovalContext idempotently completes removal of
// one exact journaled profile identity. It cleans the path-keyed Keychain
// credential and every staged .remove-* directory even when registry removal
// committed before a crash. A registry entry with the same name but a changed
// instance directory is never removed.
func (s Store) CompleteUnpublishedProfileRemovalContext(
	ctx context.Context,
	name string,
	expectedInstanceDir string,
) (completed bool, err error) {
	return s.CompleteExactProfileRemovalContext(ctx, name, ProfileRemovalSnapshot{InstanceDir: expectedInstanceDir})
}

// CompleteExactProfileRemovalContext idempotently completes a journaled
// removal. While the registry entry still exists, both its instance identity
// and actual credential fingerprint must match. Once registry removal has
// committed, replay only cleans the exact journaled paths and orphan stages.
func (s Store) CompleteExactProfileRemovalContext(
	ctx context.Context,
	name string,
	expected ProfileRemovalSnapshot,
) (completed bool, err error) {
	if err := ValidateProfileNameAllowEmail(name); err != nil {
		return false, err
	}
	if !safeProfileDir(expected.InstanceDir) {
		return false, errors.New("Claude profile rollback instance directory is invalid")
	}
	lock, err := lockProfileRegistryContext(ctx, s.ProfilesPath())
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			completed = false
			err = errors.Join(err, closeErr)
		}
	}()
	body, err := readFileForAtomicReplace(s.ProfilesPath())
	if err != nil {
		return false, err
	}
	var data profilesFile
	if err := json.Unmarshal(body, &data); err != nil {
		return false, fmt.Errorf("decode Claude profile registry: %w", err)
	}
	if data.Profiles == nil {
		return false, errors.New("Claude profile registry has no profiles map")
	}
	if err := s.ensureClaudeProfileInstanceDirAvailable(data, name, expected.InstanceDir); err != nil {
		return false, fmt.Errorf("refuse Claude rollback cleanup: %w", err)
	}
	profile, found := data.Profiles[name]
	var registryCommitErr error
	if found {
		if profile.Name != name {
			return false, fmt.Errorf("Claude profile %q has mismatched registry identity %q", name, profile.Name)
		}
		actualDir := profile.Dir
		if actualDir == "" {
			actualDir = sanitizeName(name)
		}
		if actualDir != expected.InstanceDir {
			return false, fmt.Errorf("Claude profile %q instance directory changed from %q to %q", name, expected.InstanceDir, actualDir)
		}
	}
	instancePaths, err := s.profileInstancePaths(expected.InstanceDir)
	if err != nil {
		return false, err
	}
	credentialLocks, err := lockProfileCredentialPaths(ctx, instancePaths)
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := closeProfileCredentialLocks(credentialLocks); closeErr != nil {
			completed = false
			err = errors.Join(err, closeErr)
		}
	}()
	// Validate every stage-looking candidate before moving or deleting a live
	// credential, including legacy journals that did not record a fingerprint.
	// Discovery is intentionally fail-closed on anything without exact owned
	// provenance.
	for _, instancePath := range instancePaths {
		if _, err := stagedProfileInstanceRoots(instancePath); err != nil {
			return false, err
		}
	}
	if !found {
		// A prior attempt may have renamed the registry and failed only its
		// directory fsync. Replay must prove that boundary before completing.
		if syncErr := s.syncProfilesDirectory(); syncErr != nil {
			return false, syncErr
		}
	}
	if expected.CredentialVersion != "" {
		credentialVersion, versionErr := s.profileCredentialVersionLocked(ctx, instancePaths, true)
		if versionErr != nil {
			return false, versionErr
		}
		// Once the registry and every credential payload are absent, replay is
		// already past the destructive boundary. The empty-set fingerprint is a
		// snapshot identity, not a replacement credential.
		emptyVersion, emptyErr := absentProfileCredentialVersion(instancePaths)
		if emptyErr != nil {
			return false, emptyErr
		}
		alreadyRemoved := !found && credentialVersion == emptyVersion
		if credentialVersion != expected.CredentialVersion && !alreadyRemoved {
			if cleanupErr := cleanupExactStagedProfileCredentialLockedWithSync(instancePaths, expected.CredentialVersion, s.syncProfileRemovalParents); cleanupErr != nil {
				// Keep the journal fail-closed when its staged secret cannot be
				// identified or durably removed. Reconciliation must not clear it.
				return false, cleanupErr
			}
			return false, fmt.Errorf("%w: %q", ErrProfileRemovalCredentialChanged, name)
		}
	}
	staged, err := s.stageProfileInstancePathsValidated(ctx, instancePaths)
	if err != nil {
		return false, err
	}
	if found {
		delete(data.Profiles, name)
		if data.Active == name {
			data.Active = ""
			for remaining := range data.Profiles {
				data.Active = remaining
				break
			}
		}
		if err := s.validateStagedProfileCommitBoundary(staged); err != nil {
			return false, err
		}
		if writeErr := s.writeProfiles(data); writeErr != nil {
			if !profileRegistryWriteCommitted(writeErr) {
				return false, errors.Join(writeErr, rollbackStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents))
			}
			registryCommitErr = writeErr
		}
	} else if err := s.validateStagedProfileCommitBoundary(staged); err != nil {
		return false, err
	}
	cleanupErr := errors.Join(
		deleteProfileKeychainCredentialsContext(ctx, instancePaths),
		deleteStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents),
		deleteOrphanedStagedProfileInstancesWithSync(instancePaths, s.syncProfileRemovalParents),
	)
	if cleanupErr != nil {
		return false, errors.Join(registryCommitErr, cleanupErr)
	}
	if registryCommitErr != nil {
		return false, registryCommitErr
	}
	return true, nil
}

func (s Store) profileCredentialVersionLocked(ctx context.Context, instancePaths []string, includeStaged bool) (string, error) {
	return s.profileCredentialVersionLockedRequiringAbsent(ctx, instancePaths, includeStaged, nil)
}

func (s Store) profileCredentialVersionLockedRequiringAbsent(
	ctx context.Context,
	instancePaths []string,
	includeStaged bool,
	requireAbsent []stagedProfileInstance,
) (string, error) {
	requiredAbsent := make(map[string]struct{}, len(requireAbsent))
	for _, entry := range requireAbsent {
		normalized, err := normalizedProfileRemovalPath(entry.originalPath)
		if err != nil {
			return "", err
		}
		requiredAbsent[normalized] = struct{}{}
	}
	entries := make([]profileCredentialVersionEntry, 0, len(instancePaths))
	for _, instancePath := range instancePaths {
		normalizedPath, err := normalizedProfileRemovalPath(instancePath)
		if err != nil {
			return "", err
		}
		entry := profileCredentialVersionEntry{originalPath: normalizedPath}
		if info, statErr := os.Lstat(instancePath); statErr == nil {
			if _, mustBeAbsent := requiredAbsent[normalizedPath]; mustBeAbsent {
				return "", fmt.Errorf("%w: staged Claude profile path %q reappeared during credential traversal", ErrProfileRemovalCredentialChanged, instancePath)
			}
			entry.instancePresent = true
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", fmt.Errorf("Claude profile instance %q is not a regular directory", instancePath)
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		if includeStaged {
			// Validate every stage candidate even when a live credential wins the
			// version comparison. Otherwise an unowned lookalike can be discovered
			// only after the live credential has already been staged for deletion.
			roots, listErr := stagedProfileInstanceRoots(instancePath)
			if listErr != nil {
				return "", listErr
			}
			// Once deletion has staged the live directory away, a stale path-keyed
			// Keychain item must not mask the exact file payload owned by the
			// journal. Prefer a live file, then its staged replacement, and only
			// consult Keychain when neither exists.
			livePayload, liveErr := os.ReadFile(filepath.Join(instancePath, ".credentials.json"))
			if liveErr == nil {
				entry.payloadPresent = true
				entry.payload = livePayload
				entries = append(entries, entry)
				continue
			}
			if !errors.Is(liveErr, os.ErrNotExist) {
				return "", liveErr
			}
			// A live directory, even one without a credential file, is a replacement
			// boundary. Never substitute an older staged payload at the same path.
			if !entry.instancePresent {
				var actualStages []stagedProfileInstance
				for _, root := range roots {
					owned, readErr := readOwnedProfileRemovalStage(root, instancePath)
					if readErr != nil {
						return "", readErr
					}
					if !owned.prepared {
						actualStages = append(actualStages, owned)
					}
				}
				if len(actualStages) > 1 {
					return "", fmt.Errorf("Claude profile credential has multiple stages for %q", instancePath)
				}
				if len(actualStages) == 1 {
					entry.instancePresent = true
					stagedPayload, readErr := os.ReadFile(filepath.Join(actualStages[0].stagedPath, ".credentials.json"))
					if readErr == nil {
						entry.payloadPresent = true
						entry.payload = stagedPayload
						entries = append(entries, entry)
						continue
					}
					if !errors.Is(readErr, os.ErrNotExist) {
						return "", readErr
					}
				}
			}
		}
		payload, found, err := s.readCredentialPayloadLocked(ctx, instancePath)
		if err != nil {
			return "", err
		}
		if found {
			entry.payloadPresent = true
			entry.payload = payload
		}
		entries = append(entries, entry)
	}
	return profileCredentialEntriesVersion(entries)
}

type profileCredentialVersionEntry struct {
	originalPath    string
	instancePresent bool
	payloadPresent  bool
	payload         []byte
}

func filesystemProfileCredentialVersion(instancePaths []string) (string, error) {
	entries := make([]profileCredentialVersionEntry, 0, len(instancePaths))
	for _, instancePath := range instancePaths {
		entry := profileCredentialVersionEntry{originalPath: instancePath}
		info, err := os.Lstat(instancePath)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", fmt.Errorf("Claude profile instance %q is not a regular directory", instancePath)
			}
			entry.instancePresent = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		payload, err := os.ReadFile(filepath.Join(instancePath, ".credentials.json"))
		if err == nil {
			entry.payloadPresent = true
			entry.payload = payload
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		entries = append(entries, entry)
	}
	return profileCredentialEntriesVersion(entries)
}

func profileCredentialEntriesVersion(entries []profileCredentialVersionEntry) (string, error) {
	ordered := append([]profileCredentialVersionEntry(nil), entries...)
	for index := range ordered {
		normalizedPath, err := normalizedProfileRemovalPath(ordered[index].originalPath)
		if err != nil {
			return "", err
		}
		ordered[index].originalPath = normalizedPath
	}
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].originalPath < ordered[right].originalPath })
	hash := sha256.New()
	_, _ = hash.Write([]byte("subrouter-claude-profile-credential-v2\x00"))
	var priorPath string
	for index, entry := range ordered {
		if index != 0 && entry.originalPath == priorPath {
			return "", fmt.Errorf("duplicate Claude profile credential path %q", entry.originalPath)
		}
		priorPath = entry.originalPath
		writeCredentialVersionFrame(hash, []byte(entry.originalPath))
		flags := byte(0)
		if entry.instancePresent {
			flags |= 1
		}
		if entry.payloadPresent {
			flags |= 2
		}
		_, _ = hash.Write([]byte{flags})
		payload := []byte(nil)
		if entry.payloadPresent {
			payload = bytes.TrimSpace(entry.payload)
		}
		writeCredentialVersionFrame(hash, payload)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeCredentialVersionFrame(writer io.Writer, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = writer.Write(value)
}

func absentProfileCredentialVersion(instancePaths []string) (string, error) {
	entries := make([]profileCredentialVersionEntry, 0, len(instancePaths))
	for _, path := range instancePaths {
		entries = append(entries, profileCredentialVersionEntry{originalPath: path})
	}
	return profileCredentialEntriesVersion(entries)
}

func cleanupExactStagedProfileCredentialLocked(instancePaths []string, expectedVersion string) error {
	return cleanupExactStagedProfileCredentialLockedWithSync(instancePaths, expectedVersion, syncProfileRemovalParents)
}

func cleanupExactStagedProfileCredentialLockedWithSync(
	instancePaths []string,
	expectedVersion string,
	syncParents func(map[string]struct{}) error,
) error {
	type candidate struct {
		entry          stagedProfileInstance
		payloadPresent bool
		payload        []byte
	}
	var candidates []candidate
	var prepared []stagedProfileInstance
	for _, instancePath := range instancePaths {
		roots, err := stagedProfileInstanceRoots(instancePath)
		if err != nil {
			return err
		}
		for _, root := range roots {
			owned, err := readOwnedProfileRemovalStage(root, instancePath)
			if err != nil {
				return err
			}
			if owned.prepared {
				prepared = append(prepared, owned)
				continue
			}
			stagedPath := owned.stagedPath
			payload, err := os.ReadFile(filepath.Join(stagedPath, ".credentials.json"))
			payloadPresent := err == nil
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			candidates = append(candidates, candidate{
				entry:          owned,
				payloadPresent: payloadPresent,
				payload:        payload,
			})
		}
	}
	var exact []stagedProfileInstance
	if len(candidates) == 0 {
		if len(prepared) != 0 {
			return deletePreparedProfileStagesWithSync(prepared, syncParents)
		}
		parents := make(map[string]struct{}, len(instancePaths))
		for _, instancePath := range instancePaths {
			// A prior attempt may have removed the exact stage and failed only
			// its parent fsync. Replay must re-prove that durability boundary
			// before allowing the journal to clear.
			parents[filepath.Dir(instancePath)] = struct{}{}
		}
		return syncParents(parents)
	}
	for _, candidate := range candidates {
		candidateVersion, err := stagedProfileCredentialVersion(instancePaths, []profileCredentialVersionEntry{{
			originalPath:    candidate.entry.originalPath,
			instancePresent: true,
			payloadPresent:  candidate.payloadPresent,
			payload:         candidate.payload,
		}})
		if err != nil {
			return err
		}
		if candidateVersion == expectedVersion {
			exact = append(exact, candidate.entry)
		}
	}
	if len(exact) == 1 {
		if err := deleteStagedProfileInstancesWithSync(exact, syncParents); err != nil {
			return err
		}
		return deletePreparedProfileStagesWithSync(prepared, syncParents)
	}
	if len(exact) > 1 {
		return errors.New("Claude profile removal staged credential is ambiguous")
	}
	if len(candidates) > 1 && len(candidates) <= len(instancePaths) {
		versionEntries := make([]profileCredentialVersionEntry, 0, len(candidates))
		entries := make([]stagedProfileInstance, 0, len(candidates))
		seenPaths := make(map[string]struct{}, len(candidates))
		for _, candidate := range candidates {
			key := profileInstancePathKey(candidate.entry.originalPath)
			if _, duplicate := seenPaths[key]; duplicate {
				return errors.New("Claude profile removal staged credential is ambiguous")
			}
			seenPaths[key] = struct{}{}
			versionEntries = append(versionEntries, profileCredentialVersionEntry{
				originalPath:    candidate.entry.originalPath,
				instancePresent: true,
				payloadPresent:  candidate.payloadPresent,
				payload:         candidate.payload,
			})
			entries = append(entries, candidate.entry)
		}
		candidateVersion, err := stagedProfileCredentialVersion(instancePaths, versionEntries)
		if err != nil {
			return err
		}
		if candidateVersion == expectedVersion {
			if err := deleteStagedProfileInstancesWithSync(entries, syncParents); err != nil {
				return err
			}
			return deletePreparedProfileStagesWithSync(prepared, syncParents)
		}
	}
	return errors.Join(
		errors.New("Claude profile removal staged credential does not match its journal"),
		deletePreparedProfileStagesWithSync(prepared, syncParents),
	)
}

func stagedProfileCredentialVersion(
	instancePaths []string,
	staged []profileCredentialVersionEntry,
) (string, error) {
	byPath := make(map[string]profileCredentialVersionEntry, len(staged))
	for _, entry := range staged {
		normalized, err := normalizedProfileRemovalPath(entry.originalPath)
		if err != nil {
			return "", err
		}
		if _, duplicate := byPath[normalized]; duplicate {
			return "", fmt.Errorf("duplicate staged Claude profile credential path %q", normalized)
		}
		entry.originalPath = normalized
		byPath[normalized] = entry
	}
	entries := make([]profileCredentialVersionEntry, 0, len(instancePaths))
	for _, path := range instancePaths {
		normalized, err := normalizedProfileRemovalPath(path)
		if err != nil {
			return "", err
		}
		if entry, found := byPath[normalized]; found {
			entries = append(entries, entry)
			delete(byPath, normalized)
		} else {
			entries = append(entries, profileCredentialVersionEntry{originalPath: normalized})
		}
	}
	if len(byPath) != 0 {
		return "", errors.New("staged Claude credential path is outside the exact instance set")
	}
	return profileCredentialEntriesVersion(entries)
}

func (s Store) readCredentialPayloadLocked(ctx context.Context, instancePath string) ([]byte, bool, error) {
	body, err := os.ReadFile(filepath.Join(instancePath, ".credentials.json"))
	if err == nil {
		return body, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	if runtime.GOOS != "darwin" {
		return nil, false, nil
	}
	u, err := user.Current()
	if err != nil {
		return nil, false, fmt.Errorf("resolve current user for Claude Keychain lookup: %w", err)
	}
	service := "Claude Code-credentials-" + keychainHash(instancePath)
	keychainCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(keychainCtx, "security", "find-generic-password", "-s", service, "-a", u.Username, "-w")
	body, err = cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 44 {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read Claude credential from keychain: %w", err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, false, nil
	}
	return body, true, nil
}

func (s Store) CleanupInstance(dir string) error {
	return s.CleanupInstanceContext(context.Background(), dir)
}

func (s Store) CleanupInstanceContext(ctx context.Context, dir string) error {
	if dir == "" {
		return nil
	}
	instancePaths, err := s.profileInstancePaths(dir)
	if err != nil {
		return err
	}
	credentialLocks, err := lockProfileCredentialPaths(ctx, instancePaths)
	if err != nil {
		return err
	}
	defer closeProfileCredentialLocks(credentialLocks)
	for _, instancePath := range instancePaths {
		if err := deleteKeychainCredentialContext(ctx, instancePath); err != nil {
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
	committed, err := writePrivateFileAtomicWithDirectorySync(s.ProfilesPath(), body, s.syncDirectoryForTest)
	if committed && err != nil {
		return &profileRegistryWriteCommittedError{cause: err}
	}
	return err
}

func (s Store) syncProfilesDirectory() error {
	if s.syncDirectoryForTest != nil {
		return s.syncDirectoryForTest(filepath.Dir(s.ProfilesPath()))
	}
	return syncPrivateFileDirectoryForOS(runtime.GOOS, filepath.Dir(s.ProfilesPath()), os.Open)
}

func safeProfileDir(dir string) bool {
	return dir != "" &&
		dir != "." &&
		!filepath.IsAbs(dir) &&
		filepath.VolumeName(dir) == "" &&
		filepath.Clean(dir) == dir &&
		filepath.Base(dir) == dir
}

func resolvedClaudeProfileInstanceDir(name string, profile Profile) string {
	if profile.Dir != "" {
		return profile.Dir
	}
	return sanitizeName(name)
}

func (s Store) ensureClaudeProfileInstanceDirAvailable(data profilesFile, targetName, targetDir string) error {
	targetPaths, err := s.profileInstancePaths(targetDir)
	if err != nil {
		return err
	}
	for name, profile := range data.Profiles {
		if name == targetName {
			continue
		}
		otherDir := resolvedClaudeProfileInstanceDir(name, profile)
		otherPaths, err := s.profileInstancePaths(otherDir)
		if err != nil {
			return fmt.Errorf("resolve Claude profile %q instance directory: %w", name, err)
		}
		for _, targetPath := range targetPaths {
			for _, otherPath := range otherPaths {
				aliases, err := profileInstancePathsAliasForOS(runtime.GOOS, targetPath, otherPath)
				if err != nil {
					return fmt.Errorf("compare Claude profile instance directories %q and %q: %w", targetDir, otherDir, err)
				}
				if aliases {
					return fmt.Errorf("Claude profile instance directory %q is already owned by profile %q", targetDir, name)
				}
			}
		}
	}
	return nil
}

func profileInstancePathsAliasForOS(goos, first, second string) (bool, error) {
	firstAbs, err := filepath.Abs(filepath.Clean(first))
	if err != nil {
		return false, err
	}
	secondAbs, err := filepath.Abs(filepath.Clean(second))
	if err != nil {
		return false, err
	}
	if firstAbs == secondAbs {
		return true, nil
	}
	firstInfo, firstStatErr := os.Stat(firstAbs)
	secondInfo, secondStatErr := os.Stat(secondAbs)
	if firstStatErr == nil && secondStatErr == nil && os.SameFile(firstInfo, secondInfo) {
		return true, nil
	}
	for _, statErr := range []error{firstStatErr, secondStatErr} {
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return false, statErr
		}
	}
	firstIdentity, err := profileInstancePhysicalIdentity(firstAbs)
	if err != nil {
		return false, err
	}
	secondIdentity, err := profileInstancePhysicalIdentity(secondAbs)
	if err != nil {
		return false, err
	}
	if firstIdentity == secondIdentity {
		return true, nil
	}
	if goos == "darwin" || goos == "windows" {
		return strings.EqualFold(firstIdentity, secondIdentity), nil
	}
	return false, nil
}

func profileInstancePhysicalIdentity(path string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Abs(filepath.Clean(resolved))
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("unresolved Claude profile instance symlink %q", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if resolvedParent, err := filepath.EvalSymlinks(parent); err == nil {
		return filepath.Abs(filepath.Join(resolvedParent, filepath.Base(path)))
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return filepath.Abs(filepath.Clean(path))
}

func writePrivateFileAtomic(path string, body []byte) error {
	_, err := writePrivateFileAtomicWithDirectorySync(path, body, nil)
	return err
}

func writePrivateFileAtomicWithDirectorySync(path string, body []byte, syncDirectory func(string) error) (committed bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, err
	}
	tempPath := file.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return false, err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return false, err
	}
	if syncDirectory != nil {
		return true, syncDirectory(filepath.Dir(path))
	}
	return true, syncPrivateFileDirectoryForOS(runtime.GOOS, filepath.Dir(path), os.Open)
}

func syncPrivateFileDirectoryForOS(goos, path string, openDir func(string) (*os.File, error)) error {
	// Windows has no os.File directory-sync primitive. Atomic replacement is
	// the explicit platform durability boundary, matching other store writers.
	if goos == "windows" {
		return nil
	}
	dir, err := openDir(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
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

func (s Store) prepareSharedState(instancePath string) (err error) {
	if strings.TrimSpace(s.SharedStateDir) == "" {
		return nil
	}
	if err := os.MkdirAll(s.SharedStateDir, 0o700); err != nil {
		return err
	}
	sharedLock, err := lockProfileCredential(
		context.Background(), filepath.Join(s.SharedStateDir, ".subrouter-shared-state-migration"),
	)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, sharedLock.Close()) }()
	profileLock, err := lockProfileCredential(context.Background(), instancePath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, profileLock.Close()) }()

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
	// The shared-state root is created by prepareSharedState, but keep this
	// helper safe for direct callers and first-run migrations as well.
	sourceParent, err := openMigrationDirectoryRoot(filepath.Dir(source), false)
	if err != nil {
		return fmt.Errorf("open profile parent root: %w", err)
	}
	defer sourceParent.Close()
	targetParent, err := openMigrationDirectoryRoot(filepath.Dir(target), true)
	if err != nil {
		return fmt.Errorf("open shared parent root: %w", err)
	}
	defer targetParent.Close()
	sourceName := filepath.Base(source)
	targetName := filepath.Base(target)

	if info, err := sourceParent.Lstat(sourceName); err == nil && info.Mode()&os.ModeSymlink != 0 {
		current, readErr := sourceParent.Readlink(sourceName)
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
			return targetParent.MkdirAll(targetName, 0o700)
		}
		return fmt.Errorf("existing symlink points to %s", current)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := targetParent.MkdirAll(targetName, 0o700); err != nil {
		return err
	}
	if info, err := sourceParent.Lstat(sourceName); err == nil {
		if !info.IsDir() {
			return errors.New("existing profile state is not a directory")
		}
		sourceRoot, err := sourceParent.OpenRoot(sourceName)
		if err != nil {
			return fmt.Errorf("open profile state root: %w", err)
		}
		defer sourceRoot.Close()
		targetRoot, err := targetParent.OpenRoot(targetName)
		if err != nil {
			return fmt.Errorf("open shared state root: %w", err)
		}
		defer targetRoot.Close()
		if err := mergeDirectoryPreservingConflicts(sourceRoot, targetRoot, target); err != nil {
			return err
		}
		if err := removeRootContents(sourceRoot); err != nil {
			return err
		}
		if err := sourceParent.Remove(sourceName); err != nil {
			return fmt.Errorf("remove migrated profile state: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := sourceParent.Symlink(target, sourceName); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		// A launcher outside this process may have published the same link
		// without using Subrouter's lock. Treat only the exact intended link as
		// an idempotent success; every other replacement remains fail-closed.
		info, statErr := sourceParent.Lstat(sourceName)
		if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
			return err
		}
		current, readErr := sourceParent.Readlink(sourceName)
		if readErr != nil {
			return readErr
		}
		currentPath := current
		if !filepath.IsAbs(currentPath) {
			currentPath = filepath.Join(filepath.Dir(source), currentPath)
		}
		currentAbs, currentErr := filepath.Abs(currentPath)
		targetAbs, targetErr := filepath.Abs(target)
		if currentErr == nil && targetErr == nil && currentAbs == targetAbs {
			return nil
		}
		return err
	}
	return nil
}

func openMigrationDirectoryRoot(path string, create bool) (*os.Root, error) {
	path = filepath.Clean(path)
	var suffix []string
	for {
		if _, statErr := os.Lstat(path); statErr == nil {
			resolved, resolveErr := filepath.EvalSymlinks(path)
			if resolveErr != nil {
				return nil, resolveErr
			}
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			path = resolved
			break
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
		parent := filepath.Dir(path)
		if parent == path {
			if !create {
				return nil, os.ErrNotExist
			}
			break
		}
		suffix = append(suffix, filepath.Base(path))
		path = parent
	}
	volume := filepath.VolumeName(path)
	rootPath := volume + string(filepath.Separator)
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(rootPath, path)
	if err != nil {
		root.Close()
		return nil, err
	}
	if relative == "." {
		return root, nil
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		info, statErr := root.Lstat(part)
		if errors.Is(statErr, os.ErrNotExist) && create {
			if statErr = root.Mkdir(part, 0o700); statErr == nil {
				info, statErr = root.Lstat(part)
			}
		}
		if statErr != nil {
			root.Close()
			return nil, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			root.Close()
			return nil, fmt.Errorf("migration parent component %q is not a directory", part)
		}
		next, openErr := root.OpenRoot(part)
		root.Close()
		if openErr != nil {
			return nil, openErr
		}
		root = next
	}
	return root, nil
}

func mergeDirectoryPreservingConflicts(source, target *os.Root, targetAbsolute string) error {
	return mergeRootDirectory(source, target, ".", ".", targetAbsolute)
}

func mergeRootDirectory(source, target *os.Root, sourceRelative, targetRelative, targetAbsolute string) error {
	entries, err := readRootDirectory(source, sourceRelative)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		sourcePath := entry.Name()
		if sourceRelative != "." {
			sourcePath = filepath.Join(sourceRelative, sourcePath)
		}
		destination := entry.Name()
		if targetRelative != "." {
			destination = filepath.Join(targetRelative, destination)
		}
		info, err := source.Lstat(sourcePath)
		if err != nil {
			return err
		}
		isDirectory := info.IsDir()
		if targetInfo, err := target.Lstat(destination); err == nil {
			if !isDirectory || !targetInfo.IsDir() {
				if !isDirectory && info.Mode().IsRegular() && targetInfo.Mode().IsRegular() {
					equal, compareErr := rootFilesEqual(source, target, sourcePath, destination, info)
					if compareErr != nil {
						return compareErr
					}
					if equal {
						current, statErr := source.Lstat(sourcePath)
						if statErr != nil {
							return statErr
						}
						if !os.SameFile(info, current) || info.Size() != current.Size() || !info.ModTime().Equal(current.ModTime()) || info.Mode().Perm() != targetInfo.Mode().Perm() {
						} else {
							if removeErr := source.Remove(sourcePath); removeErr != nil {
								return fmt.Errorf("remove already migrated file %q: %w", sourcePath, removeErr)
							}
							continue
						}
					}
				}
				destination, err = availableRootPath(target, destination)
				if err != nil {
					return err
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if isDirectory {
			if err := target.MkdirAll(destination, 0o700); err != nil {
				return err
			}
			if err := mergeRootDirectory(source, target, sourcePath, destination, targetAbsolute); err != nil {
				return err
			}
			if err := source.Remove(sourcePath); err != nil {
				return fmt.Errorf("remove migrated directory %q: %w", sourcePath, err)
			}
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := source.Readlink(sourcePath)
			if err != nil {
				return err
			}
			if err := target.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return err
			}
			if err := target.Symlink(link, destination); err != nil {
				return err
			}
			if err := source.Remove(sourcePath); err != nil {
				return fmt.Errorf("remove migrated symlink %q: %w", sourcePath, err)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported profile state entry %q", sourcePath)
		}
		if err := copyRootFile(source, target, sourcePath, destination, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

func rootFilesEqual(source, target *os.Root, sourcePath, targetPath string, sourceInfo os.FileInfo) (bool, error) {
	targetInfo, err := target.Stat(targetPath)
	if err != nil {
		return false, err
	}
	if sourceInfo.Size() != targetInfo.Size() {
		return false, nil
	}
	input, err := source.Open(sourcePath)
	if err != nil {
		return false, err
	}
	defer input.Close()
	output, err := target.Open(targetPath)
	if err != nil {
		return false, err
	}
	defer output.Close()
	left := make([]byte, 32*1024)
	right := make([]byte, len(left))
	for {
		leftN, leftErr := input.Read(left)
		rightN, rightErr := output.Read(right)
		if leftN != rightN || !bytes.Equal(left[:leftN], right[:rightN]) {
			return false, nil
		}
		if leftErr == io.EOF || rightErr == io.EOF {
			return leftErr == io.EOF && rightErr == io.EOF, nil
		}
		if leftErr != nil {
			return false, leftErr
		}
		if rightErr != nil {
			return false, rightErr
		}
	}
}

func readRootDirectory(root *os.Root, relative string) ([]os.DirEntry, error) {
	directory, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	return directory.ReadDir(-1)
}

func availableRootPath(root *os.Root, path string) (string, error) {
	for index := 1; ; index++ {
		candidate := fmt.Sprintf("%s.subrouter-legacy-%d", path, index)
		if _, err := root.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
}

func copyRootFile(source, target *os.Root, sourcePath, targetPath string, mode os.FileMode) error {
	if err := target.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return err
	}
	input, err := source.Open(sourcePath)
	if err != nil {
		return err
	}
	defer input.Close()
	before, err := input.Stat()
	if err != nil {
		return err
	}
	output, err := target.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		_ = target.Remove(targetPath)
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		_ = target.Remove(targetPath)
		return err
	}
	if err := setRootFileModTime(output, before.ModTime()); err != nil {
		_ = output.Close()
		_ = target.Remove(targetPath)
		return err
	}
	if err := output.Close(); err != nil {
		_ = target.Remove(targetPath)
		return err
	}
	after, err := input.Stat()
	if err != nil {
		_ = target.Remove(targetPath)
		return err
	}
	named, err := source.Lstat(sourcePath)
	if err != nil {
		_ = target.Remove(targetPath)
		return err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || !os.SameFile(before, after) || !os.SameFile(before, named) {
		_ = target.Remove(targetPath)
		return fmt.Errorf("source file %q changed during migration", sourcePath)
	}
	if err := input.Close(); err != nil {
		_ = target.Remove(targetPath)
		return err
	}
	if err := source.Remove(sourcePath); err != nil {
		_ = target.Remove(targetPath)
		return fmt.Errorf("remove migrated file %q: %w", sourcePath, err)
	}
	return nil
}

func removeRootContents(root *os.Root) error {
	entries, err := readRootDirectory(root, ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := root.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}
	return nil
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

func EnvForConfigDir(instancePath string) []string {
	remove := make(map[string]bool, len(claudeRoutingEnvKeys))
	for _, key := range claudeRoutingEnvKeys {
		remove[strings.ToUpper(key)] = true
	}
	env := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if ok && (remove[strings.ToUpper(key)] || isSubrouterEnvName(key)) {
			continue
		}
		env = append(env, item)
	}
	return append(env, "CLAUDE_CONFIG_DIR="+instancePath)
}

// RoutingEnvKeys returns every known environment selector that can redirect a
// Claude process away from its intended login or gateway.
func RoutingEnvKeys() []string {
	return append([]string(nil), claudeRoutingEnvKeys...)
}

var claudeRoutingEnvKeys = []string{
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_CUSTOM_HEADERS",
	"CLAUDE_CONFIG_DIR",
	"CLAUDE_CODE_CONFIG_DIR",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_API_KEY",
	"CLAUDE_CODE_AUTH_TOKEN",
	"CLAUDE_CODE_BASE_URL",
	"CLAUDE_CODE_USE_BEDROCK",
	"ANTHROPIC_BEDROCK_BASE_URL",
	"CLAUDE_CODE_SKIP_BEDROCK_AUTH",
	"CLAUDE_CODE_USE_VERTEX",
	"ANTHROPIC_VERTEX_BASE_URL",
	"ANTHROPIC_VERTEX_PROJECT",
	"CLAUDE_CODE_SKIP_VERTEX_AUTH",
	"CLAUDE_CODE_USE_FOUNDRY",
	"ANTHROPIC_FOUNDRY_BASE_URL",
	"CLAUDE_CODE_SKIP_FOUNDRY_AUTH",
	"CLAUDE_CODE_USE_MANTLE",
	"ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
	"CLAUDE_CODE_SKIP_MANTLE_AUTH",
	"CLAUDE_CODE_USE_ANTHROPIC_AWS",
	"ANTHROPIC_AWS_BASE_URL",
	"CLAUDE_CODE_SKIP_ANTHROPIC_AWS_AUTH",
	"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
	"ANTHROPIC_GOOGLE_CLOUD_BASE_URL",
	"CLAUDE_CODE_SKIP_ANTHROPIC_GOOGLE_CLOUD_AUTH",
	"CLAUDE_CODE_USE_GATEWAY",
	"ANTHROPIC_GATEWAY_BASE_URL",
	"CLAUDE_CODE_GATEWAY_BASE_URL",
	"CLAUDE_CODE_SKIP_GATEWAY_AUTH",
}

func isSubrouterEnvName(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	return strings.HasPrefix(upper, "SUBROUTER_")
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
	cmd.Env = EnvForConfigDir(instancePath)
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
	account, _, didRefresh, err := s.refreshProfileCredential(ctx, client, profile, false, nil)
	return account, didRefresh, err
}

func (s Store) ForceRefreshCredential(ctx context.Context, client *http.Client, profile Profile) (accounts.Account, bool, error) {
	account, _, didRefresh, err := s.refreshProfileCredential(ctx, client, profile, true, nil)
	return account, didRefresh, err
}

// RefreshCredentialDetailsIfExpired returns the credential snapshot used to
// build the account so status callers do not need a second serialized disk or
// Keychain read merely to obtain subscription metadata.
func (s Store) RefreshCredentialDetailsIfExpired(ctx context.Context, client *http.Client, profile Profile) (accounts.Account, *CredentialInfo, bool, error) {
	return s.refreshProfileCredential(ctx, client, profile, false, nil)
}

// RefreshCredentialDetailsIfExpiredBeforeRefresh calls beforeRefresh only
// after the per-profile refresh lock has been acquired and the credential has
// been re-read as expired. Callers can publish shared disk generation at that
// exact point without publishing when another process already refreshed the
// single-use token.
func (s Store) RefreshCredentialDetailsIfExpiredBeforeRefresh(
	ctx context.Context,
	client *http.Client,
	profile Profile,
	beforeRefresh func() error,
) (accounts.Account, *CredentialInfo, bool, error) {
	return s.refreshProfileCredential(ctx, client, profile, false, beforeRefresh)
}

// CredentialRefreshState returns one current credential snapshot and whether
// status should enter the account-disk publication transaction to refresh it.
// The refresh path rechecks this state under its per-profile refresh lock, so a
// concurrent rotation between this preflight and refresh remains harmless.
func (s Store) CredentialRefreshState(ctx context.Context, profile Profile, now time.Time) (accounts.Account, *CredentialInfo, bool, error) {
	configDir := s.ClaudeConfigDir(profile.Name)
	current, ok := s.FindProfile(profile.Name)
	if !ok || profileInstancePathKey(s.ClaudeConfigDir(current.Name)) != profileInstancePathKey(configDir) {
		return accounts.Account{}, nil, false, fmt.Errorf("Claude profile %q is no longer current", profile.Name)
	}
	credential, err := s.ReadCredential(ctx, configDir)
	if err != nil {
		return accounts.Account{}, nil, false, err
	}
	if credential == nil || credential.AccessToken == "" {
		return accounts.Account{}, credential, false, fmt.Errorf("Claude profile %q has no access token", profile.Name)
	}
	account, ok := profileAccount(current, configDir, credential)
	if !ok {
		return accounts.Account{}, credential, false, fmt.Errorf("Claude profile %q has no usable credential", profile.Name)
	}
	needsRefresh := credential.RefreshToken != "" && credentialExpiredAt(credential, 60*time.Second, now)
	return account, credential, needsRefresh, nil
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
func (s Store) refreshProfileCredential(ctx context.Context, client *http.Client, profile Profile, force bool, beforeRefresh func() error) (account accounts.Account, credential *CredentialInfo, didRefresh bool, err error) {
	configDir := s.ClaudeConfigDir(profile.Name)
	refreshLock, err := lockProfileRefresh(ctx, configDir)
	if err != nil {
		return accounts.Account{}, nil, false, err
	}
	defer func() {
		if closeErr := refreshLock.Close(); err == nil {
			err = closeErr
		}
	}()

	current, ok := s.FindProfile(profile.Name)
	if !ok || profileInstancePathKey(s.ClaudeConfigDir(current.Name)) != profileInstancePathKey(configDir) {
		return accounts.Account{}, nil, false, fmt.Errorf("Claude profile %q is no longer current", profile.Name)
	}
	profile = current
	credential, err = s.ReadCredential(ctx, configDir)
	if err != nil {
		return accounts.Account{}, nil, false, err
	}
	if credential == nil || credential.AccessToken == "" {
		return accounts.Account{}, credential, false, fmt.Errorf("Claude profile %q has no access token", profile.Name)
	}
	if force && credential.RefreshToken == "" {
		return accounts.Account{}, credential, false, fmt.Errorf("Claude profile %q has no refresh token", profile.Name)
	}
	shouldRefresh := credential.RefreshToken != "" &&
		(force || credentialExpired(credential, 60*time.Second))
	if !shouldRefresh {
		account, ok = profileAccount(profile, configDir, credential)
		if !ok {
			return accounts.Account{}, credential, false, fmt.Errorf("Claude profile %q has no usable credential", profile.Name)
		}
		return account, credential, false, nil
	}

	credentialBeforeRefresh := *credential
	profileBeforeRefresh := profile
	if beforeRefresh != nil {
		if err := beforeRefresh(); err != nil {
			return accounts.Account{}, credential, false, err
		}
	}
	refreshed, err := RefreshCredential(ctx, client, credentialBeforeRefresh)
	if err != nil {
		return accounts.Account{}, credential, false, err
	}
	didRefresh = true

	current, ok = s.FindProfile(profile.Name)
	if !ok ||
		current.CreatedAt != profileBeforeRefresh.CreatedAt ||
		profileInstancePathKey(s.ClaudeConfigDir(current.Name)) != profileInstancePathKey(configDir) {
		return accounts.Account{}, credential, false, fmt.Errorf("Claude profile %q is no longer current", profile.Name)
	}
	profile = current
	credential, err = s.writeRefreshedCredentialIfUnchanged(ctx, configDir, credentialBeforeRefresh, refreshed)
	if err != nil {
		return accounts.Account{}, credential, false, err
	}
	if credential == nil || credential.AccessToken == "" {
		return accounts.Account{}, credential, false, fmt.Errorf("Claude profile %q has no access token", profile.Name)
	}
	account, ok = profileAccount(profile, configDir, credential)
	if !ok {
		return accounts.Account{}, credential, false, fmt.Errorf("Claude profile %q has no usable credential", profile.Name)
	}
	return account, credential, didRefresh, nil
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
	// Tenant uploads have the same storage and concurrency requirements as a
	// local import: a collision-resistant directory for a new label, the
	// existing registered directory for a repair, and the profile credential
	// lock so a concurrent OAuth refresh cannot observe a partial replacement.
	importErr := s.ImportProfileCredential(name, credential)
	if importErr != nil && !profileRegistryWriteCommitted(importErr) {
		return Profile{}, importErr
	}
	profile, ok := s.FindProfile(name)
	if !ok {
		return Profile{}, fmt.Errorf("Claude profile %q was not readable after registration", name)
	}
	return profile, importErr
}

func deleteKeychainCredential(instancePath string) error {
	return deleteKeychainCredentialContext(context.Background(), instancePath)
}

func deleteKeychainCredentialContext(ctx context.Context, instancePath string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	service := "Claude Code-credentials-" + keychainHash(instancePath)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
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
	credential, err := decodeCredentialPayload(body)
	if err == nil {
		return credential, nil
	}
	// Claude Code may store the credential as generic-password data instead of
	// a string. On macOS, `security ... -w` renders that data as hexadecimal
	// text. Accept that representation only for keychain reads and only when
	// the complete decoded value is itself a valid credential payload.
	if source == "keychain" {
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) > 0 && len(trimmed)%2 == 0 {
			decoded := make([]byte, hex.DecodedLen(len(trimmed)))
			if _, decodeErr := hex.Decode(decoded, trimmed); decodeErr == nil {
				if credential, decodeErr := decodeCredentialPayload(decoded); decodeErr == nil {
					return credential, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("%s from %s (%s): %w", unreadableCredentialPhrase, source, describeCredentialPayload(body, err), err)
}

func decodeCredentialPayload(body []byte) (*CredentialInfo, error) {
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
	return credentialExpiredAt(credential, skew, time.Now())
}

func credentialExpiredAt(credential *CredentialInfo, skew time.Duration, now time.Time) bool {
	if credential == nil || credential.ExpiresAt <= 0 {
		return false
	}
	expiresAt := time.UnixMilli(credential.ExpiresAt)
	return !now.Add(skew).Before(expiresAt)
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
		CredentialVersion: accountpkg.OAuthCredentialVersion(
			credential.AccessToken, credential.RefreshToken,
		),
		Source: configDir,
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

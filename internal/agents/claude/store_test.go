package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestCredentialPlanTypeUsesOnlyCredentialMetadata(t *testing.T) {
	tests := []struct {
		name       string
		credential *CredentialInfo
		want       string
	}{
		{name: "nil", want: "unknown"},
		{name: "absent", credential: &CredentialInfo{AccessToken: "secret"}, want: "unknown"},
		{name: "max normalized", credential: &CredentialInfo{SubscriptionType: " MAX "}, want: "max"},
		{name: "pro normalized", credential: &CredentialInfo{SubscriptionType: "Pro"}, want: "pro"},
		{name: "free from tier", credential: &CredentialInfo{RateLimitTier: "FREE"}, want: "free"},
		{name: "max vendor tier", credential: &CredentialInfo{RateLimitTier: "default_claude_max_20x"}, want: "max"},
		{name: "subscription wins", credential: &CredentialInfo{SubscriptionType: "max", RateLimitTier: "pro"}, want: "max"},
		{name: "vendor label", credential: &CredentialInfo{RateLimitTier: "enterprise"}, want: "enterprise"},
		{name: "unsafe vendor label", credential: &CredentialInfo{RateLimitTier: "enterprise\x1b[31m"}, want: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.credential.PlanType(); got != test.want {
				t.Fatalf("PlanType() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRefreshCredentialDetailsReturnsSameCredentialSnapshot(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	credential := CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh", SubscriptionType: "max",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}
	if err := store.ImportProfileCredential("work", credential); err != nil {
		t.Fatal(err)
	}
	profile, ok := store.FindProfile("work")
	if !ok {
		t.Fatal("missing imported profile")
	}
	account, got, refreshed, err := store.RefreshCredentialDetailsIfExpired(t.Context(), http.DefaultClient, profile)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed || got == nil || got.PlanType() != "max" || account.Token != credential.AccessToken {
		t.Fatalf("details = account:%+v credential:%+v refreshed:%v", account, got, refreshed)
	}
}

func TestStoreCreateSetRemoveProfile(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"session-env", "todos", "logs", "file-history", "shell-snapshots", "debug", ".anthropic"} {
		if _, err := os.Stat(filepath.Join(instancePath, dir)); err != nil {
			t.Fatalf("missing instance dir %s: %v", dir, err)
		}
	}
	if active := store.ActiveProfile(); active != "work" {
		t.Fatalf("active = %q, want work", active)
	}
	if err := store.SetActiveProfile("work"); err != nil {
		t.Fatal(err)
	}
	removed, err := store.RemoveProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("profile was not removed")
	}
	if profiles := store.ListProfiles(); len(profiles) != 0 {
		t.Fatalf("profiles = %d, want 0", len(profiles))
	}
}

func TestRemoveProfileContextStopsAtRegistryLockDeadline(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	lock, err := lockProfileRegistry(store.ProfilesPath())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	removed, err := store.RemoveProfileContext(ctx, "work")
	if removed {
		t.Fatal("profile was removed without acquiring the registry lock")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("remove error = %v, want context deadline exceeded", err)
	}
	if _, ok := store.FindProfile("work"); !ok {
		t.Fatal("timed-out removal changed the profile registry")
	}
}

func TestRegisterProfileAllowsEmailName(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	_, dir, err := store.CreateTempInstance()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterProfile("person@example.com", dir); err != nil {
		t.Fatal(err)
	}
	profile, ok := store.FindProfile("person@example.com")
	if !ok {
		t.Fatal("profile not found")
	}
	if profile.Dir != dir {
		t.Fatalf("dir = %q, want %q", profile.Dir, dir)
	}
}

func TestProfileRegistrationRejectsSharedInstanceDirectory(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("Work"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateProfile("work"); err == nil || !strings.Contains(err.Error(), "already owned by profile \"Work\"") {
		t.Fatalf("case-colliding CreateProfile error = %v", err)
	}
	if err := store.RegisterProfile("other", "work"); err == nil || !strings.Contains(err.Error(), "already owned by profile \"Work\"") {
		t.Fatalf("colliding RegisterProfile error = %v", err)
	}
	if profiles := store.ListProfiles(); len(profiles) != 1 || profiles[0].Name != "Work" {
		t.Fatalf("collision attempts changed registry: %+v", profiles)
	}
}

func TestProfileRegistrationRejectsSymlinkedInstanceDirectory(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	if err := os.MkdirAll(store.InstancesDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(store.InstancesDir(), "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(store.InstancesDir(), "alias")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := store.RegisterProfile("first", "real"); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterProfile("second", "alias"); err == nil || !strings.Contains(err.Error(), "already owned by profile \"first\"") {
		t.Fatalf("symlink-colliding RegisterProfile error = %v", err)
	}
}

func TestProfileInstancePathsAliasUsesPlatformCaseRulesForMissingPaths(t *testing.T) {
	root := t.TempDir()
	upper := filepath.Join(root, "CaseOnlyMissing")
	lower := filepath.Join(root, "caseonlymissing")
	for _, goos := range []string{"darwin", "windows"} {
		aliases, err := profileInstancePathsAliasForOS(goos, upper, lower)
		if err != nil || !aliases {
			t.Fatalf("%s case-only aliases = %v, err %v", goos, aliases, err)
		}
	}
	aliases, err := profileInstancePathsAliasForOS("linux", upper, lower)
	if err != nil || aliases {
		t.Fatalf("linux case-only aliases = %v, err %v", aliases, err)
	}
}

func TestEnvForConfigDirFiltersInheritedClaudeRouting(t *testing.T) {
	for _, key := range RoutingEnvKeys() {
		t.Setenv(key, "must-not-survive")
	}
	t.Setenv("claude_code_use_foundry", "lowercase-must-not-survive")
	t.Setenv("SUBROUTER_ADMIN_TOKEN", "durable-admin-secret")
	t.Setenv("SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE", "/private/import-token")
	t.Setenv("SUBROUTER_FUTURE_SECRET", "future-secret")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", "/private/cloud-config")
	t.Setenv("SUBROUTER_STATE_DIR", "/private/state")

	seen := make(map[string]string)
	for _, item := range EnvForConfigDir("/new/config") {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			seen[key] = value
		}
	}
	for _, key := range append(RoutingEnvKeys(),
		"SUBROUTER_ADMIN_TOKEN",
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE",
		"SUBROUTER_FUTURE_SECRET",
		"SUBROUTER_CLOUD_CONFIG",
		"SUBROUTER_STATE_DIR",
		"claude_code_use_foundry",
	) {
		if key == "CLAUDE_CONFIG_DIR" {
			continue
		}
		if _, ok := seen[key]; ok {
			t.Fatalf("%s was inherited by Claude", key)
		}
	}
	if seen["CLAUDE_CONFIG_DIR"] != "/new/config" {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want /new/config", seen["CLAUDE_CONFIG_DIR"])
	}
}

func TestAuthStatusForPathUsesProfileConfigWithoutInheritedRouting(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(dir, "env.txt")
	claudePath := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\nenv > \"$RECORD_ENV\"\nprintf '{\"loggedIn\":false}\n'\n"
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECORD_ENV", recordPath)
	t.Setenv("ANTHROPIC_BASE_URL", "http://stale-proxy:31415")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "stale-token")
	t.Setenv("CLAUDE_CONFIG_DIR", "/old/config")

	status, err := AuthStatusForPath(context.Background(), claudePath, dir)
	if err != nil {
		t.Fatal(err)
	}
	if status == nil || status.LoggedIn {
		t.Fatalf("status = %#v, want logged out", status)
	}
	body, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ANTHROPIC_BASE_URL=", "ANTHROPIC_AUTH_TOKEN="} {
		if strings.Contains(string(body), key) {
			t.Fatalf("Claude auth status inherited %s", key)
		}
	}
	if !strings.Contains(string(body), "CLAUDE_CONFIG_DIR="+dir) {
		t.Fatalf("Claude auth status did not receive config dir: %s", body)
	}
}

func TestImportProfileCredentialKeepsSanitizedNameCollisionsIsolated(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	firstName := "first.last@example.com"
	secondName := "first+last@example.com"
	if sanitizeName(firstName) != sanitizeName(secondName) {
		t.Fatal("test inputs no longer reproduce the legacy directory collision")
	}
	if err := store.ImportProfileCredential(firstName, CredentialInfo{
		AccessToken: "first-access", RefreshToken: "first-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ImportProfileCredential(secondName, CredentialInfo{
		AccessToken: "second-access", RefreshToken: "second-refresh",
	}); err != nil {
		t.Fatal(err)
	}

	first, ok := store.FindProfile(firstName)
	if !ok {
		t.Fatalf("profile %q not found", firstName)
	}
	second, ok := store.FindProfile(secondName)
	if !ok {
		t.Fatalf("profile %q not found", secondName)
	}
	if first.Dir == second.Dir {
		t.Fatalf("distinct profiles share credential directory %q", first.Dir)
	}
	firstCredential, err := store.ReadCredential(t.Context(), store.ClaudeConfigDir(firstName))
	if err != nil {
		t.Fatal(err)
	}
	secondCredential, err := store.ReadCredential(t.Context(), store.ClaudeConfigDir(secondName))
	if err != nil {
		t.Fatal(err)
	}
	if firstCredential.AccessToken != "first-access" || secondCredential.AccessToken != "second-access" {
		t.Fatalf("credential collision: first=%q second=%q", firstCredential.AccessToken, secondCredential.AccessToken)
	}
}

func TestImportProfileCredentialSerializesRegistryAcrossProcesses(t *testing.T) {
	if os.Getenv("SUBROUTER_CLAUDE_IMPORT_HELPER") == "1" {
		dir := os.Getenv("SUBROUTER_CLAUDE_IMPORT_DIR")
		name := os.Getenv("SUBROUTER_CLAUDE_IMPORT_NAME")
		ready := os.Getenv("SUBROUTER_CLAUDE_IMPORT_READY")
		gate := os.Getenv("SUBROUTER_CLAUDE_IMPORT_GATE")
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(gate); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for concurrent import gate")
			}
			time.Sleep(time.Millisecond)
		}
		if err := (Store{Dir: dir}).ImportProfileCredential(name, CredentialInfo{
			AccessToken:  "access-" + name,
			RefreshToken: "refresh-" + name,
		}); err != nil {
			t.Fatal(err)
		}
		return
	}

	dir := t.TempDir()
	gate := filepath.Join(dir, "start-imports")
	const processCount = 24
	commands := make([]*exec.Cmd, 0, processCount)
	for index := 0; index < processCount; index++ {
		name := fmt.Sprintf("profile-%02d@example.com", index)
		ready := filepath.Join(dir, fmt.Sprintf("ready-%02d", index))
		command := exec.Command(os.Args[0], "-test.run=^TestImportProfileCredentialSerializesRegistryAcrossProcesses$")
		command.Env = append(os.Environ(),
			"SUBROUTER_CLAUDE_IMPORT_HELPER=1",
			"SUBROUTER_CLAUDE_IMPORT_DIR="+dir,
			"SUBROUTER_CLAUDE_IMPORT_NAME="+name,
			"SUBROUTER_CLAUDE_IMPORT_READY="+ready,
			"SUBROUTER_CLAUDE_IMPORT_GATE="+gate,
		)
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, command)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		readyCount := 0
		for index := 0; index < processCount; index++ {
			if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("ready-%02d", index))); err == nil {
				readyCount++
			}
		}
		if readyCount == processCount {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d import helpers became ready", readyCount, processCount)
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.WriteFile(gate, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range commands {
		if err := command.Wait(); err != nil {
			t.Errorf("import helper failed: %v", err)
		}
	}
	if t.Failed() {
		return
	}
	profiles := (Store{Dir: dir}).ListProfiles()
	if len(profiles) != processCount {
		t.Fatalf("profiles = %d, want %d", len(profiles), processCount)
	}
}

func TestRefreshCredentialDoesNotOverwriteNewerTenantUploadAcrossProcesses(t *testing.T) {
	if os.Getenv("SUBROUTER_CLAUDE_REFRESH_HELPER") == "1" {
		oauthTokenURL = os.Getenv("SUBROUTER_CLAUDE_REFRESH_URL")
		store := Store{Dir: os.Getenv("SUBROUTER_CLAUDE_REFRESH_DIR")}
		profile, ok := store.FindProfile("founders@example.com")
		if !ok {
			t.Fatal("refresh helper could not find profile")
		}
		_, didRefresh, err := store.RefreshCredentialIfExpired(context.Background(), http.DefaultClient, profile)
		if err != nil {
			t.Fatal(err)
		}
		if !didRefresh {
			t.Fatal("refresh helper did not refresh expired credential")
		}
		return
	}

	store := Store{Dir: t.TempDir()}
	if err := store.ImportProfileCredential("founders@example.com", CredentialInfo{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	requestSeen := make(chan struct{}, 1)
	releaseRefresh := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requestSeen <- struct{}{}:
		default:
		}
		select {
		case <-releaseRefresh:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"stale-refreshed-access","refresh_token":"stale-refreshed-refresh","expires_in":3600}`)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRefreshCredentialDoesNotOverwriteNewerTenantUploadAcrossProcesses$")
	command.Env = append(os.Environ(),
		"SUBROUTER_CLAUDE_REFRESH_HELPER=1",
		"SUBROUTER_CLAUDE_REFRESH_DIR="+store.Dir,
		"SUBROUTER_CLAUDE_REFRESH_URL="+server.URL,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestSeen:
	case <-ctx.Done():
		close(releaseRefresh)
		_ = command.Wait()
		t.Fatal("refresh helper did not reach OAuth endpoint")
	}

	uploadDone := make(chan error, 1)
	go func() {
		_, err := store.UpsertCredentialProfile("founders@example.com", CredentialInfo{
			AccessToken:  "new-import-access",
			RefreshToken: "new-import-refresh",
			ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
		})
		uploadDone <- err
	}()
	select {
	case err := <-uploadDone:
		if err != nil {
			close(releaseRefresh)
			_ = command.Wait()
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(releaseRefresh)
		_ = command.Wait()
		t.Fatal("credential import remained blocked by the OAuth round trip")
	}
	close(releaseRefresh)
	if err := command.Wait(); err != nil {
		t.Fatalf("refresh helper failed: %v", err)
	}

	credential, err := store.ReadCredential(context.Background(), store.ClaudeConfigDir("founders@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil {
		t.Fatal("final credential is missing")
	}
	if credential.AccessToken != "new-import-access" || credential.RefreshToken != "new-import-refresh" {
		t.Fatalf("refresh overwrote newer import: access=%q refresh=%q", credential.AccessToken, credential.RefreshToken)
	}
}

func TestClaudeConfigDirPrefersCodexAccountsAlias(t *testing.T) {
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store.Dir, filepath.Join(home, ".codex-accounts")); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(home, ".codex-accounts", "claude", "work")
	if got := store.ClaudeConfigDir("work"); got != want {
		t.Fatalf("ClaudeConfigDir = %q, want %q", got, want)
	}
}

func TestClaudeConfigDirFallsBackWhenAliasMissing(t *testing.T) {
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}

	if got := store.ClaudeConfigDir("work"); got != filepath.Clean(instancePath) {
		t.Fatalf("ClaudeConfigDir = %q, want %q", got, filepath.Clean(instancePath))
	}
}

func TestPrepareSharedStateDirSharesHistoryButNotCredentials(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	configDir := filepath.Join(root, "proxy")
	store := Store{Dir: filepath.Join(root, "store"), SharedStateDir: shared}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, ".credentials.json"), []byte("proxy-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareSharedStateDir(configDir); err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(configDir, "projects")
	info, err := os.Lstat(projects)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("projects mode = %v, want symlink", info.Mode())
	}
	credential, err := os.ReadFile(filepath.Join(configDir, ".credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(credential) != "proxy-secret" {
		t.Fatalf("credential changed: %q", credential)
	}
	if _, err := os.Stat(filepath.Join(shared, ".credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential leaked into shared state: %v", err)
	}
}

func TestImportProfileCredentialUsesPreferredRealLegacyPath(t *testing.T) {
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	legacyInstance := filepath.Join(home, ".codex-accounts", "claude", "work")
	if err := os.MkdirAll(legacyInstance, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyInstance, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"legacy-access","refreshToken":"legacy-refresh"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := store.ImportProfileCredential("work", CredentialInfo{
		AccessToken:  "imported-access",
		RefreshToken: "imported-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	credential, err := store.ReadCredential(t.Context(), store.ClaudeConfigDir("work"))
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil || credential.AccessToken != "imported-access" || credential.RefreshToken != "imported-refresh" {
		t.Fatalf("preferred credential was not updated: %+v", credential)
	}
}

func TestRemoveProfileRemovesPreferredRealLegacyPath(t *testing.T) {
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	canonicalInstance := store.InstancePath("work")
	if err := os.WriteFile(filepath.Join(canonicalInstance, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"copied-access","refreshToken":"copied-refresh"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyInstance := filepath.Join(home, ".codex-accounts", "claude", "work")
	if err := os.MkdirAll(legacyInstance, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyInstance, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"legacy-access","refreshToken":"legacy-refresh"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := store.RemoveProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("profile was not removed")
	}
	if _, err := os.Stat(legacyInstance); !os.IsNotExist(err) {
		t.Fatalf("preferred credential directory still exists: %v", err)
	}
	if _, err := os.Stat(canonicalInstance); !os.IsNotExist(err) {
		t.Fatalf("copied canonical credential directory still exists: %v", err)
	}
	recreatedInstance, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	credential, err := store.ReadCredential(t.Context(), recreatedInstance)
	if err != nil {
		t.Fatal(err)
	}
	if credential != nil {
		t.Fatalf("removed credential was resurrected after same-name recreation: %+v", credential)
	}
}

func TestRemoveProfileDeletesMacOSKeychainCredential(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Keychain is only available on Darwin")
	}
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	fakeBin := t.TempDir()
	recordPath := filepath.Join(t.TempDir(), "security-arguments")
	securityPath := filepath.Join(fakeBin, "security")
	if err := os.WriteFile(securityPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SUBROUTER_KEYCHAIN_TEST_RECORD\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SUBROUTER_KEYCHAIN_TEST_RECORD", recordPath)

	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ImportProfileCredential("work", CredentialInfo{
		AccessToken:  "access",
		RefreshToken: "refresh",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(recordPath, 0); err != nil {
		t.Fatal(err)
	}

	removed, err := store.RemoveProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("profile was not removed")
	}
	record, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	wantServices := []string{
		"Claude Code-credentials-" + keychainHash(instancePath),
		"Claude Code-credentials-" + keychainHash(filepath.Join(home, ".codex-accounts", "claude", "work")),
	}
	for _, wantService := range wantServices {
		if got := string(record); !strings.Contains(got, "delete-generic-password") || !strings.Contains(got, wantService) {
			t.Fatalf("Keychain removal = %q, want delete for %q", got, wantService)
		}
	}
}

func TestProfileRemovalSnapshotRejectsOperationalKeychainFailure(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Keychain is only available on Darwin")
	}
	store := Store{Dir: filepath.Join(t.TempDir(), ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "security"), []byte("#!/bin/sh\necho locked >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, found, err := store.SnapshotProfileRemovalContext(t.Context(), "work")
	if err == nil || found || !strings.Contains(err.Error(), "read Claude credential from keychain") {
		t.Fatalf("operational Keychain failure = found %v, err %v", found, err)
	}
}

func TestProfileRemovalSnapshotAcceptsKeychainNotFound(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Keychain is only available on Darwin")
	}
	store := Store{Dir: filepath.Join(t.TempDir(), ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "security"), []byte("#!/bin/sh\nexit 44\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	snapshot, found, err := store.SnapshotProfileRemovalContext(t.Context(), "work")
	if err != nil || !found || snapshot.CredentialVersion == "" {
		t.Fatalf("missing Keychain snapshot = found %v snapshot %+v err %v", found, snapshot, err)
	}
}

func TestSyncProfileRemovalParentsUsesDurableDirectoryBoundary(t *testing.T) {
	parent := t.TempDir()
	var opened []string
	err := syncProfileRemovalParentsForOS("darwin", map[string]struct{}{parent: {}}, func(path string) (*os.File, error) {
		opened = append(opened, path)
		return os.Open(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(opened) != 1 || opened[0] != parent {
		t.Fatalf("synced removal parents = %v, want [%s]", opened, parent)
	}
	opened = nil
	if err := syncProfileRemovalParentsForOS("windows", map[string]struct{}{parent: {}}, func(path string) (*os.File, error) {
		opened = append(opened, path)
		return os.Open(path)
	}); err != nil || len(opened) != 0 {
		t.Fatalf("Windows removal sync = opened %v, err %v", opened, err)
	}
}

func TestSyncProfileRemovalParentsAcceptsMissingLegacyParent(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-created-legacy-root")
	err := syncProfileRemovalParentsForOS("darwin", map[string]struct{}{missing: {}}, os.Open)
	if err != nil {
		t.Fatalf("missing legacy removal parent blocked replay: %v", err)
	}
}

func TestStageProfileInstancesSyncsEachRenameBeforeTouchingNextPath(t *testing.T) {
	parent := t.TempDir()
	first := filepath.Join(parent, "first[1]*")
	second := filepath.Join(parent, "second[2]*")
	for _, path := range []string{first, second} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var calls int
	staged, err := stageProfileInstancePathsWithSync([]string{first, second}, func(parents map[string]struct{}) error {
		calls++
		if _, ok := parents[parent]; !ok {
			t.Fatalf("sync %d omitted source parent: %v", calls, parents)
		}
		if calls == 2 {
			if _, err := os.Lstat(first); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("first path at first sync: %v", err)
			}
			if _, err := os.Lstat(second); err != nil {
				t.Fatalf("second path was touched before first sync: %v", err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("stage sync calls = %d, want preparation and rename sync per path", calls)
	}
	if err := deleteStagedProfileInstances(staged); err != nil {
		t.Fatal(err)
	}
}

func TestProfileRemovalStageCrashBeforeRegistryCommitIsRecoverable(t *testing.T) {
	if instancePath := os.Getenv("SUBROUTER_TEST_CLAUDE_STAGE_CRASH_PATH"); instancePath != "" {
		if _, err := stageProfileInstancePaths([]string{instancePath}); err != nil {
			os.Exit(86)
		}
		os.Exit(87)
	}

	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	literalDir := "work[1]*"
	instancePath := filepath.Join(store.InstancesDir(), literalDir)
	if err := os.MkdirAll(instancePath, 0o700); err != nil {
		t.Fatal(err)
	}
	wantCredential := []byte(`{"claudeAiOauth":{"accessToken":"survives-crash","refreshToken":"refresh"}}`)
	if err := os.WriteFile(filepath.Join(instancePath, ".credentials.json"), wantCredential, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.writeProfiles(profilesFile{
		Active: "work",
		Profiles: map[string]Profile{
			"work": {Name: "work", Dir: literalDir},
		},
	}); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestProfileRemovalStageCrashBeforeRegistryCommitIsRecoverable$")
	command.Env = append(os.Environ(), "SUBROUTER_TEST_CLAUDE_STAGE_CRASH_PATH="+instancePath)
	if err := command.Run(); err == nil {
		t.Fatal("crash helper unexpectedly completed")
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 87 {
		t.Fatalf("crash helper = %v, want exit 87", err)
	}

	restarted := Store{Dir: store.Dir}
	if _, found := restarted.FindProfile("work"); !found {
		t.Fatal("pre-commit crash changed the live registry")
	}
	if _, err := os.Lstat(instancePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crash boundary unexpectedly retained canonical path: %v", err)
	}
	if err := restarted.ReconcileProfileInstanceStagesContext(t.Context()); err != nil {
		t.Fatalf("restart reconciliation: %v", err)
	}
	gotCredential, err := os.ReadFile(filepath.Join(restarted.ClaudeConfigDir("work"), ".credentials.json"))
	if err != nil || string(gotCredential) != string(wantCredential) {
		t.Fatalf("credential after restart recovery = %q, err %v", gotCredential, err)
	}
	if roots, err := stagedProfileInstanceRoots(instancePath); err != nil || len(roots) != 0 {
		t.Fatalf("literal-path recovery left stages %v, err %v", roots, err)
	}
}

func TestRemoveProfileReportsStageAndRollbackSyncFailures(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	if err := store.ImportProfileCredential("work", CredentialInfo{AccessToken: "access", RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	profile, found := store.FindProfile("work")
	if !found {
		t.Fatal("profile missing before removal")
	}
	instancePath := filepath.Join(store.InstancesDir(), profile.Dir)
	stageSyncErr := errors.New("stage parent sync unavailable")
	rollbackSyncErr := errors.New("rollback parent sync unavailable")
	var calls int
	store.syncRemovalParentsForTest = func(map[string]struct{}) error {
		calls++
		if calls == 1 {
			return stageSyncErr
		}
		return rollbackSyncErr
	}
	removed, err := store.RemoveProfileContext(t.Context(), "work")
	if removed || !errors.Is(err, stageSyncErr) || !errors.Is(err, rollbackSyncErr) {
		t.Fatalf("sync-failed stage = removed %v, err %v", removed, err)
	}
	if _, found := store.FindProfile("work"); !found {
		t.Fatal("failed pre-commit stage changed the registry")
	}
	credential, readErr := os.ReadFile(filepath.Join(instancePath, ".credentials.json"))
	if readErr != nil || !strings.Contains(string(credential), "access") {
		t.Fatalf("failed stage did not preserve credential: %q, err %v", credential, readErr)
	}
	if roots, listErr := stagedProfileInstanceRoots(instancePath); listErr != nil || len(roots) != 0 {
		t.Fatalf("failed stage left roots %v, err %v", roots, listErr)
	}
	restarted := Store{Dir: store.Dir}
	if err := restarted.ReconcileProfileInstanceStagesContext(t.Context()); err != nil {
		t.Fatalf("restart after rollback sync failure: %v", err)
	}
	credential, readErr = os.ReadFile(filepath.Join(restarted.ClaudeConfigDir("work"), ".credentials.json"))
	if readErr != nil || !strings.Contains(string(credential), "access") {
		t.Fatalf("restart after rollback sync failure lost credential: %q, err %v", credential, readErr)
	}
}

func TestRemoveProfileReportsCommittedRemovalWhenStageCleanupSyncFails(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	if err := store.ImportProfileCredential("work", CredentialInfo{AccessToken: "access", RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	cleanupSyncErr := errors.New("cleanup parent sync unavailable")
	var calls int
	store.syncRemovalParentsForTest = func(map[string]struct{}) error {
		calls++
		if calls < 3 {
			return nil
		}
		return cleanupSyncErr
	}
	removed, err := store.RemoveProfileContext(t.Context(), "work")
	if !removed || !errors.Is(err, cleanupSyncErr) {
		t.Fatalf("cleanup-sync removal = removed %v, err %v", removed, err)
	}
	if _, found := store.FindProfile("work"); found {
		t.Fatal("committed cleanup-sync failure retained registry profile")
	}
}

func TestRollbackStagedProfileInstancePreservesConcurrentReplacement(t *testing.T) {
	parent := t.TempDir()
	instancePath := filepath.Join(parent, "work[1]*")
	if err := os.MkdirAll(instancePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instancePath, ".credentials.json"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	staged, err := stageProfileInstancePaths([]string{instancePath})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(instancePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instancePath, ".credentials.json"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := errors.New("rollback parent sync unavailable")
	err = rollbackStagedProfileInstancesWithSync(staged, func(map[string]struct{}) error { return want })
	if err == nil {
		t.Fatal("replacement rollback unexpectedly succeeded")
	}
	got, readErr := os.ReadFile(filepath.Join(instancePath, ".credentials.json"))
	if readErr != nil || string(got) != "replacement" {
		t.Fatalf("replacement after failed rollback = %q, err %v", got, readErr)
	}
	if roots, listErr := stagedProfileInstanceRoots(instancePath); listErr != nil || len(roots) != 1 {
		t.Fatalf("original recovery stage = %v, err %v", roots, listErr)
	}
}

func TestRollbackStageSyncFailureKeepsPreparedManifestForRestart(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	original := filepath.Join(store.InstancesDir(), "work[1]*")
	if err := os.MkdirAll(original, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(original, ".credentials.json"), []byte("credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{
		"work": {Name: "work", Dir: "work[1]*"},
	}}); err != nil {
		t.Fatal(err)
	}
	staged, err := stageProfileInstancePaths([]string{original})
	if err != nil || len(staged) != 1 {
		t.Fatalf("stage = %+v, err %v", staged, err)
	}
	want := errors.New("reverse rename directory sync unavailable")
	err = rollbackStagedProfileInstancesWithSync(staged, func(map[string]struct{}) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("rollback sync failure = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(original, ".credentials.json")); err != nil || string(got) != "credential" {
		t.Fatalf("visible restored credential = %q, err %v", got, err)
	}
	owned, err := readOwnedProfileRemovalStage(staged[0].stagingRoot, original)
	if err != nil || !owned.prepared {
		t.Fatalf("restart provenance = %+v, err %v", owned, err)
	}
	if err := store.ReconcileProfileInstanceStagesContext(t.Context()); err != nil {
		t.Fatalf("startup completion: %v", err)
	}
	if _, err := os.Lstat(staged[0].stagingRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("startup retained prepared stage: %v", err)
	}
}

func TestRollbackStageSyncsRenameBeforeRemovingManifestRoot(t *testing.T) {
	parent := t.TempDir()
	original := filepath.Join(parent, "work[1]*")
	if err := os.MkdirAll(original, 0o700); err != nil {
		t.Fatal(err)
	}
	staged, err := stageProfileInstancePaths([]string{original})
	if err != nil || len(staged) != 1 {
		t.Fatalf("stage = %+v, err %v", staged, err)
	}
	var calls int
	err = rollbackStagedProfileInstancesWithSync(staged, func(parents map[string]struct{}) error {
		calls++
		switch calls {
		case 1:
			if _, err := os.Lstat(original); err != nil {
				t.Fatalf("reverse rename not visible at first sync: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(staged[0].stagingRoot, profileRemovalStageManifestName)); err != nil {
				t.Fatalf("manifest missing before rename sync: %v", err)
			}
		case 2:
			if _, err := os.Lstat(filepath.Join(original, profileRemovalOperationMarkerName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("operation marker still visible at marker sync: %v", err)
			}
			if _, err := os.Lstat(staged[0].stagingRoot); err != nil {
				t.Fatalf("stage root removed before marker sync: %v", err)
			}
		case 3:
			if _, err := os.Lstat(staged[0].stagingRoot); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stage root still visible at cleanup sync: %v", err)
			}
		default:
			t.Fatalf("unexpected sync call %d: %v", calls, parents)
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("two-phase rollback = calls %d, err %v", calls, err)
	}
}

func TestOrdinaryRollbackRemovesOperationMarker(t *testing.T) {
	original := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(original, 0o700); err != nil {
		t.Fatal(err)
	}
	staged, err := stageProfileInstancePaths([]string{original})
	if err != nil {
		t.Fatal(err)
	}
	if err := rollbackStagedProfileInstances(staged); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(original, profileRemovalOperationMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ordinary rollback left operation marker: %v", err)
	}
}

func TestRollbackNeverReplacesExistingEmptyDirectory(t *testing.T) {
	original := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(original, 0o700); err != nil {
		t.Fatal(err)
	}
	staged, err := stageProfileInstancePaths([]string{original})
	if err != nil || len(staged) != 1 {
		t.Fatalf("stage = %+v, err %v", staged, err)
	}
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := rollbackStagedProfileInstances(staged); err == nil {
		t.Fatal("rollback replaced an occupied empty destination")
	}
	if info, err := os.Stat(original); err != nil || !info.IsDir() {
		t.Fatalf("empty replacement = %v, err %v", info, err)
	}
	if info, err := os.Stat(staged[0].stagedPath); err != nil || !info.IsDir() {
		t.Fatalf("staged original = %v, err %v", info, err)
	}
}

func TestOrdinaryRemovalLeavesNoOperationMarkerOrStage(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	if err := store.ImportProfileCredential("work", CredentialInfo{AccessToken: "access", RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	profile, found := store.FindProfile("work")
	if !found {
		t.Fatal("profile missing before removal")
	}
	original := filepath.Join(store.InstancesDir(), profile.Dir)
	removed, err := store.RemoveProfileContext(t.Context(), "work")
	if err != nil || !removed {
		t.Fatalf("remove = %v, err %v", removed, err)
	}
	if _, err := os.Lstat(filepath.Join(original, profileRemovalOperationMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ordinary removal left operation marker: %v", err)
	}
	if roots, err := stagedProfileInstanceRoots(original); err != nil || len(roots) != 0 {
		t.Fatalf("ordinary removal stages = %v, err %v", roots, err)
	}
}

func TestStageRefusesExistingOperationMarkerWithoutReplacingIt(t *testing.T) {
	original := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(original, 0o700); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(original, profileRemovalOperationMarkerName)
	target := filepath.Join(t.TempDir(), "foreign-marker")
	if err := os.WriteFile(target, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, markerPath); err != nil {
		t.Fatal(err)
	}
	if _, err := stageProfileInstancePaths([]string{original}); err == nil {
		t.Fatal("stage unexpectedly replaced an existing operation marker")
	}
	info, err := os.Lstat(markerPath)
	if err != nil {
		t.Fatalf("existing operation marker disappeared: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("existing operation marker mode = %v", info.Mode())
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "foreign" {
		t.Fatalf("existing operation marker target = %q, err %v", got, err)
	}
}

func TestRestartCleansCommittedOrphanMarkerAfterDirectorySyncFailure(t *testing.T) {
	if storeDir := os.Getenv("SUBROUTER_TEST_CLAUDE_ORPHAN_MARKER_STORE"); storeDir != "" {
		store := Store{Dir: storeDir}
		profile, found := store.FindProfile("work")
		if !found {
			os.Exit(80)
		}
		instancePath := filepath.Join(store.InstancesDir(), profile.Dir)
		instancePaths, err := store.profileInstancePaths(profile.Dir)
		if err != nil {
			os.Exit(81)
		}
		version, err := store.profileCredentialVersionLocked(context.Background(), instancePaths, false)
		if err != nil {
			os.Exit(81)
		}
		operationID := "ffeeddccbbaa99887766554433221100"
		stageRoot, err := os.MkdirTemp(filepath.Dir(instancePath), "."+filepath.Base(instancePath)+".remove-*")
		if err != nil {
			os.Exit(82)
		}
		entry := stagedProfileInstance{
			originalPath:         instancePath,
			stagedPath:           filepath.Join(stageRoot, profileRemovalStageEntryName),
			stagingRoot:          stageRoot,
			operationID:          operationID,
			credentialSetVersion: version,
		}
		if err := writeProfileRemovalStageManifest(entry); err != nil {
			os.Exit(83)
		}
		original, err := normalizedProfileRemovalPath(instancePath)
		if err != nil {
			os.Exit(84)
		}
		body, err := json.Marshal(profileRemovalOperationMarker{
			Version:              profileRemovalOperationMarkerVersion,
			OriginalPath:         original,
			OperationID:          operationID,
			CredentialSetVersion: version,
		})
		if err != nil {
			os.Exit(85)
		}
		committed, writeErr := writePrivateFileAtomicWithDirectorySync(
			filepath.Join(instancePath, profileRemovalOperationMarkerName),
			append(body, '\n'),
			func(string) error { return errors.New("injected marker directory sync failure") },
		)
		if !committed || writeErr == nil {
			os.Exit(86)
		}
		// Emulate the old failure ordering, then crash before exact marker
		// cleanup. New code preserves the stage root until marker cleanup.
		if err := os.RemoveAll(stageRoot); err != nil {
			os.Exit(87)
		}
		os.Exit(89)
	}

	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	if err := store.ImportProfileCredential("work", CredentialInfo{AccessToken: "survives", RefreshToken: "survives-refresh"}); err != nil {
		t.Fatal(err)
	}
	profile, found := store.FindProfile("work")
	if !found {
		t.Fatal("profile missing before subprocess")
	}
	instancePath := filepath.Join(store.InstancesDir(), profile.Dir)
	command := exec.Command(os.Args[0], "-test.run=^TestRestartCleansCommittedOrphanMarkerAfterDirectorySyncFailure$")
	command.Env = append(os.Environ(), "SUBROUTER_TEST_CLAUDE_ORPHAN_MARKER_STORE="+store.Dir)
	if err := command.Run(); err == nil {
		t.Fatal("orphan-marker crash helper unexpectedly completed")
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 89 {
		t.Fatalf("orphan-marker crash helper = %v, want exit 89", err)
	}
	if _, err := os.Lstat(filepath.Join(instancePath, profileRemovalOperationMarkerName)); err != nil {
		t.Fatalf("committed orphan marker missing before restart: %v", err)
	}
	if err := store.ReconcileProfileInstanceStagesContext(t.Context()); err != nil {
		t.Fatalf("orphan-marker restart cleanup: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(instancePath, profileRemovalOperationMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan marker remains after restart: %v", err)
	}
	credential, err := os.ReadFile(filepath.Join(instancePath, ".credentials.json"))
	if err != nil || !strings.Contains(string(credential), "survives") {
		t.Fatalf("credential after orphan cleanup = %q, err %v", credential, err)
	}
	staged, err := stageProfileInstancePaths([]string{instancePath})
	if err != nil {
		t.Fatalf("future removal remained wedged: %v", err)
	}
	if err := rollbackStagedProfileInstances(staged); err != nil {
		t.Fatalf("future removal rollback: %v", err)
	}
}

func TestOrphanMarkerCredentialMismatchIsPreservedFailClosed(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	if err := store.ImportProfileCredential("work", CredentialInfo{AccessToken: "old", RefreshToken: "old-refresh"}); err != nil {
		t.Fatal(err)
	}
	profile, _ := store.FindProfile("work")
	instancePath := filepath.Join(store.InstancesDir(), profile.Dir)
	instancePaths, err := store.profileInstancePaths(profile.Dir)
	if err != nil {
		t.Fatal(err)
	}
	version, err := store.profileCredentialVersionLocked(t.Context(), instancePaths, false)
	if err != nil {
		t.Fatal(err)
	}
	entry := stagedProfileInstance{
		originalPath:         instancePath,
		operationID:          "00112233445566778899aabbccddeeff",
		credentialSetVersion: version,
	}
	if err := writeProfileRemovalOperationMarker(entry); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instancePath, ".credentials.json"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileProfileInstanceStagesContext(t.Context()); err == nil || !strings.Contains(err.Error(), "credential changed") {
		t.Fatalf("orphan replacement reconciliation = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(instancePath, profileRemovalOperationMarkerName)); err != nil {
		t.Fatalf("mismatched orphan marker was not preserved: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(instancePath, ".credentials.json")); err != nil || string(got) != "replacement" {
		t.Fatalf("orphan replacement = %q, err %v", got, err)
	}
}

func TestUnsafeOrphanMarkerIsPreservedFailClosed(t *testing.T) {
	for _, testCase := range []struct {
		name string
		make func(t *testing.T, markerPath string)
	}{
		{name: "malformed", make: func(t *testing.T, markerPath string) {
			if err := os.WriteFile(markerPath, []byte("{bad"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink", make: func(t *testing.T, markerPath string) {
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("foreign"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, markerPath); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
			if err := store.ImportProfileCredential("work", CredentialInfo{AccessToken: "live", RefreshToken: "live-refresh"}); err != nil {
				t.Fatal(err)
			}
			profile, _ := store.FindProfile("work")
			instancePath := filepath.Join(store.InstancesDir(), profile.Dir)
			markerPath := filepath.Join(instancePath, profileRemovalOperationMarkerName)
			testCase.make(t, markerPath)
			if err := store.ReconcileProfileInstanceStagesContext(t.Context()); err == nil {
				t.Fatal("unsafe orphan marker reconciliation unexpectedly succeeded")
			}
			if _, err := os.Lstat(markerPath); err != nil {
				t.Fatalf("unsafe orphan marker was not preserved: %v", err)
			}
			credential, err := os.ReadFile(filepath.Join(instancePath, ".credentials.json"))
			if err != nil || !strings.Contains(string(credential), "live") {
				t.Fatalf("live credential = %q, err %v", credential, err)
			}
		})
	}
}

func TestTwoRootRollbackSyncFailureRestoresAllBeforeRestartCleanup(t *testing.T) {
	if home := os.Getenv("SUBROUTER_TEST_CLAUDE_TWO_ROOT_ROLLBACK_HOME"); home != "" {
		store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
		canonical := filepath.Join(store.InstancesDir(), "work[1]*")
		legacyRoot, ok := store.legacyInstancePath(store.InstancesDir())
		if !ok {
			os.Exit(84)
		}
		legacy := filepath.Join(legacyRoot, "work[1]*")
		staged, err := stageProfileInstancePaths([]string{canonical, legacy})
		if err != nil || len(staged) != 2 {
			os.Exit(85)
		}
		calls := 0
		err = rollbackStagedProfileInstancesWithSync(staged, func(parents map[string]struct{}) error {
			calls++
			if calls == 1 {
				return errors.New("injected first reverse-rename sync failure")
			}
			return syncProfileRemovalParents(parents)
		})
		if err == nil || calls != 2 {
			os.Exit(86)
		}
		os.Exit(89)
	}

	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	dir := "work[1]*"
	canonical := filepath.Join(store.InstancesDir(), dir)
	legacyRoot, ok := store.legacyInstancePath(store.InstancesDir())
	if !ok {
		t.Fatal("test store did not expose legacy instance root")
	}
	legacy := filepath.Join(legacyRoot, dir)
	if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{
		"work": {Name: "work", Dir: dir},
	}}); err != nil {
		t.Fatal(err)
	}
	for path, payload := range map[string]string{canonical: "canonical", legacy: "legacy"} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, ".credentials.json"), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command(os.Args[0], "-test.run=^TestTwoRootRollbackSyncFailureRestoresAllBeforeRestartCleanup$")
	command.Env = append(os.Environ(), "SUBROUTER_TEST_CLAUDE_TWO_ROOT_ROLLBACK_HOME="+home)
	if err := command.Run(); err == nil {
		t.Fatal("rollback crash helper unexpectedly completed")
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 89 {
		t.Fatalf("rollback crash helper = %v, want exit 89", err)
	}

	for path, want := range map[string]string{canonical: "canonical", legacy: "legacy"} {
		got, err := os.ReadFile(filepath.Join(path, ".credentials.json"))
		if err != nil || string(got) != want {
			t.Fatalf("restored before restart %q = %q, err %v", path, got, err)
		}
		roots, err := stagedProfileInstanceRoots(path)
		if err != nil || len(roots) != 1 {
			t.Fatalf("prepared provenance for %q = %v, err %v", path, roots, err)
		}
		owned, err := readOwnedProfileRemovalStage(roots[0], path)
		if err != nil || !owned.prepared {
			t.Fatalf("prepared stage for %q = %+v, err %v", path, owned, err)
		}
	}
	if err := store.ReconcileProfileInstanceStagesContext(t.Context()); err != nil {
		t.Fatalf("restart cleanup: %v", err)
	}
	for _, path := range []string{canonical, legacy} {
		if roots, err := stagedProfileInstanceRoots(path); err != nil || len(roots) != 0 {
			t.Fatalf("restart stages for %q = %v, err %v", path, roots, err)
		}
	}
}

func TestTwoRootRollbackRenameFailureCompletesOnRestart(t *testing.T) {
	if home := os.Getenv("SUBROUTER_TEST_CLAUDE_TWO_ROOT_RENAME_HOME"); home != "" {
		store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
		canonical := filepath.Join(store.InstancesDir(), "work[1]*")
		legacyRoot, ok := store.legacyInstancePath(store.InstancesDir())
		if !ok {
			os.Exit(74)
		}
		legacy := filepath.Join(legacyRoot, "work[1]*")
		staged, err := stageProfileInstancePaths([]string{canonical, legacy})
		if err != nil || len(staged) != 2 {
			os.Exit(75)
		}
		calls := 0
		err = rollbackStagedProfileInstancesWithOps(staged, syncProfileRemovalParents, func(source, destination string) error {
			calls++
			if calls == 1 {
				return errors.New("injected first reverse rename failure")
			}
			return os.Rename(source, destination)
		})
		if err == nil || calls != 2 {
			os.Exit(76)
		}
		os.Exit(79)
	}

	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	dir := "work[1]*"
	canonical := filepath.Join(store.InstancesDir(), dir)
	legacyRoot, ok := store.legacyInstancePath(store.InstancesDir())
	if !ok {
		t.Fatal("test store did not expose legacy instance root")
	}
	legacy := filepath.Join(legacyRoot, dir)
	if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{
		"work": {Name: "work", Dir: dir},
	}}); err != nil {
		t.Fatal(err)
	}
	for path, payload := range map[string]string{canonical: "canonical", legacy: "legacy"} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, ".credentials.json"), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command(os.Args[0], "-test.run=^TestTwoRootRollbackRenameFailureCompletesOnRestart$")
	command.Env = append(os.Environ(), "SUBROUTER_TEST_CLAUDE_TWO_ROOT_RENAME_HOME="+home)
	if err := command.Run(); err == nil {
		t.Fatal("rename-failure helper unexpectedly completed")
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 79 {
		t.Fatalf("rename-failure helper = %v, want exit 79", err)
	}

	liveCount, actualCount, preparedCount := 0, 0, 0
	for _, path := range []string{canonical, legacy} {
		if _, err := os.Lstat(path); err == nil {
			liveCount++
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		roots, err := stagedProfileInstanceRoots(path)
		if err != nil || len(roots) != 1 {
			t.Fatalf("partial rollback roots for %q = %v, err %v", path, roots, err)
		}
		owned, err := readOwnedProfileRemovalStage(roots[0], path)
		if err != nil {
			t.Fatal(err)
		}
		if owned.prepared {
			preparedCount++
		} else {
			actualCount++
		}
	}
	if liveCount != 1 || preparedCount != 1 || actualCount != 1 {
		t.Fatalf("partial rollback = live %d prepared %d actual %d", liveCount, preparedCount, actualCount)
	}
	if err := store.ReconcileProfileInstanceStagesContext(t.Context()); err != nil {
		t.Fatalf("partial rollback restart: %v", err)
	}
	for path, want := range map[string]string{canonical: "canonical", legacy: "legacy"} {
		got, err := os.ReadFile(filepath.Join(path, ".credentials.json"))
		if err != nil || string(got) != want {
			t.Fatalf("restart restored %q = %q, err %v", path, got, err)
		}
		if roots, err := stagedProfileInstanceRoots(path); err != nil || len(roots) != 0 {
			t.Fatalf("restart roots for %q = %v, err %v", path, roots, err)
		}
	}
}

func TestPartialRollbackReplacementWithoutOperationMarkerFailsClosed(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), ".subrouter", "codex")}
	dir := "work[1]*"
	canonical := filepath.Join(store.InstancesDir(), dir)
	legacyRoot, ok := store.legacyInstancePath(store.InstancesDir())
	if !ok {
		t.Fatal("test store did not expose legacy instance root")
	}
	legacy := filepath.Join(legacyRoot, dir)
	if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{
		"work": {Name: "work", Dir: dir},
	}}); err != nil {
		t.Fatal(err)
	}
	for path, payload := range map[string]string{canonical: "canonical-old", legacy: "legacy-old"} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, ".credentials.json"), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	staged, err := stageProfileInstancePaths([]string{canonical, legacy})
	if err != nil || len(staged) != 2 {
		t.Fatalf("stage = %+v, err %v", staged, err)
	}
	calls := 0
	err = rollbackStagedProfileInstancesWithOps(staged, syncProfileRemovalParents, func(source, destination string) error {
		calls++
		if calls == 1 {
			return errors.New("injected first reverse rename failure")
		}
		return os.Rename(source, destination)
	})
	if err == nil {
		t.Fatal("partial rollback unexpectedly succeeded")
	}

	// Replace the one restored directory, including the operation marker that
	// proved it belonged to the interrupted rollback.
	restored := canonical
	if _, statErr := os.Lstat(restored); errors.Is(statErr, os.ErrNotExist) {
		restored = legacy
	}
	if err := os.RemoveAll(restored); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(restored, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restored, ".credentials.json"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := store.ReconcileProfileInstanceStagesContext(t.Context()); err == nil {
		t.Fatal("replacement without operation marker was treated as a partial rollback")
	}
	if got, err := os.ReadFile(filepath.Join(restored, ".credentials.json")); err != nil || string(got) != "replacement" {
		t.Fatalf("replacement after reconciliation = %q, err %v", got, err)
	}
	actualStages, preparedStages := 0, 0
	for _, entry := range staged {
		owned, err := readOwnedProfileRemovalStage(entry.stagingRoot, entry.originalPath)
		if err != nil {
			t.Fatalf("preserved provenance %q: %v", entry.stagingRoot, err)
		}
		if owned.prepared {
			preparedStages++
		} else {
			actualStages++
		}
	}
	if actualStages != 1 || preparedStages != 1 {
		t.Fatalf("preserved stages = actual %d prepared %d", actualStages, preparedStages)
	}
}

func TestPartialRollbackInPlaceCredentialRewriteFailsClosedAfterRestart(t *testing.T) {
	if home := os.Getenv("SUBROUTER_TEST_CLAUDE_IN_PLACE_REWRITE_HOME"); home != "" {
		store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
		canonical := filepath.Join(store.InstancesDir(), "work")
		legacyRoot, ok := store.legacyInstancePath(store.InstancesDir())
		if !ok {
			os.Exit(64)
		}
		legacy := filepath.Join(legacyRoot, "work")
		staged, err := stageProfileInstancePaths([]string{canonical, legacy})
		if err != nil || len(staged) != 2 {
			os.Exit(65)
		}
		calls := 0
		err = rollbackStagedProfileInstancesWithOps(staged, syncProfileRemovalParents, func(source, destination string) error {
			calls++
			if calls == 1 {
				return errors.New("injected first reverse rename failure")
			}
			return os.Rename(source, destination)
		})
		if err == nil {
			os.Exit(66)
		}
		live := canonical
		if _, statErr := os.Lstat(live); errors.Is(statErr, os.ErrNotExist) {
			live = legacy
		}
		// A noncooperating Claude process rewrites only the secret. The marker
		// intentionally remains, proving why operation lineage alone is not enough.
		if err := os.WriteFile(filepath.Join(live, ".credentials.json"), []byte("replacement-in-place"), 0o600); err != nil {
			os.Exit(67)
		}
		os.Exit(69)
	}

	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	canonical := filepath.Join(store.InstancesDir(), "work")
	legacyRoot, ok := store.legacyInstancePath(store.InstancesDir())
	if !ok {
		t.Fatal("test store did not expose legacy instance root")
	}
	legacy := filepath.Join(legacyRoot, "work")
	if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{"work": {Name: "work", Dir: "work"}}}); err != nil {
		t.Fatal(err)
	}
	for path, payload := range map[string]string{canonical: "canonical-old", legacy: "legacy-old"} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, ".credentials.json"), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command(os.Args[0], "-test.run=^TestPartialRollbackInPlaceCredentialRewriteFailsClosedAfterRestart$")
	command.Env = append(os.Environ(), "SUBROUTER_TEST_CLAUDE_IN_PLACE_REWRITE_HOME="+home)
	if err := command.Run(); err == nil {
		t.Fatal("in-place rewrite helper unexpectedly completed")
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 69 {
		t.Fatalf("in-place rewrite helper = %v, want exit 69", err)
	}

	live, absent := canonical, legacy
	if _, err := os.Lstat(live); errors.Is(err, os.ErrNotExist) {
		live, absent = legacy, canonical
	}
	if _, err := os.Lstat(filepath.Join(live, profileRemovalOperationMarkerName)); err != nil {
		t.Fatalf("partial rollback marker was not preserved: %v", err)
	}
	if err := store.ReconcileProfileInstanceStagesContext(t.Context()); err == nil || !strings.Contains(err.Error(), "credential changed") {
		t.Fatalf("in-place rewrite restart = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(live, ".credentials.json")); err != nil || string(got) != "replacement-in-place" {
		t.Fatalf("in-place replacement = %q, err %v", got, err)
	}
	if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sibling stale stage was restored at %q: %v", absent, err)
	}
	for _, path := range []string{canonical, legacy} {
		roots, err := stagedProfileInstanceRoots(path)
		if err != nil || len(roots) != 1 {
			t.Fatalf("preserved recovery stage for %q = %v, err %v", path, roots, err)
		}
	}
}

func TestPartialRollbackKeychainCredentialRewriteFailsClosed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Keychain is only available on Darwin")
	}
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	canonical := filepath.Join(store.InstancesDir(), "work")
	legacyRoot, ok := store.legacyInstancePath(store.InstancesDir())
	if !ok {
		t.Fatal("test store did not expose legacy instance root")
	}
	legacy := filepath.Join(legacyRoot, "work")
	if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{"work": {Name: "work", Dir: "work"}}}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{canonical, legacy} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	keychainPayload := filepath.Join(t.TempDir(), "keychain-payload")
	oldPayload := []byte(`{"claudeAiOauth":{"accessToken":"old","refreshToken":"old-refresh"}}`)
	if err := os.WriteFile(keychainPayload, oldPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "security"), []byte("#!/bin/sh\ncat \"$SUBROUTER_TEST_KEYCHAIN_PAYLOAD\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_TEST_KEYCHAIN_PAYLOAD", keychainPayload)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	paths := []string{canonical, legacy}
	version, err := store.profileCredentialVersionLocked(t.Context(), paths, false)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := stageProfileInstancePathsWithVersion(paths, version, syncProfileRemovalParents)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	err = rollbackStagedProfileInstancesWithOps(staged, syncProfileRemovalParents, func(source, destination string) error {
		calls++
		if calls == 1 {
			return errors.New("injected first reverse rename failure")
		}
		return os.Rename(source, destination)
	})
	if err == nil {
		t.Fatal("partial Keychain rollback unexpectedly succeeded")
	}
	newPayload := []byte(`{"claudeAiOauth":{"accessToken":"new","refreshToken":"new-refresh"}}`)
	if err := os.WriteFile(keychainPayload, newPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileProfileInstanceStagesContext(t.Context()); err == nil || !strings.Contains(err.Error(), "credential changed") {
		t.Fatalf("Keychain rewrite restart = %v", err)
	}
	for _, entry := range staged {
		if _, err := os.Lstat(entry.stagingRoot); err != nil {
			t.Fatalf("Keychain rewrite provenance %q was not preserved: %v", entry.stagingRoot, err)
		}
	}
}

func TestPartialRollbackUnsafeOperationMarkerFailsClosed(t *testing.T) {
	for _, testCase := range []struct {
		name string
		make func(t *testing.T, path string)
	}{
		{name: "malformed", make: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{bad"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink", make: func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("foreign"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
			original := filepath.Join(store.InstancesDir(), "work")
			if err := os.MkdirAll(original, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{"work": {Name: "work"}}}); err != nil {
				t.Fatal(err)
			}
			stageRoot := filepath.Join(filepath.Dir(original), ".work.remove-12345")
			writeOwnedRemovalStageForTest(t, original, stageRoot, nil)
			markerPath := filepath.Join(original, profileRemovalOperationMarkerName)
			testCase.make(t, markerPath)
			if err := store.ReconcileProfileInstanceStagesContext(t.Context()); err == nil {
				t.Fatal("unsafe marker reconciliation unexpectedly succeeded")
			}
			if _, err := os.Lstat(stageRoot); err != nil {
				t.Fatalf("unsafe marker stage not preserved: %v", err)
			}
		})
	}
}

func TestPreparedStageCleanupSyncFailurePreservesManifestUntilRestart(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	original := filepath.Join(store.InstancesDir(), "work[1]*")
	if err := os.MkdirAll(original, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(original, ".credentials.json"), []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{
		"work": {Name: "work", Dir: "work[1]*"},
	}}); err != nil {
		t.Fatal(err)
	}
	stageRoot := filepath.Join(filepath.Dir(original), ".work[1]*.remove-12345")
	writeOwnedRemovalStageForTest(t, original, stageRoot, nil)
	want := errors.New("prepared cleanup pre-sync unavailable")
	store.syncRemovalParentsForTest = func(map[string]struct{}) error { return want }
	if err := store.ReconcileProfileInstanceStagesContext(t.Context()); !errors.Is(err, want) {
		t.Fatalf("prepared cleanup sync failure = %v", err)
	}
	if owned, err := readOwnedProfileRemovalStage(stageRoot, original); err != nil || !owned.prepared {
		t.Fatalf("prepared stage after sync failure = %+v, err %v", owned, err)
	}
	store.syncRemovalParentsForTest = nil
	if err := store.ReconcileProfileInstanceStagesContext(t.Context()); err != nil {
		t.Fatalf("prepared cleanup restart: %v", err)
	}
	if _, err := os.Lstat(stageRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepared stage remains after restart: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(original, ".credentials.json")); err != nil || string(got) != "live" {
		t.Fatalf("live credential after prepared cleanup = %q, err %v", got, err)
	}
}

func TestRestartReconciliationPreservesLiveReplacementAndFailsClosed(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	literalDir := "work[1]*"
	instancePath := filepath.Join(store.InstancesDir(), literalDir)
	if err := os.MkdirAll(instancePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instancePath, ".credentials.json"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{
		"work": {Name: "work", Dir: literalDir},
	}}); err != nil {
		t.Fatal(err)
	}
	staged, err := stageProfileInstancePaths([]string{instancePath})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(instancePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instancePath, ".credentials.json"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = store.ReconcileProfileInstanceStagesContext(t.Context())
	if err == nil || !strings.Contains(err.Error(), "credential changed") {
		t.Fatalf("replacement reconciliation = %v, want fail-closed ambiguity", err)
	}
	got, readErr := os.ReadFile(filepath.Join(instancePath, ".credentials.json"))
	if readErr != nil || string(got) != "replacement" {
		t.Fatalf("replacement after reconciliation = %q, err %v", got, readErr)
	}
	if _, stageErr := os.Stat(staged[0].stagedPath); stageErr != nil {
		t.Fatalf("ambiguous original stage was not preserved: %v", stageErr)
	}
}

func TestRestartReconciliationDoesNotRestoreLegacyStageOverCanonicalReplacement(t *testing.T) {
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	dir := "work[1]*"
	canonicalPath := filepath.Join(store.InstancesDir(), dir)
	legacyRoot, ok := store.legacyInstancePath(store.InstancesDir())
	if !ok {
		t.Fatal("test store did not expose legacy instance root")
	}
	legacyPath := filepath.Join(legacyRoot, dir)
	if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{
		"work": {Name: "work", Dir: dir},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(legacyPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyPath, ".credentials.json"), []byte("old-legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	staged, err := stageProfileInstancePaths([]string{legacyPath})
	if err != nil || len(staged) != 1 {
		t.Fatalf("stage legacy = %+v, err %v", staged, err)
	}
	if err := os.MkdirAll(canonicalPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonicalPath, ".credentials.json"), []byte("canonical-replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = store.ReconcileProfileInstanceStagesContext(t.Context())
	if err == nil || !strings.Contains(err.Error(), "credential changed") {
		t.Fatalf("cross-path replacement reconciliation = %v", err)
	}
	if _, err := os.Lstat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale legacy stage was restored: %v", err)
	}
	if _, err := os.Stat(staged[0].stagedPath); err != nil {
		t.Fatalf("stale legacy stage was not preserved: %v", err)
	}
	got, readErr := os.ReadFile(filepath.Join(canonicalPath, ".credentials.json"))
	if readErr != nil || string(got) != "canonical-replacement" {
		t.Fatalf("canonical replacement = %q, err %v", got, readErr)
	}
	if preferred := store.PreferredInstancePath(canonicalPath); preferred != canonicalPath {
		t.Fatalf("stale legacy stage became active: preferred %q, want %q", preferred, canonicalPath)
	}
}

func TestRestartReconciliationRestoresCanonicalAndLegacyStagesAsOneProfile(t *testing.T) {
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	dir := "work[1]*"
	canonicalPath := filepath.Join(store.InstancesDir(), dir)
	legacyRoot, ok := store.legacyInstancePath(store.InstancesDir())
	if !ok {
		t.Fatal("test store did not expose legacy instance root")
	}
	legacyPath := filepath.Join(legacyRoot, dir)
	if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{
		"work": {Name: "work", Dir: dir},
	}}); err != nil {
		t.Fatal(err)
	}
	for path, payload := range map[string]string{canonicalPath: "canonical", legacyPath: "legacy"} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, ".credentials.json"), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	staged, err := stageProfileInstancePaths([]string{canonicalPath, legacyPath})
	if err != nil || len(staged) != 2 {
		t.Fatalf("stage two roots = %+v, err %v", staged, err)
	}
	if err := store.ReconcileProfileInstanceStagesContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{canonicalPath: "canonical", legacyPath: "legacy"} {
		got, err := os.ReadFile(filepath.Join(path, ".credentials.json"))
		if err != nil || string(got) != want {
			t.Fatalf("restored %q = %q, err %v", path, got, err)
		}
	}
	for _, entry := range staged {
		if _, err := os.Lstat(entry.stagingRoot); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("restored stage remains %q: %v", entry.stagingRoot, err)
		}
	}
}

func TestRestartReconciliationRollsForwardUnregisteredLiteralStage(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	root := store.InstancesDir()
	stageRoot := filepath.Join(root, ".work[1]*.remove-12345")
	writeOwnedRemovalStageForTest(t, filepath.Join(root, "work[1]*"), stageRoot, []byte("removed-secret"))
	if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileProfileInstanceStagesContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stageRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unregistered stage remains after roll-forward: %v", err)
	}
}

func TestRestartReconciliationRejectsUnownedOrInvalidStagesWithoutMutation(t *testing.T) {
	tests := []struct {
		name       string
		registered bool
		prepare    func(*testing.T, string, string)
	}{
		{
			name: "unmarked registered", registered: true,
			prepare: func(t *testing.T, original, root string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(root, profileRemovalStageEntryName), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unmarked unregistered",
			prepare: func(t *testing.T, original, root string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(root, profileRemovalStageEntryName), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "malformed manifest", registered: true,
			prepare: func(t *testing.T, original, root string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(root, profileRemovalStageEntryName), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, profileRemovalStageManifestName), []byte("{bad"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "legacy v1 manifest without strong provenance", registered: true,
			prepare: func(t *testing.T, original, root string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(root, profileRemovalStageEntryName), 0o700); err != nil {
					t.Fatal(err)
				}
				original, err := normalizedProfileRemovalPath(original)
				if err != nil {
					t.Fatal(err)
				}
				stage, err := normalizedProfileRemovalPath(root)
				if err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(map[string]any{
					"version":       1,
					"original_path": original,
					"staging_root":  stage,
					"entry_name":    profileRemovalStageEntryName,
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, profileRemovalStageManifestName), append(body, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink manifest",
			prepare: func(t *testing.T, original, root string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(root, profileRemovalStageEntryName), 0o700); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(filepath.Dir(root), "manifest-target")
				if err := os.WriteFile(target, []byte(`{"version":1}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(root, profileRemovalStageManifestName)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink stage root",
			prepare: func(t *testing.T, original, root string) {
				t.Helper()
				target := root + "-target"
				if err := os.MkdirAll(target, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, root); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "wrong identity", registered: true,
			prepare: func(t *testing.T, original, root string) {
				t.Helper()
				writeOwnedRemovalStageForTest(t, filepath.Join(filepath.Dir(original), "someone-else"), root, []byte("secret"))
			},
		},
		{
			name: "unexpected structure", registered: true,
			prepare: func(t *testing.T, original, root string) {
				t.Helper()
				writeOwnedRemovalStageForTest(t, original, root, []byte("secret"))
				if err := os.WriteFile(filepath.Join(root, "unexpected"), []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
			original := filepath.Join(store.InstancesDir(), "work[1]*")
			if test.registered {
				if err := os.MkdirAll(original, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(original, ".credentials.json"), []byte("live"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{
					"work": {Name: "work", Dir: "work[1]*"},
				}}); err != nil {
					t.Fatal(err)
				}
			} else if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{}}); err != nil {
				t.Fatal(err)
			}
			stageRoot := filepath.Join(filepath.Dir(original), ".work[1]*.remove-12345")
			test.prepare(t, original, stageRoot)
			if err := store.ReconcileProfileInstanceStagesContext(t.Context()); err == nil {
				t.Fatal("unsafe stage was accepted")
			}
			if _, err := os.Lstat(stageRoot); err != nil {
				t.Fatalf("unsafe stage was mutated: %v", err)
			}
			if test.registered {
				got, err := os.ReadFile(filepath.Join(original, ".credentials.json"))
				if err != nil || string(got) != "live" {
					t.Fatalf("registered live credential changed: %q, err %v", got, err)
				}
			}
		})
	}
}

func TestPreparedOwnedStageUsesRegistryAuthority(t *testing.T) {
	for _, registered := range []bool{true, false} {
		t.Run(fmt.Sprintf("registered=%v", registered), func(t *testing.T) {
			store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
			original := filepath.Join(store.InstancesDir(), "work[1]*")
			if registered {
				if err := os.MkdirAll(original, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(original, ".credentials.json"), []byte("live"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{"work": {Name: "work", Dir: "work[1]*"}}}); err != nil {
					t.Fatal(err)
				}
			} else if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{}}); err != nil {
				t.Fatal(err)
			}
			stageRoot := filepath.Join(filepath.Dir(original), ".work[1]*.remove-12345")
			writeOwnedRemovalStageForTest(t, original, stageRoot, nil)
			if err := store.ReconcileProfileInstanceStagesContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(stageRoot); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("prepared stage remains: %v", err)
			}
			if registered {
				if got, err := os.ReadFile(filepath.Join(original, ".credentials.json")); err != nil || string(got) != "live" {
					t.Fatalf("registered credential = %q, err %v", got, err)
				}
			}
		})
	}
}

func TestExactCleanupAndVersionRejectUnmarkedStage(t *testing.T) {
	parent := t.TempDir()
	original := filepath.Join(parent, "work[1]*")
	stageRoot := filepath.Join(parent, ".work[1]*.remove-12345")
	if err := os.MkdirAll(filepath.Join(stageRoot, profileRemovalStageEntryName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageRoot, profileRemovalStageEntryName, ".credentials.json"), []byte("fabricated"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := Store{Dir: filepath.Join(parent, "store")}
	if _, err := store.profileCredentialVersionLocked(t.Context(), []string{original}, true); err == nil {
		t.Fatal("version discovery accepted unmarked stage")
	}
	expected, err := stagedProfileCredentialVersion([]string{original}, []profileCredentialVersionEntry{{
		originalPath:    original,
		instancePresent: true,
		payloadPresent:  true,
		payload:         []byte("fabricated"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanupExactStagedProfileCredentialLocked([]string{original}, expected); err == nil {
		t.Fatal("exact cleanup accepted unmarked stage")
	}
	if _, err := os.Lstat(stageRoot); err != nil {
		t.Fatalf("unmarked exact-cleanup candidate was mutated: %v", err)
	}
}

func TestExactRemovalPreflightsUnmarkedStageBeforeMatchingLiveDeletion(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	if err := store.ImportProfileCredential("work", CredentialInfo{AccessToken: "access", RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	snapshot, found, err := store.SnapshotProfileRemovalContext(t.Context(), "work")
	if err != nil || !found {
		t.Fatalf("snapshot = found %v, err %v", found, err)
	}
	instancePath := filepath.Join(store.InstancesDir(), snapshot.InstanceDir)
	stageRoot := filepath.Join(filepath.Dir(instancePath), "."+filepath.Base(instancePath)+".remove-12345")
	if err := os.MkdirAll(filepath.Join(stageRoot, profileRemovalStageEntryName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageRoot, profileRemovalStageEntryName, ".credentials.json"), []byte("fabricated"), 0o600); err != nil {
		t.Fatal(err)
	}
	completed, err := store.CompleteExactProfileRemovalContext(t.Context(), "work", snapshot)
	if completed || err == nil {
		t.Fatalf("unmarked preflight = completed %v, err %v", completed, err)
	}
	if _, found := store.FindProfile("work"); !found {
		t.Fatal("unmarked preflight removed registry profile")
	}
	credential, readErr := os.ReadFile(filepath.Join(instancePath, ".credentials.json"))
	if readErr != nil || !strings.Contains(string(credential), "access") {
		t.Fatalf("unmarked preflight removed live credential: %q, err %v", credential, readErr)
	}
	if _, err := os.Lstat(stageRoot); err != nil {
		t.Fatalf("unmarked preflight mutated candidate: %v", err)
	}
}

func TestExactProfileRemovalPrefersStagedCredentialOverDivergentKeychain(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Keychain is only available on Darwin")
	}
	store := Store{Dir: filepath.Join(t.TempDir(), ".subrouter", "codex")}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"claudeAiOauth":{"accessToken":"original","refreshToken":"original-refresh"}}`)
	if err := os.WriteFile(filepath.Join(instancePath, ".credentials.json"), original, 0o600); err != nil {
		t.Fatal(err)
	}

	fakeBin := t.TempDir()
	securityPath := filepath.Join(fakeBin, "security")
	script := `#!/bin/sh
if [ "$1" = "delete-generic-password" ]; then
  exit 0
fi
case "$*" in
  *"$SUBROUTER_STAGED_KEYCHAIN_SERVICE"*)
    printf '%s\n' '{"claudeAiOauth":{"accessToken":"divergent","refreshToken":"divergent-refresh"}}'
    exit 0
    ;;
esac
exit 44
`
	if err := os.WriteFile(securityPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SUBROUTER_STAGED_KEYCHAIN_SERVICE", "Claude Code-credentials-"+keychainHash(instancePath))

	snapshot, found, err := store.SnapshotProfileRemovalContext(t.Context(), "work")
	if err != nil || !found {
		t.Fatalf("snapshot = found %v, err %v", found, err)
	}
	instancePaths, err := store.profileInstancePaths(snapshot.InstanceDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stageProfileInstancePaths(instancePaths); err != nil {
		t.Fatal(err)
	}
	completed, err := store.CompleteExactProfileRemovalContext(t.Context(), "work", snapshot)
	if err != nil || !completed {
		t.Fatalf("staged replay = completed %v, err %v", completed, err)
	}
	if _, found := store.FindProfile("work"); found {
		t.Fatal("staged replay retained the deleted profile")
	}
}

func TestExactProfileRemovalRetainsRecoveryBoundaryOnRegistrySyncFailure(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	if err := store.ImportProfileCredential("work", CredentialInfo{AccessToken: "access", RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	snapshot, found, err := store.SnapshotProfileRemovalContext(t.Context(), "work")
	if err != nil || !found {
		t.Fatalf("snapshot = found %v, err %v", found, err)
	}
	want := errors.New("registry directory sync unavailable")
	store.syncDirectoryForTest = func(string) error { return want }
	completed, err := store.CompleteExactProfileRemovalContext(t.Context(), "work", snapshot)
	if completed || !errors.Is(err, want) {
		t.Fatalf("sync-failed removal = completed %v, err %v", completed, err)
	}
	if _, found := store.FindProfile("work"); found {
		t.Fatal("registry rename was not visible after injected directory-sync failure")
	}
	store.syncDirectoryForTest = nil
	completed, err = store.CompleteExactProfileRemovalContext(t.Context(), "work", snapshot)
	if err != nil || !completed {
		t.Fatalf("replayed removal = completed %v, err %v", completed, err)
	}
}

func TestExactRemovalRegistryAbsentPreservesLiveReplacementAndCleansOwnedOldStage(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	if err := store.ImportProfileCredential("work", CredentialInfo{AccessToken: "old-access", RefreshToken: "old-refresh"}); err != nil {
		t.Fatal(err)
	}
	snapshot, found, err := store.SnapshotProfileRemovalContext(t.Context(), "work")
	if err != nil || !found {
		t.Fatalf("snapshot = found %v, err %v", found, err)
	}
	instancePaths, err := store.profileInstancePaths(snapshot.InstanceDir)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := stageProfileInstancePaths(instancePaths)
	if err != nil || len(staged) != 1 {
		t.Fatalf("stage old credential = %+v, err %v", staged, err)
	}
	if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{}}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(staged[0].originalPath, 0o700); err != nil {
		t.Fatal(err)
	}
	replacement := []byte(`{"claudeAiOauth":{"accessToken":"replacement-access","refreshToken":"replacement-refresh"}}`)
	if err := os.WriteFile(filepath.Join(staged[0].originalPath, ".credentials.json"), replacement, 0o600); err != nil {
		t.Fatal(err)
	}

	completed, err := store.CompleteExactProfileRemovalContext(t.Context(), "work", snapshot)
	if completed || !errors.Is(err, ErrProfileRemovalCredentialChanged) {
		t.Fatalf("replacement replay = completed %v, err %v", completed, err)
	}
	got, readErr := os.ReadFile(filepath.Join(staged[0].originalPath, ".credentials.json"))
	if readErr != nil || string(got) != string(replacement) {
		t.Fatalf("live replacement after replay = %q, err %v", got, readErr)
	}
	if _, err := os.Lstat(staged[0].stagingRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned old stage remains after exact cleanup: %v", err)
	}
}

func TestExactRemovalDetectsIdenticalPayloadAddedAtLegacyPath(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), ".subrouter", "codex")}
	payload := []byte(`{"claudeAiOauth":{"accessToken":"same","refreshToken":"same-refresh"}}`)
	canonical, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, ".credentials.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, found, err := store.SnapshotProfileRemovalContext(t.Context(), "work")
	if err != nil || !found {
		t.Fatalf("snapshot = found %v, err %v", found, err)
	}
	legacyRoot, ok := store.legacyInstancePath(store.InstancesDir())
	if !ok {
		t.Fatal("test store did not expose legacy instance root")
	}
	legacy := filepath.Join(legacyRoot, snapshot.InstanceDir)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, ".credentials.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	completed, err := store.CompleteExactProfileRemovalContext(t.Context(), "work", snapshot)
	if completed || !errors.Is(err, ErrProfileRemovalCredentialChanged) {
		t.Fatalf("identical legacy add race = completed %v, err %v", completed, err)
	}
	for _, path := range []string{canonical, legacy} {
		if got, err := os.ReadFile(filepath.Join(path, ".credentials.json")); err != nil || string(got) != string(payload) {
			t.Fatalf("preserved identical credential %q = %q, err %v", path, got, err)
		}
	}
}

func TestProductionRemovalPrimitivesRevalidateCredentialAfterStaging(t *testing.T) {
	for _, operation := range []string{"remove", "remove-unpublished", "complete-exact"} {
		t.Run(operation, func(t *testing.T) {
			store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
			if err := store.ImportProfileCredential("work", CredentialInfo{AccessToken: "old", RefreshToken: "old-refresh"}); err != nil {
				t.Fatal(err)
			}
			profile, found := store.FindProfile("work")
			if !found {
				t.Fatal("profile missing before removal")
			}
			instancePath := filepath.Join(store.InstancesDir(), profile.Dir)
			openCredential, err := os.OpenFile(filepath.Join(instancePath, ".credentials.json"), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer openCredential.Close()
			snapshot, found, err := store.SnapshotProfileRemovalContext(t.Context(), "work")
			if err != nil || !found {
				t.Fatalf("snapshot = found %v, err %v", found, err)
			}
			replacement := []byte(`{"claudeAiOauth":{"accessToken":"new","refreshToken":"new-refresh"}}`)
			var hookErr error
			store.afterProfileInstancesStagedForTest = func([]stagedProfileInstance) {
				if hookErr = openCredential.Truncate(0); hookErr != nil {
					return
				}
				if _, hookErr = openCredential.Seek(0, io.SeekStart); hookErr != nil {
					return
				}
				_, hookErr = openCredential.Write(replacement)
				if hookErr == nil {
					hookErr = openCredential.Sync()
				}
			}
			var removed bool
			switch operation {
			case "remove":
				removed, err = store.RemoveProfileContext(t.Context(), "work")
			case "remove-unpublished":
				removed, err = store.RemoveUnpublishedProfileContext(t.Context(), "work")
			case "complete-exact":
				removed, err = store.CompleteExactProfileRemovalContext(t.Context(), "work", snapshot)
			}
			if hookErr != nil {
				t.Fatalf("post-stage mutation: %v", hookErr)
			}
			if removed || !errors.Is(err, ErrProfileRemovalCredentialChanged) {
				t.Fatalf("post-stage mutation = removed %v, err %v", removed, err)
			}
			if _, found := store.FindProfile("work"); !found {
				t.Fatal("post-stage mutation deleted registry profile")
			}
			got, readErr := os.ReadFile(filepath.Join(instancePath, ".credentials.json"))
			if readErr != nil || string(got) != string(replacement) {
				t.Fatalf("restored concurrent credential = %q, err %v", got, readErr)
			}
			if roots, err := stagedProfileInstanceRoots(instancePath); err != nil || len(roots) != 0 {
				t.Fatalf("successful rollback stages = %v, err %v", roots, err)
			}
		})
	}
}

func TestPostStageLivePathReplacementFailsClosedBeforeRegistryCommit(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	if err := store.ImportProfileCredential("work", CredentialInfo{AccessToken: "old", RefreshToken: "old-refresh"}); err != nil {
		t.Fatal(err)
	}
	profile, found := store.FindProfile("work")
	if !found {
		t.Fatal("profile missing before removal")
	}
	instancePath := filepath.Join(store.InstancesDir(), profile.Dir)
	replacement := []byte(`{"claudeAiOauth":{"accessToken":"replacement","refreshToken":"replacement-refresh"}}`)
	var hookErr error
	store.afterProfileInstancesStagedForTest = func(staged []stagedProfileInstance) {
		if len(staged) != 1 {
			hookErr = fmt.Errorf("staged entries = %d", len(staged))
			return
		}
		if hookErr = os.MkdirAll(staged[0].originalPath, 0o700); hookErr == nil {
			hookErr = os.WriteFile(filepath.Join(staged[0].originalPath, ".credentials.json"), replacement, 0o600)
		}
	}
	removed, err := store.RemoveProfileContext(t.Context(), "work")
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if removed || !errors.Is(err, ErrProfileRemovalCredentialChanged) {
		t.Fatalf("live-path replacement = removed %v, err %v", removed, err)
	}
	if _, found := store.FindProfile("work"); !found {
		t.Fatal("live-path replacement deleted registry profile")
	}
	if got, err := os.ReadFile(filepath.Join(instancePath, ".credentials.json")); err != nil || string(got) != string(replacement) {
		t.Fatalf("live-path replacement = %q, err %v", got, err)
	}
	if roots, err := stagedProfileInstanceRoots(instancePath); err != nil || len(roots) != 1 {
		t.Fatalf("original recovery stage = %v, err %v", roots, err)
	}
}

func TestPostStageEmptyLivePathReappearanceFailsEvenWhenHashMatches(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	var hookErr error
	store.afterProfileInstancesStagedForTest = func(staged []stagedProfileInstance) {
		if len(staged) != 1 {
			hookErr = fmt.Errorf("staged entries = %d", len(staged))
			return
		}
		hookErr = os.MkdirAll(staged[0].originalPath, 0o700)
	}
	removed, err := store.RemoveProfileContext(t.Context(), "work")
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if removed || !errors.Is(err, ErrProfileRemovalCredentialChanged) {
		t.Fatalf("empty live-path reappearance = removed %v, err %v", removed, err)
	}
	if _, found := store.FindProfile("work"); !found {
		t.Fatal("empty live-path reappearance deleted registry profile")
	}
	if info, err := os.Stat(instancePath); err != nil || !info.IsDir() {
		t.Fatalf("empty replacement instance = %v, err %v", info, err)
	}
	if roots, err := stagedProfileInstanceRoots(instancePath); err != nil || len(roots) != 1 {
		t.Fatalf("empty original recovery stage = %v, err %v", roots, err)
	}
}

func TestLivePathReappearanceAfterPostStageHashFailsFinalBoundary(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	var hookErr error
	store.afterPostStageCredentialHashForTest = func() {
		hookErr = os.MkdirAll(instancePath, 0o700)
	}
	removed, err := store.RemoveProfileContext(t.Context(), "work")
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if removed || !errors.Is(err, ErrProfileRemovalCredentialChanged) {
		t.Fatalf("post-hash live-path reappearance = removed %v, err %v", removed, err)
	}
	if _, found := store.FindProfile("work"); !found {
		t.Fatal("post-hash live-path reappearance deleted registry profile")
	}
	if info, err := os.Stat(instancePath); err != nil || !info.IsDir() {
		t.Fatalf("post-hash replacement instance = %v, err %v", info, err)
	}
	if roots, err := stagedProfileInstanceRoots(instancePath); err != nil || len(roots) != 1 {
		t.Fatalf("post-hash original recovery stage = %v, err %v", roots, err)
	}
}

func TestPostStageKeychainMutationFailsBeforeRegistryCommit(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Keychain is only available on Darwin")
	}
	store := Store{Dir: filepath.Join(t.TempDir(), ".subrouter", "codex")}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	keychainPayload := filepath.Join(t.TempDir(), "keychain-payload")
	oldPayload := []byte(`{"claudeAiOauth":{"accessToken":"old","refreshToken":"old-refresh"}}`)
	newPayload := []byte(`{"claudeAiOauth":{"accessToken":"new","refreshToken":"new-refresh"}}`)
	if err := os.WriteFile(keychainPayload, oldPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "security"), []byte("#!/bin/sh\ncat \"$SUBROUTER_TEST_KEYCHAIN_PAYLOAD\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_TEST_KEYCHAIN_PAYLOAD", keychainPayload)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var hookErr error
	store.afterProfileInstancesStagedForTest = func([]stagedProfileInstance) {
		hookErr = os.WriteFile(keychainPayload, newPayload, 0o600)
	}
	removed, err := store.RemoveProfileContext(t.Context(), "work")
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if removed || !errors.Is(err, ErrProfileRemovalCredentialChanged) {
		t.Fatalf("post-stage Keychain mutation = removed %v, err %v", removed, err)
	}
	if _, found := store.FindProfile("work"); !found {
		t.Fatal("post-stage Keychain mutation deleted registry profile")
	}
	if info, err := os.Stat(instancePath); err != nil || !info.IsDir() {
		t.Fatalf("post-stage Keychain rollback instance = %v, err %v", info, err)
	}
}

func TestPostStageKeychainOnlyLiveDirectoryReappearanceFailsClosed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Keychain is only available on Darwin")
	}
	store := Store{Dir: filepath.Join(t.TempDir(), ".subrouter", "codex")}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	keychainPayload := filepath.Join(t.TempDir(), "keychain-payload")
	if err := os.WriteFile(keychainPayload, []byte(`{"claudeAiOauth":{"accessToken":"same","refreshToken":"same-refresh"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "security"), []byte("#!/bin/sh\ncat \"$SUBROUTER_TEST_KEYCHAIN_PAYLOAD\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_TEST_KEYCHAIN_PAYLOAD", keychainPayload)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var hookErr error
	store.afterProfileInstancesStagedForTest = func(staged []stagedProfileInstance) {
		if len(staged) != 1 {
			hookErr = fmt.Errorf("staged entries = %d", len(staged))
			return
		}
		hookErr = os.MkdirAll(staged[0].originalPath, 0o700)
	}
	removed, err := store.RemoveProfileContext(t.Context(), "work")
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if removed || !errors.Is(err, ErrProfileRemovalCredentialChanged) {
		t.Fatalf("Keychain-only live-path reappearance = removed %v, err %v", removed, err)
	}
	if _, found := store.FindProfile("work"); !found {
		t.Fatal("Keychain-only live-path reappearance deleted registry profile")
	}
	if info, err := os.Stat(instancePath); err != nil || !info.IsDir() {
		t.Fatalf("Keychain-only replacement instance = %v, err %v", info, err)
	}
	if roots, err := stagedProfileInstanceRoots(instancePath); err != nil || len(roots) != 1 {
		t.Fatalf("Keychain-only original recovery stage = %v, err %v", roots, err)
	}
}

func TestExactRemovalDetectsIdenticalPayloadRemovedFromLegacyPath(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), ".subrouter", "codex")}
	payload := []byte(`{"claudeAiOauth":{"accessToken":"same","refreshToken":"same-refresh"}}`)
	canonical, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	legacyRoot, ok := store.legacyInstancePath(store.InstancesDir())
	if !ok {
		t.Fatal("test store did not expose legacy instance root")
	}
	legacy := filepath.Join(legacyRoot, "work")
	for _, path := range []string{canonical, legacy} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, ".credentials.json"), payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, found, err := store.SnapshotProfileRemovalContext(t.Context(), "work")
	if err != nil || !found {
		t.Fatalf("snapshot = found %v, err %v", found, err)
	}
	if err := os.RemoveAll(legacy); err != nil {
		t.Fatal(err)
	}
	completed, err := store.CompleteExactProfileRemovalContext(t.Context(), "work", snapshot)
	if completed || !errors.Is(err, ErrProfileRemovalCredentialChanged) {
		t.Fatalf("identical legacy remove race = completed %v, err %v", completed, err)
	}
	if got, err := os.ReadFile(filepath.Join(canonical, ".credentials.json")); err != nil || string(got) != string(payload) {
		t.Fatalf("canonical credential after remove race = %q, err %v", got, err)
	}
}

func TestExactRemovalDetectsKeychainOnlyPhysicalPathAddition(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Keychain is only available on Darwin")
	}
	store := Store{Dir: filepath.Join(t.TempDir(), ".subrouter", "codex")}
	canonical, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	securityPath := filepath.Join(fakeBin, "security")
	if err := os.WriteFile(securityPath, []byte("#!/bin/sh\nprintf '%s\\n' '{\"claudeAiOauth\":{\"accessToken\":\"same\",\"refreshToken\":\"same-refresh\"}}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	snapshot, found, err := store.SnapshotProfileRemovalContext(t.Context(), "work")
	if err != nil || !found {
		t.Fatalf("Keychain-only snapshot = found %v, err %v", found, err)
	}
	legacyRoot, ok := store.legacyInstancePath(store.InstancesDir())
	if !ok {
		t.Fatal("test store did not expose legacy instance root")
	}
	legacy := filepath.Join(legacyRoot, snapshot.InstanceDir)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	completed, err := store.CompleteExactProfileRemovalContext(t.Context(), "work", snapshot)
	if completed || !errors.Is(err, ErrProfileRemovalCredentialChanged) {
		t.Fatalf("Keychain-only physical add race = completed %v, err %v", completed, err)
	}
	for _, path := range []string{canonical, legacy} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("Keychain-only instance %q was not preserved: %v", path, err)
		}
	}
}

func TestExactStagedCleanupKeepsIdenticalPayloadsPathScoped(t *testing.T) {
	parent := t.TempDir()
	canonical := filepath.Join(parent, "canonical")
	legacy := filepath.Join(parent, "legacy")
	payload := []byte("identical")
	canonicalRoot := filepath.Join(parent, ".canonical.remove-12345")
	legacyRoot := filepath.Join(parent, ".legacy.remove-23456")
	writeOwnedRemovalStageForTest(t, canonical, canonicalRoot, payload)
	writeOwnedRemovalStageForTest(t, legacy, legacyRoot, payload)

	canonicalOnly, err := stagedProfileCredentialVersion(
		[]string{canonical, legacy},
		[]profileCredentialVersionEntry{{originalPath: canonical, instancePresent: true, payloadPresent: true, payload: payload}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanupExactStagedProfileCredentialLocked([]string{canonical, legacy}, canonicalOnly); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(canonicalRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact canonical stage remains: %v", err)
	}
	if _, err := os.Lstat(legacyRoot); err != nil {
		t.Fatalf("same-payload legacy stage was not preserved: %v", err)
	}

	writeOwnedRemovalStageForTest(t, canonical, canonicalRoot, payload)
	both, err := stagedProfileCredentialVersion(
		[]string{canonical, legacy},
		[]profileCredentialVersionEntry{
			{originalPath: canonical, instancePresent: true, payloadPresent: true, payload: payload},
			{originalPath: legacy, instancePresent: true, payloadPresent: true, payload: payload},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanupExactStagedProfileCredentialLocked([]string{canonical, legacy}, both); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{canonicalRoot, legacyRoot} {
		if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("combined exact stage %q remains: %v", root, err)
		}
	}
}

func TestImportProfileCredentialReportsVisibleCommitOnRegistrySyncFailure(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	installSuccessfulSecurityCommand(t)
	want := errors.New("registry directory sync unavailable")
	store.syncDirectoryForTest = func(string) error { return want }
	err := store.ImportProfileCredential("work", CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh",
	})
	if !errors.Is(err, ErrProfileRegistryWriteCommitted) || !errors.Is(err, want) {
		t.Fatalf("import sync failure = %v", err)
	}
	profile, found := store.FindProfile("work")
	if !found {
		t.Fatal("committed import is invisible in the registry")
	}
	credential, readErr := store.ReadCredential(t.Context(), store.PreferredInstancePath(filepath.Join(store.InstancesDir(), profile.Dir)))
	if readErr != nil || credential == nil || credential.AccessToken != "access" || credential.RefreshToken != "refresh" {
		t.Fatalf("committed import credential = %+v, err %v", credential, readErr)
	}
}

func TestProfileRegistryMutationsReportVisibleCommitOnDirectorySyncFailure(t *testing.T) {
	want := errors.New("registry directory sync unavailable")
	assertCommitted := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, ErrProfileRegistryWriteCommitted) || !errors.Is(err, want) {
			t.Fatalf("registry sync failure = %v", err)
		}
	}

	t.Run("create", func(t *testing.T) {
		store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
		store.syncDirectoryForTest = func(string) error { return want }
		instancePath, err := store.CreateProfile("work")
		assertCommitted(t, err)
		if instancePath == "" {
			t.Fatal("committed create did not return its instance path")
		}
		if _, found := store.FindProfile("work"); !found {
			t.Fatal("committed create is invisible in the registry")
		}
	})

	t.Run("register", func(t *testing.T) {
		store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
		_, dir, err := store.CreateTempInstance()
		if err != nil {
			t.Fatal(err)
		}
		store.syncDirectoryForTest = func(string) error { return want }
		assertCommitted(t, store.RegisterProfile("work", dir))
		if _, found := store.FindProfile("work"); !found {
			t.Fatal("committed registration is invisible in the registry")
		}
	})

	t.Run("set-active", func(t *testing.T) {
		store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
		if _, err := store.CreateProfile("first"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateProfile("second"); err != nil {
			t.Fatal(err)
		}
		store.syncDirectoryForTest = func(string) error { return want }
		assertCommitted(t, store.SetActiveProfile("second"))
		if active := store.ActiveProfile(); active != "second" {
			t.Fatalf("visible active profile = %q, want second", active)
		}
	})
}

func TestRemoveProfileRollsForwardOnRegistrySyncFailure(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "claude-store")}
	installSuccessfulSecurityCommand(t)
	if err := store.ImportProfileCredential("work", CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh",
	}); err != nil {
		t.Fatal(err)
	}
	profile, found := store.FindProfile("work")
	if !found {
		t.Fatal("profile missing before removal")
	}
	instancePath := store.PreferredInstancePath(filepath.Join(store.InstancesDir(), profile.Dir))
	want := errors.New("registry directory sync unavailable")
	store.syncDirectoryForTest = func(string) error { return want }
	removed, err := store.RemoveProfileContext(t.Context(), "work")
	if !removed || err != nil {
		t.Fatalf("sync-uncertain removal = removed %v, err %v", removed, err)
	}
	if _, found := store.FindProfile("work"); found {
		t.Fatal("sync-uncertain removal retained a visible registry entry")
	}
	if _, err := os.Lstat(instancePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sync-uncertain removal retained canonical credential: %v", err)
	}
	matches, globErr := filepath.Glob(filepath.Join(filepath.Dir(instancePath), "."+filepath.Base(instancePath)+".remove-*"))
	if globErr != nil || len(matches) != 0 {
		t.Fatalf("sync-uncertain removal retained staged secrets %v, err %v", matches, globErr)
	}
}

func TestStagedProfileRecoveryTreatsMetacharacterDirectoryLiterally(t *testing.T) {
	parent := t.TempDir()
	instancePath := filepath.Join(parent, "work[1]*")
	targetRoot := filepath.Join(parent, ".work[1]*.remove-12345")
	siblingRoot := filepath.Join(parent, ".work1-other.remove-23456")
	nestedSiblingRoot := filepath.Join(parent, ".work[1]*.remove-extra.remove-34567")
	targetPayload := []byte(`{"claudeAiOauth":{"accessToken":"target"}}`)
	siblingPayload := []byte(`{"claudeAiOauth":{"accessToken":"sibling"}}`)
	writeOwnedRemovalStageForTest(t, instancePath, targetRoot, targetPayload)
	writeOwnedRemovalStageForTest(t, filepath.Join(parent, "work1-other"), siblingRoot, siblingPayload)
	writeOwnedRemovalStageForTest(t, filepath.Join(parent, "work[1]*.remove-extra"), nestedSiblingRoot, siblingPayload)

	store := Store{Dir: filepath.Join(parent, "store")}
	version, err := store.profileCredentialVersionLocked(t.Context(), []string{instancePath}, true)
	if err != nil {
		t.Fatal(err)
	}
	want, err := stagedProfileCredentialVersion([]string{instancePath}, []profileCredentialVersionEntry{{
		originalPath:    instancePath,
		instancePresent: true,
		payloadPresent:  true,
		payload:         targetPayload,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if version != want {
		t.Fatalf("staged version = %q, want literal target %q", version, want)
	}
	if err := cleanupExactStagedProfileCredentialLocked([]string{instancePath}, version); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(targetRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("literal target stage remains: %v", err)
	}
	if _, err := os.Lstat(siblingRoot); err != nil {
		t.Fatalf("glob-like sibling was touched: %v", err)
	}
	if _, err := os.Lstat(nestedSiblingRoot); err != nil {
		t.Fatalf("prefix-colliding sibling was touched: %v", err)
	}

	writeOwnedRemovalStageForTest(t, instancePath, targetRoot, targetPayload)
	if err := deleteOrphanedStagedProfileInstances([]string{instancePath}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(targetRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("literal orphan stage remains: %v", err)
	}
	if _, err := os.Lstat(siblingRoot); err != nil {
		t.Fatalf("orphan cleanup touched glob-like sibling: %v", err)
	}
	if _, err := os.Lstat(nestedSiblingRoot); err != nil {
		t.Fatalf("orphan cleanup touched prefix-colliding sibling: %v", err)
	}
}

func writeOwnedRemovalStageForTest(t *testing.T, originalPath, stagingRoot string, payload []byte) {
	t.Helper()
	credentialSetVersion, err := filesystemProfileCredentialVersion([]string{originalPath})
	if payload != nil {
		credentialSetVersion, err = stagedProfileCredentialVersion([]string{originalPath}, []profileCredentialVersionEntry{{
			originalPath:    originalPath,
			instancePresent: true,
			payloadPresent:  true,
			payload:         payload,
		}})
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := stagedProfileInstance{
		originalPath:         originalPath,
		stagedPath:           filepath.Join(stagingRoot, profileRemovalStageEntryName),
		stagingRoot:          stagingRoot,
		operationID:          "00112233445566778899aabbccddeeff",
		credentialSetVersion: credentialSetVersion,
	}
	if err := writeProfileRemovalStageManifest(entry); err != nil {
		t.Fatal(err)
	}
	if payload == nil {
		return
	}
	if err := os.MkdirAll(entry.stagedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	// The marker identity remains bound to the eventual live path even while
	// the directory containing it is staged.
	original, err := normalizedProfileRemovalPath(entry.originalPath)
	if err != nil {
		t.Fatal(err)
	}
	markerBody, err := json.Marshal(profileRemovalOperationMarker{
		Version:              profileRemovalOperationMarkerVersion,
		OriginalPath:         original,
		OperationID:          entry.operationID,
		CredentialSetVersion: entry.credentialSetVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry.stagedPath, profileRemovalOperationMarkerName), append(markerBody, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry.stagedPath, ".credentials.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func installSuccessfulSecurityCommand(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return
	}
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "security"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestSyncPrivateFileDirectoryPropagatesOpenFailure(t *testing.T) {
	want := errors.New("directory open unavailable")
	err := syncPrivateFileDirectoryForOS("darwin", t.TempDir(), func(string) (*os.File, error) {
		return nil, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("directory sync error = %v, want %v", err, want)
	}
	opened := false
	if err := syncPrivateFileDirectoryForOS("windows", `C:\state`, func(string) (*os.File, error) {
		opened = true
		return nil, want
	}); err != nil || opened {
		t.Fatalf("Windows directory sync = opened %v, err %v", opened, err)
	}
}

func TestRemoveProfileRollsBackWhenKeychainDeletionFails(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Keychain is only available on Darwin")
	}
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ImportProfileCredential("work", CredentialInfo{
		AccessToken:  "access",
		RefreshToken: "refresh",
	}); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	securityPath := filepath.Join(fakeBin, "security")
	if err := os.WriteFile(
		securityPath,
		[]byte("#!/bin/sh\necho 'forced keychain deletion failure' >&2\nexit 1\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	removed, removeErr := store.RemoveProfile("work")
	if removeErr == nil {
		t.Fatal("profile removal ignored the keychain deletion failure")
	}
	if removed {
		t.Fatal("failed profile removal was reported as committed")
	}
	if _, ok := store.FindProfile("work"); !ok {
		t.Fatal("keychain failure left the profile absent from the registry")
	}
	credential, err := store.ReadCredential(t.Context(), instancePath)
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil || credential.AccessToken != "access" || credential.RefreshToken != "refresh" {
		t.Fatalf("keychain failure did not restore the staged credential: %+v", credential)
	}
	stagingRoots, err := filepath.Glob(filepath.Join(filepath.Dir(instancePath), ".work.remove-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stagingRoots) != 0 {
		t.Fatalf("profile rollback left staging roots: %v", stagingRoots)
	}
}

func TestRemoveProfileContextUsesFreshDeadlineForRollback(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Keychain is only available on Darwin")
	}
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	fakeBin := t.TempDir()
	securityPath := filepath.Join(fakeBin, "security")
	if err := os.WriteFile(
		securityPath,
		[]byte("#!/bin/sh\ncase \"$1\" in\ndelete-generic-password) sleep 1 ;;\nesac\nexit 0\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ImportProfileCredential("work", CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh",
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	removed, removeErr := store.RemoveProfileContext(ctx, "work")
	if removeErr == nil {
		t.Fatal("profile removal ignored the expired keychain cleanup context")
	}
	if removed {
		t.Fatal("rolled-back profile removal was reported as committed")
	}
	if _, ok := store.FindProfile("work"); !ok {
		t.Fatal("fresh rollback context did not restore the profile registry")
	}
	credential, err := store.ReadCredential(t.Context(), instancePath)
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil || credential.AccessToken != "access" || credential.RefreshToken != "refresh" {
		t.Fatalf("fresh rollback context did not restore the credential: %+v", credential)
	}
}

func TestRemoveProfileReportsCommittedRemovalWhenCleanupAndRollbackFail(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Keychain is only available on Darwin")
	}
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ImportProfileCredential("work", CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh",
	}); err != nil {
		t.Fatal(err)
	}

	fakeBin := t.TempDir()
	securityPath := filepath.Join(fakeBin, "security")
	if err := os.WriteFile(
		securityPath,
		[]byte("#!/bin/sh\nif [ \"$1\" = \"find-generic-password\" ]; then exit 44; fi\nrm -rf \"$SUBROUTER_TEST_INSTANCE_PARENT\"/.work.remove-*\necho 'forced cleanup and rollback failure' >&2\nexit 1\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_TEST_INSTANCE_PARENT", filepath.Dir(instancePath))
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	removed, removeErr := store.RemoveProfile("work")
	if removeErr == nil {
		t.Fatal("profile removal ignored cleanup and rollback failures")
	}
	if !removed {
		t.Fatalf("durably removed profile was not reported as committed: %v", removeErr)
	}
	if _, ok := store.FindProfile("work"); ok {
		t.Fatal("failed rollback unexpectedly restored the profile registry")
	}
}
func TestCleanupInstanceDeletesMacOSKeychainCredentialAliases(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Keychain is only available on Darwin")
	}
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	fakeBin := t.TempDir()
	recordPath := filepath.Join(t.TempDir(), "security-arguments")
	securityPath := filepath.Join(fakeBin, "security")
	if err := os.WriteFile(securityPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SUBROUTER_KEYCHAIN_TEST_RECORD\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SUBROUTER_KEYCHAIN_TEST_RECORD", recordPath)

	instancePath, dir, err := store.CreateTempInstance()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupInstance(dir); err != nil {
		t.Fatal(err)
	}
	record, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	wantServices := []string{
		"Claude Code-credentials-" + keychainHash(instancePath),
		"Claude Code-credentials-" + keychainHash(filepath.Join(home, ".codex-accounts", "claude", dir)),
	}
	for _, wantService := range wantServices {
		if got := string(record); !strings.Contains(got, "delete-generic-password") || !strings.Contains(got, wantService) {
			t.Fatalf("Keychain cleanup = %q, want delete for %q", got, wantService)
		}
	}
}

func TestCredentialLocksDoNotCoupleDistinctProfiles(t *testing.T) {
	root := t.TempDir()
	seen := map[uint32]string{}
	var firstPath, secondPath string
	for index := 0; index < 10_000 && secondPath == ""; index++ {
		path := filepath.Join(root, fmt.Sprintf("tenant-%d", index), "profile")
		hasher := fnv.New32a()
		_, _ = hasher.Write([]byte(filepath.Clean(path) + ".credentials.lock"))
		bucket := hasher.Sum32() % 64
		if prior := seen[bucket]; prior != "" {
			firstPath, secondPath = prior, path
		} else {
			seen[bucket] = path
		}
	}
	if secondPath == "" {
		t.Fatal("could not find two profile paths in the same legacy lock shard")
	}
	firstLock, err := lockProfileCredential(context.Background(), firstPath)
	if err != nil {
		t.Fatal(err)
	}
	secondAcquired := make(chan *profileCredentialLock, 1)
	secondError := make(chan error, 1)
	go func() {
		lock, err := lockProfileCredential(context.Background(), secondPath)
		if err != nil {
			secondError <- err
			return
		}
		secondAcquired <- lock
	}()
	select {
	case secondLock := <-secondAcquired:
		if err := secondLock.Close(); err != nil {
			t.Fatal(err)
		}
		if err := firstLock.Close(); err != nil {
			t.Fatal(err)
		}
	case err := <-secondError:
		_ = firstLock.Close()
		t.Fatal(err)
	case <-time.After(500 * time.Millisecond):
		if err := firstLock.Close(); err != nil {
			t.Fatal(err)
		}
		secondLock := <-secondAcquired
		_ = secondLock.Close()
		t.Fatal("distinct profile locks were serialized by a shared process shard")
	}
}

func TestReadCredentialWaitsForProfileWriter(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ImportProfileCredential("work", CredentialInfo{
		AccessToken:  "access",
		RefreshToken: "refresh",
	}); err != nil {
		t.Fatal(err)
	}
	writerLock, err := lockProfileCredential(context.Background(), instancePath)
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := store.ReadCredential(t.Context(), instancePath)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		_ = writerLock.Close()
		t.Fatalf("credential read bypassed the active writer lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := writerLock.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("credential read remained blocked after writer released its lock")
	}
}

func TestReadCredentialLockWaitHonorsContextCancellation(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ImportProfileCredential("work", CredentialInfo{
		AccessToken:  "access",
		RefreshToken: "refresh",
	}); err != nil {
		t.Fatal(err)
	}
	writerLock, err := lockProfileCredential(context.Background(), instancePath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	readDone := make(chan error, 1)
	go func() {
		_, err := store.ReadCredential(ctx, instancePath)
		readDone <- err
	}()
	lockKey := filepath.Clean(profileInstancePathKey(instancePath) + ".credentials.lock")
	deadline := time.Now().Add(time.Second)
	for {
		profileCredentialProcessLocks.Lock()
		entry := profileCredentialProcessLocks.entries[lockKey]
		waiterRegistered := entry != nil && entry.refs >= 2
		profileCredentialProcessLocks.Unlock()
		if waiterRegistered {
			break
		}
		if time.Now().After(deadline) {
			_ = writerLock.Close()
			<-readDone
			t.Fatal("credential read did not begin waiting for the writer lock")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-readDone:
		_ = writerLock.Close()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("credential read error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		_ = writerLock.Close()
		<-readDone
		t.Fatal("credential read ignored context cancellation while waiting for its lock")
	}
}

func TestRemoveProfileRejectsRegistryPathsOutsideInstanceRoots(t *testing.T) {
	for _, dir := range []string{"../../outside", filepath.Join(t.TempDir(), "absolute-outside")} {
		t.Run(strings.ReplaceAll(dir, string(os.PathSeparator), "_"), func(t *testing.T) {
			root := t.TempDir()
			store := Store{Dir: filepath.Join(root, "store")}
			outside := filepath.Join(root, "outside")
			if filepath.IsAbs(dir) {
				outside = dir
			}
			if err := os.MkdirAll(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(outside, "sentinel")
			if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := store.writeProfiles(profilesFile{
				Active: "work",
				Profiles: map[string]Profile{
					"work": {Name: "work", Dir: dir},
				},
			}); err != nil {
				t.Fatal(err)
			}

			removed, err := store.RemoveProfile("work")
			if err == nil {
				t.Fatal("unsafe profile directory was accepted")
			}
			if removed {
				t.Fatal("profile with an unsafe directory was reported removed")
			}
			if _, err := os.Stat(sentinel); err != nil {
				t.Fatalf("outside sentinel was changed: %v", err)
			}
			if _, ok := store.FindProfile("work"); !ok {
				t.Fatal("unsafe profile was removed from the registry")
			}
		})
	}
}

func TestReadCredentialFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"tok","refreshToken":"refresh","subscriptionType":"pro"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	credential, ok := readCredentialFile(dir)
	if !ok {
		t.Fatal("credential not read")
	}
	if credential.AccessToken != "tok" {
		t.Fatalf("access token = %q, want tok", credential.AccessToken)
	}
	if credential.RefreshToken != "refresh" {
		t.Fatalf("refresh token = %q, want refresh", credential.RefreshToken)
	}
}

func TestRefreshCredentialPostsClaudeOAuthRefresh(t *testing.T) {
	originalURL := oauthTokenURL
	defer func() { oauthTokenURL = originalURL }()

	var sawRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("anthropic-beta"); got != oauthBetaHeader {
			t.Fatalf("anthropic-beta = %q, want %q", got, oauthBetaHeader)
		}
		if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Fatalf("Content-Type = %q, want JSON", got)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if got := payload["client_id"]; got != oauthClientID {
			t.Fatalf("client_id = %q, want %q", got, oauthClientID)
		}
		if got := payload["grant_type"]; got != "refresh_token" {
			t.Fatalf("grant_type = %q, want refresh_token", got)
		}
		if got := payload["refresh_token"]; got != "old-refresh" {
			t.Fatalf("refresh_token = %q, want old-refresh", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
		})
	}))
	defer server.Close()
	oauthTokenURL = server.URL

	got, err := RefreshCredential(context.Background(), server.Client(), CredentialInfo{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawRequest {
		t.Fatal("refresh endpoint was not called")
	}
	if got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" {
		t.Fatalf("tokens = %q/%q, want new-access/new-refresh", got.AccessToken, got.RefreshToken)
	}
	if got.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("ExpiresAt = %d, want future", got.ExpiresAt)
	}
}

// Claude refresh tokens are single-use. Concurrent callers that each read the
// same stored refresh token and race to redeem it get one winner and one
// invalidated credential, which logs the profile out. Concurrent refreshes of
// one profile must therefore serialize across the network round-trip, so that
// every caller after the first observes the winner's fresh credential and
// skips the network call entirely.
func TestRefreshCredentialIfExpiredRedeemsRefreshTokenOnce(t *testing.T) {
	originalURL := oauthTokenURL
	defer func() { oauthTokenURL = originalURL }()

	var mu sync.Mutex
	redeemed := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		token := payload["refresh_token"]

		mu.Lock()
		redeemed[token]++
		reuse := redeemed[token] > 1
		mu.Unlock()

		// Model the upstream's single-use semantics: a token that has already
		// been redeemed is dead, and redeeming it again is an error.
		if reuse {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		// Slow enough that unserialized callers would overlap inside the
		// network call rather than lining up behind each other by accident.
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}`)
	}))
	defer server.Close()
	oauthTokenURL = server.URL

	store := Store{Dir: t.TempDir()}
	if err := store.ImportProfileCredential("race@example.com", CredentialInfo{
		AccessToken:  "stale-access",
		RefreshToken: "stale-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	profile, ok := store.FindProfile("race@example.com")
	if !ok {
		t.Fatal("profile not found")
	}

	const callers = 8
	type result struct {
		account    accounts.Account
		didRefresh bool
		err        error
	}
	results := make([]result, callers)
	var publications atomic.Int32
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := range results {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			account, _, didRefresh, err := store.RefreshCredentialDetailsIfExpiredBeforeRefresh(
				context.Background(), server.Client(), profile,
				func() error {
					publications.Add(1)
					return nil
				},
			)
			results[i] = result{account: account, didRefresh: didRefresh, err: err}
		}(i)
	}
	start.Done()
	done.Wait()

	refreshes := 0
	for i, got := range results {
		if got.err != nil {
			t.Fatalf("caller %d failed: %v", i, got.err)
		}
		if got.account.Token != "fresh-access" {
			t.Fatalf("caller %d token = %q, want fresh-access", i, got.account.Token)
		}
		if got.didRefresh {
			refreshes++
		}
	}
	if refreshes != 1 {
		t.Fatalf("network refreshes reported = %d, want exactly 1", refreshes)
	}
	if publications.Load() != 1 {
		t.Fatalf("refresh publications = %d, want exactly 1", publications.Load())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(redeemed) != 1 {
		t.Fatalf("distinct refresh tokens redeemed = %d (%v), want 1", len(redeemed), redeemed)
	}
	if got := redeemed["stale-refresh"]; got != 1 {
		t.Fatalf("stale-refresh redeemed %d times, want exactly 1", got)
	}
}

func TestForceRefreshCredentialRefreshesFreshProfile(t *testing.T) {
	originalURL := oauthTokenURL
	defer func() { oauthTokenURL = originalURL }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"forced-access","refresh_token":"forced-refresh","expires_in":3600}`)
	}))
	defer server.Close()
	oauthTokenURL = server.URL

	store := Store{Dir: t.TempDir()}
	if err := store.ImportProfileCredential("force@example.com", CredentialInfo{
		AccessToken:  "still-fresh-access",
		RefreshToken: "still-fresh-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	profile, ok := store.FindProfile("force@example.com")
	if !ok {
		t.Fatal("profile not found")
	}
	account, didRefresh, err := store.ForceRefreshCredential(context.Background(), server.Client(), profile)
	if err != nil {
		t.Fatal(err)
	}
	if !didRefresh || account.Token != "forced-access" {
		t.Fatalf("forced refresh result = didRefresh:%v token:%q", didRefresh, account.Token)
	}
	credential, err := store.ReadCredential(context.Background(), store.ClaudeConfigDir(profile.Name))
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil || credential.AccessToken != "forced-access" || credential.RefreshToken != "forced-refresh" {
		t.Fatalf("forced credential was not persisted: %+v", credential)
	}
}

func TestListAccountsReadsProfilesWithCredentials(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instancePath, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"tok","subscriptionType":"max","expiresAt":4102444800000}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("accounts = %d, want 1", len(got))
	}
	account := got[0]
	if account.ID != "work" {
		t.Fatalf("ID = %q, want work", account.ID)
	}
	if account.Provider != accounts.ProviderClaude {
		t.Fatalf("Provider = %q, want claude", account.Provider)
	}
	if account.AuthMode != accounts.AuthModeOAuth {
		t.Fatalf("AuthMode = %q, want oauth", account.AuthMode)
	}
	if account.Token != "tok" {
		t.Fatalf("Token = %q, want tok", account.Token)
	}
	if account.Source != filepath.Clean(instancePath) {
		t.Fatalf("Source = %q, want %q", account.Source, filepath.Clean(instancePath))
	}
}

// Anthropic rejects subscription-OAuth Messages calls that do not look like
// Claude Code with a headerless 429 regardless of quota (verified live
// 2026-07-04 on a fresh Max 20x account). The probe must carry the Claude Code
// system prompt, beta tag, and client identity, or every account's fable pool
// reads as exhausted.
func TestFetchFableUsageWindowsSendsClaudeCodeShape(t *testing.T) {
	var gotBody string
	var gotBeta, gotUA, gotXApp string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotBeta = r.Header.Get("anthropic-beta")
		gotUA = r.Header.Get("User-Agent")
		gotXApp = r.Header.Get("x-app")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d_Oi-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d_Oi-Utilization", "0.0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
	defer server.Close()
	restore := messagesURL
	messagesURL = server.URL + "/v1/messages"
	defer func() { messagesURL = restore }()

	windows, err := FetchFableUsageWindows(context.Background(), server.Client(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, "You are Claude Code") {
		t.Fatalf("probe body missing Claude Code system prompt: %s", gotBody)
	}
	if !strings.Contains(gotBeta, "claude-code-") || !strings.Contains(gotBeta, oauthBetaHeader) {
		t.Fatalf("probe beta header = %q, want claude-code tag plus oauth beta", gotBeta)
	}
	if !strings.Contains(gotUA, "claude-cli") || gotXApp != "cli" {
		t.Fatalf("probe client identity = UA %q x-app %q, want claude-cli/cli", gotUA, gotXApp)
	}
	if len(windows) != 1 || windows[0].Feature != FableFeature || windows[0].UsedPercent != 0 {
		t.Fatalf("windows = %+v, want one fresh fable window", windows)
	}
}

// A headerless 429 must return no windows (unknown), never a synthesized
// exhausted window.
func TestFetchFableUsageWindowsHeaderless429ReturnsNoWindows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`))
	}))
	defer server.Close()
	restore := messagesURL
	messagesURL = server.URL + "/v1/messages"
	defer func() { messagesURL = restore }()

	windows, err := FetchFableUsageWindows(context.Background(), server.Client(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 0 {
		t.Fatalf("windows = %+v, want none for a headerless 429", windows)
	}
}

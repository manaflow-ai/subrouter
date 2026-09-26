package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	agentkimi "github.com/manaflow-ai/subrouter/internal/agents/kimi"
	"github.com/manaflow-ai/subrouter/selectacct"
)

type transactionLockingOAuthSource struct {
	storeDir string
	entered  chan struct{}
	once     sync.Once
	account  accounts.Account
}

func (s *transactionLockingOAuthSource) Provider() accounts.Provider {
	return accounts.ProviderKimi
}

func (s *transactionLockingOAuthSource) ListAccounts(context.Context) ([]accounts.Account, error) {
	return []accounts.Account{s.account}, nil
}

func (s *transactionLockingOAuthSource) RefreshAccount(ctx context.Context, _ *http.Client, account accounts.Account) (accounts.Account, error) {
	s.once.Do(func() { close(s.entered) })
	lock, err := lockAccountImportTransaction(ctx, s.storeDir)
	if err != nil {
		return account, err
	}
	if err := lock.Close(); err != nil {
		return account, err
	}
	return account, nil
}

func publicationFailingAccountServer(t *testing.T) (Server, accounts.CodexStore, agentclaude.Store, agentkimi.Store, error) {
	t.Helper()
	root := t.TempDir()
	codexStore := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	kimiStore := agentkimi.Store{
		Path:       filepath.Join(root, "kimi", "cli.json"),
		ManagedDir: filepath.Join(root, "kimi", "managed"),
	}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = claudeStore
	ref.oauthSources = []OAuthAccountSource{kimiStore}
	want := errors.New("generation publication unavailable")
	ref.publishGenerationForTest = func(string) error { return want }
	return Server{AccountRef: ref}, codexStore, claudeStore, kimiStore, want
}

func TestAccountImportPublicationFailurePreservesCredentials(t *testing.T) {
	t.Run("Codex OAuth repair", func(t *testing.T) {
		server, store, _, _, want := publicationFailingAccountServer(t)
		before := proxyStoredOAuthAccount("owner@example.com", "before", time.Now().Add(time.Hour))
		if err := store.SaveStored(before); err != nil {
			t.Fatal(err)
		}
		after := proxyStoredOAuthAccount(before.Email, "after", time.Now().Add(time.Hour))
		_, err := server.installImportedAccount(t.Context(), accountImportRequest{
			Provider: accounts.ProviderCodex,
			Codex:    &after,
		})
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want publication failure", err)
		}
		stored, ok, err := store.FindStored(before.Email)
		if err != nil || !ok {
			t.Fatalf("stored account = found:%v err:%v", ok, err)
		}
		if stored.Auth.Tokens == nil || stored.Auth.Tokens.RefreshToken != before.Auth.Tokens.RefreshToken {
			t.Fatal("publication failure changed the Codex refresh-token chain")
		}
	})

	t.Run("API key repair", func(t *testing.T) {
		server, store, _, _, want := publicationFailingAccountServer(t)
		before := accounts.StoredCodexAccount{
			Email:    "apikey:work",
			Provider: accounts.ProviderCodex,
			Auth:     accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-before"},
		}
		if err := store.SaveStored(before); err != nil {
			t.Fatal(err)
		}
		after := before
		after.Auth.OpenAIAPIKey = "sk-after"
		_, err := server.installImportedAccount(t.Context(), accountImportRequest{
			Provider: accounts.ProviderCodex,
			Codex:    &after,
		})
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want publication failure", err)
		}
		stored, ok, err := store.FindStored(before.Email)
		if err != nil || !ok || stored.Auth.OpenAIAPIKey != before.Auth.OpenAIAPIKey {
			t.Fatalf("publication failure changed API-key state: found:%v err:%v", ok, err)
		}
	})

	t.Run("Claude OAuth repair", func(t *testing.T) {
		server, _, store, _, want := publicationFailingAccountServer(t)
		before := agentclaude.CredentialInfo{AccessToken: "before-access", RefreshToken: "before-refresh"}
		if err := store.ImportProfileCredential("work", before); err != nil {
			t.Fatal(err)
		}
		after := agentclaude.CredentialInfo{AccessToken: "after-access", RefreshToken: "after-refresh"}
		_, err := server.installImportedAccount(t.Context(), accountImportRequest{
			Provider: accounts.ProviderClaude,
			Claude:   &claudeAccountImport{Name: "work", Credential: after},
		})
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want publication failure", err)
		}
		profile, ok := store.FindProfile("work")
		if !ok {
			t.Fatal("Claude profile disappeared")
		}
		stored, err := store.ReadCredential(context.Background(), filepath.Join(store.InstancesDir(), profile.Dir))
		if err != nil || stored == nil || stored.RefreshToken != before.RefreshToken {
			t.Fatalf("publication failure changed Claude credential: present:%v err:%v", stored != nil, err)
		}
	})

	t.Run("Kimi OAuth repair", func(t *testing.T) {
		server, _, _, store, want := publicationFailingAccountServer(t)
		before := agentkimi.CredentialInfo{
			AccessToken: "before-access", RefreshToken: "before-refresh",
			OAuthDeviceID: "0123456789abcdef", ExpiresAt: time.Now().Add(time.Hour),
		}
		if _, err := store.SaveManagedCredential("work", before); err != nil {
			t.Fatal(err)
		}
		after := before
		after.AccessToken = "after-access"
		after.RefreshToken = "after-refresh"
		_, err := server.installImportedAccount(t.Context(), accountImportRequest{
			Provider: accounts.ProviderKimi,
			Kimi:     &kimiAccountImport{Label: "work", Credential: after},
		})
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want publication failure", err)
		}
		stored, ok, err := store.ReadManagedCredential("work", time.Now())
		if err != nil || !ok || stored.RefreshToken != before.RefreshToken {
			t.Fatalf("publication failure changed Kimi credential: found:%v err:%v", ok, err)
		}
	})

	t.Run("Kimi removal", func(t *testing.T) {
		server, _, _, store, want := publicationFailingAccountServer(t)
		before := agentkimi.CredentialInfo{
			AccessToken: "before-access", RefreshToken: "before-refresh",
			OAuthDeviceID: "0123456789abcdef", ExpiresAt: time.Now().Add(time.Hour),
		}
		if _, err := store.SaveManagedCredential("work", before); err != nil {
			t.Fatal(err)
		}
		_, err := server.installImportedAccount(t.Context(), accountImportRequest{
			Provider: accounts.ProviderKimi,
			Kimi:     &kimiAccountImport{Label: "work", Remove: true},
		})
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want publication failure", err)
		}
		stored, ok, err := store.ReadManagedCredential("work", time.Now())
		if err != nil || !ok || stored.RefreshToken != before.RefreshToken {
			t.Fatalf("publication failure removed Kimi credential: found:%v err:%v", ok, err)
		}
	})
}

func TestTenantAccountPublicationFailurePreservesCredentials(t *testing.T) {
	t.Run("Codex OAuth repair", func(t *testing.T) {
		server, store, _, _, _ := publicationFailingAccountServer(t)
		const email = "owner@example.com"
		beforeToken := proxyTestCodexJWT(email, "before", time.Now().Add(time.Hour))
		before := accounts.StoredCodexAccount{
			Email: email, Label: "work", Provider: accounts.ProviderCodex,
			Auth: accounts.CodexAuthFile{AuthMode: "chatgpt", Tokens: &accounts.CodexTokens{
				AccessToken: beforeToken, RefreshToken: "before-refresh", IDToken: beforeToken, AccountID: "owner",
			}},
		}
		if err := store.SaveStored(before); err != nil {
			t.Fatal(err)
		}
		afterToken := proxyTestCodexJWT(email, "after", time.Now().Add(time.Hour))
		response := serveTenantAccountUpload(&server, fmt.Sprintf(`{
			"provider":"codex","accountId":%q,"targetAccountId":%q,"label":"work",
			"tokens":{"accessToken":%q,"refreshToken":"after-refresh","idToken":%q,"accountID":"owner"}
		}`, email, email, afterToken, afterToken))
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		stored, ok, err := store.FindStored(before.Email)
		if err != nil || !ok || !reflect.DeepEqual(stored.Auth.Tokens, before.Auth.Tokens) {
			t.Fatalf("publication failure changed tenant Codex credentials: found:%v err:%v", ok, err)
		}
	})

	t.Run("API key repair", func(t *testing.T) {
		server, store, _, _, _ := publicationFailingAccountServer(t)
		before := accounts.StoredCodexAccount{
			Email: "apikey:openai-apikey:work", Label: "work", Provider: accounts.ProviderCodex,
			Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-before"},
		}
		if err := store.SaveStored(before); err != nil {
			t.Fatal(err)
		}
		response := serveTenantAccountUpload(&server, `{"provider":"openai-apikey","label":"work","apiKey":"sk-after"}`)
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		stored, ok, err := store.FindStored(before.Email)
		if err != nil || !ok || stored.Auth.OpenAIAPIKey != before.Auth.OpenAIAPIKey {
			t.Fatalf("publication failure changed tenant API-key state: found:%v err:%v", ok, err)
		}
	})

	t.Run("Claude OAuth repair", func(t *testing.T) {
		server, _, store, _, _ := publicationFailingAccountServer(t)
		before := agentclaude.CredentialInfo{AccessToken: "before-access", RefreshToken: "before-refresh"}
		if _, err := store.UpsertCredentialProfile("work", before); err != nil {
			t.Fatal(err)
		}
		response := serveTenantAccountUpload(&server, `{
			"provider":"claude","label":"work",
			"claudeAiOauth":{"accessToken":"after-access","refreshToken":"after-refresh"}
		}`)
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		profile, ok := store.FindProfile("work")
		if !ok {
			t.Fatal("tenant Claude profile disappeared")
		}
		stored, err := store.ReadCredential(context.Background(), filepath.Join(store.InstancesDir(), profile.Dir))
		if err != nil || stored == nil || stored.RefreshToken != before.RefreshToken {
			t.Fatalf("publication failure changed tenant Claude credential: present:%v err:%v", stored != nil, err)
		}
	})
}

func TestTenantAccountRejectionDoesNotPublishGeneration(t *testing.T) {
	t.Parallel()
	t.Run("repair target validation", func(t *testing.T) {
		server, _, _, _, _ := publicationFailingAccountServer(t)
		published := 0
		server.AccountRef.publishGenerationForTest = func(string) error {
			published++
			return nil
		}
		response := serveTenantAccountUpload(&server, `{
			"provider":"openai-apikey","label":"work","apiKey":"sk-after",
			"targetAccountID":"apikey:openai-apikey:other"
		}`)
		if response.Code != http.StatusConflict {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		if published != 0 {
			t.Fatalf("rejected repair published %d account generations", published)
		}
	})

	t.Run("capacity", func(t *testing.T) {
		server, store, _, _, _ := publicationFailingAccountServer(t)
		for index := 0; index < maxAccountImportAccounts; index++ {
			account := accounts.StoredCodexAccount{
				Email:    fmt.Sprintf("apikey:seed-%03d", index),
				Provider: accounts.ProviderCodex,
				Auth: accounts.CodexAuthFile{
					AuthMode: "apikey", OpenAIAPIKey: fmt.Sprintf("sk-seed-%03d", index),
				},
			}
			if err := store.SaveStored(account); err != nil {
				t.Fatal(err)
			}
		}
		published := 0
		server.AccountRef.publishGenerationForTest = func(string) error {
			published++
			return nil
		}
		response := serveTenantAccountUpload(&server, `{"provider":"openai-apikey","label":"over-limit","apiKey":"sk-new"}`)
		if response.Code != http.StatusInsufficientStorage {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		if published != 0 {
			t.Fatalf("capacity rejection published %d account generations", published)
		}
	})
}

func TestTenantAccountUploadReportsUnavailableInventoryWithoutDetails(t *testing.T) {
	t.Run("Claude registry", func(t *testing.T) {
		root := t.TempDir()
		codexStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
		claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
		if err := os.MkdirAll(claudeStore.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(claudeStore.ProfilesPath(), []byte("{not-json"), 0o600); err != nil {
			t.Fatal(err)
		}
		ref := NewAccountRef(codexStore, nil, nil)
		ref.claudeStore = claudeStore
		server := Server{AccountRef: ref}
		response := serveTenantAccountUpload(&server, `{"provider":"openai-apikey","label":"work","apiKey":"sk-new"}`)
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "account inventory unavailable for claude") {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), claudeStore.ProfilesPath()) || strings.Contains(response.Body.String(), "profiles.json") {
			t.Fatalf("tenant Claude inventory error leaked details: %s", response.Body.String())
		}
	})

	t.Run("Kimi inventory", func(t *testing.T) {
		root := t.TempDir()
		codexStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
		claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
		kimiStore := agentkimi.Store{
			Path: filepath.Join(root, "kimi", "cli.json"), ManagedDir: filepath.Join(root, "kimi", "managed"),
		}
		if err := os.MkdirAll(filepath.Dir(kimiStore.ManagedDir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(kimiStore.ManagedDir, []byte("not-a-directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		ref := NewAccountRef(codexStore, nil, nil)
		ref.claudeStore = claudeStore
		ref.oauthSources = []OAuthAccountSource{kimiStore}
		server := Server{AccountRef: ref}
		response := serveTenantAccountUpload(&server, `{"provider":"openai-apikey","label":"work","apiKey":"sk-new"}`)
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "account inventory unavailable for kimi") {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), kimiStore.ManagedDir) {
			t.Fatalf("tenant Kimi inventory error leaked details: %s", response.Body.String())
		}
	})
}

func TestMutationStageErrorReconcilesDurablePartialOutcome(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	oldAccount := accounts.StoredCodexAccount{
		Email: "apikey:old", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-old"},
	}
	if err := store.SaveStored(oldAccount); err != nil {
		t.Fatal(err)
	}
	initial, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(store, initial, nil)
	ref.claudeStore = agentclaude.Store{Dir: filepath.Join(t.TempDir(), "claude")}
	ref.usageStatusAt = time.Now()
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: oldAccount.Email, Provider: accounts.ProviderCodex,
		Headroom: 0.9, ShortHeadroom: 0.9,
	}}))
	server := Server{
		AccountRef: ref, SchedulerRef: schedulerRef,
		ScoreAccounts: func(_ context.Context, available []accounts.Account) ([]selectacct.Score, int) {
			scores := make([]selectacct.Score, 0, len(available))
			for _, account := range available {
				scores = append(scores, selectacct.Score{
					AccountID: account.ID, Provider: account.Provider,
					Headroom: 0.37, ShortHeadroom: 0.37, Fresh: true,
				})
			}
			return scores, len(scores)
		},
	}
	newAccount := accounts.StoredCodexAccount{
		Email: "apikey:new", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-new"},
	}
	want := errors.New("forced error after durable credential write")
	_, err = server.installAccountMutation(t.Context(), func() (string, func() error, error) {
		return newAccount.Email, func() error {
			if err := store.SaveStored(newAccount); err != nil {
				return err
			}
			return want
		}, nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want original mutation failure", err)
	}
	if _, ok, findErr := store.FindStored(newAccount.Email); findErr != nil || !ok {
		t.Fatalf("durable partial account = found:%v err:%v", ok, findErr)
	}
	loaded := ref.All()
	if len(loaded) != 2 {
		t.Fatalf("live accounts = %d, want durable old+new outcome: %+v", len(loaded), loaded)
	}
	if got := schedulerRef.Get().ScoreFor(accounts.ProviderCodex, newAccount.Email); got.Headroom != 0.37 {
		t.Fatalf("new durable account scheduler score = %v, want reconciled 0.37", got.Headroom)
	}
	if !ref.usageStatusAt.IsZero() {
		t.Fatal("mutation-stage failure left the usage-status cache valid")
	}
}

func TestClaudeMidMutationFailureDropsStaleLiveRoute(t *testing.T) {
	root := t.TempDir()
	codexStore := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	before := agentclaude.CredentialInfo{AccessToken: "before-access", RefreshToken: "before-refresh"}
	if err := claudeStore.ImportProfileCredential("work", before); err != nil {
		t.Fatal(err)
	}
	initial, err := claudeStore.ListAccounts(t.Context())
	if err != nil || len(initial) != 1 {
		t.Fatalf("initial Claude accounts = %d, err = %v", len(initial), err)
	}
	ref := NewAccountRef(codexStore, initial, nil)
	ref.claudeStore = claudeStore
	ref.usageStatusAt = time.Now()

	// Break the registry only after validation/capacity and generation
	// publication. ImportProfileCredential then fails after its credential-file
	// write but before registry publication.
	ref.publishGenerationForTest = func(storeDir string) error {
		if err := advanceAccountDiskGeneration(storeDir); err != nil {
			return err
		}
		if err := os.Remove(claudeStore.ProfilesPath()); err != nil {
			return err
		}
		return os.Mkdir(claudeStore.ProfilesPath(), 0o700)
	}
	after := agentclaude.CredentialInfo{AccessToken: "after-access", RefreshToken: "after-refresh"}
	server := Server{AccountRef: ref}
	_, err = server.installImportedAccount(t.Context(), accountImportRequest{
		Provider: accounts.ProviderClaude,
		Claude:   &claudeAccountImport{Name: "work", Credential: after},
	})
	if err == nil {
		t.Fatal("forced Claude registry failure succeeded")
	}
	wroteCredential := false
	walkErr := filepath.WalkDir(claudeStore.InstancesDir(), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Name() != ".credentials.json" {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(body), after.RefreshToken) {
			wroteCredential = true
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	if !wroteCredential {
		t.Fatal("fixture did not reach the forced mid-mutation credential write")
	}
	if loaded := ref.All(); len(loaded) != 0 {
		t.Fatalf("live state retained stale Claude route after partial mutation: %+v", loaded)
	}
	if !ref.usageStatusAt.IsZero() {
		t.Fatal("Claude partial mutation left the usage-status cache valid")
	}
}

func TestAccountMutationInvalidatesUsageCacheWhenReloadFails(t *testing.T) {
	root := t.TempDir()
	ref := NewAccountRef(accounts.CodexStore{Dir: filepath.Join(root, "accounts")}, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: filepath.Join(root, "claude")}
	ref.usageStatusAt = time.Now()
	notDirectory := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(notDirectory, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := Server{AccountRef: ref}
	_, err := server.installAccountMutation(t.Context(), func() (string, func() error, error) {
		return "test", func() error {
			ref.store.Dir = notDirectory
			return nil
		}, nil
	})
	if err == nil {
		t.Fatal("forced snapshot reload failure succeeded")
	}
	if !ref.usageStatusAt.IsZero() {
		t.Fatal("snapshot reload failure left the usage-status cache valid")
	}
}

func TestInstallAccountMutationAcceptsCommittedClaudeImportOnlyAfterExactReload(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	ref := NewAccountRef(store, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: filepath.Join(root, "claude")}
	server := Server{AccountRef: ref}
	committedErr := fmt.Errorf("%w: injected directory sync failure", agentclaude.ErrProfileRegistryWriteCommitted)

	installed, err := server.installAccountMutation(t.Context(), func() (string, func() error, error) {
		return "work", func() error {
			if err := ref.claudeStore.ImportProfileCredential("work", agentclaude.CredentialInfo{
				AccessToken: "access", RefreshToken: "refresh",
			}); err != nil {
				return err
			}
			return committedErr
		}, nil
	})
	if err != nil || installed != "work" {
		t.Fatalf("committed Claude import = installed %q, err %v", installed, err)
	}
	loaded := ref.All()
	if len(loaded) != 1 || loaded[0].ID != "work" || loaded[0].Token != "access" {
		t.Fatalf("committed Claude import snapshot = %+v", loaded)
	}

	_, err = server.installAccountMutation(t.Context(), func() (string, func() error, error) {
		return "missing", func() error { return committedErr }, nil
	})
	if !errors.Is(err, agentclaude.ErrProfileRegistryWriteCommitted) {
		t.Fatalf("unverified committed mutation error = %v", err)
	}
}

func TestAccountMutationReleasesTransactionBeforeUsageCacheInvalidation(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	ref := NewAccountRef(store, nil, http.DefaultClient)
	ref.claudeStore = agentclaude.Store{Dir: filepath.Join(root, "claude")}
	source := &transactionLockingOAuthSource{
		storeDir: store.StoreDir(),
		entered:  make(chan struct{}),
		account: accounts.Account{
			ID: "kimi-subscription:work", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth,
		},
	}
	ref.oauthSources = []OAuthAccountSource{source}
	server := Server{AccountRef: ref}
	mutationHoldingLock := make(chan struct{})
	allowMutation := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		_, err := server.installAccountMutation(t.Context(), func() (string, func() error, error) {
			return "test", func() error {
				close(mutationHoldingLock)
				<-allowMutation
				return nil
			}, nil
		})
		mutationDone <- err
	}()
	select {
	case <-mutationHoldingLock:
	case <-time.After(2 * time.Second):
		t.Fatal("mutation did not acquire the account transaction lock")
	}
	usageDone := make(chan []AccountUsageStatus, 1)
	go func() { usageDone <- ref.UsageStatuses(t.Context()) }()
	select {
	case <-source.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("usage sweep did not reach transaction-locking refresh")
	}
	close(allowMutation)
	select {
	case err := <-mutationDone:
		if err != nil {
			t.Fatalf("mutation failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mutation deadlocked invalidating usage cache while holding the account transaction lock")
	}
	select {
	case statuses := <-usageDone:
		if len(statuses) != 1 || !statuses[0].AuthValid {
			t.Fatalf("usage statuses = %+v", statuses)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("usage sweep did not finish after transaction release")
	}
}

func serveTenantAccountUpload(server *Server, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/_subrouter/accounts", strings.NewReader(body))
	handleTenantAccountUpload(server, response, request)
	return response
}

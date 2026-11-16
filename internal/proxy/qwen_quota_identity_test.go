package proxy

import (
	"fmt"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestQwenRetryQuotaEvidenceResolvesMembershipFromCurrentCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	account := accounts.Account{ID: "qwen-subscription:member", Provider: accounts.ProviderQwenOAuth, AuthMode: accounts.AuthModeOAuth, AccountID: "membership", Token: "access", CredentialVersion: "grant"}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil)
	scheduler := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server := Server{AccountRef: ref, SchedulerRef: scheduler}
	server.accountListSnapshotContext(t.Context())
	reset := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	body := []byte(fmt.Sprintf(`{"error":{"code":"insufficient_quota","quota":{"reset_at":%d}}}`, reset.Unix()))
	// Active failover reports the addressed credential identity; the authoritative
	// account snapshot owns its stable membership subject.
	server.markAccountExhaustedFromResponseBodyForAccount(accounts.Account{ID: account.ID, Provider: account.Provider, CredentialVersion: account.CredentialIdentity()}, "", 402, nil, body)
	rotated := account
	rotated.Token, rotated.CredentialVersion = "rotated-access", "rotated-grant"
	ref.replace(rotated)
	loaded, generation, revision := ref.CredentialSnapshot()
	scheduler.SyncAccountCredentials(generation, revision, SchedulerAccounts(loaded))
	if until, held := scheduler.ExhaustedUntilFor(account.Provider, account.ID, ""); !held || !until.Equal(reset) {
		t.Fatal("active failover lost the membership quota hold after ordinary OAuth rotation", held, until)
	}
}

package selectacct

import (
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestSchedulerRefPersistsModelIncompatibilityAcrossRestart(t *testing.T) {
	store := ModelIncompatibilityStore{Dir: t.TempDir()}
	scheduler := NewScheduler([]Score{
		{AccountID: "blocked@example.com", Provider: accounts.ProviderCodex, Headroom: 0.8, ShortHeadroom: 0.8},
		{AccountID: "healthy@example.com", Provider: accounts.ProviderCodex, Headroom: 0.8, ShortHeadroom: 0.8},
	})
	ref, err := NewSchedulerRefWithModelStore(scheduler, store)
	if err != nil {
		t.Fatal(err)
	}
	message := "The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."
	if err := ref.MarkModelIncompatible(accounts.ProviderCodex, "blocked@example.com", "gpt-5.6-sol", message); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewSchedulerRefWithModelStore(scheduler, store)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.Get().ForModel("gpt-5.6-sol").Exhausted(accounts.ProviderCodex, "blocked@example.com") {
		t.Fatal("restarted scheduler routed to the incompatible account")
	}
	if restarted.Get().ForModel("gpt-5.6-sol").Exhausted(accounts.ProviderCodex, "healthy@example.com") {
		t.Fatal("model incompatibility excluded the healthy account")
	}
	issues := restarted.ModelIncompatibilities()
	if len(issues) != 1 || issues[0].AccountID != "blocked@example.com" || issues[0].Model != "gpt-5.6-sol" || issues[0].Message != message {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestModelIncompatibilityStoreKeepsOverlappingWorkerWrites(t *testing.T) {
	store := ModelIncompatibilityStore{Dir: t.TempDir()}
	oldWorker, err := NewSchedulerRefWithModelStore(NewScheduler(nil), store)
	if err != nil {
		t.Fatal(err)
	}
	newWorker, err := NewSchedulerRefWithModelStore(NewScheduler(nil), store)
	if err != nil {
		t.Fatal(err)
	}
	if err := oldWorker.MarkModelIncompatible(accounts.ProviderCodex, "old@example.com", "gpt-5.6-sol", "old worker rejection"); err != nil {
		t.Fatal(err)
	}
	if err := newWorker.MarkModelIncompatible(accounts.ProviderCodex, "new@example.com", "gpt-5.6-sol", "new worker rejection"); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewSchedulerRefWithModelStore(NewScheduler(nil), store)
	if err != nil {
		t.Fatal(err)
	}
	if issues := restarted.ModelIncompatibilities(); len(issues) != 2 {
		t.Fatalf("issues = %+v, want both worker writes", issues)
	}
}

func TestRunningWorkerReadsModelIncompatibilityWrittenByPeer(t *testing.T) {
	store := ModelIncompatibilityStore{Dir: t.TempDir()}
	scheduler := NewScheduler([]Score{{
		AccountID: "blocked@example.com", Provider: accounts.ProviderCodex, Headroom: 0.8, ShortHeadroom: 0.8,
	}})
	writer, err := NewSchedulerRefWithModelStore(scheduler, store)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewSchedulerRefWithModelStore(scheduler, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.MarkModelIncompatible(accounts.ProviderCodex, "blocked@example.com", "gpt-5.6-sol", "peer observation"); err != nil {
		t.Fatal(err)
	}
	issues := reader.ModelIncompatibilities()
	if len(issues) != 1 || issues[0].AccountID != "blocked@example.com" || issues[0].Model != "gpt-5.6-sol" {
		t.Fatalf("running worker status issues = %+v, want peer's durable exclusion", issues)
	}
	if !reader.ModelIncompatible(accounts.ProviderCodex, "blocked@example.com", "gpt-5.6-sol") {
		t.Fatal("running worker did not observe peer's durable model exclusion")
	}
}

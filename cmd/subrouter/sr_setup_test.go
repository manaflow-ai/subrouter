package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
)

// fakeController records lifecycle calls without touching launchd or systemd.
type fakeController struct {
	present    bool
	stopped    bool
	removed    bool
	stopErr    error
	restartErr error
	removeEr   error
}

func (c *fakeController) start() error    { return nil }
func (c *fakeController) stop() error     { c.stopped = true; return c.stopErr }
func (c *fakeController) restart() error  { return c.restartErr }
func (c *fakeController) installed() bool { return c.present }
func (c *fakeController) remove() error   { c.removed = true; return c.removeEr }
func (c *fakeController) describe() string {
	return "fake.daemon"
}

func emptyStore(t *testing.T) accounts.CodexStore {
	t.Helper()
	return accounts.CodexStore{Dir: t.TempDir()}
}

func isolateCloudConfig(t *testing.T) {
	t.Helper()
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(t.TempDir(), "cloud.json"))
}

func TestCleanupWithoutYesOnlyPrintsPlan(t *testing.T) {
	controller := &fakeController{present: true}
	var out bytes.Buffer
	if err := runCleanupWith(controller, emptyStore(t), false, false, &out); err != nil {
		t.Fatal(err)
	}
	if controller.removed || controller.stopped {
		t.Fatal("cleanup acted without --yes")
	}
	if !strings.Contains(out.String(), "--yes") {
		t.Fatalf("plan should tell the user how to proceed, got %q", out.String())
	}
}

func TestCleanupWithYesRemovesDaemon(t *testing.T) {
	controller := &fakeController{present: true}
	var out bytes.Buffer
	if err := runCleanupWith(controller, emptyStore(t), true, false, &out); err != nil {
		t.Fatal(err)
	}
	if !controller.stopped || !controller.removed {
		t.Fatalf("expected stop+remove, got stopped=%v removed=%v", controller.stopped, controller.removed)
	}
}

func TestCleanupPreservesCredentialsUnlessPurge(t *testing.T) {
	store := emptyStore(t)
	cloudConfigPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudConfigPath)
	if err := os.WriteFile(
		cloudConfigPath,
		[]byte(`{"version":1,"baseUrl":"https://cmux.com"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(store.StoreDir(), "accounts.json")
	if err := os.MkdirAll(store.StoreDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runCleanupWith(&fakeController{present: true}, store, true, false, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("credentials removed without --purge: %v", err)
	}

	if err := runCleanupWith(&fakeController{present: true}, store, true, true, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("--purge should have deleted the store, stat err = %v", err)
	}
	if _, err := os.Stat(cloudConfigPath); !os.IsNotExist(err) {
		t.Fatalf("--purge should have deleted the cloud session, stat err = %v", err)
	}
}

func TestCleanupWithNothingInstalled(t *testing.T) {
	var out bytes.Buffer
	if err := runCleanupWith(&fakeController{present: false}, emptyStore(t), true, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to clean up") {
		t.Fatalf("got %q", out.String())
	}
}

func TestRestartInstalledDaemonReturnsSupervisorFailure(t *testing.T) {
	want := errors.New("restart failed")
	err := restartInstalledDaemonWith(
		&fakeController{present: true, restartErr: want},
		nil,
	)
	if !errors.Is(err, want) {
		t.Fatalf("restart error = %v, want %v", err, want)
	}
}

func TestDoctorFailsWithoutAccounts(t *testing.T) {
	isolateCloudConfig(t)
	local := healthServer(t, http.StatusOK)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")

	var out bytes.Buffer
	err := runDoctorWith(context.Background(), &fakeController{present: true}, nil, emptyStore(t), &out)
	if err == nil {
		t.Fatal("expected doctor to fail with no accounts stored")
	}
	if !strings.Contains(out.String(), "FAIL") {
		t.Fatalf("expected a FAIL line, got %q", out.String())
	}
}

func TestDoctorReportsUninstalledDaemonAsWarning(t *testing.T) {
	isolateCloudConfig(t)
	local := healthServer(t, http.StatusOK)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")

	var out bytes.Buffer
	_ = runDoctorWith(context.Background(), &fakeController{present: false}, nil, emptyStore(t), &out)
	if !strings.Contains(out.String(), "warn") {
		t.Fatalf("expected a warn line for a missing daemon, got %q", out.String())
	}
}

func TestDoctorSurfacesControllerError(t *testing.T) {
	isolateCloudConfig(t)
	local := healthServer(t, http.StatusOK)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")

	var out bytes.Buffer
	_ = runDoctorWith(context.Background(), nil, errors.New("unsupported platform"), emptyStore(t), &out)
	if !strings.Contains(out.String(), "unsupported platform") {
		t.Fatalf("expected the controller error surfaced, got %q", out.String())
	}
}

func TestDoctorFailsProviderEgressWhenLocalDaemonIsDown(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	if err := broker.SaveConfig(configPath, broker.Config{
		CredentialSource: broker.CredentialSourceLocal,
	}); err != nil {
		t.Fatal(err)
	}
	dead := healthServer(t, http.StatusServiceUnavailable)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", dead.URL+"/v1")

	var out bytes.Buffer
	_ = runDoctorWith(
		context.Background(),
		&fakeController{present: true},
		nil,
		emptyStore(t),
		&out,
	)
	if !strings.Contains(out.String(), "FAIL  provider egress") {
		t.Fatalf(
			"provider egress did not fail with the local daemon:\n%s",
			out.String(),
		)
	}
}

func TestWaitForHealthReturnsFalseWhenDead(t *testing.T) {
	dead := healthServer(t, http.StatusInternalServerError)
	if waitForHealth(context.Background(), dead.URL+"/v1", 300_000_000) {
		t.Fatal("unhealthy server reported ready")
	}
}

// TestUsageTextListsLifecycleCommands guards against help drift: the CLI has two
// help surfaces (usageText and srHelp) and the new verbs were briefly missing
// from the one users actually see.
func TestUsageTextListsLifecycleCommands(t *testing.T) {
	text := usageText("sr")
	for _, want := range []string{
		"sr setup",
		"sr doctor",
		"sr cleanup",
		"sr server up",
		"sr server down",
		"sr server restart",
		"sr server status",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("usage text missing %q", want)
		}
	}
}

func TestSRHelpListsLifecycleCommands(t *testing.T) {
	for _, want := range []string{"sr setup", "sr doctor", "sr cleanup"} {
		if !strings.Contains(srHelp, want) {
			t.Errorf("srHelp missing %q", want)
		}
	}
}

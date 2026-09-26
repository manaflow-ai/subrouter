package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func basePlanInput() setupPlanInput {
	return setupPlanInput{
		GOOS:           "darwin",
		LocalAccounts:  2,
		TeamModeReady:  true,
		WantBackground: true,
		WantConfig:     true,
	}
}

// The common path is one keystroke, and that keystroke must approve the whole
// reviewed set rather than the first of four questions.
func TestReviewAppliesOnEnter(t *testing.T) {
	var out bytes.Buffer
	plan, confirmed := reviewSetupPlan(buildSetupPlan(basePlanInput()), strings.NewReader("\n"), &out)
	if !confirmed {
		t.Fatal("Enter did not confirm the plan")
	}
	if !plan.selected(actionBackground) {
		t.Fatal("background startup was not selected by default")
	}
	screen := out.String()
	for _, want := range []string{
		"Subrouter is ready to set up this computer.",
		"Press Enter to apply.",
		"[d] details   [e] edit options   [Ctrl-C] cancel",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("review screen missing %q", want)
		}
	}
}

// Silence is not consent: EOF without input must cancel, because an unattended
// terminal cannot approve anything.
func TestReviewTreatsEOFAsCancel(t *testing.T) {
	var out bytes.Buffer
	_, confirmed := reviewSetupPlan(buildSetupPlan(basePlanInput()), strings.NewReader(""), &out)
	if confirmed {
		t.Fatal("EOF was treated as approval")
	}
	if !strings.Contains(out.String(), "nothing was changed") {
		t.Errorf("cancel path did not say nothing changed: %q", out.String())
	}
}

func TestReviewShowsDetailsWithoutApplying(t *testing.T) {
	var out bytes.Buffer
	_, confirmed := reviewSetupPlan(buildSetupPlan(basePlanInput()), strings.NewReader("d\n\n"), &out)
	if !confirmed {
		t.Fatal("plan was not applied after viewing details")
	}
	if !strings.Contains(out.String(), "openai_base_url") {
		t.Errorf("details did not show the exact Codex change: %q", out.String())
	}
}

// `e` reaches the uncommon choices without adding a prompt to the normal path.
func TestReviewEditTogglesAnOption(t *testing.T) {
	var out bytes.Buffer
	// e, toggle item 1 (background), Enter to leave the editor, Enter to apply.
	plan, confirmed := reviewSetupPlan(
		buildSetupPlan(basePlanInput()),
		strings.NewReader("e\n1\n\n\n"),
		&out,
	)
	if !confirmed {
		t.Fatal("plan was not applied after editing")
	}
	if plan.selected(actionBackground) {
		t.Fatal("toggling item 1 did not deselect background startup")
	}
	if !plan.selected(actionConfigCodex) {
		t.Fatal("editing one option changed another")
	}
}

func TestReviewRejectsUnknownOptionNumber(t *testing.T) {
	var out bytes.Buffer
	plan, _ := reviewSetupPlan(
		buildSetupPlan(basePlanInput()),
		strings.NewReader("e\n99\n\n\n"),
		&out,
	)
	if !strings.Contains(out.String(), "no such option") {
		t.Errorf("out-of-range choice was accepted silently: %q", out.String())
	}
	if !plan.selected(actionBackground) {
		t.Error("an invalid choice changed the plan")
	}
}

// --no-background and --no-config deselect without needing the review screen.
func TestFlagsDeselectWork(t *testing.T) {
	in := basePlanInput()
	in.WantBackground = false
	in.WantConfig = false
	plan := buildSetupPlan(in)
	for _, action := range []setupAction{actionBackground, actionConfigCodex, actionConfigClaude} {
		if plan.selected(action) {
			t.Errorf("%s stayed selected despite the flag", action)
		}
	}
}

// Already-satisfied work is listed and marked, never re-applied, so re-running
// setup is safe and the screen still shows the whole picture.
func TestPlanMarksExistingStateAsDone(t *testing.T) {
	in := basePlanInput()
	in.DaemonInstalled = true
	in.CodexConfigured = true
	plan := buildSetupPlan(in)
	if plan.selected(actionBackground) || plan.selected(actionConfigCodex) {
		t.Fatal("already-satisfied items would be applied again")
	}
	var out bytes.Buffer
	renderSetupPlan(plan, &out)
	if !strings.Contains(out.String(), "already done") {
		t.Errorf("screen did not mark existing state: %q", out.String())
	}
}

func TestPlanWithNothingToDoSaysSo(t *testing.T) {
	plan := buildSetupPlan(setupPlanInput{GOOS: "linux", DaemonInstalled: true, CodexConfigured: true, ClaudeConfigured: true})
	if plan.pending() {
		t.Fatal("plan reports pending work when everything is done")
	}
	var out bytes.Buffer
	renderSetupPlan(plan, &out)
	if !strings.Contains(out.String(), "Nothing to do") {
		t.Errorf("screen did not say there is nothing to do: %q", out.String())
	}
}

// Handing accounts to the vault stops this machine refreshing them, which is the
// destructive part; the screen must say so rather than calling it "adopt". With
// no vault configured there is nothing to decide, so no line appears at all.
func TestPlanStatesCredentialOwnershipChange(t *testing.T) {
	in := basePlanInput()
	plan := buildSetupPlan(in)
	noVault := basePlanInput()
	noVault.TeamModeReady = false
	if _, ok := buildSetupPlan(noVault).find(actionAdoptLocal); ok {
		t.Error("a credential decision was offered with no vault to hand accounts to")
	}
	item, ok := plan.find(actionAdoptLocal)
	if !ok {
		t.Fatal("no credential item in the plan")
	}
	if !strings.Contains(item.Summary, "team vault") {
		t.Errorf("summary = %q, want it to name the vault", item.Summary)
	}
	if !strings.Contains(item.Detail, "stops refreshing them here") {
		t.Errorf("detail = %q, want it to state the ownership change", item.Detail)
	}
}

// Each platform names its own mechanism, since "in the background" is a
// different object to inspect on each OS.
func TestPlanNamesPlatformMechanism(t *testing.T) {
	for goos, want := range map[string]string{
		"darwin":  "LaunchAgent",
		"linux":   "systemd user service",
		"windows": "scheduled task",
	} {
		in := basePlanInput()
		in.GOOS = goos
		item, ok := buildSetupPlan(in).find(actionBackground)
		if !ok {
			t.Fatalf("%s: no background item", goos)
		}
		if !strings.Contains(item.Summary, want) {
			t.Errorf("%s summary = %q, want %q", goos, item.Summary, want)
		}
	}
}

func TestClientRoutesToLocalReadsDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`openai_base_url = "http://127.0.0.1:31415/v1"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !clientRoutesToLocal(path, "http://127.0.0.1:31415") {
		t.Error("configured client reported as unconfigured")
	}
	if clientRoutesToLocal(path, "http://127.0.0.1:39999") {
		t.Error("a different port matched")
	}
	if clientRoutesToLocal(filepath.Join(dir, "missing.toml"), "http://127.0.0.1:31415") {
		t.Error("a missing file reported as configured")
	}
}

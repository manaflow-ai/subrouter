package main

import (
	"reflect"
	"testing"
)

func TestClaudeDavidDispatch(t *testing.T) {
	if !isDirectSRCommand("claude-david") {
		t.Fatal("shorthand not registered")
	}
	if shouldRouteSRCommand("claude-david") {
		t.Fatal("shorthand must launch locally, not become a remote account operation")
	}
}

func TestClaudeDavidFlagsPassThrough(t *testing.T) {
	args := []string{"--dangerously-skip-permissions", "--resume", "conversation-id", "--model", "fable", "-p", "fix this bug"}
	got, err := parseClaudeAWSArgs(args, "david")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--dangerously-skip-permissions", "--resume", "conversation-id", "-p", "fix this bug"}
	if got.account != "david" || got.model != "fable" || !reflect.DeepEqual(got.passthrough, want) {
		t.Fatalf("launch = %+v", got)
	}
	// Everything after -- belongs to Claude, including account-looking text.
	got, err = parseClaudeAWSArgs([]string{"--", "--account", "friend-b"}, "david")
	if err != nil || got.account != "david" || !reflect.DeepEqual(got.passthrough, []string{"--", "--account", "friend-b"}) {
		t.Fatalf("literal arguments = %+v, %v", got, err)
	}
}

func TestClaudeDavidRejectsAccountOverride(t *testing.T) {
	for _, args := range [][]string{{"--account", "friend-b"}, {"--aws-account", "friend-b"}, {"--account=friend-b"}, {"--aws-account=friend-b"}, {"--account", ""}, {"--account", "--dangerously-skip-permissions"}} {
		if _, err := parseClaudeAWSArgs(args, "david"); err == nil {
			t.Errorf("accepted override %q", args)
		}
	}
	for _, args := range [][]string{{"--account", "david"}, {"--aws-account=david"}} {
		got, err := parseClaudeAWSArgs(args, "david")
		if err != nil || got.account != "david" {
			t.Errorf("same account: %+v %v", got, err)
		}
	}
}

func TestClaudeAWSCanonicalSelector(t *testing.T) {
	got, err := parseClaudeAWSArgs([]string{"--account=david", "--dangerously-skip-permissions"}, "")
	if err != nil || got.account != "david" || !reflect.DeepEqual(got.passthrough, []string{"--dangerously-skip-permissions"}) {
		t.Fatalf("canonical launch: %+v %v", got, err)
	}
}

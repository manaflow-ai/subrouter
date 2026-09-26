package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// A number outside the listed rows is a typo, not an account name. Before,
// "12" with two rows fell through to a substring match and switched to
// whichever account's email contained "12".
func TestSRInteractiveRejectsOutOfRangeNumber(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("active@example.com", "acct_active")
	otherAuth := testCodexAuth("user12@example.com", "acct_user12")
	for email, auth := range map[string]accounts.CodexAuthFile{
		"active@example.com": activeAuth,
		"user12@example.com": otherAuth,
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    auth,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader("12\n"),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return usageResponseWindows(0, 20), nil
		})},
	}

	err := runner.run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("err = %v, want an out-of-range selection error", err)
	}
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil || !ok {
		t.Fatalf("read active auth: ok=%v err=%v", ok, err)
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "active@example.com" {
		t.Fatalf("active email = %q, want active@example.com", email)
	}
}

func TestParsePickerNumber(t *testing.T) {
	for _, tc := range []struct {
		answer    string
		n         int
		wantIndex int
		wantNum   bool
		wantErr   bool
	}{
		{"1", 2, 0, true, false},
		{"2", 2, 1, true, false},
		{"3", 2, 0, true, true},
		{"0", 2, 0, true, true},
		{"-1", 2, 0, true, true},
		{"alice", 2, 0, false, false},
		{"a1", 2, 0, false, false},
	} {
		index, isNumber, err := parsePickerNumber(tc.answer, tc.n)
		if index != tc.wantIndex || isNumber != tc.wantNum || (err != nil) != tc.wantErr {
			t.Fatalf("%q/%d: index=%d number=%v err=%v", tc.answer, tc.n, index, isNumber, err)
		}
	}
}

package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestParityFixtureContract(t *testing.T) {
	var fixture struct {
		Schema   int `json:"schema"`
		Accounts []struct {
			Provider string `json:"provider"`
		} `json:"accounts"`
		Unknown struct {
			Command string `json:"command"`
			ExitCode int `json:"exit_code"`
			Coderouter string `json:"coderouter"`
			Subrouter string `json:"subrouter"`
		} `json:"unknown"`
	}
	data, err := os.ReadFile("../../parity/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Schema != 1 || len(fixture.Accounts) != 2 || fixture.Unknown.ExitCode != 2 {
		t.Fatalf("unexpected parity fixture: %+v", fixture)
	}
	if got := unknownParityCommandError("sr", fixture.Unknown.Command).Error(); got != fixture.Unknown.Subrouter {
		t.Fatalf("subrouter unknown error = %q, want %q", got, fixture.Unknown.Subrouter)
	}
	if fixture.Accounts[0].Provider != "codex" || fixture.Accounts[1].Provider != "claude" {
		t.Fatalf("fixture providers = %+v", fixture.Accounts)
	}
}

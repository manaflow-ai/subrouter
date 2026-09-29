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
			ExitCode int `json:"exit_code"`
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
	if fixture.Accounts[0].Provider != "codex" || fixture.Accounts[1].Provider != "claude" {
		t.Fatalf("fixture providers = %+v", fixture.Accounts)
	}
}

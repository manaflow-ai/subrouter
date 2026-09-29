package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoveryFixture(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "projects", "demo")
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-1111-1111-111111111111"
	if err := os.WriteFile(filepath.Join(p, id+".jsonl"), []byte("{\"cwd\":\"/work\",\"message\":{\"content\":\"API Error: capacity\"}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tr := filepath.Join(root, "tasks", id, "tasks")
	if err := os.MkdirAll(tr, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tr, "task.output"), []byte("result: failure\ncontinue"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_CLAUDE_SESSION_ROOT", filepath.Join(root, "projects"))
	t.Setenv("SUBROUTER_RECOVERY_TASK_ROOTS", filepath.Join(root, "tasks"))
	var out bytes.Buffer
	if err := runRecoveryCommand([]string{"list", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var got []recoverySession
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != id || got[0].Lifecycle != "errored" || len(got[0].Tasks) != 1 {
		t.Fatalf("%+v", got)
	}
	prompt := recoveryPrompt(got[0], "task")
	if !strings.Contains(prompt, "API Error") || !strings.Contains(prompt, "task.output") {
		t.Fatal(prompt)
	}
}

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProviderCodexAliases(t *testing.T) {
	for _, alias := range []string{"oai", "openai", "az", "azure"} {
		if !isDirectSRCommand(alias) {
			t.Errorf("%s is not a direct sr command", alias)
		}
	}
}

func TestOpenAICodexArgs(t *testing.T) {
	for _, args := range [][]string{nil, {"hello"}, {"exec", "hello"}, {"resume", "--last"}} {
		got := providerCodexArgs(args, "http://localhost:31415/v1", "openai")
		joined := strings.Join(got, " ")
		for _, want := range []string{`model_provider="subrouter-openai"`, `"X-Subrouter-OpenAI"="force"`, "supports_websockets=false"} {
			if !strings.Contains(joined, want) {
				t.Errorf("missing %s in %v", want, got)
			}
		}
		if strings.Contains(joined, "X-Subrouter-Azure") {
			t.Fatal("OpenAI launch forces Azure")
		}
	}
}

func TestProviderCodexLaunch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	capture := filepath.Join(t.TempDir(), "args")
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SR_TEST_CAPTURE\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_CODEX_BIN", bin)
	t.Setenv("SR_TEST_CAPTURE", capture)
	for _, supported := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/_subrouter/health" {
				t.Errorf("unexpected path %s", r.URL.Path)
			}
			if supported {
				w.Write([]byte(`{"codex_provider_selection":true}`))
			} else {
				w.Write([]byte(`{}`))
			}
		}))
		t.Setenv("SUBROUTER_CODEX_BASE_URL", server.URL+"/v1")
		for _, provider := range []string{"openai", "azure"} {
			os.Remove(capture)
			err := providerCodex(context.Background(), []string{"exec", "hello"}, provider)
			if !supported {
				if err == nil || !strings.Contains(err.Error(), "upgrade the daemon") {
					t.Fatalf("old server: %v", err)
				}
				if _, err := os.Stat(capture); !os.IsNotExist(err) {
					t.Fatal("launched against old server")
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(got), "exec\n") || !strings.Contains(string(got), `model_provider="subrouter-`+provider+`"`) {
				t.Fatalf("args=%s", got)
			}
		}
		server.Close()
	}
}

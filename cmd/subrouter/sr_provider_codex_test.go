package main

import (
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

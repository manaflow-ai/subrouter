package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAzureCodexBaseURLNormalization(t *testing.T) {
	cases := map[string]string{
		"https://res.openai.azure.com":            "https://res.openai.azure.com/openai/v1",
		"https://res.openai.azure.com/":           "https://res.openai.azure.com/openai/v1",
		"https://res.openai.azure.com/openai":     "https://res.openai.azure.com/openai/v1",
		"https://res.openai.azure.com/openai/v1":  "https://res.openai.azure.com/openai/v1",
		"https://res.openai.azure.com/openai/v1/": "https://res.openai.azure.com/openai/v1",
	}
	for input, want := range cases {
		got, err := azureCodexBaseURL(input)
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		if got.String() != want {
			t.Fatalf("%s => %s, want %s", input, got, want)
		}
	}
	for _, bad := range []string{"", "http://res.openai.azure.com", "https://res.openai.azure.com/v1", "not a url"} {
		if _, err := azureCodexBaseURL(bad); err == nil {
			t.Fatalf("%q was accepted; a wrong base url 404s every fallback", bad)
		}
	}
}

func TestParseAzureCodexDeployments(t *testing.T) {
	got, err := parseAzureCodexDeployments("gpt-5.6-codex=codex-max, gpt-5-codex=codex-old")
	if err != nil {
		t.Fatal(err)
	}
	if got["gpt-5.6-codex"] != "codex-max" || got["gpt-5-codex"] != "codex-old" {
		t.Fatalf("deployments = %v", got)
	}
	if empty, err := parseAzureCodexDeployments("  "); err != nil || empty != nil {
		t.Fatalf("empty mapping = %v, %v", empty, err)
	}
	if _, err := parseAzureCodexDeployments("gpt-5.6-codex"); err == nil {
		t.Fatal("a mapping without = was accepted")
	}
}

func TestAzureCodexConfigFromEnvironment(t *testing.T) {
	t.Setenv("SUBROUTER_AZURE_CODEX_CONFIG_FILE", "")
	t.Setenv("SUBROUTER_AZURE_CODEX_ENDPOINT", "")
	t.Setenv("SUBROUTER_AZURE_CODEX_API_KEY", "")
	t.Setenv("SUBROUTER_AZURE_CODEX_DEPLOYMENTS", "")
	config, err := azureCodexConfigFromEnvironment()
	if err != nil || config != nil {
		t.Fatalf("unset environment => %v, %v; the fallback must stay off", config, err)
	}

	t.Setenv("SUBROUTER_AZURE_CODEX_ENDPOINT", "https://res.openai.azure.com")
	if _, err := azureCodexConfigFromEnvironment(); err == nil {
		t.Fatal("an endpoint without a key was accepted")
	}

	t.Setenv("SUBROUTER_AZURE_CODEX_API_KEY", "azure-key")
	t.Setenv("SUBROUTER_AZURE_CODEX_DEPLOYMENTS", "gpt-5.6-codex=codex-max")
	config, err = azureCodexConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Endpoints) != 1 {
		t.Fatalf("endpoints = %d, want 1", len(config.Endpoints))
	}
	endpoint := config.Endpoints[0]
	if endpoint.BaseURL.String() != "https://res.openai.azure.com/openai/v1" {
		t.Fatalf("base url = %s", endpoint.BaseURL)
	}
	if endpoint.Name != "res.openai.azure.com" {
		t.Fatalf("name = %q, want the host as the default label", endpoint.Name)
	}
	if endpoint.Deployments["gpt-5.6-codex"] != "codex-max" {
		t.Fatalf("deployments = %v", endpoint.Deployments)
	}
}

func TestAzureCodexConfigFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "azure.json")
	contents := `{"endpoints":[
	  {"name":"eastus2","base_url":"https://east.openai.azure.com/openai/v1","api_key":"key-east"},
	  {"name":"swedencentral","base_url":"https://sweden.openai.azure.com","api_key":"key-sweden","deployments":{"gpt-5.6-codex":"codex"}}
	]}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_AZURE_CODEX_CONFIG_FILE", path)
	config, err := azureCodexConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Endpoints) != 2 {
		t.Fatalf("endpoints = %d, want 2", len(config.Endpoints))
	}
	if config.Endpoints[1].BaseURL.String() != "https://sweden.openai.azure.com/openai/v1" {
		t.Fatalf("second base url = %s", config.Endpoints[1].BaseURL)
	}
	if azureCodexEndpointNames(config) != "eastus2,swedencentral" {
		t.Fatalf("names = %q", azureCodexEndpointNames(config))
	}
}

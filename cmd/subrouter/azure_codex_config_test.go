package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/proxy"
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

// A stub endpoint on loopback is how the fallback is exercised without an Azure
// subscription; anything else must still be https.
func TestAzureCodexBaseURLAllowsLoopbackHTTP(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:8080", "http://localhost:8080/openai/v1", "http://[::1]:8080"} {
		if _, err := azureCodexBaseURL(raw); err != nil {
			t.Fatalf("%s rejected: %v", raw, err)
		}
	}
	if _, err := azureCodexBaseURL("http://example.com/openai/v1"); err == nil {
		t.Fatal("plaintext to a remote host was accepted")
	}
}

func TestAzureCodexDefaultDeploymentFromEnvironment(t *testing.T) {
	t.Setenv("SUBROUTER_AZURE_CODEX_CONFIG_FILE", "")
	t.Setenv("SUBROUTER_AZURE_CODEX_ENDPOINT", "https://res.openai.azure.com")
	t.Setenv("SUBROUTER_AZURE_CODEX_API_KEY", "azure-key")
	t.Setenv("SUBROUTER_AZURE_CODEX_DEPLOYMENTS", "")
	t.Setenv("SUBROUTER_AZURE_CODEX_DEFAULT_DEPLOYMENT", "gpt-5.3-codex")
	config, err := azureCodexConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if got := config.Endpoints[0].Deployments[proxy.AzureCodexDefaultDeploymentKey]; got != "gpt-5.3-codex" {
		t.Fatalf("default deployment = %q", got)
	}
}

func TestAzCodexArgsCarryTheForceHeader(t *testing.T) {
	args := azCodexArgs([]string{"exec", "hello"}, "http://127.0.0.1:31415/v1")
	joined := strings.Join(args, " ")
	if args[0] != "exec" {
		t.Fatalf("codex subcommand lost: %v", args)
	}
	if !strings.Contains(joined, `"X-Subrouter-Azure"="force"`) {
		t.Fatalf("force header missing: %s", joined)
	}
	// Azure has no WebSocket Responses surface, so a forced session must not
	// negotiate one.
	if !strings.Contains(joined, "supports_websockets=false") {
		t.Fatalf("websockets not disabled: %s", joined)
	}
	if !strings.Contains(joined, `base_url="http://127.0.0.1:31415/v1"`) {
		t.Fatalf("base url missing: %s", joined)
	}
}

func TestAzResponseSummaryReadsJSONAndSSE(t *testing.T) {
	direct := azResponseSummary([]byte(`{"object":"response","model":"gpt-5.3-codex","usage":{"input_tokens":1738,"output_tokens":5,"input_tokens_details":{"cached_tokens":1536}},"output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}]}`))
	if direct.Model != "gpt-5.3-codex" || direct.CachedTokens != 1536 || direct.Text != "OK" {
		t.Fatalf("json summary = %+v", direct)
	}
	sse := azResponseSummary([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"object\":\"response\",\"model\":\"gpt-5.3-codex\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n"))
	if sse.Model != "gpt-5.3-codex" || sse.InputTokens != 10 {
		t.Fatalf("sse summary = %+v", sse)
	}
}

func TestAzureCodexModelsFromEnvironment(t *testing.T) {
	t.Setenv("SUBROUTER_AZURE_CODEX_CONFIG_FILE", "")
	t.Setenv("SUBROUTER_AZURE_CODEX_ENDPOINT", "https://res.openai.azure.com")
	t.Setenv("SUBROUTER_AZURE_CODEX_API_KEY", "azure-key")
	t.Setenv("SUBROUTER_AZURE_CODEX_API_KEY_FILE", "")
	t.Setenv("SUBROUTER_AZURE_CODEX_DEPLOYMENTS", "")
	t.Setenv("SUBROUTER_AZURE_CODEX_DEFAULT_DEPLOYMENT", "")
	t.Setenv("SUBROUTER_AZURE_CODEX_MODELS", "gpt-5.6*, gpt-5.3-codex ,")
	config, err := azureCodexConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Models) != 2 || config.Models[0] != "gpt-5.6*" || config.Models[1] != "gpt-5.3-codex" {
		t.Fatalf("models = %v, want the trimmed allow list", config.Models)
	}
}

func TestAzureCodexOpenAIEndpointAndKeyFile(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "openai-key")
	if err := os.WriteFile(keyPath, []byte("sk-test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "azure-codex.json")
	contents := fmt.Sprintf(`{
		"models": ["gpt-5.6*"],
		"endpoints": [
			{"name": "azure", "base_url": "https://res.openai.azure.com", "api_key": "azure-key"},
			{"name": "openai", "base_url": "https://api.openai.com/v1", "api_key_file": %q}
		]
	}`, keyPath)
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_AZURE_CODEX_CONFIG_FILE", configPath)
	config, err := azureCodexConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Endpoints) != 2 {
		t.Fatalf("endpoints = %d, want 2", len(config.Endpoints))
	}
	if got := config.Endpoints[1].BaseURL.String(); got != "https://api.openai.com/v1" {
		t.Fatalf("openai base url = %q", got)
	}
	if config.Endpoints[1].APIKey != "sk-test-key" {
		t.Fatalf("openai api key = %q, want the trimmed file contents", config.Endpoints[1].APIKey)
	}
	if len(config.Models) != 1 || config.Models[0] != "gpt-5.6*" {
		t.Fatalf("models = %v", config.Models)
	}
}

func TestAzureCodexBaseURLRejectsBareV1OnAzureHosts(t *testing.T) {
	if _, err := azureCodexBaseURL("https://res.openai.azure.com/v1"); err == nil {
		t.Fatal("/v1 on an Azure host must stay rejected; it 404s every fallback")
	}
	got, err := azureCodexBaseURL("https://api.openai.com/v1")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "https://api.openai.com/v1" {
		t.Fatalf("openai base = %q", got)
	}
}

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// azureCodexFile is the on-disk form of the Codex Azure fallback, for the case
// of several Azure resources (one per region, or one per quota pool). The
// single-resource case needs no file, only the three environment variables read
// by azureCodexConfigFromEnvironment.
type azureCodexFile struct {
	Endpoints []azureCodexFileEndpoint `json:"endpoints"`
}

type azureCodexFileEndpoint struct {
	Name              string            `json:"name"`
	BaseURL           string            `json:"base_url"`
	APIKey            string            `json:"api_key"`
	Deployments       map[string]string `json:"deployments"`
	DefaultDeployment string            `json:"default_deployment"`
}

// azureCodexConfigFromEnvironment builds the Codex Azure fallback from the
// environment. It returns nil when nothing is configured, which leaves the
// Codex pool exactly as it was.
func azureCodexConfigFromEnvironment() (*proxy.AzureCodexConfig, error) {
	if path := strings.TrimSpace(os.Getenv("SUBROUTER_AZURE_CODEX_CONFIG_FILE")); path != "" {
		return azureCodexConfigFromFile(path)
	}
	endpoint := strings.TrimSpace(os.Getenv("SUBROUTER_AZURE_CODEX_ENDPOINT"))
	apiKey, err := secretFromEnvironment("SUBROUTER_AZURE_CODEX_API_KEY", "SUBROUTER_AZURE_CODEX_API_KEY_FILE")
	if err != nil {
		return nil, fmt.Errorf("azure codex: %w", err)
	}
	if endpoint == "" && apiKey == "" {
		return nil, nil
	}
	if endpoint == "" || apiKey == "" {
		return nil, errors.New("azure codex: set both SUBROUTER_AZURE_CODEX_ENDPOINT and SUBROUTER_AZURE_CODEX_API_KEY")
	}
	deployments, err := parseAzureCodexDeployments(os.Getenv("SUBROUTER_AZURE_CODEX_DEPLOYMENTS"))
	if err != nil {
		return nil, err
	}
	return azureCodexConfig([]azureCodexFileEndpoint{{
		BaseURL:           endpoint,
		APIKey:            apiKey,
		Deployments:       deployments,
		DefaultDeployment: strings.TrimSpace(os.Getenv("SUBROUTER_AZURE_CODEX_DEFAULT_DEPLOYMENT")),
	}})
}

func azureCodexConfigFromFile(path string) (*proxy.AzureCodexConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("azure codex: read config: %w", err)
	}
	var parsed azureCodexFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("azure codex: parse config: %w", err)
	}
	return azureCodexConfig(parsed.Endpoints)
}

func azureCodexConfig(endpoints []azureCodexFileEndpoint) (*proxy.AzureCodexConfig, error) {
	if len(endpoints) == 0 {
		return nil, errors.New("azure codex: no endpoints configured")
	}
	config := &proxy.AzureCodexConfig{}
	for index, endpoint := range endpoints {
		base, err := azureCodexBaseURL(endpoint.BaseURL)
		if err != nil {
			return nil, err
		}
		apiKey := strings.TrimSpace(endpoint.APIKey)
		if apiKey == "" {
			return nil, fmt.Errorf("azure codex: endpoint %d has no api key", index)
		}
		name := strings.TrimSpace(endpoint.Name)
		if name == "" {
			name = base.Host
		}
		deployments := endpoint.Deployments
		// A default deployment catches models Azure has not shipped yet, which
		// is most of them: Azure trails the ChatGPT model list, and the request
		// that needs the fallback is exactly the one the pool refused.
		if fallback := strings.TrimSpace(endpoint.DefaultDeployment); fallback != "" {
			if deployments == nil {
				deployments = map[string]string{}
			}
			deployments[proxy.AzureCodexDefaultDeploymentKey] = fallback
		}
		config.Endpoints = append(config.Endpoints, proxy.AzureCodexEndpoint{
			Name:        name,
			BaseURL:     base,
			APIKey:      apiKey,
			Deployments: deployments,
		})
	}
	return config, nil
}

// azureCodexBaseURL normalizes an Azure resource URL onto the v1 surface Codex
// speaks. Both the bare resource URL and the full ".../openai/v1" form are
// accepted, because the Azure portal shows the first and the Codex docs show
// the second, and a fallback that silently 404s is worse than one that refuses
// to start.
func azureCodexBaseURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("azure codex: endpoint has no base url")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("azure codex: parse endpoint %q: %w", trimmed, err)
	}
	// Azure itself is always https. http is allowed only against loopback, so a
	// stub endpoint can be pointed at during local testing without shipping a
	// key over plaintext to anything else.
	if parsed.Host == "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname()))) {
		return nil, fmt.Errorf("azure codex: endpoint %q must be an https url", trimmed)
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case path == "":
		path = "/openai/v1"
	case strings.HasSuffix(path, "/openai/v1"):
	case strings.HasSuffix(path, "/openai"):
		path += "/v1"
	default:
		return nil, fmt.Errorf("azure codex: endpoint %q must end in /openai/v1", trimmed)
	}
	parsed.Path = path
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed, nil
}

// parseAzureCodexDeployments reads "model=deployment,model2=deployment2". An
// unmapped model uses its own name, which is how Foundry names a deployment by
// default, so this is only needed when a deployment was named something else.
func parseAzureCodexDeployments(raw string) (map[string]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	deployments := map[string]string{}
	for _, pair := range strings.Split(trimmed, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		model, deployment, ok := strings.Cut(pair, "=")
		model = strings.TrimSpace(model)
		deployment = strings.TrimSpace(deployment)
		if !ok || model == "" || deployment == "" {
			return nil, fmt.Errorf("azure codex: deployment mapping %q must be model=deployment", pair)
		}
		deployments[model] = deployment
	}
	return deployments, nil
}

// azureCodexEndpointNames lists the configured endpoints for one startup log
// line. Keys never appear.
func azureCodexEndpointNames(config *proxy.AzureCodexConfig) string {
	if config == nil {
		return ""
	}
	names := make([]string, 0, len(config.Endpoints))
	for _, endpoint := range config.Endpoints {
		names = append(names, endpoint.Name)
	}
	return strings.Join(names, ",")
}

// isLoopbackHost reports whether a host name resolves to this machine without a
// lookup.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

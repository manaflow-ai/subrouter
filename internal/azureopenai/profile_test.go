package azureopenai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeProfileBuildsAzureResponsesBaseURL(t *testing.T) {
	tests := []struct {
		name         string
		endpoint     string
		wantEndpoint string
		wantResource string
	}{
		{
			name:         "Azure OpenAI resource",
			endpoint:     "https://example.openai.azure.com/",
			wantEndpoint: "https://example.openai.azure.com/openai/v1",
			wantResource: FoundryTokenResource,
		},
		{
			name:         "Foundry resource",
			endpoint:     "https://example.services.ai.azure.com/openai/v1/",
			wantEndpoint: "https://example.services.ai.azure.com/openai/v1",
			wantResource: FoundryTokenResource,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile, err := NormalizeProfile(Profile{
				Name:       "WORK",
				Endpoint:   test.endpoint,
				Deployment: "gpt-5.3-codex",
				AzureCLI:   "/opt/homebrew/bin/az",
			})
			if err != nil {
				t.Fatal(err)
			}
			if profile.Name != "work" {
				t.Fatalf("name = %q, want work", profile.Name)
			}
			if profile.Endpoint != test.wantEndpoint {
				t.Fatalf("endpoint = %q, want %q", profile.Endpoint, test.wantEndpoint)
			}
			if profile.TokenResource != test.wantResource {
				t.Fatalf("token resource = %q, want %q", profile.TokenResource, test.wantResource)
			}
		})
	}
}

func TestNormalizeProfileKeepsExplicitCognitiveServicesAudience(t *testing.T) {
	profile, err := NormalizeProfile(Profile{
		Name:          "work",
		Endpoint:      "https://example.openai.azure.com",
		Deployment:    "codex-deployment",
		TokenResource: CognitiveServicesTokenResource,
		AzureCLI:      "/opt/homebrew/bin/az",
	})
	if err != nil {
		t.Fatal(err)
	}
	if profile.TokenResource != CognitiveServicesTokenResource {
		t.Fatalf("token resource = %q", profile.TokenResource)
	}
}

func TestNormalizeProfileCanonicalizesGPT56DeploymentMappings(t *testing.T) {
	profile, err := NormalizeProfile(Profile{
		Name:     "work",
		Endpoint: "https://example.openai.azure.com",
		Deployments: map[string]string{
			"sol":           "team-sol",
			"gpt-5.6-terra": "team-terra",
			"LUNA":          "team-luna",
		},
		AzureCLI: "/opt/homebrew/bin/az",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		GPT56Sol:   "team-sol",
		GPT56Terra: "team-terra",
		GPT56Luna:  "team-luna",
	}
	for model, deployment := range want {
		if profile.Deployments[model] != deployment {
			t.Fatalf("deployment %s = %q, want %q", model, profile.Deployments[model], deployment)
		}
	}
	for selector, deployment := range map[string]string{
		"":             "team-sol",
		"gpt-5.6":      "team-sol",
		"sol":          "team-sol",
		"terra":        "team-terra",
		"gpt-5.6-luna": "team-luna",
		"team-luna":    "team-luna",
	} {
		got, ok := profile.DeploymentForModel(selector)
		if !ok || got != deployment {
			t.Errorf("DeploymentForModel(%q) = %q, %t; want %q, true", selector, got, ok, deployment)
		}
	}
	model, deployment, ok := profile.ResolveModel("team-luna")
	if !ok || model != GPT56Luna || deployment != "team-luna" {
		t.Fatalf("ResolveModel(team-luna) = %q, %q, %t", model, deployment, ok)
	}
	if _, ok := profile.DeploymentForModel("gpt-5.5"); ok {
		t.Fatal("unsupported model selector was accepted")
	}
}

func TestNormalizeProfileRejectsOneDeploymentForMultipleModels(t *testing.T) {
	_, err := NormalizeProfile(Profile{
		Name:     "work",
		Endpoint: "https://example.openai.azure.com",
		Deployments: map[string]string{
			GPT56Sol:   "shared",
			GPT56Terra: "shared",
		},
		AzureCLI: "/opt/homebrew/bin/az",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot map both") {
		t.Fatalf("error = %v", err)
	}
}

func TestNormalizeProfileRejectsInconsistentCompatibilityDeployment(t *testing.T) {
	_, err := NormalizeProfile(Profile{
		Name:       "work",
		Endpoint:   "https://example.openai.azure.com",
		Deployment: "legacy",
		Deployments: map[string]string{
			GPT56Sol: "team-sol",
		},
		AzureCLI: "/opt/homebrew/bin/az",
	})
	if err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("error = %v", err)
	}
}

func TestStorePersistsOnlyProfileMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "azure-openai.json")
	store := Store{Path: path}
	profile := Profile{
		Name:       "work",
		Endpoint:   "https://example.openai.azure.com",
		Deployment: "codex-deployment",
		AzureCLI:   "/opt/homebrew/bin/az",
	}

	existed, err := store.Save(profile)
	if err != nil {
		t.Fatal(err)
	}
	if existed {
		t.Fatal("new profile reported as an update")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("profile mode = %o, want 600", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"accessToken", "refreshToken", "apiKey", "Bearer "} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("profile store contains credential field %q: %s", forbidden, body)
		}
	}

	stored, ok, err := store.Find("WORK")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || stored.Endpoint != "https://example.openai.azure.com/openai/v1" {
		t.Fatalf("stored profile = %#v, found = %t", stored, ok)
	}
	stored.Deployment = "replacement"
	existed, err = store.Save(stored)
	if err != nil {
		t.Fatal(err)
	}
	if !existed {
		t.Fatal("existing profile reported as new")
	}
	removed, err := store.Remove("work")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("existing profile was not removed")
	}
	profiles, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 0 {
		t.Fatalf("profiles after remove = %#v", profiles)
	}
}

func TestStoreDefaultsToFirstProfileAndAllowsChangingDefault(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "azure-openai.json")}
	for _, name := range []string{"work", "backup"} {
		if _, err := store.Save(Profile{
			Name:        name,
			Endpoint:    "https://" + name + ".openai.azure.com",
			Deployments: DefaultGPT56Deployments(),
			AzureCLI:    "/opt/homebrew/bin/az",
		}); err != nil {
			t.Fatal(err)
		}
	}

	profile, ok, err := store.Default()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || profile.Name != "work" {
		t.Fatalf("default profile = %#v, found = %t; want work", profile, ok)
	}
	if err := store.SetDefault("BACKUP"); err != nil {
		t.Fatal(err)
	}
	profile, ok, err = store.Default()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || profile.Name != "backup" {
		t.Fatalf("changed default profile = %#v, found = %t; want backup", profile, ok)
	}
	if removed, err := store.Remove("backup"); err != nil || !removed {
		t.Fatalf("remove default = %t, %v", removed, err)
	}
	profile, ok, err = store.Default()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || profile.Name != "work" {
		t.Fatalf("fallback default profile = %#v, found = %t; want work", profile, ok)
	}
}

func TestStoreReadsVersionTwoProfilesWithDeterministicDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "azure-openai.json")
	body := `{
  "version": 2,
  "profiles": [
    {"name":"zeta","endpoint":"https://zeta.openai.azure.com/openai/v1","deployment":"zeta","tokenResource":"https://ai.azure.com","azureCli":"az"},
    {"name":"alpha","endpoint":"https://alpha.openai.azure.com/openai/v1","deployment":"alpha","tokenResource":"https://ai.azure.com","azureCli":"az"}
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	profile, ok, err := (Store{Path: path}).Default()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || profile.Name != "alpha" {
		t.Fatalf("version 2 default profile = %#v, found = %t; want alpha", profile, ok)
	}
}

func TestNormalizeProfileRejectsUnsafeLocations(t *testing.T) {
	for _, endpoint := range []string{
		"http://example.openai.azure.com",
		"https://attacker.example.com",
		"https://user@example.openai.azure.com",
		"https://example.openai.azure.com/openai/deployments/one",
	} {
		_, err := NormalizeProfile(Profile{
			Name:       "work",
			Endpoint:   endpoint,
			Deployment: "codex-deployment",
			AzureCLI:   "az",
		})
		if err == nil {
			t.Fatalf("endpoint %q was accepted", endpoint)
		}
	}
}

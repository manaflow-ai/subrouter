package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/azureopenai"
)

type azureTestCommandRunner struct {
	output      []byte
	outputCalls [][]string
	runName     string
	runArgs     []string
	runEnv      []string
}

func (r *azureTestCommandRunner) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	return r.RunWithEnv(ctx, name, args, nil, stdin, stdout, stderr)
}

func (r *azureTestCommandRunner) RunWithEnv(_ context.Context, name string, args []string, env []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	r.runName = name
	r.runArgs = append([]string(nil), args...)
	r.runEnv = append([]string(nil), env...)
	return nil
}

func (r *azureTestCommandRunner) Output(_ context.Context, name string, args []string) ([]byte, error) {
	r.outputCalls = append(r.outputCalls, append([]string{name}, args...))
	return append([]byte(nil), r.output...), nil
}

func TestSRAzureAddValidatesCLIThenPersistsMetadata(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "az")
	if err := os.WriteFile(cliPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := azureopenai.Store{Path: filepath.Join(dir, "azure-openai.json")}
	commands := &azureTestCommandRunner{output: []byte(`{"accessToken":"azure-cli-secret","expires_on":4070908800}`)}
	var restarts int
	var out bytes.Buffer
	runner := srRunner{
		program:    "sr",
		in:         strings.NewReader(""),
		out:        &out,
		errOut:     &out,
		cmd:        commands,
		azureStore: store,
		restartDaemon: func() error {
			restarts++
			return nil
		},
	}

	err := runner.run(context.Background(), []string{
		"add", "azure", "work",
		"--endpoint", "https://example.openai.azure.com",
		"--deployment", "codex-deployment",
		"--azure-cli", cliPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCommand := []string{
		cliPath,
		"account", "get-access-token",
		"--resource", azureopenai.FoundryTokenResource,
		"--output", "json",
	}
	if len(commands.outputCalls) != 1 || !reflect.DeepEqual(commands.outputCalls[0], wantCommand) {
		t.Fatalf("Azure CLI calls = %#v, want %#v", commands.outputCalls, wantCommand)
	}
	if restarts != 1 {
		t.Fatalf("daemon restarts = %d, want 1", restarts)
	}
	profile, ok, err := store.Find("work")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || profile.Deployment != "codex-deployment" || profile.AzureCLI != cliPath {
		t.Fatalf("profile = %#v, found = %t", profile, ok)
	}
	body, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "azure-cli-secret") {
		t.Fatalf("profile store contains access token: %s", body)
	}
	if !strings.Contains(out.String(), "Access tokens are renewed through Azure CLI and are not stored") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSRAzAddDefaultsToAllGPT56Deployments(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "az")
	if err := os.WriteFile(cliPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := azureopenai.Store{Path: filepath.Join(dir, "azure-openai.json")}
	commands := &azureTestCommandRunner{output: []byte(`{"accessToken":"azure-cli-secret","expires_on":4070908800}`)}
	runner := srRunner{
		program:    "sr",
		in:         strings.NewReader(""),
		out:        io.Discard,
		errOut:     io.Discard,
		cmd:        commands,
		azureStore: store,
		restartDaemon: func() error {
			return nil
		},
	}

	if err := runner.run(context.Background(), []string{
		"az", "add", "work",
		"--endpoint", "https://example.openai.azure.com",
		"--azure-cli", cliPath,
	}); err != nil {
		t.Fatal(err)
	}
	profile, ok, err := store.Find("work")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Azure profile was not saved")
	}
	if profile.Deployment != azureopenai.GPT56Sol {
		t.Fatalf("compatibility deployment = %q, want %q", profile.Deployment, azureopenai.GPT56Sol)
	}
	for _, model := range azureopenai.GPT56Models() {
		if profile.Deployments[model] != model {
			t.Errorf("deployment %s = %q, want canonical model name", model, profile.Deployments[model])
		}
	}
}

func TestParseAzureDeploymentFlagsSupportsAliasesAndRejectsConflicts(t *testing.T) {
	deployments, legacy, err := parseAzureDeploymentFlags([]string{
		"sol=production-sol",
		"gpt-5.6-terra=production-terra",
		"luna=production-luna",
	})
	if err != nil {
		t.Fatal(err)
	}
	if legacy != "" {
		t.Fatalf("legacy deployment = %q", legacy)
	}
	for model, want := range map[string]string{
		azureopenai.GPT56Sol:   "production-sol",
		azureopenai.GPT56Terra: "production-terra",
		azureopenai.GPT56Luna:  "production-luna",
	} {
		if deployments[model] != want {
			t.Errorf("deployment %s = %q, want %q", model, deployments[model], want)
		}
	}
	if _, _, err := parseAzureDeploymentFlags([]string{"sol=one", "gpt-5.6-sol=two"}); err == nil {
		t.Fatal("conflicting model aliases were accepted")
	}
}

func TestSRAzureCodexBindsCustomProviderToDeployment(t *testing.T) {
	local := healthServer(t, 200)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(t.TempDir(), "cloud.json"))
	t.Setenv("SUBROUTER_CODEX_BIN", "codex-test")

	store := azureopenai.Store{Path: filepath.Join(t.TempDir(), "azure-openai.json")}
	if _, err := store.Save(azureopenai.Profile{
		Name:       "work",
		Endpoint:   "https://example.openai.azure.com",
		Deployment: "codex-deployment",
		AzureCLI:   "/opt/homebrew/bin/az",
	}); err != nil {
		t.Fatal(err)
	}
	commands := &azureTestCommandRunner{output: []byte(`{"accessToken":"azure-cli-secret","expires_on":4070908800}`)}
	runner := srRunner{
		program:    "sr",
		in:         strings.NewReader(""),
		out:        io.Discard,
		errOut:     io.Discard,
		client:     local.Client(),
		cmd:        commands,
		azureStore: store,
	}

	if err := runner.run(context.Background(), []string{"azure", "codex", "work", "exec", "hello"}); err != nil {
		t.Fatal(err)
	}
	if commands.runName != "codex-test" {
		t.Fatalf("command = %q, want codex-test", commands.runName)
	}
	joined := strings.Join(commands.runArgs, "\n")
	for _, want := range []string{
		"exec",
		`model="codex-deployment"`,
		`model_provider="subrouter_azure"`,
		`model_providers.subrouter_azure.base_url="` + local.URL + `/azure/work/v1"`,
		`model_providers.subrouter_azure.experimental_bearer_token="subrouter"`,
		`model_providers.subrouter_azure.wire_api="responses"`,
		`model_providers.subrouter_azure.supports_websockets=false`,
		"hello",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Codex args missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "azure-cli-secret") || strings.Contains(strings.Join(commands.runEnv, "\n"), "azure-cli-secret") {
		t.Fatal("Azure access token was passed to the Codex child")
	}
}

func TestSRAzureCodexUsesSavedDefaultWithoutProfileArgument(t *testing.T) {
	local := healthServer(t, 200)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(t.TempDir(), "cloud.json"))
	t.Setenv("SUBROUTER_CODEX_BIN", "codex-test")

	store := azureopenai.Store{Path: filepath.Join(t.TempDir(), "azure-openai.json")}
	for _, profile := range []azureopenai.Profile{
		{
			Name:        "primary",
			Endpoint:    "https://primary.openai.azure.com",
			Deployments: azureopenai.DefaultGPT56Deployments(),
			AzureCLI:    "/opt/homebrew/bin/az",
		},
		{
			Name:        "secondary",
			Endpoint:    "https://secondary.openai.azure.com",
			Deployments: azureopenai.DefaultGPT56Deployments(),
			AzureCLI:    "/opt/homebrew/bin/az",
		},
	} {
		if _, err := store.Save(profile); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetDefault("secondary"); err != nil {
		t.Fatal(err)
	}

	commands := &azureTestCommandRunner{output: []byte(`{"accessToken":"azure-cli-secret","expires_on":4070908800}`)}
	runner := srRunner{
		program:    "sr",
		in:         strings.NewReader(""),
		out:        io.Discard,
		errOut:     io.Discard,
		client:     local.Client(),
		cmd:        commands,
		azureStore: store,
	}

	if err := runner.run(context.Background(), []string{"az", "codex"}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(commands.runArgs, "\n")
	for _, want := range []string{
		`model="gpt-5.6-sol"`,
		`model_providers.subrouter_azure.base_url="` + local.URL + `/azure/secondary/v1"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Codex args missing %q:\n%s", want, joined)
		}
	}
	for _, arg := range commands.runArgs {
		if arg == "--model" || arg == "-m" || strings.HasPrefix(arg, "--model=") || strings.HasPrefix(arg, "-m=") {
			t.Fatalf("launcher pinned Codex's model flag instead of leaving /model available: %#v", commands.runArgs)
		}
	}
}

func TestSRAzureCodexProfileOverrideDoesNotReachCodex(t *testing.T) {
	store := azureopenai.Store{Path: filepath.Join(t.TempDir(), "azure-openai.json")}
	for _, name := range []string{"primary", "secondary"} {
		if _, err := store.Save(azureopenai.Profile{
			Name:        name,
			Endpoint:    "https://" + name + ".openai.azure.com",
			Deployments: azureopenai.DefaultGPT56Deployments(),
			AzureCLI:    "/opt/homebrew/bin/az",
		}); err != nil {
			t.Fatal(err)
		}
	}
	runner := srRunner{azureStore: store}
	profile, args, err := runner.azureCodexProfile([]string{"exec", "--azure-profile=secondary", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if profile.Name != "secondary" {
		t.Fatalf("profile = %q, want secondary", profile.Name)
	}
	if !reflect.DeepEqual(args, []string{"exec", "hello"}) {
		t.Fatalf("Codex args = %#v, want exec hello", args)
	}
}

func TestSRAzureCodexRejectsDeploymentOverride(t *testing.T) {
	store := azureopenai.Store{Path: filepath.Join(t.TempDir(), "azure-openai.json")}
	if _, err := store.Save(azureopenai.Profile{
		Name:       "work",
		Endpoint:   "https://example.openai.azure.com",
		Deployment: "codex-deployment",
		AzureCLI:   "/opt/homebrew/bin/az",
	}); err != nil {
		t.Fatal(err)
	}
	runner := srRunner{
		program:    "sr",
		in:         strings.NewReader(""),
		out:        io.Discard,
		errOut:     io.Discard,
		cmd:        &azureTestCommandRunner{},
		azureStore: store,
	}
	err := runner.run(context.Background(), []string{"azure", "codex", "work", "-m", "other-deployment"})
	if err == nil || !strings.Contains(err.Error(), `bound to deployment "codex-deployment"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestSRAzureCodexResolvesGPT56FamilyAndRewritesModelFlag(t *testing.T) {
	local := healthServer(t, 200)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(t.TempDir(), "cloud.json"))
	t.Setenv("SUBROUTER_CODEX_BIN", "codex-test")

	store := azureopenai.Store{Path: filepath.Join(t.TempDir(), "azure-openai.json")}
	if _, err := store.Save(azureopenai.Profile{
		Name:     "work",
		Endpoint: "https://example.openai.azure.com",
		Deployments: map[string]string{
			azureopenai.GPT56Sol:   "deployment-sol",
			azureopenai.GPT56Terra: "deployment-terra",
			azureopenai.GPT56Luna:  "deployment-luna",
		},
		AzureCLI: "/opt/homebrew/bin/az",
	}); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name       string
		modelArgs  []string
		deployment string
	}{
		{name: "default sol", deployment: "deployment-sol"},
		{name: "OpenAI alias", modelArgs: []string{"--model", "gpt-5.6"}, deployment: "deployment-sol"},
		{name: "terra short alias", modelArgs: []string{"--model=terra"}, deployment: "deployment-terra"},
		{name: "luna full name", modelArgs: []string{"-m", "gpt-5.6-luna"}, deployment: "deployment-luna"},
	} {
		t.Run(test.name, func(t *testing.T) {
			commands := &azureTestCommandRunner{output: []byte(`{"accessToken":"azure-cli-secret","expires_on":4070908800}`)}
			runner := srRunner{
				program:    "sr",
				in:         strings.NewReader(""),
				out:        io.Discard,
				errOut:     io.Discard,
				client:     local.Client(),
				cmd:        commands,
				azureStore: store,
			}
			args := []string{"az", "codex", "work", "exec"}
			args = append(args, test.modelArgs...)
			args = append(args, "hello")
			if err := runner.run(context.Background(), args); err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(commands.runArgs, "\n")
			canonicalModel, ok := azureopenai.CanonicalGPT56Model("")
			if len(test.modelArgs) > 0 {
				canonicalModel, ok = azureopenai.CanonicalGPT56Model(codexModelArg(test.modelArgs))
			}
			if !ok {
				t.Fatalf("test model selector is not canonical: %#v", test.modelArgs)
			}
			if !strings.Contains(joined, `model="`+canonicalModel+`"`) {
				t.Fatalf("Codex args do not retain canonical picker model %q:\n%s", canonicalModel, joined)
			}
			for _, arg := range commands.runArgs {
				if arg == test.deployment || arg == "gpt-5.6" || arg == "terra" || arg == "--model=terra" {
					t.Fatalf("unresolved model selector reached Codex: %#v", commands.runArgs)
				}
			}
		})
	}
}

func TestSRAzureCodexRejectsUnknownGPT56Mapping(t *testing.T) {
	store := azureopenai.Store{Path: filepath.Join(t.TempDir(), "azure-openai.json")}
	if _, err := store.Save(azureopenai.Profile{
		Name:        "work",
		Endpoint:    "https://example.openai.azure.com",
		Deployments: azureopenai.DefaultGPT56Deployments(),
		AzureCLI:    "/opt/homebrew/bin/az",
	}); err != nil {
		t.Fatal(err)
	}
	runner := srRunner{
		program:    "sr",
		in:         strings.NewReader(""),
		out:        io.Discard,
		errOut:     io.Discard,
		cmd:        &azureTestCommandRunner{},
		azureStore: store,
	}
	err := runner.run(context.Background(), []string{"azure", "codex", "work", "--model", "gpt-5.5"})
	if err == nil || !strings.Contains(err.Error(), "use sol, terra, or luna") {
		t.Fatalf("error = %v", err)
	}
}

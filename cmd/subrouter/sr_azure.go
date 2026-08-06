package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/azureopenai"
)

const srAzureHelp = `sr azure - Run Codex through Azure OpenAI with Azure CLI authentication

Usage:
  sr azure add <profile> --endpoint <url> [--deployment <model=name> ...] [--default]
  sr azure list
  sr azure default [profile]
  sr azure remove <profile>
  sr azure codex [--azure-profile <profile>] [--model sol|terra|luna] [codex args...]

The short alias 'sr az' supports the same commands. With no --deployment flags,
the profile maps Sol, Terra, and Luna to their canonical GPT-5.6 deployment names.
Repeat --deployment to override a mapping, for example --deployment sol=my-sol.
The first profile becomes the default. Use 'sr az default <profile>' to change it.
Inside Codex, use /model to switch among the configured GPT-5.6 models.
The profile stores endpoint and deployment metadata, never an access token.
Run 'az login' first. Subrouter renews Entra access tokens through the same Azure CLI session.
`

func azureProviderAlias(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "az", "azure", "azure-openai", "foundry":
		return true
	default:
		return false
	}
}

func (r srRunner) azure(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return r.azureList()
	}
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "add", "login":
		return r.azureAdd(ctx, args[1:])
	case "list", "ls", "status":
		return r.azureList()
	case "default", "use":
		return r.azureDefault(args[1:])
	case "remove", "rm":
		if len(args) != 2 {
			return errors.New("usage: sr azure remove <profile>")
		}
		return r.azureRemove(args[1])
	case "codex", "run":
		return r.azureCodex(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(r.out, srAzureHelp)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n%s", "sr azure "+args[0], srAzureHelp)
	}
}

func (r srRunner) azureProfileStore() azureopenai.Store {
	if strings.TrimSpace(r.azureStore.Path) != "" {
		return r.azureStore
	}
	return azureopenai.DefaultStore()
}

func (r srRunner) restartAzureDaemon() error {
	if r.restartDaemon != nil {
		return r.restartDaemon()
	}
	return restartInstalledDaemon()
}

func (r srRunner) azureAdd(ctx context.Context, args []string) error {
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name = args[0]
		args = args[1:]
	}
	flags := flag.NewFlagSet("azure add", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	nameFlag := flags.String("name", name, "profile name")
	endpoint := flags.String("endpoint", strings.TrimSpace(os.Getenv("AZURE_OPENAI_ENDPOINT")), "Azure OpenAI endpoint")
	var deploymentFlags repeatedStringFlag
	flags.Var(&deploymentFlags, "deployment", "Azure deployment mapping model=name; repeat for multiple models")
	tokenResource := flags.String("token-resource", strings.TrimSpace(os.Getenv("AZURE_OPENAI_TOKEN_RESOURCE")), "Azure token resource audience")
	azureCLI := flags.String("azure-cli", strings.TrimSpace(os.Getenv("AZURE_CLI")), "Azure CLI path")
	makeDefault := flags.Bool("default", false, "make this the default Azure OpenAI profile")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if len(deploymentFlags) == 0 {
		if value := strings.TrimSpace(os.Getenv("AZURE_OPENAI_DEPLOYMENT")); value != "" {
			deploymentFlags = append(deploymentFlags, value)
		}
	}
	reader := bufio.NewReader(r.in)
	var err error
	if strings.TrimSpace(*nameFlag) == "" {
		if !readerIsTerminal(r.in) {
			return errors.New("usage: sr azure add <profile> --endpoint <url> [--deployment <model=name> ...]")
		}
		*nameFlag, err = promptLine(r.out, reader, "Profile name: ")
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(*endpoint) == "" {
		if !readerIsTerminal(r.in) {
			return errors.New("Azure OpenAI endpoint is required; pass --endpoint")
		}
		*endpoint, err = promptLine(r.out, reader, "Azure OpenAI endpoint: ")
		if err != nil {
			return err
		}
	}
	deployments, legacyDeployment, err := parseAzureDeploymentFlags(deploymentFlags)
	if err != nil {
		return err
	}
	cliPath, err := resolveAzureCLI(*azureCLI)
	if err != nil {
		return err
	}
	profile, err := azureopenai.NormalizeProfile(azureopenai.Profile{
		Name:          *nameFlag,
		Endpoint:      *endpoint,
		Deployment:    legacyDeployment,
		Deployments:   deployments,
		TokenResource: *tokenResource,
		AzureCLI:      cliPath,
	})
	if err != nil {
		return err
	}
	// Validate the selected Azure CLI session before committing the profile.
	// The returned bearer token stays in memory and is deliberately discarded.
	if _, err := azureopenai.FetchCLIAccessToken(ctx, r.commandRunner(), profile); err != nil {
		return err
	}
	existed, err := r.azureProfileStore().Save(profile)
	if err != nil {
		return err
	}
	if *makeDefault {
		if err := r.azureProfileStore().SetDefault(profile.Name); err != nil {
			return err
		}
	}
	if err := r.restartAzureDaemon(); err != nil {
		return err
	}
	verb := "Added"
	if existed {
		verb = "Updated"
	}
	fmt.Fprintf(r.out, "%s Azure OpenAI profile %s (%s). Access tokens are renewed through Azure CLI and are not stored.\n", verb, profile.Name, azureDeploymentSummary(profile))
	return nil
}

func parseAzureDeploymentFlags(values []string) (map[string]string, string, error) {
	if len(values) == 0 {
		return azureopenai.DefaultGPT56Deployments(), "", nil
	}
	deployments := make(map[string]string, len(values))
	legacy := ""
	for _, value := range values {
		model, deployment, mapped := strings.Cut(value, "=")
		if !mapped {
			if len(values) != 1 {
				return nil, "", errors.New("a bare --deployment cannot be combined with model=deployment mappings")
			}
			legacy = strings.TrimSpace(value)
			continue
		}
		canonical, ok := azureopenai.CanonicalGPT56Model(model)
		if !ok || strings.TrimSpace(model) == "" {
			return nil, "", fmt.Errorf("unsupported Azure OpenAI model %q; use sol, terra, or luna", model)
		}
		deployment = strings.TrimSpace(deployment)
		if existing, exists := deployments[canonical]; exists && existing != deployment {
			return nil, "", fmt.Errorf("conflicting deployment mappings for %s", canonical)
		}
		deployments[canonical] = deployment
	}
	return deployments, legacy, nil
}

func azureDeploymentSummary(profile azureopenai.Profile) string {
	if len(profile.Deployments) == 0 {
		return "deployment " + profile.Deployment
	}
	parts := make([]string, 0, len(profile.Deployments))
	for _, model := range azureopenai.GPT56Models() {
		if deployment, ok := profile.Deployments[model]; ok {
			parts = append(parts, azureopenai.GPT56ModelAlias(model)+"="+deployment)
		}
	}
	return "deployments " + strings.Join(parts, ", ")
}

func resolveAzureCLI(explicit string) (string, error) {
	name := strings.TrimSpace(explicit)
	if name == "" {
		name = "az"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", errors.New("Azure CLI not found; install 'az', run 'az login', then retry")
	}
	return path, nil
}

func (r srRunner) azureList() error {
	store := r.azureProfileStore()
	profiles, err := store.List()
	if err != nil {
		return err
	}
	if len(profiles) == 0 {
		fmt.Fprintln(r.out, "No Azure OpenAI profiles. Run: sr azure add <profile> --endpoint <url>")
		return nil
	}
	defaultProfile, _, err := store.Default()
	if err != nil {
		return err
	}
	for _, profile := range profiles {
		marker := " "
		if profile.Name == defaultProfile.Name {
			marker = "*"
		}
		fmt.Fprintf(r.out, "%s %s\t%s\t%s\n", marker, profile.Name, azureDeploymentSummary(profile), profile.Endpoint)
	}
	return nil
}

func (r srRunner) azureDefault(args []string) error {
	store := r.azureProfileStore()
	switch len(args) {
	case 0:
		profile, ok, err := store.Default()
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("no Azure OpenAI profiles; run 'sr az add <profile> --endpoint <url>'")
		}
		fmt.Fprintf(r.out, "%s\n", profile.Name)
		return nil
	case 1:
		if err := store.SetDefault(args[0]); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "Default Azure OpenAI profile set to %s.\n", strings.ToLower(strings.TrimSpace(args[0])))
		return nil
	default:
		return errors.New("usage: sr azure default [profile]")
	}
}

func (r srRunner) azureRemove(name string) error {
	removed, err := r.azureProfileStore().Remove(name)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("Azure OpenAI profile %q not found", name)
	}
	if err := r.restartAzureDaemon(); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Removed Azure OpenAI profile %s.\n", strings.ToLower(strings.TrimSpace(name)))
	return nil
}

func (r srRunner) azureCodex(ctx context.Context, args []string) error {
	profile, codexArgsRaw, err := r.azureCodexProfile(args)
	if err != nil {
		return err
	}
	requested := codexModelArg(codexArgsRaw)
	codexModel, _, ok := profile.ResolveModel(requested)
	if !ok {
		if len(profile.Deployments) == 0 {
			return fmt.Errorf("Azure profile %s is bound to deployment %q, not %q", profile.Name, profile.Deployment, requested)
		}
		return fmt.Errorf("Azure profile %s has no mapping for model %q; use sol, terra, or luna", profile.Name, requested)
	}
	codexArgsRaw = rewriteCodexModelArg(codexArgsRaw, codexModel)
	// Fail before starting Codex when the Azure CLI session is absent or stale.
	if _, err := azureopenai.FetchCLIAccessToken(ctx, r.commandRunner(), profile); err != nil {
		return err
	}
	local := localBaseURL()
	client := fallbackHTTPClient()
	if r.client != nil {
		client = r.client
	}
	if !ensureLocalHealthy(ctx, client, local, defaultDaemonStarter(), r.errOut) {
		return fmt.Errorf("local proxy is unavailable; run '%s setup'", programBase())
	}
	baseURL, err := azureCodexBaseURL(local, profile.Name)
	if err != nil {
		return err
	}
	cloudConfig, err := cloudModeConfig()
	if err != nil {
		return err
	}
	localProxyToken := cloudClientProxyToken(cloudConfig, local)
	childArgs := azureCodexArgs(codexArgsRaw, baseURL, codexModel, localProxyToken != "")
	env := []string(nil)
	if localProxyToken != "" {
		env = []string{"SUBROUTER_CODEX_DUMMY_API_KEY=" + localProxyToken}
	}
	return r.commandRunner().RunWithEnv(
		ctx,
		envOrDefault("SUBROUTER_CODEX_BIN", "codex"),
		childArgs,
		env,
		r.in,
		r.out,
		r.errOut,
	)
}

func (r srRunner) azureCodexProfile(args []string) (azureopenai.Profile, []string, error) {
	store := r.azureProfileStore()
	profileName := ""
	codexArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--azure-profile":
			if i+1 >= len(args) {
				return azureopenai.Profile{}, nil, errors.New("--azure-profile requires a profile name")
			}
			profileName = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--azure-profile="):
			profileName = strings.TrimPrefix(args[i], "--azure-profile=")
		default:
			codexArgs = append(codexArgs, args[i])
		}
	}
	if strings.TrimSpace(profileName) != "" {
		profile, ok, err := store.Find(profileName)
		if err != nil {
			return azureopenai.Profile{}, nil, err
		}
		if !ok {
			return azureopenai.Profile{}, nil, fmt.Errorf("Azure OpenAI profile %q not found; run 'sr az list'", profileName)
		}
		return profile, codexArgs, nil
	}

	// Preserve the original positional profile syntax when the first argument
	// names an existing profile. Otherwise every argument belongs to Codex.
	if len(codexArgs) > 0 && !strings.HasPrefix(codexArgs[0], "-") {
		profile, ok, err := store.Find(codexArgs[0])
		if err != nil {
			return azureopenai.Profile{}, nil, err
		}
		if ok {
			return profile, codexArgs[1:], nil
		}
	}
	profile, ok, err := store.Default()
	if err != nil {
		return azureopenai.Profile{}, nil, err
	}
	if !ok {
		return azureopenai.Profile{}, nil, errors.New("no Azure OpenAI profiles; run 'sr az add <profile> --endpoint <url>'")
	}
	return profile, codexArgs, nil
}

func rewriteCodexModelArg(args []string, deployment string) []string {
	out := append([]string(nil), args...)
	for i := 0; i < len(out); i++ {
		switch {
		case out[i] == "-m" || out[i] == "--model":
			if i+1 < len(out) {
				out[i+1] = deployment
				i++
			}
		case strings.HasPrefix(out[i], "--model="):
			out[i] = "--model=" + deployment
		case strings.HasPrefix(out[i], "-m="):
			out[i] = "-m=" + deployment
		}
	}
	return out
}

func azureCodexBaseURL(localBaseURL, profileName string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(localBaseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid local Subrouter URL %q", localBaseURL)
	}
	parsed.Path = azureOpenAIClientPath(profileName) + "/v1"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func azureOpenAIClientPath(profileName string) string {
	return "/azure/" + strings.ToLower(strings.TrimSpace(profileName))
}

func azureCodexArgs(args []string, baseURL, deployment string, forceAuthenticatedProvider bool) []string {
	authConfig := `model_providers.subrouter_azure.experimental_bearer_token="subrouter"`
	if forceAuthenticatedProvider {
		authConfig = `model_providers.subrouter_azure.env_key="SUBROUTER_CODEX_DUMMY_API_KEY"`
	}
	configArgs := []string{
		"-c", "model=" + strconv.Quote(deployment),
		"-c", `model_provider="subrouter_azure"`,
		"-c", `model_providers.subrouter_azure.name="Azure OpenAI via Subrouter"`,
		"-c", "model_providers.subrouter_azure.base_url=" + strconv.Quote(baseURL),
		"-c", authConfig,
		"-c", `model_providers.subrouter_azure.wire_api="responses"`,
		"-c", `model_providers.subrouter_azure.supports_websockets=false`,
		"-c", `model_providers.subrouter_azure.http_headers={"X-Subrouter-Agent"="codex"}`,
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") || !isKnownCodexCommand(args[0]) {
		return append(configArgs, args...)
	}
	if !isSubrouterRoutedCodexCommand(args[0]) {
		return append([]string(nil), args...)
	}
	out := []string{args[0]}
	out = append(out, configArgs...)
	out = append(out, args[1:]...)
	return out
}

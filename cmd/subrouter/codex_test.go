package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
)

func TestCodexArgsUsesAuthenticatedSubrouterProviderByDefault(t *testing.T) {
	got := codexArgs([]string{"exec", "--cd", "/tmp", "prompt"}, "http://127.0.0.1:31415/v1", "", "")
	want := append([]string{"exec", "--cd", "/tmp", "prompt"}, defaultSubrouterCodexConfigArgs("http://127.0.0.1:31415/v1")...)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestDefaultCodexBaseURLDoesNotGuessSingleConfiguredServer(t *testing.T) {
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	if err := store.save(srServerFile{Servers: []srServerConfig{{
		Name: "community",
		URL:  "http://100.99.8.37:31415",
	}}}); err != nil {
		t.Fatal(err)
	}

	got, err := defaultCodexBaseURLFor(store)
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultCodexBaseURL {
		t.Fatalf("base URL = %q, want %q", got, defaultCodexBaseURL)
	}
}

func TestDefaultCodexBaseURLUsesExplicitDefaultServer(t *testing.T) {
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	if err := store.save(srServerFile{
		Default: "community",
		Servers: []srServerConfig{
			{Name: "community", URL: "http://100.99.8.37:31415"},
			{Name: "other", URL: "http://100.99.8.38:31415"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := defaultCodexBaseURLFor(store)
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://100.99.8.37:31415/v1" {
		t.Fatalf("base URL = %q", got)
	}
}

func TestCodexNamedLoopbackServerIsNotBuiltInLocal(t *testing.T) {
	serverStore := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	if err := serverStore.save(srServerFile{
		Default: "shadow",
		Servers: []srServerConfig{{Name: "shadow", URL: localBaseURL()}},
	}); err != nil {
		t.Fatal(err)
	}
	if codexResolvedTargetIsBuiltInLocal(serverStore, localBaseURL()) {
		t.Fatal("named loopback server was classified as the built-in local daemon")
	}
	t.Setenv("SUBROUTER_CODEX_SERVER", "local")
	if !codexResolvedTargetIsBuiltInLocal(serverStore, localBaseURL()) {
		t.Fatal("explicit built-in local server was classified as remote")
	}
}

func TestCodexRemoteFallbackIsBuiltInLocal(t *testing.T) {
	serverStore := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	if err := serverStore.save(srServerFile{
		Default: "remote",
		Servers: []srServerConfig{{Name: "remote", URL: "https://router.example"}},
	}); err != nil {
		t.Fatal(err)
	}
	if !codexResolvedTargetIsBuiltInLocal(serverStore, localBaseURL()) {
		t.Fatal("local fallback from a distinct remote was classified as remote")
	}
}

func TestCodexExplicitLoopbackURLIsNotBuiltInLocal(t *testing.T) {
	t.Setenv("SUBROUTER_CODEX_BASE_URL", localBaseURL())
	serverStore := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	if codexResolvedTargetIsBuiltInLocal(serverStore, localBaseURL()) {
		t.Fatal("explicit loopback URL was classified as the built-in local daemon")
	}
}

func TestCodexLocalCredentialModeIgnoresNamedLoopbackDefault(t *testing.T) {
	home := t.TempDir()
	cloudPath := filepath.Join(home, "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"credentialSource":"local"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	serverStore := srServerStore{Path: filepath.Join(home, "servers.json")}
	if err := serverStore.save(srServerFile{
		Default: "stale-loopback",
		Servers: []srServerConfig{{Name: "stale-loopback", URL: localBaseURL()}},
	}); err != nil {
		t.Fatal(err)
	}
	if !codexResolvedTargetIsBuiltInLocal(serverStore, localBaseURL()) {
		t.Fatal("local credential mode did not classify its forced local daemon as built-in")
	}
	t.Setenv("SUBROUTER_CODEX_SERVER", "stale-loopback")
	if codexResolvedTargetIsBuiltInLocal(serverStore, localBaseURL()) {
		t.Fatal("explicit named loopback pin was classified as built-in local")
	}
}

func TestCodexBaseURLUsesServerEnvOverride(t *testing.T) {
	t.Setenv("SUBROUTER_CODEX_SERVER", "other")
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	if err := store.save(srServerFile{
		Default: "community",
		Servers: []srServerConfig{
			{Name: "community", URL: "http://100.99.8.37:31415"},
			{Name: "other", URL: "http://100.99.8.38:31415"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := codexBaseURL(store)
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://100.99.8.38:31415/v1" {
		t.Fatalf("base URL = %q", got)
	}
}

func TestCodexBaseURLUsesExplicitURLBeforeServerDefault(t *testing.T) {
	t.Setenv("SUBROUTER_CODEX_BASE_URL", "https://explicit.example/v1")
	t.Setenv("SUBROUTER_CODEX_SERVER", "other")
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}

	got, err := codexBaseURL(store)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://explicit.example/v1" {
		t.Fatalf("base URL = %q", got)
	}
}

func TestCodexBaseURLRejectsExplicitRemoteHTTPWithoutTenantPath(t *testing.T) {
	t.Setenv("SUBROUTER_CODEX_BASE_URL", "http://explicit.example/v1")
	got, err := codexBaseURL(srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")})
	if err == nil || got != "" || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("explicit plaintext base URL = %q, err = %v", got, err)
	}
}

func TestDirectPlainHTTPEnvironmentStripsAmbientProxies(t *testing.T) {
	env := []string{"HTTP_PROXY=http://proxy", "https_proxy=http://proxy", "ALL_PROXY=socks5://proxy", "no_proxy=localhost", "KEEP=value"}
	got := directPlainHTTPEnvironment(env, "http://100.88.0.9:31415/v1")
	joined := strings.Join(got, "\n")
	if strings.Contains(strings.ToLower(joined), "proxy=") || !strings.Contains(joined, "KEEP=value") {
		t.Fatalf("direct plaintext environment = %v", got)
	}
	if gotHTTPS := directPlainHTTPEnvironment(env, "https://router.example/v1"); !slices.Equal(gotHTTPS, env) {
		t.Fatalf("HTTPS environment changed: %v", gotHTTPS)
	}
}

func TestCodexBaseURLRejectsExplicitRemoteHTTPTenantPath(t *testing.T) {
	t.Setenv("SUBROUTER_CODEX_BASE_URL", "http://192.168.1.10:31415/t/"+testTenantKey+"/v1")
	got, err := codexBaseURL(srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")})
	if err == nil || got != "" || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("explicit tenant base URL = %q, err = %v", got, err)
	}
}

func TestCodexArgsWorksWithoutSubcommand(t *testing.T) {
	got := codexArgs(nil, "http://127.0.0.1:31415/v1", "", "")
	want := defaultSubrouterCodexConfigArgs("http://127.0.0.1:31415/v1")
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsPassesThroughCodexFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--help"},
		{"-h"},
		{"--version"},
		{"-V"},
	} {
		got := codexArgs(args, "http://127.0.0.1:31415/v1", "", "")
		if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
			t.Fatalf("args = %#v, want pass-through %#v", got, args)
		}
	}
}

func TestCodexArgsDoesNotTreatUtilityTextAfterTerminatorAsAFlag(t *testing.T) {
	got := codexArgs([]string{"--", "--help"}, "http://127.0.0.1:31415/v1", "", "")
	if !contains(got, `model_provider="subrouter"`) {
		t.Fatalf("positional utility text bypassed Subrouter routing: %#v", got)
	}
}

func TestCodexArgsDoesNotInjectIntoUtilitySubcommands(t *testing.T) {
	got := codexArgs([]string{"login", "--help"}, "http://127.0.0.1:31415/v1", "", "")
	want := []string{"login", "--help"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsPreservesRoutingConfigForUtilitySubcommands(t *testing.T) {
	for _, args := range [][]string{
		{"app", "-c", `openai_base_url="http://direct.example/v1"`, "/tmp/project"},
		{"login", `--config=model_provider="openai"`},
		{"-c", `model_provider="openai"`, "mcp-server"},
	} {
		got := codexArgs(args, "http://127.0.0.1:31415/v1", "", "")
		if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
			t.Fatalf("args = %#v, want pass-through %#v", got, args)
		}
	}
}

func TestCodexArgsDoesNotMistakeGlobalOptionValueForUtilitySubcommand(t *testing.T) {
	got := codexArgs(
		[]string{"--model", "app", "write an implementation"},
		"http://127.0.0.1:31415/v1",
		"",
		"",
	)
	if !contains(got, `model_provider="subrouter"`) {
		t.Fatalf("interactive args were mistaken for a utility subcommand: %#v", got)
	}
}

func TestCodexArgsDoesNotMistakeVariadicImageValuesForUtilitySubcommands(t *testing.T) {
	got := codexArgs(
		[]string{"--image", "first.png", "login", "app"},
		"http://127.0.0.1:31415/v1",
		"",
		"",
	)
	if !contains(got, `model_provider="subrouter"`) {
		t.Fatalf("image filenames were mistaken for a utility subcommand: %#v", got)
	}
}

func TestCodexArgsFindsUtilitySubcommandAfterVariadicImagesEndAtOption(t *testing.T) {
	args := []string{"--image", "first.png", "second.png", "--model", "gpt-5.6-sol", "login"}
	got := codexArgs(args, "http://127.0.0.1:31415/v1", "", "")
	if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
		t.Fatalf("args = %#v, want utility pass-through %#v", got, args)
	}
}

func TestCodexArgsFindsUtilitySubcommandAfterAttachedImageValue(t *testing.T) {
	for _, imageArg := range []string{"--image=first.png", "-i=first.png", "-ifirst.png"} {
		args := []string{imageArg, "login", "--help"}
		got := codexArgs(args, "http://127.0.0.1:31415/v1", "", "")
		if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
			t.Fatalf("%s args = %#v, want utility pass-through %#v", imageArg, got, args)
		}
	}
}

func TestCodexUtilityRunsWithoutResolvingProxyOrPublishingResumeMetadata(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "codex-fake")
	record := filepath.Join(home, "record")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + shellQuote(record) + "\nenv | grep -E '^(SUBROUTER_CODEX_LAUNCHER|SUBROUTER_CODEX_RESUME_COMMAND|SUBROUTER_CODEX_DUMMY_API_KEY|SUBROUTER_ADMIN_TOKEN|SUBROUTER_FUTURE_KEY_FILE|SUBROUTER_CLOUD_CONFIG|SUBROUTER_STATE_DIR)=' >> " + shellQuote(record) + " || true\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_CODEX_BIN", bin)
	t.Setenv("SUBROUTER_CODEX_SERVER", "missing-server-that-must-not-be-resolved")
	t.Setenv(subrouterCodexLauncherEnv, "stale launcher")
	t.Setenv(subrouterCodexResumeCommandEnv, "stale resume")
	t.Setenv("SUBROUTER_CODEX_DUMMY_API_KEY", "stale-key")
	t.Setenv("SUBROUTER_ADMIN_TOKEN", "durable-admin-secret")
	t.Setenv("SUBROUTER_FUTURE_KEY_FILE", "/private/future-key")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", "/private/cloud-config")
	t.Setenv("SUBROUTER_STATE_DIR", "/private/state")
	if err := codex([]string{"login", "--help"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "login --help\n" {
		t.Fatal("utility launch published routing metadata or durable environment")
	}
}

func TestCodexLocalLaunchKeepsDurableProxyTokenInShortLivedRelay(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/health" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader)
		if challenge == "" {
			_, _ = fmt.Fprint(response, `{"status":"ok"}`)
			return
		}
		id, err := accounts.StoreAuthorityID(store.Dir)
		if err != nil {
			t.Fatal(err)
		}
		proof, err := accounts.StoreAuthorityProof(store.Dir, challenge)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(response, `{"account_store_id":%q,"account_store_proof":%q}`, id, proof)
	}))
	defer upstream.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", upstream.URL+"/v1")

	const durableToken = "durable-codex-local-token-must-not-reach-child"
	cloudPath := filepath.Join(home, "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := broker.SaveConfig(cloudPath, broker.Config{
		CredentialSource: broker.CredentialSourceLocal,
		LocalProxyToken:  durableToken,
	}); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "codex-fake")
	record := filepath.Join(home, "record")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + shellQuote(record) + "\nenv | grep '^SUBROUTER_CODEX_DUMMY_API_KEY=' >> " + shellQuote(record) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_CODEX_BIN", bin)
	if err := codex([]string{"exec", "prompt"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if strings.Contains(got, durableToken) || strings.Contains(got, upstream.URL) {
		t.Fatal("Codex child received durable local routing material (record redacted)")
	}
	if !strings.Contains(got, "SUBROUTER_CODEX_DUMMY_API_KEY=") ||
		!strings.Contains(got, `model_providers.subrouter.base_url="http://127.0.0.1:`) {
		t.Fatal("Codex child did not receive a loopback relay capability (record redacted)")
	}
}

func TestCodexNamedLoopbackServerDoesNotReceiveDurableProxyToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/health" {
			http.NotFound(response, request)
			return
		}
		_, _ = fmt.Fprint(response, `{"status":"ok"}`)
	}))
	defer upstream.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", upstream.URL+"/v1")
	t.Setenv("SUBROUTER_CODEX_SERVER", "shadow")
	if err := defaultSRServerStore(store).save(srServerFile{
		Servers: []srServerConfig{{Name: "shadow", URL: upstream.URL}},
	}); err != nil {
		t.Fatal(err)
	}

	const durableToken = "durable-codex-local-token-must-not-reach-named-server"
	cloudPath := filepath.Join(home, "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := broker.SaveConfig(cloudPath, broker.Config{
		CredentialSource: broker.CredentialSourceLocal,
		LocalProxyToken:  durableToken,
	}); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "codex-fake")
	record := filepath.Join(home, "record")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + shellQuote(record) + "\nenv | grep '^SUBROUTER_CODEX_DUMMY_API_KEY=' >> " + shellQuote(record) + " || true\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_CODEX_BIN", bin)
	if err := codex([]string{"exec", "prompt"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if strings.Contains(got, durableToken) || strings.Contains(got, "SUBROUTER_CODEX_DUMMY_API_KEY=") {
		t.Fatal("named loopback Codex child received the durable local proxy credential")
	}
	if !strings.Contains(got, `model_providers.subrouter.base_url="`+upstream.URL+`/v1"`) ||
		!strings.Contains(got, `experimental_bearer_token="subrouter"`) {
		t.Fatal("named loopback Codex child did not retain unauthenticated compatibility routing")
	}
}

func TestCodexCommandlessUtilityFlagsRunWithoutResolvingProxy(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "codex-fake")
	record := filepath.Join(home, "record")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + shellQuote(record) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_CODEX_BIN", bin)
	t.Setenv("SUBROUTER_CODEX_SERVER", "missing-server-that-must-not-be-resolved")

	for _, arg := range []string{"--help", "-h", "--version", "-V"} {
		t.Run(arg, func(t *testing.T) {
			if err := codex([]string{arg}); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := string(body), arg+"\n"; got != want {
				t.Fatalf("utility launch record = %q, want %q", got, want)
			}
		})
	}
}

func TestCodexArgsInjectsSubrouterBaseURLIntoAppServer(t *testing.T) {
	got := codexArgs([]string{"app-server", "--listen", "off"}, "http://127.0.0.1:31415/v1", "", "")
	want := append([]string{"app-server", "--listen", "off"}, defaultSubrouterCodexConfigArgs("http://127.0.0.1:31415/v1")...)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsInjectsSubrouterProviderIntoRemoteControl(t *testing.T) {
	got := codexArgs([]string{"remote-control", "start"}, "http://127.0.0.1:31415/v1", "", "")
	want := append([]string{"remote-control", "start"}, defaultSubrouterCodexConfigArgs("http://127.0.0.1:31415/v1")...)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsInjectsIntoRemoteControlStartAfterOptions(t *testing.T) {
	for _, args := range [][]string{
		{"remote-control", "-c", "features.foo=true", "start"},
		{"remote-control", "--config", "features.foo=true", "--enable", "bar", "start"},
		{"remote-control", "--disable", "bar", "--json", "start"},
	} {
		got := codexArgs(args, "http://127.0.0.1:31415/v1", "", "")
		if !contains(got, `model_provider="subrouter"`) {
			t.Fatalf("remote-control start options were mistaken for management args: %#v", got)
		}
	}
}

func TestCodexArgsDoesNotInjectIntoRemoteControlManagement(t *testing.T) {
	for _, args := range [][]string{
		{"remote-control", "stop"},
		{"remote-control", "pair"},
		{"remote-control", "--help"},
	} {
		got := codexArgs(args, "http://127.0.0.1:31415/v1", "", "")
		if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
			t.Fatalf("args = %#v, want management pass-through %#v", got, args)
		}
	}
}

func TestCodexRemoteControlManagementDoesNotResolveProxy(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "codex-fake")
	record := filepath.Join(home, "record")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + shellQuote(record) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_CODEX_BIN", bin)
	t.Setenv("SUBROUTER_CODEX_SERVER", "missing-server-that-must-not-be-resolved")
	if err := codex([]string{"remote-control", "stop"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "remote-control stop\n" {
		t.Fatalf("management launch record = %q", got)
	}
}

func TestCodexArgsDoesNotInjectIntoDesktopAppLauncher(t *testing.T) {
	got := codexArgs([]string{"app", "/tmp/project"}, "http://127.0.0.1:31415/v1", "", "")
	want := []string{"app", "/tmp/project"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsTreatsUnknownCommandAsInteractivePrompt(t *testing.T) {
	got := codexArgs([]string{"write", "tests"}, "http://127.0.0.1:31415/v1", "", "")
	want := append([]string{"write", "tests"}, defaultSubrouterCodexConfigArgs("http://127.0.0.1:31415/v1")...)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func defaultSubrouterCodexConfigArgs(baseURL string) []string {
	return []string{
		"-c", `model_provider="subrouter"`,
		"-c", `model_providers.subrouter.name="Subrouter"`,
		"-c", `model_providers.subrouter.base_url="` + baseURL + `"`,
		"-c", `model_providers.subrouter.experimental_bearer_token="subrouter"`,
		"-c", `model_providers.subrouter.wire_api="responses"`,
		"-c", `model_providers.subrouter.supports_websockets=true`,
		"-c", `model_providers.subrouter.http_headers={"X-Subrouter-Agent"="codex"}`,
		"-c", `model_providers.subrouter={name="Subrouter",base_url="` + baseURL + `",experimental_bearer_token="subrouter",wire_api="responses",supports_websockets=true,http_headers={"X-Subrouter-Agent"="codex"}}`,
	}
}

func TestCodexArgsInjectsUserEmailWithCustomSubrouterProvider(t *testing.T) {
	got := codexArgs([]string{"exec", "prompt"}, "http://127.0.0.1:31415/v1", "alice@example.com", "")
	want := []string{
		"exec",
		"prompt",
		"-c", `model_provider="subrouter"`,
		"-c", `model_providers.subrouter.name="Subrouter"`,
		"-c", `model_providers.subrouter.base_url="http://127.0.0.1:31415/v1"`,
		"-c", `model_providers.subrouter.experimental_bearer_token="subrouter"`,
		"-c", `model_providers.subrouter.wire_api="responses"`,
		"-c", `model_providers.subrouter.supports_websockets=true`,
		"-c", `model_providers.subrouter.http_headers={"X-Subrouter-Agent"="codex","X-Subrouter-User-Email"="alice@example.com"}`,
		"-c", `model_providers.subrouter={name="Subrouter",base_url="http://127.0.0.1:31415/v1",experimental_bearer_token="subrouter",wire_api="responses",supports_websockets=true,http_headers={"X-Subrouter-Agent"="codex","X-Subrouter-User-Email"="alice@example.com"}}`,
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsInjectsAccountIDWithCustomSubrouterProvider(t *testing.T) {
	got := codexArgs([]string{"exec", "prompt"}, "http://127.0.0.1:31415/v1", "", "team-codex-1")
	want := []string{
		"exec",
		"prompt",
		"-c", `model_provider="subrouter"`,
		"-c", `model_providers.subrouter.name="Subrouter"`,
		"-c", `model_providers.subrouter.base_url="http://127.0.0.1:31415/v1"`,
		"-c", `model_providers.subrouter.experimental_bearer_token="subrouter"`,
		"-c", `model_providers.subrouter.wire_api="responses"`,
		"-c", `model_providers.subrouter.supports_websockets=true`,
		"-c", `model_providers.subrouter.http_headers={"X-Subrouter-Agent"="codex","X-Subrouter-Account-ID"="team-codex-1"}`,
		"-c", `model_providers.subrouter={name="Subrouter",base_url="http://127.0.0.1:31415/v1",experimental_bearer_token="subrouter",wire_api="responses",supports_websockets=true,http_headers={"X-Subrouter-Agent"="codex","X-Subrouter-Account-ID"="team-codex-1"}}`,
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsKeepsCustomProviderAuthInResumableArguments(t *testing.T) {
	got := codexArgs([]string{"exec", "prompt"}, "http://127.0.0.1:31415/v1", "", "team-codex-1")
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, `env_key="SUBROUTER_CODEX_DUMMY_API_KEY"`) {
		t.Fatalf("args depend on process-only environment and will fail when Codex is resumed directly:\n%s", joined)
	}
	if !strings.Contains(joined, `experimental_bearer_token="subrouter"`) {
		t.Fatalf("args lack self-contained provider authentication:\n%s", joined)
	}
}

func TestCodexArgsInjectsUserEmailAndAccountID(t *testing.T) {
	got := codexArgs([]string{"exec", "prompt"}, "http://127.0.0.1:31415/v1", "alice@example.com", "apikey:paid")
	headers := `model_providers.subrouter.http_headers={"X-Subrouter-Agent"="codex","X-Subrouter-User-Email"="alice@example.com","X-Subrouter-Account-ID"="apikey:paid"}`
	if !contains(got, headers) {
		t.Fatalf("args = %#v, want headers %q", got, headers)
	}
}

func TestCodexArgsInjectsModelHeader(t *testing.T) {
	got := codexArgs([]string{"exec", "-m", "GPT-5.3-Codex-Spark", "prompt"}, "http://127.0.0.1:31415/v1", "", "")
	headers := `model_providers.subrouter.http_headers={"X-Subrouter-Agent"="codex","X-Subrouter-Model"="GPT-5.3-Codex-Spark"}`
	if !contains(got, headers) {
		t.Fatalf("args = %#v, want headers %q", got, headers)
	}
}

func TestCodexModelArgSupportsEqualsForm(t *testing.T) {
	if got := codexModelArg([]string{"exec", "--model=GPT-5.3-Codex-Spark"}); got != "GPT-5.3-Codex-Spark" {
		t.Fatalf("model = %q", got)
	}
}

func TestCodexModelArgUsesLastOverrideBeforeTerminator(t *testing.T) {
	got := codexModelArg([]string{
		"-m", "first",
		"--model=second",
		"-m=third",
		"--",
		"--model=prompt-value",
	})
	if got != "third" {
		t.Fatalf("model = %q, want third", got)
	}
}

func TestCodexArgsDropsStaleSubrouterOverridesFromResume(t *testing.T) {
	parentOverride := `model_providers={subrouter={name="Parent",base_url="http://parent-table.example/v1",env_key="STALE",query_params={stale="1"},wire_api="responses"}}`
	got := codexArgs([]string{
		"resume",
		"-c", parentOverride,
		"-c", `model_provider="subrouter"`,
		"-c", `model_providers.subrouter={name="Retired",base_url="http://whole-table-retired.invalid/v1",wire_api="responses"}`,
		"--config", `model_providers.subrouter.base_url="http://retired.invalid/v1"`,
		`--config=model_providers.subrouter.supports_websockets=false`,
		`-c=chatgpt_base_url="http://retired.invalid/backend-api"`,
		`-cmodel_providers.subrouter.base_url="http://attached-retired.invalid/v1"`,
		"-c", `model="gpt-5.6-sol"`,
		"thread-id",
	}, "http://current.example/v1", "", "")
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "retired.invalid") || strings.Contains(joined, "supports_websockets=false") {
		t.Fatalf("stale routing override survived:\n%s", joined)
	}
	if strings.Contains(joined, parentOverride) || strings.Contains(joined, `env_key="STALE"`) || strings.Contains(joined, `query_params={stale="1"}`) {
		t.Fatalf("stale parent-table provider override survived:\n%s", joined)
	}
	for _, want := range []string{
		`model_provider="subrouter"`,
		`model_providers.subrouter.base_url="http://current.example/v1"`,
		`model_providers.subrouter.supports_websockets=true`,
		`model="gpt-5.6-sol"`,
		"thread-id",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Count(joined, `model_provider="subrouter"`) != 1 {
		t.Fatalf("model provider was not canonicalized:\n%s", joined)
	}
	lastAssignment := ""
	for i := 0; i+1 < len(got); i++ {
		if got[i] == "-c" || got[i] == "--config" {
			lastAssignment = got[i+1]
			i++
		}
	}
	if !strings.HasPrefix(lastAssignment, `model_providers.subrouter={`) || strings.Contains(lastAssignment, "Parent") {
		t.Fatalf("last config is not the authoritative whole provider table: %q", lastAssignment)
	}
}

func TestInlineTableOwnsSubrouterOnlyAtTheParentLevel(t *testing.T) {
	for _, value := range []string{
		`{subrouter={env_key="STALE"}}`,
		`{"subrouter"={query_params={stale="1"}}}`,
		`{subrouter.base_url="http://stale.invalid"}`,
	} {
		if !inlineTableOwnsSubrouter(value) {
			t.Errorf("inlineTableOwnsSubrouter(%q) = false", value)
		}
	}
	for _, value := range []string{
		`{openai={name="mentions subrouter= only in a string"}}`,
		`{openai={query_params={subrouter="not a provider"}}}`,
	} {
		if inlineTableOwnsSubrouter(value) {
			t.Errorf("inlineTableOwnsSubrouter(%q) = true", value)
		}
	}
}

func TestSanitizeCodexRoutingArgsPreservesUnrelatedConfigAndMalformedPair(t *testing.T) {
	args := []string{
		"resume",
		"-c", `model_reasoning_effort="high"`,
		"--config=features.foo=true",
		"-c",
	}
	got := sanitizeCodexRoutingArgs(args)
	if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, args)
	}
}

func TestSanitizeCodexRoutingArgsPreservesArgumentsAfterTerminator(t *testing.T) {
	args := []string{
		"exec",
		"--",
		"-c", `model_provider="openai"`,
		`--config=model_providers.subrouter.base_url="http://prompt.example/v1"`,
	}
	got := sanitizeCodexRoutingArgs(args)
	if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
		t.Fatalf("args = %#v, want positional tail %#v", got, args)
	}
}

func TestCodexArgsDropsLocalProviderOverridesBeforeTerminator(t *testing.T) {
	got := codexArgs(
		[]string{
			"--oss",
			"--local-provider", "ollama",
			"--local-provider=lmstudio",
			"--",
			"--oss", "--local-provider", "prompt-value",
		},
		"http://current.example/v1",
		"",
		"",
	)
	joined := strings.Join(got, "\n")
	terminator := strings.Index(joined, "\n--\n")
	if terminator < 0 {
		t.Fatalf("missing option terminator:\n%s", joined)
	}
	if strings.Contains(joined[:terminator], "--oss") || strings.Contains(joined[:terminator], "--local-provider") {
		t.Fatalf("local-provider override survived before terminator:\n%s", joined)
	}
	if !strings.Contains(joined[terminator:], "--oss\n--local-provider\nprompt-value") {
		t.Fatalf("positional tail was changed:\n%s", joined)
	}
}

func TestCodexArgsPlacesAuthoritativeConfigBeforeTerminator(t *testing.T) {
	got := codexArgs(
		[]string{"exec", "-c", `model_providers={subrouter={base_url="http://parent.example/v1"}}`, "--", "-c", `model_provider="openai"`},
		"http://current.example/v1",
		"",
		"",
	)
	joined := strings.Join(got, "\n")
	current := strings.LastIndex(joined, `model_providers.subrouter.base_url="http://current.example/v1"`)
	terminator := strings.Index(joined, "\n--\n")
	positional := strings.LastIndex(joined, `model_provider="openai"`)
	if current < 0 || terminator < current || positional < terminator {
		t.Fatalf("authoritative config/terminator ordering is unsafe:\n%s", joined)
	}
}

func TestCodexArgsPlacesAuthoritativeConfigAfterOptionsFirstAndInteractiveOverrides(t *testing.T) {
	parent := `model_providers={subrouter={base_url="http://parent.example/v1"}}`
	for _, args := range [][]string{
		{"-c", parent, "exec", "prompt"},
		{"-c", parent, "interactive prompt"},
	} {
		got := codexArgs(args, "http://current.example/v1", "", "")
		joined := strings.Join(got, "\n")
		if strings.LastIndex(joined, `model_providers.subrouter.base_url="http://current.example/v1"`) <
			strings.Index(joined, parent) {
			t.Fatalf("authoritative config does not follow preserved parent override:\n%s", joined)
		}
	}
}

func TestCodexChildEnvMarksSubrouterResumeCommand(t *testing.T) {
	got := codexChildEnv([]string{
		"A=1",
		"SUBROUTER_ADMIN_TOKEN=admin-secret",
		"SUBROUTER_FUTURE_SECRET_FILE=/private/future",
		"SUBROUTER_CLOUD_CONFIG=/private/cloud-config",
		"SUBROUTER_STATE_DIR=/private/state",
		subrouterCodexLauncherEnv + "=old",
		subrouterCodexResumeCommandEnv + "=old resume",
	}, "local-secret", "subrouter")
	joined := strings.Join(got, "\n")
	for i, want := range []string{
		subrouterCodexLauncherEnv + "=subrouter codex",
		subrouterCodexResumeCommandEnv + "=subrouter codex resume",
		"SUBROUTER_CODEX_DUMMY_API_KEY=local-secret",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("required child environment entry %d is missing", i)
		}
	}
	for i, forbidden := range []string{"admin-secret", "/private/future", "/private/cloud-config", "/private/state"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("durable Subrouter secret entry %d was retained", i)
		}
	}
}

func TestCodexChildEnvOnlyTrustsKnownLauncherAliases(t *testing.T) {
	for _, test := range []struct {
		launcher string
		want     string
	}{
		{launcher: "sr", want: "sr"},
		{launcher: "subrouter", want: "subrouter"},
		{launcher: "cx", want: "cx"},
		{launcher: "/tmp/malicious launcher", want: "sr"},
		{launcher: "SR", want: "sr"},
	} {
		got := strings.Join(codexChildEnv(nil, "", test.launcher), "\n")
		if !strings.Contains(got, subrouterCodexResumeCommandEnv+"="+test.want+" codex resume") {
			t.Fatalf("launcher %q env = %q", test.launcher, got)
		}
	}
}

func TestUpsertEnvReplacesExistingValue(t *testing.T) {
	got := upsertEnv([]string{"A=1", "SUBROUTER_CODEX_DUMMY_API_KEY=old"}, "SUBROUTER_CODEX_DUMMY_API_KEY", "subrouter")
	want := []string{"A=1", "SUBROUTER_CODEX_DUMMY_API_KEY=subrouter"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("env = %#v, want %#v", got, want)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

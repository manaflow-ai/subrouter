package azureopenai

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/storepath"
)

const (
	CognitiveServicesTokenResource = "https://cognitiveservices.azure.com"
	FoundryTokenResource           = "https://ai.azure.com"
	GPT56Sol                       = "gpt-5.6-sol"
	GPT56Terra                     = "gpt-5.6-terra"
	GPT56Luna                      = "gpt-5.6-luna"
	DefaultGPT56Model              = GPT56Sol
)

var profileNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

type Profile struct {
	Name          string            `json:"name"`
	Endpoint      string            `json:"endpoint"`
	Deployment    string            `json:"deployment,omitempty"`
	Deployments   map[string]string `json:"deployments,omitempty"`
	TokenResource string            `json:"tokenResource"`
	AzureCLI      string            `json:"azureCli"`
	AddedAt       string            `json:"addedAt"`
}

type Store struct {
	Path string
}

type profilesFile struct {
	Version        int       `json:"version"`
	DefaultProfile string    `json:"defaultProfile,omitempty"`
	Profiles       []Profile `json:"profiles"`
}

func DefaultStore() Store {
	return Store{Path: filepath.Join(storepath.CodexDir(), "azure-openai.json")}
}

func (s Store) ProfilesPath() string {
	if strings.TrimSpace(s.Path) != "" {
		return s.Path
	}
	return DefaultStore().Path
}

func NormalizeProfile(profile Profile) (Profile, error) {
	profile.Name = strings.ToLower(strings.TrimSpace(profile.Name))
	if !profileNamePattern.MatchString(profile.Name) {
		return Profile{}, errors.New("Azure OpenAI profile name must use 1-64 lowercase letters, digits, dots, dashes, or underscores")
	}

	endpoint, err := normalizeEndpoint(profile.Endpoint)
	if err != nil {
		return Profile{}, err
	}
	profile.Endpoint = endpoint.String()
	profile.Deployment = strings.TrimSpace(profile.Deployment)
	if profile.Deployment != "" {
		if err := validateDeploymentName(profile.Deployment); err != nil {
			return Profile{}, err
		}
	}
	deployments := make(map[string]string, len(profile.Deployments))
	deploymentModels := make(map[string]string, len(profile.Deployments))
	for rawModel, rawDeployment := range profile.Deployments {
		if strings.TrimSpace(rawModel) == "" {
			return Profile{}, errors.New("Azure OpenAI model mapping name is required")
		}
		model, ok := CanonicalGPT56Model(rawModel)
		if !ok {
			return Profile{}, fmt.Errorf("unsupported Azure OpenAI model mapping %q", rawModel)
		}
		deployment := strings.TrimSpace(rawDeployment)
		if err := validateDeploymentName(deployment); err != nil {
			return Profile{}, fmt.Errorf("Azure OpenAI %s mapping: %w", model, err)
		}
		if existing, exists := deployments[model]; exists && existing != deployment {
			return Profile{}, fmt.Errorf("conflicting Azure OpenAI deployment mappings for %s", model)
		}
		if existingModel, exists := deploymentModels[deployment]; exists && existingModel != model {
			return Profile{}, fmt.Errorf("Azure OpenAI deployment %q cannot map both %s and %s", deployment, existingModel, model)
		}
		deployments[model] = deployment
		deploymentModels[deployment] = model
	}
	if len(deployments) == 0 {
		if profile.Deployment == "" {
			return Profile{}, errors.New("at least one Azure OpenAI deployment is required")
		}
		profile.Deployments = nil
	} else {
		defaultDeployment, ok := deployments[DefaultGPT56Model]
		if !ok {
			return Profile{}, fmt.Errorf("Azure OpenAI profile requires a %s deployment mapping", DefaultGPT56Model)
		}
		if profile.Deployment != "" && profile.Deployment != defaultDeployment {
			return Profile{}, errors.New("Azure OpenAI compatibility deployment must match the Sol deployment mapping")
		}
		// Keep the default deployment in the legacy field so rolling back to a
		// single-deployment Subrouter still launches Sol instead of rejecting the
		// profile file. New binaries use Deployments for model selection.
		profile.Deployment = defaultDeployment
		profile.Deployments = deployments
	}

	profile.TokenResource = strings.TrimRight(strings.TrimSpace(profile.TokenResource), "/")
	if profile.TokenResource == "" {
		profile.TokenResource = FoundryTokenResource
	}
	if err := validateTokenResource(profile.TokenResource); err != nil {
		return Profile{}, err
	}
	profile.AzureCLI = strings.TrimSpace(profile.AzureCLI)
	if profile.AzureCLI == "" {
		return Profile{}, errors.New("Azure CLI path is required")
	}
	if strings.ContainsAny(profile.AzureCLI, "\r\n\x00") {
		return Profile{}, errors.New("Azure CLI path is invalid")
	}
	if profile.AddedAt != "" {
		if _, err := time.Parse(time.RFC3339, profile.AddedAt); err != nil {
			return Profile{}, errors.New("Azure OpenAI profile addedAt is invalid")
		}
	}
	return profile, nil
}

func validateDeploymentName(value string) error {
	if value == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
		return errors.New("Azure OpenAI deployment name is invalid")
	}
	return nil
}

func GPT56Models() []string {
	return []string{GPT56Sol, GPT56Terra, GPT56Luna}
}

func DefaultGPT56Deployments() map[string]string {
	return map[string]string{
		GPT56Sol:   GPT56Sol,
		GPT56Terra: GPT56Terra,
		GPT56Luna:  GPT56Luna,
	}
}

func CanonicalGPT56Model(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "gpt-5.6", "sol", GPT56Sol:
		return GPT56Sol, true
	case "terra", GPT56Terra:
		return GPT56Terra, true
	case "luna", GPT56Luna:
		return GPT56Luna, true
	default:
		return "", false
	}
}

func GPT56ModelAlias(model string) string {
	switch model {
	case GPT56Sol:
		return "sol"
	case GPT56Terra:
		return "terra"
	case GPT56Luna:
		return "luna"
	default:
		return model
	}
}

func (p Profile) DeploymentForModel(value string) (string, bool) {
	_, deployment, ok := p.ResolveModel(value)
	return deployment, ok
}

// ResolveModel returns the canonical Codex model slug and the Azure deployment
// that serves it. Keeping those names separate lets Codex retain its native
// model picker metadata while the proxy translates to custom deployment names.
func (p Profile) ResolveModel(value string) (string, string, bool) {
	if len(p.Deployments) == 0 {
		requested := strings.TrimSpace(value)
		if requested == "" || requested == p.Deployment {
			return p.Deployment, p.Deployment, true
		}
		return "", "", false
	}
	model, ok := CanonicalGPT56Model(value)
	if ok {
		deployment, exists := p.Deployments[model]
		return model, deployment, exists
	}
	requested := strings.TrimSpace(value)
	for model, deployment := range p.Deployments {
		if requested == deployment {
			return model, deployment, true
		}
	}
	return "", "", false
}

func normalizeEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Azure OpenAI endpoint must be an absolute URL without credentials, query, or fragment")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopbackHost(parsed.Hostname())) {
		return nil, errors.New("Azure OpenAI endpoint must use HTTPS, except for loopback development")
	}
	if !loopbackHost(parsed.Hostname()) && !azureEndpointHost(parsed.Hostname()) {
		return nil, errors.New("Azure OpenAI endpoint must use an Azure OpenAI, Foundry, or Cognitive Services hostname")
	}
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	switch path {
	case "", "/":
		parsed.Path = "/openai/v1"
		parsed.RawPath = ""
	case "/openai/v1":
		parsed.Path = "/openai/v1"
		parsed.RawPath = ""
	default:
		return nil, errors.New("Azure OpenAI endpoint path must be empty or /openai/v1")
	}
	return parsed, nil
}

func validateTokenResource(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("Azure token resource must be an HTTPS origin")
	}
	return nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSpace(host), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func azureEndpointHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, suffix := range []string{
		".openai.azure.com",
		".openai.azure.us",
		".openai.azure.cn",
		".services.ai.azure.com",
		".services.ai.azure.us",
		".services.ai.azure.cn",
		".cognitiveservices.azure.com",
		".cognitiveservices.azure.us",
		".cognitiveservices.azure.cn",
	} {
		if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
			return true
		}
	}
	return false
}

func (s Store) List() ([]Profile, error) {
	file, err := s.load()
	if err != nil {
		return nil, err
	}
	return file.Profiles, nil
}

func (s Store) load() (profilesFile, error) {
	body, err := os.ReadFile(s.ProfilesPath())
	if errors.Is(err, os.ErrNotExist) {
		return profilesFile{Version: 3}, nil
	}
	if err != nil {
		return profilesFile{}, err
	}
	var file profilesFile
	if err := json.Unmarshal(body, &file); err != nil {
		return profilesFile{}, fmt.Errorf("parse Azure OpenAI profiles: %w", err)
	}
	profiles := make([]Profile, 0, len(file.Profiles))
	seen := map[string]bool{}
	for _, raw := range file.Profiles {
		profile, err := NormalizeProfile(raw)
		if err != nil {
			return profilesFile{}, fmt.Errorf("Azure OpenAI profile %q: %w", raw.Name, err)
		}
		if seen[profile.Name] {
			return profilesFile{}, fmt.Errorf("duplicate Azure OpenAI profile %q", profile.Name)
		}
		seen[profile.Name] = true
		profiles = append(profiles, profile)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	file.Profiles = profiles
	file.DefaultProfile = strings.ToLower(strings.TrimSpace(file.DefaultProfile))
	if file.DefaultProfile == "" && len(profiles) > 0 {
		// Version 2 stores predate an explicit default. Their first sorted
		// profile becomes the deterministic default without requiring migration.
		file.DefaultProfile = profiles[0].Name
	}
	if file.DefaultProfile != "" && !seen[file.DefaultProfile] {
		return profilesFile{}, fmt.Errorf("default Azure OpenAI profile %q not found", file.DefaultProfile)
	}
	return file, nil
}

func (s Store) Find(name string) (Profile, bool, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	profiles, err := s.List()
	if err != nil {
		return Profile{}, false, err
	}
	for _, profile := range profiles {
		if profile.Name == name {
			return profile, true, nil
		}
	}
	return Profile{}, false, nil
}

func (s Store) Default() (Profile, bool, error) {
	file, err := s.load()
	if err != nil {
		return Profile{}, false, err
	}
	if file.DefaultProfile == "" {
		return Profile{}, false, nil
	}
	for _, profile := range file.Profiles {
		if profile.Name == file.DefaultProfile {
			return profile, true, nil
		}
	}
	return Profile{}, false, fmt.Errorf("default Azure OpenAI profile %q not found", file.DefaultProfile)
}

func (s Store) SetDefault(name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	file, err := s.load()
	if err != nil {
		return err
	}
	found := false
	for _, profile := range file.Profiles {
		if profile.Name == name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("Azure OpenAI profile %q not found", name)
	}
	file.DefaultProfile = name
	return s.write(file)
}

func (s Store) Save(profile Profile) (bool, error) {
	profile, err := NormalizeProfile(profile)
	if err != nil {
		return false, err
	}
	file, err := s.load()
	if err != nil {
		return false, err
	}
	existed := false
	for i := range file.Profiles {
		if file.Profiles[i].Name != profile.Name {
			continue
		}
		existed = true
		if profile.AddedAt == "" {
			profile.AddedAt = file.Profiles[i].AddedAt
		}
		file.Profiles[i] = profile
		break
	}
	if profile.AddedAt == "" {
		profile.AddedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if !existed {
		file.Profiles = append(file.Profiles, profile)
	}
	if file.DefaultProfile == "" {
		file.DefaultProfile = profile.Name
	}
	sort.Slice(file.Profiles, func(i, j int) bool { return file.Profiles[i].Name < file.Profiles[j].Name })
	return existed, s.write(file)
}

func (s Store) Remove(name string) (bool, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	file, err := s.load()
	if err != nil {
		return false, err
	}
	kept := file.Profiles[:0]
	removed := false
	for _, profile := range file.Profiles {
		if profile.Name == name {
			removed = true
			continue
		}
		kept = append(kept, profile)
	}
	if !removed {
		return false, nil
	}
	file.Profiles = kept
	if file.DefaultProfile == name {
		file.DefaultProfile = ""
		if len(kept) > 0 {
			file.DefaultProfile = kept[0].Name
		}
	}
	return true, s.write(file)
}

func (s Store) write(file profilesFile) error {
	file.Version = 3
	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	path := s.ProfilesPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

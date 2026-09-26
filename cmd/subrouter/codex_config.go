package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var codexRoutingConfigKeys = []string{
	"openai_base_url",
	"chatgpt_base_url",
	"experimental_realtime_ws_base_url",
}

func defaultCodexConfigPath() (string, error) {
	if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
		return filepath.Join(codexHome, "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

func writeCodexConfigForServer(server srServerConfig) (string, error) {
	if err := validateTenantServerConfig(context.Background(), server); err != nil {
		return "", err
	}
	baseURL := canonicalServerProxyRootURL(server) + "/v1"
	parsed, _ := url.Parse(baseURL)
	if parsed != nil && strings.EqualFold(parsed.Scheme, "http") && !isLoopbackServerHost(parsed.Hostname()) {
		// A durable config cannot carry the result of a one-time DNS/Tailscale
		// check. Keep ordinary Codex on the local proxy; `sr codex` supplies the
		// freshly verified remote URL as a process-scoped override.
		baseURL = defaultCodexBaseURL
	}
	return writeCodexConfigForBaseURL(baseURL)
}

func isLoopbackServerHost(host string) bool {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeCodexConfigForLocal() (string, error) {
	return writeCodexConfigForBaseURL(defaultCodexBaseURL)
}

func writeCodexConfigForBaseURL(baseURL string) (string, error) {
	secureBaseURL, err := secureTenantProxyURL(context.Background(), baseURL, "protected-codex-credential")
	if err != nil {
		return "", err
	}
	baseURL = secureBaseURL
	path, err := defaultCodexConfigPath()
	if err != nil {
		return "", err
	}
	root := codexProxyRootURL(baseURL)
	values := map[string]string{
		"openai_base_url":                   root + "/v1",
		"chatgpt_base_url":                  root + "/backend-api",
		"experimental_realtime_ws_base_url": root + "/v1",
	}
	if err := writeCodexConfigValues(path, values); err != nil {
		return "", err
	}
	return path, nil
}

func codexProxyRootURL(baseURL string) string {
	root := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	for _, suffix := range []string{"/v1", "/backend-api"} {
		if strings.HasSuffix(root, suffix) {
			root = strings.TrimSuffix(root, suffix)
			break
		}
	}
	return strings.TrimRight(root, "/")
}

func writeCodexConfigValues(path string, values map[string]string) error {
	body, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	next := updateTopLevelTomlStrings(string(body), values)
	if string(body) == next {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if len(body) > 0 {
		if err := os.WriteFile(path+".bak", body, 0o600); err != nil {
			return fmt.Errorf("write backup: %w", err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(next), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func updateTopLevelTomlStrings(text string, values map[string]string) string {
	remaining := map[string]string{}
	for key, value := range values {
		remaining[key] = value
	}
	lines := splitLinesKeepingEndings(text)
	out := make([]string, 0, len(lines)+len(values))
	inTopLevel := true
	insertedMissing := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r\n"))
		if inTopLevel && isTomlTableHeader(trimmed) {
			out = appendMissingTomlStrings(out, remaining)
			insertedMissing = true
			inTopLevel = false
		}
		if inTopLevel {
			key, ok := tomlBareAssignmentKey(line)
			if ok {
				if value, exists := remaining[key]; exists {
					out = append(out, leadingWhitespace(line)+key+" = "+strconv.Quote(value)+lineEnding(line))
					delete(remaining, key)
					continue
				}
			}
		}
		out = append(out, line)
	}
	if !insertedMissing {
		out = appendMissingTomlStrings(out, remaining)
	}
	result := strings.Join(out, "")
	if result == "" || !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return result
}

func appendMissingTomlStrings(out []string, remaining map[string]string) []string {
	if len(remaining) == 0 {
		return out
	}
	if len(out) > 0 && !strings.HasSuffix(out[len(out)-1], "\n") {
		out[len(out)-1] += "\n"
	}
	keys := make([]string, 0, len(remaining))
	for _, key := range codexRoutingConfigKeys {
		if _, ok := remaining[key]; ok {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		out = append(out, key+" = "+strconv.Quote(remaining[key])+"\n")
		delete(remaining, key)
	}
	return out
}

func splitLinesKeepingEndings(text string) []string {
	if text == "" {
		return nil
	}
	var lines []string
	start := 0
	for i, ch := range text {
		if ch == '\n' {
			lines = append(lines, text[start:i+1])
			start = i + 1
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

func isTomlTableHeader(trimmed string) bool {
	return strings.HasPrefix(trimmed, "[")
}

func tomlBareAssignmentKey(line string) (string, bool) {
	idx := strings.IndexByte(line, '=')
	if idx < 0 {
		return "", false
	}
	key := strings.TrimSpace(line[:idx])
	if key == "" {
		return "", false
	}
	for _, ch := range key {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-' {
			continue
		}
		return "", false
	}
	return key, true
}

func leadingWhitespace(line string) string {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return line[:i]
}

func lineEnding(line string) string {
	if strings.HasSuffix(line, "\r\n") {
		return "\r\n"
	}
	if strings.HasSuffix(line, "\n") {
		return "\n"
	}
	return "\n"
}

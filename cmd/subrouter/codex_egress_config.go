package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// codexEgressConfigFromEnvironment reads SUBROUTER_CODEX_EGRESS_PROXIES, a
// comma-separated list of HTTP CONNECT proxy URLs in other regions
// (for example http://cmux-egress-fra:3128,http://cmux-egress-tyo:3128).
// Unset or empty means the feature is off and the Codex path is unchanged.
func codexEgressConfigFromEnvironment(sessionPath string) (*proxy.CodexEgressConfig, error) {
	raw := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_EGRESS_PROXIES"))
	if raw == "" {
		return nil, nil
	}
	proxies, err := proxy.ParseCodexEgressProxies(raw)
	if err != nil {
		return nil, err
	}
	if len(proxies) == 0 {
		return nil, nil
	}
	return &proxy.CodexEgressConfig{
		Proxies:      proxies,
		PinStorePath: filepath.Join(filepath.Dir(sessionPath), "codex-egress-pins.json"),
	}, nil
}

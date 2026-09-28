package main

import (
	"os"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// clientNameEnv overrides the name `sr` reports in X-Subrouter-Client.
const clientNameEnv = "SUBROUTER_CLIENT_NAME"

// srClientName is the label this machine's traffic carries in the server's
// token usage accounting. It is a variable so tests get a stable value.
var srClientName = defaultSRClientName

// defaultSRClientName is SUBROUTER_CLIENT_NAME when it is a valid name, else
// the short host name ("leos-mbp" rather than "leos-mbp.local"). An empty
// result means no header is sent and the server falls back to other signals.
func defaultSRClientName() string {
	if name := proxy.NormalizeClientName(os.Getenv(clientNameEnv)); name != "" {
		return name
	}
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return shortClientHostName(host)
}

func shortClientHostName(host string) string {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	host = strings.TrimSuffix(host, ".local")
	if index := strings.IndexByte(host, '.'); index > 0 {
		host = host[:index]
	}
	return proxy.NormalizeClientName(host)
}

// clientNameHeader is the header srClientName travels in.
const clientNameHeader = proxy.ClientNameHeader

func normalizeClientName(value string) string { return proxy.NormalizeClientName(value) }

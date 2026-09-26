package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const tailscaleBinaryEnv = "SUBROUTER_TAILSCALE_BIN"

const (
	tailscaleStatusTimeout = 2 * time.Second
)

type tailscaleNodeStatus struct {
	ID           string   `json:"ID"`
	DNSName      string   `json:"DNSName"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	Online       bool     `json:"Online"`
}

type tailscaleStatusDocument struct {
	Self tailscaleNodeStatus            `json:"Self"`
	Peer map[string]tailscaleNodeStatus `json:"Peer"`
}

type tailscaleStatusLoader func(context.Context) ([]byte, error)

type tailscaleRepairFailure struct {
	serverName string
	nodeID     string
	cause      error
}

func (e tailscaleRepairFailure) Error() string {
	return fmt.Sprintf("verify Subrouter server %q through Tailscale node %s: %v", e.serverName, e.nodeID, e.cause)
}

func (e tailscaleRepairFailure) Unwrap() error { return e.cause }

func validTailscaleNodeID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == ':' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func defaultTailscaleStatusLoader(ctx context.Context) ([]byte, error) {
	statusCtx, cancel := context.WithTimeout(ctx, tailscaleStatusTimeout)
	defer cancel()
	binary, err := findTailscaleBinary()
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(statusCtx, binary, "status", "--json")
	command.Env = append(envWithout(os.Environ(), []string{"TAILSCALE_BE_CLI"}), "TAILSCALE_BE_CLI=true")
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("run %s status --json: %w", filepath.Base(binary), err)
	}
	return output, nil
}

func findTailscaleBinary() (string, error) {
	if configured := strings.TrimSpace(os.Getenv(tailscaleBinaryEnv)); configured != "" {
		info, err := os.Stat(configured)
		if err != nil {
			return "", fmt.Errorf("%s=%q: %w", tailscaleBinaryEnv, configured, err)
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("%s=%q is not executable", tailscaleBinaryEnv, configured)
		}
		return configured, nil
	}
	if binary, err := exec.LookPath("tailscale"); err == nil {
		return binary, nil
	}
	for _, candidate := range []string{
		"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
		"/Applications/Tailscale.app/Contents/MacOS/tailscale",
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("tailscale CLI not found in PATH or the standard macOS app bundle")
}

func tailscaleNodeByID(data []byte, nodeID string) (tailscaleNodeStatus, bool, error) {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return tailscaleNodeStatus{}, false, errors.New("empty Tailscale node ID")
	}
	var status tailscaleStatusDocument
	if err := json.Unmarshal(data, &status); err != nil {
		return tailscaleNodeStatus{}, false, fmt.Errorf("decode tailscale status: %w", err)
	}
	if strings.TrimSpace(status.Self.ID) == nodeID {
		return status.Self, true, nil
	}
	for _, peer := range status.Peer {
		if strings.TrimSpace(peer.ID) == nodeID {
			return peer, true, nil
		}
	}
	return tailscaleNodeStatus{}, false, nil
}

func tailscaleServerURL(serverURL string, node tailscaleNodeStatus) (string, error) {
	candidates, err := tailscaleServerURLs(serverURL, node)
	if err != nil {
		return "", err
	}
	return candidates[0], nil
}

func tailscaleServerURLs(serverURL string, node tailscaleNodeStatus) ([]string, error) {
	parsed, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil {
		// url.Error includes the original input. A malformed stored URL may
		// contain a tenant route key or userinfo, so never wrap that parser error.
		return nil, errors.New("parse server URL: stored URL is invalid (value redacted)")
	}
	if parsed.Scheme == "" || parsed.Hostname() == "" {
		return nil, fmt.Errorf("server URL %q is not absolute", redactedServerURL(serverURL))
	}
	hosts := make([]string, 0, 1+len(node.TailscaleIPs))
	if dnsName := strings.TrimSuffix(strings.TrimSpace(node.DNSName), "."); dnsName != "" {
		hosts = append(hosts, dnsName)
	}
	for _, candidate := range node.TailscaleIPs {
		if ip := net.ParseIP(strings.TrimSpace(candidate)); ip != nil {
			hosts = appendUniqueString(hosts, ip.String())
		}
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("Tailscale node %s has no DNS name or IP address", node.ID)
	}
	urls := make([]string, 0, len(hosts))
	for _, host := range hosts {
		candidate := *parsed
		if port := parsed.Port(); port != "" {
			candidate.Host = net.JoinHostPort(host, port)
		} else if strings.Contains(host, ":") {
			candidate.Host = "[" + host + "]"
		} else {
			candidate.Host = host
		}
		candidate.Path = strings.TrimRight(candidate.Path, "/")
		candidate.RawPath = strings.TrimRight(candidate.RawPath, "/")
		urls = append(urls, candidate.String())
	}
	return urls, nil
}

func appendUniqueString(values []string, candidate string) []string {
	for _, value := range values {
		if strings.EqualFold(value, candidate) {
			return values
		}
	}
	return append(values, candidate)
}

func serverURLBelongsToTailscaleNode(serverURL string, node tailscaleNodeStatus) bool {
	parsed, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if dnsName := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(node.DNSName)), "."); dnsName != "" && host == dnsName {
		return true
	}
	hostIP := net.ParseIP(host)
	if hostIP == nil {
		return false
	}
	for _, candidate := range node.TailscaleIPs {
		if ip := net.ParseIP(strings.TrimSpace(candidate)); ip != nil && ip.Equal(hostIP) {
			return true
		}
	}
	return false
}

func healTailscaleServer(
	ctx context.Context,
	store srServerStore,
	server srServerConfig,
	client *http.Client,
	warn io.Writer,
	load tailscaleStatusLoader,
) (srServerConfig, error) {
	if strings.TrimSpace(server.TailscaleNodeID) == "" || strings.TrimSpace(server.URL) == "" {
		return server, nil
	}
	if client == nil {
		client = fallbackHTTPClient()
	}
	if load == nil {
		load = defaultTailscaleStatusLoader
	}
	data, err := load(ctx)
	if err != nil {
		return tailscaleRepairError(warn, server, err)
	}
	node, ok, err := tailscaleNodeByID(data, server.TailscaleNodeID)
	if err != nil {
		return tailscaleRepairError(warn, server, err)
	}
	if !ok {
		return tailscaleRepairError(warn, server, fmt.Errorf("node ID %s not found", server.TailscaleNodeID))
	}
	storedEndpointProbed := false
	if serverURLBelongsToTailscaleNode(server.URL, node) {
		storedEndpointProbed = true
		if serverHealthy(ctx, client, server.URL) {
			return server, nil
		}
	}
	candidates, err := tailscaleServerURLs(server.URL, node)
	if err != nil {
		return tailscaleRepairError(warn, server, err)
	}
	candidate := ""
	for _, discovered := range candidates {
		// The stored URL is one of the node's advertised endpoints in this
		// branch and was already given a complete probe budget above.
		if storedEndpointProbed && sameEndpoint(discovered, server.URL) {
			continue
		}
		if serverHealthy(ctx, client, discovered) {
			candidate = discovered
			break
		}
	}
	if candidate == "" {
		return tailscaleRepairError(warn, server, errors.New("no discovered endpoint passed the Subrouter health check"))
	}
	updated, err := store.compareAndSwapServerURL(
		server.Name,
		server.TailscaleNodeID,
		server.URL,
		candidate,
	)
	if err != nil {
		return tailscaleRepairError(warn, server, err)
	}
	if warn != nil && updated.URL == candidate {
		fmt.Fprintf(warn, "Subrouter server %q moved; updated %s to %s using Tailscale node %s.\n", server.Name, redactedServerURL(server.URL), redactedServerURL(candidate), server.TailscaleNodeID)
	}
	return updated, nil
}

func (r srRunner) healRemoteServer(store srServerStore, server srServerConfig) (srServerConfig, error) {
	return r.healRemoteServerContext(context.Background(), store, server)
}

func (r srRunner) healRemoteServerContext(ctx context.Context, store srServerStore, server srServerConfig) (srServerConfig, error) {
	return healTailscaleServer(
		ctx,
		store,
		server,
		r.client,
		r.errOut,
		nil,
	)
}

func tailscaleRepairError(warn io.Writer, server srServerConfig, err error) (srServerConfig, error) {
	warnTailscaleRepair(warn, server, err)
	return srServerConfig{}, tailscaleRepairFailure{serverName: server.Name, nodeID: server.TailscaleNodeID, cause: err}
}

func warnTailscaleRepair(warn io.Writer, server srServerConfig, err error) {
	if warn != nil {
		fmt.Fprintf(warn, "warning: could not repair Subrouter server %q from Tailscale node %s: %v\n", server.Name, server.TailscaleNodeID, err)
	}
}

func redactedServerURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "<invalid URL>"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.RawFragment = ""
	parts := strings.Split(parsed.Path, "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "t" && parts[i+1] != "" {
			parts[i+1] = "[redacted]"
		}
	}
	parsed.Path = strings.Join(parts, "/")
	parsed.RawPath = ""
	return strings.ReplaceAll(parsed.Redacted(), "%5Bredacted%5D", "[redacted]")
}

func (s srServerStore) compareAndSwapServerURL(name, nodeID, oldURL, newURL string) (srServerConfig, error) {
	var updated srServerConfig
	err := s.update(func(file *srServerFile) error {
		for i := range file.Servers {
			server := &file.Servers[i]
			if server.Name != name {
				continue
			}
			if strings.TrimSpace(server.TailscaleNodeID) != strings.TrimSpace(nodeID) {
				return fmt.Errorf("server identity changed concurrently")
			}
			if server.URL != oldURL {
				if server.URL == newURL {
					updated = *server
					return nil
				}
				return fmt.Errorf("server URL changed concurrently from %s to %s", redactedServerURL(oldURL), redactedServerURL(server.URL))
			}
			server.URL = newURL
			updated = *server
			return nil
		}
		return fmt.Errorf("server %q no longer exists", name)
	})
	return updated, err
}

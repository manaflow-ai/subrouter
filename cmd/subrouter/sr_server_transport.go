package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

type serverIPLookup func(context.Context, string) ([]net.IPAddr, error)

const secureTenantTransportRequirement = "tenant-scoped server URL must use HTTPS, loopback HTTP, or verified Tailscale HTTP"

func secureTenantProxyURL(ctx context.Context, rawURL, tenantKey string) (string, error) {
	server := srServerConfig{URL: rawURL, TenantKey: tenantKey}
	return secureTenantServerURL(ctx, rawURL, server)
}

func secureTenantServerURL(ctx context.Context, rawURL string, server srServerConfig) (string, error) {
	return secureTenantServerURLWithResolvers(
		ctx,
		rawURL,
		server,
		net.DefaultResolver.LookupIPAddr,
		defaultTailscaleStatusLoader,
	)
}

func secureTenantServerURLWithResolvers(
	ctx context.Context,
	rawURL string,
	server srServerConfig,
	lookup serverIPLookup,
	load tailscaleStatusLoader,
) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("tenant-scoped server URL must be absolute")
	}
	if strings.TrimSpace(server.TenantKey) == "" && tenantKeyFromURL(parsed) == "" {
		return parsed.String(), nil
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return parsed.String(), nil
	case "http":
	default:
		return "", errors.New(secureTenantTransportRequirement)
	}

	if strings.TrimSpace(server.TailscaleNodeID) != "" && !isLoopbackServerHost(parsed.Hostname()) {
		return pinTenantURLToTailscaleNode(ctx, parsed, server, load)
	}
	return pinTenantURLToVerifiedAddresses(ctx, parsed, lookup, load)
}

func pinTenantURLToTailscaleNode(ctx context.Context, parsed *url.URL, server srServerConfig, load tailscaleStatusLoader) (string, error) {
	if load == nil {
		return "", errors.New("could not verify Tailscale for plaintext tenant routing")
	}
	data, err := load(ctx)
	if err != nil {
		return "", errors.New("could not verify Tailscale for plaintext tenant routing")
	}
	var status tailscaleStatusDocument
	if err := json.Unmarshal(data, &status); err != nil || !status.Self.Online {
		return "", errors.New("Tailscale is offline; plaintext tenant routing is disabled")
	}
	node, ok, err := tailscaleNodeByID(data, server.TailscaleNodeID)
	if err != nil || !ok || !node.Online || !serverURLBelongsToTailscaleNode(server.URL, node) {
		return "", errors.New("tenant server does not match an online Tailscale node")
	}
	addresses := make([]net.IPAddr, 0, len(node.TailscaleIPs))
	for _, rawIP := range node.TailscaleIPs {
		if ip := net.ParseIP(strings.TrimSpace(rawIP)); ip != nil && safeAccountImportHTTPIP(ip) {
			addresses = append(addresses, net.IPAddr{IP: ip})
		}
	}
	if len(addresses) == 0 {
		return "", errors.New("Tailscale node has no verified tailnet address")
	}
	return pinParsedURL(parsed, addresses[0].IP), nil
}

func pinTenantURLToVerifiedAddresses(ctx context.Context, parsed *url.URL, lookup serverIPLookup, load tailscaleStatusLoader) (string, error) {
	addresses, err := safeAddressesForTenantURL(ctx, parsed, lookup)
	if err != nil {
		return "", err
	}
	allLoopback := true
	for _, address := range addresses {
		if !address.IP.IsLoopback() {
			allLoopback = false
			break
		}
	}
	if allLoopback {
		selected := addresses[0].IP
		if len(addresses) > 1 {
			selected, err = reachableLoopbackAddress(ctx, parsed, addresses)
			if err != nil {
				return "", err
			}
		}
		return pinParsedURL(parsed, selected), nil
	}
	if load == nil {
		return "", errors.New("could not verify Tailscale for plaintext tenant routing")
	}
	data, err := load(ctx)
	if err != nil {
		return "", errors.New("could not verify Tailscale for plaintext tenant routing")
	}
	var status tailscaleStatusDocument
	if err := json.Unmarshal(data, &status); err != nil || !status.Self.Online {
		return "", errors.New("Tailscale is offline; plaintext tenant routing is disabled")
	}
	if !tenantURLAddressesBelongToOnlineTailscaleNode(parsed, addresses, status) {
		return "", errors.New("plaintext tenant destination is not an authenticated Tailscale node")
	}
	return pinParsedURL(parsed, addresses[0].IP), nil
}

func reachableLoopbackAddress(ctx context.Context, parsed *url.URL, addresses []net.IPAddr) (net.IP, error) {
	port := parsed.Port()
	if port == "" {
		port = "80"
	}
	var lastErr error
	for _, address := range addresses {
		dialCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", net.JoinHostPort(address.IP.String(), port))
		cancel()
		if err == nil {
			_ = conn.Close()
			return address.IP, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("no resolved loopback address is reachable: %w", lastErr)
}

func safeAddressesForTenantURL(ctx context.Context, parsed *url.URL, lookup serverIPLookup) ([]net.IPAddr, error) {
	host := parsed.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if !safeAccountImportHTTPIP(ip) {
			return nil, errors.New(secureTenantTransportRequirement)
		}
		return []net.IPAddr{{IP: ip}}, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addresses, err := lookup(lookupCtx, host)
	if err != nil || len(addresses) == 0 {
		return nil, errors.New(secureTenantTransportRequirement)
	}
	for _, address := range addresses {
		if !safeAccountImportHTTPIP(address.IP) {
			return nil, errors.New(secureTenantTransportRequirement)
		}
	}
	return addresses, nil
}

func tenantURLAddressesBelongToOnlineTailscaleNode(parsed *url.URL, addresses []net.IPAddr, status tailscaleStatusDocument) bool {
	nodes := make([]tailscaleNodeStatus, 0, 1+len(status.Peer))
	nodes = append(nodes, status.Self)
	for _, node := range status.Peer {
		nodes = append(nodes, node)
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	hostIP := net.ParseIP(host)
	for _, node := range nodes {
		if !node.Online {
			continue
		}
		if hostIP == nil {
			dnsName := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(node.DNSName)), ".")
			if dnsName == "" || host != dnsName {
				continue
			}
		}
		allBelong := true
		for _, address := range addresses {
			belongs := false
			for _, rawIP := range node.TailscaleIPs {
				if ip := net.ParseIP(strings.TrimSpace(rawIP)); ip != nil && ip.Equal(address.IP) {
					belongs = true
					break
				}
			}
			if !belongs {
				allBelong = false
				break
			}
		}
		if allBelong {
			return true
		}
	}
	return false
}

func pinParsedURL(parsed *url.URL, ip net.IP) string {
	pinned := ip.String()
	if port := parsed.Port(); port != "" {
		parsed.Host = net.JoinHostPort(pinned, port)
	} else if strings.Contains(pinned, ":") {
		parsed.Host = "[" + pinned + "]"
	} else {
		parsed.Host = pinned
	}
	return parsed.String()
}

func tenantKeyFromURL(parsed *url.URL) string {
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "t" && strings.TrimSpace(parts[i+1]) != "" {
			return parts[i+1]
		}
	}
	return ""
}

func validateTenantServerConfig(ctx context.Context, server srServerConfig) error {
	return validateTenantServerConfigWithLookup(ctx, server, net.DefaultResolver.LookupIPAddr)
}

func validateTenantServerConfigWithLookup(ctx context.Context, server srServerConfig, lookup serverIPLookup) error {
	parsed, err := url.Parse(strings.TrimSpace(server.URL))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("tenant-scoped server URL must be absolute")
	}
	if strings.TrimSpace(server.TenantKey) == "" && tenantKeyFromURL(parsed) == "" {
		return nil
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return nil
	}
	if !strings.EqualFold(parsed.Scheme, "http") {
		return errors.New(secureTenantTransportRequirement)
	}
	if strings.TrimSpace(server.TailscaleNodeID) != "" {
		if ip := net.ParseIP(parsed.Hostname()); ip != nil && !safeAccountImportHTTPIP(ip) {
			return errors.New(secureTenantTransportRequirement)
		}
		return nil
	}
	_, err = safeAddressesForTenantURL(ctx, parsed, lookup)
	return err
}

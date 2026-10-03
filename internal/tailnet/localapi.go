package tailnet

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
	"regexp"
	"runtime"
	"sync"
)

// errNotTailnetPeer is the daemon's definitive "no such peer" answer. It is not
// a malfunction and is never logged as one.
var errNotTailnetPeer = errors.New("not a tailnet peer")

// localAPIEndpoint is where this machine's tailscaled serves its LocalAPI.
type localAPIEndpoint struct {
	network string
	address string
	token   string
}

// localAPIWhois asks tailscaled directly, without the tailscale CLI.
//
// On macOS the CLI inside Tailscale.app cannot run from a launchd job: it
// tries to reach the GUI app and fails with "The Tailscale GUI failed to
// start", and the app's group container, where the LocalAPI credentials live,
// is closed to background processes by app-data protection. The sandboxed
// network extension keeps that credential file open, so its name, which
// carries the port and token, can be read from lsof instead. That is the same
// discovery the tailscale CLI performs.
type localAPIWhois struct {
	runner commandRunner

	mu       sync.Mutex
	endpoint *localAPIEndpoint
}

const linuxTailscaledSocket = "/var/run/tailscale/tailscaled.sock"

var sameUserProofPattern = regexp.MustCompile(`sameuserproof-(\d+)-([0-9a-fA-F]+)`)

func (l *localAPIWhois) whois(ctx context.Context, host string) ([]byte, error) {
	endpoint, err := l.currentEndpoint(ctx)
	if err != nil {
		return nil, err
	}
	body, err := whoisAt(ctx, endpoint, host)
	if err == nil || errors.Is(err, errNotTailnetPeer) {
		return body, err
	}
	if ctx.Err() != nil {
		// A timed-out lookup says nothing about the endpoint; keep it.
		return nil, err
	}
	// Tailscale restarts on a new port with a new token; rediscover once.
	l.forget()
	endpoint, discoverErr := l.currentEndpoint(ctx)
	if discoverErr != nil {
		return nil, fmt.Errorf("%w; rediscovery: %v", err, discoverErr)
	}
	return whoisAt(ctx, endpoint, host)
}

func (l *localAPIWhois) currentEndpoint(ctx context.Context) (localAPIEndpoint, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.endpoint != nil {
		return *l.endpoint, nil
	}
	endpoint, err := discoverLocalAPI(ctx, l.runner)
	if err != nil {
		return localAPIEndpoint{}, err
	}
	l.endpoint = &endpoint
	return endpoint, nil
}

func (l *localAPIWhois) forget() {
	l.mu.Lock()
	l.endpoint = nil
	l.mu.Unlock()
}

func discoverLocalAPI(ctx context.Context, runner commandRunner) (localAPIEndpoint, error) {
	switch runtime.GOOS {
	case "darwin":
		// -F n prints only file names, one per line, prefixed with "n".
		out, err := runner.Output(ctx, "/usr/sbin/lsof", "-n", "-a", "-c", "IPNExtension", "-F", "n")
		if endpoint, ok := parseSameUserProof(out); ok {
			return endpoint, nil
		}
		if err != nil {
			return localAPIEndpoint{}, fmt.Errorf("find Tailscale LocalAPI with lsof: %w", err)
		}
		return localAPIEndpoint{}, errors.New("find Tailscale LocalAPI: no running Tailscale network extension holds a LocalAPI credential")
	case "linux":
		if _, err := os.Stat(linuxTailscaledSocket); err != nil {
			return localAPIEndpoint{}, fmt.Errorf("find Tailscale LocalAPI: %w", err)
		}
		return localAPIEndpoint{network: "unix", address: linuxTailscaledSocket}, nil
	default:
		return localAPIEndpoint{}, fmt.Errorf("find Tailscale LocalAPI: unsupported on %s", runtime.GOOS)
	}
}

func parseSameUserProof(lsofOutput []byte) (localAPIEndpoint, bool) {
	match := sameUserProofPattern.FindSubmatch(lsofOutput)
	if match == nil {
		return localAPIEndpoint{}, false
	}
	return localAPIEndpoint{
		network: "tcp",
		address: net.JoinHostPort("127.0.0.1", string(match[1])),
		token:   string(match[2]),
	}, true
}

func whoisAt(ctx context.Context, endpoint localAPIEndpoint, host string) ([]byte, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, endpoint.network, endpoint.address)
		},
		DisableKeepAlives: true,
	}}
	// whois takes ip:port; any port works for an address lookup.
	target := "http://local-tailscaled.sock/localapi/v0/whois?addr=" + url.QueryEscape(net.JoinHostPort(host, "1"))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Sec-Tailscale", "localapi")
	if endpoint.token != "" {
		request.SetBasicAuth("", endpoint.token)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("Tailscale LocalAPI whois: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("Tailscale LocalAPI whois: %w", err)
	}
	switch response.StatusCode {
	case http.StatusOK:
		if !json.Valid(body) {
			return nil, errors.New("Tailscale LocalAPI whois: invalid JSON")
		}
		return body, nil
	case http.StatusNotFound:
		return nil, errNotTailnetPeer
	default:
		return nil, fmt.Errorf("Tailscale LocalAPI whois: HTTP %d", response.StatusCode)
	}
}

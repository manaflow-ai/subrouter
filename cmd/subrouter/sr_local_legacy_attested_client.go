package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// newLegacyLocalStoreAttestedClient returns a direct loopback HTTP client whose
// transport proves store ownership on every newly opened connection before
// returning that connection to net/http. This closes the rebind window between
// a one-time health check and a later credential-bearing request.
//
// The proof is intentionally scoped to the local deployment topology: an
// unrelated listener cannot reach the supervisor's 0700/0600 worker socket,
// while a same-UID process that can reach it can already read the private store
// key and credentials. It is not cryptographic channel binding for a topology
// that exposes a second live attestation oracle to an untrusted relay.
func newLegacyLocalStoreAttestedClient(base *http.Client, rawURL string, store accounts.CodexStore) (*http.Client, error) {
	return newLegacyLocalStoreAttestedClientWithResolver(base, rawURL, func() (accounts.CodexStore, error) {
		return store, nil
	})
}

func newLegacyLocalStoreAttestedClientWithResolver(
	base *http.Client,
	rawURL string,
	resolveStore func() (accounts.CodexStore, error),
) (*http.Client, error) {
	if base == nil {
		base = &http.Client{}
	}
	if resolveStore == nil {
		return nil, errors.New("local store attestation requires a store resolver")
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("parse local proxy URL: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "http") || !isLoopbackServerHost(parsed.Hostname()) {
		return nil, errors.New("local store attestation requires a loopback HTTP endpoint")
	}
	port := parsed.Port()
	if port == "" {
		port = "80"
	}
	host := parsed.Hostname()
	hostHeader := parsed.Host

	client := *base
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	var transport *http.Transport
	switch configured := client.Transport.(type) {
	case nil:
		transport = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		transport = configured.Clone()
	default:
		return nil, errors.New("local store attestation requires a configurable HTTP transport")
	}
	transport.Proxy = nil
	transport.ForceAttemptHTTP2 = false
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		requestedHost, requestedPort, splitErr := net.SplitHostPort(address)
		if splitErr != nil || requestedPort != port ||
			!strings.EqualFold(strings.TrimSuffix(requestedHost, "."), strings.TrimSuffix(host, ".")) {
			return nil, errors.New("local store attestation refused an unexpected destination")
		}
		connection, dialErr := dialPinnedLoopback(ctx, network, host, port)
		if dialErr != nil {
			return nil, dialErr
		}
		store, resolveErr := resolveStore()
		if resolveErr != nil {
			_ = connection.Close()
			return nil, resolveErr
		}
		attested, attestErr := attestLocalStoreConnection(ctx, connection, hostHeader, store)
		if attestErr != nil {
			_ = connection.Close()
			return nil, attestErr
		}
		return attested, nil
	}
	client.Transport = transport
	return &client, nil
}

func dialPinnedLoopback(ctx context.Context, network, host, port string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: localStoreAttestationTimeout, KeepAlive: 30 * time.Second}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return nil, errors.New("local store attestation refused a non-loopback destination")
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	}
	if !strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return nil, errors.New("local store attestation refused an unpinned hostname")
	}
	var lastErr error
	for _, loopback := range []string{"127.0.0.1", "::1"} {
		connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(loopback, port))
		if err == nil {
			return connection, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func attestLocalStoreConnection(ctx context.Context, connection net.Conn, hostHeader string, store accounts.CodexStore) (net.Conn, error) {
	deadline := time.Now().Add(localStoreAttestationTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set local store-attestation deadline: %w", err)
	}

	var challengeBytes [32]byte
	if _, err := rand.Read(challengeBytes[:]); err != nil {
		return nil, fmt.Errorf("create local proxy store challenge: %w", err)
	}
	challenge := hex.EncodeToString(challengeBytes[:])
	request := &http.Request{
		Method:     http.MethodGet,
		URL:        &url.URL{Path: "/_subrouter/health"},
		Host:       hostHeader,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set(accounts.StoreAuthorityChallengeHeader, challenge)
	if err := request.Write(connection); err != nil {
		return nil, fmt.Errorf("write local proxy store attestation: %w", err)
	}

	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, fmt.Errorf("read local proxy store attestation: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, localStoreAttestationBodyLimit+1))
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read local proxy store-attestation body: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close local proxy store-attestation body: %w", closeErr)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("local proxy store attestation failed: %s", response.Status)
	}
	if response.ProtoMajor != 1 || response.ProtoMinor != 1 || response.Close {
		return nil, errors.New("local proxy store attestation did not preserve its connection")
	}
	if len(body) > localStoreAttestationBodyLimit {
		return nil, errors.New("local proxy store-attestation response is too large")
	}
	if reader.Buffered() != 0 {
		return nil, errors.New("local proxy store attestation returned unexpected trailing data")
	}
	var payload struct {
		AccountStoreID    string `json:"account_store_id"`
		AccountStoreProof string `json:"account_store_proof"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode local proxy store attestation: %w", err)
	}
	expectedID, err := accounts.StoreAuthorityID(store.Dir)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(payload.AccountStoreID) == "" || payload.AccountStoreID != expectedID {
		return nil, errors.New("local proxy account store does not match this CLI")
	}
	expectedProof, err := accounts.ExistingStoreAuthorityProof(store.Dir, challenge)
	if err != nil {
		return nil, err
	}
	if !hmac.Equal([]byte(strings.TrimSpace(payload.AccountStoreProof)), []byte(expectedProof)) {
		return nil, errors.New("local proxy account store does not match this CLI")
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear local store-attestation deadline: %w", err)
	}
	return &bufferedLegacyAttestedConn{Conn: connection, reader: reader}, nil
}

type bufferedLegacyAttestedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedLegacyAttestedConn) Read(buffer []byte) (int, error) {
	return c.reader.Read(buffer)
}

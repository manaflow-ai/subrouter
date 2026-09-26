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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

const (
	localStoreAttestationBodyLimit = 16 << 10
	localStoreAttestationTimeout   = 15 * time.Second
)

// newLocalDataClient returns an HTTP client whose connections use the private
// Unix data socket published with the selected serving store. Credential-
// bearing traffic never crosses the public loopback/Tailscale listener.
func newLocalDataClient(base *http.Client, rawURL string, store accounts.CodexStore) (*http.Client, error) {
	return newLocalDataClientWithResolver(base, rawURL, func() (accounts.CodexStore, error) {
		return store, nil
	})
}

func newLocalDataClientWithResolver(
	base *http.Client,
	rawURL string,
	resolveStore func() (accounts.CodexStore, error),
) (*http.Client, error) {
	return newLocalDataClientWithStoreResolvers(base, rawURL, resolveStore, resolveStore)
}

func newLocalDataClientWithStoreResolvers(
	base *http.Client,
	rawURL string,
	resolveBindingStore func() (accounts.CodexStore, error),
	resolveServingStore func() (accounts.CodexStore, error),
) (*http.Client, error) {
	if base == nil {
		base = &http.Client{}
	}
	if resolveBindingStore == nil || resolveServingStore == nil {
		return nil, errors.New("local data client requires binding and serving store resolvers")
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("parse local proxy URL: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "http") || !isLoopbackServerHost(parsed.Hostname()) {
		return nil, errors.New("local data client requires a loopback HTTP endpoint")
	}

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
		return nil, errors.New("local data client requires a configurable HTTP transport")
	}
	transport.Proxy = nil
	transport.ForceAttemptHTTP2 = false
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		bindingStore, resolveErr := resolveBindingStore()
		if resolveErr != nil {
			return nil, resolveErr
		}
		socket, socketErr := localDataSocketForStore(bindingStore)
		if socketErr != nil {
			return nil, socketErr
		}
		servingStore, resolveErr := resolveServingStore()
		if resolveErr != nil {
			return nil, resolveErr
		}
		connection, dialErr := dialValidatedLocalDataSocket(ctx, socket)
		if dialErr != nil {
			return nil, dialErr
		}
		attested, attestErr := attestLocalDataConnection(ctx, connection, parsed.Host, servingStore)
		if attestErr != nil {
			_ = connection.Close()
			return nil, attestErr
		}
		return attested, nil
	}
	client.Transport = transport
	return &client, nil
}

func localDataSocketForStore(store accounts.CodexStore) (string, error) {
	if override := strings.TrimSpace(os.Getenv("SUBROUTER_LOCAL_DATA_SOCKET")); override != "" {
		return validatePrivateLocalDataSocket(override)
	}
	if stateDir := strings.TrimSpace(os.Getenv("SUBROUTER_STATE_DIR")); stateDir != "" {
		// The macOS supervisor's stable default is state-local. Deployments that
		// override it must export SUBROUTER_LOCAL_DATA_SOCKET to explicit-state
		// clients because those clients intentionally ignore the default store's
		// binding for account authority.
		return validatePrivateLocalDataSocket(filepath.Join(stateDir, "local-data.sock"))
	}
	binding, found, err := readLocalServingStoreBinding(store)
	if err != nil {
		return "", err
	}
	if !found || binding.Schema != localServingStoreSchema || strings.TrimSpace(binding.LocalDataSocket) == "" {
		return "", errors.New("local credential-bearing request requires a published private local data socket")
	}
	return validatePrivateLocalDataSocket(binding.LocalDataSocket)
}

func newPrivateLocalDataClient(base *http.Client, socket string, store accounts.CodexStore) (*http.Client, error) {
	socket, err := validatePrivateLocalDataSocket(socket)
	if err != nil {
		return nil, err
	}
	if base == nil {
		base = &http.Client{}
	}
	client := *base
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ForceAttemptHTTP2 = false
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		connection, dialErr := dialValidatedLocalDataSocket(ctx, socket)
		if dialErr != nil {
			return nil, dialErr
		}
		attested, attestErr := attestLocalDataConnection(ctx, connection, "localhost", store)
		if attestErr != nil {
			_ = connection.Close()
			return nil, attestErr
		}
		return attested, nil
	}
	client.Transport = transport
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &client, nil
}

func attestLocalDataConnection(ctx context.Context, connection net.Conn, hostHeader string, store accounts.CodexStore) (net.Conn, error) {
	deadline := time.Now().Add(localStoreAttestationTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set local data handshake deadline: %w", err)
	}
	var nonceBytes [32]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		return nil, fmt.Errorf("create local data handshake nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes[:])
	requestProof, err := accounts.ExistingStoreHandshakeRequestProof(store.Dir, nonce)
	if err != nil {
		return nil, err
	}
	request := &http.Request{
		Method: http.MethodPost,
		URL:    &url.URL{Path: proxy.StoreHandshakePath},
		Host:   hostHeader,
		Proto:  "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: make(http.Header),
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set(accounts.StoreHandshakeNonceHeader, nonce)
	request.Header.Set(accounts.StoreHandshakeRequestHeader, requestProof)
	if err := request.Write(connection); err != nil {
		return nil, fmt.Errorf("write local data handshake: %w", err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, fmt.Errorf("read local data handshake: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, localStoreAttestationBodyLimit+1))
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read local data handshake body: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close local data handshake body: %w", closeErr)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("local data handshake failed: %s", response.Status)
	}
	if response.ProtoMajor != 1 || response.ProtoMinor != 1 || response.Close || len(body) > localStoreAttestationBodyLimit || reader.Buffered() != 0 {
		return nil, errors.New("local data handshake did not preserve a clean HTTP/1.1 connection")
	}
	var payload struct {
		AccountStoreID    string `json:"account_store_id"`
		AccountStoreProof string `json:"account_store_proof"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode local data handshake: %w", err)
	}
	expectedID, err := accounts.StoreAuthorityID(store.Dir)
	if err != nil {
		return nil, err
	}
	expectedProof, err := accounts.ExistingStoreHandshakeResponseProof(store.Dir, nonce)
	if err != nil {
		return nil, err
	}
	if payload.AccountStoreID != expectedID || !hmac.Equal([]byte(strings.TrimSpace(payload.AccountStoreProof)), []byte(expectedProof)) {
		return nil, errors.New("local data socket account store does not match this CLI")
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear local data handshake deadline: %w", err)
	}
	return &bufferedLocalDataConn{Conn: connection, reader: reader}, nil
}

type bufferedLocalDataConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedLocalDataConn) Read(buffer []byte) (int, error) { return c.reader.Read(buffer) }

func dialValidatedLocalDataSocket(ctx context.Context, socket string) (net.Conn, error) {
	validated, err := validatePrivateLocalDataSocket(socket)
	if err != nil {
		return nil, err
	}
	before, err := os.Lstat(validated)
	if err != nil {
		return nil, err
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", validated)
	if err != nil {
		return nil, err
	}
	if _, err := validatePrivateLocalDataSocket(validated); err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("validate local data socket after connect: %w", err)
	}
	after, err := os.Lstat(validated)
	if err != nil || !os.SameFile(before, after) {
		_ = connection.Close()
		return nil, errors.New("local data socket identity changed during connect")
	}
	return connection, nil
}

func (r srRunner) serverRequestClient(server srServerConfig, timeout time.Duration) *http.Client {
	if server.requestClient != nil {
		return server.requestClient
	}
	if r.client != nil {
		return r.client
	}
	return &http.Client{Timeout: timeout}
}

func (r srRunner) securedRequestClientForServer(server srServerConfig, rawURL string, timeout time.Duration) (*http.Client, error) {
	if server.requestClient != nil {
		return server.requestClient, nil
	}
	return securedServerRequestClient(r.serverRequestClient(server, timeout), rawURL)
}

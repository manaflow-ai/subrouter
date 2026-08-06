package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	frontproxy "github.com/manaflow-ai/subrouter/internal/front"
)

type frontListenerAddressOverride struct {
	net.Listener
	address net.Addr
}

func (l *frontListenerAddressOverride) Addr() net.Addr {
	return l.address
}

func TestFrontConfigRequiresDistinctAbsoluteListenerTransferSocket(t *testing.T) {
	config, err := parseFrontConfig([]string{
		"--backend-id", "slot-a",
		"--backend-address", "127.0.0.1:31417",
		"--listener-transfer-socket", "/run/subrouter/front-listener.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateFrontConfig(config); err != nil {
		t.Fatalf("valid transfer socket config failed: %v", err)
	}
	config.ListenerTransferSocket = config.ControlSocket
	if err := validateFrontConfig(config); err == nil {
		t.Fatal("shared control and listener transfer socket was accepted")
	}
}

func TestFrontListenerReplacementAcceptsExplicitFreshBind(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/_subrouter/replace-listener",
		bytes.NewBufferString(`{"address":"127.0.0.1:31416"}`))
	replacement, err := decodeFrontListenerReplacement(response, request)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Address != "127.0.0.1:31416" {
		t.Fatalf("replacement = %+v", replacement)
	}
}

func TestListenerAddressMatchRequiresCompatibleWildcard(t *testing.T) {
	tests := []struct {
		actual   string
		expected string
		want     bool
	}{
		{actual: "0.0.0.0:31415", expected: "0.0.0.0:31415", want: true},
		{actual: "127.0.0.1:31415", expected: "0.0.0.0:31415", want: false},
		{actual: "[::]:31415", expected: "0.0.0.0:31415", want: false},
		{actual: "[::]:31415", expected: "[::]:31415", want: true},
		{actual: "127.0.0.1:31415", expected: "127.0.0.1:31415", want: true},
		{actual: "127.0.0.1:31416", expected: "127.0.0.1:31415", want: false},
	}
	for _, test := range tests {
		t.Run(test.actual+"_for_"+test.expected, func(t *testing.T) {
			actual := &net.TCPAddr{}
			parsed, err := net.ResolveTCPAddr("tcp", test.actual)
			if err != nil {
				t.Fatal(err)
			}
			*actual = *parsed
			if got := listenerAddressMatches(actual, test.expected); got != test.want {
				t.Fatalf("listenerAddressMatches(%q, %q) = %t, want %t", test.actual, test.expected, got, test.want)
			}
		})
	}
}

func TestFrontSelectsConfiguredInheritedListener(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	second, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	selected, err := selectFrontPublicListener(second.Addr().String(), []net.Listener{first, second})
	if err != nil {
		t.Fatal(err)
	}
	defer selected.Close()
	if selected != second {
		t.Fatalf("selected listener = %v, want %v", selected.Addr(), second.Addr())
	}
	if connection, err := net.DialTimeout("tcp", first.Addr().String(), 50*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("unselected inherited listener remained open")
	}
}

func TestFrontFallsBackWhenInheritedListenerDoesNotMatchConfiguration(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	inherited, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		inherited.Close()
		t.Fatal(err)
	}
	opened := false
	selected, err := selectOrOpenFrontPublicListener("127.0.0.1:1", []net.Listener{inherited}, func(address string) (net.Listener, error) {
		opened = true
		if address != "127.0.0.1:1" {
			t.Fatalf("fallback address = %q", address)
		}
		return fallback, nil
	})
	if err != nil {
		fallback.Close()
		t.Fatal(err)
	}
	defer selected.Close()
	if !opened || selected != fallback {
		t.Fatalf("fallback selected = %v, opened = %t", selected, opened)
	}
	if connection, err := net.DialTimeout("tcp", inherited.Addr().String(), 50*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("mismatched inherited listener remained open")
	}
}

func TestFreshIPv4WildcardListenerDoesNotBecomeDualStack(t *testing.T) {
	listener, err := openFreshPublicListener("0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || address.IP.To4() == nil {
		t.Fatalf("fresh IPv4 wildcard listener address = %v, want IPv4", listener.Addr())
	}
}

func TestStableFrontRetirementWaitsForPinnedConnection(t *testing.T) {
	backendListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backendListener.Close()
	backendAccepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := backendListener.Accept()
		if acceptErr == nil {
			backendAccepted <- connection
		}
	}()

	router, err := frontproxy.NewRouter(frontproxy.Backend{
		ID: "old", Network: "tcp", Address: backendListener.Addr().String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &stableFront{router: router, drainLogInterval: 20 * time.Millisecond}
	publicListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	controlDir, err := os.MkdirTemp("/tmp", "subrouter-front-drain-")
	if err != nil {
		publicListener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(controlDir) })
	controlListener, err := net.Listen("unix", filepath.Join(controlDir, "front.sock"))
	if err != nil {
		publicListener.Close()
		t.Fatal(err)
	}
	signals := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() {
		done <- service.runOnListeners(publicListener, controlListener, signals)
	}()

	client, err := net.DialTimeout("tcp", publicListener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var backend net.Conn
	select {
	case backend = <-backendAccepted:
	case <-time.After(2 * time.Second):
		client.Close()
		t.Fatal("front did not pin the client to its backend")
	}
	defer backend.Close()

	signals <- os.Interrupt
	select {
	case err := <-done:
		client.Close()
		t.Fatalf("front exited while a pinned connection was live: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	_ = client.Close()
	_ = backend.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("front drain failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("front did not exit after its final pinned connection closed")
	}
}

func TestStableFrontReplacesListenerWithoutRestarting(t *testing.T) {
	backendListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backendListener.Close()
	backendAccepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := backendListener.Accept()
		if acceptErr == nil {
			backendAccepted <- connection
		}
	}()
	router, err := frontproxy.NewRouter(frontproxy.Backend{
		ID: "slot-a", Network: "tcp", Address: backendListener.Addr().String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var descriptorStoreEvents []string
	service := &stableFront{
		router: router, drainLogInterval: time.Second,
		storeListener: func(listener net.Listener) error {
			descriptorStoreEvents = append(descriptorStoreEvents, "store:"+listener.Addr().String())
			return nil
		},
		removeStoredListener: func(address net.Addr) error {
			descriptorStoreEvents = append(descriptorStoreEvents, "remove:"+address.String())
			return nil
		},
	}
	oldListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	oldAddress := oldListener.Addr().String()
	controlDir, err := os.MkdirTemp("/tmp", "subrouter-front-listener-replace-")
	if err != nil {
		oldListener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(controlDir) })
	controlListener, err := net.Listen("unix", filepath.Join(controlDir, "front.sock"))
	if err != nil {
		oldListener.Close()
		t.Fatal(err)
	}
	signals := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() { done <- service.runOnListeners(oldListener, controlListener, signals) }()
	for deadline := time.Now().Add(time.Second); ; {
		service.listenerMu.Lock()
		ready := service.listenerResults != nil
		service.listenerMu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("front did not start its initial listener")
		}
		time.Sleep(time.Millisecond)
	}

	nextListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	nextAddress := nextListener.Addr().String()
	if err := service.replacePublicListener(nextListener); err != nil {
		t.Fatal(err)
	}
	wantDescriptorStoreEvents := []string{"store:" + nextAddress, "remove:" + oldAddress}
	if fmt.Sprint(descriptorStoreEvents) != fmt.Sprint(wantDescriptorStoreEvents) {
		t.Fatalf("descriptor store events = %v, want %v", descriptorStoreEvents, wantDescriptorStoreEvents)
	}
	if connection, err := net.DialTimeout("tcp", oldAddress, 50*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("retired front listener still accepted new connections")
	}
	client, err := net.DialTimeout("tcp", nextAddress, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var backend net.Conn
	select {
	case backend = <-backendAccepted:
	case <-time.After(time.Second):
		client.Close()
		t.Fatal("replacement listener did not route a connection")
	}
	statusResponse := httptest.NewRecorder()
	service.controlHandler().ServeHTTP(statusResponse,
		httptest.NewRequest(http.MethodGet, "/_subrouter/front-status", nil))
	var status struct {
		Listener *frontListenerStatus `json:"listener"`
	}
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Listener == nil || status.Listener.Address != nextAddress || status.Listener.AcceptedConnections != 1 {
		t.Fatalf("replacement listener status = %+v, want address %s with one accepted connection", status.Listener, nextAddress)
	}
	retryUnderlying, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	retryListener := &frontListenerAddressOverride{Listener: retryUnderlying, address: nextListener.Addr()}
	if err := service.replacePublicListener(retryListener); err != nil {
		retryUnderlying.Close()
		t.Fatal(err)
	}
	wantDescriptorStoreEvents = append(wantDescriptorStoreEvents, "store:"+nextAddress)
	if fmt.Sprint(descriptorStoreEvents) != fmt.Sprint(wantDescriptorStoreEvents) {
		t.Fatalf("same-address retry descriptor store events = %v, want %v", descriptorStoreEvents, wantDescriptorStoreEvents)
	}
	_ = client.Close()
	_ = backend.Close()
	signals <- os.Interrupt
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("front did not stop after replacement listener drained")
	}
}

func TestFrontListenerCompletionDoesNotBlockOnFullResultQueue(t *testing.T) {
	backendListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backendListener.Close()
	router, err := frontproxy.NewRouter(frontproxy.Backend{
		ID: "slot-a", Network: "tcp", Address: backendListener.Addr().String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &stableFront{
		router: router, listenerResults: make(chan frontListenerResult, 1), listenerStop: make(chan struct{}),
	}
	for index := 0; index < 4; index++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		tracked := &trackedFrontListener{Listener: listener}
		service.startServingLocked(tracked)
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
	}
	waited := make(chan struct{})
	go func() {
		service.listenerWG.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(time.Second):
		service.stopListenerNotifications()
		t.Fatal("listener completion blocked behind the full result queue")
	}
	service.stopListenerNotifications()
}

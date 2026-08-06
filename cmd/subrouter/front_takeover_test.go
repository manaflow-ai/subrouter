package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	config.ListenerTransferSocket = "/run/subrouter/front-listener.sock"
	config.Addr = "localhost:31415"
	if err := validateFrontConfig(config); err == nil {
		t.Fatal("hostname front address was accepted")
	}
}

func TestFrontHotReloadRequiresProcessManagerHandoff(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := promoteFrontSuccessor(4242); err == nil {
		t.Fatal("front hot reload succeeded without a process manager handoff")
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

func TestFrontDescriptorStoreSlotIncludesAddressAndFamily(t *testing.T) {
	loopback := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 31415}
	if !frontListenersShareDescriptorStoreSlot(loopback, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 31415}) {
		t.Fatal("identical IPv4 listener addresses did not share a descriptor-store slot")
	}
	if frontListenersShareDescriptorStoreSlot(loopback, &net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 31415}) {
		t.Fatal("different IPv4 listener addresses shared a descriptor-store slot")
	}
	if frontListenersShareDescriptorStoreSlot(
		&net.TCPAddr{IP: net.IPv4zero, Port: 31415},
		&net.TCPAddr{IP: net.IPv6zero, Port: 31415},
	) {
		t.Fatal("IPv4 and IPv6 wildcard listeners shared a descriptor-store slot")
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
	selected, wasInherited, err := selectOrOpenFrontPublicListener(second.Addr().String(), []net.Listener{first, second}, func(string) (net.Listener, error) {
		t.Fatal("matching inherited listener fell back to a fresh bind")
		return nil, errors.New("unreachable")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer selected.Close()
	if !wasInherited || selected != second {
		t.Fatalf("selected listener = %v, inherited = %t, want inherited %v", selected.Addr(), wasInherited, second.Addr())
	}
	if connection, err := net.DialTimeout("tcp", first.Addr().String(), 50*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("unselected inherited listener remained open")
	}
}

func TestFrontUsesSoleInheritedListenerAsDurableActiveAddress(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	inherited, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	opened := false
	selected, wasInherited, err := selectOrOpenFrontPublicListener("127.0.0.1:1", []net.Listener{inherited}, func(address string) (net.Listener, error) {
		opened = true
		return nil, fmt.Errorf("unexpected fresh bind at %q", address)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer selected.Close()
	if opened || !wasInherited || selected != inherited {
		t.Fatalf("selected = %v, opened = %t, inherited = %t, want sole inherited listener", selected, opened, wasInherited)
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

func TestStableFrontStopClosesListenerBeforeDrainingPinnedConnection(t *testing.T) {
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
		done <- service.runOnListeners(frontConfig{}, publicListener, controlListener, signals)
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
	if secondClient, err := net.DialTimeout("tcp", publicListener.Addr().String(), 100*time.Millisecond); err == nil {
		_ = secondClient.Close()
		client.Close()
		backend.Close()
		t.Fatal("stopping front still admitted a new connection into its drain set")
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

type fakeFrontSuccessor struct {
	listener   net.Listener
	router     *frontproxy.Router
	activated  chan struct{}
	retired    chan struct{}
	confirmErr error
}

func (s *fakeFrontSuccessor) PID() int {
	return 4242
}

func (s *fakeFrontSuccessor) Commit(time.Duration) error {
	return nil
}

func (s *fakeFrontSuccessor) Activate(time.Duration) error {
	go func() { _ = s.router.Serve(s.listener) }()
	close(s.activated)
	return nil
}

func (s *fakeFrontSuccessor) Confirm() (bool, error) {
	return s.confirmErr == nil, s.confirmErr
}

func (s *fakeFrontSuccessor) Abort() {
	_ = s.listener.Close()
}

func (s *fakeFrontSuccessor) Retire() {
	_ = s.listener.Close()
	if s.retired != nil {
		close(s.retired)
	}
}

func TestStableFrontHotReloadPromotesSuccessorBeforeOldConnectionDrains(t *testing.T) {
	backendListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backendListener.Close()
	backendAccepted := make(chan net.Conn, 2)
	go func() {
		for range 2 {
			connection, acceptErr := backendListener.Accept()
			if acceptErr != nil {
				return
			}
			backendAccepted <- connection
		}
	}()
	backend := frontproxy.Backend{
		ID: "slot-a", Network: "tcp", Address: backendListener.Addr().String(),
	}
	router, err := frontproxy.NewRouter(backend)
	if err != nil {
		t.Fatal(err)
	}
	publicListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	controlDir, err := os.MkdirTemp("/tmp", "subrouter-front-handoff-")
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
	activated := make(chan struct{})
	promoted := make(chan int, 1)
	service := &stableFront{
		router: router, readyTimeout: time.Second, drainLogInterval: 20 * time.Millisecond,
		startSuccessor: func(config frontConfig, public, _, _ net.Listener) (frontSuccessor, error) {
			if config.Addr != public.Addr().String() {
				return nil, fmt.Errorf("successor address = %q, want active listener %q", config.Addr, public.Addr())
			}
			file, err := duplicateFrontListenerFile(public, "front-test-successor")
			if err != nil {
				return nil, err
			}
			defer file.Close()
			successorListener, err := net.FileListener(file)
			if err != nil {
				return nil, err
			}
			successorRouter, err := frontproxy.NewRouter(backend)
			if err != nil {
				successorListener.Close()
				return nil, err
			}
			return &fakeFrontSuccessor{
				listener: successorListener, router: successorRouter, activated: activated,
			}, nil
		},
		promoteSuccessor: func(pid int) error {
			promoted <- pid
			return nil
		},
	}
	signals := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() {
		done <- service.runOnListeners(frontConfig{}, publicListener, controlListener, signals)
	}()

	oldClient, err := net.DialTimeout("tcp", publicListener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	oldBackend := <-backendAccepted
	signals <- syscall.SIGHUP
	select {
	case pid := <-promoted:
		if pid != 4242 {
			t.Fatalf("promoted pid = %d, want 4242", pid)
		}
	case <-time.After(time.Second):
		t.Fatal("front did not promote its prepared successor")
	}
	select {
	case <-activated:
	case <-time.After(time.Second):
		t.Fatal("front did not activate its promoted successor")
	}
	newClient, err := net.DialTimeout("tcp", publicListener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("new connection failed during old-session drain: %v", err)
	}
	var newBackend net.Conn
	select {
	case newBackend = <-backendAccepted:
	case <-time.After(time.Second):
		t.Fatal("successor did not route a new connection")
	}
	_ = oldClient.Close()
	_ = oldBackend.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("old front drain failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old front remained blocked by the successor's connection")
	}
	_ = newClient.Close()
	_ = newBackend.Close()
}

func TestStableFrontFailedConfirmationKeepsParentServing(t *testing.T) {
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
	backend := frontproxy.Backend{
		ID: "slot-a", Network: "tcp", Address: backendListener.Addr().String(),
	}
	router, err := frontproxy.NewRouter(backend)
	if err != nil {
		t.Fatal(err)
	}
	publicListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	controlDir, err := os.MkdirTemp("/tmp", "subrouter-front-handoff-rollback-")
	if err != nil {
		publicListener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(controlDir) })
	controlPath := filepath.Join(controlDir, "front.sock")
	controlListener, err := net.Listen("unix", controlPath)
	if err != nil {
		publicListener.Close()
		t.Fatal(err)
	}
	retired := make(chan struct{})
	promotions := 0
	service := &stableFront{
		router: router, readyTimeout: time.Second, drainLogInterval: 20 * time.Millisecond,
		startSuccessor: func(_ frontConfig, public, _, _ net.Listener) (frontSuccessor, error) {
			file, err := duplicateFrontListenerFile(public, "front-test-failed-successor")
			if err != nil {
				return nil, err
			}
			defer file.Close()
			successorListener, err := net.FileListener(file)
			if err != nil {
				return nil, err
			}
			successorRouter, err := frontproxy.NewRouter(backend)
			if err != nil {
				successorListener.Close()
				return nil, err
			}
			return &fakeFrontSuccessor{
				listener: successorListener, router: successorRouter,
				activated: make(chan struct{}), retired: retired,
				confirmErr: errors.New("injected successor exit before serving"),
			}, nil
		},
		promoteSuccessor: func(int) error {
			promotions++
			return nil
		},
	}
	signals := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() {
		done <- service.runOnListeners(frontConfig{}, publicListener, controlListener, signals)
	}()
	waitForFrontListenerReady(t, service)
	signals <- syscall.SIGHUP
	select {
	case <-retired:
	case <-time.After(time.Second):
		t.Fatal("failed successor was not retired")
	}
	if promotions != 2 {
		t.Fatalf("process-manager ownership updates = %d, want successor promotion and parent restoration", promotions)
	}
	if _, err := os.Stat(controlPath); err != nil {
		t.Fatalf("parent control socket disappeared after failed confirmation: %v", err)
	}
	controlConnection, err := net.DialTimeout("unix", controlPath, time.Second)
	if err != nil {
		t.Fatalf("parent control socket stopped accepting after failed confirmation: %v", err)
	}
	_ = controlConnection.Close()
	client, err := net.DialTimeout("tcp", publicListener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("parent stopped serving after failed confirmation: %v", err)
	}
	var upstream net.Conn
	select {
	case upstream = <-backendAccepted:
	case <-time.After(time.Second):
		client.Close()
		t.Fatal("parent did not route after failed confirmation")
	}
	signals <- os.Interrupt
	_ = client.Close()
	_ = upstream.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parent did not stop after rollback verification")
	}
}

func TestStableFrontStopsWhenParentOwnershipCannotBeRestored(t *testing.T) {
	backendListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backendListener.Close()
	backend := frontproxy.Backend{
		ID: "slot-a", Network: "tcp", Address: backendListener.Addr().String(),
	}
	router, err := frontproxy.NewRouter(backend)
	if err != nil {
		t.Fatal(err)
	}
	publicListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	controlDir, err := os.MkdirTemp("/tmp", "subrouter-front-handoff-restore-failure-")
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
	retired := make(chan struct{})
	promotions := 0
	service := &stableFront{
		router: router, readyTimeout: time.Second, drainLogInterval: 20 * time.Millisecond,
		startSuccessor: func(_ frontConfig, public, _, _ net.Listener) (frontSuccessor, error) {
			file, err := duplicateFrontListenerFile(public, "front-test-restore-failed-successor")
			if err != nil {
				return nil, err
			}
			defer file.Close()
			successorListener, err := net.FileListener(file)
			if err != nil {
				return nil, err
			}
			successorRouter, err := frontproxy.NewRouter(backend)
			if err != nil {
				successorListener.Close()
				return nil, err
			}
			return &fakeFrontSuccessor{
				listener: successorListener, router: successorRouter,
				activated: make(chan struct{}), retired: retired,
				confirmErr: errors.New("injected successor exit before serving"),
			}, nil
		},
		promoteSuccessor: func(int) error {
			promotions++
			if promotions == 2 {
				return errors.New("injected parent ownership restoration failure")
			}
			return nil
		},
	}
	signals := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() {
		done <- service.runOnListeners(frontConfig{}, publicListener, controlListener, signals)
	}()
	waitForFrontListenerReady(t, service)
	signals <- syscall.SIGHUP
	select {
	case <-retired:
	case <-time.After(time.Second):
		t.Fatal("failed successor was not retired")
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "restore front main process ownership") {
			t.Fatalf("front exit error = %v", err)
		}
	case <-time.After(time.Second):
		signals <- os.Interrupt
		<-done
		t.Fatal("front continued after losing process-manager ownership")
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
	go func() { done <- service.runOnListeners(frontConfig{}, oldListener, controlListener, signals) }()
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
	if fmt.Sprint(descriptorStoreEvents) != fmt.Sprint(wantDescriptorStoreEvents) {
		t.Fatalf("same-address retry changed descriptor store events to %v, want %v", descriptorStoreEvents, wantDescriptorStoreEvents)
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

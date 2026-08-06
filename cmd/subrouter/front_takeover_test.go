package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	frontproxy "github.com/manaflow-ai/subrouter/internal/front"
)

func TestFrontTakeoverConfigRequiresOneCompleteSource(t *testing.T) {
	valid := []string{
		"--backend-id", "slot-a",
		"--backend-address", "127.0.0.1:31417",
		"--takeover-listener-pid", "123",
		"--takeover-listener-fd", "3",
	}
	config, err := parseFrontConfig(valid)
	if err != nil {
		t.Fatal(err)
	}
	if config.TakeoverListenerPID != 123 || config.TakeoverListenerFD != 3 {
		t.Fatalf("takeover source = pid %d fd %d", config.TakeoverListenerPID, config.TakeoverListenerFD)
	}
	if err := validateFrontConfig(config); err != nil {
		t.Fatalf("valid takeover config failed: %v", err)
	}

	partial, err := parseFrontConfig(valid[:len(valid)-2])
	if err != nil {
		t.Fatal(err)
	}
	if err := validateFrontConfig(partial); err == nil {
		t.Fatal("takeover PID without a listener FD was accepted")
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
	service := &stableFront{router: router, drainLogInterval: time.Second}
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

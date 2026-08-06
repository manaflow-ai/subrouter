package main

import (
	"net"
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

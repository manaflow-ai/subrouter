//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	frontproxy "github.com/manaflow-ai/subrouter/internal/front"
)

func TestStableFrontReceivesTransferredListenerFromOneShotHelper(t *testing.T) {
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
	bootstrapListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bootstrapAddress := bootstrapListener.Addr().String()
	directory, err := os.MkdirTemp("/tmp", "subrouter-front-transfer-")
	if err != nil {
		bootstrapListener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	controlListener, err := net.Listen("unix", filepath.Join(directory, "control.sock"))
	if err != nil {
		bootstrapListener.Close()
		t.Fatal(err)
	}
	transferPath := filepath.Join(directory, "listener.sock")
	transferListener, err := listenForTransferredListeners(transferPath)
	if err != nil {
		bootstrapListener.Close()
		controlListener.Close()
		t.Fatal(err)
	}
	service.transferListener = transferListener
	service.transferErr = make(chan error, 1)
	go func() { service.transferErr <- service.serveListenerTransfers(transferListener) }()
	signals := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() { done <- service.runOnListeners(bootstrapListener, controlListener, signals) }()
	waitForFrontListenerReady(t, service)

	sourceListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	publicAddress := sourceListener.Addr().String()
	if err := sendTransferredListener(transferPath, publicAddress, sourceListener); err != nil {
		sourceListener.Close()
		t.Fatal(err)
	}
	if err := sourceListener.Close(); err != nil {
		t.Fatal(err)
	}
	if connection, err := net.DialTimeout("tcp", bootstrapAddress, 50*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("front retained its bootstrap listener after descriptor transfer")
	}
	client, err := net.DialTimeout("tcp", publicAddress, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var backend net.Conn
	select {
	case backend = <-backendAccepted:
	case <-time.After(time.Second):
		client.Close()
		t.Fatal("transferred listener did not route a connection")
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
		t.Fatal("front did not stop after transferred listener drained")
	}
}

func waitForFrontListenerReady(t *testing.T, service *stableFront) {
	t.Helper()
	for deadline := time.Now().Add(time.Second); ; {
		service.listenerMu.Lock()
		ready := service.listenerResults != nil
		service.listenerMu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("front did not start its initial listener")
		}
		time.Sleep(time.Millisecond)
	}
}

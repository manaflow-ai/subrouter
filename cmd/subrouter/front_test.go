package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	frontproxy "github.com/manaflow-ai/subrouter/internal/front"
)

func TestFrontSwitchPreservesPinnedStreamAndProxyAddresses(t *testing.T) {
	backendA := startFrontTestSupervisor(t, "a")
	backendB := startFrontTestSupervisor(t, "b")
	router, err := frontproxy.NewRouter(backendA)
	if err != nil {
		t.Fatal(err)
	}
	service := &stableFront{router: router, readyTimeout: time.Second, drainLogInterval: 20 * time.Millisecond}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = router.Serve(listener) }()

	oldClient, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	oldReader := bufio.NewReader(oldClient)
	oldSource := oldClient.LocalAddr().String()
	assertFrontReply(t, oldClient, oldReader, "one", "a", oldSource, listener.Addr().String())

	requestBody, err := json.Marshal(backendB)
	if err != nil {
		t.Fatal(err)
	}
	switchResponse := httptest.NewRecorder()
	service.controlHandler().ServeHTTP(switchResponse,
		httptest.NewRequest(http.MethodPost, "/_subrouter/switch", bytes.NewReader(requestBody)))
	if switchResponse.Code != http.StatusOK {
		t.Fatalf("switch status = %d, body = %s", switchResponse.Code, switchResponse.Body.String())
	}

	assertFrontReply(t, oldClient, oldReader, "two", "a", oldSource, listener.Addr().String())
	newClient, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	newReader := bufio.NewReader(newClient)
	assertFrontReply(t, newClient, newReader, "three", "b", newClient.LocalAddr().String(), listener.Addr().String())

	statusResponse := httptest.NewRecorder()
	service.controlHandler().ServeHTTP(statusResponse,
		httptest.NewRequest(http.MethodGet, "/_subrouter/front-status", nil))
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("status response = %d: %s", statusResponse.Code, statusResponse.Body.String())
	}
	var status struct {
		Active        frontproxy.Backend         `json:"active"`
		Backends      []frontproxy.BackendStatus `json:"backends"`
		BuildVersion  string                     `json:"build_version"`
		BuildRevision string                     `json:"build_revision"`
	}
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Active.ID != "b" || status.BuildVersion == "" || status.BuildRevision == "" {
		t.Fatalf("front status = %+v", status)
	}
	if got := frontTestBackendConnections(status.Backends, "a"); got != 1 {
		t.Fatalf("retired backend connections = %d, want 1: %+v", got, status.Backends)
	}

	_ = oldClient.Close()
	waitForFrontBackendGone(t, router, "a")
	_ = newClient.Close()
}

func TestFrontRejectsOpenBackendThatIsNotReady(t *testing.T) {
	backendA := startFrontTestSupervisor(t, "a")
	router, err := frontproxy.NewRouter(backendA)
	if err != nil {
		t.Fatal(err)
	}
	service := &stableFront{router: router, readyTimeout: time.Second, drainLogInterval: time.Second}
	unready := startUnreadyFrontBackend(t, "unready")
	body, err := json.Marshal(unready)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	service.controlHandler().ServeHTTP(response,
		httptest.NewRequest(http.MethodPost, "/_subrouter/switch", bytes.NewReader(body)))

	if response.Code != http.StatusBadGateway {
		t.Fatalf("switch status = %d, want 502, body = %s", response.Code, response.Body.String())
	}
	if active := router.Active(); active.ID != backendA.ID || active.Address != backendA.Address {
		t.Fatalf("unready backend became active: %+v", active)
	}
}

func TestFrontSwitchRequiresStrictBackendDocument(t *testing.T) {
	backend := startFrontTestSupervisor(t, "strict")
	router, err := frontproxy.NewRouter(backend)
	if err != nil {
		t.Fatal(err)
	}
	service := &stableFront{router: router, readyTimeout: time.Second, drainLogInterval: time.Second}
	for name, body := range map[string]string{
		"missing network": `{"id":"next","address":"127.0.0.1:1"}`,
		"unknown field":   `{"id":"next","network":"tcp","address":"127.0.0.1:1","extra":true}`,
		"trailing value":  `{"id":"next","network":"tcp","address":"127.0.0.1:1"}{}`,
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			service.controlHandler().ServeHTTP(response,
				httptest.NewRequest(http.MethodPost, "/_subrouter/switch", strings.NewReader(body)))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
			}
		})
	}
}

func startFrontTestSupervisor(t *testing.T, label string) frontproxy.Backend {
	t.Helper()
	workerAddress := startFrontTestWorker(t, label, http.StatusOK)
	router, err := frontproxy.NewRouter(frontproxy.Backend{ID: label + "-worker", Address: workerAddress})
	if err != nil {
		t.Fatal(err)
	}
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	slotListener := frontproxy.NewProxyProtocolListener(base)
	go func() { _ = router.Serve(slotListener) }()
	return frontproxy.Backend{ID: label, Network: "tcp", Address: base.Addr().String()}
}

func startUnreadyFrontBackend(t *testing.T, id string) frontproxy.Backend {
	t.Helper()
	return frontproxy.Backend{ID: id, Network: "tcp", Address: startFrontTestWorker(t, id, http.StatusServiceUnavailable)}
}

func startFrontTestWorker(t *testing.T, label string, readyStatus int) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go serveFrontTestWorkerConnection(connection, label, readyStatus)
		}
	}()
	return listener.Addr().String()
}

func serveFrontTestWorkerConnection(connection net.Conn, label string, readyStatus int) {
	defer connection.Close()
	reader := bufio.NewReader(connection)
	header, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	fields := strings.Fields(header)
	if len(fields) != 6 || fields[0] != "PROXY" {
		return
	}
	first, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	if strings.HasPrefix(first, "GET /_subrouter/ready ") {
		for {
			line, err := reader.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}
		_, _ = fmt.Fprintf(connection, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
			readyStatus, http.StatusText(readyStatus))
		return
	}
	for {
		line := strings.TrimSpace(first)
		_, _ = fmt.Fprintf(connection, "%s|%s:%s|%s:%s|%s\n",
			label, fields[2], fields[4], fields[3], fields[5], line)
		first, err = reader.ReadString('\n')
		if err != nil {
			return
		}
	}
}

func assertFrontReply(t *testing.T, connection net.Conn, reader *bufio.Reader, message, label, source, destination string) {
	t.Helper()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := fmt.Fprintf(connection, "%s\n", message); err != nil {
		t.Fatal(err)
	}
	reply, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%s|%s|%s|%s\n", label, source, destination, message)
	if reply != want {
		t.Fatalf("reply = %q, want %q", reply, want)
	}
}

func frontTestBackendConnections(statuses []frontproxy.BackendStatus, id string) int {
	for _, status := range statuses {
		if status.ID == id {
			return status.Connections
		}
	}
	return -1
}

func waitForFrontBackendGone(t *testing.T, router *frontproxy.Router, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if frontTestBackendConnections(router.Status(), id) == -1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("backend %q remained after its last connection closed: %+v", id, router.Status())
}

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/front"
)

// TestMain lets the test binary double as a minimal supervised worker so
// startWorkerGeneration can be exercised end to end without a real build.
func TestMain(m *testing.M) {
	if os.Getenv("SUBROUTER_TEST_FAKE_WORKER") == "1" {
		runFakeWorker()
		return
	}
	os.Exit(m.Run())
}

func runFakeWorker() {
	listener, err := inheritedListenerFromEnv()
	if err != nil || listener == nil {
		fmt.Fprintln(os.Stderr, "fake worker: no inherited listener:", err)
		os.Exit(1)
	}
	mux := http.NewServeMux()
	retired := make(chan struct{})
	var retiredOnce sync.Once
	mux.HandleFunc("/_subrouter/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("fake-worker"))
	})
	mux.HandleFunc("/hold", func(w http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		<-retired
		_, _ = io.WriteString(w, "released")
	})
	server := &http.Server{Handler: mux}
	if retireSignal != nil {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, retireSignal)
		defer signal.Stop(signals)
		go func() {
			for range signals {
				retireServer(server, nil)
				retiredOnce.Do(func() { close(retired) })
			}
		}()
	}
	if err := server.Serve(listener); err != nil {
		fmt.Fprintln(os.Stderr, "fake worker:", err)
		os.Exit(1)
	}
}

func TestParseSupervisorConfigSeparatesWorkerArguments(t *testing.T) {
	config, err := parseSupervisorConfig([]string{
		"--addr", "0.0.0.0:31415",
		"--control-socket", "/var/run/subrouter-test.sock",
		"--worker-bin", "/usr/local/bin/subrouter",
		"--",
		"serve", "--sr-switch-interval", "10m", "--bedrock",
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.Addr != "0.0.0.0:31415" || config.WorkerBin != "/usr/local/bin/subrouter" {
		t.Fatalf("unexpected config: %+v", config)
	}
	if config.ControlSocket != "/var/run/subrouter-test.sock" {
		t.Fatalf("control socket = %q", config.ControlSocket)
	}
	want := []string{"--sr-switch-interval", "10m", "--bedrock"}
	if fmt.Sprint(config.WorkerArgs) != fmt.Sprint(want) {
		t.Fatalf("worker args = %v, want %v", config.WorkerArgs, want)
	}
}

func TestParseSupervisorConfigIncludesBoundedWorkerStopGrace(t *testing.T) {
	config, err := parseSupervisorConfig([]string{
		"--control-socket", "/var/run/subrouter-test.sock",
		"--worker-bin", "/usr/local/bin/subrouter",
		"--worker-stop-grace", "125ms",
	})
	if err != nil {
		t.Fatal(err)
	}
	field := reflect.ValueOf(config).FieldByName("WorkerStopGrace")
	if !field.IsValid() {
		t.Fatal("supervisor config has no bounded worker stop grace")
	}
	if got := time.Duration(field.Int()); got != 125*time.Millisecond {
		t.Fatalf("worker stop grace = %s, want 125ms", got)
	}
}

func TestParseSupervisorConfigCanRequireProxyProtocol(t *testing.T) {
	config, err := parseSupervisorConfig([]string{
		"--control-socket", "/var/run/subrouter-test.sock",
		"--worker-bin", "/usr/local/bin/subrouter",
		"--expect-proxy-protocol",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !config.ExpectProxyProtocol {
		t.Fatal("slot supervisor did not require PROXY protocol")
	}
}

func TestPrepareControlSocketRefusesRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareControlSocket(path); err == nil {
		t.Fatal("expected regular control-socket path to be rejected")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "keep" {
		t.Fatalf("regular file was modified: %q", body)
	}
}

func TestMonitorWorkerReportsFatalWhenRecoveryFails(t *testing.T) {
	router, err := front.NewRouter(front.Backend{ID: "failed", Address: "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	worker := &workerGeneration{
		id:      "failed",
		command: &exec.Cmd{Process: &os.Process{Pid: 12345}},
		done:    done,
		err:     errors.New("worker exited"),
	}
	s := &supervisor{
		config: supervisorConfig{
			WorkerBin:    filepath.Join(t.TempDir(), "missing-subrouter"),
			ReadyTimeout: time.Second,
		},
		router:  router,
		fatal:   make(chan error, 1),
		workers: map[string]*workerGeneration{"failed": worker},
	}

	s.monitorWorker(worker)
	select {
	case fatalErr := <-s.fatal:
		if !strings.Contains(fatalErr.Error(), "active worker recovery failed") {
			t.Fatalf("fatal error = %v", fatalErr)
		}
	default:
		t.Fatal("failed active-worker recovery was not reported as fatal")
	}
}

func TestUpgradeRejectedAfterShutdownBegins(t *testing.T) {
	s := &supervisor{stopping: true}
	err := s.upgradeLocked()
	if err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("upgrade error = %v, want shutdown rejection", err)
	}
}

func TestValidateSupervisorConfigRejectsRelativeControlSocket(t *testing.T) {
	config := supervisorConfig{
		Addr:          "0.0.0.0:31415",
		ControlSocket: "subrouter.sock",
		WorkerBin:     "/usr/local/bin/subrouter",
		ReadyTimeout:  time.Second,
	}
	if err := validateSupervisorConfig(config); err == nil {
		t.Fatal("expected relative control socket to be rejected")
	}
}

func TestValidateSupervisorConfigRejectsWorkerAddress(t *testing.T) {
	config := supervisorConfig{
		Addr:          "0.0.0.0:31415",
		ControlSocket: "/var/run/subrouter-test.sock",
		WorkerBin:     "/usr/local/bin/subrouter",
		ReadyTimeout:  time.Second,
		WorkerArgs:    []string{"--addr", "127.0.0.1:1"},
	}
	if err := validateSupervisorConfig(config); err == nil {
		t.Fatal("expected worker --addr to be rejected")
	}
}

func TestInheritedListenerFromEnv(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpListener := listener.(*net.TCPListener)
	file, err := tcpListener.File()
	if err != nil {
		t.Fatal(err)
	}
	_ = listener.Close()
	t.Setenv(inheritedListenerFDEnv, fmt.Sprint(file.Fd()))

	inherited, err := inheritedListenerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer inherited.Close()
	if inherited == nil {
		t.Fatal("expected inherited listener")
	}
	accepted := make(chan struct{})
	go func() {
		connection, acceptErr := inherited.Accept()
		if acceptErr == nil {
			_ = connection.Close()
			close(accepted)
		}
	}()
	connection, err := net.Dial("tcp", inherited.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprint(connection, "PROXY TCP4 192.0.2.10 198.51.100.20 43210 31415\r\n")
	_ = connection.Close()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("inherited listener did not accept a connection")
	}
	_ = file.Close()
	if value := os.Getenv(inheritedListenerFDEnv); value != "" {
		t.Fatalf("%s was not cleared", inheritedListenerFDEnv)
	}
}

func TestSupervisorProxyProtocolModeRejectsMissingHeaderAndRestoresAddress(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := supervisorRouterListener(base, true)
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			accepted <- connection
		}
	}()

	missing, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprint(missing, "GET / HTTP/1.1\r\n\r\n")
	_ = missing.Close()
	select {
	case connection := <-accepted:
		_ = connection.Close()
		t.Fatal("slot supervisor accepted a connection without PROXY protocol")
	case <-time.After(20 * time.Millisecond):
	}

	valid, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer valid.Close()
	_, _ = fmt.Fprint(valid, "PROXY TCP4 192.0.2.77 198.51.100.88 43210 31416\r\n")
	select {
	case connection := <-accepted:
		defer connection.Close()
		if got := connection.RemoteAddr().String(); got != "192.0.2.77:43210" {
			t.Fatalf("restored remote address = %q", got)
		}
		if got := connection.LocalAddr().String(); got != "198.51.100.88:31416" {
			t.Fatalf("restored local address = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("slot supervisor did not accept a valid PROXY connection")
	}
}

func TestSupervisorRetireEndpointIsIdempotentAndDoesNotSwitch(t *testing.T) {
	if retireSignal == nil {
		t.Skip("worker retirement signal is unavailable")
	}
	if os.Getenv("SUBROUTER_TEST_RETIRE_WORKER") == "1" {
		signals := make(chan os.Signal, 2)
		signal.Notify(signals, retireSignal)
		defer signal.Stop(signals)
		fmt.Println("ready")
		<-signals
		fmt.Println("retired")
		select {}
	}

	command := exec.Command(os.Args[0], "-test.run=^TestSupervisorRetireEndpointIsIdempotentAndDoesNotSwitch$")
	command.Env = append(os.Environ(), "SUBROUTER_TEST_RETIRE_WORKER=1")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	worker := &workerGeneration{id: "worker-a", command: command, done: make(chan struct{})}
	go func() { worker.setWaitError(command.Wait()) }()
	t.Cleanup(func() {
		select {
		case <-worker.done:
		default:
			_ = command.Process.Kill()
			<-worker.done
		}
	})
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatal("retire worker helper did not become ready")
	}
	router, err := front.NewRouter(front.Backend{ID: worker.id, Address: "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	s := &supervisor{
		config:   supervisorConfig{ExpectProxyProtocol: true},
		router:   router,
		retireCh: make(chan struct{}),
		workers:  map[string]*workerGeneration{worker.id: worker},
	}

	for attempt := 0; attempt < 2; attempt++ {
		response := httptest.NewRecorder()
		s.controlHandler().ServeHTTP(response,
			httptest.NewRequest(http.MethodPost, "/_subrouter/retire", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("retire attempt %d status = %d: %s", attempt+1, response.Code, response.Body.String())
		}
		if attempt == 0 && (!scanner.Scan() || scanner.Text() != "retired") {
			t.Fatal("first retire attempt did not signal the worker")
		}
		if active := router.Active().ID; active != worker.id {
			t.Fatalf("retire attempt %d switched active worker to %q", attempt+1, active)
		}
	}
}

func TestLegacySupervisorDoesNotExposeRetireEndpoint(t *testing.T) {
	router, err := front.NewRouter(front.Backend{ID: "legacy", Address: "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	s := &supervisor{router: router}
	response := httptest.NewRecorder()
	s.controlHandler().ServeHTTP(response,
		httptest.NewRequest(http.MethodPost, "/_subrouter/retire", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("legacy retire status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func TestSlotRetirementDrainsPinnedStreamBeforeSupervisorExit(t *testing.T) {
	if retireSignal == nil {
		t.Skip("worker retirement signal is unavailable")
	}
	t.Setenv("SUBROUTER_TEST_FAKE_WORKER", "1")
	publicAddress := reserveSupervisorAddress(t)
	controlDir, err := os.MkdirTemp("/tmp", "subrouter-supervisor-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(controlDir) })
	controlSocket := filepath.Join(controlDir, "control.sock")
	config := supervisorConfig{
		Addr:                publicAddress,
		ControlSocket:       controlSocket,
		WorkerBin:           os.Args[0],
		ReadyTimeout:        10 * time.Second,
		DrainTimeout:        20 * time.Millisecond,
		WorkerStopGrace:     time.Second,
		ExpectProxyProtocol: true,
	}
	initial, err := startWorkerGeneration(config)
	if err != nil {
		t.Fatal(err)
	}
	router, err := front.NewRouter(front.Backend{
		ID: initial.id, Network: initial.network, Address: initial.address,
	})
	if err != nil {
		terminateWorker(initial, time.Second)
		t.Fatal(err)
	}
	s := &supervisor{
		config:   config,
		router:   router,
		fatal:    make(chan error, 1),
		retireCh: make(chan struct{}),
		workers:  map[string]*workerGeneration{initial.id: initial},
	}
	go s.monitorWorker(initial)
	runDone := make(chan error, 1)
	go func() { runDone <- s.run() }()
	t.Cleanup(func() {
		select {
		case <-initial.done:
		default:
			s.beginShutdown()
			_ = initial.command.Process.Kill()
			<-initial.done
		}
	})

	controlClient := unixHTTPClient(controlSocket)
	waitForSupervisorStatus(t, controlClient, runDone)
	pinned, err := net.DialTimeout("tcp", publicAddress, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := front.WriteProxyProtocolHeader(pinned,
		&net.TCPAddr{IP: net.ParseIP("192.0.2.90"), Port: 49000},
		&net.TCPAddr{IP: net.ParseIP("198.51.100.91"), Port: 31415}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(pinned,
		"POST /hold HTTP/1.1\r\nHost: worker\r\nTransfer-Encoding: chunked\r\nExpect: 100-continue\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	pinnedReader := bufio.NewReader(pinned)
	statusLine, err := pinnedReader.ReadString('\n')
	if err != nil {
		t.Fatalf("read in-flight response: %v", err)
	}
	if !strings.Contains(statusLine, "100 Continue") {
		t.Fatalf("in-flight response status = %q", statusLine)
	}
	for {
		line, err := pinnedReader.ReadString('\n')
		if err != nil {
			t.Fatalf("read pinned response: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := io.WriteString(pinned, "1\r\nx\r\n"); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		response, err := controlClient.Post("http://supervisor/_subrouter/retire", "application/json", nil)
		if err != nil {
			t.Fatalf("retire attempt %d: %v", attempt+1, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("retire attempt %d status = %d", attempt+1, response.StatusCode)
		}
	}

	waitForSupervisorListenerClosed(t, publicAddress)
	statusResponse, err := controlClient.Get("http://supervisor/_subrouter/supervisor-status")
	if err != nil {
		t.Fatalf("status during drain: %v", err)
	}
	_, _ = io.Copy(io.Discard, statusResponse.Body)
	_ = statusResponse.Body.Close()
	if statusResponse.StatusCode != http.StatusOK {
		t.Fatalf("status during drain = %d", statusResponse.StatusCode)
	}
	select {
	case err := <-runDone:
		t.Fatalf("supervisor exited with a pinned stream: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := io.WriteString(pinned, "0\r\n\r\n"); err != nil {
		t.Fatalf("pinned stream was interrupted: %v", err)
	}
	completedResponse, err := http.ReadResponse(pinnedReader, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read completed pinned response: %v", err)
	}
	responseBytes, err := io.ReadAll(completedResponse.Body)
	_ = completedResponse.Body.Close()
	if err != nil {
		t.Fatalf("read completed pinned response body: %v", err)
	}
	if string(responseBytes) != "released" {
		t.Fatalf("completed pinned response = %q", responseBytes)
	}
	if !completedResponse.Close {
		t.Fatal("retired worker allowed the completed request connection to be reused")
	}
	_ = pinned.Close()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("supervisor retirement: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not exit after the pinned stream closed")
	}
}

func reserveSupervisorAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func unixHTTPClient(socket string) *http.Client {
	return &http.Client{
		Timeout: time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
	}
}

func waitForSupervisorStatus(t *testing.T, client *http.Client, runDone <-chan error) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-runDone:
			t.Fatalf("supervisor exited before control became ready: %v", err)
		default:
		}
		response, err := client.Get("http://supervisor/_subrouter/supervisor-status")
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("supervisor control socket did not become ready")
}

func waitForSupervisorListenerClosed(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 20*time.Millisecond)
		if err != nil {
			return
		}
		_ = connection.Close()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("retired supervisor listener still accepted new connections")
}

func TestTerminateWorkerKillsAfterGracePeriod(t *testing.T) {
	if os.Getenv("SUBROUTER_TEST_IGNORE_TERM") == "1" {
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")
		for {
			time.Sleep(time.Hour)
		}
	}

	command := exec.Command(os.Args[0], "-test.run=TestTerminateWorkerKillsAfterGracePeriod")
	command.Env = append(os.Environ(), "SUBROUTER_TEST_IGNORE_TERM=1")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
		}
	})
	worker := &workerGeneration{command: command, done: make(chan struct{})}
	go func() { worker.setWaitError(command.Wait()) }()
	if !bufio.NewScanner(stdout).Scan() {
		t.Fatal("worker helper did not become ready")
	}

	terminateWorker(worker, 10*time.Millisecond)
	if command.ProcessState == nil || command.ProcessState.Success() {
		t.Fatalf("worker was not killed after grace period: %v", command.ProcessState)
	}
}

// Regression: the supervisor closes its copy of the worker's unix listener
// after the fork. That close must not unlink the socket path, because the
// supervisor keeps dialing that path for readiness checks and client routing.
func TestStartWorkerGenerationKeepsSocketPathDialable(t *testing.T) {
	t.Setenv("SUBROUTER_TEST_FAKE_WORKER", "1")
	config := supervisorConfig{
		WorkerBin:    os.Args[0],
		ReadyTimeout: 10 * time.Second,
	}
	generation, err := startWorkerGeneration(config)
	if err != nil {
		t.Fatalf("startWorkerGeneration: %v", err)
	}
	t.Cleanup(func() { terminateWorker(generation, time.Second) })

	info, err := os.Lstat(generation.address)
	if err != nil {
		t.Fatalf("worker socket path was unlinked: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("worker socket path is not a socket: %v", info.Mode())
	}

	connection, err := net.DialTimeout(generation.network, generation.address, time.Second)
	if err != nil {
		t.Fatalf("dial worker socket: %v", err)
	}
	defer connection.Close()
	if err := front.WriteProxyProtocolHeader(connection, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprint(connection, "GET /_subrouter/ready HTTP/1.0\r\nHost: worker\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	status, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		t.Fatalf("read worker response: %v", err)
	}
	if !strings.Contains(status, "200") {
		t.Fatalf("worker ready status = %q", status)
	}
}

func TestRetiredWorkerPreservesPinnedConnectionUntilClientCloses(t *testing.T) {
	backendA := startSupervisorLineBackend(t, "a")
	backendB := startSupervisorLineBackend(t, "b")
	router, err := front.NewRouter(front.Backend{ID: "a", Address: backendA})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = router.Serve(listener) }()

	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	assertSupervisorLineReply(t, connection, "before", "a:before")
	if err := router.Switch(front.Backend{ID: "b", Address: backendB}); err != nil {
		t.Fatal(err)
	}
	s := &supervisor{
		config:  supervisorConfig{DrainTimeout: 20 * time.Millisecond, WorkerStopGrace: 20 * time.Millisecond},
		router:  router,
		workers: map[string]*workerGeneration{},
	}
	done := make(chan struct{})
	go func() {
		s.reapWhenIdle("a")
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("retired backend was reaped while its client remained connected")
	case <-time.After(60 * time.Millisecond):
	}
	assertSupervisorLineReply(t, connection, "after", "a:after")
	if !supervisorBackendPresent(router.Status(), "a") {
		t.Fatal("retired backend disappeared while its client remained connected")
	}
	_ = connection.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retired backend was not reaped after its last client closed")
	}
	if supervisorBackendPresent(router.Status(), "a") {
		t.Fatal("drained backend remained in router status")
	}
}

func startSupervisorLineBackend(t *testing.T, name string) string {
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
			go func() {
				defer connection.Close()
				scanner := bufio.NewScanner(connection)
				if !scanner.Scan() || !strings.HasPrefix(scanner.Text(), "PROXY ") {
					return
				}
				for scanner.Scan() {
					_, _ = fmt.Fprintf(connection, "%s:%s\n", name, scanner.Text())
				}
			}()
		}
	}()
	return listener.Addr().String()
}

func assertSupervisorLineReply(t *testing.T, connection net.Conn, request, want string) {
	t.Helper()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if _, err := fmt.Fprintf(connection, "%s\n", request); err != nil {
		t.Fatal(err)
	}
	got, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != want {
		t.Fatalf("reply = %q, want %q", strings.TrimSpace(got), want)
	}
}

func supervisorBackendPresent(statuses []front.BackendStatus, id string) bool {
	for _, status := range statuses {
		if status.ID == id {
			return true
		}
	}
	return false
}

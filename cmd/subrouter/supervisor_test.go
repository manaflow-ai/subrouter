package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
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
	mux.HandleFunc("/_subrouter/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("fake-worker"))
	})
	if err := (&http.Server{Handler: mux}).Serve(listener); err != nil {
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

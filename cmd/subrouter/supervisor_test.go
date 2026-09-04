package main

import (
	"bufio"
	"context"
	"encoding/json"
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

func privateSocketTempRoot(t *testing.T) string {
	t.Helper()
	if info, err := os.Stat("/private/tmp"); err == nil && info.IsDir() {
		return "/private/tmp"
	}
	return os.TempDir()
}

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
	if os.Getenv("SUBROUTER_TEST_FAKE_WORKER_HANG") == "1" {
		// Alive, holding the inherited listener, never serving. A bare
		// select{} would panic on deadlock and exit, which is a different
		// failure than the one under test.
		time.Sleep(time.Hour)
		return
	}
	mux := http.NewServeMux()
	retired := make(chan struct{})
	var retiredOnce sync.Once
	mux.HandleFunc("/_subrouter/ready", func(w http.ResponseWriter, _ *http.Request) {
		if os.Getenv("SUBROUTER_TEST_FAKE_WORKER_NEVER_READY") == "1" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/_subrouter/test-private-data-router", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, os.Getenv("SUBROUTER_PRIVATE_DATA_ROUTER"))
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

func TestParseSupervisorConfigRequiresAbsoluteUpgradeInhibitFile(t *testing.T) {
	_, err := parseSupervisorConfig([]string{
		"--control-socket", "/var/run/subrouter-test.sock",
		"--worker-bin", "/usr/local/bin/subrouter",
		"--upgrade-inhibit-file", "relative/marker",
	})
	if err != nil {
		// Parsing accepts values; validation owns filesystem invariants.
		t.Fatal(err)
	}
	config, _ := parseSupervisorConfig([]string{
		"--control-socket", "/var/run/subrouter-test.sock",
		"--worker-bin", "/usr/local/bin/subrouter",
		"--upgrade-inhibit-file", "relative/marker",
	})
	if err := validateSupervisorConfig(config); err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
		t.Fatalf("relative upgrade inhibit path validation = %v", err)
	}
}

func TestUpgradeInhibitMarkerBlocksGenerationChange(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "upgrade-inhibited")
	if err := os.WriteFile(marker, []byte("transaction\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	router, err := front.NewRouter(front.Backend{ID: "initial", Network: "unix", Address: "/tmp/initial.sock"})
	if err != nil {
		t.Fatal(err)
	}
	s := &supervisor{
		config: supervisorConfig{UpgradeInhibitFile: marker},
		router: router,
	}
	if err := s.upgradeLocked(); err == nil || !strings.Contains(err.Error(), "inhibited") {
		t.Fatalf("upgrade with active transaction marker = %v", err)
	}
	if active := router.Active().ID; active != "initial" {
		t.Fatalf("upgrade inhibitor changed active generation to %q", active)
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

func TestOpenPrivateLocalDataListenerPermissionsAndStaleSafety(t *testing.T) {
	directory, err := os.MkdirTemp(privateSocketTempRoot(t), "sr-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "data.sock")
	listener, err := openPrivateLocalDataListener(socket)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode = %v, want mode-0600 socket", info.Mode())
	}
	_ = listener.Close()
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := os.Chmod(socket, 0o666); err != nil {
		t.Fatal(err)
	}
	_ = stale.Close()
	if _, err := openPrivateLocalDataListener(socket); err == nil || !strings.Contains(err.Error(), "refuse unsafe stale socket") {
		t.Fatalf("unsafe stale socket open = %v", err)
	}
	if _, err := os.Lstat(socket); err != nil {
		t.Fatalf("unsafe stale socket was removed: %v", err)
	}
}

func TestPrivateLocalDataListenerLifetimeLockRejectsSecondOwner(t *testing.T) {
	directory, err := os.MkdirTemp(privateSocketTempRoot(t), "sr-lock-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	socket := filepath.Join(directory, "data.sock")
	first, err := openPrivateLocalDataListener(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	before, err := os.Lstat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openPrivateLocalDataListener(socket); err == nil || !strings.Contains(err.Error(), "already owned") {
		t.Fatalf("second opener = %v", err)
	}
	after, err := os.Lstat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("second opener replaced the live socket")
	}
}

func TestPrivateLocalDataListenerDoesNotUnlinkSuccessor(t *testing.T) {
	directory, err := os.MkdirTemp(privateSocketTempRoot(t), "sr-successor-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	socket := filepath.Join(directory, "data.sock")
	first, err := openPrivateLocalDataListener(socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(socket, socket+".old"); err != nil {
		t.Fatal(err)
	}
	successor, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	successor.SetUnlinkOnClose(false)
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Lstat(socket)
	_ = first.Close()
	after, err := os.Lstat(socket)
	if err != nil {
		t.Fatalf("successor unlinked: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("successor identity changed")
	}
	_ = successor.Close()
}

func TestLocalDataSocketLeasePinsParentDuringStaleRecovery(t *testing.T) {
	root, err := os.MkdirTemp(privateSocketTempRoot(t), "sr-parent-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	parent := filepath.Join(root, "private")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(parent, "data.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = stale.Close()
	lease, err := acquireLocalDataSocketLease(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	oldParent := parent + ".old"
	if err := os.Rename(parent, oldParent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	successor, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	successor.SetUnlinkOnClose(false)
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	defer successor.Close()
	if err := lease.removeStaleSocket(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(oldParent, "data.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pinned stale socket remains: %v", err)
	}
	if _, err := os.Lstat(socket); err != nil {
		t.Fatalf("replacement-parent socket was touched: %v", err)
	}
}

func TestOpenPrivateLocalDataListenerRejectsSymlinkPath(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "data.sock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := openPrivateLocalDataListener(link); err == nil {
		t.Fatal("symlink local-data socket path was accepted")
	}
	body, err := os.ReadFile(target)
	if err != nil || string(body) != "keep" {
		t.Fatalf("symlink target changed: body=%q err=%v", body, err)
	}
}

func TestPrivateLocalDataListenerFailureIsFatal(t *testing.T) {
	extraErr := make(chan error, 1)
	extraErr <- errors.New("injected local listener failure")
	server := &http.Server{Addr: "127.0.0.1:0", Handler: http.NotFoundHandler()}
	err := listenAndServeWithSignalsExtra(server, nil, time.Second, nil, extraErr)
	if err == nil || !strings.Contains(err.Error(), "injected local listener failure") {
		t.Fatalf("listener failure = %v, want fatal propagation", err)
	}
}

func TestStableLocalDataRouterFollowsGenerationSwitchAndRollback(t *testing.T) {
	backendA := startSupervisorLineBackend(t, "a")
	backendB := startSupervisorLineBackend(t, "b")
	router, err := front.NewRouter(front.Backend{ID: "a", Address: backendA})
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(privateSocketTempRoot(t), "sr-router-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	listener, err := openPrivateLocalDataListener(filepath.Join(directory, "data.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() { _ = router.Serve(listener) }()
	for _, step := range []struct{ id, address, want string }{{"a", backendA, "a:x"}, {"b", backendB, "b:x"}, {"a", backendA, "a:x"}} {
		if router.Active().ID != step.id {
			if err := router.Switch(front.Backend{ID: step.id, Address: step.address}); err != nil {
				t.Fatal(err)
			}
		}
		connection, err := net.Dial("unix", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		assertSupervisorLineReply(t, connection, "x", step.want)
		_ = connection.Close()
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
	initial, err := startWorkerGeneration(config, generationInitial)
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
	beforeRetire := waitForSupervisorStatus(t, controlClient, runDone)
	if !beforeRetire.Accepting || beforeRetire.Retiring {
		t.Fatalf("status before retirement = accepting:%t retiring:%t, want true/false", beforeRetire.Accepting, beforeRetire.Retiring)
	}
	if beforeRetire.Active.ID != initial.id {
		t.Fatalf("active generation before retirement = %q, want %q", beforeRetire.Active.ID, initial.id)
	}
	if beforeRetire.Worker.ID != initial.id || beforeRetire.Worker.PID != initial.command.Process.Pid ||
		beforeRetire.Worker.ProcessStart == "" || beforeRetire.Worker.IdentityKind == "" ||
		beforeRetire.Worker.ExecutableIdentity == "" {
		t.Fatalf("active worker process identity was not kernel-bound in status: %+v", beforeRetire.Worker)
	}
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
		retiringStatus := getSupervisorStatus(t, controlClient)
		if retiringStatus.Accepting || !retiringStatus.Retiring {
			t.Fatalf("status after retire attempt %d = accepting:%t retiring:%t, want false/true",
				attempt+1, retiringStatus.Accepting, retiringStatus.Retiring)
		}
		if retiringStatus.Active.ID != initial.id {
			t.Fatalf("active generation after retire attempt %d = %q, want %q", attempt+1, retiringStatus.Active.ID, initial.id)
		}
		if connections := backendConnectionCount(retiringStatus.Backends, initial.id); connections != 1 {
			t.Fatalf("routed connections after retire attempt %d = %d, want 1", attempt+1, connections)
		}
	}

	waitForSupervisorListenerClosed(t, publicAddress)
	statusDuringDrain := getSupervisorStatus(t, controlClient)
	if statusDuringDrain.Accepting || !statusDuringDrain.Retiring {
		t.Fatalf("status during drain = accepting:%t retiring:%t, want false/true",
			statusDuringDrain.Accepting, statusDuringDrain.Retiring)
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

type supervisorControlStatus struct {
	Accepting bool                      `json:"accepting"`
	Retiring  bool                      `json:"retiring"`
	Active    front.Backend             `json:"active"`
	Backends  []front.BackendStatus     `json:"backends"`
	Worker    activeWorkerProcessStatus `json:"active_worker"`
}

func waitForSupervisorStatus(t *testing.T, client *http.Client, runDone <-chan error) supervisorControlStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-runDone:
			t.Fatalf("supervisor exited before control became ready: %v", err)
		default:
		}
		status, err := fetchSupervisorStatus(client)
		if err == nil {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("supervisor control socket did not become ready")
	return supervisorControlStatus{}
}

func getSupervisorStatus(t *testing.T, client *http.Client) supervisorControlStatus {
	t.Helper()
	status, err := fetchSupervisorStatus(client)
	if err != nil {
		t.Fatalf("supervisor status: %v", err)
	}
	return status
}

func fetchSupervisorStatus(client *http.Client) (supervisorControlStatus, error) {
	response, err := client.Get("http://supervisor/_subrouter/supervisor-status")
	if err != nil {
		return supervisorControlStatus{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, response.Body)
		return supervisorControlStatus{}, fmt.Errorf("status = %d", response.StatusCode)
	}
	var status supervisorControlStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		return supervisorControlStatus{}, err
	}
	return status, nil
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
	generation, err := startWorkerGeneration(config, generationInitial)
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

func TestStartWorkerGenerationScopesPrivateRouterEnvToConfiguredSocket(t *testing.T) {
	t.Setenv("SUBROUTER_TEST_FAKE_WORKER", "1")
	readValue := func(localDataSocket string) string {
		t.Helper()
		generation, err := startWorkerGeneration(supervisorConfig{
			WorkerBin: os.Args[0], ReadyTimeout: 10 * time.Second,
			LocalDataSocket: localDataSocket,
		}, generationInitial)
		if err != nil {
			t.Fatal(err)
		}
		defer terminateWorker(generation, time.Second)
		connection, err := net.DialTimeout(generation.network, generation.address, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		if err := front.WriteProxyProtocolHeader(connection, nil, nil); err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprint(connection, "GET /_subrouter/test-private-data-router HTTP/1.0\r\nHost: worker\r\n\r\n")
		response, err := http.ReadResponse(bufio.NewReader(connection), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	if got := readValue(""); got != "" {
		t.Fatalf("worker without local data socket inherited private-router mode %q", got)
	}
	if got := readValue("/unused/test-data.sock"); got != "1" {
		t.Fatalf("worker with local data socket inherited private-router mode %q, want 1", got)
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

// A worker that serves traffic but never reports ready must not keep the
// public listener closed. Refusing the initial generation is what turned one
// provider's unusable credentials into a total outage on 2026-09-04: the
// supervisor exited before binding and launchd restart-looped it.
func TestInitialWorkerGenerationServesWhenReadinessNeverArrives(t *testing.T) {
	t.Setenv("SUBROUTER_TEST_FAKE_WORKER", "1")
	t.Setenv("SUBROUTER_TEST_FAKE_WORKER_NEVER_READY", "1")
	config := supervisorConfig{WorkerBin: os.Args[0], ReadyTimeout: 300 * time.Millisecond}

	generation, err := startWorkerGeneration(config, generationInitial)
	if err != nil {
		t.Fatalf("initial generation must start without readiness: %v", err)
	}
	t.Cleanup(func() { terminateWorker(generation, time.Second) })

	connection, err := net.DialTimeout(generation.network, generation.address, time.Second)
	if err != nil {
		t.Fatalf("dial unready worker: %v", err)
	}
	defer connection.Close()
	if err := front.WriteProxyProtocolHeader(connection, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprint(connection, "GET / HTTP/1.0\r\nHost: worker\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	body, err := io.ReadAll(connection)
	if err != nil {
		t.Fatalf("read from unready worker: %v", err)
	}
	if !strings.Contains(string(body), "fake-worker") {
		t.Fatalf("unready worker did not serve proxy traffic: %q", body)
	}
}

// A replacement generation is free to refuse: the current worker keeps serving.
func TestReplacementWorkerGenerationStillRequiresReadiness(t *testing.T) {
	t.Setenv("SUBROUTER_TEST_FAKE_WORKER", "1")
	t.Setenv("SUBROUTER_TEST_FAKE_WORKER_NEVER_READY", "1")
	config := supervisorConfig{WorkerBin: os.Args[0], ReadyTimeout: 300 * time.Millisecond}

	generation, err := startWorkerGeneration(config, generationReplacement)
	if err == nil {
		terminateWorker(generation, time.Second)
		t.Fatal("a replacement worker that never becomes ready must be rejected")
	}
}

// An initial worker that exits has nothing to serve, so the error stays fatal
// instead of leaving the supervisor routing to a dead socket.
func TestInitialWorkerGenerationStillFailsWhenWorkerExits(t *testing.T) {
	t.Setenv("SUBROUTER_TEST_FAKE_WORKER", "1")
	binary := filepath.Join(t.TempDir(), "exiting-worker")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := supervisorConfig{WorkerBin: binary, ReadyTimeout: 300 * time.Millisecond}

	generation, err := startWorkerGeneration(config, generationInitial)
	if err == nil {
		terminateWorker(generation, time.Second)
		t.Fatal("a worker that exited must not be treated as serving")
	}
}

// A worker that never answers on its socket is not serving anything, so the
// initial generation must stay fatal: binding in front of it would turn
// connection refused into 502s and hide a hard failure.
func TestInitialWorkerGenerationStillFailsWhenWorkerNeverAnswers(t *testing.T) {
	t.Setenv("SUBROUTER_TEST_FAKE_WORKER", "1")
	t.Setenv("SUBROUTER_TEST_FAKE_WORKER_HANG", "1")
	config := supervisorConfig{WorkerBin: os.Args[0], ReadyTimeout: 300 * time.Millisecond}

	generation, err := startWorkerGeneration(config, generationInitial)
	if err == nil {
		terminateWorker(generation, time.Second)
		t.Fatal("a worker that never answers must not be served in front of")
	}
	// The worker must still have been alive: this has to be the never-answered
	// decision, not the already-exited one.
	if strings.Contains(err.Error(), "worker exited before readiness") {
		t.Fatalf("worker died instead of hanging, so the test proved nothing: %v", err)
	}
}

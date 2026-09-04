package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/manaflow-ai/subrouter/internal/front"
)

const inheritedListenerFDEnv = "SUBROUTER_LISTEN_FD"

type supervisorConfig struct {
	Addr                string
	ControlSocket       string
	LocalDataSocket     string
	WorkerBin           string
	UpgradeInhibitFile  string
	ReadyTimeout        time.Duration
	DrainTimeout        time.Duration
	WorkerStopGrace     time.Duration
	ExpectProxyProtocol bool
	TakeoverListenerPID int
	TakeoverListenerFD  int
	WorkerArgs          []string
}

type workerGeneration struct {
	id        string
	network   string
	address   string
	socketDir string
	command   *exec.Cmd
	identity  processExecutableIdentity
	done      chan struct{}

	mu  sync.Mutex
	err error
}

func (g *workerGeneration) setWaitError(err error) {
	g.mu.Lock()
	g.err = err
	g.mu.Unlock()
	if g.socketDir != "" {
		_ = os.RemoveAll(g.socketDir)
	}
	close(g.done)
}

func (g *workerGeneration) waitError() error {
	<-g.done
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}

type supervisor struct {
	config supervisorConfig
	router *front.Router
	fatal  chan error

	upgradeMu   sync.Mutex
	lifecycleMu sync.RWMutex
	stopping    bool
	retiring    bool
	retireCh    chan struct{}
	workersMu   sync.Mutex
	workers     map[string]*workerGeneration
}

func supervise(args []string) error {
	config, err := parseSupervisorConfig(args)
	if err != nil {
		return err
	}
	if err := validateSupervisorConfig(config); err != nil {
		return err
	}
	initial, err := startWorkerGeneration(config, generationInitial)
	if err != nil {
		return err
	}
	router, err := front.NewRouter(front.Backend{ID: initial.id, Network: initial.network, Address: initial.address})
	if err != nil {
		_ = initial.command.Process.Signal(syscall.SIGTERM)
		return err
	}
	s := &supervisor{
		config:   config,
		router:   router,
		fatal:    make(chan error, 1),
		retireCh: make(chan struct{}),
		workers:  map[string]*workerGeneration{initial.id: initial},
	}
	go s.monitorWorker(initial)
	return s.run()
}

func parseSupervisorConfig(args []string) (supervisorConfig, error) {
	flags := flag.NewFlagSet("supervise", flag.ContinueOnError)
	config := supervisorConfig{}
	flags.StringVar(&config.Addr, "addr", "127.0.0.1:31415", "stable client listen address")
	flags.StringVar(&config.ControlSocket, "control-socket", "/var/run/subrouter-supervisor.sock", "permissioned supervisor control socket")
	flags.StringVar(&config.LocalDataSocket, "local-data-socket", "", "stable private mode-0600 Unix data socket")
	flags.StringVar(&config.WorkerBin, "worker-bin", "", "replaceable subrouter worker binary")
	flags.StringVar(&config.UpgradeInhibitFile, "upgrade-inhibit-file", "", "absolute marker path that blocks worker generation changes while present")
	flags.DurationVar(&config.ReadyTimeout, "ready-timeout", 30*time.Second, "maximum time for a new worker to become ready")
	flags.DurationVar(&config.DrainTimeout, "drain-timeout", 10*time.Minute, "interval for reporting retired worker connections that remain pinned")
	flags.DurationVar(&config.WorkerStopGrace, "worker-stop-grace", 30*time.Second, "maximum time for a retired worker to exit after SIGTERM")
	flags.BoolVar(&config.ExpectProxyProtocol, "expect-proxy-protocol", false, "require PROXY protocol from the stable private front")
	flags.IntVar(&config.TakeoverListenerPID, "takeover-listener-pid", 0, "existing process whose live TCP listener is inherited without rebinding")
	flags.IntVar(&config.TakeoverListenerFD, "takeover-listener-fd", -1, "listener file descriptor in --takeover-listener-pid")
	if err := flags.Parse(args); err != nil {
		return supervisorConfig{}, err
	}
	config.WorkerArgs = flags.Args()
	if len(config.WorkerArgs) > 0 && config.WorkerArgs[0] == "serve" {
		config.WorkerArgs = config.WorkerArgs[1:]
	}
	return config, nil
}

func validateSupervisorConfig(config supervisorConfig) error {
	if strings.TrimSpace(config.Addr) == "" {
		return errors.New("addr is required")
	}
	if !filepath.IsAbs(config.ControlSocket) {
		return fmt.Errorf("control-socket must be an absolute path, got %q", config.ControlSocket)
	}
	if config.LocalDataSocket != "" && !filepath.IsAbs(config.LocalDataSocket) {
		return fmt.Errorf("local-data-socket must be an absolute path, got %q", config.LocalDataSocket)
	}
	if config.LocalDataSocket != "" && config.LocalDataSocket == config.ControlSocket {
		return errors.New("local-data-socket must differ from control-socket")
	}
	if strings.TrimSpace(config.WorkerBin) == "" {
		return errors.New("worker-bin is required")
	}
	if config.UpgradeInhibitFile != "" && !filepath.IsAbs(config.UpgradeInhibitFile) {
		return fmt.Errorf("upgrade-inhibit-file must be an absolute path, got %q", config.UpgradeInhibitFile)
	}
	if config.ReadyTimeout <= 0 {
		return errors.New("ready-timeout must be positive")
	}
	if config.DrainTimeout <= 0 {
		return errors.New("drain-timeout must be positive")
	}
	if config.WorkerStopGrace <= 0 {
		return errors.New("worker-stop-grace must be positive")
	}
	if (config.TakeoverListenerPID == 0) != (config.TakeoverListenerFD == -1) ||
		config.TakeoverListenerPID < 0 || config.TakeoverListenerPID == 1 || config.TakeoverListenerFD < -1 {
		return errors.New("takeover-listener-pid and takeover-listener-fd must identify one complete listener source")
	}
	for i, arg := range config.WorkerArgs {
		if arg == "--addr" || strings.HasPrefix(arg, "--addr=") {
			return fmt.Errorf("worker argument %d sets --addr; the supervisor owns worker addresses", i+1)
		}
		if arg == "--local-data-socket" || strings.HasPrefix(arg, "--local-data-socket=") {
			return fmt.Errorf("worker argument %d sets --local-data-socket; the supervisor owns the stable local data socket", i+1)
		}
	}
	return nil
}

// generationRole says what is lost when a new worker never reports ready.
//
// A replacement generation costs nothing to refuse: the current worker keeps
// serving behind the bound listener, so a candidate that cannot become ready
// is simply discarded. The initial generation is the opposite. Refusing it
// means `supervise` returns before it binds the public port, launchd restarts
// the job, and clients get connection refused for as long as the worker stays
// unready. On 2026-09-04 that turned one provider's unusable credentials into
// a total outage of a proxy whose other providers were fine, twice.
//
// Readiness is a routing preference, not a serving capability: the worker's
// mux answers proxy traffic as soon as it listens, and the scheduler already
// falls back to stale scores with per-request 401/429 failover. So at cold
// start the supervisor serves with an unready worker and says so, loudly.
type generationRole uint8

const (
	generationInitial generationRole = iota
	generationReplacement
)

func startWorkerGeneration(config supervisorConfig, role generationRole) (*workerGeneration, error) {
	socketDir, err := os.MkdirTemp("", "subrouter-worker-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketDir, 0o700); err != nil {
		_ = os.RemoveAll(socketDir)
		return nil, err
	}
	address := filepath.Join(socketDir, "worker.sock")
	listener, err := net.Listen("unix", address)
	if err != nil {
		_ = os.RemoveAll(socketDir)
		return nil, err
	}
	if err := os.Chmod(address, 0o600); err != nil {
		_ = listener.Close()
		_ = os.RemoveAll(socketDir)
		return nil, err
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		_ = listener.Close()
		_ = os.RemoveAll(socketDir)
		return nil, errors.New("worker listener cannot be inherited")
	}
	// The supervisor closes its copy of the listener after the worker
	// inherits the fd, but it keeps dialing the socket path for readiness
	// checks and client routing. Without this, Close unlinks the path and
	// every dial fails until the readiness timeout kills the worker.
	unixListener.SetUnlinkOnClose(false)
	file, err := unixListener.File()
	if err != nil {
		_ = listener.Close()
		_ = os.RemoveAll(socketDir)
		return nil, err
	}
	workerArgs := append([]string{"serve", "--addr", address}, config.WorkerArgs...)
	command := exec.Command(config.WorkerBin, workerArgs...)
	command.ExtraFiles = []*os.File{file}
	command.Env = append(os.Environ(), inheritedListenerFDEnv+"=3")
	if config.LocalDataSocket != "" {
		command.Env = append(command.Env, "SUBROUTER_PRIVATE_DATA_ROUTER=1")
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		_ = file.Close()
		_ = listener.Close()
		_ = os.RemoveAll(socketDir)
		return nil, err
	}
	identity, err := executableIdentityForProcess(command.Process.Pid)
	if err != nil {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		_ = file.Close()
		_ = listener.Close()
		_ = os.RemoveAll(socketDir)
		return nil, fmt.Errorf("capture worker process identity: %w", err)
	}
	_ = file.Close()
	_ = listener.Close()

	generation := &workerGeneration{
		id:        fmt.Sprintf("%d-%d", time.Now().UTC().UnixNano(), command.Process.Pid),
		network:   "unix",
		address:   address,
		socketDir: socketDir,
		command:   command,
		identity:  identity,
		done:      make(chan struct{}),
	}
	go func() { generation.setWaitError(command.Wait()) }()
	if err := waitForWorkerReady(generation, config.ReadyTimeout); err != nil {
		if role == generationReplacement || workerAlreadyExited(generation) {
			terminateWorker(generation, time.Second)
			return nil, err
		}
		slog.Error("initial worker did not report ready; serving with it anyway rather than leaving the public port closed",
			"generation", generation.id, "pid", command.Process.Pid, "timeout", config.ReadyTimeout, "error", err)
		go reportDelayedWorkerReadiness(generation, config.ReadyTimeout)
		return generation, nil
	}
	slog.Info("subrouter worker ready", "generation", generation.id, "pid", command.Process.Pid, "addr", address)
	return generation, nil
}

// workerAlreadyExited reports whether the worker process is gone. A worker
// that died has nothing to serve, so an unready-but-serving generation is not
// an option and the error stays fatal.
func workerAlreadyExited(generation *workerGeneration) bool {
	select {
	case <-generation.done:
		return true
	default:
		return false
	}
}

// reportDelayedWorkerReadiness records when a worker that missed its readiness
// deadline catches up, so an operator reading the log can tell a slow start
// from a permanently unready one.
func reportDelayedWorkerReadiness(generation *workerGeneration, timeout time.Duration) {
	deadline := 10 * timeout
	if deadline < time.Minute {
		deadline = time.Minute
	}
	if err := waitForWorkerReady(generation, deadline); err != nil {
		if !workerAlreadyExited(generation) {
			slog.Error("worker is serving but still not ready", "generation", generation.id, "waited", deadline, "error", err)
		}
		return
	}
	slog.Info("worker reported ready after serving unready", "generation", generation.id)
}

func terminateWorker(worker *workerGeneration, gracePeriod time.Duration) {
	_ = worker.command.Process.Signal(syscall.SIGTERM)
	timer := time.NewTimer(gracePeriod)
	defer timer.Stop()
	select {
	case <-worker.done:
		return
	case <-timer.C:
	}
	_ = worker.command.Process.Kill()
	<-worker.done
}

func waitForWorkerReady(generation *workerGeneration, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, generation.network, generation.address)
			if err != nil {
				return nil, err
			}
			if err := front.WriteProxyProtocolHeader(connection, nil, nil); err != nil {
				_ = connection.Close()
				return nil, err
			}
			return connection, nil
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: time.Second, Transport: transport}
	readyURL := "http://subrouter-worker/_subrouter/ready"
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, readyURL, nil)
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("ready check returned status %d", response.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case <-generation.done:
			return fmt.Errorf("worker exited before readiness: %w", generation.waitError())
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("worker readiness timed out after %s: last error: %w", timeout, lastErr)
			}
			return fmt.Errorf("worker readiness timed out after %s", timeout)
		case <-ticker.C:
		}
	}
}

func (s *supervisor) run() error {
	listener, err := openPublicListener(s.config.Addr, s.config.TakeoverListenerPID, s.config.TakeoverListenerFD)
	if err != nil {
		s.stopAllWorkers()
		return err
	}
	if err := prepareControlSocket(s.config.ControlSocket); err != nil {
		_ = listener.Close()
		s.stopAllWorkers()
		return err
	}
	controlListener, err := net.Listen("unix", s.config.ControlSocket)
	if err != nil {
		_ = listener.Close()
		s.stopAllWorkers()
		return err
	}
	if err := os.Chmod(s.config.ControlSocket, 0o600); err != nil {
		_ = listener.Close()
		_ = controlListener.Close()
		_ = os.Remove(s.config.ControlSocket)
		s.stopAllWorkers()
		return err
	}
	defer os.Remove(s.config.ControlSocket)
	controlServer := &http.Server{Handler: s.controlHandler(), ReadHeaderTimeout: 5 * time.Second}
	var localDataListener net.Listener
	if s.config.LocalDataSocket != "" {
		localDataListener, err = openPrivateLocalDataListener(s.config.LocalDataSocket)
		if err != nil {
			_ = listener.Close()
			_ = controlServer.Close()
			s.stopAllWorkers()
			return fmt.Errorf("local-data-socket: %w", err)
		}
		defer localDataListener.Close()
	}
	routerErrCh := make(chan error, 1)
	localDataErrCh := make(chan error, 1)
	controlErrCh := make(chan error, 1)
	routerListener := supervisorRouterListener(listener, s.config.ExpectProxyProtocol)
	go func() { routerErrCh <- s.router.Serve(routerListener) }()
	if localDataListener != nil {
		go func() { localDataErrCh <- s.router.Serve(localDataListener) }()
	}
	go func() { controlErrCh <- controlServer.Serve(controlListener) }()

	slog.Info("subrouter supervisor listening", "addr", s.config.Addr, "control_socket", s.config.ControlSocket, "worker", s.router.Active().ID)
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	for {
		select {
		case err := <-s.fatal:
			_ = listener.Close()
			if localDataListener != nil {
				_ = localDataListener.Close()
			}
			_ = controlServer.Close()
			s.beginShutdown()
			<-routerErrCh
			if localDataListener != nil {
				<-localDataErrCh
			}
			s.stopAllWorkers()
			return err
		case err := <-routerErrCh:
			if !errors.Is(err, net.ErrClosed) {
				if localDataListener != nil {
					_ = localDataListener.Close()
					<-localDataErrCh
				}
				_ = controlServer.Close()
				s.stopAllWorkers()
				return err
			}
		case err := <-localDataErrCh:
			_ = listener.Close()
			<-routerErrCh
			_ = controlServer.Close()
			s.stopAllWorkers()
			if errors.Is(err, net.ErrClosed) {
				return errors.New("private local data router closed unexpectedly")
			}
			return err
		case err := <-controlErrCh:
			if !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
				_ = listener.Close()
				if localDataListener != nil {
					_ = localDataListener.Close()
				}
				<-routerErrCh
				if localDataListener != nil {
					<-localDataErrCh
				}
				s.stopAllWorkers()
				return err
			}
		case <-s.retireCh:
			// A slot retirement stops only new public connections. The control
			// listener stays available while existing streams drain without a
			// deadline, then every worker is terminated before this process exits.
			_ = listener.Close()
			if localDataListener != nil {
				_ = localDataListener.Close()
			}
			<-routerErrCh
			if localDataListener != nil {
				<-localDataErrCh
			}
			if err := s.router.WaitAllIdle(context.Background()); err != nil {
				return err
			}
			s.terminateAllWorkers()
			_ = controlServer.Close()
			<-controlErrCh
			return nil
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				go func() {
					if err := s.upgrade(); err != nil {
						slog.Error("subrouter worker upgrade failed", "error", err)
					}
				}()
				continue
			}
			_ = listener.Close()
			if localDataListener != nil {
				_ = localDataListener.Close()
			}
			_ = controlServer.Close()
			s.beginShutdown()
			// Join the accept loop before checking connection counts. Accept and
			// acquireActive are synchronous, so no connection can appear afterward.
			<-routerErrCh
			if localDataListener != nil {
				<-localDataErrCh
			}
			drainCtx, cancel := context.WithTimeout(context.Background(), s.config.DrainTimeout)
			if err := s.router.WaitAllIdle(drainCtx); err != nil {
				slog.Warn("subrouter supervisor drain timed out", "timeout", s.config.DrainTimeout, "error", err)
			}
			cancel()
			s.stopAllWorkers()
			return nil
		}
	}
}

func supervisorRouterListener(listener net.Listener, expectProxyProtocol bool) net.Listener {
	if expectProxyProtocol {
		return front.NewProxyProtocolListener(listener)
	}
	return listener
}

func (s *supervisor) beginShutdown() {
	s.upgradeMu.Lock()
	s.lifecycleMu.Lock()
	s.stopping = true
	s.lifecycleMu.Unlock()
	s.upgradeMu.Unlock()
}

func (s *supervisor) lifecycleStatus() (accepting, retiring bool) {
	s.lifecycleMu.RLock()
	defer s.lifecycleMu.RUnlock()
	return !s.stopping, s.retiring
}

func prepareControlSocket(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("control-socket path exists and is not a socket: %s", path)
	}
	return os.Remove(path)
}

func (s *supervisor) upgrade() error {
	s.upgradeMu.Lock()
	defer s.upgradeMu.Unlock()
	return s.upgradeLocked()
}

func (s *supervisor) upgradeLocked() error {
	accepting, _ := s.lifecycleStatus()
	if !accepting {
		return errors.New("supervisor is shutting down")
	}
	if s.config.UpgradeInhibitFile != "" {
		if _, err := os.Lstat(s.config.UpgradeInhibitFile); err == nil {
			return errors.New("worker upgrades are inhibited by an active deployment transaction")
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect upgrade inhibit marker: %w", err)
		}
	}
	next, err := startWorkerGeneration(s.config, generationReplacement)
	if err != nil {
		return err
	}
	s.workersMu.Lock()
	s.workers[next.id] = next
	s.workersMu.Unlock()
	go s.monitorWorker(next)
	previous := s.router.Active()
	if err := s.router.Switch(front.Backend{ID: next.id, Network: next.network, Address: next.address}); err != nil {
		_ = next.command.Process.Signal(syscall.SIGTERM)
		return err
	}
	slog.Info("subrouter worker switched", "from", previous.ID, "to", next.id)
	go s.reapWhenIdle(previous.ID)
	return nil
}

func (s *supervisor) monitorWorker(worker *workerGeneration) {
	err := worker.waitError()
	s.upgradeMu.Lock()
	defer s.upgradeMu.Unlock()
	accepting, _ := s.lifecycleStatus()
	if !accepting {
		return
	}
	if s.router.Active().ID != worker.id {
		return
	}
	slog.Error("active subrouter worker exited", "generation", worker.id, "pid", worker.command.Process.Pid, "error", err)
	if replaceErr := s.upgradeLocked(); replaceErr != nil {
		slog.Error("subrouter worker recovery failed", "generation", worker.id, "error", replaceErr)
		select {
		case s.fatal <- fmt.Errorf("active worker recovery failed: %w", replaceErr):
		default:
		}
	}
}

func (s *supervisor) reapWhenIdle(id string) {
	// Tell the retired worker to stop reusing connections before waiting on it.
	// A client holding a keep-alive connection pins an obsolete generation
	// indefinitely: a worker from four
	// hours and two upgrades ago was still serving 64 connections, which meant
	// deployed fixes never took effect for those clients. SIGUSR1 makes the
	// worker answer with "Connection: close", so each client finishes its
	// current request and reconnects onto the current generation. Nothing in
	// flight is interrupted.
	s.workersMu.Lock()
	retiring := s.workers[id]
	s.workersMu.Unlock()
	if retireSignal != nil && retiring != nil && retiring.command != nil && retiring.command.Process != nil {
		if err := retiring.command.Process.Signal(retireSignal); err != nil {
			slog.Warn("subrouter worker retire signal failed", "generation", id, "error", err)
		}
	}

	for {
		drainCtx, cancel := context.WithTimeout(context.Background(), s.config.DrainTimeout)
		err := s.router.ForgetWhenIdleContext(drainCtx, id)
		cancel()
		if err == nil {
			break
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("subrouter retired backend could not be forgotten", "generation", id, "error", err)
			return
		}
		slog.Warn("subrouter retired worker still draining",
			"generation", id,
			"stale_after", s.config.DrainTimeout,
			"connections", backendConnectionCount(s.router.Status(), id))
	}
	s.workersMu.Lock()
	worker := s.workers[id]
	s.workersMu.Unlock()
	if worker != nil {
		slog.Info("subrouter worker drained", "generation", id, "pid", worker.command.Process.Pid)
		terminateWorker(worker, s.config.WorkerStopGrace)
		if err := worker.waitError(); err != nil {
			slog.Warn("subrouter worker exited after drain", "generation", id, "error", err)
		}
	}
	s.workersMu.Lock()
	delete(s.workers, id)
	s.workersMu.Unlock()
}

func backendConnectionCount(statuses []front.BackendStatus, id string) int {
	for _, status := range statuses {
		if status.ID == id {
			return status.Connections
		}
	}
	return 0
}

func (s *supervisor) controlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_subrouter/supervisor-status", func(w http.ResponseWriter, _ *http.Request) {
		accepting, retiring := s.lifecycleStatus()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepting":     accepting,
			"retiring":      retiring,
			"active":        s.router.Active(),
			"backends":      s.router.Status(),
			"active_worker": s.activeWorkerProcessStatus(),
		})
	})
	mux.HandleFunc("POST /_subrouter/upgrade", func(w http.ResponseWriter, _ *http.Request) {
		if err := s.upgrade(); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"active": s.router.Active()})
	})
	if s.config.ExpectProxyProtocol {
		mux.HandleFunc("POST /_subrouter/retire", func(w http.ResponseWriter, _ *http.Request) {
			if err := s.requestRetirement(); err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"active": s.router.Active(), "retired": true})
		})
	}
	return mux
}

type activeWorkerProcessStatus struct {
	ID                 string `json:"id"`
	PID                int    `json:"pid"`
	ProcessStart       string `json:"process_start_identity"`
	IdentityKind       string `json:"identity_kind"`
	ExecutableIdentity string `json:"executable_identity"`
}

func (s *supervisor) activeWorkerProcessStatus() activeWorkerProcessStatus {
	active := s.router.Active()
	s.workersMu.Lock()
	worker := s.workers[active.ID]
	s.workersMu.Unlock()
	if worker == nil || worker.command == nil || worker.command.Process == nil {
		return activeWorkerProcessStatus{ID: active.ID}
	}
	identity, err := executableIdentityForProcess(worker.command.Process.Pid)
	if err != nil || identity != worker.identity {
		return activeWorkerProcessStatus{ID: active.ID, PID: worker.command.Process.Pid}
	}
	return activeWorkerProcessStatus{
		ID:                 active.ID,
		PID:                worker.command.Process.Pid,
		ProcessStart:       identity.StartIdentity,
		IdentityKind:       identity.Kind,
		ExecutableIdentity: identity.Value,
	}
}

// requestRetirement begins the one-way shutdown of a private slot supervisor.
// It stops worker keep-alive reuse first, then wakes run to close the slot's
// client listener and wait indefinitely for pinned streams. Repeated calls are
// harmless. Legacy public supervisors never expose this transition.
func (s *supervisor) requestRetirement() error {
	s.upgradeMu.Lock()
	defer s.upgradeMu.Unlock()
	if !s.config.ExpectProxyProtocol {
		return errors.New("slot retirement requires --expect-proxy-protocol")
	}
	accepting, retiring := s.lifecycleStatus()
	if retiring {
		return nil
	}
	if !accepting {
		return errors.New("supervisor is shutting down")
	}
	if s.retireCh == nil {
		return errors.New("supervisor retirement channel is unavailable")
	}
	if retireSignal == nil {
		return errors.New("worker retirement is unsupported on this platform")
	}
	id := s.router.Active().ID
	s.workersMu.Lock()
	worker := s.workers[id]
	s.workersMu.Unlock()
	if worker == nil || worker.command == nil || worker.command.Process == nil {
		return fmt.Errorf("active worker %q is unavailable", id)
	}
	if err := worker.command.Process.Signal(retireSignal); err != nil {
		return fmt.Errorf("retire active worker %q: %w", id, err)
	}
	s.lifecycleMu.Lock()
	s.retiring = true
	s.stopping = true
	s.lifecycleMu.Unlock()
	close(s.retireCh)
	slog.Info("subrouter active worker retired", "generation", id, "pid", worker.command.Process.Pid)
	return nil
}

func (s *supervisor) terminateAllWorkers() {
	s.workersMu.Lock()
	workers := make([]*workerGeneration, 0, len(s.workers))
	for _, worker := range s.workers {
		workers = append(workers, worker)
	}
	s.workersMu.Unlock()
	for _, worker := range workers {
		terminateWorker(worker, s.config.WorkerStopGrace)
	}
}

func (s *supervisor) stopAllWorkers() {
	s.workersMu.Lock()
	workers := make([]*workerGeneration, 0, len(s.workers))
	for _, worker := range s.workers {
		workers = append(workers, worker)
	}
	s.workersMu.Unlock()
	for _, worker := range workers {
		_ = worker.command.Process.Signal(syscall.SIGTERM)
	}
}

func inheritedListenerFromEnv() (net.Listener, error) {
	raw := strings.TrimSpace(os.Getenv(inheritedListenerFDEnv))
	if raw == "" {
		return nil, nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return nil, fmt.Errorf("invalid %s %q", inheritedListenerFDEnv, raw)
	}
	_ = os.Unsetenv(inheritedListenerFDEnv)
	file := os.NewFile(uintptr(fd), "subrouter-supervisor-listener")
	if file == nil {
		return nil, fmt.Errorf("listener fd %d is unavailable", fd)
	}
	listener, err := net.FileListener(file)
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		_ = listener.Close()
		return nil, closeErr
	}
	return front.NewProxyProtocolListener(listener), nil
}

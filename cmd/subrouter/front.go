package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	frontproxy "github.com/manaflow-ai/subrouter/internal/front"
)

const maxFrontSwitchBodyBytes = 4 << 10

type frontConfig struct {
	Addr                string
	ControlSocket       string
	BackendID           string
	BackendNetwork      string
	BackendAddress      string
	ReadyTimeout        time.Duration
	DrainLogInterval    time.Duration
	TakeoverListenerPID int
	TakeoverListenerFD  int
}

func runFront(args []string) error {
	config, err := parseFrontConfig(args)
	if err != nil {
		return err
	}
	if err := validateFrontConfig(config); err != nil {
		return err
	}
	initial := frontproxy.Backend{
		ID: config.BackendID, Network: config.BackendNetwork, Address: config.BackendAddress,
	}
	if err := probeFrontBackend(context.Background(), initial, config.ReadyTimeout); err != nil {
		return fmt.Errorf("initial front backend is not ready: %w", err)
	}
	router, err := frontproxy.NewRouter(initial)
	if err != nil {
		return err
	}
	service := &stableFront{
		router: router, readyTimeout: config.ReadyTimeout, drainLogInterval: config.DrainLogInterval,
		openListener: openPublicListener,
	}
	return service.run(config)
}

func parseFrontConfig(args []string) (frontConfig, error) {
	flags := flag.NewFlagSet("front", flag.ContinueOnError)
	config := frontConfig{}
	flags.StringVar(&config.Addr, "addr", "127.0.0.1:31415", "stable public listen address")
	flags.StringVar(&config.ControlSocket, "control-socket", "/var/run/subrouter-front.sock", "permissioned front control socket")
	flags.StringVar(&config.BackendID, "backend-id", "", "initial private supervisor identifier")
	flags.StringVar(&config.BackendNetwork, "backend-network", "tcp", "initial private supervisor network (tcp or unix)")
	flags.StringVar(&config.BackendAddress, "backend-address", "", "initial private supervisor address")
	flags.DurationVar(&config.ReadyTimeout, "ready-timeout", 5*time.Second, "maximum time to validate a private supervisor")
	flags.DurationVar(&config.DrainLogInterval, "drain-log-interval", 10*time.Minute, "interval for reporting a stale retired supervisor")
	flags.IntVar(&config.TakeoverListenerPID, "takeover-listener-pid", 0, "existing process whose live TCP listener is inherited without rebinding")
	flags.IntVar(&config.TakeoverListenerFD, "takeover-listener-fd", -1, "listener file descriptor in --takeover-listener-pid")
	if err := flags.Parse(args); err != nil {
		return frontConfig{}, err
	}
	if flags.NArg() != 0 {
		return frontConfig{}, fmt.Errorf("front takes no positional arguments")
	}
	return config, nil
}

func validateFrontConfig(config frontConfig) error {
	if strings.TrimSpace(config.Addr) == "" {
		return errors.New("addr is required")
	}
	if strings.TrimSpace(config.ControlSocket) == "" || config.ControlSocket[0] != '/' {
		return fmt.Errorf("control-socket must be an absolute path, got %q", config.ControlSocket)
	}
	if config.ReadyTimeout <= 0 {
		return errors.New("ready-timeout must be positive")
	}
	if config.DrainLogInterval <= 0 {
		return errors.New("drain-log-interval must be positive")
	}
	if (config.TakeoverListenerPID == 0) != (config.TakeoverListenerFD == -1) ||
		config.TakeoverListenerPID < 0 || config.TakeoverListenerPID == 1 || config.TakeoverListenerFD < -1 {
		return errors.New("takeover-listener-pid and takeover-listener-fd must identify one complete listener source")
	}
	return frontproxy.ValidateBackend(frontproxy.Backend{
		ID: config.BackendID, Network: config.BackendNetwork, Address: config.BackendAddress,
	})
}

type stableFront struct {
	router           *frontproxy.Router
	readyTimeout     time.Duration
	drainLogInterval time.Duration
	switchMu         sync.Mutex
	listenerMu       sync.Mutex
	activeListener   *trackedFrontListener
	listenerResults  chan frontListenerResult
	listenerWG       sync.WaitGroup
	stopping         bool
	openListener     func(string, int, int) (net.Listener, error)
}

type frontListenerResult struct {
	listener *trackedFrontListener
	err      error
}

type trackedFrontListener struct {
	net.Listener
	accepted atomic.Uint64
}

func (l *trackedFrontListener) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return connection, err
}

type frontListenerStatus struct {
	Address             string `json:"address"`
	AcceptedConnections uint64 `json:"accepted_connections"`
}

type frontListenerTakeoverRequest struct {
	Address string `json:"address"`
	PID     int    `json:"pid"`
	FD      int    `json:"fd"`
}

func (f *stableFront) run(config frontConfig) error {
	listener, err := openPublicListener(config.Addr, config.TakeoverListenerPID, config.TakeoverListenerFD)
	if err != nil {
		return err
	}
	if err := prepareControlSocket(config.ControlSocket); err != nil {
		_ = listener.Close()
		return err
	}
	controlListener, err := net.Listen("unix", config.ControlSocket)
	if err != nil {
		_ = listener.Close()
		return err
	}
	if err := os.Chmod(config.ControlSocket, 0o600); err != nil {
		_ = listener.Close()
		_ = controlListener.Close()
		_ = os.Remove(config.ControlSocket)
		return err
	}
	defer os.Remove(config.ControlSocket)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	return f.runOnListeners(listener, controlListener, signals)
}

func openPublicListener(address string, takeoverPID, takeoverFD int) (net.Listener, error) {
	if takeoverPID != 0 {
		return takeoverTCPListener(takeoverPID, takeoverFD, address)
	}
	return net.Listen("tcp", address)
}

func (f *stableFront) runOnListeners(
	listener net.Listener,
	controlListener net.Listener,
	signals <-chan os.Signal,
) error {
	controlServer := &http.Server{Handler: f.controlHandler(), ReadHeaderTimeout: 5 * time.Second}
	controlErr := make(chan error, 1)
	f.listenerMu.Lock()
	f.listenerResults = make(chan frontListenerResult, 8)
	tracked := &trackedFrontListener{Listener: listener}
	f.activeListener = tracked
	f.stopping = false
	f.startServingLocked(tracked)
	f.listenerMu.Unlock()
	go func() { controlErr <- controlServer.Serve(controlListener) }()

	slog.Info("subrouter front listening", "addr", listener.Addr(), "control_socket", controlListener.Addr(), "backend", f.router.Active().ID)
	for {
		select {
		case <-signals:
			f.closeActiveListener()
			f.listenerWG.Wait()
			if err := f.waitAllIdle(); err != nil {
				_ = controlServer.Close()
				return err
			}
			_ = controlServer.Close()
			if err := <-controlErr; err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		case result := <-f.listenerResults:
			if !f.isActiveListener(result.listener) {
				if result.err != nil && !errors.Is(result.err, net.ErrClosed) {
					slog.Warn("retired front listener stopped", "addr", result.listener.Addr(), "error", result.err)
				}
				continue
			}
			_ = controlServer.Close()
			if errors.Is(result.err, net.ErrClosed) {
				return nil
			}
			return result.err
		case err := <-controlErr:
			f.closeActiveListener()
			if errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		}
	}
}

func (f *stableFront) startServingLocked(listener *trackedFrontListener) {
	f.listenerWG.Add(1)
	go func() {
		defer f.listenerWG.Done()
		f.listenerResults <- frontListenerResult{listener: listener, err: f.router.Serve(listener)}
	}()
}

func (f *stableFront) replacePublicListener(listener net.Listener) error {
	tracked := &trackedFrontListener{Listener: listener}
	f.listenerMu.Lock()
	if f.stopping || f.listenerResults == nil {
		f.listenerMu.Unlock()
		_ = listener.Close()
		return errors.New("front listener is stopping")
	}
	previous := f.activeListener
	f.activeListener = tracked
	f.startServingLocked(tracked)
	f.listenerMu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
	slog.Info("subrouter front listener replaced", "addr", listener.Addr())
	return nil
}

func (f *stableFront) closeActiveListener() {
	f.listenerMu.Lock()
	f.stopping = true
	listener := f.activeListener
	f.listenerMu.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
}

func (f *stableFront) isActiveListener(listener *trackedFrontListener) bool {
	f.listenerMu.Lock()
	defer f.listenerMu.Unlock()
	return f.activeListener == listener
}

func (f *stableFront) publicListenerStatus() *frontListenerStatus {
	f.listenerMu.Lock()
	defer f.listenerMu.Unlock()
	if f.activeListener == nil {
		return nil
	}
	return &frontListenerStatus{
		Address:             f.activeListener.Addr().String(),
		AcceptedConnections: f.activeListener.accepted.Load(),
	}
}

func (f *stableFront) waitAllIdle() error {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), f.drainLogInterval)
		err := f.router.WaitAllIdle(ctx)
		cancel()
		if err == nil {
			return nil
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		connections := 0
		for _, backend := range f.router.Status() {
			connections += backend.Connections
		}
		slog.Warn("subrouter front still draining", "stale_after", f.drainLogInterval, "connections", connections)
	}
}

func (f *stableFront) controlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_subrouter/front-status", func(w http.ResponseWriter, _ *http.Request) {
		version, revision := frontBuildIdentity()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"active": f.router.Active(), "backends": f.router.Status(),
			"build_version": version, "build_revision": revision,
			"listener": f.publicListenerStatus(),
		})
	})
	mux.HandleFunc("POST /_subrouter/takeover-listener", func(w http.ResponseWriter, r *http.Request) {
		request, err := decodeFrontListenerTakeover(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		opener := f.openListener
		if opener == nil {
			opener = openPublicListener
		}
		listener, err := opener(request.Address, request.PID, request.FD)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if err := f.replacePublicListener(listener); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"address": listener.Addr().String()})
	})
	mux.HandleFunc("POST /_subrouter/switch", func(w http.ResponseWriter, r *http.Request) {
		backend, err := decodeFrontBackend(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := f.switchBackend(r.Context(), backend); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"active": f.router.Active()})
	})
	return mux
}

func decodeFrontListenerTakeover(w http.ResponseWriter, r *http.Request) (frontListenerTakeoverRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFrontSwitchBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request frontListenerTakeoverRequest
	if err := decoder.Decode(&request); err != nil {
		return frontListenerTakeoverRequest{}, fmt.Errorf("invalid listener takeover: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return frontListenerTakeoverRequest{}, errors.New("invalid listener takeover: trailing JSON value")
	}
	if strings.TrimSpace(request.Address) == "" || request.PID <= 1 || request.FD < 0 {
		return frontListenerTakeoverRequest{}, errors.New("invalid listener takeover: address, pid, and fd are required")
	}
	if _, _, err := net.SplitHostPort(request.Address); err != nil {
		return frontListenerTakeoverRequest{}, fmt.Errorf("invalid listener takeover address: %w", err)
	}
	return request, nil
}

func decodeFrontBackend(w http.ResponseWriter, r *http.Request) (frontproxy.Backend, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFrontSwitchBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var backend frontproxy.Backend
	if err := decoder.Decode(&backend); err != nil {
		return frontproxy.Backend{}, fmt.Errorf("invalid backend: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return frontproxy.Backend{}, errors.New("invalid backend: trailing JSON value")
	}
	if strings.TrimSpace(backend.ID) == "" || strings.TrimSpace(backend.Network) == "" || strings.TrimSpace(backend.Address) == "" {
		return frontproxy.Backend{}, errors.New("invalid backend: id, network, and address are required")
	}
	if err := frontproxy.ValidateBackend(backend); err != nil {
		return frontproxy.Backend{}, err
	}
	return backend, nil
}

func (f *stableFront) switchBackend(ctx context.Context, backend frontproxy.Backend) error {
	f.switchMu.Lock()
	defer f.switchMu.Unlock()
	if err := frontproxy.ValidateBackend(backend); err != nil {
		return err
	}
	if err := probeFrontBackend(ctx, backend, f.readyTimeout); err != nil {
		return fmt.Errorf("backend %q is not ready: %w", backend.ID, err)
	}
	previous := f.router.Active()
	if err := f.router.Switch(backend); err != nil {
		return err
	}
	if previous.ID != backend.ID {
		slog.Info("subrouter front switched", "from", previous.ID, "to", backend.ID)
		go f.forgetWhenIdle(previous.ID)
	}
	return nil
}

func (f *stableFront) forgetWhenIdle(id string) {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), f.drainLogInterval)
		err := f.router.ForgetWhenIdleContext(ctx, id)
		cancel()
		if err == nil {
			slog.Info("subrouter front backend drained", "backend", id)
			return
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("subrouter front backend cleanup stopped", "backend", id, "error", err)
			return
		}
		slog.Warn("subrouter front backend still draining", "backend", id,
			"stale_after", f.drainLogInterval, "connections", backendConnectionCount(f.router.Status(), id))
	}
}

func probeFrontBackend(parent context.Context, backend frontproxy.Backend, timeout time.Duration) error {
	if err := frontproxy.ValidateBackend(backend); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			connection, err := (&net.Dialer{}).DialContext(ctx, backend.Network, backend.Address)
			if err != nil {
				return nil, err
			}
			loopback := net.ParseIP("127.0.0.1")
			if err := frontproxy.WriteProxyProtocolHeader(connection,
				&net.TCPAddr{IP: loopback, Port: 1},
				&net.TCPAddr{IP: loopback, Port: 31415}); err != nil {
				_ = connection.Close()
				return nil, err
			}
			return connection, nil
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://subrouter-private/_subrouter/ready", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("ready check returned status %d", response.StatusCode)
	}
	return nil
}

func frontBuildIdentity() (version, revision string) {
	version = "unknown"
	revision = "unknown"
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version, revision
	}
	if strings.TrimSpace(info.Main.Version) != "" {
		version = info.Main.Version
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && strings.TrimSpace(setting.Value) != "" {
			revision = setting.Value
			break
		}
	}
	return version, revision
}

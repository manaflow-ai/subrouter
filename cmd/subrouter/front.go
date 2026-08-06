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
	"time"

	frontproxy "github.com/manaflow-ai/subrouter/internal/front"
)

const maxFrontSwitchBodyBytes = 4 << 10

var (
	errNoMatchingInheritedFrontListener = errors.New("no matching inherited front listener")
	errFrontSuccessorRetired            = errors.New("front successor retired before ownership")
)

type frontSuccessor interface {
	PID() int
	Commit(time.Duration) error
	Activate(time.Duration) error
	Confirm() (bool, error)
	Abort()
	Retire()
}

type inheritedFrontProcess struct {
	publicListener    net.Listener
	controlListener   net.Listener
	transferListener  net.Listener
	prepared          func() error
	waitForCommit     func() error
	ready             func() error
	waitForActivation func() error
	started           func() error
	waitForOwnership  func() (bool, error)
	serving           func() error
	closeSync         func()
}

type frontConfig struct {
	Addr                   string
	ControlSocket          string
	ListenerTransferSocket string
	BackendID              string
	BackendNetwork         string
	BackendAddress         string
	ReadyTimeout           time.Duration
	DrainLogInterval       time.Duration
	executable             string
}

func runFront(args []string) error {
	config, err := parseFrontConfig(args)
	if err != nil {
		return err
	}
	if err := validateFrontConfig(config); err != nil {
		return err
	}
	config.executable = os.Args[0]
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
		openListener:         openFreshPublicListener,
		storeListener:        storeFrontListener,
		removeStoredListener: removeStoredFrontListener,
		startSuccessor:       startFrontSuccessor,
		promoteSuccessor:     promoteFrontSuccessor,
	}
	return service.run(config)
}

func parseFrontConfig(args []string) (frontConfig, error) {
	flags := flag.NewFlagSet("front", flag.ContinueOnError)
	config := frontConfig{}
	flags.StringVar(&config.Addr, "addr", "127.0.0.1:31415", "stable public listen address")
	flags.StringVar(&config.ControlSocket, "control-socket", "/var/run/subrouter-front.sock", "permissioned front control socket")
	flags.StringVar(&config.ListenerTransferSocket, "listener-transfer-socket", "/var/run/subrouter-front-listener.sock", "permissioned Unix socket for receiving a live TCP listener")
	flags.StringVar(&config.BackendID, "backend-id", "", "initial private supervisor identifier")
	flags.StringVar(&config.BackendNetwork, "backend-network", "tcp", "initial private supervisor network (tcp or unix)")
	flags.StringVar(&config.BackendAddress, "backend-address", "", "initial private supervisor address")
	flags.DurationVar(&config.ReadyTimeout, "ready-timeout", 5*time.Second, "maximum time to validate a private supervisor")
	flags.DurationVar(&config.DrainLogInterval, "drain-log-interval", 10*time.Minute, "interval for reporting a stale retired supervisor")
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
	if err := validateNumericTCPAddress(config.Addr); err != nil {
		return fmt.Errorf("addr must be a numeric TCP address: %w", err)
	}
	if strings.TrimSpace(config.ControlSocket) == "" || config.ControlSocket[0] != '/' {
		return fmt.Errorf("control-socket must be an absolute path, got %q", config.ControlSocket)
	}
	if strings.TrimSpace(config.ListenerTransferSocket) == "" || config.ListenerTransferSocket[0] != '/' {
		return fmt.Errorf("listener-transfer-socket must be an absolute path, got %q", config.ListenerTransferSocket)
	}
	if config.ListenerTransferSocket == config.ControlSocket {
		return errors.New("listener-transfer-socket must differ from control-socket")
	}
	if config.ReadyTimeout <= 0 {
		return errors.New("ready-timeout must be positive")
	}
	if config.DrainLogInterval <= 0 {
		return errors.New("drain-log-interval must be positive")
	}
	return frontproxy.ValidateBackend(frontproxy.Backend{
		ID: config.BackendID, Network: config.BackendNetwork, Address: config.BackendAddress,
	})
}

type stableFront struct {
	router               *frontproxy.Router
	readyTimeout         time.Duration
	drainLogInterval     time.Duration
	switchMu             sync.Mutex
	listenerTransitionMu sync.Mutex
	listenerMu           sync.Mutex
	activeListener       *trackedFrontListener
	listenerResults      chan frontListenerResult
	listenerWG           sync.WaitGroup
	listenerStop         chan struct{}
	listenerStopOnce     sync.Once
	stopping             bool
	openListener         func(string) (net.Listener, error)
	storeListener        func(net.Listener) error
	removeStoredListener func(net.Addr) error
	transferListener     net.Listener
	transferErr          chan error
	startSuccessor       func(frontConfig, net.Listener, net.Listener, net.Listener) (frontSuccessor, error)
	promoteSuccessor     func(int) error
	beforeListening      func() error
	afterPublicListening func() error
	afterListening       func() error
	handedOff            bool
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

type frontListenerReplacementRequest struct {
	Address string `json:"address"`
}

func (f *stableFront) run(config frontConfig) error {
	inheritedProcess, err := inheritedFrontProcessFromEnvironment(config)
	if err != nil {
		return err
	}
	var listener, controlListener, transferListener net.Listener
	ownsSocketPaths := false
	if inheritedProcess != nil {
		defer inheritedProcess.closeSync()
		listener = inheritedProcess.publicListener
		controlListener = inheritedProcess.controlListener
		transferListener = inheritedProcess.transferListener
		if err := inheritedProcess.prepared(); err != nil {
			closeFrontListeners(listener, controlListener, transferListener)
			return fmt.Errorf("announce prepared front successor: %w", err)
		}
		if err := inheritedProcess.waitForCommit(); err != nil {
			closeFrontListeners(listener, controlListener, transferListener)
			return fmt.Errorf("wait for front successor commit: %w", err)
		}
		f.beforeListening = func() error {
			if err := inheritedProcess.ready(); err != nil {
				return fmt.Errorf("announce ready front successor: %w", err)
			}
			if err := inheritedProcess.waitForActivation(); err != nil {
				return fmt.Errorf("wait for front successor activation: %w", err)
			}
			return nil
		}
		f.afterPublicListening = func() error {
			if err := inheritedProcess.started(); err != nil {
				return fmt.Errorf("announce started front successor: %w", err)
			}
			ownsPaths, err := inheritedProcess.waitForOwnership()
			if err != nil {
				return fmt.Errorf("wait for front successor ownership: %w", err)
			}
			if !ownsPaths {
				return errFrontSuccessorRetired
			}
			ownsSocketPaths = true
			return nil
		}
		f.afterListening = func() error {
			if err := inheritedProcess.serving(); err != nil {
				slog.Warn("could not acknowledge fully serving front successor", "error", err)
			}
			return nil
		}
	} else {
		listener, controlListener, transferListener, err = f.openFrontListeners(config)
		if err != nil {
			return err
		}
		ownsSocketPaths = true
	}
	disableAutomaticUnixUnlink(controlListener)
	disableAutomaticUnixUnlink(transferListener)
	defer closeFrontListeners(listener, controlListener, transferListener)
	defer func() {
		if ownsSocketPaths && !f.handedOff {
			_ = os.Remove(config.ControlSocket)
			_ = os.Remove(config.ListenerTransferSocket)
		}
	}()
	f.transferListener = transferListener
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, frontProcessSignals()...)
	defer signal.Stop(signals)
	return f.runOnListeners(config, listener, controlListener, signals)
}

func (f *stableFront) openFrontListeners(config frontConfig) (net.Listener, net.Listener, net.Listener, error) {
	listener, inherited, err := openFrontPublicListener(config.Addr)
	if err != nil {
		return nil, nil, nil, err
	}
	if !inherited && f.storeListener != nil {
		if err := f.storeListener(listener); err != nil {
			_ = listener.Close()
			return nil, nil, nil, fmt.Errorf("retain front listener across restart: %w", err)
		}
	}
	if err := prepareControlSocket(config.ControlSocket); err != nil {
		_ = listener.Close()
		return nil, nil, nil, err
	}
	controlListener, err := net.Listen("unix", config.ControlSocket)
	if err != nil {
		_ = listener.Close()
		return nil, nil, nil, err
	}
	if err := os.Chmod(config.ControlSocket, 0o600); err != nil {
		closeFrontListeners(listener, controlListener)
		_ = os.Remove(config.ControlSocket)
		return nil, nil, nil, err
	}
	if err := prepareControlSocket(config.ListenerTransferSocket); err != nil {
		closeFrontListeners(listener, controlListener)
		_ = os.Remove(config.ControlSocket)
		return nil, nil, nil, err
	}
	transferListener, err := listenForTransferredListeners(config.ListenerTransferSocket)
	if err != nil {
		closeFrontListeners(listener, controlListener)
		_ = os.Remove(config.ControlSocket)
		return nil, nil, nil, err
	}
	if err := os.Chmod(config.ListenerTransferSocket, 0o600); err != nil {
		closeFrontListeners(listener, controlListener, transferListener)
		_ = os.Remove(config.ControlSocket)
		_ = os.Remove(config.ListenerTransferSocket)
		return nil, nil, nil, err
	}
	return listener, controlListener, transferListener, nil
}

func closeFrontListeners(listeners ...net.Listener) {
	for _, listener := range listeners {
		if listener != nil {
			_ = listener.Close()
		}
	}
}

func disableAutomaticUnixUnlink(listener net.Listener) {
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
}

func openFreshPublicListener(address string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	network := "tcp"
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() != nil {
			network = "tcp4"
		} else {
			network = "tcp6"
		}
	}
	return (&net.ListenConfig{}).Listen(context.Background(), network, address)
}

func openFrontPublicListener(address string) (net.Listener, bool, error) {
	listeners, err := inheritedSystemdListeners()
	if err != nil {
		return nil, false, err
	}
	return selectOrOpenFrontPublicListener(address, listeners, openFreshPublicListener)
}

func selectOrOpenFrontPublicListener(
	address string,
	listeners []net.Listener,
	opener func(string) (net.Listener, error),
) (net.Listener, bool, error) {
	if len(listeners) == 0 {
		listener, err := opener(address)
		return listener, false, err
	}
	listener, err := selectFrontPublicListener(address, listeners)
	if err == nil {
		return listener, true, nil
	}
	if !errors.Is(err, errNoMatchingInheritedFrontListener) {
		return nil, false, err
	}
	slog.Warn("discarding inherited front listeners that do not match configured address", "address", address)
	listener, err = opener(address)
	return listener, false, err
}

func selectFrontPublicListener(address string, listeners []net.Listener) (net.Listener, error) {
	var selected net.Listener
	var discardErrs []error
	for _, listener := range listeners {
		if listenerAddressMatches(listener.Addr(), address) && selected == nil {
			selected = listener
			continue
		}
		if err := removeStoredFrontListener(listener.Addr()); err != nil {
			discardErrs = append(discardErrs, fmt.Errorf("discard inherited listener %s: %w", listener.Addr(), err))
		}
		_ = listener.Close()
	}
	if len(discardErrs) > 0 {
		if selected != nil {
			_ = selected.Close()
		}
		return nil, errors.Join(discardErrs...)
	}
	if selected == nil {
		return nil, fmt.Errorf("%w for %q", errNoMatchingInheritedFrontListener, address)
	}
	return selected, nil
}

func (f *stableFront) runOnListeners(
	config frontConfig,
	listener net.Listener,
	controlListener net.Listener,
	signals <-chan os.Signal,
) error {
	controlServer := &http.Server{Handler: f.controlHandler(), ReadHeaderTimeout: 5 * time.Second}
	controlErr := make(chan error, 1)
	f.listenerMu.Lock()
	f.listenerResults = make(chan frontListenerResult, 8)
	f.listenerStop = make(chan struct{})
	f.listenerStopOnce = sync.Once{}
	tracked := &trackedFrontListener{Listener: listener}
	f.activeListener = tracked
	f.stopping = false
	f.handedOff = false
	f.listenerMu.Unlock()
	if f.beforeListening != nil {
		if err := f.beforeListening(); err != nil {
			return err
		}
	}
	f.listenerMu.Lock()
	f.startServingLocked(tracked)
	f.listenerMu.Unlock()
	if f.afterPublicListening != nil {
		if err := f.afterPublicListening(); err != nil {
			f.closeActiveListener()
			f.closeTransferListener()
			f.listenerWG.Wait()
			f.stopListenerNotifications()
			if errors.Is(err, errFrontSuccessorRetired) {
				if drainErr := f.waitAllIdle(); drainErr != nil {
					return drainErr
				}
				return nil
			}
			return err
		}
	}
	go func() { controlErr <- controlServer.Serve(controlListener) }()
	if f.transferListener != nil {
		f.transferErr = make(chan error, 1)
		go func() { f.transferErr <- f.serveListenerTransfers(f.transferListener) }()
	}
	if f.afterListening != nil {
		if err := f.afterListening(); err != nil {
			f.closeActiveListener()
			f.closeTransferListener()
			_ = controlServer.Close()
			f.listenerWG.Wait()
			f.stopListenerNotifications()
			return err
		}
	}

	slog.Info("subrouter front listening", "addr", listener.Addr(), "control_socket", controlListener.Addr(), "backend", f.router.Active().ID)
	for {
		select {
		case receivedSignal := <-signals:
			if isFrontReloadSignal(receivedSignal) {
				if err := f.handoffToSuccessor(config, controlListener); err != nil {
					slog.Error("subrouter front hot reload failed; continuing on current process", "error", err)
					continue
				}
			}
			// A normal stop closes the listener before draining, so continuing
			// arrivals cannot keep shutdown alive forever. A hot reload reaches
			// this point only after its successor is already accepting the exact
			// same kernel listener.
			f.closeActiveListener()
			f.closeTransferListener()
			_ = controlServer.Close()
			f.listenerWG.Wait()
			f.stopListenerNotifications()
			if err := f.waitAllIdle(); err != nil {
				return err
			}
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
			f.closeTransferListener()
			f.stopListenerNotifications()
			if errors.Is(result.err, net.ErrClosed) {
				return nil
			}
			return result.err
		case err := <-controlErr:
			f.closeActiveListener()
			f.closeTransferListener()
			f.stopListenerNotifications()
			if errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case err := <-f.transferErr:
			f.closeActiveListener()
			_ = controlServer.Close()
			f.stopListenerNotifications()
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
	}
}

func (f *stableFront) closeTransferListener() {
	if f.transferListener != nil {
		_ = f.transferListener.Close()
	}
}

func (f *stableFront) handoffToSuccessor(config frontConfig, controlListener net.Listener) error {
	if f.startSuccessor == nil {
		return errors.New("front hot reload is unsupported on this platform")
	}
	f.listenerTransitionMu.Lock()
	defer f.listenerTransitionMu.Unlock()
	f.switchMu.Lock()
	defer f.switchMu.Unlock()

	f.listenerMu.Lock()
	if f.stopping || f.activeListener == nil {
		f.listenerMu.Unlock()
		return errors.New("front listener is already stopping")
	}
	f.stopping = true
	publicListener := f.activeListener.Listener
	f.listenerMu.Unlock()
	resetStopping := true
	defer func() {
		if resetStopping {
			f.listenerMu.Lock()
			f.stopping = false
			f.listenerMu.Unlock()
		}
	}()

	active := f.router.Active()
	nextConfig := config
	nextConfig.Addr = publicListener.Addr().String()
	nextConfig.BackendID = active.ID
	nextConfig.BackendNetwork = active.Network
	nextConfig.BackendAddress = active.Address
	successor, err := f.startSuccessor(nextConfig, publicListener, controlListener, f.transferListener)
	if err != nil {
		return fmt.Errorf("start front successor: %w", err)
	}
	abort := true
	activated := false
	defer func() {
		if abort {
			if activated {
				successor.Retire()
			} else {
				successor.Abort()
			}
		}
	}()
	if err := successor.Commit(f.readyTimeout); err != nil {
		return fmt.Errorf("prepare front successor: %w", err)
	}
	if err := successor.Activate(f.readyTimeout); err != nil {
		return fmt.Errorf("activate front successor: %w", err)
	}
	activated = true
	promote := f.promoteSuccessor
	if promote == nil {
		promote = promoteFrontSuccessor
	}
	if err := promote(successor.PID()); err != nil {
		return fmt.Errorf("promote front successor: %w", err)
	}
	ownershipCommitted, confirmErr := successor.Confirm()
	if ownershipCommitted {
		abort = false
	}
	if confirmErr != nil {
		if !ownershipCommitted {
			restoreErr := promote(os.Getpid())
			if restoreErr != nil {
				return errors.Join(
					fmt.Errorf("confirm front successor ownership: %w", confirmErr),
					fmt.Errorf("restore front main process ownership: %w", restoreErr),
				)
			}
			return fmt.Errorf("confirm front successor ownership: %w", confirmErr)
		}
		slog.Warn("front successor ownership committed without serving acknowledgement", "error", confirmErr)
	}
	abort = false
	resetStopping = false
	f.handedOff = true
	slog.Info("subrouter front successor active", "pid", successor.PID(), "backend", active.ID, "addr", publicListener.Addr())
	return nil
}

func (f *stableFront) stopListenerNotifications() {
	if f.listenerStop != nil {
		f.listenerStopOnce.Do(func() { close(f.listenerStop) })
	}
}

func (f *stableFront) startServingLocked(listener *trackedFrontListener) {
	f.listenerWG.Add(1)
	go func() {
		err := f.router.Serve(listener)
		f.listenerWG.Done()
		select {
		case f.listenerResults <- frontListenerResult{listener: listener, err: err}:
		case <-f.listenerStop:
		}
	}()
}

func (f *stableFront) replacePublicListener(listener net.Listener) error {
	f.listenerTransitionMu.Lock()
	defer f.listenerTransitionMu.Unlock()

	f.listenerMu.Lock()
	if f.stopping || f.listenerResults == nil {
		f.listenerMu.Unlock()
		_ = listener.Close()
		return errors.New("front listener is stopping")
	}
	previous := f.activeListener
	sameStoreSlot := previous != nil && frontListenersShareDescriptorStoreSlot(previous.Addr(), listener.Addr())
	f.listenerMu.Unlock()
	if f.storeListener != nil && !sameStoreSlot {
		if err := f.storeListener(listener); err != nil {
			_ = listener.Close()
			return fmt.Errorf("retain replacement listener across restart: %w", err)
		}
	}
	tracked := &trackedFrontListener{Listener: listener}
	f.listenerMu.Lock()
	if f.stopping || f.listenerResults == nil {
		f.listenerMu.Unlock()
		if f.removeStoredListener != nil && !sameStoreSlot {
			_ = f.removeStoredListener(listener.Addr())
		}
		_ = listener.Close()
		return errors.New("front listener is stopping")
	}
	f.activeListener = tracked
	f.startServingLocked(tracked)
	f.listenerMu.Unlock()
	if previous != nil {
		_ = previous.Close()
		if f.removeStoredListener != nil && !sameStoreSlot {
			if err := f.removeStoredListener(previous.Addr()); err != nil {
				slog.Warn("could not remove retired listener from systemd descriptor store", "addr", previous.Addr(), "error", err)
			}
		}
	}
	slog.Info("subrouter front listener replaced", "addr", listener.Addr())
	return nil
}

func frontListenersShareDescriptorStoreSlot(first, second net.Addr) bool {
	firstSlot, firstErr := frontListenerDescriptorStoreSlot(first)
	secondSlot, secondErr := frontListenerDescriptorStoreSlot(second)
	return firstErr == nil && secondErr == nil && firstSlot == secondSlot
}

func frontListenerDescriptorStoreSlot(address net.Addr) (string, error) {
	return numericTCPAddressKey(address.String())
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
	mux.HandleFunc("POST /_subrouter/replace-listener", func(w http.ResponseWriter, r *http.Request) {
		request, err := decodeFrontListenerReplacement(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		opener := f.openListener
		if opener == nil {
			opener = openFreshPublicListener
		}
		listener, err := opener(request.Address)
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

func decodeFrontListenerReplacement(w http.ResponseWriter, r *http.Request) (frontListenerReplacementRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFrontSwitchBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request frontListenerReplacementRequest
	if err := decoder.Decode(&request); err != nil {
		return frontListenerReplacementRequest{}, fmt.Errorf("invalid listener replacement: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return frontListenerReplacementRequest{}, errors.New("invalid listener replacement: trailing JSON value")
	}
	if strings.TrimSpace(request.Address) == "" {
		return frontListenerReplacementRequest{}, errors.New("invalid listener replacement: address is required")
	}
	if err := validateNumericTCPAddress(request.Address); err != nil {
		return frontListenerReplacementRequest{}, fmt.Errorf("invalid listener replacement address: %w", err)
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
	f.listenerMu.Lock()
	stopping := f.stopping
	f.listenerMu.Unlock()
	if stopping {
		return errors.New("front listener is stopping")
	}
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

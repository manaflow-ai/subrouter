package front

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"time"
)

// Backend identifies one worker generation behind the stable front listener.
type Backend struct {
	Network string `json:"network"`
	ID      string `json:"id"`
	Address string `json:"address"`
}

// BackendStatus is a point-in-time view of a worker generation.
type BackendStatus struct {
	ID          string `json:"id"`
	Network     string `json:"network"`
	Address     string `json:"address"`
	Connections int    `json:"connections"`
	Active      bool   `json:"active"`
	// Canary marks the candidate generation of a weighted rollout; Weight is
	// the percent of new sessions it receives.
	Canary bool `json:"canary,omitempty"`
	Weight int  `json:"weight,omitempty"`
}

type backendState struct {
	backend     Backend
	connections map[net.Conn]struct{}
}

// Router pins each accepted client connection to the backend that was active
// when the connection arrived. Switching affects only future connections.
//
// During a canary rollout (StartCanary) a second, candidate backend receives
// a weighted share of new sessions; see canary.go.
type Router struct {
	mu       sync.Mutex
	changed  *sync.Cond
	activity chan struct{}
	active   *backendState
	backends map[string]*backendState
	dial     func(network, address string) (net.Conn, error)

	canary  *backendState
	weight  int
	pins    map[sessionDigest]string
	pending map[net.Conn]struct{}
	// sessionKey derives the routing session from a connection's first
	// request head. Nil splits every connection independently.
	sessionKey  func(*http.Request) string
	peekTimeout time.Duration
	intn        func(int) int
}

func NewRouter(initial Backend) (*Router, error) {
	initial = normalizeBackend(initial)
	if err := validateBackend(initial); err != nil {
		return nil, err
	}
	state := &backendState{backend: initial, connections: make(map[net.Conn]struct{})}
	router := &Router{
		active:   state,
		backends: map[string]*backendState{initial.ID: state},
		activity: make(chan struct{}),
		pending:  make(map[net.Conn]struct{}),
		intn:     rand.IntN,
		dial: func(network, address string) (net.Conn, error) {
			return net.DialTimeout(network, address, 10*time.Second)
		},
	}
	router.changed = sync.NewCond(&router.mu)
	return router, nil
}

func validateBackend(backend Backend) error {
	if backend.ID == "" {
		return errors.New("backend id is required")
	}
	if backend.Address == "" {
		return errors.New("backend address is required")
	}
	if backend.Network != "tcp" && backend.Network != "unix" {
		return fmt.Errorf("unsupported backend network %q", backend.Network)
	}
	return nil
}

// ValidateBackend checks whether a backend can be installed without changing
// the router. Callers can perform readiness checks before an atomic Switch.
func ValidateBackend(backend Backend) error {
	return validateBackend(normalizeBackend(backend))
}

func normalizeBackend(backend Backend) Backend {
	if backend.Network == "" {
		backend.Network = "tcp"
	}
	return backend
}

// Switch atomically selects backend for new connections. Existing connections
// remain pinned to their original backend.
func (r *Router) Switch(backend Backend) error {
	backend = normalizeBackend(backend)
	if err := validateBackend(backend); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if state, ok := r.backends[backend.ID]; ok {
		if state == r.canary {
			return fmt.Errorf("backend %q is the canary; promote it instead", backend.ID)
		}
		if state.backend.Network != backend.Network || state.backend.Address != backend.Address {
			return fmt.Errorf("backend %q already uses %s address %q", backend.ID, state.backend.Network, state.backend.Address)
		}
		r.active = state
		return nil
	}
	state := &backendState{backend: backend, connections: make(map[net.Conn]struct{})}
	r.backends[backend.ID] = state
	r.active = state
	return nil
}

// ForgetWhenIdleContext atomically waits for an inactive backend's last pinned
// connection and removes it. No new connection can select an inactive backend,
// so a successful return guarantees the backend cannot reappear in status.
func (r *Router) ForgetWhenIdleContext(ctx context.Context, id string) error {
	for {
		r.mu.Lock()
		state, ok := r.backends[id]
		if !ok {
			r.mu.Unlock()
			return nil
		}
		if state == r.active || state == r.canary {
			r.mu.Unlock()
			return fmt.Errorf("cannot forget selectable backend %q", id)
		}
		if len(state.connections) == 0 {
			delete(r.backends, id)
			r.mu.Unlock()
			return nil
		}
		activity := r.activity
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-activity:
		}
	}
}

func (r *Router) Active() Backend {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active.backend
}

func (r *Router) Status() []BackendStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	statuses := make([]BackendStatus, 0, len(r.backends))
	for _, state := range r.backends {
		statuses = append(statuses, BackendStatus{
			ID:          state.backend.ID,
			Network:     state.backend.Network,
			Address:     state.backend.Address,
			Connections: len(state.connections),
			Active:      state == r.active,
			Canary:      state == r.canary,
			Weight:      canaryWeightFor(state, r.canary, r.weight),
		})
	}
	return statuses
}

// WaitIdle waits until a retired backend has no pinned client connections.
func (r *Router) WaitIdle(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		state, ok := r.backends[id]
		if !ok || len(state.connections) == 0 {
			return
		}
		r.changed.Wait()
	}
}

// WaitIdleContext waits until a retired backend has no pinned client
// connections or the caller's drain deadline expires.
func (r *Router) WaitIdleContext(ctx context.Context, id string) error {
	for {
		r.mu.Lock()
		state, ok := r.backends[id]
		idle := !ok || len(state.connections) == 0
		activity := r.activity
		r.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-activity:
		}
	}
}

// WaitAllIdle waits until every accepted client connection has closed or the
// context expires.
func (r *Router) WaitAllIdle(ctx context.Context) error {
	for {
		r.mu.Lock()
		idle := len(r.pending) == 0
		for _, state := range r.backends {
			if len(state.connections) != 0 {
				idle = false
				break
			}
		}
		activity := r.activity
		r.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-activity:
		}
	}
}

// Forget removes an idle, inactive backend from status tracking.
func (r *Router) Forget(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, ok := r.backends[id]
	if !ok {
		return nil
	}
	if state == r.active || state == r.canary {
		return fmt.Errorf("cannot forget selectable backend %q", id)
	}
	if len(state.connections) != 0 {
		return fmt.Errorf("cannot forget backend %q with %d connections", id, len(state.connections))
	}
	delete(r.backends, id)
	return nil
}

func (r *Router) Serve(listener net.Listener) error {
	for {
		client, err := listener.Accept()
		if err != nil {
			return err
		}
		// Registration is synchronous with Accept, so once the accept loop
		// is joined no connection can appear afterward. During a canary the
		// connection is held as pending until its first request head names
		// a session; pending connections count toward WaitAllIdle.
		if state := r.acquireActive(client); state != nil {
			go r.serveConnection(client, state, nil)
			continue
		}
		go r.serveCanaryConnection(client)
	}
}

func (r *Router) serveConnection(client net.Conn, state *backendState, prefix []byte) {
	defer r.release(state, client)
	defer client.Close()

	upstream, err := r.dial(state.backend.Network, state.backend.Address)
	if err != nil {
		return
	}
	defer upstream.Close()
	if err := WriteProxyProtocolHeader(upstream, client.RemoteAddr(), client.LocalAddr()); err != nil {
		return
	}
	if len(prefix) > 0 {
		if _, err := upstream.Write(prefix); err != nil {
			return
		}
	}
	proxyBidirectional(client, upstream)
}

// acquireActive pins client to the active backend. While a canary runs it
// records client as pending instead and returns nil.
func (r *Router) acquireActive(client net.Conn) *backendState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.canary != nil {
		r.pending[client] = struct{}{}
		return nil
	}
	state := r.active
	state.connections[client] = struct{}{}
	return state
}

func (r *Router) release(state *backendState, client net.Conn) {
	r.mu.Lock()
	delete(state.connections, client)
	r.changed.Broadcast()
	close(r.activity)
	r.activity = make(chan struct{})
	r.mu.Unlock()
}

func proxyBidirectional(client, upstream net.Conn) {
	done := make(chan struct{}, 2)
	copyOneWay := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if closer, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
		done <- struct{}{}
	}
	go copyOneWay(upstream, client)
	go copyOneWay(client, upstream)
	<-done
	<-done
}

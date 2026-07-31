package front

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
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
}

type backendState struct {
	backend     Backend
	connections map[net.Conn]struct{}
}

// Router pins each accepted client connection to the backend that was active
// when the connection arrived. Switching affects only future connections.
type Router struct {
	mu       sync.Mutex
	changed  *sync.Cond
	activity chan struct{}
	active   *backendState
	backends map[string]*backendState
	dial     func(network, address string) (net.Conn, error)
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
		if state.backend.Address != backend.Address {
			return fmt.Errorf("backend %q already uses address %q", backend.ID, state.backend.Address)
		}
		r.active = state
		return nil
	}
	state := &backendState{backend: backend, connections: make(map[net.Conn]struct{})}
	r.backends[backend.ID] = state
	r.active = state
	return nil
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

// CloseBackendConnections forcibly disconnects every client still pinned to a
// retired backend after its graceful drain deadline.
func (r *Router) CloseBackendConnections(id string) int {
	r.mu.Lock()
	state, ok := r.backends[id]
	if !ok {
		r.mu.Unlock()
		return 0
	}
	connections := make([]net.Conn, 0, len(state.connections))
	for connection := range state.connections {
		connections = append(connections, connection)
	}
	r.mu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
	return len(connections)
}

// WaitAllIdle waits until every accepted client connection has closed or the
// context expires.
func (r *Router) WaitAllIdle(ctx context.Context) error {
	for {
		r.mu.Lock()
		idle := true
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
	if state == r.active {
		return fmt.Errorf("cannot forget active backend %q", id)
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
		state := r.acquireActive(client)
		go r.serveConnection(client, state)
	}
}

func (r *Router) serveConnection(client net.Conn, state *backendState) {
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
	proxyBidirectional(client, upstream)
}

func (r *Router) acquireActive(client net.Conn) *backendState {
	r.mu.Lock()
	defer r.mu.Unlock()
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

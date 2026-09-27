package front

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"time"
)

// A canary rollout runs a candidate backend beside the active one and sends a
// weighted share of new sessions to it.
//
// The unit of routing is still the client connection: an accepted connection
// is pinned to one backend for its whole life, so nothing in flight ever
// moves. What the canary adds is how a new connection picks its backend. The
// router reads the connection's first request head (it does not consume it;
// the bytes are forwarded unchanged) and derives a session key with the
// function installed by SetSessionKey. A session seen before goes to the
// backend it was first given. A new session is given the candidate when a
// hash of its key, salted with the candidate id, falls under the weight, and
// is pinned there. A connection with no session key is split on its own.
//
// Pinning is per connection: a keep-alive connection that later carries a
// different session stays on the generation its first request chose.
//
// Pins exist only while a canary runs. Abort and promote clear them, because
// afterwards there is only one backend to choose.

const (
	// defaultCanaryPeekTimeout bounds how long a new connection may take to
	// send its first request head before it is routed without a session.
	defaultCanaryPeekTimeout = 2 * time.Second
	// canaryPeekLimit bounds the bytes read while looking for the head.
	canaryPeekLimit = 64 << 10
	// maxCanaryPins bounds session pins for one rollout. Pins are keyed by
	// a 32-byte digest, so the table stays under about 20 MB. Past the cap
	// new sessions are still hashed consistently, just not remembered.
	maxCanaryPins = 200_000
)

// SetSessionKey installs the function that derives a routing session key from
// a connection's first request. It returns "" when the request names no
// session. Call it before Serve.
func (r *Router) SetSessionKey(key func(*http.Request) string) {
	r.mu.Lock()
	r.sessionKey = key
	r.mu.Unlock()
}

// SetCanaryPeekTimeout overrides how long a canary routing decision waits for
// a connection's first request head. Tests use it; zero restores the default.
func (r *Router) SetCanaryPeekTimeout(timeout time.Duration) {
	r.mu.Lock()
	r.peekTimeout = timeout
	r.mu.Unlock()
}

// StartCanary installs backend as the candidate at weight percent (0-100) of
// new sessions. The active backend keeps everything else.
func (r *Router) StartCanary(backend Backend, weight int) error {
	backend = normalizeBackend(backend)
	if err := validateBackend(backend); err != nil {
		return err
	}
	if err := validateCanaryWeight(weight); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.canary != nil {
		return fmt.Errorf("canary %q is already running", r.canary.backend.ID)
	}
	if _, ok := r.backends[backend.ID]; ok {
		return fmt.Errorf("backend %q is already known", backend.ID)
	}
	state := &backendState{backend: backend, connections: make(map[net.Conn]struct{})}
	r.backends[backend.ID] = state
	r.canary = state
	r.weight = weight
	r.pins = make(map[sessionDigest]string)
	return nil
}

// SetCanaryWeight changes the share of new sessions the candidate receives.
// Sessions already pinned keep their backend.
func (r *Router) SetCanaryWeight(weight int) error {
	if err := validateCanaryWeight(weight); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.canary == nil {
		return errors.New("no canary is running")
	}
	r.weight = weight
	return nil
}

// Canary reports the candidate backend and its weight.
func (r *Router) Canary() (Backend, int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.canary == nil {
		return Backend{}, 0, false
	}
	return r.canary.backend, r.weight, true
}

// AbortCanary stops selecting the candidate. Its existing connections stay
// pinned to it until they close; the caller drains it like any retired
// backend. It returns the candidate.
func (r *Router) AbortCanary() (Backend, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.canary == nil {
		return Backend{}, errors.New("no canary is running")
	}
	candidate := r.canary.backend
	r.canary = nil
	r.weight = 0
	r.pins = nil
	return candidate, nil
}

// PromoteCanary makes the candidate the sole active backend for new
// connections. The previous active backend keeps its pinned connections and
// is returned so the caller can drain it.
func (r *Router) PromoteCanary() (Backend, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.canary == nil {
		return Backend{}, errors.New("no canary is running")
	}
	previous := r.active.backend
	r.active = r.canary
	r.canary = nil
	r.weight = 0
	r.pins = nil
	return previous, nil
}

func validateCanaryWeight(weight int) error {
	if weight < 0 || weight > 100 {
		return fmt.Errorf("canary weight %d is outside 0-100", weight)
	}
	return nil
}

func canaryWeightFor(state, canary *backendState, weight int) int {
	if state != nil && state == canary {
		return weight
	}
	return 0
}

// serveCanaryConnection routes a pending connection once its first request
// head has been read.
func (r *Router) serveCanaryConnection(client net.Conn) {
	r.mu.Lock()
	timeout := r.peekTimeout
	keyFunc := r.sessionKey
	r.mu.Unlock()
	if timeout <= 0 {
		timeout = defaultCanaryPeekTimeout
	}
	prefix, head := peekRequestHead(client, timeout, canaryPeekLimit)
	key := ""
	if head != nil && keyFunc != nil {
		if request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head))); err == nil {
			key = keyFunc(request)
		}
	}
	state := r.assignPending(client, key)
	r.serveConnection(client, state, prefix)
}

// assignPending moves a pending connection to the backend its session maps
// to. It runs under the router lock against the current selection, so a
// backend that was aborted or retired while the head was read cannot be
// chosen.
func (r *Router) assignPending(client net.Conn, key string) *backendState {
	// Hash outside the lock: a key can be a header value of tens of KiB.
	hasKey := key != ""
	var digest sessionDigest
	if hasKey {
		digest = sha256.Sum256([]byte(key))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pending, client)
	state := r.chooseLocked(hasKey, digest)
	state.connections[client] = struct{}{}
	return state
}

// sessionDigest is the fixed-size form a session key is pinned under, so a
// long header value costs the pin table no more than a short one.
type sessionDigest [sha256.Size]byte

func (r *Router) chooseLocked(hasKey bool, key sessionDigest) *backendState {
	if r.canary == nil {
		return r.active
	}
	if !hasKey {
		if r.intn(100) < r.weight {
			return r.canary
		}
		return r.active
	}
	if id, ok := r.pins[key]; ok {
		switch id {
		case r.active.backend.ID:
			return r.active
		case r.canary.backend.ID:
			return r.canary
		}
	}
	chosen := r.active
	if canaryBucket(r.canary.backend.ID, key) < r.weight {
		chosen = r.canary
	}
	if len(r.pins) < maxCanaryPins {
		r.pins[key] = chosen.backend.ID
	}
	return chosen
}

// canaryBucket maps a session to 0-99. Salting with the candidate id gives
// each rollout a different slice of sessions.
func canaryBucket(salt string, key sessionDigest) int {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(salt))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(key[:])
	return int(hash.Sum64() % 100)
}

// peekRequestHead reads from client until it has a complete HTTP/1 request
// head, the bytes stop looking like one, limit bytes are read, or timeout
// passes. It returns every byte read, which the caller must forward, and the
// head when one was found.
func peekRequestHead(client net.Conn, timeout time.Duration, limit int) (prefix, head []byte) {
	_ = client.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = client.SetReadDeadline(time.Time{}) }()
	buffer := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for len(buffer) < limit {
		room := limit - len(buffer)
		if room > len(chunk) {
			room = len(chunk)
		}
		n, err := client.Read(chunk[:room])
		buffer = append(buffer, chunk[:n]...)
		if end := bytes.Index(buffer, []byte("\r\n\r\n")); end >= 0 {
			return buffer, buffer[:end+4]
		}
		if !plausibleRequestStart(buffer) {
			return buffer, nil
		}
		if err != nil {
			return buffer, nil
		}
	}
	return buffer, nil
}

// plausibleRequestStart reports whether data can still become an HTTP/1
// request line, so a non-HTTP client is routed at once instead of waiting
// for the peek timeout.
func plausibleRequestStart(data []byte) bool {
	line := data
	if index := bytes.IndexByte(data, '\n'); index >= 0 {
		line = data[:index]
		if !bytes.Contains(line, []byte(" HTTP/")) {
			return false
		}
	}
	for index, character := range line {
		if character == ' ' {
			return index > 0
		}
		if character < 'A' || character > 'Z' {
			return false
		}
	}
	return true
}

// Command subrouter-transport-observer is a loopback-only reverse proxy used by
// the GCP deployment gate. It records only the request method, path, and whether
// the client performed a WebSocket handshake. Header values and bodies are
// deliberately never written to its evidence file.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type transportEvent struct {
	Timestamp string `json:"timestamp"`
	Transport string `json:"transport"`
	Method    string `json:"method"`
	Path      string `json:"path"`
}

type eventRecorder struct {
	mu     sync.Mutex
	writer io.Writer
}

func (r *eventRecorder) record(request *http.Request) error {
	event := transportEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Transport: observedTransport(request),
		Method:    request.Method,
		Path:      request.URL.EscapedPath(),
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return json.NewEncoder(r.writer).Encode(event)
}

func observedTransport(request *http.Request) string {
	if headerHasToken(request.Header, "Connection", "upgrade") &&
		strings.EqualFold(strings.TrimSpace(request.Header.Get("Upgrade")), "websocket") {
		return "websocket"
	}
	return "http"
}

func headerHasToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, candidate := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(candidate), token) {
				return true
			}
		}
	}
	return false
}

func newObserverHandler(upstream *url.URL, events io.Writer) http.Handler {
	recorder := &eventRecorder{writer: events}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	originalDirector := proxy.Director
	proxy.Director = func(request *http.Request) {
		originalDirector(request)
		request.Host = upstream.Host
	}
	proxy.FlushInterval = -1
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_observer/health" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := recorder.record(request); err != nil {
			http.Error(w, "record transport evidence", http.StatusInternalServerError)
			return
		}
		proxy.ServeHTTP(w, request)
	})
}

func validateObserverUpstream(upstream *url.URL) error {
	if upstream == nil || upstream.Host == "" {
		return fmt.Errorf("--upstream must include a host")
	}
	if upstream.Scheme != "http" && upstream.Scheme != "https" {
		return fmt.Errorf("--upstream must be an http or https URL")
	}
	return nil
}

func run() error {
	listenAddress := flag.String("listen", "127.0.0.1:0", "loopback address to listen on")
	upstreamValue := flag.String("upstream", "", "upstream HTTP URL")
	eventPath := flag.String("events", "", "JSONL evidence path")
	flag.Parse()

	if strings.TrimSpace(*upstreamValue) == "" {
		return fmt.Errorf("--upstream is required")
	}
	if strings.TrimSpace(*eventPath) == "" {
		return fmt.Errorf("--events is required")
	}
	upstream, err := url.Parse(*upstreamValue)
	if err != nil {
		return fmt.Errorf("parse upstream: %w", err)
	}
	if err := validateObserverUpstream(upstream); err != nil {
		return err
	}
	events, err := os.OpenFile(*eventPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open evidence file: %w", err)
	}
	defer events.Close()

	server := &http.Server{
		Addr:              *listenAddress,
		Handler:           newObserverHandler(upstream, events),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("transport observer listening on %s", *listenAddress)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

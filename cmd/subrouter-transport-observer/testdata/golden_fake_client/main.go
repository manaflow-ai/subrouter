package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	noncePattern  = regexp.MustCompile(`(?:fresh_)?nonce_[0-9a-f]+`)
	markerPattern = regexp.MustCompile(`SR_GOLDEN_(?:COMPLETE|FRESH|RESUME)_[0-9a-f]+`)
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "codex":
		fakeCodex(os.Args[2:])
	default:
		os.Exit(2)
	}
}

func argument(args []string, name string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			return args[index+1]
		}
	}
	return ""
}

func serve(args []string) {
	address := argument(args, "--addr")
	configPath := argument(args, "--cloud-config")
	data, err := os.ReadFile(configPath)
	if err != nil {
		os.Exit(3)
	}
	var config struct {
		HostedURL string `json:"hostedUrl"`
		TenantKey string `json:"tenantKey"`
	}
	if json.Unmarshal(data, &config) != nil {
		os.Exit(3)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/_subrouter/health", "/_subrouter/ready":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		if !strings.HasSuffix(request.URL.Path, "/responses") {
			http.NotFound(w, request)
			return
		}
		leaseURL := strings.TrimRight(config.HostedURL, "/") + "/t/" + config.TenantKey + "/_subrouter/leases"
		leaseRequest, _ := http.NewRequestWithContext(request.Context(), http.MethodPost, leaseURL, strings.NewReader("LEASE_REQUEST_BODY_SECRET"))
		leaseRequest.Header.Set("Authorization", "Bearer LEASE_HEADER_SECRET")
		if response, leaseErr := http.DefaultClient.Do(leaseRequest); leaseErr == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
		streamResponse(w, request)
	})
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: time.Second}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		os.Exit(4)
	}
}

func streamResponse(w http.ResponseWriter, request *http.Request) {
	duration := 8 * time.Second
	if request.Header.Get("X-Golden-Short") == "1" {
		duration = 120 * time.Millisecond
	}
	if strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "upgrade", http.StatusInternalServerError)
			return
		}
		connection, buffered, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer connection.Close()
		key := request.Header.Get("Sec-WebSocket-Key")
		digest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = fmt.Fprintf(buffered, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(digest[:]))
		_ = buffered.Flush()
		deadline := time.Now().Add(duration)
		for time.Now().Before(deadline) {
			_, _ = connection.Write([]byte{0x81, 0x01, 'x'})
			time.Sleep(20 * time.Millisecond)
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		_, _ = w.Write([]byte("data:x\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type fakeState struct {
	Nonce    string `json:"nonce"`
	ThreadID string `json:"thread_id"`
}

func fakeCodex(args []string) {
	resumeIndex := -1
	for index, arg := range args {
		if arg == "resume" {
			resumeIndex = index
			break
		}
	}
	prompt := args[len(args)-1]
	codexHome := os.Getenv("CODEX_HOME")
	statePath := filepath.Join(codexHome, "fake-state.json")
	var state fakeState
	if resumeIndex >= 0 {
		data, err := os.ReadFile(statePath)
		if err != nil || json.Unmarshal(data, &state) != nil {
			os.Exit(5)
		}
	} else {
		state.Nonce = noncePattern.FindString(prompt)
		hash := sha1.Sum([]byte(codexHome))
		state.ThreadID = fmt.Sprintf("thread-%x", hash[:8])
		data, _ := json.Marshal(state)
		_ = os.WriteFile(statePath, data, 0o600)
	}
	marker := markerPattern.FindString(prompt)
	if state.Nonce == "" || state.ThreadID == "" || marker == "" {
		os.Exit(6)
	}
	encoder := json.NewEncoder(os.Stdout)
	_ = encoder.Encode(map[string]any{"type": "thread.started", "thread_id": state.ThreadID})
	transport := "websocket"
	for _, arg := range args {
		if strings.Contains(arg, "supports_websockets=false") {
			transport = "http"
		}
	}
	short := strings.Contains(marker, "_FRESH_") || strings.Contains(marker, "_RESUME_")
	if err := makeRequest(os.Getenv("SUBROUTER_CODEX_BASE_URL"), transport, short); err != nil {
		os.Exit(7)
	}
	text := state.Nonce + "\n" + marker
	_ = encoder.Encode(map[string]any{
		"type": "item.completed",
		"item": map[string]any{"type": "agent_message", "text": text},
	})
}

func makeRequest(baseURL, transport string, short bool) error {
	target := strings.TrimRight(baseURL, "/") + "/responses"
	if transport == "http" {
		request, err := http.NewRequest(http.MethodPost, target, strings.NewReader("REQUEST_BODY_SECRET"))
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer REQUEST_HEADER_SECRET")
		if short {
			request.Header.Set("X-Golden-Short", "1")
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return err
		}
		_, err = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return err
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return err
	}
	connection, err := net.Dial("tcp", parsed.Host)
	if err != nil {
		return err
	}
	defer connection.Close()
	shortHeader := ""
	if short {
		shortHeader = "X-Golden-Short: 1\r\n"
	}
	_, err = fmt.Fprintf(connection, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: ZmFrZS1nb2xkZW4ta2V5\r\nAuthorization: Bearer REQUEST_HEADER_SECRET\r\n%s\r\n", parsed.RequestURI(), parsed.Host, shortHeader)
	if err != nil {
		return err
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("upgrade status")
	}
	_, err = io.Copy(io.Discard, reader)
	return err
}

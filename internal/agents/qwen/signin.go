package qwen

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const consoleOrigin = "https://modelstudio.console.alibabacloud.com"
const consoleCallbackMaxBytes = 64 << 10

// AuthorizeConsole receives Alibaba's official console-login callback directly.
// The console session grants account access beyond quota reads. It is separate
// from the model API key and requires no vendor CLI or browser cookie access.
func AuthorizeConsole(ctx context.Context, out io.Writer, openBrowser func(string)) (ConsoleCredential, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return ConsoleCredential{}, errors.New("could not listen for Alibaba authorization on localhost")
	}
	defer listener.Close()
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ConsoleCredential{}, errors.New("could not create Alibaba authorization state")
	}
	state := hex.EncodeToString(nonce[:])
	result := make(chan ConsoleCredential, 1)
	var mu sync.Mutex
	complete := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Vary", "Origin, Access-Control-Request-Private-Network")
		origin := r.Header.Get("Origin")
		if r.Host != listener.Addr().String() || origin != "" && origin != consoleOrigin {
			http.Error(w, "Origin not allowed", http.StatusForbidden)
			return
		}
		if origin == consoleOrigin {
			w.Header().Set("Access-Control-Allow-Origin", consoleOrigin)
		}
		if r.Method == http.MethodOptions {
			if origin != consoleOrigin {
				http.Error(w, "Origin not allowed", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
				w.Header().Set("Access-Control-Allow-Private-Network", "true")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(query["state"]) != 1 || subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(state)) != 1 {
			http.Error(w, "Invalid authorization state", http.StatusBadRequest)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, consoleCallbackMaxBytes)
		credential, err := parseConsoleCallback(r, query)
		if err != nil {
			http.Error(w, "Invalid console authorization callback", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if complete {
			http.Error(w, "Authorization already received", http.StatusConflict)
			return
		}
		complete = true
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "Authorization received. Return to Subrouter to finish quota setup.\n")
		_ = http.NewResponseController(w).Flush()
		result <- credential
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 8192, ErrorLog: log.New(io.Discard, "", 0)}
	defer server.Close()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	// Alibaba's official browser route expects this nested query syntax.
	authorizationURL := consoleOrigin + "/console-login?notice=" + listener.Addr().String() + "?state=" + state
	fmt.Fprintln(out, "Alibaba console authorization grants account access beyond quota reads. Subrouter saves it separately from the model API key.")
	fmt.Fprintf(out, "Open %s\nWaiting up to 15 minutes. Allow local-network access if your browser asks.\n", authorizationURL)
	if openBrowser != nil {
		openBrowser(authorizationURL)
	}
	select {
	case credential := <-result:
		return credential, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ConsoleCredential{}, errors.New("Alibaba authorization timed out; run sr qwen login again")
		}
		return ConsoleCredential{}, ctx.Err()
	case <-serveErr:
		return ConsoleCredential{}, errors.New("Alibaba authorization listener stopped")
	}
}

func parseConsoleCallback(r *http.Request, query url.Values) (ConsoleCredential, error) {
	fields := map[string]any{}
	for key, values := range query {
		if len(values) == 1 {
			fields[key] = values[0]
		}
	}
	if r.Method != http.MethodGet {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return ConsoleCredential{}, err
		}
		if len(body) > 0 {
			mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if mediaType == "multipart/form-data" {
				r.Body = io.NopCloser(strings.NewReader(string(body)))
				if err := r.ParseMultipartForm(consoleCallbackMaxBytes); err != nil {
					return ConsoleCredential{}, err
				}
				defer r.MultipartForm.RemoveAll()
				for key, values := range r.MultipartForm.Value {
					if len(values) == 1 {
						fields[key] = values[0]
					}
				}
			} else {
				var object map[string]any
				decoder := json.NewDecoder(strings.NewReader(string(body)))
				decoder.UseNumber()
				if decoder.Decode(&object) == nil && object != nil {
					var trailing any
					if decoder.Decode(&trailing) != io.EOF {
						return ConsoleCredential{}, errors.New("invalid console callback JSON")
					}
					if data, ok := object["data"].(map[string]any); ok {
						object = data
					}
					for key, value := range object {
						fields[key] = value
					}
				} else {
					if mediaType == "application/json" || mediaType == "text/json" || strings.HasPrefix(strings.TrimSpace(string(body)), "{") {
						return ConsoleCredential{}, errors.New("invalid console callback JSON")
					}
					form, err := url.ParseQuery(string(body))
					if err != nil {
						return ConsoleCredential{}, err
					}
					for key, values := range form {
						if len(values) == 1 {
							fields[key] = values[0]
						}
					}
				}
			}
		}
	}
	field := func(names ...string) string {
		for _, name := range names {
			if value, ok := fields[name].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	credential := ConsoleCredential{AccessToken: field("access_token", "accessToken"), ConsoleSite: field("console_site", "consoleSite"), ConsoleRegion: field("console_region", "consoleRegion")}
	if credential.AccessToken == "" || len(credential.AccessToken) > 16384 || strings.IndexFunc(credential.AccessToken, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return ConsoleCredential{}, errors.New("invalid console token")
	}
	if credential.ConsoleSite == "" {
		credential.ConsoleSite = defaultSite
	}
	if credential.ConsoleRegion == "" {
		credential.ConsoleRegion = defaultRegion
	}
	if credential.ConsoleSite != defaultSite || credential.ConsoleRegion != defaultRegion {
		return ConsoleCredential{}, errors.New("unexpected console region or site")
	}
	var selector string
	for _, name := range []string{"console_switch_agent", "consoleSwitchAgent"} {
		switch value := fields[name].(type) {
		case nil:
			continue
		case string:
			selector = strings.TrimSpace(value)
		case json.Number:
			selector = value.String()
		default:
			return ConsoleCredential{}, errors.New("invalid console account selector")
		}
		if selector != "" {
			break
		}
	}
	if selector != "" {
		agent, err := strconv.ParseInt(selector, 10, 64)
		if err != nil || agent < 0 {
			return ConsoleCredential{}, errors.New("invalid console account selector")
		}
		credential.ConsoleSwitchAgent = &agent
	}
	return credential, nil
}

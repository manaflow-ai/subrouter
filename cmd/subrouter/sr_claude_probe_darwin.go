//go:build darwin

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/storepath"
)

// claude.ai's Cloudflare blocks Go's net/http at the TLS fingerprint level
// even with a valid sessionKey, so on macOS the web API is reached through
// this tiny Swift URLSession probe, the same approach CodexBar ships. The
// probe is compiled lazily with swiftc into the state dir; compilation takes
// far longer than the sr status enrichment budget, so the first run kicks it
// off in the background and enrichment simply no-ops until the binary exists.
//
// The session key is passed via the CLAUDE_WEB_SK environment variable, never
// argv (which is visible in ps), and never logged.
const claudeWebProbeSource = `import Foundation
let url = CommandLine.arguments[1]
let sk = ProcessInfo.processInfo.environment["CLAUDE_WEB_SK"] ?? ""
var req = URLRequest(url: URL(string: url)!)
req.setValue("sessionKey=\(sk)", forHTTPHeaderField: "Cookie")
req.setValue("application/json", forHTTPHeaderField: "Accept")
let sem = DispatchSemaphore(value: 0)
var result: [String: Any] = ["status": -1]
URLSession(configuration: .ephemeral).dataTask(with: req) { data, resp, _ in
    if let http = resp as? HTTPURLResponse {
        result["status"] = http.statusCode
        if let sc = http.allHeaderFields["Set-Cookie"] as? String { result["set_cookie"] = sc }
    }
    if let data = data, let s = String(data: data, encoding: .utf8) { result["body"] = s }
    sem.signal()
}.resume()
_ = sem.wait(timeout: .now() + 20)
if let out = try? JSONSerialization.data(withJSONObject: result), let s = String(data: out, encoding: .utf8) { print(s) }
`

func init() {
	claudeWebDefaultTransport = probeClaudeWebTransport
	claudeWebTransportReady = claudeWebProbeReady
}

var (
	errClaudeWebProbeNotReady = errors.New("claude web probe is not compiled yet")
	claudeWebSwiftcPath       = "/usr/bin/swiftc"
	claudeWebProbeCompileOnce sync.Once
	// claudeWebProbeCompileDone closes when the background compile (if any)
	// finishes; tests wait on it so the goroutine cannot outlive a temp dir.
	claudeWebProbeCompileDone = make(chan struct{})
)

// kickClaudeWebProbeCompile starts at most one background compile per process.
func kickClaudeWebProbeCompile(path string) {
	claudeWebProbeCompileOnce.Do(func() {
		go func() {
			defer close(claudeWebProbeCompileDone)
			compileClaudeWebProbe(path)
		}()
	})
}

func claudeWebProbeBinaryPath() string {
	sum := sha256.Sum256([]byte(claudeWebProbeSource))
	return filepath.Join(storepath.CodexDir(), "bin", fmt.Sprintf("claude-web-probe-%x", sum[:6]))
}

func claudeWebProbeBinaryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// claudeWebProbeReady reports whether the probe binary exists, kicking off a
// background compile once per process when it does not.
func claudeWebProbeReady() bool {
	path := claudeWebProbeBinaryPath()
	if claudeWebProbeBinaryExists(path) {
		return true
	}
	kickClaudeWebProbeCompile(path)
	return false
}

// compileClaudeWebProbe builds the probe with swiftc -O. Concurrent compilers
// are serialized with an exclusive lockfile; a lock older than five minutes
// is treated as stale (a crashed compile) and stolen. The binary is written
// to a temp path and renamed into place so a partial build is never served.
func compileClaudeWebProbe(path string) {
	if _, err := os.Stat(claudeWebSwiftcPath); err != nil {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	lockPath := path + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > 5*time.Minute {
			_ = os.Remove(lockPath)
			lock, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		}
		if err != nil {
			return
		}
	}
	defer func() {
		_ = lock.Close()
		_ = os.Remove(lockPath)
	}()

	srcPath := path + ".swift"
	if err := os.WriteFile(srcPath, []byte(claudeWebProbeSource), 0600); err != nil {
		return
	}
	defer os.Remove(srcPath)
	tmpBin := path + ".tmp"
	defer os.Remove(tmpBin)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, claudeWebSwiftcPath, "-O", "-o", tmpBin, srcPath).Run(); err != nil {
		return
	}
	if err := os.Chmod(tmpBin, 0700); err != nil {
		return
	}
	_ = os.Rename(tmpBin, path)
}

// probeClaudeWebTransport runs the compiled probe with a 25s ceiling and
// parses its JSON stdout protocol: {"status":int,"body":string,"set_cookie":...}.
func probeClaudeWebTransport(ctx context.Context, url, sessionKey string) (claudeWebResponse, error) {
	path := claudeWebProbeBinaryPath()
	if !claudeWebProbeBinaryExists(path) {
		kickClaudeWebProbeCompile(path)
		return claudeWebResponse{}, errClaudeWebProbeNotReady
	}
	execCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(execCtx, path, url)
	cmd.Env = append(os.Environ(), "CLAUDE_WEB_SK="+sessionKey)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return claudeWebResponse{}, err
	}
	if err := cmd.Start(); err != nil {
		return claudeWebResponse{}, err
	}
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, io.LimitReader(stdout, claudeWebMaxBodyBytes))
	if err := cmd.Wait(); err != nil {
		return claudeWebResponse{}, err
	}
	var probe struct {
		Status    int    `json:"status"`
		Body      string `json:"body"`
		SetCookie string `json:"set_cookie"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &probe); err != nil {
		return claudeWebResponse{}, err
	}
	if probe.Status < 0 {
		return claudeWebResponse{}, errors.New("claude web probe timed out or failed")
	}
	return claudeWebResponse{status: probe.Status, body: []byte(probe.Body), setCookie: probe.SetCookie}, nil
}

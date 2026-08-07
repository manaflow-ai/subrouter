package main

import (
	"io"
	"sort"
	"strings"
	"time"
)

func goldenTransportIssueCategories(text string) []string {
	text = strings.ToLower(text)
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		if strings.Contains(line, "subrouter shutdown signal received") && strings.Contains(line, "signal=terminated") {
			lines[index] = ""
		}
	}
	text = strings.Join(lines, "\n")
	categories := map[string][]string{
		"reconnect": {"reconnect", "disconnected", "connection reset"},
		"retry":     {"retry", "retrying"},
		"fallback":  {"falling back", "fallback"},
		"timeout":   {"timed out", "timeout"},
		"oom":       {"out of memory", "oom-kill", "oom_kill"},
		"error":     {"error", "failed", "failure"},
	}
	result := make([]string, 0, len(categories))
	for category, needles := range categories {
		for _, needle := range needles {
			if strings.Contains(text, needle) {
				result = append(result, category)
				break
			}
		}
	}
	if strings.Contains(text, "deadline exceeded") && !containsString(result, "timeout") {
		result = append(result, "timeout")
	}
	sort.Strings(result)
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (r *goldenRunner) consumeGoldenLocalDaemonStderr(reader io.Reader) {
	buffer := make([]byte, 32<<10)
	tail := ""
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			text := tail + string(buffer[:n])
			for _, category := range goldenTransportIssueCategories(text) {
				r.localIssueMu.Lock()
				if r.localIssues == nil {
					r.localIssues = make(map[string]int)
				}
				first := r.localIssues[category] == 0
				r.localIssues[category]++
				r.localIssueMu.Unlock()
				if first {
					_ = r.evidence.write(map[string]any{
						"kind": "local_daemon_transport_issue", "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
						"category": category,
					})
				}
			}
			if len(text) > 128 {
				tail = text[len(text)-128:]
			} else {
				tail = text
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *goldenRunner) requireGoldenLocalDaemonTransportClean() error {
	r.localIssueMu.Lock()
	defer r.localIssueMu.Unlock()
	if len(r.localIssues) == 0 {
		return nil
	}
	keys := make([]string, 0, len(r.localIssues))
	for key, count := range r.localIssues {
		if count > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return nil
	}
	return failGolden("local_daemon_transport_issue_" + keys[0])
}

func (r *goldenRunner) waitGoldenLocalDaemonStderr() error {
	r.mu.Lock()
	done := r.localStderrDone
	r.mu.Unlock()
	if done == nil {
		return failGolden("local_daemon_stderr_drain_failed")
	}
	select {
	case <-done:
		return nil
	case <-time.After(2 * time.Second):
		return failGolden("local_daemon_stderr_drain_failed")
	}
}

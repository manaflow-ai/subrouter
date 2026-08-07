package main

import (
	"bufio"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	goldenStructuredLogMessage = regexp.MustCompile(`(?:^|[[:space:]])msg=("(?:\\.|[^"\\])*"|[^[:space:]]+)`)
	goldenLegacyLogMessage     = regexp.MustCompile(`^[0-9]{4}[-/][0-9]{2}[-/][0-9]{2}(?:T[^[:space:]]+|[[:space:]]+[^[:space:]]+)[[:space:]]+(?:debug|info|warn|error)[[:space:]]+(.*)$`)
	goldenStructuredAttribute  = regexp.MustCompile(`[[:space:]][a-z_][a-z0-9_]*=`)
)

func goldenTransportIssueCategories(text string) []string {
	text = strings.ToLower(strings.TrimSpace(text))
	if strings.Contains(text, "subrouter shutdown signal received") && strings.Contains(text, "signal=terminated") {
		return nil
	}
	text = goldenLocalDaemonLogMessage(text)
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

func goldenLocalDaemonLogMessage(line string) string {
	if match := goldenStructuredLogMessage.FindStringSubmatch(line); len(match) == 2 {
		if strings.HasPrefix(match[1], `"`) {
			if message, err := strconv.Unquote(match[1]); err == nil {
				return message
			}
		}
		return match[1]
	}
	match := goldenLegacyLogMessage.FindStringSubmatch(line)
	if len(match) != 2 {
		return line
	}
	message := match[1]
	if attribute := goldenStructuredAttribute.FindStringIndex(message); attribute != nil {
		message = message[:attribute[0]]
	}
	return strings.TrimSpace(message)
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
	buffered := bufio.NewReaderSize(reader, 32<<10)
	for {
		line, err := buffered.ReadString('\n')
		if len(line) > 0 {
			for _, category := range goldenTransportIssueCategories(line) {
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

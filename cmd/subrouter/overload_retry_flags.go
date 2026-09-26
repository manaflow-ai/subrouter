package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

const (
	retryIntervalFlag = "--retry-interval"
	retryMaxWaitFlag  = "--retry-max-wait"
)

// takeOverloadRetryFlags removes --retry-interval and --retry-max-wait
// ("--flag 2s" or "--flag=2s") from launcher arguments, never after --, and
// returns the X-Subrouter-Retry header value they ask for, or "" when
// neither is given. The daemon honors the header only when its operator
// allows it.
func takeOverloadRetryFlags(args []string) ([]string, string, error) {
	out := make([]string, 0, len(args))
	interval := time.Duration(0)
	maxWait := time.Duration(-1)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			out = append(out, args[i:]...)
			break
		}
		name, value, inline := strings.Cut(arg, "=")
		if name != retryIntervalFlag && name != retryMaxWaitFlag {
			out = append(out, arg)
			continue
		}
		if !inline {
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("%s needs a duration such as 2s", name)
			}
			i++
			value = args[i]
		}
		d, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil && strings.TrimSpace(value) == "0" {
			d, err = 0, nil
		}
		if err != nil {
			return nil, "", fmt.Errorf("%s %q: want a duration such as 2s or 20m", name, value)
		}
		switch name {
		case retryIntervalFlag:
			if d < proxy.OverloadRetryMinInterval {
				return nil, "", fmt.Errorf("%s %q: must be at least %s", name, value, proxy.OverloadRetryMinInterval)
			}
			interval = d
		case retryMaxWaitFlag:
			if d < 0 || d > proxy.OverloadRetryMaxWaitCap {
				return nil, "", fmt.Errorf("%s %q: want 0 (until you stop the request) up to %s", name, value, proxy.OverloadRetryMaxWaitCap)
			}
			maxWait = d
		}
	}
	if interval == 0 && maxWait < 0 {
		return out, "", nil
	}
	return out, proxy.FormatOverloadRetryHeader(interval, maxWait), nil
}

// codexOverloadRetryConfigArgs send the X-Subrouter-Retry header on every
// Codex request, as a leaf of the launcher's provider table.
func codexOverloadRetryConfigArgs(header string) []string {
	return []string{"-c", `model_providers.subrouter.http_headers.X-Subrouter-Retry="` + header + `"`}
}

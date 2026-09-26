package proxy

import (
	"encoding/json"
	"io"
	"os"
)

// ReleaseState is the subset of the deploy scripts' release-state.json that
// /_subrouter/health reports. The file also carries the bake baseline, which
// is the guard's business and stays out of the health payload.
type ReleaseState struct {
	Version         string `json:"version,omitempty"`
	PreviousVersion string `json:"previous_version,omitempty"`
	State           string `json:"state"`
	Reason          string `json:"reason,omitempty"`
	Since           string `json:"since,omitempty"`
	BakeUntil       string `json:"bake_until,omitempty"`
}

// releaseStateMaxBytes bounds the read; the real file is a few hundred bytes.
const releaseStateMaxBytes = 64 << 10

// readReleaseState reads path. A missing, unreadable, oversized, or malformed
// file reports nothing rather than failing health: the field is advisory.
func readReleaseState(path string) (ReleaseState, bool) {
	if path == "" {
		return ReleaseState{}, false
	}
	file, err := os.Open(path)
	if err != nil {
		return ReleaseState{}, false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, releaseStateMaxBytes+1))
	if err != nil || len(data) > releaseStateMaxBytes {
		return ReleaseState{}, false
	}
	var state ReleaseState
	if err := json.Unmarshal(data, &state); err != nil || state.State == "" {
		return ReleaseState{}, false
	}
	return state, true
}

// Package buildversion reports the identity of the running subrouter binary.
//
// Release builds stamp the fields at link time:
//
//	-ldflags "-X github.com/manaflow-ai/subrouter/internal/buildversion.version=v1.2.3
//	          -X github.com/manaflow-ai/subrouter/internal/buildversion.commit=<sha>
//	          -X github.com/manaflow-ai/subrouter/internal/buildversion.buildDate=<rfc3339>"
//
// Unstamped builds fall back to runtime/debug build info (module version,
// vcs.revision, vcs.time, vcs.modified), and finally to "devel"/"unknown".
package buildversion

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Link-time stamped values. Empty means "not stamped".
var (
	version   = ""
	commit    = ""
	buildDate = ""
)

// Info is the resolved identity of the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	Modified  bool   `json:"modified,omitempty"`
	GoVersion string `json:"go_version"`
}

// readBuildInfo is replaceable in tests.
var readBuildInfo = debug.ReadBuildInfo

// Get resolves the running binary's identity.
func Get() Info {
	info := Info{
		Version:   strings.TrimSpace(version),
		Commit:    strings.TrimSpace(commit),
		BuildDate: strings.TrimSpace(buildDate),
		GoVersion: runtime.Version(),
	}
	if bi, ok := readBuildInfo(); ok && bi != nil {
		if info.Version == "" {
			if v := strings.TrimSpace(bi.Main.Version); v != "" && v != "(devel)" {
				info.Version = v
			}
		}
		stampedCommit := info.Commit != ""
		for _, setting := range bi.Settings {
			value := strings.TrimSpace(setting.Value)
			switch setting.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = value
				}
			case "vcs.time":
				if info.BuildDate == "" {
					info.BuildDate = value
				}
			case "vcs.modified":
				// A stamped commit describes the release; only trust the
				// toolchain's dirty flag when it also supplied the revision.
				if !stampedCommit && value == "true" {
					info.Modified = true
				}
			}
		}
	}
	if len(info.Commit) > 12 {
		info.Commit = info.Commit[:12]
	}
	if info.Version == "" {
		info.Version = "devel"
	}
	if info.Commit == "" {
		info.Commit = "unknown"
	}
	if info.BuildDate == "" {
		info.BuildDate = "unknown"
	}
	return info
}

// Version returns a compact version string such as "v1.2.3" or
// "devel+abc123def456-dirty", suitable for health and status payloads.
func Version() string {
	info := Get()
	if info.Version != "devel" || info.Commit == "unknown" {
		return info.Version
	}
	out := info.Version + "+" + info.Commit
	if info.Modified {
		out += "-dirty"
	}
	return out
}

// String renders a one-line human description for `<program> version`.
func (info Info) String() string {
	commit := info.Commit
	if info.Modified {
		commit += "-dirty"
	}
	return fmt.Sprintf("%s (commit %s, built %s, %s)", info.Version, commit, info.BuildDate, info.GoVersion)
}

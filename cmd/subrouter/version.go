package main

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
)

// These are stamped at link time via:
//
//	-ldflags "-X main.version=… -X main.commit=… -X main.buildDate=…"
//
// Unset values fall back to module build info or "devel".
var (
	version   = ""
	commit    = ""
	buildDate = ""
)

type buildInfo struct {
	Version   string
	Commit    string
	BuildDate string
	GoVersion string
}

func resolveBuildInfo() buildInfo {
	info := buildInfo{
		Version:   strings.TrimSpace(version),
		Commit:    strings.TrimSpace(commit),
		BuildDate: strings.TrimSpace(buildDate),
		GoVersion: runtime.Version(),
	}
	if info.Version == "" || info.Commit == "" || info.BuildDate == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			if info.Version == "" || info.Version == "devel" {
				if v := strings.TrimSpace(bi.Main.Version); v != "" && v != "(devel)" {
					info.Version = v
				}
			}
			for _, setting := range bi.Settings {
				switch setting.Key {
				case "vcs.revision":
					if info.Commit == "" {
						info.Commit = setting.Value
						if len(info.Commit) > 12 {
							info.Commit = info.Commit[:12]
						}
					}
				case "vcs.time":
					if info.BuildDate == "" {
						info.BuildDate = setting.Value
					}
				}
			}
		}
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

func printVersion(w io.Writer, program string) {
	info := resolveBuildInfo()
	fmt.Fprintf(w, "%s %s (commit %s, built %s, %s)\n",
		program, info.Version, info.Commit, info.BuildDate, info.GoVersion)
}

func isVersionCommand(arg string) bool {
	switch arg {
	case "version", "-v", "--version":
		return true
	default:
		return false
	}
}

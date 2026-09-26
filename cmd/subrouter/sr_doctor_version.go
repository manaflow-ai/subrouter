package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/buildversion"
)

// doctorTeamPlistGlob finds team LaunchDaemons; tests point it at a temp dir.
var doctorTeamPlistGlob = teamLaunchDaemonGlob

// doctorVersionChecks compares this CLI's build with the daemon's (from
// /_subrouter/health) and, on a team host, reports whether worker autoupdate
// is pinned. Neither is blocking: a skewed or pinned host still routes.
func doctorVersionChecks(ctx context.Context, baseURL string) []doctorCheck {
	program := programBase()
	cli := buildversion.Version()
	teamPlists, _ := filepath.Glob(doctorTeamPlistGlob)
	fix := fmt.Sprintf("run '%s update'", program)
	if len(teamPlists) > 0 {
		fix = "on this team host run 'sudo subrouter-deploy.sh install-release <version>' or let autoupdate catch up"
	}

	var checks []doctorCheck
	probe := &updater{healthBaseURL: baseURL}
	health, ok := probe.daemonHealth(ctx)
	daemon := health.Version
	switch {
	case !ok:
		checks = append(checks, doctorCheck{"ok", "version", fmt.Sprintf("CLI %s (daemon not answering)", displayVersion(cli))})
	case daemon == "":
		checks = append(checks, doctorCheck{"warn", "version", fmt.Sprintf("CLI %s; the daemon predates version reporting; %s", displayVersion(cli), fix)})
	case sameVersion(cli, daemon):
		checks = append(checks, doctorCheck{"ok", "version", fmt.Sprintf("%s (CLI and daemon)", displayVersion(cli))})
	default:
		checks = append(checks, doctorCheck{"warn", "version", fmt.Sprintf("CLI %s, daemon %s; %s", displayVersion(cli), displayVersion(daemon), fix)})
	}
	if text := releaseStatusText(health.Release, time.Now()); ok && text != "" {
		// A supervised team host reports its post-upgrade bake. A rollback
		// is the one state a human should look at.
		status := "ok"
		if health.Release.State == "rolled_back" {
			status = "warn"
			text += " ('sudo subrouter-deploy.sh status' shows details; 'sudo subrouter-deploy.sh unpin' resumes autoupdate)"
		}
		checks = append(checks, doctorCheck{status, "release", text})
	}

	for _, plist := range teamPlists {
		label := strings.TrimSuffix(filepath.Base(plist), ".plist")
		sentinel := plist + ".supervisor-transaction/upgrade-inhibited"
		if _, err := os.Lstat(sentinel); err != nil {
			checks = append(checks, doctorCheck{"ok", "autoupdate", fmt.Sprintf("%s installs new releases", label)})
			continue
		}
		reason := firstLine(sentinel)
		switch {
		case strings.HasPrefix(reason, "pinned at "):
			checks = append(checks, doctorCheck{"ok", "autoupdate", fmt.Sprintf("%s %s", label, strings.SplitN(reason, ";", 2)[0])})
		case reason == "":
			checks = append(checks, doctorCheck{"warn", "autoupdate", fmt.Sprintf("%s paused by %s; 'sudo subrouter-deploy.sh list' shows why", label, sentinel)})
		default:
			checks = append(checks, doctorCheck{"warn", "autoupdate", fmt.Sprintf("%s paused: %s ('sudo subrouter-deploy.sh unpin' resumes it)", label, reason)})
		}
	}
	return checks
}

// firstLine returns the first line of path, or "" when it cannot be read (the
// team sentinel is root-only).
func firstLine(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text())
	}
	return ""
}

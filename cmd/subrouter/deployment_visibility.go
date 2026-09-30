package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/buildversion"
)

type buildVisibilityView struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	Mainline  string `json:"mainline"`
}

type recoveryCountersView struct {
	RetriesHeld          int64 `json:"retries_held"`
	PersistentRetries    int64 `json:"persistent_retries"`
	Retryable503Handoffs int64 `json:"retryable_503_handoffs"`
	Exhausted            int64 `json:"exhausted"`
}

func (r srRunner) printDeploymentVisibilityStatus(ctx context.Context, server srServerConfig) {
	baseURL, err := serverControlBaseURL(server)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/_subrouter/health", nil)
	if err != nil {
		return
	}
	addServerAdminAuth(req, server)
	client, err := r.securedRequestClientForServer(server, baseURL, 10*time.Second)
	if err != nil {
		return
	}
	res, err := client.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return
	}
	var health struct {
		Version  string                          `json:"version"`
		Build    buildVisibilityView             `json:"build"`
		Release  *releaseStateView               `json:"release"`
		Recovery map[string]recoveryCountersView `json:"recovery_counters"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 256<<10)).Decode(&health); err != nil {
		return
	}
	clientInfo := buildversion.Get()
	serverVersion := health.Version
	if serverVersion == "" {
		serverVersion = health.Build.Version
	}
	if serverVersion == "" {
		serverVersion = "unknown"
	}
	fmt.Fprintf(r.out, "\nDeployment             server %s · client %s\n", serverVersion, clientInfo.Version)
	if text := releaseStatusText(health.Release, time.Now()); text != "" {
		fmt.Fprintln(r.out, "Rollout                "+text)
	}
	if health.Build.Commit != "" || health.Build.BuildDate != "" {
		provenance := health.Build.Mainline
		if provenance == "" {
			provenance = "unknown"
		}
		fmt.Fprintf(r.out, "Build                  %s · commit %s · built %s · %s\n", provenance, health.Build.Commit, health.Build.BuildDate, provenance)
	}
	serverLocal := func(p string) bool { return p == "unknown" || strings.Contains(p, "local") }
	switch {
	case !sameVersion(clientInfo.Version, serverVersion):
		fmt.Fprintf(r.out, "Warning                client is behind or differs from server; run '%s update'\n", r.programBase())
	case serverLocal(health.Build.Mainline):
		fmt.Fprintln(r.out, "Warning                server is a local or unpushed build; deploy a stamped release before promoting it")
	case serverLocal(clientInfo.Mainline):
		fmt.Fprintf(r.out, "Warning                client is a local or unpushed build; run '%s update'\n", r.programBase())
	}
	for provider, c := range health.Recovery {
		fmt.Fprintf(r.out, "Recovery (%s, 1h)       held %d · persistent %d · retryable-503 handoffs %d · exhausted %d\n", provider, c.RetriesHeld, c.PersistentRetries, c.Retryable503Handoffs, c.Exhausted)
	}
}

func (r srRunner) programBase() string {
	if strings.TrimSpace(r.program) == "" {
		return "sr"
	}
	return r.program
}

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/front"
)

type workerLaunchReport struct {
	Args []string `json:"args"`
	Env  string   `json:"env"`
}

func readWorkerLaunch(t *testing.T, generation *workerGeneration) workerLaunchReport {
	t.Helper()
	connection, err := net.DialTimeout(generation.network, generation.address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := front.WriteProxyProtocolHeader(connection, nil, nil); err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprint(connection, "GET /_subrouter/test-launch HTTP/1.0\r\nHost: worker\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var report workerLaunchReport
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	return report
}

func writeWorkerConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Every generation re-reads the worker config, so a flag or env change reaches
// the next worker through a hot upgrade instead of a LaunchDaemon restart.
func TestWorkerConfigIsReadForEveryGeneration(t *testing.T) {
	t.Setenv("SUBROUTER_TEST_FAKE_WORKER", "1")
	t.Setenv("SUBROUTER_TEST_LAUNCH_ENV", "from-supervisor")
	path := filepath.Join(t.TempDir(), "worker-config.json")
	config := supervisorConfig{
		WorkerBin: os.Args[0], ReadyTimeout: 10 * time.Second,
		WorkerConfig: path, WorkerArgs: []string{"--argv-flag"},
	}

	missing, err := startWorkerGeneration(config, generationInitial)
	if err != nil {
		t.Fatal(err)
	}
	report := readWorkerLaunch(t, missing)
	terminateWorker(missing, time.Second)
	if !reflect.DeepEqual(report.Args[3:], []string{"--argv-flag"}) || report.Env != "from-supervisor" {
		t.Fatalf("missing config launch = %+v, want argv args and inherited env", report)
	}

	writeWorkerConfig(t, path, `{"args":["serve","--bedrock","--region","us-east-1"],"env":{"SUBROUTER_TEST_LAUNCH_ENV":"from-file"}}`)
	replacement, err := startWorkerGeneration(config, generationReplacement)
	if err != nil {
		t.Fatal(err)
	}
	report = readWorkerLaunch(t, replacement)
	terminateWorker(replacement, time.Second)
	if want := []string{"--bedrock", "--region", "us-east-1"}; !reflect.DeepEqual(report.Args[3:], want) {
		t.Fatalf("file args = %q, want %q", report.Args[3:], want)
	}
	if report.Args[0] != "serve" || report.Args[1] != "--addr" {
		t.Fatalf("supervisor-owned prefix lost: %q", report.Args)
	}
	if report.Env != "from-file" {
		t.Fatalf("file env = %q, want from-file", report.Env)
	}
}

// A bad config on upgrade must leave the serving worker in place.
func TestInvalidWorkerConfigRefusesUpgradeAndKeepsActiveGeneration(t *testing.T) {
	for name, body := range map[string]string{
		"syntax":        `{"args":[`,
		"missing args":  `{"env":{}}`,
		"unknown field": `{"args":[],"argz":[]}`,
		"worker addr":   `{"args":["--addr","127.0.0.1:1"]}`,
		"owned env":     `{"args":[],"env":{"SUBROUTER_LISTEN_FD":"9"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "worker-config.json")
			writeWorkerConfig(t, path, body)
			router, err := front.NewRouter(front.Backend{ID: "initial", Network: "unix", Address: "/tmp/initial.sock"})
			if err != nil {
				t.Fatal(err)
			}
			s := &supervisor{
				config: supervisorConfig{WorkerBin: "/nonexistent/worker", ReadyTimeout: time.Second, WorkerConfig: path},
				router: router,
			}
			err = s.upgradeLocked()
			if err == nil || !strings.Contains(err.Error(), "worker config") {
				t.Fatalf("upgrade with invalid config = %v", err)
			}
			if active := router.Active().ID; active != "initial" {
				t.Fatalf("invalid config changed active generation to %q", active)
			}
		})
	}
}

// The initial generation must never leave the public port closed over a bad
// config file, so it falls back to the argv worker args.
func TestInitialGenerationFallsBackToArgvOnInvalidWorkerConfig(t *testing.T) {
	t.Setenv("SUBROUTER_TEST_FAKE_WORKER", "1")
	path := filepath.Join(t.TempDir(), "worker-config.json")
	writeWorkerConfig(t, path, `not json`)
	generation, err := startWorkerGeneration(supervisorConfig{
		WorkerBin: os.Args[0], ReadyTimeout: 10 * time.Second,
		WorkerConfig: path, WorkerArgs: []string{"--argv-flag"},
	}, generationInitial)
	if err != nil {
		t.Fatalf("initial generation refused over a bad config: %v", err)
	}
	defer terminateWorker(generation, time.Second)
	if report := readWorkerLaunch(t, generation); !reflect.DeepEqual(report.Args[3:], []string{"--argv-flag"}) {
		t.Fatalf("fallback args = %q", report.Args[3:])
	}
}

func TestParseSupervisorConfigRequiresAbsoluteWorkerConfig(t *testing.T) {
	config, err := parseSupervisorConfig([]string{
		"--addr", "127.0.0.1:31415", "--control-socket", "/tmp/sup.sock",
		"--worker-bin", "/usr/local/bin/subrouter", "--worker-config", "relative.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSupervisorConfig(config); err == nil || !strings.Contains(err.Error(), "worker-config must be an absolute path") {
		t.Fatalf("relative worker config validation = %v", err)
	}
}

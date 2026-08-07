package main

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGoldenLocalMacHarnessOrchestratesAllModesWithoutContentEvidence(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a deterministic fake released client")
	}
	root := t.TempDir()
	requestState := filepath.Join(root, "request-state")
	if err := os.Mkdir(requestState, 0o700); err != nil {
		t.Fatal(err)
	}
	streamReleaseState := filepath.Join(root, "stream-release-state")
	if err := os.Mkdir(streamReleaseState, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(goldenRequestStateEnv, requestState)
	fakeClient := filepath.Join(root, "released-subrouter")
	build := exec.Command("go", "build", "-o", fakeClient, "./testdata/golden_fake_client")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake released client: %v\n%s", err, output)
	}
	fakeClientData, err := os.ReadFile(fakeClient)
	if err != nil {
		t.Fatal(err)
	}
	fakeClientHash := sha256.Sum256(fakeClientData)
	assetName := "subrouter_9.9.9_darwin_arm64"
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/latest":
			_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
		case "/download/v9.9.9/" + assetName:
			_, _ = w.Write(fakeClientData)
		case "/download/v9.9.9/SHA256SUMS":
			_, _ = fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(fakeClientHash[:]), assetName)
		default:
			http.NotFound(w, request)
		}
	}))
	defer release.Close()

	actionLog := filepath.Join(root, "actions.log")
	hosted := httptest.NewServer(goldenFakeHostedHandler(func() bool {
		actions, err := os.ReadFile(actionLog)
		return err == nil && strings.Contains(string(actions), "legacy-cleanup\n")
	}))
	defer hosted.Close()
	cloudPath := filepath.Join(root, "cloud.json")
	tenantKey := "srt_0123456789abcdef0123456789abcdef"
	cloud := map[string]any{
		"version": 1, "baseUrl": hosted.URL,
		"accessToken": "ACCESS_TOKEN_SECRET", "refreshToken": "REFRESH_TOKEN_SECRET",
		"localProxyToken": "LOCAL_PROXY_SECRET", "teamId": "team-golden", "teamName": "Golden",
		"credentialSource": "team", "hostedUrl": hosted.URL, "tenantKey": tenantKey,
	}
	data, _ := json.Marshal(cloud)
	if err := os.WriteFile(cloudPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(root, "codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"access_token":"CODEX_AUTH_SECRET"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeBin := filepath.Join(root, "bin")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	lsofPath := filepath.Join(fakeBin, "lsof")
	lsofScript := `#!/bin/sh
pid=0
previous=
for value in "$@"; do
  if [ "$previous" = "-p" ]; then pid="$value"; fi
  previous="$value"
done
if [ -n "${SUBROUTER_GOLDEN_FAKE_DAEMON_PID:-}" ] && [ -r "${SUBROUTER_GOLDEN_FAKE_DAEMON_PID}" ] &&
   [ "$pid" = "$(cat "${SUBROUTER_GOLDEN_FAKE_DAEMON_PID}")" ]; then
  printf 'p%s\n' "$pid"
  if [ -s "${SUBROUTER_GOLDEN_FAKE_SOCKET_STATE}" ]; then
    while IFS= read -r socket; do
      [ -n "$socket" ] && printf 'f9\nn%s\nTST=ESTABLISHED\n' "$socket"
    done <"${SUBROUTER_GOLDEN_FAKE_SOCKET_STATE}"
  fi
  exit 0
fi
printf 'p%s\nf9\nn127.0.0.1:41000->203.0.113.10:443\nTST=ESTABLISHED\n' "$pid"
`
	if err := os.WriteFile(lsofPath, []byte(lsofScript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "pgrep"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	psScript := `#!/bin/sh
state_dir=${SUBROUTER_GOLDEN_FAKE_PROCESS_STATE:-}
if [ "${1:-}" = "-axo" ]; then
  for record in "$state_dir"/*; do
    [ -f "$record" ] && /bin/cat "$record"
  done
  exit 0
fi
pids=
format=
previous=
for value in "$@"; do
  if [ "$previous" = "-p" ]; then pids=$value; fi
  if [ "$previous" = "-o" ]; then format=$value; fi
  previous=$value
done
case "$format" in
  state=) printf 'S\n' ;;
  rss=) printf '1024\n' ;;
  pid=,ppid=,state=,rss=)
    old_ifs=$IFS
    IFS=,
    for pid in $pids; do
      record="$state_dir/$pid"
      if [ -f "$record" ]; then
        /bin/cat "$record"
      else
        printf '%s 1 S 1024\n' "$pid"
      fi
    done
    IFS=$old_ifs
    ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(fakeBin, "ps"), []byte(psScript), 0o700); err != nil {
		t.Fatal(err)
	}
	processState := filepath.Join(root, "process-state")
	if err := os.Mkdir(processState, 0o700); err != nil {
		t.Fatal(err)
	}
	observerRecord := fmt.Sprintf("%d %d S 1024\n", os.Getpid(), os.Getppid())
	if err := os.WriteFile(filepath.Join(processState, strconv.Itoa(os.Getpid())), []byte(observerRecord), 0o600); err != nil {
		t.Fatal(err)
	}
	daemonPIDPath := filepath.Join(root, "daemon-pid")
	daemonAddressPath := filepath.Join(root, "daemon-address")
	enableGoldenTestMode(t, release.URL+"/latest", release.URL+"/download")
	goldenTestHooks.outboundRequestWritten = func(token string) error {
		return signalGoldenFakeRequestWritten(requestState, token)
	}
	goldenTestHooks.releaseStream = func(token string) error {
		return signalGoldenFakeStreamRelease(streamReleaseState, token)
	}
	goldenTestHooks.processTable = func(pids []int) (goldenProcessTable, error) {
		return loadGoldenFakeProcessTable(processState, pids)
	}
	goldenTestHooks.socketSnapshot = loadGoldenFakeSocketSnapshot
	goldenTestHooks.localDaemonListenAddr = func(_ context.Context, pid int) (string, error) {
		pidData, err := os.ReadFile(daemonPIDPath)
		if os.IsNotExist(err) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		registeredPID, parseErr := strconv.Atoi(strings.TrimSpace(string(pidData)))
		if parseErr != nil || registeredPID != pid {
			return "", nil
		}
		addressData, err := os.ReadFile(daemonAddressPath)
		if os.IsNotExist(err) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(addressData)), nil
	}
	goldenTestHooks.sessionProcessReady = func(ctx context.Context, process *os.Process) error {
		return waitGoldenFakeProcessRegistration(ctx, processState, process)
	}
	goldenTestHooks.sessionProcessDone = func(process *os.Process) {
		_ = os.Remove(filepath.Join(processState, strconv.Itoa(process.Pid)))
	}
	t.Setenv("DEPLOY_ENV_SECRET", "DEPLOY_ENV_VALUE_SECRET")
	t.Setenv("ACTION_LOG", actionLog)
	t.Setenv("FAKE_PREDECESSOR_SHA256", goldenPinnedBootstrapLinuxSHA256)
	t.Setenv("SUBROUTER_GOLDEN_FAKE_SOCKET_STATE", filepath.Join(root, "daemon-sockets"))
	t.Setenv("SUBROUTER_GOLDEN_FAKE_DAEMON_PID", daemonPIDPath)
	t.Setenv("SUBROUTER_GOLDEN_FAKE_DAEMON_ADDR", daemonAddressPath)
	t.Setenv(goldenFakeStreamReleaseStateEnv, streamReleaseState)
	t.Setenv("SUBROUTER_GOLDEN_FAKE_PROCESS_STATE", processState)
	t.Setenv("SUBROUTER_GOLDEN_FAKE_PROCESS_PARENT_OWNED", "1")
	t.Setenv("SUBROUTER_GOLDEN_FAKE_MIGRATION_RETRY_ONCE", "1")
	t.Setenv("SUBROUTER_GOLDEN_FAKE_REQUIRE_PREPARE_BEFORE_SESSIONS", "1")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	artifacts := filepath.Join(root, "artifacts")
	err = runGolden([]string{
		"--cloud-config", cloudPath,
		"--codex-home", codexHome,
		"--codex-bin", fakeClient,
		"--artifact-dir", artifacts,
		"--stream-lines", "8",
		"--timeout", "30s",
		"--migration-prepare", fakeClient, "action", "migration-prepare", "10ms",
		"--migration-switch", fakeClient, "action", "migration-switch", "100ms",
		"--legacy-retirement", fakeClient, "action", "legacy-cleanup", "10ms",
		"--activate", fakeClient, "action", "activation", "400ms",
		"--rollback", fakeClient, "action", "rollback", "300ms",
		"--old-generation-check", fakeClient, "action", "cleanup", "10ms",
	})
	if err != nil {
		result, _ := os.ReadFile(filepath.Join(artifacts, "result.json"))
		t.Fatalf("golden harness: %v\n%s", err, result)
	}
	actions, err := os.ReadFile(actionLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(actions) != "migration-prepare\nmigration-switch\nlegacy-cleanup\nactivation\nrollback\ncleanup\nactivation\ncleanup\n" {
		t.Fatalf("actions = %q", actions)
	}
	resultData, err := os.ReadFile(filepath.Join(artifacts, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result goldenSummary
	if err := json.Unmarshal(resultData, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Passed || !result.PrivateWorkspaceRemoved || !result.FreshLocalLeaseObserved ||
		!result.ReleaseChecksumVerified || result.ReleasedVersion != "9.9.9" || len(result.Sessions) != 15 {
		t.Fatalf("incomplete result: %#v", result)
	}
	retryAccepted := false
	rejectedPresent := false
	for _, session := range result.Sessions {
		retryAccepted = retryAccepted || session.Label == "migration-candidate-front-final-destination-direct-attempt-2"
		rejectedPresent = rejectedPresent || session.Label == "migration-candidate-front-final-destination-direct"
	}
	if !retryAccepted || rejectedPresent {
		t.Fatalf("retry summary accepted=%t rejected_present=%t", retryAccepted, rejectedPresent)
	}
	allEvidence := readGoldenArtifacts(t, artifacts)
	for _, forbidden := range []string{
		"ACCESS_TOKEN_SECRET", "REFRESH_TOKEN_SECRET", "LOCAL_PROXY_SECRET", "CODEX_AUTH_SECRET",
		"DEPLOY_ENV_VALUE_SECRET", "REQUEST_BODY_SECRET", "REQUEST_HEADER_SECRET",
		"LEASE_REQUEST_BODY_SECRET", "LEASE_HEADER_SECRET", tenantKey,
		"healthy-response-secret", "response-body-not-recorded",
		"Do not use tools", "exact nonce from the first turn",
	} {
		if strings.Contains(allEvidence, forbidden) {
			t.Fatalf("content-blind evidence leaked %q", forbidden)
		}
	}
}

func TestGoldenPredecessorDirectConfigUsesLegacyCredentialSource(t *testing.T) {
	if got := goldenPredecessorDirectCredentialSource(); got != "legacy" {
		t.Fatalf("predecessor direct credential source = %q, want legacy", got)
	}
}

func loadGoldenFakeProcessTable(directory string, pids []int) (goldenProcessTable, error) {
	requested := make(map[int]bool, len(pids))
	for _, pid := range pids {
		requested[pid] = true
	}
	newTable := func() (goldenProcessTable, func(string, []byte, int) error) {
		processes := make(map[int]goldenProcessSample)
		children := make(map[int][]int)
		table := goldenProcessTable{processes: processes, children: children}
		add := func(name string, data []byte, expectedPID int) error {
			fields := strings.Fields(string(data))
			if len(fields) != 4 {
				return fmt.Errorf("invalid fake process record %q", name)
			}
			pid, pidErr := strconv.Atoi(fields[0])
			parent, parentErr := strconv.Atoi(fields[1])
			rssKiB, rssErr := strconv.ParseInt(fields[3], 10, 64)
			if pidErr != nil || parentErr != nil || rssErr != nil || pid <= 0 || rssKiB <= 0 ||
				(expectedPID > 0 && pid != expectedPID) {
				return fmt.Errorf("invalid fake process record %q", name)
			}
			processes[pid] = goldenProcessSample{parent: parent, state: fields[2], rss: rssKiB * 1024}
			children[parent] = append(children[parent], pid)
			return nil
		}
		return table, add
	}
	if len(requested) != 0 {
		table, add := newTable()
		for pid := range requested {
			name := strconv.Itoa(pid)
			data, err := os.ReadFile(filepath.Join(directory, name))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return goldenProcessTable{}, err
			}
			if err := add(name, data, pid); err != nil {
				return goldenProcessTable{}, err
			}
		}
		return table, nil
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return goldenProcessTable{}, err
	}
	table, add := newTable()
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			return goldenProcessTable{}, err
		}
		process, err := os.FindProcess(pid)
		if err != nil || !processAlive(process) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return goldenProcessTable{}, err
		}
		if err := add(entry.Name(), data, 0); err != nil {
			return goldenProcessTable{}, err
		}
	}
	return table, nil
}

func waitGoldenFakeProcessRegistration(ctx context.Context, directory string, process *os.Process) error {
	if process == nil {
		return errors.New("missing fake process")
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		table, err := loadGoldenFakeProcessTable(directory, []int{process.Pid})
		if err != nil {
			return err
		}
		if _, ok := table.processes[process.Pid]; ok {
			return nil
		}
		if !processAlive(process) {
			return fmt.Errorf("fake process %d exited before registration", process.Pid)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func loadGoldenFakeSocketSnapshot(pid int) ([]byte, error) {
	daemonPIDData, err := os.ReadFile(strings.TrimSpace(os.Getenv("SUBROUTER_GOLDEN_FAKE_DAEMON_PID")))
	if err == nil && strings.TrimSpace(string(daemonPIDData)) == strconv.Itoa(pid) {
		socketData, err := os.ReadFile(strings.TrimSpace(os.Getenv("SUBROUTER_GOLDEN_FAKE_SOCKET_STATE")))
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		var output strings.Builder
		for _, socket := range strings.Fields(string(socketData)) {
			fmt.Fprintf(&output, "n%s\n", socket)
		}
		return []byte(output.String()), nil
	}
	return []byte("n127.0.0.1:41000->203.0.113.10:443\n"), nil
}

func goldenFakeHostedHandler(leasesReady func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_subrouter/health" || request.URL.Path == "/_subrouter/ready" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("healthy-response-secret"))
			return
		}
		if request.URL.Path == "/api/subrouter/leases" || strings.HasSuffix(request.URL.Path, "/_subrouter/leases") {
			if leasesReady != nil && !leasesReady() {
				http.NotFound(w, request)
				return
			}
			_, _ = io.Copy(io.Discard, request.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"lease":"response-body-not-recorded"}`))
			return
		}
		if !strings.HasSuffix(request.URL.Path, "/responses") {
			http.NotFound(w, request)
			return
		}
		if err := discardGoldenFakeRequestBody(request.Body); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		token, err := goldenRequestToken(request.Header)
		if err != nil {
			http.Error(w, "invalid request token", http.StatusBadRequest)
			return
		}
		if err := waitForGoldenFakeRequestWritten(request.Context(), os.Getenv(goldenRequestStateEnv), token); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errGoldenFakeRequestSignalTimeout) {
				status = http.StatusGatewayTimeout
			} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusRequestTimeout
			}
			http.Error(w, "request completion unavailable", status)
			return
		}
		goldenFakeStream(w, request)
	})
}

const (
	goldenFakeRequestBodyLimit    = 1 << 20
	goldenFakeRequestPollInterval = 2 * time.Millisecond
)

var (
	goldenFakeRequestWaitTimeout      = 2 * time.Second
	errGoldenFakeRequestSignalTimeout = errors.New("golden request completion signal timed out")
)

func discardGoldenFakeRequestBody(body io.Reader) error {
	if body == nil {
		return nil
	}
	count, err := io.CopyN(io.Discard, body, goldenFakeRequestBodyLimit+1)
	if count > goldenFakeRequestBodyLimit {
		return errors.New("request body exceeds fake handler limit")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func signalGoldenFakeRequestWritten(stateDir, token string) error {
	if !validGoldenRequestToken(token) {
		return errors.New("invalid golden request token")
	}
	info, err := os.Stat(stateDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("golden request state is not a directory")
	}
	temporary, err := os.CreateTemp(stateDir, ".request-written-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.WriteString("written\n"); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	// Closing completes the contents before the hard link atomically publishes
	// this inode under the opaque request token.
	return os.Link(temporaryPath, filepath.Join(stateDir, token))
}

func waitForGoldenFakeRequestWritten(ctx context.Context, stateDir, token string) error {
	if !validGoldenRequestToken(token) {
		return errors.New("invalid golden request token")
	}
	info, err := os.Stat(stateDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("golden request state is not a directory")
	}
	if goldenFakeRequestWaitStarted != nil {
		goldenFakeRequestWaitStarted(token)
	}
	timeout := time.NewTimer(goldenFakeRequestWaitTimeout)
	defer timeout.Stop()
	ticker := time.NewTicker(goldenFakeRequestPollInterval)
	defer ticker.Stop()
	for {
		ready, err := consumeGoldenFakeRequestWritten(stateDir, token)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return errGoldenFakeRequestSignalTimeout
		case <-ticker.C:
		}
	}
}

func consumeGoldenFakeRequestWritten(stateDir, token string) (bool, error) {
	path := filepath.Join(stateDir, token)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if string(data) != "written\n" {
		return false, errors.New("invalid golden request completion signal")
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
}

type goldenBlockingRequestBody struct {
	data      []byte
	waiting   chan struct{}
	release   chan struct{}
	exhausted bool
}

func (body *goldenBlockingRequestBody) Read(buffer []byte) (int, error) {
	if len(body.data) != 0 {
		count := copy(buffer, body.data)
		body.data = body.data[count:]
		return count, nil
	}
	select {
	case <-body.waiting:
	default:
		close(body.waiting)
	}
	<-body.release
	body.exhausted = true
	return 0, io.EOF
}

func (*goldenBlockingRequestBody) Close() error { return nil }

type goldenOrderedResponseRecorder struct {
	*httptest.ResponseRecorder
	firstWrite chan struct{}
	once       sync.Once
}

func configureGoldenFakeStreamRelease(t *testing.T, request *http.Request) {
	t.Helper()
	t.Setenv(goldenFakeStreamReleaseStateEnv, t.TempDir())
	request.Header.Set(goldenFakeStreamReleaseTokenHeader, "abcdefabcdefabcdefabcdefabcdefab")
}

func (recorder *goldenOrderedResponseRecorder) Write(data []byte) (int, error) {
	recorder.once.Do(func() { close(recorder.firstWrite) })
	return recorder.ResponseRecorder.Write(data)
}

var goldenFakeRequestWaitStarted func(string)

func TestGoldenFakeHostedHandlerConsumesRequestBodyBeforeStreaming(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv(goldenRequestStateEnv, stateDir)
	token := "0123456789abcdef0123456789abcdef"
	if err := signalGoldenFakeRequestWritten(stateDir, token); err != nil {
		t.Fatal(err)
	}
	body := &goldenBlockingRequestBody{
		data: []byte("REQUEST_BODY_SECRET"), waiting: make(chan struct{}), release: make(chan struct{}),
	}
	recorder := &goldenOrderedResponseRecorder{
		ResponseRecorder: httptest.NewRecorder(), firstWrite: make(chan struct{}),
	}
	request := httptest.NewRequest(http.MethodPost, "http://host.test/v1/responses", body)
	request.Header.Set("X-Golden-Short", "1")
	request.Header.Set(goldenRequestTokenHeader, token)
	configureGoldenFakeStreamRelease(t, request)
	done := make(chan struct{})
	go func() {
		defer close(done)
		goldenFakeHostedHandler(nil).ServeHTTP(recorder, request)
	}()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(body.release) }) }
	t.Cleanup(func() {
		release()
		<-done
	})

	select {
	case <-body.waiting:
	case <-recorder.firstWrite:
		t.Fatal("response streaming started before the request body was consumed")
	case <-time.After(time.Second):
		t.Fatal("handler did not consume the request body")
	}
	select {
	case <-recorder.firstWrite:
		t.Fatal("response streaming started while the request body read was blocked")
	default:
	}
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish after the request body was released")
	}
	if !body.exhausted {
		t.Fatal("handler did not read the complete request body")
	}
	select {
	case <-recorder.firstWrite:
	default:
		t.Fatal("handler did not stream a response after consuming the request body")
	}
}

type goldenFailingRequestBody struct{}

func (goldenFailingRequestBody) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

func TestGoldenFakeHostedHandlerRejectsInvalidRequestBodies(t *testing.T) {
	tests := []struct {
		name string
		body io.Reader
	}{
		{name: "read failure", body: goldenFailingRequestBody{}},
		{name: "oversized", body: strings.NewReader(strings.Repeat("x", goldenFakeRequestBodyLimit+1))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://host.test/v1/responses", test.body)
			request.Header.Set("X-Golden-Short", "1")
			recorder := httptest.NewRecorder()
			goldenFakeHostedHandler(nil).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			if strings.Contains(recorder.Body.String(), "data:x") {
				t.Fatal("handler streamed after rejecting the request body")
			}
		})
	}
}

func TestGoldenFakeHostedHandlerWaitsForSenderCompletionAfterBodyEOF(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv(goldenRequestStateEnv, stateDir)
	token := "0123456789abcdef0123456789abcdef"
	body := &goldenBlockingRequestBody{
		data: []byte("REQUEST_BODY_SECRET"), waiting: make(chan struct{}), release: make(chan struct{}),
	}
	recorder := &goldenOrderedResponseRecorder{
		ResponseRecorder: httptest.NewRecorder(), firstWrite: make(chan struct{}),
	}
	request := httptest.NewRequest(http.MethodPost, "http://host.test/v1/responses", body)
	request.Header.Set("X-Golden-Short", "1")
	request.Header.Set(goldenRequestTokenHeader, token)
	configureGoldenFakeStreamRelease(t, request)
	waitStarted := make(chan struct{})
	previousWaitStarted := goldenFakeRequestWaitStarted
	goldenFakeRequestWaitStarted = func(got string) {
		if got == token {
			close(waitStarted)
		}
	}
	t.Cleanup(func() { goldenFakeRequestWaitStarted = previousWaitStarted })
	done := make(chan struct{})
	go func() {
		defer close(done)
		goldenFakeHostedHandler(nil).ServeHTTP(recorder, request)
	}()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(body.release) }) }
	t.Cleanup(func() {
		release()
		<-done
	})

	select {
	case <-body.waiting:
	case <-recorder.firstWrite:
		t.Fatal("response streaming started before body EOF")
	case <-time.After(time.Second):
		t.Fatal("handler did not consume the request body")
	}
	release()
	select {
	case <-waitStarted:
	case <-recorder.firstWrite:
		t.Fatal("body EOF permitted streaming before sender completion")
	case <-time.After(time.Second):
		t.Fatal("handler did not enter the sender-completion rendezvous")
	}
	select {
	case <-recorder.firstWrite:
		t.Fatal("handler streamed while sender completion was absent")
	default:
	}
	if err := signalGoldenFakeRequestWritten(stateDir, token); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish after sender completion")
	}
	select {
	case <-recorder.firstWrite:
	default:
		t.Fatal("handler did not stream after sender completion")
	}
}

func TestGoldenFakeHostedHandlerRejectsInvalidSenderCompletion(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv(goldenRequestStateEnv, stateDir)
	for _, test := range []struct {
		name  string
		token string
	}{
		{name: "missing token"},
		{name: "invalid token", token: "../not-opaque"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://host.test/v1/responses", strings.NewReader("body"))
			if test.token != "" {
				request.Header.Set(goldenRequestTokenHeader, test.token)
			}
			recorder := httptest.NewRecorder()
			goldenFakeHostedHandler(nil).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			if strings.Contains(recorder.Body.String(), "data:x") {
				t.Fatal("handler streamed after rejecting sender completion")
			}
		})
	}
}

func TestGoldenFakeHostedHandlerTimesOutWithoutSenderCompletion(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv(goldenRequestStateEnv, stateDir)
	previousTimeout := goldenFakeRequestWaitTimeout
	goldenFakeRequestWaitTimeout = 20 * time.Millisecond
	t.Cleanup(func() { goldenFakeRequestWaitTimeout = previousTimeout })
	request := httptest.NewRequest(http.MethodPost, "http://host.test/v1/responses", strings.NewReader("body"))
	request.Header.Set(goldenRequestTokenHeader, "0123456789abcdef0123456789abcdef")
	recorder := httptest.NewRecorder()
	goldenFakeHostedHandler(nil).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusGatewayTimeout)
	}
	if strings.Contains(recorder.Body.String(), "data:x") {
		t.Fatal("handler streamed after the sender-completion timeout")
	}
}

func TestSignalGoldenFakeRequestWrittenRejectsDuplicateAndInvalidTokens(t *testing.T) {
	stateDir := t.TempDir()
	token := "0123456789abcdef0123456789abcdef"
	if err := signalGoldenFakeRequestWritten(stateDir, token); err != nil {
		t.Fatal(err)
	}
	if err := signalGoldenFakeRequestWritten(stateDir, token); err == nil {
		t.Fatal("duplicate signal unexpectedly replaced existing state")
	}
	if err := signalGoldenFakeRequestWritten(stateDir, "../outside"); err == nil {
		t.Fatal("invalid token unexpectedly created state")
	}
}

const goldenFakeStreamReleaseTokenHeader = "X-Subrouter-Golden-Stream-Release-Token"

func goldenFakeStreamReleaseToken(header http.Header) (string, error) {
	values := header.Values(goldenFakeStreamReleaseTokenHeader)
	if len(values) != 1 || !validGoldenRequestToken(values[0]) {
		return "", errors.New("invalid golden stream release token")
	}
	return values[0], nil
}

func validateGoldenFakeStreamReleaseState(stateDir string) error {
	info, err := os.Stat(stateDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("golden stream release state is not a directory")
	}
	return nil
}

func publishGoldenFakeStreamState(stateDir, name, content string) error {
	if err := validateGoldenFakeStreamReleaseState(stateDir); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(stateDir, ".stream-state-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.WriteString(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Link(temporaryPath, filepath.Join(stateDir, name))
}

func claimGoldenFakeStreamRelease(stateDir, token string) error {
	if !validGoldenRequestToken(token) {
		return errors.New("invalid golden stream release token")
	}
	if err := validateGoldenFakeStreamReleaseState(stateDir); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(stateDir, "release-"+token)); err == nil || !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("golden stream was released before claim")
		}
		return err
	}
	return publishGoldenFakeStreamState(stateDir, "claim-"+token, "claimed\n")
}

func signalGoldenFakeStreamRelease(stateDir, token string) error {
	if !validGoldenRequestToken(token) {
		return errors.New("invalid golden stream release token")
	}
	if err := validateGoldenFakeStreamReleaseState(stateDir); err != nil {
		return err
	}
	claim, err := os.ReadFile(filepath.Join(stateDir, "claim-"+token))
	if err != nil {
		return err
	}
	if string(claim) != "claimed\n" {
		return errors.New("invalid golden stream claim")
	}
	return publishGoldenFakeStreamState(stateDir, "release-"+token, "released\n")
}

func goldenFakeStreamReleased(stateDir, token string) (bool, error) {
	if !validGoldenRequestToken(token) {
		return false, errors.New("invalid golden stream release token")
	}
	if err := validateGoldenFakeStreamReleaseState(stateDir); err != nil {
		return false, err
	}
	release, err := os.ReadFile(filepath.Join(stateDir, "release-"+token))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if string(release) != "released\n" {
		return false, errors.New("invalid golden stream release")
	}
	return true, nil
}

func TestGoldenFakeStreamReleaseIsScopedToOwningSession(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv(goldenFakeStreamReleaseStateEnv, stateDir)
	sourceToken := "11111111111111111111111111111111"
	destinationToken := "22222222222222222222222222222222"
	source, err := newGoldenFakeStreamLifetime(false, http.Header{goldenFakeStreamReleaseTokenHeader: {sourceToken}})
	if err != nil {
		t.Fatal(err)
	}
	destination, err := newGoldenFakeStreamLifetime(false, http.Header{goldenFakeStreamReleaseTokenHeader: {destinationToken}})
	if err != nil {
		t.Fatal(err)
	}
	if err := signalGoldenFakeStreamRelease(stateDir, sourceToken); err != nil {
		t.Fatal(err)
	}
	if source.keepOpen() {
		t.Fatal("source stream remained open after its release")
	}
	if !destination.keepOpen() {
		t.Fatal("destination stream inherited the source release")
	}
	if err := signalGoldenFakeStreamRelease(stateDir, destinationToken); err != nil {
		t.Fatal(err)
	}
}

func TestGoldenFakeStreamReleaseFailsClosedForInvalidAndDuplicateTokens(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv(goldenFakeStreamReleaseStateEnv, stateDir)
	if _, err := newGoldenFakeStreamLifetime(false, http.Header{}); err == nil {
		t.Fatal("missing release token was accepted")
	}
	if _, err := newGoldenFakeStreamLifetime(false, http.Header{goldenFakeStreamReleaseTokenHeader: {"../outside"}}); err == nil {
		t.Fatal("invalid release token was accepted")
	}
	unclaimed := "33333333333333333333333333333333"
	if err := signalGoldenFakeStreamRelease(stateDir, unclaimed); err == nil {
		t.Fatal("unclaimed release token was signaled")
	}
	token := "44444444444444444444444444444444"
	header := http.Header{goldenFakeStreamReleaseTokenHeader: {token}}
	if _, err := newGoldenFakeStreamLifetime(false, header); err != nil {
		t.Fatal(err)
	}
	if _, err := newGoldenFakeStreamLifetime(false, header); err == nil {
		t.Fatal("duplicate release token was claimed")
	}
	if err := signalGoldenFakeStreamRelease(stateDir, token); err != nil {
		t.Fatal(err)
	}
	if err := signalGoldenFakeStreamRelease(stateDir, token); err == nil {
		t.Fatal("duplicate release signal replaced existing state")
	}
}

func goldenFakeStream(w http.ResponseWriter, request *http.Request) {
	lifetime, err := newGoldenFakeStreamLifetime(request.Header.Get("X-Golden-Short") == "1", request.Header)
	if err != nil {
		http.Error(w, "invalid stream release", http.StatusBadRequest)
		return
	}
	if strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
		connection, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer connection.Close()
		digest := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = fmt.Fprintf(buffered, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(digest[:]))
		_ = buffered.Flush()
		for lifetime.keepOpen() {
			if _, err := connection.Write([]byte{0x81, 0x01, 'x'}); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	for lifetime.keepOpen() {
		if _, err := w.Write([]byte("data:x\n\n")); err != nil {
			return
		}
		flusher.Flush()
		time.Sleep(20 * time.Millisecond)
	}
}

type goldenFakeStreamLifetime struct {
	stateDir string
	token    string
	deadline time.Time
}

func newGoldenFakeStreamLifetime(short bool, header http.Header) (*goldenFakeStreamLifetime, error) {
	token, err := goldenFakeStreamReleaseToken(header)
	if err != nil {
		return nil, err
	}
	stateDir := strings.TrimSpace(os.Getenv(goldenFakeStreamReleaseStateEnv))
	if err := claimGoldenFakeStreamRelease(stateDir, token); err != nil {
		return nil, err
	}
	lifetime := &goldenFakeStreamLifetime{stateDir: stateDir, token: token}
	if short {
		lifetime.deadline = time.Now().Add(120 * time.Millisecond)
	}
	return lifetime, nil
}

func (lifetime *goldenFakeStreamLifetime) keepOpen() bool {
	if !lifetime.deadline.IsZero() {
		return time.Now().Before(lifetime.deadline)
	}
	released, err := goldenFakeStreamReleased(lifetime.stateDir, lifetime.token)
	return err == nil && !released
}

func readGoldenArtifacts(t *testing.T, root string) string {
	t.Helper()
	var output strings.Builder
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			output.Write(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func TestObserverTemplatesTenantAndLeasePathsAndRecordsOnlyByteMetadata(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		_, _ = w.Write([]byte("RESPONSE_BODY_SECRET"))
	}))
	defer upstream.Close()
	parsed, _ := url.Parse(upstream.URL)
	var events strings.Builder
	handler := newObserverHandler(parsed, &events)
	request := httptest.NewRequest(http.MethodPost, "http://observer/t/srt_TENANT_SECRET/_subrouter/leases/LEASE_SECRET/events", strings.NewReader("REQUEST_BODY_SECRET"))
	request.Header.Set("Authorization", "Bearer HEADER_SECRET")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	evidence := events.String()
	for _, required := range []string{`"path":"/_subrouter/leases/:id/events"`, `"kind":"request_chunk"`, `"kind":"response_chunk"`, `"connection_id":"`} {
		if !strings.Contains(evidence, required) {
			t.Fatalf("evidence missing %q:\n%s", required, evidence)
		}
	}
	for _, forbidden := range []string{"srt_TENANT_SECRET", "LEASE_SECRET", "REQUEST_BODY_SECRET", "RESPONSE_BODY_SECRET", "HEADER_SECRET", "Authorization"} {
		if strings.Contains(evidence, forbidden) {
			t.Fatalf("evidence leaked %q:\n%s", forbidden, evidence)
		}
	}
}

func TestGoldenSessionValidationRejectsEveryContinuityFailureClass(t *testing.T) {
	newSession := func() *goldenSession {
		return &goldenSession{
			threadID: "thread", threadIDCount: 1, marker: "marker", markerCount: 1,
			nonce: "nonce", nonceCount: 1, issues: map[string]int{}, exitCode: 0,
			peakRSSBytes: 1 << 20, rssSamples: 1,
		}
	}
	tests := []struct {
		name string
		edit func(*goldenSession)
		want string
	}{
		{name: "nonzero", edit: func(s *goldenSession) { s.exitCode = 7 }, want: "codex_nonzero_exit"},
		{name: "duplicate marker", edit: func(s *goldenSession) { s.markerCount = 2 }, want: "duplicate_completion_marker"},
		{name: "missing marker", edit: func(s *goldenSession) { s.markerCount = 0 }, want: "completion_marker_missing"},
		{name: "missing nonce", edit: func(s *goldenSession) { s.nonceCount = 0 }, want: "nonce_context_missing"},
		{name: "reconnect", edit: func(s *goldenSession) { s.issues["reconnect"] = 1 }, want: "codex_transport_issue_reconnect"},
		{name: "retry", edit: func(s *goldenSession) { s.issues["retry"] = 1 }, want: "codex_transport_issue_retry"},
		{name: "fallback", edit: func(s *goldenSession) { s.issues["fallback"] = 1 }, want: "codex_transport_issue_fallback"},
		{name: "error", edit: func(s *goldenSession) { s.issues["error"] = 1 }, want: "codex_transport_issue_error"},
		{name: "process sampling gap", edit: func(s *goldenSession) { s.maxProcessSampleGap = goldenProcessSampleMaxGap + time.Millisecond }, want: "process_sampling_gap"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := newSession()
			test.edit(session)
			if got := fixedGoldenFailure(validateGoldenSessions([]*goldenSession{session}, false)); got != test.want {
				t.Fatalf("failure = %q, want %q", got, test.want)
			}
		})
	}
}

func TestGoldenReleasedClientRejectsChecksumMismatch(t *testing.T) {
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/latest":
			_, _ = w.Write([]byte(`{"tag_name":"v1.2.3"}`))
		case "/download/v1.2.3/subrouter_1.2.3_darwin_arm64":
			_, _ = w.Write([]byte("fake-binary"))
		case "/download/v1.2.3/SHA256SUMS":
			_, _ = w.Write([]byte(strings.Repeat("0", 64) + "  subrouter_1.2.3_darwin_arm64\n"))
		default:
			http.NotFound(w, request)
		}
	}))
	defer release.Close()
	enableGoldenTestMode(t, release.URL+"/latest", release.URL+"/download")
	_, err := acquireReleasedClient(context.Background(), goldenOptions{releasedVersion: "latest"}, t.TempDir(), true)
	if got := fixedGoldenFailure(err); got != "release_checksum_mismatch" {
		t.Fatalf("failure = %q", got)
	}
}

func enableGoldenTestMode(t *testing.T, releaseAPI, releaseDownloadRoot string) {
	t.Helper()
	previous := goldenTestHooks
	validator, err := filepath.Abs(filepath.Join("..", "..", "deploy", "gcp", "validate-deploy-evidence.py"))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(validator); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("production evidence validator is unavailable: %v", err)
	}
	goldenTestHooks.enabled = true
	goldenTestHooks.releaseAPI = releaseAPI
	goldenTestHooks.releaseDownloadRoot = releaseDownloadRoot
	goldenTestHooks.socketEndpoint = "127.0.0.1:41000"
	goldenTestHooks.evidenceValidator = validator
	// This test validates orchestration and evidence shape. Dedicated monitor
	// tests retain the production cadence limits, while this synthetic process
	// swarm tolerates busy shared CI schedulers.
	goldenTestHooks.localEgressMaxGap = time.Second
	goldenTestHooks.probeScheduleTolerance = time.Second
	t.Cleanup(func() { goldenTestHooks = previous })
}

func TestGoldenObserverValidationAcceptsMultiResponseAndRejectsTransportFallback(t *testing.T) {
	makeSession := func(transports ...string) *goldenSession {
		stats := newObserverStats()
		for index, transport := range transports {
			stats.observe(transportEvent{
				Kind: "request_started", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
				Transport: transport, Method: http.MethodPost, Path: "/v1/responses",
				RequestID: fmt.Sprintf("request-%d", index), ConnectionID: fmt.Sprintf("connection-%d", index),
			})
			stats.observe(transportEvent{
				Kind: "response_chunk", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
				Transport: transport, Method: http.MethodPost, Path: "/v1/responses",
				RequestID: fmt.Sprintf("request-%d", index), ConnectionID: fmt.Sprintf("connection-%d", index), Bytes: 1,
			})
		}
		return &goldenSession{transport: "websocket", observer: &runningGoldenObserver{stats: stats}}
	}
	if err := validateObserverTurns([]*goldenSession{makeSession("websocket", "websocket")}, 1); err != nil {
		t.Fatalf("normal same-transport follow-on response was rejected: %v", err)
	}
	if got := fixedGoldenFailure(validateObserverTurns([]*goldenSession{makeSession("http")}, 1)); got != "transport_fallback_detected" {
		t.Fatalf("fallback failure = %q", got)
	}
}

func TestGoldenSessionSummarySeparatesModelContinuationFromTransportRetry(t *testing.T) {
	stats := newObserverStats()
	for index := range 2 {
		requestID := fmt.Sprintf("request-%d", index)
		connectionID := strings.Repeat(string(rune('a'+index)), 64)
		stats.observe(transportEvent{
			Kind: "request_started", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Transport: "websocket", Method: http.MethodGet, Path: "/v1/responses",
			RequestID: requestID, ConnectionID: connectionID,
		})
		stats.observe(transportEvent{
			Kind: "response_chunk", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Transport: "websocket", Method: http.MethodGet, Path: "/v1/responses",
			RequestID: requestID, ConnectionID: connectionID, Bytes: 1,
		})
	}
	session := &goldenSession{
		label: "multi-response", transport: "websocket", observer: &runningGoldenObserver{stats: stats},
		command: &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}, issues: make(map[string]int),
	}
	summary := summarizeGoldenSession(session, nil, 1, goldenProcessEvidence{}, goldenProcessEvidence{})
	if summary.ResponseRequests != 2 || summary.ResponseConnections != 2 {
		t.Fatalf("responses=%d connections=%d, want two independently observed model calls", summary.ResponseRequests, summary.ResponseConnections)
	}
	if summary.RetryCount != 0 || summary.ReconnectCount != 0 {
		t.Fatalf("model continuation reported as retry=%d reconnect=%d", summary.RetryCount, summary.ReconnectCount)
	}

	session.issues["retry"] = 1
	session.issues["reconnect"] = 1
	summary = summarizeGoldenSession(session, nil, 1, goldenProcessEvidence{}, goldenProcessEvidence{})
	if summary.RetryCount != 1 || summary.ReconnectCount != 1 {
		t.Fatalf("explicit transport issues were lost: retry=%d reconnect=%d", summary.RetryCount, summary.ReconnectCount)
	}
}

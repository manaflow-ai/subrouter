package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

// `sr update` and `sr rollback` replace the binary a non-team install runs,
// using the same release source, asset names and SHA256SUMS verification as
// install.sh and the npm/pip wrappers. Every replacement keeps the outgoing
// binary as a versioned backup and puts it back by itself when the restarted
// daemon does not answer health with the version that was just installed.
//
// Team hosts (the supervised LaunchDaemon, GCP front slots) and package
// manager wrappers own their binary through another path, so both commands
// refuse there and name the command that does own it.

const (
	updateReleaseRepo    = "manaflow-ai/subrouter"
	updateBackupDirName  = ".subrouter-backups"
	updateKeepBackups    = 3
	updateMaxAssetBytes  = 512 << 20
	updateHealthTimeout  = 45 * time.Second
	updateRunTimeout     = 15 * time.Second
	cliAutoupdateLabel   = "ai.manaflow.subrouter-cli-autoupdate"
	teamLaunchDaemonGlob = "/Library/LaunchDaemons/ai.manaflow.subrouter*.plist"
)

var releaseTagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$`)

const srUpdateHelp = `Usage:
  sr update [--version vX.Y.Z] [--check] [--yes]
  sr rollback [--to vX.Y.Z] [--list] [--yes]

update    Install the latest release (or --version) over this machine's
          daemon binary, keep the old one as a backup, restart the daemon and
          wait for /_subrouter/health to report the new version. A failed
          health or version check restores the previous binary automatically.
          --check only compares the installed, running and available versions.
rollback  Put the newest kept backup (or --to) back the same way. The last
          three replaced binaries are kept next to the installed binary in
          .subrouter-backups/. --list prints them.

Team hosts are updated with subrouter-deploy.sh (install-release, pin,
rollback); npm and pip installs are upgraded by their package manager.
`

type installKind string

const (
	installKindLaunchAgent           installKind = "LaunchAgent"
	installKindLaunchAgentSupervised installKind = "supervised LaunchAgent"
	installKindSystemdUser           installKind = "systemd user service"
	installKindSystemdSystem         installKind = "systemd system service"
	installKindWindowsTask           installKind = "Windows scheduled task"
	installKindBinary                installKind = "binary on PATH"
)

// installTarget is the binary an update replaces and how its daemon restarts.
type installTarget struct {
	kind          installKind
	binary        string
	controller    serviceController
	controlSocket string
	// leasePath is the supervisor mutation lease the LaunchAgent migration
	// and rollback scripts hold; empty when nothing else mutates the binary.
	leasePath string
}

func (t installTarget) hasDaemon() bool {
	return t.controller != nil || t.controlSocket != ""
}

// updater carries every host dependency so tests can point it at temp dirs,
// httptest servers and fake service controllers.
type updater struct {
	goos, goarch string
	home         string
	localAppData string

	executable func() (string, error)
	lookPath   func(string) (string, error)
	environ    func(string) string

	client       *http.Client
	latestURL    string
	apiLatestURL string
	downloadBase func(tag string) string

	healthBaseURL string
	healthTimeout time.Duration
	pollInterval  time.Duration

	teamPlistGlob  string
	linuxTeamUnits []string
	systemUnitPath string

	controllerFor  func(kind installKind) serviceController
	runBinary      func(ctx context.Context, path string, args ...string) (string, error)
	upgradeWorker  func(ctx context.Context, socket string) error
	now            func() time.Time
	isInteractive  bool
	in             io.Reader
	out            io.Writer
	programName    string
	cliVersionPath string
}

func newDefaultUpdater(in io.Reader, out io.Writer) *updater {
	home, _ := os.UserHomeDir()
	base := "https://github.com/" + updateReleaseRepo + "/releases"
	u := &updater{
		goos:         runtime.GOOS,
		goarch:       releaseArch(runtime.GOARCH),
		home:         home,
		localAppData: os.Getenv("LOCALAPPDATA"),
		executable:   os.Executable,
		lookPath:     exec.LookPath,
		environ:      os.Getenv,
		client:       &http.Client{Timeout: 10 * time.Minute},
		latestURL:    envOrDefault("SUBROUTER_RELEASE_LATEST_URL", base+"/latest"),
		apiLatestURL: envOrDefault("SUBROUTER_RELEASE_API_URL", "https://api.github.com/repos/"+updateReleaseRepo+"/releases/latest"),
		downloadBase: func(tag string) string {
			if override := strings.TrimSpace(os.Getenv("SUBROUTER_DOWNLOAD_BASE")); override != "" {
				return strings.TrimRight(override, "/")
			}
			return base + "/download/" + tag
		},
		healthBaseURL:  localBaseURL(),
		healthTimeout:  updateHealthTimeout,
		pollInterval:   500 * time.Millisecond,
		teamPlistGlob:  teamLaunchDaemonGlob,
		linuxTeamUnits: []string{"/etc/systemd/system/subrouter-autoupdate.timer", "/etc/systemd/system/subrouter-front.service", "/etc/systemd/system/subrouter-slot@.service"},
		systemUnitPath: systemdUnitPath(systemdConfig{ServiceName: defaultSystemdServiceName}),
		runBinary:      runBinaryOutput,
		upgradeWorker:  postSupervisorUpgrade,
		now:            time.Now,
		isInteractive:  readerIsTerminal(in),
		in:             in,
		out:            out,
		programName:    programBase(),
	}
	if home != "" {
		u.cliVersionPath = filepath.Join(home, ".subrouter", "cli-version")
	}
	u.controllerFor = u.defaultController
	return u
}

// releaseArch maps GOARCH to the architecture suffix build-release.sh uses.
func releaseArch(goarch string) string {
	if goarch != "arm" {
		return goarch
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "GOARM" && strings.HasPrefix(setting.Value, "6") {
				return "armv6"
			}
		}
	}
	return "armv7"
}

func (u *updater) defaultController(kind installKind) serviceController {
	switch kind {
	case installKindLaunchAgent, installKindLaunchAgentSupervised:
		return launchdController{label: defaultDaemonLabel, home: u.home, runner: commandRunner{}}
	case installKindSystemdUser, installKindSystemdSystem:
		return systemdController{service: defaultSystemdServiceName, home: u.home, runner: commandRunner{}}
	case installKindWindowsTask:
		paths := newWindowsPaths(u.localAppData)
		return windowsController{name: defaultWindowsTaskName, xmlPath: filepath.Join(paths.TaskXMLDir, "daemon.xml"), runner: commandRunner{}}
	}
	return nil
}

func runUpdateCommand(program string, args []string) error {
	u := newDefaultUpdater(os.Stdin, os.Stdout)
	u.programName = program
	return u.runUpdateArgs(context.Background(), args)
}

func runRollbackCommand(program string, args []string) error {
	u := newDefaultUpdater(os.Stdin, os.Stdout)
	u.programName = program
	return u.runRollbackArgs(context.Background(), args)
}

type updateOptions struct {
	version string
	check   bool
	yes     bool
}

type rollbackOptions struct {
	to   string
	list bool
	yes  bool
}

func (u *updater) runUpdateArgs(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var opts updateOptions
	flags.StringVar(&opts.version, "version", "", "release tag to install instead of the latest")
	flags.BoolVar(&opts.check, "check", false, "only report installed, running and available versions")
	flags.BoolVar(&opts.yes, "yes", false, "do not ask for confirmation")
	flags.BoolVar(&opts.yes, "y", false, "do not ask for confirmation")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(u.out, srUpdateHelp)
			return nil
		}
		return fmt.Errorf("%w\n%s", err, srUpdateHelp)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), srUpdateHelp)
	}
	return u.update(ctx, opts)
}

func (u *updater) runRollbackArgs(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("rollback", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var opts rollbackOptions
	flags.StringVar(&opts.to, "to", "", "kept version to restore instead of the newest backup")
	flags.BoolVar(&opts.list, "list", false, "list kept backups")
	flags.BoolVar(&opts.yes, "yes", false, "do not ask for confirmation")
	flags.BoolVar(&opts.yes, "y", false, "do not ask for confirmation")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(u.out, srUpdateHelp)
			return nil
		}
		return fmt.Errorf("%w\n%s", err, srUpdateHelp)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), srUpdateHelp)
	}
	return u.rollback(ctx, opts)
}

// ---------------------------------------------------------------------------
// Detection

// detect finds the binary this machine's daemon runs, or refuses when a team
// deployment or a package manager wrapper owns it.
func (u *updater) detect() (installTarget, error) {
	exe, err := u.executable()
	if err != nil {
		return installTarget{}, fmt.Errorf("locate the running binary: %w", err)
	}
	exe = resolvePath(exe)

	target, found, err := u.detectService()
	if err != nil {
		return installTarget{}, err
	}
	if !found {
		if manager, ok := u.wrapperManager(exe); ok {
			return installTarget{}, wrapperRefusal(manager, exe)
		}
		target = installTarget{kind: installKindBinary, binary: exe}
	}
	if err := u.refuseTeamManaged(target.binary, exe); err != nil {
		return installTarget{}, err
	}
	return target, nil
}

func (u *updater) detectService() (installTarget, bool, error) {
	switch u.goos {
	case "darwin":
		plist := launchAgentPath(u.home, defaultDaemonLabel)
		body, err := os.ReadFile(plist)
		if errors.Is(err, os.ErrNotExist) {
			return installTarget{}, false, nil
		}
		if err != nil {
			return installTarget{}, false, err
		}
		arguments, err := plistProgramArguments(body)
		if err != nil || len(arguments) == 0 {
			return installTarget{}, false, fmt.Errorf("read ProgramArguments from %s: %v", plist, err)
		}
		if len(arguments) > 1 && arguments[1] == "supervise" {
			worker := argumentValue(arguments, "--worker-bin")
			if worker == "" {
				return installTarget{}, false, fmt.Errorf("%s runs a supervisor without --worker-bin", plist)
			}
			return installTarget{
				kind:          installKindLaunchAgentSupervised,
				binary:        resolvePath(worker),
				controller:    u.controllerFor(installKindLaunchAgentSupervised),
				controlSocket: argumentValue(arguments, "--control-socket"),
				leasePath:     plist + ".supervisor-mutation.lock",
			}, true, nil
		}
		return installTarget{kind: installKindLaunchAgent, binary: resolvePath(arguments[0]), controller: u.controllerFor(installKindLaunchAgent)}, true, nil
	case "linux":
		for _, candidate := range []struct {
			kind installKind
			path string
		}{
			{installKindSystemdUser, userSystemdUnitPath(u.home, defaultSystemdServiceName)},
			{installKindSystemdSystem, u.systemUnitPath},
		} {
			body, err := os.ReadFile(candidate.path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return installTarget{}, false, err
			}
			binary := systemdExecStartBinary(string(body))
			if binary == "" {
				return installTarget{}, false, fmt.Errorf("no ExecStart binary in %s", candidate.path)
			}
			return installTarget{kind: candidate.kind, binary: resolvePath(binary), controller: u.controllerFor(candidate.kind)}, true, nil
		}
	case "windows":
		if strings.TrimSpace(u.localAppData) == "" {
			return installTarget{}, false, nil
		}
		controller := u.controllerFor(installKindWindowsTask)
		if controller != nil && controller.installed() {
			return installTarget{kind: installKindWindowsTask, binary: newWindowsPaths(u.localAppData).Exe, controller: controller}, true, nil
		}
	}
	return installTarget{}, false, nil
}

// refuseTeamManaged refuses binaries that a team deployment replaces through
// its own staged, guarded path.
func (u *updater) refuseTeamManaged(paths ...string) error {
	switch u.goos {
	case "darwin":
		plists, _ := filepath.Glob(u.teamPlistGlob)
		for _, plist := range plists {
			body, err := os.ReadFile(plist)
			if err != nil {
				continue
			}
			arguments, err := plistProgramArguments(body)
			if err != nil {
				continue
			}
			managed := map[string]bool{}
			for _, candidate := range []string{firstOrEmpty(arguments), argumentValue(arguments, "--worker-bin")} {
				if candidate != "" {
					managed[resolvePath(candidate)] = true
				}
			}
			for _, path := range paths {
				if managed[path] {
					return fmt.Errorf("%s is managed by the team LaunchDaemon %s; update it on the host with\n  sudo subrouter-deploy.sh install-release vX.Y.Z\nand roll back with 'sudo subrouter-deploy.sh rollback' (see deploy/macos/DEPLOY.md)", path, filepath.Base(plist))
				}
			}
		}
	case "linux":
		for _, unit := range u.linuxTeamUnits {
			if _, err := os.Stat(unit); err == nil {
				return fmt.Errorf("this host is a team deployment (%s exists); its binary is replaced by the release pipeline and subrouter-autoupdate, not by '%s update' (see deploy/gcp/README.md)", unit, u.programName)
			}
		}
	}
	return nil
}

// wrapperManager recognizes the per-version cache the npm and pip wrappers
// download into: <cache>/<version>/subrouter_<version>_<os>_<arch>[.exe].
func (u *updater) wrapperManager(exe string) (string, bool) {
	base := strings.TrimSuffix(filepath.Base(exe), ".exe")
	version := filepath.Base(filepath.Dir(exe))
	if !strings.HasPrefix(base, "subrouter_"+version+"_") {
		return "", false
	}
	if u.environ("npm_config_user_agent") != "" || u.environ("npm_execpath") != "" {
		return "npm", true
	}
	for _, name := range []string{"sr", "subrouter"} {
		shim, err := u.lookPath(name)
		if err != nil {
			continue
		}
		head := make([]byte, 256)
		file, err := os.Open(shim)
		if err != nil {
			continue
		}
		n, _ := file.Read(head)
		file.Close()
		text := string(head[:n])
		switch {
		case strings.Contains(text, "node"):
			return "npm", true
		case strings.Contains(text, "python") || strings.Contains(text, "subrouter_cli"):
			return "pip", true
		}
	}
	return "package manager", true
}

func wrapperRefusal(manager, exe string) error {
	switch manager {
	case "npm":
		return fmt.Errorf("%s is downloaded by the npm package; upgrade with\n  npm install -g subrouter@latest\n(or subrouter@X.Y.Z for a specific release)", exe)
	case "pip":
		return fmt.Errorf("%s is downloaded by the Python package; upgrade with\n  pip install --upgrade subrouter\n(or 'pipx upgrade subrouter', or subrouter==X.Y.Z for a specific release)", exe)
	}
	return fmt.Errorf("%s is downloaded by the npm or pip package; upgrade it with 'npm install -g subrouter@latest' or 'pip install --upgrade subrouter'", exe)
}

func resolvePath(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

func firstOrEmpty(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func argumentValue(arguments []string, name string) string {
	for i, argument := range arguments {
		if argument == name && i+1 < len(arguments) {
			return arguments[i+1]
		}
		if strings.HasPrefix(argument, name+"=") {
			return strings.TrimPrefix(argument, name+"=")
		}
	}
	return ""
}

// plistProgramArguments reads the root dictionary's ProgramArguments array,
// falling back to Program when the array is absent.
func plistProgramArguments(body []byte) ([]string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	depth := 0
	pendingKey := ""
	arrayKey := ""
	element := ""
	var arguments []string
	program := ""
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			element = value.Name.Local
			switch element {
			case "dict":
				depth++
			case "array":
				if depth == 1 {
					arrayKey = pendingKey
				}
				depth++
			}
		case xml.EndElement:
			switch value.Name.Local {
			case "dict", "array":
				depth--
				if depth == 1 {
					arrayKey = ""
					pendingKey = ""
				}
			}
			element = ""
		case xml.CharData:
			text := string(value)
			switch {
			case depth == 1 && element == "key":
				pendingKey = strings.TrimSpace(text)
			case depth == 1 && element == "string":
				if pendingKey == "Program" {
					program = strings.TrimSpace(text)
				}
				pendingKey = ""
			case depth == 2 && arrayKey == "ProgramArguments" && element == "string":
				arguments = append(arguments, text)
			}
		}
	}
	if len(arguments) == 0 && program != "" {
		arguments = []string{program}
	}
	return arguments, nil
}

// systemdExecStartBinary returns the executable of the first ExecStart= line,
// undoing the quoting systemdQuote applies.
func systemdExecStartBinary(unit string) string {
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		command := strings.TrimLeft(strings.TrimPrefix(line, "ExecStart="), "-@+!:")
		if strings.HasPrefix(command, `"`) {
			var out strings.Builder
			for i := 1; i < len(command); i++ {
				switch c := command[i]; c {
				case '\\':
					if i+1 < len(command) {
						i++
						out.WriteByte(command[i])
					}
				case '"':
					return out.String()
				case '%':
					if i+1 < len(command) && command[i+1] == '%' {
						i++
					}
					out.WriteByte('%')
				default:
					out.WriteByte(c)
				}
			}
			return out.String()
		}
		if fields := strings.Fields(command); len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Versions

func normalizeVersion(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "v")
}

func sameVersion(left, right string) bool {
	return left != "" && right != "" && normalizeVersion(left) == normalizeVersion(right)
}

func displayVersion(version string) string {
	switch {
	case version == "":
		return "unknown"
	case len(version) > 0 && version[0] >= '0' && version[0] <= '9':
		return "v" + version
	}
	return version
}

// binaryVersion asks a binary for its version. Builds before `version` existed
// answer with an error and report "".
func (u *updater) binaryVersion(ctx context.Context, path string) string {
	output, err := u.runBinary(ctx, path, "version")
	if err != nil {
		return ""
	}
	fields := strings.Fields(output)
	if len(fields) < 2 {
		return ""
	}
	switch fields[0] {
	case "subrouter", "sr", "cx", "subrouter.exe", "sr.exe", "cx.exe":
		return fields[1]
	}
	return ""
}

func runBinaryOutput(ctx context.Context, path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, updateRunTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, path, args...)
	output, err := command.Output()
	return string(output), err
}

// healthVersion reads the local daemon's health. ok is false when it does not
// answer 200; version is "" for builds that predate the field.
func (u *updater) healthVersion(ctx context.Context) (version string, ok bool) {
	healthURL, err := healthURLFor(u.healthBaseURL)
	if err != nil {
		return "", false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, healthURL, nil)
	if err != nil {
		return "", false
	}
	response, err := fallbackHTTPClient().Do(request)
	if err != nil {
		return "", false
	}
	defer response.Body.Close()
	var body struct {
		Version string `json:"version"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body)
	return body.Version, response.StatusCode == http.StatusOK
}

// ---------------------------------------------------------------------------
// Release resolution and download

func (u *updater) resolveTag(ctx context.Context, requested string) (string, error) {
	if requested = strings.TrimSpace(requested); requested != "" {
		tag := "v" + normalizeVersion(requested)
		if !releaseTagPattern.MatchString(tag) {
			return "", fmt.Errorf("invalid release version %q; expected vX.Y.Z", requested)
		}
		return tag, nil
	}
	// Same order as install.sh and the autoupdaters: the releases/latest
	// redirect is not rate limited; the API is the fallback.
	if request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.latestURL, nil); err == nil {
		if response, err := u.client.Do(request); err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			final := response.Request.URL.Path
			if index := strings.LastIndex(final, "/releases/tag/"); index >= 0 && response.StatusCode < 400 {
				tag := "v" + normalizeVersion(final[index+len("/releases/tag/"):])
				if releaseTagPattern.MatchString(tag) {
					return tag, nil
				}
			}
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.apiLatestURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := u.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("resolve latest release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolve latest release: %s answered %s", u.apiLatestURL, response.Status)
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&release); err != nil {
		return "", fmt.Errorf("resolve latest release: %w", err)
	}
	tag := "v" + normalizeVersion(release.TagName)
	if !releaseTagPattern.MatchString(tag) {
		return "", fmt.Errorf("resolve latest release: unexpected tag %q", release.TagName)
	}
	return tag, nil
}

func (u *updater) assetName(tag string) string {
	suffix := ""
	if u.goos == "windows" {
		suffix = ".exe"
	}
	return fmt.Sprintf("subrouter_%s_%s_%s%s", normalizeVersion(tag), u.goos, u.goarch, suffix)
}

// download fetches the release asset into dir (so the final rename is atomic)
// and verifies it against the release's SHA256SUMS exactly like install.sh:
// one and only one matching line, and a matching digest.
func (u *updater) download(ctx context.Context, tag, dir string) (string, error) {
	asset := u.assetName(tag)
	base := u.downloadBase(tag)
	sums, err := u.fetch(ctx, base+"/SHA256SUMS", 1<<20)
	if err != nil {
		return "", err
	}
	var expected []string
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && (fields[1] == asset || fields[1] == "*"+asset) {
			expected = append(expected, strings.ToLower(fields[0]))
		}
	}
	if len(expected) != 1 {
		return "", fmt.Errorf("expected exactly one checksum for %s in SHA256SUMS, found %d", asset, len(expected))
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+asset, nil)
	if err != nil {
		return "", err
	}
	response, err := u.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", asset, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", asset, response.Status)
	}
	file, err := os.CreateTemp(dir, ".subrouter.update.*")
	if err != nil {
		return "", writableError(dir, err, u.programName)
	}
	staged := file.Name()
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, updateMaxAssetBytes))
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		os.Remove(staged)
		return "", fmt.Errorf("download %s: %w", asset, err)
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != expected[0] {
		os.Remove(staged)
		return "", fmt.Errorf("checksum mismatch for %s", asset)
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		os.Remove(staged)
		return "", err
	}
	return staged, nil
}

func (u *updater) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := u.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, response.Status)
	}
	return io.ReadAll(io.LimitReader(response.Body, limit))
}

func writableError(dir string, err error, program string) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("cannot write to %s; re-run as 'sudo %s' for a system-wide install: %w", dir, program, err)
	}
	return err
}

// ---------------------------------------------------------------------------
// Backups

type binaryBackup struct {
	path    string
	version string
	modTime time.Time
}

func backupDir(binary string) string {
	return filepath.Join(filepath.Dir(binary), updateBackupDirName)
}

func (u *updater) backupExt() string {
	if u.goos == "windows" {
		return ".exe"
	}
	return ""
}

var unsafeBackupChars = regexp.MustCompile(`[^0-9A-Za-z.+_-]`)

// backup keeps a copy of binary named for the version it reports, so rollback
// can offer versions rather than timestamps.
func (u *updater) backup(binary, version string) (string, error) {
	dir := backupDir(binary)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", writableError(dir, err, u.programName)
	}
	label := displayVersion(version)
	if version == "" {
		sum, err := fileSHA256(binary)
		if err != nil {
			return "", err
		}
		label = "unknown-" + sum[:12]
	}
	label = unsafeBackupChars.ReplaceAllString(label, "_")
	destination := filepath.Join(dir, "subrouter-"+label+u.backupExt())
	if err := copyFileAtomic(binary, destination); err != nil {
		return "", fmt.Errorf("back up %s: %w", binary, err)
	}
	now := u.now()
	_ = os.Chtimes(destination, now, now)
	return destination, nil
}

func (u *updater) listBackups(binary string) []binaryBackup {
	dir := backupDir(binary)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var backups []binaryBackup
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "subrouter-") || strings.HasPrefix(name, ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		label := strings.TrimSuffix(strings.TrimPrefix(name, "subrouter-"), ".exe")
		version := label
		if strings.HasPrefix(label, "unknown-") {
			version = ""
		}
		backups = append(backups, binaryBackup{path: filepath.Join(dir, name), version: version, modTime: info.ModTime()})
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].modTime.After(backups[j].modTime) })
	return backups
}

func (u *updater) pruneBackups(binary string) {
	backups := u.listBackups(binary)
	for i := updateKeepBackups; i < len(backups); i++ {
		_ = os.Remove(backups[i].path)
	}
}

func backupLabel(b binaryBackup) string {
	if b.version == "" {
		return strings.TrimSuffix(strings.TrimPrefix(filepath.Base(b.path), "subrouter-"), ".exe")
	}
	return b.version
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyFileAtomic(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.CreateTemp(filepath.Dir(destination), ".subrouter.copy.*")
	if err != nil {
		return err
	}
	tmp := output.Name()
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if err := errors.Join(copyErr, closeErr, os.Chmod(tmp, 0o755)); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, destination); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Swap, restart, verify

// swap moves staged over the installed binary. POSIX rename is atomic even
// while the old binary runs; Windows cannot replace a running executable but
// can rename it aside first.
func (u *updater) swap(staged, binary string) error {
	previousSHA, _ := fileSHA256(binary)
	if u.goos == "windows" {
		aside := binary + ".old"
		_ = os.Remove(aside)
		if err := os.Rename(binary, aside); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(staged, binary); err != nil {
			_ = os.Rename(aside, binary)
			return err
		}
		_ = os.Remove(aside)
	} else if err := os.Rename(staged, binary); err != nil {
		return writableError(filepath.Dir(binary), err, u.programName)
	}
	u.refreshAliases(binary, previousSHA)
	return nil
}

// refreshAliases keeps sr and cx next to the binary pointing at the new build.
// Symlinks already follow the rename; a plain copy of the old binary (Windows,
// or an install without symlinks) is replaced too.
func (u *updater) refreshAliases(binary, previousSHA string) {
	for _, alias := range []string{"sr", "cx"} {
		path := filepath.Join(filepath.Dir(binary), alias+u.backupExt())
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		sum, err := fileSHA256(path)
		if err != nil || previousSHA == "" || sum != previousSHA {
			continue
		}
		if err := copyFileAtomic(binary, path); err != nil {
			fmt.Fprintf(u.out, "warning: could not refresh %s: %v\n", path, err)
		}
	}
}

// restartAndVerify restarts the daemon and waits until health reports want.
// want == "" accepts any healthy answer (a binary too old to report its
// version).
func (u *updater) restartAndVerify(ctx context.Context, target installTarget, want string) error {
	if !target.hasDaemon() {
		return nil
	}
	if target.controlSocket != "" {
		if err := u.upgradeWorker(ctx, target.controlSocket); err != nil {
			return fmt.Errorf("supervisor did not take the new worker: %w", err)
		}
	} else if err := target.controller.restart(); err != nil {
		return fmt.Errorf("restart %s: %w", target.controller.describe(), err)
	}
	deadline := u.now().Add(u.healthTimeout)
	last := ""
	for {
		version, ok := u.healthVersion(ctx)
		if ok && (want == "" || sameVersion(version, want)) {
			return nil
		}
		if ok {
			last = "health reports " + displayVersion(version)
		} else {
			last = "health is not answering"
		}
		if !u.now().Before(deadline) {
			return fmt.Errorf("daemon did not report %s within %s (%s)", displayVersion(want), u.healthTimeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(u.pollInterval):
		}
	}
}

// postSupervisorUpgrade asks a supervised LaunchAgent to start a generation
// from the replaced worker binary behind its still-bound listener.
func postSupervisorUpgrade(ctx context.Context, socket string) error {
	client := &http.Client{
		Timeout: 150 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socket)
		}},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/_subrouter/upgrade", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// replaceAndVerify installs staged, restarts, and restores backup when the
// daemon does not come back reporting want.
func (u *updater) replaceAndVerify(ctx context.Context, target installTarget, staged, want, backup, previous string) error {
	if err := u.swap(staged, target.binary); err != nil {
		return fmt.Errorf("replace %s: %w", target.binary, err)
	}
	verifyErr := u.restartAndVerify(ctx, target, want)
	if verifyErr == nil {
		return nil
	}
	fmt.Fprintf(u.out, "%v\nrestoring %s from %s\n", verifyErr, displayVersion(previous), backup)
	restoreErr := u.restore(ctx, target, backup, previous)
	if restoreErr != nil {
		return fmt.Errorf("%w; restoring the previous binary also failed: %v (it is kept at %s)", verifyErr, restoreErr, backup)
	}
	return fmt.Errorf("%w; the previous binary (%s) was restored and is serving", verifyErr, displayVersion(previous))
}

func (u *updater) restore(ctx context.Context, target installTarget, backup, previous string) error {
	file, err := os.CreateTemp(filepath.Dir(target.binary), ".subrouter.restore.*")
	if err != nil {
		return err
	}
	staged := file.Name()
	file.Close()
	if err := copyFileAtomic(backup, staged); err != nil {
		os.Remove(staged)
		return err
	}
	if err := u.swap(staged, target.binary); err != nil {
		os.Remove(staged)
		return err
	}
	return u.restartAndVerify(ctx, target, previous)
}

// lockTarget serializes updates of one install with each other and with the
// deploy scripts that share the supervisor mutation lease.
func (u *updater) lockTarget(target installTarget) (func(), error) {
	dir := backupDir(target.binary)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, writableError(dir, err, u.programName)
	}
	var releases []func()
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, path := range []string{filepath.Join(dir, ".update.lock"), target.leasePath} {
		if path == "" {
			continue
		}
		unlock, err := tryLockUpdateLease(path)
		if err != nil {
			release()
			if errors.Is(err, errUpdateLockBusy) {
				return nil, fmt.Errorf("another update or deployment holds %s; try again when it finishes", path)
			}
			return nil, writableError(filepath.Dir(path), err, u.programName)
		}
		releases = append(releases, unlock)
	}
	return release, nil
}

func (u *updater) confirm(prompt string, yes bool) error {
	if yes {
		return nil
	}
	if !u.isInteractive {
		return fmt.Errorf("%s\nre-run with --yes to proceed without a prompt", prompt)
	}
	fmt.Fprintf(u.out, "%s Continue? [y/N] ", prompt)
	line, _ := bufio.NewReader(u.in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return errors.New("cancelled")
}

// ---------------------------------------------------------------------------
// Commands

func (u *updater) update(ctx context.Context, opts updateOptions) error {
	target, err := u.detect()
	if err != nil {
		return err
	}
	installed := u.binaryVersion(ctx, target.binary)
	daemonVersion, daemonOK := u.healthVersion(ctx)
	tag, err := u.resolveTag(ctx, opts.version)
	if err != nil {
		return err
	}

	fmt.Fprintf(u.out, "install    %s (%s)\n", target.binary, target.kind)
	fmt.Fprintf(u.out, "installed  %s\n", displayVersion(installed))
	if target.hasDaemon() || daemonOK {
		if daemonOK {
			fmt.Fprintf(u.out, "running    %s\n", displayVersion(daemonVersion))
		} else {
			fmt.Fprintf(u.out, "running    not answering at %s\n", u.healthBaseURL)
		}
	}
	label := "available"
	if opts.version != "" {
		label = "requested"
	}
	fmt.Fprintf(u.out, "%-10s %s\n", label, tag)

	binaryCurrent := sameVersion(installed, tag)
	daemonCurrent := !target.hasDaemon() || (daemonOK && sameVersion(daemonVersion, tag))
	if opts.check {
		switch {
		case binaryCurrent && daemonCurrent:
			fmt.Fprintln(u.out, "up to date")
		case binaryCurrent:
			fmt.Fprintf(u.out, "the daemon is not running %s yet; run '%s update' to restart it\n", tag, u.programName)
		default:
			fmt.Fprintf(u.out, "update available; run '%s update%s'\n", u.programName, versionFlag(opts.version, tag))
		}
		return nil
	}

	if binaryCurrent {
		if daemonCurrent {
			fmt.Fprintf(u.out, "already at %s\n", tag)
			return nil
		}
		if err := u.confirm(fmt.Sprintf("%s is installed but the daemon runs %s; restart it.", tag, displayVersion(daemonVersion)), opts.yes); err != nil {
			return err
		}
		if err := u.restartAndVerify(ctx, target, tag); err != nil {
			return err
		}
		fmt.Fprintf(u.out, "daemon restarted on %s\n", tag)
		return nil
	}

	if err := u.confirm(fmt.Sprintf("Replace %s %s with %s.", target.binary, displayVersion(installed), tag), opts.yes); err != nil {
		return err
	}
	unlock, err := u.lockTarget(target)
	if err != nil {
		return err
	}
	defer unlock()
	staged, err := u.download(ctx, tag, filepath.Dir(target.binary))
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	stagedVersion := u.binaryVersion(ctx, staged)
	if stagedVersion == "" {
		if _, err := u.runBinary(ctx, staged, "--help"); err != nil {
			return fmt.Errorf("downloaded %s does not run on this machine: %w", u.assetName(tag), err)
		}
	} else if !sameVersion(stagedVersion, tag) {
		return fmt.Errorf("downloaded %s reports version %s, not %s", u.assetName(tag), stagedVersion, tag)
	}
	backup, err := u.backup(target.binary, installed)
	if err != nil {
		return err
	}
	want := ""
	if stagedVersion != "" {
		want = tag
	}
	if err := u.replaceAndVerify(ctx, target, staged, want, backup, installed); err != nil {
		return fmt.Errorf("update to %s failed: %w", tag, err)
	}
	u.pruneBackups(target.binary)
	u.recordCLIVersion(tag)
	if target.hasDaemon() {
		fmt.Fprintf(u.out, "updated to %s; the daemon reports %s\n", tag, tag)
	} else {
		fmt.Fprintf(u.out, "updated %s to %s\n", target.binary, tag)
		if daemonOK {
			fmt.Fprintf(u.out, "note: a daemon at %s still runs %s and is not managed by a service; restart it yourself\n", u.healthBaseURL, displayVersion(daemonVersion))
		}
	}
	fmt.Fprintf(u.out, "previous binary kept at %s; undo with '%s rollback'\n", backup, u.programName)
	return nil
}

func versionFlag(requested, tag string) string {
	if requested == "" {
		return ""
	}
	return " --version " + tag
}

// recordCLIVersion keeps subrouter-cli-autoupdate.sh's marker in step so the
// CLI updater does not reinstall what `sr update` just installed.
func (u *updater) recordCLIVersion(tag string) {
	if u.cliVersionPath == "" {
		return
	}
	if _, err := os.Stat(u.cliVersionPath); err != nil {
		return
	}
	_ = writeFileAtomic(u.cliVersionPath, []byte(tag+"\n"), 0o644)
}

func (u *updater) rollback(ctx context.Context, opts rollbackOptions) error {
	target, err := u.detect()
	if err != nil {
		return err
	}
	backups := u.listBackups(target.binary)
	if opts.list {
		if len(backups) == 0 {
			fmt.Fprintf(u.out, "no backups in %s\n", backupDir(target.binary))
			return nil
		}
		for _, b := range backups {
			fmt.Fprintf(u.out, "%-24s %s  %s\n", backupLabel(b), b.modTime.Format(time.RFC3339), b.path)
		}
		return nil
	}
	currentSHA, err := fileSHA256(target.binary)
	if err != nil {
		return err
	}
	var chosen *binaryBackup
	for i := range backups {
		b := backups[i]
		if opts.to != "" {
			if sameVersion(b.version, opts.to) || backupLabel(b) == opts.to {
				chosen = &b
				break
			}
			continue
		}
		if sum, err := fileSHA256(b.path); err == nil && sum != currentSHA {
			chosen = &b
			break
		}
	}
	if chosen == nil {
		if opts.to != "" {
			return fmt.Errorf("no kept backup for %s in %s; '%s rollback --list' shows what is kept, '%s update --version %s' installs a release", opts.to, backupDir(target.binary), u.programName, u.programName, opts.to)
		}
		return fmt.Errorf("no backup to roll back to in %s; '%s update' keeps the replaced binary there", backupDir(target.binary), u.programName)
	}
	if sum, err := fileSHA256(chosen.path); err == nil && sum == currentSHA {
		fmt.Fprintf(u.out, "%s is already installed\n", backupLabel(*chosen))
		return nil
	}
	installed := u.binaryVersion(ctx, target.binary)
	if err := u.confirm(fmt.Sprintf("Replace %s %s with the kept %s.", target.binary, displayVersion(installed), backupLabel(*chosen)), opts.yes); err != nil {
		return err
	}
	unlock, err := u.lockTarget(target)
	if err != nil {
		return err
	}
	defer unlock()
	file, err := os.CreateTemp(filepath.Dir(target.binary), ".subrouter.rollback.*")
	if err != nil {
		return writableError(filepath.Dir(target.binary), err, u.programName)
	}
	staged := file.Name()
	file.Close()
	defer os.Remove(staged)
	if err := copyFileAtomic(chosen.path, staged); err != nil {
		return err
	}
	want := u.binaryVersion(ctx, staged)
	current, err := u.backup(target.binary, installed)
	if err != nil {
		return err
	}
	if err := u.replaceAndVerify(ctx, target, staged, want, current, installed); err != nil {
		return fmt.Errorf("rollback to %s failed: %w", backupLabel(*chosen), err)
	}
	u.pruneBackups(target.binary)
	fmt.Fprintf(u.out, "rolled back to %s; %s kept at %s\n", backupLabel(*chosen), displayVersion(installed), current)
	if u.goos == "darwin" && u.home != "" {
		if _, err := os.Stat(launchAgentPath(u.home, cliAutoupdateLabel)); err == nil {
			fmt.Fprintf(u.out, "note: %s will install the next release when one is published\n", cliAutoupdateLabel)
		}
	}
	return nil
}

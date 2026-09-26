package cutovercanary

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

const (
	LegSchema       = "subrouter.launchagent-functional-canary-leg/v1"
	PeerProbeSchema = "subrouter.cutover-peer-probe/v1"
	ProofSchema     = "subrouter.cutover-canary-proof/v1"
	LiveStateSchema = "subrouter.cutover-canary-live-state/v1"
	JournalSchema   = "subrouter.cutover-canary-cleanup-journal/v1"
	ChallengeSchema = "subrouter.cutover-canary-challenge/v1"
	WitnessSchema   = "subrouter.cutover-canary-witness/v1"
)

type HTTPConfig struct {
	BaseURL          string `json:"base_url"`
	AdminTokenFile   string `json:"admin_token_file,omitempty"`
	TimeoutSeconds   int    `json:"timeout_seconds"`
	MaxResponseBytes int64  `json:"max_response_bytes"`
}

type PeerTarget struct {
	Name                       string `json:"name"`
	SSHHost                    string `json:"ssh_host"`
	SSHIdentityFile            string `json:"ssh_identity_file,omitempty"`
	RemoteExecutable           string `json:"remote_executable"`
	RemoteConfigFile           string `json:"remote_config_file"`
	ExpectedExecutableIdentity string `json:"expected_executable_identity"`
	ExpectedIdentityKind       string `json:"expected_identity_kind"`
	TimeoutSeconds             int    `json:"timeout_seconds"`
}

type PeerLegConfig struct {
	Schema     string       `json:"schema"`
	ProofFile  string       `json:"proof_file"`
	Peers      []PeerTarget `json:"peers"`
	configPath string
}

type RoutedLegConfig struct {
	Schema     string     `json:"schema"`
	HTTP       HTTPConfig `json:"http"`
	ProofFile  string     `json:"proof_file"`
	StateFile  string     `json:"state_file"`
	Journal    string     `json:"cleanup_journal"`
	Model      string     `json:"model"`
	configPath string
}

type IsolatedLegConfig struct {
	Schema               string     `json:"schema"`
	HTTP                 HTTPConfig `json:"http"`
	ProofFile            string     `json:"proof_file"`
	Journal              string     `json:"cleanup_journal"`
	Model                string     `json:"model"`
	UnavailableAccountID string     `json:"unavailable_account_id"`
	configPath           string
}

type ClaudeLegConfig struct {
	Schema     string     `json:"schema"`
	HTTP       HTTPConfig `json:"http"`
	ProofFile  string     `json:"proof_file"`
	Journal    string     `json:"cleanup_journal"`
	Model      string     `json:"model"`
	configPath string
}

type ExistingLegConfig struct {
	Schema            string     `json:"schema"`
	HTTP              HTTPConfig `json:"http"`
	ProofFile         string     `json:"proof_file"`
	SelectionFile     string     `json:"selection_file"`
	ChallengeFile     string     `json:"challenge_file"`
	WitnessFile       string     `json:"witness_file"`
	WaitSeconds       int        `json:"wait_seconds"`
	CandidateLogFiles []string   `json:"candidate_log_files"`
	MaxLogAppendBytes int64      `json:"max_log_append_bytes"`
	configPath        string
}

type PeerProbeConfig struct {
	Schema     string     `json:"schema"`
	HTTP       HTTPConfig `json:"http"`
	configPath string
}

type Selection struct {
	Schema    string `json:"schema"`
	AgentType string `json:"agent_type"`
	SessionID string `json:"session_id"`
}

type Challenge struct {
	Schema    string `json:"schema"`
	RunID     string `json:"run_id"`
	Nonce     string `json:"nonce"`
	Prompt    string `json:"prompt"`
	NotBefore string `json:"not_before"`
	ExpiresAt string `json:"expires_at"`
}

type Witness struct {
	Schema     string `json:"schema"`
	NonceHash  string `json:"nonce_hash"`
	ObservedAt string `json:"observed_at"`
}

func validRunID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func readStrictPrivateJSON(path string, dst any) error {
	b, err := readPrivateFile(path, 1<<20)
	if err != nil {
		return err
	}
	if err := rejectDuplicateJSONKeys(b); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid canary configuration: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("invalid canary configuration: trailing JSON")
	}
	return nil
}

func rejectDuplicateJSONKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	first, err := decoder.Token()
	if err != nil {
		return errors.New("invalid canary configuration JSON")
	}
	if err := scanJSONValue(decoder, first); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("invalid canary configuration: trailing JSON")
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, first json.Token) error {
	delimiter, ok := first.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return errors.New("invalid canary configuration JSON")
			}
			key, ok := keyToken.(string)
			if !ok || seen[key] {
				return errors.New("invalid canary configuration: duplicate object key")
			}
			seen[key] = true
			value, err := decoder.Token()
			if err != nil {
				return errors.New("invalid canary configuration JSON")
			}
			if err := scanJSONValue(decoder, value); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			value, err := decoder.Token()
			if err != nil {
				return errors.New("invalid canary configuration JSON")
			}
			if err := scanJSONValue(decoder, value); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid canary configuration JSON")
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim(map[json.Delim]json.Delim{'{': '}', '[': ']'}[delimiter]) {
		return errors.New("invalid canary configuration JSON")
	}
	return nil
}

func readPrivateFile(path string, limit int64) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("canary file path must be absolute")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("canary file must be a regular non-symlink")
	}
	if before.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("canary file must not be group/world accessible")
	}
	if runtime.GOOS != "windows" && fileUID(before) != os.Getuid() {
		return nil, errors.New("canary file must be owned by the current user")
	}
	f, err := openPrivateNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) || !after.Mode().IsRegular() {
		return nil, errors.New("canary file changed during open")
	}
	r := io.LimitReader(f, limit+1)
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("canary file exceeds size limit")
	}
	return b, nil
}

func validateOutputPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Base(path) == "." {
		return errors.New("canary output path must be an absolute file path")
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || parent.Mode().Perm()&0o077 != 0 {
		return errors.New("canary output directory must be private and non-symlink")
	}
	if runtime.GOOS != "windows" && fileUID(parent) != os.Getuid() {
		return errors.New("canary output directory must be owned by the current user")
	}
	if existing, err := os.Lstat(path); err == nil {
		if !existing.Mode().IsRegular() || existing.Mode()&os.ModeSymlink != 0 || existing.Mode().Perm()&0o077 != 0 {
			return errors.New("existing canary output must be a private regular file")
		}
		if runtime.GOOS != "windows" && fileUID(existing) != os.Getuid() {
			return errors.New("existing canary output must be owned by the current user")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func distinctPaths(paths ...string) error {
	type pathIdentity struct {
		canonical string
		info      os.FileInfo
	}
	seen := make([]pathIdentity, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) || filepath.Base(path) == "." {
			return errors.New("canary artifact paths must be distinct absolute files")
		}
		canonical, err := filepath.EvalSymlinks(path)
		if errors.Is(err, os.ErrNotExist) {
			parent, parentErr := filepath.EvalSymlinks(filepath.Dir(path))
			if parentErr != nil {
				return errors.New("canary artifact path identity is unavailable")
			}
			canonical = filepath.Join(parent, filepath.Base(path))
		} else if err != nil {
			return errors.New("canary artifact path identity is unavailable")
		}
		var info os.FileInfo
		if existing, statErr := os.Lstat(path); statErr == nil {
			info = existing
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return errors.New("canary artifact path identity is unavailable")
		}
		for _, prior := range seen {
			if canonical == prior.canonical || (info != nil && prior.info != nil && os.SameFile(info, prior.info)) {
				return errors.New("canary artifact paths must be distinct")
			}
		}
		seen = append(seen, pathIdentity{canonical: canonical, info: info})
	}
	return nil
}

func validateArtifactPaths(writable, readOnly []string) error {
	for _, path := range writable {
		if err := validateOutputPath(path); err != nil {
			return err
		}
	}
	all := make([]string, 0, len(writable)+len(readOnly))
	all = append(all, writable...)
	all = append(all, readOnly...)
	if err := distinctPaths(all...); err != nil {
		return err
	}
	return nil
}

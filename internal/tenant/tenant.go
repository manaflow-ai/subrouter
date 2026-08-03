// Package tenant stores the multi-tenant registry for a subrouter server.
// Each tenant owns an isolated account pool under <state-dir>/tenants/<id>/
// that mirrors the single-tenant layout (codex/accounts/*.json, codex/claude/,
// sessions.json). Tenant keys look like srt_<32 hex>; only SHA-256 hashes are
// stored, so a key is shown once at creation and cannot be recovered later.
package tenant

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const KeyPrefix = "srt_"
const keyRandomBytes = 16
const keyDisplayPrefixLen = len(KeyPrefix) + 8

type Key struct {
	Hash      string    `json:"hash"`
	Prefix    string    `json:"prefix"`
	CreatedAt time.Time `json:"createdAt"`
}

type Tenant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	Keys      []Key     `json:"keys,omitempty"`
}

type registryFile struct {
	Tenants []Tenant `json:"tenants"`
}

// Registry reads and writes tenants.json under a server state dir. Reads are
// cached on file modtime+size so the per-request key resolution is one stat.
type Registry struct {
	stateDir string

	mu       sync.Mutex
	cached   registryFile
	haveFile bool
	modTime  time.Time
	size     int64
}

func NewRegistry(stateDir string) *Registry {
	return &Registry{stateDir: stateDir}
}

func (r *Registry) Path() string {
	return filepath.Join(r.stateDir, "tenants.json")
}

func (r *Registry) TenantsDir() string {
	return filepath.Join(r.stateDir, "tenants")
}

// Dir returns the tenant's isolated state dir.
func (r *Registry) Dir(id string) string {
	return filepath.Join(r.TenantsDir(), id)
}

// ValidKeyFormat reports whether value is shaped like a tenant key
// (srt_ followed by 32 lowercase hex chars).
func ValidKeyFormat(value string) bool {
	if !strings.HasPrefix(value, KeyPrefix) {
		return false
	}
	rest := value[len(KeyPrefix):]
	if len(rest) != keyRandomBytes*2 {
		return false
	}
	for _, ch := range rest {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func newKey() (string, Key, error) {
	buf := make([]byte, keyRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", Key{}, err
	}
	plaintext := KeyPrefix + hex.EncodeToString(buf)
	return plaintext, Key{
		Hash:      HashKey(plaintext),
		Prefix:    plaintext[:keyDisplayPrefixLen],
		CreatedAt: time.Now().UTC(),
	}, nil
}

func newTenantID() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// DeriveKey deterministically maps an external tenant identity to a valid
// tenant key. The secret stays on the server; only the derived srt_ key is
// returned to the authenticated client.
func DeriveKey(secret []byte, namespace, externalID string) (string, error) {
	if len(secret) < 32 {
		return "", errors.New("tenant key secret must be at least 32 bytes")
	}
	externalID = strings.TrimSpace(externalID)
	if !ValidExternalID(externalID) {
		return "", errors.New("invalid external tenant ID")
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(strings.TrimSpace(namespace)))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(externalID))
	return KeyPrefix + hex.EncodeToString(mac.Sum(nil)[:keyRandomBytes]), nil
}

// ValidExternalID permits lowercase identity-provider IDs as directory names
// while rejecting separators, traversal, and case-folding collisions.
func ValidExternalID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	if value != strings.ToLower(value) {
		return false
	}
	for _, ch := range value {
		if (ch >= 'a' && ch <= 'z') ||
			(ch >= '0' && ch <= '9') ||
			ch == '-' || ch == '_' || ch == '.' {
			continue
		}
		return false
	}
	return true
}

// load returns a deep copy of the registry, served from the modtime+size
// cache when fresh. Copying keeps callers (and failed mutations) from
// aliasing the cached authentication state.
func (r *Registry) load() (registryFile, error) {
	info, err := os.Stat(r.Path())
	if errors.Is(err, os.ErrNotExist) {
		r.cached = registryFile{}
		r.haveFile = false
		return registryFile{}, nil
	}
	if err != nil {
		return registryFile{}, err
	}
	if r.haveFile && info.ModTime().Equal(r.modTime) && info.Size() == r.size {
		return copyRegistryFile(r.cached), nil
	}
	return r.loadFromDisk(info)
}

// loadFresh always re-reads tenants.json, bypassing the modtime cache.
// Mutations use it under the interprocess lock so another process's write
// within the same modtime granule cannot be lost.
func (r *Registry) loadFresh() (registryFile, error) {
	info, err := os.Stat(r.Path())
	if errors.Is(err, os.ErrNotExist) {
		r.cached = registryFile{}
		r.haveFile = false
		return registryFile{}, nil
	}
	if err != nil {
		return registryFile{}, err
	}
	return r.loadFromDisk(info)
}

func (r *Registry) loadFromDisk(info os.FileInfo) (registryFile, error) {
	body, err := os.ReadFile(r.Path())
	if err != nil {
		return registryFile{}, err
	}
	var file registryFile
	if err := json.Unmarshal(body, &file); err != nil {
		return registryFile{}, fmt.Errorf("parse %s: %w", r.Path(), err)
	}
	r.cached = file
	r.haveFile = true
	r.modTime = info.ModTime()
	r.size = info.Size()
	return copyRegistryFile(file), nil
}

func copyRegistryFile(file registryFile) registryFile {
	out := registryFile{Tenants: make([]Tenant, len(file.Tenants))}
	for i, t := range file.Tenants {
		t.Keys = append([]Key(nil), t.Keys...)
		out.Tenants[i] = t
	}
	return out
}

func (r *Registry) save(file registryFile) error {
	if err := os.MkdirAll(r.stateDir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp := r.Path() + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.Path()); err != nil {
		return err
	}
	r.cached = copyRegistryFile(file)
	if info, statErr := os.Stat(r.Path()); statErr == nil {
		r.haveFile = true
		r.modTime = info.ModTime()
		r.size = info.Size()
	} else {
		r.haveFile = false
	}
	return nil
}

func (r *Registry) List() ([]Tenant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	file, err := r.load()
	if err != nil {
		return nil, err
	}
	return append([]Tenant(nil), file.Tenants...), nil
}

// HasTenants reports whether at least one tenant exists.
func (r *Registry) HasTenants() bool {
	tenants, err := r.List()
	return err == nil && len(tenants) > 0
}

// Create registers a new tenant, provisions its state dir, and returns the
// tenant plus its first key in plaintext (the only time it is available).
func (r *Registry) Create(name string) (Tenant, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Tenant{}, "", errors.New("tenant name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	lock, err := r.lockRegistry()
	if err != nil {
		return Tenant{}, "", err
	}
	defer lock.Close()
	file, err := r.loadFresh()
	if err != nil {
		return Tenant{}, "", err
	}
	for _, existing := range file.Tenants {
		if strings.EqualFold(existing.Name, name) {
			return Tenant{}, "", fmt.Errorf("tenant %q already exists", name)
		}
	}
	id, err := newTenantID()
	if err != nil {
		return Tenant{}, "", err
	}
	plaintext, key, err := newKey()
	if err != nil {
		return Tenant{}, "", err
	}
	created := Tenant{
		ID:        id,
		Name:      name,
		CreatedAt: time.Now().UTC(),
		Keys:      []Key{key},
	}
	if err := os.MkdirAll(filepath.Join(r.Dir(id), "codex", "accounts"), 0o700); err != nil {
		return Tenant{}, "", err
	}
	file.Tenants = append(file.Tenants, created)
	if err := r.save(file); err != nil {
		return Tenant{}, "", err
	}
	return created, plaintext, nil
}

// EnsureExternal creates or updates the tenant owned by an external identity
// provider. plaintextKey must be a deterministic key derived by the caller, so
// repeated logins return the same client configuration without accumulating
// registry keys.
func (r *Registry) EnsureExternal(id, name, plaintextKey string) (Tenant, error) {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if !ValidExternalID(id) {
		return Tenant{}, errors.New("invalid external tenant ID")
	}
	if name == "" {
		name = id
	}
	if !ValidKeyFormat(plaintextKey) {
		return Tenant{}, errors.New("invalid external tenant key")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	lock, err := r.lockRegistry()
	if err != nil {
		return Tenant{}, err
	}
	defer lock.Close()
	file, err := r.loadFresh()
	if err != nil {
		return Tenant{}, err
	}
	hash := HashKey(plaintextKey)
	for i := range file.Tenants {
		if file.Tenants[i].ID != id {
			continue
		}
		changed := false
		if file.Tenants[i].Name != name {
			file.Tenants[i].Name = name
			changed = true
		}
		found := false
		for _, key := range file.Tenants[i].Keys {
			if key.Hash == hash {
				found = true
				break
			}
		}
		if !found {
			file.Tenants[i].Keys = append(file.Tenants[i].Keys, Key{
				Hash: hash, Prefix: plaintextKey[:keyDisplayPrefixLen],
				CreatedAt: time.Now().UTC(),
			})
			changed = true
		}
		if err := os.MkdirAll(filepath.Join(r.Dir(id), "codex", "accounts"), 0o700); err != nil {
			return Tenant{}, err
		}
		if changed {
			if err := r.save(file); err != nil {
				return Tenant{}, err
			}
		}
		return file.Tenants[i], nil
	}
	created := Tenant{
		ID: id, Name: name, CreatedAt: time.Now().UTC(),
		Keys: []Key{{
			Hash: hash, Prefix: plaintextKey[:keyDisplayPrefixLen],
			CreatedAt: time.Now().UTC(),
		}},
	}
	if err := os.MkdirAll(filepath.Join(r.Dir(id), "codex", "accounts"), 0o700); err != nil {
		return Tenant{}, err
	}
	file.Tenants = append(file.Tenants, created)
	if err := r.save(file); err != nil {
		return Tenant{}, err
	}
	return created, nil
}

// CreateKey mints an additional key for an existing tenant and returns it in
// plaintext.
func (r *Registry) CreateKey(tenantID string) (Tenant, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lock, err := r.lockRegistry()
	if err != nil {
		return Tenant{}, "", err
	}
	defer lock.Close()
	file, err := r.loadFresh()
	if err != nil {
		return Tenant{}, "", err
	}
	for i := range file.Tenants {
		if file.Tenants[i].ID != tenantID {
			continue
		}
		plaintext, key, err := newKey()
		if err != nil {
			return Tenant{}, "", err
		}
		file.Tenants[i].Keys = append(file.Tenants[i].Keys, key)
		if err := r.save(file); err != nil {
			return Tenant{}, "", err
		}
		return file.Tenants[i], plaintext, nil
	}
	return Tenant{}, "", fmt.Errorf("tenant %q not found", tenantID)
}

// RevokeKey removes the key whose display prefix exactly matches keyRef, or
// whose hash matches when keyRef is a full plaintext key, and returns how many
// keys were revoked. Exact matching only: a loose prefix like "srt_" must not
// wipe every key on the tenant.
func (r *Registry) RevokeKey(tenantID, keyRef string) (int, error) {
	keyRef = strings.TrimSpace(keyRef)
	if keyRef == "" {
		return 0, errors.New("key prefix is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	lock, err := r.lockRegistry()
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	file, err := r.loadFresh()
	if err != nil {
		return 0, err
	}
	fullHash := ""
	if ValidKeyFormat(keyRef) {
		fullHash = HashKey(keyRef)
	}
	for i := range file.Tenants {
		if file.Tenants[i].ID != tenantID {
			continue
		}
		kept := file.Tenants[i].Keys[:0]
		revoked := 0
		for _, key := range file.Tenants[i].Keys {
			if (fullHash != "" && key.Hash == fullHash) || key.Prefix == keyRef {
				revoked++
				continue
			}
			kept = append(kept, key)
		}
		if revoked == 0 {
			return 0, fmt.Errorf("no key matching %q on tenant %q", keyRef, tenantID)
		}
		file.Tenants[i].Keys = kept
		if err := r.save(file); err != nil {
			return 0, err
		}
		return revoked, nil
	}
	return 0, fmt.Errorf("tenant %q not found", tenantID)
}

// Resolve maps a plaintext tenant key to its tenant. A revoked or unknown key
// returns ok=false.
func (r *Registry) Resolve(key string) (Tenant, bool, error) {
	if !ValidKeyFormat(key) {
		return Tenant{}, false, nil
	}
	hash := HashKey(key)
	r.mu.Lock()
	defer r.mu.Unlock()
	file, err := r.load()
	if err != nil {
		return Tenant{}, false, err
	}
	for _, t := range file.Tenants {
		for _, k := range t.Keys {
			if k.Hash == hash {
				return t, true, nil
			}
		}
	}
	return Tenant{}, false, nil
}

// Find matches a tenant by exact ID or case-insensitive name.
func (r *Registry) Find(ref string) (Tenant, bool, error) {
	ref = strings.TrimSpace(ref)
	tenants, err := r.List()
	if err != nil {
		return Tenant{}, false, err
	}
	for _, t := range tenants {
		if t.ID == ref || strings.EqualFold(t.Name, ref) {
			return t, true, nil
		}
	}
	return Tenant{}, false, nil
}

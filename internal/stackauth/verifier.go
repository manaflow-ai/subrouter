package stackauth

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Claims struct {
	Subject        string
	ProjectID      string
	SelectedTeamID string
	Email          string
	Role           string
	Issuer         string
	ExpiresAt      time.Time
	IsAnonymous    bool
	IsRestricted   bool
}

type Verifier struct {
	APIURL     string
	ProjectID  string
	HTTPClient *http.Client
	CacheTTL   time.Duration
	// ForceRefreshInterval bounds signature-triggered JWKS refreshes.
	ForceRefreshInterval time.Duration
	Now                  func() time.Time

	mu           sync.Mutex
	keys         map[string]*ecdsa.PublicKey
	fetchedAt    time.Time
	forcedAt     time.Time
	fetching     bool
	fetchDone    chan struct{}
	lastFetchErr error
}

func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("malformed Stack access token")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, errors.New("malformed Stack token header")
	}
	var header struct {
		Algorithm string          `json:"alg"`
		KeyID     string          `json:"kid"`
		Type      string          `json:"typ"`
		Critical  json.RawMessage `json:"crit"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Claims{}, errors.New("malformed Stack token header")
	}
	if header.Algorithm != "ES256" || header.KeyID == "" {
		return Claims{}, fmt.Errorf("unsupported Stack token algorithm %q", header.Algorithm)
	}
	if header.Type != "" && header.Type != "JWT" {
		return Claims{}, fmt.Errorf("unsupported Stack token type %q", header.Type)
	}
	if len(header.Critical) != 0 {
		return Claims{}, errors.New("unsupported Stack token critical header")
	}
	key, err := v.key(ctx, header.KeyID, false)
	if err != nil {
		return Claims{}, err
	}
	if err := verifyES256(key, parts[0]+"."+parts[1], parts[2]); err != nil {
		// A newly rotated key can arrive before the cache expires.
		key, refreshErr := v.key(ctx, header.KeyID, true)
		if refreshErr != nil || verifyES256(key, parts[0]+"."+parts[1], parts[2]) != nil {
			return Claims{}, errors.New("invalid Stack token signature")
		}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, errors.New("malformed Stack token payload")
	}
	var wire struct {
		Subject        string          `json:"sub"`
		ProjectID      string          `json:"project_id"`
		SelectedTeamID string          `json:"selected_team_id"`
		Email          string          `json:"email"`
		Role           string          `json:"role"`
		Issuer         string          `json:"iss"`
		Audience       json.RawMessage `json:"aud"`
		ExpiresAt      int64           `json:"exp"`
		NotBefore      int64           `json:"nbf"`
		IsAnonymous    bool            `json:"is_anonymous"`
		IsRestricted   bool            `json:"is_restricted"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return Claims{}, errors.New("malformed Stack token payload")
	}
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	if wire.ExpiresAt == 0 || !now.Before(time.Unix(wire.ExpiresAt, 0).Add(30*time.Second)) {
		return Claims{}, errors.New("Stack access token expired")
	}
	if wire.NotBefore != 0 && now.Add(30*time.Second).Before(time.Unix(wire.NotBefore, 0)) {
		return Claims{}, errors.New("Stack access token is not active")
	}
	if wire.ProjectID != v.ProjectID {
		return Claims{}, errors.New("Stack access token belongs to another project")
	}
	expectedIssuer := strings.TrimRight(v.apiURL(), "/") + "/projects/" + v.ProjectID
	if wire.Issuer != expectedIssuer {
		return Claims{}, errors.New("Stack access token has an invalid issuer")
	}
	if !audienceContains(wire.Audience, v.ProjectID) {
		return Claims{}, errors.New("Stack access token has an invalid audience")
	}
	if wire.Subject == "" || wire.Role != "authenticated" || wire.IsAnonymous || wire.IsRestricted {
		return Claims{}, errors.New("Stack access token does not represent an unrestricted authenticated user")
	}
	if strings.TrimSpace(wire.SelectedTeamID) == "" {
		return Claims{}, errors.New("select a Stack team before using hosted Subrouter")
	}
	return Claims{
		Subject:        wire.Subject,
		ProjectID:      wire.ProjectID,
		SelectedTeamID: wire.SelectedTeamID,
		Email:          wire.Email,
		Role:           wire.Role,
		Issuer:         wire.Issuer,
		ExpiresAt:      time.Unix(wire.ExpiresAt, 0),
		IsAnonymous:    wire.IsAnonymous,
		IsRestricted:   wire.IsRestricted,
	}, nil
}

func (v *Verifier) key(ctx context.Context, keyID string, force bool) (*ecdsa.PublicKey, error) {
	ttl := v.CacheTTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	forceInterval := v.ForceRefreshInterval
	if forceInterval <= 0 {
		forceInterval = 30 * time.Second
	}

	v.mu.Lock()
	now := v.now()
	fresh := !v.fetchedAt.IsZero() && now.Sub(v.fetchedAt) < ttl
	if !force && fresh {
		if key := v.keys[keyID]; key != nil {
			v.mu.Unlock()
			return key, nil
		}
		// An unknown key ID may be a real key rotation. Give it one bounded
		// refresh attempt instead of letting arbitrary IDs bypass the cache.
		force = true
	}
	if v.fetching {
		done := v.fetchDone
		v.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
		}
		v.mu.Lock()
		key := v.keys[keyID]
		fetchErr := v.lastFetchErr
		v.mu.Unlock()
		if key != nil {
			return key, nil
		}
		if fetchErr != nil {
			return nil, fetchErr
		}
		return nil, fmt.Errorf("Stack signing key %q not found", keyID)
	}
	if force && !v.forcedAt.IsZero() &&
		now.Sub(v.forcedAt) < forceInterval {
		key := v.keys[keyID]
		v.mu.Unlock()
		if key != nil {
			return key, nil
		}
		return nil, fmt.Errorf("Stack signing key %q not found", keyID)
	}
	v.fetching = true
	v.fetchDone = make(chan struct{})
	v.lastFetchErr = nil
	if force {
		v.forcedAt = now
	}
	v.mu.Unlock()

	keys, err := v.fetchKeys(ctx)

	v.mu.Lock()
	if err == nil {
		v.keys = keys
		v.fetchedAt = v.now()
	}
	v.lastFetchErr = err
	v.fetching = false
	close(v.fetchDone)
	key := v.keys[keyID]
	v.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, fmt.Errorf("Stack signing key %q not found", keyID)
	}
	return key, nil
}

func (v *Verifier) fetchKeys(ctx context.Context) (map[string]*ecdsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		strings.TrimRight(v.apiURL(), "/")+"/projects/"+v.ProjectID+"/.well-known/jwks.json",
		nil,
	)
	if err != nil {
		return nil, err
	}
	client := v.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("load Stack signing keys: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, responseError("load Stack signing keys", res)
	}
	var document struct {
		Keys []struct {
			KeyID     string `json:"kid"`
			KeyType   string `json:"kty"`
			Curve     string `json:"crv"`
			Algorithm string `json:"alg"`
			X         string `json:"x"`
			Y         string `json:"y"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&document); err != nil {
		return nil, fmt.Errorf("decode Stack signing keys: %w", err)
	}
	keys := make(map[string]*ecdsa.PublicKey, len(document.Keys))
	for _, item := range document.Keys {
		if item.KeyID == "" || item.KeyType != "EC" || item.Curve != "P-256" || item.Algorithm != "ES256" {
			continue
		}
		x, errX := base64.RawURLEncoding.DecodeString(item.X)
		y, errY := base64.RawURLEncoding.DecodeString(item.Y)
		if errX != nil || errY != nil || len(x) > 32 || len(y) > 32 {
			continue
		}
		point := make([]byte, 65)
		point[0] = 4
		copy(point[33-len(x):33], x)
		copy(point[65-len(y):], y)
		if _, err := ecdh.P256().NewPublicKey(point); err != nil {
			continue
		}
		key := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		keys[item.KeyID] = key
	}
	if len(keys) == 0 {
		return nil, errors.New("Stack JWKS contained no usable ES256 keys")
	}
	return keys, nil
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *Verifier) apiURL() string {
	if strings.TrimSpace(v.APIURL) == "" {
		return DefaultAPIURL
	}
	return strings.TrimRight(v.APIURL, "/")
}

func verifyES256(key *ecdsa.PublicKey, signingInput, encodedSignature string) error {
	raw, err := base64.RawURLEncoding.DecodeString(encodedSignature)
	if err != nil || len(raw) != 64 {
		return errors.New("malformed ES256 signature")
	}
	sum := sha256.Sum256([]byte(signingInput))
	r := new(big.Int).SetBytes(raw[:32])
	s := new(big.Int).SetBytes(raw[32:])
	if !ecdsa.Verify(key, sum[:], r, s) {
		return errors.New("invalid ES256 signature")
	}
	return nil
}

func audienceContains(raw json.RawMessage, expected string) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == expected
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, item := range many {
			if item == expected {
				return true
			}
		}
	}
	return false
}

package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/manaflow-ai/subrouter/session"
)

const (
	cutoverChallengeSchema       = "subrouter.cutover-challenge/v1"
	cutoverChallengeMaxBodyBytes = int64(4096)
	cutoverChallengeMaxLifetime  = 10 * time.Minute
)

var (
	errCutoverChallengeConflict = errors.New("cutover challenge already armed")
	errCutoverChallengeMismatch = errors.New("cutover challenge identity mismatch")
)

type sessionAdminView struct {
	session.Assignment
	Active bool `json:"active"`
}

type cutoverChallengeRegistration struct {
	Schema       string `json:"schema"`
	AgentType    string `json:"agent_type"`
	SessionID    string `json:"session_id"`
	InputSHA256  string `json:"input_sha256"`
	MarkerSHA256 string `json:"marker_sha256"`
	NotBefore    string `json:"not_before"`
	ExpiresAt    string `json:"expires_at"`
}

type armedCutoverChallenge struct {
	cutoverChallengeRegistration
	notBefore time.Time
	expiresAt time.Time
}

type cutoverChallengeRegistry struct {
	mu     sync.Mutex
	active *armedCutoverChallenge
}

func newCutoverChallengeRegistry() *cutoverChallengeRegistry {
	return &cutoverChallengeRegistry{}
}

func (r *cutoverChallengeRegistry) arm(challenge armedCutoverChallenge, now time.Time) error {
	if r == nil {
		return errors.New("cutover challenge registry unavailable")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != nil && now.Before(r.active.expiresAt) {
		return errCutoverChallengeConflict
	}
	copy := challenge
	r.active = &copy
	return nil
}

func (r *cutoverChallengeRegistry) disarm(challenge cutoverChallengeRegistration, now time.Time) error {
	if r == nil {
		return errors.New("cutover challenge registry unavailable")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil || !now.Before(r.active.expiresAt) {
		r.active = nil
		return nil
	}
	if r.active.cutoverChallengeRegistration != challenge {
		return errCutoverChallengeMismatch
	}
	r.active = nil
	return nil
}

func (r *cutoverChallengeRegistry) take(agentType, sessionID string, now time.Time) (armedCutoverChallenge, bool) {
	if r == nil {
		return armedCutoverChallenge{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		return armedCutoverChallenge{}, false
	}
	if !now.Before(r.active.expiresAt) {
		r.active = nil
		return armedCutoverChallenge{}, false
	}
	if now.Before(r.active.notBefore) || r.active.AgentType != agentType || r.active.SessionID != sessionID {
		return armedCutoverChallenge{}, false
	}
	challenge := *r.active
	r.active = nil
	return challenge, true
}

func (s Server) handleCutoverChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	registration, challenge, err := decodeCutoverChallengeRegistration(w, r, time.Now().UTC(), r.Method == http.MethodPost)
	if err != nil {
		http.Error(w, "invalid cutover challenge registration", http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.cutoverChallenges.disarm(registration, time.Now().UTC()); err != nil {
			http.Error(w, "cutover challenge identity mismatch", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.Sessions == nil {
		http.Error(w, "selected existing session unavailable", http.StatusConflict)
		return
	}
	if _, ok := s.Sessions.Get(registration.AgentType, registration.SessionID); !ok {
		http.Error(w, "selected existing session unavailable", http.StatusConflict)
		return
	}
	if s.activeSession(registration.AgentType, registration.SessionID) {
		http.Error(w, "selected existing session is active", http.StatusConflict)
		return
	}
	if err := s.cutoverChallenges.arm(challenge, time.Now().UTC()); err != nil {
		http.Error(w, "cutover challenge already armed", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeCutoverChallengeRegistration(w http.ResponseWriter, r *http.Request, now time.Time, requireLive bool) (cutoverChallengeRegistration, armedCutoverChallenge, error) {
	var registration cutoverChallengeRegistration
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "application/json") {
		return registration, armedCutoverChallenge{}, errors.New("content type must be application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, cutoverChallengeMaxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&registration); err != nil {
		return registration, armedCutoverChallenge{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return registration, armedCutoverChallenge{}, errors.New("trailing JSON")
	}
	if registration.Schema != cutoverChallengeSchema || session.NormalizeAgentType(registration.AgentType) != "codex" ||
		!validCutoverSessionID(registration.SessionID) || !validLowerHexSHA256(registration.InputSHA256) ||
		!validLowerHexSHA256(registration.MarkerSHA256) {
		return registration, armedCutoverChallenge{}, errors.New("invalid challenge identity")
	}
	notBefore, err := time.Parse(time.RFC3339Nano, registration.NotBefore)
	if err != nil {
		return registration, armedCutoverChallenge{}, fmt.Errorf("invalid not_before: %w", err)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, registration.ExpiresAt)
	if err != nil || !notBefore.Before(expiresAt) || expiresAt.Sub(notBefore) > cutoverChallengeMaxLifetime {
		return registration, armedCutoverChallenge{}, errors.New("invalid challenge lifetime")
	}
	if requireLive && (!now.Before(expiresAt) || notBefore.After(now.Add(cutoverChallengeMaxLifetime))) {
		return registration, armedCutoverChallenge{}, errors.New("invalid challenge lifetime")
	}
	return registration, armedCutoverChallenge{cutoverChallengeRegistration: registration, notBefore: notBefore, expiresAt: expiresAt}, nil
}

func validCutoverSessionID(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 1024 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validLowerHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

// cutoverCanaryRequestEvidence consumes a matching server-owned challenge only
// for the selected active session. It returns the pre-registered marker digest
// only when the streamed Responses input has the exact challenged leaf.
func cutoverCanaryRequestEvidence(request *http.Request, agentType, sessionID string, active bool, registry *cutoverChallengeRegistry, now time.Time) (string, bool) {
	if request == nil || !active || session.NormalizeAgentType(agentType) != "codex" ||
		request.Method != http.MethodPost || !codexResponsePath(request.URL.Path) || request.GetBody == nil {
		return "", false
	}
	challenge, armed := registry.take(agentType, sessionID, now)
	if !armed {
		return "", false
	}
	body, err := request.GetBody()
	if err != nil {
		return "", true
	}
	defer body.Close()
	clone := request.Clone(request.Context())
	clone.Body = body
	inputHash, ok := session.ExtractResponsesCurrentUserInputHash(clone, replayablePostMaxBodyBytes)
	if !ok || inputHash != challenge.InputSHA256 {
		return "", true
	}
	return challenge.MarkerSHA256, true
}

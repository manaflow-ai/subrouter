package proxy

import (
	"encoding/json"
	"net/http"
	"time"
)

func (s Server) handleRecoveryStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var update struct {
			Agent           string    `json:"agent"`
			SessionID       string    `json:"session_id"`
			Action          string    `json:"action"`
			DispatchedAt    time.Time `json:"dispatched_at"`
			GenerationBegan bool      `json:"generation_began"`
			RequestTokens   int64     `json:"request_tokens"`
			ResponseTokens  int64     `json:"response_tokens"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&update); err != nil {
			http.Error(w, "invalid recovery update", http.StatusBadRequest)
			return
		}
		if update.Agent == "" || update.SessionID == "" {
			http.Error(w, "agent and session_id are required", http.StatusBadRequest)
			return
		}
		if update.Action != "" {
			at := update.DispatchedAt
			if at.IsZero() {
				at = time.Now().UTC()
			}
			s.Recovery.RecordReplayDispatch(update.Agent, update.SessionID, update.Action, at)
		} else {
			s.Recovery.RecordGeneration(update.Agent, update.SessionID, update.GenerationBegan, update.RequestTokens, update.ResponseTokens)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.Recovery.List(time.Now().UTC()))
}

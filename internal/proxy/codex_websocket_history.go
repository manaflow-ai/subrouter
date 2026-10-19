package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
)

// codexWebSocketHistoryLimit bounds the conversation one connection keeps
// for rebuilding chained turns; a larger one is not kept and its chained
// turns stay on their connection.
const codexWebSocketHistoryLimit = 8 << 20

// codexWebSocketCreateInput is a response.create's input items and its
// previous_response_id, top level or under response.
func codexWebSocketCreateInput(body []byte) (input []json.RawMessage, previous string, ok bool) {
	var event struct {
		Input              []json.RawMessage `json:"input"`
		PreviousResponseID string            `json:"previous_response_id"`
		Response           *struct {
			Input              []json.RawMessage `json:"input"`
			PreviousResponseID string            `json:"previous_response_id"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		return nil, "", false
	}
	if event.Response != nil && event.Input == nil && event.PreviousResponseID == "" {
		return event.Response.Input, event.Response.PreviousResponseID, true
	}
	return event.Input, event.PreviousResponseID, true
}

// fullInputLocked is the full conversation a response.create asks for: its
// own input, after the history when it is chained on the response the
// history belongs to. nil when that history is not known.
func (s *webSocketModelState) fullInputLocked(body []byte) []json.RawMessage {
	input, previous, ok := codexWebSocketCreateInput(body)
	if !ok {
		return nil
	}
	if previous == "" {
		return input
	}
	if s.history == nil || previous != s.historyResponse {
		return nil
	}
	full := make([]json.RawMessage, 0, len(s.history)+len(input))
	return append(append(full, s.history...), input...)
}

// noteHistory follows the upstream's events: the output items of the turn
// in flight, and on response.completed the conversation that response now
// holds.
func (s *webSocketModelState) noteHistory(body []byte) {
	if !bytes.Contains(body, []byte("response.output_item.done")) && !bytes.Contains(body, []byte("response.completed")) {
		return
	}
	var event struct {
		Type     string          `json:"type"`
		Item     json.RawMessage `json:"item"`
		Response struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.autonomousRetrying && !s.regionRetry {
		return
	}
	switch strings.ToLower(event.Type) {
	case "response.output_item.done":
		if len(event.Item) > 0 {
			s.turnOutput = append(s.turnOutput, event.Item)
		}
	case "response.completed":
		s.history, s.historyResponse = nil, ""
		if len(s.pendingInputs) == 0 || s.pendingInputs[0] == nil || event.Response.ID == "" {
			return
		}
		history := make([]json.RawMessage, 0, len(s.pendingInputs[0])+len(s.turnOutput))
		history = append(append(history, s.pendingInputs[0]...), s.turnOutput...)
		size := 0
		for _, item := range history {
			size += len(item)
		}
		if size > codexWebSocketHistoryLimit {
			return
		}
		s.history, s.historyResponse = history, event.Response.ID
	}
}

// rebuildPendingCreate turns the chained response.create in flight into a
// self-contained one (its full input, no previous_response_id) that any
// connection of the same account can serve, and keeps it as the turn's body
// for later retries. nil when the conversation is not known.
func (s *webSocketModelState) rebuildPendingCreate() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pendingBodies) == 0 || len(s.pendingInputs) == 0 || s.pendingInputs[0] == nil {
		return nil
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(s.pendingBodies[0], &event); err != nil {
		return nil
	}
	input, err := json.Marshal(s.pendingInputs[0])
	if err != nil {
		return nil
	}
	if _, top := event["previous_response_id"]; top || event["response"] == nil {
		delete(event, "previous_response_id")
		event["input"] = input
	} else {
		var response map[string]json.RawMessage
		if err := json.Unmarshal(event["response"], &response); err != nil {
			return nil
		}
		delete(response, "previous_response_id")
		response["input"] = input
		if event["response"], err = json.Marshal(response); err != nil {
			return nil
		}
	}
	rebuilt, err := json.Marshal(event)
	if err != nil {
		return nil
	}
	s.pendingBodies[0] = rebuilt
	return rebuilt
}

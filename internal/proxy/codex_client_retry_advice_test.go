package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestCodexCapacityClientRetryAdvice(t *testing.T) {
	for _, tc := range []struct {
		name, body, want   string
		advised, unchanged bool
	}{
		{"failed response", `{"type":"response.failed","response":{"id":"r","opaque_id":9007199254740993,"error":{"code":"server_is_overloaded","message":"busy"}}}`, "1", true, false},
		{"error frame", `{"type":"error","status":503,"error":{"code":"server_is_overloaded","message":"busy"}}`, "1", true, false},
		{"existing advice", `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","headers":{"retry-after":"120","X-Test":"kept"}}}}`, "120", true, true},
		{"zero advice", `{"type":"error","error":{"code":"server_is_overloaded","headers":{"Retry-After":"0"}}}`, "0", true, true},
		{"date advice", `{"type":"error","error":{"code":"server_is_overloaded","headers":{"Retry-After":"Wed, 28 Oct 2026 19:00:00 GMT"}}}`, "Wed, 28 Oct 2026 19:00:00 GMT", true, true},
		{"body delay", `{"type":"error","error":{"code":"server_is_overloaded","retry_after_ms":1250}}`, "2", true, false},
		{"invalid advice", `{"type":"error","error":{"code":"server_is_overloaded","headers":{"Retry-After":"bad","X-Test":"kept"}}}`, "1", true, false},
		{"newline advice", `{"type":"error","error":{"code":"server_is_overloaded","headers":{"Retry-After":"\n5\n"}}}`, "1", true, false},
		{"tab advice", `{"type":"error","error":{"code":"server_is_overloaded","headers":{"Retry-After":"\t5\t"}}}`, "\t5\t", true, true},
		{"quota", `{"type":"error","error":{"code":"usage_limit_reached"}}`, "", false, true},
		{"unknown", `{"type":"response.failed","response":{"error":{"code":"unknown_failure"}}}`, "", false, true},
		{"malformed", `{`, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, advised := codexCapacityClientRetryAdvice([]byte(tc.body))
			if advised != tc.advised || tc.unchanged && string(got) != tc.body {
				t.Fatalf("advised=%v body=%s", advised, got)
			}
			if !advised {
				return
			}
			var before, after map[string]any
			json.Unmarshal([]byte(tc.body), &before)
			json.Unmarshal(got, &after)
			container := after
			if response, ok := after["response"].(map[string]any); ok {
				container = response
			}
			err := container["error"].(map[string]any)
			headers := err["headers"].(map[string]any)
			value := headers["Retry-After"]
			if value == nil {
				value = headers["retry-after"]
			}
			if value != tc.want || err["code"] != "server_is_overloaded" {
				t.Fatalf("error changed incorrectly: %s", got)
			}
			if bytes.Contains([]byte(tc.body), []byte("X-Test")) && headers["X-Test"] != "kept" {
				t.Fatal("unrelated header lost")
			}
			if bytes.Contains([]byte(tc.body), []byte("9007199254740993")) && !bytes.Contains(got, []byte("9007199254740993")) {
				t.Fatal("opaque integer metadata lost precision")
			}
			beforeContainer := before
			if response, ok := before["response"].(map[string]any); ok {
				beforeContainer = response
			}
			delete(beforeContainer["error"].(map[string]any), "headers")
			delete(err, "headers")
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("non-header payload changed: %s", got)
			}
		})
	}
}

func TestCodexClientRetryAdviceKeepsBufferBudget(t *testing.T) {
	for _, full := range []bool{false, true} {
		limit := int64(4096)
		if full {
			limit = int64(len(turnRetryCapacityFailure))
		}
		budget := newWebSocketByteBudget(limit)
		writer := &bufferedWebSocketWriter{budget: budget, clientRetryAdvice: true}
		writer.Write([]byte(turnRetryCapacityFailure))
		err := writer.addClientRetryAdvice()
		if full {
			if !errors.Is(err, errWebSocketAdmissionBufferFull) {
				t.Fatalf("full budget: %v", err)
			}
		} else if err != nil || budget.used != int64(writer.buffer.Len()) {
			t.Fatalf("rewrite budget mismatch: %d, %v", budget.used, err)
		}
		writer.discard()
		if budget.used != 0 {
			t.Fatalf("rewrite retained %d bytes", budget.used)
		}
	}
}

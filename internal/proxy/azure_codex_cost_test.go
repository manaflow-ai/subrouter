package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAzureCodexUsageFromJSONAndSSE(t *testing.T) {
	usage, model, ok := azureCodexUsageFromBody([]byte(`{"object":"response","model":"gpt-5.3-codex","usage":{"input_tokens":1738,"output_tokens":16,"input_tokens_details":{"cached_tokens":1536,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":8}}}`))
	if !ok || model != "gpt-5.3-codex" {
		t.Fatalf("json usage = %+v %q %v", usage, model, ok)
	}
	if usage.InputTokens != 1738 || usage.CachedTokens != 1536 || usage.OutputTokens != 16 || usage.ReasoningTokens != 8 {
		t.Fatalf("json usage = %+v", usage)
	}
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-5.3-codex\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.3-codex\",\"usage\":{\"input_tokens\":10,\"output_tokens\":4,\"input_tokens_details\":{\"cached_tokens\":0}}}}\n\n"
	usage, model, ok = azureCodexUsageFromBody([]byte(stream))
	if !ok || model != "gpt-5.3-codex" || usage.InputTokens != 10 || usage.OutputTokens != 4 {
		t.Fatalf("sse usage = %+v %q %v", usage, model, ok)
	}
	if _, _, ok := azureCodexUsageFromBody([]byte(`{"object":"response","model":"gpt-5.3-codex"}`)); ok {
		t.Fatal("a response with no usage reported usage")
	}
}

// Cached tokens are reported inside input_tokens. Charging both at the full
// rate would overstate every sticky Codex turn, which is most of them.
func TestAzureCodexCostDoesNotDoubleCountCachedInput(t *testing.T) {
	usage := azureCodexUsage{InputTokens: 1000, CachedTokens: 800, OutputTokens: 100}
	got := usage.costUSD("gpt-5.3-codex")
	want := (200*1.25 + 800*0.125 + 100*10) / 1_000_000
	if got != want {
		t.Fatalf("cost = %v, want %v", got, want)
	}
	// An unpriced model must never invent spend.
	if usage.costUSD("some-unknown-deployment") != 0 {
		t.Fatal("an unknown model was priced")
	}
}

func TestAzureCodexCostBodyRecordsOnceOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "azure-cost.jsonl")
	moment := time.Date(2026, 8, 18, 4, 0, 0, 0, time.UTC)
	body := newAzureCodexCostBody(
		io.NopCloser(strings.NewReader(`{"object":"response","model":"gpt-5.3-codex","usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":40}}}`)),
		path,
		azureCodexCostRecord{Endpoint: "eastus2", Model: "gpt-5.6-codex", Deployment: "gpt-5.3-codex", Reason: "forced", Status: 200},
		func() time.Time { return moment },
	)
	if _, err := io.ReadAll(body); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
	if len(lines) != 1 {
		t.Fatalf("records = %d, want 1; EOF and Close must not both record", len(lines))
	}
	var record azureCodexCostRecord
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatal(err)
	}
	if record.Usage.InputTokens != 100 || record.Usage.CachedTokens != 40 || record.Reason != "forced" {
		t.Fatalf("record = %+v", record)
	}
	if record.CostUSD <= 0 {
		t.Fatalf("cost = %v, want a priced request", record.CostUSD)
	}

	summary := summarizeAzureCodexCost(path)
	if summary.Requests != 1 || summary.CachedTokens != 40 || summary.ByReason["forced"] != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.ByModel["gpt-5.3-codex"].Requests != 1 {
		t.Fatalf("by model = %+v", summary.ByModel)
	}
}

// A client that hangs up mid-stream still cost money on the provider side, so
// the request has to be recorded.
func TestAzureCodexCostBodyRecordsAnAbandonedStream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "azure-cost.jsonl")
	body := newAzureCodexCostBody(
		io.NopCloser(strings.NewReader("event: x\ndata: {\"response\":{\"model\":\"gpt-5.3-codex\",\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n")),
		path,
		azureCodexCostRecord{Endpoint: "eastus2", Deployment: "gpt-5.3-codex", Status: 200},
		nil,
	)
	buffer := make([]byte, 8)
	if _, err := body.Read(buffer); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(contents)) == "" {
		t.Fatal("an abandoned stream recorded nothing")
	}
}

// End to end: a served fallback request lands in the cost log and shows up on
// the admin endpoint, the same way Bedrock spend does.
func TestAzureCodexCostEndpointReportsServedRequests(t *testing.T) {
	costPath := filepath.Join(t.TempDir(), "azure-cost.jsonl")
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"response","model":"gpt-5.3-codex","usage":{"input_tokens":2000,"output_tokens":20,"input_tokens_details":{"cached_tokens":1500}}}`)
	})
	poolURL, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	server := azureCodexFallbackServer(t, azureURL, poolURL, 0)
	server.AzureCodex.CostLogPath = costPath
	server.AdminToken = "admin-token"
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	response, err := http.Post(proxy.URL+"/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5.6-codex","session_id":"cost-session"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}

	request, err := http.NewRequest(http.MethodGet, proxy.URL+"/_subrouter/azure-codex-cost", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer admin-token")
	costResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer costResponse.Body.Close()
	var summary azureCodexCostSummary
	if err := json.NewDecoder(costResponse.Body).Decode(&summary); err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 1 || summary.CachedTokens != 1500 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.ByReason["no_usable_account"] != 1 {
		t.Fatalf("by reason = %+v, want the fallback reason recorded", summary.ByReason)
	}
	if summary.TotalUSD <= 0 {
		t.Fatalf("total = %v, want priced spend", summary.TotalUSD)
	}
}

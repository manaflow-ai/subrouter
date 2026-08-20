package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const srAzHelp = `sr az - Route Codex through the Azure OpenAI fallback

Usage:
  sr az test [model]    Send one request through the daemon, forced to Azure
  sr az status          Show which Azure endpoints the daemon has armed
  sr az cost            Show what the Azure fallback has spent
  sr az codex [args]    Run Codex with every request forced to Azure

The fallback normally runs on its own, after the Codex pool has spent its
retries. These commands force it, so the route can be proven without waiting
for an outage.
`

// azureForceHeader is the request header the daemon reads to skip the pool.
const azureForceHeader = "X-Subrouter-Azure"

// gpt-5.6-sol: the gpt-5.3-codex Azure deployment was deleted on 2026-08-20
// (no gpt-5.3 usage is allowed via Azure), and the live fallback gate only
// serves the gpt-5.6 family anyway.
const defaultAzureTestModel = "gpt-5.6-sol"

func (r srRunner) az(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return r.azStatus(ctx)
	}
	switch args[0] {
	case "status":
		return r.azStatus(ctx)
	case "test", "check":
		return r.azTest(ctx, args[1:])
	case "cost", "spend":
		return r.azCost(ctx)
	case "codex":
		return azCodex(args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(r.out, srAzHelp)
		return nil
	default:
		return fmt.Errorf("unknown command: sr az %s\n%s", args[0], srAzHelp)
	}
}

// azStatus reports the endpoints the daemon armed, which is the difference
// between "the fallback is configured" and "the binary supports it".
func (r srRunner) azStatus(ctx context.Context) error {
	baseURL, err := azBaseURL()
	if err != nil {
		return err
	}
	health, err := azHealth(ctx, baseURL)
	if err != nil {
		return err
	}
	if len(health.AzureCodex) == 0 {
		fmt.Fprintf(r.out, "Azure fallback is OFF at %s\n", baseURL)
		fmt.Fprintln(r.out, "Set SUBROUTER_AZURE_CODEX_ENDPOINT and SUBROUTER_AZURE_CODEX_API_KEY on the daemon, then restart it.")
		return nil
	}
	fmt.Fprintf(r.out, "Azure fallback is ON at %s\n", baseURL)
	for _, endpoint := range health.AzureCodex {
		fmt.Fprintf(r.out, "  %s\n", endpoint)
	}
	return nil
}

type azHealthResponse struct {
	AzureCodex []string `json:"azure_codex"`
}

func azHealth(ctx context.Context, baseURL string) (azHealthResponse, error) {
	var health azHealthResponse
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/_subrouter/health", nil)
	if err != nil {
		return health, err
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return health, fmt.Errorf("reach the Subrouter daemon at %s: %w", baseURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return health, err
	}
	if response.StatusCode != http.StatusOK {
		return health, fmt.Errorf("daemon health at %s returned %d", baseURL, response.StatusCode)
	}
	if err := json.Unmarshal(body, &health); err != nil {
		return health, fmt.Errorf("parse daemon health: %w", err)
	}
	return health, nil
}

// azTest sends one small Responses request with the force header and reports
// what happened, including the cached-token counts that show whether the prompt
// cache is being reused across turns.
func (r srRunner) azTest(ctx context.Context, args []string) error {
	model := defaultAzureTestModel
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		model = strings.TrimSpace(args[0])
	}
	baseURL, err := azBaseURL()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"model":             model,
		"instructions":      azCachePrefix(),
		"input":             "Reply with the single word OK.",
		"max_output_tokens": 16,
		"reasoning":         map[string]any{"effort": "low"},
		"prompt_cache_key":  "sr-az-test",
	})
	if err != nil {
		return err
	}
	target := strings.TrimRight(baseURL, "/") + "/responses"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Subrouter-Agent", "codex")
	request.Header.Set(azureForceHeader, "force")
	started := time.Now()
	response, err := (&http.Client{Timeout: 5 * time.Minute}).Do(request)
	if err != nil {
		return fmt.Errorf("reach the Subrouter daemon at %s: %w", baseURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		fmt.Fprintf(r.out, "FAILED  %s  status %d\n%s\n", target, response.StatusCode, strings.TrimSpace(string(body)))
		return errors.New("the Azure route did not serve the request")
	}
	summary := azResponseSummary(body)
	fmt.Fprintf(r.out, "OK  %s  %s  deployment=%s  input=%d cached=%d output=%d\n",
		target, time.Since(started).Round(time.Millisecond), summary.Model,
		summary.InputTokens, summary.CachedTokens, summary.OutputTokens)
	if summary.Text != "" {
		fmt.Fprintf(r.out, "    reply: %s\n", summary.Text)
	}
	if summary.CachedTokens > 0 {
		fmt.Fprintln(r.out, "    cached tokens > 0: the prompt cache was reused, which is the stickiness working.")
	} else {
		fmt.Fprintln(r.out, "    run it again within 30 minutes; the second run should report cached tokens.")
	}
	return nil
}

// azCachePrefix is a fixed instruction block long enough to be cacheable. A
// provider caches nothing below about 1024 identical leading tokens, so a
// one-line probe can never show a cache hit and would make a working cache look
// broken.
func azCachePrefix() string {
	const line = "This is a Subrouter Azure fallback cache probe. The text repeats so the prompt prefix is long enough for the provider to cache it, and it is identical on every run so a later run can hit that cache. "
	return strings.Repeat(line, 40)
}

type azSummary struct {
	Model        string
	Text         string
	InputTokens  int
	CachedTokens int
	OutputTokens int
}

// azResponseSummary pulls the few fields worth printing out of a Responses
// body, tolerating both the JSON and SSE forms.
func azResponseSummary(body []byte) azSummary {
	summary := azSummary{}
	payload := azResponseObject(body)
	if payload == nil {
		return summary
	}
	if model, ok := payload["model"].(string); ok {
		summary.Model = model
	}
	if usage, ok := payload["usage"].(map[string]any); ok {
		summary.InputTokens = azInt(usage["input_tokens"])
		summary.OutputTokens = azInt(usage["output_tokens"])
		if details, ok := usage["input_tokens_details"].(map[string]any); ok {
			summary.CachedTokens = azInt(details["cached_tokens"])
		}
	}
	summary.Text = azOutputText(payload)
	return summary
}

// azResponseObject finds the response object in a plain JSON body or in the
// final event of an SSE stream.
func azResponseObject(body []byte) map[string]any {
	var direct map[string]any
	if json.Unmarshal(body, &direct) == nil {
		if response, ok := direct["response"].(map[string]any); ok {
			return response
		}
		if direct["object"] == "response" {
			return direct
		}
	}
	var last map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) != nil {
			continue
		}
		if response, ok := event["response"].(map[string]any); ok {
			last = response
		}
	}
	return last
}

func azOutputText(payload map[string]any) string {
	output, ok := payload["output"].([]any)
	if !ok {
		return ""
	}
	for _, item := range output {
		entry, ok := item.(map[string]any)
		if !ok || entry["type"] != "message" {
			continue
		}
		contents, ok := entry["content"].([]any)
		if !ok {
			continue
		}
		for _, content := range contents {
			block, ok := content.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func azInt(value any) int {
	number, ok := value.(float64)
	if !ok {
		return 0
	}
	return int(number)
}

// azCodex runs Codex with every request forced onto the Azure route, through
// the same daemon and session bookkeeping as `sr codex`.
func azCodex(args []string) error {
	baseURL, err := azBaseURL()
	if err != nil {
		return err
	}
	bin := envOrDefault("SUBROUTER_CODEX_BIN", "codex")
	cmd := exec.CommandContext(context.Background(), bin, azCodexArgs(args, baseURL+"/v1")...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// azCodexArgs pins Codex to a Subrouter provider that carries the force header.
// WebSockets are off: Azure has no WebSocket Responses surface, so a forced
// session must speak HTTP.
func azCodexArgs(args []string, baseURL string) []string {
	configArgs := []string{
		"-c", `model_provider="subrouter-azure"`,
		"-c", `model_providers.subrouter-azure.name="Subrouter Azure"`,
		"-c", "model_providers.subrouter-azure.base_url=" + strconv.Quote(baseURL),
		"-c", `model_providers.subrouter-azure.experimental_bearer_token="subrouter"`,
		"-c", `model_providers.subrouter-azure.wire_api="responses"`,
		"-c", `model_providers.subrouter-azure.supports_websockets=false`,
		"-c", `model_providers.subrouter-azure.http_headers={"X-Subrouter-Agent"="codex","` + azureForceHeader + `"="force"}`,
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") || !isKnownCodexCommand(args[0]) {
		return append(configArgs, args...)
	}
	if !isSubrouterRoutedCodexCommand(args[0]) {
		return args
	}
	return append([]string{args[0]}, append(configArgs, args[1:]...)...)
}

// azBaseURL resolves the daemon this machine routes Codex through, so `sr az`
// tests the same server the agents use. The /v1 suffix Codex needs is trimmed:
// these commands talk to the daemon's own paths.
func azBaseURL() (string, error) {
	baseURL, err := codexBaseURL(defaultSRServerStore(accounts.DefaultCodexStore()))
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1"), nil
}

// azCost prints the metered spend of the Azure route. The subscription pool is
// a flat cost; this one is not, so it is the number to watch after turning the
// fallback on.
func (r srRunner) azCost(ctx context.Context) error {
	baseURL, err := azBaseURL()
	if err != nil {
		return err
	}
	server := srServerConfig{URL: strings.TrimRight(baseURL, "/")}
	summary, err := r.fetchAzureCodexSummary(ctx, server)
	if err != nil {
		return err
	}
	if summary.Requests == 0 {
		fmt.Fprintf(r.out, "No Azure Codex requests recorded at %s\n", server.URL)
		return nil
	}
	fmt.Fprintf(r.out, "Azure Codex spend at %s\n", server.URL)
	fmt.Fprintf(r.out, "  today $%s · 7d $%s · 30d $%s · all-time $%s (%d req)\n",
		fmtUSD4(summary.TodayUSD), fmtUSD4(summary.Week7dUSD), fmtUSD4(summary.Month30dUSD),
		fmtUSD4(summary.TotalUSD), summary.Requests)
	fmt.Fprintf(r.out, "  tokens in %d (cached %d) · out %d\n",
		summary.InputTokens, summary.CachedTokens, summary.OutputTokens)
	models := make([]string, 0, len(summary.ByModel))
	for model := range summary.ByModel {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		agg := summary.ByModel[model]
		fmt.Fprintf(r.out, "  %-24s $%s (%d req)\n", model, fmtUSD4(agg.TotalUSD), agg.Requests)
	}
	return nil
}

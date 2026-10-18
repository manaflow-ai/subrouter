package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnalyzeMergesClientsAndKeepsCacheSubsetsSeparate(t *testing.T) {
	input := `{"hour":"2026-01-01T01:00:00Z","provider":"claude","account_id":"private-account","model":"example","client":"private-device-one","requests":4,"requests_without_usage":2,"input_tokens":200,"cached_input_tokens":150,"cache_write_input_tokens":20,"output_tokens":40}
{"hour":"2026-01-01T01:00:00Z","provider":"claude","account_id":"private-account","model":"example","client":"private-device-two","requests":1,"input_tokens":100,"cached_input_tokens":50,"output_tokens":20}
{"hour":"2026-01-01T02:00:00Z","provider":"claude","account_id":"another-private-account","model":"example","requests":1,"input_tokens":1000,"output_tokens":200}`
	result, err := analyze(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if result.QuotaCalibrationSupported {
		t.Fatal("hourly totals cannot establish quota calibration")
	}
	if len(result.Models) != 1 {
		t.Fatalf("models: %+v", result.Models)
	}
	got := result.Models[0]
	if got.AccountHours != 2 || got.Requests != 6 || got.RequestsWithUsage != 4 {
		t.Fatalf("counts: %+v", got)
	}
	if *got.MeanInputTokens != 325 || *got.MeanOutputTokens != 65 || *got.MeanCachedInputTokens != 50 || *got.MeanCacheWriteInputTokens != 5 {
		t.Fatalf("means: %+v", got)
	}
	if *got.MinBucketMeanTokens != 120 || *got.MaxBucketMeanTokens != 1200 {
		t.Fatalf("bucket means: %+v", got)
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "private-account") || strings.Contains(string(body), "private-device") {
		t.Fatalf("private labels in report: %s", body)
	}
}

func TestAnalyzeEndpointAndUnobservedRequests(t *testing.T) {
	result, err := analyze(strings.NewReader(`{"rows":[{"hour":"2026-01-01T00:00:00Z","provider":"codex","account_id":"a","model":"example","requests":3,"requests_without_usage":3}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got := result.Models[0]
	if got.MeanInputTokens != nil || got.MinBucketMeanTokens != nil || got.MissingUsageFraction != 1 {
		t.Fatalf("missing usage must not appear as zero cost: %+v", got)
	}
}

func TestAnalyzeRejectsInvalidCountersWithoutEchoingInput(t *testing.T) {
	for _, input := range []string{
		`{"hour":"private-content"}`,
		`{"hour":"2026-01-01T00:01:00Z","provider":"codex","account_id":"a","model":"x"}`,
		`{"hour":"2026-01-01T00:00:00Z","provider":"codex","account_id":"a","model":"x","requests":1,"requests_without_usage":2}`,
		`{"hour":"2026-01-01T00:00:00Z","provider":"codex","account_id":"a","model":"x","requests":1,"input_tokens":5,"cached_input_tokens":6}`,
		`{"hour":"2026-01-01T00:00:00Z","provider":"codex","account_id":"a","model":"x","input_tokens":5}`,
		`{"access_token":"private-content"`,
	} {
		_, err := analyze(strings.NewReader(input))
		if err == nil {
			t.Fatalf("accepted %s", input)
		}
		if strings.Contains(err.Error(), "private-content") {
			t.Fatalf("echoed input: %v", err)
		}
	}
}

func TestAnalyzeDeterministicEmptyReport(t *testing.T) {
	result, err := analyze(strings.NewReader(`{"rows":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Models) != 0 || result.Models == nil {
		t.Fatalf("empty report: %+v", result)
	}
}

func TestAnalyzeNormalizesHourAndRejectsOverflow(t *testing.T) {
	row := `{"hour":"2026-01-01T00:00:00Z","provider":"codex","account_id":"a","model":"example","requests":1}`
	equivalent := strings.Replace(row, "2026-01-01T00:00:00Z", "2025-12-31T19:00:00-05:00", 1)
	result, err := analyze(strings.NewReader(row + "\n" + equivalent))
	if err != nil {
		t.Fatal(err)
	}
	if result.Models[0].AccountHours != 1 || result.Models[0].Requests != 2 {
		t.Fatalf("equivalent hour: %+v", result.Models)
	}
	huge := strings.Replace(row, `"requests":1`, `"requests":9223372036854775807`, 1)
	if _, err := analyze(strings.NewReader(huge + "\n" + row)); err == nil {
		t.Fatal("accepted overflow")
	}
}

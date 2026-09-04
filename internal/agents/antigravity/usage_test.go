package antigravity

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchUsagePreservesIdentityPlanFamiliesAndCadences(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer account-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/userinfo":
			_, _ = io.WriteString(w, `{"email":"verified@example.com","verified_email":true}`)
		case loadCodeAssistPath:
			_, _ = io.WriteString(w, `{"planInfo":{"planType":"Google AI Ultra"},"cloudaicompanionProject":{"id":"project-1"}}`)
		case retrieveSummaryPath:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["project"] != "project-1" {
				t.Fatalf("project = %#v", body["project"])
			}
			metadata, _ := body["metadata"].(map[string]any)
			if metadata["ideName"] != "antigravity" || metadata["extensionName"] != "antigravity" || metadata["locale"] != "en" || metadata["ideVersion"] != "unknown" {
				t.Fatalf("metadata = %#v", metadata)
			}
			_, _ = io.WriteString(w, `{"groups":[
				{"displayName":"Models","buckets":[
					{"bucketId":"gemini-5h","remainingFraction":0.75,"resetTime":"2026-09-01T13:00:00Z"},
					{"bucketId":"gemini-weekly","remaining":{"case":"remainingFraction","value":0.40},"resetTime":"2026-09-03T12:00:00Z"},
					{"bucketId":"3p-5h","remainingFraction":0.10,"resetTime":"2026-09-01T12:30:00Z"},
					{"bucketId":"3p-weekly","remainingFraction":0.90,"resetTime":"2026-09-06T12:00:00Z"}]}
			]}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	details, err := (usageFetcher{baseURL: server.URL, userInfoURL: server.URL + "/userinfo"}).fetch(
		context.Background(), server.Client(), "account-token", now)
	if err != nil {
		t.Fatal(err)
	}
	if details.Email != "verified@example.com" || details.Plan != "Google AI Ultra" {
		t.Fatalf("identity = %+v", details)
	}
	wantPaths := []string{"/userinfo", loadCodeAssistPath, retrieveSummaryPath}
	if strings.Join(paths, ",") != strings.Join(wantPaths, ",") {
		t.Fatalf("endpoint order = %v, want %v", paths, wantPaths)
	}
	want := map[string]struct {
		used, reset float64
		seconds     int64
	}{
		"gemini 5h":         {25, 3600, 18000},
		"gemini weekly":     {60, 172800, 604800},
		"claude-gpt 5h":     {90, 1800, 18000},
		"claude-gpt weekly": {10, 432000, 604800},
	}
	if len(details.Windows) != len(want) {
		t.Fatalf("windows = %+v", details.Windows)
	}
	for _, window := range details.Windows {
		expected, ok := want[window.Name]
		if !ok || math.Abs(window.UsedPercent-expected.used) > 0.0001 || window.ResetAfterSeconds != int64(expected.reset) || window.LimitWindowSeconds != expected.seconds {
			t.Fatalf("window = %+v want=%+v", window, expected)
		}
	}
}

func TestQuotaSummaryMissingDisabledAndFamiliesRemainIndependent(t *testing.T) {
	var payload any
	if err := json.Unmarshal([]byte(`{"groups":[
		{"displayName":"Gemini","buckets":[
			{"bucketId":"5h","remainingFraction":0},
			{"bucketId":"weekly"}]},
		{"displayName":"Claude/GPT","buckets":[
			{"bucketId":"5h","remainingFraction":1},
			{"bucketId":"weekly","remainingFraction":0.5,"disabled":true}]}
	]}`), &payload); err != nil {
		t.Fatal(err)
	}
	windows := quotaSummaryWindows(payload, time.Now())
	if len(windows) != 2 {
		t.Fatalf("windows = %+v, want only known enabled buckets", windows)
	}
	got := map[string]float64{}
	for _, window := range windows {
		got[window.Name] = window.UsedPercent
	}
	if got["gemini 5h"] != 100 || got["claude-gpt 5h"] != 0 {
		t.Fatalf("independent family exhaustion lost: %+v", got)
	}
}

func TestFetchUsageFallsBackToPerFamilyModelQuotaWithoutInventingCadence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/userinfo":
			_, _ = io.WriteString(w, `{"email":"unverified@example.com","verified_email":false}`)
		case loadCodeAssistPath:
			_, _ = io.WriteString(w, `{"currentTier":{"id":"free-tier"}}`)
		case retrieveSummaryPath:
			http.Error(w, "not enabled", http.StatusForbidden)
		case retrieveUserQuotaPath:
			_, _ = io.WriteString(w, `{"buckets":[]}`)
		case fetchModelsPath:
			_, _ = io.WriteString(w, `{"models":{
				"gemini-3-pro":{"quotaInfo":{"remainingFraction":0.2}},
				"claude-sonnet":{"quotaInfo":{"remainingFraction":0.7}},
				"other":{"quotaInfo":{"remainingFraction":0}}
			}}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	details, err := (usageFetcher{baseURL: server.URL, userInfoURL: server.URL + "/userinfo"}).fetch(
		context.Background(), server.Client(), "token", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if details.Email != "" || details.Plan != "Starter" || len(details.Windows) != 3 {
		t.Fatalf("details = %+v", details)
	}
	for _, window := range details.Windows {
		if strings.Contains(window.Name, "5h") || strings.Contains(window.Name, "weekly") || window.LimitWindowSeconds != 0 {
			t.Fatalf("invented cadence: %+v", window)
		}
	}
	used := map[string]float64{}
	for _, window := range details.Windows {
		used[window.Feature] = window.UsedPercent
	}
	if math.Abs(used["claude-sonnet"]-30) > 0.0001 || math.Abs(used["gemini-3-pro"]-80) > 0.0001 || math.Abs(used["other"]-100) > 0.0001 {
		t.Fatalf("model percentages = %+v", used)
	}
}

func TestLegacyModelQuotaPreservesExactModelPools(t *testing.T) {
	var payload any
	if err := json.Unmarshal([]byte(`{"models":{
		"claude-sonnet-4.5":{"quotaInfo":{"remainingFraction":0}},
		"claude-opus-4.1":{"quotaInfo":{"remainingFraction":0.8}}
	}}`), &payload); err != nil {
		t.Fatal(err)
	}
	windows := modelQuotaWindows(payload, time.Now())
	if len(windows) != 2 {
		t.Fatalf("windows = %+v", windows)
	}
	used := map[string]float64{}
	for _, window := range windows {
		if window.Name != window.Feature {
			t.Fatalf("window lost exact identity: %+v", window)
		}
		used[window.Feature] = window.UsedPercent
	}
	if used["claude-sonnet-4.5"] != 100 || math.Abs(used["claude-opus-4.1"]-20) > 0.0001 {
		t.Fatalf("exact pools = %+v", used)
	}
}

func TestFetchUsageContinuesWhenUserInfoScopeIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/userinfo":
			http.Error(w, "insufficient scope", http.StatusForbidden)
		case loadCodeAssistPath:
			_, _ = io.WriteString(w, `{"planInfo":{"planType":"Paid"}}`)
		case retrieveSummaryPath:
			_, _ = io.WriteString(w, `{"groups":[{"displayName":"Gemini models","buckets":[{"bucketId":"gemini-5h","remainingFraction":0.5}]}]}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	details, err := (usageFetcher{baseURL: server.URL, userInfoURL: server.URL + "/userinfo"}).fetch(
		context.Background(), server.Client(), "scoped-token", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if details.Email != "" || details.Plan != "Paid" || len(details.Windows) != 1 || details.Windows[0].Name != "gemini 5h" {
		t.Fatalf("details = %+v", details)
	}
}

func TestQuotaSummarySkipsUnidentifiedFamilies(t *testing.T) {
	var payload any
	if err := json.Unmarshal([]byte(`{"groups":[{"displayName":"Models","buckets":[{"bucketId":"mystery-5h","remainingFraction":0.5}]}]}`), &payload); err != nil {
		t.Fatal(err)
	}
	if windows := quotaSummaryWindows(payload, time.Now()); len(windows) != 0 {
		t.Fatalf("windows = %+v, want unidentified family omitted", windows)
	}
}

func TestUsageRequestNeverLeaksTokenInErrors(t *testing.T) {
	secret := "secret-access-token"
	client := &http.Client{Transport: usageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(secret)), Request: req}, nil
	})}
	_, err := (usageFetcher{baseURL: "https://cloudcode-pa.googleapis.com", userInfoURL: "https://www.googleapis.com/userinfo"}).fetch(
		context.Background(), client, secret, time.Now())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %v", err)
	}
}

type usageRoundTripFunc func(*http.Request) (*http.Response, error)

func (f usageRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestDefaultAntigravityFetchBaseURLs(t *testing.T) {
	want := []string{
		antigravityBaseURLDaily,
		antigravityBaseURLProd,
		antigravitySandboxBaseURLDaily,
	}

	got := defaultAntigravityFetchBaseURLs()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaultAntigravityFetchBaseURLs() = %#v, want %#v", got, want)
	}
}

func TestFetchModelsRetryPerEndpoint(t *testing.T) {
	var endpoint1Calls atomic.Int32
	var endpoint2Calls atomic.Int32

	server1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := endpoint1Calls.Add(1)
		if call == 1 {
			http.Error(w, `{"error":"temporary server error"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"models": {
				"gemini-3.6-flash": {
					"displayName": "Gemini 3.6 Flash",
					"maxTokens": 1048576,
					"maxOutputTokens": 8192
				}
			}
		}`))
	}))
	defer server1.Close()

	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoint2Calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"models": {
				"gemini-1.5-pro": {
					"displayName": "Gemini 1.5 Pro"
				}
			}
		}`))
	}))
	defer server2.Close()

	auth := &coreauth.Auth{
		Metadata: map[string]interface{}{
			"access_token": "test-token",
			"project_id":   "test-project",
		},
	}

	// Case 1: First endpoint fails on attempt 1, succeeds on attempt 2.
	// It should succeed without calling endpoint 2, and endpoint 1 should have been called 2 times.
	models := fetchModelsFromBaseURLs(context.Background(), auth, []string{server1.URL, server2.URL}, server1.Client())
	if len(models) != 1 || models[0].ID != "gemini-3.6-flash" {
		t.Fatalf("expected 1 model (gemini-3.6-flash), got: %#v", models)
	}
	if calls := endpoint1Calls.Load(); calls != 2 {
		t.Fatalf("expected endpoint 1 to be called 2 times, got %d", calls)
	}
	if calls := endpoint2Calls.Load(); calls != 0 {
		t.Fatalf("expected endpoint 2 not to be called, got %d", calls)
	}
}

func TestFetchModelsFallbackAfterTwoAttempts(t *testing.T) {
	var endpoint1Calls atomic.Int32
	var endpoint2Calls atomic.Int32

	server1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoint1Calls.Add(1)
		http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer server1.Close()

	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoint2Calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"models": {
				"gemini-2.5-flash": {
					"displayName": "Gemini 2.5 Flash"
				}
			}
		}`))
	}))
	defer server2.Close()

	auth := &coreauth.Auth{
		Metadata: map[string]interface{}{
			"access_token": "test-token",
		},
	}

	// Case 2: First endpoint fails all 2 attempts, then falls back to endpoint 2.
	models := fetchModelsFromBaseURLs(context.Background(), auth, []string{server1.URL, server2.URL}, server1.Client())
	if len(models) != 1 || models[0].ID != "gemini-2.5-flash" {
		t.Fatalf("expected 1 model (gemini-2.5-flash), got: %#v", models)
	}
	if calls := endpoint1Calls.Load(); calls != 2 {
		t.Fatalf("expected endpoint 1 to be called 2 times before fallback, got %d", calls)
	}
	if calls := endpoint2Calls.Load(); calls != 1 {
		t.Fatalf("expected endpoint 2 to be called 1 time, got %d", calls)
	}
}

func TestParseQuotaBucketsGroupsBySharedAllowance(t *testing.T) {
	raw := []byte(`{
		"models": {
			"gemini-3.8-flash-tiered": {"quotaInfo": {"remainingFraction": 0.098, "resetTime": "2026-09-17T22:10:00Z"}},
			"gemini-3.7-flash-tiered": {"quotaInfo": {"remainingFraction": 0.098, "resetTime": "2026-09-17T22:10:00Z"}},
			"claude-sonnet-4-6":       {"quotaInfo": {"remainingFraction": 1, "resetTime": "2026-09-17T23:35:31Z"}}
		}
	}`)

	buckets, errBuckets := parseQuotaBuckets(raw)
	if errBuckets != nil {
		t.Fatalf("parseQuotaBuckets() error = %v", errBuckets)
	}
	if len(buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d: %#v", len(buckets), buckets)
	}
	// Buckets sort ascending by remaining fraction, so the scarcer one comes first.
	first := buckets[0]
	if !first.HasRemain || first.Remaining > 0.099 {
		t.Fatalf("expected the 9.8%% bucket first, got %#v", first)
	}
	if len(first.Models) != 2 {
		t.Fatalf("expected the 9.8%% bucket to hold 2 models, got %#v", first.Models)
	}
	second := buckets[1]
	if !second.HasRemain || second.Remaining != 1 || len(second.Models) != 1 {
		t.Fatalf("expected the 100%% bucket holding 1 model, got %#v", second)
	}
}

func TestParseQuotaBucketsPutsExhaustedFirst(t *testing.T) {
	// An exhausted allowance omits remainingFraction entirely on the wire.
	raw := []byte(`{
		"models": {
			"gemini-3.8-flash-tiered": {"quotaInfo": {"resetTime": "2026-09-17T20:25:07Z"}},
			"claude-sonnet-4-6":       {"quotaInfo": {"remainingFraction": 1}}
		}
	}`)

	buckets, errBuckets := parseQuotaBuckets(raw)
	if errBuckets != nil {
		t.Fatalf("parseQuotaBuckets() error = %v", errBuckets)
	}
	if len(buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(buckets))
	}
	if buckets[0].HasRemain {
		t.Fatalf("expected the exhausted bucket first, got %#v", buckets[0])
	}
	if len(buckets[0].Models) != 1 || buckets[0].Models[0] != "gemini-3.8-flash-tiered" {
		t.Fatalf("exhausted bucket models = %#v", buckets[0].Models)
	}
}

func TestParseQuotaBucketsWithoutModelsObject(t *testing.T) {
	if _, errBuckets := parseQuotaBuckets([]byte(`{"foo": 1}`)); errBuckets == nil {
		t.Fatal("expected an error when the payload has no models object")
	}
}

func TestFormatResetFallsBackToRawValue(t *testing.T) {
	formatted := formatReset("2026-09-17T22:10:00Z")
	if !strings.Contains(formatted, "2026-09-17T22:10:00Z") {
		t.Fatalf("formatReset() = %q, want it to contain the UTC instant", formatted)
	}
	if !strings.Contains(formatted, "local ") {
		t.Fatalf("formatReset() = %q, want it to contain the local rendering", formatted)
	}
	if got := formatReset("not-a-timestamp"); got != "not-a-timestamp" {
		t.Fatalf("formatReset() on unparsable input = %q, want the raw value", got)
	}
}

func TestTruncateForLogCollapsesWhitespace(t *testing.T) {
	if got := truncateForLog("a\n\tb", 10); got != "a b" {
		t.Fatalf("truncateForLog() = %q, want %q", got, "a b")
	}
	long := strings.Repeat("x", 40)
	got := truncateForLog(long, 10)
	if got != strings.Repeat("x", 10)+"..." {
		t.Fatalf("truncateForLog() = %q, want a truncated value", got)
	}
}

// TestResolveToolUserAgentPrecedence pins the ordering that makes the config
// fallback work: auth attributes beat auth metadata beat config. Newly added
// accounts carry no per-auth UA, so the config value is what keeps them off the
// dynamic version that Google rejects.
func TestResolveToolUserAgentPrecedence(t *testing.T) {
	cfg := &config.Config{}
	cfg.Antigravity.UserAgent = "antigravity/1.11.5 windows/amd64"

	auth := &coreauth.Auth{Metadata: map[string]interface{}{}}
	if got := resolveToolUserAgent(auth, cfg); got != "antigravity/1.11.5 windows/amd64" {
		t.Fatalf("config fallback = %q, want the configured UA", got)
	}

	auth.Metadata["user_agent"] = "antigravity/1.0.0 windows/amd64"
	if got := resolveToolUserAgent(auth, cfg); got != "antigravity/1.0.0 windows/amd64" {
		t.Fatalf("metadata override = %q, want the metadata UA", got)
	}

	auth.Attributes = map[string]string{"user_agent": "antigravity/2.0.0 windows/amd64"}
	if got := resolveToolUserAgent(auth, cfg); got != "antigravity/2.0.0 windows/amd64" {
		t.Fatalf("attribute override = %q, want the attribute UA", got)
	}

	// With nothing configured anywhere the dynamic default is used.
	if got := resolveToolUserAgent(&coreauth.Auth{Metadata: map[string]interface{}{}}, &config.Config{}); got == "" {
		t.Fatal("expected a non-empty default UA when nothing is configured")
	}
}

package updater

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"reader/server/internal/config"
)

func TestCheckUpdateFallsBackAfterConfirmedRateLimit(t *testing.T) {
	assetName := ExpectedAssetName()
	var apiCalls, feedCalls, assetPageCalls, headCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.EqualFold(r.URL.Path, "/repos/houalibaba/Reader/releases/latest"):
			apiCalls.Add(1)
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
		case strings.EqualFold(r.URL.Path, "/houalibaba/reader/releases.atom"):
			feedCalls.Add(1)
			w.Header().Set("Content-Type", "application/atom+xml")
			_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom">
				<entry><title>Reader v0.4.0-rc1</title><link rel="alternate" href="/houalibaba/reader/releases/tag/v0.4.0-rc1"/></entry>
				<entry><title>Reader v0.3.99</title><updated>2026-11-20T10:00:00Z</updated><link rel="alternate" href="/houalibaba/reader/releases/tag/v0.3.99"/></entry>
			</feed>`)
		case strings.EqualFold(r.URL.Path, "/houalibaba/reader/releases/expanded_assets/v0.3.99"):
			assetPageCalls.Add(1)
			_, _ = fmt.Fprintf(w, `<a href="/houalibaba/reader/releases/download/v0.3.99/%s">binary</a>
				<a href="/houalibaba/reader/releases/download/v0.3.99/%s">runtime</a>
				<a href="/houalibaba/reader/releases/download/v0.3.99/checksums.txt">checksums</a>`, assetName, runtimeAssetName)
		case r.Method == http.MethodHead && strings.Contains(strings.ToLower(r.URL.Path), "/releases/download/"):
			headCalls.Add(1)
			if strings.HasSuffix(r.URL.Path, "/"+assetName) {
				w.Header().Set("Content-Length", "1234567")
				w.WriteHeader(http.StatusOK)
			} else if strings.HasSuffix(r.URL.Path, "/"+runtimeAssetName) {
				w.Header().Set("Content-Length", "45678")
				w.WriteHeader(http.StatusOK)
			} else {
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := config.DefaultConfig()
	cfg.Update.Repo = "houalibaba/Reader"
	u := New(cfg)
	u.apiBaseURL = server.URL
	u.githubBaseURL = server.URL

	result, err := u.CheckUpdate(context.Background(), "")
	if err != nil {
		t.Fatalf("CheckUpdate fallback error: %v", err)
	}
	if !result.HasUpdate || result.LatestVersion != "v0.3.99" {
		t.Fatalf("expected latest stable version v0.3.99, got %+v", result)
	}
	if result.AssetURL != server.URL+"/houalibaba/reader/releases/download/v0.3.99/"+assetName || result.AssetSize != 1234567 {
		t.Fatalf("binary asset URL/size mismatch: %+v", result)
	}
	if result.RuntimeAssetURL != server.URL+"/houalibaba/reader/releases/download/v0.3.99/"+runtimeAssetName || result.RuntimeAssetSize != 45678 {
		t.Fatalf("runtime asset URL/size mismatch: %+v", result)
	}
	if result.ChecksumURL != server.URL+"/houalibaba/reader/releases/download/v0.3.99/checksums.txt" {
		t.Fatalf("checksum URL mismatch: %+v", result)
	}
	if apiCalls.Load() != 1 || feedCalls.Load() != 1 || assetPageCalls.Load() != 1 || headCalls.Load() != 2 {
		t.Fatalf("unexpected request counts: api=%d feed=%d assets=%d HEAD=%d", apiCalls.Load(), feedCalls.Load(), assetPageCalls.Load(), headCalls.Load())
	}
}

func TestCheckUpdateDoesNotFallbackOnUnrelatedForbidden(t *testing.T) {
	var fallbackCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/releases.atom") || strings.Contains(r.URL.Path, "/expanded_assets/") {
			fallbackCalls.Add(1)
		}
		w.Header().Set("X-RateLimit-Remaining", "5")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	defer server.Close()

	cfg := config.DefaultConfig()
	cfg.Update.Repo = "houalibaba/Reader"
	u := New(cfg)
	u.apiBaseURL = server.URL
	u.githubBaseURL = server.URL
	_, err := u.CheckUpdate(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "Resource not accessible by integration") {
		t.Fatalf("expected original non-rate-limit 403, got %v", err)
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("unrelated 403 unexpectedly triggered fallback %d times", fallbackCalls.Load())
	}
}

func TestIsGitHubRateLimitResponse(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		headers http.Header
		body    string
		want    bool
	}{
		{"primary header", http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": []string{"0"}}, "forbidden", true},
		{"secondary body", http.StatusForbidden, http.Header{}, "You have exceeded a secondary rate limit", true},
		{"generic forbidden", http.StatusForbidden, http.Header{}, "Resource not accessible", false},
		{"too many requests", http.StatusTooManyRequests, http.Header{}, "rate limited", true},
		{"other status", http.StatusInternalServerError, http.Header{}, "rate limit", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGitHubRateLimitResponse(tc.status, tc.headers, []byte(tc.body)); got != tc.want {
				t.Fatalf("isGitHubRateLimitResponse() = %v, want %v", got, tc.want)
			}
		})
	}
}

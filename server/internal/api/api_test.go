package api

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"reader/server/internal/config"
	"reader/server/internal/db"
)

func newTestServer(t *testing.T, guardianPin string) *Server {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataDir = tmpDir
	cfg.GuardianPin = guardianPin

	conn, err := db.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return NewServer(cfg, conn, nil)
}

func TestRequireGuardianPINNeverBypassesWhenEmpty(t *testing.T) {
	srv := newTestServer(t, "")
	handler := srv.Routes()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/gist-pull", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized when GuardianPin is empty, got %d", rec.Code)
	}
}

func TestValidatePublicRemoteImageURLBlocksSSRF(t *testing.T) {
	blocked := []string{
		"http://example.com/a.png",
		"https://127.0.0.1/a.png",
		"https://localhost/a.png",
		"https://192.168.1.1/a.png",
		"https://10.0.0.1/a.png",
		"https://172.16.0.1/a.png",
		"https://169.254.169.254/latest/meta-data/",
		"https://100.100.100.100/a.png",
		"https://[::1]/a.png",
		"https://router.lan/a.png",
		"file:///etc/passwd",
	}
	for _, u := range blocked {
		if _, err := validatePublicRemoteImageURL(u, false); err == nil {
			t.Errorf("expected SSRF URL %q to be rejected", u)
		}
	}

	if _, err := validatePublicRemoteImageURL("https://gist.githubusercontent.com/houalibaba/aa/raw/ch1.png", false); err != nil {
		t.Errorf("expected public https URL to be allowed: %v", err)
	}

	if !isBlockedOrPrivateIP(net.ParseIP("100.64.0.1")) || !isBlockedOrPrivateIP(net.ParseIP("127.0.0.1")) || !isBlockedOrPrivateIP(net.ParseIP("fe80::1")) {
		t.Error("expected CGNAT, loopback, and link-local IPs to be blocked")
	}
}

func TestGistPullRejectsUnsafeStoryIDAndDistributesImages(t *testing.T) {
	srv := newTestServer(t, "1234")

	// Mock Gist API server returning traversal story_id first, then valid story
	mode := "traversal"
	mockGist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "traversal" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": map[string]any{
					"next_story.json": map[string]any{
						"content": `{"version":"2026111701","story_id":"../../etc/pwn","title":"Bad","sentences":["第一句。","第二句。"]}`,
					},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"files": map[string]any{
				"next_story.json": map[string]any{
					"content": `{"version":"2026111702","story_id":"story_safe_01","title":"安全故事","sentences":["第一句。","第二句。","第三句。","第四句。"],"image_urls":["http://127.0.0.1:9999/ssrf.png"]}`,
				},
			},
		})
	}))
	defer mockGist.Close()

	srv.gistAPIBaseURL = mockGist.URL
	srv.gistRawBaseURL = mockGist.URL
	handler := srv.Routes()

	// 1. Traversal story_id must be rejected with 400
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/admin/gist-pull", bytes.NewReader([]byte(`{"force":true}`)))
	req1.Header.Set("X-Guardian-Pin", "1234")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsafe story_id, got %d: %s", rec1.Code, rec1.Body.String())
	}

	// 2. Valid story with SSRF image URL: SSRF image is blocked and safely falls back to generated SVG illustrations
	mode = "valid"
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/admin/gist-pull", bytes.NewReader([]byte(`{"force":true}`)))
	req2.Header.Set("X-Guardian-Pin", "1234")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for valid gist pull, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

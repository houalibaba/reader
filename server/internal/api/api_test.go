package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	cfg.MinFreeDiskMB = 1

	conn, err := db.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return NewServer(cfg, conn, nil)
}

func seedTestStory(t *testing.T, srv *Server, storyID string) {
	t.Helper()
	_, err := srv.db.Exec(
		`INSERT INTO stories (id, title, theme, description, cover_url, status) VALUES (?, ?, ?, ?, '', 'published')`,
		storyID, "测试故事", "探险", "测试描述",
	)
	if err != nil {
		t.Fatalf("seed story: %v", err)
	}
}

func buildAnimationUploadReq(t *testing.T, storyID, pin, filename string, payload []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("animation", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/stories/"+storyID+"/animation", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Guardian-Pin", pin)
	return req
}

func mediaURLToDiskPath(srv *Server, mediaURL string) string {
	rel := strings.TrimPrefix(mediaURL, "/media/")
	return filepath.Join(srv.cfg.MediaDir(), filepath.FromSlash(rel))
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
	markerPath := filepath.Join(srv.cfg.DataDir, config.MountMarkerFile)
	if err := os.WriteFile(markerPath, []byte("ok"), 0o644); err != nil {
		t.Fatalf("write mount marker: %v", err)
	}

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

	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/admin/gist-pull", bytes.NewReader([]byte(`{"force":true}`)))
	req1.Header.Set("X-Guardian-Pin", "1234")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsafe story_id, got %d: %s", rec1.Code, rec1.Body.String())
	}

	mode = "valid"
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/admin/gist-pull", bytes.NewReader([]byte(`{"force":true}`)))
	req2.Header.Set("X-Guardian-Pin", "1234")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for valid gist pull, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestGistPullRequiresMountMarkerBeforeWriting(t *testing.T) {
	srv := newTestServer(t, "1234")
	markerPath := filepath.Join(srv.cfg.DataDir, config.MountMarkerFile)
	dropDuringFetch := false

	mockGist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dropDuringFetch {
			_ = os.Remove(markerPath)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"files": map[string]any{
				"next_story.json": map[string]any{
					"content": `{"version":"2026112001","story_id":"story_gist_mount","title":"外接盘检查","sentences":["第一句。","第二句。"],"images":["data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="]}`,
				},
			},
		})
	}))
	defer mockGist.Close()

	srv.gistAPIBaseURL = mockGist.URL
	srv.gistRawBaseURL = mockGist.URL
	handler := srv.Routes()

	// 1. 入口处缺少 .reader-disk 时直接拒绝（503），不写图片也不写数据库
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/admin/gist-pull", bytes.NewReader([]byte(`{"force":true}`)))
	req1.Header.Set("X-Guardian-Pin", "1234")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when .reader-disk missing before gist pull, got %d: %s", rec1.Code, rec1.Body.String())
	}

	// 2. 拉取途中外接盘掉线（模拟 HTTP 拉取后标记消失）：写图片和入库前复查必须拒绝（503）
	if err := os.WriteFile(markerPath, []byte("ok"), 0o644); err != nil {
		t.Fatalf("write mount marker: %v", err)
	}
	dropDuringFetch = true
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/admin/gist-pull", bytes.NewReader([]byte(`{"force":true}`)))
	req2.Header.Set("X-Guardian-Pin", "1234")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when .reader-disk disappears mid-gist-pull, got %d: %s", rec2.Code, rec2.Body.String())
	}

	if _, err := os.Stat(filepath.Join(srv.cfg.MediaDir(), "stories", "story_gist_mount")); !os.IsNotExist(err) {
		t.Fatalf("expected no story media directory written when mount marker is missing")
	}
	var storyCount int
	if err := srv.db.QueryRow("SELECT COUNT(*) FROM stories WHERE id = 'story_gist_mount'").Scan(&storyCount); err != nil {
		t.Fatalf("query stories: %v", err)
	}
	if storyCount != 0 {
		t.Fatalf("expected 0 stories persisted when mount marker is missing, got %d", storyCount)
	}
}

func TestStoryAnimationUploadRequiresMountMarker(t *testing.T) {
	srv := newTestServer(t, "1234")
	srv.cfg.UploadMaxMB = 1
	seedTestStory(t, srv, "story_mount_01")
	handler := srv.Routes()

	req := buildAnimationUploadReq(t, "story_mount_01", "1234", "clip.mp4", bytes.Repeat([]byte("a"), 1024))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when .reader-disk marker missing, got %d: %s", rec.Code, rec.Body.String())
	}
	entries, _ := os.ReadDir(filepath.Join(srv.cfg.MediaDir(), "stories", "story_mount_01"))
	if len(entries) > 0 {
		t.Fatalf("expected no animation file written when mount marker is missing, found %d entries", len(entries))
	}
}

func TestStoryAnimationUploadRespectsLowLimitAndPreservesOldAnimation(t *testing.T) {
	srv := newTestServer(t, "1234")
	srv.cfg.UploadMaxMB = 1
	markerPath := filepath.Join(srv.cfg.DataDir, config.MountMarkerFile)
	if err := os.WriteFile(markerPath, []byte("ok"), 0o644); err != nil {
		t.Fatalf("write mount marker: %v", err)
	}
	seedTestStory(t, srv, "story_limit_01")
	handler := srv.Routes()

	oldPayload := []byte("GIF89a-old-animation-bytes")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, buildAnimationUploadReq(t, "story_limit_01", "1234", "old.gif", oldPayload))
	if rec1.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid small animation, got %d: %s", rec1.Code, rec1.Body.String())
	}
	var resp1 struct {
		AnimationURL string `json:"animation_url"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &resp1); err != nil || resp1.AnimationURL == "" {
		t.Fatalf("invalid upload response: %s", rec1.Body.String())
	}

	oversized := bytes.Repeat([]byte("x"), 2*1024*1024)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, buildAnimationUploadReq(t, "story_limit_01", "1234", "big.mp4", oversized))
	if rec2.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for 2MB upload under 1MB limit, got %d: %s", rec2.Code, rec2.Body.String())
	}

	oldPath := mediaURLToDiskPath(srv, resp1.AnimationURL)
	gotOld, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatalf("expected old animation to be preserved after rejected upload: %v", err)
	}
	if !bytes.Equal(gotOld, oldPayload) {
		t.Fatalf("old animation content was corrupted")
	}

	_ = os.Remove(markerPath)
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, buildAnimationUploadReq(t, "story_limit_01", "1234", "new.mp4", []byte("new-mp4")))
	if rec3.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 after mount marker disappears at runtime, got %d: %s", rec3.Code, rec3.Body.String())
	}
}

func TestStoryAnimationReplaceSameExtAndDeletePreserveFileOnDBFailure(t *testing.T) {
	srv := newTestServer(t, "1234")
	markerPath := filepath.Join(srv.cfg.DataDir, config.MountMarkerFile)
	if err := os.WriteFile(markerPath, []byte("ok"), 0o644); err != nil {
		t.Fatalf("write mount marker: %v", err)
	}
	seedTestStory(t, srv, "story_atomic_01")
	handler := srv.Routes()

	oldPayload := []byte("original-mp4-content-v1")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, buildAnimationUploadReq(t, "story_atomic_01", "1234", "v1.mp4", oldPayload))
	if rec1.Code != http.StatusOK {
		t.Fatalf("expected 200 on initial mp4 upload, got %d: %s", rec1.Code, rec1.Body.String())
	}
	var resp1 struct {
		AnimationURL string `json:"animation_url"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &resp1); err != nil || resp1.AnimationURL == "" {
		t.Fatalf("invalid initial response: %s", rec1.Body.String())
	}
	oldPath := mediaURLToDiskPath(srv, resp1.AnimationURL)
	storyDir := filepath.Dir(oldPath)

	_, err := srv.db.Exec(`
		CREATE TRIGGER fail_animation_url_update
		BEFORE UPDATE OF animation_url ON stories
		BEGIN
			SELECT RAISE(ABORT, 'simulated db failure');
		END;
	`)
	if err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	newPayload := []byte("replacement-mp4-content-v2")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, buildAnimationUploadReq(t, "story_atomic_01", "1234", "v2.mp4", newPayload))
	if rec2.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when DB update fails on replace, got %d: %s", rec2.Code, rec2.Body.String())
	}
	gotOld, err := os.ReadFile(oldPath)
	if err != nil || !bytes.Equal(gotOld, oldPayload) {
		t.Fatalf("old mp4 was overwritten or lost on DB failure: err=%v got=%q", err, string(gotOld))
	}
	entries, _ := os.ReadDir(storyDir)
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 animation file after rollback, got %d", len(entries))
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/stories/story_atomic_01/animation", nil)
	delReq.Header.Set("X-Guardian-Pin", "1234")
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, delReq)
	if rec3.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when DB update fails on delete, got %d: %s", rec3.Code, rec3.Body.String())
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("animation file was deleted despite DB update failure: %v", err)
	}

	if _, err := srv.db.Exec(`DROP TRIGGER fail_animation_url_update;`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	delReq2 := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/stories/story_atomic_01/animation", nil)
	delReq2.Header.Set("X-Guardian-Pin", "1234")
	rec4 := httptest.NewRecorder()
	handler.ServeHTTP(rec4, delReq2)
	if rec4.Code != http.StatusOK {
		t.Fatalf("expected 200 on delete after trigger removed, got %d: %s", rec4.Code, rec4.Body.String())
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expected animation file to be removed after successful DB update")
	}
}

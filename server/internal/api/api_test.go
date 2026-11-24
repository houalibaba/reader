package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestForgettingMetricsUsesLatestSeenTimeAndGraduates(t *testing.T) {
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	tenDaysAgo := now.Add(-240 * time.Hour).Format(time.RFC3339)
	oneHourAgo := now.Add(-1 * time.Hour).Format(time.RFC3339)

	// 历史求助 4 次，但后续独立无求助读对 3 次且 1 小时前刚读过 -> 应毕业，保留率高，不再强制锁定 needs_support
	ret, retPct, _, _, activeNeeds := computeForgettingMetrics(4, 1, 4, 3, tenDaysAgo, oneHourAgo, now)
	if activeNeeds {
		t.Fatalf("expected graduated character to leave activeNeedsSupport, got ret=%.2f retPct=%d", ret, retPct)
	}
	if ret < 0.90 {
		t.Fatalf("expected graduated character recently seen to have high retention >= 0.90, got %.2f", ret)
	}

	// 同一字若 7 天未再阅读 -> 应按艾宾浩斯遗忘曲线自然回落到 0.78 以下重新进入复习队列
	sevenDaysAgo := now.Add(-168 * time.Hour).Format(time.RFC3339)
	retLater, _, _, _, _ := computeForgettingMetrics(4, 1, 4, 3, tenDaysAgo, sevenDaysAgo, now)
	if retLater >= 0.78 {
		t.Fatalf("expected graduated character after 7 days to drop below 0.78 on forgetting curve, got %.2f", retLater)
	}
}

func TestStoryPlanGraduatesOldHelpAndSurfacesForgettingCurve(t *testing.T) {
	srv := newTestServer(t, "1234")
	ctx := context.Background()
	now := time.Now().UTC()
	ts := func(hoursAgo float64) string {
		return now.Add(-time.Duration(hoursAgo * float64(time.Hour))).Format(time.RFC3339)
	}

	seedTestStory(t, srv, "story_fc_01")
	_, err := srv.db.Exec(`INSERT INTO story_versions (id, story_id, version_number, text_content, checksum, status) VALUES ('ver_fc_01', 'story_fc_01', 1, '旧新忘', 'chk_fc', 'published')`)
	if err != nil {
		t.Fatalf("seed version: %v", err)
	}
	for idx, row := range []struct{ sid, tid, ch, py string }{
		{"sent_old", "tok_old", "旧", "jiù"},
		{"sent_new", "tok_new", "新", "xīn"},
		{"sent_fgt", "tok_fgt", "忘", "wàng"},
	} {
		if _, err := srv.db.Exec(`INSERT INTO story_sentences (id, version_id, sequence_index, text) VALUES (?, 'ver_fc_01', ?, ?)`, row.sid, idx, row.ch); err != nil {
			t.Fatalf("seed sentence %s: %v", row.sid, err)
		}
		if _, err := srv.db.Exec(`INSERT INTO sentence_tokens (id, sentence_id, token_index, character, pinyin, word, word_index) VALUES (?, ?, 0, ?, ?, ?, 0)`, row.tid, row.sid, row.ch, row.py, row.ch); err != nil {
			t.Fatalf("seed token %s: %v", row.tid, err)
		}
	}
	if _, err := srv.db.Exec(`INSERT INTO reading_sessions (id, child_id, story_version_id, mode, status, pinyin_hint_enabled) VALUES ('sess_fc', 'child_default_liuliu', 'ver_fc_01', 'solo', 'completed', 0)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// 1. 「旧」：10 天前求助 4 次，近两天独立无求助读对 3 次 -> 毕业进入 familiar_anchors
	_, _ = srv.db.Exec(`INSERT INTO sentence_views (id, session_id, sentence_id, entered_at, dwell_time_ms, is_valid) VALUES ('sv_old_h', 'sess_fc', 'sent_old', ?, 5000, 1)`, ts(240))
	for i := 0; i < 4; i++ {
		_, _ = srv.db.Exec(`INSERT INTO character_events (id, sentence_view_id, token_id, character, pinyin_played, clicked_at, is_valid) VALUES (?, 'sv_old_h', 'tok_old', '旧', 'jiù', ?, 1)`, "ce_old_"+string(rune('a'+i)), ts(240-0.01*float64(i+1)))
	}
	for i, h := range []float64{48, 24, 1} {
		_, _ = srv.db.Exec(`INSERT INTO sentence_views (id, session_id, sentence_id, entered_at, dwell_time_ms, is_valid) VALUES (?, 'sess_fc', 'sent_old', ?, 5000, 1)`, "sv_old_ok_"+string(rune('a'+i)), ts(h))
	}

	// 2. 「新」：2 小时前求助 3 次 -> 应排在 review_targets 第一位
	_, _ = srv.db.Exec(`INSERT INTO sentence_views (id, session_id, sentence_id, entered_at, dwell_time_ms, is_valid) VALUES ('sv_new_h', 'sess_fc', 'sent_new', ?, 5000, 1)`, ts(2))
	for i := 0; i < 3; i++ {
		_, _ = srv.db.Exec(`INSERT INTO character_events (id, sentence_view_id, token_id, character, pinyin_played, clicked_at, is_valid) VALUES (?, 'sv_new_h', 'tok_new', '新', 'xīn', ?, 1)`, "ce_new_"+string(rune('a'+i)), ts(2-0.01*float64(i+1)))
	}

	// 3. 「忘」：6 天前独立读过 3 次（0 次求助）-> 按艾宾浩斯遗忘曲线跌破 78% 应自动进入 review_targets
	for i, h := range []float64{152, 151, 150} {
		_, _ = srv.db.Exec(`INSERT INTO sentence_views (id, session_id, sentence_id, entered_at, dwell_time_ms, is_valid) VALUES (?, 'sess_fc', 'sent_fgt', ?, 5000, 1)`, "sv_fgt_"+string(rune('a'+i)), ts(h))
	}

	plan, err := srv.buildChildStoryPlan(ctx, "child_default_liuliu")
	if err != nil {
		t.Fatalf("buildChildStoryPlan: %v", err)
	}

	hasReview := func(ch string) bool {
		for _, item := range plan.ReviewTargets {
			if item.Character == ch {
				return true
			}
		}
		return false
	}
	hasFamiliar := func(ch string) bool {
		for _, item := range plan.FamiliarAnchors {
			if item.Character == ch {
				return true
			}
		}
		return false
	}

	if hasReview("旧") || !hasFamiliar("旧") {
		t.Fatalf("expected 「旧」 to graduate to familiar_anchors; review=%v familiar=%v", plan.ReviewTargets, plan.FamiliarAnchors)
	}
	if len(plan.ReviewTargets) == 0 || plan.ReviewTargets[0].Character != "新" {
		t.Fatalf("expected 「新」 to be top review target, got %+v", plan.ReviewTargets)
	}
	if !hasReview("忘") {
		t.Fatalf("expected 「忘」 to re-enter review_targets via Ebbinghaus forgetting curve, got %+v", plan.ReviewTargets)
	}
}

func TestNewCandidatesUsesExpandedPoolAndSkipsUnreadStory(t *testing.T) {
	srv := newTestServer(t, "1234")
	ctx := context.Background()

	// 写入一篇已发布但孩子尚未读完的故事，包含原 30 字池最前面的「水」「火」「山」
	seedTestStory(t, srv, "story_unread_01")
	_, err := srv.db.Exec(`INSERT INTO story_versions (id, story_id, version_number, text_content, checksum, status) VALUES ('ver_unread_01', 'story_unread_01', 1, '水火山', 'chk_u1', 'published')`)
	if err != nil {
		t.Fatalf("seed version: %v", err)
	}
	_, _ = srv.db.Exec(`INSERT INTO story_sentences (id, version_id, sequence_index, text) VALUES ('sent_u1', 'ver_unread_01', 0, '水火山')`)
	for i, ch := range []string{"水", "火", "山"} {
		_, _ = srv.db.Exec(`INSERT INTO sentence_tokens (id, sentence_id, token_index, character, pinyin, word, word_index) VALUES (?, 'sent_u1', ?, ?, 'x', ?, 0)`, "tok_u_"+string(rune('a'+i)), i, ch, ch)
	}

	if len(gradedStarterPool) < 300 {
		t.Fatalf("expected expanded gradedStarterPool >= 300 chars, got %d", len(gradedStarterPool))
	}

	plan, err := srv.buildChildStoryPlan(ctx, "child_default_liuliu")
	if err != nil {
		t.Fatalf("buildChildStoryPlan: %v", err)
	}
	if len(plan.NewCandidates) == 0 {
		t.Fatalf("expected non-empty new_candidates")
	}
	for _, cand := range plan.NewCandidates {
		if cand.Character == "水" || cand.Character == "火" || cand.Character == "山" {
			t.Fatalf("expected unread story char %q to be skipped in new_candidates, got %+v", cand.Character, plan.NewCandidates)
		}
	}
}

func TestAllocateTargetBudgetStrictlyCapsTotal(t *testing.T) {
	makeTargets := func(n int) []planTarget {
		out := make([]planTarget, n)
		for i := 0; i < n; i++ {
			out[i] = planTarget{Character: string(rune('一' + i)), Urgency: float64(n - i)}
		}
		return out
	}

	// 边界样例：7 复习 + 4 观察，总预算 10 -> 必须严格封顶 10（7 复习 + 3 观察 + 0 新字）
	rev, obs, newQ := allocateTargetBudget(makeTargets(7), makeTargets(4), 10, 7, 4, 6)
	if len(rev) != 7 || len(obs) != 3 || newQ != 0 {
		t.Fatalf("expected 7+3+0=10, got %d+%d+%d", len(rev), len(obs), newQ)
	}
	if len(rev)+len(obs)+newQ > 10 {
		t.Fatalf("total budget exceeded 10: %d", len(rev)+len(obs)+newQ)
	}

	// 复习 10 + 观察 4，总预算 10 -> 复习封顶 7，观察 3，新字 0
	rev2, obs2, newQ2 := allocateTargetBudget(makeTargets(10), makeTargets(4), 10, 7, 4, 6)
	if len(rev2) != 7 || len(obs2) != 3 || newQ2 != 0 {
		t.Fatalf("expected 7+3+0=10 when both full, got %d+%d+%d", len(rev2), len(obs2), newQ2)
	}

	// 复习 2 + 观察 1，总预算 10 -> 新字受 maxNew=6 限制，总计 9 <= 10
	rev3, obs3, newQ3 := allocateTargetBudget(makeTargets(2), makeTargets(1), 10, 7, 4, 6)
	if len(rev3) != 2 || len(obs3) != 1 || newQ3 != 6 {
		t.Fatalf("expected 2+1+6=9, got %d+%d+%d", len(rev3), len(obs3), newQ3)
	}
}

func TestNewCandidatesExcludesScaffoldChars(t *testing.T) {
	srv := newTestServer(t, "1234")
	plan, err := srv.buildChildStoryPlan(context.Background(), "child_default_liuliu")
	if err != nil {
		t.Fatalf("buildChildStoryPlan: %v", err)
	}
	scaffold := buildScaffoldCharSet(foundationalScaffoldWords)
	for _, cand := range plan.NewCandidates {
		if scaffold[cand.Character] {
			t.Fatalf("expected scaffold char %q to be excluded from new_candidates", cand.Character)
		}
	}
}

func TestAutoPublishBlockedWhenDraftFailsValidation(t *testing.T) {
	srv := newTestServer(t, "1234")
	handler := srv.Routes()

	// 未配置 LLM 时降级为 5 句短模板，即便传入 auto_publish=true 也绝不允许自动发布
	body := `{"child_id":"child_default_liuliu","theme":"勇气与探索（森林过河）","auto_publish":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/auto-generate-story", strings.NewReader(body))
	req.Header.Set("X-Guardian-Pin", "1234")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Published          bool   `json:"published"`
		AutoPublishBlocked bool   `json:"auto_publish_blocked"`
		Generator          string `json:"generator"`
		Validation         *struct {
			Passed bool `json:"passed"`
		} `json:"validation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Published {
		t.Fatalf("expected short template draft NOT to be auto-published")
	}
	if !resp.AutoPublishBlocked {
		t.Fatalf("expected auto_publish_blocked=true")
	}
	if resp.Validation == nil || resp.Validation.Passed {
		t.Fatalf("expected validation.passed=false for short template")
	}
	var storyCount int
	_ = srv.db.QueryRow("SELECT COUNT(*) FROM stories").Scan(&storyCount)
	if storyCount != 0 {
		t.Fatalf("expected 0 published stories in DB, got %d", storyCount)
	}
}

func TestGistPromptSyncRetriesRecordsStatusAndRedactsChildName(t *testing.T) {
	srv := newTestServer(t, "1234")
	srv.gistSyncBackoff = time.Millisecond

	seedTestStory(t, srv, "story_gist_sync")
	_, _ = srv.db.Exec(`INSERT INTO story_versions (id, story_id, version_number, text_content, checksum, status) VALUES ('ver_gs', 'story_gist_sync', 1, '测试', 'chk_gs', 'published')`)
	_, _ = srv.db.Exec(`INSERT INTO reading_sessions (id, child_id, story_version_id, mode, status, pinyin_hint_enabled) VALUES ('sess_gs', 'child_default_liuliu', 'ver_gs', 'solo', 'completed', 0)`)
	nowStr := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < 7; i++ {
		sid := "sent_gs_" + string(rune('a'+i))
		svID := "sv_gs_" + string(rune('a'+i))
		_, _ = srv.db.Exec(`INSERT INTO story_sentences (id, version_id, sequence_index, text) VALUES (?, 'ver_gs', ?, '测试句子')`, sid, i)
		_, _ = srv.db.Exec(`INSERT INTO sentence_views (id, session_id, sentence_id, entered_at, dwell_time_ms, is_valid) VALUES (?, 'sess_gs', ?, ?, 5000, 1)`, svID, sid, nowStr)
	}

	attempts := 0
	var lastPayload string
	mockGist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		var reqMap map[string]any
		_ = json.NewDecoder(r.Body).Decode(&reqMap)
		if files, ok := reqMap["files"].(map[string]any); ok {
			if np, ok := files["next_prompt.md"].(map[string]any); ok {
				if c, ok := np["content"].(string); ok {
					lastPayload = c
				}
			}
		}
		if attempts == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("temporary error"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"aa8591c33578eb903d881cd133ec8fc3"}`))
	}))
	defer mockGist.Close()

	srv.gistAPIBaseURL = mockGist.URL
	t.Setenv("GITHUB_GIST_TOKEN", "test_token_only")

	srv.syncSessionPromptToGist("sess_gs", "completed", "finished")
	if attempts != 2 {
		t.Fatalf("expected 2 attempts (1 retry after 500), got %d", attempts)
	}
	if !srv.lastGistSyncStatus.Success || srv.lastGistSyncStatus.HTTPStatus != http.StatusOK {
		t.Fatalf("expected recorded success status, got %+v", srv.lastGistSyncStatus)
	}
	if strings.Contains(lastPayload, "child_name:") || strings.Contains(lastPayload, "session_id:") {
		t.Fatalf("expected child_name and session_id to be redacted from Gist metadata, got:\n%s", lastPayload)
	}
	firstVer := srv.lastGistSyncVersion
	srv.syncSessionPromptToGist("sess_gs", "completed", "finished")
	if srv.lastGistSyncVersion <= firstVer {
		t.Fatalf("expected strictly monotonic version on consecutive syncs: %d -> %d", firstVer, srv.lastGistSyncVersion)
	}
}

func TestGistPromptSyncSkipsAccompaniedAndShortEffectiveSessions(t *testing.T) {
	srv := newTestServer(t, "1234")
	srv.gistSyncBackoff = time.Millisecond

	seedTestStory(t, srv, "story_gist_filter")
	_, err := srv.db.Exec(`INSERT INTO story_versions (id, story_id, version_number, text_content, checksum, status) VALUES ('ver_gf', 'story_gist_filter', 1, '测试', 'chk_gf', 'published')`)
	if err != nil {
		t.Fatalf("seed version: %v", err)
	}
	// 种 9 句，每句 4 个有效汉字 -> 基准时长 4000ms，有效阅读净停留门槛为 2000ms
	for i := 0; i < 9; i++ {
		sid := "sent_gf_" + string(rune('a'+i))
		if _, err := srv.db.Exec(`INSERT INTO story_sentences (id, version_id, sequence_index, text) VALUES (?, 'ver_gf', ?, '测试句子')`, sid, i); err != nil {
			t.Fatalf("seed sentence %s: %v", sid, err)
		}
	}

	nowStr := time.Now().UTC().Format(time.RFC3339)
	seedSession := func(sessID, mode string, validDistinctPages int) {
		t.Helper()
		if _, err := srv.db.Exec(
			`INSERT INTO reading_sessions (id, child_id, story_version_id, mode, status, pinyin_hint_enabled) VALUES (?, 'child_default_liuliu', 'ver_gf', ?, 'completed', 0)`,
			sessID, mode,
		); err != nil {
			t.Fatalf("seed session %s: %v", sessID, err)
		}
		for i := 0; i < validDistinctPages; i++ {
			svID := "sv_" + sessID + "_" + string(rune('a'+i))
			sentID := "sent_gf_" + string(rune('a'+i))
			if _, err := srv.db.Exec(
				`INSERT INTO sentence_views (id, session_id, sentence_id, entered_at, dwell_time_ms, is_valid, invalid_reason) VALUES (?, ?, ?, ?, 5000, 1, '')`,
				svID, sessID, sentID, nowStr,
			); err != nil {
				t.Fatalf("seed view %s: %v", svID, err)
			}
		}
	}

	// 1. 家长陪读：即便有 7 页有效阅读，也必须跳过
	seedSession("sess_gf_acc", "accompanied", 7)

	// 2. 自主阅读：仅 6 页有效阅读，再额外塞入 4 种“不能算数”的浏览记录，仍必须跳过
	seedSession("sess_gf_short", "solo", 6)
	// 2a. 重复翻看第 1 页（不能靠刷同一页凑满 7 页）
	_, _ = srv.db.Exec(`INSERT INTO sentence_views (id, session_id, sentence_id, entered_at, dwell_time_ms, is_valid, invalid_reason) VALUES ('sv_short_dup', 'sess_gf_short', 'sent_gf_a', ?, 5000, 1, '')`, nowStr)
	// 2b. 第 7 页停留仅 1500ms（虽 >=300ms 故 is_valid=1，但低于基准一半 2000ms -> 有效阅读时长为 0）
	_, _ = srv.db.Exec(`INSERT INTO sentence_views (id, session_id, sentence_id, entered_at, dwell_time_ms, is_valid, invalid_reason) VALUES ('sv_short_too_fast', 'sess_gf_short', 'sent_gf_g', ?, 1500, 1, '')`, nowStr)
	// 2c. 第 8 页停留 5000ms，但整句朗读播了 4000ms（净停留 1000ms < 2000ms -> 有效阅读时长为 0）
	_, _ = srv.db.Exec(`INSERT INTO sentence_views (id, session_id, sentence_id, entered_at, dwell_time_ms, is_valid, invalid_reason) VALUES ('sv_short_audio_only', 'sess_gf_short', 'sent_gf_h', ?, 5000, 1, '')`, nowStr)
	_, _ = srv.db.Exec(`INSERT INTO audio_events (id, sentence_view_id, audio_type, played_at, play_duration_ms, completed) VALUES ('ae_short_h', 'sv_short_audio_only', 'sentence', ?, 4000, 1)`, nowStr)
	// 2d. 第 9 页挂机超时（is_valid=0 -> 无效）
	_, _ = srv.db.Exec(`INSERT INTO sentence_views (id, session_id, sentence_id, entered_at, dwell_time_ms, is_valid, invalid_reason) VALUES ('sv_short_idle', 'sess_gf_short', 'sent_gf_i', ?, 200000, 0, 'abnormal_long_dwell')`, nowStr)

	// 3. 自主阅读：满 7 页去重有效阅读，必须正常推送
	seedSession("sess_gf_ok", "solo", 7)

	attempts := 0
	mockGist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"aa8591c33578eb903d881cd133ec8fc3"}`))
	}))
	defer mockGist.Close()

	srv.gistAPIBaseURL = mockGist.URL
	t.Setenv("GITHUB_GIST_TOKEN", "test_token_filter")

	srv.syncSessionPromptToGist("sess_gf_acc", "completed", "finished")
	if attempts != 0 {
		t.Fatalf("expected accompanied session to skip gist sync, got %d requests", attempts)
	}
	if srv.lastGistSyncStatus.Error != "" {
		t.Fatalf("expected accompanied skip not to pollute lastGistSyncStatus.Error, got %+v", srv.lastGistSyncStatus)
	}

	srv.syncSessionPromptToGist("sess_gf_short", "active", "user_exit")
	if attempts != 0 {
		t.Fatalf("expected session with only 6 effective pages to skip gist sync, got %d requests", attempts)
	}
	if srv.lastGistSyncStatus.Error != "" {
		t.Fatalf("expected short-session skip not to pollute lastGistSyncStatus.Error, got %+v", srv.lastGistSyncStatus)
	}

	srv.syncSessionPromptToGist("sess_gf_ok", "active", "user_exit")
	if attempts != 1 {
		t.Fatalf("expected solo session with 7 effective pages to sync once, got %d requests", attempts)
	}
	if !srv.lastGistSyncStatus.Success {
		t.Fatalf("expected successful gist sync status for 7 effective pages, got %+v", srv.lastGistSyncStatus)
	}
}

func TestVocabularyEndpointReturnsLastSeenAndPostHelpUnhelped(t *testing.T) {
	srv := newTestServer(t, "1234")
	handler := srv.Routes()
	now := time.Now().UTC()
	ts := func(hoursAgo float64) string {
		return now.Add(-time.Duration(hoursAgo * float64(time.Hour))).Format(time.RFC3339)
	}

	seedTestStory(t, srv, "story_v_align")
	_, _ = srv.db.Exec(`INSERT INTO story_versions (id, story_id, version_number, text_content, checksum, status) VALUES ('ver_va', 'story_v_align', 1, '河', 'chk_va', 'published')`)
	_, _ = srv.db.Exec(`INSERT INTO story_sentences (id, version_id, sequence_index, text) VALUES ('sent_va', 'ver_va', 0, '河')`)
	_, _ = srv.db.Exec(`INSERT INTO sentence_tokens (id, sentence_id, token_index, character, pinyin, word, word_index) VALUES ('tok_va', 'sent_va', 0, '河', 'hé', '小河', 0)`)
	_, _ = srv.db.Exec(`INSERT INTO reading_sessions (id, child_id, story_version_id, mode, status, pinyin_hint_enabled) VALUES ('sess_va', 'child_default_liuliu', 'ver_va', 'solo', 'completed', 0)`)

	_, _ = srv.db.Exec(`INSERT INTO sentence_views (id, session_id, sentence_id, entered_at, dwell_time_ms, is_valid) VALUES ('sv_va_0', 'sess_va', 'sent_va', ?, 5000, 1)`, ts(72))
	for i := 0; i < 3; i++ {
		_, _ = srv.db.Exec(`INSERT INTO character_events (id, sentence_view_id, token_id, character, pinyin_played, clicked_at, is_valid) VALUES (?, 'sv_va_0', 'tok_va', '河', 'hé', ?, 1)`, "ce_va_"+string(rune('a'+i)), ts(72-0.01*float64(i+1)))
	}
	for i, h := range []float64{48, 24, 1} {
		_, _ = srv.db.Exec(`INSERT INTO sentence_views (id, session_id, sentence_id, entered_at, dwell_time_ms, is_valid) VALUES (?, 'sess_va', 'sent_va', ?, 5000, 1)`, "sv_va_ok_"+string(rune('a'+i)), ts(h))
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/children/child_default_liuliu/vocabulary", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Vocabulary []childVocabStat `json:"vocabulary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Vocabulary) != 1 {
		t.Fatalf("unexpected vocabulary response: %s", rec.Body.String())
	}
	got := resp.Vocabulary[0]
	if got.LastSeenAt == "" || got.PostHelpUnhelped != 3 || got.EstimatedStatus != "likely_familiar" {
		t.Fatalf("expected last_seen_at non-empty, post_help_unhelped=3, graduated likely_familiar; got %+v", got)
	}
}

func TestAdminGistPushPromptReturnsLatestPromptAndSyncsGist(t *testing.T) {
	srv := newTestServer(t, "1234")
	srv.gistSyncBackoff = time.Millisecond
	handler := srv.Routes()

	// 未带 PIN 必须拒绝
	reqUnauth := httptest.NewRequest(http.MethodPost, "/api/v1/admin/gist-push-prompt", strings.NewReader(`{"child_id":"child_default_liuliu"}`))
	recUnauth := httptest.NewRecorder()
	handler.ServeHTTP(recUnauth, reqUnauth)
	if recUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without PIN, got %d", recUnauth.Code)
	}

	var pushedContent string
	mockGist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if files, ok := body["files"].(map[string]any); ok {
			if np, ok := files["next_prompt.md"].(map[string]any); ok {
				if c, ok := np["content"].(string); ok {
					pushedContent = c
				}
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"aa8591c33578eb903d881cd133ec8fc3"}`))
	}))
	defer mockGist.Close()

	srv.gistAPIBaseURL = mockGist.URL
	t.Setenv("GITHUB_GIST_TOKEN", "test_token_manual_push")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/gist-push-prompt", strings.NewReader(`{"child_id":"child_default_liuliu"}`))
	req.Header.Set("X-Guardian-Pin", "1234")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		StoryGenerationPrompt string         `json:"story_generation_prompt"`
		GistSync              gistSyncStatus `json:"gist_sync"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if strings.TrimSpace(resp.StoryGenerationPrompt) == "" {
		t.Fatalf("expected non-empty story_generation_prompt")
	}
	if !resp.GistSync.Success || resp.GistSync.Trigger != "manual_push" {
		t.Fatalf("expected gist_sync success with trigger=manual_push, got %+v", resp.GistSync)
	}
	if !strings.Contains(pushedContent, "trigger: manual_push") || strings.Contains(pushedContent, "child_name:") {
		t.Fatalf("unexpected pushed Gist content metadata: %s", pushedContent)
	}
}

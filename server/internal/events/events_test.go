package events

import (
	"context"
	"path/filepath"
	"testing"

	"reader/server/internal/content"
	"reader/server/internal/db"
)

func TestComputeValidDwellAndEvents(t *testing.T) {
	// Scene 3: 5 minutes in background (300,000 ms) out of 305,000 ms total elapsed -> 5,000 ms valid dwell
	dwell, valid, reason := ComputeValidDwell(305000, []int64{300000})
	if dwell != 5000 || valid != 1 || reason != "" {
		t.Fatalf("expected 5000ms valid dwell, got dwell=%d valid=%d reason=%s", dwell, valid, reason)
	}

	// Fast page-flip (< 300ms) -> invalid
	dwellFast, validFast, reasonFast := ComputeValidDwell(120, nil)
	if dwellFast != 120 || validFast != 0 || reasonFast != "fast_flip" {
		t.Fatalf("expected fast_flip invalid, got dwell=%d valid=%d reason=%s", dwellFast, validFast, reasonFast)
	}
}

func TestBatchIdempotencyAndVocabularyView(t *testing.T) {
	tmpDir := t.TempDir()
	conn, err := db.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ctx := context.Background()
	_, err = conn.ExecContext(ctx, `INSERT INTO children (id, name, avatar) VALUES ('c1', '乐乐', '🐻')`)
	if err != nil {
		t.Fatalf("insert child: %v", err)
	}

	imp := content.NewImporter(conn, filepath.Join(tmpDir, "media"), filepath.Join(tmpDir, "tmp"))
	manifest := &content.StoryManifest{
		StoryID:       "s1",
		Title:         "小熊过河",
		VersionID:     "v1",
		VersionNumber: 1,
		Sentences: []content.SentenceManifest{
			{
				ID:            "v1_s00",
				SequenceIndex: 0,
				Text:          "小熊来到一条小河边。",
				Tokens: []content.TokenManifest{
					{ID: "v1_s00_t06", TokenIndex: 6, Character: "小", Pinyin: "xiǎo", Word: "小河", WordIndex: 3},
					{ID: "v1_s00_t07", TokenIndex: 7, Character: "河", Pinyin: "hé", Word: "小河", WordIndex: 3},
				},
			},
		},
	}
	if err := imp.SaveManifest(ctx, manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	_, err = conn.ExecContext(ctx, `
		INSERT INTO reading_sessions (id, child_id, story_version_id, mode, status, current_sentence_index)
		VALUES ('sess1', 'c1', 'v1', 'solo', 'active', 0)
	`)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}

	svc := NewService(conn)
	idx := 2
	payload := BatchPayload{
		CurrentSentenceIndex: &idx,
		SentenceViews: []SentenceViewInput{
			{
				ID:           "sv1",
				SentenceID:   "v1_s00",
				EnteredAt:    "2026-11-12T08:00:00Z",
				TotalElapsed: 305000,
			},
		},
		VisibilityEvents: []VisibilityEventInput{
			{
				ID:                   "ve1",
				SentenceViewID:       "sv1",
				State:                "visible",
				OccurredAt:           "2026-11-12T08:05:00Z",
				BackgroundDurationMs: 300000,
			},
		},
		CharacterEvents: []CharacterEventInput{
			{
				ID:             "ce1",
				SentenceViewID: "sv1",
				TokenID:        "v1_s00_t07",
				Character:      "河",
				PinyinPlayed:   "hé",
				ClickedAt:      "2026-11-12T08:00:02Z",
			},
		},
	}

	// First ingestion
	if _, err := svc.IngestBatch(ctx, "sess1", payload); err != nil {
		t.Fatalf("ingest batch 1: %v", err)
	}
	// Duplicate retry ingestion (must be idempotent)
	if _, err := svc.IngestBatch(ctx, "sess1", payload); err != nil {
		t.Fatalf("ingest batch 2 (retry): %v", err)
	}

	var count int
	_ = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM character_events WHERE id = 'ce1'").Scan(&count)
	if count != 1 {
		t.Fatalf("expected idempotent character_events count=1, got %d", count)
	}

	var dwell int64
	_ = conn.QueryRowContext(ctx, "SELECT dwell_time_ms FROM sentence_views WHERE id = 'sv1'").Scan(&dwell)
	if dwell != 5000 {
		t.Fatalf("expected dwell_time_ms=5000 after subtracting 5m background, got %d", dwell)
	}
}

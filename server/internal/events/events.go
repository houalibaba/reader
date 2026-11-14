package events

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	MinValidDwellMs = 300    // Fast page-flip threshold (< 300ms marked abnormal)
	MaxValidDwellMs = 180000 // Abnormal long stay (> 3min without activity capped/flagged)
)

type SentenceViewInput struct {
	ID            string  `json:"id"`
	SentenceID    string  `json:"sentence_id"`
	EnteredAt     string  `json:"entered_at"`
	ExitedAt      *string `json:"exited_at,omitempty"`
	TotalElapsed  int64   `json:"total_elapsed_ms"`
	IsRevisit     bool    `json:"is_revisit"`
	ClientVersion string  `json:"client_version,omitempty"`
}

type CharacterEventInput struct {
	ID             string `json:"id"`
	SentenceViewID string `json:"sentence_view_id"`
	TokenID        string `json:"token_id"`
	Character      string `json:"character"`
	PinyinPlayed   string `json:"pinyin_played"`
	ClickedAt      string `json:"clicked_at"`
}

type AudioEventInput struct {
	ID             string `json:"id"`
	SentenceViewID string `json:"sentence_view_id"`
	AudioType      string `json:"audio_type"`
	PlayedAt       string `json:"played_at"`
	PlayDurationMs int64  `json:"play_duration_ms"`
	Completed      bool   `json:"completed"`
}

type VisibilityEventInput struct {
	ID                   string `json:"id"`
	SentenceViewID       string `json:"sentence_view_id"`
	State                string `json:"state"` // hidden / visible
	OccurredAt           string `json:"occurred_at"`
	BackgroundDurationMs int64  `json:"background_duration_ms"`
}

type BatchPayload struct {
	CurrentSentenceIndex *int                   `json:"current_sentence_index,omitempty"`
	SessionStatus        string                 `json:"session_status,omitempty"`
	EndReason            string                 `json:"end_reason,omitempty"`
	SentenceViews        []SentenceViewInput    `json:"sentence_views"`
	CharacterEvents      []CharacterEventInput  `json:"character_events"`
	AudioEvents          []AudioEventInput      `json:"audio_events"`
	VisibilityEvents     []VisibilityEventInput `json:"visibility_events"`
}

type BatchResult struct {
	ProcessedViews      int `json:"processed_views"`
	ProcessedCharEvents int `json:"processed_char_events"`
	ProcessedAudio      int `json:"processed_audio"`
	ProcessedVisibility int `json:"processed_visibility"`
}

// ComputeValidDwell subtracts background visibility intervals from total elapsed time
// and determines validity flags (excluding fast page-flips or extreme idle states).
func ComputeValidDwell(totalElapsedMs int64, bgDurationsMs []int64) (dwellMs int64, isValid int, reason string) {
	var totalBg int64
	for _, d := range bgDurationsMs {
		if d > 0 {
			totalBg += d
		}
	}
	dwellMs = totalElapsedMs - totalBg
	if dwellMs < 0 {
		dwellMs = 0
	}
	if dwellMs < MinValidDwellMs {
		return dwellMs, 0, "fast_flip"
	}
	if dwellMs > MaxValidDwellMs {
		return dwellMs, 0, "abnormal_long_dwell"
	}
	return dwellMs, 1, ""
}

type Service struct {
	db *sql.DB
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

func (s *Service) IngestBatch(ctx context.Context, sessionID string, payload BatchPayload) (*BatchResult, error) {
	if sessionID == "" {
		return nil, errors.New("session_id is required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Verify session exists
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM reading_sessions WHERE id = ?", sessionID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("reading session %s not found", sessionID)
		}
		return nil, fmt.Errorf("query session: %w", err)
	}

	res := &BatchResult{}

	// 1. Upsert sentence_views initially so foreign keys for visibility_events/character_events/audio_events succeed
	for _, sv := range payload.SentenceViews {
		if sv.ID == "" || sv.SentenceID == "" {
			continue
		}
		revisitInt := 0
		if sv.IsRevisit {
			revisitInt = 1
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO sentence_views (
				id, session_id, sentence_id, entered_at, exited_at,
				dwell_time_ms, is_revisit, is_valid, invalid_reason, client_version
			) VALUES (?, ?, ?, ?, ?, 0, ?, 1, '', ?)
			ON CONFLICT(id) DO UPDATE SET
				exited_at = COALESCE(excluded.exited_at, sentence_views.exited_at)
		`, sv.ID, sessionID, sv.SentenceID, sv.EnteredAt, sv.ExitedAt, revisitInt, sv.ClientVersion)
		if err != nil {
			return nil, fmt.Errorf("upsert sentence_view %s: %w", sv.ID, err)
		}
		res.ProcessedViews++
	}

	// 2. Insert visibility_events idempotently (INSERT OR IGNORE deduplicates by event ID)
	for _, ve := range payload.VisibilityEvents {
		if ve.ID == "" || ve.SentenceViewID == "" {
			continue
		}
		_, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO visibility_events (
				id, sentence_view_id, state, occurred_at, background_duration_ms
			) VALUES (?, ?, ?, ?, ?)
		`, ve.ID, ve.SentenceViewID, ve.State, ve.OccurredAt, ve.BackgroundDurationMs)
		if err != nil {
			return nil, fmt.Errorf("insert visibility_event %s: %w", ve.ID, err)
		}
		res.ProcessedVisibility++
	}

	// 3. Recompute valid dwell_time_ms for each sentence_view using deduplicated visibility_events rows
	for _, sv := range payload.SentenceViews {
		if sv.ID == "" || sv.SentenceID == "" {
			continue
		}
		rows, err := tx.QueryContext(ctx, "SELECT background_duration_ms FROM visibility_events WHERE sentence_view_id = ?", sv.ID)
		var dedupBg []int64
		if err == nil {
			for rows.Next() {
				var b int64
				if err := rows.Scan(&b); err == nil {
					dedupBg = append(dedupBg, b)
				}
			}
			_ = rows.Close()
		}
		dwellMs, isValid, invalidReason := ComputeValidDwell(sv.TotalElapsed, dedupBg)
		_, err = tx.ExecContext(ctx, `
			UPDATE sentence_views
			SET dwell_time_ms = ?,
			    is_valid = CASE
			        WHEN invalid_reason = 'manual_override' THEN is_valid
			        ELSE ?
			    END,
			    invalid_reason = CASE
			        WHEN invalid_reason = 'manual_override' THEN invalid_reason
			        ELSE ?
			    END
			WHERE id = ?
		`, dwellMs, isValid, invalidReason, sv.ID)
		if err != nil {
			return nil, fmt.Errorf("update dwell_time_ms for %s: %w", sv.ID, err)
		}
	}

	// 4. Insert character_events idempotently with rapid-double-tap anomaly detection
	for i, ce := range payload.CharacterEvents {
		if ce.ID == "" || ce.SentenceViewID == "" || ce.TokenID == "" {
			continue
		}
		isValid := 1
		invalidReason := ""
		if i > 0 {
			prev := payload.CharacterEvents[i-1]
			if prev.SentenceViewID == ce.SentenceViewID && prev.TokenID == ce.TokenID {
				t1, err1 := time.Parse(time.RFC3339Nano, prev.ClickedAt)
				t2, err2 := time.Parse(time.RFC3339Nano, ce.ClickedAt)
				if err1 == nil && err2 == nil && t2.Sub(t1) >= 0 && t2.Sub(t1) < 250*time.Millisecond {
					invalidReason = "suspected_rapid_tap"
				}
			}
		}

		_, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO character_events (
				id, sentence_view_id, token_id, character, pinyin_played, clicked_at, is_valid, invalid_reason
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, ce.ID, ce.SentenceViewID, ce.TokenID, ce.Character, ce.PinyinPlayed, ce.ClickedAt, isValid, invalidReason)
		if err != nil {
			return nil, fmt.Errorf("insert character_event %s: %w", ce.ID, err)
		}
		res.ProcessedCharEvents++
	}

	// 5. Insert audio_events idempotently
	for _, ae := range payload.AudioEvents {
		if ae.ID == "" || ae.SentenceViewID == "" {
			continue
		}
		audioType := ae.AudioType
		if audioType == "" {
			audioType = "sentence"
		}
		completedInt := 0
		if ae.Completed {
			completedInt = 1
		}
		_, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO audio_events (
				id, sentence_view_id, audio_type, played_at, play_duration_ms, completed
			) VALUES (?, ?, ?, ?, ?, ?)
		`, ae.ID, ae.SentenceViewID, audioType, ae.PlayedAt, ae.PlayDurationMs, completedInt)
		if err != nil {
			return nil, fmt.Errorf("insert audio_event %s: %w", ae.ID, err)
		}
		res.ProcessedAudio++
	}

	// 6. Update session progress & status if provided
	if payload.CurrentSentenceIndex != nil {
		if _, err := tx.ExecContext(ctx, `
			UPDATE reading_sessions
			SET current_sentence_index = ?
			WHERE id = ?
		`, *payload.CurrentSentenceIndex, sessionID); err != nil {
			return nil, fmt.Errorf("update session index: %w", err)
		}
	}
	if payload.SessionStatus != "" {
		var endedAt any
		if payload.SessionStatus == "completed" || payload.SessionStatus == "abandoned" {
			endedAt = time.Now().UTC().Format(time.RFC3339)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE reading_sessions
			SET status = ?, end_reason = COALESCE(NULLIF(?, ''), end_reason), ended_at = COALESCE(?, ended_at)
			WHERE id = ?
		`, payload.SessionStatus, payload.EndReason, endedAt, sessionID); err != nil {
			return nil, fmt.Errorf("update session status: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit batch: %w", err)
	}
	return res, nil
}

// MarkCharacterEventValidity marks a character click event as valid or invalid (e.g. accidental touch)
// while preserving the original raw event and recording the audit reason.
func (s *Service) MarkCharacterEventValidity(ctx context.Context, eventID string, isValid bool, reason string) error {
	valInt := 0
	if isValid {
		valInt = 1
	}
	if reason == "" && !isValid {
		reason = "guardian_marked_accidental"
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE character_events
		SET is_valid = ?, invalid_reason = ?
		WHERE id = ?
	`, valInt, reason, eventID)
	if err != nil {
		return fmt.Errorf("update character_event %s: %w", eventID, err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("character_event %s not found", eventID)
	}
	return nil
}

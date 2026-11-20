package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"

	"reader/server/internal/config"
	"reader/server/internal/content"
	"reader/server/internal/events"
	"reader/server/internal/jobs"
	"reader/server/internal/media"
	"reader/server/internal/updater"
)

type Server struct {
	cfg                     *config.Config
	db                      *sql.DB
	events                  *events.Service
	importer                *content.Importer
	jobs                    *jobs.Runner
	media                   *media.Handler
	webFS                   fs.FS
	updater                 *updater.Updater
	gistAPIBaseURL          string
	gistRawBaseURL          string
	allowLoopbackImageFetch bool
}

type httpStatusError struct {
	statusCode int
	err        error
}

func (e *httpStatusError) Error() string { return e.err.Error() }
func (e *httpStatusError) Unwrap() error { return e.err }

func NewServer(cfg *config.Config, dbConn *sql.DB, webFS fs.FS) *Server {
	srv := &Server{
		cfg:      cfg,
		db:       dbConn,
		events:   events.NewService(dbConn),
		importer: content.NewImporter(dbConn, cfg.MediaDir(), cfg.TmpDir()),
		jobs:     jobs.NewRunner(dbConn, cfg),
		media:    media.NewHandler(cfg.MediaDir()),
		webFS:    webFS,
		updater:  updater.New(cfg),
	}
	go srv.precacheAllPublishedStories()
	srv.StartDailyStorySyncWorker()
	return srv
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/health", s.handleHealth)
	mux.HandleFunc("/api/v1/tts", s.handleTTS)
	mux.HandleFunc("/api/v1/children", s.handleChildren)
	mux.HandleFunc("/api/v1/children/", s.handleChildSubroutes)
	mux.HandleFunc("/api/v1/stories", s.handleStories)
	mux.HandleFunc("/api/v1/stories/", s.handleStoryVersionDetail)
	mux.HandleFunc("/api/v1/sessions", s.handleSessions)
	mux.HandleFunc("/api/v1/sessions/", s.handleSessionEvents)
	mux.HandleFunc("/api/v1/admin/import", s.requireGuardianPIN(s.handleAdminImport))
	mux.HandleFunc("/api/v1/admin/quick-story", s.requireGuardianPIN(s.handleAdminQuickStory))
	mux.HandleFunc("/api/v1/admin/auto-generate-story", s.requireGuardianPIN(s.handleAdminAutoGenerateStory))
	mux.HandleFunc("/api/v1/admin/gist-pull", s.requireGuardianPIN(s.handleAdminGistPull))
	mux.HandleFunc("/api/v1/admin/export", s.requireGuardianPIN(s.handleAdminExport))
	mux.HandleFunc("/api/v1/admin/events/", s.requireGuardianPIN(s.handleAdminEventValidity))
	mux.HandleFunc("/api/v1/admin/stories/", s.requireGuardianPIN(s.handleAdminStoriesSubroutes))
	mux.HandleFunc("/api/v1/admin/update/check", s.requireGuardianPIN(s.handleAdminUpdateCheck))
	mux.HandleFunc("/api/v1/admin/update/apply", s.requireGuardianPIN(s.handleAdminUpdateApply))

	mux.Handle("/media/", s.media)

	if s.webFS != nil {
		fileServer := http.FileServer(http.FS(s.webFS))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			path := strings.TrimPrefix(r.URL.Path, "/")
			if path == "" {
				path = "index.html"
			}
			if _, err := fs.Stat(s.webFS, path); err != nil {
				r.URL.Path = "/"
			}
			fileServer.ServeHTTP(w, r)
		})
	}
	return mux
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(b))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) requireGuardianPIN(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pin := strings.TrimSpace(r.Header.Get("X-Guardian-Pin"))
		if pin == "" {
			pin = strings.TrimSpace(r.URL.Query().Get("pin"))
		}
		expectedPin := ""
		if s.cfg != nil {
			expectedPin = strings.TrimSpace(s.cfg.GuardianPin)
		}
		if expectedPin == "" || pin != expectedPin {
			writeError(w, http.StatusUnauthorized, "invalid guardian PIN")
			return
		}
		next(w, r)
	}
}

// verifyStorageWritable 运行时严格复查外接盘挂载标记与可用空间（fail-closed），
// 防止外接盘运行中掉线或空间检查失败时误写入路由器闪存。
func (s *Server) verifyStorageWritable() (int, error) {
	if s.cfg == nil {
		return http.StatusServiceUnavailable, errors.New("server config unavailable")
	}
	if err := s.cfg.VerifyDiskMount(); err != nil {
		return http.StatusServiceUnavailable, fmt.Errorf("external storage unavailable: %w", err)
	}
	freeMB, ok, err := s.cfg.CheckFreeDiskMB()
	if err != nil {
		return http.StatusServiceUnavailable, fmt.Errorf("disk space check failed: %w", err)
	}
	if !ok {
		return http.StatusInsufficientStorage, fmt.Errorf(
			"free disk space (%d MB) is below minimum required (%d MB)",
			freeMB, s.cfg.MinFreeDiskMB,
		)
	}
	return http.StatusOK, nil
}

// verifyDiskMountedOnly 运行时仅复查外接盘挂载（供删除接口使用，避免磁盘满时无法删除释放空间）。
func (s *Server) verifyDiskMountedOnly() (int, error) {
	if s.cfg == nil {
		return http.StatusServiceUnavailable, errors.New("server config unavailable")
	}
	if err := s.cfg.VerifyDiskMount(); err != nil {
		return http.StatusServiceUnavailable, fmt.Errorf("external storage unavailable: %w", err)
	}
	return http.StatusOK, nil
}

// maxUploadBytes 严格遵守配置中的 UploadMaxMB（仅在未配置或 <=0 时使用默认 200MB）
func (s *Server) maxUploadBytes() int64 {
	if s.cfg != nil && s.cfg.UploadMaxMB > 0 {
		return s.cfg.UploadMaxMB * 1024 * 1024
	}
	return 200 * 1024 * 1024
}

func isMaxBytesErr(err error) bool {
	if err == nil {
		return false
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "request body too large")
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	freeMB, ok, err := s.cfg.CheckFreeDiskMB()
	diskErr := ""
	if err != nil {
		diskErr = err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"version":          updater.CurrentVersion,
		"arch":             runtime.GOOS + "/" + runtime.GOARCH,
		"free_disk_mb":     freeMB,
		"disk_space_ok":    ok,
		"min_free_disk_mb": s.cfg.MinFreeDiskMB,
		"disk_error":       diskErr,
	})
}

func (s *Server) handleChildren(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, err := s.db.QueryContext(r.Context(), `
			SELECT id, name, COALESCE(avatar, ''), COALESCE(learning_stage, ''), created_at
			FROM children ORDER BY created_at ASC
		`)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer func() { _ = rows.Close() }()

		type childDTO struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			Avatar        string `json:"avatar"`
			LearningStage string `json:"learning_stage"`
			CreatedAt     string `json:"created_at"`
		}
		list := []childDTO{}
		for rows.Next() {
			var c childDTO
			if err := rows.Scan(&c.ID, &c.Name, &c.Avatar, &c.LearningStage, &c.CreatedAt); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			list = append(list, c)
		}
		writeJSON(w, http.StatusOK, map[string]any{"children": list})

	case http.MethodPost:
		var req struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			Avatar        string `json:"avatar"`
			Pin           string `json:"pin"`
			LearningStage string `json:"learning_stage"`
			PriorSource   string `json:"prior_source"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" {
			writeError(w, http.StatusBadRequest, "name is required")
			return
		}
		if req.ID == "" {
			req.ID = newID("child")
		}
		if req.Avatar == "" {
			req.Avatar = "🐻"
		}
		now := time.Now().UTC().Format(time.RFC3339)
		_, err := s.db.ExecContext(r.Context(), `
			INSERT INTO children (id, name, avatar, pin, learning_stage, prior_source, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, req.ID, req.Name, req.Avatar, req.Pin, req.LearningStage, req.PriorSource, now, now)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"id":             req.ID,
			"name":           req.Name,
			"avatar":         req.Avatar,
			"learning_stage": req.LearningStage,
			"created_at":     now,
		})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleChildSubroutes(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1/children/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "child id required")
		return
	}
	childID := parts[0]

	if len(parts) == 1 && r.Method == http.MethodDelete {
		s.requireGuardianPIN(func(w http.ResponseWriter, r *http.Request) {
			res, err := s.db.ExecContext(r.Context(), "DELETE FROM children WHERE id = ?", childID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			aff, _ := res.RowsAffected()
			if aff == 0 {
				writeError(w, http.StatusNotFound, "child not found")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"deleted": childID})
		})(w, r)
		return
	}

	if len(parts) == 2 && parts[1] == "vocabulary" && r.Method == http.MethodGet {
		rows, err := s.db.QueryContext(r.Context(), `
			SELECT character, pinyin, exposure_count, helped_view_count, help_count, COALESCE(last_help_at, ''), estimated_status
			FROM vocabulary_view
			WHERE child_id = ?
			ORDER BY help_count DESC, exposure_count DESC
		`, childID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer func() { _ = rows.Close() }()

		type vocabItem struct {
			Character       string `json:"character"`
			Pinyin          string `json:"pinyin"`
			ExposureCount   int    `json:"exposure_count"`
			HelpedViewCount int    `json:"helped_view_count"`
			HelpCount       int    `json:"help_count"`
			LastHelpAt      string `json:"last_help_at"`
			EstimatedStatus string `json:"estimated_status"`
		}
		items := []vocabItem{}
		for rows.Next() {
			var it vocabItem
			if err := rows.Scan(&it.Character, &it.Pinyin, &it.ExposureCount, &it.HelpedViewCount, &it.HelpCount, &it.LastHelpAt, &it.EstimatedStatus); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			items = append(items, it)
		}
		writeJSON(w, http.StatusOK, map[string]any{"child_id": childID, "vocabulary": items})
		return
	}

	if len(parts) == 2 && parts[1] == "story-plan" && r.Method == http.MethodGet {
		s.handleChildStoryPlan(w, r, childID)
		return
	}

	if len(parts) == 2 && parts[1] == "history" && r.Method == http.MethodGet {
		rows, err := s.db.QueryContext(r.Context(), `
			SELECT rs.id, rs.story_version_id, s.title, rs.mode, rs.status, COALESCE(rs.end_reason, ''),
			       rs.current_sentence_index, rs.started_at, COALESCE(rs.ended_at, '')
			FROM reading_sessions rs
			JOIN story_versions sv ON rs.story_version_id = sv.id
			JOIN stories s ON sv.story_id = s.id
			WHERE rs.child_id = ?
			ORDER BY rs.started_at DESC
		`, childID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer func() { _ = rows.Close() }()

		type historyItem struct {
			SessionID            string `json:"session_id"`
			StoryVersionID       string `json:"story_version_id"`
			StoryTitle           string `json:"story_title"`
			Mode                 string `json:"mode"`
			Status               string `json:"status"`
			EndReason            string `json:"end_reason"`
			CurrentSentenceIndex int    `json:"current_sentence_index"`
			StartedAt            string `json:"started_at"`
			EndedAt              string `json:"ended_at"`
		}
		history := []historyItem{}
		for rows.Next() {
			var h historyItem
			if err := rows.Scan(&h.SessionID, &h.StoryVersionID, &h.StoryTitle, &h.Mode, &h.Status, &h.EndReason, &h.CurrentSentenceIndex, &h.StartedAt, &h.EndedAt); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			history = append(history, h)
		}
		writeJSON(w, http.StatusOK, map[string]any{"child_id": childID, "sessions": history})
		return
	}

	writeError(w, http.StatusNotFound, "not found")
}

type planTarget struct {
	Character         string   `json:"character"`
	Pinyin            string   `json:"pinyin"`
	HelpCount         int      `json:"help_count"`
	ExposureCount     int      `json:"exposure_count"`
	RecommendedWords  []string `json:"recommended_words"`
	TargetRepetitions int      `json:"target_repetitions"`
	RetentionPct      int      `json:"retention_pct,omitempty"`
	ForgettingStage   string   `json:"forgetting_stage,omitempty"`
	Urgency           float64  `json:"urgency,omitempty"`
	Reason            string   `json:"reason"`
}

type childStoryPlanResult struct {
	ChildID               string                `json:"child_id"`
	Summary               map[string]any        `json:"summary"`
	ReadingScience        map[string]any        `json:"reading_science"`
	AvailableThemes       []content.ThemePreset `json:"available_themes"`
	ReviewTargets         []planTarget          `json:"review_targets"`
	ObserveTargets        []planTarget          `json:"observe_targets"`
	FamiliarAnchors       []planTarget          `json:"familiar_anchors"`
	NewCandidates         []planTarget          `json:"new_candidates"`
	StoryGenerationPrompt string                `json:"story_generation_prompt"`
}

const foundationalScaffoldWords = "的、一、是、在、有、和、小熊、六六、我、你、他、她、它、们、这、那、来、到、去、走、看、见、说、听、想、要、会、能、可、以、好、大、小、多、少、上、下、里、外、前、后、左、右、中、天、地、人、手、心、开、关、高、兴、快、乐、平、安、朋、友"

func parseSQLiteTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func computeForgettingMetrics(expCount, helpedViews, helpCount int, lastHelp, lastSeen string, now time.Time) (float64, int, float64, string) {
	if expCount < 1 {
		expCount = 1
	}
	unhelped := expCount - helpedViews
	if unhelped < 0 {
		unhelped = 0
	}
	elapsedHours := 4.0
	if helpCount > 0 {
		elapsedHours = 12.0
	}
	if t, ok := parseSQLiteTime(lastHelp); ok {
		if h := now.Sub(t).Hours(); h > 0.25 {
			elapsedHours = h
		} else {
			elapsedHours = 0.25
		}
	} else if t, ok := parseSQLiteTime(lastSeen); ok {
		if h := now.Sub(t).Hours(); h > 0.25 {
			elapsedHours = h
		} else {
			elapsedHours = 0.25
		}
	}

	uCap := unhelped
	if uCap > 6 {
		uCap = 6
	}
	stability := (18.0 * math.Pow(2.2, float64(uCap))) / (1.0 + 0.85*float64(helpCount))
	if stability < 1.0 {
		stability = 1.0
	}
	retention := math.Exp(-elapsedHours / stability)
	if helpCount > 2 {
		capR := 0.45 / (1.0 + 0.15*float64(helpCount-2))
		if retention > capR {
			retention = capR
		}
	} else if expCount < 3 && unhelped < 2 {
		if retention > 0.72 {
			retention = 0.72
		}
	}

	retPct := int(math.Round(retention * 100))
	if retPct < 5 {
		retPct = 5
	}
	if retPct > 99 {
		retPct = 99
	}

	urgency := (1.0 - retention) + 0.25*float64(helpCount)
	if expCount < 3 {
		urgency += 0.20
	}

	var stage string
	switch {
	case helpCount > 2:
		stage = fmt.Sprintf("高频求助易错期（求助 %d 次，记忆保留率 %d%%）", helpCount, retPct)
	case retention < 0.60:
		stage = fmt.Sprintf("艾宾浩斯遗忘临界期（记忆保留率 %d%%）", retPct)
	case expCount < 3:
		stage = fmt.Sprintf("艾宾浩斯初次巩固期（独立曝光 %d 次，保留率 %d%%）", expCount, retPct)
	case retention < 0.78:
		stage = fmt.Sprintf("艾宾浩斯周期复习节点（记忆保留率 %d%%）", retPct)
	default:
		stage = fmt.Sprintf("已稳固掌握（记忆保留率 %d%%）", retPct)
	}
	return retention, retPct, urgency, stage
}

func (s *Server) buildChildStoryPlan(ctx context.Context, childID string) (*childStoryPlanResult, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT character, pinyin, exposure_count, helped_view_count, help_count,
		       COALESCE(last_help_at, ''), COALESCE(last_seen_at, ''), estimated_status
		FROM vocabulary_view
		WHERE child_id = ?
		ORDER BY help_count DESC, exposure_count DESC
	`, childID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var forgettingReview []planTarget
	var forgettingObserve []planTarget
	familiarAnchors := []planTarget{}
	seenChars := map[string]bool{}
	now := time.Now().UTC()
	totalTracked := 0

	for rows.Next() {
		var ch, py, lastHelp, lastSeen, status string
		var expCount, helpedViews, helpCount int
		if err := rows.Scan(&ch, &py, &expCount, &helpedViews, &helpCount, &lastHelp, &lastSeen, &status); err != nil {
			continue
		}
		totalTracked++
		seenChars[ch] = true
		words := content.MatchWordsForCharPinyin(ch, py)
		ret, retPct, urgency, stage := computeForgettingMetrics(expCount, helpedViews, helpCount, lastHelp, lastSeen, now)

		item := planTarget{
			Character:        ch,
			Pinyin:           py,
			HelpCount:        helpCount,
			ExposureCount:    expCount,
			RecommendedWords: words,
			RetentionPct:     retPct,
			ForgettingStage:  stage,
			Urgency:          urgency,
		}

		if status == "needs_support" || (status == "likely_familiar" && ret < 0.78) {
			item.TargetRepetitions = 3
			item.Reason = fmt.Sprintf("%s，需在 10 分钟故事不同章节自然复现 ≥3 次", stage)
			forgettingReview = append(forgettingReview, item)
		} else if status == "insufficient_evidence" {
			item.TargetRepetitions = 2
			item.Reason = fmt.Sprintf("%s，需在故事中补充复现 ≥2 次以跨过遗忘拐点", stage)
			forgettingObserve = append(forgettingObserve, item)
		} else {
			item.TargetRepetitions = 1
			item.Reason = fmt.Sprintf("%s，作为已掌握熟字全面复用", stage)
			familiarAnchors = append(familiarAnchors, item)
		}
	}

	sort.Slice(forgettingReview, func(i, j int) bool { return forgettingReview[i].Urgency > forgettingReview[j].Urgency })
	sort.Slice(forgettingObserve, func(i, j int) bool { return forgettingObserve[i].Urgency > forgettingObserve[j].Urgency })

	reviewTargets := forgettingReview
	if len(reviewTargets) > 7 {
		reviewTargets = reviewTargets[:7]
	}
	observeTargets := forgettingObserve
	if len(observeTargets) > 4 {
		observeTargets = observeTargets[:4]
	}
	if reviewTargets == nil {
		reviewTargets = []planTarget{}
	}
	if observeTargets == nil {
		observeTargets = []planTarget{}
	}

	type sessionMetric struct {
		Date    string `json:"date"`
		Minutes int    `json:"minutes"`
		Title   string `json:"title"`
	}
	cstLoc := time.FixedZone("CST", 8*3600)
	sevenDaysAgo := now.AddDate(0, 0, -7)
	weeklyReadCount := 0
	var weeklyDwellMs int64
	var allDwellMs int64
	weeklyFallbackMins := 0
	allFallbackMins := 0
	var recentSessionsRev []sessionMetric

	type rawSessRow struct {
		id         string
		title      string
		startedStr string
		endedStr   string
		curIdx     int
	}
	var rawSessions []rawSessRow

	sessRows, sessErr := s.db.QueryContext(ctx, `
		SELECT rs.id, COALESCE(s.title, ''), rs.started_at, COALESCE(rs.ended_at, ''), rs.current_sentence_index
		FROM reading_sessions rs
		LEFT JOIN story_versions sv ON rs.story_version_id = sv.id
		LEFT JOIN stories s ON sv.story_id = s.id
		WHERE rs.child_id = ?
		ORDER BY rs.started_at DESC
		LIMIT 30
	`, childID)
	if sessErr == nil {
		for sessRows.Next() {
			var r rawSessRow
			if err := sessRows.Scan(&r.id, &r.title, &r.startedStr, &r.endedStr, &r.curIdx); err != nil {
				continue
			}
			rawSessions = append(rawSessions, r)
		}
		_ = sessRows.Close()
	}

	for _, r := range rawSessions {
		startT, okStart := parseSQLiteTime(r.startedStr)
		if !okStart {
			continue
		}
		effMs, viewCount := s.computeSessionReadingMs(ctx, r.id)
		mins := 0
		if viewCount > 0 {
			if effMs > 0 {
				allDwellMs += effMs
				if !startT.Before(sevenDaysAgo) {
					weeklyDwellMs += effMs
				}
				mins = int(math.Round(float64(effMs) / 60000.0))
				if mins < 1 {
					mins = 1
				}
			}
		} else {
			if endT, okEnd := parseSQLiteTime(r.endedStr); okEnd && endT.After(startT) {
				diffMins := int(math.Round(endT.Sub(startT).Minutes()))
				if diffMins >= 1 && diffMins <= 120 {
					mins = diffMins
				}
			}
			if mins <= 0 && r.curIdx > 0 {
				est := int(math.Round(float64(r.curIdx+1) * 0.6))
				if est < 1 {
					est = 1
				}
				mins = est
			}
			allFallbackMins += mins
			if !startT.Before(sevenDaysAgo) {
				weeklyFallbackMins += mins
			}
		}
		if !startT.Before(sevenDaysAgo) {
			weeklyReadCount++
		}
		if mins > 0 {
			recentSessionsRev = append(recentSessionsRev, sessionMetric{
				Date:    startT.In(cstLoc).Format("01-02"),
				Minutes: mins,
				Title:   r.title,
			})
		}
	}

	readingSessions := make([]sessionMetric, 0, len(recentSessionsRev))
	for i := len(recentSessionsRev) - 1; i >= 0; i-- {
		readingSessions = append(readingSessions, recentSessionsRev[i])
	}
	weeklyMinutes := int(math.Round(float64(weeklyDwellMs)/60000.0)) + weeklyFallbackMins
	allMinutes := int(math.Round(float64(allDwellMs)/60000.0)) + allFallbackMins
	totalReadMinutes := weeklyMinutes
	if totalReadMinutes == 0 {
		totalReadMinutes = allMinutes
	}

	totalBudget := 10
	forgettingCount := len(reviewTargets) + len(observeTargets)
	newQuota := totalBudget - forgettingCount
	if newQuota < 3 {
		newQuota = 3
	}
	if newQuota > 6 {
		newQuota = 6
	}

	starterPool := []struct{ Ch, Py, Word string }{
		{"水", "shuǐ", "河水"}, {"火", "huǒ", "火把"}, {"山", "shān", "高山"},
		{"木", "mù", "木桥"}, {"日", "rì", "日出"}, {"月", "yuè", "月亮"},
		{"大", "dà", "大家"}, {"小", "xiǎo", "小熊"}, {"上", "shàng", "上面"},
		{"下", "xià", "下面"}, {"左", "zuǒ", "左边"}, {"右", "yòu", "右边"},
		{"风", "fēng", "春风"}, {"雨", "yǔ", "大雨"}, {"花", "huā", "花朵"},
		{"草", "cǎo", "青草"}, {"鸟", "niǎo", "小鸟"}, {"鱼", "yú", "小鱼"},
		{"石", "shí", "石头"}, {"光", "guāng", "星光"}, {"桥", "qiáo", "木桥"},
		{"林", "lín", "森林"}, {"灯", "dēng", "红灯"}, {"宝", "bǎo", "宝石"},
		{"船", "chuán", "飞船"}, {"海", "hǎi", "大海"}, {"星", "xīng", "星星"},
		{"云", "yún", "白云"}, {"路", "lù", "小路"}, {"家", "jiā", "回家"},
	}

	newCandidates := []planTarget{}
	for _, st := range starterPool {
		if !seenChars[st.Ch] && len(newCandidates) < newQuota {
			words := content.MatchWordsForCharPinyin(st.Ch, st.Py)
			hasSample := false
			for _, w := range words {
				if w == st.Word {
					hasSample = true
					break
				}
			}
			if !hasSample {
				words = append([]string{st.Word}, words...)
			}
			newCandidates = append(newCandidates, planTarget{
				Character:         st.Ch,
				Pinyin:            st.Py,
				RecommendedWords:  words,
				TargetRepetitions: 3,
				Reason:            "按 10 分钟阅读科学比例（8%~10% 生字率）新引入的拓展生字，建议跨章节复现 ≥3 次",
			})
		}
	}

	var prompt strings.Builder
	prompt.WriteString("请为正在学习中文阅读的儿童编写一篇【10分钟阅读量（约 360~450 字，共 4 个章节、16~20 句话）】的趣味分级绘本故事。\n\n")
	prompt.WriteString("【一、十分钟阅读科学字数与最近发展区（ZPD）配比】\n")
	prompt.WriteString("- 目标阅读时长：10 分钟（按小学一年级儿童点读与理解速度 36~45 字/分钟测算）。\n")
	prompt.WriteString("- 篇幅与句长：共 4 个章节、16~20 句话（每页 1 句话，每句 18~24 个汉字，总字数 360~450 字）。\n")
	prompt.WriteString("- 科学识字配比（90%~92% 已掌握熟字 + 8%~10% 目标生字）：\n")
	prompt.WriteString(fmt.Sprintf("  1) 已掌握字词基座（占全篇总字数的 90%%~92%%，约 330~410 字次）：完整复用下方列出的全部已掌握字词（共 %d 个历史已掌握字音 + 基础高频熟字基座）。\n", len(familiarAnchors)))
	prompt.WriteString(fmt.Sprintf("  2) 科学比例生字池（占全篇总字数的 8%%~10%%，约 30~40 字次）：由系统根据【艾宾浩斯遗忘曲线】算出的 %d 个待复习生字 + %d 个全新拓展生字组成，每个生字在文中不同章节自然复现 2~3 次。\n\n", forgettingCount, len(newCandidates)))

	prompt.WriteString("【二、孩子已掌握的全部字词库（占全篇 90%~92%，请全面复用作为故事主干，切勿超纲使用未学过的生僻字）】\n")
	if len(familiarAnchors) > 0 {
		var famParts []string
		for _, t := range familiarAnchors {
			famParts = append(famParts, fmt.Sprintf("%s(%s：%s)", t.Character, t.Pinyin, strings.Join(t.RecommendedWords, "/")))
		}
		prompt.WriteString(fmt.Sprintf("- 历史已掌握的全部字词（共 %d 项，完整列出）：%s\n", len(familiarAnchors), strings.Join(famParts, "、")))
	} else {
		prompt.WriteString("- 历史已掌握字词：当前为初始建档阶段，尚无完整历史熟字记录，请直接使用下方基础高频熟字基座。\n")
	}
	prompt.WriteString(fmt.Sprintf("- 基础高频熟字与语法脚手架：%s\n\n", foundationalScaffoldWords))

	prompt.WriteString("【三、十分钟阅读科学比例生字池（占全篇 8%~10%：艾宾浩斯遗忘曲线复习字 + 新拓展生字）】\n")
	if len(reviewTargets) > 0 {
		var parts []string
		for _, t := range reviewTargets {
			parts = append(parts, fmt.Sprintf("「%s(%s)」（推荐词组：%s；状态：%s；要求自然复现 ≥%d 次）", t.Character, t.Pinyin, strings.Join(t.RecommendedWords, "/"), t.ForgettingStage, t.TargetRepetitions))
		}
		prompt.WriteString("1. 遗忘曲线急需复习生字（高频求助/接近遗忘临界点）：" + strings.Join(parts, "、") + "\n")
	}
	if len(observeTargets) > 0 {
		var parts []string
		for _, t := range observeTargets {
			parts = append(parts, fmt.Sprintf("「%s(%s)」（推荐词组：%s；状态：%s；要求自然复现 ≥%d 次）", t.Character, t.Pinyin, strings.Join(t.RecommendedWords, "/"), t.ForgettingStage, t.TargetRepetitions))
		}
		prompt.WriteString("2. 遗忘曲线待巩固生字（短期记忆巩固节点）：" + strings.Join(parts, "、") + "\n")
	}
	if len(reviewTargets) == 0 && len(observeTargets) == 0 {
		prompt.WriteString("1. 遗忘曲线待复习生字：当前无处于遗忘临界点的历史生字。\n")
	}
	if len(newCandidates) > 0 {
		var parts []string
		for _, t := range newCandidates {
			wSample := t.RecommendedWords
			if len(wSample) > 2 {
				wSample = wSample[:2]
			}
			parts = append(parts, fmt.Sprintf("「%s(%s，推荐词组：%s)」（要求跨章节自然复现 ≥%d 次）", t.Character, t.Pinyin, strings.Join(wSample, "/"), t.TargetRepetitions))
		}
		prompt.WriteString(fmt.Sprintf("3. 按科学比例新引入的拓展生字（共 %d 个，占全篇约 4%%~5%% 字次）：%s\n", len(newCandidates), strings.Join(parts, "、")))
	}

	prompt.WriteString("\n【四、排版与多音字语境强约束】\n")
	prompt.WriteString("- 请直接输出故事正文，每行严格对应 1 句话（共 16~20 行，或精简模式至少 5~8 行），不要输出多余解释。\n")
	prompt.WriteString("- 多音字必须严格匹配上述指定读音的语境词组（例如要求复习 háng 时使用“一行/银行”，要求复习 xíng 时使用“行走/通行”）。\n")
	prompt.WriteString("- 遗忘曲线复习字与新拓展生字应分布在故事的起承转合不同段落中（首次出现有上下文线索、中段复现、结尾巩固），严禁生硬堆砌。")

	return &childStoryPlanResult{
		ChildID: childID,
		Summary: map[string]any{
			"total_tracked_char_pinyins":  totalTracked,
			"needs_support_count":         len(reviewTargets),
			"insufficient_evidence_count": len(observeTargets),
			"likely_familiar_count":       len(familiarAnchors),
			"forgetting_curve_total":      forgettingCount,
			"new_candidates_count":        len(newCandidates),
			"reading_duration_minutes":    10,
			"target_total_chars":          "360~450",
			"weekly_read_count":           weeklyReadCount,
			"total_read_minutes":          totalReadMinutes,
			"reading_sessions":            readingSessions,
		},
		ReadingScience: map[string]any{
			"reading_duration_minutes":      10,
			"target_total_chars_min":        360,
			"target_total_chars_max":        450,
			"target_sentences_min":          16,
			"target_sentences_max":          20,
			"target_chars_per_sentence":     "18~24",
			"mastered_ratio_pct":            "90%~92%",
			"unfamiliar_ratio_pct":          "8%~10%",
			"target_unfamiliar_char_budget": totalBudget,
		},
		AvailableThemes:       content.BuiltinThemes,
		ReviewTargets:         reviewTargets,
		ObserveTargets:        observeTargets,
		FamiliarAnchors:       familiarAnchors,
		NewCandidates:         newCandidates,
		StoryGenerationPrompt: prompt.String(),
	}, nil
}

func (s *Server) handleChildStoryPlan(w http.ResponseWriter, r *http.Request, childID string) {
	plan, err := s.buildChildStoryPlan(r.Context(), childID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func isAllowedAnimationExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".mp4", ".webm", ".gif", ".webp":
		return true
	default:
		return false
	}
}

func isRewardAnimationFile(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".tmp") {
		return false
	}
	return strings.HasPrefix(lower, "reward.") || strings.HasPrefix(lower, "reward_")
}

func newRewardAnimationFilename(ext string) string {
	return newID("reward") + strings.ToLower(ext)
}

func removeExistingRewardAnimations(storyDir string) {
	removeOtherRewardAnimations(storyDir, "")
}

func removeOtherRewardAnimations(storyDir, keepName string) {
	entries, err := os.ReadDir(storyDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if isRewardAnimationFile(name) && name != keepName {
			_ = os.Remove(filepath.Join(storyDir, name))
		}
	}
}

// handleAdminStoriesSubroutes 分发 /api/v1/admin/stories/:id 及 /api/v1/admin/stories/:id/animation
func (s *Server) handleAdminStoriesSubroutes(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/stories/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 2 && parts[1] == "animation" {
		s.handleAdminStoryAnimation(w, r, parts[0])
		return
	}
	s.handleAdminDeleteStory(w, r)
}

// POST/DELETE /api/v1/admin/stories/:id/animation — 1 对 1 专属奖励动画安全流式上传与删除
// 失败时序保证：
// - POST: 每次使用唯一文件名写入临时文件 -> 重命名落盘（不覆盖旧文件） -> 更新 DB -> DB 失败则回滚删新文件，DB 成功后再清理旧文件
// - DELETE: 先更新 DB 清空 animation_url -> DB 失败不删文件，DB 成功后再清理磁盘文件
func (s *Server) handleAdminStoryAnimation(w http.ResponseWriter, r *http.Request, storyID string) {
	if err := content.ValidateSafeIdentifier("story_id", storyID); err != nil {
		writeError(w, http.StatusBadRequest, "valid story_id required")
		return
	}

	var existingTitle string
	err := s.db.QueryRowContext(
		r.Context(),
		"SELECT title FROM stories WHERE id = ? AND status != 'deleted'",
		storyID,
	).Scan(&existingTitle)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "story not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	storyDir := filepath.Join(s.cfg.MediaDir(), "stories", storyID)

	switch r.Method {
	case http.MethodPost:
		// 运行时复查外接盘挂载标记与磁盘空间（fail-closed）
		if statusCode, storageErr := s.verifyStorageWritable(); storageErr != nil {
			writeError(w, statusCode, storageErr.Error())
			return
		}

		maxBytes := s.maxUploadBytes()
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

		memLimit := int64(8 << 20)
		if maxBytes < memLimit {
			memLimit = maxBytes
		}
		if err := r.ParseMultipartForm(memLimit); err != nil {
			if isMaxBytesErr(err) {
				writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("animation exceeds maximum upload limit (%d MB)", s.cfg.UploadMaxMB))
				return
			}
			writeError(w, http.StatusBadRequest, fmt.Sprintf("parse multipart form: %v", err))
			return
		}
		if r.MultipartForm != nil {
			defer func() { _ = r.MultipartForm.RemoveAll() }()
		}

		file, header, err := r.FormFile("animation")
		if err != nil {
			writeError(w, http.StatusBadRequest, "missing 'animation' file field")
			return
		}
		defer func() { _ = file.Close() }()

		ext := strings.ToLower(filepath.Ext(header.Filename))
		if !isAllowedAnimationExt(ext) {
			writeError(w, http.StatusBadRequest, "unsupported animation format (allowed: .mp4, .webm, .gif, .webp)")
			return
		}

		if err := os.MkdirAll(storyDir, 0o755); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		// 先写入同目录临时文件
		tmpFile, err := os.CreateTemp(storyDir, ".reward_upload_*.tmp")
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("create temp animation file: %v", err))
			return
		}
		tmpPath := tmpFile.Name()
		cleanupTmp := true
		defer func() {
			if cleanupTmp {
				_ = os.Remove(tmpPath)
			}
		}()

		written, copyErr := io.Copy(tmpFile, io.LimitReader(file, maxBytes+1))
		if copyErr != nil {
			_ = tmpFile.Close()
			if isMaxBytesErr(copyErr) {
				writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("animation exceeds maximum upload limit (%d MB)", s.cfg.UploadMaxMB))
				return
			}
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("save animation: %v", copyErr))
			return
		}
		if written > maxBytes {
			_ = tmpFile.Close()
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("animation exceeds maximum upload limit (%d MB)", s.cfg.UploadMaxMB))
			return
		}
		if written == 0 {
			_ = tmpFile.Close()
			writeError(w, http.StatusBadRequest, "uploaded animation file is empty")
			return
		}
		if err := tmpFile.Sync(); err != nil {
			_ = tmpFile.Close()
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("sync animation file: %v", err))
			return
		}
		if err := tmpFile.Close(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = os.Chmod(tmpPath, 0o644)

		// 落盘提交前再次复查外接盘挂载状态
		if statusCode, storageErr := s.verifyStorageWritable(); storageErr != nil {
			writeError(w, statusCode, storageErr.Error())
			return
		}

		// 使用唯一新文件名提交，即使与旧动画同扩展名也绝不覆盖旧文件
		targetName := newRewardAnimationFilename(ext)
		targetPath := filepath.Join(storyDir, targetName)
		if err := os.Rename(tmpPath, targetPath); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("commit animation file: %v", err))
			return
		}
		cleanupTmp = false

		mediaURL := fmt.Sprintf("/media/stories/%s/%s", storyID, targetName)
		if _, err := s.db.ExecContext(r.Context(), "UPDATE stories SET animation_url = ? WHERE id = ?", mediaURL, storyID); err != nil {
			// DB 更新失败：立即回滚删除新文件，旧文件与旧数据库记录完好无损
			_ = os.Remove(targetPath)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		// DB 更新成功后，再清理旧动画文件
		removeOtherRewardAnimations(storyDir, targetName)

		writeJSON(w, http.StatusOK, map[string]any{
			"story_id":      storyID,
			"title":         existingTitle,
			"animation_url": mediaURL,
		})

	case http.MethodDelete:
		// 删除仅需确认外接盘在位（允许磁盘空间不足时删除以释放空间）
		if statusCode, storageErr := s.verifyDiskMountedOnly(); storageErr != nil {
			writeError(w, statusCode, storageErr.Error())
			return
		}
		// 严格先写数据库，再删磁盘文件；若数据库更新失败则保留磁盘文件
		if _, err := s.db.ExecContext(r.Context(), "UPDATE stories SET animation_url = '' WHERE id = ?", storyID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		removeExistingRewardAnimations(storyDir)
		writeJSON(w, http.StatusOK, map[string]any{
			"story_id":          storyID,
			"deleted_animation": true,
		})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleAdminDeleteStory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/stories/")
	if trimmed == r.URL.Path {
		trimmed = strings.TrimPrefix(r.URL.Path, "/api/v1/stories/")
	}
	storyID := strings.Trim(trimmed, "/")
	if err := content.ValidateSafeIdentifier("story_id", storyID); err != nil {
		writeError(w, http.StatusBadRequest, "valid story_id required")
		return
	}

	var title string
	err := s.db.QueryRowContext(r.Context(), "SELECT title FROM stories WHERE id = ? AND status != 'deleted'", storyID).Scan(&title)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "story not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var sessionCount int
	_ = s.db.QueryRowContext(r.Context(), `
		SELECT COUNT(*) FROM reading_sessions rs
		JOIN story_versions sv ON rs.story_version_id = sv.id
		WHERE sv.story_id = ?
	`, storyID).Scan(&sessionCount)

	hardDelete := r.URL.Query().Get("hard") == "1" || sessionCount == 0
	if hardDelete {
		if _, err := s.db.ExecContext(r.Context(), "DELETE FROM stories WHERE id = ?", storyID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		now := time.Now().UTC().Format(time.RFC3339)
		_, _ = s.db.ExecContext(r.Context(), "UPDATE stories SET status = 'deleted' WHERE id = ?", storyID)
		_, _ = s.db.ExecContext(r.Context(), "UPDATE story_versions SET status = 'deleted' WHERE story_id = ?", storyID)
		_, _ = s.db.ExecContext(r.Context(), `
			UPDATE reading_sessions
			SET status = 'abandoned', end_reason = 'story_deleted', ended_at = COALESCE(ended_at, ?)
			WHERE status = 'active' AND story_version_id IN (SELECT id FROM story_versions WHERE story_id = ?)
		`, now, storyID)
	}

	storyMediaPath := filepath.Join(s.cfg.MediaDir(), "stories", storyID)
	if !strings.Contains(storyID, "..") {
		_ = os.RemoveAll(storyMediaPath)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"deleted":                 storyID,
		"title":                   title,
		"preserved_vocab_history": !hardDelete,
	})
}

func (s *Server) handleAdminQuickStory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		StoryID      string                      `json:"story_id"`
		Title        string                      `json:"title"`
		Theme        string                      `json:"theme"`
		Description  string                      `json:"description"`
		RawText      string                      `json:"raw_text"`
		CustomImages []content.CustomImageUpload `json:"custom_images"`
		ImageAPIURL  string                      `json:"image_api_url"`
		ImageAPIKey  string                      `json:"image_api_key"`
		ImageModel   string                      `json:"image_model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	req.RawText = strings.TrimSpace(req.RawText)
	if req.Title == "" || req.RawText == "" {
		writeError(w, http.StatusBadRequest, "title and raw_text are required")
		return
	}
	req.StoryID = strings.TrimSpace(req.StoryID)
	if req.StoryID == "" {
		req.StoryID = newID("story")
	} else if err := content.ValidateSafeIdentifier("story_id", req.StoryID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Theme == "" {
		req.Theme = "勇气与探索（森林过河）"
	}

	var maxVer int
	_ = s.db.QueryRowContext(r.Context(), "SELECT COALESCE(MAX(version_number), 0) FROM story_versions WHERE story_id = ?", req.StoryID).Scan(&maxVer)
	nextVer := maxVer + 1
	versionID := fmt.Sprintf("%s_v%d", req.StoryID, nextVer)

	rawSentences := content.SplitSentences(req.RawText)
	if len(rawSentences) == 0 {
		writeError(w, http.StatusBadRequest, "no valid sentences found in raw_text")
		return
	}

	var imageURLs []string
	if len(req.CustomImages) > 0 {
		var saveErr error
		imageURLs, saveErr = content.SaveUserPreparedImages(s.cfg.MediaDir(), req.StoryID, versionID, req.CustomImages, len(rawSentences))
		if saveErr != nil {
			writeError(w, http.StatusBadRequest, saveErr.Error())
			return
		}
	}
	needsFallback := len(imageURLs) == 0
	if !needsFallback {
		for _, u := range imageURLs {
			if strings.TrimSpace(u) == "" {
				needsFallback = true
				break
			}
		}
	}
	if needsFallback {
		fallbackURLs, _ := content.RenderStoryIllustrations(
			r.Context(), s.cfg.MediaDir(), req.StoryID, versionID, req.Theme, rawSentences,
			req.ImageAPIURL, req.ImageAPIKey, req.ImageModel,
		)
		if len(imageURLs) == 0 {
			imageURLs = fallbackURLs
		} else {
			for idx := range imageURLs {
				if strings.TrimSpace(imageURLs[idx]) == "" && idx < len(fallbackURLs) {
					imageURLs[idx] = fallbackURLs[idx]
				}
			}
		}
	}

	var sentences []content.SentenceManifest
	for idx, sText := range rawSentences {
		sentID := fmt.Sprintf("%s_s%02d", versionID, idx)
		imgURL := ""
		if idx < len(imageURLs) {
			imgURL = imageURLs[idx]
		}
		tokens := content.SegmentAndAnnotate(sentID, sText)
		sentences = append(sentences, content.SentenceManifest{
			ID:            sentID,
			SequenceIndex: idx,
			Text:          sText,
			ImageURL:      imgURL,
			Tokens:        tokens,
		})
	}

	coverURL := ""
	if len(imageURLs) > 0 {
		coverURL = imageURLs[0]
	}

	manifest := &content.StoryManifest{
		StoryID:       req.StoryID,
		Title:         req.Title,
		Theme:         req.Theme,
		Description:   req.Description,
		CoverURL:      coverURL,
		VersionID:     versionID,
		VersionNumber: nextVer,
		Status:        "published",
		Sentences:     sentences,
	}

	if err := s.importer.SaveManifest(r.Context(), manifest); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	go s.precacheStoryVersionAudio(manifest.VersionID)

	writeJSON(w, http.StatusOK, map[string]any{
		"imported_story_id":   manifest.StoryID,
		"imported_version_id": manifest.VersionID,
		"version_number":      manifest.VersionNumber,
		"sentence_count":      len(manifest.Sentences),
		"cover_url":           coverURL,
	})
}

func (s *Server) handleAdminAutoGenerateStory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		ChildID     string `json:"child_id"`
		Theme       string `json:"theme"`
		AutoPublish bool   `json:"auto_publish"`
		LLMAPIURL   string `json:"llm_api_url"`
		LLMAPIKey   string `json:"llm_api_key"`
		LLMModel    string `json:"llm_model"`
		ImageAPIURL string `json:"image_api_url"`
		ImageAPIKey string `json:"image_api_key"`
		ImageModel  string `json:"image_model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	var reviewWords, observeWords, newWords []string
	var reviewChars []string
	var prompt string
	if plan, err := s.buildChildStoryPlan(r.Context(), req.ChildID); err == nil && plan != nil {
		prompt = plan.StoryGenerationPrompt
		for _, t := range plan.ReviewTargets {
			reviewChars = append(reviewChars, t.Character)
			reviewWords = append(reviewWords, t.RecommendedWords...)
		}
		for _, t := range plan.ObserveTargets {
			observeWords = append(observeWords, t.RecommendedWords...)
		}
		for _, t := range plan.NewCandidates {
			newWords = append(newWords, t.RecommendedWords...)
		}
	}
	if len(newWords) == 0 {
		newWords = []string{"春风", "河水"}
	}

	apiURL := req.LLMAPIURL
	if apiURL == "" {
		apiURL = s.cfg.LLM.APIURL
	}
	apiKey := req.LLMAPIKey
	if apiKey == "" {
		apiKey = s.cfg.LLM.APIKey
	}
	model := req.LLMModel
	if model == "" {
		model = s.cfg.LLM.Model
	}
	draft, err := content.GenerateAdaptiveStoryDraft(r.Context(), content.AutoStorySpec{
		Theme:        req.Theme,
		ReviewWords:  reviewWords,
		ObserveWords: observeWords,
		NewWords:     newWords,
		Prompt:       prompt,
		LLMAPIURL:    apiURL,
		LLMAPIKey:    apiKey,
		LLMModel:     model,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := map[string]any{
		"title":         draft.Title,
		"theme":         draft.Theme,
		"description":   draft.Description,
		"raw_text":      draft.RawText,
		"sentences":     draft.Sentences,
		"image_prompts": draft.ImagePrompts,
		"generator":     draft.Generator,
		"review_words":  reviewWords,
		"review_chars":  reviewChars,
		"published":     false,
	}

	if req.AutoPublish {
		storyID := newID("story")
		versionID := fmt.Sprintf("%s_v1", storyID)
		imageURLs, _ := content.RenderStoryIllustrations(
			r.Context(), s.cfg.MediaDir(), storyID, versionID, draft.Theme, draft.Sentences,
			req.ImageAPIURL, req.ImageAPIKey, req.ImageModel,
		)

		var sentences []content.SentenceManifest
		for idx, sText := range draft.Sentences {
			sentID := fmt.Sprintf("%s_s%02d", versionID, idx)
			imgURL := ""
			if idx < len(imageURLs) {
				imgURL = imageURLs[idx]
			}
			sentences = append(sentences, content.SentenceManifest{
				ID:            sentID,
				SequenceIndex: idx,
				Text:          sText,
				ImageURL:      imgURL,
				Tokens:        content.SegmentAndAnnotate(sentID, sText),
			})
		}
		coverURL := ""
		if len(imageURLs) > 0 {
			coverURL = imageURLs[0]
		}
		manifest := &content.StoryManifest{
			StoryID:       storyID,
			Title:         draft.Title,
			Theme:         draft.Theme,
			Description:   draft.Description,
			CoverURL:      coverURL,
			VersionID:     versionID,
			VersionNumber: 1,
			Status:        "published",
			Sentences:     sentences,
		}
		if err := s.importer.SaveManifest(r.Context(), manifest); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		go s.precacheStoryVersionAudio(manifest.VersionID)
		resp["published"] = true
		resp["imported_story_id"] = storyID
		resp["imported_version_id"] = versionID
		resp["version_number"] = 1
		resp["sentence_count"] = len(sentences)
		resp["cover_url"] = coverURL
		resp["image_urls"] = imageURLs
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleStories(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	childID := r.URL.Query().Get("child_id")

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT s.id, s.title, COALESCE(s.theme, ''), COALESCE(s.description, ''),
		       COALESCE(
		           NULLIF((
		               SELECT ss_img.image_url
		               FROM story_sentences ss_img
		               WHERE ss_img.version_id = sv.id AND COALESCE(TRIM(ss_img.image_url), '') != ''
		               ORDER BY ss_img.sequence_index ASC
		               LIMIT 1
		           ), ''),
		           COALESCE(s.cover_url, '')
		       ) AS cover_url,
		       COALESCE(s.animation_url, ''), COALESCE(s.created_at, ''),
		       sv.id AS version_id, sv.version_number,
		       (SELECT COUNT(*) FROM story_sentences ss WHERE ss.version_id = sv.id) AS sentence_count
		FROM stories s
		JOIN story_versions sv ON sv.story_id = s.id
		WHERE s.status = 'published' AND sv.status = 'published'
		  AND sv.version_number = (
			SELECT MAX(sv2.version_number) FROM story_versions sv2
			WHERE sv2.story_id = s.id AND sv2.status = 'published'
		  )
		ORDER BY s.created_at DESC
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = rows.Close() }()

	type storyItem struct {
		ID                string `json:"id"`
		Title             string `json:"title"`
		Theme             string `json:"theme"`
		Description       string `json:"description"`
		CoverURL          string `json:"cover_url"`
		AnimationURL      string `json:"animation_url,omitempty"`
		CreatedAt         string `json:"created_at,omitempty"`
		LatestVersionID   string `json:"latest_version_id"`
		VersionNumber     int    `json:"version_number"`
		SentenceCount     int    `json:"sentence_count"`
		ActiveSessionID   string `json:"active_session_id,omitempty"`
		ResumeSentenceIdx int    `json:"resume_sentence_index"`
		ReviewCharCount   int    `json:"review_char_count"`
		RecommendedReason string `json:"recommended_reason,omitempty"`
	}

	list := []storyItem{}
	for rows.Next() {
		var st storyItem
		if err := rows.Scan(
			&st.ID, &st.Title, &st.Theme, &st.Description, &st.CoverURL,
			&st.AnimationURL, &st.CreatedAt,
			&st.LatestVersionID, &st.VersionNumber, &st.SentenceCount,
		); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if childID != "" {
			_ = s.db.QueryRowContext(r.Context(), `
				SELECT id, current_sentence_index FROM reading_sessions
				WHERE child_id = ? AND story_version_id = ? AND status = 'active'
				ORDER BY started_at DESC LIMIT 1
			`, childID, st.LatestVersionID).Scan(&st.ActiveSessionID, &st.ResumeSentenceIdx)

			_ = s.db.QueryRowContext(r.Context(), `
				SELECT COUNT(DISTINCT st.character)
				FROM story_sentences ss
				JOIN sentence_tokens st ON st.sentence_id = ss.id
				JOIN vocabulary_view vv ON vv.character = st.character AND vv.pinyin = st.pinyin
				WHERE ss.version_id = ? AND vv.child_id = ? AND vv.estimated_status = 'needs_support'
			`, st.LatestVersionID, childID).Scan(&st.ReviewCharCount)

			if st.ActiveSessionID != "" {
				st.RecommendedReason = fmt.Sprintf("继续上次阅读（第 %d 句）", st.ResumeSentenceIdx+1)
			} else if st.ReviewCharCount > 0 {
				st.RecommendedReason = fmt.Sprintf("包含 %d 个重点复习字音", st.ReviewCharCount)
			} else {
				st.RecommendedReason = "适合当前阅读阶段"
			}
		}
		list = append(list, st)
	}
	writeJSON(w, http.StatusOK, map[string]any{"stories": list})
}

func (s *Server) handleStoryVersionDetail(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1/stories/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 1 && parts[0] != "" && r.Method == http.MethodDelete {
		s.requireGuardianPIN(s.handleAdminDeleteStory)(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if len(parts) != 3 || parts[1] != "versions" {
		writeError(w, http.StatusNotFound, "expected /api/v1/stories/:id/versions/:vid")
		return
	}
	storyID, versionID := parts[0], parts[2]

	var title, theme, desc, coverURL, animationURL string
	var versionNum int
	err := s.db.QueryRowContext(r.Context(), `
		SELECT s.title, COALESCE(s.theme, ''), COALESCE(s.description, ''), COALESCE(s.cover_url, ''),
		       COALESCE(s.animation_url, ''), sv.version_number
		FROM stories s
		JOIN story_versions sv ON sv.story_id = s.id
		WHERE s.id = ? AND sv.id = ?
	`, storyID, versionID).Scan(&title, &theme, &desc, &coverURL, &animationURL, &versionNum)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "story version not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	sentRows, err := s.db.QueryContext(r.Context(), `
		SELECT id, sequence_index, text, COALESCE(image_url, ''), COALESCE(audio_url, '')
		FROM story_sentences
		WHERE version_id = ?
		ORDER BY sequence_index ASC
	`, versionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = sentRows.Close() }()

	type tokenDTO struct {
		ID          string `json:"id"`
		TokenIndex  int    `json:"token_index"`
		Character   string `json:"character"`
		Pinyin      string `json:"pinyin"`
		Word        string `json:"word"`
		WordIndex   int    `json:"word_index"`
		AudioURL    string `json:"audio_url"`
		IsPolyphone bool   `json:"is_polyphone"`
	}
	type sentenceDTO struct {
		ID            string     `json:"id"`
		SequenceIndex int        `json:"sequence_index"`
		Text          string     `json:"text"`
		ImageURL      string     `json:"image_url"`
		AudioURL      string     `json:"audio_url"`
		Tokens        []tokenDTO `json:"tokens"`
	}

	sentences := []sentenceDTO{}
	for sentRows.Next() {
		var sent sentenceDTO
		if err := sentRows.Scan(&sent.ID, &sent.SequenceIndex, &sent.Text, &sent.ImageURL, &sent.AudioURL); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		sentences = append(sentences, sent)
	}
	_ = sentRows.Close()

	for i := range sentences {
		tokRows, err := s.db.QueryContext(r.Context(), `
			SELECT id, token_index, character, pinyin, word, word_index, COALESCE(audio_url, ''), is_polyphone
			FROM sentence_tokens
			WHERE sentence_id = ?
			ORDER BY token_index ASC
		`, sentences[i].ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		tokens := []tokenDTO{}
		needsReannotate := false
		for tokRows.Next() {
			var t tokenDTO
			var poly int
			if err := tokRows.Scan(&t.ID, &t.TokenIndex, &t.Character, &t.Pinyin, &t.Word, &t.WordIndex, &t.AudioURL, &poly); err != nil {
				_ = tokRows.Close()
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			t.IsPolyphone = poly == 1
			if !s.isRealMediaAudio(t.AudioURL) {
				t.AudioURL = ""
			}
			if !isPunctuationChar(t.Character) && (t.Pinyin == "" || t.Pinyin == t.Character) {
				needsReannotate = true
			}
			tokens = append(tokens, t)
		}
		_ = tokRows.Close()
		if !s.isRealMediaAudio(sentences[i].AudioURL) {
			sentences[i].AudioURL = ""
		}
		if needsReannotate {
			reTokens := content.SegmentAndAnnotate(sentences[i].ID, sentences[i].Text)
			for idx := range tokens {
				if idx < len(reTokens) && tokens[idx].Character == reTokens[idx].Character {
					tokens[idx].Pinyin = reTokens[idx].Pinyin
					tokens[idx].Word = reTokens[idx].Word
					tokens[idx].WordIndex = reTokens[idx].WordIndex
					tokens[idx].IsPolyphone = reTokens[idx].IsPolyphone
					polyInt := 0
					if tokens[idx].IsPolyphone {
						polyInt = 1
					}
					_, _ = s.db.ExecContext(r.Context(), `
						UPDATE sentence_tokens SET pinyin = ?, word = ?, word_index = ?, is_polyphone = ? WHERE id = ?
					`, tokens[idx].Pinyin, tokens[idx].Word, tokens[idx].WordIndex, polyInt, tokens[idx].ID)
				}
			}
		}
		sentences[i].Tokens = tokens
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"story_id":       storyID,
		"version_id":     versionID,
		"version_number": versionNum,
		"title":          title,
		"theme":          theme,
		"description":    desc,
		"cover_url":      coverURL,
		"animation_url":  animationURL,
		"sentences":      sentences,
	})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		ChildID           string `json:"child_id"`
		StoryVersionID    string `json:"story_version_id"`
		Mode              string `json:"mode"`
		PinyinHintEnabled bool   `json:"pinyin_hint_enabled"`
		ForceNew          bool   `json:"force_new"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if req.ChildID == "" || req.StoryVersionID == "" {
		writeError(w, http.StatusBadRequest, "child_id and story_version_id are required")
		return
	}
	if req.Mode == "" {
		req.Mode = "solo"
	}

	if !req.ForceNew {
		var existingID, mode, startedAt string
		var curIdx int
		err := s.db.QueryRowContext(r.Context(), `
			SELECT id, mode, current_sentence_index, started_at
			FROM reading_sessions
			WHERE child_id = ? AND story_version_id = ? AND status = 'active'
			ORDER BY started_at DESC LIMIT 1
		`, req.ChildID, req.StoryVersionID).Scan(&existingID, &mode, &curIdx, &startedAt)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"id":                     existingID,
				"child_id":               req.ChildID,
				"story_version_id":       req.StoryVersionID,
				"mode":                   mode,
				"status":                 "active",
				"current_sentence_index": curIdx,
				"resumed":                true,
				"started_at":             startedAt,
			})
			return
		}
	}

	sessionID := newID("sess")
	now := time.Now().UTC().Format(time.RFC3339)
	pinyinInt := 0
	if req.PinyinHintEnabled {
		pinyinInt = 1
	}
	_, err := s.db.ExecContext(r.Context(), `
		INSERT INTO reading_sessions (
			id, child_id, story_version_id, mode, status, current_sentence_index, pinyin_hint_enabled, started_at
		) VALUES (?, ?, ?, ?, 'active', 0, ?, ?)
	`, sessionID, req.ChildID, req.StoryVersionID, req.Mode, pinyinInt, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":                     sessionID,
		"child_id":               req.ChildID,
		"story_version_id":       req.StoryVersionID,
		"mode":                   req.Mode,
		"status":                 "active",
		"current_sentence_index": 0,
		"resumed":                false,
		"started_at":             now,
	})
}

func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) != 2 || parts[1] != "events" || parts[0] == "" {
		writeError(w, http.StatusNotFound, "expected /api/v1/sessions/:id/events")
		return
	}
	sessionID := parts[0]

	var payload events.BatchPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}

	res, err := s.events.IngestBatch(r.Context(), sessionID, payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	isExitingReader := payload.SessionStatus == "completed" ||
		payload.SessionStatus == "abandoned" ||
		payload.EndReason == "user_exit" ||
		payload.EndReason == "finished"

	if isExitingReader {
		go s.syncSessionPromptToGist(sessionID, payload.SessionStatus, payload.EndReason)
	}

	writeJSON(w, http.StatusOK, res)
}

func (s *Server) syncSessionPromptToGist(sessionID, sessionStatus, endReason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var childID, childName string
	err := s.db.QueryRowContext(ctx, `
		SELECT rs.child_id, COALESCE(c.name, '')
		FROM reading_sessions rs
		LEFT JOIN children c ON c.id = rs.child_id
		WHERE rs.id = ?
	`, sessionID).Scan(&childID, &childName)
	if err != nil || childID == "" {
		return
	}

	plan, err := s.buildChildStoryPlan(ctx, childID)
	if err != nil || plan == nil || strings.TrimSpace(plan.StoryGenerationPrompt) == "" {
		return
	}

	loc := time.FixedZone("CST", 8*3600)
	now := time.Now().In(loc)
	version := now.Format("20060102150405")
	updatedAt := now.Format("2006-01-02 15:04:05")

	triggerType := "completed"
	if endReason == "user_exit" && sessionStatus != "completed" {
		triggerType = "partial_exit (中途保存退出)"
	}

	metaHeader := fmt.Sprintf(
		"<!-- READING_SYNC_META\nversion: %s\nupdated_at: %s\nchild_id: %s\nchild_name: %s\nsession_id: %s\ntrigger: %s\n-->\n\n",
		version, updatedAt, childID, childName, sessionID, triggerType,
	)
	fullContent := metaHeader + plan.StoryGenerationPrompt

	const gistID = "aa8591c33578eb903d881cd133ec8fc3"
	githubToken := strings.TrimSpace(os.Getenv("GITHUB_GIST_TOKEN"))
	if githubToken == "" {
		return
	}

	reqBody := map[string]any{
		"description": fmt.Sprintf("自适应阅读 10 分钟科学配比 AI 提示词 (最新更新: %s [%s])", updatedAt, triggerType),
		"files": map[string]any{
			"next_prompt.md": map[string]string{
				"content": fullContent,
			},
		},
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, "https://api.github.com/gists/"+gistID, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "reader-gist-sync/1.0")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
}

func (s *Server) StartDailyStorySyncWorker() {
	go func() {
		loc := time.FixedZone("CST", 8*3600)
		for {
			now := time.Now().In(loc)
			next := time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, loc)
			if !now.Before(next) {
				next = next.Add(24 * time.Hour)
			}
			time.Sleep(time.Until(next))
			s.fetchAndImportLatestStoryFromGist()
		}
	}()
}

type gistRewardAnimation struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	MediaType string `json:"media_type"`
	Poster    string `json:"poster"`
}

type gistStoryPayload struct {
	Version         string               `json:"version"`
	StoryID         string               `json:"story_id"`
	Title           string               `json:"title"`
	Theme           string               `json:"theme"`
	Description     string               `json:"description"`
	RawText         string               `json:"raw_text"`
	Sentences       []string             `json:"sentences"`
	ImageURLs       []string             `json:"image_urls"`
	Images          []string             `json:"images"`
	AnimationURL    string               `json:"animation_url"`
	RewardAnimation *gistRewardAnimation `json:"reward_animation"`
}

type gistImportResult struct {
	Status            string   `json:"status"`
	Version           string   `json:"version"`
	ImportedStoryID   string   `json:"imported_story_id,omitempty"`
	ImportedVersionID string   `json:"imported_version_id,omitempty"`
	VersionNumber     int      `json:"version_number,omitempty"`
	Title             string   `json:"title"`
	Theme             string   `json:"theme"`
	Description       string   `json:"description"`
	RawText           string   `json:"raw_text"`
	Sentences         []string `json:"sentences"`
	SentenceCount     int      `json:"sentence_count"`
	ImageCount        int      `json:"image_count"`
	LinesPerImage     string   `json:"lines_per_image"`
	CoverURL          string   `json:"cover_url,omitempty"`
	AnimationURL      string   `json:"animation_url,omitempty"`
	ImageURLs         []string `json:"image_urls,omitempty"`
	Source            string   `json:"source"`
}

func extractMetaField(content, fieldName string) string {
	prefix := fieldName + ":"
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
		}
	}
	return ""
}

func parseGistStoryContent(contentStr string) (gistStoryPayload, bool) {
	contentStr = strings.TrimSpace(contentStr)
	if contentStr == "" {
		return gistStoryPayload{}, false
	}
	metaVer := extractMetaField(contentStr, "version")

	jsonPart := contentStr
	if idx := strings.Index(jsonPart, "-->"); idx >= 0 {
		jsonPart = strings.TrimSpace(jsonPart[idx+3:])
	}
	if startIdx := strings.Index(jsonPart, "{"); startIdx >= 0 {
		if endIdx := strings.LastIndex(jsonPart, "}"); endIdx > startIdx {
			jsonPart = jsonPart[startIdx : endIdx+1]
		}
	}

	var p gistStoryPayload
	if err := json.Unmarshal([]byte(jsonPart), &p); err == nil {
		if p.Version == "" {
			p.Version = metaVer
		}
		if strings.TrimSpace(p.Title) != "" && (len(p.Sentences) > 0 || strings.TrimSpace(p.RawText) != "") {
			return p, true
		}
	}
	return gistStoryPayload{}, false
}

func isBlockedOrPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
		if ip4[0] == 0 || ip4[0] == 127 || (ip4[0] == 169 && ip4[1] == 254) || ip4[0] >= 224 {
			return true
		}
		return false
	}
	if len(ip) == net.IPv6len && (ip[0]&0xfe) == 0xfc {
		return true
	}
	return false
}

func validatePublicRemoteImageURL(rawURL string, allowLoopback bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("invalid image url: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if !allowLoopback {
		if scheme != "https" {
			return nil, fmt.Errorf("disallowed image url scheme %q (only https allowed)", parsed.Scheme)
		}
	} else if scheme != "https" && scheme != "http" {
		return nil, fmt.Errorf("disallowed image url scheme %q", parsed.Scheme)
	}
	if parsed.User != nil {
		return nil, errors.New("credentials in image url are not allowed")
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	if host == "" {
		return nil, errors.New("empty host in image url")
	}
	if !allowLoopback {
		if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".lan") || strings.HasSuffix(host, ".internal") {
			return nil, fmt.Errorf("disallowed local host %q", host)
		}
		if ip := net.ParseIP(host); ip != nil && isBlockedOrPrivateIP(ip) {
			return nil, fmt.Errorf("disallowed private or loopback IP %s", ip.String())
		}
	}
	return parsed, nil
}

func (s *Server) newSSRFProtectedImageClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("no IP addresses resolved for %s", host)
			}
			for _, ipAddr := range ips {
				if !s.allowLoopbackImageFetch && isBlockedOrPrivateIP(ipAddr.IP) {
					return nil, fmt.Errorf("ssrf blocked: host %s resolved to non-public IP %s", host, ipAddr.IP.String())
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
	}
	return &http.Client{
		Timeout:   45 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects when fetching remote media")
			}
			_, err := validatePublicRemoteImageURL(req.URL.String(), s.allowLoopbackImageFetch)
			return err
		},
	}
}

// downloadRemoteStoryAnimation 使用 SSRF 防护客户端流式下载远程动画到唯一新文件名（不覆盖旧文件）
// 由调用方在 DB 更新成功后再清理旧动画；DB 更新失败则回滚删除新文件。
func (s *Server) downloadRemoteStoryAnimation(ctx context.Context, imgClient *http.Client, storyID, rawAnimURL string) (mediaURL, targetName, targetPath string) {
	rawAnimURL = strings.TrimSpace(rawAnimURL)
	if rawAnimURL == "" {
		return "", "", ""
	}
	if _, err := s.verifyStorageWritable(); err != nil {
		return "", "", ""
	}
	parsedU, err := validatePublicRemoteImageURL(rawAnimURL, s.allowLoopbackImageFetch)
	if err != nil {
		return "", "", ""
	}
	ext := strings.ToLower(filepath.Ext(parsedU.Path))
	if !isAllowedAnimationExt(ext) {
		ext = ".mp4"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedU.String(), nil)
	if err != nil {
		return "", "", ""
	}
	req.Header.Set("Cache-Control", "no-cache, no-store, max-age=0")
	req.Header.Set("User-Agent", "reader-gist-sync/1.0")
	resp, err := imgClient.Do(req)
	if err != nil {
		return "", "", ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", ""
	}

	storyDir := filepath.Join(s.cfg.MediaDir(), "stories", storyID)
	if err := os.MkdirAll(storyDir, 0o755); err != nil {
		return "", "", ""
	}

	maxBytes := s.maxUploadBytes()
	if maxBytes > 64*1024*1024 {
		maxBytes = 64 * 1024 * 1024
	}

	tmpFile, err := os.CreateTemp(storyDir, ".reward_remote_*.tmp")
	if err != nil {
		return "", "", ""
	}
	tmpPath := tmpFile.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	written, copyErr := io.Copy(tmpFile, io.LimitReader(resp.Body, maxBytes+1))
	syncErr := tmpFile.Sync()
	closeErr := tmpFile.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || written < 512 || written > maxBytes {
		return "", "", ""
	}
	_ = os.Chmod(tmpPath, 0o644)

	if _, err := s.verifyStorageWritable(); err != nil {
		return "", "", ""
	}

	targetName = newRewardAnimationFilename(ext)
	targetPath = filepath.Join(storyDir, targetName)
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return "", "", ""
	}
	cleanupTmp = false

	return fmt.Sprintf("/media/stories/%s/%s", storyID, targetName), targetName, targetPath
}

func (s *Server) pullAndImportStoryFromGist(ctx context.Context, forceImport bool) (*gistImportResult, error) {
	// 入口处先校验外接盘挂载标记与磁盘空间（覆盖手动拉取与凌晨 3 点定时任务），防止外接盘缺失时写入路由器闪存
	if statusCode, storageErr := s.verifyStorageWritable(); storageErr != nil {
		return nil, &httpStatusError{statusCode: statusCode, err: storageErr}
	}

	const gistID = "aa8591c33578eb903d881cd133ec8fc3"
	githubToken := strings.TrimSpace(os.Getenv("GITHUB_GIST_TOKEN"))

	apiBase := strings.TrimRight(strings.TrimSpace(s.gistAPIBaseURL), "/")
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	rawBase := strings.TrimRight(strings.TrimSpace(s.gistRawBaseURL), "/")
	if rawBase == "" {
		rawBase = "https://gist.githubusercontent.com/houalibaba/" + gistID + "/raw"
	}

	client := &http.Client{Timeout: 30 * time.Second}
	var payload gistStoryPayload
	var parsedOK bool
	var sourceUsed string
	var gistSiblingFiles map[string]struct {
		Content string `json:"content"`
		RawURL  string `json:"raw_url"`
	}

	apiURL := fmt.Sprintf("%s/gists/%s?t=%d", apiBase, gistID, time.Now().UnixNano())
	if apiReq, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil); err == nil {
		apiReq.Header.Set("Accept", "application/vnd.github+json")
		apiReq.Header.Set("Cache-Control", "no-cache, no-store, max-age=0")
		apiReq.Header.Set("Pragma", "no-cache")
		apiReq.Header.Set("User-Agent", "reader-gist-sync/1.0")
		if githubToken != "" {
			apiReq.Header.Set("Authorization", "Bearer "+githubToken)
		}
		if resp, err := client.Do(apiReq); err == nil {
			bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
			_ = resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK && len(bodyBytes) > 0 {
				var gistObj struct {
					Files map[string]struct {
						Content string `json:"content"`
						RawURL  string `json:"raw_url"`
					} `json:"files"`
				}
				if err := json.Unmarshal(bodyBytes, &gistObj); err == nil && len(gistObj.Files) > 0 {
					gistSiblingFiles = gistObj.Files
					candidateNames := []string{"next_story.json"}
					for fname := range gistObj.Files {
						if fname != "next_story.json" && strings.HasSuffix(strings.ToLower(fname), ".json") {
							candidateNames = append(candidateNames, fname)
						}
					}
					candidateNames = append(candidateNames, "next_prompt.md")
					for _, fname := range candidateNames {
						if f, ok := gistObj.Files[fname]; ok && strings.TrimSpace(f.Content) != "" {
							if p, ok := parseGistStoryContent(f.Content); ok {
								payload = p
								parsedOK = true
								sourceUsed = "github_api:" + fname
								break
							}
						}
					}
				}
			}
		}
	}

	if !parsedOK {
		candidates := []string{
			rawBase + "/next_story.json",
			rawBase + "/next_prompt.md",
		}
		for _, rawURL := range candidates {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s?t=%d", rawURL, time.Now().UnixNano()), nil)
			if err != nil {
				continue
			}
			req.Header.Set("Cache-Control", "no-cache, no-store, max-age=0")
			req.Header.Set("Pragma", "no-cache")
			req.Header.Set("User-Agent", "reader-gist-sync/1.0")
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
			_ = resp.Body.Close()
			if readErr != nil || resp.StatusCode != http.StatusOK || len(bodyBytes) == 0 {
				continue
			}
			if p, ok := parseGistStoryContent(string(bodyBytes)); ok {
				payload = p
				parsedOK = true
				sourceUsed = "raw_nocache:" + filepath.Base(rawURL)
				break
			}
		}
	}

	if !parsedOK {
		return nil, errors.New("Gist 中暂未找到有效的新故事 JSON（请确认 Gist 中已上传包含 title 与 sentences/raw_text 的 next_story.json）")
	}

	storyID := strings.TrimSpace(payload.StoryID)
	if storyID == "" {
		storyID = newID("story")
	} else if err := content.ValidateSafeIdentifier("story_id", storyID); err != nil {
		return nil, fmt.Errorf("invalid story_id in gist payload: %w", err)
	}

	rawSentences := payload.Sentences
	if len(rawSentences) == 0 {
		rawSentences = content.SplitSentences(payload.RawText)
	}
	if len(rawSentences) == 0 {
		return nil, errors.New("Gist 故事内容中没有解析到有效句子")
	}

	verStr := strings.TrimSpace(payload.Version)
	if verStr == "" {
		verStr = time.Now().In(time.FixedZone("CST", 8*3600)).Format("20060102150405")
	}
	versionTag := "[GistVersion:" + verStr + "]"

	if !forceImport {
		var existingID, existingTitle, existingVersionID, existingAnimURL string
		err := s.db.QueryRowContext(ctx, `
			SELECT s.id, s.title, COALESCE(s.animation_url, ''), COALESCE(
				(SELECT sv.id FROM story_versions sv WHERE sv.story_id = s.id AND sv.status = 'published' ORDER BY sv.version_number DESC LIMIT 1),
				''
			)
			FROM stories s
			WHERE s.description LIKE ? AND s.status != 'deleted'
			LIMIT 1
		`, "%"+versionTag+"%").Scan(&existingID, &existingTitle, &existingAnimURL, &existingVersionID)
		if err == nil && existingID != "" {
			var existingImageURLs []string
			if existingVersionID != "" {
				rows, qErr := s.db.QueryContext(ctx, "SELECT COALESCE(image_url, '') FROM story_sentences WHERE version_id = ? ORDER BY sequence_index ASC", existingVersionID)
				if qErr == nil {
					for rows.Next() {
						var iu string
						if rows.Scan(&iu) == nil {
							existingImageURLs = append(existingImageURLs, iu)
						}
					}
					_ = rows.Close()
				}
			}
			return &gistImportResult{
				Status:            "already_imported",
				Version:           verStr,
				ImportedStoryID:   existingID,
				ImportedVersionID: existingVersionID,
				Title:             existingTitle,
				Theme:             payload.Theme,
				Description:       payload.Description,
				RawText:           strings.Join(rawSentences, "\n"),
				Sentences:         rawSentences,
				SentenceCount:     len(rawSentences),
				AnimationURL:      existingAnimURL,
				ImageURLs:         existingImageURLs,
				Source:            sourceUsed,
			}, nil
		}
	}

	imgLinks := payload.ImageURLs
	if len(imgLinks) == 0 {
		imgLinks = payload.Images
	}

	theme := strings.TrimSpace(payload.Theme)
	if theme == "" {
		theme = "我的世界·故事模式"
	}

	var maxVer int
	_ = s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version_number), 0) FROM story_versions WHERE story_id = ?", storyID).Scan(&maxVer)
	nextVer := maxVer + 1
	versionID := fmt.Sprintf("%s_v%d", storyID, nextVer)

	imgClient := s.newSSRFProtectedImageClient()
	var customImages []content.CustomImageUpload
	for idx, rawImg := range imgLinks {
		u := strings.TrimSpace(rawImg)
		if u == "" {
			continue
		}
		fname := fmt.Sprintf("chapter_%02d.webp", idx)
		var b64Data string

		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "data:") && gistSiblingFiles != nil {
			if sib, exists := gistSiblingFiles[u]; exists {
				if strings.TrimSpace(sib.RawURL) != "" {
					u = strings.TrimSpace(sib.RawURL)
				} else if strings.TrimSpace(sib.Content) != "" {
					u = strings.TrimSpace(sib.Content)
				}
			}
		}

		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			parsedU, err := validatePublicRemoteImageURL(u, s.allowLoopbackImageFetch)
			if err != nil {
				continue
			}
			if ext := strings.ToLower(filepath.Ext(parsedU.Path)); ext != "" {
				fname = fmt.Sprintf("chapter_%02d%s", idx, ext)
			}
			imgReq, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedU.String(), nil)
			if err != nil {
				continue
			}
			imgReq.Header.Set("Cache-Control", "no-cache, no-store, max-age=0")
			imgReq.Header.Set("User-Agent", "reader-gist-sync/1.0")
			imgResp, err := imgClient.Do(imgReq)
			if err != nil {
				continue
			}
			imgBytes, readErr := io.ReadAll(io.LimitReader(imgResp.Body, 8*1024*1024))
			_ = imgResp.Body.Close()
			if readErr != nil || imgResp.StatusCode != http.StatusOK || len(imgBytes) == 0 {
				continue
			}
			b64Data = base64.StdEncoding.EncodeToString(imgBytes)
		} else if strings.HasPrefix(u, "data:") {
			if commaIdx := strings.Index(u, ","); commaIdx >= 0 {
				b64Data = strings.TrimSpace(u[commaIdx+1:])
			}
		} else {
			b64Data = u
		}

		if b64Data != "" {
			customImages = append(customImages, content.CustomImageUpload{
				Filename:   fname,
				DataBase64: b64Data,
			})
		}
	}

	// 写入图片和故事数据前再次复查外接盘挂载标记与磁盘空间（防下载期间掉盘误写闪存）
	if statusCode, storageErr := s.verifyStorageWritable(); storageErr != nil {
		return nil, &httpStatusError{statusCode: statusCode, err: storageErr}
	}

	var imageURLs []string
	if len(customImages) > 0 {
		var saveErr error
		imageURLs, saveErr = content.SaveUserPreparedImages(s.cfg.MediaDir(), storyID, versionID, customImages, len(rawSentences))
		if saveErr != nil {
			return nil, saveErr
		}
	}
	if len(imageURLs) == 0 {
		imageURLs, _ = content.RenderStoryIllustrations(
			ctx, s.cfg.MediaDir(), storyID, versionID, theme, rawSentences,
			"", "", "",
		)
	}

	var sentences []content.SentenceManifest
	for idx, sText := range rawSentences {
		sentID := fmt.Sprintf("%s_s%02d", versionID, idx)
		imgURL := ""
		if idx < len(imageURLs) {
			imgURL = imageURLs[idx]
		}
		sentences = append(sentences, content.SentenceManifest{
			ID:            sentID,
			SequenceIndex: idx,
			Text:          sText,
			ImageURL:      imgURL,
			Tokens:        content.SegmentAndAnnotate(sentID, sText),
		})
	}

	coverURL := ""
	if len(imageURLs) > 0 {
		coverURL = imageURLs[0]
	}

	desc := strings.TrimSpace(payload.Description)
	if desc == "" {
		desc = fmt.Sprintf("主题【%s】· Gist 同步故事 %s", theme, versionTag)
	} else if !strings.Contains(desc, versionTag) {
		desc = desc + " " + versionTag
	}

	manifest := &content.StoryManifest{
		StoryID:       storyID,
		Title:         strings.TrimSpace(payload.Title),
		Theme:         theme,
		Description:   desc,
		CoverURL:      coverURL,
		VersionID:     versionID,
		VersionNumber: nextVer,
		Status:        "published",
		Sentences:     sentences,
	}

	if statusCode, storageErr := s.verifyStorageWritable(); storageErr != nil {
		return nil, &httpStatusError{statusCode: statusCode, err: storageErr}
	}
	if err := s.importer.SaveManifest(ctx, manifest); err != nil {
		return nil, err
	}

	remoteAnimURL := strings.TrimSpace(payload.AnimationURL)
	if payload.RewardAnimation != nil && strings.TrimSpace(payload.RewardAnimation.URL) != "" {
		remoteAnimURL = strings.TrimSpace(payload.RewardAnimation.URL)
	}
	localAnimURL := ""
	if remoteAnimURL != "" {
		mediaURL, targetName, targetPath := s.downloadRemoteStoryAnimation(ctx, imgClient, manifest.StoryID, remoteAnimURL)
		if mediaURL != "" {
			if _, dbErr := s.db.ExecContext(ctx, "UPDATE stories SET animation_url = ? WHERE id = ?", mediaURL, manifest.StoryID); dbErr != nil {
				_ = os.Remove(targetPath)
			} else {
				localAnimURL = mediaURL
				removeOtherRewardAnimations(filepath.Join(s.cfg.MediaDir(), "stories", manifest.StoryID), targetName)
			}
		}
	}

	go s.precacheStoryVersionAudio(manifest.VersionID)

	imgCount := len(customImages)
	if imgCount == 0 {
		imgCount = len(imageURLs)
	}
	linesRatio := fmt.Sprintf("共 %d 行", len(rawSentences))
	if imgCount > 0 {
		avgLines := float64(len(rawSentences)) / float64(imgCount)
		linesRatio = fmt.Sprintf("%d 张图平均分配 %d 行（每张图约 %.1f 行）", imgCount, len(rawSentences), avgLines)
	}

	return &gistImportResult{
		Status:            "imported",
		Version:           verStr,
		ImportedStoryID:   manifest.StoryID,
		ImportedVersionID: manifest.VersionID,
		VersionNumber:     manifest.VersionNumber,
		Title:             manifest.Title,
		Theme:             manifest.Theme,
		Description:       manifest.Description,
		RawText:           strings.Join(rawSentences, "\n"),
		Sentences:         rawSentences,
		SentenceCount:     len(manifest.Sentences),
		ImageCount:        imgCount,
		LinesPerImage:     linesRatio,
		CoverURL:          coverURL,
		AnimationURL:      localAnimURL,
		ImageURLs:         imageURLs,
		Source:            sourceUsed,
	}, nil
}

func (s *Server) fetchAndImportLatestStoryFromGist() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, _ = s.pullAndImportStoryFromGist(ctx, false)
}

func (s *Server) handleAdminGistPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if statusCode, storageErr := s.verifyStorageWritable(); storageErr != nil {
		writeError(w, statusCode, storageErr.Error())
		return
	}
	var req struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	res, err := s.pullAndImportStoryFromGist(ctx, req.Force)
	if err != nil {
		var statusErr *httpStatusError
		if errors.As(err, &statusErr) {
			writeError(w, statusErr.statusCode, statusErr.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleAdminImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if statusCode, storageErr := s.verifyStorageWritable(); storageErr != nil {
		writeError(w, statusCode, storageErr.Error())
		return
	}

	maxBytes := s.maxUploadBytes()
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	contentType := r.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/json") {
		var m content.StoryManifest
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			if isMaxBytesErr(err) {
				writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("manifest exceeds maximum upload limit (%d MB)", s.cfg.UploadMaxMB))
				return
			}
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid manifest json: %v", err))
			return
		}
		if err := s.importer.SaveManifest(r.Context(), &m); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		go s.precacheStoryVersionAudio(m.VersionID)
		writeJSON(w, http.StatusOK, map[string]any{
			"imported_story_id":   m.StoryID,
			"imported_version_id": m.VersionID,
			"sentence_count":      len(m.Sentences),
		})
		return
	}

	memLimit := int64(16 << 20)
	if maxBytes < memLimit {
		memLimit = maxBytes
	}
	if err := r.ParseMultipartForm(memLimit); err != nil {
		if isMaxBytesErr(err) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("package exceeds maximum upload limit (%d MB)", s.cfg.UploadMaxMB))
			return
		}
		writeError(w, http.StatusBadRequest, fmt.Sprintf("parse multipart form: %v", err))
		return
	}
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}
	file, _, err := r.FormFile("package")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing 'package' file field")
		return
	}
	defer func() { _ = file.Close() }()

	tmpFile, err := os.CreateTemp(s.cfg.TmpDir(), "import_*.zip")
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("create temp file in %s: %v", s.cfg.TmpDir(), err))
		return
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := io.Copy(tmpFile, file); err != nil {
		_ = tmpFile.Close()
		if isMaxBytesErr(err) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("package exceeds maximum upload limit (%d MB)", s.cfg.UploadMaxMB))
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("write temp zip: %v", err))
		return
	}
	_ = tmpFile.Close()

	manifest, err := s.importer.ImportZip(r.Context(), tmpPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	go s.precacheStoryVersionAudio(manifest.VersionID)

	writeJSON(w, http.StatusOK, map[string]any{
		"imported_story_id":   manifest.StoryID,
		"imported_version_id": manifest.VersionID,
		"sentence_count":      len(manifest.Sentences),
	})
}

func (s *Server) handleAdminExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	format := r.URL.Query().Get("format")
	if format == "sqlite" {
		backupPath, err := s.jobs.RunBackup(r.Context(), time.Now())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(backupPath)))
		http.ServeFile(w, r, backupPath)
		return
	}

	exportData, err := s.buildJSONExport(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, exportData)
}

func (s *Server) handleAdminEventValidity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPatch {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/events/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) != 2 || parts[1] != "validity" || parts[0] == "" {
		writeError(w, http.StatusNotFound, "expected /api/v1/admin/events/:id/validity")
		return
	}
	eventID := parts[0]

	var req struct {
		IsValid bool   `json:"is_valid"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := s.events.MarkCharacterEventValidity(r.Context(), eventID, req.IsValid, req.Reason); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event_id": eventID, "is_valid": req.IsValid})
}

func (s *Server) buildJSONExport(ctx context.Context) (map[string]any, error) {
	out := map[string]any{
		"exported_at": time.Now().UTC().Format(time.RFC3339),
	}
	childrenRows, err := s.db.QueryContext(ctx, "SELECT id, name, COALESCE(avatar, ''), created_at FROM children")
	if err != nil {
		return nil, err
	}
	defer func() { _ = childrenRows.Close() }()
	var children []map[string]any
	for childrenRows.Next() {
		var id, name, avatar, createdAt string
		if err := childrenRows.Scan(&id, &name, &avatar, &createdAt); err != nil {
			return nil, err
		}
		children = append(children, map[string]any{"id": id, "name": name, "avatar": avatar, "created_at": createdAt})
	}
	out["children"] = children

	sessRows, err := s.db.QueryContext(ctx, "SELECT id, child_id, story_version_id, mode, status, COALESCE(end_reason, ''), current_sentence_index, started_at, COALESCE(ended_at, '') FROM reading_sessions")
	if err != nil {
		return nil, err
	}
	defer func() { _ = sessRows.Close() }()
	var sessions []map[string]any
	for sessRows.Next() {
		var id, childID, svID, mode, status, endReason, startedAt, endedAt string
		var idx int
		if err := sessRows.Scan(&id, &childID, &svID, &mode, &status, &endReason, &idx, &startedAt, &endedAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, map[string]any{
			"id": id, "child_id": childID, "story_version_id": svID, "mode": mode,
			"status": status, "end_reason": endReason, "current_sentence_index": idx,
			"started_at": startedAt, "ended_at": endedAt,
		})
	}
	out["reading_sessions"] = sessions

	ceRows, err := s.db.QueryContext(ctx, "SELECT id, sentence_view_id, token_id, character, pinyin_played, clicked_at, is_valid, COALESCE(invalid_reason, '') FROM character_events")
	if err != nil {
		return nil, err
	}
	defer func() { _ = ceRows.Close() }()
	var charEvents []map[string]any
	for ceRows.Next() {
		var id, svID, tokID, ch, py, clickedAt, reason string
		var valid int
		if err := ceRows.Scan(&id, &svID, &tokID, &ch, &py, &clickedAt, &valid, &reason); err != nil {
			return nil, err
		}
		charEvents = append(charEvents, map[string]any{
			"id": id, "sentence_view_id": svID, "token_id": tokID, "character": ch,
			"pinyin_played": py, "clicked_at": clickedAt, "is_valid": valid == 1, "invalid_reason": reason,
		})
	}
	out["character_events"] = charEvents
	return out, nil
}

func (s *Server) handleAdminUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	mirror := r.URL.Query().Get("mirror_url")
	res, err := s.updater.CheckUpdate(r.Context(), mirror)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleAdminUpdateApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		MirrorURL string `json:"mirror_url"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)

	res, err := s.updater.ApplyUpdate(r.Context(), req.MirrorURL)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if res.Status == "restarting" {
		s.updater.SpawnRestartAndRollbackWatchdog(res.ExePath, res.NewVersion)
	}
}

func isPunctuationChar(ch string) bool {
	switch ch {
	case "，", "。", "！", "？", "、", "：", "；", "“", "”", "‘", "’", "（", "）", ",", ".", "!", "?", ":", ";":
		return true
	}
	return false
}

func (s *Server) isRealMediaAudio(audioURL string) bool {
	u := strings.TrimSpace(audioURL)
	if u == "" || !strings.HasPrefix(u, "/media/") {
		return false
	}
	rel := strings.TrimPrefix(u, "/media/")
	if strings.Contains(rel, "..") {
		return false
	}
	full := filepath.Join(s.cfg.MediaDir(), filepath.FromSlash(rel))
	st, err := os.Stat(full)
	if err != nil || st.IsDir() || st.Size() <= 512 {
		return false
	}
	return true
}

func (s *Server) ensureTTSAudioCached(ctx context.Context, text string) (string, string, error) {
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > 200 {
		return "", "", errors.New("invalid text")
	}

	sum := sha256.Sum256([]byte(text))
	cacheName := "tts_" + hex.EncodeToString(sum[:8]) + ".mp3"
	audioDir := filepath.Join(s.cfg.MediaDir(), "audio")
	_ = os.MkdirAll(audioDir, 0o755)
	cachePath := filepath.Join(audioDir, cacheName)
	mediaURL := "/media/audio/" + cacheName

	if st, err := os.Stat(cachePath); err == nil && st.Size() > 512 {
		return mediaURL, cachePath, nil
	}

	escaped := url.QueryEscape(text)
	candidates := []string{
		"https://dict.youdao.com/dictvoice?le=zh&audio=" + escaped,
		"https://fanyi.baidu.com/gettts?lan=zh&spd=4&source=web&text=" + escaped,
	}

	reqCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	client := &http.Client{Timeout: 2500 * time.Millisecond}
	ch := make(chan []byte, len(candidates))

	for _, u := range candidates {
		go func(targetURL string) {
			req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, targetURL, nil)
			if err != nil {
				ch <- nil
				return
			}
			req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; x86_64) AppleWebKit/537.36")
			resp, err := client.Do(req)
			if err != nil {
				ch <- nil
				return
			}
			data, readErr := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
			_ = resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK && len(data) > 512 {
				ch <- data
				return
			}
			ch <- nil
		}(u)
	}

	for i := 0; i < len(candidates); i++ {
		data := <-ch
		if len(data) > 512 {
			cancel()
			_ = os.WriteFile(cachePath, data, 0o644)
			return mediaURL, cachePath, nil
		}
	}
	return "", "", errors.New("tts upstream unavailable")
}

func (s *Server) precacheStoryVersionAudio(versionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sentRows, err := s.db.QueryContext(ctx, `
		SELECT id, text, COALESCE(audio_url, '')
		FROM story_sentences
		WHERE version_id = ?
		ORDER BY sequence_index ASC
	`, versionID)
	if err != nil {
		return
	}
	type sentItem struct {
		id, text, audioURL string
	}
	var sents []sentItem
	for sentRows.Next() {
		var it sentItem
		if err := sentRows.Scan(&it.id, &it.text, &it.audioURL); err == nil {
			sents = append(sents, it)
		}
	}
	_ = sentRows.Close()

	for _, st := range sents {
		if !s.isRealMediaAudio(st.audioURL) {
			if mediaURL, _, err := s.ensureTTSAudioCached(ctx, st.text); err == nil {
				_, _ = s.db.ExecContext(ctx, "UPDATE story_sentences SET audio_url = ? WHERE id = ?", mediaURL, st.id)
			}
		}
	}

	charRows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT st.character
		FROM sentence_tokens st
		JOIN story_sentences ss ON st.sentence_id = ss.id
		WHERE ss.version_id = ?
	`, versionID)
	if err != nil {
		return
	}
	var chars []string
	for charRows.Next() {
		var ch string
		if err := charRows.Scan(&ch); err == nil && !isPunctuationChar(ch) {
			chars = append(chars, ch)
		}
	}
	_ = charRows.Close()

	for _, ch := range chars {
		if mediaURL, _, err := s.ensureTTSAudioCached(ctx, ch); err == nil {
			_, _ = s.db.ExecContext(ctx, `
				UPDATE sentence_tokens
				SET audio_url = ?
				WHERE character = ? AND sentence_id IN (SELECT id FROM story_sentences WHERE version_id = ?)
			`, mediaURL, ch, versionID)
		}
	}
}

func (s *Server) precacheAllPublishedStories() {
	time.Sleep(2 * time.Second)
	rows, err := s.db.Query(`
		SELECT sv.id
		FROM story_versions sv
		JOIN stories s ON sv.story_id = s.id
		WHERE s.status = 'published' AND sv.status = 'published'
		ORDER BY sv.created_at DESC
	`)
	if err != nil {
		return
	}
	var vids []string
	for rows.Next() {
		var vid string
		if err := rows.Scan(&vid); err == nil {
			vids = append(vids, vid)
		}
	}
	_ = rows.Close()

	for _, vid := range vids {
		s.precacheStoryVersionAudio(vid)
	}
}

func (s *Server) handleTTS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method notAllowed")
		return
	}
	text := strings.TrimSpace(r.URL.Query().Get("text"))
	if text == "" || len([]rune(text)) > 200 {
		writeError(w, http.StatusBadRequest, "valid text required")
		return
	}

	_, cachePath, err := s.ensureTTSAudioCached(r.Context(), text)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, cachePath)
}

func countReadableChars(text string) int {
	n := 0
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			n++
		}
	}
	return n
}

// computeEffectivePageReadingMs 按自定义规则计算单页有效阅读时长（毫秒）：
// - 非标点字符每字基准 1 秒，标点不计时间
// - 被点击过的字按“连点前两次加时间”：每个字前两次点击每次 +6 秒（第 1 次点击变为 7 秒，第 2 次变为 13 秒，第 3 次起不再加时）
// - 扣除本页整句朗读音频播放时长后得到净阅读停留时长
// - 净停留 > 基准上限：按基准上限计；净停留 < 基准上限一半：计 0
func computeEffectivePageReadingMs(dwellMs, sentenceAudioMs int64, charCount, cappedCharClicks int) int64 {
	targetMs := int64(charCount)*1000 + int64(cappedCharClicks)*6000
	if targetMs <= 0 {
		return 0
	}
	readingStayMs := dwellMs - sentenceAudioMs
	if readingStayMs <= 0 {
		return 0
	}
	if readingStayMs*2 < targetMs {
		return 0
	}
	if readingStayMs > targetMs {
		return targetMs
	}
	return readingStayMs
}

func (s *Server) computeSessionReadingMs(ctx context.Context, sessionID string) (int64, int) {
	cappedClicksByView := make(map[string]int)
	ceRows, ceErr := s.db.QueryContext(ctx, `
		SELECT ce.sentence_view_id, ce.token_id, COUNT(*)
		FROM character_events ce
		JOIN sentence_views svw ON svw.id = ce.sentence_view_id
		WHERE svw.session_id = ? AND ce.is_valid = 1
		GROUP BY ce.sentence_view_id, ce.token_id
	`, sessionID)
	if ceErr == nil {
		for ceRows.Next() {
			var svID, tokID string
			var cnt int
			if err := ceRows.Scan(&svID, &tokID, &cnt); err != nil {
				continue
			}
			if cnt > 2 {
				cnt = 2
			}
			cappedClicksByView[svID] += cnt
		}
		_ = ceRows.Close()
	}

	svRows, svErr := s.db.QueryContext(ctx, `
		SELECT svw.id,
		       svw.dwell_time_ms,
		       COALESCE(ss.text, ''),
		       COALESCE((
		           SELECT SUM(CASE WHEN ae.play_duration_ms > 0 THEN ae.play_duration_ms ELSE 0 END)
		           FROM audio_events ae
		           WHERE ae.sentence_view_id = svw.id AND ae.audio_type = 'sentence'
		       ), 0) AS sentence_audio_ms
		FROM sentence_views svw
		LEFT JOIN story_sentences ss ON ss.id = svw.sentence_id
		WHERE svw.session_id = ?
	`, sessionID)
	if svErr != nil {
		return 0, 0
	}
	defer func() { _ = svRows.Close() }()

	var totalEffectiveMs int64
	viewCount := 0
	for svRows.Next() {
		var svID, text string
		var dwellMs, sentenceAudioMs int64
		if err := svRows.Scan(&svID, &dwellMs, &text, &sentenceAudioMs); err != nil {
			continue
		}
		viewCount++
		charCount := countReadableChars(text)
		cappedClicks := cappedClicksByView[svID]
		totalEffectiveMs += computeEffectivePageReadingMs(dwellMs, sentenceAudioMs, charCount, cappedClicks)
	}
	return totalEffectiveMs, viewCount
}

package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"reader/server/internal/config"
)

type Runner struct {
	db  *sql.DB
	cfg *config.Config
}

func NewRunner(db *sql.DB, cfg *config.Config) *Runner {
	return &Runner{db: db, cfg: cfg}
}

func (r *Runner) Start(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	var lastBackupDate string
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			_, _ = r.ExpireStaleSessions(ctx, now)
			hhmm := now.Format("15:04")
			today := now.Format("2006-01-02")
			if hhmm == r.cfg.Backup.Time && lastBackupDate != today {
				if _, err := r.RunBackup(ctx, now); err == nil {
					lastBackupDate = today
					_ = r.CleanupOldBackups()
				}
			}
		}
	}
}

// ExpireStaleSessions marks active sessions idle for longer than SessionTimeoutMinutes as abandoned (end_reason = 'timeout').
func (r *Runner) ExpireStaleSessions(ctx context.Context, now time.Time) (int64, error) {
	cutoff := now.Add(-time.Duration(r.cfg.SessionTimeoutMinutes) * time.Minute).UTC().Format(time.RFC3339)
	nowStr := now.UTC().Format(time.RFC3339)

	res, err := r.db.ExecContext(ctx, `
		UPDATE reading_sessions
		SET status = 'abandoned',
		    end_reason = 'timeout',
		    ended_at = ?
		WHERE status = 'active'
		  AND started_at < ?
		  AND NOT EXISTS (
		      SELECT 1 FROM sentence_views sv
		      WHERE sv.session_id = reading_sessions.id
		        AND sv.received_at >= ?
		  )
	`, nowStr, cutoff, cutoff)
	if err != nil {
		return 0, fmt.Errorf("expire stale sessions: %w", err)
	}
	return res.RowsAffected()
}

// RunBackup uses SQLite VACUUM INTO to create a consistent online backup on the external disk.
func (r *Runner) RunBackup(ctx context.Context, now time.Time) (string, error) {
	if err := os.MkdirAll(r.cfg.BackupDir(), 0o755); err != nil {
		return "", err
	}
	filename := fmt.Sprintf("reader_backup_%s.db", now.Format("20060102_150405"))
	dest := filepath.Join(r.cfg.BackupDir(), filename)
	_ = os.Remove(dest)

	if _, err := r.db.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return "", fmt.Errorf("vacuum into %s: %w", dest, err)
	}
	return dest, nil
}

func (r *Runner) CleanupOldBackups() error {
	entries, err := os.ReadDir(r.cfg.BackupDir())
	if err != nil {
		return err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "reader_backup_") && strings.HasSuffix(e.Name(), ".db") {
			files = append(files, filepath.Join(r.cfg.BackupDir(), e.Name()))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	keep := r.cfg.Backup.KeepDaily
	if keep <= 0 {
		keep = 7
	}
	for i := keep; i < len(files); i++ {
		_ = os.Remove(files[i])
	}
	return nil
}

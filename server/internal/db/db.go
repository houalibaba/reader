package db

import (
	"database/sql"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"reader/server/internal/migrations"

	_ "github.com/mattn/go-sqlite3"
)

func Open(dbPath string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000&_synchronous=NORMAL", dbPath)
	conn, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}
	// Limit connections to keep memory footprint well below 80MB on iStoreOS
	conn.SetMaxOpenConns(4)
	conn.SetMaxIdleConns(2)
	conn.SetConnMaxLifetime(30 * time.Minute)

	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA wal_autocheckpoint = 1000;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA foreign_keys = ON;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA temp_store = FILE;",
		"PRAGMA mmap_size = 0;",     // Disable mmap to avoid virtual memory spikes on 120MiB router limit
		"PRAGMA cache_size = -8000;", // ~8MB page cache cap (from v0.1.0 spec)
	}
	for _, p := range pragmas {
		if _, err := conn.Exec(p); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("exec pragma %q: %w", p, err)
		}
	}

	var checkRes string
	if err := conn.QueryRow("PRAGMA quick_check;").Scan(&checkRes); err != nil || checkRes != "ok" {
		_ = conn.Close()
		return nil, fmt.Errorf("sqlite quick_check failed (%v: %s)", err, checkRes)
	}

	if err := Migrate(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("migrate sqlite: %w", err)
	}
	return conn, nil
}

func Migrate(conn *sql.DB) error {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		sqlBytes, err := migrations.FS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := conn.Exec(string(sqlBytes)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}

	// 幂等补齐 stories.animation_url 列（支持每篇故事 1 对 1 专属奖励动画）
	if err := ensureColumn(conn, "stories", "animation_url", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("ensure stories.animation_url: %w", err)
	}
	return nil
}

func ensureColumn(conn *sql.DB, tableName, colName, colDef string) error {
	rows, err := conn.Query(fmt.Sprintf("PRAGMA table_info(%s);", tableName))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil {
			if strings.EqualFold(name, colName) {
				return nil
			}
		}
	}
	_, err = conn.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s;", tableName, colName, colDef))
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
		return nil
	}
	return err
}

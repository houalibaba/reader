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
		"PRAGMA foreign_keys = ON;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA temp_store = FILE;",
		"PRAGMA cache_size = -4000;", // ~4MB page cache limit
	}
	for _, p := range pragmas {
		if _, err := conn.Exec(p); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("exec pragma %q: %w", p, err)
		}
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
	return nil
}

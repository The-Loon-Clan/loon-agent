package storage

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// DB wraps *sql.DB with a pin on the connection pool. modernc's SQLite
// driver is safe for concurrent reads but a single writer avoids "database
// is locked" on long PAR2/encoding tasks that hold a transaction open
// while other goroutines try to write. One writer, a handful of readers.
type DB struct {
	*sql.DB
}

//go:embed migrations/*.sql
var migrationsFS embed.FS

// OpenDB creates (or opens) the agent's local SQLite file and runs any
// migrations that haven't been applied yet. Migrations are numbered
// SQL files in storage/migrations/; filenames like "001_groups.sql"
// are applied in ascending order, tracked in the schema_migrations table.
func OpenDB(path string) (*DB, error) {
	if path == "" {
		return nil, fmt.Errorf("db path is empty")
	}
	// _pragma params configure the connection on open. WAL gives us
	// concurrent readers with one writer (fits the agent's pattern of
	// many progress updates + occasional UI queries). foreign_keys=on
	// so ON DELETE CASCADE actually runs.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)", filepath.ToSlash(path))
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Single writer — see struct comment. Readers fall through to
	// SetMaxOpenConns > 1 once we have a concurrent read path.
	sqldb.SetMaxOpenConns(4)
	sqldb.SetMaxIdleConns(2)

	db := &DB{DB: sqldb}
	if err := db.migrate(); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

func (db *DB) migrate() error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return err
	}

	applied := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		applied[n] = true
	}
	rows.Close()

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(name) VALUES(?)`, name); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		log.Printf("db: applied migration %s", name)
	}
	return nil
}

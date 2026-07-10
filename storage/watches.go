package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// WatchFolder is a filesystem path the offline pipeline monitors for new
// torrent or completed-media files. GroupID is a pointer because FKs use
// ON DELETE SET NULL — a watch can outlive its group and wait for the
// operator to reassign it from the UI.
type WatchFolder struct {
	ID        int64     `json:"id"`
	Path      string    `json:"path"`
	GroupID   *int64    `json:"group_id,omitempty"`
	GroupName string    `json:"group_name,omitempty"` // populated by List joins
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var ErrWatchNotFound = errors.New("watch folder not found")

// validateWatch normalises the path so "C:\data\anime\" and
// "C:/data/anime" don't create two rows that point at the same directory.
// Requires an absolute path because polling resolves relative paths
// against the agent's CWD, which is wrong in a Docker container.
func validateWatch(w *WatchFolder) error {
	w.Path = strings.TrimSpace(w.Path)
	if w.Path == "" {
		return errors.New("path is required")
	}
	if !filepath.IsAbs(w.Path) {
		return fmt.Errorf("path must be absolute (%q is relative)", w.Path)
	}
	w.Path = filepath.Clean(w.Path)
	return nil
}

func (db *DB) CreateWatch(w *WatchFolder) error {
	if err := validateWatch(w); err != nil {
		return err
	}
	now := time.Now().UTC()
	res, err := db.Exec(`
		INSERT INTO watch_folders (path, group_id, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		w.Path, nullInt64(w.GroupID), boolToInt(w.Enabled), now, now)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	w.ID = id
	w.CreatedAt = now
	w.UpdatedAt = now
	return nil
}

func (db *DB) UpdateWatch(w *WatchFolder) error {
	if w.ID == 0 {
		return errors.New("update: id is required")
	}
	if err := validateWatch(w); err != nil {
		return err
	}
	now := time.Now().UTC()
	res, err := db.Exec(`
		UPDATE watch_folders SET
		  path = ?, group_id = ?, enabled = ?, updated_at = ?
		WHERE id = ?`,
		w.Path, nullInt64(w.GroupID), boolToInt(w.Enabled), now, w.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrWatchNotFound
	}
	w.UpdatedAt = now
	return nil
}

func (db *DB) DeleteWatch(id int64) error {
	res, err := db.Exec(`DELETE FROM watch_folders WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrWatchNotFound
	}
	return nil
}

// ListWatches returns all watch folders with their group name eagerly
// joined — the /watches UI needs both to render the row, and doing the
// LEFT JOIN here avoids N+1 queries when the list grows.
func (db *DB) ListWatches() ([]*WatchFolder, error) {
	rows, err := db.Query(`
		SELECT w.id, w.path, w.group_id, COALESCE(g.name, ''), w.enabled, w.created_at, w.updated_at
		FROM watch_folders w
		LEFT JOIN groups g ON g.id = w.group_id
		ORDER BY w.path ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*WatchFolder
	for rows.Next() {
		var (
			w       WatchFolder
			gid     sql.NullInt64
			enabled int
		)
		if err := rows.Scan(&w.ID, &w.Path, &gid, &w.GroupName, &enabled, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, err
		}
		if gid.Valid {
			v := gid.Int64
			w.GroupID = &v
		}
		w.Enabled = enabled != 0
		out = append(out, &w)
	}
	return out, rows.Err()
}

// ListActiveWatches is the query the watcher goroutine uses each tick — only
// enabled folders with a resolvable group. Separated from ListWatches so
// the UI still surfaces disabled/unassigned rows for the operator to fix.
func (db *DB) ListActiveWatches() ([]*WatchFolder, error) {
	rows, err := db.Query(`
		SELECT w.id, w.path, w.group_id, g.name, w.enabled, w.created_at, w.updated_at
		FROM watch_folders w
		JOIN groups g ON g.id = w.group_id
		WHERE w.enabled = 1
		ORDER BY w.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*WatchFolder
	for rows.Next() {
		var (
			w       WatchFolder
			gid     sql.NullInt64
			enabled int
		)
		if err := rows.Scan(&w.ID, &w.Path, &gid, &w.GroupName, &enabled, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, err
		}
		if gid.Valid {
			v := gid.Int64
			w.GroupID = &v
		}
		w.Enabled = enabled != 0
		out = append(out, &w)
	}
	return out, rows.Err()
}

func nullInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

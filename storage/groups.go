package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Group is a user-defined category of uploads. Drop-folder → group → newsgroups
// is the routing chain for standalone (offline) jobs; the indexer-polling
// path doesn't use groups today but the model is shared so a future site
// push ("here are our categories") can land without a schema churn.
type Group struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	Type           string    `json:"type"` // 'video' | 'manga' | 'music' | custom
	Newsgroups     []string  `json:"newsgroups"`
	Screenshots    *int      `json:"screenshots,omitempty"`     // count of sample captures; nil = inherit
	SampleSeconds  *int      `json:"sample_seconds,omitempty"`  // audio-only: duration per clip; nil = inherit
	Par2Redundancy *int      `json:"par2_redundancy,omitempty"` // nil = inherit global
	Obfuscate      *bool     `json:"obfuscate,omitempty"`       // nil = inherit global
	WatermarkText  string    `json:"watermark_text"`            // "" = no watermark
	// BannedExtensions replaces the hardcoded blocklist for this group
	// when non-empty; empty falls through to services.DefaultBlockedExtensions.
	BannedExtensions []string  `json:"banned_extensions"`
	Source           string    `json:"source"`  // 'local' or 'site'
	Version          int       `json:"version"` // bumped by DB trigger on UPDATE
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// ErrGroupNotFound is returned by GetGroup/UpdateGroup/DeleteGroup when the
// id doesn't exist. Callers use this to turn misses into 404s without
// having to sniff for "sql: no rows" across the codebase.
var ErrGroupNotFound = errors.New("group not found")

// validateGroup centralises the rules that should hold regardless of path
// (UI form, future site-push, future API). Keeping them here rather than in
// handlers means the DB never sees an invalid row even if someone forgets.
func validateGroup(g *Group) error {
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" {
		return errors.New("name is required")
	}
	// A group with zero newsgroups can't post anything; rejecting this up
	// front is friendlier than letting the offline pipeline discover it
	// later and dump a cryptic NNTP error.
	clean := make([]string, 0, len(g.Newsgroups))
	for _, ng := range g.Newsgroups {
		ng = strings.TrimSpace(ng)
		if ng != "" {
			clean = append(clean, ng)
		}
	}
	if len(clean) == 0 {
		return errors.New("at least one newsgroup is required")
	}
	g.Newsgroups = clean
	// Type stays an open field (no enum check) so new sampling strategies
	// — audiobook, software, whatever — can be introduced by the site
	// admin without an agent migration. An unknown type means the
	// offline processor skips sampling and posts the raw files.
	g.Type = strings.TrimSpace(strings.ToLower(g.Type))
	if g.Type == "" {
		g.Type = "video"
	}
	// Banned-extensions: normalise to ".ext" lowercase so `.EXE`, `exe`,
	// and `.exe` all produce the same match at runtime. Empty strings are
	// dropped so the UI's freeform textarea is forgiving.
	bans := make([]string, 0, len(g.BannedExtensions))
	for _, ext := range g.BannedExtensions {
		ext = strings.TrimSpace(strings.ToLower(ext))
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		bans = append(bans, ext)
	}
	g.BannedExtensions = bans
	if g.Source == "" {
		g.Source = "local"
	}
	if g.Source != "local" && g.Source != "site" {
		return fmt.Errorf("invalid source %q", g.Source)
	}
	return nil
}

func (db *DB) CreateGroup(g *Group) error {
	if err := validateGroup(g); err != nil {
		return err
	}
	ngJSON, err := json.Marshal(g.Newsgroups)
	if err != nil {
		return err
	}
	banJSON, err := json.Marshal(g.BannedExtensions)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	res, err := db.Exec(`
		INSERT INTO groups (name, type, newsgroups_json, banned_extensions_json, screenshots, sample_seconds, par2_redundancy, obfuscate, watermark_text, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.Name, g.Type, string(ngJSON), string(banJSON),
		nullInt(g.Screenshots), nullInt(g.SampleSeconds),
		nullInt(g.Par2Redundancy), nullBool(g.Obfuscate),
		g.WatermarkText, g.Source, now, now)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	g.ID = id
	g.CreatedAt = now
	g.UpdatedAt = now
	return nil
}

func (db *DB) UpdateGroup(g *Group) error {
	if g.ID == 0 {
		return errors.New("update: id is required")
	}
	if err := validateGroup(g); err != nil {
		return err
	}
	ngJSON, _ := json.Marshal(g.Newsgroups)
	banJSON, _ := json.Marshal(g.BannedExtensions)
	now := time.Now().UTC()
	res, err := db.Exec(`
		UPDATE groups SET
		  name = ?,
		  type = ?,
		  newsgroups_json = ?,
		  banned_extensions_json = ?,
		  screenshots = ?,
		  sample_seconds = ?,
		  par2_redundancy = ?,
		  obfuscate = ?,
		  watermark_text = ?,
		  source = ?,
		  updated_at = ?
		WHERE id = ?`,
		g.Name, g.Type, string(ngJSON), string(banJSON),
		nullInt(g.Screenshots), nullInt(g.SampleSeconds),
		nullInt(g.Par2Redundancy), nullBool(g.Obfuscate),
		g.WatermarkText, g.Source, now, g.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrGroupNotFound
	}
	g.UpdatedAt = now
	return nil
}

func (db *DB) DeleteGroup(id int64) error {
	res, err := db.Exec(`DELETE FROM groups WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrGroupNotFound
	}
	return nil
}

// UpsertSiteGroup writes a group that came from the site's /api/agent/groups
// pull. Matches by name, source='site' — lets the same "anime" exist as a
// local-edited row AND a site-pushed row only if operators deliberately
// create both (the DB's UNIQUE(name) would block that; see below).
//
// If a row with the same name already exists and has source='local', we
// log and return nil without touching it: the local operator's override
// wins over an incoming site push until they explicitly delete it. This
// preserves local customisation when a site rolls out a new catalog.
//
// The incoming Group's Version is the site's version, which we store
// directly so the next since_version poll reflects the published value.
func (db *DB) UpsertSiteGroup(g *Group) (upserted bool, err error) {
	g.Source = "site"
	if err := validateGroup(g); err != nil {
		return false, err
	}
	ngJSON, _ := json.Marshal(g.Newsgroups)
	banJSON, _ := json.Marshal(g.BannedExtensions)

	// Check for a pre-existing row by name. If it's local, skip; if it's
	// site, we update; if nothing exists, we insert. Done as a read
	// followed by a targeted write rather than one big CONFLICT clause
	// because SQLite's ON CONFLICT handling can't easily express "leave
	// local rows alone" — the read lets us log the skip with context.
	var existingID int64
	var existingSource string
	err = db.QueryRow(`SELECT id, source FROM groups WHERE name = ?`, g.Name).Scan(&existingID, &existingSource)
	switch {
	case err == sql.ErrNoRows:
		// Insert fresh. Version is set explicitly because the auto-bump
		// trigger only fires on UPDATE.
		now := time.Now().UTC()
		res, ierr := db.Exec(`
			INSERT INTO groups (name, type, newsgroups_json, banned_extensions_json, screenshots, sample_seconds, par2_redundancy, obfuscate, watermark_text, source, version, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'site', ?, ?, ?)`,
			g.Name, g.Type, string(ngJSON), string(banJSON),
			nullInt(g.Screenshots), nullInt(g.SampleSeconds),
			nullInt(g.Par2Redundancy), nullBool(g.Obfuscate),
			g.WatermarkText, g.Version, now, now)
		if ierr != nil {
			return false, ierr
		}
		id, _ := res.LastInsertId()
		g.ID = id
		g.CreatedAt = now
		g.UpdatedAt = now
		return true, nil
	case err != nil:
		return false, err
	case existingSource == "local":
		// Don't clobber an operator override.
		return false, nil
	}

	// existingSource == "site" — update in place. We set version
	// explicitly from the site's value; the version-bump trigger has a
	// WHEN clause that skips when NEW.version == OLD.version, but if the
	// site lowered version (e.g. a rollback) we want the site's value to
	// win. UPDATE … SET version = ? bypasses the trigger's recursion
	// guard by matching exactly what we intend.
	now := time.Now().UTC()
	_, err = db.Exec(`
		UPDATE groups SET
		  type = ?, newsgroups_json = ?, banned_extensions_json = ?,
		  screenshots = ?, sample_seconds = ?, par2_redundancy = ?,
		  obfuscate = ?, watermark_text = ?, version = ?, updated_at = ?
		WHERE id = ?`,
		g.Type, string(ngJSON), string(banJSON),
		nullInt(g.Screenshots), nullInt(g.SampleSeconds),
		nullInt(g.Par2Redundancy), nullBool(g.Obfuscate),
		g.WatermarkText, g.Version, now, existingID)
	if err != nil {
		return false, err
	}
	g.ID = existingID
	g.UpdatedAt = now
	return true, nil
}

// ReconcileSiteGroups removes locally-stored source='site' rows whose
// names no longer appear in the site's catalog. Called after a full
// fetch (since_version=0) so stale entries don't stick around after the
// site admin deletes them. Incremental polls can't detect deletes on
// their own — the site has no tombstone mechanism — so we rely on boot-
// time reconciliation to catch up.
func (db *DB) ReconcileSiteGroups(liveNames map[string]bool) (removed int, err error) {
	rows, err := db.Query(`SELECT id, name FROM groups WHERE source = 'site'`)
	if err != nil {
		return 0, err
	}
	type toDelete struct {
		id   int64
		name string
	}
	var stale []toDelete
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return 0, err
		}
		if !liveNames[name] {
			stale = append(stale, toDelete{id, name})
		}
	}
	rows.Close()
	for _, d := range stale {
		if _, err := db.Exec(`DELETE FROM groups WHERE id = ?`, d.id); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func (db *DB) GetGroup(id int64) (*Group, error) {
	row := db.QueryRow(`
		SELECT id, name, type, newsgroups_json, banned_extensions_json, screenshots, sample_seconds, par2_redundancy, obfuscate, watermark_text, source, version, created_at, updated_at
		FROM groups WHERE id = ?`, id)
	return scanGroup(row.Scan)
}

func (db *DB) ListGroups() ([]*Group, error) {
	rows, err := db.Query(`
		SELECT id, name, type, newsgroups_json, banned_extensions_json, screenshots, sample_seconds, par2_redundancy, obfuscate, watermark_text, source, version, created_at, updated_at
		FROM groups ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Group
	for rows.Next() {
		g, err := scanGroup(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// scanGroup is shared by the single-row and list queries so the column
// order stays in one place — easy to keep in sync if we add columns later.
func scanGroup(scan func(dest ...any) error) (*Group, error) {
	var (
		g          Group
		ngJSON     string
		banJSON    string
		ss, ssec   sql.NullInt64
		p2         sql.NullInt64
		ob         sql.NullBool
	)
	if err := scan(&g.ID, &g.Name, &g.Type, &ngJSON, &banJSON, &ss, &ssec, &p2, &ob, &g.WatermarkText, &g.Source, &g.Version, &g.CreatedAt, &g.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrGroupNotFound
		}
		return nil, err
	}
	if ngJSON != "" {
		_ = json.Unmarshal([]byte(ngJSON), &g.Newsgroups)
	}
	if banJSON != "" {
		_ = json.Unmarshal([]byte(banJSON), &g.BannedExtensions)
	}
	if ss.Valid {
		v := int(ss.Int64)
		g.Screenshots = &v
	}
	if ssec.Valid {
		v := int(ssec.Int64)
		g.SampleSeconds = &v
	}
	if p2.Valid {
		v := int(p2.Int64)
		g.Par2Redundancy = &v
	}
	if ob.Valid {
		v := ob.Bool
		g.Obfuscate = &v
	}
	return &g, nil
}

func nullInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullBool(p *bool) any {
	if p == nil {
		return nil
	}
	return *p
}

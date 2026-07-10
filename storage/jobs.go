package storage

import (
	"database/sql"
	"errors"
	"time"
)

// OfflineJob is one watch-folder-detected file flowing through the local
// upload pipeline. Separate from the site-driven "request" concept in
// main.go — offline jobs don't have a lock_id, boost count, or any
// coupling to the indexer site.
type OfflineJob struct {
	ID                    int64      `json:"id"`
	SourcePath            string     `json:"source_path"`
	Title                 string     `json:"title"`
	GroupID               *int64     `json:"group_id,omitempty"`
	GroupNameAtCreation   string     `json:"group_name_at_creation"`
	Status                string     `json:"status"`
	Phase                 string     `json:"phase"`
	Progress              float64    `json:"progress"`
	Error                 string     `json:"error,omitempty"`
	NzbPath               string     `json:"nzb_path,omitempty"`
	Password              string     `json:"password,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	StartedAt             *time.Time `json:"started_at,omitempty"`
	CompletedAt           *time.Time `json:"completed_at,omitempty"`
}

var ErrJobNotFound = errors.New("offline job not found")

// CreateOfflineJob inserts a new queued job. The source_path UNIQUE
// constraint means re-detection is a no-op — callers treat the unique
// violation as "we've already seen this file" rather than an error.
func (db *DB) CreateOfflineJob(j *OfflineJob) error {
	if j.Status == "" {
		j.Status = "queued"
	}
	if j.CreatedAt.IsZero() {
		j.CreatedAt = time.Now().UTC()
	}
	res, err := db.Exec(`
		INSERT INTO offline_jobs (source_path, title, group_id, group_name_at_creation, status, phase, progress, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		j.SourcePath, j.Title, nullInt64(j.GroupID), j.GroupNameAtCreation, j.Status, j.Phase, j.Progress, j.CreatedAt)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	j.ID = id
	return nil
}

// ClaimNextJob atomically transitions the oldest queued job to processing
// and returns it, or (nil, nil) if nothing was waiting. Uses an UPDATE
// with a subquery rather than a transaction because the single-writer
// pool guarantees a narrow window — but this still works correctly if we
// later widen it, since the UPDATE is atomic on SQLite.
func (db *DB) ClaimNextJob() (*OfflineJob, error) {
	now := time.Now().UTC()
	res, err := db.Exec(`
		UPDATE offline_jobs
		SET status = 'processing', started_at = ?, phase = 'starting'
		WHERE id = (
			SELECT id FROM offline_jobs
			WHERE status = 'queued'
			ORDER BY created_at ASC
			LIMIT 1
		)`, now)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, nil
	}
	// Fetch the row we just claimed. Ordering by started_at DESC is safe
	// because we just set it to `now` on this one row only.
	return db.queryOneJob(`WHERE status = 'processing' ORDER BY started_at DESC LIMIT 1`)
}

// SourcePathExists is the cheap check the watcher uses on every scan so
// we don't re-queue a file that already has a job. A unique-violation on
// insert would also catch it but at a higher cost.
func (db *DB) SourcePathExists(path string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM offline_jobs WHERE source_path = ?`, path).Scan(&n)
	return n > 0, err
}

func (db *DB) UpdateJobPhase(id int64, phase string, progress float64) error {
	_, err := db.Exec(`UPDATE offline_jobs SET phase = ?, progress = ? WHERE id = ?`, phase, progress, id)
	return err
}

func (db *DB) MarkJobCompleted(id int64, nzbPath, password string) error {
	now := time.Now().UTC()
	_, err := db.Exec(`
		UPDATE offline_jobs
		SET status = 'completed', phase = 'done', progress = 100,
		    nzb_path = ?, password = ?, completed_at = ?
		WHERE id = ?`, nzbPath, password, now, id)
	return err
}

func (db *DB) MarkJobFailed(id int64, errMsg string) error {
	now := time.Now().UTC()
	_, err := db.Exec(`
		UPDATE offline_jobs
		SET status = 'failed', error = ?, completed_at = ?
		WHERE id = ?`, errMsg, now, id)
	return err
}

// ResetQueuedJob moves a failed/completed job back to queued so the
// processor picks it up again. Clears the error + nzb_path so the UI
// doesn't show stale data while the retry is running.
func (db *DB) ResetQueuedJob(id int64) error {
	res, err := db.Exec(`
		UPDATE offline_jobs
		SET status = 'queued', phase = '', progress = 0,
		    error = NULL, nzb_path = NULL, started_at = NULL, completed_at = NULL
		WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrJobNotFound
	}
	return nil
}

// RequeueStuckJobs resets any 'processing' rows back to 'queued' on
// startup — the process that was working them has obviously crashed, and
// without this a restart would permanently strand those jobs.
func (db *DB) RequeueStuckJobs() (int64, error) {
	res, err := db.Exec(`
		UPDATE offline_jobs
		SET status = 'queued', phase = '', progress = 0, started_at = NULL
		WHERE status = 'processing'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (db *DB) DeleteJob(id int64) error {
	res, err := db.Exec(`DELETE FROM offline_jobs WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrJobNotFound
	}
	return nil
}

// CountJobsByStatus returns a map status→count for all offline_jobs.
// Used by the /events SSE feed to drive the sidebar's per-status badges
// without pulling every row. Cheap enough to run every SSE tick since
// it's a single GROUP BY over an indexed column.
func (db *DB) CountJobsByStatus() (map[string]int, error) {
	rows, err := db.Query(`SELECT status, COUNT(*) FROM offline_jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// ListOfflineJobs returns the most recent jobs, newest first. Capped
// because the UI renders them all in one page; old jobs aren't useful to
// show past that cap and an unbounded query is a footgun once the user
// has been running this for months.
func (db *DB) ListOfflineJobs(limit int) ([]*OfflineJob, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(`
		SELECT id, source_path, title, group_id, group_name_at_creation, status, phase, progress,
		       COALESCE(error, ''), COALESCE(nzb_path, ''), COALESCE(password, ''),
		       created_at, started_at, completed_at
		FROM offline_jobs
		ORDER BY created_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OfflineJob
	for rows.Next() {
		j, err := scanJob(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (db *DB) queryOneJob(where string) (*OfflineJob, error) {
	row := db.QueryRow(`
		SELECT id, source_path, title, group_id, group_name_at_creation, status, phase, progress,
		       COALESCE(error, ''), COALESCE(nzb_path, ''), COALESCE(password, ''),
		       created_at, started_at, completed_at
		FROM offline_jobs ` + where)
	return scanJob(row.Scan)
}

func scanJob(scan func(dest ...any) error) (*OfflineJob, error) {
	var (
		j              OfflineJob
		gid            sql.NullInt64
		startedAt, cmp sql.NullTime
	)
	if err := scan(&j.ID, &j.SourcePath, &j.Title, &gid, &j.GroupNameAtCreation, &j.Status, &j.Phase,
		&j.Progress, &j.Error, &j.NzbPath, &j.Password, &j.CreatedAt, &startedAt, &cmp); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	if gid.Valid {
		v := gid.Int64
		j.GroupID = &v
	}
	if startedAt.Valid {
		t := startedAt.Time
		j.StartedAt = &t
	}
	if cmp.Valid {
		t := cmp.Time
		j.CompletedAt = &t
	}
	return &j, nil
}

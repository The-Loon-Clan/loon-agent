package services

// Collection-mode scanner. Walks a configured on-disk root, batches
// the filenames against the site's /api/agent/title-match-bulk
// endpoint, and persists the enriched result set to a JSON file under
// the agent's data dir so reloads of the Collection page don't
// re-hit the API or re-walk the tree on every refresh.
//
// State model: one in-memory snapshot per Scanner instance, mirrored
// to disk after each successful scan. The local UI's handlers load
// the snapshot on read (cheap) and call Scan() on the explicit user
// action (expensive). No background loop — Collection is operator-
// driven, not request-driven like Mirror.
//
// File-type filter is video / archive extensions only. Subtitle and
// metadata sidecars aren't candidates for usenet upload by
// themselves; they ride along with the parent during upload (slice 4).

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ameNZB/usenet-pipeline/client"
)

// CollectionItem is one file the scanner found + (optionally) its
// site-supplied enrichment. The hit/miss boolean lets the UI render
// a "Match" badge vs an empty cell without inspecting AID directly,
// and lets a later "Upload" pass quickly filter to confirmed rows.
type CollectionItem struct {
	Path           string    `json:"path"`             // absolute path on disk
	RelPath        string    `json:"rel_path"`         // relative to scan root, for display
	Filename       string    `json:"filename"`         // basename only
	SizeBytes      int64     `json:"size_bytes"`
	ModTime        time.Time `json:"mod_time"`
	Ext            string    `json:"ext"`              // lowercase, without leading dot

	// Enrichment results — copied from the site's bulk response.
	Matched        bool   `json:"matched"`
	AID            int    `json:"aid,omitempty"`
	AnimeTitle     string `json:"anime_title,omitempty"`
	MalID          int    `json:"mal_id,omitempty"`
	AnilistID      int    `json:"anilist_id,omitempty"`
	Format         string `json:"format,omitempty"`
	CoverURL       string `json:"cover_url,omitempty"`
	ResolutionHint string `json:"resolution_hint,omitempty"`
	SourceHint     string `json:"source_hint,omitempty"`

	// Operator-edited overrides — written by the Collection edit
	// handler. Empty means "use the hint / leave unset". Persisted so
	// a later Upload pass picks the human's choice over the site
	// guess. Season + Episode are free-form strings ("01", "1-12",
	// "S2") rather than ints so the operator can express ranges or
	// special markers without us second-guessing the format.
	OverrideAID        int    `json:"override_aid,omitempty"`
	OverrideResolution string `json:"override_resolution,omitempty"`
	OverrideSource     string `json:"override_source,omitempty"`
	OverrideSeason     string `json:"override_season,omitempty"`
	OverrideEpisode    string `json:"override_episode,omitempty"`

	// Selected marks rows the operator has picked for the next
	// Upload pass. Stored on the snapshot so a reload (meta-refresh
	// during a long enrichment, e.g.) doesn't lose the selection.
	Selected bool `json:"selected,omitempty"`

	// UploadStatus + UploadDetail track per-row upload lifecycle.
	// Empty = not yet uploaded. "queued" = picked for upload, awaiting
	// the worker. Future states ("uploading", "done", "failed") get
	// set by the slice-5 upload backend; the Collection page renders
	// non-empty statuses in a Status section at the bottom.
	UploadStatus string `json:"upload_status,omitempty"`
	UploadDetail string `json:"upload_detail,omitempty"`
}

// CollectionSnapshot is the JSON shape persisted to disk. We keep
// the wrapper rather than a bare []CollectionItem so we can add
// scanner metadata (last-scanned-at, source root, version) without
// breaking the on-disk file.
type CollectionSnapshot struct {
	ScanRoot   string           `json:"scan_root"`
	ScannedAt  time.Time        `json:"scanned_at"`
	TotalCount int              `json:"total_count"`
	Items      []CollectionItem `json:"items"`
	// FolderOverrides hold per-directory metadata set once at the
	// folder level and cascaded to every file inside it. Keyed by
	// folder path relative to ScanRoot ("Highschool DxD/Season 1"
	// or "Highschool DxD"). Empty string is the root.
	//
	// Effective-value precedence for a given file:
	//   file's Override*  → folder's Override*  → site hint
	// So an operator sets AID once on the show folder, then
	// optionally overrides one file inside if needed.
	FolderOverrides map[string]FolderOverride `json:"folder_overrides,omitempty"`
}

// FolderOverride carries the four metadata fields that typically
// stay constant within a release folder. Episode is intentionally
// per-file — it varies per row — so it doesn't live here.
type FolderOverride struct {
	AID        int    `json:"aid,omitempty"`
	Season     string `json:"season,omitempty"`
	Resolution string `json:"resolution,omitempty"`
	Source     string `json:"source,omitempty"`
	// Selected marks the folder as a bulk-upload target: queueing
	// uploads picks up every file under this folder.
	Selected bool `json:"selected,omitempty"`
}

// CollectionScanner owns the lifecycle of a single Collection mode
// snapshot. Methods are goroutine-safe via the mu mutex — the local
// UI reads happen on the request thread and a Scan triggered by the
// operator runs on a goroutine; they must not race on snap.
type CollectionScanner struct {
	site        TitleMatchBulker
	storePath   string // JSON file we read/write
	mu          sync.RWMutex
	snap        *CollectionSnapshot
	scanning    bool
	lastScanErr string
}

// TitleMatchBulker is the only piece of the site client this scanner
// needs. Defined as an interface so tests can stub the network round-
// trip without standing up an HTTP server.
type TitleMatchBulker interface {
	TitleMatchBulk(titles []string) ([]client.TitleMatchResult, error)
}

// collectionVideoExts is the filter for what counts as a scan candidate.
// Subtitles + nfo + jpg sidecars are intentionally excluded — they ride
// along during the Upload pass (slice 4) but aren't independent rows
// in the UI. cbz/cbr included for manga collections.
var collectionVideoExts = map[string]bool{
	"mkv":  true,
	"mp4":  true,
	"avi":  true,
	"mov":  true,
	"wmv":  true,
	"flv":  true,
	"webm": true,
	"m4v":  true,
	"ts":   true,
	"m2ts": true,
	"cbz":  true,
	"cbr":  true,
}

// collectionBatchSize must stay <= the site's 500 cap (see
// indexer-site/web/handlers/agent_collection.go titleMatchBulkBatchCap).
// 200 keeps responses small enough that a slow link doesn't time out
// mid-scan and tracks comfortably under the site's per-batch ceiling.
const collectionBatchSize = 200

// NewCollectionScanner wires a scanner to its on-disk snapshot file.
// Existing snapshots are loaded immediately so an agent restart picks
// up where it left off without forcing the operator to re-scan.
//
// site may be nil — the scanner falls back to "no enrichment, just
// list" so the UI still works against a partially-configured agent
// (no AGENT_TOKEN, for example).
func NewCollectionScanner(site TitleMatchBulker, dataDir string) *CollectionScanner {
	cs := &CollectionScanner{
		site:      site,
		storePath: filepath.Join(dataDir, "collection.json"),
	}
	cs.loadSnapshot() // best-effort; absence isn't an error
	return cs
}

// Snapshot returns the current in-memory snapshot (or nil). Caller
// must NOT mutate the returned slice — that races with an active
// scan. Cheap copy is fine but typical render path treats it as
// read-only via html/template.
func (cs *CollectionScanner) Snapshot() *CollectionSnapshot {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.snap
}

// IsScanning reports whether a Scan goroutine is mid-flight. The
// local UI uses this to disable the Scan button + show a spinner so
// the operator doesn't double-click into a duplicate run.
func (cs *CollectionScanner) IsScanning() bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.scanning
}

// LastError returns the most recent scan error string, or empty if
// the last scan succeeded. Render in the page's flash slot.
func (cs *CollectionScanner) LastError() string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.lastScanErr
}

// Scan is the operator-triggered scan + enrich pass. It walks `root`
// for files matching collectionVideoExts, batches their filenames
// through the site's title-match-bulk endpoint (when configured),
// writes the snapshot to disk, and atomically swaps the in-memory
// snap so subsequent Snapshot() calls return the new state.
//
// Re-entrant: a second call while one is in flight returns
// "scan in progress" without starting another goroutine.
func (cs *CollectionScanner) Scan(ctx context.Context, root string) error {
	cs.mu.Lock()
	if cs.scanning {
		cs.mu.Unlock()
		return fmt.Errorf("scan already in progress")
	}
	cs.scanning = true
	cs.lastScanErr = ""
	cs.mu.Unlock()
	defer func() {
		cs.mu.Lock()
		cs.scanning = false
		cs.mu.Unlock()
	}()

	root = strings.TrimSpace(root)
	if root == "" {
		err := fmt.Errorf("scan root not configured (set COLLECTION_ROOT or fill it in via /config)")
		cs.setLastErr(err)
		return err
	}
	if _, err := os.Stat(root); err != nil {
		err = fmt.Errorf("scan root not accessible: %w", err)
		cs.setLastErr(err)
		return err
	}

	items, err := cs.walk(ctx, root)
	if err != nil {
		cs.setLastErr(err)
		return err
	}

	// Enrich in batches if a site client is wired. A failed batch
	// doesn't abort the scan — the affected rows just stay with
	// Matched=false and the user sees raw filenames.
	if cs.site != nil {
		cs.enrich(ctx, items)
	}

	snap := &CollectionSnapshot{
		ScanRoot:   root,
		ScannedAt:  time.Now().UTC(),
		TotalCount: len(items),
		Items:      items,
	}
	cs.mu.Lock()
	cs.snap = snap
	cs.mu.Unlock()
	if err := cs.persistSnapshot(snap); err != nil {
		cs.setLastErr(fmt.Errorf("persist snapshot: %w", err))
		return err
	}
	return nil
}

// walk visits root recursively and gathers candidate rows. Skips
// directories the user can't open + zero-byte files (incomplete
// transfers, .Apple metadata stubs).
func (cs *CollectionScanner) walk(ctx context.Context, root string) ([]CollectionItem, error) {
	var items []CollectionItem
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best-effort: skip unreadable subtrees, keep walking
		}
		if d.IsDir() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(d.Name()), "."))
		if !collectionVideoExts[ext] {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.Size() == 0 {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = d.Name()
		}
		items = append(items, CollectionItem{
			Path:      path,
			RelPath:   filepath.ToSlash(rel),
			Filename:  d.Name(),
			SizeBytes: info.Size(),
			ModTime:   info.ModTime(),
			Ext:       ext,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Stable order so successive scans diff cleanly in git / log diffs.
	sort.Slice(items, func(i, j int) bool { return items[i].RelPath < items[j].RelPath })
	return items, nil
}

// enrich batches filename strings against the site endpoint and
// merges the results back into the in-place slice. Failures are
// counted + surfaced via LastError but don't bubble up — a partial
// enrichment is better than no rows at all.
func (cs *CollectionScanner) enrich(ctx context.Context, items []CollectionItem) {
	var batchErrors int
	for start := 0; start < len(items); start += collectionBatchSize {
		if ctx.Err() != nil {
			return
		}
		end := start + collectionBatchSize
		if end > len(items) {
			end = len(items)
		}
		batch := items[start:end]
		titles := make([]string, len(batch))
		for i, it := range batch {
			titles[i] = it.Filename
		}
		results, err := cs.site.TitleMatchBulk(titles)
		if err != nil {
			batchErrors++
			continue
		}
		// 1:1 by position per the API contract; defensive bounds check
		// in case a future site version trims the slice.
		for i := range batch {
			if i >= len(results) {
				break
			}
			r := results[i]
			batch[i].Matched = r.Matched
			batch[i].AID = r.AID
			batch[i].AnimeTitle = r.AnimeTitle
			batch[i].MalID = r.MalID
			batch[i].AnilistID = r.AnilistID
			batch[i].Format = r.Format
			batch[i].CoverURL = r.CoverURL
			batch[i].ResolutionHint = r.ResolutionHint
			batch[i].SourceHint = r.SourceHint
		}
	}
	if batchErrors > 0 {
		cs.setLastErr(fmt.Errorf("enrichment: %d batch(es) failed (rows shown without metadata)", batchErrors))
	}
}

// UpdateOverrides applies operator edits to an existing snapshot row
// in-memory and persists. Called from the local UI's edit handler.
// Returns "not found" when the path no longer matches a snapshot row
// — a re-scan probably superseded the row.
//
// All five override fields are written every call (the form posts
// every cell of the row together). Empty strings clear the field so
// the upload pass falls back to the site's hint — that's the
// "revert to the auto value" affordance.
func (cs *CollectionScanner) UpdateOverrides(path string, aid int, resolution, source, season, episode string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.snap == nil {
		return fmt.Errorf("no snapshot to edit")
	}
	for i := range cs.snap.Items {
		if cs.snap.Items[i].Path != path {
			continue
		}
		cs.snap.Items[i].OverrideAID = aid
		cs.snap.Items[i].OverrideResolution = strings.TrimSpace(resolution)
		cs.snap.Items[i].OverrideSource = strings.TrimSpace(source)
		cs.snap.Items[i].OverrideSeason = strings.TrimSpace(season)
		cs.snap.Items[i].OverrideEpisode = strings.TrimSpace(episode)
		return cs.persistSnapshot(cs.snap)
	}
	return fmt.Errorf("path not in current snapshot")
}

// SetSelected toggles the Selected flag for a single row by path.
// The Collection page wires this to per-row checkboxes — each
// checkbox change POSTs here so a later browser reload doesn't lose
// the selection. Returns "not found" when the path no longer matches.
func (cs *CollectionScanner) SetSelected(path string, selected bool) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.snap == nil {
		return fmt.Errorf("no snapshot to edit")
	}
	for i := range cs.snap.Items {
		if cs.snap.Items[i].Path != path {
			continue
		}
		cs.snap.Items[i].Selected = selected
		return cs.persistSnapshot(cs.snap)
	}
	return fmt.Errorf("path not in current snapshot")
}

// SetFolderOverride writes the per-folder metadata cascade for one
// directory. Folder is relative to ScanRoot (use FolderOf on an item
// to derive it). Empty fields clear the cascade entry — operator
// edited it out and wants to fall back to site hints.
func (cs *CollectionScanner) SetFolderOverride(folder string, aid int, season, resolution, source string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.snap == nil {
		return fmt.Errorf("no snapshot to edit")
	}
	if cs.snap.FolderOverrides == nil {
		cs.snap.FolderOverrides = make(map[string]FolderOverride)
	}
	existing := cs.snap.FolderOverrides[folder]
	existing.AID = aid
	existing.Season = strings.TrimSpace(season)
	existing.Resolution = strings.TrimSpace(resolution)
	existing.Source = strings.TrimSpace(source)
	// If the entire override is empty AND not selected, drop the key
	// rather than persist a no-op entry. Keeps the JSON file clean
	// across many edit-then-clear cycles.
	if existing.AID == 0 && existing.Season == "" && existing.Resolution == "" && existing.Source == "" && !existing.Selected {
		delete(cs.snap.FolderOverrides, folder)
	} else {
		cs.snap.FolderOverrides[folder] = existing
	}
	return cs.persistSnapshot(cs.snap)
}

// SelectFolder toggles the Selected flag on the folder-override
// entry, creating one if needed. Folder selection is the bulk-pick
// affordance: clicking the folder's checkbox queues every file
// under it on the next QueueSelectedUploads.
func (cs *CollectionScanner) SelectFolder(folder string, selected bool) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.snap == nil {
		return fmt.Errorf("no snapshot to edit")
	}
	if cs.snap.FolderOverrides == nil {
		cs.snap.FolderOverrides = make(map[string]FolderOverride)
	}
	existing := cs.snap.FolderOverrides[folder]
	existing.Selected = selected
	cs.snap.FolderOverrides[folder] = existing
	return cs.persistSnapshot(cs.snap)
}

// QueueSelectedUploads marks every Selected row as UploadStatus
// "queued" and returns the count. Two sources of "selected":
//
//   - File-level: items with Selected=true
//   - Folder-level: items whose parent FolderOverride.Selected is true
//
// Folder selection clears as it consumes, but the per-file Selected
// flag also clears so a re-click of Upload doesn't requeue the same
// rows. The actual upload worker (slice 5) will drain "queued" rows,
// flip them to "uploading", then "done" or "failed" with a detail.
func (cs *CollectionScanner) QueueSelectedUploads() (int, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.snap == nil {
		return 0, fmt.Errorf("no snapshot to upload")
	}
	queued := 0
	for i := range cs.snap.Items {
		it := &cs.snap.Items[i]
		folder := FolderOf(*it)
		folderSelected := cs.snap.FolderOverrides[folder].Selected
		if !it.Selected && !folderSelected {
			continue
		}
		it.Selected = false
		it.UploadStatus = "queued"
		it.UploadDetail = "Waiting for upload worker"
		queued++
	}
	// Clear folder selections too — same "don't requeue" reasoning.
	for k, v := range cs.snap.FolderOverrides {
		if v.Selected {
			v.Selected = false
			cs.snap.FolderOverrides[k] = v
		}
	}
	if queued == 0 {
		return 0, nil
	}
	if err := cs.persistSnapshot(cs.snap); err != nil {
		return 0, err
	}
	return queued, nil
}

// FolderOf returns the parent folder of a scanned item, relative to
// the snapshot's ScanRoot. Used as the lookup key into
// FolderOverrides + as the grouping key for the tree-view UI.
//
// Root-level files map to "" (empty string). The path separator is
// always "/" since RelPath was already normalised at scan time
// (filepath.ToSlash).
func FolderOf(it CollectionItem) string {
	rel := it.RelPath
	if rel == "" {
		return ""
	}
	idx := strings.LastIndex(rel, "/")
	if idx < 0 {
		return ""
	}
	return rel[:idx]
}

// EffectiveAID returns the precedence-resolved AID for one item.
// File override > folder override > site hint. Used by the render
// path so the UI shows the value the upload pass will actually use.
func (cs *CollectionScanner) EffectiveAID(it CollectionItem) int {
	if it.OverrideAID > 0 {
		return it.OverrideAID
	}
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.snap != nil {
		if f, ok := cs.snap.FolderOverrides[FolderOf(it)]; ok && f.AID > 0 {
			return f.AID
		}
	}
	return it.AID
}

// EffectiveResolution / EffectiveSource / EffectiveSeason mirror
// EffectiveAID's precedence rule for the remaining cascade fields.
// Episode isn't part of this set — it's per-file by nature, lives
// only on the file's OverrideEpisode.
func (cs *CollectionScanner) EffectiveResolution(it CollectionItem) string {
	if it.OverrideResolution != "" {
		return it.OverrideResolution
	}
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.snap != nil {
		if f, ok := cs.snap.FolderOverrides[FolderOf(it)]; ok && f.Resolution != "" {
			return f.Resolution
		}
	}
	return it.ResolutionHint
}

func (cs *CollectionScanner) EffectiveSource(it CollectionItem) string {
	if it.OverrideSource != "" {
		return it.OverrideSource
	}
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.snap != nil {
		if f, ok := cs.snap.FolderOverrides[FolderOf(it)]; ok && f.Source != "" {
			return f.Source
		}
	}
	return ""
}

func (cs *CollectionScanner) EffectiveSeason(it CollectionItem) string {
	if it.OverrideSeason != "" {
		return it.OverrideSeason
	}
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.snap != nil {
		if f, ok := cs.snap.FolderOverrides[FolderOf(it)]; ok && f.Season != "" {
			return f.Season
		}
	}
	return ""
}

// ── Persistence ─────────────────────────────────────────────────────

func (cs *CollectionScanner) loadSnapshot() {
	data, err := os.ReadFile(cs.storePath)
	if err != nil {
		return
	}
	var snap CollectionSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return
	}
	cs.mu.Lock()
	cs.snap = &snap
	cs.mu.Unlock()
}

func (cs *CollectionScanner) persistSnapshot(snap *CollectionSnapshot) error {
	if err := os.MkdirAll(filepath.Dir(cs.storePath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write — temp file + rename so a crash mid-write doesn't
	// leave a half-truncated snapshot the operator has to delete.
	tmp := cs.storePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, cs.storePath)
}

func (cs *CollectionScanner) setLastErr(err error) {
	cs.mu.Lock()
	if err != nil {
		cs.lastScanErr = err.Error()
	}
	cs.mu.Unlock()
}

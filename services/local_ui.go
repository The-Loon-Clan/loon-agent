package services

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/the-loon-clan/loon-agent/client"
	"github.com/the-loon-clan/loon-agent/config"
	"github.com/the-loon-clan/loon-agent/storage"
)

// LocalUI serves a small HTML + JSON interface on loopback for users who
// prefer editing the agent's on-disk config (and the per-tracker passkeys)
// from a browser instead of SSH'ing in. Default bind is 127.0.0.1 so it's
// invisible outside the host unless the operator sets LOCAL_UI_BIND.
//
// It is deliberately feature-narrow:
//   - GET  /         → edit config.yml + secrets.yml
//   - POST /config   → write config.yml via layered.WriteYml
//   - POST /secrets  → write secrets.yml via SecretsStore.Set
//   - GET  /status   → current Layered + secrets snapshot for polling
//
// Anything that would need cross-host access (dashboard, job history) is
// served by the main site. This service never accepts site traffic.
type LocalUI struct {
	cfg      *config.Config
	secrets  *SecretsStore
	db       *storage.DB
	port     int
	bindAddr string

	// site is the authenticated client to the indexer site. Optional:
	// the local UI works without it (groups, watch folders, local
	// config), but the "Agent settings" panel's write path requires
	// it to PUT /api/agent/web-config. Wired via SetSite after
	// StartLocalUI returns because the site client is constructed in
	// main.go alongside poll/status/complete and the LocalUI is
	// created before them.
	site client.Site

	// collection is the Collection-mode scanner — owns the on-disk
	// snapshot, kicks off scans on operator request. Constructed in
	// StartLocalUI so a fork that disables the local UI doesn't pay
	// for the scanner either. Calls Site.TitleMatchBulk per batch
	// when the site client is wired (post-SetSite); falls back to
	// "list-only, no enrichment" if not.
	collection *CollectionScanner
}

// SetSite wires the site client into the local UI. Called from main
// after client.New so the /web-override form handler can write back
// to the site. Concurrent writes are fine — the field is only read
// from HTTP handler goroutines that start after StartLocalUI returns.
//
// Also rebinds the Collection scanner's TitleMatchBulk source so a
// site client connected after the scanner was constructed (the
// common ordering) gets used for enrichment without a re-init.
func (u *LocalUI) SetSite(c client.Site) {
	u.site = c
	if u.collection != nil {
		u.collection.site = c
	}
}

func StartLocalUI(cfg *config.Config, secrets *SecretsStore, db *storage.DB) *LocalUI {
	port, _ := strconv.Atoi(os.Getenv("LOCAL_UI_PORT"))
	if port <= 0 {
		return nil
	}
	bind := os.Getenv("LOCAL_UI_BIND")
	if bind == "" {
		bind = "127.0.0.1"
	}
	ui := &LocalUI{cfg: cfg, secrets: secrets, db: db, port: port, bindAddr: bind}
	// Scanner data dir defaults to ./data so the snapshot lives next
	// to the docker volume the operator is already mounting; main.go
	// creates ./data on boot for other agent state files. The
	// scanner's `site` is nil here — SetSite (called from main after
	// client.New) fills it in.
	ui.collection = NewCollectionScanner(nil, "data")
	addr := net.JoinHostPort(bind, strconv.Itoa(port))

	mux := http.NewServeMux()
	// Shared assets — design tokens vendored from the site. Kept
	// under /_shared/ to flag them visually as "same file as the
	// site ships" and to reserve that prefix for future shared
	// components (a component library, icons, etc).
	mux.HandleFunc("/_shared/tokens.css", ServeTokensCSS)
	mux.HandleFunc("/_shared/components.css", ServeComponentsCSS)
	mux.HandleFunc("/_shared/agent-shell.css", ServeAgentShellCSS)
	// Agent-only assets. Everything under /static/ is baked into
	// the binary via go:embed and cached aggressively by the
	// browser — see cssHandler in localui_assets.go.
	mux.HandleFunc("/static/localui.css", ServeLocalUICSS)

	mux.HandleFunc("/", ui.handleIndex)
	mux.HandleFunc("/config", ui.handleConfig)
	mux.HandleFunc("/config/web-override", ui.handleWebOverride)
	mux.HandleFunc("/secrets", ui.handleSecrets)
	mux.HandleFunc("/status", ui.handleStatus)
	mux.HandleFunc("/groups", ui.handleGroups)
	mux.HandleFunc("/groups/create", ui.handleGroupCreate)
	mux.HandleFunc("/groups/update", ui.handleGroupUpdate)
	mux.HandleFunc("/groups/delete", ui.handleGroupDelete)
	mux.HandleFunc("/watches", ui.handleWatches)
	mux.HandleFunc("/watches/create", ui.handleWatchCreate)
	mux.HandleFunc("/watches/update", ui.handleWatchUpdate)
	mux.HandleFunc("/watches/delete", ui.handleWatchDelete)
	mux.HandleFunc("/jobs", ui.handleJobs)
	mux.HandleFunc("/jobs/retry", ui.handleJobRetry)
	mux.HandleFunc("/jobs/delete", ui.handleJobDelete)
	// Mirror live-status — lists in-flight site-driven tasks from
	// storage.GlobalState.Jobs with per-row Cancel. Without this
	// page the operator can see the agent IS downloading (sidebar
	// throughput) but has no way to know WHAT it's downloading or
	// stop one task without going to the site dashboard.
	mux.HandleFunc("/mirror", ui.handleMirror)
	mux.HandleFunc("/mirror/cancel", ui.handleMirrorCancel)
	mux.HandleFunc("/mirror/pause", ui.handleMirrorPause)
	// Top-level mode stubs (see modeForPage in local_ui_templates.go).
	// Mirror tab lands on /mirror (live tasks); offers + collection
	// get their own landing pages so the three-tab strip in the
	// layout always has a destination per mode.
	mux.HandleFunc("/offers", ui.handleOffers)
	mux.HandleFunc("/collection", ui.handleCollection)
	mux.HandleFunc("/collection/scan", ui.handleCollectionScan)
	mux.HandleFunc("/collection/edit", ui.handleCollectionEdit)
	mux.HandleFunc("/collection/select", ui.handleCollectionSelect)
	mux.HandleFunc("/collection/upload", ui.handleCollectionUpload)
	mux.HandleFunc("/collection/folder-edit", ui.handleCollectionFolderEdit)
	mux.HandleFunc("/collection/folder-select", ui.handleCollectionFolderSelect)
	mux.HandleFunc("/events", ui.handleEvents)

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("Local UI listening on http://%s/", addr)
		if bind != "127.0.0.1" && bind != "localhost" {
			log.Printf("WARNING: Local UI bound to %s — anyone on that network can edit config/secrets. Use 127.0.0.1 unless you've put this behind auth.", bind)
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Local UI server exited: %v", err)
		}
	}()
	return ui
}

// URL is the URL the site UI shows to link back here. When bound to
// loopback we still report it so the user sees where to reach it from
// the agent host; remote users will have to SSH-tunnel.
func (u *LocalUI) URL() string {
	if u == nil {
		return ""
	}
	host := u.bindAddr
	if host == "0.0.0.0" {
		host = "localhost"
	}
	return fmt.Sprintf("http://%s:%d/", host, u.port)
}

func (u *LocalUI) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	type kv struct {
		Key, Value, Source string
	}
	rows := make([]kv, 0, len(config.ConfigYmlKeys))
	for _, k := range config.ConfigYmlKeys {
		rows = append(rows, kv{Key: k, Value: u.cfg.Layered.Effective(k), Source: sourceFor(u.cfg.Layered, k)})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"config":          rows,
		"config_writable": u.cfg.Layered.Writable(),
		"secrets_hosts":   u.secrets.List(),
		"has_secrets":     u.secrets.Has(),
	})
}

// sourceFor returns the tier that supplied a key's effective value so the
// local UI can show a small badge matching the site's state model.
func sourceFor(l *config.Layered, key string) string {
	snap := l.LocalSnapshot()
	if v, ok := snap[key]; ok {
		return v.Source
	}
	return "default"
}

func (u *LocalUI) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	updates := map[string]string{}
	for _, k := range config.ConfigYmlKeys {
		if v := r.PostForm.Get(k); v != "" || r.PostForm.Has(k) {
			updates[k] = strings.TrimSpace(v)
		}
	}
	written, err := u.cfg.Layered.WriteYml(updates)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.cfg.Refresh()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "written": written})
}

// handleWebOverride writes (or clears) a single web-tier override on
// the site via PUT /api/agent/web-config. The form posts three fields:
// "key", "value" (empty to clear), and "return_to" (redirect target).
// This is the agent's first "manage the site from the local UI" entry
// point — everything else the site's admin dashboard does still lives
// there, but the per-agent runtime knobs can now be tweaked without
// alt-tabbing.
func (u *LocalUI) handleWebOverride(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if u.site == nil {
		http.Error(w, "site client not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(r.PostForm.Get("key"))
	value := strings.TrimSpace(r.PostForm.Get("value"))
	returnTo := r.PostForm.Get("return_to")
	if returnTo == "" {
		returnTo = "/"
	}
	if key == "" {
		redirectWithFlash(w, r, returnTo, "", "key is required")
		return
	}
	if err := u.site.PutWebConfig(key, value); err != nil {
		redirectWithFlash(w, r, returnTo, "", err.Error())
		return
	}
	// Re-fetch the effective config from the site so the next render
	// reflects what we just wrote (avoids a confusing lag where the
	// form shows the old value until the next poll-driven refresh).
	if rc, err := u.site.GetConfig(); err == nil && rc != nil {
		u.cfg.Layered.ApplyWeb(rc.WebOverrides)
	}
	msg := "set " + key
	if value == "" {
		msg = "cleared " + key
	}
	redirectWithFlash(w, r, returnTo, msg, "")
}

func (u *LocalUI) handleSecrets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	host := r.PostForm.Get("host")
	key := r.PostForm.Get("key")
	if host == "" {
		http.Error(w, "missing host", http.StatusBadRequest)
		return
	}
	if err := u.secrets.Set(host, key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// ── Template FuncMaps ─────────────────────────────────────────────────────

// groupsTmplFuncs: small helpers the groups form relies on. Registered
// before Parse so the template compiler treats them as function calls
// instead of field lookups (which would fail at render time).
var groupsTmplFuncs = template.FuncMap{
	// derefBool reads through a *bool, returning false for nil so template
	// comparisons like `{{if derefBool .Obfuscate}}` don't panic on the
	// "inherit global" case.
	"derefBool": func(b *bool) bool {
		if b == nil {
			return false
		}
		return *b
	},
}

// jobsTmplFuncs: helpers for the /jobs page.
var jobsTmplFuncs = template.FuncMap{
	"formatTime": func(t *time.Time) string {
		if t == nil || t.IsZero() {
			return "—"
		}
		return t.Local().Format("Jan 02 15:04:05")
	},
	// statusClass maps a job status to a CSS class so the status badge
	// can be coloured without the template having to branch five times.
	"statusClass": func(s string) string {
		switch s {
		case "completed":
			return "ok"
		case "failed":
			return "err"
		case "processing":
			return "yml"
		case "cancelled":
			return "default"
		default: // queued
			return "env"
		}
	},
}

// watchesTmplFuncs: helpers for comparing nullable group FKs in the
// watches template.
var watchesTmplFuncs = template.FuncMap{
	// isGroup reports whether a nullable group pointer matches a given id.
	// Lets the <select> option loop mark the currently-assigned group
	// without template gymnastics over *int64.
	"isGroup": func(sel *int64, id int64) bool {
		return sel != nil && *sel == id
	},
}

// ── Page templates ────────────────────────────────────────────────────────
//
// Each page overrides two blocks defined by the layout: "title" (shown in
// <title> + top bar <h1>) and "content" (main column). The shared sidebar,
// status panel, and Flash/Err rendering come from the layout — page
// templates focus only on what's actually different between pages.

var groupsTmpl = buildPageTemplate("groups", `
{{define "title"}}Groups{{end}}
{{define "content"}}
<p class="lead">
Groups define where offline uploads post (newsgroups) and let you override
the global PAR2/screenshot/obfuscation defaults per category. Leave a
field blank to inherit the global default.
</p>
<p class="small" style="border-left:3px solid #888;padding-left:8px;margin:6px 0 12px;color:#888;">
<strong>Scope reminder:</strong> these settings — including <em>banned extensions</em> — only
apply to <em>watch-folder / offline</em> jobs handled by this agent. Tasks
fetched from the site (the polling loop) use the per-agent list on the
site's <code>/account-settings/agent/&lt;id&gt;</code> page instead.
</p>

<h2>Existing</h2>
{{if .Groups}}
<table>
<tr>
  <th>Name</th><th>Type</th><th>Newsgroups</th>
  <th>Samples</th><th>Sec/sample</th>
  <th>Banned ext.</th>
  <th>PAR2%</th><th>Obf.</th><th>Watermark</th>
  <th>Source</th><th>v</th><th></th>
</tr>
{{range .Groups}}
<tr>
<form class="row-form" method="post" action="/groups/update">
  <input type="hidden" name="id" value="{{.ID}}">
  <td><input name="name" value="{{.Name}}" size="12" required{{if eq .Source "site"}} readonly{{end}}></td>
  <td><input name="type" value="{{.Type}}" size="7" placeholder="video" list="type-list"{{if eq .Source "site"}} readonly{{end}}></td>
  <td><textarea name="newsgroups" rows="2" required{{if eq .Source "site"}} readonly{{end}}>{{range .Newsgroups}}{{.}}
{{end}}</textarea></td>
  <td><input name="screenshots" value="{{if .Screenshots}}{{.Screenshots}}{{end}}" size="3" type="number" min="0"{{if eq .Source "site"}} readonly{{end}}></td>
  <td><input name="sample_seconds" value="{{if .SampleSeconds}}{{.SampleSeconds}}{{end}}" size="3" type="number" min="1"{{if eq .Source "site"}} readonly{{end}}></td>
  <td><textarea name="banned_extensions" rows="2" cols="12" placeholder="(default list)"{{if eq .Source "site"}} readonly{{end}}>{{range .BannedExtensions}}{{.}}
{{end}}</textarea></td>
  <td><input name="par2_redundancy" value="{{if .Par2Redundancy}}{{.Par2Redundancy}}{{end}}" size="3" type="number" min="0" max="100"{{if eq .Source "site"}} readonly{{end}}></td>
  <td>
    <select name="obfuscate"{{if eq .Source "site"}} disabled{{end}}>
      <option value=""{{if not .Obfuscate}} selected{{end}}>inherit</option>
      {{if .Obfuscate}}
        {{if derefBool .Obfuscate}}<option value="1" selected>yes</option><option value="0">no</option>
        {{else}}<option value="1">yes</option><option value="0" selected>no</option>{{end}}
      {{else}}<option value="1">yes</option><option value="0">no</option>{{end}}
    </select>
  </td>
  <td><input name="watermark_text" value="{{.WatermarkText}}" size="10" placeholder="-YourTag"{{if eq .Source "site"}} readonly{{end}}></td>
  <td><span class="badge {{.Source}}">{{.Source}}</span></td>
  <td class="small">{{.Version}}</td>
  <td>
    {{if ne .Source "site"}}<button type="submit" class="primary">Save</button>{{end}}
</form>
{{if ne .Source "site"}}
<form class="row-form" method="post" action="/groups/delete" style="display:inline;" onsubmit="return confirm('Delete {{.Name}}?')">
  <input type="hidden" name="id" value="{{.ID}}">
  <button type="submit" class="danger">Delete</button>
</form>
{{end}}
  </td>
</tr>
{{end}}
</table>
<datalist id="type-list">
  <option value="video"><option value="manga"><option value="music">
</datalist>
{{else}}
<p class="small">No groups defined yet.</p>
{{end}}

<h2 style="margin-top:28px;">Create new</h2>
<form method="post" action="/groups/create">
<table>
<tr><th>Name</th><td><input name="name" placeholder="anime" required></td></tr>
<tr><th>Type</th><td><input name="type" placeholder="video" list="type-list-new"><datalist id="type-list-new"><option value="video"><option value="manga"><option value="music"></datalist> <span class="small">video / manga / music — or any other label for a custom sampling behaviour</span></td></tr>
<tr><th>Newsgroups</th><td><textarea name="newsgroups" rows="3" placeholder="alt.binaries.multimedia.anime.highspeed&#10;alt.binaries.boneless" required></textarea><div class="small">One per line (commas also work).</div></td></tr>
<tr><th>Samples</th><td><input name="screenshots" size="3" type="number" min="0"> <span class="small">count — for video: screenshots; manga: pages; music: audio clips. Blank = inherit default (6).</span></td></tr>
<tr><th>Sec/sample</th><td><input name="sample_seconds" size="3" type="number" min="1"> <span class="small">audio-only: duration of each clip. Blank = inherit default (5).</span></td></tr>
<tr><th>Banned extensions</th><td><textarea name="banned_extensions" rows="2" placeholder="(leave blank to inherit the default blocklist)"></textarea><div class="small">One per line. Blank = use the hardcoded default; non-empty list replaces the default outright. <strong>Scope:</strong> applies only to <em>watch-folder / offline</em> jobs for this group. Site-polling tasks use the per-agent list on the site's <code>/account-settings/agent/&lt;id&gt;</code> page instead.</div></td></tr>
<tr><th>PAR2 %</th><td><input name="par2_redundancy" size="3" type="number" min="0" max="100"> <span class="small">blank = inherit global default</span></td></tr>
<tr><th>Obfuscate</th><td><select name="obfuscate"><option value="">inherit</option><option value="1">yes</option><option value="0">no</option></select></td></tr>
<tr><th>Watermark</th><td><input name="watermark_text" placeholder="-YourTag"> <span class="small">drawn on every screenshot; blank = off</span></td></tr>
</table>
<p><button type="submit" class="primary">Create group</button></p>
</form>
{{end}}
`, groupsTmplFuncs)

var watchesTmpl = buildPageTemplate("watches", `
{{define "title"}}Watch Folders{{end}}
{{define "content"}}
<p class="lead">
Folders the offline pipeline scans for new files. Each folder is tagged
with a group, which decides where the resulting NZB gets posted. Paths
must be absolute.
</p>

<h2>Existing</h2>
{{if .Watches}}
<table>
<tr><th>Path</th><th>Group</th><th>Enabled</th><th></th></tr>
{{range .Watches}}
<tr>
<form class="row-form" method="post" action="/watches/update">
  <input type="hidden" name="id" value="{{.ID}}">
  <input type="hidden" name="enabled_present" value="1">
  <td><input name="path" value="{{.Path}}" size="40" required></td>
  <td>
    <select name="group_id">
      <option value=""{{if not .GroupID}} selected{{end}}>— unassigned —</option>
      {{$selected := .GroupID}}
      {{range $.Groups}}
        <option value="{{.ID}}"{{if isGroup $selected .ID}} selected{{end}}>{{.Name}}</option>
      {{end}}
    </select>
  </td>
  <td><input type="checkbox" name="enabled" value="1"{{if .Enabled}} checked{{end}}></td>
  <td>
    <button type="submit" class="primary">Save</button>
</form>
<form class="row-form" method="post" action="/watches/delete" style="display:inline;" onsubmit="return confirm('Delete watch {{.Path}}?')">
  <input type="hidden" name="id" value="{{.ID}}">
  <button type="submit" class="danger">Delete</button>
</form>
  </td>
</tr>
{{end}}
</table>
{{else}}
<p class="small">No watch folders configured yet.</p>
{{end}}

<h2 style="margin-top:28px;">Create new</h2>
{{if .Groups}}
<form method="post" action="/watches/create">
<input type="hidden" name="enabled_present" value="1">
<table>
<tr><th>Path</th><td><input name="path" placeholder="/data/watch/anime" size="40" required></td></tr>
<tr><th>Group</th><td>
  <select name="group_id" required>
    <option value="">— pick a group —</option>
    {{range .Groups}}<option value="{{.ID}}">{{.Name}}</option>{{end}}
  </select>
</td></tr>
<tr><th>Enabled</th><td><input type="checkbox" name="enabled" value="1" checked></td></tr>
</table>
<p><button type="submit" class="primary">Add watch</button></p>
</form>
{{else}}
<p style="color:var(--warn);">Create at least one <a href="/groups" style="color:var(--blue);">group</a> first, then come back to wire a folder to it.</p>
{{end}}
{{end}}
`, watchesTmplFuncs)

var jobsTmpl = buildPageTemplate("jobs", `
{{define "title"}}Offline Jobs{{end}}
{{define "content"}}
<p class="lead">
Every file detected in a watch folder becomes a job. The processor runs
them end-to-end: stage → PAR2 → optional encrypt → upload → NZB written
to the output dir. Retry a failed job once the underlying issue is fixed;
deleting a row lets the watcher re-queue the same source file on next scan.
</p>

{{if .Jobs}}
<table>
<thead>
<tr><th>Title</th><th>Group</th><th>Status</th><th>Created</th><th>Error</th><th></th></tr>
</thead>
<tbody>
{{range .Jobs}}
<tr>
  <td>{{.Title}}<div class="small">{{.SourcePath}}</div></td>
  <td>{{.GroupNameAtCreation}}</td>
  <td><span class="badge {{statusClass .Status}}">{{.Status}}</span>{{if .Phase}} <span class="small">{{.Phase}}</span>{{end}}</td>
  <td class="small">{{.CreatedAt.Local.Format "Jan 02 15:04:05"}}</td>
  <td style="color:var(--red);" class="small">{{.Error}}</td>
  <td>
    {{if or (eq .Status "failed") (eq .Status "completed")}}
    <form class="row-form" method="post" action="/jobs/retry" style="display:inline;">
      <input type="hidden" name="id" value="{{.ID}}">
      <button type="submit" class="primary">Retry</button>
    </form>
    {{end}}
    <form class="row-form" method="post" action="/jobs/delete" style="display:inline;" onsubmit="return confirm('Delete job for {{.Title}}?')">
      <input type="hidden" name="id" value="{{.ID}}">
      <button type="submit" class="danger">Delete</button>
    </form>
  </td>
</tr>
{{end}}
</tbody>
</table>
{{else}}
<p class="small">No jobs yet. Drop a file into a watch folder and give it a moment.</p>
{{end}}
{{end}}
`, jobsTmplFuncs)

var localUITmpl = buildPageTemplate("config", `
{{define "title"}}Config{{end}}
{{define "content"}}
<p class="lead">
Edit the layered agent config and per-tracker passkeys. Changes here are
persisted on this machine only; passkeys never leave the host.
{{if not .Writable}}<br><strong style="color:var(--warn);">config.yml is read-only</strong> — check file permission / Docker bind-mount.{{end}}
</p>

<h2>Config (config.yml)</h2>
<form id="cf">
<table>
<tr><th>Key</th><th>Value</th><th>Source</th></tr>
{{range .Rows}}
<tr>
  <td><code>{{.Key}}</code></td>
  <td><input name="{{.Key}}" value="{{.Value}}" size="30"></td>
  <td><span class="badge {{.Source}}">{{.Source}}</span></td>
</tr>
{{end}}
</table>
<p><button type="submit" class="primary" {{if not .Writable}}disabled{{end}}>Save to config.yml</button>
<span id="cs" class="small"></span></p>
</form>

<h2 style="margin-top:28px;">Site-side Overrides (web tier)</h2>
<p class="small">
Values the site has set for this agent. Edited here, written back to the site over the same
channel the agent polls with — no need to log into the site's admin dashboard to tweak an
override. Empty the value and save to clear an override.
{{if not .SiteConnected}}<br><strong style="color:var(--warn);">Site client not configured — form disabled.</strong>{{end}}
</p>
<table>
<tr><th>Key</th><th>Value</th><th></th></tr>
{{range $k, $v := .WebOverrides}}
<tr>
  <td><code>{{$k}}</code></td>
  <td>
    <form class="row-form" method="post" action="/config/web-override">
      <input type="hidden" name="key" value="{{$k}}">
      <input type="hidden" name="return_to" value="/">
      <input name="value" value="{{$v}}" size="30">
      <button type="submit" class="primary" {{if not $.SiteConnected}}disabled{{end}}>Save</button>
    </form>
  </td>
  <td>
    <form class="row-form" method="post" action="/config/web-override" style="display:inline;">
      <input type="hidden" name="key" value="{{$k}}">
      <input type="hidden" name="value" value="">
      <input type="hidden" name="return_to" value="/">
      <button type="submit" class="danger" {{if not $.SiteConnected}}disabled{{end}}>Clear</button>
    </form>
  </td>
</tr>
{{else}}
<tr><td colspan="3" class="small">No site-side overrides set.</td></tr>
{{end}}
</table>
<form method="post" action="/config/web-override" style="margin-top:12px;">
  <input type="hidden" name="return_to" value="/">
  <input name="key" placeholder="max_concurrent_downloads" required size="30">
  <input name="value" placeholder="new value" size="20">
  <button type="submit" class="primary" {{if not .SiteConnected}}disabled{{end}}>Add override</button>
</form>

<h2 style="margin-top:28px;">Private Trackers (secrets.yml, 0600)</h2>
<p class="small">Passkeys stay on this machine only. The site sees <em>that</em> trackers are configured, not the keys.</p>
<table id="sec">
<tr><th>Host</th><th></th></tr>
{{range .SecretsHosts}}
<tr><td>{{.}}</td><td><button type="button" onclick="delHost('{{.}}')" class="danger">remove</button></td></tr>
{{else}}
<tr><td colspan="2" class="small">No trackers configured.</td></tr>
{{end}}
</table>
<form id="sf" style="margin-top:12px;">
<input name="host" placeholder="nekobt.to" required>
<input name="key" placeholder="your passkey" required size="40">
<button type="submit" class="primary">Add / update</button>
<span id="ss" class="small"></span>
</form>

<script>
const cs = document.getElementById('cs');
document.getElementById('cf').addEventListener('submit', async e => {
    e.preventDefault();
    const body = new FormData(e.target);
    const r = await fetch('/config', { method: 'POST', body: new URLSearchParams(body) });
    cs.textContent = r.ok ? ' saved.' : ' error.';
    setTimeout(() => cs.textContent = '', 2000);
});
const ss = document.getElementById('ss');
document.getElementById('sf').addEventListener('submit', async e => {
    e.preventDefault();
    const body = new FormData(e.target);
    const r = await fetch('/secrets', { method: 'POST', body: new URLSearchParams(body) });
    if (r.ok) location.reload();
    else ss.textContent = ' error.';
});
async function delHost(h) {
    const body = new URLSearchParams({ host: h, key: '' });
    const r = await fetch('/secrets', { method: 'POST', body });
    if (r.ok) location.reload();
}
</script>
{{end}}
`, nil)

func (u *LocalUI) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	type row struct{ Key, Value, Source string }
	rows := make([]row, 0, len(config.ConfigYmlKeys))
	for _, k := range config.ConfigYmlKeys {
		rows = append(rows, row{Key: k, Value: u.cfg.Layered.Effective(k), Source: sourceFor(u.cfg.Layered, k)})
	}
	data := u.baseData("config", r.URL.Query().Get("msg"), r.URL.Query().Get("err"))
	data["Writable"] = u.cfg.Layered.Writable()
	data["Rows"] = rows
	data["SecretsHosts"] = u.secrets.List()
	data["WebOverrides"] = u.cfg.Layered.WebSnapshot()
	data["SiteConnected"] = u.site != nil
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = localUITmpl.Execute(w, data)
}

// ── Groups ────────────────────────────────────────────────────────────────
//
// Groups are edited exclusively from the local UI for now (no /api prefix,
// no auth) because the whole localui is loopback-only. Once we add an auth
// layer (planned for when the offline pipeline goes GA) the same handlers
// can mount under an authenticated mux without being rewritten.

func (u *LocalUI) handleGroups(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	groups, err := u.db.ListGroups()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := u.baseData("groups", r.URL.Query().Get("msg"), r.URL.Query().Get("err"))
	data["Groups"] = groups
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = groupsTmpl.Execute(w, data)
}

func (u *LocalUI) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	g, err := groupFromForm(r)
	if err != nil {
		redirectGroups(w, r, "", err.Error())
		return
	}
	if err := u.db.CreateGroup(g); err != nil {
		redirectGroups(w, r, "", err.Error())
		return
	}
	redirectGroups(w, r, "created "+g.Name, "")
}

func (u *LocalUI) handleGroupUpdate(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	g, err := groupFromForm(r)
	if err != nil {
		redirectGroups(w, r, "", err.Error())
		return
	}
	if err := u.db.UpdateGroup(g); err != nil {
		redirectGroups(w, r, "", err.Error())
		return
	}
	redirectGroups(w, r, "updated "+g.Name, "")
}

func (u *LocalUI) handleGroupDelete(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		redirectGroups(w, r, "", "invalid id")
		return
	}
	if err := u.db.DeleteGroup(id); err != nil {
		redirectGroups(w, r, "", err.Error())
		return
	}
	redirectGroups(w, r, "deleted", "")
}

// groupFromForm parses the shared create/update form into a Group. The id
// field is optional (create path leaves it 0); everything else is validated
// in storage.validateGroup so this stays thin.
func groupFromForm(r *http.Request) (*storage.Group, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	g := &storage.Group{Source: "local"}
	if s := r.PostFormValue("id"); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid id: %v", err)
		}
		g.ID = id
	}
	g.Name = r.PostFormValue("name")
	// Newsgroups entered one per line in the textarea, but accept commas
	// and whitespace too so copy-paste from anywhere works.
	raw := r.PostFormValue("newsgroups")
	for _, line := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t'
	}) {
		g.Newsgroups = append(g.Newsgroups, line)
	}
	g.Screenshots = parseOptInt(r.PostFormValue("screenshots"))
	g.Par2Redundancy = parseOptInt(r.PostFormValue("par2_redundancy"))
	if v := r.PostFormValue("obfuscate"); v != "" {
		b := v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
		g.Obfuscate = &b
	}
	g.WatermarkText = strings.TrimSpace(r.PostFormValue("watermark_text"))
	g.Type = strings.TrimSpace(r.PostFormValue("type"))
	g.SampleSeconds = parseOptInt(r.PostFormValue("sample_seconds"))
	// Banned extensions: accept one-per-line or comma-separated, validate
	// normalises dots and case so the operator can paste loose input.
	rawBans := r.PostFormValue("banned_extensions")
	for _, ext := range strings.FieldsFunc(rawBans, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t'
	}) {
		g.BannedExtensions = append(g.BannedExtensions, ext)
	}
	return g, nil
}

// parseOptInt returns nil for blank input so "inherit global default"
// survives a form round-trip; non-empty but invalid falls through to nil
// for the same reason — the UI's numeric input prevents that in practice.
func parseOptInt(s string) *int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		return nil
	}
	return &i
}

func redirectGroups(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	redirectWithFlash(w, r, "/groups", msg, errMsg)
}

// ── Watch folders ─────────────────────────────────────────────────────────
//
// Same UI pattern as /groups: one row per watch with an inline update form,
// a create form below, and flash messages via redirect query params. The
// polling watcher goroutine (added in a later commit) consumes the same
// rows via ListActiveWatches.

func (u *LocalUI) handleWatches(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	watches, err := u.db.ListWatches()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Groups are fetched so the edit/create forms can render a <select>
	// with human names rather than raw ids; N+1 avoided by doing it once.
	groups, err := u.db.ListGroups()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := u.baseData("watches", r.URL.Query().Get("msg"), r.URL.Query().Get("err"))
	data["Watches"] = watches
	data["Groups"] = groups
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = watchesTmpl.Execute(w, data)
}

func (u *LocalUI) handleWatchCreate(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	watch, err := watchFromForm(r)
	if err != nil {
		redirectWatches(w, r, "", err.Error())
		return
	}
	if err := u.db.CreateWatch(watch); err != nil {
		redirectWatches(w, r, "", err.Error())
		return
	}
	redirectWatches(w, r, "added "+watch.Path, "")
}

func (u *LocalUI) handleWatchUpdate(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	watch, err := watchFromForm(r)
	if err != nil {
		redirectWatches(w, r, "", err.Error())
		return
	}
	if err := u.db.UpdateWatch(watch); err != nil {
		redirectWatches(w, r, "", err.Error())
		return
	}
	redirectWatches(w, r, "updated "+watch.Path, "")
}

func (u *LocalUI) handleWatchDelete(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		redirectWatches(w, r, "", "invalid id")
		return
	}
	if err := u.db.DeleteWatch(id); err != nil {
		redirectWatches(w, r, "", err.Error())
		return
	}
	redirectWatches(w, r, "deleted", "")
}

func watchFromForm(r *http.Request) (*storage.WatchFolder, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	wf := &storage.WatchFolder{}
	if s := r.PostFormValue("id"); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid id: %v", err)
		}
		wf.ID = id
	}
	wf.Path = r.PostFormValue("path")
	if s := strings.TrimSpace(r.PostFormValue("group_id")); s != "" {
		gid, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid group id: %v", err)
		}
		wf.GroupID = &gid
	}
	// HTML checkboxes don't post when unchecked — the hidden "enabled_present"
	// field lets us distinguish "form submitted with box unchecked" from
	// "field missing entirely" so the update handler can flip enabled to 0.
	if r.PostFormValue("enabled_present") != "" {
		wf.Enabled = r.PostFormValue("enabled") == "1"
	} else {
		wf.Enabled = true
	}
	return wf, nil
}

func redirectWatches(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	redirectWithFlash(w, r, "/watches", msg, errMsg)
}

// ── Offline jobs ──────────────────────────────────────────────────────────
//
// Read-only list for now plus retry/delete. The processor lands in the
// next commit; until then rows only ever transition between 'queued' and
// 'failed' via retry, never to 'completed'.

func (u *LocalUI) handleJobs(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	jobs, err := u.db.ListOfflineJobs(100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := u.baseData("jobs", r.URL.Query().Get("msg"), r.URL.Query().Get("err"))
	data["Jobs"] = jobs
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = jobsTmpl.Execute(w, data)
}

func (u *LocalUI) handleJobRetry(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		redirectJobs(w, r, "", "invalid id")
		return
	}
	if err := u.db.ResetQueuedJob(id); err != nil {
		redirectJobs(w, r, "", err.Error())
		return
	}
	redirectJobs(w, r, "requeued", "")
}

func (u *LocalUI) handleJobDelete(w http.ResponseWriter, r *http.Request) {
	if u.db == nil {
		http.Error(w, "database not initialised", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		redirectJobs(w, r, "", "invalid id")
		return
	}
	if err := u.db.DeleteJob(id); err != nil {
		redirectJobs(w, r, "", err.Error())
		return
	}
	redirectJobs(w, r, "deleted", "")
}

func redirectJobs(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	redirectWithFlash(w, r, "/jobs", msg, errMsg)
}

// ── Live events (SSE) ─────────────────────────────────────────────────────
//
// /events streams the agent's current speed/state/job-count snapshot every
// ~1.5s. Every page subscribes via EventSource in the layout's <script>
// block and updates the sidebar in place — no polling, no page reloads.
// The 5s site-post loop is the authoritative aggregation cadence; SSE
// pushes whatever snapshot is most recent, so clients see up-to-5s-old
// numbers in between aggregations. Good enough for a sidebar; the graph
// just repeats a point until the next aggregation.
func (u *LocalUI) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // stop nginx/gluetun proxies from buffering

	rc := http.NewResponseController(w)
	// The server has a 10s WriteTimeout that would kill a long-lived SSE
	// connection. Disable it on this response only.
	_ = rc.SetWriteDeadline(time.Time{})

	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()

	writeEvent := func() bool {
		snap := GetLiveSnapshot()
		payload := map[string]any{
			"phase":            snap.Phase,
			"task_title":       snap.TaskTitle,
			"download_mbps":    snap.DownloadMBps,
			"upload_mbps":      snap.UploadMBps,
			"vpn_status":       snap.VPNStatus,
			"public_ip":        snap.PublicIP,
			"disk_free_gb":     snap.DiskFreeGB,
			"disk_reserved_gb": snap.DiskReservedGB,
			"disk_total_gb":    snap.DiskTotalGB,
		}
		if u.db != nil {
			// Job counts per status — drive sidebar badges and also catches
			// the case where the operator opens the UI mid-job to see "1
			// processing" without having to refresh.
			if counts, err := u.db.CountJobsByStatus(); err == nil {
				payload["jobs"] = counts
			}
		}
		body, _ := json.Marshal(payload)
		if _, err := fmt.Fprintf(w, "event: status\ndata: %s\n\n", body); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	// Send an initial event immediately so the sidebar doesn't show zeros
	// for up to 1.5s after page load.
	if !writeEvent() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !writeEvent() {
				return
			}
		}
	}
}

// ─── Mirror live-status ─────────────────────────────────────────────
//
// Mirror is the request-claim pipeline — the agent polls the site,
// claims a request, fetches the torrent, posts the NZB, reports back.
// The in-flight state for each claim lives in
// storage.GlobalState.Jobs (a map keyed by job name) and
// storage.JobCancels (a parallel map of context.CancelFunc per job).
//
// Without /mirror the local UI never surfaced either: the sidebar's
// "Throughput" tile shows raw bytes/sec, but the operator couldn't
// tell WHAT was downloading or stop one stuck claim without going
// to the site dashboard. /mirror reads GlobalState.Jobs directly +
// /mirror/cancel triggers the same cancel function the site sends
// via CancelRequestID — same kill path, just local.

func (u *LocalUI) handleMirror(w http.ResponseWriter, r *http.Request) {
	type row struct {
		Name      string
		Title     string
		Phase     string
		Details   string
		Progress  float64
		UpdatedAt string
	}
	// Filter to LIVE tasks only — those with a registered cancel
	// context in JobCancels. JobCancels is in-memory (sync.Map) and
	// not persisted, while GlobalState.Jobs is loaded from state.json
	// on boot. So entries with no cancel func are leftovers from a
	// prior agent run that died mid-task; we don't want them showing
	// as "Downloading" on an idle agent.
	storage.GlobalState.RLock()
	rows := make([]row, 0, len(storage.GlobalState.Jobs))
	for _, j := range storage.GlobalState.Jobs {
		if _, live := storage.JobCancels.Load(j.Name); !live {
			continue
		}
		rows = append(rows, row{
			Name:      j.Name,
			Title:     j.Title,
			Phase:     j.Phase,
			Details:   j.Details,
			Progress:  j.Progress,
			UpdatedAt: j.UpdatedAt,
		})
	}
	paused := storage.GlobalState.QueuePaused
	storage.GlobalState.RUnlock()
	// Stable order so a repeat refresh doesn't reshuffle rows under
	// the operator's eyes. Name is the unique claim identifier
	// (request-<id>) so alpha-sorting groups newest claims at the
	// bottom and the oldest stuck one at the top — easy to spot.
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	data := u.baseData("mirror", r.URL.Query().Get("msg"), r.URL.Query().Get("err"))
	data["Rows"] = rows
	data["QueuePaused"] = paused
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = mirrorTmpl.Execute(w, data)
}

// handleMirrorPause toggles the QueuePaused flag. When set, the poll
// loop stops claiming new work but in-flight tasks keep running —
// per-task Cancel is the way to stop one of those. Form action
// "pause" sets it; "resume" clears it.
func (u *LocalUI) handleMirrorPause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/mirror", http.StatusSeeOther)
		return
	}
	storage.GlobalState.Lock()
	switch strings.TrimSpace(r.FormValue("action")) {
	case "pause":
		storage.GlobalState.QueuePaused = true
	case "resume":
		storage.GlobalState.QueuePaused = false
	}
	storage.GlobalState.Unlock()
	storage.SaveState()
	msg := "Queue resumed — claims will begin again on the next poll."
	if storage.GlobalState.QueuePaused {
		msg = "Queue paused — no new claims; in-flight tasks keep running."
	}
	redirectWithFlash(w, r, "/mirror", msg, "")
}

// handleMirrorCancel triggers the cancel context for a named job.
// The cancel signal flows the same path as the site-initiated
// cancellation in main.go's status loop — pulls from JobCancels,
// invokes the CancelFunc; the in-flight task tears down at the
// next ctx.Err() check.
//
// No-op + harmless flash when the name isn't found (job already
// finished, name typo). The map's CancelFunc deletes itself on
// task end-of-life so a stale name just misses.
func (u *LocalUI) handleMirrorCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/mirror", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		redirectWithFlash(w, r, "/mirror", "", "Cancel: name required")
		return
	}
	cancelFn, ok := storage.JobCancels.Load(name)
	if !ok {
		redirectWithFlash(w, r, "/mirror", "", "No active task named "+name)
		return
	}
	cancelFn.(context.CancelFunc)()
	redirectWithFlash(w, r, "/mirror", "Cancel signal sent to "+name+".", "")
}

var mirrorTmpl = buildPageTemplate("mirror", `
{{define "title"}}Mirror — Active Downloads{{end}}
{{define "content"}}
{{/* Live view. Meta-refresh keeps the table fresh without JS —
     5s is the same cadence the site dashboard uses for the same
     data. */}}
<meta http-equiv="refresh" content="5">

{{/* Queue-pause control. The toggle is operator-only via the local
     UI; the polling loop checks storage.GlobalState.QueuePaused
     before claiming new work. In-flight tasks aren't affected — use
     per-row Cancel to stop one of those. */}}
<div class="card" style="display:flex;justify-content:space-between;align-items:center;gap:12px;flex-wrap:wrap;">
  <div>
    {{if .QueuePaused}}
    <strong style="color:var(--warn);">Queue paused</strong>
    <div class="small">Agent is not claiming new tasks. In-flight tasks keep running.</div>
    {{else}}
    <strong style="color:var(--green);">Queue active</strong>
    <div class="small">Agent claims new tasks as capacity allows.</div>
    {{end}}
  </div>
  <form method="post" action="/mirror/pause" class="row-form">
    {{if .QueuePaused}}
      <input type="hidden" name="action" value="resume">
      <button type="submit" class="primary">Resume queue</button>
    {{else}}
      <input type="hidden" name="action" value="pause">
      <button type="submit" class="danger">Pause queue</button>
    {{end}}
  </form>
</div>

{{if .Rows}}
<table style="margin-top:16px;">
<thead>
<tr>
  <th>Release</th>
  <th style="width:160px;">Phase</th>
  <th style="width:140px;">Progress</th>
  <th style="width:90px;">Updated</th>
  <th style="width:80px;"></th>
</tr>
</thead>
<tbody>
{{range .Rows}}
<tr>
  <td>
    {{/* Title first — the human-readable release name. Lock id
         lives below as small monospace so the operator can still
         reference it in logs / site dashboard URLs. */}}
    <div>{{if .Title}}{{.Title}}{{else}}<em class="small">no title</em>{{end}}</div>
    <div class="small"><code>{{.Name}}</code></div>
  </td>
  <td>
    <div>{{.Phase}}</div>
    {{if .Details}}<div class="small">{{.Details}}</div>{{end}}
  </td>
  <td>
    <div class="disk-bar-track" style="margin-top:2px;">
      <div class="disk-bar-fill" style="width:{{printf "%.0f" .Progress}}%;"></div>
    </div>
    <div class="small" style="margin-top:2px;">{{printf "%.1f" .Progress}}%</div>
  </td>
  <td class="small">{{.UpdatedAt}}</td>
  <td>
    <form method="post" action="/mirror/cancel" class="row-form"
          onsubmit="return confirm('Cancel {{if .Title}}{{.Title}}{{else}}{{.Name}}{{end}}? The task will tear down at the next checkpoint.');">
      <input type="hidden" name="name" value="{{.Name}}">
      <button type="submit" class="danger">Cancel</button>
    </form>
  </td>
</tr>
{{end}}
</tbody>
</table>
{{else}}
<p class="small" style="margin-top:16px;">
  {{if .QueuePaused}}
    Queue is paused; no in-flight tasks.
  {{else}}
    No active Mirror tasks. The agent is idle (or between polls — the next poll happens within the configured POLL_INTERVAL).
  {{end}}
</p>
{{end}}
{{end}}
`, nil)

// ─── Offers + Collection mode stubs ─────────────────────────────────
//
// These two routes are the placeholders that make the three-tab UI
// (Mirror / Offers / Collection) feel real. Each one renders an
// info card explaining what the mode does + where the existing
// surfaces live, with a "coming soon" footer. The real screens land
// in later slices:
//
//   Offers     — surface the offer queue + offer config that
//                services/offer_*.go already power; right now they're
//                visible only via the site dashboard.
//   Collection — local-file walker + per-file enrichment using the
//                /api/agent/title-match-bulk endpoint shipped in
//                indexer-site commit a4102f0, then push the resulting
//                NZB back to the site.
//
// The placeholder content matters more than zero: it gives the
// operator a clear mental model of what each mode is for the moment
// they discover the tabs, instead of an empty pane that looks broken.

func (u *LocalUI) handleOffers(w http.ResponseWriter, r *http.Request) {
	data := u.baseData("offers", r.URL.Query().Get("msg"), r.URL.Query().Get("err"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = offersTmpl.Execute(w, data)
}

// handleCollection renders the Collection landing page: a scan
// control bar at the top (root path + Scan button), a flash slot
// for the last error, and the persisted snapshot table beneath.
// All operator actions live here — POST /collection/scan kicks off
// a walk + enrich pass; POST /collection/edit updates one row's
// overrides.
// collectionFileView is the per-file row enriched with cascade-
// resolved effective values + the parent folder string. Computed
// once in the handler so the template doesn't have to call methods
// per row.
type collectionFileView struct {
	Item                CollectionItem
	Folder              string
	EffectiveAID        int
	EffectiveResolution string
	EffectiveSource     string
	EffectiveSeason     string
}

// collectionFolderView groups one folder's files with its override
// + selection state for the tree-view UI. Files inside are the
// pre-computed file views so the template can render either tree
// or flat from the same data.
type collectionFolderView struct {
	Folder    string
	Override  FolderOverride
	Files     []collectionFileView
	FileCount int
}

func (u *LocalUI) handleCollection(w http.ResponseWriter, r *http.Request) {
	data := u.baseData("collection", r.URL.Query().Get("msg"), r.URL.Query().Get("err"))
	data["CollectionRoot"] = u.cfg.CollectionRoot
	data["HasSite"] = u.site != nil
	data["IsScanning"] = u.collection.IsScanning()
	// Mount status: list candidate roots so the operator can see
	// what the container actually has access to before guessing
	// what to type. RootStatus tells them whether their configured
	// CollectionRoot is reachable; Candidates lists every /data/*
	// subdirectory the agent can stat — that's where bind mounts
	// from docker-compose land, plus the named-volume's own state
	// files.
	data["RootStatus"] = describeRoot(u.cfg.CollectionRoot)
	data["Candidates"] = listDataMountCandidates()
	// View mode toggle — tree (default, set folder once) vs flat
	// (per-file editing). Stored in the query string so a refresh
	// or link-share preserves the operator's choice.
	view := r.URL.Query().Get("view")
	if view != "flat" {
		view = "tree"
	}
	data["View"] = view
	if errMsg := u.collection.LastError(); errMsg != "" && data["Err"] == "" {
		data["Err"] = errMsg
	}
	if snap := u.collection.Snapshot(); snap != nil {
		data["Snapshot"] = snap
		// Compute the cascade-resolved view rows once + group by
		// folder for the tree mode. Same data feeds both views;
		// the template picks which structure to render.
		files := make([]collectionFileView, 0, len(snap.Items))
		groups := make(map[string]*collectionFolderView)
		for _, it := range snap.Items {
			folder := FolderOf(it)
			v := collectionFileView{
				Item:                it,
				Folder:              folder,
				EffectiveAID:        u.collection.EffectiveAID(it),
				EffectiveResolution: u.collection.EffectiveResolution(it),
				EffectiveSource:     u.collection.EffectiveSource(it),
				EffectiveSeason:     u.collection.EffectiveSeason(it),
			}
			files = append(files, v)
			g, ok := groups[folder]
			if !ok {
				g = &collectionFolderView{
					Folder:   folder,
					Override: snap.FolderOverrides[folder],
				}
				groups[folder] = g
			}
			g.Files = append(g.Files, v)
			g.FileCount++
		}
		// Stable folder ordering — alphabetical by relative path.
		folderList := make([]*collectionFolderView, 0, len(groups))
		for _, g := range groups {
			folderList = append(folderList, g)
		}
		sort.Slice(folderList, func(i, j int) bool { return folderList[i].Folder < folderList[j].Folder })
		data["Files"] = files
		data["Folders"] = folderList
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = collectionTmpl.Execute(w, data)
}

// rootStatusKind is the verdict for the operator's configured
// CollectionRoot, surfaced as a colour-coded badge above the scan
// field so they can tell at a glance whether to click Scan or fix
// their compose mount first.
type rootStatusKind string

const (
	rootStatusUnset   rootStatusKind = "unset"   // COLLECTION_ROOT empty
	rootStatusMissing rootStatusKind = "missing" // path doesn't exist
	rootStatusNotDir  rootStatusKind = "not_dir" // exists but isn't a directory
	rootStatusEmpty   rootStatusKind = "empty"   // dir exists but has no files (mount line probably not enabled)
	rootStatusOK      rootStatusKind = "ok"      // dir exists and has entries — looks mounted
)

// rootStatus is the {kind + free-text detail + entry count} bundle
// passed to the template. EntryCount is from ReadDir, capped at the
// first 1 sample for the cheap stat — we don't recursively walk.
type rootStatus struct {
	Kind   rootStatusKind
	Path   string
	Detail string
	Sample []string // first few entries for the operator to recognise
}

// describeRoot stats the path + reads its first few entries. Cheap:
// one Stat call + one ReadDir — no recursion. Surfaces enough info
// for the operator to recognise their own folder without leaking
// the full tree on every page render.
func describeRoot(path string) rootStatus {
	path = strings.TrimSpace(path)
	if path == "" {
		return rootStatus{
			Kind:   rootStatusUnset,
			Detail: "COLLECTION_ROOT not set — type a path or set it in .env / config.yml",
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return rootStatus{
			Kind:   rootStatusMissing,
			Path:   path,
			Detail: "not accessible inside the container — uncomment the COLLECTION_HOST_DIR mount line in docker-compose.yml",
		}
	}
	if !info.IsDir() {
		return rootStatus{
			Kind:   rootStatusNotDir,
			Path:   path,
			Detail: "exists but isn't a directory",
		}
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return rootStatus{
			Kind:   rootStatusMissing,
			Path:   path,
			Detail: "directory exists but isn't readable (permissions?)",
		}
	}
	if len(entries) == 0 {
		return rootStatus{
			Kind:   rootStatusEmpty,
			Path:   path,
			Detail: "directory exists but is empty — the host folder probably isn't mounted (check docker-compose.yml)",
		}
	}
	// Sample up to 4 entries so the operator can recognise their own
	// folder by name. Listing too many turns the status card into a
	// wall of text.
	sample := make([]string, 0, 4)
	for i, e := range entries {
		if i >= 4 {
			break
		}
		sample = append(sample, e.Name())
	}
	return rootStatus{
		Kind:   rootStatusOK,
		Path:   path,
		Detail: fmt.Sprintf("%d entries", len(entries)),
		Sample: sample,
	}
}

// listDataMountCandidates walks /data and returns every subdirectory
// the container can stat — that's the universe of paths the
// operator can target with Collection mode short of typing
// arbitrary in-container paths. Filters out agent-internal dirs so
// the suggestion list isn't cluttered with temp/, watch/, done/,
// offline-output/. Each candidate is a click-to-fill chip in the UI.
//
// Returns the same in-container paths the agent process sees (e.g.
// /data/collection), which is also exactly what the operator should
// type into the Scan field — so a click on a chip pre-fills the
// input without any path translation.
func listDataMountCandidates() []string {
	entries, err := os.ReadDir("/data")
	if err != nil {
		return nil
	}
	// Skip agent-internal directories — these are managed by the
	// agent itself, not mount targets for Collection.
	skip := map[string]bool{
		"temp":           true,
		"watch":          true,
		"done":           true,
		"offline-output": true,
		"backfill":       true,
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if skip[e.Name()] {
			continue
		}
		out = append(out, "/data/"+e.Name())
	}
	sort.Strings(out)
	return out
}

// handleCollectionScan kicks off a scan in a goroutine and redirects
// immediately so the operator's browser doesn't hang for what could
// be a multi-minute walk + enrich pass. The collection page polls
// itself via a meta-refresh while IsScanning() is true.
//
// Form fields:
//
//	root  — optional override of cfg.CollectionRoot. Empty falls
//	        back to the config value. Letting the operator type a
//	        path each scan is the simplest UX for an early-stage
//	        feature; a "save as default" follow-up can write through
//	        to the layered config when needed.
func (u *LocalUI) handleCollectionScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/collection", http.StatusSeeOther)
		return
	}
	root := strings.TrimSpace(r.FormValue("root"))
	if root == "" {
		root = u.cfg.CollectionRoot
	}
	// Fire-and-forget: the page redirect happens immediately and the
	// operator watches the result table populate on the next refresh.
	// IsScanning() guards against double-trigger so a quick browser
	// double-click doesn't spawn two walks of the same root.
	go func(root string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := u.collection.Scan(ctx, root); err != nil {
			log.Printf("collection scan error: %v", err)
		}
	}(root)
	redirectWithFlash(w, r, "/collection", "Scan started — refresh to see results", "")
}

// handleCollectionEdit applies a per-row override (anime id /
// resolution / source / season / episode). The whole row is
// submitted in one form so every cell is written together —
// avoids the "did I save resolution but not source" question.
// Best-effort: a stale path silently redirects without flashing —
// the typical reason is "the operator just re-scanned and this
// path isn't in the new snapshot."
func (u *LocalUI) handleCollectionEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/collection", http.StatusSeeOther)
		return
	}
	path := strings.TrimSpace(r.FormValue("path"))
	if path == "" {
		redirectWithFlash(w, r, "/collection", "", "Edit: path required")
		return
	}
	aid, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("aid")))
	resolution := strings.TrimSpace(r.FormValue("resolution"))
	source := strings.TrimSpace(r.FormValue("source"))
	season := strings.TrimSpace(r.FormValue("season"))
	episode := strings.TrimSpace(r.FormValue("episode"))
	if err := u.collection.UpdateOverrides(path, aid, resolution, source, season, episode); err != nil {
		redirectWithFlash(w, r, "/collection", "", "Edit: "+err.Error())
		return
	}
	redirectWithFlash(w, r, "/collection", "Row updated.", "")
}

// handleCollectionFolderEdit applies a folder-level metadata
// cascade. All files in the folder inherit these values unless
// they have their own per-file override. POST form: folder, aid,
// season, resolution, source. Empty fields clear the cascade
// entry (operator deliberately reverting to site hints).
func (u *LocalUI) handleCollectionFolderEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/collection", http.StatusSeeOther)
		return
	}
	folder := r.FormValue("folder")
	aid, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("aid")))
	season := strings.TrimSpace(r.FormValue("season"))
	resolution := strings.TrimSpace(r.FormValue("resolution"))
	source := strings.TrimSpace(r.FormValue("source"))
	if err := u.collection.SetFolderOverride(folder, aid, season, resolution, source); err != nil {
		redirectWithFlash(w, r, "/collection?view=tree", "", "Folder edit: "+err.Error())
		return
	}
	label := folder
	if label == "" {
		label = "(root)"
	}
	redirectWithFlash(w, r, "/collection?view=tree", "Folder "+label+" updated — all files inside inherit these values.", "")
}

// handleCollectionFolderSelect toggles the bulk-select flag on a
// folder. Each click POSTs the new state; QueueSelectedUploads
// reads FolderOverrides[folder].Selected when picking the rows to
// queue.
func (u *LocalUI) handleCollectionFolderSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/collection", http.StatusSeeOther)
		return
	}
	folder := r.FormValue("folder")
	selected := r.FormValue("selected") == "1"
	if err := u.collection.SelectFolder(folder, selected); err != nil {
		redirectWithFlash(w, r, "/collection?view=tree", "", "Folder select: "+err.Error())
		return
	}
	http.Redirect(w, r, "/collection?view=tree", http.StatusSeeOther)
}

// handleCollectionSelect toggles the Selected flag on one row.
// Wired to a per-row checkbox; each click POSTs the new state so
// the snapshot persists between page renders. Idempotent —
// re-asserting the same value is a no-op write.
func (u *LocalUI) handleCollectionSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/collection", http.StatusSeeOther)
		return
	}
	path := strings.TrimSpace(r.FormValue("path"))
	if path == "" {
		redirectWithFlash(w, r, "/collection", "", "Select: path required")
		return
	}
	selected := r.FormValue("selected") == "1"
	if err := u.collection.SetSelected(path, selected); err != nil {
		redirectWithFlash(w, r, "/collection", "", "Select: "+err.Error())
		return
	}
	// No flash on success — checkbox toggles are routine and a flash
	// banner per click would be visual noise.
	http.Redirect(w, r, "/collection", http.StatusSeeOther)
}

// handleCollectionUpload promotes every Selected row to UploadStatus
// "queued". Real upload backend (build NZB → post → report to site)
// lands in slice 5; today this just stages the state so operators
// can see selections flow into the Status pane.
func (u *LocalUI) handleCollectionUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/collection", http.StatusSeeOther)
		return
	}
	n, err := u.collection.QueueSelectedUploads()
	if err != nil {
		redirectWithFlash(w, r, "/collection", "", "Upload: "+err.Error())
		return
	}
	if n == 0 {
		redirectWithFlash(w, r, "/collection", "", "No rows selected — tick the checkbox on one or more rows first.")
		return
	}
	msg := fmt.Sprintf("Queued %d row(s) for upload. Worker lands in the next slice.", n)
	redirectWithFlash(w, r, "/collection", msg, "")
}

var offersTmpl = buildPageTemplate("offers", `
{{define "title"}}Offers{{end}}
{{define "content"}}
<div class="card">
  <div class="card-head">Offers — torrent / on-disk → usenet</div>
  <p>
    Offers let this agent advertise content it already has — files on disk
    or torrents in its BT client — as fulfilment sources for site requests.
    When a site user clicks "Get this from <em>your-agent</em>" on a request,
    the offer pipeline kicks in: download (or re-read the local copy),
    re-upload to usenet, hand the resulting NZB back to the site.
  </p>
  <p class="small" style="color:var(--text-muted);">
    The sync and fulfilment services are already running in the background
    (see <code>services/offer_*.go</code>). A dedicated queue + config UI
    lands in a follow-up slice; for now offers are managed from the site
    dashboard under <a href="{{.SiteURL}}/account/offers" target="_blank" rel="noopener">account / offers</a>.
  </p>
</div>
{{end}}
`, nil)

var collectionTmpl = buildPageTemplate("collection", `
{{define "title"}}Collection{{end}}
{{define "content"}}
{{if .IsScanning}}
{{/* While a scan is in flight the page polls itself every 4s so the
     operator sees the table populate without manual refreshes. The
     <meta> is the simplest path that doesn't need JS in the template
     for a feature this early. */}}
<meta http-equiv="refresh" content="4">
{{end}}

<div class="card">
  <div class="card-head">Collection — on-disk → usenet</div>
  <p class="small" style="color:var(--text-muted);margin:0 0 12px;">
    Point the agent at a folder; it walks the tree, asks the site to
    enrich each filename (AniDB title match + resolution / source
    hints from the same regex parser the ingest pipeline uses), and
    lets you review the rows before pushing each one to usenet plus
    an NZB push to the site.
  </p>

  {{/* Mount status — tells the operator whether their configured
       root is actually reachable inside the container BEFORE they
       click Scan. The colour codes mirror the alert classes used
       elsewhere: ok=green, missing/not_dir=red, empty=warn,
       unset=neutral. */}}
  {{with .RootStatus}}
  <div style="padding:8px 12px;border-radius:6px;margin-bottom:10px;font-size:12.5px;background:
    {{if eq .Kind "ok"}}rgba(34,197,94,0.08);border:1px solid rgba(34,197,94,0.3);color:var(--text-primary);
    {{else if eq .Kind "missing"}}rgba(239,68,68,0.08);border:1px solid rgba(239,68,68,0.3);color:var(--text-primary);
    {{else if eq .Kind "not_dir"}}rgba(239,68,68,0.08);border:1px solid rgba(239,68,68,0.3);color:var(--text-primary);
    {{else if eq .Kind "empty"}}rgba(245,158,11,0.08);border:1px solid rgba(245,158,11,0.3);color:var(--text-primary);
    {{else}}rgba(91,138,245,0.08);border:1px solid rgba(91,138,245,0.25);color:var(--text-primary);{{end}}">
    {{if eq .Kind "ok"}}
      <strong>✓ Mounted</strong>
      <code style="margin:0 6px;">{{.Path}}</code>
      <span class="small">— {{.Detail}}{{if .Sample}}; e.g. {{range $i, $e := .Sample}}{{if $i}}, {{end}}<code>{{$e}}</code>{{end}}{{end}}</span>
    {{else if eq .Kind "empty"}}
      <strong style="color:var(--warn);">⚠ Empty</strong>
      <code style="margin:0 6px;">{{.Path}}</code>
      <span class="small">— {{.Detail}}</span>
    {{else if eq .Kind "missing"}}
      <strong style="color:var(--red);">✕ Not mounted</strong>
      <code style="margin:0 6px;">{{.Path}}</code>
      <span class="small">— {{.Detail}}</span>
    {{else if eq .Kind "not_dir"}}
      <strong style="color:var(--red);">✕ Not a directory</strong>
      <code style="margin:0 6px;">{{.Path}}</code>
      <span class="small">— {{.Detail}}</span>
    {{else}}
      <strong style="color:var(--blue);">No root configured</strong>
      <span class="small" style="margin-left:8px;">— {{.Detail}}</span>
    {{end}}
  </div>
  {{end}}

  <form method="post" action="/collection/scan" style="display:flex;gap:8px;align-items:center;flex-wrap:wrap;">
    <input type="text" name="root" id="collection-root-input" value="{{.CollectionRoot}}" placeholder="/data/collection" style="flex:1;min-width:260px;padding:7px 10px;background:var(--bg-base);border:1px solid var(--border);border-radius:6px;color:var(--text-primary);">
    <button type="submit" class="primary" {{if .IsScanning}}disabled{{end}}>
      {{if .IsScanning}}Scanning…{{else}}Scan{{end}}
    </button>
    {{if not .HasSite}}<span class="badge warn" title="No site client configured — scan will list files without enrichment">no site</span>{{end}}
  </form>

  {{/* Candidate chips — every /data/* subdirectory the container
       can see (excluding agent-internal ones). Clicking pre-fills
       the Scan input so the operator never has to type the path. */}}
  {{if .Candidates}}
  <div class="small" style="margin-top:10px;color:var(--text-muted);">
    Detected in <code>/data/</code>:
    {{range .Candidates}}
    <button type="button" onclick="document.getElementById('collection-root-input').value='{{.}}';" style="margin:0 4px 4px 0;padding:3px 8px;border:1px solid var(--border);background:var(--bg-elevated);color:var(--text-primary);border-radius:14px;cursor:pointer;font-size:11.5px;font-family:inherit;">{{.}}</button>
    {{end}}
  </div>
  {{end}}
  {{if .Snapshot}}
  <p class="small" style="margin:10px 0 0;color:var(--text-muted);">
    {{.Snapshot.TotalCount}} item{{if ne .Snapshot.TotalCount 1}}s{{end}}
    in <code>{{.Snapshot.ScanRoot}}</code>
    · scanned {{.Snapshot.ScannedAt.Local.Format "Jan 02 15:04:05"}}
  </p>
  {{end}}
</div>

{{if .Snapshot}}
{{if .Snapshot.Items}}

{{/* View toggle — tree (set folder once, all files inherit) vs
     flat (per-file editor). Tree is the default since anime
     libraries are folder-organised; flat is the override for
     mixed bags or per-file fixups. */}}
<div style="display:flex;justify-content:space-between;align-items:center;margin-top:16px;margin-bottom:8px;flex-wrap:wrap;gap:8px;">
  <div style="display:inline-flex;border:1px solid var(--border);border-radius:6px;overflow:hidden;">
    <a href="/collection?view=tree" style="padding:6px 12px;text-decoration:none;font-size:12.5px;{{if eq .View "tree"}}background:var(--blue);color:#fff;{{else}}background:transparent;color:var(--text-muted);{{end}}">
      Tree — all in folder
    </a>
    <a href="/collection?view=flat" style="padding:6px 12px;text-decoration:none;font-size:12.5px;border-left:1px solid var(--border);{{if eq .View "flat"}}background:var(--blue);color:#fff;{{else}}background:transparent;color:var(--text-muted);{{end}}">
      Flat — each file
    </a>
  </div>
  <form method="post" action="/collection/upload" id="collection-upload-form" style="margin:0;">
    <button type="submit" class="primary">Upload selected</button>
  </form>
</div>
<div class="small" style="color:var(--text-muted);margin-bottom:10px;">
  {{if eq .View "tree"}}
    Set anime + season + resolution + source once per folder; every file inside inherits.
    Per-file overrides (in flat view) win over folder defaults if you need an exception.
  {{else}}
    One row per file — edit every field individually. Useful for mixed-bag folders or one-off fixups
    on top of a folder cascade.
  {{end}}
</div>

{{if eq .View "tree"}}
{{/* ── Tree view ── one editable card per folder.
     The row's cascade (aid/season/resolution/source) applies to
     every file inside unless that file has its own per-file
     override (set via the flat view). Folder checkbox queues the
     entire folder; a small per-file preview lists the first few
     filenames so the operator recognises the group. */}}
{{range .Folders}}
{{$folder := .Folder}}
{{$ov := .Override}}
<div style="margin-top:10px;padding:10px 12px;background:var(--bg-surface);border:1px solid var(--border);border-radius:6px;">
  <div style="display:flex;align-items:center;gap:10px;margin-bottom:8px;">
    <form method="post" action="/collection/folder-select" class="row-form">
      <input type="hidden" name="folder" value="{{$folder}}">
      <input type="hidden" name="selected" value="{{if $ov.Selected}}0{{else}}1{{end}}">
      <input type="checkbox" {{if $ov.Selected}}checked{{end}} onchange="this.form.submit();" title="Queue every file in this folder for upload">
    </form>
    <strong style="font-size:13px;">
      {{if eq $folder ""}}<em>(root)</em>{{else}}{{$folder}}{{end}}
    </strong>
    <span class="small" style="color:var(--text-muted);">— {{.FileCount}} file{{if ne .FileCount 1}}s{{end}}</span>
  </div>

  <form method="post" action="/collection/folder-edit" style="display:grid;grid-template-columns:1fr 1fr 1fr 1fr auto;gap:8px;align-items:end;">
    <input type="hidden" name="folder" value="{{$folder}}">
    <label style="font-size:11px;color:var(--text-muted);">
      Anime ID
      <input type="number" name="aid" value="{{if $ov.AID}}{{$ov.AID}}{{end}}" placeholder="aid"
             style="display:block;width:100%;padding:4px 6px;background:var(--bg-base);border:1px solid var(--border);border-radius:4px;color:var(--text-primary);font-size:12.5px;margin-top:2px;">
    </label>
    <label style="font-size:11px;color:var(--text-muted);">
      Season
      <input type="text" name="season" value="{{$ov.Season}}" placeholder="01"
             style="display:block;width:100%;padding:4px 6px;background:var(--bg-base);border:1px solid var(--border);border-radius:4px;color:var(--text-primary);font-size:12.5px;margin-top:2px;">
    </label>
    <label style="font-size:11px;color:var(--text-muted);">
      Resolution
      <input type="text" name="resolution" value="{{$ov.Resolution}}" placeholder="1080p"
             style="display:block;width:100%;padding:4px 6px;background:var(--bg-base);border:1px solid var(--border);border-radius:4px;color:var(--text-primary);font-size:12.5px;margin-top:2px;">
    </label>
    <label style="font-size:11px;color:var(--text-muted);">
      Source
      <input type="text" name="source" value="{{$ov.Source}}" placeholder="WEB-DL"
             style="display:block;width:100%;padding:4px 6px;background:var(--bg-base);border:1px solid var(--border);border-radius:4px;color:var(--text-primary);font-size:12.5px;margin-top:2px;">
    </label>
    <button type="submit" class="primary" style="font-size:11.5px;padding:4px 10px;">Save folder</button>
  </form>

  {{/* File preview — first 5 names, with cascade-resolved values
       visible below each. Operators recognise the group, see
       what the cascade ended up doing, and have a quick link to
       the flat-view for per-file fixups. */}}
  <div class="small" style="color:var(--text-muted);margin-top:10px;">
    {{range $i, $f := .Files}}{{if lt $i 5}}
    <div style="padding:2px 0;">
      <code>{{$f.Item.Filename}}</code>
      {{if $f.EffectiveAID}}<span style="margin-left:6px;">aid {{$f.EffectiveAID}}</span>{{end}}
      {{if $f.EffectiveSeason}}<span style="margin-left:6px;">S{{$f.EffectiveSeason}}</span>{{end}}
      {{if $f.EffectiveResolution}}<span style="margin-left:6px;">{{$f.EffectiveResolution}}</span>{{end}}
      {{if $f.EffectiveSource}}<span style="margin-left:6px;">{{$f.EffectiveSource}}</span>{{end}}
    </div>
    {{end}}{{end}}
    {{if gt .FileCount 5}}<div style="padding:2px 0;font-style:italic;">… and {{sub .FileCount 5}} more</div>{{end}}
  </div>
</div>
{{end}}
{{else}}
<table style="margin-top:4px;font-size:12.5px;">
<thead>
<tr>
  <th style="width:30px;"></th>
  <th>File</th>
  <th style="width:80px;">Size</th>
  <th style="width:170px;">Anime</th>
  <th style="width:80px;">Season</th>
  <th style="width:80px;">Episode</th>
  <th style="width:100px;">Resolution</th>
  <th style="width:110px;">Source</th>
  <th style="width:70px;"></th>
</tr>
</thead>
<tbody>
{{range .Snapshot.Items}}
{{$aidVal := .AID}}{{if .OverrideAID}}{{$aidVal = .OverrideAID}}{{end}}
{{$rezVal := .ResolutionHint}}{{if .OverrideResolution}}{{$rezVal = .OverrideResolution}}{{end}}
{{$srcVal := .SourceHint}}{{if .OverrideSource}}{{$srcVal = .OverrideSource}}{{end}}
<tr>
  {{/* Selection checkbox — its own tiny form so a toggle saves
       without dragging the whole row's edits along. The form
       attribute on the checkbox is implicit (closest enclosing
       <form>), but here we use a standalone form per row. */}}
  <td>
    <form method="post" action="/collection/select" class="row-form">
      <input type="hidden" name="path" value="{{.Path}}">
      <input type="hidden" name="selected" value="{{if .Selected}}0{{else}}1{{end}}">
      <input type="checkbox" {{if .Selected}}checked{{end}} onchange="this.form.submit();">
    </form>
  </td>
  <td>
    <div>{{.Filename}}</div>
    <div class="small" style="color:var(--text-muted);">{{.RelPath}}</div>
  </td>
  <td class="small">{{humanBytes .SizeBytes}}</td>

  {{/* Per-row edit form covers anime/season/episode/resolution/source.
       Inputs in this row that share form="row-{{.Path}}" submit
       together when the Save button is clicked. */}}
  <td>
    <input type="number" form="row-form-{{$aidVal}}-{{.Filename}}" name="aid" value="{{if $aidVal}}{{$aidVal}}{{end}}" placeholder="aid"
           style="width:80px;padding:4px 6px;background:var(--bg-base);border:1px solid var(--border);border-radius:4px;color:var(--text-primary);font-size:12px;">
    {{if .Matched}}
    <div class="small" style="color:var(--text-muted);">
      <a href="{{$.SiteURL}}/anime/{{.AID}}" target="_blank" rel="noopener" style="color:var(--blue);">{{.AnimeTitle}}</a>
    </div>
    {{end}}
  </td>
  <td>
    <input type="text" form="row-form-{{$aidVal}}-{{.Filename}}" name="season" value="{{.OverrideSeason}}" placeholder="—"
           style="width:70px;padding:4px 6px;background:var(--bg-base);border:1px solid var(--border);border-radius:4px;color:var(--text-primary);font-size:12px;">
  </td>
  <td>
    <input type="text" form="row-form-{{$aidVal}}-{{.Filename}}" name="episode" value="{{.OverrideEpisode}}" placeholder="—"
           style="width:70px;padding:4px 6px;background:var(--bg-base);border:1px solid var(--border);border-radius:4px;color:var(--text-primary);font-size:12px;">
  </td>
  <td>
    <input type="text" form="row-form-{{$aidVal}}-{{.Filename}}" name="resolution" value="{{$rezVal}}" placeholder="—"
           style="width:90px;padding:4px 6px;background:var(--bg-base);border:1px solid var(--border);border-radius:4px;color:var(--text-primary);font-size:12px;">
  </td>
  <td>
    <input type="text" form="row-form-{{$aidVal}}-{{.Filename}}" name="source" value="{{$srcVal}}" placeholder="—"
           style="width:100px;padding:4px 6px;background:var(--bg-base);border:1px solid var(--border);border-radius:4px;color:var(--text-primary);font-size:12px;">
  </td>
  <td>
    {{/* The hidden path input + Save button live in a TINY form
         outside the table cells via the form attribute trick.
         Multiple <input form="..."> elements submit together. */}}
    <form method="post" action="/collection/edit" id="row-form-{{$aidVal}}-{{.Filename}}" class="row-form">
      <input type="hidden" name="path" value="{{.Path}}">
      <button type="submit" class="primary" style="font-size:11px;padding:3px 8px;">Save row</button>
    </form>
  </td>
</tr>
{{end}}
</tbody>
</table>
{{end}}{{/* /if eq .View "tree" — else flat */}}

{{/* ── Status pane — bottom half ─────────────────────────────────
     Shows every row whose UploadStatus has been set (queued /
     uploading / done / failed). Today populated only by Queue —
     the upload worker that flips queued→uploading→done lands in
     slice 5. */}}
<div style="margin-top:24px;padding-top:16px;border-top:1px solid var(--border);">
  <h3 style="font-size:14px;font-weight:600;margin:0 0 8px;">Upload status</h3>
  {{$hasStatus := false}}
  {{range .Snapshot.Items}}{{if .UploadStatus}}{{$hasStatus = true}}{{end}}{{end}}
  {{if $hasStatus}}
  <table style="font-size:12.5px;">
  <thead>
  <tr>
    <th>File</th>
    <th style="width:120px;">Status</th>
    <th>Detail</th>
  </tr>
  </thead>
  <tbody>
  {{range .Snapshot.Items}}{{if .UploadStatus}}
  <tr>
    <td>
      <div>{{.Filename}}</div>
      <div class="small" style="color:var(--text-muted);">{{.RelPath}}</div>
    </td>
    <td>
      {{if eq .UploadStatus "queued"}}
        <span class="badge default">queued</span>
      {{else if eq .UploadStatus "uploading"}}
        <span class="badge yml">uploading</span>
      {{else if eq .UploadStatus "done"}}
        <span class="badge ok">done</span>
      {{else if eq .UploadStatus "failed"}}
        <span class="badge err">failed</span>
      {{else}}
        <span class="badge default">{{.UploadStatus}}</span>
      {{end}}
    </td>
    <td class="small">{{.UploadDetail}}</td>
  </tr>
  {{end}}{{end}}
  </tbody>
  </table>
  {{else}}
  <p class="small" style="color:var(--text-muted);">
    No uploads in progress. Tick a row's checkbox and click <em>Upload selected</em> to queue it.
    The upload worker (NZB build + usenet post + site report) lands in the next slice — until then,
    queued rows just stay in this list.
  </p>
  {{end}}
</div>

{{else}}
<p class="small" style="margin-top:16px;">Scan returned no candidates. Check the root path or extension filter.</p>
{{end}}
{{else}}
<p class="small" style="margin-top:16px;">No scan yet. Set a root above and click Scan.</p>
{{end}}
{{end}}
`, collectionTmplFuncs)

// collectionTmplFuncs exposes humanBytes for the size column.
// Pulled out of the template literal so it can be unit-tested
// independently when we add tests for the size formatting tier
// thresholds.
var collectionTmplFuncs = template.FuncMap{
	"humanBytes": humanBytes,
	// sub is the "and N more" subtraction helper used by the tree
	// view's file preview ("… and 7 more"). html/template doesn't
	// expose arithmetic operators so we hand it a tiny func.
	"sub": func(a, b int) int { return a - b },
}

// humanBytes formats a byte count with binary-prefix units. Tracks
// the convention used in the Mirror /jobs page so the operator sees
// consistent sizes across modes.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for nn := n / unit; nn >= unit; nn /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// redirectWithFlash is the shared tail for all CRUD handlers — POST-Redirect-GET
// with a single query param carrying the success or error message. Keeps
// flash state out of sessions/cookies, which the local UI doesn't have.
func redirectWithFlash(w http.ResponseWriter, r *http.Request, path, msg, errMsg string) {
	q := ""
	if msg != "" {
		q = "?msg=" + template.URLQueryEscaper(msg)
	} else if errMsg != "" {
		q = "?err=" + template.URLQueryEscaper(errMsg)
	}
	http.Redirect(w, r, path+q, http.StatusSeeOther)
}

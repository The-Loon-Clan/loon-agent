package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Layered keeps the three sources that combine into the agent's effective
// config visible separately, so we can:
//
//   - report the *local* snapshot (yml + env) back to the site for display
//     alongside any web-override the user set;
//   - apply web overrides arriving in a poll response without losing the
//     underlying local values;
//   - write edits back to config.yml when the user clicks "Save to agent
//     config" in the web UI.
//
// Keys are free-form strings matching the snake-case names used in the site
// DB (see migration 143). The legacy *Config struct is still populated from
// the effective tier for the rest of the code that already reads it.
type Layered struct {
	mu       sync.RWMutex
	yml      map[string]string // from config.yml on disk
	env      map[string]string // from process environment
	web      map[string]string // pushed down from the site
	ymlPath  string
	writable bool
}

// Torrent-related keys that live in config.yml (not .env). Extend as new
// settings are added. Kept as a flat list so the settings UI can render
// them without code changes per key.
var ConfigYmlKeys = []string{
	// torrent
	"torrent_max_upload_kbps",
	"torrent_seed_ratio",
	"torrent_seed_hours",
	"torrent_require_full_seed",
	"torrent_no_full_seed_timeout_mins",
	"torrent_port",
	// concurrency / throttle
	"max_concurrent_downloads",
	"cpu_max_percent",
	"max_disk_usage_gb",
	// slow-download rules
	"slow_speed_threshold_mbs",
	"slow_speed_timeout_mins",
	"low_peers_threshold",
	"low_peers_timeout_mins",
	// local UI
	"local_ui_port",
	"local_ui_bind",
}

// EnvOnlyKeys live in .env and are never written through the web UI. Kept
// as a denylist so the save-to-config handler can refuse to write them to
// config.yml even if a caller asks.
var EnvOnlyKeys = map[string]struct{}{
	"site_url":          {},
	"agent_token":       {},
	"vpn_provider":      {},
	"vpn_download_only": {},
	"vpn_proxy_addr":    {},
	"nntp_server":       {},
	"nntp_ssl":          {},
	"nntp_connections":  {},
	"nntp_user":         {},
	"nntp_pass":         {},
	"nntp_group":        {},
	"nntp_poster":       {},
	"nntp_from":         {},
	"nntp_domain":       {},
}

// NewLayered loads config.yml (if present), records the env values for the
// known keys, and probes whether the yml path is writable. Missing files are
// fine — the caller still gets a usable, empty yml tier.
func NewLayered(ymlPath string) *Layered {
	l := &Layered{
		yml:     map[string]string{},
		env:     map[string]string{},
		web:     map[string]string{},
		ymlPath: ymlPath,
	}
	l.loadYml()
	l.loadEnv()
	l.probeWritable()
	return l
}

func (l *Layered) loadYml() {
	f, err := os.Open(l.ymlPath)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(line[:i]))
		v := strings.TrimSpace(line[i+1:])
		v = strings.Trim(v, `"'`)
		l.yml[k] = v
	}
}

func (l *Layered) loadEnv() {
	for _, k := range ConfigYmlKeys {
		if v, ok := os.LookupEnv(strings.ToUpper(k)); ok {
			l.env[k] = v
		}
	}
	// .env-scoped keys are picked up by the legacy Config loader directly;
	// we don't mirror them here.
}

func (l *Layered) probeWritable() {
	// Try opening for append without truncating — if the file doesn't exist,
	// try creating it so first-time users still get a writable tier.
	f, err := os.OpenFile(l.ymlPath, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		if os.IsNotExist(err) {
			// Check parent directory writability instead.
			dir := filepath.Dir(l.ymlPath)
			probe := filepath.Join(dir, ".write-probe")
			pf, perr := os.Create(probe)
			if perr == nil {
				pf.Close()
				os.Remove(probe)
				l.writable = true
			}
			return
		}
		return
	}
	f.Close()
	l.writable = true
}

// Writable reports whether config.yml (or its parent dir) accepts writes.
func (l *Layered) Writable() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.writable
}

// Effective returns the merged value for a key: web > env > yml > "".
func (l *Layered) Effective(key string) string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if v, ok := l.web[key]; ok && v != "" {
		return v
	}
	if v, ok := l.env[key]; ok && v != "" {
		return v
	}
	if v, ok := l.yml[key]; ok {
		return v
	}
	return ""
}

// LocalSnapshot returns yml ∪ env values the site should display as the
// "local" tier. env takes precedence over yml at the same key.
func (l *Layered) LocalSnapshot() map[string]LocalValue {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]LocalValue, len(l.yml)+len(l.env))
	for k, v := range l.yml {
		out[k] = LocalValue{Value: v, Source: "yml"}
	}
	for k, v := range l.env {
		out[k] = LocalValue{Value: v, Source: "env"}
	}
	return out
}

// LocalValue is one entry in the snapshot the agent posts to the site.
type LocalValue struct {
	Value  string `json:"value"`
	Source string `json:"source"` // "yml" or "env"
}

// WebSnapshot returns a copy of the current web-tier overrides — the
// key/value map the site applied via the last GetConfig poll. Used by
// the local UI to render which overrides are in effect so the operator
// can see/clear them without opening the site's admin dashboard.
func (l *Layered) WebSnapshot() map[string]string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]string, len(l.web))
	for k, v := range l.web {
		out[k] = v
	}
	return out
}

// ApplyWeb replaces the web-override tier. An empty map clears all overrides
// (used when the site processes "reset web settings").
func (l *Layered) ApplyWeb(m map[string]string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.web = map[string]string{}
	for k, v := range m {
		l.web[strings.ToLower(k)] = v
	}
}

// WriteYml persists the given key/value pairs to config.yml on disk by
// merging them into the existing yml tier and rewriting the file. Keys in
// EnvOnlyKeys are rejected so the web UI can't leak .env-scoped settings
// into the on-disk config. Returns the set of keys actually written.
func (l *Layered) WriteYml(updates map[string]string) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.writable {
		return nil, fmt.Errorf("config.yml is not writable (%s)", l.ymlPath)
	}
	written := make([]string, 0, len(updates))
	for k, v := range updates {
		kk := strings.ToLower(k)
		if _, denied := EnvOnlyKeys[kk]; denied {
			continue
		}
		l.yml[kk] = v
		written = append(written, kk)
	}
	sort.Strings(written)
	keys := make([]string, 0, len(l.yml))
	for k := range l.yml {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# indexer-agent config.yml — edit with care.\n")
	b.WriteString("# Writes from the site UI or the local UI overwrite this file in place.\n")
	b.WriteString("# Keys in .env (VPN_*, NNTP_*, SITE_URL, AGENT_TOKEN) are NOT managed here.\n\n")
	for _, k := range keys {
		v := l.yml[k]
		if strings.ContainsAny(v, ":#\"'\n") {
			b.WriteString(fmt.Sprintf("%s: %q\n", k, v))
		} else {
			b.WriteString(fmt.Sprintf("%s: %s\n", k, v))
		}
	}
	tmp := l.ymlPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, l.ymlPath); err != nil {
		return nil, err
	}
	return written, nil
}

// SettingsReport is what the agent posts to the site on each poll so the
// UI can render state badges without the site having to track sources.
// HasPrivateTrackers + LocalUIURL let the site's UI enable the private
// tracker toggle and link to the agent's local-edit page; neither the
// passkeys nor the UI auth token ever leave the agent.
type SettingsReport struct {
	OnDiskWritable     bool   `json:"on_disk_writable"`
	HasPrivateTrackers bool   `json:"has_private_trackers"`
	LocalUIURL         string `json:"local_ui_url"`
	// GpuInfo + UpscaleModels advertise the agent's AI-upscale capability
	// (GPU description + the model keys it has bundled). Empty on a
	// CPU-only agent. The site stores these on agent_config so the
	// operator can opt in and the request dropdown can offer only
	// models some capable agent can run.
	GpuInfo       string                `json:"gpu_info"`
	UpscaleModels []string              `json:"upscale_models"`
	YmlPath       string                `json:"yml_path"`
	Local         map[string]LocalValue `json:"local"`
}

// Report snapshots the layered state for posting to the site. Extras is
// non-nil when the caller wants to merge additional signals (private
// tracker presence, local UI URL) into the report without Layered itself
// having to know about them.
func (l *Layered) Report(extras ...ReportExtra) SettingsReport {
	r := SettingsReport{
		OnDiskWritable: l.Writable(),
		YmlPath:        l.ymlPath,
		Local:          l.LocalSnapshot(),
	}
	for _, e := range extras {
		e(&r)
	}
	return r
}

// ReportExtra lets call sites annotate the SettingsReport with signals
// that don't belong in the layered map itself (agent-local capabilities).
type ReportExtra func(*SettingsReport)

func WithPrivateTrackers(v bool) ReportExtra {
	return func(r *SettingsReport) { r.HasPrivateTrackers = v }
}
func WithLocalUIURL(v string) ReportExtra { return func(r *SettingsReport) { r.LocalUIURL = v } }
func WithGpuInfo(v string) ReportExtra    { return func(r *SettingsReport) { r.GpuInfo = v } }
func WithUpscaleModels(v []string) ReportExtra {
	return func(r *SettingsReport) { r.UpscaleModels = v }
}

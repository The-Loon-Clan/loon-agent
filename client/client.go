package client

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/the-loon-clan/loon-agent/config"
	"github.com/google/uuid"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// AgentTask is the task payload returned by the site when polling.
type AgentTask struct {
	RequestID  int64  `json:"request_id"`
	LockID     int    `json:"lock_id"`
	Title      string `json:"title"`
	NyaaURL    string `json:"nyaa_url,omitempty"`
	InfoHash   string `json:"info_hash,omitempty"`
	Category   string `json:"category,omitempty"`
	Season     string `json:"season,omitempty"`
	Episodes   string `json:"episodes,omitempty"`
	BoostCount int    `json:"boost_count,omitempty"`

	// Private tells the agent to fetch the .torrent file from
	// TorrentFileURL (a site-relative path) instead of resolving the info
	// hash via magnet/DHT. Set by the site when the uploading user marked
	// the request as private — the agent must also skip any public-tracker
	// injection to keep private-tracker passkeys from leaking.
	Private        bool   `json:"private,omitempty"`
	TorrentFileURL string `json:"torrent_file_url,omitempty"`

	// RemuxOption is the per-request post-download transform. Set by
	// the site dispatcher; gated server-side on per-agent capability
	// flags so a remux/convert-incapable agent always sees "" or
	// "none" here. Values:
	//   ""       / "none"  → no transform, deliver raw payload
	//   "remux"            → mkvmerge stream-copy, deliver MKV(s), drop raw
	//   "both"             → mkvmerge stream-copy, keep raw payload + MKV(s)
	//   "convert_h264"     → ffmpeg re-encode to H.264 (libx264 CRF 21 slow)
	//   "convert_h265"     → ffmpeg re-encode to H.265 (libx265 CRF 23 medium)
	//   "convert_av1"      → ffmpeg re-encode to AV1   (libsvtav1 CRF 30 preset 6)
	// The stream-copy values gate on agent_config.remux_bluray = TRUE;
	// the convert_* values gate on agent_config.convert_video = TRUE.
	// services.RunRemux dispatches into the right pipeline based on
	// the prefix.
	RemuxOption string `json:"remux_option,omitempty"`

	// UpscaleOption is the per-request AI upscale model key (e.g.
	// "upscale_anime_2x"), or "" for no upscale. Set by the dispatcher
	// and gated server-side on agent_config.ai_upscale, so an incapable
	// agent always sees "" here. services.RunUpscale (Phase 2) acts on
	// it post-download, parallel to the RemuxOption transform.
	UpscaleOption string `json:"upscale_option,omitempty"`
}

// SiteClient communicates with the indexer site via HTTP.
type SiteClient struct {
	baseURL string
	token   string
	http    *http.Client
	// lastOKNano is the UnixNano timestamp of the most recent RoundTrip
	// that returned a response (any status). Populated by the transport
	// wrapper below and read by the watchdog to detect extended
	// site-unreachability (e.g. VPN tunnel dropped, DNS broken).
	lastOKNano atomic.Int64
}

// versionHeaderTransport adds X-Agent-Protocol and X-Agent-Version to every
// outbound request so the site can gate agents below its required minimum
// protocol version, and records the timestamp of any successful roundtrip
// so the watchdog can act on sustained network failure.
type versionHeaderTransport struct {
	inner  http.RoundTripper
	client *SiteClient
}

func (t *versionHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("X-Agent-Protocol", fmt.Sprintf("%d", AgentProtocolVersion))
	req.Header.Set("X-Agent-Version", AgentVersion)
	// Generate a per-request ID and set it as X-Request-ID so the site
	// can stamp the same value on every log line it emits while handling
	// this request. Logged locally too so the agent log carries the ID
	// before the request goes on the wire — cross-machine correlation
	// works even when one side's logs are missing (request_lock.fail_reason
	// → docker log line, no timestamp matching needed).
	reqID := uuid.New().String()
	req.Header.Set("X-Request-ID", reqID)
	log.Printf("client: %s %s X-Request-ID=%s", req.Method, req.URL.Path, reqID)
	resp, err := t.inner.RoundTrip(req)
	if err == nil && t.client != nil {
		t.client.lastOKNano.Store(time.Now().UnixNano())
	}
	return resp, err
}

// New creates a SiteClient from config.
func New(cfg *config.Config) *SiteClient {
	c := &SiteClient{
		baseURL: cfg.SiteURL,
		token:   cfg.AgentToken,
	}
	// Seed lastOK with the current time so the watchdog doesn't fire on
	// startup before any request has had a chance to complete.
	c.lastOKNano.Store(time.Now().UnixNano())
	c.http = &http.Client{
		Timeout:   120 * time.Second,
		Transport: &versionHeaderTransport{inner: http.DefaultTransport, client: c},
	}
	return c
}

// LastOK returns the timestamp of the most recent successful HTTP roundtrip
// to the site. A roundtrip is "successful" if the transport got a response
// (any status code), which is enough to prove DNS + TCP + TLS all worked.
func (c *SiteClient) LastOK() time.Time {
	return time.Unix(0, c.lastOKNano.Load())
}

// UpgradeRequiredError is returned when the site refuses the agent because
// its reported X-Agent-Protocol is below the site's minimum. The operator
// should update the binary; there is no retry loop that can recover from
// this on its own.
type UpgradeRequiredError struct {
	MinProtocol int
	Message     string
}

func (e *UpgradeRequiredError) Error() string {
	return fmt.Sprintf("agent upgrade required: site needs protocol v%d (this agent is v%d) — %s",
		e.MinProtocol, AgentProtocolVersion, e.Message)
}

// IsUpgradeRequired reports whether err is an UpgradeRequiredError.
func IsUpgradeRequired(err error) (*UpgradeRequiredError, bool) {
	var ue *UpgradeRequiredError
	if errors.As(err, &ue) {
		return ue, true
	}
	return nil, false
}

// parseUpgradeRequired decodes a 426 response body into an UpgradeRequiredError.
// Accepts {"min_protocol":N,"message":"..."} or falls back to the raw body.
func parseUpgradeRequired(body []byte) *UpgradeRequiredError {
	var m struct {
		MinProtocol int    `json:"min_protocol"`
		Message     string `json:"message"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(body, &m)
	msg := m.Message
	if msg == "" {
		msg = m.Error
	}
	if msg == "" {
		msg = string(body)
	}
	return &UpgradeRequiredError{MinProtocol: m.MinProtocol, Message: msg}
}

// PollResult holds the response from a poll request.
type PollResult struct {
	Task    *AgentTask
	Reason  string // why no task was returned (empty if task assigned)
	Command string // "stop" if agent should stop
}

// Poll checks the site for available work.
func (c *SiteClient) Poll() (*PollResult, error) {
	req, err := http.NewRequest("POST", c.baseURL+"/api/agent/poll", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("poll request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return &PollResult{Reason: "no content (legacy 204)"}, nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("unauthorized — check your AGENT_TOKEN")
	}
	if resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("BLOCKED: %s (approve new IP in Account Settings on the site)", body)
	}
	if resp.StatusCode == http.StatusUpgradeRequired {
		body, _ := io.ReadAll(resp.Body)
		return nil, parseUpgradeRequired(body)
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		var m MaintenanceResponse
		if json.Unmarshal(body, &m) == nil && m.Maintenance {
			return nil, &MaintenanceError{Info: m}
		}
		return nil, fmt.Errorf("poll returned 503: %s", body)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("poll returned %d: %s", resp.StatusCode, body)
	}

	// Parse the response — could be a task or a reason/command.
	var raw struct {
		AgentTask
		Reason  string `json:"reason"`
		Command string `json:"command"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode poll response: %w", err)
	}

	if raw.Command != "" {
		return &PollResult{Command: raw.Command}, nil
	}
	if raw.Reason != "" {
		return &PollResult{Reason: raw.Reason}, nil
	}
	if raw.RequestID == 0 {
		return &PollResult{Reason: "empty response (no request_id)"}, nil
	}
	task := raw.AgentTask
	return &PollResult{Task: &task}, nil
}

// FetchCachedTorrentByInfoHash asks the site for a server-pre-fetched
// .torrent blob keyed by info_hash. Returns (nil, nil) on a 404 (no
// cache entry yet — the caller falls back to its own DHT lookup), or
// (nil, err) on a real failure (network, auth, parse). The site's
// metadata-prefetch worker populates these in the background; using
// the cache lets us skip the 2-minute DHT round-trip when it's there.
func (c *SiteClient) FetchCachedTorrentByInfoHash(infoHash string) ([]byte, error) {
	url := fmt.Sprintf("%s/api/agent/cached-torrent/%s", c.baseURL, infoHash)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("cached-torrent fetch returned %d: %s", resp.StatusCode, body)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 10<<20))
}

// FetchTorrentFile downloads a .torrent file from the site. urlPath is the
// path returned in AgentTask.TorrentFileURL (e.g. "/api/agent/torrent/42").
// Absolute URLs are also accepted, in case the site ever starts returning
// them. The Authorization header is attached so the site can re-verify the
// caller owns the referenced lock.
func (c *SiteClient) FetchTorrentFile(urlPath string) ([]byte, error) {
	fullURL := urlPath
	if !strings.HasPrefix(fullURL, "http://") && !strings.HasPrefix(fullURL, "https://") {
		fullURL = c.baseURL + fullURL
	}
	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("torrent file not found (site returned 404 — lock may have expired)")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("torrent fetch returned %d: %s", resp.StatusCode, body)
	}
	// Bound the read so a malicious or corrupted site response can't eat
	// all our memory. 10 MB matches the server-side upload cap.
	return io.ReadAll(io.LimitReader(resp.Body, 10<<20))
}

// RemoteConfig holds server-side agent configuration fetched from the site.
// WebOverrides is the key/value map from the agent_config_web table — only
// keys present here are applied as the web tier on the agent's Layered
// config (so a missing key falls through to env/yml).
type RemoteConfig struct {
	MaxConcurrent      int     `json:"max_concurrent"` // 0 = use local default
	CpuMaxPercent      int     `json:"cpu_max_percent"`
	MaxDiskUsageGB     float64 `json:"max_disk_usage_gb"`    // 0 = no limit
	SlowSpeedThreshold float64 `json:"slow_speed_threshold"` // MB/s
	SlowSpeedTimeout   int     `json:"slow_speed_timeout"`   // minutes
	LowPeersThreshold  int     `json:"low_peers_threshold"`  // skip if seeds <= this
	LowPeersTimeout    int     `json:"low_peers_timeout"`    // minutes
	// BannedExtensions is the operator-configured blocklist for the
	// post-download cleanup sweep on the online path. Non-empty
	// REPLACES the agent's hardcoded DefaultBlockedExtensions —
	// services.EffectiveBlocklist handles the empty-fallback. Values
	// are bare extensions ("iso", "exe"); the agent re-adds the dot
	// at lookup time. Lets operators drop .iso (for Bluray remux,
	// say) without a client redeploy.
	BannedExtensions []string `json:"banned_extensions,omitempty"`
	// RemuxBluray mirrors agent_config.remux_bluray. Set by the
	// operator on /agent/<id>; the dispatcher already gates remux-
	// required jobs upstream so the agent doesn't strictly need to
	// re-check this, but it's exposed here for the local UI.
	RemuxBluray  bool              `json:"remux_bluray,omitempty"`
	WebOverrides map[string]string `json:"web_overrides,omitempty"`
}

// PostLocalConfig uploads the agent's local snapshot (yml + env values for
// the known tunable keys) so the settings UI can show state badges and
// compare against any web-override the user has set.
func (c *SiteClient) PostLocalConfig(report config.SettingsReport) error {
	body, _ := json.Marshal(report)
	req, err := http.NewRequest("POST", c.baseURL+"/api/agent/local-config", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// PutWebConfig writes (or clears) a single web-tier override on the site,
// as this agent. Empty value means "clear the override" — matches the
// wire contract of PUT /api/agent/web-config on the site side.
//
// Used by the local UI's "Agent settings" form to let the operator
// manage site-side config without logging into the site. The agent's
// next poll picks up the new WebOverrides and re-applies them through
// config.Layered.ApplyWeb so the change takes effect without a
// restart.
func (c *SiteClient) PutWebConfig(key, value string) error {
	body, _ := json.Marshal(map[string]string{"key": key, "value": value})
	req, err := http.NewRequest("PUT", c.baseURL+"/api/agent/web-config", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("web-config write returned %d: %s", resp.StatusCode, body)
	}
	return nil
}

// SiteAgentGroup mirrors the site's AgentGroup model. Decoded verbatim
// from GET /api/agent/groups; the caller converts it into the local
// storage.Group shape (source='site') before upserting into SQLite.
// Field types match the site: *int / *bool for nullable overrides,
// []string for the array columns.
type SiteAgentGroup struct {
	ID               int      `json:"id"`
	Name             string   `json:"name"`
	Type             string   `json:"type"`
	Newsgroups       []string `json:"newsgroups"`
	BannedExtensions []string `json:"banned_extensions"`
	Screenshots      *int     `json:"screenshots,omitempty"`
	SampleSeconds    *int     `json:"sample_seconds,omitempty"`
	Par2Redundancy   *int     `json:"par2_redundancy,omitempty"`
	Obfuscate        *bool    `json:"obfuscate,omitempty"`
	WatermarkText    string   `json:"watermark_text"`
	Version          int      `json:"version"`
}

// AgentGroupsResponse is the wire shape of GET /api/agent/groups:
// {max_version: N, groups: [...]}. The agent uses max_version as the
// since_version query param on its next poll so a steady-state fetch
// (no changes) is a single-int comparison server-side.
type AgentGroupsResponse struct {
	MaxVersion int              `json:"max_version"`
	Groups     []SiteAgentGroup `json:"groups"`
}

// FetchAgentGroups pulls the site-managed catalog of posting groups.
// sinceVersion should be the agent's last-seen max_version from the
// previous poll (0 on first boot).
//
// A 404 means the site hasn't shipped this endpoint yet — treat as
// empty rather than an error so old sites don't break new agents.
func (c *SiteClient) FetchAgentGroups(sinceVersion int) (*AgentGroupsResponse, error) {
	url := fmt.Sprintf("%s/api/agent/groups?since_version=%d", c.baseURL, sinceVersion)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Site hasn't been upgraded — treat as "no groups yet".
		return &AgentGroupsResponse{Groups: []SiteAgentGroup{}}, nil
	}
	// 503 with a maintenance body is the site telling all agents to back
	// off during a vacuum / migration. Surface it as a typed error so the
	// caller can downgrade it from "error" to "info" in logs (sync is
	// optional and we're going to retry on the next tick anyway).
	if resp.StatusCode == http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		var m MaintenanceResponse
		if json.Unmarshal(body, &m) == nil && m.Maintenance {
			return nil, &MaintenanceError{Info: m}
		}
		return nil, fmt.Errorf("agent groups returned 503: %s", body)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent groups returned %d", resp.StatusCode)
	}
	var out AgentGroupsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Directive is a queued instruction from the site. Currently only
// kind="write_config" with Payload.Updates (map[string]string) is defined.
type Directive struct {
	ID      int64           `json:"id"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// WriteConfigPayload is the decoded payload for kind="write_config".
type WriteConfigPayload struct {
	Updates map[string]string `json:"updates"`
}

// FetchDirectives returns any pending directives queued for this agent.
func (c *SiteClient) FetchDirectives() ([]Directive, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/api/agent/directives", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Site hasn't shipped the directives endpoint yet — treat as empty.
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("directives returned %d", resp.StatusCode)
	}
	var out struct {
		Directives []Directive `json:"directives"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Directives, nil
}

// AckDirective reports the outcome of a directive back to the site so the
// row can be marked consumed. Err is empty on success.
func (c *SiteClient) AckDirective(id int64, errMsg string) error {
	body, _ := json.Marshal(map[string]interface{}{"id": id, "error": errMsg})
	req, err := http.NewRequest("POST", c.baseURL+"/api/agent/directives/ack", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// GetConfig fetches the agent configuration from the site.
func (c *SiteClient) GetConfig() (*RemoteConfig, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/api/agent/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("config returned %d", resp.StatusCode)
	}
	var cfg RemoteConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// PostLog sends a log entry to the site for display on the agent dashboard.
func (c *SiteClient) PostLog(level, message string) error {
	body, _ := json.Marshal(map[string]string{"level": level, "message": message})
	req, err := http.NewRequest("POST", c.baseURL+"/api/agent/log", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ClearMyLocks tells the site to expire all active locks held by this agent.
// Called on startup to recover from crashes.
func (c *SiteClient) ClearMyLocks() (int, error) {
	req, err := http.NewRequest("POST", c.baseURL+"/api/agent/clear-locks", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var result struct {
		Cleared int `json:"cleared"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("client: decode response from %s: %v", "clear-locks", err)
	}
	return result.Cleared, nil
}

// ReportProgress sends a progress update to the site.
func (c *SiteClient) ReportProgress(lockID int, progress, speed string, warnings []LockWarning) error {
	// warnings is serialized as its own JSON-encoded string field so the
	// site can store it verbatim in the JSONB column without re-marshal.
	warnJSON := "[]"
	if len(warnings) > 0 {
		if b, err := json.Marshal(warnings); err == nil {
			warnJSON = string(b)
		}
	}
	body, _ := json.Marshal(map[string]interface{}{
		"lock_id":  lockID,
		"progress": progress,
		"speed":    speed,
		"warnings": warnJSON,
	})
	req, err := http.NewRequest("POST", c.baseURL+"/api/agent/progress", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// AgentLiveStatus is the real-time status posted to the site every few seconds.
//
// Upload bytes are split into two named buckets so the dashboard can color
// them separately: NzbUploadSpeed is NNTP POST traffic to the Usenet provider,
// SeedUploadSpeed is BitTorrent seed-back traffic to peers. UploadSpeed is
// the sum (NzbUploadSpeed + SeedUploadSpeed), kept for backwards compatibility
// with older sites that only know the combined field.
type AgentLiveStatus struct {
	Phase           string         `json:"phase"`
	VPNStatus       string         `json:"vpn_status"`
	PublicIP        string         `json:"public_ip"`
	DownloadSpeed   string         `json:"download_speed,omitempty"`
	UploadSpeed     string         `json:"upload_speed,omitempty"`
	NzbUploadSpeed  string         `json:"nzb_upload_speed,omitempty"`
	SeedUploadSpeed string         `json:"seed_upload_speed,omitempty"`
	Files           []FileProgress `json:"files,omitempty"`
	TaskTitle       string         `json:"task_title,omitempty"`
	RequestID       int64          `json:"request_id,omitempty"`
	DiskFreeGB      float64        `json:"disk_free_gb,omitempty"`
	DiskReservedGB  float64        `json:"disk_reserved_gb,omitempty"`
	// SeedingCount is how many completed tasks are still seeding back. Those
	// phases hold disk reservations for up to an hour after the task leaves
	// the visible queue, so without this number the dashboard shows
	// "Reserved N GB" against an empty queue and it reads as a leak.
	SeedingCount int `json:"seeding_count,omitempty"`
}

// FileProgress tracks per-file download/upload progress.
type FileProgress struct {
	Name        string        `json:"name"`
	Size        int64         `json:"size"`
	Transferred int64         `json:"transferred"`
	Percent     float64       `json:"percent"`
	Speed       string        `json:"speed,omitempty"`
	UpSpeed     string        `json:"up_speed,omitempty"`
	Phase       string        `json:"phase"`
	Peers       int           `json:"peers,omitempty"`
	Warnings    []LockWarning `json:"warnings,omitempty"`
}

// LockWarning mirrors the site's models.LockWarning. The agent emits
// one of these per currently-counting skip rule (slow speed, low
// peers, stalled) so the dashboard can surface an icon with a live
// countdown before the rule fires.
type LockWarning struct {
	Kind      string    `json:"kind"`
	Label     string    `json:"label"`
	Icon      string    `json:"icon"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PostStatus sends the agent's live status to the site for dashboard display.
// StatusResponse holds the site's response to a status post, which may include commands.
type StatusResponse struct {
	OK              bool  `json:"ok"`
	CancelRequestID int64 `json:"cancel_request_id,omitempty"`
}

func (c *SiteClient) PostStatus(status AgentLiveStatus) (*StatusResponse, error) {
	body, _ := json.Marshal(status)
	req, err := http.NewRequest("POST", c.baseURL+"/api/agent/status", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var sr StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		log.Printf("client: decode response from %s: %v", "status", err)
	}
	return &sr, nil
}

// StageRecord is one row in the agent's per-release pipeline checklist
// (migration 227). Shipped in the Complete payload's pipeline_stages
// JSON map so the site can render "subtitles: skipped (mkvextract
// missing)" / "audio_tracks: ok (3 tracks: jpn,eng,und)" without
// having to infer from empty per-stage tables.
//
// Status values are an enum the site validates on receive:
//   - "ok"      — stage ran, produced Count items
//   - "empty"   — stage ran, produced nothing (e.g. no subtitle tracks)
//   - "skipped" — stage didn't run (tool missing, not applicable, etc.)
//   - "failed"  — stage errored out; Note carries a short reason
type StageRecord struct {
	Status string `json:"status"`
	Count  int    `json:"count,omitempty"`
	Note   string `json:"note,omitempty"`
}

// CompleteResult holds all data sent on task completion.
type CompleteResult struct {
	LockID      int
	RequestID   int64
	Status      string // "completed" or "failed"
	FailReason  string // human-readable reason when Status == "failed"
	NzbData     []byte
	Password    string
	MediaInfo   interface{} // *services.VideoInfo — JSON-serialized
	Screenshots []string    // file paths to screenshot JPEGs
	// Subtitles populated by services.ExtractSubtitles when the
	// downloaded payload had any subtitle tracks. Uploaded
	// individually after the main Complete call resolves the nzb_id
	// (same split-on-size pattern Screenshots uses). Empty for
	// audio-only / no-subs releases — common case, zero overhead.
	Subtitles []SubtitleUpload
	// AudioTracks is the per-track audio catalog produced by
	// services.ProbeAudioTracks. Metadata-only (see migration 217 —
	// we never ship the audio bytes), so it ships in the same
	// multipart form as MediaInfo, encoded as a single JSON field.
	// Empty for releases without an MKV / probe failure / mkvmerge
	// not on PATH — common, zero overhead.
	AudioTracks []AudioTrackUpload
	// AudioFingerprints is the per-video Chromaprint fingerprint
	// catalog produced by services.FingerprintAudio. JSON-encoded in
	// the multipart Complete payload alongside MediaInfo. Empty when
	// fpcalc isn't installed or the release has no audio — the site
	// treats absence as "feature not run", not "track has no audio".
	AudioFingerprints []AudioFingerprintUpload
	// DominantPalette is the agent-computed top-N hex colour list
	// derived from the screenshots (services.ExtractDominantPalette).
	// Ships as a JSON array string in the dominant_palette form field
	// of the multipart Complete payload — small enough that there's
	// no point breaking it out into a per-row table.
	DominantPalette []string
	// OCRText is the manga-page text extracted by tesseract
	// (services.OCRMangaPages). Empty for anime, missing tesseract,
	// or pages that recognised as noise. Site stores inline on
	// nzbs.ocr_text (migration 220).
	OCRText     string
	OCRLanguage string
	// PipelineStages is the agent's per-stage execution checklist
	// (migration 227 on the site). One entry per stage with status,
	// count, and a short note. Serialised as a JSON object string
	// in the multipart Complete form's `pipeline_stages` field so
	// the site can persist it on the nzbs row without inferring
	// stage health from empty per-stage tables.
	//
	// Keys are stage names (mediainfo / screenshots / subtitles /
	// audio_tracks / audio_fingerprints / dominant_palette / ocr).
	// Empty when no stage was run (skipped/failed completion path).
	PipelineStages map[string]StageRecord
	// TotalSizeBytes is the torrent's discovered TotalLength from
	// metainfo. Reported on successful completion AND on oversize-abort so
	// the site can record it on nzb_requests.size_bytes — future poll
	// queries skip the same torrent for any agent whose free disk is
	// smaller. Zero means "unknown" and is omitted from the form.
	TotalSizeBytes int64
}

// maxUploadSize is the threshold (uncompressed) above which screenshots are
// sent individually instead of bundled with the completion request.
// Cloudflare free-tier caps uploads at 100 MB; we stay well under.
const maxUploadSize = 80 << 20 // 80 MB

type screenshotFile struct {
	index int
	path  string
	size  int64
}

// completeClient returns a one-shot http.Client with a longer Timeout
// for the multipart Complete upload. A successful task with 30+ HD
// screenshots on a slow uplink can run several minutes; the global
// 120s client.Timeout would kill the request and leave the lock held
// until expiry even though the agent did all the work. The global
// client keeps its tight timeout for periodic Poll / PostStatus /
// ReportProgress / PostLog calls — only Complete (and the per-
// screenshot fallback path it drives) needs the long fuse.
func (c *SiteClient) completeClient() *http.Client {
	return &http.Client{
		Timeout:   10 * time.Minute,
		Transport: &versionHeaderTransport{inner: http.DefaultTransport, client: c},
	}
}

// Complete notifies the site that a task is done (or failed).
// Uses multipart form to send NZB data, metadata JSON, and screenshot images.
// If bundling everything would exceed maxUploadSize, screenshots are sent
// individually after the initial completion call.
func (c *SiteClient) Complete(result CompleteResult) error {
	// Stat screenshot files up front.
	var screenshots []screenshotFile
	var ssTotal int64
	for i, path := range result.Screenshots {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		screenshots = append(screenshots, screenshotFile{index: i + 1, path: path, size: info.Size()})
		ssTotal += info.Size()
	}

	// Use a per-request HTTP client with a longer timeout. The
	// bundled-screenshot Complete payload can run tens of MB and the
	// split-path screenshot/subtitle POSTs that follow are each
	// multi-MB. The global 120s client.Timeout was killing those on
	// slow uplinks and orphaning the lock; this client gives the
	// whole completion sequence 10 minutes of headroom while leaving
	// the global client tight for periodic polls.
	hc := c.completeClient()

	// Build the base form (no screenshots) to measure its size.
	baseBuf, baseCT := c.buildCompleteForm(result, nil)
	baseSize := int64(baseBuf.Len())

	// uploadSubtitlesFor is shared between the bundled and split
	// paths below. Subtitles are ALWAYS sent individually (one per
	// /api/agent/subtitle POST) regardless of total size — the upload
	// flow is simpler and a bitmap track can run 20+ MB on its own.
	// The nzb_id resolution flow follows the screenshot pattern.
	uploadSubtitlesFor := func(nzbID int64) {
		if len(result.Subtitles) == 0 || nzbID == 0 {
			if len(result.Subtitles) > 0 {
				log.Printf("WARN: have %d subtitle(s) but nzb_id is 0 — skipping uploads (site /complete response was missing nzb_id?)", len(result.Subtitles))
			}
			return
		}
		log.Printf("Sending %d subtitle(s) for nzb_id=%d", len(result.Subtitles), nzbID)
		ok, failed := 0, 0
		for _, s := range result.Subtitles {
			s.NzbID = nzbID
			if err := c.uploadSubtitleWith(hc, s); err != nil {
				log.Printf("WARN: subtitle %s (track %d) upload failed: %v",
					s.Language, s.TrackIndex, err)
				failed++
			} else {
				ok++
			}
		}
		log.Printf("subtitle uploads for nzb_id=%d: %d ok, %d failed of %d total",
			nzbID, ok, failed, len(result.Subtitles))
	}

	// nzbIDFrom decodes the "nzb_id" field the site returns in its
	// /api/agent/complete response. Both the bundled and split paths
	// need it: bundled to upload subtitles after, split to upload
	// screenshots + subtitles after.
	nzbIDFrom := func(resp map[string]interface{}) int64 {
		v, ok := resp["nzb_id"]
		if !ok {
			return 0
		}
		switch n := v.(type) {
		case float64:
			return int64(n)
		case json.Number:
			id, _ := n.Int64()
			return id
		}
		return 0
	}

	if baseSize+ssTotal+int64(len(screenshots))*512 <= maxUploadSize {
		// Everything fits in one request.
		buf, ct := c.buildCompleteForm(result, screenshots)
		log.Printf("Reporting to site: %d bytes (with %d screenshots), gzipping...", buf.Len(), len(screenshots))
		resp, err := c.postGzippedWith(hc, c.baseURL+"/api/agent/complete", buf.Bytes(), ct)
		if err != nil {
			log.Printf("Complete POST failed: %v", err)
			return err
		}
		log.Printf("Complete POST succeeded (response: %v)", resp)
		uploadSubtitlesFor(nzbIDFrom(resp))
		return nil
	}

	// Too large — send completion without screenshots, then upload each
	// screenshot individually using the NZB ID the site returns.
	log.Printf("Reporting to site: payload too large (%d base + %d screenshots = %d bytes) — splitting",
		baseSize, ssTotal, baseSize+ssTotal)
	resp, err := c.postGzippedWith(hc, c.baseURL+"/api/agent/complete", baseBuf.Bytes(), baseCT)
	if err != nil {
		log.Printf("Complete POST failed: %v", err)
		return err
	}
	log.Printf("Complete POST succeeded (response: %v)", resp)
	nzbID := nzbIDFrom(resp)
	if nzbID == 0 {
		log.Printf("No nzb_id in response — cannot send screenshots/subtitles separately")
		return nil
	}

	log.Printf("Sending %d screenshots individually for nzb_id=%d", len(screenshots), nzbID)
	for _, sf := range screenshots {
		if err := c.uploadScreenshotWith(hc, nzbID, sf.index, sf.path); err != nil {
			log.Printf("WARN: screenshot %d upload failed: %v", sf.index, err)
		} else {
			log.Printf("Screenshot %d uploaded OK", sf.index)
		}
	}
	uploadSubtitlesFor(nzbID)
	return nil
}

// buildCompleteForm constructs the multipart body for /api/agent/complete.
// Returns the body bytes and the Content-Type header (with boundary).
func (c *SiteClient) buildCompleteForm(result CompleteResult, screenshots []screenshotFile) (bytes.Buffer, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	w.WriteField("lock_id", fmt.Sprintf("%d", result.LockID))
	w.WriteField("request_id", fmt.Sprintf("%d", result.RequestID))
	w.WriteField("status", result.Status)
	if result.FailReason != "" {
		w.WriteField("fail_reason", result.FailReason)
	}
	if result.Password != "" {
		w.WriteField("password", result.Password)
	}
	if result.TotalSizeBytes > 0 {
		w.WriteField("total_size_bytes", fmt.Sprintf("%d", result.TotalSizeBytes))
	}

	if result.NzbData != nil {
		if part, err := w.CreateFormFile("nzb_data", "release.nzb"); err == nil {
			part.Write(result.NzbData)
		}
	}

	if result.MediaInfo != nil {
		if infoJSON, err := json.Marshal(result.MediaInfo); err == nil {
			w.WriteField("media_info", string(infoJSON))
		}
	}

	if len(result.AudioTracks) > 0 {
		if atJSON, err := json.Marshal(result.AudioTracks); err == nil {
			w.WriteField("audio_tracks", string(atJSON))
		}
	}

	if len(result.AudioFingerprints) > 0 {
		if afJSON, err := json.Marshal(result.AudioFingerprints); err == nil {
			w.WriteField("audio_fingerprints", string(afJSON))
		}
	}

	if len(result.DominantPalette) > 0 {
		if dpJSON, err := json.Marshal(result.DominantPalette); err == nil {
			w.WriteField("dominant_palette", string(dpJSON))
		}
	}

	if result.OCRText != "" {
		w.WriteField("ocr_text", result.OCRText)
		if result.OCRLanguage != "" {
			w.WriteField("ocr_language", result.OCRLanguage)
		}
	}

	if len(result.PipelineStages) > 0 {
		if psJSON, err := json.Marshal(result.PipelineStages); err == nil {
			w.WriteField("pipeline_stages", string(psJSON))
		}
	}

	// 1.5.22: surface every silent skip path here so a missing screenshot
	// on the release page is one log line away. Previously os.Open errors
	// silently `continue`'d and CreateFormFile errors silently produced a
	// multipart with no part — leaving the operator with no way to tell
	// "screenshots upload happened but stored wrong" from "screenshots
	// never left the agent".
	screenshotsSent := 0
	screenshotsFailed := 0
	for _, sf := range screenshots {
		f, err := os.Open(sf.path)
		if err != nil {
			log.Printf("WARN: screenshot %d open %q failed: %v — skipping", sf.index, sf.path, err)
			screenshotsFailed++
			continue
		}
		part, err := w.CreateFormFile(fmt.Sprintf("screenshot_%d", sf.index), filepath.Base(sf.path))
		if err != nil {
			log.Printf("WARN: screenshot %d CreateFormFile failed: %v — skipping", sf.index, err)
			f.Close()
			screenshotsFailed++
			continue
		}
		n, err := io.Copy(part, f)
		f.Close()
		if err != nil {
			log.Printf("WARN: screenshot %d copy from %q failed after %d bytes: %v — skipping", sf.index, sf.path, n, err)
			screenshotsFailed++
			continue
		}
		if n == 0 {
			log.Printf("WARN: screenshot %d zero-bytes copied from %q — skipping (file empty or vanished mid-copy)", sf.index, sf.path)
			screenshotsFailed++
			continue
		}
		screenshotsSent++
	}
	if len(screenshots) > 0 {
		log.Printf("screenshot multipart: %d/%d included (%d failed)",
			screenshotsSent, len(screenshots), screenshotsFailed)
	}

	ct := w.FormDataContentType()
	w.Close()
	return buf, ct
}

// Backfill re-submits an NZB from a local backup file (e.g. one written by
// the agent when the original Complete call to the site failed). The site
// performs the same hash/dedup/insert/fulfill flow as Complete but doesn't
// require a lock_id. Returns the resulting nzb_id on success.
func (c *SiteClient) Backfill(requestID int64, nzbData []byte, password string) (int64, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("request_id", fmt.Sprintf("%d", requestID))
	if password != "" {
		w.WriteField("password", password)
	}
	if part, err := w.CreateFormFile("nzb_data", "release.nzb"); err == nil {
		part.Write(nzbData)
	}
	ct := w.FormDataContentType()
	w.Close()

	resp, err := c.postGzipped(c.baseURL+"/api/agent/backfill", buf.Bytes(), ct)
	if err != nil {
		return 0, err
	}
	if v, ok := resp["nzb_id"]; ok {
		switch n := v.(type) {
		case float64:
			return int64(n), nil
		case json.Number:
			id, _ := n.Int64()
			return id, nil
		}
	}
	return 0, nil
}

// postGzipped gzip-compresses body and POSTs it with auth headers.
// Returns the parsed JSON response map on success.
func (c *SiteClient) postGzipped(url string, body []byte, contentType string) (map[string]interface{}, error) {
	return c.postGzippedWith(c.http, url, body, contentType)
}

// postGzippedWith is the explicit-client variant of postGzipped. The
// Complete path uses it with completeClient() so a slow-uplink
// screenshot upload doesn't die on the global 120s timeout; every
// other caller stays on the default client via postGzipped.
func (c *SiteClient) postGzippedWith(hc *http.Client, url string, body []byte, contentType string) (map[string]interface{}, error) {
	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	if _, err := gz.Write(body); err != nil {
		return nil, fmt.Errorf("gzip compress: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}

	req, err := http.NewRequest("POST", url, &gzBuf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Content-Type", contentType)

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// Detect maintenance mode: the site returns 503 with a JSON body
		// like {"maintenance":true,"reason":"...","eta_seconds":243}.
		// Parse it into a typed error so the caller can wait for maintenance
		// to end instead of burning retries.
		if resp.StatusCode == http.StatusServiceUnavailable {
			var m MaintenanceResponse
			if json.Unmarshal(respBody, &m) == nil && m.Maintenance {
				return nil, &MaintenanceError{Info: m}
			}
		}
		return nil, fmt.Errorf("returned %d: %s", resp.StatusCode, respBody)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		log.Printf("client: decode response from %s: %v", "complete", err)
	}
	return result, nil
}

// MaintenanceResponse is the JSON payload the site returns with a 503 when
// it's in maintenance mode (e.g. VACUUM FULL, backup, migration).
type MaintenanceResponse struct {
	Maintenance    bool   `json:"maintenance"`
	Reason         string `json:"reason"`
	StartedAt      int64  `json:"started_at"`
	ElapsedSeconds int    `json:"elapsed_seconds"`
	ETASeconds     int    `json:"eta_seconds"`
}

// MaintenanceError is returned by Complete/postGzipped when the site is in
// maintenance mode. Callers should wait (not retry at a fixed backoff) and
// can inspect Info.ETASeconds for a hint at how long.
type MaintenanceError struct {
	Info MaintenanceResponse
}

func (e *MaintenanceError) Error() string {
	return fmt.Sprintf("site maintenance: %s (elapsed %ds, eta %ds)",
		e.Info.Reason, e.Info.ElapsedSeconds, e.Info.ETASeconds)
}

// IsMaintenanceError reports whether err wraps a MaintenanceError.
func IsMaintenanceError(err error) (*MaintenanceError, bool) {
	var me *MaintenanceError
	if errors.As(err, &me) {
		return me, true
	}
	return nil, false
}

// uploadScreenshot sends a single screenshot to /api/agent/screenshot.
func (c *SiteClient) uploadScreenshot(nzbID int64, index int, path string) error {
	return c.uploadScreenshotWith(c.http, nzbID, index, path)
}

// uploadScreenshotWith is the explicit-client variant used by Complete
// so the screenshot fallback path inherits the long Complete timeout.
func (c *SiteClient) uploadScreenshotWith(hc *http.Client, nzbID int64, index int, path string) error {
	_ = index // index is kept on the calling side for log labelling only
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("nzb_id", fmt.Sprintf("%d", nzbID))
	if part, err := w.CreateFormFile("screenshot", filepath.Base(path)); err == nil {
		io.Copy(part, f)
	}
	ct := w.FormDataContentType()
	w.Close()

	_, err = c.postGzippedWith(hc, c.baseURL+"/api/agent/screenshot", buf.Bytes(), ct)
	return err
}

// AudioTrackUpload is one row of the metadata-only audio catalog. The
// agent ships these as a JSON array in the audio_tracks form field on
// /api/agent/complete; the site upserts them once it knows the nzb_id.
// Mirrors services.AudioCatalogTrack but without SourcePath (the site
// doesn't care which file the track lived in).
type AudioTrackUpload struct {
	TrackIndex   int    `json:"track_index"`
	Language     string `json:"language"`
	TrackName    string `json:"track_name,omitempty"`
	Codec        string `json:"codec"`
	Channels     int    `json:"channels"`
	SampleRateHz int    `json:"sample_rate_hz,omitempty"`
	BitrateKbps  int    `json:"bitrate_kbps,omitempty"`
	DefaultTrack bool   `json:"default_track,omitempty"`
	Forced       bool   `json:"forced,omitempty"`
}

// AudioFingerprintUpload is one Chromaprint fingerprint row. The
// agent ships these as a JSON array in the audio_fingerprints form
// field on /api/agent/complete; the site upserts them once it knows
// the nzb_id. Mirrors services.AudioFingerprint with no path mapping
// — fingerprints are pure text and never reference local files.
type AudioFingerprintUpload struct {
	SourceFilename   string  `json:"source_filename"`
	DurationSeconds  float64 `json:"duration_seconds"`
	AlgorithmVersion int     `json:"algorithm_version"`
	Fingerprint      string  `json:"fingerprint"`
}

// SubtitleUpload is the multipart payload for one extracted subtitle.
// Mirrors services.SubtitleTrack but with the file bytes resolved so
// the client doesn't have to know about agent-side disk paths.
type SubtitleUpload struct {
	NzbID        int64
	TrackIndex   int
	Language     string
	TrackName    string
	Codec        string
	Forced       bool
	DefaultTrack bool
	Path         string // local file the agent extracted
}

// UploadSubtitle sends one extracted subtitle file to
// /api/agent/subtitle. Idempotent on the site side
// (UpsertSubtitle keys on (nzb_id, track_index)) so a retry after a
// transient HTTP failure overwrites in place — safe to retry the
// whole upload pass without producing duplicate rows.
func (c *SiteClient) UploadSubtitle(s SubtitleUpload) error {
	return c.uploadSubtitleWith(c.http, s)
}

// uploadSubtitleWith is the explicit-client variant used by Complete
// so the subtitle fallback path inherits the long Complete timeout.
//
// Three failure paths used to be silent until 1.5.21:
//   - CreateFormFile error was an `if err == nil` skip, silently
//     producing a multipart with subtitle FIELDS but no file part
//   - io.Copy return value was ignored, so a partial read (file
//     vanishing mid-copy) finished as if successful
//   - No positive log on success, so an operator couldn't tell
//     "subtitle uploaded but DB store failed" from "subtitle never
//     left the agent"
// All three now hard-fail with context. Caller already logs the
// returned error as WARN — that visibility is now accurate.
func (c *SiteClient) uploadSubtitleWith(hc *http.Client, s SubtitleUpload) error {
	startedAt := time.Now()
	log.Printf("subtitle upload: starting nzb_id=%d track=%d lang=%s codec=%s path=%q",
		s.NzbID, s.TrackIndex, s.Language, s.Codec, s.Path)

	f, err := os.Open(s.Path)
	if err != nil {
		return fmt.Errorf("subtitle upload: open %q (nzb_id=%d track=%d): %w", s.Path, s.NzbID, s.TrackIndex, err)
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("nzb_id", fmt.Sprintf("%d", s.NzbID))
	w.WriteField("track_index", fmt.Sprintf("%d", s.TrackIndex))
	w.WriteField("language", s.Language)
	w.WriteField("track_name", s.TrackName)
	w.WriteField("codec", s.Codec)
	if s.Forced {
		w.WriteField("forced", "1")
	}
	if s.DefaultTrack {
		w.WriteField("default_track", "1")
	}
	part, err := w.CreateFormFile("subtitle", filepath.Base(s.Path))
	if err != nil {
		return fmt.Errorf("subtitle upload: CreateFormFile for nzb_id=%d track=%d: %w", s.NzbID, s.TrackIndex, err)
	}
	bytesWritten, copyErr := io.Copy(part, f)
	if copyErr != nil {
		return fmt.Errorf("subtitle upload: io.Copy from %q (wrote %d bytes, nzb_id=%d track=%d): %w", s.Path, bytesWritten, s.NzbID, s.TrackIndex, copyErr)
	}
	if bytesWritten == 0 {
		return fmt.Errorf("subtitle upload: zero bytes copied from %q (nzb_id=%d track=%d) — file empty or vanished mid-copy", s.Path, s.NzbID, s.TrackIndex)
	}
	ct := w.FormDataContentType()
	w.Close()

	_, err = c.postGzippedWith(hc, c.baseURL+"/api/agent/subtitle", buf.Bytes(), ct)
	if err != nil {
		return fmt.Errorf("subtitle upload: POST nzb_id=%d track=%d (%d bytes): %w", s.NzbID, s.TrackIndex, bytesWritten, err)
	}
	log.Printf("subtitle upload: OK nzb_id=%d track=%d lang=%s wrote %d bytes in %s",
		s.NzbID, s.TrackIndex, s.Language, bytesWritten, time.Since(startedAt).Round(time.Millisecond))
	return nil
}

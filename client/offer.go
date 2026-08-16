package client

// Offer-system client methods. Lives in a separate file from client.go
// so the existing Usenet-pipeline API surface stays uncluttered while
// the offer feature is built out.
//
// All endpoints under /api/agent/offer/* require the 'offer' scope
// on the bearer token (added in site migration 241). When the agent
// boots, it calls OfferHealth() once to confirm the scope; an error
// here is the right time to log + skip the offer-sync loop so a
// mis-configured token doesn't bombard the site with 403s.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
)

// OfferHealthResponse is what /api/agent/offer/health returns on success.
type OfferHealthResponse struct {
	OK      bool     `json:"ok"`
	AgentID int      `json:"agent_id"`
	UserID  int      `json:"user_id"`
	Scopes  []string `json:"scopes"`
}

// OfferEntry is the agent-side payload row in the bulk register call.
// Matches the site handler's expected JSON shape exactly — field
// renames here break agent ↔ site compatibility.
type OfferEntry struct {
	EntityType    string   `json:"entity_type"`              // anime / manga / music / movie
	EntityID      int      `json:"entity_id"`                // catalog id (anidb aid / mal id / etc.)
	Season        int      `json:"season"`                   // 0 when N/A
	Episode       int      `json:"episode"`                  // 0 when N/A
	Resolution    string   `json:"resolution"`               // "1080p" / "720p" / "4k" / "" — lowercase
	SourceTag     string   `json:"source_tag"`               // "bd-remux" / "web-dl" / etc. — lowercase
	SizeBucket    string   `json:"size_bucket"`              // "<500MB" / "<1GB" / "<2.5GB" / ">=2.5GB"
	Points        int      `json:"points"`                   // 0 = free
	InfoHash      string   `json:"info_hash,omitempty"`      // 40-char SHA-1 of .torrent info dict; empty for folder sources
	DeliveryModes []string `json:"delivery_modes,omitempty"` // ["torrent"] default if empty server-side
}

// OfferRegisterResponse — accepted/submitted counts. The site silently
// drops rows whose entity_type or size_bucket aren't in the allowlist
// (the agent client doesn't bail on partial acceptance because new
// bucket schemes may roll out before the agent is updated).
type OfferRegisterResponse struct {
	OK        bool `json:"ok"`
	Accepted  int  `json:"accepted"`
	Submitted int  `json:"submitted"`
}

// OfferHealth checks the agent's token is valid + carries the 'offer'
// scope. Returns the resolved user_id + scopes so the agent can log
// "registered as <username>" on boot.
func (c *SiteClient) OfferHealth() (*OfferHealthResponse, error) {
	resp, err := c.offerGet("/api/agent/offer/health", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.offerError(resp, "health")
	}
	var out OfferHealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("offer health decode: %w", err)
	}
	return &out, nil
}

// OfferRegister bulk-uploads offers for one source (private tracker,
// public indexer, or 'personal' for folder scans). Returns the per-
// call accepted/submitted tally; rows that silently dropped (bad
// entity_type / size_bucket) account for the delta.
func (c *SiteClient) OfferRegister(trackerShortName string, offers []OfferEntry) (*OfferRegisterResponse, error) {
	body, err := json.Marshal(map[string]interface{}{
		"tracker_short_name": trackerShortName,
		"offers":             offers,
	})
	if err != nil {
		return nil, err
	}
	resp, err := c.offerPost("/api/agent/offer/register", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.offerError(resp, "register")
	}
	var out OfferRegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("offer register decode: %w", err)
	}
	return &out, nil
}

// OfferResolveTitleRow is one resolved entry from /resolve-titles.
// EntityType + EntityID stay zero when the site couldn't match.
type OfferResolveTitleRow struct {
	Title      string `json:"title"`
	EntityType string `json:"entity_type"`
	EntityID   int    `json:"entity_id"`
}

// OfferResolveTitles hands a batch of raw titles to the site for
// catalog-ID resolution. The site uses its existing TitleMatcher
// (anime + manga) so the agent doesn't have to ship the index.
// Output is order-preserved 1:1 with the input.
func (c *SiteClient) OfferResolveTitles(titles []string) ([]OfferResolveTitleRow, error) {
	if len(titles) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(map[string][]string{"titles": titles})
	if err != nil {
		return nil, err
	}
	resp, err := c.offerPost("/api/agent/offer/resolve-titles", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.offerError(resp, "resolve-titles")
	}
	var out struct {
		OK       bool                   `json:"ok"`
		Resolved []OfferResolveTitleRow `json:"resolved"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("resolve-titles decode: %w", err)
	}
	return out.Resolved, nil
}

// OfferHeartbeat re-stamps last_seen_at on the listed bucket_ids
// without re-uploading metadata. Cheap weekly keep-alive — pair with
// OfferRegister: register on first run, heartbeat thereafter when
// nothing changed in the source.
func (c *SiteClient) OfferHeartbeat(bucketIDs []int) error {
	if len(bucketIDs) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string][]int{"bucket_ids": bucketIDs})
	if err != nil {
		return err
	}
	resp, err := c.offerPost("/api/agent/offer/heartbeat", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c.offerError(resp, "heartbeat")
	}
	return nil
}

// OfferPendingRequest mirrors storage.PendingRequest on the site —
// an open or expired-claim request joined with the agent owner's
// matching offer + the bucket's identity fields. offer_hash drives
// the agent's local hash→path cache lookup during fulfillment.
type OfferPendingRequest struct {
	ID              int64  `json:"id"`
	BucketID        int    `json:"bucket_id"`
	RequesterUserID int    `json:"requester_user_id"`
	Status          string `json:"status"`
	PointsOffered   int    `json:"points_offered"`
	Notes           string `json:"notes"`
	InfoHash        string `json:"info_hash"`
	OfferID         int    `json:"offer_id"`
	OfferHash       string `json:"offer_hash"`
	EntityType      string `json:"entity_type"`
	EntityID        *int   `json:"entity_id"`
	SeasonNum       *int   `json:"season_num"`
	EpisodeNum      *int   `json:"episode_num"`
	Resolution      string `json:"resolution"`
	SourceTag       string `json:"source_tag"`
	SizeBucket      string `json:"size_bucket"`
}

// OfferClaim takes a request id and tries to lock it for this agent.
// Returns true on win; false when another offerer beat us (no error).
// 15-minute claim window — agent has that long to call OfferDeliver
// before the site sweeper reopens the row.
func (c *SiteClient) OfferClaim(requestID int) (bool, error) {
	path := fmt.Sprintf("/api/agent/offer/requests/%d/claim", requestID)
	resp, err := c.offerPost(path, []byte("{}"))
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, c.offerError(resp, "claim")
	}
	var out struct {
		OK      bool `json:"ok"`
		Claimed bool `json:"claimed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("claim decode: %w", err)
	}
	return out.Claimed, nil
}

// OfferDeliver closes a claimed request with the nzb_id the agent
// produced. Returns false when the row was already closed by another
// path (rare — sweeper-reclaim race). Bumps the offerer's
// fulfilled_count on success.
func (c *SiteClient) OfferDeliver(requestID int, nzbID int64) (bool, error) {
	body, err := json.Marshal(map[string]int64{"nzb_id": nzbID})
	if err != nil {
		return false, err
	}
	path := fmt.Sprintf("/api/agent/offer/requests/%d/deliver", requestID)
	resp, err := c.offerPost(path, body)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, c.offerError(resp, "deliver")
	}
	var out struct {
		OK        bool `json:"ok"`
		Delivered bool `json:"delivered"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("deliver decode: %w", err)
	}
	return out.Delivered, nil
}

// OfferUploadNZBResponse is the site's reply from /upload-nzb. The
// nzb_id is the catalog row created (or the existing dup's id when
// status="duplicate"). delivered=false means the close-the-request
// step lost a race (sweeper reopened, requester cancelled, etc.) —
// rare but worth surfacing so the agent can log + move on.
type OfferUploadNZBResponse struct {
	OK        bool   `json:"ok"`
	NzbID     int64  `json:"nzb_id"`
	Status    string `json:"status"` // "new" | "duplicate"
	Delivered bool   `json:"delivered"`
}

// OfferUploadNZB ships the .nzb blob this agent produced for a
// fulfillment + closes the offer_request in one round-trip. The site
// reuses its bulk-ingest pipeline (dedup on content hash, parse
// title, gzip + persist) and then calls DeliverOfferRequest with the
// resulting nzb_id.
//
// `filename` should be the original media filename + ".nzb" so the
// title-parser has something to work with. `uploadMode` accepts the
// same values as /api/bulk/nzb ("" / "normal" / "anonymous" /
// "true_anonymous"); empty = normal.
func (c *SiteClient) OfferUploadNZB(requestID int, filename string, nzbData []byte, uploadMode string) (*OfferUploadNZBResponse, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	if err := w.WriteField("request_id", strconv.Itoa(requestID)); err != nil {
		return nil, err
	}
	if uploadMode != "" {
		if err := w.WriteField("upload_mode", uploadMode); err != nil {
			return nil, err
		}
	}
	part, err := w.CreateFormFile("nzb", filename)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(nzbData); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", c.baseURL+"/api/agent/offer/upload-nzb", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.offerError(resp, "upload-nzb")
	}
	var out OfferUploadNZBResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("upload-nzb decode: %w", err)
	}
	return &out, nil
}

// OfferUploadTorrentResponse mirrors the site's /upload-torrent reply.
// TorrentRequestID points at the private-request row created by the
// ingest. delivered=false means the close-the-request step lost a
// race (sweeper reopened, requester cancelled).
type OfferUploadTorrentResponse struct {
	OK               bool   `json:"ok"`
	TorrentRequestID int64  `json:"torrent_request_id"`
	Status           string `json:"status"` // "new" | "duplicate"
	Delivered        bool   `json:"delivered"`
}

// OfferUploadTorrent re-uploads the .torrent the agent fetched from
// the source tracker + closes the offer_request in one round-trip.
// The site's bulk private-torrent ingest validates + dedups + creates
// a community-request row; the offer_request gets stamped with that
// id so the requester can pull the .torrent immediately (the eventual
// NZB still flows through the community queue asynchronously).
//
// `filename` should keep the original `.torrent` extension so the
// site's validateTorrent recognises the payload.
func (c *SiteClient) OfferUploadTorrent(requestID int, filename string, torrentData []byte) (*OfferUploadTorrentResponse, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	if err := w.WriteField("request_id", strconv.Itoa(requestID)); err != nil {
		return nil, err
	}
	part, err := w.CreateFormFile("torrent", filename)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(torrentData); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", c.baseURL+"/api/agent/offer/upload-torrent", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.offerError(resp, "upload-torrent")
	}
	var out OfferUploadTorrentResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("upload-torrent decode: %w", err)
	}
	return &out, nil
}

// OfferFail releases a claim back to open + bumps failed_count on the
// agent owner's offer. Call this when fulfillment can't complete —
// missing file, upload pipeline error, source 404, etc.
func (c *SiteClient) OfferFail(requestID int) error {
	path := fmt.Sprintf("/api/agent/offer/requests/%d/fail", requestID)
	resp, err := c.offerPost(path, []byte("{}"))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c.offerError(resp, "fail")
	}
	return nil
}

// OfferPendingRequests returns work the calling agent can fulfill.
func (c *SiteClient) OfferPendingRequests() ([]OfferPendingRequest, error) {
	resp, err := c.offerGet("/api/agent/offer/requests/pending", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.offerError(resp, "pending")
	}
	var out struct {
		OK       bool                  `json:"ok"`
		Requests []OfferPendingRequest `json:"requests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("pending decode: %w", err)
	}
	return out.Requests, nil
}

// PublishedOfferPath is one bucket this agent's owner has published, with the
// path this agent itself reported for it.
type PublishedOfferPath struct {
	OfferHash string `json:"offer_hash"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

// OfferPublishedPaths asks the site where our own published files live.
//
// Needed because publishing MOVED and fulfilment did not. The fulfil loop
// resolves a request through the local offer_hash → path cache, and only the
// folder-scanning sync ever writes to that cache — so an offer the operator
// published from the site's inventory page is one this agent cannot serve. It
// logs "no route for hash" and skips, on every tick, forever.
//
// The paths are our own scan coming back to us: the site stores what we
// reported. Nothing here is information we did not send it.
func (c *SiteClient) OfferPublishedPaths() ([]PublishedOfferPath, error) {
	resp, err := c.offerGet("/api/agent/offer/published", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.offerError(resp, "published")
	}
	var out struct {
		OK        bool                 `json:"ok"`
		Published []PublishedOfferPath `json:"published"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("published decode: %w", err)
	}
	return out.Published, nil
}

// ─── shared HTTP helpers ───────────────────────────────────────────

func (c *SiteClient) offerGet(path string, query url.Values) (*http.Response, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return c.http.Do(req)
}

func (c *SiteClient) offerPost(path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest("POST", c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return c.http.Do(req)
}

// offerError wraps non-OK responses with the path label and the body
// (capped) so error messages route the operator to the right log line.
func (c *SiteClient) offerError(resp *http.Response, label string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("offer %s: HTTP %d: %s", label, resp.StatusCode, string(body))
}

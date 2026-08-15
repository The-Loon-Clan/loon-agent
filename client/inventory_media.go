package client

// Inventory media reporting.
//
// The agent describes a file it will NEVER upload, so the site can give an
// offer a detail page as informative as a release page. Same ffprobe the
// upload pipeline runs; the difference is only where the answer goes.
//
// METADATA ONLY, deliberately. Screenshots for a whole library run to ~160 GB
// against the site's free disk and cost a video decode each; ffprobe output is
// a few kilobytes and reads headers. So this covers everything the operator has
// staged, and screenshots stay on the publish path where the operator's own
// decision bounds the count.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// InventoryMedia is the site's shape for one probed file. A deliberately
// narrower view than the agent's own VideoInfo: the offer page renders a
// summary, not a mediainfo dump, and shipping fields nobody displays would
// make the payload the widest part of the feature for no benefit.
type InventoryMedia struct {
	DurationSec float64 `json:"duration_sec,omitempty"`
	Container   string  `json:"container,omitempty"`
	VideoCodec  string  `json:"video_codec,omitempty"`
	Width       int     `json:"width,omitempty"`
	Height      int     `json:"height,omitempty"`
	FrameRate   string  `json:"frame_rate,omitempty"`
	BitrateKbps int     `json:"bitrate_kbps,omitempty"`

	AudioTracks []InventoryAudioTrack `json:"audio_tracks,omitempty"`
	Subtitles   []InventorySubtitle   `json:"subtitles,omitempty"`
}

type InventoryAudioTrack struct {
	Language string `json:"language,omitempty"`
	Codec    string `json:"codec,omitempty"`
	Channels int    `json:"channels,omitempty"`
	Default  bool   `json:"default,omitempty"`
}

type InventorySubtitle struct {
	Language string `json:"language,omitempty"`
	Codec    string `json:"codec,omitempty"`
	Forced   bool   `json:"forced,omitempty"`
}

// InventoryPendingFile is one entry from the site's work queue.
type InventoryPendingFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

// InventoryMediaEntry is one probe result on the way back.
//
// Media is a POINTER and nil is meaningful: it reports "I looked and could not
// describe this". The site stamps the row as probed either way, which is what
// keeps an unreadable file off the queue instead of retrying it every tick for
// the life of the library.
type InventoryMediaEntry struct {
	Path  string          `json:"path"`
	Media *InventoryMedia `json:"media"`
}

// InventoryMediaBatchMax mirrors the site's per-request cap.
const InventoryMediaBatchMax = 200

// InventoryPending fetches the next files to describe, oldest first.
func (c *SiteClient) InventoryPending(limit int) ([]InventoryPendingFile, error) {
	if limit <= 0 || limit > InventoryMediaBatchMax {
		limit = InventoryMediaBatchMax
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	resp, err := c.offerGet("/api/agent/offer/inventory/pending", q)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.offerError(resp, "inventory pending")
	}
	var out struct {
		Files []InventoryPendingFile `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("inventory pending decode: %w", err)
	}
	return out.Files, nil
}

// InventoryMediaResponse is what the site reports back.
type InventoryMediaResponse struct {
	OK      bool `json:"ok"`
	Saved   int  `json:"saved"`
	Skipped int  `json:"skipped"`
}

// InventoryReportMedia ships one batch of probes.
func (c *SiteClient) InventoryReportMedia(entries []InventoryMediaEntry) (*InventoryMediaResponse, error) {
	if len(entries) == 0 {
		return &InventoryMediaResponse{OK: true}, nil
	}
	if len(entries) > InventoryMediaBatchMax {
		return nil, fmt.Errorf("inventory media: %d entries exceeds the %d cap",
			len(entries), InventoryMediaBatchMax)
	}
	body, err := json.Marshal(map[string]interface{}{"files": entries})
	if err != nil {
		return nil, err
	}
	resp, err := c.offerPost("/api/agent/offer/inventory/media", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.offerError(resp, "inventory media")
	}
	var out InventoryMediaResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("inventory media decode: %w", err)
	}
	return &out, nil
}

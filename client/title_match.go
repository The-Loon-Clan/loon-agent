package client

// Client wrapper around the site's POST /api/agent/title-match-bulk
// endpoint. Used by services/collection_scanner.go to enrich a batch
// of filenames in one round-trip during a Collection scan tick.
//
// The site contract is one-shot: the agent posts a list of strings;
// the response carries a 1:1 result slice. We don't currently send
// any pagination/cursor — the site caps the batch at 500 and the
// scanner respects that on the agent side.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// TitleMatchResult is one row of the bulk match response.
//
// `Matched` is the explicit hit flag so callers don't have to special-
// case AID==0 (which legitimately means "not found"). Hint fields are
// empty when the site's regex parser couldn't extract them — UI falls
// back to a manual-entry cell in that case.
type TitleMatchResult struct {
	Title          string `json:"title"`
	Matched        bool   `json:"matched"`
	AID            int    `json:"aid"`
	AnimeTitle     string `json:"anime_title"`
	MalID          int    `json:"mal_id"`
	AnilistID      int    `json:"anilist_id"`
	Format         string `json:"format"`
	CoverURL       string `json:"cover_url"`
	ResolutionHint string `json:"resolution_hint"`
	SourceHint     string `json:"source_hint"`
}

// titleMatchBulkResponse mirrors the site handler's response envelope
// — `ok` + `results`. We keep `ok` even though a 200 already implies
// success because the site's JSONOK helper writes it on every healthy
// response and downstream tooling may eventually grep for it.
type titleMatchBulkResponse struct {
	OK      bool               `json:"ok"`
	Results []TitleMatchResult `json:"results"`
}

// TitleMatchBulk posts a slice of raw filename strings and returns the
// enriched results aligned 1:1 with the input. Returns nil + error on
// any non-200 status; the caller logs + retries (the Collection scan
// flow treats a failed batch as "skip enrichment, surface the file
// row with no metadata" rather than aborting the whole scan).
//
// Batch size: bound by the site's 500-cap. Callers chunk before
// calling — see services/collection_scanner.go enrich pass.
func (c *SiteClient) TitleMatchBulk(titles []string) ([]TitleMatchResult, error) {
	if len(titles) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(map[string]any{"titles": titles})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", c.baseURL+"/api/agent/title-match-bulk", bytes.NewReader(body))
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
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("title-match-bulk returned %d", resp.StatusCode)
	}
	var out titleMatchBulkResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

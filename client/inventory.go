package client

// Inventory reporting — the agent half of the offer staging flow.
//
// The distinction from OfferRegister matters. Register PUBLISHES: every entry
// becomes a live offer the moment the site accepts it, so the agent has to
// decide what is worth offering and everything it declines is invisible.
// Inventory REPORTS: it ships the tree verbatim and publishes nothing. The
// site resolves titles against its own catalogue and the operator selects from
// the rendered tree.
//
// That is why this endpoint carries no tracker, no points and no delivery
// modes. Those are decisions, and this call does not make any.

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// InventoryEntry is one reported file.
//
// Path is RELATIVE to the scan root and uses forward slashes. The site
// re-derives the directory, filename and extension from it rather than
// trusting three separate fields to agree, and it refuses anything absolute or
// containing "..".
type InventoryEntry struct {
	Path       string `json:"path"`
	SizeBytes  int64  `json:"size_bytes"`
	RawTitle   string `json:"raw_title,omitempty"`
	Season     int    `json:"season,omitempty"`
	Episode    int    `json:"episode,omitempty"`
	Resolution string `json:"resolution,omitempty"`
	SourceTag  string `json:"source_tag,omitempty"`
}

// InventoryResponse is what the site reports back per batch.
//
// SkippedInvalid and SkippedPath are counts of rows the site REFUSED — a path
// that is not valid UTF-8, or one that could not be normalised to a plain
// relative path. They are surfaced rather than swallowed because a file the
// operator can see on disk but never in the tree is otherwise unexplainable.
type InventoryResponse struct {
	OK             bool   `json:"ok"`
	Accepted       int    `json:"accepted"`
	Submitted      int    `json:"submitted"`
	SkippedInvalid int    `json:"skipped_invalid"`
	SkippedPath    int    `json:"skipped_path"`
	ResolvedAnime  int    `json:"resolved_anime"`
	ScanID         string `json:"scan_id"`

	// Set only on the final batch.
	Final         bool `json:"final"`
	Pruned        int  `json:"pruned"`
	MarkedMissing int  `json:"marked_missing"`
}

// InventoryBatchMax mirrors the site's per-request cap. Exported so the
// scanner batches to the same number instead of discovering the limit from a
// 400 halfway through a library.
const InventoryBatchMax = 2000

// OfferInventory ships one batch.
//
// scanID must be the SAME value for every batch of one walk: it is the
// generation marker the site prunes against. `final` closes the generation and
// must be sent exactly once, on the last batch — sending it early truncates the
// inventory to whatever has arrived so far, and the site cannot detect that
// because a short batch is indistinguishable from a small library.
func (c *SiteClient) OfferInventory(scanID string, final bool, files []InventoryEntry) (*InventoryResponse, error) {
	if scanID == "" {
		return nil, fmt.Errorf("offer inventory: scan_id is required")
	}
	if len(files) > InventoryBatchMax {
		return nil, fmt.Errorf("offer inventory: %d files exceeds the %d per-batch cap",
			len(files), InventoryBatchMax)
	}
	body, err := json.Marshal(map[string]interface{}{
		"scan_id": scanID,
		"final":   final,
		"files":   files,
	})
	if err != nil {
		return nil, err
	}
	resp, err := c.offerPost("/api/agent/offer/inventory", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.offerError(resp, "inventory")
	}
	var out InventoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("offer inventory decode: %w", err)
	}
	return &out, nil
}

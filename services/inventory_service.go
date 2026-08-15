package services

// Inventory reporting service.
//
// Walks each configured root and ships it to /api/agent/offer/inventory in
// batches. Publishes nothing: the site stores the rows as staging and the
// operator selects from the rendered tree.
//
// THE ONE INVARIANT WORTH STATING TWICE. Every batch of one walk carries the
// same scan_id, and exactly one batch — the last — carries final=true. That
// flag is what closes the generation and prunes rows this walk did not report.
// Sending it after a partial walk tells the site "these are all the files I
// have", so anything the walk never reached is deleted, or if it backs a live
// offer, flagged as missing on the basis of not having looked. Every early
// return in here therefore leaves the generation OPEN.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/the-loon-clan/loon-agent/client"
	"github.com/the-loon-clan/loon-agent/config"
)

type InventoryService struct {
	cfg   *config.Config
	site  *client.SiteClient
	roots []string
}

// NewInventoryService returns (nil, nil) when the feature is off, matching
// NewOfferSyncService so the caller can start it without a nil check.
//
// Roots default to the folder sources already declared in offer.json. An
// operator who has told the agent where their library is should not have to say
// it twice, and two lists that can disagree is a bug waiting for a Friday.
func NewInventoryService(cfg *config.Config, site *client.SiteClient, offers *OfferConfig) (*InventoryService, error) {
	if !cfg.InventoryEnabled {
		return nil, nil
	}
	roots := splitRoots(cfg.InventoryRoots)
	if len(roots) == 0 && offers != nil {
		seen := map[string]bool{}
		for _, src := range offers.Sources {
			if src.Type == "folder" && strings.TrimSpace(src.Root) != "" && !seen[src.Root] {
				seen[src.Root] = true
				roots = append(roots, src.Root)
			}
		}
	}
	return &InventoryService{cfg: cfg, site: site, roots: roots}, nil
}

func splitRoots(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Start fires the reporting loop.
//
// The boot delay is longer than offer-sync's 90s on purpose: a full walk plus
// the site's per-file title resolution is the heaviest thing either process
// does, and having it land while the agent is still warming up its NNTP pool
// and the site is still running boot migrations helps nobody.
func (s *InventoryService) Start(ctx context.Context) {
	go func() {
		select {
		case <-time.After(3 * time.Minute):
		case <-ctx.Done():
			return
		}
		if len(s.roots) == 0 {
			s.report("error", "enabled but no roots configured — set INVENTORY_ROOTS "+
				"or declare a folder source in offer.json")
			return
		}
		interval := time.Duration(s.cfg.InventoryIntervalMin) * time.Minute
		if interval <= 0 {
			interval = 6 * time.Hour
		}
		for {
			s.runOnce(ctx)
			select {
			case <-time.After(interval):
			case <-ctx.Done():
				return
			}
		}
	}()
}

// report writes one line to the container log AND to the site's agent_logs,
// which is what the agent dashboard renders.
//
// Both, deliberately. The container log is the only place with the full story
// when something is badly wrong (a panic, a config the agent never parsed), but
// it requires shell access to the box the agent runs on. The whole point of
// this feature is that the operator drives it from the SITE, so "did a walk
// happen, what did it find" has to be visible there too. Shipping it only to
// stdout is what made the first hour of running this feature guesswork.
//
// Site delivery is best-effort: a log line that fails to post must never fail
// the walk that produced it.
func (s *InventoryService) report(level, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("[inventory] %s", msg)
	if s.site != nil {
		_ = s.site.PostLog(level, "[inventory] "+msg)
	}
}

func (s *InventoryService) runOnce(ctx context.Context) {
	for _, root := range s.roots {
		if ctx.Err() != nil {
			return
		}
		if err := s.reportRoot(ctx, root); err != nil {
			// error, not info: a root that cannot be walked is the single most
			// common way this feature silently does nothing, and the message
			// names the path so a container-vs-host mount mistake is obvious.
			s.report("error", "%s: %v", root, err)
		}
	}
}

// reportRoot walks one root and ships it.
func (s *InventoryService) reportRoot(ctx context.Context, root string) error {
	opts := InventoryOptions{
		MinSizeBytes: int64(s.cfg.InventoryMinMB) * 1024 * 1024,
		ExcludeExts:  DefaultInventoryExcludes,
		MaxFiles:     s.cfg.InventoryMaxFiles,
	}
	started := time.Now()
	res, err := ScanInventory(root, opts)
	if err != nil {
		return err
	}
	if len(res.Files) == 0 {
		// Deliberately not a final=true empty generation. "I found nothing"
		// and "the mount is not there" look identical from here, and only one
		// of them should wipe the operator's inventory.
		s.report("warn", "%s: no files matched (skipped %d) — not closing the generation. "+
			"If the library is not empty, check the path exists INSIDE the container.",
			root, res.Skipped)
		return nil
	}

	scanID := NewScanID(root, started)
	batches := BatchInventory(res.Files, client.InventoryBatchMax)
	var accepted, resolved, refused int

	for i, batch := range batches {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// final only on the last batch, and never on a truncated walk.
		final := i == len(batches)-1 && !res.Truncated

		entries := make([]client.InventoryEntry, 0, len(batch))
		for _, f := range batch {
			entries = append(entries, client.InventoryEntry{
				Path:       f.RelPath,
				SizeBytes:  f.SizeBytes,
				RawTitle:   f.RawTitle,
				Season:     f.Season,
				Episode:    f.Episode,
				Resolution: f.Resolution,
				SourceTag:  f.SourceTag,
			})
		}
		resp, err := s.site.OfferInventory(scanID, final, entries)
		if err != nil {
			// Abandoning mid-walk leaves the generation open, which is the safe
			// state: the site keeps the previous inventory until a walk
			// completes. The next tick starts a fresh generation.
			return err
		}
		accepted += resp.Accepted
		resolved += resp.ResolvedAnime
		refused += resp.SkippedInvalid + resp.SkippedPath
		if final {
			s.report("info", "%s: %d files (%.1f GB) in %d batch(es) — %d accepted, "+
				"%d matched anime, %d pruned, %d now missing [%s]",
				root, len(res.Files), float64(res.TotalBytes())/1e9, len(batches),
				accepted, resolved, resp.Pruned, resp.MarkedMissing, time.Since(started).Round(time.Second))
		}
	}

	if res.Truncated {
		s.report("warn", "%s: TRUNCATED at %d files — generation left open, nothing pruned. "+
			"Raise INVENTORY_MAX_FILES or narrow the root.", root, s.cfg.InventoryMaxFiles)
	}
	if refused > 0 {
		s.report("warn", "%s: the site refused %d path(s) as unusable (invalid UTF-8, or not "+
			"relative) — those files will not appear in the tree", root, refused)
	}
	if res.Skipped > 0 {
		s.report("warn", "%s: %d entr(ies) unreadable during the walk", root, res.Skipped)
	}
	return nil
}

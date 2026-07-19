package services

// Offer sync service — ties the folder scanner (and future tracker
// scrapers) to the site's /api/agent/offer/* surface. Runs as a
// background goroutine started from main.go when OFFER_ENABLED is
// true AND the agent token carries the 'offer' scope.
//
// Per tick:
//   1. For each declared source, scan it (folder walk for now;
//      tracker scrapers in Phase 2c).
//   2. Batch the raw titles into OfferResolveTitles for catalog ids.
//   3. Compute size bucket per row and build OfferEntry objects.
//   4. POST /api/agent/offer/register for each source.
//   5. (Future) OfferHeartbeat the cached bucket ids on slow ticks.
//
// Safety: on /health failure we log + back off for the full interval
// rather than retrying tight — the site is the source of truth on
// "is offer enabled for me?" and a no-scope token should not spam.

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/the-loon-clan/loon-agent/client"
	"github.com/the-loon-clan/loon-agent/config"
	"github.com/the-loon-clan/loon-agent/storage"
)

// OfferSyncService walks declared sources and keeps the site's
// offers table in sync with what this agent can deliver. Per file:
//   - compute offer_hash (canonical site-matching SHA-1)
//   - cache hash → local_path in the agent DB so fulfillment can
//     find the file when a request arrives
//   - resolve title against the site (batched)
//   - register the offer with the site
//
// The site's UpsertOfferBucket dedupes by hash so the per-tick cost
// is bounded by source size rather than offer count.
type OfferSyncService struct {
	cfg    *config.Config
	site   *client.SiteClient
	db     *storage.DB
	loaded *OfferConfig // parsed from cfg.OfferConfigPath
}

// NewOfferSyncService constructs the service. Returns (nil, nil) when
// OFFER_ENABLED is false — caller can ignore the return and skip the
// service start without a nil check. Returns (nil, err) when the
// offer config file exists but doesn't parse, so the operator sees
// the misconfiguration on boot rather than after the first tick.
func NewOfferSyncService(cfg *config.Config, site *client.SiteClient, db *storage.DB) (*OfferSyncService, error) {
	if !cfg.OfferEnabled {
		return nil, nil
	}
	loaded, err := LoadOfferConfig(cfg.OfferConfigPath)
	if err != nil {
		return nil, err
	}
	return &OfferSyncService{cfg: cfg, site: site, db: db, loaded: loaded}, nil
}

// Start fires the sync loop. Background goroutine; never returns.
// Cancel via ctx (typically the agent's root signal-cancelled ctx).
// Boot delay 90s so the rest of the agent has finished its own
// warm-up before the agent starts hammering the resolve-titles
// endpoint.
func (s *OfferSyncService) Start(ctx context.Context) {
	go func() {
		select {
		case <-time.After(90 * time.Second):
		case <-ctx.Done():
			return
		}
		s.bootCheck(ctx)
		interval := time.Duration(s.cfg.OfferSyncIntervalMin) * time.Minute
		if interval <= 0 {
			interval = 60 * time.Minute
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

// bootCheck calls /health once and logs the resolved identity. On
// auth/scope failure we don't try the sync loop — the operator's
// token is misconfigured and we should report it loudly.
func (s *OfferSyncService) bootCheck(ctx context.Context) {
	h, err := s.site.OfferHealth()
	if err != nil {
		log.Printf("[offer] disabling sync — health check failed: %v", err)
		// Keep running so the operator sees the periodic retry log
		// (one per interval) without restarting the agent.
		return
	}
	log.Printf("[offer] enabled — user_id=%d scopes=%v sources=%d",
		h.UserID, h.Scopes, len(s.loaded.Sources))
}

// runOnce processes every source once. Errors per source are logged
// and the next source proceeds — one bad scrape shouldn't stop the
// rest of the sync.
func (s *OfferSyncService) runOnce(ctx context.Context) {
	if len(s.loaded.Sources) == 0 {
		return
	}
	for i, src := range s.loaded.Sources {
		if ctx.Err() != nil {
			return
		}
		if err := s.syncOne(ctx, src); err != nil {
			log.Printf("[offer] source #%d (%s/%s): %v", i, src.Type, src.ShortName, err)
		}
	}
}

func (s *OfferSyncService) syncOne(ctx context.Context, src OfferSource) error {
	switch src.Type {
	case "folder":
		return s.syncFolder(ctx, src)
	case "scraper":
		return s.syncScraper(ctx, src)
	default:
		return fmt.Errorf("unknown source type %q", src.Type)
	}
}

// syncScraper runs a tracker scrape + optionally pairs against a
// local downloads folder for fulfillment readiness:
//
//   1. Look up the scraper in the registry by src.ShortName.
//   2. Construct + Scan — gets the remote release list.
//   3. If src.DownloadsRoot is set, walk it and match scraped
//      releases to local files by normalised filename.
//   4. For matched entries: resolve titles via the site + register
//      offers + cache hash→path for fulfillment.
//   5. For unmatched scraped entries (no local file): register the
//      offer anyway with no path cached. Fulfillment will skip them
//      until a torrent-delivery path is wired (Phase 4) or the
//      operator pairs a downloads_root.
//
// info_hash flows through to UpsertOffer so the fulfill loop's
// auto-resolve path lights up for tracker-sourced offers.
func (s *OfferSyncService) syncScraper(ctx context.Context, src OfferSource) error {
	ctor := LookupScraper(src.ShortName)
	if ctor == nil {
		log.Printf("[offer] source %s — no scraper registered (have: %v)",
			src.ShortName, RegisteredScrapers())
		return nil
	}
	run := ScraperRunConfig{
		MinPageDelay: time.Duration(s.cfg.OfferSyncIntervalMin) * time.Minute,
		MaxPages:     50,
		Browser:      s.loaded.DefaultBrowser,
		CookiesPath:  s.loaded.CookiesFile,
	}
	scraper, err := ctor(src, run)
	if err != nil {
		return fmt.Errorf("scraper init: %w", err)
	}
	releases, err := scraper.Scan()
	if err != nil {
		return fmt.Errorf("scraper scan: %w", err)
	}
	if len(releases) == 0 {
		log.Printf("[offer] source %s: scrape returned 0 releases", src.ShortName)
		return nil
	}
	log.Printf("[offer] source %s: scraped %d release(s)", src.ShortName, len(releases))

	// Build the local file index ONCE per sync tick, keyed by a
	// normalised stem so the per-release match is a map lookup
	// (not a linear walk). Empty downloads_root → empty index +
	// every release shows up "without local file".
	localIndex := s.indexDownloads(src)

	deliveryModes := src.DeliveryModes
	if len(deliveryModes) == 0 {
		deliveryModes = []string{"nzb"}
	}

	// Resolve all titles in one batch + register the offers. Same
	// structure as syncFolder; only difference is per-row info_hash
	// from the scraper and the optional local-path pairing.
	titles := make([]string, len(releases))
	for i, r := range releases {
		titles[i] = r.RawTitle
	}
	const batchSize = 500
	accepted, submitted := 0, 0
	for i := 0; i < len(titles); i += batchSize {
		j := i + batchSize
		if j > len(titles) {
			j = len(titles)
		}
		resolved, err := s.site.OfferResolveTitles(titles[i:j])
		if err != nil {
			return fmt.Errorf("resolve: %w", err)
		}
		if len(resolved) != j-i {
			return fmt.Errorf("resolve length mismatch")
		}
		entries := make([]client.OfferEntry, 0, j-i)
		for k, rel := range releases[i:j] {
			if resolved[k].EntityID == 0 {
				continue
			}
			row, season, episode, resLower, srcLower := parseScrapedRelease(rel.RawTitle)
			sizeBytes := rel.SizeBytes
			// Local-file pairing.
			localPath := ""
			if matched, ok := localIndex[normalizeTitle(rel.RawTitle)]; ok {
				localPath = matched.Path
				if matched.SizeBytes > 0 {
					sizeBytes = matched.SizeBytes
				}
				if row.Season == 0 && matched.Season > 0 {
					season = matched.Season
				}
				if row.Episode == 0 && matched.Episode > 0 {
					episode = matched.Episode
				}
			}
			_ = row // row presently only used to seed season/episode

			// Cache hash → path when we found the file locally.
			if s.db != nil && localPath != "" {
				hash := ComputeOfferHash(
					resolved[k].EntityType, resolved[k].EntityID,
					season, episode, resLower, srcLower)
				if err := s.db.UpsertOfferPath(hash, localPath, sizeBytes); err != nil {
					log.Printf("[offer] cache path failed for %s: %v", localPath, err)
				}
			}
			entries = append(entries, client.OfferEntry{
				EntityType:    resolved[k].EntityType,
				EntityID:      resolved[k].EntityID,
				Season:        season,
				Episode:       episode,
				Resolution:    resLower,
				SourceTag:     srcLower,
				SizeBucket:    SizeBucket(sizeBytes),
				InfoHash:      rel.InfoHash,
				DeliveryModes: deliveryModes,
			})
		}
		if len(entries) == 0 {
			continue
		}
		resp, err := s.site.OfferRegister(src.ShortName, entries)
		if err != nil {
			return fmt.Errorf("register batch: %w", err)
		}
		accepted += resp.Accepted
		submitted += resp.Submitted
	}
	matched := 0
	for _, rel := range releases {
		if _, ok := localIndex[normalizeTitle(rel.RawTitle)]; ok {
			matched++
		}
	}
	log.Printf("[offer] source %s: %d/%d offers accepted, %d/%d matched local file(s)",
		src.ShortName, accepted, submitted, matched, len(releases))
	return nil
}

// indexDownloads walks src.DownloadsRoot once and returns a map keyed
// by normalised title. Empty root → empty map (no error).
func (s *OfferSyncService) indexDownloads(src OfferSource) map[string]ScannedFile {
	if src.DownloadsRoot == "" {
		return map[string]ScannedFile{}
	}
	rows, err := ScanFolder(src.DownloadsRoot, src.Extensions, src.SizeMinMB)
	if err != nil {
		log.Printf("[offer] downloads_root walk %s: %v", src.DownloadsRoot, err)
		return map[string]ScannedFile{}
	}
	out := make(map[string]ScannedFile, len(rows))
	for _, r := range rows {
		out[normalizeTitle(r.RawTitle)] = r
	}
	return out
}

// parseScrapedRelease runs the same hint extraction the folder
// scanner uses, but on a tracker-provided title string rather than
// a filename. Reuses the regexes defined alongside ScanFolder.
func parseScrapedRelease(title string) (row ScannedFile, season, episode int, res, source string) {
	row.RawTitle = title
	if m := reSeasonEp.FindStringSubmatch(title); m != nil {
		row.Season = atoiOr(m[1])
		row.Episode = atoiOr(m[2])
	} else if m := reEpOnly.FindStringSubmatch(title); m != nil {
		row.Season = 1
		row.Episode = atoiOr(m[1])
	}
	if m := reResolution.FindStringSubmatch(title); m != nil {
		row.Resolution = strings.ToLower(m[1])
		if row.Resolution == "uhd" {
			row.Resolution = "4k"
		}
	}
	if m := reSourceTag.FindStringSubmatch(title); m != nil {
		row.SourceTag = strings.ToLower(strings.ReplaceAll(m[1], "-", ""))
		switch row.SourceTag {
		case "bdremux":
			row.SourceTag = "bd-remux"
		case "webdl":
			row.SourceTag = "web-dl"
		}
	}
	return row, row.Season, row.Episode, row.Resolution, row.SourceTag
}

// atoiOr returns the int parse of s, or 0 on error. Used only by
// parseScrapedRelease where a non-numeric match-group means the
// regex misfired and we'd rather drop the hint than fail the scrape.
func atoiOr(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// normalizeTitle drops bracket groups, lowercases, collapses spaces,
// and strips file extensions. Used as the join key between scraped
// release titles and the local downloads_root walk. Intentionally
// liberal — false matches are caught by entity_id resolution later.
//
// Extension strip uses EqualFold on the trailing slice instead of
// lowercasing the whole title first — strings.ToLower can change byte
// length on some locale-sensitive runes, which would make t[:len(t)-len(ext)]
// slice into a different position than intended. The extensions
// themselves are ASCII so EqualFold is byte-equivalent for them.
func normalizeTitle(s string) string {
	t := reBracketGroup.ReplaceAllString(s, " ")
	for _, ext := range []string{".mkv", ".mp4", ".avi", ".m4v"} {
		if len(t) >= len(ext) && strings.EqualFold(t[len(t)-len(ext):], ext) {
			t = t[:len(t)-len(ext)]
			break
		}
	}
	t = strings.ToLower(t)
	t = strings.Join(strings.Fields(t), " ")
	return t
}

// syncFolder scans the folder, resolves titles via the site, and
// pushes the resulting offer set with delivery_modes defaulted to
// ['nzb'] for personal collections (the agent owns Usenet posting,
// so handing the file to upload_nzb.go is the natural path). The
// operator can override via offer.yml's per-source delivery_modes.
func (s *OfferSyncService) syncFolder(ctx context.Context, src OfferSource) error {
	if src.ShortName == "" {
		return fmt.Errorf("short_name required")
	}
	rows, err := ScanFolder(src.Root, src.Extensions, src.SizeMinMB)
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	if len(rows) == 0 {
		log.Printf("[offer] source %s: 0 files matched", src.ShortName)
		return nil
	}

	// Resolve titles in batches of 500 so a giant library doesn't
	// blow past the site's 2000-title-per-call cap and so we can
	// pipeline scan + resolve + register.
	const batchSize = 500
	deliveryModes := src.DeliveryModes
	if len(deliveryModes) == 0 {
		deliveryModes = []string{"nzb"}
	}

	totalAccepted := 0
	totalSubmitted := 0
	for i := 0; i < len(rows); i += batchSize {
		j := i + batchSize
		if j > len(rows) {
			j = len(rows)
		}
		chunk := rows[i:j]
		titles := make([]string, len(chunk))
		for k, r := range chunk {
			titles[k] = r.RawTitle
		}
		resolved, err := s.site.OfferResolveTitles(titles)
		if err != nil {
			return fmt.Errorf("resolve: %w", err)
		}
		if len(resolved) != len(chunk) {
			return fmt.Errorf("resolve length mismatch: got %d want %d", len(resolved), len(chunk))
		}
		entries := make([]client.OfferEntry, 0, len(chunk))
		for k, r := range chunk {
			if resolved[k].EntityID == 0 {
				continue // unresolved — skip; nothing to register against
			}
			resLower := strings.ToLower(r.Resolution)
			srcLower := strings.ToLower(r.SourceTag)
			// Cache hash → local path BEFORE register so a process
			// crash mid-sync still leaves the agent able to fulfill
			// the offers it already shipped (idempotent on re-sync).
			if s.db != nil {
				hash := ComputeOfferHash(
					resolved[k].EntityType, resolved[k].EntityID,
					r.Season, r.Episode, resLower, srcLower)
				if err := s.db.UpsertOfferPath(hash, r.Path, r.SizeBytes); err != nil {
					log.Printf("[offer] cache path failed for %s: %v", r.Path, err)
				}
			}
			entries = append(entries, client.OfferEntry{
				EntityType:    resolved[k].EntityType,
				EntityID:      resolved[k].EntityID,
				Season:        r.Season,
				Episode:       r.Episode,
				Resolution:    resLower,
				SourceTag:     srcLower,
				SizeBucket:    SizeBucket(r.SizeBytes),
				DeliveryModes: deliveryModes,
			})
		}
		if len(entries) == 0 {
			continue
		}
		resp, err := s.site.OfferRegister(src.ShortName, entries)
		if err != nil {
			return fmt.Errorf("register batch %d: %w", i/batchSize, err)
		}
		totalAccepted += resp.Accepted
		totalSubmitted += resp.Submitted
	}
	log.Printf("[offer] source %s: %d/%d offers accepted (folder=%s)",
		src.ShortName, totalAccepted, totalSubmitted, src.Root)
	return nil
}

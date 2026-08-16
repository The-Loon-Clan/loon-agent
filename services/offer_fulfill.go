package services

// Offer-fulfill service — polls the site for requests this agent can
// satisfy and walks them through the claim → upload → deliver flow.
//
// Per request:
//   1. Look up the local file via hash→path cache (sync-side wrote it).
//   2. Claim the request (optimistic lock).
//   3. Stage the file into a fresh temp dir.
//   4. GeneratePAR2 writes recovery blocks alongside it.
//   5. UploadDirectory pushes to NNTP, returns FileSegments.
//   6. CreateMultiFileNZBBytes assembles the NZB blob.
//   7. OfferUploadNZB ships blob + request_id; site dedups + closes.
//   8. On any failure after claim, /fail the request so it reopens.

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/the-loon-clan/loon-agent/client"
	"github.com/the-loon-clan/loon-agent/config"
	"github.com/the-loon-clan/loon-agent/storage"
)

type OfferFulfillService struct {
	cfg  *config.Config
	site *client.SiteClient
	db   *storage.DB
	// loaded is the same offer.json the sync service reads. Needed here for
	// the cookie jar and browser identity a remote .torrent fetch has to
	// present; a parse failure is NOT fatal for fulfillment (the local-file
	// route needs none of it), so this may be nil.
	loaded *OfferConfig
}

// NewOfferFulfillService returns (nil, nil) when the feature is off
// so the caller can skip the start without a nil check.
func NewOfferFulfillService(cfg *config.Config, site *client.SiteClient, db *storage.DB) *OfferFulfillService {
	if !cfg.OfferEnabled {
		return nil
	}
	// Best-effort: the sync service already reports a bad offer.json loudly
	// at boot, and refusing to start the fulfill loop over it would take the
	// working local-file route down with the broken remote one.
	loaded, err := LoadOfferConfig(cfg.OfferConfigPath)
	if err != nil {
		log.Printf("[offer-fulfill] offer config unreadable (%v) — remote fulfillment unavailable", err)
	}
	return &OfferFulfillService{cfg: cfg, site: site, db: db, loaded: loaded}
}

// Start runs the fulfill loop in a background goroutine. Default
// tick is one minute — the site's request fan-out via notifications
// is the primary signal; this loop is the fallback for agents that
// didn't get the push.
func (s *OfferFulfillService) Start(ctx context.Context) {
	go func() {
		// Boot delay 120s — slightly after the sync service so the
		// hash→path cache is warm before the first fulfill tick.
		select {
		case <-time.After(120 * time.Second):
		case <-ctx.Done():
			return
		}
		const interval = 60 * time.Second
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

func (s *OfferFulfillService) runOnce(ctx context.Context) {
	pending, err := s.site.OfferPendingRequests()
	if err != nil {
		log.Printf("[offer-fulfill] poll error: %v", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	log.Printf("[offer-fulfill] %d pending request(s)", len(pending))
	// Refresh the hash-to-path cache from what the SITE says we published,
	// before trying to serve anything.
	//
	// Publishing moved to the site's inventory page and fulfilment did not:
	// only the folder-scanning sync writes this cache, so an offer made from
	// the site is one we cannot serve, and the loop below would log "no route
	// for hash" for it on every tick forever. Doing it here rather than in the
	// sync service keeps it next to the code that needs it, and it only costs
	// a request when there is actually work to do.
	s.refreshPublishedPaths()
	for _, r := range pending {
		if ctx.Err() != nil {
			return
		}
		// Skip rows already claimed by someone else (the site only
		// returns 'open' or expired 'claimed'; the explicit check is
		// belt-and-suspenders for future schema drift).
		if r.Status != "open" && r.Status != "claimed" {
			continue
		}
		s.fulfillOne(ctx, r)
	}
}

// refreshPublishedPaths caches where our site-published buckets live locally.
//
// Best-effort: a failure leaves the cache as it was, which degrades to the
// behaviour we already had rather than dropping requests we could serve from
// offer.json. Reported once per tick, because a call that fails EVERY tick is
// an agent that silently cannot fulfil anything it published from the site.
func (s *OfferFulfillService) refreshPublishedPaths() {
	rows, err := s.site.OfferPublishedPaths()
	if err != nil {
		log.Printf("[offer-fulfill] published-paths refresh failed: %v", err)
		return
	}
	roots := s.inventoryRoots()
	added, unresolved := 0, 0
	for _, r := range rows {
		if r.OfferHash == "" || r.Path == "" {
			continue
		}
		// The site's path is RELATIVE to one of our roots, and it has never
		// been told where those are — the same library is a different absolute
		// path inside this container than on the host. Resolve here, and cache
		// only what actually exists: a path that fails os.Stat later would
		// make the loop pick the local route and then abandon the request,
		// which is strictly worse than admitting we have no route.
		abs := resolveAgainstRoots(roots, r.Path)
		if abs == "" {
			unresolved++
			continue
		}
		// Per-column fill: a bucket reachable BOTH locally and via a tracker
		// keeps its torrent URL, so this cannot demote a remote route.
		if err := s.db.UpsertOfferPath(r.OfferHash, abs, r.SizeBytes); err != nil {
			log.Printf("[offer-fulfill] caching %s: %v", shortHash(r.OfferHash), err)
			continue
		}
		added++
	}
	if added > 0 {
		log.Printf("[offer-fulfill] cached %d published path(s) from the site", added)
	}
	if unresolved > 0 {
		// Named loudly: this is an operator problem (a root that moved, or
		// INVENTORY_ROOTS pointing somewhere the fulfil container cannot see),
		// and it is invisible from the site side.
		log.Printf("[offer-fulfill] %d published path(s) matched no root under %v — those offers cannot be served",
			unresolved, roots)
	}
}

// inventoryRoots is where this agent's library lives, by the same rule the
// inventory reporter uses: INVENTORY_ROOTS if set, else the folder sources
// already declared in offer.json. One list, because an operator who has told
// the agent where their library is should not have to say it twice.
func (s *OfferFulfillService) inventoryRoots() []string {
	roots := splitRoots(s.cfg.InventoryRoots)
	if len(roots) > 0 {
		return roots
	}
	if s.loaded == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, src := range s.loaded.Sources {
		root := strings.TrimSpace(src.Root)
		if src.Type == "folder" && root != "" && !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	return roots
}

// resolveAgainstRoots returns the first root under which relPath exists, or ""
// when none does.
//
// An absolute path is taken as-is: a future site that stores absolute paths
// should not have them mangled by a join, and filepath.Join would silently
// produce nonsense for one.
func resolveAgainstRoots(roots []string, relPath string) string {
	if filepath.IsAbs(relPath) {
		if _, err := os.Stat(relPath); err == nil {
			return relPath
		}
		return ""
	}
	for _, root := range roots {
		candidate := filepath.Join(root, filepath.FromSlash(relPath))
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func (s *OfferFulfillService) fulfillOne(ctx context.Context, r client.OfferPendingRequest) {
	rid := int(r.ID)
	// 1. Work out how — if at all — we can serve this bucket.
	src, err := s.db.GetOfferSource(r.OfferHash)
	if err != nil {
		log.Printf("[offer-fulfill #%d] cache lookup error: %v", rid, err)
		return
	}
	route := chooseFulfillRoute(src, s.cfg)
	switch route {
	case fulfillRouteNone:
		log.Printf("[offer-fulfill #%d] no route for hash %s — skipping", rid, shortHash(r.OfferHash))
		return
	case fulfillRouteRemoteDisabled:
		// Say it once per encounter rather than silently doing nothing: an
		// operator wondering why a request never fills should find the
		// reason in the log next to the request id.
		log.Printf("[offer-fulfill #%d] only a remote source (%s) and OFFER_REMOTE_FULFILL is off — skipping",
			rid, src.SourceShort)
		return
	}

	localPath := src.LocalPath
	if route == fulfillRouteLocal {
		if _, err := os.Stat(localPath); err != nil {
			log.Printf("[offer-fulfill #%d] file gone (%s): %v — skipping", rid, localPath, err)
			return
		}
	}

	// 2. Claim. Always before any expensive work — a remote fulfillment
	// spends bandwidth and tracker ratio, and doing that for a request
	// another offerer already owns wastes both.
	got, err := s.site.OfferClaim(rid)
	if err != nil {
		log.Printf("[offer-fulfill #%d] claim error: %v", rid, err)
		return
	}
	if !got {
		// Another offerer beat us — fine, move on.
		return
	}
	log.Printf("[offer-fulfill #%d] claimed via %s route", rid, route)

	// Helper closure: any failure path after claim should /fail the
	// request so it reopens for other offerers + bumps our
	// failed_count. Log + swallow the /fail error so a flaky site
	// connection doesn't hide the original failure reason.
	failRequest := func(stage string, cause error) {
		log.Printf("[offer-fulfill #%d] %s failed: %v — releasing claim", rid, stage, cause)
		if ferr := s.site.OfferFail(rid); ferr != nil {
			log.Printf("[offer-fulfill #%d] /fail error: %v", rid, ferr)
		}
	}

	jobName := fmt.Sprintf("offer-%d", rid)
	// Hand back the disk reservation however this request ends.
	//
	// downloadTorrentFile reserves for EVERY caller, and ReleaseDisk was
	// deferred in only two places — the site-task handler and the offline
	// processor. This path was the third caller and released nothing, so a
	// remote fulfilment permanently subtracted ~1.3x the torrent size from
	// the free space the agent believes it has, for the life of the process.
	// The counter is in memory, so the symptom is "Reserved N GB" on an agent
	// with nothing running, and a restart hides it.
	//
	// Cheap and safe on the local route too: ReleaseDisk is a no-op for a
	// jobName that never reserved.
	defer ReleaseDisk(jobName)

	// 3. Get the bytes into a directory UploadDirectory can walk. Two
	// routes converge here: a local file is symlinked (or copied) into a
	// fresh staging dir, while a remote source is downloaded — and the
	// download's own data directory IS that directory, multi-file torrents
	// included, so nothing is staged twice.
	var uploadDir, releaseName string
	if route == fulfillRouteRemote {
		// The claim TTL is 15 minutes and this download is not; keep the
		// claim alive for as long as we are genuinely working on it. The
		// site treats a re-claim by the holder as an extension.
		stopKeepalive := s.keepClaimAlive(ctx, rid)
		dir, name, err := s.downloadRemote(ctx, rid, src, jobName)
		stopKeepalive()
		if err != nil {
			failRequest("remote-download", err)
			return
		}
		defer os.RemoveAll(dir)
		uploadDir, releaseName = dir, name
	} else {
		stageDir, err := os.MkdirTemp(s.cfg.TempDir, jobName+"-")
		if err != nil {
			failRequest("stage-mkdir", err)
			return
		}
		defer os.RemoveAll(stageDir)
		staged := filepath.Join(stageDir, filepath.Base(localPath))
		if err := os.Symlink(localPath, staged); err != nil {
			// Symlink not supported (Windows non-admin, exotic FS) — copy.
			if cerr := copyFile(localPath, staged); cerr != nil {
				failRequest("stage-copy", cerr)
				return
			}
		}
		uploadDir, releaseName = stageDir, filepath.Base(localPath)
	}

	// 4. PAR2 recovery, into the same directory the upload walks.
	//
	// "We ship the bits as-is" was the Phase-3 note here, and it was fine
	// while nothing had ever fulfilled. The first real delivery was 14.9 GB
	// posted with NO recovery blocks: one missing article and the whole
	// release is unrepairable, with nothing on the release page to say so.
	// Every other upload path on this agent generates PAR2; this one skipped
	// it by omission rather than by decision.
	//
	// Non-fatal, matching both the online and offline paths: a post without
	// recovery is worse than one with it and far better than no post at all,
	// after the download and staging are already paid for.
	par2Base := SanitizeBaseName(releaseName)
	if par2Base == "" {
		par2Base = jobName
	}
	if _, err := GeneratePAR2(ctx, uploadDir, par2Base, PAR2Options{
		Redundancy: s.cfg.PAR2Redundancy,
		BlockSize:  ChunkSize,
		Threads:    s.cfg.PAR2Threads,
		MemoryMB:   s.cfg.PAR2Memory,
	}, nil); err != nil {
		log.Printf("[offer-fulfill #%d] PAR2 warning (non-fatal): %v", rid, err)
	}

	// 5. Upload to Usenet. Same path the task-driven pipeline uses.
	fileSegments, err := UploadDirectory(ctx, s.cfg, uploadDir, releaseName, jobName)
	if err != nil {
		failRequest("upload", err)
		return
	}

	// 6. Build the NZB blob with the request id as metadata so the
	// site's ingest path can correlate even when the agent name is
	// the only other hint.
	nzbData, err := CreateMultiFileNZBBytes(s.cfg, fileSegments, "", NZBMetaInfo{
		Title:     releaseName,
		RequestID: r.ID,
	})
	if err != nil {
		failRequest("nzb-build", err)
		return
	}

	// 7. Ship to the site — single round-trip handles both ingest
	// (creates nzbs row) and deliver (closes offer_request).
	uploadName := releaseName + ".nzb"
	resp, err := s.site.OfferUploadNZB(rid, uploadName, nzbData, "")
	if err != nil {
		failRequest("upload-nzb", err)
		return
	}
	log.Printf("[offer-fulfill #%d] delivered nzb_id=%d status=%s delivered=%v",
		rid, resp.NzbID, resp.Status, resp.Delivered)
}

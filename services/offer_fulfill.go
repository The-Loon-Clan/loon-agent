package services

// Offer-fulfill service — polls the site for requests this agent can
// satisfy and walks them through the claim → publish → deliver flow.
//
// Per request:
//  1. Look up the local file via hash→path cache (sync-side wrote it).
//  2. Claim the request (optimistic lock), kept alive for the whole job.
//  3. Get the content into a directory (symlink a local file, or download
//     the remote source).
//  4. services.PublishDirectory — THE SAME PIPELINE THE TASK PATH RUNS:
//     metadata + screenshots, staging, archive extraction, PAR2, optional
//     encryption, the upload slot, the manifest audit, the NNTP upload,
//     the NZB build.
//  5. OfferUploadNZB ships blob + request_id + the sidecar description;
//     the site dedups, closes the request, and pays the escrow.
//  6. Screenshots + subtitles follow on the per-NZB endpoints.
//  7. On any failure after claim, /fail the request so it reopens.

import (
	"context"
	"encoding/json"
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

	// 3. Get the bytes into a CONTENT directory the publish pipeline can
	// read. Two routes converge here: a local file is symlinked (or copied)
	// into a fresh dir, while a remote source is downloaded — the download's
	// own data directory IS that directory, multi-file torrents included.
	//
	// The claim TTL is 15 minutes and neither the download nor the pipeline
	// (screenshots + PAR2 + a multi-GB NNTP upload behind the shared slot)
	// is; keep the claim alive for the WHOLE job. The site treats a re-claim
	// by the holder as an extension.
	stopKeepalive := s.keepClaimAlive(ctx, rid)
	defer stopKeepalive()
	var contentDir, releaseName string
	if route == fulfillRouteRemote {
		dir, name, err := s.downloadRemote(ctx, rid, src, jobName)
		if err != nil {
			failRequest("remote-download", err)
			return
		}
		defer os.RemoveAll(dir)
		contentDir, releaseName = dir, name
	} else {
		dir, err := os.MkdirTemp(s.cfg.TempDir, jobName+"-")
		if err != nil {
			failRequest("stage-mkdir", err)
			return
		}
		defer os.RemoveAll(dir)
		staged := filepath.Join(dir, filepath.Base(localPath))
		if err := os.Symlink(localPath, staged); err != nil {
			// Symlink not supported (Windows non-admin, exotic FS) — copy.
			if cerr := copyFile(localPath, staged); cerr != nil {
				failRequest("stage-copy", cerr)
				return
			}
		}
		contentDir, releaseName = dir, filepath.Base(localPath)
	}

	// 4. THE SAME PIPELINE THE TASK PATH RUNS. This path used to re-implement
	// fragments of it and every stage it lacked was its own incident: the
	// first real delivery shipped 14.9 GB with no PAR2, the second fix added
	// PAR2 and nothing else — no screenshots, no media probe, no manifest
	// audit, no upload slot, no obfuscation or encryption. PublishDirectory
	// is the whole set, and a stage added there exists here automatically.
	pub, pubErr := PublishDirectory(ctx, PublishJob{
		Cfg:        s.cfg,
		JobName:    jobName,
		Title:      strings.TrimSuffix(releaseName, filepath.Ext(releaseName)),
		RequestID:  r.ID,
		ContentDir: contentDir,
		Describe:   true,
		Progress: func(step, detail string) {
			storage.UpdateState(jobName, step, detail, 0)
		},
		PostLog: func(level, msg string) {
			if perr := s.site.PostLog(level, msg); perr != nil {
				log.Printf("[offer-fulfill #%d] PostLog failed: %v", rid, perr)
			}
		},
	})
	if pubErr != nil {
		failRequest("publish", pubErr)
		return
	}

	// 5. Ship to the site — one round-trip handles ingest (creates the nzbs
	// row), delivery (closes the offer_request), and the sidecar description
	// the pipeline produced. Password included: an encrypted delivery whose
	// password never reached the site would be undownloadable.
	uploadName := releaseName + ".nzb"
	resp, err := s.site.OfferUploadNZB(rid, uploadName, pub.NzbData, "", &client.OfferSidecars{
		Password:              pub.Password,
		MediaInfoJSON:         marshalJSON(pub.VideoInfo),
		AudioTracksJSON:       marshalJSON(pub.AudioTracks),
		AudioFingerprintsJSON: marshalJSON(pub.Fingerprints),
		DominantPaletteJSON:   marshalJSON(pub.DominantPalette),
		PipelineStagesJSON:    marshalJSON(pub.Stages),
		OCRText:               pub.OCRText,
		OCRLanguage:           pub.OCRLanguage,
	})
	if err != nil {
		failRequest("upload-nzb", err)
		return
	}
	log.Printf("[offer-fulfill #%d] delivered nzb_id=%d status=%s delivered=%v",
		rid, resp.NzbID, resp.Status, resp.Delivered)

	// 6. Screenshots and subtitles ride the existing per-NZB endpoints, now
	// that the nzb_id exists. Best-effort by design: the delivery above is
	// already done, and a failed image must never look like a failed
	// fulfilment.
	if resp.NzbID > 0 {
		if len(pub.Screenshots) > 0 {
			n := s.site.UploadScreenshots(resp.NzbID, pub.Screenshots)
			log.Printf("[offer-fulfill #%d] uploaded %d/%d screenshot(s)", rid, n, len(pub.Screenshots))
		}
		for _, sub := range pub.Subtitles {
			sub.NzbID = resp.NzbID
			if serr := s.site.UploadSubtitle(sub); serr != nil {
				log.Printf("[offer-fulfill #%d] subtitle upload failed (non-fatal): %v", rid, serr)
			}
		}
	}
}

// marshalJSON renders a sidecar payload, or "" for nil/empty — the form field
// is omitted entirely rather than shipping "null" for a release with nothing
// to describe.
func marshalJSON(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case *VideoInfo:
		if t == nil {
			return ""
		}
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" || string(b) == "[]" || string(b) == "{}" {
		return ""
	}
	return string(b)
}

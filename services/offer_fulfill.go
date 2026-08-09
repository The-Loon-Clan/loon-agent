package services

// Offer-fulfill service — polls the site for requests this agent can
// satisfy and walks them through the claim → upload → deliver flow.
//
// Per request:
//   1. Look up the local file via hash→path cache (sync-side wrote it).
//   2. Claim the request (optimistic lock).
//   3. Stage the file into a fresh temp dir.
//   4. UploadDirectory pushes to NNTP, returns FileSegments.
//   5. CreateMultiFileNZBBytes assembles the NZB blob.
//   6. OfferUploadNZB ships blob + request_id; site dedups + closes.
//   7. On any failure after claim, /fail the request so it reopens.

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
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

	// 4. Upload to Usenet. Same path the task-driven pipeline uses,
	// minus the post-download media transforms — for personal-source
	// fulfillment we ship the bits as-is.
	fileSegments, err := UploadDirectory(ctx, s.cfg, uploadDir, releaseName, jobName)
	if err != nil {
		failRequest("upload", err)
		return
	}

	// 5. Build the NZB blob with the request id as metadata so the
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

	// 6. Ship to the site — single round-trip handles both ingest
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

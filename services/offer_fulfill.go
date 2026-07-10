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

	"github.com/ameNZB/usenet-pipeline/client"
	"github.com/ameNZB/usenet-pipeline/config"
	"github.com/ameNZB/usenet-pipeline/storage"
)

type OfferFulfillService struct {
	cfg  *config.Config
	site *client.SiteClient
	db   *storage.DB
}

// NewOfferFulfillService returns (nil, nil) when the feature is off
// so the caller can skip the start without a nil check.
func NewOfferFulfillService(cfg *config.Config, site *client.SiteClient, db *storage.DB) *OfferFulfillService {
	if !cfg.OfferEnabled {
		return nil
	}
	return &OfferFulfillService{cfg: cfg, site: site, db: db}
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
	// 1. Look up the local file via hash→path cache.
	localPath, err := s.db.GetOfferPath(r.OfferHash)
	if err != nil {
		log.Printf("[offer-fulfill #%d] cache lookup error: %v", rid, err)
		return
	}
	if localPath == "" {
		log.Printf("[offer-fulfill #%d] no cached path for hash %s — skipping", rid, r.OfferHash[:12])
		return
	}
	fi, err := os.Stat(localPath)
	if err != nil {
		log.Printf("[offer-fulfill #%d] file gone (%s): %v — skipping", rid, localPath, err)
		return
	}

	// 2. Claim.
	got, err := s.site.OfferClaim(rid)
	if err != nil {
		log.Printf("[offer-fulfill #%d] claim error: %v", rid, err)
		return
	}
	if !got {
		// Another offerer beat us — fine, move on.
		return
	}
	log.Printf("[offer-fulfill #%d] claimed; file=%s (%d bytes)", rid, localPath, fi.Size())

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

	// 3. Stage the file in a fresh temp dir. UploadDirectory walks
	// the dir and uploads every file — for an offer fulfillment we
	// only have one. Symlink first (no copy cost), fall back to
	// copy if cross-device or otherwise refused.
	jobName := fmt.Sprintf("offer-%d", rid)
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

	// 4. Upload to Usenet. Same path the task-driven pipeline uses,
	// minus the post-download media transforms — for personal-source
	// fulfillment we ship the bits as-is.
	fileSegments, err := UploadDirectory(ctx, s.cfg, stageDir, jobName)
	if err != nil {
		failRequest("upload", err)
		return
	}

	// 5. Build the NZB blob with the request id as metadata so the
	// site's ingest path can correlate even when the agent name is
	// the only other hint.
	nzbData, err := CreateMultiFileNZBBytes(s.cfg, fileSegments, "", NZBMetaInfo{
		Title:     filepath.Base(localPath),
		RequestID: r.ID,
	})
	if err != nil {
		failRequest("nzb-build", err)
		return
	}

	// 6. Ship to the site — single round-trip handles both ingest
	// (creates nzbs row) and deliver (closes offer_request).
	uploadName := filepath.Base(localPath) + ".nzb"
	resp, err := s.site.OfferUploadNZB(rid, uploadName, nzbData, "")
	if err != nil {
		failRequest("upload-nzb", err)
		return
	}
	log.Printf("[offer-fulfill #%d] delivered nzb_id=%d status=%s delivered=%v",
		rid, resp.NzbID, resp.Status, resp.Delivered)
}

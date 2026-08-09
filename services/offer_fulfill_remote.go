package services

// Remote-source fulfillment: the branch that downloads a release before
// posting it, for offers whose bytes are not already on this disk.
//
// Split out of offer_fulfill.go because it is the part with policy in it —
// whether to spend the operator's bandwidth at all, how long to keep trying,
// and how to hold a claim that outlives the site's TTL — and because the
// routing decision is worth testing without a tracker, a swarm or a site.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/the-loon-clan/loon-agent/config"
	"github.com/the-loon-clan/loon-agent/storage"
)

// fulfillRoute is how (or whether) a request can be served.
type fulfillRoute string

const (
	// fulfillRouteNone: nothing cached for this bucket. Either the sync has
	// not run since the offer was registered, or the file is gone.
	fulfillRouteNone fulfillRoute = "none"
	// fulfillRouteLocal: the bytes are on disk. Cheapest and always
	// preferred — a local copy costs no bandwidth and no tracker ratio.
	fulfillRouteLocal fulfillRoute = "local"
	// fulfillRouteRemote: no local copy, but a .torrent URL we may download.
	fulfillRouteRemote fulfillRoute = "remote"
	// fulfillRouteRemoteDisabled: a remote route exists and the operator has
	// not enabled remote fulfillment. Distinct from "none" so the log can
	// say which it is — "no route" and "route you turned off" send an
	// operator looking in completely different places.
	fulfillRouteRemoteDisabled fulfillRoute = "remote-disabled"
)

// chooseFulfillRoute decides how to serve one bucket. Local always wins over
// remote: it is free, and a scraped release that also sits in a declared
// folder is exactly the case the per-column cache upsert preserves.
func chooseFulfillRoute(src storage.OfferSourceRow, cfg *config.Config) fulfillRoute {
	if src.LocalPath != "" {
		return fulfillRouteLocal
	}
	if src.TorrentURL == "" {
		return fulfillRouteNone
	}
	if cfg == nil || !cfg.OfferRemoteFulfill {
		return fulfillRouteRemoteDisabled
	}
	// The size ceiling is checked here on the SCRAPED size so an obviously
	// oversized release is refused before we fetch anything. The torrent's
	// real length is checked again by the download path's disk pre-flight,
	// which is the authority — the scraped figure can be wrong or absent.
	if maxGB := cfg.OfferRemoteMaxGB; maxGB > 0 && src.SizeBytes > int64(maxGB)*(1<<30) {
		return fulfillRouteRemoteDisabled
	}
	return fulfillRouteRemote
}

// shortHash truncates an offer hash for logging without assuming its length —
// the previous inline hash[:12] would panic on a short or empty hash, and a
// panic while logging a skip is a bad trade.
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// keepClaimAlive re-claims the request periodically until the returned stop
// function is called. The site treats a re-claim by the current holder as an
// extension, so this is what lets a multi-hour download hold a 15-minute
// claim without a rival taking the request out from under it.
//
// Deliberately quiet on success and loud only on a LOST claim: if the site
// says we no longer hold it, something else is now working on this request
// and the operator wants to know why their agent kept downloading.
func (s *OfferFulfillService) keepClaimAlive(ctx context.Context, rid int) (stop func()) {
	// Half the site's 15-minute TTL: frequent enough that one failed
	// extension is not fatal, rare enough to be invisible in the site log.
	const every = 7 * time.Minute
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				held, err := s.site.OfferClaim(rid)
				if err != nil {
					log.Printf("[offer-fulfill #%d] claim keepalive error: %v", rid, err)
					continue
				}
				if !held {
					log.Printf("[offer-fulfill #%d] claim keepalive REFUSED — another offerer now holds this request", rid)
				}
			}
		}
	}()
	var once bool
	return func() {
		if !once {
			once = true
			close(done)
		}
	}
}

// remoteFetchTimeout bounds the .torrent HTTP call only — not the download.
// A tracker that has not answered in half a minute is down or throttling us.
const remoteFetchTimeout = 30 * time.Second

// httpClient is the client the .torrent fetch uses. Deliberately not shared
// with the scrapers: those run on a politeness schedule with their own
// long-lived transports, while this is one request on a request path.
func (s *OfferFulfillService) httpClient() *http.Client {
	return &http.Client{Timeout: remoteFetchTimeout}
}

// cookiesPath is the jar from offer.json, or "" when the config is absent —
// in which case the fetch simply sends no Cookie header and a private
// tracker answers with its login page, which validateTorrentBytes reports as
// ErrTorrentAuthWall rather than as a mysterious parse failure.
func (s *OfferFulfillService) cookiesPath() string {
	if s.loaded == nil {
		return ""
	}
	return s.loaded.CookiesFile
}

// browserFor returns the browser identity to present for one source: the
// source's own override when it declares one, else the config default. The
// identity matters because a tracker may fingerprint User-Agent against the
// session the jar was exported from.
func (s *OfferFulfillService) browserFor(shortName string) string {
	if s.loaded == nil {
		return ""
	}
	for _, src := range s.loaded.Sources {
		if src.ShortName == shortName && src.Browser != "" {
			return src.Browser
		}
	}
	return s.loaded.DefaultBrowser
}

// downloadRemote fetches the .torrent and downloads its contents. Returns the
// directory holding the downloaded files (caller removes it) and the release
// name to post under.
func (s *OfferFulfillService) downloadRemote(ctx context.Context, rid int, src storage.OfferSourceRow, jobName string) (dir, releaseName string, err error) {
	timeout := time.Duration(s.cfg.OfferRemoteTimeoutMin) * time.Minute
	if timeout <= 0 {
		timeout = 4 * time.Hour
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	log.Printf("[offer-fulfill #%d] downloading from %s (timeout %s)", rid, src.SourceShort, timeout)
	torrentBytes, err := fetchTorrentBytes(ctx, s.httpClient(), src.TorrentURL,
		s.browserFor(src.SourceShort), s.cookiesPath())
	if err != nil {
		return "", "", fmt.Errorf("fetch .torrent from %s: %w", src.SourceShort, err)
	}

	// Private mode: no DHT, no PEX. A tracker-sourced torrent must not
	// announce its info hash to the public network even if the .torrent
	// forgot to set info.private — that is how a private-tracker account
	// gets closed.
	session, err := DownloadPrivateTorrentBytes(ctx, torrentBytes, s.cfg, jobName, nil)
	if err != nil {
		return "", "", fmt.Errorf("download: %w", err)
	}
	defer session.Close()

	// session.Path is the file or directory anacrolix wrote. UploadDirectory
	// walks a directory, so hand it the containing one for a single-file
	// torrent and the payload directory itself for a multi-file one.
	info, err := os.Stat(session.Path)
	if err != nil {
		return "", "", fmt.Errorf("stat downloaded payload %s: %w", session.Path, err)
	}
	if info.IsDir() {
		return session.Path, filepath.Base(session.Path), nil
	}
	return filepath.Dir(session.Path), filepath.Base(session.Path), nil
}

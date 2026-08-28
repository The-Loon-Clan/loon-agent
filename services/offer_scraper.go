package services

// Tracker-scraper plumbing.
//
// A scraper produces a list of releases the user has access to on a
// remote tracker. Each row carries a raw title + size + an optional
// info_hash; the sync orchestrator pairs the scrape against a local
// folder walk (downloads_root) to find which of those releases are
// actually present on disk for fulfillment.
//
// Concrete trackers register a constructor in scraperRegistry via
// their init() function. The sync orchestrator looks up by
// source.ShortName at dispatch time and constructs an instance with
// the per-source config + run-wide knobs.

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ScrapedRelease is one row from a tracker scrape — the metadata
// the sync orchestrator needs to match a remote release to a local
// file and register the offer.
type ScrapedRelease struct {
	// RawTitle is the tracker's display title for the release. The
	// sync orchestrator normalises it for filename matching and
	// hands it to the site's TitleMatcher for catalog resolution.
	RawTitle string
	// SizeBytes is the release size as the tracker reports it. The
	// local file's actual size wins downstream (used for the bucket).
	SizeBytes int64
	// InfoHash is the BitTorrent SHA-1 of the .torrent info dict.
	// Empty when the tracker doesn't expose it on listing pages
	// (caller falls back to operator prompt at fulfill time).
	InfoHash string
	// TorrentURL is the .torrent download URL (used by the
	// torrent-delivery path in Phase 4 when the agent re-uploads to
	// ameNZB's private tracker instead of running through NNTP).
	TorrentURL string
}

// TrackerScraper is the contract every concrete tracker implements.
// Implementations should respect the run-wide MinPageDelay floor +
// MaxPages cap; the site can push a tracker-specific floor via
// scrape_min_seconds on private_trackers.
type TrackerScraper interface {
	// ShortName returns the tracker short name this scraper handles.
	// Must match a row in private_trackers on the site.
	ShortName() string
	// Scan walks the tracker (RSS, HTML, API) and emits all releases
	// the configured user has access to. Stops on first non-200 to
	// avoid pillaging a server that's rate-limiting.
	Scan() ([]ScrapedRelease, error)
}

// ScraperRunConfig is the run-wide knob set the orchestrator passes
// down — politeness floor, page cap, shared HTTP client.
type ScraperRunConfig struct {
	MinPageDelay time.Duration
	MaxPages     int
	HTTPClient   *http.Client
	// Browser + CookiesPath are the per-run defaults from offer.json.
	// Concrete scrapers grab CookieHeader(LoadCookies(...)) for the
	// tracker domain when they need auth.
	Browser     string
	CookiesPath string
	// StartOffset is where a paging scraper resumes its catalog walk —
	// the persisted cursor from the previous tick, 0 for a fresh walk.
	// Single-fetch scrapers (Nyaa's RSS) ignore it.
	StartOffset int
}

// resumableScraper is the optional face of a scraper that walks a paged
// catalog across ticks. After Scan the orchestrator persists NextOffset
// under the source's short_name; 0 means the walk completed and the next
// tick starts over from the newest.
type resumableScraper interface {
	NextOffset() int
}

// ScraperConstructor builds a scraper instance for one source. Returns
// an error when the config doesn't carry the fields the tracker
// needs (e.g. Nyaa wants username).
type ScraperConstructor func(src OfferSource, run ScraperRunConfig) (TrackerScraper, error)

var (
	scraperRegistryMu sync.RWMutex
	scraperRegistry   = map[string]ScraperConstructor{}
)

// RegisterScraper adds a constructor under one short_name. Called
// from each concrete scraper's init(). Panics on duplicate
// registration so a name clash fails at boot rather than dispatch.
func RegisterScraper(shortName string, ctor ScraperConstructor) {
	scraperRegistryMu.Lock()
	defer scraperRegistryMu.Unlock()
	if _, dup := scraperRegistry[shortName]; dup {
		panic(fmt.Sprintf("scraper already registered: %s", shortName))
	}
	scraperRegistry[shortName] = ctor
}

// LookupScraper returns the constructor for a short_name, or nil
// when no implementation is registered (sync logs + skips).
func LookupScraper(shortName string) ScraperConstructor {
	scraperRegistryMu.RLock()
	defer scraperRegistryMu.RUnlock()
	return scraperRegistry[shortName]
}

// RegisteredScrapers returns the sorted list of short_names for
// logging / debug. Cheap — only called at boot or per-tick log.
func RegisteredScrapers() []string {
	scraperRegistryMu.RLock()
	defer scraperRegistryMu.RUnlock()
	out := make([]string, 0, len(scraperRegistry))
	for k := range scraperRegistry {
		out = append(out, k)
	}
	return out
}

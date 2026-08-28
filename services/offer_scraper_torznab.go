package services

// Torznab scraper — one implementation, any tracker.
//
// Torznab is the feed dialect the *arr ecosystem standardised: Prowlarr and
// Jackett translate hundreds of trackers into it, and many private trackers
// speak it natively. Pointing this at a Prowlarr indexer's "Torznab feed"
// URL therefore covers a tracker with ZERO tracker-specific code here — the
// proxy owns the login, the cookies, the HTML parsing and its own upstream
// rate limits, all on the operator's machine where those credentials
// already live. (AnimeZ/AnimeTorrents, the first source this was built
// for, is exactly that: a Prowlarr-supported private tracker.)
//
// The walk is deliberately SLOW. Every page fetched here becomes a live
// search against the tracker behind the proxy, so the scraper takes
// max_pages_per_tick pages per sync tick with page_delay_seconds between
// them, and persists its offset in the agent DB (scrape_cursors) so a
// large catalog is covered across ticks instead of hammered in one. When a
// page comes back empty the walk has reached the end and the cursor wraps
// to zero — the next pass re-walks from the newest, which doubles as the
// heartbeat keeping registered offers fresh.
//
// Offset paging over a newest-first feed drifts as new releases land: a few
// rows repeat across pages (harmless — registration upserts) and a few slip
// a tick (caught on the next pass). Both beat the alternative of holding a
// snapshot open against a remote we do not control.

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// TorznabScraperName is the offer.json `scraper` value selecting this
// implementation. Unlike the bespoke scrapers it is NOT a tracker identity:
// the source's short_name still names the tracker row on the site.
const TorznabScraperName = "torznab"

func init() {
	RegisterScraper(TorznabScraperName, newTorznabScraper)
}

const (
	torznabDefaultPageDelay = 20 * time.Second
	torznabMinPageDelay     = 5 * time.Second
	torznabDefaultMaxPages  = 10
	torznabPageLimit        = 100
)

type torznabScraper struct {
	shortName string
	feedURL   string // up to and including /api
	apiKey    string
	cats      []int
	pageDelay time.Duration
	maxPages  int
	client    *http.Client

	startOffset int
	nextOffset  int
}

func newTorznabScraper(src OfferSource, run ScraperRunConfig) (TrackerScraper, error) {
	feed := strings.TrimRight(strings.TrimSpace(src.TorznabURL), "/?&")
	if feed == "" {
		return nil, errors.New("torznab scraper: 'torznab_url' field required in offer.json (paste the indexer's Torznab feed URL, e.g. http://localhost:9696/8/api)")
	}
	delay := torznabDefaultPageDelay
	if src.PageDelaySeconds > 0 {
		delay = time.Duration(src.PageDelaySeconds) * time.Second
		if delay < torznabMinPageDelay {
			delay = torznabMinPageDelay
		}
	}
	maxPages := src.MaxPagesPerTick
	if maxPages <= 0 {
		maxPages = torznabDefaultMaxPages
	}
	if run.MaxPages > 0 && maxPages > run.MaxPages {
		maxPages = run.MaxPages
	}
	return &torznabScraper{
		shortName:   src.ShortName,
		feedURL:     feed,
		apiKey:      strings.TrimSpace(src.APIKey),
		cats:        src.CategoryIDs,
		pageDelay:   delay,
		maxPages:    maxPages,
		client:      run.HTTPClient,
		startOffset: run.StartOffset,
		nextOffset:  run.StartOffset,
	}, nil
}

func (t *torznabScraper) ShortName() string { return t.shortName }

// NextOffset reports where the next tick should resume: 0 after the walk
// reached the feed's end (start over from the newest), otherwise the item
// offset one past what this Scan consumed. The orchestrator persists it.
func (t *torznabScraper) NextOffset() int { return t.nextOffset }

// ─── feed document ─────────────────────────────────────────────────

// Namespaced elements are matched by local name, same as the Nyaa scraper:
// <torznab:attr name="size" value="..."/> decodes via `xml:"attr"`.
type torznabDoc struct {
	XMLName xml.Name       `xml:"rss"`
	Channel torznabChannel `xml:"channel"`
}

type torznabChannel struct {
	Items []torznabItem `xml:"item"`
}

type torznabItem struct {
	Title     string           `xml:"title"`
	Link      string           `xml:"link"`
	Size      string           `xml:"size"` // plain-element form some feeds use
	Enclosure torznabEnclosure `xml:"enclosure"`
	Attrs     []torznabAttr    `xml:"attr"` // torznab:attr name/value pairs
}

type torznabEnclosure struct {
	URL    string `xml:"url,attr"`
	Length string `xml:"length,attr"`
}

type torznabAttr struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

func (it torznabItem) attr(name string) string {
	for _, a := range it.Attrs {
		if strings.EqualFold(a.Name, name) {
			return a.Value
		}
	}
	return ""
}

// sizeBytes prefers the torznab attr (always bytes), then the plain <size>
// element, then the enclosure length. All three are integer byte counts;
// an unparseable value degrades to 0 rather than failing the page.
func (it torznabItem) sizeBytes() int64 {
	for _, raw := range []string{it.attr("size"), it.Size, it.Enclosure.Length} {
		if raw == "" {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// ─── the walk ──────────────────────────────────────────────────────

func (t *torznabScraper) pageURL(offset int) string {
	var sb strings.Builder
	sb.WriteString(t.feedURL)
	sb.WriteString("?t=search&extended=1")
	fmt.Fprintf(&sb, "&limit=%d&offset=%d", torznabPageLimit, offset)
	if t.apiKey != "" {
		fmt.Fprintf(&sb, "&apikey=%s", t.apiKey)
	}
	if len(t.cats) > 0 {
		parts := make([]string, len(t.cats))
		for i, c := range t.cats {
			parts[i] = strconv.Itoa(c)
		}
		fmt.Fprintf(&sb, "&cat=%s", strings.Join(parts, ","))
	}
	return sb.String()
}

func (t *torznabScraper) fetchPage(offset int) ([]torznabItem, error) {
	req, err := http.NewRequest("GET", t.pageURL(offset), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "loon-agent/torznab")
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml")
	client := t.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("torznab %s offset %d: HTTP %d: %s", t.shortName, offset, resp.StatusCode, body)
	}
	var doc torznabDoc
	if err := xml.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("torznab %s decode: %w", t.shortName, err)
	}
	return doc.Channel.Items, nil
}

// Scan walks up to maxPages pages from startOffset. A failed page after a
// successful one returns what was gathered (with the cursor parked at the
// failure point) rather than throwing the tick away; a failure on the very
// first page is a real error the operator should see.
func (t *torznabScraper) Scan() ([]ScrapedRelease, error) {
	out := make([]ScrapedRelease, 0, torznabPageLimit)
	offset := t.startOffset
	for page := 0; page < t.maxPages; page++ {
		if page > 0 {
			time.Sleep(t.pageDelay)
		}
		items, err := t.fetchPage(offset)
		if err != nil {
			if len(out) == 0 {
				return nil, err
			}
			t.nextOffset = offset
			return out, nil
		}
		if len(items) == 0 {
			// End of the feed: wrap so the next tick re-walks from the
			// newest — the re-walk is also the offer heartbeat.
			t.nextOffset = 0
			return out, nil
		}
		for _, it := range items {
			title := strings.TrimSpace(it.Title)
			if title == "" {
				continue
			}
			link := strings.TrimSpace(it.Link)
			if link == "" {
				link = strings.TrimSpace(it.Enclosure.URL)
			}
			out = append(out, ScrapedRelease{
				RawTitle:   title,
				SizeBytes:  it.sizeBytes(),
				InfoHash:   strings.ToLower(strings.TrimSpace(it.attr("infohash"))),
				TorrentURL: link,
			})
		}
		offset += len(items)
	}
	t.nextOffset = offset
	return out, nil
}

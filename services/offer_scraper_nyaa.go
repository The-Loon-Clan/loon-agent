package services

// Nyaa scraper — reads the per-user RSS feed at
//   <base_url>/user/<username>?page=rss
// and emits one ScrapedRelease per item.
//
// Why RSS instead of HTML: the HTML structure changes; the RSS feed
// is documented and stable, includes a custom <nyaa:infoHash> element
// for free, and skips the pagination dance (Nyaa caps the feed at
// ~75 items, which is enough for the typical uploader's recent
// catalog; older releases can be reached by RSS-by-search later).
//
// Config (in offer.json):
//   {
//     "type": "scraper",
//     "short_name": "nyaa",
//     "base_url": "https://nyaa.si",
//     "username": "MyUsername",
//     "downloads_root": "/downloads/nyaa",    // optional, used by sync
//     "browser": "chrome"                     // optional override
//   }
//
// The agent doesn't authenticate against Nyaa for the public feed —
// no cookies needed. Browser fingerprint is still applied so the
// fetch looks like a normal RSS reader, not a scraper.

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const NyaaShortName = "nyaa"

func init() {
	RegisterScraper(NyaaShortName, newNyaaScraper)
}

type nyaaScraper struct {
	baseURL  string
	username string
	browser  string
	run      ScraperRunConfig
}

func newNyaaScraper(src OfferSource, run ScraperRunConfig) (TrackerScraper, error) {
	base := strings.TrimRight(src.BaseURL, "/")
	if base == "" {
		base = "https://nyaa.si"
	}
	// Username is required — nyaa has no "all uploaders" endpoint
	// that we'd want to scrape (that would be the entire site).
	username := strings.TrimSpace(extraField(src, "username"))
	if username == "" {
		return nil, errors.New("nyaa scraper: 'username' field required in offer.json")
	}
	browser := src.Browser
	if browser == "" {
		browser = run.Browser
	}
	return &nyaaScraper{
		baseURL:  base,
		username: username,
		browser:  browser,
		run:      run,
	}, nil
}

func (n *nyaaScraper) ShortName() string { return NyaaShortName }

// rssDoc mirrors the subset of Nyaa's RSS we care about. Nyaa's feed
// uses a custom namespace `nyaa` for size + info_hash; encoding/xml
// resolves the namespace via the prefix attribute so a plain
// `Size string \`xml:"size"\`` (matching local name) is enough.
type rssDoc struct {
	XMLName xml.Name  `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Items []rssItem `xml:"item"`
}

type rssItem struct {
	Title    string `xml:"title"`
	Link     string `xml:"link"`           // /download/<id>.torrent
	Size     string `xml:"size"`           // "1.4 GiB" — nyaa namespace
	InfoHash string `xml:"infoHash"`       // 40 hex chars — nyaa namespace
	GUID     string `xml:"guid"`
}

// Scan fetches the RSS feed once + decodes it. Pagination isn't
// useful — Nyaa caps the per-user feed at the most recent ~75 items.
func (n *nyaaScraper) Scan() ([]ScrapedRelease, error) {
	rssURL := fmt.Sprintf("%s/user/%s?page=rss", n.baseURL, url.PathEscape(n.username))
	req, err := http.NewRequest("GET", rssURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range BrowserHeaders(n.browser) {
		req.Header.Set(k, v)
	}
	// RSS feeds prefer the XML Accept; override the default.
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml")

	client := n.run.HTTPClient
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
		return nil, fmt.Errorf("nyaa rss %d: %s", resp.StatusCode, body)
	}
	var doc rssDoc
	if err := xml.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("nyaa rss decode: %w", err)
	}
	out := make([]ScrapedRelease, 0, len(doc.Channel.Items))
	for _, it := range doc.Channel.Items {
		size := parseNyaaSize(it.Size)
		torrentURL := it.Link
		if !strings.HasPrefix(torrentURL, "http") {
			torrentURL = n.baseURL + it.Link
		}
		out = append(out, ScrapedRelease{
			RawTitle:   strings.TrimSpace(it.Title),
			SizeBytes:  size,
			InfoHash:   strings.ToLower(strings.TrimSpace(it.InfoHash)),
			TorrentURL: torrentURL,
		})
	}
	return out, nil
}

// ─── size parsing ──────────────────────────────────────────────────

// Nyaa's <nyaa:size> looks like "1.4 GiB" / "732.5 MiB" / "401.2 KiB".
// Parse to bytes; unknown unit returns 0 so downstream gets a
// well-defined zero rather than a parse panic.
var reNyaaSize = regexp.MustCompile(`(?i)^([\d.]+)\s*([KMGTPE]i?B)$`)

func parseNyaaSize(s string) int64 {
	m := reNyaaSize.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	unit := strings.ToUpper(m[2])
	var mult float64
	switch unit {
	case "B":
		mult = 1
	case "KB":
		mult = 1000
	case "KIB":
		mult = 1024
	case "MB":
		mult = 1000 * 1000
	case "MIB":
		mult = 1024 * 1024
	case "GB":
		mult = 1000 * 1000 * 1000
	case "GIB":
		mult = 1024 * 1024 * 1024
	case "TB":
		mult = 1000 * 1000 * 1000 * 1000
	case "TIB":
		mult = 1024 * 1024 * 1024 * 1024
	default:
		return 0
	}
	return int64(n * mult)
}

// extraField is a tiny helper for fields we don't have first-class
// struct slots for (Nyaa wants `username`). The orchestrator passes
// these through src.Extra map; we accept either a flat string from
// CookiesFile-style top-level or fall back to nothing. Keeps the
// scraper resilient to schema evolution.
func extraField(src OfferSource, name string) string {
	switch name {
	case "username":
		return src.Username
	}
	return ""
}

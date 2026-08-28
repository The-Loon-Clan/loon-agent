package services

// Torznab scraper tests. The contract under test is the WALK, not any one
// tracker: paging resumes from the cursor, stops at the page budget, wraps
// at the feed's end, keeps a partial harvest when a later page fails, and
// reads the three places a feed hides its numbers (torznab attr, plain
// <size>, enclosure length). Tests construct the struct directly so the
// politeness floor in the constructor cannot slow the suite.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const torznabTestHash = "aabbccddeeff00112233445566778899aabbccdd"

func torznabTestItem(i int) string {
	return fmt.Sprintf(`    <item>
      <title>[Group] Example Show - %02d (1080p).mkv</title>
      <link>http://proxy.local/8/download?file=%d</link>
      <enclosure url="http://proxy.local/8/download?file=%d" length="1471026299" type="application/x-bittorrent"/>
      <torznab:attr name="category" value="5070"/>
      <torznab:attr name="infohash" value="%s"/>
      <torznab:attr name="size" value="1471026299"/>
    </item>`, i, i, i, strings.ToUpper(torznabTestHash))
}

func torznabTestPage(items ...string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <title>Test Indexer</title>
` + strings.Join(items, "\n") + `
  </channel>
</rss>`
}

// newTorznabTestScraper builds the scraper against a test server with a
// millisecond page delay — the constructor's politeness floor is for
// production, not for the suite.
func newTorznabTestScraper(srv *httptest.Server, start, maxPages int) *torznabScraper {
	return &torznabScraper{
		shortName:   "animez",
		feedURL:     srv.URL + "/api",
		apiKey:      "k3y",
		cats:        []int{5070, 2020},
		pageDelay:   time.Millisecond,
		maxPages:    maxPages,
		client:      srv.Client(),
		startOffset: start,
		nextOffset:  start,
	}
}

func TestTorznabScanParsesItemsAndWraps(t *testing.T) {
	var queries []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		if r.URL.Query().Get("offset") == "0" {
			fmt.Fprint(w, torznabTestPage(torznabTestItem(1), torznabTestItem(2)))
			return
		}
		fmt.Fprint(w, torznabTestPage()) // the end of the feed
	}))
	defer srv.Close()

	sc := newTorznabTestScraper(srv, 0, 10)
	got, err := sc.Scan()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("scanned %d releases, want 2", len(got))
	}
	r := got[0]
	if r.RawTitle != "[Group] Example Show - 01 (1080p).mkv" {
		t.Errorf("title = %q", r.RawTitle)
	}
	if r.SizeBytes != 1471026299 {
		t.Errorf("size = %d, want 1471026299", r.SizeBytes)
	}
	if r.InfoHash != torznabTestHash {
		t.Errorf("infohash = %q, want lowercased %q", r.InfoHash, torznabTestHash)
	}
	if r.TorrentURL != "http://proxy.local/8/download?file=1" {
		t.Errorf("torrent url = %q", r.TorrentURL)
	}
	// An empty page means the walk finished: wrap so the next tick
	// re-walks from the newest.
	if sc.NextOffset() != 0 {
		t.Errorf("NextOffset = %d after reaching the end, want 0", sc.NextOffset())
	}
	// The wire carries the auth and the category scope on every page.
	q := queries[0]
	if q.Get("t") != "search" || q.Get("apikey") != "k3y" || q.Get("cat") != "5070,2020" {
		t.Errorf("first page query = %v", q)
	}
	if q.Get("limit") != strconv.Itoa(torznabPageLimit) {
		t.Errorf("limit = %q", q.Get("limit"))
	}
}

func TestTorznabScanStopsAtThePageBudgetAndParksTheCursor(t *testing.T) {
	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		fmt.Fprint(w, torznabTestPage(torznabTestItem(1), torznabTestItem(2), torznabTestItem(3)))
	}))
	defer srv.Close()

	sc := newTorznabTestScraper(srv, 0, 2)
	got, err := sc.Scan()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if pages != 2 {
		t.Errorf("fetched %d pages, want exactly the budget of 2", pages)
	}
	if len(got) != 6 {
		t.Errorf("scanned %d releases, want 6", len(got))
	}
	// Item-based offset: the next tick resumes one past what was consumed.
	if sc.NextOffset() != 6 {
		t.Errorf("NextOffset = %d, want 6", sc.NextOffset())
	}
}

func TestTorznabScanResumesFromTheCursor(t *testing.T) {
	var firstOffset string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if firstOffset == "" {
			firstOffset = r.URL.Query().Get("offset")
		}
		fmt.Fprint(w, torznabTestPage())
	}))
	defer srv.Close()

	sc := newTorznabTestScraper(srv, 40, 3)
	if _, err := sc.Scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if firstOffset != "40" {
		t.Errorf("first page asked offset %q, want the persisted 40", firstOffset)
	}
}

func TestTorznabScanKeepsThePartialHarvestOnAMidWalkFailure(t *testing.T) {
	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if pages > 1 {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, torznabTestPage(torznabTestItem(1), torznabTestItem(2), torznabTestItem(3)))
	}))
	defer srv.Close()

	sc := newTorznabTestScraper(srv, 0, 5)
	got, err := sc.Scan()
	if err != nil {
		t.Fatalf("a mid-walk failure threw away the harvested page: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("kept %d releases, want the 3 from the good page", len(got))
	}
	// The cursor parks at the failure point so the next tick retries
	// there instead of restarting the catalog.
	if sc.NextOffset() != 3 {
		t.Errorf("NextOffset = %d, want 3", sc.NextOffset())
	}
}

func TestTorznabScanFirstPageFailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusForbidden)
	}))
	defer srv.Close()

	sc := newTorznabTestScraper(srv, 12, 5)
	if _, err := sc.Scan(); err == nil {
		t.Fatal("a first-page failure must surface — the operator's config is wrong")
	}
	if sc.NextOffset() != 12 {
		t.Errorf("NextOffset moved to %d on total failure, want the untouched 12", sc.NextOffset())
	}
}

func TestTorznabConstructorValidatesAndDefaults(t *testing.T) {
	if _, err := newTorznabScraper(OfferSource{ShortName: "animez"}, ScraperRunConfig{}); err == nil {
		t.Error("missing torznab_url must refuse at init, not 404 at scan")
	}
	s, err := newTorznabScraper(OfferSource{
		ShortName:  "animez",
		TorznabURL: "http://localhost:9696/8/api/",
	}, ScraperRunConfig{StartOffset: 7})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	tz := s.(*torznabScraper)
	if tz.feedURL != "http://localhost:9696/8/api" {
		t.Errorf("feed url not normalised: %q", tz.feedURL)
	}
	if tz.pageDelay != torznabDefaultPageDelay || tz.maxPages != torznabDefaultMaxPages {
		t.Errorf("defaults = %v/%d, want %v/%d", tz.pageDelay, tz.maxPages, torznabDefaultPageDelay, torznabDefaultMaxPages)
	}
	if tz.startOffset != 7 || tz.NextOffset() != 7 {
		t.Errorf("cursor not adopted: start %d next %d", tz.startOffset, tz.NextOffset())
	}
	if s.ShortName() != "animez" {
		t.Errorf("ShortName = %q — the tracker identity, not the implementation, names the source", s.ShortName())
	}
}

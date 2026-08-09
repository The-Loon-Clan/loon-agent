package services

// Fetching a .torrent for remote-source offer fulfillment.
//
// Separate from the scrapers because it is not scraping: the URL was already
// discovered at scan time and persisted, and this is the one HTTP call that
// turns it into bytes an torrent client can add. Kept out of offer_fulfill.go
// so the validation below is testable without a tracker.
//
// The whole reason this file has more than four lines is that a tracker
// answering "you are not logged in" does NOT answer 401 or 403. It answers
// 200 with an HTML login page, and an HTTP client that only checks the status
// code hands those bytes on as a .torrent. Downstream that surfaces as
// "malformed .torrent (no info dict)" thirty seconds later, or — worse — as a
// silent skip, which is exactly the shape of the AniRena anti-bot page already
// on record in the site's error log. So the response is validated as bencode
// before anyone else sees it, and an HTML body is reported as the credential
// problem it actually is.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxTorrentBytes caps what we will read into memory for one .torrent. Real
// files are kilobytes — a 4 MB torrent would be a ~40 TB release at the usual
// piece sizes. The cap exists because the URL comes from scraped content: a
// hostile or simply wrong one must fail fast instead of streaming until the
// agent runs out of RAM.
const maxTorrentBytes = 4 << 20

// ErrTorrentAuthWall is returned when the tracker served a web page instead of
// a torrent — nearly always an expired or missing cookie jar. Distinguished
// from a parse failure because the operator action is different: refresh the
// cookies, not investigate the release.
var ErrTorrentAuthWall = errors.New("tracker returned a web page instead of a .torrent (cookies expired?)")

// fetchTorrentBytes downloads and validates one .torrent.
//
// cookiesPath/browser come from the offer config so the request looks like the
// browser whose session the jar was exported from; a tracker that fingerprints
// User-Agent against the session will otherwise reject a jar that is
// perfectly valid.
func fetchTorrentBytes(ctx context.Context, client *http.Client, torrentURL, browser, cookiesPath string) ([]byte, error) {
	if torrentURL == "" {
		return nil, errors.New("empty torrent URL")
	}
	u, err := url.Parse(torrentURL)
	if err != nil {
		return nil, fmt.Errorf("parse torrent URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("refusing non-HTTP torrent URL scheme %q", u.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, torrentURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range BrowserHeaders(browser) {
		req.Header.Set(k, v)
	}
	if ck := CookieHeader(LoadCookies(cookiesPath, u.Hostname())); ck != "" {
		req.Header.Set("Cookie", ck)
	}
	// Ask for a torrent explicitly. Some trackers content-negotiate their
	// download endpoint and will hand back HTML to a browser-looking Accept.
	req.Header.Set("Accept", "application/x-bittorrent,*/*")

	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch .torrent: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch .torrent: HTTP %d", resp.StatusCode)
	}

	// One extra byte so a file exactly at the cap is still recognised as over
	// it rather than silently truncated into a corrupt torrent.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTorrentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read .torrent: %w", err)
	}
	if len(body) > maxTorrentBytes {
		return nil, fmt.Errorf("refusing .torrent larger than %d bytes — the URL probably does not point at a torrent", maxTorrentBytes)
	}
	if err := validateTorrentBytes(body); err != nil {
		return nil, err
	}
	return body, nil
}

// validateTorrentBytes checks the payload is plausibly a .torrent before it
// reaches the torrent client. Deliberately shallow — a full bencode parse
// belongs to the client, and this only has to catch the case the client
// reports thirty seconds late and unhelpfully.
func validateTorrentBytes(body []byte) error {
	if len(body) == 0 {
		return errors.New("empty .torrent response")
	}
	// A bencoded dict starts with 'd'. HTML starts with '<' after optional
	// whitespace and possibly a BOM, so check that first to give the useful
	// error. The BOM is written as bytes rather than as a literal character
	// because a real U+FEFF in Go source is a compile error ("invalid BOM in
	// the middle of the file").
	const utf8BOM = "\xef\xbb\xbf"
	head := strings.TrimPrefix(string(body[:min(len(body), 512)]), utf8BOM)
	lower := strings.ToLower(strings.TrimSpace(head))
	if strings.HasPrefix(lower, "<") || strings.HasPrefix(lower, "<!doctype") ||
		strings.Contains(lower, "<html") {
		return ErrTorrentAuthWall
	}
	if body[0] != 'd' {
		return fmt.Errorf("response is not bencode (first byte %q) — not a .torrent", body[0])
	}
	// Every .torrent carries an info dict; its absence is the specific thing
	// that makes the torrent client wait for metadata it will never get.
	if !strings.Contains(string(body), "4:info") {
		return errors.New("bencode has no info dict — magnet-only or truncated .torrent")
	}
	return nil
}

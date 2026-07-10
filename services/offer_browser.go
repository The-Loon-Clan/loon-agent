package services

// Browser fingerprint helpers — pick a stable User-Agent string per
// source. Mirrors the Python browser_profile.py the prior agent used.
//
// Why one UA per source (not rotating): trackers fingerprint on the
// (UA, cookies, IP) triple as a session signature. Rotating the UA
// mid-session is a stronger bot signal than holding the triple
// steady. Pick the SAME browser you used when you imported cookies
// from your local profile.
//
// Update window: when major browsers drift more than ~6 months past
// these strings, the UA reads as "old browser" and some strict checks
// will downgrade or refuse. Worth refreshing on a yearly cadence.

const (
	BrowserChrome  = "chrome"
	BrowserFirefox = "firefox"
	BrowserEdge    = "edge"
	BrowserSafari  = "safari"
)

// userAgents holds the canonical UA string per allowed browser key.
// Strings dated late 2025 — see file header for the refresh window.
var userAgents = map[string]string{
	BrowserChrome: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) " +
		"Chrome/131.0.0.0 Safari/537.36",
	BrowserFirefox: "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:133.0) " +
		"Gecko/20100101 Firefox/133.0",
	BrowserEdge: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) " +
		"Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0",
	BrowserSafari: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
		"AppleWebKit/605.1.15 (KHTML, like Gecko) " +
		"Version/17.4 Safari/605.1.15",
}

// UserAgentFor returns the canonical UA for the named browser. Falls
// back to Chrome on Windows for unknown / empty keys so a typo never
// kills a scrape run.
func UserAgentFor(browser string) string {
	if ua, ok := userAgents[browser]; ok {
		return ua
	}
	return userAgents[BrowserChrome]
}

// BrowserHeaders is the full set of headers a real browser sends with
// a top-level navigation request. Caller merges these into the
// scraper's HTTP client default headers. Cookies aren't included
// here — they come from LoadCookies(domain) separately.
//
// The Sec-Fetch-* set models a "user clicked a link" navigation; for
// XHR-style requests (e.g. an API call inside a tracker page) the
// caller should override Sec-Fetch-Mode / Dest accordingly. We keep
// the navigation defaults because tracker pages are typically the
// first request in a scrape session.
func BrowserHeaders(browser string) map[string]string {
	return map[string]string{
		"User-Agent": UserAgentFor(browser),
		"Accept": "text/html,application/xhtml+xml,application/xml;q=0.9," +
			"image/avif,image/webp,*/*;q=0.8",
		"Accept-Language":           "en-US,en;q=0.9",
		"Accept-Encoding":           "gzip, deflate, br",
		"Sec-Fetch-Site":            "same-origin",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-User":            "?1",
		"Sec-Fetch-Dest":            "document",
		"Upgrade-Insecure-Requests": "1",
		"DNT":                       "1",
	}
}

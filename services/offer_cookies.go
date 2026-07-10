package services

// Cookie loader for tracker scrapers.
//
// Reads the JSON jar produced by the host-side cookie import helper
// (the one that ran browser_cookie3 outside the container; lives in
// the offer-feature docs, not in this repo). Schema:
//
//   {
//     "tracker.example.com": {"session_id": "...", "remember": "..."},
//     "nyaa.si":             {"session": "..."}
//   }
//
// Domain match is suffix-based — querying "tracker.example.com"
// picks up a "example.com" entry too, mirroring the browser's own
// cookie-scope semantics. Empty map on missing file / parse error
// so a stale jar never crashes the scrape loop; the caller logs the
// empty result and proceeds (tracker will respond with login page).

import (
	"encoding/json"
	"os"
	"strings"
)

// LoadCookies returns the cookie name→value map for one domain.
// `path` should be the jar file path from offer.yml's cookies_file
// (or an empty string to skip). Empty result is the "no cookies"
// signal — caller proceeds without setting any Cookie header.
func LoadCookies(path, domain string) map[string]string {
	if path == "" || domain == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var jar map[string]map[string]string
	if err := json.Unmarshal(raw, &jar); err != nil {
		return nil
	}
	// Exact match wins.
	if cookies, ok := jar[domain]; ok {
		return cookies
	}
	// Suffix walk — "tracker.example.com" matches "example.com".
	for stored, cookies := range jar {
		if domain == stored || strings.HasSuffix(domain, "."+stored) {
			return cookies
		}
	}
	return nil
}

// CookieHeader serialises a cookie map into one Cookie header value
// suitable for `req.Header.Set("Cookie", ...)`. Returns "" for an
// empty map so the caller can use a single Set with no nil check.
func CookieHeader(cookies map[string]string) string {
	if len(cookies) == 0 {
		return ""
	}
	parts := make([]string, 0, len(cookies))
	for name, value := range cookies {
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; ")
}

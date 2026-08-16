package services

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/the-loon-clan/loon-agent/config"
	"github.com/the-loon-clan/loon-agent/storage"
)

func TestChooseFulfillRoute(t *testing.T) {
	const gb = int64(1) << 30
	on := &config.Config{OfferRemoteFulfill: true, OfferRemoteMaxGB: 25}
	off := &config.Config{OfferRemoteFulfill: false, OfferRemoteMaxGB: 25}
	uncapped := &config.Config{OfferRemoteFulfill: true, OfferRemoteMaxGB: 0}

	cases := []struct {
		name string
		src  storage.OfferSourceRow
		cfg  *config.Config
		want fulfillRoute
	}{
		{"nothing cached", storage.OfferSourceRow{}, on, fulfillRouteNone},
		{"local file", storage.OfferSourceRow{LocalPath: "/data/x.mkv"}, on, fulfillRouteLocal},
		// Local wins even when remote is available and enabled: a local copy
		// costs no bandwidth and no tracker ratio.
		{"both routes prefers local",
			storage.OfferSourceRow{LocalPath: "/data/x.mkv", TorrentURL: "https://t/x.torrent"}, on, fulfillRouteLocal},
		// ...and even when remote fulfillment is switched off, so turning the
		// feature off never breaks the route that always worked.
		{"both routes with remote off still serves local",
			storage.OfferSourceRow{LocalPath: "/data/x.mkv", TorrentURL: "https://t/x.torrent"}, off, fulfillRouteLocal},
		{"remote only, enabled",
			storage.OfferSourceRow{TorrentURL: "https://t/x.torrent", SourceShort: "nyaa"}, on, fulfillRouteRemote},
		{"remote only, disabled",
			storage.OfferSourceRow{TorrentURL: "https://t/x.torrent", SourceShort: "nyaa"}, off, fulfillRouteRemoteDisabled},
		{"remote over the size ceiling is refused",
			storage.OfferSourceRow{TorrentURL: "https://t/x.torrent", SizeBytes: 40 * gb}, on, fulfillRouteRemoteDisabled},
		{"remote under the size ceiling is allowed",
			storage.OfferSourceRow{TorrentURL: "https://t/x.torrent", SizeBytes: 20 * gb}, on, fulfillRouteRemote},
		{"zero ceiling means no ceiling",
			storage.OfferSourceRow{TorrentURL: "https://t/x.torrent", SizeBytes: 900 * gb}, uncapped, fulfillRouteRemote},
		// A nil config must never be read as "remote enabled" — the feature
		// spends the operator's bandwidth and has to fail closed.
		{"nil config fails closed",
			storage.OfferSourceRow{TorrentURL: "https://t/x.torrent"}, nil, fulfillRouteRemoteDisabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooseFulfillRoute(tc.src, tc.cfg); got != tc.want {
				t.Errorf("route = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestShortHashDoesNotPanicOnShortInput(t *testing.T) {
	for _, in := range []string{"", "abc", "0123456789ab", "0123456789abcdef"} {
		got := shortHash(in)
		if len(got) > 12 {
			t.Errorf("shortHash(%q) = %q, longer than 12", in, got)
		}
	}
}

// browserFor picks the per-source override when present, else the default.
// The identity is not cosmetic: a tracker may fingerprint User-Agent against
// the session the cookie jar was exported from and reject a valid jar.
func TestBrowserForPrefersSourceOverride(t *testing.T) {
	s := &OfferFulfillService{loaded: &OfferConfig{
		DefaultBrowser: "firefox",
		Sources: []OfferSource{
			{ShortName: "nyaa", Browser: "chrome"},
			{ShortName: "other"},
		},
	}}
	if got := s.browserFor("nyaa"); got != "chrome" {
		t.Errorf("browserFor(nyaa) = %q, want the source override", got)
	}
	if got := s.browserFor("other"); got != "firefox" {
		t.Errorf("browserFor(other) = %q, want the default", got)
	}
	if got := s.browserFor("unknown"); got != "firefox" {
		t.Errorf("browserFor(unknown) = %q, want the default", got)
	}
	// No config at all must be answerable rather than a nil deref: the
	// local-file route works without offer.json and must keep working.
	empty := &OfferFulfillService{}
	if got := empty.browserFor("nyaa"); got != "" {
		t.Errorf("browserFor with no config = %q, want empty", got)
	}
	if got := empty.cookiesPath(); got != "" {
		t.Errorf("cookiesPath with no config = %q, want empty", got)
	}
}

// The site's published path is RELATIVE to a root it has never been told
// about, so resolving it is this side's job. The first cut cached it verbatim:
// a file at the top of a root has an empty dir_path, so what arrived was a
// bare filename, the loop saw "a path is present" and chose the local route,
// then failed os.Stat and skipped. Nothing in the log said why.
func TestResolveAgainstRoots(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "anime", "show")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	atRoot := filepath.Join(dir, "top.mkv")
	deep := filepath.Join(nested, "ep01.mkv")
	for _, p := range []string{atRoot, deep} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	other := t.TempDir()

	for _, tc := range []struct {
		name  string
		roots []string
		rel   string
		want  string
	}{
		{"file at the top of a root", []string{dir}, "top.mkv", atRoot},
		{"nested, forward slashes as the site stores them", []string{dir}, "anime/show/ep01.mkv", deep},
		// The point of taking a LIST: an operator with two roots should not
		// have to care which one a given offer came from.
		{"second root wins", []string{other, dir}, "top.mkv", atRoot},
		{"absent under every root", []string{other}, "top.mkv", ""},
		{"no roots configured", nil, "top.mkv", ""},
		{"absolute path that exists is taken as-is", nil, atRoot, atRoot},
		{"absolute path that does not exist is refused", nil, filepath.Join(dir, "nope.mkv"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveAgainstRoots(tc.roots, tc.rel); got != tc.want {
				t.Errorf("resolveAgainstRoots(%v, %q) = %q, want %q", tc.roots, tc.rel, got, tc.want)
			}
		})
	}
}

// Caching an unresolvable path is worse than caching nothing: chooseFulfillRoute
// reads "a local path exists" off the row and commits to the local route, so the
// request is claimed and then abandoned instead of being left for an agent that
// can actually serve it.
func TestUnresolvedPathIsNotCached(t *testing.T) {
	if got := resolveAgainstRoots([]string{t.TempDir()}, "does/not/exist.mkv"); got != "" {
		t.Errorf("resolved a missing file to %q", got)
	}
}

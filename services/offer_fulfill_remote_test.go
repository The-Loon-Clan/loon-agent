package services

import (
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

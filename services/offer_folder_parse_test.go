package services

import "testing"

// Season/episode parsing, which is not cosmetic: both values feed
// ComputeOfferHash, so a name that parses to (0,0) collapses every episode of
// a season onto ONE offer bucket. Publishing twelve episodes then creates one
// offer, and a member requesting it receives whichever episode the offerer
// resolved. That is the failure this file exists to prevent.

func TestParseSeasonEpisode(t *testing.T) {
	cases := []struct {
		name    string
		season  int
		episode int
	}{
		// Unambiguous — both stated.
		{"Show.Name.S02E05.1080p.mkv", 2, 5},
		{"show name s02e05.mkv", 2, 5},
		{"[Judas] Liar Game - S01E17.mkv", 1, 17},

		// THE REGRESSION. The fansub convention, and the most common anime
		// naming there is. Every one of these returned (0,0) before.
		{"[SubsPlease] Dr. Stone S3 - 07 (1080p) [A1B2C3D4].mkv", 3, 7},
		{"[SubsPlease] Frieren - 12 (1080p) [DEADBEEF].mkv", 1, 12},
		{"[Erai-raws] Show Title - 003 [1080p].mkv", 1, 3},
		{"[Group] Some Show S2 - 01 (720p).mkv", 2, 1},
		{"Show Name - 07v2 (1080p).mkv", 1, 7},

		// Episode-only forms keep working, and now pick up a stated season
		// instead of assuming 1.
		{"Show - ep01.mkv", 1, 1},
		{"Show S4 - ep09.mkv", 4, 9},
		{"Show.Name.Season 2.episode 3.mkv", 2, 3},

		// NOT episodes. A movie with a year, a resolution, a release group
		// number — every one of these is a dash followed by digits.
		{"Some.Movie.2024.1080p.BluRay.x264-GROUP.mkv", 0, 0},
		{"Some Movie - 2024 (1080p).mkv", 0, 0},
		{"Concert Film - 4k.mkv", 0, 0},
		{"Album - 320kbps.mkv", 0, 0},
		{"creditless-op.mkv", 0, 0},
		{"NCIS3.mkv", 0, 0},
	}
	for _, tc := range cases {
		got := parseScannedFile("/lib/"+tc.name, 1<<20)
		if got.Season != tc.season || got.Episode != tc.episode {
			t.Errorf("%q -> S%dE%d, want S%dE%d",
				tc.name, got.Season, got.Episode, tc.season, tc.episode)
		}
	}
}

// THE ONE THAT MATTERS. Distinct episodes must produce distinct
// (season, episode) pairs — that pair is what keeps their offer buckets apart.
func TestEpisodesOfOneSeasonStayDistinct(t *testing.T) {
	names := []string{
		"[SubsPlease] Dr. Stone S3 - 07 (1080p) [A1B2C3D4].mkv",
		"[SubsPlease] Dr. Stone S3 - 08 (1080p) [E5F6G7H8].mkv",
		"[SubsPlease] Dr. Stone S3 - 09 (1080p) [12345678].mkv",
	}
	seen := map[[2]int]string{}
	for _, n := range names {
		p := parseScannedFile("/lib/"+n, 700<<20)
		key := [2]int{p.Season, p.Episode}
		if prev, dup := seen[key]; dup {
			t.Fatalf("%q and %q both parsed to S%dE%d — they would share one offer bucket",
				prev, n, key[0], key[1])
		}
		seen[key] = n
		if p.Season != 3 {
			t.Errorf("%q season = %d, want 3", n, p.Season)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("three episodes produced %d distinct identities", len(seen))
	}
}

// The resolution and source hints ride along on the same parse and are also
// part of the bucket identity.
func TestParseHintsAlongsideEpisode(t *testing.T) {
	p := parseScannedFile("/lib/[Group] Show S2 - 04 (1080p) [WEB-DL].mkv", 1<<20)
	if p.Season != 2 || p.Episode != 4 {
		t.Errorf("S%dE%d, want S2E4", p.Season, p.Episode)
	}
	if p.Resolution != "1080p" {
		t.Errorf("resolution = %q", p.Resolution)
	}
	if p.SourceTag != "web-dl" {
		t.Errorf("source tag = %q, want web-dl", p.SourceTag)
	}
}

// CJK names must parse identically — Go's RE2 has no non-ASCII word boundary,
// which is the bug the resolution regex already carries a comment about.
func TestParseHandlesCJK(t *testing.T) {
	p := parseScannedFile("/lib/【推しの子】 - 01 (1080p).mkv", 1<<20)
	if p.Episode != 1 {
		t.Errorf("episode = %d, want 1 — the dash form failed on a CJK title", p.Episode)
	}
	if p.Resolution != "1080p" {
		t.Errorf("resolution = %q, want 1080p", p.Resolution)
	}
}

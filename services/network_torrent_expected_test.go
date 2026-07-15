package services

import "testing"

// Whether anacrolix wraps content in a <torrent-name>/ directory is decided by
// the torrent's FORM, not its file count — and the pre-stage check joins these
// paths against dataDir to decide whether a download succeeded. Get it wrong
// and a complete download is reported as missing.
//
// The regression: this gated on `len(files) > 1`, which agrees with the form
// only when a multi-file torrent has more than one file. A multi-file torrent
// with exactly ONE file is still wrapped, so the check looked at the dataDir
// root and failed a download that was sitting on disk — the screenshots step
// had just opened the very file the next step called missing:
//
//	POST-MORTEM: dirContents=[…, "[Arid] Princess Mononoke […]"(4096B)]   ← a dir
//	screenshots: video=".../dl-request-22606/[Arid] Princess Mononoke […]/[Arid] Princess Mononoke [0CDD16E6].mkv"
//	Prepare: pre-stage file check failed: torrent declared 1 file(s), 1 missing
func TestExpectedPath(t *testing.T) {
	const mononoke = "[Arid] Princess Mononoke [Dual-Audio][BD 1920x1036 x264 FLAC]"
	const acca = "ACCA 13-Territory Inspection Dept. (2017)"

	cases := []struct {
		name          string
		displayPath   string
		torrentName   string
		multiFileForm bool
		want          string
	}{
		{
			// The bug: one file, multi-file form, therefore wrapped.
			name:          "multi-file form with a single file is still wrapped",
			displayPath:   "[Arid] Princess Mononoke [0CDD16E6].mkv",
			torrentName:   mononoke,
			multiFileForm: true,
			want:          mononoke + "/[Arid] Princess Mononoke [0CDD16E6].mkv",
		},
		{
			// True single-file form: anacrolix writes to dataDir/<name>, no wrapper.
			// Prefixing here would break the case that always worked.
			name:          "single-file form is not wrapped",
			displayPath:   "[Arid] Princess Mononoke [0CDD16E6].mkv",
			torrentName:   "[Arid] Princess Mononoke [0CDD16E6].mkv",
			multiFileForm: false,
			want:          "[Arid] Princess Mononoke [0CDD16E6].mkv",
		},
		{
			name:          "multi-file form with many files is wrapped",
			displayPath:   "E01.mkv",
			torrentName:   acca,
			multiFileForm: true,
			want:          acca + "/E01.mkv",
		},
		{
			// Some library versions already include the wrapper — must not double it.
			name:          "already-wrapped display path is left alone",
			displayPath:   acca + "/E01.mkv",
			torrentName:   acca,
			multiFileForm: true,
			want:          acca + "/E01.mkv",
		},
		{
			name:          "nested inner paths keep their structure",
			displayPath:   "Season 1/E01.mkv",
			torrentName:   acca,
			multiFileForm: true,
			want:          acca + "/Season 1/E01.mkv",
		},
		{
			name:          "empty torrent name cannot form a prefix",
			displayPath:   "E01.mkv",
			torrentName:   "",
			multiFileForm: true,
			want:          "E01.mkv",
		},
		{
			// A file whose name merely starts with the torrent name is NOT
			// already-wrapped — the separator is what makes it a prefix.
			name:          "name-lookalike is not mistaken for a wrapper",
			displayPath:   acca + " [extras].mkv",
			torrentName:   acca,
			multiFileForm: true,
			want:          acca + "/" + acca + " [extras].mkv",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expectedPath(tc.displayPath, tc.torrentName, tc.multiFileForm)
			if got != tc.want {
				t.Errorf("expectedPath(%q, %q, multiFileForm=%v)\n got  %q\n want %q",
					tc.displayPath, tc.torrentName, tc.multiFileForm, got, tc.want)
			}
		})
	}
}

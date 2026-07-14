package services

import "testing"

// subjectSafe must strip anything that could inject a spurious [i/j] or (n/m)
// marker (or break the quotes) into the canonical subject — otherwise a release
// title with brackets/parens would corrupt how a crawler parses the post.
func TestSubjectSafe(t *testing.T) {
	cases := map[string]string{
		`My Release (2024)`:              "My Release 2024",
		`Show [S01E02] "1080p"`:          "Show S01E02 1080p",
		"line1\r\nline2\ttab":            "line1 line2 tab",
		`  spaced   out  `:               "spaced out",
		`clean.release.name-GRP`:         "clean.release.name-GRP",
		``:                               "release", // never emit an empty base
		`(((`:                            "release", // collapses to empty -> placeholder
	}
	for in, want := range cases {
		if got := subjectSafe(in); got != want {
			t.Errorf("subjectSafe(%q) = %q, want %q", in, got, want)
		}
		// The result must contain none of the marker/quote characters.
		for _, ch := range []string{"[", "]", "(", ")", `"`} {
			if got := subjectSafe(in); containsRune(got, ch) {
				t.Errorf("subjectSafe(%q) = %q still contains %q", in, got, ch)
			}
		}
	}
}

func containsRune(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

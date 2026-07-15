package services

import (
	"os/exec"
	"strings"
	"testing"
)

// smokePAR2 must not reject a binary that actually works — a false negative
// silently downgrades every release to the slower, CJK-mangling par2create.
// Skips where the binary isn't installed (dev boxes); runs in the container.
func TestSmokePAR2AcceptsWorkingBinary(t *testing.T) {
	for _, bin := range []string{"parpar", "par2create"} {
		t.Run(bin, func(t *testing.T) {
			if _, err := exec.LookPath(bin); err != nil {
				t.Skipf("%s not installed here", bin)
			}
			if err := smokePAR2(bin, ""); err != nil {
				t.Errorf("smokePAR2(%q) = %v, want nil — the probe rejects a binary that is present and expected to work", bin, err)
			}
		})
	}
}

// The ladder must end in a method that needs no SIMD at all, or a CPU whose
// vector kernels are all broken (prod's Xeon Gold 6140 is one) has nothing to
// fall through to and loses parpar entirely.
func TestPAR2MethodLadder(t *testing.T) {
	if len(par2MethodLadder) == 0 {
		t.Fatal("ladder is empty")
	}
	if par2MethodLadder[0] != "" {
		t.Errorf("ladder[0] = %q, want \"\" — parpar's own auto-select should be tried first, it tunes more than we can express", par2MethodLadder[0])
	}
	if last := par2MethodLadder[len(par2MethodLadder)-1]; last != "lookup" {
		t.Errorf("ladder ends with %q, want \"lookup\" — the scalar kernel is the only one guaranteed to run on any CPU", last)
	}
	seen := map[string]bool{}
	for _, m := range par2MethodLadder {
		if seen[m] {
			t.Errorf("duplicate ladder entry %q — probes cost a real par2 run each", methodLabel(m))
		}
		seen[m] = true
	}
}

// A forced method must be honoured exactly: an operator overriding the probe
// does not want us quietly trying six other kernels behind their back.
func TestForcedPAR2MethodSkipsLadder(t *testing.T) {
	if _, err := exec.LookPath("parpar"); err != nil {
		t.Skip("parpar not installed here")
	}
	SetPAR2Method("definitely-not-a-real-method")
	defer SetPAR2Method("")

	_, err := resolveParparMethod()
	if err == nil {
		t.Fatal("resolveParparMethod() = nil error for a bogus forced method, want failure")
	}
	// One failure reported, not the whole ladder.
	if strings.Contains(err.Error(), "lookup") {
		t.Errorf("forced method fell through to the ladder: %v", err)
	}
}

// fitBlockSize guards the ceiling that shipped the 33GB "Nausicaa" remux to
// Usenet with no recovery at all: callers hand in the 700KB Usenet article
// size as the PAR2 slice size, which caps a release at 32768*700KB ~= 21.9 GiB
// before the tool exits non-zero.
func TestFitBlockSize(t *testing.T) {
	const article = 700 * 1024 // services.ChunkSize, what every caller passes

	cases := []struct {
		name  string
		total int64
		want  int
		grow  bool
	}{
		{"small release fits untouched", 431 << 20, article, false},
		{"1.2GB fits untouched", 1252 << 20, article, false},
		// 32768 * 716800 = 23488102400 exactly: the last size that still fits.
		{"exactly at the ceiling", 23488102400, article, false},
		{"one slice past the ceiling", 23488102400 + article, article, true},
		// The real failure: 33437.6 MiB / 716800 = 48914 slices > 32768.
		{"33GB Nausicaa remux", 33437 << 20, article, true},
		{"100GB release", 100 << 30, article, true},
		{"zero total is left alone", 0, article, false},
		{"zero want is left alone", 1 << 40, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fitBlockSize(tc.total, tc.want)

			if !tc.grow {
				if got != tc.want {
					t.Fatalf("fitBlockSize(%d, %d) = %d, want it unchanged at %d",
						tc.total, tc.want, got, tc.want)
				}
				return
			}

			if got <= tc.want {
				t.Fatalf("fitBlockSize(%d, %d) = %d, expected it to grow past %d",
					tc.total, tc.want, got, tc.want)
			}
			// The whole point: the resulting slice count must clear the limit.
			if slices := tc.total / int64(got); slices > maxPAR2Slices {
				t.Errorf("fitBlockSize(%d, %d) = %d still yields %d slices, over the %d limit",
					tc.total, tc.want, got, slices, maxPAR2Slices)
			}
			// PAR2 requires the slice size to be a multiple of 4.
			if got%4 != 0 {
				t.Errorf("fitBlockSize(%d, %d) = %d, not a multiple of 4",
					tc.total, tc.want, got)
			}
		})
	}
}

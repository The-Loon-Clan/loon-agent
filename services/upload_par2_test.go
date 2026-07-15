package services

import (
	"os/exec"
	"runtime"
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

// parpar's method names are architecture-specific, and we publish linux/amd64
// AND linux/arm64 from this one tree. Offering an arch a kernel that cannot
// exist there wastes a real par2 run per entry and lands on the scalar kernel,
// skipping the vector unit that arch actually has.
func TestPAR2MethodLadderPerArch(t *testing.T) {
	// Substrings that only ever appear in one arch's parpar method names,
	// per `parpar --help-full`.
	x86 := []string{"sse", "avx", "vbmi", "affine", "gfni"}
	arm := []string{"neon", "sve"}
	riscv := []string{"rvv"}

	cases := []struct {
		goarch  string
		allowed []string // substrings legal here
		banned  []string // substrings that must not appear
	}{
		{"amd64", x86, append(append([]string{}, arm...), riscv...)},
		{"386", x86, append(append([]string{}, arm...), riscv...)},
		{"arm64", arm, append(append([]string{}, x86...), riscv...)},
		{"arm", arm, append(append([]string{}, x86...), riscv...)},
		{"riscv64", riscv, append(append([]string{}, x86...), arm...)},
		// An arch we've taught nothing about must still be usable, not guessed at.
		{"s390x", nil, append(append(append([]string{}, x86...), arm...), riscv...)},
	}

	for _, tc := range cases {
		t.Run(tc.goarch, func(t *testing.T) {
			ladder := par2MethodLadderFor(tc.goarch)
			if len(ladder) == 0 {
				t.Fatal("empty ladder — this arch would have no par2 at all")
			}
			if ladder[0] != "" {
				t.Errorf("ladder[0] = %q, want \"\" — parpar's own auto-select tunes more than we can express and should lead", ladder[0])
			}
			if last := ladder[len(ladder)-1]; last != "lookup" {
				t.Errorf("ladder ends with %q, want \"lookup\" — the scalar kernel is the only floor guaranteed on any CPU", last)
			}
			seen := map[string]bool{}
			for _, m := range ladder {
				if seen[m] {
					t.Errorf("duplicate entry %q — every probe costs a real par2 run", methodLabel(m))
				}
				seen[m] = true
				for _, bad := range tc.banned {
					if strings.Contains(m, bad) {
						t.Errorf("%s ladder offers %q, which contains %q — that kernel does not exist on this arch", tc.goarch, m, bad)
					}
				}
			}
			// s390x legitimately has no vector entries; the rest should have some,
			// or the ladder is pointless there.
			if tc.allowed != nil {
				vector := false
				for _, m := range ladder {
					for _, ok := range tc.allowed {
						if strings.Contains(m, ok) {
							vector = true
						}
					}
				}
				if !vector {
					t.Errorf("%s ladder has no vector kernel — auto-select failing would drop straight to scalar", tc.goarch)
				}
			}
		})
	}
}

// The live ladder must be one of the per-arch lists, not something else.
func TestPAR2MethodLadderUsesGOARCH(t *testing.T) {
	got := par2MethodLadder()
	want := par2MethodLadderFor(runtime.GOARCH)
	if len(got) != len(want) {
		t.Fatalf("par2MethodLadder() has %d entries, par2MethodLadderFor(%q) has %d", len(got), runtime.GOARCH, len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %q, want %q", i, got[i], want[i])
		}
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

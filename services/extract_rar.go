package services

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// rarExtractBinary is the resolved CLI used to unpack RAR sets.
// Detected once at startup. Prefers `unrar` (RAR-specific, cleanest
// output) and falls back to `7z` (already shipped in the Alpine
// image — handles RAR3 + RAR5). When neither is on PATH the
// extractor is a no-op with a warning log; .rar files are then
// uploaded as-is and the user notices nothing has changed about
// the historical behaviour.
var rarExtractBinary = detectRARBinary()

func detectRARBinary() string {
	if path, err := exec.LookPath("unrar"); err == nil {
		log.Printf("RAR: using unrar (%s)", path)
		return "unrar"
	}
	if path, err := exec.LookPath("7z"); err == nil {
		log.Printf("RAR: using 7z (%s)", path)
		return "7z"
	}
	log.Println("RAR: WARNING — no unrar or 7z binary found in PATH; .rar files will be uploaded as-is")
	return ""
}

// rarPartRE matches `<base>.part<N>.rar` (new-style multi-volume).
// rarPlainRE matches `<base>.rar` (single-volume or old-style first).
// The two are kept separate so the caller can read N from the
// part-rar shape and skip everything except N == 1 — extracting a
// part2+ archive would just re-run the same job and clutter the
// dir with duplicate files.
var (
	rarPartRE  = regexp.MustCompile(`(?i)^(.+?)\.part(\d+)\.rar$`)
	rarPlainRE = regexp.MustCompile(`(?i)^(.+?)\.rar$`)
)

// rarFirstVolumeBase returns (base, true) when name is the first
// volume of a RAR set (or a single-volume archive). Returns
// ("", false) for non-RAR files and for part2+ volumes.
func rarFirstVolumeBase(name string) (string, bool) {
	if m := rarPartRE.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[2])
		return m[1], n == 1
	}
	if m := rarPlainRE.FindStringSubmatch(name); m != nil {
		return m[1], true
	}
	return "", false
}

// volumeMatcherFor returns a regexp that matches every member of
// the RAR set whose base is `base`:
//
//	foo.rar               (old-style first / single)
//	foo.r00 ... foo.r999  (old-style continuations)
//	foo.s00 ... foo.s999  (some packers also emit .sNN)
//	foo.part1.rar ...     (new-style multi-volume)
//
// PAR2 recovery files use a separate matcher (par2MatcherFor)
// because the .vol<N>+<M>.par2 shape isn't a RAR volume even
// though it ships alongside the archive.
func volumeMatcherFor(base string) *regexp.Regexp {
	esc := regexp.QuoteMeta(base)
	return regexp.MustCompile(`(?i)^` + esc + `(?:\.part\d+\.rar|\.r\d{2,3}|\.s\d{2,3}|\.rar)$`)
}

// par2MatcherFor matches the recovery files that ship alongside
// the RAR set we just extracted. They become orphaned (their target
// is gone) so we sweep them too.
func par2MatcherFor(base string) *regexp.Regexp {
	esc := regexp.QuoteMeta(base)
	return regexp.MustCompile(`(?i)^` + esc + `(?:\.vol\d+\+\d+)?\.par2$`)
}

// ExtractRARArchives walks dir for RAR archives, extracts each set
// in place, and removes the source .rar volumes + any orphaned
// .par2 recovery files. Returns the number of archives extracted.
//
// Safe on a dir with no RARs (returns 0,nil) and on a host with
// no extractor binary (returns 0,nil + a startup warning).
//
// Per-archive failures are logged and surfaced via the returned
// error but don't abort the walk — partial success is better than
// rolling back what already extracted cleanly. The caller can
// inspect (count > 0 && err != nil) to detect that case.
func ExtractRARArchives(ctx context.Context, dir string, logFn func(string)) (int, error) {
	if rarExtractBinary == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read dir: %w", err)
	}

	type rarSet struct {
		first string // filename of the first volume (relative to dir)
		base  string // common base name (no .part1, no .rar)
	}
	var sets []rarSet
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base, ok := rarFirstVolumeBase(e.Name())
		if !ok {
			continue
		}
		sets = append(sets, rarSet{first: e.Name(), base: base})
	}
	if len(sets) == 0 {
		return 0, nil
	}

	var lastErr error
	extracted := 0
	for _, s := range sets {
		archivePath := filepath.Join(dir, s.first)
		if logFn != nil {
			logFn(fmt.Sprintf("Extracting %s ...", s.first))
		}
		if err := extractOneRAR(ctx, archivePath, dir); err != nil {
			log.Printf("RAR: extract %s failed: %v", archivePath, err)
			lastErr = err
			continue
		}
		removeRARSet(dir, s.base, entries)
		extracted++
	}
	return extracted, lastErr
}

// extractOneRAR shells to the resolved binary with flags that
// (a) refuse to prompt on overwrite (any user-interactive prompt
// would hang the agent), (b) extract with the full archived path
// layout so multi-folder RARs reconstruct correctly, and (c) write
// every file relative to outDir.
func extractOneRAR(ctx context.Context, archive, outDir string) error {
	var args []string
	switch rarExtractBinary {
	case "unrar":
		// x   = extract with full paths
		// -o+ = overwrite existing files
		// -y  = answer yes to every prompt
		// --  = end of options; archive path follows
		args = []string{"x", "-o+", "-y", "--", archive}
	case "7z":
		// x   = extract with full paths
		// -y  = answer yes to every prompt
		// -aoa = overwrite all existing files without prompt
		args = []string{"x", "-y", "-aoa", archive}
	default:
		return fmt.Errorf("no rar extractor available")
	}
	cmd := exec.CommandContext(ctx, rarExtractBinary, args...)
	cmd.Dir = outDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", rarExtractBinary, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// removeRARSet deletes every volume of the named set + any
// orphaned .par2 recovery files. Errors are intentionally swallowed
// — a stray un-removable file isn't worth failing the whole upload
// over; the worst case is that the upload carries extra .rar +
// extracted files together. The caller has already counted the
// archive as extracted.
func removeRARSet(dir, base string, entries []os.DirEntry) {
	volRE := volumeMatcherFor(base)
	parRE := par2MatcherFor(base)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if volRE.MatchString(n) || parRE.MatchString(n) {
			if err := os.Remove(filepath.Join(dir, n)); err != nil && !os.IsNotExist(err) {
				log.Printf("rar: remove %s: %v", n, err)
			}
		}
	}
}

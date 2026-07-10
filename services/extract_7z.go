package services

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// sevenZipBinary is the resolved CLI used to unpack .7z archives.
// Detected once at startup. 7z has no native Go stdlib decoder, so we
// shell to the same p7zip/7zip family the RAR fallback already uses
// (always present in the Alpine image). Tries 7z → 7za → 7zr. When
// none is on PATH the extractor is a no-op with a warning; .7z files
// are then uploaded as-is and behaviour is unchanged.
//
// Unlike ZIP, 7z gets no store-mode exception: 7z is effectively
// always compressed (LZMA/LZMA2) and isn't a streaming-friendly
// container, so we always unpack it — same as the RAR stage.
var sevenZipBinary = detectSevenZipBinary()

func detectSevenZipBinary() string {
	for _, name := range []string{"7z", "7za", "7zr"} {
		if path, err := exec.LookPath(name); err == nil {
			log.Printf("7z: using %s (%s)", name, path)
			return name
		}
	}
	log.Println("7z: WARNING — no 7z/7za/7zr binary found in PATH; .7z files will be uploaded as-is")
	return ""
}

var (
	// foo.7z.001 / .002 … — split (multi-volume) 7z. Part .001 is the
	// first volume; pointing 7z at it unpacks the whole set.
	sevenZSplitRE = regexp.MustCompile(`(?i)^(.+)\.7z\.(\d{3})$`)
	// foo.7z — single-volume archive.
	sevenZPlainRE = regexp.MustCompile(`(?i)^(.+)\.7z$`)
)

// Extract7zArchives walks dir for .7z archives (single + split),
// extracts each set in place, and removes the source .7z volumes +
// any orphaned .par2 recovery files. Returns the number extracted.
//
// Safe on a dir with no .7z files (0,nil) and on a host with no
// binary (0,nil + a startup warning). Per-archive failures are
// logged and surfaced via the returned error but don't abort the
// walk — partial success is preserved, and on failure the source
// .7z is left untouched and uploaded as-is (mirrors the RAR/ZIP
// stages' contract).
func Extract7zArchives(ctx context.Context, dir string, logFn func(string)) (int, error) {
	if sevenZipBinary == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read dir: %w", err)
	}

	type sevenZSet struct {
		invoke string // file 7z is pointed at (.7z or the .001 part)
		stem   string // base name (no .7z) for the volume + par2 sweep
	}
	var sets []sevenZSet
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if m := sevenZSplitRE.FindStringSubmatch(n); m != nil {
			if m[2] == "001" {
				sets = append(sets, sevenZSet{invoke: n, stem: m[1]})
			}
			continue // skip split parts 002+
		}
		if m := sevenZPlainRE.FindStringSubmatch(n); m != nil {
			sets = append(sets, sevenZSet{invoke: n, stem: m[1]})
		}
	}
	if len(sets) == 0 {
		return 0, nil
	}

	var lastErr error
	extracted := 0
	for _, s := range sets {
		if ctx.Err() != nil {
			return extracted, ctx.Err()
		}
		if logFn != nil {
			logFn(fmt.Sprintf("Extracting %s ...", s.invoke))
		}
		if err := extractOne7z(ctx, filepath.Join(dir, s.invoke), dir); err != nil {
			log.Printf("7z: extract %s failed: %v", s.invoke, err)
			lastErr = err
			continue
		}
		remove7zSet(dir, s.stem, entries)
		extracted++
	}
	return extracted, lastErr
}

// extractOne7z shells to the resolved binary with flags that refuse
// to prompt (any prompt would hang the agent) and overwrite cleanly.
//
//	x    = extract with full archived paths
//	-y   = answer yes to every prompt
//	-aoa = overwrite all existing files without asking
func extractOne7z(ctx context.Context, archive, outDir string) error {
	cmd := exec.CommandContext(ctx, sevenZipBinary, "x", "-y", "-aoa", archive)
	cmd.Dir = outDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", sevenZipBinary, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// remove7zSet deletes the archive's .7z file(s) — the single .7z or
// every split .7z.NNN — plus orphaned .par2 recovery files (reusing
// the RAR stage's par2 matcher). Errors are swallowed: a stray
// un-removable file isn't worth failing the upload over.
func remove7zSet(dir, stem string, entries []os.DirEntry) {
	esc := regexp.QuoteMeta(stem)
	volRE := regexp.MustCompile(`(?i)^` + esc + `\.7z(?:\.\d{3})?$`)
	parRE := par2MatcherFor(stem)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if volRE.MatchString(n) || parRE.MatchString(n) {
			if err := os.Remove(filepath.Join(dir, n)); err != nil && !os.IsNotExist(err) {
				log.Printf("7z: remove %s: %v", n, err)
			}
		}
	}
}

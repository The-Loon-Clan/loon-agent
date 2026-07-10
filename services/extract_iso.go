package services

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
)

// ISO extraction reuses the 7z binary detected in extract_7z.go:
// p7zip reads ISO9660 + UDF disc images natively, so a data ISO or a
// BD/DVD disc image (BDMV / VIDEO_TS) is unpacked into its real file
// tree before PAR2 + upload — the NZB then carries the streamable
// media rather than a disc-image wrapper. No new dependency: when no
// 7z/7za/7zr binary is present the stage is a silent no-op and .iso
// files upload as-is.
//
// Single-volume .iso only. Split disc images are uncommon and, when
// they appear, arrive inside the rar/7z volumes the earlier stages
// already join — by the time this stage runs they're a plain .iso.
var isoRE = regexp.MustCompile(`(?i)^(.+)\.iso$`)

// ExtractISOArchives walks dir for .iso disc images, extracts each in
// place via the shared 7z binary, and removes the source .iso + any
// orphaned .par2 recovery files. Returns the number extracted.
//
// Same contract as the RAR/7z stages: per-archive failures are logged
// and surfaced via the returned error but don't abort the walk, and a
// failed extract leaves the .iso untouched so it uploads as-is.
func ExtractISOArchives(ctx context.Context, dir string, logFn func(string)) (int, error) {
	if sevenZipBinary == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read dir: %w", err)
	}

	type isoSet struct {
		name string // the .iso file
		stem string // base name (no .iso) for the par2 sweep
	}
	var sets []isoSet
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if m := isoRE.FindStringSubmatch(e.Name()); m != nil {
			sets = append(sets, isoSet{name: e.Name(), stem: m[1]})
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
			logFn(fmt.Sprintf("Extracting %s ...", s.name))
		}
		if err := extractOne7z(ctx, filepath.Join(dir, s.name), dir); err != nil {
			log.Printf("iso: extract %s failed: %v", s.name, err)
			lastErr = err
			continue
		}
		removeISOSet(dir, s.name, s.stem, entries)
		extracted++
	}
	return extracted, lastErr
}

// removeISOSet deletes the extracted .iso plus orphaned .par2 recovery
// files (reusing the RAR stage's par2 matcher). Errors are swallowed:
// a stray un-removable file isn't worth failing the upload over.
func removeISOSet(dir, name, stem string, entries []os.DirEntry) {
	if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
		log.Printf("iso: remove %s: %v", name, err)
	}
	parRE := par2MatcherFor(stem)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if parRE.MatchString(e.Name()) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
				log.Printf("iso: remove %s: %v", e.Name(), err)
			}
		}
	}
}

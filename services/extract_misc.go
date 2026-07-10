package services

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
)

// Single-pass legacy/odd archive formats the bundled 7z binary can
// unpack with one `7z x` invocation. None of these is common for anime
// media — lzh/lha shows up only in very old Japanese scene rips, and
// cab/arj/cpio essentially never wrap media — but they're cheap to
// cover via the binary that's already required, so a release that does
// use one unpacks instead of uploading as an opaque wrapper.
//
// Tarballs are NOT here: they're owned by ExtractTarArchives (which
// decodes the gzip/bzip2 layer in pure Go and only leans on 7z for
// xz/zstd). Lone single-stream compressors (.gz/.xz/.bz2/.Z without a
// .tar) are deliberately omitted — they wrap one file, virtually never
// media, and a bare .gz matcher would collide with the tar stage.
var miscFormats = []struct {
	re    *regexp.Regexp
	label string
}{
	{regexp.MustCompile(`(?i)^(.+)\.(?:lzh|lha)$`), "lzh"},
	{regexp.MustCompile(`(?i)^(.+)\.cab$`), "cab"},
	{regexp.MustCompile(`(?i)^(.+)\.arj$`), "arj"},
	{regexp.MustCompile(`(?i)^(.+)\.cpio$`), "cpio"},
}

// ExtractMiscArchives walks dir for the formats in miscFormats,
// extracts each via the shared 7z binary, and removes the source + any
// orphaned .par2 recovery files. Returns the number extracted.
//
// No-op when no 7z binary is present (these have no stdlib decoder).
// Same partial-success contract as the other stages.
func ExtractMiscArchives(ctx context.Context, dir string, logFn func(string)) (int, error) {
	if sevenZipBinary == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read dir: %w", err)
	}

	type miscJob struct{ name, stem string }
	var jobs []miscJob
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		for _, mf := range miscFormats {
			if m := mf.re.FindStringSubmatch(e.Name()); m != nil {
				jobs = append(jobs, miscJob{name: e.Name(), stem: m[1]})
				break
			}
		}
	}
	if len(jobs) == 0 {
		return 0, nil
	}

	var lastErr error
	extracted := 0
	for _, j := range jobs {
		if ctx.Err() != nil {
			return extracted, ctx.Err()
		}
		if logFn != nil {
			logFn(fmt.Sprintf("Extracting %s ...", j.name))
		}
		if err := extractOne7z(ctx, filepath.Join(dir, j.name), dir); err != nil {
			log.Printf("misc: extract %s failed: %v", j.name, err)
			lastErr = err
			continue
		}
		removeArchiveAndPar2(dir, j.name, j.stem, entries)
		extracted++
	}
	return extracted, lastErr
}

// removeArchiveAndPar2 deletes a single-file archive plus any orphaned
// .par2 recovery files for its stem (reusing the RAR stage's par2
// matcher). Shared by the tar + misc stages. Errors are swallowed: a
// stray un-removable file isn't worth failing the upload over.
func removeArchiveAndPar2(dir, name, stem string, entries []os.DirEntry) {
	if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
		log.Printf("misc: remove %s: %v", name, err)
	}
	parRE := par2MatcherFor(stem)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if parRE.MatchString(e.Name()) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
				log.Printf("misc: remove %s: %v", e.Name(), err)
			}
		}
	}
}

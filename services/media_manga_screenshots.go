package services

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// mangaArchiveExts lists the file extensions we know how to open as manga
// archives. Both CBZ and EPUB are ZIP files under the hood, so one code
// path serves both. CBR (RAR) and PDF are deliberately out of scope for
// this wave — they need external binaries (unrar, pdftoppm) and CGO libs.
var mangaArchiveExts = map[string]bool{
	".cbz":  true,
	".epub": true,
}

// mangaImageExts is the page-image filter applied to archive entries. EPUB
// archives also contain HTML/CSS/OPF/NCX files, so we can't just take the
// first N entries — we filter by extension. WebP is intentionally omitted:
// the site's screenshot decoder only registers JPEG and PNG, so a webp
// page would upload successfully but then fail ingest. Mainstream manga
// archives almost exclusively ship JPEG or PNG anyway.
var mangaImageExts = map[string]bool{
	".jpg":  true,
	".jpeg": true,
	".png":  true,
}

// FindMangaArchive walks dir looking for the first CBZ/EPUB. Returns "" if
// no matching archive is found. Largest-file tie-break isn't needed — manga
// torrents almost always contain exactly one archive per volume.
func FindMangaArchive(dir string) string {
	var found string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if mangaArchiveExts[ext] {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// GenerateMangaScreenshots extracts `count` preview images from a CBZ or
// EPUB archive to outputDir. The first extracted slot is always the cover
// (the alphabetically-first image, which is the archive-packer convention
// for the front cover); the remaining count-1 slots are evenly-spaced
// interior pages sampled from the middle 90% to skip title pages and
// back-matter. The extracted files keep their native image format — the
// site's screenshot ingest decodes PNG/JPEG transparently. Returned paths
// are in display order, cover first, so the release-page grid reads
// cover-left-to-right like the book does.
func GenerateMangaScreenshots(ctx context.Context, archivePath, outputDir string, count int) ([]string, error) {
	if count <= 0 {
		count = 6
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}

	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer zr.Close()

	// Gather image entries, then sort by path so pages come out in
	// reading order. Most manga archives use zero-padded filenames
	// ("001.jpg", "002.jpg", ...), which sort correctly as strings; for
	// archives that don't, the screenshots are still useful as a
	// representative sample even if not strictly ordered.
	var pages []*zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f.Name))
		if !mangaImageExts[ext] {
			continue
		}
		pages = append(pages, f)
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("no image pages found in %s", filepath.Base(archivePath))
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].Name < pages[j].Name })

	// Build the pick list: cover (index 0) followed by count-1 evenly
	// spaced interior pages from the middle 90%. Dedupe so very small
	// archives don't emit the same page twice.
	total := len(pages)
	picks := make([]int, 0, count)
	seen := make(map[int]bool, count)
	add := func(idx int) {
		if idx < 0 || idx >= total || seen[idx] {
			return
		}
		seen[idx] = true
		picks = append(picks, idx)
	}
	add(0) // cover

	interior := count - 1
	if interior > 0 {
		start := total * 5 / 100
		end := total - (total * 5 / 100)
		if end-start < interior {
			start = 1 // leave room for the cover at index 0
			end = total
		}
		span := end - start
		if span <= 0 {
			span = total
			start = 0
		}
		for i := 0; i < interior; i++ {
			add(start + span*i/interior)
		}
	}

	var paths []string
	for outIdx, pageIdx := range picks {
		if ctx.Err() != nil {
			break
		}
		src := pages[pageIdx]
		ext := strings.ToLower(filepath.Ext(src.Name))
		outPath := filepath.Join(outputDir, fmt.Sprintf("screen_%02d%s", outIdx+1, ext))

		rc, err := src.Open()
		if err != nil {
			continue
		}
		out, err := os.Create(outPath)
		if err != nil {
			rc.Close()
			continue
		}
		// Cap per-page copy at 20 MB so a pathological archive can't
		// eat the disk — real manga pages are well under 5 MB.
		_, copyErr := io.Copy(out, io.LimitReader(rc, 20<<20))
		rc.Close()
		out.Close()
		if copyErr != nil {
			os.Remove(outPath)
			continue
		}
		if info, err := os.Stat(outPath); err == nil && info.Size() > 0 {
			paths = append(paths, outPath)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no pages extracted")
	}
	return paths, nil
}

package services

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// extractZIPMaxEntryBytes caps how much a single zip entry may write
// to disk — a zip-bomb guard. 50 GiB is far above any real media file
// yet bounds a maliciously-crafted entry that claims petabytes.
const extractZIPMaxEntryBytes = 50 << 30

// ExtractZIPArchives walks dir for .zip files and unpacks any that
// are NOT pure store-mode, then removes the source .zip. The
// store-mode distinction is the whole point:
//
//   - A store-mode zip (every entry stored with no compression) is
//     already byte-for-byte uncompressed and seekable, so a streaming
//     consumer (nzbdav / UsenetStreamer) can read the media inside it
//     directly. Extracting would just duplicate the bytes for no gain,
//     so we leave it in place.
//   - A compressed zip (any entry uses Deflate/etc.) locks the media
//     behind compression — not streamable. We unpack it so the upload
//     carries the real, seekable files instead of a compressed wrapper,
//     exactly like the RAR stage does.
//
// Uses Go's native archive/zip — no external binary required. Returns
// the number of archives extracted. Per-zip failures are logged and
// surfaced via the returned error but don't abort the walk
// (mirrors ExtractRARArchives' partial-success contract): on any
// failure the source .zip is left untouched and uploaded as-is.
func ExtractZIPArchives(ctx context.Context, dir string, logFn func(string)) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read dir: %w", err)
	}
	var lastErr error
	extracted := 0
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".zip") {
			continue
		}
		if ctx.Err() != nil {
			return extracted, ctx.Err()
		}
		zipPath := filepath.Join(dir, e.Name())

		compressed, err := zipHasCompressedEntry(zipPath)
		if err != nil {
			// Includes split/spanned zips that archive/zip can't open —
			// leave them as-is rather than failing the upload.
			log.Printf("ZIP: inspect %s failed: %v", zipPath, err)
			lastErr = err
			continue
		}
		if !compressed {
			// Pure store-mode → already streamable; don't touch it.
			continue
		}

		if logFn != nil {
			logFn(fmt.Sprintf("Extracting %s ...", e.Name()))
		}
		if err := extractOneZIP(ctx, zipPath, dir); err != nil {
			log.Printf("ZIP: extract %s failed: %v", zipPath, err)
			lastErr = err
			continue
		}
		if err := os.Remove(zipPath); err != nil && !os.IsNotExist(err) {
			log.Printf("zip: remove %s: %v", zipPath, err)
		}
		extracted++
	}
	return extracted, lastErr
}

// zipHasCompressedEntry reports whether the archive has at least one
// file entry stored with a compression method other than Store. A
// zip where every file entry is Store is "store mode".
func zipHasCompressedEntry(path string) (bool, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return false, err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if f.Method != zip.Store {
			return true, nil
		}
	}
	return false, nil
}

// extractOneZIP unpacks every entry into outDir, recreating the
// archived directory layout. Zip-slip safe: any entry whose cleaned
// destination escapes outDir aborts the extraction.
func extractOneZIP(ctx context.Context, archive, outDir string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()

	absOut, err := filepath.Abs(outDir)
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name := decodeZipName(f)
		target := filepath.Join(absOut, name)
		// Reject "../" traversal that would write outside outDir.
		if target != absOut && !strings.HasPrefix(target, absOut+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe zip entry path %q", name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeZipEntry(f, target); err != nil {
			return err
		}
	}
	return nil
}

// decodeZipName returns the entry's filename as a Go string, best-effort
// decoded out of legacy CJK encodings. The PKWARE spec says the General-
// Purpose-Bit-11 (EFS bit) signals UTF-8; in practice older tools
// (Japanese WinRAR, 7-Zip <9.30, Windows Explorer pre-2012) store raw
// CP932 / GBK bytes with the bit unset. Go's archive/zip surfaces those
// bytes via f.Name unchanged, so a Japanese release zip ends up with
// mojibake filenames in archive.Walk and downstream walkers — exactly
// the "release was empty before the sweep" failure shape the agent
// hit on CJK content.
//
// Heuristic:
//  1. If the EFS bit is set, the writer claimed UTF-8 — trust it.
//  2. If already valid UTF-8 as bytes, use as-is (covers the common case
//     where a UTF-8 writer forgot to set the bit).
//  3. Else try Shift-JIS (Japanese), then GBK (Simplified Chinese), and
//     return whichever yields valid UTF-8. Log which decoder won so the
//     operator can see when this fired.
//  4. Fall back to the raw bytes so the entry is still extracted (with
//     a possibly-mangled name) rather than dropped — the existing
//     zip-slip safety check still applies via filepath.Join + prefix.
func decodeZipName(f *zip.File) string {
	if !f.NonUTF8 {
		return f.Name
	}
	if utf8.ValidString(f.Name) {
		return f.Name
	}
	raw := []byte(f.Name)
	if out, err := japanese.ShiftJIS.NewDecoder().Bytes(raw); err == nil && utf8.Valid(out) {
		log.Printf("zip: decoded legacy filename via Shift-JIS: %q", string(out))
		return string(out)
	}
	if out, err := simplifiedchinese.GBK.NewDecoder().Bytes(raw); err == nil && utf8.Valid(out) {
		log.Printf("zip: decoded legacy filename via GBK: %q", string(out))
		return string(out)
	}
	log.Printf("zip: legacy filename %q is neither valid UTF-8 nor decodable as Shift-JIS/GBK; keeping raw bytes", f.Name)
	return f.Name
}

func writeZipEntry(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, io.LimitReader(rc, extractZIPMaxEntryBytes)); err != nil {
		return err
	}
	return nil
}

package services

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Tar-family extraction. Linux-origin releases occasionally ship media
// inside a (compressed) tarball rather than a RAR/ZIP, which would
// otherwise upload as an opaque wrapper. This stage unpacks them in
// place before PAR2 + upload.
//
// gzip / bzip2 / plain tar decode through the Go stdlib — no external
// binary, so .tar(.gz|.bz2) extract even on a host with no 7z. xz and
// zstd have no stdlib decoder, so those two pipe through the shared 7z
// binary's stdout (-so); if 7z is absent they degrade to "upload
// as-is" like the other binary-backed stages.
type tarDecomp int

const (
	tarPlain tarDecomp = iota
	tarGzip
	tarBzip2
	tarXz
	tarZstd
)

// Suffixes are matched longest-first conceptually, but none here is a
// suffix of another so iteration order is not load-bearing. All lower-
// case; names are folded before comparison.
var tarFormats = []struct {
	suffix string
	dec    tarDecomp
}{
	{".tar.gz", tarGzip}, {".tgz", tarGzip},
	{".tar.bz2", tarBzip2}, {".tbz2", tarBzip2}, {".tbz", tarBzip2},
	{".tar.xz", tarXz}, {".txz", tarXz},
	{".tar.zst", tarZstd}, {".tzst", tarZstd},
	{".tar", tarPlain},
}

// ExtractTarArchives walks dir for tarballs (plain + gzip/bzip2/xz/zstd
// wrapped), extracts each in place, and removes the source + any
// orphaned .par2 recovery files. Returns the number extracted.
//
// Same partial-success contract as the other stages: per-archive
// failures are logged and surfaced via the returned error but don't
// abort the walk, and a failed extract leaves the source untouched.
func ExtractTarArchives(ctx context.Context, dir string, logFn func(string)) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read dir: %w", err)
	}

	type tarJob struct {
		name string
		stem string
		dec  tarDecomp
	}
	var jobs []tarJob
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		lower := strings.ToLower(e.Name())
		for _, tf := range tarFormats {
			if strings.HasSuffix(lower, tf.suffix) {
				jobs = append(jobs, tarJob{
					name: e.Name(),
					stem: e.Name()[:len(e.Name())-len(tf.suffix)],
					dec:  tf.dec,
				})
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
		if err := extractOneTar(ctx, filepath.Join(dir, j.name), dir, j.dec); err != nil {
			log.Printf("tar: extract %s failed: %v", j.name, err)
			lastErr = err
			continue
		}
		removeArchiveAndPar2(dir, j.name, j.stem, entries)
		extracted++
	}
	return extracted, lastErr
}

// extractOneTar decodes the tarball's compression wrapper (if any) and
// streams its entries into outDir.
func extractOneTar(ctx context.Context, archive, outDir string, dec tarDecomp) error {
	if dec == tarXz || dec == tarZstd {
		return extractTarVia7z(ctx, archive, outDir)
	}

	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()

	var tr *tar.Reader
	switch dec {
	case tarGzip:
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		tr = tar.NewReader(gz)
	case tarBzip2:
		tr = tar.NewReader(bzip2.NewReader(f))
	default: // tarPlain
		tr = tar.NewReader(f)
	}
	return writeTarEntries(ctx, tr, outDir)
}

// extractTarVia7z decompresses an xz/zstd outer layer through the 7z
// binary's stdout and feeds the resulting tar stream straight into the
// native reader — no intermediate file, no second pass. Kept robust to
// either side failing first: on extract error we kill 7z, then always
// reap it; a non-zero 7z exit is reported only if extraction itself
// succeeded (so the underlying cause isn't masked).
func extractTarVia7z(ctx context.Context, archive, outDir string) error {
	if sevenZipBinary == "" {
		return fmt.Errorf("no 7z binary to decompress %s", filepath.Base(archive))
	}
	cmd := exec.CommandContext(ctx, sevenZipBinary, "x", "-so", archive)
	cmd.Env = toolEnv()
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	extractErr := writeTarEntries(ctx, tar.NewReader(stdout), outDir)
	if extractErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if extractErr != nil {
		return extractErr
	}
	if waitErr != nil {
		escalateToolCrash(sevenZipBinary, archive, errBuf.Bytes(), waitErr)
		return fmt.Errorf("%s -so: %w\n%s", sevenZipBinary, waitErr, strings.TrimSpace(errBuf.String()))
	}
	return nil
}

// writeTarEntries reconstructs the archived tree under outDir. Tar-slip
// safe (any entry resolving outside outDir aborts) and capped per file
// by the shared zip-bomb guard. Only regular files + directories are
// materialised; symlinks/hardlinks/devices are skipped — media
// tarballs don't need them and a symlink is another escape vector.
func writeTarEntries(ctx context.Context, tr *tar.Reader, outDir string) error {
	absOut, err := filepath.Abs(outDir)
	if err != nil {
		return err
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(absOut, hdr.Name)
		if target != absOut && !strings.HasPrefix(target, absOut+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe tar entry path %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeTarFile(tr, target); err != nil {
				return err
			}
		}
	}
}

func writeTarFile(tr *tar.Reader, target string) error {
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, io.LimitReader(tr, extractZIPMaxEntryBytes)); err != nil {
		return err
	}
	return nil
}

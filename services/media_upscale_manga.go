package services

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Manga / CBZ image-upscale pipeline. Much simpler shape than video:
// each archive's pages are independent images, no temporal coherence,
// no ffmpeg, no chunking. A whole chapter usually finishes in seconds
// on a 5090.
//
// Shape per CBZ:
//   1. Extract pages to a temp dir (CBZ == ZIP, but typically store-
//      mode since JPEG/PNG/WebP are already compressed).
//   2. Run the ncnn-vulkan binary in dir-mode (-i in_dir -o out_dir):
//      one process spawn, every page upscaled in a single GPU run —
//      avoids the per-page fork overhead that would dominate a manga
//      chapter's actual GPU time.
//   3. Repack the upscaled pages into a new CBZ alongside the source,
//      under dir/upscale/.

// imagePageExts is the set of file extensions runImageUpscale will
// hand to the ncnn-vulkan binary. Anything else inside a CBZ
// (info.txt, ComicInfo.xml, .DS_Store, thumbnails) is passed through
// unchanged so the output is a faithful repack.
var imagePageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true,
}

// runImageUpscale walks dir for CBZ archives and upscales each one in
// turn. Same partial-success + drop-originals contract as the video
// pipeline.
func runImageUpscale(ctx context.Context, dir string, m UpscaleModel) (*UpscaleResult, error) {
	cbzFiles, err := findCBZArchives(dir)
	if err != nil {
		return nil, fmt.Errorf("upscale: walk %s: %w", dir, err)
	}
	if len(cbzFiles) == 0 {
		return &UpscaleResult{Skipped: true, Reason: "no CBZ archives to upscale"}, nil
	}

	outDir := filepath.Join(dir, "upscale")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("upscale: mkdir %s: %w", outDir, err)
	}

	result := &UpscaleResult{}
	for i, src := range cbzFiles {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		base := filepath.Base(src)
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		dst := filepath.Join(outDir, stem+".upscale.cbz")
		if _, err := os.Stat(dst); err == nil {
			dst = filepath.Join(outDir, stem+"_"+strconv.Itoa(i)+".upscale.cbz")
		}
		log.Printf("upscale: %s → %s (model=%s scale=%dx)",
			src, filepath.Base(dst), m.Key, m.Scale)
		if err := upscaleOneCBZ(ctx, src, dst, m); err != nil {
			log.Printf("upscale: failed on %s: %v", src, err)
			continue
		}
		result.EmittedFiles = append(result.EmittedFiles, dst)
	}

	if len(result.EmittedFiles) == 0 {
		_ = os.RemoveAll(outDir)
		return nil, fmt.Errorf("upscale: every invocation failed")
	}

	for _, src := range cbzFiles {
		if err := os.Remove(src); err != nil {
			log.Printf("upscale: failed to remove original %s: %v", src, err)
		}
	}
	return result, nil
}

// upscaleOneCBZ extracts → ncnn-vulkan dir-mode → repack. Non-image
// entries (metadata, thumbnails) are forwarded verbatim into the
// output CBZ so the repack is byte-faithful for everything the model
// can't sensibly upscale.
func upscaleOneCBZ(ctx context.Context, src, dst string, m UpscaleModel) error {
	work, err := os.MkdirTemp(filepath.Dir(dst), ".upscale_cbz_")
	if err != nil {
		return fmt.Errorf("mkdir work: %w", err)
	}
	defer os.RemoveAll(work)

	pageIn := filepath.Join(work, "in")
	pageOut := filepath.Join(work, "out")
	if err := os.MkdirAll(pageIn, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(pageOut, 0o755); err != nil {
		return err
	}

	// 1. Extract — split image pages from passthrough metadata so the
	// ncnn binary only sees files it can process.
	passthrough, err := extractCBZSplit(src, pageIn)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	// 2. Upscale. ncnn-vulkan dir-mode does the whole chapter in one
	// GPU run; same arg shape as the video pipeline.
	if entries, _ := os.ReadDir(pageIn); len(entries) > 0 {
		upArgs := append([]string{
			"-i", pageIn,
			"-o", pageOut,
			"-s", strconv.Itoa(m.Scale),
			"-f", "png",
		}, m.Args...)
		upCmd := exec.CommandContext(ctx, m.Binary, upArgs...)
		upCmd.Env = toolEnv()
		if out, err := upCmd.CombinedOutput(); err != nil {
			escalateToolCrash(m.Binary, src, out, err)
			return fmt.Errorf("%s: %w\n%s", m.Binary, err, tailLines(string(out), 6))
		}
	}

	// 3. Repack — upscaled pages + verbatim passthrough metadata.
	return repackCBZ(dst, pageOut, passthrough)
}

// extractCBZSplit reads a CBZ and writes only the image pages into
// pageDir (flattened — ncnn-vulkan dir-mode needs a flat input). Non-
// image entries are returned as a slice of {archive-relative path,
// bytes} so repackCBZ can mux them back into the output untouched.
//
// Page filenames are normalised to PNG-friendly names but keep their
// archived order via a 5-digit prefix: ncnn-vulkan sorts its input
// directory lexically, so any zero-padded prefix preserves chapter
// page order even when the source CBZ used unpadded names.
type cbzPassthrough struct {
	name string
	data []byte
}

func extractCBZSplit(src, pageDir string) ([]cbzPassthrough, error) {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	// Pre-sort by archived name so the 5-digit prefix tracks the
	// original page order exactly.
	files := make([]*zip.File, 0, len(zr.File))
	for _, f := range zr.File {
		if !f.FileInfo().IsDir() {
			files = append(files, f)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	var passthrough []cbzPassthrough
	pageIdx := 0
	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f.Name))
		if !imagePageExts[ext] {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return nil, err
			}
			passthrough = append(passthrough, cbzPassthrough{name: f.Name, data: data})
			continue
		}
		pageIdx++
		outName := fmt.Sprintf("%05d%s", pageIdx, ext)
		if err := copyZipEntry(f, filepath.Join(pageDir, outName)); err != nil {
			return nil, err
		}
	}
	return passthrough, nil
}

func copyZipEntry(f *zip.File, target string) error {
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
	_, err = io.Copy(out, io.LimitReader(rc, extractZIPMaxEntryBytes))
	return err
}

// repackCBZ writes an output CBZ containing the upscaled pages from
// pageOut (sorted lex, which matches the 5-digit prefix order) plus the
// passthrough entries in their original archive-relative paths. Stored
// mode (no compression) because every payload is already compressed.
func repackCBZ(dst, pageOut string, passthrough []cbzPassthrough) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	defer zw.Close()

	// Pages first (sorted) so a reader paging the CBZ sees them in
	// order without needing a metadata index.
	entries, err := os.ReadDir(pageOut)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := addZipFile(zw, e.Name(), filepath.Join(pageOut, e.Name())); err != nil {
			return err
		}
	}
	for _, p := range passthrough {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: p.name, Method: zip.Store})
		if err != nil {
			return err
		}
		if _, err := w.Write(p.data); err != nil {
			return err
		}
	}
	return nil
}

func addZipFile(zw *zip.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

// findCBZArchives walks dir for .cbz / .cbr files. .cbr is rare for
// manga but handled the same way — the ZIP reader accepts anything
// matching the central directory format, and most "CBR" releases are
// actually CBZ-shaped despite the name. Real RAR-shaped CBRs will
// fail at extract time and log + skip.
func findCBZArchives(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		switch strings.ToLower(filepath.Ext(d.Name())) {
		case ".cbz", ".cbr":
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

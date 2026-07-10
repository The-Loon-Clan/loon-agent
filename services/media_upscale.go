package services

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// UpscaleResult mirrors RemuxResult: the list of emitted MKVs and a skip
// reason when there was no work to do for this dir.
type UpscaleResult struct {
	EmittedFiles []string
	Skipped      bool
	Reason       string
}

// upscaleChunkSeconds bounds how much video each pass extracts → upscales
// → re-encodes. 30s at 24fps is ~720 frames; on disk that peaks around
// 1–3 GiB of PNGs per chunk after the 2x model runs. Shrink if disk is
// tight, grow to amortise extract/encode overhead on a fast GPU.
//
// Whole-file extraction is not an option: a 2h movie at all-frames-as-PNG
// would need hundreds of GB; chunking is mandatory for the ncnn-vulkan
// toolchain, which has no streaming mode.
const upscaleChunkSeconds = 30

// upscaleExtractFilters is the ffmpeg pre-filter chain run during frame
// extraction. bwdif deinterlaces frames that the demuxer flags as
// interlaced (progressive frames pass through unchanged); hqdn3d strips
// DVD/VHS/low-bitrate noise so the upscaler doesn't lock the artefacts
// into the output. Good default for old anime; per-model tuning lives
// in a later phase.
const upscaleExtractFilters = "bwdif=mode=send_frame:parity=auto:deint=interlaced,hqdn3d"

// RunUpscale is the AI upscale entry point — parallel to RunRemux. It
// resolves the requested model and dispatches to the right pipeline:
//
//	"video" (default) — chunked ffmpeg extract → ncnn-vulkan → encode
//	"image"           — CBZ extract → ncnn-vulkan dir-mode → repack
//
// Both pipelines write to dir/upscale/ and drop the original sources
// once at least one output succeeded — same contract as runConvert.
func RunUpscale(ctx context.Context, dir, upscaleOption string) (*UpscaleResult, error) {
	model, ok := UpscaleModelByKey(upscaleOption)
	if !ok {
		return nil, fmt.Errorf("upscale: unknown option %q", upscaleOption)
	}
	if _, err := exec.LookPath(model.Binary); err != nil {
		return nil, fmt.Errorf("upscale: %s not on PATH (GPU image not installed?): %w", model.Binary, err)
	}
	if model.Type == "image" {
		return runImageUpscale(ctx, dir, model)
	}
	return runVideoUpscale(ctx, dir, model)
}

// runVideoUpscale walks dir for video files, splits each into chunks,
// extracts frames + upscales them, re-encodes, concatenates, and
// muxes the original audio/subtitles/chapters back into the upscaled
// video. Output MKVs land in dir/upscale/.
func runVideoUpscale(ctx context.Context, dir string, model UpscaleModel) (*UpscaleResult, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("upscale: ffmpeg not found in PATH: %w", err)
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return nil, fmt.Errorf("upscale: ffprobe not found in PATH: %w", err)
	}

	sources, err := findUpscalableVideoFiles(dir)
	if err != nil {
		return nil, fmt.Errorf("upscale: walk %s: %w", dir, err)
	}
	if len(sources) == 0 {
		return &UpscaleResult{Skipped: true, Reason: "no video files to upscale"}, nil
	}

	outDir := filepath.Join(dir, "upscale")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("upscale: mkdir %s: %w", outDir, err)
	}

	result := &UpscaleResult{}
	for i, src := range sources {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		base := filepath.Base(src)
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		dst := filepath.Join(outDir, stem+".upscale.mkv")
		if _, err := os.Stat(dst); err == nil {
			dst = filepath.Join(outDir, stem+"_"+strconv.Itoa(i)+".upscale.mkv")
		}
		log.Printf("upscale: %s → %s (model=%s scale=%dx)",
			src, filepath.Base(dst), model.Key, model.Scale)
		if err := upscaleOneFile(ctx, src, dst, model); err != nil {
			log.Printf("upscale: failed on %s: %v", src, err)
			continue
		}
		result.EmittedFiles = append(result.EmittedFiles, dst)
	}

	if len(result.EmittedFiles) == 0 {
		_ = os.RemoveAll(outDir)
		return nil, fmt.Errorf("upscale: every invocation failed")
	}

	// Drop originals — keeping a 4–8x-larger upscaled version alongside
	// the source defeats the point. Same as runConvert.
	for _, src := range sources {
		if err := os.Remove(src); err != nil {
			log.Printf("upscale: failed to remove original %s: %v", src, err)
		}
	}
	return result, nil
}

// upscaleOneFile drives the chunked pipeline for one input video:
// probe → loop(extract→upscale→encode) → concat → mux.
func upscaleOneFile(ctx context.Context, src, dst string, m UpscaleModel) error {
	dur, fps, err := probeVideo(ctx, src)
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	if dur <= 0 || fps <= 0 {
		return fmt.Errorf("probe: bad duration/fps (dur=%.2fs fps=%.3f)", dur, fps)
	}

	work, err := os.MkdirTemp(filepath.Dir(dst), ".upscale_work_")
	if err != nil {
		return fmt.Errorf("mkdir work: %w", err)
	}
	defer os.RemoveAll(work)

	var chunkMKVs []string
	chunkCount := int((dur + float64(upscaleChunkSeconds) - 1) / float64(upscaleChunkSeconds))
	for i := 0; i < chunkCount; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		startSec := i * upscaleChunkSeconds
		chunkOut := filepath.Join(work, fmt.Sprintf("chunk_%05d.mkv", i))
		if err := upscaleOneChunk(ctx, src, chunkOut, work, i, startSec, upscaleChunkSeconds, fps, m); err != nil {
			return fmt.Errorf("chunk %d/%d: %w", i+1, chunkCount, err)
		}
		chunkMKVs = append(chunkMKVs, chunkOut)
		log.Printf("upscale: %s chunk %d/%d done", filepath.Base(src), i+1, chunkCount)
	}
	if len(chunkMKVs) == 0 {
		return fmt.Errorf("no chunks produced")
	}

	concatList := filepath.Join(work, "concat.txt")
	if err := writeConcatList(concatList, chunkMKVs); err != nil {
		return fmt.Errorf("concat list: %w", err)
	}
	videoOnly := filepath.Join(work, "video_only.mkv")
	if err := runFFmpegConcat(ctx, concatList, videoOnly); err != nil {
		return fmt.Errorf("concat: %w", err)
	}

	if err := runFFmpegMux(ctx, videoOnly, src, dst); err != nil {
		return fmt.Errorf("mux: %w", err)
	}
	return nil
}

// upscaleOneChunk extracts chunkSec seconds of frames starting at
// startSec, upscales them via the ncnn-vulkan binary, then re-encodes
// to a video-only MKV at the source frame rate.
//
// `-ss` is placed AFTER `-i` for frame-accurate seek: slower than the
// fast (pre-input) form but chunk boundaries MUST align — a misseeked
// chunk would show a glitch at the concat seam.
func upscaleOneChunk(ctx context.Context, src, chunkOut, work string, idx, startSec, chunkSec int, fps float64, m UpscaleModel) error {
	framesIn := filepath.Join(work, fmt.Sprintf("in_%05d", idx))
	framesUp := filepath.Join(work, fmt.Sprintf("up_%05d", idx))
	if err := os.MkdirAll(framesIn, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(framesUp, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(framesIn)
	defer os.RemoveAll(framesUp)

	extractCmd := exec.CommandContext(ctx, "ffmpeg",
		"-y", "-hide_banner", "-loglevel", "warning",
		"-i", src,
		"-ss", strconv.Itoa(startSec),
		"-t", strconv.Itoa(chunkSec),
		"-vf", upscaleExtractFilters,
		filepath.Join(framesIn, "%08d.png"))
	extractCmd.Env = toolEnv()
	if out, err := extractCmd.CombinedOutput(); err != nil {
		escalateToolCrash("ffmpeg", src, out, err)
		return fmt.Errorf("extract: %w\n%s", err, tailLines(string(out), 4))
	}

	upArgs := append([]string{
		"-i", framesIn,
		"-o", framesUp,
		"-s", strconv.Itoa(m.Scale),
		"-f", "png",
	}, m.Args...)
	upCmd := exec.CommandContext(ctx, m.Binary, upArgs...)
	upCmd.Env = toolEnv()
	if out, err := upCmd.CombinedOutput(); err != nil {
		escalateToolCrash(m.Binary, src, out, err)
		return fmt.Errorf("%s: %w\n%s", m.Binary, err, tailLines(string(out), 6))
	}

	// 10-bit HEVC is the archival-quality default for anime and matches
	// the convert_h265 preset. yuv420p10le keeps colour banding in flat
	// regions (sky, gradients) tight — important on upscaled anime.
	encodeCmd := exec.CommandContext(ctx, "ffmpeg",
		"-y", "-hide_banner", "-loglevel", "warning",
		"-framerate", strconv.FormatFloat(fps, 'f', -1, 64),
		"-i", filepath.Join(framesUp, "%08d.png"),
		"-c:v", "libx265", "-crf", "23", "-preset", "medium",
		"-pix_fmt", "yuv420p10le",
		"-an",
		chunkOut)
	encodeCmd.Env = toolEnv()
	if out, err := encodeCmd.CombinedOutput(); err != nil {
		escalateToolCrash("ffmpeg", src, out, err)
		return fmt.Errorf("encode: %w\n%s", err, tailLines(string(out), 6))
	}
	return nil
}

// probeVideo returns (duration_seconds, frames_per_second) via ffprobe.
func probeVideo(ctx context.Context, src string) (float64, float64, error) {
	durCmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", src)
	durCmd.Env = toolEnv()
	durOut, err := durCmd.Output()
	if err != nil {
		escalateToolCrash("ffprobe", src, durOut, err)
		return 0, 0, err
	}
	dur, err := strconv.ParseFloat(strings.TrimSpace(string(durOut)), 64)
	if err != nil {
		return 0, 0, err
	}

	fpsCmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=r_frame_rate",
		"-of", "default=noprint_wrappers=1:nokey=1", src)
	fpsCmd.Env = toolEnv()
	fpsOut, err := fpsCmd.Output()
	if err != nil {
		escalateToolCrash("ffprobe", src, fpsOut, err)
		return 0, 0, err
	}
	fps, err := parseFraction(strings.TrimSpace(string(fpsOut)))
	if err != nil {
		return 0, 0, err
	}
	return dur, fps, nil
}

// parseFraction handles ffprobe's "24000/1001" rational frame-rate
// format and falls through to a plain float for sources that report
// "23.976" or similar.
func parseFraction(s string) (float64, error) {
	if idx := strings.IndexByte(s, '/'); idx >= 0 {
		num, err1 := strconv.ParseFloat(s[:idx], 64)
		den, err2 := strconv.ParseFloat(s[idx+1:], 64)
		if err1 == nil && err2 == nil && den != 0 {
			return num / den, nil
		}
	}
	return strconv.ParseFloat(s, 64)
}

// writeConcatList writes the ffmpeg concat demuxer format. Paths are
// single-quoted; embedded backslashes are doubled first so a Windows-
// style path or a literal `\'` sequence doesn't break the apostrophe
// escape that follows. Order matters — backslash MUST be escaped before
// the single-quote pass because the apostrophe-escape itself produces
// new backslash bytes.
func writeConcatList(path string, mkvs []string) error {
	var b strings.Builder
	for _, m := range mkvs {
		escaped := strings.ReplaceAll(m, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, "'", `'\''`)
		b.WriteString("file '")
		b.WriteString(escaped)
		b.WriteString("'\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// runFFmpegConcat stitches the per-chunk video MKVs into a single
// video-only MKV via the concat demuxer + stream copy (no re-encode).
// Each chunk shares the same encode settings so concat is seam-clean.
func runFFmpegConcat(ctx context.Context, listPath, dst string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y", "-hide_banner", "-loglevel", "warning",
		"-f", "concat", "-safe", "0",
		"-i", listPath,
		"-c", "copy",
		dst)
	cmd.Env = toolEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		escalateToolCrash("ffmpeg", dst, out, err)
		return fmt.Errorf("%w\n%s", err, tailLines(string(out), 4))
	}
	return nil
}

// runFFmpegMux pulls the upscaled video (stream 0) and copies the
// original audio / subtitles / chapters (stream 1) into the final
// container. `?` on map specs makes missing audio/sub streams
// non-fatal — silent / sub-less sources still produce a usable MKV.
func runFFmpegMux(ctx context.Context, videoOnly, originalForAudio, dst string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y", "-hide_banner", "-loglevel", "warning",
		"-i", videoOnly,
		"-i", originalForAudio,
		"-map", "0:v:0",
		"-map", "1:a?",
		"-map", "1:s?",
		"-map_chapters", "1",
		"-c", "copy",
		dst)
	cmd.Env = toolEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		escalateToolCrash("ffmpeg", dst, out, err)
		return fmt.Errorf("%w\n%s", err, tailLines(string(out), 6))
	}
	return nil
}

// findUpscalableVideoFiles walks dir for video files worth upscaling,
// skipping samples and tiny extras. Looser than findRemuxableFiles —
// upscale targets any video container, not just Bluray-shaped content.
func findUpscalableVideoFiles(dir string) ([]string, error) {
	exts := map[string]bool{
		".mkv": true, ".mp4": true, ".avi": true,
		".m2ts": true, ".ts": true, ".mov": true,
	}
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := strings.ToLower(d.Name())
		if !exts[filepath.Ext(name)] {
			return nil
		}
		if strings.Contains(name, "sample") || strings.HasPrefix(name, "extra") {
			return nil
		}
		info, infoErr := d.Info()
		// <50 MiB is almost certainly a sample / extra / promo, not the
		// feature — skip to avoid burning GPU hours on nothing useful.
		if infoErr != nil || info.Size() < 50<<20 {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out, err
}

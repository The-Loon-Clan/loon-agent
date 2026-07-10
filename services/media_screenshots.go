package services

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// tailLines returns the last n non-empty lines of s, joined by " | ".
// Used to surface the actionable tail of an external-tool stderr blob
// without dumping the full output (ffmpeg in particular emits 100+
// progress lines before the actual error). Returns the original
// string if it's shorter than n lines.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	out := make([]string, 0, n)
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			out = append([]string{line}, out...)
		}
	}
	return strings.Join(out, " | ")
}

// screenshotWorkers controls how many ffmpeg screenshot processes run in
// parallel. 3 is a good balance — input seeking is largely I/O-bound (disk
// read) so more workers overlap the seek latency without hammering the CPU.
const screenshotWorkers = 3

// GenerateScreenshots creates lossless PNG screenshots at evenly spaced
// intervals through the video. Thin wrapper over
// GenerateScreenshotsWatermarked with no watermark — keeps the existing
// online-path call sites untouched while the offline path opts in to
// watermark support via the newer signature.
func GenerateScreenshots(ctx context.Context, videoPath, outputDir string, duration float64, count int) ([]string, error) {
	return GenerateScreenshotsWatermarked(ctx, videoPath, outputDir, duration, count, "")
}

// GenerateScreenshotsWatermarked is the implementation. If watermark is
// non-empty, the text is burned into the bottom-right of every frame via
// ffmpeg's drawtext filter. We emit PNG (rather than JPEG) so the server
// can re-encode to lossless WebP without ever going through a lossy stage —
// users use these to judge release quality, and any JPEG step in the middle
// would defeat that.
func GenerateScreenshotsWatermarked(ctx context.Context, videoPath, outputDir string, duration float64, count int, watermark string) ([]string, error) {
	log.Printf("screenshots: GenerateScreenshotsWatermarked entry video=%q outDir=%q duration=%.1fs count=%d watermark=%q", videoPath, outputDir, duration, count, watermark)
	if count <= 0 {
		count = 6
	}
	if duration <= 0 {
		return nil, fmt.Errorf("invalid duration: %.1f", duration)
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}

	// Skip the first and last 5% to avoid intros/credits/black frames.
	start := duration * 0.05
	end := duration * 0.95
	span := end - start
	if span <= 0 {
		span = duration
		start = 0
	}

	// Capture screenshots in parallel — ffmpeg seeking is I/O-bound, so
	// overlapping seeks cuts wall-clock time roughly in half for 6 shots.
	type result struct {
		index int
		path  string
	}
	var (
		mu      sync.Mutex
		results []result
		wg      sync.WaitGroup
		sem     = make(chan struct{}, screenshotWorkers)
	)

	for i := 0; i < count; i++ {
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}        // acquire worker slot
			defer func() { <-sem }() // release

			if ctx.Err() != nil {
				return
			}

			t := start + span*float64(idx)/float64(count)
			timestamp := formatTimestamp(t)
			outPath := filepath.Join(outputDir, fmt.Sprintf("screen_%02d.png", idx+1))

			args := []string{
				"-ss", timestamp,
				"-i", videoPath,
				"-vframes", "1",
			}
			// Burn the watermark into the frame via drawtext. Positioned
			// 10px from the bottom-right corner with a translucent black
			// box so it stays legible on both bright and dark scenes.
			// Chars that are special in ffmpeg filter syntax (: \ ')
			// are escaped so operators can paste odd group names without
			// breaking the pipeline.
			if watermark != "" {
				escaped := escapeDrawtext(watermark)
				filter := fmt.Sprintf("drawtext=text='%s':fontcolor=white:fontsize=24:box=1:boxcolor=black@0.5:boxborderw=6:x=w-tw-12:y=h-th-12", escaped)
				args = append(args, "-vf", filter)
			}
			args = append(args, "-c:v", "png", "-y", outPath)
			cmd := exec.CommandContext(ctx, "ffmpeg", args...)
			cmd.Env = toolEnv()
			// Capture stderr so a failed frame produces an actionable
			// log line. Previously this was nil'd which silently
			// dropped the failure — operator had no way to tell whether
			// the source was unseekable, the timestamp was past the
			// end of the file, or ffmpeg itself was missing. cmd.Run
			// returns non-nil exit on most failures, so logging the
			// last few stderr lines covers both classes.
			var stderr strings.Builder
			cmd.Stdout = nil
			cmd.Stderr = &stderr

			if err := cmd.Run(); err != nil {
				escalateToolCrash("ffmpeg", videoPath, []byte(stderr.String()), err)
				log.Printf("screenshot %d/%d failed at %s: %v\n  ffmpeg stderr: %s",
					idx+1, count, timestamp, err, tailLines(stderr.String(), 3))
				return
			}

			if info, err := os.Stat(outPath); err == nil && info.Size() > 0 {
				mu.Lock()
				results = append(results, result{index: idx, path: outPath})
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if len(results) == 0 {
		return nil, fmt.Errorf("no screenshots generated")
	}

	// Return paths in index order so screenshots appear in timeline order.
	sort.Slice(results, func(a, b int) bool { return results[a].index < results[b].index })
	paths := make([]string, 0, len(results))
	for _, r := range results {
		paths = append(paths, r.path)
	}
	return paths, nil
}

// escapeDrawtext escapes the characters that are special inside an ffmpeg
// drawtext text= value: backslash is the escape char, colon separates
// filter options, and single-quote closes the filter argument we built
// around the text. No general-purpose shell escape needed — the filter
// string is passed as a single argv argument, not run through a shell.
func escapeDrawtext(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`:`, `\:`,
		`'`, `\'`,
	)
	return r.Replace(s)
}

// formatTimestamp converts seconds to HH:MM:SS.mmm format for ffmpeg.
func formatTimestamp(seconds float64) string {
	h := int(seconds) / 3600
	m := (int(seconds) % 3600) / 60
	s := seconds - float64(h*3600+m*60)
	return fmt.Sprintf("%02d:%02d:%06.3f", h, m, s)
}

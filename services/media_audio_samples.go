package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
)

// DefaultSampleSeconds is the fallback clip duration when a group doesn't
// set one. Five seconds is short enough to preview the mix without any
// plausible copyright claim (many sites already gate samples at this
// length) and long enough to hear timbre + tempo.
const DefaultSampleSeconds = 5

// audioSampleWorkers mirrors screenshotWorkers: ffmpeg's seek + decode is
// mostly I/O-bound so overlapping a handful of processes cuts total wall
// time, more than that just contends on disk.
const audioSampleWorkers = 3

// GenerateAudioSamples cuts count short clips out of audioPath at evenly
// spaced positions and writes them as MP3 into outputDir. MP3 chosen over
// Opus for universal playback — every browser, every phone, every legacy
// device can play it without fuss; saving a few KB per sample isn't worth
// making a user install a codec.
//
// Layout and timestamp logic mirror GenerateScreenshots so the two
// primitives feel alike in the pipeline: skip the first/last 5% of the
// file (intros, silent tails), span the middle 90%, N evenly-spaced
// start points, clip of `seconds` duration at each.
//
// Non-fatal errors at the per-sample level return fewer files rather
// than a hard failure — matches the screenshot primitive's behaviour.
func GenerateAudioSamples(ctx context.Context, audioPath, outputDir string, duration float64, count, seconds int) ([]string, error) {
	if count <= 0 {
		count = 6
	}
	if seconds <= 0 {
		seconds = DefaultSampleSeconds
	}
	if duration <= 0 {
		return nil, fmt.Errorf("invalid duration: %.1f", duration)
	}
	// A clip can't start after the track ends. Shrink count rather than
	// returning an error so a very short track (e.g. a 30s interlude)
	// still produces whatever samples fit.
	minSpan := float64(seconds) * float64(count)
	if minSpan > duration {
		count = int(duration / float64(seconds))
		if count <= 0 {
			return nil, fmt.Errorf("track too short (%.1fs) for even one %ds sample", duration, seconds)
		}
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}

	start := duration * 0.05
	end := duration * 0.95
	span := end - start
	if span <= 0 {
		span = duration
		start = 0
	}

	type result struct {
		index int
		path  string
	}
	var (
		mu      sync.Mutex
		results []result
		wg      sync.WaitGroup
		sem     = make(chan struct{}, audioSampleWorkers)
	)

	for i := 0; i < count; i++ {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			t := start + span*float64(idx)/float64(count)
			outPath := filepath.Join(outputDir, fmt.Sprintf("sample_%02d.mp3", idx+1))
			// -ss before -i is the "fast seek" form — ffmpeg jumps near
			// the target frame without decoding everything before it,
			// which matters a lot on long tracks. Accuracy is good
			// enough for preview-length samples.
			// -vn drops any embedded album art from the input so we
			// don't end up re-encoding a 1MB cover onto every clip.
			cmd := exec.CommandContext(ctx, "ffmpeg",
				"-ss", formatTimestamp(t),
				"-i", audioPath,
				"-t", fmt.Sprintf("%d", seconds),
				"-vn",
				"-c:a", "libmp3lame",
				"-q:a", "4", // VBR ~165kbps — transparent for sampling
				"-y",
				outPath,
			)
			cmd.Stdout = nil
			cmd.Stderr = nil
			if err := cmd.Run(); err != nil {
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
		return nil, fmt.Errorf("no audio samples generated")
	}
	sort.Slice(results, func(a, b int) bool { return results[a].index < results[b].index })
	paths := make([]string, 0, len(results))
	for _, r := range results {
		paths = append(paths, r.path)
	}
	return paths, nil
}

// AudioExtensions are the file types we treat as "audio release" input
// when the group type is "music". Kept small and canonical — no legacy
// codecs, no container ambiguity. If a user drops an .ape or .mka in
// they'll need to transcode first or use a custom group type.
var AudioExtensions = map[string]bool{
	".mp3":  true,
	".flac": true,
	".m4a":  true,
	".aac":  true,
	".ogg":  true,
	".opus": true,
	".wav":  true,
}

// FindAudioFiles is the audio counterpart to FindVideoFiles: return every
// matching file beneath dir, biggest first. The largest audio file in a
// typical music release is a lossless master or a concatenated DJ mix;
// either way it's the right one to sample.
func FindAudioFiles(dir string) []string {
	type af struct {
		path string
		size int64
	}
	var files []af
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return err
		}
		ext := filepath.Ext(info.Name())
		if AudioExtensions[normaliseExt(ext)] {
			files = append(files, af{path, info.Size()})
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].size > files[j].size })
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.path
	}
	return out
}

// normaliseExt lowercases the extension; kept as a package helper so the
// walk closure doesn't need the extra strings import footprint.
func normaliseExt(ext string) string {
	b := []byte(ext)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

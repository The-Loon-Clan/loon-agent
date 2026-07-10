package services

import (
	"fmt"
	"image"
	_ "image/jpeg" // register decoder for JPEG screenshots
	_ "image/png"  // register decoder for PNG screenshots
	"log"
	"os"
	"sort"
)

// ExtractDominantPalette walks the screenshot files the agent just
// generated, bucket-histograms their pixels into a coarse RGB grid,
// and returns the top N hex colors (default 8) by frequency.
//
// We deliberately operate on the existing screenshot images rather
// than re-decoding the source video — screenshots are already
// representative samples of the release, and decoding 8 PNGs is two
// orders of magnitude cheaper than another ffmpeg pass.
//
// Returns an empty slice (not an error) when no readable screenshots
// exist — the site treats absence as "feature didn't run" and hides
// the color strip, which is the same forward-compat policy as the
// other Phase D-onward features.
func ExtractDominantPalette(screenshotPaths []string, topN int) []string {
	if topN <= 0 {
		topN = 8
	}
	if len(screenshotPaths) == 0 {
		return nil
	}

	// 4 bits per channel → 4096 buckets. Coarse enough that near-
	// identical pixels collapse together, fine enough that distinct
	// scene colors stay separate. uint32 keyed map fits comfortably
	// in cache for a release-scale histogram.
	hist := make(map[uint32]uint64, 4096)
	for _, path := range screenshotPaths {
		if err := accumulatePalette(path, hist); err != nil {
			log.Printf("palette: %s: %v (continuing)", path, err)
			continue
		}
	}
	if len(hist) == 0 {
		return nil
	}

	type bucket struct {
		key   uint32
		count uint64
	}
	buckets := make([]bucket, 0, len(hist))
	for k, v := range hist {
		buckets = append(buckets, bucket{k, v})
	}
	sort.Slice(buckets, func(i, j int) bool {
		return buckets[i].count > buckets[j].count
	})

	if topN > len(buckets) {
		topN = len(buckets)
	}
	out := make([]string, 0, topN)
	for i := 0; i < topN; i++ {
		// Reconstruct an 8-bit color from the 4-bit bucket key. We
		// recenter into the middle of each bucket (×16+8) so the
		// rendered swatch matches the average pixel, not the
		// bucket's bottom-left corner.
		k := buckets[i].key
		r := byte(((k >> 8) & 0xF) << 4) | 0x08
		g := byte(((k >> 4) & 0xF) << 4) | 0x08
		b := byte(((k >> 0) & 0xF) << 4) | 0x08
		out = append(out, fmt.Sprintf("#%02x%02x%02x", r, g, b))
	}
	return out
}

// accumulatePalette decodes one screenshot and folds its pixels into
// hist. We sample every Nth pixel rather than every pixel — at 1080p
// that's 2M samples per image, which is plenty to find dominant
// colors and ~64× faster than reading every pixel.
func accumulatePalette(path string, hist map[uint32]uint64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return err
	}
	bounds := img.Bounds()
	// Sample stride: tune so a 4K screenshot samples ~250K pixels
	// (≈3% of the frame). Smaller images sample at stride 1.
	w := bounds.Dx()
	h := bounds.Dy()
	stride := 1
	if w*h > 500_000 {
		stride = 4
	}
	if w*h > 2_000_000 {
		stride = 8
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y += stride {
		for x := bounds.Min.X; x < bounds.Max.X; x += stride {
			r, g, b, a := img.At(x, y).RGBA()
			if a < 0x8000 {
				continue
			}
			// Drop near-black pixels (subtitle backgrounds, letterbox
			// bars) — they swamp the histogram on anime releases that
			// happen to have one screenshot mid-cut.
			if r < 0x1000 && g < 0x1000 && b < 0x1000 {
				continue
			}
			key := uint32(r>>12)<<8 | uint32(g>>12)<<4 | uint32(b>>12)
			hist[key]++
		}
	}
	return nil
}

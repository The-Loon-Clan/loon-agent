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

// convertPreset is one (codec, CRF, ffmpeg-preset) tuple per supported
// target. Single source of truth — both the dispatcher gate (site) and
// the runtime branch (agent) read the same enum names.
type convertPreset struct {
	Codec  string // ffmpeg -c:v value
	CRF    string // -crf value (AV1's libsvtav1 uses a different scale)
	Preset string // -preset value
}

// convertPresets maps the request-level remux_option to the actual
// ffmpeg arguments. Defaults are chosen for archival quality, not for
// streaming bandwidth — we expect a long pass, not a real-time one.
var convertPresets = map[string]convertPreset{
	"convert_h264": {Codec: "libx264", CRF: "21", Preset: "slow"},
	"convert_h265": {Codec: "libx265", CRF: "23", Preset: "medium"},
	"convert_av1":  {Codec: "libsvtav1", CRF: "30", Preset: "6"},
}

// runConvert re-encodes the remuxable video files under dir into MKVs
// alongside the originals (in dir/remux/, same layout as RunRemux's
// stream-copy mode). Audio + subtitle + chapter tracks pass through
// unchanged.
//
// We rely on ffmpeg being on PATH — every agent image with the remux
// pipeline already carries it (used for ffprobe + screenshot
// generation). The dispatcher gates dispatch on convert_video=TRUE
// per agent (migration 221).
func runConvert(ctx context.Context, dir, remuxOption string) (*RemuxResult, error) {
	preset, ok := convertPresets[remuxOption]
	if !ok {
		return nil, fmt.Errorf("convert: unknown option %q", remuxOption)
	}

	isBluray, why := looksLikeBluray(dir)
	if !isBluray {
		return &RemuxResult{Skipped: true, Reason: "no Bluray-shaped content found"}, nil
	}
	log.Printf("convert: detected (%s) in %s, target=%s", why, dir, remuxOption)

	if strings.Contains(strings.ToLower(why), "iso file present") {
		return &RemuxResult{Skipped: true, Reason: "ISO content needs MakeMKV layer (not in this agent image)"}, nil
	}

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("convert: ffmpeg not found in PATH: %w", err)
	}

	sources, err := findRemuxableFiles(dir)
	if err != nil {
		return nil, fmt.Errorf("convert: walk %s: %w", dir, err)
	}
	if len(sources) == 0 {
		return &RemuxResult{Skipped: true, Reason: "no source files to encode"}, nil
	}

	outDir := filepath.Join(dir, "remux")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("convert: mkdir %s: %w", outDir, err)
	}

	result := &RemuxResult{}
	for i, src := range sources {
		base := filepath.Base(src)
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		dst := filepath.Join(outDir, stem+".mkv")
		if _, err := os.Stat(dst); err == nil {
			dst = filepath.Join(outDir, stem+"_"+strconv.Itoa(i)+".mkv")
		}
		log.Printf("convert: %s → %s (codec=%s crf=%s preset=%s)",
			src, filepath.Base(dst), preset.Codec, preset.CRF, preset.Preset)
		cmd := exec.CommandContext(ctx, "ffmpeg",
			"-y", "-hide_banner", "-loglevel", "warning",
			"-i", src,
			"-c:v", preset.Codec,
			"-crf", preset.CRF,
			"-preset", preset.Preset,
			// Audio + subtitles + chapters: passthrough. No reason
			// to re-encode lossless audio just because the video
			// codec is changing.
			"-c:a", "copy",
			"-c:s", "copy",
			"-map_chapters", "0",
			"-map", "0",
			dst)
		cmd.Env = toolEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			escalateToolCrash("ffmpeg", src, out, err)
			log.Printf("convert: ffmpeg failed on %s: %v\n%s", src, err, tailLines(string(out), 6))
			continue
		}
		result.EmittedMKVs = append(result.EmittedMKVs, dst)
	}

	if len(result.EmittedMKVs) == 0 {
		_ = os.RemoveAll(outDir)
		return nil, fmt.Errorf("convert: every ffmpeg invocation failed")
	}

	// Convert always drops the original sources — keeping the BDMV
	// alongside a re-encoded MKV doubles the disk footprint with no
	// gain, since the user explicitly asked for a transcoded output.
	for _, src := range sources {
		if err := os.Remove(src); err != nil {
			log.Printf("convert: failed to remove original %s: %v", src, err)
		}
	}
	for _, sub := range []string{"BDMV", "CERTIFICATE", "AACS"} {
		_ = os.RemoveAll(filepath.Join(dir, sub))
	}
	return result, nil
}

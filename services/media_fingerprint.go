package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
)

// AudioFingerprint is the per-video Chromaprint fingerprint payload
// the agent ships in the Complete request. The site stores one row
// per release+filename in audio_fingerprints (migration 218); a
// future similarity-search feature compares them via Hamming
// distance.
type AudioFingerprint struct {
	SourceFilename   string  // basename of the video the fingerprint came from
	DurationSeconds  float64 // fpcalc-reported analysis duration
	AlgorithmVersion int     // chromaprint version (currently always 2)
	Fingerprint      string  // base32 fingerprint string
}

// fpcalcOutput is the shape returned by `fpcalc -json INPUT`.
// fpcalc's JSON output is stable across versions; we ignore any
// future-added fields automatically.
type fpcalcOutput struct {
	Duration    float64 `json:"duration"`
	Fingerprint string  `json:"fingerprint"`
}

// FingerprintAudio runs fpcalc on every video under srcDir and
// returns one AudioFingerprint per file. Best-effort: a probe
// failure logs and skips rather than aborting the upload — partial
// fingerprint coverage is strictly better than none.
//
// Silently skips when fpcalc isn't on PATH (older agent images
// without chromaprint-tools). The site treats empty fingerprint
// lists as "feature not run for this release", not as "track has
// no audio".
func FingerprintAudio(ctx context.Context, srcDir string) ([]AudioFingerprint, error) {
	if _, err := exec.LookPath("fpcalc"); err != nil {
		log.Printf("audio fingerprint: fpcalc not found in PATH — skipping (install chromaprint-tools)")
		return nil, nil
	}
	videos := FindVideoFiles(srcDir)
	if len(videos) == 0 {
		return nil, nil
	}
	var out []AudioFingerprint
	for _, v := range videos {
		fp, err := fpcalcOne(ctx, v)
		if err != nil {
			log.Printf("audio fingerprint: %s: %v (continuing)", filepath.Base(v), err)
			continue
		}
		if fp == nil {
			continue
		}
		out = append(out, *fp)
	}
	return out, nil
}

// fpcalcOne runs fpcalc on a single video file. -json gives a
// machine-parseable response; -length 0 means "analyse the whole
// track" (the default is 120s — fine for matching, but anime fans
// will want full-track matching for OP/ED detection later).
func fpcalcOne(ctx context.Context, video string) (*AudioFingerprint, error) {
	cmd := exec.CommandContext(ctx, "fpcalc", "-json", "-length", "0", video)
	cmd.Env = toolEnv()
	bytes, err := cmd.Output()
	if err != nil {
		escalateToolCrash("fpcalc", video, bytes, err)
		return nil, err
	}
	var data fpcalcOutput
	if err := json.Unmarshal(bytes, &data); err != nil {
		return nil, fmt.Errorf("fpcalc json parse: %w", err)
	}
	if strings.TrimSpace(data.Fingerprint) == "" {
		return nil, nil
	}
	return &AudioFingerprint{
		SourceFilename:   filepath.Base(video),
		DurationSeconds:  data.Duration,
		AlgorithmVersion: 2,
		Fingerprint:      data.Fingerprint,
	}, nil
}

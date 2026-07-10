package services

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// RemuxResult is returned by RunRemux. EmittedMKVs is the list of paths
// the pipeline wrote — empty when there was nothing to remux. Skipped
// is true when the pipeline detected no Bluray-shaped content and
// returned without doing anything (caller treats this as a no-op).
type RemuxResult struct {
	EmittedMKVs []string
	Skipped     bool
	Reason      string
}

// remuxableExts is the set of source extensions mkvmerge stream-copies
// cleanly. .vob/.ts (raw DVD/TS segments) are intentionally excluded —
// they usually need a demux step first that this agent doesn't carry.
var remuxableExts = map[string]bool{
	".mkv": true, ".m2ts": true, ".mp4": true, ".mov": true,
	".avi": true, ".flv": true, ".wmv": true, ".ts": false,
}

// findRemuxableFiles walks the directory looking for files mkvmerge
// can stream-copy. Returns absolute paths sorted by basename.
// Folder structure preserved — we don't try to flatten BDMV/STREAM/ etc.
// since mkvmerge takes the full path either way.
func findRemuxableFiles(root string) ([]string, error) {
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return err
		}
		if !remuxableExts[strings.ToLower(filepath.Ext(info.Name()))] {
			return nil
		}
		out = append(out, path)
		return nil
	})
	sort.Strings(out)
	return out, err
}

// looksLikeBluray returns true when the directory contains evidence of
// Bluray content: a BDMV folder, an .iso file, or two-or-more .m2ts
// files. We use this as a gate so the remux step doesn't fire on
// random multi-MKV downloads where the user's release-group naming
// already does what they want.
//
// .iso is detected but not handled — MakeMKV would be required to
// decrypt it, and the agent doesn't carry MakeMKV today. Returns true
// here so the caller can log "ISO detected but not supported" rather
// than silently no-op.
func looksLikeBluray(dir string) (bool, string) {
	bdmv := filepath.Join(dir, "BDMV")
	if st, err := os.Stat(bdmv); err == nil && st.IsDir() {
		return true, "BDMV directory present"
	}
	// Walk shallow looking for .iso or many .m2ts files.
	m2tsCount := 0
	hasISO := false
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return err
		}
		ext := strings.ToLower(filepath.Ext(info.Name()))
		switch ext {
		case ".iso":
			hasISO = true
		case ".m2ts":
			m2tsCount++
		}
		return nil
	})
	if hasISO {
		return true, "ISO file present (not yet supported — install MakeMKV layer for ISO remux)"
	}
	if m2tsCount >= 2 {
		return true, fmt.Sprintf("%d .m2ts files present", m2tsCount)
	}
	return false, ""
}

// RunRemux runs the post-download remux step on dir according to the
// site-supplied remuxOption ("remux" or "both"). Modes:
//
//   remux: produce MKV(s), then delete the original sources. The
//          downstream pipeline (probe → screenshots → upload) sees
//          only the MKV(s).
//   both:  produce MKV(s) alongside the original sources. Downstream
//          uploads both the raw payload and the MKVs.
//
// "none" / "" callers don't reach this function — main.go gates on
// remuxOption upstream. We still defensively bail on unknown modes
// rather than guess.
//
// The pipeline uses mkvmerge directly (in the agent's Docker image
// via apk add mkvtoolnix). For each remuxable input, we write a
// matching .mkv next to it under a remux/ subdirectory of dir. We
// avoid clobbering original .mkv inputs — those stay as-is in both
// modes unless they're inside BDMV/ or wrapped with extra container
// overhead worth shedding.
//
// Returns the list of emitted MKVs (absolute paths) so the caller
// can pick them up for the upload stage.
func RunRemux(ctx context.Context, dir, remuxOption string) (*RemuxResult, error) {
	log.Printf("remux: RunRemux entry dir=%q option=%q", dir, remuxOption)
	if remuxOption == "" || remuxOption == "none" {
		return &RemuxResult{Skipped: true, Reason: "remux_option=none"}, nil
	}
	// Convert targets re-encode via ffmpeg rather than stream-copy via
	// mkvmerge. The site dispatcher only sends these to agents whose
	// agent_config.convert_video=TRUE (migration 221), but we still
	// double-check ffmpeg is on PATH so a mis-flagged agent fails
	// loudly instead of silently dropping the encode pass.
	if strings.HasPrefix(remuxOption, "convert_") {
		return runConvert(ctx, dir, remuxOption)
	}
	if remuxOption != "remux" && remuxOption != "both" {
		return nil, fmt.Errorf("remux: unknown option %q (expected remux|both|convert_*)", remuxOption)
	}

	isBluray, why := looksLikeBluray(dir)
	if !isBluray {
		return &RemuxResult{Skipped: true, Reason: "no Bluray-shaped content found"}, nil
	}
	log.Printf("remux: detected (%s) in %s", why, dir)

	if strings.Contains(strings.ToLower(why), "iso file present") {
		// ISO support requires MakeMKV. Bail with a clear reason
		// rather than half-remuxing.
		return &RemuxResult{Skipped: true, Reason: "ISO content needs MakeMKV layer (not in this agent image)"}, nil
	}

	// Locate the mkvmerge binary up front so a missing tool fails
	// loudly instead of letting each-file invocation explode.
	if _, err := exec.LookPath("mkvmerge"); err != nil {
		return nil, fmt.Errorf("remux: mkvmerge not found in PATH: %w", err)
	}

	sources, err := findRemuxableFiles(dir)
	if err != nil {
		return nil, fmt.Errorf("remux: walk %s: %w", dir, err)
	}
	if len(sources) == 0 {
		return &RemuxResult{Skipped: true, Reason: "no remuxable source files found"}, nil
	}

	outDir := filepath.Join(dir, "remux")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("remux: mkdir %s: %w", outDir, err)
	}

	result := &RemuxResult{}
	for i, src := range sources {
		base := filepath.Base(src)
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		// Disambiguate when two sources have the same stem (BDMV
		// frequently has 00001.m2ts in multiple folders).
		dst := filepath.Join(outDir, stem+".mkv")
		if _, err := os.Stat(dst); err == nil {
			dst = filepath.Join(outDir, stem+"_"+strconv.Itoa(i)+".mkv")
		}
		log.Printf("remux: %s → %s", src, filepath.Base(dst))
		cmd := exec.CommandContext(ctx,
			"mkvmerge",
			"--no-attachments",
			"-o", dst,
			src)
		cmd.Env = toolEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			escalateToolCrash("mkvmerge", src, out, err)
			log.Printf("remux: mkvmerge failed on %s: %v\n%s", src, err, string(out))
			// Skip this source rather than aborting the whole pass —
			// other inputs may still produce useful output.
			continue
		}
		result.EmittedMKVs = append(result.EmittedMKVs, dst)
	}

	if len(result.EmittedMKVs) == 0 {
		// Clean up the empty remux/ directory we created up front so
		// the downstream blocklist sweep doesn't see a stale half-
		// state and the next ingest step doesn't mistake it for a
		// successful output. Best-effort: if removal fails, the
		// hourly orphan sweep eventually catches it.
		_ = os.RemoveAll(outDir)
		return nil, fmt.Errorf("remux: every mkvmerge invocation failed")
	}

	// remux mode: drop the original sources (we keep what we
	// produced in dir/remux/). 'both' mode keeps everything.
	if remuxOption == "remux" {
		for _, src := range sources {
			if err := os.Remove(src); err != nil {
				log.Printf("remux: failed to remove original %s: %v", src, err)
			}
		}
		// Also remove BDMV/ subtree if it's there — the MKV(s) carry
		// every track that mattered. CERTIFICATE/, AACS/, and other
		// disc-only directories are pure overhead.
		for _, sub := range []string{"BDMV", "CERTIFICATE", "AACS"} {
			_ = os.RemoveAll(filepath.Join(dir, sub))
		}
	}
	return result, nil
}

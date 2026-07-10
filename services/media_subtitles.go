package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SubtitleTrack is one extracted subtitle, ready for upload to the
// site. ExtractSubtitles produces a list of these per source video.
// File holds the on-disk path inside the agent's temp dir; the
// caller streams the bytes to /api/agent/subtitle and removes the
// file afterward.
type SubtitleTrack struct {
	SourcePath   string // the video file the track came from
	TrackIndex   int    // mkvmerge -J's tracks[N].id
	Language     string // ISO 639-2; "und" when source didn't carry one
	TrackName    string // human-readable name from the MKV header
	Codec        string // normalised: srt|ass|ssa|webvtt|pgs|vobsub|other
	Forced       bool
	DefaultTrack bool
	File         string // extracted file on disk
	SizeBytes    int64
}

// mkvmergeJSON is the shape returned by `mkvmerge -J INPUT`. We only
// care about the tracks array — the file metadata + container info
// are ignored.
type mkvmergeJSON struct {
	Tracks []struct {
		ID         int    `json:"id"`
		Type       string `json:"type"`     // "video" | "audio" | "subtitles"
		Codec      string `json:"codec"`    // human label e.g. "SubRip/SRT"
		CodecID    string `json:"codec_id"` // technical id e.g. "S_TEXT/UTF8"
		Properties struct {
			Language     string `json:"language"`      // ISO 639-2
			LanguageIETF string `json:"language_ietf"` // BCP-47 (often empty)
			TrackName    string `json:"track_name"`
			Forced       bool   `json:"forced_track"`
			DefaultTrack bool   `json:"default_track"`
		} `json:"properties"`
	} `json:"tracks"`
}

// codecFromMKVID maps mkvmerge's codec_id to the normalised codec
// label the site expects. Unknown ids fall through to "other" so
// the agent never refuses to upload — the site still gets the
// bytes; the UI just renders a generic icon.
func codecFromMKVID(codecID string) string {
	switch strings.ToUpper(codecID) {
	case "S_TEXT/UTF8", "S_TEXT/ASCII":
		return "srt"
	case "S_TEXT/ASS":
		return "ass"
	case "S_TEXT/SSA":
		return "ssa"
	case "S_TEXT/WEBVTT":
		return "webvtt"
	case "S_HDMV/PGS":
		return "pgs"
	case "S_VOBSUB":
		return "vobsub"
	}
	return "other"
}

// extensionForCodec returns the file extension mkvextract should
// write for the given normalised codec. Mirrors the site-side
// DownloadSubtitle filename builder.
func extensionForCodec(codec string) string {
	switch codec {
	case "pgs":
		return "sup"
	case "vobsub":
		return "sub"
	case "other":
		return "bin"
	default:
		return codec
	}
}

// ExtractSubtitles walks a downloaded directory for video files,
// probes each via `mkvmerge -J`, and extracts every subtitle track
// it finds via `mkvextract tracks`. Output files go under outDir
// with names "<sha-prefix>-<trackid>.<ext>" so two videos with the
// same track-3 don't clobber each other.
//
// Best-effort: a probe failure or an unextractable track logs and
// skips rather than aborting the whole release — partial subtitle
// coverage is strictly better than none. Returns the slice of
// successful extractions, which the caller uploads one-by-one.
//
// Skips when mkvmerge/mkvextract aren't on PATH (image build without
// mkvtoolnix). Logs the absence once so operators know what to
// install if they want the feature.
// SubtitleToolStatus returns a non-empty short reason string when
// ExtractSubtitles will short-circuit because a required binary is
// missing — "mkvmerge missing" or "mkvextract missing". Empty string
// means both binaries are on PATH and extraction will at least
// attempt to run. Used by the agent's per-release pipeline-stage
// checklist so a missing-tool skip shows up with attribution rather
// than as an unattributed "empty" stage on the release page.
func SubtitleToolStatus() string {
	if _, err := exec.LookPath("mkvmerge"); err != nil {
		return "mkvmerge missing (install mkvtoolnix in agent image)"
	}
	if _, err := exec.LookPath("mkvextract"); err != nil {
		return "mkvextract missing (install mkvtoolnix in agent image)"
	}
	return ""
}

func ExtractSubtitles(ctx context.Context, srcDir, outDir string) ([]SubtitleTrack, error) {
	log.Printf("subtitles: ExtractSubtitles entry srcDir=%q outDir=%q", srcDir, outDir)
	if _, err := exec.LookPath("mkvmerge"); err != nil {
		log.Printf("subtitles: mkvmerge not found in PATH — skipping subtitle extraction (install mkvtoolnix)")
		return nil, nil
	}
	if _, err := exec.LookPath("mkvextract"); err != nil {
		log.Printf("subtitles: mkvextract not found in PATH — skipping subtitle extraction (install mkvtoolnix)")
		return nil, nil
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("subtitles: mkdir %s: %w", outDir, err)
	}

	videos := FindVideoFiles(srcDir)
	log.Printf("subtitles: found %d video file(s) under %q", len(videos), srcDir)
	if len(videos) == 0 {
		return nil, nil
	}

	var out []SubtitleTrack
	for _, v := range videos {
		tracks, err := probeSubtitleTracks(ctx, v)
		if err != nil {
			log.Printf("subtitles: probe %s: %v (continuing)", filepath.Base(v), err)
			continue
		}
		if len(tracks) == 0 {
			continue
		}
		// Use a short hash of the source filename as a per-video
		// prefix so two videos in the same release don't clobber
		// each other's "track 3" extracts.
		prefix := stableShortHash(filepath.Base(v))
		for _, t := range tracks {
			ext := extensionForCodec(t.Codec)
			dst := filepath.Join(outDir, fmt.Sprintf("%s-track%d.%s", prefix, t.TrackIndex, ext))
			// mkvextract syntax: mkvextract tracks INPUT N:OUT
			cmd := exec.CommandContext(ctx, "mkvextract", "tracks", v,
				fmt.Sprintf("%d:%s", t.TrackIndex, dst))
			cmd.Env = toolEnv()
			if outBytes, err := cmd.CombinedOutput(); err != nil {
				escalateToolCrash("mkvextract", v, outBytes, err)
				log.Printf("subtitles: mkvextract %s track %d failed: %v\n  %s",
					filepath.Base(v), t.TrackIndex, err, tailLines(string(outBytes), 2))
				continue
			}
			info, err := os.Stat(dst)
			if err != nil || info.Size() == 0 {
				log.Printf("subtitles: mkvextract %s track %d produced no output", filepath.Base(v), t.TrackIndex)
				_ = os.Remove(dst)
				continue
			}
			t.SourcePath = v
			t.File = dst
			t.SizeBytes = info.Size()
			out = append(out, t)
		}
	}
	return out, nil
}

// probeSubtitleTracks runs `mkvmerge -J FILE` and returns one
// SubtitleTrack per subtitle-type track. Pure read — never modifies
// the source.
func probeSubtitleTracks(ctx context.Context, video string) ([]SubtitleTrack, error) {
	cmd := exec.CommandContext(ctx, "mkvmerge", "-J", video)
	cmd.Env = toolEnv()
	outBytes, err := cmd.Output()
	if err != nil {
		escalateToolCrash("mkvmerge", video, outBytes, err)
		return nil, err
	}
	var data mkvmergeJSON
	if err := json.Unmarshal(outBytes, &data); err != nil {
		return nil, fmt.Errorf("json parse: %w", err)
	}
	var tracks []SubtitleTrack
	for _, t := range data.Tracks {
		if t.Type != "subtitles" {
			continue
		}
		lang := t.Properties.Language
		if lang == "" {
			lang = "und"
		}
		tracks = append(tracks, SubtitleTrack{
			TrackIndex:   t.ID,
			Language:     lang,
			TrackName:    t.Properties.TrackName,
			Codec:        codecFromMKVID(t.CodecID),
			Forced:       t.Properties.Forced,
			DefaultTrack: t.Properties.DefaultTrack,
		})
	}
	return tracks, nil
}

// stableShortHash returns the first 8 chars of a SHA-1 hash of s.
// Used as a per-source-file prefix for extracted subtitle filenames
// so two videos sharing a track index don't write to the same path.
func stableShortHash(s string) string {
	// Lightweight FNV-32 rather than SHA-1: we just need uniqueness
	// across the handful of videos in one release directory, not
	// cryptographic strength. 8 hex chars = 32 bits, collision
	// probability for n=10 inputs is ~10^-9.
	const offset32 = 2166136261
	const prime32 = 16777619
	h := uint32(offset32)
	for _, b := range []byte(s) {
		h ^= uint32(b)
		h *= prime32
	}
	return fmt.Sprintf("%08x", h)
}

package services

import (
	"context"
	"encoding/json"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// AudioCatalogTrack is one probed audio track from a source container.
// ProbeAudioTracks produces a slice of these per video; the caller
// bundles them into the Complete payload alongside subtitles.
//
// Pure metadata: we deliberately don't extract the audio bytes
// (see migration 217 — a single TrueHD track can be 2 GB).
type AudioCatalogTrack struct {
	SourcePath   string // the video file the track came from
	TrackIndex   int    // mkvmerge -J's tracks[N].id
	Language     string // ISO 639-2; "und" when source didn't carry one
	TrackName    string
	Codec        string // normalised: aac|ac3|eac3|dts|dts_hd|dts_hd_ma|truehd|flac|opus|mp3|pcm|other
	Channels     int    // 2, 6, 8, etc. (raw count; UI maps to 2.0/5.1/7.1)
	SampleRateHz int    // 0 when unknown
	BitrateKbps  int    // 0 when unknown (TrueHD/DTS-MA often don't report)
	DefaultTrack bool
	Forced       bool
}

// mkvmergeAudioJSON is the audio-specific shape returned by
// `mkvmerge -J INPUT`. Separate from mkvmergeJSON (subtitles) so each
// stays narrow — Go's encoding/json ignores unknown fields, but the
// audio_channels / audio_sampling_frequency properties only show up
// here so keeping them in one struct would be misleading.
type mkvmergeAudioJSON struct {
	Tracks []struct {
		ID         int    `json:"id"`
		Type       string `json:"type"`     // "video" | "audio" | "subtitles"
		Codec      string `json:"codec"`    // human label e.g. "DTS-HD Master Audio"
		CodecID    string `json:"codec_id"` // technical id e.g. "A_DTS"
		Properties struct {
			Language               string `json:"language"`
			LanguageIETF           string `json:"language_ietf"`
			TrackName              string `json:"track_name"`
			Forced                 bool   `json:"forced_track"`
			DefaultTrack           bool   `json:"default_track"`
			AudioChannels          int    `json:"audio_channels"`
			AudioSamplingFrequency int    `json:"audio_sampling_frequency"`
			TagBPS                 string `json:"tag_bps"` // bitrate in bits/sec when present
		} `json:"properties"`
	} `json:"tracks"`
}

// audioCodecFromMKV maps mkvmerge's (codec_id, codec) to the
// normalised codec label the site expects. codec_id alone is
// ambiguous for DTS variants — DTS, DTS-HD HRA, and DTS-HD MA all
// share A_DTS, with the variant only visible in the human "codec"
// label — so we look at both.
func audioCodecFromMKV(codecID, codecHuman string) string {
	id := strings.ToUpper(codecID)
	human := strings.ToLower(codecHuman)
	switch id {
	case "A_AAC", "A_AAC/MPEG2/LC", "A_AAC/MPEG4/LC":
		return "aac"
	case "A_AC3":
		return "ac3"
	case "A_EAC3":
		return "eac3"
	case "A_DTS":
		switch {
		case strings.Contains(human, "master audio") || strings.Contains(human, "dts-hd ma"):
			return "dts_hd_ma"
		case strings.Contains(human, "high resolution") || strings.Contains(human, "dts-hd hr") || strings.Contains(human, "dts-hd"):
			return "dts_hd"
		}
		return "dts"
	case "A_TRUEHD":
		return "truehd"
	case "A_FLAC":
		return "flac"
	case "A_OPUS":
		return "opus"
	case "A_MPEG/L3":
		return "mp3"
	case "A_PCM/INT/LIT", "A_PCM/INT/BIG", "A_PCM/FLOAT/IEEE":
		return "pcm"
	}
	return "other"
}

// ProbeAudioTracks runs `mkvmerge -J` on every video under srcDir
// and returns one AudioCatalogTrack per audio-type track. Pure read — never
// modifies the source. Skips when mkvmerge isn't on PATH so older
// agent images keep working (same forward-compat behaviour as
// ExtractSubtitles).
func ProbeAudioTracks(ctx context.Context, srcDir string) ([]AudioCatalogTrack, error) {
	log.Printf("audio: ProbeAudioTracks entry srcDir=%q", srcDir)
	if _, err := exec.LookPath("mkvmerge"); err != nil {
		log.Printf("audio: mkvmerge not found in PATH — skipping audio probe (install mkvtoolnix)")
		return nil, nil
	}
	videos := FindVideoFiles(srcDir)
	if len(videos) == 0 {
		return nil, nil
	}
	var out []AudioCatalogTrack
	for _, v := range videos {
		tracks, err := probeAudioForVideo(ctx, v)
		if err != nil {
			log.Printf("audio: probe %s: %v (continuing)", v, err)
			continue
		}
		out = append(out, tracks...)
	}
	return out, nil
}

func probeAudioForVideo(ctx context.Context, video string) ([]AudioCatalogTrack, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "mkvmerge", "-J", video)
	outBytes, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var data mkvmergeAudioJSON
	if err := json.Unmarshal(outBytes, &data); err != nil {
		return nil, err
	}
	var out []AudioCatalogTrack
	for _, t := range data.Tracks {
		if t.Type != "audio" {
			continue
		}
		lang := t.Properties.Language
		if lang == "" {
			lang = "und"
		}
		// tag_bps is "192000" → 192 kbps. mkvmerge omits it for
		// many MKVs (it's an optional element) — 0 means "unknown",
		// not "no bitrate", and the UI renders "—".
		bitrateKbps := 0
		if bps := strings.TrimSpace(t.Properties.TagBPS); bps != "" {
			if n, perr := strconv.ParseInt(bps, 10, 64); perr == nil && n > 0 {
				bitrateKbps = int(n / 1000)
			}
		}
		out = append(out, AudioCatalogTrack{
			SourcePath:   video,
			TrackIndex:   t.ID,
			Language:     lang,
			TrackName:    t.Properties.TrackName,
			Codec:        audioCodecFromMKV(t.CodecID, t.Codec),
			Channels:     t.Properties.AudioChannels,
			SampleRateHz: t.Properties.AudioSamplingFrequency,
			BitrateKbps:  bitrateKbps,
			DefaultTrack: t.Properties.DefaultTrack,
			Forced:       t.Properties.Forced,
		})
	}
	return out, nil
}

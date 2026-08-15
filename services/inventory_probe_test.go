package services

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/the-loon-clan/loon-agent/client"
)

// toInventoryMedia narrows the agent's full probe to what the offer page
// renders. The narrowing is the point — VideoInfo carries HDR side data, Dolby
// Vision profiles, chapters and pixel formats that no offer page shows — so
// what matters is that the fields it DOES keep survive intact, and that the
// unit conversion is right.
func TestToInventoryMediaKeepsWhatThePageShows(t *testing.T) {
	got := toInventoryMedia(&VideoInfo{
		Format:     "matroska",
		Duration:   1425.5,
		Bitrate:    4_200_000, // bits/sec on the way in
		VideoCodec: "hevc",
		Width:      1920,
		Height:     1080,
		FrameRate:  "23.976",
		AudioTracks: []AudioTrack{
			{Codec: "flac", Language: "jpn", Channels: 2, Default: true},
			{Codec: "aac", Language: "eng", Channels: 6},
		},
		SubtitleTracks: []SubTrack{
			{Codec: "ass", Language: "eng"},
			{Codec: "srt", Language: "spa", Forced: true},
		},
		// Present in the probe, deliberately absent from the payload.
		HDR:                "HDR10",
		DolbyVisionProfile: 8,
		PixelFormat:        "yuv420p10le",
	})
	if got == nil {
		t.Fatal("a complete probe produced nil")
	}
	if got.Container != "matroska" || got.VideoCodec != "hevc" {
		t.Errorf("container/codec = %q/%q", got.Container, got.VideoCodec)
	}
	if got.Width != 1920 || got.Height != 1080 {
		t.Errorf("dimensions = %dx%d", got.Width, got.Height)
	}
	if got.DurationSec != 1425.5 {
		t.Errorf("duration = %v", got.DurationSec)
	}
	// bits/sec -> kbps. Reporting 4,200,000 kbps on an offer page would be
	// visibly absurd, which is the only reason this is cheap to catch.
	if got.BitrateKbps != 4200 {
		t.Errorf("bitrate = %d kbps, want 4200 (ffprobe reports bits/sec)", got.BitrateKbps)
	}
	if len(got.AudioTracks) != 2 || got.AudioTracks[0].Language != "jpn" ||
		got.AudioTracks[0].Channels != 2 || !got.AudioTracks[0].Default {
		t.Errorf("audio tracks = %+v", got.AudioTracks)
	}
	if len(got.Subtitles) != 2 || got.Subtitles[1].Language != "spa" || !got.Subtitles[1].Forced {
		t.Errorf("subtitles = %+v", got.Subtitles)
	}
}

// A probe that found almost nothing is still worth sending: the site stamps
// the row as looked-at either way, and a container plus a duration beats the
// filename alone.
func TestToInventoryMediaHandlesSparseProbes(t *testing.T) {
	got := toInventoryMedia(&VideoInfo{Format: "avi", Duration: 60})
	if got == nil {
		t.Fatal("a sparse probe produced nil — the row would be retried forever")
	}
	if got.Container != "avi" || got.DurationSec != 60 {
		t.Errorf("sparse probe lost its fields: %+v", got)
	}
	if got.BitrateKbps != 0 || len(got.AudioTracks) != 0 {
		t.Errorf("sparse probe invented data: %+v", got)
	}
	// Nil in, nil out — the caller uses that to mean "looked, cannot describe".
	if toInventoryMedia(nil) != nil {
		t.Error("nil probe did not produce nil media")
	}
}

// resolve maps a site-relative path back onto a local file across several
// roots. Existence is CHECKED rather than assumed: a re-mounted or renamed
// root must fail per file, not silently probe whichever root happened to be
// first and report another file's metadata under this file's name.
func TestResolveChecksExistenceAcrossRoots(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	want := filepath.Join(rootB, "Show", "ep01.mkv")
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &InventoryProbeService{roots: []string{rootA, rootB}}

	got, ok := s.resolve("Show/ep01.mkv")
	if !ok {
		t.Fatal("a file present under the second root was not found")
	}
	if got != want {
		t.Errorf("resolved to %q, want %q", got, want)
	}

	// Absent everywhere: reported as undescribable rather than guessed at.
	if _, ok := s.resolve("Show/missing.mkv"); ok {
		t.Error("a path present in no root resolved anyway")
	}

	// A DIRECTORY matching the path is not a file. Probing one would produce a
	// confusing ffprobe error per tick rather than an honest "not found".
	if err := os.MkdirAll(filepath.Join(rootA, "Show", "adir.mkv"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.resolve("Show/adir.mkv"); ok {
		t.Error("a directory was accepted as the file to probe")
	}
}

// The batch cap is shared with the site; exceeding it is a 400, so the client
// refuses locally rather than spending a round trip to be told.
func TestInventoryMediaBatchCap(t *testing.T) {
	entries := make([]client.InventoryMediaEntry, client.InventoryMediaBatchMax+1)
	c := &client.SiteClient{}
	if _, err := c.InventoryReportMedia(entries); err == nil {
		t.Error("an over-cap batch was accepted locally")
	}
	// Empty is a no-op, not an error — a tick that probed nothing must not log
	// a failure.
	if resp, err := c.InventoryReportMedia(nil); err != nil || resp == nil || !resp.OK {
		t.Errorf("empty batch = (%v, %v), want a quiet success", resp, err)
	}
}

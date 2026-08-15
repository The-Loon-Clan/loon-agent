package services

// Inventory media prober.
//
// Walks the site's queue of staged-but-undescribed files, runs the same
// ffprobe the upload pipeline uses, and reports the result. The point is that
// an offer gets a detail page as informative as a release page for bytes
// nobody uploaded — the site has no other way to know what is in the file.
//
// TWO THINGS THIS DELIBERATELY DOES NOT DO.
//
// It does not generate screenshots. For this library that is ~160 GB against
// the site's free disk, and each one costs a video decode. ffprobe reads
// headers, so it can cover everything; screenshots belong on the publish path
// where the operator's own decision bounds how many exist.
//
// It does not walk the disk. The SITE decides what needs describing, because
// the site is the only side that knows what it already has — an agent deciding
// for itself would re-probe a finished library on every restart.

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/the-loon-clan/loon-agent/client"
	"github.com/the-loon-clan/loon-agent/config"
)

type InventoryProbeService struct {
	cfg   *config.Config
	site  *client.SiteClient
	roots []string
}

// NewInventoryProbeService returns (nil, nil) when inventory reporting is off.
// Probing without reporting would describe files the site has never heard of.
func NewInventoryProbeService(cfg *config.Config, site *client.SiteClient, offers *OfferConfig) (*InventoryProbeService, error) {
	if !cfg.InventoryEnabled || !cfg.InventoryProbeEnabled {
		return nil, nil
	}
	inv, err := NewInventoryService(cfg, site, offers)
	if err != nil || inv == nil {
		return nil, err
	}
	return &InventoryProbeService{cfg: cfg, site: site, roots: inv.roots}, nil
}

func (s *InventoryProbeService) report(level, format string, args ...interface{}) {
	(&InventoryService{site: s.site}).report(level, format, args...)
}

// Start runs the probe loop.
//
// The boot delay is longer than the walker's: the walker has to run FIRST or
// there is nothing queued to describe, and starting both together just means a
// wasted empty pass.
func (s *InventoryProbeService) Start(ctx context.Context) {
	go func() {
		select {
		case <-time.After(8 * time.Minute):
		case <-ctx.Done():
			return
		}
		if len(s.roots) == 0 {
			return
		}
		interval := time.Duration(s.cfg.InventoryProbeIntervalMin) * time.Minute
		if interval <= 0 {
			interval = 15 * time.Minute
		}
		for {
			s.runOnce(ctx)
			select {
			case <-time.After(interval):
			case <-ctx.Done():
				return
			}
		}
	}()
}

// runOnce describes one batch.
//
// Bounded per tick rather than draining the queue: 35,000 files is days of
// work, and an agent that spends every cycle probing is an agent not doing the
// job it was installed for. Little and often converges without anyone noticing.
func (s *InventoryProbeService) runOnce(ctx context.Context) {
	batch := s.cfg.InventoryProbeBatch
	if batch <= 0 || batch > client.InventoryMediaBatchMax {
		batch = 25
	}
	pending, err := s.site.InventoryPending(batch)
	if err != nil {
		s.report("error", "probe queue: %v", err)
		return
	}
	if len(pending) == 0 {
		return // library fully described; nothing to say
	}

	started := time.Now()
	entries := make([]client.InventoryMediaEntry, 0, len(pending))
	probed, unreadable, missing := 0, 0, 0

	for _, f := range pending {
		if ctx.Err() != nil {
			break
		}
		full, ok := s.resolve(f.Path)
		if !ok {
			// The site knows this path but no configured root holds it —
			// typically a root that was removed or re-mounted elsewhere.
			// Reported as undescribable so it leaves the queue.
			missing++
			entries = append(entries, client.InventoryMediaEntry{Path: f.Path})
			continue
		}
		info, err := ProbeVideo(ctx, full)
		if err != nil || info == nil {
			// Nil media is meaningful: "looked, could not describe". The site
			// stamps it probed either way, which is what stops an unreadable
			// file being retried every tick forever.
			unreadable++
			entries = append(entries, client.InventoryMediaEntry{Path: f.Path})
			continue
		}
		entries = append(entries, client.InventoryMediaEntry{Path: f.Path, Media: toInventoryMedia(info)})
		probed++
	}

	resp, err := s.site.InventoryReportMedia(entries)
	if err != nil {
		s.report("error", "probe report: %v", err)
		return
	}
	s.report("info", "described %d file(s) in %s — %d probed, %d unreadable, %d not found on disk (%d saved)",
		len(entries), time.Since(started).Round(time.Second), probed, unreadable, missing, resp.Saved)
}

// resolve maps a site-relative path back onto a local file.
//
// The site stores paths relative to the root the agent reported them from, and
// an agent may have several roots, so the only honest answer is to try each and
// take the one that exists. Checking existence rather than guessing is what
// makes a re-mounted or renamed root fail loudly per file instead of producing
// a probe of the wrong file.
func (s *InventoryProbeService) resolve(rel string) (string, bool) {
	for _, root := range s.roots {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if st, err := os.Stat(full); err == nil && st.Mode().IsRegular() {
			return full, true
		}
	}
	return "", false
}

// toInventoryMedia narrows the agent's full probe to what the offer page shows.
//
// VideoInfo carries HDR side data, Dolby Vision profiles, chapters, pixel
// formats — none of which the offer page renders. Shipping them would make the
// payload the widest part of the feature for no visible benefit, and the site
// would store fields nobody reads.
func toInventoryMedia(v *VideoInfo) *client.InventoryMedia {
	if v == nil {
		return nil
	}
	m := &client.InventoryMedia{
		DurationSec: v.Duration,
		Container:   v.Format,
		VideoCodec:  v.VideoCodec,
		Width:       v.Width,
		Height:      v.Height,
		FrameRate:   v.FrameRate,
	}
	// ffprobe reports bits per second; the page shows kbps.
	if v.Bitrate > 0 {
		m.BitrateKbps = int(v.Bitrate / 1000)
	}
	for _, a := range v.AudioTracks {
		m.AudioTracks = append(m.AudioTracks, client.InventoryAudioTrack{
			Language: a.Language, Codec: a.Codec, Channels: a.Channels, Default: a.Default,
		})
	}
	for _, sub := range v.SubtitleTracks {
		m.Subtitles = append(m.Subtitles, client.InventorySubtitle{
			Language: sub.Language, Codec: sub.Codec, Forced: sub.Forced,
		})
	}
	return m
}

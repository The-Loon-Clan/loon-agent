package services

import (
	"context"
	"errors"
	"fmt"
	"github.com/ameNZB/usenet-pipeline/config"
	"github.com/ameNZB/usenet-pipeline/storage"
	"github.com/ameNZB/usenet-pipeline/utils"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
	"golang.org/x/net/proxy"
	"golang.org/x/time/rate"
)

// ErrInsufficientDisk is returned by the pre-flight capacity check when a
// specific torrent won't fit on this agent. Callers use errors.Is to detect
// this case and skip the self-pause counter — one oversized torrent
// shouldn't pause the whole queue when smaller ones could still succeed.
var ErrInsufficientDisk = errors.New("insufficient disk space")

// NormalizeInfoHash uppercases base32-encoded infohashes (the 32-char
// magnet shape: A-Z + 2-7, RFC 4648). anacrolix's metainfo parser is
// strict about case — feeding it `xt=urn:btih:ghgsyznfvrqxz3iwed2hv...`
// (lowercase) fails with "error decoding xt: illegal base32 data at
// input byte 0". User-reported 2026-06-05.
//
// Hex infohashes (40 chars of [0-9a-fA-F]) are case-insensitive and
// pass through unchanged. Anything that isn't 32 chars or contains
// non-base32 characters also passes through, so the helper is safe
// to call on arbitrary strings.
func NormalizeInfoHash(h string) string {
	h = strings.TrimSpace(h)
	if len(h) != 32 {
		return h
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '2' && c <= '7')
		if !ok {
			return h
		}
	}
	return strings.ToUpper(h)
}

// DiskShortfallError wraps ErrInsufficientDisk with the discovered torrent
// size so the abort path can report it back to the site. Once the site
// stores size_bytes on the request, the poll dispatcher will filter this
// torrent out for every agent (and every future poll from this one) with
// equal-or-less free disk, eliminating the repeat metadata-fetch cost for
// the oversize backlog. errors.Is(err, ErrInsufficientDisk) keeps working
// via Unwrap; errors.As gives access to the fields.
type DiskShortfallError struct {
	TorrentBytes   int64 // total_length the agent just learned
	AvailableBytes int64 // what we had free after reservations
}

func (e *DiskShortfallError) Error() string {
	return fmt.Sprintf("insufficient disk space: torrent is %.1f GB, have %.1f GB free",
		float64(e.TorrentBytes)/1e9, float64(e.AvailableBytes)/1e9)
}

func (e *DiskShortfallError) Unwrap() error { return ErrInsufficientDisk }

// layeredInt pulls a whole-number setting from the layered config; returns
// the fallback when the key is unset or not parseable.
func layeredInt(cfg *config.Config, key string, fallback int) int {
	if cfg == nil || cfg.Layered == nil {
		return fallback
	}
	v := cfg.Layered.Effective(key)
	if v == "" {
		return fallback
	}
	if i, err := strconv.Atoi(v); err == nil {
		return i
	}
	return fallback
}

// layeredFloat is the float64 variant of layeredInt.
func layeredFloat(cfg *config.Config, key string, fallback float64) float64 {
	if cfg == nil || cfg.Layered == nil {
		return fallback
	}
	v := cfg.Layered.Effective(key)
	if v == "" {
		return fallback
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	return fallback
}

// seedOpts reads torrent_* knobs from the layered config. 0 means "don't
// seed" and skips the whole seeding phase — matching the pre-seeding
// behaviour so users who haven't enabled it see no change.
type seedOpts struct {
	UploadKBps  int
	RatioTarget float64
	MaxHours    float64
	RequireFull bool
	StallMins   int
	ListenPort  int
}

func seedOptsFromConfig(cfg *config.Config) seedOpts {
	return seedOpts{
		UploadKBps:  layeredInt(cfg, "torrent_max_upload_kbps", 0),
		RatioTarget: layeredFloat(cfg, "torrent_seed_ratio", 0),
		MaxHours:    layeredFloat(cfg, "torrent_seed_hours", 0),
		RequireFull: layeredInt(cfg, "torrent_require_full_seed", 0) == 1,
		StallMins:   layeredInt(cfg, "torrent_no_full_seed_timeout_mins", 0),
		ListenPort:  layeredInt(cfg, "torrent_port", 0),
	}
}

// applyVPNProxy configures the torrent client to route HTTP traffic
// (tracker announces, metadata fetches) through a SOCKS5 proxy
// (e.g. gluetun) when VPN_DOWNLOAD_ONLY is enabled.
//
// CRITICAL LIMITATION — user-reported 2026-06-04 by eee:
// anacrolix/torrent's peer-to-peer TCP connections are made via the
// package-internal listener/dialer and CANNOT be redirected through a
// SOCKS5 proxy from clientConfig alone. The two settings below
// (TrackerDialContext + HTTPDialContext) only cover HTTP traffic —
// tracker announces, metadata fetches over HTTP, the WebSeed fallback.
//
// What this MEANS for the user:
//   - Tracker announces: VPN IP ✓
//   - HTTP metadata:     VPN IP ✓
//   - PEER TCP CONNECTIONS (where the actual data flows): DIRECT ✗
//     The real agent IP is exposed to every peer in the swarm.
//
// For ACTUAL VPN protection, use full-tunnel mode:
//   VPN_DOWNLOAD_ONLY=false (default)
//   network_mode: service:vpn   (in docker-compose.yml)
// which routes ALL traffic through gluetun's tunnel — including
// peer connections — because anacrolix is running INSIDE the VPN's
// network namespace.
//
// Split-tunnel (this function's mode) is only "leak-free" for the
// HTTP signalling portion. Useful if you have a separate routing
// rule for peers (e.g. iptables policy routing inside the agent
// container), but useless on its own as a privacy control.
//
// The startup log now states this honestly so users don't get a
// false sense of protection from the (misleading) prior
// "torrent traffic routed via SOCKS5" line.
func applyVPNProxy(clientConfig *torrent.ClientConfig, cfg *config.Config) {
	if !cfg.VPNDownloadOnly || cfg.VPNProxyAddr == "" {
		return
	}
	dialer, err := proxy.SOCKS5("tcp", cfg.VPNProxyAddr, nil, proxy.Direct)
	if err != nil {
		log.Printf("WARNING: failed to create SOCKS5 dialer (%s): %v — downloads will NOT go through VPN", cfg.VPNProxyAddr, err)
		return
	}
	ctxDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		log.Printf("WARNING: SOCKS5 dialer does not support DialContext — downloads will NOT go through VPN")
		return
	}
	dialFn := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return ctxDialer.DialContext(ctx, network, addr)
	}
	clientConfig.TrackerDialContext = dialFn
	clientConfig.HTTPDialContext = dialFn
	proxyURL, _ := url.Parse("socks5://" + cfg.VPNProxyAddr)
	clientConfig.HTTPProxy = http.ProxyURL(proxyURL)
	log.Printf("VPN split-tunnel: HTTP/tracker traffic via SOCKS5 (%s). WARNING: peer-to-peer TCP connections still go DIRECT — your real IP is exposed to every peer in the swarm. For full VPN protection, set VPN_DOWNLOAD_ONLY=false and use network_mode: service:vpn in docker-compose.yml.",
		cfg.VPNProxyAddr)
}

// newTorrentClient creates a torrent.Client, and if the first attempt fails
// with a "port already in use" error (either because torrent_port was pinned
// to a busy port or a previous client's socket is still in TIME_WAIT), it
// retries once with ListenPort=0 so the OS picks a fresh random port. Every
// other error is returned unchanged.
func newTorrentClient(clientConfig *torrent.ClientConfig) (*torrent.Client, error) {
	client, err := torrent.NewClient(clientConfig)
	if err == nil {
		return client, nil
	}
	if !isBindInUseError(err) {
		return nil, err
	}
	log.Printf("torrent client bind failed on port %d (%v) — retrying with random port", clientConfig.ListenPort, err)
	clientConfig.ListenPort = 0
	return torrent.NewClient(clientConfig)
}

// isBindInUseError returns true if err looks like an "address already in use"
// bind failure from the torrent library's listener setup. Match is on string
// substrings because the library wraps the underlying OS error several levels
// deep ("first listen: listen tcp4 :NNNN: bind: address already in use").
func isBindInUseError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "address already in use") ||
		strings.Contains(s, "Only one usage of each socket address")
}

// DownloadPrivateTorrentBytes runs the .torrent-file download path against a
// blob the site handed us (from a private upload). The bytes are staged to
// a temp file so we can reuse the existing AddTorrentFromFile pipeline, and
// we force DHT off on the client so the info hash never leaves the private
// tracker's swarm even if the .torrent forgot to set info.private = 1.
func DownloadPrivateTorrentBytes(ctx context.Context, torrentBytes []byte, cfg *config.Config, jobName string, opts *DownloadOpts) (*TorrentSession, error) {
	tempPath := filepath.Join(cfg.TempDir, "dl-"+jobName+".torrent")
	if err := os.MkdirAll(filepath.Dir(tempPath), 0755); err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	if err := os.WriteFile(tempPath, torrentBytes, 0644); err != nil {
		return nil, fmt.Errorf("stage .torrent file: %w", err)
	}
	defer os.Remove(tempPath)
	return downloadTorrentFile(ctx, tempPath, cfg, jobName, true)
}

// DownloadCachedTorrentBytes is the public-torrent equivalent of
// DownloadPrivateTorrentBytes. Used when the site's metadata-prefetch
// worker has already resolved the .torrent for a public request, so
// the agent can skip its own 2-minute DHT round-trip. DHT stays on
// (peers come from DHT + the trackers baked into the .torrent), and
// no public-tracker injection happens — those trackers are already in
// the file.
func DownloadCachedTorrentBytes(ctx context.Context, torrentBytes []byte, cfg *config.Config, jobName string, opts *DownloadOpts) (*TorrentSession, error) {
	tempPath := filepath.Join(cfg.TempDir, "dl-"+jobName+".torrent")
	if err := os.MkdirAll(filepath.Dir(tempPath), 0755); err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	if err := os.WriteFile(tempPath, torrentBytes, 0644); err != nil {
		return nil, fmt.Errorf("stage .torrent file: %w", err)
	}
	defer os.Remove(tempPath)
	return downloadTorrentFile(ctx, tempPath, cfg, jobName, false)
}

// DownloadTorrent handles adding a torrent file and downloading its contents.
func DownloadTorrent(ctx context.Context, torrentPath string, cfg *config.Config, jobName string) (*TorrentSession, error) {
	return downloadTorrentFile(ctx, torrentPath, cfg, jobName, false)
}

func downloadTorrentFile(ctx context.Context, torrentPath string, cfg *config.Config, jobName string, privateMode bool) (*TorrentSession, error) {
	dataDir := filepath.Join(cfg.TempDir, "dl-"+jobName)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("create download dir: %w", err)
	}

	so := seedOptsFromConfig(cfg)
	clientConfig := torrent.NewDefaultClientConfig()
	clientConfig.DataDir = dataDir
	clientConfig.DisableIPv6 = true
	clientConfig.NoDefaultPortForwarding = true
	clientConfig.ListenPort = so.ListenPort // 0 = random
	if privateMode {
		// Private-tracker torrents must not leak their info hash to DHT
		// or trade peers over PEX/LSD. anacrolix honors info.private=1 in
		// the .torrent automatically, but we force it here too as a
		// belt-and-suspenders — even a mis-flagged .torrent stays
		// contained to the private tracker's swarm.
		clientConfig.NoDHT = true
	}
	if so.UploadKBps > 0 {
		// burst = 1s worth of tokens keeps the limiter responsive without
		// starving bursty writers. Values are bytes/sec, not bits.
		clientConfig.UploadRateLimiter = rate.NewLimiter(rate.Limit(so.UploadKBps*1024), so.UploadKBps*1024)
	}
	applyVPNProxy(clientConfig, cfg)

	client, err := newTorrentClient(clientConfig)
	if err != nil {
		return nil, err
	}

	t, err := client.AddTorrentFromFile(torrentPath)
	if err != nil {
		client.Close()
		return nil, err
	}

	log.Printf("Fetching metadata for %s...", filepath.Base(torrentPath))
	// File-based torrent adds should populate info() immediately
	// (anacrolix parses the .torrent on AddTorrentFromFile), so a
	// short timeout is plenty. A wait longer than this means the
	// .torrent is malformed (no info dict, magnet-only file) or
	// anacrolix is wedged — either way, fail-fast beats hanging
	// the agent on an unrecoverable torrent. User-reported 2026-06-04:
	// "agent stalls on downloads if it can't fetch torrent metadata".
	const fileMetaTimeout = 30 * time.Second
	select {
	case <-t.GotInfo():
		// proceed
	case <-time.After(fileMetaTimeout):
		t.Drop()
		client.Close()
		return nil, fmt.Errorf("metadata fetch timed out after %v for %s (malformed .torrent or anacrolix wedged)",
			fileMetaTimeout, filepath.Base(torrentPath))
	case <-ctx.Done():
		t.Drop()
		client.Close()
		return nil, ctx.Err()
	}

	// Mirror the magnet path's preflight: refuse the torrent if we
	// don't have effective free disk for it (other in-flight tasks'
	// reservations included), then claim our slice so concurrent
	// workers see it. Without this, .torrent-file downloads (private-
	// tracker path + cached-metadata path) silently report
	// Reserved=0 to the dashboard while consuming real bytes.
	torrentSize := t.Info().TotalLength()
	requiredBytes := int64(float64(torrentSize) * DiskMultiplier)
	if effective, dErr := FreeDiskAfterReservations(cfg.TempDir); dErr != nil {
		log.Printf("Warning: could not check disk space: %v", dErr)
	} else if effective < uint64(requiredBytes) {
		t.Drop()
		client.Close()
		return nil, &DiskShortfallError{
			TorrentBytes:   torrentSize,
			AvailableBytes: int64(effective),
		}
	} else {
		log.Printf("Disk space OK: %.1f GB effective free, reserving %.1f GB",
			float64(effective)/1e9, float64(requiredBytes)/1e9)
	}
	ReserveDisk(jobName, torrentSize)

	return downloadAndWaitSeed(ctx, client, t, dataDir, jobName, nil, so)
}

// DownloadMagnet downloads a torrent by magnet URI (used for site-assigned tasks).
// publicTrackers are appended to magnet URIs to improve metadata resolution.
var publicTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.stealth.si:80/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://exodus.desync.com:6969/announce",
	"http://nyaa.tracker.wf:7777/announce",
}

func DownloadMagnet(ctx context.Context, magnetURI string, cfg *config.Config, jobName string, opts *DownloadOpts) (*TorrentSession, error) {
	return downloadMagnet(ctx, magnetURI, cfg, jobName, opts, false)
}

// DownloadPrivateMagnet is like DownloadMagnet but skips the public-tracker
// injection — required for private-tracker torrents where announcing to the
// public trackers would leak the release to strangers and risk the user's
// tracker account. Private .torrent files already carry their own (private)
// announce list; we use that alone.
func DownloadPrivateMagnet(ctx context.Context, magnetURI string, cfg *config.Config, jobName string, opts *DownloadOpts) (*TorrentSession, error) {
	return downloadMagnet(ctx, magnetURI, cfg, jobName, opts, true)
}

func downloadMagnet(ctx context.Context, magnetURI string, cfg *config.Config, jobName string, opts *DownloadOpts, privateMode bool) (*TorrentSession, error) {
	// Append public trackers if not already present — but never for private
	// torrents, where announcing to the public trackers would de-anonymize
	// the user's private-tracker traffic.
	if !privateMode {
		for _, tr := range publicTrackers {
			if !strings.Contains(magnetURI, tr) {
				magnetURI += "&tr=" + url.QueryEscape(tr)
			}
		}
	}

	// Each download gets its own data dir to avoid piece-completion DB conflicts
	// when multiple torrent clients run concurrently.
	dataDir := filepath.Join(cfg.TempDir, "dl-"+jobName)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("create download dir: %w", err)
	}

	so := seedOptsFromConfig(cfg)
	clientConfig := torrent.NewDefaultClientConfig()
	clientConfig.DataDir = dataDir
	clientConfig.DisableIPv6 = true
	clientConfig.NoDefaultPortForwarding = true
	clientConfig.ListenPort = so.ListenPort // 0 = random
	if so.UploadKBps > 0 {
		// burst = 1s worth of tokens keeps the limiter responsive without
		// starving bursty writers. Values are bytes/sec, not bits.
		clientConfig.UploadRateLimiter = rate.NewLimiter(rate.Limit(so.UploadKBps*1024), so.UploadKBps*1024)
	}
	applyVPNProxy(clientConfig, cfg)

	client, err := newTorrentClient(clientConfig)
	if err != nil {
		return nil, err
	}

	t, err := client.AddMagnet(magnetURI)
	if err != nil {
		client.Close()
		return nil, err
	}

	log.Printf("Fetching metadata for magnet (timeout 2min)...")
	metaTimeout := time.After(2 * time.Minute)
	select {
	case <-t.GotInfo():
		log.Printf("Metadata received: %s (%d bytes)", t.Name(), t.Info().TotalLength())
	case <-metaTimeout:
		t.Drop()
		client.Close()
		return nil, fmt.Errorf("metadata fetch timed out after 2 minutes (DHT/trackers unreachable)")
	case <-ctx.Done():
		t.Drop()
		client.Close()
		return nil, ctx.Err()
	}

	// Check disk space accounting for other in-flight tasks' reservations.
	torrentSize := t.Info().TotalLength()
	requiredBytes := int64(float64(torrentSize) * DiskMultiplier)
	effective, err := FreeDiskAfterReservations(cfg.TempDir)
	if err != nil {
		log.Printf("Warning: could not check disk space: %v", err)
	} else if effective < uint64(requiredBytes) {
		t.Drop()
		client.Close()
		return nil, &DiskShortfallError{
			TorrentBytes:   torrentSize,
			AvailableBytes: int64(effective),
		}
	} else {
		log.Printf("Disk space OK: %.1f GB effective free, reserving %.1f GB",
			float64(effective)/1e9, float64(requiredBytes)/1e9)
	}

	// Reserve space NOW (before download starts) so concurrent tasks see it.
	ReserveDisk(jobName, torrentSize)

	return downloadAndWaitSeed(ctx, client, t, dataDir, jobName, opts, so)
}

// defaultMaxSeedHours caps the seed phase when the operator configured a
// ratio target but left torrent_seed_hours=0. Without an upper bound a
// torrent that never reaches its ratio (dead/slow swarm) seeds forever and
// pins its download dir + disk reservation until the process restarts.
const defaultMaxSeedHours = 24.0

// seedNoProgressTimeout ends seeding early when there are no peers AND no
// upload progress for this long — a dead swarm can't ever meet the ratio,
// so there's no point holding the downloaded files for the full cap.
const seedNoProgressTimeout = 30 * time.Minute

// runSeedPhase keeps the torrent active after download completion until
// one of these boundary conditions is hit: target upload ratio reached,
// max seed time elapsed, or the context is cancelled. The UploadRateLimiter
// configured on the client bounds outbound bandwidth; here we only track
// progress and emit status updates the dashboard renders as a seed bar.
//
// Ratio is computed as bytesWritten / torrentSize — close enough for the
// display and the stopping condition. anacrolix/torrent doesn't expose a
// first-class ratio getter, so this stays manual.
func runSeedPhase(ctx context.Context, t *torrent.Torrent, jobName string, so seedOpts) {
	total := t.Length()
	if total <= 0 {
		return
	}
	// Always bound the seed phase in time. When the operator set a ratio
	// target but left torrent_seed_hours=0, fall back to defaultMaxSeedHours
	// — otherwise a torrent that never reaches its ratio seeds FOREVER.
	// That is a disk leak, not just wasted bandwidth: processTask calls
	// Seed as its last statement, so the download dir removal + ReleaseDisk
	// defers don't run until Seed returns. An unbounded seed pins both until
	// a restart — the reported "eats disk while idle → restart daily".
	maxHours := so.MaxHours
	if maxHours <= 0 {
		maxHours = defaultMaxSeedHours
	}
	deadline := time.Now().Add(time.Duration(maxHours * float64(time.Hour)))
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	log.Printf("[%s] Seeding: ratio target %.2f, max %.1fh, cap %d KB/s",
		jobName, so.RatioTarget, maxHours, so.UploadKBps)
	var lastUp, lastDown int64
	var lastProgressUp int64
	lastProgressAt := time.Now()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[%s] Seeding: cancelled", jobName)
			return
		case <-ticker.C:
			stats := t.Stats()
			uploaded := stats.BytesWrittenData.Int64()
			downloaded := stats.BytesReadData.Int64()
			ratio := float64(uploaded) / float64(total)
			upSpeed := float64(uploaded-lastUp) / 2.0 / 1024 / 1024 // MB/s (2s tick)
			dnSpeed := float64(downloaded-lastDown) / 2.0 / 1024 / 1024
			lastUp = uploaded
			lastDown = downloaded
			peers := len(t.PeerConns())

			// Dead-swarm exit: niche torrents often land in a swarm with no
			// peers that can never satisfy the ratio. Rather than pin the
			// download dir + reservation for the full fallback window, stop
			// once there's been no upload progress AND no peers for a
			// sustained stretch.
			if uploaded > lastProgressUp {
				lastProgressUp = uploaded
				lastProgressAt = time.Now()
			}
			if peers == 0 && time.Since(lastProgressAt) > seedNoProgressTimeout {
				log.Printf("[%s] Seeding: no upload progress + no peers for %v, stopping (dead swarm)",
					jobName, seedNoProgressTimeout)
				return
			}

			// Surface seed progress through the existing callback so the
			// dashboard can render a ratio/time bar without a new channel.
			if cb := GetProgressCallback(jobName); cb != nil {
				// Percent here is seed progress (ratio%) — the "phase"
				// value tells the dashboard to switch to a seed bar.
				var pct float64
				if so.RatioTarget > 0 {
					pct = ratio / so.RatioTarget * 100
				} else if !deadline.IsZero() {
					total := deadline.Sub(deadline.Add(-time.Duration(so.MaxHours * float64(time.Hour))))
					elapsed := time.Since(deadline.Add(-time.Duration(so.MaxHours * float64(time.Hour))))
					pct = float64(elapsed) / float64(total) * 100
				}
				if pct > 100 {
					pct = 100
				}
				// Total/transferred during seeding: keep the original
				// torrent size as "total" and bytes-uploaded so far as
				// "transferred" so the dashboard can show "uploaded
				// X / Y" instead of a bare ratio. ETA is ratio- or
				// time-bounded, not byte-bounded, so leave it as 0.
				cb(dnSpeed, upSpeed, pct, "seeding", peers, t.Length(), uploaded, 0, nil)
			}
			storage.UpdateState(jobName, "Seeding",
				fmt.Sprintf("ratio %.3f / %.2f - %.2f MB/s up - %d peers", ratio, so.RatioTarget, upSpeed, peers),
				0)

			if so.RatioTarget > 0 && ratio >= so.RatioTarget {
				log.Printf("[%s] Seeding: ratio target %.2f reached (%.3f)", jobName, so.RatioTarget, ratio)
				return
			}
			if !deadline.IsZero() && time.Now().After(deadline) {
				log.Printf("[%s] Seeding: max time %.1fh elapsed (ratio %.3f)", jobName, so.MaxHours, ratio)
				return
			}
		}
	}
}

// ProgressWarning is one rule-violation countdown surfaced to the site
// so the admin / owner dashboards can render a live "will skip in X"
// indicator. ExpiresAt is absolute (now + remaining timeout), so the
// browser ticks it down without extra polling.
type ProgressWarning struct {
	Kind      string    // slow_speed | low_peers | stalled
	Label     string    // hover description
	Icon      string    // emoji shown next to the timer
	ExpiresAt time.Time // when the rule will trigger a skip
}

// ProgressCallback is called periodically with the running stats for a
// task. downMBs is the payload receive rate, upMBs is the payload send
// rate (peer uploads in a torrent phase, NNTP upload rate in the usenet
// phase). totalBytes / transferredBytes are the absolute counters so
// the dashboard can render "X / Y MB" without re-deriving from percent
// (which loses precision and bottoms out at 100). etaSeconds is the
// remaining time at the current speed; 0 means unknown / not applicable.
// warnings is the current active-rule countdown set; empty means the
// task is healthy.
type ProgressCallback func(downMBs float64, upMBs float64, percent float64, phase string, peers int, totalBytes int64, transferredBytes int64, etaSeconds float64, warnings []ProgressWarning)

// ErrSlowDownload is returned when a download is rejected for being too slow.
var ErrSlowDownload = fmt.Errorf("download rejected: speed too low for too long")

// DownloadOpts holds optional parameters for the download loop.
type DownloadOpts struct {
	SlowThresholdMBs    float64 // speed below this is "slow" (0 = no limit)
	SlowTimeoutMins     int     // minutes of sustained slow speed before rejection
	LowPeersThreshold   int     // skip if seeds <= this (-1 = disabled)
	LowPeersTimeoutMins int     // minutes of sustained low seeds before rejection
	IsBoosted           bool    // boosted requests tolerate slow (non-zero) speeds
}

// TorrentSession is a successfully-downloaded torrent whose client and
// torrent handle are still active. The caller owns the lifecycle and MUST
// defer Close immediately after a Download* call returns nil error —
// without it the torrent client and its goroutines leak.
//
// Between download and Close, the caller may invoke Seed to run the
// optional ratio/time-bounded seeding phase. The split exists so the
// agent can post the NZB and report the request fulfilled before seeding
// starts: users see the request as available the moment the report
// lands, while the agent goes on sharing back to the swarm in the
// background.
type TorrentSession struct {
	Path     string // path to downloaded files (was the old (string, error) return)
	client   *torrent.Client
	torrent  *torrent.Torrent
	jobName  string
	seedOpts seedOpts
}

// Seed runs the post-download seeding phase if the user has configured
// torrent_seed_ratio or torrent_seed_hours. No-op otherwise. Blocks
// until the ratio/time target is met or the context is cancelled.
func (s *TorrentSession) Seed(ctx context.Context) {
	if s == nil || s.torrent == nil {
		return
	}
	if s.seedOpts.RatioTarget > 0 || s.seedOpts.MaxHours > 0 {
		runSeedPhase(ctx, s.torrent, s.jobName, s.seedOpts)
	}
}

// Close shuts down the torrent client and is safe on a nil session and
// safe to call multiple times. Always defer immediately after a successful
// Download*.
func (s *TorrentSession) Close() {
	if s == nil || s.client == nil {
		return
	}
	s.client.Close()
	s.client = nil
}

// ExpectedBytes returns the torrent's manifested total length — what
// the metainfo says SHOULD be on disk after a complete download.
// Returns 0 if the session has already been Close()'d or never got a
// torrent handle (defensive — callers in cmd/agent/main.go can safely
// use this value in a comparison even after Close).
//
// Used by the pre-stage size-parity check: if the actual bytes on
// disk are wildly less than ExpectedBytes() right before staging,
// something deleted files between download-complete and stage-start
// (disk-sweep race, manual rm, FS corruption). Aborting beats
// silently shipping a partial NZB.
func (s *TorrentSession) ExpectedBytes() int64 {
	if s == nil || s.torrent == nil {
		return 0
	}
	return s.torrent.Length()
}

// ExpectedFile is one entry from a torrent's metainfo: the relative
// path the file should land at + its expected byte length. Used by
// the pre-stage parity check in cmd/agent/main.go to verify EVERY
// file the torrent promised is actually on disk before staging.
//
// Path is rooted relative to dataDir as anacrolix would lay it out:
//   single-file torrent  → just "<filename>"
//   multi-file torrent   → "<torrent-name>/<inner/path/file>"
// This matches what filepath.Join(dataDir, ef.Path) resolves to.
type ExpectedFile struct {
	Path string // relative to dataDir
	Size int64
}

// ExpectedFiles enumerates every file the torrent's metainfo declared,
// in the layout anacrolix actually wrote them. Used by the pre-stage
// check to catch the "specific file missing while total bytes are
// almost right" failure mode — a byte total of 25 of 26 GB might
// look OK but be one corrupt/zero-byte file the byte check misses.
//
// For multi-file torrents, anacrolix wraps the file tree in a
// directory named after t.Name(). Single-file torrents land at the
// dataDir root. The caller (main.go) joins ef.Path against dataDir
// when checking disk.
func (s *TorrentSession) ExpectedFiles() []ExpectedFile {
	if s == nil || s.torrent == nil {
		return nil
	}
	info := s.torrent.Info()
	if info == nil {
		return nil
	}
	files := s.torrent.Files()
	if len(files) == 0 {
		return nil
	}
	out := make([]ExpectedFile, 0, len(files))
	// Single-file torrent: Files() returns a single File whose
	// DisplayPath() == the torrent's Name. Multi-file: the name is
	// the wrapping dir, DisplayPath is the inner path.
	for _, f := range files {
		// f.Path() returns the inner path components; for multi-file
		// the wrapping name is added at write-time. DisplayPath
		// already includes the wrap, which is what we want.
		p := f.DisplayPath()
		// For multi-file torrents anacrolix writes to
		// dataDir/<torrent-name>/<inner>. DisplayPath() returns
		// "<inner>" for some library versions, "<wrapname>/<inner>"
		// for others — normalise by checking if it already starts
		// with t.Name()+"/".
		name := info.Name
		if name != "" && len(files) > 1 {
			if !strings.HasPrefix(p, name+"/") && !strings.HasPrefix(p, name+string(os.PathSeparator)) {
				p = name + "/" + p
			}
		}
		out = append(out, ExpectedFile{Path: p, Size: f.Length()})
	}
	return out
}

// downloadAndWait runs the download loop with progress reporting.
func downloadAndWait(ctx context.Context, cl *torrent.Client, t *torrent.Torrent, dataDir string, jobName string, opts *DownloadOpts) (*TorrentSession, error) {
	return downloadAndWaitSeed(ctx, cl, t, dataDir, jobName, opts, seedOpts{})
}

// downloadAndWaitSeed runs the download loop and returns a session the
// caller can later Seed and Close. The seed phase is no longer run inline
// here — that used to block the post-download pipeline (Usenet upload +
// site.Complete) for hours and made the request appear unfulfilled to
// users while the NZB was already sittable. Callers invoke session.Seed
// after the report has landed instead.
//
// Error paths close the client themselves since no session is returned.
func downloadAndWaitSeed(ctx context.Context, cl *torrent.Client, t *torrent.Torrent, dataDir string, jobName string, opts *DownloadOpts, so seedOpts) (*TorrentSession, error) {
	log.Printf("Downloading %s (%d bytes)...", t.Name(), t.Info().TotalLength())
	t.DownloadAll()

	// cl.WaitAll blocks until every torrent in the client finishes;
	// it has no ctx flavour. We wrap it so that on ctx cancel the
	// caller's ctx.Done() branch can cl.Close() (which forces
	// WaitAll to return) and then drain done before returning, so
	// the goroutine never leaks past the parent. Without joining
	// done in the cancel path, a SIGTERM mid-download leaves the
	// WaitAll goroutine blocked on the torrent socket until the OS
	// reaps the process.
	done := make(chan struct{})
	go func() {
		cl.WaitAll()
		close(done)
	}()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	total := t.Length()
	var lastCompleted int64
	var lastUploaded int64
	var lastLog time.Time
	startedAt := time.Now()
	var stallSince time.Time

	// Snapshot of the last progress tick — captured by every ticker
	// iteration and emitted by every exit log line so a "what just
	// happened" question can be answered in one log line per task,
	// not three separated by minutes of unrelated chatter. peakPeers
	// surfaces "did this swarm EVER have any peers" — critical for
	// the dead-swarm diagnosis. Strings are cheap; the goal is to
	// make overnight failures self-diagnosing.
	var lastProgressSnapshot string
	var peakPeers int

	log.Printf("[%s] download loop start: torrent=%q length=%d bytes (%.2f GB) initial-peers=%d data-dir=%s",
		jobName, t.Name(), total, float64(total)/(1<<30), len(t.PeerConns()), dataDir)

	// Slow download tracking.
	var slowSince time.Time
	slowTimeout := time.Duration(0)
	slowThreshold := 0.0
	isBoosted := false
	// Low peer tracking.
	var lowPeersSince time.Time
	lowPeersTimeout := time.Duration(0)
	lowPeersThreshold := -1 // -1 = disabled

	// Hysteresis: require N consecutive good ticks before clearing a
	// rule's "since" timer. Without this the dashboard warning
	// flickers as speed/peers bounce around the threshold every tick.
	const recoveryTicksRequired = 3
	var slowRecovery, stallRecovery, lowPeersRecovery int
	if opts != nil {
		slowThreshold = opts.SlowThresholdMBs
		if opts.SlowTimeoutMins > 0 {
			slowTimeout = time.Duration(opts.SlowTimeoutMins) * time.Minute
		}
		isBoosted = opts.IsBoosted
		lowPeersThreshold = opts.LowPeersThreshold
		if opts.LowPeersTimeoutMins > 0 {
			lowPeersTimeout = time.Duration(opts.LowPeersTimeoutMins) * time.Minute
		}
	}

	for {
		select {
		case <-ctx.Done():
			log.Printf("[%s] download cancelled by ctx after %s — last-progress=%q peak-peers=%d reason=%v",
				jobName, time.Since(startedAt).Round(time.Second), lastProgressSnapshot, peakPeers, ctx.Err())
			t.Drop()
			cl.Close()
			// Drain the WaitAll goroutine so it exits before we
			// return — cl.Close() above is what unblocks it.
			<-done
			return nil, ctx.Err()
		case <-done:
			// Log concrete completion stats BEFORE marking 100% in storage.
			// cl.WaitAll() can return with completed < length on dead-swarm /
			// zero-length-info / removed-while-fetching cases; without this
			// line the "100% (Download Complete)" UpdateState below lies and
			// the operator has to infer "did the download actually happen?"
			// from the downstream walker being empty. Including peer count
			// + file count makes the next failure self-diagnosing in one log
			// line.
			completedBytes := t.BytesCompleted()
			totalLength := t.Length()
			fileCount := 0
			if info := t.Info(); info != nil {
				fileCount = len(info.UpvertedFiles())
			}
			peerCount := len(t.PeerConns())
			runtime := time.Since(startedAt).Round(time.Second)
			if completedBytes < totalLength {
				pct := 0.0
				if totalLength > 0 {
					pct = float64(completedBytes) / float64(totalLength) * 100
				}
				log.Printf("[%s] WARNING: WaitAll returned but completed %d / %d bytes (%.1f%%) in %s — anacrolix signalled premature completion. peak-peers=%d last-progress=%q files=%d. Downstream walker will likely find nothing.",
					jobName, completedBytes, totalLength, pct, runtime, peakPeers, lastProgressSnapshot, fileCount)
			} else {
				log.Printf("[%s] download done: name=%q bytes=%d files=%d peers=%d peak-peers=%d runtime=%s last-progress=%q",
					jobName, t.Name(), completedBytes, fileCount, peerCount, peakPeers, runtime, lastProgressSnapshot)
			}
			storage.UpdateState(jobName, "Downloading", "100% (Download Complete)", 100)
			if cb := GetProgressCallback(jobName); cb != nil {
				cb(0, 0, 100, "downloading", 0, t.Length(), t.Length(), 0, nil)
			}
			// session.Path is ALWAYS the per-request temp dir (dataDir),
			// not the inner <dataDir>/<t.Name()> path. Reasoning:
			//
			// anacrolix writes single-file torrents to
			//   <dataDir>/<filename>
			// and multi-file torrents to
			//   <dataDir>/<torrent-name>/<file...>
			//
			// Every downstream consumer of session.Path (RemoveBlockedFiles,
			// DirHasUsableFiles, FindVideoFiles, FindMangaArchive,
			// RunRemux, RunUpscale, ObfuscateFiles, CopyFiles,
			// ManifestOf, and any filepath.Join(_, "_subtitles") /
			// "_screenshots" / etc) expects a DIRECTORY. The old
			// "return the inner path" convention silently broke for
			// single-file torrents because the returned path was a
			// regular file — directory walks tolerated that, but
			// filepath.Join(file_path, "_subtitles") → mkdir is an
			// "<file>/_subtitles: not a directory" error.
			//
			// Returning dataDir uniformly fixes the directory-shaped
			// callers, simplifies the cleanup defer (os.RemoveAll
			// removes the whole per-request temp dir), and the walks
			// still find the content because dataDir is one level
			// above where anacrolix wrote.
			//
			// Diagnostic-only: still lstat the expected inner path so
			// the operator can see in the log when anacrolix signals
			// completion but the expected file isn't where we thought
			// it would be (dead-swarm / sanitization-mismatch cases).
			// The log is informational; routing always uses dataDir.
			sessionPath := dataDir
			expectedInner := filepath.Join(dataDir, t.Name())
			if _, statErr := os.Lstat(expectedInner); statErr != nil {
				log.Printf("[%s] expected output %q missing after WaitAll (%v) — anacrolix may have written to a different subpath or signalled premature completion; downstream walker will descend %q regardless",
					jobName, expectedInner, statErr, dataDir)
			}
			// Hand the client + torrent off to the caller. They post the
			// NZB and call site.Complete first, then invoke session.Seed
			// for the optional ratio/time seed phase. This makes the
			// request fulfilled (visible to users) before seeding starts
			// instead of hours after.
			return &TorrentSession{
				Path:     sessionPath,
				client:   cl,
				torrent:  t,
				jobName:  jobName,
				seedOpts: so,
			}, nil
		case <-ticker.C:
			completed := t.BytesCompleted()
			peers := len(t.PeerConns())
			if peers > peakPeers {
				peakPeers = peers
			}
			dlStats := t.Stats()
			uploaded := dlStats.BytesWrittenData.Int64()
			if total > 0 {
				percent := float64(completed) / float64(total) * 100
				speed := float64(completed-lastCompleted) / 1024 / 1024
				upSpeed := float64(uploaded-lastUploaded) / 1024 / 1024

				// Update the per-tick snapshot so every exit log line
				// can include "what the download looked like just before
				// it ended". Captured outside the if total>0 guard so
				// even zero-length-info torrents get a useful state on
				// exit.
				lastProgressSnapshot = fmt.Sprintf("%.1f%% (%d/%d bytes) %.2f MB/s peers=%d (peak=%d)",
					percent, completed, total, speed, peers, peakPeers)

				var etaSeconds float64
				etaStr := "Calculating..."
				if speed > 0 {
					remainingMB := float64(total-completed) / 1024 / 1024
					etaSeconds = remainingMB / speed
					etaStr = utils.FormatETA(etaSeconds)
				}

				lastCompleted = completed
				lastUploaded = uploaded
				storage.UpdateState(jobName, "Downloading", fmt.Sprintf("%.1f%% (%.2f / %.2f MB) - %.2f MB/s dn / %.2f MB/s up - ETA: %s - %d peers", percent, float64(completed)/1024/1024, float64(total)/1024/1024, speed, upSpeed, etaStr, peers), percent)

				// Periodic log so stdout isn't silent during long downloads.
				if time.Since(lastLog) >= 30*time.Second {
					lastLog = time.Now()
					log.Printf("[%s] %.1f%% (%.1f/%.1f GB) %.2f MB/s %d peers",
						jobName, percent,
						float64(completed)/1e9, float64(total)/1e9,
						speed, peers)
				}

				// Slow download detection — skip the first 30s to allow ramp-up.
				slowActive := false
				if slowTimeout > 0 && slowThreshold > 0 && percent < 95 {
					isSlow := speed < slowThreshold
					// Boosted requests are only rejected if speed is truly zero.
					if isBoosted {
						isSlow = speed == 0
					}

					if isSlow {
						slowRecovery = 0
						if slowSince.IsZero() {
							slowSince = time.Now()
						} else if time.Since(slowSince) > slowTimeout {
							log.Printf("[%s] Rejecting slow download: %.4f MB/s for %v (threshold: %.2f MB/s, boosted: %v) — runtime=%s peak-peers=%d last-progress=%q",
								jobName, speed, time.Since(slowSince).Round(time.Second), slowThreshold, isBoosted,
								time.Since(startedAt).Round(time.Second), peakPeers, lastProgressSnapshot)
							t.Drop()
							cl.Close()
							return nil, ErrSlowDownload
						}
						slowActive = true
					} else if !slowSince.IsZero() {
						// Hysteresis: only clear the timer after several
						// consecutive good ticks so the dashboard
						// countdown doesn't flicker on threshold edges.
						slowRecovery++
						if slowRecovery >= recoveryTicksRequired {
							slowSince = time.Time{}
							slowRecovery = 0
						} else {
							slowActive = true
						}
					}
				}

				// Full-seed gate + stall detection (torrent_* layered settings).
				// If RequireFull is set, we treat the first 60s with zero
				// progress as "no full peer reachable" and drop. Past that,
				// StallMins minutes of zero progress + zero speed drops too.
				if so.RequireFull && completed == 0 && time.Since(startedAt) > 60*time.Second {
					log.Printf("[%s] Rejecting: no full seed (0 bytes after 60s) — peak-peers=%d (swarm was %s)",
						jobName, peakPeers, func() string {
							if peakPeers == 0 {
								return "completely dead"
							}
							return "live but no full peer"
						}())
					t.Drop()
					cl.Close()
					return nil, ErrSlowDownload
				}
				// No percent gate: a torrent stuck at 99.x% with 0 peers
				// never finishes — the earlier `percent < 99` cap let those
				// hold a slot forever. StallMins itself is the knob.
				//
				// BUT: skip the check entirely once bytes are fully
				// downloaded. The select { <-done : <-ticker.C } race can
				// pick ticker even after WaitAll has signalled completion;
				// without this guard the stall timer trips on a
				// 100%-complete download that's just waiting for the upload
				// slot to open, producing the user-reported "Downloads
				// complete but don't start uploading, then fail with
				// Download too slow — skipping" symptom. The done case
				// will be picked on a subsequent select iteration and
				// return the session cleanly.
				stallActive := false
				if so.StallMins > 0 && completed < total {
					if speed == 0 {
						stallRecovery = 0
						if stallSince.IsZero() {
							stallSince = time.Now()
						} else if time.Since(stallSince) > time.Duration(so.StallMins)*time.Minute {
							log.Printf("[%s] Rejecting stalled download: 0 speed for %v at %.1f%% — runtime=%s peak-peers=%d last-progress=%q",
								jobName, time.Since(stallSince).Round(time.Second), percent,
								time.Since(startedAt).Round(time.Second), peakPeers, lastProgressSnapshot)
							t.Drop()
							cl.Close()
							return nil, ErrSlowDownload
						}
						stallActive = true
					} else if !stallSince.IsZero() {
						stallRecovery++
						if stallRecovery >= recoveryTicksRequired {
							stallSince = time.Time{}
							stallRecovery = 0
						} else {
							stallActive = true
						}
					}
				} else if completed >= total {
					// Downloaded — clear any stall timer that may have
					// started during the final tick race so a subsequent
					// timer trip can't fire on a now-completed download.
					stallSince = time.Time{}
				}

				// Low peer detection — skip if seeds stay at or below threshold.
				lowPeersActive := false
				if lowPeersTimeout > 0 && lowPeersThreshold >= 0 && percent < 95 {
					if peers <= lowPeersThreshold {
						lowPeersRecovery = 0
						if lowPeersSince.IsZero() {
							lowPeersSince = time.Now()
						} else if time.Since(lowPeersSince) > lowPeersTimeout {
							log.Printf("[%s] Rejecting low-seed download: %d peers for %v (threshold: %d) — runtime=%s peak-peers=%d last-progress=%q",
								jobName, peers, time.Since(lowPeersSince).Round(time.Second), lowPeersThreshold,
								time.Since(startedAt).Round(time.Second), peakPeers, lastProgressSnapshot)
							t.Drop()
							cl.Close()
							return nil, ErrSlowDownload
						}
						lowPeersActive = true
					} else if !lowPeersSince.IsZero() {
						lowPeersRecovery++
						if lowPeersRecovery >= recoveryTicksRequired {
							lowPeersSince = time.Time{}
							lowPeersRecovery = 0
						} else {
							lowPeersActive = true
						}
					}
				}

				// Build the live warnings list from whichever rules are
				// currently counting down. ExpiresAt is absolute so the
				// browser can tick the countdown without re-polling.
				var warnings []ProgressWarning
				if slowActive && !slowSince.IsZero() {
					warnings = append(warnings, ProgressWarning{
						Kind:      "slow_speed",
						Label:     fmt.Sprintf("Speed below %.2f MB/s — will skip", slowThreshold),
						Icon:      "🐢",
						ExpiresAt: slowSince.Add(slowTimeout),
					})
				}
				if stallActive && !stallSince.IsZero() {
					warnings = append(warnings, ProgressWarning{
						Kind:      "stalled",
						Label:     "Download stalled at 0 MB/s — will skip",
						Icon:      "⏸",
						ExpiresAt: stallSince.Add(time.Duration(so.StallMins) * time.Minute),
					})
				}
				if lowPeersActive && !lowPeersSince.IsZero() {
					warnings = append(warnings, ProgressWarning{
						Kind:      "low_peers",
						Label:     fmt.Sprintf("Peers ≤ %d — will skip", lowPeersThreshold),
						Icon:      "👥",
						ExpiresAt: lowPeersSince.Add(lowPeersTimeout),
					})
				}

				if cb := GetProgressCallback(jobName); cb != nil {
					cb(speed, upSpeed, percent, "downloading", peers, total, completed, etaSeconds, warnings)
				}
			}
		}
	}
}

package config

import (
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	// Site connection
	SiteURL      string
	AgentToken   string
	PollInterval int // seconds between polls
	// SiteName is the display label for the site this agent talks to.
	// Pulled into the local UI sidebar and any future operator-facing
	// log lines so forks of loon-agent don't have to grep + edit
	// hardcoded brand strings. Empty falls back to a neutral default
	// at render time so the UI still looks finished out of the box.
	// Configure via SITE_NAME env var or the on-disk config.yml.
	SiteName string

	// Directories
	TempDir  string
	WatchDir string // hand-off dir for AGENT_MODE=watch_folder; also legacy for offline .torrent drops
	DoneDir  string // where the user's BT client deposits the completed download in watch_folder mode

	// AgentMode controls how the per-task download is performed:
	//   internal     (default) — agent runs its own embedded torrent client.
	//                  Self-contained, requires no external infrastructure.
	//   watch_folder — agent drops the magnet/.torrent into WatchDir and waits
	//                  for a completed directory to appear under DoneDir/<info_hash>/.
	//                  Lets users plug in their own BT client (qBittorrent,
	//                  Transmission, etc.) running anywhere reachable as a
	//                  filesystem path (local volume, SMB/NFS mount, FTP via
	//                  curlftpfs, …).
	AgentMode string

	// WatchHandoffTimeoutMin caps how long watch_folder mode waits for the
	// user's BT client to finish before giving up and aborting the task back
	// to the pool. 6 hours (360 min) covers most slow trackers without
	// pinning a lock indefinitely on a stalled handoff.
	WatchHandoffTimeoutMin int

	// VPN
	VPNProvider     string
	VPNDownloadOnly bool   // route only torrent traffic through VPN (SOCKS5); uploads go direct
	VPNProxyAddr    string // SOCKS5 proxy address (e.g. "vpn:1080") when VPNDownloadOnly=true

	// NNTP
	NNTPServer      string
	NNTPSSL         bool
	NNTPConnections int
	NNTPUser        string
	NNTPPass        string
	NNTPGroup       string
	NNTPPoster      string
	NNTPFrom        string
	NNTPDomain      string

	// PAR2
	PAR2Redundancy int // recovery percentage (default 5)
	PAR2Threads    int // 0 = all cores (parpar default), >0 = limit threads
	PAR2Memory     int // MB; 0 = auto (parpar default), >0 = cap memory usage
	// PAR2Method pins parpar's GF16 kernel (e.g. "shuffle-avx2"). Empty = the
	// agent probes for the fastest kernel that actually runs on this CPU, which
	// is the right answer on hardware we don't control. Set only to override.
	PAR2Method string

	// Concurrency
	MaxConcurrentDownloads int // how many torrents to download in parallel (default 3)

	// Disk
	MaxDiskUsageGB float64 // max temp disk usage in GB (0 = no limit, uses all available)

	// CPU throttle
	CPUMaxPercent float64 // don't start new tasks above this CPU usage (default 85, 0 = disabled)

	// Slow download rejection
	SlowSpeedThresholdMBs float64 // MB/s below which download is "slow" (default 0.05)
	SlowSpeedTimeoutMins  int     // minutes of sustained slow speed before rejecting (default 10)

	// Branding
	GeneratorName string // NZB x-generator header (default "loon-agent")

	// Obfuscation & Encryption
	Obfuscate bool // rename files to random hex before upload (default false)
	Encrypt   bool // wrap files in password-protected 7z before upload

	// ── Offer feature (gated by OFFER_ENABLED) ──────────────────────
	// When disabled (default) the agent skips the offer-sync loop
	// entirely — no scraping, no /api/agent/offer/* polls. Existing
	// loon-agent users stay on the old path until they explicitly
	// turn this on. The token must ALSO carry the 'offer' scope (set
	// on the site at /account-settings); a bare OFFER_ENABLED=true
	// with no scope will get 403s from the site and the sync loop
	// will log + back off.
	// ── Collection mode ──────────────────────────────────────────
	// CollectionRoot is the on-disk root the Collection scanner walks
	// to find video / archive files the operator wants to enrich +
	// upload to usenet. Per-scan tick: walk the tree, batch enrichment
	// via the site's /api/agent/title-match-bulk, persist results to
	// data/collection.json. Empty disables the Collection tab's scan
	// action; the page still renders the explainer card.
	CollectionRoot string

	OfferEnabled bool
	// OfferConfigPath points to the JSON file declaring sources +
	// browser + cookies. Defaults to <CONFIG_DIR>/offer.json so a
	// docker volume mount lands it next to config.yml. JSON (not
	// YAML) because the agent's vendored deps don't include a YAML
	// parser; the file is small enough that the difference doesn't
	// matter to human editors.
	OfferConfigPath string
	// OfferSyncIntervalMin controls how often the agent re-walks its
	// sources to refresh registrations. Cheap when nothing changed
	// (heartbeat path), expensive on first run. Default 60.
	OfferSyncIntervalMin int
	// OfferRemoteFulfill allows fulfilling a request by DOWNLOADING from the
	// source tracker when the file is not already on disk. Off by default,
	// and deliberately so: it spends the operator's bandwidth and their
	// tracker ratio, which is not a decision an agent should make for them
	// by merely being upgraded.
	OfferRemoteFulfill bool
	// OfferRemoteMaxGB refuses a remote fulfillment whose torrent is larger
	// than this. The download must also fit the disk pre-flight, which is
	// enforced separately by the torrent path; this is the operator's
	// "never spend more than this on one request" ceiling. 0 = no ceiling.
	OfferRemoteMaxGB int
	// OfferRemoteTimeoutMin bounds one remote fulfillment end to end. A
	// download that cannot finish inside it is abandoned and the request is
	// failed back so another offerer can try — better than holding a claim
	// alive indefinitely against a dead swarm.
	OfferRemoteTimeoutMin int

	// Layered holds the yml/env/web tiers for settings that are tunable via
	// the site or local web UI. The fields above continue to be populated
	// from the *effective* tier at construction time so legacy readers see
	// the merged value, but runtime changes arriving from the site are
	// applied through Layered.ApplyWeb and then mirrored back via Refresh.
	Layered *Layered `json:"-"`
}

// ConfigYmlPath returns the path to the layered YAML file, honouring
// CONFIG_YML when set and otherwise sitting beside TempDir's parent.
func ConfigYmlPath() string {
	if p := os.Getenv("CONFIG_YML"); p != "" {
		return p
	}
	base := getEnv("CONFIG_DIR", ".")
	return filepath.Join(base, "config.yml")
}

func NewConfig() *Config {
	l := NewLayered(ConfigYmlPath())
	return newConfigFromLayered(l)
}

func newConfigFromLayered(l *Layered) *Config {
	layeredInt := func(key string, fallback int) int {
		if v := l.Effective(key); v != "" {
			if i, err := strconv.Atoi(v); err == nil {
				return i
			}
		}
		return fallback
	}
	layeredFloat := func(key string, fallback float64) float64 {
		if v := l.Effective(key); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
		return fallback
	}
	return &Config{
		Layered:                l,
		SiteURL:                getEnv("SITE_URL", ""),
		AgentToken:             getEnv("AGENT_TOKEN", ""),
		PollInterval:           getEnvAsInt("POLL_INTERVAL", 30),
		SiteName:               getEnv("SITE_NAME", ""),
		TempDir:                getEnv("TEMP_DIR", "./temp"),
		WatchDir:               getEnv("WATCH_DIR", "./watch"),
		DoneDir:                getEnv("DONE_DIR", "./done"),
		AgentMode:              getEnv("AGENT_MODE", "internal"),
		WatchHandoffTimeoutMin: getEnvAsInt("WATCH_HANDOFF_TIMEOUT_MIN", 360),
		VPNProvider:            getEnv("VPN_PROVIDER", "Unknown"),
		VPNDownloadOnly:        getEnv("VPN_DOWNLOAD_ONLY", "false") == "true",
		VPNProxyAddr:           getEnv("VPN_PROXY_ADDR", "vpn:1080"),
		NNTPServer:             getEnv("NNTP_SERVER", "news.example.com:119"),
		NNTPSSL:                getEnv("NNTP_SSL", "false") == "true",
		NNTPConnections:        getEnvAsInt("NNTP_CONNECTIONS", 10),
		NNTPUser:               getEnv("NNTP_USER", "username"),
		NNTPPass:               getEnv("NNTP_PASS", "password"),
		NNTPGroup:              getEnv("NNTP_GROUP", "alt.binaries.test"),
		NNTPPoster:             getEnv("NNTP_POSTER", "Pipeline_Uploader"),
		NNTPFrom:               getEnv("NNTP_FROM", "uploader@yourdomain.com"),
		NNTPDomain:             getEnv("NNTP_DOMAIN", ""),
		PAR2Redundancy:         getEnvAsInt("PAR2_REDUNDANCY", 5),
		PAR2Threads:            getEnvAsInt("PAR2_THREADS", 0),
		PAR2Memory:             getEnvAsInt("PAR2_MEMORY_MB", 0),
		PAR2Method:             getEnv("PAR2_METHOD", ""),
		MaxDiskUsageGB:         layeredFloat("max_disk_usage_gb", 0),
		MaxConcurrentDownloads: layeredInt("max_concurrent_downloads", 3),
		CPUMaxPercent:          layeredFloat("cpu_max_percent", 85),
		SlowSpeedThresholdMBs:  layeredFloat("slow_speed_threshold_mbs", 0.05),
		SlowSpeedTimeoutMins:   layeredInt("slow_speed_timeout_mins", 10),
		GeneratorName:          getEnv("GENERATOR_NAME", "loon-agent"),
		Obfuscate:              getEnv("OBFUSCATE", "false") == "true",
		Encrypt:                getEnv("ENCRYPT", "false") == "true",
		CollectionRoot:         getEnv("COLLECTION_ROOT", ""),
		OfferEnabled:           getEnv("OFFER_ENABLED", "false") == "true",
		OfferConfigPath:        getEnv("OFFER_CONFIG", filepath.Join(getEnv("CONFIG_DIR", "."), "offer.json")),
		OfferSyncIntervalMin:   getEnvAsInt("OFFER_SYNC_INTERVAL_MIN", 60),
		OfferRemoteFulfill:     getEnv("OFFER_REMOTE_FULFILL", "false") == "true",
		OfferRemoteMaxGB:       getEnvAsInt("OFFER_REMOTE_MAX_GB", 25),
		OfferRemoteTimeoutMin:  getEnvAsInt("OFFER_REMOTE_TIMEOUT_MIN", 240),
	}
}

// Refresh re-derives the layered fields on Config after the web tier has
// been replaced via Layered.ApplyWeb. Env-scoped fields are untouched.
func (c *Config) Refresh() {
	if c.Layered == nil {
		return
	}
	fresh := newConfigFromLayered(c.Layered)
	c.MaxDiskUsageGB = fresh.MaxDiskUsageGB
	c.MaxConcurrentDownloads = fresh.MaxConcurrentDownloads
	c.CPUMaxPercent = fresh.CPUMaxPercent
	c.SlowSpeedThresholdMBs = fresh.SlowSpeedThresholdMBs
	c.SlowSpeedTimeoutMins = fresh.SlowSpeedTimeoutMins
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func getEnvAsInt(key string, fallback int) int {
	if value, ok := os.LookupEnv(key); ok {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return fallback
}

func getEnvAsFloat(key string, fallback float64) float64 {
	if value, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return f
		}
	}
	return fallback
}

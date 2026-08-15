package services

// Offer-sources config loader. Reads <CONFIG_DIR>/offer.json (or
// $OFFER_CONFIG when set) and produces the typed source list the
// offer-sync service walks. JSON instead of YAML because the agent's
// vendored module doesn't include a YAML parser — stdlib encoding/json
// works without adding a dep, and the schema is small enough that JSON
// is fine to hand-edit.
//
// Schema:
//
//   {
//     "default_browser": "chrome",
//     "cookies_file": "/app/cookies.json",
//     "sources": [
//       {
//         "type": "folder",
//         "short_name": "personal",
//         "root": "/media/anime",
//         "extensions": [".mkv", ".mp4"],
//         "size_min_mb": 100
//       },
//       {
//         "type": "scraper",
//         "short_name": "ab",
//         "base_url": "https://example.com",
//         "browser": "firefox",
//         "categories": ["anime"]
//       }
//     ]
//   }
//
// Missing or absent file → empty config (no sources). The agent's
// offer-sync service logs once and stays idle in that case.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// OfferConfig is the top-level shape of offer.json.
type OfferConfig struct {
	DefaultBrowser string        `json:"default_browser"`
	CookiesFile    string        `json:"cookies_file"`
	Sources        []OfferSource `json:"sources"`
}

// OfferSource is one source declaration. Either `folder` or `scraper`.
type OfferSource struct {
	Type       string   `json:"type"`        // "folder" or "scraper"
	ShortName  string   `json:"short_name"`  // matches site tracker.short_name
	Root       string   `json:"root"`        // folder mode
	Extensions []string `json:"extensions"`  // folder mode allowlist (".mkv", ".mp4")
	SizeMinMB  int      `json:"size_min_mb"` // folder mode min size

	BaseURL    string   `json:"base_url"`   // scraper mode
	Username   string   `json:"username"`   // scraper mode — tracker username (Nyaa, AB, …)
	Browser    string   `json:"browser"`    // scraper mode per-source override
	Categories []string `json:"categories"` // scraper mode

	// DownloadsRoot pairs with scraper sources: it points at the
	// local folder where this tracker's downloads land (qBittorrent
	// save dir, watch_folder DoneDir, etc.). The sync orchestrator
	// matches scraped release titles against the files here so the
	// hash→path cache is populated for fulfillment. Without this
	// the scraper still registers offers but the agent can't deliver
	// them — they sit as "anonymous offers" until a fulfillment path
	// other than NZB upload lands.
	DownloadsRoot string `json:"downloads_root"`

	// DeliveryModes is the agent's per-source override list. Empty
	// falls back to the natural default for the source type:
	//   folder  → ['nzb']     (handed to Usenet pipeline)
	//   scraper → ['torrent'] (re-uploaded to ameNZB)
	// Multiple values declare the agent CAN deliver either way and
	// lets the requester pick at file time.
	DeliveryModes []string `json:"delivery_modes"`
}

// LoadOfferConfig reads + parses the offer config file. Missing file
// is not an error — returns an empty config so the caller can treat
// "no offers declared yet" uniformly. Other I/O / parse errors bubble.
// LoadOfferConfigQuiet is LoadOfferConfig for callers that only want the file
// as a HINT rather than as their configuration.
//
// The inventory service reads it to discover folder roots when INVENTORY_ROOTS
// is unset. A malformed offer.json is a fatal error for offer-sync, which is
// governed by it — but it must not stop inventory reporting, which merely
// looked. Returns an empty config rather than nil so callers need no nil check.
func LoadOfferConfigQuiet(path string) *OfferConfig {
	cfg, err := LoadOfferConfig(path)
	if err != nil || cfg == nil {
		return &OfferConfig{}
	}
	return cfg
}

func LoadOfferConfig(path string) (*OfferConfig, error) {
	if path == "" {
		return &OfferConfig{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &OfferConfig{}, nil
		}
		return nil, fmt.Errorf("offer config %s: %w", path, err)
	}
	var cfg OfferConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("offer config %s parse: %w", path, err)
	}
	if cfg.DefaultBrowser == "" {
		cfg.DefaultBrowser = BrowserChrome
	}
	return &cfg, nil
}

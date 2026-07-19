package client

import (
	"time"

	"github.com/the-loon-clan/loon-agent/config"
)

// Site is the contract the agent expects from whatever indexer it
// talks to. The concrete *SiteClient in this package implements it
// against the indexer-site HTTP API; downstream forks can provide
// their own implementation (for a Sonarr-style upload bridge, an
// internal staging server, a mock used in tests, etc.) without
// touching the rest of the agent. Every code path in services/ and
// main.go takes Site rather than *SiteClient so this swap is a
// constructor change, not a rewrite.
//
// Method ordering mirrors the agent's task lifecycle: connection
// health, then task dispatch, then resource fetch, then progress
// reporting, then completion / backfill, then config + directives.
type Site interface {
	// Connection health — read-only timestamp set by the HTTP
	// transport on every successful roundtrip. The watchdog calls
	// this to spot extended unreachability (VPN down, DNS broken).
	LastOK() time.Time

	// Task dispatch + resource fetch.
	Poll() (*PollResult, error)
	FetchCachedTorrentByInfoHash(infoHash string) ([]byte, error)
	FetchTorrentFile(urlPath string) ([]byte, error)

	// Per-task progress + completion. ReportProgress is called many
	// times during a download/upload; Complete is the terminal call
	// that promotes / fails / aborts the request.
	ReportProgress(lockID int, progress, speed string, warnings []LockWarning) error
	Complete(result CompleteResult) error

	// Backfill is a separate-path upload for content the agent
	// already produced via offline / watch-folder modes. Distinct
	// from Complete so the site can branch on origin.
	Backfill(requestID int64, nzbData []byte, password string) (int64, error)

	// UploadSubtitle sends one extracted subtitle track produced
	// post-download by services.ExtractSubtitles. Called once per
	// track after the NZB has been promoted (so nzb_id exists).
	// Idempotent — the site upserts on (nzb_id, track_index).
	UploadSubtitle(s SubtitleUpload) error

	// Agent state + admin signals. PostStatus reports live phase /
	// throughput for the dashboard; PostLog writes a single line to
	// agent_logs; ClearMyLocks releases any active locks left over
	// from a crash on previous run.
	PostStatus(status AgentLiveStatus) (*StatusResponse, error)
	PostLog(level, message string) error
	ClearMyLocks() (int, error)

	// Config sync — site → agent.
	GetConfig() (*RemoteConfig, error)
	// Config push — agent → site. PostLocalConfig sends the agent's
	// local-tier snapshot so the settings UI can show state badges;
	// PutWebConfig writes a single key into the web-override tier.
	PostLocalConfig(report config.SettingsReport) error
	PutWebConfig(key, value string) error

	// Posting-group sync. FetchAgentGroups returns rows newer than
	// sinceVersion so steady-state polls return a few bytes.
	FetchAgentGroups(sinceVersion int) (*AgentGroupsResponse, error)

	// Out-of-band operator directives queued from the admin UI
	// (e.g. "reload config", "stop now"). FetchDirectives reads
	// the queue; AckDirective marks one as done so it doesn't
	// fire repeatedly.
	FetchDirectives() ([]Directive, error)
	AckDirective(id int64, errMsg string) error

	// Collection mode — bulk title-match lookup used by the
	// scanner to enrich a batch of filenames in one round-trip.
	// See client/title_match.go and services/collection_scanner.go.
	TitleMatchBulk(titles []string) ([]TitleMatchResult, error)
}

// Compile-time check that *SiteClient still satisfies the interface.
// If this fails to compile, the concrete type has drifted and every
// caller using Site will break — catch it here instead of at the
// first test that mocks Site.
var _ Site = (*SiteClient)(nil)

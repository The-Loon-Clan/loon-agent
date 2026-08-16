package services

import (
	"sync"
	"time"
)

// LiveSnapshot is the low-cost, numerical view of current agent activity
// that the local UI streams over SSE. Intentionally smaller and more
// typed than client.AgentLiveStatus (which is shaped for the site's
// consumption): no per-file list, speeds as float MB/s not strings, and
// only the fields a sparkline / sidebar actually needs.
//
// Producer: main.go's startStatusReporter publishes a new snapshot after
// each aggregateLiveStatus tick. Consumer: the /events SSE handler reads
// the latest value on its own ~1.5s cadence; there's no coupling between
// the producer and consumer rates.
type LiveSnapshot struct {
	Phase          string    `json:"phase"`                // idle | downloading | uploading | screenshots | par2 | ...
	TaskTitle      string    `json:"task_title,omitempty"` // current task if any
	DownloadMBps   float64   `json:"download_mbps"`        // aggregate across active tasks
	UploadMBps     float64   `json:"upload_mbps"`          // total upload — kept for older clients
	NzbUploadMBps  float64   `json:"nzb_upload_mbps"`      // NNTP POST traffic only
	SeedUploadMBps float64   `json:"seed_upload_mbps"`     // BitTorrent seed-back traffic only
	VPNStatus      string    `json:"vpn_status"`
	PublicIP       string    `json:"public_ip"`
	DiskFreeGB     float64   `json:"disk_free_gb"`
	DiskReservedGB float64   `json:"disk_reserved_gb"`
	DiskTotalGB    float64   `json:"disk_total_gb"` // 0 on stub platforms; sidebar hides the bar when 0
	SeedingCount   int       `json:"seeding_count"` // tasks still in their seed-back phase, holding reservations
	UpdatedAt      time.Time `json:"updated_at"`
}

var (
	liveSnapshotMu sync.RWMutex
	liveSnapshot   LiveSnapshot
)

// SetLiveSnapshot publishes a new snapshot. Called from the main-package
// status reporter after it's aggregated per-task progress into totals.
// Taking a copy under the write lock avoids exposing the caller's struct
// literal to a concurrent reader mid-mutation.
func SetLiveSnapshot(s LiveSnapshot) {
	s.UpdatedAt = time.Now()
	liveSnapshotMu.Lock()
	liveSnapshot = s
	liveSnapshotMu.Unlock()
}

// GetLiveSnapshot returns a value copy of the current snapshot so callers
// (the SSE handler) can serialize without holding the lock.
func GetLiveSnapshot() LiveSnapshot {
	liveSnapshotMu.RLock()
	defer liveSnapshotMu.RUnlock()
	return liveSnapshot
}

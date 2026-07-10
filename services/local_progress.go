package services

import "sync"

// Per-task progress callbacks keyed by jobName (e.g. "request-123").
// Set by main.go before starting download/upload, read by torrent/usenet workers.
var (
	progressMu  sync.RWMutex
	progressCbs = map[string]ProgressCallback{}
)

// SetProgressCallbackForJob registers a callback for a specific job.
func SetProgressCallbackForJob(jobName string, cb ProgressCallback) {
	progressMu.Lock()
	progressCbs[jobName] = cb
	progressMu.Unlock()
}

// ClearProgressCallbackForJob removes the callback for a job.
func ClearProgressCallbackForJob(jobName string) {
	progressMu.Lock()
	delete(progressCbs, jobName)
	progressMu.Unlock()
}

// GetProgressCallback returns the callback for a specific job (or nil).
func GetProgressCallback(jobName string) ProgressCallback {
	progressMu.RLock()
	defer progressMu.RUnlock()
	return progressCbs[jobName]
}

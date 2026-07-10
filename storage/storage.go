package storage

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type JobState struct {
	Name           string  `json:"name"`
	// Title is the human-readable release name (e.g. the AGENT_TASK
	// title the site sent on Poll). Distinct from Name (which is the
	// `request-NNNN` lock identifier) so the local UI can render
	// something legible without throwing away the lock id used as a
	// primary key in JobCancels / the cancel API. Populated once at
	// processTask entry via SetJobTitle; empty entries fall back to
	// Name in the template.
	Title          string  `json:"title,omitempty"`
	Phase          string  `json:"phase"`
	Details        string  `json:"details"`
	Progress       float64 `json:"progress"`
	UpdatedAt      string  `json:"updated_at"`
	DownloadedPath string  `json:"downloaded_path,omitempty"`
	// StagePath is the absolute path to this task's stage-XXX upload-staging
	// directory while it exists. It's published here so the orphan sweep can
	// keep the dir in its keep-set; without this, a task that sits long
	// enough waiting for an NNTP slot can have its prepared stage wiped out
	// from under it and then post zero articles to "Complete".
	StagePath string `json:"stage_path,omitempty"`
	Password  string `json:"password,omitempty"`
	ZipName   string `json:"zip_name,omitempty"`
}

type AppState struct {
	sync.RWMutex
	PublicIP    string               `json:"public_ip"`
	VPNStatus   string               `json:"vpn_status"`
	VPNProvider string               `json:"vpn_provider"`
	WatchFiles  []string             `json:"watch_files"`
	Jobs        map[string]*JobState `json:"jobs"`
	NNTPDomain  string               `json:"nntp_domain"`
	NNTPPoster  string               `json:"nntp_poster"`
	QueuePaused bool                 `json:"queue_paused"`
}

var GlobalState = AppState{Jobs: make(map[string]*JobState)}
var JobCancels sync.Map

var StateFile string

func SaveState() {
	if StateFile == "" {
		return
	}
	GlobalState.RLock()
	data, err := json.MarshalIndent(GlobalState.Jobs, "", "  ")
	GlobalState.RUnlock()
	if err == nil {
		os.WriteFile(StateFile, data, 0644)
	}
}

func LoadState() {
	if StateFile == "" {
		return
	}
	data, err := os.ReadFile(StateFile)
	if err == nil {
		var jobs map[string]*JobState
		if err := json.Unmarshal(data, &jobs); err == nil {
			GlobalState.Lock()
			GlobalState.Jobs = jobs
			GlobalState.Unlock()
		}
	}
}

func UpdateJobMeta(name, path, password, zipName string) {
	GlobalState.Lock()
	if job, exists := GlobalState.Jobs[name]; exists {
		if path != "" {
			job.DownloadedPath = path
		}
		if password != "" {
			job.Password = password
		}
		if zipName != "" {
			job.ZipName = zipName
		}
	}
	GlobalState.Unlock()
	SaveState()
}

// SetJobTitle stamps the human-readable release title on the
// (already-created) job state. Called once from processTask after
// the first UpdateState seeds the entry. Mirrors SetJobStagePath's
// pattern — write through the lock + persist.
func SetJobTitle(name, title string) {
	if name == "" || title == "" {
		return
	}
	GlobalState.Lock()
	if job, exists := GlobalState.Jobs[name]; exists {
		job.Title = title
	}
	GlobalState.Unlock()
	SaveState()
}

// SetJobStagePath publishes (or clears, when empty) the upload-staging dir
// for a task so the orphan sweep won't wipe it while the task waits for
// the NNTP slot. Called once after stage-XXX is created, and again with ""
// after the deferred os.RemoveAll fires.
func SetJobStagePath(name, stagePath string) {
	GlobalState.Lock()
	if job, exists := GlobalState.Jobs[name]; exists {
		job.StagePath = stagePath
	}
	GlobalState.Unlock()
	SaveState()
}

// UpdateState safely modifies the tracked status of a pipeline job
func UpdateState(name, phase, details string, progress float64) {
	GlobalState.Lock()

	if _, exists := GlobalState.Jobs[name]; !exists {
		GlobalState.Jobs[name] = &JobState{Name: name}
	}

	GlobalState.Jobs[name].Phase = phase
	GlobalState.Jobs[name].Details = details
	GlobalState.Jobs[name].Progress = progress
	GlobalState.Jobs[name].UpdatedAt = time.Now().Format("15:04:05")
	GlobalState.Unlock()
	SaveState()
}

// RemoveJob drops the job entry from GlobalState.Jobs and persists.
// Called from the agent's task end-of-life paths (success, fail,
// abort) so the orphan-sweep's keep[] doesn't protect its working
// dirs forever. Without this, a failed job leaves an entry in
// GlobalState pointing at DownloadedPath; the sweep's keep set then
// shields any leftover files in that path indefinitely — exactly
// the "files accumulate over time" leak the operator was seeing.
//
// Safe to call for a name that doesn't exist: the delete is a no-op.
// SaveState always runs so disk reflects the in-memory state and
// future restarts don't re-hydrate the removed entry.
func RemoveJob(name string) {
	if name == "" {
		return
	}
	GlobalState.Lock()
	delete(GlobalState.Jobs, name)
	GlobalState.Unlock()
	SaveState()
}

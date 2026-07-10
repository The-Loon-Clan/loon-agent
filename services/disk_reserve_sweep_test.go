package services

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ameNZB/usenet-pipeline/storage"
)

// resetGlobalState wipes the package-global GlobalState.Jobs map so
// each test starts from a known-empty state. Cannot use t.Cleanup
// alone because the map is shared across all tests in this package.
func resetGlobalState(t *testing.T) {
	t.Helper()
	storage.GlobalState.Lock()
	storage.GlobalState.Jobs = map[string]*storage.JobState{}
	storage.GlobalState.Unlock()
}

// TestSweep_ProtectsActiveDownload_WithoutDownloadedPath locks in the
// 1.5.24 fix: a job that is in GlobalState but has NOT yet stamped
// DownloadedPath (i.e. the active-download window between
// UpdateState("claimed") and the post-download UpdateJobMeta) must
// have its dl-<jobName> directory protected from the orphan sweep.
//
// Pre-1.5.24, the keep-set was sourced solely from DownloadedPath via
// a Base(Dir(...)) walk. During the download window DownloadedPath
// was "" and the keep-set was empty, so the post-task minAge=0 sweep
// at cmd/agent/main.go:1110 deleted the active dl-XXX directory of
// every concurrently-downloading task whenever ANY other task
// completed. Production symptom (2026-06-04, request-21856): 1m21s
// download reached 100% bytes with active peers, then WaitAll
// returned to a missing data directory.
func TestSweep_ProtectsActiveDownload_WithoutDownloadedPath(t *testing.T) {
	resetGlobalState(t)
	tempDir := t.TempDir()
	jobName := "request-21856"
	dlDir := filepath.Join(tempDir, "dl-"+jobName)
	if err := os.MkdirAll(dlDir, 0755); err != nil {
		t.Fatalf("setup dl dir: %v", err)
	}

	// Simulate UpdateState("claimed") on a new task — JobState exists
	// but DownloadedPath is the empty string because the download
	// hasn't completed yet (the realistic state of a task that is
	// actively running anacrolix's DownloadAll).
	storage.GlobalState.Lock()
	storage.GlobalState.Jobs[jobName] = &storage.JobState{
		Name:           jobName,
		Phase:          "Downloading",
		DownloadedPath: "", // <- the critical condition the old code missed
	}
	storage.GlobalState.Unlock()

	// minAge=0 mirrors the per-task-completion sweep at
	// cmd/agent/main.go:1110 — the most dangerous variant.
	sweepWithMinAge(tempDir, 0, false)

	if _, err := os.Stat(dlDir); os.IsNotExist(err) {
		t.Fatalf("active download dir %s was swept despite JobState entry — 1.5.24 fix regressed", dlDir)
	} else if err != nil {
		t.Fatalf("stat dl dir: %v", err)
	}
}

// TestSweep_RemovesTrueOrphan confirms the sweep still removes
// dl-XXX directories that have NO corresponding JobState entry —
// the legitimate orphan case from a crashed prior agent process.
// Without this, the 1.5.24 fix could over-protect and leak disk.
func TestSweep_RemovesTrueOrphan(t *testing.T) {
	resetGlobalState(t)
	tempDir := t.TempDir()
	orphan := filepath.Join(tempDir, "dl-request-99999")
	if err := os.MkdirAll(orphan, 0755); err != nil {
		t.Fatalf("setup orphan: %v", err)
	}

	// GlobalState empty — no in-flight task owns this dir.
	sweepWithMinAge(tempDir, 0, false)

	if _, err := os.Stat(orphan); err == nil {
		t.Fatalf("true orphan %s survived sweep — keep-set is over-protecting", orphan)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat orphan: %v", err)
	}
}

// TestSweep_PostTaskCompletion_ProtectsSiblings reproduces the exact
// production race: Task A finishes (storage.RemoveJob then
// SweepOrphanDownloads with minAge=0) while Task B is mid-download
// with DownloadedPath="". Task B's dl- directory MUST survive.
//
// This is the failure mode witnessed on request-21856 — task 19353
// (a small audio task) completed during request-21856's 1m21s
// download window, and 19353's post-task minAge=0 sweep deleted
// dl-request-21856. Pre-1.5.24 this test fails (dir gets wiped);
// post-1.5.24 it passes.
func TestSweep_PostTaskCompletion_ProtectsSiblings(t *testing.T) {
	resetGlobalState(t)
	tempDir := t.TempDir()

	// Task A — just finished, RemoveJob has already been called so
	// it is NOT in GlobalState. Its dl- dir is already gone (the
	// deferred RemoveAll fired on processTask return).
	// (Nothing to set up — the absence is the simulation.)

	// Task B — mid-download. JobState exists, DownloadedPath="".
	taskB := "request-21856"
	dlB := filepath.Join(tempDir, "dl-"+taskB)
	if err := os.MkdirAll(dlB, 0755); err != nil {
		t.Fatalf("setup taskB dl dir: %v", err)
	}
	storage.GlobalState.Lock()
	storage.GlobalState.Jobs[taskB] = &storage.JobState{
		Name:  taskB,
		Phase: "Downloading",
	}
	storage.GlobalState.Unlock()

	// Task A's post-task sweep.
	sweepWithMinAge(tempDir, 0, false)

	if _, err := os.Stat(dlB); os.IsNotExist(err) {
		t.Fatalf("Task B's active dl- dir was wiped by Task A's post-task sweep — race regressed")
	} else if err != nil {
		t.Fatalf("stat taskB dl dir: %v", err)
	}
}

// TestSweep_ProtectsViaJobNameKey_NotJustStateName covers the
// belt-and-braces line `keep["dl-"+job.Name]` in 1.5.24. The map key
// and JobState.Name should normally agree (both are jobName), but if
// they ever diverge — e.g. a future bug where someone mutates one
// without the other — the keep-set should protect both names so a
// rename-mid-flight can't leak the in-flight dir to the sweeper.
func TestSweep_ProtectsViaJobNameKey_NotJustStateName(t *testing.T) {
	resetGlobalState(t)
	tempDir := t.TempDir()

	mapKey := "request-A"
	stateName := "request-B"
	dlA := filepath.Join(tempDir, "dl-"+mapKey)
	dlB := filepath.Join(tempDir, "dl-"+stateName)
	for _, p := range []string{dlA, dlB} {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatalf("setup %s: %v", p, err)
		}
	}

	storage.GlobalState.Lock()
	storage.GlobalState.Jobs[mapKey] = &storage.JobState{Name: stateName}
	storage.GlobalState.Unlock()

	sweepWithMinAge(tempDir, 0, false)

	if _, err := os.Stat(dlA); os.IsNotExist(err) {
		t.Fatalf("dl-<mapKey> swept — map-key protection regressed")
	}
	if _, err := os.Stat(dlB); os.IsNotExist(err) {
		t.Fatalf("dl-<job.Name> swept — Name-field belt-and-braces regressed")
	}
}

// TestSweep_ExtendedFamilies_BootOnly locks in the boot-sweep coverage
// fix: the offline processor + offer fulfiller create temp families
// (offline-*, enc-, wrap-, offer-, bare *.7z) whose prefixes the periodic
// sweep intentionally ignores — so a force-kill orphan of one used to leak
// forever. SweepOrphanTempStartup (boot only, before those goroutines run)
// must reclaim them, while the conservative variant must leave them alone.
func TestSweep_ExtendedFamilies_BootOnly(t *testing.T) {
	resetGlobalState(t)

	extended := []string{
		"offline-stage-abc123",
		"offline-enc-abc123",
		"offline-shots-abc123",
		"enc-deadbeef",
		"wrap-deadbeef",
		"offer-42-deadbeef",
	}
	bareArchive := "0123456789abcdef.7z"

	makeAll := func(dir string) {
		t.Helper()
		for _, name := range extended {
			if err := os.MkdirAll(filepath.Join(dir, name), 0755); err != nil {
				t.Fatalf("setup %s: %v", name, err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, bareArchive), []byte("x"), 0644); err != nil {
			t.Fatalf("setup %s: %v", bareArchive, err)
		}
	}

	// Conservative sweep (post-task / hourly): must NOT touch these — the
	// offline/offer jobs that own them aren't in GlobalState, so deleting
	// one could rip out an in-flight pipeline's working dir.
	conservative := t.TempDir()
	makeAll(conservative)
	sweepWithMinAge(conservative, 0, false)
	for _, name := range append(extended, bareArchive) {
		if _, err := os.Stat(filepath.Join(conservative, name)); os.IsNotExist(err) {
			t.Fatalf("conservative sweep removed %s — must be boot-only", name)
		}
	}

	// Boot sweep: reclaims every extended family (guaranteed-dead orphans
	// because it runs before the owning goroutines launch).
	boot := t.TempDir()
	makeAll(boot)
	SweepOrphanTempStartup(boot)
	for _, name := range append(extended, bareArchive) {
		if _, err := os.Stat(filepath.Join(boot, name)); err == nil {
			t.Fatalf("boot sweep left %s behind — extended coverage regressed", name)
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", name, err)
		}
	}
}

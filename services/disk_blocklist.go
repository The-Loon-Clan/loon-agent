package services

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
)

// remoteBannedExtensions holds the operator-configured blocklist
// pushed down from the site via /api/agent/config. atomic.Pointer so
// the poll loop can update it without blocking the task processor
// that reads it post-download. Nil = no override (caller falls back
// to DefaultBlockedExtensions). Set once per successful GetConfig.
var remoteBannedExtensions atomic.Pointer[[]string]

// SetRemoteBannedExtensions publishes a new operator override for the
// post-download blocklist sweep. Called from the main poll loop after
// each successful GetConfig roundtrip. Empty slice means "operator
// explicitly set an empty list" — and is preserved as such; only nil
// triggers the DefaultBlockedExtensions fallback in
// OnlineBlocklist below.
func SetRemoteBannedExtensions(exts []string) {
	cp := make([]string, len(exts))
	copy(cp, exts)
	remoteBannedExtensions.Store(&cp)
}

// OnlineBlocklist returns the effective blocklist for the online
// (site-polling) download path.
//
// Architecture (as of agent v1.5.2): the site is the source of truth.
// /api/agent/config returns the operator-configured list when set, or
// the system defaults (services.DefaultAgentBannedExtensions on the
// site side) otherwise. The agent applies what arrives verbatim.
//
// DefaultBlockedExtensions on this side is now a cold-start safety
// net only — it fires when the agent has never had a successful
// GetConfig roundtrip (process just started, network down, etc.).
// In steady-state operation the agent should always have a non-empty
// remoteBannedExtensions pointer and this fallback never executes.
// Edits to the operator-facing list must happen on the site UI at
// /account-settings/agent/<id>; edits to the local agent_blocklist
// list will only affect offline / watch-folder jobs (see
// EffectiveBlocklist below).
func OnlineBlocklist() map[string]bool {
	if p := remoteBannedExtensions.Load(); p != nil && len(*p) > 0 {
		return EffectiveBlocklist(*p)
	}
	return DefaultBlockedExtensions
}

// DirHasUsableFiles reports whether dir contains at least one regular
// non-empty file (recursively). Used by the online task path right
// after RemoveBlockedFiles to detect "the blocklist stripped
// everything" — usually a DVD_ISO or similar all-blocked release —
// so we can abort cleanly with a clear reason instead of letting the
// pipeline produce an empty stage dir and a confusing "no files to
// upload" error from the NNTP uploader four steps later.
//
// Short-circuits on the first hit via a sentinel error so a release
// with thousands of files doesn't pay a full walk just to learn the
// answer is yes.
//
// When it returns false, logs a one-line inventory of what the walk
// actually saw (dirs, regular files, size-0 files, errors, first few
// entry names) so the operator can tell empty-dir / wrong-dir /
// CJK-walker-glitch / silent-permission-error apart from one line of
// agent log. Failure path was previously silent — the abort surface
// only showed the blocklist result, not whether the dir held anything
// the walker could see.
func DirHasUsableFiles(dir string) bool {
	found := false
	stop := fmt.Errorf("found")
	var (
		entries, dirs, files, zeroSize, walkErrors int
		firstWalkErr                               error
		sampleNames                                []string
	)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		entries++
		if err != nil {
			walkErrors++
			if firstWalkErr == nil {
				firstWalkErr = err
			}
			return nil
		}
		if info == nil {
			return nil
		}
		if len(sampleNames) < 5 && path != dir {
			sampleNames = append(sampleNames, filepath.Base(path))
		}
		if info.IsDir() {
			dirs++
			return nil
		}
		if info.Size() == 0 {
			zeroSize++
			return nil
		}
		files++
		found = true
		return stop
	})
	if err != nil && err != stop {
		// Unexpected walk error — treat as "we can't tell, assume yes"
		// rather than aborting a task on a transient EACCES on one file.
		log.Printf("DirHasUsableFiles: walk %q failed (%v); failing open to true", dir, err)
		return true
	}
	if !found {
		log.Printf("DirHasUsableFiles: %q reported NO usable files — entries=%d dirs=%d files=%d zero-size=%d walk-errs=%d firstErr=%v samples=%q",
			dir, entries, dirs, files, zeroSize, walkErrors, firstWalkErr, sampleNames)
	}
	return found
}

// OnlineBlocklistSource reports a short, human-readable description of
// which list OnlineBlocklist() is currently returning ("site override
// (N ext)" or "hardcoded defaults (N ext)"). Used in agent task logs
// so an operator can tell at a glance whether their /agent/<id>
// banned_extensions field actually took effect.
func OnlineBlocklistSource() string {
	if p := remoteBannedExtensions.Load(); p != nil && len(*p) > 0 {
		return fmt.Sprintf("site override (%d ext)", len(*p))
	}
	return fmt.Sprintf("hardcoded defaults (%d ext)", len(DefaultBlockedExtensions))
}

// DefaultBlockedExtensions is the fallback blocklist applied when a group
// doesn't specify its own. Lifted verbatim out of main.go's hardcoded map
// so both the online (site-polling) and offline (watch-folder) paths can
// share it, and so per-group overrides can replace it entirely when the
// group's content makes the default wrong (e.g. a music group that
// legitimately ships .iso alongside audio).
//
// The list is the Microsoft "high-risk file types" set plus a handful of
// common scripting / executable formats. Extensions are stored with the
// leading dot and in lowercase; RemoveBlockedFiles lowercases the runtime
// extension before the lookup.
var DefaultBlockedExtensions = map[string]bool{
	".ade": true, ".adp": true, ".app": true, ".application": true, ".appref-ms": true,
	".asp": true, ".aspx": true, ".asx": true, ".bas": true, ".bat": true, ".bgi": true,
	".cab": true, ".cer": true, ".chm": true, ".cmd": true, ".cnt": true, ".com": true,
	".cpl": true, ".crt": true, ".csh": true, ".der": true, ".diagcab": true, ".exe": true,
	".fxp": true, ".gadget": true, ".grp": true, ".hlp": true, ".hpj": true, ".hta": true,
	".htc": true, ".inf": true, ".ins": true, ".iso": true, ".isp": true, ".its": true,
	".jar": true, ".jnlp": true, ".js": true, ".jse": true, ".ksh": true, ".lnk": true,
	".mad": true, ".maf": true, ".mag": true, ".mam": true, ".maq": true, ".mar": true,
	".mas": true, ".mat": true, ".mau": true, ".mav": true, ".maw": true, ".mcf": true,
	".mda": true, ".mdb": true, ".mde": true, ".mdt": true, ".mdw": true, ".mdz": true,
	".msc": true, ".msh": true, ".msh1": true, ".msh2": true, ".mshxml": true,
	".msh1xml": true, ".msh2xml": true, ".msi": true, ".msp": true, ".mst": true,
	".msu": true, ".ops": true, ".osd": true, ".pcd": true, ".pif": true, ".pl": true,
	".plg": true, ".prf": true, ".prg": true, ".printerexport": true, ".ps1": true,
	".ps1xml": true, ".ps2": true, ".ps2xml": true, ".psc1": true, ".psc2": true,
	".psd1": true, ".psdm1": true, ".pst": true, ".py": true, ".pyc": true, ".pyo": true,
	".pyw": true, ".pyz": true, ".pyzw": true, ".reg": true, ".scf": true, ".scr": true,
	".sct": true, ".shb": true, ".shs": true, ".sln": true, ".theme": true, ".tmp": true,
	".url": true, ".vb": true, ".vbe": true, ".vbp": true, ".vbs": true, ".vcxproj": true,
	".vhd": true, ".vhdx": true, ".vsmacros": true, ".vsw": true, ".webpnp": true,
	".website": true, ".ws": true, ".wsc": true, ".wsf": true, ".wsh": true, ".xbap": true,
	".xll": true, ".xnk": true,
}

// EffectiveBlocklist chooses the blocklist a pipeline invocation should
// enforce: if the group provided an explicit list, that replaces the
// default outright; otherwise we fall back to DefaultBlockedExtensions.
// Passing `nil` or an empty slice for groupList means "use default," so
// callers outside the offline path (the online site-polling pipeline,
// one-off tools) get safe behaviour by default.
//
// The returned map is fresh on every call when a group list was given —
// mutating it in the caller won't leak into the default or other groups.
func EffectiveBlocklist(groupList []string) map[string]bool {
	if len(groupList) == 0 {
		return DefaultBlockedExtensions
	}
	m := make(map[string]bool, len(groupList))
	for _, ext := range groupList {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		m[ext] = true
	}
	return m
}

// RemoveBlockedFiles walks dir and deletes any file whose extension is
// in blocklist. Returns the count of files removed and a per-extension
// breakdown (e.g. {".iso": 3, ".bat": 1}) so callers can report which
// extensions were stripped — the abort path on /agent task processing
// uses this to print a concrete reason instead of "(e.g. .iso)".
//
// Errors while walking are logged but don't abort the pass — the callers
// treat blocklist enforcement as best-effort; the worst case is that a
// single risky file slips through to the uploader, which is still a
// staging step.
func RemoveBlockedFiles(dir string, blocklist map[string]bool) (int, map[string]int) {
	if blocklist == nil {
		blocklist = DefaultBlockedExtensions
	}
	removed := 0
	byExt := map[string]int{}
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return err
		}
		ext := strings.ToLower(filepath.Ext(info.Name()))
		if blocklist[ext] {
			log.Printf("Removing blocked file: %s", info.Name())
			_ = os.Remove(path)
			removed++
			byExt[ext]++
		}
		return nil
	})
	return removed, byExt
}

// ExpectedAfterBlocklist filters a torrent's declared file list down to the
// files that should still exist on disk once RemoveBlockedFiles has run,
// returning the survivors and the count excluded.
//
// It lives beside RemoveBlockedFiles deliberately: the two are halves of one
// fact — what we delete, and what we therefore must not expect. They were
// apart, and drifted. The online task path swept the blocklist at Step 3 and
// then ran a per-file pre-stage check at Step 4 that demanded every declared
// file, so any torrent shipping a .bat/.exe/.iso failed for doing exactly what
// it was told to: "torrent declared 24 file(s), 2 missing" naming the two .bat
// files the agent had just deleted itself. The error even blamed a
// disk_reserve_sweep race and a partial download. The byte-total check nearby
// already allowed for the sweep (loosened to 80% for this exact reason); the
// per-file check simply never learned.
func ExpectedAfterBlocklist(files []ExpectedFile, blocked map[string]bool) (keep []ExpectedFile, excluded int) {
	if blocked == nil {
		blocked = DefaultBlockedExtensions
	}
	keep = make([]ExpectedFile, 0, len(files))
	for _, f := range files {
		if blocked[strings.ToLower(filepath.Ext(f.Path))] {
			excluded++
			continue
		}
		keep = append(keep, f)
	}
	return keep, excluded
}

// FormatExtCounts renders a per-extension count map as a short,
// human-readable string sorted by count desc, ext asc — e.g.
// `.iso×3, .bat×1`. Returns "" for an empty/nil map so callers can
// concatenate without conditional plumbing. Used in agent logs and
// the abort reason on the online task path.
func FormatExtCounts(byExt map[string]int) string {
	if len(byExt) == 0 {
		return ""
	}
	type kv struct {
		ext   string
		count int
	}
	pairs := make([]kv, 0, len(byExt))
	for k, v := range byExt {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count != pairs[j].count {
			return pairs[i].count > pairs[j].count
		}
		return pairs[i].ext < pairs[j].ext
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = fmt.Sprintf("%s×%d", p.ext, p.count)
	}
	return strings.Join(parts, ", ")
}

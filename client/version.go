package client

// AgentProtocolVersion is an integer that identifies the API contract this
// agent binary speaks with the site. Bump it any time a breaking change is
// made to what the agent sends or expects in return (request shape, required
// fields, status values, etc). The site advertises its own minimum required
// version; anything below that gets 426 Upgrade Required and is expected to
// pause until the operator updates the binary.
//
// Changelog:
//
//	1 — initial versioned protocol. Adds the "aborted" completion status
//	    (agent-local errors release the lock without marking failed), and
//	    X-Agent-Protocol / X-Agent-Version headers on every request.
//	2 — private torrent uploads. Task payload carries Private + TorrentFileURL
//	    fields; when set, the agent MUST fetch the .torrent from the site
//	    over HTTPS and MUST NOT resolve the info hash via DHT (which would
//	    leak the release off the user's private tracker). Minimum is
//	    bumped to v2 so a pre-v2 agent can't silently do the unsafe thing
//	    on a private task.
//	3 — per-release data extraction wave. Adds five additive optional
//	    fields to the multipart /api/agent/complete payload:
//	      audio_tracks         JSON array — language/codec/channels/etc per track
//	      audio_fingerprints   JSON array — Chromaprint base32 per video file
//	      dominant_palette     JSON array — top-N hex colours from screenshots
//	      ocr_text             string     — tesseract output for manga releases
//	      ocr_language         string     — the tesseract -l value used
//	    Adds /api/agent/subtitle (one POST per extracted track, idempotent
//	    on (nzb_id, track_index)) and the convert_h264 / convert_h265 /
//	    convert_av1 values for the per-request remux_option. Convert
//	    dispatch is gated on a new per-agent convert_video flag returned
//	    by /api/agent/config so a NUC-class agent fine for remux doesn't
//	    get assigned a multi-hour transcode. Minimum stays at 2 — every
//	    addition is optional and a v2 agent talking to a v3 site degrades
//	    cleanly (no extracted data, no convert capability).
const AgentProtocolVersion = 3

// AgentVersion is the human-readable build string logged on the site when
// this agent polls. Not used for compatibility gating — that's
// AgentProtocolVersion's job — but useful for debugging which agents in the
// field have picked up a release.
//
// 1.5.30 — agent disk-leak audit (AUDIT-2026-07-opus-4.8): four fixes that
//	stop the agent accumulating disk + phantom reservation while it
//	otherwise looks idle, so it no longer needs a daily restart to
//	reclaim space.
//
//	1. Offline .torrent jobs now release their disk reservation
//	   (processOfflineJob defers ReleaseDisk). The .torrent download
//	   path reserves ~1.3x via ReserveDisk but nothing in the offline
//	   pipeline ever released it, so diskReserved climbed monotonically
//	   and eventually starved the poll loop with a phantom "disk full"
//	   — the 1.5.23 regression class, reintroduced via the offline path.
//	2. Seeding is now always time-bounded. A ratio target with
//	   torrent_seed_hours=0 previously seeded FOREVER on a dead swarm;
//	   because processTask invokes Seed as its last statement, the
//	   download-dir removal + ReleaseDisk defers never ran, pinning real
//	   bytes AND the reservation until restart. Adds a 24h fallback cap
//	   plus a dead-swarm exit (no peers + no progress for 30m).
//	3. Offline temp dirs no longer leak on failure: the sample dir
//	   (offline-shots-/pages-/samples-) gets a defer, and the encrypt
//	   archive's cleanup defer is now registered BEFORE the fallible 7z
//	   call (a partial .7z on encrypt failure previously orphaned).
//	4. The startup orphan sweep now also reclaims the offline-/enc-/
//	   wrap-/offer-/*.7z temp families the periodic sweep intentionally
//	   ignores — safe only at boot, before those pipelines' goroutines
//	   start, so leftovers accumulate to the next restart rather than
//	   forever.
//
// 1.5.29 — NormalizeInfoHash: uppercase lowercase base32 magnet hashes.
//
//	User-reported 2026-06-05: "Download error: error parsing
//	infohash 'ghgsyznfvrqxz3iwed2hv57dlvdnwo7q': error decoding
//	xt: illegal base32 data at input byte 0". anacrolix's
//	metainfo parser is strict about base32 case (RFC 4648 uses
//	uppercase A-Z + 2-7); lowercase 32-char hashes get rejected
//	at byte 0. The site sometimes hands the agent lowercase
//	base32 hashes (legacy storage shape), so every such task
//	failed at the magnet-construction step.
//
//	Fix: new NormalizeInfoHash helper uppercases 32-char base32
//	hashes; 40-char hex hashes (case-insensitive at the parser
//	level) pass through unchanged. Called at both magnet-build
//	sites: cmd/agent/main.go (internal download path) and
//	services/network_watch_handoff.go (BT-client handoff). Hex
//	hashes and non-base32 shapes are untouched — defensive: the
//	helper recognises ONLY the 32-char-all-base32 pattern.
//
//	Site-side normalization is a follow-up — the right long-term
//	fix is the SITE storing hashes in a canonical form, not the
//	agent fixing them up at consumption. This release unblocks
//	affected requests immediately.
//
// 1.5.28 — DiskMultiplier 2.1 → 1.3 (over-reservation fix).
//
//	User-reported 2026-06-05 across many releases:
//	  "Download error: insufficient disk space:
//	   torrent is 1.4 GB, have 1.4 GB free"
//	  "torrent is 0.7 GB, have 1.2 GB free"
//	  "torrent is 1.5 GB, have 2.8 GB free"
//	A 1.4 GB torrent should fit in 1.4 GB free disk plus a small
//	overhead, not the ~3 GB the 2.1x multiplier was demanding.
//
//	Root cause: the multiplier was sized for the worst-case
//	cross-device stage fallback (full copy), but this agent
//	creates stageDir at filepath.Join(cfg.TempDir, "stage-XXX")
//	(cmd/agent/main.go) — same device as dataDir under TempDir.
//	CopyFiles + ObfuscateFiles both call os.Link first, which
//	succeeds on same-device and costs ~0 bytes (just additional
//	inodes pointing at the same extents). So the staging line of
//	the worst-case budget is always ~0 in practice, not 1.0x.
//
//	New budget breakdown (1.3x):
//	  download files:         1.0x
//	  stage via hardlink:     ~0x
//	  PAR2 (5% redundancy):    0.05x
//	  safety (FS overhead +
//	     PAR2 working files):  0.20x
//	  ────────────────────────────
//	  total:                  ~1.25x → rounded to 1.3 for headroom
//
//	Effect: 1.4 GB torrent now reserves 1.82 GB instead of 2.94 GB.
//	Unblocks rejected tasks that fit in the real disk budget plus
//	margin. Pre-stage size check (1.5.25) + audit (existing) still
//	catch the rare cross-device case before PAR2 burns more disk.
//
//	No new config knob: stageDir is structurally same-device with
//	dataDir, so the multiplier doesn't need to be tunable for the
//	agent's own layout. If you remount stage to a different device
//	for some reason, you're already in custom-config territory and
//	can edit this constant directly.
//
// 1.5.27 — Two production-reported bugs + honest VPN limitations doc.
//
//	Bug 1 (user-reported): "Downloads complete but don't start
//	uploading, then fail with 'Download too slow — skipping'".
//	Root cause: the stall-detection check in downloadAndWaitSeed
//	intentionally has NO percent gate (legitimate case: stuck at
//	99.x% with 0 peers forever). But the select{<-done : <-ticker.C}
//	race can pick the ticker case AFTER WaitAll has already
//	signalled completion. In the ticker branch speed=0 (no new
//	bytes), so the stall timer starts. After StallMins minutes of
//	the agent waiting for an upload slot to open, the timer trips
//	and returns ErrSlowDownload — failing a 100%-complete download.
//	Fix: skip the stall check when bytes are fully downloaded
//	(completed >= total), and clear any stall timer that might have
//	armed during the race so a later timer trip can't fire.
//
//	Bug 2 (user-reported by eee, security-impacting): "gluetun
//	socks5 integration DOES NOT work in bridge mode
//	(VPN_DOWNLOAD_ONLY=true)". The applyVPNProxy function in
//	network_torrent.go was setting clientConfig.TrackerDialContext
//	+ HTTPDialContext, which only cover HTTP traffic (tracker
//	announces, metadata fetches). anacrolix's PEER-TO-PEER TCP
//	connections — where the actual torrent bytes flow — are made
//	via the package-internal listener/dialer and cannot be
//	redirected through SOCKS5 from clientConfig alone. So
//	split-tunnel mode was leaking the agent's real IP to every
//	peer in the swarm while logging "VPN split-tunnel: torrent
//	traffic routed via SOCKS5" (misleading).
//
//	Fix: replace the misleading log line with an honest warning
//	naming the exact leak ("peer-to-peer TCP connections still go
//	DIRECT — your real IP is exposed"). Update docker-compose.yml
//	comments to call out the limitation prominently and recommend
//	full-tunnel mode (VPN_DOWNLOAD_ONLY=false +
//	network_mode: service:vpn) as the only true VPN-protected
//	configuration. The split-tunnel path remains available for
//	users who add their own kernel-level routing for peer traffic
//	(iptables policy routing) — that scenario is hinted at in the
//	new comment block, but the agent itself can't enforce it.
//
//	No code change to the SOCKS5 setup — the agent CANNOT make
//	peer connections go through SOCKS5 without forking anacrolix
//	or running iptables tricks. The only honest thing to do is
//	tell the user so they don't trust a mode that doesn't protect
//	them.
//
// 1.5.26 — PAR2 failure visibility.
//
//	Witnessed 2026-06-04 on the re-upload of "[Poopoo] Another -
//	S01" (the same release that previously hit the disk-sweep
//	race). All 14 .mkv files shipped to Usenet correctly, but
//	ZERO .par2 recovery files were included — meaning any
//	article loss during retention is now unrecoverable. The
//	pre-1.5.26 path treated PAR2 generation errors as a single
//	stderr line ("PAR2 warning (non-fatal): %v") and proceeded
//	with the upload silently. Easy to miss; hard to attribute.
//
//	Two visibility upgrades:
//	  - Error case: log carries baseName + stageDir; site.PostLog
//	    ships a tagged "error" entry to /admin/errors so an admin
//	    can see "N releases shipped without recovery in the past
//	    hour" without ssh-ing into the agent.
//	  - Success-but-zero-files case: defensive guard for the
//	    weird state where GeneratePAR2 returns nil error but the
//	    output walker found nothing. Posts a "warn" so silent
//	    success doesn't masquerade as a real PAR2 run.
//
//	Behaviour unchanged: PAR2 failure is still non-fatal, the
//	upload still proceeds, the release still lands on Usenet —
//	just degraded (no recovery). Operator policy can flip this
//	to fail-closed by replacing the warning with the agent's
//	fail() call. Default kept lenient because a small release
//	may not need PAR2 (10% redundancy on a single 100 MB file is
//	rarely meaningful), but multi-GB multi-file releases really
//	should have it.
//
//	Diagnostic command for the 2026-06-04 incident:
//	  docker logs indexer-agent 2>&1 | grep -E "PAR2|par2" | tail -50
//	The new log line will name the underlying cause — most likely
//	a binary-missing exec error, a parpar internal failure, or a
//	filesystem permission issue inside the stage dir.
//
// 1.5.25 — Two leaks in the staging walker + TWO new pre-stage
//
//	"bad state" checks. Witnessed 2026-06-04 on
//	"[Poopoo] Another - S01" upload: 3 of 14 .mkv files shipped
//	on Usenet (only E10-E12 — alphabetically last), with
//	_screenshots/, _subtitles/, and `.torrent.bolt.db` shipped
//	alongside them. The partial-file failure had no detection
//	before this release: anacrolix's WaitAll tracks pieces it
//	RECEIVED, not files that EXIST on disk. The 1.5.20 sweep
//	wipe deleted files anacrolix had already marked complete,
//	so WaitAll returned to a directory with most files missing
//	— and nothing downstream noticed.
//
//	Pre-1.5.25 detection layers (all passed for this incident):
//	  - DirHasUsableFiles (1.5.13): "≥1 usable file" → 3/14 passed
//	  - auditStaged (existing):     "src files == dst files" → 3==3 passed
//	  - No comparison against torrent.Files() metadata existed.
//	The agent therefore proceeded to PAR2 + upload normally.
//
//	Leak 1: agent-managed subdirs inside dataDir (_screenshots,
//	_subtitles, created by 1.5.22 to inherit the dl-XXX keep-set
//	protection) were getting included in the NZB. Stage walker in
//	upload_obfuscate.go's CopyFiles / ObfuscateFiles walked the
//	entire dataDir; the post-walk auditStaged parity check passed
//	because src AND dst BOTH included them.
//
//	Leak 2: anacrolix's internal `.torrent.bolt.db` state file —
//	written into dataDir by the torrent client itself — also
//	shipped. Same root cause: stage walker didn't know it was
//	agent-internal.
//
//	Both leaks fixed by an explicit skip set:
//	  agentManagedDirs       — _screenshots, _subtitles (dirs)
//	  isAgentManagedFile     — .torrent.* prefix (files)
//	Applied symmetrically in the source walk AND the
//	countStagedFiles audit so apples-to-apples comparison holds.
//
//	Check 1 — per-file existence + size:
//	  TorrentSession.ExpectedFiles() enumerates the metainfo's
//	  file list with relative paths + lengths. The pre-stage hook
//	  stats EVERY expected file against downloadedPath. Missing
//	  files or truncated files (Size < Expected) abort the
//	  pipeline with a list of offenders (capped at 5 names per
//	  category for log readability). This is the canonical
//	  "compare reality against the manifest" check; the absence
//	  of it is why the Another S01 incident shipped silently.
//
//	Check 2 — gross byte-total parity (defence in depth):
//	  Compare CountUploadableBytes(downloadedPath) against
//	  ExpectedBytes(). Tolerance 80% leaves room for legitimate
//	  blocklist deletions (sample.txt, NFOs on a media-only
//	  group). Catches cases where Check 1 might be fooled by an
//	  inflated zero-byte file that os.Stat says "exists".
//
//	Both checks run BEFORE staging. On abort: the request stays
//	open for re-dispatch, no partial NZB ships, /admin/errors
//	gets a tagged entry with the exact missing/truncated paths.
//
// 1.5.24 — Disk-sweep keep-set protects active downloads BEFORE
//
//	DownloadedPath is stamped. The 1.5.20 fix walked
//	job.DownloadedPath up the tree looking for a "dl-" prefix
//	to put in the keep-set — but DownloadedPath is set by
//	storage.UpdateJobMeta only AFTER the download completes
//	(cmd/agent/main.go ~line 1530). During the active download
//	itself, DownloadedPath is the empty string and the keep-set
//	loop short-circuited, leaving the dl-XXX dir unprotected
//	for the entire download window.
//
//	Production symptom (2026-06-04 on request-21856,
//	[geckyzz] Go for It, Nakamura-kun!! S01E11): download ran
//	for 1m21s, hit "files=1 peers=2 peak-peers=5 runtime=1m21s
//	last-progress=100.0% (715387712/715420480 bytes) 5.86 MB/s"
//	— then "expected output ... missing after WaitAll (lstat:
//	no such file or directory)" with the entire dl-request-21856
//	directory gone by the time WaitAll returned. POST-MORTEM
//	confirmed ReadDir-err=no such file or directory — not "no
//	usable files in it" but the directory itself vanished. A
//	second run reproduced identically (99.6% then directory
//	missing). This was the 30-minute periodic sweep firing
//	during the active download. 1.5.20 only fixed the
//	post-download window; the active-download window remained
//	racy.
//
//	Fix: unconditionally add "dl-<jobName>" to the keep-set
//	for every job in storage.GlobalState.Jobs. jobName matches
//	the lock identifier ("request-NNNN" → "dl-request-NNNN")
//	and the JobState entry is created by storage.UpdateState
//	at the very first "claimed" status update (processTask
//	line 1177), so by the time any sweep tick can run the job
//	is registered. The DownloadedPath-walk loop is kept as
//	belt-and-braces for stage-XXX / screens-XXX prefixes that
//	downstream pipeline stages embed in DownloadedPath.
//
// 1.5.23 — ReserveDisk idempotent — fixes 159 GB phantom-reserved leak.
//
//	Production symptom: agent dashboard showing "Free: 169.9 GB,
//	Reserved: 159.4 GB, Available: 10.5 GB" on a VPS with maybe
//	2-3 in-flight downloads — nowhere near 159 GB of real
//	occupancy. New tasks rejected with "insufficient disk space:
//	torrent is X GB, have Y GB free" where Y barely exceeded X
//	even though the volume had >150 GB actually free.
//
//	Root cause: ReserveDisk always atomic.AddInt64(&diskReserved,
//	reserve) AND unconditionally overwrote the per-task map entry.
//	A double-call for the same job (retry / resume-from-partial /
//	re-dispatch) added the reservation TWICE to the global counter
//	but only stored ONE entry in the map — so the eventual
//	ReleaseDisk subtracted half of what had been added, leaking
//	the other half permanently. Hundreds of leaks over agent
//	lifetime accumulated to 159 GB of imaginary occupancy.
//
//	Fix: ReserveDisk now reads the prior reservation for jobName
//	from the map and computes delta = (new - old). For a fresh
//	job, delta == new (same as before). For a re-reservation,
//	delta = (new - old) so the global counter tracks ONLY the
//	current claim. Also logs a "re-reserved" line when the
//	overwrite path fires so the operator can see how often the
//	double-call shape happens in their workload — useful telemetry
//	for finding the upstream cause.
//
//	One-time cleanup: restarting the agent zeros the in-memory
//	counter; in-flight tasks reclaim their reservations on the
//	next ReserveDisk call. No persistent state to migrate.
//
// 1.5.22 — Screenshot upload race + silent-failure cleanup.
//
//	Two issues in the same shape as 1.5.20 + 1.5.21:
//
//	1. screens-XXX dir was top-level under <tempDir>/. The
//	   disk_reserve_sweep matched the "screens-" prefix for cleanup
//	   but ONLY protected dl-XXX + stage-XXX in its keep-set. Slow
//	   uploads past the 30-min minAge gate let the sweep nuke the
//	   screenshot directory mid-use — exactly the metadata-without-
//	   files symptom we saw on the Rent-a-Girlfriend release (counts
//	   surfaced but no images downloadable).
//	   Moved the screen dir inside dataDir as
//	     <dataDir>/_screenshots
//	   so the dl-XXX keep-set (1.5.20-fixed) covers it automatically.
//	   Both video + manga paths updated.
//
//	2. buildCompleteForm's screenshot multipart loop had three silent
//	   skip branches (os.Open error / CreateFormFile error / io.Copy
//	   error → continue without log). Promoted all three to explicit
//	   WARN log lines with screenshot index + path + bytes-written.
//	   Added a summary line "screenshot multipart: N/M included
//	   (P failed)" so an operator can see in one line whether the
//	   release's screenshot count metadata matches what actually
//	   shipped.
//
//	No behaviour change for healthy uploads. The screens-XXX
//	top-level prefix is still in the sweep's matches list for
//	backward compatibility with directories left behind by
//	pre-1.5.22 agent processes — those will get swept on the next
//	tick.
//
// 1.5.21 — Subtitle upload error visibility — three silent failure
//
//	paths in uploadSubtitleWith promoted to hard returns with
//	context. Specifically:
//
//	  * CreateFormFile error was previously "if err == nil { Copy }"
//	    — meaning a CreateFormFile failure silently produced a
//	    multipart with the subtitle FIELDS (nzb_id / lang / codec /
//	    track_index) but no file part. The site stored the row;
//	    the release page later 404'd on the download link with no
//	    log explaining why.
//
//	  * io.Copy return value was discarded — partial copy (file
//	    vanishing mid-read, e.g. disk_reserve_sweep race pre-1.5.20)
//	    succeeded as far as the multipart was concerned, sent
//	    truncated bytes, site stored a stub.
//
//	  * No positive log on success — operator couldn't tell
//	    "uploaded but DB-stored to wrong row" from "never left the
//	    agent" without grepping for absence-of-error.
//
//	Caller (uploadSubtitlesFor in Complete) also now tracks per-batch
//	OK/failed counts and logs "uploaded N/M for nzb_id=X" so an
//	operator inspecting one nzb can see in one line whether all
//	expected tracks landed.
//
//	Paired site-side change in agent_handler.go's UploadSubtitle:
//	  - Entry log with received nzb_id / track / codec / content-length
//	  - Exit log with stored subtitle_id + storage location (DB row
//	    for text formats, disk path for bitmap formats)
//	The two log lines bracket the round-trip; an operator can grep
//	by nzb_id on EITHER side and see both ends.
//
//	No behaviour change for healthy uploads (the new logs are net
//	additive, the new error returns trigger the same WARN line that
//	already existed for os.Open failures). Existing JSONInternalError
//	site-side error capture is unchanged.
//
// 1.5.20 — Fix: disk-reserve sweep was deleting active downloads.
//
//	1.5.18 regression caught by the 1.5.19 logging uplift. The
//	disk_reserve_sweep keep-set computation was:
//	  keep[filepath.Base(filepath.Dir(job.DownloadedPath))] = true
//	This was CORRECT when DownloadedPath = "<tempDir>/dl-XXX/<file>"
//	(pre-1.5.18) — Dir(...) = "<tempDir>/dl-XXX", Base(...) = "dl-XXX".
//	Post-1.5.18, DownloadedPath = "<tempDir>/dl-XXX" directly —
//	Dir(...) = "<tempDir>", Base(...) = the temp dir name ("temp"),
//	not "dl-XXX". The active download was therefore NOT in the keep
//	set; the periodic sweep then deleted the dl-XXX directory
//	mid-download.
//
//	Production symptom (from the 1.5.19 diagnostic logs):
//	  download done: bytes=422M files=1 peak-peers=4 runtime=3m26s
//	     last-progress="99.8% (421M/422M) 1.45 MB/s peers=4"
//	  expected output ".../Tadaima...mkv" missing after WaitAll
//	     (lstat: no such file or directory)
//	  POST-MORTEM: dirContents=[ReadDir-err=open
//	     /data/temp/dl-request-21347: no such file or directory]
//	The bytes match and peers were real — the download was healthy.
//	The whole per-request dir was just GONE by the time WaitAll
//	returned.
//
//	Fix: walk up the DownloadedPath path components looking for a
//	"dl-" / "stage-" / "screens-" prefix and keep THAT name. Works
//	correctly for both legacy file-path and 1.5.18 dir-path shapes.
//
//	Every download since 1.5.18 has been racing this sweep — the
//	"download produced no usable files" reports were never about
//	dead swarms; they were about the agent deleting its own work.
//
// 1.5.19 — Diagnostic logging uplift across the agent pipeline. Pure
//
//	additive log volume; zero behaviour change. Bias to verbose so
//	overnight failures are self-diagnosing without grep gymnastics.
//
//	network_torrent.go — every exit path of downloadAndWaitSeed now
//	emits a single line with: runtime since download started, peak
//	peer count seen across all ticks, and the last per-tick progress
//	snapshot (percent / bytes / speed / peers). Lines added to:
//	  - ctx.Done cancellation
//	  - <-done WaitAll completion (both happy + WARNING premature paths)
//	  - slow-download rejection
//	  - stalled-download rejection
//	  - low-seed rejection
//	  - RequireFull "no full seed" rejection
//	Plus a download-loop-start line at the top of the function so a
//	stuck task can be correlated to the moment it started. The
//	lastProgressSnapshot string is updated every tick; without it,
//	"why did WaitAll return with 0 bytes" is unanswerable.
//
//	cmd/agent/main.go — POST-MORTEM line immediately before the
//	"download produced no usable files" abort gate, dumping
//	downloadedPath, blockedByExt counts, and a sample of the on-disk
//	directory contents (up to 8 entries with sizes). Operator sees
//	in one line what the walker saw — no need to scroll up looking
//	for the 1.5.13 DirHasUsableFiles inventory.
//
//	media_subtitles.go / media_screenshots.go / media_audio.go /
//	media_remux.go / upload_par2.go — entry-point log lines on
//	ExtractSubtitles / GenerateScreenshotsWatermarked /
//	ProbeAudioTracks / RunRemux / GeneratePAR2 so a wedged
//	subprocess is greppable to its caller in one log line
//	(previously these functions logged only on failure).
//
//	Roll-back plan: every new log call is a single log.Printf — easy
//	to remove or downgrade to a debug-level helper later if the
//	volume becomes noise once we have prod confidence.
//
// 1.5.18 — session.Path always returns dataDir (not the inner file).
//
//	Production bug: single-file torrents with subtitle extraction
//	enabled crashed at "subtitles: mkdir <file>.mp4/_subtitles: not
//	a directory". The 1.5.15 lstat fallback only kicked in when the
//	expected inner path was MISSING — for healthy single-file
//	downloads the inner path EXISTED as a regular file, so
//	session.Path stayed pointed at the file. Every downstream
//	consumer (RemoveBlockedFiles / DirHasUsableFiles / FindVideoFiles
//	/ RunRemux / RunUpscale / ObfuscateFiles / CopyFiles / ManifestOf
//	/ subtitle mkdir / screenshot mkdir) expects a directory; the
//	walkers tolerated a file path by accident, the mkdir paths did
//	not.
//
//	network_torrent.go now returns sessionPath = dataDir
//	unconditionally. anacrolix writes either:
//	  - <dataDir>/<filename> for single-file torrents
//	  - <dataDir>/<torrent-name>/<files> for multi-file torrents
//	Walking dataDir finds both. filepath.Join(dataDir, "_subtitles")
//	creates a sibling directory of the content, which mkdir handles
//	cleanly. Cleanup defer (os.RemoveAll(downloadedPath)) removes
//	the whole per-request temp dir, which is what we want anyway.
//
//	The lstat is preserved as a DIAGNOSTIC-ONLY check: if the
//	expected inner path is missing, log a line so the operator can
//	see "anacrolix may have written to a different subpath" without
//	the routing depending on it. The 1.5.13 DirHasUsableFiles
//	diagnostic + 1.5.17 download-done stats still apply on top.
//
// 1.5.17 — Download-completion stats logged at WaitAll exit.
//
//	The case <-done branch in downloadAndWaitSeed always wrote
//	storage.UpdateState(jobName, "Downloading", "100% (Download
//	Complete)", 100) regardless of whether anacrolix actually
//	completed the download. On dead-swarm / zero-length-info /
//	removed-while-fetching cases, cl.WaitAll() returns with
//	completedBytes < totalLength but the log line and dashboard
//	both reported 100% — operator had to infer "did the download
//	actually happen?" from the downstream walker reporting an
//	empty directory.
//
//	Now logs a single explicit line at the moment WaitAll returns:
//	  - happy path: "[%s] download done: name=%q bytes=%d files=%d
//	    peers=%d" — self-describing healthy completion.
//	  - WARNING path: "[%s] WARNING: WaitAll returned but completed
//	    X / Y bytes (Z%%) — anacrolix signalled premature completion,
//	    downstream walker will likely find nothing" — operator sees
//	    the WARNING immediately and knows to look at peer count /
//	    torrent metadata rather than chasing a non-existent file.
//
//	Pairs with the 1.5.13 DirHasUsableFiles diagnostic + 1.5.15
//	lstat fallback: 1.5.17 says "did the download finish at all",
//	1.5.15 says "was the path resolution correct", 1.5.13 says
//	"did the walker see anything". Together those three lines fully
//	characterize any future empty-release abort.
//
// 1.5.16 — stop_after_current restart-loop guard.
//
//	Production incident: the site sent stop_after_current to an agent
//	and never cleared the command. The agent's old behaviour was: if
//	stop_after_current arrives and activeTaskCount() == 0, exit
//	cleanly with PostLog("Graceful shutdown..."). Combined with
//	docker compose's restart: unless-stopped policy, this turned
//	into a tight restart loop: process boots → polls → receives the
//	stale stop → exits clean (exit 0) → docker restarts → cycle
//	repeats every ~60s. We caught it at RestartCount=290 across ~5
//	hours of agent downtime; operator thought the agent had crashed.
//
//	cmd/agent/main.go now captures processStart at package init and
//	guards the stop_after_current handler with a 10-minute uptime
//	window: if we receive stop_after_current with no active tasks
//	AND the process has been alive less than 10 minutes, refuse to
//	exit and continue polling. Log line surfaces the wait so an
//	operator can see the guard fired and clear the stop on the site
//	side. Past the 10-minute window the guard releases and the
//	original "graceful shutdown" path runs — preserves the
//	deliberate-stop-of-idle-agent UX while breaking the
//	auto-restart loop.
//
//	Two follow-ups should ship on the SITE side (separate spec):
//	(1) /api/agent/poll's command field should be one-shot —
//	acknowledged on delivery so a fresh process can't re-receive the
//	same stop, and (2) the dashboard should expose a "clear
//	pending command" affordance so the recovery doesn't require a
//	DB-side UPDATE.
//
// 1.5.15 — session.Path lstat fallback + abort message accuracy
//
//	Production bug: downloadAndWaitSeed returned TorrentSession.Path =
//	filepath.Join(dataDir, t.Name()) without verifying the joined path
//	actually exists on disk. cl.WaitAll signalled "complete" on dead
//	swarms / zero-length info dicts / sanitization-mismatch cases, and
//	the downstream walker then reported "release was empty before the
//	sweep" pointing at a path that lstat returned ENOENT for. The
//	abort message also incorrectly suggested editing banned_extensions
//	even when zero files had been stripped by the blocklist —
//	misleading operators into thinking it was a blocklist problem.
//
//	network_torrent.go now lstats the computed path before returning
//	the TorrentSession; if missing, falls back to dataDir so the
//	downstream walker descends the per-request temp dir directly.
//	When the dir is genuinely empty, the DirHasUsableFiles diagnostic
//	from 1.5.13 still fires with the inventory line, so the operator
//	gets the truthful "no usable files" path instead of a confusing
//	"file not at expected path" walk error.
//
//	cmd/agent/main.go's abort reason now branches: when the blocklist
//	did remove files, it surfaces "blocked: .ext×N" + the
//	banned_extensions suggestion. When the blocklist removed nothing
//	but the dir is still empty, it surfaces the no-peers / empty-
//	swarm diagnosis and points operators at the DirHasUsableFiles
//	log line for the walker inventory — not at banned_extensions
//	(which can't help in this branch).
//
// 1.5.14 — CJK / non-ASCII safety hardening across nine call sites
//
//	identified by the three-lens audit (subprocess, NZB/NNTP, string/
//	filesystem). Pure additive — every fix is a no-op for ASCII inputs.
//
//	HIGH severity:
//	  - SanitizeBaseName now truncates by rune count, not byte count.
//	    A 200-byte cut in the middle of a 3-byte CJK codepoint produced
//	    invalid UTF-8 that json.Marshal then replaced with U+FFFD on
//	    every round-trip, silently corrupting PAR2 base names. Also
//	    adds fullwidth Unicode equivalents (U+FF1A FULLWIDTH COLON,
//	    U+FF1F FULLWIDTH QUESTION MARK, U+FF5C FULLWIDTH VERTICAL LINE,
//	    etc.) to the replacer so CJK-titled releases get consistent
//	    handling with the ASCII set.
//	  - extract_zip.go now decodes legacy CP932 / GBK filenames stored
//	    in zips written with PKWARE GP-bit-11 (EFS) unset. Older
//	    Japanese WinRAR / Windows Explorer / 7-Zip <9.30 produced such
//	    zips; archive/zip's f.Name returns the raw bytes, which the
//	    walker then writes to disk as mojibake. New decodeZipName helper
//	    tries Shift-JIS then GBK and uses whichever yields valid UTF-8,
//	    logging which decoder won. Pulls in golang.org/x/text/encoding.
//	  - NNTP article From / Subject / X-Newsreader headers now wrap via
//	    mime.QEncoding.Encode("UTF-8", ...) — no-op for pure ASCII,
//	    RFC 2047 encoded-word otherwise. Defensive belt for the moment
//	    any operator sets NNTPFrom / GeneratorName to a CJK or accented
//	    value. Message-ID domain is now run through idna.Lookup.ToASCII
//	    so an IDN NNTP_FROM (x@蝶龙.com) emits the punycoded domain
//	    instead of the raw CJK bytes that would 441-reject at strict
//	    relays. splitNNTPGroups now drops tokens that violate RFC 5536
//	    §3.1.4 (must be ASCII letters / digits / "." / "+" / "-" / "_")
//	    with a warning rather than letting them onto the wire.
//	  - upload_archive.go EncryptWith7z now enumerates srcDir with
//	    os.ReadDir and passes each entry as its own argv element
//	    instead of relying on 7z's "*" glob expansion (which would
//	    misinterpret literal '*' / '?' / '[' bytes in CJK-titled
//	    filenames).
//
//	MEDIUM:
//	  - offer_folder_scanner reResolution / reSourceTag dropped the
//	    ASCII-only `\b` word boundary so "アニメ1080p.mkv" matches
//	    "1080p" again. reEpOnly keeps a boundary but uses
//	    (?:^|[^A-Za-z0-9]) which fires next to CJK runes too.
//	  - upload_par2 buildPar2createCmd now logs a one-line warning
//	    when invoked against any non-ASCII filename, since par2cmdline
//	    is known to mojibake the PAR2 header UTF-16LE names — parpar
//	    is the recommended binary and the warning points operators at
//	    it.
//	  - writeConcatList in media_upscale escapes backslashes before
//	    the single-quote pass so romaji titles like "Doomdos's path"
//	    containing both a backslash and an apostrophe don't break
//	    ffmpeg's concat demuxer.
//	  - normalizeTitle in offer_sync uses EqualFold on the trailing
//	    slice instead of lowercasing the whole title first
//	    (strings.ToLower can change byte length on locale-sensitive
//	    runes, making the slice cut at the wrong position).
//
//	Confirmed safe (verified by audit, no change needed): every
//	exec.Command* argv site, NZB XML generation (xml.Encoder), yEnc
//	body byte loop, all JSON encoding paths, ffprobe / fpcalc /
//	tesseract output parsing, resume_validation, upload_manifest,
//	disk_blocklist diagnostic added in 1.5.13.
//
// 1.5.13 — DirHasUsableFiles diagnostic. The "release was empty before
//
//	the sweep" abort previously logged nothing about WHAT the walker
//	saw — operator couldn't tell empty-dir from wrong-dir from
//	CJK-walker-glitch from a silent EACCES on the readdir. The walk
//	now counts entries / dirs / regular files / size-0 files / walk
//	errors as it goes, captures the first walk error, and logs a
//	one-line inventory plus the first five sample basenames whenever
//	DirHasUsableFiles returns false. Specifically aimed at debugging
//	releases with CJK / non-ASCII characters in filenames that appear
//	in the torrent but vanish before staging — the inventory will
//	show whether the file is being visited (and filtered out by
//	size==0 or IsDir misclassification) or never visited at all
//	(anacrolix sanitized the name, mount-point codeset translation,
//	wrong downloadedPath). No behaviour change beyond log volume.
//
// 1.5.12 — Non-ASCII filename support — UTF-8 locale baked into both
//
//	Dockerfile + Dockerfile.gpu. The runtime container previously
//	inherited the default POSIX locale, which causes mediainfo to
//	silently skip files whose names contain non-ASCII bytes
//	(Chinese, Japanese, accented characters) and ffmpeg to mangle
//	output paths to those files. Setting LANG=C.UTF-8 + LC_ALL=
//	C.UTF-8 at the runtime stage flips every spawned binary to the
//	correct codeset without dragging in a full glibc locale-gen or
//	musl-locales package (C.UTF-8 is built into musl 1.2.4+ and
//	glibc 2.35+, both of which ship in the bases we use). No agent
//	code change — pure image fix; rebuild + pull on the VPS to
//	pick it up.
//
// 1.5.11 — yEnc standards compliance — file-level CRC32. The =yend
//
//	trailer on the final part of every multi-part article now
//	carries a whole-file crc32 attribute (yEnc 1.3 §5.4) alongside
//	the existing per-part pcrc32. Format is lowercase 8-hex-char
//	zero-padded, identical to pcrc32's encoding. Some Usenet
//	decoders (sabnzbd, nzbget) downgrade verification to a best-
//	effort hash check when the whole-file crc32 is missing on the
//	final part; pickier tools warn or refuse. The new attribute is
//	purely additive — decoders that don't recognise it ignore it,
//	decoders that do can verify end-to-end without reassembling
//	and recomputing the CRC themselves. Pure data-on-the-wire
//	change: successful upload paths are byte-identical except for
//	the extra " crc32=<8hex>" on each file's terminal article.
//
//	Implementation: the upload worker pool consumes chunks in
//	parallel across N nntpWorker goroutines, so a running
//	hash.Hash32 isn't safe (CRC-32 is byte-position-dependent and
//	chunks can arrive out of order). Instead, yEncodeChunk records
//	each part's pcrc + length in a package-level sync.Map keyed by
//	(filename, totalSize), and the final part waits on a sync.Cond
//	for every earlier part to register before combining them in
//	part-order via crc32CombineIEEE (the standard zlib GF(2)
//	matrix-exponentiation algorithm, since Go's stdlib doesn't
//	expose crc32.Combine). The map entry is deleted as soon as
//	the final part emits, so consecutive uploads of the same
//	(filename, totalSize) tuple start with fresh state. Single-
//	part files also emit crc32 (== pcrc32) for spec-strict
//	decoders. New regression test (TestYEncFileCRC) gates the
//	fix: encodes a deterministic 4 KiB ramp into four parts and
//	asserts non-final parts carry only pcrc32, the final part
//	carries both pcrc32 and crc32, and the crc32 value equals
//	crc32.ChecksumIEEE of the full input.
//
// 1.5.10 — Observability release. No behaviour change anywhere; only
//
//	log lines and one new request header. Three small wins land
//	together. NNTP per-chunk logs gain a request-id prefix:
//	UploadJob picks up a JobName field that's threaded from
//	UploadDirectory through the worker pool, and every chunk-level
//	log line ("POST chunk N", "Uploaded chunk N - MsgID: X",
//	"Chunk N upload failed", "FATAL: chunk N failed after …",
//	"nntp: auth error …") is now prefixed with [request-N] so
//	concurrent uploads interleaved in agent logs can be untangled
//	without timestamp matching. Outbound HTTP gets a per-request
//	UUID: versionHeaderTransport.RoundTrip now generates a
//	uuid.New().String() per call, sets it as the X-Request-ID
//	header, and logs "client: METHOD PATH X-Request-ID=…" locally
//	so the agent log carries the ID before the request is on the
//	wire. The site picks the header up on its side (no site change
//	required in this PR — header is just present) so when a
//	request_lock.fail_reason row shows up the operator can grep
//	the agent's docker logs for the matching ID instead of
//	matching timestamps by eye. Watchdog gains runtime
//	introspection: each tick captures runtime.NumGoroutine() and a
//	brief runtime.MemStats sample (HeapAlloc, StackInuse) and
//	emits "[watchdog] goroutines=X heap=YMiB stack=ZMiB
//	tasks_active=N stalled=M" alongside the existing per-task
//	lines. A leak in any of the long-lived background workers
//	(offline processor, offer sync, NNTP pool, BT client) was
//	previously invisible until the agent OOMed; the watchdog now
//	flags a >50% goroutine-count growth between consecutive ticks
//	with "WARNING: goroutine count grew P -> N (>50%) — likely
//	leak" so the next operator sees the signal hours before OOM.
//
// 1.5.9 — Audit cleanup. No behaviour change for in-flight successful
//
//	tasks; four small wins land together. NNTP error classification:
//	nntpWorker's retry loop now treats auth-class responses (401,
//	403, 480, 481, 482, 502, and the legacy "NNTP Auth failed"
//	wrapper from connectNNTP) as permanent failures and exits
//	immediately instead of burning the full 3-retry × backoff
//	budget on a misconfigured NNTP_USER / NNTP_PASS. 5xx outside
//	the auth set, network errors, and i/o timeouts stay on the
//	retry path — those are genuinely transient. A wrong-password
//	upload used to chew ~30 seconds of confused logs before the
//	worker gave up; it's now a single auth-error log line and
//	the task fails fast back to the operator. Per-request
//	completion timeout: the global SiteClient.http kept its
//	120s Timeout, which was killing legitimate Complete uploads
//	on slow uplinks when the multipart payload carried 30+ HD
//	screenshots — the agent did all the work but the site never
//	saw the result, the lock expired, and the next agent re-did
//	the task. Complete now uses a dedicated 10-minute-timeout
//	client for the multipart POST and the screenshot / subtitle
//	fallback uploads it drives; periodic Poll / PostStatus /
//	ReportProgress / PostLog calls stay on the tight 120s
//	client so a wedged site still surfaces fast. Message-ID
//	hygiene: the local part of the article Message-ID is now
//	32 hex chars (UUID with dashes stripped) instead of the
//	canonical dashed form. Same entropy, more conventional on
//	the wire, friendlier to the few legacy NNTP servers that
//	get unhappy with hyphens. Swallowed-error logging: seven
//	site.Complete / site.PostLog state-transition call sites in
//	cmd/agent/main.go (oversize cooldown, disk shortfall, agent-
//	local error, task-failed, watch_folder user-cancel,
//	watch_folder timeout, slow-download skip, user-skipped
//	download, blocklist-empty abort, manifest-mismatch report)
//	now log on error with the request ID and the action that
//	failed instead of dropping the error with `_ =`. Three
//	periodic site.ReportProgress calls inside hot per-iteration
//	callbacks stay silent — those fire many times per task and
//	a transient progress-write failure has no user-visible
//	consequence (the next tick will succeed).
//
// 1.5.8 — Stability release. No behaviour change for in-flight successful
//
//	tasks; only the failure and shutdown paths change. The agent now
//	owns a SIGINT/SIGTERM-aware root context (signal.NotifyContext
//	wired through services.SetRootContext / RootContext, mirroring
//	the site) and threads it through every long-lived background
//	goroutine: StartOfflineWatcher, StartOfflineProcessor,
//	StartSiteGroupsSync, OfferSync.Start, OfferFulfill.Start, and
//	runWatchdog. The polling loop checks ctx.Err() at the top of
//	each iteration so docker stop exits within one poll interval
//	rather than waiting for the current sleep to expire, and per-
//	task ctxs now derive from the root via context.WithCancel so
//	a shutdown mid-task propagates to nntpWorker, runSeedPhase,
//	and downloadAndWaitSeed instead of being killed mid-syscall.
//	Three goroutine-leak / responsiveness wins follow from the
//	new ctx plumbing. The cl.WaitAll watcher in downloadAndWaitSeed
//	used to leak past parent ctx cancel — the ctx.Done() branch
//	now drains the done channel after cl.Close() so the goroutine
//	always joins. The NNTP upload worker used to be stuck up to
//	~6 minutes on a wedged provider (3 retries × 120s op timeout
//	+ backoff) before it noticed shutdown; nntpWorker now checks
//	ctx.Err() at the top of every retry and a new backoffCtx
//	replaces backoff in the worker so an 8-second backoff is
//	pre-empted on cancel. The seed phase already honoured
//	ctx.Done() but never received a cancellable ctx; that's
//	fixed too. And one correctness item: the resume-from-prev-
//	download branch in processTask now calls a new
//	services.ValidatePartialDownload helper before trusting the
//	staged dir — refuses on symlinks, empty trees, or (if the
//	caller supplies expected bytes) size outside [0.5x..1.5x].
//	An interrupted previous run that left a stub dir on disk now
//	starts a fresh download instead of feeding a corrupt
//	skeleton into PAR2 / NZB upload.
//
// 1.5.7 — Hygiene release. No behaviour change, no operator-visible default
//
//	flipped: the wire NNTP Subject is still obfuscated and the
//	planned subject_mode toggle is still pending operator signoff.
//	Four audit quick-wins land together. First, NNTP article
//	headers — every posted article now carries a Date header
//	(RFC 5322 / RFC 3977 compliant), an X-Newsreader set from
//	cfg.GeneratorName, and X-No-Archive: yes so archive.org-class
//	crawlers skip us by convention; the Subject line itself is
//	unchanged from 1.5.6. Second, subprocess timeouts — mkvmerge
//	-J probes run under a 30s context and parpar / par2create get
//	a 60-minute backstop, so either binary wedging on a corrupt
//	input now fails fast with a ctx-deadline error and releases
//	the slot for the next task instead of hanging until the
//	operator restarts the agent. Third, concurrency + resource
//	safety — UploadSlot acquire/release picks up a panic-recover
//	defer so a panic mid-upload no longer leaks the slot,
//	nntpWorker closes its connection on ctx cancel instead of
//	leaking the socket, and copyFile checks the Truncate error
//	during pre-allocate so an ENOSPC during sparse-file setup
//	surfaces before the write loop wastes time on a doomed
//	target. Fourth, error logging that was silently swallowed —
//	screenshot upload io.Copy errors, json.Decode errors in
//	ClearMyLocks / PostStatus / Complete, NNTP QUIT errors, and
//	the post-extract os.Remove calls in extract_rar / extract_7z
//	/ extract_zip / extract_iso / extract_misc all log their
//	failures now instead of dropping them on the floor. None of
//	these change task outcomes; they just turn future
//	"what happened?" investigations into one grep.
//
// 1.5.6 — AI upscale extends to manga (CBZ). UpscaleModel gains a Type
//
//	field; "image"-typed models route through a new pipeline
//	(services.runImageUpscale) instead of the chunked video path:
//	CBZ extract → ncnn-vulkan dir-mode on the whole chapter →
//	repack as a new CBZ. Pages are sorted + 5-digit-prefixed so
//	order survives the upscale; non-image entries (ComicInfo,
//	thumbnails) are forwarded verbatim into the output. The same
//	realcugan / realesrgan binaries handle both — RealCUGAN with
//	denoise on noisy raws, the anime-stills RealESRGAN
//	(RealESRGAN_x4plus_anime_6B) on clean line art. UpscaleResult
//	's emitted-file slice renamed EmittedMKVs → EmittedFiles
//	since the manga path emits CBZs.
//
// 1.5.5 — AI upscale, opt-in (Phase 1 + 2). Agents detect their GPU via
//
//	nvidia-smi and advertise both the GPU and the upscale model
//	keys they have bundled (services.GPUCapabilities) on every
//	capability report. The operator opts in by flipping the new
//	ai_upscale toggle on /agent/<id>; the dispatcher then routes
//	requests carrying a non-empty upscale_option only to opted-in
//	agents.
//
//	services.RunUpscale (parallel to RunRemux, Step 1c) drives a
//	chunked pipeline per video: ffmpeg deinterlace (bwdif) +
//	denoise (hqdn3d) → ncnn-vulkan (RealCUGAN or Real-ESRGAN per
//	the model registry) → re-encode (libx265 CRF 23 10-bit) →
//	concat the upscaled chunks → mux the original audio /
//	subtitles / chapters back in. 30-second chunks bound peak
//	disk on a 2h movie to ~3 GiB of working PNGs.
//
//	The model catalog (services/upscale_models.go) is
//	deliberately data-driven: each registry row carries the
//	binary + scale + per-binary args, so adding a model is a
//	one-line entry plus dropping its (few-MB) file into the GPU
//	image. Detection probes binary-on-PATH; CPU agents report
//	empty advertised_models, which keeps the request dropdown
//	off them entirely.
//
//	A new Dockerfile.gpu builds the variant on nvidia/cuda 12.4
//	with the realesrgan / realcugan ncnn-vulkan binaries +
//	models bundled. The CPU image is unchanged — without the
//	binaries, AvailableUpscaleModels returns nil and every
//	upscale-related code path is a no-op, so a 1.5.5 agent on
//	the CPU image behaves identically to 1.5.4. Catalog
//	surfacing of upscaled releases lands in a follow-up.
//
// 1.5.4 — Archive extraction expanded to the full container set. The
//
//	post-staging extract wave (between Step 4 staging and Step 5
//	PAR2) now covers, in order: RAR (4.5), ZIP (4.6), 7z (4.7),
//	ISO disc images (4.8), tarballs (4.9), and legacy lzh/cab/
//	arj/cpio (4.10). ZIP gets a store-mode exception — an all-
//	stored zip is already streamable so it's left intact; any
//	compressed entry triggers a full unpack. 7z + ISO + the
//	legacy set shell to the p7zip family already in the image
//	(7z reads ISO9660 + UDF, so BD/DVD disc images unpack to
//	their real BDMV/VIDEO_TS tree). Tarballs decode through the
//	Go stdlib for gzip/bzip2/plain — so .tar(.gz|.bz2) extract
//	even with no 7z — and pipe xz/zstd through `7z -so`. Every
//	stage shares the RAR stage's par2-orphan sweep, preserves
//	partial success on per-archive failure, and is a silent
//	no-op when its binary is absent — the source archive then
//	uploads as-is, unchanged from 1.5.3.
//
// 1.5.3 — Three things land together:
//
//  1. Automatic RAR extraction. New services.ExtractRARArchives
//     step runs between staging (Step 4) and PAR2 (Step 5).
//     Walks the staged dir for .rar archives, extracts each
//     set in place via `unrar` (preferred) or `7z` (fallback,
//     already in the image), and sweeps the source .rar
//     volumes + any orphaned .par2 recovery files. PAR2 then
//     runs over the extracted media files and the uploaded
//     NZB carries the real content rather than a wrapper.
//     Per-archive failures are logged but don't abort the
//     task — partial-success is preserved. Silent no-op
//     when neither binary is on PATH.
//
//  2. Multi-arch docker images. Dockerfile now copies go.mod
//     + go.sum first and runs `go mod download` instead of
//     depending on a pre-populated vendor/ folder (which is
//     .gitignored and never present on fresh clones — used
//     to break `docker compose build` for everyone except
//     the maintainer). New .dockerignore excludes vendor/
//     so a maintainer with a stale local vendor folder
//     can't accidentally re-mask the bug. Push scripts
//     (push_docker.{sh,bat}) switched to `docker buildx
//     build --platform linux/amd64,linux/arm64 --push`.
//     New .github/workflows/docker-publish.yml auto-publishes
//     the same multi-arch manifest on every push to main —
//     ARM operators (Raspberry Pi 4/5, Apple Silicon, AWS
//     Graviton) can now `docker pull amenzb/loon-agent
//     :latest` without building from source.
//
//  3. Collection mode. New 3-tab local UI (Mirror / Offers /
//     Collection) for walking an existing media library on
//     disk and turning each release into a queued upload.
//     See services/collection_scanner.go + the UI shell.
//     Mirror tab gains live status + per-task cancel; site-
//     offer end-to-end pipeline + per-stage telemetry are
//     wired through.
//
// 1.5.2 — Online-task blocklist becomes site-driven. The site's
//
//	/api/agent/config response now always carries the effective
//	banned_extensions list (operator-configured if set, system
//	defaults otherwise — single source of truth lives in the
//	site's services.DefaultAgentBannedExtensions, mirrored
//	from this agent's DefaultBlockedExtensions). The agent
//	applies what arrives; its hardcoded fallback is now a
//	cold-start safety net only and effectively never fires
//	in steady state.
//
//	Also lands the empty-after-blocklist-sweep abort: if a
//	release contains only blocked extensions (typical for
//	DVD_ISO-shaped releases on default-blocking agents), the
//	task aborts cleanly with a reason that names which
//	blocklist ran and where the operator can edit it, instead
//	of the confusing "no files to upload in stage-XXX" five
//	steps later in the NNTP uploader.
//
//	And clarifies the local /groups UI scope: the per-group
//	banned_extensions field there only governs watch-folder
//	jobs; site-polling tasks use the per-agent list configured
//	at the site's /account-settings/agent/<id>.
//
// 1.5.1 — Dockerfile: install chromaprint-tools (fpcalc) and
//
//	tesseract-ocr + the eng/jpn tessdata packages in the
//	runtime image. 1.5.0 shipped the Phase G + I extractor
//	code with LookPath guards but didn't add the binaries to
//	the image, so every stock deployment was silently
//	skipping audio fingerprints and manga OCR. No code
//	changes; deploy-side fix only. Image size grows ~80 MB
//	(mostly Japanese tessdata).
//
// 1.5.0 — Per-release data extraction wave. Bumps AgentProtocolVersion
//
//	to 3. Adds six post-download stages that ship structured
//	metadata back to the site alongside the existing NZB +
//	screenshots:
//	  • Subtitle extraction (mkvmerge -J + mkvextract) — one
//	    /api/agent/subtitle POST per track; text formats ship
//	    bytes inline, bitmap formats (PGS, VOBSUB) ship as files.
//	  • Audio track catalog (mkvmerge -J) — language, codec,
//	    channels, sample rate, bitrate, flags. Metadata-only;
//	    the bytes stay in the NZB.
//	  • Audio fingerprint (fpcalc / Chromaprint) — full-track
//	    scan per video. Sets up cross-release dub-detection.
//	  • Dominant colour palette — bucket-histograms the
//	    screenshot PNGs we already produced; top-8 hex.
//	  • Manga OCR (tesseract eng+jpn) — runs on the same
//	    sample pages we extract for screenshots. Noise-filtered
//	    so graphic-only spreads drop out.
//	  • HDR side data — MaxCLL, MaxFALL, mastering display
//	    luminance bounds, Dolby Vision profile + layer flags.
//	    Folded into the existing media_info JSON; no new field.
//	Adds three Bluray re-encode targets to the per-request
//	remux dropdown (convert_h264 / convert_h265 / convert_av1
//	via ffmpeg). Gated on a new per-agent convert_video flag —
//	re-encoding is two orders of magnitude heavier than
//	remuxing, so the dispatcher routes convert_* requests only
//	to opted-in agents. Audio + subs + chapters pass through
//	unchanged on every target.
//	Every external binary (mkvmerge, mkvextract, fpcalc,
//	tesseract) is optional — LookPath miss logs once and skips
//	the corresponding step, so older agent images keep working
//	exactly as before with no behaviour change.
//
// 1.4.5 — NZB upload vs torrent-seed split on the agent dashboard.
//
//	The combined upload throughput (NNTP POST + BT seed-back)
//	was previously aggregated into a single ▲ value and a single
//	blue line on the speed graph, so operators couldn't tell at
//	a glance whether their upstream bandwidth was reaching
//	Usenet or just sharing back to torrent peers.
//	AgentLiveStatus now carries NzbUploadSpeed + SeedUploadSpeed
//	alongside the existing UploadSpeed (kept as the sum for
//	backwards compat). aggregateLiveStatus buckets per-task
//	entries by Phase: "uploading" → NZB, anything else with
//	a non-zero UpSpeed → seed. The site dashboard graph now
//	draws three polylines (green/blue/warn-amber) and the ▲
//	row splits into "NZB X · Seed Y" when both are active.
//
// 1.4.4 — Dashboard "stuck at 100%" fix. The per-task in-flight
//
//	progress entry is now cleared the moment the pipeline
//	finishes, so the UI no longer shows the task as
//	"uploading 100%" for the up-to-1h seed window. The
//	deferred clear at processTask entry only fired on
//	function return (i.e. after seed completed), which made
//	finished tasks look stuck even though the NZB was already
//	on the site. The Pipeline-complete log + Slot RELEASED log
//	are the source of truth; the dashboard now agrees with them.
//
// 1.4.3 — Stage-dir keep-set fix. The orphan sweeper runs every 30 min
//
//	with minAge=30 min and previously only protected dl-XXX
//	download dirs (via JobState.DownloadedPath). Stage-XXX upload
//	dirs were unprotected, so any task that sat in the upload
//	queue longer than 30 min — e.g. the v1.4.1-era seed-wedge,
//	or just a deep queue behind a multi-GB transfer — could have
//	its prepared stage wiped out from under it. The task would
//	then "Complete" with zero articles posted and the release
//	would land on the site with no NZB.
//	JobState now carries StagePath, set the moment stage-XXX is
//	created and cleared in the deferred RemoveAll path. The
//	sweep's keep-set honors both fields so an in-flight task's
//	working dirs are always safe.
//
// 1.4.2 — Upload slot scoped to the NNTP phase only. Previously the
//
//	slot was held across the post-upload seeding window (up to
//	1h per the default ratio target), so the queue could only
//	drain at one task per hour even though uploads themselves
//	finished in minutes. session.Seed now runs after the slot
//	is explicitly released; releaseSlot is sync.Once-guarded so
//	the deferred error/panic paths still unlock cleanly without
//	double-Unlock panic. Adds slot-lifecycle logs
//	([N] Slot ACQUIRED / [N] Slot RELEASED held Xs) so future
//	slot-held-too-long bugs are one grep away.
//
//	Watchdog goroutine: every 60s, logs one line per in-flight
//	task with its current phase + percent. Tasks that haven't
//	moved across 3+ consecutive ticks get flagged "⚠ STALLED
//	Xs in <phase>" — catches stuck-in-disk-IO, stuck-in-
//	subprocess, stuck-on-channel-send, anything outside the
//	timeout-protected code paths.
//
// 1.4.1 — NNTP I/O timeouts. connectNNTP gets a 30s dial timeout;
//
//	every uploadChunk exchange runs under a 120s socket
//	deadline so a wedged provider can't freeze a worker forever.
//	Adds "POST chunk N" / "NNTP connected" / "Acquired upload
//	slot after Xs" lifecycle logs so silent stalls are
//	observable in agent logs going forward.
//
// 1.4.0 — upload-aggregator fix (sums fp.UpSpeed unconditionally so
//
//	usenet uploads register on the strip total + speed graph),
//	per-file Size + Transferred populated, ProgressCallback
//	signature carries total/transferred bytes, opt-in cache
//	hit before DHT.
const AgentVersion = "1.5.30"

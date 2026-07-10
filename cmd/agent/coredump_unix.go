//go:build !windows

package main

import (
	"log"
	"syscall"
)

// disableCoreDumps sets RLIMIT_CORE to 0 so the kernel never writes a
// core dump if this process (or a child that inherits the limit)
// crashes.
//
// This is a SECURITY control, not a debugging preference. A core dump
// is a snapshot of full process memory — it contains the live NNTP
// password, the agent bearer token, and every other secret pulled
// from the container env. The kernel writes it as `core.<pid>` into
// the crashing process's working directory; when that directory sat
// inside a torrent's content tree, the upload stage walked it and
// posted the dump to Usenet alongside the media (incident: core.33378
// shipped with a video release, leaking creds publicly). Disabling
// dumps at the source means the file can never exist in the first
// place — the definitive fix, independent of the upload-stage
// allowlist that also now rejects it.
func disableCoreDumps() {
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0}); err != nil {
		// Non-fatal: the upload-stage allowlist (isUploadableContent)
		// is the second line of defence, and a running agent is better
		// than a dead one. But this should never fail — log loudly so
		// it's visible if it ever does.
		log.Printf("SECURITY: failed to disable core dumps via RLIMIT_CORE: %v", err)
		return
	}
	log.Printf("core dumps disabled (RLIMIT_CORE=0)")
}

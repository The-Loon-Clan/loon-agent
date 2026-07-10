package services

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

// This file centralizes how the agent spawns external tools (ffmpeg,
// par2, mkvmerge, tesseract, 7z, unrar, …). Two incident-driven
// invariants live here:
//
//  1. Child tools get a SCRUBBED environment. A media/par2 tool has no
//     need for the agent's NNTP password, agent token, or VPN
//     credentials — and a crash dump of a child process captures its
//     whole environment. That is exactly how the 2026-07-10 leak put
//     the NNTP password on Usenet: par2/ffmpeg segfaulted with the
//     agent's full env inherited, dumped core into the content dir,
//     and the upload stage swept it up. Stripping secrets from the
//     child env means a future dump (if core dumps are ever re-enabled
//     or a tool writes memory elsewhere) can't carry them.
//
//  2. Tool FAILURES are reported to the site, not just local stdout. A
//     signal kill (SIGSEGV etc.) is the crash-dump class and is
//     escalated through ToolFailureSink → the site's agent_logs so it
//     shows in the admin dashboard instead of vanishing into a
//     container log nobody collects.
//
// Every exec.Command(Context) site routes its env through toolEnv()
// and its failures through reportToolFailure().

// secretEnvKey reports whether an environment variable name holds a
// credential a child tool must never receive. Pattern-based on top of
// the known set so a future secret env var is excluded by default —
// the safe direction for a security control.
func secretEnvKey(name string) bool {
	u := strings.ToUpper(strings.TrimSpace(name))
	switch u {
	case "AGENT_TOKEN", "NNTP_PASS", "NNTP_USER", "VPN_USER", "VPN_PASS":
		return true
	}
	return strings.Contains(u, "PASS") ||
		strings.Contains(u, "TOKEN") ||
		strings.Contains(u, "SECRET") ||
		strings.Contains(u, "CRED") ||
		strings.Contains(u, "APIKEY") ||
		strings.HasSuffix(u, "_KEY")
}

// toolEnv returns os.Environ() with every secret key removed — the
// environment every spawned tool should run with. Keeping everything
// else (PATH, HOME, TMPDIR, locale, GPU runtime vars) means no tool
// loses an env var it legitimately needs; only credentials are
// dropped, and the credential set is small and fully known.
func toolEnv() []string {
	all := os.Environ()
	out := make([]string, 0, len(all))
	for _, kv := range all {
		i := strings.IndexByte(kv, '=')
		if i > 0 && secretEnvKey(kv[:i]) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// prepareTool applies the scrubbed environment to cmd unless the
// caller already set one. Call on every exec.Command before running.
// Returns cmd for one-line chaining:
//
//	out, err := services.prepareTool(cmd).CombinedOutput()
func prepareTool(cmd *exec.Cmd) *exec.Cmd {
	if cmd.Env == nil {
		cmd.Env = toolEnv()
	}
	return cmd
}

// ToolFailureSink, when set from cmd/agent (wired to
// client.SiteClient.PostLog), forwards a child-tool failure to the
// site's agent_logs. nil = local log only. Package-level so the ~15
// free functions that spawn tools don't each need a client handle
// threaded through — same pattern as loon's schedule.LogSink.
var ToolFailureSink func(level, message string)

// exitSignal returns the signal description ("signal: segmentation
// fault") when err is a signal-kill exit, or "" otherwise. Parsed
// from the error string so it stays portable across GOOS without a
// build-tagged syscall.WaitStatus decode.
func exitSignal(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.Index(s, "signal:"); i >= 0 {
		return strings.TrimSpace(s[i:])
	}
	return ""
}

// tailStr returns the last n bytes of s, prefixed with an ellipsis
// when truncated, so a report carries the tool's most-recent stderr
// without flooding the log/site.
func tailStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// escalateToolCrash reports a child-tool SIGNAL KILL (SIGSEGV etc. —
// the crash-dump class) to local logs and the site's agent_logs, so a
// segfaulting ffmpeg/par2/tesseract is visible in the admin dashboard
// instead of vanishing into container stdout nobody collects.
//
// It is a NO-OP for a nil error and for ordinary non-zero exits (those
// keep whatever per-call handling the site already has), so it is safe
// to add at any exec site without changing existing behavior — it only
// adds crash visibility. tool is the binary name ("ffmpeg"), target
// the file it was working on, out its combined output (may be nil).
func escalateToolCrash(tool, target string, out []byte, err error) {
	sig := exitSignal(err)
	if sig == "" {
		return
	}
	msg := fmt.Sprintf("%s CRASHED (%s) on %q. output: %s",
		tool, sig, target, tailStr(string(out), 600))
	log.Printf("SECURITY/tool-crash: %s", msg)
	if ToolFailureSink != nil {
		ToolFailureSink("error", msg)
	}
}

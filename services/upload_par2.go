package services

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PAR2ProgressFunc is called with the current phase ("Scanning", "Computing",
// "Creating recovery") and percentage (0-100) as the par2 tool writes progress
// lines to stdout. The callback fires from a background goroutine.
type PAR2ProgressFunc func(phase string, pct float64)

var (
	par2Once     sync.Once
	par2Bin      string
	par2Method   string // resolved parpar --method ("" = parpar's own auto-select)
	par2ForcedMu sync.Mutex
	par2Forced   string // PAR2_METHOD override; skips the ladder
)

// SetPAR2Method pins parpar's GF16 kernel instead of probing for one, for an
// operator who knows their hardware or needs to work around a bad kernel we
// haven't seen. Empty (the default) means probe. Call before the first par2 job.
func SetPAR2Method(m string) {
	par2ForcedMu.Lock()
	defer par2ForcedMu.Unlock()
	par2Forced = strings.TrimSpace(m)
}

// par2MethodLadder is probed in descending order of expected throughput; the
// first kernel that actually runs on this CPU wins.
//
// It exists because "the CPU supports this instruction set" and "this kernel
// runs on this CPU" turned out to be different claims. Production is a Xeon
// Gold 6140 (Skylake-SP): parpar's own detection correctly picks Shuffle
// (AVX512), which the CPU implements, and the kernel dies on SIGILL regardless.
// Since the agent is fleet software running on hardware we don't control, the
// only trustworthy answer is empirical — ask the machine, don't infer.
//
// "" first: parpar's auto-select is the best choice wherever it works, and it
// tunes more than we can express here (loop tiling, thread count).
var par2MethodLadder = []string{
	"",               // parpar's own auto-select
	"xorjit-avx512",  // AVX512BW
	"shuffle-avx512", // AVX512BW
	"xorjit-avx2",    // AVX2
	"shuffle-avx2",   // AVX2
	"shuffle-sse",    // SSSE3
	"lookup",         // scalar; no SIMD, runs anywhere
}

// par2Binary resolves, once, which par2 implementation to use.
//
// Deliberately lazy rather than a package-level initializer: the probes below
// run real par2 jobs, and a mismatched parpar dies on SIGILL. Resolving at init
// would fork those children before main() reaches disableCoreDumps(), and a
// crash dump of a child in a content tree is exactly what published our NNTP
// password to Usenet once already.
func par2Binary() string {
	par2Once.Do(func() { par2Bin = detectPAR2Binary() })
	return par2Bin
}

// par2ResolvedMethod returns the kernel chosen by detectPAR2Binary.
func par2ResolvedMethod() string {
	par2Binary()
	return par2Method
}

func detectPAR2Binary() string {
	// parpar is a parallel PAR2 implementation — dramatically faster on
	// multi-core systems, and it handles non-ASCII filenames correctly where
	// par2cmdline mangles them. Worth some effort to keep.
	if path, err := exec.LookPath("parpar"); err == nil {
		if m, err := resolveParparMethod(); err == nil {
			par2Method = m
			log.Printf("PAR2: using parpar (%s) with method %s — multi-threaded",
				path, methodLabel(m))
			return "parpar"
		} else {
			log.Printf("PAR2: parpar (%s) is installed but no GF16 kernel runs on this CPU (%v). "+
				"Falling back to par2create — recovery data is still generated, but "+
				"single-threaded, and non-ASCII filenames may be mangled in the PAR2 header.",
				path, err)
		}
	}
	if path, err := exec.LookPath("par2create"); err == nil {
		log.Printf("PAR2: using par2create (%s) — single-threaded", path)
		return "par2create"
	}
	log.Println("PAR2: WARNING — no par2 binary found in PATH")
	return "par2create" // will fail at exec time with a clear error
}

// resolveParparMethod walks the ladder and returns the first kernel that
// survives a real par2 run on this machine.
func resolveParparMethod() (string, error) {
	par2ForcedMu.Lock()
	forced := par2Forced
	par2ForcedMu.Unlock()

	ladder := par2MethodLadder
	if forced != "" {
		ladder = []string{forced} // operator's call: no silent second-guessing
	}

	var failures []string
	for _, m := range ladder {
		err := smokePAR2("parpar", m)
		if err == nil {
			if len(failures) > 0 {
				// Not an error, but the operator wants to know their fastest
				// kernel is unusable — it points at a bad build or a CPU we
				// should ladder differently.
				log.Printf("PAR2: %d faster parpar kernel(s) failed on this CPU before %s worked: %s",
					len(failures), methodLabel(m), strings.Join(failures, "; "))
			}
			return m, nil
		}
		failures = append(failures, fmt.Sprintf("%s (%v)", methodLabel(m), err))
	}
	return "", fmt.Errorf("%s", strings.Join(failures, "; "))
}

func methodLabel(m string) string {
	if m == "" {
		return "auto"
	}
	return m
}

// smokePAR2 proves the binary can actually produce recovery data on THIS CPU,
// rather than merely existing on it.
//
// LookPath alone is not enough. parpar dispatches hand-written SIMD kernels off
// runtime CPU detection; a build whose kernels don't match the host takes SIGILL
// the instant it reaches the GF16 hot path, while `parpar --version` still exits
// 0 because it never executes one. So LookPath happily selected a binary that
// failed every single job — and since par2 failure is non-fatal by design, every
// release shipped to Usenet with no recovery at all and nothing looked broken.
// Probing the real code path is the only check that would have caught it.
func smokePAR2(bin string, method string) error {
	dir, err := os.MkdirTemp("", "par2probe")
	if err != nil {
		return fmt.Errorf("probe tempdir: %w", err)
	}
	defer os.RemoveAll(dir)

	// Real data, not zeros: the kernels we need to exercise only run once
	// there is something to compute over.
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := os.WriteFile(filepath.Join(dir, "probe.bin"), payload, 0o644); err != nil {
		return fmt.Errorf("probe file: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts := PAR2Options{Redundancy: 5, BlockSize: 64 * 1024, Method: method}
	var cmd *exec.Cmd
	if bin == "parpar" {
		cmd = buildParparCmd(ctx, dir, "probe", opts, []string{"probe.bin"})
	} else {
		cmd = buildPar2createCmd(ctx, dir, "probe", opts.Redundancy, opts.BlockSize, []string{"probe.bin"})
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, tailStr(string(out), 200))
	}
	// Exit 0 alone isn't proof — require recovery files on disk.
	hits, _ := filepath.Glob(filepath.Join(dir, "probe*.par2"))
	if len(hits) == 0 {
		return fmt.Errorf("exited 0 but produced no .par2 files")
	}
	return nil
}

// maxPAR2Slices is the PAR2 specification's ceiling on input slices. Both
// parpar and par2create refuse to run past it, so the slice size — not the
// slice count — is what has to give on a large release.
const maxPAR2Slices = 32768

// PAR2Options controls PAR2 generation parameters.
type PAR2Options struct {
	Redundancy int    // recovery percentage (default 5)
	BlockSize  int    // bytes per block (default 700KB = article size)
	Threads    int    // 0 = all cores, >0 = limit (parpar only)
	MemoryMB   int    // 0 = auto, >0 = cap in MB (parpar only)
	Method     string // parpar --method; "" = parpar's auto-select. Normally
	// left empty by callers and filled in by GeneratePAR2 from the probed
	// ladder — set it only to override for one call (the probe does).
}

// GeneratePAR2 creates PAR2 recovery files for all files in the given directory.
// Prefers parpar (multi-threaded) when available, falls back to par2create.
// Returns the list of generated PAR2 file paths.
func GeneratePAR2(ctx context.Context, dir string, baseName string, opts PAR2Options, progressFn PAR2ProgressFunc) ([]string, error) {
	binary := par2Binary()
	log.Printf("PAR2: GeneratePAR2 entry dir=%q baseName=%q redundancy=%d%% blockSize=%d threads=%d memMB=%d binary=%s",
		dir, baseName, opts.Redundancy, opts.BlockSize, opts.Threads, opts.MemoryMB, binary)
	if opts.Redundancy <= 0 {
		opts.Redundancy = 5
	}
	if opts.BlockSize <= 0 {
		opts.BlockSize = 700 * 1024
	}

	// Collect all files to protect.
	var files []string
	var totalSize int64
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if strings.HasSuffix(strings.ToLower(path), ".par2") {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		files = append(files, rel)
		totalSize += info.Size()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk dir: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files found in %s", dir)
	}

	if scaled := fitBlockSize(totalSize, opts.BlockSize); scaled != opts.BlockSize {
		log.Printf("PAR2: %d slices at %dB exceeds the %d-slice PAR2 limit — scaling slice size to %dB",
			totalSize/int64(opts.BlockSize), opts.BlockSize, maxPAR2Slices, scaled)
		opts.BlockSize = scaled
	}

	log.Printf("PAR2: generating %d%% recovery for %d files (%.1f MB total), base=%s, binary=%s, slice=%dB",
		opts.Redundancy, len(files), float64(totalSize)/1024/1024, baseName, binary, opts.BlockSize)

	// 60-minute cap is a backstop, not a target — most PAR2 runs complete
	// in seconds-to-minutes. Protects against a wedged par2create/parpar
	// holding an upload slot forever.
	par2Ctx, parCancel := context.WithTimeout(ctx, 60*time.Minute)
	defer parCancel()

	var cmd *exec.Cmd
	if binary == "parpar" {
		if opts.Method == "" {
			opts.Method = par2ResolvedMethod()
		}
		cmd = buildParparCmd(par2Ctx, dir, baseName, opts, files)
	} else {
		cmd = buildPar2createCmd(par2Ctx, dir, baseName, opts.Redundancy, opts.BlockSize, files)
	}

	// Pipe stdout so we can parse progress lines in real time.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout // merge stderr into the same pipe

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("par2 start: %w", err)
	}

	// Parse progress in a goroutine so we don't block the command.
	var lastOutput strings.Builder
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		parsePAR2Progress(stdout, &lastOutput, progressFn)
	}()

	// Drain the pipe BEFORE reaping the process: cmd.Wait closes the stdout
	// pipe once the child exits, so waiting on it first can cut the reader
	// off mid-scan and empty out lastOutput — the one diagnostic that matters
	// when par2 dies. os/exec documents this ordering as required.
	wg.Wait()
	err = cmd.Wait()

	if err != nil {
		escalateToolCrash(binary, dir, []byte(lastOutput.String()), err)
		log.Printf("PAR2 output:\n%s", lastOutput.String())
		return nil, fmt.Errorf("par2 create failed: %w", err)
	}

	// Collect generated PAR2 files.
	var par2Files []string
	var par2Size int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(strings.ToLower(e.Name()), ".par2") {
			par2Files = append(par2Files, filepath.Join(dir, e.Name()))
			if info, err := e.Info(); err == nil {
				par2Size += info.Size()
			}
		}
	}
	log.Printf("PAR2: done — %d recovery files (%.1f MB)", len(par2Files), float64(par2Size)/1024/1024)
	return par2Files, nil
}

// fitBlockSize returns a slice size that keeps totalSize under the PAR2
// input-slice ceiling, or want unchanged if it already fits.
//
// Callers pass the Usenet article size (700KB) as the slice size, but the two
// are unrelated: article size is a posting constraint, slice size is a PAR2
// one. Holding the slice at 700KB silently caps a release at
// 32768*700KB ≈ 21.9 GiB — past that the tool exits non-zero and the release
// ships with no recovery at all. Growing the slice is what par2 tooling does
// by default; the spec requires a multiple of 4.
func fitBlockSize(totalSize int64, want int) int {
	if want <= 0 || totalSize <= 0 || totalSize/int64(want) <= maxPAR2Slices {
		return want
	}
	scaled := (totalSize + maxPAR2Slices - 1) / maxPAR2Slices
	return int(((scaled + 3) / 4) * 4)
}

// buildPar2createCmd builds the exec.Cmd for the traditional par2create binary.
//
// par2cmdline has known bugs with non-ASCII filenames stored in the PAR2
// header (the spec mandates UTF-16LE; some par2cmdline builds truncate or
// mojibake). parpar handles it correctly, so the agent prefers parpar at
// startup. When the fallback path runs against any non-ASCII filename we
// log a one-line warning so the operator can investigate verify failures
// downstream rather than wondering where the corruption came from.
func buildPar2createCmd(ctx context.Context, dir, baseName string, redundancy, blockSize int, files []string) *exec.Cmd {
	for _, f := range files {
		if !isASCII(f) {
			log.Printf("PAR2: par2create may mangle non-ASCII filename %q in the header (parpar handles correctly — install it for clean CJK support)", f)
			break
		}
	}
	args := []string{
		fmt.Sprintf("-r%d", redundancy),
		fmt.Sprintf("-s%d", blockSize),
		baseName + ".par2",
	}
	args = append(args, files...)
	cmd := exec.CommandContext(ctx, "par2create", args...)
	cmd.Dir = dir
	cmd.Env = toolEnv()
	return cmd
}

// isASCII reports whether every byte in s is in the 0x00-0x7F range.
// Used by buildPar2createCmd to detect filenames that would trip the
// par2cmdline header bug.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7F {
			return false
		}
	}
	return true
}

// buildParparCmd builds the exec.Cmd for the multi-threaded parpar binary.
// parpar uses different flag names than par2create — see `parpar --help`.
func buildParparCmd(ctx context.Context, dir, baseName string, opts PAR2Options, files []string) *exec.Cmd {
	args := []string{
		"-s", fmt.Sprintf("%dB", opts.BlockSize), // --input-slices with byte suffix
		"-r", fmt.Sprintf("%d%%", opts.Redundancy), // --recovery-slices as percentage
		"-o", baseName + ".par2", // --out (relative to cmd.Dir)
		"-O", // --overwrite
	}
	if opts.Threads > 0 {
		args = append(args, "-t", fmt.Sprintf("%d", opts.Threads))
	}
	if opts.MemoryMB > 0 {
		args = append(args, "-m", fmt.Sprintf("%dM", opts.MemoryMB))
	}
	if opts.Method != "" {
		// parpar's own help: "Process can crash if CPU does not support
		// selected method." Only ever set from a probed/forced value.
		args = append(args, "--method", opts.Method)
	}
	args = append(args, "--")
	args = append(args, files...)
	cmd := exec.CommandContext(ctx, "parpar", args...)
	cmd.Dir = dir
	cmd.Env = toolEnv()
	return cmd
}

// rePAR2Pct matches percentage output from par2create. Handles both:
//
//	"Processing: 12.3%"
//	"Creating recovery file(s): 45.6%"
//	"Verifying: 78.9%"
var rePAR2Pct = regexp.MustCompile(`^(.+?):\s+([\d.]+)%`)

// parsePAR2Progress reads par2create output line-by-line, fires the callback
// on every percentage update, and captures all output in lastOutput for error
// reporting. par2create uses \r for in-place progress on a terminal; we split
// on both \r and \n so we catch every update.
func parsePAR2Progress(r io.Reader, lastOutput *strings.Builder, fn PAR2ProgressFunc) {
	// par2create mixes \r (in-place overwrite) and \n in its output. We
	// need to split on both to catch every progress line. bufio.Scanner
	// only splits on \n, so we use a custom split function.
	scanner := bufio.NewScanner(r)
	scanner.Split(scanLinesOrCR)

	// Throttle callbacks to avoid flooding the status channel. Once every
	// 2 seconds is enough for the dashboard's 5-second poll interval.
	var lastCallback time.Time

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lastOutput.WriteString(line)
		lastOutput.WriteByte('\n')

		if fn == nil {
			continue
		}
		m := rePAR2Pct.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		pct, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			continue
		}
		phase := strings.TrimSpace(m[1])

		now := time.Now()
		if now.Sub(lastCallback) >= 2*time.Second || pct >= 99.9 {
			fn(phase, pct)
			lastCallback = now
		}
	}
}

// scanLinesOrCR is a bufio.SplitFunc that splits on \n, \r\n, or bare \r.
// This handles par2create's use of \r for in-place terminal progress.
func scanLinesOrCR(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			return i + 1, data[:i], nil
		}
		if data[i] == '\r' {
			// Check for \r\n
			if i+1 < len(data) && data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

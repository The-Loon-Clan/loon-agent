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

// par2Binary caches the resolved binary name at startup so we only probe once.
// Prefers parpar (multi-threaded, 4-8x faster) over par2create (single-threaded).
var par2Binary = detectPAR2Binary()

func detectPAR2Binary() string {
	// parpar is a parallel PAR2 implementation — dramatically faster on
	// multi-core systems. If it's installed, use it.
	if path, err := exec.LookPath("parpar"); err == nil {
		log.Printf("PAR2: using parpar (%s) — multi-threaded", path)
		return "parpar"
	}
	if path, err := exec.LookPath("par2create"); err == nil {
		log.Printf("PAR2: using par2create (%s) — single-threaded", path)
		return "par2create"
	}
	log.Println("PAR2: WARNING — no par2 binary found in PATH")
	return "par2create" // will fail at exec time with a clear error
}

// maxPAR2Slices is the PAR2 specification's ceiling on input slices. Both
// parpar and par2create refuse to run past it, so the slice size — not the
// slice count — is what has to give on a large release.
const maxPAR2Slices = 32768

// PAR2Options controls PAR2 generation parameters.
type PAR2Options struct {
	Redundancy int // recovery percentage (default 5)
	BlockSize  int // bytes per block (default 700KB = article size)
	Threads    int // 0 = all cores, >0 = limit (parpar only)
	MemoryMB   int // 0 = auto, >0 = cap in MB (parpar only)
}

// GeneratePAR2 creates PAR2 recovery files for all files in the given directory.
// Prefers parpar (multi-threaded) when available, falls back to par2create.
// Returns the list of generated PAR2 file paths.
func GeneratePAR2(ctx context.Context, dir string, baseName string, opts PAR2Options, progressFn PAR2ProgressFunc) ([]string, error) {
	log.Printf("PAR2: GeneratePAR2 entry dir=%q baseName=%q redundancy=%d%% blockSize=%d threads=%d memMB=%d binary=%s",
		dir, baseName, opts.Redundancy, opts.BlockSize, opts.Threads, opts.MemoryMB, par2Binary)
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
		opts.Redundancy, len(files), float64(totalSize)/1024/1024, baseName, par2Binary, opts.BlockSize)

	// 60-minute cap is a backstop, not a target — most PAR2 runs complete
	// in seconds-to-minutes. Protects against a wedged par2create/parpar
	// holding an upload slot forever.
	par2Ctx, parCancel := context.WithTimeout(ctx, 60*time.Minute)
	defer parCancel()

	var cmd *exec.Cmd
	if par2Binary == "parpar" {
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
		escalateToolCrash(par2Binary, dir, []byte(lastOutput.String()), err)
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

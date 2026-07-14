package services

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"github.com/ameNZB/loon-agent/config"
	"github.com/ameNZB/loon-agent/storage"
	"github.com/ameNZB/loon-agent/utils"
	"hash/crc32"
	"io"
	"log"
	"mime"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/idna"
)

const ChunkSize = 700 * 1024 // 700KB — Nyuu/Usenet standard article size

// UploadSlot serialises Usenet uploads across the whole agent so the
// online (site-polling) and offline (watch-folder) paths share NNTP
// connection budget. Callers acquire the lock before UploadDirectory
// and release it after — holding it across progress reporting keeps the
// UI phase ("queued for upload" vs "uploading") accurate.
var UploadSlot sync.Mutex

// bufPool reuses chunk read buffers to avoid per-chunk allocation.
var bufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, ChunkSize)
		return &b
	},
}

// yencPool reuses yEnc output buffers. Encoded output is roughly 1-2% larger
// than input due to escaping, plus ~200 bytes of headers/trailer.
var yencPool = sync.Pool{
	New: func() interface{} {
		b := bytes.NewBuffer(make([]byte, 0, ChunkSize+ChunkSize/50+256))
		return b
	},
}

// fileCRCState tracks the per-part CRC-32/IEEE values for a single
// multi-part yEnc upload so the FINAL part's =yend trailer can carry
// the whole-file crc32 attribute (yEnc 1.3 §5.4). Some decoders
// (sabnzbd, nzbget) downgrade verification to a best-effort check
// when this is missing; pickier tools refuse entirely.
//
// Production workers may invoke yEncodeChunk for a single file in any
// order (jobs are dispatched in part-order on a single goroutine but
// consumed by N parallel workers), so we cannot maintain a running
// hash.Hash32 directly — CRC-32 is byte-position-dependent. Instead
// each call records its part's pcrc + length, and the FINAL part
// waits on cond for all earlier parts to register, then combines
// them in part-order via crc32Combine to produce the whole-file CRC.
//
// Keyed by (filename, totalSize) so concurrent uploads of different
// files do not collide; the entry is deleted once the final part has
// emitted, so a later upload of the same (filename, totalSize) pair
// starts with fresh state.
type fileCRCState struct {
	mu    sync.Mutex
	cond  *sync.Cond
	parts map[int]partCRCInfo
}

type partCRCInfo struct {
	pcrc   uint32
	length int64
}

var fileCRCStates sync.Map // map[string]*fileCRCState

// fileCRCKey is the (filename, totalSize) tuple key. Both fields are
// included because two unrelated uploads can race with identical
// filenames (different sizes) — e.g. two different episodes named
// "01.mkv" being prepared in parallel.
func fileCRCKey(filename string, totalSize int64) string {
	return filename + "\x00" + strconv.FormatInt(totalSize, 10)
}

func loadOrCreateFileCRCState(key string) *fileCRCState {
	if v, ok := fileCRCStates.Load(key); ok {
		return v.(*fileCRCState)
	}
	s := &fileCRCState{parts: make(map[int]partCRCInfo)}
	s.cond = sync.NewCond(&s.mu)
	actual, _ := fileCRCStates.LoadOrStore(key, s)
	return actual.(*fileCRCState)
}

type UploadJob struct {
	ChunkData   []byte
	Number      int
	TotalParts  int
	Subject     string
	FileName    string
	ChunkOffset int64
	TotalSize   int64
	// JobName is the request-scoped log prefix (e.g. "request-1234" or
	// the watch-folder job name). Threaded into nntpWorker / uploadChunk
	// so every per-chunk log line ("POST chunk N", "Uploaded chunk N",
	// "Chunk N upload failed", "FATAL: chunk N…") can be correlated
	// back to a specific task when several uploads interleave.
	JobName string
}

type NZBSegment struct {
	Bytes     int    `xml:"bytes,attr"`
	Number    int    `xml:"number,attr"`
	MessageID string `xml:",chardata"`
}

// UploadDirectory uploads all files in a directory to Usenet and returns
// per-file segment lists suitable for NZB generation.
//
// Every article is posted with a CANONICAL yEnc subject —
// `<release> [i/F] - "name" yEnc (n/P)` — so any indexer (a third party, or our
// own crawler) can group the parts and rebuild the release from the newsgroup
// alone; this is the round-trip partner of the usenet plugin's parseSubject.
// releaseName is the shared "base" before the [i/F] marker that groups the
// files. Privacy is a NAME concern, not a structure one: when cfg.Obfuscate is
// set the release base + per-file names become random tokens (and the yEnc body
// name matches), so the posts stay rebuildable but reveal nothing; otherwise the
// real title/filenames are used (full interop).
func UploadDirectory(ctx context.Context, cfg *config.Config, dir string, releaseName string, jobName string) ([]FileSegments, error) {
	var files []string
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Size() == 0 {
			return err
		}
		files = append(files, path)
		return nil
	})
	if len(files) == 0 {
		return nil, fmt.Errorf("no files to upload in %s", dir)
	}
	fileCount := len(files)

	// Calculate total size across all files for cumulative progress.
	var totalDirSize int64
	for _, f := range files {
		if info, err := os.Stat(f); err == nil {
			totalDirSize += info.Size()
		}
	}
	var cumulativeUploaded int64

	// The release base is shared by every file's subject (the text before [i/F])
	// so a crawler groups them into one release. Real title for interop; one
	// random token per release when obfuscating.
	releaseBase := subjectSafe(releaseName)
	if cfg.Obfuscate {
		releaseBase = GenerateRandomPassword(16)
	}

	var allFiles []FileSegments
	for i, filePath := range files {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Use relative path so NZB preserves subdirectory structure.
		relName, _ := filepath.Rel(dir, filePath)
		relName = filepath.ToSlash(relName) // normalize to forward slashes for NZB

		// The name shown in the subject + written into the yEnc body. Obfuscate
		// keeps the extension so clients still recognise the type.
		postName := filepath.Base(relName)
		if cfg.Obfuscate {
			postName = GenerateRandomPassword(16) + filepath.Ext(postName)
		}

		storage.UpdateState(jobName, "Uploading",
			fmt.Sprintf("File %d/%d: %s", i+1, fileCount, relName), 0)

		// Canonical subject up to the segment marker; UploadToUsenet appends
		// " (n/P)" per chunk.
		subjectPrefix := fmt.Sprintf(`%s [%d/%d] - "%s" yEnc`, releaseBase, i+1, fileCount, subjectSafe(postName))

		segments, err := UploadToUsenet(ctx, cfg, filePath, subjectPrefix, postName, jobName, cumulativeUploaded, totalDirSize)
		if err != nil {
			return nil, fmt.Errorf("upload %s: %w", relName, err)
		}

		// Track cumulative bytes for next file's progress offset.
		if info, err := os.Stat(filePath); err == nil {
			cumulativeUploaded += info.Size()
		}

		// The NZB always records the REAL name + a canonical first-segment
		// subject, so the site (and any NZB consumer) sees the real release even
		// when the posts are name-obfuscated. The <segments> carry the message-ids.
		nzbSubject := fmt.Sprintf(`%s [%d/%d] - "%s" yEnc (1/%d)`,
			subjectSafe(releaseName), i+1, fileCount, relName, len(segments))

		allFiles = append(allFiles, FileSegments{
			FileName: nzbSubject,
			Segments: segments,
		})
	}
	return allFiles, nil
}

// subjectSafe strips characters that would confuse a yEnc subject parser
// (brackets, parens, quotes) and collapses whitespace, so a release title used
// as the subject base can't inject a spurious [i/j] or (n/m) marker. Empty input
// falls back to a placeholder so the subject is never malformed.
func subjectSafe(s string) string {
	s = strings.NewReplacer(
		"[", " ", "]", " ", "(", " ", ")", " ", `"`, " ",
		"\r", " ", "\n", " ", "\t", " ",
	).Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "release"
	}
	return s
}

// UploadToUsenet chunks the file and uploads it using a worker pool.
//
// subjectPrefix is the canonical yEnc subject up to (but not including) the
// segment marker — `<release> [i/F] - "<name>" yEnc` — to which each chunk
// appends its own ` (n/P)`. embedName is the filename written into the yEnc
// `=ybegin name=` header; it equals the subject's quoted name so the article
// body and its subject agree (both real, or both obfuscated).
func UploadToUsenet(ctx context.Context, cfg *config.Config, filePath string, subjectPrefix string, embedName string, jobName string, cumulativeBytes int64, totalDirSize int64) ([]NZBSegment, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	stat, _ := file.Stat()
	totalChunks := int((stat.Size() + ChunkSize - 1) / ChunkSize)

	// Fixed-size channel buffers prevent unbounded memory growth on large files.
	workerCount := cfg.NNTPConnections
	jobs := make(chan UploadJob, workerCount*2)
	results := make(chan NZBSegment, workerCount*2)
	errs := make(chan error, 1) // first fatal error

	var wg sync.WaitGroup

	// Start workers.
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go nntpWorker(ctx, cfg, jobs, results, errs, &wg)
	}

	// Dispatch jobs (read file in chunks).
	go func() {
		defer close(jobs)
		fileName := embedName
		for i := 1; i <= totalChunks; i++ {
			if ctx.Err() != nil {
				return
			}

			// Get a buffer from the pool.
			bp := bufPool.Get().(*[]byte)
			buffer := *bp
			n, err := file.Read(buffer)
			if err != nil && err != io.EOF {
				select {
				case errs <- fmt.Errorf("read chunk %d: %v", i, err):
				default:
				}
				bufPool.Put(bp)
				return
			}
			if n == 0 {
				bufPool.Put(bp)
				break
			}

			// Copy the data out so we can return the buffer to the pool.
			// The copy is needed because the worker will hold ChunkData
			// until upload completes, while we reuse the read buffer.
			chunkData := make([]byte, n)
			copy(chunkData, buffer[:n])
			bufPool.Put(bp)

			offset := int64(i-1) * ChunkSize
			jobs <- UploadJob{
				ChunkData:   chunkData,
				Number:      i,
				TotalParts:  totalChunks,
				Subject:     fmt.Sprintf("%s (%d/%d)", subjectPrefix, i, totalChunks),
				FileName:    fileName,
				ChunkOffset: offset,
				TotalSize:   stat.Size(),
				JobName:     jobName,
			}
		}
	}()

	// Close results when all workers finish.
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect segments.
	var segments []NZBSegment
	uploadedCount := 0
	var uploadedBytes int64
	startTime := time.Now()

	for seg := range results {
		if ctx.Err() != nil {
			continue // drain channel
		}

		segments = append(segments, seg)
		uploadedCount++
		uploadedBytes += int64(seg.Bytes)

		// Per-file percent for state string.
		filePercent := float64(uploadedCount) / float64(totalChunks) * 100

		// Cumulative percent across all files in the directory.
		var overallPercent float64
		if totalDirSize > 0 {
			overallPercent = float64(cumulativeBytes+uploadedBytes) / float64(totalDirSize) * 100
		} else {
			overallPercent = filePercent
		}

		elapsed := time.Since(startTime).Seconds()
		speed := 0.0
		if elapsed > 0 {
			speed = float64(uploadedBytes) / elapsed / 1024 / 1024
		}

		var etaSeconds float64
		etaStr := "Calculating..."
		if speed > 0 {
			var remainingMB float64
			if totalDirSize > 0 {
				remainingMB = float64(totalDirSize-cumulativeBytes-uploadedBytes) / 1024 / 1024
			} else {
				remainingMB = (float64(totalChunks*ChunkSize) - float64(uploadedBytes)) / 1024 / 1024
			}
			etaSeconds = remainingMB / speed
			etaStr = utils.FormatETA(etaSeconds)
		}

		storage.UpdateState(jobName, "Uploading", fmt.Sprintf("%.1f%% - %.2f MB/s - ETA: %s", overallPercent, speed, etaStr), overallPercent)

		if cb := GetProgressCallback(jobName); cb != nil {
			// Total = totalDirSize when known; transferred = cumulative
			// across already-finished files + bytes pushed this iter.
			var total int64
			if totalDirSize > 0 {
				total = totalDirSize
			} else {
				total = int64(totalChunks) * int64(ChunkSize)
			}
			cb(0, speed, overallPercent, "uploading", 0, total, cumulativeBytes+uploadedBytes, etaSeconds, nil)
		}
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Check for worker errors.
	select {
	case err := <-errs:
		return nil, err
	default:
	}

	return segments, nil
}

func nntpWorker(ctx context.Context, cfg *config.Config, jobs <-chan UploadJob, results chan<- NZBSegment, errs chan<- error, wg *sync.WaitGroup) {
	defer wg.Done()

	var conn *nntpConn

	// The job-pull select makes ctx cancel preempt jobs <-chan reads,
	// so a SIGTERM-mid-pipeline drops out of the worker within one
	// scheduler tick instead of waiting for the producer to close
	// the channel. Pair it with the per-attempt ctx checks below
	// so a wedged provider mid-retry also exits within the
	// shutdown budget.
	for {
		var job UploadJob
		select {
		case <-ctx.Done():
			if conn != nil {
				conn.Close()
			}
			return
		case j, ok := <-jobs:
			if !ok {
				// Channel closed by producer — drain and exit.
				if conn != nil {
					conn.withTimeout(5 * time.Second)
					if err := conn.text.PrintfLine("QUIT"); err != nil {
						log.Printf("nntp: QUIT failed (non-fatal): %v", err)
					}
					conn.Close()
				}
				return
			}
			job = j
		}

		// Generate a unique Message-ID. The local part is 32 hex chars
		// (UUID with dashes stripped) — same entropy as the dashed
		// form but more conventional on the wire and friendlier to
		// the few legacy NNTP servers that get unhappy with hyphens
		// in the local part of a Message-ID.
		domain := cfg.NNTPDomain
		if domain == "" {
			if parts := strings.Split(cfg.NNTPFrom, "@"); len(parts) == 2 {
				domain = parts[1]
			} else {
				domain = "example.com"
			}
		}
		// RFC 5536 §3.1.3: Message-IDs MUST be ASCII. Punycode any
		// IDN domain (e.g. "蝶龙.com" → "xn--7csv2g2gua.com") so the
		// resulting header passes strict-validating relays. idna.ToASCII
		// is a no-op for already-ASCII domains.
		if asciiDomain, err := idna.Lookup.ToASCII(domain); err == nil {
			domain = asciiDomain
		} else {
			log.Printf("upload: Message-ID domain %q failed IDN→ASCII (%v); using as-is — relay may reject", domain, err)
		}
		messageID := fmt.Sprintf("%s@%s", strings.ReplaceAll(uuid.New().String(), "-", ""), domain)

		// yEnc encode the chunk using a pooled buffer.
		encodedData := yEncodeChunk(job.ChunkData, job.FileName, job.Number, job.TotalParts, job.ChunkOffset, job.TotalSize)

		maxRetries := 3
		success := false

		for attempt := 1; attempt <= maxRetries; attempt++ {
			// Bail mid-retry on shutdown — without this a worker
			// wedged on a 120s nntpOpTimeout could chew up to
			// ~6 minutes of shutdown budget (3 attempts × 120s
			// timeout + backoff) before the outer-loop check
			// at the next job-pull fires.
			if ctx.Err() != nil {
				if conn != nil {
					conn.Close()
				}
				return
			}
			if conn == nil {
				var err error
				conn, err = connectNNTP(cfg)
				if err != nil {
					log.Printf("[%s] Worker connection failed (attempt %d): %v", job.JobName, attempt, err)
					// Auth errors are permanent — retrying just burns
					// the backoff budget on the same misconfiguration.
					// Fail fast so the operator sees the real reason
					// (bad NNTP_USER / NNTP_PASS) within seconds
					// instead of minutes.
					if isNNTPAuthError(err) {
						log.Printf("[%s] nntp: auth error (%v) — failing fast, no retry", job.JobName, err)
						select {
						case errs <- fmt.Errorf("nntp auth failed: %w", err):
						default:
						}
						return
					}
					if !backoffCtx(ctx, attempt) {
						if conn != nil {
							conn.Close()
						}
						return
					}
					continue
				}
			}

			err := uploadChunk(cfg, conn, job, messageID, encodedData)
			if err != nil {
				log.Printf("[%s] Chunk %d upload failed (attempt %d): %v", job.JobName, job.Number, attempt, err)
				conn.Close()
				conn = nil
				// Same fail-fast logic for auth errors surfaced mid-
				// session (e.g. provider revoked the session or the
				// account hit a posting ban): retry can't recover and
				// is just noise in the logs.
				if isNNTPAuthError(err) {
					log.Printf("[%s] nntp: auth error during upload (%v) — failing fast, no retry", job.JobName, err)
					select {
					case errs <- fmt.Errorf("nntp auth failed: %w", err):
					default:
					}
					return
				}
				if !backoffCtx(ctx, attempt) {
					return
				}
				continue
			}

			success = true
			break
		}

		if !success {
			err := fmt.Errorf("chunk %d failed after %d attempts", job.Number, maxRetries)
			log.Printf("[%s] FATAL: %v", job.JobName, err)
			select {
			case errs <- err:
			default:
			}
			return // exit worker, don't abort the entire process
		}

		log.Printf("[%s] Uploaded chunk %d - MsgID: %s", job.JobName, job.Number, messageID)

		results <- NZBSegment{
			Bytes:     len(job.ChunkData),
			Number:    job.Number,
			MessageID: messageID,
		}
	}
}

// isNNTPAuthError reports whether an NNTP error is a permanent auth
// failure (or a permission denial that no amount of retry can fix).
// The textproto package surfaces server replies as strings prefixed
// with the 3-digit response code, e.g. "481 Authentication failed".
// Matched codes:
//
//	401 — service unavailable to the client
//	403 — server policy refusal
//	480 — authentication required
//	481 — authentication failed / credentials rejected
//	482 — authentication out of sequence
//	502 — access restriction / permission denied (some providers
//	      use this for IP-blocked or expired accounts)
//
// 5xx codes outside the auth set, network errors, and i/o timeouts
// stay on the retry path — those are genuinely transient.
func isNNTPAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "401 ") ||
		strings.Contains(s, "403 ") ||
		strings.Contains(s, "480 ") ||
		strings.Contains(s, "481 ") ||
		strings.Contains(s, "482 ") ||
		strings.Contains(s, "502 ") ||
		strings.Contains(s, "NNTP Auth failed")
}

// backoff sleeps with exponential backoff: 2s, 4s, 8s.
func backoff(attempt int) {
	d := time.Duration(1<<uint(attempt)) * time.Second
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	time.Sleep(d)
}

// backoffCtx is the ctx-aware sibling of backoff. Returns true if
// the sleep completed normally, false if ctx was cancelled mid-
// sleep — callers use the false return to bail out of the retry
// loop instead of finishing the doomed attempt. Without this, a
// worker stuck in an 8-second backoff couldn't observe shutdown
// until the sleep expired, which violates the per-task 15-30s
// shutdown SLA when several backoffs stack.
func backoffCtx(ctx context.Context, attempt int) bool {
	d := time.Duration(1<<uint(attempt)) * time.Second
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// nntpConn pairs a textproto.Conn with the underlying net.Conn so the
// caller can SetDeadline on the socket before each exchange. textproto
// itself doesn't expose its conn, so without this wrapper there's no
// way to bound a stuck POST or ReadCodeLine — historically the worker
// pool deadlocked on unresponsive providers (TCP socket open, no FIN,
// server wedged) and burned through agent slots indefinitely.
type nntpConn struct {
	net  net.Conn
	text *textproto.Conn
}

// nntpDialTimeout / nntpOpTimeout cap the dial-and-handshake and the
// per-POST exchange respectively. 30s covers slow EU/US round trips
// without false-positive cancellations on a healthy connection;
// 120s for the POST itself is generous (a 700KB chunk over a slow
// link still finishes well inside that). If you need tighter SLAs,
// thread these through cfg later.
const (
	nntpDialTimeout = 30 * time.Second
	nntpOpTimeout   = 120 * time.Second
)

func (c *nntpConn) Close() error {
	if c == nil || c.text == nil {
		return nil
	}
	return c.text.Close()
}

// withTimeout sets a fresh deadline on the underlying socket so the
// next I/O op (Read or Write through textproto) can't hang forever.
// Caller invokes this before each ReadCodeLine / PrintfLine / DotWriter
// exchange — clearing the deadline isn't necessary; the next call sets
// a fresh one.
func (c *nntpConn) withTimeout(d time.Duration) {
	_ = c.net.SetDeadline(time.Now().Add(d))
}

func connectNNTP(cfg *config.Config) (*nntpConn, error) {
	var netConn net.Conn
	var err error

	dialer := &net.Dialer{Timeout: nntpDialTimeout}
	if cfg.NNTPSSL {
		netConn, err = tls.DialWithDialer(dialer, "tcp", cfg.NNTPServer, nil)
	} else {
		netConn, err = dialer.Dial("tcp", cfg.NNTPServer)
	}

	if err != nil {
		return nil, err
	}
	// One deadline covers banner + auth as a single handshake.
	_ = netConn.SetDeadline(time.Now().Add(nntpDialTimeout))

	conn := textproto.NewConn(netConn)
	c := &nntpConn{net: netConn, text: conn}

	if _, _, err = conn.ReadCodeLine(0); err != nil {
		c.Close()
		return nil, fmt.Errorf("read banner: %w", err)
	}

	if cfg.NNTPUser != "" {
		if err = conn.PrintfLine("AUTHINFO USER %s", cfg.NNTPUser); err != nil {
			c.Close()
			return nil, err
		}
		if _, _, err = conn.ReadCodeLine(381); err == nil {
			if err = conn.PrintfLine("AUTHINFO PASS %s", cfg.NNTPPass); err != nil {
				c.Close()
				return nil, err
			}
			if _, _, err = conn.ReadCodeLine(281); err != nil {
				c.Close()
				return nil, fmt.Errorf("NNTP Auth failed: %v", err)
			}
		}
	}
	// Connect-success log so the agent log carries a positive signal
	// that the worker pool actually established its NNTP session.
	// Previously only failures were logged — a successful connect was
	// silent, which made "is the worker even talking to the server?"
	// unanswerable from the logs alone.
	log.Printf("NNTP connected to %s", cfg.NNTPServer)
	return c, nil
}

func uploadChunk(cfg *config.Config, c *nntpConn, job UploadJob, messageID string, encodedData []byte) error {
	// One deadline covers the full POST exchange: command line, 340
	// ack, headers, body, dot, 240 ack. If anything in that chain
	// stalls past nntpOpTimeout the next syscall returns
	// `i/o timeout`, the worker's retry path closes the conn and
	// reconnects. Without this, a wedged provider would freeze the
	// worker forever and starve the upload pipeline of progress.
	c.withTimeout(nntpOpTimeout)

	// Pair this with the existing "Uploaded chunk N" success log so a
	// silent wedge is visible in logs by the unmatched POST line:
	//
	//     POST chunk 1 (file=foo.bin size=716800)
	//     <silence — workers are stuck>
	//
	// vs the healthy case:
	//
	//     POST chunk 1 (...)
	//     Uploaded chunk 1 - MsgID: ...
	//
	// On a wedge the timeout fires after nntpOpTimeout and the
	// existing retry path takes over (chunk-failed log, reconnect).
	postStart := time.Now()
	log.Printf("[%s] POST chunk %d (file=%s size=%d)", job.JobName, job.Number, job.FileName, len(encodedData))

	conn := c.text
	if err := conn.PrintfLine("POST"); err != nil {
		return err
	}
	if _, _, err := conn.ReadCodeLine(340); err != nil {
		return err
	}

	// RFC 5322 + RFC 2047: header values containing non-ASCII bytes must
	// be encoded-word wrapped. mime.QEncoding.Encode is a no-op for pure
	// ASCII (covers ~all real config values today) and wraps any non-
	// ASCII in =?UTF-8?Q?...?= otherwise — defensive belt for the moment
	// any operator sets NNTPFrom / GeneratorName / Subject to a CJK or
	// accented value. Newsgroup names MUST be ASCII per RFC 5536 §3.1.4;
	// we don't encoded-word wrap that one (which would corrupt the
	// group routing) and instead rely on splitNNTPGroups to drop invalid
	// tokens with a warning.
	dw := conn.DotWriter()
	fmt.Fprintf(dw, "From: %s\r\n", mime.QEncoding.Encode("UTF-8", cfg.NNTPFrom))
	fmt.Fprintf(dw, "Newsgroups: %s\r\n", cfg.NNTPGroup)
	fmt.Fprintf(dw, "Subject: %s\r\n", mime.QEncoding.Encode("UTF-8", job.Subject))
	fmt.Fprintf(dw, "Message-ID: <%s>\r\n", messageID)
	fmt.Fprintf(dw, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	fmt.Fprintf(dw, "X-Newsreader: %s\r\n", mime.QEncoding.Encode("UTF-8", cfg.GeneratorName))
	fmt.Fprintf(dw, "X-No-Archive: yes\r\n")
	fmt.Fprintf(dw, "\r\n")

	if _, err := dw.Write(encodedData); err != nil {
		dw.Close()
		return err
	}
	if err := dw.Close(); err != nil {
		return err
	}

	if _, _, err := conn.ReadCodeLine(240); err != nil {
		return err
	}
	// Per-chunk timing logged on success too — pairs with the POST log
	// at the top so unusually slow chunks are visible. Plain network
	// providers run a 700KB chunk well under 1 s; anything above 5 s
	// is a signal worth eyeballing in production logs.
	if d := time.Since(postStart); d > 5*time.Second {
		log.Printf("[%s] POST chunk %d slow: %s", job.JobName, job.Number, d.Round(time.Millisecond))
	}
	return nil
}

// yEncodeChunk encodes a buffer into yEnc format for a specific part.
// Uses a pooled buffer to avoid per-chunk allocation.
//
// For multi-part files, the trailer follows yEnc 1.3 §5.4:
//
//	non-final parts:  =yend size=N part=K pcrc32=<8hex>
//	final part:       =yend size=N part=K pcrc32=<8hex> crc32=<8hex>
//
// where pcrc32 is the CRC-32/IEEE of this part's decoded bytes and
// crc32 is the CRC-32/IEEE of the entire decoded file. The whole-
// file CRC is accumulated across yEncodeChunk calls via fileCRCState
// (see comment on that type) so the function signature can stay
// stable for callers that don't track upload state explicitly.
func yEncodeChunk(data []byte, filename string, partNumber int, totalParts int, chunkOffset int64, totalSize int64) []byte {
	maxCap := ChunkSize*2 + 256
	buf := yencPool.Get().(*bytes.Buffer)
	// Prevent unbounded capacity growth: discard oversized buffers.
	if buf.Cap() > maxCap {
		buf = bytes.NewBuffer(make([]byte, 0, ChunkSize+ChunkSize/50+256))
	}
	buf.Reset()

	crc := crc32.ChecksumIEEE(data)

	// Record this part's pcrc + length so the final-part call can
	// combine them in part-order into the whole-file CRC. Single-part
	// files skip the map entirely — pcrc32 == crc32 in that case.
	var (
		wholeFileCRC     uint32
		emitWholeFileCRC bool
	)
	if totalParts > 1 {
		key := fileCRCKey(filename, totalSize)
		state := loadOrCreateFileCRCState(key)
		state.mu.Lock()
		state.parts[partNumber] = partCRCInfo{pcrc: crc, length: int64(len(data))}
		state.cond.Broadcast()

		if partNumber == totalParts {
			// Final part: wait for every earlier part to register its
			// pcrc + length, then combine in part-order. In tests
			// (sequential) this never blocks; in production it blocks
			// at most until the slowest concurrent worker for this
			// file has handed its chunk to yEncodeChunk.
			for len(state.parts) < totalParts {
				state.cond.Wait()
			}
			for i := 1; i <= totalParts; i++ {
				info := state.parts[i]
				wholeFileCRC = crc32CombineIEEE(wholeFileCRC, info.pcrc, info.length)
			}
			emitWholeFileCRC = true
			state.mu.Unlock()
			// Drop the entry so a later upload of the same
			// (filename, totalSize) pair starts with fresh state.
			fileCRCStates.Delete(key)
		} else {
			state.mu.Unlock()
		}
	} else if totalParts == 1 {
		// Single-part file: pcrc32 already covers the whole file,
		// but emit crc32 too so spec-strict decoders are happy.
		wholeFileCRC = crc
		emitWholeFileCRC = true
	}

	// Headers.
	fmt.Fprintf(buf, "=ybegin part=%d total=%d line=128 size=%d name=%s\r\n", partNumber, totalParts, totalSize, filename)
	fmt.Fprintf(buf, "=ypart begin=%d end=%d\r\n", chunkOffset+1, chunkOffset+int64(len(data)))

	// Encode data.
	lineLen := 0
	for _, b := range data {
		val := (b + 42) & 255

		if val == 0 || val == 10 || val == 13 || val == 61 || (lineLen == 0 && val == 46) {
			buf.WriteByte('=')
			buf.WriteByte((val + 64) & 255)
			lineLen += 2
		} else {
			buf.WriteByte(val)
			lineLen++
		}

		if lineLen >= 128 {
			buf.WriteString("\r\n")
			lineLen = 0
		}
	}
	if lineLen > 0 {
		buf.WriteString("\r\n")
	}

	// Trailer.
	if emitWholeFileCRC {
		fmt.Fprintf(buf, "=yend size=%d part=%d pcrc32=%08x crc32=%08x\r\n", len(data), partNumber, crc, wholeFileCRC)
	} else {
		fmt.Fprintf(buf, "=yend size=%d part=%d pcrc32=%08x\r\n", len(data), partNumber, crc)
	}

	// Copy out so we can return the buffer to the pool.
	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())
	yencPool.Put(buf)
	return result
}

// crc32CombineIEEE returns the CRC-32/IEEE of a||b given CRC(a),
// CRC(b), and len(b). It uses the standard GF(2) matrix-exponentiation
// algorithm from zlib (crc32_combine) — operate on the CRC register
// as a 32-bit vector over GF(2), build the matrix for "shift by 1
// bit through the CRC", square-and-multiply it up to "shift by
// len(b)*8 bits", apply to crc1, then XOR in crc2.
//
// Go's standard library doesn't expose this; we need it because yEnc
// emits per-part pcrc32 values and the final part's whole-file crc32
// must equal CRC(part1 || part2 || ... || partN). Accumulating a
// running hash.Hash32 doesn't work when parts arrive out of order,
// which they can in our concurrent worker pool.
//
// The implementation is the standard one — see zlib's crc32.c
// crc32_combine64 / gf2_matrix_times / gf2_matrix_square.
func crc32CombineIEEE(crc1, crc2 uint32, len2 int64) uint32 {
	if len2 <= 0 {
		return crc1
	}

	var even, odd [32]uint32

	// odd[i] is the operator for shift-by-2^i+1 bits applied to a
	// CRC register. odd[0] is the identity step (CRC of one zero bit).
	// IEEE polynomial reflected = 0xedb88320 (low bit corresponds to
	// the high-degree coefficient under the reflected convention used
	// by crc32.IEEETable).
	odd[0] = 0xedb88320
	row := uint32(1)
	for i := 1; i < 32; i++ {
		odd[i] = row
		row <<= 1
	}

	// even = odd^2 (shift-by-2 operator)
	gf2MatrixSquare(&even, &odd)
	// odd = even^2 (shift-by-4 operator)
	gf2MatrixSquare(&odd, &even)

	crc := crc1
	l := uint64(len2)
	for {
		// Apply pending bits of l to crc using "even" (currently the
		// shift-by-2^(2k) operator for the iteration). Then square.
		gf2MatrixSquare(&even, &odd)
		if l&1 != 0 {
			crc = gf2MatrixTimes(&even, crc)
		}
		l >>= 1
		if l == 0 {
			break
		}
		gf2MatrixSquare(&odd, &even)
		if l&1 != 0 {
			crc = gf2MatrixTimes(&odd, crc)
		}
		l >>= 1
		if l == 0 {
			break
		}
	}
	return crc ^ crc2
}

func gf2MatrixTimes(mat *[32]uint32, vec uint32) uint32 {
	var sum uint32
	i := 0
	for vec != 0 {
		if vec&1 != 0 {
			sum ^= mat[i]
		}
		vec >>= 1
		i++
	}
	return sum
}

func gf2MatrixSquare(square, mat *[32]uint32) {
	for i := 0; i < 32; i++ {
		square[i] = gf2MatrixTimes(mat, mat[i])
	}
}

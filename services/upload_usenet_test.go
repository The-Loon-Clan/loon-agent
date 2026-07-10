package services

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"regexp"
	"testing"
)

// TestYEncFileCRC pins the yEnc 1.3 trailer contract for multi-part files:
//
//   - Non-final parts emit "=yend size=… part=… pcrc32=<8hex>" with NO
//     trailing crc32 attribute. pcrc32 is the CRC-32/IEEE of THIS part's
//     decoded bytes only.
//   - The final part emits both pcrc32 AND a whole-file crc32 attribute:
//     "=yend size=… part=N pcrc32=<8hex> crc32=<8hex>". The crc32 value
//     is the CRC-32/IEEE of the full unaltered input — i.e. equal to
//     crc32.ChecksumIEEE(input).
//
// Several Usenet decoders (sabnzbd, nzbget) downgrade verification to a
// best-effort hash check when the whole-file crc32 is missing on the final
// part. Some pickier tools warn or refuse. Emitting crc32 is purely additive
// — decoders that don't recognise it ignore it, decoders that do can verify
// end-to-end without reassembling and recomputing.
//
// We feed yEncodeChunk a deterministic 4096-byte ramp (every value 0..255
// cycled 16 times). That exhausts the byte domain so the encoder exercises
// every escape branch (NUL, LF, CR, '=', leading '.'), and it slices cleanly
// into a small number of equal-sized parts. We pick chunk=1024 to force a
// 4-part split without changing the package-level ChunkSize constant.
func TestYEncFileCRC(t *testing.T) {
	const chunkSize = 1024
	input := make([]byte, 4096)
	for i := range input {
		input[i] = byte(i)
	}
	expectedFileCRC := crc32.ChecksumIEEE(input)

	totalParts := (len(input) + chunkSize - 1) / chunkSize
	if totalParts != 4 {
		t.Fatalf("test setup: expected 4 parts, got %d", totalParts)
	}

	// Regexes for trailer parsing. We accept arbitrary attribute ordering by
	// using look-anywhere matches against the trailer line — the spec is
	// space-separated key=value pairs after "=yend".
	yendLineRE := regexp.MustCompile(`(?m)^=yend [^\r\n]+`)
	pcrcRE := regexp.MustCompile(`\bpcrc32=([0-9a-fA-F]+)\b`)
	crcRE := regexp.MustCompile(`\bcrc32=([0-9a-fA-F]+)\b`)

	for partIdx := 0; partIdx < totalParts; partIdx++ {
		partNum := partIdx + 1
		start := partIdx * chunkSize
		end := start + chunkSize
		if end > len(input) {
			end = len(input)
		}
		chunk := input[start:end]
		isFinal := partNum == totalParts

		encoded := yEncodeChunk(chunk, "ramp.bin", partNum, totalParts, int64(start), int64(len(input)))

		// Find the =yend trailer line.
		trailer := yendLineRE.Find(encoded)
		if trailer == nil {
			t.Fatalf("part %d: no =yend trailer found in encoded output: %q", partNum, encoded)
		}

		// pcrc32 must always be present and must equal CRC of THIS chunk.
		pm := pcrcRE.FindSubmatch(trailer)
		if pm == nil {
			t.Fatalf("part %d: pcrc32 missing from trailer: %q", partNum, trailer)
		}
		wantPCRC := fmt.Sprintf("%08x", crc32.ChecksumIEEE(chunk))
		if !bytes.EqualFold(pm[1], []byte(wantPCRC)) {
			t.Errorf("part %d: pcrc32=%s, want %s", partNum, pm[1], wantPCRC)
		}

		// crc32 (whole file) — required on final part, forbidden elsewhere.
		cm := crcRE.FindSubmatch(trailer)
		switch {
		case isFinal && cm == nil:
			t.Errorf("final part %d: trailer missing whole-file crc32 attribute (S2.13): %q", partNum, trailer)
		case isFinal && cm != nil:
			wantCRC := fmt.Sprintf("%08x", expectedFileCRC)
			if !bytes.EqualFold(cm[1], []byte(wantCRC)) {
				t.Errorf("final part %d: crc32=%s, want %s (CRC-32/IEEE of full input)", partNum, cm[1], wantCRC)
			}
		case !isFinal && cm != nil:
			t.Errorf("non-final part %d: trailer must NOT carry whole-file crc32 (got %s): %q", partNum, cm[1], trailer)
		}
	}
}

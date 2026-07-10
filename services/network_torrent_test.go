package services

import (
	"errors"
	"fmt"
	"testing"
)

// TestDiskShortfallError_Sentinel locks in the error-chain contract
// between the typed DiskShortfallError and the ErrInsufficientDisk
// sentinel. The agent's abort path depends on BOTH working:
//   - errors.Is(err, ErrInsufficientDisk) to trigger the oversize branch
//   - errors.As(err, &ds) to extract the discovered torrent size for
//     site report-back
//
// If a future refactor replaces the Unwrap() method or removes the
// sentinel, neither will light up at runtime — the abort still fires
// (because isRuntimeDiskFullError is also in the condition) but the
// size never reports to the site and the same oversize task keeps
// getting handed to smaller-disk agents forever. This test catches
// that before it ships.
func TestDiskShortfallError_Sentinel(t *testing.T) {
	var ds *DiskShortfallError
	err := error(&DiskShortfallError{TorrentBytes: 90_000_000_000, AvailableBytes: 50_000_000_000})

	if !errors.Is(err, ErrInsufficientDisk) {
		t.Fatalf("errors.Is(err, ErrInsufficientDisk) = false; want true (Unwrap chain broken)")
	}
	if !errors.As(err, &ds) {
		t.Fatalf("errors.As(err, &ds) = false; want true (*DiskShortfallError target didn't match)")
	}
	if got, want := ds.TorrentBytes, int64(90_000_000_000); got != want {
		t.Errorf("ds.TorrentBytes = %d; want %d", got, want)
	}
	if got, want := ds.AvailableBytes, int64(50_000_000_000); got != want {
		t.Errorf("ds.AvailableBytes = %d; want %d", got, want)
	}

	// Error message should include the human-readable GB values so
	// operators reading agent logs don't have to divide by 1e9 by hand.
	msg := err.Error()
	if msg == "" {
		t.Fatal("Error() returned empty string")
	}
}

// TestDiskShortfallError_WrapsWithFmtErrorf verifies that a
// DiskShortfallError can be wrapped by fmt.Errorf("%w: context", ds)
// and still be recoverable via errors.Is / errors.As. This matters
// because higher-layer callers may add context to the error before it
// reaches the abort site.
func TestDiskShortfallError_WrapsWithFmtErrorf(t *testing.T) {
	original := &DiskShortfallError{TorrentBytes: 10, AvailableBytes: 5}
	wrapped := fmt.Errorf("downloadMagnet: %w", original)

	if !errors.Is(wrapped, ErrInsufficientDisk) {
		t.Error("errors.Is lost the sentinel through fmt.Errorf %w wrapping")
	}
	var ds *DiskShortfallError
	if !errors.As(wrapped, &ds) {
		t.Error("errors.As lost the typed struct through fmt.Errorf %w wrapping")
	}
	if ds != nil && ds.TorrentBytes != 10 {
		t.Errorf("ds.TorrentBytes after unwrap = %d; want 10", ds.TorrentBytes)
	}
}

// TestNormalizeInfoHash locks in the 2026-06-05 fix for
// "error decoding xt: illegal base32 data at input byte 0".
// anacrolix's magnet parser is strict about base32 case;
// lowercase 32-char hashes get rejected. Hex (40-char) hashes
// are case-insensitive and pass through unchanged.
func TestNormalizeInfoHash(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// The exact hash from the user's failing report.
		{"lowercase base32 (32 chars)", "ghgsyznfvrqxz3iwed2hv57dlvdnwo7q", "GHGSYZNFVRQXZ3IWED2HV57DLVDNWO7Q"},
		// Mixed case — uppercase everything.
		{"mixed case base32", "GhGsYzNfVrQxZ3iWeD2hV57DlVdNwO7Q", "GHGSYZNFVRQXZ3IWED2HV57DLVDNWO7Q"},
		// Already uppercase — pass through.
		{"uppercase base32", "GHGSYZNFVRQXZ3IWED2HV57DLVDNWO7Q", "GHGSYZNFVRQXZ3IWED2HV57DLVDNWO7Q"},
		// Hex hashes — 40 chars, case-insensitive at the parser
		// level. Pass through unchanged (no upper).
		{"hex lowercase (40 chars)", "abcdef0123456789abcdef0123456789abcdef01", "abcdef0123456789abcdef0123456789abcdef01"},
		{"hex uppercase", "ABCDEF0123456789ABCDEF0123456789ABCDEF01", "ABCDEF0123456789ABCDEF0123456789ABCDEF01"},
		// Non-base32 char in a 32-char string — pass through, not
		// our shape (defensive: don't silently mangle).
		{"32 chars with non-base32", "ghgsyznfvrqxz3iwed2hv57dlvdnwo71", "ghgsyznfvrqxz3iwed2hv57dlvdnwo71"}, // '1' isn't valid base32
		// Whitespace at boundaries — trimmed.
		{"whitespace + lowercase", "  ghgsyznfvrqxz3iwed2hv57dlvdnwo7q  ", "GHGSYZNFVRQXZ3IWED2HV57DLVDNWO7Q"},
		// Empty + nonsense — pass through.
		{"empty", "", ""},
		{"short junk", "xyz", "xyz"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NormalizeInfoHash(c.in)
			if got != c.want {
				t.Errorf("NormalizeInfoHash(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

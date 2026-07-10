//go:build !linux

package services

import "math"

// FreeDiskSpace is a stub for non-Linux platforms (dev only).
// Returns max uint64 so space checks never block during local testing.
func FreeDiskSpace(path string) (uint64, error) {
	return math.MaxUint64, nil
}

// TotalDiskSpace is a stub for non-Linux platforms (dev only). Returns 0
// so the local UI's percentage math yields NaN, and JS skips rendering
// the bar — cleaner than a fake value that would look real.
func TotalDiskSpace(path string) (uint64, error) {
	return 0, nil
}

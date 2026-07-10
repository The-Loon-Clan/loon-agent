//go:build !linux

package services

// CPUUsagePercent is a stub for non-Linux platforms (dev only).
// Returns 0 so CPU checks never block during local testing.
func CPUUsagePercent() (float64, error) {
	return 0, nil
}

package services

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// CPUUsagePercent returns the system CPU usage as a percentage (0-100)
// based on the 1-minute load average from /proc/loadavg.
// Uses runtime.NumCPU() for core count (works correctly in Docker).
func CPUUsagePercent() (float64, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, nil
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	cpuCount := runtime.NumCPU()
	if cpuCount < 1 {
		cpuCount = 1
	}
	pct := (load / float64(cpuCount)) * 100
	if pct > 100 {
		pct = 100
	}
	return pct, nil
}

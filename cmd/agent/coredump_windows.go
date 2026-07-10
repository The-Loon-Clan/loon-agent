//go:build windows

package main

// disableCoreDumps is a no-op on Windows: the kernel core-dump
// mechanism this guards against is Linux-specific, and the production
// agent always runs in a Linux container. Present so the dev build on
// Windows compiles. See coredump_unix.go for the real implementation
// and the rationale.
func disableCoreDumps() {}

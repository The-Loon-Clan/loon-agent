package services

import "syscall"

// FreeDiskSpace returns the number of free bytes on the filesystem containing path.
func FreeDiskSpace(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

// TotalDiskSpace returns the total size in bytes of the filesystem
// containing path. Pairs with FreeDiskSpace so the local UI can show
// "45 GB free (23% used)" instead of just the absolute free number —
// operators on different-sized drives can compare at a glance.
func TotalDiskSpace(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Blocks * uint64(stat.Bsize), nil
}

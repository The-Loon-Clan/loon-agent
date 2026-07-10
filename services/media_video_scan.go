package services

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// VideoExtensions are the container formats we treat as "main content" when
// looking for the file to probe/screenshot. Kept as a package-level map so
// both the online (site-polling) path and the offline (watch-folder)
// pipeline agree on what counts — divergence would mean some releases get
// screenshots on one path and not the other.
var VideoExtensions = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m2ts": true,
	".ts": true, ".wmv": true, ".flv": true, ".webm": true, ".mov": true,
}

// FindVideoFiles returns every video file beneath dir, biggest first.
// The main video in a release is reliably the largest; callers that want
// a single candidate can take the first element.
func FindVideoFiles(dir string) []string {
	type vf struct {
		path string
		size int64
	}
	var files []vf
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return err
		}
		if VideoExtensions[strings.ToLower(filepath.Ext(info.Name()))] {
			files = append(files, vf{path, info.Size()})
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].size > files[j].size })
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.path
	}
	return paths
}

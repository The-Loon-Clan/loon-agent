package services

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// stageLocalContent places localPath's content into stageDir for the publish
// pipeline, honouring an optional file filter (site migration 321: a request
// scoped to named files inside a folder offer).
//
// No filter: localPath — file or directory — is symlinked in whole, exactly
// the pre-filter behaviour. With a filter, only the named files are staged,
// under a directory keeping the source folder's name so the release keeps
// its identity. Names are matched on the file's BASE name, because that is
// what the inventory scan reported and what the member ticked; matches are
// flattened into the staged folder, since a 40-of-136 episode selection does
// not need the source's internal subfolders to make sense as a release.
//
// Symlink first, copy when the filesystem refuses (Windows non-admin,
// exotic mounts) — same fallback the unfiltered path always had.
func stageLocalContent(localPath, stageDir string, fileFilter []string) error {
	if len(fileFilter) == 0 {
		staged := filepath.Join(stageDir, filepath.Base(localPath))
		if err := os.Symlink(localPath, staged); err != nil {
			if cerr := copyFile(localPath, staged); cerr != nil {
				return cerr
			}
		}
		return nil
	}

	wanted := make(map[string]bool, len(fileFilter))
	for _, name := range fileFilter {
		if name = strings.TrimSpace(name); name != "" {
			wanted[name] = true
		}
	}

	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		// A single-file source can satisfy a filter only if it IS the file.
		if !wanted[filepath.Base(localPath)] {
			return fmt.Errorf("request is scoped to %d file(s) but the source is the single file %q",
				len(wanted), filepath.Base(localPath))
		}
		staged := filepath.Join(stageDir, filepath.Base(localPath))
		if err := os.Symlink(localPath, staged); err != nil {
			if cerr := copyFile(localPath, staged); cerr != nil {
				return cerr
			}
		}
		return nil
	}

	root := filepath.Join(stageDir, filepath.Base(localPath))
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	found := map[string]bool{}
	walkErr := filepath.WalkDir(localPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		base := filepath.Base(path)
		if !wanted[base] || found[base] {
			return nil
		}
		staged := filepath.Join(root, base)
		if serr := os.Symlink(path, staged); serr != nil {
			if cerr := copyFile(path, staged); cerr != nil {
				return cerr
			}
		}
		found[base] = true
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	if len(found) != len(wanted) {
		missing := make([]string, 0, len(wanted)-len(found))
		for name := range wanted {
			if !found[name] {
				missing = append(missing, name)
			}
		}
		// Refuse rather than deliver a partial partial: the member picked an
		// exact set, and the inventory that offered it has drifted. Failing
		// reopens the request for an offerer whose copy still has them.
		return fmt.Errorf("%d of the requested file(s) are not in the source folder (e.g. %q) — inventory has drifted",
			len(missing), missing[0])
	}
	return nil
}

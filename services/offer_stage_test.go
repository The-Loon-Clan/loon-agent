package services

import (
	"os"
	"path/filepath"
	"testing"
)

// The staging rules the file-scoped request feature depends on: no filter
// stages everything (the pre-321 behaviour), a filter stages exactly the
// named files flattened under the source folder's name, and a filter naming
// a file the folder no longer holds refuses rather than delivering a
// partial partial.

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sourceFolder(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "One Piece [BD]")
	writeFile(t, filepath.Join(src, "One Piece - 0783.mkv"))
	writeFile(t, filepath.Join(src, "One Piece - 0784.mkv"))
	writeFile(t, filepath.Join(src, "Extras", "One Piece - NCOP.mkv"))
	return src
}

func TestStageWholeFolderWhenUnfiltered(t *testing.T) {
	src, stage := sourceFolder(t), t.TempDir()
	// The unfiltered path symlinks the DIRECTORY whole, and its copy
	// fallback is file-only — unchanged pre-321 behaviour that only ever
	// runs on the Linux agents. Windows without symlink rights cannot
	// exercise it, and pretending otherwise would test the wrong thing.
	if err := os.Symlink(src, filepath.Join(t.TempDir(), "probe")); err != nil {
		t.Skipf("symlinks unavailable here (%v) — the unfiltered dir path needs them", err)
	}
	if err := stageLocalContent(src, stage, nil); err != nil {
		t.Fatalf("unfiltered stage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stage, "One Piece [BD]", "One Piece - 0784.mkv")); err != nil {
		t.Errorf("whole-folder stage missing a file: %v", err)
	}
}

func TestStageOnlyTheRequestedFiles(t *testing.T) {
	src, stage := sourceFolder(t), t.TempDir()
	err := stageLocalContent(src, stage, []string{"One Piece - 0783.mkv", "One Piece - NCOP.mkv"})
	if err != nil {
		t.Fatalf("filtered stage: %v", err)
	}
	root := filepath.Join(stage, "One Piece [BD]")
	for _, want := range []string{"One Piece - 0783.mkv", "One Piece - NCOP.mkv"} {
		if _, err := os.Stat(filepath.Join(root, want)); err != nil {
			t.Errorf("requested file %q not staged: %v", want, err)
		}
	}
	// The nested extra was requested and must be FLATTENED to the root.
	if _, err := os.Stat(filepath.Join(root, "Extras")); !os.IsNotExist(err) {
		t.Error("source subfolder structure leaked into the staged set")
	}
	if _, err := os.Stat(filepath.Join(root, "One Piece - 0784.mkv")); !os.IsNotExist(err) {
		t.Error("an unrequested file was staged")
	}
}

func TestStageRefusesWhenARequestedFileIsGone(t *testing.T) {
	src, stage := sourceFolder(t), t.TempDir()
	err := stageLocalContent(src, stage, []string{"One Piece - 0783.mkv", "One Piece - 9999.mkv"})
	if err == nil {
		t.Fatal("staging succeeded with a requested file missing from the source")
	}
}

func TestStageSingleFileSourceHonoursTheFilter(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "Movie.mkv")
	writeFile(t, file)
	stage := t.TempDir()
	if err := stageLocalContent(file, stage, []string{"Movie.mkv"}); err != nil {
		t.Fatalf("matching single-file stage: %v", err)
	}
	if err := stageLocalContent(file, t.TempDir(), []string{"Other.mkv"}); err == nil {
		t.Fatal("single-file source staged against a filter naming a different file")
	}
}

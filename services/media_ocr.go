package services

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
)

// OCRResult is the recognised-text payload the agent ships per
// release. Text is the concatenated result across pages; Language
// is the tesseract -l string that produced it (e.g. "eng+jpn") so
// future searches can filter on what was scanned.
type OCRResult struct {
	Text     string
	Language string
}

// OCRMangaPages runs tesseract over the manga sample-page images
// the agent already extracted for screenshots. Returns an empty
// result when tesseract isn't on PATH, when no pages were passed,
// or when every page came back blank — the site treats absence as
// "feature didn't run" and hides the OCR section.
//
// Language is the tesseract -l value. Callers should pass a
// combined string like "eng+jpn" for manga so both romanised
// signs and Japanese dialogue come back; tesseract resolves the
// installed data files and silently drops any that are missing.
func OCRMangaPages(ctx context.Context, pagePaths []string, language string) OCRResult {
	if _, err := exec.LookPath("tesseract"); err != nil {
		log.Printf("ocr: tesseract not found in PATH — skipping (install tesseract-ocr + tessdata-jpn)")
		return OCRResult{}
	}
	if len(pagePaths) == 0 {
		return OCRResult{}
	}
	if language == "" {
		language = "eng"
	}
	var parts []string
	for _, p := range pagePaths {
		text, err := tesseractOne(ctx, p, language)
		if err != nil {
			log.Printf("ocr: %s: %v (continuing)", filepath.Base(p), err)
			continue
		}
		text = strings.TrimSpace(text)
		// Drop very short / pure-garbage pages — tesseract emits
		// random punctuation salads on graphic-only spreads, and
		// keeping them inflates the indexed text without adding
		// search value.
		if len(text) < 4 || isNoiseOCR(text) {
			continue
		}
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		return OCRResult{Language: language}
	}
	return OCRResult{
		Text:     strings.Join(parts, "\n\n"),
		Language: language,
	}
}

// tesseractOne runs `tesseract IMAGE - -l LANG` and captures stdout.
// We use "-" as the output base so tesseract writes to stdout
// instead of creating <base>.txt next to the image.
func tesseractOne(ctx context.Context, image, language string) (string, error) {
	cmd := exec.CommandContext(ctx, "tesseract", image, "-", "-l", language, "--psm", "6")
	cmd.Env = toolEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		escalateToolCrash("tesseract", image, stderr.Bytes(), err)
		return "", fmt.Errorf("%w: %s", err, tailLines(stderr.String(), 2))
	}
	return stdout.String(), nil
}

// isNoiseOCR returns true when text looks like OCR garbage rather
// than real recognised content. Heuristic: if fewer than 30% of
// characters are letters/digits, we treat it as noise. Manga pages
// with only sound effects ("WHOOSH!", "BANG") still clear this bar.
func isNoiseOCR(text string) bool {
	if len(text) == 0 {
		return true
	}
	alnum, total := 0, 0
	for _, r := range text {
		total++
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || (r >= 0x3040 && r <= 0x30FF) ||
			(r >= 0x4E00 && r <= 0x9FFF) {
			alnum++
		}
	}
	// Compare against rune count, not byte count — a single kanji is
	// 3 bytes in UTF-8, so dividing by len(text) would mis-classify
	// pure-Japanese pages as noise.
	return alnum*100/total < 30
}

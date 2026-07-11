package services

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"github.com/ameNZB/loon-agent/config"
	"log"
	"os"
	"strings"
	"time"
)

// splitNNTPGroups turns a comma/space-separated newsgroup string into a
// clean slice, one element per destination. The offline pipeline joins a
// group's newsgroups list with commas so UploadDirectory can set the
// NNTP Newsgroups: header once for crossposting, but the NZB spec wants
// one <group> child per destination. Trims + drops empties so padded
// input ("alt.a, alt.b") doesn't leak blanks into the output.
//
// RFC 5536 §3.1.4 restricts newsgroup names to ASCII letters / digits /
// "." / "+" / "-" / "_". Any token containing other bytes (CJK from a
// typo, fullwidth dots from a CJK keyboard) is rejected with a warning
// because it can't legally be on the wire — better to drop the bad
// group and post to the good ones than fail the whole crosspost.
func splitNNTPGroups(s string) []string {
	out := make([]string, 0, 1)
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !isValidNewsgroupName(part) {
			log.Printf("upload: skipping non-RFC5536 newsgroup name %q (must be ASCII a-z 0-9 . + - _)", part)
			continue
		}
		out = append(out, part)
	}
	if len(out) == 0 {
		// Preserve the prior behaviour of always having at least one group
		// element in the NZB even if config is somehow empty — some
		// downloaders reject NZBs with zero <group> children.
		out = append(out, s)
	}
	return out
}

// isValidNewsgroupName reports whether s contains only the bytes allowed
// in a newsgroup name by RFC 5536 §3.1.4: ASCII letters, digits, ".",
// "+", "-", "_". Rejects empty, leading/trailing dot, and any non-ASCII
// rune. Used as a defensive filter in splitNNTPGroups so a CJK typo in
// config doesn't 441 the article.
func isValidNewsgroupName(s string) bool {
	if s == "" || s[0] == '.' || s[len(s)-1] == '.' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '.' || c == '+' || c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

type NZB struct {
	XMLName xml.Name  `xml:"nzb"`
	Xmlns   string    `xml:"xmlns,attr"`
	Head    []NZBMeta `xml:"head>meta,omitempty"`
	Files   []NZBFile `xml:"file"`
}

type NZBMeta struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

type NZBFile struct {
	Poster   string       `xml:"poster,attr"`
	Date     int64        `xml:"date,attr"`
	Subject  string       `xml:"subject,attr"`
	Groups   []string     `xml:"groups>group"`
	Segments []NZBSegment `xml:"segments>segment"`
}

// CreateNZB generates a valid NZB file from the uploaded segments.
func CreateNZB(cfg *config.Config, nzbPath string, subject string, segments []NZBSegment) error {
	data, err := CreateNZBBytes(cfg, subject, segments)
	if err != nil {
		return err
	}
	return os.WriteFile(nzbPath, data, 0644)
}

// NZBMeta provides title and request ID for NZB header metadata.
type NZBMetaInfo struct {
	Title     string
	RequestID int64
}

// CreateNZBBytes generates NZB XML as bytes for a single-file upload (legacy).
func CreateNZBBytes(cfg *config.Config, subject string, segments []NZBSegment) ([]byte, error) {
	return CreateMultiFileNZBBytes(cfg, []FileSegments{
		{FileName: subject, Segments: segments},
	}, "", NZBMetaInfo{})
}

// FileSegments groups segments by their source file.
type FileSegments struct {
	FileName string
	Segments []NZBSegment
}

// CreateMultiFileNZBBytes generates NZB XML for multiple files (raw + PAR2).
// password is stored as NZB metadata (empty string = no password).
func CreateMultiFileNZBBytes(cfg *config.Config, files []FileSegments, password string, meta NZBMetaInfo) ([]byte, error) {
	nzb := NZB{
		Xmlns: "http://www.newzbin.com/DTD/2003/nzb",
	}

	// NZB metadata.
	nzb.Head = append(nzb.Head,
		NZBMeta{Type: "x-generator", Value: cfg.GeneratorName},
	)
	if meta.Title != "" {
		nzb.Head = append(nzb.Head, NZBMeta{Type: "title", Value: meta.Title})
	}
	if meta.RequestID > 0 && cfg.SiteURL != "" {
		nzb.Head = append(nzb.Head, NZBMeta{Type: "x-info-url",
			Value: fmt.Sprintf("%s/release/%d", cfg.SiteURL, meta.RequestID)})
	}
	if password != "" {
		nzb.Head = append(nzb.Head, NZBMeta{Type: "password", Value: password})
	}

	now := time.Now().Unix()
	totalFiles := len(files)

	// cfg.NNTPGroup may be a comma-separated list for cross-posting (the
	// offline pipeline joins a group's newsgroups into one string to
	// leverage NNTP's crosspost header). In the NZB we want one <group>
	// element per destination, not a single comma-containing one.
	groupsList := splitNNTPGroups(cfg.NNTPGroup)

	for i, f := range files {
		// Standard subject format: [filenum/totalfiles] - "filename" yEnc (1/totalparts) filesize
		subject := f.FileName
		if totalFiles > 1 {
			subject = f.FileName // subject per-file is set by the uploader
		}

		nzb.Files = append(nzb.Files, NZBFile{
			Poster:   cfg.NNTPPoster,
			Date:     now,
			Subject:  subject,
			Groups:   groupsList,
			Segments: f.Segments,
		})
		_ = i // suppress unused
	}

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	buf.WriteString(fmt.Sprintf("<!-- NZB Generated by %s -->\n", cfg.GeneratorName))

	encoder := xml.NewEncoder(&buf)
	encoder.Indent("", "  ")
	if err := encoder.Encode(nzb); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

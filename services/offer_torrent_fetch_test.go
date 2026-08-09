package services

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A minimal but structurally real .torrent: bencoded dict with an info dict.
var fakeTorrent = []byte("d8:announce20:http://tracker/annou4:infod4:name4:test6:lengthi1234eee")

func TestValidateTorrentBytes(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr error  // sentinel, when there is one
		wantSub string // else a substring of the message
	}{
		{name: "real torrent", body: string(fakeTorrent)},
		{name: "empty", body: "", wantSub: "empty"},
		// The case this file exists for: 200 OK with a login page.
		{name: "html login page", body: "<!DOCTYPE html><html><body>Please log in</body></html>", wantErr: ErrTorrentAuthWall},
		{name: "html with leading whitespace", body: "\n\n  <html><head></head></html>", wantErr: ErrTorrentAuthWall},
		{name: "cloudflare style", body: "<html><title>Just a moment...</title>", wantErr: ErrTorrentAuthWall},
		{name: "json error body", body: `{"error":"unauthorized"}`, wantSub: "not bencode"},
		{name: "bencode without info dict", body: "d8:announce20:http://tracker/annoue", wantSub: "no info dict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTorrentBytes([]byte(tc.body))
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.wantSub != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
					t.Errorf("err = %v, want one containing %q", err, tc.wantSub)
				}
			default:
				if err != nil {
					t.Errorf("valid torrent rejected: %v", err)
				}
			}
		})
	}
}

func TestFetchTorrentBytesSendsCookiesAndBrowserHeaders(t *testing.T) {
	var gotCookie, gotUA, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		w.Write(fakeTorrent)
	}))
	defer srv.Close()

	// Jar keyed by the test server's host so the domain match has to work.
	host := strings.Split(strings.TrimPrefix(srv.URL, "http://"), ":")[0]
	jarPath := filepath.Join(t.TempDir(), "cookies.json")
	jar, _ := json.Marshal(map[string]map[string]string{host: {"session": "abc123"}})
	if err := os.WriteFile(jarPath, jar, 0o600); err != nil {
		t.Fatal(err)
	}

	body, err := fetchTorrentBytes(context.Background(), srv.Client(), srv.URL+"/dl.torrent", "chrome", jarPath)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if string(body) != string(fakeTorrent) {
		t.Errorf("body mismatch: %q", body)
	}
	if !strings.Contains(gotCookie, "session=abc123") {
		t.Errorf("Cookie header = %q, want the jar's session", gotCookie)
	}
	if gotUA == "" {
		t.Error("no User-Agent sent — trackers that fingerprint the session against it will refuse a valid jar")
	}
	if !strings.Contains(gotAccept, "bittorrent") {
		t.Errorf("Accept = %q, want it to ask for a torrent", gotAccept)
	}
}

func TestFetchTorrentBytesRejections(t *testing.T) {
	// The auth wall, end to end: 200 OK, HTML body.
	t.Run("html body is an auth wall", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("<html><body>login</body></html>"))
		}))
		defer srv.Close()
		_, err := fetchTorrentBytes(context.Background(), srv.Client(), srv.URL, "", "")
		if !errors.Is(err, ErrTorrentAuthWall) {
			t.Errorf("err = %v, want ErrTorrentAuthWall", err)
		}
	})

	t.Run("oversize body is refused", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("d"))
			// Stream past the cap; the reader must stop and complain rather
			// than buffer whatever the far end feels like sending.
			blob := make([]byte, 1<<20)
			for i := 0; i < 5; i++ {
				w.Write(blob)
			}
		}))
		defer srv.Close()
		_, err := fetchTorrentBytes(context.Background(), srv.Client(), srv.URL, "", "")
		if err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Errorf("err = %v, want an oversize refusal", err)
		}
	})

	t.Run("non-200 is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		_, err := fetchTorrentBytes(context.Background(), srv.Client(), srv.URL, "", "")
		if err == nil || !strings.Contains(err.Error(), "403") {
			t.Errorf("err = %v, want the status in the message", err)
		}
	})

	t.Run("non-http scheme is refused before dialling", func(t *testing.T) {
		_, err := fetchTorrentBytes(context.Background(), nil, "file:///etc/passwd", "", "")
		if err == nil || !strings.Contains(err.Error(), "scheme") {
			t.Errorf("err = %v, want a scheme refusal", err)
		}
	})
}

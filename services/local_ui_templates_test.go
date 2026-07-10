package services

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"

	"github.com/ameNZB/usenet-pipeline/storage"
)

// TestPageTemplatesRender exercises every page template with a plausible
// data shape so Execute surfaces any "undefined variable" or "wrong type"
// mistakes at test time instead of at the user's first page load. We
// don't assert full output — a successful Execute plus a few keyword
// checks is enough signal that the layout/block composition works.
func TestPageTemplatesRender(t *testing.T) {
	now := time.Now()
	iTrue := 1
	bTrue := true
	int64One := int64(1)

	cases := []struct {
		name  string
		tmpl  *template.Template
		data  map[string]any
		needs []string
	}{
		{
			"groups-empty",
			groupsTmpl,
			map[string]any{"CurrentPage": "groups", "BindAddr": "127.0.0.1:4869", "Groups": []*storage.Group(nil)},
			[]string{"<title>Groups"},
		},
		{
			"groups-one-row",
			groupsTmpl,
			map[string]any{
				"CurrentPage": "groups", "BindAddr": "127.0.0.1:4869",
				"Groups": []*storage.Group{{
					ID: 1, Name: "anime", Newsgroups: []string{"alt.binaries.x"},
					Screenshots: &iTrue, Par2Redundancy: &iTrue, Obfuscate: &bTrue,
					WatermarkText: "-Team", Source: "local", CreatedAt: now, UpdatedAt: now,
				}},
			},
			[]string{"anime", "alt.binaries.x", "-Team"},
		},
		{
			"watches-empty",
			watchesTmpl,
			map[string]any{"CurrentPage": "watches", "BindAddr": "127.0.0.1:4869", "Watches": []*storage.WatchFolder(nil), "Groups": []*storage.Group(nil)},
			[]string{"<title>Watch Folders"},
		},
		{
			"watches-with-group-fk",
			watchesTmpl,
			map[string]any{
				"CurrentPage": "watches", "BindAddr": "127.0.0.1:4869",
				"Watches": []*storage.WatchFolder{{ID: 1, Path: "/data/watch/anime", GroupID: &int64One, Enabled: true, CreatedAt: now, UpdatedAt: now}},
				"Groups":  []*storage.Group{{ID: 1, Name: "anime"}},
			},
			[]string{"/data/watch/anime"},
		},
		{
			"jobs-empty",
			jobsTmpl,
			map[string]any{"CurrentPage": "jobs", "BindAddr": "127.0.0.1:4869", "Jobs": []*storage.OfflineJob(nil)},
			[]string{"No jobs yet"},
		},
		{
			"jobs-completed",
			jobsTmpl,
			map[string]any{
				"CurrentPage": "jobs", "BindAddr": "127.0.0.1:4869",
				"Jobs": []*storage.OfflineJob{{
					ID: 1, Title: "t", SourcePath: "/s", GroupNameAtCreation: "anime",
					Status: "completed", CreatedAt: now,
				}},
			},
			[]string{"completed", "Retry"},
		},
		{
			"config-no-rows",
			localUITmpl,
			map[string]any{
				"CurrentPage": "config", "BindAddr": "127.0.0.1:4869",
				"Writable": true, "Rows": []struct{ Key, Value, Source string }{}, "SecretsHosts": []string{},
			},
			[]string{"<title>Config", "config.yml"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tc.tmpl.Execute(&buf, tc.data); err != nil {
				t.Fatalf("execute: %v", err)
			}
			out := buf.String()
			for _, need := range tc.needs {
				if !strings.Contains(out, need) {
					t.Errorf("missing %q in output (len=%d)", need, len(out))
				}
			}
			// Every page must render the sidebar nav — catches layout regressions.
			for _, nav := range []string{"Jobs", "Watch Folders", "Groups", "Config"} {
				if !strings.Contains(out, nav) {
					t.Errorf("sidebar missing nav link %q", nav)
				}
			}
		})
	}
}

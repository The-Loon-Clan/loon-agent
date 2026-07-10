package services

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// secretsPath returns where the agent reads/writes its passkey map. The
// file is intentionally a sibling of config.yml so users who copy one
// directory to a new host keep their private-tracker setup intact.
func secretsPath() string {
	if p := os.Getenv("SECRETS_YML"); p != "" {
		return p
	}
	base := os.Getenv("CONFIG_DIR")
	if base == "" {
		base = "."
	}
	return filepath.Join(base, "secrets.yml")
}

// SecretsStore holds per-tracker passkeys loaded from secrets.yml. Keys
// are tracker hostnames ("nekobt.to"); values are opaque passkeys the
// tracker expects in the announce URL. Nothing here ever leaves the
// agent — the site only learns that secrets *exist*, never their value.
type SecretsStore struct {
	mu    sync.RWMutex
	path  string
	items map[string]string
}

func LoadSecrets() *SecretsStore {
	s := &SecretsStore{path: secretsPath(), items: map[string]string{}}
	s.reload()
	return s
}

func (s *SecretsStore) reload() {
	f, err := os.Open(s.path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	items := map[string]string{}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		host := strings.ToLower(strings.TrimSpace(line[:i]))
		key := strings.Trim(strings.TrimSpace(line[i+1:]), `"'`)
		if host != "" && key != "" {
			items[host] = key
		}
	}
	s.mu.Lock()
	s.items = items
	s.mu.Unlock()
}

// Has reports whether any passkeys are configured — used for the
// has_private_trackers flag the agent reports to the site.
func (s *SecretsStore) Has() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items) > 0
}

// List returns hostnames (not keys) for UI display.
func (s *SecretsStore) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.items))
	for k := range s.items {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Get returns the passkey for a host, or "" if none.
func (s *SecretsStore) Get(host string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.items[strings.ToLower(host)]
}

// Set stores host→key and persists the full file with 0600 perms. Empty
// key deletes the entry. Writes are atomic via tmp+rename so a crash
// mid-write can't leave a half-valid secrets.yml.
func (s *SecretsStore) Set(host, key string) error {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == "" {
		delete(s.items, host)
	} else {
		s.items[host] = key
	}
	return s.persistLocked()
}

func (s *SecretsStore) persistLocked() error {
	keys := make([]string, 0, len(s.items))
	for k := range s.items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# Agent-local secrets (passkeys). Never transmitted to the site.\n")
	b.WriteString("# Format: one entry per line, \"host: passkey\".\n\n")
	for _, k := range keys {
		b.WriteString(k + ": " + s.items[k] + "\n")
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

package services

// ComputeOfferHash mirrors handlers.ComputeOfferHash on the site.
// Must match byte-for-byte — same field order, same lowercasing,
// pipe-joined, SHA-1 hex digest. The site uses this hash as the
// dedup key on offer_buckets; the agent uses it to remember which
// local file maps to which site-side bucket.
//
// Empty / zero values encode as empty segments so the byte layout
// stays stable when optional fields aren't present.

import (
	"crypto/sha1"
	"encoding/hex"
	"strconv"
	"strings"
)

func ComputeOfferHash(entityType string, entityID, season, episode int, resolution, sourceTag string) string {
	h := sha1.New()
	parts := []string{
		strings.ToLower(strings.TrimSpace(entityType)),
		strconv.Itoa(entityID),
		strconv.Itoa(season),
		strconv.Itoa(episode),
		strings.ToLower(strings.TrimSpace(resolution)),
		strings.ToLower(strings.TrimSpace(sourceTag)),
	}
	h.Write([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h.Sum(nil))
}

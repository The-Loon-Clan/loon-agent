package services

import (
	"context"
	"log"
	"time"

	"github.com/the-loon-clan/loon-agent/client"
	"github.com/the-loon-clan/loon-agent/storage"
)

// siteGroupsPollInterval is how often we pull the site's catalog when
// steady-state. 5 minutes is slow enough that an admin edit shows up
// "soon but not instantly" without generating background traffic on
// every tick; most operators won't notice the delay.
const siteGroupsPollInterval = 5 * time.Minute

// StartSiteGroupsSync runs a goroutine that pulls the site's curated
// catalog of posting groups into the local DB. Runs an immediate full
// sync on startup (since_version=0) so newly-booted agents get the
// current catalog without waiting five minutes, then switches to
// incremental polling with the last-seen max_version.
//
// The full sync also reconciles deletes — the site has no tombstone
// mechanism, so a site-deleted group would otherwise linger on the
// agent forever. ReconcileSiteGroups removes any local source='site'
// row whose name doesn't appear in the fetched list.
//
// Noops when db or site is nil (the agent falls back gracefully when
// either is unavailable; the offline pipeline still works with purely
// locally-defined groups).
func StartSiteGroupsSync(ctx context.Context, site client.Site, db *storage.DB) {
	if db == nil || site == nil {
		return
	}
	log.Printf("site-groups sync started (interval=%s)", siteGroupsPollInterval)

	// First sync is always a full fetch so we can reconcile deletes.
	// After this, lastVersion tracks the site's high-water mark and the
	// loop polls incrementally.
	lastVersion := 0
	doFullSync := true

	runOnce := func() {
		since := lastVersion
		if doFullSync {
			since = 0
		}
		resp, err := site.FetchAgentGroups(since)
		if err != nil {
			// Maintenance mode returns a typed error; log it at a calmer
			// level since we'll naturally retry on the next tick and
			// there's nothing for the operator to do about it.
			if me, ok := client.IsMaintenanceError(err); ok {
				log.Printf("site-groups sync: site in maintenance (%s) — retrying in %s",
					me.Info.Reason, siteGroupsPollInterval)
				return
			}
			log.Printf("site-groups sync: fetch failed: %v", err)
			return
		}
		upserted := 0
		for _, sg := range resp.Groups {
			g := &storage.Group{
				Name:             sg.Name,
				Type:             sg.Type,
				Newsgroups:       sg.Newsgroups,
				BannedExtensions: sg.BannedExtensions,
				Screenshots:      sg.Screenshots,
				SampleSeconds:    sg.SampleSeconds,
				Par2Redundancy:   sg.Par2Redundancy,
				Obfuscate:        sg.Obfuscate,
				WatermarkText:    sg.WatermarkText,
				Version:          sg.Version,
			}
			ok, err := db.UpsertSiteGroup(g)
			if err != nil {
				log.Printf("site-groups sync: upsert %q: %v", sg.Name, err)
				continue
			}
			if ok {
				upserted++
			} else {
				// Skipped because a local row with the same name exists.
				// Log once per sync per name so the operator knows why the
				// site version isn't taking effect.
				log.Printf("site-groups sync: skipped %q (local override present)", sg.Name)
			}
		}
		if doFullSync {
			// Reconcile deletes: rows the site removed since last sync
			// (or ever, on first boot) that we still carry as source='site'.
			live := make(map[string]bool, len(resp.Groups))
			for _, sg := range resp.Groups {
				live[sg.Name] = true
			}
			if n, err := db.ReconcileSiteGroups(live); err != nil {
				log.Printf("site-groups sync: reconcile failed: %v", err)
			} else if n > 0 {
				log.Printf("site-groups sync: removed %d stale site group(s)", n)
			}
			doFullSync = false
		}
		if resp.MaxVersion > lastVersion {
			lastVersion = resp.MaxVersion
		}
		if upserted > 0 {
			log.Printf("site-groups sync: upserted %d group(s), max_version=%d", upserted, lastVersion)
		}
	}

	runOnce()
	ticker := time.NewTicker(siteGroupsPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

# Offer sources

How the agent decides what to offer. Fulfillment — what happens when a
member requests one — is [OFFER-FULFILLMENT.md](OFFER-FULFILLMENT.md);
this file is about the other end: the scan.

Sources are declared in `offer.json` (path from `OFFER_CONFIG`, default
`<CONFIG_DIR>/offer.json`). Two types exist, and they make different
promises:

- **folder** — walks a local directory. Every offer it registers is a
  file in hand, so the site tags them **have**.
- **scraper** — walks a tracker the operator belongs to. A scraped
  release matched to a file under `downloads_root` is a **have**; one
  the agent has only *seen* is a **can get** — still offerable, but
  fulfilment fetches it from the tracker first. The site shows the tag
  to requesters, so the split is honest by construction.

## The torznab scraper — any tracker with *arr support

Bespoke scrapers exist per tracker (`nyaa`). The `torznab` scraper is
the generic one: point it at any Torznab endpoint — a Prowlarr or
Jackett indexer, or a tracker's native feed — and that tracker works
with no new code. The proxy owns the login, cookies, HTML parsing and
its own upstream rate limits, all on the operator's machine; the site
never sees the tracker credentials or the API key.

The first real source this was built for is **AnimeZ (AnimeTorrents)**
through Prowlarr:

```json
{
  "sources": [
    {
      "type": "scraper",
      "scraper": "torznab",
      "short_name": "animez",
      "torznab_url": "http://localhost:9696/8/api",
      "api_key": "<prowlarr api key>",
      "category_ids": [5070, 2020],
      "downloads_root": "D:/torrents/animez",
      "page_delay_seconds": 20,
      "max_pages_per_tick": 10
    }
  ]
}
```

- `scraper` picks the implementation; `short_name` still names the
  tracker row on the site (add it under /admin/trackers, and the
  agent's owner needs access to it on /account-settings — `personal`
  is the only tracker that auto-attaches).
- `torznab_url` is the "Torznab feed" URL Prowlarr shows for the
  indexer, up to and including `/api`. The scraper appends the query.
- `category_ids` are Torznab category numbers (5070 = TV/Anime).
  Empty walks everything the endpoint returns.
- `downloads_root` is where this tracker's finished downloads land.
  It is what upgrades a scraped row from *can get* to *have*.

## The walk is slow on purpose

Every feed page is a live search against the tracker behind the proxy,
so the scraper takes `max_pages_per_tick` pages per sync tick (default
10) with `page_delay_seconds` between them (default 20, floor 5), and
persists its position in the agent DB (`scrape_cursors`). A 40k-item
catalog is covered across a couple of days of hourly ticks rather than
in one hammering session; when the walk reaches the end it wraps to the
newest, which doubles as the heartbeat keeping registered offers fresh.
The site's per-tracker `scrape_min_seconds` (visible on the tracker's
row) is the reference when tuning the delay down.

Offset paging over a newest-first feed drifts as new releases land: a
few rows repeat across ticks (harmless — registration upserts) and a
few slip a tick (caught on the next full pass).

## What reaches the site

Per release: the raw title (resolved to a catalog id server-side — an
unresolved title registers nothing), season/episode/resolution/source
parsed from it, the size **bucket** (never the exact size), the
info_hash when the feed carries one, and the kind. The .torrent URL
stays local in the agent DB for fulfilment. The site's privacy shape is
unchanged: no titles are stored in offer buckets, and search surfaces
show visibility groups, never the tracker's name.

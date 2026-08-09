# Offer fulfillment

How the agent turns "somebody requested a release I offered" into a delivered
NZB. Two routes, one pipeline.

## The routes

The offer-sync loop registers buckets with the site and remembers, locally,
how this agent could serve each one. There are two kinds of answer, and a
bucket may have both:

| route | when | cost |
|---|---|---|
| **local** | the bytes are already on disk (folder source, or a scraped release paired against `downloads_root`) | none |
| **remote** | no local copy, but the scrape captured a `.torrent` URL | bandwidth + tracker ratio |

Local always wins. The cache (`offer_paths`, migrations 006 + 007) stores the
two routes in separate columns and fills them **per column**, because they are
learned by different passes — a row-level overwrite would mean whichever ran
second erased the other route.

Before migration 007 the URL was discovered at scan time and thrown away, so
tracker-sourced offers were permanently undeliverable: every request against
one was skipped by the fulfill loop, reopened on the claim timeout, and
skipped again, forever.

## Remote fulfillment is off by default

It spends the operator's bandwidth and their tracker ratio. That is not a
decision an agent should start making because it was upgraded.

```
OFFER_REMOTE_FULFILL=true      # opt in
OFFER_REMOTE_MAX_GB=25         # refuse a single request bigger than this (0 = no ceiling)
OFFER_REMOTE_TIMEOUT_MIN=240   # abandon and fail back if it cannot finish in time
```

The size ceiling is checked against the **scraped** size before anything is
fetched. The torrent's real length is checked again by the download path's
disk pre-flight, which is the authority — a scraped figure can be wrong or
missing.

## The flow

1. **Route** the bucket (above). No route, or remote-with-the-feature-off, is
   logged with the request id and skipped — the two cases are logged
   distinctly, because "no route" and "route you turned off" send an operator
   looking in completely different places.
2. **Claim** — always before any expensive work. A remote download for a
   request another offerer already owns wastes real money.
3. **Fetch the `.torrent`** with the source's cookie jar and browser identity.
   Validated as bencode before it reaches the torrent client: a tracker that
   wants a login does **not** answer 401, it answers **200 with an HTML page**,
   and passing that on surfaces thirty seconds later as "malformed .torrent".
   An HTML body is reported as `ErrTorrentAuthWall` — refresh the cookies.
4. **Download** in private mode: DHT and PEX off, so a tracker-sourced info
   hash never reaches the public network even if the `.torrent` forgot to set
   `info.private`. That is how accounts get closed.
5. **Keep the claim alive.** The site's claim TTL is 15 minutes, sized for
   local fulfillment where staging takes seconds; a download is not. The agent
   re-claims every 7 minutes while it works, and the site treats a re-claim by
   the current holder as an extension (nobody else can touch an unexpired
   claim). A refused keepalive is logged loudly — it means another offerer now
   owns the request.
6. **Upload → NZB → deliver** through exactly the same path local fulfillment
   uses. The download's own data directory is what gets walked, so multi-file
   torrents work and nothing is staged twice.

Any failure after the claim calls `/fail`, which reopens the request for other
offerers and bumps this agent's `failed_count`.

## What is not here

- **Torrent-mode delivery.** The proposal's primary mode re-uploads a
  `.torrent` to the site's own tracker; that waits on the tracker itself.
  NZB delivery is the working mode.
- **Seeding back.** The download is fetched, posted and deleted. Seed
  settings (`torrent_seed_ratio` / `torrent_seed_hours`) apply to the
  task-driven pipeline, not to offer fulfillment — an operator who wants to
  seed what they fetched for someone else should say so explicitly, and that
  switch does not exist yet.

# loon cookie bridge (Chrome/Edge extension)

Your private trackers keep you logged in with session cookies that live in
**your** browser. A headless agent on a VPS has no browser and no way to log
in — so scraping and fetching from those trackers has always meant exporting
cookies by hand and re-exporting them every time the tracker rotated a
session.

This extension is the bridge. For each tracker domain you list, it reads that
domain's cookies and sends them to your site's cookie endpoint, which stores
them **encrypted** for your agent to pull on its next sync. You stay logged in
in your browser; your agent stays current automatically.

## What it does and does not touch

- Reads cookies **only** for the domains you list, nothing else.
- Sends them **only** to the one Site URL you configure. No third party, no
  telemetry.
- The site stores them encrypted (AES-256-GCM) and hands them back **only** to
  your own agent, authenticated by your agent token. Nothing renders a jar
  back to a browser.
- Log out of a tracker and the next sync sends an empty set, which tells the
  site to forget that jar.

## Install (unpacked)

1. Open `chrome://extensions` (or `edge://extensions`).
2. Turn on **Developer mode**.
3. **Load unpacked** → pick this `browser-extension/` folder.

## Configure

Click the extension icon:

- **Site URL** — `https://amenzb.moe` (default).
- **Your API key** — Account Settings → API. The same key your *arr apps use.
- **Tracker domains** — one hostname per line, e.g. `animetorrents.me`. Log in
  to each in this browser first.
- **Sync every** — how often to push (minutes; 30 is plenty — sessions rotate
  on the order of days).

**Save**, then **Sync now**. Each domain shows how many cookies it sent.

## Rip this page

Open a tracker's listing (search results, latest, a category page) and click
**Rip current page**. The extension reads the release list off that page —
every magnet / `.torrent` / download link and the release name beside it — and
shows you the ones you have **not seen before** on that domain. The seen list
is stored locally per domain, so each rip surfaces only what is new since your
last one. **Reset seen** clears it for the current domain.

Nothing leaves your browser here — the rip is a local "what's new" list, not a
push. (Cookie sync is the only thing that talks to the site.) It reads whatever
page is in the active tab, so it works on any tracker without per-site setup;
the title heuristic takes the longest link text in each row, which is the
release name on essentially every tracker layout.

## On the agent side

The agent pulls its jars from `GET /api/agent/cookies` and writes them into
the same jar file its torznab / nyaa scrapers already read (see
`docs/OFFER-SOURCES.md`). Nothing in `offer.json` changes — a scraper that
was pointed at a login-walled tracker simply starts seeing a logged-in
session.

## Security notes

- The extension needs the `cookies` permission and host access to read
  session cookies (a page's `document.cookie` cannot see httpOnly login
  cookies; the extension API can). That access is scoped by your domain list
  at read time — the code filters every read to the exact host or a
  dot-subdomain of a listed domain.
- Your API key is stored in the browser's extension storage, local to this
  profile. Treat it like the key it is; rotate it in Account Settings if the
  machine is shared.
- The site pins the API IP the same way it does for the Newznab API: if you
  pin it, pushes must come from the pinned address.

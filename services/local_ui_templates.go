package services

import (
	"html/template"

	"github.com/ameNZB/usenet-pipeline/storage"
)

// The local UI uses one layout template with a sidebar + top bar, and each
// page is a content block composed into it. Keeping all templates inline
// avoids a static-files mount and makes the single-binary deploy story
// work unchanged. When this file grows past ~1000 lines we'll migrate to
// embed.FS with separate .html files.
//
// Two client-side libraries are loaded from CDN:
//   htmx    — drives the forms + future partial-swap interactions.
//   alpine  — small amount of local component state (menus, dialogs).
// Both are loaded at the bottom of the layout so page content renders
// immediately even if the CDN is slow.

const layoutHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>{{block "title" .}}Agent{{end}} — indexer-agent</title>
<link rel="icon" href="data:,"> {{/* suppress favicon 404 without shipping one */}}
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
{{/* Load order: tokens (palette) → shared components (.dot/.alert)
     → agent-shell primitives (.local-shell-scoped cards/rows/bars)
     → agent-specific layout. All four baked into the binary via
     //go:embed — see services/localui_assets.go. */}}
<link rel="stylesheet" href="/_shared/tokens.css">
<link rel="stylesheet" href="/_shared/components.css">
<link rel="stylesheet" href="/_shared/agent-shell.css">
<link rel="stylesheet" href="/static/localui.css">
</head>
<body>
<aside class="sidebar local-shell">
  <div class="brand">
    {{if .SiteURL}}<a href="{{.SiteURL}}" class="wordmark" target="_blank" rel="noopener">{{.SiteName}}</a>{{else}}<span class="wordmark">{{.SiteName}}</span>{{end}}
    <span class="subtitle">agent · local UI</span>
  </div>

  <div class="card">
    <div class="card-head">Agent</div>
    <div class="status-row"><span class="k"><span class="dot dot-success" id="live-agent-dot"></span>State</span><span class="v" id="live-phase">idle</span></div>
    <div class="status-row"><span class="k">VPN</span><span class="v"><span class="dot {{if eq .VPNStatus "Connected"}}dot-success{{else if eq .VPNStatus "Disconnected"}}dot-danger{{else}}dot-warning{{end}}" id="live-vpn-dot"></span><span id="live-vpn">{{.VPNStatus}}</span></span></div>
    <div class="status-row"><span class="k">Public IP</span><span class="v" id="live-ip">{{.PublicIP}}</span></div>
  </div>

  <div class="card">
    <div class="card-head">Throughput</div>
    <div class="speed-row">
      <span>↓ <span class="down num" id="live-down">0</span><span class="unit" id="live-down-unit">B/s</span></span>
      <span>↑ <span class="up num" id="live-up">0</span><span class="unit" id="live-up-unit">B/s</span></span>
    </div>
    {{/* Split upload throughput row: shown only when at least one bucket
         is non-zero. Hidden by default so an idle agent's sidebar stays
         tight. NZB (Usenet POST) is the blue accent; seed-back (BT) is
         amber so it visually reads as "background" traffic distinct from
         the headline NZB work the agent exists to do. */}}
    <div class="speed-row split" id="live-up-split" style="display:none;font-size:11px;color:var(--text-muted);">
      <span>📡 NZB <span id="live-up-nzb">0 B/s</span></span>
      <span>🔄 Seed <span id="live-up-seed">0 B/s</span></span>
    </div>
    <svg id="speed-graph" class="speed-graph" viewBox="0 0 100 30" preserveAspectRatio="none"></svg>
  </div>

  <div class="card">
    <div class="card-head">Disk</div>
    <div class="status-row"><span class="k">Free</span><span class="v" id="live-disk-free">—</span></div>
    <div class="status-row"><span class="k">Reserved</span><span class="v" id="live-disk-reserved">—</span></div>
    {{/* Usage bar — rendered only when the SSE feed includes disk_total_gb
         > 0 (Linux hosts; stub platforms send 0 and we hide the bar to
         avoid lying with fake values). Fill color shifts red past 85%
         so operators notice before ENOSPC kills a job. */}}
    <div id="live-disk-usage-wrap" style="display:none;margin-top:6px;">
      <div class="disk-bar-track"><div class="disk-bar-fill" id="live-disk-bar" style="width:0%;"></div></div>
      <div class="status-row" style="margin-top:2px;"><span class="k" id="live-disk-pct">—</span><span class="v small" id="live-disk-total"></span></div>
    </div>
  </div>

  <div class="nav-group">
    <div class="head">Navigate</div>
    {{/* Active Mirror tasks first — most operators land here to
         answer "what's the agent doing right now?". Jobs below is
         the OFFLINE-mode queue (watch-folder hand-off), labelled to
         disambiguate from the live Mirror view. */}}
    <a href="/mirror"{{if eq .CurrentPage "mirror"}} class="active"{{end}}>Active downloads</a>
    <a href="/jobs"{{if eq .CurrentPage "jobs"}} class="active"{{end}}>Offline Jobs<span class="count" id="nav-jobs-count"></span></a>
    <a href="/watches"{{if eq .CurrentPage "watches"}} class="active"{{end}}>Watch Folders</a>
    <a href="/groups"{{if eq .CurrentPage "groups"}} class="active"{{end}}>Groups</a>
    <a href="/"{{if eq .CurrentPage "config"}} class="active"{{end}}>Config</a>
    {{if .SiteURL}}<a href="{{.SiteURL}}" target="_blank" rel="noopener" class="external">Site dashboard <span class="external-arrow">↗</span></a>{{end}}
  </div>

  {{/* Footer is honest about the bind: when bound to loopback the
       "local-only" badge is green-ish; when bound to anything else
       (e.g. 0.0.0.0 behind a reverse proxy, or a LAN IP) the badge
       turns red so the operator can't miss that this UI — which has
       no auth — is reachable off-host. */}}
  <div class="footer">
    {{if or (eq .BindAddr "127.0.0.1") (eq .BindAddr "localhost") (eq .BindAddr "::1")}}
    <span class="badge ok">loopback-only</span> bind: <code>{{.BindAddr}}</code>
    {{else}}
    <span class="badge err">exposed</span> bind: <code>{{.BindAddr}}</code>
    <div style="margin-top:4px;color:var(--red);">This UI has no auth — put a reverse proxy with auth in front of it or rebind to 127.0.0.1.</div>
    {{end}}
  </div>
</aside>

<div class="main">
  {{/* Top-level mode strip — three exclusive entry points, one
       always lit. Mirror is the original request-claim pipeline,
       Offers advertises local content for site requests, Collection
       enriches + uploads local files directly to usenet + the site.
       The strip sits above the topbar so every page in every mode
       starts with the same "where am I in the agent" answer. */}}
  <nav class="mode-tabs" aria-label="Mode">
    <a href="/mirror"     class="mode-tab{{if eq .CurrentMode "mirror"}}     is-active{{end}}">Mirror     <span class="mode-tab-sub">torrent → usenet</span></a>
    <a href="/offers"     class="mode-tab{{if eq .CurrentMode "offers"}}     is-active{{end}}">Offers     <span class="mode-tab-sub">torrent/disk → usenet</span></a>
    <a href="/collection" class="mode-tab{{if eq .CurrentMode "collection"}} is-active{{end}}">Collection <span class="mode-tab-sub">on disk → usenet</span></a>
  </nav>
  <header class="topbar">
    <h1>{{template "title" .}}</h1>
    <div class="spacer"></div>
    {{block "topbar-actions" .}}{{end}}
  </header>
  <main class="content">
    {{if .Flash}}<div class="alert alert-success">{{.Flash}}</div>{{end}}
    {{if .Err}}<div class="alert alert-danger">{{.Err}}</div>{{end}}
    {{block "content" .}}<p>no content</p>{{end}}
  </main>
</div>

<script src="https://unpkg.com/htmx.org@2.0.4" defer></script>
<script src="https://unpkg.com/alpinejs@3.14.9/dist/cdn.min.js" defer></script>
<script>
// SSE client for the sidebar live status. One EventSource per tab,
// reconnects automatically via the browser's built-in EventSource retry.
// Kept tiny + framework-free because the sidebar is on every page and
// loading Alpine/HTMX would be overkill for "set 8 text nodes."
(function() {
  const MAX_POINTS = 60; // ~90s at the 1.5s SSE cadence
  // Three rolling buffers — download (green), NZB upload (blue), seed
  // upload (amber). Older agents that only emit the combined
  // upload_mbps land in the NZB bucket; that's the conservative
  // fallback since pre-split agents posted NZB-dominant traffic during
  // the upload window anyway.
  const dl = [], nzb = [], seed = [];
  const graph = document.getElementById('speed-graph');

  // fmtSpeed takes MB/s and returns [number, unit] tuned for readability:
  // big speeds in MB/s, smaller in KB/s, idle in B/s. No floating "0.02"
  // MB/s that requires squinting at the page.
  function fmtSpeed(mbps) {
    if (!mbps || mbps < 0) return ['0', 'B/s'];
    if (mbps >= 1) return [mbps.toFixed(2), 'MB/s'];
    const kbs = mbps * 1024;
    if (kbs >= 1) return [kbs.toFixed(0), 'KB/s'];
    return [Math.round(mbps * 1024 * 1024).toString(), 'B/s'];
  }

  function setText(id, txt) {
    const el = document.getElementById(id);
    if (el && el.textContent !== String(txt)) el.textContent = txt;
  }
  function setDot(id, variant) {
    // variant is "success" | "danger" | "warning" (matches .dot-* suffix in components.css).
    const el = document.getElementById(id);
    if (!el) return;
    el.className = 'dot dot-' + variant;
  }

  // redrawGraph draws download + NZB upload + seed-back as three
  // polylines, scaled to the rolling window's own peak so a low-activity
  // period still renders as something visible rather than a flat line on
  // the x-axis. Colors come from tokens.css so palette tweaks propagate
  // here automatically.
  const svgNS = 'http://www.w3.org/2000/svg';
  function themeColor(name, fallback) {
    const v = getComputedStyle(document.documentElement).getPropertyValue(name);
    return (v && v.trim()) || fallback;
  }
  function redrawGraph() {
    if (!graph) return;
    while (graph.firstChild) graph.removeChild(graph.firstChild);
    if (dl.length < 2) return;
    let peak = 0;
    for (const v of dl)   if (v > peak) peak = v;
    for (const v of nzb)  if (v > peak) peak = v;
    for (const v of seed) if (v > peak) peak = v;
    if (peak < 0.05) peak = 0.05;
    const drawLine = (arr, stroke) => {
      const pts = arr.map((v, i) => {
        const x = (i / (MAX_POINTS - 1)) * 100;
        const y = 30 - (v / peak) * 28;
        return x.toFixed(1) + ',' + y.toFixed(1);
      }).join(' ');
      const p = document.createElementNS(svgNS, 'polyline');
      p.setAttribute('points', pts);
      p.setAttribute('stroke', stroke);
      p.setAttribute('fill', 'none');
      p.setAttribute('stroke-width', '1.2');
      graph.appendChild(p);
    };
    drawLine(dl, themeColor('--green', '#22c55e'));
    // Only draw upload lines that have something to draw — avoids a
    // distracting flat line pinned to the x-axis during pure downloads.
    if (nzb.some(v => v > 0))  drawLine(nzb,  themeColor('--blue', '#5b8af5'));
    if (seed.some(v => v > 0)) drawLine(seed, themeColor('--warn', '#f59e0b'));
  }

  function onStatus(d) {
    // The agent posts the upload total plus two named buckets:
    //   nzb_upload_mbps  — NNTP POST traffic
    //   seed_upload_mbps — BitTorrent seed-back
    // Older agents only post upload_mbps; in that case we treat the
    // total as NZB (the dominant case during a normal upload window)
    // and the seed line stays at zero.
    const nzbMbps  = (typeof d.nzb_upload_mbps  === 'number') ? d.nzb_upload_mbps  : (d.upload_mbps || 0);
    const seedMbps = (typeof d.seed_upload_mbps === 'number') ? d.seed_upload_mbps : 0;
    const [dv, du] = fmtSpeed(d.download_mbps);
    const [uv, uu] = fmtSpeed(d.upload_mbps);
    setText('live-down', dv); setText('live-down-unit', du);
    setText('live-up', uv);   setText('live-up-unit', uu);

    // Split row: only show when at least one bucket is non-zero, and
    // only show both halves when both are non-zero (idle seed during
    // an active upload reads cleaner as just "NZB X"). Numbers stay
    // muted to keep the headline ↑ value the primary read.
    const upSplit = document.getElementById('live-up-split');
    if (upSplit) {
      const showNzb  = nzbMbps  > 0;
      const showSeed = seedMbps > 0;
      if (showNzb || showSeed) {
        const [nv, nu] = fmtSpeed(nzbMbps);
        const [sv, su] = fmtSpeed(seedMbps);
        setText('live-up-nzb',  showNzb  ? (nv + ' ' + nu) : '0 B/s');
        setText('live-up-seed', showSeed ? (sv + ' ' + su) : '0 B/s');
        upSplit.style.display = (showNzb && showSeed) ? '' : 'none';
      } else {
        upSplit.style.display = 'none';
      }
    }
    setText('live-phase', d.phase || 'idle');
    setText('live-vpn', d.vpn_status || 'Unknown');
    setText('live-ip', d.public_ip || '—');

    // Agent "State" dot green when doing useful work, amber when idle —
    // lets the operator see at a glance whether the pipeline is flowing.
    setDot('live-agent-dot', d.phase && d.phase !== 'idle' ? 'success' : 'warning');
    setDot('live-vpn-dot',
      d.vpn_status === 'Connected' ? 'success' :
      d.vpn_status === 'Disconnected' ? 'danger' : 'warning');

    if (typeof d.disk_free_gb === 'number') {
      setText('live-disk-free', d.disk_free_gb.toFixed(1) + ' GB');
    }
    if (typeof d.disk_reserved_gb === 'number') {
      setText('live-disk-reserved', d.disk_reserved_gb.toFixed(1) + ' GB');
    }
    // Usage bar: compute used% from (total - free - reserved) / total.
    // Stub platforms send disk_total_gb = 0; leave the bar hidden then.
    const wrap = document.getElementById('live-disk-usage-wrap');
    if (wrap && d.disk_total_gb > 0) {
      const reserved = d.disk_reserved_gb || 0;
      const used = d.disk_total_gb - d.disk_free_gb - reserved;
      const pct = Math.max(0, Math.min(100, (used / d.disk_total_gb) * 100));
      const fill = document.getElementById('live-disk-bar');
      if (fill) {
        fill.style.width = pct.toFixed(1) + '%';
        fill.className = 'disk-bar-fill' +
          (pct >= 95 ? ' danger' : pct >= 85 ? ' warn' : '');
      }
      setText('live-disk-pct', pct.toFixed(0) + '% used');
      setText('live-disk-total', 'of ' + d.disk_total_gb.toFixed(0) + ' GB');
      wrap.style.display = '';
    }

    if (d.jobs) {
      // Active = queued + processing; surfaces work-in-flight on the nav
      // label. Completed/failed history doesn't belong next to the link.
      const active = (d.jobs.queued || 0) + (d.jobs.processing || 0);
      setText('nav-jobs-count', active > 0 ? active : '');
    }

    dl.push(d.download_mbps || 0);
    nzb.push(nzbMbps);
    seed.push(seedMbps);
    if (dl.length   > MAX_POINTS) dl.shift();
    if (nzb.length  > MAX_POINTS) nzb.shift();
    if (seed.length > MAX_POINTS) seed.shift();
    redrawGraph();
  }

  try {
    const es = new EventSource('/events');
    es.addEventListener('status', ev => {
      try { onStatus(JSON.parse(ev.data)); } catch (e) {}
    });
    // EventSource auto-reconnects on network errors; we only flag it
    // visually so the operator knows the sidebar isn't lying.
    es.addEventListener('error', () => setDot('live-agent-dot', 'warning'));
  } catch (e) { /* old browser / CSP; sidebar stays static */ }
})();
</script>
</body>
</html>`

// buildPageTemplate composes the shared layout with a page-specific
// content block. Each page calls this once at package init so parse
// errors surface on boot rather than on first request.
func buildPageTemplate(name, src string, funcs template.FuncMap) *template.Template {
	t := template.New(name)
	if funcs != nil {
		t = t.Funcs(funcs)
	}
	return template.Must(template.Must(t.Parse(layoutHTML)).Parse(src))
}

// baseData fills in the fields the layout needs. Every handler wraps
// its page-specific data with this so sidebar/status rendering doesn't
// require each handler to remember the full field set.
func (u *LocalUI) baseData(currentPage string, flash, errMsg string) map[string]any {
	vpn := "Unknown"
	ip := "—"
	if storage.GlobalState.VPNStatus != "" {
		storage.GlobalState.RLock()
		vpn = storage.GlobalState.VPNStatus
		if storage.GlobalState.PublicIP != "" {
			ip = storage.GlobalState.PublicIP
		}
		storage.GlobalState.RUnlock()
	}
	// SiteName falls back to "indexer-site" rather than a brand
	// string so a fresh fork without SITE_NAME set still looks
	// finished, but doesn't carry the upstream's brand into a
	// downstream operator's UI.
	siteName := u.cfg.SiteName
	if siteName == "" {
		siteName = "indexer-site"
	}
	return map[string]any{
		"CurrentPage": currentPage,
		"CurrentMode": modeForPage(currentPage),
		"Flash":       flash,
		"Err":         errMsg,
		"BindAddr":    u.bindAddr,
		"VPNStatus":   vpn,
		"PublicIP":    ip,
		"SiteURL":     u.cfg.SiteURL,
		"SiteName":    siteName,
	}
}

// modeForPage derives the top-level mode (mirror / offers / collection)
// from the page slug each handler passes to baseData. Mirror is the
// catch-all so legacy pages (jobs / watches / groups / config / etc)
// keep the existing behavior — they ARE Mirror. Newer surfaces declare
// their mode by using a slug prefix.
//
// Three modes:
//
//   mirror     — request-claim pipeline (the agent's original purpose).
//                Pulls site requests, fetches torrent → posts NZB →
//                reports complete.
//   offers     — local files / torrents the agent advertises to fulfil
//                site requests on demand (see services/offer_*).
//   collection — self-initiated: walks a local root, enriches per file
//                via /api/agent/title-match-bulk, posts to usenet, and
//                pushes the resulting NZB to the site.
//
// The function is intentionally simple so a future fourth mode is one
// case statement plus a slug prefix.
func modeForPage(page string) string {
	switch {
	case page == "offers" || len(page) > 7 && page[:7] == "offers/":
		return "offers"
	case page == "collection" || len(page) > 11 && page[:11] == "collection/":
		return "collection"
	default:
		return "mirror"
	}
}

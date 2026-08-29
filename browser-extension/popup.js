// Popup: edit config, save it, trigger a sync, and show the last result.

const $ = (id) => document.getElementById(id);

function parseDomains(text) {
  return text
    .split(/\r?\n/)
    .map((s) => s.trim().toLowerCase().replace(/^https?:\/\//, "").replace(/\/.*$/, "").replace(/^\./, ""))
    .filter(Boolean);
}

async function load() {
  const c = await chrome.storage.local.get(["siteUrl", "apiKey", "domains", "intervalMinutes", "lastSync", "ripLast"]);
  $("siteUrl").value = c.siteUrl || "https://amenzb.moe";
  $("apiKey").value = c.apiKey || "";
  $("domains").value = (c.domains || []).join("\n");
  $("interval").value = c.intervalMinutes || 30;
  render(c.lastSync);
  renderRip(c.ripLast);
}

function render(last) {
  const el = $("status");
  if (!last) { el.className = "muted"; el.textContent = "Not synced yet."; return; }
  if (last.error) { el.className = "bad"; el.textContent = "Error: " + last.error; return; }
  const when = new Date(last.at).toLocaleTimeString();
  const lines = (last.results || []).map((r) => {
    if (r.ok && r.deleted) return `<div class="line"><span>${r.domain}</span><span class="muted">logged out — forgotten</span></div>`;
    if (r.ok) return `<div class="line"><span>${r.domain}</span><span class="ok">${r.stored} cookies</span></div>`;
    return `<div class="line"><span>${r.domain}</span><span class="bad">${r.error}</span></div>`;
  });
  el.className = "";
  el.innerHTML = `<div class="muted">Last sync ${when}</div>` + (lines.join("") || `<div class="muted">no domains listed</div>`);
}

async function save() {
  await chrome.storage.local.set({
    siteUrl: $("siteUrl").value.trim().replace(/\/+$/, ""),
    apiKey: $("apiKey").value.trim(),
    domains: parseDomains($("domains").value),
    intervalMinutes: Math.max(5, parseInt($("interval").value, 10) || 30),
  });
  await chrome.runtime.sendMessage({ type: "rearm" });
}

$("save").addEventListener("click", async () => {
  await save();
  $("status").className = "ok";
  $("status").textContent = "Saved.";
});

$("sync").addEventListener("click", async () => {
  await save();
  $("status").className = "muted";
  $("status").textContent = "Syncing…";
  const res = await chrome.runtime.sendMessage({ type: "sync-now" });
  render(res);
});

// ─── Page ripper ────────────────────────────────────────────────────────
//
// extractReleases runs IN THE PAGE (via chrome.scripting.executeScript), so it
// must be entirely self-contained — no closure over popup variables. It looks
// for magnet / .torrent / download links, and for each takes the release name
// from the longest link text in its row (which is almost always the title on
// a tracker listing). Returns [{title, href, hash}].
function extractReleases() {
  const rowTitle = (a) => {
    const row = a.closest(
      'tr, li, article, .row, [class*="torrent"], [class*="release"], [class*="result"]'
    );
    if (!row) return "";
    let best = "";
    for (const link of row.querySelectorAll("a")) {
      const t = (link.textContent || "").trim();
      if (t.length > best.length) best = t;
    }
    return best || (row.textContent || "").trim().slice(0, 200);
  };
  const found = [];
  for (const a of document.querySelectorAll("a[href]")) {
    const href = a.href || "";
    let hash = "";
    let isRelease = false;
    const m = href.match(/xt=urn:btih:([a-z0-9]+)/i);
    if (href.startsWith("magnet:") && m) {
      hash = m[1].toLowerCase();
      isRelease = true;
    } else if (/\.torrent(\?|#|$)/i.test(href)) {
      isRelease = true;
    } else if (/\/(download|torrent|dl|get)\/[^/]+/i.test(href)) {
      isRelease = true;
    }
    if (!isRelease) continue;
    let title = (a.textContent || "").trim();
    if (title.length < 5 || /^(magnet|download|dl|torrent|get)\b/i.test(title) || /^[\d.,\s]*(kb|mb|gb|tb)?$/i.test(title)) {
      title = rowTitle(a) || title;
    }
    title = title.replace(/\s+/g, " ").trim();
    if (!title) continue;
    found.push({ title, href: href.startsWith("magnet:") ? "" : href, hash });
  }
  // Dedupe within the page by infohash (durable) or title.
  const seen = new Set();
  const out = [];
  for (const r of found) {
    const key = r.hash || r.title.toLowerCase();
    if (seen.has(key)) continue;
    seen.add(key);
    out.push(r);
  }
  return out.slice(0, 1000);
}

// renderRip builds the results with textContent only. Ripped titles are
// arbitrary web-page content — innerHTML here would let a hostile page run
// script in the popup's privileged context.
function renderRip(last) {
  const status = $("ripstatus");
  const list = $("riplist");
  list.textContent = "";
  if (!last) { status.className = "muted"; status.textContent = "No page ripped yet."; return; }
  if (last.error) { status.className = "bad"; status.textContent = "Error: " + last.error; return; }
  const when = new Date(last.at).toLocaleTimeString();
  status.className = "";
  status.textContent = `${last.domain} · ${when} · ${last.total} on page · ${last.newCount} new`;
  for (const r of last.new || []) {
    const row = document.createElement("div");
    row.className = "riprow clip";
    row.textContent = r.title;
    row.title = r.title;
    list.appendChild(row);
  }
  if ((last.newCount || 0) === 0) {
    const none = document.createElement("div");
    none.className = "muted";
    none.textContent = "Nothing new since your last rip here.";
    list.appendChild(none);
  }
}

async function ripCurrentPage() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (!tab || !tab.id || !/^https?:/.test(tab.url || "")) {
    return { error: "the active tab is not a web page" };
  }
  const domain = new URL(tab.url).hostname.replace(/^www\./, "");
  let injected;
  try {
    injected = await chrome.scripting.executeScript({ target: { tabId: tab.id }, func: extractReleases });
  } catch (e) {
    return { error: String((e && e.message) || e) };
  }
  const releases = (injected && injected[0] && injected[0].result) || [];
  const store = await chrome.storage.local.get(["ripSeen"]);
  const seenAll = store.ripSeen || {};
  const seen = seenAll[domain] || {};
  const now = Date.now();
  const fresh = [];
  for (const r of releases) {
    const key = r.hash || r.title.toLowerCase();
    if (!(key in seen)) fresh.push(r);
    seen[key] = now;
  }
  // Bound the per-domain memory: keep the newest 5000 keys.
  const entries = Object.entries(seen);
  seenAll[domain] = entries.length > 5000
    ? Object.fromEntries(entries.sort((a, b) => b[1] - a[1]).slice(0, 5000))
    : seen;
  const result = { domain, at: now, total: releases.length, newCount: fresh.length, new: fresh.slice(0, 200) };
  await chrome.storage.local.set({ ripSeen: seenAll, ripLast: result });
  return result;
}

$("rip").addEventListener("click", async () => {
  $("ripstatus").className = "muted";
  $("ripstatus").textContent = "Ripping…";
  $("riplist").textContent = "";
  renderRip(await ripCurrentPage());
});

$("ripforget").addEventListener("click", async () => {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  const domain = tab && tab.url && /^https?:/.test(tab.url)
    ? new URL(tab.url).hostname.replace(/^www\./, "") : "";
  const store = await chrome.storage.local.get(["ripSeen"]);
  const seenAll = store.ripSeen || {};
  if (domain) delete seenAll[domain];
  await chrome.storage.local.set({ ripSeen: seenAll });
  await chrome.storage.local.remove("ripLast");
  $("ripstatus").className = "muted";
  $("ripstatus").textContent = domain ? `Cleared seen list for ${domain}.` : "Cleared.";
  $("riplist").textContent = "";
});

load();

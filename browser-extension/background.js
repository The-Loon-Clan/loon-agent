// loon cookie bridge — service worker.
//
// One job: for each tracker domain the operator listed, read that domain's
// cookies and POST them to the site's /api/browser/cookies endpoint, which
// stores them encrypted for the operator's agent to pull. Runs on a timer
// and on demand from the popup.
//
// It never reads a cookie for a domain the operator did not list, and it
// only ever talks to the one site URL the operator configured. There is no
// telemetry and no third party — the two destinations are the tracker
// (read) and your own site (write).

const ALARM = "loon-cookie-sync";

// cfg: { siteUrl, apiKey, domains: [<hostname>...], intervalMinutes }
async function getConfig() {
  const c = await chrome.storage.local.get(["siteUrl", "apiKey", "domains", "intervalMinutes"]);
  return {
    siteUrl: (c.siteUrl || "").replace(/\/+$/, ""),
    apiKey: c.apiKey || "",
    domains: Array.isArray(c.domains) ? c.domains : [],
    intervalMinutes: c.intervalMinutes || 30,
  };
}

// Collect every cookie scoped to a domain into a name->value map. We ask by
// domain rather than by URL so httpOnly session cookies (the ones that
// matter for a login) are included — chrome.cookies.getAll returns them to
// an extension with the cookies permission, which a page's document.cookie
// never could.
async function cookiesForDomain(domain) {
  const all = await chrome.cookies.getAll({ domain });
  const jar = {};
  for (const ck of all) {
    // getAll on "example.com" also returns cookies for unrelated domains
    // that merely end in it (…notexample.com). Keep only real scope
    // matches: the exact host or a dot-subdomain of it.
    const d = ck.domain.replace(/^\./, "");
    if (d === domain || d.endsWith("." + domain)) jar[ck.name] = ck.value;
  }
  return jar;
}

async function pushDomain(cfg, domain) {
  const cookies = await cookiesForDomain(domain);
  const res = await fetch(cfg.siteUrl + "/api/browser/cookies", {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Api-Key": cfg.apiKey },
    body: JSON.stringify({ domain, cookies }),
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok || !body.ok) {
    throw new Error(body.error || ("HTTP " + res.status));
  }
  return body; // { ok, domain, stored } or { ok, domain, deleted }
}

// syncAll returns a per-domain result the popup renders. It never throws:
// one tracker being logged out must not stop the others syncing.
async function syncAll() {
  const cfg = await getConfig();
  const out = { at: Date.now(), results: [] };
  if (!cfg.siteUrl || !cfg.apiKey) {
    out.error = "not configured";
    await chrome.storage.local.set({ lastSync: out });
    return out;
  }
  for (const domain of cfg.domains) {
    try {
      const r = await pushDomain(cfg, domain);
      out.results.push({ domain, ok: true, stored: r.stored || 0, deleted: !!r.deleted });
    } catch (e) {
      out.results.push({ domain, ok: false, error: String(e.message || e) });
    }
  }
  await chrome.storage.local.set({ lastSync: out });
  return out;
}

async function rearm() {
  const cfg = await getConfig();
  await chrome.alarms.clear(ALARM);
  chrome.alarms.create(ALARM, { periodInMinutes: Math.max(5, cfg.intervalMinutes) });
}

chrome.alarms.onAlarm.addListener((a) => {
  if (a.name === ALARM) syncAll();
});

chrome.runtime.onInstalled.addListener(rearm);
chrome.runtime.onStartup.addListener(rearm);

// The popup drives a manual sync and re-arms the timer after a config edit.
chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (msg.type === "sync-now") {
    syncAll().then(sendResponse);
    return true; // async
  }
  if (msg.type === "rearm") {
    rearm().then(() => sendResponse({ ok: true }));
    return true;
  }
});

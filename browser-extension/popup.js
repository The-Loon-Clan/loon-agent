// Popup: edit config, save it, trigger a sync, and show the last result.

const $ = (id) => document.getElementById(id);

function parseDomains(text) {
  return text
    .split(/\r?\n/)
    .map((s) => s.trim().toLowerCase().replace(/^https?:\/\//, "").replace(/\/.*$/, "").replace(/^\./, ""))
    .filter(Boolean);
}

async function load() {
  const c = await chrome.storage.local.get(["siteUrl", "apiKey", "domains", "intervalMinutes", "lastSync"]);
  $("siteUrl").value = c.siteUrl || "https://amenzb.moe";
  $("apiKey").value = c.apiKey || "";
  $("domains").value = (c.domains || []).join("\n");
  $("interval").value = c.intervalMinutes || 30;
  render(c.lastSync);
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

load();

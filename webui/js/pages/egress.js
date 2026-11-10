// Page: egress and relay status. It keeps the free-tier /64 budget table and
// adds one row per WARP exit, proxy and peer so operators can see each egress
// IP and its live state without reading the raw health document.
import { api, hasSession } from "../api.js";
import { refreshMonitor } from "../data.js";
import { t, onChange } from "../i18n.js";
import { $, el, localTime, toast } from "../ui.js";
import * as store from "../store.js";

let timer = null;
let loading = false;
let visible = false;
let filter = "all";
let search = "";
let snapshot = null;

function stateLabel(state) {
  switch (state) {
    case "cooling":
      return t("Đang nghỉ", "egress.state.cooling");
    case "standby":
      return t("Dự phòng", "egress.state.standby");
    case "blocked":
      return t("Bị chặn", "egress.state.blocked");
    case "checking":
      return t("Đang kiểm tra", "egress.state.checking");
    case "unavailable":
      return t("Không dùng được", "egress.state.unavailable");
    case "active":
      return t("Sẵn sàng", "egress.state.active");
    default:
      return t("Chưa dùng", "egress.state.idle");
  }
}

function stateBadge(state) {
  const cls =
    state === "active" || state === "standby"
      ? "status ok"
      : state === "cooling" || state === "checking"
        ? "status warn"
        : state === "blocked" || state === "unavailable"
          ? "status bad"
          : "status";
  return el("span", { class: cls, text: stateLabel(state) });
}

function duration(seconds) {
  if (!seconds || seconds <= 0) return "—";
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.round((seconds % 3600) / 60);
  if (hours <= 0) return `${minutes} ${t("phút", "egress.minutes")}`;
  return `${hours} ${t("giờ", "egress.hours")} ${minutes} ${t("phút", "egress.minutes")}`;
}

function secondsUntil(value) {
  if (!value) return 0;
  const at = Date.parse(value);
  if (!Number.isFinite(at)) return 0;
  return Math.max(0, Math.round((at - Date.now()) / 1000));
}

function shortID(value) {
  const text = String(value || "");
  return text.length > 8 ? `${text.slice(0, 8)}…` : text;
}

// The upstream counts an IPv6 egress by its /64, so the budget board keys rows
// on the first four hextets while the pool reports full addresses. Normalising
// both sides here is what lets one table show "this exit" and "its bucket".
function prefix64(address) {
  const text = String(address || "").trim();
  if (!text.includes(":")) return text;
  return text.split(":").slice(0, 4).join(":");
}

function quotaIndex() {
  const index = new Map();
  for (const row of snapshot?.exits || []) {
    const key = prefix64(row.egress);
    if (!key) continue;
    const entry =
      index.get(key) || { requests: 0, quotaHits: 0, lastQuotaAt: 0, resetAt: "" };
    entry.requests += row.requests || 0;
    entry.quotaHits += row.quota_hits || 0;
    entry.resetAt = row.reset_at || entry.resetAt;
    const at = Date.parse(row.last_quota_at || "");
    if (Number.isFinite(at) && at > entry.lastQuotaAt) entry.lastQuotaAt = at;
    index.set(key, entry);
  }
  return index;
}

function quotaCell(quota) {
  if (!quota) return el("td", { text: "—" });
  return el("td", { class: "num mono", text: String(quota.requests || 0) });
}

function lastQuotaCell(quota) {
  if (!quota?.lastQuotaAt) return el("td", { text: "—" });
  return el("td", { class: "mono", text: localTime(new Date(quota.lastQuotaAt).toISOString()) });
}

function sourceRow(kind, name, address, note, state, load, cooling, quota) {
  return { kind, name, address, note, state, load, cooling, quota };
}

function warpRows(warp, quota) {
  const exits = warp?.exits || [];
  const egress = warp?.egress || [];
  if (!exits.length && !egress.length) {
    return warp?.enabled
      ? [
          sourceRow(
            "warp",
            `${t("WARP", "egress.sources.warp")} · ${warp.stack || "ipv6"}`,
            "—",
            t("Đang đăng ký exit, IP sẽ xuất hiện khi pool sẵn sàng.", "egress.sources.warming"),
            "checking",
            "—",
            0,
          ),
        ]
      : [];
  }
  if (!exits.length && egress.length) {
    // Older gateways exposed only the egress IP list. Keep those IPs visible
    // instead of rendering an empty table after an upgrade.
    return warp.egress.map((address, index) =>
      sourceRow(
        "warp",
        `${t("WARP", "egress.sources.warp")} · ${index + 1}`,
        address || "—",
        "",
        warp.active > 0 ? "active" : "standby",
        "—",
        0,
        quota?.get(prefix64(address)),
      ),
    );
  }
  return exits.map((exit) => {
    const state = exit.cooling ? "cooling" : exit.in_flight > 0 ? "active" : "standby";
    const address = exit.egress || exit.egress_key || "—";
    const note = [
      exit.egress_key && exit.egress_key !== exit.egress ? exit.egress_key : "",
      exit.source ? `${t("nguồn ngoài", "egress.sources.outer")}: ${exit.source}` : "",
    ]
      .filter(Boolean)
      .join(" · ");
    return sourceRow(
      "warp",
      `${t("WARP", "egress.sources.warp")}${exit.id ? ` · ${shortID(exit.id)}` : ""}`,
      address,
      note,
      state,
      exit.in_flight || 0,
      exit.cooling ? secondsUntil(exit.cooling_until) : 0,
      quota?.get(prefix64(address)),
    );
  });
}

function proxyRows(proxies) {
  return (proxies || []).map((proxy) => {
    const state = proxy.blocked_until
      ? "blocked"
      : proxy.checking
        ? "checking"
        : proxy.healthy
          ? "active"
          : "unavailable";
    const binding = [`Z ${proxy.zen_keys || 0}`, `G ${proxy.go_keys || 0}`];
    if (proxy.pool) binding.push(t("pool xoay", "egress.sources.pool"));
    if (proxy.last_checked) {
      binding.push(t("kiểm tra {0}", "egress.sources.checked", localTime(proxy.last_checked)));
    }
    return sourceRow(
      "proxy",
      `${t("Proxy", "egress.sources.proxy")} #${(proxy.index ?? 0) + 1}`,
      proxy.address || "—",
      binding.join(" · "),
      state,
      "—",
      proxy.blocked_until ? secondsUntil(proxy.blocked_until) : 0,
    );
  });
}

function peerRows(peers) {
  return (peers || []).map((peer) => {
    const cooling = Number(peer.cooling_seconds || 0);
    const tier = peer.tier ? t("tier {0}", "egress.sources.tier", peer.tier) : "";
    return sourceRow(
      "peer",
      `${t("Peer", "egress.sources.peer")} · ${peer.name || "—"}`,
      peer.base_url || "—",
      tier,
      cooling > 0 ? "cooling" : "active",
      "—",
      cooling,
    );
  });
}

const STATE_ORDER = {
  blocked: 0,
  unavailable: 1,
  cooling: 2,
  checking: 3,
  standby: 4,
  active: 5,
};

function renderSources() {
  const body = $("#egress-sources");
  if (!body) return;
  const resources = store.get("monitor")?.resources || {};
  const warp = resources.warp;
  const proxies = resources.proxies || [];
  const peers = resources.peers || [];
  const quota = quotaIndex();
  const rows = [
    ...warpRows(warp, quota),
    ...proxyRows(proxies),
    ...peerRows(peers),
  ].sort((a, b) => (STATE_ORDER[a.state] ?? 99) - (STATE_ORDER[b.state] ?? 99));

  $("#egress-m-warp").textContent = warp?.enabled
    ? `${warp.active || 0}/${warp.size || 0}`
    : "—";
  $("#egress-m-proxies").textContent = proxies.length
    ? `${proxies.filter((proxy) => proxy.healthy).length}/${proxies.length}`
    : "—";
  const readyPeers = peers.filter((peer) => !peer.cooling_seconds).length;
  $("#egress-m-peers").textContent = peers.length ? `${readyPeers}/${peers.length}` : "—";
  $("#egress-m-quota").textContent = snapshot ? String(snapshot.observed || 0) : "—";

  const needle = search.trim().toLowerCase();
  const matches = (row) =>
    !needle ||
    [row.name, row.address, row.note].some((value) =>
      String(value || "").toLowerCase().includes(needle),
    );
  const visibleRows = rows.filter(
    (row) =>
      matches(row) &&
      (filter !== "issues" ||
        ["blocked", "unavailable", "cooling", "checking"].includes(row.state) ||
        (row.quota?.quotaHits || 0) > 0),
  );
  const issues = rows.filter(
    (row) =>
      ["blocked", "unavailable", "cooling", "checking"].includes(row.state) ||
      (row.quota?.quotaHits || 0) > 0,
  ).length;
  $("#egress-sources-meta").textContent = rows.length
    ? t("{0} nguồn · {1} cần chú ý", "egress.sources.meta", rows.length, issues)
    : t("Chưa có nguồn ra.", "egress.sources.empty");

  body.replaceChildren();
  if (!visibleRows.length) {
    body.append(
      el("tr", {}, [
        el("td", { colspan: "9", class: "empty", text: t("Chưa có nguồn ra.", "egress.sources.empty") }),
      ]),
    );
    return;
  }
  for (const row of visibleRows) {
    const address = el("td", {}, [
      el("div", { class: "mono", text: row.address }),
      row.note ? el("div", { class: "metric-note", text: row.note }) : null,
    ].filter(Boolean));
    const reset = row.quota?.resetAt ? localTime(row.quota.resetAt) : duration(row.cooling);
    body.append(
      el("tr", {}, [
        el("td", { text: row.name }),
        address,
        el("td", {}, [stateBadge(row.state)]),
        el("td", { class: "num", text: String(row.load ?? "—") }),
        quotaCell(row.quota),
        el("td", { class: "num", text: String(row.quota?.quotaHits || 0) }),
        lastQuotaCell(row.quota),
        el("td", { text: reset }),
        el("td", { text: duration(row.cooling) }),
      ]),
    );
  }
}

function renderQuota() {
  const body = $("#egress-rows");
  const rows = snapshot?.exits || [];
  body.replaceChildren();
  if (!rows.length) {
    body.append(
      el("tr", {}, [
        el("td", { colspan: "8", text: t("Chưa có dữ liệu.", "egress.empty") }),
      ]),
    );
    $("#egress-summary").textContent = t(
      "Pool chưa phát request nào trong phiên này.",
      "egress.emptySummary",
    );
    return;
  }
  for (const row of rows) {
    body.append(
      el("tr", {}, [
        el("td", { class: "mono", text: row.egress || "—" }),
        el("td", { text: row.family || t("mọi họ", "egress.anyFamily") }),
        el("td", {}, [stateBadge(row.state)]),
        el("td", { class: "num", text: String(row.requests || 0) }),
        el("td", { class: "num", text: String(row.quota_hits || 0) }),
        el("td", {
          class: "mono",
          text: row.last_quota_at ? localTime(row.last_quota_at) : "—",
        }),
        el("td", { text: duration(row.cooling_seconds) }),
        el("td", { text: row.reset_at ? localTime(row.reset_at) : "—" }),
      ]),
    );
  }
  $("#egress-summary").textContent = t(
    "Đang giữ {0} mục · {1} exit sẵn sàng · {2} /64 đang nghỉ · reset lúc {3}",
    "egress.summary",
    snapshot.observed,
    snapshot.active,
    snapshot.cooling,
    localTime(snapshot.reset_at),
  );
}

function render() {
  renderSources();
  renderQuota();
}

async function load() {
  if (loading) return;
  loading = true;
  const button = $("#egress-refresh");
  if (button) button.disabled = true;
  try {
    const value = await api("/api/egress-quota");
    if (!hasSession()) return;
    snapshot = value;
    render();
  } catch (error) {
    const summary = $("#egress-summary");
    if (summary) {
      summary.textContent = t(
        "Không đọc được ngân sách: {0}",
        "egress.failed",
        error.message,
      );
    }
  } finally {
    loading = false;
    if (button) button.disabled = false;
  }
}

async function checkProxies() {
  const button = $("#egress-check");
  if (button) button.disabled = true;
  try {
    const result = await api("/api/proxies/check", {
      method: "POST",
      body: "{}",
      timeoutMs: 60000,
    });
    await refreshMonitor();
    await load();
    toast(t("Đã kiểm tra {0} proxy", "toast.proxiesChecked", result.checked));
  } catch (error) {
    toast(error.message, true);
  } finally {
    if (button) button.disabled = false;
  }
}

export default {
  id: "egress",
  title: "Egress & WARP",
  sub: "WARP nội bộ, proxy usque, peer và ngân sách /64.",
  init() {
    $("#egress-refresh")?.addEventListener("click", load);
    $("#egress-check")?.addEventListener("click", checkProxies);
    $("#egress-filter")?.addEventListener("change", (event) => {
      filter = event.target.value === "issues" ? "issues" : "all";
      renderSources();
    });
    $("#egress-search")?.addEventListener("input", (event) => {
      search = event.target.value || "";
      renderSources();
    });
    store.on("monitor", () => {
      if (visible) renderSources();
    });
    onChange(() => {
      if (visible) render();
    });
  },
  show() {
    visible = true;
    renderSources();
    load();
    clearInterval(timer);
    timer = setInterval(load, 10000);
  },
  hide() {
    visible = false;
    clearInterval(timer);
    timer = null;
  },
  reset() {
    visible = false;
    snapshot = null;
    filter = "all";
    search = "";
    clearInterval(timer);
    timer = null;
  },
  refresh: load,
};

// Page: egress and relay status. One table now carries both live sources and
// their free-tier /64 budgets: a parent row is the exit, proxy or peer, and its
// children are the model families counted against that /64. Prefixes that have
// left the pool stay visible as "outside pool" rows until the UTC reset.
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
let insights = null;
const expanded = new Set();

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

function renderHeatmap() {
  const wrap = $("#egress-heatmap");
  if (!wrap) return;
  wrap.replaceChildren();
  for (const day of insights?.usage?.days || []) {
    const rate = Math.max(day.quota_hit_rate || 0, day.forbidden_rate || 0);
    const level = !day.requests ? 0 : rate >= 0.5 ? 4 : rate >= 0.25 ? 3 : rate >= 0.1 ? 2 : 1;
    wrap.append(
      el("span", {
        class: `heat-cell h${level}`,
        title: `${day.date}: ${day.requests} request · ${day.quota_hits} 429 · ${day.forbidden} 403`,
      }),
    );
  }
}

function renderCanary() {
  const node = $("#egress-canary");
  if (!node) return;
  const usage = insights?.usage;
  if (!usage) {
    node.textContent = "";
    return;
  }
  const pct = (value) => `${Math.round((value || 0) * 100)}%`;
  const verdict =
    {
      normal: t("bình thường", "egress.canary.normal"),
      quota: t("nhiều 429", "egress.canary.quota"),
      shift: t("403 tăng bất thường", "egress.canary.shift"),
      blocked: t("có thể bị chặn", "egress.canary.blocked"),
    }[usage.verdict] || usage.verdict;
  node.textContent = t(
    "Canary 30 ngày: 403 {0} · 429 {1} · hôm nay: {2}",
    "egress.canary",
    pct(usage.forbidden_rate),
    pct(usage.quota_hit_rate),
    verdict,
  );
}

async function runAction(row, action) {
  if (
    action === "retire" &&
    !confirm(t("Loại {0} khỏi pool tới 00:00 UTC?", "egress.action.confirm", row.prefix))
  ) {
    return;
  }
  try {
    const result = await api("/api/egress/action", {
      method: "POST",
      body: JSON.stringify({ prefix: row.prefix, action }),
    });
    toast(
      t("Đã {0} {1} exit", "egress.action.done", action === "rotate" ? "xoay" : "loại", result.matched),
    );
    await refreshMonitor();
    await load();
  } catch (error) {
    toast(error.message, true);
  }
}

function actionCell(row) {
  const cell = el("td", { class: "actions-cell" });
  if (!row.prefix) return cell;
  const rotate = el("button", {
    class: "btn btn-sm",
    type: "button",
    text: t("Xoay", "egress.action.rotate"),
    title: t("Đổi sang /64 mới ngay", "egress.action.rotateTitle"),
  });
  rotate.addEventListener("click", () => runAction(row, "rotate"));
  const retire = el("button", {
    class: "btn btn-sm",
    type: "button",
    text: t("Loại", "egress.action.retire"),
    title: t("Giữ ngoài pool tới 00:00 UTC", "egress.action.retireTitle"),
  });
  retire.addEventListener("click", () => runAction(row, "retire"));
  cell.append(rotate, retire);
  return cell;
}

// The upstream counts an IPv6 egress by its /64, so the budget board keys rows
// on the first four hextets while the pool reports full addresses.
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
      index.get(key) || { requests: 0, quotaHits: 0, lastQuotaAt: 0, resetAt: "", families: [] };
    const at = Date.parse(row.last_quota_at || "");
    const lastQuotaAt = Number.isFinite(at) ? at : 0;
    entry.requests += row.requests || 0;
    entry.quotaHits += row.quota_hits || 0;
    entry.resetAt = row.reset_at || entry.resetAt;
    if (lastQuotaAt > entry.lastQuotaAt) entry.lastQuotaAt = lastQuotaAt;
    entry.families.push({
      family: row.family || t("mọi họ", "egress.anyFamily"),
      requests: row.requests || 0,
      quotaHits: row.quota_hits || 0,
      lastQuotaAt,
      state: row.state || "idle",
      cooling: row.cooling_seconds || 0,
      resetAt: row.reset_at || "",
    });
    index.set(key, entry);
  }
  for (const entry of index.values()) {
    entry.families.sort((a, b) => b.requests - a.requests || b.quotaHits - a.quotaHits);
  }
  return index;
}

function sourceRow(kind, name, address, note, state, load, cooling, prefix, quota) {
  return { kind, name, address, note, state, load, cooling, prefix, quota };
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
            "",
            undefined,
          ),
        ]
      : [];
  }
  if (!exits.length && egress.length) {
    // Older gateways exposed only the egress IP list.
    return warp.egress.map((address, index) =>
      sourceRow(
        "warp",
        `${t("WARP", "egress.sources.warp")} · ${index + 1}`,
        address || "—",
        "",
        warp.active > 0 ? "active" : "standby",
        "—",
        0,
        prefix64(address),
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
      prefix64(address),
      quota?.get(prefix64(address)),
    );
  });
}

// Prefixes that still hold today's counters but no longer appear in the live
// pool: a retired exit, or one whose tunnel is cooling outside the pool list.
function orphanRows(quota, used) {
  const rows = [];
  for (const [prefix, entry] of quota) {
    if (used.has(prefix)) continue;
    if (!entry.requests && !entry.quotaHits) continue;
    const cooling = entry.families.reduce((max, family) => Math.max(max, family.cooling), 0);
    rows.push(
      sourceRow(
        "orphan",
        t("Ngoài pool", "egress.sources.orphan"),
        `${prefix}::/64`,
        t("không còn trong pool, giữ tới 00:00 UTC", "egress.sources.orphanNote"),
        cooling > 0 ? "cooling" : "unavailable",
        "—",
        cooling,
        prefix,
        entry,
      ),
    );
  }
  return rows;
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
      "",
      undefined,
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
      "",
      undefined,
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

function lastQuotaCell(entry) {
  return el("td", {
    class: "mono",
    text: entry?.lastQuotaAt ? localTime(new Date(entry.lastQuotaAt).toISOString()) : "—",
  });
}

function familyRow(row, family) {
  const key = `${row.prefix}|${family.family}`;
  return el("tr", { class: "sub-row", "data-family": key }, [
    el("td", { text: "" }),
    el("td", { class: "mono", text: family.family }),
    el("td", {}, [stateBadge(family.state)]),
    el("td", { class: "num", text: "—" }),
    el("td", { class: "num mono", text: String(family.requests || 0) }),
    el("td", { class: "num", text: String(family.quotaHits || 0) }),
    lastQuotaCell(family),
    el("td", { class: "mono", text: family.resetAt ? localTime(family.resetAt) : "—" }),
    el("td", { text: duration(family.cooling) }),
    el("td", { text: "" }),
  ]);
}

function parentRow(row) {
  const families = row.quota?.families || [];
  const canExpand = families.length > 1;
  const isOpen = expanded.has(row.prefix);
  const nameCell = el("td", {}, [
    canExpand
      ? el("button", {
          class: "row-toggle",
          type: "button",
          "aria-expanded": String(isOpen),
          title: t("Xem từng họ model", "egress.sources.expand"),
          text: isOpen ? "▾" : "▸",
          onclick: () => {
            if (isOpen) expanded.delete(row.prefix);
            else expanded.add(row.prefix);
            renderSources();
          },
        })
      : null,
    el("span", { text: row.name }),
    row.note ? el("div", { class: "metric-note", text: row.note }) : null,
  ].filter(Boolean));
  const addressCell = el("td", {}, [
    el("div", { class: "mono", text: row.address }),
    row.prefix && row.prefix !== row.address
      ? el("div", { class: "metric-note", text: `/64 · ${row.prefix}::` })
      : null,
  ].filter(Boolean));
  const reset = row.quota?.resetAt ? localTime(row.quota.resetAt) : "—";
  return el("tr", { class: row.kind === "orphan" ? "muted-row" : "" }, [
    nameCell,
    addressCell,
    el("td", {}, [stateBadge(row.state)]),
    el("td", { class: "num", text: String(row.load ?? "—") }),
    el("td", {
      class: "num mono",
      text: row.quota ? String(row.quota.requests || 0) : "—",
      title:
        row.quota && row.quota.rate_per_min
          ? t(
              "~{0}/phút · dự phóng {1} request đến 00:00 UTC",
              "egress.sources.rate",
              row.quota.rate_per_min.toFixed(1),
              row.quota.projected || 0,
            )
          : null,
    }),
    el("td", { class: "num", text: row.quota ? String(row.quota.quotaHits || 0) : "—" }),
    lastQuotaCell(row.quota),
    el("td", { class: "mono", text: reset }),
    el("td", { text: duration(row.cooling) }),
    actionCell(row),
  ]);
}

function renderSources() {
  const body = $("#egress-sources");
  if (!body) return;
  const resources = store.get("monitor")?.resources || {};
  const warp = resources.warp;
  const proxies = resources.proxies || [];
  const peers = resources.peers || [];
  const quota = quotaIndex();
  const warpList = warpRows(warp, quota);
  const used = new Set(warpList.map((row) => row.prefix).filter(Boolean));
  const rows = [
    ...warpList,
    ...proxyRows(proxies),
    ...peerRows(peers),
    ...orphanRows(quota, used),
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
    [row.name, row.address, row.note, row.prefix].some((value) =>
      String(value || "").toLowerCase().includes(needle),
    );
  const isIssue = (row) =>
    ["blocked", "unavailable", "cooling", "checking"].includes(row.state) ||
    (row.quota?.quotaHits || 0) > 0;
  const visibleRows = rows.filter(
    (row) => matches(row) && (filter !== "issues" || isIssue(row)),
  );
  const issues = rows.filter(isIssue).length;
  $("#egress-sources-meta").textContent = rows.length
    ? t("{0} nguồn · {1} cần chú ý", "egress.sources.meta", rows.length, issues)
    : t("Chưa có nguồn ra.", "egress.sources.empty");

  body.replaceChildren();
  if (!visibleRows.length) {
    body.append(
      el("tr", {}, [
        el("td", { colspan: "10", class: "empty", text: t("Chưa có nguồn ra.", "egress.sources.empty") }),
      ]),
    );
    return;
  }
  for (const row of visibleRows) {
    body.append(parentRow(row));
    if (row.prefix && expanded.has(row.prefix)) {
      for (const family of row.quota?.families || []) body.append(familyRow(row, family));
    }
  }
  $("#egress-summary").textContent = snapshot
    ? t(
        "Đang giữ {0} mục · {1} exit sẵn sàng · {2} /64 đang nghỉ · reset lúc {3}",
        "egress.summary",
        snapshot.observed,
        snapshot.active,
        snapshot.cooling,
        localTime(snapshot.reset_at),
      )
    : t("Pool chưa phát request nào trong phiên này.", "egress.emptySummary");
  renderHeatmap();
  renderCanary();
}

async function load() {
  if (loading) return;
  loading = true;
  const button = $("#egress-refresh");
  if (button) button.disabled = true;
  try {
    const value = await api("/api/egress/insights?days=30");
    if (!hasSession()) return;
    insights = value;
    snapshot = value.quota;
    renderSources();
  } catch (error) {
    const summary = $("#egress-summary");
    if (summary) {
      summary.textContent = t("Không đọc được ngân sách: {0}", "egress.failed", error.message);
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
      if (visible) renderSources();
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
    insights = null;
    filter = "all";
    search = "";
    expanded.clear();
    clearInterval(timer);
    timer = null;
  },
  refresh: load,
};

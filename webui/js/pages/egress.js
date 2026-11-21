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
let issuesOnly = false;
let showOrphans = false;
let snapshot = null;
let insights = null;
let surfsharkState = null;
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
    case "running":
      return t("Đang chạy · không ra được", "egress.state.running");
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
      : state === "cooling" || state === "checking" || state === "running"
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
  // An orphan /64 has no exit in the pool any more: Rotate/Retire address a
  // live exit and would match nothing, so show why instead of a dead button.
  if (row.kind === "orphan") {
    cell.append(
      el("span", {
        class: "metric-note",
        text: t("đã ra khỏi pool", "egress.sources.orphanNote"),
      }),
    );
    return cell;
  }
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
  const managed = new Map((surfsharkState?.ports || []).map((port, index) => [port, index + 1]));
  return (proxies || []).map((proxy) => {
    const state = proxy.blocked_until
      ? "blocked"
      : proxy.checking
        ? "checking"
        : proxy.healthy
          ? "active"
          : proxy.listening
            ? "running"
            : "unavailable";
    const binding = [`Z ${proxy.zen_keys || 0}`, `G ${proxy.go_keys || 0}`];
    if (proxy.pool) binding.push(t("pool xoay", "egress.sources.pool"));
    if (proxy.last_checked) {
      binding.push(t("kiểm tra {0}", "egress.sources.checked", localTime(proxy.last_checked)));
    }
    if (!proxy.healthy && proxy.last_error) {
      const reason = String(proxy.last_error);
      binding.push(reason.length > 70 ? `${reason.slice(0, 70)}…` : reason);
    }
    const port = String(proxy.address || "").match(/:(\d+)$/)?.[1];
    const managedIndex = port ? managed.get(Number(port)) : undefined;
    const label = managedIndex
      ? `${t("Surfshark", "egress.sources.surfshark")} · ${managedIndex}`
      : proxy.pool
        ? `${t("Pool", "egress.sources.poolName")} #${(proxy.index ?? 0) + 1}`
        : `${t("Proxy", "egress.sources.proxy")} #${(proxy.index ?? 0) + 1}`;
    return sourceRow(
      "proxy",
      label,
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
    el("td", {}, [
      stateBadge(row.state),
      (row.quota?.quotaHits || 0) > 0
        ? el("span", {
            class: "status warn",
            text: "429",
            title: t("{0} lần 429 trong ngày", "egress.sources.hits", row.quota.quotaHits),
          })
        : null,
    ].filter(Boolean)),
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
    ...(showOrphans ? orphanRows(quota, used) : []),
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
  // The static notes were a legend ("ready / standby / cooling") that read like
  // a second value. Replace them with the actual split for this moment.
  const warpNote = $("#egress-m-warp-note");
  if (warpNote) {
    warpNote.textContent = warp?.enabled
      ? t(
          "dự phòng {0} · nghỉ {1} dải /64{2}",
          "egress.metrics.warp.detail",
          warp.standby || 0,
          warp.cooling || 0,
          warp.lane_gated ? t(" · lane tạm dừng", "egress.metrics.warp.paused") : "",
        )
      : t("Sẵn sàng / dự phòng / đang nghỉ", "egress.metrics.warp.note");
  }
  const proxyHealthy = proxies.filter((proxy) => proxy.healthy).length;
  const proxyNote = $("#egress-m-proxies-note");
  if (proxyNote) {
    proxyNote.textContent = proxies.length
      ? t("{0} khoẻ · {1} lỗi", "egress.metrics.proxies.detail", proxyHealthy, proxies.length - proxyHealthy)
      : t("Đang dùng được / tổng cấu hình", "egress.metrics.proxies.note");
  }
  const bar = $("#egress-proxy-bar");
  const barValue = $("#egress-proxy-bar-value");
  const barFill = $("#egress-proxy-bar-fill");
  if (bar && barValue && barFill) {
    const ratio = proxies.length ? proxyHealthy / proxies.length : 0;
    barValue.textContent = proxies.length ? `${proxyHealthy}/${proxies.length}` : "—";
    barFill.style.width = `${Math.round(ratio * 100)}%`;
    const tone = !proxies.length ? "info" : ratio < 0.2 ? "bad" : ratio < 0.5 ? "warn" : "ok";
    bar.className = `status-bar tone-${tone}`;
  }

  const isIssue = (row) =>
    ["blocked", "unavailable", "cooling", "checking"].includes(row.state) ||
    (row.quota?.quotaHits || 0) > 0;
  const visibleRows = issuesOnly ? rows.filter(isIssue) : rows;
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
  const resetAt = parseStamp(snapshot?.reset_at);
  const resetMinutes = resetAt ? Math.max(0, Math.ceil((resetAt.getTime() - Date.now()) / 60000)) : 0;
  $("#egress-summary").textContent = snapshot
    ? t(
        "{0} mục · {1} sẵn sàng · {2} /64 nghỉ · reset 00:00 UTC (còn ~{3} phút)",
        "egress.summaryCompact",
        snapshot.observed,
        snapshot.active,
        snapshot.cooling,
        resetMinutes,
      )
    : t("Pool chưa phát request nào trong phiên này.", "egress.emptySummary");
  renderLaneBanner();
  renderHeatmap();
  renderCanary();
}

// parseStamp ignores Go's zero time, which serializes as 0001-01-01.
function parseStamp(value) {
  if (!value) return null;
  const date = new Date(value);
  return Number.isNaN(date.getTime()) || date.getFullYear() < 1970 ? null : date;
}

// renderLaneBanner surfaces the free-lane pause: the pool can look healthy
// while it is really waiting out an upstream refusal, and the reset is the
// one action that shortens that wait.
function renderLaneBanner() {
  const node = $("#egress-lane");
  if (!node) return;
  const gated = !!snapshot?.lane_gated;
  const refusals = Number(snapshot?.lane_refusals || 0);
  if (!gated && !refusals) {
    node.classList.add("hidden");
    return;
  }
  node.classList.remove("hidden");
  node.classList.toggle("danger", gated);
  const until = parseStamp(snapshot?.lane_until);
  const minutes = until ? Math.max(0, Math.round((until.getTime() - Date.now()) / 60000)) : 0;
  node.replaceChildren(
    el("span", {
      text: gated
        ? t(
            "Lane đang tạm dừng đăng ký: {0} lần từ chối liên tiếp, thử lại sau ~{1} phút.",
            "egress.lane.gated",
            refusals,
            minutes,
          )
        : t("Lane đã từ chối {0} lần liên tiếp; pool vẫn chạy bằng các exit còn lại.", "egress.lane.refusals", refusals),
    }),
    gated
      ? el("button", {
          class: "btn btn-sm",
          type: "button",
          text: t("Mở khoá ngay", "egress.lane.reset"),
          onclick: resetLane,
        })
      : null,
  );
}

async function resetLane() {
  try {
    await api("/api/egress/lane-reset", { method: "POST", body: "{}" });
    toast(t("Đã mở khoá lane, pool sẽ thử đăng ký lại.", "egress.lane.resetDone"));
    await refreshMonitor();
    await load();
  } catch (error) {
    toast(error.message, true);
  }
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

const COUNTRIES = [
  ["US", "Hoa Kỳ"], ["GB", "Anh"], ["CA", "Canada"], ["AU", "Úc"], ["NZ", "New Zealand"],
  ["DE", "Đức"], ["FR", "Pháp"], ["NL", "Hà Lan"], ["IT", "Ý"], ["ES", "Tây Ban Nha"],
  ["CH", "Thuỵ Sĩ"], ["AT", "Áo"], ["BE", "Bỉ"], ["DK", "Đan Mạch"], ["FI", "Phần Lan"],
  ["NO", "Na Uy"], ["SE", "Thuỵ Điển"], ["PL", "Ba Lan"], ["CZ", "Cộng hoà Séc"], ["PT", "Bồ Đào Nha"],
  ["SG", "Singapore"], ["JP", "Nhật Bản"], ["HK", "Hồng Kông"], ["KR", "Hàn Quốc"], ["IN", "Ấn Độ"],
  ["TW", "Đài Loan"], ["TH", "Thái Lan"], ["MY", "Malaysia"], ["ID", "Indonesia"], ["VN", "Việt Nam"],
  ["BR", "Brazil"], ["AR", "Argentina"], ["MX", "Mexico"], ["ZA", "Nam Phi"], ["TR", "Thổ Nhĩ Kỳ"],
  ["AL", "Albania"], ["IE", "Ireland"], ["IS", "Iceland"], ["RO", "Romania"], ["HU", "Hungary"],
];
const selectedCountries = new Set();

function renderCountryMenu() {
  const menu = $("#ss-country-menu");
  if (!menu) return;
  menu.replaceChildren();
  for (const [code, name] of COUNTRIES) {
    const label = el("label", { class: "country-option" }, [
      el("input", {
        type: "checkbox",
        value: code,
        checked: selectedCountries.has(code),
        onchange: (event) => {
          if (event.target.checked) selectedCountries.add(code);
          else selectedCountries.delete(code);
          updateCountrySummary();
        },
      }),
      el("span", { class: "country-code", text: code }),
      el("span", { text: name }),
    ]);
    menu.append(label);
  }
}

function updateCountrySummary() {
  const summary = $("#ss-country-summary");
  const trigger = $("#ss-country-trigger");
  if (!summary) return;
  if (!selectedCountries.size) summary.textContent = t("Mọi quốc gia", "surfshark.anyCountry");
  else if (selectedCountries.size <= 3) summary.textContent = [...selectedCountries].join(", ");
  else summary.textContent = t("{0} quốc gia", "surfshark.countryCount", selectedCountries.size);
  trigger?.setAttribute("aria-expanded", "false");
}

function toggleCountryMenu(force) {
  const menu = $("#ss-country-menu");
  const trigger = $("#ss-country-trigger");
  if (!menu || !trigger) return;
  const open = force ?? menu.classList.contains("hidden");
  menu.classList.toggle("hidden", !open);
  trigger.setAttribute("aria-expanded", String(open));
}

function bindCountryMenu() {
  $("#ss-country-trigger")?.addEventListener("click", () => toggleCountryMenu());
  document.addEventListener("click", (event) => {
    const host = $("#ss-country-select");
    if (host && !host.contains(event.target)) toggleCountryMenu(false);
  });
  renderCountryMenu();
  updateCountrySummary();
}

// Surfshark provisioning: the form sends credentials to the admin API once,
// then the gateway owns the tunnels and registers them as local SOCKS proxies.
function renderSurfsharkState(state) {
  const node = $("#surfshark-state");
  const value = $("#surfshark-state-value");
  if (!node || !value) return;
  const running = !!state?.running;
  const unavailable = state?.available === false;
  const tone = unavailable ? "info" : running ? "ok" : state?.last_error ? "bad" : "info";
  const icon = unavailable ? "–" : running ? "✓" : state?.last_error ? "!" : "–";
  node.classList.remove("tone-ok", "tone-warn", "tone-bad", "tone-info");
  node.classList.add("tone-" + tone);
  const iconNode = node.querySelector(".status-chip-icon");
  if (iconNode) iconNode.textContent = icon;
  value.textContent = unavailable
    ? t("Không hỗ trợ", "surfshark.unavailable")
    : running
      ? t("{0} tunnel", "surfshark.running", state.tunnels || 0)
      : state?.last_error
        ? t("Lỗi", "surfshark.error")
        : t("Chưa tạo tunnel tại đây", "surfshark.idle");
}

async function loadSurfsharkStatus() {
  try {
    const state = await api("/api/surfshark/status");
    surfsharkState = state;
    renderSurfsharkState(state);
    if (visible) renderSources();
  } catch {
    renderSurfsharkState(null);
  }
}

async function submitSurfshark(event) {
  event.preventDefault();
  const button = $("#surfshark-setup");
  const result = $("#surfshark-result");
  if (button) button.disabled = true;
  if (result) result.textContent = t("Đang đăng nhập Surfshark và tạo tunnel…", "surfshark.working");
  try {
    const payload = {
      username: ($("#ss-user")?.value || "").trim(),
      password: $("#ss-pass")?.value || "",
      count: Number($("#ss-count")?.value || 0),
      countries: [...selectedCountries],
      private_key: ($("#ss-key")?.value || "").trim(),
      base_port: Number($("#ss-port")?.value || 0),
    };
    const state = await api("/api/surfshark/setup", {
      method: "POST",
      body: JSON.stringify(payload),
      timeoutMs: 120000,
    });
    if ($("#ss-pass")) $("#ss-pass").value = "";
    if ($("#ss-key")) $("#ss-key").value = "";
    surfsharkState = state;
    renderSurfsharkState(state);
    if (result)
      result.textContent = t(
        "Đã tạo {0} tunnel — {1}",
        "surfshark.done",
        state.tunnels || 0,
        (state.ports || []).join(", "),
      );
    toast(t("Đã thiết lập Surfshark xong.", "surfshark.toastDone"));
    await refreshMonitor();
    await load();
  } catch (error) {
    if (result) result.textContent = t("Không thiết lập được: {0}", "surfshark.failed", error.message);
    toast(error.message, true);
  } finally {
    if (button) button.disabled = false;
  }
}

// rotateNow lets the operator change the current WARP /64 on demand, instead
// of waiting for the next 429. It reuses the existing action endpoint with the
// first prefix currently in the live exit list.
async function rotateNow() {
  const button = $("#egress-rotate");
  if (button) button.disabled = true;
  const resources = store.get("monitor")?.resources || {};
  const warp = resources.warp || {};
  // penalizePrefix matches the exact egress_key (/64), so prefer that over the
  // full egress address.
  const prefix =
    (warp.exits || []).map((exit) => exit.egress_key).filter(Boolean)[0] ||
    (warp.egress || []).map(prefix64).filter(Boolean)[0] ||
    "";
  if (!prefix) {
    toast(t("Không có exit WARP để xoay.", "egress.rotateNoExit"), true);
    if (button) button.disabled = false;
    return;
  }
  try {
    const result = await api("/api/egress/action", {
      method: "POST",
      body: JSON.stringify({ prefix, action: "rotate" }),
    });
    toast(t("Đã xoay {0} exit.", "egress.rotateDone", result.matched));
    await refreshMonitor();
    await load();
  } catch (error) {
    toast(error.message, true);
  } finally {
    if (button) button.disabled = false;
  }
}

async function stopSurfshark() {
  const button = $("#surfshark-stop");
  if (button) button.disabled = true;
  try {
    const state = await api("/api/surfshark/stop", { method: "POST", body: "{}", timeoutMs: 60000 });
    surfsharkState = state;
    renderSurfsharkState(state);
    const result = $("#surfshark-result");
    if (result) result.textContent = t("Đã dừng tunnel Surfshark.", "surfshark.stopped");
    await refreshMonitor();
    await load();
  } catch (error) {
    toast(error.message, true);
  } finally {
    if (button) button.disabled = false;
  }
}

export default {
  id: "egress",
  title: "Đường ra & Proxy",
  sub: "WARP, Surfshark, peer và ngân sách /64 cho model miễn phí.",
  init() {
    $("#egress-check")?.addEventListener("click", checkProxies);
    $("#egress-rotate")?.addEventListener("click", rotateNow);
    $("#surfshark-form")?.addEventListener("submit", submitSurfshark);
    bindCountryMenu();
    $("#surfshark-stop")?.addEventListener("click", stopSurfshark);
    $("#egress-issues-only")?.addEventListener("change", (event) => {
      issuesOnly = !!event.target.checked;
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
    loadSurfsharkStatus();
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
    showOrphans = false;
    issuesOnly = false;
    search = "";
    filter = "all";
    expanded.clear();
    clearInterval(timer);
    timer = null;
  },
  refresh: load,
};

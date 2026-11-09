// Page: ngân sách egress — one row per WARP egress /64 and model family, with
// the pool's live cooling state and the next UTC reset of the free-tier window.
import { api, hasSession } from "../api.js";
import { t } from "../i18n.js";
import { $, el, localTime } from "../ui.js";

let timer = null;
let loading = false;
let snapshot = null;

function stateLabel(state) {
  if (state === "cooling") return t("Đang nghỉ", "egress.state.cooling");
  if (state === "active") return t("Sẵn sàng", "egress.state.active");
  return t("Chưa dùng", "egress.state.idle");
}

function stateBadge(state) {
  const cls = state === "cooling" ? "status warn" : state === "active" ? "status ok" : "status";
  return el("span", { class: cls, text: stateLabel(state) });
}

function duration(seconds) {
  if (!seconds || seconds <= 0) return "—";
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.round((seconds % 3600) / 60);
  if (hours <= 0) return `${minutes} ${t("phút", "egress.minutes")}`;
  return `${hours} ${t("giờ", "egress.hours")} ${minutes} ${t("phút", "egress.minutes")}`;
}

function render() {
  const body = $("#egress-rows");
  const rows = snapshot?.exits || [];
  body.replaceChildren();
  if (!rows.length) {
    const cell = el("td", { colspan: "7", text: t("Chưa có dữ liệu.", "egress.empty") });
    body.append(el("tr", {}, [cell]));
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
        el("td", { text: String(row.requests || 0) }),
        el("td", { text: String(row.quota_hits || 0) }),
        el("td", { text: duration(row.cooling_seconds) }),
        el("td", { text: row.reset_at ? localTime(row.reset_at) : "—" }),
      ]),
    );
  }
  $("#egress-summary").textContent = t(
    `Đang giữ ${snapshot.observed} mục · ${snapshot.active} exit sẵn sàng · ${snapshot.cooling} /64 đang nghỉ · reset lúc ${localTime(snapshot.reset_at)}`,
    "egress.summary",
    snapshot.observed,
    snapshot.active,
    snapshot.cooling,
    localTime(snapshot.reset_at),
  );
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
    $("#egress-summary").textContent = t(
      `Không đọc được ngân sách: ${error.message}`,
      "egress.failed",
      error.message,
    );
  } finally {
    loading = false;
    if (button) button.disabled = false;
  }
}

export default {
  id: "egress",
  title: "Ngân sách egress",
  sub: "Mỗi /64 là một xô quota riêng cho từng họ model; xem exit nào còn dùng được.",
  init() {
    $("#egress-refresh")?.addEventListener("click", load);
  },
  show() {
    load();
    clearInterval(timer);
    timer = setInterval(load, 10000);
  },
  hide() {
    clearInterval(timer);
    timer = null;
  },
  reset() {
    snapshot = null;
    clearInterval(timer);
    timer = null;
  },
  refresh: load,
};

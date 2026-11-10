// Page: overview — readiness checklist, metrics, resources and token usage.
import * as store from "../store.js";
import { $, bindTabs, el, emptyRow, formatNumber, timeOnly } from "../ui.js";
import egress from "./egress.js";
import { t, onChange } from "../i18n.js";

function renderChecklist() {
  const config = store.get("config");
  const monitor = store.get("monitor");
  if (!config || !monitor) return;
  const resources = monitor.resources;
  const models = resources.models || {};
  const modelDescription = models.stale
    ? t(
        "Danh mục lấy từ cache {0}, đang chờ nền làm mới.",
        "guide.models.cached",
        models.cache_source || "đĩa",
      )
    : t("Danh mục upstream đã qua lần làm mới đầu tiên và có mô hình để phục vụ.", "guide.models.ready");
  const checks = [
    [
      t("Cấu hình Key cục bộ", "guide.step.serverkey"),
      t("Client dùng Server Key để truy cập Gateway.", "guide.step.serverkey.desc"),
      (config.server_keys || []).length > 0,
    ],
    [
      t("Chọn kênh upstream", "guide.step.upstream"),
      t("Có ít nhất một Zen / Go Key, hoặc đã bật kênh ẩn danh.", "guide.step.upstream.desc"),
      (config.zen_keys || []).length + (config.go_keys || []).length > 0 || config.anonymous,
    ],
    [
      t("Kiểm tra Proxy", "guide.step.proxy"),
      t("Có ít nhất một nút proxy còn khỏe, hoặc WARP đang tải lưu lượng model miễn phí.", "guide.step.proxy.desc"),
      (resources.warp?.active || 0) > 0 ||
        (resources.proxies || []).some((proxy) => proxy.healthy),
    ],
    [
      t("Chờ danh mục mô hình", "guide.step.models"),
      modelDescription,
      models.exposed > 0 && !!models.updated_at,
    ],
  ];
  const card = $("#overview-checklist");
  const ready = checks.every(([, , done]) => done);
  card.classList.toggle("hidden", ready);
  if (ready) return;
  const host = $("#guide-steps");
  host.replaceChildren();
  checks.forEach(([title, description, done]) => {
    host.append(
      el("article", { class: "guide-step" }, [
        el("h3", { text: title }),
        el("p", { text: description }),
        el("div", {
          class: `step-state ${done ? "ok" : "warn"}`,
          text: done ? t("✓ Đã sẵn sàng", "guide.done") : t("○ Cần xử lý", "guide.todo"),
        }),
      ]),
    );
  });
}

function renderResources(resources) {
  const proxies = $("#proxy-table");
  proxies.replaceChildren();
  (resources.proxies || []).forEach((proxy) => {
    proxies.append(
      el("tr", {}, [
        el("td", { text: proxy.address }),
        el("td", {
          class: `status ${proxy.healthy ? "ok" : "bad"}`,
          text: proxy.healthy ? t("Khỏe", "state.healthy") : t("Không dùng được", "state.unavailable"),
        }),
        el("td", {
          text: `Z ${proxy.zen_keys} · G ${proxy.go_keys} · A ${
            proxy.anonymous ? t("Bật", "common.on") : t("Tắt", "common.off")
          }`,
        }),
      ]),
    );
  });
  const warp = resources.warp;
  if (warp?.enabled) {
    // WARP is not a proxy: show it beside them so the egress that carries
    // free-model traffic is visible without pretending it is a bound node.
    // The egress list only contains live tunnels; cooling exits still carry
    // their /64 in exits, so use those keys as a fallback and never leave the
    // IP column blank while WARP has any known route.
    const addresses = [
      ...new Set([
        ...(warp.egress || []),
        ...(warp.exits || []).map((exit) => exit.egress || exit.egress_key).filter(Boolean),
      ]),
    ];
    const first = addresses[0] || "—";
    const more = addresses.length > 1 ? ` (+${addresses.length - 1})` : "";
    const state = warp.active > 0 ? "ok" : addresses.length ? "warn" : "bad";
    proxies.append(
      el("tr", {}, [
        el("td", { text: `WARP · ${warp.stack}` }),
        el("td", {
          class: `status ${state}`,
          text:
            warp.active > 0
              ? t("Khỏe", "state.healthy")
              : addresses.length
                ? t("Đang nghỉ / chờ", "state.cooling")
                : t("Không dùng được", "state.unavailable"),
        }),
        el("td", {
          title: addresses.join("\n"),
          text: `${warp.active}/${warp.size} · ${first}${more}`,
        }),
      ]),
    );
  }
  if (!proxies.children.length) emptyRow(proxies, 3);
  const keys = $("#key-table");
  keys.replaceChildren();
  if (resources.anonymous) {
    keys.append(
      el("tr", {}, [
        el("td", { text: "anonymous" }),
        el("td", { text: "zen" }),
        el("td", { class: "status ok", text: t("Đang bật", "state.enabled") }),
      ]),
    );
  }
  (resources.keys || []).forEach((key) => {
    keys.append(
      el("tr", {}, [
        el("td", { text: key.id }),
        el("td", { text: key.tier }),
        el("td", {
          class: `status ${key.cooldown_until ? "warn" : "ok"}`,
          text: key.cooldown_until ? t("Đang nghỉ (cooldown)", "state.cooling") : t("Sẵn sàng", "state.available"),
        }),
      ]),
    );
  });
  if (!keys.children.length) emptyRow(keys, 3);
}

function renderBreakdowns(metrics) {
  const host = $("#breakdowns");
  host.replaceChildren();
  [
    [t("Endpoint", "breakdown.endpoint"), metrics.endpoints],
    [t("Mã trạng thái", "breakdown.status"), metrics.statuses],
    [t("Mô hình", "breakdown.model"), metrics.models],
    ["Tier", metrics.tiers],
  ].forEach(([title, data]) => {
    const body = el("tbody");
    Object.entries(data || {})
      .sort((a, b) => b[1] - a[1])
      .slice(0, 10)
      .forEach(([key, value]) => {
        body.append(el("tr", {}, [el("td", { text: key }), el("td", { class: "num", text: value })]));
      });
    if (!body.children.length) emptyRow(body, 2, t("Chưa có request", "breakdown.empty"));
    host.append(
      el("div", { class: "breakdown" }, [
        el("h3", { text: title }),
        el("div", { class: "table-wrap" }, [el("table", { class: "table" }, [body])]),
      ]),
    );
  });
}

function renderSummary(monitor) {
  const metrics = monitor.metrics;
  $("#m-total").textContent = formatNumber(metrics.last_hour.total);
  $("#m-rate").textContent = `${((metrics.last_hour.success_rate || 0) * 100).toFixed(1)}%`;
  $("#m-p95").textContent = `${metrics.last_hour.p95_ms || 0} ms`;
  $("#m-active").textContent = `${metrics.active_requests || 0} / ${metrics.active_streams || 0}`;
  renderResources(monitor.resources);
  renderBreakdowns(metrics);
}

function renderTrend(metrics) {
  const trend = $("#usage-trend");
  const series = metrics?.series || [];
  const maximum = Math.max(1, ...series.map((point) => point.total_tokens || 0));
  trend.replaceChildren();
  series.forEach((point) => {
    const bar = el("div", { class: "trend-bar" });
    bar.style.height = `${Math.max(2, Math.round(((point.total_tokens || 0) / maximum) * 100))}%`;
    bar.title = `${timeOnly(point.minute)} · input ${formatNumber(point.input_tokens)} · output ${formatNumber(point.output_tokens)} · total ${formatNumber(point.total_tokens)}`;
    trend.append(bar);
  });
  $("#usage-trend-meta").textContent = series.length
    ? t(
        "Đỉnh {0} token / phút · {1} phút gần nhất",
        "usage.trend.peak",
        formatNumber(maximum),
        series.length,
      )
    : t("Chưa có dữ liệu xu hướng", "usage.trend.empty");
}

function renderUsageModels(usage) {
  const host = $("#usage-models");
  host.replaceChildren();
  Object.entries(usage.models || {})
    .sort((a, b) => (b[1].total_tokens || 0) - (a[1].total_tokens || 0))
    .slice(0, 15)
    .forEach(([model, value]) => {
      host.append(
        el("tr", {}, [
          el("td", { class: "mono", text: model }),
          el("td", { class: "num", text: formatNumber(value.input_tokens) }),
          el("td", { class: "num", text: formatNumber(value.output_tokens) }),
          el("td", { class: "num", text: formatNumber(value.total_tokens) }),
        ]),
      );
    });
  if (!host.children.length) emptyRow(host, 4, t("Chưa có mức dùng token", "usage.empty.models"));
}

function renderTiers(usage) {
  const host = $("#usage-tiers");
  const entries = Object.entries(usage.tiers || {}).sort(
    (a, b) => (b[1].total_tokens || 0) - (a[1].total_tokens || 0),
  );
  const total = Math.max(1, ...entries.map(([, value]) => value.total_tokens || 0));
  host.replaceChildren();
  entries.forEach(([tier, value]) => {
    const fill = el("div", { class: "tier-fill" });
    fill.style.width = `${Math.round(((value.total_tokens || 0) / total) * 100)}%`;
    host.append(
      el("div", { class: "tier-row" }, [
        el("strong", { text: tier }),
        el("div", { class: "tier-track" }, [fill]),
        el("span", { class: "tier-value", text: formatNumber(value.total_tokens) }),
      ]),
    );
  });
  if (!entries.length) host.append(el("div", { class: "empty", text: t("Chưa có mức dùng theo Tier", "usage.empty.tiers") }));
}

function renderUsage(monitor) {
  const usage = monitor.usage?.last_hour || {};
  const tokens = usage.tokens || {};
  $("#u-coverage").textContent = `${((usage.coverage || 0) * 100).toFixed(1)}%`;
  $("#u-total").textContent = formatNumber(tokens.total_tokens);
  $("#u-cached").textContent = formatNumber(tokens.cached_tokens);
  $("#u-reasoning").textContent = formatNumber(tokens.reasoning_tokens);
  renderTrend(monitor.metrics);
  renderUsageModels(usage);
  renderTiers(usage);
}

function render() {
  renderChecklist();
  const monitor = store.get("monitor");
  if (!monitor) return;
  renderSummary(monitor);
  renderUsage(monitor);
}

let activeTab = "summary";

export default {
  id: "overview",
  title: "Tổng quan",
  sub: "Chỉ số vận hành, token, tài nguyên và ngân sách egress.",
  init() {
    bindTabs($("#page-overview"), (id) => {
      activeTab = id;
      if (id === "egress") egress.show();
      else egress.hide();
    });
    store.on("monitor", render);
    store.on("config", renderChecklist);
    onChange(render);
  },
  show() {
    render();
    if (activeTab === "egress") egress.show();
    else egress.hide();
  },
  hide() {
    egress.hide();
  },
};

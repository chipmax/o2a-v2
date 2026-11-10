// Page: diagnostics — request testing, routing, ledgers and the live event stream.
import { api } from "../api.js";
import { loadDebugModels } from "../data.js";
import * as logstream from "../logstream.js";
import * as store from "../store.js";
import { t, dateLocale, onChange } from "../i18n.js";
import access from "./access.js";
import {
  $,
  bindTabs,
  copyText,
  el,
  emptyRow,
  formatNumber,
  renderFacts,
  timeOnly,
  toast,
} from "../ui.js";

const keyTestLabels = () => ({
  usable: t("Key dùng được", "play.key.usable"),
  rejected: t("Key bị từ chối", "play.key.rejected"),
  rate_limited: t("Key được chấp nhận nhưng bị giới hạn tần suất", "play.key.rateLimited"),
  upstream_error: t("Upstream bất thường, không kết luận được về Key", "play.key.upstreamError"),
  transport_error: t("Mạng hoặc proxy bất thường, không kết luận được về Key", "play.key.transportError"),
  unavailable: t("Key hoặc môi trường chạy không dùng được", "play.key.unavailable"),
  request_error: t("Request không thành công", "play.key.requestError"),
});

function playgroundTemplate() {
  const protocol = $("#play-protocol").value;
  const model = $("#play-model").value || "YOUR_MODEL";
  const prompt = t("Giải thích năng lực của mô hình này trong một câu.", "playground.samplePrompt");
  if (protocol === "responses") return { model, input: prompt };
  if (protocol === "anthropic")
    return {
      model,
      max_tokens: 512,
      messages: [{ role: "user", content: prompt }],
    };
  return { model, messages: [{ role: "user", content: prompt }] };
}

function resetTemplate() {
  $("#play-request").value = JSON.stringify(playgroundTemplate(), null, 2);
}

function renderKeys() {
  const select = $("#play-key");
  const selected = select.value;
  select.replaceChildren();
  select.append(el("option", { value: "auto", text: t("Định tuyến tự động (ẩn danh / dự phòng Tier / luân phiên Key)", "playground.key.auto") }));
  const keys = store.get("debug")?.keys || {};
  for (const [tier, label] of [
    ["zen", "Zen Keys"],
    ["go", "Go Keys"],
  ]) {
    const items = keys[tier] || [];
    if (!items.length) continue;
    const group = el("optgroup", { label });
    for (const item of items) {
      const cooling = item.cooldown_until
        ? t(
            " · nghỉ đến {0}",
            "playground.key.cooling",
            new Date(item.cooldown_until).toLocaleTimeString(dateLocale(), { hour12: false }),
          )
        : "";
      group.append(
        el("option", {
          value: `${tier}:${item.id}`,
          text: `${tier.toUpperCase()} · ${item.display} · ${item.id}${cooling}`,
        }),
      );
    }
    select.append(group);
  }
  if (selected && [...select.options].some((option) => option.value === selected)) select.value = selected;
  else select.value = "auto";
}

function renderRelays() {
  const select = $("#play-relay");
  if (!select) return;
  const selected = select.value;
  select.replaceChildren();
  select.append(el("option", { value: "auto", text: t("Mọi relay (định tuyến thường)", "playground.relay.auto") }));
  (store.get("debug")?.relays || []).forEach((name, index) => {
    select.append(el("option", { value: String(index), text: `${index} · ${name}` }));
  });
  if (selected && [...select.options].some((option) => option.value === selected)) select.value = selected;
  else select.value = "auto";
}

function renderModels() {
  const select = $("#play-model");
  const selected = select.value;
  select.replaceChildren();
  (store.get("debug")?.models || []).forEach((item) => {
    select.append(el("option", { value: item.model, text: item.model }));
  });
  if (selected && [...select.options].some((option) => option.value === selected)) select.value = selected;
  if (!$("#play-request").value.trim()) resetTemplate();
  renderKeys();
  renderRelays();
}

function showResult(result) {
  const keyTest = result.key_test ? keyTestLabels()[result.key_test] || result.key_test : "";
  const state = $("#play-state");
  state.textContent = result.ok
    ? t("Request thành công · HTTP {0}", "play.state.ok", result.http_status)
    : t("Request thất bại · HTTP {0}", "play.state.failed", result.http_status);
  state.className = `section-caption ${result.ok ? "ok" : "bad"}`;
  const route = result.route || {};
  const eligibility = route.anonymous_eligibility || {};
  const selected = result.selected_key;
  const routeLabel = route.tier
    ? `${route.tier} / ${route.anonymous ? "anonymous" : route.key_id || "key"}`
    : t("Chưa định tuyến", "play.route.none");
  renderFacts($("#play-facts"), [
    [t("HTTP", "play.httpStatus"), result.http_status, result.ok ? "ok" : "bad"],
    [t("Thời gian", "common.duration"), `${result.duration_ms} ms`],
    ["Request ID", result.request_id || "—"],
    [
      t("Chế độ thử", "play.mode"),
      selected
        ? t("Chỉ định Key {0}", "play.mode.selected", selected.tier.toUpperCase())
        : t("Định tuyến tự động", "play.mode.auto"),
    ],
    [t("Key được thử", "play.testedKey"), selected ? `${selected.display} · ${selected.id}` : "—"],
    [
      t("Kết luận về Key", "play.keyVerdict"),
      keyTest || t("Định tuyến tự động không có kết luận riêng", "play.keyVerdict.none"),
      result.key_test === "usable" ? "ok" : result.key_test ? "bad" : "",
    ],
    [t("Relay được thử", "play.testedRelay"), result.relay_test || "—"],
    [t("Định tuyến thực tế", "play.actualRoute"), routeLabel],
    [t("Giao thức gốc", "play.nativeProtocol"), route.native_protocol || "—"],
    [
      t("Kết luận ẩn danh", "play.anonymousVerdict"),
      t(
        `${eligibility.allowed ? "Cho phép" : "Từ chối"} · ${eligibility.source || "—"}`,
        eligibility.allowed ? "play.eligibility.allowed" : "play.eligibility.denied",
        eligibility.source || "—",
      ),
      eligibility.allowed ? "ok" : "warn",
    ],
  ]);
  $("#play-output").textContent = JSON.stringify(result.response, null, 2);
}

async function run(event) {
  event.preventDefault();
  let request;
  try {
    request = JSON.parse($("#play-request").value);
  } catch (error) {
    toast(t("JSON của request không hợp lệ: {0}", "toast.play.invalidJson", error.message), true);
    return;
  }
  const relayValue = $("#play-relay")?.value || "auto";
  const relayNames = store.get("debug")?.relays || [];
  const relay = relayValue === "auto" ? undefined : { index: Number(relayValue), name: relayNames[Number(relayValue)] };
  const keyValue = $("#play-key").value;
  const key =
    keyValue === "auto"
      ? { mode: "auto" }
      : { mode: "selected", tier: keyValue.split(":")[0], id: keyValue.split(":").slice(1).join(":") };
  $("#play-run").disabled = true;
  $("#play-state").textContent =
    key.mode === "selected"
      ? t("Đang thử với Key được chỉ định…", "play.state.testingKey")
      : t("Đang gửi request qua Gateway…", "play.state.running");
  try {
    store.set(
      "lastPlayground",
      await api("/api/debug/inference", {
        method: "POST",
        body: JSON.stringify({ protocol: $("#play-protocol").value, key, ...(relay ? { relay } : {}), request }),
      }),
    );
  } catch (error) {
    toast(error.message, true);
    $("#play-state").textContent = t("Không gọi được cổng chẩn đoán", "play.state.failedToCall");
  } finally {
    $("#play-run").disabled = false;
  }
}

function paginate(name, items) {
  const state = pagerState[name];
  const totalPages = Math.max(1, Math.ceil(items.length / state.size));
  state.page = Math.min(Math.max(1, state.page), totalPages);
  $(`#${name}-pager-info`).textContent = t(
    "Trang {0} / {1} · {2} mục",
    "diag.pager.info",
    state.page,
    totalPages,
    items.length,
  );
  $(`#${name}-pager-prev`).disabled = state.page <= 1;
  $(`#${name}-pager-next`).disabled = state.page >= totalPages;
  return items.slice((state.page - 1) * state.size, state.page * state.size);
}

function renderRequests() {
  const host = $("#upstream-requests");
  host.replaceChildren();
  const requests = paginate("requests", (store.get("monitor")?.upstream?.requests || []).slice().reverse());
  requests.forEach((request) => {
    const terminal = {
      stream_error: t("Streaming thất bại", "diag.streamError"),
      client_canceled: t("Client đã huỷ", "diag.clientCanceled"),
    };
    const result = terminal[request.outcome]
      ? `HTTP ${request.status} · ${terminal[request.outcome]}`
      : request.status || request.outcome;
    host.append(
      el("tr", {}, [
        el("td", { class: "mono", text: timeOnly(request.time) }),
        el("td", { class: "mono", text: request.request_id }),
        el("td", { text: request.model }),
        el("td", { text: request.tier || "—" }),
        el("td", { text: request.channel }),
        el("td", { text: request.key_id || "—" }),
        el("td", { class: "num", text: request.attempts }),
        el("td", { class: request.success ? "ok" : "bad", text: result }),
        el("td", { class: "num", text: `${request.duration_ms} ms` }),
      ]),
    );
  });
  if (!host.children.length) emptyRow(host, 9, t("Chưa có request nào được định tuyến", "diag.noRequests"));
}

function renderAttempts() {
  const host = $("#upstream-attempts");
  host.replaceChildren();
  const attempts = paginate("attempts", (store.get("monitor")?.upstream?.recent || []).slice().reverse());
  attempts.forEach((attempt) => {
    host.append(
      el("tr", {}, [
        el("td", { class: "mono", text: timeOnly(attempt.time) }),
        el("td", { class: "mono", text: attempt.request_id }),
        el("td", { text: attempt.model }),
        el("td", { text: `${attempt.tier} / ${attempt.channel}` }),
        el("td", { text: attempt.key_id }),
        el("td", { text: attempt.proxy_node }),
        el("td", { class: attempt.success ? "ok" : "bad", text: attempt.status || attempt.outcome }),
        el("td", { class: "num", text: `${attempt.duration_ms} ms` }),
      ]),
    );
  });
  if (!host.children.length) emptyRow(host, 8, t("Chưa có lần thử upstream", "diag.noAttempts"));
}

function renderMetadata() {
  const monitor = store.get("monitor");
  const debug = store.get("debug");
  const metadata = debug?.metadata || monitor?.resources?.metadata || {};
  const stamp = (value) => new Date(value).toLocaleString(dateLocale(), { hour12: false });
  renderFacts($("#metadata-facts"), [
    [
      t("Sẵn sàng", "diag.ready"),
      metadata.ready ? t("Có", "common.yes") : t("Không", "common.no"),
      metadata.ready ? "ok" : "warn",
    ],
    [t("Số mô hình", "diag.modelCount"), formatNumber(metadata.models)],
    [
      t("Thời điểm cập nhật", "diag.updatedAt"),
      metadata.updated_at ? stamp(metadata.updated_at) : t("Chưa cập nhật", "diag.notUpdated"),
    ],
    [t("Đã cũ", "diag.stale"), metadata.stale ? t("Có", "common.yes") : t("Không", "common.no"), metadata.stale ? "warn" : "ok"],
    [t("Lần làm mới kế tiếp", "diag.nextRefresh"), metadata.next_refresh ? stamp(metadata.next_refresh) : "—"],
    [
      t("Lỗi gần nhất", "diag.lastError"),
      metadata.last_error || t("Không có", "common.none"),
      metadata.last_error ? "bad" : "ok",
    ],
  ]);
}

function renderRouteModels() {
  const debug = store.get("debug");
  const host = $("#diagnostic-models");
  if (!host || !debug) return;
  const query = $("#diagnostic-model-search").value.trim().toLowerCase();
  host.replaceChildren();
  (debug.models || [])
    .filter((item) => !query || item.model.toLowerCase().includes(query))
    .forEach((item) => {
      const eligibility = item.anonymous_eligibility || {};
      const plan = [...(item.anonymous ? ["anonymous"] : []), ...(item.key_tiers || [])];
      host.append(
        el("tr", {}, [
          el("td", { class: "mono", text: item.model }),
          el("td", { text: `${item.native_protocol} · ${item.protocol_source}` }),
          el("td", {
            class: item.route_error ? "bad" : "",
            text: item.route_error || (plan.length ? plan.join(" → ") : "—"),
          }),
          el("td", {
            class: eligibility.allowed ? "ok" : "warn",
            text: eligibility.allowed ? t("Cho phép", "common.allowed") : t("Từ chối", "common.denied"),
          }),
          el("td", { text: eligibility.source || "—" }),
          el("td", {
            class: "num",
            text: `${eligibility.input_cost == null ? "?" : eligibility.input_cost} / ${
              eligibility.output_cost == null ? "?" : eligibility.output_cost
            }`,
          }),
        ]),
      );
    });
  if (!host.children.length) emptyRow(host, 6, t("Không có mô hình phù hợp", "diag.noMatch"));
}

const pagerState = {
  requests: { page: 1, size: 100 },
  attempts: { page: 1, size: 100 },
};

let activeTab = "playground";

function renderLedger() {
  renderRequests();
  renderAttempts();
}

function renderRoute() {
  renderMetadata();
  renderRouteModels();
}

export default {
  id: "diagnostics",
  title: "Chẩn đoán",
  sub: "Thử request, kiểm tra định tuyến và nhật ký vận hành.",
  init() {
    bindTabs($("#page-diagnostics"), (id) => {
      activeTab = id;
      logstream.setVisible(id === "logs");
      if (id === "playground") {
        if (!store.get("debug")) loadDebugModels();
        renderModels();
      }
      if (id === "ledger") renderLedger();
      if (id === "route") renderRoute();
      if (id === "access") access.show();
    });
    logstream.bind();
    $("#play-protocol").addEventListener("change", resetTemplate);
    $("#play-model").addEventListener("change", () => {
      try {
        const request = JSON.parse($("#play-request").value);
        request.model = $("#play-model").value;
        $("#play-request").value = JSON.stringify(request, null, 2);
      } catch {
        resetTemplate();
      }
    });
    $("#play-reset").addEventListener("click", resetTemplate);
    $("#play-form").addEventListener("submit", run);
    $("#play-copy").addEventListener("click", () =>
      copyText($("#play-output").textContent, t("Đã sao chép phản hồi gốc", "toast.play.copied")),
    );
    $("#diagnostic-model-search").addEventListener("input", renderRouteModels);
    for (const name of ["requests", "attempts"]) {
      $(`#${name}-pager-prev`).addEventListener("click", () => {
        pagerState[name].page -= 1;
        renderLedger();
      });
      $(`#${name}-pager-next`).addEventListener("click", () => {
        pagerState[name].page += 1;
        renderLedger();
      });
      $(`#${name}-pager-size`).addEventListener("change", (event) => {
        pagerState[name].size = Number(event.target.value);
        pagerState[name].page = 1;
        renderLedger();
      });
    }
    store.on("debug", () => {
      renderModels();
      renderRoute();
    });
    store.on("monitor", () => {
      renderLedger();
      renderMetadata();
    });
    store.on("lastPlayground", (value) => {
      if (value) showResult(value);
    });
    // The module owns every string it renders, so a language switch rebuilds it.
    onChange(() => {
      renderModels();
      renderLedger();
      renderMetadata();
      renderRoute();
      const last = store.get("lastPlayground");
      if (last) showResult(last);
    });
  },
  show() {
    if (!store.get("debug")) loadDebugModels();
    renderModels();
    renderLedger();
    renderRoute();
    if (activeTab === "logs") logstream.setVisible(true);
    if (activeTab === "access") access.show();
  },
  hide() {
    logstream.setVisible(false);
  },
  refresh() {
    if (activeTab === "logs") logstream.render();
  },
};

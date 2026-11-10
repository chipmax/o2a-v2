// Page: models and quotas — Go quota windows and free model availability.
import { api, hasSession } from "../api.js";
import { loadDebugModels } from "../data.js";
import { t, onChange } from "../i18n.js";
import { $, bindTabs, copyText, el, emptyRow, localTime, toast } from "../ui.js";
import rotation from "./rotation.js";

const windows = () => [
  ["rolling", t("Cửa sổ 5 giờ", "quotas.window.hour")],
  ["weekly", t("Cửa sổ theo tuần", "quotas.window.week")],
  ["monthly", t("Cửa sổ theo tháng", "quotas.window.month")],
];

let data = null;
let timer = null;
let loading = false;
let activeTab = "quota";

const fmtTokens = (value) =>
  value >= 1_000_000
    ? `${(value / 1_000_000).toFixed(2)}M`
    : value >= 1_000
      ? `${Math.round(value / 1_000)}k`
      : String(value ?? "?");

const contextCell = (model) => {
  if (!model.context_window) return el("td", { class: "mono", text: t("Không rõ", "common.unknown") });
  const source =
    model.context_source === "config"
      ? t("đặt trong cấu hình", "quotas.context.config")
      : model.context_source === "learned"
        ? t("học từ lưu lượng", "quotas.context.learned")
        : t("danh mục", "quotas.context.catalog");
  const notes = [source];
  if (model.context_observed)
    notes.push(t("đã dùng {0}", "quotas.context.observed", fmtTokens(model.context_observed)));
  if (model.context_rejected)
    notes.push(t("bị từ chối ở {0}", "quotas.context.rejected", fmtTokens(model.context_rejected)));
  return el("td", {}, [
    el("div", { class: "mono", text: fmtTokens(model.context_window) }),
    el("div", { class: "metric-note", text: notes.join(" · ") }),
  ]);
};

async function probeContext(id, button) {
  button.disabled = true;
  const label = button.textContent;
  button.textContent = t("Đang đo…", "quotas.contextProbing");
  try {
    const result = await api("/api/models/context-probe", {
      method: "POST",
      body: JSON.stringify({ model: id }),
      timeoutMs: 1200000,
    });
    toast(
      t(
        "Đã đo {0} sau {1} lượt: ngữ cảnh {2} → {3}.",
        "quotas.contextProbeDone",
        id,
        String(result.attempts?.length || 0),
        fmtTokens(result.advertised_before || 0),
        fmtTokens(result.advertised_after || 0),
      ),
    );
    await load();
  } catch (error) {
    if (error.code === "quota_exhausted") {
      const seconds = Number(error.retryAfter || 0);
      const hours = Math.floor(seconds / 3600);
      const minutes = Math.round((seconds % 3600) / 60);
      const wait =
        hours > 0
          ? `${hours} ${t("giờ", "egress.hours")} ${minutes} ${t("phút", "egress.minutes")}`
          : `${minutes} ${t("phút", "egress.minutes")}`;
      toast(
        t(
          "Hạn mức miễn phí của /64 này đã hết nên chưa đo được ngữ cảnh. Thử lại sau {0} (hạn mức reset lúc 00:00 UTC).",
          "quotas.contextProbeQuota",
          wait,
        ),
      );
    } else {
      toast(error.message, true);
    }
  } finally {
    button.disabled = false;
    button.textContent = label;
  }
}

async function load() {
  if (loading) return;
  loading = true;
  $("#quota-refresh").disabled = true;
  $("#quota-refresh").textContent = t("Đang đọc…", "quotas.loading");
  try {
    const value = await api("/api/model-quotas", { timeoutMs: 15000 });
    if (!hasSession()) return;
    data = value;
    const select = $("#quota-account");
    const selected = select.value;
    select.replaceChildren();
    for (const account of value.accounts || []) {
      select.append(el("option", { value: account.id, text: `Go Key ${account.display}` }));
    }
    if ([...select.options].some((option) => option.value === selected)) select.value = selected;
    if (!select.options.length)
      select.append(el("option", { value: "", text: t("Chưa cấu hình Go Key", "quotas.noKey") }));
    render();
  } catch (error) {
    $("#quota-status").textContent = t(
      "Truy vấn thất bại: {0}. Nội dung đang hiển thị chỉ là kết quả truy vấn trước.",
      "quotas.queryFailed",
      error.message,
    );
  } finally {
    loading = false;
    $("#quota-refresh").disabled = false;
    $("#quota-refresh").textContent = t("Làm mới hạn mức", "quotas.refresh");
  }
}

function render() {
  if (!data) return;
  const accounts = data.accounts || [];
  const hasGoAccount = accounts.length > 0;
  $("#quota-summary-section")?.classList.toggle("hidden", !hasGoAccount);
  const account = accounts.find((item) => item.id === $("#quota-account").value);
  const stale = account?.status === "stale";
  const cards = $("#quota-cards");
  cards.replaceChildren();
  $("#quota-status").textContent = !account
    ? t("Hãy thêm OpenCode Go Key trong Trung tâm cấu hình trước.", "quotas.addKeyFirst")
    : account.error
      ? stale
        ? t("{0}; bên dưới hiển thị dữ liệu thành công gần nhất, hiện không rõ hạn mức.", "quotas.staleError", account.error)
        : t("{0}; hiện không rõ hạn mức.", "quotas.error", account.error)
      : t("Đã kết nối OpenCode · hạn mức gói dùng chung. Hạn mức của nhiều Key không cộng dồn.", "quotas.connected");
  $("#quota-updated").textContent = account?.fetched_at
    ? t(
        `${stale ? "Lần truy vấn thành công trước" : "Cập nhật hạn mức"} : ${localTime(account.fetched_at)} · cache 60 giây · nguồn: OpenCode Go /v1/usage`,
        stale ? "quotas.lastSuccess" : "quotas.updated",
        localTime(account.fetched_at),
      )
    : "";
  for (const [id, label] of windows()) {
    const item = account?.windows?.[id];
    const progress = el("progress", { max: 100, "aria-label": t("Hạn mức còn lại của {0}", "quotas.window.aria", label) });
    progress.value = item ? item.remaining_percent : 0;
    cards.append(
      el("div", { class: `quota-card${item?.remaining_percent < 20 ? " quota-low" : ""}` }, [
        el("h3", { text: label }),
        el("div", {
          class: "quota-value",
          text: item ? `≈ ${item.remaining_percent}%` : t("Không rõ", "common.unknown"),
        }),
        progress,
        el("div", {
          class: "quota-reset",
          text: item
            ? `${stale ? t("Còn lại (lịch sử)", "quotas.remainingHistory") : t("Còn lại", "quotas.remaining")} · ${
                item.status === "rate-limited" ? t("đã chạm trần · ", "quotas.rateLimited") : ""
              }${t("reset lúc {0}", "quotas.resetsAt", localTime(item.resets_at))}`
            : t("Đang chờ dữ liệu hạn mức từ upstream", "quotas.waiting"),
        }),
      ]),
    );
  }
  const query = $("#quota-model-search").value.trim().toLowerCase();
  const allModels = data.models || [];
  const models = allModels.filter((model) => model.id.toLowerCase().includes(query));
  $("#quota-model-count").textContent = `${models.length} / ${allModels.length}`;
  const note = $("#quota-model-note");
  note.classList.toggle("hidden", hasGoAccount);
  note.textContent = hasGoAccount
    ? ""
    : t(
        "Chưa cấu hình Go Key — danh sách dưới là các mô hình đang phục vụ được qua kênh miễn phí.",
        "quotas.freeOnly",
      );
  const catalog = data.catalog;
  const catalogUpdated =
    catalog?.updated_at && !catalog.updated_at.startsWith("0001-")
      ? localTime(catalog.updated_at)
      : t("Chưa sẵn sàng", "common.notReady");
  $("#quota-catalog-status").textContent = `${catalog?.stale ? t("Danh mục mô hình chưa cập nhật, đang hiện cache · ", "quotas.catalogStale") : ""}${t(
    "Danh mục cập nhật lúc: {0}",
    "quotas.catalogUpdated",
    catalogUpdated,
  )}`;
  const host = $("#quota-models");
  host.replaceChildren();
  for (const model of models) {
    const copy = el("button", { class: "btn btn-sm btn-ghost", text: t("Sao chép tên", "quotas.copyName") });
    copy.addEventListener("click", () => copyText(model.id));
    const probe = el("button", { class: "btn btn-sm btn-ghost", text: t("Đo trần", "quotas.contextProbe") });
    probe.addEventListener("click", () => probeContext(model.id, probe));
    const protocol =
      { chat: "Chat Completions", responses: "Responses", anthropic: "Messages" }[model.protocol] ||
      model.protocol;
    const row = el("tr", {}, [
      el("td", { class: "mono", text: model.id }),
      el("td", {}, [
        el("div", { text: protocol }),
        el("div", {
          class: "metric-note",
          text: model.free
            ? t("Miễn phí · không dùng hạn mức Go", "quotas.freeChannel")
            : `Tier ${model.tier || "—"}`,
        }),
      ]),
      contextCell(model),
    ]);
    if (!hasGoAccount) {
      row.append(
        el("td", {
          class: "num",
          colspan: "3",
          text: t("Không áp dụng — mô hình miễn phí", "quotas.freeNotApplicable"),
        }),
      );
    } else {
      for (const [id] of windows()) {
        const item = account?.windows?.[id];
        row.append(
          el("td", {
            class: "num",
            text: item
              ? `≈ ${item.remaining_percent}%${stale ? t(" (lịch sử)", "quotas.history") : ""}`
              : t("Không rõ", "common.unknown"),
          }),
        );
      }
    }
    row.append(el("td", {}, [el("div", { class: "key-actions" }, [copy, probe])]));
    host.append(row);
  }
  if (!models.length)
    emptyRow(
      host,
      7,
      allModels.length
        ? t("Không có mô hình phù hợp", "diag.noMatch")
        : t("Danh mục mô hình chưa sẵn sàng, vui lòng làm mới sau", "quotas.catalogPending"),
    );
}

const date = (value) =>
  value && !value.startsWith("0001-") ? localTime(value) : t("Chờ dò", "availability.pending");

async function loadAvailability() {
  try {
    const value = await api("/api/models/availability");
    $("#availability-error").textContent = "";
    const host = $("#availability-models");
    host.replaceChildren();
    for (const item of value.models) {
      const restore = el("button", {
        type: "button",
        class: "btn btn-sm btn-ghost",
        text: t("Khôi phục bật", "availability.restore"),
        disabled: !item.disabled,
      });
      const toggle = el("button", {
        type: "button",
        class: "btn btn-sm btn-ghost",
        text: item.manual
          ? t("Chuyển lại tự động", "availability.switchAuto")
          : t("Chuyển sang thủ công", "availability.switchManual"),
      });
      const run = async (path, body) => {
        restore.disabled = true;
        toggle.disabled = true;
        try {
          await api(path, { method: "POST", body: JSON.stringify(body) });
          await loadAvailability();
          await loadDebugModels();
        } catch (error) {
          $("#availability-error").textContent = error.message;
          restore.disabled = !item.disabled;
          toggle.disabled = false;
        }
      };
      restore.addEventListener("click", () => run("/api/models/restore", { model: item.model }));
      toggle.addEventListener("click", () =>
        run("/api/models/manual", { model: item.model, manual: !item.manual }),
      );
      host.append(
        el("tr", {}, [
          el("td", { class: "mono", text: item.model }),
          el("td", {
            class: item.disabled ? "warn" : "ok",
            text: item.disabled ? t("Đang tắt", "state.disabled") : t("Đang bật", "state.enabled"),
          }),
          el("td", {
            text:
              [item.channel, item.reason].filter(Boolean).join(" / ") ||
              t("Chờ dò", "availability.pending"),
          }),
          el("td", {
            text: item.manual
              ? t("Thủ công", "availability.manual")
              : t("Tự động", "availability.auto"),
          }),
          el("td", {
            text: item.auto_effort?.effort
              ? `${item.auto_effort.effort} (${date(item.effort_checked_at)})`
              : t("Chưa xác minh", "availability.unverified"),
          }),
          el("td", { class: "mono", text: date(item.checked_at) }),
          el("td", {
            class: "mono",
            text: item.manual
              ? t("Điều khiển thủ công, không dò tự động", "availability.manualControl")
              : date(item.next_check),
          }),
          el("td", {}, [el("div", { class: "key-actions" }, [restore, toggle])]),
        ]),
      );
    }
    if (!value.models.length)
      $("#availability-error").textContent = t(
        "Danh mục mô hình hiện tại không có mô hình nào đủ điều kiện miễn phí.",
        "availability.noneEligible",
      );
  } catch (error) {
    $("#availability-error").textContent = error.message;
  }
}

async function refreshAvailability() {
  const button = $("#availability-refresh");
  button.disabled = true;
  try {
    await api("/api/models/probe", { method: "POST", body: "{}" });
    toast(t("Đã lên lịch dò lại; kết quả sẽ cập nhật sau khi các mô hình được kiểm tra.", "availability.probeScheduled"));
  } catch (error) {
    toast(error.message, true);
  } finally {
    button.disabled = false;
  }
  await loadAvailability();
  // The sweep re-probes one model at a time; reload once more when the first
  // fresh results have landed.
  setTimeout(loadAvailability, 20000);
}

function startPolling() {
  clearInterval(timer);
  timer = setInterval(() => {
    if (!document.hidden) load();
  }, 60000);
}

function stopPolling() {
  clearInterval(timer);
  timer = null;
}

export default {
  id: "model-quotas",
  title: "Mô hình & hạn mức",
  sub: "Hạn mức gói chia sẻ và trạng thái dò mô hình miễn phí.",
  init() {
    bindTabs($("#page-model-quotas"), (id) => {
      activeTab = id;
      if (id === "quota") {
        load();
        startPolling();
        return;
      }
      stopPolling();
      if (id === "rotation") rotation.show();
      else loadAvailability();
    });
    $("#quota-refresh").addEventListener("click", load);
    $("#quota-account").addEventListener("change", render);
    $("#quota-model-search").addEventListener("input", render);
    $("#availability-refresh").addEventListener("click", refreshAvailability);
    // Only the rows are rebuilt: a refresh button that is mid-request keeps its
    // disabled state and its "Đang tải…" label.
    onChange(() => {
      if (!loading) $("#quota-refresh").textContent = t("Làm mới hạn mức", "quotas.refresh");
      const emptyOpt = $("#quota-account")?.querySelector('option[value=""]');
      if (emptyOpt) emptyOpt.textContent = t("Chưa cấu hình Go Key", "quotas.noKey");
      if (activeTab === "quota") render();
      else if (activeTab === "free") loadAvailability();
    });
  },
  show() {
    if (activeTab === "quota") {
      load();
      startPolling();
    } else if (activeTab === "rotation") {
      rotation.show();
    } else {
      loadAvailability();
    }
  },
  hide() {
    stopPolling();
  },
  refresh() {
    if (activeTab === "quota") load();
    else if (activeTab === "rotation") rotation.show();
    else loadAvailability();
  },
};

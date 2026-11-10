// Page: model rotation — sota/sweet alias groups, ordering, manual switch and attempts.
import { api, hasSession } from "../api.js";
import { t, onChange } from "../i18n.js";
import { $, el, emptyRow, localTime, toast } from "../ui.js";

const reasons = () => ({
  success: t("Thành công", "rotation.reason.success"),
  client_canceled: t("Bên gọi huỷ", "rotation.reason.clientCanceled"),
  quota_or_rate_limit: t("Hết hạn mức / bị giới hạn tần suất", "rotation.reason.quota"),
  transport_error: t("Lỗi kết nối", "rotation.reason.transport"),
  first_output_timeout: t("Timeout output đầu tiên", "rotation.reason.firstOutputTimeout"),
  stream_error: t("Output bị gián đoạn", "rotation.reason.streamError"),
  response_read_error: t("Đọc phản hồi thất bại", "rotation.reason.readError"),
  response_conversion_error: t("Phân tích hoặc chuyển đổi phản hồi thất bại", "rotation.reason.conversionError"),
  request_conversion_error: t("Chuyển đổi request thất bại", "rotation.reason.requestConversion"),
  upstream_error: t("Lỗi upstream", "rotation.reason.upstreamError"),
});

let data = null;
let dirty = false;

// The persisted rotation state serializes nil maps and slices as JSON null;
// normalize them before anything renders against them.
function normalize(value) {
  for (const group of value.groups || []) {
    group.order = group.order || [];
    group.available = group.available || {};
    group.paused = group.paused || {};
    group.switches = group.switches || [];
  }
  value.attempts = value.attempts || [];
  return value;
}

async function load() {
  try {
    const value = await api("/api/rotation");
    if (!hasSession()) return;
    data = normalize(value);
    dirty = false;
    render();
  } catch (error) {
    toast(error.message, true);
  }
}

function renderGroup(group) {
  const reasonMap = reasons();
  const aliasInput = el("input", {
    id: `rotation-alias-${group.id}`,
    value: group.alias,
    required: true,
    maxlength: 64,
  });
  aliasInput.addEventListener("input", () => {
    group.alias = aliasInput.value;
    dirty = true;
  });
  const table = el("table", { class: "table" }, [
    el("thead", {}, [
      el(
        "tr",
        {},
        [
          t("Thứ tự", "rotation.col.order"),
          t("Mô hình", "common.model"),
          t("Trạng thái / thời điểm hồi phục", "rotation.col.state"),
          t("Thao tác", "common.action"),
        ].map((text) => el("th", { text })),
      ),
    ]),
  ]);
  const body = el("tbody");
  table.append(body);
  group.order.forEach((model, index) => {
    const row = el("tr", {}, [
      el("td", { class: "num", text: String(index + 1) }),
      el("td", { class: "mono", text: model }),
    ]);
    const until = group.paused[model];
    const paused = until && new Date(until) > new Date();
    row.append(
      el("td", {
        text: !group.available[model]
          ? t("Đã gỡ khỏi danh mục / tạm không dùng được", "rotation.unavailable")
          : paused
            ? t("Tạm dừng đến {0}", "rotation.pausedUntil", localTime(until))
            : model === group.current
              ? t("Đang dùng", "rotation.inUse")
              : t("Sẵn sàng", "state.available"),
      }),
    );
    const actions = el("div", { class: "rotation-actions" });
    for (const [text, delta] of [
      [t("Lên", "rotation.moveUp"), -1],
      [t("Xuống", "rotation.moveDown"), 1],
    ]) {
      const button = el("button", {
        type: "button",
        class: "btn btn-sm btn-ghost",
        text,
        "aria-label": `${model} ${text}`,
        disabled: index + delta < 0 || index + delta >= group.order.length,
      });
      button.addEventListener("click", () => {
        [group.order[index], group.order[index + delta]] = [
          group.order[index + delta],
          group.order[index],
        ];
        data.first_output_seconds = Number($("#rotation-timeout").value);
        dirty = true;
        render();
      });
      actions.append(button);
    }
    const select = el("button", {
      type: "button",
      class: "btn btn-sm btn-ghost",
      text: t("Đặt làm hiện tại", "rotation.setCurrent"),
      "aria-label": t("Dùng {0}", "rotation.useModel", model),
      disabled: !group.available[model],
    });
    select.addEventListener("click", async () => {
      select.disabled = true;
      try {
        const result = normalize(
          await api("/api/rotation/current", {
            method: "POST",
            body: JSON.stringify({ group: group.id, model }),
          }),
        );
        // Keep unsaved alias/order edits while refreshing operational state.
        for (const item of data.groups) {
          const live = result.groups.find((entry) => entry.id === item.id);
          Object.assign(item, { current: live.current, paused: live.paused, switches: live.switches });
        }
        data.persistence_error = result.persistence_error;
        render();
        toast(t("Đã chuyển mô hình hiện tại", "toast.rotation.switched"));
      } catch (error) {
        select.disabled = false;
        toast(error.message, true);
      }
    });
    actions.append(select);
    row.append(el("td", {}, [actions]));
    body.append(row);
  });
  if (!group.order.length) emptyRow(body, 4, t("Đang chờ danh mục mô hình Go", "rotation.waitingCatalog"));
  const recent = el("div", { class: "rotation-switches" }, [
    el("h3", { text: t("Chuyển đổi gần đây", "rotation.recentSwitches") }),
  ]);
  for (const item of (group.switches || []).slice(-5).reverse()) {
    recent.append(
      el("p", {
        text: `${localTime(item.time)} · ${item.from} → ${item.to} · ${
          reasonMap[item.reason] || item.reason
        }`,
      }),
    );
  }
  if (!group.switches?.length)
    recent.append(el("p", { text: t("Chưa có bản ghi chuyển đổi", "rotation.noSwitches") }));
  return el("section", { class: "section rotation-group" }, [
    el("h2", { class: "section-title", text: group.id === "sota" ? "SOTA" : t("sweet", "rotation.sweet") }),
    el("div", { class: "field" }, [
      el("label", { for: `rotation-alias-${group.id}`, text: t("Bí danh gọi", "rotation.aliasLabel") }),
      aliasInput,
    ]),
    el("p", {
      class: "rotation-current",
      text: t(
        "Mô hình hiện tại: {0}",
        "rotation.current",
        group.current || t("đang chờ mô hình khả dụng", "rotation.waitingModel"),
      ),
    }),
    el("div", { class: "table-wrap" }, [table]),
    recent,
  ]);
}

function render() {
  if (!data) return;
  const reasonMap = reasons();
  $("#rotation-status").textContent =
    data.persistence_error ||
    t("Đã cập nhật trạng thái · hai nhóm luân chuyển độc lập, mọi bên gọi dùng chung vị trí hiện tại.", "rotation.status");
  $("#rotation-timeout").value = data.first_output_seconds;
  const host = $("#rotation-groups");
  host.replaceChildren();
  for (const group of data.groups) host.append(renderGroup(group));
  const attempts = $("#rotation-attempts");
  attempts.replaceChildren();
  for (const item of (data.attempts || []).slice().reverse()) {
    attempts.append(
      el("tr", {}, [
        el("td", { class: "mono", text: localTime(item.time) }),
        el("td", { class: "mono", text: item.request }),
        el("td", { text: item.alias }),
        el("td", { class: "mono", text: item.model }),
        el("td", { class: "num", text: String(item.number) }),
        el("td", { text: reasonMap[item.outcome] || item.outcome }),
      ]),
    );
  }
  if (!data.attempts?.length)
    emptyRow(attempts, 6, t("Gọi bằng bí danh nhóm để xem 100 lần thử gần nhất", "rotation.attemptsEmpty"));
}

async function save(event) {
  event.preventDefault();
  if (!data) return;
  const button = $("#rotation-save");
  button.disabled = true;
  try {
    const body = { first_output_seconds: Number($("#rotation-timeout").value) };
    for (const group of data.groups) body[group.id] = { alias: group.alias.trim(), order: group.order };
    data = normalize(await api("/api/rotation", { method: "PUT", body: JSON.stringify(body) }));
    dirty = false;
    render();
    toast(t("Đã lưu cấu hình luân chuyển mô hình và có hiệu lực", "toast.rotation.saved"));
  } catch (error) {
    toast(error.message, true);
  } finally {
    button.disabled = false;
  }
}

export default {
  id: "rotation",
  title: "Luân chuyển mô hình",
  sub: "Mô hình hiện tại dùng chung, chuyển khi lỗi và lịch sử thử.",
  init() {
    $("#rotation-refresh").addEventListener("click", () => {
      if (dirty) {
        toast(t("Có thay đổi chưa lưu, hãy lưu trước khi làm mới", "toast.rotation.dirty"), true);
        return;
      }
      load();
    });
    $("#rotation-timeout").addEventListener("input", () => {
      dirty = true;
    });
    $("#rotation-form").addEventListener("submit", save);
    onChange(() => {
      if (data) {
        data.first_output_seconds = Number($("#rotation-timeout").value);
        render();
      }
    });
  },
  show: load,
};

// Page: API Key management — create, rename, enable/disable and delete caller keys.
import { api, derivedAPIBase, hasSession } from "../api.js";
import { loadConfig } from "../data.js";
import * as store from "../store.js";
import { t, onChange } from "../i18n.js";
import { $, el, emptyRow, formatNumber, localTime, confirmDialog, copyText, promptDialog, openDialog, toast } from "../ui.js";

let keysLoading = false;
let createdKey = "";
let lastKeys = null;

async function load() {
  if (keysLoading) return;
  keysLoading = true;
  $("#api-key-refresh").disabled = true;
  try {
    const data = await api("/api/api-keys");
    if (hasSession()) {
      lastKeys = data.keys;
      render(data.keys);
    }
  } catch (error) {
    $("#api-key-status").textContent = t(
      "Đọc thất bại: {0}",
      "apikeys.loadFailed",
      error.message,
    );
  } finally {
    keysLoading = false;
    $("#api-key-refresh").disabled = false;
  }
}

async function mutate(id, method, body, button) {
  button.disabled = true;
  try {
    const data = await api(`/api/api-keys/${encodeURIComponent(id)}`, {
      method,
      ...(body ? { body: JSON.stringify(body) } : {}),
    });
    if (!hasSession()) return;
    lastKeys = data.keys;
    render(data.keys);
    await loadConfig();
    toast(t("Đã lưu và có hiệu lực ngay", "toast.apikeys.saved"));
  } catch (error) {
    toast(error.message, true);
  } finally {
    button.disabled = false;
  }
}

function policySummary(key) {
  const parts = [];
  const models = key.allow_models || [];
  if (models.length) {
    parts.push(t("{0} mô hình", "apikeys.policy.models", models.length));
  }
  if (key.max_concurrency) {
    parts.push(t("{0} song song", "apikeys.policy.concurrency", key.max_concurrency));
  }
  if (key.daily_requests) {
    parts.push(t("{0} lượt/ngày", "apikeys.policy.daily", key.daily_requests));
  }
  return parts.length ? parts.join(" · ") : t("Không giới hạn", "apikeys.policy.none");
}

// editPolicy opens the per-key limit dialog: allowed models, concurrent
// requests and requests per day. Empty or zero means no limit, which is what
// the gateway treats as unlimited too.
function editPolicy(key, button) {
  const models = el("input", {
    value: (key.allow_models || []).join(", "),
    placeholder: "deepseek-v4.1-flash-free, iceman-free",
  });
  const concurrency = el("input", {
    type: "number",
    min: "0",
    max: "256",
    value: String(key.max_concurrency || 0),
  });
  const daily = el("input", {
    type: "number",
    min: "0",
    max: "1000000",
    value: String(key.daily_requests || 0),
  });
  const body = el("div", { class: "stack" }, [
    el("div", { class: "field" }, [
      el("label", { text: t("Mô hình được phép (ID cách nhau bằng dấu phẩy; trống = tất cả)", "apikeys.policy.modelsLabel") }),
      models,
    ]),
    el("div", { class: "field" }, [
      el("label", { text: t("Số yêu cầu song song (0 = không giới hạn)", "apikeys.policy.concurrencyLabel") }),
      concurrency,
    ]),
    el("div", { class: "field" }, [
      el("label", { text: t("Số yêu cầu mỗi ngày (0 = không giới hạn)", "apikeys.policy.dailyLabel") }),
      daily,
    ]),
    el("p", { class: "section-caption", text: t("Hạn mức chỉ áp dụng cho khoá này và có hiệu lực ngay.", "apikeys.policy.hint") }),
  ]);
  openDialog({
    title: t("Hạn mức sử dụng", "apikeys.policy.title"),
    body,
    actions: [
      { label: t("Huỷ", "common.cancel") },
      {
        label: t("Lưu", "common.save"),
        kind: "btn-primary",
        onClick: (close) => {
          const maxConcurrency = Number(String(concurrency.value).trim() || "0");
          const dailyRequests = Number(String(daily.value).trim() || "0");
          if (
            !Number.isInteger(maxConcurrency) ||
            maxConcurrency < 0 ||
            !Number.isInteger(dailyRequests) ||
            dailyRequests < 0
          ) {
            toast(t("Vui lòng nhập số nguyên từ 0 trở lên", "apikeys.policy.invalid"), true);
            return;
          }
          const allowModels = String(models.value)
            .split(",")
            .map((value) => value.trim())
            .filter(Boolean);
          close();
          mutate(
            key.id,
            "PATCH",
            { allow_models: allowModels, max_concurrency: maxConcurrency, daily_requests: dailyRequests },
            button,
          );
        },
      },
    ],
  });
}

function render(keys) {
  keys = keys || [];
  $("#api-key-count").textContent = t("{0} khóa", "apikeys.count", keys.length);
  $("#api-key-status").textContent = t(
    "{0} khóa đang bật",
    "apikeys.enabledCount",
    keys.filter((key) => key.enabled).length,
  );
  const host = $("#api-key-list");
  host.replaceChildren();
  for (const key of keys) {
    const usage = key.usage || {};
    const summary = (value = {}) =>
      `${formatNumber(value.requests)} ${t("lần", "apikeys.times")} · ${formatNumber(value.total_tokens)} tokens`;
    const rename = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("Đổi tên", "common.rename") });
    rename.addEventListener("click", async () => {
      const name = await promptDialog({
        title: t("Đổi tên", "common.rename"),
        label: t("Tên khóa", "apikeys.namePrompt"),
        value: key.name,
        confirmLabel: t("Lưu", "common.save"),
      });
      if (name !== null) mutate(key.id, "PATCH", { name }, rename);
    });
    const toggle = el("button", {
      type: "button",
      class: "btn btn-sm btn-ghost",
      text: key.enabled ? t("Tắt", "apikeys.disable") : t("Bật", "apikeys.enable"),
    });
    toggle.addEventListener("click", () => mutate(key.id, "PATCH", { enabled: !key.enabled }, toggle));
    const policy = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("Hạn mức", "apikeys.policy.action") });
    policy.addEventListener("click", () => editPolicy(key, policy));
    const remove = el("button", { type: "button", class: "btn btn-sm btn-danger", text: t("Xoá", "common.delete") });
    remove.addEventListener("click", async () => {
      const ok = await confirmDialog({
        title: t("Xoá", "common.delete"),
        message: t(
          "Sau khi xoá “{0}”, các ứng dụng đang dùng khóa này sẽ không truy cập được. Xoá thật chứ?",
          "apikeys.confirmDelete",
          key.name,
        ),
        confirmLabel: t("Xoá", "common.delete"),
        danger: true,
      });
      if (ok) mutate(key.id, "DELETE", null, remove);
    });
    const menu = el("details", { class: "action-menu" }, [
      el("summary", {
        class: "btn btn-sm btn-ghost",
        title: t("Thao tác", "apikeys.menu"),
        "aria-label": t("Thao tác", "apikeys.menu"),
        text: "⋯",
      }),
      el("div", { class: "action-menu-pop" }, [rename, policy, toggle, remove]),
    ]);
    menu.addEventListener("toggle", () => {
      if (!menu.open) return;
      document.querySelectorAll("details.action-menu[open]").forEach((other) => {
        if (other !== menu) other.open = false;
      });
    });
    host.append(
      el("tr", {}, [
        el("td", { text: key.name }),
        el("td", { class: "mono", text: key.display }),
        el("td", {
          class: `status ${key.enabled ? "ok" : "warn"}`,
          text: key.enabled ? t("Đang bật", "state.enabled") : t("Đang tắt", "state.disabled"),
        }),
        el("td", { text: `${summary(usage.last_hour)} / ${summary(usage.lifetime)}` }),
        el("td", { text: policySummary(key) }),
        el("td", { text: key.created_at ? localTime(key.created_at) : t("Đã có trong cấu hình", "apikeys.col.legacy") }),
        el("td", {}, [menu]),
      ]),
    );
  }
  if (!keys.length) emptyRow(host, 7, t("Chưa có API Key", "apikeys.empty"));
}

async function create(event) {
  event.preventDefault();
  const button = $("#api-key-create");
  button.disabled = true;
  try {
    const data = await api("/api/api-keys", {
      method: "POST",
      body: JSON.stringify({ name: $("#api-key-name").value, value: $("#api-key-value").value }),
    });
    if (!hasSession()) return;
    createdKey = data.key;
    const goModels = store.get("debug")?.models?.filter((item) => item.available_go) || [];
    const model =
      goModels.find((item) => item.model === "glm-5.3-flash")?.model ||
      goModels[0]?.model ||
      "YOUR_MODEL";
    $("#api-key-env").textContent = `OPENAI_API_KEY=${data.key}\nOPENAI_BASE_URL=${derivedAPIBase()}/v1\nOPENAI_MODEL=${model}`;
    $("#api-key-created").classList.remove("hidden");
    $("#api-key-form").reset();
    lastKeys = data.keys;
    render(data.keys);
    await loadConfig();
    toast(t("Đã tạo API Key", "toast.apikeys.created"));
  } catch (error) {
    toast(error.message, true);
  } finally {
    button.disabled = false;
  }
}

function resetCreated() {
  createdKey = "";
  lastKeys = null;
  $("#api-key-env").textContent = "";
  $("#api-key-created").classList.add("hidden");
  $("#api-key-value").value = "";
}

export default {
  id: "api-keys",
  title: "Quản lý API Key",
  sub: "Cấp khoá truy cập riêng cho từng ứng dụng.",
  init() {
    $("#api-key-form").addEventListener("submit", create);
    $("#api-key-refresh").addEventListener("click", load);
    $("#api-key-dismiss").addEventListener("click", resetCreated);
    $("#api-key-copy").addEventListener("click", () => copyText(createdKey));
    $("#api-key-copy-env").addEventListener("click", () => copyText($("#api-key-env").textContent));
    // Re-render the rows in place so the key that was just created stays on
    // screen instead of being wiped by a reload.
    onChange(() => {
      if (lastKeys) render(lastKeys);
    });
  },
  show: load,
  refresh: load,
  reset: resetCreated,
};

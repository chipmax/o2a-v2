// Page: Agent config adapters — generate omp/opencode/DSH connection files from
// the live model catalog, preview them, and apply with a backup.
import { api, hasSession } from "../api.js";
import { t, onChange } from "../i18n.js";
import { $, el, toast } from "../ui.js";

let targets = [];
let pending = null;

function defaultBase() {
  const host = location.hostname || "127.0.0.1";
  return `${location.protocol || "http:"}//${host}:8090/v1`;
}

function baseURL() {
  return $("#agent-base").value.trim() || defaultBase();
}

function key() {
  return $("#agent-key").value.trim();
}

function pathOf(row) {
  return row.querySelector("input")?.value.trim() || "";
}

function stateText(target) {
  if (!target.exists) return t("chưa có", "agents.state.missing");
  return target.adapter ? t("do adapter tạo", "agents.state.adapter") : t("file khác", "agents.state.foreign");
}

function render() {
  const body = $("#agent-targets");
  body.replaceChildren();
  if (!targets.length) {
    body.append(
      el("tr", {}, [
        el("td", { colspan: "4", class: "empty", text: t("Không tìm thấy thư mục agent.", "agents.empty") }),
      ]),
    );
    return;
  }
  for (const target of targets) {
    const pathInput = el("input", { type: "text", value: target.path, spellcheck: "false" });
    const preview = el("button", { class: "btn btn-sm", type: "button", text: t("Xem trước", "agents.preview") });
    preview.addEventListener("click", () => openPreview(target, pathInput.value.trim()));
    const restore = el("button", {
      class: "btn btn-sm",
      type: "button",
      text: t("Khôi phục", "agents.restore"),
      disabled: !target.backup,
      title: target.backup || "",
    });
    restore.addEventListener("click", () => restoreTarget(pathInput.value.trim()));
    body.append(
      el("tr", {}, [
        el("td", {}, [el("strong", { text: target.label })]),
        el("td", {}, [pathInput]),
        el("td", { text: stateText(target) }),
        el("td", { class: "actions-cell" }, [preview, restore]),
      ]),
    );
  }
}

async function load() {
  try {
    const data = await api("/api/agents");
    if (!hasSession()) return;
    targets = data.targets || [];
    render();
  } catch (error) {
    toast(error.message, true);
  }
}

async function openPreview(target, path) {
  try {
    const result = await api("/api/agents/preview", {
      method: "POST",
      body: JSON.stringify({ kind: target.kind, path, base_url: baseURL(), key: key() }),
    });
    if (!hasSession()) return;
    pending = { kind: target.kind, path: result.path };
    $("#agent-preview-card").classList.remove("hidden");
    $("#agent-preview-title").textContent = t("Xem trước: {0}", "agents.previewTitle", target.label);
    $("#agent-preview-meta").textContent = t(
      "{0} · {1} ký tự · {2}",
      "agents.previewMeta",
      result.path,
      result.content.length,
      result.adapter ? t("adapter sở hữu", "agents.state.adapter") : t("file khác — cần xác nhận", "agents.state.foreign"),
    );
    $("#agent-preview").textContent = result.content;
    $("#agent-preview-card").scrollIntoView({ behavior: "smooth", block: "start" });
  } catch (error) {
    toast(error.message, true);
  }
}

async function applyPending() {
  if (!pending) return;
  try {
    const result = await api("/api/agents/apply", {
      method: "POST",
      body: JSON.stringify({
        kind: pending.kind,
        path: pending.path,
        base_url: baseURL(),
        key: key(),
        force: $("#agent-force").checked,
      }),
    });
    toast(
      t("Đã ghi {0} ({1} byte), sao lưu: {2}", "agents.applied", result.path, result.bytes, result.backup || "—"),
    );
    $("#agent-preview-card").classList.add("hidden");
    pending = null;
    await load();
  } catch (error) {
    toast(error.message, true);
  }
}

async function restoreTarget(path) {
  if (!path) return;
  try {
    const result = await api("/api/agents/restore", {
      method: "POST",
      body: JSON.stringify({ path }),
    });
    toast(t("Đã khôi phục {0}", "agents.restored", result.path));
    await load();
  } catch (error) {
    toast(error.message, true);
  }
}

function reset() {
  pending = null;
  targets = [];
  $("#agent-preview-card")?.classList.add("hidden");
}

export default {
  id: "agents",
  title: "Agent Adapter",
  sub: "Sinh cấu hình omp / opencode / DSH từ model đang phát.",
  init() {
    if (!$("#agent-base").value) $("#agent-base").value = defaultBase();
    $("#agent-refresh").addEventListener("click", load);
    $("#agent-apply").addEventListener("click", applyPending);
    $("#agent-close").addEventListener("click", () => {
      $("#agent-preview-card").classList.add("hidden");
      pending = null;
    });
    onChange(() => {
      if (targets.length) render();
    });
  },
  show: load,
  refresh: load,
  reset,
};

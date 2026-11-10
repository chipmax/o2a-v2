// Page: configuration center — masked config editing with validation, hot reload and reveal.
import { api } from "../api.js";
import { loadConfig, loadDebugModels } from "../data.js";
import * as store from "../store.js";
import { t, onChange } from "../i18n.js";
import { $, $$, bindTabs, el, openDialog, toast } from "../ui.js";

// A long proxy list is rendered collapsed: the first CHIP_RENDER_LIMIT chips
// plus a toggle. A single flex-wrap row with thousands of chips is what froze
// the config page on large pools.
const CHIP_RENDER_LIMIT = 100;
const expandedChips = new Set();

function renderChips(name) {
  const host = $(`#${name}-chips`);
  host.replaceChildren();
  const items = store.get("config")?.[name] || [];
  const expanded = expandedChips.has(name);
  const visible = expanded ? items : items.slice(0, CHIP_RENDER_LIMIT);
  visible.forEach((item) => {
    const remove = el("button", { type: "button", title: t("Gỡ bỏ", "common.remove"), text: "×" });
    const chip = el("span", { class: "chip", dataset: { id: item.id } });
    remove.addEventListener("click", () => chip.remove());
    chip.append(el("span", { text: item.display }), remove);
    host.append(chip);
  });
  if (items.length > visible.length)
    host.append(chipToggle(`config.chips.expand`, `Mở rộng tất cả (${items.length})`, items.length, () => {
      expandedChips.add(name);
      renderChips(name);
    }));
  else if (expanded)
    host.append(chipToggle(`config.chips.collapse`, "Thu gọn", null, () => {
      expandedChips.delete(name);
      renderChips(name);
    }));
}

function chipToggle(key, fallback, count, onClick) {
  const props = { type: "button", class: "chip chip-toggle", text: t(fallback, key, count) };
  if (count != null) props.dataset = { count };
  const button = el("button", props);
  button.addEventListener("click", onClick);
  return button;
}

function renderRestartNotice(value) {
  const notice = $("#restart-notice");
  if (value?.restart_required_fields?.length) {
    notice.textContent = t(
      "Đã lưu nhưng chưa có hiệu lực: {0}. API hiện tại {1}, WebUI {2}. Vui lòng khởi động lại tiến trình.",
      "config.restartNotice",
      value.restart_required_fields.join(", "),
      value.effective.listen,
      value.effective.webui_listen,
    );
    notice.classList.remove("hidden");
  } else {
    notice.classList.add("hidden");
  }
}

function secretInputs(name) {
  const kept = [...$(`#${name}-chips`).children]
    .filter((node) => node.dataset.id !== undefined)
    .map((node) => ({ id: node.dataset.id }));
  const added = $(`#${name}-new`)
    .value.split(/\r?\n/)
    .map((value) => value.trim())
    .filter(Boolean)
    .map((value) => ({ value }));
  return kept.concat(added);
}

const numberValue = (selector) => Number($(selector).value);

function fillConfig(value) {
  if (!value) return;
  $("#c-listen").value = value.listen;
  $("#c-prefer").value = value.prefer;
  $("#c-web-listen").value = value.webui.listen;
  $("#c-session").value = value.webui.session_ttl_minutes;
  $("#c-web-enabled").checked = value.webui.enabled;
  $("#c-anonymous").checked = !!value.anonymous;
  const warp = value.warp || {};
  $("#c-warp-enabled").checked = !!warp.enabled;
  $("#c-warp-pool").value = warp.pool_size || 2;
  $("#c-warp-stack").value = warp.stack || "ipv6";
  $("#c-warp-source").value = warp.source_ipv6 || "";
  $("#c-proxyfile").value = value.proxyfile;
  $("#c-up-zen").value = value.upstream.zen;
  $("#c-up-go").value = value.upstream.go;
  $("#c-attempts").value = value.retry.max_attempts;
  $("#c-timeout").value = value.retry.timeout_seconds;
  $("#c-refresh").value = value.models.refresh_seconds;
  $("#c-protocols").value = JSON.stringify(value.models.protocols || {}, null, 2);
  $("#c-free-only").checked = !!value.models.free_only;
  $("#c-active-probes").checked = !!value.models.active_probes;
  const reasoning = value.reasoning || {};
  $("#c-reasoning-effort").value = reasoning.effort || "";
  $("#c-reasoning-by-model").value = JSON.stringify(reasoning.effort_by_model || {}, null, 2);
  $("#c-idle").value = value.performance.max_idle_conns;
  $("#c-idle-host").value = value.performance.max_idle_conns_per_host;
  $("#c-max-host").value = value.performance.max_conns_per_host;
  $("#c-idle-timeout").value = value.performance.idle_conn_timeout_seconds;
  $("#c-connect").value = value.performance.connect_timeout_seconds;
  $("#c-cooldown").value = value.performance.failure_cooldown_seconds;
  $("#c-attempt-timeout").value = value.performance.attempt_timeout_seconds;
  $("#c-first-event-timeout").value = value.performance.first_event_timeout_seconds || 0;
  $("#c-level").value = value.logging.level;
  $("#c-ring").value = value.logging.ring_size;
  $("#c-dump-bodies").checked = !!value.logging.dump_request_bodies;
  ["server_keys", "zen_keys", "go_keys", "proxies"].forEach(renderChips);
  renderRestartNotice(value);
}

async function save(event) {
  event.preventDefault();
  const current = store.get("config");
  if (!current) return;
  try {
    const update = {
      listen: $("#c-listen").value,
      server_keys: secretInputs("server_keys"),
      zen_keys: secretInputs("zen_keys"),
      go_keys: secretInputs("go_keys"),
      anonymous: $("#c-anonymous").checked,
      warp: {
        enabled: $("#c-warp-enabled").checked,
        pool_size: numberValue("#c-warp-pool"),
        stack: $("#c-warp-stack").value,
        source_ipv6: $("#c-warp-source").value.trim(),
      },
      proxies: secretInputs("proxies"),
      proxyfile: $("#c-proxyfile").value,
      prefer: $("#c-prefer").value,
      upstream: { zen: $("#c-up-zen").value, go: $("#c-up-go").value },
      retry: { max_attempts: numberValue("#c-attempts"), timeout_seconds: numberValue("#c-timeout") },
      models: {
        refresh_seconds: numberValue("#c-refresh"),
        protocols: JSON.parse($("#c-protocols").value || "{}"),
        free_only: $("#c-free-only").checked,
        active_probes: $("#c-active-probes").checked,
      },
      reasoning: {
        effort: $("#c-reasoning-effort").value,
        effort_by_model: JSON.parse($("#c-reasoning-by-model").value || "{}"),
      },
      performance: {
        max_idle_conns: numberValue("#c-idle"),
        max_idle_conns_per_host: numberValue("#c-idle-host"),
        max_conns_per_host: numberValue("#c-max-host"),
        idle_conn_timeout_seconds: numberValue("#c-idle-timeout"),
        connect_timeout_seconds: numberValue("#c-connect"),
        failure_cooldown_seconds: numberValue("#c-cooldown"),
        attempt_timeout_seconds: numberValue("#c-attempt-timeout"),
        first_event_timeout_seconds: numberValue("#c-first-event-timeout"),
      },
      logging: {
        level: $("#c-level").value,
        ring_size: numberValue("#c-ring"),
        dump_request_bodies: $("#c-dump-bodies").checked,
      },
      webui: {
        enabled: $("#c-web-enabled").checked,
        listen: $("#c-web-listen").value,
        username: current.webui.username,
        session_ttl_minutes: numberValue("#c-session"),
      },
    };
    const result = await api("/api/config", {
      method: "PUT",
      body: JSON.stringify(update),
      timeoutMs: 90000,
    });
    store.set("config", result.config);
    ["server_keys", "zen_keys", "go_keys", "proxies"].forEach(
      (name) => ($(`#${name}-new`).value = ""),
    );
    await loadDebugModels();
    toast(
      result.result.restart_required
        ? t(
            "Đã áp dụng; cần khởi động lại cho {0}",
            "toast.config.restart",
            result.result.restart_fields.join(", "),
          )
        : t("Cấu hình đã lưu và có hiệu lực ngay", "toast.config.saved"),
    );
  } catch (error) {
    toast(error.message, true);
  }
}

async function reload() {
  try {
    const result = await api("/api/config/reload", {
      method: "POST",
      body: "{}",
      timeoutMs: 90000,
    });
    await loadConfig();
    await loadDebugModels();
    toast(
      result.restart_required
        ? t("Đã nạp lại cấu hình từ đĩa, các trường lắng nghe cần khởi động lại", "toast.config.reloadedRestart")
        : t("Đã nạp lại cấu hình từ đĩa", "toast.config.reloaded"),
    );
  } catch (error) {
    toast(error.message, true);
  }
}

function reveal() {
  const input = el("input", { type: "password", autocomplete: "current-password" });
  const submit = async (close) => {
    try {
      const values = await api("/api/config/reveal", {
        method: "POST",
        body: JSON.stringify({ password: input.value }),
      });
      close();
      openDialog({
        title: t("Cấu hình nhạy cảm", "modal.title"),
        size: "dialog-lg",
        body: el("pre", { text: JSON.stringify(values, null, 2) }),
        actions: [{ label: t("Đóng", "modal.close") }],
      });
    } catch (error) {
      toast(error.message, true);
    }
  };
  const dialog = openDialog({
    title: t("Xem cấu hình nhạy cảm", "modal.revealTitle"),
    body: el("div", { class: "field" }, [
      el("label", { text: t("Nhập lại mật khẩu quản trị", "config.revealPrompt") }),
      input,
    ]),
    actions: [
      { label: t("Huỷ", "common.cancel") },
      { label: t("Xem", "common.view"), kind: "btn-primary", onClick: submit },
    ],
  });
  input.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      event.preventDefault();
      submit(dialog.close);
    }
  });
}

export default {
  id: "config",
  title: "Cấu hình",
  sub: "Kiểm tra, lưu và áp dụng nóng các trường có thể thay đổi.",
  init() {
    store.on("config", fillConfig);
    bindTabs($("#page-config"), (id) => {
      $("#config-form").classList.toggle("hidden", id === "account");
    });
    $("#config-form").addEventListener("submit", save);
    $("#reload-config").addEventListener("click", reload);
    $("#reveal").addEventListener("click", reveal);
    onChange(() => {
      const value = store.get("config");
      if (value) renderRestartNotice(value);
      // Update chip tooltips in place so pending unsaved chip removals survive a language switch.
      $$(".chips .chip > button").forEach((button) => (button.title = t("Gỡ bỏ", "common.remove")));
      $$(".chip-toggle").forEach((node) => {
        node.textContent = node.dataset.count
          ? t("Hiện tất cả ({0})", "config.chips.expand", node.dataset.count)
          : t("Thu gọn", "config.chips.collapse");
      });
    });
  },
};

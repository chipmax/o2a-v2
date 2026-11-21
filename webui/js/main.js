// Console shell: boot, login, navigation and shared data loading.
import { api, setCsrf, clearCsrf, setUnauthorizedHandler } from "./api.js";
import { refreshMonitor, loadConfig, loadDebugModels } from "./data.js";
import * as i18n from "./i18n.js";
import * as store from "./store.js";
import { $, $$, el, toast } from "./ui.js";
import * as logstream from "./logstream.js";
import { init as initTheme } from "./theme.js";

import overview from "./pages/overview.js";
import diagnostics from "./pages/diagnostics.js";
import access from "./pages/access.js";
import apiKeys from "./pages/api-keys.js";
import quotas from "./pages/model-quotas.js";
import egress from "./pages/egress.js";
import agents from "./pages/agents.js";
import rotation from "./pages/rotation.js";
import configPage from "./pages/config.js";
import account from "./pages/account.js";

const PAGES = [overview, diagnostics, access, apiKeys, quotas, egress, agents, rotation, configPage, account];
const byId = new Map(PAGES.map((page) => [page.id, page]));

// Hrefs from the previous 13-page layout keep working. A page that absorbed
// several old pages also restores the matching tab instead of its default.
const HASH_ALIASES = {
  guide: "overview",
  usage: "overview",
  playground: "diagnostics",
  logs: "diagnostics",
  access: "diagnostics",
  availability: "model-quotas",
  rotation: "model-quotas",
  account: "config",
};

const HASH_TABS = {
  usage: ["overview", "usage"],
  playground: ["diagnostics", "playground"],
  logs: ["diagnostics", "logs"],
  access: ["diagnostics", "route"],
  availability: ["model-quotas", "quota"],
  rotation: ["model-quotas", "rotation"],
  account: ["config", "account"],
};

let current = null;
let monitorTimer = null;

function renderPageHeader(page) {
  if (!page) return;
  $("#page-title").textContent = i18n.t(page.title, `page.${page.id}.title`);
  $("#page-sub").textContent = i18n.t(page.sub, `page.${page.id}.sub`);
}

function setActiveNav(id, tab) {
  const hasExactTab = !!tab && $$(".nav-item").some(
    (button) => button.dataset.page === id && button.dataset.tab === tab,
  );
  $$(".nav-item").forEach((button) => {
    const pageMatch = button.dataset.page === id;
    const tabMatch = hasExactTab ? button.dataset.tab === tab : !button.dataset.tab;
    button.classList.toggle("active", pageMatch && tabMatch);
  });
}

function openPage(id, tab) {
  const page = byId.get(id);
  if (!page) return;
  current?.hide?.();
  if (id === "overview" && !tab) {
    document.querySelector('#page-overview [data-tab="summary"]')?.click();
  }
  history.replaceState(null, "", `#${id}`);
  setActiveNav(id, tab);
  $$(".page").forEach((node) => node.classList.toggle("hidden", node.id !== `page-${id}`));
  renderPageHeader(tab && byId.has(tab) ? byId.get(tab) : page);
  current = page;
  page.show?.();
  if (tab) document.querySelector(`#page-${id} [data-tab="${tab}"]`)?.click();
}

function showLogin() {
  clearCsrf();
  closeMenu();
  closeCommand();
  current?.hide?.();
  current = null;
  $("#app").classList.add("hidden");
  $("#login").classList.remove("hidden");
  clearInterval(monitorTimer);
  monitorTimer = null;
  logstream.disconnect();
  for (const page of PAGES) page.reset?.();
}

async function enterConsole(session) {
  setCsrf(session.csrf_token);
  $("#login").classList.add("hidden");
  $("#app").classList.remove("hidden");
  await loadConfig();
  await Promise.all([refreshMonitor(), loadDebugModels()]);
  clearInterval(monitorTimer);
  monitorTimer = setInterval(refreshMonitor, 3000);
  logstream.connect();
  const rawHash = location.hash.slice(1).replace(/^\/+/, "");
  const hash = rawHash.split("?")[0]; // deep links may carry ?request=...
  const id = HASH_ALIASES[hash] || hash;
  const tab = HASH_TABS[hash];
  if (tab) openPage(tab[0], tab[1]);
  else openPage(byId.has(id) ? id : "overview");
}

async function boot() {
  try {
    await enterConsole(await api("/api/auth/session"));
  } catch {
    showLogin();
  }
}

setUnauthorizedHandler(showLogin);

function updateShellHealth(data) {
  const ready = !!data?.health?.ready;
  const label = ready
    ? i18n.t("Hoạt động", "shell.health.ready")
    : i18n.t("Suy giảm", "shell.health.degraded");
  const pill = $("#topbar-health");
  if (pill) {
    pill.classList.toggle("is-warn", !ready);
    const text = pill.querySelector("span:last-child");
    if (text) text.textContent = label;
  }
  const side = $("#sidebar-health-label");
  if (side) side.textContent = label;
}

store.on("monitor", (data) => {
  const version = String(data.version || "");
  $("#version").textContent = version.startsWith("v") || version === "dev" ? version : `v${version}`;
  updateShellHealth(data);
});

for (const page of PAGES) page.init?.();
initTheme();

function closeMenu() {
  $("#app").classList.remove("nav-open");
  $("#nav-scrim").hidden = true;
  $("#menu-toggle")?.setAttribute("aria-expanded", "false");
}

function openMenu() {
  $("#app").classList.add("nav-open");
  $("#nav-scrim").hidden = false;
  $("#menu-toggle")?.setAttribute("aria-expanded", "true");
}

$("#menu-toggle")?.addEventListener("click", () => {
  if ($("#app").classList.contains("nav-open")) closeMenu();
  else openMenu();
});
$("#nav-scrim")?.addEventListener("click", closeMenu);

$$(".nav-item").forEach((button) => {
  button.addEventListener("click", () => {
    openPage(button.dataset.page, button.dataset.tab);
    closeMenu();
  });
});

$("#quick-api-keys")?.addEventListener("click", () => openPage("api-keys"));
$("#quick-diagnostics")?.addEventListener("click", () => openPage("diagnostics"));
$("#quick-egress")?.addEventListener("click", () => openPage("egress"));
$("#hero-open-egress")?.addEventListener("click", () => openPage("egress"));
$("#hero-open-logs")?.addEventListener("click", () => openPage("diagnostics", "logs"));

// Keep the sidebar honest when a tab is chosen inside a page rather than from
// the navigation rail (for example Settings → Account).
$$(".page > .tabs .tab").forEach((button) => {
  button.addEventListener("click", () => {
    const page = button.closest(".page")?.id.replace(/^page-/, "");
    if (!page) return;
    const exact = $$(".nav-item").find(
      (item) => item.dataset.page === page && item.dataset.tab === button.dataset.tab,
    );
    if (exact) {
      setActiveNav(page, button.dataset.tab);
      return;
    }
    const base = $$(".nav-item").find((item) => item.dataset.page === page && !item.dataset.tab);
    if (base) setActiveNav(page);
  });
});

$("#refresh-now").addEventListener("click", async () => {
  const [monitor] = await Promise.all([refreshMonitor(), loadConfig(), loadDebugModels()]);
  overview.renderStatusStrip?.(monitor);
  await current?.refresh?.();
  toast(i18n.t("Đã làm mới trạng thái", "toast.refreshed"));
});

$("#logout").addEventListener("click", async () => {
  try {
    await api("/api/auth/logout", { method: "POST", body: "{}" });
  } finally {
    showLogin();
  }
});

$("#login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  $("#login-error").textContent = "";
  try {
    const session = await api("/api/auth/login", {
      method: "POST",
      body: JSON.stringify({ username: $("#login-user").value, password: $("#login-pass").value }),
    });
    $("#login-pass").value = "";
    await enterConsole(session);
  } catch (error) {
    $("#login-error").textContent = error.message;
  }
});

// Command palette: keyboard-first navigation and the few global actions an
// operator needs without hunting through tabs.
const COMMANDS = [
  { label: "Tổng quan", hint: "Sức khỏe, lưu lượng, sự kiện", icon: "i-overview", page: "overview", keywords: "dashboard home tong quan" },
  { label: "Đường ra & Proxy", hint: "WARP, Surfshark, ngân sách /64", icon: "i-egress", page: "egress", keywords: "warp surfshark proxy egress duong ra" },
  { label: "Mô hình & hạn mức", hint: "Danh mục, quota, mô hình miễn phí", icon: "i-models", page: "model-quotas", keywords: "model quota free models" },
  { label: "Luân chuyển mô hình", hint: "Mô hình hiện tại và lịch sử thử", icon: "i-models", page: "model-quotas", tab: "rotation", keywords: "rotation model luan chuyen" },
  { label: "Agent", hint: "Sinh cấu hình cho omp, opencode, DSH", icon: "i-agents", page: "agents", keywords: "agent adapter config" },
  { label: "API Keys", hint: "Tạo, thu hồi và xem key", icon: "i-key", page: "api-keys", keywords: "api key token" },
  { label: "Hướng dẫn kết nối", hint: "cURL, Python, JavaScript", icon: "i-book", page: "diagnostics", tab: "route", keywords: "access guide curl sdk integration" },
  { label: "Chẩn đoán", hint: "Playground, ledger, log, định tuyến", icon: "i-diagnostics", page: "diagnostics", keywords: "diagnostics logs playground route" },
  { label: "Logs", hint: "Luồng sự kiện trực tiếp", icon: "i-diagnostics", page: "diagnostics", tab: "logs", keywords: "logs events stream" },
  { label: "Cài đặt", hint: "Dịch vụ, upstream, proxy, logging", icon: "i-settings", page: "config", keywords: "settings config cau hinh" },
  { label: "Tài khoản & bảo mật", hint: "Đổi tài khoản quản trị", icon: "i-user", page: "config", tab: "account", keywords: "account password security tai khoan" },
  { label: "Làm mới dữ liệu", hint: "Tải lại monitor, config và model", icon: "i-overview", run: () => $("#refresh-now")?.click(), keywords: "refresh reload lam moi" },
  { label: "Nạp lại cấu hình từ đĩa", hint: "Đọc lại config.json", icon: "i-settings", run: () => $("#reload-config")?.click(), keywords: "reload config cau hinh" },
  { label: "Đổi giao diện", hint: "Sáng / tối / theo hệ thống", icon: "i-settings", run: () => $("#theme-toggle")?.click(), keywords: "theme dark light giao dien" },
  {
    label: "Đổi ngôn ngữ",
    hint: "Tiếng Việt / English",
    icon: "i-book",
    run: () => {
      const select = document.querySelector(".hidden-actions .lang-switch");
      if (!select || !select.options.length) return;
      select.value = [...select.options].find((option) => option.value !== select.value)?.value || select.value;
      select.dispatchEvent(new Event("change", { bubbles: true }));
    },
    keywords: "language ngon ngu viet anh en vi",
  },
  { label: "Đăng xuất", hint: "Kết thúc phiên quản trị", icon: "i-user", run: () => $("#logout")?.click(), keywords: "logout sign out dang xuat" },
];

let commandItems = [];
let commandIndex = 0;
let commandReturnFocus = null;

function commandIcon(name) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("aria-hidden", "true");
  const use = document.createElementNS("http://www.w3.org/2000/svg", "use");
  use.setAttribute("href", "#" + name);
  svg.append(use);
  return svg;
}

function normalizeCommand(value) {
  return String(value || "")
    .toLowerCase()
    .normalize("NFD")
    .replace(/[\u0300-\u036f]/g, "");
}

function runCommand(item) {
  closeCommand();
  if (item.page) openPage(item.page, item.tab);
  else item.run?.();
}

function renderCommands(query) {
  const host = $("#command-results");
  if (!host) return;
  const needle = normalizeCommand(query);
  commandItems = COMMANDS.filter((item) =>
    !needle || normalizeCommand(item.label + " " + item.hint + " " + (item.keywords || "")).includes(needle),
  );
  commandIndex = Math.min(commandIndex, Math.max(0, commandItems.length - 1));
  host.replaceChildren();
  if (!commandItems.length) {
    host.append(el("p", { class: "command-empty", text: "Không tìm thấy trang hoặc hành động." }));
    return;
  }
  commandItems.forEach((item, index) => {
    const button = el("button", {
      class: "command-item" + (index === commandIndex ? " active" : ""),
      type: "button",
      role: "option",
      "aria-selected": String(index === commandIndex),
      onclick: () => runCommand(item),
    }, [
      el("span", { class: "command-icon" }, [commandIcon(item.icon)]),
      el("span", { class: "command-copy" }, [
        el("strong", { text: item.label }),
        el("small", { text: item.hint }),
      ]),
      el("span", { class: "command-enter", text: "↵" }),
    ]);
    host.append(button);
  });
}

function selectCommand(index) {
  if (!commandItems.length) return;
  commandIndex = (index + commandItems.length) % commandItems.length;
  $$(".command-item").forEach((node, i) => {
    node.classList.toggle("active", i === commandIndex);
    node.setAttribute("aria-selected", String(i === commandIndex));
  });
  $$(".command-item")[commandIndex]?.scrollIntoView({ block: "nearest" });
}

function openCommand() {
  const palette = $("#command-palette");
  if (!palette || $("#app").classList.contains("hidden")) return;
  commandReturnFocus = document.activeElement;
  commandIndex = 0;
  palette.classList.remove("hidden");
  document.body.classList.add("command-open");
  const input = $("#command-input");
  if (input) {
    input.value = "";
    input.focus();
  }
  renderCommands("");
}

function closeCommand() {
  const palette = $("#command-palette");
  if (!palette || palette.classList.contains("hidden")) return;
  palette.classList.add("hidden");
  document.body.classList.remove("command-open");
  commandReturnFocus?.focus?.();
  commandReturnFocus = null;
}

$("#command-open")?.addEventListener("click", openCommand);
$("[data-command-close]")?.addEventListener("click", closeCommand);
$("#command-input")?.addEventListener("input", (event) => {
  commandIndex = 0;
  renderCommands(event.target.value);
});

document.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && $("#app")?.classList.contains("nav-open")) {
    closeMenu();
    return;
  }
  const palette = $("#command-palette");
  const isOpen = palette && !palette.classList.contains("hidden");
  if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
    event.preventDefault();
    if (isOpen) closeCommand();
    else openCommand();
    return;
  }
  if (!isOpen) return;
  if (event.key === "Escape") {
    event.preventDefault();
    closeCommand();
  } else if (event.key === "ArrowDown") {
    event.preventDefault();
    selectCommand(commandIndex + 1);
  } else if (event.key === "ArrowUp") {
    event.preventDefault();
    selectCommand(commandIndex - 1);
  } else if (event.key === "Enter") {
    event.preventDefault();
    const item = commandItems[commandIndex];
    if (item) runCommand(item);
  }
});

i18n.onChange(() => {
  renderPageHeader(current);
  logstream.syncPauseButton();
  logstream.render();
});

i18n.init();
boot();

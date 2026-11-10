// Console shell: boot, login, navigation and shared data loading.
import { api, setCsrf, clearCsrf, setUnauthorizedHandler } from "./api.js";
import { refreshMonitor, loadConfig, loadDebugModels } from "./data.js";
import * as i18n from "./i18n.js";
import * as store from "./store.js";
import { $, $$, toast } from "./ui.js";
import * as logstream from "./logstream.js";
import { init as initTheme } from "./theme.js";

import overview from "./pages/overview.js";
import diagnostics from "./pages/diagnostics.js";
import access from "./pages/access.js";
import apiKeys from "./pages/api-keys.js";
import quotas from "./pages/model-quotas.js";
import egress from "./pages/egress.js";
import rotation from "./pages/rotation.js";
import configPage from "./pages/config.js";
import account from "./pages/account.js";

const PAGES = [overview, diagnostics, access, apiKeys, quotas, egress, rotation, configPage, account];
const byId = new Map(PAGES.map((page) => [page.id, page]));

// Hrefs from the previous 13-page layout keep working. A page that absorbed
// several old pages also restores the matching tab instead of its default.
const HASH_ALIASES = {
  guide: "overview",
  usage: "overview",
  egress: "egress",
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
  access: ["diagnostics", "access"],
  availability: ["model-quotas", "free"],
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

function renderEgressHeader() {
  $("#page-title").textContent = i18n.t("Egress & WARP", "page.egress.title");
  $("#page-sub").textContent = i18n.t(
    "WARP nội bộ, proxy usque, peer và ngân sách /64.",
    "page.egress.sub",
  );
}

function setActiveNav(id) {
  $$(".nav-item").forEach((button) =>
    button.classList.toggle("active", button.dataset.page === id),
  );
}

function openEgress() {
  openPage("overview");
  document.querySelector('#page-overview [data-tab="egress"]')?.click();
  history.replaceState(null, "", "#egress");
  setActiveNav("egress");
  renderEgressHeader();
}

function openPage(id) {
  if (id === "egress") {
    openEgress();
    return;
  }
  const page = byId.get(id);
  if (!page) return;
  current?.hide?.();
  if (id === "overview") {
    document.querySelector('#page-overview [data-tab="summary"]')?.click();
  }
  history.replaceState(null, "", `#${id}`);
  setActiveNav(id);
  $$(".page").forEach((node) => node.classList.toggle("hidden", node.id !== `page-${id}`));
  renderPageHeader(page);
  current = page;
  page.show?.();
}

function showLogin() {
  clearCsrf();
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
  const hash = location.hash.slice(1).replace(/^\/+/, "");
  const id = HASH_ALIASES[hash] || hash;
  openPage(byId.has(id) ? id : "overview");
  const tab = HASH_TABS[hash];
  if (tab) document.querySelector(`#page-${tab[0]} [data-tab="${tab[1]}"]`)?.click();
}

async function boot() {
  try {
    await enterConsole(await api("/api/auth/session"));
  } catch {
    showLogin();
  }
}

setUnauthorizedHandler(showLogin);

store.on("monitor", (data) => {
  const version = String(data.version || "");
  $("#version").textContent = version.startsWith("v") || version === "dev" ? version : `v${version}`;
});

for (const page of PAGES) page.init?.();
initTheme();

$$(".nav-item").forEach((button) => {
  button.addEventListener("click", () => openPage(button.dataset.page));
});

$("#quick-api-keys")?.addEventListener("click", () => openPage("api-keys"));
$("#quick-diagnostics")?.addEventListener("click", () => openPage("diagnostics"));
$("#quick-egress")?.addEventListener("click", openEgress);

$$("#page-overview [data-tab]").forEach((tab) => {
  tab.addEventListener("click", () => {
    if (tab.dataset.tab === "egress") {
      setActiveNav("egress");
      history.replaceState(null, "", "#egress");
      renderEgressHeader();
      return;
    }
    setActiveNav("overview");
    renderPageHeader(overview);
  });
});

$("#refresh-now").addEventListener("click", async () => {
  await Promise.all([refreshMonitor(), loadConfig(), loadDebugModels()]);
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

i18n.onChange(() => {
  if (document.querySelector('.nav-item[data-page="egress"]')?.classList.contains("active")) {
    renderEgressHeader();
  } else {
    renderPageHeader(current);
  }
  logstream.syncPauseButton();
  logstream.render();
});

i18n.init();
boot();

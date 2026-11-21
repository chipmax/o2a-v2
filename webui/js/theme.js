// Theme handling: a light/dark choice persisted per browser. The inline head
// script applies it before first paint; this module owns the toggle button and
// the settings radio cards. There is no "auto" mode by design: an operator
// always sees the same console regardless of the host's appearance setting.
import { t, onChange } from "./i18n.js";

const STORAGE_KEY = "opencode2api.ui.theme";
const MODES = ["light", "dark"];
const ICONS = { light: "☀", dark: "☾" };

const stored = () => {
  try {
    return localStorage.getItem(STORAGE_KEY) || "";
  } catch {
    return "";
  }
};

let mode = MODES.includes(stored()) ? stored() : "light";

function label(next = mode) {
  return next === "dark" ? t("Tối", "theme.dark") : t("Sáng", "theme.light");
}

function persisted(next) {
  mode = MODES.includes(next) ? next : "light";
  try {
    localStorage.setItem(STORAGE_KEY, mode);
  } catch (_) {}
  apply();
}

export function setMode(next) {
  persisted(next);
}

function apply() {
  document.documentElement.setAttribute("data-theme", mode);
  const button = document.getElementById("theme-toggle");
  if (button) {
    button.textContent = ICONS[mode] + " " + label();
    button.title = t("Đổi giao diện", "theme.toggle");
    button.setAttribute("aria-label", t("Đổi giao diện", "theme.toggle"));
  }
  document.querySelectorAll("[data-theme-option]").forEach((option) => {
    const active = option.dataset.themeOption === mode;
    option.classList.toggle("active", active);
    const input = option.querySelector("input");
    if (input) input.checked = active;
  });
}

export function init() {
  const button = document.getElementById("theme-toggle");
  if (button)
    button.addEventListener("click", () => {
      persisted(mode === "light" ? "dark" : "light");
    });
  document.querySelectorAll("[data-theme-option] input").forEach((input) => {
    input.addEventListener("change", () => {
      if (input.checked) persisted(input.value);
    });
  });
  onChange(apply);
  apply();
}

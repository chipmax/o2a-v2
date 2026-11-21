// Live log stream: SSE connection, bounded buffer and rendering. The
// connection lifecycle is owned by the console shell.
import { $, el, timeOnly, toast } from "./ui.js";
import { t } from "./i18n.js";

let events = [];
let source = null;
let paused = false;
let visible = false;
let renderPending = false;
let outcome = "all";

function fieldsCell(fields) {
  const cell = el("span", { class: "log-fields" });
  const entries = Object.entries(fields);
  if (!entries.length) return cell;
  for (const [key, value] of entries.slice(0, 8)) {
    const text = typeof value === "object" ? JSON.stringify(value) : String(value);
    cell.append(
      el("span", { class: "log-field" }, [
        el("b", { text: key }),
        document.createTextNode(text.length > 90 ? `${text.slice(0, 90)}…` : text),
      ]),
    );
  }
  if (entries.length > 8) cell.append(el("span", { class: "log-field muted", text: `+${entries.length - 8}` }));
  return cell;
}

function render() {
  const level = $("#log-level").value;
  const component = $("#log-component").value.toLowerCase();
  const query = $("#log-search").value.toLowerCase();
  const matchOutcome = (item) => {
    if (outcome === "all") return true;
    const status = Number(item.fields?.status || 0);
    if (outcome === "error") return item.level === "error" || status >= 500;
    if (outcome === "200") return status === 200;
    if (outcome === "429") return status === 429;
    return true;
  };
  const rows = events
    .filter(
      (item) =>
        matchOutcome(item) &&
        (!level || item.level === level) &&
        (!component || (item.component || "").toLowerCase().includes(component)) &&
        (!query || JSON.stringify(item).toLowerCase().includes(query)),
    )
    .slice(-500);
  const host = $("#log-list");
  host.replaceChildren();
  rows.forEach((item) => {
    const levelClass =
      item.level === "error" ? "bad" : item.level === "warn" ? "warn" : item.level === "info" ? "ok" : "";
    host.append(
      el("div", { class: "log" }, [
        el("span", { class: "log-time", text: timeOnly(item.time) }),
        el("span", { class: levelClass, text: item.level }),
        el("span", { class: "log-component", text: item.component || "core" }),
        el("span", { class: "log-message", text: item.message }),
        fieldsCell(item.fields || {}),
      ]),
    );
  });
  if (!rows.length) host.append(el("div", { class: "empty", text: t("Đang chờ sự kiện log…", "logs.waiting") }));
  host.scrollTop = host.scrollHeight;
}

export function connect() {
  disconnect();
  // The server replays its recent ring on every subscribe; start from a clean
  // buffer so a re-login does not duplicate the previous session's events.
  events = [];
  source = new EventSource("/api/logs/stream");
  source.addEventListener("log", (event) => {
    events.push(JSON.parse(event.data));
    if (events.length > 2000) events.shift();
    if (!paused && visible && !renderPending) {
      renderPending = true;
      requestAnimationFrame(() => {
        renderPending = false;
        render();
      });
    }
  });
  source.addEventListener("gap", () => toast(t("Log cũ hơn đã bị xóa khỏi bộ nhớ", "logs.gap"), true));
}

export function disconnect() {
  if (source) source.close();
  source = null;
}

// The stream tab reports its visibility so hidden panels skip rendering while
// the buffer keeps filling.
export function setVisible(value) {
  visible = value;
  if (visible) render();
}

export function bind() {
  ["#log-level", "#log-component", "#log-search"].forEach((selector) =>
    $(selector).addEventListener("input", render),
  );
  document.querySelectorAll("#log-outcome-chips .outcome-chip").forEach((button) => {
    button.addEventListener("click", () => {
      outcome = button.dataset.outcome || "all";
      document.querySelectorAll("#log-outcome-chips .outcome-chip").forEach((chip) =>
        chip.classList.toggle("active", chip === button),
      );
      render();
    });
  });
  $("#log-pause").addEventListener("click", () => {
    paused = !paused;
    syncPauseButton();
    if (!paused) render();
  });
}

// The button carries the opposite action of the current state, so it is
// rebuilt on every language switch.
export function syncPauseButton() {
  $("#log-pause").textContent = paused
    ? t("Tiếp tục", "logs.resume")
    : t("Tạm dừng", "logs.pause");
}

export { render };

// fetch wrapper: JSON handling, CSRF for writes, 401 routed to the login view.
import * as store from "./store.js";
import { t } from "./i18n.js";

let csrf = "";
let onUnauthorized = null;

export function setCsrf(value) {
  csrf = value || "";
}

export function clearCsrf() {
  csrf = "";
}

export function hasSession() {
  return csrf !== "";
}

export function setUnauthorizedHandler(handler) {
  onUnauthorized = handler;
}

export async function api(path, options = {}) {
  const headers = { ...(options.headers || {}), Accept: "application/json" };
  if (options.body) headers["Content-Type"] = "application/json";
  if (csrf && !["GET", "HEAD"].includes((options.method || "GET").toUpperCase()))
    headers["X-CSRF-Token"] = csrf;
  // A stalled tunnel or upstream must not leave a page permanently in its
  // loading state. Callers that legitimately wait longer can raise timeoutMs.
  const { timeoutMs = 30000, signal, ...fetchOptions } = options;
  const controller = signal ? null : new AbortController();
  let timer;
  const timeout = new Promise((_, reject) => {
    timer = setTimeout(() => {
      controller?.abort();
      reject(new Error(t("Yêu cầu quá thời gian chờ", "error.timeout")));
    }, timeoutMs);
  });
  const request = (async () => {
    const response = await fetch(path, {
      ...fetchOptions,
      headers,
      signal: signal || controller.signal,
    });
    if (response.status === 204) return null;
    const data = await response.json().catch(() => ({}));
    if (!response.ok) {
      // The login endpoint answers 401 for bad credentials: surface its message
      // instead of treating the attempt as an expired session.
      if (response.status === 401 && path !== "/api/auth/login") {
        if (onUnauthorized) onUnauthorized();
        throw new Error(t("Phiên đăng nhập đã hết hạn", "error.session"));
      }
      const error = new Error(data.error?.message || `HTTP ${response.status}`);
      if (data.error?.code) error.code = data.error.code;
      if (data.error?.retry_after_seconds) error.retryAfter = data.error.retry_after_seconds;
      throw error;
    }
    return data;
  })();
  try {
    return await Promise.race([request, timeout]);
  } finally {
    clearTimeout(timer);
  }
}

// Client-facing API base derived from the configured listen address.
export function derivedAPIBase() {
  const config = store.get("config");
  const listen = config?.effective?.listen || config?.listen || ":8080";
  const match = listen.match(/^\[.*\]:(\d+)$/) || listen.match(/:(\d+)$/);
  const port = match?.[1] || "8080";
  return `${location.protocol}//${location.hostname}:${port}`;
}

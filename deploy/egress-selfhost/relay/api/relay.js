// Relay egress for the OpenCode free tier, Bun runtime.
//
// The free tier accepts a client only when the TLS ClientHello lists Bun's
// cipher suites in Bun's order. Run this function on Vercel with the Bun
// runtime (vercel.json: bunVersion) or locally with `bun api/relay.js`, and it
// dials the upstream from its own address with fetch, which already sends that
// handshake. The gateway then speaks plain HTTPS to the relay instead of trying
// to imitate Bun itself.
//
// Protocol (see internal/relay in the gateway):
//   x-relay-target   origin of the upstream, e.g. https://opencode.ai
//   x-relay-path     path and query of the upstream request
//   x-relay-key      POLYGATE_RELAY_TOKEN, compared in constant time
//   x-relay-avoid    comma separated exit addresses that already spent today
//   x-relay-control  ping  -> report runtime and exit address
//                    recycle -> discard this instance (next call gets a new one)
// Answers carry x-relay-runtime, x-relay-instance and x-relay-egress; a call
// whose exit address is in x-relay-avoid is answered 409 with x-relay-rotate: 1.
//
// Environment:
//   POLYGATE_RELAY_TOKEN    required; any long random string
//   POLYGATE_ALLOWED_HOSTS  comma separated hosts, "*.example.com" wildcards,
//                           default "*" (private addresses are always refused)

const TOKEN = process.env.POLYGATE_RELAY_TOKEN ?? "";
const ALLOWED = (process.env.POLYGATE_ALLOWED_HOSTS ?? "*")
  .split(",")
  .map((value) => value.trim().toLowerCase())
  .filter(Boolean);

const INSTANCE = crypto.randomUUID();
const RUNTIME = typeof Bun !== "undefined" ? `bun ${Bun.version}` : `node ${process.versions.node}`;

const DROP = /^(host|connection|content-length|accept-encoding|transfer-encoding|x-relay-.*|forwarded|x-forwarded-.*|x-real-ip|true-client-ip|client-ip|cf-.*|cdn-loop|x-vercel-.*|x-nf-.*|x-amzn-.*|x-invoke|via|x-country|x-azure-.*|fly-.*)$/i;

const json = (status, body, headers = {}) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json", ...headers } });

const sameKey = (given, required) => {
  const a = new TextEncoder().encode(String(given));
  const b = new TextEncoder().encode(String(required));
  let diff = a.length ^ b.length;
  for (let i = 0; i < Math.max(a.length, b.length); i++) diff |= (a[i] ?? 0) ^ (b[i] ?? 0);
  return diff === 0;
};

const hostAllowed = (host) =>
  !/^[\d.]+$/.test(host) &&
  !host.includes(":") &&
  host !== "localhost" &&
  !/\.(localhost|local|internal|lan)$/.test(host) &&
  ALLOWED.some((entry) => entry === "*" || entry === host || (entry.startsWith("*.") && host.endsWith(entry.slice(1))));

let cachedEgress = { value: "", at: 0 };
const egressIp = async () => {
  if (cachedEgress.value && Date.now() - cachedEgress.at < 55000) return cachedEgress.value;
  try {
    const trace = await (await fetch("https://opencode.ai/cdn-cgi/trace", { signal: AbortSignal.timeout(4000) })).text();
    const ip = (trace.match(/^ip=(.+)$/m) ?? [])[1] ?? "";
    if (ip) cachedEgress = { value: ip, at: Date.now() };
    return ip;
  } catch {
    return "";
  }
};

const forward = async (request) => {
  if (!TOKEN) return json(503, { error: "POLYGATE_RELAY_TOKEN is not configured" });
  if (!sameKey(request.headers.get("x-relay-key") ?? "", TOKEN)) return json(401, { error: "unauthorized relay request" });

  const egress = await egressIp();
  const control = request.headers.get("x-relay-control");
  if (control === "ping") return json(200, { runtime: RUNTIME, instance: INSTANCE, egress });
  if (control === "recycle") return new Response(null, { status: 204, headers: { "x-relay-egress": egress } });

  const avoid = (request.headers.get("x-relay-avoid") ?? "").split(",").map((value) => value.trim()).filter(Boolean);
  if (egress && avoid.includes(egress)) {
    return new Response(null, { status: 409, headers: { "x-relay-rotate": "1", "x-relay-egress": egress, "x-relay-instance": INSTANCE } });
  }

  const target = request.headers.get("x-relay-target") ?? "";
  let url;
  try {
    url = new URL(target);
  } catch {
    return json(400, { error: "x-relay-target must be an absolute URL" });
  }
  if (url.protocol !== "https:") return json(400, { error: "x-relay-target must use https" });
  if (!hostAllowed(url.hostname)) return json(403, { error: "target host not allowed" });
  const path = request.headers.get("x-relay-path") ?? "/";
  const upstream = new URL(path.startsWith("/") ? path : "/" + path, url);

  const headers = new Headers();
  for (const [name, value] of request.headers) if (!DROP.test(name)) headers.set(name, value);

  let response;
  try {
    response = await fetch(upstream, {
      method: request.method,
      headers,
      body: request.body,
      duplex: "half",
      redirect: "manual",
      signal: AbortSignal.timeout(290000),
    });
  } catch (error) {
    return json(502, { error: String(error?.message ?? error) });
  }

  const out = new Headers(response.headers);
  if (typeof Bun !== "undefined") {
    // fetch decodes the body, so the wire encoding and its length no longer
    // describe what is sent on: leaving them would make the caller gunzip text.
    out.delete("content-encoding");
    out.delete("content-length");
  }
  out.set("x-relay-runtime", RUNTIME);
  out.set("x-relay-instance", INSTANCE);
  out.set("x-relay-egress", egress);
  return new Response(response.body, { status: response.status, headers: out });
};

export default { fetch: forward };

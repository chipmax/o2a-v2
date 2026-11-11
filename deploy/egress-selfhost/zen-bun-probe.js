// Same v2 wire shape as zen-gate-probe.py's "agent+wire" case, but on Bun's
// fetch stack — the one thing 12errh/zen-proxy says node/python cannot
// substitute for. Usage: bun zen-bun-probe.js <preamble> <tools.json> [model]
const fs = require("fs");
const crypto = require("crypto");

const UA = "opencode/latest/2.1.4/cli";
const V1 = "https://opencode.ai/inference/v1/chat/completions";
const V2 = "https://opencode.ai/inference/openai/v1/chat/completions";
const B62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz";

function session() {
  const inv = (~(((Date.now() << 12) | 1) >>> 0)) & 0xffffffffffff;
  let tail = "";
  for (let i = 0; i < 14; i++) tail += B62[crypto.randomInt(B62.length)];
  return "ses_" + inv.toString(16).padStart(12, "0") + tail;
}
const project = crypto.createHash("sha1").update("directory:/home/ubuntu/project").digest("hex");
function trace() {
  const root = crypto.randomBytes(16).toString("hex");
  const span = crypto.randomBytes(8).toString("hex");
  return { b3: `${root}-${span}-1-${crypto.randomBytes(8).toString("hex")}`, traceparent: `00-${root}-${span}-01` };
}

const [prePath, toolsPath, modelArg] = process.argv.slice(2);
const model = modelArg || "deepseek-v4.1-flash-free";
const preamble = fs.readFileSync(prePath, "utf8");
const rawTools = JSON.parse(fs.readFileSync(toolsPath, "utf8"));
const tools = Array.isArray(rawTools) ? rawTools : rawTools.tools;

const s = session();
const headers = {
  "Content-Type": "application/json",
  "User-Agent": UA,
  Accept: "application/json, text/event-stream",
  "x-opencode-client": "cli",
  "x-opencode-session": s,
  "x-opencode-session-id": s,
  "x-session-affinity": s,
  "x-session-id": s,
  "x-opencode-project": project,
  ...trace(),
};
const body = {
  model,
  messages: [{ role: "system", content: preamble }, { role: "user", content: "say ok" }],
  tools,
  max_tokens: 8,
  stream: false,
};

for (const [name, url] of [["v1", V1], ["v2", V2]]) {
  try {
    const r = await fetch(url, { method: "POST", headers, body: JSON.stringify(body) });
    const t = (await r.text()).replace(/\s+/g, " ").slice(0, 130);
    console.log(`${name} agent+wire -> ${r.status} ${t}`);
  } catch (e) {
    console.log(`${name} -> ERROR ${e}`);
  }
  await Bun.sleep(2000);
}
console.log("bun", Bun.version);

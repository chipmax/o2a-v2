#!/usr/bin/env python3
"""Build a sing-box egress pool from public subscription aggregators.

Fetches already-tested node lists, dedupes real servers, renders one SOCKS
inbound per exit, then probes each exit with curl (egress IP + opencode.ai).
The surviving exits are written as an o2a proxyfile, ordered by latency.

Run as root (it restarts the sing-box unit). No third-party Python packages.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import ipaddress
import json
import subprocess
import sys
import time
from pathlib import Path
from urllib.request import Request, urlopen

SOURCES = [
    "https://raw.githubusercontent.com/LeilaoMi/AutoMergePublicNodes-Optimized/main/output/verified.json",
    "https://raw.githubusercontent.com/snakem982/proxypool/main/source/sing-box-2.json",
]
SKIP_TYPES = {"selector", "urltest", "url-test", "direct", "block", "dns", "wireguard"}
PROBE_URL = "https://api.ipify.org"
TARGET_URL = "https://opencode.ai/"


def fetch(url: str, timeout: int = 30) -> dict:
    req = Request(url, headers={"User-Agent": "o2a-egress-sync/1"})
    with urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode("utf-8", "replace"))


def outbounds(doc: dict) -> list[dict]:
    if isinstance(doc, list):
        return [o for o in doc if isinstance(o, dict)]
    return [o for o in doc.get("outbounds") or [] if isinstance(o, dict)]


def candidates(docs: list[dict], limit: int) -> list[dict]:
    seen_host: set[str] = set()
    seen_node: set[str] = set()
    picked: list[dict] = []
    for doc in docs:
        for node in outbounds(doc):
            kind = str(node.get("type") or "")
            server = str(node.get("server") or "").strip()
            port = node.get("server_port")
            if kind in SKIP_TYPES or not server or not port:
                continue
            node_key = f"{kind}|{server}|{port}"
            if node_key in seen_node:
                continue
            if server in seen_host:
                continue
            seen_node.add(node_key)
            seen_host.add(server)
            picked.append(node)
            if len(picked) >= limit:
                return picked
    return picked


def render(nodes: list[dict], base_port: int) -> tuple[dict, list[int]]:
    inbounds: list[dict] = []
    rules: list[dict] = []
    out: list[dict] = []
    ports: list[int] = []
    for i, node in enumerate(nodes):
        port = base_port + i
        tag = f"exit-{i}"
        item = dict(node)
        item["tag"] = tag
        out.append(item)
        inbounds.append(
            {
                "type": "socks",
                "tag": f"in-{i}",
                "listen": "127.0.0.1",
                "listen_port": port,
            }
        )
        rules.append({"inbound": [f"in-{i}"], "outbound": tag})
        ports.append(port)
    out.append({"type": "block", "tag": "block"})
    cfg = {
        "log": {"level": "error"},
        "inbounds": inbounds,
        "outbounds": out,
        "route": {"rules": rules, "final": "block"},
    }
    return cfg, ports


def probe(port: int, ip_timeout: int, target_timeout: int) -> dict:
    started = time.monotonic()
    try:
        ip = subprocess.run(
            [
                "curl", "-s", "-m", str(ip_timeout), "--socks5-hostname",
                f"127.0.0.1:{port}", PROBE_URL,
            ],
            capture_output=True,
            text=True,
            timeout=ip_timeout + 3,
        ).stdout.strip()
    except subprocess.TimeoutExpired:
        ip = ""
    if not ip:
        return {"port": port, "ok": False, "ip": "", "code": "000", "ms": 0}
    try:
        code = subprocess.run(
            [
                "curl", "-s", "-m", str(target_timeout), "-o", "/dev/null",
                "-w", "%{http_code}", "--socks5-hostname",
                f"127.0.0.1:{port}", TARGET_URL,
            ],
            capture_output=True,
            text=True,
            timeout=target_timeout + 3,
        ).stdout.strip()
    except subprocess.TimeoutExpired:
        code = "000"
    ok = code in {"200", "301", "302", "307"}
    return {
        "port": port,
        "ok": ok,
        "ip": ip,
        "code": code,
        "ms": int((time.monotonic() - started) * 1000),
    }


def is_public_ip(value: str) -> bool:
    try:
        addr = ipaddress.ip_address(value)
    except ValueError:
        return False
    return addr.is_global and not addr.is_multicast


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--workdir", default="/opt/o2a-v2/egress")
    ap.add_argument("--base-port", type=int, default=22001)
    ap.add_argument("--limit", type=int, default=60)
    ap.add_argument("--keep", type=int, default=24)
    ap.add_argument("--workers", type=int, default=8)
    ap.add_argument("--ip-timeout", type=int, default=6)
    ap.add_argument("--target-timeout", type=int, default=8)
    ap.add_argument("--no-restart", action="store_true")
    args = ap.parse_args()

    workdir = Path(args.workdir)
    workdir.mkdir(parents=True, exist_ok=True)

    docs = []
    for url in SOURCES:
        try:
            doc = fetch(url)
            docs.append(doc)
            print(f"source ok: {url} ({len(outbounds(doc))} outbounds)", file=sys.stderr)
        except Exception as exc:  # noqa: BLE001 - a failed source is not fatal
            print(f"source failed: {url}: {exc}", file=sys.stderr)
    if not docs:
        print("no source available", file=sys.stderr)
        return 2

    nodes = candidates(docs, args.limit)
    if not nodes:
        print("no usable outbound", file=sys.stderr)
        return 2

    cfg, ports = render(nodes, args.base_port)
    config_path = workdir / "sing-box.json"
    config_path.write_text(json.dumps(cfg, ensure_ascii=False), encoding="utf-8")

    if not args.no_restart:
        subprocess.run(["systemctl", "restart", "o2a-egress-singbox"], check=False)
        time.sleep(3)

    results = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as pool:
        futures = [
            pool.submit(probe, p, args.ip_timeout, args.target_timeout) for p in ports
        ]
        for fut in concurrent.futures.as_completed(futures):
            results.append(fut.result())

    good = [r for r in results if r["ok"] and is_public_ip(r["ip"])]
    by_ip: dict[str, dict] = {}
    for item in good:
        current = by_ip.get(item["ip"])
        if current is None or item["ms"] < current["ms"]:
            by_ip[item["ip"]] = item
    good = sorted(by_ip.values(), key=lambda r: r["ms"])
    kept = good[: args.keep]

    proxyfile = workdir / "egress.txt"
    proxyfile.write_text(
        "".join(f"socks5://127.0.0.1:{r['port']}\n" for r in kept), encoding="utf-8"
    )
    summary = {
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "candidates": len(nodes),
        "probed": len(results),
        "alive": len(good),
        "kept": len(kept),
        "exits": kept,
        "failed": [r for r in results if not r["ok"]],
    }
    (workdir / "state.json").write_text(json.dumps(summary, indent=2), encoding="utf-8")
    print(json.dumps({k: summary[k] for k in ("candidates", "probed", "alive", "kept")}))
    return 0 if kept else 1


if __name__ == "__main__":
    raise SystemExit(main())

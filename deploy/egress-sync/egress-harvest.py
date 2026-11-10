#!/usr/bin/env python3
"""Harvest and admission-test free egress exits for the o2a free tier.

The OpenCode free tier is bucketed per egress IP and resets at 00:00 UTC. A
public node is only useful if it (a) connects and (b) still has quota, so this
tool tests every candidate with the real free-tier request and keeps only the
ones that answer 200. Reachable-but-spent exits are dropped.

Sources: sing-box JSON aggregators (rendered into one sing-box instance with a
SOCKS inbound per node) and plain HTTP/SOCKS proxy lists. No third-party
Python packages; curl and sing-box do the network work.

    sudo python3 egress-harvest.py --workdir /opt/o2a-v2/egress --limit 300
"""

from __future__ import annotations

import argparse
import concurrent.futures
import ipaddress
import json
import os
import subprocess
import sys
import time
from pathlib import Path
from urllib.request import Request, urlopen

SINGBOX_SOURCES = [
    "https://raw.githubusercontent.com/LeilaoMi/AutoMergePublicNodes-Optimized/main/output/global.json",
    "https://raw.githubusercontent.com/LeilaoMi/AutoMergePublicNodes-Optimized/main/output/verified.json",
    "https://raw.githubusercontent.com/snakem982/proxypool/main/source/sing-box-2.json",
    "https://raw.githubusercontent.com/Argh94/Proxy-List/main/singbox.json",
]
PLAIN_SOURCES = [
    ("https://raw.githubusercontent.com/roosterkid/openproxylist/main/SOCKS5.txt", "socks5"),
    ("https://raw.githubusercontent.com/roosterkid/openproxylist/main/HTTPS.txt", "http"),
    ("https://raw.githubusercontent.com/TheSpeedX/PROXY-List/master/socks5.txt", "socks5"),
    ("https://raw.githubusercontent.com/TheSpeedX/PROXY-List/master/http.txt", "http"),
    ("https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/socks5.txt", "socks5"),
    ("https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/http.txt", "http"),
    ("https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/all/data.txt", ""),
]
SKIP_TYPES = {"selector", "urltest", "url-test", "direct", "block", "dns", "wireguard", "http", "anytls"}
FREE_TIER_URL = "https://opencode.ai/inference/v1/chat/completions"
IP_URL = "https://api.ipify.org"
BODY = json.dumps(
    {
        "model": "deepseek-v4.1-flash-free",
        "messages": [{"role": "user", "content": "hi"}],
        "max_tokens": 4,
    }
)


def curl(args: list[str], timeout: int) -> str:
    try:
        done = subprocess.run(
            ["curl", "-s", "-m", str(timeout), *args],
            capture_output=True,
            text=True,
            timeout=timeout + 3,
        )
        return (done.stdout or "").strip()
    except Exception:
        return ""


def fetch(url: str, timeout: int = 30) -> str:
    """Fetch through curl: urllib can block in DNS with no usable timeout."""
    try:
        done = subprocess.run(
            ["curl", "-sL", "-m", str(timeout), "-A", "o2a-egress-harvest/2", url],
            capture_output=True,
            timeout=timeout + 5,
        )
        return (done.stdout or b"").decode("utf-8", "replace")
    except Exception:
        return ""


def fetch_json(url: str) -> object | None:
    text = fetch(url)
    try:
        return json.loads(text)
    except Exception:
        return None


def fetch_text(url: str) -> str:
    return fetch(url)


def singbox_nodes(doc: object, limit: int) -> list[dict]:
    if isinstance(doc, dict):
        raw = doc.get("outbounds") or []
    elif isinstance(doc, list):
        raw = doc
    else:
        return []
    picked = []
    seen = set()
    for node in raw:
        if not isinstance(node, dict):
            continue
        kind = str(node.get("type") or "")
        server = str(node.get("server") or "").strip()
        port = node.get("server_port")
        if kind in SKIP_TYPES or not server or not port:
            continue
        key = (kind, server, int(port))
        if key in seen:
            continue
        seen.add(key)
        picked.append(node)
        if len(picked) >= limit:
            break
    return picked


def plain_proxies(text: str, hint: str, limit: int) -> list[str]:
    out = []
    seen = set()
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#") or line.startswith("//"):
            continue
        if "://" in line:
            scheme, _, rest = line.partition("://")
            scheme = scheme.lower()
            if scheme not in ("socks5", "socks5h", "http", "https"):
                continue
        elif hint:
            scheme, rest = hint, line
        else:
            continue
        host, _, port = rest.partition(":")
        host = host.strip("[]")
        if not host or not port.split("#")[0].strip().isdigit():
            continue
        key = (scheme, host, int(port.split("#")[0]))
        if key in seen:
            continue
        seen.add(key)
        out.append(f"{scheme}://{host}:{key[2]}")
        if len(out) >= limit:
            break
    return out


def render(nodes: list[dict], base_port: int) -> tuple[dict, list[int]]:
    inbounds, rules, out, ports = [], [], [], []
    for i, node in enumerate(nodes):
        port = base_port + i
        tag = f"exit-{i}"
        item = dict(node)
        item["tag"] = tag
        out.append(item)
        inbounds.append(
            {"type": "socks", "tag": f"in-{i}", "listen": "127.0.0.1", "listen_port": port}
        )
        rules.append({"inbound": [f"in-{i}"], "outbound": tag})
        ports.append(port)
    config = {
        "log": {"level": "error"},
        "inbounds": inbounds,
        "outbounds": out + [{"type": "block", "tag": "block"}],
        "route": {"rules": rules, "final": "block"},
    }
    return config, ports


def probe(proxy: str, via_socks: bool) -> dict:
    prefix = f"{proxy}" if via_socks else proxy
    url = curl(["-x", prefix, "-o", "/dev/null", "-w", "%{http_code}", IP_URL], 8)
    ip = url if url.count(".") == 3 else ""
    status = curl(
        [
            "-x",
            prefix,
            "-H",
            "Content-Type: application/json",
            "-o",
            "/dev/null",
            "-w",
            "%{http_code}",
            "-d",
            BODY,
            FREE_TIER_URL,
        ],
        25,
    )
    return {"proxy": prefix, "ip": ip, "status": status}


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
    ap.add_argument("--limit", type=int, default=300, help="sing-box nodes to render")
    ap.add_argument("--plain-limit", type=int, default=1200, help="plain proxies to test")
    ap.add_argument("--keep", type=int, default=24, help="verified exits to publish")
    ap.add_argument("--workers", type=int, default=40)
    ap.add_argument("--service", default="o2a-egress-singbox.service")
    ap.add_argument("--singbox", default="/opt/zen-gate/data/bin/sing-box")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    work = Path(args.workdir)
    work.mkdir(parents=True, exist_ok=True)
    started = time.time()

    nodes: list[dict] = []
    per_source: dict[str, int] = {}
    for url in SINGBOX_SOURCES:
        got = singbox_nodes(fetch_json(url), args.limit)
        per_source[url] = len(got)
        nodes.extend(got)
    nodes = singbox_nodes({"outbounds": nodes}, args.limit)

    plain: list[str] = []
    for url, hint in PLAIN_SOURCES:
        got = plain_proxies(fetch_text(url), hint, args.plain_limit)
        per_source[url] = len(got)
        plain.extend(got)
    plain = plain[: args.plain_limit]

    print(json.dumps({"nodes": len(nodes), "plain": len(plain), "sources": per_source}), flush=True)
    if args.dry_run:
        return 0

    config, ports = render(nodes, args.base_port)
    (work / "singbox.json").write_text(json.dumps(config), encoding="utf-8")
    subprocess.run(["systemctl", "restart", args.service], check=False)
    time.sleep(4)

    tasks: list[tuple[str, bool]] = [(f"socks5://127.0.0.1:{p}", True) for p in ports]
    tasks += [(p, False) for p in plain]

    results: list[dict] = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as pool:
        futures = [pool.submit(probe, proxy, socks) for proxy, socks in tasks]
        for done in concurrent.futures.as_completed(futures):
            try:
                results.append(done.result())
            except Exception:
                pass

    good = [r for r in results if r["status"] == "200" and is_public_ip(r["ip"])]
    by_ip: dict[str, dict] = {}
    for item in good:
        by_ip.setdefault(item["ip"], item)
    kept = list(by_ip.values())[: args.keep]

    egress = work / "egress.txt"
    egress.write_text("".join(item["proxy"] + "\n" for item in kept), encoding="utf-8")
    state = {
        "started": started,
        "duration_s": round(time.time() - started, 1),
        "nodes": len(nodes),
        "plain": len(plain),
        "tested": len(results),
        "status": {
            "200": sum(1 for r in results if r["status"] == "200"),
            "429": sum(1 for r in results if r["status"] == "429"),
            "other": sum(1 for r in results if r["status"] not in ("200", "429")),
        },
        "kept": len(kept),
        "sources": per_source,
        "exits": [
            {"proxy": item["proxy"], "ip": item["ip"]} for item in kept
        ],
    }
    (work / "state.json").write_text(json.dumps(state, indent=2), encoding="utf-8")
    print(json.dumps({k: state[k] for k in ("tested", "status", "kept", "duration_s")}), flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())

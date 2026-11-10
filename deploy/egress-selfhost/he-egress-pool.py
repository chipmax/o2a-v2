#!/usr/bin/env python3
"""Turn a Hurricane Electric routed /48 into many free-tier egress buckets.

Quota is bucketed per egress IP (IPv6: per /64, see README.vi.md), so one
address from each /64 of the routed /48 is an independent daily allowance.
This creates one direct sing-box outbound per /64, bound to that source
address, exposes a SOCKS inbound per outbound, admission-tests each one with
the real free-tier request, and publishes only the ones that answer 200 as an
o2a proxyfile.

    sudo ./he-egress-pool.py --routed 2001:470:1f0b:1a2b::/48 --count 16
    sudo ./he-egress-pool.py --routed ... --count 16 --test-only

Requires the 6in4 tunnel from he-ipv6-tunnel.sh (the /48 must already route to
this host) and sing-box at /opt/o2a-v2/bin/sing-box.
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

FREE_TIER_URL = "https://opencode.ai/inference/v1/chat/completions"
BODY = json.dumps(
    {
        "model": "deepseek-v4.1-flash-free",
        "messages": [{"role": "user", "content": "hi"}],
        "max_tokens": 4,
    }
)
DUMMY = "he-v6"


def run(args: list[str], timeout: int = 30) -> subprocess.CompletedProcess:
    return subprocess.run(args, capture_output=True, text=True, timeout=timeout)


def ranges(routed: str, count: int) -> list[str]:
    net = ipaddress.ip_network(routed, strict=False)
    if net.version != 6 or net.prefixlen != 48:
        raise SystemExit("--routed must be an IPv6 /48")
    out = []
    for i, subnet in enumerate(net.subnets(new_prefix=64)):
        if i >= count:
            break
        out.append(str(subnet.network_address + 1))
    return out


def render(addresses: list[str], base_port: int) -> tuple[dict, list[tuple[str, int]]]:
    inbounds, rules, outbounds, ports = [], [], [], []
    for i, addr in enumerate(addresses):
        tag = f"he-{i}"
        outbounds.append({"type": "direct", "tag": tag, "inet6_bind_address": addr})
        inbounds.append(
            {"type": "socks", "tag": f"in-{i}", "listen": "127.0.0.1", "listen_port": base_port + i}
        )
        rules.append({"inbound": [f"in-{i}"], "outbound": tag})
        ports.append((addr, base_port + i))
    config = {
        "log": {"level": "error"},
        "inbounds": inbounds,
        "outbounds": outbounds + [{"type": "block", "tag": "block"}],
        "route": {"rules": rules, "final": "block"},
    }
    return config, ports


def free_tier(port: int) -> str:
    done = run(
        [
            "curl", "-s", "-m", "30", "-o", "/dev/null", "-w", "%{http_code}",
            "-x", f"socks5h://127.0.0.1:{port}",
            "-H", "Content-Type: application/json", "-d", BODY, FREE_TIER_URL,
        ],
        40,
    )
    return (done.stdout or "").strip()


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--routed", required=True, help="routed /48, e.g. 2001:470:xxxx:yyyy::/48")
    ap.add_argument("--count", type=int, default=16, help="number of /64s to use")
    ap.add_argument("--base-port", type=int, default=24001)
    ap.add_argument("--workdir", default="/opt/o2a-v2/egress-he")
    ap.add_argument("--singbox", default="/opt/o2a-v2/bin/sing-box")
    ap.add_argument("--unit", default="o2a-egress-he")
    ap.add_argument("--test-only", action="store_true")
    args = ap.parse_args()

    work = Path(args.workdir)
    work.mkdir(parents=True, exist_ok=True)
    addresses = ranges(args.routed, args.count)
    config, ports = render(addresses, args.base_port)

    (work / "singbox.json").write_text(json.dumps(config, indent=2), encoding="utf-8")
    checked = run([args.singbox, "check", "-c", str(work / "singbox.json")])
    if checked.returncode != 0:
        print("sing-box config rejected: " + (checked.stderr or checked.stdout)[:400], file=sys.stderr)
        return 1
    print(f"rendered {len(addresses)} outbounds -> {work / 'singbox.json'}")

    if not args.test_only:
        for addr in addresses:
            run(["ip", "-6", "addr", "replace", f"{addr}/64", "dev", DUMMY])
        run(["systemctl", "restart", args.unit])
        time.sleep(4)
    else:
        run(["systemctl", "restart", args.unit])
        time.sleep(4)

    results: list[dict] = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=min(8, len(ports))) as pool:
        futures = {pool.submit(free_tier, port): addr for addr, port in ports}
        for done in concurrent.futures.as_completed(futures):
            addr = futures[done]
            try:
                status = done.result()
            except Exception:
                status = "000"
            results.append({"addr": addr, "status": status})

    good = [r for r in results if r["status"] == "200"]
    egress = work / "egress.txt"
    ports_by_addr = {addr: port for addr, port in ports}
    egress.write_text(
        "".join(f"socks5://127.0.0.1:{ports_by_addr[r['addr']]}\n" for r in good),
        encoding="utf-8",
    )
    summary = {
        "routed": args.routed,
        "tested": len(results),
        "ok": len(good),
        "status": {s: sum(1 for r in results if r["status"] == s) for s in {r["status"] for r in results}},
        "published": str(egress),
        "exits": good,
    }
    (work / "state.json").write_text(json.dumps(summary, indent=2), encoding="utf-8")
    print(json.dumps({k: summary[k] for k in ("tested", "ok", "status")}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())

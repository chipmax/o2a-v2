#!/usr/bin/env python3
"""Probe the OpenCode free lane from many IPv6 /128s in the HE routed /64.

Purpose: learn whether the free-lane allowance is per-IP (fresh IPs get 200
after the 00:00 UTC reset) or a single global bucket.

Writes JSON lines to --out and prints a summary table.
"""
import argparse
import ipaddress
import json
import os
import subprocess
import sys
import time
import urllib.request
from concurrent.futures import ThreadPoolExecutor

API = "https://opencode.ai/inference/v1/chat/completions"
BODY = json.dumps({
    "model": "deepseek-v4.1-flash-free",
    "messages": [{"role": "user", "content": "hi"}],
    "max_tokens": 4,
}).encode()

IFACE = "he-v6"


def ensure_addr(addr):
    subprocess.run(["sudo", "ip", "addr", "add", f"{addr}/64", "dev", IFACE],
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def probe(addr, timeout=35):
    opener = urllib.request.build_opener()
    req = urllib.request.Request(API, data=BODY, headers={"Content-Type": "application/json"})
    # urllib cannot bind a source address directly; shell out to curl.
    out = subprocess.run(
        ["curl", "-6", "-s", "-m", str(timeout), "--interface", addr,
         "-o", "-", "-w", "\n%{http_code}", API,
         "-H", "Content-Type: application/json", "--data-binary", "@-"],
        input=BODY, capture_output=True)
    raw = out.stdout.decode("utf-8", "replace")
    code = raw.rsplit("\n", 1)[-1].strip() if "\n" in raw else "000"
    body = raw.rsplit("\n", 1)[0][:120]
    return {"addr": addr, "code": code, "body": body, "ts": int(time.time())}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--prefix", default="2001:470:36:965::")
    ap.add_argument("--count", type=int, default=20)
    ap.add_argument("--out", default="/opt/o2a-v2/egress/he-reset.json")
    ap.add_argument("--pause", type=float, default=0.0)
    args = ap.parse_args()

    base = ipaddress.ip_address(args.prefix.split("/")[0])
    addrs = [str(base + i) for i in range(1, args.count + 1)]
    for a in addrs:
        ensure_addr(a)
    if args.pause:
        time.sleep(args.pause)

    with ThreadPoolExecutor(max_workers=min(20, len(addrs))) as pool:
        results = list(pool.map(probe, addrs))

    ok = [r for r in results if r["code"] == "200"]
    os.makedirs(os.path.dirname(args.out), exist_ok=True)
    with open(args.out, "a") as fh:
        for r in results:
            fh.write(json.dumps(r) + "\n")

    print(f"probed={len(results)} ok200={len(ok)}")
    for r in results:
        print(f'{r["addr"]:>28} {r["code"]:>4} {r["body"][:60]}')
    return 0 if ok else 2


if __name__ == "__main__":
    sys.exit(main())

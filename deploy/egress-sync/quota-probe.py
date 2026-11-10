#!/usr/bin/env python3
"""Measure one egress IP's daily free-tier allowance.

Sends the cheapest possible free-tier request through the given egress until
the gateway answers 429, then reports how many went through and when the
window resets. Use it right after 00:00 UTC to learn the real per-IP budget:

    sudo python3 quota-probe.py --egress direct --max 600 --out /opt/o2a-v2/egress/quota.json
    sudo python3 quota-probe.py --egress socks5://127.0.0.1:9051

`--egress direct` uses the machine's own route; any proxy URL works otherwise.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import subprocess
import sys
import time

URL = "https://opencode.ai/inference/v1/chat/completions"
BODY = json.dumps(
    {
        "model": "deepseek-v4.1-flash-free",
        "messages": [{"role": "user", "content": "hi"}],
        "max_tokens": 1,
    }
)


def request(egress: str) -> tuple[str, str, float]:
    args = ["curl", "-s", "-m", "30", "-o", "/dev/null", "-D", "-", "-w", "\\n%{http_code}", "-H", "Content-Type: application/json", "-d", BODY]
    if egress != "direct":
        args[1:1] = ["-x", egress]
    started = time.time()
    done = subprocess.run(args + [URL], capture_output=True, text=True, timeout=40)
    took = time.time() - started
    out = done.stdout or ""
    status = out.rsplit("\n", 1)[-1].strip()
    retry = ""
    for line in out.splitlines():
        if line.lower().startswith("retry-after:"):
            retry = line.split(":", 1)[1].strip()
    return status, retry, took


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--egress", default="direct")
    ap.add_argument("--max", type=int, default=600)
    ap.add_argument("--out", default="")
    ap.add_argument("--pause", type=float, default=0.2)
    args = ap.parse_args()

    started = dt.datetime.now(dt.timezone.utc)
    count = 0
    status = ""
    retry = ""
    errors = 0
    while count < args.max:
        status, retry, _ = request(args.egress)
        if status == "200":
            count += 1
            time.sleep(args.pause)
            continue
        if status in ("000", ""):
            errors += 1
            if errors > 5:
                break
            time.sleep(1)
            continue
        break
    result = {
        "egress": args.egress,
        "started_utc": started.isoformat(timespec="seconds"),
        "finished_utc": dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds"),
        "allowance": count,
        "final_status": status,
        "retry_after_s": retry,
        "errors": errors,
    }
    line = json.dumps(result)
    print(line)
    if args.out:
        with open(args.out, "w", encoding="utf-8") as fh:
            fh.write(line + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())

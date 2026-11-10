#!/usr/bin/env python3
"""Load-test an OpenAI-compatible endpoint and print per-phase statistics.

No third-party packages. Intended for the egress lane: point --url at a
disposable gateway (never production) and pass an anonymous-only config.

    python3 high-load-test.py --url http://127.0.0.1:18002/v1/chat/completions \
        --key oc_sk_test --model deepseek-v4.1-flash-free \
        --phases warm:20:4 load:120:12 stress:240:24
"""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import time
import urllib.error
import urllib.request
from collections import Counter


def one(url: str, key: str, model: str, prompt: str, max_tokens: int) -> tuple[int, float]:
    body = json.dumps(
        {
            "model": model,
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": max_tokens,
            "stream": False,
        }
    ).encode()
    req = urllib.request.Request(
        url,
        data=body,
        headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"},
    )
    started = time.monotonic()
    try:
        with urllib.request.urlopen(req, timeout=90) as resp:
            resp.read()
            return resp.status, time.monotonic() - started
    except urllib.error.HTTPError as exc:
        exc.read()
        return exc.code, time.monotonic() - started
    except Exception:  # noqa: BLE001 - a transport failure is a result here
        return 0, time.monotonic() - started


def phase(url: str, key: str, model: str, prompt: str, max_tokens: int, n: int, conc: int) -> dict:
    started = time.monotonic()
    with concurrent.futures.ThreadPoolExecutor(max_workers=conc) as pool:
        results = list(pool.map(lambda _: one(url, key, model, prompt, max_tokens), range(n)))
    duration = time.monotonic() - started
    lat = sorted(item[1] for item in results)

    def pct(p: float) -> float:
        return lat[min(len(lat) - 1, int(len(lat) * p))]

    return {
        "n": n,
        "concurrency": conc,
        "duration_s": round(duration, 2),
        "rps": round(n / duration, 2),
        "status": dict(Counter(item[0] for item in results)),
        "p50_s": round(pct(0.5), 2),
        "p95_s": round(pct(0.95), 2),
        "max_s": round(lat[-1], 2),
    }


def parse_phase(value: str) -> tuple[str, int, int]:
    name, n, conc = value.split(":")
    return name, int(n), int(conc)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True)
    ap.add_argument("--key", required=True)
    ap.add_argument("--model", default="deepseek-v4.1-flash-free")
    ap.add_argument("--prompt", default="Reply with ok.")
    ap.add_argument("--max-tokens", type=int, default=16)
    ap.add_argument("--phases", default="warm:20:4 load:120:12 stress:240:24")
    ap.add_argument("--out", default="")
    args = ap.parse_args()

    phases = [parse_phase(item) for item in args.phases.split(",")]
    output = []
    for name, n, conc in phases:
        result = phase(
            args.url, args.key, args.model, args.prompt, args.max_tokens, n, conc
        )
        result["phase"] = name
        print(json.dumps(result))
        output.append(result)
    if args.out:
        with open(args.out, "w", encoding="utf-8") as fh:
            json.dump(output, fh, indent=2)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

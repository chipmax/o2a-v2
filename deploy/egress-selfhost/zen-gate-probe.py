#!/usr/bin/env python3
"""Probe the Zen free gateway with the *request shape* from 12errh/zen-proxy:
harness preamble + OpenCode v2 identity headers (+ optional tool manifest).

Compares four variants from ONE egress so a status difference cannot be blamed
on the IP: bare body, preamble only, full v2 wire, full v2 wire on the v2 path.

Usage: zen-gate-probe.py <preamble.txt> <agent-tools.json> [model]
"""
import hashlib
import json
import os
import secrets
import sys
import time
import urllib.request

UA = "opencode/latest/2.1.4/cli"
PATH_V1 = "https://opencode.ai/inference/v1/chat/completions"
PATH_V2 = "https://opencode.ai/inference/openai/v1/chat/completions"
B62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"


def session():
    inv = (~((int(time.time() * 1000) << 12) | 1)) & 0xFFFFFFFFFFFF
    return "ses_%012x%s" % (inv, "".join(secrets.choice(B62) for _ in range(14)))


def project():
    return hashlib.sha1(b"directory:/home/ubuntu/project").hexdigest()


def trace():
    root = secrets.token_hex(16)
    span = secrets.token_hex(8)
    return {"b3": f"{root}-{span}-1-{secrets.token_hex(8)}",
            "traceparent": f"00-{root}-{span}-01"}


def post(url, body, headers):
    req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST")
    for k, v in headers.items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, r.read(220).decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read(220).decode("utf-8", "replace")
    except Exception as e:
        return 0, f"{type(e).__name__}: {e}"


def main():
    preamble = open(sys.argv[1], encoding="utf-8").read()
    tools_raw = json.load(open(sys.argv[2], encoding="utf-8"))
    tools = tools_raw["tools"] if isinstance(tools_raw, dict) and "tools" in tools_raw else tools_raw
    model = sys.argv[3] if len(sys.argv) > 3 else "deepseek-v4.1-flash-free"
    bare = {"model": model, "messages": [{"role": "user", "content": "say ok"}], "max_tokens": 8}
    agent = {"model": model,
             "messages": [{"role": "system", "content": preamble},
                          {"role": "user", "content": "say ok"}],
             "tools": tools, "max_tokens": 8, "stream": False}
    min_h = {"Content-Type": "application/json", "User-Agent": UA}
    s, p = session(), project()
    full_h = {"Content-Type": "application/json", "User-Agent": UA,
              "Accept": "application/json, text/event-stream",
              "x-opencode-client": "cli",
              "x-opencode-session": s, "x-opencode-session-id": s,
              "x-session-affinity": s, "x-session-id": s,
              "x-opencode-project": p, **trace()}
    cases = [
        ("v1 bare      ", PATH_V1, bare, min_h),
        ("v1 preamble  ", PATH_V1, agent, min_h),
        ("v1 agent+wire", PATH_V1, agent, full_h),
        ("v2 bare      ", PATH_V2, bare, min_h),
        ("v2 preamble  ", PATH_V2, agent, min_h),
        ("v2 agent+wire", PATH_V2, agent, full_h),
    ]
    print("preamble chars:", len(preamble), "| tools:", len(tools))
    for name, url, body, hdr in cases:
        code, text = post(url, body, hdr)
        text = " ".join(text.split())[:110]
        print(f"{name} -> {code} {text}")
        time.sleep(2)


if __name__ == "__main__":
    main()

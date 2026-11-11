#!/usr/bin/env python3
"""Egress x lane matrix: does a zen key buy quota on an egress the anonymous
lane already refuses?

Lanes: anon (no Authorization) and zen (the configured key). Egresses: whatever
addresses are passed on the command line (bound with curl --interface).
"""
import json
import secrets
import string
import subprocess
import sys
import time

B62 = string.digits + string.ascii_uppercase + string.ascii_lowercase
URL = "https://opencode.ai/inference/v1/chat/completions"


def session():
    inv = (~((int(time.time() * 1000) << 12) | 1)) & 0xFFFFFFFFFFFF
    return "ses_%012x%s" % (inv, "".join(secrets.choice(B62) for _ in range(14)))


def probe(addr, key, model):
    s = session()
    body = json.dumps({"model": model, "messages": [{"role": "user", "content": "say ok"}],
                       "max_tokens": 8}).encode()
    cmd = ["curl", "-s", "-m", "45", "-o", "/tmp/m.out", "-w", "%{http_code}"]
    if addr:
        cmd += ["--interface", addr]
    for h in ("User-Agent: opencode/latest/2.1.1/cli", "x-opencode-client: cli",
              "x-opencode-session: " + s, "x-opencode-session-id: " + s,
              "x-session-affinity: " + s, "X-Session-Id: " + s,
              "x-opencode-project: " + secrets.token_hex(20),
              "Content-Type: application/json"):
        cmd += ["-H", h]
    if key:
        cmd += ["-H", "Authorization: Bearer " + key]
    cmd += ["--data-binary", "@-", URL]
    r = subprocess.run(cmd, input=body, capture_output=True)
    code = r.stdout.decode().strip() or "000"
    try:
        t = open("/tmp/m.out", "rb").read(90).decode("utf-8", "replace").replace("\n", " ")
    except OSError:
        t = ""
    return code, " ".join(t.split())[:60]


if __name__ == "__main__":
    key = sys.argv[1] if len(sys.argv) > 1 and sys.argv[1] != "-" else ""
    addrs = sys.argv[2:]
    for model in ("deepseek-v4.1-flash-free", "big-pickle"):
        for addr in addrs or [None]:
            code, t = probe(addr, key, model)
            tag = "zen" if key else "anon"
            print(f"{model:26s} {tag:4s} {addr or 'direct':26s} -> {code} {t}")
            time.sleep(1)

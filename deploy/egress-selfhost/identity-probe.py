#!/usr/bin/env python3
"""Test the free lane with the real v2 identity headers from several /128s."""
import json, secrets, string, subprocess, sys, time

B62 = string.digits + string.ascii_uppercase + string.ascii_lowercase
URL = "https://opencode.ai/inference/v1/chat/completions"


def session_id():
    inv = (~((int(time.time() * 1000) << 12) | 1)) & 0xFFFFFFFFFFFF
    return "ses_%012x%s" % (inv, "".join(secrets.choice(B62) for _ in range(14)))


def project_id():
    return secrets.token_hex(20)


def probe(addr=None):
    s = session_id()
    body = json.dumps({
        "model": "deepseek-v4.1-flash-free",
        "messages": [{"role": "user", "content": "say ok"}],
        "max_tokens": 8,
    }).encode()
    hdrs = [
        "User-Agent: opencode/latest/2.1.1/cli",
        "x-opencode-client: cli",
        "x-opencode-session: " + s,
        "x-opencode-session-id: " + s,
        "x-session-affinity: " + s,
        "X-Session-Id: " + s,
        "x-opencode-project: " + project_id(),
        "Content-Type: application/json",
    ]
    cmd = ["curl", "-s", "-m", "40", "-o", "/tmp/probe.out", "-w", "%{http_code}"]
    if addr:
        cmd += ["--interface", addr]
    for h in hdrs:
        cmd += ["-H", h]
    cmd += ["--data-binary", "@-", URL]
    out = subprocess.run(cmd, input=body, capture_output=True)
    code = out.stdout.decode().strip() or "000"
    try:
        tail = open("/tmp/probe.out", "rb").read(120).decode("utf-8", "replace").replace("\n", " ")
    except OSError:
        tail = ""
    return code, tail


if __name__ == "__main__":
    addrs = sys.argv[1:] or [None]
    for a in addrs:
        code, tail = probe(a)
        print(f"{a or 'default'} -> {code} {tail}")

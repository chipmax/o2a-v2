#!/usr/bin/env python3
"""Register one fresh WARP device (curl), bring it up with wg-quick, then measure
how many free-lane requests that ONE fresh WARP egress answers with 200.

Env: PROBE_EP=v4|v6 (transport family, default v6), PROBE_N=20 (requests)."""
import ipaddress
import json
import os
import subprocess
import sys

BASE = "https://api.cloudflareclient.com/v0i1909051800"
MODEL = "deepseek-v4.1-flash-free"
BODY = json.dumps({
    "model": MODEL,
    "messages": [{"role": "user", "content": "say ok"}],
    "max_tokens": 8,
}).encode()


def curl(args, data=None):
    cmd = ["curl", "-s", "-m", "30"] + args
    r = subprocess.run(cmd, input=data, capture_output=True)
    return r.stdout


def reg():
    priv = subprocess.run(["wg", "genkey"], capture_output=True, text=True).stdout.strip()
    pub = subprocess.run(["wg", "pubkey"], input=priv, capture_output=True, text=True).stdout.strip()
    payload = json.dumps({
        "key": pub, "install_id": "", "fcm_token": "",
        "tos": "2021-09-14T17:00:00.000+00:00", "model": "PC",
        "serial_number": "", "locale": "en_US",
    }).encode()
    hdr = ["-H", "Content-Type: application/json", "-H", "User-Agent: okhttp/3.12.1",
           "-H", "CF-Client-Version: a-6.30-2158"]
    created = json.loads(curl(hdr + ["-X", "POST", BASE + "/reg", "--data-binary", "@-"], payload))
    r = created["result"]
    did, token = r["id"], r["token"]
    auth = ["-H", "Content-Type: application/json", "-H", "CF-Client-Version: a-6.30-2158",
            "-H", "Authorization: Bearer " + token]
    curl(auth + ["-X", "PATCH", BASE + "/reg/" + did, "--data-binary", "@-"], b'{"warp_enabled":true}')
    cur = json.loads(curl(auth + [BASE + "/reg/" + did]))
    return priv, cur["result"]["config"]


def main():
    priv, cfg = reg()
    peer = cfg["peers"][0]
    ep = peer["endpoint"]
    prefer = os.environ.get("PROBE_EP", "v6")
    raw = ep.get("v4") if prefer == "v4" else (ep.get("host") or ep.get("v6"))
    host = raw.rsplit(":", 1)[0].strip("[]")
    port = peer["endpoint"].get("ports", [2408])[0] if ":" not in raw.rsplit(":", 1)[-1] else 2408
    v6 = cfg["interface"]["addresses"]["v6"]
    n = int(os.environ.get("PROBE_N", "20"))
    addr_line = f"Address = {v6}/128\n"
    if prefer == "v4":
        addr_line += f"Address = {cfg['interface']['addresses']['v4']}/32\n"
    endpoint = f"[{host}]:{port}" if ":" in host else f"{host}:{port}"
    conf = ("[Interface]\nPrivateKey = " + priv + "\n" + addr_line +
            "MTU = 1280\n\n[Peer]\nPublicKey = " + peer["public_key"] +
            "\nAllowedIPs = ::/0\nEndpoint = " + endpoint + "\nPersistentKeepalive = 25\n")
    open("/tmp/wgtest.conf", "w").write(conf)
    subprocess.run(["sudo", "wg-quick", "down", "/tmp/wgtest.conf"], capture_output=True)
    up = subprocess.run(["sudo", "wg-quick", "up", "/tmp/wgtest.conf"], capture_output=True, text=True)
    print("endpoint:", endpoint, "| wg-quick rc:", up.returncode, up.stderr.strip()[:160])
    if up.returncode != 0:
        sys.exit(1)
    try:
        egress = subprocess.run(["curl", "-6", "-s", "-m", "20", "https://api64.ipify.org"],
                                capture_output=True, text=True).stdout.strip()
        fam = ""
        try:
            fam = str(ipaddress.ip_network(egress + "/48", strict=False))
        except Exception:
            pass
        print("warp egress:", egress, "| family:", fam)
        ok = first429 = 0
        for i in range(1, n + 1):
            r = subprocess.run(["curl", "-6", "-s", "-m", "45", "-o", "/dev/null", "-w", "%{http_code}",
                                "-X", "POST", "https://opencode.ai/inference/v1/chat/completions",
                                "-H", "Content-Type: application/json",
                                "-H", "User-Agent: opencode/latest/2.1.1/cli",
                                "-H", "x-opencode-client: cli",
                                "--data-binary", "@-"], input=BODY, capture_output=True)
            code = r.stdout.decode().strip() or "000"
            if code == "200":
                ok += 1
            elif code == "429" and not first429:
                first429 = i
            print(code, end=" ", flush=True)
        print(f"\nRESULT ok={ok}/{n} first_429_at={first429}")
    finally:
        subprocess.run(["sudo", "wg-quick", "down", "/tmp/wgtest.conf"], capture_output=True)


if __name__ == "__main__":
    main()

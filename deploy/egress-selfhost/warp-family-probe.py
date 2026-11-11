#!/usr/bin/env python3
"""Register WARP devices through different Cloudflare API bases/device types and
report the assigned client IPv6 family (the /48, which is what matters here)."""
import ipaddress
import json
import subprocess
import sys
import time
import urllib.request

BASES = [
    "https://api.cloudflareclient.com/v0i1909051800",
    "https://api.cloudflareclient.com/v0a1922",
    "https://api.cloudflareclient.com/v0a2158",
]
MODELS = ["PC", "Android", "iOS"]


def keypair():
    priv = subprocess.run(["wg", "genkey"], capture_output=True, check=True).stdout
    pub = subprocess.run(["wg", "pubkey"], input=priv, capture_output=True, check=True).stdout
    return priv.decode().strip(), pub.decode().strip()


def post(url, payload, headers=None, method="POST"):
    data = json.dumps(payload).encode()
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    req.add_header("CF-Client-Version", "a-6.30-2158")
    req.add_header("User-Agent", "okhttp/3.12.1")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read().decode())


def register(base, model):
    _, pub = keypair()
    body = {
        "key": pub,
        "install_id": "",
        "fcm_token": "",
        "tos": "2021-09-14T17:00:00.000+00:00",
        "model": model,
        "serial_number": "",
        "locale": "en_US",
    }
    created = post(base + "/reg", body)
    res = created["result"]
    did, token = res["id"], res["token"]
    post(base + "/reg/" + did, {"warp_enabled": True}, {"Authorization": "Bearer " + token}, method="PATCH")
    cur = post(base + "/reg/" + did, None, {"Authorization": "Bearer " + token}, method="GET")
    addrs = cur["result"]["config"]["interface"]["addresses"]
    return addrs.get("v6"), addrs.get("v4")


if __name__ == "__main__":
    print("base | model | v6 | /48")
    for base in BASES:
        for model in MODELS:
            try:
                v6, v4 = register(base, model)
                net = ipaddress.ip_network(v6 + "/48", strict=False)
                print(f"{base.split('/')[-1]} | {model:8s} | {v6} | {net}")
            except Exception as e:
                print(f"{base.split('/')[-1]} | {model:8s} | ERROR {type(e).__name__}: {str(e)[:80]}")
            time.sleep(4)

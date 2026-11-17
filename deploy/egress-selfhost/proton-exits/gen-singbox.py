#!/usr/bin/env python3
"""Generate a Proton WireGuard exit layer for o2a v2.

Reads standard Proton `.conf` files (one WireGuard tunnel each) and writes:
  * proton-singbox.json — one userspace WireGuard endpoint + one SOCKS inbound
    per config, so no kernel interfaces, no ip rules and no per-UID setup;
  * proton.txt         — an o2a proxyfile, one `socks5h://127.0.0.1:PORT`
    per exit, ready for the gateway's `proxyfile` setting.

Usage:
  python3 gen-singbox.py --configs /opt/o2a-v2/egress/proton \
      --out-dir /opt/o2a-v2/egress --base-port 23101

Then load it with `o2a-proton-exits.service` and point config.json at
`"proxyfile": "/opt/o2a-v2/egress/proton.txt"`.
"""

import argparse
import glob
import json
import os
import re
import sys


def parse_conf(path):
    text = open(path, encoding="utf-8").read()
    fields = {}
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, value = line.partition("=")
        fields[key.strip()] = value.strip()
    for required in ("PrivateKey", "Address", "PublicKey", "Endpoint"):
        if required not in fields:
            raise SystemExit(f"{path}: missing {required}")
    host, _, port = fields["Endpoint"].rpartition(":")
    if not host or not port.isdigit():
        raise SystemExit(f"{path}: bad Endpoint {fields['Endpoint']!r}")
    return {
        "address": fields["Address"].split(",")[0].strip(),
        "private_key": fields["PrivateKey"],
        "peer_host": host,
        "peer_port": int(port),
        "peer_key": fields["PublicKey"],
        "name": os.path.splitext(os.path.basename(path))[0],
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--configs", default="/opt/o2a-v2/egress/proton",
                    help="directory holding Proton *.conf files")
    ap.add_argument("--out-dir", default="/opt/o2a-v2/egress")
    ap.add_argument("--base-port", type=int, default=23101)
    ap.add_argument("--mtu", type=int, default=1320)
    ap.add_argument("--name", default="proton-singbox.json")
    ap.add_argument("--proxyfile", default="proton.txt")
    args = ap.parse_args()

    paths = sorted(glob.glob(os.path.join(args.configs, "*.conf")))
    if not paths:
        raise SystemExit(f"no *.conf under {args.configs}")
    if args.base_port + len(paths) - 1 > 65535:
        raise SystemExit("base-port leaves no room")

    endpoints, inbounds, rules, out_lines = [], [], [], []
    for index, path in enumerate(paths):
        conf = parse_conf(path)
        tag = f"wg-{index}"
        port = args.base_port + index
        endpoints.append({
            "type": "wireguard",
            "tag": tag,
            "address": [conf["address"]],
            "private_key": conf["private_key"],
            "peers": [{
                "address": conf["peer_host"],
                "port": conf["peer_port"],
                "public_key": conf["peer_key"],
                "allowed_ips": ["0.0.0.0/0", "::/0"],
                "persistent_keepalive_interval": 25,
            }],
            "mtu": args.mtu,
        })
        inbounds.append({
            "type": "socks",
            "tag": f"in-{index}",
            "listen": "127.0.0.1",
            "listen_port": port,
        })
        rules.append({"inbound": [f"in-{index}"], "outbound": tag})
        out_lines.append(f"socks5h://127.0.0.1:{port}")

    config = {
        "log": {"level": "warn", "timestamp": True},
        "inbounds": inbounds,
        "endpoints": endpoints,
        "outbounds": [{"type": "direct", "tag": "direct"}],
        "route": {"rules": rules, "final": "direct"},
    }

    os.makedirs(args.out_dir, exist_ok=True)
    config_path = os.path.join(args.out_dir, args.name)
    with open(config_path, "w", encoding="utf-8") as fh:
        json.dump(config, fh, indent=1)
    proxy_path = os.path.join(args.out_dir, args.proxyfile)
    with open(proxy_path, "w", encoding="utf-8") as fh:
        fh.write("\n".join(out_lines) + "\n")
    os.chmod(config_path, 0o600)

    print(f"exits: {len(paths)}  ports: {args.base_port}-{args.base_port + len(paths) - 1}")
    for index, path in enumerate(paths):
        print(f"  {index + 1:2d}  {os.path.basename(path):24s} -> 127.0.0.1:{args.base_port + index}")
    print(f"wrote {config_path}")
    print(f"wrote {proxy_path}")


if __name__ == "__main__":
    sys.exit(main())

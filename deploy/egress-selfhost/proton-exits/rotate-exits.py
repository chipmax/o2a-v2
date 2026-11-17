#!/usr/bin/env python3
"""Re-point the Proton exit layer at a new set of countries.

The account's WireGuard keys are server-agnostic: one key works on any Proton
server, and the server's public key + hostname come from the account's server
list. So a fixed set of N keys can be mapped to N *new* servers at any time,
which gives the gateway a brand-new set of egress IPs without a new
certificate (Proton caps certificate creation per hour).

  route: keys[i] -> server for countries[i % len(countries)]

Usage:
  python3 rotate-exits.py --countries AU,BR,IN,UK,KR,TW
  python3 rotate-exits.py --countries AU,BR --dry-run

Reads  /opt/o2a-v2/egress/proton-keys.json   (private keys + address)
       /opt/o2a-v2/egress/proton-servers.json (server list snapshot)
Writes /opt/o2a-v2/egress/proton-singbox.json + proton.txt, then restarts
o2a-proton-exits.service unless --no-restart is given.
"""

import argparse
import glob
import json
import os
import re
import subprocess
import sys

BASE = "/opt/o2a-v2/egress"
KEYS_FILE = os.path.join(BASE, "proton-keys.json")
SERVERS_FILE = os.path.join(BASE, "proton-servers.json")
CONFIG_FILE = os.path.join(BASE, "proton-singbox.json")
PROXY_FILE = os.path.join(BASE, "proton.txt")
CONF_DIR = os.path.join(BASE, "proton")
BASE_PORT = 23101


def bootstrap_keys(conf_dir=CONF_DIR):
    """Derive one key per downloaded .conf (Proton keys are server-agnostic)."""
    keys, seen = [], set()
    for path in sorted(glob.glob(os.path.join(conf_dir, "*.conf"))):
        text = open(path, encoding="utf-8").read()
        private = re.search(r"PrivateKey\s*=\s*(\S+)", text)
        address = re.search(r"Address\s*=\s*(\S+)", text)
        if not private or not address:
            continue
        key = private.group(1)
        if key in seen:
            continue
        seen.add(key)
        keys.append({
            "name": os.path.basename(path)[:-5],
            "private_key": key,
            "address": address.group(1).split(",")[0].strip(),
        })
    if not keys:
        raise SystemExit(f"{conf_dir}: no usable *.conf to bootstrap from")
    with open(KEYS_FILE, "w", encoding="utf-8") as fh:
        json.dump(keys, fh, indent=1)
    os.chmod(KEYS_FILE, 0o600)
    print(f"bootstrapped {len(keys)} keys from {conf_dir} -> {KEYS_FILE}")
    return keys


def load_keys():
    if not os.path.exists(KEYS_FILE):
        return bootstrap_keys()
    data = json.load(open(KEYS_FILE))
    if not data:
        raise SystemExit(f"{KEYS_FILE}: no keys")
    return data


def refresh_servers(username, password):
    cmd = ["protonvpn-wg-confgen", "--username", username,
           "--password-stdin", "--list-servers", "--json"]
    proc = subprocess.run(cmd, input=password + "\n", text=True,
                          capture_output=True, check=False)
    if proc.returncode != 0:
        raise SystemExit(f"list-servers failed: {proc.stderr.strip()[:200]}")
    raw = proc.stdout
    start = raw.find("[")
    if start < 0:
        raise SystemExit("list-servers returned no JSON array")
    with open(SERVERS_FILE, "w", encoding="utf-8") as fh:
        fh.write(raw[start:])
    os.chmod(SERVERS_FILE, 0o600)
    print(f"refreshed server list -> {SERVERS_FILE}")


def load_servers(path):
    raw = open(path, encoding="utf-8").read()
    start = raw.find("[")
    if start < 0:
        raise SystemExit(f"{path}: no JSON array")
    return json.loads(raw[start:])


def candidates(servers, country):
    """Online Plus servers of a country, best (lowest load) first."""
    found = []
    for srv in servers:
        if srv.get("country") != country or srv.get("tier") != "Plus":
            continue
        for ep in srv.get("endpoints") or []:
            if not (ep.get("online") and ep.get("public_key") and ep.get("hostname")):
                continue
            found.append((srv, ep))
            break
    found.sort(key=lambda pair: (pair[0].get("load") or 100, pair[0].get("name") or ""))
    return found


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--countries", required=True,
                    help="comma separated Proton country codes, e.g. AU,BR,IN")
    ap.add_argument("--servers", default=SERVERS_FILE)
    ap.add_argument("--keys", default=KEYS_FILE)
    ap.add_argument("--refresh-servers", metavar="USERNAME",
                    help="re-fetch the Proton server list first (password from PROTON_PASSWORD)")
    ap.add_argument("--mtu", type=int, default=1320)
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--no-restart", action="store_true")
    args = ap.parse_args()

    if args.refresh_servers:
        password = os.environ.get("PROTON_PASSWORD")
        if not password:
            raise SystemExit("--refresh-servers needs PROTON_PASSWORD in the environment")
        refresh_servers(args.refresh_servers, password)

    countries = [c.strip().upper() for c in args.countries.split(",") if c.strip()]
    keys = load_keys()
    servers = load_servers(args.servers)

    endpoints, inbounds, rules, proxy_lines, plan = [], [], [], [], []
    used = {}
    for index, entry in enumerate(keys):
        cc = countries[index % len(countries)]
        pool = candidates(servers, cc)
        if not pool:
            raise SystemExit(f"no online Plus server for {cc}")
        # spread the keys over DISTINCT servers: reuse only when the country
        # has fewer servers than keys.
        pool.sort(key=lambda pair: (used.get(pair[0].get("name"), 0), pair[0].get("load") or 100))
        srv, ep = pool[0]
        used[srv.get("name")] = used.get(srv.get("name"), 0) + 1
        tag = f"wg-{index}"
        endpoints.append({
            "type": "wireguard",
            "tag": tag,
            "address": [entry["address"]],
            "private_key": entry["private_key"],
            "peers": [{
                "address": ep["hostname"],
                "port": 51820,
                "public_key": ep["public_key"],
                "allowed_ips": ["0.0.0.0/0", "::/0"],
                "persistent_keepalive_interval": 25,
            }],
            "mtu": args.mtu,
        })
        inbounds.append({"type": "socks", "tag": f"in-{index}",
                         "listen": "127.0.0.1", "listen_port": BASE_PORT + index})
        rules.append({"inbound": [f"in-{index}"], "outbound": tag})
        proxy_lines.append(f"socks5h://127.0.0.1:{BASE_PORT + index}")
        plan.append((index, cc, srv.get("name"), ep.get("hostname"), ep.get("exit_ip")))

    config = {
        "log": {"level": "warn", "timestamp": True},
        "inbounds": inbounds,
        "endpoints": endpoints,
        "outbounds": [{"type": "direct", "tag": "direct"}],
        "route": {"rules": rules, "final": "direct"},
    }

    for index, cc, name, host, exit_ip in plan:
        print(f"{index:2d}  {cc:3s} {name:12s} {host:30s} exit~{exit_ip}  port {BASE_PORT + index}")

    if args.dry_run:
        print("dry run: nothing written")
        return 0

    os.makedirs(BASE, exist_ok=True)
    with open(CONFIG_FILE, "w", encoding="utf-8") as fh:
        json.dump(config, fh, indent=1)
    os.chmod(CONFIG_FILE, 0o600)
    with open(PROXY_FILE, "w", encoding="utf-8") as fh:
        fh.write("\n".join(proxy_lines) + "\n")
    print(f"wrote {CONFIG_FILE} and {PROXY_FILE} ({len(proxy_lines)} exits)")

    if not args.no_restart:
        subprocess.run(["systemctl", "restart", "o2a-proton-exits.service"], check=True)
        print("restarted o2a-proton-exits.service")
    return 0


if __name__ == "__main__":
    sys.exit(main())

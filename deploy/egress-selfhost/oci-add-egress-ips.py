#!/usr/bin/env python3
"""Give VPS2 its own extra egress IPv4 addresses (one quota bucket each).

The OpenCode free tier is bucketed per egress IP and resets at 00:00 UTC, so an
extra public IPv4 is worth about one free-tier allowance per day. OCI lets a
VNC hold several private IPs, each with its own reserved public IP; this tool
creates them through the instance principal and configures them on ens3.

One-time IAM setup (console, as tenancy admin):

    Dynamic group: o2a-egress
      matching rule: instance.id = 'ocid1.instance.oc1.ap-singapore-1.<...>'

    Policy (tenancy root):
      Allow dynamic-group o2a-egress to manage vnics in tenancy
      Allow dynamic-group o2a-egress to manage private-ips in tenancy
      Allow dynamic-group o2a-egress to manage public-ips in tenancy
      Allow dynamic-group o2a-egress to read subnets in tenancy

Then:

    sudo /tmp/oci-venv/bin/python oci-add-egress-ips.py --count 4 --apply

Billed by OCI: reserved public IPv4 addresses (check the current rate). IPv6
addresses in a second subnet are free but need a second VNIC instead.

Requires the `oci` Python package (python3 -m venv /tmp/oci-venv && pip install oci).
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys

import oci

IMDS = "http://169.254.169.254/opc/v2"
HEADERS = {"Authorization": "Bearer Oracle"}


def imds(path: str) -> object:
    done = subprocess.run(
        ["curl", "-s", "-m", "5", "-H", "Authorization: Bearer Oracle", f"{IMDS}/{path}"],
        capture_output=True,
        text=True,
    )
    return json.loads(done.stdout)


def sh(*args: str) -> subprocess.CompletedProcess:
    return subprocess.run(list(args), capture_output=True, text=True)


def free_tier(interface: str) -> str:
    body = (
        '{"model":"deepseek-v4.1-flash-free",'
        '"messages":[{"role":"user","content":"hi"}],"max_tokens":4}'
    )
    done = sh(
        "curl", "-6" if ":" in interface else "-4", "-s", "--interface", interface,
        "-m", "25", "-o", "/dev/null", "-w", "%{http_code}",
        "-H", "Content-Type: application/json", "-d", body,
        "https://opencode.ai/inference/v1/chat/completions",
    )
    return (done.stdout or "").strip()


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--count", type=int, default=4)
    ap.add_argument("--apply", action="store_true", help="create them (default: dry run)")
    ap.add_argument("--test", action="store_true", help="test egress and free tier after adding")
    args = ap.parse_args()

    instance = imds("instance/")
    vnics = imds("vnics/")
    if not isinstance(vnics, list) or not vnics:
        print("no vnic from IMDS", file=sys.stderr)
        return 1
    vnic_id = vnics[0]["vnicId"]
    compartment = instance["compartmentId"]

    signer = oci.auth.signers.InstancePrincipalsSecurityTokenSigner()
    net = oci.core.VirtualNetworkClient({}, signer=signer)
    try:
        vnic = net.get_vnic(vnic_id).data
    except oci.exceptions.ServiceError as exc:
        if exc.status == 404:
            print(
                "Instance principal is not authorized yet (NotAuthorizedOrNotFound).\n"
                "Add the dynamic group + policy from the README, wait ~1 minute, retry.",
                file=sys.stderr,
            )
            return 2
        raise
    subnet_id = vnic.subnet_id
    subnet = net.get_subnet(subnet_id).data
    prefix = subnet.cidr_block.split("/")[1]
    print(f"vnic={vnic_id} subnet={subnet.cidr_block} prefix={prefix} compartment={compartment}")

    if not args.apply:
        print("dry run: pass --apply to create", args.count, "public IPv4 addresses")
        return 0

    created = []
    for i in range(args.count):
        label = f"o2a-egress-{i + 1}"
        private = net.create_private_ip(
            oci.core.models.CreatePrivateIpDetails(
                subnet_id=subnet_id,
                vnic_id=vnic_id,
                display_name=label,
            )
        ).data
        public = net.create_public_ip(
            oci.core.models.CreatePublicIpDetails(
                compartment_id=compartment,
                lifetime="RESERVED",
                private_ip_id=private.id,
                display_name=label,
            )
        ).data
        created.append({"private_ip": private.ip_address, "public_ip": public.ip_address})
        print(f"created {label}: {private.ip_address} -> {public.ip_address}")

    iface = "ens3"
    for item in created:
        sh("ip", "addr", "add", f"{item['private_ip']}/{prefix}", "dev", iface)
        print(f"configured {item['private_ip']}/{prefix} on {iface}")

    if args.test:
        for item in created:
            egress = sh("curl", "-4", "-s", "-m", "8", "--interface", item["public_ip"], "https://api.ipify.org")
            status = free_tier(item["public_ip"])
            print(
                json.dumps(
                    {
                        "public_ip": item["public_ip"],
                        "egress": (egress.stdout or "").strip(),
                        "free_tier": status,
                    }
                )
            )
    return 0


if __name__ == "__main__":
    sys.exit(main())

# egress-sync: public subscription lane for o2a

Pulls already-tested node lists, dedupes by real server, renders one sing-box
SOCKS inbound per exit, probes each exit, and publishes the survivors as an o2a
`proxyfile`.

```text
LeilaoMi verified.json ─┐
snakem982 sing-box.json ─┴─► dedupe by server ─► sing-box 22001.. ─► probe ─► egress.txt
```

## Install (root)

```bash
install -d /opt/o2a-v2/egress /opt/o2a-v2/egress-sync
install -m 0755 egress-sync.py /opt/o2a-v2/egress-sync/egress-sync.py
install -m 0644 o2a-egress-singbox.service o2a-egress-sync.service o2a-egress-sync.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now o2a-egress-singbox.service
systemctl enable --now o2a-egress-sync.timer
```

The unit expects the sing-box binary at `/opt/zen-gate/data/bin/sing-box`;
change `ExecStart` when it lives elsewhere.

Point o2a at the generated list (config.json):

```json
"proxies": [],
"proxyfile": "/opt/o2a-v2/egress/egress.txt"
```

Keep `warp.enabled=false`. Reload o2a (`POST /api/config/reload` or restart) after
a new `egress.txt` is written.

## Probe meaning

`ok` requires both an egress IP from `api.ipify.org` and an HTTP 200/30x from
`https://opencode.ai/` through the exit. It does not spend free-tier quota, and
that is also its limitation: a reachable exit can still be quota-spent.

## Measured verdict (2026-11-10, VPS2)

A 24-exit pool passed the connectivity probe (24 unique egress IPs) and then
answered **0/380** free-tier requests; every pinned exit answered 429 or 503.
Three fresh exits from the `global` list answered 429 too. Public subscription
IPs are widely used against the same free tier, so most are already spent.

Do not enable this lane in production until the missing admission step exists:
one pinned admin diagnostic (`POST /api/debug/inference` with `relay.name`)
whose `http_status` must be 200 before an exit is kept.

## Notes

- Public exits are untrusted: use them for free-tier traffic only, never for
  paid keys or sensitive data.
- 24 kept exits ≈ 24 independent daily buckets, but public nodes die often;
  the 6-hour refresh is deliberate.
- Do not raise `--limit` far above 60: probing 27k candidates on a 1 GB VPS is
  slower than fetching the aggregator's verified list.

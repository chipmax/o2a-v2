#!/usr/bin/env python3
"""How much does Kilo's anonymous free tier give one address? Round-robins the
free chat models and reports status per request until an address-wide 429."""
import json
import time
import urllib.error
import urllib.request

BASE = "https://api.kilo.ai/api/gateway"
SKIP = ("safety", "guard", "moderation")


def free_ids():
    with urllib.request.urlopen(BASE + "/models", timeout=30) as r:
        d = json.load(r)
    lst = d.get("data") or d.get("models") or []
    return [m["id"] for m in lst if m.get("isFree") and not any(k in m["id"] for k in SKIP)]


def chat(mid):
    body = json.dumps({"model": mid, "messages": [{"role": "user", "content": "say ok"}],
                       "max_tokens": 8, "stream": False}).encode()
    req = urllib.request.Request(BASE + "/chat/completions", data=body, method="POST")
    req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            r.read(80)
            return r.status, ""
    except urllib.error.HTTPError as e:
        return e.code, e.read(160).decode("utf-8", "replace").replace("\n", " ")
    except Exception as e:
        return 0, type(e).__name__


ids = free_ids()
print("free models:", len(ids))
ok = bad = 0
for i in range(18):
    mid = ids[i % len(ids)]
    code, note = chat(mid)
    ok += code == 200
    bad += code != 200
    print(f"{i+1:2d} {mid:45s} {code} {note[:90]}")
    if code == 429 and "PROMOTION_MODEL_LIMIT_REACHED" in note:
        print("=> address-wide limit hit")
        break
    time.sleep(3)
print(f"RESULT ok={ok} bad={bad}")

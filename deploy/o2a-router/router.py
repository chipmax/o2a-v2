import json, os, sys, http.client, time, threading, urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

CFG_PATH = os.environ.get("O2A_ROUTER_CONFIG", "/opt/o2a-router/config.json")
CFG = json.load(open(CFG_PATH))
UPSTREAMS = CFG["upstreams"]
LOCAL_KEYS = set(CFG.get("keys") or [])
DEFAULT_COOLDOWN = int(CFG.get("cooldown_seconds", 60))
MAX_COOLDOWN = int(CFG.get("max_cooldown_seconds", 900))
FALLBACK_CODES = {401, 403, 429, 500, 502, 503, 504}
SHORT_CODES = {401, 403}
SOFT_CODES = {400, 404}
SOFT_MARKERS = ("not available", "not found", "not registered", "unknown model",
                "does not exist", "unsupported model", "no such model")
KILO_MODELS = set(CFG.get("kilo_models") or [])
KILO_UPSTREAMS = set(CFG.get("kilo_upstreams") or [])
KILO_CATALOG = CFG.get("kilo_catalog") or []
KILO_CATALOG_URL = CFG.get("kilo_catalog_url", "https://ai-gateway.kilo.ai/api/v1/models")
KILO_REFRESH_SECONDS = int(CFG.get("kilo_catalog_refresh_seconds", 21600))
UI_BASE = CFG.get("ui_base", "http://127.0.0.1:8092")
UI_TIMEOUT = int(CFG.get("ui_timeout", 60))
HOP = {"transfer-encoding", "connection", "content-length", "keep-alive", "proxy-connection"}
COOL = {}
LAST_429 = {}
RR = 0
RR_ON = bool(CFG.get("round_robin", False))
LOCK = threading.Lock()

def log(msg):
    print(msg, flush=True)

def cooldown_for(up):
    return int(up.get("cooldown_seconds", DEFAULT_COOLDOWN))

def request_model(body):
    if not body:
        return ""
    try:
        obj = json.loads(body)
    except Exception:
        return ""
    if isinstance(obj, dict):
        return str(obj.get("model") or "")
    return ""

def ordered():
    global RR
    now = time.time()
    with LOCK:
        hot = [u for u in UPSTREAMS if COOL.get(u["name"], 0) <= now]
        if RR_ON and len(hot) > 1:
            top = min(u.get("tier", 1) for u in hot)
            primary = [u for u in hot if u.get("tier", 1) == top]
            rest = [u for u in hot if u.get("tier", 1) != top]
            if len(primary) > 1:
                i = RR % len(primary)
                primary = primary[i:] + primary[:i]
                RR += 1
            hot = primary + rest
    return hot

def is_soft_model_error(st, body):
    if st not in SOFT_CODES or not body:
        return False
    text = body.decode("utf-8", "ignore").lower()
    return any(m in text for m in SOFT_MARKERS)

def cool_down(name, seconds):
    with LOCK:
        COOL[name] = time.time() + max(1, seconds)


def refresh_kilo_catalog():
    """Refresh the free Kilo catalog from the upstream endpoint.

    The configured list is only a seed: Kilo retires and adds :free models often.
    A failed refresh keeps the seed so a transient network error never empties
    the routing table. Only pricing==0 models become routable here.
    """
    global KILO_MODELS, KILO_CATALOG
    if not KILO_UPSTREAMS:
        return
    try:
        with urllib.request.urlopen(KILO_CATALOG_URL, timeout=15) as response:
            payload = json.loads(response.read().decode("utf-8"))
        data = payload.get("data") or []
        models, catalog = [], []
        for item in data:
            if not isinstance(item, dict):
                continue
            mid = str(item.get("id") or "")
            pricing = item.get("pricing") or {}
            free = all(str(pricing.get(field, "")) in ("0", "0.0", "") for field in ("prompt", "completion"))
            if not mid or not free or not (mid.endswith(":free") or mid.endswith("/free")):
                continue
            top = item.get("top_provider") or {}
            # P21: Kilo's own docs warn that kilo-auto may route to providers
            # that log prompts. Make that visible in /v1/models.
            display = item.get("name") or mid
            if mid == "kilo-auto/free" and "data-sharing" not in display.lower():
                display = display + " (data-sharing)"
            entry = {
                "id": mid,
                "name": display,
                "context_length": int(top.get("context_length") or 200000),
                "max_completion_tokens": int(top.get("max_completion_tokens") or 32768),
            }
            models.append(mid)
            catalog.append(entry)
        if not models:
            log("kilo catalog refresh returned no free models; keeping seed")
            return
        with LOCK:
            KILO_MODELS = set(models)
            KILO_CATALOG = catalog
        log(f"kilo catalog refreshed: {len(models)} free models from {KILO_CATALOG_URL}")
    except Exception as exc:
        log(f"kilo catalog refresh failed: {exc}; keeping seed")


def catalog_refresh_loop():
    while True:
        time.sleep(max(60, KILO_REFRESH_SECONDS))
        refresh_kilo_catalog()


def merge_kilo_models(blob):
    if not KILO_CATALOG:
        return blob
    try:
        obj = json.loads(blob)
    except Exception:
        return blob
    if not isinstance(obj, dict):
        return blob
    data = obj.get("data")
    if not isinstance(data, list):
        return blob
    seen = {m.get("id") for m in data if isinstance(m, dict)}
    for item in KILO_CATALOG:
        if not isinstance(item, dict) or item.get("id") in seen:
            continue
        entry = dict(item)
        entry.setdefault("object", "model")
        entry.setdefault("created", 0)
        entry.setdefault("owned_by", "kilo")
        data.append(entry)
    obj["data"] = data
    return json.dumps(obj).encode()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, fmt, *args):
        log("%s %s" % (self.address_string(), fmt % args))

    def do_GET(self): self.route()
    def do_POST(self): self.route()
    def do_PUT(self): self.route()
    def do_PATCH(self): self.route()
    def do_DELETE(self): self.route()
    def do_HEAD(self): self.route()

    def route(self):
        path = urlsplit(self.path).path
        self.kilo_merge = (path == "/v1/models" and self.command == "GET")
        if path == "/healthz" or path == "/v1" or path.startswith("/v1/"):
            return self.proxy()
        return self.webui()

    def read_body(self):
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n > 0 else b""

    def webui(self):
        body = self.read_body()
        u = urlsplit(UI_BASE)
        conn = http.client.HTTPConnection(u.hostname, u.port or 80, timeout=UI_TIMEOUT)
        forward = {}
        for k, v in self.headers.items():
            lk = k.lower()
            if lk in HOP:
                continue
            forward[k] = v
        if body:
            forward["Content-Length"] = str(len(body))
        try:
            conn.request(self.command, self.path, body=body if body else None, headers=forward)
            resp = conn.getresponse()
        except Exception as e:
            try: conn.close()
            except Exception: pass
            log(f"webui proxy failed: {type(e).__name__}: {e}")
            return self.send_json(502, {"error": {"message": f"webui proxy failed: {type(e).__name__}", "type": "router_error"}})
        self.send_response(resp.status)
        for k, v in resp.getheaders():
            if k.lower() in HOP:
                continue
            self.send_header(k, v)
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True
        if self.command == "HEAD":
            try: resp.close()
            except Exception: pass
            try: conn.close()
            except Exception: pass
            return
        try:
            while True:
                chunk = resp.read(65536)
                if not chunk: break
                self.wfile.write(chunk)
                self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass
        finally:
            try: resp.close()
            except Exception: pass
            try: conn.close()
            except Exception: pass

    def proxy(self):
        auth = self.headers.get("Authorization", "")
        token = auth[7:] if auth.startswith("Bearer ") else ""
        if LOCAL_KEYS and token not in LOCAL_KEYS:
            self.send_json(401, {"error": {"message": "invalid router key", "type": "auth_error"}})
            return
        body = self.read_body()
        errors = []
        self.retry_reply = None
        for round_no in (0, 1):
            done = self.round(body, errors)
            if done: return
            if round_no == 0:
                wait = self.next_wait()
                if wait is not None and wait <= 12:
                    log(f"all upstreams cooling, wait {wait:.1f}s then retry")
                    time.sleep(wait)
                    continue
            break
        if self.retry_reply:
            self.reply_named("none", *self.retry_reply)
            return
        with LOCK:
            last_429 = None
            for up in UPSTREAMS:
                item = LAST_429.get(up["name"])
                if item and (last_429 is None or item[3] > last_429[3]):
                    last_429 = item
            wait = min(
                (COOL[u["name"]] - time.time() for u in UPSTREAMS if COOL.get(u["name"], 0) > time.time()),
                default=None,
            )
        if last_429:
            self.reply_named("none", *last_429[:3])
            return
        if wait is not None:
            data = json.dumps({
                "error": {"message": "all upstreams cooling down", "type": "router_unavailable"},
            }).encode()
            self.reply_named(
                "none", 503,
                [("Content-Type", "application/json"), ("Retry-After", str(max(1, int(wait + 0.999))))],
                data,
            )
            return
        self.send_json(502, {"error": {"message": "all upstreams failed: " + "; ".join(errors), "type": "router_error"}})

    def next_wait(self):
        now = time.time()
        waits = [COOL[u["name"]] - now for u in UPSTREAMS if COOL.get(u["name"], 0) > now]
        return min(waits) if waits else None

    def round(self, body, errors):
        soft = None
        model = request_model(body)
        chat_path = urlsplit(self.path).path.endswith("/chat/completions")
        only = KILO_UPSTREAMS if (model and model in KILO_MODELS and chat_path) else None
        for up in ordered():
            if only is not None and up["name"] not in only:
                continue
            if only is None and up["name"] in KILO_UPSTREAMS:
                continue
            try:
                st, hdrs, resp = self.try_upstream(up, body)
            except Exception as e:
                errors.append(f"{up['name']}: {type(e).__name__}")
                log(f"upstream {up['name']} error {type(e).__name__}: {e}")
                continue
            if st in SOFT_CODES:
                blob = b""
                try:
                    blob = resp.read(65536)
                except Exception:
                    pass
                finally:
                    try: resp.close()
                    except Exception: pass
                if is_soft_model_error(st, blob):
                    errors.append(f"{up['name']}: {st}(model)")
                    log(f"upstream {up['name']} -> {st} model-not-available, fallback")
                    soft = soft or (st, hdrs, blob)
                    continue
                self.reply_named(up["name"], st, hdrs, blob)
                return True
            if st in FALLBACK_CODES:
                blob = b""
                try:
                    if st == 429:
                        blob = resp.read(65536)
                        with LOCK:
                            LAST_429[up["name"]] = (st, hdrs, blob, time.time())
                        self.retry_reply = (st, hdrs, blob)
                    else:
                        resp.read()
                        with LOCK:
                            LAST_429.pop(up["name"], None)
                except Exception:
                    pass
                finally:
                    try: resp.close()
                    except Exception: pass
                cd = 15 if st in SHORT_CODES else cooldown_for(up)
                ra = ""
                for k, v in hdrs:
                    if k.lower() == "retry-after":
                        ra = v.strip(); break
                if ra.isdigit() and st not in SHORT_CODES:
                    cap = int(up.get("max_cooldown_seconds", MAX_COOLDOWN))
                    cd = min(max(int(ra), 5), cap, MAX_COOLDOWN)
                cool_down(up["name"], cd)
                errors.append(f"{up['name']}: {st}")
                log(f"upstream {up['name']} -> {st}, cooldown {cd}s, fallback")
                continue
            with LOCK:
                COOL.pop(up["name"], None)
                LAST_429.pop(up["name"], None)
            self.reply(up, st, hdrs, resp)
            return True
        if soft:
            self.reply_named("none", soft[0], soft[1], soft[2])
            return True
        return False

    def reply(self, up, st, hdrs, resp):
        if getattr(self, "kilo_merge", False) and st == 200:
            blob = b""
            try:
                blob = resp.read()
            finally:
                try: resp.close()
                except Exception: pass
            self.reply_named(up["name"], st, hdrs, merge_kilo_models(blob))
            return
        self.send_response(st)
        skip = {"transfer-encoding", "connection", "content-length", "keep-alive"}
        for k, v in hdrs:
            if k.lower() in skip: continue
            self.send_header(k, v)
        self.send_header("X-Router-Upstream", up["name"])
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True
        try:
            while True:
                chunk = resp.read(8192)
                if not chunk: break
                self.wfile.write(chunk)
                self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass
        finally:
            try: resp.close()
            except Exception: pass
        log(f"served by {up['name']} status={st}")

    def reply_named(self, name, st, hdrs, blob):
        if getattr(self, "kilo_merge", False) and st == 200:
            blob = merge_kilo_models(blob)
        self.send_response(st)
        for k, v in hdrs:
            if k.lower() in ("transfer-encoding", "connection", "content-length", "keep-alive"): continue
            self.send_header(k, v)
        self.send_header("X-Router-Upstream", name)
        self.send_header("Content-Length", str(len(blob)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(blob)
        self.close_connection = True
        log(f"served by {name} status={st}")

    def try_upstream(self, up, body):
        u = urlsplit(up["base"])
        tls = u.scheme == "https"
        port = u.port or (443 if tls else 80)
        cls = http.client.HTTPSConnection if tls else http.client.HTTPConnection
        conn = cls(u.hostname, port, timeout=up.get("timeout", 120))
        headers = {}
        for k, v in self.headers.items():
            lk = k.lower()
            if lk in ("content-type", "accept", "user-agent", "x-session-id",
                      "x-opencode-session", "session_id", "conversation-id",
                      "conversation_id", "accept-encoding"):
                headers[k] = v
        if up.get("key"):
            headers["Authorization"] = "Bearer " + up["key"]
        headers["Content-Length"] = str(len(body))
        req_path = self.path
        if up.get("strip_v1") and req_path.startswith("/v1/"):
            req_path = req_path[3:]
        if u.path:
            req_path = u.path.rstrip("/") + req_path
        try:
            conn.request(self.command, req_path, body=body if body else None, headers=headers)
            resp = conn.getresponse()
        except Exception:
            try: conn.close()
            except Exception: pass
            raise
        return resp.status, resp.getheaders(), resp

    def send_json(self, code, obj):
        data = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(data)
        self.close_connection = True

class Server(ThreadingHTTPServer):
    daemon_threads = True
    request_queue_size = 128

if __name__ == "__main__":
    refresh_kilo_catalog()
    threading.Thread(target=catalog_refresh_loop, daemon=True).start()
    srv = Server(("0.0.0.0", int(CFG.get("listen_port", 8090))), Handler)
    log(f"o2a-router listening on :{CFG.get('listen_port', 8090)} upstreams={[u['name'] for u in UPSTREAMS]} cooldowns={[(u['name'], cooldown_for(u)) for u in UPSTREAMS]} kilo={sorted(KILO_MODELS)} ui={UI_BASE}")
    srv.serve_forever()
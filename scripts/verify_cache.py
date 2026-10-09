#!/usr/bin/env python3
"""Verify the shared-state layer of a running AniMind stack.

What the Go suites cannot prove from inside one process: that a resolved credential actually
lands in Redis and is deleted when the session is revoked, that the AniList relay answers its
second identical query without leaving the container, that rate-limit counters live in Redis with
a TTL, and that the Prometheus target, the Grafana dashboard and the nginx port rules all line up
at the same moment.

    python3 scripts/verify_cache.py [--base http://localhost:8080] [--container animind-api]
                                    [--redis-container animind-redis]
                                    [--prometheus http://localhost:9090]
                                    [--grafana http://localhost:3000]

--base should be the nginx origin, not the API port: part of what this checks is that nginx does
not forward /metrics. The Redis and container inspections shell out to `docker exec`, so run this
on the host that owns the stack. Without Redis configured, pass --no-redis and the Redis checks
report as skipped rather than failed.
"""

import argparse
import hashlib
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request
import uuid

parser = argparse.ArgumentParser(description="verify Redis and metrics against a running stack")
parser.add_argument("--base", default=os.environ.get("E2E_BASE", "http://localhost:8080"))
parser.add_argument("--container", default=os.environ.get("E2E_CONTAINER", "animind-api"))
parser.add_argument("--redis-container", default=os.environ.get("E2E_REDIS_CONTAINER", "animind-redis"))
parser.add_argument("--namespace", default=os.environ.get("CACHE_NAMESPACE", "animind"))
parser.add_argument("--prometheus", default=os.environ.get("E2E_PROMETHEUS", "http://localhost:9090"))
parser.add_argument("--grafana", default=os.environ.get("E2E_GRAFANA", "http://localhost:3000"))
parser.add_argument("--grafana-user", default=os.environ.get("GRAFANA_USER", "admin"))
parser.add_argument("--grafana-password", default=os.environ.get("GRAFANA_PASSWORD", "admin"))
parser.add_argument("--no-redis", action="store_true", help="stack runs without REDIS_URL")
args, _ = parser.parse_known_args()

BASE = args.base.rstrip("/")
CONTAINER = args.container
REDIS = args.redis_container
NS = args.namespace
results, skipped = [], []


def record(label, ok, detail=""):
    results.append((label, ok))
    print(("  PASS  " if ok else "  FAIL  ") + label + (("  | " + str(detail)[:220]) if detail else ""))


def skip(label, why):
    skipped.append(label)
    print("  SKIP  " + label + "  | " + why)


def http(method, url, body=None, token=None, headers=None, timeout=30):
    req = urllib.request.Request(url, method=method)
    req.add_header("Accept", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    data = json.dumps(body).encode() if body is not None else None
    if data is not None:
        req.add_header("Content-Type", "application/json")
    for key, value in (headers or {}).items():
        req.add_header(key, value)
    try:
        with urllib.request.urlopen(req, data, timeout=timeout) as response:
            return response.status, response.read(), dict(response.headers)
    except urllib.error.HTTPError as error:
        return error.code, error.read(), dict(error.headers)
    except Exception as error:  # connection refused is a finding, not a crash
        return 0, str(error).encode(), {}


def api(method, path, body=None, token=None, headers=None):
    status, raw, hdrs = http(method, BASE + path, body, token, headers)
    try:
        return status, json.loads(raw or b"{}"), hdrs
    except Exception:
        return status, raw.decode("utf-8", "replace"), hdrs


def get_json(url, headers=None):
    """Fetch an absolute URL and hand back parsed JSON, so a dict check cannot pass on bytes."""
    status, raw, _ = http("GET", url, headers=headers)
    try:
        return status, json.loads(raw or b"{}")
    except Exception:
        return status, raw if isinstance(raw, str) else raw.decode("utf-8", "replace")


def exec_in(container, command):
    try:
        out = subprocess.run(["docker", "exec", container] + command,
                             capture_output=True, text=True, timeout=30)
        return out.returncode, out.stdout.strip(), out.stderr.strip()
    except FileNotFoundError:
        return -1, "", "docker is not on PATH"
    except Exception as error:
        return -1, "", str(error)


def redis_cli(*arguments):
    return exec_in(REDIS, ["redis-cli"] + list(arguments))


def metrics_text():
    """The metrics listener is not published, so read it from inside the API container."""
    code, out, err = exec_in(CONTAINER, ["wget", "-qO-", "http://127.0.0.1:3002/metrics"])
    if code != 0:
        return "", err or out
    return out, ""


def metric_value(text, name, labels=""):
    """Last sample value for `name{labels}`, or None."""
    found = None
    needle = name + labels
    for line in text.splitlines():
        if line.startswith("#") or not line.strip():
            continue
        head, _, value = line.partition(" ")
        if head == needle or head == needle + "{}":
            try:
                found = float(value)
            except ValueError:
                pass
    return found


print("== AniMind shared-state verification against " + BASE + " ==\n")

# --- 1. The stack answers, and the proxy is not leaking the metrics port ------------------
status, body, _ = api("GET", "/health")
record("nginx reaches the API", status == 200, body if status != 200 else "")

status, _, _ = api("GET", "/metrics")
record("/metrics is not proxied through nginx", status == 404, "got " + str(status))

# --- 2. Credentials really are resolved through the store --------------------------------
email = "cache-" + uuid.uuid4().hex[:10] + "@example.com"
password = "correct horse battery"
status, signup, _ = api("POST", "/api/auth/signup", {"email": email, "password": password, "username": "cachecheck"})
token = signup.get("access_token", "") if isinstance(signup, dict) else ""
record("signup returns a session token", bool(token.startswith("as_")), str(signup)[:120])

before_text, err = metrics_text()
if not before_text:
    record("metrics listener answers inside the API container", False, err)
else:
    record("metrics listener answers inside the API container", True, "")

misses_before = metric_value(before_text, "animind_cache_operations_total", '{backend="redis",result="miss"}')
hits_before = metric_value(before_text, "animind_cache_operations_total", '{backend="redis",result="hit"}')

status, _, _ = api("GET", "/api/auth/me", token=token)
record("first authenticated request succeeds", status == 200, "got " + str(status))
status, second, _ = api("GET", "/api/auth/me", token=token)
record("second authenticated request succeeds", status == 200, "got " + str(status))

if args.no_redis:
    skip("Redis holds the credential", "--no-redis was passed")
else:
    # The key is derived, not discovered: the API caches under the same SHA-256 the database
    # stores, so asserting on this exact key proves the derivation and cannot be confused by
    # another run's still-live entries.
    user_id = (signup.get("user") or {}).get("id", "") if isinstance(signup, dict) else ""
    credential_key = NS + ":credential:web:" + hashlib.sha256(token.encode()).hexdigest()
    group_key = NS + ":group:user:" + user_id

    code, exists, err = redis_cli("EXISTS", credential_key)
    record("this session's credential is stored in Redis", exists.strip() == "1",
           credential_key if exists.strip() == "1" else (exists or err))

    code, group_exists, err = redis_cli("EXISTS", group_key)
    record("the account's invalidation group exists", group_exists.strip() == "1",
           group_key if group_exists.strip() == "1" else (group_exists or err))

    code, ttl, err = redis_cli("PTTL", credential_key)
    try:
        ttl_ms = int(ttl)
    except ValueError:
        ttl_ms = -2
    record("the credential entry expires", 0 < ttl_ms <= 61_000, "PTTL=" + ttl + "ms")

    # Logout must delete the entry, not wait for its TTL: this is the security assertion.
    status, _, _ = api("POST", "/api/auth/logout", token=token)
    record("logout succeeds", status in (200, 204), "got " + str(status))
    code, exists, err = redis_cli("EXISTS", credential_key)
    record("logout evicted the cached credential", exists.strip() == "0",
           "key still present: " + credential_key if exists.strip() != "0" else "gone")

    status, _, _ = api("GET", "/api/auth/me", token=token)
    record("the revoked token is refused", status == 401, "got " + str(status))

# A second account proves the hit counter moved without depending on the first one's lifecycle.
status, other, _ = api("POST", "/api/auth/signup", {"email": "cache2-" + uuid.uuid4().hex[:8] + "@example.com",
                                                   "password": password, "username": "cachecheck2"})
other_token = other.get("access_token", "") if isinstance(other, dict) else ""
if other_token:
    api("GET", "/api/auth/me", token=other_token)
    api("GET", "/api/auth/me", token=other_token)
    after_text, _ = metrics_text()
    hits_after = metric_value(after_text, "animind_cache_operations_total", '{backend="redis",result="hit"}')
    if hits_before is not None and hits_after is not None:
        record("the second resolution was served from the store", hits_after > hits_before,
               "hits " + str(hits_before) + " -> " + str(hits_after))
    else:
        skip("the second resolution was served from the store", "no redis-backed counters exposed (in-process cache?)")

# --- 3. The AniList relay answers a repeat without leaving the container -----------------
# Documents live for CACHE_METADATA_TTL_HOURS, so a second run of this harness would otherwise
# find its own previous answers and report a healthy cache as a failed MISS assertion.
if not args.no_redis:
    code, out, err = redis_cli("--scan", "--pattern", NS + ":anilist:*")
    stale = [line for line in out.splitlines() if line.startswith(NS + ":anilist:")]
    if stale:
        code, delout, derr = redis_cli("DEL", *stale)
    record("the relay cache starts empty for this run", not stale or not derr,
           "cleared %d stored document(s)" % len(stale) if stale else "nothing to clear")

query = {"query": "query($s:String){Page(perPage:2){media(type:ANIME,search:$s){id title{romaji}}}}",
         "variables": {"search": "Frieren"}}
status, first, first_headers = api("POST", "/api/anilist", query)
record("the AniList relay answers", status == 200, "got " + str(status))
record("the relay reports a MISS the first time", str(first_headers.get("X-Cache", "")).upper() == "MISS",
       "X-Cache=" + str(first_headers.get("X-Cache")))

status, again, again_headers = api("POST", "/api/anilist", query)
record("the identical query is served from the store",
       str(again_headers.get("X-Cache", "")).upper() == "HIT" and again == first,
       "X-Cache=" + str(again_headers.get("X-Cache")) + " status=" + str(status))

other_query = dict(query, variables={"search": "Bocchi the Rock!"})
status, _, other_headers = api("POST", "/api/anilist", other_query)
record("a different search is a different document",
       str(other_headers.get("X-Cache", "")).upper() == "MISS",
       "X-Cache=" + str(other_headers.get("X-Cache")))

# --- 4. Rate-limit counters live in the shared store -------------------------------------
status, _, _ = api("POST", "/api/rescan", token="not-a-real-token")
code, windows, err = (0, "", "") if args.no_redis else redis_cli("--scan", "--pattern", NS + ":rl:*")
if args.no_redis:
    skip("rate-limit windows live in Redis", "--no-redis was passed")
else:
    keys = [line for line in windows.splitlines() if line.startswith(NS + ":rl:")]
    record("the rate-limit window is a Redis key", bool(keys), (keys[:2] or err))
    if keys:
        code, value, err = redis_cli("GET", keys[0])
        code, ttl, err = redis_cli("PTTL", keys[0])
        record("the window counts hits and resets itself", value.isdigit() and 0 < int(ttl or -1) <= 61_000,
               "count=" + str(value) + " PTTL=" + str(ttl) + "ms")

# --- 5. Prometheus and Grafana are wired to each other -----------------------------------
status, targets = get_json(args.prometheus + "/api/v1/targets")
scrape_ok = False
detail = ""
if isinstance(targets, dict):
    for entry in targets.get("data", {}).get("activeTargets", []):
        if entry.get("labels", {}).get("job") == "animind-api":
            scrape_ok = entry.get("health") == "up"
            detail = str(entry.get("labels", {}).get("instance")) + " " + str(entry.get("lastError", ""))[:80]
            break
if not detail:
    detail = str(targets)[:160]
record("Prometheus scrapes the API", scrape_ok, detail)

status, series = get_json(args.prometheus + "/api/v1/query?query=animind_cache_operations_total")
has_series = isinstance(series, dict) and bool(series.get("data", {}).get("result"))
record("the cache counters reach Prometheus", has_series,
       str(len(series.get("data", {}).get("result", []))) + " series" if has_series else str(series)[:120])

status, counters = get_json(args.prometheus + "/api/v1/query?query=animind_http_requests_total")
has_routes = isinstance(counters, dict) and any(
    entry.get("metric", {}).get("route") for entry in counters.get("data", {}).get("result", []))
record("the route-labelled request counter reaches Prometheus", has_routes, str(counters)[:120])

auth = "Basic " + __import__("base64").b64encode(
    (args.grafana_user + ":" + args.grafana_password).encode()).decode()
status, health = get_json(args.grafana + "/api/health")
record("Grafana is healthy", status == 200 and isinstance(health, dict) and health.get("database") == "ok", str(health)[:120])

status, dash = get_json(args.grafana + "/api/dashboards/uid/animind-operations", headers={"Authorization": auth})
panels = dash.get("dashboard", {}).get("panels", []) if isinstance(dash, dict) else []
record("the AniMind dashboard is provisioned", status == 200 and len(panels) >= 10,
       str(len(panels)) + " panels" if panels else str(dash)[:120])

# --- 6. Nothing that should be gone is left ----------------------------------------------
if not args.no_redis:
    code, out, err = redis_cli("DBSIZE")
    # redis-cli prints "N" for `redis-cli dbsize`; take the last token either way.
    tokens = out.split()
    count = int(tokens[-1]) if tokens and tokens[-1].isdigit() else -1
    record("Redis holds a bounded number of keys", 0 <= count < 5_000, "dbsize=" + str(count) + " " + err)

passed = len([1 for _, ok in results if ok])
print("\n%d/%d checks passed" % (passed, len(results)) + (", %d skipped" % len(skipped) if skipped else ""))
sys.exit(0 if passed == len(results) else 1)

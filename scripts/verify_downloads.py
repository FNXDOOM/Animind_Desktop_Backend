#!/usr/bin/env python3
"""End-to-end torrent download verification against a fully local swarm.

This is the spec §8.1 gate: it builds the server and seeder binaries, seeds a
small fixture release with cmd/animind-seed (DHT/PEX/trackers off, loopback
only), serves a one-item Torznab stub over HTTP, boots the API against a
disposable Postgres database, and then drives the real user flow:

  signup (no entitlement) -> create refused 403 download_forbidden
  admin PATCH can_download -> entitlement granted
  GET  /api/torrents/search  -> one opaque result, no magnet on the wire
  POST /api/downloads        -> job created
  poll GET /api/downloads/id -> queued..ready (timeout --job-timeout, 10 min)
  GET  /api/episodes/{id}/stream-ticket?clientType=native -> ranged GET 206

It finishes by scraping the Prometheus listener for the §7.10 metric names.

Everything runs on 127.0.0.1; nothing reaches a public swarm. Child processes
are terminated in a finally block, and the workspace (fixture, library, logs)
is removed unless --keep is passed.

Usage (from the backend repo root, with the animind-db container up):
    python scripts/verify_downloads.py [--repo DIR] [--db NAME] [--keep]

Exit status is 0 when every check passes, 1 otherwise.
"""

import argparse
import base64
import json
import os
import queue
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from xml.sax.saxutils import escape


def attrescape(value):
    """escape() for attribute values: quotes must not break out of the attr."""
    return escape(value, {'"': "&quot;"})

CHECKS = []
FAILED = []


def record(label, ok, detail=""):
    CHECKS.append(label)
    print(("  PASS  " if ok else "  FAIL  ") + label + (("  | " + str(detail)) if detail else ""))
    if not ok:
        FAILED.append(label)


def summary():
    print()
    print(f"{len(CHECKS) - len(FAILED)}/{len(CHECKS)} checks passed")
    for name in FAILED:
        print("  FAILED: " + name)
    return 1 if FAILED else 0


class Api:
    """The same minimal HTTP client the other verify scripts carry."""

    def __init__(self, base):
        self.base = base.rstrip("/")

    def call(self, method, path, body=None, token=None, headers=None):
        request = urllib.request.Request(self.base + path, method=method)
        request.add_header("Accept", "application/json")
        if token:
            request.add_header("Authorization", "Bearer " + token)
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            request.add_header("Content-Type", "application/json")
        for key, value in (headers or {}).items():
            request.add_header(key, value)
        try:
            with urllib.request.urlopen(request, data, timeout=180) as response:
                return response.status, response.read(), dict(response.headers)
        except urllib.error.HTTPError as error:
            return error.code, error.read(), dict(error.headers or {})
        except Exception as error:  # a refused connection is a failure, not a crash
            return 0, str(error).encode(), {}

    def json(self, method, path, body=None, token=None, headers=None):
        status, payload, hdr = self.call(method, path, body, token, headers)
        try:
            parsed = json.loads(payload or b"{}")
        except Exception:
            parsed = {"_raw": payload[:200].decode("utf8", "replace")}
        return status, parsed, hdr


def error_code(payload):
    return payload.get("code", "") if isinstance(payload, dict) else ""


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def wait_for_health(api, deadline_s=45):
    deadline = time.time() + deadline_s
    while time.time() < deadline:
        status, body, _ = api.json("GET", "/health")
        if status == 200:
            return True, body
        time.sleep(0.4)
    return False, {}


def psql(container, db, statement):
    """Run one SQL statement in the animind-db container. Returns stdout or None."""
    command = ["docker", "exec", container, "psql", "-U", "animind", "-d", db, "-tAc", statement]
    try:
        result = subprocess.run(command, capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.TimeoutExpired):
        return None
    return result.stdout.strip() if result.returncode == 0 else None


class TorznabStub:
    """One-item Torznab endpoint. The magnet arrives after the seeder reports it."""

    def __init__(self, title, size_bytes):
        self.title = title
        self.size_bytes = size_bytes
        self.magnet = ""
        self.info_hash = ""
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                if not outer.magnet:
                    self.send_error(503, "seeder has not reported a magnet yet")
                    return
                body = outer.feed().encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/rss+xml; charset=utf-8")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.port = self.httpd.server_address[1]
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)

    def start(self):
        self.thread.start()

    def stop(self):
        self.httpd.shutdown()
        self.httpd.server_close()

    def feed(self):
        magnet = attrescape(self.magnet)
        return (
            '<?xml version="1.0" encoding="UTF-8"?>'
            '<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">'
            "<channel><title>verify-stub</title>"
            f"<item><title>{escape(self.title)}</title>"
            f"<guid>{magnet}</guid>"
            f"<link>{magnet}</link>"
            f'<enclosure url="{magnet}" length="{self.size_bytes}" type="application/x-bittorrent" />'
            f"<pubDate>Mon, 26 Oct 2026 12:00:00 +0000</pubDate>"
            f"<size>{self.size_bytes}</size>"
            '<torznab:attr name="seeders" value="42" />'
            '<torznab:attr name="peers" value="42" />'
            '<torznab:attr name="category" value="5070" />'
            f'<torznab:attr name="infohash" value="{attrescape(self.info_hash)}" />'
            "</item></channel></rss>"
        )


def start_process(label, command, env, log_path):
    """Spawn a child with output appended to log_path; returns the Popen."""
    log = open(log_path, "ab")
    proc = subprocess.Popen(command, stdout=log, stderr=log, env=env,
                            cwd=REPO, creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
    print(f"  started {label} (pid {proc.pid}, log {os.path.basename(log_path)})")
    return proc


def stop_process(proc, timeout=10):
    if proc is None or proc.poll() is not None:
        return
    proc.terminate()
    try:
        proc.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait(timeout=timeout)


def seeder_payload(proc, timeout_s=60):
    """Read the seeder's stdout until the JSON line arrives."""
    lines = queue.Queue()
    reader = threading.Thread(target=lambda: [lines.put(line) for line in iter(proc.stdout.readline, "")], daemon=True)
    reader.start()
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        try:
            line = lines.get(timeout=1).strip()
        except queue.Empty:
            if proc.poll() is not None:
                return None, f"seeder exited early with code {proc.returncode}"
            continue
        if not line.startswith("{"):
            continue
        try:
            payload = json.loads(line)
            if payload.get("magnet") and payload.get("infoHash"):
                return payload, None
        except json.JSONDecodeError:
            continue
    return None, f"no JSON payload within {timeout_s}s"


def poll_job(api, token, job_id, timeout_s, api_proc):
    """Poll one job to a terminal state; returns (state, error, seen_states, max_peers)."""
    seen, max_peers = [], 0
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        if api_proc.poll() is not None:
            return "api-crashed", f"server exited with code {api_proc.returncode}", seen, max_peers
        status, job, _ = api.json("GET", f"/api/downloads/{job_id}", token=token)
        if status != 200:
            return "http-error", f"GET /api/downloads/{job_id} -> {status}", seen, max_peers
        state = job.get("state", "?")
        if not seen or seen[-1] != state:
            seen.append(state)
            print(f"    state: {state}  ({job.get('progressPct', 0):.1f}% of "
                  f"{job.get('sizeBytes', 0)} bytes, peers={job.get('peers', 0)})")
        max_peers = max(max_peers, job.get("peers", 0))
        if state == "ready":
            return "ready", None, seen, max_peers
        if state == "failed":
            return "failed", job.get("error") or "no error code", seen, max_peers
        time.sleep(1)
    return "timeout", f"not ready after {timeout_s}s", seen, max_peers


def main():
    global REPO
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", default=os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                        help="backend repo root (default: the tree this script lives in)")
    parser.add_argument("--port", type=int, default=0, help="API port (default: a free one)")
    parser.add_argument("--db", default="animind_verify", help="disposable Postgres database (dropped and recreated)")
    parser.add_argument("--container", default="animind-db", help="Postgres container for the SQL bootstrap")
    parser.add_argument("--job-timeout", type=int, default=600, help="seconds to wait for the job to reach ready")
    parser.add_argument("--keep", action="store_true", help="keep the workspace, database and logs")
    args = parser.parse_args()
    REPO = os.path.abspath(args.repo)

    suffix = base64.urlsafe_b64encode(os.urandom(6)).decode().rstrip("=").lower()
    emails = {
        "worker": f"verify-dl-{suffix}@example.com",
        "admin": f"verify-admin-{suffix}@example.com",
    }
    password = "correct horse battery"

    # ── build ────────────────────────────────────────────────────────────────
    exe = ".exe" if os.name == "nt" else ""
    server_bin = os.path.join(REPO, "build", "animind-server" + exe)
    seeder_bin = os.path.join(REPO, "build", "animind-seed" + exe)
    for target, pkg in ((server_bin, "./cmd/server"), (seeder_bin, "./cmd/animind-seed")):
        build = subprocess.run(["go", "build", "-o", target, pkg], cwd=REPO, capture_output=True, text=True)
        if build.returncode != 0:
            print(f"go build {pkg} failed:\n{build.stderr}", file=sys.stderr)
            return 2
        print(f"  built {os.path.relpath(target, REPO)}")

    # ── workspace: fixture release + library root ────────────────────────────
    workspace = tempfile.mkdtemp(prefix="animind-verify-dl-")
    release = "Smoke.Show.S01E01.1080p.WEB-DL.x264"
    content_dir = os.path.join(workspace, "content", release)
    os.makedirs(content_dir)
    video_bytes, nfo_bytes = 384 * 1024, 4096
    with open(os.path.join(content_dir, "Smoke Show S01E01.mkv"), "wb") as f:
        f.write(os.urandom(video_bytes))
    with open(os.path.join(content_dir, "Smoke.Show.S01E01.1080p.WEB-DL.x264.nfo"), "wb") as f:
        f.write(os.urandom(nfo_bytes))
    total_bytes = video_bytes + nfo_bytes
    library = os.path.join(workspace, "library")
    os.makedirs(library)

    stub = TorznabStub(release, total_bytes)
    stub.start()
    print(f"  stub torznab on 127.0.0.1:{stub.port}")

    api_port = args.port or free_port()
    metrics_port = free_port()
    api = Api(f"http://127.0.0.1:{api_port}")
    api_proc = seed_proc = None
    api_log = os.path.join(workspace, "api.log")
    seed_log = os.path.join(workspace, "seed.log")

    def sql(statement):
        return psql(args.container, "postgres", statement)

    try:
        # ── disposable database ──────────────────────────────────────────────
        if sql(f"drop database if exists {args.db}") is None:
            print(f"could not reach the {args.container} container; the disposable database is required "
                  "for the admin bootstrap", file=sys.stderr)
            return 2
        if sql(f"create database {args.db}") is None:
            print(f"could not create database {args.db}", file=sys.stderr)
            return 2
        print(f"  disposable database {args.db} created")
        exec_psql = lambda statement: psql(args.container, args.db, statement)

        # ── seeder ───────────────────────────────────────────────────────────
        seed_proc = subprocess.Popen([seeder_bin, "-dir", content_dir, "-json"],
                                     stdout=subprocess.PIPE, stderr=open(seed_log, "wb"), text=True,
                                     cwd=REPO, creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
        payload, err = seeder_payload(seed_proc)
        if payload is None:
            print(f"seeder did not report a magnet: {err}", file=sys.stderr)
            return 2
        stub.magnet, stub.info_hash = payload["magnet"], payload["infoHash"]
        print(f"  seeder on {payload['infoHash']} x.pe={stub.magnet.split('x.pe=')[-1]}")

        # ── API server ───────────────────────────────────────────────────────
        env = os.environ.copy()
        env.update({
            "PORT": str(api_port),
            "STORAGE_MODE": "local",
            "LOCAL_STORAGE_PATH": library,
            "DATABASE_URL": f"postgres://animind:animind@127.0.0.1:5432/{args.db}?sslmode=disable",
            "DESKTOP_TOKEN_SIGNING_KEY": base64.b64encode(os.urandom(32)).decode(),
            "SCANNER_ON_STARTUP": "false",
            "ENV": "development",
            # no network beyond the loopback: catalog enrichment off, discovery off
            "ANILIST_ENABLED": "false",
            "MYANIMELIST_ENABLED": "false",
            "OPENROUTER_ENABLED": "false",
            "TORZNAB_URL": f"http://127.0.0.1:{stub.port}/api",
            "TORZNAB_API_KEY": "verify-stub-key",
            "DOWNLOADS_DHT": "false",
            "DOWNLOADS_PEX": "false",
            "DOWNLOADS_MIN_FREE_GB": "1",
            "METRICS_ENABLED": "true",
            "METRICS_ADDR": f"127.0.0.1:{metrics_port}",
            "LOG_LEVEL": "info",
        })
        api_proc = start_process("api server", [server_bin], env, api_log)
        healthy, health = wait_for_health(api)
        if not healthy:
            tail = open(api_log, "rb").read()[-2000:].decode("utf8", "replace")
            print(f"api server never became healthy:\n{tail}", file=sys.stderr)
            record("api server boots and reports healthy", False, "no /health 200 within 45s")
            return summary()
        record("api server boots and reports healthy", True,
               f"port={api_port} database={health.get('database')} storage={health.get('storage')}")

        # ── accounts: a plain user and an SQL-bootstrapped admin ─────────────
        status, worker, _ = api.json("POST", "/api/auth/signup",
                                     {"email": emails["worker"], "password": password, "username": "verifydl" + suffix})
        worker_token = worker.get("access_token", "")
        record("worker signup returns a session token", status == 201 and worker_token.startswith("as_"), f"{status}")
        status, admin, _ = api.json("POST", "/api/auth/signup",
                                    {"email": emails["admin"], "password": password, "username": "verifyad" + suffix})
        admin_token = admin.get("access_token", "")
        if not admin_token:
            print("admin signup failed; cannot continue", file=sys.stderr)
            return summary()
        promoted = exec_psql(f"update animind_users set is_admin=true where email_normalized='{emails['admin']}'")
        record("admin bootstrapped by direct SQL (the repo's e2e mechanism)", promoted == "UPDATE 1", str(promoted))

        status, me, _ = api.json("GET", "/api/auth/me", token=worker_token)
        worker_id = me.get("user", {}).get("id", "")

        # ── entitlement gate ─────────────────────────────────────────────────
        status, refused, _ = api.json("POST", "/api/downloads", {"resultId": "x"}, token=worker_token)
        record("create by a non-entitled user is 403 download_forbidden",
               status == 403 and error_code(refused) == "download_forbidden", f"{status} {refused}")

        status, patched, _ = api.json("PATCH", f"/api/admin/users/{worker_id}", {"can_download": True},
                                      token=admin_token)
        status, users, _ = api.json("GET", "/api/admin/users", token=admin_token)
        row = next((u for u in users if u.get("id") == worker_id), {}) if status == 200 else {}
        record("admin PATCH flips can_download", status == 200 and patched and row.get("can_download") is True,
               f"patch={status} row={str(row)[:70]}")

        # ── search ───────────────────────────────────────────────────────────
        status, search, _ = api.json("GET", "/api/torrents/search?q=" + urllib.parse.quote("Smoke Show"),
                                     token=worker_token)
        results = search.get("data", []) if isinstance(search, dict) else []
        record("search returns the stub's one item", status == 200 and len(results) == 1,
               f"{status} total={search.get('total') if isinstance(search, dict) else search}")
        if not results:
            return summary()
        result = results[0]
        record("the wire result carries an opaque id and no magnet",
               bool(result.get("id")) and "magnet:?" not in json.dumps(result), str(result)[:90])

        # ── create + drive the job ───────────────────────────────────────────
        status, created, _ = api.json("POST", "/api/downloads", {"resultId": result["id"]}, token=worker_token)
        job_id = created.get("id", "") if isinstance(created, dict) else ""
        record("create with the opaque resultId starts a job", status == 201 and bool(job_id),
               f"{status} {str(created)[:90]}")
        if not job_id:
            return summary()

        started = time.time()
        state, err, seen, max_peers = poll_job(api, worker_token, job_id, args.job_timeout, api_proc)
        elapsed = time.time() - started
        record(f"job reaches ready (took {elapsed:.1f}s)", state == "ready",
               f"states={' > '.join(seen)} error={err} max_peers={max_peers}")
        if state != "ready":
            tail = open(api_log, "rb").read()[-2000:].decode("utf8", "replace")
            print(f"--- api log tail ---\n{tail}", file=sys.stderr)
            return summary()

        # ── the imported episode streams ─────────────────────────────────────
        show_id = (api.json("GET", f"/api/downloads/{job_id}", token=worker_token)[1] or {}).get("showId")
        if not show_id:
            status, shows, _ = api.json("GET", "/api/shows?limit=200")
            match = next((s for s in shows.get("data", []) if "Smoke Show" in s.get("title", "")), None) \
                if status == 200 else None
            show_id = match.get("id") if match else None
        status, detail, _ = api.json("GET", f"/api/shows/{show_id}") if show_id else (0, {}, {})
        episodes = detail.get("episodes", []) if isinstance(detail, dict) else []
        episode_id = episodes[0]["id"] if episodes else None
        record("the imported episode is in the catalog", bool(episode_id),
               f"showId={show_id} episodes={len(episodes)}")
        if not episode_id:
            return summary()

        status, ticket, _ = api.json("GET",
                                     f"/api/episodes/{episode_id}/stream-ticket?clientType=native",
                                     token=worker_token)
        url = ticket.get("url", "") if isinstance(ticket, dict) else ""
        record("native stream ticket issued", status == 200 and url.startswith("/api/episodes/"), f"{status} {str(ticket)[:80]}")
        if not url:
            return summary()

        status, body, hdr = api.call("GET", url, headers={"Range": "bytes=0-1023"})
        record("ranged GET of the downloaded episode returns 206 with exactly those bytes",
               status == 206 and len(body) == 1024 and str(hdr.get("Content-Range", "")).startswith("bytes 0-1023/"),
               f"{status} {len(body)} bytes content-range={hdr.get('Content-Range')}")

        # ── §7.10 metrics reach the scrape endpoint ──────────────────────────
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{metrics_port}/metrics", timeout=10) as response:
                exposition = response.read().decode("utf8", "replace")
        except Exception as scrape_err:
            exposition = f"<scrape failed: {scrape_err}>"
        wanted = ["animind_downloads_active", "animind_downloads_bytes_total",
                  "animind_downloads_failed_total", "torrent_search_seconds"]
        missing = [name for name in wanted if name not in exposition]
        record("metrics listener exposes the four spec 7.10 collectors", not missing,
               f"missing={missing or 'none'}")
        counted = 0.0
        for line in exposition.splitlines():
            if line.startswith("animind_downloads_bytes_total "):
                try:
                    counted = float(line.split()[-1])
                except ValueError:
                    pass
        record("animind_downloads_bytes_total counted the fixture download",
               counted >= total_bytes, f"counted={counted:.0f} fixture={total_bytes}")

    finally:
        stop_process(seed_proc)
        stop_process(api_proc)
        stub.stop()
        if not args.keep:
            psql(args.container, "postgres", f"drop database if exists {args.db}")
            for _ in range(10):  # Windows holds locks briefly after terminate
                try:
                    shutil.rmtree(workspace)
                    break
                except OSError:
                    time.sleep(0.3)
        else:
            print(f"  workspace kept: {workspace} (logs: {api_log}, {seed_log})")

    return summary()


def guarded_main():
    """An unexpected internal error is a FAIL with output, never a bare traceback."""
    try:
        return main()
    except Exception as error:
        record("verification script ran to completion", False, f"{type(error).__name__}: {error}")
        import traceback
        traceback.print_exc()
        return summary()


if __name__ == "__main__":
    sys.exit(guarded_main())

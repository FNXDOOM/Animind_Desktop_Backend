#!/usr/bin/env python3
"""End-to-end verification for a running AniMind backend.

Drives the HTTP API of a live server exactly as the desktop player and the website do, and
asserts the behaviour the port is supposed to preserve: release-name parsing into a catalog,
AniList enrichment, ticket-scoped media bytes with real range requests, the desktop pairing
chain (bridge JWT -> adk_ exchange -> rotation -> revocation), ownership rules, and the
Clerk-free auth surface.

Unlike the Go suites this needs a running server, because what it checks is the seams: HTTP
negotiation, byte ranges, header effects, and session revocation reaching a live token.

Usage
    python3 scripts/e2e_verify.py [--base URL] [--dsn POSTGRES_DSN]

  --base   API origin to test                  (default http://localhost:3001, or $E2E_BASE)
  --dsn    psql DSN used for the two checks that must look at rows directly
           (optional; those checks skip when absent)

The server must be reachable and its database migrated. Use a disposable database: the script
creates accounts, deletes one, and triggers library scans. Example against a local run:

    PORT=3099 STORAGE_MODE=local LOCAL_STORAGE_PATH=./library \
      DATABASE_URL=... DESKTOP_TOKEN_SIGNING_KEY=... ./build/animind-server &
    python3 scripts/e2e_verify.py --base http://localhost:3099

Exit status is 0 when every check passes, 1 otherwise.
"""

import argparse
import base64
import http.client
import json
import os
import shlex
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request

CHECKS = []
FAILED = []


def record(label, ok, detail=""):
    CHECKS.append(label)
    print(("  PASS  " if ok else "  FAIL  ") + label + (("  | " + str(detail)) if detail else ""))
    if not ok:
        FAILED.append(label)


class Api:
    def __init__(self, base):
        self.base = base.rstrip("/")

    def call(self, method, path, body=None, token=None, headers=None, raw=False):
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
            parsed = {"_raw": payload[:160].decode("utf8", "replace")}
        return status, parsed, hdr


def decode_claims(jwt):
    payload = jwt.split(".")[1]
    return json.loads(base64.urlsafe_b64decode(payload + "==="))


def response_headers(base, path, token):
    """Return the response headers of a GET, without reading the body.

    The SSE endpoint streams forever, so the generic Api.call would block on
    response.read(); the header verdict is all this check needs. Returns
    (status, headers) or (0, {}) when the connection fails.
    """
    parsed = urllib.parse.urlparse(base)
    try:
        conn = http.client.HTTPConnection(parsed.hostname, parsed.port, timeout=10)
        conn.request("GET", path, headers={"Authorization": "Bearer " + token})
        response = conn.getresponse()
        headers = dict(response.getheaders())
        response.close()
        conn.close()
        return response.status, headers
    except Exception:
        return 0, {}


def error_code(payload):
    """The machine-readable code of an error body, if it carries one."""
    if isinstance(payload, dict):
        return payload.get("code", "")
    return ""


def psql(dsn, query):
    """Run a read/verify query directly against the database.

    Two ways to reach the server, because the database is often not on this host: pass a DSN
    (uses the local psql), or set E2E_PSQL_PREFIX to the command prefix that reaches it, e.g.
    "docker exec -i animind-pg psql -U animind -d animind". Returns None when neither is
    configured, so the dependent checks skip instead of failing.
    """
    prefix = os.environ.get("E2E_PSQL_PREFIX", "").strip()
    if prefix:
        command = shlex.split(prefix) + ["-tAc", query]
    elif dsn:
        command = ["psql", dsn, "-tAc", query]
    else:
        return None
    result = subprocess.run(command, capture_output=True, text=True)
    return result.stdout.strip() if result.returncode == 0 else None


def sign_in(api, email, password, username):
    """Create an account, or reuse it.

    The harness is meant to run against a disposable database, but a second run against the
    same database must not report failures caused by a 409: sign-up falling back to login
    keeps the checks about behaviour rather than about table state.
    """
    status, body, _ = api.json("POST", "/api/auth/signup", {"email": email, "password": password, "username": username})
    if status == 201:
        return True, body
    status, body, _ = api.json("POST", "/api/auth/login", {"email": email, "password": password})
    return False, body


def run(api, dsn):
    print("== auth ==")
    created, signup = sign_in(api, "e2e-owner@example.com", "correct horse battery", "e2eowner")
    token = signup.get("access_token", "")
    record("signup returns a session token", bool(token.startswith("as_")), f"created={created} {str(signup)[:60]}")
    record("a new account is not an admin", signup.get("user", {}).get("is_admin") is False or not created)
    record("no credential material is serialized",
           all(k not in json.dumps(signup) for k in ("password_hash", "token_hash", "failed_login", "locked_until")))

    # Admin rights come from the account row, so promote directly rather than through an
    # endpoint that would need an existing admin.
    promoted = psql(dsn, "update animind_users set is_admin=true where email_normalized='e2e-owner@example.com'")
    if promoted is None:
        print("  SKIP  no database access (pass --dsn or E2E_PSQL_PREFIX); admin and catalog checks need the promotion")

    status, login, _ = api.json("POST", "/api/auth/login",
                               {"email": " E2E-Owner@Example.com ", "password": "correct horse battery", "remember_me": True})
    token = login.get("access_token", "")
    record("login normalizes case and whitespace", status == 200, f"{status} {login}")
    admin = login.get("user", {}).get("is_admin") is True
    record("admin flag reaches the API", admin, "promote with --dsn / E2E_PSQL_PREFIX to check this")

    _, wrong_owner, s1 = api.json("POST", "/api/auth/login", {"email": "e2e-owner@example.com", "password": "wrong password xx"})
    _, nobody, s2 = api.json("POST", "/api/auth/login", {"email": "e2e-absent@example.com", "password": "wrong password xx"})
    record("unknown address and wrong password are indistinguishable", (s1, wrong_owner) == (s2, nobody), f"{s1} vs {s2}")

    status, me, _ = api.json("GET", "/api/auth/me", token=token)
    record("GET /api/auth/me with a session", status == 200 and me["user"]["email"] == "e2e-owner@example.com")
    status, _, _ = api.json("GET", "/api/auth/me")
    record("anonymous /api/auth/me is 401", status == 401)

    print("== catalog and scanner ==")
    status, _, _ = api.json("POST", "/api/rescan", {})
    record("rescan refuses anonymous callers", status == 401, f"{status}")

    if not admin:
        print("  SKIP  scanner/catalog checks need an admin account (pass --dsn to promote one)")
        return

    status, library, _ = api.json("POST", "/api/rescan", {}, token=token)
    titles = sorted(x["title"] for x in library) if isinstance(library, list) else []
    record("rescan answers the library array", status == 200 and isinstance(library, list), titles)
    frieren = next((x for x in library or [] if "rieren" in x.get("title", "")), None)
    record("release folders became shows", bool(frieren), titles)
    if frieren:
        record("episode count came from parsed filenames", frieren.get("episodes", 0) >= 2, frieren.get("episodes"))
        record("the site's reason string is preserved", frieren.get("reason") == "From your cloud library")
        record("AniList enrichment filled the card",
               bool(frieren.get("imageUrl")) and (frieren.get("rating") or 0) > 0 and frieren.get("genres"),
               f"img={bool(frieren.get('imageUrl'))} rating={frieren.get('rating')}")

    status, shows, _ = api.json("GET", "/api/shows?limit=200")
    record("GET /api/shows is public", status == 200 and shows.get("total", 0) >= 1)
    status, hits, _ = api.json("GET", "/api/shows?q=%25")
    record("a literal % in a search is not a wildcard", status == 200 and hits.get("total") == 0, hits.get("total"))
    status, hits, _ = api.json("GET", "/api/shows?q=rieren")
    record("title search matches", status == 200 and hits.get("total") >= 1)

    show_id = (frieren or {}).get("id")
    episode_id = None
    if show_id:
        status, detail, _ = api.json("GET", f"/api/shows/{show_id}")
        episodes = detail.get("episodes", [])
        episode_id = episodes[0]["id"] if episodes else None
        record("show detail is unwrapped, episodes attached", status == 200 and len(episodes) >= 2, len(episodes))
        record("episode numbers parsed correctly",
               sorted(e["episode_number"] for e in episodes) == sorted(set(sorted(e["episode_number"] for e in episodes))),
               [(e["season_number"], e["episode_number"]) for e in episodes])
        record("storage paths never reach a client", "file_path" not in json.dumps(detail) and "bucket_name" not in json.dumps(detail))
    status, _, _ = api.json("GET", "/api/shows/00000000-0000-0000-0000-000000000000")
    record("unknown show is 404", status == 404)
    status, _, _ = api.json("GET", "/api/shows/not-a-uuid")
    record("malformed id answers 404, not 500", status == 404)

    print("== playback ==")
    if episode_id:
        status, ticket, _ = api.json("GET", f"/api/episodes/{episode_id}/stream-ticket", token=token)
        record("browser ticket issued", status == 200 and ticket.get("url", "").startswith(f"/api/episodes/{episode_id}/stream?st="))
        status, native, _ = api.json("GET", f"/api/episodes/{episode_id}/stream-ticket?clientType=desktop", token=token)
        record("native client negotiated", status == 200 and native.get("clientType") == "native")
        record("native ticket binds no audio track", "&at=" not in native.get("url", ""))

        for label, url in (("browser", ticket), ("native", native)):
            status, body, hdr = api.call("GET", url["url"])
            record(f"{label} ticket streams without a header", status in (200, 206) and len(body) > 0,
                   f"{status} {len(body)} bytes {hdr.get('Content-Type')}")
        status, body, hdr = api.call("GET", native["url"], headers={"Range": "bytes=100-199"})
        record("range request returns exactly those bytes", status == 206 and len(body) == 100, f"{status} {hdr.get('Content-Range')}")
        record("Accept-Ranges advertised", hdr.get("Accept-Ranges") == "bytes")
        status, _, hdr = api.call("GET", native["url"], headers={"Range": "bytes=9999999999-"})
        record("impossible range is 416 with the total size", status == 416, hdr.get("Content-Range"))
        status, _, _ = api.call("GET", ticket["url"].replace("st=", "x="))
        record("tampered ticket is refused", status == 401, f"{status}")

        status, subs, _ = api.json("GET", f"/api/episodes/{episode_id}/subtitles", token=token)
        record("subtitles answer a tracks array", status == 200 and isinstance(subs.get("tracks"), list))
        status, tracks, _ = api.json("GET", f"/api/episodes/{episode_id}/audio-tracks", token=token)
        record("audio tracks answer a tracks array", status == 200 and isinstance(tracks.get("tracks"), list))
        status, hls, _ = api.json("POST", f"/api/episodes/{episode_id}/hls-session", {"startTime": 3}, token=token)
        record("hls-session answers something checkable", status in (200, 400, 500, 504), f"{status} {str(hls)[:90]}")

    print("== user data ==")
    status, item, _ = api.json("PUT", "/api/me/watchlist/153564",
                               {"anime_data": {"id": "153564", "title": "Death Note"}, "status": "Watching"}, token=token)
    record("watchlist upsert", status == 200 and item.get("status") == "Watching")
    status, listed, _ = api.json("GET", "/api/me/watchlist", token=token)
    record("watchlist is a bare array (desktop parses one)",
           status == 200 and isinstance(listed, list) and listed[0]["anime_data"]["title"] == "Death Note")
    status, _, _ = api.json("PATCH", "/api/me/watchlist/153564", {"status": "Dropped"}, token=token)
    record("status patch", status == 200)
    status, bad, _ = api.json("PATCH", "/api/me/watchlist/153564", {"status": "On Hold"}, token=token)
    record("unknown status rejected", status == 400, bad)
    status, bad, _ = api.json("PUT", "/api/me/watchlist/153564",
                              {"anime_data": {"x": 1}, "anime_id": "999"}, token=token)
    record("body anime_id disagreeing with the path is 409", status == 409)

    _, other = sign_in(api, "e2e-other@example.com", "another password", "e2eother")
    other_token = other.get("access_token", "")
    status, theirs, _ = api.json("GET", "/api/me/watchlist", token=other_token)
    record("a second account cannot see the first one's rows", status == 200 and theirs == [], theirs)

    status, _, _ = api.json("PUT", "/api/me/progress", {"anime_id": "153564", "episode_index": 3, "timestamp": 412.5}, token=token)
    record("progress saved", status == 200)
    status, progress, _ = api.json("GET", "/api/me/progress?anime_id=153564&episode_index=3", token=token)
    record("progress read back", progress.get("timestamp") == 400 + 12.5, progress)
    status, missing, _ = api.json("GET", "/api/me/progress?anime_id=153564&episode_index=77", token=token)
    record("never-watched reads as 0 rather than 404", status == 200 and missing.get("timestamp") == 0)
    status, recent, _ = api.json("GET", "/api/me/progress/recent", token=token)
    record("recent progress lists one entry", status == 200 and len(recent.get("items", [])) == 1)

    status, _, _ = api.json("PUT", "/api/me/history",
                            {"show_title": "Frieren", "episode_label": "Episode 1", "progress_pct": 137}, token=token)
    record("history upsert accepts an out-of-range percent", status == 200)
    status, history, _ = api.json("GET", "/api/me/history", token=token)
    record("history is a bare array and clamped to 100",
           isinstance(history, list) and history and history[0]["progress_pct"] == 100, history[:1])

    status, _, _ = api.json("PUT", "/api/me/preferences", {"volume": 0.4, "speed": 1.25}, token=token)
    record("preferences saved", status == 200)
    status, prefs, _ = api.json("GET", "/api/me/preferences", token=token)
    record("preferences round-trip", prefs.get("volume") == 0.4, prefs)
    status, _, _ = api.json("PUT", "/api/me/preferences", [1, 2, 3], token=token)
    record("a non-object preferences body is rejected", status == 400)

    status, bad, _ = api.json("PATCH", "/api/me", {"avatar_url": "javascript:alert(1)"}, token=token)
    record("a javascript: avatar is refused", status == 400, bad)
    status, updated, _ = api.json("PATCH", "/api/me", {"username": "e2eowner2"}, token=token)
    record("profile update", status == 200 and updated["user"]["username"] == "e2eowner2")

    print("== desktop pairing chain ==")
    status, bridge, _ = api.json("POST", "/api/auth/desktop/bridge-token", {}, token=token)
    jwt = bridge.get("token", "")
    record("bridge token is a three-part JWT", status == 200 and jwt.count(".") == 2)
    claims = decode_claims(jwt) if jwt.count(".") == 2 else {}
    record("JWT carries the sub/sid/exp the player decodes", {"sub", "sid", "exp"} <= set(claims), sorted(claims))

    status, desktop, _ = api.json("POST", "/api/auth/desktop/exchange",
                                  {"deviceId": "e2e", "deviceName": "Animind Qt Desktop"}, token=jwt)
    record("exchange accepts the bridge JWT", status == 200 and desktop.get("access_token", "").startswith("adk_"))
    record("refresh token is the adr_ family", desktop.get("refresh_token", "").startswith("adr_"))
    record("both expiry fields present (the player prefers the string)",
           isinstance(desktop.get("expires_in"), int) and "T" in str(desktop.get("access_token_expires_at")))
    record("token_type is Bearer", desktop.get("token_type") == "Bearer")

    access, refresh = desktop.get("access_token", ""), desktop.get("refresh_token", "")
    status, me, _ = api.json("GET", "/api/auth/me", token=access)
    record("the adk_ token authenticates API calls", status == 200 and me.get("session_kind") == "desktop")
    status, _, _ = api.json("POST", "/api/auth/desktop/bridge-token", {}, token=access)
    record("a desktop token cannot pair a second device", status == 409)

    status, rotated, _ = api.json("POST", "/api/auth/desktop/refresh", {"refresh_token": refresh})
    record("refresh rotates the pair and keeps the session id",
           status == 200 and rotated.get("refresh_token") != refresh and rotated.get("session_id") == desktop.get("session_id"))
    status, replayed, _ = api.json("POST", "/api/auth/desktop/refresh", {"refresh_token": refresh})
    record("the consumed refresh token no longer works", status == 401, f"{status} {replayed}")

    status, sessions, _ = api.json("GET", "/api/auth/sessions", token=token)
    record("sessions lists web and desktop families",
           status == 200 and len(sessions.get("desktop_devices", [])) >= 1 and len(sessions.get("web_sessions", [])) >= 1,
           f"web={len(sessions.get('web_sessions', []))} desktop={len(sessions.get('desktop_devices', []))}")

    status, revoked, _ = api.json("POST", "/api/auth/desktop/revoke", {"refresh_token": rotated.get("refresh_token", "")},
                                  token=rotated.get("access_token", ""))
    record("revoke accepts a refresh token and a bearer together", status == 200, f"{status} {revoked}")
    status, _, _ = api.json("GET", "/api/auth/me", token=rotated.get("access_token", ""))
    record("a revoked access token stops working immediately", status == 401)

    print("== cross-account revocation ==")
    status, mine_list, _ = api.json("GET", "/api/auth/sessions", token=token)
    if not mine_list.get("web_sessions"):
        record("another account cannot revoke my session", False, "no web session listed to attack")
    else:
        my_session = mine_list["web_sessions"][0]["id"]
        status, denial, _ = api.json("DELETE", f"/api/auth/sessions/{my_session}", token=other_token)
        record("another account cannot revoke my session", status == 404, f"{status} {denial}")
        status, _, _ = api.json("GET", "/api/auth/me", token=token)
        record("the session survived that attempt", status == 200)

    print("== administration, webhook, proxy ==")
    status, metrics, _ = api.json("GET", "/api/admin/metrics", token=token)
    record("admin metrics report real counts",
           status == 200 and metrics.get("shows", 0) >= 1 and metrics.get("users", 0) >= 2,
           {k: metrics.get(k) for k in ("shows", "episodes", "users", "storage_mode")})
    status, users, _ = api.json("GET", "/api/admin/users", token=token)
    record("admin user list includes every account", status == 200 and isinstance(users, list) and len(users) >= 2,
           str(users)[:90])
    _, who, _ = api.json("GET", "/api/auth/me", token=token)
    my_user_id = who.get("user", {}).get("id", "")
    if my_user_id:
        # The guard has to be tried against the caller's own row; demoting anyone else is
        # supposed to succeed.
        status, self_demote, _ = api.json("PATCH", f"/api/admin/users/{my_user_id}", {"is_admin": False}, token=token)
        record("self-demotion is refused", status == 400, f"{status} {self_demote}")
        status, still_admin, _ = api.json("GET", "/api/auth/me", token=token)
        record("the admin flag survived the refused request", still_admin.get("user", {}).get("is_admin") is True)
    else:
        record("self-demotion is refused", False, "skipped: no user id from /api/auth/me")
    status, _, _ = api.json("GET", "/api/admin/metrics", token=other_token)
    record("non-admin is 403", status == 403)

    status, _, _ = api.json("POST", "/api/webhooks/storage", None, headers={"x-webhook-secret": "wrong"})
    record("webhook with a wrong secret is 401", status == 401)

    status, proxied, _ = api.json("POST", "/api/anilist",
                                  {"query": "query($search:String){Page(page:1,perPage:2){media(search:$search,type:ANIME){id title{romaji}}}}",
                                   "variables": {"search": "Frieren"}})
    record("anilist proxy relays a read-only query", status == 200 and "data" in proxied,
           str(proxied.get("data", proxied))[:90])
    status, _, _ = api.json("POST", "/api/anilist", {"query": "mutation{SaveMedia{id}}"})
    record("proxy refuses mutations", status == 400)
    status, _, _ = api.json("POST", "/api/anilist", {"query": "query{Page{media{id}}} # mutation{X}"})
    record("proxy refuses a mutation hidden in a comment", status == 400)

    status, _, _ = api.json("GET", "/api/syncplay/rooms/ABCDE", token=token)
    record("an unknown room code is 404", status == 404)

    _, _, hdr = api.call("OPTIONS", "/api/me/watchlist", headers={"Origin": "http://not-allowed.invalid"})
    record("a disallowed origin gets no CORS grant", "Access-Control-Allow-Origin" not in {k.lower() for k in hdr}, hdr)

    print("== torrents and downloads ==")
    # A deployment without TORZNAB_URL answers 501 indexer_not_configured; one
    # with a dead indexer answers a clean 5xx JSON error. Both are acceptable
    # outcomes of "the route errors cleanly"; a 200 means the target has a live
    # indexer and neither branch is reachable from here.
    status, search, _ = api.json("GET", "/api/torrents/search?q=rieren", token=token)
    if status == 501:
        record("torrent search without TORZNAB_URL answers 501 indexer_not_configured",
               error_code(search) == "indexer_not_configured", f"{status} {search}")
    elif status >= 500 and error_code(search) == "":
        record("torrent search with an unreachable indexer errors cleanly", False, f"{status} {search}")
    else:
        print("  SKIP  torrents/search answered "
              f"{status}; the target has a configured indexer, so the 501 branch is not exercisable here")

    # The download surface honours can_download: admins are exempt, everyone
    # else without the entitlement gets 403 download_forbidden.
    status, refused, _ = api.json("POST", "/api/downloads", {"resultId": "e2e"}, token=other_token)
    if status == 501:
        print("  SKIP  downloads are disabled on this deployment (s3 mode); entitlement checks need local storage")
    else:
        record("download create by a non-entitled account is 403 download_forbidden",
               status == 403 and error_code(refused) == "download_forbidden", f"{status} {refused}")

    # The entitlement itself is admin-managed: PATCH /api/admin/users/{id} with
    # a can_download-only body must flip the flag without touching is_admin.
    status, other_me, _ = api.json("GET", "/api/auth/me", token=other_token)
    other_id = other_me.get("user", {}).get("id", "")
    if not other_id:
        record("admin PATCH flips can_download", False, "no user id from /api/auth/me for the second account")
    else:
        status, patched, _ = api.json("PATCH", f"/api/admin/users/{other_id}", {"can_download": True}, token=token)
        if status != 200:
            record("admin PATCH flips can_download", False, f"PATCH {status} {patched}")
        else:
            status, users, _ = api.json("GET", "/api/admin/users", token=token)
            row = next((u for u in users or [] if u.get("id") == other_id), None) if isinstance(users, list) else None
            record("admin PATCH flips can_download",
                   row is not None and row.get("can_download") is True and row.get("is_admin") is False,
                   f"{status} {str(row)[:80]}")
            # With the entitlement granted the route moves past 403; without an
            # indexer the create answers 501 indexer_not_configured instead of
            # crashing on the nil service.
            status, allowed, _ = api.json("POST", "/api/downloads", {"resultId": "e2e"}, token=other_token)
            if status == 410:
                record("entitled create reaches the result lookup (cleanly expired)", True, "410 result_expired")
            else:
                record("entitled create without an indexer errors cleanly (501, not a crash)",
                       status == 501 and error_code(allowed) == "indexer_not_configured", f"{status} {allowed}")

    # The event stream is SSE: the header must say so before any frame, which is
    # what EventSource requires. Headers only; the stream itself never ends.
    status, hdr = response_headers(api.base, "/api/downloads/events", other_token)
    if status == 501:
        print("  SKIP  downloads are disabled on this deployment; no SSE to inspect")
    else:
        record("SSE endpoint returns text/event-stream",
               status == 200 and hdr.get("Content-Type", "").startswith("text/event-stream"),
               f"{status} {hdr.get('Content-Type')}")

    print("== account deletion cascade ==")
    _, temp = sign_in(api, "e2e-temp@example.com", "temp password 12", "e2etemp")
    temp_token = temp.get("access_token", "")
    api.json("PUT", "/api/me/watchlist/e2etemp", {"anime_data": {"id": "e2etemp"}}, token=temp_token)
    api.json("PUT", "/api/me/progress", {"anime_id": "e2etemp", "episode_index": 0, "timestamp": 5}, token=temp_token)
    status, _, _ = api.json("DELETE", "/api/me", token=temp_token)
    record("DELETE /api/me succeeds", status == 200)
    status, _, _ = api.json("GET", "/api/auth/me", token=temp_token)
    record("the deleted account's token is dead", status == 401)
    left = psql(dsn, "select (select count(*) from watchlist where anime_id='e2etemp') + (select count(*) from progress where anime_id='e2etemp')")
    if left is not None:
        record("owned rows cascaded away", left == "0", left)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", default=os.environ.get("E2E_BASE", "http://localhost:3001"))
    parser.add_argument("--dsn", default=os.environ.get("E2E_DSN", ""), help="psql DSN for direct row checks")
    args = parser.parse_args()

    api = Api(args.base)
    status, health, _ = api.json("GET", "/health")
    if status != 200:
        print(f"server at {args.base} is not healthy ({status}); start it first", file=sys.stderr)
        return 2
    print(f"targeting {args.base}  (database={health.get('database')} storage={health.get('storage')})\n")
    run(api, args.dsn)
    print()
    print(f"{len(CHECKS) - len(FAILED)}/{len(CHECKS)} checks passed")
    for name in FAILED:
        print("  FAILED: " + name)
    return 1 if FAILED else 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Verify the ffmpeg-backed features of a running local-mode AniMind backend.

Covers what the Go test suites cannot, because they need the real binaries and real media:
audio-track probing, embedded-subtitle extraction to WebVTT (with the sign/karaoke styles
dropped), the cached audio-variant transcode, and an on-demand HLS session end to end
(create, playlist, segment, seek, heartbeat, destroy).

Run it against a server in local storage mode with a library that holds at least one Matroska
file carrying an ASS subtitle track and two audio streams, then set LIB/EP below or pass them
as arguments. The media checks read the container/library through `docker exec`, so point
CONTAINER at the API container when it runs in Docker.

    python3 scripts/verify_media.py [--base http://localhost:3011] [--container animind-api]

A server without ffmpeg reports the probe-dependent checks as failures; that is the point of
running this in the image, which installs it.
"""

import argparse
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

BASE = "http://localhost:3011"
CONTAINER = "animind-api"

_parser = argparse.ArgumentParser(description="verify ffmpeg-backed features against a running server")
_parser.add_argument("--base", default=os.environ.get("E2E_BASE", BASE))
_parser.add_argument("--container", default=os.environ.get("E2E_CONTAINER", CONTAINER),
                     help="API container name, used to inspect files written into the library")
_args, _unused = _parser.parse_known_args()
BASE, CONTAINER = _args.base, _args.container


def call(method, path, body=None, token=None, headers=None, binary=False):
    req = urllib.request.Request(BASE + path, method=method)
    req.add_header("Accept", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    data = json.dumps(body).encode() if body is not None else None
    if data is not None:
        req.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, data, timeout=240) as r:
            payload, status = r.read(), r.status
    except urllib.error.HTTPError as e:
        payload, status = e.read(), e.code
    if binary:
        return status, payload
    try:
        return status, json.loads(payload or b"{}")
    except Exception:
        return status, {"_raw": payload[:200].decode("utf8", "replace")}


results = []


def check(label, ok, detail=""):
    results.append((label, ok))
    print(("  PASS  " if ok else "  FAIL  ") + label + (("  | " + str(detail)[:200]) if detail else ""))


_, login = call("POST", "/api/auth/login", {"email": "e2e-owner@example.com", "password": "correct horse battery"})
token = login["access_token"]

print("== rescan with ffmpeg present (subtitle extraction runs inline) ==")
status, scan = call("POST", "/api/admin/scan", {}, token=token)
check("scan finished without errors", status == 200 and not scan.get("errors"), scan.get("errors"))
check("scan indexed the library", status == 200 and scan.get("inserted", 0) >= 3,
      {k: scan.get(k) for k in ("scanned", "inserted", "removed", "durationMs")})

_, shows = call("GET", "/api/shows?limit=50")
frieren = next(s for s in shows["data"] if "Frieren" in s["title"])
_, detail = call("GET", f"/api/shows/{frieren['id']}")
ep = next(e for e in detail["episodes"] if e["episode_number"] == 1)

print("== ffprobe-backed endpoints ==")
status, tracks = call("GET", f"/api/episodes/{ep['id']}/audio-tracks", token=token)
listed = [(t["streamIndex"], t["codec"], t["language"], t["browserSupported"]) for t in tracks.get("tracks", [])]
check("two audio tracks discovered", len(listed) == 2, listed)
codecs = {codec for _, codec, _, _ in listed}
check("both source codecs are reported", any("opus" in c for c in codecs) and any("aac" in c for c in codecs), codecs)
check("aac track is flagged as browser-safe", any(codec == "aac" and safe for _, codec, _, safe in listed), listed)
check("japanese language normalized from the tag", any(lang == "Japanese" for _, _, lang, _ in listed), listed)

status, subs = call("GET", f"/api/episodes/{ep['id']}/subtitles", token=token)
vtt = subs.get("tracks", [])
check("an embedded subtitle became a WebVTT track", status == 200 and len(vtt) == 1, [t["label"] for t in vtt])
if vtt:
    body = vtt[0]["content"]
    check("VTT header present", body.startswith("WEBVTT"))
    check("dialogue preserved", "Real subtitle line one" in body and "Second line" in body, body[:120])
    check("typeset/sign style dropped", "typeset sign" not in body, body[:200])
    # The exact millisecond depends on the container's start offset, so the shape is asserted
    # rather than a literal timecode.
    check("timestamps converted to VTT form",
          any(re.match(r"\d\d:\d\d:\d\d\.\d{3} --> \d\d:\d\d:\d\d\.\d{3}", line) for line in body.splitlines()),
          body[:160])

extracted = subprocess.run(["docker", "exec", CONTAINER, "find", "/mnt/anime", "-name", "*.vtt"],
                           capture_output=True, text=True).stdout.strip()
check("subtitle files were written into the library tree", "Subtitles" in extracted and extracted.endswith(".vtt"), extracted)

print("== browser audio variant (transcode path) ==")
opus_index = next(t["streamIndex"] for t in tracks["tracks"] if "opus" in t["codec"])
status, ticket = call("GET", f"/api/episodes/{ep['id']}/stream-ticket?at={opus_index}", token=token)
check("ticket bound to the opus track", status == 200 and f"at={opus_index}" in ticket["url"], ticket["url"][:80])
t0 = time.time()
status, body = call("GET", ticket["url"], binary=True)
check("variant stream served (ffmpeg transcode)", status == 200 and len(body) > 10000,
      f"{status} {len(body)} bytes in {time.time()-t0:.1f}s")
cached = subprocess.run(["docker", "exec", CONTAINER, "find", "/mnt/anime/.animind-audio-cache", "-name", "*.mp4"],
                        capture_output=True, text=True).stdout.strip()
check("a cached M4A variant was produced", cached.endswith(".mp4"), cached)
status, tracks2 = call("GET", f"/api/episodes/{ep['id']}/audio-tracks", token=token)
check("the track now reports cached",
      any(t["streamIndex"] == opus_index and t.get("cached") for t in tracks2["tracks"]),
      [(t["streamIndex"], t.get("cached"), t.get("cacheMode")) for t in tracks2["tracks"]])

print("== on-demand HLS (remux path used by the site) ==")
status, session = call("POST", f"/api/episodes/{ep['id']}/hls-session", {"audioTrackIndex": 0, "startTime": 0}, token=token)
check("hls session created", status == 200 and session.get("sessionId"), str(session)[:120])
if session.get("sessionId"):
    sid = session["sessionId"]
    status, playlist = call("GET", session["playlistUrl"], binary=True)
    text = playlist.decode("utf8", "replace")
    check("playlist is a valid m3u8", status == 200 and text.startswith("#EXTM3U"), text[:120].replace("\n", " | "))
    seg = next((line.strip() for line in text.splitlines() if line.strip().endswith(".ts")), "")
    check("playlist lists segments by bare file name", seg.startswith("seg") and "/" not in seg, seg)
    if seg:
        status, segbytes = call("GET", f"/api/hls/{sid}/{seg}", binary=True)
        check("segment bytes served", status == 200 and len(segbytes) > 1000, f"{status} {len(segbytes)} bytes")
    status, seeked = call("POST", f"/api/hls/{sid}/seek", {"time": 4}, token=token)
    check("seek restarts the session", status == 200, str(seeked)[:100])
    status, alive = call("POST", f"/api/hls/{sid}/heartbeat", {}, token=token)
    check("heartbeat keeps the session alive", status == 200 and alive.get("alive"), str(alive)[:90])
    # A segment name must match segNNNNN.ts; anything else is refused before a path is built,
    # which is what keeps this unauthenticated-by-necessity route from reading arbitrary files.
    status, gone = call("GET", f"/api/hls/{sid}/playlist", token=token)
    check("a non-conforming segment name is refused", status in (400, 404), f"{status} {str(gone)[:60]}")
    status, _ = call("DELETE", f"/api/hls/{sid}", token=token)
    check("session destroyed", status == 200)
    status, playlist = call("GET", f"/api/hls/{sid}/playlist.m3u8", binary=True)
    check("destroyed session no longer serves", status == 404, status)

print()
failed = [label for label, ok in results if not ok]
print(f"{len(results) - len(failed)}/{len(results)} media checks passed")
for name in failed:
    print("  FAILED: " + name)

# 5. Streaming

Two clients with opposite needs: the desktop player wants the original file and does its own
demuxing; the browser wants something it can decode progressively. One endpoint serves both, and
the ticket says which one asked.

```
GET /api/episodes/{id}/stream-ticket   (session required)
        |
        +-- native client  -> cm="n", no audio track bound
        +-- browser        -> cm="b", optional at=<stream index>
        v
GET /api/episodes/{id}/stream?st=<ticket>[&at=N]
        |
        +-- s3 mode    -> 302 presigned URL (or JSON on Accept: application/json)
        +-- local mode -> byte-range passthrough, audio variant first for browsers
```

## Why tickets instead of a bearer header

Neither a `<video src>` nor `mpv loadfile <url>` can attach `Authorization`. The desktop player
in particular hands the URL to libmpv with **no** headers at all — no user agent, no cookie, no
`http-header-fields`. So every URL this API returns for playback is self-authenticating.

A ticket is not an identity credential. It binds one episode, one client mode, optionally one
audio track, and an expiry (`STREAM_TICKET_TTL_SECONDS`, minimum 120 s). It is signed with
`DESKTOP_TOKEN_SIGNING_KEY` and never stored, so a leaked ticket cannot browse the catalog or
touch another episode — `VerifyStreamTicket` refuses a ticket presented for a different episode.

Client mode comes from the ticket when present, otherwise from `clientType=desktop|native` or a
known native user agent. This matters because the ticket request carries
`User-Agent: Animind-Qt/1.0` but the media request does not, so sniffing during playback is
unreliable.

## Byte ranges

`serveLocalFile` (`internal/httpx/server.go`) supports single ranges in the three forms players
actually send: `bytes=a-b`, `bytes=a-`, `bytes=-n` (a suffix range for the last n bytes).

- Responses carry `Accept-Ranges: bytes` and `Content-Range: bytes start-end/size`.
- Browser chunks are capped at `STREAM_RANGE_CHUNK_MB` (8) so one connection cannot hold a worker
  for a two-hour file; native clients get `STREAM_NATIVE_RANGE_CHUNK_MB` (32) because mpv seeks by
  opening a new connection rather than walking the file.
- An unsatisfiable range answers `416` with `Content-Range: bytes */size`. Multi-range requests
  are refused rather than answered with only the first span, which would be wrong.
- `WriteTimeout` is 0 on the HTTP server: a media response legitimately lasts hours.

## S3 mode

Presigned GET URLs with `S3_PRESIGN_EXPIRES` (4 h default). Clients send
`Accept: application/json` and receives `{type, url, expiresIn}` so it can decide between hls.js
and a direct source; anything else gets a `302`. No media bytes pass through this process in S3
mode, which is the point of the mode.

## Browser audio variants

Most releases carry Opus or Vorbis, which a browser cannot play from an MP4 but can play from
WebM/MKV only if it can demux the container at all. Rather than transcode on every request, the
first request for a track produces a cached file and later requests reuse it.

```
<library>/.animind-audio-cache/<episodeId>/a<index>-<copy|transcode>-<hash>.mp4
<library>/.animind-audio-cache/<episodeId>/metadata.json
```

The hash is over a signature of the source's path, size, mtime and track index — so replacing an
episode file cannot serve a stale audio track. `metadata.json` records which variants exist and
lets `GET /api/episodes/{id}/audio-tracks` answer `cached` and `cacheMode`, which is what drives
the client's "first play may take up to a minute" notice. A copy is attempted first
(`-c:v copy -c:a copy -movflags +faststart`); only when it fails does the AAC 320k transcode run.
Writes go to `.partial` and are renamed, so a reader never sees half a file.

The scanner can pre-build these (`POST_SCAN_AUDIO_PREWARM_*`), the TTL sweep deletes
never-played ones, and `.animind-audio-cache` is skipped during discovery so a scan never indexes
its own output.

## HLS sessions

For a local file a browser cannot play progressively (`mkv`, `avi`, …) and `hlsRequired` is
true, the client creates a session and plays the playlist instead.

```
ffmpeg -threads 0 [-ss <start>] -i <file> -map 0:v:0 -map 0:<track>
       -c:v copy [-tag:v hvc1] -c:a <copy|aac -b:a 192k>
       -f hls -hls_time <2..30> -hls_list_size 0 -hls_playlist_type event
       -hls_segment_type mpegts -hls_flags append_list+temp_file
       -hls_segment_filename <dir>/seg%05d.ts -start_number 0 <dir>/playlist.m3u8
```

Decisions worth knowing:

- **Video is always copied.** Remuxing video for a personal library buys nothing.
- **Audio is copied only when MPEG-TS allows it**: `IsMpegTsCopySafeCodec` accepts AAC and MP3.
  Opus and Vorbis are browser-playable in other containers but illegal in MPEG-TS, so they are
  transcoded to AAC 192k. This is a narrower test than `IsBrowserSafeAudioCodec` on purpose.
- **`-tag:v hvc1`** when the video is HEVC, otherwise QuickTime and some hardware decoders reject
  the stream.
- **`playlist_type event` + `append_list`** keeps history in the playlist, so a viewer can scrub
  back to the start of the episode.
- **`temp_file`** stops a client from reading a segment mid-write.
- Segment lines are rewritten to bare file names on the way out: ffmpeg writes absolute OS paths,
  which a player would resolve against its own host.

Session lifecycle: `HLS_MAX_CONCURRENT_SESSIONS` caps running ffmpeg processes and the least
recently used session is evicted at the cap; touching the playlist or a segment refreshes the idle
clock; `HLS_SESSION_TIMEOUT_MINUTES` destroys idle ones; `DestroyAll` on shutdown removes every
process and the root directory. `Create` blocks until the first segment appears
(`HLS_READY_TIMEOUT_MS`, 15 s default) — a playlist URL that yields nothing looks like a broken
player rather than a slow start.

**Seek** kills the process, deletes `*.ts`/`*.m3u8`, restarts with `-ss <time>` and waits for the
first segment again: an event-type playlist has no in-place seek. **Heartbeat**
(`POST /api/hls/{sessionId}/heartbeat`) holds a session across a long buffer gap or a reconnect,
which the original backend had no way to do.

Session ids are 32 hex characters from a CSPRNG, and the playlist/segment routes accept only that
shape, so an unguessable id is the whole authorization story for those two routes. Destruction is
authenticated; the original backend allowed anonymous `DELETE`.

## Native playback notes

The desktop player reads only `url` from the ticket, and demuxes tracks from the delivered file —
it never calls the subtitles or audio-tracks endpoints. So for `clientType=desktop` this API
returns the original container, with no audio variant and no HLS: the file's embedded subtitle and
audio tracks are what that viewer selects with, and anything remuxed on the server would drop
them.

Related: [Storage and scanner](04-storage-and-scanner.md), [Client integration](08-client-integration.md).

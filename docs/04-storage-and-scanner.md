# 4. Storage and scanner

## Two storage modes

| | `STORAGE_MODE=s3` | `STORAGE_MODE=local` |
| --- | --- | --- |
| Discovery | `ListObjectsV2` pagination over the whole bucket | Recursive walk of `LOCAL_STORAGE_PATH` |
| Playback | `302` to a presigned GET URL | Byte-range passthrough from this process |
| Subtitles | Sidecar files only | Extracted to `.vtt` on disk, plus sidecars |
| Audio tracks | Not applicable; the browser demuxes the file | ffprobe listing + cached M4A variants |
| HLS | Not used | On-demand remux for files a browser cannot play |
| Requires | Bucket, credentials, optional endpoint | ffmpeg, a writable library directory |

`episodes.bucket_name` records which bucket a file came from, or the literal `local` — the same
convention the previous schema used, so rows written before this rewrite still resolve.

### S3-compatible endpoints

`S3_ENDPOINT` set to a non-AWS endpoint (R2, DigitalOcean Spaces, MinIO) automatically switches
to path-style addressing, because virtual-host style would resolve to a DNS name that does not
exist. `S3_REGION=auto` is correct for R2.

### Path containment in local mode

`storage.LocalPath()` resolves every path against `LOCAL_STORAGE_PATH` and refuses anything that
does not stay inside it. This runs on every local read — file listing, streaming, subtitles,
audio variants, HLS — so a path stored in the database cannot reach `/etc` or a sibling mount.
The check is on the cleaned, joined result, not on the input string, which is what catches the
`..` and absolute-path variants.

`GET /api/hls/{sessionId}/{segment}` has a stricter rule: the segment name must match
`seg\d{5}\.ts`. A route reachable without a session has to fail closed rather than clean and
hope.

## Scanning

```
list files (video extensions only)
  -> parse each path                     deterministic first, LLM only on request
  -> group by normalized title           so a 200-episode show resolves metadata once
  -> resolve the show                    exact title, AniList id, canonical title, fuzzy, insert
  -> upsert the episode                  on (show_id, season_number, content_type, episode_number)
  -> extract subtitles                   sequential, local mode only
  -> prune                               episodes whose file is gone, shows with no episodes
```

`POST /api/admin/scan`, `POST /admin/library/scan`, `POST /api/webhooks/storage`, cron, and an
optional startup pass all call the same `scanner.Run`. A second concurrent call gets 409.

### Filename parsing

`internal/scanner/parser.go` is a port of the original rules, pattern order included, because
the order is the behaviour:

1. `Title - 01` (hyphen or en-dash)
2. bare `S01E04`
3. `Title S03E01v3` (season captured, release version discarded)
4. `Title Episode 07` / `Title Ep01`
5. `Title 1000` (trailing number)

The enclosing folder beats a filename title whenever it is longer than one character and is not
a bare season marker: `Frieren/Season 1/S01E01.mkv` names the show **Frieren**, not the empty
string that `S01E01` alone would produce. Folder cleanup strips `_`/`.` separators, season
markers, quality and codec tags (`1080p`, `BluRay`, `x265`, `H.264`, …) and the trailing
release group, so
`Re.ZERO...S03.1080p.BluRay.Opus.2.0.x265-Headpatter` becomes `Re ZERO Starting Life in Another
World`.

Season comes from the filename if it carries one, otherwise from the nearest enclosing
`Season N` / `S2` directory, scanning from the file outward. Content type is inferred from the
whole path (`movie|film|theatrical`, `ova|oad`, `ona`, `special|extra`), and a non-`tv` type
settles in season 0. Titles are matched case-insensitively after Unicode normalization and
punctuation removal, which is what keeps `Re:Zero` and `Re Zero` one show.

`TestParseFolderPath` pins these behaviours, including the ones where the parser returns nothing
(a file with no number is not indexed rather than invented).

### Metadata enrichment

AniList is asked first; MyAnimeList answers when AniList throttles or finds nothing. Results are
cached per normalized title for `METADATA_CACHE_TTL_HOURS`, and concurrent lookups for one title
share a single upstream request — a first scan of a 200-episode series would otherwise fire 200
identical GraphQL calls.

That cache is two-level: an in-process map for the scan running now, and, when `REDIS_URL` is
set, the shared store behind it. The second level is what a restart used to throw away — an
interrupted index of a large library would re-query every title it had already resolved, and
AniList's throttle made that the limiting factor rather than ffmpeg. A title no provider matched
is kept in the process only, so a shared "nothing found" cannot outlive the run that learned it.

A 429 sets a process-wide cooldown from `Retry-After` (60 s default), during which AniList is not
called at all; retries use exponential backoff. MyAnimeList is tried once per title with two
attempts.

Show metadata is written with a fill-only statement: existing synopsis, artwork, genres, rating,
studio, status, year and AniList id are **never overwritten**. A show row missing any of them is
re-enriched on the next scan.

### LLM fallback

Off unless `SCANNER_OPENROUTER_FALLBACK=true` and `OPENROUTER_ENABLED=true`. It is consulted only
when deterministic parsing produced nothing, and its answer is accepted only at or above
`OPENROUTER_MIN_CONFIDENCE` (0.72). It is asked with a strict JSON schema, `temperature: 0`, the
file's path, up to 40 sibling file names, and no title it should invent: a wrong title
silently corrupts the catalog, so declining to guess is the better failure. The row records
`scanner_source`, so a title that came from a model is distinguishable later.

### Subtitle extraction

For each embedded subtitle stream that is text-based, this writes
`<Show>/Subtitles/[Season NN/]Episode NN/<Language>.vtt`, skipping work when the folder already
holds subtitles and skipping read-only directories (probed by writing and removing a file, since
mode bits lie on ACL-backed mounts). ASS/SSA is converted in-process rather than by ffmpeg's own
muxer, dropping styles matching signs, karaoke, openings, credits and typesetting, plus drawing
commands and cues that are only digits or punctuation. Duplicate language tags become
`English.2.vtt`. Image-based streams (`hdmv_pgs_subtitle`, `dvd_subtitle`, `xsub`, `dvb_*`) are
skipped: they cannot become timed text.

Extraction runs sequentially per file, not concurrently, and at `nice` priority on POSIX:
several ffmpeg processes competing with playback is the failure mode viewers notice.

### Pruning

Pruning is derived from the listing, so it is correct after any interleaving: rows whose
`file_path` is absent from the current listing are deleted, then shows that no longer own any
episode are deleted. A partially failed listing therefore cannot mass-delete the catalog,
because `ListVideoFiles` returns an error rather than a short list.

### Prewarm and cache TTL

`POST_SCAN_AUDIO_PREWARM_*` builds browser-playable variants for the tracks a browser could not
play, Japanese audio first, capped at `..._EPISODE_LIMIT` episodes and
`..._MAX_TRACKS_PER_EPISODE` tracks each. `AUDIO_CACHE_VARIANT_TTL_DAYS` deletes variants nobody
played recently, rewrites `metadata.json` so it never references a removed file, and removes
empty episode directories. Both are local-mode only.

Related: [Streaming](05-streaming.md), [Deployment](07-deployment.md).

# Changelog

Notable changes to AniMind Backend (Go). The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

**There are no tagged releases yet.** Everything below is unreleased work on `main`.
The sections exist so that changes land in the right place from now on rather than being
reconstructed from 60 commits afterwards.

## [Unreleased]

### Added

- **Torrent search.** Torznab indexers, a search pipeline with ranking, opaque result ids,
  a bounded client and a trending feed.
- **Downloads.** A job manager with per-user and global quotas, a state machine, resume on
  startup, file selection, an importer that writes selected files into the library, and a
  per-user Server-Sent-Events hub with snapshot and coalescing. Route surface:
  `POST/GET/PATCH/DELETE /api/downloads`, `GET /api/downloads/{id}`, `/api/downloads/events`.
- `migrations/0003_downloads.sql`, the download-job repository, and a Prometheus row plus a
  live verification script for the whole subsystem.
- Shared auth cache and observability; AniList response caching.
- A browser-facing Google entry point and app-link return targets.
- Password accounts can change their email address.

### Changed

- Line endings are pinned to LF via `.gitattributes`, so a Windows checkout no longer makes
  every file look unformatted to `gofmt`.
- Watch-party group starts are scheduled rather than immediate, and the buffer gate was
  hardened; drift corrections and private packets are now stamped.
- The watch-party snapshot publishes the parent show, not only the episode.
- Proxy and production defaults were locked down.

### Fixed

- Torrent clients can pin their listen address; port mapping is disabled.
- Torrents at terminal states stop, and seeding knobs are clamped.
- SSE subscribers close on shutdown.
- Imports are contained by symlink and root, and resume idempotently.
- Unselected packs resume into selection, and job ownership is enforced on every path.
- Nil-service wiring, admin patch semantics and typed duplicate errors in the download
  manager.
- The scanner no longer fabricates subtitle tags, and batch range separators are trimmed
  from delegated titles.
- OAuth and login hardening; polling events are authenticated.

### Documentation

- Comments that described behaviour the code does not have were corrected, including one
  claiming a packet-retransmit path that is not implemented and one attributing path-style
  addressing to a field nothing reads.

## Policy for this file

One line under `Unreleased` per user-visible change, added in the same pull request as the
code. Cosmetic internal refactors do not need an entry. When a version is tagged, promote
`Unreleased` to a dated section and start a new one.

[Unreleased]: https://github.com/FNXDOOM/Animind_Desktop_Backend/commits/main

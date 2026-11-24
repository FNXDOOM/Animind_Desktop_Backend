# Business-Logic Audit

Business logic is where this codebase is most interesting: it has real state machines (download
jobs, watch parties, HLS sessions), quotas, entitlements and a multi-step identity flow. This
section maps the intended rules, the implementation, and the gaps.

---

## 1. Business rules inventory

| # | Rule (as stated or implied by the code/docs) | Implementation | Status |
|---|---|---|---|
| BR-01 | Only an admin may list users, change admin/`can_download`, delete shows, scan, view admin metrics or clean caches | `RequireAuth` + `RequireAdmin` on every `/api/admin/*` and `/api/rescan` | **Holds** |
| BR-02 | A user may read/write only their own watchlist, progress, history, preferences, sessions and download jobs | SQL `where user_id = $sessionUser`; handlers re-check download ownership | **Holds** |
| BR-03 | Starting and deleting a download requires `can_download`, unless admin | `canDownload` before create/delete; admins exempt | **Holds** (lookup is inefficient, F-17) |
| BR-04 | A user may have at most `MaxActivePerUser` active jobs and the server at most `MaxActive` | Count-then-insert in `Manager.Create` | **Raceable** (F-16) |
| BR-05 | A job's total size must fit `MaxBytesPerJob` and leave `MinFreeBytes` headroom | pre-flight size check + `statfsFree` | Holds at admission; disk can change later |
| BR-06 | The same torrent cannot be queued twice while active | `FindDownloadJobByHash` + partial unique index | **Holds at rest; race returns 500 instead of 409** (F-16) |
| BR-07 | A download job follows the declared state machine; illegal transitions are refused | `canTransition` + CAS `TransitionDownloadJob(from,to)` | **Holds** |
| BR-08 | Pausing stops transfers; resuming restarts the drive loop from the paused state | `Pause`/`Resume` + `launch` | **Holds** |
| BR-09 | Cancelling a live job transitions it to cancelled; cancelling a terminal job deletes the row | `Cancel` | **Holds** |
| BR-10 | `deleteFiles=true` removes the job directory and re-scans the library | `removeJobDir` + `rescanAfterDelete` | **Holds** |
| BR-11 | Imported files never overwrite an existing library file | `moveInto` checks `Lstat(dst)` and returns `import_conflict` | **Holds** |
| BR-12 | Import moves only allowlisted video/subtitle extensions and permits `MaxFiles` | `SelectableFiles` | **Holds** |
| BR-13 | A torrent file path may not escape the job directory | importer `ensureSourceInsideJobDir` + `escapesTorrentRoot` | Holds at import; download-time writes depend on anacrolix (P-02) |
| BR-14 | The library scanner is single-flight; a second scan answers 409 | `scanner.running` mutex | Holds per process (not across replicas) |
| BR-15 | A rescan requires admin; the webhook requires the shared secret | route middleware + constant-time compare | **Holds** |
| BR-16 | An HLS session is capped by `MaxConcurrentSessions`, evicts LRU, and expires when idle | manager mutex, `evictIfNeeded`, idle sweep | Mostly holds; concurrent `Create` can exceed the cap briefly |
| BR-17 | An HLS session id is unguessable and minted only for a signed-in user | 16 random bytes; `Create` route has `RequireAuth` | **Holds** |
| BR-18 | Only the session creator may seek/destroy it | no owner field; only `Has(sessionId)` | **Broken** (F-08) |
| BR-19 | A watch party is limited to `MaxParticipants` and one socket per participant | room mutex; participant map keyed by socket id | **Holds** (a user can open several sockets; each counts separately) |
| BR-20 | A peer must have joined the room before it can control playback | `ensureParticipant` | **Holds** (joining is the only gate) |
| BR-21 | A room code uniquely identifies a live party and is hard to guess | 5 chars from 32 alphabet; collision retry | **Weak** (F-09) |
| BR-22 | Host leaves → earliest remaining participant is promoted | `handlePeerLeft` sorted by join order | **Holds** |
| BR-23 | Room state is persisted; a restart marks stale active rows ended | `CreateParty`/`EndAllActiveParties` | **Holds** |
| BR-24 | The site session is revocable from the security screen | `RevokeSession` scoped by user | **Holds** |
| BR-25 | Password change / reset / email change invalidates stolen sessions | web sessions revoked; desktop sessions not | **Broken** (F-01) |
| BR-26 | A desktop refresh token is single-use and rotation invalidates its predecessor | hash replaced in place | **Holds for the pair; no reuse detection** (F-11) |
| BR-27 | Password reset token is single-use and expires in one hour | `ConsumeAuthToken` DELETE … RETURNING | **Holds** |
| BR-28 | OAuth state is signed, single-use, browser-bound and expires in 10 minutes | signed claims + cache `Take` + nonce cookie | **Holds** |
| BR-29 | Google sign-in only accepts a verified email | `validateGoogleProfile` requires `email_verified=true` | **Holds** (the local side is broken, F-04) |
| BR-30 | Signup creates an account for the address the user controls | no verification | **Broken** (F-04) |
| BR-31 | A user can be in at most one party at a time | `joinRoom` moves the peer out of its previous party | Holds for one socket; multiple sockets = multiple parties |
| BR-32 | A download event stream is per-user and capped | `Subscribe(userID)` with 2 per user, 100 total | **Holds** |
| BR-33 | Search results are per-query and expire | result store keyed by opaque id | Holds; result ids are not a security boundary |
| BR-34 | Trending counts contain no user identity | counter stores query text only | **Holds** |
| BR-35 | A storage webhook answers before scanning and scans in the background | handler + goroutine | **Holds** |
| BR-36 | An admin cannot remove their own admin flag | self-demote guard | **Holds** |
| BR-37 | Account deletion cascades all owned rows and sessions | FK `on delete cascade` | **Holds** |
| BR-38 | The last admin cannot be removed through the UI | no such guard beyond self-demote | **Gap (operational)** — an admin can demote the last *other* admin and leave the instance unmanageable |
| BR-39 | A user's email change requires the current password | `ChangeEmail` verifies argon2 hash | **Holds** (no confirmation mail; F-01/F-04) |
| BR-40 | An account with a provider-only identity cannot use password change | `PasswordHash == nil` → `ErrPasswordNotSet` | **Holds** |
| BR-41 | A session row records where it was created (IP, user agent) | `SignInUser` reads context client info; `WithClient` is never attached on the auth routes | **Broken** (F-22) |

---

## 2. Session lifecycle — the main business-logic weakness

The intended rule is "when the credential that protects an account changes, every credential
derived from it dies." The implementation is asymmetric:

| Event | Web sessions | Desktop access token | Desktop refresh token | Credential cache |
|---|---|---|---|---|
| Logout | caller revoked | not revoked (by design) | not revoked | user group invalidated |
| Password change | **all revoked** | **survives** | **survives** | invalidated |
| Password reset | **all revoked** | **survives** | **survives** | invalidated |
| Email change | all except caller | **survives** | **survives** | invalidated |
| Session revoke (security screen) | target revoked | target revocable | target revocable | invalidated |
| Account delete | cascade | cascade | cascade | invalidated |
| Desktop refresh | n/a | replaced | replaced | invalidated |
| Desktop revoke | n/a | revoked | revoked | invalidated |

This is F-01. It is a business-rule failure, not a missing middleware: the web path proves the
team knows the rule; the desktop path was simply not included.

**Recommended rule:** a password change/reset and an email change are "credential reset" events.
They must revoke **all** session families (web + desktop), invalidate the cache, and notify the
account out-of-band. Keep the caller's current web session only if the UX requires it; do not
keep any desktop session, because the desktop has no way to prove the user is the same person.

## 3. Download quota and state machine

### 3.1 Quota TOCTOU (F-16)

```
Request A                          Request B
count(user) = 2 (< 3)              count(user) = 2 (< 3)
count(all)  = 1 (< 2)              count(all)  = 1 (< 2)
free disk OK                       free disk OK
insert job A                       insert job B
=> 3 active for a limit of 3?      => both pass; now 4 active for a limit of 3
```

The partial unique index only protects identical `(user_id, info_hash)` rows. Two different
torrents both pass.

**Fix pattern:**

```sql
begin;
select pg_advisory_xact_lock(hashtext($userID));
-- re-count inside the transaction
insert into download_jobs (...) values (...);
commit;
```

or a single statement that includes the count predicate. Map `23505` to the existing
`DuplicateJobError` so a concurrent identical create returns 409, not 500.

### 3.2 State transitions

`canTransition` and the CAS on `(id, from)` are well designed. A pause/cancel racing a drive-loop
hop cannot resurrect a job: the CAS fails and the loop exits silently. The `drive` loop also
re-reads the row each iteration, so a user action wins. No finding.

### 3.3 Import

- Destination is `<libraryRoot>/<sanitised show>/Season NN`.
- `sanitizeTitle` strips separators, control characters and trailing dots; `windowsDeviceName`
  rejects `CON`, `NUL`, `COM1`, etc.
- `moveInto` never overwrites; a present destination with a present source is
  `import_conflict`; a present destination with a missing source is treated as a completed crash
  recovery.
- Symlinks are resolved before the move and must stay inside the job directory.
- **Residual:** the same show title from two concurrent imports can race on `MkdirAll`/`Rename`;
  the conflict path returns an error and the job fails rather than corrupting a file. Acceptable.

## 4. Watch-party logic

- Room creation validates the episode exists, mints a code, records the party, and sets the
  creator as host.
- Join is gated only by the code; it re-gates playback so the newcomer can buffer.
- Every control event calls `ensureParticipant`; a peer that has not joined is silently ignored.
- Host departure promotes the earliest remaining participant.
- Play/pause/seek are broadcast with a server timestamp; the gate schedules a future start.
- The room is deleted when empty; the DB row is marked ended; a scheduler cleans ended rows.

**Business-logic gaps:**

1. **Code strength / join throttling (F-09).** The code is the entire authorization model.
2. **Multiple sockets per user.** `MaxParticipants` counts sockets, not accounts; one user can
   occupy all 12 slots, or join 12 different parties simultaneously. This is a fairness issue,
   not a security boundary, but it enables a denial-of-party.
3. **No host-only actions.** Any participant can seek/change episode; the host role is mostly
   informational. If the product intends host control, the handlers should enforce it.
4. **No invitation/approval.** The code is shareable and reusable; there is no expiry on a live
   room beyond the process lifetime and the cleanup TTL.

## 5. HLS sessions

- Cap 2 global sessions, LRU eviction, idle timeout 120 min, first-segment wait 15 s.
- `Create` starts ffmpeg, registers the session, waits for the first segment, and destroys the
  session on failure.
- `Seek` kills the process, clears the directory, restarts ffmpeg at the new offset.
- **Race:** two simultaneous `Create` calls can both observe `len(sessions) < max` before either
  inserts, briefly exceeding the cap. `evictIfNeeded` re-checks under the lock after eviction and
  returns `ErrTooManySessions` only if still full, so the failure is bounded; the cap can still be
  exceeded by the number of concurrent creators. Low.
- **Authorization (F-08):** session id is the only key for seek/destroy/heartbeat.
- **Seek cost:** an authenticated user can call seek every second; each call kills and restarts
  ffmpeg. The route is limited to 60/min, but that is still 60 process spawns/min. Consider a
  per-session seek debounce.

## 6. Identity and account linking

The intended rule is "one account per email; a provider identity attaches to the account that
owns that email." The implementation matches on normalized email, but the email was never
verified at signup. That inverts the trust: the first person to claim the string wins, and the
real owner's Google login attaches to the squatter's account (F-04).

A safe rule set:

1. Signup → `pending_verification`, no session, verification mail.
2. Verification → `active`.
3. Provider sign-in:
   - existing **verified** account with that email → link after a confirmation to that address
     (or require the password once);
   - existing **unverified** account → do not auto-link; complete the verification flow first;
   - no account → create an active account (Google already verified the address).
4. Never allow a password login on an unverified address.

## 7. Entitlements and admin

- `can_download` defaults false; the first admin is a manual SQL grant (documented).
- Admins bypass the entitlement; they can also grant it to themselves/others.
- `handleAdminSetUserAdmin` prevents self-demotion but not demoting the last other admin. On a
  single-admin instance, an admin can lock themselves out by demoting the only other admin and
  then being unable to promote anyone. **Operational gap**, not a security vulnerability; add a
  "cannot demote the last admin" check if the instance has more than one.
- The entitlement read is a full-table page scan (F-17); replace it with a single-row query.

## 8. Search, trending and cache logic

- Torznab query is validated (2–120 runes, enum filters, integer bounds) and `url.Values`-encoded.
- Results are ranked, magnets are stripped from the wire shape, and the opaque result id resolves
  server-side at download creation. A caller cannot inject an arbitrary magnet.
- Trending counts query text only; no identity.
- AniList proxy normalizes the query for cache identity; response size is the only unbounded
  dimension (F-03).

## 9. Business-logic false positives

| Suspicion | Why it is not a bug |
|---|---|
| Any user can stream any episode | The catalog is intentionally shared; the product has no per-title entitlement |
| Admin can see every download job with `all=true` | Explicit admin feature; scoped by `IsAdmin` |
| A user can download their own job's files twice | `Cancel` on a terminal row deletes it; second call 404 |
| A paused job can be resumed by an admin | Admin bypass is documented and deliberate |
| Search results from another user are usable | Results are not user-scoped, but the magnet is server-held and any entitled user may download the same public torrent |
| HLS segment is unauthenticated | Session id is 128-bit random and only minted by an authenticated call; segment name is regex-validated |
| Multiple sessions per account | Intended: one per browser/device; the security screen lists and revokes them |
| `remember_me` extends a web session to 30 days | Explicit user choice; revocation still works |

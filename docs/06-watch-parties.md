# 6. Watch parties (SyncPlay)

A watch party is shared playback with one authoritative clock. The server does not decode video;
it decides *when* everyone starts and corrects anyone who drifts.

## Transport

Socket.IO v4 over `/api/socket.io`, implemented in `internal/syncplay` without a Socket.IO
dependency: only the subset the feature needs is maintained. Engine.IO v4 framing is honoured
exactly, so an unmodified `socket.io-client` connects.

| | |
| --- | --- |
| Path | `/api/socket.io` (mounted under `/api` so one reverse-proxy rule covers API and socket) |
| Protocol | Engine.IO 4, Socket.IO 4, root namespace `/` |
| Transports | WebSocket first (the site requests `['websocket','polling']`); long-polling with upgrade is also implemented |
| Auth | `auth: { token }` on the Socket.IO CONNECT packet — nothing else is read: not a query parameter, not a cookie |
| Liveness | Server-sent Engine.IO ping every `SYNCPLAY_PING_INTERVAL_SECONDS` (25 s), pong deadline `SYNCPLAY_PONG_TIMEOUT_SECONDS` (40 s) |
| Message cap | `SYNCPLAY_MAX_MESSAGE_BYTES` (64 KiB) on the read limit and the polling body |

Handshake failures are answered with a Socket.IO `CONNECT_ERROR` packet carrying
`{"message": "Authentication required."}` or `{"message": "Invalid token."}` — the two strings
the deployed client expects. The token is resolved with the same rules as the HTTP API:
`adk_` desktop tokens and `as_` site sessions only. There is no Clerk or Supabase verification
path any more.

Identity is per socket: `participants` is keyed by socket id, so one user on two tabs is two
participants, and the ready gate waits on both. `joinRoom` answers with a participant list
de-duplicated by user id alongside a socket-based count, which is why those two numbers can
differ.

## Room codes

Five characters from `ABCDEFGHJKLMNPQRSTUVWXYZ23456789` — 32 symbols, excluding `I`, `O`, `0`, `1`
because users mistype them. Drawn from a CSPRNG, re-drawn on collision with a live room, uppercased
on entry. `GET /api/syncplay/rooms/{code}` lets a client check a code before asking the user to
open the matching episode.

## The ready gate

Any change that could make peers disagree — play, seek, a new joiner, a drift correction above
`SYNCPLAY_SPEED_SYNC_MAX_MS` — opens a gate:

```
initiateBufferGate(playAfterReady)
  1. clear timers, readyPeers, stallingPeers; isPlaying = false; waitingForReady = true
  2. broadcast waitForBufferGoal {currentTime, bufferGoalSeconds, peers, totalPeers, readyCount, sentAt}
     and waitForReady (same, minus bufferGoalSeconds) — both, for older clients
  3. arm a per-peer force-ready timer (SYNCPLAY_READY_TIMEOUT_MS, 12 s)
  4. arm one escape timer (SYNCPLAY_ESCAPE_WAIT_MS, 10 s)
```

A peer becomes ready by any of: sending `ready`; reporting `bufferedSeconds >= 
SYNCPLAY_BUFFER_GOAL_SECONDS` (120 s) in `bufferingProgress`; its force-ready timer firing
(announced with `peerReady` and `timedOut: true`, the only place that flag appears); or the escape
hatch. When `readyPeers` reaches `participants`, the gate closes with `syncPlay` if play was
intended, otherwise `syncPaused`.

The escape hatch is the honesty mechanism: at 10 s, if any peer has buffered the goal **or** any
peer is already ready, everyone is readied at once. Without it a single slow connection holds the
party hostage; the trade is that some peers resume under-buffered and may stall shortly after,
which the stall path then handles.

`pause` cancels an open gate outright — otherwise the room would resume playing after the
stragglers finished buffering. A peer leaving mid-gate lowers the bar and re-attempts the close, so
the room cannot wait forever on a socket that is gone.

## Clock and drift

Only the host's heartbeat moves the canonical time, and only while playing. Every heartbeat also:
broadcasts `participantStates` to the room and evaluates that peer's drift.

| Drift | Correction |
| --- | --- |
| < 200 ms | nothing |
| < 500 ms | `softCorrect {currentTime}` — the client seeks quietly |
| < 10 000 ms | `speedSeek {rate, duration, targetTime}` — rate 1.5 if behind, 0.8 if ahead, for `min(drift*2, 8000)` ms |
| >= 10 000 ms | full re-gate of the room, with play intent forced on |

Speed correction is used before seeking because a seek costs a buffer refill for everyone watching
that peer; rate manipulation closes a few seconds of gap silently.

Peers also reconcile their own clocks with `timesync_ping` / `timesync_pong`, which carries
`serverTime` so the client can schedule a start at a shared future instant rather than reacting to
a broadcast it received late.

## Stalls

Two independent detectors:

1. The client sends `buffering` (debounced at 600 ms client-side) → `peerStalling` to the room, and
   a `SYNCPLAY_STALL_GRACE_MS` (5 s) timer starts. If the peer has not sent `stallRecovered` when it
   fires, the whole room re-gates with play intent.
2. Heartbeat-based: a peer whose position moves less than 0.1 s for `SYNCPLAY_STALL_DETECT_MS`
   (2 s) while the room is playing is declared stalling, entering the same grace path. Recovery is
   automatic when its position starts advancing again (`peerStallRecovered`).

## Host roles

Host-only actions are `transferHost` and `changeEpisode`; anything else a participant can do is
accepted from anyone (dual control, as shipped). Violations answer `syncDenied` with one of three
reasons: `Only the current host can transfer control.`, `Target user is not in the room.`,
`Only the host can change the episode.`

Transfer picks the first socket belonging to the target user id. On host disconnect the earliest
remaining participant by join order is promoted and everyone hears `hostChanged`. There is no
timeout that demotes a silent host: a host whose heartbeats stop but whose socket stays open keeps
authority and freezes the clock. That is a known limitation of this design rather than something
this port introduced.

`changeEpisode` resets the position to 0, clears buffers and readiness so a stale report from the
previous episode cannot satisfy the next gate, and leaves the room paused until someone plays.

**Leaving is disconnecting.** There is no `leaveRoom` event, because the deployed clients have
none — `leaveSyncPlay` just calls `disconnect()`. Adding one would require a client change first.

## Event reference

Client → server (`createRoom` and `joinRoom` are the only two that request an ack):

| Event | Payload |
| --- | --- |
| `createRoom` | `{ episodeId }` |
| `joinRoom` | `{ roomCode }` |
| `play` / `pause` | `{ currentTime }` |
| `seek` | `{ time }` |
| `ready` / `buffering` / `stallRecovered` / `requestSync` | – |
| `bufferingProgress` | `{ bufferedSeconds, percent }` |
| `heartbeat` | `{ currentTime, playbackRate?, bufferedAhead? }` |
| `transferHost` | `{ targetUserId }` |
| `changeEpisode` | `{ episodeId }` |
| `timesync_ping` | `{ clientSendTime }` |

Server → client: `waitForBufferGoal`, `waitForReady`, `peerReady`, `syncPlay`, `syncPaused`,
`pause`, `seek`, `softCorrect`, `speedSeek`, `peerStalling`, `peerStallRecovered`, `peerJoined`,
`peerLeft`, `participantStates`, `bufferingUpdate`, `hostChanged`, `syncDenied`, `episodeChanged`,
`timesync_pong`, `sync`. Payload shapes, and which peers receive each (all, others, or just the
caller), are documented in `internal/syncplay/room.go` beside each emit.

`syncPlay` for a solo room is sent only to the caller, and a solo `seek` uses the field name `time`
while `pause` uses `currentTime` — both asymmetric with each other, and both preserved because the
deployed client reads them literally.

Each peer entry in `waitForBufferGoal`, `waitForReady` and `participantStates` carries
`socketId`, `userId`, `displayName`, `currentTime`, `playbackRate`, `readyState`, `bufferedAhead`,
`ready` and `stalling`. The site's roster and ready-badges read all of them, so the two identity
fields are part of the contract, not debug detail — `TestWatchPartyHandshakeAndGate` asserts they
survive the JSON round trip.

## Coverage

The state machine is exercised over real websockets against a real database by
`internal/syncplay/rooms_functional_test.go`: create-room validation and its refusal of an
unknown episode, joining by a lower-case code, the gate both peers must see, the buffer reports
that close it, `syncPlay` carrying the host's position, `participantStates` after a heartbeat, and
`peerLeft` when a socket dies. `decideDrift` is tested as a pure ladder
(`drift_test.go`) so the four correction bands and the duration cap are pinned without timing.
Framing, ack ids and batched packets are in `protocol_test.go`.

## Persistence and cleanup

In-memory state is authoritative; rows in `watch_parties` and `watch_party_participants` are a
best-effort audit trail. A failed insert warns and the party continues — refusing to let someone
watch because an audit write failed is the wrong trade.

At startup every row still marked `active` is ended: live sockets did not survive the restart, so
such a row is a stale record. `SYNCPLAY_CLEANUP_CRON` (every 10 minutes) deletes rooms that ended
more than `SYNCPLAY_ENDED_TTL_MINUTES` (60) ago along with their participant rows, in one
transaction so participants never outlive their room.

## Known limitations

- A restart ends every party; there is no socket rejoin with retained state.
- Only the host advances the canonical clock, so a stalled host freezes it.
- Room capacity is `SYNCPLAY_MAX_PARTICIPANTS` (12) per room, enforced on join.
- Cross-episode joins are not validated against the room's episode: a member can join a code while
  playing something else. The site's UI never does this today; `changeEpisode` and
  `episodeChanged` exist so a room can be steered, and the desktop player has no watch-party code at
  all.

Related: [Architecture](01-architecture.md), [API reference](03-api-reference.md).

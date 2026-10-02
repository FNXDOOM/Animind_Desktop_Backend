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
  1. clear timers, readyPeers, stallingPeers; isPlaying = false; waitingForReady = true;
     bump gateEpoch
  2. count every member with ignoreWait as ready at once — opting out is a standing preference,
     not a one-gate favour
  3. broadcast waitForBufferGoal {currentTime, bufferGoalSeconds, peers, totalPeers, readyCount,
     groupState, waitDeadlineMs, sentAt} and waitForReady (same, minus bufferGoalSeconds) — both,
     for older clients
  4. arm a per-peer force-ready timer (SYNCPLAY_READY_TIMEOUT_MS, 12 s)
  5. arm the escape timer (SYNCPLAY_ESCAPE_WAIT_MS, 10 s) and the hard wait deadline
     (SYNCPLAY_WAIT_DEADLINE_MS, 30 s)
```

A peer becomes ready by any of: sending `ready`; reporting `bufferedSeconds >= goal` in
`bufferingProgress`; its force-ready timer firing (announced with `peerReady` and
`timedOut: true`); opting out; or the escape hatch. When `readyPeers` reaches `participants` the
gate closes with `allReady` and then `syncPlay` if play was intended, otherwise `syncPaused`.

The goal is `min(SYNCPLAY_BUFFER_GOAL_SECONDS, SYNCPLAY_BUFFER_GOAL_FRACTION * seconds remaining)`
with a 10 s floor: a fixed 120 s asks for the entire file during the last two minutes of every
episode, which turned the end of each episode into a permanent gate.

The escape hatch is the honesty mechanism: at 10 s, if any peer has buffered the goal **or** any
peer is already ready, everyone is readied at once. Without it a single slow connection holds the
party hostage; the trade is that some peers resume under-buffered and may stall shortly after,
which the stall path then handles. The deadline is the same argument taken to its conclusion: at
30 s the gate closes whatever the buffers look like, because waiting longer has already cost more
than it can save.

`gateEpoch` invalidates timers that cannot be stopped. A `time.AfterFunc` callback that has
already begun running survives `Stop()`, so without the epoch a force-ready from a closed gate
could grant readiness into the gate that replaced it.

`pause` cancels an open gate outright — otherwise the room would resume playing after the
stragglers finished buffering. A peer leaving mid-gate lowers the bar and re-attempts the close, so
the room cannot wait forever on a socket that is gone.

## Clock and drift

Only the host's heartbeat moves the canonical time, and only while playing. Every heartbeat also:
broadcasts `participantStates` to the room and evaluates that peer's drift.

| Drift | Correction |
| --- | --- |
| < 200 ms | nothing |
| < 500 ms | `softCorrect {currentTime, fromUserId, driftMs}` — the client seeks quietly |
| < 10 000 ms | `speedSeek {rate, duration, targetTime, fromUserId, driftMs}` — rate 1.5 if behind, 0.8 if ahead, for `min(drift*2, 8000)` ms |
| >= 10 000 ms | full re-gate of the room, with play intent forced on |

Speed correction is used before seeking because a seek costs a buffer refill for everyone watching
that peer; rate manipulation closes a few seconds of gap silently. Both packets name the clock they
were measured against (`fromUserId`) and how far off they found the peer (`driftMs`): a correction
that arrives without attribution reads as a glitch, while one that says who fell behind reads as
the group working. A member that opted out of the wait is told how far off it is and never
steered — that is what the opt-out means.

Every broadcast carries `seq` (the room's monotonic sequence number, assigned at emit time) and
`serverTime`. A client that orders by arrival alone will apply a stale pause after the play that
overtook it; the pair makes that detectable, and the desktop client drops anything at or below the
last sequence number it applied.

### Scheduled starts

A group play, unpause or seek is not an instruction to act; it is an instruction to act **at**
something. The hub computes

```
scheduledPlayAt = now + clamp(2 x highest member pingMs, SYNCPLAY_START_LEAD_MIN_MS,
                                                SYNCPLAY_START_LEAD_MAX_MS)
```

— the floor matters more than the ceiling: a room of locally connected peers has a near-zero ping,
and scheduling nothing would put everyone back on "act when the packet lands". `pingMs` is
reported by clients on `heartbeat`, `play`, `pause`, `seek` and `ping`, and clamped to
`SYNCPLAY_MAX_PING_MS` so no client can push a room's start arbitrarily far out.

The field is named `scheduledPlayAt` because that is what the deployed web client already parses
(`msUntilServerTime` in `services/syncplay.client.ts`), which means the site gains scheduled starts
without a release.

### Clock service

`timesync_ping` is answered with four stamps: `clientSendTime` echoed, `serverReceiveTime` read on
the first line of the handler before any work, and `serverTime` at the moment of the answer. With
only two, offset and round trip collapse into one number and every estimate inherits response
latency as clock error; `serverReceiveTime` is what lets a client tell a slow link from a wrong
wall clock. Clients report their own conclusion with `timesync_offset {offsetMs, pingMs}`, which
the hub publishes per peer in `participantStates`.

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

`changeEpisode` (host only, and the target episode must exist in the library) resets the position
to 0, clears buffers and readiness so a stale report from the previous episode cannot satisfy the
next gate, re-reads the episode duration for the scaled goal, and opens a gate: every member has to
load a different file, which is exactly the condition the gate exists for.

**Leaving is disconnecting.** There is no `leaveRoom` event, because the deployed clients have
none — `leaveSyncPlay` just calls `disconnect()`. Adding one would require a client change first.

## Event reference

Client → server (`createRoom` and `joinRoom` are the only two that request an ack):

| Event | Payload |
| --- | --- |
| `createRoom` | `{ episodeId }` |
| `joinRoom` | `{ roomCode }` |
| `play` / `pause` | `{ currentTime, pingMs? }` |
| `seek` | `{ time, pingMs? }` |
| `ready` / `buffering` / `stallRecovered` / `requestSync` | – |
| `bufferingProgress` | `{ bufferedSeconds, percent }` |
| `heartbeat` | `{ currentTime, playbackRate?, bufferedAhead?, pingMs?, durationSeconds? }` |
| `ping` | `{ pingMs }` — a latency report outside a heartbeat, for a paused room |
| `timesync_offset` | `{ offsetMs?, pingMs? }` |
| `ignoreWait` | `{ ignore }` — stop holding this member at gates (or resume) |
| `transferHost` | `{ targetUserId }` |
| `changeEpisode` | `{ episodeId }` |
| `timesync_ping` | `{ clientSendTime }` |

Server → client: `waitForBufferGoal`, `waitForReady`, `allReady`, `peerReady`, `syncPlay`,
`syncPaused`, `pause`, `seek`, `softCorrect`, `speedSeek`, `peerStalling`, `peerBuffering`,
`peerStallRecovered`, `peerJoined`, `peerLeft`, `peerIgnoreWait`, `participantStates`,
`bufferingUpdate`, `hostChanged`, `syncDenied`, `episodeChanged`, `timesync_pong`, `sync`.
Payload shapes, and which peers receive each (all, others, or just the caller), are documented in
`internal/syncplay/room.go` beside each emit.

`allReady` and `peerBuffering` are named for what the site already listens for
(`services/syncplay.client.ts`); until now the hub never sent either, so that code path was dead.
`peerBuffering` is the mid-play counterpart of a gate and is announced once per fall below the
goal, not once per report.

### Tuning published to clients

The `createRoom` / `joinRoom` acks and `sync` carry `syncConfig`: `version`, `features` (a
capability list such as `scheduledStart`, `broadcastSeq`, `timeSyncReceive`, `ignoreWait`), and
the gate / start / correction / drift / timeSync / limits blocks. A threshold that lives there is
changed by redeploying the server; one baked into a client needs a release, which is how a bad
number becomes permanent. The desktop client reads it whole (`SyncPlayClient::applySyncConfig`).

Each peer entry also carries `pingMs`, `clockOffsetMs`, `ignoreWait` and `driftMs` — published,
never acted on, so a human can see what the server sees.

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

`TestWatchPartyScheduledStart` and `TestWatchPartyIgnoreWait` cover the v2 surface over the same
two real sockets: the tuning block and capability list in the join ack, `serverReceiveTime` in the
pong, the slowest member moving the shared start instant, `allReady` preceding `syncPlay` with the
same `scheduledPlayAt` and an increasing `seq`, and a member being released by its own opt-out
without anyone waiting on its buffer.

`gate_test.go` drives the same state machine without a database, by building a hub whose peers own
in-memory outbound channels: the lead / clamp / scaled-goal / group-state policies, sequence
stamping, the scheduled release, `ignoreWait`, the wait deadline, a stale `forceReady` crossing
gates, the pause cancelling a gate, `peerBuffering` announced once per fall, and correction
attribution. It exists because the functional suite skips without a database, and a protocol with
no test that always runs is a protocol with no test.

## Persistence and cleanup

In-memory state is authoritative; rows in `watch_parties` and `watch_party_participants` are a
best-effort audit trail. A failed insert warns and the party continues — refusing to let someone
watch because an audit write failed is the wrong trade.

At startup every row still marked `active` is ended: live sockets did not survive the restart, so
such a row is a stale record. `SYNCPLAY_CLEANUP_CRON` (every 10 minutes) deletes rooms that ended
more than `SYNCPLAY_ENDED_TTL_MINUTES` (60) ago along with their participant rows, in one
transaction so participants never outlive their room.

## Known limitations

- The hub can schedule a start but cannot make a client trust it: a peer whose clock estimate is
  still warm (its first ~2 s, or ~4 probes) acts on receipt. Arming against an unmeasured wall
  clock was tried against a live room and reverted — a peer more wrong than the room's lead never
  reached its own start and the gate reopened around it.
- A restart ends every party; there is no socket rejoin with retained state.
- Only the host advances the canonical clock, so a stalled host freezes it.
- Room capacity is `SYNCPLAY_MAX_PARTICIPANTS` (12) per room, enforced on join.
- Cross-episode joins are not validated against the room's episode: a member can join a code while
  playing something else. The site's UI never does this today; `changeEpisode` and
  `episodeChanged` exist so a room can be steered, and the desktop player has no watch-party code at
  all.

Related: [Architecture](01-architecture.md), [API reference](03-api-reference.md).

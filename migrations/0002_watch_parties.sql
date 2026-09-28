-- 0002: watch parties (SyncPlay).
--
-- Rooms are persisted as a best-effort audit trail; live socket state (readiness,
-- drift, stall grace, timers) stays in memory. `id` is the human-typeable room code
-- because clients join by code, matching the original handler's insert shape.

create table if not exists watch_parties (
  id               text primary key,
  host_user_id     uuid not null references animind_users(id) on delete cascade,
  host_name        text,
  episode_id       uuid references episodes(id) on delete set null,
  show_id          uuid references shows(id) on delete set null,
  status           text not null default 'active',
  created_at       timestamptz not null default now(),
  updated_at       timestamptz not null default now(),
  ended_at         timestamptz
);

create index if not exists idx_watch_parties_status on watch_parties(status, ended_at);

create table if not exists watch_party_participants (
  id        uuid primary key default gen_random_uuid(),
  party_id  text not null references watch_parties(id) on delete cascade,
  user_id   uuid references animind_users(id) on delete set null,
  joined_at timestamptz not null default now()
);

create index if not exists idx_party_participants on watch_party_participants(party_id);

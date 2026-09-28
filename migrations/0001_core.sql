-- 0001: identity, sessions, and the media library.
--
-- The Go backend owns authentication end to end: no Clerk, no hosted auth service.
-- `animind_users` stores argon2id password hashes; `animind_web_sessions` holds
-- hashed opaque session tokens for the site; `desktop_auth_sessions` holds the
-- desktop app's short-lived HMAC access tokens plus rotating refresh tokens.
--
-- Column names on shows/episodes keep the original snake_case spelling because the
-- existing desktop client parses those JSON field names directly.

-- ── Users ────────────────────────────────────────────────────────────────────
-- password_hash is NULL for accounts created through an OAuth provider only.
create table if not exists animind_users (
  id                uuid primary key default gen_random_uuid(),
  email             text not null,
  email_normalized  text not null unique,
  username          text not null,
  avatar_url        text,
  password_hash     text,
  is_admin          boolean not null default false,
  failed_login_count integer not null default 0,
  locked_until      timestamptz,
  created_at        timestamptz not null default now(),
  updated_at        timestamptz not null default now()
);

create index if not exists idx_animind_users_admin on animind_users(is_admin);

-- One row per external identity (currently Google) linked to a user.
create table if not exists animind_user_identities (
  id            uuid primary key default gen_random_uuid(),
  user_id       uuid not null references animind_users(id) on delete cascade,
  provider      text not null,
  provider_user_id text not null,
  email         text,
  display_name  text,
  avatar_url    text,
  created_at    timestamptz not null default now(),
  unique (provider, provider_user_id)
);

create index if not exists idx_animind_identities_user on animind_user_identities(user_id);

-- ── Web sessions (site) ──────────────────────────────────────────────────────
-- session_token is never stored; only its SHA-256 hash. Revoking a row signs the
-- browser out on its next request.
create table if not exists animind_web_sessions (
  id                uuid primary key default gen_random_uuid(),
  user_id           uuid not null references animind_users(id) on delete cascade,
  session_hash      text not null unique,
  device_name       text,
  user_agent        text,
  ip_address        inet,
  remember_me       boolean not null default false,
  expires_at        timestamptz not null,
  last_used_at      timestamptz not null default now(),
  revoked_at        timestamptz,
  created_at        timestamptz not null default now()
);

create index if not exists idx_animind_web_sessions_user on animind_web_sessions(user_id);
create index if not exists idx_animind_web_sessions_expires on animind_web_sessions(expires_at);

-- Password reset and email verification tokens. A single table covers both kinds;
-- the consumer that creates a token decides how long it lives.
create table if not exists animind_auth_tokens (
  id          uuid primary key default gen_random_uuid(),
  user_id     uuid not null references animind_users(id) on delete cascade,
  kind        text not null,
  token_hash  text not null unique,
  expires_at  timestamptz not null,
  used_at     timestamptz,
  created_at  timestamptz not null default now()
);

create index if not exists idx_animind_auth_tokens_user_kind on animind_auth_tokens(user_id, kind);

-- ── Desktop sessions ─────────────────────────────────────────────────────────
-- Mirrors the original desktop_auth_sessions layout so the desktop client's
-- exchange/refresh/revoke flow keeps working. Both token kinds are stored hashed;
-- the access token is additionally self-contained (HMAC-signed) so most requests
-- verify without a database round trip, while the row still gates revocation.
create table if not exists desktop_auth_sessions (
  id                      uuid primary key default gen_random_uuid(),
  user_id                 uuid not null references animind_users(id) on delete cascade,
  session_id              text not null unique,
  device_id               text,
  device_name             text,
  refresh_token_hash      text not null unique,
  refresh_token_expires_at timestamptz not null,
  access_token_hash       text not null unique,
  access_token_expires_at timestamptz not null,
  revoked_at              timestamptz,
  created_at              timestamptz not null default now(),
  updated_at              timestamptz not null default now()
);

create index if not exists idx_desktop_auth_user_id on desktop_auth_sessions(user_id);
create index if not exists idx_desktop_auth_refresh_hash on desktop_auth_sessions(refresh_token_hash);
create index if not exists idx_desktop_auth_access_hash on desktop_auth_sessions(access_token_hash);
create index if not exists idx_desktop_auth_refresh_exp on desktop_auth_sessions(refresh_token_expires_at);
create index if not exists idx_desktop_auth_access_exp on desktop_auth_sessions(access_token_expires_at);

-- ── Media library ────────────────────────────────────────────────────────────
-- Title uniqueness is enforced on the normalized form, not the raw text: a release folder
-- and an AniList import differ in case and punctuation for the same show, and two rows for
-- one show is what makes the library look broken downstream.
create table if not exists shows (
  id                uuid primary key default gen_random_uuid(),
  title             text not null,
  synopsis          text,
  cover_image_url   text,
  genres            text[] not null default '{}',
  rating            numeric,
  episode_count     integer,
  studio            text,
  status            text,
  year              text,
  anilist_id        integer,
  trailer_id        text,
  trailer_site      text,
  trailer_thumbnail text,
  created_at        timestamptz not null default now()
);

-- Case-insensitive uniqueness, and the lookup the scanner's title matching uses. A plain
-- UNIQUE(title) would let "Frieren" and "FRIEREN" both in, which is how one show ends up as
-- two cards with half its episodes each.
create unique index if not exists shows_title_unique on shows (lower(trim(title)));
create index if not exists shows_anilist_id on shows(anilist_id);

create table if not exists episodes (
  id                 uuid primary key default gen_random_uuid(),
  show_id            uuid not null references shows(id) on delete cascade,
  season_number      integer not null default 1,
  content_type       text not null default 'tv',
  episode_number     numeric,
  title              text,
  thumbnail          text,
  duration           integer,
  file_path          text not null,
  bucket_name        text not null default 'local',
  original_title     text,
  scanner_source     text,
  scanner_confidence numeric,
  created_at         timestamptz not null default now(),
  -- The scanner upserts on this exact conflict target.
  unique (show_id, season_number, content_type, episode_number)
);

create index if not exists idx_episodes_show on episodes(show_id, season_number, episode_number);
create index if not exists idx_episodes_file_path on episodes(file_path);

-- ── User-scoped viewing data ─────────────────────────────────────────────────
-- Previously these tables were written directly from the browser under Clerk JWT
-- row-level security, and from the desktop player against PostgREST. Both now go
-- through this API, so the row shapes are kept byte-compatible with what those
-- clients already parse: watchlist stores the whole anime object as JSON under
-- `anime_data`, and anime_id is an AniList id (text) rather than a local show id.
create table if not exists watchlist (
  id          uuid primary key default gen_random_uuid(),
  user_id     uuid not null references animind_users(id) on delete cascade,
  anime_id    text not null,
  anime_data  jsonb not null default '{}'::jsonb,
  status      text not null default 'Plan to Watch',
  created_at  timestamptz not null default now(),
  updated_at  timestamptz not null default now(),
  unique (user_id, anime_id)
);

create index if not exists idx_watchlist_user_created on watchlist(user_id, created_at desc);

-- Per-episode resume position, keyed by show and the client's positional episode
-- index, exactly as the site writes it today.
create table if not exists progress (
  id            uuid primary key default gen_random_uuid(),
  user_id       uuid not null references animind_users(id) on delete cascade,
  anime_id      text not null,
  episode_index integer not null default 0,
  timestamp     double precision not null default 0,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now(),
  unique (user_id, anime_id, episode_index)
);

create index if not exists idx_progress_user_updated on progress(user_id, updated_at desc);

-- Continue-watching rail written by the desktop player. One row per show+episode.
create table if not exists watch_history (
  id            uuid primary key default gen_random_uuid(),
  user_id       uuid not null references animind_users(id) on delete cascade,
  anilist_id    text,
  show_title    text not null,
  episode_label text not null default '',
  thumbnail_url text,
  progress_pct  integer not null default 0,
  last_watched  timestamptz not null default now(),
  unique (user_id, show_title, episode_label)
);

create index if not exists idx_watch_history_user on watch_history(user_id, last_watched desc);

-- Per-user player preferences, replacing the localStorage-only settings the clients
-- keep today (volume, speed, subtitle and audio track choices).
create table if not exists user_preferences (
  user_id    uuid primary key references animind_users(id) on delete cascade,
  preferences jsonb not null default '{}'::jsonb,
  updated_at timestamptz not null default now()
);


-- 0004: desktop refresh-token families.
--
-- A rotated refresh token stays in rotated_from_hash so a later replay of the old value
-- can be recognised as theft and revoke the whole family instead of merely failing. The
-- absolute expiry is not extended by rotation, which bounds a stolen family even when the
-- attacker keeps refreshing.

alter table desktop_auth_sessions
  add column if not exists family_id uuid not null default gen_random_uuid(),
  add column if not exists rotated_from_hash text,
  add column if not exists absolute_expires_at timestamptz not null
    default (now() + interval '90 days');

create index if not exists idx_desktop_auth_family
  on desktop_auth_sessions(family_id);
create index if not exists idx_desktop_auth_rotated
  on desktop_auth_sessions(rotated_from_hash);

-- 0003: torrent download jobs and the can_download entitlement.
--
-- One row per user-initiated torrent download. `state` follows the manager's state
-- machine; the partial unique index lets failed/cancelled attempts for the same
-- torrent repeat while a live (or ready/paused) job exists.

create table if not exists download_jobs (
  id              uuid primary key default gen_random_uuid(),
  user_id         uuid not null references animind_users(id) on delete cascade,
  result_title    text not null,
  info_hash       text not null,
  size_bytes      bigint not null,
  state           text not null check (state in (
                    'queued','fetching_metadata','awaiting_selection','downloading',
                    'verifying','importing','ready','failed','cancelled','paused')),
  error           text,
  progress_bytes  bigint not null default 0,
  selected_files  jsonb,
  save_path       text,
  show_id         uuid references shows(id) on delete set null,
  created_at      timestamptz not null default now(),
  updated_at      timestamptz not null default now(),
  completed_at    timestamptz
);

create index if not exists idx_download_jobs_user on download_jobs(user_id, created_at desc);
create index if not exists idx_download_jobs_state on download_jobs(state);

-- One live or ready job per user and torrent; terminal failures and cancellations may repeat.
create unique index if not exists uq_download_jobs_active_hash on download_jobs(user_id, info_hash)
  where state in ('queued','fetching_metadata','awaiting_selection','downloading',
                  'verifying','importing','ready','paused');

alter table animind_users add column if not exists can_download boolean not null default false;

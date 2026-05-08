drop index if exists public.idx_memory_embedding;

alter table public.agent_memory_log
    drop column if exists embedding;

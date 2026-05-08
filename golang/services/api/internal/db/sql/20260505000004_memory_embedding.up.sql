-- Add pgvector embedding column to agent_memory_log.
-- Requires the vector extension (created in migration 003).

alter table public.agent_memory_log
    add column if not exists embedding vector(768);

create index if not exists idx_memory_embedding
    on public.agent_memory_log using ivfflat (embedding vector_cosine_ops)
    with (lists = 100);

-- chat schema

create table if not exists public.conversations (
    id              uuid         primary key default gen_random_uuid(),
    user_id         uuid         not null,
    agent_id        varchar(100) not null default 'default',
    title           text,
    summary         text,
    status          varchar(20)  not null default 'active',
    message_count   int          not null default 0,
    created_at      timestamptz  not null default now(),
    updated_at      timestamptz  not null default now(),
    last_message_at timestamptz
);

create index if not exists idx_conversations_user
    on public.conversations (user_id, last_message_at desc);

create index if not exists idx_conversations_agent
    on public.conversations (agent_id, status);

create table if not exists public.messages (
    id               uuid         primary key default gen_random_uuid(),
    conversation_id  uuid         not null references public.conversations(id) on delete cascade,
    session_id       uuid,
    role             varchar(20)  not null,
    content          text         not null default '',
    content_type     varchar(20)  not null default 'text',
    tool_calls       jsonb,
    tool_results     jsonb,
    tool_name        varchar(100),
    eval_ok          boolean,
    confidence_score float,
    model            varchar(100),
    usage_tokens     int,
    latency_ms       int,
    trace_id         varchar(64),
    sequence         int          not null default 0,
    created_at       timestamptz  not null default now()
);

create index if not exists idx_messages_conversation
    on public.messages (conversation_id, sequence);

create index if not exists idx_messages_session
    on public.messages (session_id);

create index if not exists idx_messages_created
    on public.messages (conversation_id, created_at desc);

create or replace function public.set_message_sequence()
returns trigger language plpgsql as $$
begin
    select coalesce(max(sequence), 0) + 1
    into new.sequence
    from public.messages
    where conversation_id = new.conversation_id;
    return new;
end;
$$;

create trigger trg_set_message_sequence
before insert on public.messages
for each row execute function public.set_message_sequence();

create or replace function public.sync_conversation_on_message()
returns trigger language plpgsql as $$
begin
    update public.conversations
    set message_count   = message_count + 1,
        last_message_at = new.created_at,
        updated_at      = now(),
        title = case
            when title is null and new.role = 'user'
                then left(new.content, 80)
            else title
        end
    where id = new.conversation_id;
    return new;
end;
$$;

create trigger trg_sync_conversation_on_message
after insert on public.messages
for each row execute function public.sync_conversation_on_message();

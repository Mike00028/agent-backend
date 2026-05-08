-- agent orchestration schema

create table if not exists public.agents (
    id                      varchar(100) primary key,
    owner_user_id           uuid,
    name                    varchar(100) not null,
    description             text,
    system_prompt           text         not null,
    agent_type              varchar(20)  not null default 'react',
    model                   varchar(50),
    planner_model           varchar(50),
    tools                   jsonb,
    sub_agents              jsonb,
    approval_required_tools jsonb,
    evaluator_enabled       boolean      not null default true,
    max_iterations          int          not null default 2,
    memory_policy           jsonb,
    is_public               boolean      not null default false,
    created_at              timestamptz  not null default now(),
    updated_at              timestamptz  not null default now()
);

create index if not exists idx_agents_owner
    on public.agents (owner_user_id);

create table if not exists public.agent_sessions (
    id                 uuid         primary key default gen_random_uuid(),
    user_id            uuid         not null,
    agent_id           varchar(100) not null references public.agents(id) on delete cascade,
    request_id         varchar(100) not null unique,
    status             varchar(20)  not null default 'running',
    started_at         timestamptz  not null default now(),
    completed_at       timestamptz,
    dag_json           jsonb,
    dag_failure_mode   varchar(20)  not null default 'fail_fast',
    dag_failure_reason varchar(500),
    confidence_score   float,
    confidence_reason  varchar(255),
    message_count      int          not null default 0,
    last_memory_flush  timestamptz
);

create index if not exists idx_agent_sessions_user
    on public.agent_sessions (user_id, started_at desc);

create index if not exists idx_agent_sessions_agent
    on public.agent_sessions (agent_id, started_at desc);

create index if not exists idx_agent_sessions_status
    on public.agent_sessions (status);

create table if not exists public.agent_task_nodes (
    task_id               uuid        not null references public.agent_sessions(id) on delete cascade,
    node_id               varchar(50) not null,
    status                varchar(20) not null default 'pending',
    created_at            timestamptz not null default now(),
    started_at            timestamptz,
    completed_at          timestamptz,
    input_args            jsonb       not null default '{}',
    output                jsonb,
    retry_count           int         not null default 0,
    max_retries           int         not null default 3,
    last_error            varchar(500),
    error_code            varchar(20),
    refinement_generation int         not null default 0,
    duration_ms           int,
    primary key (task_id, node_id)
);

create index if not exists idx_task_nodes_task_id
    on public.agent_task_nodes (task_id);

create index if not exists idx_task_nodes_status
    on public.agent_task_nodes (status);

create table if not exists public.agent_memory_log (
    id          uuid        primary key default gen_random_uuid(),
    session_id  uuid        not null references public.agent_sessions(id) on delete cascade,
    user_id     uuid        not null,
    memory_type varchar(20) not null,
    content     text        not null,
    metadata    jsonb,
    created_at  timestamptz not null default now()
);

create index if not exists idx_memory_session
    on public.agent_memory_log (session_id, memory_type);

create index if not exists idx_memory_user
    on public.agent_memory_log (user_id, memory_type);

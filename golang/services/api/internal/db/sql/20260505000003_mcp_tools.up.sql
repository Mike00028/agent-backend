-- MCP tool registry: stores tools discovered from MCP servers
-- Supports hybrid search (keyword + vector) for tool selection

create extension if not exists vector;

-- MCP server configurations
create table if not exists public.mcp_servers (
    id          uuid         primary key default gen_random_uuid(),
    name        varchar(100) not null unique,
    transport   varchar(10)  not null default 'sse',   -- 'sse' | 'stdio'
    url         text,                                  -- SSE endpoint URL (null for stdio)
    command     text,                                  -- stdio command (null for SSE)
    args        jsonb,                                 -- stdio args / extra config
    auth_type   varchar(20),                           -- 'bearer' | 'api_key' | null
    auth_secret text,                                  -- encrypted token / key
    is_enabled  boolean      not null default true,
    last_sync   timestamptz,
    sync_error  text,
    created_at  timestamptz  not null default now(),
    updated_at  timestamptz  not null default now()
);

-- Tools discovered from MCP servers
create table if not exists public.mcp_tools (
    id              uuid          primary key default gen_random_uuid(),
    server_id       uuid          not null references public.mcp_servers(id) on delete cascade,
    name            varchar(200)  not null,
    description     text          not null default '',
    input_schema    jsonb,                              -- JSON Schema for tool parameters
    is_enabled      boolean       not null default true,

    -- Hybrid search
    search_text     text generated always as (
        name || ' ' || coalesce(description, '')
    ) stored,
    embedding       vector(768),                        -- tool description embedding

    -- Sync tracking
    schema_hash     varchar(64),                        -- SHA-256 of input_schema for change detection
    last_synced_at  timestamptz   not null default now(),
    created_at      timestamptz   not null default now(),
    updated_at      timestamptz   not null default now(),

    unique (server_id, name)
);

-- Indexes for hybrid search
create index if not exists idx_mcp_tools_search_text
    on public.mcp_tools using gin (to_tsvector('english', search_text));

create index if not exists idx_mcp_tools_embedding
    on public.mcp_tools using hnsw (embedding vector_cosine_ops);

create index if not exists idx_mcp_tools_server
    on public.mcp_tools (server_id);

create index if not exists idx_mcp_tools_enabled
    on public.mcp_tools (is_enabled) where is_enabled = true;

-- MCP tool execution log (for auditing)
create table if not exists public.mcp_tool_calls (
    id          uuid         primary key default gen_random_uuid(),
    tool_id     uuid         not null references public.mcp_tools(id) on delete set null,
    session_id  uuid,
    task_id     varchar(50),
    input_json  jsonb        not null,
    output_json jsonb,
    status      varchar(20)  not null,  -- 'success' | 'error' | 'timeout'
    error_msg   text,
    duration_ms int,
    created_at  timestamptz  not null default now()
);

create index if not exists idx_mcp_tool_calls_tool
    on public.mcp_tool_calls (tool_id, created_at desc);

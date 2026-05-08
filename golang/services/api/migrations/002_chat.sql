-- Migration: Chat Schema
-- Created: May 5, 2026
-- Purpose: Persistent chat history — conversations and per-message records.

-- ── Conversations ─────────────────────────────────────────────────────────────
-- One row per "chat thread". A session (agent_sessions) is a single request;
-- a conversation is a long-lived thread that can span many sessions.
CREATE TABLE IF NOT EXISTS conversations (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL,
    agent_id    VARCHAR(100) NOT NULL DEFAULT 'default',

    -- Display
    title       TEXT,                               -- Auto-set from first message; editable
    summary     TEXT,                               -- LLM-generated rolling summary (memory)

    -- State
    status      VARCHAR(20) NOT NULL DEFAULT 'active',  -- 'active' | 'archived' | 'deleted'
    message_count INT        NOT NULL DEFAULT 0,

    -- Timestamps
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_message_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_conversations_user
    ON conversations (user_id, last_message_at DESC);

CREATE INDEX IF NOT EXISTS idx_conversations_agent
    ON conversations (agent_id, status);

-- ── Messages ──────────────────────────────────────────────────────────────────
-- One row per turn (user or assistant). Tool calls are stored inline in metadata.
CREATE TABLE IF NOT EXISTS messages (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id UUID        NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    session_id      UUID,                           -- Links to agent_sessions if available

    -- Content
    role            VARCHAR(20) NOT NULL,           -- 'user' | 'assistant' | 'system' | 'tool'
    content         TEXT        NOT NULL DEFAULT '',
    content_type    VARCHAR(20) NOT NULL DEFAULT 'text',  -- 'text' | 'markdown' | 'json'

    -- Tool-call metadata (populated for assistant turns that used tools)
    tool_calls      JSONB,      -- [{"id":"..","name":"count_vowels","args":{...}}]
    tool_results    JSONB,      -- [{"tool_call_id":"..","content":".."}]
    tool_name       VARCHAR(100),  -- Set when role='tool'

    -- Quality & tracing
    eval_ok         BOOLEAN,
    confidence_score FLOAT,
    model           VARCHAR(100),  -- Which LLM produced this turn
    usage_tokens    INT,           -- Total tokens (input + output)
    latency_ms      INT,

    -- OTel / Langfuse link
    trace_id        VARCHAR(64),   -- OTel trace_id for this turn

    -- Ordering
    sequence        INT         NOT NULL,           -- 1-based position in conversation
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_messages_conversation
    ON messages (conversation_id, sequence);

CREATE INDEX IF NOT EXISTS idx_messages_session
    ON messages (session_id);

CREATE INDEX IF NOT EXISTS idx_messages_created
    ON messages (conversation_id, created_at DESC);

-- ── Auto-increment sequence per conversation ──────────────────────────────────
-- Trigger keeps messages.sequence accurate without app-level locking.
CREATE OR REPLACE FUNCTION trg_set_message_sequence()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    SELECT COALESCE(MAX(sequence), 0) + 1
    INTO   NEW.sequence
    FROM   messages
    WHERE  conversation_id = NEW.conversation_id;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS set_message_sequence ON messages;
CREATE TRIGGER set_message_sequence
    BEFORE INSERT ON messages
    FOR EACH ROW
    WHEN (NEW.sequence IS NULL OR NEW.sequence = 0)
    EXECUTE FUNCTION trg_set_message_sequence();

-- ── Auto-update conversations on new message ──────────────────────────────────
CREATE OR REPLACE FUNCTION trg_update_conversation_on_message()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    UPDATE conversations
    SET    message_count   = message_count + 1,
           last_message_at = NEW.created_at,
           updated_at      = NOW(),
           -- Set title from first user message if not already set
           title = CASE
               WHEN title IS NULL AND NEW.role = 'user'
               THEN LEFT(NEW.content, 100)
               ELSE title
           END
    WHERE  id = NEW.conversation_id;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS update_conversation_on_message ON messages;
CREATE TRIGGER update_conversation_on_message
    AFTER INSERT ON messages
    FOR EACH ROW
    EXECUTE FUNCTION trg_update_conversation_on_message();

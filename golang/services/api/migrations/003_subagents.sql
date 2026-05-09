-- Migration: Subagents / Workflows
-- Created: May 9, 2026
-- Purpose: User-uploaded agent definitions and workflow templates with hybrid search

-- ── Subagents table ───────────────────────────────────────────────────────────
-- Stores both agent definitions and workflow definitions.
-- Discriminated by the `type` column: 'simple' | 'react' | 'workflow'.
-- Uploaded via POST /agents/upload as agents.md (YAML frontmatter + markdown)
-- or as Flowise JSON exports.
CREATE TABLE IF NOT EXISTS subagents (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id        UUID,                                     -- NULL for system agents; NOT NULL for user agents
    name            TEXT        NOT NULL,                     -- unique per owner (users) or globally (system)
    description     TEXT        NOT NULL DEFAULT '',          -- one-line summary for planner prompt

    -- Classification
    type            TEXT        NOT NULL DEFAULT 'react',     -- 'simple' | 'react' | 'workflow'
    source_format   TEXT        NOT NULL DEFAULT 'agents.md', -- 'agents.md' | 'flowise'

    -- Raw content (lossless storage)
    content         TEXT        NOT NULL,                     -- full original file (markdown or JSON)

    -- Parsed configuration
    config          JSONB       NOT NULL DEFAULT '{}',

    -- Idempotency
    schema_hash     TEXT        NOT NULL,                     -- SHA-256 of content; skip re-embed on match

    -- Vector search
    embedding       vector(768),                              -- embed("name: description") via Ollama

    -- Full-text search (auto-maintained generated column)
    search_text     TEXT GENERATED ALWAYS AS (name || ' ' || description) STORED,

    -- Metadata for discovery & organization
    tags            TEXT[]      DEFAULT ARRAY[]::TEXT[],     -- e.g., ["security", "code-review", "go"]
    category        VARCHAR(50),                              -- single category: security|dev-tools|qa|data|other
    version         INT         DEFAULT 1,                    -- config schema version for migrations

    -- Deprecation
    deprecated_at   TIMESTAMPTZ,                              -- marks when agent was deprecated (if null, not deprecated)
    deprecation_notice VARCHAR(500),                          -- message: why deprecated, what to use instead

    -- Visibility
    is_shared       BOOLEAN     NOT NULL DEFAULT false,       -- true = visible to all users
    is_system       BOOLEAN     NOT NULL DEFAULT false,       -- true = platform-managed; hidden from user APIs

    -- Lifecycle
    is_enabled      BOOLEAN     NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Constraints: system agents have owner_id=NULL, user agents have owner_id!=NULL
    CONSTRAINT chk_owner_system CHECK (
        (is_system = false AND owner_id IS NOT NULL) OR (is_system = true AND owner_id IS NULL)
    ),
    
    -- User agents: name unique per owner; system agents: globally unique (enforced via unique index)
    UNIQUE (owner_id, name) WHERE is_system = false
);

-- ── Indexes ───────────────────────────────────────────────────────────────────

-- Owner lookup (list my agents) — only user agents
CREATE INDEX IF NOT EXISTS idx_subagents_owner
    ON subagents (owner_id, is_enabled, name) WHERE is_system = false;

-- Shared agent discovery (list agents shared with everyone) — only user agents
CREATE INDEX IF NOT EXISTS idx_subagents_shared
    ON subagents (is_shared, is_enabled, name) WHERE is_shared = true AND is_system = false;

-- System agent lookup (global, by name) — globally unique
CREATE UNIQUE INDEX IF NOT EXISTS idx_subagents_system_name
    ON subagents (name) WHERE is_system = true;

-- Hybrid search: vector similarity
CREATE INDEX IF NOT EXISTS idx_subagents_vector
    ON subagents USING hnsw (embedding vector_cosine_ops);

-- Hybrid search: keyword (tsvector over generated search_text)
CREATE INDEX IF NOT EXISTS idx_subagents_fts
    ON subagents USING gin (to_tsvector('english', search_text));

-- Type filter (list all workflows vs agents)
CREATE INDEX IF NOT EXISTS idx_subagents_type
    ON subagents (type, is_enabled);

-- Discover by category
CREATE INDEX IF NOT EXISTS idx_subagents_category
    ON subagents (category, is_enabled) WHERE is_system = false;

-- Discover by tags (GIN for fast array membership)
CREATE INDEX IF NOT EXISTS idx_subagents_tags
    ON subagents USING gin (tags) WHERE is_system = false;

-- Deprecation lookup
CREATE INDEX IF NOT EXISTS idx_subagents_deprecated
    ON subagents (deprecated_at) WHERE deprecated_at IS NOT NULL;

-- ── Prevent user agents from conflicting with system agent names ──────────────

CREATE OR REPLACE FUNCTION trg_prevent_system_name_conflict()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NOT NEW.is_system THEN
        -- Inserting a user agent: check if a system agent with this name exists
        IF EXISTS (SELECT 1 FROM subagents WHERE name = NEW.name AND is_system = true) THEN
            RAISE EXCEPTION 'Cannot create user agent: system agent with name "%" already exists', NEW.name;
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS prevent_system_name_conflict ON subagents;
CREATE TRIGGER prevent_system_name_conflict
    BEFORE INSERT ON subagents
    FOR EACH ROW
    EXECUTE FUNCTION trg_prevent_system_name_conflict();

-- ── Auto-update updated_at ────────────────────────────────────────────────────

CREATE OR REPLACE FUNCTION trg_subagents_updated_at()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS set_subagents_updated_at ON subagents;
CREATE TRIGGER set_subagents_updated_at
    BEFORE UPDATE ON subagents
    FOR EACH ROW
    EXECUTE FUNCTION trg_subagents_updated_at();

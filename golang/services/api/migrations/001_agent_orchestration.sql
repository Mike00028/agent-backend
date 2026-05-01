-- Migration: Agent Orchestration Schema
-- Created: May 2, 2026
-- Purpose: Tables for DAG execution, checkpointing, memory, and state recovery

-- Core sessions table (replaces simple Redis key)
CREATE TABLE IF NOT EXISTS agent_sessions (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id UUID NOT NULL,
    agent_id VARCHAR(100) NOT NULL,
    request_id VARCHAR(100) UNIQUE NOT NULL,
    
    -- Execution state
    status VARCHAR(20) NOT NULL DEFAULT 'running',  -- 'running', 'done', 'failed', 'awaiting_approval'
    started_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at TIMESTAMP,
    
    -- DAG and evaluation
    dag_json JSONB,                            -- The planned DAG (full spec)
    dag_failure_mode VARCHAR(20) DEFAULT 'fail_fast',  -- 'fail_fast' | 'continue_all' | 'continue_dependents'
    dag_failure_reason VARCHAR(500),           -- Why DAG failed
    
    -- Confidence & quality
    confidence_score FLOAT,                    -- 0.0-1.0, auto-calculated
    confidence_reason VARCHAR(255),            -- Why score is low
    
    -- Recovery
    last_checkpoint_node_id VARCHAR(50),       -- For resuming after crash
    previous_dag_id UUID REFERENCES agent_sessions(id),  -- Link to previous attempt if re-planned
    
    -- Message counter for memory flushing
    message_count INT DEFAULT 0,
    last_memory_flush TIMESTAMP,
    
    -- Indexes
    INDEX idx_user_session (user_id, started_at),
    INDEX idx_agent_session (agent_id, started_at),
    INDEX idx_status (status)
);

-- Task nodes (checkpoints)
CREATE TABLE IF NOT EXISTS agent_task_nodes (
    task_id UUID REFERENCES agent_sessions(id) ON DELETE CASCADE,
    node_id VARCHAR(50) NOT NULL,
    
    -- Execution tracking
    status VARCHAR(20) NOT NULL DEFAULT 'pending',  -- 'pending', 'running', 'done', 'failed', 'cancelled'
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    started_at TIMESTAMP,
    completed_at TIMESTAMP,
    
    -- I/O
    input_args JSONB NOT NULL,     -- The exact args sent to Python tool
    output JSONB,                  -- The result from tool (null until done)
    
    -- Retry tracking
    retry_count INT DEFAULT 0,
    max_retries INT DEFAULT 3,
    last_error VARCHAR(500),       -- Why did it fail?
    error_code VARCHAR(20),        -- 'timeout' | 'tool_error' | 'validation_failed' | 'unknown'
    retry_reason VARCHAR(255),     -- Human-readable retry context
    
    -- Refinement tracking (for evaluation retries)
    original_node_id VARCHAR(50),  -- NULL if original, else points to parent (e.g., 't2')
    refinement_generation INT DEFAULT 0,  -- 0=original, 1=t2_v2, 2=t2_v3, stop at 2
    
    -- Execution context
    worker_id VARCHAR(100),        -- Which Python worker handled this
    duration_ms INT,               -- Wall time from started to completed
    
    PRIMARY KEY (task_id, node_id),
    INDEX idx_status (status),
    INDEX idx_task_id (task_id),
    FOREIGN KEY (task_id) REFERENCES agent_sessions(id) ON DELETE CASCADE
);

-- Tool execution events (for SSE buffering recovery)
CREATE TABLE IF NOT EXISTS tool_execution_events (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    task_id UUID REFERENCES agent_sessions(id) ON DELETE CASCADE,
    node_id VARCHAR(50),
    
    -- Event metadata
    event_type VARCHAR(50) NOT NULL,  -- 'started' | 'progress' | 'done' | 'error'
    payload JSONB NOT NULL,           -- Event data (progress %, error msg, final result, etc)
    
    -- Timing
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    
    -- Indexes for efficient recovery
    INDEX idx_task_events (task_id, created_at),
    INDEX idx_node_events (task_id, node_id, created_at),
    FOREIGN KEY (task_id) REFERENCES agent_sessions(id) ON DELETE CASCADE
);

-- Memory log (for RAG / context retrieval via ANN search)
CREATE TABLE IF NOT EXISTS agent_memory_log (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    session_id UUID REFERENCES agent_sessions(id) ON DELETE CASCADE,
    user_id UUID NOT NULL,
    
    -- Memory type
    memory_type VARCHAR(20) NOT NULL,  -- 'summary' | 'entity' | 'workflow'
    
    -- Content
    content TEXT NOT NULL,             -- The actual text (summary, fact, etc)
    metadata JSONB,                    -- Source message IDs, timestamps, confidence scores
    
    -- Vector for semantic search
    embedding vector(768) NOT NULL,    -- Computed by Go (Ollama/Gemini)
    
    -- Timing
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    
    -- Indexes
    INDEX idx_session_memory (session_id, memory_type),
    INDEX idx_user_memory (user_id, memory_type),
    INDEX idx_memory_vector USING hnsw (embedding vector_cosine_ops),
    FOREIGN KEY (session_id) REFERENCES agent_sessions(id) ON DELETE CASCADE
);

-- Tool execution audit log (for debugging, cost tracking)
CREATE TABLE IF NOT EXISTS tool_audit_log (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    session_id UUID REFERENCES agent_sessions(id) ON DELETE CASCADE,
    user_id UUID NOT NULL,
    
    -- Tool metadata
    tool_name VARCHAR(100) NOT NULL,
    tool_args JSONB NOT NULL,        -- What args were passed
    tool_result JSONB,               -- What it returned
    
    -- Execution
    status VARCHAR(20) NOT NULL,     -- 'success' | 'error' | 'timeout'
    duration_ms INT,
    error_message VARCHAR(500),
    
    -- Timing
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    
    -- Indexes
    INDEX idx_session_audit (session_id, created_at),
    INDEX idx_user_audit (user_id, tool_name),
    FOREIGN KEY (session_id) REFERENCES agent_sessions(id) ON DELETE CASCADE
);

-- Worker pool status (for ReverseProxy health checks)
CREATE TABLE IF NOT EXISTS worker_pool_status (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    worker_url VARCHAR(255) UNIQUE NOT NULL,
    
    -- Health
    is_healthy BOOLEAN DEFAULT true,
    failure_count INT DEFAULT 0,
    last_health_check TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_health_check_result VARCHAR(20),  -- 'ok' | 'timeout' | 'error'
    
    -- Metrics
    active_connections INT DEFAULT 0,
    total_requests_handled INT DEFAULT 0,
    
    -- Timing
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    
    INDEX idx_healthy (is_healthy),
    INDEX idx_last_check (last_health_check)
);

-- Optional: Agent definitions (for custom agents, Level A)
CREATE TABLE IF NOT EXISTS agents (
    id VARCHAR(100) PRIMARY KEY,
    owner_user_id UUID,
    
    -- Spec
    name VARCHAR(100) NOT NULL,
    description TEXT,
    system_prompt TEXT NOT NULL,
    agent_type VARCHAR(20) DEFAULT 'react',  -- 'react' | 'simple'
    
    -- Config
    model VARCHAR(50),
    planner_model VARCHAR(50),
    tools JSONB,  -- ["web_search", "rag", "code_exec", ...]
    sub_agents JSONB,
    approval_required_tools JSONB,
    
    -- Policies
    evaluator_enabled BOOLEAN DEFAULT true,
    max_iterations INT DEFAULT 2,
    memory_policy JSONB,  -- {"summary": "eval_ok_only", "entity": "always", ...}
    sandbox_backend VARCHAR(20) DEFAULT 'restrictedpython',
    
    -- Visibility
    is_public BOOLEAN DEFAULT false,
    
    -- Timing
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    
    INDEX idx_owner (owner_user_id),
    INDEX idx_public (is_public)
);

-- Indexes for common queries
CREATE INDEX IF NOT EXISTS idx_agent_sessions_recent 
ON agent_sessions(user_id, started_at DESC);

CREATE INDEX IF NOT EXISTS idx_agent_task_nodes_pending
ON agent_task_nodes(task_id, status) 
WHERE status IN ('pending', 'running');

CREATE INDEX IF NOT EXISTS idx_memory_log_search
ON agent_memory_log(user_id, memory_type, created_at DESC);

-- Views for common queries

-- Active sessions (still running)
CREATE OR REPLACE VIEW v_active_sessions AS
SELECT * FROM agent_sessions 
WHERE status IN ('running', 'awaiting_approval')
  AND completed_at IS NULL;

-- Completed sessions with confidence scores
CREATE OR REPLACE VIEW v_completed_sessions AS
SELECT * FROM agent_sessions
WHERE status IN ('done', 'failed')
  AND completed_at IS NOT NULL;

-- Pending tasks (things that haven't started yet)
CREATE OR REPLACE VIEW v_pending_tasks AS
SELECT 
    tn.task_id,
    tn.node_id,
    tn.status,
    tn.created_at,
    tn.retry_count,
    tn.max_retries
FROM agent_task_nodes tn
WHERE tn.status IN ('pending', 'running');

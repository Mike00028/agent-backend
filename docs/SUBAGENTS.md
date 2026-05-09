# Subagents: Schema & Permissions Reference

## Overview

The `subagents` table stores both **agent definitions** and **workflow templates** uploaded by users or created as system agents. The table uses a single discriminator model:

| Column | Purpose |
|--------|---------|
| `type` | Execution type: `simple` \| `react` \| `workflow` |
| `is_system` | Platform-managed agents (not editable/deletable by users) |
| `owner_id` | User who uploaded (NULL for system agents) |
| `is_shared` | Visible to all users (if true) or private to owner (if false) |

---

## Database Schema

### Core Columns

```
id              UUID        — unique identifier
owner_id        UUID        — NULL for system agents; NOT NULL for user agents
name            TEXT        — agent name/slug (validated: lowercase, alphanumeric + underscore, 3-64 chars)
description     TEXT        — one-line summary (appears in planner prompts, search results)
type            TEXT        — 'simple' | 'react' | 'workflow'
source_format   TEXT        — 'agents.md' (YAML + markdown) | 'flowise' (JSON export)
content         TEXT        — full original upload (lossless storage)
config          JSONB       — parsed frontmatter + computed fields
schema_hash     TEXT        — SHA-256(content) for idempotent re-upload
embedding       vector(768) — embed("name: description") for hybrid search
is_enabled      BOOLEAN     — soft-delete: not discoverable if false
is_shared       BOOLEAN     — true = visible to all; false = private to owner
is_system       BOOLEAN     — true = platform-managed, hidden from user APIs
created_at      TIMESTAMPTZ
updated_at      TIMESTAMPTZ
```

### Config JSONB Schema

```json
{
  "model": "gemini-2.0-flash",
  "temperature": 0.1,
  "max_tokens": 4096,
  "max_iterations": 8,
  "timeout_seconds": 120,
  "type": "react",
  "tools": ["rag_agent", "mcp_agent"],
  "approval_required": ["delete_file"],
  "delegates_to": ["chat_agent", "summarize_agent"],
  "output_format": "json",
  "output_schema": {
    "type": "object",
    "properties": { /* JSON Schema */ }
  },
  "memory": {
    "write_on_eval_ok": true,
    "min_score_to_write": 0.8,
    "top_k_read": 5
  },
  "guardrails": {
    "input": {
      "max_input_chars": 50000,
      "block_patterns": ["regex1", "regex2"]
    },
    "output": {
      "redact_patterns": ["secret_pattern"]
    }
  },
  "trace_tags": {
    "team": "platform",
    "domain": "security"
  },
  "steps": [
    {
      "id": "fetch",
      "agent": "mcp_agent",
      "args": { "server": "filesystem", "tool_name": "read_file" },
      "execution_mode": "sequential",
      "depends_on": []
    }
  ],
  "execution_mode": "sequential",
  "credentials": {},
  "input_hints": "code snippet or file path",
  "notes": "Deployment notes, limitations"
}
```

---

## Permissions Model

### Upload: `POST /agents/upload`
- **Who can upload?** All authenticated users
- **Requirements:**
  - Valid YAML frontmatter + markdown body (agents.md) OR Flowise JSON export
  - Name validation: lowercase, alphanumeric + underscore, 3-64 chars
  - Must not conflict with system agent name (enforced by trigger)
- **Effect:** New user agent created with `owner_id = current_user_id`, `is_shared = false` (default)

### Share: `PATCH /agents/:name` with `is_shared = true`
- **Who can share?** The agent's owner
- **Effect:** Agent becomes discoverable by all users via search/list
- **Visibility:** Planner prompt + search results now include shared agents from all users

### Delete: `DELETE /agents/:name`
- **Who can delete?**
  - Agent owner (can always delete their own agents)
  - Admin users (can delete any user agent)
  - Cannot delete system agents (returns 403 Forbidden)
- **Soft delete:** Sets `is_enabled = false` (agents still in DB for audit but not discoverable)

### Update: `PATCH /agents/:name`
- **Who can update?** Agent owner only
- **Forbidden changes:**
  - `type` (re-parse required; would need re-upload)
  - `owner_id` (no transfer)
  - `is_system` (cannot convert user agent to system agent)
- **Allowed changes:**
  - `is_shared`, `is_enabled`, `description`, config tweaks

### Visibility: `GET /agents` (list)
- **Returns:** All agents where:
  - `is_system = false` (exclude system agents from list)
  - `is_enabled = true` (exclude soft-deleted)
  - `owner_id = current_user_id OR is_shared = true` (owner's private + all shared agents)

### Visibility: `GET /agents/:name`
- **Returns:** Agent if:
  - `is_system = false` (user cannot directly query system agents by name)
  - `is_enabled = true`
  - `owner_id = current_user_id OR is_shared = true` (owner or shared)
- **Returns 404** if system agent, disabled, or private to someone else

### Planner Discovery: Parallel search at query time
- **Visible to planner:** All agents where:
  - `is_enabled = true` (regardless of is_system)
  - `owner_id = session_user_id OR is_shared = true` (user's private + all shared)
  - **System agents ARE visible** (is_system = true doesn't exclude)
- **Embedding:** Only `is_system = false` agents are re-embedded on upload; system agents are pre-computed
- **Naming:** System agents can coexist globally; user agents scoped per owner

---

## Name Collision Prevention

**Constraint:** `UNIQUE (owner_id, name) WHERE is_system = false`
- User A can have agent named `code_review`
- User B can have agent named `code_review` (different owner_id)
- System can have agent named `code_review` (owner_id = NULL)
- User C **cannot** create agent named `code_review` if system agent exists (enforced by trigger)

**Trigger:** `prevent_system_name_conflict()`
- Fires on INSERT for user agents (is_system = false)
- Checks if a system agent with the same name exists
- Raises exception if conflict detected

**Result:** System agents act as reserved names. Users cannot override them.

---

## Hybrid Search

### Parallel Goroutine Pattern (Query Time)
```go
// 1. Embed user query once
vec, _ := embedder.Embed(ctx, userMessage)

// 2. Fan-out to 3 parallel searches
g, gctx := errgroup.WithContext(ctx)

g.Go(func() error {
    subagents, _ := subagentSvc.Search(gctx, userID, userMessage, vec, limit: 3)
    // WHERE is_system=false AND is_enabled=true AND (owner_id=$userID OR is_shared=true)
    // Returns: top 3 agents ranked by hybrid score (0.3*keyword + 0.7*vector)
    return nil
})
```

### Indexes

| Index | Purpose |
|-------|---------|
| `idx_subagents_owner` | Partial: owned agents lookup (is_system=false) |
| `idx_subagents_shared` | Partial: shared agents discovery (is_shared=true AND is_system=false) |
| `idx_subagents_system_name` | Unique: system agent name lookup (is_system=true) |
| `idx_subagents_vector` | HNSW: cosine distance on embedding |
| `idx_subagents_fts` | GIN: full-text search on generated search_text column |
| `idx_subagents_type` | Filter by type (simple/react/workflow) |

---

## File Format Support

### agents.md (YAML + Markdown)
```markdown
---
name: security_auditor
description: Audits code for vulnerabilities
type: react
model: gemini-2.0-flash
temperature: 0.1
max_tokens: 4096
tools:
  - rag_agent
  - mcp_agent
---

## System Prompt

You are a senior security engineer...
```

**Parser:** Extracts YAML frontmatter → `config` JSONB, markdown body → system prompt

### Flowise Export (JSON)
```json
{
  "name": "Code Review Bot",
  "type": "AGENTFLOW",
  "flowData": "{\"nodes\":[...],\"edges\":[...]}"
}
```

**Parser:** Walks node graph, identifies component types, converts to our config schema

---

## Validation Rules

| Field | Rule |
|-------|------|
| `name` | ^[a-z0-9_]{3,64}$ (case-insensitive, unique per owner or globally for system) |
| `type` | Must be one of: simple, react, workflow |
| `model` | Required; must match known LLM provider pattern |
| `temperature` | 0.0 - 2.0 |
| `max_tokens` | 100 - 100000 |
| `max_iterations` | 1 - 100 |
| `tools` | Each tool must exist in AgentRegistry or other searchable subagents |
| `delegates_to` | Same as tools; cannot delegate to self |
| `output_schema` | If provided, must be valid JSON Schema draft-07 |
| `guardrails.*.patterns` | Must be valid regex |
| `workflow steps` | DAG must be acyclic; all referenced agents must exist |

---

## Discovery Rules for Planner

When the planner receives `user_query` at chat time:

1. **Embedding:** Compute vector once: `embed("security audit golang code")`
2. **Search:** Parallel lookup returning top-3 agents:
   ```sql
   SELECT * FROM subagents
   WHERE is_enabled = true
     AND (owner_id = $userID OR is_shared = true)
     AND (0.3 * ts_rank(...) + 0.7 * cosine_similarity(...)) > 0.1
   ORDER BY hybrid_score DESC
   LIMIT 3
   ```
3. **Inject into planner prompt:**
   - List of discovered user subagents
   - List of all system agents (is_system = true)
   - Built-in agent registry (go/dag/registry.go)

4. **Planner can emit:**
   - `tool_name: security_auditor` → found subagent
   - `tool_name: chat_agent` → built-in agent
   - `tool_name: mcp_agent` → falls back to MCP tool search

---

## API Endpoints

### List
```
GET /agents?search=security&type=react&shared=false

Query params:
  search: string (hybrid search: name + description)
  type: string (simple|react|workflow)
  shared: bool (true=only shared, false=only private, omit=both)

Response:
  [
    {
      id: UUID,
      name: string,
      description: string,
      type: string,
      is_shared: bool,
      created_at: timestamp,
      updated_at: timestamp,
      (config: {omitted from list view} )
    }
  ]
```

### Get
```
GET /agents/:name
GET /agents/:id

Response:
  {
    id: UUID,
    name: string,
    description: string,
    type: string,
    source_format: string,
    content: string,
    config: JSONB,
    is_shared: bool,
    is_enabled: bool,
    created_at: timestamp,
    updated_at: timestamp
  }
```

### Upload
```
POST /agents/upload
Content-Type: multipart/form-data

form:
  file: (agents.md | agents.json [Flowise export])
  shared: bool (optional, default: false)

Response:
  {
    id: UUID,
    name: string,
    description: string,
    type: string,
    created_at: timestamp,
    status: "created" | "updated" (201 or 200)
  }
```

### Update
```
PATCH /agents/:name

body:
  {
    is_shared: bool,
    is_enabled: bool,
    description: string
  }

Response: 200 OK
```

### Delete
```
DELETE /agents/:name

Response: 204 No Content (soft delete, sets is_enabled=false)
```

---

## Examples

### User Uploads Private Agent
```bash
curl -F "file=@security_auditor.md" \
     -F "shared=false" \
     POST /agents/upload
```
- `owner_id = $user_id`
- `is_shared = false` (private)
- `is_system = false`
- Only this user can use it at chat time

### User Shares Agent
```bash
curl -X PATCH /agents/security_auditor \
     -d '{"is_shared": true}'
```
- `is_shared = true`
- Now all users see it in search results
- All users can use it in chat

### Admin Adds System Agent
```sql
INSERT INTO subagents (
    owner_id, name, description, type, config, content, schema_hash, is_system
) VALUES (
    NULL,
    'code_review_pro',
    'Advanced code review with AST analysis',
    'react',
    '{"model": "gpt-4o", ...}',
    <full markdown>,
    <sha256>,
    true
);
```
- `owner_id = NULL` (system flag)
- `is_system = true` (hidden from user APIs, not editable)
- Discoverable by planner for all users

### User Tries to Override System Agent
```bash
curl -F "file=@code_review_pro.md" POST /agents/upload
```
- If system agent named `code_review_pro` exists
- Returns **409 Conflict**: "Cannot create user agent: system agent with name 'code_review_pro' already exists"

---

## Filtering Rules Summary

| Scenario | WHERE Clause |
|----------|--------------|
| List user's private agents | `owner_id = $userID AND is_shared = false AND is_enabled = true AND is_system = false` |
| List all shared agents | `is_shared = true AND is_enabled = true AND is_system = false` |
| List + shared (union) | `(owner_id = $userID OR is_shared = true) AND is_enabled = true AND is_system = false` |
| Planner discovery (all visible) | `is_enabled = true AND (owner_id = $userID OR is_shared = true)` |
| System agent lookup | `is_system = true AND is_enabled = true` |
| Search (discovery) | `is_enabled = true AND (owner_id = $userID OR is_shared = true) AND is_system = false` |

---

## Design Decisions

| Decision | Rationale |
|----------|-----------|
| Single table for agents + workflows | Type discriminator is sufficient; no relational benefit to separate tables |
| Config as JSONB | Flexibility for diverse agent types; validated at application layer |
| Owner_id = NULL for system agents | Clear intent; enforced by CHECK constraint; enables global unique name index |
| Soft delete (is_enabled) | Audit trail; allows recovery; prevents ID reuse issues |
| Partial unique index on system names | Allows name overlap between user agents (scoped to owner) and system agents (global) |
| Trigger for name conflict prevention | Cleaner than application logic; enforced at DB boundary |
| Hybrid search (keyword + vector) | Balances recall (keyword) with relevance (semantic); 0.3/0.7 weights favor semantic |


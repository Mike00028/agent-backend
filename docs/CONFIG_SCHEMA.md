# SubAgent Config JSONB Schema Specification

## Overview

The `config` JSONB column in the `subagents` table contains the parsed agent configuration. This document formally specifies the schema, validation rules, and field semantics.

## Full Schema (JSON Example)

```json
{
  "model": "gemini-2.0-flash",
  "temperature": 0.1,
  "max_tokens": 4096,
  "max_iterations": 8,
  "timeout_seconds": 120,
  "execution_mode": "sequential",
  "tools": ["rag_agent", "mcp_agent"],
  "approval_required": ["delete_file"],
  "delegates_to": ["chat_agent", "summarize_agent"],
  "output_format": "json",
  "output_schema": {
    "type": "object",
    "properties": {
      "findings": {
        "type": "array",
        "items": { "type": "object" }
      },
      "summary": { "type": "string" },
      "risk_score": { "type": "number" }
    },
    "required": ["findings", "summary"]
  },
  "memory": {
    "write_on_eval_ok": true,
    "min_score_to_write": 0.8,
    "top_k_read": 5
  },
  "guardrails": {
    "input": {
      "max_input_chars": 50000,
      "block_patterns": [
        "(?i)ignore previous instructions",
        "(?i)reveal.*(secret|api.?key)"
      ]
    },
    "output": {
      "redact_patterns": [
        "(?i)(api[_-]?key|secret|password)\\s*[:=]\\s*\\S+",
        "ghp_[A-Za-z0-9]{36}",
        "sk-[A-Za-z0-9]{48}"
      ]
    }
  },
  "trace_tags": {
    "team": "platform",
    "domain": "security",
    "cost_center": "eng-security"
  },
  "steps": [
    {
      "id": "fetch",
      "agent": "mcp_agent",
      "args": { "server": "filesystem", "tool_name": "read_file" },
      "execution_mode": "sequential",
      "depends_on": [],
      "retry_policy": {
        "max_retries": 3,
        "backoff_multiplier": 2.0,
        "initial_delay_ms": 100,
        "max_delay_ms": 5000
      },
      "timeout_seconds": 60
    },
    {
      "id": "audit",
      "agent": "security_auditor",
      "args": { "question": "audit this code" },
      "depends_on": ["fetch"],
      "execution_mode": "parallel"
    },
    {
      "id": "style",
      "agent": "chat_agent",
      "args": { "question": "review style" },
      "depends_on": ["fetch"],
      "execution_mode": "parallel"
    },
    {
      "id": "summarize",
      "agent": "summarize_agent",
      "depends_on": ["audit", "style"],
      "execution_mode": "sequential"
    }
  ],
  "input_hints": "Go or Python code file paths, one per line",
  "notes": "Requires AWS credentials. Best for code < 100KB. Does not work with Rust.",
  "deprecated": false,
  "system_prompt": "You are a senior security engineer...",
  "config_schema_version": 1
}
```

---

## Field Definitions

### Execution (Required)

| Field | Type | Range | Default | Purpose |
|-------|------|-------|---------|---------|
| `model` | string | — | required | LLM to use (format: `provider/model`, e.g., `gemini-2.0-flash`, `gpt-4o`, `ollama/mistral`) |
| `temperature` | number | 0.0-2.0 | 0.7 | Sampling temperature for randomness |
| `max_tokens` | number | 100-100000 | 4096 | Per-call output token limit |
| `max_iterations` | number | 1-100 | 10 | ReAct loop limit (for type: react) |
| `timeout_seconds` | number | 10-3600 | 120 | Hard wall-clock limit per invocation |
| `execution_mode` | string | `sequential`\|`parallel` | `sequential` | Default workflow execution mode |

### Tool Surface (Optional)

| Field | Type | Constraints | Purpose |
|-------|------|-------------|---------|
| `tools` | string[] | Each tool must exist in AgentRegistry or searchable subagents | Tools available to this agent |
| `approval_required` | string[] | Subset of `tools` | Tools requiring human approval before execution |
| `delegates_to` | string[] | Cannot include self; must exist; cannot form cycles | Agents this agent can create sub-tasks for |

### Output (Optional)

| Field | Type | Constraints | Purpose |
|-------|------|-------------|---------|
| `output_format` | string | `text`\|`json`\|`markdown` | default: `text` | Output format hint for LLM |
| `output_schema` | JSON Schema | Valid JSON Schema draft-07 | If provided, LLM respects structured output format |

### Memory (Optional)

```json
"memory": {
  "write_on_eval_ok": boolean,      // default: true
  "min_score_to_write": 0.0-1.0,    // default: 0.7
  "top_k_read": 1-100               // default: 5
}
```

- **`write_on_eval_ok`**: Only persist learnings when evaluator score passes threshold
- **`min_score_to_write`**: Minimum evaluator score (0-1) to write memory
- **`top_k_read`**: How many past memories to retrieve at query time

### Guardrails (Optional)

```json
"guardrails": {
  "input": {
    "max_input_chars": 1-1000000,      // default: no limit
    "block_patterns": ["regex1", ...]  // reject matching inputs before LLM
  },
  "output": {
    "redact_patterns": ["regex1", ...] // scrub from output before returning
  }
}
```

**Validation:**
- All patterns must be valid regex (compiled at parse time)
- Patterns are case-insensitive unless explicitly specified
- Matched strings are logged but not returned to user (PII safety)

### Observability (Optional)

```json
"trace_tags": {
  "team": "platform",
  "domain": "security",
  "cost_center": "eng-sec",
  ...
}
```

Key-value pairs injected into OpenTelemetry spans and logs for correlation and cost tracking.

### Workflow Steps (Required for type: workflow)

Array of execution steps:

```json
"steps": [
  {
    "id": "step_id",
    "agent": "agent_name",
    "args": { /* agent-specific args */ },
    "execution_mode": "sequential|parallel",
    "depends_on": ["step_id_1", "step_id_2"],
    "retry_policy": {
      "max_retries": 0-10,
      "backoff_multiplier": 1.0-10.0,
      "initial_delay_ms": 10-10000,
      "max_delay_ms": 100-60000
    },
    "timeout_seconds": 10-3600
  }
]
```

**Validation:**
- All `id` values must be unique within the workflow
- All `depends_on` references must resolve to existing step IDs
- DAG must be acyclic (detected at parse time via topological sort)
- All referenced agents must exist (checked at execution time)

### Metadata (Optional)

| Field | Type | Constraints | Purpose |
|-------|------|-------------|---------|
| `input_hints` | string | — | What input format is expected (helps LLM/user understand) |
| `notes` | string | max 500 chars | Deployment notes, limitations, warnings |
| `deprecated` | boolean | — | If true, agent is deprecated (still usable, but discouraged) |
| `system_prompt` | string | — | Computed from markdown body during parse; not user-editable |
| `config_schema_version` | int | 1+ | Version for schema migrations |

---

## Credentials Management

**Credentials are NOT stored in config.** Sensitive values are managed separately via environment variables or a secrets manager (AWS Secrets Manager, HashiCorp Vault, K8s Secrets, etc.):

1. **Deployment:** Set env vars with agent-namespaced prefixes (e.g., `SECURITY_AUDITOR_OPENAI_API_KEY`, `MY_AGENT_SLACK_WEBHOOK`)
2. **Execution:** Agent executor and tool runners read env vars at runtime by name
3. **Config args:** Agent `system_prompt` or tool args reference credentials by env var name (e.g., `"${SECURITY_AUDITOR_OPENAI_API_KEY}"`)

**Example agents.md:**
```yaml
---
name: security_auditor
type: react
model: gpt-4o
tools:
  - mcp_agent
system_prompt: |
  You are a senior security engineer.
  Required environment variables:
  - SECURITY_AUDITOR_OPENAI_API_KEY
  - SECURITY_AUDITOR_SLACK_WEBHOOK
---

Your task: audit the provided code for security vulnerabilities.
```

**Rationale:**
- Keeps secrets out of JSONB (no accidental config logs)
- Aligns with 12-factor app / DevOps best practices
- Supports K8s Secrets, AWS Secrets Manager, HashiCorp Vault, Docker secrets, etc.
- Credentials never leave the agent runtime context
- Easier rotation and revocation
- No auditing burden on the application layer

---

## Validation Rules (Comprehensive)

### On Upload (Parse Time)

| Field | Rule | Error Code |
|-------|------|-----------|
| `model` | Required; format: `[a-z0-9]+([-/][a-z0-9]+)*` | `invalid_model_format` |
| `temperature` | 0.0 - 2.0 inclusive | `out_of_range` |
| `max_tokens` | 100 - 100000 inclusive | `out_of_range` |
| `max_iterations` | 1 - 100 inclusive | `out_of_range` |
| `timeout_seconds` | 10 - 3600 inclusive | `out_of_range` |
| `output_format` | One of: `text`, `json`, `markdown` | `invalid_enum` |
| `output_schema` | If provided, valid JSON Schema draft-07 | `invalid_json_schema` |
| `tools` | Each exists in AgentRegistry or subagents table | `tool_not_found` |
| `approval_required` | Subset of `tools` | `invalid_tool_reference` |
| `delegates_to` | Each exists; not self-reference | `invalid_agent_reference` |
| `guardrails.*.patterns` | Valid regex; compilation succeeds | `invalid_regex` |
| `steps[].id` | Unique within steps array; matches `^[a-z0-9_]{1,50}$` | `invalid_step_id` |
| `steps[].depends_on` | All references resolve to existing step IDs | `step_not_found` |
| DAG | Topological sort succeeds (no cycles) | `cyclic_workflow` |
| `memory.min_score_to_write` | 0.0 - 1.0 inclusive | `out_of_range` |
| `memory.top_k_read` | 1 - 100 inclusive | `out_of_range` |
| `input_hints` | max 500 chars | `field_too_long` |
| `notes` | max 500 chars | `field_too_long` |

### On Execution

| Check | Trigger | Action |
|-------|---------|--------|
| Tool exists at runtime | Before `CallTool` | Return error if tool unavailable |
| Agent exists at runtime | Before routing to executor | Return error if agent unavailable |
| Guardrail input validation | Before LLM call | Block + log if pattern matches |
| Output redaction | After LLM returns | Scrub matched patterns; log redaction |
| Timeout | During execution | Kill process, return timeout error |
| Max iterations | During ReAct loop | Halt if count exceeds limit |
| Approval required | After tool execution | Pause execution, wait for HITL response |

---

## Type Discriminator: type field

The `type` field at the table level (`subagents.type`) determines execution semantics:

### type: simple
- Single LLM call
- No tools, no loop
- `max_iterations` fixed at 1
- Ignores: `tools`, `approval_required`, `delegates_to`, `steps`

### type: react
- ReAct loop: think → select tool → execute → observe → repeat
- Respects `max_iterations` (default: 10)
- Can use `tools` and `delegates_to`
- Ignores: `steps`

### type: workflow
- DAG execution from pre-built `steps[]`
- Skips planner entirely
- `max_iterations` = number of DAG passes (default: 1)
- Ignores: `tools`, `delegates_to` (tool surface is per-step)

---

## Schema Versioning

The `config_schema_version` field enables forward/backward compatibility:

- **Current version:** 1
- **Upgrade path:** If version > 1 at runtime, apply migration logic before parsing
- **Example:** If future schema adds new required field, migration can populate it from existing fields

---

## Example: Simple Agent

```json
{
  "model": "gpt-4o",
  "temperature": 0.7,
  "max_tokens": 2048,
  "system_prompt": "You are a helpful assistant.",
  "config_schema_version": 1
}
```

---

## Example: React Agent (Full)

```json
{
  "model": "gemini-2.0-flash",
  "temperature": 0.1,
  "max_tokens": 4096,
  "max_iterations": 8,
  "timeout_seconds": 120,
  "tools": ["rag_agent", "mcp_agent"],
  "approval_required": ["delete_file"],
  "delegates_to": ["chat_agent"],
  "output_format": "json",
  "output_schema": {...},
  "memory": {
    "write_on_eval_ok": true,
    "min_score_to_write": 0.8,
    "top_k_read": 5
  },
  "guardrails": {...},
  "trace_tags": {...},
  "input_hints": "Code snippet or file path",
  "notes": "Works best with Go/Python code.",
  "system_prompt": "You are a senior security engineer...",
  "config_schema_version": 1
}
```

---

## Example: Workflow Agent

```json
{
  "model": "gpt-4o",
  "max_iterations": 1,
  "timeout_seconds": 300,
  "execution_mode": "sequential",
  "steps": [
    {
      "id": "fetch",
      "agent": "mcp_agent",
      "args": { "server": "fs", "tool_name": "read_file" },
      "execution_mode": "sequential"
    },
    {
      "id": "audit",
      "agent": "security_auditor",
      "args": { "question": "audit this" },
      "depends_on": ["fetch"],
      "execution_mode": "parallel"
    },
    {
      "id": "summarize",
      "agent": "summarize_agent",
      "depends_on": ["audit"],
      "execution_mode": "sequential"
    }
  ],
  "system_prompt": "You are orchestrating a code audit workflow.",
  "config_schema_version": 1
}
```

---

## Parsing & Storage

1. **Upload:** User provides agents.md or Flowise JSON
2. **Parse:** Extract YAML frontmatter or JSON; validate all fields per spec
3. **Hash:** Compute SHA-256 of raw content
4. **Embed:** Embed `name + description` via Ollama/Gemini
5. **Store:** Insert into `subagents` table with config JSONB
6. **Idempotency:** If hash matches existing agent, skip re-embed and update `updated_at`

---

## Error Handling

All validation failures are returned as structured errors with:

```json
{
  "error": "validation_failed",
  "details": [
    {
      "field": "max_tokens",
      "message": "max_tokens must be between 100 and 100000",
      "code": "out_of_range"
    },
    {
      "field": "steps[0].depends_on[0]",
      "message": "step 'unknown_step' not found",
      "code": "step_not_found"
    }
  ]
}
```


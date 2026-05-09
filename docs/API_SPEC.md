# SubAgent API Specification

## Endpoints

All endpoints require authentication (`Authorization: Bearer <token>`).

---

## POST /agents/upload

Upload a new agent or update an existing one.

### Request

**Content-Type:** `multipart/form-data`

**Form Fields:**

| Field | Type | Required | Constraints |
|-------|------|----------|-------------|
| `file` | file | yes | agents.md (YAML+markdown) or agents.json (Flowise export) |
| `shared` | boolean | no | default: false |
| `tags` | string[] | no | e.g., `["security", "code-review"]` |
| `category` | string | no | one of: security, dev-tools, qa, data, other |

### Request Example

```bash
curl -X POST http://localhost:8080/agents/upload \
  -H "Authorization: Bearer <token>" \
  -F "file=@security_auditor.md" \
  -F "shared=false" \
  -F "tags=security" \
  -F "tags=code-review" \
  -F "category=security"
```

### Response

**Status:** `201 Created` (new) or `200 OK` (updated)

**Content-Type:** `application/json`

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "name": "security_auditor",
  "description": "Audits code for vulnerabilities",
  "type": "react",
  "source_format": "agents.md",
  "is_shared": false,
  "is_enabled": true,
  "tags": ["security", "code-review"],
  "category": "security",
  "created_at": "2026-05-09T15:30:00Z",
  "updated_at": "2026-05-09T15:30:00Z",
  "status": "created"
}
```

### Error Responses

**400 Bad Request:** Validation failed

```json
{
  "error": "validation_failed",
  "details": [
    {
      "field": "max_tokens",
      "message": "max_tokens must be between 100 and 100000",
      "code": "out_of_range"
    }
  ]
}
```

**409 Conflict:** Name conflicts with system agent

```json
{
  "error": "name_conflict",
  "message": "Cannot create agent 'code_review': system agent with this name already exists"
}
```

**413 Payload Too Large:** File exceeds 10MB

```json
{
  "error": "file_too_large",
  "message": "Maximum file size is 10MB"
}
```

---

## GET /agents

List agents visible to the user (private + shared).

### Query Parameters

| Param | Type | Default | Description |
|-------|------|---------|-------------|
| `search` | string | — | Hybrid search: name + description |
| `type` | string | — | Filter: `simple`\|`react`\|`workflow` |
| `category` | string | — | Filter: `security`\|`dev-tools`\|`qa`\|`data` |
| `tags` | string[] | — | Filter (AND): e.g., `?tags=security&tags=golang` |
| `shared` | boolean | — | Filter: true=only shared, false=only private, omit=both |
| `limit` | int | 50 | Results per page (1-200) |
| `offset` | int | 0 | Pagination offset |

### Request Example

```bash
curl "http://localhost:8080/agents?search=security&type=react&tags=golang&limit=20" \
  -H "Authorization: Bearer <token>"
```

### Response

**Status:** `200 OK`

```json
{
  "agents": [
    {
      "id": "550e8400-e29b-41d4-a716-446655440000",
      "name": "security_auditor",
      "description": "Audits code for vulnerabilities",
      "type": "react",
      "is_shared": false,
      "is_enabled": true,
      "tags": ["security", "golang"],
      "category": "security",
      "deprecated_at": null,
      "created_at": "2026-05-09T15:30:00Z",
      "updated_at": "2026-05-09T15:30:00Z"
    }
  ],
  "total": 1,
  "limit": 20,
  "offset": 0
}
```

**Note:** `content` and `config` are omitted from list responses; use `GET /agents/:name` for full details.

---

## GET /agents/{name}

Get details of a specific agent by name.

### Path Parameters

| Param | Type | Description |
|-------|------|-------------|
| `name` | string | Agent name (lowercase alphanumeric + underscore) |

### Request Example

```bash
curl "http://localhost:8080/agents/security_auditor" \
  -H "Authorization: Bearer <token>"
```

### Response

**Status:** `200 OK`

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "name": "security_auditor",
  "description": "Audits code for vulnerabilities",
  "type": "react",
  "source_format": "agents.md",
  "content": "---\nname: security_auditor\n...\n---\n\nYou are a senior security engineer...",
  "config": {
    "model": "gemini-2.0-flash",
    "temperature": 0.1,
    "max_tokens": 4096,
    "tools": ["rag_agent", "mcp_agent"],
    "...": "..."
  },
  "is_shared": false,
  "is_enabled": true,
  "tags": ["security", "golang"],
  "category": "security",
  "deprecated_at": null,
  "deprecation_notice": null,
  "created_at": "2026-05-09T15:30:00Z",
  "updated_at": "2026-05-09T15:30:00Z"
}
```

### Error Responses

**404 Not Found:** Agent not found or not accessible

```json
{
  "error": "not_found",
  "message": "Agent 'security_auditor' not found"
}
```

---

## GET /agents/{id}

Get details of a specific agent by ID.

### Path Parameters

| Param | Type | Description |
|-------|------|-------------|
| `id` | UUID | Agent UUID |

### Response

Same as `GET /agents/{name}` but matched by ID instead.

---

## PATCH /agents/{name}

Update an agent (owner or admin only).

### Path Parameters

| Param | Type | Description |
|-------|------|-------------|
| `name` | string | Agent name |

### Request Body

```json
{
  "is_shared": true,
  "is_enabled": true,
  "description": "Updated description",
  "tags": ["security", "golang", "devops"],
  "category": "dev-tools",
  "deprecated_at": null,
  "deprecation_notice": "Use security_auditor_v2 instead"
}
```

**All fields optional.** Omitted fields are not updated.

**Forbidden changes:** Cannot update `type`, `owner_id`, `is_system`, `content`, `config` (would require re-upload).

### Request Example

```bash
curl -X PATCH "http://localhost:8080/agents/security_auditor" \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"is_shared": true, "tags": ["security", "golang"]}'
```

### Response

**Status:** `200 OK`

Returns updated agent (same format as `GET /agents/{name}`).

### Error Responses

**403 Forbidden:** Not the owner

```json
{
  "error": "forbidden",
  "message": "Only the agent owner can update this agent"
}
```

**400 Bad Request:** Invalid update (e.g., deprecated_at is invalid)

```json
{
  "error": "validation_failed",
  "details": [
    {
      "field": "deprecated_at",
      "message": "deprecated_at must be a valid RFC3339 timestamp",
      "code": "invalid_format"
    }
  ]
}
```

---

## DELETE /agents/{name}

Delete an agent (owner or admin only). Soft-deletes by setting `is_enabled=false`.

### Path Parameters

| Param | Type | Description |
|-------|------|-------------|
| `name` | string | Agent name |

### Request Example

```bash
curl -X DELETE "http://localhost:8080/agents/security_auditor" \
  -H "Authorization: Bearer <token>"
```

### Response

**Status:** `204 No Content`

### Error Responses

**403 Forbidden:** Not the owner and not admin

```json
{
  "error": "forbidden",
  "message": "Only the agent owner or admin can delete this agent"
}
```

**400 Bad Request:** Attempting to delete system agent

```json
{
  "error": "invalid_operation",
  "message": "Cannot delete system agents"
}
```

---

## GET /agents/template

Download example agents.md template.

### Request Example

```bash
curl "http://localhost:8080/agents/template" \
  -O -D agent_template.md
```

### Response

**Status:** `200 OK`

**Content-Type:** `text/plain`

Returns the template file `docs/templates/agent.md`.

---

## GET /agents/schema

Get the Config JSONB schema (schema.json format).

### Request Example

```bash
curl "http://localhost:8080/agents/schema" \
  -H "Authorization: Bearer <token>"
```

### Response

**Status:** `200 OK`

**Content-Type:** `application/json`

Returns JSON Schema draft-07 spec for `SubAgentConfig`.

---

## POST /agents/validate

Validate an agent definition without uploading.

### Request Body

**Content-Type:** `application/json`

```json
{
  "content": "---\nname: test_agent\n...",
  "format": "agents.md"
}
```

or Flowise format:

```json
{
  "content": { "name": "...", "flowData": "..." },
  "format": "flowise"
}
```

### Response

**Status:** `200 OK` (valid) or `400 Bad Request` (invalid)

```json
{
  "valid": true,
  "errors": [],
  "warnings": [],
  "parsed": {
    "name": "test_agent",
    "description": "...",
    "type": "react",
    "config": { ... }
  }
}
```

or with errors:

```json
{
  "valid": false,
  "errors": [
    {
      "field": "max_tokens",
      "message": "max_tokens must be between 100 and 100000",
      "code": "out_of_range"
    }
  ],
  "warnings": [
    {
      "field": "memory.min_score_to_write",
      "message": "Memory policy will have limited effect on this agent type"
    }
  ]
}
```

---

## Authentication & Authorization

### Bearer Token

All endpoints require `Authorization: Bearer <token>` header.

Tokens are validated by the `middleware.Auth()` middleware, which populates `user_id` in the context.

### Ownership Check

For `PATCH` and `DELETE`:
- Agent owner can always perform the action
- Admin users (role = "admin") can always perform the action
- Other users receive `403 Forbidden`

### Visibility Check

For `GET /agents/{name}`:
- Owner can always see their agent
- Shared agents (`is_shared=true`) are visible to all
- Private agents (`is_shared=false`) are hidden from non-owners
- System agents (`is_system=true`) are hidden from user API (not returned by `GET /agents` or `GET /agents/{name}`, but visible to planner)

---

## Rate Limiting

All endpoints are subject to the `middleware.RateLimit()` default: 100 requests per minute per user.

Exceeding the limit returns:

```json
{
  "error": "rate_limited",
  "message": "Too many requests. Try again in 60 seconds.",
  "retry_after": 60
}
```

**Status:** `429 Too Many Requests`

---

## Pagination

Endpoints that return lists support pagination via `limit` and `offset`:

- **limit:** 1-200, default 50
- **offset:** 0+, default 0

Response includes:

```json
{
  "agents": [...],
  "total": 150,
  "limit": 50,
  "offset": 0
}
```

---

## Error Format (Uniform)

All errors follow this format:

```json
{
  "error": "<error_code>",
  "message": "<human_readable_message>",
  "details": [
    {
      "field": "<field_name>",
      "code": "<validation_code>",
      "message": "<detail_message>"
    }
  ]
}
```

**Common Error Codes:**
- `validation_failed` — Field validation error (400)
- `name_conflict` — Name already taken or conflicts with system agent (409)
- `not_found` — Agent not found (404)
- `forbidden` — User not authorized (403)
- `invalid_operation` — Operation not allowed (e.g., deleting system agent) (400)
- `rate_limited` — Exceeded rate limit (429)
- `internal_error` — Server error (500)

---

## Workflow (Happy Path)

1. **User uploads agent:** `POST /agents/upload` with agents.md file
2. **Validation runs:** Parser validates YAML, checks references, computes hash
3. **Agent stored:** Inserted into `subagents` table with config JSONB
4. **Embedding computed:** Name + description embedded via Ollama (async)
5. **Response:** 201 Created with agent id + metadata
6. **User shares:** `PATCH /agents/{name}` with `{"is_shared": true}`
7. **Discovery:** At next chat, planner can see agent in hybrid search results
8. **Invocation:** Planner emits task with agent name; executor routes to subagent runner


# Agent Harness — Architecture Roles & Responsibilities

> **Target: 10,000 concurrent users · p95 latency < 800 ms (first token) · accuracy via evaluator loop**
> **Pattern: Go DAG Orchestrator + gRPC Python tool execution · 7 agent types (5 Go-local, 2 Python-remote) · External MCP servers via JSON-RPC**

> ⚠️ **Document reflects the actual implemented codebase as of May 2026.**

---

## Nine Components of the Harness

```
01  ReAct loop            — Go DAG orchestrator: plan→execute batch→evaluate per refinement generation
02  context management    — short-term (in-request memory context string) + long-term (Go/pgvector, agent_memory_log)
03  skills & tools        — 7 agents in static registry; external MCP servers registered via MCP_SERVERS_JSON, synced every 5 min
04  sub-agents            — 5 Go-local handlers + 2 Python-remote (gRPC ExecuteTask); static AgentRegistry
05  built-in skills       — math (Go arithmetic), RAG (Python in-memory), text analysis (Python ReAct), MCP (Go)
06  session persistence   — Go custom Postgres checkpointer (agent_task_nodes); HITL via in-memory channel store
07  system prompt assembly — Go-native planner builds system prompt from AgentRegistry + agent_spec + memory context
08  lifecycle hooks        — BeforePlanFunc hooks (pre-plan guards); TaskMiddleware (per-task HITL approval check)
09  permissions & safety   — Auth stub (TODO: JWT/OIDC); rate limit per-IP token bucket; agent spec tool allow-list
```

---

## System Overview

```
Browser / Client
     │  POST /chat (SSE)    POST /agent/invoke (JSON)    POST /agent/approve
     ▼
┌──────────────────────────────────────────────────────────────────┐
│                  Go Orchestrator (Gin)                           │
│                                                                  │
│  Logger → Recovery → CORS → RateLimit → Auth (stub, TODO: JWT) │
│  SSE Writer · Static AgentRegistry (7 agents, binary-compiled)  │
│  Memory Service (Ollama embed + pgvector agent_memory_log)      │
│  MCP Manager (Postgres store, 5-min sync, Ollama embed)         │
│                                                                  │
│  ┌─ DAG Orchestrator ──────────────────────────────────────────┐│
│  │ 1. Go-native Planner → {tasks, depends_on} (Ollama/Gemini)  ││
│  │ 2. Topo-sort tasks into parallel batches (Kahn's algorithm)  ││
│  │ 3. Load completed tasks from Postgres checkpoint (resume)    ││
│  │ 4. Execute batches — 5 local handlers + gRPC to Python       ││
│  │    ├─ chat_agent / math_agent / summarize_agent /           ││
│  │    │   clarify_agent / mcp_agent → Go-local handlers        ││
│  │    └─ rag_agent / text_agent → Python gRPC ExecuteTask      ││
│  │ 5. Checkpoint every start/done/failed to agent_task_nodes   ││
│  │ 6. Go-native Evaluator → {eval_ok, score, feedback}         ││
│  │ 7. If !eval_ok && score < 0.5 && gen < 2 → refine (re-plan) ││
│  │ 8. On success: write memory entry if score ≥ 0.7            ││
│  └─────────────────────────────────────────────────────────────┘│
│  ┌─ gRPC Pool (round-robin) ───────────────────────────────────┐│
│  │ • N *grpc.ClientConn to PYTHON_GRPC_ADDR (50051)            ││
│  │ • Atomic round-robin Next() — lock-free selection           ││
│  │ • Keepalive ping every 120s; no active health-checks        ││
│  └─────────────────────────────────────────────────────────────┘│
│  ┌─ HITL Store ────────────────────────────────────────────────┐│
│  │ • In-memory map[key]chan — blocks task goroutine             ││
│  │ • POST /agent/approve unblocks it; 10-min auto-timeout      ││
│  └─────────────────────────────────────────────────────────────┘│
└────────────────┬─────────────────────────────────────────────────┘
                 │  gRPC streaming — ExecuteTask RPC
                 │  (only for rag_agent and text_agent)
                 ▼
┌──────────────────────────────────────────────────────────────────┐
│         Python gRPC Server (grpc.aio, port 50051)               │
│                                                                  │
│  AgentService RPCs:                                              │
│  • ExecuteTask (server-streaming) ← Go DAG calls this           │
│    ├─ rag_agent  → InMemoryEmbeddingRetriever + LLM answer      │
│    └─ text_agent → ReAct graph (count_vowels/consonants/words)  │
│  • RunAgent (unary) — legacy full supervisor invocation         │
│  • StreamAgent (streaming) — legacy streaming supervisor        │
│                                                                  │
│  Python-side orchestrator (for legacy RunAgent/StreamAgent):    │
│  gate → plan → execute → evaluate → (retry → plan | done)      │
│                                                                  │
│  RAG: InMemoryEmbeddingRetriever (3 seed docs; no pgvector)     │
│  OTel: W3C traceparent extracted from gRPC metadata             │
│        → Python spans become children of Go spans in Langfuse   │
└──────────────────────────────────────────────────────────────────┘
       │                       │
  Ollama / Gemini           Postgres + pgvector
  (LLM inference)          (checkpoints, memory,
   Go + Python              MCP tool store)

External MCP Servers (any — Go speaks JSON-RPC 2.0 directly)
  • HTTP SSE transport or stdio subprocess
  • Registered at startup from MCP_SERVERS_JSON env var
  • Discovered / re-synced every 5 min via Manager.StartPeriodicSync
  • Descriptions embedded via Ollama → stored in Postgres (mcp_tools table)
  • mcp_agent: pgvector hybrid search → call top-ranked tool

  Filesystem MCP server (first-class example):
  • Transport: stdio (Node.js subprocess via npx)
  • Tools: read_file, write_file, list_directory, create_directory,
            move_file, search_files, get_file_info, delete_file
  • Scoped to allowed directories passed as CLI args
  • Security: path traversal blocked by server; allowlist enforced
```

---

## 1. Go — Responsibilities (DAG Orchestrator & Control Plane)

Go is the **control plane**. It does planning, evaluation, memory retrieval/writing, and MCP tool invocation natively. It uses Python only for `rag_agent` and `text_agent` task execution via gRPC.

### 1.1 Request Pipeline

```
Incoming HTTP
    │
    ├─ Logger (structured JSON: level, method, path, duration_ms, status)
    ├─ Recovery (panic → 500)
    ├─ CORS (allowlist: localhost:8000, localhost:8080, 127.0.0.1:8000/8080)
    ├─ Rate Limit (per-IP token bucket: 50 req/s, burst 100 — golang.org/x/time/rate)
    ├─ Auth (stub — currently a no-op; TODO: validate JWT/OIDC)
    │
    └─ Routes:
         POST /chat          → ChatHandler.Stream (SSE DAG execution)
         POST /agent/invoke  → ChatHandler.Invoke (unary JSON DAG execution)
         POST /agent/approve → ApproveHandler.Approve (HITL resume)
         GET  /healthz       → Health (public, returns {"status":"ok"})
```

**Not present:** `GET /agent/status/:task_id`, no `/agents` CRUD endpoints (planned for v2).

### 1.2 DAG Orchestration (The Core Loop)

Go executes this loop for every user request:

```
1. Retrieve memory context
   Go embeds message via Ollama → pgvector cosine search on agent_memory_log
   Returns top-k chunks as plain text string (soft failure: returns "" on error)

2. Call Go-native Planner (internal/planner.Planner)
   Uses llm.Client.ChatInto() with structured DAGPlan schema
   LLM: Ollama (PlannerModel) or Gemini (if LLM_PROVIDER=gemini)
   Outputs {tasks[], reasoning}

3. Emit plan_ready SSE event

4. Topo-sort tasks (Kahn's algorithm → [][]*Task parallel batches)
   - Detects cycles and unknown dependencies
   - Returns an error if any dependency is missing

5. Load completed tasks from Postgres checkpoint (crash-resume)
   SELECT node_id, output FROM agent_task_nodes WHERE task_id=? AND status='done'
   Pre-populate results map, skip those tasks in execution

6. Execute batches in parallel (one goroutine per task)
   - LocalTaskFunc  → Go-native handler (chat, math, summarize, clarify, mcp)
   - Python remote  → gRPC pool.Next().ExecuteTask() streaming RPC
   - TaskMiddleware runs before each attempt (HITL approval check if spec.ApprovalRequiredTools set)
   - Retry up to 3 times, exponential backoff: 1s, 2s, 4s
   - Timeout: 180s per task (local or remote)
   - Checkpoint SaveTaskStart / SaveTaskDone / SaveTaskFailed per task
   - Fail-fast: cancel all sibling goroutines if any task fails after retries

7. Auto-summarize: if >1 task output and summarize_agent not in plan,
   call Go-native OllamaSummarizer to merge outputs

8. Call Go-native Evaluator (internal/evaluator.Evaluator)
   Uses llm.Client.ChatInto() with evalOutput schema
   Returns {eval_ok, score, feedback, summary}

9. If !eval_ok && score < 0.5 && gen < 2:
   Call runGeneration(ctx, req, gen+1, feedback)  ← recursion, max 2 refinements

10. If !eval_ok && (score ≥ 0.5 || gen ≥ 2):
    Return best-effort result with confidence_score = eval.Score
    confidence_reason = "Hit refinement cap (max 2)" if gen ≥ 2

11. On success (eval_ok=true && score ≥ 0.7):
    Background goroutine: memory.WriteEntry(sessionID, userID, summary)
    Ollama-embed summary → INSERT INTO agent_memory_log

12. DeleteSession from Postgres (CASCADE removes agent_task_nodes)
    Stream dag_done SSE with final output + confidence_score
```

### 1.3 Agent Registry (Static — 7 Agents)

The registry lives in `internal/dag/registry.go`. Adding a new agent requires only editing this file; the planner prompt, executor routing, and safety-net fallbacks all derive from it automatically.

| Agent | IsLocal | Description |
|---|---|---|
| `chat_agent` | **Go** | Answers questions, explains concepts, writes code via `llm.Client.Chat()` |
| `math_agent` | **Go** | Evaluates arithmetic expressions; resolves `{tN}` dependency placeholders |
| `rag_agent` | **Python gRPC** | Looks up internal docs (LangGraph, gRPC, Ollama) via in-memory RAG |
| `summarize_agent` | **Go** | Merges results from multiple tasks via `planner.OllamaSummarizer` |
| `text_agent` | **Python gRPC** | Text analysis via ReAct graph: counts vowels, consonants, word occurrences |
| `clarify_agent` | **Go** | Zero-latency; returns `args.question` directly to user |
| `mcp_agent` | **Go** | pgvector search for best-fit external MCP tool → JSON-RPC 2.0 call |

Unknown agent names (e.g. future DB-loaded custom agents) default to `IsLocal=false, NeedsQuestion=true` — they route to Python gRPC gracefully without code changes.

### 1.4 gRPC Connection Pool

```go
// internal/grpcclient/pool.go
type Pool struct {
    conns []*grpc.ClientConn
    idx   atomic.Uint64  // lock-free round-robin
}

// New creates `size` connections to PYTHON_GRPC_ADDR (default :50051)
func New(size int) (*Pool, error) { ... }

// Next returns the next connection (round-robin, no health checks)
func (p *Pool) Next() *grpc.ClientConn {
    i := p.idx.Add(1) % uint64(len(p.conns))
    return p.conns[i]
}
// Keepalive: ping every 120s, timeout 10s, no ping without active stream
```

**Not implemented:** health-check polling, least-connections routing, automatic connection removal on failure.

### 1.5 Go-native Planner

```go
// internal/planner/planner.go
type Planner struct { client llm.Client; model string }

func (p *Planner) Plan(ctx context.Context, req dag.GoPlanRequest) (*dag.GoPlanResult, error) {
    // Builds system prompt from AgentRegistry descriptions + agent_spec tools + memory context
    // Calls llm.Client.ChatInto() with DAGPlan JSON schema
    // Post-normalises: injects dag.UserMessage as args.question for NeedsQuestion agents
    // Returns {Tasks []*dag.Task, Reasoning string}
}
```

### 1.6 Go-native Evaluator

```go
// internal/evaluator/evaluator.go
type Evaluator struct { client llm.Client; model string }

func (e *Evaluator) Eval(ctx context.Context, req dag.GoEvalRequest) (*dag.EvalResult, error) {
    // Builds eval prompt from user goal + task outputs (truncated at 500 chars each)
    // Calls llm.Client.ChatInto() with evalOutput JSON schema
    // Returns {EvalOK bool, Score float64, Feedback string, Summary string}
}

// Refinement triggers only when: !eval_ok AND score < 0.5
// Evaluators with high scores (≥ 0.5) but eval_ok=false do NOT retrigger refinement
```

### 1.7 Checkpoint & Recovery

```go
// internal/dag/checkpoint.go — PgCheckpoint
SaveTaskStart(ctx, sessionID, task)   // INSERT ... ON CONFLICT DO UPDATE (retry-safe)
SaveTaskDone(ctx, sessionID, task)    // UPDATE status='done', output, duration_ms
SaveTaskFailed(ctx, sessionID, task)  // UPDATE status='failed', last_error
LoadCompletedTasks(ctx, sessionID, gen) // → map[nodeID]output (for crash-resume)
DeleteSession(ctx, sessionID)         // CASCADE removes agent_task_nodes
```

Schema tables: `agent_sessions`, `agent_task_nodes` (created by migrations at startup).

On a Go restart with the same `session_id`: completed tasks are loaded from `agent_task_nodes`, pre-populated into the results map, and skipped during dispatch. Execution resumes from the first pending task.

### 1.8 HITL (Human-in-the-Loop)

```go
// internal/hitl/store.go — in-memory channel map
type Store struct {
    mu      sync.Mutex
    pending map[string]chan ApprovalResult  // key = "sessionID/taskID"
}

// TaskMiddleware registered on the executor:
// If task.ToolName is in spec.ApprovalRequiredTools:
//   emit hitl_approval_required SSE event
//   hitlStore.Request(ctx, sessionID, task.ID)  ← blocks up to 10 minutes
//   if !approved → return error (counts as task failure)

// POST /agent/approve → hitlStore.Respond(sessionID, taskID, ApprovalResult)
```

### 1.9 Memory Service

```go
// internal/memory/service.go
type Service struct { db DB; ollamaURL string; embedModel string }

// Retrieve: embed query → pgvector cosine search → top-k chunks as plain text
func (s *Service) Retrieve(ctx, userID, text string, topK int) string {
    vec := ollamaEmbed(ctx, text)  // POST /api/embeddings
    rows := db.Query(`
        SELECT content FROM agent_memory_log
        WHERE user_id = $1
        ORDER BY embedding <=> $2::vector
        LIMIT $3`, userID, vec, topK)
    return strings.Join(chunks, "\n")  // returns "" on any failure (soft)
}

// WriteEntry: embed content → INSERT INTO agent_memory_log
func (s *Service) WriteEntry(ctx, sessionID, userID, content, memoryType string) error
```

Single table: `agent_memory_log` (`id`, `user_id`, `session_id`, `content`, `embedding vector(N)`, `memory_type`, `created_at`).

**Not implemented:** 4 separate memory type tables, entity extraction, workflow memory, toolbox memory.

### 1.10 MCP Tool Manager

```go
// internal/mcptools/manager.go
type Manager struct { store *Store; embedder Embedder }

// RegisterServer: called at startup for each entry in MCP_SERVERS_JSON.
//   Upserts server config into mcp_servers table, creates a Client.

// StartPeriodicSync: every 5 minutes
//   for each enabled mcp_servers row:
//     JSON-RPC tools/list → get tool names + descriptions
//     Embed description via Ollama → store in mcp_tools table (SHA-256 dedup)
//     RemoveStaleTools: delete tools no longer advertised

// SearchTools(ctx, query, limit) — pgvector + tsvector hybrid search
// CallTool(ctx, serverName, toolName, args) — JSON-RPC tools/call

// mcp_agent local handler (two modes):
//   Search mode: args.question → SearchTools(q, 5) → call top-ranked tool
//   Direct mode: args.server + args.tool_name + args.tool_args → CallTool directly
```

Transports supported:
- **stdio** — spawns a subprocess (`exec.Cmd`), communicates via JSON-RPC 2.0 over stdin/stdout. Used by the filesystem MCP server and any other Node.js/Python/binary MCP server.
- **SSE** — HTTP POST to a `/message` endpoint that returns JSON-RPC over `text/event-stream`.

### 1.11 Filesystem MCP Server Integration

The official `@modelcontextprotocol/server-filesystem` Node.js package is the primary MCP server. It exposes file operations as MCP tools scoped to a list of allowed directories.

**Registration (via `MCP_SERVERS_JSON` env var):**

```json
[
  {
    "name": "filesystem",
    "transport": "stdio",
    "command": "npx",
    "args": ["-y", "@modelcontextprotocol/server-filesystem", "/workspace", "/tmp"]
  }
]
```

Go's `main.go` parses `MCP_SERVERS_JSON` and calls `mcpMgr.RegisterServer()` for each entry before starting `StartPeriodicSync`. On first sync, Go calls `tools/list` via JSON-RPC → gets all tool definitions → embeds descriptions → stores in `mcp_tools` table.

**Tools exposed by the filesystem server:**

| MCP Tool | Description | Key Args |
|---|---|---|
| `read_file` | Read full file content | `path` |
| `read_multiple_files` | Read multiple files at once | `paths[]` |
| `write_file` | Create or overwrite a file | `path`, `content` |
| `edit_file` | Apply line-level diffs to a file | `path`, `edits[]` |
| `create_directory` | Create directory (recursive) | `path` |
| `list_directory` | List files/dirs in a path | `path` |
| `directory_tree` | Recursive tree view | `path` |
| `move_file` | Move or rename a file | `source`, `destination` |
| `search_files` | Recursive search by filename pattern | `path`, `pattern` |
| `get_file_info` | Stat: size, times, permissions | `path` |
| `list_allowed_directories` | Returns the configured allowlist | — |

**How the planner uses it:**

The planner sees all filesystem tools in its system prompt (via `AgentRegistry.mcp_agent` description). For file tasks, it emits:
```json
{"id": "t1", "tool_name": "mcp_agent", "args": {
  "server": "filesystem",
  "tool_name": "read_file",
  "tool_args": {"path": "/workspace/README.md"}
}}
```

Or using search-based invocation (planner describes the intent, Go selects the tool):
```json
{"id": "t1", "tool_name": "mcp_agent", "args": {
  "question": "read the file at /workspace/README.md"
}}
```

**Security constraints:**
- The filesystem server enforces its own allowlist (directories passed as CLI args). Any path outside the allowlist returns an error — Go does not need additional path validation.
- `mcp_agent` must be in `agent_spec.tools[]` (default: included). Removing it prevents the planner from using any MCP tool.
- All calls are logged to `mcp_tool_calls` table (audit trail: input_json, output_json, status, duration_ms).
- `MCP_SERVERS_JSON` should be kept in a secrets manager in production (tokens in `auth_secret` field stored encrypted).

**Prerequisite:** Node.js must be installed in the Go server's runtime environment (for `npx`). Alternatively, install the package globally: `npm install -g @modelcontextprotocol/server-filesystem` and use `"command": "mcp-server-filesystem"`.

### 1.11 LLM Clients

```go
// internal/llm/client.go — interface
type Client interface {
    Chat(ctx, model string, messages []Message) (string, error)
    ChatInto(ctx, model string, messages []Message, out any) error  // structured JSON
}

// Implementations:
// internal/llm/gemini/  — Google Gemini API (LLM_PROVIDER=gemini)
// internal/planner/ollama.go — OllamaClient, POST OLLAMA_BASE_URL/api/chat with Format field

// SchemaOf(T) — reflection-based JSON Schema for structured output requests
// Uses struct tags: json:"name", description:"description text"
```

---

## 2. Python — Responsibilities (gRPC Tool Execution Server)

Python is a **gRPC server** (`grpc.aio`, port 50051). Go calls it only for `rag_agent` and `text_agent` task execution via the `ExecuteTask` server-streaming RPC. Planning, evaluation, and memory are all handled in Go.

### 2.1 gRPC Service Definition

```proto
// proto/langgraph/v1/agent.proto
service AgentService {
  rpc RunAgent    (AgentRequest) returns (AgentResponse);           // legacy unary
  rpc StreamAgent (AgentRequest) returns (stream AgentEvent);       // legacy streaming
  rpc ExecuteTask (TaskRequest)  returns (stream TaskEvent);        // Go DAG → Python tool
}

// TaskRequest: session_id, task_id, tool_name, args_json, context, agent_id
// TaskEvent:   type ("started"/"progress"/"text"/"done"/"error"), payload, pct, error

// NOTE: PlanDAG and EvaluateDAG are handled natively in Go (Ollama structured output).
// Python is only responsible for ExecuteTask (tool execution).
```

### 2.2 ExecuteTask — Routing

```python
# servicer/agent_servicer.py
_AGENT_REGISTRY = {
    "rag_agent":  build_rag_graph(chat_provider, retriever),
    "text_agent": build_text_graph(chat_provider),
}

async def ExecuteTask(self, request, context):
    # Extract W3C traceparent from gRPC metadata → attach to OTel context
    # → Python spans become children of Go spans in Langfuse
    graph = _AGENT_REGISTRY.get(request.tool_name)
    async for event in graph.astream(state):
        yield TaskEvent(type="text", payload=event["result"])
    yield TaskEvent(type="done")
```

### 2.3 Agent Graphs (Python)

| Agent | Graph | Description |
|---|---|---|
| `rag_agent` | `agents/rag/graph.py` | `retriever → answer → END`; InMemoryEmbeddingRetriever (3 seed docs) |
| `text_agent` | `agents/text/graph.py` | `create_react_agent` with tools: `count_vowels`, `count_consonants`, `count_word_occurrences` |

**RAG knowledge base** is in-memory only — 3 documents about LangGraph, gRPC, and Ollama seeded from `rag_seed_documents` env var. No pgvector access from Python.

### 2.4 Legacy RPCs (RunAgent / StreamAgent)

Full Python-side supervisor used when Go calls `RunAgent`/`StreamAgent` directly (not used in the normal DAG flow):

```python
# agents/router/graph.py
gate → plan → execute → evaluate → (retry → plan | done)
```

- **Gate**: classifies intent via regex + cheap LLM call
- **Planner**: LLM-generated `{mode, tasks}` JSON
- **Executor**: dispatches on task fields — `expr` → Python arithmetic, `retrieve=true` → in-memory RAG, `question` → LLM call
- **Evaluator**: LLM-generated `{ok, feedback}`

### 2.5 OTel Trace Propagation

Go's executor injects W3C `traceparent` into gRPC metadata before calling `ExecuteTask`. Python's servicer extracts it via `otel_propagate.extract()` and attaches to the active OTel context. This makes Python spans children of Go task spans in the same Langfuse trace.

---

## 3. Topological Sort & Parallel Execution

### 3.1 TopoSort

```go
// internal/dag/toposort.go — Kahn's algorithm
func TopoSort(tasks []*Task) ([][]*Task, error) {
    // Validates all dependencies exist (returns error on unknown dep)
    // Detects cycles (returns error if visited < len(tasks))
    // Returns [][]*Task — each inner slice is one parallel batch
    // Tasks in the same batch have no unresolved dependencies between them
}
```

### 3.2 Parallel Batch Execution

```go
// internal/dag/executor.go
func (e *Executor) RunBatch(ctx context.Context, batch []*Task, results map[string]string) {
    ctx, cancel := context.WithCancel(ctx)
    defer cancel()  // fail-fast: cancel siblings on first failure
    
    var wg sync.WaitGroup
    for _, task := range batch {
        wg.Add(1)
        go func(t *Task) {
            defer wg.Done()
            for attempt := range maxTaskRetries {
                // Run TaskMiddleware chain (HITL check, etc.)
                // If local handler exists → LocalTaskFunc(ctx, task)
                // Else → gRPC pool.Next().ExecuteTask(ctx, TaskRequest)
                //         drain TaskEvent stream → collect output
                //         W3C traceparent injected into gRPC metadata
                // On success: checkpoint SaveTaskDone, record output in results
                // On failure: exponential backoff, retry, then cancel()/fail-fast
            }
        }(task)
    }
    wg.Wait()
}
```

### 3.3 Refinement Loop (Max 2 Generations)

```go
// internal/dag/orchestrator.go
const maxRefinementGeneration = 2

func (o *Orchestrator) runGeneration(ctx context.Context, req RunRequest, gen int, feedback string) (*RunResult, error) {
    // Plan → Execute → Evaluate
    eval, _ := o.evaluator.Eval(ctx, GoEvalRequest{...})
    
    // Refinement condition: BOTH must be true
    if !eval.EvalOK && eval.Score < 0.5 && gen < maxRefinementGeneration {
        return o.runGeneration(ctx, req, gen+1, eval.Feedback)
    }
    
    // Done — return with confidence score
    return &RunResult{
        FinalOutput:      finalOutput,
        ConfidenceScore:  eval.Score,
        ConfidenceReason: confidenceReason,
        EvalOK:           eval.EvalOK,
    }, nil
}
```

---

## 4. Scaling to 10,000 Users

### 4.1 Bottleneck Map

```
10,000 users  →  ~500 req/s peak
    │
    ▼
Go (stateless DAG orchestrator)   trivial horizontal scale
    │  gRPC streaming — ExecuteTask only
    ▼
Python gRPC Worker Pool           N grpc.aio server instances
    │  Each handles rag_agent / text_agent tasks
    ▼
Ollama / Gemini                   THE real bottleneck — GPU inference
    (planning, evaluation, chat, summarization — all Go-initiated)
```

### 4.2 Latency Budget (p95, first token)

```
Go auth + rate limit + logger                  <   5 ms
Memory retrieve (embed + pgvector)             <  50 ms
Go-native planner (Ollama structured call)     < 200 ms
Topo-sort + checkpoint load (in-memory + SQL)  <  10 ms
Dispatch goroutines                            <   2 ms
First local task (chat_agent / math_agent)     < 400 ms  ← first token from LLM
gRPC task (rag_agent / text_agent via Python)  < 500 ms
Go-native evaluator (Ollama structured call)   < 200 ms
Memory write (Ollama embed + pgvector INSERT)  <  50 ms  (background goroutine)
──────────────────────────────────────────────────────────
p95 first-token (simple single-task query)     < 500 ms
p95 final answer (multi-task DAG)              < 800 ms
```

### 4.3 Scaling Levers

| Layer | Action | Config |
|---|---|---|
| Go | Horizontal pods, L4 LB | Stateless, auto scale |
| Go | gRPC pool size | `GRPC_POOL_SIZE=10` |
| Go | gRPC call timeout | `GRPC_TIMEOUT_MS=180000` |
| Python | More grpc.aio server replicas | Multiple instances behind gRPC load balancer |
| Python | Concurrency limit | `GRPC_MAX_CONCURRENT=50` (semaphore) |
| LLM | Multiple Ollama replicas | LB in front of `OLLAMA_BASE_URL` |
| LLM | Quantized models | `Q4_K_M` GGUF → 4x throughput |
| LLM | Gemini provider | `LLM_PROVIDER=gemini` — no local GPU needed |
| DB | pgvector index tuning | `ivfflat` lists tuning for dataset size |

---

## 5. Invariants (Never Break These)

**Go Ownership (State & Orchestration):**

| Rule | Enforced by |
|---|---|
| Go owns all DAG orchestration (planning, dispatch, topo-sort, checkpointing) | Architecture boundary |
| Go owns all DB writes (checkpoint, memory, audit logs) | Go goroutines only write |
| Go owns session state and task checkpoints | Postgres transactions |
| Go owns embedding calls (Ollama `/api/embeddings`) | Go HTTP client (memory + MCP manager) |
| Go owns pgvector search (memory retrieval + MCP tool search) | Go SQL queries |
| Go owns gRPC pool and connection lifecycle | `grpcclient.Pool` |
| Go enforces DAG failure policy (fail-fast, cancel siblings) | `context.WithCancel` in executor |
| Go enforces refinement cap (`gen < maxRefinementGeneration = 2`) | Recursive guard in orchestrator |
| Go enforces refinement trigger threshold (`score < 0.5`) | Orchestrator eval check |
| Planning and evaluation are Go-native (Ollama/Gemini) | No Python MCP plan/eval tools |

**Python Constraints (gRPC Tool Executor):**

| Rule | Enforced by |
|---|---|
| Python only executes tasks via `ExecuteTask` gRPC RPC | servicer routes on tool_name |
| Python never queries Postgres | No DB in Python — stateless per task |
| Python never tracks state across `ExecuteTask` calls | RAG uses in-memory retriever only |
| Python RAG is in-memory (3 seed docs) — not pgvector | `InMemoryEmbeddingRetriever` |
| Python must propagate W3C trace context from gRPC metadata | OTel extract in servicer |

**DAG & Task Execution:**

| Rule | Enforced by |
|---|---|
| DAG cycle detection runs before execution | `TopoSort()` validates before dispatch |
| All dependencies resolved before task dispatch | Kahn's algorithm batch ordering |
| Parallel tasks cancelled on first failure (fail-fast) | `context.WithCancel` in RunBatch |
| Node retry cap is 3 | `maxTaskRetries = 3` constant in executor |
| Refinement attempts capped at 2 new generations | `maxRefinementGeneration = 2` constant |
| Task output checkpointed to Postgres immediately | `SaveTaskDone/Failed` after each attempt |
| Planner normalises: agents with `NeedsQuestion=true` always get `args.question` set | `planner.Plan()` post-normalisation |

**Memory:**

| Rule | Enforced by |
|---|---|
| Memory always scoped by `user_id` (no cross-user leakage) | SQL `WHERE user_id = $1` constraint |
| Memory write guarded by `eval_ok=true && score ≥ 0.7` | `onStreamDone` gate in handler |
| Memory write is a fire-and-forget background goroutine | Won't block SSE stream |
| ANN search limited to user's own memory | `WHERE user_id = $1` in Retrieve query |

**Tool Execution:**

| Rule | Enforced by |
|---|---|
| Only tools in `agent_spec.tools[]` are callable | Planner system prompt + executor routing |
| HITL approval blocks task goroutine (not entire DAG) | Per-task TaskMiddleware channel wait |
| HITL auto-rejects after 10 minutes | `time.After(ApprovalTimeout)` |
| MCP tools called via JSON-RPC 2.0 (not Python) | Go `mcptools.Manager.CallTool()` |

**Fault Tolerance:**

| Rule | Enforced by |
|---|---|
| Go restarts resume DAG from last checkpoint | `LoadCompletedTasks` on RunBatch entry |
| Python gRPC failure → Go executor retries with next pool connection | `pool.Next()` returns different conn |
| gRPC keepalive: ping every 120s | `keepalive.ClientParameters` in pool opts |
| `NoopCheckpoint` used when Postgres is unavailable | Fallback in main.go |

---

---

## 6. gRPC Contract — Go↔Python

All Go→Python communication is via gRPC streaming. Python tools return `TaskEvent` streams.

### 6.1 ExecuteTask (Primary — Used by DAG)

```proto
rpc ExecuteTask(TaskRequest) returns (stream TaskEvent);

message TaskRequest {
  string session_id = 1;
  string task_id    = 2;
  string tool_name  = 3;  // "rag_agent" | "text_agent" | (future custom agents)
  string args_json  = 4;  // JSON: {"question": "..."}
  string context    = 5;  // Dependency outputs as human-readable text
  string agent_id   = 6;
}

message TaskEvent {
  string type    = 1;  // "started" | "progress" | "text" | "done" | "error"
  string payload = 2;  // Result text or JSON
  int32  pct     = 3;  // Progress 0-100
  string error   = 4;  // Error message if type="error"
}
```

**Go executor injects** W3C `traceparent` into gRPC metadata so Python spans appear as children in Langfuse.

### 6.2 RunAgent / StreamAgent (Legacy — Full Python Supervisor)

```proto
rpc RunAgent    (AgentRequest) returns (AgentResponse);           // unary
rpc StreamAgent (AgentRequest) returns (stream AgentEvent);       // streaming

message AgentRequest {
  string session_id = 1;
  string message    = 2;
  string model      = 3;     // optional model override
  string options    = 4;     // JSON options blob
}
```

Used when Python's full supervisor graph (`gate→plan→execute→eval`) is needed directly, bypassing Go's DAG entirely.

---

## 7. Key Environment Variables

### Go

| Variable | Default | Purpose |
|---|---|---|
| `HTTP_ADDR` | `:8080` | Gin listen address |
| `PYTHON_GRPC_ADDR` | `localhost:50051` | gRPC address for Python server |
| `GRPC_POOL_SIZE` | `5` | Number of gRPC connections in pool |
| `GRPC_TIMEOUT_MS` | `5000` | gRPC call timeout (ms) |
| `GIN_MODE` | `debug` | `debug` or `release` |
| `LLM_PROVIDER` | `gemini` | `gemini` or `ollama` |
| `GEMINI_API_KEY` | — | Required when `LLM_PROVIDER=gemini` |
| `OLLAMA_BASE_URL` | `http://localhost:11434` | Used for planning/eval/embedding when not Gemini |
| `PLANNER_MODEL` | — | **Required.** Model for planning LLM calls |
| `CHAT_MODEL` | — | **Required.** Model for chat_agent / summarize_agent |
| `EVAL_MODEL` | — | **Required.** Model for evaluator LLM calls |
| `EMBED_MODEL` | `nomic-embed-text` | Model for Ollama embeddings |
| `POSTGRES_DSN` | — | Postgres connection string (optional for local dev) |
| `AGENT_SYSTEM_PROMPT` | `"You are a helpful assistant."` | Default agent system prompt |
| `AGENT_MAX_ITERATIONS` | `3` | Max ReAct iterations |
| `AGENT_TOOLS` | `"chat_agent math_agent rag_agent summarize_agent text_agent mcp_agent"` | Space-separated allowed tools |
| `REFINEMENT_MAX_GENERATION` | `2` | Max refinement attempts |
| `MCP_SERVERS_JSON` | — | JSON array of MCP server configs (see §1.11). Set this to register the filesystem server and any other MCP servers. |
| `MESSAGE_BATCH_THRESHOLD` | `15` | (Reserved — memory flush threshold) |
| `MEMORY_FLUSH_INTERVAL_SEC` | `1800` | (Reserved — memory flush interval) |
| `LANGFUSE_OTLP_ENDPOINT` | — | OTel OTLP/HTTP endpoint (e.g. `http://localhost:4318`) |
| `LANGFUSE_PUBLIC_KEY` | — | Langfuse auth |
| `LANGFUSE_SECRET_KEY` | — | Langfuse auth |
| `OTEL_SERVICE_NAME` | `go-orchestrator` | Service name in traces |

### Python

| Variable | Default | Purpose |
|---|---|---|
| `GRPC_PORT` | `50051` | gRPC server listen port |
| `GRPC_MAX_CONCURRENT` | (from config) | Max concurrent gRPC requests (asyncio.Semaphore) |
| `LLM_PROVIDER` | — | `ollama` or `gemini` |
| `LLM_MODEL` | — | Default LLM model |
| `PLANNER_MODEL` | — | Model for Python-side planner (legacy supervisor) |
| `EVALUATOR_MODEL` | — | Model for Python-side evaluator (legacy supervisor) |
| `OLLAMA_BASE_URL` | `http://localhost:11434` | Ollama endpoint |
| `GEMINI_API_KEY` | — | Required when `LLM_PROVIDER=gemini` |
| `RAG_SEED_DOCUMENTS` | — | Pipe-separated seed docs for InMemoryEmbeddingRetriever |
| `AGENT_MAX_ITERATIONS` | `3` | Max ReAct iterations (text_agent) |
| `LANGFUSE_OTLP_ENDPOINT` | — | OTel OTLP/HTTP endpoint |
| `LANGFUSE_PUBLIC_KEY` | — | Langfuse SDK auth |
| `LANGFUSE_SECRET_KEY` | — | Langfuse SDK auth |
| `LANGFUSE_HOST` | — | Self-hosted Langfuse base URL |
| `OTEL_SERVICE_NAME` | `python-agent` | Service name in traces |

---

## 8. Observability & Monitoring

### 8.1 OTel / Langfuse Architecture

```
Go (go.opentelemetry.io/otel — internal/telemetry/)
    │  OTLP/HTTP  →  LANGFUSE_OTLP_ENDPOINT (e.g. :4318)
    ▼
Langfuse self-hosted OTel endpoint (via otel-collector in docker-compose)

Python (opentelemetry-sdk + langfuse v4 SDK)
    │  OTLP/HTTP  →  LANGFUSE_OTLP_ENDPOINT
    ▼
Langfuse self-hosted OTel endpoint

Trace correlation: W3C traceparent injected into gRPC metadata by Go executor →
  extracted by Python servicer → Python spans become children of Go task spans.
  Same Langfuse session_id tag on all spans for session-level filtering.
```

### 8.2 Go Span Attributes (telemetry package)

```go
// Per DAG generation:
span.SetAttr(
    StringAttr("langfuse.session.id", sessionID),
    StringAttr("langfuse.user.id", userID),
    StringAttr("agent.id", agentID),
    IntAttr("generation", gen),
    StringAttr("langfuse.observation.input", req.Message),
)

// Per task (local or remote):
span.SetAttr(
    StringAttr("task.id", task.ID),
    StringAttr("task.tool", toolName),
    StringAttr("langfuse.observation.input", taskInputForTrace(task)),
    StringAttr("langfuse.observation.output", output),
    IntAttr("output.bytes", len(output)),
)
```

### 8.3 Python Langfuse Integration

Langfuse v4 SDK is initialised in `servicer/agent_servicer.py` if keys are present. It auto-instruments every LangChain/LangGraph LLM call, tool call, and token count via OTel auto-instrumentation — no decorator required on individual functions.

### 8.4 Docker Compose Services (Observability)

```yaml
# Observability stack (infra-only, not app services):
otel-collector   # Receives OTLP/HTTP :4318, exports to ClickHouse
clickhouse       # Langfuse event/analytics store (v24.3)
langfuse         # UI + API (:3000) + OTel endpoint (:4318)
langfuse-worker  # Background job processor
minio            # Langfuse media/event blob storage
redis            # Used by Langfuse worker ONLY (not by Go or Python API)
postgres         # pgvector/pg16 — shared by Go API + Langfuse
```

**Go API and Python gRPC server are run locally** via `make run-go` / `make run-python`. They are not Docker Compose services.

### 8.5 Debugging — Manual SQL Queries

```sql
-- What's the status of tasks for a specific session?
SELECT node_id, status, retry_count, started_at, completed_at, duration_ms
FROM agent_task_nodes
WHERE task_id = 'SESSION_UUID'
ORDER BY started_at;

-- What's the memory for a specific user?
SELECT content, memory_type, created_at
FROM agent_memory_log
WHERE user_id = 'USER_UUID'
ORDER BY created_at DESC
LIMIT 10;

-- Are MCP tools synced and embedded?
SELECT server_id, tool_name, created_at
FROM mcp_tools  -- (actual table name may vary — check migrations)
ORDER BY created_at DESC
LIMIT 20;
```

---

## 9. DAG Execution — Full Sequence Diagram

```
POST /chat {message, session_id, user_id, agent_id}
    │
    ├─ [Go] ChatHandler.Stream
    │    ├─ memorySvc.Retrieve(userID, message, topK=3) → contextString
    │    ├─ agentStore.Load(agentID) → AgentSpec → ToSpecJSON()
    │    └─ Build RunRequest{SessionID, UserID, AgentID, Message, MemoryContext}
    │
    ├─ [Go] Orchestrator.Run (gen=0)
    │    ├─ BeforePlanFunc hooks (if any registered)
    │    ├─ planner.Plan(GoPlanRequest) → {Tasks, Reasoning}
    │    ├─ emit SSE: plan_ready {tasks json, reasoning}
    │    ├─ dag.TopoSort(Tasks) → [][]*Task batches
    │    ├─ checkpoint.LoadCompletedTasks(sessionID, gen=0) → priorResults
    │    │
    │    └─ for each batch:
    │         ├─ executor.RunBatch(ctx, batch, results)
    │         │    ├─ [task: chat_agent]    → Go llmClient.Chat()
    │         │    ├─ [task: math_agent]    → Go arithmetic eval
    │         │    ├─ [task: clarify_agent] → Go returns args.question
    │         │    ├─ [task: mcp_agent]     → Go pgvector hybrid search → JSON-RPC call
    │         │    │    ├─ search mode: args.question → SearchTools → top tool → CallTool
    │         │    │    └─ direct mode: args.server+tool_name+tool_args → CallTool
    │         │    │    Examples (filesystem): read_file, write_file, list_directory,
    │         │    │                           search_files, create_directory, move_file
    │         │    ├─ [task: rag_agent]     → gRPC pool.Next().ExecuteTask stream
    │         │    └─ [task: text_agent]    → gRPC pool.Next().ExecuteTask stream
    │         │         (Python ReAct: count_vowels / count_consonants / count_words)
    │         │
    │         └─ each task:
    │              ├─ TaskMiddleware (HITL check if tool in ApprovalRequiredTools)
    │              ├─ checkpoint.SaveTaskStart
    │              ├─ execute (up to 3 retries, 1s/2s/4s backoff)
    │              ├─ checkpoint.SaveTaskDone or SaveTaskFailed
    │              └─ emit SSE: task_started / task_progress / task_done / task_failed
    │
    ├─ [Go] if >1 output and no summarize_agent in plan:
    │         OllamaSummarizer.Summarize(message, outputs[]) → merged string
    │
    ├─ [Go] evaluator.Eval(GoEvalRequest) → {EvalOK, Score, Feedback, Summary}
    │
    ├─ if !EvalOK && Score < 0.5 && gen < 2:
    │    └─ Orchestrator.runGeneration(ctx, req, gen+1, feedback)  ← recurse
    │
    ├─ [Go] if EvalOK && Score ≥ 0.7:
    │         go memory.WriteEntry(sessionID, userID, summary)  ← background
    │
    ├─ [Go] checkpoint.DeleteSession(sessionID)
    └─ emit SSE: dag_done {output, confidence_score, eval_ok}
```

---

## 10. Agent Memory Model

### 10.1 Memory Table

All memory lives in **Postgres + pgvector**. Single table:

```sql
CREATE TABLE agent_memory_log (
    id           UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id      UUID NOT NULL,
    session_id   UUID NOT NULL,
    content      TEXT NOT NULL,        -- plain text chunk (summary, fact, etc.)
    embedding    vector(N) NOT NULL,   -- Ollama-embedded content
    memory_type  TEXT NOT NULL,        -- "summary" | "entity" | (extensible)
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON agent_memory_log USING ivfflat (embedding vector_cosine_ops);
CREATE INDEX ON agent_memory_log (user_id);
```

### 10.2 Read Path (Go, before planning)

```go
func (h *ChatHandler) buildRunRequest(ctx context.Context, req chatRequest) (*RunRequest, *AgentSpec, error) {
    // Retrieve memory context (soft failure — empty string on any error)
    memCtx := h.memorySvc.Retrieve(ctx, req.UserID, req.Message, 3)
    // memCtx is injected into the planner's system prompt as plain text
    return &RunRequest{..., MemoryContext: memCtx}, spec, nil
}
```

### 10.3 Write Path (Go, after successful eval)

```go
// In handler, after Orchestrator.Run returns:
if result.EvalOK && result.ConfidenceScore >= minScoreToWrite {  // minScoreToWrite = 0.7
    go func() {
        h.memorySvc.WriteEntry(ctx, req.SessionID, req.UserID, result.Summary, "summary")
    }()
}
// WriteEntry: embed Summary via Ollama → INSERT INTO agent_memory_log
```

### 10.4 Memory Policy Defaults

Memory writes are guarded:
- `eval_ok = true` AND `score ≥ 0.7` → write summary entry
- `eval_ok = false` → no memory written (don't persist failed responses)

**Not implemented:** separate entity/workflow/toolbox tables, configurable per-agent memory policy, entity extraction, rolling summary of conversation history.

---

## 11. User-defined Custom Agents (Planned — v2)

### Current State (v1)

The agent spec is loaded from environment variables at startup (`AGENT_SYSTEM_PROMPT`, `AGENT_TOOLS`, etc.) and stored as a single hardcoded `AgentSpec` in `agentstore.Store`. All requests use this same spec. There is no database-backed agent registry, no CRUD API, and no per-user agent isolation.

### Planned v2 REST API

```
POST   /agents           — create a custom agent
PUT    /agents/:id       — update (owner only)
DELETE /agents/:id       — delete (owner only)
GET    /agents/:id       — read spec
GET    /agents           — list caller's agents + public platform agents
```

### Planned Validation Logic (v2)

When the CRUD API lands, the Go validation will enforce:

**Mandatory fields:** `name`, `description`, `system_prompt`, `type` (`"react"` or `"simple"`)

**Optional with defaults:** `model`, `planner_model`, `tools[]` (must exist in AgentRegistry or MCP registry), `sub_agents[]` (must be public or owned by caller), `approval_required_tools[]` (must be subset of tools), `evaluator_enabled`, `max_iterations` (1–5), `memory_policy`, `is_public`

**Simple agent fast-path:** `agent_type=simple` → Python gate routes directly to `direct_answer`, bypassing plan/execute/evaluate entirely.


# Agent Harness — Architecture Roles & Responsibilities

> **Target: 10,000 concurrent users · p95 latency < 800 ms (first token) · accuracy via evaluator loop**
> **Pattern: Custom Supervisor + ReAct sub-agents · DAG execution · Dynamic dispatch · 30+ agents · 100s of MCP tools**

---

## Nine Components of the Harness

```
01  while loop            — ReAct think→act→observe per sub-agent
02  context management    — short-term (Go/Redis) + long-term (Python/pgvector, 4 memory types)
03  skills & tools        — MCP registry, 100s of tools, filtered per agent spec
04  sub-agents            — 30+ compiled ReAct graphs, cached by tool-signature
05  built-in skills       — math, RAG, code execution (always available)
06  session persistence   — LangGraph Postgres checkpointer, enables HITL resume
07  system prompt assembly — Go injects persona+tools manifest, Python builds prompt
08  lifecycle hooks       — guardrail_in, memory_inject, guardrail_out, memory_write
09  permissions & safety  — Go circuit breaker (input) + Python semantic guards (I/O)
```

---

## System Overview

```
Browser / Client
     │  POST /chat (SSE)          POST /agent/approve   GET /agent/status/:id
     ▼
┌──────────────────────────────────────────────────────────────────┐
│                  Go Orchestrator (Gin)                           │
│                                                                  │
│  Circuit Breaker → Auth → CORS → Rate Limit → Logger            │
│  SSE Writer · Agent Registry (Postgres, 30s cache)              │
│  Session History (Redis, token-bounded)                         │
│  ┌─ DAG Orchestrator ──────────────────────────────────────────┐│
│  │ 1. Call Python Planner MCP tool {msg, spec} → {DAG}         ││
│  │ 2. Validate DAG against agent_spec (security check)         ││
│  │ 3. Dispatch tasks in parallel (topo-sort, goroutines)       ││
│  │ 4. Checkpoint each completed node to Postgres               ││
│  │ 5. If evaluate fails, call Planner again (refinement ≤2)    ││
│  │ 6. On success, flush memory (summary, entities)             ││
│  └─────────────────────────────────────────────────────────────┘│
│  ┌─ ReverseProxy (Worker Pool) ────────────────────────────────┐│
│  │ • Round-robin + Least-Connections to N Python workers       ││
│  │ • Health-check /health every 10s                            ││
│  │ • Remove worker after 2 consecutive failures                ││
│  │ • Route MCP tool calls to available workers                 ││
│  └─────────────────────────────────────────────────────────────┘│
│                                                                  │
│  Custom Agent CRUD: POST/PUT/DELETE /agents (Level A)           │
└────────────────┬─────────────────────────────────────────────────┘
                 │  
                 │  HTTP (POST /tools/{name}/invoke → 202)
                 │  SSE stream to fetch results (with buffering)
                 ▼
┌──────────────────────────────────────────────────────────────────┐
│               Python FastMCP Worker Pool                         │
│                                                                  │
│  MCP Tools (Stateless Functions):                                │
│  • plan_agent_execution(msg, spec, memory) → {DAG}              │
│  • memory_inject(msg) → {context_text}                          │
│  • memory_summarizer(messages) → {summary, entities}            │
│  • evaluate_response(msg, output) → {eval_ok, score}            │
│  • tool_executor(name, args) → {result} (code, RAG, math, etc)  │
│  • guardrail_in(msg) → {ok, reason}                             │
│  • guardrail_out(response) → {ok, reason, entities}             │
│                                                                  │
│  Features:                                                       │
│  • All state lives in Postgres, not in Python                   │
│  • Events buffered in memory (SSE) for <1ms latency             │
│  • Final results persisted to Postgres                          │
│  • Fault-tolerant: Go can resume from last checkpoint           │
└──────────────────────────────────────────────────────────────────┘
                           │
                    Ollama / vLLM
                 (inference via Python)
                 
                 Postgres
             (orchest. state,
              memory, checkpoints)
```

---

## 1. Go — Responsibilities (Distributed Task Orchestrator)

Go is the **control plane and orchestrator**. It never calls an LLM directly; it delegates all inference to Python MCP tools. It owns DAG execution, checkpointing, and state recovery.

### 1.1 Request Pipeline

```
Incoming HTTP
    │
    ├─ Circuit Breaker (regex + blocklist + embedding deny-list)
    │    trips → HTTP 400, no MCP calls made, < 1ms
    │
    ├─ Auth (JWT middleware)
    ├─ CORS (origin allowlist)
    ├─ Rate Limit (per-IP token bucket: 50 req/s, burst 100)
    ├─ Logger (structured JSON: request_id, session_id, latency_ms)
    │
    └─ Route: POST /chat → SSE stream (DAG execution)
              POST /agent/invoke → unary JSON
              POST /agent/approve → HITL resume
              GET  /agent/status/:task_id → pending|running|awaiting|done
              GET  /healthz → load balancer probe
```

### 1.2 DAG Orchestration (The Core Loop)

Go executes this loop for every user request:

```
1. Call Python Planner MCP tool
   {message, agent_spec, memory_context} → {tasks: [...], depends_on: {...}}

2. Validate DAG against agent_spec
   - Check all tasks use allowed tools
   - Check all dependencies exist
   - Check nodes don't exceed refinement_generation > 2

3. Topo-sort tasks (detect cycles, order by dependencies)

4. Dispatch tasks in parallel
   foreach task_batch in topo_sorted:
       goroutine for each task in batch:
           POST {task_id}/invoke to Python ReverseProxy
           SSE subscribe to see events (buffered by Python)
           On tool_done, checkpoint output to agent_task_nodes
           If timeout, retry up to 3 times, then mark failed

5. Decision: Is DAG successful?
   - All tasks done and passed output validation? YES → evaluate
   - Any task failed after 3 retries? (fail-fast) → NO, abort DAG

6. Call Python Evaluate MCP tool
   {message, dag_output, agent_spec} → {eval_ok: bool, score: float, feedback: str}
   
7. If eval_ok=false and refinement_generation < 2:
   - Call Planner with {message, failure_feedback, previous_dag_outputs}
   - Planner returns refined DAG
   - Loop back to step 2 (re-plan)
   
8. If eval_ok=false and refinement_generation >= 2:
   - Mark answer with confidence_warning=true
   - Send to user with confidence_score

9. Flush memory (summary, entities) via Python MCP tool
   - Every 15 messages OR 30 minutes
   - Memory is then embedded by Go and stored in agent_memory_log

10. Stream done event with final summary, entities, confidence_score
```

### 1.3 ReverseProxy & Worker Pool

Go doesn't talk directly to one Python process. Instead:

```go
type WorkerPool struct {
    workers []*Worker
    mu sync.RWMutex
}

type Worker struct {
    URL string
    Healthy bool
    ActiveConns int
    FailureCount int
}

// Every HTTP POST to Python tool goes through:
func (pool *WorkerPool) DispatchTool(task *Task) (*Response, error) {
    // Round-robin + Least-Connections
    worker := pool.SelectLeastLoaded()
    
    // POST /tools/{tool_name}/invoke {args} → 202 {task_id, stream_url}
    resp, err := http.Post(
        worker.URL + "/tools/" + task.Tool + "/invoke",
        jsonBody(task.Args),
    )
    
    // SSE subscribe to stream_url
    stream := http.Get(resp.StreamURL)
    
    // Drain events, checkpoint on completion
    for event := range stream {
        checkpoint(task.ID, event)
        if event.Type == "done" {
            return event.Payload, nil
        }
    }
}

// Every 10 seconds, check worker health:
func (pool *WorkerPool) HealthCheckLoop() {
    ticker := time.NewTicker(10 * time.Second)
    for range ticker.C {
        for _, w := range pool.workers {
            if !w.IsHealthy() {
                w.FailureCount++
                if w.FailureCount > 2 {
                    pool.RemoveWorker(w)  // Kick out dead workers
                }
            } else {
                w.FailureCount = 0
            }
        }
    }
}
```

### 1.4 Embedding & ANN Search (Go owns this)

Go calls Ollama to embed, then queries Postgres pgvector for context:

```go
func FetchMemoryContext(ctx context.Context, userID string, message string) (string, error) {
    // Embed user message
    vec := ollamaEmbed(ctx, message)
    
    // Two ANN queries in parallel
    summaries, entities := asyncio.WaitAll(
        pgvectorSearch(ctx, "agent_memory_log", vec, 
            "memory_type='summary' AND user_id=?", 3),
        pgvectorSearch(ctx, "agent_memory_log", vec,
            "memory_type='entity' AND user_id=?", 3),
    )
    
    // Format as text for Python Planner tool
    contextText := formatContext(summaries, entities)
    return contextText, nil
}
```

### 1.5 Checkpoint & Recovery

Every completed DAG node is saved:

```sql
INSERT INTO agent_task_nodes (
    task_id, node_id, status, input_args, output, worker_id, 
    started_at, completed_at
) VALUES (?, ?, 'done', ?, ?, ?, ?, ?);
```

If Go crashes mid-DAG, the next request to the same task_id:

```go
// On restart, check what's already done:
SELECT node_id, output FROM agent_task_nodes 
WHERE task_id = ? AND status = 'done'

// Skip those nodes, dispatch only pending ones
```

### 1.6 Parallel Node Failure (Fail-Fast)

By default, if 1 of 10 parallel tasks fails after 3 retries:

```go
// Cancel all siblings
for _, sibling := range parallelBatch {
    if sibling.Status != "done" {
        sibling.Status = "cancelled"
        cancelContext(sibling)  // Kill goroutine
    }
}

// Mark DAG as failed
session.Status = "failed"
session.DagFailureReason = "Node t5 failed after 3 retries"
```

User sees: _"Analysis stopped: Node t5 became unavailable. Partial results saved."_

### 1.7 Memory Flushing (Event-Driven)

```go
// In the SSE handler, after each node completes:
session.MessageCount++

if session.MessageCount >= 15 || time.Since(session.LastMemoryFlush) > 30*time.Minute {
    go FlushMemoryAsync(sessionID)
    session.MessageCount = 0
    session.LastMemoryFlush = time.Now()
}

func FlushMemoryAsync(sessionID string) {
    // Call Python MCP tool
    result := invokeToolMCP("memory_summarizer", {
        session_id: sessionID,
        recent_messages: getLastNMessages(sessionID, 50),
    })
    
    // Extract summary and entities from result
    summary := result["summary"]  // string
    entities := result["entities"]  // [{name, type, context}]
    
    // Embed both
    summaryVec := ollamaEmbed(summary)
    
    for _, entity := range entities {
        entityVec := ollamaEmbed(entity.Name + " " + entity.Type)
        // Upsert to agent_memory_log
        db.Exec(
            `INSERT INTO agent_memory_log (...)
             VALUES (?, ?, 'entity', ?, ?, ?, ...)`,
            sessionID, entity.Name, entity.ContextText, entityVec, ...
        )
    }
}
```

| Hook | Trigger | Action |
|---|---|---|
### 1.8 Agent Registry & Custom Agents

- Loaded from Postgres at startup, refreshed every 30s via background goroutine.
- Cached in `sync.Map` keyed by `agent_id`.
- Access control enforced here — user roles checked against `agent_permissions` table.

---

## 2. Python — Responsibilities (Stateless MCP Tool Server)

Python is a **stateless MCP tool server**. It provides inference and tool execution. All orchestration, state, and DAG logic lives in Go.

### 2.1 MCP Tools (Stateless Functions)

```python
# FastMCP Server on port 3001

@mcp_tool
def plan_agent_execution(
    message: str,
    agent_spec: dict,              # {tools, model, system_prompt, ...}
    memory_context: str            # Pre-retrieved by Go (text form)
) -> dict:
    """
    Generate a DAG of tasks to execute.
    Go already has context from ANN search; just use it.
    """
    system_prompt = f"""
    You are a task planner. Given a user message and available tools, 
    generate a JSON plan with tasks and dependencies.
    
    Available tools: {', '.join(agent_spec['tools'])}
    Available resources: {memory_context}
    """
    
    plan_text = ollama_call(system_prompt, message)
    return json.loads(plan_text)
    # Returns: {"tasks": [...], "depends_on": {...}, "reasoning": "..."}


@mcp_tool
def memory_inject(
    message: str
) -> dict:
    """
    Retrieve context from memory for this message.
    DEPRECATED: Go now owns embedding + ANN search.
    This is for fallback only.
    """
    pass


@mcp_tool
def memory_summarizer(
    session_id: str,
    last_n_messages: list[dict]  # [{role, content}]
) -> dict:
    """
    Summarize recent conversation messages and extract entities.
    Called by Go every 15 messages or 30 minutes.
    """
    system = "Summarize the conversation and extract entities."
    
    summary_text = ollama_call(system, json.dumps(last_n_messages))
    
    # Parse summary and entities
    return {
        "summary": "...",  # 1-2 sentences
        "entities": [
            {"name": "John", "type": "person", "context": "..."},
            ...
        ]
    }


@mcp_tool
def evaluate_response(
    message: str,
    dag_output: dict,             # Full DAG result
    agent_spec: dict
) -> dict:
    """
    Judge whether the DAG output adequately answers the user's question.
    """
    system = """You are a quality evaluator. Given a user message and 
    an AI-generated response, score it 0-1 (1=perfect, 0=useless).
    Also provide feedback for refinement if score < 0.7."""
    
    prompt = f"User: {message}\n\nResponse: {json.dumps(dag_output)}"
    eval_text = ollama_call(system, prompt)
    
    return {
        "eval_ok": score > 0.7,
        "score": float(score),
        "feedback": "..."  # Why it scored low (for refinement)
    }


@mcp_tool
def guardrail_in(message: str) -> dict:
    """Input validation. Block obvious jailbreaks."""
    system = "Is this message safe? 1=safe, 0=unsafe."
    score = float(ollama_call(system, message))
    return {"ok": score > 0.5, "reason": "..."}


@mcp_tool
def guardrail_out(response: str) -> dict:
    """Output validation. Block harmful answers."""
    system = "Is this response safe? 1=safe, 0=unsafe."
    score = float(ollama_call(system, response))
    return {"ok": score > 0.5, "reason": "..."}


@mcp_tool
def code_exec(code: str, sandbox_backend: str = "restrictedpython") -> dict:
    """Execute Python code safely."""
    if sandbox_backend == "restrictedpython":
        from RestrictedPython import compile_restricted, safe_globals
        byte_code = compile_restricted(code, filename="<agent>", mode="exec")
        loc = {}
        exec(byte_code, safe_globals, loc)
        return {"result": str(loc.get("result", ""))}
    elif sandbox_backend == "e2b":
        # e2b.dev integration
        pass


@mcp_tool
def rag_retrieve(query: str, top_k: int = 3) -> dict:
    """RAG: retrieve relevant documents."""
    # InMemoryEmbeddingRetriever or pgvector query
    return {"documents": [...], "scores": [...]}


@mcp_tool
def web_search(query: str) -> dict:
    """Search the web."""
    return {"results": [...]}


# ... (other custom tools matching agent_spec.tools)
```

### 2.2 SSE Event Buffering & Recovery

When Go calls a tool via the ReverseProxy, Python returns 202 Accepted with a stream URL:

```python
# FastMCP server
@app.post("/tools/{tool_name}/invoke")
async def invoke_tool(tool_name: str, request_body: dict):
    task_id = str(uuid.uuid4())
    
    # Store task metadata
    ACTIVE_TASKS[task_id] = {
        "status": "running",
        "events": [],  # Buffer last 10 events
        "future": asyncio.create_task(
            execute_tool_async(tool_name, request_body, task_id)
        )
    }
    
    return {
        "task_id": task_id,
        "stream_url": f"/stream/{task_id}"
    }


@app.get("/stream/{task_id}")
async def stream_events(task_id: str):
    """SSE endpoint. Go subscribes on this URL."""
    async def event_generator():
        task = ACTIVE_TASKS[task_id]
        last_index = 0
        
        while True:
            # Send buffered events
            for event in task["events"][last_index:]:
                yield f"data: {json.dumps(event)}\n\n"
            last_index = len(task["events"])
            
            # If done, send final event and close
            if task["status"] in ["done", "failed"]:
                yield f"data: {json.dumps({'type': 'done', 'status': task['status']})}\n\n"
                del ACTIVE_TASKS[task_id]  # Clean up
                break
            
            # Wait a bit before polling again
            await asyncio.sleep(0.1)
    
    return StreamingResponse(event_generator(), media_type="text/event-stream")


async def execute_tool_async(tool_name: str, args: dict, task_id: str):
    """Execute tool and buffer events."""
    task = ACTIVE_TASKS[task_id]
    
    try:
        task["events"].append({"type": "started", "timestamp": now()})
        
        # Call the MCP tool
        result = MCP_TOOLS[tool_name](**args)
        
        task["events"].append({
            "type": "done",
            "result": result,
            "timestamp": now()
        })
        task["status"] = "done"
    except Exception as e:
        task["events"].append({
            "type": "error",
            "error": str(e),
            "timestamp": now()
        })
        task["status"] = "failed"
        
    # Keep last 10 events in buffer (space/time tradeoff)
    if len(task["events"]) > 10:
        task["events"] = task["events"][-10:]
```

### 2.3 Health Check Endpoint

```python
@app.get("/health")
async def health_check():
    # Go's ReverseProxy uses this every 10 seconds
    return {"status": "ok", "timestamp": now()}
```

---

## 3. Code Execution Sandbox (Code Safety)

`SANDBOX_BACKEND` controls isolation level. When a DAG task calls the `code_exec` MCP tool, Go validates the agent spec permits it, then routes to Python.

| Backend | Env value | Isolation | When to use |
|---|---|---|---|
| **RestrictedPython** | `restrictedpython` | AST-level — strips `os`, `subprocess`, `open`, `__import__` | Windows dev, zero infra |
| **e2b.dev** | `e2b` | Cloud microVM, full OS isolation | Staging + production, no local infra |

**Python MCP Tool:**
```python
@mcp_tool
def code_exec(code: str, sandbox_backend: str = "restrictedpython") -> dict:
    """Execute Python code safely."""
    backend = sandbox_backend or os.getenv("SANDBOX_BACKEND", "restrictedpython")
    
    if backend == "e2b":
        import e2b
        async with e2b.AsyncSandbox() as sb:
            result = await sb.run_code(code)
            return {"result": result.text, "status": "ok"}
    else:  # restrictedpython
        from RestrictedPython import compile_restricted, safe_globals
        byte_code = compile_restricted(code, filename="<agent>", mode="exec")
        loc = {}
        exec(byte_code, safe_globals, loc)  # noqa: S102
        return {"result": str(loc.get("result", "")), "status": "ok"}
```

**Go's enforcement:** Agent spec must explicitly include `"tools": ["code_exec"]`. If omitted, Go rejects any task that tries to call it.

---

## 4. Scaling to 10,000 Users

### 4.1 Bottleneck Map

```
10,000 users  →  ~500 req/s peak
    │
    ▼
Go (stateless DAG orchestrator)   trivial horizontal scale
    │  HTTP POST to Python ReverseProxy
    ▼
Python Worker Pool               N workers, round-robin + least-conn
    │  Each worker executes MCP tools
    ▼
Ollama / vLLM                    THE real bottleneck — GPU inference
```

### 4.2 Latency Budget (p95, first token)

```
Go circuit breaker + auth + rate limit         <   5 ms
Fetch memory context (embed + ANN)             <  50 ms
Call Python planner MCP tool (over HTTP)       < 200 ms
DAG topo-sort (deterministic, O(n))            <   5 ms
Dispatch parallel task goroutines              <   2 ms
Wait for first task completion (fastest node)  < 400 ms  ← first token from LLM
Wait for slowest task (depends)                < 600 ms
Call Python evaluate MCP tool                  < 200 ms
Call Python memory_summarizer MCP tool         < 150 ms
────────────────────────────────────────────────────────
p95 first-token (simple query)                 < 500 ms
p95 final answer (complex DAG)                 < 800 ms
```

### 4.3 Scaling Levers

| Layer | Action | Config |
|---|---|---|
| Go | Horizontal pods, L4 LB | Stateless, auto scale |
| Go | ReverseProxy tuning | `WORKER_MAX_CONCURRENCY=5`, `HEALTH_INTERVAL_SEC=10` |
| Go | Memory cache TTL | `MEMORY_CACHE_TTL_SEC=300` |
| Python | More worker replicas | `PYTHON_WORKERS="http://py-1:8000,http://py-2:8000,..."` |
| Python | Tune batching | `MESSAGE_BATCH_THRESHOLD=15`, `MEMORY_FLUSH_INTERVAL_SEC=1800` |
| Python | Disable expensive tools | Config tool availability per agent |
| LLM | Multiple Ollama replicas | LB in front of `OLLAMA_BASE_URL` |
| LLM | Quantized models | `Q4_K_M` GGUF → 4x throughput |
| LLM | Use smaller models | Planner: `gemma2:2b`, Evaluator: `gemma2:2b` |

---

## 5. Invariants (Never Break These)

**Go Ownership (State & Orchestration):**

| Rule | Enforced by |
|---|---|
| Go owns all DAG orchestration (planning, dispatch, topo-sort, checkpointing) | Architecture boundary |
| Go owns all DB writes (checkpoint, memory, audit logs) | Go goroutines only write |
| Go owns session state and task checkpoints | Postgres transactions |
| Go owns embedding calls (Ollama) | Go HTTP client |
| Go owns ANN search queries (pgvector) | Go SQL queries |
| Go owns ReverseProxy and worker health checks | Go health-check loop |
| Go enforces DAG failure policy (fail-fast by default) | Go cancels goroutines |
| Go enforces refinement cap (`refinement_generation <= 2`) | Go rejects DAG if exceeded |

**Python Constraints (Stateless Tools):**

| Rule | Enforced by |
|---|---|
| Python never queries a DB directly | Python is tool server, not data layer |
| Python never tracks state across requests | Each request is independent |
| Python MCP tools are **stateless functions**: {inputs} → {output} | No side effects |
| Python SSE buffering is temporary (loses if worker dies) | Final results persisted by Go |

**DAG & Task Execution:**

| Rule | Enforced by |
|---|---|
| DAG cycle detection runs before execution | Go `topo_sort()` validates |
| All dependencies resolved before task dispatch | Go checks `depends_on` graph |
| Parallel tasks killed on first failure (fail-fast) | Go `cancel` context |
| Node retry cap is 3 | Go checks `retry_count <= 3` |
| Refinement attempts capped at 2 new generations | Go checks `refinement_generation <= 2` |
| Task output checkpointed to Postgres immediately after completion | Go goroutine writes |
| Event sequence preserved (SSE events ordered by timestamp) | Postgres `tool_execution_events` with `created_at` |

**Memory & ANN Search:**

| Rule | Enforced by |
|---|---|
| Memory always scoped by `user_id` (no cross-user leakage) | SQL `WHERE user_id = ?` constraint |
| Memory flushing triggered every 15 messages OR 30min | Go `message_count` counter |
| Memory entries are immutable once flushed to Postgres | INSERT-only, no UPDATE |
| ANN search limited to user's own memory | SQL query constraint |

**Tool Execution:**

| Rule | Enforced by |
|---|---|
| Only tools in `agent_spec.tools[]` are callable | Go ReverseProxy allow-list |
| `code_exec` tool only runs if in agent spec | Go rejects task otherwise |
| Guardrails run before (input) & after (output) task execution | Python MCP tool calls |
| Tool audit logged for all executions | Go writes `tool_audit_log` |

**Confidence & Quality:**

| Rule | Enforced by |
|---|---|
| Confidence score written if eval fails after refinement cap | Go calculates from eval output |
| `confidence_warning` persisted in both SSE event AND `agent_sessions` | Go dual-write |
| Confidence reason always populated (never NULL) | Go validation before insert |

**Fault Tolerance:**

| Rule | Enforced by |
|---|---|
| Crashed worker is removed after 2 health-check failures | Go health loop removes from pool |
| Crashed Go instance recovers from last checkpoint | Go reads `last_checkpoint_node_id` on restart |
| Python worker death → Go retries node with fresh worker | Go ReverseProxy routes to `pool.SelectLeastLoaded()` |
| SSE reconnection replays events from Postgres | Go subscribes with `?since=timestamp` parameter |

---

---

## 6. Contract — MCP Tool Signatures (Python Server)

All communication from Go to Python is via stateless MCP HTTP calls. Python tools return JSON.

### 6.1 Planner Tool

```python
@mcp_tool
def plan_agent_execution(
    message: str,
    agent_spec: dict,  # {name, tools[], model, system_prompt, ...}
    memory_context: str,  # Pre-fetched context (text) from Go
    feedback: str = ""  # If refinement: feedback from failed eval
) -> dict:
    """
    Generate a DAG of tasks to execute.
    Go handles context retrieval; Python just plans.
    """
    return {
        "tasks": [
            {"id": "t1", "tool": "web_search", "args": {"query": "..."}},
            {"id": "t2", "tool": "rag", "args": {...}, "depends_on": ["t1"]},
        ],
        "depends_on": {"t2": ["t1"]},  # Explicit dependency map
        "reasoning": "Step by step reasoning..."
    }
```

### 6.2 Memory Summarizer Tool

```python
@mcp_tool
def memory_summarizer(
    session_id: str,
    last_n_messages: list[dict]  # [{role: "user"|"assistant", content: "..."}]
) -> dict:
    """Summarize recent messages and extract entities."""
    return {
        "summary": "User asked about X, we discussed Y...",
        "entities": [
            {"name": "John Smith", "type": "person", "context": "CEO of YYY"},
            {"name": "Acme Corp", "type": "organization", "context": "Founded 2010"}
        ]
    }
```

### 6.3 Evaluate Tool

```python
@mcp_tool
def evaluate_response(
    message: str,
    dag_output: dict,  # Full execution results
    agent_spec: dict
) -> dict:
    """Judge whether output answers the user's question."""
    return {
        "eval_ok": False,  # bool
        "score": 0.65,     # 0.0-1.0
        "feedback": "Output is incomplete; should mention X"  # For refinement
    }
```

### 6.4 Guardrails (Input & Output)

```python
@mcp_tool
def guardrail_in(message: str) -> dict:
    """Validate input safety. Block jailbreaks."""
    return {
        "ok": True,
        "reason": "Message is safe"
    }

@mcp_tool
def guardrail_out(response: str) -> dict:
    """Validate output safety. Block harmful answers."""
    return {
        "ok": True,
        "reason": "Response is safe and accurate"
    }
```

### 6.5 Tool Executors (Built-in)

| Tool | Input | Output |
|---|---|---|
| `code_exec` | `{code: str, sandbox_backend: str}` | `{result: str, status: str}` |
| `rag_retrieve` | `{query: str, top_k: int}` | `{documents: [...], scores: [...]}` |
| `web_search` | `{query: str}` | `{results: [...], count: int}` |
| `math_eval` | `{expression: str}` | `{result: float}` |
| (custom tools) | (agent-defined) | (agent-defined) |

### 6.6 HTTP Protocol (Go calls Python)

**Request:**
```
POST /tools/{tool_name}/invoke
Content-Type: application/json

{
  "args": {
    "message": "user input",
    "agent_spec": {...},
    "memory_context": "...",
    ...
  }
}
```

**Response (202 Accepted):**
```
{
  "task_id": "uuid-123",
  "stream_url": "/stream/uuid-123"
}
```

**SSE Stream (Go subscribes):**
```
GET /stream/uuid-123

data: {"type": "started", "timestamp": "2026-05-02T10:15:00Z"}
data: {"type": "progress", "pct": 20, "message": "Parsing..."}
data: {"type": "done", "result": {...}, "status": "ok"}
```

### 6.7 Health Check

```
GET /health

{
  "status": "ok",
  "timestamp": "2026-05-02T10:15:00Z"
}
```

---

## 7. Key Environment Variables

### Go

| Variable | Default | Purpose |
|---|---|---|
| `PYTHON_WORKERS` | `"http://localhost:8000"` | Comma-separated URLs of Python worker instances |
| `WORKER_HEALTH_INTERVAL_SEC` | `10` | How often to ping /health on Python workers |
| `WORKER_MAX_CONCURRENCY` | `5` | Max concurrent connections per worker |
| `AUTH_TOKEN` | — | Bearer token (replace with JWKS URL in prod) |
| `REDIS_ADDR` | `localhost:6379` | Session history store |
| `POSTGRES_DSN` | — | Agent orchestration DB |
| `OLLAMA_BASE_URL` | `http://localhost:11434` | Used by Go for embedding calls |
| `MEMORY_CACHE_TTL_SEC` | `300` | How long to cache ANN search results |
| `MESSAGE_BATCH_THRESHOLD` | `15` | Flush memory after N messages |
| `MEMORY_FLUSH_INTERVAL_SEC` | `1800` | Max time before memory flush (30 min) |
| `REFINEMENT_MAX_GENERATION` | `2` | Max refinement attempts |

### Python

| Variable | Default | Purpose |
|---|---|---|
| `FASTMCP_PORT` | `8000` | Listen port for MCP server |
| `WORKERS` | `1` | Number of async workers for tool execution |
| `LLM_MODEL` | `gemma2:2b` | Default executor model |
| `PLANNER_MODEL` | `gemma2:2b` | Used for plan generation |
| `EVALUATOR_MODEL` | `gemma2:2b` | Used for eval scoring |
| `OLLAMA_BASE_URL` | `http://localhost:11434` | Ollama endpoint for LLM calls |
| `SANDBOX_BACKEND` | `restrictedpython` | `restrictedpython` (dev) \| `e2b` (prod) |
| `E2B_API_KEY` | — | Required when `SANDBOX_BACKEND=e2b` |
| `SYSTEM_PROMPT` | — | Global fallback system prompt |
| `LANGFUSE_PUBLIC_KEY` | — | Langfuse SDK auth |
| `LANGFUSE_SECRET_KEY` | — | Langfuse SDK auth |
| `LANGFUSE_BASE_URL` | `http://localhost:3000` | Self-hosted Langfuse UI/API URL |

---

## 8. Observability & Monitoring

### 8.1 Metrics to Track

**Go Orchestrator:**
- `dag_executions_total` (counter) — number of DAGs started
- `dag_completions_total` (counter, label: `status` ∈ {success, failed, refinement_cap_reached})
- `dag_latency_p95_ms` (histogram) — time from planner call to done event
- `task_dispatch_latency_p95_ms` (histogram) — time to dispatch task to worker
- `worker_health_failures` (counter, label: `worker_url`) — health check failures
- `refinement_attempts` (histogram) — distribution of refinement counts per DAG
- `checkpoint_writes_total` (counter) — DB writes to `agent_task_nodes`
- `memory_flush_total` (counter) — calls to Python `memory_summarizer`

**Python Worker Pool:**
- `tool_execution_total` (counter, label: `tool`, `status`) — tool invocations and outcomes
- `tool_latency_p95_ms` (histogram, label: `tool`) — per-tool execution time
- `worker_active_connections` (gauge, label: `worker_url`) — current concurrent usage
- `sse_event_count` (counter, label: `tool`, `event_type`) — buffered events

**Database:**
- `postgres_checkpoint_insert_latency_ms` (histogram) — write latency to `agent_task_nodes`
- `postgres_ann_search_latency_ms` (histogram) — ANN query latency on `agent_memory_log`

### 8.2 Logging

**Go:**
```
{
  "level": "info|warn|error",
  "timestamp": "2026-05-02T10:15:00Z",
  "request_id": "uuid",
  "session_id": "uuid",
  "event": "dag_started|task_dispatched|task_completed|refinement_triggered|eval_failed",
  "dag_id": "uuid",
  "node_id": "t1|t2",
  "duration_ms": 123,
  "error": "..."
}
```

**Python:**
```
{
  "level": "info|warn|error",
  "timestamp": "2026-05-02T10:15:00Z",
  "tool": "plan_agent_execution|code_exec|rag_retrieve",
  "task_id": "uuid",
  "duration_ms": 456,
  "status": "ok|error|timeout",
  "error": "..."
}
```

### 8.3 Alerting

| Alert | Condition | Action |
|---|---|---|
| High DAG failure rate | `dag_failures_5m / dag_completions_5m > 0.1` | Page on-call |
| Worker health degradation | `worker_health_failures_5m > 5 per worker` | Restart worker |
| Slow ANN search | `postgres_ann_search_p95 > 500ms` | Investigate connectivity |
| Refinement loop saturation | `refinement_cap_reached_rate > 5/min` | Review eval model |
| Memory flush lag | `last_memory_flush_age > 2h` | Manual trigger or investigate |

### 8.4 Debugging (Manual Queries)

```sql
-- What's the status of tasks for a specific session?
SELECT task_id, node_id, status, retry_count, created_at, completed_at
FROM agent_task_nodes
WHERE task_id = 'SESSION_UUID'
ORDER BY created_at;

-- How many refinements happened for this session?
SELECT MAX(refinement_generation) as max_generation
FROM agent_task_nodes
WHERE task_id = 'SESSION_UUID' AND original_node_id IS NOT NULL;

-- What's the memory for a specific user?
SELECT memory_type, created_at, content
FROM agent_memory_log
WHERE user_id = 'USER_UUID'
ORDER BY created_at DESC
LIMIT 10;

-- Which workers are healthy?
SELECT worker_url, is_healthy, failure_count, active_connections
FROM worker_pool_status
ORDER BY is_healthy DESC, active_connections;
```

### 8.5 Langfuse — LLM Observability (Self-Hosted)

Langfuse is the LLM observability layer. It captures prompt/response pairs, token usage, eval scores, and tool latencies. Self-hosted via Docker Compose alongside the stack.

**What Langfuse captures that Prometheus cannot:**
- Full prompt text in / response text out per LLM call
- Token usage and cost per call (even with Ollama)
- Eval scores tracked over time per session
- DAG execution tree as trace hierarchy

**Architecture:**
```
Go (OTel spans via go.opentelemetry.io/otel)
    │  OTLP/HTTP  →  :4318
    ▼
Langfuse self-hosted OTel endpoint
    ◄──────────────────────────────
Python (langfuse SDK @observe decorator)
    │  OTLP/HTTP  →  :4318
    ▼
Langfuse self-hosted OTel endpoint
```

Both Go and Python tag all spans/traces with `session_id` — Langfuse groups them for per-session visibility. No distributed `traceparent` propagation required (correlated by `session_id`, not unified trace hierarchy).

**Go — OTel instrumentation:**

```go
// go.mod additions:
// go.opentelemetry.io/otel
// go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp
// go.opentelemetry.io/otel/sdk/trace

func initOTel(ctx context.Context) (*sdktrace.TracerProvider, error) {
    exporter, err := otlptracehttp.New(ctx,
        otlptracehttp.WithEndpoint(os.Getenv("LANGFUSE_OTLP_ENDPOINT")), // e.g. localhost:4318
        otlptracehttp.WithHeaders(map[string]string{
            "Authorization": "Basic " + base64(
                os.Getenv("LANGFUSE_PUBLIC_KEY") + ":" + os.Getenv("LANGFUSE_SECRET_KEY"),
            ),
        }),
        otlptracehttp.WithInsecure(),
    )
    tp := sdktrace.NewTracerProvider(
        sdktrace.WithBatcher(exporter),
        sdktrace.WithResource(resource.NewWithAttributes(
            semconv.SchemaURL,
            semconv.ServiceName("go-orchestrator"),
        )),
    )
    otel.SetTracerProvider(tp)
    return tp, err
}

// Per DAG execution:
tracer := otel.Tracer("go-orchestrator")

ctx, dagSpan := tracer.Start(ctx, "dag_execution",
    trace.WithAttributes(
        attribute.String("session_id", sessionID),
        attribute.String("user_id", userID),
        attribute.String("agent_id", agentID),
        attribute.Int("refinement_generation", gen),
    ),
)
defer dagSpan.End()

// Per task:
_, taskSpan := tracer.Start(ctx, "task_execute",
    trace.WithAttributes(
        attribute.String("task_id", task.ID),
        attribute.String("tool_name", task.Tool),
        attribute.String("session_id", sessionID),
    ),
)
taskSpan.SetAttributes(attribute.String("status", result.Status))
taskSpan.End()
```

**Python — Langfuse SDK instrumentation:**

```python
# pyproject.toml additions:
# langfuse >= 3.0.0

from langfuse.decorators import observe, langfuse_context

@observe(as_type="generation")  # Langfuse captures: prompt, response, tokens, latency
def plan_agent_execution(message: str, agent_spec: dict, memory_context: str) -> dict:
    langfuse_context.update_current_trace(
        session_id=agent_spec.get("session_id"),
        user_id=agent_spec.get("user_id"),
        tags=["planner"],
    )
    # ... LLM call ...

@observe(as_type="generation")
def evaluate_response(message: str, dag_output: dict, agent_spec: dict) -> dict:
    langfuse_context.update_current_trace(
        session_id=agent_spec.get("session_id"),
        tags=["evaluator"],
    )
    # ... eval LLM call ...

@observe()  # Generic span — not an LLM call
def code_exec(code: str, sandbox_backend: str = "restrictedpython") -> dict:
    # ... sandbox execution ...
```

**Environment variables (both Go and Python):**

| Variable | Side | Purpose |
|---|---|---|
| `LANGFUSE_PUBLIC_KEY` | Go + Python | OTel/SDK auth |
| `LANGFUSE_SECRET_KEY` | Go + Python | OTel/SDK auth |
| `LANGFUSE_OTLP_ENDPOINT` | Go | OTLP/HTTP endpoint (e.g. `http://localhost:4318`) |
| `LANGFUSE_BASE_URL` | Python | SDK base URL (e.g. `http://localhost:3000`) |

**Self-hosting (Docker Compose addition):**
```yaml
langfuse:
  image: langfuse/langfuse:latest
  ports:
    - "3000:3000"   # Langfuse UI
    - "4318:4318"   # OTel OTLP/HTTP endpoint
  environment:
    DATABASE_URL: postgresql://postgres:postgres@postgres:5432/langfuse
    NEXTAUTH_SECRET: changeme
    SALT: changeme
  depends_on:
    - postgres
```

Langfuse shares the existing Postgres instance (separate `langfuse` database).

**What you see in Langfuse UI per session:**
- DAG execution span (Go) with `session_id`, `refinement_generation`
- Child task spans (Go) per tool dispatched
- Python `plan_agent_execution` generation: full prompt + DAG JSON output + latency
- Python `evaluate_response` generation: eval prompt + score + feedback
- Token counts and model used per LLM call
- Eval scores plotted over time (filterable by `agent_id`, `user_id`)

### 8.6 Go — Environment Variables (Observability additions)

| Variable | Default | Purpose |
|---|---|---|
| `LANGFUSE_PUBLIC_KEY` | — | Langfuse OTel auth |
| `LANGFUSE_SECRET_KEY` | — | Langfuse OTel auth |
| `LANGFUSE_OTLP_ENDPOINT` | `http://localhost:4318` | OTLP/HTTP export target |
| `OTEL_SERVICE_NAME` | `go-orchestrator` | Service name in traces |

---

## 9. DAG Execution & Refinement Details

### 9.1 Topological Sort & Cycle Detection

Before dispatching any task, Go validates the DAG:

```go
func TopoSort(tasks []Task) ([]Task, error) {
    index := make(map[string]Task)
    inDegree := make(map[string]int)
    
    for _, t := range tasks {
        index[t.ID] = t
        inDegree[t.ID] = 0
    }
    
    // Count in-degree
    for _, t := range tasks {
        for _, dep := range t.DependsOn {
            if _, ok := index[dep]; !ok {
                return nil, fmt.Errorf("unknown dependency: %s", dep)
            }
            inDegree[t.ID]++
        }
    }
    
    // Kahn's algorithm
    queue := []Task{}
    for _, t := range tasks {
        if inDegree[t.ID] == 0 {
            queue = append(queue, t)
        }
    }
    
    var order []Task
    for len(queue) > 0 {
        node := queue[0]
        queue = queue[1:]
        order = append(order, node)
        
        for _, t := range tasks {
            for _, dep := range t.DependsOn {
                if dep == node.ID {
                    inDegree[t.ID]--
                    if inDegree[t.ID] == 0 {
                        queue = append(queue, t)
                    }
                }
            }
        }
    }
    
    if len(order) != len(tasks) {
        return nil, errors.New("cycle detected in DAG")
    }
    return order, nil
}
```

### 9.2 Parallel Execution with Dependency Injection

```go
for _, batch := range topoSortedBatches {
    // All tasks in batch have no unresolved dependencies
    for _, task := range batch {
        go func(t Task) {
            // Build context from dependency outputs
            var depContext strings.Builder
            for _, depID := range t.DependsOn {
                depContext.WriteString(fmt.Sprintf("[%s result]: %v\n", depID, results[depID]))
            }
            
            // Inject dependency context into task args
            t.Args["context"] = depContext.String()
            
            // Dispatch to Python tool
            result, err := orchestrator.DispatchTool(ctx, t)
            if err != nil {
                t.Status = "failed"
                t.Error = err.Error()
            } else {
                t.Status = "done"
                t.Output = result
                // Checkpoint immediately
                db.InsertTaskNode(t)
            }
            results[t.ID] = result
        }(task)
    }
    
    // Wait for batch to complete
    <-batchDone
}
```

### 9.3 Refinement Loop (Max 2 Generations)

```go
func RefineDAG(
    ctx context.Context,
    originalDAG DAG,
    failureReason string,
    previousOutputs map[string]interface{},
    generation int,  // 0=original, 1=first refine, 2=second refine
) (DAG, error) {
    // Hard cap
    if generation >= 2 {
        return nil, fmt.Errorf("refinement cap reached (max 2 generations)")
    }
    
    // Call Python planner with failure feedback
    refinedDAGJson, err := invokeToolMCP(ctx, "plan_agent_execution", map[string]interface{}{
        "message": originalDAG.UserMessage,
        "agent_spec": originalDAG.AgentSpec,
        "memory_context": originalDAG.MemoryContext,
        "feedback": failureReason,  // Additional context for planner
    })
    
    newDAG := parseDAG(refinedDAGJson)
    newDAG.PreviousDAGID = originalDAG.ID
    newDAG.Refinement Generation = generation + 1 // Mark all new nodes as refined
    
    return newDAG, nil
}
```

### 9.4 Confidence Scoring on Shortcut

When refinement cap is reached (`generation >= 2`), Go calculates confidence:

```go
func CalculateConfidenceScore(evalResult EvalResult, refinementGeneration int) (float64, string) {
    baseScore := evalResult.EvalScore  // 0.0-1.0 from Python eval
    
    if refinementGeneration >= 2 {
        // Penalize for hitting refinement cap
        penalty := 0.15  // Reduce by 15%
        confidence := math.Max(0, baseScore - penalty)
        return confidence, "Hit refinement cap (max 2); answer is best-effort"
    }
    
    return baseScore, "Evaluation passed"
}
```

### 9.5 Failure Modes & Retry Logic

| Failure Mode | Cause | Retry Strategy | Max Retries | Fallback |
|---|---|---|---|---|
| Tool Timeout | Tool execution > 30s | Exponential backoff (1s, 2s, 4s) | 2 | Return "timeout" error to planner |
| MCP Server Down | gRPC connection failed | Reconnect with backoff; auto-dial new connection | 3 | Fail task; trigger refinement |
| Invalid Task Params | Missing `type`, invalid JSON in `args` | Log error; mark task failed | 0 | Skip task; return error in results |
| Python Exception | Tool logic error (KeyError, TypeError, etc.) | No retry; pass traceback to planner | 0 | Trigger refinement with error context |
| Eval Failure | Eval agent fails or inconsistent | Retry entire DAG execution with feedback | 1 time (becomes generation 1) | Force refinement |
| **At Refinement Cap** | DAG still failing after 2 gens | **NO MORE RETRIES** | N/A | **Shortcut to best-effort response + confidence score** |

### 9.6 Checkpointing & Session State

After each task completes, Go checkpoints:

```go
// Immediate checkpoint after task completion
db.InsertTaskNode(ctx, &TaskNode{
    SessionID: sessionID,
    DAGGenerationID: dagGen,
    TaskID: task.ID,
    Type: task.Type,
    Status: "done",
    Output: result,
    ExecutedAt: time.Now(),
})

// Periodic flush of session state
if !sessionState.IsDirty {
    return  // Skip if no changes
}

db.UpdateSessionState(ctx, map[string]interface{}{
    "last_dag_generation": currentGen,
    "current_memory": sessionMemory,
    "total_tasks_executed": taskCount,
    "created_at": sessionState.CreatedAt,
    "updated_at": time.Now(),
})
```

---

## 10. Agent Memory Model

### 10.1 Memory Type Map

All memory lives in **Postgres + pgvector**. No ChromaDB, no separate vector service.

```
Redis                     Postgres (pgvector)
─────────────────────     ──────────────────────────────────────────────
session history[]         memory_summary    (user_id scoped)
  token-bounded                             long-term fact summaries
  compressed on overflow
                          memory_entity     (user_id scoped)
                                            people, orgs, preferences

                          memory_workflow   (agent_id scoped, shared)
                                            successful DAG plans

                          memory_toolbox    (platform-wide, v2)
                                            MCP tool definitions for hybrid search

                          tool_log          (user_id + session_id, append-only SQL)
                                            raw tool inputs/outputs for audit
```

### 10.2 Postgres Schema

```sql
-- Long-term summaries (one-sentence facts per session)
CREATE TABLE memory_summary (
    id           UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id      UUID NOT NULL,
    agent_id     TEXT NOT NULL,
    session_id   UUID NOT NULL,
    embedding    vector(768) NOT NULL,
    text         TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON memory_summary USING ivfflat (embedding vector_cosine_ops);
CREATE INDEX ON memory_summary (user_id);

-- Entity memory (people, orgs, systems mentioned)
CREATE TABLE memory_entity (
    id           UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id      UUID NOT NULL,
    entity_name  TEXT NOT NULL,
    entity_type  TEXT NOT NULL,   -- "person" | "org" | "system" | "preference" | ...
    embedding    vector(768) NOT NULL,
    context_text TEXT NOT NULL,
    last_seen    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, entity_name)
);
CREATE INDEX ON memory_entity USING ivfflat (embedding vector_cosine_ops);
CREATE INDEX ON memory_entity (user_id);

-- Workflow memory (successful DAG plans, agent-scoped)
CREATE TABLE memory_workflow (
    id              UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    agent_id        TEXT NOT NULL,
    query_embedding vector(768) NOT NULL,
    dag_json        JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON memory_workflow USING ivfflat (query_embedding vector_cosine_ops);
CREATE INDEX ON memory_workflow (agent_id);

-- Tool audit log (no vector column needed)
CREATE TABLE tool_log (
    id          UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id     UUID NOT NULL,
    session_id  UUID NOT NULL,
    tool_name   TEXT NOT NULL,
    args_json   JSONB NOT NULL,
    result_json JSONB NOT NULL,
    duration_ms INT NOT NULL,
    error       BOOL NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON tool_log (user_id, session_id);
CREATE INDEX ON tool_log (tool_name, error);  -- for per-tool error rate queries

-- Toolbox (v2 — populated by Go MCP adapter, platform-wide)
CREATE TABLE memory_toolbox (
    tool_key     TEXT PRIMARY KEY,   -- "{server_id}_{tool_name}"
    server_id    TEXT NOT NULL,
    tool_name    TEXT NOT NULL,
    description  TEXT NOT NULL,
    embedding    vector(768) NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON memory_toolbox USING ivfflat (embedding vector_cosine_ops);
```

### 10.3 Read Path (Python `memory_inject` node)

```python
async def memory_inject(state: RouterState) -> RouterState:
    user_id = state["user_id"]
    agent_id = state["agent_id"]
    query_vec = await embed(state["messages"][-1].content)

    # Two ANN queries in parallel — both user_id scoped
    summary_rows, entity_rows = await asyncio.gather(
        pgvector_search("memory_summary", query_vec, user_id=user_id, top_k=3),
        pgvector_search("memory_entity",  query_vec, user_id=user_id, top_k=3),
    )

    # Compress history if Go flagged it
    if state.get("compress_history"):
        summary_block = await llm_summarise(state["history"])
        history_context = f"[Conversation summary]: {summary_block}"
    else:
        history_context = None

    # Build context prefix
    facts = [f"[fact]: {r.text}" for r in summary_rows]
    entities = [f"[entity]: {r.entity_name} ({r.entity_type}) — {r.context_text}" for r in entity_rows]
    prefix = "\n".join(filter(None, [history_context] + facts + entities))

    if prefix:
        state["messages"].insert(0, SystemMessage(content=prefix))
    return state
```

### 10.4 Workflow Hint Read Path (Python `plan` node)

```python
async def plan_node(state: RouterState) -> RouterState:
    agent_id = state["agent_id"]
    query_vec = await embed(state["messages"][-1].content)

    # ANN search for similar past successful plans — agent_id scoped
    workflow_rows = await pgvector_search(
        "memory_workflow", query_vec, agent_id=agent_id, top_k=1, threshold=0.85
    )
    hint = ""
    if workflow_rows:
        hint = f"\n[Workflow hint]: A similar request previously succeeded with this plan:\n{workflow_rows[0].dag_json}"

    plan = await llm_plan(state, hint=hint)
    return {**state, "plan": plan}
```

### 10.5 Write Path (Go `onStreamDone`)

```go
var modelContextWindow = map[string]int{
    "gemma2:2b":    8192,
    "gemma2:9b":    8192,
    "llama3:8b":    8192,
    "llama3:70b":   8192,
    "mistral:7b":   32768,
    "mixtral:8x7b": 32768,
    // default fallback: 4096 (conservative)
}

type MemoryPolicy struct {
    Summary  string `json:"summary"`   // "always" | "eval_ok_only" | "never"
    Workflow string `json:"workflow"`
    Entity   string `json:"entity"`
    Knowledge string `json:"knowledge"`
}

func onStreamDone(ctx context.Context, userID, agentID string,
    done DoneEvent, policy MemoryPolicy) {

    if policy.Summary != "never" && (done.EvalOk || policy.Summary == "always") {
        go writeMemorySummary(ctx, userID, agentID, done.Summary)
    }
    if policy.Workflow != "never" && done.DagJson != "" &&
        (done.EvalOk || policy.Workflow == "always") {
        go writeMemoryWorkflow(ctx, agentID, done.DagJson)
    }
    if policy.Entity != "never" {
        go writeMemoryEntities(ctx, userID, done.Entities)
    }
}

func writeMemoryWorkflow(ctx context.Context, agentID, dagJson string) {
    vec := ollamaEmbed(ctx, dagJson[:min(200, len(dagJson))]) // embed first 200 chars as query proxy
    // Deduplication: skip if near-duplicate already exists
    var count int
    db.QueryRow(`SELECT COUNT(*) FROM memory_workflow
                 WHERE agent_id=$1 AND (query_embedding <=> $2) < 0.1`,
        agentID, vec).Scan(&count)
    if count > 0 {
        return
    }
    db.Exec(`INSERT INTO memory_workflow (agent_id, query_embedding, dag_json)
             VALUES ($1, $2, $3)`, agentID, vec, dagJson)
}

func writeMemoryEntities(ctx context.Context, userID string, entities []Entity) {
    for _, e := range entities {
        vec := ollamaEmbed(ctx, e.Name+" "+e.Type)
        db.Exec(`INSERT INTO memory_entity
                   (user_id, entity_name, entity_type, embedding, context_text, last_seen)
                 VALUES ($1,$2,$3,$4,$5,now())
                 ON CONFLICT (user_id, entity_name)
                 DO UPDATE SET last_seen=now(), context_text=EXCLUDED.context_text`,
            userID, e.Name, e.Type, vec, e.ContextText)
    }
}
```

### 10.6 History Token Bounding (Go `on_request_start`)

```go
func boundHistory(history []Turn, model string) (bounded []Turn, compress bool) {
    limit, ok := modelContextWindow[model]
    if !ok {
        limit = 4096
    }
    threshold := int(float64(limit) * 0.8)

    total := 0
    for _, t := range history {
        total += estimateTokens(t.Content) // len(words) * 1.3
    }
    if total <= threshold {
        return history, false
    }
    // Keep last 5 turns for continuity, discard the rest
    start := max(0, len(history)-5)
    return history[start:], true  // compress=true signals Python to run rolling summary
}

func estimateTokens(text string) int {
    return int(float64(len(strings.Fields(text))) * 1.3)
}
```

### 10.7 Memory Policy Defaults

When an agent spec omits `memory_policy`, Go applies:

```go
var defaultMemoryPolicy = MemoryPolicy{
    Summary:   "eval_ok_only",
    Workflow:  "eval_ok_only",
    Entity:    "always",
    Knowledge: "never",  // v2
}
```

### 10.8 Toolbox Memory (v2 — Go MCP Adapter)

Not in v1. When the Go MCP adapter lands:

- Go calls each configured MCP server at startup → receives tool name + description
- Go embeds `server_name_tool_name + description` → writes to `memory_toolbox`
- Row key: `server_id + "_" + tool_name` — **idempotent**: skip insert if row already exists
- Python `memory_inject` will add a third parallel ANN query on `memory_toolbox` scoped to `WHERE tool_name IN state["tools"]` — Go's allowlist ceiling is always respected

---

## 11. User-defined Custom Agents

### 11.1 Scope (v1: Level A — Spec Only)

Users define agents by submitting a JSON spec. Go validates, writes to the `agents` table, and the 30s registry refresh makes it live. Python does not change — it executes whatever spec Go injects.

Level B (custom MCP tool endpoints) lands with the Go MCP adapter in v2. Level C (custom graph code) is permanently out of scope — it is arbitrary code execution in the production runtime.

### 11.2 REST API (Go — Gin)

```
POST   /agents           — create a custom agent
PUT    /agents/:id       — update (owner only)
DELETE /agents/:id       — delete (owner only)
GET    /agents/:id       — read spec
GET    /agents           — list caller's agents + public platform agents
```

### 11.3 Registration Payload

**Mandatory fields** (HTTP 422 if missing or empty):

| Field | Validation |
|---|---|
| `name` | Non-empty string, max 100 chars |
| `description` | Non-empty string, max 500 chars |
| `system_prompt` | Non-empty string, max 4000 chars |
| `type` | `"react"` or `"simple"` exactly |

**Optional fields with defaults:**

| Field | Default | Validation when provided |
|---|---|---|
| `model` | `LLM_MODEL` env var | Must exist in `modelContextWindow` map |
| `planner_model` | `PLANNER_MODEL` env var | Must exist in `modelContextWindow` map |
| `tools[]` | `[]` | Each entry must exist in platform MCP registry; ignored if `type=simple` |
| `sub_agents[]` | `[]` | Each entry must exist in `agents` table AND be public or owned by caller |
| `approval_required_tools[]` | `[]` | Must be a subset of `tools[]` |
| `evaluator_enabled` | `true` if react, `false` if simple | — |
| `max_iterations` | `2` | Integer 1–5 |
| `memory_policy` | `{"summary":"eval_ok_only","workflow":"eval_ok_only","entity":"always","knowledge":"never"}` | Per-type `always\|eval_ok_only\|never` |
| `sandbox_backend` | `"restrictedpython"` | `"restrictedpython"` or `"e2b"` only — `"subprocess"` never allowed for user agents |
| `is_public` | `false` | — |

**Minimal valid body:**
```json
{
  "name":          "My Assistant",
  "description":   "Helps me with research tasks",
  "system_prompt": "You are a helpful research assistant that...",
  "type":          "react"
}
```

### 11.4 Go Validation Logic

```go
func validateAgentSpec(spec AgentSpec, callerID string,
    mcpRegistry map[string]bool, agentsTable map[string]Agent) error {

    // Mandatory fields
    if strings.TrimSpace(spec.Name) == "" { return ErrMissingName }
    if strings.TrimSpace(spec.Description) == "" { return ErrMissingDescription }
    if strings.TrimSpace(spec.SystemPrompt) == "" { return ErrMissingSystemPrompt }
    if spec.Type != "react" && spec.Type != "simple" { return ErrInvalidType }
    if len(spec.SystemPrompt) > 4000 { return ErrSystemPromptTooLong }

    // Model validation
    if spec.Model != "" {
        if _, ok := modelContextWindow[spec.Model]; !ok {
            return fmt.Errorf("unknown model: %s", spec.Model)
        }
    }

    // Tools — only validated for react; silently ignored for simple
    if spec.Type == "react" {
        unknown := []string{}
        for _, t := range spec.Tools {
            if !mcpRegistry[t] { unknown = append(unknown, t) }
        }
        if len(unknown) > 0 {
            return fmt.Errorf("unknown tools: %v", unknown)
        }
    }

    // Sub-agents — must be public or owned by caller
    for _, sa := range spec.SubAgents {
        a, ok := agentsTable[sa]
        if !ok { return fmt.Errorf("unknown sub_agent: %s", sa) }
        if !a.IsPublic && a.OwnerUserID != callerID {
            return fmt.Errorf("sub_agent not accessible: %s", sa)
        }
    }

    // approval_required_tools must be subset of tools
    toolSet := make(map[string]bool, len(spec.Tools))
    for _, t := range spec.Tools { toolSet[t] = true }
    for _, t := range spec.ApprovalRequiredTools {
        if !toolSet[t] { return fmt.Errorf("approval_required_tool not in tools: %s", t) }
    }

    // max_iterations bounds
    if spec.MaxIterations < 1 || spec.MaxIterations > 5 {
        return ErrInvalidMaxIterations
    }

    return nil
}
```

### 11.5 Postgres Schema Addition

```sql
-- Extend agents table
ALTER TABLE agents ADD COLUMN owner_user_id UUID REFERENCES users(id);
ALTER TABLE agents ADD COLUMN is_public     BOOL NOT NULL DEFAULT false;
ALTER TABLE agents ADD COLUMN agent_type    TEXT NOT NULL DEFAULT 'react';  -- 'react' | 'simple'

-- Policy enforced at query level in Go
CREATE INDEX ON agents (owner_user_id);
CREATE INDEX ON agents (is_public) WHERE is_public = true;
```

### 11.6 Python — simple agent fast-path

When `agent_type = "simple"`, Python's gate node routes directly to a single LLM call node, bypassing plan, execute, evaluate, and the DAG entirely:

```python
def pick_gate(state: RouterState) -> str:
    if state.get("agent_type") == "simple":
        return "direct_answer"   # single LLM call, no planner
    # existing gate logic for react agents...
    return "plan" if needs_planning(state) else "direct_answer"
```

No new graph nodes — `direct_answer` is the same fast-path node the gate already routes to for simple queries in react agents. The only difference is it's forced unconditionally for `simple` type.

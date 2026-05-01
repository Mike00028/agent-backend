# Orchestration Plan: Go owns orchestration, Python owns LLM execution only

## Architecture Decision
Go handles all stateful orchestration work (routing, memory, context budgeting, rate limiting).  
Python becomes a stateless LLM executor — receives a pre-assembled prompt, streams tokens back.  
This keeps Python instances minimal (2-3 stateless workers vs 10-20 heavy instances).

---

## Phase 1 — Go internal packages (foundation)

### 1.1 — store/postgres.go
- New file: golang/services/api/internal/store/postgres.go
- Add pgx/v5 connection pool to go.mod
- Tables: sessions, messages, episodic_log, user_memory
- Functions: LoadConversation(sessionID), SaveTurn(sessionID, turn), LoadUserMemory(userID), AppendEpisodicEvent(event)
- All writes via async goroutine (never block request path)
- Use prepared statements; parameterized queries only (no string interpolation)

### 1.2 — store/redis.go
- New file: golang/services/api/internal/store/redis.go
- Add rueidis to go.mod
- Two caches: context cache (key=session_id, TTL=5 min), routing cache (key=query_fingerprint, TTL=60s)
- Functions: GetContextCache(sessionID), SetContextCache(sessionID, payload), GetRouteCache(fingerprint), SetRouteCache(fingerprint, graphType)

### 1.3 — router/workflow.go
- New file: golang/services/api/internal/router/workflow.go
- Workflow registry struct: name, description, triggerKeywords, embeddingVector ([]float32)
- Default workflows: chat, rag
- At startup: embed each workflow description via Ollama HTTP → store vectors in memory
- Functions: Register(workflow), All() []Workflow

### 1.4 — router/semantic.go
- New file: golang/services/api/internal/router/semantic.go
- OllamaEmbedder struct: base_url, model, timeout, http.Client
- Functions: Embed(ctx, text) ([]float32, error), CosineSimilarity(a, b []float32) float32
- Route(ctx, query) (graphType string, confidence float32, cached bool)
- Stage A: lexical keyword match (instant, no HTTP)
- Stage B: embed query → cosine similarity against workflow vectors → pick highest if > threshold
- Cache result in Redis by query fingerprint (sha256 of lowercased trimmed query)
- Fallback: always return "chat" if confidence < threshold or Ollama unreachable

### 1.5 — context/assembler.go
- New file: golang/services/api/internal/context/assembler.go
- Load: last N turns from Postgres (or Redis cache hit), compact rolling summary, user memory hints
- Enforce: maxTurns, maxTokensPerSection, total token budget cap
- Trim: drop oldest turns first, truncate retrieved chunks last
- Returns: AssembledContext{graphType, messages[]Turn, userHints string, tokenCount int}
- Token count: simple whitespace tokenizer first; swap for tiktoken-compatible table later

---

## Phase 2 — Go config + middleware updates

### 2.1 — config/config.go
- Extend Config struct: PostgresDSN, RedisAddr, OllamaBaseURL, EmbeddingModel, RoutingThreshold (float32), MaxContextTurns (int), MaxContextTokens (int), GlobalConcurrencyLimit (int)
- Load all from env with safe defaults

### 2.2 — middleware/ratelimit.go
- Extend from IP-only to per-user rate limiting (keyed by session_id or user_id from auth header)
- Add global concurrency semaphore (channel-based) — blocks request before gRPC call if Python slots full
- On semaphore timeout: return 429 with Retry-After header

### 2.3 — middleware/auth.go
- Implement real auth (JWT validation or API key lookup)
- Attach user_id and session_id to gin context for downstream use

---

## Phase 3 — Go handler rewrite

### 3.1 — handler/chat.go Stream() and Invoke()
- Inject store and router dependencies via ChatHandler struct
- Pre-gRPC pipeline (in order):
  1. Auth/session validation
  2. Rate limit check
  3. Acquire global concurrency slot
  4. Load context: Redis hit → Postgres fallback
  5. Semantic routing: Redis cache hit → lexical match → embedding similarity → default
  6. Context budget enforcement: trim to token cap
  7. Build AgentRequest: message, graph_type in options, context_messages in options, user_hints in options
  8. Call Python gRPC (StreamAgent or RunAgent)
- Post-gRPC:
  1. Release concurrency slot
  2. Async goroutine: append episodic log to Postgres
  3. Async goroutine: update context cache in Redis
  4. Stream events to client

---

## Phase 4 — Python simplification

### 4.1 — servicer/agent_servicer.py
- Remove _semaphore (Go enforces concurrency limit upstream)
- Remove _select_graph routing (Go sends graph_type in options)
- Remove DB connections (Go handles all memory)
- Read context_messages, user_hints from options → inject into graph state
- Python receives clean pre-assembled state, runs graph, streams tokens

### 4.2 — config.py
- Remove grpc_max_concurrent (Go controls this now)
- Keep: llm model, ollama url, rag_top_k (Python still runs retriever inside graph if graph_type==rag)
- Note: RAG retrieval stays in Python because it runs inside the LangGraph node during generation

### 4.3 — pyproject.toml
- Add asyncpg or psycopg3 only if Python still needs any DB access (it should not after Phase 4)
- Remove any routing/classification dependencies if added

---

## Phase 5 — Database schema

### 5.1 — migrations/
- sessions: id (uuid), user_id, created_at, last_active_at
- messages: id, session_id (fk), role, content, token_count, created_at
- conversation_summaries: session_id (fk), summary_text, turn_count, updated_at
- user_memory: user_id, key, value, confidence, source, updated_at
- episodic_log: id, session_id, user_id, graph_type, confidence, tool_calls_json, latency_ms, token_count, created_at
- Indexes: sessions(user_id), messages(session_id, created_at DESC), episodic_log(session_id, created_at DESC)

---

## Phase 6 — Observability

### 6.1 — middleware or handler metrics
- Prometheus counters/histograms: routing_decision_total (by graph_type, cached/not), routing_latency_ms, context_assembly_latency_ms, grpc_call_latency_ms, total_request_latency_ms, token_count_per_request, concurrency_queue_depth
- Track p50/p95 per stage separately so you know exactly where latency lives

---

## New files summary

golang/services/api/
  internal/
    store/
      postgres.go
      redis.go
    router/
      semantic.go
      workflow.go
    context/
      assembler.go
  migrations/
    001_initial_schema.sql

## Modified files summary

golang/services/api/config/config.go        — extend Config struct
golang/services/api/handler/chat.go         — inject store/router, add pre-gRPC pipeline
golang/services/api/middleware/ratelimit.go — per-user + global concurrency semaphore
golang/services/api/middleware/auth.go      — real JWT/API key validation
golang/services/api/cmd/main.go             — wire store/router into ChatHandler
python/servicer/agent_servicer.py           — remove routing/DB, read context from options
python/config.py                            — remove routing/concurrency config

## Dependencies to add

Go:
  github.com/jackc/pgx/v5          — Postgres driver
  github.com/redis/rueidis          — Redis client
  github.com/golang-jwt/jwt/v5     — JWT auth

Python: none new (removing complexity, not adding)

---

## Verification

1. Unit test router: lexical match, semantic match above/below threshold, cache hit, fallback
2. Unit test assembler: maxTurns cap, token budget trim, empty history
3. Unit test store: LoadConversation, SaveTurn, async episodic write isolation
4. Integration test: full Stream() pipeline with mock Python gRPC, verify options payload shape
5. Load test: 500 concurrent requests, measure p95 routing latency, p95 context assembly, p95 total
6. Verify Python instance count needed drops with stateless executor model
7. Verify no DB query executes on pre-cached routing + context cache hit path

---

## Decisions

- Go owns: routing, memory, context budgeting, rate limiting, auth, concurrency control
- Python owns: LangGraph execution, LLM streaming, RAG retrieval inside graph nodes
- RAG retrieval stays in Python because it runs inside graph nodes during generation, not before
- Semantic routing uses Ollama HTTP embedding endpoint directly from Go — no extra service
- Episodic writes are always async (goroutine) — never block request path
- Redis is optional but strongly recommended for routing + context cache to hit latency targets
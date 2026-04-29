# Agent Backend

A production-ready AI agent backend built with a **Go API gateway** and a **Python LangGraph gRPC service**. Supports streaming (SSE) and unary HTTP endpoints, multi-turn conversations, tool calls with HITL, and flexible graph routing (Chat / RAG).

---

## Architecture

```
                          ┌─────────────────────────────────────────────────────┐
                          │                  CLIENT                             │
                          │  (browser / mobile / curl)                          │
                          └────────────────────┬────────────────────────────────┘
                                               │  HTTP POST /api/v1/chat/stream
                                               │  { message, session_id,
                                               │    model?, options? }
                                               ▼
                          ┌─────────────────────────────────────────────────────┐
                          │             GO API GATEWAY  :8080                   │
                          │                                                     │
                          │  ┌──────────┐  ┌────────────┐  ┌────────────────┐  │
                          │  │  Router  │  │    Auth    │  │  Rate Limiter  │  │
                          │  │  (Gin)   │─▶│ Middleware │─▶│  (token bucket)│  │
                          │  └──────────┘  └────────────┘  └───────┬────────┘  │
                          │                                         │           │
                          │                              ┌──────────▼────────┐  │
                          │                              │  Chat Handler     │  │
                          │                              │  - parse JSON     │  │
                          │                              │  - options→Struct │  │
                          │                              │  - SSE writer     │  │
                          │                              └──────────┬────────┘  │
                          │                                         │           │
                          │                              ┌──────────▼────────┐  │
                          │                              │  gRPC Pool        │  │
                          │                              │  (round-robin,    │  │
                          │                              │   5 connections)  │  │
                          └──────────────────────────────┬───────────────────┘  
                                                         │                       
                                                         │  gRPC (proto3)
                                                         │  AgentRequest {
                                                         │    session_id, message,
                                                         │    model?, options{}
                                                         │  }
                                                         ▼
                          ┌─────────────────────────────────────────────────────┐
                          │          PYTHON LANGGRAPH SERVICE  :50051           │
                          │                                                     │
                          │  ┌─────────────────────────────────────────────┐   │
                          │  │  AgentServicer (gRPC)                       │   │
                          │  │  - select_graph(message, options)           │   │
                          │  │    ├─ options.graph_type == "rag" → RAG     │   │
                          │  │    ├─ keywords in message → RAG             │   │
                          │  │    └─ default → Chat                        │   │
                          │  └──────────────┬──────────────────────────────┘   │
                          │                 │                                   │
                          │        ┌────────┴────────┐                         │
                          │        ▼                 ▼                         │
                          │  ┌──────────┐     ┌──────────┐                    │
                          │  │  Chat    │     │   RAG    │                    │
                          │  │  Graph   │     │  Graph   │                    │
                          │  │(LangGraph│     │(LangGraph│                    │
                          │  │  + LLM)  │     │+ Retriever│                   │
                          │  └────┬─────┘     └────┬─────┘                   │
                          │       └────────┬────────┘                         │
                          │                │  stream AgentEvent {             │
                          │                │    event_type, metadata,         │
                          │                │    text | tool_call |            │
                          │                │    tool_result | thinking        │
                          │                │  }                               │
                          └────────────────┼─────────────────────────────────┘
                                           │  gRPC server-stream
                                           ▼
                          ┌─────────────────────────────────────────────────────┐
                          │             GO API GATEWAY  (handler)               │
                          │  - recv each AgentEvent                             │
                          │  - marshal to JSON                                  │
                          │  - write as SSE: data: {...}\n\n                    │
                          └────────────────────┬────────────────────────────────┘
                                               │  SSE events
                                               ▼
                          ┌─────────────────────────────────────────────────────┐
                          │                  CLIENT                             │
                          │  data: {"event_type":"thinking","metadata":{...}}  │
                          │  data: {"event_type":"tool_call","tool_call":{...}} │
                          │  data: {"event_type":"text","text":"Hello!"}        │
                          │  event: done  data: [STREAM_END]                   │
                          └─────────────────────────────────────────────────────┘
```

Database interaction view:

```
Client
  -> Go API Gateway
  -> Python LangGraph gRPC Service
      -> SQLite (Conversational memory per thread)
      -> PostgreSQL + pgvector (Workflow / Toolbox / Entity / Summary memory)
      -> Knowledge Base (Documents / facts / search results)
      -> Tool Log Store (PostgreSQL or SQLite)
  <- AgentEvent stream (thinking, tool_call, tool_result, text, metadata)
  <- SSE/JSON response
```

---

## Database Interactions

Current state:
- Database is not wired into the request path yet.
- Go API currently forwards requests to Python over gRPC.
- Python graphs handle routing and generation; persistence/retrieval backend is pluggable.

Planned data flow for RAG + session persistence:
1. Client sends chat request to Go API.
2. Go API forwards `AgentRequest` to Python gRPC service.
3. Python RAG graph queries pgvector-backed memory stores for relevant context.
4. Python graph reads/writes conversational and audit memory stores.
5. Python returns streamed `AgentEvent` messages back through Go to client.

### Memory Model (RAG + Agent Memory)

| Memory Type | Storage | Purpose |
|-------------|---------|---------|
| Conversational | SQLite | Chat history per thread |
| Knowledge Base | Documents, facts, search results | Source corpus for retrieval |
| Workflow | PostgreSQL + pgvector | Learned tool execution patterns |
| Toolbox | PostgreSQL + pgvector | Tool definitions for semantic discovery |
| Entity | PostgreSQL + pgvector | People, organizations, and systems mentioned |
| Summary | PostgreSQL + pgvector | Compressed conversation summaries |
| Tool Log | PostgreSQL or SQLite | Raw tool inputs/outputs for audit |

Implementation notes:
- `pgvector` tables live in PostgreSQL and are used for semantic retrieval.
- SQLite is used for lightweight local/thread-level durability.
- Tool logs can start in SQLite for local/dev and move to PostgreSQL in production.

---

## Project Structure

```
.
├── proto/
│   └── langgraph/v1/agent.proto       # Shared gRPC contract
├── golang/
│   └── services/api/
│       ├── cmd/                        # Entrypoint
│       ├── config/                     # Env-based config
│       ├── handler/                    # HTTP handlers (chat.go)
│       ├── middleware/                 # Auth, rate limiting
│       ├── router/                     # Gin route setup
│       └── internal/
│           ├── grpcclient/             # Round-robin connection pool
│           ├── sse/                    # SSE writer helper
│           └── langgraphv1/            # Generated protobuf stubs
└── python/
    ├── server.py                       # gRPC server entrypoint
    ├── config.py                       # Python config
    ├── servicer/
    │   └── agent_servicer.py           # gRPC servicer + graph routing
    ├── agents/
    │   ├── chat/graph.py               # Chat LangGraph graph
    │   └── rag/graph.py                # RAG LangGraph graph
    └── gen/                            # Generated protobuf stubs
```

---

## Prerequisites

| Tool | Version |
|------|---------|
| Go | >= 1.25 |
| Python | >= 3.11 |
| Poetry | >= 1.8 |
| protoc | >= 27 |
| protoc-gen-go | latest |
| protoc-gen-go-grpc | latest |

Install protoc plugins:
```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
```

---

## Setup

### 1. Install Python dependencies
```bash
make install-python
# or
cd python && poetry install
```

### 2. Install Go dependencies
```bash
cd golang/services/api && go mod tidy
```

### 3. Regenerate protobuf stubs (after editing .proto)
```bash
make proto
```

---

## Environment Variables

### Go API Gateway
| Variable | Default | Description |
|----------|---------|-------------|
| `HTTP_ADDR` | `:8080` | HTTP listen address |
| `PYTHON_GRPC_ADDR` | `localhost:50051` | Python gRPC service address |
| `GRPC_POOL_SIZE` | `5` | Number of gRPC connections in pool |
| `GRPC_TIMEOUT_MS` | `5000` | gRPC call timeout (ms) |
| `GIN_MODE` | `debug` | Gin mode (`debug` / `release`) |

### Python gRPC Service
| Variable | Default | Description |
|----------|---------|-------------|
| `GRPC_PORT` | `50051` | gRPC listen port |
| `OPENAI_API_KEY` | — | OpenAI API key |

### Database / Retrieval (planned)
| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_URL` | — | PostgreSQL connection string |
| `PGVECTOR_TABLE` | `embeddings` | Table used for vector search |
| `SQLITE_PATH` | `./data/agent_memory.db` | SQLite path for conversational memory |
| `TOOL_LOG_BACKEND` | `sqlite` | Tool log backend: `sqlite` or `postgres` |
| `TOOL_LOG_TABLE` | `tool_logs` | Table name for tool audit logs |
| `RAG_TOP_K` | `5` | Number of chunks to retrieve per query |
| `EMBEDDING_DIM` | `1536` | Vector dimension for embeddings |

Copy and fill in your values:
```bash
cp .env.example .env
```

---

## Running

### Development (two terminals)

**Terminal 1 — Python gRPC service:**
```bash
make run-python
# or
cd python && poetry run python server.py
```

**Terminal 2 — Go API gateway:**
```bash
make run-go
# or
cd golang/services/api && go run ./cmd
```

---

## API Reference

### POST `/api/v1/chat/stream` — SSE Streaming

Streams LangGraph events (tool calls, thinking, text) as Server-Sent Events.

**Request:**
```json
{
  "message": "Find a document about protobuf",
  "session_id": "abc123",
  "model": "default-chat",
  "options": {
    "graph_type": "rag",
    "file_id": "doc_456"
  }
}
```

**Response (SSE stream):**
```
data: {"event_type":"thinking","metadata":{"timestamp_ms":1714520400000,"node_name":"llm_node","thinking":"User wants RAG..."}}

data: {"event_type":"tool_call","metadata":{...},"tool_call":{"id":"tc_1","name":"retrieve","args_json":"{}"}}

data: {"event_type":"tool_result","metadata":{...},"tool_result":{"tool_call_id":"tc_1","content":"..."}}

data: {"event_type":"text","metadata":{...},"text":"Here is what I found..."}

event: done
data: [STREAM_END]
```

---

### POST `/api/v1/chat/invoke` — Unary

Waits for the full response and returns it as JSON.

**Request:** same shape as above.

**Response:**
```json
{
  "result": "Here is what I found...",
  "metadata": {
    "session_id": "abc123",
    "model": "default-chat",
    "tool_calls_count": 1,
    "execution_time_ms": 1234
  }
}
```

---

### GET `/healthz`

```json
{ "status": "ok" }
```

---

## Testing

```bash
# Go tests
make test-go

# Python tests
make test-python
```

---

## Lint

```bash
make lint
```

---

## License

MIT

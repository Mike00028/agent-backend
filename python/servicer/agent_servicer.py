"""
gRPC AgentService servicer.

Routes each request to the correct LangGraph graph based on query intent.
Request shape is: session_id, message, model (optional), options (flexible object)

Streams all graph events: tool_calls, tool_results, text, messages, thinking, errors.
Includes metadata: timestamp_ms, node_name, thinking.
"""

import asyncio
import json
import os
import re
import sys
import time
import uuid
from typing import cast

import grpc
import logging

try:
    from opentelemetry import trace as otel_trace
    from opentelemetry import propagate as otel_propagate
    import opentelemetry.context as otel_ctx_api
    _tracer = otel_trace.get_tracer("python-agent")
except Exception:
    otel_trace = None  # type: ignore[assignment]
    otel_propagate = None  # type: ignore[assignment]
    otel_ctx_api = None  # type: ignore[assignment]
    _tracer = None  # type: ignore[assignment]





# Generated grpc stubs import from "langgraph.v1", so expose python/gen on sys.path.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "gen"))

from langgraph.v1 import agent_pb2, agent_pb2_grpc  # type: ignore[import-not-found]
from agents.router.graph import RouterState, build_graph as build_router_graph
from agents.rag.graph import RagState, build_graph as build_rag_graph
from agents.text.graph import build_graph as build_text_graph
from agents.rag.retriever import InMemoryEmbeddingRetriever
from config import load
from providers import LLMProvider
from tracing import TracedGraph

_log = logging.getLogger("servicer")

# ─── Langfuse tracing ────────────────────────────────────────────────────────
# Langfuse v4 uses OTel auto-instrumentation — initialising the client is enough
# to trace every LangChain/LangGraph LLM call, tool call, and token count.
try:
    from langfuse import Langfuse as _Langfuse
    _lf_public = os.getenv("LANGFUSE_PUBLIC_KEY", "")
    _lf_secret = os.getenv("LANGFUSE_SECRET_KEY", "")
    _lf_host   = os.getenv("LANGFUSE_HOST", "").rstrip("/") or None
    if _lf_public and _lf_secret:
        _lf_client = _Langfuse(
            public_key=_lf_public,
            secret_key=_lf_secret,
            host=_lf_host,
        )
        _log.info("Langfuse tracing enabled host=%s", _lf_host or "cloud")
    else:
        _lf_client = None  # type: ignore[assignment]
        _log.info("Langfuse tracing disabled (LANGFUSE_PUBLIC_KEY/SECRET_KEY not set)")
except ImportError:
    _lf_client = None  # type: ignore[assignment]
    _log.warning("langfuse not installed — poetry add langfuse")

_cfg = load()
_semaphore = asyncio.Semaphore(_cfg.grpc_max_concurrent)

_llm_provider_registry = LLMProvider(_cfg)
_chat_provider = _llm_provider_registry.create_chat_provider()
_embedding_provider = _llm_provider_registry.create_embedding_provider()
_retriever = InMemoryEmbeddingRetriever(
    embedding_provider=_embedding_provider,
    embedding_model=_cfg.embedding_model,
    documents=_cfg.rag_seed_documents,
    top_k=_cfg.rag_top_k,
)

# ─── Agent graph registry ────────────────────────────────────────────────────
# supervisor/router graph — used by RunAgent / StreamAgent
_graph = TracedGraph(
    build_router_graph(
        chat_provider=_chat_provider,
        retriever=_retriever,
        default_model=_cfg.llm_model,
    ),
    "router_agent",
)

# Specialist graphs — used by ExecuteTask (Go DAG executor calls these directly)
# chat_agent, math_agent, summarize_agent are Go-local handlers — not registered here.
# Maps tool_name → TracedGraph  (Go planner uses exactly these names)
_AGENT_REGISTRY: dict[str, TracedGraph] = {
    "rag_agent": TracedGraph(
        build_rag_graph(
            chat_provider=_chat_provider,
            retriever=_retriever,
            default_model=_cfg.llm_model,
        ),
        "rag_agent",
    ),
    "text_agent": TracedGraph(
        build_text_graph(
            chat_provider=_chat_provider,
            default_model=_cfg.llm_model,
        ),
        "text_agent",
    ),
}


class AgentServicer(agent_pb2_grpc.AgentServiceServicer):
    # ── Unary ────────────────────────────────────────────────────────────────

    async def RunAgent(
        self,
        request: agent_pb2.AgentRequest,
        context: grpc.aio.ServicerContext,
    ) -> agent_pb2.AgentResponse:
        """Unary: wait for graph to complete, return final result with metadata."""
        if not _is_valid_uuid(request.session_id):
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "session_id must be a valid UUID")

        options = dict(request.options) if request.options else {}
        model = _resolve_model(request.model)
        history = _extract_history(options)
        messages = history + [{"role": "user", "content": request.message}]

        async with _semaphore:
            try:
                start_time = time.time()
                tool_calls_count = 0
                request_id = str(uuid.uuid4())

                state = cast(RouterState, {
                    "request_id": request_id,
                    "session_id": request.session_id,
                    "messages": messages,
                    "model": model,
                    "planner_model": _cfg.planner_model,
                    "evaluator_model": _cfg.evaluator_model,
                    "options": options,
                    "gate": "",
                    "plan_mode": "",
                    "plan_tasks": [],
                    "plan_max_tasks": _cfg.planner_max_tasks,
                    "plan_max_text_len": _cfg.planner_max_text_len,
                    "planner_timeout_seconds": _cfg.planner_timeout_seconds,
                    "executor_timeout_seconds": _cfg.executor_timeout_seconds,
                    "system_prompt": _cfg.system_prompt,
                    "retrieved_context": "",
                    "result": "",
                    "message": "",
                    "thinking": "",
                    "tool_calls": [],
                    "tool_results": [],
                    "eval_ok": False,
                    "eval_feedback": "",
                    "iteration": 0,
                    "max_iterations": _cfg.agent_max_iterations,
                    "evaluator_enabled": _cfg.evaluator_enabled,
                })
                output = await _graph.ainvoke(state)
                
                # Count tool calls from output if available
                if "tool_calls" in output:
                    tool_calls_count = len(output.get("tool_calls", []))
                
                execution_time_ms = int((time.time() - start_time) * 1000)

                if output.get("error"):
                    return agent_pb2.AgentResponse(
                        metadata=agent_pb2.ResponseMetadata(
                            session_id=request.session_id,
                            model=model,
                            tool_calls_count=tool_calls_count,
                            execution_time_ms=execution_time_ms,
                        ),
                        error=str(output.get("error")),
                    )

                _log.info(
                    "{\"event\":\"request.done\",\"request_id\":\"%s\",\"session_id\":\"%s\",\"model\":\"%s\",\"latency_ms\":%d}",
                    request_id,
                    request.session_id,
                    model,
                    execution_time_ms,
                )
                
                return agent_pb2.AgentResponse(
                    metadata=agent_pb2.ResponseMetadata(
                        session_id=request.session_id,
                        model=model,
                        tool_calls_count=tool_calls_count,
                        execution_time_ms=execution_time_ms,
                    ),
                    text=output.get("result", "")
                )
            except Exception as exc:
                await context.abort(grpc.StatusCode.INTERNAL, str(exc))

    # ── Server-streaming ─────────────────────────────────────────────────────

    async def StreamAgent(
        self,
        request: agent_pb2.AgentRequest,
        context: grpc.aio.ServicerContext,
    ):
        """Stream: yield events as graph executes with metadata (timestamp, node_name, thinking)."""
        if not _is_valid_uuid(request.session_id):
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "session_id must be a valid UUID")

        options = dict(request.options) if request.options else {}
        model = _resolve_model(request.model)
        history = _extract_history(options)
        messages = history + [{"role": "user", "content": request.message}]

        async with _semaphore:
            try:
                request_id = str(uuid.uuid4())
                session_id = request.session_id
                state = cast(RouterState, {
                    "request_id": request_id,
                    "session_id": request.session_id,
                    "messages": messages,
                    "model": model,
                    "planner_model": _cfg.planner_model,
                    "evaluator_model": _cfg.evaluator_model,
                    "options": options,
                    "gate": "",
                    "plan_mode": "",
                    "plan_tasks": [],
                    "plan_max_tasks": _cfg.planner_max_tasks,
                    "plan_max_text_len": _cfg.planner_max_text_len,
                    "planner_timeout_seconds": _cfg.planner_timeout_seconds,
                    "executor_timeout_seconds": _cfg.executor_timeout_seconds,
                    "system_prompt": _cfg.system_prompt,
                    "retrieved_context": "",
                    "result": "",
                    "message": "",
                    "thinking": "",
                    "tool_calls": [],
                    "tool_results": [],
                    "eval_ok": False,
                    "eval_feedback": "",
                    "iteration": 0,
                    "max_iterations": _cfg.agent_max_iterations,
                    "evaluator_enabled": _cfg.evaluator_enabled,
                })

                # Stream graph events
                _log.info(
                    "{\"event\":\"stream.start\",\"request_id\":\"%s\",\"session_id\":\"%s\"}",
                    request_id,
                    session_id,
                )
                async for event in _graph.astream(state, stream_mode="updates"):
                    _log.info(
                        "{\"event\":\"stream.astream_event\",\"request_id\":\"%s\",\"session_id\":\"%s\",\"nodes\":\"%s\"}",
                        request_id,
                        session_id,
                        ",".join(event.keys()),
                    )
                    # event is {node_name: node_output}
                    for node_name, node_output in event.items():
                        _log.info(
                            "{\"event\":\"stream.node\",\"request_id\":\"%s\",\"session_id\":\"%s\",\"node\":\"%s\",\"keys\":\"%s\"}",
                            request_id,
                            session_id,
                            node_name,
                            ",".join(sorted(node_output.keys())),
                        )
                        # Get current timestamp in milliseconds
                        timestamp_ms = int(time.time() * 1000)
                        
                        # Extract thinking/analysis from node_output if available
                        thinking = node_output.get("thinking", "")
                        
                        # Build EventMetadata for this event
                        metadata = agent_pb2.EventMetadata(
                            timestamp_ms=timestamp_ms,
                            node_name=node_name,
                            thinking=thinking,
                        )
                        
                        # Tool calls emitted by LLM
                        if "tool_calls" in node_output:
                            for tc in node_output["tool_calls"]:
                                try:
                                    args_json = json.dumps(tc.get("args", {}))
                                except (TypeError, ValueError) as e:
                                    _log.warning(
                                        "{\"event\":\"stream.error\",\"type\":\"tool_call_args\",\"request_id\":\"%s\",\"error\":\"%s\"}",
                                        request_id,
                                        str(e),
                                    )
                                    args_json = "{}"
                                _log.info(
                                    "{\"event\":\"stream.emit\",\"type\":\"tool_call\",\"request_id\":\"%s\",\"session_id\":\"%s\",\"node\":\"%s\"}",
                                    request_id,
                                    session_id,
                                    node_name,
                                )
                                yield agent_pb2.AgentEvent(
                                    event_type="tool_call",
                                    metadata=metadata,
                                    tool_call=agent_pb2.ToolCall(
                                        id=tc.get("id", ""),
                                        name=tc.get("name", ""),
                                        args_json=args_json,
                                    ),
                                )
                        
                        # Tool results from execution
                        if "tool_results" in node_output:
                            for tr in node_output["tool_results"]:
                                _log.info(
                                    "{\"event\":\"stream.emit\",\"type\":\"tool_result\",\"request_id\":\"%s\",\"session_id\":\"%s\",\"node\":\"%s\"}",
                                    request_id,
                                    session_id,
                                    node_name,
                                )
                                yield agent_pb2.AgentEvent(
                                    event_type="tool_result",
                                    metadata=metadata,
                                    tool_result=agent_pb2.ToolResult(
                                        tool_call_id=tr.get("tool_call_id", ""),
                                        content=tr.get("content", ""),
                                    ),
                                )
                        
                        # Final result text
                        if "result" in node_output and node_output["result"]:
                            _log.info(
                                "{\"event\":\"stream.emit\",\"type\":\"text\",\"request_id\":\"%s\",\"session_id\":\"%s\",\"node\":\"%s\"}",
                                request_id,
                                session_id,
                                node_name,
                            )
                            yield agent_pb2.AgentEvent(
                                event_type="text",
                                metadata=metadata,
                                text=node_output["result"],
                            )
                        
                        # Any intermediate messages (skip if empty)
                        if "message" in node_output and str(node_output["message"]).strip():
                            _log.info(
                                "{\"event\":\"stream.emit\",\"type\":\"message\",\"request_id\":\"%s\",\"session_id\":\"%s\",\"node\":\"%s\"}",
                                request_id,
                                session_id,
                                node_name,
                            )
                            yield agent_pb2.AgentEvent(
                                event_type="message",
                                metadata=metadata,
                                message=node_output["message"],
                            )
                        
                        # Thinking/analysis events (skip if empty or same as message)
                        if thinking and thinking.strip() and thinking != node_output.get("message", ""):
                            _log.info(
                                "{\"event\":\"stream.emit\",\"type\":\"thinking\",\"request_id\":\"%s\",\"session_id\":\"%s\",\"node\":\"%s\"}",
                                request_id,
                                session_id,
                                node_name,
                            )
                            yield agent_pb2.AgentEvent(
                                event_type="thinking",
                                metadata=metadata,
                                text=thinking,
                            )
                        
                        # Errors
                        if "error" in node_output:
                            error_text = str(node_output.get("error") or "Unknown error")
                            _log.info(
                                "{\"event\":\"stream.emit\",\"type\":\"error\",\"request_id\":\"%s\",\"session_id\":\"%s\",\"node\":\"%s\"}",
                                request_id,
                                session_id,
                                node_name,
                            )
                            yield agent_pb2.AgentEvent(
                                event_type="error",
                                metadata=metadata,
                                error=error_text,
                            )
            except Exception as exc:
                _log.exception("stream agent error")
                await context.abort(grpc.StatusCode.INTERNAL, str(exc) or "internal error")

    # ── ExecuteTask ──────────────────────────────────────────────────────────

    async def ExecuteTask(
        self,
        request: agent_pb2.TaskRequest,
        context: grpc.aio.ServicerContext,
    ):
        """Stream tool-execution events back to Go's DAG executor."""
        session_id = request.session_id
        task_id = request.task_id
        tool_name = (request.tool_name or "").lower().strip()
        ctx_text = (request.context or "").strip()
        model = _resolve_model("")

        args: dict = {}
        if request.args_json:
            try:
                args = json.loads(request.args_json)
            except json.JSONDecodeError:
                pass

        _log.info(
            '{"event":"execute_task.start","session_id":"%s","task_id":"%s","tool":"%s"}',
            session_id, task_id, tool_name,
        )

        yield agent_pb2.TaskEvent(type="started", payload=task_id)

        async with _semaphore:
            # Extract the W3C traceparent propagated by the Go executor so that
            # Python spans become children of the Go dag.task.* span in Langfuse.
            parent_ctx = None
            if otel_propagate is not None:
                grpc_md = dict(context.invocation_metadata())
                parent_ctx = otel_propagate.extract(grpc_md)
            ctx_token = otel_ctx_api.attach(parent_ctx) if (otel_ctx_api is not None and parent_ctx) else None

            try:
                graph = _AGENT_REGISTRY.get(tool_name)
                if graph is None:
                    await context.abort(
                        grpc.StatusCode.NOT_FOUND,
                        f"tool '{tool_name}' is not handled by Python (it may be a Go-local agent)",
                    )
                    return

                # Build the state for whichever agent was selected.
                # args.question holds the per-task sub-query set by the planner.
                # ctx_text contains only "[tN result]: ..." lines from prior tasks
                # (the executor no longer injects [user message] here).
                question = str(args.get("question") or args.get("query") or args.get("message") or ctx_text or "")

                _log.info(
                    '{"event":"execute_task.input","task_id":"%s","tool":"%s","question":%s}',
                    task_id, tool_name, json.dumps(question[:300]),
                )

                if tool_name == "rag_agent":
                    state = {
                        "session_id": session_id,
                        "messages": [{"role": "user", "content": question}],
                        "model": model,
                        "options": {},
                        "retrieved_context": "",
                        "result": "",
                    }
                elif tool_name == "text_agent":
                    # Prepend prior dependency results when present (sequential tasks).
                    user_content = f"{ctx_text}\n{question}".strip() if ctx_text else question
                    state = {
                        "messages": [{"role": "user", "content": user_content}],
                    }
                else:
                    await context.abort(
                        grpc.StatusCode.NOT_FOUND,
                        f"tool '{tool_name}' is not handled by Python",
                    )
                    return

                output = await asyncio.wait_for(
                    graph.ainvoke(state),
                    timeout=_cfg.executor_timeout_seconds,
                )

                # Log every tool call made during the ReAct loop.
                # AIMessage.tool_calls = [{"name": "...", "args": {...}, "id": "..."}]
                if "messages" in output:
                    for msg in output["messages"]:
                        calls = getattr(msg, "tool_calls", None)
                        if calls:
                            for tc in calls:
                                _log.info(
                                    '{"event":"tool_call","task_id":"%s","tool":"%s","args":%s}',
                                    task_id,
                                    tc.get("name", "?"),
                                    json.dumps(tc.get("args", {})),
                                )

                # create_react_agent returns {"messages": [...]} — last AI message is the answer.
                # Other agents return {"result": "..."}.
                if "messages" in output and "result" not in output:
                    msgs = output["messages"]
                    last = msgs[-1] if msgs else None
                    if last is None:
                        result = ""
                    elif hasattr(last, "content"):
                        content = last.content
                        # Gemini may return a list of content parts
                        if isinstance(content, list):
                            result = " ".join(
                                p.get("text", str(p)) if isinstance(p, dict) else str(p)
                                for p in content
                            )
                        else:
                            result = str(content)
                    else:
                        result = str(last)
                else:
                    result = str(output.get("result", ""))

                _log.info(
                    '{"event":"execute_task.done","session_id":"%s","task_id":"%s","agent":"%s"}',
                    session_id, task_id, tool_name,
                )
                yield agent_pb2.TaskEvent(type="text", payload=result)
                yield agent_pb2.TaskEvent(type="done", payload=result)

            except asyncio.TimeoutError:
                msg = f"task {task_id} timed out after {_cfg.executor_timeout_seconds}s"
                _log.warning(
                    '{"event":"execute_task.timeout","session_id":"%s","task_id":"%s"}',
                    session_id, task_id,
                )
                await context.abort(grpc.StatusCode.DEADLINE_EXCEEDED, msg)

            except Exception as exc:
                _log.exception("execute_task error task_id=%s", task_id)
                yield agent_pb2.TaskEvent(type="error", error=str(exc))
            finally:
                # Detach the parent OTel context propagated from Go via gRPC metadata.
                if ctx_token is not None and otel_ctx_api is not None:
                    otel_ctx_api.detach(ctx_token)


def _is_valid_uuid(session_id: str) -> bool:
    if not session_id:
        return False
    try:
        uuid.UUID(session_id)
        return True
    except ValueError:
        return False


def _resolve_model(request_model: str) -> str:
    candidate = (request_model or "").strip()
    if not candidate:
        return _cfg.llm_model
    if candidate.lower() in {"default-chat", "default", "chat"}:
        return _cfg.llm_model
    return candidate


def _extract_history(options: dict) -> list[dict[str, str]]:
    history = options.get("history") if isinstance(options, dict) else None
    if not isinstance(history, list):
        return []
    normalized: list[dict[str, str]] = []
    for item in history:
        if not isinstance(item, dict):
            continue
        role = str(item.get("role", "user"))
        content = str(item.get("content", ""))
        if content:
            normalized.append({"role": role, "content": content})
    return normalized

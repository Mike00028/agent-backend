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
import sys
import time
import uuid
from typing import cast

import grpc
import logging

# Generated grpc stubs import from "langgraph.v1", so expose python/gen on sys.path.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "gen"))

from langgraph.v1 import agent_pb2, agent_pb2_grpc  # type: ignore[import-not-found]
from agents.router.graph import RouterState, build_graph as build_router_graph
from agents.rag.retriever import InMemoryEmbeddingRetriever
from config import load
from providers import LLMProvider

_log = logging.getLogger("servicer")

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

# Single supervisor graph — routes to chat / rag / math internally via conditional edges.
_graph = build_router_graph(
    chat_provider=_chat_provider,
    retriever=_retriever,
    default_model=_cfg.llm_model,
)


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
                                    args_json = \"{}\"
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

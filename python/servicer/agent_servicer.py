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

# Generated grpc stubs import from "langgraph.v1", so expose python/gen on sys.path.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "gen"))

from langgraph.v1 import agent_pb2, agent_pb2_grpc  # type: ignore[import-not-found]
from agents.router.graph import RouterState, build_graph as build_router_graph
from agents.rag.retriever import InMemoryEmbeddingRetriever
from config import load
from providers import LLMProvider

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

        async with _semaphore:
            try:
                start_time = time.time()
                tool_calls_count = 0

                state = cast(RouterState, {
                    "session_id": request.session_id,
                    "messages": [{"role": "user", "content": request.message}],
                    "model": model,
                    "options": options,
                    "route": "",
                    "retrieved_context": "",
                    "result": "",
                    "tool_calls": [],
                    "tool_results": [],
                })
                output = await _graph.ainvoke(state)
                
                # Count tool calls from output if available
                if "tool_calls" in output:
                    tool_calls_count = len(output.get("tool_calls", []))
                
                execution_time_ms = int((time.time() - start_time) * 1000)
                
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

        async with _semaphore:
            try:
                state = cast(RouterState, {
                    "session_id": request.session_id,
                    "messages": [{"role": "user", "content": request.message}],
                    "model": model,
                    "options": options,
                    "route": "",
                    "retrieved_context": "",
                    "result": "",
                    "tool_calls": [],
                    "tool_results": [],
                })

                # Stream graph events
                async for event in _graph.astream(state):
                    # event is {node_name: node_output}
                    for node_name, node_output in event.items():
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
                                yield agent_pb2.AgentEvent(
                                    event_type="tool_call",
                                    metadata=metadata,
                                    tool_call=agent_pb2.ToolCall(
                                        id=tc.get("id", ""),
                                        name=tc.get("name", ""),
                                        args_json=json.dumps(tc.get("args", {})),
                                    ),
                                )
                        
                        # Tool results from execution
                        if "tool_results" in node_output:
                            for tr in node_output["tool_results"]:
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
                            yield agent_pb2.AgentEvent(
                                event_type="text",
                                metadata=metadata,
                                text=node_output["result"],
                            )
                        
                        # Any intermediate messages
                        if "message" in node_output:
                            yield agent_pb2.AgentEvent(
                                event_type="message",
                                metadata=metadata,
                                message=node_output["message"],
                            )
                        
                        # Thinking/analysis events
                        if thinking:
                            yield agent_pb2.AgentEvent(
                                event_type="thinking",
                                metadata=metadata,
                                text=thinking,
                            )
                        
                        # Errors
                        if "error" in node_output:
                            yield agent_pb2.AgentEvent(
                                event_type="error",
                                metadata=metadata,
                                error=node_output["error"],
                            )
            except Exception as exc:
                await context.abort(grpc.StatusCode.INTERNAL, str(exc))


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

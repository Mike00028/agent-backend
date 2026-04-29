"""
gRPC AgentService servicer.

Routes each request to the correct LangGraph graph based on query intent.
Request shape is: session_id, message, model (optional), options (flexible object)

Streams all graph events: tool_calls, tool_results, text, messages, thinking, errors.
Includes metadata: timestamp_ms, node_name, thinking.
"""

import asyncio
import json
import sys
import os
import time

# Make generated stubs importable (../gen)
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "gen"))

import grpc
from langgraph.v1 import agent_pb2, agent_pb2_grpc
from agents.chat.graph import build_graph as build_chat_graph
from agents.rag.graph import build_graph as build_rag_graph
from config import load

_cfg = load()
_semaphore = asyncio.Semaphore(_cfg.grpc_max_concurrent)

# Build graphs once at import time (thread/async safe after compilation)
_graphs = {
    "chat": build_chat_graph(),
    "rag": build_rag_graph(),
}


def _select_graph(message: str, options: dict):
    """Select graph using explicit 'graph_type' option or lightweight query heuristics."""
    if not options:
        options = {}
    
    # Check for explicit graph_type option
    if isinstance(options, dict):
        graph_type = options.get("graph_type", "").lower()
        if graph_type == "rag":
            return _graphs["rag"]
        elif graph_type == "chat":
            return _graphs["chat"]
    
    # Heuristic: check message content for RAG-related keywords
    msg = (message or "").lower()
    rag_hints = ("document", "docs", "knowledge base", "search", "retrieve", "citation", "context")
    if any(h in msg for h in rag_hints):
        return _graphs["rag"]
    return _graphs["chat"]


class AgentServicer(agent_pb2_grpc.AgentServiceServicer):
    # ── Unary ────────────────────────────────────────────────────────────────

    async def RunAgent(
        self,
        request: agent_pb2.AgentRequest,
        context: grpc.aio.ServicerContext,
    ) -> agent_pb2.AgentResponse:
        """Unary: wait for graph to complete, return final result with metadata."""
        if not request.session_id:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "session_id required")

        graph = _select_graph(request.message, dict(request.options) if request.options else {})
        model = request.model or "default-chat"

        async with _semaphore:
            try:
                start_time = time.time()
                tool_calls_count = 0
                
                state = {
                    "session_id": request.session_id,
                    "messages": [{"role": "user", "content": request.message}],
                    "result": "",
                }
                output = await graph.ainvoke(state)
                
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
        if not request.session_id:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "session_id required")

        graph = _select_graph(request.message, dict(request.options) if request.options else {})

        async with _semaphore:
            try:
                state = {
                    "session_id": request.session_id,
                    "messages": [{"role": "user", "content": request.message}],
                    "result": "",
                }
                
                # Stream graph events
                async for event in graph.astream(state):
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

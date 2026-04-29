"""Node functions for the RAG agent graph."""

from typing import Any
from langchain_core.messages import AIMessage, ToolMessage
from agents.rag.tools import retrieve_tool

_TOOLS = [retrieve_tool]
_TOOL_MAP = {t.name: t for t in _TOOLS}


def retriever_node(state: dict[str, Any]) -> dict[str, Any]:
    """Calls retrieve_tool with the user's query. Emits tool_calls for streaming."""
    last = state["messages"][-1]["content"]
    ai_msg = AIMessage(
        content="",
        tool_calls=[{"id": "call_r1", "name": "retrieve_tool", "args": {"query": last}}],
    )
    return {
        "messages": state["messages"] + [ai_msg.dict()],
        "result": "",
        "tool_calls": [{"id": "call_r1", "name": "retrieve_tool", "args": {"query": last}}],
    }


def answer_node(state: dict[str, Any]) -> dict[str, Any]:
    """Generates an answer from retrieved context. Stub — replace with real LLM call."""
    last = state["messages"][-1]
    tool_calls = last.get("tool_calls", [])
    results = []
    tool_results = []
    
    for call in tool_calls:
        tool = _TOOL_MAP.get(call["name"])
        if tool:
            output = tool.invoke(call["args"])
            results.append(ToolMessage(content=output, tool_call_id=call["id"]).dict())
            tool_results.append({
                "tool_call_id": call["id"],
                "content": output,
            })

    context = results[-1]["content"] if results else ""
    # TODO: pass context to LLM for grounded answer generation
    final = f"[RAG] {context}"
    return {
        "messages": state["messages"] + results,
        "result": final,
        "tool_results": tool_results,
    }

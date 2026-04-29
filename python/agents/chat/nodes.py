"""Node functions for the chat agent graph."""

from typing import Any
from langchain_core.messages import AIMessage, ToolMessage
from agents.chat.tools import hello_tool

_TOOLS = [hello_tool]
_TOOL_MAP = {t.name: t for t in _TOOLS}


def llm_node(state: dict[str, Any]) -> dict[str, Any]:
    """
    Mock LLM node: always emits a hello_tool call.
    Replace with a real ChatOpenAI / ChatAnthropic call when ready.
    Emits tool_calls that the servicer will forward as stream events.
    """
    last = state["messages"][-1]["content"]
    # Simulate the LLM deciding to call hello_tool with the user's input
    ai_msg = AIMessage(
        content="",
        tool_calls=[{"id": "call_1", "name": "hello_tool", "args": {"name": last}}],
    )
    # Emit tool_calls so the servicer can stream them
    return {
        "messages": state["messages"] + [ai_msg.dict()],
        "result": "",
        "tool_calls": [{"id": "call_1", "name": "hello_tool", "args": {"name": last}}],
    }


def tools_node(state: dict[str, Any]) -> dict[str, Any]:
    """
    Executes tool calls emitted by the LLM node.
    Emits tool_results that the servicer will forward as stream events.
    """
    last = state["messages"][-1]
    tool_calls = last.get("tool_calls", [])
    results = []
    tool_results = []
    
    for call in tool_calls:
        tool = _TOOL_MAP.get(call["name"])
        if tool:
            output = tool.invoke(call["args"])
            results.append(ToolMessage(content=output, tool_call_id=call["id"]).dict())
            # Emit tool_result for streaming
            tool_results.append({
                "tool_call_id": call["id"],
                "content": output,
            })

    final_result = results[-1]["content"] if results else ""
    return {
        "messages": state["messages"] + results,
        "result": final_result,
        "tool_results": tool_results,
    }

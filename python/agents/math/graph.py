"""Math agent StateGraph definition."""

from typing import Any, TypedDict

from langgraph.graph import END, StateGraph

from agents.math.nodes import build_math_llm_node, build_math_tools_node
from providers.interfaces import ChatProvider


class MathState(TypedDict):
    session_id: str
    messages: list[dict[str, Any]]
    model: str
    options: dict[str, Any]
    result: str
    tool_calls: list[dict[str, Any]]
    tool_results: list[dict[str, Any]]


def build_graph(*, chat_provider: ChatProvider, default_model: str):
    math_llm_node = build_math_llm_node(chat_provider, default_model)
    math_tools_node = build_math_tools_node()

    graph = StateGraph(MathState)
    graph.add_node("math_llm", math_llm_node)
    graph.add_node("math_tools", math_tools_node)
    graph.add_edge("math_llm", "math_tools")
    graph.add_edge("math_tools", END)
    graph.set_entry_point("math_llm")
    return graph.compile()

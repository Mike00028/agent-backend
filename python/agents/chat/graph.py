"""Chat agent StateGraph definition."""

from typing import TypedDict, Any
from langgraph.graph import StateGraph, END
from agents.chat.nodes import llm_node, tools_node


class ChatState(TypedDict):
    session_id: str
    messages: list[dict[str, Any]]
    result: str


def build_graph():
    graph = StateGraph(ChatState)
    graph.add_node("llm", llm_node)
    graph.add_node("tools", tools_node)
    graph.add_edge("llm", "tools")
    graph.add_edge("tools", END)
    graph.set_entry_point("llm")
    return graph.compile()

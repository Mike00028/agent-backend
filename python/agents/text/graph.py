"""Text analysis agent StateGraph — pure-Python tools, no LLM call."""
from typing import Any, TypedDict

from langgraph.graph import END, StateGraph

from agents.text.nodes import build_text_node


class TextState(TypedDict):
    session_id: str
    task_id: str
    tool_name: str
    args: dict[str, Any]
    context: str
    result: str


def build_graph():
    text_node = build_text_node()

    graph = StateGraph(TextState)
    graph.add_node("text", text_node)
    graph.add_edge("text", END)
    graph.set_entry_point("text")
    return graph.compile()

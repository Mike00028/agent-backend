"""Chat agent StateGraph definition."""

from typing import TypedDict, Any
from langgraph.graph import StateGraph, END
from agents.chat.nodes import build_llm_node
from providers.interfaces import ChatProvider


class ChatState(TypedDict):
    session_id: str
    messages: list[dict[str, Any]]
    model: str
    options: dict[str, Any]
    result: str


def build_graph(*, chat_provider: ChatProvider, default_model: str):
    llm_node = build_llm_node(chat_provider, default_model)
    graph = StateGraph(ChatState)
    graph.add_node("llm", llm_node)
    graph.add_edge("llm", END)
    graph.set_entry_point("llm")
    return graph.compile()

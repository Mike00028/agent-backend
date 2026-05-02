"""Summarize agent StateGraph — takes prior task outputs and synthesizes them."""
from typing import TypedDict, Any

from langgraph.graph import StateGraph, END

from agents.summarize.nodes import build_summarize_node
from providers.interfaces import ChatProvider


class SummarizeState(TypedDict):
    session_id: str
    question: str       # original user question (optional, for framing)
    context: str        # injected dependency outputs from Go executor
    model: str
    options: dict[str, Any]
    result: str


def build_graph(*, chat_provider: ChatProvider, default_model: str):
    summarize_node = build_summarize_node(chat_provider, default_model)

    graph = StateGraph(SummarizeState)
    graph.add_node("summarize", summarize_node)
    graph.add_edge("summarize", END)
    graph.set_entry_point("summarize")
    return graph.compile()

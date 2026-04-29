"""RAG agent StateGraph definition."""

from typing import TypedDict, Any
from langgraph.graph import StateGraph, END
from agents.rag.nodes import retriever_node, answer_node


class RagState(TypedDict):
    session_id: str
    messages: list[dict[str, Any]]
    result: str


def build_graph():
    graph = StateGraph(RagState)
    graph.add_node("retriever", retriever_node)
    graph.add_node("answer", answer_node)
    graph.add_edge("retriever", "answer")
    graph.add_edge("answer", END)
    graph.set_entry_point("retriever")
    return graph.compile()

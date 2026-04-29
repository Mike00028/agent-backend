"""RAG agent StateGraph definition."""

from typing import TypedDict, Any
from langgraph.graph import StateGraph, END
from agents.rag.nodes import build_answer_node, build_retriever_node
from agents.rag.retriever import InMemoryEmbeddingRetriever
from providers.interfaces import ChatProvider


class RagState(TypedDict):
    session_id: str
    messages: list[dict[str, Any]]
    model: str
    options: dict[str, Any]
    retrieved_context: str
    result: str


def build_graph(*, chat_provider: ChatProvider, retriever: InMemoryEmbeddingRetriever, default_model: str):
    retriever_node = build_retriever_node(retriever)
    answer_node = build_answer_node(chat_provider, default_model)

    graph = StateGraph(RagState)
    graph.add_node("retriever", retriever_node)
    graph.add_node("answer", answer_node)
    graph.add_edge("retriever", "answer")
    graph.add_edge("answer", END)
    graph.set_entry_point("retriever")
    return graph.compile()

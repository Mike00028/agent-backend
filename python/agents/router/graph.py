"""Supervisor router graph: classifies then conditionally branches to agent nodes."""

from typing import Any, TypedDict

from langgraph.graph import END, StateGraph

from agents.chat.nodes import build_llm_node
from agents.math.nodes import build_math_llm_node, build_math_tools_node
from agents.rag.nodes import build_answer_node, build_retriever_node
from agents.rag.retriever import InMemoryEmbeddingRetriever
from agents.router.nodes import build_multi_node, build_route_node, pick_route
from providers.interfaces import ChatProvider


class RouterState(TypedDict):
    session_id: str
    messages: list[dict[str, Any]]
    model: str
    options: dict[str, Any]
    route: str
    retrieved_context: str
    result: str
    tool_calls: list[dict[str, Any]]
    tool_results: list[dict[str, Any]]


def build_graph(
    *,
    chat_provider: ChatProvider,
    retriever: InMemoryEmbeddingRetriever,
    default_model: str,
):
    route_node     = build_route_node(chat_provider, default_model)
    chat_node      = build_llm_node(chat_provider, default_model)
    retriever_node = build_retriever_node(retriever)
    answer_node    = build_answer_node(chat_provider, default_model)
    math_llm_node  = build_math_llm_node(chat_provider, default_model)
    math_tools_node = build_math_tools_node()

    graph = StateGraph(RouterState)

    # ── Nodes ──────────────────────────────────────────────────────────────
    graph.add_node("route",        route_node)
    graph.add_node("chat",         chat_node)
    graph.add_node("rag_retriever", retriever_node)
    graph.add_node("rag_answer",   answer_node)
    graph.add_node("math_llm",     math_llm_node)
    graph.add_node("math_tools",   math_tools_node)

    # ── Conditional routing from route node ────────────────────────────────
    graph.add_conditional_edges(
        "route",
        pick_route,
        {
            "chat": "chat",
            "rag":  "rag_retriever",
            "math": "math_llm",
        },
    )

    # ── Agent-internal edges ───────────────────────────────────────────────
    graph.add_edge("chat",         END)
    graph.add_edge("rag_retriever", "rag_answer")
    graph.add_edge("rag_answer",   END)
    graph.add_edge("math_llm",     "math_tools")
    graph.add_edge("math_tools",   END)

    graph.set_entry_point("route")
    return graph.compile()

"""Supervisor graph: gate -> planner -> executor -> evaluator loop."""

from typing import Any, TypedDict

from langgraph.graph import END, StateGraph

from agents.rag.retriever import InMemoryEmbeddingRetriever
from agents.router.nodes import (
    build_evaluator_node,
    build_executor_node,
    build_gate_node,
    build_planner_node,
    pick_eval,
    pick_gate,
)
from providers.interfaces import ChatProvider


class RouterState(TypedDict):
    request_id: str
    session_id: str
    messages: list[dict[str, Any]]
    model: str
    planner_model: str
    evaluator_model: str
    options: dict[str, Any]
    gate: str
    plan_mode: str
    plan_tasks: list[dict[str, Any]]
    plan_max_tasks: int
    plan_max_text_len: int
    planner_timeout_seconds: float
    executor_timeout_seconds: float
    retrieved_context: str
    result: str
    message: str
    thinking: str
    tool_calls: list[dict[str, Any]]
    tool_results: list[dict[str, Any]]
    eval_ok: bool
    eval_feedback: str
    iteration: int
    max_iterations: int
    evaluator_enabled: bool
    system_prompt: str


def build_graph(
    *,
    chat_provider: ChatProvider,
    retriever: InMemoryEmbeddingRetriever,
    default_model: str,
):
    gate_node = build_gate_node(chat_provider, default_model)
    planner_node = build_planner_node(chat_provider, default_model)
    executor_node = build_executor_node(chat_provider, retriever, default_model)
    evaluator_node = build_evaluator_node(chat_provider, default_model)

    graph = StateGraph(RouterState)

    graph.add_node("gate", gate_node)
    graph.add_node("plan", planner_node)
    graph.add_node("execute", executor_node)
    graph.add_node("evaluate", evaluator_node)

    graph.add_conditional_edges(
        "gate",
        pick_gate,
        {
            "plan": "plan",
            "execute": "execute",
        },
    )

    graph.add_edge("plan", "execute")
    graph.add_edge("execute", "evaluate")

    graph.add_conditional_edges(
        "evaluate",
        pick_eval,
        {
            "retry": "plan",
            "done": END,
        },
    )

    graph.set_entry_point("gate")
    return graph.compile()

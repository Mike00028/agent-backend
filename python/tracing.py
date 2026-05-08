"""Tracing wrapper for LangGraph agents using Langfuse @observe.

Wraps compiled LangGraph graphs so every ainvoke/astream call automatically
creates a Langfuse span (as_type="agent") that captures:
- Agent name, session_id, task_id, tags
- All nested LLM generations (auto-captured by langfuse + langchain)
- All tool calls with names, inputs, outputs (auto-captured)
- Token usage and cost (auto-captured)

Usage:
    graph = TracedGraph(build_text_graph(...), "text_agent")
    output = await graph.ainvoke(state)       # traced automatically
    async for event in graph.astream(state):  # same for streaming
        ...
"""

from __future__ import annotations

import logging
from typing import Any

_log = logging.getLogger("tracing")

# ── Langfuse @observe ────────────────────────────────────────────────────────
try:
    from langfuse import observe as _observe
    _HAS_LANGFUSE = True
except ImportError:
    _HAS_LANGFUSE = False
    _log.warning("langfuse not installed — TracedGraph will pass through without tracing")


class TracedGraph:
    """Wraps a compiled LangGraph to auto-instrument with Langfuse @observe.

    Every ``ainvoke`` / ``astream`` call creates a Langfuse agent span.
    Nested LLM calls and tool calls are auto-captured by langfuse's
    LangChain integration — no manual callback wiring needed.
    """

    def __init__(self, graph: Any, agent_name: str) -> None:
        self._graph = graph
        self._agent_name = agent_name

        # Pre-build the observed wrappers once at init time so @observe
        # sees a consistent function name in Langfuse.
        if _HAS_LANGFUSE:
            self._observed_ainvoke = _observe(
                name=agent_name, as_type="agent"
            )(self._raw_ainvoke)
            self._observed_astream = _observe(
                name=agent_name, as_type="agent"
            )(self._raw_astream)

    # ── raw methods wrapped by @observe ──────────────────────────────────

    async def _raw_ainvoke(self, state: dict, config: dict | None = None, **kwargs: Any) -> dict:
        return await self._graph.ainvoke(state, config=config or {}, **kwargs)

    async def _raw_astream(self, state: dict, config: dict | None = None, **kwargs: Any):
        async for event in self._graph.astream(state, config=config or {}, **kwargs):
            yield event

    # ── public API (mirrors compiled graph) ───────────────────────────────

    async def ainvoke(self, state: dict, config: dict | None = None, **kwargs: Any) -> dict:
        cfg = self._build_config(config, state)
        if _HAS_LANGFUSE:
            return await self._observed_ainvoke(state, config=cfg, **kwargs)
        return await self._graph.ainvoke(state, config=cfg, **kwargs)

    async def astream(self, state: dict, config: dict | None = None, **kwargs: Any):
        cfg = self._build_config(config, state)
        if _HAS_LANGFUSE:
            async for event in self._observed_astream(state, config=cfg, **kwargs):
                yield event
        else:
            async for event in self._graph.astream(state, config=cfg, **kwargs):
                yield event

    # ── internal helpers ─────────────────────────────────────────────────

    def _build_config(self, config: dict | None, state: dict) -> dict:
        """Merge caller config with tracing defaults (run_name, tags, metadata)."""
        config = dict(config or {})
        config.setdefault("run_name", self._agent_name)

        md = dict(config.get("metadata", {}) or {})
        session_id = state.get("session_id", "")
        if session_id:
            md.setdefault("session_id", session_id)
        config["metadata"] = md

        tags = list(config.get("tags", []) or [])
        if self._agent_name not in tags:
            tags.append(self._agent_name)
        config["tags"] = tags

        return config

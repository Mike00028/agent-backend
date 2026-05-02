"""Summarize agent node — receives prior task outputs via context and synthesizes."""
from __future__ import annotations

from typing import Any

from providers.interfaces import ChatProvider

_SYSTEM = (
    "You are a synthesis assistant. You will be given the outputs of multiple tasks "
    "that were run in parallel or sequence. Combine them into a single, clear, "
    "well-structured response for the user. Do not repeat yourself. Be concise."
)


def build_summarize_node(chat_provider: ChatProvider, default_model: str):

    async def summarize_node(state):
        model = state.get("model") or default_model
        options = state.get("options") or {}
        context = str(state.get("context", "")).strip()
        question = str(state.get("question", "")).strip()

        # context contains the injected outputs from all dependency tasks
        # (Go's executor injects them as "[task_id result]: ..." lines)
        user_content = (
            f"Original question: {question}\n\nTask results to summarize:\n{context}"
            if question
            else f"Task results to summarize:\n{context}"
        )

        messages = [
            {"role": "system", "content": _SYSTEM},
            {"role": "user", "content": user_content},
        ]

        response = await chat_provider.generate(messages, model=model, options=options)
        return {"result": response.text}

    return summarize_node

"""Node functions for the chat agent graph."""

from __future__ import annotations

from typing import Any

from providers.interfaces import ChatProvider


def build_llm_node(chat_provider: ChatProvider, default_model: str):
    """Create an async LLM node with provider injection."""

    async def llm_node(state):
        messages = list(state.get("messages", []))
        model = state.get("model") or default_model
        options = state.get("options") or {}

        response = await chat_provider.generate(messages, model=model, options=options)
        assistant_message = {"role": "assistant", "content": response.text}

        return {
            "messages": messages + [assistant_message],
            "result": response.text,
            "tool_calls": response.tool_calls,
        }

    return llm_node

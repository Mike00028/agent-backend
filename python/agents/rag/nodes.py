"""Node functions for the RAG agent graph."""

from __future__ import annotations

from typing import Any

from agents.rag.retriever import InMemoryEmbeddingRetriever
from providers.interfaces import ChatProvider


def build_retriever_node(retriever: InMemoryEmbeddingRetriever):
    """Create retriever node bound to an embedding retriever implementation."""

    async def retriever_node(state):
        messages = list(state.get("messages", []))
        user_query = str(messages[-1].get("content", "")) if messages else ""

        docs = await retriever.retrieve(user_query)
        context_text = "\n\n".join(docs)

        return {
            "messages": messages,
            "retrieved_context": context_text,
            "result": "",
            "tool_calls": [{"id": "call_r1", "name": "retrieve_tool", "args": {"query": user_query}}],
            "tool_results": [{"tool_call_id": "call_r1", "content": context_text}],
        }

    return retriever_node


def build_answer_node(chat_provider: ChatProvider, default_model: str):
    """Create answer node that calls the injected chat provider."""

    async def answer_node(state):
        messages = list(state.get("messages", []))
        model = state.get("model") or default_model
        options = state.get("options") or {}
        context_text = state.get("retrieved_context", "")

        if messages:
            user_query = str(messages[-1].get("content", ""))
        else:
            user_query = ""

        prompt_messages = [
            {
                "role": "system",
                "content": (
                    "You are a grounded assistant. Use the provided context first. "
                    "If context is not enough, state that clearly."
                ),
            },
            {
                "role": "user",
                "content": f"Context:\n{context_text}\n\nQuestion:\n{user_query}",
            },
        ]

        response = await chat_provider.generate(prompt_messages, model=model, options=options)
        assistant_message = {"role": "assistant", "content": response.text}

        return {
            "messages": messages + [assistant_message],
            "result": response.text,
        }

    return answer_node

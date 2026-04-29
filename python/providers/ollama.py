"""Ollama provider implementations for chat and embeddings."""

from __future__ import annotations

import asyncio
from collections.abc import Callable
from typing import Any

from langchain_core.messages import AIMessage, BaseMessage, HumanMessage, SystemMessage

from providers.interfaces import ChatProvider, ChatResult, EmbeddingProvider


class OllamaChatProvider(ChatProvider):
    """Chat provider backed by Ollama via LangChain."""

    def __init__(self, *, base_url: str, timeout_seconds: float):
        self._base_url = base_url
        self._timeout_seconds = timeout_seconds
        self._clients: dict[str, Any] = {}

    async def generate(
        self,
        messages: list[dict[str, Any]],
        *,
        model: str,
        options: dict[str, Any] | None = None,
    ) -> ChatResult:
        client = self._get_client(model)
        lc_messages = [_to_langchain_message(m) for m in messages]

        invoke_callable: Callable[..., Any] | None = getattr(client, "ainvoke", None)
        if invoke_callable is not None:
            response = await asyncio.wait_for(
                client.ainvoke(lc_messages),
                timeout=self._timeout_seconds,
            )
        else:
            loop = asyncio.get_running_loop()
            response = await asyncio.wait_for(
                loop.run_in_executor(None, client.invoke, lc_messages),
                timeout=self._timeout_seconds,
            )

        text = response.content if isinstance(response.content, str) else str(response.content)
        tool_calls = []
        if isinstance(response, AIMessage) and response.tool_calls:
            tool_calls = [dict(tc) for tc in response.tool_calls]

        return ChatResult(text=text, tool_calls=tool_calls)

    def get_langchain_model(self, model: str) -> Any:
        """Return the raw ChatOllama instance for tool binding / react agents."""
        return self._get_client(model)

    def _get_client(self, model: str):
        chat_ollama_cls, _ = _load_ollama_classes()
        if model not in self._clients:
            self._clients[model] = chat_ollama_cls(model=model, base_url=self._base_url)
        return self._clients[model]


class OllamaEmbeddingProvider(EmbeddingProvider):
    """Embedding provider backed by Ollama via LangChain."""

    def __init__(self, *, base_url: str, timeout_seconds: float):
        self._base_url = base_url
        self._timeout_seconds = timeout_seconds
        self._clients: dict[str, Any] = {}

    async def embed_query(self, text: str, *, model: str) -> list[float]:
        client = self._get_client(model)
        loop = asyncio.get_running_loop()
        return await asyncio.wait_for(
            loop.run_in_executor(None, client.embed_query, text),
            timeout=self._timeout_seconds,
        )

    async def embed_documents(self, texts: list[str], *, model: str) -> list[list[float]]:
        client = self._get_client(model)
        loop = asyncio.get_running_loop()
        return await asyncio.wait_for(
            loop.run_in_executor(None, client.embed_documents, texts),
            timeout=self._timeout_seconds,
        )

    def _get_client(self, model: str):
        _, ollama_embeddings_cls = _load_ollama_classes()
        if model not in self._clients:
            self._clients[model] = ollama_embeddings_cls(model=model, base_url=self._base_url)
        return self._clients[model]


def _to_langchain_message(message: dict[str, Any]) -> BaseMessage:
    role = (message.get("role") or "user").lower()
    content = str(message.get("content") or "")
    if role == "system":
        return SystemMessage(content=content)
    if role == "assistant":
        return AIMessage(content=content)
    return HumanMessage(content=content)


def _load_ollama_classes():
    try:
        from langchain_ollama import ChatOllama, OllamaEmbeddings  # type: ignore[import-not-found]
    except ImportError as exc:  # pragma: no cover - environment specific
        raise RuntimeError(
            "langchain-ollama is not installed in the active interpreter. "
            "Use 'poetry install' and run with 'poetry run python server.py'."
        ) from exc
    return ChatOllama, OllamaEmbeddings

"""Gemini provider implementations for chat and embeddings via LangChain."""

from __future__ import annotations

import asyncio
from typing import Any

from langchain_core.messages import AIMessage

from providers.interfaces import ChatProvider, ChatResult, EmbeddingProvider


class GeminiChatProvider(ChatProvider):
    """Chat provider backed by Google Gemini via langchain-google-genai."""

    def __init__(self, *, api_key: str, timeout_seconds: float):
        self._api_key = api_key
        self._timeout_seconds = timeout_seconds
        self._clients: dict[str, Any] = {}

    def _get_client(self, model: str) -> Any:
        if model not in self._clients:
            from langchain_google_genai import ChatGoogleGenerativeAI
            self._clients[model] = ChatGoogleGenerativeAI(
                model=model,
                google_api_key=self._api_key,
                timeout=self._timeout_seconds,
            )
        return self._clients[model]

    def get_langchain_model(self, model: str) -> Any:
        """Return raw ChatGoogleGenerativeAI for tool binding."""
        return self._get_client(model)

    async def generate(
        self,
        messages: list[dict[str, Any]],
        *,
        model: str,
        options: dict[str, Any] | None = None,
    ) -> ChatResult:
        from langchain_core.messages import HumanMessage, SystemMessage

        lc_messages = []
        for m in messages:
            role = m.get("role", "user")
            content = m.get("content", "")
            if role == "system":
                lc_messages.append(SystemMessage(content=content))
            else:
                lc_messages.append(HumanMessage(content=content))

        client = self._get_client(model)
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


class GeminiEmbeddingProvider(EmbeddingProvider):
    """Embedding provider backed by Google Gemini via langchain-google-genai."""

    def __init__(self, *, api_key: str, timeout_seconds: float):
        self._api_key = api_key
        self._timeout_seconds = timeout_seconds
        self._clients: dict[str, Any] = {}

    def _get_client(self, model: str) -> Any:
        if model not in self._clients:
            from langchain_google_genai import GoogleGenerativeAIEmbeddings
            self._clients[model] = GoogleGenerativeAIEmbeddings(
                model=model,
                google_api_key=self._api_key,
            )
        return self._clients[model]

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

"""Provider interfaces and DTOs for LLM and embedding backends."""

from __future__ import annotations

from abc import ABC, abstractmethod
from dataclasses import dataclass, field
from typing import Any


@dataclass(frozen=True)
class ChatResult:
    """Normalized chat output shape across all providers."""

    text: str
    tool_calls: list[dict[str, Any]] = field(default_factory=list)


class ChatProvider(ABC):
    """Abstraction for chat model providers."""

    @abstractmethod
    async def generate(
        self,
        messages: list[dict[str, Any]],
        *,
        model: str,
        options: dict[str, Any] | None = None,
    ) -> ChatResult:
        """Generate a chat completion from normalized role/content messages."""

    def get_langchain_model(self, model: str) -> Any | None:
        """Return the underlying LangChain BaseChatModel for tool binding (optional)."""
        return None


class EmbeddingProvider(ABC):
    """Abstraction for embedding model providers."""

    @abstractmethod
    async def embed_query(self, text: str, *, model: str) -> list[float]:
        """Embed a single query string."""

    @abstractmethod
    async def embed_documents(self, texts: list[str], *, model: str) -> list[list[float]]:
        """Embed one or more document strings."""

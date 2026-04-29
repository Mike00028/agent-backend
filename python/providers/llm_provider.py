"""LLMProvider registry for provider resolution and future backend extensibility."""

from __future__ import annotations

from config import Config
from providers.interfaces import ChatProvider, EmbeddingProvider
from providers.ollama import OllamaChatProvider, OllamaEmbeddingProvider


class LLMProvider:
    """Creates providers behind stable interfaces (DIP + OCP)."""

    def __init__(self, config: Config):
        self._cfg = config

    def create_chat_provider(self) -> ChatProvider:
        provider_name = self._cfg.llm_provider.lower()
        if provider_name == "ollama":
            return OllamaChatProvider(
                base_url=self._cfg.ollama_base_url,
                timeout_seconds=self._cfg.provider_timeout_seconds,
            )
        if provider_name in {"azure", "gemini"}:
            raise NotImplementedError(f"chat provider '{provider_name}' is not implemented yet")
        raise ValueError(f"unsupported chat provider: {provider_name}")

    def create_embedding_provider(self) -> EmbeddingProvider:
        provider_name = self._cfg.embedding_provider.lower()
        if provider_name == "ollama":
            return OllamaEmbeddingProvider(
                base_url=self._cfg.ollama_base_url,
                timeout_seconds=self._cfg.provider_timeout_seconds,
            )
        if provider_name in {"azure", "gemini"}:
            raise NotImplementedError(f"embedding provider '{provider_name}' is not implemented yet")
        raise ValueError(f"unsupported embedding provider: {provider_name}")

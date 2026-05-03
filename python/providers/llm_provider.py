"""LLMProvider registry for provider resolution and future backend extensibility."""

from __future__ import annotations

from config import Config
from providers.interfaces import ChatProvider, EmbeddingProvider
from providers.ollama import OllamaChatProvider, OllamaEmbeddingProvider
from providers.gemini import GeminiChatProvider, GeminiEmbeddingProvider


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
        if provider_name == "gemini":
            if not self._cfg.gemini_api_key:
                raise ValueError("GEMINI_API_KEY is required when LLM_PROVIDER=gemini")
            return GeminiChatProvider(
                api_key=self._cfg.gemini_api_key,
                timeout_seconds=self._cfg.provider_timeout_seconds,
            )
        raise ValueError(f"unsupported chat provider: {provider_name}")

    def create_embedding_provider(self) -> EmbeddingProvider:
        provider_name = self._cfg.embedding_provider.lower()
        if provider_name == "ollama":
            return OllamaEmbeddingProvider(
                base_url=self._cfg.ollama_base_url,
                timeout_seconds=self._cfg.provider_timeout_seconds,
            )
        if provider_name == "gemini":
            if not self._cfg.gemini_api_key:
                raise ValueError("GEMINI_API_KEY is required when EMBEDDING_PROVIDER=gemini")
            return GeminiEmbeddingProvider(
                api_key=self._cfg.gemini_api_key,
                timeout_seconds=self._cfg.provider_timeout_seconds,
            )
        raise ValueError(f"unsupported embedding provider: {provider_name}")

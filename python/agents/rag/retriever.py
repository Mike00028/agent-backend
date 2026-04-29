"""In-memory embedding retriever for local RAG."""

from __future__ import annotations

import asyncio
import math

from providers.interfaces import EmbeddingProvider


class InMemoryEmbeddingRetriever:
    """Simple embedding-based retriever with cached document vectors."""

    def __init__(
        self,
        *,
        embedding_provider: EmbeddingProvider,
        embedding_model: str,
        documents: tuple[str, ...],
        top_k: int,
    ):
        self._embedding_provider = embedding_provider
        self._embedding_model = embedding_model
        self._documents = list(documents)
        self._top_k = max(1, top_k)
        self._doc_vectors: list[list[float]] | None = None
        self._vector_lock = asyncio.Lock()

    async def retrieve(self, query: str) -> list[str]:
        if not self._documents:
            return []

        await self._ensure_doc_vectors()
        if not self._doc_vectors:
            return []

        query_vector = await self._embedding_provider.embed_query(
            query,
            model=self._embedding_model,
        )

        scored: list[tuple[float, str]] = []
        for doc, doc_vector in zip(self._documents, self._doc_vectors, strict=False):
            score = _cosine_similarity(query_vector, doc_vector)
            scored.append((score, doc))

        scored.sort(key=lambda item: item[0], reverse=True)
        return [doc for _, doc in scored[: self._top_k]]

    async def _ensure_doc_vectors(self) -> None:
        if self._doc_vectors is not None:
            return

        async with self._vector_lock:
            if self._doc_vectors is not None:
                return
            self._doc_vectors = await self._embedding_provider.embed_documents(
                self._documents,
                model=self._embedding_model,
            )


def _cosine_similarity(left: list[float], right: list[float]) -> float:
    if not left or not right or len(left) != len(right):
        return -1.0

    dot = sum(a * b for a, b in zip(left, right, strict=False))
    norm_left = math.sqrt(sum(a * a for a in left))
    norm_right = math.sqrt(sum(b * b for b in right))
    if norm_left == 0 or norm_right == 0:
        return -1.0
    return dot / (norm_left * norm_right)

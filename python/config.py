import os
from dataclasses import dataclass


@dataclass(frozen=True)
class Config:
    grpc_port: int
    grpc_max_workers: int
    grpc_max_concurrent: int
    llm_provider: str
    llm_model: str
    embedding_provider: str
    embedding_model: str
    ollama_base_url: str
    provider_timeout_seconds: float
    rag_top_k: int
    rag_seed_documents: tuple[str, ...]


def load() -> Config:
    rag_seed_documents_raw = os.getenv(
        "RAG_SEED_DOCUMENTS",
        "LangGraph is a graph-based orchestration framework for stateful agents.|"
        "gRPC is a high-performance RPC framework built on HTTP/2.|"
        "Ollama allows running open models locally with a simple API.",
    )
    rag_seed_documents = tuple(
        part.strip() for part in rag_seed_documents_raw.split("|") if part.strip()
    )

    return Config(
        grpc_port=int(os.getenv("GRPC_PORT", "50051")),
        grpc_max_workers=int(os.getenv("GRPC_MAX_WORKERS", "10")),
        grpc_max_concurrent=int(os.getenv("GRPC_MAX_CONCURRENT", "200")),
        llm_provider=os.getenv("LLM_PROVIDER", "ollama"),
        llm_model=os.getenv("LLM_MODEL", "gemma3"),
        embedding_provider=os.getenv("EMBEDDING_PROVIDER", "ollama"),
        embedding_model=os.getenv("EMBEDDING_MODEL", "embeddinggemma300"),
        ollama_base_url=os.getenv("OLLAMA_BASE_URL", "http://localhost:11434"),
        provider_timeout_seconds=float(os.getenv("PROVIDER_TIMEOUT_SECONDS", "30")),
        rag_top_k=int(os.getenv("RAG_TOP_K", "3")),
        rag_seed_documents=rag_seed_documents,
    )

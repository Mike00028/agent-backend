import os
from dataclasses import dataclass

from dotenv import load_dotenv, find_dotenv


@dataclass(frozen=True)
class Config:
    grpc_port: int
    grpc_max_workers: int
    grpc_max_concurrent: int
    llm_provider: str
    llm_model: str
    planner_model: str
    evaluator_model: str
    embedding_provider: str
    embedding_model: str
    gemini_api_key: str
    ollama_base_url: str
    provider_timeout_seconds: float
    planner_timeout_seconds: float
    executor_timeout_seconds: float
    rag_top_k: int
    rag_seed_documents: tuple[str, ...]
    agent_max_iterations: int
    planner_max_tasks: int
    planner_max_text_len: int
    evaluator_enabled: bool
    system_prompt: str


def load() -> Config:
    load_dotenv(find_dotenv(usecwd=True))
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
        llm_model=os.getenv("CHAT_MODEL") or os.getenv("LLM_MODEL", "gemma2:2b"),
        planner_model=os.getenv("PLANNER_MODEL") or os.getenv("LLM_MODEL", "gemma2:2b"),
        evaluator_model=os.getenv("EVAL_MODEL") or os.getenv("EVALUATOR_MODEL") or os.getenv("LLM_MODEL", "gemma2:2b"),
        embedding_provider=os.getenv("EMBEDDING_PROVIDER", "ollama"),
        embedding_model=os.getenv("EMBEDDING_MODEL", "embeddinggemma300"),
        gemini_api_key=os.getenv("GEMINI_API_KEY", ""),
        ollama_base_url=os.getenv("OLLAMA_BASE_URL", "http://localhost:11434"),
        provider_timeout_seconds=float(os.getenv("PROVIDER_TIMEOUT_SECONDS", "30")),
        planner_timeout_seconds=float(os.getenv("PLANNER_TIMEOUT_SECONDS", os.getenv("PROVIDER_TIMEOUT_SECONDS", "30"))),
        executor_timeout_seconds=float(os.getenv("EXECUTOR_TIMEOUT_SECONDS", os.getenv("PROVIDER_TIMEOUT_SECONDS", "180"))),
        rag_top_k=int(os.getenv("RAG_TOP_K", "3")),
        rag_seed_documents=rag_seed_documents,
        agent_max_iterations=int(os.getenv("AGENT_MAX_ITERATIONS", "2")),
        planner_max_tasks=int(os.getenv("PLANNER_MAX_TASKS", "5")),
        planner_max_text_len=int(os.getenv("PLANNER_MAX_TEXT_LEN", "500")),
        evaluator_enabled=os.getenv("EVALUATOR_ENABLED", "true").lower() in {"1", "true", "yes"},
        system_prompt=os.getenv("SYSTEM_PROMPT", "").strip(),
    )

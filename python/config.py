import os
from dataclasses import dataclass


@dataclass(frozen=True)
class Config:
    grpc_port: int
    grpc_max_workers: int
    grpc_max_concurrent: int
    openai_api_key: str


def load() -> Config:
    return Config(
        grpc_port=int(os.getenv("GRPC_PORT", "50051")),
        grpc_max_workers=int(os.getenv("GRPC_MAX_WORKERS", "10")),
        grpc_max_concurrent=int(os.getenv("GRPC_MAX_CONCURRENT", "200")),
        openai_api_key=os.getenv("OPENAI_API_KEY", ""),
    )

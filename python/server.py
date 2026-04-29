"""gRPC server entry point."""

import asyncio
import logging
import os
import platform
import sys

import grpc
import grpc.aio

# uvloop only works on Unix/Linux
uvloop = None
if platform.system() != "Windows":
    try:
        import uvloop  # type: ignore
    except ImportError:
        uvloop = None

# Generated grpc stubs import from "langgraph.v1", so expose python/gen on sys.path.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "gen"))

from langgraph.v1 import agent_pb2_grpc
from servicer.agent_servicer import AgentServicer
from config import load

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger(__name__)


async def serve() -> None:
    cfg = load()
    server = grpc.aio.server(
        options=[
            ("grpc.max_receive_message_length", 10 * 1024 * 1024),
            ("grpc.max_send_message_length",    10 * 1024 * 1024),
        ]
    )
    agent_pb2_grpc.add_AgentServiceServicer_to_server(AgentServicer(), server)
    listen_addr = f"[::]:{cfg.grpc_port}"
    server.add_insecure_port(listen_addr)
    await server.start()
    log.info("gRPC server listening on %s", listen_addr)
    await server.wait_for_termination()


if __name__ == "__main__":
    if uvloop is not None:
        uvloop.install()
    asyncio.run(serve())

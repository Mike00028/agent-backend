"""gRPC server entry point."""

import asyncio
import logging
import sys
import os
import platform

import grpc
import grpc.aio

# uvloop only works on Unix/Linux
if platform.system() != "Windows":
    import uvloop

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
    if platform.system() != "Windows":
        uvloop.install()
    asyncio.run(serve())

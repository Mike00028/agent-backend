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

# ── OpenTelemetry bootstrap ───────────────────────────────────────────────────
def _init_otel() -> None:
    """Bootstrap OTel SDK if OTEL_EXPORTER_OTLP_ENDPOINT is configured."""
    endpoint = os.getenv("LANGFUSE_OTLP_ENDPOINT") or os.getenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
    if not endpoint:
        return
    try:
        from opentelemetry import trace, propagate
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import BatchSpanProcessor
        from opentelemetry.sdk.resources import Resource, SERVICE_NAME
        from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
        from opentelemetry.propagators.composite import CompositeHTTPPropagator
        from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator
        from opentelemetry.baggage.propagation import W3CBaggagePropagator
        import base64

        headers = {}
        pub = os.getenv("LANGFUSE_PUBLIC_KEY", "")
        sec = os.getenv("LANGFUSE_SECRET_KEY", "")
        if pub and sec:
            token = base64.b64encode(f"{pub}:{sec}".encode()).decode()
            headers["Authorization"] = f"Basic {token}"

        resource = Resource(attributes={SERVICE_NAME: os.getenv("OTEL_SERVICE_NAME", "python-agent")})
        exporter = OTLPSpanExporter(endpoint=endpoint, headers=headers)
        provider = TracerProvider(resource=resource)
        provider.add_span_processor(BatchSpanProcessor(exporter))
        trace.set_tracer_provider(provider)

        # Register W3C TraceContext propagator so Go→Python gRPC trace context
        # (traceparent header injected by Go executor) is correctly extracted
        # and Python spans become children of Go spans in the same Langfuse trace.
        propagate.set_global_textmap(CompositeHTTPPropagator([
            TraceContextTextMapPropagator(),
            W3CBaggagePropagator(),
        ]))
        logging.getLogger(__name__).info("OTel tracing enabled endpoint=%s", endpoint)
    except Exception as exc:  # noqa: BLE001
        logging.getLogger(__name__).warning("OTel init failed: %s", exc)

_init_otel()

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

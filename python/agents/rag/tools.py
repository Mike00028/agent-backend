"""Tool definitions for the RAG agent. Wire up pgvector lookups here."""

from langchain_core.tools import tool


@tool
def retrieve_tool(query: str) -> str:
    """Retrieves relevant documents from the vector store for a given query. Stub — implement pgvector search."""
    # TODO: connect to pgvector pool and run similarity search
    return f"[stub] retrieved context for: {query}"

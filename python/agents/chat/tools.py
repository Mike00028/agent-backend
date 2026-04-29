"""Tool definitions for the chat agent."""

from langchain_core.tools import tool


@tool
def hello_tool(name: str) -> str:
    """Returns a friendly greeting. Used as a boilerplate tool call to verify the pipeline."""
    return f"Hello, {name}!"

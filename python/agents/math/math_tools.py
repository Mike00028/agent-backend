"""Arithmetic tools for the math agent."""

from langchain_core.tools import tool


@tool
def add(a: float, b: float) -> float:
    """Add two numbers together."""
    return a + b


@tool
def subtract(a: float, b: float) -> float:
    """Subtract b from a."""
    return a - b


@tool
def multiply(a: float, b: float) -> float:
    """Multiply two numbers together."""
    return a * b


@tool
def divide(a: float, b: float) -> float:
    """Divide a by b. Raises ValueError if b is zero."""
    if b == 0:
        raise ValueError("Division by zero is not allowed.")
    return a / b


MATH_TOOLS = [add, subtract, multiply, divide]
MATH_TOOL_MAP = {t.name: t for t in MATH_TOOLS}

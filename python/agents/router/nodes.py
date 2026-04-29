"""Node functions for the router supervisor graph."""

from __future__ import annotations

import asyncio
import json

from agents.math.math_tools import MATH_TOOL_MAP, MATH_TOOLS
from providers.interfaces import ChatProvider

_MATH_KEYWORDS = (
    "add", "subtract", "multiply", "divide", "sum", "plus", "minus",
    "times", "divided by", "product", "difference", "quotient",
    "calculate", "compute", "math", "+", "-", "*", "/",
)
_RAG_KEYWORDS = (
    "document", "docs", "knowledge base", "search", "retrieve",
    "citation", "context", "knowledge",
)


def build_route_node(chat_provider: ChatProvider, default_model: str):
    """Classify request into chat | rag | math | multi via LLM then conditional edge."""

    async def route_node(state):
        messages = list(state.get("messages", []))
        user_text = str(messages[-1].get("content", "")) if messages else ""
        model = state.get("model") or default_model

        tool_names = ", ".join(t.name for t in MATH_TOOLS)
        classifier_prompt = [
            {
                "role": "system",
                "content": (
                    "Classify the user request. Reply with ONE word only: chat, rag, math, or multi.\n"
                    "- math: ONLY arithmetic (add, subtract, multiply, divide). "
                    f"Available math tools: {tool_names}.\n"
                    "- rag: search/retrieve from documents, knowledge base, citations.\n"
                    "- multi: message contains BOTH a math operation AND another question (chat or rag).\n"
                    "- chat: general questions, greetings, reasoning, everything else."
                ),
            },
            {"role": "user", "content": user_text},
        ]

        response = await chat_provider.generate(
            classifier_prompt, model=model, options={"temperature": 0}
        )
        raw = (response.text or "").strip().lower()
        for label in ("multi", "math", "rag", "chat"):
            if raw.startswith(label):
                return {"route": label}

        # Keyword fallback: detect multi-intent first.
        text_lower = user_text.lower()
        has_math = any(k in text_lower for k in _MATH_KEYWORDS)
        has_rag = any(k in text_lower for k in _RAG_KEYWORDS)
        has_chat = not has_math or not has_rag  # anything non-pure is chat-ish

        if has_math and (has_rag or has_chat):
            return {"route": "multi"}
        if has_math:
            return {"route": "math"}
        if has_rag:
            return {"route": "rag"}
        return {"route": "chat"}

    return route_node


def build_multi_node(chat_provider: ChatProvider, default_model: str):
    """Handle mixed-intent messages: run math tool + chat LLM concurrently, merge answers."""

    async def _run_chat(user_text: str, model: str, options: dict) -> str:
        response = await chat_provider.generate(
            [{"role": "user", "content": user_text}],
            model=model,
            options=options,
        )
        return response.text

    async def _run_math(user_text: str, model: str, options: dict) -> tuple[str, list, list]:
        """Ask LLM to emit a tool call, execute it, return (result, tool_calls, tool_results)."""
        tool_descriptions = "\n".join(
            f"- {t.name}: {t.description}" for t in MATH_TOOLS
        )
        system = (
            "Extract ONLY the arithmetic sub-question from the user message and respond "
            "with a single JSON tool call. Format:\n"
            '{"tool": "<name>", "args": {"a": <number>, "b": <number>}}\n\n'
            f"Available tools:\n{tool_descriptions}"
        )
        response = await chat_provider.generate(
            [{"role": "system", "content": system}, {"role": "user", "content": user_text}],
            model=model,
            options=options,
        )
        tool_calls = []
        tool_results = []
        math_result = ""
        try:
            raw = response.text.strip()
            if raw.startswith("```"):
                raw = raw.split("```")[1]
                if raw.startswith("json"):
                    raw = raw[4:]
            parsed = json.loads(raw.strip())
            tool_name = parsed.get("tool", "")
            args = parsed.get("args", {})
            tool = MATH_TOOL_MAP.get(tool_name)
            if tool:
                call_id = "call_multi_math"
                tool_calls.append({"id": call_id, "name": tool_name, "args": args})
                try:
                    math_result = str(tool.invoke(args))
                except ValueError as exc:
                    math_result = f"Error: {exc}"
                tool_results.append({"tool_call_id": call_id, "content": math_result})
        except (json.JSONDecodeError, AttributeError):
            math_result = response.text
        return math_result, tool_calls, tool_results

    async def multi_node(state):
        messages = list(state.get("messages", []))
        user_text = str(messages[-1].get("content", "")) if messages else ""
        model = state.get("model") or default_model
        options = state.get("options") or {}

        # Run chat and math concurrently.
        chat_answer, (math_answer, tool_calls, tool_results) = await asyncio.gather(
            _run_chat(user_text, model, options),
            _run_math(user_text, model, options),
        )

        # Merge both answers into one coherent result.
        combined = f"{chat_answer}\n\nMath result: {math_answer}"
        assistant_message = {"role": "assistant", "content": combined}
        return {
            "messages": messages + [assistant_message],
            "result": combined,
            "tool_calls": tool_calls,
            "tool_results": tool_results,
        }

    return multi_node


def pick_route(state) -> str:
    """Conditional edge resolver — returns the route key for LangGraph branching."""
    return str(state.get("route", "chat")).lower()

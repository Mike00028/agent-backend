"""Node functions for the math agent graph."""

from __future__ import annotations

import json

from langchain_core.messages import AIMessage

from agents.math.math_tools import MATH_TOOL_MAP, MATH_TOOLS
from providers.interfaces import ChatProvider


def build_math_llm_node(chat_provider: ChatProvider, default_model: str):
    """LLM node: ask the model to emit a tool call for the requested arithmetic."""

    async def math_llm_node(state):
        messages = list(state.get("messages", []))
        model = state.get("model") or default_model
        options = state.get("options") or {}

        tool_descriptions = "\n".join(
            f"- {t.name}: {t.description}" for t in MATH_TOOLS
        )
        system_prompt = (
            "You are a precise math assistant. "
            "When the user asks for arithmetic, respond ONLY with a JSON tool call in this exact format "
            "and nothing else:\n"
            '{"tool": "<tool_name>", "args": {"a": <number>, "b": <number>}}\n\n'
            f"Available tools:\n{tool_descriptions}"
        )

        prompt_messages = [
            {"role": "system", "content": system_prompt},
            *messages,
        ]

        response = await chat_provider.generate(prompt_messages, model=model, options=options)

        # Try to parse a tool call from the model response.
        tool_calls = []
        try:
            raw = response.text.strip()
            # Strip markdown code fences if present.
            if raw.startswith("```"):
                raw = raw.split("```")[1]
                if raw.startswith("json"):
                    raw = raw[4:]
            parsed = json.loads(raw.strip())
            tool_name = parsed.get("tool", "")
            args = parsed.get("args", {})
            if tool_name in MATH_TOOL_MAP:
                tool_calls = [{"id": "call_math_1", "name": tool_name, "args": args}]
        except (json.JSONDecodeError, AttributeError):
            # Model returned plain text — treat as direct answer.
            pass

        ai_message = {"role": "assistant", "content": response.text}
        return {
            "messages": messages + [ai_message],
            "result": response.text if not tool_calls else "",
            "tool_calls": tool_calls,
        }

    return math_llm_node


def build_math_tools_node():
    """Tool execution node: runs the arithmetic tool and returns the result."""

    async def math_tools_node(state):
        tool_calls = state.get("tool_calls") or []
        tool_results = []
        final_result = ""

        for call in tool_calls:
            tool = MATH_TOOL_MAP.get(call.get("name", ""))
            if tool:
                try:
                    output = tool.invoke(call["args"])
                    final_result = str(output)
                except ValueError as exc:
                    final_result = f"Error: {exc}"

                tool_results.append({
                    "tool_call_id": call.get("id", ""),
                    "content": final_result,
                })

        return {
            "messages": state.get("messages", []),
            "result": final_result,
            "tool_results": tool_results,
        }

    return math_tools_node

"""Node for the text analysis agent — runs entirely in Python, no LLM call."""
from __future__ import annotations

import json

from agents.text.tools import TOOL_MAP


def build_text_node():

    async def text_node(state):
        tool_name = str(state.get("tool_name", "")).strip()
        args: dict = state.get("args", {}) or {}

        fn = TOOL_MAP.get(tool_name)
        if fn is None:
            return {"result": json.dumps({
                "error": f"unknown tool '{tool_name}'",
                "available": list(TOOL_MAP.keys()),
            })}

        try:
            # Each tool takes different positional args — unpack from args dict.
            if tool_name == "count_vowels":
                result = fn(text=str(args.get("text", "")))
            elif tool_name == "count_consonants":
                result = fn(text=str(args.get("text", "")))
            elif tool_name == "count_word":
                result = fn(
                    word=str(args.get("word", "")),
                    paragraph=str(args.get("paragraph", "")),
                )
            else:
                result = {"error": f"unhandled tool '{tool_name}'"}
        except Exception as exc:
            result = {"error": str(exc)}

        return {"result": json.dumps(result)}

    return text_node

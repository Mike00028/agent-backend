"""Text analysis agent — ReAct loop over pure-Python tools, no LLM knowledge of internals."""
from typing import Any

from langgraph.prebuilt import create_react_agent

from agents.text.tools import TEXT_TOOLS
from providers.interfaces import ChatProvider

_SYSTEM_PROMPT = """You are a text analysis agent. You have three tools:
- count_vowels(text): count vowels in a piece of text
- count_consonants(text): count consonants in a piece of text
- count_word_occurrences(word, paragraph): count how many times a word appears

Rules:
1. The user's message contains all the text and instructions you need. Extract the text to analyze directly from their message and call the appropriate tool(s) immediately.
2. NEVER ask the user to provide a paragraph, clarify, or give more information. If text is embedded in the message, use it. If the entire message IS the text, use the whole message.
3. Call every tool that the request asks for. If the user asks for both vowels and consonants, call both tools.
4. Return the results directly without preamble."""


def build_graph(*, chat_provider: ChatProvider, default_model: str):
    """Build a ReAct agent that picks the right text tool from the user's natural language request."""
    lc_model = chat_provider.get_langchain_model(default_model)
    if lc_model is None:
        raise RuntimeError(
            f"Provider does not support tool-binding (get_langchain_model returned None) "
            f"for model '{default_model}'"
        )
    # create_react_agent binds the tools to the model internally — do not pre-bind.
    return create_react_agent(lc_model, tools=TEXT_TOOLS, prompt=_SYSTEM_PROMPT)

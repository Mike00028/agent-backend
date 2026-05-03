"""Pure-Python text analysis tools — no LLM needed."""
from __future__ import annotations

import re

from langchain_core.tools import tool


@tool
def count_vowels(text: str) -> str:
    """Count the vowels (a, e, i, o, u) in the given text. Returns total count and per-vowel breakdown."""
    vowels = set("aeiouAEIOU")
    breakdown: dict[str, int] = {}
    for ch in text:
        if ch in vowels:
            key = ch.lower()
            breakdown[key] = breakdown.get(key, 0) + 1
    total = sum(breakdown.values())
    parts = ", ".join(f"{k}={v}" for k, v in sorted(breakdown.items()))
    return f"{total} vowels ({parts}) in {len(text)} characters"


@tool
def count_consonants(text: str) -> str:
    """Count the consonants (non-vowel letters) in the given text. Returns total count and per-letter breakdown."""
    vowels = set("aeiouAEIOU")
    breakdown: dict[str, int] = {}
    for ch in text:
        if ch.isalpha() and ch not in vowels:
            key = ch.lower()
            breakdown[key] = breakdown.get(key, 0) + 1
    total = sum(breakdown.values())
    parts = ", ".join(f"{k}={v}" for k, v in sorted(breakdown.items()))
    return f"{total} consonants ({parts}) in {len(text)} characters"


@tool
def count_word_occurrences(word: str, paragraph: str) -> str:
    """Count how many times a specific word appears in a paragraph (case-insensitive, whole-word match)."""
    if not word.strip():
        return "Error: word must not be empty"
    pattern = re.compile(r'\b' + re.escape(word.strip()) + r'\b', re.IGNORECASE)
    count = len(pattern.findall(paragraph))
    word_count = len(paragraph.split())
    return f"'{word}' appears {count} time(s) in a {word_count}-word paragraph"


TEXT_TOOLS = [count_vowels, count_consonants, count_word_occurrences]

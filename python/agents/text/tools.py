"""Pure-Python text analysis tools — no LLM needed."""
from __future__ import annotations

import re


VOWELS = set("aeiouAEIOU")


def count_vowels(text: str) -> dict:
    """Count vowels in text. Returns total and per-vowel breakdown."""
    breakdown: dict[str, int] = {}
    for ch in text:
        if ch in VOWELS:
            key = ch.lower()
            breakdown[key] = breakdown.get(key, 0) + 1
    return {
        "total": sum(breakdown.values()),
        "breakdown": breakdown,
        "input_length": len(text),
    }


def count_consonants(text: str) -> dict:
    """Count consonants (letters that are not vowels) in text."""
    breakdown: dict[str, int] = {}
    for ch in text:
        if ch.isalpha() and ch not in VOWELS:
            key = ch.lower()
            breakdown[key] = breakdown.get(key, 0) + 1
    return {
        "total": sum(breakdown.values()),
        "breakdown": breakdown,
        "input_length": len(text),
    }


def count_word(word: str, paragraph: str) -> dict:
    """Count how many times `word` appears in `paragraph` (case-insensitive, whole-word)."""
    if not word.strip():
        return {"error": "word must not be empty", "count": 0}
    pattern = re.compile(r'\b' + re.escape(word.strip()) + r'\b', re.IGNORECASE)
    matches = pattern.findall(paragraph)
    return {
        "word": word.strip(),
        "count": len(matches),
        "paragraph_length": len(paragraph),
        "word_count": len(paragraph.split()),
    }


# Tool dispatch table used by the node
TOOL_MAP = {
    "count_vowels":    count_vowels,
    "count_consonants": count_consonants,
    "count_word":      count_word,
}

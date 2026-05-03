"""Generic agent harness â€” executor dispatches on task fields, not type names.

Task field conventions (set by Go/planner):
  expr            â†’ math execution (local arithmetic tools)
  retrieve=true   â†’ RAG execution (vector retrieval + LLM)
  question        â†’ generic LLM call with optional system_prompt field
  system_prompt   â†’ per-task system prompt (overrides global)
"""

from __future__ import annotations

import asyncio
import json
import logging
import re
import time
from typing import Any
from pydantic import BaseModel, Field, ValidationError

from agents.rag.retriever import InMemoryEmbeddingRetriever
from providers.interfaces import ChatProvider

_log = logging.getLogger("router")

_MATH_REGEX = re.compile(r"\d+(?:\.\d+)?\s*[\+\-\*/]\s*\d+(?:\.\d+)?")


# â”€â”€ Pydantic models â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

class Plan(BaseModel):
    mode: str = "parallel"
    tasks: list[dict[str, Any]] = Field(default_factory=list)


class EvalDecision(BaseModel):
    ok: bool
    feedback: str = ""


# â”€â”€ Helpers â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

def _log_event(event: str, **fields: Any) -> None:
    _log.info(json.dumps({"event": event, **fields}, ensure_ascii=True))


def _trim(text: str, max_len: int) -> str:
    return text if max_len <= 0 or len(text) <= max_len else text[:max_len].rstrip()


def _math_exprs(text: str) -> list[str]:
    return [m.group(0) for m in _MATH_REGEX.finditer(text)]


def _parse_expr(expr: str) -> tuple[str, str, str]:
    m = re.match(r"(\d+(?:\.\d+)?)\s*([\+\-\*/])\s*(\d+(?:\.\d+)?)", expr)
    return (m.group(1), m.group(2), m.group(3)) if m else ("", "", "")


# â”€â”€ Gate â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

def build_gate_node(chat_provider: ChatProvider, default_model: str) -> Any:
    """
    Classify intent with a cheap LLM call.
    Emits field-based tasks directly for clear-cut cases, falls through to planner for mixed.
    """

    _RAG_KEYWORDS = (
        "document", "docs", "knowledge base", "search", "retrieve",
        "citation", "context", "knowledge",
    )

    async def gate_node(state):
        start = time.monotonic()
        messages = list(state.get("messages", []))
        user_text = str(messages[-1].get("content", "")) if messages else ""
        text_lower = user_text.lower()
        request_id = state.get("request_id", "")

        has_math = bool(_MATH_REGEX.search(text_lower))
        has_rag = any(k in text_lower for k in _RAG_KEYWORDS)
        exprs = _math_exprs(user_text) if has_math else []
        non_math = " ".join(re.sub(r"\d+(?:\.\d+)?\s*[\+\-\*/]\s*\d+(?:\.\d+)?", " ", user_text).split())
        mixed = bool(re.search(r"[A-Za-z]", non_math))

        if has_math and not has_rag and not mixed:
            _log_event("gate.match", request_id=request_id, route="math",
                       latency_ms=int((time.monotonic() - start) * 1000))
            return {
                "gate": "execute",
                "plan_mode": "parallel",
                "plan_tasks": [{"expr": e} for e in (exprs or [user_text])],
            }

        if has_rag and not has_math:
            _log_event("gate.match", request_id=request_id, route="rag",
                       latency_ms=int((time.monotonic() - start) * 1000))
            return {
                "gate": "execute",
                "plan_mode": "parallel",
                "plan_tasks": [{"question": user_text, "retrieve": True}],
            }

        if user_text.strip():
            model = state.get("planner_model") or state.get("model") or default_model
            system = 'Classify user intent. Return JSON only: {"intent": "math" | "rag" | "chat" | "mixed"}.'
            try:
                response = await chat_provider.generate(
                    [{"role": "system", "content": system}, {"role": "user", "content": user_text}],
                    model=model,
                    options={"temperature": 0},
                )
                raw = (response.text or "").strip()
                if raw.startswith("```"):
                    raw = raw.split("```", 1)[1].lstrip("json").strip()
                intent = str(json.loads(raw.strip()).get("intent", "mixed")).lower()
                _log_event("gate.intent", request_id=request_id, intent=intent,
                           latency_ms=int((time.monotonic() - start) * 1000))

                if intent == "math":
                    if not exprs:
                        exprs.extend(_math_exprs(user_text) or [user_text])
                    return {"gate": "execute", "plan_mode": "parallel",
                            "plan_tasks": [{"expr": e} for e in exprs]}
                if intent == "rag":
                    return {"gate": "execute", "plan_mode": "parallel",
                            "plan_tasks": [{"question": user_text, "retrieve": True}]}
                if intent == "chat":
                    return {"gate": "execute", "plan_mode": "parallel",
                            "plan_tasks": [{"question": user_text}]}
            except (json.JSONDecodeError, AttributeError, KeyError):
                pass

        _log_event("gate.fallback", request_id=request_id, route="plan",
                   latency_ms=int((time.monotonic() - start) * 1000))
        return {"gate": "plan"}

    return gate_node


# â”€â”€ Planner â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

_PLANNER_SYSTEM = (
    "You are a planner. Break the user request into tasks.\n"
    "Return JSON only:\n"
    '{"mode":"parallel"|"sequential","tasks":[...]}\n\n'
    "Each task uses only the fields that apply:\n"
    '  {"question":"..."} â€” answer a question or explain something\n'
    '  {"expr":"..."} â€” arithmetic expression to compute\n'
    '  {"question":"...","retrieve":true} â€” look up in knowledge base\n\n'
    "Rules:\n"
    "- Use parallel for independent tasks, sequential if order matters.\n"
    "- Split compound questions into separate tasks.\n"
    "- Arithmetic goes in expr, never in question.\n"
)


def build_planner_node(chat_provider: ChatProvider, default_model: str):

    async def planner_node(state):
        start = time.monotonic()
        messages = list(state.get("messages", []))
        user_text = str(messages[-1].get("content", "")) if messages else ""
        model = state.get("planner_model") or state.get("model") or default_model
        feedback = str(state.get("eval_feedback", "")).strip()
        request_id = state.get("request_id", "")
        max_tasks = int(state.get("plan_max_tasks", 5))
        max_text_len = int(state.get("plan_max_text_len", 500))

        system = _PLANNER_SYSTEM + (f"\nEvaluator feedback: {feedback}\n" if feedback else "")

        planner_timeout = float(state.get("planner_timeout_seconds", 30))
        response = await asyncio.wait_for(
            chat_provider.generate(
                [{"role": "system", "content": system}, {"role": "user", "content": user_text}],
                model=model,
                options={"temperature": 0},
            ),
            timeout=planner_timeout,
        )

        raw = (response.text or "").strip()
        mode = "parallel"
        raw_tasks: list[dict[str, Any]] = []
        try:
            if raw.startswith("```"):
                raw = raw.split("```", 1)[1].lstrip("json").strip()
            plan = Plan.model_validate_json(raw.strip())
            mode = plan.mode if plan.mode in {"parallel", "sequential"} else "parallel"
            raw_tasks = plan.tasks
        except (ValidationError, json.JSONDecodeError, AttributeError):
            parts = [p.strip() for p in re.split(r"\band\b|[.?!]", user_text) if p.strip()]
            raw_tasks = [{"question": _trim(p, max_text_len)} for p in parts] or [{"question": user_text}]
            mode = "parallel" if len(raw_tasks) > 1 else "sequential"

        # Normalise: keep only known fields, trim text
        normalized: list[dict[str, Any]] = []
        for t in raw_tasks[:max_tasks]:
            task: dict[str, Any] = {}
            if "expr" in t:
                task["expr"] = _trim(str(t["expr"]), max_text_len)
            if "question" in t:
                task["question"] = _trim(str(t["question"]), max_text_len)
            if t.get("retrieve"):
                task["retrieve"] = True
            if t.get("system_prompt"):
                task["system_prompt"] = _trim(str(t["system_prompt"]), max_text_len)
            if task:
                normalized.append(task)

        # Guard: mixed query must have a question task for non-math text
        exprs = _math_exprs(user_text)
        non_math = " ".join(re.sub(r"\d+(?:\.\d+)?\s*[\+\-\*/]\s*\d+(?:\.\d+)?", " ", user_text).split())
        if exprs and bool(re.search(r"[A-Za-z]", non_math)) and not any("question" in t for t in normalized):
            normalized = [{"question": _trim(non_math, max_text_len)}] + \
                         [{"expr": _trim(e, max_text_len)} for e in exprs]
            normalized = normalized[:max_tasks]
            mode = "parallel"

        _log_event("planner.done", request_id=request_id, model=model, mode=mode,
                   task_count=len(normalized), tasks=normalized,
                   latency_ms=int((time.monotonic() - start) * 1000))

        plan_payload = json.dumps({"mode": mode, "tasks": normalized}, ensure_ascii=True)
        return {"plan_mode": mode, "plan_tasks": normalized, "message": plan_payload}

    return planner_node


# â”€â”€ Executor â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

def build_executor_node(
    chat_provider: ChatProvider,
    retriever: InMemoryEmbeddingRetriever,
    default_model: str,
):
    """
    Field-driven executor. No type strings, no agent name comparisons.
      task has 'expr'            â†’ arithmetic tools
      task has 'retrieve=true'   â†’ vector retrieval + LLM
      task has 'question'        â†’ generic LLM call
    """

    async def _exec_math(expr: str) -> tuple[str, list, list]:
        expressions = _math_exprs(expr)
        if not expressions:
            return "Unable to parse math expression.", [], []
        results: list[str] = []
        for expression in expressions:
            a_str, op, b_str = _parse_expr(expression)
            if not a_str:
                results.append("Unable to parse math expression.")
                continue
            try:
                a, b = float(a_str), float(b_str)
                if op == "+":
                    value = a + b
                elif op == "-":
                    value = a - b
                elif op == "*":
                    value = a * b
                elif op == "/":
                    if b == 0:
                        results.append("Error: division by zero")
                        continue
                    value = a / b
                else:
                    results.append("Unsupported operator.")
                    continue
                results.append(str(int(value) if value == int(value) else value))
            except (ValueError, OverflowError) as exc:
                results.append(f"Error: {exc}")
        return "\n".join(results), [], []

    async def _exec_rag(question: str, model: str, options: dict, system_prompt: str) -> tuple[str, list, list]:
        docs = await retriever.retrieve(question)
        context_text = "\n\n".join(docs)
        prompt = (
            "Use the context if relevant. If not enough, say so.\n\n"
            f"Context:\n{context_text}\n\nQuestion:\n{question}"
        )
        msgs = []
        if system_prompt:
            msgs.append({"role": "system", "content": system_prompt})
        msgs.append({"role": "user", "content": prompt})
        response = await chat_provider.generate(msgs, model=model, options=options)
        return response.text, [], []

    async def _exec_llm(question: str, model: str, options: dict, system_prompt: str) -> str:
        msgs = []
        if system_prompt:
            msgs.append({"role": "system", "content": system_prompt})
        msgs.append({"role": "user", "content": question})
        response = await chat_provider.generate(msgs, model=model, options=options)
        return response.text

    async def _run_task(task: dict[str, Any], model: str, options: dict, global_system: str) -> tuple[str, list, list]:
        task_system = task.get("system_prompt") or global_system
        if "expr" in task:
            return await _exec_math(task["expr"])
        if task.get("retrieve") and "question" in task:
            return await _exec_rag(task["question"], model, options, task_system)
        if "question" in task:
            return await _exec_llm(task["question"], model, options, task_system), [], []
        return "No executable field in task.", [], []

    async def executor_node(state):
        start = time.monotonic()
        model = state.get("model") or default_model
        options = state.get("options") or {}
        tasks = list(state.get("plan_tasks", []))
        mode = state.get("plan_mode", "parallel")
        request_id = state.get("request_id", "")
        executor_timeout = float(state.get("executor_timeout_seconds", 30))
        global_system = str(state.get("system_prompt", "")).strip()

        results: list[str] = []
        tool_calls: list[dict[str, Any]] = []
        tool_results: list[dict[str, Any]] = []

        try:
            if mode == "sequential":
                for task in tasks:
                    result, tc, tr = await asyncio.wait_for(
                        _run_task(task, model, options, global_system), timeout=executor_timeout
                    )
                    results.append(result)
                    tool_calls.extend(tc)
                    tool_results.extend(tr)
            else:
                outputs = await asyncio.wait_for(
                    asyncio.gather(*(_run_task(t, model, options, global_system) for t in tasks)),
                    timeout=executor_timeout,
                )
                for result, tc, tr in outputs:
                    results.append(result)
                    tool_calls.extend(tc)
                    tool_results.extend(tr)
        except asyncio.TimeoutError:
            error_text = f"Executor timed out after {executor_timeout}s"
            _log_event("executor.timeout", request_id=request_id,
                       timeout_seconds=executor_timeout, task_count=len(tasks),
                       latency_ms=int((time.monotonic() - start) * 1000))
            return {"messages": state.get("messages", []), "result": error_text,
                    "error": error_text, "tool_calls": tool_calls, "tool_results": tool_results}

        combined = "\n\n".join(r for r in results if r)
        _log_event("executor.done", request_id=request_id, mode=mode,
                   task_count=len(tasks), latency_ms=int((time.monotonic() - start) * 1000))

        return {
            "messages": state.get("messages", []) + [{"role": "assistant", "content": combined}],
            "result": combined,
            "tool_calls": tool_calls,
            "tool_results": tool_results,
        }

    return executor_node


# â”€â”€ Evaluator â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

def build_evaluator_node(chat_provider: ChatProvider, default_model: str) -> Any:

    async def evaluator_node(state):
        start = time.monotonic()
        model = state.get("evaluator_model") or state.get("model") or default_model
        messages = list(state.get("messages", []))
        result_text = str(state.get("result", "")).strip()
        iteration = int(state.get("iteration", 0))
        max_iterations = int(state.get("max_iterations", 1))
        request_id = state.get("request_id", "")
        evaluator_enabled = bool(state.get("evaluator_enabled", True))
        error_text = str(state.get("error", "")).strip()

        if error_text:
            _log_event("evaluator.skip_error", request_id=request_id, iteration=iteration,
                       latency_ms=int((time.monotonic() - start) * 1000))
            return {"eval_ok": True, "iteration": iteration, "error": error_text}

        if not evaluator_enabled:
            _log_event("evaluator.disabled", request_id=request_id, iteration=iteration,
                       latency_ms=int((time.monotonic() - start) * 1000))
            return {"eval_ok": True, "iteration": iteration}

        if iteration >= max_iterations:
            _log_event("evaluator.skip", request_id=request_id, iteration=iteration,
                       max_iterations=max_iterations,
                       latency_ms=int((time.monotonic() - start) * 1000))
            return {"eval_ok": True, "iteration": iteration}

        user_content = messages[-1]["content"] if messages else ""
        response = await chat_provider.generate(
            [
                {"role": "system", "content": (
                    "You are a strict evaluator. Return JSON only: "
                    '{"ok": true|false, "feedback": "short fix guidance"}.'
                )},
                {"role": "user", "content": f"USER: {user_content}\nANSWER: {result_text}"},
            ],
            model=model,
            options={"temperature": 0},
        )
        raw = (response.text or "").strip()
        try:
            if raw.startswith("```"):
                raw = raw.split("```", 1)[1].lstrip("json").strip()
            decision = EvalDecision.model_validate_json(raw.strip())
            decision_data = {"eval_ok": bool(decision.ok), "eval_feedback": decision.feedback,
                             "iteration": iteration + 1}
            _log_event("evaluator.done", request_id=request_id, ok=decision_data["eval_ok"],
                       iteration=decision_data["iteration"],
                       latency_ms=int((time.monotonic() - start) * 1000))
            return decision_data
        except (ValidationError, json.JSONDecodeError, AttributeError):
            _log_event("evaluator.error", request_id=request_id, iteration=iteration,
                       latency_ms=int((time.monotonic() - start) * 1000))
            return {"eval_ok": True, "iteration": iteration}

    return evaluator_node


# â”€â”€ Graph routing helpers â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

def pick_gate(state) -> str:
    return str(state.get("gate", "plan")).lower()


def pick_eval(state) -> str:
    return "done" if state.get("eval_ok") else "retry"

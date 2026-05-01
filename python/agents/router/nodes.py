"""Generic agent harness — executor dispatches on task fields, not type names.

Task field conventions (set by Go/planner):
  expr            → math execution (local arithmetic tools)
  retrieve=true   → RAG execution (vector retrieval + LLM)
  question        → generic LLM call with optional system_prompt field
  system_prompt   → per-task system prompt (overrides global)
"""

from __future__ import annotations

import asyncio
import json
import logging
import re
import time
from typing import Any
from pydantic import BaseModel, Field, ValidationError

from agents.math.math_tools import add, divide, multiply, subtract
from agents.rag.retriever import InMemoryEmbeddingRetriever
from providers.interfaces import ChatProvider

_log = logging.getLogger("router")

_MATH_REGEX = re.compile(r"\d+(?:\.\d+)?\s*[\+\-\*/]\s*\d+(?:\.\d+)?")


# ── Pydantic models ───────────────────────────────────────────────────────────

class Plan(BaseModel):
    mode: str = "parallel"
    tasks: list[dict[str, Any]] = Field(default_factory=list)


class EvalDecision(BaseModel):
    ok: bool
    feedback: str = ""


# ── Helpers ───────────────────────────────────────────────────────────────────

def _log_event(event: str, **fields: Any) -> None:
    _log.info(json.dumps({"event": event, **fields}, ensure_ascii=True))


def _trim(text: str, max_len: int) -> str:
    return text if max_len <= 0 or len(text) <= max_len else text[:max_len].rstrip()


def _math_exprs(text: str) -> list[str]:
    return [m.group(0) for m in _MATH_REGEX.finditer(text)]


def _parse_expr(expr: str) -> tuple[str, str, str]:
    m = re.match(r"(\d+(?:\.\d+)?)\s*([\+\-\*/])\s*(\d+(?:\.\d+)?)", expr)
    return (m.group(1), m.group(2), m.group(3)) if m else ("", "", "")


# ── Gate ──────────────────────────────────────────────────────────────────────

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


# ── Planner ───────────────────────────────────────────────────────────────────

_PLANNER_SYSTEM = (
    "You are a planner. Break the user request into tasks.\n"
    "Return JSON only:\n"
    '{"mode":"parallel"|"sequential","tasks":[...]}\n\n'
    "Each task uses only the fields that apply:\n"
    '  {"question":"..."} — answer a question or explain something\n'
    '  {"expr":"..."} — arithmetic expression to compute\n'
    '  {"question":"...","retrieve":true} — look up in knowledge base\n\n'
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


# ── Executor ──────────────────────────────────────────────────────────────────

def build_executor_node(
    chat_provider: ChatProvider,
    retriever: InMemoryEmbeddingRetriever,
    default_model: str,
):
    """
    Field-driven executor. No type strings, no agent name comparisons.
      task has 'expr'            → arithmetic tools
      task has 'retrieve=true'   → vector retrieval + LLM
      task has 'question'        → generic LLM call
    """

    _TOOL_MAP = {
        "+": ("add", add),
        "-": ("subtract", subtract),
        "*": ("multiply", multiply),
        "/": ("divide", divide),
    }

    async def _exec_math(expr: str) -> tuple[str, list, list]:
        expressions = _math_exprs(expr)
        if not expressions:
            return "Unable to parse math expression.", [], []
        tool_calls: list[dict[str, Any]] = []
        tool_results: list[dict[str, Any]] = []
        results: list[str] = []
        for idx, expression in enumerate(expressions, start=1):
            a_str, op, b_str = _parse_expr(expression)
            if not a_str:
                results.append("Unable to parse math expression.")
                continue
            tool_name, tool_fn = _TOOL_MAP.get(op, ("", None))
            if tool_fn is None:
                results.append("Unsupported operator.")
                continue
            call_id = f"call_math_{idx}"
            try:
                value = tool_fn.invoke({"a": float(a_str), "b": float(b_str)})
            except ValueError as exc:
                value = f"Error: {exc}"
            results.append(str(value))
            tool_calls.append({"id": call_id, "name": tool_name, "args": {"a": float(a_str), "b": float(b_str)}})
            tool_results.append({"tool_call_id": call_id, "content": str(value)})
        return "\n".join(results), tool_calls, tool_results

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


# ── Evaluator ─────────────────────────────────────────────────────────────────

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


# ── Graph routing helpers ─────────────────────────────────────────────────────

def pick_gate(state) -> str:
    return str(state.get("gate", "plan")).lower()


def pick_eval(state) -> str:
    return "done" if state.get("eval_ok") else "retry"
"""Planner + executor nodes for the supervisor graph."""

from __future__ import annotations

import asyncio
import json
import logging
import re
import time
from typing import Any
from pydantic import BaseModel, Field, ValidationError

from agents.math.math_tools import MATH_TOOLS, add, divide, multiply, subtract
from agents.rag.retriever import InMemoryEmbeddingRetriever
from providers.interfaces import ChatProvider

_log = logging.getLogger("router")

_MATH_REGEX = re.compile(r"\d+(?:\.\d+)?\s*[\+\-\*/]\s*\d+(?:\.\d+)?")

_RAG_KEYWORDS = (
    "document", "docs", "knowledge base", "search", "retrieve",
    "citation", "context", "knowledge",
)


class PlannerTask(BaseModel):
    type: str
    question: str | None = None
    expr: str | None = None


class PlannerPlan(BaseModel):
    mode: str = "parallel"
    tasks: list[PlannerTask] = Field(default_factory=list)


class EvalDecision(BaseModel):
    ok: bool
    feedback: str = ""


class IntentDecision(BaseModel):
    intent: str  # validated against registry + "mixed" at use time


def _log_event(event: str, **fields: Any) -> None:
    payload = {"event": event, **fields}
    _log.info(json.dumps(payload, ensure_ascii=True))


def build_gate_node(chat_provider: ChatProvider, default_model: str) -> Any:
    """Routing gate with optional intent classification for ambiguous cases."""

    async def gate_node(state):
        start = time.monotonic()
        messages = list(state.get("messages", []))
        user_text = str(messages[-1].get("content", "")) if messages else ""
        text_lower = user_text.lower()
        request_id = state.get("request_id", "")

        # TODO: check workflow/agent table in DB to prefill plan_tasks/plan_mode.

        has_math = bool(_MATH_REGEX.search(text_lower))
        has_rag = any(k in text_lower for k in _AGENT_KEYWORDS.get("rag", ()))

        math_exprs = _extract_math_expressions(user_text) if has_math else []
        non_math_text = user_text
        for expr in math_exprs:
            non_math_text = non_math_text.replace(expr, " ")
        mixed_intent = bool(re.search(r"[A-Za-z]", non_math_text))

        if has_math and not has_rag and not mixed_intent:
            if not math_exprs:
                math_exprs = [user_text]
            _log_event(
                "gate.match",
                request_id=request_id,
                route="math",
                latency_ms=int((time.monotonic() - start) * 1000),
            )
            return {
                "gate": "execute",
                "plan_mode": "parallel",
                "plan_tasks": [{"type": "math", "expr": expr} for expr in math_exprs],
            }
        if has_rag and not has_math:
            _log_event(
                "gate.match",
                request_id=request_id,
                route="rag",
                latency_ms=int((time.monotonic() - start) * 1000),
            )
            return {
                "gate": "execute",
                "plan_mode": "parallel",
                "plan_tasks": [{"type": "rag", "question": user_text}],
            }

        if user_text.strip():
            model = state.get("planner_model") or state.get("model") or default_model
            system = intent_schema()
            try:
                response = await chat_provider.generate(
                    [{"role": "system", "content": system}, {"role": "user", "content": user_text}],
                    model=model,
                    options={"temperature": 0},
                )
                raw = (response.text or "").strip()
                if raw.startswith("```"):
                    raw = raw.split("```", 1)[1]
                    if raw.startswith("json"):
                        raw = raw[4:]
                intent = IntentDecision.model_validate_json(raw.strip()).intent
                _log_event(
                    "gate.intent",
                    request_id=request_id,
                    intent=intent,
                    latency_ms=int((time.monotonic() - start) * 1000),
                )
                agent = get_agent(intent)
                if agent and intent != "mixed":
                    input_field = agent.input_field
                    if intent == "math":
                        if not math_exprs:
                            math_exprs = _extract_math_expressions(user_text) or [user_text]
                        return {
                            "gate": "execute",
                            "plan_mode": "parallel",
                            "plan_tasks": [{"type": intent, input_field: expr} for expr in math_exprs],
                        }
                    return {
                        "gate": "execute",
                        "plan_mode": "parallel",
                        "plan_tasks": [{"type": intent, input_field: user_text}],
                    }
            except (ValidationError, json.JSONDecodeError, AttributeError):
                pass
        _log_event(
            "gate.fallback",
            request_id=request_id,
            route="plan",
            latency_ms=int((time.monotonic() - start) * 1000),
        )
        return {"gate": "plan"}

    return gate_node


def build_planner_node(chat_provider: ChatProvider, default_model: str):
    """Create a planner that outputs tasks and execution mode (parallel/sequential)."""

    async def planner_node(state):
        start = time.monotonic()
        messages = list(state.get("messages", []))
        user_text = str(messages[-1].get("content", "")) if messages else ""
        model = state.get("planner_model") or state.get("model") or default_model
        feedback = str(state.get("eval_feedback", "")).strip()
        request_id = state.get("request_id", "")

        tool_names = ", ".join(t.name for t in MATH_TOOLS)
        system = planner_schema() + f"Available math tools: {tool_names}.\n"
        if feedback:
            system += f"\nEvaluator feedback: {feedback}\n"

        planner_timeout = float(state.get("planner_timeout_seconds", 30))
        response = await asyncio.wait_for(
            chat_provider.generate(
                [{"role": "system", "content": system}, {"role": "user", "content": user_text}],
                model=model,
                options={"temperature": 0},
            ),
            timeout=planner_timeout,
        )

        mode = "parallel"
        tasks: list[PlannerTask] = []
        raw = (response.text or "").strip()
        try:
            if raw.startswith("```"):
                raw = raw.split("```")[1]
                if raw.startswith("json"):
                    raw = raw[4:]
            plan = PlannerPlan.model_validate_json(raw.strip())
            mode = plan.mode
            tasks = plan.tasks
        except (ValidationError, json.JSONDecodeError, AttributeError):
            # Fallback: simple heuristic split on 'and'.
            parts = [p.strip() for p in user_text.split(" and ") if p.strip()]
            tasks = [PlannerTask(type="chat", question=part) for part in parts] or [
                PlannerTask(type="chat", question=user_text)
            ]
            mode = "parallel" if len(tasks) > 1 else "sequential"

        max_tasks = int(state.get("plan_max_tasks", 5))
        max_text_len = int(state.get("plan_max_text_len", 500))

        # Lightweight validation — normalise each task using registry
        known_types = set(agent_names())
        normalized: list[dict[str, Any]] = []
        for task in tasks:
            task_type = task.type if task.type in known_types else "chat"
            agent = get_agent(task_type)
            input_field = agent.input_field if agent else "question"
            if input_field == "expr":
                value = (task.expr or task.question or "").strip()
                value = _trim_text(value, max_text_len)
                normalized.append({"type": task_type, input_field: value})
            else:
                value = (task.question or "").strip() or user_text
                value = _trim_text(value, max_text_len)
                normalized.append({"type": task_type, input_field: value})

        if len(normalized) > max_tasks:
            normalized = normalized[:max_tasks]

        if mode not in {"parallel", "sequential"}:
            mode = "parallel"

        # Guard: if mixed query, ensure non-math text has a non-expr-type task
        math_exprs = _extract_math_expressions(user_text)
        non_math_text = user_text
        for expr in math_exprs:
            non_math_text = non_math_text.replace(expr, " ")
        non_math_text = " ".join(non_math_text.split())
        has_non_math = bool(re.search(r"[A-Za-z]", non_math_text))
        non_expr_types = {a.name for a in all_agents() if a.input_field != "expr"}
        has_non_expr_task = any(task.get("type") in non_expr_types for task in normalized)
        if math_exprs and has_non_math and not has_non_expr_task:
            enforced: list[dict[str, Any]] = []
            if non_math_text:
                enforced.append({"type": "chat", "question": _trim_text(non_math_text, max_text_len)})
            for expr in math_exprs:
                enforced.append({"type": "math", "expr": _trim_text(expr, max_text_len)})
            normalized = enforced[:max_tasks]
            mode = "parallel"

        plan_message = {
            "type": "plan",
            "mode": mode,
            "tasks": normalized,
        }

        _log_event(
            "planner.done",
            request_id=request_id,
            model=model,
            mode=mode,
            task_count=len(normalized),
            tasks=normalized,
            latency_ms=int((time.monotonic() - start) * 1000),
        )
        plan_payload = json.dumps(plan_message, ensure_ascii=True)
        return {
            "plan_mode": mode,
            "plan_tasks": normalized,
            "message": plan_payload,
            "thinking": plan_payload,
        }

    return planner_node


def build_executor_node(
    chat_provider: ChatProvider,
    retriever: InMemoryEmbeddingRetriever,
    default_model: str,
):
    """Execute planned tasks either sequentially or in parallel."""

    async def _run_math(expr: str, model: str) -> tuple[str, list, list]:
        expressions = _extract_math_expressions(expr)
        if not expressions:
            return "Unable to parse math expression.", [], []

        tool_calls: list[dict[str, Any]] = []
        tool_results: list[dict[str, Any]] = []
        results: list[str] = []

        for idx, expression in enumerate(expressions, start=1):
            a_str, op, b_str = _extract_math_parts(expression)
            if not a_str or not b_str or not op:
                results.append("Unable to parse math expression.")
                continue

            a = float(a_str)
            b = float(b_str)
            result, call, tool_result = _compute_math_expression(op, a, b, idx)
            results.append(result)
            if call:
                tool_calls.append(call)
            if tool_result:
                tool_results.append(tool_result)

        return "\n".join(results), tool_calls, tool_results

    async def _run_chat(question: str, model: str, options: dict, system_prompt: str) -> str:
        messages = []
        if system_prompt:
            messages.append({"role": "system", "content": system_prompt})
        messages.append({"role": "user", "content": question})
        response = await chat_provider.generate(messages, model=model, options=options)
        return response.text

    async def _run_rag(
        question: str,
        model: str,
        options: dict,
        system_prompt: str,
    ) -> tuple[str, list, list]:
        docs = await retriever.retrieve(question)
        context_text = "\n\n".join(docs)
        prompt = (
            "Use the context if relevant. If not enough, say so.\n\n"
            f"Context:\n{context_text}\n\nQuestion:\n{question}"
        )
        messages = []
        if system_prompt:
            messages.append({"role": "system", "content": system_prompt})
        messages.append({"role": "user", "content": prompt})
        response = await chat_provider.generate(messages, model=model, options=options)
        return response.text, [], []

    async def _run_task(task: dict[str, Any], model: str, options: dict, system_prompt: str):
        task_type = task.get("type", "chat")
        agent = get_agent(task_type)
        input_field = agent.input_field if agent else "question"
        if task_type == "math":
            return await _run_math(task.get(input_field, ""), model)
        if task_type == "rag":
            return await _run_rag(task.get(input_field, ""), model, options, system_prompt)
        # Default: chat (or any registered question-type agent)
        return await _run_chat(task.get(input_field, ""), model, options, system_prompt), [], []

    async def executor_node(state):
        start = time.monotonic()
        model = state.get("model") or default_model
        options = state.get("options") or {}
        tasks = list(state.get("plan_tasks", []))
        mode = state.get("plan_mode", "parallel")
        request_id = state.get("request_id", "")
        executor_timeout = float(state.get("executor_timeout_seconds", 30))
        system_prompt = str(state.get("system_prompt", "")).strip()

        results: list[str] = []
        tool_calls: list[dict[str, Any]] = []
        tool_results: list[dict[str, Any]] = []

        try:
            if mode == "sequential":
                for task in tasks:
                    result, t_calls, t_results = await asyncio.wait_for(
                        _run_task(task, model, options, system_prompt),
                        timeout=executor_timeout,
                    )
                    results.append(result)
                    tool_calls.extend(t_calls)
                    tool_results.extend(t_results)
            else:
                outputs = await asyncio.wait_for(
                    asyncio.gather(*(_run_task(task, model, options, system_prompt) for task in tasks)),
                    timeout=executor_timeout,
                )
                for result, t_calls, t_results in outputs:
                    results.append(result)
                    tool_calls.extend(t_calls)
                    tool_results.extend(t_results)
        except asyncio.TimeoutError:
            _log_event(
                "executor.timeout",
                request_id=request_id,
                timeout_seconds=executor_timeout,
                task_count=len(tasks),
                latency_ms=int((time.monotonic() - start) * 1000),
            )
            error_text = f"Executor timed out after {executor_timeout} seconds"
            return {
                "messages": state.get("messages", []),
                "result": error_text,
                "error": error_text,
                "tool_calls": tool_calls,
                "tool_results": tool_results,
            }

        combined = "\n\n".join(r for r in results if r)
        assistant_message = {"role": "assistant", "content": combined}

        _log_event(
            "executor.done",
            request_id=request_id,
            mode=mode,
            task_count=len(tasks),
            latency_ms=int((time.monotonic() - start) * 1000),
        )

        return {
            "messages": state.get("messages", []) + [assistant_message],
            "result": combined,
            "tool_calls": tool_calls,
            "tool_results": tool_results,
        }

    return executor_node


def _trim_text(text: str, max_len: int) -> str:
    if max_len <= 0:
        return text
    if len(text) <= max_len:
        return text
    return text[:max_len].rstrip()


def _extract_math_parts(expr: str) -> tuple[str, str, str]:
    match = re.match(r"(\d+(?:\.\d+)?)\s*([\+\-\*/])\s*(\d+(?:\.\d+)?)", expr)
    if not match:
        return "", "", ""
    return match.group(1), match.group(2), match.group(3)


def _extract_math_expressions(text: str) -> list[str]:
    return [match.group(0) for match in _MATH_REGEX.finditer(text)]


def _compute_math_expression(op: str, a: float, b: float, index: int) -> tuple[str, dict[str, Any] | None, dict[str, Any] | None]:
    tool_map = {
        "+": ("add", add),
        "-": ("subtract", subtract),
        "*": ("multiply", multiply),
        "/": ("divide", divide),
    }
    tool_name, tool_fn = tool_map.get(op, ("", None))
    if tool_fn is None:
        return "Unsupported math operator.", None, None

    call_id = f"call_math_{index}"
    call = {"id": call_id, "name": tool_name, "args": {"a": a, "b": b}}
    try:
        result = tool_fn.invoke({"a": a, "b": b})
    except ValueError as exc:
        result = f"Error: {exc}"
    tool_result = {"tool_call_id": call_id, "content": str(result)}
    return str(result), call, tool_result


def build_evaluator_node(chat_provider: ChatProvider, default_model: str) -> Any:
    """Evaluate combined result and decide whether to iterate."""

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
            _log_event(
                "evaluator.skip_error",
                request_id=request_id,
                iteration=iteration,
                latency_ms=int((time.monotonic() - start) * 1000),
            )
            return {"eval_ok": True, "iteration": iteration, "error": error_text}

        if not evaluator_enabled:
            _log_event(
                "evaluator.disabled",
                request_id=request_id,
                iteration=iteration,
                latency_ms=int((time.monotonic() - start) * 1000),
            )
            return {"eval_ok": True, "iteration": iteration}

        if iteration >= max_iterations:
            _log_event(
                "evaluator.skip",
                request_id=request_id,
                iteration=iteration,
                max_iterations=max_iterations,
                latency_ms=int((time.monotonic() - start) * 1000),
            )
            return {"eval_ok": True, "iteration": iteration}

        system = (
            "You are a strict evaluator. Determine if the response fully answers all user questions. "
            "Return JSON only: {\"ok\": true|false, \"feedback\": \"short fix guidance\"}."
        )
        response = await chat_provider.generate(
            [
                {"role": "system", "content": system},
                {"role": "user", "content": f"USER: {messages[-1]['content'] if messages else ''}\nANSWER: {result_text}"},
            ],
            model=model,
            options={"temperature": 0},
        )

        raw = (response.text or "").strip()
        try:
            if raw.startswith("```"):
                raw = raw.split("```")[1]
                if raw.startswith("json"):
                    raw = raw[4:]
            decision = EvalDecision.model_validate_json(raw.strip())
            decision_data = {
                "eval_ok": bool(decision.ok),
                "eval_feedback": decision.feedback,
                "iteration": iteration + 1,
            }
            _log_event(
                "evaluator.done",
                request_id=request_id,
                ok=decision_data["eval_ok"],
                iteration=decision_data["iteration"],
                latency_ms=int((time.monotonic() - start) * 1000),
            )
            return decision_data
        except (ValidationError, json.JSONDecodeError, AttributeError):
            _log_event(
                "evaluator.error",
                request_id=request_id,
                iteration=iteration,
                latency_ms=int((time.monotonic() - start) * 1000),
            )
            return {"eval_ok": True, "iteration": iteration}

    return evaluator_node


def pick_gate(state) -> str:
    return str(state.get("gate", "plan")).lower()


def pick_eval(state) -> str:
    if state.get("eval_ok"):
        return "done"
    return "retry"

package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mike00028/golang-backend/services/api/internal/dag"
	"github.com/mike00028/golang-backend/services/api/internal/llm"
)

// -- Structured output model (the "Pydantic" struct for Go) -------------------

// TaskArgs holds every possible argument across all agents.
// All fields are optional (omitempty) — each agent uses only its own field.
type TaskArgs struct {
	Question     string `json:"question,omitempty"     description:"Required for chat_agent, text_agent, rag_agent: verbatim excerpt from the user message for this specific task"`
	Expr         string `json:"expr,omitempty"         description:"Required for math_agent only: bare arithmetic expression e.g. 144*37 or {t1}+56"`
	SystemPrompt string `json:"system_prompt,omitempty" description:"Optional system prompt override for chat_agent only"`
}

// PlannedTask is a single node in the LLM-generated execution plan.
type PlannedTask struct {
	ID            string   `json:"id"             description:"Unique task ID, e.g. t1, t2"`
	Title         string   `json:"title"          description:"Short human-readable task title, e.g. Multiply 144 × 37"`
	ToolName      string   `json:"tool_name"      description:"Name of the agent to invoke"`
	Args          TaskArgs `json:"args"           description:"Arguments for the agent"`
	DependsOn     []string `json:"depends_on"     description:"IDs of tasks that must complete before this one; empty means parallel"`
	ExecutionMode string   `json:"execution_mode" description:"parallel if depends_on is empty, sequential if depends_on is not empty"`
}

// DAGPlan is the full plan returned by the planner LLM.
type DAGPlan struct {
	Tasks     []PlannedTask `json:"tasks"     description:"Ordered list of tasks to execute"`
	Reasoning string        `json:"reasoning" description:"Step-by-step reasoning for the plan"`
}

// -- Planner -------------------------------------------------------------------

// Planner satisfies dag.PlannerClient by calling an llm.Client with structured output.
type Planner struct {
	client llm.Client
	model  string
}

// NewPlanner creates a Planner backed by any llm.Client.
func NewPlanner(client llm.Client, model string) *Planner {
	return &Planner{client: client, model: model}
}

// Plan implements dag.PlannerClient.
// It calls Ollama with a structured schema for DAGPlan and converts the result.
func (p *Planner) Plan(ctx context.Context, req dag.GoPlanRequest) (*dag.GoPlanResult, error) {
	messages := []llm.Message{
		{Role: "system", Content: buildPlannerSystemPrompt(req)},
		{Role: "user", Content: req.Message},
	}

	var plan DAGPlan
	if err := p.client.ChatInto(ctx, p.model, messages, &plan); err != nil {
		return nil, fmt.Errorf("planner LLM failed: %w", err)
	}
	if len(plan.Tasks) == 0 {
		return nil, fmt.Errorf("planner returned empty task list")
	}

	// Log the plan so routing issues are visible without a debugger.
	for _, pt := range plan.Tasks {
		slog.Info("planner.task",
			"id", pt.ID,
			"tool", pt.ToolName,
			"question", pt.Args.Question,
			"expr", pt.Args.Expr,
		)
	}

	// Post-plan normalization — the planner LLM often omits args even with
	// structured output. Enforce invariants deterministically here so the
	// executor always receives well-formed tasks.
	for i, pt := range plan.Tasks {
		def, _ := dag.AgentByName(pt.ToolName)
		// 1. Every question-based agent must have a non-empty question.
		//    Fall back to the full user message — better than nothing.
		if def.NeedsQuestion && plan.Tasks[i].Args.Question == "" {
			plan.Tasks[i].Args.Question = req.Message
			slog.Warn("planner.normalize: injected question from user message",
				"task_id", pt.ID, "tool", pt.ToolName)
		}
		// 2. math_agent must have a non-empty expr.
		if pt.ToolName == "math_agent" && plan.Tasks[i].Args.Expr == "" {
			plan.Tasks[i].Args.Expr = req.Message
			slog.Warn("planner.normalize: injected expr from user message",
				"task_id", pt.ID)
		}
	}

	tasks := make([]*dag.Task, len(plan.Tasks))
	for i, pt := range plan.Tasks {
		// Serialize typed TaskArgs → compact JSON for downstream handlers.
		// Always marshal (struct, not a map — never nil).
		argsJSON := "{}"
		if b, err := json.Marshal(pt.Args); err == nil {
			argsJSON = string(b)
		}
		execMode := pt.ExecutionMode
		if execMode == "" {
			if len(pt.DependsOn) == 0 {
				execMode = "parallel"
			} else {
				execMode = "sequential"
			}
		}
		tasks[i] = &dag.Task{
			ID:            pt.ID,
			Title:         pt.Title,
			ToolName:      pt.ToolName,
			ArgsJSON:      argsJSON,
			DependsOn:     pt.DependsOn,
			ExecutionMode: execMode,
			Status:        dag.StatusPending,
		}
	}
	return &dag.GoPlanResult{Tasks: tasks, Reasoning: plan.Reasoning}, nil
}

// -- System prompt builder -----------------------------------------------------

func buildPlannerSystemPrompt(req dag.GoPlanRequest) string {
	var sb strings.Builder

	sb.WriteString("You are a task planner for an AI agent system.\n")
	sb.WriteString("Given a user message and available tools, generate a JSON execution plan.\n\n")

	if req.AgentSpecJSON != "" {
		var spec struct {
			Tools        []string `json:"tools"`
			SystemPrompt string   `json:"system_prompt"`
		}
		if err := json.Unmarshal([]byte(req.AgentSpecJSON), &spec); err == nil {
			if len(spec.Tools) > 0 {
				sb.WriteString("Available tools: " + strings.Join(spec.Tools, ", ") + "\n")
			}
			if spec.SystemPrompt != "" {
				sb.WriteString("Agent persona: " + spec.SystemPrompt + "\n")
			}
		}
	}

	if req.MemoryContext != "" {
		sb.WriteString("\nRelevant context from memory:\n" + req.MemoryContext + "\n")
	}

	if req.Feedback != "" {
		sb.WriteString(fmt.Sprintf(
			"\nPrevious attempt (generation %d) failed. Feedback: %s\n"+
				"Generate an improved plan that addresses this feedback.\n",
			req.Generation, req.Feedback,
		))
	}

	sb.WriteString("\nAvailable agents:\n")
	for _, a := range dag.AgentRegistry {
		sb.WriteString(fmt.Sprintf("  %-16s — %s\n", a.Name, a.Description))
	}
	// Custom agents from this session's AgentSpec (e.g. loaded from DB).
	// When a description is available it's listed; otherwise the name alone is shown.
	// The executor routes these to Python gRPC automatically (IsLocal=false default).
	if req.AgentSpecJSON != "" {
		var spec struct {
			SubAgents []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"sub_agents"`
		}
		// Try rich format [{"name":"...","description":"..."}] first.
		if err := json.Unmarshal([]byte(req.AgentSpecJSON), &spec); err == nil && len(spec.SubAgents) > 0 {
			for _, sa := range spec.SubAgents {
				// Skip if already in the static registry.
				if _, found := dag.AgentByName(sa.Name); found {
					continue
				}
				desc := sa.Description
				if desc == "" {
					desc = "custom agent"
				}
				sb.WriteString(fmt.Sprintf("  %-16s — %s\n", sa.Name, desc))
			}
		} else {
			// Fallback: legacy flat string list ["agent_a", "agent_b"].
			var flat struct {
				SubAgents []string `json:"sub_agents"`
			}
			if err := json.Unmarshal([]byte(req.AgentSpecJSON), &flat); err == nil {
				for _, name := range flat.SubAgents {
					if _, found := dag.AgentByName(name); !found {
						sb.WriteString(fmt.Sprintf("  %-16s — custom agent\n", name))
					}
				}
			}
		}
	}
	sb.WriteString("\nTask scoping rules for args.question:\n")
	sb.WriteString("- Each task must do exactly ONE thing. One agent, one operation, one focused question.\n")
	sb.WriteString("- args.question must contain ONLY the text and operation for that specific task — nothing else from the user message.\n")
	sb.WriteString("- If the user asks for multiple operations (e.g. count vowels AND count a word), create a SEPARATE task for each operation.\n")
	sb.WriteString("- args.question must include the actual text to operate on, copied verbatim from the user message.\n")
	sb.WriteString("- args.question must NEVER be empty for chat_agent, text_agent, or rag_agent.\n")
	sb.WriteString("\nExample — user says: \"Count vowels and how many times 'fox' appears in 'The quick brown fox'\"\n")
	sb.WriteString(`{"tasks":[` +
		`{"id":"t1","title":"Count vowels","tool_name":"text_agent","args":{"question":"Count vowels in 'The quick brown fox'"},"depends_on":[],"execution_mode":"parallel"},` +
		`{"id":"t2","title":"Count word 'fox'","tool_name":"text_agent","args":{"question":"How many times does 'fox' appear in 'The quick brown fox'"},"depends_on":[],"execution_mode":"parallel"},` +
		`{"id":"t3","title":"Combine results","tool_name":"summarize_agent","args":{},"depends_on":["t1","t2"],"execution_mode":"sequential"}` +
		`]}` + "\n")
	sb.WriteString("\nSequential vs parallel:\n")
	sb.WriteString("- PARALLEL: tasks are independent → depends_on: [], execution_mode: \"parallel\".\n")
	sb.WriteString("- SEQUENTIAL: task B needs task A's output → depends_on: [\"t1\"], execution_mode: \"sequential\".\n")
	sb.WriteString("- Always set execution_mode. Set title to a short description of what this task does.\n")
	sb.WriteString("- When a task depends on another, its output is forwarded automatically — do NOT repeat the prior answer in args.\n")
	sb.WriteString("- For math that uses a prior result, write the expr as \"{t1} + 56\" (use the task ID in curly braces as placeholder).\n")
	sb.WriteString("\nRules:\n")
	sb.WriteString("1. Use math_agent for every arithmetic expression (+, -, *, /). Never use chat_agent for math.\n")
	sb.WriteString("2. Use rag_agent only for LangGraph, gRPC, or Ollama internals.\n")
	sb.WriteString("3. Use text_agent for ANY request involving counting vowels, consonants, or word occurrences.\n")
	sb.WriteString("   - Any words or phrase in the user message after 'in', 'in the text', 'for', 'the following' IS the text to analyse — copy it verbatim into args.question.\n")
	sb.WriteString("   - Example: 'count vowels in good things take time' → args.question = \"count vowels in good things take time\".\n")
	sb.WriteString("   - Do NOT route vowel/consonant/word-count tasks to chat_agent under any circumstances.\n")
	sb.WriteString("4. Use chat_agent for everything else.\n")
	sb.WriteString("5. If the request has multiple unrelated parts, create one task per part. Add a final summarize_agent depending on all when there are 2+ tasks.\n")
	sb.WriteString("6. tool_name must be one of the agents listed above.\n")
	sb.WriteString("7. CLARIFICATION RULE: If required inputs are genuinely missing (e.g. text_agent needs text but NONE is present anywhere in the user message),\n")
	sb.WriteString("   create ONE single clarify_agent task. Set args.question to a single concise sentence asking for ALL missing inputs.\n")
	sb.WriteString("   Do NOT use chat_agent for clarification. Do NOT create multiple clarification tasks.\n")
	sb.WriteString("   Do NOT create summarize_agent for a single clarification.\n")

	return sb.String()
}

package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mike00028/golang-backend/services/api/internal/dag"
	"github.com/mike00028/golang-backend/services/api/internal/llm"
)

// -- Structured output model (the "Pydantic" struct for Go) -------------------

// TaskArgs holds every possible argument across all agents.
// Only the relevant field(s) are populated per task; unused fields are omitted.
type TaskArgs struct {
	Question     string `json:"question,omitempty"      description:"Text question or instruction (chat_agent, rag_agent, summarize_agent)"`
	Expr         string `json:"expr,omitempty"          description:"Bare arithmetic expression to evaluate, e.g. 144*37 (math_agent only)"`
	SystemPrompt string `json:"system_prompt,omitempty" description:"Optional system prompt override (chat_agent only)"`
	// text_agent fields
	Tool      string `json:"tool,omitempty"      description:"Tool to call: count_vowels | count_consonants | count_word (text_agent only)"`
	Text      string `json:"text,omitempty"      description:"Input text for count_vowels or count_consonants (text_agent only)"`
	Word      string `json:"word,omitempty"      description:"Word to search for (text_agent count_word only)"`
	Paragraph string `json:"paragraph,omitempty" description:"Paragraph to search within (text_agent count_word only)"`
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
			SubAgents    []string `json:"sub_agents"`
		}
		if err := json.Unmarshal([]byte(req.AgentSpecJSON), &spec); err == nil {
			if len(spec.Tools) > 0 {
				sb.WriteString("Available tools: " + strings.Join(spec.Tools, ", ") + "\n")
			}
			if len(spec.SubAgents) > 0 {
				sb.WriteString("Available sub-agents: " + strings.Join(spec.SubAgents, ", ") + "\n")
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
	sb.WriteString("  chat_agent      — answers general questions, explains concepts, writes code\n")
	sb.WriteString("  math_agent      — evaluates arithmetic expressions (+, -, *, /); args: {\"expr\": \"<expression>\"}\n")
	sb.WriteString("  rag_agent       — looks up internal docs about LangGraph, gRPC, or Ollama\n")
	sb.WriteString("  summarize_agent — merges results from multiple tasks into a final answer\n")
	sb.WriteString("  text_agent      — counts vowels, consonants, or word occurrences in text; tools: count_vowels, count_consonants, count_word\n")
	sb.WriteString("\nSequential vs parallel:\n")
	sb.WriteString("- PARALLEL: tasks are independent → depends_on: [], execution_mode: \"parallel\".\n")
	sb.WriteString("- SEQUENTIAL: task B needs task A's output → depends_on: [\"t1\"], execution_mode: \"sequential\".\n")
	sb.WriteString("- Always set execution_mode. Set title to a short description of what this task does.\n")
	sb.WriteString("- When a task depends on another, its output is forwarded automatically — do NOT repeat the prior answer in args.\n")
	sb.WriteString("\nRules:\n")
	sb.WriteString("1. Use math_agent for every arithmetic expression (+, -, *, /). Never use chat_agent for math.\n")
	sb.WriteString("2. Use rag_agent only for LangGraph, gRPC, or Ollama internals.\n")
	sb.WriteString("3. Use text_agent for any vowel/consonant/word-count request.\n")
	sb.WriteString("4. Use chat_agent for everything else.\n")
	sb.WriteString("5. If the request has multiple unrelated parts, create one task per part plus a final summarize_agent that depends on all.\n")
	sb.WriteString("6. tool_name must be one of the agents listed above.\n")

	return sb.String()
}

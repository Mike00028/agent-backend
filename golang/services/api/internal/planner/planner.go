package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mike00028/golang-backend/services/api/internal/dag"
)

// -- Structured output model (the "Pydantic" struct for Go) -------------------

// PlannedTask is a single node in the LLM-generated execution plan.
type PlannedTask struct {
	ID        string   `json:"id"         description:"Unique task ID, e.g. t1, t2"`
	ToolName  string   `json:"tool_name"  description:"Name of the MCP tool to invoke"`
	ArgsJSON  string   `json:"args_json"  description:"JSON-encoded arguments for the tool"`
	DependsOn []string `json:"depends_on" description:"IDs of tasks that must complete before this one"`
}

// DAGPlan is the full plan returned by the planner LLM.
type DAGPlan struct {
	Tasks     []PlannedTask `json:"tasks"     description:"Ordered list of tasks to execute"`
	Reasoning string        `json:"reasoning" description:"Step-by-step reasoning for the plan"`
}

// -- Planner -------------------------------------------------------------------

// Planner satisfies dag.PlannerClient by calling Ollama with structured output.
type Planner struct {
	ollama *OllamaClient
	model  string
}

// NewPlanner creates a Planner. It satisfies dag.PlannerClient.
func NewPlanner(ollama *OllamaClient, model string) *Planner {
	return &Planner{ollama: ollama, model: model}
}

// Plan implements dag.PlannerClient.
// It calls Ollama with a structured schema for DAGPlan and converts the result.
func (p *Planner) Plan(ctx context.Context, req dag.GoPlanRequest) (*dag.GoPlanResult, error) {
	messages := []Message{
		{Role: "system", Content: buildPlannerSystemPrompt(req)},
		{Role: "user", Content: req.Message},
	}

	var plan DAGPlan
	if err := p.ollama.ChatInto(ctx, p.model, messages, &plan); err != nil {
		return nil, fmt.Errorf("planner LLM failed: %w", err)
	}
	if len(plan.Tasks) == 0 {
		return nil, fmt.Errorf("planner returned empty task list")
	}

	tasks := make([]*dag.Task, len(plan.Tasks))
	for i, pt := range plan.Tasks {
		tasks[i] = &dag.Task{
			ID:        pt.ID,
			ToolName:  pt.ToolName,
			ArgsJSON:  pt.ArgsJSON,
			DependsOn: pt.DependsOn,
			Status:    dag.StatusPending,
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

	sb.WriteString("\nRules:\n")
	sb.WriteString("- Each task must use a tool from the available tools list.\n")
	sb.WriteString("- Use depends_on to enforce ordering where one task needs another's output.\n")
	sb.WriteString("- Tasks with no dependencies will run in parallel.\n")
	sb.WriteString("- Keep the plan minimal: only as many tasks as needed.\n")

	return sb.String()
}

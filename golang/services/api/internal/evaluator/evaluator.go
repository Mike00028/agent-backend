package evaluator

import (
	"context"
	"fmt"
	"strings"

	"github.com/mike00028/golang-backend/services/api/internal/dag"
	"github.com/mike00028/golang-backend/services/api/internal/planner"
)

// -- Structured output model ---------------------------------------------------

// evalOutput is the raw structured response from the evaluator LLM.
type evalOutput struct {
	EvalOK   bool    `json:"eval_ok"  description:"true if the task results satisfy the user goal"`
	Score    float64 `json:"score"    description:"Quality score between 0.0 and 1.0"`
	Feedback string  `json:"feedback" description:"Specific actionable feedback if eval_ok is false"`
	Summary  string  `json:"summary"  description:"One-sentence summary of the results for the user"`
}

// -- Evaluator -----------------------------------------------------------------

// Evaluator satisfies dag.EvaluatorClient using Ollama structured output.
type Evaluator struct {
	ollama *planner.OllamaClient
	model  string
}

// NewEvaluator creates an Evaluator. It satisfies dag.EvaluatorClient.
func NewEvaluator(ollama *planner.OllamaClient, model string) *Evaluator {
	return &Evaluator{ollama: ollama, model: model}
}

// Eval implements dag.EvaluatorClient.
func (e *Evaluator) Eval(ctx context.Context, req dag.GoEvalRequest) (*dag.EvalResult, error) {
	messages := []planner.Message{
		{Role: "system", Content: buildEvalSystemPrompt()},
		{Role: "user", Content: buildEvalUserMessage(req)},
	}

	var out evalOutput
	if err := e.ollama.ChatInto(ctx, e.model, messages, &out); err != nil {
		return nil, fmt.Errorf("evaluator LLM failed: %w", err)
	}

	return &dag.EvalResult{
		EvalOK:   out.EvalOK,
		Score:    out.Score,
		Feedback: out.Feedback,
		Summary:  out.Summary,
	}, nil
}

// -- Prompt builders -----------------------------------------------------------

func buildEvalSystemPrompt() string {
	return `You are a strict evaluator for an AI agent system.
You will receive a user goal and a list of completed tasks with their outputs.
Assess whether the task results collectively fulfill the user goal.

Rules:
- Set eval_ok = true only if the outputs fully and correctly address the user goal.
- Set score between 0.0 (total failure) and 1.0 (perfect).
- If eval_ok = false, provide specific, actionable feedback on what is missing or wrong.
- Keep the summary concise (one sentence) suitable for the end user.`
}

func buildEvalUserMessage(req dag.GoEvalRequest) string {
	var sb strings.Builder
	sb.WriteString("User goal: " + req.UserMessage + "\n\nExecuted tasks:\n")
	for _, t := range req.Tasks {
		sb.WriteString(fmt.Sprintf("  [%s] tool=%s status=%s\n", t.ID, t.ToolName, t.Status))
		if t.Output != "" {
			out := t.Output
			if len(out) > 500 {
				out = out[:500] + "...(truncated)"
			}
			sb.WriteString("    output: " + out + "\n")
		}
		if t.Error != "" {
			sb.WriteString("    error: " + t.Error + "\n")
		}
	}
	return sb.String()
}

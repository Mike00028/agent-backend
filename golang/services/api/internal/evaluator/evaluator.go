package evaluator

import (
	"context"
	"fmt"
	"strings"

	"github.com/mike00028/golang-backend/services/api/internal/dag"
	"github.com/mike00028/golang-backend/services/api/internal/llm"
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

// Evaluator satisfies dag.EvaluatorClient using an llm.Client for structured output.
type Evaluator struct {
	client llm.Client
	model  string
}

// NewEvaluator creates an Evaluator backed by any llm.Client.
func NewEvaluator(client llm.Client, model string) *Evaluator {
	return &Evaluator{client: client, model: model}
}

// Eval implements dag.EvaluatorClient.
func (e *Evaluator) Eval(ctx context.Context, req dag.GoEvalRequest) (*dag.EvalResult, error) {
	messages := []llm.Message{
		{Role: "system", Content: buildEvalSystemPrompt()},
		{Role: "user", Content: buildEvalUserMessage(req)},
	}

	var out evalOutput
	if err := e.client.ChatInto(ctx, e.model, messages, &out); err != nil {
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
	return `You are an evaluator for an AI agent system.
You will receive a user goal and a list of completed tasks with their outputs.
Assess whether the task results collectively fulfill the user goal.

Rules:
- Set eval_ok = false ONLY when a required task failed with an error OR a clearly mandatory part of the user goal is completely missing from all outputs.
- Set eval_ok = true when the outputs address the user's intent, even if phrasing or formatting could be improved.
- Set score between 0.0 (total failure) and 1.0 (perfect).
- If eval_ok = false, provide specific, actionable feedback on what is missing or wrong.
- Keep the summary concise (one sentence) suitable for the end user.
- Do NOT set eval_ok = false just because the answer could be more detailed or polished.`
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

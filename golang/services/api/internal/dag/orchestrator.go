package dag

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	langgraphv1 "github.com/mike00028/golang-backend/services/api/internal/langgraphv1/langgraph/v1"
	"github.com/mike00028/golang-backend/services/api/internal/telemetry"
)

const maxRefinementGeneration = 2

// -- Planner / Evaluator interfaces -------------------------------------------
// Satisfied by internal/planner.Planner and internal/evaluator.Evaluator.
// Defined here so the dag package stays free of concrete dependencies.

// GoPlanRequest carries everything the planner needs.
type GoPlanRequest struct {
	SessionID     string
	UserID        string
	AgentID       string
	Message       string
	AgentSpecJSON string
	MemoryContext string
	Feedback      string
	Generation    int
}

// GoPlanResult is returned by the planner.
type GoPlanResult struct {
	Tasks     []*Task
	Reasoning string
}

// GoEvalRequest carries everything the evaluator needs.
type GoEvalRequest struct {
	SessionID   string
	UserMessage string
	Tasks       []*Task
}

// PlannerClient is implemented by the Go-native Ollama planner.
type PlannerClient interface {
	Plan(ctx context.Context, req GoPlanRequest) (*GoPlanResult, error)
}

// EvaluatorClient is implemented by the Go-native Ollama evaluator.
type EvaluatorClient interface {
	Eval(ctx context.Context, req GoEvalRequest) (*EvalResult, error)
}

// SummarizerClient synthesizes multiple task outputs into one response.
// Implemented by internal/planner.OllamaSummarizer.
type SummarizerClient interface {
	Summarize(ctx context.Context, userMessage string, taskOutputs []string) (string, error)
}

// -- Orchestrator --------------------------------------------------------------

// BeforePlanFunc is called before every planning attempt.
// Returning a non-nil error aborts the invocation immediately.
// Use it for: cost estimation, PII checks on the prompt, budget guards.
type BeforePlanFunc func(ctx context.Context, req *GoPlanRequest) error

// Orchestrator drives the full DAG loop: plan -> validate -> execute -> eval -> refine.
type Orchestrator struct {
	planner    PlannerClient
	evaluator  EvaluatorClient
	summarizer SummarizerClient
	executor   *Executor
	checkpoint CheckpointWriter
	beforePlan []BeforePlanFunc
	events     chan<- SSEEvent
	tracer     telemetry.Tracer
}

// NewOrchestrator creates an Orchestrator.
func NewOrchestrator(planner PlannerClient, evaluator EvaluatorClient, executor *Executor, cp CheckpointWriter) *Orchestrator {
	return &Orchestrator{
		planner:    planner,
		evaluator:  evaluator,
		executor:   executor,
		checkpoint: cp,
		tracer:     telemetry.NewTracer("dag.orchestrator"),
	}
}

// SetSummarizer attaches a Go-native summarizer (backed by Ollama).
func (o *Orchestrator) SetSummarizer(s SummarizerClient) {
	o.summarizer = s
}

// SetEvents attaches the SSE event channel so the orchestrator can emit
// plan_ready before task execution begins.
func (o *Orchestrator) SetEvents(ch chan<- SSEEvent) {
	o.events = ch
}

func (o *Orchestrator) emit(ev SSEEvent) {
	if o.events == nil {
		return
	}
	select {
	case o.events <- ev:
	default:
	}
}

// AddBeforePlan registers a hook that runs before every PlanDAG call.
// Hooks run in registration order; the first error short-circuits the rest.
func (o *Orchestrator) AddBeforePlan(fn BeforePlanFunc) {
	o.beforePlan = append(o.beforePlan, fn)
}

// RunRequest is the input to Run.
type RunRequest struct {
	SessionID     string
	UserID        string
	AgentID       string
	Message       string
	AgentSpecJSON string
	MemoryContext string
}

// RunResult is returned when the DAG loop completes.
type RunResult struct {
	FinalOutput      string
	ConfidenceScore  float64
	ConfidenceReason string
	EvalOK           bool
}

// Run executes the full DAG loop.
func (o *Orchestrator) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	return o.runGeneration(ctx, req, 0, "")
}

func (o *Orchestrator) runGeneration(ctx context.Context, req RunRequest, gen int, feedback string) (*RunResult, error) {
	ctx, span := o.tracer.Start(ctx, fmt.Sprintf("dag.generation.%d", gen),
		telemetry.StringAttr("langfuse.session.id", req.SessionID),
		telemetry.StringAttr("langfuse.user.id", req.UserID),
		telemetry.StringAttr("agent.id", req.AgentID),
		telemetry.IntAttr("generation", gen),
	)
	defer span.End()

	// -- Step 1: Plan -----------------------------------------------------------
	planReq := GoPlanRequest{
		SessionID:     req.SessionID,
		UserID:        req.UserID,
		AgentID:       req.AgentID,
		Message:       req.Message,
		AgentSpecJSON: req.AgentSpecJSON,
		MemoryContext: req.MemoryContext,
		Feedback:      feedback,
		Generation:    gen,
	}
	for _, hook := range o.beforePlan {
		if err := hook(ctx, &planReq); err != nil {
			return nil, fmt.Errorf("before-plan hook rejected request: %w", err)
		}
	}
	var planResult *GoPlanResult
	{
		pctx, pspan := o.tracer.Start(ctx, "dag.plan",
			telemetry.StringAttr("langfuse.observation.input", req.Message),
		)
		var perr error
		planResult, perr = o.planner.Plan(pctx, planReq)
		if perr != nil {
			pspan.RecordError(perr)
			pspan.SetError(perr.Error())
			pspan.End()
			return nil, fmt.Errorf("planner failed (gen=%d): %w", gen, perr)
		}
		planJSON, _ := json.Marshal(planResult.Tasks)
		pspan.SetAttr(
			telemetry.IntAttr("task.count", len(planResult.Tasks)),
			telemetry.StringAttr("langfuse.observation.output", string(planJSON)),
		)
		pspan.End()
	}

	// Emit plan_ready immediately so the frontend shows task identifiers
	// before the (potentially slow) execution phase begins.
	type planTask struct {
		ID            string   `json:"id"`
		Title         string   `json:"title"`
		Tool          string   `json:"tool"`
		DepsOn        []string `json:"depends_on"`
		ExecutionMode string   `json:"execution_mode"`
	}
	planTasks := make([]planTask, len(planResult.Tasks))
	for i, t := range planResult.Tasks {
		planTasks[i] = planTask{
			ID:            t.ID,
			Title:         t.Title,
			Tool:          t.ToolName,
			DepsOn:        t.DependsOn,
			ExecutionMode: t.ExecutionMode,
		}
	}
	planPayload, _ := json.Marshal(map[string]interface{}{
		"generation": gen,
		"reasoning":  planResult.Reasoning,
		"tasks":      planTasks,
	})
	o.emit(SSEEvent{Type: "plan_ready", Payload: string(planPayload)})

	// -- Step 2: Build DAG with stable IDs -------------------------------------
	dagID := fmt.Sprintf("%s/g%d", req.SessionID, gen)
	prevDAGID := ""
	if gen > 0 {
		prevDAGID = fmt.Sprintf("%s/g%d", req.SessionID, gen-1)
	}
	d := &DAG{
		ID:            dagID,
		PreviousDAGID: prevDAGID,
		SessionID:     req.SessionID,
		UserID:        req.UserID,
		AgentID:       req.AgentID,
		UserMessage:   req.Message,
		AgentSpecJSON: req.AgentSpecJSON,
		Reasoning:     planResult.Reasoning,
		Generation:    gen,
		Tasks:         planResult.Tasks,
	}

	// -- Step 3: Validate -------------------------------------------------------
	if err := validateDAG(d, req.AgentSpecJSON); err != nil {
		return nil, fmt.Errorf("DAG validation failed: %w", err)
	}

	// -- Step 4: Topo-sort ------------------------------------------------------
	batches, err := TopoSort(d.Tasks)
	if err != nil {
		return nil, fmt.Errorf("topo-sort failed: %w", err)
	}

	// -- Step 5: Load prior progress (crash-resume) ----------------------------
	// LoadCompletedTasks returns outputs for tasks that already reached 'done'
	// in a previous run of this session+generation. At-least-once semantics:
	// tools must be idempotent for tasks that were done but not yet visible here.
	results := make(map[string]string, len(d.Tasks))
	if prior, loadErr := o.checkpoint.LoadCompletedTasks(ctx, req.SessionID, gen); loadErr == nil {
		for taskID, output := range prior {
			results[taskID] = output
			// Mark tasks as done so the executor skips them in this batch.
			for _, task := range d.Tasks {
				if task.ID == taskID {
					task.Status = StatusDone
					task.Output = output
				}
			}
		}
	}
	// If LoadCompletedTasks fails we simply re-execute all tasks — safe because
	// Python tools are expected to be idempotent.

	// -- Step 6: Execute batches (skip already-done tasks) --------------------
	var batchErrors []string
	for _, batch := range batches {
		// Filter to only tasks that are not yet done.
		pending := batch[:0]
		for _, t := range batch {
			if t.Status != StatusDone {
				pending = append(pending, t)
			}
		}
		if len(pending) == 0 {
			continue
		}
		bctx, bspan := o.tracer.Start(ctx, fmt.Sprintf("dag.execute_batch.%d", len(batchErrors)),
			telemetry.IntAttr("tasks.pending", len(pending)),
		)
		if berr := o.executor.RunBatch(bctx, d, pending, results); berr != nil {
			bspan.RecordError(berr)
			bspan.SetError(berr.Error())
			batchErrors = append(batchErrors, berr.Error())
		}
		bspan.End()
	}
	// If every task failed and we have nothing to show, surface the error.
	if len(results) == 0 && len(batchErrors) > 0 {
		return nil, fmt.Errorf("all tasks failed: %s", strings.Join(batchErrors, "; "))
	}

	// -- Step 7: Auto-summarize via Go Ollama (no gRPC) ----------------------
	// If more than one task ran and the planner didn't already include a
	// summarize_agent task, synthesize all outputs into one response in Go.
	finalOutput := o.autoSummarize(ctx, req.Message, d.Tasks, results)

	// -- Step 8: Evaluate -------------------------------------------------------
	ectx, espan := o.tracer.Start(ctx, "dag.eval",
		telemetry.StringAttr("langfuse.observation.input", req.Message),
	)
	eval, err := o.evaluator.Eval(ectx, GoEvalRequest{
		SessionID:   req.SessionID,
		UserMessage: req.Message,
		Tasks:       d.Tasks,
	})
	if err != nil {
		espan.RecordError(err)
		eval = &EvalResult{EvalOK: false, Score: 0.5, Feedback: fmt.Sprintf("eval error: %s", err)}
	} else {
		espan.SetAttr(
			telemetry.BoolAttr("eval.ok", eval.EvalOK),
			telemetry.Float64Attr("eval.score", eval.Score),
			telemetry.StringAttr("langfuse.observation.output", eval.Summary),
		)
	}
	espan.End()

	// -- Step 9: Refine or return -----------------------------------------------
	// Only trigger a refinement cycle when the evaluator is truly unhappy
	// (score < 0.5). A strict eval_ok=false with a high score just means the
	// LLM evaluator is being cautious — not worth re-running everything.
	if !eval.EvalOK && eval.Score < 0.5 {
		if gen < maxRefinementGeneration {
			return o.runGeneration(ctx, req, gen+1, eval.Feedback)
		}
		score, reason := calculateConfidence(*eval, gen)
		// Final generation exhausted — delete checkpoint rows immediately.
		_ = o.checkpoint.DeleteSession(ctx, req.SessionID)
		return &RunResult{
			FinalOutput:      finalOutput,
			ConfidenceScore:  score,
			ConfidenceReason: reason,
			EvalOK:           false,
		}, nil
	}

	// Success path — delete checkpoint rows immediately.
	_ = o.checkpoint.DeleteSession(ctx, req.SessionID)
	return &RunResult{
		FinalOutput:      finalOutput,
		ConfidenceScore:  eval.Score,
		ConfidenceReason: eval.Summary,
		EvalOK:           true,
	}, nil
}

// autoSummarize calls the Go-native summarizer when multiple tasks ran.
// Falls back to simple concatenation if no summarizer is set or the call fails.
func (o *Orchestrator) autoSummarize(ctx context.Context, userMessage string, tasks []*Task, results map[string]string) string {
	// If the planner already added a summarize_agent task, use its output.
	for _, t := range tasks {
		if t.ToolName == "summarize_agent" {
			if out, ok := results[t.ID]; ok && out != "" {
				return out
			}
		}
	}

	// Collect non-empty, non-debug outputs in plan order.
	var outputs []string
	for _, t := range tasks {
		if t.ToolName == "inspect_agent" || t.ToolName == "summarize_agent" {
			continue
		}
		if out, ok := results[t.ID]; ok && out != "" {
			outputs = append(outputs, out)
		}
	}

	if len(outputs) == 0 {
		raw, _ := json.Marshal(results)
		return string(raw)
	}
	if len(outputs) == 1 {
		return outputs[0]
	}

	// Multiple results: call the Go-native LLM summarizer.
	if o.summarizer != nil {
		if summary, err := o.summarizer.Summarize(ctx, userMessage, outputs); err == nil && summary != "" {
			return summary
		}
	}

	// Fallback: plain join.
	return strings.Join(outputs, "\n\n")
}

// -- Helpers -------------------------------------------------------------------

func validateDAG(d *DAG, agentSpecJSON string) error {
	if agentSpecJSON == "" {
		return nil
	}
	var spec struct {
		Tools     []string `json:"tools"`
		SubAgents []struct {
			Name string `json:"name"`
		} `json:"sub_agents"`
	}
	if err := json.Unmarshal([]byte(agentSpecJSON), &spec); err != nil {
		return fmt.Errorf("invalid agent_spec_json: %w", err)
	}
	if len(spec.Tools) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(spec.Tools)+len(spec.SubAgents))
	for _, t := range spec.Tools {
		allowed[t] = true
	}
	// Custom agents loaded from DB live in sub_agents, not tools.
	for _, sa := range spec.SubAgents {
		allowed[sa.Name] = true
	}
	for _, task := range d.Tasks {
		// Built-in agents from the static registry are always permitted —
		// they are system-level tools, not user-configured spec tools.
		if _, isBuiltin := AgentByName(task.ToolName); isBuiltin {
			continue
		}
		if !allowed[task.ToolName] {
			return fmt.Errorf("task %s uses tool %q which is not in agent spec", task.ID, task.ToolName)
		}
	}
	return nil
}

func calculateConfidence(eval EvalResult, gen int) (float64, string) {
	const refinementPenalty = 0.15
	score := math.Max(0, eval.Score-refinementPenalty)
	return score, fmt.Sprintf("Hit refinement cap (gen=%d); answer is best-effort", gen)
}

// -- gRPC adapter for Python ExecuteTask --------------------------------------

// ExecutorClientFromGRPC wraps the generated gRPC stub to satisfy PythonClient.
type ExecutorClientFromGRPC struct {
	stub langgraphv1.AgentServiceClient
}

// NewExecutorClient creates an ExecutorClientFromGRPC.
func NewExecutorClient(stub langgraphv1.AgentServiceClient) *ExecutorClientFromGRPC {
	return &ExecutorClientFromGRPC{stub: stub}
}

func (e *ExecutorClientFromGRPC) ExecuteTask(ctx context.Context, req *langgraphv1.TaskRequest) (langgraphv1.AgentService_ExecuteTaskClient, error) {
	return e.stub.ExecuteTask(ctx, req)
}

// -- Noop checkpoint -----------------------------------------------------------

// NoopCheckpoint is used until Postgres is configured.
type NoopCheckpoint struct{}

func (NoopCheckpoint) SaveTaskStart(ctx context.Context, sessionID string, t *Task) error { return nil }
func (NoopCheckpoint) SaveTaskDone(ctx context.Context, sessionID string, t *Task) error  { return nil }
func (NoopCheckpoint) SaveTaskFailed(ctx context.Context, sessionID string, t *Task) error {
	return nil
}
func (NoopCheckpoint) LoadCompletedTasks(_ context.Context, _ string, _ int) (map[string]string, error) {
	return map[string]string{}, nil
}
func (NoopCheckpoint) DeleteSession(_ context.Context, _ string) error { return nil }

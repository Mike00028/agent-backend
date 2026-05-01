package dag

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	langgraphv1 "github.com/mike00028/golang-backend/services/api/internal/langgraphv1/langgraph/v1"
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

// -- Orchestrator --------------------------------------------------------------

// BeforePlanFunc is called before every planning attempt.
// Returning a non-nil error aborts the invocation immediately.
// Use it for: cost estimation, PII checks on the prompt, budget guards.
type BeforePlanFunc func(ctx context.Context, req *GoPlanRequest) error

// Orchestrator drives the full DAG loop: plan -> validate -> execute -> eval -> refine.
type Orchestrator struct {
	planner    PlannerClient
	evaluator  EvaluatorClient
	executor   *Executor
	checkpoint CheckpointWriter
	beforePlan []BeforePlanFunc
}

// NewOrchestrator creates an Orchestrator.
func NewOrchestrator(planner PlannerClient, evaluator EvaluatorClient, executor *Executor, cp CheckpointWriter) *Orchestrator {
	return &Orchestrator{planner: planner, evaluator: evaluator, executor: executor, checkpoint: cp}
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
	planResult, err := o.planner.Plan(ctx, planReq)
	if err != nil {
		return nil, fmt.Errorf("planner failed (gen=%d): %w", gen, err)
	}

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
		if err := o.executor.RunBatch(ctx, d, pending, results); err != nil {
			return nil, fmt.Errorf("batch execution failed (gen=%d): %w", gen, err)
		}
	}

	// -- Step 7: Evaluate -------------------------------------------------------
	eval, err := o.evaluator.Eval(ctx, GoEvalRequest{
		SessionID:   req.SessionID,
		UserMessage: req.Message,
		Tasks:       d.Tasks,
	})
	if err != nil {
		eval = &EvalResult{EvalOK: false, Score: 0.5, Feedback: fmt.Sprintf("eval error: %s", err)}
	}

	// -- Step 8: Refine or return -----------------------------------------------
	if !eval.EvalOK {
		if gen < maxRefinementGeneration {
			return o.runGeneration(ctx, req, gen+1, eval.Feedback)
		}
		score, reason := calculateConfidence(*eval, gen)
		dagOutputJSON, _ := json.Marshal(results)
		return &RunResult{
			FinalOutput:      string(dagOutputJSON),
			ConfidenceScore:  score,
			ConfidenceReason: reason,
			EvalOK:           false,
		}, nil
	}

	dagOutputJSON, _ := json.Marshal(results)
	return &RunResult{
		FinalOutput:      string(dagOutputJSON),
		ConfidenceScore:  eval.Score,
		ConfidenceReason: eval.Summary,
		EvalOK:           true,
	}, nil
}

// -- Helpers -------------------------------------------------------------------

func validateDAG(d *DAG, agentSpecJSON string) error {
	if agentSpecJSON == "" {
		return nil
	}
	var spec struct {
		Tools []string `json:"tools"`
	}
	if err := json.Unmarshal([]byte(agentSpecJSON), &spec); err != nil {
		return fmt.Errorf("invalid agent_spec_json: %w", err)
	}
	if len(spec.Tools) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(spec.Tools))
	for _, t := range spec.Tools {
		allowed[t] = true
	}
	for _, task := range d.Tasks {
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

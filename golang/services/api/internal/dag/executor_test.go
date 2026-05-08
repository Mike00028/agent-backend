package dag

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ── null events channel helper ────────────────────────────────────────────────

func nullEvents() chan SSEEvent {
	ch := make(chan SSEEvent, 64)
	go func() {
		for range ch {
		}
	}()
	return ch
}

// ── mock planner ──────────────────────────────────────────────────────────────

type mockPlanner struct {
	tasks []*Task
	err   error
}

// Plan returns fresh copies of the task slice on each call so that status
// mutations from a previous generation don't bleed into the next.
func (m *mockPlanner) Plan(_ context.Context, _ GoPlanRequest) (*GoPlanResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	fresh := make([]*Task, len(m.tasks))
	for i, t := range m.tasks {
		cp := *t
		cp.Status = StatusPending
		cp.Output = ""
		cp.Error = ""
		fresh[i] = &cp
	}
	return &GoPlanResult{Tasks: fresh, Reasoning: "mock"}, nil
}

// ── mock evaluator ────────────────────────────────────────────────────────────

type mockEval struct {
	result *EvalResult
	err    error
}

func (m *mockEval) Eval(_ context.Context, _ GoEvalRequest) (*EvalResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.result, nil
}

// ── Executor.RunBatch tests ───────────────────────────────────────────────────

func newTestExecutor() *Executor {
	events := nullEvents()
	return NewExecutor(nil, NoopCheckpoint{}, events)
}

func TestRunBatch_LocalHandler_HappyPath(t *testing.T) {
	e := newTestExecutor()
	e.RegisterLocal("chat_agent", func(_ context.Context, task *Task) (string, error) {
		return "hello from local", nil
	})

	tasks := []*Task{
		{ID: "t1", ToolName: "chat_agent", Title: "ask"},
	}
	d := &DAG{ID: "d1", SessionID: "s1"}
	results := make(map[string]string)

	if err := e.RunBatch(context.Background(), d, tasks, results); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results["t1"] != "hello from local" {
		t.Errorf("results[t1] = %q, want %q", results["t1"], "hello from local")
	}
	if tasks[0].Status != StatusDone {
		t.Errorf("task status = %q, want %q", tasks[0].Status, StatusDone)
	}
}

func TestRunBatch_LocalHandler_Error(t *testing.T) {
	e := newTestExecutor()
	e.RegisterLocal("chat_agent", func(_ context.Context, _ *Task) (string, error) {
		return "", errors.New("local handler failed")
	})

	tasks := []*Task{{ID: "t1", ToolName: "chat_agent"}}
	d := &DAG{ID: "d1", SessionID: "s1"}
	results := make(map[string]string)

	err := e.RunBatch(context.Background(), d, tasks, results)
	if err == nil {
		t.Fatal("expected error from failing local handler")
	}
	if tasks[0].Status != StatusFailed {
		t.Errorf("task status = %q, want %q", tasks[0].Status, StatusFailed)
	}
	// Error should be stored in results so downstream tasks get a message
	if !strings.Contains(results["t1"], "[error]") {
		t.Errorf("expected [error] prefix in results[t1], got %q", results["t1"])
	}
}

func TestRunBatch_MultipleParallelTasks(t *testing.T) {
	e := newTestExecutor()
	e.RegisterLocal("chat_agent", func(_ context.Context, task *Task) (string, error) {
		return "output-" + task.ID, nil
	})

	tasks := []*Task{
		{ID: "t1", ToolName: "chat_agent"},
		{ID: "t2", ToolName: "chat_agent"},
		{ID: "t3", ToolName: "chat_agent"},
	}
	d := &DAG{SessionID: "s1"}
	results := make(map[string]string)

	if err := e.RunBatch(context.Background(), d, tasks, results); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, task := range tasks {
		want := "output-" + task.ID
		if results[task.ID] != want {
			t.Errorf("results[%s] = %q, want %q", task.ID, results[task.ID], want)
		}
	}
}

func TestRunBatch_DepContextInjected(t *testing.T) {
	e := newTestExecutor()
	var gotContext string
	e.RegisterLocal("chat_agent", func(_ context.Context, task *Task) (string, error) {
		gotContext = task.Context
		return "ok", nil
	})

	tasks := []*Task{
		{ID: "t2", ToolName: "chat_agent", DependsOn: []string{"t1"}},
	}
	d := &DAG{SessionID: "s1"}
	// t1's output was produced in a prior batch
	results := map[string]string{"t1": "prior output"}

	if err := e.RunBatch(context.Background(), d, tasks, results); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(gotContext, "prior output") {
		t.Errorf("dependency context not injected: %q", gotContext)
	}
}

func TestRunBatch_DepResultsInjected(t *testing.T) {
	e := newTestExecutor()
	var gotDepResults map[string]string
	e.RegisterLocal("math_agent", func(_ context.Context, task *Task) (string, error) {
		gotDepResults = task.DepResults
		return "42", nil
	})

	tasks := []*Task{
		{ID: "t2", ToolName: "math_agent", DependsOn: []string{"t1"}},
	}
	d := &DAG{SessionID: "s1"}
	results := map[string]string{"t1": "21"}

	if err := e.RunBatch(context.Background(), d, tasks, results); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotDepResults["t1"] != "21" {
		t.Errorf("dep result not injected: %v", gotDepResults)
	}
}

func TestRunBatch_ContextCancelled(t *testing.T) {
	e := newTestExecutor()
	e.RegisterLocal("chat_agent", func(ctx context.Context, _ *Task) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	tasks := []*Task{{ID: "t1", ToolName: "chat_agent"}}
	d := &DAG{SessionID: "s1"}
	results := make(map[string]string)

	err := e.RunBatch(ctx, d, tasks, results)
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
}

// ── Middleware tests ──────────────────────────────────────────────────────────

func TestMiddleware_BlocksTask(t *testing.T) {
	e := newTestExecutor()
	e.RegisterLocal("chat_agent", func(_ context.Context, _ *Task) (string, error) {
		return "should not reach here", nil
	})
	e.AddMiddleware(func(_ context.Context, task *Task) error {
		return fmt.Errorf("middleware blocked task %s", task.ID)
	})

	tasks := []*Task{{ID: "t1", ToolName: "chat_agent"}}
	d := &DAG{SessionID: "s1"}
	results := make(map[string]string)

	if err := e.RunBatch(context.Background(), d, tasks, results); err == nil {
		t.Fatal("expected error when middleware blocks")
	}
	if tasks[0].Status != StatusFailed {
		t.Errorf("task status = %q, want %q", tasks[0].Status, StatusFailed)
	}
}

func TestMiddleware_AllowsTask(t *testing.T) {
	e := newTestExecutor()
	e.RegisterLocal("chat_agent", func(_ context.Context, _ *Task) (string, error) {
		return "allowed", nil
	})
	e.AddMiddleware(func(_ context.Context, _ *Task) error {
		return nil // allow
	})

	tasks := []*Task{{ID: "t1", ToolName: "chat_agent"}}
	d := &DAG{SessionID: "s1"}
	results := make(map[string]string)

	if err := e.RunBatch(context.Background(), d, tasks, results); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results["t1"] != "allowed" {
		t.Errorf("results[t1] = %q, want 'allowed'", results["t1"])
	}
}

// ── validateDAG tests ─────────────────────────────────────────────────────────

func TestValidateDAG_EmptySpec(t *testing.T) {
	d := &DAG{Tasks: []*Task{{ID: "t1", ToolName: "any_tool"}}}
	if err := validateDAG(d, ""); err != nil {
		t.Errorf("expected nil for empty spec, got: %v", err)
	}
}

func TestValidateDAG_AllowsBuiltins(t *testing.T) {
	spec := `{"tools":["some_other_tool"]}`
	d := &DAG{Tasks: []*Task{
		{ID: "t1", ToolName: "chat_agent"},  // builtin — always allowed
		{ID: "t2", ToolName: "math_agent"},  // builtin — always allowed
	}}
	if err := validateDAG(d, spec); err != nil {
		t.Errorf("builtins should be allowed, got: %v", err)
	}
}

func TestValidateDAG_BlocksUnknownTool(t *testing.T) {
	spec := `{"tools":["chat_agent"]}`
	d := &DAG{Tasks: []*Task{
		{ID: "t1", ToolName: "evil_tool"},
	}}
	if err := validateDAG(d, spec); err == nil {
		t.Error("expected error for tool not in spec")
	}
}

func TestValidateDAG_AllowsSubAgents(t *testing.T) {
	// tools is non-empty so the validation loop actually runs.
	// sub_agents entries are added to the allowed set alongside spec.Tools.
	spec := `{"tools":["chat_agent"],"sub_agents":[{"name":"my_custom_agent"}]}`
	d := &DAG{Tasks: []*Task{
		{ID: "t1", ToolName: "my_custom_agent"},
	}}
	if err := validateDAG(d, spec); err != nil {
		t.Errorf("sub_agents should be allowed, got: %v", err)
	}
}

func TestValidateDAG_InvalidJSON(t *testing.T) {
	d := &DAG{Tasks: []*Task{{ID: "t1", ToolName: "x"}}}
	if err := validateDAG(d, `{bad json}`); err == nil {
		t.Error("expected error for invalid JSON spec")
	}
}

// ── autoSummarize tests ───────────────────────────────────────────────────────

func TestAutoSummarize_UsesSummarizeAgentOutput(t *testing.T) {
	o := NewOrchestrator(nil, nil, nil, NoopCheckpoint{})
	tasks := []*Task{
		{ID: "t1", ToolName: "chat_agent"},
		{ID: "t2", ToolName: "summarize_agent"},
	}
	results := map[string]string{"t1": "raw answer", "t2": "synthesized answer"}

	got := o.autoSummarize(context.Background(), "q", tasks, results)
	if got != "synthesized answer" {
		t.Errorf("autoSummarize should prefer summarize_agent output, got %q", got)
	}
}

func TestAutoSummarize_SingleResult(t *testing.T) {
	o := NewOrchestrator(nil, nil, nil, NoopCheckpoint{})
	tasks := []*Task{{ID: "t1", ToolName: "chat_agent"}}
	results := map[string]string{"t1": "only answer"}

	got := o.autoSummarize(context.Background(), "q", tasks, results)
	if got != "only answer" {
		t.Errorf("autoSummarize single result = %q, want %q", got, "only answer")
	}
}

func TestAutoSummarize_MultipleResults_NoSummarizer(t *testing.T) {
	o := NewOrchestrator(nil, nil, nil, NoopCheckpoint{})
	// No summarizer set — should fall back to newline join
	tasks := []*Task{
		{ID: "t1", ToolName: "chat_agent"},
		{ID: "t2", ToolName: "chat_agent"},
	}
	results := map[string]string{"t1": "answer1", "t2": "answer2"}

	got := o.autoSummarize(context.Background(), "q", tasks, results)
	if !strings.Contains(got, "answer1") || !strings.Contains(got, "answer2") {
		t.Errorf("autoSummarize fallback should join answers, got %q", got)
	}
}

func TestAutoSummarize_EmptyResults(t *testing.T) {
	o := NewOrchestrator(nil, nil, nil, NoopCheckpoint{})
	tasks := []*Task{{ID: "t1", ToolName: "chat_agent"}}
	results := map[string]string{} // no outputs

	got := o.autoSummarize(context.Background(), "q", tasks, results)
	// Should return JSON of results map (empty in this case = "{}")
	if got == "" {
		t.Error("autoSummarize should not return empty string for empty results")
	}
}

// ── calculateConfidence tests ─────────────────────────────────────────────────

func TestCalculateConfidence(t *testing.T) {
	cases := []struct {
		score     float64
		gen       int
		wantScore float64
	}{
		{0.8, 1, 0.65},  // 0.8 - 0.15 penalty
		{0.5, 2, 0.35},  // 0.5 - 0.15
		{0.1, 1, 0.0},   // floor at 0
	}
	for _, tc := range cases {
		eval := EvalResult{Score: tc.score}
		got, reason := calculateConfidence(eval, tc.gen)
		if got < 0 {
			t.Errorf("confidence score must be >= 0, got %f", got)
		}
		if tc.wantScore > 0 && abs(got-tc.wantScore) > 0.001 {
			t.Errorf("calculateConfidence(score=%.2f, gen=%d) = %.4f, want %.4f", tc.score, tc.gen, got, tc.wantScore)
		}
		if !strings.Contains(reason, fmt.Sprintf("gen=%d", tc.gen)) {
			t.Errorf("reason should mention gen, got %q", reason)
		}
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// ── Orchestrator.Run full loop tests ─────────────────────────────────────────

func newOrchForTest(tasks []*Task, evalOK bool, evalScore float64) *Orchestrator {
	planner := &mockPlanner{tasks: tasks}
	eval := &mockEval{result: &EvalResult{EvalOK: evalOK, Score: evalScore, Summary: "ok"}}
	events := nullEvents()
	executor := NewExecutor(nil, NoopCheckpoint{}, events)
	o := NewOrchestrator(planner, eval, executor, NoopCheckpoint{})
	o.SetEvents(events)
	return o
}

func TestOrchestrator_Run_SingleLocalTask(t *testing.T) {
	tasks := []*Task{{ID: "t1", ToolName: "chat_agent", Title: "ask"}}
	o := newOrchForTest(tasks, true, 0.9)

	// Register local handler on the executor — reach it via the orchestrator
	o.executor.RegisterLocal("chat_agent", func(_ context.Context, _ *Task) (string, error) {
		return "the answer", nil
	})

	result, err := o.Run(context.Background(), RunRequest{
		SessionID: "s1", UserID: "u1", AgentID: "default", Message: "hello",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.EvalOK {
		t.Error("expected EvalOK=true")
	}
	if result.FinalOutput != "the answer" {
		t.Errorf("FinalOutput = %q, want %q", result.FinalOutput, "the answer")
	}
}

func TestOrchestrator_Run_PlannerFails(t *testing.T) {
	events := nullEvents()
	planner := &mockPlanner{err: errors.New("LLM unavailable")}
	eval := &mockEval{result: &EvalResult{EvalOK: true, Score: 0.9}}
	executor := NewExecutor(nil, NoopCheckpoint{}, events)
	o := NewOrchestrator(planner, eval, executor, NoopCheckpoint{})
	o.SetEvents(events)

	_, err := o.Run(context.Background(), RunRequest{SessionID: "s1", Message: "hi"})
	if err == nil {
		t.Fatal("expected error when planner fails")
	}
	if !strings.Contains(err.Error(), "planner failed") {
		t.Errorf("expected 'planner failed' in error, got %q", err.Error())
	}
}

func TestOrchestrator_Run_EvalFailTriggersRefine(t *testing.T) {
	callCount := 0
	tasks := []*Task{{ID: "t1", ToolName: "chat_agent", Title: "q"}}

	// Planner tracks how many times it's called
	planner := &mockPlanner{tasks: tasks}
	origPlan := planner
	_ = origPlan

	// Evaluator: fail first call, pass second
	evalCallCount := 0
	customEval := &countingEval{
		results: []*EvalResult{
			{EvalOK: false, Score: 0.2, Feedback: "not good enough"},
			{EvalOK: true, Score: 0.9, Summary: "great"},
		},
	}

	events := nullEvents()
	executor := NewExecutor(nil, NoopCheckpoint{}, events)
	executor.RegisterLocal("chat_agent", func(_ context.Context, _ *Task) (string, error) {
		callCount++
		return fmt.Sprintf("answer attempt %d", callCount), nil
	})
	o := NewOrchestrator(planner, customEval, executor, NoopCheckpoint{})
	o.SetEvents(events)

	result, err := o.Run(context.Background(), RunRequest{SessionID: "s1", Message: "hi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.EvalOK {
		t.Error("expected EvalOK=true on second generation")
	}
	if evalCallCount > 2 {
		_ = evalCallCount // referenced via customEval
	}
	if customEval.calls < 2 {
		t.Errorf("expected at least 2 eval calls (1 fail + 1 pass), got %d", customEval.calls)
	}
}

func TestOrchestrator_Run_MaxRefinementReached(t *testing.T) {
	tasks := []*Task{{ID: "t1", ToolName: "chat_agent", Title: "q"}}
	// Always fail eval with low score
	alwaysFail := &countingEval{
		results: []*EvalResult{
			{EvalOK: false, Score: 0.1, Feedback: "bad"},
			{EvalOK: false, Score: 0.1, Feedback: "still bad"},
			{EvalOK: false, Score: 0.1, Feedback: "still bad gen3"},
		},
	}

	events := nullEvents()
	executor := NewExecutor(nil, NoopCheckpoint{}, events)
	executor.RegisterLocal("chat_agent", func(_ context.Context, _ *Task) (string, error) {
		return "best effort", nil
	})
	o := NewOrchestrator(&mockPlanner{tasks: tasks}, alwaysFail, executor, NoopCheckpoint{})
	o.SetEvents(events)

	result, err := o.Run(context.Background(), RunRequest{SessionID: "s1", Message: "hi"})
	if err != nil {
		t.Fatalf("unexpected error even at max refinement: %v", err)
	}
	if result.EvalOK {
		t.Error("expected EvalOK=false when max refinement is exhausted")
	}
	// Must stop at maxRefinementGeneration — not loop forever
	if alwaysFail.calls > maxRefinementGeneration+1 {
		t.Errorf("orchestrator called eval %d times, expected <= %d", alwaysFail.calls, maxRefinementGeneration+1)
	}
}

func TestOrchestrator_Run_DAGValidationFails(t *testing.T) {
	tasks := []*Task{{ID: "t1", ToolName: "banned_tool"}}
	events := nullEvents()
	executor := NewExecutor(nil, NoopCheckpoint{}, events)
	o := NewOrchestrator(&mockPlanner{tasks: tasks}, &mockEval{}, executor, NoopCheckpoint{})
	o.SetEvents(events)

	// Spec only allows chat_agent — banned_tool should fail validation
	_, err := o.Run(context.Background(), RunRequest{
		SessionID:     "s1",
		Message:       "hi",
		AgentSpecJSON: `{"tools":["chat_agent"]}`,
	})
	if err == nil {
		t.Fatal("expected DAG validation error")
	}
	if !strings.Contains(err.Error(), "validation failed") {
		t.Errorf("expected 'validation failed' in error, got %q", err.Error())
	}
}

func TestOrchestrator_BeforePlanHook_Blocks(t *testing.T) {
	tasks := []*Task{{ID: "t1", ToolName: "chat_agent"}}
	events := nullEvents()
	executor := NewExecutor(nil, NoopCheckpoint{}, events)
	o := NewOrchestrator(&mockPlanner{tasks: tasks}, &mockEval{}, executor, NoopCheckpoint{})
	o.SetEvents(events)
	o.AddBeforePlan(func(_ context.Context, _ *GoPlanRequest) error {
		return errors.New("budget exceeded")
	})

	_, err := o.Run(context.Background(), RunRequest{SessionID: "s1", Message: "hi"})
	if err == nil {
		t.Fatal("expected error from before-plan hook")
	}
	if !strings.Contains(err.Error(), "budget exceeded") {
		t.Errorf("expected 'budget exceeded' in error, got %q", err.Error())
	}
}

// ── counting evaluator helper ─────────────────────────────────────────────────

type countingEval struct {
	results []*EvalResult
	calls   int
}

func (c *countingEval) Eval(_ context.Context, _ GoEvalRequest) (*EvalResult, error) {
	idx := c.calls
	c.calls++
	if idx < len(c.results) {
		return c.results[idx], nil
	}
	// If we run out of pre-configured results, return a pass
	return &EvalResult{EvalOK: true, Score: 0.9, Summary: "default pass"}, nil
}

package dag

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	langgraphv1 "github.com/mike00028/golang-backend/services/api/internal/langgraphv1/langgraph/v1"
	"github.com/mike00028/golang-backend/services/api/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc/metadata"
)

const (
	maxTaskRetries       = 3
	taskRetryBaseMs      = 1000 // exponential backoff: 1s, 2s, 4s
	localTaskTimeoutSec  = 180  // Go/Ollama handlers: LLM can be slow locally
	remoteTaskTimeoutSec = 180  // Python gRPC handlers: ReAct loops need multiple LLM round-trips
)

// PythonClient is a subset of the gRPC client used by the executor.
type PythonClient interface {
	ExecuteTask(ctx context.Context, req *langgraphv1.TaskRequest) (langgraphv1.AgentService_ExecuteTaskClient, error)
}

// LocalTaskFunc handles a task entirely in Go without a Python gRPC call.
// Return the string result or an error.
type LocalTaskFunc func(ctx context.Context, task *Task) (string, error)

// TaskMiddleware is called before each task execution attempt (including retries).
// Returning a non-nil error skips this attempt and counts as a task failure.
// Use it for: per-tool rate limiting, approval checks, PII scrubbing of args.
type TaskMiddleware func(ctx context.Context, task *Task) error

// Executor runs a single batch of tasks in parallel and collects their results.
type Executor struct {
	client        PythonClient
	checkpoint    CheckpointWriter
	events        chan<- SSEEvent
	middleware    []TaskMiddleware
	localHandlers map[string]LocalTaskFunc
	tracer        telemetry.Tracer
}

// NewExecutor creates an Executor.
func NewExecutor(client PythonClient, cp CheckpointWriter, events chan<- SSEEvent) *Executor {
	return &Executor{client: client, checkpoint: cp, events: events, tracer: telemetry.NewTracer("dag.executor")}
}

// AddMiddleware registers a hook that runs before each task execution attempt.
func (e *Executor) AddMiddleware(fn TaskMiddleware) {
	e.middleware = append(e.middleware, fn)
}

// RegisterLocal registers a Go-native handler for a specific tool_name.
// Every handler is automatically wrapped with a generic tracing decorator that
// records decoded input, output, and errors on a child span — individual
// handlers stay completely free of tracing concerns.
func (e *Executor) RegisterLocal(toolName string, fn LocalTaskFunc) {
	if e.localHandlers == nil {
		e.localHandlers = make(map[string]LocalTaskFunc)
	}
	e.localHandlers[toolName] = e.localTraceDecorator(toolName, fn)
}

// localTraceDecorator wraps a LocalTaskFunc with a child span that captures:
// decoded input (question / expr), raw output, byte count, and any error.
// Applied once at registration — zero per-handler boilerplate required.
func (e *Executor) localTraceDecorator(toolName string, fn LocalTaskFunc) LocalTaskFunc {
	return func(ctx context.Context, task *Task) (string, error) {
		ctx, span := e.tracer.Start(ctx, "agent.invoke."+toolName,
			telemetry.StringAttr("task.id", task.ID),
			telemetry.StringAttr("task.tool", toolName),
			telemetry.StringAttr("task.title", task.Title),
			telemetry.StringAttr("langfuse.observation.input", taskInputForTrace(task)),
		)
		defer span.End()

		output, err := fn(ctx, task)
		if err != nil {
			span.RecordError(err)
			span.SetError(err.Error())
			return "", err
		}
		span.SetAttr(
			telemetry.StringAttr("langfuse.observation.output", output),
			telemetry.IntAttr("output.bytes", len(output)),
		)
		return output, nil
	}
}

// RunBatch executes all tasks in the batch concurrently.
// It blocks until every task finishes (done or failed).
// Fail-fast: if any task fails after retries, all siblings are cancelled.
//
// Data-race safety: dep-context is built here, before goroutines are launched,
// so the results map (which holds prior-batch outputs) is only read sequentially.
// Within the batch, writes to results are protected by mu.
func (e *Executor) RunBatch(ctx context.Context, dag *DAG, batch []*Task, results map[string]string) error {
	// Pre-build dependency context for each task from prior-batch results.
	// Must happen before goroutines launch; no concurrent writes are in flight yet.
	for _, t := range batch {
		var depCtx strings.Builder
		// Only inject prior dependency outputs — each agent receives its own
		// scoped sub-query via args.question set by the planner.
		// Injecting the full user message here would re-expose every agent to
		// unrelated parts of a multi-task query (violates single responsibility).
		for _, depID := range t.DependsOn {
			if out, ok := results[depID]; ok {
				depCtx.WriteString(fmt.Sprintf("[%s result]: %s\n", depID, out))
			}
		}
		t.Context = depCtx.String()
		// Also store raw dep outputs for Go-local agents (e.g. math_agent)
		// so they don't need to parse the text format.
		if len(t.DependsOn) > 0 {
			deps := make(map[string]string, len(t.DependsOn))
			for _, depID := range t.DependsOn {
				if out, ok := results[depID]; ok {
					deps[depID] = out
				}
			}
			t.DepResults = deps
		}
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed []string
	)

	for _, t := range batch {
		wg.Add(1)
		go func(task *Task) {
			defer wg.Done()

			err := e.runTaskWithRetry(ctx, dag, task)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", task.ID, err))
				// Store the error as the task output so downstream tasks that
				// depend on this one receive an error string rather than nothing.
				results[task.ID] = fmt.Sprintf("[error] %v", err)
			} else {
				results[task.ID] = task.Output
			}
		}(t)
	}

	wg.Wait()
	if len(failed) > 0 {
		return fmt.Errorf("tasks failed: %s", strings.Join(failed, "; "))
	}
	return nil
}

// runTaskWithRetry executes one task, retrying up to maxTaskRetries on transient failures.
// task.Context must already be populated by RunBatch before this is called.
func (e *Executor) runTaskWithRetry(ctx context.Context, dag *DAG, task *Task) error {
	var lastErr error
	for attempt := 0; attempt <= maxTaskRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(taskRetryBaseMs*(1<<(attempt-1))) * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		task.Status = StatusRunning
		task.StartedAt = time.Now()
		task.RetryCount = attempt

		// Run middleware hooks before each attempt.
		for _, mw := range e.middleware {
			if err := mw(ctx, task); err != nil {
				task.Error = err.Error()
				lastErr = err
				break // treat middleware rejection same as task failure
			}
		}
		if lastErr != nil && task.Error != "" {
			continue
		}

		taskCtx, taskSpan := e.tracer.Start(ctx, "dag.task."+task.ToolName,
			telemetry.StringAttr("task.id", task.ID),
			telemetry.StringAttr("task.tool", task.ToolName),
			telemetry.StringAttr("task.title", task.Title),
			telemetry.StringAttr("langfuse.session.id", dag.SessionID),
			telemetry.IntAttr("task.attempt", attempt),
		)
		e.emit(SSEEvent{Type: "task_started", TaskID: task.ID, Payload: task.ToolName})
		_ = e.checkpoint.SaveTaskStart(ctx, dag.SessionID, task)

		err := e.streamTask(taskCtx, dag, task)
		if err == nil {
			task.Status = StatusDone
			task.DoneAt = time.Now()
			// input/output/error details are recorded by the per-handler decorator
			// (local) or by the Python servicer (remote). The lifecycle span only
			// needs the final output for the Langfuse task observation.
			taskSpan.SetAttr(
				telemetry.StringAttr("langfuse.observation.input", taskInputForTrace(task)),
				telemetry.StringAttr("langfuse.observation.output", task.Output),
			)
			taskSpan.End()
			_ = e.checkpoint.SaveTaskDone(ctx, dag.SessionID, task)
			e.emit(SSEEvent{Type: "task_done", TaskID: task.ID, Payload: task.Output})
			return nil
		}

		task.Error = err.Error()
		taskSpan.RecordError(err)
		taskSpan.SetError(err.Error())
		taskSpan.End()
		lastErr = err
	}

	task.Status = StatusFailed
	task.DoneAt = time.Now()
	_ = e.checkpoint.SaveTaskFailed(ctx, dag.SessionID, task)
	e.emit(SSEEvent{Type: "task_failed", TaskID: task.ID, Payload: task.Error})
	return lastErr
}

// streamTask dispatches a task either to a Go-native local handler or to
// Python's ExecuteTask RPC, depending on whether a local handler is registered.
func (e *Executor) streamTask(ctx context.Context, dag *DAG, task *Task) error {
	// Local handlers run entirely in Go — zero Python round-trip.
	if fn, ok := e.localHandlers[task.ToolName]; ok {
		taskCtx, cancel := context.WithTimeout(ctx, localTaskTimeoutSec*time.Second)
		defer cancel()
		result, err := fn(taskCtx, task)
		if err != nil {
			return err
		}
		task.Output = result
		e.emit(SSEEvent{Type: "task_progress", TaskID: task.ID, Payload: result})
		return nil
	}

	taskCtx, cancel := context.WithTimeout(ctx, remoteTaskTimeoutSec*time.Second)
	defer cancel()

	// Safety net: if the planner failed to populate args.question, inject the
	// full user message so the agent always has something concrete to work from.
	// Which agents need this is declared in AgentRegistry (NeedsQuestion field)
	// — no hardcoded agent-name checks here.
	argsJSON := task.ArgsJSON
	if def, ok := AgentByName(task.ToolName); ok && def.NeedsQuestion && dag.UserMessage != "" {
		var a map[string]interface{}
		if err := json.Unmarshal([]byte(argsJSON), &a); err == nil {
			q, _ := a["question"].(string)
			if q == "" {
				a["question"] = dag.UserMessage
				if b, err := json.Marshal(a); err == nil {
					argsJSON = string(b)
				}
			}
		}
	}

	// Propagate the current OTel trace context into gRPC metadata so that
	// Python-side spans become children of this Go span in Langfuse.
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(taskCtx, carrier)
	if len(carrier) > 0 {
		md := metadata.New(map[string]string(carrier))
		taskCtx = metadata.NewOutgoingContext(taskCtx, md)
	}

	stream, err := e.client.ExecuteTask(taskCtx, &langgraphv1.TaskRequest{
		SessionId: dag.SessionID,
		TaskId:    task.ID,
		ToolName:  task.ToolName,
		ArgsJson:  argsJSON,
		Context:   task.Context,
		AgentId:   dag.AgentID,
	})
	if err != nil {
		return fmt.Errorf("ExecuteTask RPC failed: %w", err)
	}

	var outputParts []string
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("stream error: %w", err)
		}

		switch ev.Type {
		case "error":
			return fmt.Errorf("tool error: %s", ev.Error)
		case "progress":
			e.emit(SSEEvent{Type: "task_progress", TaskID: task.ID, Pct: int(ev.Pct)})
		case "text":
			outputParts = append(outputParts, ev.Payload)
			e.emit(SSEEvent{Type: "task_progress", TaskID: task.ID, Payload: ev.Payload})
		case "done":
			// Only use done.payload if no text events were received yet;
			// prevents double-encoding when Python sends text then done with same payload.
			if len(outputParts) == 0 && ev.Payload != "" {
				outputParts = append(outputParts, ev.Payload)
			}
		}
	}

	// Merge all output parts into final JSON result
	if len(outputParts) == 1 {
		task.Output = outputParts[0]
	} else {
		combined, _ := json.Marshal(outputParts)
		task.Output = string(combined)
	}
	return nil
}

func (e *Executor) emit(ev SSEEvent) {
	select {
	case e.events <- ev:
	default: // non-blocking; drop if consumer is slow
	}
}

// taskInputForTrace builds a human-readable input string for the Langfuse
// dag.task span. It extracts args.question / args.expr from the raw JSON
// so the trace shows the actual question the agent received, not raw JSON.
func taskInputForTrace(task *Task) string {
	var args struct {
		Question string `json:"question"`
		Expr     string `json:"expr"`
	}
	if err := json.Unmarshal([]byte(task.ArgsJSON), &args); err != nil {
		return task.ArgsJSON // fallback to raw JSON if parsing fails
	}
	if args.Question != "" {
		if task.Context != "" {
			return task.Context + "\n" + args.Question
		}
		return args.Question
	}
	if args.Expr != "" {
		return "expr: " + args.Expr
	}
	return task.ArgsJSON
}

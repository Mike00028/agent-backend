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
)

const (
	maxTaskRetries  = 3
	taskRetryBaseMs = 1000 // exponential backoff: 1s, 2s, 4s
	taskTimeoutSec  = 30
)

// PythonClient is a subset of the gRPC client used by the executor.
type PythonClient interface {
	ExecuteTask(ctx context.Context, req *langgraphv1.TaskRequest) (langgraphv1.AgentService_ExecuteTaskClient, error)
}

// TaskMiddleware is called before each task execution attempt (including retries).
// Returning a non-nil error skips this attempt and counts as a task failure.
// Use it for: per-tool rate limiting, approval checks, PII scrubbing of args.
type TaskMiddleware func(ctx context.Context, task *Task) error

// Executor runs a single batch of tasks in parallel and collects their results.
type Executor struct {
	client     PythonClient
	checkpoint CheckpointWriter
	events     chan<- SSEEvent  // forwarded to SSE stream
	middleware []TaskMiddleware // runs before each task attempt
}

// NewExecutor creates an Executor.
func NewExecutor(client PythonClient, cp CheckpointWriter, events chan<- SSEEvent) *Executor {
	return &Executor{client: client, checkpoint: cp, events: events}
}

// AddMiddleware registers a hook that runs before each task execution attempt.
func (e *Executor) AddMiddleware(fn TaskMiddleware) {
	e.middleware = append(e.middleware, fn)
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
		for _, depID := range t.DependsOn {
			if out, ok := results[depID]; ok {
				depCtx.WriteString(fmt.Sprintf("[%s result]: %s\n", depID, out))
			}
		}
		t.Context = depCtx.String()
	}

	batchCtx, cancelBatch := context.WithCancel(ctx)
	defer cancelBatch()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)

	for _, t := range batch {
		wg.Add(1)
		go func(task *Task) {
			defer wg.Done()

			err := e.runTaskWithRetry(batchCtx, dag, task, results)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("task %s failed: %w", task.ID, err)
					cancelBatch() // fail-fast: cancel siblings
				}
				mu.Unlock()
			} else {
				mu.Lock()
				results[task.ID] = task.Output
				mu.Unlock()
			}
		}(t)
	}

	wg.Wait()
	return firstErr
}

// runTaskWithRetry executes one task, retrying up to maxTaskRetries on transient failures.
// task.Context must already be populated by RunBatch before this is called.
func (e *Executor) runTaskWithRetry(ctx context.Context, dag *DAG, task *Task, results map[string]string) error {
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

		e.emit(SSEEvent{Type: "task_started", TaskID: task.ID, Payload: task.ToolName})
		_ = e.checkpoint.SaveTaskStart(ctx, dag.SessionID, task)

		err := e.streamTask(ctx, dag, task)
		if err == nil {
			task.Status = StatusDone
			task.DoneAt = time.Now()
			_ = e.checkpoint.SaveTaskDone(ctx, dag.SessionID, task)
			e.emit(SSEEvent{Type: "task_done", TaskID: task.ID, Payload: task.Output})
			return nil
		}

		task.Error = err.Error()
		lastErr = err
	}

	task.Status = StatusFailed
	task.DoneAt = time.Now()
	_ = e.checkpoint.SaveTaskFailed(ctx, dag.SessionID, task)
	e.emit(SSEEvent{Type: "task_failed", TaskID: task.ID, Payload: task.Error})
	return lastErr
}

// streamTask calls Python's ExecuteTask RPC and drains the event stream.
func (e *Executor) streamTask(ctx context.Context, dag *DAG, task *Task) error {
	taskCtx, cancel := context.WithTimeout(ctx, taskTimeoutSec*time.Second)
	defer cancel()

	stream, err := e.client.ExecuteTask(taskCtx, &langgraphv1.TaskRequest{
		SessionId: dag.SessionID,
		TaskId:    task.ID,
		ToolName:  task.ToolName,
		ArgsJson:  task.ArgsJSON,
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
			outputParts = append(outputParts, ev.Payload)
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

package dag

import (
	"time"
)

// TaskStatus represents the execution state of a single DAG task.
type TaskStatus string

const (
	StatusPending   TaskStatus = "pending"
	StatusRunning   TaskStatus = "running"
	StatusDone      TaskStatus = "done"
	StatusFailed    TaskStatus = "failed"
	StatusCancelled TaskStatus = "cancelled"
)

// Task is a single node in the DAG.
type Task struct {
	ID        string   // e.g. "t1", "t2"
	ToolName  string   // MCP tool to invoke
	ArgsJSON  string   // JSON arguments for the tool
	DependsOn []string // IDs of tasks that must complete first
	Context   string   // Dependency outputs injected at dispatch time

	// Execution state (mutated by executor)
	Status     TaskStatus
	Output     string // JSON result from Python tool
	Error      string // Error description if failed
	RetryCount int
	StartedAt  time.Time
	DoneAt     time.Time
}

// DAG holds a complete execution plan.
type DAG struct {
	ID            string
	SessionID     string
	UserID        string
	AgentID       string
	UserMessage   string
	AgentSpecJSON string
	Tasks         []*Task
	Reasoning     string
	Generation    int    // 0=original, 1=first refine, 2=second refine
	PreviousDAGID string // points to parent DAG if refined
}

// EvalResult holds the output of the Go evaluator.
type EvalResult struct {
	EvalOK   bool
	Score    float64
	Feedback string
	Summary  string // one-sentence user-facing summary
	Error    string
}

// SSEEvent is forwarded to the browser via the SSE writer.
type SSEEvent struct {
	Type    string // "task_started" | "task_progress" | "task_done" | "task_failed" | "dag_done" | "error"
	TaskID  string
	Payload string // JSON payload
	Pct     int    // progress 0-100
}

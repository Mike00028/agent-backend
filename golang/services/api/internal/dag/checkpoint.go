package dag

import (
	"context"
	"encoding/json"
)

// CheckpointWriter persists task execution state to durable storage.
// The Postgres implementation lives here; NoopCheckpoint is in orchestrator.go.
type CheckpointWriter interface {
	SaveTaskStart(ctx context.Context, sessionID string, t *Task) error
	SaveTaskDone(ctx context.Context, sessionID string, t *Task) error
	SaveTaskFailed(ctx context.Context, sessionID string, t *Task) error

	// LoadCompletedTasks returns all tasks that reached 'done' status for the
	// given session and refinement generation. Used to resume a crashed DAG:
	// the orchestrator pre-populates the results map and skips those tasks.
	// Returns an empty map (not an error) when no prior progress exists.
	LoadCompletedTasks(ctx context.Context, sessionID string, gen int) (map[string]string, error)
}

// PgCheckpoint writes checkpoints to Postgres via pgx.
// The DB field accepts any interface with Exec+Query — use *pgxpool.Pool in production.
type PgCheckpoint struct {
	DB PgxDB
}

// PgxDB is the minimal pgx interface needed by PgCheckpoint.
type PgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (interface{ RowsAffected() int64 }, error)
	Query(ctx context.Context, sql string, args ...any) (PgxRows, error)
}

// PgxRows is the minimal pgx row-iterator interface.
type PgxRows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
	Err() error
}

func NewPgCheckpoint(db PgxDB) *PgCheckpoint {
	return &PgCheckpoint{DB: db}
}

func (p *PgCheckpoint) SaveTaskStart(ctx context.Context, sessionID string, t *Task) error {
	argsJSON, _ := json.Marshal(t.ArgsJSON)
	_, err := p.DB.Exec(ctx, `
		INSERT INTO agent_task_nodes
			(task_id, node_id, status, input_args, retry_count, started_at, refinement_generation)
		VALUES ($1, $2, 'running', $3, $4, now(), $5)
		ON CONFLICT (task_id, node_id) DO UPDATE
			SET status = 'running', started_at = now(), retry_count = EXCLUDED.retry_count`,
		sessionID, t.ID, argsJSON, t.RetryCount, 0,
	)
	return err
}

func (p *PgCheckpoint) SaveTaskDone(ctx context.Context, sessionID string, t *Task) error {
	outputJSON, _ := json.RawMessage(t.Output), error(nil)
	durationMs := int(t.DoneAt.Sub(t.StartedAt).Milliseconds())
	_, err := p.DB.Exec(ctx, `
		UPDATE agent_task_nodes
		SET status = 'done', output = $3, completed_at = now(), duration_ms = $4
		WHERE task_id = $1 AND node_id = $2`,
		sessionID, t.ID, outputJSON, durationMs,
	)
	return err
}

func (p *PgCheckpoint) SaveTaskFailed(ctx context.Context, sessionID string, t *Task) error {
	durationMs := int(t.DoneAt.Sub(t.StartedAt).Milliseconds())
	_, err := p.DB.Exec(ctx, `
		UPDATE agent_task_nodes
		SET status = 'failed', last_error = $3, completed_at = now(), duration_ms = $4,
		    retry_count = $5
		WHERE task_id = $1 AND node_id = $2`,
		sessionID, t.ID, t.Error, durationMs, t.RetryCount,
	)
	return err
}

// LoadCompletedTasks returns all done tasks for a given session + refinement generation.
// The orchestrator uses this to skip already-finished tasks when resuming a crashed DAG.
func (p *PgCheckpoint) LoadCompletedTasks(ctx context.Context, sessionID string, gen int) (map[string]string, error) {
	rows, err := p.DB.Query(ctx, `
		SELECT node_id, output::text
		FROM agent_task_nodes
		WHERE task_id = $1 AND refinement_generation = $2 AND status = 'done'`,
		sessionID, gen,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var nodeID, output string
		if err := rows.Scan(&nodeID, &output); err != nil {
			return nil, err
		}
		out[nodeID] = output
	}
	return out, rows.Err()
}

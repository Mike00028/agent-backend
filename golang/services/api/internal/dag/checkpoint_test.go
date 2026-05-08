package dag

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ── mock PgxDB ────────────────────────────────────────────────────────────────

type mockTag struct{ n int64 }

func (m mockTag) RowsAffected() int64 { return m.n }

type cpRows struct {
	data [][2]string // [nodeID, output] pairs
	idx  int
	err  error
}

func (r *cpRows) Next() bool { r.idx++; return r.idx <= len(r.data) }
func (r *cpRows) Close()     {}
func (r *cpRows) Err() error { return r.err }
func (r *cpRows) Scan(dst ...any) error {
	*dst[0].(*string) = r.data[r.idx-1][0]
	*dst[1].(*string) = r.data[r.idx-1][1]
	return nil
}

type cpDB struct {
	execErr  error
	queryErr error
	rows     *cpRows
}

func (d *cpDB) Exec(_ context.Context, _ string, _ ...any) (interface{ RowsAffected() int64 }, error) {
	return mockTag{1}, d.execErr
}
func (d *cpDB) Query(_ context.Context, _ string, _ ...any) (PgxRows, error) {
	if d.queryErr != nil {
		return nil, d.queryErr
	}
	return d.rows, nil
}

// ── NoopCheckpoint tests ──────────────────────────────────────────────────────

func TestNoopCheckpoint_AllNil(t *testing.T) {
	n := NoopCheckpoint{}
	ctx := context.Background()
	tk := &Task{ID: "t1"}

	if err := n.SaveTaskStart(ctx, "s", tk); err != nil {
		t.Errorf("SaveTaskStart: %v", err)
	}
	if err := n.SaveTaskDone(ctx, "s", tk); err != nil {
		t.Errorf("SaveTaskDone: %v", err)
	}
	if err := n.SaveTaskFailed(ctx, "s", tk); err != nil {
		t.Errorf("SaveTaskFailed: %v", err)
	}
	if err := n.DeleteSession(ctx, "s"); err != nil {
		t.Errorf("DeleteSession: %v", err)
	}
	m, err := n.LoadCompletedTasks(ctx, "s", 0)
	if err != nil {
		t.Errorf("LoadCompletedTasks: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("LoadCompletedTasks: expected empty map, got %v", m)
	}
}

// ── PgCheckpoint tests ────────────────────────────────────────────────────────

func TestPgCheckpoint_SaveTaskStart(t *testing.T) {
	db := &cpDB{}
	cp := NewPgCheckpoint(db)
	tk := &Task{ID: "t1", ArgsJSON: `{"q":"hello"}`, RetryCount: 0}
	if err := cp.SaveTaskStart(context.Background(), "sess", tk); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPgCheckpoint_SaveTaskStart_DBError(t *testing.T) {
	db := &cpDB{execErr: errors.New("db down")}
	cp := NewPgCheckpoint(db)
	tk := &Task{ID: "t1"}
	if err := cp.SaveTaskStart(context.Background(), "sess", tk); err == nil {
		t.Fatal("expected error")
	}
}

func TestPgCheckpoint_SaveTaskDone(t *testing.T) {
	db := &cpDB{}
	cp := NewPgCheckpoint(db)
	tk := &Task{ID: "t1", Output: `"result"`, StartedAt: time.Now(), DoneAt: time.Now()}
	if err := cp.SaveTaskDone(context.Background(), "sess", tk); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPgCheckpoint_SaveTaskFailed(t *testing.T) {
	db := &cpDB{}
	cp := NewPgCheckpoint(db)
	tk := &Task{ID: "t1", Error: "timeout", RetryCount: 2, StartedAt: time.Now(), DoneAt: time.Now()}
	if err := cp.SaveTaskFailed(context.Background(), "sess", tk); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPgCheckpoint_DeleteSession(t *testing.T) {
	db := &cpDB{}
	cp := NewPgCheckpoint(db)
	if err := cp.DeleteSession(context.Background(), "sess"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPgCheckpoint_LoadCompletedTasks_HappyPath(t *testing.T) {
	db := &cpDB{rows: &cpRows{data: [][2]string{{"t1", `"output1"`}, {"t2", `"output2"`}}}}
	cp := NewPgCheckpoint(db)
	got, err := cp.LoadCompletedTasks(context.Background(), "sess", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["t1"] != `"output1"` || got["t2"] != `"output2"` {
		t.Errorf("unexpected results: %v", got)
	}
}

func TestPgCheckpoint_LoadCompletedTasks_Empty(t *testing.T) {
	db := &cpDB{rows: &cpRows{data: nil}}
	cp := NewPgCheckpoint(db)
	got, err := cp.LoadCompletedTasks(context.Background(), "sess", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

func TestPgCheckpoint_LoadCompletedTasks_QueryError(t *testing.T) {
	db := &cpDB{queryErr: errors.New("query failed")}
	cp := NewPgCheckpoint(db)
	_, err := cp.LoadCompletedTasks(context.Background(), "sess", 0)
	if err == nil {
		t.Fatal("expected error")
	}
}

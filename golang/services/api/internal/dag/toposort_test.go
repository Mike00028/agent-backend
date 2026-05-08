package dag

import (
	"testing"
)

// ── registry tests ────────────────────────────────────────────────────────────

func TestAgentByName_Known(t *testing.T) {
	for _, name := range []string{"chat_agent", "math_agent", "rag_agent", "summarize_agent", "text_agent", "clarify_agent", "mcp_agent"} {
		def, ok := AgentByName(name)
		if !ok {
			t.Errorf("AgentByName(%q) found=false, want true", name)
		}
		if def.Name != name {
			t.Errorf("AgentByName(%q).Name = %q", name, def.Name)
		}
	}
}

func TestAgentByName_Unknown(t *testing.T) {
	def, ok := AgentByName("custom_db_agent")
	if ok {
		t.Error("AgentByName(unknown) found=true, want false")
	}
	// Safe defaults for unknown agents
	if def.IsLocal {
		t.Error("unknown agent should default IsLocal=false")
	}
	if !def.NeedsQuestion {
		t.Error("unknown agent should default NeedsQuestion=true")
	}
}

func TestAgentRegistry_NoDuplicates(t *testing.T) {
	seen := make(map[string]bool)
	for _, a := range AgentRegistry {
		if seen[a.Name] {
			t.Errorf("duplicate agent name in registry: %q", a.Name)
		}
		seen[a.Name] = true
	}
}

// ── toposort tests ────────────────────────────────────────────────────────────

// task is a helper to build a *Task for tests.
func task(id string, deps ...string) *Task {
	return &Task{ID: id, Title: id, ToolName: "chat_agent", DependsOn: deps}
}

// batchIDs extracts the IDs from a batch slice for easy comparison.
func batchIDs(batches [][]*Task) [][]string {
	out := make([][]string, len(batches))
	for i, b := range batches {
		ids := make([]string, len(b))
		for j, t := range b {
			ids[j] = t.ID
		}
		out[i] = ids
	}
	return out
}

func TestTopoSort_SingleTask(t *testing.T) {
	batches, err := TopoSort([]*Task{task("t1")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(batches) != 1 || len(batches[0]) != 1 || batches[0][0].ID != "t1" {
		t.Errorf("unexpected batches: %v", batchIDs(batches))
	}
}

func TestTopoSort_LinearChain(t *testing.T) {
	// t1 → t2 → t3 — must come out as 3 sequential batches
	tasks := []*Task{task("t1"), task("t2", "t1"), task("t3", "t2")}
	batches, err := TopoSort(tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(batches) != 3 {
		t.Fatalf("expected 3 batches, got %d: %v", len(batches), batchIDs(batches))
	}
	if batches[0][0].ID != "t1" || batches[1][0].ID != "t2" || batches[2][0].ID != "t3" {
		t.Errorf("wrong order: %v", batchIDs(batches))
	}
}

func TestTopoSort_ParallelTasks(t *testing.T) {
	// t1, t2 have no deps → should be in the same batch
	tasks := []*Task{task("t1"), task("t2"), task("t3", "t1", "t2")}
	batches, err := TopoSort(tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(batches) != 2 {
		t.Fatalf("expected 2 batches, got %d: %v", len(batches), batchIDs(batches))
	}
	if len(batches[0]) != 2 {
		t.Errorf("expected 2 parallel tasks in first batch, got %d: %v", len(batches[0]), batchIDs(batches)[0])
	}
	if len(batches[1]) != 1 || batches[1][0].ID != "t3" {
		t.Errorf("expected t3 in second batch, got %v", batchIDs(batches)[1])
	}
}

func TestTopoSort_CycleDetected(t *testing.T) {
	// t1 → t2 → t1 is a cycle
	tasks := []*Task{task("t1", "t2"), task("t2", "t1")}
	_, err := TopoSort(tasks)
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}
}

func TestTopoSort_UnknownDependency(t *testing.T) {
	tasks := []*Task{task("t1", "t99")} // t99 doesn't exist
	_, err := TopoSort(tasks)
	if err == nil {
		t.Fatal("expected error for unknown dependency, got nil")
	}
}

func TestTopoSort_Empty(t *testing.T) {
	batches, err := TopoSort([]*Task{})
	if err != nil {
		t.Fatalf("unexpected error on empty input: %v", err)
	}
	if len(batches) != 0 {
		t.Errorf("expected 0 batches for empty input, got %d", len(batches))
	}
}

func TestTopoSort_Diamond(t *testing.T) {
	// t1 → t2, t1 → t3, t2+t3 → t4
	tasks := []*Task{
		task("t1"),
		task("t2", "t1"),
		task("t3", "t1"),
		task("t4", "t2", "t3"),
	}
	batches, err := TopoSort(tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(batches) != 3 {
		t.Fatalf("expected 3 batches (t1 | t2,t3 | t4), got %d: %v", len(batches), batchIDs(batches))
	}
	if batches[0][0].ID != "t1" {
		t.Errorf("first batch should be t1, got %v", batchIDs(batches)[0])
	}
	if len(batches[1]) != 2 {
		t.Errorf("second batch should have 2 parallel tasks, got %v", batchIDs(batches)[1])
	}
	if batches[2][0].ID != "t4" {
		t.Errorf("third batch should be t4, got %v", batchIDs(batches)[2])
	}
}

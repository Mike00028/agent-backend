package dag

import "fmt"

// TopoSort returns tasks ordered by dependency (Kahn's algorithm).
// Tasks with no dependencies come first; tasks in the same batch can run in parallel.
// Returns an error if a cycle is detected or an unknown dependency is referenced.
func TopoSort(tasks []*Task) ([][]*Task, error) {
	index := make(map[string]*Task, len(tasks))
	for _, t := range tasks {
		index[t.ID] = t
	}

	// Validate all dependencies exist
	for _, t := range tasks {
		for _, dep := range t.DependsOn {
			if _, ok := index[dep]; !ok {
				return nil, fmt.Errorf("task %q has unknown dependency %q", t.ID, dep)
			}
		}
	}

	// Build in-degree map
	inDegree := make(map[string]int, len(tasks))
	for _, t := range tasks {
		inDegree[t.ID] = len(t.DependsOn)
	}

	// Build reverse adjacency: dep → tasks that depend on dep
	dependents := make(map[string][]string, len(tasks))
	for _, t := range tasks {
		for _, dep := range t.DependsOn {
			dependents[dep] = append(dependents[dep], t.ID)
		}
	}

	// Kahn's algorithm — group nodes into batches (one batch = one parallel wave)
	var batches [][]*Task
	queue := make([]*Task, 0, len(tasks))
	for _, t := range tasks {
		if inDegree[t.ID] == 0 {
			queue = append(queue, t)
		}
	}

	visited := 0
	for len(queue) > 0 {
		// All tasks currently in the queue form one batch (no unresolved deps)
		batch := make([]*Task, len(queue))
		copy(batch, queue)
		batches = append(batches, batch)
		visited += len(batch)

		// Reduce in-degree for all dependents of this batch
		nextQueue := queue[:0]
		for _, t := range batch {
			for _, depID := range dependents[t.ID] {
				inDegree[depID]--
				if inDegree[depID] == 0 {
					nextQueue = append(nextQueue, index[depID])
				}
			}
		}
		queue = nextQueue
	}

	if visited != len(tasks) {
		return nil, fmt.Errorf("cycle detected in DAG")
	}

	return batches, nil
}

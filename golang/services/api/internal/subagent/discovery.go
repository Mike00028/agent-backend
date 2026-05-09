package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// DiscoveryResult consolidates all discovered resources for the planner.
// Results are populated via parallel goroutines for low latency.
type DiscoveryResult struct {
	// SubAgents discovered via hybrid search (top-k by relevance)
	SubAgents []SubAgentSummary `json:"sub_agents,omitempty"`

	// ErrorMsg captures any discovery errors (non-fatal; partial results still sent to planner)
	ErrorMsg string `json:"error,omitempty"`
}

// SubAgentSummary is minimal metadata for planner context (not full config).
type SubAgentSummary struct {
	Name        string  `json:"name"`
	Type        string  `json:"type"`
	Description string  `json:"description"`
	Score       float64 `json:"score,omitempty"`
	IsShared    bool    `json:"is_shared"`
}

// DiscoverRequest configures discovery parameters.
type DiscoverRequest struct {
	// Query for hybrid search (user message)
	Query string

	// UserID for permission checks (see shared/owned agents)
	UserID string

	// SearchLimit controls top-k results returned from hybrid search
	SearchLimit int

	// TimeoutMS sets overall discovery deadline (typically 500-1000ms)
	TimeoutMS int
}

// Discover performs parallel discovery of subagents via hybrid search.
// Returns DiscoveryResult with matching agents + any errors encountered.
// Does NOT block on errors; partial results are always returned.
//
// Discovery runs in parallel:
//   - SubAgent search (via store.Search)
//   - Error handling (non-fatal; logs but forwards result)
//
// Typical latency: 50-200ms (varies with DB latency).
func (svc *Service) Discover(ctx context.Context, req DiscoverRequest) (*DiscoveryResult, error) {
	if req.Query == "" {
		return &DiscoveryResult{}, nil // no query = no discovery
	}

	if req.SearchLimit == 0 {
		req.SearchLimit = 10 // default top-k
	}

	// Set deadline if timeoutMS specified
	if req.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMS)*time.Millisecond)
		defer cancel()
	}

	result := &DiscoveryResult{}
	var wg sync.WaitGroup
	var mu sync.Mutex

	// Parallel discovery of subagents via hybrid search
	wg.Add(1)
	go func() {
		defer wg.Done()

		searchReq := &SearchRequest{
			UserID: req.UserID,
			Query:  req.Query,
			Limit:  req.SearchLimit,
		}
		searchRes, err := svc.Search(ctx, searchReq)
		if err != nil {
			mu.Lock()
			result.ErrorMsg = fmt.Sprintf("subagent search failed: %v", err)
			mu.Unlock()
			return
		}

		// Convert SearchResult → SubAgentSummary
		summaries := make([]SubAgentSummary, len(searchRes))
		for i, sr := range searchRes {
			summaries[i] = SubAgentSummary{
				Name:        sr.Agent.Name,
				Type:        string(sr.Agent.Type),
				Description: sr.Agent.Description,
				Score:       sr.Score,
				IsShared:    sr.Agent.IsShared,
			}
		}

		mu.Lock()
		result.SubAgents = summaries
		mu.Unlock()
	}()

	wg.Wait()
	return result, nil
}

// ToAgentSpecJSON serializes discovery result into planner-compatible JSON.
// Format: {"sub_agents": [{"name": "...", "description": "...", "type": "..."}]}
// This gets embedded in the planner system prompt.
func (dr *DiscoveryResult) ToAgentSpecJSON() string {
	type agentSpec struct {
		SubAgents []map[string]interface{} `json:"sub_agents"`
	}

	spec := agentSpec{
		SubAgents: make([]map[string]interface{}, len(dr.SubAgents)),
	}

	for i, sa := range dr.SubAgents {
		spec.SubAgents[i] = map[string]interface{}{
			"name":        sa.Name,
			"type":        sa.Type,
			"description": sa.Description,
		}
	}

	b, _ := json.Marshal(spec)
	return string(b)
}

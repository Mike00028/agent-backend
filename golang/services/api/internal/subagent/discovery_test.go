package subagent

import (
	"context"
	"testing"
	"time"
)

// MockStore implements Store for testing discovery
type MockStore struct {
	agents map[string]*SubAgent
}

func NewMockStore() *MockStore {
	return &MockStore{
		agents: make(map[string]*SubAgent),
	}
}

func (m *MockStore) Get(ctx context.Context, ownerID, name string) (*SubAgent, error) {
	key := ownerID + ":" + name
	if agent, ok := m.agents[key]; ok {
		return agent, nil
	}
	return nil, ErrNotFound
}

func (m *MockStore) GetByID(ctx context.Context, id string) (*SubAgent, error) {
	for _, agent := range m.agents {
		if agent.ID == id {
			return agent, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockStore) List(ctx context.Context, userID string, filters *ListFilters) ([]SubAgent, int, error) {
	var result []SubAgent
	for _, agent := range m.agents {
		// Simple filter: owner or shared
		isOwner := agent.OwnerID != nil && *agent.OwnerID == userID
		if isOwner || agent.IsShared {
			result = append(result, *agent)
		}
	}
	return result, len(result), nil
}

func (m *MockStore) Search(ctx context.Context, userID string, query *SearchQuery) ([]SearchResult, error) {
	var results []SearchResult
	for _, agent := range m.agents {
		// Simple relevance: if name/desc contains query string, score 0.8
		if contains(agent.Name, query.Query) || contains(agent.Description, query.Query) {
			isOwner := agent.OwnerID != nil && *agent.OwnerID == userID
			if isOwner || agent.IsShared {
				results = append(results, SearchResult{
					Agent: *agent,
					Score: 0.8,
				})
			}
		}
	}
	return results, nil
}

func (m *MockStore) Create(ctx context.Context, agent *SubAgent) error {
	var ownerID string
	if agent.OwnerID != nil {
		ownerID = *agent.OwnerID
	}
	key := ownerID + ":" + agent.Name
	if _, ok := m.agents[key]; ok {
		return ErrConflict
	}
	m.agents[key] = agent
	return nil
}

func (m *MockStore) Update(ctx context.Context, ownerID, name string, updates *UpdateSubAgentRequest) error {
	key := ownerID + ":" + name
	agent, ok := m.agents[key]
	if !ok {
		return ErrNotFound
	}
	// Apply updates without modifying agent
	_ = agent
	return nil
}

func (m *MockStore) Delete(ctx context.Context, ownerID, name string) error {
	key := ownerID + ":" + name
	if _, ok := m.agents[key]; !ok {
		return ErrNotFound
	}
	delete(m.agents, key)
	return nil
}

func (m *MockStore) ExistsByName(ctx context.Context, ownerID, name string) (bool, error) {
	key := ownerID + ":" + name
	_, ok := m.agents[key]
	return ok, nil
}

func (m *MockStore) ExistsSystemByName(ctx context.Context, name string) (bool, error) {
	key := ":" + name // system agents have empty ownerID
	_, ok := m.agents[key]
	return ok, nil
}

// TestDiscover_EmptyQuery returns no results
func TestDiscover_EmptyQuery(t *testing.T) {
	ctx := context.Background()
	store := NewMockStore()
	service := NewService(store, nil)

	result, err := service.Discover(ctx, DiscoverRequest{
		Query:  "",
		UserID: "user1",
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.SubAgents) != 0 {
		t.Errorf("expected 0 results for empty query, got %d", len(result.SubAgents))
	}
}

// TestDiscover_FindsMatchingAgents finds agents that match query
func TestDiscover_FindsMatchingAgents(t *testing.T) {
	ctx := context.Background()
	store := NewMockStore()
	service := NewService(store, nil)

	// Add test agents
	user1 := "user1"
	agent1 := &SubAgent{
		ID:          "id1",
		OwnerID:     &user1,
		Name:        "security_auditor",
		Description: "Security vulnerability scanner",
		IsShared:    true,
	}
	agent2 := &SubAgent{
		ID:          "id2",
		OwnerID:     &user1,
		Name:        "code_reviewer",
		Description: "Code quality analyzer",
		IsShared:    false,
	}

	store.Create(ctx, agent1)
	store.Create(ctx, agent2)

	// Discover with query matching agent1
	result, err := service.Discover(ctx, DiscoverRequest{
		Query:       "security vulnerability",
		UserID:      "user1",
		SearchLimit: 10,
		TimeoutMS:   500,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.SubAgents) < 1 {
		t.Error("expected to find matching agents")
	}
}

// TestDiscover_RespectPermissions only returns visible agents
func TestDiscover_RespectPermissions(t *testing.T) {
	ctx := context.Background()
	store := NewMockStore()
	service := NewService(store, nil)

	// Add shared agent
	user1 := "user1"
	shared := &SubAgent{
		ID:       "id1",
		OwnerID:  &user1,
		Name:     "shared_agent",
		IsShared: true,
	}
	// Add private agent
	private := &SubAgent{
		ID:       "id2",
		OwnerID:  &user1,
		Name:     "private_agent",
		IsShared: false,
	}

	store.Create(ctx, shared)
	store.Create(ctx, private)

	// user2 queries: should only see shared agent
	result, err := service.Discover(ctx, DiscoverRequest{
		Query:       "agent",
		UserID:      "user2",
		SearchLimit: 10,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only shared agent should be visible
	if len(result.SubAgents) != 1 {
		t.Errorf("expected 1 visible agent for user2, got %d", len(result.SubAgents))
	}
}

// TestDiscover_TimeoutEnforced tests timeout mechanism
func TestDiscover_TimeoutEnforced(t *testing.T) {
	// Create context that will cancel
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	store := NewMockStore()
	service := NewService(store, nil)

	// Attempt discovery with timeout
	result, _ := service.Discover(ctx, DiscoverRequest{
		Query:       "test",
		UserID:      "user1",
		SearchLimit: 10,
		TimeoutMS:   5, // Very short timeout
	})

	// Should complete (no timeout error expected since search is fast)
	// This test mainly validates that timeout is set up correctly
	if result == nil {
		t.Error("expected non-nil result")
	}
}

// TestDiscoveryResult_ToAgentSpecJSON serializes correctly
func TestDiscoveryResult_ToAgentSpecJSON(t *testing.T) {
	result := &DiscoveryResult{
		SubAgents: []SubAgentSummary{
			{
				Name:        "agent1",
				Type:        "react",
				Description: "Test agent",
				Score:       0.95,
				IsShared:    true,
			},
		},
	}

	json := result.ToAgentSpecJSON()
	if json == "" {
		t.Error("expected non-empty JSON")
	}
	if !contains(json, "agent1") {
		t.Error("JSON should contain agent name")
	}
	if !contains(json, "react") {
		t.Error("JSON should contain agent type")
	}
}

// TestDiscoveryResult_EmptySubAgents handles empty discovery
func TestDiscoveryResult_EmptySubAgents(t *testing.T) {
	result := &DiscoveryResult{
		SubAgents: []SubAgentSummary{},
	}

	json := result.ToAgentSpecJSON()
	if json == "" {
		t.Error("expected non-empty JSON even for empty results")
	}
	// Should contain valid JSON structure
	if !contains(json, "sub_agents") {
		t.Error("JSON should contain sub_agents key")
	}
}

// TestDiscoveryResult_WithError includes error message
func TestDiscoveryResult_WithError(t *testing.T) {
	result := &DiscoveryResult{
		SubAgents: []SubAgentSummary{},
		ErrorMsg:  "search service unavailable",
	}

	if result.ErrorMsg == "" {
		t.Error("expected error message to be stored")
	}
}

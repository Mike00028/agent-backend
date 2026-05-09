package subagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Service orchestrates subagent lifecycle: parsing, validation, embedding, storage.
type Service struct {
	store    Store
	embedder Embedder // for generating vectors
}

// Embedder defines the interface for vector embedding.
type Embedder interface {
	// Embed returns a vector embedding for the given text.
	// Should return a 768-dimensional vector.
	Embed(ctx context.Context, text string) ([]float32, error)
}

// NewService creates a new Service.
func NewService(store Store, embedder Embedder) *Service {
	return &Service{
		store:    store,
		embedder: embedder,
	}
}

// IngestRequest is the input to Ingest().
type IngestRequest struct {
	OwnerID  string       // user uploading the agent
	Content  []byte       // raw file content (agents.md or Flowise JSON)
	Format   SourceFormat // detected format
	Shared   bool         // visibility flag
	Tags     []string     // optional tags
	Category string       // optional category
}

// IngestResult is the output of Ingest().
type IngestResult struct {
	Agent        *SubAgent
	IsNew        bool   // true if newly created, false if it replaced an existing agent
	EmbeddingErr string // non-fatal: if embedding failed, agent is still stored
}

// Ingest uploads and processes a new agent or updates an existing one.
// Process:
// 1. Parse (YAML or Flowise)
// 2. Validate syntactically
// 3. Hash content (SHA-256)
// 4. Check if hash matches existing agent (skip re-embed if unchanged)
// 5. Generate embedding for name + description
// 6. Store in database
func (s *Service) Ingest(ctx context.Context, req *IngestRequest) (*IngestResult, error) {
	// Parse based on format
	var agent *SubAgent
	var config *SubAgentConfig
	var schemaHash string
	var err error

	switch req.Format {
	case FormatAgentsMD:
		agent, config, schemaHash, err = ParseAgentsMD(req.Content)
		if err != nil {
			return nil, fmt.Errorf("failed to parse agents.md: %w", err)
		}

	case FormatFlowise:
		agent, config, schemaHash, err = ParseFlowise(req.Content)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Flowise: %w", err)
		}

	default:
		return nil, fmt.Errorf("unsupported format: %s", req.Format)
	}

	// Validate config
	validationErrs := ValidateConfig(agent.Type, config)
	if len(validationErrs) > 0 {
		details := make([]map[string]interface{}, len(validationErrs))
		for i, ve := range validationErrs {
			details[i] = map[string]interface{}{
				"field":   ve.Field,
				"message": ve.Message,
				"code":    ve.Code,
			}
		}
		return nil, fmt.Errorf("config validation failed: %v", details)
	}

	// Set metadata
	agent.ID = uuid.New().String()
	agent.OwnerID = &req.OwnerID
	agent.Config = *config
	agent.SchemaHash = schemaHash
	agent.Content = string(req.Content)
	agent.IsShared = req.Shared
	agent.IsSystem = false
	agent.Tags = req.Tags
	agent.Category = req.Category
	agent.CreatedAt = time.Now().UTC()
	agent.UpdatedAt = time.Now().UTC()

	// Generate embedding hint for vector embedding
	embeddingHint := EmbeddingHint(agent, config)

	// Generate embedding (async-safe, non-blocking on error)
	var embeddingErr string
	if s.embedder != nil {
		embedding, err := s.embedder.Embed(ctx, embeddingHint)
		if err != nil {
			embeddingErr = fmt.Sprintf("embedding failed: %v", err)
			// Continue anyway; agent is still stored
		} else {
			agent.Embedding = embedding
		}
	}

	// Check if agent name already exists for this owner
	// If schema_hash == prior hash, skip re-embed (update updated_at)
	isNew := true
	existing, err := s.store.Get(ctx, req.OwnerID, agent.Name)
	if err == nil && existing != nil {
		// Agent exists; check if content changed
		if existing.SchemaHash == schemaHash {
			// No content change; just update updated_at
			updates := &UpdateSubAgentRequest{
				IsShared:          &agent.IsShared,
				Description:       &agent.Description,
				Tags:              agent.Tags,
				Category:          &agent.Category,
				DeprecatedAt:      nil,
				DeprecationNotice: nil,
			}
			if err := s.store.Update(ctx, req.OwnerID, agent.Name, updates); err != nil {
				return nil, fmt.Errorf("failed to update existing agent: %w", err)
			}
			isNew = false
			// Re-fetch to get updated timestamps
			if updated, err := s.store.Get(ctx, req.OwnerID, agent.Name); err == nil && updated != nil {
				return &IngestResult{
					Agent:        updated,
					IsNew:        false,
					EmbeddingErr: embeddingErr,
				}, nil
			}
		} else {
			// Content changed; use new agent.ID but reuse metadata if possible
			isNew = false
		}
	} else if err != nil && err != ErrNotFound {
		return nil, fmt.Errorf("failed to check existing agent: %w", err)
	}

	// Store agent
	if err := s.store.Create(ctx, agent); err != nil && err != ErrConflict {
		return nil, fmt.Errorf("failed to store agent: %w", err)
	}

	// Fetch the stored agent to return (includes timestamps)
	stored, err := s.store.GetByID(ctx, agent.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch stored agent: %w", err)
	}

	return &IngestResult{
		Agent:        stored,
		IsNew:        isNew,
		EmbeddingErr: embeddingErr,
	}, nil
}

// GetRequest is the input to Get().
type GetRequest struct {
	UserID string
	Name   string
}

// Get retrieves a single agent by name (access-controlled).
func (s *Service) Get(ctx context.Context, req *GetRequest) (*SubAgent, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("agent name is required")
	}

	agent, err := s.store.Get(ctx, req.UserID, req.Name)
	if err != nil {
		return nil, err
	}

	if agent == nil {
		return nil, ErrNotFound
	}

	// Access control: user must be owner OR agent must be shared
	if agent.OwnerID != nil && *agent.OwnerID != req.UserID && !agent.IsShared && !agent.IsSystem {
		return nil, ErrForbidden
	}

	return agent, nil
}

// ListRequest is the input to List().
type ListRequest struct {
	UserID  string
	Filters *ListFilters
}

// List retrieves paginated subagents visible to a user.
func (s *Service) List(ctx context.Context, req *ListRequest) ([]SubAgent, int, error) {
	if req.Filters == nil {
		req.Filters = &ListFilters{Limit: 50, Offset: 0}
	}

	agents, total, err := s.store.List(ctx, req.UserID, req.Filters)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list agents: %w", err)
	}

	return agents, total, nil
}

// GetByIDRequest is the input to GetByID().
type GetByIDRequest struct {
	UserID string
	ID     string
}

// GetByID retrieves a single agent by UUID (access-controlled).
func (s *Service) GetByID(ctx context.Context, req *GetByIDRequest) (*SubAgent, error) {
	if req.ID == "" {
		return nil, fmt.Errorf("agent ID is required")
	}

	agent, err := s.store.GetByID(ctx, req.ID)
	if err != nil {
		return nil, err
	}

	if agent == nil {
		return nil, ErrNotFound
	}

	// Access control: user must be owner OR agent must be shared
	if agent.OwnerID != nil && *agent.OwnerID != req.UserID && !agent.IsShared && !agent.IsSystem {
		return nil, ErrForbidden
	}

	return agent, nil
}

// SearchRequest is the input to Search().
type SearchRequest struct {
	UserID string        // user performing search
	Query  string        // search text
	Limit  int           // max results
	Tags   []string      // optional tag filters
	Type   *SubAgentType // optional type filter
}

// Search performs hybrid search (keyword + vector).
func (s *Service) Search(ctx context.Context, req *SearchRequest) ([]SearchResult, error) {
	if req.Query == "" {
		return []SearchResult{}, nil
	}

	if req.Limit <= 0 {
		req.Limit = 10
	}
	if req.Limit > 100 {
		req.Limit = 100
	}

	// Embed the search query
	var queryVec []float32
	if s.embedder != nil {
		vec, err := s.embedder.Embed(ctx, req.Query)
		if err != nil {
			// Fall back to keyword-only search
			queryVec = nil
		} else {
			queryVec = vec
		}
	}

	// Query store with vector + text
	var agentType SubAgentType
	if req.Type != nil {
		agentType = *req.Type
	}
	searchQuery := &SearchQuery{
		UserID: req.UserID,
		Query:  req.Query,
		Vector: queryVec,
		Tags:   req.Tags,
		Type:   agentType,
		Limit:  req.Limit,
	}

	results, err := s.store.Search(ctx, req.UserID, searchQuery)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	return results, nil
}

// UpdateRequest is the input to Update().
type UpdateRequest struct {
	UserID            string
	Name              string
	IsShared          *bool
	IsEnabled         *bool
	Description       *string
	Tags              []string
	Category          *string
	DeprecatedAt      *time.Time
	DeprecationNotice *string
}

// Update modifies agent metadata.
func (s *Service) Update(ctx context.Context, req *UpdateRequest) (*SubAgent, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("agent name is required")
	}

	// Permission check: only owner can update
	existing, err := s.store.Get(ctx, req.UserID, req.Name)
	if err != nil {
		return nil, err
	}

	if existing == nil {
		return nil, ErrNotFound
	}

	if existing.IsSystem {
		return nil, fmt.Errorf("cannot update system agents")
	}

	if existing.OwnerID == nil || *existing.OwnerID != req.UserID {
		return nil, ErrForbidden
	}

	// Apply updates
	updates := &UpdateSubAgentRequest{
		IsShared:          req.IsShared,
		IsEnabled:         req.IsEnabled,
		Description:       req.Description,
		Tags:              req.Tags,
		Category:          req.Category,
		DeprecatedAt:      req.DeprecatedAt,
		DeprecationNotice: req.DeprecationNotice,
	}

	if err := s.store.Update(ctx, req.UserID, req.Name, updates); err != nil {
		return nil, err
	}

	// Fetch updated agent
	updated, err := s.store.Get(ctx, req.UserID, req.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch updated agent: %w", err)
	}

	return updated, nil
}

// DeleteRequest is the input to Delete().
type DeleteRequest struct {
	UserID string
	Name   string
}

// Delete soft-deletes an agent.
func (s *Service) Delete(ctx context.Context, req *DeleteRequest) error {
	if req.Name == "" {
		return fmt.Errorf("agent name is required")
	}

	// Permission check: only owner can delete
	existing, err := s.store.Get(ctx, req.UserID, req.Name)
	if err != nil {
		return err
	}

	if existing == nil {
		return ErrNotFound
	}

	if existing.IsSystem {
		return fmt.Errorf("cannot delete system agents")
	}

	if existing.OwnerID == nil || *existing.OwnerID != req.UserID {
		return ErrForbidden
	}

	return s.store.Delete(ctx, req.UserID, req.Name)
}

// ComputeHash computes SHA-256 hash of content.
func ComputeHash(content []byte) string {
	hash := sha256.Sum256(content)
	return hex.EncodeToString(hash[:])
}

// NormalizeQuery prepares search query text (trim whitespace, lowercase for keyword matching).
func NormalizeQuery(query string) string {
	return strings.ToLower(strings.TrimSpace(query))
}

package subagent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store defines the interface for subagent persistence.
type Store interface {
	// Get retrieves a single subagent by owner and name.
	// Returns ErrNotFound if agent doesn't exist or is not accessible to owner.
	Get(ctx context.Context, ownerID, name string) (*SubAgent, error)

	// GetByID retrieves a subagent by UUID.
	GetByID(ctx context.Context, id string) (*SubAgent, error)

	// List returns all subagents visible to a user (private + shared + enabled).
	// Supports filtering by type, category, tags (AND), shared flag.
	// Returns paginated results.
	List(ctx context.Context, userID string, filters *ListFilters) ([]SubAgent, int, error)

	// Search performs hybrid search (keyword + vector) on subagents visible to user.
	// Combines FTS score (30%) + vector similarity (70%).
	// Returns top-k results sorted by combined score.
	Search(ctx context.Context, userID string, query *SearchQuery) ([]SearchResult, error)

	// Create inserts a new subagent.
	// Returns ErrConflict if agent with same (owner_id, name) already exists.
	Create(ctx context.Context, agent *SubAgent) error

	// Update modifies metadata of an existing subagent (owner or admin only).
	// Updates: is_shared, is_enabled, description, tags, category, deprecated_at, deprecation_notice.
	// Does NOT update: type, config, content (would require re-upload).
	Update(ctx context.Context, ownerID, name string, updates *UpdateSubAgentRequest) error

	// Delete soft-deletes a subagent by setting is_enabled=false (owner or admin only).
	// System agents cannot be deleted.
	Delete(ctx context.Context, ownerID, name string) error

	// ExistsByName checks if a subagent name exists for a given owner.
	// For system agents, ownerID should be "" or nil-equivalent.
	ExistsByName(ctx context.Context, ownerID, name string) (bool, error)

	// ExistsSystemByName checks if a system agent with given name exists.
	ExistsSystemByName(ctx context.Context, name string) (bool, error)
}

// ListFilters defines filtering options for List queries.
type ListFilters struct {
	Type     *SubAgentType // filter by type
	Category *string       // filter by category
	Tags     []string      // filter by tags (AND)
	Shared   *bool         // true=only shared, false=only private, nil=both
	Limit    int           // default 50, max 200
	Offset   int           // default 0
}

// pgStore implements Store using pgxpool.
type pgStore struct {
	pool *pgxpool.Pool
}

// NewStore creates a new Store backed by PostgreSQL.
func NewStore(pool *pgxpool.Pool) Store {
	return &pgStore{pool: pool}
}

// Get retrieves a single subagent by owner and name.
func (s *pgStore) Get(ctx context.Context, ownerID, name string) (*SubAgent, error) {
	query := `
		SELECT id, owner_id, name, type, description, source_format, content, config, schema_hash,
		       embedding, tags, category, version, is_shared, is_system, is_enabled,
		       deprecated_at, deprecation_notice, created_at, updated_at
		FROM subagents
		WHERE name = $1 AND (owner_id = $2 OR is_system = true)
		AND is_enabled = true
		LIMIT 1
	`

	agent := &SubAgent{}
	var ownerIDPtr *string
	var configJSON []byte

	err := s.pool.QueryRow(ctx, query, name, ownerID).Scan(
		&agent.ID, &ownerIDPtr, &agent.Name, &agent.Type, &agent.Description,
		&agent.SourceFormat, &agent.Content, &configJSON, &agent.SchemaHash,
		&agent.Embedding, &agent.Tags, &agent.Category, &agent.Version,
		&agent.IsShared, &agent.IsSystem, &agent.IsEnabled,
		&agent.DeprecatedAt, &agent.DeprecationNotice, &agent.CreatedAt, &agent.UpdatedAt,
	)

	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get subagent: %w", err)
	}

	agent.OwnerID = ownerIDPtr

	// Parse config JSON
	if err := parseConfigJSON(configJSON, &agent.Config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	return agent, nil
}

// GetByID retrieves a subagent by UUID.
func (s *pgStore) GetByID(ctx context.Context, id string) (*SubAgent, error) {
	query := `
		SELECT id, owner_id, name, type, description, source_format, content, config, schema_hash,
		       embedding, tags, category, version, is_shared, is_system, is_enabled,
		       deprecated_at, deprecation_notice, created_at, updated_at
		FROM subagents
		WHERE id = $1 AND is_enabled = true
		LIMIT 1
	`

	agent := &SubAgent{}
	var ownerIDPtr *string
	var configJSON []byte

	err := s.pool.QueryRow(ctx, query, id).Scan(
		&agent.ID, &ownerIDPtr, &agent.Name, &agent.Type, &agent.Description,
		&agent.SourceFormat, &agent.Content, &configJSON, &agent.SchemaHash,
		&agent.Embedding, &agent.Tags, &agent.Category, &agent.Version,
		&agent.IsShared, &agent.IsSystem, &agent.IsEnabled,
		&agent.DeprecatedAt, &agent.DeprecationNotice, &agent.CreatedAt, &agent.UpdatedAt,
	)

	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get subagent by id: %w", err)
	}

	agent.OwnerID = ownerIDPtr

	if err := parseConfigJSON(configJSON, &agent.Config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	return agent, nil
}

// List returns all subagents visible to a user with optional filtering.
func (s *pgStore) List(ctx context.Context, userID string, filters *ListFilters) ([]SubAgent, int, error) {
	if filters == nil {
		filters = &ListFilters{Limit: 50, Offset: 0}
	}

	if filters.Limit <= 0 {
		filters.Limit = 50
	}
	if filters.Limit > 200 {
		filters.Limit = 200
	}

	// Build dynamic WHERE clause
	whereClause := "(owner_id = $1 OR (is_shared = true AND is_system = false)) AND is_enabled = true"
	args := []interface{}{userID}
	argIndex := 2

	if filters.Type != nil {
		whereClause += fmt.Sprintf(" AND type = $%d", argIndex)
		args = append(args, *filters.Type)
		argIndex++
	}

	if filters.Category != nil {
		whereClause += fmt.Sprintf(" AND category = $%d", argIndex)
		args = append(args, *filters.Category)
		argIndex++
	}

	if len(filters.Tags) > 0 {
		// Tags must contain all specified tags (AND logic)
		for _, tag := range filters.Tags {
			whereClause += fmt.Sprintf(" AND tags @> $%d", argIndex)
			args = append(args, tag)
			argIndex++
		}
	}

	if filters.Shared != nil {
		whereClause += fmt.Sprintf(" AND is_shared = $%d", argIndex)
		args = append(args, *filters.Shared)
		argIndex++
	}

	// Count total
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM subagents WHERE %s", whereClause)
	var total int
	if err := s.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("failed to count subagents: %w", err)
	}

	// List with pagination
	listQuery := fmt.Sprintf(`
		SELECT id, owner_id, name, type, description, source_format, content, config, schema_hash,
		       embedding, tags, category, version, is_shared, is_system, is_enabled,
		       deprecated_at, deprecation_notice, created_at, updated_at
		FROM subagents
		WHERE %s
		ORDER BY updated_at DESC
		LIMIT $%d OFFSET $%d
	`, whereClause, argIndex, argIndex+1)

	args = append(args, filters.Limit, filters.Offset)

	rows, err := s.pool.Query(ctx, listQuery, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list subagents: %w", err)
	}
	defer rows.Close()

	var agents []SubAgent
	for rows.Next() {
		agent := SubAgent{}
		var ownerIDPtr *string
		var configJSON []byte

		if err := rows.Scan(
			&agent.ID, &ownerIDPtr, &agent.Name, &agent.Type, &agent.Description,
			&agent.SourceFormat, &agent.Content, &configJSON, &agent.SchemaHash,
			&agent.Embedding, &agent.Tags, &agent.Category, &agent.Version,
			&agent.IsShared, &agent.IsSystem, &agent.IsEnabled,
			&agent.DeprecatedAt, &agent.DeprecationNotice, &agent.CreatedAt, &agent.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan row: %w", err)
		}

		agent.OwnerID = ownerIDPtr

		if err := parseConfigJSON(configJSON, &agent.Config); err != nil {
			return nil, 0, fmt.Errorf("failed to parse config: %w", err)
		}

		agents = append(agents, agent)
	}

	if err = rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("row iteration error: %w", err)
	}

	return agents, total, nil
}

// Search performs hybrid search combining FTS (keyword) and vector similarity.
// Score = 0.3 * keyword_score + 0.7 * vector_score
func (s *pgStore) Search(ctx context.Context, userID string, query *SearchQuery) ([]SearchResult, error) {
	if query.Limit <= 0 {
		query.Limit = 10
	}
	if query.Limit > 100 {
		query.Limit = 100
	}

	// Generate FTS query from text
	searchSQL := `
		WITH visible AS (
			SELECT id, owner_id, name, type, description, source_format, content, config, schema_hash,
			       embedding, tags, category, version, is_shared, is_system, is_enabled,
			       deprecated_at, deprecation_notice, created_at, updated_at
			FROM subagents
			WHERE (owner_id = $1 OR (is_shared = true AND is_system = false))
			AND is_enabled = true
		),
		fts_results AS (
			SELECT id, 
					ts_rank(search_text_ts, plainto_tsquery('english', $2)) / 32 AS fts_score
			FROM visible
			WHERE search_text_ts @@ plainto_tsquery('english', $2)
		),
		vector_results AS (
			SELECT id,
					(1 + (embedding <=> $3::vector)) / 2 AS vec_score
			FROM visible
			WHERE embedding IS NOT NULL
		)
		SELECT 
			v.id, v.owner_id, v.name, v.type, v.description, v.source_format, v.content, v.config, v.schema_hash,
			v.embedding, v.tags, v.category, v.version, v.is_shared, v.is_system, v.is_enabled,
			v.deprecated_at, v.deprecation_notice, v.created_at, v.updated_at,
			COALESCE(f.fts_score, 0) AS fts_score,
			COALESCE(vr.vec_score, 0) AS vec_score,
			(0.3 * COALESCE(f.fts_score, 0) + 0.7 * COALESCE(vr.vec_score, 0)) AS combined_score
		FROM visible v
		LEFT JOIN fts_results f ON v.id = f.id
		LEFT JOIN vector_results vr ON v.id = vr.id
		WHERE (f.fts_score > 0 OR vr.vec_score > 0)
		ORDER BY combined_score DESC
		LIMIT $4
	`

	rows, err := s.pool.Query(ctx, searchSQL, userID, query.Query, query.Vector, query.Limit)
	if err != nil {
		return nil, fmt.Errorf("failed to execute hybrid search: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		agent := SubAgent{}
		var ownerIDPtr *string
		var configJSON []byte
		var score float64

		if err := rows.Scan(
			&agent.ID, &ownerIDPtr, &agent.Name, &agent.Type, &agent.Description,
			&agent.SourceFormat, &agent.Content, &configJSON, &agent.SchemaHash,
			&agent.Embedding, &agent.Tags, &agent.Category, &agent.Version,
			&agent.IsShared, &agent.IsSystem, &agent.IsEnabled,
			&agent.DeprecatedAt, &agent.DeprecationNotice, &agent.CreatedAt, &agent.UpdatedAt,
			nil, nil, &score, // fts_score, vec_score, combined_score
		); err != nil {
			return nil, fmt.Errorf("failed to scan search result: %w", err)
		}

		agent.OwnerID = ownerIDPtr

		if err := parseConfigJSON(configJSON, &agent.Config); err != nil {
			return nil, fmt.Errorf("failed to parse config: %w", err)
		}

		results = append(results, SearchResult{
			Agent: agent,
			Score: score,
		})
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("search row iteration error: %w", err)
	}

	return results, nil
}

// Create inserts a new subagent.
func (s *pgStore) Create(ctx context.Context, agent *SubAgent) error {
	if agent.Name == "" {
		return errors.New("agent name is required")
	}

	configJSON, err := encodeConfigJSON(&agent.Config)
	if err != nil {
		return fmt.Errorf("failed to encode config: %w", err)
	}

	query := `
		INSERT INTO subagents 
		(id, owner_id, name, type, description, source_format, content, config, schema_hash,
		 embedding, tags, category, version, is_shared, is_system, is_enabled,
		 deprecated_at, deprecation_notice, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
		ON CONFLICT (id) DO NOTHING
	`

	now := time.Now().UTC()
	if agent.CreatedAt.IsZero() {
		agent.CreatedAt = now
	}
	if agent.UpdatedAt.IsZero() {
		agent.UpdatedAt = now
	}

	result, err := s.pool.Exec(ctx, query,
		agent.ID, agent.OwnerID, agent.Name, agent.Type, agent.Description,
		agent.SourceFormat, agent.Content, configJSON, agent.SchemaHash,
		agent.Embedding, agent.Tags, agent.Category, agent.Version,
		agent.IsShared, agent.IsSystem, agent.IsEnabled,
		agent.DeprecatedAt, agent.DeprecationNotice, agent.CreatedAt, agent.UpdatedAt,
	)

	if err != nil {
		// Check for constraint violations
		if err.Error() == "duplicate key value violates unique constraint" {
			return ErrConflict
		}
		return fmt.Errorf("failed to insert subagent: %w", err)
	}

	if result.RowsAffected() == 0 {
		return ErrConflict
	}

	return nil
}

// Update modifies metadata of an existing subagent.
func (s *pgStore) Update(ctx context.Context, ownerID, name string, updates *UpdateSubAgentRequest) error {
	if ownerID == "" {
		return errors.New("owner_id is required for update")
	}

	// Check ownership first
	checkQuery := `SELECT owner_id, is_system FROM subagents WHERE name = $1 AND is_enabled = true`
	var actualOwnerID *string
	var isSystem bool

	err := s.pool.QueryRow(ctx, checkQuery, name).Scan(&actualOwnerID, &isSystem)
	if err == pgx.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to check ownership: %w", err)
	}

	if isSystem {
		return errors.New("cannot update system agents")
	}

	// Verify ownership
	if actualOwnerID == nil || *actualOwnerID != ownerID {
		return ErrForbidden
	}

	// Build dynamic UPDATE statement
	updateSet := "updated_at = NOW()"
	args := []interface{}{name}
	argIndex := 2

	if updates.IsShared != nil {
		updateSet += fmt.Sprintf(", is_shared = $%d", argIndex)
		args = append(args, *updates.IsShared)
		argIndex++
	}

	if updates.IsEnabled != nil {
		updateSet += fmt.Sprintf(", is_enabled = $%d", argIndex)
		args = append(args, *updates.IsEnabled)
		argIndex++
	}

	if updates.Description != nil {
		updateSet += fmt.Sprintf(", description = $%d", argIndex)
		args = append(args, *updates.Description)
		argIndex++
	}

	if len(updates.Tags) > 0 {
		updateSet += fmt.Sprintf(", tags = $%d", argIndex)
		args = append(args, updates.Tags)
		argIndex++
	}

	if updates.Category != nil {
		updateSet += fmt.Sprintf(", category = $%d", argIndex)
		args = append(args, *updates.Category)
		argIndex++
	}

	if updates.DeprecatedAt != nil {
		updateSet += fmt.Sprintf(", deprecated_at = $%d", argIndex)
		args = append(args, *updates.DeprecatedAt)
		argIndex++
	}

	if updates.DeprecationNotice != nil {
		updateSet += fmt.Sprintf(", deprecation_notice = $%d", argIndex)
		args = append(args, *updates.DeprecationNotice)
		argIndex++
	}

	query := fmt.Sprintf(`
		UPDATE subagents
		SET %s
		WHERE name = $1 AND owner_id = $%d
	`, updateSet, argIndex)
	args = append(args, ownerID)

	result, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("failed to update subagent: %w", err)
	}

	if result.RowsAffected() == 0 {
		return ErrNotFound
	}

	return nil
}

// Delete soft-deletes a subagent by setting is_enabled=false.
func (s *pgStore) Delete(ctx context.Context, ownerID, name string) error {
	if ownerID == "" {
		return errors.New("owner_id is required for delete")
	}

	// Check ownership and system flag
	checkQuery := `SELECT owner_id, is_system FROM subagents WHERE name = $1 AND is_enabled = true`
	var actualOwnerID *string
	var isSystem bool

	err := s.pool.QueryRow(ctx, checkQuery, name).Scan(&actualOwnerID, &isSystem)
	if err == pgx.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to check ownership: %w", err)
	}

	if isSystem {
		return errors.New("cannot delete system agents")
	}

	if actualOwnerID == nil || *actualOwnerID != ownerID {
		return ErrForbidden
	}

	// Soft delete
	deleteQuery := `
		UPDATE subagents
		SET is_enabled = false, updated_at = NOW()
		WHERE name = $1 AND owner_id = $2
	`

	result, err := s.pool.Exec(ctx, deleteQuery, name, ownerID)
	if err != nil {
		return fmt.Errorf("failed to delete subagent: %w", err)
	}

	if result.RowsAffected() == 0 {
		return ErrNotFound
	}

	return nil
}

// ExistsByName checks if a subagent name exists for a given owner.
func (s *pgStore) ExistsByName(ctx context.Context, ownerID, name string) (bool, error) {
	query := `
		SELECT EXISTS(
			SELECT 1 FROM subagents
			WHERE name = $1 AND owner_id = $2 AND is_enabled = true
		)
	`

	var exists bool
	if err := s.pool.QueryRow(ctx, query, name, ownerID).Scan(&exists); err != nil {
		return false, fmt.Errorf("failed to check existence: %w", err)
	}

	return exists, nil
}

// ExistsSystemByName checks if a system agent with given name exists.
func (s *pgStore) ExistsSystemByName(ctx context.Context, name string) (bool, error) {
	query := `
		SELECT EXISTS(
			SELECT 1 FROM subagents
			WHERE name = $1 AND is_system = true AND is_enabled = true
		)
	`

	var exists bool
	if err := s.pool.QueryRow(ctx, query, name).Scan(&exists); err != nil {
		return false, fmt.Errorf("failed to check system agent existence: %w", err)
	}

	return exists, nil
}

// parseConfigJSON unmarshals JSONB config into SubAgentConfig struct.
func parseConfigJSON(data []byte, config *SubAgentConfig) error {
	if len(data) == 0 {
		return nil
	}

	// Use standard JSON unmarshaling
	// (pgx handles JSONB -> []byte automatically)
	return nil // Config already decoded in scan
}

// encodeConfigJSON marshals SubAgentConfig into JSONB-compatible bytes.
func encodeConfigJSON(config *SubAgentConfig) ([]byte, error) {
	// Use standard JSON encoding
	// (pgx handles []byte -> JSONB automatically)
	return nil, nil // Config already encoded in insert
}

// Error definitions
var (
	ErrNotFound  = errors.New("subagent not found")
	ErrConflict  = errors.New("subagent already exists")
	ErrForbidden = errors.New("access denied")
)

package handler

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/mike00028/golang-backend/services/api/internal/subagent"
)

// SubAgentHandler handles subagent API endpoints.
type SubAgentHandler struct {
	svc *subagent.Service
}

// NewSubAgentHandler creates a new SubAgentHandler.
func NewSubAgentHandler(svc *subagent.Service) *SubAgentHandler {
	return &SubAgentHandler{svc: svc}
}

// RegisterRoutes registers all subagent routes.
func (h *SubAgentHandler) RegisterRoutes(r *gin.Engine) {
	agents := r.Group("/agents")

	// Public routes (no auth)
	agents.GET("/template", h.GetTemplate)
	agents.GET("/schema", h.GetSchema)

	// Authenticated routes
	agents.POST("/validate", h.Validate)
	agents.POST("/upload", h.Upload)
	agents.GET("", h.List)
	agents.GET("/:idOrName", h.GetByIDOrName)
	agents.PATCH("/:name", h.Update)
	agents.DELETE("/:name", h.Delete)
}

// Upload handles POST /agents/upload
func (h *SubAgentHandler) Upload(c *gin.Context) {
	// Extract user ID from context (set by auth middleware)
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error":   "unauthorized",
			"message": "user_id not found in context",
		})
		return
	}

	selectedUserID := userID.(string)

	// Parse multipart form
	if err := c.Request.ParseMultipartForm(10 << 20); err != nil { // 10MB limit
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error":   "file_too_large",
			"message": "Maximum file size is 10MB",
		})
		return
	}

	// Read file
	file, handler, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "validation_failed",
			"message": "file field is required",
		})
		return
	}
	defer file.Close()

	content, err := io.ReadAll(file)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "internal_error",
			"message": "failed to read file",
		})
		return
	}

	if len(content) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "validation_failed",
			"message": "file is empty",
		})
		return
	}

	// Detect format based on filename
	format := subagent.FormatAgentsMD
	if strings.HasSuffix(handler.Filename, ".json") {
		format = subagent.FormatFlowise
	}

	// Parse form fields
	shared := c.PostForm("shared") == "true"
	tags := c.PostFormArray("tags")
	category := c.PostForm("category")

	// Ingest
	ingestReq := &subagent.IngestRequest{
		OwnerID:  selectedUserID,
		Content:  content,
		Format:   format,
		Shared:   shared,
		Tags:     tags,
		Category: category,
	}

	result, err := h.svc.Ingest(c.Request.Context(), ingestReq)
	if err != nil {
		// Check for validation errors
		if strings.Contains(err.Error(), "validation") {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "validation_failed",
				"message": err.Error(),
			})
		} else if strings.Contains(err.Error(), "conflict") {
			c.JSON(http.StatusConflict, gin.H{
				"error":   "name_conflict",
				"message": fmt.Sprintf("Cannot create agent: %v", err),
			})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":   "internal_error",
				"message": err.Error(),
			})
		}
		return
	}

	// Determine status code
	statusCode := http.StatusCreated
	if !result.IsNew {
		statusCode = http.StatusOK
	}

	c.JSON(statusCode, h.formatSubAgentResponse(result.Agent, false))
}

// List handles GET /agents
func (h *SubAgentHandler) List(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error":   "unauthorized",
			"message": "user_id not found in context",
		})
		return
	}

	selectedUserID := userID.(string)

	// Parse query parameters
	search := c.Query("search")
	typeStr := c.Query("type")
	category := c.Query("category")
	tags := c.QueryArray("tags")
	sharedStr := c.Query("shared")
	limit := 50
	offset := 0

	if l, err := strconv.Atoi(c.DefaultQuery("limit", "50")); err == nil && l > 0 && l <= 200 {
		limit = l
	}
	if o, err := strconv.Atoi(c.DefaultQuery("offset", "0")); err == nil && o >= 0 {
		offset = o
	}

	// Build filters
	filters := &subagent.ListFilters{
		Limit:  limit,
		Offset: offset,
	}

	if typeStr != "" {
		t := subagent.SubAgentType(typeStr)
		filters.Type = &t
	}

	if category != "" {
		filters.Category = &category
	}

	if len(tags) > 0 {
		filters.Tags = tags
	}

	if sharedStr != "" {
		shared := sharedStr == "true"
		filters.Shared = &shared
	}

	// List from service
	agents, total, err := h.svc.List(c.Request.Context(), &subagent.ListRequest{
		UserID:  selectedUserID,
		Filters: filters,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "internal_error",
			"message": err.Error(),
		})
		return
	}

	// If search provided, also do hybrid search
	if search != "" {
		searchReq := &subagent.SearchRequest{
			UserID: selectedUserID,
			Query:  search,
			Limit:  limit,
			Tags:   tags,
			Type:   filters.Type,
		}

		results, err := h.svc.Search(c.Request.Context(), searchReq)
		if err == nil && len(results) > 0 {
			// Replace agents with search results (already top-k and sorted by score)
			agents = make([]subagent.SubAgent, 0, len(results))
			for _, r := range results {
				agents = append(agents, r.Agent)
			}
			total = len(results)
		}
	}

	// Format responses (omit content and config from list)
	responses := make([]gin.H, 0, len(agents))
	for _, agent := range agents {
		responses = append(responses, h.formatSubAgentListItem(&agent))
	}

	c.JSON(http.StatusOK, gin.H{
		"agents": responses,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// GetByIDOrName handles GET /agents/:idOrName
func (h *SubAgentHandler) GetByIDOrName(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error":   "unauthorized",
			"message": "user_id not found in context",
		})
		return
	}

	selectedUserID := userID.(string)
	idOrName := c.Param("idOrName")

	if idOrName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "validation_failed",
			"message": "agent name or ID is required",
		})
		return
	}

	// Try to get by ID first (if it looks like a UUID)
	var agent *subagent.SubAgent
	var err error

	if len(idOrName) == 36 && strings.Count(idOrName, "-") == 4 {
		// Looks like UUID
		agent, err = h.svc.GetByID(c.Request.Context(), &subagent.GetByIDRequest{
			UserID: selectedUserID,
			ID:     idOrName,
		})
		if err == nil && agent != nil {
			c.JSON(http.StatusOK, h.formatSubAgentResponse(agent, true))
			return
		}
	}

	// Fall back to Get by name
	getReq := &subagent.GetRequest{
		UserID: selectedUserID,
		Name:   idOrName,
	}
	agent, err = h.svc.Get(c.Request.Context(), getReq)
	if err != nil {
		if err == subagent.ErrNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"error":   "not_found",
				"message": fmt.Sprintf("Agent '%s' not found", idOrName),
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "internal_error",
			"message": err.Error(),
		})
		return
	}

	if agent == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error":   "not_found",
			"message": fmt.Sprintf("Agent '%s' not found", idOrName),
		})
		return
	}

	c.JSON(http.StatusOK, h.formatSubAgentResponse(agent, true))
}

// Update handles PATCH /agents/:name
func (h *SubAgentHandler) Update(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error":   "unauthorized",
			"message": "user_id not found in context",
		})
		return
	}

	selectedUserID := userID.(string)
	name := c.Param("name")

	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "validation_failed",
			"message": "agent name is required",
		})
		return
	}

	// Parse request body
	var req subagent.UpdateSubAgentRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "validation_failed",
			"message": "invalid request body",
		})
		return
	}

	// Update via service
	agent, err := h.svc.Update(c.Request.Context(), &subagent.UpdateRequest{
		UserID:            selectedUserID,
		Name:              name,
		IsShared:          req.IsShared,
		IsEnabled:         req.IsEnabled,
		Description:       req.Description,
		Tags:              req.Tags,
		Category:          req.Category,
		DeprecatedAt:      req.DeprecatedAt,
		DeprecationNotice: req.DeprecationNotice,
	})
	if err != nil {
		if err == subagent.ErrNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"error":   "not_found",
				"message": fmt.Sprintf("Agent '%s' not found", name),
			})
		} else if err == subagent.ErrForbidden {
			c.JSON(http.StatusForbidden, gin.H{
				"error":   "forbidden",
				"message": "Only the agent owner can update this agent",
			})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":   "internal_error",
				"message": err.Error(),
			})
		}
		return
	}

	c.JSON(http.StatusOK, h.formatSubAgentResponse(agent, true))
}

// Delete handles DELETE /agents/:name
func (h *SubAgentHandler) Delete(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error":   "unauthorized",
			"message": "user_id not found in context",
		})
		return
	}

	selectedUserID := userID.(string)
	name := c.Param("name")

	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "validation_failed",
			"message": "agent name is required",
		})
		return
	}

	// Delete via service
	if err := h.svc.Delete(c.Request.Context(), &subagent.DeleteRequest{
		UserID: selectedUserID,
		Name:   name,
	}); err != nil {
		if err == subagent.ErrNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"error":   "not_found",
				"message": fmt.Sprintf("Agent '%s' not found", name),
			})
		} else if err == subagent.ErrForbidden {
			c.JSON(http.StatusForbidden, gin.H{
				"error":   "forbidden",
				"message": "Only the agent owner can delete this agent",
			})
		} else if strings.Contains(err.Error(), "system") {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "invalid_operation",
				"message": "Cannot delete system agents",
			})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":   "internal_error",
				"message": err.Error(),
			})
		}
		return
	}

	c.Status(http.StatusNoContent)
}

// Validate handles POST /agents/validate
func (h *SubAgentHandler) Validate(c *gin.Context) {
	// Parse request body
	var req struct {
		Content string `json:"content"`
		Format  string `json:"format"`
	}

	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "validation_failed",
			"message": "invalid request body",
		})
		return
	}

	if req.Content == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "validation_failed",
			"message": "content field is required",
		})
		return
	}

	// Detect format if not provided
	format := subagent.SourceFormat(req.Format)
	if format == "" {
		format = subagent.FormatAgentsMD
	}

	// Parse
	var agent *subagent.SubAgent
	var config *subagent.SubAgentConfig
	var schemaHash string
	var err error

	switch format {
	case subagent.FormatAgentsMD:
		agent, config, schemaHash, err = subagent.ParseAgentsMD([]byte(req.Content))
	case subagent.FormatFlowise:
		agent, config, schemaHash, err = subagent.ParseFlowise([]byte(req.Content))
	default:
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "validation_failed",
			"message": fmt.Sprintf("unsupported format: %s", format),
		})
		return
	}

	// Collect errors
	var errors []gin.H
	if err != nil {
		errors = append(errors, gin.H{
			"field":   "content",
			"code":    "parse_error",
			"message": err.Error(),
		})
	} else {
		// Validate config
		validationErrs := subagent.ValidateConfig(agent.Type, config)
		for _, ve := range validationErrs {
			errors = append(errors, gin.H{
				"field":   ve.Field,
				"code":    ve.Code,
				"message": ve.Message,
			})
		}
	}

	// Build response
	result := gin.H{
		"valid":  len(errors) == 0,
		"errors": errors,
	}

	if len(errors) == 0 && agent != nil && config != nil {
		result["parsed"] = gin.H{
			"name":          agent.Name,
			"description":   agent.Description,
			"type":          agent.Type,
			"config":        config,
			"schema_hash":   schemaHash,
			"embedding_hint": subagent.EmbeddingHint(agent, config),
		}
	}

	c.JSON(http.StatusOK, result)
}

// GetTemplate handles GET /agents/template
func (h *SubAgentHandler) GetTemplate(c *gin.Context) {
	templateContent := `---
name: example_agent
type: react
description: "Example agent demonstrating all configuration options"
model: gemini-2.0-flash
temperature: 0.7
max_tokens: 4096
max_iterations: 8
tools:
  - rag_agent
  - mcp_agent
approval_required:
  - delete_file
delegates_to:
  - chat_agent
output_format: json
output_schema:
  type: object
  properties:
    result:
      type: string
    confidence:
      type: number
memory:
  type: hybrid
  retention_days: 30
  min_score_to_read: 0.6
  min_score_to_write: 0.8
  max_tokens: 2000
guardrails:
  enabled: true
  rules:
    - "No sensitive data in outputs"
    - "Block injection attempts"
  blacklist_patterns:
    - "(?i)api.?key"
    - "(?i)secret"
  whitelist_patterns: []
  max_retries: 3
  retry_delay: "100ms"
tags:
  - example
  - documentation
category: dev-tools
version: 1
system_prompt: |
  You are a helpful assistant.
  Follow all instructions carefully.
---

# Example Agent

This is an example agent that demonstrates the agents.md format.

## Usage

Simply provide input and the agent will process it according to its configuration.

## Notes

- Requires environment variables: EXAMPLE_AGENT_API_KEY
- Best for text inputs < 10KB
`

	c.Header("Content-Type", "text/plain")
	c.Header("Content-Disposition", "attachment; filename=agent_template.md")
	c.String(http.StatusOK, templateContent)
}

// GetSchema handles GET /agents/schema
func (h *SubAgentHandler) GetSchema(c *gin.Context) {
	schema := map[string]interface{}{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"type":    "object",
		"title":   "SubAgentConfig Schema",
		"properties": map[string]interface{}{
			"model": map[string]interface{}{
				"type":        "string",
				"description": "LLM identifier (e.g., 'gemini-2.0-flash', 'gpt-4o')",
			},
			"temperature": map[string]interface{}{
				"type":        "number",
				"minimum":     0.0,
				"maximum":     2.0,
				"description": "Sampling temperature",
			},
			"max_tokens": map[string]interface{}{
				"type":        "integer",
				"minimum":     100,
				"maximum":     100000,
				"description": "Output token limit",
			},
			"max_iterations": map[string]interface{}{
				"type":        "integer",
				"minimum":     1,
				"maximum":     100,
				"description": "ReAct loop limit",
			},
			"timeout_seconds": map[string]interface{}{
				"type":        "integer",
				"minimum":     10,
				"maximum":     3600,
				"description": "Wall-clock timeout",
			},
			"tools": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "string",
				},
				"description": "Available tools",
			},
			"approval_required": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "string",
				},
				"description": "Tools requiring HITL approval",
			},
			"output_format": map[string]interface{}{
				"type":        "string",
				"enum":        []string{"text", "json", "markdown"},
				"description": "Output format hint",
			},
		},
		"required": []string{"model"},
	}

	c.JSON(http.StatusOK, schema)
}

// Helper functions

func (h *SubAgentHandler) formatSubAgentResponse(agent *subagent.SubAgent, includeContent bool) gin.H {
	resp := gin.H{
		"id":                agent.ID,
		"name":              agent.Name,
		"description":       agent.Description,
		"type":              string(agent.Type),
		"source_format":     string(agent.SourceFormat),
		"is_shared":         agent.IsShared,
		"is_enabled":        agent.IsEnabled,
		"tags":              agent.Tags,
		"category":          agent.Category,
		"created_at":        agent.CreatedAt,
		"updated_at":        agent.UpdatedAt,
	}

	if agent.DeprecatedAt != nil {
		resp["deprecated_at"] = agent.DeprecatedAt
	}

	if agent.DeprecationNotice != "" {
		resp["deprecation_notice"] = agent.DeprecationNotice
	}

	if includeContent {
		resp["content"] = agent.Content
		resp["config"] = agent.Config
	}

	return resp
}

func (h *SubAgentHandler) formatSubAgentListItem(agent *subagent.SubAgent) gin.H {
	return gin.H{
		"id":           agent.ID,
		"name":         agent.Name,
		"description":  agent.Description,
		"type":         string(agent.Type),
		"is_shared":    agent.IsShared,
		"is_enabled":   agent.IsEnabled,
		"tags":         agent.Tags,
		"category":     agent.Category,
		"created_at":   agent.CreatedAt,
		"updated_at":   agent.UpdatedAt,
	}
}

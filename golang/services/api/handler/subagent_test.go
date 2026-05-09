package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/mike00028/golang-backend/services/api/internal/subagent"
)

// MockSubAgentService implements subagent.Service for testing
type MockSubAgentService struct {
	agents map[string]*subagent.SubAgent
}

func NewMockSubAgentService() *MockSubAgentService {
	return &MockSubAgentService{
		agents: make(map[string]*subagent.SubAgent),
	}
}

func (m *MockSubAgentService) Ingest(ctx interface{}, req interface{}) (*subagent.IngestResult, error) {
	// Mock: return success
	return &subagent.IngestResult{
		Agent: &subagent.SubAgent{
			Name: "test_agent",
			Type: subagent.TypeSimple,
		},
		IsNew: true,
	}, nil
}

func (m *MockSubAgentService) Get(ctx interface{}, req interface{}) (*subagent.SubAgent, error) {
	return &subagent.SubAgent{
		Name: "test_agent",
		Type: subagent.TypeSimple,
	}, nil
}

func (m *MockSubAgentService) List(ctx interface{}, req interface{}) ([]subagent.SubAgent, int, error) {
	return []subagent.SubAgent{
		{
			Name: "agent1",
			Type: subagent.TypeSimple,
		},
	}, 1, nil
}

func (m *MockSubAgentService) Discover(ctx interface{}, req interface{}) (*subagent.DiscoveryResult, error) {
	return &subagent.DiscoveryResult{
		SubAgents: []subagent.SubAgentSummary{
			{
				Name:        "discovered_agent",
				Type:        "react",
				Description: "Test discovered agent",
			},
		},
	}, nil
}

// TestUploadEndpoint_ValidFile tests uploading valid agent file
func TestUploadEndpoint_ValidFile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	mockService := NewMockSubAgentService()
	handler := NewSubAgentHandler(mockService)

	router.POST("/agents/upload", handler.Upload)

	// Create multipart form with agent file
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)

	// Add file
	part, _ := writer.CreateFormFile("file", "agent.md")
	part.Write([]byte(`---
name: test_agent
type: simple
---
Test agent`))

	// Add fields
	writer.WriteField("shared", "true")
	writer.WriteField("category", "test")
	writer.Close()

	req := httptest.NewRequest("POST", "/agents/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("user_id", "user1")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Errorf("expected 200/201, got %d: %s", w.Code, w.Body.String())
	}
}

// TestListEndpoint returns agents
func TestListEndpoint_ReturnsAgents(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	mockService := NewMockSubAgentService()
	handler := NewSubAgentHandler(mockService)

	router.GET("/agents", handler.List)

	req := httptest.NewRequest("GET", "/agents?limit=10&offset=0", nil)
	req.Header.Set("user_id", "user1")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)

	if _, ok := resp["agents"]; !ok {
		t.Error("expected 'agents' in response")
	}
}

// TestGetByIDOrNameEndpoint retrieves agent
func TestGetByIDOrNameEndpoint_RetrievesAgent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	mockService := NewMockSubAgentService()
	handler := NewSubAgentHandler(mockService)

	router.GET("/agents/:idOrName", handler.GetByIDOrName)

	req := httptest.NewRequest("GET", "/agents/test_agent", nil)
	req.Header.Set("user_id", "user1")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// TestDeleteEndpoint removes agent
func TestDeleteEndpoint_RemovesAgent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	mockService := NewMockSubAgentService()
	handler := NewSubAgentHandler(mockService)

	router.DELETE("/agents/:name", handler.Delete)

	req := httptest.NewRequest("DELETE", "/agents/test_agent", nil)
	req.Header.Set("user_id", "user1")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should return 204 No Content on deletion
	if w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Errorf("expected 204 or 200, got %d", w.Code)
	}
}

// TestValidateEndpoint validates without uploading
func TestValidateEndpoint_ValidatesConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	mockService := NewMockSubAgentService()
	handler := NewSubAgentHandler(mockService)

	router.POST("/agents/validate", handler.Validate)

	payload := map[string]string{
		"content": `---
name: test_agent
type: simple
---
Test`,
		"format": "agents.md",
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest("POST", "/agents/validate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)

	if _, ok := resp["valid"]; !ok {
		t.Error("expected 'valid' in response")
	}
}

// TestTemplateEndpoint returns template
func TestTemplateEndpoint_ReturnsTemplate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	mockService := NewMockSubAgentService()
	handler := NewSubAgentHandler(mockService)

	router.GET("/agents/template", handler.GetTemplate)

	req := httptest.NewRequest("GET", "/agents/template", nil)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	// Should be text/plain containing YAML
	if !bytes.Contains(w.Body.Bytes(), []byte("---")) {
		t.Error("expected YAML template")
	}
}

// TestSchemaEndpoint returns JSON schema
func TestSchemaEndpoint_ReturnsSchema(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	mockService := NewMockSubAgentService()
	handler := NewSubAgentHandler(mockService)

	router.GET("/agents/schema", handler.GetSchema)

	req := httptest.NewRequest("GET", "/agents/schema", nil)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	// Should be valid JSON
	var schema map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &schema)
	if err != nil {
		t.Errorf("expected valid JSON schema: %v", err)
	}
}

// TestUploadEndpoint_FileTooLarge rejects large files
func TestUploadEndpoint_FileTooLarge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	mockService := NewMockSubAgentService()
	handler := NewSubAgentHandler(mockService)

	router.POST("/agents/upload", handler.Upload)

	// Create large file (>10MB)
	largeContent := make([]byte, 11*1024*1024) // 11MB

	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "large.md")
	io.WriteString(part, string(largeContent))
	writer.Close()

	req := httptest.NewRequest("POST", "/agents/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("user_id", "user1")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should reject with 413 or similar error
	if w.Code < http.StatusBadRequest {
		t.Errorf("expected error status, got %d", w.Code)
	}
}

// TestListEndpoint_WithFilters applies query filters
func TestListEndpoint_WithFilters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	mockService := NewMockSubAgentService()
	handler := NewSubAgentHandler(mockService)

	router.GET("/agents", handler.List)

	req := httptest.NewRequest("GET", "/agents?type=react&category=security&shared=true", nil)
	req.Header.Set("user_id", "user1")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// TestUpdateEndpoint updates metadata
func TestUpdateEndpoint_UpdatesMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	mockService := NewMockSubAgentService()
	handler := NewSubAgentHandler(mockService)

	router.PATCH("/agents/:name", handler.Update)

	updates := map[string]interface{}{
		"description": "Updated description",
		"is_shared":   true,
		"tags":        []string{"new", "tags"},
	}
	body, _ := json.Marshal(updates)

	req := httptest.NewRequest("PATCH", "/agents/test_agent", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("user_id", "user1")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

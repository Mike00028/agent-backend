package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/mike00028/golang-backend/services/api/internal/hitl"
)

func TestHealth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/healthz", nil)

	Health(c)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), "ok") {
		t.Errorf("body = %q, want 'ok'", w.Body.String())
	}
}

func TestNewApproveHandler_NotNil(t *testing.T) {
	store := hitl.NewStore()
	h := NewApproveHandler(store)
	if h == nil {
		t.Fatal("expected non-nil ApproveHandler")
	}
}

func TestApprove_BadJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/agent/approve", strings.NewReader(`{bad json}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h := NewApproveHandler(hitl.NewStore())
	h.Approve(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestApprove_NotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"session_id":"s1","task_id":"t1","approved":true}`
	c.Request = httptest.NewRequest(http.MethodPost, "/agent/approve", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h := NewApproveHandler(hitl.NewStore()) // empty store — no pending request
	h.Approve(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

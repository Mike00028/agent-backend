package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/mike00028/golang-backend/services/api/internal/hitl"
)

type approveRequest struct {
	SessionID string `json:"session_id" binding:"required"`
	TaskID    string `json:"task_id"    binding:"required"`
	Approved  bool   `json:"approved"`
	Reason    string `json:"reason"`
}

// ApproveHandler resolves pending HITL approval requests.
type ApproveHandler struct {
	store *hitl.Store
}

// NewApproveHandler creates an ApproveHandler backed by store.
func NewApproveHandler(store *hitl.Store) *ApproveHandler {
	return &ApproveHandler{store: store}
}

// Approve handles POST /agent/approve.
//
// Request body:
//
//	{
//	  "session_id": "abc-123",
//	  "task_id":    "t2",
//	  "approved":   true,
//	  "reason":     ""          // optional; required when approved=false
//	}
//
// The call blocks until the waiting executor goroutine drains the channel
// (<1 ms), then returns 200 OK or a 4xx/5xx error.
func (h *ApproveHandler) Approve(c *gin.Context) {
	var req approveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	err := h.store.Respond(req.SessionID, req.TaskID, hitl.ApprovalResult{
		Approved: req.Approved,
		Reason:   req.Reason,
	})
	if errors.Is(err, hitl.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "no pending approval for this session/task"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "approved": req.Approved})
}

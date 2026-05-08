package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/mike00028/golang-backend/services/api/internal/apperror"
	pkgsse "github.com/mike00028/golang-backend/services/api/internal/sse"
)

// ── evalMathExpr ──────────────────────────────────────────────────────────────

func TestEvalMathExpr(t *testing.T) {
	cases := []struct {
		expr    string
		want    string
		wantErr bool
	}{
		{"1 + 2", "3", false},
		{"10 - 4", "6", false},
		{"3 * 7", "21", false},
		{"10 / 4", "2.5", false},
		{"9 / 3", "3", false},
		{"-5 + 3", "-2", false},
		{"0.5 + 0.5", "1", false},
		{"10 / 0", "", true},      // division by zero
		{"not an expr", "", true}, // unparseable
		{"", "", true},            // empty
		{"1 + 2 + 3", "", true},   // too many operands
	}
	for _, tc := range cases {
		got, err := evalMathExpr(tc.expr)
		if tc.wantErr {
			if err == nil {
				t.Errorf("evalMathExpr(%q): expected error, got %q", tc.expr, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("evalMathExpr(%q): unexpected error: %v", tc.expr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("evalMathExpr(%q) = %q, want %q", tc.expr, got, tc.want)
		}
	}
}

// ── resolveFromDepResults ─────────────────────────────────────────────────────

func TestResolveFromDepResults(t *testing.T) {
	cases := []struct {
		name string
		expr string
		deps map[string]string
		want string
	}{
		{
			name: "no deps — passthrough",
			expr: "t1 + 5",
			deps: nil,
			want: "t1 + 5",
		},
		{
			name: "curly brace substitution",
			expr: "{t1} + {t2}",
			deps: map[string]string{"t1": "10", "t2": "20"},
			want: "10 + 20",
		},
		{
			name: "bare word substitution",
			expr: "t1 + 56",
			deps: map[string]string{"t1": "44"},
			want: "44 + 56",
		},
		{
			name: "dep output with extra text",
			expr: "{t1} * 2",
			deps: map[string]string{"t1": "The answer is 7 things"},
			want: "7 * 2",
		},
		{
			name: "negative number in dep",
			expr: "{t1} + 10",
			deps: map[string]string{"t1": "-3"},
			want: "-3 + 10",
		},
		{
			name: "unknown dep key — leave as-is",
			expr: "{t99} + 1",
			deps: map[string]string{"t1": "5"},
			want: "{t99} + 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveFromDepResults(tc.expr, tc.deps)
			if got != tc.want {
				t.Errorf("resolveFromDepResults(%q, %v) = %q, want %q", tc.expr, tc.deps, got, tc.want)
			}
		})
	}
}

// ── httpError ─────────────────────────────────────────────────────────────────

func TestHTTPError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	ae := apperror.New(apperror.CodeAgentNotFound, "agent not found", http.StatusNotFound)
	httpError(c, ae)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	var body map[string]string
	json.NewDecoder(w.Body).Decode(&body) //nolint:errcheck
	if body["code"] != string(apperror.CodeAgentNotFound) {
		t.Errorf("code = %q, want %q", body["code"], apperror.CodeAgentNotFound)
	}
	if body["message"] != "agent not found" {
		t.Errorf("message = %q, want %q", body["message"], "agent not found")
	}
}

// ── sseError ──────────────────────────────────────────────────────────────────

func TestSSEError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	// Set headers SSE writer requires
	w.Header().Set("Content-Type", "text/event-stream")
	sse, ok := pkgsse.New(w)
	if !ok {
		t.Fatal("httptest.ResponseRecorder does not implement http.Flusher — cannot test SSE")
	}

	ae := apperror.New(apperror.CodeTaskFailed, "task failed", http.StatusInternalServerError)
	sseError(sse, ae)

	body := w.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Errorf("SSE frame missing 'event: error'; got:\n%s", body)
	}
	if !strings.Contains(body, string(apperror.CodeTaskFailed)) {
		t.Errorf("SSE frame missing error code %q; got:\n%s", apperror.CodeTaskFailed, body)
	}
	if !strings.Contains(body, "task failed") {
		t.Errorf("SSE frame missing message 'task failed'; got:\n%s", body)
	}
}

// ── normalise ─────────────────────────────────────────────────────────────────

func TestNormalise_DefaultsSessionAndAgent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/chat", nil)

	h := &ChatHandler{}
	req := &chatRequest{}
	h.normalise(c, req)

	if req.SessionID == "" {
		t.Error("expected SessionID to be generated, got empty string")
	}
	if req.AgentID != "default" {
		t.Errorf("AgentID = %q, want %q", req.AgentID, "default")
	}
}

func TestNormalise_PreservesExistingValues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/chat", nil)

	h := &ChatHandler{}
	req := &chatRequest{SessionID: "sess-abc", AgentID: "my-agent"}
	h.normalise(c, req)

	if req.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want %q", req.SessionID, "sess-abc")
	}
	if req.AgentID != "my-agent" {
		t.Errorf("AgentID = %q, want %q", req.AgentID, "my-agent")
	}
}

func TestNormalise_UsesXSessionIDHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	req.Header.Set("X-Session-ID", "header-session")
	c.Request = req

	h := &ChatHandler{}
	cr := &chatRequest{}
	h.normalise(c, cr)

	if cr.SessionID != "header-session" {
		t.Errorf("SessionID = %q, want %q", cr.SessionID, "header-session")
	}
}

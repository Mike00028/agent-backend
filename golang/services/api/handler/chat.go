package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/mike00028/golang-backend/services/api/internal/grpcclient"
	langgraphv1 "github.com/mike00028/golang-backend/services/api/internal/langgraphv1/langgraph/v1"
	pkgsse "github.com/mike00028/golang-backend/services/api/internal/sse"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

type chatRequest struct {
	Message   string                 `json:"message" binding:"required"`
	SessionID string                 `json:"session_id"`
	Model     string                 `json:"model"` // optional: defaults to "default-chat"
	Options   map[string]interface{} `json:"options"`
}

// ChatHandler handles both streaming SSE (GET) and unary JSON (POST /agent/invoke).
type ChatHandler struct {
	pool *grpcclient.Pool
}

// NewChatHandler creates a ChatHandler backed by the given connection pool.
func NewChatHandler(pool *grpcclient.Pool) *ChatHandler {
	return &ChatHandler{pool: pool}
}

// streamEventJSON wraps an AgentEvent into a JSON object for SSE transmission.
type streamEventJSON struct {
	EventType  string          `json:"event_type"`
	Text       string          `json:"text,omitempty"`
	Error      string          `json:"error,omitempty"`
	ToolCall   *toolCallJSON   `json:"tool_call,omitempty"`
	ToolResult *toolResultJSON `json:"tool_result,omitempty"`
	Message    string          `json:"message,omitempty"`
}

type toolCallJSON struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	ArgsJSON json.RawMessage `json:"args_json"`
}

type toolResultJSON struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
}

// Stream handles POST /chat — streams LangGraph events as Server-Sent Events.
func (h *ChatHandler) Stream(c *gin.Context) {
	var req chatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.SessionID == "" {
		req.SessionID = c.GetHeader("X-Session-ID")
	}

	sse, ok := pkgsse.New(c.Writer)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming not supported"})
		return
	}

	client := langgraphv1.NewAgentServiceClient(h.pool.Next())

	// Convert options map to protobuf Struct
	var optionsStruct *structpb.Struct
	if req.Options != nil {
		optionsStruct, _ = structpb.NewStruct(req.Options)
	}

	// Set default model if not provided
	model := req.Model
	if model == "" {
		model = "default-chat"
	}

	stream, err := client.StreamAgent(c.Request.Context(), &langgraphv1.AgentRequest{
		SessionId: req.SessionID,
		Message:   req.Message,
		Model:     model,
		Options:   optionsStruct,
	})
	if err != nil {
		sse.SendEvent("error", status.Convert(err).Message())
		return
	}

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			sse.SendEvent("done", "[STREAM_END]")
			return
		}
		if err != nil {
			sse.SendEvent("error", status.Convert(err).Message())
			return
		}

		// Convert protobuf event to JSON and send over SSE
		sseEvent := h.eventToJSON(event)
		data, _ := json.Marshal(sseEvent)
		sse.Send(string(data))
	}
}

// eventToJSON converts a protobuf AgentEvent to a JSON-serializable struct.
func (h *ChatHandler) eventToJSON(event *langgraphv1.AgentEvent) streamEventJSON {
	result := streamEventJSON{EventType: event.EventType}

	switch c := event.Content.(type) {
	case *langgraphv1.AgentEvent_Text:
		result.Text = c.Text
	case *langgraphv1.AgentEvent_Error:
		result.Error = c.Error
	case *langgraphv1.AgentEvent_Message:
		result.Message = c.Message
	case *langgraphv1.AgentEvent_ToolCall:
		if c.ToolCall != nil {
			result.ToolCall = &toolCallJSON{
				ID:       c.ToolCall.Id,
				Name:     c.ToolCall.Name,
				ArgsJSON: json.RawMessage(c.ToolCall.ArgsJson),
			}
		}
	case *langgraphv1.AgentEvent_ToolResult:
		if c.ToolResult != nil {
			result.ToolResult = &toolResultJSON{
				ToolCallID: c.ToolResult.ToolCallId,
				Content:    c.ToolResult.Content,
			}
		}
	}
	return result
}

// Invoke handles POST /agent/invoke — unary, returns full response as JSON.
func (h *ChatHandler) Invoke(c *gin.Context) {
	var req chatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*1e9) // 30s
	defer cancel()

	client := langgraphv1.NewAgentServiceClient(h.pool.Next())

	// Convert options map to protobuf Struct
	var optionsStruct *structpb.Struct
	if req.Options != nil {
		optionsStruct, _ = structpb.NewStruct(req.Options)
	}

	// Set default model if not provided
	model := req.Model
	if model == "" {
		model = "default-chat"
	}

	resp, err := client.RunAgent(ctx, &langgraphv1.AgentRequest{
		SessionId: req.SessionID,
		Message:   req.Message,
		Model:     model,
		Options:   optionsStruct,
	})

	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": status.Convert(err).Message()})
		return
	}

	switch r := resp.Result.(type) {
	case *langgraphv1.AgentResponse_Text:
		responseBody := gin.H{
			"result": r.Text,
		}
		if resp.Metadata != nil {
			responseBody["metadata"] = gin.H{
				"session_id":        resp.Metadata.SessionId,
				"model":             resp.Metadata.Model,
				"tool_calls_count":  resp.Metadata.ToolCallsCount,
				"execution_time_ms": resp.Metadata.ExecutionTimeMs,
			}
		}
		c.JSON(http.StatusOK, responseBody)
	case *langgraphv1.AgentResponse_Error:
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": r.Error})
	}
}

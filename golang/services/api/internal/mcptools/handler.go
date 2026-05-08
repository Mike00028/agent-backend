package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mike00028/golang-backend/services/api/internal/dag"
)

// RegisterMCPHandler registers a DAG local handler that:
// 1. Searches for matching MCP tools based on the task's question/args
// 2. Calls the selected tool on its MCP server
// 3. Returns the result as the task output
//
// The handler is registered under "mcp_agent" — the planner can emit tasks
// with tool_name="mcp_agent" and args.question describing what tool to invoke,
// or args.tool_name + args.tool_args for direct invocation.
func RegisterMCPHandler(exec *dag.Executor, mgr *Manager) {
	exec.RegisterLocal("mcp_agent", func(ctx context.Context, task *dag.Task) (string, error) {
		var args struct {
			Question string          `json:"question"`
			ToolName string          `json:"tool_name"`
			ToolArgs json.RawMessage `json:"tool_args"`
			Server   string          `json:"server"`
		}
		_ = json.Unmarshal([]byte(task.ArgsJSON), &args)

		// Direct invocation: tool_name + tool_args specified
		if args.ToolName != "" && args.Server != "" {
			return callDirect(ctx, mgr, args.Server, args.ToolName, args.ToolArgs, task)
		}

		// Search-based invocation: find the best matching tool
		query := args.Question
		if query == "" {
			query = task.Title
		}
		if query == "" {
			return "", fmt.Errorf("mcp_agent: no question or tool_name provided")
		}

		tools, err := mgr.SearchTools(ctx, query, 5)
		if err != nil {
			return "", fmt.Errorf("mcp_agent search: %w", err)
		}
		if len(tools) == 0 {
			return "", fmt.Errorf("mcp_agent: no matching tools found for %q", query)
		}

		// Use the top-ranked tool
		tool := tools[0]
		slog.Info("mcp_agent selected tool", "tool", tool.Name, "server", tool.ServerName,
			"query", query)

		// Build tool args from the question
		toolArgs := args.ToolArgs
		if len(toolArgs) == 0 {
			// Wrap the question as the tool input
			toolArgs, _ = json.Marshal(map[string]string{"query": query})
		}

		return callDirect(ctx, mgr, tool.ServerName, tool.Name, toolArgs, task)
	})
}

func callDirect(ctx context.Context, mgr *Manager, server, toolName string, args json.RawMessage, task *dag.Task) (string, error) {
	start := time.Now()

	result, err := mgr.CallTool(ctx, server, toolName, args)
	durationMs := int(time.Since(start).Milliseconds())

	if err != nil {
		// Log the failed call for audit
		_ = mgr.store.LogToolCall(ctx, "", "", task.ID, args, nil, "error", err.Error(), durationMs)
		return "", fmt.Errorf("mcp tool %s/%s: %w", server, toolName, err)
	}

	// Extract text content from result
	var parts []string
	for _, block := range result.Content {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	output := strings.Join(parts, "\n")

	if result.IsError {
		_ = mgr.store.LogToolCall(ctx, "", "", task.ID, args, json.RawMessage(`"`+output+`"`), "error", output, durationMs)
		return "", fmt.Errorf("mcp tool error: %s", output)
	}

	outputJSON, _ := json.Marshal(output)
	_ = mgr.store.LogToolCall(ctx, "", "", task.ID, args, outputJSON, "success", "", durationMs)

	return output, nil
}

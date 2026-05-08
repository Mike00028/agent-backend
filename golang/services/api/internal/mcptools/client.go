package mcptools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Transport identifies how to connect to an MCP server.
type Transport string

const (
	TransportSSE   Transport = "sse"
	TransportStdio Transport = "stdio"
)

// ServerConfig describes one MCP server endpoint.
type ServerConfig struct {
	Name      string    `json:"name"`
	Transport Transport `json:"transport"`
	URL       string    `json:"url,omitempty"`     // SSE endpoint
	Command   string    `json:"command,omitempty"` // stdio command
	Args      []string  `json:"args,omitempty"`    // stdio args
	AuthType  string    `json:"auth_type,omitempty"`
	AuthToken string    `json:"auth_secret,omitempty"`
}

// ToolDef is a tool definition returned by an MCP server.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolCallResult is the result of calling an MCP tool.
type ToolCallResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// ContentBlock is one piece of tool output (text, image, etc).
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// jsonrpcRequest is an MCP JSON-RPC 2.0 request envelope.
type jsonrpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// jsonrpcResponse is a JSON-RPC 2.0 response envelope.
type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Client communicates with an MCP server over SSE or stdio.
type Client struct {
	cfg    ServerConfig
	httpCl *http.Client
	mu     sync.Mutex
	nextID int
	// stdio process handles (nil for SSE)
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

// NewClient creates an MCP client for the given server config.
func NewClient(cfg ServerConfig) *Client {
	return &Client{
		cfg:    cfg,
		httpCl: &http.Client{Timeout: 30 * time.Second},
		nextID: 1,
	}
}

// ListTools asks the MCP server for its available tools.
func (c *Client) ListTools(ctx context.Context) ([]ToolDef, error) {
	resp, err := c.call(ctx, "tools/list", nil)
	if err != nil {
		return nil, fmt.Errorf("mcp list tools: %w", err)
	}
	var result struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("mcp parse tools: %w", err)
	}
	return result.Tools, nil
}

// CallTool invokes a tool on the MCP server with the given arguments.
func (c *Client) CallTool(ctx context.Context, toolName string, args json.RawMessage) (*ToolCallResult, error) {
	params := map[string]interface{}{
		"name": toolName,
	}
	if len(args) > 0 {
		var a interface{}
		if err := json.Unmarshal(args, &a); err == nil {
			params["arguments"] = a
		}
	}
	resp, err := c.call(ctx, "tools/call", params)
	if err != nil {
		return nil, fmt.Errorf("mcp call tool %s: %w", toolName, err)
	}
	var result ToolCallResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("mcp parse tool result: %w", err)
	}
	return &result, nil
}

// Close shuts down a stdio process if one is running.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

// call dispatches a JSON-RPC method to the appropriate transport.
func (c *Client) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	switch c.cfg.Transport {
	case TransportSSE:
		return c.callSSE(ctx, method, params)
	case TransportStdio:
		return c.callStdio(ctx, method, params)
	default:
		return nil, fmt.Errorf("unsupported transport: %s", c.cfg.Transport)
	}
}

// callSSE sends an HTTP POST JSON-RPC request to the SSE endpoint.
// MCP SSE transports typically expose a /message endpoint that accepts
// JSON-RPC over HTTP POST and returns text/event-stream or JSON.
func (c *Client) callSSE(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	c.mu.Unlock()

	req := jsonrpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	// POST to the SSE messages endpoint
	url := strings.TrimSuffix(c.cfg.URL, "/")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.cfg.AuthType == "bearer" && c.cfg.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.AuthToken)
	} else if c.cfg.AuthType == "api_key" && c.cfg.AuthToken != "" {
		httpReq.Header.Set("X-API-Key", c.cfg.AuthToken)
	}

	resp, err := c.httpCl.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("mcp sse request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("mcp sse %d: %s", resp.StatusCode, string(b))
	}

	// Handle SSE text/event-stream responses — scan for JSON-RPC result.
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return c.parseSSEStream(resp.Body, id)
	}

	// Plain JSON response
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, err
	}
	var rpcResp jsonrpcResponse
	if err := json.Unmarshal(b, &rpcResp); err != nil {
		return nil, fmt.Errorf("mcp parse response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("mcp rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

// parseSSEStream reads SSE events until it finds a JSON-RPC response with matching ID.
func (c *Client) parseSSEStream(r io.Reader, expectedID int) (json.RawMessage, error) {
	scanner := bufio.NewScanner(r)
	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		} else if line == "" && len(dataLines) > 0 {
			// End of event
			data := strings.Join(dataLines, "\n")
			dataLines = nil
			var rpcResp jsonrpcResponse
			if err := json.Unmarshal([]byte(data), &rpcResp); err != nil {
				slog.Debug("mcp sse: non-json event, skipping", "data", data[:min(len(data), 100)])
				continue
			}
			if rpcResp.ID == expectedID {
				if rpcResp.Error != nil {
					return nil, fmt.Errorf("mcp rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
				}
				return rpcResp.Result, nil
			}
		}
	}
	return nil, fmt.Errorf("mcp sse: stream ended without response for id=%d", expectedID)
}

// callStdio sends JSON-RPC over stdin/stdout of a child process.
func (c *Client) callStdio(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.ensureProcess(ctx); err != nil {
		return nil, err
	}

	id := c.nextID
	c.nextID++

	req := jsonrpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')

	if _, err := c.stdin.Write(body); err != nil {
		return nil, fmt.Errorf("mcp stdio write: %w", err)
	}

	// Read one JSON-RPC response line
	line, err := c.stdout.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("mcp stdio read: %w", err)
	}

	var resp jsonrpcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("mcp stdio parse: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcp rpc error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

// ensureProcess starts the stdio child process if not already running.
// Must be called with c.mu held.
func (c *Client) ensureProcess(ctx context.Context) error {
	if c.cmd != nil && c.cmd.ProcessState == nil {
		return nil // already running
	}

	cmd := exec.CommandContext(ctx, c.cfg.Command, c.cfg.Args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("mcp stdio stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("mcp stdio stdout: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mcp stdio start %s: %w", c.cfg.Command, err)
	}

	c.cmd = cmd
	c.stdin = stdin
	c.stdout = bufio.NewReader(stdout)

	// Send initialize
	initReq := jsonrpcRequest{
		JSONRPC: "2.0", ID: 0, Method: "initialize",
		Params: map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{},
			"clientInfo": map[string]string{
				"name":    "golang-backend",
				"version": "1.0.0",
			},
		},
	}
	b, _ := json.Marshal(initReq)
	b = append(b, '\n')
	if _, err := c.stdin.Write(b); err != nil {
		return fmt.Errorf("mcp stdio init write: %w", err)
	}
	// Read init response (discard)
	if _, err := c.stdout.ReadBytes('\n'); err != nil {
		return fmt.Errorf("mcp stdio init read: %w", err)
	}

	return nil
}

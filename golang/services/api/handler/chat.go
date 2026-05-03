package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/mike00028/golang-backend/services/api/internal/agentstore"
	"github.com/mike00028/golang-backend/services/api/internal/apperror"
	"github.com/mike00028/golang-backend/services/api/internal/dag"
	"github.com/mike00028/golang-backend/services/api/internal/evaluator"
	"github.com/mike00028/golang-backend/services/api/internal/grpcclient"
	"github.com/mike00028/golang-backend/services/api/internal/hitl"
	langgraphv1 "github.com/mike00028/golang-backend/services/api/internal/langgraphv1/langgraph/v1"
	"github.com/mike00028/golang-backend/services/api/internal/llm"
	"github.com/mike00028/golang-backend/services/api/internal/memory"
	"github.com/mike00028/golang-backend/services/api/internal/planner"
	pkgsse "github.com/mike00028/golang-backend/services/api/internal/sse"
	"github.com/mike00028/golang-backend/services/api/internal/telemetry"
)

type chatRequest struct {
	Message   string                 `json:"message"  binding:"required"`
	SessionID string                 `json:"session_id"`
	AgentID   string                 `json:"agent_id"`
	UserID    string                 `json:"user_id"`
	Options   map[string]interface{} `json:"options"`
	Stream    *bool                  `json:"stream"`
}

// ChatHandler drives DAG execution and streams results as SSE.
type ChatHandler struct {
	pool       *grpcclient.Pool
	checkpoint dag.CheckpointWriter
	agentStore *agentstore.Store
	memorySvc  *memory.Service
	hitlStore  *hitl.Store
	llmClient  llm.Client
	planModel  string
	chatModel  string // tool execution: chat_agent, summarize_agent
	evalModel  string
}

// NewChatHandler creates a ChatHandler.
func NewChatHandler(
	pool *grpcclient.Pool,
	cp dag.CheckpointWriter,
	agentStore *agentstore.Store,
	memorySvc *memory.Service,
	hitlStore *hitl.Store,
	client llm.Client,
	planModel, chatModel, evalModel string,
) *ChatHandler {
	return &ChatHandler{
		pool:       pool,
		checkpoint: cp,
		agentStore: agentStore,
		memorySvc:  memorySvc,
		hitlStore:  hitlStore,
		llmClient:  client,
		planModel:  planModel,
		chatModel:  chatModel,
		evalModel:  evalModel,
	}
}

// Stream handles POST /chat.
func (h *ChatHandler) Stream(c *gin.Context) {
	var req chatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.normalise(c, &req)
	c.Set("langfuse.input", req.Message)

	streamEnabled := true
	if req.Stream != nil {
		streamEnabled = *req.Stream
	}
	if !streamEnabled {
		h.invoke(c, req)
		return
	}

	sse, ok := pkgsse.New(c.Writer)
	if !ok {
		c.JSON(http.StatusInternalServerError, apperror.New(apperror.CodeInternal, "Streaming is not supported by this client.", http.StatusInternalServerError))
		return
	}
	sse.SendEvent("open", "[STREAM_START]")

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Minute)
	defer cancel()
	ctx, span := telemetry.NewTracer("handler").Start(ctx, "chat.stream")
	defer span.End()
	span.SetAttr(
		telemetry.StringAttr("langfuse.input", req.Message),
		telemetry.StringAttr("langfuse.session.id", req.SessionID),
		telemetry.StringAttr("langfuse.user.id", req.UserID),
	)

	runReq, spec, err := h.buildRunRequest(ctx, req)
	if err != nil {
		sseError(sse, apperror.Classify(err))
		return
	}

	events := make(chan dag.SSEEvent, 64)
	stub := langgraphv1.NewAgentServiceClient(h.pool.Next())
	orch := h.buildOrchestrator(stub, events, spec, runReq.SessionID, runReq.Message, true)

	resultCh := make(chan *dag.RunResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := orch.Run(ctx, runReq)
		if err != nil {
			errCh <- err
		} else {
			resultCh <- result
		}
		close(events)
	}()

	for ev := range events {
		data, _ := json.Marshal(ev)
		sse.SendEvent(ev.Type, string(data))
	}

	select {
	case err := <-errCh:
		ae := apperror.Classify(err)
		log.Printf("dag.error session_id=%s code=%s detail=%s", req.SessionID, ae.Code, ae.Detail)
		sseError(sse, ae)
	case result := <-resultCh:
		log.Printf("dag.done session_id=%s eval_ok=%v score=%.2f", req.SessionID, result.EvalOK, result.ConfidenceScore)
		span.SetAttr(
			telemetry.StringAttr("langfuse.output", result.FinalOutput),
			telemetry.BoolAttr("eval.ok", result.EvalOK),
			telemetry.Float64Attr("eval.score", result.ConfidenceScore),
		)
		h.maybeFlushMemory(req.UserID, req.SessionID, result, spec)
		// Emit final_response first so the frontend can render the answer immediately.
		sse.SendEvent("final_response", result.FinalOutput)
		donePayload, _ := json.Marshal(map[string]interface{}{
			"output":            result.FinalOutput,
			"eval_ok":           result.EvalOK,
			"confidence_score":  result.ConfidenceScore,
			"confidence_reason": result.ConfidenceReason,
		})
		sse.SendEvent("dag_done", string(donePayload))
		sse.SendEvent("done", "[STREAM_END]")
	}
}

// Invoke handles POST /agent/invoke (unary).
func (h *ChatHandler) Invoke(c *gin.Context) {
	var req chatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.Set("langfuse.input", req.Message)
	h.invoke(c, req)
}

func (h *ChatHandler) invoke(c *gin.Context, req chatRequest) {
	h.normalise(c, &req)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	defer cancel()
	ctx, span := telemetry.NewTracer("handler").Start(ctx, "chat.invoke")
	defer span.End()
	span.SetAttr(
		telemetry.StringAttr("langfuse.input", req.Message),
		telemetry.StringAttr("langfuse.session.id", req.SessionID),
		telemetry.StringAttr("langfuse.user.id", req.UserID),
	)

	runReq, spec, err := h.buildRunRequest(ctx, req)
	if err != nil {
		ae := apperror.Classify(err)
		httpError(c, ae)
		return
	}

	events := make(chan dag.SSEEvent, 64)
	stub := langgraphv1.NewAgentServiceClient(h.pool.Next())
	orch := h.buildOrchestrator(stub, events, spec, runReq.SessionID, runReq.Message, false)
	go func() {
		for range events {
		}
	}()

	result, err := orch.Run(ctx, runReq)
	close(events)
	if err != nil {
		ae := apperror.Classify(err)
		log.Printf("dag.error session_id=%s code=%s detail=%s", req.SessionID, ae.Code, ae.Detail)
		httpError(c, ae)
		return
	}

	h.maybeFlushMemory(req.UserID, req.SessionID, result, spec)
	span.SetAttr(
		telemetry.StringAttr("langfuse.output", result.FinalOutput),
		telemetry.BoolAttr("eval.ok", result.EvalOK),
		telemetry.Float64Attr("eval.score", result.ConfidenceScore),
	)
	c.JSON(http.StatusOK, gin.H{
		"session_id":        req.SessionID,
		"output":            result.FinalOutput,
		"eval_ok":           result.EvalOK,
		"confidence_score":  result.ConfidenceScore,
		"confidence_reason": result.ConfidenceReason,
	})
}

// -- Helpers ------------------------------------------------------------------

// normalise fills in default session/user/agent IDs.
func (h *ChatHandler) normalise(c *gin.Context, req *chatRequest) {
	if req.SessionID == "" {
		req.SessionID = c.GetHeader("X-Session-ID")
	}
	if req.SessionID == "" {
		req.SessionID = uuid.NewString()
	}
	if req.UserID == "" {
		req.UserID = c.GetString("user_id") // set by Auth middleware
	}
	if req.AgentID == "" {
		req.AgentID = "default"
	}
}

// buildRunRequest loads the agent spec from DB, fetches memory context,
// and returns a fully-populated dag.RunRequest.
func (h *ChatHandler) buildRunRequest(ctx context.Context, req chatRequest) (dag.RunRequest, *agentstore.AgentSpec, error) {
	// -- Load agent spec from DB (security boundary) --------------------------
	spec, err := h.agentStore.Load(ctx, req.AgentID, req.UserID)
	if err != nil {
		return dag.RunRequest{}, nil, fmt.Errorf("agent %q: %w", req.AgentID, err)
	}

	// -- Fetch semantic memory context ----------------------------------------
	memCtx := ""
	if h.memorySvc != nil && spec.MemoryPolicy.TopKRead > 0 {
		memCtx = h.memorySvc.Retrieve(ctx, req.UserID, req.Message, spec.MemoryPolicy.TopKRead)
	}

	return dag.RunRequest{
		SessionID:     req.SessionID,
		UserID:        req.UserID,
		AgentID:       req.AgentID,
		Message:       req.Message,
		AgentSpecJSON: spec.ToSpecJSON(),
		MemoryContext: memCtx,
	}, spec, nil
}

// buildOrchestrator wires Go-native planner + evaluator + gRPC executor + hooks.
// streaming=true means the caller is forwarding SSE events to a browser, which
// is required for HITL: approval_required events must reach the client before
// Request() blocks the goroutine.  In non-streaming (Invoke) mode, tasks that
// require approval fail immediately with an actionable error.
func (h *ChatHandler) buildOrchestrator(
	stub langgraphv1.AgentServiceClient,
	events chan dag.SSEEvent,
	spec *agentstore.AgentSpec,
	sessionID string,
	userMessage string,
	streaming bool,
) *dag.Orchestrator {
	// Pick per-agent model overrides or fall back to server defaults.
	planModel := h.planModel
	if spec.PlannerModel != "" {
		planModel = spec.PlannerModel
	}

	ollama := h.llmClient
	p := planner.NewPlanner(ollama, planModel)
	e := evaluator.NewEvaluator(ollama, h.evalModel)
	execClient := dag.NewExecutorClient(stub)
	executor := dag.NewExecutor(execClient, h.checkpoint, events)

	// ── Local handlers: run in Go, no Python gRPC round-trip ─────────────────
	// spec.Model overrides the server default; otherwise use the dedicated chat model.
	chatModel := spec.Model
	if chatModel == "" {
		chatModel = h.chatModel
	}
	ollamaExec := h.llmClient

	// chat_agent: LLM answer / code generation / explanation via Ollama
	executor.RegisterLocal("chat_agent", func(localCtx context.Context, task *dag.Task) (string, error) {
		var args struct {
			Question     string `json:"question"`
			SystemPrompt string `json:"system_prompt"`
		}
		_ = json.Unmarshal([]byte(task.ArgsJSON), &args)
		// Use the planner-scoped sub-query; fall back to full user message only
		// when the planner failed to populate args.question.
		question := args.Question
		if question == "" {
			question = userMessage
		}
		// Prepend upstream dependency results (e.g. prior math/text outputs)
		// so the LLM can reference them. task.Context now contains ONLY
		// [tN result] lines — never the full user message.
		if task.Context != "" {
			question = task.Context + "\n" + question
		}
		sys := args.SystemPrompt
		if sys == "" {
			sys = spec.SystemPrompt
		}
		// Ensure chat_agent is always concise — never writes tutorials or long explanations.
		// The planner may inject a more specific system prompt via args.system_prompt.
		const concisenessRule = " Be concise. Answer in 1-3 sentences maximum. Do not write tutorials, code examples, or lengthy explanations unless explicitly asked."
		sys += concisenessRule
		msgs := []llm.Message{}
		if sys != "" {
			msgs = append(msgs, llm.Message{Role: "system", Content: sys})
		}
		msgs = append(msgs, llm.Message{Role: "user", Content: question})
		return ollamaExec.Chat(localCtx, chatModel, msgs, nil)
	})

	// math_agent: evaluate arithmetic expressions directly in Go.
	// Resolves {tN} placeholders from task.DepResults (typed map) — not by
	// parsing the text context string, which could contain false matches.
	executor.RegisterLocal("math_agent", func(localCtx context.Context, task *dag.Task) (string, error) {
		var args struct {
			Expr string `json:"expr"`
		}
		_ = json.Unmarshal([]byte(task.ArgsJSON), &args)
		expr := resolveFromDepResults(args.Expr, task.DepResults)
		return evalMathExpr(expr)
	})

	// summarize_agent: synthesize multiple task outputs via Go Ollama call
	executor.RegisterLocal("summarize_agent", func(localCtx context.Context, task *dag.Task) (string, error) {
		// task.Context contains "[t1 result]: ...\n[t2 result]: ..." from RunBatch.
		// Pass the original user message (not args.question — the planner may not
		// populate it for summarize_agent) so the synthesizer knows the full intent.
		s := planner.NewSummarizer(ollamaExec, chatModel)
		return s.Summarize(localCtx, userMessage, []string{task.Context})
	})

	// clarify_agent: zero-latency passthrough — outputs args.question directly to
	// the user without any LLM call. Used by the planner when required inputs are
	// genuinely missing (e.g. user asked to analyse text but provided no text).
	executor.RegisterLocal("clarify_agent", func(_ context.Context, task *dag.Task) (string, error) {
		var args struct {
			Question string `json:"question"`
		}
		_ = json.Unmarshal([]byte(task.ArgsJSON), &args)
		if args.Question == "" {
			return "Please provide the missing inputs.", nil
		}
		return args.Question, nil
	})

	// HITL middleware: pause tasks whose tools are in approval_required_tools.
	// In streaming mode: emit a hitl_approval_required SSE event then block until
	// a human calls POST /agent/approve.  In non-streaming mode: fail fast with a
	// clear error so the caller can switch to the streaming endpoint.
	if len(spec.ApprovalRequiredTools) > 0 {
		approvalSet := make(map[string]bool, len(spec.ApprovalRequiredTools))
		for _, t := range spec.ApprovalRequiredTools {
			approvalSet[t] = true
		}
		executor.AddMiddleware(func(mwCtx context.Context, task *dag.Task) error {
			if !approvalSet[task.ToolName] {
				return nil
			}
			if !streaming {
				return fmt.Errorf("tool %q requires human approval; use the streaming /chat endpoint", task.ToolName)
			}
			// Notify the browser that approval is required.
			payload, _ := json.Marshal(map[string]string{
				"session_id": sessionID,
				"task_id":    task.ID,
				"tool_name":  task.ToolName,
				"args_json":  task.ArgsJSON,
			})
			select {
			case events <- dag.SSEEvent{Type: "hitl_approval_required", TaskID: task.ID, Payload: string(payload)}:
			case <-mwCtx.Done():
				return mwCtx.Err()
			}
			// Block until a human responds via POST /agent/approve.
			return h.hitlStore.Request(mwCtx, sessionID, task.ID)
		})
	}

	orch := dag.NewOrchestrator(p, e, executor, h.checkpoint)
	orch.SetSummarizer(planner.NewSummarizer(ollamaExec, chatModel))
	orch.SetEvents(events)

	// Cost-estimation / iteration guard: reject plans that exceed max_iterations.
	maxIter := spec.MaxIterations
	orch.AddBeforePlan(func(_ context.Context, planReq *dag.GoPlanRequest) error {
		if planReq.Generation > maxIter {
			return fmt.Errorf("exceeded max_iterations (%d) for agent %q", maxIter, spec.ID)
		}
		return nil
	})

	return orch
}

// maybeFlushMemory writes a memory entry when policy allows.
func (h *ChatHandler) maybeFlushMemory(userID, sessionID string, result *dag.RunResult, spec *agentstore.AgentSpec) {
	if h.memorySvc == nil {
		return
	}
	policy := spec.MemoryPolicy
	if !policy.WriteOnEvalOK {
		return
	}
	if !result.EvalOK || result.ConfidenceScore < policy.MinScoreToWrite {
		return
	}

	// Write in a background goroutine - never block the response.
	go func() {
		writeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		content := fmt.Sprintf("Q: %s\nA (confidence %.2f): %s",
			"[session "+sessionID+"]", result.ConfidenceScore, result.FinalOutput)
		if err := h.memorySvc.WriteEntry(writeCtx, userID, sessionID, content, "workflow"); err != nil {
			log.Printf("memory.write warn session_id=%s err=%v", sessionID, err)
		}
	}()
}

// resolveFromDepResults substitutes {tN} and bare tN placeholders in expr
// with values from the typed DepResults map (keyed by task ID).
// This replaces the old text-parsing approach (resolveFromContext) which was
// fragile — any agent output containing "[tN result]: 99" would corrupt math.
func resolveFromDepResults(expr string, deps map[string]string) string {
	if len(deps) == 0 {
		return expr
	}
	// Replace {tN} first (unambiguous).
	resolved := regexp.MustCompile(`\{([^}]+)\}`).ReplaceAllStringFunc(expr, func(tok string) string {
		id := tok[1 : len(tok)-1]
		// Extract leading numeric value from the dep output (e.g. "5328" from "5328\n...")
		if raw, ok := deps[id]; ok {
			if m := regexp.MustCompile(`-?\d+(?:\.\d+)?`).FindString(raw); m != "" {
				return m
			}
		}
		return tok
	})
	// Then replace bare word-boundary task IDs (e.g. "t1 + 56").
	for id, raw := range deps {
		if m := regexp.MustCompile(`-?\d+(?:\.\d+)?`).FindString(raw); m != "" {
			re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(id) + `\b`)
			resolved = re.ReplaceAllString(resolved, m)
		}
	}
	return resolved
}

// evalMathExpr evaluates a simple "a op b" arithmetic expression and returns the result as a string.
// Supports +, -, *, / on integers and decimals.
var _mathRe = regexp.MustCompile(`^\s*(-?\d+(?:\.\d+)?)\s*([\+\-\*/])\s*(-?\d+(?:\.\d+)?)\s*$`)

func evalMathExpr(expr string) (string, error) {
	m := _mathRe.FindStringSubmatch(expr)
	if m == nil {
		return "", fmt.Errorf("cannot parse math expression: %q", expr)
	}
	a, _ := strconv.ParseFloat(m[1], 64)
	b, _ := strconv.ParseFloat(m[3], 64)
	var result float64
	switch m[2] {
	case "+":
		result = a + b
	case "-":
		result = a - b
	case "*":
		result = a * b
	case "/":
		if b == 0 {
			return "", fmt.Errorf("division by zero")
		}
		result = a / b
	}
	if result == float64(int64(result)) {
		return strconv.FormatInt(int64(result), 10), nil
	}
	return strconv.FormatFloat(result, 'f', -1, 64), nil
}

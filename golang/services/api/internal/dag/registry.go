package dag

// AgentDef is the single source of truth for one agent in the system.
// The planner prompt, executor safety net, and handler registrations all
// derive from this list — adding a new agent requires editing only here.
type AgentDef struct {
	Name          string // matches tool_name in planner output
	Description   string // shown verbatim in the planner system prompt
	IsLocal       bool   // true = Go-native handler; false = Python gRPC
	NeedsQuestion bool   // safety net: inject dag.UserMessage when args.question is empty
}

// AgentRegistry is the ordered list of all agents available to the planner.
var AgentRegistry = []AgentDef{
	{
		Name:          "chat_agent",
		Description:   "answers general questions, explains concepts, writes code",
		IsLocal:       true,
		NeedsQuestion: true,
	},
	{
		Name:          "math_agent",
		Description:   `evaluates arithmetic expressions (+, -, *, /); args: {"expr": "<expression>"}`,
		IsLocal:       true,
		NeedsQuestion: false,
	},
	{
		Name:          "rag_agent",
		Description:   "looks up internal docs about LangGraph, gRPC, or Ollama",
		IsLocal:       false,
		NeedsQuestion: true,
	},
	{
		Name:          "summarize_agent",
		Description:   "merges results from multiple tasks into a final answer",
		IsLocal:       true,
		NeedsQuestion: false,
	},
	{
		Name:          "text_agent",
		Description:   "analyses text: counts vowels, consonants, or word occurrences",
		IsLocal:       false,
		NeedsQuestion: true,
	},
	{
		Name:          "clarify_agent",
		Description:   "outputs a clarification question to the user when required inputs are genuinely missing; args.question must contain the full question to display",
		IsLocal:       true,
		NeedsQuestion: true,
	},
}

// AgentByName returns the AgentDef for the given name, and whether it was found.
// For agents not in the static registry (e.g. custom agents loaded from DB at runtime),
// it returns a safe default: NeedsQuestion=true, IsLocal=false.
// This means the executor safety net and planner prompt degrade gracefully —
// no code change needed when a new dynamic agent is added to the DB.
func AgentByName(name string) (AgentDef, bool) {
	for _, a := range AgentRegistry {
		if a.Name == name {
			return a, true
		}
	}
	// Unknown agent (e.g. custom DB agent): safe defaults.
	// IsLocal=false — no Go handler, will route to Python gRPC.
	// NeedsQuestion=true — all LLM-based agents need a question.
	return AgentDef{Name: name, IsLocal: false, NeedsQuestion: true}, false
}

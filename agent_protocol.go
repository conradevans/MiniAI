package main

// These types mirror Ollama's chat and function-tool protocol. Keep their JSON
// representation stable: they are shared by planner and final-response calls.
type chatMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	Type     string       `json:"type,omitempty"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type toolDefinition struct {
	Type     string             `json:"type"`
	Function toolDefinitionBody `json:"function"`
}

type toolDefinitionBody struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type chatAPIRequest struct {
	Model     string           `json:"model"`
	Messages  []chatMessage    `json:"messages"`
	Tools     []toolDefinition `json:"tools,omitempty"`
	Format    any              `json:"format,omitempty"`
	Stream    bool             `json:"stream"`
	Think     bool             `json:"think"`
	KeepAlive any              `json:"keep_alive,omitempty"`
	Options   map[string]any   `json:"options,omitempty"`
}

type chatAPIResponse struct {
	Message            chatMessage `json:"message"`
	Done               bool        `json:"done"`
	PromptEvalCount    int         `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64       `json:"prompt_eval_duration,omitempty"`
	EvalCount          int         `json:"eval_count,omitempty"`
	EvalDuration       int64       `json:"eval_duration,omitempty"`
	LoadDuration       int64       `json:"load_duration,omitempty"`
	TotalDuration      int64       `json:"total_duration,omitempty"`
	Error              string      `json:"error,omitempty"`
}

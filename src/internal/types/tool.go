package types

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

type ExecutionOutput struct {
	Output   string
	Error    string
	Code     int
	Duration time.Duration
}

type Tool interface {
	Name() string
	Description() string
	Parameters() json.RawMessage
	Execute(ctx context.Context, args string) ExecutionOutput
}

type Registry struct {
	tools map[string]Tool
}

func (r *Registry) String() string {
	toolNames := make([]string, 0, len(r.tools))
	for name := range r.tools {
		toolNames = append(toolNames, name)
	}
	return "Registry{" + strings.Join(toolNames, ", ") + "}"
}

func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

func (r *Registry) Register(tool Tool) {
	r.tools[tool.Name()] = tool
}

func (r *Registry) Get(name string) (Tool, bool) {
	tool, ok := r.tools[name]
	return tool, ok
}
func (r *Registry) List() []Tool {
	tools := make([]Tool, 0, len(r.tools))
	for _, tool := range r.tools {
		tools = append(tools, tool)
	}
	return tools
}

package llm

import (
	"encoding/json"
	"vyasa/src/types"
)

type parameterizedTool interface {
	Parameters() json.RawMessage
}

func ToolSpecFromTool(tool types.Tool) ToolSpec {
	spec := ToolSpec{
		Name:        tool.Name(),
		Description: tool.Description(),
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}

	if parameterized, ok := tool.(parameterizedTool); ok {
		if schema := parameterized.Parameters(); len(schema) > 0 && json.Valid(schema) {
			spec.Parameters = schema
		}
	}

	return spec
}

func ToolSpecs(registry *types.Registry) []ToolSpec {
	if registry == nil {
		return nil
	}

	tools := registry.List()
	specs := make([]ToolSpec, 0, len(tools))
	for _, tool := range tools {
		specs = append(specs, ToolSpecFromTool(tool))
	}
	return specs
}

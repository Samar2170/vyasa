package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"vyasa/src/llm"
	"vyasa/src/types"
)

const defaultDelegateParams = `{"type":"object","properties":{"task":{"type":"string","description":"The task for the specialist agent to perform."}},"required":["task"]}`

type delegateArgs struct {
	Task string `json:"task"`
}

type DelegateTool struct {
	runner      Runner
	client      llm.ModelClient
	name        string
	description string
	params      json.RawMessage
}

var _ types.Tool = (*DelegateTool)(nil)

func NewDelegateTool(name, description string, runner Runner, client llm.ModelClient) *DelegateTool {
	return &DelegateTool{
		runner:      runner,
		client:      client,
		name:        name,
		description: description,
		params:      json.RawMessage(defaultDelegateParams),
	}
}
func (d *DelegateTool) Name() string        { return d.name }
func (d *DelegateTool) Description() string { return d.description }
func (d *DelegateTool) Parameters() json.RawMessage {
	return d.params
}

func (d *DelegateTool) Execute(ctx context.Context, args string) types.ExecutionOutput {
	var a delegateArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return types.ExecutionOutput{Error: fmt.Sprintf("invalid delegation arguments: %v", err)}
	}
	a.Task = strings.TrimSpace(a.Task)
	res, err := d.runner.Run(ctx, d.client, "", a.Task)
	if err != nil {
		return types.ExecutionOutput{Error: fmt.Sprintf("delegation failed: %v", err)}
	}

	switch res.Finish {
	case FinishCompleted:
		return types.ExecutionOutput{Output: res.FinalText}
	case FinishMaxSteps:
		return types.ExecutionOutput{
			Output: res.FinalText,
			Error:  "note: sub-agent hit its step limit; result may be incomplete",
		}
	case FinishCancelled:
		return types.ExecutionOutput{Error: "delegation cancelled"}
	case FinishError:
		return types.ExecutionOutput{
			Output: res.FinalText,
			Error:  fmt.Sprintf("sub-agent failed: %v", res.Err),
		}
	}
	return types.ExecutionOutput{Output: res.FinalText}
}

package agent

import (
	"context"
	"fmt"
	"vyasa/src/llm"
)

type PipelineStep struct {
	Agent *Agent
	Input string
	Map   func(prev *Result) string
}

type Pipeline struct {
	Name  string
	Steps []PipelineStep
}

var _ Runner = (*Pipeline)(nil)

func (p *Pipeline) Run(ctx context.Context, client llm.ModelClient, systemPrompt, input string, opts ...RunOption) (*Result, error) {
	if p == nil || len(p.Steps) == 0 {
		return nil, fmt.Errorf("agent: pipeline %q has no steps", p.Name)
	}

	total := Result{Finish: FinishCompleted}
	var prev *Result
	for i, st := range p.Steps {
		if st.Agent == nil {
			return nil, fmt.Errorf("agent: pipeline %q step %d has no agent", p.Name, i)
		}

		task := st.Input
		switch {
		case st.Map != nil:
			task = st.Map(prev)
		case task == "":
			if prev != nil {
				task = prev.FinalText
			} else {
				task = input
			}
		}

		res, err := st.Agent.Run(ctx, client, systemPrompt, task, opts...)
		if err != nil {
			return nil, fmt.Errorf("agent: pipeline %q step %d (%s): %w", p.Name, i, st.Agent.Name, err)
		}
		total.Usage.InputTokens += res.Usage.InputTokens
		total.Usage.OutputTokens += res.Usage.OutputTokens
		total.Usage.CachedTokens += res.Usage.CachedTokens
		total.Steps += res.Steps
		prev = res

		if res.Finish != FinishCompleted {
			total.Finish = res.Finish
			total.Err = res.Err
			total.FinalText = res.FinalText
			return &total, nil
		}
		total.FinalText = res.FinalText
	}
	return &total, nil
}

// AsDelegate wraps the pipeline so an orchestrator agent can call it as a
// tool, mixing static and routed orchestration.
func (p *Pipeline) AsDelegate(client llm.ModelClient, description string) *DelegateTool {
	return NewDelegateTool(p.Name, description, p, client)
}

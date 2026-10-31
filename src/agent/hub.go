package agent

import (
	"fmt"
	"sort"
	"strings"
	"vyasa/src/llm"
)

type Hub struct {
	agents map[string]*Agent
}

func NewHub() *Hub {
	return &Hub{agents: make(map[string]*Agent)}
}
func (h *Hub) Add(agent *Agent) error {
	if agent == nil || agent.Name == "" {
		return fmt.Errorf("agent: hub requires a non-nil agent with a name")
	}
	if _, exists := h.agents[agent.Name]; exists {
		return fmt.Errorf("agent: duplicate agent %q", agent.Name)
	}
	h.agents[agent.Name] = agent
	return nil
}

func (h *Hub) Get(name string) (*Agent, bool) {
	a, ok := h.agents[name]
	return a, ok
}

// Names returns agent names in stable (sorted) order.
func (h *Hub) Names() []string {
	names := make([]string, 0, len(h.agents))
	for name := range h.agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (h *Hub) DelegateTools(client llm.ModelClient) []*DelegateTool {
	names := h.Names()
	out := make([]*DelegateTool, 0, len(names))
	for _, name := range names {
		a := h.agents[name]
		out = append(out, NewDelegateTool(a.Name, a.Description, a, client))
	}
	return out
}

// Roster renders the specialist catalog for an orchestrator's system prompt.
func (h *Hub) Roster() string {
	var b strings.Builder
	b.WriteString("Available specialist agents:\n")
	for _, name := range h.Names() {
		a := h.agents[name]
		b.WriteString("- " + name)
		if a.Description != "" {
			b.WriteString(": " + a.Description)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// CompleteJSON sends a two-message (system, user) request through client and
// unmarshals the model's reply into out. Fenced code blocks are stripped
// before decoding so a ```json wrapper does not break parsing.
func CompleteJSON(ctx context.Context, client ModelClient, system, user string, out any) error {
	if client == nil {
		return fmt.Errorf("llm: nil client")
	}
	resp, err := client.Complete(ctx, Request{
		Messages: []Message{
			{Role: RoleSystem, Parts: []Part{{Kind: PartText, Text: system}}},
			{Role: RoleUser, Parts: []Part{{Kind: PartText, Text: user}}},
		},
	})
	if err != nil {
		return err
	}
	content := stripCodeFence(resp.Message.Text())
	if content == "" {
		return fmt.Errorf("llm: empty reply")
	}
	if err := json.Unmarshal([]byte(content), out); err != nil {
		return fmt.Errorf("llm: decode JSON reply: %w", err)
	}
	return nil
}

// stripCodeFence removes a leading markdown code fence so JSON-only replies
// wrapped by the model still decode.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}
